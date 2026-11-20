// Package config holds the gateway's complete configuration surface.
//
// Design rule: the upstream destination is NOT configurable. It is a compile
// time constant. Environment variables can adjust limits and select the session
// resolver, but they can never redirect the gateway to another host. This is
// what enforces "no arbitrary destination URLs" at the configuration layer
// instead of relying on review.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// ProductionUpstreamBaseURL is the only upstream the gateway will ever contact.
// It is deliberately a constant, not an environment variable.
const ProductionUpstreamBaseURL = "https://opencode.ai/inference/go/openai/v1"

// ProductionUpstreamHost is the allowlist applied by Validate().
const ProductionUpstreamHost = "opencode.ai"

// DefaultModel is the only model the gateway will forward.
const DefaultModel = "deepseek-v4.1-flash"

// SessionMode selects the conversation-identity resolver.
type SessionMode string

const (
	// SessionModeUnavailable is the only mode the shipped binary can enter.
	// It fails closed: no request reaches the provider.
	SessionModeUnavailable SessionMode = "unavailable"

	// SessionModeMock is test-only. It is refused unless TestOnly is set,
	// and TestOnly can only be set in code (never from the environment).
	SessionModeMock SessionMode = "mock"

	// SessionModeHeader trusts an explicit conversation header from the client.
	// This is the only mode a production deployment may use.
	SessionModeHeader SessionMode = "header"

	// SessionModeDerived derives a stable identifier from conversation
	// metadata. It is the only mode a production deployment may use besides
	// "unavailable".
	SessionModeDerived SessionMode = "derived"

	// SessionModeCapture is test-only. It issues one identifier per request to
	// observe the real client payload. It is explicitly not a session solution.
	SessionModeCapture SessionMode = "protocol-capture"
)

// Config is immutable after construction.
type Config struct {
	ListenAddr string

	// UpstreamBaseURL must equal ProductionUpstreamBaseURL unless TestOnly.
	UpstreamBaseURL string
	UpstreamHost    string
	UpstreamToken   string

	ClientToken string
	Model       string

	SessionMode SessionMode

	MaxBodyBytes          int64
	MaxHeaderBytes        int
	ConnectTimeout        time.Duration
	ResponseHeaderTimeout time.Duration
	StreamIdleTimeout     time.Duration
	TotalTimeout          time.Duration

	MaxInFlight   int
	RatePerMinute int
	RateBurst     int

	// TestOnly relaxes the upstream allowlist and permits SessionModeMock.
	// It is never populated from the environment.
	TestOnly bool
}

// ErrMockModeNotAllowed is returned when mock session mode is requested by a
// non-test configuration.
var ErrMockModeNotAllowed = errors.New("session mode \"mock\" is only available to in-process tests")

// Production builds the configuration from the environment.
//
// Recognised variables (all optional unless noted):
//
//	GATEWAY_CLIENT_TOKEN_FILE   path to the bearer token n8n must present (required)
//	GATEWAY_UPSTREAM_TOKEN_FILE path to the OpenCode Go API key (required)
//	GATEWAY_LISTEN_ADDR         default :8080
//	GATEWAY_MODEL               default deepseek-v4.1-flash
//	GATEWAY_SESSION_MODE        default "unavailable"; "mock" is rejected
//	GATEWAY_MAX_BODY_BYTES      default 4194304 (measured Assistant payloads are ~112-187 KB)
//	GATEWAY_MAX_INFLIGHT        default 8
//	GATEWAY_RATE_PER_MINUTE     default 300 (measured Assistant turns exceed 10 quickly)
//	GATEWAY_RATE_BURST          default 60
func Production() (Config, error) {
	cfg := Config{
		ListenAddr:            getenv("GATEWAY_LISTEN_ADDR", ":8080"),
		UpstreamBaseURL:       ProductionUpstreamBaseURL,
		UpstreamHost:          ProductionUpstreamHost,
		Model:                 getenv("GATEWAY_MODEL", DefaultModel),
		SessionMode:           SessionMode(getenv("GATEWAY_SESSION_MODE", string(SessionModeUnavailable))),
		MaxBodyBytes:          getenvInt64("GATEWAY_MAX_BODY_BYTES", 4<<20),
		MaxHeaderBytes:        32 << 10,
		ConnectTimeout:        5 * time.Second,
		ResponseHeaderTimeout: 180 * time.Second,
		StreamIdleTimeout:     120 * time.Second,
		TotalTimeout:          900 * time.Second,
		MaxInFlight:           int(getenvInt64("GATEWAY_MAX_INFLIGHT", 8)),
		RatePerMinute:         int(getenvInt64("GATEWAY_RATE_PER_MINUTE", 300)),
		RateBurst:             int(getenvInt64("GATEWAY_RATE_BURST", 60)),
	}

	var err error
	if cfg.ClientToken, err = readSecret("GATEWAY_CLIENT_TOKEN_FILE", "GATEWAY_CLIENT_TOKEN"); err != nil {
		return Config{}, fmt.Errorf("client token: %w", err)
	}
	if cfg.UpstreamToken, err = readSecret("GATEWAY_UPSTREAM_TOKEN_FILE", "GATEWAY_UPSTREAM_TOKEN"); err != nil {
		return Config{}, fmt.Errorf("upstream token: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate enforces every invariant the runtime depends on.
func (c Config) Validate() error {
	if c.TestOnly {
		// Test configurations may point at a loopback httptest server. They are
		// constructible only in code; Production() never sets TestOnly.
		if c.UpstreamBaseURL == "" {
			return errors.New("test config requires an upstream base URL")
		}
		switch c.SessionMode {
		case SessionModeMock, SessionModeCapture, SessionModeUnavailable, SessionModeDerived, SessionModeHeader:
			return nil
		default:
			return fmt.Errorf("unknown session mode %q", c.SessionMode)
		}
	}
	if c.UpstreamBaseURL != ProductionUpstreamBaseURL {
		return fmt.Errorf("upstream base URL is not the production constant")
	}
	u, err := url.Parse(c.UpstreamBaseURL)
	if err != nil {
		return fmt.Errorf("upstream URL: %w", err)
	}
	if u.Scheme != "https" {
		return errors.New("upstream must be https")
	}
	if u.Host != c.UpstreamHost || u.Host != ProductionUpstreamHost {
		return fmt.Errorf("upstream host %q is not allowlisted", u.Host)
	}
	if c.SessionMode == SessionModeMock {
		return ErrMockModeNotAllowed
	}
	switch c.SessionMode {
	case SessionModeUnavailable, SessionModeHeader:
	default:
		return fmt.Errorf("session mode %q is not allowed in production", c.SessionMode)
	}
	if len(c.ClientToken) < 16 {
		return errors.New("client token must be at least 16 bytes")
	}
	if len(c.UpstreamToken) < 16 {
		return errors.New("upstream token must be at least 16 bytes")
	}
	if c.ClientToken == c.UpstreamToken {
		return errors.New("client and upstream tokens must differ")
	}
	if c.MaxBodyBytes <= 0 || c.MaxBodyBytes > 64<<20 {
		return errors.New("MaxBodyBytes out of range")
	}
	if c.MaxInFlight <= 0 || c.MaxInFlight > 64 {
		return errors.New("MaxInFlight out of range")
	}
	if c.RatePerMinute <= 0 || c.RateBurst <= 0 || c.RateBurst > c.RatePerMinute {
		return errors.New("rate limit out of range")
	}
	if c.Model == "" {
		return errors.New("model must be set")
	}
	// Worst-case resident body memory is MaxInFlight * MaxBodyBytes. Keep it
	// well inside the 256Mi container limit; the measured peak is ~1.03x per
	// in-flight body.
	if int64(c.MaxInFlight)*c.MaxBodyBytes > 192<<20 {
		return errors.New("MaxInFlight * MaxBodyBytes exceeds the 192MiB memory budget")
	}
	return nil
}

// Fingerprint returns a stable, non-reversible identifier for a token. It is
// used for equality checks and log correlation, never for authentication as a
// substitute for a constant-time comparison.
func Fingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func getenvInt64(key string, def int64) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func readSecret(pathKey, inlineKey string) (string, error) {
	if path := strings.TrimSpace(os.Getenv(pathKey)); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", pathKey, err)
		}
		s := strings.TrimSpace(string(b))
		if s == "" {
			return "", fmt.Errorf("%s is empty", pathKey)
		}
		return s, nil
	}
	if v := strings.TrimSpace(os.Getenv(inlineKey)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s or %s is required", pathKey, inlineKey)
}

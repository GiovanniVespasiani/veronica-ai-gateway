package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/veronica/ai-gateway/internal/config"
)

const (
	testClientToken   = "client-token-for-tests-0123456789"
	testUpstreamToken = "upstream-token-for-tests-9876543210"
)

func setTokens(t *testing.T) {
	t.Helper()
	t.Setenv("GATEWAY_CLIENT_TOKEN", testClientToken)
	t.Setenv("GATEWAY_UPSTREAM_TOKEN", testUpstreamToken)
	t.Setenv("GATEWAY_CLIENT_TOKEN_FILE", "")
	t.Setenv("GATEWAY_UPSTREAM_TOKEN_FILE", "")
}

func TestProductionDefaults(t *testing.T) {
	setTokens(t)
	t.Setenv("GATEWAY_SESSION_MODE", "")
	t.Setenv("GATEWAY_MODEL", "")
	t.Setenv("GATEWAY_LISTEN_ADDR", "")

	cfg, err := config.Production()
	if err != nil {
		t.Fatalf("production config: %v", err)
	}
	if cfg.UpstreamBaseURL != config.ProductionUpstreamBaseURL {
		t.Fatalf("unexpected upstream %q", cfg.UpstreamBaseURL)
	}
	if cfg.Model != config.DefaultModel {
		t.Fatalf("unexpected model %q", cfg.Model)
	}
	if cfg.SessionMode != config.SessionModeUnavailable {
		t.Fatalf("unexpected session mode %q", cfg.SessionMode)
	}
	if cfg.ListenAddr != ":8080" {
		t.Fatalf("unexpected listen address %q", cfg.ListenAddr)
	}
	if cfg.MaxBodyBytes != 4<<20 || cfg.MaxInFlight != 8 || cfg.RatePerMinute != 300 || cfg.RateBurst != 60 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

// The upstream destination must not be configurable.
func TestProductionIgnoresUpstreamEnvironment(t *testing.T) {
	setTokens(t)
	t.Setenv("GATEWAY_UPSTREAM_URL", "https://evil.example/v1")
	t.Setenv("GATEWAY_UPSTREAM_BASE_URL", "https://evil.example/v1")
	t.Setenv("OPENAI_BASE_URL", "https://evil.example/v1")

	cfg, err := config.Production()
	if err != nil {
		t.Fatalf("production config: %v", err)
	}
	if cfg.UpstreamBaseURL != config.ProductionUpstreamBaseURL {
		t.Fatalf("upstream was overridden by the environment: %q", cfg.UpstreamBaseURL)
	}
	if cfg.UpstreamHost != config.ProductionUpstreamHost {
		t.Fatalf("host was overridden: %q", cfg.UpstreamHost)
	}
}

func TestProductionRefusesMockSessionMode(t *testing.T) {
	setTokens(t)
	t.Setenv("GATEWAY_SESSION_MODE", "mock")

	_, err := config.Production()
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, config.ErrMockModeNotAllowed) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProductionRequiresTokens(t *testing.T) {
	t.Setenv("GATEWAY_CLIENT_TOKEN", "")
	t.Setenv("GATEWAY_UPSTREAM_TOKEN", "")
	t.Setenv("GATEWAY_CLIENT_TOKEN_FILE", "")
	t.Setenv("GATEWAY_UPSTREAM_TOKEN_FILE", "")

	if _, err := config.Production(); err == nil {
		t.Fatal("expected an error when tokens are missing")
	}
}

func TestProductionReadsTokenFiles(t *testing.T) {
	dir := t.TempDir()
	clientFile := dir + "/client"
	upstreamFile := dir + "/upstream"
	writeFile(t, clientFile, testClientToken)
	writeFile(t, upstreamFile, testUpstreamToken)

	t.Setenv("GATEWAY_CLIENT_TOKEN", "")
	t.Setenv("GATEWAY_UPSTREAM_TOKEN", "")
	t.Setenv("GATEWAY_CLIENT_TOKEN_FILE", clientFile)
	t.Setenv("GATEWAY_UPSTREAM_TOKEN_FILE", upstreamFile)
	t.Setenv("GATEWAY_SESSION_MODE", "")

	cfg, err := config.Production()
	if err != nil {
		t.Fatalf("production config: %v", err)
	}
	if cfg.ClientToken != testClientToken || cfg.UpstreamToken != testUpstreamToken {
		t.Fatal("token files were not read")
	}
}

func TestValidateRejectsForeignUpstream(t *testing.T) {
	cfg := baseStrict()
	cfg.UpstreamBaseURL = "https://evil.example/v1"
	if err := cfg.Validate(); err == nil {
		t.Fatal("foreign upstream accepted")
	}

	cfg = baseStrict()
	cfg.UpstreamBaseURL = "http://opencode.ai/inference/go/openai/v1"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-TLS upstream accepted")
	}

	cfg = baseStrict()
	cfg.UpstreamHost = "evil.example"
	if err := cfg.Validate(); err == nil {
		t.Fatal("foreign host accepted")
	}
}

func TestValidateRejectsSharedTokens(t *testing.T) {
	cfg := baseStrict()
	cfg.UpstreamToken = cfg.ClientToken
	if err := cfg.Validate(); err == nil {
		t.Fatal("identical client and upstream tokens accepted")
	}
}

func TestValidateRejectsShortTokens(t *testing.T) {
	cfg := baseStrict()
	cfg.ClientToken = "short"
	if err := cfg.Validate(); err == nil {
		t.Fatal("short token accepted")
	}
}

func TestValidateRejectsOutOfRangeLimits(t *testing.T) {
	cases := []func(*config.Config){
		func(c *config.Config) { c.MaxBodyBytes = 0 },
		func(c *config.Config) { c.MaxBodyBytes = 128 << 20 },
		func(c *config.Config) { c.MaxInFlight = 0 },
		func(c *config.Config) { c.MaxInFlight = 1000 },
		func(c *config.Config) { c.MaxBodyBytes = 64 << 20; c.MaxInFlight = 8 },
		func(c *config.Config) { c.RateBurst = c.RatePerMinute + 1 },
		func(c *config.Config) { c.Model = "" },
	}
	for i, mutate := range cases {
		cfg := baseStrict()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("case %d: invalid config accepted", i)
		}
	}
}

func TestFingerprintIsStableAndShort(t *testing.T) {
	a := config.Fingerprint("token-a")
	b := config.Fingerprint("token-a")
	c := config.Fingerprint("token-b")
	if a != b || a == c {
		t.Fatal("fingerprint is not stable/distinct")
	}
	if len(a) != 16 || strings.Contains(a, "token") {
		t.Fatalf("unexpected fingerprint %q", a)
	}
}

func baseStrict() config.Config {
	return config.Config{
		ListenAddr:            ":8080",
		UpstreamBaseURL:       config.ProductionUpstreamBaseURL,
		UpstreamHost:          config.ProductionUpstreamHost,
		UpstreamToken:         testUpstreamToken,
		ClientToken:           testClientToken,
		Model:                 config.DefaultModel,
		SessionMode:           config.SessionModeUnavailable,
		MaxBodyBytes:          4 << 20,
		MaxHeaderBytes:        32 << 10,
		MaxInFlight:           8,
		RatePerMinute:         30,
		RateBurst:             10,
		ConnectTimeout:        1e9,
		ResponseHeaderTimeout: 1e9,
		StreamIdleTimeout:     1e9,
		TotalTimeout:          1e9,
	}
}

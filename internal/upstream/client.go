// Package upstream is the only code that speaks to the provider.
//
// Hard rules enforced here:
//   - the destination is built from a validated base URL, never from a request
//   - environment proxies are ignored (Proxy: nil)
//   - redirects are never followed
//   - a bounded set of headers is sent; nothing is copied from the client
//   - TLS verification is always on
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/veronica/ai-gateway/internal/config"
	"github.com/veronica/ai-gateway/internal/session"
)

// UserAgent identifies the gateway as its own client, as the provider asks.
const UserAgent = "veronica-ai-gateway/0.2 (n8n Assistant; internal)"

// ErrRedirect is returned when the provider answers with a redirect.
var ErrRedirect = errors.New("upstream attempted a redirect")

// ErrOversize is returned when the upstream response exceeds the caller's limit.
var ErrOversize = errors.New("upstream response exceeded limit")

// Client is a provider client with a fixed destination.
type Client struct {
	base      *url.URL
	modelsURL *url.URL
	host      string
	token     string
	client    *http.Client
}

// New builds a client. The base URL must already have passed config.Validate.
func New(cfg config.Config) (*Client, error) {
	base, err := url.Parse(cfg.UpstreamBaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse upstream URL: %w", err)
	}
	if base.Scheme != "https" && !cfg.TestOnly {
		return nil, errors.New("upstream must use https")
	}

	transport := &http.Transport{
		// Explicitly ignore HTTP_PROXY / HTTPS_PROXY / NO_PROXY.
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   cfg.ConnectTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   cfg.ConnectTimeout,
		ResponseHeaderTimeout: cfg.ResponseHeaderTimeout,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true,
	}

	return &Client{
		base:      base,
		modelsURL: modelsEndpoint(base, cfg.TestOnly),
		host:      base.Host,
		token:     cfg.UpstreamToken,
		client: &http.Client{
			Transport: transport,
			// Never follow a redirect; surface it instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// modelsEndpoint derives the read-only model list URL. In production the model
// list lives at a sibling path of the chat base URL; the host is still fixed.
func modelsEndpoint(base *url.URL, testOnly bool) *url.URL {
	if testOnly {
		u := *base
		u.Path = "/v1/models"
		u.RawQuery = ""
		return &u
	}
	u, err := url.Parse("https://" + config.ProductionUpstreamHost + "/inference/go/v1/models")
	if err != nil {
		return nil
	}
	return u
}

// ChatCompletion posts a Chat Completions request and returns the raw
// streaming response. The body is forwarded byte-for-byte; the gateway does not
// rewrite provider payloads.
func (c *Client) ChatCompletion(ctx context.Context, body []byte, sess session.ID) (*http.Response, error) {
	if !sess.Valid() {
		return nil, fmt.Errorf("refusing to send an invalid session identifier")
	}

	endpoint := *c.base
	endpoint.Path = strings.TrimSuffix(c.base.Path, "/") + "/chat/completions"
	if endpoint.Host != c.host {
		return nil, errors.New("upstream host changed during URL construction")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Closed header set. Nothing from the inbound request is copied.
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("x-opencode-session", string(sess))
	req.Host = c.host

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, ErrRedirect
	}
	return resp, nil
}

// Models fetches the provider model list (read-only validation helper).
func (c *Client) Models(ctx context.Context) (*http.Response, error) {
	if c.modelsURL == nil {
		return nil, errors.New("model list endpoint is not configured")
	}
	if c.host == "" {
		return nil, errors.New("client not initialised")
	}
	u := *c.modelsURL
	if c.base.Scheme != "" && c.base.Scheme != u.Scheme {
		u.Scheme = c.base.Scheme
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, ErrRedirect
	}
	return resp, nil
}

// ReadLimited reads at most limit bytes and reports whether the stream was
// longer than the limit.
func ReadLimited(r io.Reader, limit int64) ([]byte, bool, error) {
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if n > limit {
		return nil, true, ErrOversize
	}
	return buf.Bytes(), false, nil
}

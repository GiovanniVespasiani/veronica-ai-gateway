//go:build testmode

// This file exists only in binaries built with `-tags testmode`. It is the
// single place where the upstream destination may be overridden, and it is
// excluded from production builds by the build tag.

package main

import (
	"errors"
	"net/url"
	"os"
	"strings"

	"github.com/veronica/ai-gateway/internal/config"
	"github.com/veronica/ai-gateway/internal/session"
)

func testOverride(cfg config.Config) (config.Config, error) {
	raw := strings.TrimSpace(os.Getenv("GATEWAY_TEST_UPSTREAM_URL"))
	if raw == "" {
		return cfg, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return cfg, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return cfg, errors.New("test upstream must be http or https")
	}
	cfg.TestOnly = true
	cfg.UpstreamBaseURL = raw
	cfg.UpstreamHost = u.Host
	if mode := strings.TrimSpace(os.Getenv("GATEWAY_TEST_SESSION_MODE")); mode != "" {
		cfg.SessionMode = config.SessionMode(mode)
	}
	if cfg.SessionMode == config.SessionModeMock {
		cfg.TestOnly = true
	}
	return cfg, nil
}

func buildResolver(cfg config.Config) (session.Resolver, error) {
	if !cfg.TestOnly {
		return session.Unavailable{}, nil
	}
	switch cfg.SessionMode {
	case config.SessionModeMock:
		return session.NewMockForTests(), nil
	case config.SessionModeCapture:
		return session.NewCaptureForTests(), nil
	case config.SessionModeDerived:
		return session.NewDerived(), nil
	case config.SessionModeHeader:
		return session.NewHeader(), nil
	default:
		return session.Unavailable{}, nil
	}
}

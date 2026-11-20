//go:build !testmode

package main

import (
	"github.com/veronica/ai-gateway/internal/config"
	"github.com/veronica/ai-gateway/internal/session"
)

// testOverride is a no-op in production builds. The `testmode` build tag is
// required to change the upstream destination or to enable mock sessions, so a
// production binary cannot be redirected by environment variables alone.
func testOverride(cfg config.Config) (config.Config, error) { return cfg, nil }

// buildResolver returns the resolvers a production build may use. "unavailable"
// fails closed; "derived" is the approved conversation-identity mechanism.
func buildResolver(cfg config.Config) (session.Resolver, error) {
	if cfg.SessionMode == config.SessionModeHeader {
		return session.NewHeader(), nil
	}
	return session.Unavailable{}, nil
}

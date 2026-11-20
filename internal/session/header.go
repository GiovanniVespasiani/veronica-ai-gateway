package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// HeaderResolver is the production resolver. The client sends the
// authoritative thread id in X-N8N-Thread-Id. The gateway never guesses: a
// missing or malformed header fails closed and no provider request is made.
//
// The provider-visible identifier is a cryptographic hash of the thread id, so
// the raw value never leaves the cluster and never reaches a log line.
type HeaderResolver struct {
	// Headers are checked in order; the first non-empty value wins.
	Headers []string
}

func NewHeader() HeaderResolver {
	return HeaderResolver{Headers: []string{"X-N8N-Thread-Id", "X-Conversation-Id"}}
}

func (h HeaderResolver) Resolve(_ context.Context, meta RequestMeta) (ID, error) {
	raw := strings.TrimSpace(meta.ConversationHeader)
	if raw == "" {
		return "", fmt.Errorf("%w: no conversation header", ErrNoReliableConversationID)
	}
	if err := validateThreadID(raw); err != nil {
		return "", fmt.Errorf("%w: %v", ErrNoReliableConversationID, err)
	}
	sum := sha256.Sum256([]byte(raw))
	return ID("ses_" + hex.EncodeToString(sum[:16])), nil
}

func (h HeaderResolver) Describe() string { return "header (n8n thread id)" }

// validateThreadID accepts UUIDs and comparable opaque ids, and rejects
// anything that could be used to smuggle control characters or unbounded data.
func validateThreadID(v string) error {
	if len(v) < 8 || len(v) > 200 {
		return fmt.Errorf("conversation id length %d out of range", len(v))
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return fmt.Errorf("conversation id contains an invalid character")
		}
	}
	return nil
}

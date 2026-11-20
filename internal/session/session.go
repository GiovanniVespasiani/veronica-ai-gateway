// Package session defines conversation identity for the upstream provider.
//
// OpenCode Go requires a stable per-conversation identifier in
// x-opencode-session. n8n Assistant does not supply one (verified against
// @n8n/instance-ai 1.33.4 / @n8n/agents 0.33.4: no x-opencode-session, no
// previous_response_id, no per-thread metadata). A constant or per-request
// value would misrepresent traffic, so the production resolver is "fail
// closed" and returns ErrNoReliableConversationID.
//
// This package deliberately does NOT implement prefix-hash derivation. That
// approach is a proposal awaiting approval (blocker B2) and must not be
// enabled implicitly.
package session

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
)

// ErrNoReliableConversationID means the gateway cannot truthfully identify the
// conversation, so it must not call the provider.
var ErrNoReliableConversationID = errors.New("no reliable conversation identity is available from the client")

// Format is the documented shape of the provider's session header.
var Format = regexp.MustCompile(`^ses_[A-Za-z0-9_-]{4,120}$`)

// ID is a provider session identifier.
type ID string

// Valid reports whether the identifier has the documented shape.
func (id ID) Valid() bool { return Format.MatchString(string(id)) }

// Prefix returns a short, non-sensitive fragment for logs.
func (id ID) Prefix(n int) string {
	s := string(id)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// RequestMeta is what a resolver may inspect. It intentionally carries no
// prompt text.
type RequestMeta struct {
	RequestID string
	ClientID  string
	Model     string

	// ConversationHeader carries an explicit conversation id supplied by the
	// client (X-Conversation-Id). Empty when the client does not send one.
	ConversationHeader string

	// FirstUserText is the text of the first user message in the conversation.
	// It is stable for the life of the conversation.
	FirstUserText string

	// ConversationHint is populated ONLY by the test harness. Production code
	// never sets it.
	ConversationHint string
}

// Resolver produces a provider session identifier.
//
// Implementations must guarantee: the same conversation yields the same ID,
// different conversations never share an ID, and an ID is never reused across
// clients.
type Resolver interface {
	Resolve(ctx context.Context, meta RequestMeta) (ID, error)
	Describe() string
}

// Unavailable is the production resolver. It fails closed.
type Unavailable struct{}

func (Unavailable) Resolve(context.Context, RequestMeta) (ID, error) {
	return "", ErrNoReliableConversationID
}

func (Unavailable) Describe() string { return "unavailable (fail-closed)" }

// MockForTests maps an explicit conversation hint to a stable synthetic ID.
// It is only constructible by tests; the shipped binary refuses to build it.
type MockForTests struct {
	mu   sync.Mutex
	ids  map[string]ID
	next int
}

// NewMockForTests returns a test-only resolver seeded with session IDs that
// carry the documented prefix. The values are synthetic and must never be
// compared against real provider sessions.
func NewMockForTests() *MockForTests {
	return &MockForTests{ids: make(map[string]ID)}
}

func (m *MockForTests) Resolve(_ context.Context, meta RequestMeta) (ID, error) {
	if meta.ConversationHint == "" {
		return "", fmt.Errorf("%w: test conversation hint missing", ErrNoReliableConversationID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.ids[meta.ConversationHint]; ok {
		return id, nil
	}
	m.next++
	id := ID(fmt.Sprintf("ses_mock_%06d", m.next))
	m.ids[meta.ConversationHint] = id
	return id, nil
}

func (m *MockForTests) Describe() string { return "mock-for-tests" }

// CaptureForTests issues one synthetic identifier per request.
//
// It exists solely to observe the real n8n request shape in an isolated test.
// It is NOT a session solution: it defeats provider routing affinity and prompt
// caching. It is never constructible from a production binary.
type CaptureForTests struct {
	mu  sync.Mutex
	seq int
}

func NewCaptureForTests() *CaptureForTests { return &CaptureForTests{} }

func (c *CaptureForTests) Resolve(context.Context, RequestMeta) (ID, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	return ID(fmt.Sprintf("ses_capture_%06d", c.seq)), nil
}

func (c *CaptureForTests) Describe() string {
	return "protocol-capture-for-tests (NOT a session solution)"
}

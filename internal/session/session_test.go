package session_test

import (
	"context"
	"errors"
	"testing"

	"github.com/veronica/ai-gateway/internal/session"
)

func TestUnavailableFailsClosed(t *testing.T) {
	var r session.Resolver = session.Unavailable{}
	_, err := r.Resolve(context.Background(), session.RequestMeta{RequestID: "r1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, session.ErrNoReliableConversationID) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMockIsStablePerConversation(t *testing.T) {
	r := session.NewMockForTests()
	ctx := context.Background()

	a1, err := r.Resolve(ctx, session.RequestMeta{ConversationHint: "a"})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := r.Resolve(ctx, session.RequestMeta{ConversationHint: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b1, err := r.Resolve(ctx, session.RequestMeta{ConversationHint: "b"})
	if err != nil {
		t.Fatal(err)
	}

	if a1 != a2 {
		t.Fatalf("not stable: %q vs %q", a1, a2)
	}
	if a1 == b1 {
		t.Fatalf("not distinct: %q", a1)
	}
	if !a1.Valid() || !b1.Valid() {
		t.Fatalf("generated identifiers are not in the documented shape: %q %q", a1, b1)
	}
}

func TestMockRequiresHint(t *testing.T) {
	r := session.NewMockForTests()
	if _, err := r.Resolve(context.Background(), session.RequestMeta{}); err == nil {
		t.Fatal("expected an error without a conversation hint")
	}
}

func TestFormat(t *testing.T) {
	valid := []string{"ses_mock_000001", "ses_abc-def_123"}
	invalid := []string{"", "ses_", "ses_a", "mock_000001", "ses_ab cd", "ses_" + string(make([]byte, 300))}
	for _, s := range valid {
		if !session.ID(s).Valid() {
			t.Fatalf("%q should be valid", s)
		}
	}
	for _, s := range invalid {
		if session.ID(s).Valid() {
			t.Fatalf("%q should be invalid", s)
		}
	}
}

func TestPrefix(t *testing.T) {
	id := session.ID("ses_mock_000001")
	if got := id.Prefix(8); got != "ses_mock" {
		t.Fatalf("unexpected prefix %q", got)
	}
	if got := id.Prefix(64); got != string(id) {
		t.Fatalf("short identifiers must be returned whole: %q", got)
	}
}

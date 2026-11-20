package session_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/veronica/ai-gateway/internal/session"
)

func TestHeaderResolverStableAndOpaque(t *testing.T) {
	r := session.NewHeader()
	ctx := context.Background()

	a1, err := r.Resolve(ctx, session.RequestMeta{ConversationHeader: "563f9220-e9d8-4f60-b8f2-1d165e50471a"})
	if err != nil {
		t.Fatal(err)
	}
	a2, err := r.Resolve(ctx, session.RequestMeta{ConversationHeader: "563f9220-e9d8-4f60-b8f2-1d165e50471a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Resolve(ctx, session.RequestMeta{ConversationHeader: "9c1b8f2a-0000-4000-8000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}

	if a1 != a2 {
		t.Fatal("header resolver is not stable")
	}
	if a1 == b {
		t.Fatal("distinct threads collapsed to one session")
	}
	if !a1.Valid() {
		t.Fatalf("not in documented shape: %q", a1)
	}
	if strings.Contains(string(a1), "563f9220") {
		t.Fatal("session id exposes the raw thread id")
	}
}

func TestHeaderResolverRejectsBadInput(t *testing.T) {
	r := session.NewHeader()
	for _, v := range []string{
		"",
		"   ",
		"short",
		"has spaces in it",
		"has\nnewline1234",
		strings.Repeat("a", 201),
		"../../etc/passwd",
	} {
		_, err := r.Resolve(context.Background(), session.RequestMeta{ConversationHeader: v})
		if !errors.Is(err, session.ErrNoReliableConversationID) {
			t.Fatalf("%q should fail closed, got %v", v, err)
		}
	}
}

func TestHeaderResolverAcceptsUUIDAndOpaque(t *testing.T) {
	r := session.NewHeader()
	for _, v := range []string{
		"563f9220-e9d8-4f60-b8f2-1d165e50471a",
		"thread_01JABCXYZ",
		"ns:12345.thread-7",
	} {
		if _, err := r.Resolve(context.Background(), session.RequestMeta{ConversationHeader: v}); err != nil {
			t.Fatalf("%q rejected: %v", v, err)
		}
	}
}

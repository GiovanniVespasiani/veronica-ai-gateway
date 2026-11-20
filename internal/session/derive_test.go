package session_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/veronica/ai-gateway/internal/session"
)

func TestDeriveIDIsStableAndDistinct(t *testing.T) {
	a := session.DeriveID("deepseek-v4.1-flash", "hello")
	b := session.DeriveID("deepseek-v4.1-flash", "hello")
	c := session.DeriveID("deepseek-v4.1-flash", "hello!")
	d := session.DeriveID("other-model", "hello")

	if a != b {
		t.Fatal("derivation is not deterministic")
	}
	if a == c || a == d {
		t.Fatal("derivation is not sensitive to its inputs")
	}
	if !a.Valid() {
		t.Fatalf("derived id is not in the documented shape: %q", a)
	}
	if strings.Contains(string(a), "hello") {
		t.Fatal("derived id leaks content")
	}
	if len(a) > 40 {
		t.Fatalf("derived id is unexpectedly long: %d", len(a))
	}
}

func TestDerivedResolverContinuity(t *testing.T) {
	r := session.NewDerived()
	ctx := context.Background()

	first, err := r.Resolve(ctx, session.RequestMeta{Model: "m", FirstUserText: "build a workflow"})
	if err != nil {
		t.Fatal(err)
	}
	// Later turns replay history; the first user text is unchanged.
	later, err := r.Resolve(ctx, session.RequestMeta{Model: "m", FirstUserText: "build a workflow"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.Resolve(ctx, session.RequestMeta{Model: "m", FirstUserText: "build a different workflow"})
	if err != nil {
		t.Fatal(err)
	}

	if first != later {
		t.Fatalf("session changed across turns: %q vs %q", first, later)
	}
	if first == other {
		t.Fatalf("different conversations share a session: %q", first)
	}
}

func TestDerivedResolverPrefersExplicitHeader(t *testing.T) {
	r := session.NewDerived()
	id, err := r.Resolve(context.Background(), session.RequestMeta{
		Model:              "m",
		FirstUserText:      "hello",
		ConversationHeader: "ses_explicit_conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(id) != "ses_explicit_conversation" {
		t.Fatalf("header was not preferred: %q", id)
	}

	// An invalid header falls back to derivation rather than failing.
	id2, err := r.Resolve(context.Background(), session.RequestMeta{
		Model:              "m",
		FirstUserText:      "hello",
		ConversationHeader: "not a valid id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id2 != session.DeriveID("m", "hello") {
		t.Fatalf("invalid header did not fall back: %q", id2)
	}
}

func TestDerivedResolverFailsClosedWithoutUserText(t *testing.T) {
	r := session.NewDerived()
	_, err := r.Resolve(context.Background(), session.RequestMeta{Model: "m"})
	if !errors.Is(err, session.ErrNoReliableConversationID) {
		t.Fatalf("expected fail-closed error, got %v", err)
	}
}

func TestFirstUserText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
		ok   bool
	}{
		{"simple", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "hi", true},
		{"after system", `{"model":"m","messages":[{"role":"system","content":"big prompt"},{"role":"user","content":"hi"}]}`, "hi", true},
		{"parts", `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"b"}]}]}`, "ab", true},
		{"null content", `{"model":"m","messages":[{"role":"user","content":null}]}`, "", true},
		{"no user", `{"model":"m","messages":[{"role":"system","content":"s"}]}`, "", false},
		{"empty", `{}`, "", false},
		{"not json", `nope`, "", false},
		{"nested decoy", `{"model":"m","messages":[{"role":"assistant","content":"x"}],"tools":[{"function":{"name":"messages"}}]}`, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := session.FirstUserText([]byte(c.body))
			if ok != c.ok || got != c.want {
				t.Fatalf("FirstUserText = (%q,%v), want (%q,%v)", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestFirstUserTextAllocationIsBounded(t *testing.T) {
	// A large history must not copy the body: the first user message is found
	// at the front, so cost stays proportional to that message.
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[{"role":"system","content":"`)
	b.WriteString(strings.Repeat("s", 2<<20))
	b.WriteString(`"},{"role":"user","content":"find me"},`)
	for i := 0; i < 50; i++ {
		fmt.Fprintf(&b, `{"role":"assistant","content":"%s"},`, strings.Repeat("a", 1024))
	}
	b.WriteString(`{"role":"user","content":"tail"}]}`)

	body := []byte(b.String())
	got, ok := session.FirstUserText(body)
	if !ok || got != "find me" {
		t.Fatalf("unexpected result %q %v", got, ok)
	}
}

package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// DerivedResolver builds a stable conversation identifier from conversation
// metadata the client does send.
//
// Preferred input: an explicit conversation header. n8n Assistant does not send
// one, so the fallback is the conversation's first user message: it is present
// in every request of a conversation (the client replays full history) and
// never changes for the life of that conversation.
//
// Collision semantics are documented and bounded: two conversations that begin
// with byte-identical first user messages share a routing bucket. Requests are
// stateless and carry their own history, so this affects cache locality only,
// never message content. Callers that need exactness can send X-Conversation-Id.
type DerivedResolver struct {
	// Header is the request header carrying an explicit conversation id.
	Header string
}

// NewDerived returns the production resolver.
func NewDerived() DerivedResolver { return DerivedResolver{Header: "X-Conversation-Id"} }

func (d DerivedResolver) Resolve(_ context.Context, meta RequestMeta) (ID, error) {
	if id := ID(meta.ConversationHeader); id.Valid() {
		return id, nil
	}
	if meta.FirstUserText == "" {
		return "", ErrNoReliableConversationID
	}
	return DeriveID(meta.Model, meta.FirstUserText), nil
}

func (d DerivedResolver) Describe() string { return "derived (first user message)" }

// DeriveID is the pure, restart-stable derivation. It is exported so tests and
// future clients can reproduce a value.
func DeriveID(model, firstUserText string) ID {
	sum := sha256.Sum256([]byte(model + "\x00" + firstUserText))
	return ID("ses_" + hex.EncodeToString(sum[:16]))
}

// skipValue lets the decoder walk a JSON value without materialising it.
type skipValue struct{}

func (*skipValue) UnmarshalJSON([]byte) error { return nil }

// FirstUserText extracts the text of the first message whose role is "user",
// without copying the rest of the body.
//
// It walks the top-level object token by token, records byte offsets for each
// element of "messages", and only decodes the element that is a user message.
// A 4 MiB Assistant history therefore costs one small allocation, not a copy of
// the body.
func FirstUserText(body []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))

	depth := 0
	found := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", false
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth <= 0 {
					return "", false
				}
			}
		case string:
			if depth == 1 && t == "messages" {
				next, err := dec.Token()
				if err != nil {
					return "", false
				}
				if d, ok := next.(json.Delim); !ok || d != '[' {
					return "", false
				}
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found {
		return "", false
	}

	for dec.More() {
		start := dec.InputOffset()
		if err := dec.Decode(&skipValue{}); err != nil {
			return "", false
		}
		end := dec.InputOffset()
		raw := bytes.TrimLeft(body[start:end], ", \t\r\n")
		if len(raw) == 0 || raw[0] != '{' {
			continue
		}
		// Cheap pre-filter: most elements are system/tool messages.
		if !bytes.Contains(raw, []byte(`"user"`)) {
			continue
		}
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil || m.Role != "user" {
			continue
		}
		return contentText(m.Content), true
	}
	return "", false
}

func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var b bytes.Buffer
	for _, p := range parts {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

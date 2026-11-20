// Command mockupstream is a local, OpenAI-compatible stand-in for OpenCode Go.
//
// It exists so the gateway can be exercised without any live provider call.
// It is a development tool: it accepts a fixed token, records metadata only,
// and never talks to the internet.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type requestSummary struct {
	Seq          int               `json:"seq"`
	Path         string            `json:"path"`
	Bytes        int               `json:"bytes"`
	Model        string            `json:"model"`
	Stream       bool              `json:"stream"`
	Session      string            `json:"session"`
	TopKeys      []string          `json:"top_keys"`
	ToolNames    []string          `json:"tool_names"`
	KeyHashes    map[string]string `json:"key_hashes"`
	HeaderNames  []string          `json:"header_names"`
	HeaderHashes map[string]string `json:"header_hashes"`
}

type state struct {
	mu             sync.Mutex
	requests       int
	rejected       int
	lastSession    string
	lastModel      string
	lastBody       []byte
	lastAuthHeader string
	summaries      []requestSummary
}

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "listen address")
	token := flag.String("token", "mock-upstream-token-0123456789", "expected bearer token")
	model := flag.String("model", "deepseek-v4.1-flash", "model id to advertise")
	flag.Parse()

	st := &state{}
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r, *token) {
			http.Error(w, `{"error":{"code":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []map[string]any{{
				"id":       *model,
				"object":   "model",
				"owned_by": "mock",
			}},
		})
	})

	mux.HandleFunc("/openai/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if !authorized(r, *token) {
			http.Error(w, `{"error":{"code":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		sess := r.Header.Get("x-opencode-session")
		if !strings.HasPrefix(sess, "ses_") {
			http.Error(w, `{"error":{"code":"MissingSessionID","message":"x-opencode-session required"}}`, http.StatusBadRequest)
			return
		}

		bodyRaw, _ := io.ReadAll(io.LimitReader(r.Body, 32<<20))
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
			Tools  []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(bodyRaw, &body); err != nil {
			http.Error(w, `{"error":{"code":"invalid_json"}}`, http.StatusBadRequest)
			return
		}

		st.mu.Lock()
		st.requests++
		st.lastSession = sess
		st.lastModel = body.Model
		st.lastAuthHeader = r.Header.Get("Authorization")
		st.summaries = append(st.summaries, summarise(len(st.summaries)+1, r.URL.Path, bodyRaw, body.Model, body.Stream, sess, r.Header))
		st.mu.Unlock()

		wantTool := false
		for _, m := range body.Messages {
			if m.Role == "tool" {
				wantTool = false
				break
			}
		}
		toolName := "mock_tool"
		if len(body.Tools) > 0 {
			wantTool = true
			if n := body.Tools[0].Function.Name; n != "" {
				toolName = n
			}
		}

		if !body.Stream {
			msg := map[string]any{"role": "assistant", "content": "mock reply"}
			finish := "stop"
			if wantTool {
				msg = map[string]any{
					"role":    "assistant",
					"content": nil,
					"tool_calls": []map[string]any{{
						"id":   "call_mock_1",
						"type": "function",
						"function": map[string]any{
							"name":      toolName,
							"arguments": `{"query":"mock"}`,
						},
					}},
				}
				finish = "tool_calls"
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion",
				"created": time.Now().Unix(),
				"model":   body.Model,
				"choices": []map[string]any{{
					"index":         0,
					"message":       msg,
					"finish_reason": finish,
				}},
				"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 3, "total_tokens": 14},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)

		send := func(v any) {
			b, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}

		if wantTool {
			send(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": body.Model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"index": 0, "id": "call_mock_1", "type": "function",
						"function": map[string]any{"name": toolName, "arguments": `{}`},
					}},
				}, "finish_reason": nil}},
			})
			send(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": body.Model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{
					"tool_calls": []map[string]any{{"index": 0, "function": map[string]any{"arguments": ""}}},
				}, "finish_reason": "tool_calls"}},
			})
		} else {
			for _, piece := range []string{"mock ", "stream ", "reply"} {
				send(map[string]any{
					"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": body.Model,
					"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": piece}, "finish_reason": nil}},
				})
				time.Sleep(40 * time.Millisecond)
			}
			send(map[string]any{
				"id": "chatcmpl-mock", "object": "chat.completion.chunk", "model": body.Model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 3, "total_tokens": 14},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})

	mux.HandleFunc("/__admin/state", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"requests":     st.requests,
			"rejected":     st.rejected,
			"last_session": st.lastSession,
			"last_model":   st.lastModel,
			"auth_present": strings.HasPrefix(st.lastAuthHeader, "Bearer "),
		})
	})

	mux.HandleFunc("/__admin/summary", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		defer st.mu.Unlock()
		out := append([]requestSummary(nil), st.summaries...)
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("/__admin/reset", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.requests, st.rejected, st.lastSession, st.lastModel, st.lastAuthHeader = 0, 0, "", "", ""
		st.summaries = nil
		st.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
	})

	log.Printf("mock upstream listening on %s (model %s)", *addr, *model)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func authorized(r *http.Request, token string) bool {
	h := r.Header.Get("Authorization")
	return strings.HasPrefix(h, "Bearer ") && strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")) == token
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// summarise records structural fingerprints only: top-level key names and a
// truncated SHA-256 of each value. Values themselves are never stored, so the
// tool can show whether a field is stable across turns without retaining prompt
// content.
func summarise(seq int, path string, raw []byte, model string, stream bool, sess string, h http.Header) requestSummary {
	s := requestSummary{
		Seq: seq, Path: path, Bytes: len(raw), Model: model, Stream: stream, Session: sess,
		KeyHashes:    map[string]string{},
		HeaderHashes: map[string]string{},
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		for k, v := range m {
			s.KeyHashes[k] = hash12(v)
			s.TopKeys = append(s.TopKeys, k)
		}
		sort.Strings(s.TopKeys)
	}
	var probe struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(raw, &probe) == nil {
		for _, t := range probe.Tools {
			s.ToolNames = append(s.ToolNames, t.Function.Name)
		}
	}
	for k, v := range h {
		name := strings.ToLower(k)
		s.HeaderNames = append(s.HeaderNames, name)
		s.HeaderHashes[name] = hash12([]byte(strings.Join(v, ",")))
	}
	sort.Strings(s.HeaderNames)
	return s
}

func hash12(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

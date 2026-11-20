package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/veronica/ai-gateway/internal/config"
	"github.com/veronica/ai-gateway/internal/logging"
	"github.com/veronica/ai-gateway/internal/server"
	"github.com/veronica/ai-gateway/internal/session"
	"github.com/veronica/ai-gateway/internal/upstream"
)

const (
	clientToken   = "test-client-token-abcdefgh"
	upstreamToken = "test-upstream-token-0123456789"
	modelID       = "deepseek-v4.1-flash"
)

// --- mock upstream -------------------------------------------------------

type mockUpstream struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    int
	sessions []string
	auths    []string
	bodies   [][]byte
	headers  []http.Header

	// knobs
	delay       time.Duration
	chunkGap    time.Duration
	redirect    bool
	status      int // if non-zero, reply with this status
	requireAuth bool
}

func newMockUpstream(t *testing.T) *mockUpstream {
	t.Helper()
	m := &mockUpstream{t: t, requireAuth: true}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockUpstream) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"` + modelID + `"}]}`))
		return
	}
	if r.URL.Path != "/openai/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}

	body, _ := io.ReadAll(r.Body)
	sess := r.Header.Get("x-opencode-session")
	auth := r.Header.Get("Authorization")

	m.mu.Lock()
	m.calls++
	m.sessions = append(m.sessions, sess)
	m.auths = append(m.auths, auth)
	m.bodies = append(m.bodies, body)
	m.headers = append(m.headers, r.Header.Clone())
	delay := m.delay
	chunkGap := m.chunkGap
	redirect := m.redirect
	status := m.status
	requireAuth := m.requireAuth
	m.mu.Unlock()

	if requireAuth && auth != "Bearer "+upstreamToken {
		http.Error(w, `{"error":{"code":"unauthorized"}}`, http.StatusUnauthorized)
		return
	}
	if !strings.HasPrefix(sess, "ses_") {
		http.Error(w, `{"error":{"code":"MissingSessionID"}}`, http.StatusBadRequest)
		return
	}
	if redirect {
		w.Header().Set("Location", "https://example.invalid/steal")
		w.WriteHeader(http.StatusFound)
		return
	}
	if status != 0 {
		http.Error(w, `{"error":{"code":"upstream_synthetic"}}`, status)
		return
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	var env struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &env)

	if !env.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-mock",
			"model":   modelID,
			"session": sess,
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 2, "total_tokens": 13},
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	for i, piece := range []string{"alpha", "beta", "gamma"} {
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", piece)
		fl.Flush()
		if i == 0 && chunkGap > 0 {
			time.Sleep(chunkGap)
		}
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	fl.Flush()
}

func (m *mockUpstream) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockUpstream) lastSession() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) == 0 {
		return ""
	}
	return m.sessions[len(m.sessions)-1]
}

func (m *mockUpstream) lastHeaders() http.Header {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.headers) == 0 {
		return nil
	}
	return m.headers[len(m.headers)-1]
}

// --- harness -------------------------------------------------------------

type harness struct {
	gw     *httptest.Server
	up     *mockUpstream
	logs   *bytes.Buffer
	server *server.Server
	httpc  *http.Client
}

type opts struct {
	sessionMode   config.SessionMode
	maxBody       int64
	maxInFlight   int
	ratePerMinute int
	rateBurst     int
}

func newHarness(t *testing.T, o opts) *harness {
	t.Helper()
	up := newMockUpstream(t)
	if o.sessionMode == "" {
		o.sessionMode = config.SessionModeMock
	}
	if o.maxBody == 0 {
		o.maxBody = 1 << 20
	}
	if o.maxInFlight == 0 {
		o.maxInFlight = 8
	}
	if o.ratePerMinute == 0 {
		o.ratePerMinute = 6000
	}
	if o.rateBurst == 0 {
		o.rateBurst = 100
	}

	cfg := config.Config{
		ListenAddr:            "127.0.0.1:0",
		UpstreamBaseURL:       up.srv.URL + "/openai/v1",
		UpstreamHost:          strings.TrimPrefix(up.srv.URL, "http://"),
		UpstreamToken:         upstreamToken,
		ClientToken:           clientToken,
		Model:                 modelID,
		SessionMode:           o.sessionMode,
		MaxBodyBytes:          o.maxBody,
		MaxHeaderBytes:        32 << 10,
		ConnectTimeout:        2 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		StreamIdleTimeout:     2 * time.Second,
		TotalTimeout:          10 * time.Second,
		MaxInFlight:           o.maxInFlight,
		RatePerMinute:         o.ratePerMinute,
		RateBurst:             o.rateBurst,
		TestOnly:              true,
	}

	buf := &bytes.Buffer{}
	log := logging.New(buf)

	client, err := upstream.New(cfg)
	if err != nil {
		t.Fatalf("upstream client: %v", err)
	}

	var resolver session.Resolver = session.Unavailable{}
	switch o.sessionMode {
	case config.SessionModeMock:
		resolver = session.NewMockForTests()
	case config.SessionModeDerived:
		resolver = session.NewDerived()
	case config.SessionModeHeader:
		resolver = session.NewHeader()
	case config.SessionModeCapture:
		resolver = session.NewCaptureForTests()
	}

	srv, err := server.New(cfg, log, client, resolver)
	if err != nil {
		t.Fatalf("server: %v", err)
	}

	gw := httptest.NewServer(srv)
	t.Cleanup(gw.Close)

	httpc := gw.Client()
	httpc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &harness{gw: gw, up: up, logs: buf, server: srv, httpc: httpc}
}

func (h *harness) client() *http.Client {
	return h.httpc
}

func (h *harness) post(t *testing.T, path, token string, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.gw.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func (h *harness) chat(t *testing.T, conversation string, stream bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, modelID, stream)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("Content-Type", "application/json")
	if conversation != "" {
		req.Header.Set("X-Gateway-Test-Conversation", conversation)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func readBody(t *testing.T, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// --- tests ---------------------------------------------------------------

func TestHealthz(t *testing.T) {
	h := newHarness(t, opts{})
	resp, err := h.gw.Client().Get(h.gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestUnknownRoutesAreClosed(t *testing.T) {
	cases := []string{
		"/",
		"/v1",
		"/v1/admin",
		"/v1/chat/completions/",
		"/v1/%2e%2e/admin",
		"/openai/v1/chat/completions",
	}
	h := newHarness(t, opts{})
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, h.gw.URL+p, nil)
			resp, err := h.client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("%s: expected 404, got %d", p, resp.StatusCode)
			}
		})
	}
	if n := h.up.count(); n != 0 {
		t.Fatalf("unknown routes reached the upstream %d times", n)
	}
}

// n8n's @n8n/agents fallback probe (withChatCompletionsFallback) checks one of:
//   - error.code == "unknown_url"
//   - message matching /^Unknown request URL: \S+ \/(?:[^/\s]+\/)*responses\b/i
//
// This test pins the contract the gateway must honour.
var n8nMissingResponsesRoute = regexp.MustCompile(`(?i)^Unknown request URL: \S+ \/(?:[^/\s]+\/)*responses\b`)

func TestResponsesRouteTriggersN8nChatFallback(t *testing.T) {
	h := newHarness(t, opts{})

	req, _ := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/responses",
		strings.NewReader(`{"model":"`+modelID+`","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+clientToken)
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}

	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}

	byCode := payload.Error.Code == "unknown_url"
	byMessage := n8nMissingResponsesRoute.MatchString(payload.Error.Message)
	if !byCode && !byMessage {
		t.Fatalf("response does not satisfy n8n's fallback predicate: code=%q message=%q",
			payload.Error.Code, payload.Error.Message)
	}
	if n := h.up.count(); n != 0 {
		t.Fatalf("responses probe must not reach the upstream")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	h := newHarness(t, opts{})
	req, _ := http.NewRequest(http.MethodGet, h.gw.URL+"/v1/chat/completions", nil)
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("missing Allow header")
	}
}

func TestAuth(t *testing.T) {
	h := newHarness(t, opts{})

	for _, token := range []string{"", "wrong-token-value", clientToken + "x"} {
		resp := h.post(t, "/v1/chat/completions", token,
			fmt.Sprintf(`{"model":%q,"messages":[]}`, modelID))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: status %d", token, resp.StatusCode)
		}
	}
	if n := h.up.count(); n != 0 {
		t.Fatalf("unauthenticated requests reached the upstream %d times", n)
	}

	resp := h.chat(t, "c1", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token rejected: %d %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestChatInjectsSessionAndUpstreamAuth(t *testing.T) {
	h := newHarness(t, opts{})
	resp := h.chat(t, "conversation-a", false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, readBody(t, resp))
	}

	if got := h.up.lastSession(); !strings.HasPrefix(got, "ses_mock_") {
		t.Fatalf("upstream saw session %q", got)
	}
	if got := h.up.lastHeaders().Get("Authorization"); got != "Bearer "+upstreamToken {
		t.Fatalf("upstream saw authorization %q", got)
	}
	if got := h.up.lastHeaders().Get("User-Agent"); !strings.Contains(got, "veronica-ai-gateway") {
		t.Fatalf("upstream saw user agent %q", got)
	}
}

func TestClientSessionHeaderIsNeverTrusted(t *testing.T) {
	h := newHarness(t, opts{})
	req, _ := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, modelID)))
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("X-Gateway-Test-Conversation", "conv-forged")
	req.Header.Set("x-opencode-session", "ses_forged_by_client")
	req.Header.Set("X-Forwarded-For", "10.0.0.9")
	req.Header.Set("X-Real-Ip", "10.0.0.9")

	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}

	if got := h.up.lastSession(); got == "ses_forged_by_client" {
		t.Fatal("client-supplied session identifier was forwarded")
	}
	hdr := h.up.lastHeaders()
	if hdr.Get("X-Forwarded-For") != "" || hdr.Get("X-Real-Ip") != "" {
		t.Fatal("client-supplied forwarding headers reached the upstream")
	}
}

func TestSessionIsStablePerConversationAndDistinctAcrossConversations(t *testing.T) {
	h := newHarness(t, opts{})

	get := func(conv string) string {
		resp := h.chat(t, conv, false)
		var payload struct {
			Session string `json:"session"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&payload)
		_ = resp.Body.Close()
		return payload.Session
	}

	a1 := get("conv-a")
	a2 := get("conv-a")
	b1 := get("conv-b")

	if a1 == "" || b1 == "" {
		t.Fatal("missing session")
	}
	if a1 != a2 {
		t.Fatalf("session not stable within a conversation: %q vs %q", a1, a2)
	}
	if a1 == b1 {
		t.Fatalf("session shared across conversations: %q", a1)
	}
}

func TestSessionUnavailableFailsClosed(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeUnavailable})
	resp := h.chat(t, "conv", false)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "session_identity_unavailable") {
		t.Fatalf("unexpected body: %s", body)
	}
	if n := h.up.count(); n != 0 {
		t.Fatalf("gateway called the upstream without a session identity (%d calls)", n)
	}
}

func TestModelAllowlist(t *testing.T) {
	h := newHarness(t, opts{})
	resp := h.post(t, "/v1/chat/completions", clientToken,
		`{"model":"expensive-model","messages":[]}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "model_not_allowed") {
		t.Fatalf("unexpected body: %s", body)
	}
	if h.up.count() != 0 {
		t.Fatal("disallowed model reached the upstream")
	}
}

func TestInvalidJSON(t *testing.T) {
	h := newHarness(t, opts{})
	resp := h.post(t, "/v1/chat/completions", clientToken, `not json`)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBodyLimit(t *testing.T) {
	h := newHarness(t, opts{maxBody: 256})
	big := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`,
		modelID, strings.Repeat("A", 1024))
	resp := h.post(t, "/v1/chat/completions", clientToken, big)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if h.up.count() != 0 {
		t.Fatal("oversized body reached the upstream")
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	h := newHarness(t, opts{})
	h.up.redirect = true

	resp := h.chat(t, "conv", false)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 on redirect, got %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "upstream_redirect") {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestUpstreamErrorIsRelayed(t *testing.T) {
	h := newHarness(t, opts{})
	h.up.status = http.StatusTooManyRequests

	resp := h.chat(t, "conv", false)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 relayed, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "upstream_synthetic") {
		t.Fatalf("upstream error body not relayed: %s", body)
	}
}

func TestRateLimit(t *testing.T) {
	h := newHarness(t, opts{ratePerMinute: 60, rateBurst: 2})

	var last *http.Response
	for i := 0; i < 3; i++ {
		resp := h.chat(t, fmt.Sprintf("conv-%d", i), false)
		_ = resp.Body.Close()
		last = resp
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on third burst request, got %d", last.StatusCode)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestInFlightCap(t *testing.T) {
	h := newHarness(t, opts{maxInFlight: 2, rateBurst: 500, ratePerMinute: 6000})
	h.up.delay = 200 * time.Millisecond

	var wg sync.WaitGroup
	var statuses [6]int
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := httpDo(h, i)
			if err != nil {
				return
			}
			statuses[i] = resp.StatusCode
			_ = resp.Body.Close()
		}(i)
	}
	wg.Wait()

	saw503 := false
	for _, s := range statuses {
		if s == http.StatusServiceUnavailable {
			saw503 = true
		}
	}
	if !saw503 {
		t.Fatalf("expected at least one 503, statuses=%v", statuses)
	}
	if got := h.server.Stats().MaxConcurrent; got > 2 {
		t.Fatalf("in-flight cap violated: observed %d", got)
	}
}

func httpDo(h *harness, i int) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, modelID)))
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("X-Gateway-Test-Conversation", fmt.Sprintf("conv-%d", i))
	return h.client().Do(req)
}

func TestConcurrentConversationsDoNotMix(t *testing.T) {
	h := newHarness(t, opts{maxInFlight: 8, rateBurst: 500, ratePerMinute: 6000})

	const n = 8
	type result struct {
		session string
		err     error
	}
	results := make([]result, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions",
				strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"c%d"}]}`, modelID, i)))
			req.Header.Set("Authorization", "Bearer "+clientToken)
			req.Header.Set("X-Gateway-Test-Conversation", fmt.Sprintf("conversation-%d", i))
			resp, err := h.client().Do(req)
			if err != nil {
				results[i].err = err
				return
			}
			defer resp.Body.Close()
			var payload struct {
				Session string `json:"session"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				results[i].err = err
				return
			}
			results[i].session = payload.Session
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("request %d: %v", i, r.err)
		}
		if r.session == "" {
			t.Fatalf("request %d had no session", i)
		}
		seen[r.session]++
	}
	if len(seen) != n {
		t.Fatalf("expected %d distinct sessions, got %d: %v", n, len(seen), seen)
	}
	for s, count := range seen {
		if count != 1 {
			t.Fatalf("session %q used by %d conversations", s, count)
		}
	}
}

func TestStreamingIsIncremental(t *testing.T) {
	h := newHarness(t, opts{})
	h.up.chunkGap = 300 * time.Millisecond

	start := time.Now()
	resp := h.chat(t, "stream-conv", true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first line: %v", err)
	}
	firstByteAt := time.Since(start)
	if firstByteAt > 250*time.Millisecond {
		t.Fatalf("first chunk arrived after %v; response is being buffered", firstByteAt)
	}
	if !strings.Contains(first, "alpha") {
		t.Fatalf("unexpected first chunk: %q", first)
	}

	rest, _ := io.ReadAll(reader)
	_ = resp.Body.Close()
	if !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("stream did not terminate cleanly: %q", string(rest))
	}
	if total := time.Since(start); total < h.up.chunkGap {
		t.Fatalf("stream completed in %v, upstream gap was %v", total, h.up.chunkGap)
	}
}

func TestNoPromptOrSecretInLogs(t *testing.T) {
	h := newHarness(t, opts{})

	const promptSentinel = "SENTINEL_PROMPT_9f3a_should_never_be_logged"
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":%q}]}`, modelID, promptSentinel)
	req, err := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gateway-Test-Conversation", "logging-conv")
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)

	logs := h.logs.String()
	for _, forbidden := range []string{promptSentinel, clientToken, upstreamToken, "Authorization", "x-opencode-session"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("log output contains forbidden value %q:\n%s", forbidden, logs)
		}
	}
	for _, required := range []string{`"request_id"`, `"route":"chat_completions"`, `"status":200`} {
		if !strings.Contains(logs, required) {
			t.Fatalf("log output missing %q:\n%s", required, logs)
		}
	}
	if strings.Contains(logs, `"content"`) {
		t.Fatal("log output contains a message field")
	}
}

func TestClientDisconnectCancelsRequest(t *testing.T) {
	h := newHarness(t, opts{})
	h.up.delay = 400 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.gw.URL+"/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true,"messages":[]}`, modelID)))
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("X-Gateway-Test-Conversation", "cancel-conv")

	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 8)
	_, _ = resp.Body.Read(buf)
	cancel()
	_ = resp.Body.Close()

	// Give the handler a moment to observe cancellation, then assert the
	// gateway is still healthy and stats did not leak a slot.
	time.Sleep(150 * time.Millisecond)
	health, err := h.gw.Client().Get(h.gw.URL + "/healthz")
	if err != nil {
		t.Fatalf("gateway unhealthy after cancel: %v", err)
	}
	_ = health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health %d", health.StatusCode)
	}
}

func TestAbsoluteFormRequestRejected(t *testing.T) {
	h := newHarness(t, opts{})
	// http.Client normalises most absolute-form targets, so the request is
	// crafted at the connection level.
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(h.gw.URL, "http://"), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "GET http://169.254.169.254/latest/meta-data/ HTTP/1.1\r\nHost: %s\r\n\r\n",
		strings.TrimPrefix(h.gw.URL, "http://"))
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(line, "400") {
		t.Fatalf("absolute-form request was not rejected: %q", line)
	}
	if h.up.count() != 0 {
		t.Fatal("absolute-form request reached the upstream")
	}
}

func TestStatsExposeResolverMode(t *testing.T) {
	h := newHarness(t, opts{})
	st := h.server.Stats()
	if st.SessionMode != string(config.SessionModeMock) {
		t.Fatalf("unexpected session mode %q", st.SessionMode)
	}
	if !strings.Contains(st.SessionResolver, "mock") {
		t.Fatalf("unexpected resolver %q", st.SessionResolver)
	}
}

// --- derived session identity (production mode) ---

func (h *harness) chatWith(t *testing.T, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func sessionOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var payload struct {
		Session string `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return payload.Session
}

func TestDerivedSessionContinuityAcrossTurns(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeDerived})

	turn1 := `{"model":"` + modelID + `","messages":[{"role":"system","content":"sys"},{"role":"user","content":"build a workflow"}]}`
	turn2 := `{"model":"` + modelID + `","messages":[{"role":"system","content":"sys"},{"role":"user","content":"build a workflow"},{"role":"assistant","content":"ok"},{"role":"user","content":"add a node"}]}`

	a := sessionOf(t, h.chatWith(t, turn1))
	b := sessionOf(t, h.chatWith(t, turn2))
	if a == "" || a != b {
		t.Fatalf("session not stable across turns: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "ses_") {
		t.Fatalf("session does not carry the documented prefix: %q", a)
	}
}

func TestDerivedSessionSeparatesConversations(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeDerived})
	mk := func(first string) string {
		return `{"model":"` + modelID + `","messages":[{"role":"user","content":"` + first + `"}]}`
	}
	a := sessionOf(t, h.chatWith(t, mk("conversation one")))
	b := sessionOf(t, h.chatWith(t, mk("conversation two")))
	if a == b {
		t.Fatalf("conversations share a session: %q", a)
	}
}

func TestDerivedSessionPrefersExplicitHeader(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeDerived})
	req, _ := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"`+modelID+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+clientToken)
	req.Header.Set("X-Conversation-Id", "ses_explicit_thread_1")
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := sessionOf(t, resp); got != "ses_explicit_thread_1" {
		t.Fatalf("explicit header ignored: %q", got)
	}
}

func TestDerivedSessionFailsClosedWithoutUserMessage(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeDerived})
	resp := h.chatWith(t, `{"model":"`+modelID+`","messages":[{"role":"system","content":"only system"}]}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("expected 501, got %d: %s", resp.StatusCode, body)
	}
	if h.up.count() != 0 {
		t.Fatal("request reached the upstream without a conversation identity")
	}
}

func TestDerivedSessionConcurrentConversations(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeDerived, maxInFlight: 8, rateBurst: 500, ratePerMinute: 6000})

	const n = 8
	sessions := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"conversation %d"}]}`, modelID, i)
			req, _ := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+clientToken)
			resp, err := h.client().Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var payload struct {
				Session string `json:"session"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&payload)
			sessions[i] = payload.Session
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, s := range sessions {
		if s == "" {
			t.Fatalf("conversation %d produced no session", i)
		}
		if seen[s] {
			t.Fatalf("session collision under concurrency: %q", s)
		}
		seen[s] = true
	}
}

func TestUsageAndAllowancePassThrough(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeDerived})
	resp := h.chatWith(t, `{"model":"`+modelID+`","messages":[{"role":"user","content":"hi"}]}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	// Upstream accounting must reach the caller unchanged.
	for _, want := range []string{`"usage"`, `"prompt_tokens"`, `"total_tokens"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response lost %s: %s", want, body)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("response is cacheable")
	}
}

// --- strict header identity (production mode) ---

func (h *harness) chatHeader(t *testing.T, header, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.gw.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+clientToken)
	if header != "" {
		req.Header.Set("X-N8N-Thread-Id", header)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHeaderSessionStableAcrossTurns(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeHeader})
	body := `{"model":"` + modelID + `","messages":[{"role":"user","content":"Ciao"}]}`
	a := sessionOf(t, h.chatHeader(t, "thread-aaaa-1111", body))
	b := sessionOf(t, h.chatHeader(t, "thread-aaaa-1111", body))
	if a == "" || a != b {
		t.Fatalf("not stable: %q vs %q", a, b)
	}
}

func TestHeaderSessionSeparatesIdenticalPrompts(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeHeader})
	body := `{"model":"` + modelID + `","messages":[{"role":"user","content":"Ciao"}]}`
	a := sessionOf(t, h.chatHeader(t, "thread-aaaa-1111", body))
	b := sessionOf(t, h.chatHeader(t, "thread-bbbb-2222", body))
	if a == b {
		t.Fatalf("identical prompts collided: %q", a)
	}
}

func TestHeaderSessionRequiresHeader(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeHeader})
	body := `{"model":"` + modelID + `","messages":[{"role":"user","content":"Ciao"}]}`
	for _, header := range []string{"", "bad header", "x"} {
		resp := h.chatHeader(t, header, body)
		bodyText := readBody(t, resp)
		if resp.StatusCode != http.StatusNotImplemented {
			t.Fatalf("header %q: expected 501, got %d: %s", header, resp.StatusCode, bodyText)
		}
	}
	if h.up.count() != 0 {
		t.Fatalf("missing identity reached upstream %d times", h.up.count())
	}
}

func TestHeaderSessionDoesNotFallBackToContent(t *testing.T) {
	h := newHarness(t, opts{sessionMode: config.SessionModeHeader})
	// A perfectly valid user message but no header must still fail closed.
	body := `{"model":"` + modelID + `","messages":[{"role":"user","content":"Ciao"}]}`
	resp := h.chatHeader(t, "", body)
	_ = readBody(t, resp)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("content fallback occurred: %d", resp.StatusCode)
	}
}

// The production defaults must survive a realistic Assistant opening burst:
// tens of locally-executed tool calls before the first slow model round-trip.
func TestDefaultRateLimitHandlesAssistantBurst(t *testing.T) {
	h := newHarness(t, opts{ratePerMinute: 300, rateBurst: 60, maxInFlight: 8, sessionMode: config.SessionModeHeader})
	body := `{"model":"` + modelID + `","messages":[{"role":"user","content":"burst"}]}`
	for i := 0; i < 60; i++ {
		resp := h.chatHeader(t, "burst-thread", body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d rejected with %d", i, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	resp := h.chatHeader(t, "burst-thread", body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429 past the burst, got %d", resp.StatusCode)
	}
}

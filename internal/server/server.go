// Package server implements the closed-routing gateway HTTP surface.
package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/veronica/ai-gateway/internal/config"
	"github.com/veronica/ai-gateway/internal/logging"
	"github.com/veronica/ai-gateway/internal/ratelimit"
	"github.com/veronica/ai-gateway/internal/session"
	"github.com/veronica/ai-gateway/internal/upstream"
)

// Error codes returned to the caller. These are stable strings, not free text.
const (
	codeUnauthorized     = "unauthorized"
	codeRateLimited      = "rate_limited"
	codeOverloaded       = "overloaded"
	codePayloadTooLarge  = "payload_too_large"
	codeInvalidJSON      = "invalid_json"
	codeModelNotAllowed  = "model_not_allowed"
	codeSessionMissing   = "session_identity_unavailable"
	codeUpstreamFailure  = "upstream_failure"
	codeUpstreamTimeout  = "upstream_timeout"
	codeUpstreamRedirect = "upstream_redirect"
	codeNotFound         = "not_found"
	codeMethodNotAllowed = "method_not_allowed"
	codeBadRequest       = "bad_request"
	codeInternal         = "internal_error"
)

// n8nResponsesUnknownURL is the shape n8n's @n8n/agents fallback probe expects:
// a 404 whose error.code is "unknown_url" (or a message matching
// /^Unknown request URL: .../responses/). Matching it deliberately lets n8n
// switch to Chat Completions without the gateway reinterpreting payloads.
const n8nResponsesUnknownURL = "Unknown request URL: POST /v1/responses"

// Server is the gateway handler.
type Server struct {
	cfg      config.Config
	log      *logging.Logger
	client   *upstream.Client
	limiter  *ratelimit.Bucket
	sem      chan struct{}
	resolver session.Resolver

	maxConcurrent atomic.Int64
	upstreamCalls atomic.Int64
	requests      atomic.Int64
}

// New constructs a Server.
func New(cfg config.Config, log *logging.Logger, client *upstream.Client, resolver session.Resolver) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if resolver == nil {
		return nil, errors.New("session resolver is required")
	}
	return &Server{
		cfg:      cfg,
		log:      log,
		client:   client,
		limiter:  ratelimit.New(cfg.RatePerMinute, cfg.RateBurst),
		sem:      make(chan struct{}, cfg.MaxInFlight),
		resolver: resolver,
	}, nil
}

// Stats is a test/introspection snapshot.
type Stats struct {
	Requests          int64
	MaxConcurrent     int64
	UpstreamCalls     int64
	SessionResolver   string
	SessionMode       string
	MaxInFlight       int
	RatePerMinute     int
	RateBurst         int
	MaxBodyBytes      int64
	StreamIdleTimeout time.Duration
}

func (s *Server) Stats() Stats {
	return Stats{
		Requests:          s.requests.Load(),
		MaxConcurrent:     s.maxConcurrent.Load(),
		UpstreamCalls:     s.upstreamCalls.Load(),
		SessionResolver:   s.resolver.Describe(),
		SessionMode:       string(s.cfg.SessionMode),
		MaxInFlight:       s.cfg.MaxInFlight,
		RatePerMinute:     s.cfg.RatePerMinute,
		RateBurst:         s.cfg.RateBurst,
		MaxBodyBytes:      s.cfg.MaxBodyBytes,
		StreamIdleTimeout: s.cfg.StreamIdleTimeout,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newRequestID()
	w.Header().Set("X-Request-Id", reqID)

	rec := logging.AccessRecord{
		RequestID: reqID,
		Event:     "access",
		Method:    r.Method,
	}

	// Absolute-form request lines and non-origin paths are rejected before any
	// routing. This removes the classic proxy SSRF shapes.
	if r.URL.IsAbs() || !strings.HasPrefix(r.URL.Path, "/") {
		rec.Route, rec.Status, rec.ErrorCode = "rejected", http.StatusBadRequest, codeBadRequest
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusBadRequest, codeBadRequest, "request target must be an origin-form path")
		return
	}

	switch {
	case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
		rec.Route, rec.Status = "healthz", http.StatusOK
		s.finish(w, rec, start, 0, 0)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return

	case r.URL.Path == "/readyz" && r.Method == http.MethodGet:
		rec.Route, rec.Status = "readyz", http.StatusOK
		s.finish(w, rec, start, 0, 0)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
		return

	case r.URL.Path == "/v1/responses":
		// Deliberate contract with n8n: advertise that this gateway speaks Chat
		// Completions only, so the documented fallback engages.
		rec.Route, rec.Status, rec.ErrorCode = "responses_probe", http.StatusNotFound, "unknown_url"
		s.finish(w, rec, start, 0, 0)
		writeUnknownURL(w)
		return

	case r.URL.Path == "/v1/chat/completions":
		if r.Method != http.MethodPost {
			rec.Route, rec.Status, rec.ErrorCode = "chat_completions", http.StatusMethodNotAllowed, codeMethodNotAllowed
			w.Header().Set("Allow", http.MethodPost)
			s.finish(w, rec, start, 0, 0)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "use POST")
			return
		}
		s.handleChat(w, r, rec, start)

	case r.URL.Path == "/v1/models":
		if r.Method != http.MethodGet {
			rec.Route, rec.Status, rec.ErrorCode = "models", http.StatusMethodNotAllowed, codeMethodNotAllowed
			w.Header().Set("Allow", http.MethodGet)
			s.finish(w, rec, start, 0, 0)
			writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed, "use GET")
			return
		}
		s.handleModels(w, r, rec, start)

	default:
		rec.Route, rec.Status, rec.ErrorCode = "not_found", http.StatusNotFound, codeNotFound
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusNotFound, codeNotFound, "no route")
	}
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, rec logging.AccessRecord, start time.Time) {
	rec.Route = "chat_completions"
	s.requests.Add(1)

	if !s.authorized(r) {
		rec.Status, rec.ErrorCode = http.StatusUnauthorized, codeUnauthorized
		w.Header().Set("WWW-Authenticate", `Bearer realm="veronica-ai-gateway"`)
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "invalid or missing bearer token")
		return
	}

	if ok, wait := s.limiter.Allow(); !ok {
		rec.Status, rec.ErrorCode = http.StatusTooManyRequests, codeRateLimited
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds()+0.999)))
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusTooManyRequests, codeRateLimited, "rate limit exceeded")
		return
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	default:
		rec.Status, rec.ErrorCode = http.StatusServiceUnavailable, codeOverloaded
		w.Header().Set("Retry-After", "1")
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusServiceUnavailable, codeOverloaded, "too many concurrent requests")
		return
	}

	cur := int64(len(s.sem))
	for {
		prev := s.maxConcurrent.Load()
		if cur <= prev || s.maxConcurrent.CompareAndSwap(prev, cur) {
			break
		}
	}

	body, err := readRequestBody(w, r, s.cfg.MaxBodyBytes)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			rec.Status, rec.ErrorCode, rec.BytesIn = http.StatusRequestEntityTooLarge, codePayloadTooLarge, mbe.Limit
			s.finish(w, rec, start, mbe.Limit, 0)
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "request body too large")
			return
		}
		rec.Status, rec.ErrorCode = http.StatusBadRequest, codeBadRequest
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusBadRequest, codeBadRequest, "could not read request body")
		return
	}
	rec.BytesIn = int64(len(body))

	var envelope struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		rec.Status, rec.ErrorCode = http.StatusBadRequest, codeInvalidJSON
		s.finish(w, rec, start, rec.BytesIn, 0)
		writeError(w, http.StatusBadRequest, codeInvalidJSON, "body must be a JSON object")
		return
	}
	rec.Model = envelope.Model

	if envelope.Model != s.cfg.Model {
		rec.Status, rec.ErrorCode = http.StatusBadRequest, codeModelNotAllowed
		s.finish(w, rec, start, rec.BytesIn, 0)
		writeError(w, http.StatusBadRequest, codeModelNotAllowed, "model is not allowed by this gateway")
		return
	}

	meta := session.RequestMeta{
		RequestID:          rec.RequestID,
		ClientID:           config.Fingerprint(s.cfg.ClientToken),
		Model:              envelope.Model,
		ConversationHeader: conversationHeader(r),
	}
	// Cheap only when the resolver needs it: one small decode, no body copy.
	if s.cfg.SessionMode == config.SessionModeDerived {
		if text, ok := session.FirstUserText(body); ok {
			meta.FirstUserText = text
		}
	}
	if hint := r.Header.Get("X-Gateway-Test-Conversation"); hint != "" && s.cfg.TestOnly && s.cfg.SessionMode == config.SessionModeMock {
		meta.ConversationHint = hint
	}

	sess, err := s.resolver.Resolve(r.Context(), meta)
	if err != nil {
		status := http.StatusNotImplemented
		code := codeSessionMissing
		if !errors.Is(err, session.ErrNoReliableConversationID) {
			status, code = http.StatusInternalServerError, codeInternal
		}
		rec.Status, rec.ErrorCode = status, code
		s.finish(w, rec, start, rec.BytesIn, 0)
		writeError(w, status, code, "conversation identity is not available; refusing to call the provider")
		return
	}
	rec.SessionPrefix = sess.Prefix(16)

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.TotalTimeout)
	defer cancel()

	s.upstreamCalls.Add(1)
	resp, err := s.client.ChatCompletion(ctx, body, sess)
	if err != nil {
		status, code := http.StatusBadGateway, codeUpstreamFailure
		switch {
		case errors.Is(err, upstream.ErrRedirect):
			code = codeUpstreamRedirect
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
			status, code = http.StatusGatewayTimeout, codeUpstreamTimeout
		}
		rec.Status, rec.ErrorCode = status, code
		s.finish(w, rec, start, rec.BytesIn, 0)
		writeError(w, status, code, "upstream request failed")
		return
	}
	defer resp.Body.Close()
	rec.UpstreamStatus = resp.StatusCode

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if clamped, ok := clampRetryAfter(ra, time.Now()); ok {
			w.Header().Set("Retry-After", clamped)
		}
	}
	w.Header().Set("Cache-Control", "no-store")

	w.WriteHeader(resp.StatusCode)
	if resp.StatusCode >= 400 {
		// Relay a bounded upstream error body; it is never written to logs.
		payload, _, _ := upstream.ReadLimited(resp.Body, 64<<10)
		n, _ := w.Write(payload)
		rec.Status = resp.StatusCode
		s.finish(w, rec, start, rec.BytesIn, int64(n))
		return
	}

	n, streamErr := s.copyStream(w, resp.Body)
	rec.Status = resp.StatusCode
	if streamErr != nil {
		rec.ErrorCode = codeUpstreamFailure
	}
	s.finish(w, rec, start, rec.BytesIn, n)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request, rec logging.AccessRecord, start time.Time) {
	rec.Route = "models"
	s.requests.Add(1)

	if !s.authorized(r) {
		rec.Status, rec.ErrorCode = http.StatusUnauthorized, codeUnauthorized
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "invalid or missing bearer token")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	resp, err := s.client.Models(ctx)
	if err != nil {
		rec.Status, rec.ErrorCode = http.StatusBadGateway, codeUpstreamFailure
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusBadGateway, codeUpstreamFailure, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	payload, oversize, err := upstream.ReadLimited(resp.Body, 4<<20)
	if err != nil {
		rec.Status, rec.ErrorCode = http.StatusBadGateway, codeUpstreamFailure
		if oversize {
			rec.Status, rec.ErrorCode = http.StatusBadGateway, codeOversizeName
		}
		s.finish(w, rec, start, 0, 0)
		writeError(w, http.StatusBadGateway, codeUpstreamFailure, "invalid upstream response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	n, _ := w.Write(payload)
	rec.Status = resp.StatusCode
	rec.UpstreamStatus = resp.StatusCode
	s.finish(w, rec, start, 0, int64(n))
}

const codeOversizeName = "upstream_response_too_large"

// copyStream relays bytes as they arrive. It never buffers the full response.
func (s *Server) copyStream(w http.ResponseWriter, body io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	var written int64

	for {
		n, err := body.Read(buf)
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.StreamIdleTimeout))
			m, werr := w.Write(buf[:n])
			written += int64(m)
			if werr != nil {
				return written, werr
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return written, nil
			}
			return written, err
		}
	}
}

// conversationHeader prefers the client-specific header, then the generic one.
func conversationHeader(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-N8N-Thread-Id")); v != "" {
		return v
	}
	return strings.TrimSpace(r.Header.Get("X-Conversation-Id"))
}

func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	want := s.cfg.ClientToken
	// Compare fixed-size digests in constant time: length differences must not
	// leak through timing.
	gh := sha256.Sum256([]byte(got))
	wh := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gh[:], wh[:]) == 1
}

func (s *Server) finish(w http.ResponseWriter, rec logging.AccessRecord, start time.Time, in, out int64) {
	if rec.BytesIn == 0 {
		rec.BytesIn = in
	}
	rec.BytesOut = out
	rec.DurMS = time.Since(start).Milliseconds()
	if rec.Status == 0 {
		rec.Status = http.StatusOK
	}
	s.log.Write(rec)
}

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-fallback"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// --- response helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "gateway_error",
			"code":    code,
		},
	})
}

// writeUnknownURL emits the exact shape n8n uses to detect "no Responses API".
func writeUnknownURL(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]string{
			"message": n8nResponsesUnknownURL,
			"type":    "invalid_request_error",
			"code":    "unknown_url",
			"param":   "",
		},
	})
}

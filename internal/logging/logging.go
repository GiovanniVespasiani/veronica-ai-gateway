// Package logging emits metadata-only access records.
//
// The record type below is the entire logging surface. There is no generic
// key/value passthrough, so a caller cannot accidentally log a prompt, a
// header, or a token. Any new field must be added here deliberately.
package logging

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// AccessRecord is the only thing this package can serialize.
type AccessRecord struct {
	TS             string `json:"ts"`
	Level          string `json:"level"`
	Event          string `json:"event"`
	RequestID      string `json:"request_id,omitempty"`
	Method         string `json:"method,omitempty"`
	Route          string `json:"route,omitempty"`
	Status         int    `json:"status,omitempty"`
	DurMS          int64  `json:"dur_ms,omitempty"`
	BytesIn        int64  `json:"bytes_in,omitempty"`
	BytesOut       int64  `json:"bytes_out,omitempty"`
	Model          string `json:"model,omitempty"`
	SessionPrefix  string `json:"session_prefix,omitempty"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	ErrorCode      string `json:"error_code,omitempty"`
	Message        string `json:"message,omitempty"`
	Fields         int    `json:"fields,omitempty"`
}

// Logger writes newline-delimited JSON to a single sink.
type Logger struct {
	mu  sync.Mutex
	out io.Writer
	now func() time.Time
}

func New(out io.Writer) *Logger {
	return &Logger{out: out, now: time.Now}
}

// Write stamps and emits a record. It never inspects request bodies.
func (l *Logger) Write(rec AccessRecord) {
	rec.TS = l.now().UTC().Format(time.RFC3339Nano)
	if rec.Level == "" {
		rec.Level = "info"
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(b, '\n'))
}

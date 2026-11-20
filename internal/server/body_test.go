package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClampRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", false},
		{"0", "1", true},
		{"-5", "1", true},
		{"7", "7", true},
		{"600", "600", true},
		{"601", "600", true},
		{"999999", "600", true},
		{"garbage", "", false},
		{now.Add(30 * time.Second).Format(http.TimeFormat), "30", true},
		{now.Add(48 * time.Hour).Format(http.TimeFormat), "600", true},
		{now.Add(-48 * time.Hour).Format(http.TimeFormat), "1", true},
	}
	for _, c := range cases {
		got, ok := clampRetryAfter(c.in, now)
		if ok != c.ok || got != c.want {
			t.Fatalf("clampRetryAfter(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func mkRequest(body string, contentLength int64) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.ContentLength = contentLength
	return r
}

// readAllBaseline mirrors the Phase A implementation so the allocation
// difference is measured rather than asserted.
func readAllBaseline(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, max))
}

func benchmarkBody(b *testing.B, size int, preallocated bool) {
	unit := strings.Repeat("a", 1024)
	payload := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"` +
		strings.Repeat(unit, size/1024) + `"}]}`
	n := int64(len(payload))

	b.SetBytes(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		r := mkRequest(payload, n)
		var err error
		if preallocated {
			_, err = readRequestBody(rec, r, 32<<20)
		} else {
			_, err = readAllBaseline(rec, r, 32<<20)
		}
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBody1MiBPreallocated(b *testing.B)  { benchmarkBody(b, 1<<20, true) }
func BenchmarkBody1MiBBaseline(b *testing.B)      { benchmarkBody(b, 1<<20, false) }
func BenchmarkBody8MiBPreallocated(b *testing.B)  { benchmarkBody(b, 8<<20, true) }
func BenchmarkBody8MiBBaseline(b *testing.B)      { benchmarkBody(b, 8<<20, false) }
func BenchmarkBody16MiBPreallocated(b *testing.B) { benchmarkBody(b, 16<<20, true) }
func BenchmarkBody16MiBBaseline(b *testing.B)     { benchmarkBody(b, 16<<20, false) }

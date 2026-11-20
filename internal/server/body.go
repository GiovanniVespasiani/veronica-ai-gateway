package server

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryAfterSeconds is the ceiling applied to any Relayed Retry-After value.
// An upstream (or a compromised upstream) must not be able to park a client for
// an arbitrarily long time.
const maxRetryAfterSeconds = 600

// clampRetryAfter normalises a Retry-After header to a whole number of seconds
// in [1, maxRetryAfterSeconds]. It returns ok=false when the value is absent or
// unparseable, in which case the header must be dropped rather than relayed.
func clampRetryAfter(v string, now time.Time) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if n, err := strconv.Atoi(v); err == nil {
		if n < 1 {
			n = 1
		}
		if n > maxRetryAfterSeconds {
			n = maxRetryAfterSeconds
		}
		return strconv.Itoa(n), true
	}
	if t, err := http.ParseTime(v); err == nil {
		secs := int(t.Sub(now).Round(time.Second).Seconds())
		if secs < 1 {
			secs = 1
		}
		if secs > maxRetryAfterSeconds {
			secs = maxRetryAfterSeconds
		}
		return strconv.Itoa(secs), true
	}
	return "", false
}

// readRequestBody reads at most max bytes and returns the payload.
//
// When Content-Length is known and within the limit the buffer is allocated
// exactly once, which avoids the geometric growth (and transient double
// allocation) of io.ReadAll on multi-megabyte Assistant histories.
func readRequestBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, max)

	if r.ContentLength > 0 && r.ContentLength <= max {
		buf := make([]byte, 0, r.ContentLength+1)
		tmp := make([]byte, 32<<10)
		for {
			n, err := limited.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return buf, nil
				}
				return nil, err
			}
		}
	}
	return io.ReadAll(limited)
}

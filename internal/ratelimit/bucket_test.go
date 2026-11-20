package ratelimit

import (
	"testing"
	"time"
)

func TestBurstThenRefill(t *testing.T) {
	now := time.Unix(0, 0)
	b := newWithClock(60, 2, func() time.Time { return now })

	for i := 0; i < 2; i++ {
		if ok, _ := b.Allow(); !ok {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	ok, wait := b.Allow()
	if ok {
		t.Fatal("third request should be refused")
	}
	if wait <= 0 || wait > time.Minute {
		t.Fatalf("unexpected wait %v", wait)
	}

	// One token per second at 60/minute.
	now = now.Add(time.Second)
	if ok, _ := b.Allow(); !ok {
		t.Fatal("refill did not grant a token")
	}
}

func TestCapacityCeiling(t *testing.T) {
	now := time.Unix(0, 0)
	b := newWithClock(600, 2, func() time.Time { return now })

	now = now.Add(24 * time.Hour)
	if ok, _ := b.Allow(); !ok {
		t.Fatal("expected a token")
	}
	if ok, _ := b.Allow(); !ok {
		t.Fatal("expected a token")
	}
	if ok, _ := b.Allow(); ok {
		t.Fatal("bucket exceeded its burst capacity")
	}
}

package mailin

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	l := NewRateLimiter(6, time.Hour, 2, func() time.Time { return now })
	for i, want := range []bool{true, true, false} {
		if got := l.Allow("a"); got != want {
			t.Fatalf("burst of 2: call %d allowed=%v", i, got)
		}
	}
	if !l.Allow("b") {
		t.Fatal("keys are independent")
	}
	now = now.Add(10 * time.Minute) // one token per 10 minutes
	if !l.Allow("a") {
		t.Fatal("refill")
	}
	if l.Allow("a") {
		t.Fatal("only one token refilled")
	}

	var nilLim *RateLimiter
	off := NewRateLimiter(0, time.Hour, 0, nil)
	for i := 0; i < 100; i++ {
		if !nilLim.Allow("x") || !off.Allow("x") {
			t.Fatal("a disabled limiter must allow")
		}
	}
}

func TestRateLimiterPrune(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	l := NewRateLimiter(1, time.Hour, 1, func() time.Time { return now })
	for i := 0; i < limiterMaxKeys; i++ {
		l.Allow(fmt.Sprint("old", i))
	}
	now = now.Add(2 * limiterIdle)
	l.Allow("fresh")
	if len(l.m) != 1 {
		t.Fatalf("idle keys not pruned: %d", len(l.m))
	}
	// "fresh" plus limiterMaxKeys-1 busy keys fill the table; the next new
	// key finds nothing idle and starts it over.
	for i := 0; i < limiterMaxKeys-1; i++ {
		l.Allow(fmt.Sprint("busy", i))
	}
	if len(l.m) != limiterMaxKeys {
		t.Fatalf("table size %d", len(l.m))
	}
	l.Allow("overflow")
	if len(l.m) != 1 {
		t.Fatalf("table not reset when full of busy keys: %d", len(l.m))
	}
}

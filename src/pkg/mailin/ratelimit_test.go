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

func TestByteBudget(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	b := NewByteBudget(100, 24*time.Hour, func() time.Time { return now })
	if !b.Reserve("a", 60) || b.Reserve("a", 50) || !b.Reserve("b", 100) {
		t.Fatal("per-key budget")
	}
	b.Refund("a", 60)
	b.Refund("a", 1000) // never below zero
	b.Refund("unknown", 1)
	if !b.Reserve("a", 100) || b.Reserve("a", 1) {
		t.Fatal("refund")
	}
	now = now.Add(24 * time.Hour)
	if !b.Reserve("a", 100) {
		t.Fatal("the window did not roll over")
	}
	var off *ByteBudget
	if !off.Reserve("a", 1<<40) || !NewByteBudget(0, time.Hour, nil).Reserve("a", 1<<40) {
		t.Fatal("a disabled budget refused")
	}
	off.Refund("a", 1)
	NewByteBudget(0, time.Hour, nil).Refund("a", 1)
}

func TestByteBudgetPrune(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	b := NewByteBudget(10, time.Hour, func() time.Time { return now })
	for i := 0; i < limiterMaxKeys; i++ {
		b.Reserve(fmt.Sprint(i), 1)
	}
	now = now.Add(2 * time.Hour)
	b.Reserve("fresh", 1)
	if len(b.m) != 1 {
		t.Fatalf("expired keys kept: %d", len(b.m))
	}
	for i := 0; i < limiterMaxKeys-1; i++ {
		b.Reserve(fmt.Sprint("k", i), 1)
	}
	b.Reserve("overflow", 1)
	if len(b.m) != 1 {
		t.Fatalf("full table not reset: %d", len(b.m))
	}
}

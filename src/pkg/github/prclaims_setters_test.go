package github

// Tests for the ClaimLedger test-seam setters in prclaims.go that guard
// against mis-set values reopening #4929: SetWeakDeferWindow must ignore
// non-positive durations (never treat them as "defer nothing") and must be
// nil-receiver safe, like SetTTL and SetClock.

import (
	"testing"
	"time"
)

func TestSetWeakDeferWindow_Override(t *testing.T) {
	l := NewClaimLedger(t.TempDir()+"/ledger.json", nil)
	l.SetWeakDeferWindow(5 * time.Minute)
	if l.weakDefer != 5*time.Minute {
		t.Fatalf("weakDefer = %v, want 5m", l.weakDefer)
	}
}

func TestSetWeakDeferWindow_NonPositiveIgnored(t *testing.T) {
	l := NewClaimLedger(t.TempDir()+"/ledger.json", nil)
	orig := l.weakDefer
	l.SetWeakDeferWindow(0)
	l.SetWeakDeferWindow(-time.Minute)
	if l.weakDefer != orig {
		t.Fatalf("weakDefer = %v after non-positive sets, want unchanged %v", l.weakDefer, orig)
	}
}

func TestSetWeakDeferWindow_NilReceiverSafe(t *testing.T) {
	var l *ClaimLedger
	l.SetWeakDeferWindow(time.Minute) // must not panic
}

// TestSetWeakDeferWindow_AffectsExpiry pins that the override actually feeds
// weakDeferExpired: shrinking the window flips an anchored claim from held to
// released.
func TestSetWeakDeferWindow_AffectsExpiry(t *testing.T) {
	l := NewClaimLedger(t.TempDir()+"/ledger.json", nil)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return base.Add(10 * time.Minute) })

	claim := IssueClaim{
		Repo:            "org/repo",
		Issue:           1,
		FirstObservedAt: base,
		ObservedAt:      base,
	}

	l.SetWeakDeferWindow(time.Hour)
	if l.weakDeferExpired(claim) {
		t.Fatal("claim expired under 1h window after 10m — window not applied")
	}

	l.SetWeakDeferWindow(time.Minute)
	if !l.weakDeferExpired(claim) {
		t.Fatal("claim not expired under 1m window after 10m — override not applied")
	}
}

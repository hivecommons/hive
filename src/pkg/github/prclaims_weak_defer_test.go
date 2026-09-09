package github

// Tests pinning weakDeferExpired (prclaims.go), the predicate that decides
// when an issue held back by a WEAK claim — an external PR (#3768) or a
// non-closing "Refs #N" reference (#3980) — is released back for agent work
// (#4929). The branches pinned here are exactly the failure modes the
// function's doc comment promises against:
//
//   - an UNANCHORED claim (no FirstObservedAt, no ObservedAt) is NOT expired,
//     so the first eval cycle after an upgrade cannot re-offer everything the
//     deferral window exists to hold back;
//   - ObservedAt anchors the window when FirstObservedAt is missing;
//   - a nil ledger and a non-positive window both fail open (expired), so a
//     broken ledger can never freeze the pipeline behind a stranger's PR;
//   - expiry is inclusive: elapsed == window releases.

import (
	"testing"
	"time"
)

// deferTestLedger returns a ledger with a fixed clock and a 72h window,
// plus the base time the clock is anchored to.
func deferTestLedger(t *testing.T) (*ClaimLedger, time.Time) {
	t.Helper()
	l := NewClaimLedger(t.TempDir()+"/ledger.json", nil)
	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return base })
	l.SetWeakDeferWindow(72 * time.Hour)
	return l, base
}

func TestWeakDeferExpired_UnanchoredClaimNotExpired(t *testing.T) {
	l, _ := deferTestLedger(t)
	claim := IssueClaim{Repo: "org/repo", Issue: 1}
	if l.weakDeferExpired(claim) {
		t.Fatal("unanchored claim reported expired — first cycle after an upgrade would re-offer every deferred issue (#4929)")
	}
}

func TestWeakDeferExpired_ObservedAtAnchorsWhenFirstObservedMissing(t *testing.T) {
	l, base := deferTestLedger(t)

	// Inside the window: ObservedAt is the anchor, 1h elapsed of 72h.
	held := IssueClaim{Repo: "org/repo", Issue: 1, ObservedAt: base.Add(-time.Hour)}
	if l.weakDeferExpired(held) {
		t.Fatal("claim anchored by ObservedAt expired 1h into a 72h window")
	}

	// Past the window: same anchor, 73h elapsed.
	released := IssueClaim{Repo: "org/repo", Issue: 2, ObservedAt: base.Add(-73 * time.Hour)}
	if !l.weakDeferExpired(released) {
		t.Fatal("claim anchored by ObservedAt not expired 73h into a 72h window")
	}
}

func TestWeakDeferExpired_FirstObservedAtWinsOverObservedAt(t *testing.T) {
	l, base := deferTestLedger(t)

	// FirstObservedAt is past the window even though the claim was
	// re-confirmed (ObservedAt refreshed) minutes ago: the window is measured
	// from FIRST observation, so re-confirmation must not restart it.
	claim := IssueClaim{
		Repo:            "org/repo",
		Issue:           1,
		FirstObservedAt: base.Add(-100 * time.Hour),
		ObservedAt:      base.Add(-time.Minute),
	}
	if !l.weakDeferExpired(claim) {
		t.Fatal("re-confirmed claim not expired — refreshing ObservedAt restarted the deferral window")
	}
}

func TestWeakDeferExpired_ExactWindowBoundaryReleases(t *testing.T) {
	l, base := deferTestLedger(t)
	claim := IssueClaim{
		Repo:            "org/repo",
		Issue:           1,
		FirstObservedAt: base.Add(-72 * time.Hour),
	}
	if !l.weakDeferExpired(claim) {
		t.Fatal("claim exactly at the window boundary not expired — expiry must be inclusive (>=)")
	}
	justInside := IssueClaim{
		Repo:            "org/repo",
		Issue:           2,
		FirstObservedAt: base.Add(-72*time.Hour + time.Second),
	}
	if l.weakDeferExpired(justInside) {
		t.Fatal("claim 1s inside the window reported expired")
	}
}

func TestWeakDeferExpired_NilLedgerFailsOpen(t *testing.T) {
	var l *ClaimLedger
	if !l.weakDeferExpired(IssueClaim{Repo: "org/repo", Issue: 1}) {
		t.Fatal("nil ledger did not fail open — a broken ledger would freeze issues behind weak claims")
	}
}

func TestWeakDeferExpired_NonPositiveWindowFailsOpen(t *testing.T) {
	l, base := deferTestLedger(t)
	// SetWeakDeferWindow guards against non-positive values, so reach the
	// branch the way a bug would: by corrupting the field directly.
	l.mu.Lock()
	l.weakDefer = 0
	l.mu.Unlock()

	claim := IssueClaim{Repo: "org/repo", Issue: 1, FirstObservedAt: base}
	if !l.weakDeferExpired(claim) {
		t.Fatal("zero window did not fail open (expired)")
	}
}

package claims

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMarkListedHoldsWithoutClaiming(t *testing.T) {
	var claimed int
	l, now := newTestLedger(t, Hooks{OnClaimed: func(Claim, Outcome) { claimed++ }})
	li, ok := l.MarkListed("o/r", 5, "scanner")
	if !ok || li.Holder != "scanner" || !li.ExpiresAt.Equal(now.Add(DefaultListedTTL)) {
		t.Fatalf("MarkListed = %+v, %v", li, ok)
	}
	if claimed != 0 {
		t.Fatal("a kick listing fired OnClaimed (the 🔒 comment)")
	}
	if _, held := l.Lookup("o/r", 5); held {
		t.Fatal("a kick listing created a claim")
	}
	if _, ok := l.LastAttempt("o/r", 5); ok {
		t.Fatal("a kick listing counted as a claim attempt")
	}
	if keys := l.HeldKeys("relay#s"); !keys["o/r#5"] {
		t.Fatalf("listing not excluded for a relay contributor: %v", keys)
	}
	if keys := l.HeldKeys("scanner"); keys["o/r#5"] {
		t.Fatal("listing excluded for the agent it was listed to")
	}
	if _, ok := l.MarkListed("o/r", 5, "quality"); ok {
		t.Fatal("a second agent's kick displaced a live listing")
	}

	*now = now.Add(DefaultListedTTL)
	if _, ok := l.Listed("o/r", 5); ok {
		t.Fatal("listing still holds after DefaultListedTTL")
	}
	if keys := l.HeldKeys(""); keys["o/r#5"] {
		t.Fatal("lapsed listing still excluded")
	}
	if li, ok := l.MarkListed("o/r", 5, "quality"); !ok || li.Holder != "quality" {
		t.Fatalf("lapsed listing not replaced: %+v %v", li, ok)
	}
}

func TestMarkListedLeavesClaimedIssuesAlone(t *testing.T) {
	l, _ := newTestLedger(t, Hooks{})
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 5, Holder: "alice", Kind: KindHuman}); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.MarkListed("o/r", 5, "scanner"); ok {
		t.Fatal("listed an issue a person claims")
	}
	if _, ok := l.MarkListed("", 5, "scanner"); ok {
		t.Fatal("listed without a repo")
	}
	var nilLedger *Ledger
	if _, ok := nilLedger.MarkListed("o/r", 5, "scanner"); ok {
		t.Fatal("nil ledger listed")
	}
}

func TestClaimOnStartClaimsOnlyListedIssues(t *testing.T) {
	var outcomes []Outcome
	l, now := newTestLedger(t, Hooks{OnClaimed: func(_ Claim, o Outcome) { outcomes = append(outcomes, o) }})
	if _, started, _ := l.ClaimOnStart("o/r", 5, "scanner"); started {
		t.Fatal("start signal on an issue no kick listed recorded a claim")
	}
	l.MarkListed("o/r", 5, "scanner")
	if _, started, _ := l.ClaimOnStart("o/r", 5, "quality"); started {
		t.Fatal("another agent's start signal claimed a listing")
	}

	// The start signal can come after the listing's short hold lapsed, as
	// long as it is within the agent claim TTL of the kick.
	*now = now.Add(DefaultListedTTL + time.Minute)
	res, started, err := l.ClaimOnStart("o/r", 5, "scanner")
	if err != nil || !started || res.Outcome != OutcomeClaimed || res.Claim.Kind != KindAgent {
		t.Fatalf("first start signal: res=%+v started=%v err=%v", res, started, err)
	}
	if _, ok := l.Listed("o/r", 5); ok {
		t.Fatal("listing left behind once the claim was recorded")
	}
	a, ok := l.LastAttempt("o/r", 5)
	if !ok || a.Holder != "scanner" || !a.ClaimedAt.Equal(*now) {
		t.Fatalf("LastAttempt = %+v, %v", a, ok)
	}

	// A second signal renews: no second 🔒 comment, no second attempt.
	*now = now.Add(time.Minute)
	if res, started, _ := l.ClaimOnStart("o/r", 5, "scanner"); !started || res.Outcome != OutcomeRenewed {
		t.Fatalf("second start signal: %+v started=%v", res, started)
	}
	if len(outcomes) != 2 || outcomes[0] != OutcomeClaimed || outcomes[1] != OutcomeRenewed {
		t.Fatalf("OnClaimed outcomes = %v, want [claimed renewed]", outcomes)
	}
	if s, _ := l.Stall("o/r", 5, 1); len(s.Attempts) > 0 {
		t.Fatalf("live claim reported as a stall: %+v", s)
	}
}

func TestClaimOnStartAfterTheListingWindowRecordsNothing(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	l.MarkListed("o/r", 5, "scanner")
	*now = now.Add(DefaultAgentTTL)
	if _, started, _ := l.ClaimOnStart("o/r", 5, "scanner"); started {
		t.Fatal("start signal past the agent claim TTL of the kick recorded a claim")
	}
	// Pruned once the window has passed.
	l.Expire()
	if _, ok := l.MarkListed("o/r", 5, "quality"); !ok {
		t.Fatal("stale listing blocked a new one")
	}
}

func TestListedIssuesNeverStall(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	for range 5 {
		l.MarkListed("o/r", 5, "scanner")
		*now = now.Add(DefaultAgentTTL + time.Minute)
	}
	if s, ok := l.Stall("o/r", 5, 2); ok {
		t.Fatalf("issues only listed, never started, reported as a stall: %+v", s)
	}
}

func TestListingsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	l, err := New(path, DefaultPolicy(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	l.SetNow(func() time.Time { return now })
	l.MarkListed("o/r", 5, "scanner")

	reloaded, err := New(path, DefaultPolicy(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	reloaded.SetNow(func() time.Time { return now.Add(time.Minute) })
	if li, ok := reloaded.Listed("o/r", 5); !ok || li.Holder != "scanner" {
		t.Fatalf("listing lost across restart: %+v %v", li, ok)
	}
	if res, started, err := reloaded.ClaimOnStart("o/r", 5, "scanner"); err != nil || !started || res.Outcome != OutcomeClaimed {
		t.Fatalf("start after restart: %+v started=%v err=%v", res, started, err)
	}
}

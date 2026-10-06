package claims

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claimAndLapse records an agent claim at *now and moves the clock past its
// expiry, the way an agent kick's claim runs out with nothing done.
func claimAndLapse(t *testing.T, l *Ledger, now *time.Time, holder string) {
	t.Helper()
	if res, err := l.Claim(Request{Repo: "o/r", Issue: 5, Holder: holder, HolderID: holder, Kind: KindAgent}); err != nil || res.Outcome != OutcomeClaimed {
		t.Fatalf("claim by %s: outcome=%s err=%v", holder, res.Outcome, err)
	}
	*now = now.Add(DefaultAgentTTL + time.Minute)
}

func TestStallNeedsThresholdAgentClaimsOnAFreeIssue(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	first := *now
	claimAndLapse(t, l, now, "scanner")
	if _, ok := l.Stall("o/r", 5, 2); ok {
		t.Fatal("one claim reported as a stall at threshold 2")
	}
	second := *now
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 5, Holder: "scanner", HolderID: "scanner", Kind: KindAgent}); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.Stall("o/r", 5, 2); ok {
		t.Fatal("stall reported while the second claim is still live")
	}
	*now = now.Add(DefaultAgentTTL + time.Minute)

	s, ok := l.Stall("o/r", 5, 2)
	if !ok || s.Holder != "scanner" || len(s.Attempts) != 2 || !s.Since().Equal(first) || !s.Attempts[1].ClaimedAt.Equal(second) {
		t.Fatalf("Stall=%+v ok=%v, want scanner run since %v", s, ok, first)
	}
	for _, a := range s.Attempts {
		if a.Ended != "expired" || !a.EndedAt.Equal(a.ClaimedAt.Add(DefaultAgentTTL)) {
			t.Fatalf("attempt end not recorded: %+v", a)
		}
	}
	if _, ok := l.Stall("o/r", 5, 3); ok {
		t.Fatal("two claims reported as a stall at threshold 3")
	}
	if _, ok := l.Stall("o/r", 5, 0); ok {
		t.Fatal("threshold 0 must disable the check")
	}
}

func TestStallIgnoresHumanClaimsAndCountsPerAgent(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 5, Holder: "alice", Kind: KindHuman}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(DefaultHumanTTL + time.Minute)
	claimAndLapse(t, l, now, "ci-maintainer")
	claimAndLapse(t, l, now, "scanner")
	if s, ok := l.Stall("o/r", 5, 2); ok {
		t.Fatalf("claims by three different holders reported as a stall: %+v", s)
	}
	claimAndLapse(t, l, now, "ci-maintainer")
	s, ok := l.Stall("o/r", 5, 2)
	if !ok || s.Holder != "ci-maintainer" {
		t.Fatalf("Stall=%+v ok=%v, want ci-maintainer", s, ok)
	}
}

func TestStallRecordsTakeoverAsTheAttemptEnd(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	if _, err := l.Claim(Request{Repo: "o/r", Issue: 5, Holder: "scanner", Kind: KindAgent}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if res, _ := l.Claim(Request{Repo: "o/r", Issue: 5, Holder: "alice", Kind: KindHuman}); res.Outcome != OutcomeTakenOver {
		t.Fatalf("outcome=%s want taken_over", res.Outcome)
	}
	if _, _, err := l.Release("o/r", 5, "alice", KindHuman, "released by alice"); err != nil {
		t.Fatal(err)
	}
	claimAndLapse(t, l, now, "scanner")
	s, ok := l.Stall("o/r", 5, 2)
	if !ok || s.Attempts[0].Ended != "taken over by alice" {
		t.Fatalf("Stall=%+v ok=%v, want first attempt ended by takeover", s, ok)
	}
}

func TestMarkEscalatedOncePerRunAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	l, err := New(path, DefaultPolicy(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	l.SetNow(func() time.Time { return now })
	claimAndLapse(t, l, &now, "scanner")
	claimAndLapse(t, l, &now, "scanner")
	s, ok := l.Stall("o/r", 5, 2)
	if !ok {
		t.Fatal("no stall after two lapsed claims")
	}
	if !l.MarkEscalated(s) {
		t.Fatal("first MarkEscalated = false")
	}
	if l.MarkEscalated(s) {
		t.Fatal("same run escalated twice")
	}

	reloaded, err := New(path, DefaultPolicy(), Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	reloaded.SetNow(func() time.Time { return now })
	rs, ok := reloaded.Stall("o/r", 5, 2)
	if !ok || !rs.Since().Equal(s.Since()) || rs.Attempts[1].Ended != "expired" {
		t.Fatalf("reloaded Stall=%+v ok=%v, want %+v", rs, ok, s)
	}
	if reloaded.MarkEscalated(rs) {
		t.Fatal("escalation forgotten across a restart: the same run would be escalated again")
	}

	// A later run (it begins after the escalated one) escalates again.
	claimAndLapse(t, reloaded, &now, "scanner")
	next, ok := reloaded.Stall("o/r", 5, 2)
	if !ok || !next.Since().After(s.Since()) {
		t.Fatalf("next Stall=%+v ok=%v", next, ok)
	}
	if !reloaded.MarkEscalated(next) {
		t.Fatal("a new run was suppressed as already escalated")
	}
}

func TestStallForgetsAttemptsPastRetention(t *testing.T) {
	l, now := newTestLedger(t, Hooks{})
	claimAndLapse(t, l, now, "scanner")
	*now = now.Add(AttemptRetention)
	claimAndLapse(t, l, now, "scanner")
	if s, ok := l.Stall("o/r", 5, 2); ok {
		t.Fatalf("claims more than AttemptRetention apart reported as one run: %+v", s)
	}
}

func TestEscalationComment(t *testing.T) {
	at := time.Date(2026, 10, 4, 15, 50, 0, 0, time.UTC)
	s := Stall{Repo: "o/r", Issue: 5, Holder: "scanner", Attempts: []Attempt{
		{Holder: "scanner", ClaimedAt: at, EndedAt: at.Add(2 * time.Hour), Ended: "expired"},
		{Holder: "scanner", ClaimedAt: at.Add(127 * time.Minute), EndedAt: at.Add(247 * time.Minute), Ended: "a | b"},
	}}
	got := EscalationComment(s, "needs-human")
	for _, want := range []string{
		"<!-- hive:claim-escalation who=scanner kind=agent attempts=2 since=2026-10-04T15:50:00Z -->",
		"agent `scanner` claimed it 2 times",
		"| 2026-10-04 15:50 UTC | expired at 2026-10-04 17:50 UTC |",
		`| 2026-10-04 17:57 UTC | a \| b at 2026-10-04 19:57 UTC |`,
		"Labelled `needs-human`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("EscalationComment missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<!-- hive:claim who=") {
		t.Fatalf("escalation comment carries a claim marker:\n%s", got)
	}
}

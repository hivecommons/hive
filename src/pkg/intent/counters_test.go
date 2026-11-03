package intent

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWouldBlockReasonCode(t *testing.T) {
	cases := []struct {
		name string
		v    Verdict
		want string
	}{
		{"human PR", Verdict{Tier: Tier3, Authorized: false, Reason: ReasonTier3NeedsApproval, AgentPR: false}, ""},
		{"tier0 authorized", Evaluate(Classification{Tier: Tier0, AgentPR: true}, Evidence{}), ""},
		{"tier1 needs issue", Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{}), ReasonCodeTier1NeedsIssue},
		{"tier1 with issue", Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{LinkedIssue: true}), ""},
		{"tier2 needs plan", Evaluate(Classification{Tier: Tier2, AgentPR: true}, Evidence{}), ReasonCodeTier2NeedsPlan},
		{"tier3 needs approval", Evaluate(Classification{Tier: Tier3, AgentPR: true}, Evidence{}), ReasonCodeTier3NeedsApproval},
		{"evidence unavailable", Verdict{Tier: Tier1, Authorized: false, Reason: "intent evidence unavailable: boom", AgentPR: true}, ReasonCodeEvidenceUnavailable},
		{"unknown reason", Verdict{Tier: Tier1, Authorized: false, Reason: "something else", AgentPR: true}, ReasonCodeUnknown},
		{"misaligned", Verdict{Tier: Tier1, Authorized: true, Reason: ReasonLinkedIssue, AgentPR: true,
			Alignment: &AlignmentVerdict{Status: AlignmentStatusMisaligned, Rationale: "drift"}}, ReasonCodeAlignmentMisaligned},
	}
	for _, tc := range cases {
		if got := WouldBlockReasonCode(tc.v); got != tc.want {
			t.Errorf("%s: WouldBlockReasonCode = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCountersRecordAndSnapshot(t *testing.T) {
	var c Counters
	var seen []Observation
	c.SetAuditSink(func(o Observation) { seen = append(seen, o) })

	c.Record("merge-eligible", "org/repo", 1, Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{}), false)
	c.Record("merge-eligible", "org/repo", 2, Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{LinkedIssue: true}), false)
	c.Record("self-merge", "org/repo", 3, Evaluate(Classification{Tier: Tier3, AgentPR: true}, Evidence{}), true)
	c.Record("merge-eligible", "org/repo", 4, Evaluate(Classification{Tier: Tier0, AgentPR: false}, Evidence{}), false)

	got := c.Snapshot()
	if got["tier1"].Attempted != 2 || got["tier1"].Blocked != 1 || got["tier1"].BlockedByReason[ReasonCodeTier1NeedsIssue] != 1 {
		t.Errorf("tier1 = %+v", got["tier1"])
	}
	if got["tier3"].Attempted != 1 || got["tier3"].Blocked != 1 || got["tier3"].BlockedByReason[ReasonCodeTier3NeedsApproval] != 1 {
		t.Errorf("tier3 = %+v", got["tier3"])
	}
	if got["tier0"].Attempted != 1 || got["tier0"].Blocked != 0 {
		t.Errorf("tier0 = %+v", got["tier0"])
	}
	if len(seen) != 4 {
		t.Fatalf("audit observations = %d, want 4", len(seen))
	}
	if o := seen[0]; !o.WouldBlock || o.Tier != "tier1" || o.ReasonCode != ReasonCodeTier1NeedsIssue || o.Lane != "merge-eligible" || o.Number != 1 {
		t.Errorf("obs[0] = %+v", o)
	}
	if o := seen[2]; !o.Enforced || o.Lane != "self-merge" {
		t.Errorf("obs[2] = %+v", o)
	}
	// Snapshot is a copy.
	got["tier1"].BlockedByReason["x"] = 9
	if c.Snapshot()["tier1"].BlockedByReason["x"] != 0 {
		t.Error("snapshot aliased internal map")
	}
}

func TestCounterWeekStart(t *testing.T) {
	cases := map[string]string{
		"2026-10-12T00:00:00Z": "2026-10-12",
		"2026-10-15T20:00:00Z": "2026-10-12",
		"2026-10-18T23:59:59Z": "2026-10-12",
		"2026-10-19T00:00:00Z": "2026-10-19",
		"2026-01-01T10:00:00Z": "2025-12-29",
	}
	for in, want := range cases {
		ts, _ := time.Parse(time.RFC3339, in)
		if got := CounterWeekStart(ts); got != want {
			t.Errorf("CounterWeekStart(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestCountersFlushMergesAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "intent-counters.json")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	blockedT1 := Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{})
	passedT1 := Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{LinkedIssue: true})
	blockedT3 := Evaluate(Classification{Tier: Tier3, AgentPR: true}, Evidence{})

	var first Counters
	first.now = func() time.Time { return now }
	first.Record("merge-eligible", "o/r", 1, passedT1, false)
	first.Record("merge-eligible", "o/r", 2, blockedT1, false)
	if err := first.Flush(path, now); err != nil {
		t.Fatal(err)
	}

	var second Counters
	second.now = func() time.Time { return now.AddDate(0, 0, 7) }
	second.Record("merge-eligible", "o/r", 3, blockedT1, false)
	second.Record("self-merge", "o/r", 4, blockedT3, false)
	if err := second.Flush(path, now); err != nil {
		t.Fatal(err)
	}

	got, err := LoadDurableCounts(path)
	if err != nil {
		t.Fatal(err)
	}
	t1 := got.Totals["tier1"]
	if t1.Attempted != 3 || t1.Blocked != 2 || t1.BlockedByReason[ReasonCodeTier1NeedsIssue] != 2 {
		t.Errorf("totals tier1 = %+v", t1)
	}
	w1 := got.Weeks["2026-10-12"]
	if w1.Attempted != 2 || w1.Blocked != 1 || w1.Tiers["tier1"].Blocked != 1 {
		t.Errorf("week 1 = %+v", w1)
	}
	w2 := got.Weeks["2026-10-19"]
	if w2.Attempted != 2 || w2.Blocked != 2 || w2.BlockedByReason[ReasonCodeTier3NeedsApproval] != 1 || w2.Tiers["tier3"].Attempted != 1 {
		t.Errorf("week 2 = %+v", w2)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not set")
	}
	// A flush with nothing pending leaves the file alone.
	before, _ := os.ReadFile(path)
	if err := second.Flush(path, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("empty flush rewrote the file")
	}
}

func TestCountersFlushKeepsPendingOnFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// MkdirAll fails because a regular file sits where the directory must go.
	path := filepath.Join(blocker, "intent-counters.json")
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var c Counters
	c.now = func() time.Time { return now }
	c.Record("merge-eligible", "o/r", 1, Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{}), false)
	if err := c.Flush(path, now); err == nil {
		t.Fatal("expected flush error")
	}
	good := filepath.Join(dir, "ok", "intent-counters.json")
	if err := c.Flush(good, now); err != nil {
		t.Fatal(err)
	}
	got, err := LoadDurableCounts(good)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals["tier1"].Attempted != 1 || got.Totals["tier1"].Blocked != 1 {
		t.Errorf("pending counts lost after failed flush: %+v", got.Totals)
	}
}

func TestLoadDurableCounts(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadDurableCounts(filepath.Join(dir, "missing.json"))
	if err != nil || len(got.Totals) != 0 || len(got.Weeks) != 0 {
		t.Errorf("missing file: %+v, %v", got, err)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDurableCounts(bad); err == nil {
		t.Error("corrupt file: expected error")
	}
	// A corrupt file is replaced, not fatal, on the next flush.
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var c Counters
	c.now = func() time.Time { return now }
	c.Record("merge-eligible", "o/r", 1, Evaluate(Classification{Tier: Tier0, AgentPR: true}, Evidence{}), false)
	if err := c.Flush(bad, now); err != nil {
		t.Fatal(err)
	}
	got, err = LoadDurableCounts(bad)
	if err != nil || got.Totals["tier0"].Attempted != 1 {
		t.Errorf("after corrupt flush: %+v, %v", got, err)
	}
}

func TestCountersPersistenceStopFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intent-counters.json")
	var c Counters
	stop := c.StartPersistence(path, nil)
	c.Record("merge-eligible", "o/r", 1, Evaluate(Classification{Tier: Tier2, AgentPR: true}, Evidence{}), false)
	stop()
	stop() // idempotent
	got, err := LoadDurableCounts(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals["tier2"].Attempted != 1 || got.Totals["tier2"].BlockedByReason[ReasonCodeTier2NeedsPlan] != 1 {
		t.Errorf("final flush missing: %+v", got.Totals)
	}
}

func TestRecordEvaluationDefaultCounters(t *testing.T) {
	defaultCounters = Counters{}
	t.Cleanup(func() { defaultCounters = Counters{} })
	var n int
	SetEvaluationAuditSink(func(Observation) { n++ })
	RecordEvaluation("merge-eligible", "o/r", 1, Evaluate(Classification{Tier: Tier1, AgentPR: true}, Evidence{}), false)
	if EvaluationCounts()["tier1"].Blocked != 1 || n != 1 {
		t.Errorf("default counters = %+v, audit=%d", EvaluationCounts(), n)
	}
	path := filepath.Join(t.TempDir(), "c.json")
	if err := FlushEvaluationCounters(path); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadDurableCounts(path); got.Totals["tier1"].Attempted != 1 {
		t.Errorf("flushed = %+v", got.Totals)
	}
}

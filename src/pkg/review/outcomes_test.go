package review

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOutcomeLedger_ObserveResolveSummary(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l := &OutcomeLedger{Items: map[string]*PROutcome{}}

	open := []OpenPR{
		{Repo: "acme/a", Number: 1, Author: "bot", AgentAuthored: true, CreatedAt: t0.Add(-2 * time.Hour)},
		{Repo: "acme/a", Number: 2, Author: "alice", CreatedAt: t0.Add(-time.Hour)},
		{Repo: "acme/b", Number: 7, Author: "bob", CreatedAt: t0.Add(-3 * time.Hour)},
	}
	if missing := l.Observe(t0, open, nil); len(missing) != 0 {
		t.Fatalf("first observation must report nothing missing, got %v", missing)
	}
	if len(l.Items) != 3 {
		t.Fatalf("want 3 rows, got %d", len(l.Items))
	}

	// Cycle 2: #1 reviewed (approve), #7 reviewed (changes_requested); #2 not.
	t1 := t0.Add(30 * time.Minute)
	reviews := map[string]ReviewSignal{
		"acme/a#1": {At: t1, Verdict: VerdictApprove},
		"acme/b#7": {At: t1, Verdict: VerdictChangesRequested},
	}
	l.Observe(t1, open, reviews)
	if got := l.Items["acme/a#1"]; !got.Reviewed() || got.FirstVerdict != VerdictApprove || !got.FirstReviewAt.Equal(t1) {
		t.Fatalf("review signal not attached: %+v", got)
	}
	if l.Items["acme/a#2"].Reviewed() {
		t.Fatal("#2 must be unreviewed")
	}

	// An earlier signal (a posted review the artifact missed) moves first-review back.
	l.Observe(t1, open, map[string]ReviewSignal{"acme/a#1": {At: t0.Add(10 * time.Minute)}})
	if got := l.Items["acme/a#1"]; !got.FirstReviewAt.Equal(t0.Add(10*time.Minute)) || got.FirstVerdict != VerdictApprove {
		t.Fatalf("earlier signal must win on time and keep the verdict: %+v", got)
	}

	// Cycle 3: #1 and #2 vanish from the open list.
	t2 := t1.Add(2 * time.Hour)
	missing := l.Observe(t2, open[2:], nil)
	if len(missing) != 2 || missing[0].Key() != "acme/a#1" || missing[1].Key() != "acme/a#2" {
		t.Fatalf("want #1 and #2 missing, got %v", missing)
	}
	// Until resolved they stay open — a flaky enumeration is not a merge.
	if l.Items["acme/a#1"].Outcome != OutcomeOpen {
		t.Fatal("unresolved PR must remain open")
	}
	l.Resolve("acme/a", 1, "closed", t2, time.Time{})
	l.Resolve("acme/a", 2, "closed", time.Time{}, t2)
	l.Resolve("acme/b", 7, "open", time.Time{}, time.Time{}) // GitHub says still open
	if l.Items["acme/a#1"].Outcome != OutcomeMerged || l.Items["acme/a#2"].Outcome != OutcomeClosed || l.Items["acme/b#7"].Outcome != OutcomeOpen {
		t.Fatalf("resolution wrong: %+v %+v %+v", l.Items["acme/a#1"], l.Items["acme/a#2"], l.Items["acme/b#7"])
	}

	s := l.Summary(t2, 7*24*time.Hour)
	if s.Reviewed.PRs != 2 || s.Reviewed.Merged != 1 || s.Reviewed.Open != 1 {
		t.Fatalf("reviewed group: %+v", s.Reviewed)
	}
	if s.Unreviewed.PRs != 1 || s.Unreviewed.Closed != 1 {
		t.Fatalf("unreviewed group: %+v", s.Unreviewed)
	}
	// #1: first seen t0, merged t2 = 2.5h; first review t0+10m → 2h20m ≈ 2.3h.
	if s.Reviewed.MedianHoursSeenToMerge != 2.5 || s.Reviewed.MedianHoursReviewToMerge != 2.3 || s.Reviewed.MergedWithin24h != 1 {
		t.Fatalf("reviewed latencies: %+v", s.Reviewed)
	}
	if s.AgentAuthored.PRs != 1 || s.HumanAuthored.PRs != 1 {
		t.Fatalf("author split: agent=%+v human=%+v", s.AgentAuthored, s.HumanAuthored)
	}
	if s.ByVerdict[VerdictApprove].Merged != 1 || s.ByVerdict[VerdictChangesRequested].Open != 1 {
		t.Fatalf("by verdict: %+v", s.ByVerdict)
	}
}

func TestOutcomeLedger_ReviewAfterOutcomeIsControl(t *testing.T) {
	// A review that lands after the PR merged (the reviewer got to it late)
	// must not count the PR as reviewed: the review cannot have helped.
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l := &OutcomeLedger{Items: map[string]*PROutcome{}}
	l.Observe(t0, []OpenPR{{Repo: "acme/a", Number: 1, CreatedAt: t0}}, nil)
	l.Observe(t0.Add(time.Hour), nil, nil)
	l.Resolve("acme/a", 1, "closed", t0.Add(time.Hour), time.Time{})
	l.Observe(t0.Add(2*time.Hour), nil, map[string]ReviewSignal{"acme/a#1": {At: t0.Add(90 * time.Minute), Verdict: VerdictApprove}})
	if l.Items["acme/a#1"].Reviewed() {
		t.Fatal("a review after the merge is not a reviewed outcome")
	}
	s := l.Summary(t0.Add(3*time.Hour), 24*time.Hour)
	if s.Unreviewed.Merged != 1 || s.Reviewed.PRs != 0 {
		t.Fatalf("late review must land in the control group: %+v / %+v", s.Reviewed, s.Unreviewed)
	}
}

func TestOutcomeLedger_SummaryWindowIsFirstSeen(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l := &OutcomeLedger{Items: map[string]*PROutcome{
		"acme/a#1": {Repo: "acme/a", Number: 1, FirstSeenAt: now.Add(-40 * 24 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-time.Hour)},
		"acme/a#2": {Repo: "acme/a", Number: 2, FirstSeenAt: now.Add(-2 * 24 * time.Hour), Outcome: OutcomeOpen},
	}}
	s := l.Summary(now, 30*24*time.Hour)
	if s.Unreviewed.PRs != 1 || s.Unreviewed.Open != 1 {
		t.Fatalf("PR first seen 40d ago must be outside a 30d window: %+v", s.Unreviewed)
	}
	if s.WindowDays != 30 {
		t.Fatalf("window days = %d", s.WindowDays)
	}
}

func TestOutcomeLedger_SnapshotAndRetention(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l := &OutcomeLedger{Items: map[string]*PROutcome{
		"acme/a#1": {Repo: "acme/a", Number: 1, FirstSeenAt: now, Outcome: OutcomeOpen, FirstReviewAt: now},
		"acme/a#2": {Repo: "acme/a", Number: 2, FirstSeenAt: now, Outcome: OutcomeOpen},
		"acme/a#3": {Repo: "acme/a", Number: 3, FirstSeenAt: now.Add(-100 * 24 * time.Hour), Outcome: OutcomeMerged, OutcomeAt: now.Add(-95 * 24 * time.Hour)},
	}}
	l.Snapshot(now)
	if _, ok := l.Items["acme/a#3"]; ok {
		t.Fatal("resolved row past retention must be aged out")
	}
	if len(l.Snapshots) != 1 || l.Snapshots[0].Open != 2 || l.Snapshots[0].OpenReviewed != 1 {
		t.Fatalf("snapshot: %+v", l.Snapshots)
	}
	l.Snapshot(now.Add(time.Hour))
	if len(l.Snapshots) != 1 {
		t.Fatal("a second sample within the interval must not be appended")
	}
	l.Snapshot(now.Add(24 * time.Hour))
	if len(l.Snapshots) != 2 {
		t.Fatal("a sample a day later must be appended")
	}
}

func TestOutcomeLedger_ReopenedPRIsOpenAgain(t *testing.T) {
	t0 := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l := &OutcomeLedger{Items: map[string]*PROutcome{}}
	pr := []OpenPR{{Repo: "acme/a", Number: 1, CreatedAt: t0}}
	l.Observe(t0, pr, nil)
	l.Observe(t0.Add(time.Hour), nil, nil)
	l.Resolve("acme/a", 1, "closed", time.Time{}, t0.Add(time.Hour))
	l.Observe(t0.Add(2*time.Hour), pr, nil)
	if o := l.Items["acme/a#1"]; o.Outcome != OutcomeOpen || !o.OutcomeAt.IsZero() {
		t.Fatalf("reopened PR must read open: %+v", o)
	}
}

func TestOutcomeLedger_LoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-outcomes.json")
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	l, err := LoadOutcomeLedger(path)
	if err != nil || len(l.Items) != 0 {
		t.Fatalf("missing file must load as empty: %v %d", err, len(l.Items))
	}
	l.Observe(now, []OpenPR{{Repo: "acme/a", Number: 1, CreatedAt: now}}, map[string]ReviewSignal{"acme/a#1": {At: now, Verdict: VerdictApprove}})
	l.Snapshot(now)
	if err := l.Save(path, now); err != nil {
		t.Fatal(err)
	}
	back, err := LoadOutcomeLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := back.Items["acme/a#1"]; got == nil || got.FirstVerdict != VerdictApprove || !got.FirstReviewAt.Equal(now) {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if len(back.Snapshots) != 1 {
		t.Fatalf("snapshots lost: %+v", back.Snapshots)
	}
}

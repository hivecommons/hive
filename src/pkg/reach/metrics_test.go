package reach

import (
	"reflect"
	"testing"
	"time"
)

var (
	mergedAt = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	// wellPastGrace is comfortably beyond the default 3-day never-ran
	// grace period after mergedAt.
	wellPastGrace = mergedAt.Add(10 * 24 * time.Hour)
)

// analyzer builds an Analyzer over a fake fleet and a frozen clock.
func analyzer(reports map[string][]ComponentReach, anc Ancestry, now time.Time) *Analyzer {
	return &Analyzer{
		Ancestry: anc,
		Reporter: &StubReachReporter{Reports: reports},
		Now:      func() time.Time { return now },
	}
}

// prHub is a PR touching only the hub component (v2/pkg/hub/**).
var prHub = PRInfo{
	Number:      3994,
	MergeCommit: "merge1",
	MergedAt:    mergedAt,
	Files:       []string{"v2/pkg/hub/saas.go", "docs/notes.md"},
}

// TestAnalyzeReach: a hive counts only when its RUNNING commit contains the
// merge commit AND a touched component executed (spans_total > 0) with
// first_seen after the merge — the full ancestry join of #3994.
func TestAnalyzeReach(t *testing.T) {
	anc := &fakeAncestry{pairs: map[string]map[string]bool{
		"merge1": {"deployed1": true}, // deployed1 contains the PR
	}}
	firstRun := mergedAt.Add(2 * time.Hour)
	reports := map[string][]ComponentReach{
		// Qualifies: descendant commit, touched component ran post-merge.
		"hive-good": {{Component: "hub", Commit: "deployed1", SpansTotal: 12, FirstSeen: firstRun, LastSeen: firstRun}},
		// Later first_seen: still reaches, but must not win the latency.
		"hive-later": {{Component: "hub", Commit: "deployed1", SpansTotal: 3, FirstSeen: mergedAt.Add(6 * time.Hour), LastSeen: mergedAt.Add(7 * time.Hour)}},
		// Running an OLD commit that does not contain the PR: excluded.
		"hive-stale": {{Component: "hub", Commit: "olddeploy", SpansTotal: 99, FirstSeen: firstRun, LastSeen: firstRun}},
		// Right commit, wrong component: excluded.
		"hive-othercomp": {{Component: "governor", Commit: "deployed1", SpansTotal: 50, FirstSeen: firstRun, LastSeen: firstRun}},
		// Right commit and component but zero executions: excluded.
		"hive-zero": {{Component: "hub", Commit: "deployed1", SpansTotal: 0, FirstSeen: firstRun, LastSeen: firstRun}},
		// first_seen BEFORE the merge (clock skew / stale counter file on a
		// pre-merge build): excluded.
		"hive-preexisting": {{Component: "hub", Commit: "deployed1", SpansTotal: 8, FirstSeen: mergedAt.Add(-time.Hour), LastSeen: firstRun}},
	}

	report, err := analyzer(reports, anc, mergedAt.Add(24*time.Hour)).Analyze(prHub)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if want := []string{"hive-good", "hive-later"}; !reflect.DeepEqual(report.ReachHives, want) {
		t.Errorf("ReachHives = %v, want %v", report.ReachHives, want)
	}
	if report.ReachCount != 2 {
		t.Errorf("ReachCount = %d, want 2", report.ReachCount)
	}
	if !report.Deployed {
		t.Error("Deployed = false, want true")
	}
	if report.FirstExecution == nil || !report.FirstExecution.Equal(firstRun) {
		t.Errorf("FirstExecution = %v, want %v", report.FirstExecution, firstRun)
	}
	if report.FirstExecutionLatencySeconds == nil || *report.FirstExecutionLatencySeconds != (2*time.Hour).Seconds() {
		t.Errorf("FirstExecutionLatencySeconds = %v, want %v", report.FirstExecutionLatencySeconds, (2 * time.Hour).Seconds())
	}
	if report.NeverRan {
		t.Error("NeverRan = true for a PR that ran")
	}
	// D3: attribution coverage rides along on every report.
	if want := 0.5; report.Attribution.Coverage != want {
		t.Errorf("Attribution.Coverage = %v, want %v", report.Attribution.Coverage, want)
	}
}

// TestAnalyzeNeverRan: merged + deployed + attributable + zero qualifying
// executions past the grace period → the flag acceptance-rate cannot see.
func TestAnalyzeNeverRan(t *testing.T) {
	anc := &fakeAncestry{pairs: map[string]map[string]bool{
		"merge1": {"deployed1": true},
	}}
	// The build containing the PR is running, but only OTHER components
	// executed on it — the PR's code is deployed and unused.
	reports := map[string][]ComponentReach{
		"hive-a": {{Component: "governor", Commit: "deployed1", SpansTotal: 100, FirstSeen: mergedAt.Add(time.Hour), LastSeen: wellPastGrace}},
	}

	report, err := analyzer(reports, anc, wellPastGrace).Analyze(prHub)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if !report.Deployed {
		t.Error("Deployed = false, want true")
	}
	if report.ReachCount != 0 {
		t.Errorf("ReachCount = %d, want 0", report.ReachCount)
	}
	if !report.NeverRan {
		t.Error("NeverRan = false, want true (deployed, attributable, unused, past grace)")
	}
	if report.NeverRanThresholdDays != DefaultNeverRanDays {
		t.Errorf("NeverRanThresholdDays = %d, want %d", report.NeverRanThresholdDays, DefaultNeverRanDays)
	}
}

// TestAnalyzeNeverRanSuppressed enumerates each condition that must HOLD
// the flag: still inside grace, not deployed anywhere, or nothing
// attributable to execute.
func TestAnalyzeNeverRanSuppressed(t *testing.T) {
	containing := &fakeAncestry{pairs: map[string]map[string]bool{
		"merge1": {"deployed1": true},
	}}
	deployedNoRun := map[string][]ComponentReach{
		"hive-a": {{Component: "governor", Commit: "deployed1", SpansTotal: 1, FirstSeen: mergedAt.Add(time.Hour), LastSeen: wellPastGrace}},
	}

	t.Run("inside grace period", func(t *testing.T) {
		report, err := analyzer(deployedNoRun, containing, mergedAt.Add(24*time.Hour)).Analyze(prHub)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		if report.NeverRan {
			t.Error("NeverRan raised inside the grace period")
		}
	})

	t.Run("merged but not deployed", func(t *testing.T) {
		nothingContains := &fakeAncestry{pairs: map[string]map[string]bool{}}
		report, err := analyzer(deployedNoRun, nothingContains, wellPastGrace).Analyze(prHub)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		if report.Deployed {
			t.Error("Deployed = true, want false")
		}
		if report.NeverRan {
			t.Error("NeverRan raised for an undeployed PR — that is 'not deployed', a different failure")
		}
	})

	t.Run("docs-only PR", func(t *testing.T) {
		docsPR := PRInfo{Number: 1, MergeCommit: "merge1", MergedAt: mergedAt, Files: []string{"README.md"}}
		report, err := analyzer(deployedNoRun, containing, wellPastGrace).Analyze(docsPR)
		if err != nil {
			t.Fatalf("Analyze: %v", err)
		}
		if report.NeverRan {
			t.Error("NeverRan raised for a PR with no attributable components")
		}
	})
}

// TestNeverRanThresholdEnv: the grace period is env-tunable with a safe
// fallback on garbage.
func TestNeverRanThresholdEnv(t *testing.T) {
	t.Setenv(NeverRanDaysEnvVar, "7")
	if got, want := NeverRanThreshold(), 7*24*time.Hour; got != want {
		t.Errorf("NeverRanThreshold = %v, want %v", got, want)
	}
	t.Setenv(NeverRanDaysEnvVar, "not-a-number")
	if got, want := NeverRanThreshold(), DefaultNeverRanDays*24*time.Hour; got != want {
		t.Errorf("NeverRanThreshold (invalid env) = %v, want %v", got, want)
	}
	t.Setenv(NeverRanDaysEnvVar, "-2")
	if got, want := NeverRanThreshold(), DefaultNeverRanDays*24*time.Hour; got != want {
		t.Errorf("NeverRanThreshold (negative env) = %v, want %v", got, want)
	}
}

// TestAnalyzeEmptyFleet: the pre-2a state — StubReachReporter with no data
// — yields zero reach and no never-ran flag (nothing is known deployed).
func TestAnalyzeEmptyFleet(t *testing.T) {
	report, err := analyzer(nil, &fakeAncestry{}, wellPastGrace).Analyze(prHub)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if report.ReachCount != 0 || report.Deployed || report.NeverRan {
		t.Errorf("empty fleet: got %+v, want zero reach, not deployed, no flag", report)
	}
}

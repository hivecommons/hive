package workflowmodel

import (
	"strings"
	"testing"
)

// history: c0 is where the model was enabled; PRs merge at c1..c5.
var attrHistory = []string{"c0", "c1", "c2", "c3", "c4", "c5"}

func attrPRs() []MergedPR {
	return []MergedPR{{1, "c1"}, {2, "c2"}, {3, "c3"}, {4, "c4"}, {5, "c5"}}
}

func wantStatus(t *testing.T, got map[int]map[string]Attribution, column string, want map[int]AttributionStatus) {
	t.Helper()
	for pr, status := range want {
		a := got[pr][column]
		if a.Status != status {
			t.Errorf("PR #%d %s: status %q (run %q, reason %q), want %q", pr, column, a.Status, a.RunID, a.Reason, status)
		}
		if a.Status == AttributionUnknown && a.Reason == "" {
			t.Errorf("PR #%d %s: unknown without a recorded reason", pr, column)
		}
	}
}

func TestAttributeRunsBatchedRanges(t *testing.T) {
	got := AttributeRuns(AttributionInput{
		History:   attrHistory,
		EnabledAt: "c0",
		Columns: map[string][]PostMergeRun{
			"release": {
				{ID: "r1", Revision: "c2", Conclusion: RunSuccess},
				{ID: "r2", Revision: "c4", Conclusion: RunSuccess},
			},
		},
		PRs: attrPRs(),
	})
	wantStatus(t, got, "release", map[int]AttributionStatus{
		1: AttributionSuccess, 2: AttributionSuccess,
		3: AttributionSuccess, 4: AttributionSuccess,
		5: AttributionUnknown,
	})
	for pr, run := range map[int]string{1: "r1", 2: "r1", 3: "r2", 4: "r2"} {
		if id := got[pr]["release"].RunID; id != run {
			t.Errorf("PR #%d credited to %q, want %q (prev..R must not re-credit)", pr, id, run)
		}
	}
}

func TestAttributeRunsFailureThenSuccess(t *testing.T) {
	runs := []PostMergeRun{
		{ID: "d1", Revision: "c3", Conclusion: RunFailure},
	}
	in := AttributionInput{History: attrHistory, EnabledAt: "c0", PRs: attrPRs(),
		Columns: map[string][]PostMergeRun{"deploy/prod": runs}}

	got := AttributeRuns(in)
	wantStatus(t, got, "deploy/prod", map[int]AttributionStatus{
		1: AttributionFailed, 2: AttributionFailed, 3: AttributionFailed, 4: AttributionUnknown,
	})

	// A later success's range starts at the previous *successful* run, so
	// it covers the PRs the failure covered.
	in.Columns["deploy/prod"] = append(runs,
		PostMergeRun{ID: "d2", Revision: "c4", Conclusion: RunSuccess},
		PostMergeRun{ID: "d3", Revision: "c5", Conclusion: RunFailure},
	)
	got = AttributeRuns(in)
	wantStatus(t, got, "deploy/prod", map[int]AttributionStatus{
		1: AttributionSuccess, 2: AttributionSuccess, 3: AttributionSuccess,
		4: AttributionSuccess, 5: AttributionFailed,
	})
	if id := got[3]["deploy/prod"].RunID; id != "d2" {
		t.Errorf("PR #3 credited to %q, want d2", id)
	}
}

func TestAttributeRunsFirstRunOnlySinceEnabled(t *testing.T) {
	got := AttributeRuns(AttributionInput{
		History:   attrHistory,
		EnabledAt: "c2",
		Columns: map[string][]PostMergeRun{
			"release": {{ID: "r1", Revision: "c4", Conclusion: RunSuccess}},
		},
		PRs: attrPRs(),
	})
	wantStatus(t, got, "release", map[int]AttributionStatus{
		1: AttributionUnknown, 2: AttributionUnknown,
		3: AttributionSuccess, 4: AttributionSuccess,
	})
	if r := got[1]["release"].Reason; !strings.Contains(r, "before the workflow model was enabled") {
		t.Errorf("PR #1 reason %q", r)
	}

	// Without a known enablement revision the first range is not guessed.
	got = AttributeRuns(AttributionInput{
		History: attrHistory,
		Columns: map[string][]PostMergeRun{
			"release": {
				{ID: "r1", Revision: "c2", Conclusion: RunSuccess},
				{ID: "r2", Revision: "c4", Conclusion: RunSuccess},
			},
		},
		PRs: attrPRs(),
	})
	wantStatus(t, got, "release", map[int]AttributionStatus{
		1: AttributionUnknown, 2: AttributionUnknown,
		3: AttributionSuccess, 4: AttributionSuccess,
	})
	if r := got[1]["release"].Reason; !strings.Contains(r, "r1") {
		t.Errorf("PR #1 reason %q should name the undeterminable run", r)
	}
}

func TestAttributeRunsUndeterminableRevision(t *testing.T) {
	got := AttributeRuns(AttributionInput{
		History:   attrHistory,
		EnabledAt: "c0",
		Columns: map[string][]PostMergeRun{
			"deploy/staging": {
				{ID: "plr-a", Revision: "c1", Conclusion: RunSuccess},
				{ID: "plr-b", Revision: "", Conclusion: RunFailure},
				{ID: "plr-c", Revision: "deadbeef", Conclusion: RunSuccess},
			},
		},
		PRs: append(attrPRs(), MergedPR{6, "elsewhere"}),
	})
	wantStatus(t, got, "deploy/staging", map[int]AttributionStatus{
		1: AttributionSuccess,
		2: AttributionUnknown, 3: AttributionUnknown, 4: AttributionUnknown, 5: AttributionUnknown,
		6: AttributionUnknown,
	})
	if r := got[2]["deploy/staging"].Reason; !strings.Contains(r, "plr-c") || !strings.Contains(r, "deadbeef") {
		t.Errorf("PR #2 reason %q should name the last undeterminable run", r)
	}
	if r := got[6]["deploy/staging"].Reason; !strings.Contains(r, "not on the observed default-branch history") {
		t.Errorf("PR #6 reason %q", r)
	}
}

func TestAttributeRunsIgnoresUnfinishedAndKeepsColumnsApart(t *testing.T) {
	got := AttributeRuns(AttributionInput{
		History:   attrHistory,
		EnabledAt: "c0",
		Columns: map[string][]PostMergeRun{
			"main-ci": {{ID: "m1", Revision: "c5", Conclusion: RunSuccess}},
			"release": {{ID: "r1", Revision: "c5", Conclusion: "running"}},
		},
		PRs: attrPRs(),
	})
	wantStatus(t, got, "main-ci", map[int]AttributionStatus{1: AttributionSuccess, 5: AttributionSuccess})
	wantStatus(t, got, "release", map[int]AttributionStatus{1: AttributionUnknown, 5: AttributionUnknown})
}

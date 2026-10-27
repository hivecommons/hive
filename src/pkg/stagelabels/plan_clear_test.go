package stagelabels

import (
	"slices"
	"testing"
)

// Desired.Clear names third-party labels the write must take off alongside
// the stage (hivecommons/hive#14586: the reporter-trust `needs-triage` left
// on a claimed issue). It removes only what is present, matches
// case-insensitively, never adds, and leaves every other foreign label alone.
func TestPlanClearRemovesNamedLabelsOnly(t *testing.T) {
	want := Desired{Stage: Claimed, Clear: []string{"needs-triage"}}

	add, remove := Plan([]string{"stage/015-triage-needed", "needs-triage", "kind/bug", "hive/claimed-by-scanner"}, want)
	if !slices.Equal(add, []string{"stage/020-claimed"}) {
		t.Fatalf("add = %v", add)
	}
	if !slices.Contains(remove, "needs-triage") || !slices.Contains(remove, "stage/015-triage-needed") {
		t.Fatalf("remove = %v; want the stale stage and needs-triage in one write", remove)
	}
	for _, l := range remove {
		if l == "kind/bug" || l == "hive/claimed-by-scanner" {
			t.Fatalf("plan touched an unrelated label: remove = %v", remove)
		}
	}

	// Stage already right: the stale label alone is due.
	add, remove = Plan([]string{"stage/020-claimed", "Needs-Triage", "kind/bug"}, want)
	if len(add) != 0 || !slices.Equal(remove, []string{"Needs-Triage"}) {
		t.Fatalf("stage correct: add = %v remove = %v; want only the (case-insensitive) stale label removed", add, remove)
	}

	// Label absent: nothing to do.
	add, remove = Plan([]string{"stage/020-claimed", "kind/bug"}, want)
	if len(add) != 0 || len(remove) != 0 {
		t.Fatalf("absent: add = %v remove = %v; want a no-op", add, remove)
	}

	// Clear never adds, and an empty Clear leaves the label where it is.
	add, remove = Plan([]string{"stage/020-claimed", "needs-triage"}, Desired{Stage: Claimed})
	if len(add) != 0 || len(remove) != 0 {
		t.Fatalf("no Clear: add = %v remove = %v; want needs-triage untouched", add, remove)
	}
}

// SettlesTriage is true from the claim on (and for the PR-mirrored stages)
// and false before it, on the 045 park, and for None — those are the stages
// the transition table lets fall back to 015, so the label must survive.
func TestSettlesTriage(t *testing.T) {
	cases := map[Stage]bool{
		None:               false,
		Actionable:         false,
		TriageNeeded:       false,
		IssueNeedsDecision: false,
		Claimed:            true,
		PRInProgress:       true,
		CoveredByPR:        true,
		ReviewPending:      true,
		MergeQueue:         true,
	}
	for s, want := range cases {
		if got := s.SettlesTriage(); got != want {
			t.Errorf("%s.SettlesTriage() = %v, want %v", s, got, want)
		}
	}
}

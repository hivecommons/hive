package reach

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeAncestry answers IsAncestor from an explicit pair table — the
// in-memory double for the join tests.
type fakeAncestry struct {
	// pairs[ancestor][descendant] = true means ancestor is contained in
	// descendant. Self-ancestry is implied.
	pairs map[string]map[string]bool
	err   error
}

func (f *fakeAncestry) IsAncestor(ancestor, descendant string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if ancestor == descendant {
		return true, nil
	}
	return f.pairs[ancestor][descendant], nil
}

// TestAssignWindows pins D4: PRs whose merge commits land between the same
// two deployed commits share one window and are labeled shared_with; a 1:1
// deploy stays precise; an undeployed PR gets no window.
func TestAssignWindows(t *testing.T) {
	// History: prA(101), prB(102) merged, then deploy d1; prC(103) merged,
	// then deploy d2; prD(104) merged, never deployed.
	anc := &fakeAncestry{pairs: map[string]map[string]bool{
		"mA": {"d1": true, "d2": true},
		"mB": {"d1": true, "d2": true},
		"mC": {"d2": true},
		"mD": {},
	}}
	prs := []PRInfo{
		{Number: 101, MergeCommit: "mA"},
		{Number: 102, MergeCommit: "mB"},
		{Number: 103, MergeCommit: "mC"},
		{Number: 104, MergeCommit: "mD"},
	}

	got, err := AssignWindows([]string{"d1", "d2"}, prs, anc)
	if err != nil {
		t.Fatalf("AssignWindows: %v", err)
	}

	want := map[int]WindowAssignment{
		// A and B batch into the d1 deploy: co-attributed, labeled shared.
		101: {DeployedCommit: "d1", SharedWith: []int{102}},
		102: {DeployedCommit: "d1", SharedWith: []int{101}},
		// C deployed alone in d2: per-PR precision, nothing shared.
		103: {DeployedCommit: "d2"},
		// D merged but no deployed commit contains it.
		104: {},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AssignWindows = %+v, want %+v", got, want)
	}
}

// TestAssignWindowsAncestryErrorAborts: a failed ancestry answer must abort
// the whole assignment — partial windows would silently mis-attribute.
func TestAssignWindowsAncestryErrorAborts(t *testing.T) {
	anc := &fakeAncestry{err: errors.New("boom")}
	_, err := AssignWindows([]string{"d1"}, []PRInfo{{Number: 1, MergeCommit: "m1"}}, anc)
	if err == nil {
		t.Fatal("want error, got nil")
	}
}

// TestDeployedCommits: deploy history is derived from what hives REPORT
// RUNNING, ordered by first observation — never from tags (#3816).
func TestDeployedCommits(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	reports := map[string][]ComponentReach{
		"hive-a": {
			{Component: "hub", Commit: "d2", FirstSeen: base.Add(48 * time.Hour)},
			{Component: "governor", Commit: "d1", FirstSeen: base.Add(2 * time.Hour)},
		},
		"hive-b": {
			// Same d1 commit seen EARLIER on another hive: earliest wins.
			{Component: "hub", Commit: "d1", FirstSeen: base},
			// Empty commit (no ldflags stamp) must never become a window.
			{Component: "hub", Commit: "", FirstSeen: base},
		},
	}
	got := DeployedCommits(reports)
	if want := []string{"d1", "d2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DeployedCommits = %v, want %v", got, want)
	}
}

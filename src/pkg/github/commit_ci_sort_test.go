package github

import (
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// TestLatestCheckRunsByNameAndAppDeterministicOrder guards against
// hivecommons/hive#9475: latestCheckRunsByNameAndApp used to return its
// result straight from a Go map range, so callers building a CI failure
// excerpt from the first few entries (fetchFailureExcerpt) saw a different,
// arbitrary subset of failing checks on every call. The result must now be
// sorted by name (app ID as tie-break) so the ordering is stable.
func TestLatestCheckRunsByNameAndAppDeterministicOrder(t *testing.T) {
	names := []string{"zeta", "test (rest 1/5)", "alpha", "test (rest 2/5)", "build"}
	checks := make([]*gh.CheckRun, 0, len(names))
	for i, name := range names {
		id := int64(i + 1)
		checks = append(checks, &gh.CheckRun{ID: &id, Name: gh.Ptr(name)})
	}

	var firstOrder []string
	for i := 0; i < 20; i++ {
		out := latestCheckRunsByNameAndApp(checks)
		if len(out) != len(names) {
			t.Fatalf("run %d: got %d check runs, want %d", i, len(out), len(names))
		}
		gotOrder := make([]string, len(out))
		for j, cr := range out {
			gotOrder[j] = cr.GetName()
		}
		if i == 0 {
			firstOrder = gotOrder
			continue
		}
		for j := range gotOrder {
			if gotOrder[j] != firstOrder[j] {
				t.Fatalf("run %d: order changed: got %v, want %v", i, gotOrder, firstOrder)
			}
		}
	}

	want := []string{"alpha", "build", "test (rest 1/5)", "test (rest 2/5)", "zeta"}
	for j, name := range want {
		if firstOrder[j] != name {
			t.Errorf("order[%d] = %q, want %q (full order: %v)", j, firstOrder[j], name, firstOrder)
		}
	}
}

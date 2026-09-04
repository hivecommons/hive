package github

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Phase 1 of kubestellar/hive#5953 removed this package's production
// dependency on pkg/config. Two values had to be duplicated to do that. This
// file is the only place pkg/config is still referenced here, and it exists to
// make the duplication safe: if either definition moves, the build of this
// test fails and the drift is caught at CI time rather than in behaviour.
//
// A test-only import does not reintroduce the coupling the issue is about —
// production code in this package no longer pulls in the application config
// surface, so importers of the client no longer do either.

func TestAutoMergeQueuedLabelMatchesConfigDefault(t *testing.T) {
	if AutoMergeQueuedLabel != config.DefaultAutoMergeLabel {
		t.Fatalf("AutoMergeQueuedLabel = %q, config.DefaultAutoMergeLabel = %q — "+
			"these name the same merger-queue label and must not drift",
			AutoMergeQueuedLabel, config.DefaultAutoMergeLabel)
	}
}

func TestSelfMergeMinACMMLevelMatchesConfig(t *testing.T) {
	if selfMergeMinACMMLevel != config.SelfMergeMinACMMLevel {
		t.Fatalf("selfMergeMinACMMLevel = %d, config.SelfMergeMinACMMLevel = %d — "+
			"the sweep logs this as the authority for self-authored auto-merge; "+
			"a mismatch would misreport the gate an operator is being held to",
			selfMergeMinACMMLevel, config.SelfMergeMinACMMLevel)
	}
}

// config.IssueFilterConfig must keep satisfying IssueAdmitter, because every
// construction site passes it directly. If it stops, those sites break at the
// call rather than here, which is a much less obvious failure.
func TestIssueFilterConfigSatisfiesIssueAdmitter(t *testing.T) {
	var _ IssueAdmitter = config.IssueFilterConfig{}
}

// An unset filter must admit everything: that was the zero-value behaviour of
// the struct this interface replaced, and enumeration relies on it.
func TestUnsetIssueFilterAdmitsEverything(t *testing.T) {
	c := &Client{}
	if got := c.getIssueFilter(); got == nil {
		t.Fatal("getIssueFilter() returned nil; the enumeration path would panic")
	}
	if !c.getIssueFilter().Admits([]string{"anything"}) {
		t.Fatal("an unset filter rejected a label; unset must admit everything")
	}
	if !c.getIssueFilter().Admits(nil) {
		t.Fatal("an unset filter rejected an unlabelled issue")
	}
}

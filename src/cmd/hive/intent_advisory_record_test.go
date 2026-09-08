package main

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/intent"
)

func newAdvisoryTestStore(t *testing.T) *beads.Store {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("beads.NewStore: %v", err)
	}
	return store
}

func misalignedVerdict() intent.AlignmentVerdict {
	return intent.AlignmentVerdict{
		Rationale: "diff exceeds linked issue scope",
		DeterministicFindings: []intent.AlignmentFinding{
			{Code: "unrelated-files", Status: intent.AlignmentStatusMisaligned,
				Reason: "touches files outside the issue", Files: []string{"pkg/other/other.go"}},
		},
	}
}

// recordIntentAlignmentAdvisory must mint exactly ONE open advisory bead per
// misaligned PR, carrying the alignment evidence in its notes — and a second
// observation of the same PR must NOT mint a duplicate while the first is
// still open, or every eval tick would pile advisories onto the same drift.
func TestRecordIntentAlignmentAdvisoryCreatesAndDedupes(t *testing.T) {
	store := newAdvisoryTestStore(t)
	stores := map[string]*beads.Store{"intent": store}

	recordIntentAlignmentAdvisory(stores, "hivecommons/hive", 42, misalignedVerdict(), restoreTestLogger())

	all := store.List(beads.ListFilter{})
	if len(all) != 1 {
		t.Fatalf("got %d beads after first record, want 1", len(all))
	}
	b := all[0]
	if b.Type != beads.TypeAdvisory {
		t.Errorf("bead type = %q, want %q", b.Type, beads.TypeAdvisory)
	}
	// "<owner>/<repo>#<n>", NOT the old "gh-<owner>/<repo>#<n>". The digest
	// split the ref on its first "/" to build a link, so the "gh-" prefix
	// became part of the OWNER and every one of these rendered a link to
	// github.com/gh-<owner>, which is not an org (#6080). This expectation
	// was pinning the malformed form.
	if b.ExternalRef != "hivecommons/hive#42" {
		t.Errorf("external ref = %q, want %q", b.ExternalRef, "hivecommons/hive#42")
	}
	if !strings.Contains(b.Notes, "diff exceeds linked issue scope") ||
		!strings.Contains(b.Notes, "unrelated-files") {
		t.Errorf("notes lack the alignment evidence: %q", b.Notes)
	}

	// Same PR again: the open advisory suppresses a duplicate.
	recordIntentAlignmentAdvisory(stores, "hivecommons/hive", 42, misalignedVerdict(), restoreTestLogger())
	if got := len(store.List(beads.ListFilter{})); got != 1 {
		t.Errorf("got %d beads after re-record of the same PR, want 1 (no duplicate open advisory)", got)
	}

	// A DIFFERENT PR is a different drift and gets its own advisory.
	recordIntentAlignmentAdvisory(stores, "hivecommons/hive", 43, misalignedVerdict(), restoreTestLogger())
	if got := len(store.List(beads.ListFilter{})); got != 2 {
		t.Errorf("got %d beads after recording a second PR, want 2", got)
	}
}

// Store selection order: the dedicated "intent" store wins, then "quality",
// then any non-nil store — and an all-nil or empty map is a silent no-op, so
// a hive without bead stores can never crash the intent gate.
func TestRecordIntentAlignmentAdvisoryStoreFallback(t *testing.T) {
	intentStore := newAdvisoryTestStore(t)
	qualityStore := newAdvisoryTestStore(t)

	stores := map[string]*beads.Store{"intent": intentStore, "quality": qualityStore}
	recordIntentAlignmentAdvisory(stores, "hivecommons/hive", 7, misalignedVerdict(), restoreTestLogger())
	if len(intentStore.List(beads.ListFilter{})) != 1 || len(qualityStore.List(beads.ListFilter{})) != 0 {
		t.Error("advisory not routed to the dedicated intent store")
	}

	fallback := map[string]*beads.Store{"intent": nil, "quality": qualityStore}
	recordIntentAlignmentAdvisory(fallback, "hivecommons/hive", 8, misalignedVerdict(), restoreTestLogger())
	if len(qualityStore.List(beads.ListFilter{})) != 1 {
		t.Error("nil intent store did not fall back to the quality store")
	}

	// No usable store anywhere: must be a quiet no-op, not a panic.
	recordIntentAlignmentAdvisory(map[string]*beads.Store{"x": nil}, "hivecommons/hive", 9, misalignedVerdict(), restoreTestLogger())
	recordIntentAlignmentAdvisory(nil, "hivecommons/hive", 10, misalignedVerdict(), restoreTestLogger())
}

// A bead written BEFORE the #6080 ref change carries the prefixed form. The
// dedup has to recognise it, or the first eval tick after upgrading would fail
// to see the existing advisory and open a duplicate for the same drift -- one
// per tick, forever.
func TestRecordIntentAlignmentAdvisoryDedupesLegacyPrefixedRef(t *testing.T) {
	store := newAdvisoryTestStore(t)
	if _, err := store.Create("Intent alignment drift in hivecommons/hive#42",
		beads.TypeAdvisory, beads.PriorityHigh, "intent", "gh-hivecommons/hive#42"); err != nil {
		t.Fatalf("seeding legacy bead: %v", err)
	}

	recordIntentAlignmentAdvisory(map[string]*beads.Store{"intent": store},
		"hivecommons/hive", 42, misalignedVerdict(), restoreTestLogger())

	if got := len(store.List(beads.ListFilter{})); got != 1 {
		t.Errorf("got %d beads, want 1 -- the pre-existing advisory carries the legacy gh- ref and must still suppress a duplicate", got)
	}
}

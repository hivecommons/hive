package beads

import (
	"testing"
	"time"
)

// TestBeadUpsert covers the contract advisory agents rely on: re-filing the
// same finding refreshes the existing bead instead of piling up duplicates,
// while a genuinely different finding opens its own bead.
func TestBeadUpsert(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}

	first, err := store.Upsert("pr-verifier.yml failing on run 3279", TypeAdvisory, PriorityMedium, "ci-maintainer", "gh-1")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	firstSeen, ok := first.LastSeen()
	if !ok {
		t.Fatal("first upsert did not stamp LastSeenAt — staleness pruning would never consider this bead")
	}

	// Cosmetic drift only (a different run number): the SAME finding.
	time.Sleep(2 * time.Millisecond)
	again, err := store.Upsert("pr-verifier.yml failing on run 3291", TypeAdvisory, PriorityCritical, "ci-maintainer", "gh-1")
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("re-upsert created bead %s, want the existing %s", again.ID, first.ID)
	}
	againSeen, ok := again.LastSeen()
	if !ok {
		t.Fatal("re-upsert cleared LastSeenAt")
	}
	if !againSeen.After(firstSeen) {
		t.Errorf("LastSeenAt = %v, want it advanced past %v", againSeen, firstSeen)
	}
	if again.Priority != PriorityCritical {
		t.Errorf("priority = %v, want %v — a more severe re-report must raise the bead", again.Priority, PriorityCritical)
	}

	// A less severe re-report must never lower it back.
	lower, err := store.Upsert("pr-verifier.yml failing on run 3300", TypeAdvisory, PriorityLow, "ci-maintainer", "gh-1")
	if err != nil {
		t.Fatalf("lower-severity upsert: %v", err)
	}
	if lower.Priority != PriorityCritical {
		t.Errorf("priority = %v, want it held at %v", lower.Priority, PriorityCritical)
	}

	// A different subject is a different finding.
	other, err := store.Upsert("secrets scanning is disabled on the release workflow", TypeAdvisory, PriorityHigh, "scanner", "")
	if err != nil {
		t.Fatalf("distinct upsert: %v", err)
	}
	if other.ID == first.ID {
		t.Fatal("a distinct finding was folded into the existing bead")
	}
	if got := len(store.List(ListFilter{})); got != 2 {
		t.Errorf("store holds %d beads, want 2", got)
	}
}

// TestBeadUpsertClosedDoesNotMatch pins the recurrence case: once a finding is
// resolved, the same condition coming BACK must open a fresh bead rather than
// silently reviving a closed one, or the digest would stay quiet about it.
func TestBeadUpsertClosedDoesNotMatch(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	first, err := store.Upsert("repo permissions insufficient", TypeAdvisory, PriorityHigh, "scanner", "")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := store.Close(first.ID); err != nil {
		t.Fatalf("closing bead: %v", err)
	}

	again, err := store.Upsert("repo permissions insufficient", TypeAdvisory, PriorityHigh, "scanner", "")
	if err != nil {
		t.Fatalf("re-upsert after close: %v", err)
	}
	if again.ID == first.ID {
		t.Fatal("re-upsert revived the closed bead; a recurring condition must open a new one")
	}
	if again.Status != StatusOpen {
		t.Errorf("new bead status = %q, want %q", again.Status, StatusOpen)
	}
}

// TestBeadUpsertRejectsInvalidType keeps Upsert's validation identical to
// Create's — a typo'd type must fail loudly, not create an untyped bead.
func TestBeadUpsertRejectsInvalidType(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	if _, err := store.Upsert("whatever", BeadType("nonsense"), PriorityLow, "scanner", ""); err == nil {
		t.Fatal("Upsert accepted an invalid bead type")
	}
}

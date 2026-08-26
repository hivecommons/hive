package scheduler

import (
	"strings"
	"testing"
)

// enforceLabels is the seam every kick builder calls before joining
// attacker-controllable issue/PR labels into a prompt line. The
// policy-returning variant is covered elsewhere; these pin the public
// wrapper's contract: strict no-op when ioscan is off, per-label redaction
// (never dropping a label) when on.

func TestEnforceLabelsDisabledReturnsInputUnchanged(t *testing.T) {
	s := newSchedulerWithIoscan(false)
	labels := []string{"bug", blockingTitle}
	got := s.enforceLabels(labels)
	if len(got) != 2 || got[0] != "bug" || got[1] != blockingTitle {
		t.Fatalf("disabled ioscan must be a strict no-op: got %v", got)
	}
}

func TestEnforceLabelsEmptyInput(t *testing.T) {
	s := newSchedulerWithIoscan(true)
	if got := s.enforceLabels(nil); len(got) != 0 {
		t.Fatalf("nil labels: got %v, want empty", got)
	}
}

func TestEnforceLabelsRedactsMaliciousKeepsBenign(t *testing.T) {
	s := newSchedulerWithIoscan(true)
	labels := []string{"good-first-issue", blockingTitle, "quality"}
	got := s.enforceLabels(labels)
	if len(got) != len(labels) {
		t.Fatalf("labels dropped: got %d, want %d (%v)", len(got), len(labels), got)
	}
	if got[0] != "good-first-issue" || got[2] != "quality" {
		t.Fatalf("benign labels mutated: %v", got)
	}
	if strings.Contains(got[1], "ignore previous") {
		t.Fatalf("raw injection leaked through label enforcement: %q", got[1])
	}
}

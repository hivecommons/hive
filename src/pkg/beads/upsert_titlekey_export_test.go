package beads

// Tests for the exported UpsertTitleKey wrapper (beads.go). The advisory
// provenance gate relies on it returning EXACTLY the key Upsert matches on,
// including cosmetic-drift folding — so pin the exported contract against the
// internal upsertTitleKey, not a reconstruction of it.

import "testing"

func TestUpsertTitleKey_MatchesInternalKey(t *testing.T) {
	titles := []string{
		"scanner: flaky run #3279 detected",
		"scanner: flaky run #3291 detected", // cosmetic drift folds to same key
		"UPPER Case Title",
		"1234 5678", // no letters: falls back to trimmed lowercase
		"",
	}
	for _, title := range titles {
		if got, want := UpsertTitleKey(title), upsertTitleKey(title); got != want {
			t.Errorf("UpsertTitleKey(%q) = %q, want internal key %q", title, got, want)
		}
	}
}

func TestUpsertTitleKey_FoldsCosmeticDrift(t *testing.T) {
	a := UpsertTitleKey("scanner: flaky run #3279 detected")
	b := UpsertTitleKey("scanner: flaky run #3291 detected")
	if a != b {
		t.Fatalf("cosmetic drift not folded: %q vs %q", a, b)
	}
	c := UpsertTitleKey("scanner: DIFFERENT words entirely")
	if a == c {
		t.Fatalf("semantically different titles collided on key %q", a)
	}
}

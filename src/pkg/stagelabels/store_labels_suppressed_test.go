package stagelabels

import (
	"testing"
	"time"
)

// The gate's reason lives only while the labels are suppressed: a landed
// write clears it, whatever the caller passes.
func TestNoteLabelsSuppressedKeepsReasonOnlyWhileSuppressed(t *testing.T) {
	s := NewStore("", time.Now)
	key := Key("projectbluefin/utah-packages", 812)
	s.Observe(key, NeedsDecision, "adoption-stalled:reviewer-verdict:changes_requested")
	s.NoteLabelsSuppressed(key, true, "backfill-off — #812 was created before hive could write")
	if r, _ := s.Get(key); !r.LabelsSuppressed || r.LabelsSuppressedReason != "backfill-off — #812 was created before hive could write" {
		t.Fatalf("suppressed: %+v", r)
	}
	s.NoteLabelsSuppressed(key, true, "needs-opt-in-label")
	if r, _ := s.Get(key); r.LabelsSuppressedReason != "needs-opt-in-label" {
		t.Fatalf("a new reason must replace the old one: %+v", r)
	}
	s.NoteLabelsSuppressed(key, false, "needs-opt-in-label")
	if r, _ := s.Get(key); r.LabelsSuppressed || r.LabelsSuppressedReason != "" {
		t.Fatalf("landed: %+v, want flag and reason cleared", r)
	}
	s.NoteLabelsSuppressed(Key("o/r", 1), true, "x")
	if _, ok := s.Get(Key("o/r", 1)); ok {
		t.Fatal("a missing record must stay missing")
	}
}

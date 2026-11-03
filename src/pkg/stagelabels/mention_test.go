package stagelabels

import (
	"path/filepath"
	"testing"
	"time"
)

// An @-mention is due once per distinct cause class, not once per head: the
// same cause again is silent, a new cause mentions, and an empty cause never
// does.
func TestMentionDueOncePerCause(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	s.Observe("k", NeedsAuthor, "conflicting")
	r, _ := s.Get("k")
	if !r.MentionDue("conflicting") {
		t.Fatal("first cause must mention")
	}
	s.MarkMentioned("k", "conflicting")
	r, _ = s.Get("k")
	if r.MentionDue("conflicting") {
		t.Fatal("same cause must not mention again")
	}
	if !r.MentionDue("check-failing:unit") {
		t.Fatal("new cause must mention")
	}
	s.MarkMentioned("k", "check-failing:unit")
	r, _ = s.Get("k")
	for _, same := range []string{"check-failing:lint", "required-check-failing:unit", "check-failing:unit(1 of 3)"} {
		if r.MentionDue(same) {
			t.Fatalf("%q is the same cause class as a failing check and must not mention again", same)
		}
	}
	if r.MentionDue("") {
		t.Fatal("empty cause never mentions")
	}
	if !r.MentionDue("conflicting") {
		t.Fatal("cause changing back must mention again")
	}
}

package writeback

import (
	"testing"
	"time"
)

// Decide answers what Allowed would, without the refusal landing in the
// dashboard's suppressed list: a note that describes a stage write another
// writer makes must not count as a second suppression.
func TestDecideAnswersLikeAllowedWithoutRecording(t *testing.T) {
	g := New(Config{}, Deps{LevelFor: func(string) int { return 5 }}, "")
	pr := Item{Number: 491, IsPR: true, Author: "kylerankin", CreatedAt: time.Now()}
	d := g.Decide("o/r", pr, KindStageLabel)
	if d.Allowed || d.Reason == "" {
		t.Fatalf("Decide on an un-opted contributor PR = %+v, want refused with a reason", d)
	}
	if n := len(g.Recent()); n != 0 {
		t.Fatalf("Decide recorded %d suppression(s), want none", n)
	}
	if a := g.Allowed("o/r", pr, KindStageLabel); a.Allowed != d.Allowed || a.Reason != d.Reason {
		t.Fatalf("Allowed = %+v, Decide = %+v: they must agree", a, d)
	}
	if n := len(g.Recent()); n != 1 {
		t.Fatalf("Allowed recorded %d suppression(s), want 1", n)
	}
	pr.Labels = []string{DefaultOptInLabel}
	if d := g.Decide("o/r", pr, KindStageLabel); !d.Allowed {
		t.Fatalf("opted-in PR = %+v, want allowed", d)
	}
	if d := (*Gate)(nil).Decide("o/r", pr, KindStageLabel); !d.Allowed {
		t.Fatalf("nil gate = %+v, want allowed", d)
	}
}

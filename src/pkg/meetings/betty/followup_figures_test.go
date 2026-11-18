package betty

import (
	"strings"
	"testing"
)

func TestHasUnsourcedFigure(t *testing.T) {
	for _, c := range []struct {
		summary string
		want    bool
	}{
		{"About 9,300 to 9,400 attendees at the 2025 Atlanta event.", true},
		{"Roughly 12 thousand people came.", true},
		{"Per cncf.io, 9,300 attendees came to the 2025 Atlanta event.", false},
		{"See https://events.linuxfoundation.org/ for 9,300 attendees.", false},
		{"Cert-manager renews it every night at 02:00.", false},
		{"Friday.", false},
	} {
		if got := hasUnsourcedFigure(c.summary); got != c.want {
			t.Errorf("hasUnsourcedFigure(%q) = %v, want %v", c.summary, got, c.want)
		}
	}
}

func TestFollowUpWithUnsourcedFigureSaysNotSure(t *testing.T) {
	r := newRig(t, testSettings(), &fakeLLM{})
	r.e.FollowUpIn("what was the attendance at KubeCon", "The last count was about 9,300 attendees.", false)
	f := r.reply()
	if f.Trigger != TriggerFollowUp || !strings.Contains(f.Text, "not sure") || strings.Contains(f.Text, "9,300") {
		t.Fatalf("follow-up %+v", f)
	}
}

package scheduler

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

func TestIsFoldedTracker(t *testing.T) {
	tests := []struct {
		name  string
		issue github.Issue
		want  bool
	}{
		{"marker first line", github.Issue{Body: foldedFindingMarker + "\nbody"}, true},
		{"leading whitespace", github.Issue{Body: "\n  " + foldedFindingMarker}, true},
		{"marker not first", github.Issue{Body: "intro\n" + foldedFindingMarker}, false},
		{"no marker", github.Issue{Body: "plain"}, false},
		{"closed", github.Issue{State: "closed", Body: foldedFindingMarker}, false},
		{"open state", github.Issue{State: "OPEN", Body: foldedFindingMarker}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFoldedTracker(tt.issue); got != tt.want {
				t.Errorf("isFoldedTracker = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFormatFoldedTrackersNote(t *testing.T) {
	s := &Scheduler{}
	if got := s.formatFoldedTrackersNote([]github.Issue{{Body: "plain"}}); got != "" {
		t.Errorf("expected empty note, got %q", got)
	}
	got := s.formatFoldedTrackersNote([]github.Issue{
		{Repo: "acme/widget", Number: 7, Title: "folded class", Body: foldedFindingMarker + "\nx"},
		{Repo: "acme/widget", Number: 8, Title: "ordinary", Body: "x"},
	})
	if !strings.Contains(got, "Folded trackers (1)") || !strings.Contains(got, "acme/widget#7 — folded class") {
		t.Errorf("tracker not listed:\n%s", got)
	}
	if strings.Contains(got, "ordinary") {
		t.Errorf("non-tracker leaked:\n%s", got)
	}
}

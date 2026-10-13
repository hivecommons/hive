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
		{"client flag, plain body", github.Issue{FoldedTracker: true, Body: "original finding"}, true},
		{"client flag, closed", github.Issue{State: "closed", FoldedTracker: true, Body: "original finding"}, false},
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

// The scanner lane: a tracker the client flagged at fold time is tagged in
// its work-list line and named in the summary line; unflagged issues are not.
func TestScannerMessageNamesFoldedTrackers(t *testing.T) {
	s := newScheduler()
	issues := []github.Issue{
		{Repo: "acme/widget", Number: 7, Title: "svg gate bypasses", Body: "original finding", FoldedTracker: true},
		{Repo: "acme/widget", Number: 8, Title: "ordinary", Body: "x"},
	}
	msg := s.buildScannerMessage(issues, emptyActionable())
	if !strings.Contains(msg, "svg gate bypasses [FOLDED-TRACKER]") {
		t.Errorf("tracker line not tagged:\n%s", msg)
	}
	if !strings.Contains(msg, "FOLDED-FINDING TRACKERS (1") || !strings.Contains(msg, "acme/widget#7") {
		t.Errorf("summary line missing tracker:\n%s", msg)
	}
	if strings.Contains(msg, "ordinary [FOLDED-TRACKER]") {
		t.Errorf("non-tracker tagged:\n%s", msg)
	}
	if got := s.buildScannerMessage(issues[1:], emptyActionable()); strings.Contains(got, "FOLDED-FINDING TRACKERS") {
		t.Errorf("summary line present with no trackers:\n%s", got)
	}
}

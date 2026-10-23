package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// foldedFindingMarker opens the body of a tracker issue that consolidates
// several agent-filed findings (written by the issue request watcher).
const foldedFindingMarker = "<!-- hive-finding-folded -->"

// maxFoldedTrackersNamed caps the section so it cannot crowd out ready work.
const maxFoldedTrackersNamed = 10

// isFoldedTracker reports whether an open issue is a folded-finding tracker:
// either the client saw a finding fold into it this process lifetime
// (Issue.FoldedTracker, #11587 — the watcher posts the marker as a comment,
// which the enumerated body never carries) or its body opens with the marker.
func isFoldedTracker(issue github.Issue) bool {
	if issue.State != "" && !strings.EqualFold(issue.State, "open") {
		return false
	}
	if issue.FoldedTracker {
		return true
	}
	return strings.HasPrefix(strings.TrimSpace(issue.Body), foldedFindingMarker)
}

// formatFoldedTrackersNote names the open folded-tracker issues so agents add
// later findings of that class as comments instead of filing new issues.
func (s *Scheduler) formatFoldedTrackersNote(issues []github.Issue) string {
	var trackers []github.Issue
	for _, issue := range issues {
		if isFoldedTracker(issue) {
			trackers = append(trackers, issue)
		}
	}
	if len(trackers) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\nFolded trackers (%d): several findings were consolidated into these issues — comment on the tracker with later findings of that class instead of filing new issues:\n", len(trackers)))
	for i, issue := range trackers {
		if i >= maxFoldedTrackersNamed {
			b.WriteString(fmt.Sprintf("  … and %d more\n", len(trackers)-i))
			break
		}
		title, _ := s.enforceTextFrom(issue.Title, issueSource(issue))
		b.WriteString(fmt.Sprintf("  %s — %s\n", issueDisplayRef(issue), title))
	}
	return b.String()
}

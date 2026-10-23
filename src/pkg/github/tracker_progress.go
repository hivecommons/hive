package github

import "strings"

// RendersAsTracker is the display-only tracker predicate, not a parent-close signal.
// It uses the same trackerTaskListMin threshold as isTracker to ensure consistency.
func RendersAsTracker(isTracker bool, labels []string, progress *IssueChildren) bool {
	// Render as tracker if the issue meets the isTracker predicate (which includes
	// title/label markers and 3+ task-list items), or if it has PartiallyAddressedLabel.
	// Note: progress.Total reflects all task-list items regardless of threshold;
	// only show the chip if it meets the same threshold as isTracker.
	return isTracker || issueHasLabel(labels, PartiallyAddressedLabel)
}

// TrackerTaskProgress counts issue-delegating checklist entries, not ordinary
// implementation tasks. Like isTracker, it requires trackerTaskListMin references.
// Checked boxes are the body's recorded completion state;
// a populated Issue.Children (from the child ledger) takes precedence.
func TrackerTaskProgress(body string) *IssueChildren {
	entries := trackerTaskListRe.FindAllString(body, -1)
	if len(entries) < trackerTaskListMin {
		return nil
	}
	progress := &IssueChildren{Total: len(entries)}
	for _, entry := range entries {
		if strings.Contains(strings.ToLower(entry), "[x]") {
			progress.Completed++
		}
	}
	return progress
}

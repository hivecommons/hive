package github

import "strings"

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

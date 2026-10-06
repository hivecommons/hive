package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// InflightLookup reports whether a work item is currently held by an active
// session, and by whom. holder is a short human-readable owner ("agent
// quality via Linear session sess-1").
type InflightLookup func(issue github.Issue) (holder string, held bool)

// SetInflightLookup installs (or with nil, removes) the in-flight lookup.
func (s *Scheduler) SetInflightLookup(fn InflightLookup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inflight = fn
}

// inflightHeld is one held item, for the kick note.
type inflightHeld struct {
	Ref    string
	Holder string
}

// splitInflight partitions issues into those free to hand out and those held.
// With no lookup installed everything is free.
func (s *Scheduler) splitInflight(issues []github.Issue) (free []github.Issue, held []inflightHeld) {
	s.mu.RLock()
	fn := s.inflight
	s.mu.RUnlock()
	if fn == nil {
		return issues, nil
	}
	free = make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if holder, ok := fn(issue); ok {
			held = append(held, inflightHeld{Ref: issueDisplayRef(issue), Holder: holder})
			continue
		}
		free = append(free, issue)
	}
	return free, held
}

// inflightNote renders the "these were withheld" note, or "" when nothing was.
func inflightNote(held []inflightHeld) string {
	if len(held) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## In Flight (withheld from your list)\n\n")
	b.WriteString("These items are already being worked through an active session and are NOT in your\n")
	b.WriteString("work list. Do not start, plan, comment on, or open PRs for them:\n")
	for _, h := range held {
		fmt.Fprintf(&b, "  %s — %s\n", h.Ref, h.Holder)
	}
	b.WriteString("\n")
	return b.String()
}

// inflightNoteHeader is the marker the seam checks for before appending.
const inflightNoteHeader = "## In Flight (withheld from your list)"

// addInflightNote appends the note for held items to a resolved kick message
// when the template did not place ${IN_FLIGHT} itself.
func (s *Scheduler) addInflightNote(message string, issues []github.Issue) string {
	if message == "" || strings.Contains(message, inflightNoteHeader) {
		return message
	}
	_, held := s.splitInflight(issues)
	note := inflightNote(held)
	if note == "" {
		return message
	}
	return strings.TrimRight(message, "\n") + "\n\n" + note
}

// freeOfInflight returns the issues not held by a session.
func (s *Scheduler) freeOfInflight(issues []github.Issue) []github.Issue {
	free, _ := s.splitInflight(issues)
	return free
}

// kickIssueRefs returns the issues a kick actually offers: not held by a
// session and not dependency-blocked. The blocked footer tells the agent
// "do NOT start these", so recording a claim for them (and posting the 🔒
// comment on GitHub every kick) would contradict the kick itself.
func (s *Scheduler) kickIssueRefs(issues []github.Issue) []github.Issue {
	ready, _ := partitionBlockedIssues(s.freeOfInflight(issues))
	return ready
}

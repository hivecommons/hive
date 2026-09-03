package scheduler

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/github"
)

// In-flight work filter.
//
// A work item can reach an agent by two roads: a work-source delegation that
// opens a session (Linear agent sessions today — the responder kicks the
// session agent immediately) and the governor's periodic sweep, which
// enumerates the same item into the backlog. Kicks do not interrupt a running
// agent (SendKick waits for the input prompt), so the danger is not a clobbered
// run but a RE-hand: the governor kicks the item again right after the
// session's run ends, and the agent does the work twice — or a second agent in
// the same lane picks it up in parallel.
//
// The scheduler therefore consults an injected in-flight lookup before it
// renders a kick. Held items are dropped from ${ISSUE_LIST} and from the
// kick's IssueRefs, and named in an "In flight" note so the agent knows why
// its list is shorter and does not go hunting for them. The lookup is a
// function so pkg/scheduler stays free of pkg/linearagent; main.go wires it
// to the Linear session tracker.

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

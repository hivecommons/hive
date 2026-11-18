package scheduler

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Whether a hand-off started (hivecommons/hive#18749).
//
// A kick that landed says only that the agent was told. The agent can
// decline (an advisory session asked to open a pull request does, and is
// right to), or end its turn having done nothing, and a record that counted
// the delivery as the hand-off would hide that for good: once per item and
// state, nothing retries. So a delivered hand-off is recorded as pending and
// reviewed on later passes, while the item is still in the state it was
// handed off in, against the deliverable the model requires for that state:
//
//   - a profile that opens a pull request: the item leaves the state when
//     the pull request covers it, so an item still in the state has no
//     deliverable yet;
//   - a reviewing profile: its comment moves the item's updated_at past the
//     hand-off.
//
// An item that left the state settles its hand-off as started
// (WorkflowHandoffLeftState).
//
// The hand-off is not started when the agent may not do what the profile
// needs (a record from before the resolver checked that), when the agent
// declined, when its turn ended and the deliverable has not shown up within
// WorkflowHandoffGrace, or, with no word on the turn at all, within
// WorkflowHandoffCeiling. A hand-off that was not started is planned again
// for a different agent, WorkflowDispatchMaxAttempts hand-offs in all, and
// is then left for the owner's "Send to <profile>".

// Hand-off statuses (WorkflowDispatchEntry.Status).
const (
	// WorkflowHandoffPending: the kick landed; the deliverable is awaited.
	WorkflowHandoffPending = "handed_off"
	// WorkflowHandoffStarted: the deliverable showed up.
	WorkflowHandoffStarted = "started"
	// WorkflowHandoffNotStarted: the agent declined or did not act.
	WorkflowHandoffNotStarted = "not_started"
)

// WorkflowDispatchMaxAttempts bounds the hand-offs of one item and state to
// agents that could do the work: the first, and one automatic retry with a
// different agent. Past it the item waits for the owner.
const WorkflowDispatchMaxAttempts = 2

// WorkflowHandoffGrace is how long after the agent's turn ended the
// deliverable may still show up (the pull-request relay opens the pull
// request after the turn, and the item is read from a cache).
const WorkflowHandoffGrace = 15 * time.Minute

// WorkflowHandoffCeiling is how long a hand-off may stay pending when
// nothing is known about the agent's turn (a restart forgets it).
const WorkflowHandoffCeiling = 2 * time.Hour

// Retryable reports whether the hand-off was not started and may be made
// again automatically.
func (e WorkflowDispatchEntry) Retryable() bool {
	return e.Status == WorkflowHandoffNotStarted && e.Attempts < WorkflowDispatchMaxAttempts
}

// Settled reports whether nothing more will be learned about the hand-off
// from reviewing it.
func (e WorkflowDispatchEntry) Settled() bool {
	return e.Status == WorkflowHandoffStarted || e.Status == WorkflowHandoffNotStarted
}

// WorkflowHandoffEvidence is what a pass knows about a recorded hand-off
// whose item is still in the state it was handed off in.
type WorkflowHandoffEvidence struct {
	Now time.Time
	// Unqualified says why the recorded agent may not do what the profile
	// needs ("" when it may).
	Unqualified string
	// Reviews is true when the deliverable is a comment on the item, false
	// when it is a pull request.
	Reviews bool
	// ItemUpdatedAt is the item's updated_at as last read.
	ItemUpdatedAt time.Time
	// Declined is the line the agent ended the hand-off's turn on when it
	// declined, asked what to do or reported nothing produced.
	Declined string
	// TurnEndedAt is when the hand-off's turn was seen to be over; zero
	// while it runs or when nothing is known.
	TurnEndedAt time.Time
}

// ReviewWorkflowHandoff decides what the evidence says about a recorded
// hand-off. It returns the record to store and whether it changed.
func ReviewWorkflowHandoff(e WorkflowDispatchEntry, ev WorkflowHandoffEvidence) (WorkflowDispatchEntry, bool) {
	if e.Settled() {
		return e, false
	}
	deliverable := "opening the pull request"
	if ev.Reviews {
		deliverable = "commenting on the item"
	}
	notStarted := func(reason string, counts bool) (WorkflowDispatchEntry, bool) {
		e.Status, e.Reason = WorkflowHandoffNotStarted, reason
		e = stampReason(e, ev.Now)
		if counts {
			if e.Attempts < 1 {
				e.Attempts = 1
			}
			e.Tried = appendUnique(e.Tried, e.Agent)
		} else {
			// The agent could never have done it: the hand-off is no
			// attempt, and the agent is free to be picked once it can.
			if e.Attempts > 0 {
				e.Attempts--
			}
			e.Tried = without(e.Tried, e.Agent)
		}
		at := ev.Now
		e.CheckedAt = &at
		return e, true
	}
	started := func() (WorkflowDispatchEntry, bool) {
		e.Status, e.Reason = WorkflowHandoffStarted, ""
		e = clearReason(e)
		at := ev.Now
		e.CheckedAt = &at
		return e, true
	}
	if ev.Unqualified != "" {
		return notStarted(e.Agent+" "+ev.Unqualified, false)
	}
	if ev.Declined != "" {
		return notStarted(fmt.Sprintf("%s declined the hand-off: %s", e.Agent, declineReason(ev.Declined)), true)
	}
	turnOver := !ev.TurnEndedAt.IsZero() && !ev.TurnEndedAt.Before(e.At)
	pastCeiling := ev.Now.Sub(e.At) >= WorkflowHandoffCeiling
	if ev.Reviews && (turnOver || pastCeiling) && ev.ItemUpdatedAt.After(e.At) {
		return started()
	}
	if turnOver {
		if ev.Now.Sub(ev.TurnEndedAt) < WorkflowHandoffGrace {
			return e, false
		}
		return notStarted(fmt.Sprintf("%s ended its turn without %s", e.Agent, deliverable), true)
	}
	if pastCeiling {
		return notStarted(fmt.Sprintf("%s did not finish %s within %s of the hand-off", e.Agent, deliverable, WorkflowHandoffCeiling), true)
	}
	return e, false
}

// WorkflowHandoffLeftState is the record of an undecided hand-off whose item
// is no longer in the state it was handed off in: the deliverable landed (a
// pull request covers the issue) or a person moved the item on. Either way
// the hand-off is over, and if the item ever returns to the state (a
// discarded draft) it is not handed off again on its own: that retry is a
// person's choice.
func WorkflowHandoffLeftState(e WorkflowDispatchEntry, now time.Time) WorkflowDispatchEntry {
	e.Status, e.Reason = WorkflowHandoffStarted, ""
	e = clearReason(e)
	e.CheckedAt = &now
	return e
}

// NextWorkflowDispatchEntry is the record a new hand-off of the same key
// writes when its kick lands: pending, with the attempts and agents of the
// hand-offs before it carried over.
func NextWorkflowDispatchEntry(e WorkflowDispatchEntry, prior *WorkflowDispatchEntry) WorkflowDispatchEntry {
	e.Status, e.Reason, e.CheckedAt = WorkflowHandoffPending, "", nil
	e = clearReason(e)
	e.Attempts, e.Tried = 1, []string{e.Agent}
	if prior != nil {
		e.Attempts = prior.Attempts + 1
		e.Tried = appendUnique(append([]string(nil), prior.Tried...), e.Agent)
	}
	return e
}

// NotStarted returns the hand-offs that were not started, by reference.
func (l *WorkflowDispatchLog) NotStarted() []WorkflowDispatchEntry {
	var out []WorkflowDispatchEntry
	for _, e := range l.Entries() {
		if e.Status == WorkflowHandoffNotStarted {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// WorkflowHandoffNext says what happens to a hand-off that was not started.
func WorkflowHandoffNext(e WorkflowDispatchEntry) string {
	if e.Retryable() {
		return "the hive hands it to another agent that qualifies, if there is one"
	}
	return "left for the owner: Send to " + strings.TrimSpace(e.Profile) + " on the item's row"
}

// declineMarkerRe matches the stand-down marker a declining agent's line
// opens with (WorkflowDeclineMarker, after any bullet).
var declineMarkerRe = regexp.MustCompile(`(?i)^[^a-z0-9]*stand(?:ing)?[ -]down[^a-z0-9]*`)

// declineReason is the agent's line without the marker it opens with. The
// reason is repeated in the retry's prompt, where a line that happened to
// start with the marker would read as the next agent standing down.
func declineReason(line string) string {
	reason := strings.TrimSpace(declineMarkerRe.ReplaceAllString(strings.TrimSpace(line), ""))
	if reason == "" {
		return "it stood down without a reason"
	}
	return reason
}

func appendUnique(list []string, s string) []string {
	if s == "" {
		return list
	}
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func without(list []string, s string) []string {
	var out []string
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

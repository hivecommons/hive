package scheduler

import (
	"fmt"
	"time"
)

// Where a replayed reason came from.
//
// The hand-off ledger keeps the last reason an agent gave for not starting an
// item, and the planner prints it on every pass while the item waits. The
// text outlives the session that wrote it, so a line printed hours later reads
// as a new refusal. Each reason therefore carries when it was decided
// (reason_at), which hand-off session decided it (reason_session), and, at
// print time, its age and whether a newer hand-off of the same item has since
// replaced the state it described (stale).

// ReasonSessionUnknown labels a reason written before provenance was
// recorded.
const ReasonSessionUnknown = "unknown (before provenance was recorded)"

// WorkflowHandoffSession names the hand-off a record belongs to: the agent
// and the delivery time. It holds ids only, never agent text.
func WorkflowHandoffSession(e WorkflowDispatchEntry) string {
	if e.Agent == "" || e.At.IsZero() {
		return ""
	}
	return e.Agent + "@" + e.At.UTC().Format(time.RFC3339)
}

// stampReason records when and in which hand-off e's reason was decided.
func stampReason(e WorkflowDispatchEntry, at time.Time) WorkflowDispatchEntry {
	e.ReasonAt = &at
	e.ReasonSession = WorkflowHandoffSession(e)
	return e
}

// clearReason drops the provenance of a reason that no longer applies.
func clearReason(e WorkflowDispatchEntry) WorkflowDispatchEntry {
	e.ReasonAt, e.ReasonSession = nil, ""
	return e
}

// ReasonOrigin says where a replayed reason came from.
type ReasonOrigin struct {
	// At is when the reason was decided; zero when unknown.
	At time.Time
	// Age is how long ago, in a few words ("3h 5m"); empty when At is
	// unknown.
	Age string
	// Session is the hand-off that decided it, or ReasonSessionUnknown.
	Session string
	// Stale is true once a newer hand-off of the same item exists: the
	// reason describes a session that is over, not the item's state now.
	Stale bool
}

// ReasonDecidedAt is when e's reason was decided: the recorded reason time,
// else when the hand-off was last reviewed, else when it was delivered (a
// record written before provenance was kept).
func (e WorkflowDispatchEntry) ReasonDecidedAt() time.Time {
	switch {
	case e.ReasonAt != nil:
		return *e.ReasonAt
	case e.CheckedAt != nil:
		return *e.CheckedAt
	}
	return e.At
}

// ReplayedReasonOrigin is the origin of e's reason as of now. superseded is
// whether a newer hand-off of the item exists (WorkflowDispatchLog.Superseded).
func ReplayedReasonOrigin(e WorkflowDispatchEntry, now time.Time, superseded bool) ReasonOrigin {
	o := ReasonOrigin{Session: e.ReasonSession, Stale: superseded, At: e.ReasonDecidedAt()}
	if o.Session == "" {
		o.Session = ReasonSessionUnknown
	}
	if !o.At.IsZero() {
		o.Age = HandoffAge(now.Sub(o.At))
	}
	return o
}

// Label renders the origin for a log line or a tooltip: no agent text.
func (o ReasonOrigin) Label() string {
	s := "decided"
	if o.Age != "" {
		s += " " + o.Age + " ago"
	} else {
		s += " at an unknown time"
	}
	s += " in session " + o.Session
	if o.Stale {
		s += "; stale, a newer hand-off of this item exists"
	}
	return s
}

// HandoffAge renders d as its two largest units ("2d 3h", "3h 5m", "40s").
func HandoffAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	days, hours := int(d/(24*time.Hour)), int(d/time.Hour)%24
	mins, secs := int(d/time.Minute)%60, int(d/time.Second)%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm %ds", mins, secs)
	}
	return fmt.Sprintf("%ds", secs)
}

// Superseded reports whether a hand-off of the same item was delivered after
// e's reason was decided, under any key: the reason is then history, not the
// item's current state.
func (l *WorkflowDispatchLog) Superseded(e WorkflowDispatchEntry) bool {
	if l == nil {
		return false
	}
	decided := e.ReasonDecidedAt()
	for _, o := range l.Entries() {
		if o.Ref == e.Ref && o.Key != e.Key && o.At.After(decided) {
			return true
		}
	}
	return false
}

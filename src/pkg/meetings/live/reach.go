package live

import (
	"context"
	"time"

	"github.com/hivecommons/hive/pkg/meetings"
)

// Reaching the meeting (live Teams test: every chat reply failed, and the
// bot sat silent in the call). A reply that gets out in neither chat nor
// voice is kept in the meeting record as not delivered and counted in the
// live state; the count clears on the next delivery. Once nothing has got
// out for DefaultUnreachableWarnAfter the meeting view says the bot cannot
// reach the participants, and past DefaultUnreachableLeaveAfter the bot
// leaves with the state reason meetings.ReasonUnreachable rather than stay
// in a meeting it cannot talk to.
const (
	// DefaultUnreachableWarnAfter is how long no reply may get out before
	// the meeting is marked unreachable.
	DefaultUnreachableWarnAfter = 3 * time.Minute
	// DefaultUnreachableLeaveAfter is how long that may last before the
	// bot leaves.
	DefaultUnreachableLeaveAfter = 10 * time.Minute
	// unreachableMinFailures is the least undelivered replies in a row
	// that count as unreachable: one failed post is not a pattern.
	unreachableMinFailures = 2
	// unreachableLeaveRetry spaces the tries of a leave that failed.
	unreachableLeaveRetry = time.Minute
)

// reached clears the undelivered count after a delivery.
func (s *session) reached() {
	if s.state.Undelivered == 0 && s.state.UnreachableSince == nil {
		return
	}
	if s.state.Unreachable {
		s.log().Info("[meetings] the meeting is reachable again", "undelivered", s.state.Undelivered)
	}
	s.state.Undelivered, s.state.UnreachableSince, s.state.Unreachable, s.state.UndeliveredReason = 0, nil, false, ""
	s.emit(Event{Kind: EventReach})
}

// unreached counts a reply that got out in neither channel.
func (s *session) unreached(ctx context.Context, reason string) {
	now := s.now()
	s.state.Undelivered++
	s.state.UndeliveredReason = reason
	if s.state.UnreachableSince == nil {
		s.state.UnreachableSince = &now
	}
	s.log().Warn("[meetings] reply not delivered in chat or by voice", "undelivered", s.state.Undelivered, "reason", reason)
	s.emit(Event{Kind: EventReach, Reason: reason})
	s.checkReach(ctx)
}

// checkReach marks the meeting unreachable once nothing has got out for
// UnreachableWarnAfter, and leaves it past UnreachableLeaveAfter. It runs
// after every undelivered reply and on the session's tick.
func (s *session) checkReach(ctx context.Context) {
	since := s.state.UnreachableSince
	if since == nil || s.state.Undelivered < unreachableMinFailures {
		return
	}
	now := s.now()
	age := now.Sub(*since)
	if age >= s.r.opts.UnreachableWarnAfter && !s.state.Unreachable {
		s.state.Unreachable = true
		s.log().Warn("[meetings] cannot reach the participants: no reply has been delivered",
			"since", since.UTC().Format(time.RFC3339), "undelivered", s.state.Undelivered, "reason", s.state.UndeliveredReason)
		s.emit(Event{Kind: EventReach, Reason: s.state.UndeliveredReason})
		s.save()
	}
	if age < s.r.opts.UnreachableLeaveAfter || (!s.leaveTried.IsZero() && now.Sub(s.leaveTried) < unreachableLeaveRetry) {
		return
	}
	lv, ok := s.r.opts.Meetings.(Leaver)
	if !ok {
		return
	}
	s.leaveTried = now
	s.log().Warn("[meetings] leaving a meeting the bot cannot reach", "unreachable_for", age.Round(time.Second).String(), "undelivered", s.state.Undelivered)
	cctx, cancel := context.WithTimeout(ctx, s.r.opts.CallTimeout)
	defer cancel()
	if _, err := lv.Leave(cctx, s.id, meetings.ReasonUnreachable); err != nil {
		s.log().Warn("[meetings] leaving an unreachable meeting failed; will retry", "error", err)
	}
}

package schedule

import (
	"errors"
	"fmt"
)

// Deleting Calendar entries (the dashboard's 🗑 Delete on past entries):
// only a finished event may be deleted; one still scheduled, awaiting
// approval or live keeps Skip and Cancel. Deleting a meeting from the
// Meetings section forgets it here too (ForgetMeeting).

// LinkedMeetings returns the ids of the meetings an event created: the
// current one and those in its history, each once.
func LinkedMeetings(e Event) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(e.MeetingID)
	for _, h := range e.History {
		add(h.MeetingID)
	}
	return out
}

// Delete removes a finished event (ended, cancelled, declined, failed,
// blocked, skipped). Anything else is ErrConflict: skip or cancel it
// instead.
func (s *Scheduler) Delete(id string) (Event, error) {
	return s.opts.Store.Remove(id, func(e Event) error {
		if !e.Status.Terminal() {
			return fmt.Errorf("%w: %s is %s; skip or cancel it instead", ErrConflict, id, e.Status)
		}
		return nil
	})
}

// ForgetMeeting drops a deleted meeting from the Calendar: its history
// entries go, and a finished event that existed only for that meeting is
// removed. A series or an event with other occurrences stays. It returns
// how many events were removed.
func (s *Scheduler) ForgetMeeting(meetingID string) int {
	if meetingID == "" {
		return 0
	}
	removed := 0
	for _, e := range s.opts.Store.List() {
		linked := false
		for _, id := range LinkedMeetings(e) {
			if id == meetingID {
				linked = true
			}
		}
		if !linked {
			continue
		}
		next, err := s.opts.Store.Update(e.ID, func(e *Event) error {
			kept := e.History[:0:0]
			for _, h := range e.History {
				if h.MeetingID != meetingID {
					kept = append(kept, h)
				}
			}
			e.History = kept
			// A live event keeps its link: reconcile records the missing
			// meeting as ended on the next tick.
			if e.MeetingID == meetingID && e.Status != StatusLive {
				e.MeetingID = ""
			}
			return nil
		})
		if err != nil {
			s.log.Warn("[schedule] forgetting a deleted meeting failed", "event", e.ID, "meeting", meetingID, "error", err)
			continue
		}
		if !next.Status.Terminal() || next.Recurring() || len(LinkedMeetings(next)) > 0 {
			continue
		}
		if _, err := s.opts.Store.Remove(next.ID, func(cur Event) error {
			if !cur.Status.Terminal() || len(LinkedMeetings(cur)) > 0 {
				return errUnchanged
			}
			return nil
		}); err != nil {
			if !errors.Is(err, errUnchanged) {
				s.log.Warn("[schedule] removing a deleted meeting's event failed", "event", next.ID, "error", err)
			}
			continue
		}
		removed++
	}
	return removed
}

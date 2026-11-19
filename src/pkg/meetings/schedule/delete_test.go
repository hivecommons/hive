package schedule

import (
	"errors"
	"testing"
)

// putEvent stores ev as is (status, history and links included).
func putEvent(t *testing.T, s *Scheduler, ev Event) Event {
	t.Helper()
	ev.ID = EventID(ev.UID, ev.RecurrenceID)
	got, err := s.opts.Store.Upsert(ev, func(_ *Event, in Event) Event { return in })
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDeleteOnlyFinishedEvents(t *testing.T) {
	c := &clock{t: t0}
	s := newTestScheduler(t, t.TempDir(), c, &fakeJoiner{})
	for _, st := range []Status{StatusScheduled, StatusAwaitingApproval, StatusLive} {
		e := putEvent(t, s, Event{UID: "u-" + string(st), Start: t0, Status: st})
		if _, err := s.Delete(e.ID); !errors.Is(err, ErrConflict) {
			t.Errorf("Delete %s = %v, want ErrConflict", st, err)
		}
		if _, err := s.Get(e.ID); err != nil {
			t.Errorf("%s event removed: %v", st, err)
		}
	}
	for _, st := range []Status{StatusEnded, StatusCancelled, StatusDeclined, StatusFailed, StatusBlocked, StatusSkipped} {
		e := putEvent(t, s, Event{UID: "u-" + string(st), Start: t0, Status: st, MeetingID: "mtg-" + string(st)})
		got, err := s.Delete(e.ID)
		if err != nil || got.MeetingID != "mtg-"+string(st) {
			t.Errorf("Delete %s = %+v, %v", st, got, err)
		}
		if _, err := s.Get(e.ID); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s event kept: %v", st, err)
		}
	}
	if _, err := s.Delete("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown = %v", err)
	}
}

func TestForgetMeeting(t *testing.T) {
	c := &clock{t: t0}
	s := newTestScheduler(t, t.TempDir(), c, &fakeJoiner{})
	single := putEvent(t, s, Event{UID: "single", Start: t0, Status: StatusFailed, MeetingID: "mtg-1",
		History: []Occurrence{{Start: t0, MeetingID: "mtg-1", Status: StatusFailed}}})
	series := putEvent(t, s, Event{UID: "series", Start: t0, Status: StatusScheduled, RRule: "FREQ=WEEKLY", MeetingID: "mtg-3",
		History: []Occurrence{{Start: t0, MeetingID: "mtg-2", Status: StatusEnded}, {Start: t0.AddDate(0, 0, 7), MeetingID: "mtg-3", Status: StatusEnded}}})
	other := putEvent(t, s, Event{UID: "other", Start: t0, Status: StatusEnded, MeetingID: "mtg-9"})

	if n := s.ForgetMeeting("mtg-1"); n != 1 {
		t.Fatalf("ForgetMeeting single = %d, want 1 removed", n)
	}
	if _, err := s.Get(single.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an event that existed only for the meeting was kept: %v", err)
	}
	if n := s.ForgetMeeting("mtg-3"); n != 0 {
		t.Fatalf("ForgetMeeting series = %d, want 0 removed", n)
	}
	got, err := s.Get(series.ID)
	if err != nil || len(got.History) != 1 || got.History[0].MeetingID != "mtg-2" || got.MeetingID != "" {
		t.Fatalf("series after forget = %+v, %v", got, err)
	}
	if _, err := s.Get(other.ID); err != nil {
		t.Fatalf("an unrelated event was touched: %v", err)
	}
	if n := s.ForgetMeeting(""); n != 0 {
		t.Fatal("empty id must be a no-op")
	}
}

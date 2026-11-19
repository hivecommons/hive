package meetings

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteStateMatrix(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name  string
		mt    Meeting
		ok    bool
		erase bool // meeting.delete called
	}{
		{"ended", Meeting{State: StateEnded, EndedAt: &now}, true, true},
		{"failed", Meeting{State: StateFailed, StateReason: ReasonDeclined, EndedAt: &now}, true, true},
		{"blocked", Meeting{State: StateBlocked, EndedAt: &now}, true, true},
		{"handed off", Meeting{State: StateHandedOff, EndedAt: &now}, true, false},
		{"purged", Meeting{State: StatePurged, EndedAt: &now}, true, false},
		{"pending request", Meeting{State: StateRequested, RequestedBy: &Person{Name: "andy"}}, true, true},
		{"dashboard join in flight", Meeting{State: StateRequested}, false, false},
		{"joining", Meeting{State: StateJoining}, false, false},
		{"lobby", Meeting{State: StateLobby}, false, false},
		{"live", Meeting{State: StateLive}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newFakeQuorum()
			m := newTestManager(t, q, 0)
			st := m.opts.Store
			mt := c.mt
			mt.ID, mt.NativeID, mt.CreatedAt = "mtg-x", "q-x", now
			if err := st.Save(mt); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendTranscript(mt.ID, Event{Type: EventChat, Text: "hello"}); err != nil {
				t.Fatal(err)
			}
			if err := st.SaveDoc(mt.ID, "state.json", map[string]string{"k": "v"}); err != nil {
				t.Fatal(err)
			}
			res, err := m.Delete(context.Background(), mt.ID)
			if !c.ok {
				if !errors.Is(err, ErrStillRunning) {
					t.Fatalf("Delete = %v, want ErrStillRunning", err)
				}
				if _, gerr := m.Get(mt.ID); gerr != nil {
					t.Fatalf("a running meeting was removed: %v", gerr)
				}
				if len(q.deletes) != 0 || len(q.leaves) != 0 {
					t.Fatalf("quorum called for a running meeting: deletes %v leaves %v", q.deletes, q.leaves)
				}
				return
			}
			if err != nil || res.UpstreamError != "" || res.Meeting.ID != mt.ID {
				t.Fatalf("Delete = %+v, %v", res, err)
			}
			if _, err := m.Get(mt.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("record still there: %v", err)
			}
			if _, err := os.Stat(filepath.Join(st.Dir(), mt.ID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("meeting dir (transcript, live state) still there: %v", err)
			}
			if erased := len(q.deletes) == 1 && q.deletes[0] == "q-x"; erased != c.erase {
				t.Fatalf("meeting.delete calls %v, want called=%v", q.deletes, c.erase)
			}
			if pending := isPendingRequest(c.mt); pending != (len(q.leaves) == 1) {
				t.Fatalf("leaves %v for pending=%v", q.leaves, pending)
			}
		})
	}
}

func TestDeleteUpstreamErrors(t *testing.T) {
	now := time.Now()
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	st := m.opts.Store
	if _, err := m.Delete(context.Background(), "mtg-none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	// Vexa already purged it: not an error.
	q.deleteErr = &ToolError{Tool: "meeting.delete", Code: CodeNotFound}
	_ = st.Save(Meeting{ID: "mtg-gone", NativeID: "q-gone", State: StateEnded, EndedAt: &now, CreatedAt: now})
	if res, err := m.Delete(context.Background(), "mtg-gone"); err != nil || res.UpstreamError != "" {
		t.Fatalf("not_found = %+v, %v", res, err)
	}
	// Any other Quorum error is surfaced, and the record is removed anyway.
	q.deleteErr = &ToolError{Tool: "meeting.delete", Code: CodeUpstreamError, Message: "vexa 500"}
	_ = st.Save(Meeting{ID: "mtg-err", NativeID: "q-err", State: StateFailed, EndedAt: &now, CreatedAt: now})
	res, err := m.Delete(context.Background(), "mtg-err")
	if err != nil || res.UpstreamError == "" {
		t.Fatalf("upstream error = %+v, %v", res, err)
	}
	if _, err := m.Get("mtg-err"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record kept after an upstream error: %v", err)
	}
	// A meeting Quorum never knew (no native id) is removed without a call.
	q.deleteErr = nil
	before := len(q.deletes)
	_ = st.Save(Meeting{ID: "mtg-local", State: StateFailed, EndedAt: &now, CreatedAt: now})
	if _, err := m.Delete(context.Background(), "mtg-local"); err != nil || len(q.deletes) != before {
		t.Fatalf("local = %v, deletes %v", err, q.deletes)
	}
}

func TestClearFinished(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour)
	later := now.Add(time.Hour)
	q := newFakeQuorum()
	m := newTestManager(t, q, 0)
	st := m.opts.Store
	_ = st.Save(Meeting{ID: "mtg-ended", NativeID: "q1", State: StateEnded, EndedAt: &old, CreatedAt: old})
	_ = st.Save(Meeting{ID: "mtg-blocked", NativeID: "q2", State: StateBlocked, EndedAt: &old, CreatedAt: old})
	_ = st.Save(Meeting{ID: "mtg-after", NativeID: "q3", State: StateEnded, EndedAt: &later, CreatedAt: old})
	_ = st.Save(Meeting{ID: "mtg-live", NativeID: "q4", State: StateLive, CreatedAt: old})
	_ = st.Save(Meeting{ID: "mtg-req", NativeID: "q5", State: StateRequested, RequestedBy: &Person{Name: "andy"}, CreatedAt: old})
	got, err := m.ClearFinished(context.Background(), now)
	if err != nil || len(got) != 2 {
		t.Fatalf("ClearFinished = %+v, %v", got, err)
	}
	for _, id := range []string{"mtg-ended", "mtg-blocked"} {
		if _, err := m.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s not cleared", id)
		}
	}
	for _, id := range []string{"mtg-after", "mtg-live", "mtg-req"} {
		if _, err := m.Get(id); err != nil {
			t.Errorf("%s cleared: %v", id, err)
		}
	}
}

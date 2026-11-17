package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/live"
)

// stubQuorum is a minimal meetings.Quorum: joins succeed, the event stream
// delivers what the test sends.
type stubQuorum struct {
	events chan meetings.Event
}

func (q *stubQuorum) Join(context.Context, meetings.JoinArgs) (meetings.JoinResult, error) {
	return meetings.JoinResult{MeetingID: "q1", State: meetings.StateJoining}, nil
}
func (q *stubQuorum) Leave(context.Context, string) (meetings.State, error) {
	return meetings.StateEnded, nil
}
func (q *stubQuorum) ChatPost(context.Context, string, string) error { return nil }
func (q *stubQuorum) Say(context.Context, string, string) error      { return nil }
func (q *stubQuorum) Delete(context.Context, string) error           { return nil }
func (q *stubQuorum) Events(ctx context.Context, id string, _ int64, fn func(meetings.Event) bool) error {
	if id == "" {
		<-ctx.Done()
		return ctx.Err()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-q.events:
			if !fn(ev) {
				return nil
			}
		}
	}
}

type stubSpek struct{ lines chan []byte }

func (s *stubSpek) Start(context.Context) error { return nil }
func (s *stubSpek) Send(any) error              { return nil }
func (s *stubSpek) SendLine([]byte) error       { return nil }
func (s *stubSpek) Lines() <-chan []byte        { return s.lines }
func (s *stubSpek) Close(any)                   {}

func TestMeetingsDashboardAdapter(t *testing.T) {
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := &stubQuorum{events: make(chan meetings.Event, 8)}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: q, ConsentText: "consent"})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	spek := &stubSpek{lines: make(chan []byte, 8)}
	lr, err := live.NewRunner(live.Options{Meetings: mgr, Store: store, NewSpek: func(meetings.Meeting) live.Spek { return spek }})
	if err != nil {
		t.Fatal(err)
	}
	lr.Start()
	defer lr.Close()
	d := &meetingsDashboard{mgr: mgr, live: lr}

	var kinds []string
	frames := make(chan dashboard.MeetingFrame, 1024)
	unsub := d.Subscribe(func(f dashboard.MeetingFrame) { frames <- f })
	defer unsub()

	if _, err := d.Join(context.Background(), dashboard.MeetingJoinRequest{}, "octo"); !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("invalid join = %v", err)
	}
	got, err := d.Join(context.Background(), dashboard.MeetingJoinRequest{URL: "https://meet.google.com/x"}, "octo")
	if err != nil {
		t.Fatal(err)
	}
	m, ok := got.(meetings.Meeting)
	if !ok || m.InvitedBy != "octo" {
		t.Fatalf("join = %#v", got)
	}
	q.events <- meetings.Event{Type: meetings.EventState, State: meetings.StateLive}
	q.events <- meetings.Event{Type: meetings.EventChat, Author: &meetings.Person{Name: "Ana"}, Text: "hi"}
	spek.lines <- []byte(`{"type":"notes.append","section":"notes","text":"n"}`)
	testutil.Eventually(t, 5*time.Second, func() bool {
		for {
			select {
			case f := <-frames:
				kinds = append(kinds, f.Kind)
			default:
				seen := map[string]bool{}
				for _, k := range kinds {
					seen[k] = true
				}
				return seen[dashboard.MeetingFrameMeeting] && seen["line"] && seen[live.EventNotes]
			}
		}
	}, "meeting, line and notes frames")

	if list, err := d.List(); err != nil || len(list.([]meetings.Meeting)) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if _, err := d.Get(m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get("mtg-none"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("missing = %v", err)
	}
	snap, err := d.Live(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	sl, ok := snap.(meetingLive)
	if !ok || sl.Live == nil {
		t.Fatalf("live = %#v", snap)
	}
	// People's lines; hive's own reply lines (the consent notice) are in the
	// transcript too.
	heard := 0
	for _, ev := range sl.Transcript {
		if !ev.IsReply() {
			heard++
		}
	}
	if heard != 1 {
		t.Fatalf("live transcript = %#v", sl.Transcript)
	}
	if _, err := d.Live("mtg-none"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("live missing = %v", err)
	}
	if v, err := d.SetVoice(m.ID, true); err != nil || !v.(meetings.Meeting).VoiceEnabled {
		t.Fatalf("voice = %v, %v", v, err)
	}
	if _, err := d.SetVoice("mtg-none", true); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("voice missing = %v", err)
	}
	if err := d.ApproveResearch(m.ID, "r-none"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("approve unknown = %v", err)
	}
	if err := d.ApproveResearch("mtg-none", "r"); !errors.Is(err, dashboard.ErrMeetingConflict) {
		t.Fatalf("approve no session = %v", err)
	}
	if v, err := d.Leave(context.Background(), m.ID); err != nil || v.(meetings.Meeting).State != meetings.StateEnded {
		t.Fatalf("leave = %v, %v", v, err)
	}
	if _, err := d.Leave(context.Background(), "mtg-none"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("leave missing = %v", err)
	}

	if _, err := d.ApproveRequest(context.Background(), m.ID, false); !errors.Is(err, dashboard.ErrMeetingConflict) {
		t.Fatalf("approve non-request = %v", err)
	}
	if _, err := d.DeclineRequest(context.Background(), "mtg-none"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("decline missing = %v", err)
	}
	for _, id := range []string{"mtg-req1", "mtg-req2"} {
		if err := store.Save(meetings.Meeting{ID: id, NativeID: "q-" + id, State: meetings.StateRequested, RequestedBy: &meetings.Person{ID: "u"}}); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := d.ApproveRequest(context.Background(), "mtg-req1", true); err != nil || !v.(meetings.Meeting).VoiceEnabled {
		t.Fatalf("approve = %v, %v", v, err)
	}
	if v, err := d.DeclineRequest(context.Background(), "mtg-req2"); err != nil || v.(meetings.Meeting).State != meetings.StateFailed {
		t.Fatalf("decline = %v, %v", v, err)
	}

	bare := &meetingsDashboard{mgr: mgr}
	if err := bare.ApproveResearch(m.ID, "r"); !errors.Is(err, dashboard.ErrMeetingConflict) {
		t.Fatalf("no live loop = %v", err)
	}
	if _, err := bare.Live(m.ID); err != nil {
		t.Fatal(err)
	}
	bare.Subscribe(func(dashboard.MeetingFrame) {})()
	if meetingsDashboardError(nil) != nil {
		t.Fatal("nil maps to nil")
	}
	if err := meetingsDashboardError(&meetings.ToolError{Code: meetings.CodeInvalidArgument}); !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("invalid_argument = %v", err)
	}
	plain := errors.New("x")
	if meetingsDashboardError(plain) != plain {
		t.Fatal("other errors pass through")
	}
}

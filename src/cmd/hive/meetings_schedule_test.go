package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

func TestAttendeeName(t *testing.T) {
	cases := map[string]string{
		"hive (notes)":            "Hive",
		"Betty Bee from the hive": "Betty Bee",
		"betty":                   "Betty",
		"":                        "Hive",
		"HIVE":                    "Hive",
		"Quorum Bot":              "Quorum Bot",
	}
	for in, want := range cases {
		if got := attendeeName(in); got != want {
			t.Errorf("attendeeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMeetingsScheduleError(t *testing.T) {
	cases := map[error]error{
		fmt.Errorf("%w: x", schedule.ErrNotFound):      dashboard.ErrMeetingNotFound,
		fmt.Errorf("%w: x", schedule.ErrInvalid):       dashboard.ErrMeetingInvalid,
		fmt.Errorf("%w: x", schedule.ErrConflict):      dashboard.ErrMeetingConflict,
		fmt.Errorf("%w: x", schedule.ErrStaleSequence): dashboard.ErrMeetingConflict,
	}
	for in, want := range cases {
		if got := meetingsScheduleError(in); !errors.Is(got, want) {
			t.Errorf("%v mapped to %v", in, got)
		}
	}
	if meetingsScheduleError(nil) != nil {
		t.Fatal("nil error mapped")
	}
	other := errors.New("disk full")
	if meetingsScheduleError(other) != other {
		t.Fatal("unknown error rewrapped")
	}
}

func TestMeetingsScheduleAdapter(t *testing.T) {
	dir := t.TempDir()
	store, err := meetings.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: &stubQuorum{events: make(chan meetings.Event, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	if svc, stop := wireMeetingsSchedule(nil, mgr, nil); svc != nil || stop != nil {
		t.Fatal("wired without config")
	}
	cfg := &config.Config{}
	cfg.Meetings.DataDir = dir
	cfg.Meetings.BotName = "Betty Bee from the hive"
	svc, stop := wireMeetingsSchedule(cfg, mgr, nil)
	if svc == nil {
		t.Fatal("schedule not wired")
	}
	defer stop()
	if svc.attendee != "Betty Bee" {
		t.Fatalf("attendee = %q", svc.attendee)
	}
	ctx := context.Background()
	start := time.Now().Add(time.Hour)
	added, err := svc.Add(ctx, dashboard.MeetingScheduleAdd{URL: "https://meet.google.com/abc", Title: "Sync", Start: start, AddedBy: "octo", ByOwner: true})
	if err != nil {
		t.Fatal(err)
	}
	ev := added.(schedule.Event)
	if ev.Status != schedule.StatusScheduled {
		t.Fatalf("owner add = %s", ev.Status)
	}
	pending, err := svc.Add(ctx, dashboard.MeetingScheduleAdd{URL: "https://meet.google.com/def", Start: start, AddedBy: "dev"})
	if err != nil || pending.(schedule.Event).Status != schedule.StatusAwaitingApproval {
		t.Fatalf("read-write add = %+v %v", pending, err)
	}
	if _, err := svc.Add(ctx, dashboard.MeetingScheduleAdd{URL: "https://example.com/x", Start: start}); !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("bad link = %v", err)
	}
	for _, action := range []string{dashboard.MeetingScheduleApprove, dashboard.MeetingScheduleSkip} {
		if _, err := svc.Act(ctx, pending.(schedule.Event).ID, action, "octo"); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
	}
	if _, err := svc.Act(ctx, ev.ID, dashboard.MeetingScheduleDecline, "octo"); !errors.Is(err, dashboard.ErrMeetingConflict) {
		t.Fatalf("decline scheduled = %v", err)
	}
	if _, err := svc.Act(ctx, ev.ID, dashboard.MeetingScheduleCancel, "octo"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Act(ctx, "missing", dashboard.MeetingScheduleCancel, "octo"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := svc.Act(ctx, ev.ID, "explode", "octo"); !errors.Is(err, dashboard.ErrMeetingInvalid) {
		t.Fatalf("unknown action = %v", err)
	}
	out, err := svc.List()
	if err != nil {
		t.Fatal(err)
	}
	l := out.(meetingsScheduleList)
	if l.AttendeeName != "Betty Bee" || len(l.Upcoming) != 0 || len(l.Recent) != 2 {
		t.Fatalf("list = %+v", l)
	}
	// A meeting update reaches the scheduler without blocking the manager.
	if _, err := mgr.Join(ctx, meetings.JoinRequest{URL: "https://meet.google.com/zzz"}); err != nil {
		t.Fatal(err)
	}
}

func TestWireMeetingsScheduleBadStore(t *testing.T) {
	store, err := meetings.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: &stubQuorum{events: make(chan meetings.Event, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	cfg := &config.Config{}
	// A data dir under a file cannot be created.
	cfg.Meetings.DataDir = "/dev/null/meetings"
	if svc, stop := wireMeetingsSchedule(cfg, mgr, nil); svc != nil || stop != nil {
		t.Fatal("wired over an unusable data dir")
	}
}

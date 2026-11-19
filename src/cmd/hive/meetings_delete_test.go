package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/meetings"
	"github.com/hivecommons/hive/pkg/meetings/schedule"
)

// TestMeetingsDeleteAdapters: deleting a meeting drops the Calendar entry
// that existed only for it; deleting a past Calendar entry deletes its
// meeting; live meetings and entries still to come answer 409.
func TestMeetingsDeleteAdapters(t *testing.T) {
	dir := t.TempDir()
	store, err := meetings.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-2 * time.Hour)
	for _, m := range []meetings.Meeting{
		{ID: "mtg-ended", NativeID: "q-e", State: meetings.StateEnded, EndedAt: &past, CreatedAt: past},
		{ID: "mtg-cal", NativeID: "q-c", State: meetings.StateFailed, EndedAt: &past, CreatedAt: past},
		{ID: "mtg-live", NativeID: "q-l", State: meetings.StateLive, CreatedAt: past},
	} {
		if err := store.Save(m); err != nil {
			t.Fatal(err)
		}
	}
	ev := func(uid string, st schedule.Status, start time.Time, mid string) schedule.Event {
		e := schedule.Event{UID: uid, Start: start, Status: st, Platform: schedule.PlatformGoogleMeet,
			JoinURL: "https://meet.google.com/abc", Source: schedule.SourceManual, MeetingID: mid, CreatedAt: past, UpdatedAt: now}
		if mid != "" {
			e.History = []schedule.Occurrence{{Start: start, MeetingID: mid, Status: st}}
		}
		e.ID = schedule.EventID(uid, nil)
		return e
	}
	ended := ev("u-ended", schedule.StatusEnded, past, "mtg-ended")
	failed := ev("u-cal", schedule.StatusFailed, past, "mtg-cal")
	future := ev("u-future", schedule.StatusScheduled, now.Add(24*time.Hour), "")
	data, err := json.Marshal(map[string]any{"version": 1, "events": []schedule.Event{ended, failed, future}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, schedule.FileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := meetings.NewManager(meetings.Options{Store: store, Quorum: &stubQuorum{events: make(chan meetings.Event, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	cfg := &config.Config{}
	cfg.Meetings.DataDir = dir
	svc, stop := wireMeetingsSchedule(cfg, mgr, nil)
	if svc == nil {
		t.Fatal("schedule not wired")
	}
	defer stop()
	md := &meetingsDashboard{mgr: mgr, sched: svc.sched}
	ctx := context.Background()

	if _, err := md.Delete(ctx, "mtg-live"); !errors.Is(err, dashboard.ErrMeetingConflict) {
		t.Fatalf("delete live = %v, want conflict", err)
	}
	if _, err := md.Delete(ctx, "mtg-none"); !errors.Is(err, dashboard.ErrMeetingNotFound) {
		t.Fatalf("delete unknown = %v, want not found", err)
	}
	d, err := md.Delete(ctx, "mtg-ended")
	if err != nil || d.ID != "mtg-ended" || d.State != "ended" || d.CalendarRemoved != 1 {
		t.Fatalf("delete ended = %+v, %v", d, err)
	}
	if _, err := svc.sched.Get(ended.ID); !errors.Is(err, schedule.ErrNotFound) {
		t.Fatalf("calendar entry kept: %v", err)
	}

	if _, err := svc.Delete(ctx, future.ID); !errors.Is(err, dashboard.ErrMeetingConflict) {
		t.Fatalf("delete future entry = %v, want conflict", err)
	}
	cd, err := svc.Delete(ctx, failed.ID)
	if err != nil || len(cd.Meetings) != 1 || cd.Meetings[0].ID != "mtg-cal" {
		t.Fatalf("delete past entry = %+v, %v", cd, err)
	}
	if _, err := mgr.Get("mtg-cal"); !errors.Is(err, meetings.ErrNotFound) {
		t.Fatalf("entry's meeting kept: %v", err)
	}
	if _, err := svc.sched.Get(future.ID); err != nil {
		t.Fatalf("future entry touched: %v", err)
	}
	if _, err := mgr.Get("mtg-live"); err != nil {
		t.Fatalf("live meeting touched: %v", err)
	}
}

package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// TestStageTrackerRecentlyClosed: closed-unmerged PR snapshots are
// remembered once each, newest first, within the merged window; open,
// merged and issue snapshots are not (hivecommons/hive#15808).
func TestStageTrackerRecentlyClosed(t *testing.T) {
	s := &Server{}
	now := time.Now()
	snaps := []github.WebhookItemSnapshot{
		{Repo: "o/r", Number: 15793, IsPR: true, State: "closed", UpdatedAt: now.Add(-time.Minute)},
		{Repo: "o/r", Number: 15805, IsPR: true, State: "closed", Merged: true, UpdatedAt: now},
		{Repo: "o/r", Number: 15791, IsPR: true, State: "open"},
		{Repo: "o/r", Number: 7, IsPR: false, State: "closed"},
		{Repo: "o/r", Number: 15700, IsPR: true, State: "closed", UpdatedAt: now.Add(-stageTrackerMergedWindow - time.Minute)},
		{Repo: "o/r", Number: 15790, IsPR: true, State: "closed", UpdatedAt: now},
	}
	s.noteClosedSnapshots(snaps)
	s.noteClosedSnapshots(snaps[:1])
	got := s.stageTrackerRecentlyClosed(now)
	if len(got) != 2 || got[0].Number != 15790 || got[1].Number != 15793 {
		t.Fatalf("recently closed = %+v, want [15790 15793]", got)
	}
	if got := s.stageTrackerRecentlyClosed(now.Add(stageTrackerMergedWindow + time.Second)); got != nil {
		t.Fatalf("outside the window = %+v, want none", got)
	}
}

// TestStageTrackerMergedHoldKnob: dashboard.stage_tracker.merged_hold_s
// defaults to 8 s, floors at 3 s, and the server reads the config default
// with no config wired.
func TestStageTrackerMergedHoldKnob(t *testing.T) {
	cases := map[int]time.Duration{0: 8 * time.Second, 1: 3 * time.Second, 3: 3 * time.Second, 20: 20 * time.Second}
	for in, want := range cases {
		c := &config.Config{}
		c.Dashboard.StageTracker.MergedHoldS = in
		if got := c.StageTrackerMergedHold(); got != want {
			t.Errorf("merged_hold_s=%d: got %s, want %s", in, got, want)
		}
	}
	if got := (&Server{}).stageTrackerMergedHold(); got != 8*time.Second {
		t.Errorf("server default = %s, want 8s", got)
	}
}

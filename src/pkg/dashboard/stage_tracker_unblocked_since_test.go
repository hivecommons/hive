package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An issue the blocked sweep just unblocked has no stage record yet; its
// row must age from the lift, not from issue creation
// (hivecommons/hive#15779).
func TestStageTrackerNewRowAgesFromUnblock(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	s, _ := stageTrackerFixture(t, now)
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	created := now.Add(-17 * time.Hour)
	lifted := now.Add(-20 * time.Minute)
	s.statusMu.Lock()
	s.status.Repos[0].ActionableIssues = append(s.status.Repos[0].ActionableIssues,
		github.Issue{Number: 15517, Title: "Was blocked", Author: "pat", AuthorIsHuman: true, CreatedAt: created, UpdatedAt: created})
	s.statusMu.Unlock()

	resp := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, resp, "issue", 15517)
	if it.StageSlug != StageTrackerNewSlug || it.StageSince == nil || !it.StageSince.Equal(created) {
		t.Fatalf("before the lift: slug %q since %v, want 000-new since creation %v", it.StageSlug, it.StageSince, created)
	}

	store.NoteUnblocked(stagelabels.Key("acme/console", 15517), lifted)
	resp = s.buildStageTracker(s.status, store, now)
	it = trackerItem(t, resp, "issue", 15517)
	if it.StageSlug != StageTrackerNewSlug || it.Stage != 0 || it.New == nil {
		t.Fatalf("after the lift the row must still be 000-new: stage %d slug %q new %+v", it.Stage, it.StageSlug, it.New)
	}
	if it.StageSince == nil || !it.StageSince.Equal(lifted) {
		t.Fatalf("after the lift since = %v, want the lift %v", it.StageSince, lifted)
	}

	// The stage-label pass that follows may still decide None for the
	// issue (SyncIssueStageLabels → ForgetStage); the lift survives it.
	store.ForgetStage(stagelabels.Key("acme/console", 15517))
	resp = s.buildStageTracker(s.status, store, now)
	it = trackerItem(t, resp, "issue", 15517)
	if it.StageSlug != StageTrackerNewSlug || it.StageSince == nil || !it.StageSince.Equal(lifted) {
		t.Fatalf("after a None pass: slug %q since %v, want 000-new since the lift %v", it.StageSlug, it.StageSince, lifted)
	}

	// The next stage-label pass stages it: since is the stage entry.
	staged := now.Add(-5 * time.Minute)
	clock = staged
	store.Observe(stagelabels.Key("acme/console", 15517), stagelabels.Actionable, "actionable")
	resp = s.buildStageTracker(s.status, store, now)
	it = trackerItem(t, resp, "issue", 15517)
	if it.Stage != stagelabels.Actionable.Number() || it.StageSince == nil || !it.StageSince.Equal(staged) {
		t.Fatalf("after staging: stage %d since %v, want 010 since %v", it.Stage, it.StageSince, staged)
	}
}

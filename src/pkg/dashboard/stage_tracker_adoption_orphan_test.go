package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#15052: an adopted PR whose hive copy was closed unmerged
// reads "Adopted → #N (closed) · recovering" rather than "Needs human
// decision"; after re-adoption it names the fresh copy; the loop guard
// park says plainly why a person must decide.

func orphanTrackerFixture(t *testing.T, now time.Time, stage stagelabels.Stage, reason string, labels []string) (*Server, *stagelabels.Store, *StatusPayload, string) {
	t.Helper()
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	s.deps = &Dependencies{Config: &config.Config{}}
	key := stagelabels.Key("hivecommons/hive", 14947)
	store.Observe(key, stage, reason)
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 14947, Title: "Fix", Author: "thatguywob", HeadSHA: "bc3a6ea", Labels: append([]string{"author/clanker", "role/contributor"}, labels...)}}},
	}}
	s.UpdateStatus(status)
	return s, store, status, key
}

func TestStageTrackerAdoptionOrphanedShowsRecovering(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.AdoptedByHive, "contributor-lane:adopted", []string{"stage/074-adopted-by-hive"})
	store.MarkAdopted(key, "bc3a6ea", 14977, now.Add(-20*time.Minute))
	store.NoteAdoptionOrphaned(key, 14977, now)

	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if it.AdoptedBy != 14977 || it.AdoptedByState != "closed" {
		t.Fatalf("adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
	if !strings.Contains(it.Caption, "copy #14977 was closed without merging") || !strings.Contains(it.Caption, "fresh copy") {
		t.Fatalf("caption = %q", it.Caption)
	}
	raw, _ := json.Marshal(it)
	if !strings.Contains(string(raw), `"adopted_by_state":"closed"`) {
		t.Fatalf("payload lacks adopted_by_state: %s", raw)
	}

	// Re-adopted: the fresh copy is on record and the state clears.
	store.MarkAdopted(key, "bc3a6ea", 15060, now)
	it = trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if it.AdoptedBy != 15060 || it.AdoptedByState != "" {
		t.Fatalf("after re-adopt adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
	if !strings.Contains(it.Caption, "superseded by #15060") {
		t.Fatalf("caption after re-adopt = %q", it.Caption)
	}
}

func TestStageTrackerAdoptionClosedTwiceSaysWhy(t *testing.T) {
	now := time.Date(2026, 10, 28, 16, 0, 0, 0, time.UTC)
	s, store, status, key := orphanTrackerFixture(t, now, stagelabels.NeedsDecision, stagelabels.LaneReasonAdoptionChildClosedTwice, []string{"stage/085-needs-decision"})
	store.MarkAdopted(key, "bc3a6ea", 15060, now.Add(-10*time.Minute))
	store.NoteAdoptionOrphaned(key, 15060, now)
	store.MarkAdoptionEscalated(key, "bc3a6ea", stagelabels.LaneReasonAdoptionChildClosedTwice)

	it := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 14947)
	if it.AdoptedBy != 15060 || it.AdoptedByState != "closed-twice" {
		t.Fatalf("adopted_by=%d state=%q", it.AdoptedBy, it.AdoptedByState)
	}
	if !strings.Contains(it.Caption, "closed without merging twice within an hour") || !strings.Contains(it.Caption, "a person must decide") {
		t.Fatalf("caption = %q", it.Caption)
	}
	if it.DecisionNeeded == nil || !strings.Contains(it.DecisionNeeded.Question, "closed without merging twice within an hour") || !strings.Contains(it.DecisionNeeded.Question, "#15060") {
		t.Fatalf("decision_needed = %+v", it.DecisionNeeded)
	}
	if got := humanizeStageReason(stagelabels.LaneReasonAdoptionChildClosedTwice); !strings.Contains(got, "twice within an hour") {
		t.Fatalf("humanizeStageReason = %q", got)
	}
}

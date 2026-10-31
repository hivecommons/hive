package dashboard

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hubactivity"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestHubActivityItemsMapsTrackerRows(t *testing.T) {
	since := time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)
	rows := []StageTrackerItem{
		{Repo: "acme/widget", Number: 1, Kind: "pr", Title: "Ship it", AuthorKind: "human", Stage: 90, StageSlug: "ready-for-human-merge", StageSince: &since, NeedsHuman: true, Modifiers: []string{"hold"}},
		{Repo: "acme/widget", Number: 2, Kind: "issue", Title: "Decide", NeedsHuman: true, Modifiers: []string{"needs-decision"}},
		{Repo: "acme/secret", Number: 3, Kind: "pr", Title: "Hidden", Modifiers: []string{"stalled"}},
	}
	got := hubActivityItems(rows, publicRepoSet{"acme/widget": true})
	if len(got) != 3 {
		t.Fatalf("mapped %d rows, want 3", len(got))
	}
	first := got[0]
	if first.Repo != "acme/widget" || first.Number != 1 || first.Kind != "pr" || first.Title != "Ship it" || first.AuthorKind != "human" ||
		first.Stage != "ready-for-human-merge" || first.StageNumber != 90 || first.Since == nil || !first.Since.Equal(since) || first.Private {
		t.Fatalf("row 1 = %+v", first)
	}
	if !slices.Equal(first.Mods, []string{hubactivity.ModNeedsHuman, "hold"}) {
		t.Fatalf("a NeedsHuman row's mods = %v, want needs-human first so urgency ordering sees it", first.Mods)
	}
	if !slices.Equal(got[1].Mods, []string{"needs-decision"}) {
		t.Fatalf("row already marked needs-decision got mods %v, want no duplicate marker", got[1].Mods)
	}
	if !got[2].Private || !slices.Equal(got[2].Mods, []string{"stalled"}) {
		t.Fatalf("repo outside the public set = %+v, want private", got[2])
	}
	if !slices.Equal(rows[0].Modifiers, []string{"hold"}) {
		t.Fatalf("mapping modified the tracker row: %v", rows[0].Modifiers)
	}
}

func TestHubActivityItemsReadsTheMemoisedTracker(t *testing.T) {
	t.Setenv("HIVE_PUBLIC_REPOS", "")
	var nilServer *Server
	if items, ok := nilServer.HubActivityItems(context.Background()); ok || items != nil {
		t.Fatal("a nil server must report no tracker")
	}

	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	s := newTestServer()
	s.stageStore = stagelabels.NewStore("", func() time.Time { return now })
	s.deps = &Dependencies{Config: &config.Config{
		Project: config.ProjectConfig{Org: "acme", PrimaryRepo: "acme/widget", Repos: []string{"acme/widget"}},
		Hub:     config.HubConfig{IsPublic: true},
	}}
	builds := 0
	s.stageTrackerBuildFn = func(_ *StatusPayload, _ *stagelabels.Store, at time.Time) StageTrackerResponse {
		builds++
		return StageTrackerResponse{GeneratedAt: at, Items: []StageTrackerItem{
			{Repo: "acme/widget", Number: 7, Kind: "pr", Title: "Public", Stage: 70, StageSlug: "ci", Modifiers: []string{}},
			{Repo: "acme/private", Number: 8, Kind: "issue", Title: "Private", Modifiers: []string{}},
		}}
	}

	if items, ok := s.HubActivityItems(context.Background()); ok || items != nil {
		t.Fatal("a spoke with no status yet must report no tracker")
	}

	s.statusMu.Lock()
	s.status = minimalPayload()
	s.statusMu.Unlock()
	items, ok := s.HubActivityItems(context.Background())
	if !ok || len(items) != 2 {
		t.Fatalf("HubActivityItems = %+v, %v; want both tracker rows", items, ok)
	}
	if items[0].Number != 7 || items[0].Private || items[0].StageNumber != 70 || !items[1].Private {
		t.Fatalf("items = %+v, want the hub.is_public project repo public and the other private", items)
	}
	if _, ok := s.HubActivityItems(context.Background()); !ok || builds != 1 {
		t.Fatalf("second read rebuilt the tracker (%d builds); want the memoised payload", builds)
	}
	s.stageTrackerCacheMu.Lock()
	served := s.stageTrackerServed
	s.stageTrackerCacheMu.Unlock()
	if served {
		t.Fatal("the in-process digest read counted as a tracker client and would add SSE nudges")
	}
}

func TestOnStageTrackerPublishFiresOnInvalidation(t *testing.T) {
	s := newTestServer()
	other := newTestServer()
	fired := 0
	s.OnStageTrackerPublish(func() { fired++ })
	t.Cleanup(func() { s.OnStageTrackerPublish(nil) })

	s.invalidateStageTrackerCache()
	other.invalidateStageTrackerCache()
	if fired != 1 {
		t.Fatalf("hook fired %d times, want once for its own server only", fired)
	}
	s.OnStageTrackerPublish(nil)
	s.invalidateStageTrackerCache()
	if fired != 1 {
		t.Fatal("a removed hook still fired")
	}
	var nilServer *Server
	nilServer.OnStageTrackerPublish(func() {})
}

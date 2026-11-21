package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The stage tracker row of an abandoned contributor PR keeps its stage and
// recorded reason and says how long it has been stuck and on what; an
// unflagged one (setting off or below the threshold) reads as before.
func TestStageTrackerAbandonedRow(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now.Add(-10*24*time.Hour - time.Hour)
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	s.deps = &Dependencies{Config: &config.Config{}}
	for _, n := range []int{41, 42} {
		key := stagelabels.Key("example/testrepo", n)
		store.ObserveHead(key, "feed", stagelabels.NeedsAuthor, "check-failing:unit")
		store.NoteActivity(key, stagelabels.Activity{Head: "feed", Comments: 1, Reviews: 0})
	}
	clock = now
	store.MarkAbandoned(stagelabels.Key("example/testrepo", 41), now)

	status := minimalPayload()
	status.HiveID = "hive"
	pr := func(n int) any {
		return FrontendPR{PullRequest: github.PullRequest{Number: n, Title: "contributor change", Author: "contributor", HeadSHA: "feed", Labels: []string{"stage/071-needs-author", "author/human", "role/contributor"}}}
	}
	status.Repos = []FrontendRepo{{Name: "testrepo", Full: "example/testrepo", OpenPrs: []any{pr(41), pr(42)}}}
	s.UpdateStatus(status)

	resp := s.buildStageTracker(status, store, now)
	flagged := trackerItem(t, resp, "pr", 41)
	if flagged.Abandoned != "abandoned 10d: check-failing:unit" {
		t.Fatalf("abandoned = %q", flagged.Abandoned)
	}
	if flagged.Stage != 71 || flagged.Reason != "check-failing:unit" {
		t.Fatalf("stage %d reason %q: the flag must not move the stage or rewrite the reason", flagged.Stage, flagged.Reason)
	}
	if !strings.Contains(flagged.Caption, "Stale: close or take over") || !strings.Contains(flagged.Caption, "abandoned 10d: check-failing:unit") {
		t.Fatalf("caption = %q", flagged.Caption)
	}
	quiet := trackerItem(t, resp, "pr", 42)
	if quiet.Abandoned != "" || strings.Contains(quiet.Caption, "Stale") {
		t.Fatalf("unflagged row changed: abandoned=%q caption=%q", quiet.Abandoned, quiet.Caption)
	}
}

// A trusted contributor's PR flagged on abandon_after_trusted says so on
// its row, its caption and its inbox reason.
func TestStageTrackerAbandonedRowNamesTheTrustedTimer(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now.Add(-15*24*time.Hour - time.Hour)
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	s.deps = &Dependencies{Config: &config.Config{}}
	key := stagelabels.Key("example/testrepo", 43)
	store.ObserveHead(key, "feed", stagelabels.NeedsAuthor, "check-failing:unit")
	store.NoteActivity(key, stagelabels.Activity{Head: "feed", Comments: 1, Reviews: 0})
	store.MarkAbandonTrustedSeen(key)
	clock = now
	store.MarkAbandoned(key, now)

	status := minimalPayload()
	status.HiveID = "hive"
	pr := FrontendPR{PullRequest: github.PullRequest{Number: 43, Title: "trusted change", Author: "trusted-contributor", HeadSHA: "feed", Labels: []string{"stage/071-needs-author", "author/human", "role/trusted"}}}
	status.Repos = []FrontendRepo{{Name: "testrepo", Full: "example/testrepo", OpenPrs: []any{pr}}}
	s.UpdateStatus(status)

	const want = "abandoned 15d: check-failing:unit (trusted contributor timer)"
	row := trackerItem(t, s.buildStageTracker(status, store, now), "pr", 43)
	if row.Abandoned != want || !strings.Contains(row.Caption, want) {
		t.Fatalf("abandoned = %q caption = %q", row.Abandoned, row.Caption)
	}
	rec, _ := store.Get(key)
	if got := abandonedOwnershipText(abandonedRowReason(rec, now)); got != "Stale: close or take over — "+want {
		t.Fatalf("inbox reason = %q", got)
	}
	// The delegate's abandoned-stale rule reads the same reason.
	in := delegatedecision.Input{Repo: "example/testrepo", Number: 43, Kind: delegatedecision.KindPR}
	s.gatherDelegatedRecordedPark("example/testrepo", 43, "feed", &in)
	if !in.Abandoned || !strings.HasSuffix(in.AbandonedReason, ": check-failing:unit (trusted contributor timer)") {
		t.Fatalf("delegate abandoned=%v reason=%q", in.Abandoned, in.AbandonedReason)
	}
	// Seen as a maintainer later: the row no longer names the trusted timer.
	store.MarkAbandonMaintainerSeen(key)
	rec, _ = store.Get(key)
	if got := abandonedRowReason(rec, now); strings.Contains(got, "trusted") {
		t.Fatalf("maintainer row = %q", got)
	}
}

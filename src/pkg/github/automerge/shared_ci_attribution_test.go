package automerge

import (
	"context"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestBotSquashAttributionMatchesRequiredCheck(t *testing.T) {
	for _, tc := range []struct {
		author   string
		message  string
		unsafe   int
		unsigned int
	}{
		{"dependabot[bot]", "repair\n\nSigned-off-by: Hive <hive@users.noreply.github.com>", 0, 0},
		{"renovate[bot]", "repair\n\nSigned-off-by: Hive <hive@users.noreply.github.com>", 0, 0},
		{"person", "repair\n\nSigned-off-by: Hive <hive@users.noreply.github.com>", 1, 0},
		{"person-bot", "repair\n\nSigned-off-by: Hive <hive@users.noreply.github.com>", 1, 0},
		{"dependabot[bot]", "unsigned repair", 0, 1},
	} {
		t.Run(tc.author+tc.message, func(t *testing.T) {
			commits := []*gh.RepositoryCommit{{
				Author: &gh.User{Login: gh.Ptr("hive[bot]")},
				Commit: &gh.Commit{Message: gh.Ptr(tc.message)},
			}}
			got := dcoCommitStatsDetailedForAuthor(commits, "hive[bot]", tc.author)
			if got.total != 1 || got.squashUnsafeHive != tc.unsafe || got.unsigned != tc.unsigned {
				t.Fatalf("stats = %+v, want unsafe=%d unsigned=%d", got, tc.unsafe, tc.unsigned)
			}
		})
	}
}

func TestStageDCONormalizesCachedBotAttributionWithoutChangingSignoffs(t *testing.T) {
	api := &stageAPI{labels: map[string]string{}}
	srv := newStageAPI(t, api)
	defer srv.Close()
	e, _, store := newStageEngine(t, srv.URL, &contributorLaneClock{at: time.Now()})
	key := stageKey("acme", "widget", 42)
	store.Observe(key, stagelabels.Fixing, stagelabels.LaneReasonHiveSignoffAttribution)
	store.NoteDCO(key, "aaa", 3, 0, 0, 1, true)
	got, err := e.stageDCO(context.Background(), store, key, stageObservation{
		Owner: "acme", Repo: "widget", Number: 42, HeadSHA: "aaa", Author: "dependabot[bot]",
	}, StageLabelPolicy{})
	if err != nil || got.total != 3 || got.unsigned != 0 || got.squashUnsafeHive != 0 {
		t.Fatalf("cached bot attribution: %+v, %v", got, err)
	}
}

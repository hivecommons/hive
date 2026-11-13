package automerge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Regression for hivecommons/hive#17148: a repository GitHub refuses to
// search for an author is re-asked once per contributorUnsearchableTTL, not
// on every sweep, the searchable repositories keep counting meanwhile, and
// the refusal warns once per process instead of on every PR evaluation.
func TestContributorMergedReposRemembersUnsearchableRepository(t *testing.T) {
	fx := validContributorFixture()
	fx.otherHistory = map[string][]map[string]any{"hive": {mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change")}}
	fx.unsearchable = map[string]bool{"acme/docs": true}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	var logs bytes.Buffer
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngineRepos(api.URL, []string{"widget", "hive", "docs", "homebrew-hive"}, defaultContributorLanePolicy(), clock, Options{
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	ask := func(step string, wantQueries ...string) {
		t.Helper()
		before := len(fx.searchQueries)
		var got []string
		for _, ref := range engine.contributorMergedRepos(context.Background(), "acme", "widget", contributorLaneTestAuthor) {
			got = append(got, ref.Owner+"/"+ref.Repo)
		}
		if strings.Join(got, ",") != "acme/hive" {
			t.Fatalf("%s: repos = %v, want [acme/hive]", step, got)
		}
		base := "is:pr is:merged author:" + contributorLaneTestAuthor
		var want []string
		for _, repos := range wantQueries {
			want = append(want, base+repos)
		}
		if gotQueries := fx.searchQueries[before:]; strings.Join(gotQueries, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s: queries =\n%s\nwant\n%s", step, strings.Join(gotQueries, "\n"), strings.Join(want, "\n"))
		}
	}

	// The PR's own repository is searched too: the per-author answer is
	// cached for every active repository (#15088) and filtered on read.
	ask("first refusal isolates the repository",
		" repo:acme/widget repo:acme/hive repo:acme/docs repo:acme/homebrew-hive",
		" repo:acme/widget",
		" repo:acme/hive",
		" repo:acme/docs",
		" repo:acme/homebrew-hive",
	)
	// The refused pair drops out of the candidate set, which changes the
	// cached answer's repository list, so the next sweep asks again without
	// it; the sweep after that is served from the per-author cache.
	ask("next sweep leaves the refused pair out", " repo:acme/widget repo:acme/hive repo:acme/homebrew-hive")
	clock.at = clock.at.Add(contributorUnsearchableTTL - time.Minute)
	ask("still inside the TTL")
	clock.at = clock.at.Add(time.Minute)
	ask("TTL expiry re-asks GitHub",
		" repo:acme/widget repo:acme/hive repo:acme/docs repo:acme/homebrew-hive",
		" repo:acme/widget",
		" repo:acme/hive",
		" repo:acme/docs",
		" repo:acme/homebrew-hive",
	)

	out := logs.String()
	if n := strings.Count(out, contributorUnsearchableMessage); n != 1 {
		t.Fatalf("unsearchable warning logged %d times, want 1:\n%s", n, out)
	}
	if strings.Contains(out, "repo search failed") {
		t.Fatalf("a refused repository must not degrade the whole search:\n%s", out)
	}
	if !strings.Contains(out, "searched_repo=acme/docs") {
		t.Fatalf("warning should name the refused repository:\n%s", out)
	}
}

// Regression for hivecommons/hive#18059: one author refused for several
// repositories, and several authors refused for the same repository, warn
// once in total instead of once per (author, repository) pair — the pair
// product flooded WARN (15 in 10m on one hive) and self-reported as an
// unclassified repeated error. Every refusal is still remembered per pair.
func TestContributorUnsearchableWarnsOncePerAuthorAndRepository(t *testing.T) {
	fx := validContributorFixture()
	fx.unsearchable = map[string]bool{"acme/docs": true, "acme/homebrew-hive": true}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	var logs bytes.Buffer
	clock := &contributorLaneClock{at: time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngineRepos(api.URL, []string{"widget", "hive", "docs", "homebrew-hive"}, defaultContributorLanePolicy(), clock, Options{
		Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	authors := []string{contributorLaneTestAuthor, "thatguywob", "another-dev"}
	for _, author := range authors {
		engine.contributorMergedRepos(context.Background(), "acme", "widget", author)
	}

	out := logs.String()
	if n := strings.Count(out, contributorUnsearchableMessage); n != 1 {
		t.Fatalf("unsearchable warning logged %d times for %d authors × 2 refused repositories, want 1:\n%s", n, len(authors), out)
	}
	for _, author := range authors {
		for _, refused := range []string{"docs", "homebrew-hive"} {
			if !engine.contributorSearchSkips(author, contributorRepoRef{Owner: "acme", Repo: refused}) {
				t.Fatalf("refusal of acme/%s for %s was not remembered", refused, author)
			}
		}
	}
}

// The pair TTL map and the WARN-dedup map are each bounded at 4096 entries:
// a map holding exactly 4096 is kept, and only one past the cap is reset, so
// the cap is the documented size and not one entry fewer.
func TestNoteContributorUnsearchableResetsOnlyPastCap(t *testing.T) {
	const capacity = 4096
	engine := &Engine{
		contributorUnsearchable:       map[string]time.Time{},
		contributorUnsearchableWarned: map[string]bool{},
	}
	for i := 0; i < capacity; i++ {
		engine.contributorUnsearchable[fmt.Sprintf("filler-%d", i)] = time.Time{}
		engine.contributorUnsearchableWarned[fmt.Sprintf("filler-%d", i)] = true
	}
	refusal := errors.New("422")

	engine.noteContributorUnsearchable("acme", "widget", "first-dev", contributorRepoRef{Owner: "acme", Repo: "docs"}, refusal)
	if got := len(engine.contributorUnsearchable); got != capacity+1 {
		t.Fatalf("pair map at the cap: len = %d, want %d (kept, new pair added)", got, capacity+1)
	}
	if got := len(engine.contributorUnsearchableWarned); got != capacity+2 {
		t.Fatalf("warned map at the cap: len = %d, want %d (kept, author and repo added)", got, capacity+2)
	}

	engine.noteContributorUnsearchable("acme", "widget", "second-dev", contributorRepoRef{Owner: "acme", Repo: "wiki"}, refusal)
	if got := len(engine.contributorUnsearchable); got != 1 {
		t.Fatalf("pair map past the cap: len = %d, want 1 (reset, new pair added)", got)
	}
	if got := len(engine.contributorUnsearchableWarned); got != 2 {
		t.Fatalf("warned map past the cap: len = %d, want 2 (reset, author and repo added)", got)
	}
}

package main

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// mixedHiveConfig is a Forgejo hive with one repo (widgets) on Forgejo and one
// (gh-repo) moved to GitHub by its repo policy (hivecommons/hive#19703).
func mixedHiveConfig(url string) *config.Config {
	cfg := forgejoOnlyConfig(url)
	cfg.Project.Repos = []string{"widgets", "gh-repo"}
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "gh-repo", Forge: config.ForgeGitHub}}
	return cfg
}

// githubShareOfMixedHive is what the GitHub client reports for the mixed
// hive: real work on gh-repo, and a bogus share for widgets (a same-named
// GitHub repo, or a transient failure marked stale) that must be dropped.
func githubShareOfMixedHive(now time.Time) *github.ActionableResult {
	issues := []github.Issue{
		{Repo: "gh-repo", Number: 7, Title: "GitHub issue"},
		{Repo: "widgets", Number: 900, Title: "same-named GitHub repo issue"},
	}
	prs := []github.PullRequest{
		{Repo: "widgets", Number: 901, Title: "same-named GitHub repo PR"},
		{Repo: "gh-repo", Number: 8, Title: "fix: GitHub PR"},
	}
	return &github.ActionableResult{
		GeneratedAt: now,
		Issues:      github.IssueResultFromItems(issues),
		PRs: github.PRResult{
			Count:       len(prs),
			Items:       prs,
			Held:        []github.PullRequest{{Repo: "widgets", Number: 902}},
			StaleDrafts: []github.PullRequest{{Repo: "widgets", Number: 903}},
			Attributed:  []github.PullRequest{{Repo: "widgets", Number: 904}, {Repo: "gh-repo", Number: 8}},
		},
		Hold: github.HoldResult{Issues: 1, Total: 1, Items: []github.HoldItem{{Repo: "widgets", Number: 905, Type: "issue"}}},
		TotalByRepo: map[string]github.RepoCounts{
			"gh-repo": {Issues: 1, PRs: 1},
			"widgets": {Issues: 99, PRs: 99},
		},
		WorkDetailsByRepo: map[string]github.RepoWorkDetails{"widgets": {}, "gh-repo": {}},
		ItemPatches: map[string]github.ItemPatchMeta{
			github.ItemPatchKey("widgets", 900, false): {},
			github.ItemPatchKey("gh-repo", 7, false):   {},
		},
		StaleRepos: []string{"widgets"},
		StaleSince: now.Add(-time.Hour),
	}
}

func TestHiveMixesForges(t *testing.T) {
	ghOnly := &config.Config{}
	ghOnly.Project.Repos = []string{"a", "b"}

	ghWithForgeRepo := &config.Config{}
	ghWithForgeRepo.Project.Repos = []string{"a", "b"}
	ghWithForgeRepo.Project.RepoPolicies = []config.RepoPolicy{{Repo: "b", Forge: config.ForgeGitea}}

	cases := map[string]struct {
		cfg  *config.Config
		want bool
	}{
		"nil":                       {nil, false},
		"github only":               {ghOnly, false},
		"forge only":                {forgejoOnlyConfig("https://fj.example"), false},
		"forge hive with a GH repo": {mixedHiveConfig("https://fj.example"), true},
		"GH hive with a forge repo": {ghWithForgeRepo, true},
	}
	for name, tc := range cases {
		if got := hiveMixesForges(tc.cfg); got != tc.want {
			t.Errorf("%s: hiveMixesForges = %v, want %v", name, got, tc.want)
		}
	}
}

func TestMergeForgeIntoGitHubResultServesForgeRepo(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	got := mergeForgeIntoGitHubResult(context.Background(), mixedHiveConfig(srv.URL), githubShareOfMixedHive(time.Now()), discardLogger())

	issueKeys := map[string]bool{}
	for _, is := range got.Issues.Items {
		issueKeys[github.ItemPatchKey(is.Repo, is.Number, false)] = true
	}
	for _, want := range []string{"gh-repo#7:issue", "widgets#1:issue"} {
		if !issueKeys[want] {
			t.Errorf("issues = %v, missing %s", issueKeys, want)
		}
	}
	if issueKeys["widgets#900:issue"] || got.Issues.Count != 2 {
		t.Errorf("issues = %v (count %d), want only gh-repo#7 and the forge's widgets#1", issueKeys, got.Issues.Count)
	}

	prKeys := map[string]bool{}
	for _, pr := range got.PRs.Items {
		prKeys[github.ItemPatchKey(pr.Repo, pr.Number, true)] = true
	}
	if !prKeys["gh-repo#8:pr"] || !prKeys["widgets#10:pr"] || len(prKeys) != 2 || got.PRs.Count != 2 {
		t.Errorf("prs = %v (count %d), want gh-repo#8 and the forge's widgets#10", prKeys, got.PRs.Count)
	}
	if len(got.PRs.Held) != 1 || got.PRs.Held[0].Number != 12 {
		t.Errorf("held prs = %+v, want only the forge's widgets#12", got.PRs.Held)
	}
	if len(got.PRs.StaleDrafts) != 0 {
		t.Errorf("stale drafts = %+v, want the GitHub share of widgets dropped", got.PRs.StaleDrafts)
	}
	if len(got.PRs.Attributed) != 1 || got.PRs.Attributed[0].Repo != "gh-repo" {
		t.Errorf("attributed = %+v, want only gh-repo#8", got.PRs.Attributed)
	}
	if got.Hold.Total != 2 || got.Hold.Issues != 1 || got.Hold.PRs != 1 {
		t.Errorf("hold = %+v, want the forge's issue #2 and PR #12 only", got.Hold)
	}
	for _, h := range got.Hold.Items {
		if h.Number == 905 {
			t.Errorf("hold kept the GitHub share of widgets: %+v", h)
		}
	}
	if c := got.TotalByRepo["widgets"]; c.Issues != 4 || c.PRs != 3 {
		t.Errorf("TotalByRepo[widgets] = %+v, want the forge's 4 issues and 3 PRs", c)
	}
	if c := got.TotalByRepo["gh-repo"]; c.Issues != 1 || c.PRs != 1 {
		t.Errorf("TotalByRepo[gh-repo] = %+v, want the GitHub counts kept", c)
	}
	if _, ok := got.WorkDetailsByRepo["widgets"]; ok {
		t.Error("WorkDetailsByRepo kept the GitHub share of widgets")
	}
	if _, ok := got.ItemPatches[github.ItemPatchKey("widgets", 900, false)]; ok {
		t.Error("ItemPatches kept the GitHub share of widgets")
	}
	if _, ok := got.ItemPatches[github.ItemPatchKey("gh-repo", 7, false)]; !ok {
		t.Error("ItemPatches dropped a GitHub repo's patch")
	}
	if got.Stale() || !got.StaleSince.IsZero() {
		t.Errorf("result stale (%v since %v); the only stale repo was the dropped GitHub share", got.StaleRepos, got.StaleSince)
	}
	for _, p := range fake.paths {
		if p != "/api/v1/repos/acme/widgets/issues" && p != "/api/v1/repos/acme/widgets/pulls" {
			t.Errorf("forge was asked about %s; only widgets lives there", p)
		}
	}
}

func TestMergeForgeIntoGitHubResultForgeFailureKeepsGitHubShare(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{fail: true}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	got := mergeForgeIntoGitHubResult(context.Background(), mixedHiveConfig(srv.URL), githubShareOfMixedHive(time.Now()), discardLogger())
	if got == nil {
		t.Fatal("a failed forge listing discarded the GitHub share of the pass")
	}
	if got.Issues.Count != 1 || got.Issues.Items[0].Repo != "gh-repo" {
		t.Errorf("issues = %+v, want only gh-repo#7", got.Issues.Items)
	}
	if !got.RepoStale("widgets") || got.RepoStale("gh-repo") || got.StaleSince.IsZero() {
		t.Errorf("stale = %v since %v, want only widgets", got.StaleRepos, got.StaleSince)
	}
	if c, ok := got.TotalByRepo["widgets"]; !ok || c != (github.RepoCounts{}) {
		t.Errorf("TotalByRepo[widgets] = %+v (present %v), want a zero entry", c, ok)
	}
}

func TestMergeForgeIntoGitHubResultSkipsPausedForgeRepo(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	cfg := mixedHiveConfig(srv.URL)
	cfg.Project.PausedRepos = []config.RepoPause{{Repo: "widgets"}}

	got := mergeForgeIntoGitHubResult(context.Background(), cfg, githubShareOfMixedHive(time.Now()), discardLogger())
	if len(fake.paths) != 0 {
		t.Errorf("paused forge repo was listed: %v", fake.paths)
	}
	if _, ok := got.TotalByRepo["widgets"]; ok {
		t.Error("paused forge repo has a TotalByRepo entry")
	}
	if got.Issues.Count != 1 {
		t.Errorf("issues = %d, want gh-repo's 1", got.Issues.Count)
	}
}

func TestEnrichGitHubPRsSkipsForgeRepoOnMixedHive(t *testing.T) {
	prs := []github.PullRequest{
		{Repo: "gh-repo", Number: 1},
		{Repo: "widgets", Number: 2},
		{Repo: "gh-repo", Number: 3},
	}
	var seen []string
	enrichGitHubPRs(mixedHiveConfig("https://fj.example"), prs, func(sub []github.PullRequest) {
		for i := range sub {
			seen = append(seen, sub[i].Repo)
			sub[i].CIStatus = "success"
		}
	})
	if len(seen) != 2 || seen[0] != "gh-repo" || seen[1] != "gh-repo" {
		t.Errorf("enriched repos = %v, want only the two gh-repo PRs", seen)
	}
	if prs[0].CIStatus != "success" || prs[2].CIStatus != "success" {
		t.Errorf("GitHub PRs not written back: %+v", prs)
	}
	if prs[1].CIStatus != "" {
		t.Errorf("forge PR got GitHub CI status %q", prs[1].CIStatus)
	}
}

// TestEnrichGitHubPRsHandsGitHubHiveTheSameSlice pins that a GitHub-only hive
// enriches its own slice, not a copy.
func TestEnrichGitHubPRsHandsGitHubHiveTheSameSlice(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Repos = []string{"a"}
	prs := []github.PullRequest{{Repo: "a", Number: 1}, {Repo: "a", Number: 2}}
	called := false
	enrichGitHubPRs(cfg, prs, func(sub []github.PullRequest) {
		called = true
		if len(sub) != len(prs) || &sub[0] != &prs[0] {
			t.Errorf("GitHub-only hive was handed a copy, not its slice")
		}
	})
	if !called {
		t.Error("enrich not called on a GitHub-only hive")
	}
}

// TestGitHubPRViewKeepsOnlyGitHubPRsOnMixedHive pins hivecommons/hive#19709:
// the GitHub-client steps of the eval cycle (review threads, PR follow-ups,
// the hold guard's branch upkeep) see only the GitHub share of a mixed hive,
// and the result they were derived from keeps its forge PRs.
func TestGitHubPRViewKeepsOnlyGitHubPRsOnMixedHive(t *testing.T) {
	cfg := mixedHiveConfig("https://fj.example")
	prs := []github.PullRequest{{Repo: "widgets", Number: 1}, {Repo: "gh-repo", Number: 2}}
	result := &github.ActionableResult{
		PRs: github.PRResult{
			Count:       len(prs),
			Items:       prs,
			Held:        []github.PullRequest{{Repo: "widgets", Number: 3}, {Repo: "gh-repo", Number: 4}},
			Attributed:  []github.PullRequest{{Repo: "widgets", Number: 5}},
			StaleDrafts: []github.PullRequest{{Repo: "gh-repo", Number: 6}, {Repo: "widgets", Number: 7}},
		},
	}
	setHoldItems(result, []github.HoldItem{
		{Repo: "widgets", Number: 3, Type: "pr"},
		{Repo: "gh-repo", Number: 4, Type: "pr"},
		{Repo: "widgets", Number: 9, Type: "issue"},
	})

	view := githubPRView(cfg, result)
	if view == result {
		t.Fatal("mixed hive got the result itself, not a GitHub-only view")
	}
	for name, list := range map[string][]github.PullRequest{
		"items": view.PRs.Items, "held": view.PRs.Held, "attributed": view.PRs.Attributed, "stale drafts": view.PRs.StaleDrafts,
	} {
		for _, pr := range list {
			if pr.Repo != "gh-repo" {
				t.Errorf("%s: forge PR %s#%d reached the GitHub view", name, pr.Repo, pr.Number)
			}
		}
	}
	if view.PRs.Count != 1 || len(view.PRs.Held) != 1 || len(view.PRs.StaleDrafts) != 1 {
		t.Errorf("view PRs = %+v, want one GitHub PR in items, held and stale drafts", view.PRs)
	}
	if view.Hold.Total != 1 || view.Hold.Items[0].Repo != "gh-repo" {
		t.Errorf("view hold = %+v, want only gh-repo's held PR", view.Hold)
	}
	if result.PRs.Count != 2 || len(result.PRs.Held) != 2 || result.Hold.Total != 3 {
		t.Errorf("source result was changed: prs=%+v hold=%+v", result.PRs, result.Hold)
	}

	if githubClientForRepo(cfg, &github.Client{}, "widgets") != nil {
		t.Error("forge repo got a GitHub client for per-PR reads")
	}
	if githubClientForRepo(cfg, &github.Client{}, "gh-repo") == nil {
		t.Error("GitHub repo lost its GitHub client")
	}
}

// TestGitHubPRViewIsTheResultOnGitHubHive pins that a GitHub-only hive is
// untouched: every step keeps acting on the result itself.
func TestGitHubPRViewIsTheResultOnGitHubHive(t *testing.T) {
	cfg := &config.Config{}
	cfg.Project.Repos = []string{"a"}
	result := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{{Repo: "a", Number: 1}}}}
	if githubPRView(cfg, result) != result {
		t.Error("GitHub-only hive was handed a copy")
	}
	client := &github.Client{}
	if githubClientForRepo(cfg, client, "a") != client {
		t.Error("GitHub-only hive lost its client")
	}
}

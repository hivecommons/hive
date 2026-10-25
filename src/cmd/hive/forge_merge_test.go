package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// fakeMerger records Merge calls in place of a real GitLab/Gitea adapter.
type fakeMerger struct {
	kind  forge.Kind
	calls []fakeMergeCall
	err   error
}

type fakeMergeCall struct {
	repo   string
	number int
	opts   forge.MergeOptions
}

func (f *fakeMerger) Merge(_ context.Context, repo string, number int, opts forge.MergeOptions) error {
	f.calls = append(f.calls, fakeMergeCall{repo: repo, number: number, opts: opts})
	return f.err
}

// withFakeForgeMergers swaps newForgeMerger for one handing out a fake per
// forge kind, and returns the fakes.
func withFakeForgeMergers(t *testing.T) map[forge.Kind]*fakeMerger {
	t.Helper()
	fakes := map[forge.Kind]*fakeMerger{
		forge.KindGitLab: {kind: forge.KindGitLab},
		forge.KindGitea:  {kind: forge.KindGitea},
	}
	orig := newForgeMerger
	newForgeMerger = func(kind forge.Kind, _ string, _ forge.Options) (forge.Merger, error) {
		f, ok := fakes[kind]
		if !ok {
			t.Fatalf("newForgeMerger called for unexpected kind %q", kind)
		}
		return f, nil
	}
	t.Cleanup(func() { newForgeMerger = orig })
	return fakes
}

func mixedForgeConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Repos = []string{"app", "lab", "tea"}
	cfg.Project.RepoPolicies = []config.RepoPolicy{
		{Repo: "lab", Forge: config.ForgeGitLab, ForgeURL: "https://gitlab.example.com"},
		{Repo: "tea", Forge: config.ForgeGitea, ForgeURL: "https://codeberg.org"},
	}
	return cfg
}

// A GitHub repo (explicit, defaulted, or an unknown kind that falls back to
// GitHub) gets no forge merger, so the relay keeps its GitHub path unchanged.
func TestForgeMergeSelectorKeepsGitHubOnGitHubPath(t *testing.T) {
	withFakeForgeMergers(t)
	cfg := mixedForgeConfig()
	sel := forgeMergeSelector(func() *config.Config { return cfg }, discardLogger())
	if sel("acme/app") != nil {
		t.Fatal("GitHub repo must stay on the relay's GitHub path (nil forge merge)")
	}
	if sel("lab") == nil || sel("acme/tea") == nil {
		t.Fatal("GitLab/Gitea repos must route through a forge merger")
	}
	if forgeMergeSelector(func() *config.Config { return nil }, discardLogger())("acme/tea") != nil {
		t.Fatal("nil config must keep the GitHub path")
	}
	if m, err := forgeMergerForRepo(cfg, "acme/app"); m != nil || err != nil {
		t.Fatalf("forgeMergerForRepo(GitHub) = (%v, %v), want (nil, nil)", m, err)
	}
}

// End-to-end through merge-eligibility on a non-GitHub repo: the governor
// writes the Gitea PR to merge-eligible.json, the relay's target binding
// authorizes it only at the reviewed head, and the merge lands through the
// Gitea forge.Merger (never the GitHub client) pinned to that head.
func TestForgeMergeThroughMergeEligibilityOnGitea(t *testing.T) {
	dir := t.TempDir()
	origMerge, origFail := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() {
		mergeEligiblePath = origMerge
		ciFailingPath = origFail
	})
	fakes := withFakeForgeMergers(t)
	cfg := mixedForgeConfig()

	const head = "c0ffee1234"
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{
		{Repo: "acme/tea", Number: 7, Title: "green", Author: "agent", CIStatus: "success", Mergeable: github.MergeableYes, HeadSHA: head},
		{Repo: "acme/tea", Number: 8, Title: "red", Author: "agent", CIStatus: "failure", FailingChecks: []string{"test"}, HeadSHA: "bad"},
	}}}
	writeMergeEligible(actionable, github.HoldResult{}, "acme", nil, false, nil, false, nil, nil, nil, discardLogger())

	authz := bindMergeAuthz(func(string, int, string) error { return nil })
	sel := forgeMergeSelector(func() *config.Config { return cfg }, discardLogger())
	ctx := context.Background()

	// relay mirrors the watcher's order: authorize (merge-eligible binding),
	// then route to the repo's forge.
	relay := func(repo string, number int, sha string) error {
		if err := authz("scanner", 0, repo, number, sha); err != nil {
			return err
		}
		merge := sel(repo)
		if merge == nil {
			t.Fatalf("%s routed to the GitHub path", repo)
		}
		return merge(ctx, repo, number, "squash", sha)
	}

	if err := relay("acme/tea", 8, "bad"); err == nil {
		t.Fatal("a ci-failing PR must not be authorized")
	}
	if err := relay("acme/tea", 7, "moved"); err == nil {
		t.Fatal("an eligible PR at a moved head must not be authorized")
	}
	if len(fakes[forge.KindGitea].calls) != 0 {
		t.Fatalf("refused targets reached the forge: %+v", fakes[forge.KindGitea].calls)
	}

	if err := relay("acme/tea", 7, head); err != nil {
		t.Fatalf("eligible Gitea PR at the reviewed head: %v", err)
	}
	calls := fakes[forge.KindGitea].calls
	if len(calls) != 1 {
		t.Fatalf("Gitea merges = %d, want 1", len(calls))
	}
	if c := calls[0]; c.repo != "acme/tea" || c.number != 7 || c.opts.HeadSHA != head || c.opts.Strategy != string(forge.MergeStrategySquash) {
		t.Fatalf("merge call = %+v, want acme/tea#7 squash pinned at %s", c, head)
	}
	if len(fakes[forge.KindGitLab].calls) != 0 {
		t.Fatal("a Gitea repo must not merge through the GitLab adapter")
	}
}

// A retried request whose first attempt already landed is success; any other
// forge refusal is surfaced to the relay's retry path.
func TestForgeMergeErrors(t *testing.T) {
	fakes := withFakeForgeMergers(t)
	cfg := mixedForgeConfig()
	merge := forgeMergeSelector(func() *config.Config { return cfg }, discardLogger())("acme/lab")

	fakes[forge.KindGitLab].err = forge.ErrAlreadyMerged
	if err := merge(context.Background(), "acme/lab", 3, "squash", "abc"); err != nil {
		t.Fatalf("already merged should be success, got %v", err)
	}
	fakes[forge.KindGitLab].err = forge.ErrConflict
	if err := merge(context.Background(), "acme/lab", 3, "squash", "abc"); !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}

	newForgeMerger = func(forge.Kind, string, forge.Options) (forge.Merger, error) {
		return nil, errors.New("no url")
	}
	if err := merge(context.Background(), "acme/lab", 3, "squash", "abc"); err == nil {
		t.Fatal("an unbuildable adapter must fail closed, not fall back to GitHub")
	}
}

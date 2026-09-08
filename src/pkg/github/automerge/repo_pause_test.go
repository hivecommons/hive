package automerge

import (
	"log/slog"
	"testing"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// The v4 sweep lived in package github and called the client's own
// activeRepos() directly, so the pause gate could not come unwired. On v5 the
// sweep is its own package behind the Transport interface and reaches the
// narrowed list through an OPTIONAL capability — which fails silently if it
// ever stops matching: the sweep would quietly go back to merging into paused
// repos with every existing test still green. These two tests pin both halves
// of that seam (#6203).

// The production transport is *github.Client. If it stops satisfying the
// capability — renamed, moved, or wrapped at the call site — the operator's
// pause stops reaching the automerge sweep.
func TestEngineActiveRepos_HonorsClientPause(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget", "gadget", "doodad"}, slog.New(slog.DiscardHandler), "https://api.github.com/")
	if _, ok := any(client).(interface{ ActiveRepositories() []string }); !ok {
		t.Fatal("*github.Client no longer satisfies the ActiveRepositories capability the sweep probes for")
	}
	client.SetRepoPausedFunc(func(repo string) bool { return repo == "gadget" })

	got := New(client, Options{}).activeRepos()
	if len(got) != 2 || got[0] != "widget" || got[1] != "doodad" {
		t.Fatalf("activeRepos() = %v, want the paused repo omitted", got)
	}
}

// pauseBlindTransport is the minimum Transport, with no pause capability —
// the shape every fake in this package has. It must keep its full repo list
// rather than silently sweeping nothing.
type pauseBlindTransport struct{ repos []string }

func (p pauseBlindTransport) GoGitHub() *gh.Client                            { return gh.NewClient(nil) }
func (p pauseBlindTransport) Repositories() []string                          { return p.repos }
func (p pauseBlindTransport) SplitRepo(repo string) (string, string)          { return "acme", repo }
func (p pauseBlindTransport) AutoMergeLabel() string                          { return "auto-merge" }
func (p pauseBlindTransport) AppBotLogin() string                             { return testHiveAppBotLogin }
func (p pauseBlindTransport) IsExemptLabels([]string) bool                    { return false }
func (p pauseBlindTransport) RecordPRMergedAudit(string, int, string, string) {}

func TestEngineActiveRepos_FallsBackWithoutCapability(t *testing.T) {
	transport := pauseBlindTransport{repos: []string{"widget", "gadget"}}

	got := New(transport, Options{}).activeRepos()
	if len(got) != 2 || got[0] != "widget" || got[1] != "gadget" {
		t.Fatalf("activeRepos() = %v, want the full list for a pause-blind transport", got)
	}
}

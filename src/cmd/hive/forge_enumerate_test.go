package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

const (
	forgejoTestTokenEnv = "HIVE_TEST_FORGEJO_ENUM_TOKEN"
	forgejoTestToken    = "fj-enum-token"
)

// fakeForgejoIssuesJSON is a Forgejo /issues page: one plain issue, one held,
// one exempt, one gated by the issue filter, and a PR stub the adapter drops.
const fakeForgejoIssuesJSON = `[
	{"number": 1, "title": "Fix the widget", "body": "it breaks", "state": "open",
	 "labels": [{"name": "kind/bug"}, {"name": "hive-ok"}],
	 "html_url": "https://fj.example/acme/widgets/issues/1",
	 "created_at": "2026-11-01T12:00:00Z", "updated_at": "2026-11-02T12:00:00Z",
	 "user": {"login": "alice"}, "assignees": [{"login": "bob"}]},
	{"number": 2, "title": "Parked", "state": "open",
	 "labels": [{"name": "hold"}, {"name": "hive-ok"}],
	 "html_url": "https://fj.example/acme/widgets/issues/2",
	 "created_at": "2026-11-01T12:00:00Z", "user": {"login": "alice"}},
	{"number": 3, "title": "Not for agents", "state": "open",
	 "labels": [{"name": "do-not-touch"}, {"name": "hive-ok"}],
	 "created_at": "2026-11-01T12:00:00Z", "user": {"login": "alice"}},
	{"number": 4, "title": "No admit label", "state": "open", "labels": [],
	 "created_at": "2026-11-01T12:00:00Z", "user": {"login": "alice"}},
	{"number": 5, "title": "A PR stub", "state": "open", "labels": [],
	 "user": {"login": "alice"}, "pull_request": {"merged": false}}
]`

// fakeForgejoPullsJSON is a Forgejo /pulls page: one open PR, one draft and
// one held PR.
const fakeForgejoPullsJSON = `[
	{"number": 10, "title": "fix: the widget", "state": "open", "labels": [],
	 "html_url": "https://fj.example/acme/widgets/pulls/10",
	 "created_at": "2026-11-03T12:00:00Z", "draft": false, "user": {"login": "carol"},
	 "head": {"ref": "fix-widget", "sha": "abc123"}, "base": {"ref": "main", "sha": "def456"}},
	{"number": 11, "title": "wip", "state": "open", "labels": [],
	 "created_at": "2026-11-03T12:00:00Z", "draft": true, "user": {"login": "carol"},
	 "head": {"ref": "wip"}, "base": {"ref": "main"}},
	{"number": 12, "title": "held change", "state": "open", "labels": [{"name": "hold"}],
	 "created_at": "2026-11-03T12:00:00Z", "draft": false, "user": {"login": "dave"},
	 "head": {"ref": "held", "sha": "999"}, "base": {"ref": "main"}}
]`

// fakeForgejo serves the two list endpoints for acme/widgets and records the
// Authorization header and paths it saw.
type fakeForgejo struct {
	mu    sync.Mutex
	auths []string
	paths []string
	fail  bool
}

func (f *fakeForgejo) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths = append(f.auths, r.Header.Get("Authorization"))
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		if f.fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s: enumeration must only read", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		// Page 2 and later are empty so the adapter stops paginating.
		if p := r.URL.Query().Get("page"); p != "" && p != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		switch r.URL.Path {
		case "/api/v1/repos/acme/widgets/issues":
			_, _ = w.Write([]byte(fakeForgejoIssuesJSON))
		case "/api/v1/repos/acme/widgets/pulls":
			_, _ = w.Write([]byte(fakeForgejoPullsJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"not found"}`))
		}
	}
}

func forgejoOnlyConfig(url string) *config.Config {
	cfg := &config.Config{}
	cfg.HiveID = "test-hive"
	cfg.Project.Org = "acme"
	cfg.Project.Forge = config.ForgeForgejo
	cfg.Project.Repos = []string{"widgets"}
	cfg.Forgejo.URL = url
	cfg.Forgejo.TokenEnv = forgejoTestTokenEnv
	cfg.Governor.Labels.Exempt = []string{"do-not-touch"}
	cfg.Project.IssueFilter.RequireLabels = []string{"hive-ok"}
	return cfg
}

func TestEnumerateForgeActionableAgainstFakeForgejo(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	cfg := forgejoOnlyConfig(srv.URL)

	got, err := enumerateActionableForCycle(context.Background(), cfg, nil, discardLogger())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}

	for _, a := range fake.auths {
		if a != "token "+forgejoTestToken {
			t.Fatalf("Authorization = %q, want the forgejo token from token_env", a)
		}
	}

	if got.Issues.Count != 1 || len(got.Issues.Items) != 1 {
		t.Fatalf("actionable issues = %+v, want only #1", got.Issues.Items)
	}
	is := got.Issues.Items[0]
	if is.Repo != "widgets" || is.Number != 1 || is.Title != "Fix the widget" || is.Body != "it breaks" ||
		is.Author != "alice" || !is.AuthorIsHuman || is.URL != "https://fj.example/acme/widgets/issues/1" {
		t.Errorf("issue #1 mapped wrong: %+v", is)
	}
	if !reflect.DeepEqual(is.Assignees, []string{"bob"}) {
		t.Errorf("assignees = %v, want [bob]", is.Assignees)
	}
	if want := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC); !is.UpdatedAt.Equal(want) {
		t.Errorf("UpdatedAt = %v, want %v", is.UpdatedAt, want)
	}

	if got.PRs.Count != 1 || len(got.PRs.Items) != 1 {
		t.Fatalf("actionable PRs = %+v, want only #10 (draft dropped, held moved)", got.PRs.Items)
	}
	pr := got.PRs.Items[0]
	if pr.Number != 10 || pr.HeadSHA != "abc123" || pr.HeadRef != "fix-widget" || pr.BaseRef != "main" ||
		pr.Author != "carol" || pr.Mergeable != github.MergeableUnknown || pr.CIStatus != "" {
		t.Errorf("PR #10 mapped wrong: %+v", pr)
	}
	if len(got.PRs.Held) != 1 || got.PRs.Held[0].Number != 12 {
		t.Errorf("held PRs = %+v, want #12", got.PRs.Held)
	}

	if got.Hold.Total != 2 || got.Hold.Issues != 1 || got.Hold.PRs != 1 {
		t.Errorf("hold = %+v, want one issue (#2) and one PR (#12)", got.Hold)
	}
	if c := got.TotalByRepo["widgets"]; c.Issues != 4 || c.PRs != 3 {
		t.Errorf("TotalByRepo[widgets] = %+v, want 4 issues and 3 PRs", c)
	}
	if got.Stale() {
		t.Errorf("result marked stale: %v", got.StaleRepos)
	}
}

func TestEnumerateForgeActionableAllReposFailKeepsPriorState(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{fail: true}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	got, err := enumerateForgeActionable(context.Background(), forgejoOnlyConfig(srv.URL), discardLogger())
	if err == nil || got != nil {
		t.Fatalf("want an error and no result when every repo fails, got %+v, %v", got, err)
	}
	if !strings.Contains(err.Error(), "all 1 repos failed") {
		t.Errorf("error = %v, want the all-repos-failed message", err)
	}
}

func TestEnumerateForgeActionablePartialFailureMarksRepoStale(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	cfg := forgejoOnlyConfig(srv.URL)
	// "missing" is not served by the fake, so its listing 404s.
	cfg.Project.Repos = []string{"widgets", "missing"}

	got, err := enumerateForgeActionable(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if !got.RepoStale("missing") || got.RepoStale("widgets") {
		t.Errorf("stale repos = %v, want only missing", got.StaleRepos)
	}
	if got.StaleSince.IsZero() {
		t.Error("StaleSince is zero with a stale repo")
	}
	if _, ok := got.TotalByRepo["missing"]; !ok {
		t.Error("failed repo has no TotalByRepo entry, so the dashboard would drop it")
	}
	if got.Issues.Count != 1 {
		t.Errorf("issues = %d, want the healthy repo's 1", got.Issues.Count)
	}
}

func TestEnumerateForgeActionableSkipsPausedRepos(t *testing.T) {
	t.Setenv(forgejoTestTokenEnv, forgejoTestToken)
	fake := &fakeForgejo{}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	cfg := forgejoOnlyConfig(srv.URL)
	cfg.Project.Repos = []string{"widgets", "paused"}
	cfg.Project.PausedRepos = []config.RepoPause{{Repo: "paused"}}

	got, err := enumerateForgeActionable(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("enumerate: %v", err)
	}
	if _, ok := got.TotalByRepo["paused"]; ok {
		t.Error("paused repo was enumerated")
	}
	for _, p := range fake.paths {
		if strings.Contains(p, "/paused/") {
			t.Errorf("paused repo was listed: %s", p)
		}
	}
}

func TestForgeIssueGateReporterTrustFailsClosed(t *testing.T) {
	on := true
	cfg := &config.Config{}
	cfg.Project.IssueFilter.ReporterTrust.Enabled = &on
	cfg.Project.IssueFilter.ReporterTrust.TrustedLogins = []string{"maintainer"}
	gate := newForgeIssueGate(cfg)

	if got := gate.classifyIssue("stranger", nil); got != forgeGateReporterTriage {
		t.Errorf("stranger with no triage label = %q, want %q (no association on forge payloads)", got, forgeGateReporterTriage)
	}
	triage := config.DefaultUntrustedRequireLabels[0]
	if got := gate.classifyIssue("stranger", []string{triage}); got != "" {
		t.Errorf("stranger with %q = %q, want actionable", triage, got)
	}
	if got := gate.classifyIssue("maintainer", nil); got != "" {
		t.Errorf("trusted login = %q, want actionable", got)
	}
}

func TestForgeIssueGateHardSuppress(t *testing.T) {
	gate := newForgeIssueGate(&config.Config{})
	if got := gate.classifyIssue("alice", []string{config.DefaultNeedsHumanLabel}); got != config.DefaultNeedsHumanLabel {
		t.Errorf("needs-human issue = %q, want %q", got, config.DefaultNeedsHumanLabel)
	}
}

// TestHoldLabelsForConfigMatchesGitHubClient pins that the forge enumeration
// holds exactly what the GitHub client is configured to hold.
func TestHoldLabelsForConfigMatchesGitHubClient(t *testing.T) {
	on := true
	cfg := &config.Config{}
	cfg.HiveID = "hive-x"
	cfg.Project.IssueFilter.ReporterTrust.ClankerRequested = &on
	b := &boot{cfg: cfg}
	if got, want := holdLabelsForConfig(cfg), b.githubHoldLabels(); !reflect.DeepEqual(got, want) {
		t.Errorf("holdLabelsForConfig = %v, want the GitHub client's %v", got, want)
	}
}

// TestEnumerateActionableForCycleKeepsGitHubPath is the negative test: a
// GitHub hive, and a hive with any repo on GitHub, must still enumerate
// through the GitHub client first. A failed GitHub pass returns before any
// forge lister is built, so the cycle keeps its prior state.
func TestEnumerateActionableForCycleKeepsGitHubPath(t *testing.T) {
	prev := forgeListerForRepo
	t.Cleanup(func() { forgeListerForRepo = prev })
	forgeListerForRepo = func(*config.Config, string) (forgeLister, error) {
		t.Fatal("forge lister built on a hive with a GitHub repo")
		return nil, nil
	}

	ghCfg := &config.Config{}
	ghCfg.Project.Org = "acme"
	ghCfg.Project.Repos = []string{"widgets"}

	mixed := forgejoOnlyConfig("https://fj.example")
	mixed.Project.RepoPolicies = []config.RepoPolicy{{Repo: "gh-repo", Forge: config.ForgeGitHub}}

	for name, cfg := range map[string]*config.Config{"github": ghCfg, "mixed": mixed} {
		t.Run(name, func(t *testing.T) {
			// A nil client is the GitHub path's own refusal: proof the call
			// went to github.Client.EnumerateActionable.
			_, err := enumerateActionableForCycle(context.Background(), cfg, nil, discardLogger())
			if !errors.Is(err, github.ErrNoGitHubClient) {
				t.Fatalf("err = %v, want the GitHub client's ErrNoGitHubClient", err)
			}
		})
	}
}

func TestForgeListerForRepoRefusesGitHubAndForeignHost(t *testing.T) {
	cfg := &config.Config{}
	if _, err := forgeListerForRepo(cfg, "widgets"); !errors.Is(err, errForgeNotOffGitHub) {
		t.Errorf("GitHub repo: err = %v, want errForgeNotOffGitHub", err)
	}

	cfg = forgejoOnlyConfig("https://fj.example")
	cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "widgets", ForgeURL: "https://elsewhere.example"}}
	if _, err := forgeListerForRepo(cfg, "widgets"); err == nil || !strings.Contains(err.Error(), "differs from") {
		t.Errorf("foreign forge_url: err = %v, want the token host refusal", err)
	}

	for _, kind := range []string{config.ForgeGitLab, config.ForgeGitea, config.ForgeForgejo} {
		cfg := &config.Config{}
		cfg.Project.Forge = kind
		cfg.GitLab.URL, cfg.Gitea.URL, cfg.Forgejo.URL = "https://gl.example", "https://gt.example", "https://fj.example"
		l, err := forgeListerForRepo(cfg, "widgets")
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if f, ok := l.(forge.Forge); !ok || string(f.Kind()) != kind {
			t.Errorf("%s: lister = %T, want the %s adapter", kind, l, kind)
		}
	}
}

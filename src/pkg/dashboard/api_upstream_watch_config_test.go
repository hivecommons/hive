package dashboard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

const upstreamWatchConfigPath = "/api/config/upstream-watch"

// upstreamWatchConfigServer is apiServer with a second watchable repo and a
// fork already declared, so add / update / remove all have something to act on.
func upstreamWatchConfigServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	s, deps := apiServer(t)
	deps.Config.Project.Repos = []string{"repo1", "repo2"}
	deps.Config.UpstreamWatch = config.UpstreamWatchConfig{
		Enabled:  true,
		Interval: 6 * time.Hour,
		Repos: map[string]config.UpstreamWatchRepo{
			"repo1": {Upstream: "up/stream", Sources: []string{"releases", "prs"}, Label: "upstream/port"},
		},
	}
	return s, deps
}

func TestUpstreamWatchConfigPutOwnerGated(t *testing.T) {
	s, deps := upstreamWatchConfigServer(t)
	rec := doPutNoRole(s, upstreamWatchConfigPath, `{"enabled":false}`)
	if rec.Code != 403 {
		t.Fatalf("PUT without owner role = %d, want 403", rec.Code)
	}
	if !deps.Config.UpstreamWatch.Enabled {
		t.Error("a forbidden PUT changed the config")
	}
}

func TestUpstreamWatchConfigPutRejected(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"malformed", `{`, "invalid body"},
		{"empty", `{}`, "nothing to update"},
		{"bad interval", `{"interval":"soon"}`, "invalid interval"},
		{"negative interval", `{"interval":"-1h"}`, "must not be negative"},
		{"unknown repo", `{"repos":{"not-listed":{}}}`, "not listed in project.repos"},
		{"blank repo", `{"repos":{"  ":{}}}`, "repo name is required"},
		{"bad upstream", `{"repos":{"repo1":{"upstream":"nope"}}}`, "invalid upstream"},
		{"bad source", `{"repos":{"repo2":{"sources":["tags"]}}}`, "invalid source"},
		{"negative cap", `{"repos":{"repo1":{"max_issues_per_run":-1}}}`, "max_issues_per_run"},
		{"foreign org", `{"repos":{"other/repo2":{}}}`, "not listed in project.repos"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, deps := upstreamWatchConfigServer(t)
			saves := 0
			deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
			deps.SkipReloadFunc = func() { saves++ }
			rec := doPutRaw(s, upstreamWatchConfigPath, tc.body)
			if rec.Code != 400 || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("PUT %s = %d %s, want 400 containing %q", tc.body, rec.Code, rec.Body.String(), tc.want)
			}
			w := deps.Config.UpstreamWatch
			if !w.Enabled || w.Interval != 6*time.Hour || len(w.Repos) != 1 || w.Repos["repo1"].Upstream != "up/stream" {
				t.Errorf("rejected PUT mutated the config: %+v", w)
			}
			if saves != 0 {
				t.Errorf("rejected PUT persisted (%d saves)", saves)
			}
		})
	}
}

func TestUpstreamWatchConfigPutEdits(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, w config.UpstreamWatchConfig)
	}{
		{"disable", `{"enabled":false}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			if w.Enabled || len(w.Repos) != 1 {
				t.Errorf("got %+v, want disabled with repo1 kept", w)
			}
		}},
		{"interval", `{"interval":"30m"}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			if w.Interval != 30*time.Minute || !w.Enabled {
				t.Errorf("got %+v, want 30m and still enabled", w)
			}
		}},
		{"interval reset to default", `{"interval":""}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			if w.Interval != config.DefaultUpstreamWatchInterval {
				t.Errorf("interval = %s, want default", w.Interval)
			}
		}},
		{"add with defaults", `{"repos":{"repo2":{}}}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			r, ok := w.Repos["repo2"]
			if !ok || len(w.Repos) != 2 {
				t.Fatalf("repo2 not added: %+v", w.Repos)
			}
			if r.Upstream != "" || len(r.Sources) != 2 || r.Label != config.DefaultUpstreamWatchLabel {
				t.Errorf("repo2 = %+v, want fork-parent upstream and loader defaults", r)
			}
		}},
		{"add org-qualified stores bare", `{"repos":{"myorg/repo2":{"upstream":"a/b"}}}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			if _, ok := w.Repos["myorg/repo2"]; ok {
				t.Error("org-qualified key stored as-is")
			}
			if w.Repos["repo2"].Upstream != "a/b" {
				t.Errorf("repo2 = %+v", w.Repos["repo2"])
			}
		}},
		{"update keeps absent fields", `{"repos":{"repo1":{"sources":["prs"],"pr_labels":[" bug ",""],"max_issues_per_run":3,"label":"port"}}}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			r := w.Repos["repo1"]
			if r.Upstream != "up/stream" {
				t.Errorf("upstream = %q, want unchanged up/stream", r.Upstream)
			}
			if len(r.Sources) != 1 || r.Sources[0] != "prs" || len(r.PRLabels) != 1 || r.PRLabels[0] != "bug" ||
				r.MaxIssuesPerRun != 3 || r.Label != "port" {
				t.Errorf("repo1 = %+v", r)
			}
		}},
		{"clear upstream", `{"repos":{"repo1":{"upstream":""}}}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			if w.Repos["repo1"].Upstream != "" {
				t.Errorf("upstream = %q, want cleared", w.Repos["repo1"].Upstream)
			}
		}},
		{"remove", `{"repos":{"repo1":null}}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			if w.Repos != nil {
				t.Errorf("repos = %v, want nil after removing the last one", w.Repos)
			}
		}},
		{"remove absent is a no-op", `{"repos":{"repo2":null}}`, func(t *testing.T, w config.UpstreamWatchConfig) {
			if len(w.Repos) != 1 {
				t.Errorf("repos = %v", w.Repos)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, deps := upstreamWatchConfigServer(t)
			rec := doPutRaw(s, upstreamWatchConfigPath, tc.body)
			if rec.Code != 200 {
				t.Fatalf("PUT %s = %d %s", tc.body, rec.Code, rec.Body.String())
			}
			tc.check(t, deps.Config.UpstreamWatch)
			if err := deps.Config.ValidateUpstreamWatch(); err != nil {
				t.Errorf("accepted edit left an invalid block: %v", err)
			}
		})
	}
}

// A YAML key spelled org-qualified is the same repo as the bare name: an
// update rewrites it to the bare spelling instead of adding a duplicate.
func TestUpstreamWatchConfigPutRespellsQualifiedKey(t *testing.T) {
	s, deps := upstreamWatchConfigServer(t)
	deps.Config.UpstreamWatch.Repos = map[string]config.UpstreamWatchRepo{"MyOrg/repo1": {Upstream: "up/stream"}}
	if rec := doPutRaw(s, upstreamWatchConfigPath, `{"repos":{"repo1":{"label":"x"}}}`); rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	got := deps.Config.UpstreamWatch.Repos
	if len(got) != 1 || got["repo1"].Upstream != "up/stream" || got["repo1"].Label != "x" {
		t.Errorf("repos = %+v, want one bare repo1 entry", got)
	}
}

func TestUpstreamWatchConfigPutPersistsAndAudits(t *testing.T) {
	s, deps := upstreamWatchConfigServer(t)
	saves := 0
	deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	deps.SkipReloadFunc = func() { saves++ }
	rec := doPutRaw(s, upstreamWatchConfigPath, `{"repos":{"repo2":{"upstream":"origin/thing"}}}`)
	if rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	if saves != 1 {
		t.Errorf("saveConfig ran %d times, want 1", saves)
	}
	raw, err := os.ReadFile(deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if !strings.Contains(string(raw), "upstream_watch") || !strings.Contains(string(raw), "origin/thing") {
		t.Errorf("persisted config lacks the edit:\n%s", raw)
	}
	entries := s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "config_upstream_watch")
	if len(entries) != 1 || !strings.Contains(entries[0].Detail, "repos=2") || !strings.Contains(entries[0].Detail, "enabled=true") {
		t.Errorf("change not audited as expected: %+v", entries)
	}
}

// The GET view carries what the editor prefills: interval, project.repos for
// the picker, and each repo's configured entry.
func TestUpstreamWatchGetCarriesEditorFields(t *testing.T) {
	s, _ := upstreamWatchConfigServer(t)
	SetUpstreamWatchStatePathForTest(t, filepath.Join(t.TempDir(), "missing.json"))
	got := decodeUpstreamWatch(t, s)
	if got.Interval != "6h0m0s" {
		t.Errorf("interval = %q", got.Interval)
	}
	if len(got.ProjectRepos) != 2 {
		t.Errorf("project_repos = %v", got.ProjectRepos)
	}
	if len(got.Repos) != 1 || got.Repos[0].Config.Label != "upstream/port" || len(got.Repos[0].Config.Sources) != 2 {
		t.Errorf("repos = %+v", got.Repos)
	}
}

// Removing a repo in the Repos tab must prune its upstream_watch entry, or the
// persisted config fails the next load.
func TestHandleGovernorReposPrunesUpstreamWatch(t *testing.T) {
	s, deps := upstreamWatchConfigServer(t)
	deps.Config.UpstreamWatch.Repos["repo2"] = config.UpstreamWatchRepo{}
	rec := doPut(s, "/api/config/governor/repos", map[string]interface{}{"repos": []string{"repo2"}, "primaryRepo": "repo2"})
	if rec.Code != 200 {
		t.Fatalf("PUT repos = %d %s", rec.Code, rec.Body.String())
	}
	if _, ok := deps.Config.UpstreamWatch.Repos["repo1"]; ok {
		t.Error("removed repo1 is still watched")
	}
	if _, ok := deps.Config.UpstreamWatch.Repos["repo2"]; !ok {
		t.Error("kept repo2 was pruned")
	}
	if err := deps.Config.ValidateUpstreamWatch(); err != nil {
		t.Errorf("config after repo removal fails validation: %v", err)
	}
}

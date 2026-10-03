package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// issuesDisabledGHServer serves repo metadata for myorg/{open,fork-off,off}:
// "open" has Issues on, "fork-off" is a fork with Issues off, "off" is a
// non-fork with Issues off. When fail is set every metadata call returns 500.
func issuesDisabledGHServer(t *testing.T, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	repos := map[string]map[string]any{
		"/repos/myorg/open":     {"has_issues": true, "fork": false},
		"/repos/myorg/fork-off": {"has_issues": false, "fork": true},
		"/repos/other/off":      {"has_issues": false, "fork": false},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		meta, ok := repos[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(meta)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func issuesDisabledTestCollector(gh *ghpkg.Client) *MetricsCollector {
	mc := &MetricsCollector{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		metrics: make(map[string]any),
		org:     "myorg",
		repo:    "open",
	}
	mc.SetGitHubClientProvider(func() *ghpkg.Client { return gh })
	return mc
}

// #9972: every watched repo is probed, bare names resolve under the org, and
// each Issues-disabled repo is reported with its fork flag and the same remedy
// text IssuesDisabledError carries.
func TestCollectIssuesDisabled_ReportsEachDisabledRepo(t *testing.T) {
	srv := issuesDisabledGHServer(t, nil)
	gh := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"open", "fork-off", "other/off"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mc := issuesDisabledTestCollector(gh)

	mc.collectIssuesDisabled(context.Background())

	got := mc.GetIssuesDisabledRepos()
	if len(got) != 2 {
		t.Fatalf("issues-disabled repos = %+v, want fork-off and other/off", got)
	}
	if got[0].Repo != "myorg/fork-off" || !got[0].Fork {
		t.Errorf("first = %+v, want myorg/fork-off (fork)", got[0])
	}
	if got[1].Repo != "other/off" || got[1].Fork {
		t.Errorf("second = %+v, want other/off (not a fork)", got[1])
	}
	for _, r := range got {
		want := (&ghpkg.IssuesDisabledError{Repo: r.Repo, Fork: r.Fork}).Error()
		if r.Message != want {
			t.Errorf("%s message = %q, want IssuesDisabledError text %q", r.Repo, r.Message, want)
		}
		if !strings.Contains(r.Message, "Settings > General > Features") {
			t.Errorf("%s message lacks the remedy: %q", r.Repo, r.Message)
		}
	}
}

func TestCollectIssuesDisabled_AllEnabledReportsNone(t *testing.T) {
	srv := issuesDisabledGHServer(t, nil)
	gh := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"open"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mc := issuesDisabledTestCollector(gh)

	mc.collectIssuesDisabled(context.Background())

	if got := mc.GetIssuesDisabledRepos(); got != nil {
		t.Fatalf("issues-disabled repos = %+v, want none", got)
	}
}

// A failed metadata probe must fail open: it neither invents a warning nor
// clears one already observed.
func TestCollectIssuesDisabled_ProbeFailureKeepsPreviousVerdict(t *testing.T) {
	var fail atomic.Bool
	srv := issuesDisabledGHServer(t, &fail)
	gh := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"open", "fork-off"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mc := issuesDisabledTestCollector(gh)

	mc.collectIssuesDisabled(context.Background())
	if got := mc.GetIssuesDisabledRepos(); len(got) != 1 || got[0].Repo != "myorg/fork-off" {
		t.Fatalf("initial probe = %+v, want myorg/fork-off", got)
	}

	fail.Store(true)
	mc.collectIssuesDisabled(context.Background())
	if got := mc.GetIssuesDisabledRepos(); len(got) != 1 || got[0].Repo != "myorg/fork-off" {
		t.Fatalf("after failed probe = %+v, want the previous myorg/fork-off verdict kept", got)
	}
}

func TestCollectIssuesDisabled_NoClientIsNoop(t *testing.T) {
	mc := issuesDisabledTestCollector(nil)
	mc.collectIssuesDisabled(context.Background())
	if got := mc.GetIssuesDisabledRepos(); got != nil {
		t.Fatalf("issues-disabled repos with no client = %+v, want none", got)
	}
	var nilMC *MetricsCollector
	if got := nilMC.GetIssuesDisabledRepos(); got != nil {
		t.Fatalf("nil collector = %+v, want nil", got)
	}
}

// The status payload carries the collector's verdict so the banner can render.
func TestStatusPayload_IssuesDisabledReposJSON(t *testing.T) {
	mc := &MetricsCollector{
		metrics:        make(map[string]any),
		issuesDisabled: []IssuesDisabledRepo{{Repo: "myorg/fork-off", Fork: true, Message: "m"}},
	}
	payload := &StatusPayload{IssuesDisabledRepos: mc.GetIssuesDisabledRepos()}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"issuesDisabledRepos":[{"repo":"myorg/fork-off","fork":true,"message":"m"}]`) {
		t.Fatalf("status JSON missing issuesDisabledRepos: %s", data)
	}
	empty, _ := json.Marshal(&StatusPayload{})
	if strings.Contains(string(empty), "issuesDisabledRepos") {
		t.Fatalf("issuesDisabledRepos must be omitted when empty: %s", empty)
	}
}

func TestIssuesDisabledBannerWired(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="issues-disabled-banner" class="gh-app-install-banner perm-issue" hidden`,
		`id="issues-disabled-banner-list"`,
		"renderIssuesDisabledBanner(data);",
		"data.issuesDisabledRepos",
		`data-action="dismissIssuesDisabledRepo"`,
		"function dismissIssuesDisabledRepo(repo)",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("issues-disabled banner is missing %q", want)
		}
	}
	notices := strings.Index(html, `<div id="dash-notices">`)
	banner := strings.Index(html, `id="issues-disabled-banner"`)
	firstSection := strings.Index(html, `data-dashboard-section="overview-section"`)
	if notices < 0 || banner < notices || banner > firstSection {
		t.Fatal("issues-disabled banner must sit in the pinned dashboard notices")
	}
}

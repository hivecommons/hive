package scheduler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// utah-packages#879 as a scanner task: before, the query was the hive's own
// routing labels, its complexity tier and the review backlog stem
// ("stage/010-actionable agent/reviewer kind/cleanup approved-direction
// medium reviewer review backlog style …"), so review notes from other repos
// outranked anything about utah-packages. The query keeps what the issue is
// about.
func TestExtractKeywords_DropsHiveRouting(t *testing.T) {
	issue := github.Issue{
		Repo:           "projectbluefin/utah-packages",
		Number:         879,
		Title:          "[reviewer] Review backlog (style): spec file mixes tabs and spaces",
		ComplexityTier: "Medium",
		Labels: []string{"stage/010-actionable", "agent/reviewer", "kind/cleanup", "approved-direction",
			"hive/claimed-by-scanner", "needs-triage", "epic", "claimed", "author/human", "hive-pause/h1",
			"origin/governor", "ok-to-test", "tide/merge-method-squash", "do-not-merge"},
	}
	got := extractKeywords([]github.Issue{issue})
	want := []string{"kind/cleanup", "style", "spec", "file", "mixes", "tabs", "spaces"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords = %q\nwant       %q", got, want)
	}
}

// An issue whose every term is hive routing gets the query earlier releases
// built, so the filter never leaves it with nothing to search on.
func TestExtractKeywords_AllRoutingFallsBackToLegacyQuery(t *testing.T) {
	issue := github.Issue{
		Repo:   "projectbluefin/utah",
		Number: 12,
		Title:  "[ci-maintainer] Fix CI",
		Labels: []string{"agent/ci-maintainer", "stage/010-actionable", "kind/bug"},
	}
	got := extractKeywords([]github.Issue{issue})
	want := []string{"agent/ci-maintainer", "stage/010-actionable", "ci-maintainer"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords = %q\nwant the legacy query %q", got, want)
	}
}

// A hive that configures its own never-claim labels gets those left out too,
// and the default "epic" then counts as an ordinary word.
func TestExtractKeywords_ConfiguredNeverClaimLabels(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governor.Claims.NeverClaimLabels = []string{"Tracker"}
	issue := github.Issue{Repo: "org/repo", Number: 3, Title: "Sidebar anchors drift", Labels: []string{"tracker", "epic"}}
	got := extractKeywordsFor(cfg, []github.Issue{issue})
	want := []string{"epic", "sidebar", "anchors", "drift"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keywords = %q, want %q", got, want)
	}
}

func TestSearchableTitle(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{"packager": {}}
	filers := hiveFilers(cfg)
	for title, want := range map[string]string{
		"[reviewer] Review backlog (security): token in log line": "security token in log line",
		"[someone] Review backlog (style): long line":             "style long line",
		"Review backlog (style): long line":                       "style long line",
		"[scanner] Sidebar anchors drift":                         "Sidebar anchors drift",
		"[selfreport] Agent pane froze":                           "Agent pane froze",
		"[packager] Bump the spec":                                "Bump the spec",
		"[nvidia] driver fails to build on F44":                   "[nvidia] driver fails to build on F44",
		"[ACMM L0] Add a CONTRIBUTING file":                       "[ACMM L0] Add a CONTRIBUTING file",
		"Review the backlog of stale PRs":                         "Review the backlog of stale PRs",
	} {
		if got := searchableTitle(filers, title); got != want {
			t.Errorf("searchableTitle(%q) = %q, want %q", title, got, want)
		}
	}
	if got := searchableTitle(hiveFilers(nil), "[packager] Bump the spec"); got != "[packager] Bump the spec" {
		t.Errorf("an unconfigured agent name was stripped as a filer: %q", got)
	}
}

func TestIsRoutingLabel(t *testing.T) {
	for _, l := range []string{"kind/bug", "triage/accepted", "stage/010-actionable", "stage/stalled", "agent/reviewer",
		"hive/claimed-by-scanner", "hive: churn-triaged", "origin/governor", "triage/needs-information", "size/l",
		"needs-human", "do-not-merge", "do-not-merge/hold", "dco-signoff: yes", "sentinel-alert", "approved-direction",
		"hive-review", "lgtm", "approved", "claimed", "author/human", "hive-pause/h1", "ok-to-test", "needs-ok-to-test",
		"tide/merge-method-squash", "release-note-none", "cncf-cla: yes"} {
		if !isRoutingLabel(l) {
			t.Errorf("%q is a routing label and should not be a search term", l)
		}
	}
	for _, l := range []string{"kind/cleanup", "priority/high", "area/packaging", "rpm", "stagehand", "agentic", "authoring", "epic"} {
		if isRoutingLabel(l) {
			t.Errorf("%q describes the work and should stay a search term", l)
		}
	}
}

// The issue's own terms match nothing in the wiki, but the terms earlier
// releases used do: the kick still gets the section it got before, from a
// second search with the full query.
func TestPrimeKnowledge_NoHitFallsBackToLegacyQuery(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		results := []map[string]interface{}{}
		if strings.Contains(q, "ci-maintainer") {
			results = append(results, map[string]interface{}{
				"slug": "ci-lane-notes", "title": "CI lane notes", "score": 0.9,
				"type": "gotcha", "confidence": 0.9, "snippet": "LEGACY-HIT: rerun lint once before repairing.",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"total": len(results), "results": results})
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := newScheduler()
	s.SetPrimer(knowledge.NewPrimer([]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: srv.URL, Shared: true}},
		knowledge.PrimerConfig{MaxFacts: 25}, logger))
	issue := makeIssue("projectbluefin/utah", 12, "[ci-maintainer] Flaky lint job", "", 5,
		[]string{"agent/ci-maintainer", "stage/010-actionable"}, false)

	got := s.primeKnowledge("ci-maintainer", []github.Issue{issue})
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(got, "LEGACY-HIT") {
		t.Fatalf("the kick lost the section the full query finds:\n%s\nqueries: %q", got, queries)
	}
	if len(queries) != 2 || queries[0] != "flaky lint job" || !strings.Contains(queries[1], "agent/ci-maintainer") {
		t.Fatalf("queries = %q, want the issue's terms first and the full query second", queries)
	}
}

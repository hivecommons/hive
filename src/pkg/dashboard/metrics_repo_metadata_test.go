package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// repoMetaServer serves GET /repos/{owner}/{repo} for a fixed set of repos and
// counts how often each is read. When fail is set every read returns 500.
type repoMetaServer struct {
	mu    sync.Mutex
	reads map[string]int
	meta  map[string]map[string]any
	fail  atomic.Bool
}

func newRepoMetaServer(t *testing.T) (*repoMetaServer, *httptest.Server) {
	t.Helper()
	s := &repoMetaServer{
		reads: map[string]int{},
		meta: map[string]map[string]any{
			"/repos/myorg/open":     {"has_issues": true, "fork": false, "stargazers_count": 32, "forks_count": 4},
			"/repos/myorg/fork-off": {"has_issues": false, "fork": true},
			"/repos/other/off":      {"has_issues": false, "fork": false},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta, ok := s.meta[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.mu.Lock()
		s.reads[r.URL.Path]++
		s.mu.Unlock()
		if s.fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(meta)
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *repoMetaServer) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[path]
}

// The live projectbluefin hive (2026-11-08) sent GET /repos/{owner}/{repo}
// 189 times an hour from the metrics collector, 177 of them charged: the
// primary repo for stars/forks plus every watched repo for has_issues, on
// every five-minute collect. Stars, forks and the Issues-disabled banner do
// not need that: each repo's metadata is read at most once an hour, and the
// primary repo's read serves both the outreach card and the banner.
func TestMetricsCollector_RepoMetadataReadHourly(t *testing.T) {
	s, srv := newRepoMetaServer(t)
	gh := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"open", "fork-off", "other/off"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mc := issuesDisabledTestCollector(gh)
	now := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	mc.now = func() time.Time { return now }

	collect := func() map[string]any {
		out := mc.collectOutreach(context.Background())
		mc.collectIssuesDisabled(context.Background())
		return out
	}

	// Twelve collects, five minutes apart: one hour of the old cadence.
	var outreach map[string]any
	for i := 0; i < 12; i++ {
		outreach = collect()
		if i < 11 {
			now = now.Add(metricsCollectInterval)
		}
	}
	for _, path := range []string{"/repos/myorg/open", "/repos/myorg/fork-off", "/repos/other/off"} {
		if got := s.count(path); got != 1 {
			t.Errorf("GET %s = %d reads in 55 minutes, want 1", path, got)
		}
	}
	if outreach["stars"] != 32 || outreach["forks"] != 4 {
		t.Errorf("outreach = %v, want stars 32 and forks 4 from the held metadata", outreach)
	}
	if got := mc.GetIssuesDisabledRepos(); len(got) != 2 || got[0].Repo != "myorg/fork-off" || got[1].Repo != "other/off" {
		t.Errorf("issues-disabled repos = %+v, want myorg/fork-off and other/off from the held metadata", got)
	}

	// Past the hour, each repo is read once more and a change is seen.
	s.meta["/repos/myorg/fork-off"] = map[string]any{"has_issues": true, "fork": true}
	now = now.Add(repoMetadataRefreshInterval)
	collect()
	for _, path := range []string{"/repos/myorg/open", "/repos/myorg/fork-off", "/repos/other/off"} {
		if got := s.count(path); got != 2 {
			t.Errorf("GET %s = %d reads after the hour, want 2", path, got)
		}
	}
	if got := mc.GetIssuesDisabledRepos(); len(got) != 1 || got[0].Repo != "other/off" {
		t.Errorf("issues-disabled repos after refresh = %+v, want only other/off", got)
	}
}

// A failed read is not held: the next collect asks again instead of waiting
// an hour, and the banner keeps its previous verdict meanwhile.
func TestMetricsCollector_RepoMetadataFailureRetriesNextCollect(t *testing.T) {
	s, srv := newRepoMetaServer(t)
	gh := ghpkg.NewClientForTest(srv.URL, "myorg", []string{"fork-off"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mc := issuesDisabledTestCollector(gh)
	now := time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)
	mc.now = func() time.Time { return now }

	mc.collectIssuesDisabled(context.Background())
	if got := mc.GetIssuesDisabledRepos(); len(got) != 1 {
		t.Fatalf("initial verdict = %+v, want myorg/fork-off", got)
	}

	now = now.Add(repoMetadataRefreshInterval + time.Minute)
	s.fail.Store(true)
	mc.collectIssuesDisabled(context.Background())
	if got := mc.GetIssuesDisabledRepos(); len(got) != 1 || got[0].Repo != "myorg/fork-off" {
		t.Fatalf("after a failed read = %+v, want the previous verdict kept", got)
	}
	s.fail.Store(false)
	now = now.Add(metricsCollectInterval)
	mc.collectIssuesDisabled(context.Background())
	if got := s.count("/repos/myorg/fork-off"); got != 3 {
		t.Fatalf("reads = %d, want 3 (initial, failed, retried on the next collect)", got)
	}
}

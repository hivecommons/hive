package knowledge

// Tests for the PREnricher GitHub fetch paths (fetchPR / fetchPRFromRepos) and
// the KnowledgeAPI.Promoter accessor. The existing enricher tests only exercise
// the nil-client fallback, leaving the cache-hit, 404 negative-cache,
// transient-error retry, and repo fan-out branches uncovered — exactly the
// paths the #3581-era rate-limit hardening depends on.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/beads"
)

// newEnricherServer returns an enricher wired to an httptest GitHub API stub
// and a counter of PR GET requests it has served. The handler decides the
// response per repo: repos in found get a PR, repos in missing get a 404, and
// anything else gets a 500 (transient).
func newEnricherServer(t *testing.T, org string, repos []string, found map[string]bool, missing map[string]bool) (*PREnricher, *atomic.Int64) {
	t.Helper()

	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Path shape: /api/v3/repos/{owner}/{repo}/pulls/{number}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 6 || parts[2] != "repos" || parts[5] != "pulls" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		repo := parts[4]
		switch {
		case found[repo]:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"number": %s, "body": "found in %s", "changed_files": 2, "additions": 5, "deletions": 1}`, parts[6], repo)
		case missing[repo]:
			http.Error(w, `{"message": "Not Found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message": "boom"}`, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL+"/", srv.URL+"/")
	if err != nil {
		t.Fatalf("WithEnterpriseURLs: %v", err)
	}

	return NewPREnricher(client, org, repos, synthTestLogger()), &calls
}

func TestPREnricher_FetchPR_CachesHit(t *testing.T) {
	e, calls := newEnricherServer(t, "org", nil,
		map[string]bool{"console": true}, nil)

	pr := e.fetchPR(context.Background(), "org", "console", 7)
	if pr == nil {
		t.Fatal("first fetchPR returned nil; want PR")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first lookup made %d requests; want 1", got)
	}

	// Second lookup must be served from the cache — zero extra requests.
	pr2 := e.fetchPR(context.Background(), "org", "console", 7)
	if pr2 == nil {
		t.Fatal("cached fetchPR returned nil; want PR")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cached lookup made %d total requests; want 1", got)
	}
}

func TestPREnricher_FetchPR_404IsNegativeCached(t *testing.T) {
	e, calls := newEnricherServer(t, "org", nil,
		nil, map[string]bool{"console": true})

	if pr := e.fetchPR(context.Background(), "org", "console", 9); pr != nil {
		t.Fatalf("fetchPR on 404 = %v; want nil", pr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first 404 lookup made %d requests; want 1", got)
	}

	// A definitive 404 is a cacheable miss: no re-fetch.
	if pr := e.fetchPR(context.Background(), "org", "console", 9); pr != nil {
		t.Fatalf("negative-cached fetchPR = %v; want nil", pr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("negative-cached lookup made %d total requests; want 1", got)
	}
}

func TestPREnricher_FetchPR_TransientErrorIsRetried(t *testing.T) {
	// "console" is neither found nor missing → 500 every time.
	e, calls := newEnricherServer(t, "org", nil, nil, nil)

	if pr := e.fetchPR(context.Background(), "org", "console", 3); pr != nil {
		t.Fatalf("fetchPR on 500 = %v; want nil", pr)
	}
	first := calls.Load()

	// Transient failures must NOT be blacklisted — the next lookup re-fetches.
	if pr := e.fetchPR(context.Background(), "org", "console", 3); pr != nil {
		t.Fatalf("retried fetchPR = %v; want nil", pr)
	}
	if got := calls.Load(); got <= first {
		t.Fatalf("transient error was negative-cached: %d requests after retry, %d after first", got, first)
	}
}

func TestPREnricher_FetchPRFromRepos_FoundInLaterRepo(t *testing.T) {
	e, _ := newEnricherServer(t, "org", []string{"first", "second"},
		map[string]bool{"second": true}, map[string]bool{"first": true})

	pr := e.fetchPRFromRepos(context.Background(), "org", 11)
	if pr == nil {
		t.Fatal("fetchPRFromRepos returned nil; want PR found in second repo")
	}
	if body := pr.GetBody(); body != "found in second" {
		t.Fatalf("PR body = %q; want %q", body, "found in second")
	}
}

func TestPREnricher_FetchPRFromRepos_MissEverywhereCachesFanKey(t *testing.T) {
	e, calls := newEnricherServer(t, "org", []string{"a", "b", "c"},
		nil, map[string]bool{"a": true, "b": true, "c": true})

	if pr := e.fetchPRFromRepos(context.Background(), "org", 44); pr != nil {
		t.Fatalf("fetchPRFromRepos = %v; want nil", pr)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("first fan-out made %d requests; want 3 (one per repo)", got)
	}

	// The fan-out-level miss is cached: a repeat lookup of the same dead
	// number must skip the loop entirely.
	if pr := e.fetchPRFromRepos(context.Background(), "org", 44); pr != nil {
		t.Fatalf("cached-miss fetchPRFromRepos = %v; want nil", pr)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("cached fan-out miss made %d total requests; want 3", got)
	}
}

func TestPREnricher_ClearCacheResetsFanOutMiss(t *testing.T) {
	e, calls := newEnricherServer(t, "org", []string{"a"},
		nil, map[string]bool{"a": true})

	e.fetchPRFromRepos(context.Background(), "org", 5)
	if got := calls.Load(); got != 1 {
		t.Fatalf("first fan-out made %d requests; want 1", got)
	}

	// ClearCache is the cycle boundary: a PR opened mid-cycle was legitimately
	// absent when probed and must be re-probed next cycle.
	e.ClearCache()
	e.fetchPRFromRepos(context.Background(), "org", 5)
	if got := calls.Load(); got != 2 {
		t.Fatalf("post-ClearCache fan-out made %d total requests; want 2", got)
	}
}

func TestPREnricher_EnrichBody_ViaGhRefFanOut(t *testing.T) {
	// gh-N refs have no repo, so EnrichBody must route through the fan-out.
	e, _ := newEnricherServer(t, "org", []string{"only"},
		map[string]bool{"only": true}, nil)

	b := &beads.Bead{ID: "bead-1", ExternalRef: "gh-21"}
	body := e.EnrichBody(context.Background(), b, "quality")
	if !strings.Contains(body, "found in only") {
		t.Fatalf("EnrichBody = %q; want PR description from fan-out repo", body)
	}
	if !strings.Contains(body, "- Changed: 2 files (+5, -1)") {
		t.Fatalf("EnrichBody = %q; want change stats line", body)
	}
	if !strings.Contains(body, "- Source: bead:quality/bead-1") {
		t.Fatalf("EnrichBody = %q; want source bead line", body)
	}
}

func TestKnowledgeAPI_PromoterAccessor(t *testing.T) {
	api := NewKnowledgeAPI(nil, KnowledgeConfig{Enabled: true}, synthTestLogger())

	p := api.Promoter()
	if p == nil {
		t.Fatal("Promoter() returned nil")
	}
	// The accessor must expose the same promoter the manual PromoteFact path
	// uses, so the scheduled promotion loop shares its layer clients.
	if p != api.promoter {
		t.Fatal("Promoter() returned a different instance than api.promoter")
	}
}

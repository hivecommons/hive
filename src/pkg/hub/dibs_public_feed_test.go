package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ============================================================
// #4233 — the dibs repo feed lists repos that are ACTUALLY public on
// github.com, verified against the GitHub API with a TTL verdict cache,
// instead of requiring the is_public opt-in (false on every production hive,
// so the feed could never populate). These tests drive the verification path
// against a fake GitHub API server; dibs_sso_followups_test.go keeps the
// static filtering (github_host, forge, placeholder) pins.
// ============================================================

// newTestDibsChecker builds a checker pointed at a fake GitHub API, mirroring
// newDibsPublicChecker minus the env token read (tests must not inherit a
// real token from the environment).
func newTestDibsChecker(apiBase string) *dibsPublicChecker {
	return &dibsPublicChecker{
		verdicts: map[string]dibsPublicVerdict{},
		inflight: map[string]bool{},
		sem:      make(chan struct{}, dibsPublicCheckParallel),
		apiBase:  apiBase,
		client:   &http.Client{Timeout: 2 * time.Second},
		logger:   slog.Default(),
		now:      time.Now,
	}
}

// fakeGitHubAPI serves GET /repos/{owner}/{repo} from a mutex-guarded
// visibility map ("public"/"private"; absent = 404) and counts requests. A
// set failAll makes every response a 500, simulating an outage/rate-limit.
type fakeGitHubAPI struct {
	mu       sync.Mutex
	repos    map[string]string
	requests atomic.Int64
	failAll  atomic.Bool
	srv      *httptest.Server
}

func (f *fakeGitHubAPI) setRepo(id, visibility string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos[id] = visibility
}

func newFakeGitHubAPI(t *testing.T, repos map[string]string) *fakeGitHubAPI {
	t.Helper()
	if repos == nil {
		repos = map[string]string{}
	}
	f := &fakeGitHubAPI{repos: repos}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.failAll.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.mu.Lock()
		visibility := f.repos[strings.TrimPrefix(r.URL.Path, "/repos/")]
		f.mu.Unlock()
		switch visibility {
		case "public":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"private":false}`)
		case "private":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"private":true}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// dibsFeed performs one handler call and returns the decoded entries.
func dibsFeed(t *testing.T, s *HubServer) []dibsRepoEntry {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "https://hive.kubestellar.io/api/saas/dibs/repos", nil)
	rec := httptest.NewRecorder()
	s.handleDibsRepos(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/saas/dibs/repos = %d, want 200", rec.Code)
	}
	var got []dibsRepoEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("feed body is not JSON: %v (body %s)", err, rec.Body.String())
	}
	return got
}

// waitForFeed polls the handler until it returns exactly the wanted repo IDs
// (in sorted feed order) or times out. The verification is lazy/async by
// design — the handler never blocks on the GitHub API — so tests converge the
// way production does, across successive polls.
func waitForFeed(t *testing.T, s *HubServer, want []string) []dibsRepoEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got []dibsRepoEntry
	for {
		got = dibsFeed(t, s)
		if len(got) == len(want) {
			match := true
			for i := range want {
				if got[i].RepoID != want[i] {
					match = false
					break
				}
			}
			if match {
				return got
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("feed never converged: got %v, want repo IDs %v", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestDibsReposVerifiesPublicOnGitHub pins the verification path: hives
// WITHOUT the is_public opt-in are included exactly when the GitHub API says
// their repo is public — "private": true and 404 (private or nonexistent)
// both exclude — and every verdict, positive and negative, is served from the
// cache afterwards without further API traffic.
func TestDibsReposVerifiesPublicOnGitHub(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	gh := newFakeGitHubAPI(t, map[string]string{
		"acme/widgets": "public",
		"acme/secret":  "private",
		// acme/ghost absent → 404
	})
	s.dibsPublic = newTestDibsChecker(gh.srv.URL)

	for i, repo := range []string{"widgets", "secret", "ghost"} {
		mkDibsHive(t, SaaSHive{
			ID: fmt.Sprintf("hosted-verify-%d", i), Owner: "octocat", Org: "acme",
			PrimaryRepo: repo, GitHubHost: "github.com", // is_public deliberately false
		})
	}

	got := waitForFeed(t, s, []string{"acme/widgets"})
	if got[0].HiveID != "hosted-verify-0" || got[0].Owner != "octocat" {
		t.Errorf("verified entry = %+v, want hive hosted-verify-0 owned by octocat", got[0])
	}

	// Cache: all three verdicts (one positive, two negative) are now fresh,
	// so further polls must not touch the GitHub API at all.
	after := gh.requests.Load()
	if after != 3 {
		t.Errorf("verification made %d GitHub API calls, want exactly 3 (one per repo)", after)
	}
	for i := 0; i < 3; i++ {
		waitForFeed(t, s, []string{"acme/widgets"})
	}
	if gh.requests.Load() != after {
		t.Errorf("cached polls made %d further API calls, want 0 — both positive AND negative verdicts must cache", gh.requests.Load()-after)
	}
}

// TestDibsReposIsPublicSkipsVerification pins the immediate-include lane: an
// operator's explicit is_public opt-in needs no GitHub round-trip, so the
// entry appears on the FIRST poll with zero API traffic.
func TestDibsReposIsPublicSkipsVerification(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	gh := newFakeGitHubAPI(t, nil)
	s.dibsPublic = newTestDibsChecker(gh.srv.URL)

	mkDibsHive(t, SaaSHive{
		ID: "hosted-optin", Owner: "octocat", Org: "acme",
		PrimaryRepo: "widgets", GitHubHost: "github.com", IsPublic: true,
	})

	got := dibsFeed(t, s)
	if len(got) != 1 || got[0].RepoID != "acme/widgets" {
		t.Fatalf("first poll = %v, want the opted-in repo immediately (no async wait)", got)
	}
	if n := gh.requests.Load(); n != 0 {
		t.Errorf("is_public include made %d GitHub API calls, want 0", n)
	}
}

// TestDibsReposTTLExpiryAndTransientFallback pins the cache's time behavior:
// an expired verdict is re-verified (so a repo flipping private disappears
// within one TTL), and a TRANSIENT failure keeps serving the prior verdict
// instead of dropping the repo or blocking the response.
func TestDibsReposTTLExpiryAndTransientFallback(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	gh := newFakeGitHubAPI(t, map[string]string{"acme/widgets": "public"})
	checker := newTestDibsChecker(gh.srv.URL)
	var clockMu sync.Mutex
	now := time.Now()
	checker.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	s.dibsPublic = checker

	mkDibsHive(t, SaaSHive{
		ID: "hosted-ttl", Owner: "octocat", Org: "acme",
		PrimaryRepo: "widgets", GitHubHost: "github.com",
	})
	waitForFeed(t, s, []string{"acme/widgets"})
	base := gh.requests.Load()

	// Fresh verdict: no re-check within the TTL.
	waitForFeed(t, s, []string{"acme/widgets"})
	if gh.requests.Load() != base {
		t.Fatalf("re-checked a fresh verdict (%d extra calls)", gh.requests.Load()-base)
	}

	// TRANSIENT failure past the TTL: the stale POSITIVE verdict keeps the
	// repo listed (never a flap on a GitHub hiccup), a re-check was attempted,
	// and the failure is cached with the short error TTL (no hammering).
	gh.failAll.Store(true)
	advance(dibsPublicCheckTTL + time.Minute)
	for i := 0; i < 3; i++ {
		waitForFeed(t, s, []string{"acme/widgets"})
	}
	// Let the (single) background re-check land before counting.
	time.Sleep(100 * time.Millisecond)
	waitForFeed(t, s, []string{"acme/widgets"})
	if gh.requests.Load() == base {
		t.Error("expired verdict was never re-checked")
	}

	// Definitive flip past the error TTL: the repo went private, so the next
	// re-verification drops it from the feed.
	gh.failAll.Store(false)
	gh.setRepo("acme/widgets", "private")
	advance(dibsPublicCheckErrTTL + time.Minute)
	waitForFeed(t, s, []string{})
}

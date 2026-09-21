package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Each resolved channel row carries when its commit landed, so the dashboard
// can say how OLD the promoted build is — not only how many commits behind.
func TestChannelTargetsCarryCommitDates(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v5-latest": "sha256:v5",
		"stable":    "sha256:old",
		"candidate": "sha256:v5",
	})
	stubChannelRevisions(t, map[string]string{"stable": "55bd2bc"})
	stableAt := time.Date(2026, 9, 21, 15, 4, 0, 0, time.UTC)
	candidateAt := time.Date(2026, 9, 21, 20, 51, 0, 0, time.UTC)
	stubChannelCommitDates(t, map[string]time.Time{
		"55bd2bc": stableAt,
		"9eefa3a": candidateAt,
	})

	targets := resolveChannelTargets(map[string]string{"v5": "9eefa3a"}, testChannelLogger())

	if got := targetFor(targets, ReleaseChannelStable).CommittedAt; got != "2026-09-21T15:04:00Z" {
		t.Errorf("stable committed_at = %q, want 2026-09-21T15:04:00Z", got)
	}
	if got := targetFor(targets, ReleaseChannelCandidate).CommittedAt; got != "2026-09-21T20:51:00Z" {
		t.Errorf("candidate committed_at = %q, want 2026-09-21T20:51:00Z", got)
	}
	// edge did not resolve at all: no SHA, so no date may be invented.
	if got := targetFor(targets, ReleaseChannelEdge).CommittedAt; got != "" {
		t.Errorf("unresolved edge must carry no committed_at, got %q", got)
	}
}

// An unknown date renders as NO timestamp, never as the zero time.
func TestUnknownCommitDateIsOmitted(t *testing.T) {
	stubChannelDigests(t, map[string]string{
		"v5-latest": "sha256:v5",
		"candidate": "sha256:v5",
	})
	// stubChannelDigests leaves fetchCommitDate failing by default.
	targets := resolveChannelTargets(map[string]string{"v5": "9eefa3a"}, testChannelLogger())
	if got := targetFor(targets, ReleaseChannelCandidate).CommittedAt; got != "" {
		t.Errorf("failed date lookup must leave committed_at empty, got %q", got)
	}
}

// A commit's date is immutable: one lookup per SHA, ever.
func TestCommitDateIsCachedPerSHA(t *testing.T) {
	resetChannelCommitDateCache()
	calls := 0
	orig := fetchCommitDate
	fetchCommitDate = func(string, *slog.Logger) (time.Time, error) {
		calls++
		return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), nil
	}
	t.Cleanup(func() {
		fetchCommitDate = orig
		resetChannelCommitDateCache()
	})
	for i := 0; i < 4; i++ {
		if commitDateForSHA("9eefa3a", testChannelLogger()).IsZero() {
			t.Fatalf("call %d returned zero time", i)
		}
	}
	if calls != 1 {
		t.Errorf("commit API called %d times for one SHA, want 1", calls)
	}
}

// The hub's GitHub API reads must carry HIVE_HUB_GITHUB_TOKEN when it is set:
// anonymous reads share a 60/h budget that the branch poller alone exhausts,
// after which every compare answers 403 and the distance column goes blank.
// Pinned on the compare and commit-date fetchers, which are the two reads this
// column depends on.
func TestGitHubReadsCarryTheHubToken(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"status":"behind","ahead_by":0,"behind_by":3,"commit":{"committer":{"date":"2026-09-21T15:04:00Z"}}}`))
	}))
	defer srv.Close()
	oldBase := githubAPIBase
	githubAPIBase = srv.URL
	t.Cleanup(func() { githubAPIBase = oldBase })
	t.Setenv(hubGitHubTokenEnv, "ghp_test")

	if _, err := fetchCommitCompareCounts("aaa", "bbb", testChannelLogger()); err != nil {
		t.Fatalf("compare: %v", err)
	}
	if _, err := fetchCommitDate("aaa", testChannelLogger()); err != nil {
		t.Fatalf("commit date: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, saw %d", len(seen))
	}
	for i, a := range seen {
		if a != "Bearer ghp_test" {
			t.Errorf("request %d Authorization = %q, want Bearer ghp_test", i, a)
		}
	}
}

// With no token configured the reads stay anonymous — no empty Bearer header,
// which GitHub rejects with 401 rather than treating as unauthenticated.
func TestGitHubReadsStayAnonymousWithoutToken(t *testing.T) {
	t.Setenv(hubGitHubTokenEnv, "  ")
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	authGitHubRequest(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("blank token must add no Authorization header, got %q", got)
	}
	authGitHubRequest(nil) // must not panic
}

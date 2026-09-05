package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// rescanTestServer is the smallest server the rescan handler needs: an audit
// log (auditFromRequest dereferences it) and a Dependencies carrying the hook
// under test.
func rescanTestServer(fn func(context.Context) (*ghpkg.ActionableResult, error)) *Server {
	return &Server{
		audit: &AuditLog{},
		deps: &Dependencies{
			Config:          &config.Config{Project: config.ProjectConfig{Repos: []string{"a", "b"}}},
			RescanReposFunc: fn,
		},
	}
}

func postRescan(t *testing.T, s *Server) (*httptest.ResponseRecorder, ReposRescanResult) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleReposRescan(w, httptest.NewRequest(http.MethodPost, "/api/repos/rescan", nil))
	var got ReposRescanResult
	if w.Body.Len() > 0 {
		_ = json.Unmarshal(w.Body.Bytes(), &got)
	}
	return w, got
}

func sampleActionable() *ghpkg.ActionableResult {
	return &ghpkg.ActionableResult{
		Issues:      ghpkg.IssueResult{Count: 7},
		PRs:         ghpkg.PRResult{Count: 3},
		Hold:        ghpkg.HoldResult{Total: 2},
		TotalByRepo: map[string]ghpkg.RepoCounts{"a": {Issues: 4, PRs: 1}, "b": {Issues: 3, PRs: 2}},
	}
}

// A hive with no rescan hook wired (no forge credentials, or any deployment
// that never installed one) must say so rather than reporting a successful
// scan that found zero of everything.
func TestHandleReposRescan_NotWired(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *Server
	}{
		{"nil deps", &Server{audit: &AuditLog{}}},
		{"nil func", rescanTestServer(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _ := postRescan(t, tc.srv)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
			}
		})
	}
}

func TestHandleReposRescan_ReportsFreshCounts(t *testing.T) {
	var calls int32
	s := rescanTestServer(func(context.Context) (*ghpkg.ActionableResult, error) {
		atomic.AddInt32(&calls, 1)
		return sampleActionable(), nil
	})

	w, got := postRescan(t, s)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("rescan func called %d times, want 1", calls)
	}
	if got.Status != "rescanned" {
		t.Errorf("status = %q, want %q", got.Status, "rescanned")
	}
	if got.Issues != 7 || got.PRs != 3 || got.Hold != 2 || got.Repos != 2 {
		t.Errorf("counts = %+v, want issues=7 prs=3 hold=2 repos=2", got)
	}
	if _, err := time.Parse(time.RFC3339, got.ScannedAt); err != nil {
		t.Errorf("scanned_at %q not RFC3339: %v", got.ScannedAt, err)
	}
}

func TestReposRescanRouteRequiresDashboardAuth(t *testing.T) {
	var calls int32
	s := NewServerWithAuth(0, "secret-token", nil)
	s.RegisterAPI(&Dependencies{
		Config: &config.Config{Project: config.ProjectConfig{Repos: []string{"a"}}},
		RescanReposFunc: func(context.Context) (*ghpkg.ActionableResult, error) {
			atomic.AddInt32(&calls, 1)
			return sampleActionable(), nil
		},
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Post(ts.URL+"/api/repos/rescan", "application/json", nil)
	if err != nil {
		t.Fatalf("POST without auth: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if calls != 0 {
		t.Fatalf("unauthenticated request ran rescan %d times", calls)
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/repos/rescan", nil)
	if err != nil {
		t.Fatalf("build authenticated request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST with auth: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if calls != 1 {
		t.Fatalf("authenticated request ran rescan %d times, want 1", calls)
	}
}

// An enumeration that returned nothing per-repo (a forge error tolerated by
// the work-source path, say) still names how many repos the hive watches, so
// the toast never reads "Scanned 0 repositories" on a hive with repos.
func TestHandleReposRescan_RepoCountFallsBackToConfig(t *testing.T) {
	s := rescanTestServer(func(context.Context) (*ghpkg.ActionableResult, error) {
		return &ghpkg.ActionableResult{}, nil
	})
	_, got := postRescan(t, s)
	if got.Repos != 2 {
		t.Fatalf("repos = %d, want 2 (from Project.Repos)", got.Repos)
	}
}

// The whole point of the debounce: a second press inside the window must NOT
// reach GitHub again. It is answered from the first scan's counts, labelled
// so the UI can say "already up to date" instead of reporting a failure.
func TestHandleReposRescan_DebouncesRepeatPresses(t *testing.T) {
	var calls int32
	s := rescanTestServer(func(context.Context) (*ghpkg.ActionableResult, error) {
		atomic.AddInt32(&calls, 1)
		return sampleActionable(), nil
	})

	if w, _ := postRescan(t, s); w.Code != http.StatusOK {
		t.Fatalf("first scan status = %d, want 200", w.Code)
	}
	w, got := postRescan(t, s)
	if w.Code != http.StatusOK {
		t.Fatalf("debounced status = %d, want 200", w.Code)
	}
	if calls != 1 {
		t.Fatalf("rescan func called %d times, want 1 — the debounce did not hold", calls)
	}
	if got.Status != "debounced" {
		t.Errorf("status = %q, want %q", got.Status, "debounced")
	}
	if got.RetryAfterS <= 0 || got.RetryAfterS > int(repoRescanDebounce.Seconds()) {
		t.Errorf("retry_after_s = %d, want 1..%d", got.RetryAfterS, int(repoRescanDebounce.Seconds()))
	}
	// Debounced is "this data is that fresh", not "no data": the previous
	// scan's counts must still be there for the cards to render.
	if got.Issues != 7 || got.PRs != 3 {
		t.Errorf("debounced counts = %+v, want the previous scan's issues=7 prs=3", got)
	}
}

// Once the window has passed, the next press scans for real again.
func TestHandleReposRescan_ScansAgainAfterDebounceWindow(t *testing.T) {
	var calls int32
	s := rescanTestServer(func(context.Context) (*ghpkg.ActionableResult, error) {
		atomic.AddInt32(&calls, 1)
		return sampleActionable(), nil
	})
	postRescan(t, s)
	s.repoRescanMu.Lock()
	s.repoRescanAt = time.Now().Add(-repoRescanDebounce - time.Second)
	s.repoRescanMu.Unlock()

	_, got := postRescan(t, s)
	if calls != 2 {
		t.Fatalf("rescan func called %d times, want 2", calls)
	}
	if got.Status != "rescanned" {
		t.Errorf("status = %q, want %q", got.Status, "rescanned")
	}
}

// Two presses that overlap must cost ONE forge sweep. The second is told a
// scan is already running rather than queueing a duplicate enumeration.
func TestHandleReposRescan_CollapsesConcurrentPresses(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls int32
	s := rescanTestServer(func(context.Context) (*ghpkg.ActionableResult, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			close(entered)
			<-release
		}
		return sampleActionable(), nil
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		postRescan(t, s)
	}()
	<-entered

	w, got := postRescan(t, s)
	if w.Code != http.StatusOK {
		t.Fatalf("concurrent status = %d, want 200", w.Code)
	}
	if got.Status != "in-progress" {
		t.Errorf("status = %q, want %q", got.Status, "in-progress")
	}
	close(release)
	<-done
	if calls != 1 {
		t.Fatalf("rescan func called %d times, want 1", calls)
	}
}

// A forge failure is reported as one, and must not arm the debounce — an
// operator retrying after a rate-limit blip should not be told the (absent)
// data is fresh.
func TestHandleReposRescan_ForgeError(t *testing.T) {
	s := rescanTestServer(func(context.Context) (*ghpkg.ActionableResult, error) {
		return nil, errors.New("boom")
	})
	w, _ := postRescan(t, s)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
	s.repoRescanMu.Lock()
	armed := !s.repoRescanAt.IsZero()
	s.repoRescanMu.Unlock()
	if armed {
		t.Fatal("a failed scan armed the debounce; the retry would be refused")
	}
}

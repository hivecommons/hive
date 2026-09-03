package hub

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ============================================================
// saas.go — SHA-poll fetch helpers pointed at a fake GitHub/GHCR
// ============================================================

// fakeGitHubGHCR serves branch, workflow-runs, commit, GHCR token, and manifest
// endpoints and points githubAPIBase/ghcrBase at itself for the test's duration.
func fakeGitHubGHCR(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	waitForCommitOrderResolvers(t)
	srv := httptest.NewServer(handler)
	oldGH, oldGHCR := githubAPIBase, ghcrBase
	githubAPIBase = srv.URL
	ghcrBase = srv.URL
	t.Cleanup(func() {
		waitForCommitOrderResolvers(t)
		githubAPIBase, ghcrBase = oldGH, oldGHCR
		srv.Close()
	})
	return srv
}

func waitForCommitOrderResolvers(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		commitOrderMu.Lock()
		inFlight := len(commitOrderInFlight)
		commitOrderMu.Unlock()
		if inFlight == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d commit order resolver(s)", inFlight)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestListRepoBranches(t *testing.T) {
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"name":"v2"},{"name":"dev"}]`))
	})
	client := &http.Client{}
	names := listRepoBranches(client)
	if len(names) != 2 || names[0] != "v2" {
		t.Errorf("listRepoBranches = %+v", names)
	}
}

func TestListRepoBranchesError(t *testing.T) {
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	if names := listRepoBranches(&http.Client{}); names != nil {
		t.Errorf("non-200 should return nil, got %+v", names)
	}
}

func TestFetchBranchSHAImageReady(t *testing.T) {
	resetSHACaches(t)
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK) // GHCR manifest exists
		case strings.Contains(r.URL.Path, "/token"):
			w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(r.URL.Path, "/branches/"):
			w.Write([]byte(`{"commit":{"sha":"abcdef1234567890","commit":{"message":"new commit\nbody"}}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})

	fetchBranchSHA(slog.Default(), "v2")

	if got := getLatestSHAForBranch("v2"); got == "" {
		t.Error("expected image-verified SHA cached for v2")
	}
}

func TestFetchBranchSHAImageBuilding(t *testing.T) {
	resetSHACaches(t)
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusNotFound) // image NOT on GHCR yet
		case strings.Contains(r.URL.Path, "/token"):
			w.Write([]byte(`{"token":"anon"}`))
		case strings.Contains(r.URL.Path, "/actions/workflows/"):
			w.Write([]byte(`{"workflow_runs":[{"status":"in_progress"}]}`))
		case strings.Contains(r.URL.Path, "/branches/"):
			w.Write([]byte(`{"commit":{"sha":"fedcba9876543210","commit":{"message":"wip"}}}`))
		default:
			w.Write([]byte(`{}`))
		}
	})

	fetchBranchSHA(slog.Default(), "dev")

	// Image not yet on GHCR -> head cache advances with a building status.
	head := getBranchHead("dev")
	if head.SHA == "" {
		t.Error("expected head cache to advance even without image")
	}
	if head.ImageStatus == "" {
		t.Errorf("expected an image status, got empty")
	}
}

func TestFetchBranchSHANon200(t *testing.T) {
	resetSHACaches(t)
	// Seed a cached SHA with no message so the backfill branch runs.
	latestSHAMu.Lock()
	latestSHAByBranch["v2"] = branchSHAInfo{SHA: "abc1234"}
	latestSHAMu.Unlock()

	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/commits/") {
			w.Write([]byte(`{"commit":{"message":"backfilled"}}`))
			return
		}
		// branch API returns 403 (rate limited) -> backfill path.
		w.WriteHeader(http.StatusForbidden)
	})
	fetchBranchSHA(slog.Default(), "v2")

	latestSHAMu.RLock()
	msg := latestSHAByBranch["v2"].Message
	latestSHAMu.RUnlock()
	if msg != "backfilled" {
		t.Errorf("expected backfilled message, got %q", msg)
	}
}

func TestFetchAllBranchSHAs(t *testing.T) {
	resetSHACaches(t)
	var calls int
	fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/branches/") {
			calls++
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if strings.Contains(r.URL.Path, "/token") {
			w.Write([]byte(`{"token":"anon"}`))
			return
		}
		w.Write([]byte(`{"commit":{"sha":"1234567abcdef","commit":{"message":"m"}}}`))
	})

	fetchAllBranchSHAs(slog.Default(), []string{"v2", "dev"})
	if calls < 2 {
		t.Errorf("expected a branch fetch per branch, got %d", calls)
	}
}

func TestNextGitHubLink(t *testing.T) {
	link := `<https://api.github.com/x?page=2>; rel="next", <https://api.github.com/x?page=5>; rel="last"`
	if got := nextGitHubLink(link); got != "https://api.github.com/x?page=2" {
		t.Errorf("nextGitHubLink = %q", got)
	}
	if got := nextGitHubLink(`<x>; rel="last"`); got != "" {
		t.Errorf("no next -> %q, want empty", got)
	}
}

func TestListRepoBranchesPagination(t *testing.T) {
	var page int
	srv := fakeGitHubGHCR(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			// Point the next link at the same server, page 2.
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/hivecommons/hive/branches?page=2>; rel="next"`, githubAPIBase))
			w.Write([]byte(`[{"name":"a"}]`))
			return
		}
		w.Write([]byte(`[{"name":"b"}]`))
	})
	_ = srv
	names := listRepoBranches(&http.Client{})
	if len(names) != 2 {
		t.Errorf("paginated branches = %+v, want 2", names)
	}
}

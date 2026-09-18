package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Tests for postRecommendationsForCycle (#7570): the wiring between the eval
// cycle's already-enumerated PR set and the per-repo recommendations issue.
// The gates pinned here — off by default, repo allowlist, MinReadyToOpen,
// continue-on-error — are what keeps this feature from posting uninvited or
// letting one repo's failure starve the rest of the digest run.

// recCycleServer records issue creates per repo so tests can see exactly
// which repositories the cycle wrote to.
type recCycleServer struct {
	*httptest.Server
	mux     *http.ServeMux
	creates map[string]int // repo -> POST /issues count
}

func newRecCycleServer(t *testing.T, org string, repos ...string) *recCycleServer {
	t.Helper()
	s := &recCycleServer{mux: http.NewServeMux(), creates: map[string]int{}}
	for _, repo := range repos {
		repo := repo
		s.mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				json.NewEncoder(w).Encode([]map[string]any{})
			case http.MethodPost:
				io.Copy(io.Discard, r.Body)
				s.creates[repo]++
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(map[string]any{"number": 5})
			}
		})
	}
	s.Server = httptest.NewServer(s.mux)
	t.Cleanup(s.Close)
	return s
}

func recCycleClient(t *testing.T, srv *recCycleServer, org string, repos []string) *github.Client {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return github.NewClientForTest(srv.URL, org, repos, logger)
}

func readyPR(repo string, number int) github.PullRequest {
	return github.PullRequest{
		Repo:           repo,
		Number:         number,
		Title:          fmt.Sprintf("fix: thing %d", number),
		Mergeable:      github.MergeableYes,
		MergeableState: "clean",
		CIStatus:       "success",
		CreatedAt:      time.Now().Add(-48 * time.Hour),
	}
}

func recCycleConfig(enabled bool, repos []string) *config.Config {
	cfg := &config.Config{}
	cfg.Review.Recommendations = config.RecommendationsConfig{
		Enabled: enabled,
		Repos:   repos,
	}
	return cfg
}

// quietLogger is shared from githubauth_test.go.

func TestPostRecommendationsForCycle_DisabledPostsNothing(t *testing.T) {
	org := "testorg"
	srv := newRecCycleServer(t, org, "alpha")
	client := recCycleClient(t, srv, org, []string{"alpha"})

	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{readyPR("alpha", 1)}

	postRecommendationsForCycle(context.Background(),
		recCycleConfig(false, nil), client, actionable, quietLogger())

	if srv.creates["alpha"] != 0 {
		t.Fatalf("disabled feature posted to alpha %d times — it is off by default for a reason", srv.creates["alpha"])
	}
}

func TestPostRecommendationsForCycle_NilArgsDoNotPanic(t *testing.T) {
	logger := quietLogger()
	postRecommendationsForCycle(context.Background(), nil, nil, nil, logger)
	postRecommendationsForCycle(context.Background(), recCycleConfig(true, nil), nil, nil, logger)
}

func TestPostRecommendationsForCycle_RepoAllowlistIsEnforced(t *testing.T) {
	org := "testorg"
	srv := newRecCycleServer(t, org, "allowed", "uninvited")
	client := recCycleClient(t, srv, org, []string{"allowed", "uninvited"})

	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{
		readyPR("allowed", 1),
		readyPR("uninvited", 2),
	}

	postRecommendationsForCycle(context.Background(),
		recCycleConfig(true, []string{"allowed"}), client, actionable, quietLogger())

	if srv.creates["allowed"] != 1 {
		t.Fatalf("allowed repo: creates = %d, want 1", srv.creates["allowed"])
	}
	// Opening an issue in a repository the operator did not list is exactly
	// the uninvited write the allowlist exists to prevent.
	if srv.creates["uninvited"] != 0 {
		t.Fatalf("non-allowlisted repo got %d issue(s)", srv.creates["uninvited"])
	}
}

func TestPostRecommendationsForCycle_NothingReadyOpensNothing(t *testing.T) {
	org := "testorg"
	srv := newRecCycleServer(t, org, "alpha")
	client := recCycleClient(t, srv, org, []string{"alpha"})

	// One open PR, conflicting — so the digest has content, but zero ready
	// PRs. MinReadyToOpen defaults to 1: no issue exists yet, so nothing may
	// be opened just to say nothing is ready.
	conflicting := readyPR("alpha", 1)
	conflicting.MergeableState = "dirty"
	conflicting.Mergeable = github.MergeableNo
	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{conflicting}

	postRecommendationsForCycle(context.Background(),
		recCycleConfig(true, nil), client, actionable, quietLogger())

	if srv.creates["alpha"] != 0 {
		t.Fatalf("no ready PRs and no existing issue, yet %d issue(s) were opened", srv.creates["alpha"])
	}
}

func TestPostRecommendationsForCycle_OneRepoFailingDoesNotStarveTheRest(t *testing.T) {
	org := "testorg"
	// "aaa-broken" sorts before "bbb-healthy", so the failure happens first.
	srv := newRecCycleServer(t, org, "bbb-healthy")
	srv.mux.HandleFunc(fmt.Sprintf("/repos/%s/aaa-broken/issues", org), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	client := recCycleClient(t, srv, org, []string{"aaa-broken", "bbb-healthy"})

	actionable := &github.ActionableResult{}
	actionable.PRs.Items = []github.PullRequest{
		readyPR("aaa-broken", 1),
		readyPR("bbb-healthy", 2),
	}

	postRecommendationsForCycle(context.Background(),
		recCycleConfig(true, nil), client, actionable, quietLogger())

	if srv.creates["bbb-healthy"] != 1 {
		t.Fatalf("healthy repo after a failing one: creates = %d, want 1 (errors must be logged, not fatal)", srv.creates["bbb-healthy"])
	}
}

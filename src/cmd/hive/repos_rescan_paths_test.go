package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Tests for the remaining arms of rescanRepos (cmd/hive/repos_rescan.go):
// the success path (enumerate, publish, refresh), the fail-closed GitHub
// error path on the default work source, and the two non-GitHub work-source
// overlay arms (GitHub down but overlay keeps the scan alive; GitHub up but
// the overlay replaces the issue side and fails closed on a config error).
// The nil-client arm is covered in repos_rescan_test.go.

// rescanWire mirrors the minimal wire shapes go-github decodes for the two
// list endpoints rescanRepos's enumeration touches.
type rescanWireIssue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
}

type rescanWirePR struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
}

// newRescanAPI serves the issue and PR listings for testorg/widget. A non-OK
// issuesStatus fails the issue fetch, which (with a single repo) makes the
// whole enumeration fail — the arm the work-source tests need. The catch-all
// answers the per-PR enrichment GETs with errors, which EnrichCIStatus
// tolerates by leaving mergeability unknown.
func newRescanAPI(t *testing.T, issuesStatus int, issues []rescanWireIssue, prs []rescanWirePR) *httptest.Server {
	t.Helper()
	marshal := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/testorg/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		if issuesStatus != http.StatusOK {
			w.WriteHeader(issuesStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(marshal(issues))
	})
	mux.HandleFunc("/repos/testorg/widget/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(marshal(prs))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func rescanCreatedAt() string {
	return time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
}

func rescanLogger(buf *strings.Builder) *slog.Logger {
	if buf == nil {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(buf, nil))
}

// The whole point of the Rescan button: a successful pass returns the fresh
// enumeration, publishes it to the shared lastActionable pointer the
// dashboard reads, and asks the dashboard to repaint.
func TestRescanRepos_SuccessPublishesAndRefreshes(t *testing.T) {
	srv := newRescanAPI(t, http.StatusOK,
		[]rescanWireIssue{
			{Number: 1, Title: "bug one", CreatedAt: rescanCreatedAt()},
			{Number: 2, Title: "bug two", CreatedAt: rescanCreatedAt()},
		},
		[]rescanWirePR{
			{Number: 7, Title: "fix: widget", CreatedAt: rescanCreatedAt()},
		},
	)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, rescanLogger(nil))

	var last atomic.Pointer[github.ActionableResult]
	var refreshed bool
	got, err := rescanRepos(context.Background(), &config.Config{}, ghClient, &last, func() { refreshed = true }, rescanLogger(nil))

	if err != nil {
		t.Fatalf("rescanRepos: %v", err)
	}
	if got == nil {
		t.Fatal("result = nil, want enumeration")
	}
	if got.Issues.Count != 2 {
		t.Errorf("Issues.Count = %d, want 2", got.Issues.Count)
	}
	if got.PRs.Count != 1 {
		t.Errorf("PRs.Count = %d, want 1", got.PRs.Count)
	}
	if last.Load() != got {
		t.Error("lastActionable not published with the returned enumeration")
	}
	if !refreshed {
		t.Error("refresh callback not invoked on success")
	}
}

// A nil refresh hook (no dashboard wired) must not panic the success path.
func TestRescanRepos_SuccessTolerateNilRefresh(t *testing.T) {
	srv := newRescanAPI(t, http.StatusOK, nil, nil)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, rescanLogger(nil))

	var last atomic.Pointer[github.ActionableResult]
	got, err := rescanRepos(context.Background(), &config.Config{}, ghClient, &last, nil, rescanLogger(nil))
	if err != nil {
		t.Fatalf("rescanRepos: %v", err)
	}
	if got == nil || last.Load() != got {
		t.Fatal("empty-but-successful scan must still publish its result")
	}
}

// On the default (GitHub) work source, an all-repos enumeration failure must
// abort the rescan and leave the previously published snapshot in place: a
// zero-count repaint would read as "your repos went quiet", not "GitHub is
// down".
func TestRescanRepos_GitHubErrorFailsClosedOnDefaultWorkSource(t *testing.T) {
	srv := newRescanAPI(t, http.StatusInternalServerError, nil, nil)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, rescanLogger(nil))

	prior := &github.ActionableResult{GeneratedAt: time.Now()}
	var last atomic.Pointer[github.ActionableResult]
	last.Store(prior)
	var refreshed bool

	got, err := rescanRepos(context.Background(), &config.Config{}, ghClient, &last, func() { refreshed = true }, rescanLogger(nil))

	if err == nil {
		t.Fatal("err = nil, want enumeration failure")
	}
	if got != nil {
		t.Errorf("result = %+v, want nil", got)
	}
	if last.Load() != prior {
		t.Error("failed rescan must not replace the previously published snapshot")
	}
	if refreshed {
		t.Error("failed rescan must not trigger a dashboard repaint")
	}
}

// On a non-GitHub work source, the GitHub call is only there for the PR side;
// an all-repos GitHub failure keeps the rescan alive with an empty result so
// the overlay can populate issues. Here the overlay itself has a config error
// (linear without an api_key), so it fails closed: zero issues, no error, and
// the (empty) result is still published.
func TestRescanRepos_GitHubDownNonGitHubWorkSourceContinues(t *testing.T) {
	srv := newRescanAPI(t, http.StatusInternalServerError, nil, nil)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, rescanLogger(nil))

	cfg := &config.Config{}
	cfg.Governor.WorkSource.Type = "linear"

	var logs strings.Builder
	var last atomic.Pointer[github.ActionableResult]
	var refreshed bool
	got, err := rescanRepos(context.Background(), cfg, ghClient, &last, func() { refreshed = true }, rescanLogger(&logs))

	if err != nil {
		t.Fatalf("rescanRepos: %v (a non-GitHub work source must survive a GitHub outage)", err)
	}
	if got == nil {
		t.Fatal("result = nil, want a live (if empty) enumeration")
	}
	if got.Issues.Count != 0 {
		t.Errorf("Issues.Count = %d, want 0 (overlay config error fails closed)", got.Issues.Count)
	}
	if got.GeneratedAt.IsZero() {
		t.Error("GeneratedAt not stamped on the substitute result")
	}
	if last.Load() != got {
		t.Error("surviving rescan must publish its result")
	}
	if !refreshed {
		t.Error("surviving rescan must repaint the dashboard")
	}
	for _, want := range []string{"GitHub enumeration failed", "work_source config error"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
}

// With GitHub healthy and a non-GitHub work source configured, only the issue
// side is replaced by the overlay; the PRs GitHub returned are kept for PR
// maintenance. The overlay's fail-closed arm (config error → zero issues)
// must not discard those PRs.
func TestRescanRepos_WorkSourceOverlayReplacesIssuesKeepsPRs(t *testing.T) {
	srv := newRescanAPI(t, http.StatusOK,
		[]rescanWireIssue{{Number: 1, Title: "github-side issue", CreatedAt: rescanCreatedAt()}},
		[]rescanWirePR{
			{Number: 7, Title: "fix: widget", CreatedAt: rescanCreatedAt()},
			{Number: 8, Title: "fix: gadget", CreatedAt: rescanCreatedAt()},
		},
	)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, rescanLogger(nil))

	cfg := &config.Config{}
	cfg.Governor.WorkSource.Type = "linear"

	var last atomic.Pointer[github.ActionableResult]
	got, err := rescanRepos(context.Background(), cfg, ghClient, &last, nil, rescanLogger(nil))
	if err != nil {
		t.Fatalf("rescanRepos: %v", err)
	}
	if got.Issues.Count != 0 {
		t.Errorf("Issues.Count = %d, want 0 (overlay replaces the GitHub issue side)", got.Issues.Count)
	}
	if got.PRs.Count != 2 {
		t.Errorf("PRs.Count = %d, want 2 (GitHub PRs kept for maintenance)", got.PRs.Count)
	}
}

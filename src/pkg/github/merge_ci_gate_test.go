package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ciFixture describes what the fake GitHub reports for a PR head SHA: the
// evidence the merge-request watcher's positive-confirmation gate (#6173)
// must weigh before it is allowed to call PUT /merge.
type ciFixture struct {
	// heads maps PR number to head SHA; defaultHead applies otherwise.
	heads       map[int]string
	defaultHead string
	statuses    []ciStatus
	checks      []ciCheck
	runs        []ciRun
	// branchProtectionStatus is the HTTP status for GET
	// /branches/{base}/protection. Zero means 200 protected.
	branchProtectionStatus int
	// baseUnprotected makes GitHub's required-status-checks endpoint report
	// ErrBranchNotProtected. The merge-request watcher must still merge when
	// its own CI evidence is green.
	baseUnprotected bool
	// branchStatus, when non-zero, is the HTTP status for GET
	// /branches/{branch}. It exercises fail-closed handling when the readable
	// Branch.protected field cannot be fetched.
	branchStatus int
	// mergeStatus, when non-zero, is the HTTP status PUT /merge answers with
	// (to exercise the GitHub-side refusal paths); zero merges successfully.
	mergeStatus int
}

type ciStatus struct{ context, state string }
type ciCheck struct {
	id                       int64
	appID                    int64
	name, status, conclusion string
	startedAt                string
}

// ciRun is a workflow run on the head SHA. jobs is what GET .../jobs reports
// as total_count: zero models a startup failure (or a run still queued with
// no job created yet), which emits NO check run.
type ciRun struct {
	id                        int64
	workflowID                int64
	name, status, conc, event string
	startedAt                 string
	jobs                      int
}

func greenFixture() *ciFixture {
	return &ciFixture{
		defaultHead: "abc",
		heads:       map[int]string{100: "abc123"},
		checks:      []ciCheck{{name: "build", status: "completed", conclusion: "success"}},
		runs:        []ciRun{{id: 1, name: "CI", status: "completed", conc: "success", jobs: 1}},
	}
}

// serveCI answers the read-side endpoints the gate consults. It returns false
// for anything it does not handle so callers can layer their own routes.
func (f *ciFixture) serveCI(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	enc := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	switch {
	case strings.Contains(p, "/pulls/") && strings.Count(p, "/") == 5: // /repos/{o}/{r}/pulls/{n}
		n, _ := strconv.Atoi(p[strings.LastIndex(p, "/")+1:])
		head := f.defaultHead
		if h, ok := f.heads[n]; ok {
			head = h
		}
		enc(map[string]any{"number": n, "state": "open", "head": map[string]any{"sha": head}, "base": map[string]any{"ref": "main"}})
	case strings.Contains(p, "/branches/") && !strings.Contains(p, "/protection"): // /repos/{o}/{r}/branches/{branch}
		if f.branchStatus != 0 {
			w.WriteHeader(f.branchStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "branch unavailable"})
			return true
		}
		enc(map[string]any{"name": p[strings.LastIndex(p, "/")+1:], "protected": !f.baseUnprotected})
	case strings.HasSuffix(p, "/status"):
		statuses := []map[string]string{}
		for _, s := range f.statuses {
			statuses = append(statuses, map[string]string{"context": s.context, "state": s.state})
		}
		enc(map[string]any{"state": "pending", "total_count": len(statuses), "statuses": statuses})
	case strings.HasSuffix(p, "/check-runs"):
		runs := []map[string]any{}
		for _, c := range f.checks {
			run := map[string]any{"id": c.id, "name": c.name, "status": c.status, "conclusion": c.conclusion}
			if c.appID != 0 {
				run["app"] = map[string]any{"id": c.appID}
			}
			if c.startedAt != "" {
				run["started_at"] = c.startedAt
			}
			runs = append(runs, run)
		}
		enc(map[string]any{"total_count": len(runs), "check_runs": runs})
	case strings.HasSuffix(p, "/actions/runs"):
		runs := []map[string]any{}
		for _, run := range f.runs {
			event := run.event
			if event == "" {
				event = "pull_request"
			}
			workflowID := run.workflowID
			if workflowID == 0 {
				workflowID = run.id
			}
			item := map[string]any{"id": run.id, "workflow_id": workflowID, "name": run.name, "status": run.status, "conclusion": run.conc, "event": event, "head_sha": r.URL.Query().Get("head_sha")}
			if run.startedAt != "" {
				item["run_started_at"] = run.startedAt
			}
			runs = append(runs, item)
		}
		enc(map[string]any{"total_count": len(runs), "workflow_runs": runs})
	case strings.HasSuffix(p, "/branches/main/protection"):
		if f.baseUnprotected {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "Branch not protected"})
			return true
		}
		if f.branchProtectionStatus != 0 {
			w.WriteHeader(f.branchProtectionStatus)
			message := "branch protection unavailable"
			if f.branchProtectionStatus == http.StatusNotFound {
				message = "Branch not protected"
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
			return true
		}
		enc(map[string]any{"url": "https://api.github.test/repos/o/r/branches/main/protection"})
	case strings.HasSuffix(p, "/jobs"):
		parts := strings.Split(p, "/")
		id, _ := strconv.ParseInt(parts[len(parts)-2], 10, 64)
		for _, run := range f.runs {
			if run.id == id {
				enc(map[string]any{"total_count": run.jobs, "jobs": []any{}})
				return true
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case strings.HasSuffix(p, "/required_status_checks"):
		// Unresolvable required set: exercises the allowlist fallback.
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"Not Found"}`)
	default:
		return false
	}
	return true
}

// ciGateServer is a fake GitHub with the fixture's CI state and a counting
// PUT /merge. merges is the number of merges GitHub actually performed: the
// invariant every refusal test asserts on is that it stays zero.
func ciGateServer(t *testing.T, f *ciFixture, merges *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.serveCI(w, r) {
			return
		}
		switch {
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge"):
			if f.mergeStatus != 0 {
				w.WriteHeader(f.mergeStatus)
				_, _ = io.WriteString(w, `{"message":"Pull Request is not mergeable"}`)
				return
			}
			merges.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true,"message":"Pull Request successfully merged"}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/update-branch"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"message":"Updating pull request branch."}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

var fixedNow = func() time.Time { return time.Unix(1700000000, 0) }

// driveToExhaustion runs the handler mergeRequestMaxAttempts times against
// one request file and returns the number of re-engage hook calls.
func driveToExhaustion(t *testing.T, c *Client, reqPath string) int32 {
	t.Helper()
	var hook atomic.Int32
	c.SetMergeReEngageHook(func(string, int) bool { hook.Add(1); return true })
	for i := 0; i < mergeRequestMaxAttempts; i++ {
		c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
	}
	return hook.Load()
}

func mustExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: expected %s to exist: %v", what, path, err)
	}
}

func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: expected %s to be gone (err=%v)", what, path, err)
	}
}

// The invariant (#6173): with ZERO evidence on the head SHA the watcher must
// not merge. Not "warn and merge" - the PUT never happens, the request is
// refused, and after the retry budget it is quarantined without re-engaging
// the fix loop (there is no red check for an agent to fix).
func TestMergeCIGate_ZeroCheckRunsRefuses(t *testing.T) {
	f := &ciFixture{defaultHead: "abc"} // no statuses, no check runs, no workflow runs
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if got := merges.Load(); got != 0 {
		t.Fatalf("INVARIANT VIOLATED: PR with zero check runs was merged (%d PUT /merge calls)", got)
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || resp.Attempts != 1 || !strings.Contains(resp.Error, "absent CI is not passing") {
		t.Fatalf("expected a refused attempt naming absent CI, got %+v", resp)
	}
	if hooks := driveToExhaustion(t, c, reqPath); hooks != 0 {
		t.Fatalf("no-CI refusal must not re-engage the fix loop, got %d calls", hooks)
	}
	if got := merges.Load(); got != 0 {
		t.Fatalf("INVARIANT VIOLATED on retry: %d merges", got)
	}
	mustExist(t, reqPath+".exhausted", "unverified request after retry budget")
	mustNotExist(t, reqPath, "live request after exhaustion")
}

// All queued: the watcher WAITS. No merge, no attempt consumed, the request
// file stays live so the next tick re-evaluates it.
func TestMergeCIGate_AllQueuedWaits(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", checks: []ciCheck{{name: "build", status: "queued"}, {name: "test", status: "in_progress"}}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	var hook atomic.Int32
	c.SetMergeReEngageHook(func(string, int) bool { hook.Add(1); return true })
	const ticks = mergeRequestMaxAttempts + 2
	for i := 0; i < ticks; i++ {
		c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
	}

	if got := merges.Load(); got != 0 {
		t.Fatalf("INVARIANT VIOLATED: PR with only queued checks was merged (%d)", got)
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || resp.Attempts != 0 || resp.CIWaits != ticks || !strings.Contains(resp.Error, "still running") {
		t.Fatalf("pending CI must park the request without consuming attempts, got %+v", resp)
	}
	mustExist(t, reqPath, "request parked on pending CI")
	mustNotExist(t, reqPath+".exhausted", "pending request must not be quarantined")
	if hook.Load() != 0 {
		t.Fatalf("pending CI must not re-engage the fix loop")
	}

	// Once CI reports green on a later tick the same request merges.
	f.checks = []ciCheck{{name: "build", status: "completed", conclusion: "success"}, {name: "test", status: "completed", conclusion: "success"}}
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
	if got := merges.Load(); got != 1 {
		t.Fatalf("green CI after a wait should merge exactly once, got %d", got)
	}
	mustNotExist(t, reqPath, "request consumed after merge")
}

// The pending budget is finite: after mergeRequestMaxCIWaits consecutive
// waits the request becomes a failed attempt instead of parking forever.
func TestMergeCIGate_PendingBudgetExhausts(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", checks: []ciCheck{{name: "build", status: "queued"}}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.writeMergeResult(reqPath, MergeResponse{Number: 42, CIWaits: mergeRequestMaxCIWaits - 1, At: "x"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	resp := readMergeResult(t, reqPath)
	if resp.OK || resp.Attempts != 1 || !strings.Contains(resp.Error, "still pending after") {
		t.Fatalf("exceeding the wait budget must count as a failed attempt, got %+v", resp)
	}
	if merges.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: budget exhaustion merged the PR")
	}
}

// Mixed: one green, one red. Refused, and because the blocker IS a failed
// check, the terminal attempt re-engages the fix loop exactly as a
// branch-protection refusal would.
func TestMergeCIGate_MixedSuccessAndFailureRefuses(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", checks: []ciCheck{{name: "build", status: "completed", conclusion: "success"}, {name: "test", status: "completed", conclusion: "failure"}}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	hooks := driveToExhaustion(t, c, reqPath)

	if got := merges.Load(); got != 0 {
		t.Fatalf("INVARIANT VIOLATED: PR with a failed check was merged (%d)", got)
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || resp.Attempts != mergeRequestMaxAttempts || !strings.Contains(resp.Error, "check-failure") {
		t.Fatalf("expected refusal naming the failed check, got %+v", resp)
	}
	if !isRequiredCheckMergeBlocker(resp.Error) {
		t.Fatalf("a red-CI refusal must classify as a required-check blocker so the fix loop re-engages: %q", resp.Error)
	}
	if hooks != 1 {
		t.Fatalf("red CI should re-engage the fix loop once at the terminal attempt, got %d", hooks)
	}
	mustExist(t, reqPath+".exhausted", "red request after retry budget")
}

// All success: positive confirmation, the merge proceeds and the request is
// consumed. Non-gating noise (a cancelled Playwright shard, a pending meta
// status) does not block, preserving the existing ignore list.
func TestMergeCIGate_AllSuccessMerges(t *testing.T) {
	f := greenFixture()
	f.checks = append(f.checks, ciCheck{name: "Playwright", status: "completed", conclusion: "cancelled"}, ciCheck{name: "lint", status: "completed", conclusion: "skipped"})
	f.statuses = []ciStatus{{"tide", "pending"}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if got := merges.Load(); got != 1 {
		t.Fatalf("all-green PR should merge exactly once, got %d", got)
	}
	resp := readMergeResult(t, reqPath)
	if !resp.OK || resp.SHA != "deadbeef" || resp.Attempts != 1 {
		t.Fatalf("expected ok result, got %+v", resp)
	}
	mustNotExist(t, reqPath, "request consumed after merge")
}

func TestMergeCIGate_DedupesSupersededCheckRuns(t *testing.T) {
	const (
		older = "2026-09-27T22:44:00Z"
		later = "2026-09-27T22:45:01Z"
	)
	tests := []struct {
		name       string
		checks     []ciCheck
		wantMerge  bool
		wantReason string
	}{
		{
			name: "superseded cancelled latest success passes",
			checks: []ciCheck{
				{id: 108724704279, appID: 15368, name: "changelog-fragment-guard", status: "completed", conclusion: "cancelled", startedAt: older},
				{id: 108724738287, appID: 15368, name: "changelog-fragment-guard", status: "completed", conclusion: "success", startedAt: later},
			},
			wantMerge: true,
		},
		{
			name: "latest cancelled still fails",
			checks: []ciCheck{
				{id: 108724704279, appID: 15368, name: "changelog-fragment-guard", status: "completed", conclusion: "success", startedAt: older},
				{id: 108724738287, appID: 15368, name: "changelog-fragment-guard", status: "completed", conclusion: "cancelled", startedAt: later},
			},
			wantReason: "check-cancelled",
		},
		{
			name: "different names remain independent",
			checks: []ciCheck{
				{id: 1, appID: 15368, name: "build", status: "completed", conclusion: "success", startedAt: later},
				{id: 2, appID: 15368, name: "lint", status: "completed", conclusion: "cancelled", startedAt: later},
			},
			wantReason: "check-cancelled",
		},
		{
			name: "same start time tiebreaks by highest id",
			checks: []ciCheck{
				{id: 10, appID: 15368, name: "build", status: "completed", conclusion: "cancelled", startedAt: later},
				{id: 11, appID: 15368, name: "build", status: "completed", conclusion: "success", startedAt: later},
			},
			wantMerge: true,
		},
		{
			name: "same name different app remains independent",
			checks: []ciCheck{
				{id: 10, appID: 1, name: "build", status: "completed", conclusion: "success", startedAt: later},
				{id: 11, appID: 2, name: "build", status: "completed", conclusion: "cancelled", startedAt: later},
			},
			wantReason: "check-cancelled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &ciFixture{defaultHead: "abc", checks: tt.checks}
			var merges atomic.Int32
			srv := ciGateServer(t, f, &merges)
			defer srv.Close()
			c := testMergeClient(t, srv.URL)

			reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
			c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

			if tt.wantMerge {
				if got := merges.Load(); got != 1 {
					t.Fatalf("expected merge, got %d (%+v)", got, readMergeResult(t, reqPath))
				}
				return
			}
			if got := merges.Load(); got != 0 {
				t.Fatalf("expected no merge, got %d", got)
			}
			if resp := readMergeResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, tt.wantReason) {
				t.Fatalf("expected refusal containing %q, got %+v", tt.wantReason, resp)
			}
		})
	}
}

func TestMergeRequestBaseProtection_UnprotectedMergesWithGreenCI(t *testing.T) {
	f := greenFixture()
	f.baseUnprotected = true
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 1 {
		t.Fatalf("unprotected base with green CI should merge, got %d merges (result=%+v)", merges.Load(), readMergeResult(t, reqPath))
	}
	resp := readMergeResult(t, reqPath)
	if !resp.OK {
		t.Fatalf("expected successful merge, got %+v", resp)
	}
	mustNotExist(t, reqPath, "request consumed after merge")
}

func TestMergeRequestBaseProtection_ProtectedMergesWithGreenCI(t *testing.T) {
	f := greenFixture()
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 1 {
		t.Fatalf("protected base with green CI should merge, got %d merges (result=%+v)", merges.Load(), readMergeResult(t, reqPath))
	}
	resp := readMergeResult(t, reqPath)
	if !resp.OK {
		t.Fatalf("expected successful merge, got %+v", resp)
	}
}

func TestMergeRequestBaseProtection_DeprecatedAllowlistDoesNotOverrideFailingCI(t *testing.T) {
	f := &ciFixture{
		defaultHead:     "abc",
		checks:          []ciCheck{{name: "build", status: "completed", conclusion: "failure"}},
		baseUnprotected: true,
	}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetMergeRequestAllowUnprotectedBaseRepos(map[string]bool{"o/r": true})

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: deprecated allow_unprotected_base overrode failing CI")
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, "check-failure") {
		t.Fatalf("expected failing check refusal, got %+v", resp)
	}
}

// The production shape from #6173: every workflow concluded failure with
// zero jobs (startup failures), so the commit has zero check runs and an
// empty status rollup. A check-run-only gate sees nothing; this gate sees
// the failed runs and refuses.
func TestMergeCIGate_ZeroJobWorkflowFailureRefuses(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", runs: []ciRun{
		{id: 11, name: "CI", status: "completed", conc: "failure", jobs: 0},
		{id: 12, name: "Lint", status: "completed", conc: "failure", jobs: 0},
	}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if got := merges.Load(); got != 0 {
		t.Fatalf("INVARIANT VIOLATED: startup-failed workflows with zero check runs were merged (%d)", got)
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, "without producing a job") || !strings.Contains(resp.Error, `"CI"(11)`) {
		t.Fatalf("expected refusal naming the zero-job failed run, got %+v", resp)
	}
}

func TestMergeCIGate_DedupesSupersededWorkflowRuns(t *testing.T) {
	const (
		older = "2026-09-27T22:44:00Z"
		later = "2026-09-27T22:45:01Z"
	)
	tests := []struct {
		name       string
		runs       []ciRun
		wantMerge  bool
		wantReason string
	}{
		{
			name: "superseded failure latest success passes",
			runs: []ciRun{
				{id: 101, workflowID: 7, name: "CI", status: "completed", conc: "failure", event: "pull_request", startedAt: older, jobs: 0},
				{id: 102, workflowID: 7, name: "CI", status: "completed", conc: "success", event: "pull_request", startedAt: later, jobs: 0},
			},
			wantMerge: true,
		},
		{
			name: "latest failure still fails",
			runs: []ciRun{
				{id: 101, workflowID: 7, name: "CI", status: "completed", conc: "success", event: "pull_request", startedAt: older, jobs: 0},
				{id: 102, workflowID: 7, name: "CI", status: "completed", conc: "failure", event: "pull_request", startedAt: later, jobs: 0},
			},
			wantReason: `"CI"(102)`,
		},
		{
			name: "same start time tiebreaks by highest id",
			runs: []ciRun{
				{id: 101, workflowID: 7, name: "CI", status: "completed", conc: "failure", event: "pull_request", startedAt: later, jobs: 0},
				{id: 102, workflowID: 7, name: "CI", status: "completed", conc: "success", event: "pull_request", startedAt: later, jobs: 0},
			},
			wantMerge: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &ciFixture{
				defaultHead: "abc",
				checks:      []ciCheck{{id: 1, appID: 1, name: "build", status: "completed", conclusion: "success", startedAt: later}},
				runs:        tt.runs,
			}
			var merges atomic.Int32
			srv := ciGateServer(t, f, &merges)
			defer srv.Close()
			c := testMergeClient(t, srv.URL)

			reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
			c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

			if tt.wantMerge {
				if got := merges.Load(); got != 1 {
					t.Fatalf("expected merge, got %d (%+v)", got, readMergeResult(t, reqPath))
				}
				return
			}
			if got := merges.Load(); got != 0 {
				t.Fatalf("expected no merge, got %d", got)
			}
			if resp := readMergeResult(t, reqPath); resp.OK || !strings.Contains(resp.Error, tt.wantReason) {
				t.Fatalf("expected refusal containing %q, got %+v", tt.wantReason, resp)
			}
		})
	}
}

// A failed workflow run that DID produce jobs is already visible through its
// check runs; when those check runs are non-gating (ignore list) the run's
// own conclusion must not re-block the merge, or optional suites would wedge
// the queue again.
func TestMergeCIGate_FailedRunWithJobsDefersToCheckRuns(t *testing.T) {
	f := &ciFixture{defaultHead: "abc",
		checks: []ciCheck{{name: "build", status: "completed", conclusion: "success"}, {name: "Playwright", status: "completed", conclusion: "failure"}},
		runs:   []ciRun{{id: 21, name: "Playwright", status: "completed", conc: "failure", jobs: 3}},
	}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
	if got := merges.Load(); got != 1 {
		t.Fatalf("non-gating failed suite with jobs should not block, got %d merges (%+v)", got, readMergeResult(t, reqPath))
	}
}

// A queued workflow run with no job yet is opaque: wait, do not merge, do
// not fail.
func TestMergeCIGate_QueuedZeroJobRunWaits(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", runs: []ciRun{{id: 31, name: "CI", status: "queued", jobs: 0}}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: queued run with no jobs was merged")
	}
	resp := readMergeResult(t, reqPath)
	if resp.Attempts != 0 || resp.CIWaits != 1 || !strings.Contains(resp.Error, "still in flight") {
		t.Fatalf("expected a parked wait, got %+v", resp)
	}
	mustExist(t, reqPath, "request parked on queued run")
}

// With a config-declared required set, a required check that has not been
// created at all is "expected", not passed: wait. Other green checks on the
// SHA do not substitute for it.
func TestMergeCIGate_MissingRequiredCheckWaits(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", checks: []ciCheck{{name: "lint", status: "completed", conclusion: "success"}}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetRequiredChecks(map[string]bool{"build-gate": true})

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: merged with the required check never reported")
	}
	resp := readMergeResult(t, reqPath)
	if resp.Attempts != 0 || resp.CIWaits != 1 || !strings.Contains(resp.Error, "build-gate") {
		t.Fatalf("expected a wait naming the missing required check, got %+v", resp)
	}

	f.checks = append(f.checks, ciCheck{name: "build-gate", status: "completed", conclusion: "success"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
	if merges.Load() != 1 {
		t.Fatalf("required check reporting success should unblock the merge")
	}
}

// The pinned SHA must be the PR head: CI of a different commit never
// authorizes a merge.
func TestMergeCIGate_HeadMovedRefuses(t *testing.T) {
	f := greenFixture()
	f.defaultHead = "newhead"
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: merged after the head moved")
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || resp.Attempts != 1 || !strings.Contains(resp.Error, "head moved") {
		t.Fatalf("expected head-moved refusal, got %+v", resp)
	}
}

// An API failure while gathering evidence is a failed attempt, never a pass.
func TestMergeCIGate_EvidenceAPIErrorRefuses(t *testing.T) {
	var merges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/merge") {
			merges.Add(1)
			_, _ = io.WriteString(w, `{"sha":"deadbeef","merged":true}`)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":42,"state":"open","head":{"sha":"abc"},"base":{"ref":"main"}}`)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"main","protected":true}`)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/branches/main/protection") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"url":"https://api.github.test/repos/o/r/branches/main/protection"}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: merged although CI evidence could not be read")
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || resp.Attempts != 1 || !strings.Contains(resp.Error, "ci gate") {
		t.Fatalf("expected a failed attempt from the gate, got %+v", resp)
	}
}

func TestMergeCIGate_NoCIOKOptInMergesUnverifiedRepo(t *testing.T) {
	f := &ciFixture{defaultHead: "abc"} // no statuses, no check runs, no workflow runs
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{"o/r": true})

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 1 {
		t.Fatalf("explicit no_ci_ok repo should merge absent-CI PR, got %d merges (result=%+v)", merges.Load(), readMergeResult(t, reqPath))
	}
	resp := readMergeResult(t, reqPath)
	if !resp.OK {
		t.Fatalf("expected successful merge, got %+v", resp)
	}
}

func TestMergeCIGate_NoCIOKDoesNotOverrideFailingCheck(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", checks: []ciCheck{{name: "build", status: "completed", conclusion: "failure"}}}
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetMergeRequestNoCIAllowedRepos(map[string]bool{"o/r": true})

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if merges.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: no_ci_ok overrode a failing check")
	}
	resp := readMergeResult(t, reqPath)
	if resp.OK || !strings.Contains(resp.Error, "check-failure") {
		t.Fatalf("expected failing check refusal, got %+v", resp)
	}
}

func TestMergeCIVerdictString(t *testing.T) {
	for v, want := range map[mergeCIVerdict]string{mergeCIGreen: "green", mergeCIPending: "pending", mergeCIRed: "red", mergeCIUnverified: "unverified", mergeCIVerdict(9): "mergeCIVerdict(9)"} {
		if got := v.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(v), got, want)
		}
	}
}

// The unprotected-base allowlist is now deprecated: an unprotected base with
// green CI merges without consulting auto_merge.allow_unprotected_base.
func TestMergeCIGate_UnprotectedBaseMergesWithoutAllowlist(t *testing.T) {
	f := greenFixture()
	f.baseUnprotected = true
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

	if got := merges.Load(); got != 1 {
		t.Fatalf("unprotected base with green CI should merge exactly once, got %d", got)
	}
	resp := readMergeResult(t, reqPath)
	if !resp.OK {
		t.Fatalf("expected successful merge, got %+v", resp)
	}
	mustNotExist(t, reqPath, "request consumed after merge")
}

// Existing configs may still carry auto_merge.allow_unprotected_base. It is
// accepted as a no-op and does not change the outcome.
func TestMergeCIGate_DeprecatedAllowUnprotectedBaseAcceptedAsNoop(t *testing.T) {
	for name, allow := range map[string]map[string]bool{"owner/repo form": {"o/r": true}, "bare name form": {"r": true}} {
		t.Run(name, func(t *testing.T) {
			f := greenFixture()
			f.baseUnprotected = true
			var merges atomic.Int32
			srv := ciGateServer(t, f, &merges)
			defer srv.Close()
			c := testMergeClient(t, srv.URL)
			c.SetMergeRequestPolicy(allow, nil)

			reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
			c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)

			if got := merges.Load(); got != 1 {
				t.Fatalf("allowlisted repo with green CI should merge exactly once, got %d", got)
			}
			mustNotExist(t, reqPath, "request consumed after merge")
		})
	}
}

// #6281 gate 2: the per-repo no_ci_ok opt-in downgrades ONLY the unverified
// verdict. A repo with zero CI evidence merges when opted in; the same
// opt-in never rescues a RED verdict.
func TestMergeCIGate_NoCIOKOptIn(t *testing.T) {
	f := &ciFixture{defaultHead: "abc"} // zero statuses/checks/runs, base protected
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetMergeRequestPolicy(nil, map[string]bool{"o/r": true})

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
	if got := merges.Load(); got != 1 {
		t.Fatalf("no_ci_ok repo with zero CI evidence should merge exactly once, got %d", got)
	}
	mustNotExist(t, reqPath, "request consumed after merge")

	// RED stays red: a failed check on an opted-in repo still refuses.
	f2 := &ciFixture{defaultHead: "abc", checks: []ciCheck{{name: "build", status: "completed", conclusion: "failure"}}}
	var merges2 atomic.Int32
	srv2 := ciGateServer(t, f2, &merges2)
	defer srv2.Close()
	c2 := testMergeClient(t, srv2.URL)
	c2.SetMergeRequestPolicy(nil, map[string]bool{"o/r": true})

	reqPath2, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 43, ExpectSHA: "abc", Agent: "scanner"})
	c2.handleOneMergeRequest(context.Background(), reqPath2, fixedNow)
	if merges2.Load() != 0 {
		t.Fatalf("INVARIANT VIOLATED: no_ci_ok downgraded a RED verdict to green")
	}
	resp := readMergeResult(t, reqPath2)
	if resp.OK || !strings.Contains(resp.Error, "has not succeeded") {
		t.Fatalf("expected a red refusal despite no_ci_ok, got %+v", resp)
	}
}

// #6281: a nil/cleared policy fails closed — no allowlist, no opt-in.
func TestMergeCIGate_PolicyFailsClosed(t *testing.T) {
	c := testMergeClient(t, "http://127.0.0.1:0")
	if c.repoAllowsUnprotectedBase("o", "r") || c.repoNoCIOK("o", "r") {
		t.Fatalf("uninstalled merge policy must deny everything")
	}
	c.SetMergeRequestPolicy(map[string]bool{"o/r": true}, map[string]bool{"r": true})
	if !c.repoAllowsUnprotectedBase("o", "r") || !c.repoNoCIOK("o", "r") {
		t.Fatalf("installed merge policy should match o/r")
	}
	c.SetMergeRequestPolicy(nil, nil)
	if c.repoAllowsUnprotectedBase("o", "r") || c.repoNoCIOK("o", "r") {
		t.Fatalf("cleared merge policy must deny everything again")
	}
	var nilClient *Client
	nilClient.SetMergeRequestPolicy(map[string]bool{"o/r": true}, nil) // must not panic
	if nilClient.repoAllowsUnprotectedBase("o", "r") || nilClient.repoNoCIOK("o", "r") {
		t.Fatalf("nil client must deny everything")
	}
}

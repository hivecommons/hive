package mergelane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghub "github.com/hivecommons/hive/pkg/github"
)

func (h *harness) gate(pr int) ghub.LaneMergeResult {
	h.t.Helper()
	res, err := h.lane.Merge(context.Background(), ghub.LaneMergeRequest{
		Repo: testRepo, Branch: testBranch, Number: pr, Path: ghub.PRAuditPathSweep, Authorize: h.authorize,
	})
	if err != nil {
		h.t.Fatalf("Merge(#%d): %v", pr, err)
	}
	return res
}

func expectGate(t *testing.T, res ghub.LaneMergeResult, want, reasonContains string) {
	t.Helper()
	if res.Outcome != want || !strings.Contains(res.Reason, reasonContains) {
		t.Fatalf("gate = %+v, want outcome %q with reason containing %q", res, want, reasonContains)
	}
}

func newGateHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.f.required.MergeQueueKnown = true
	return h
}

// AC6 through the gate the merge paths call: with two green eligible PRs
// into one branch at most one is in final validation; the second is deferred
// without any GitHub write, and merges only after the first merged and it
// was brought up to date and re-passed on the new tip.
func TestGateMergesTwoGreenPRsOneAtATime(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1")
	h.f.addPR(2, "h2")
	h.f.setRun("h1", "build", "in_progress", "")

	expectGate(t, h.gate(1), ghub.LaneOutcomeWaiting, "build (in_progress)")
	expectGate(t, h.gate(2), ghub.LaneOutcomeDeferred, ReasonNotAtFront)
	if h.f.count("Merge") != 0 || h.f.count("UpdateBranch") != 0 {
		t.Fatalf("no merge or update before the front passes, calls = %v", h.f.calls)
	}
	if h.frontPR() != 1 {
		t.Fatalf("front = #%d, want #1", h.frontPR())
	}

	h.f.pass("h1")
	res := h.gate(1)
	expectGate(t, res, ghub.LaneOutcomeMerged, "merged")
	if !res.Merged() || res.SHA == "" || res.SHA != h.f.tip {
		t.Fatalf("merge result = %+v, tip = %q", res, h.f.tip)
	}

	expectGate(t, h.gate(2), ghub.LaneOutcomeUpdated, "merged the tip in")
	newHead := h.f.prs[2].Head
	expectGate(t, h.gate(2), ghub.LaneOutcomeWaiting, "build (missing)")
	h.f.pass(newHead)
	expectGate(t, h.gate(2), ghub.LaneOutcomeMerged, "")
	if want := []string{"1:h1", "2:" + newHead}; fmt.Sprint(h.f.merges) != fmt.Sprint(want) {
		t.Fatalf("merges = %v, want %v", h.f.merges, want)
	}
}

// AC26: a branch with GitHub's native merge queue is left alone; the lane
// does not run, nothing is updated or merged, a PR that held the front is
// released and the refusal is audited with its reason.
func TestGateRefusesNativeMergeQueueBranch(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1")
	h.f.setRun("h1", "build", "queued", "")
	expectGate(t, h.gate(1), ghub.LaneOutcomeWaiting, "build (queued)")

	h.f.required.MergeQueue = true
	h.f.pass("h1")
	expectGate(t, h.gate(1), ghub.LaneOutcomeRefused, NativeMergeQueueReason(testBranch))
	if h.f.count("Merge") != 0 || h.f.count("UpdateBranch") != 0 {
		t.Fatalf("no direct merge into a merge-queue branch, calls = %v", h.f.calls)
	}
	if h.frontPR() != 0 {
		t.Fatalf("front = #%d, want the lane released", h.frontPR())
	}
	found := false
	for _, e := range h.eventsFor(ActionRefusal) {
		if e.PR == 1 && strings.Contains(e.Reason, "native merge queue") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no audited refusal naming the native merge queue in %+v", h.events)
	}
}

// Fail closed when the branch rules cannot tell whether a native merge queue
// is on.
func TestGateRefusesWhenMergeQueueUnknown(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1")
	h.f.required.MergeQueueKnown = false
	h.f.required.Reason = "branch rulesets unreadable: boom"
	expectGate(t, h.gate(1), ghub.LaneOutcomeRefused, "branch rulesets unreadable")
	h.f.required.Reason = ""
	expectGate(t, h.gate(1), ghub.LaneOutcomeRefused, "branch rules unreadable")
	if h.f.count("Merge") != 0 {
		t.Fatalf("no merge when the merge queue is unknown, calls = %v", h.f.calls)
	}
}

// A path that does not know the target branch (the relay) lets the lane read
// it from the PR; a failed read waits for the next round.
func TestGateResolvesBranchFromThePullRequest(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1")
	res, err := h.lane.Merge(context.Background(), ghub.LaneMergeRequest{Repo: testRepo, Number: 1, Path: ghub.PRAuditPathRelay, Authorize: h.authorize})
	if err != nil || !res.Merged() {
		t.Fatalf("Merge = %+v, %v; want merged into the PR's base", res, err)
	}

	h.f.addPR(2, "h2")
	h.f.errs["PullRequest"] = errBoom
	res, err = h.lane.Merge(context.Background(), ghub.LaneMergeRequest{Repo: testRepo, Number: 2, Authorize: h.authorize})
	if err != nil || res.Outcome != ghub.LaneOutcomeWaiting || !strings.Contains(res.Reason, "boom") {
		t.Fatalf("Merge = %+v, %v; want waiting on a failed PR read", res, err)
	}

	if _, err := h.lane.Merge(context.Background(), ghub.LaneMergeRequest{Repo: testRepo, Branch: testBranch}); err == nil {
		t.Fatal("a request without a PR number must be refused")
	}
}

// The path's authorization is re-checked at the final re-check: a nil or
// failing authorizer means no merge (AC17).
func TestGatePathAuthorizationIsRechecked(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1")
	res, err := h.lane.Merge(context.Background(), ghub.LaneMergeRequest{Repo: testRepo, Branch: testBranch, Number: 1})
	if err != nil || res.Outcome != ghub.LaneOutcomeWaiting || !strings.Contains(res.Reason, "no merge-path authorization") {
		t.Fatalf("nil authorizer: %+v, %v", res, err)
	}
	h.authErr = errors.New("lgtm approval covers another head")
	expectGate(t, h.gate(1), ghub.LaneOutcomeWaiting, "lgtm approval covers another head")
	if h.f.count("Merge") != 0 {
		t.Fatalf("no merge without the path's authorization, calls = %v", h.f.calls)
	}
}

// AC4: the strategy never turns merging on; auto-merge off or a pause stop
// the lane too, after every path's own checks.
func TestGateNeverMergesWhenAutoMergeOffOrPaused(t *testing.T) {
	h := newGateHarness(t)
	h.f.addPR(1, "h1")
	h.autoMerge = false
	expectGate(t, h.gate(1), ghub.LaneOutcomeLeft, "auto-merge is not allowed")
	h.autoMerge = true
	h.paused = true
	expectGate(t, h.gate(1), ghub.LaneOutcomeLeft, "paused")
	h.paused = false
	h.f.prs[1].Labels = []string{"hold"}
	expectGate(t, h.gate(1), ghub.LaneOutcomeLeft, "hold")
	h.strategy = config.MergeStrategyDirect
	h.f.prs[1].Labels = nil
	expectGate(t, h.gate(1), ghub.LaneOutcomeLeft, "validation ended without a merge")
	if h.f.count("Merge") != 0 {
		t.Fatalf("no merge, calls = %v", h.f.calls)
	}
}

// restAPI serves the GitHub endpoints RESTGitHub calls for acme/widgets.
func restAPI(t *testing.T, mergeBodies *[]map[string]any) *httptest.Server {
	t.Helper()
	enc := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := strings.TrimPrefix(r.URL.Path, "/api/v3")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(p, "/repos/acme/widgets/pulls/"):
			n := strings.TrimPrefix(p, "/repos/acme/widgets/pulls/")
			enc(w, map[string]any{
				"number": 1, "state": "open", "draft": false, "merged": false,
				"mergeable_state": "clean", "mergeable": true, "title": "PR " + n,
				"head":   map[string]any{"sha": "h1", "ref": "feature"},
				"base":   map[string]any{"ref": "main"},
				"labels": []map[string]string{{"name": "hold"}},
			})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/branches/main":
			enc(w, map[string]any{"name": "main", "commit": map[string]any{"sha": "t0"}})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/branches/empty":
			enc(w, map[string]any{"name": "empty"})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/compare/t0...h1":
			enc(w, map[string]any{"status": "ahead", "ahead_by": 1, "behind_by": 0})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/compare/t1...h1":
			enc(w, map[string]any{"status": "diverged", "ahead_by": 1, "behind_by": 2})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/branches/main/protection/required_status_checks":
			w.WriteHeader(http.StatusNotFound)
			enc(w, map[string]any{"message": "Branch not protected"})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/rules/branches/main":
			enc(w, []map[string]any{})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/commits/h1/check-runs":
			enc(w, map[string]any{"total_count": 2, "check_runs": []map[string]any{
				{"name": "build", "status": "completed", "conclusion": "success"},
				{"name": "build", "status": "completed", "conclusion": "failure"},
			}})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/commits/h1/status":
			enc(w, map[string]any{"state": "failure", "statuses": []map[string]any{
				{"context": "build", "state": "failure"},
				{"context": "lint", "state": "success"},
				{"context": "sec", "state": "error"},
				{"context": "e2e", "state": "pending"},
			}})
		case r.Method == http.MethodPut && p == "/repos/acme/widgets/pulls/1/update-branch":
			w.WriteHeader(http.StatusAccepted)
			enc(w, map[string]any{"message": "Updating pull request branch."})
		case r.Method == http.MethodPut && p == "/repos/acme/widgets/pulls/2/update-branch":
			w.WriteHeader(http.StatusUnprocessableEntity)
			enc(w, map[string]any{"message": "expected head sha didn't match current head ref"})
		case r.Method == http.MethodPut && strings.HasSuffix(p, "/merge"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*mergeBodies = append(*mergeBodies, body)
			switch p {
			case "/repos/acme/widgets/pulls/1/merge":
				enc(w, map[string]any{"merged": true, "sha": "m1"})
			case "/repos/acme/widgets/pulls/2/merge":
				w.WriteHeader(http.StatusConflict)
				enc(w, map[string]any{"message": "Head branch was modified"})
			default:
				enc(w, map[string]any{"merged": false, "message": "not applied"})
			}
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/git/commits/m1":
			enc(w, map[string]any{"sha": "m1", "parents": []map[string]any{{"sha": "t0"}, {"sha": "h1"}}})
		case r.Method == http.MethodGet && p == "/repos/acme/widgets/commits/t9/pulls":
			enc(w, []map[string]any{{"number": 5}})
		default:
			w.WriteHeader(http.StatusNotFound)
			enc(w, map[string]any{"message": "Not Found"})
		}
	}))
}

// RESTGitHub is the lane's live GitHub over the hive's REST client.
func TestRESTGitHubAdapter(t *testing.T) {
	var mergeBodies []map[string]any
	api := restAPI(t, &mergeBodies)
	defer api.Close()
	client := ghub.NewClient("token", "acme", []string{"widgets"}, nil, api.URL)
	g := &RESTGitHub{
		Client:               func() *ghub.Client { return client },
		ConfigRequiredChecks: func() (map[string]bool, bool) { return map[string]bool{"build": true}, true },
	}
	ctx := context.Background()

	p, err := g.PullRequest(ctx, testRepo, 1)
	if err != nil || p.Head != "h1" || p.Base != "main" || !p.Open || p.MergeableState != "clean" || len(p.Labels) != 1 {
		t.Fatalf("PullRequest = %+v, %v", p, err)
	}
	if tip, err := g.BranchTip(ctx, testRepo, "main"); err != nil || tip != "t0" {
		t.Fatalf("BranchTip = %q, %v", tip, err)
	}
	if _, err := g.BranchTip(ctx, testRepo, "empty"); err == nil {
		t.Fatal("a branch without a tip commit must be an error")
	}
	if _, err := g.BranchTip(ctx, testRepo, "gone"); err == nil {
		t.Fatal("a missing branch must be an error")
	}
	if ok, err := g.HeadContains(ctx, testRepo, "t0", "h1"); err != nil || !ok {
		t.Fatalf("HeadContains(t0, h1) = %v, %v; want contained", ok, err)
	}
	if ok, err := g.HeadContains(ctx, testRepo, "t1", "h1"); err != nil || ok {
		t.Fatalf("HeadContains(t1, h1) = %v, %v; want stale", ok, err)
	}
	if _, err := g.HeadContains(ctx, testRepo, "x", "y"); err == nil {
		t.Fatal("a failed compare must be an error")
	}
	rules := g.RequiredChecks(ctx, testRepo, "main")
	if !rules.Known || !rules.Required["build"] || !rules.MergeQueueKnown || rules.MergeQueue {
		t.Fatalf("RequiredChecks = %+v", rules)
	}

	runs, err := g.CheckRuns(ctx, testRepo, "h1")
	if err != nil {
		t.Fatalf("CheckRuns: %v", err)
	}
	want := map[string]CheckRun{
		"build": {Status: "completed", Conclusion: "success"},
		"lint":  {Status: "completed", Conclusion: "success"},
		"sec":   {Status: "completed", Conclusion: "failure"},
		"e2e":   {Status: "pending"},
	}
	if fmt.Sprint(runs) != fmt.Sprint(want) {
		t.Fatalf("CheckRuns = %v, want %v", runs, want)
	}
	if _, err := g.CheckRuns(ctx, testRepo, "nope"); err == nil {
		t.Fatal("a failed check-run read must be an error")
	}

	if err := g.UpdateBranch(ctx, testRepo, 1, "h1"); err != nil {
		t.Fatalf("UpdateBranch accepted: %v", err)
	}
	if err := g.UpdateBranch(ctx, testRepo, 2, "h2"); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("UpdateBranch on a moved head = %v, want ErrHeadMoved", err)
	}
	if err := g.UpdateBranch(ctx, testRepo, 3, "h3"); err == nil || errors.Is(err, ErrHeadMoved) {
		t.Fatalf("UpdateBranch failure = %v, want a plain error", err)
	}

	method := &mergeMethod{requested: "merge"}
	sha, err := g.Merge(context.WithValue(ctx, mergeMethodKey{}, method), testRepo, 1, "h1")
	if err != nil || sha != "m1" || method.used != "merge" {
		t.Fatalf("Merge = %q, %v, used %q", sha, err, method.used)
	}
	if got := mergeBodies[0]; got["sha"] != "h1" || got["merge_method"] != "merge" {
		t.Fatalf("merge body = %v, want head pinned with the path's method", got)
	}
	if _, err := g.Merge(ctx, testRepo, 2, "h2"); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("Merge on a moved head = %v, want ErrHeadMoved", err)
	}
	if got := mergeBodies[1]; got["merge_method"] != "squash" {
		t.Fatalf("default merge method = %v, want squash", got["merge_method"])
	}
	if _, err := g.Merge(ctx, testRepo, 3, "h3"); err == nil {
		t.Fatal("an unapplied merge must be an error")
	}

	if parents, err := g.CommitParents(ctx, testRepo, "m1"); err != nil || fmt.Sprint(parents) != "[t0 h1]" {
		t.Fatalf("CommitParents = %v, %v", parents, err)
	}
	if _, err := g.CommitParents(ctx, testRepo, "zz"); err == nil {
		t.Fatal("a failed commit read must be an error")
	}
	if prs, err := g.PullRequestsForCommit(ctx, testRepo, "t9"); err != nil || fmt.Sprint(prs) != "[5]" {
		t.Fatalf("PullRequestsForCommit = %v, %v", prs, err)
	}
	if _, err := g.PullRequestsForCommit(ctx, testRepo, "zz"); err == nil {
		t.Fatal("a failed PR lookup must be an error")
	}
}

// Without a client every call fails closed.
func TestRESTGitHubWithoutClient(t *testing.T) {
	ctx := context.Background()
	for _, g := range []*RESTGitHub{nil, {}, {Client: func() *ghub.Client { return nil }}} {
		if _, err := g.PullRequest(ctx, testRepo, 1); !errors.Is(err, ghub.ErrNoGitHubClient) {
			t.Fatalf("PullRequest err = %v", err)
		}
		if _, err := g.BranchTip(ctx, testRepo, "main"); err == nil {
			t.Fatal("BranchTip without a client")
		}
		if _, err := g.HeadContains(ctx, testRepo, "a", "b"); err == nil {
			t.Fatal("HeadContains without a client")
		}
		if rules := g.RequiredChecks(ctx, testRepo, "main"); rules.Known || rules.MergeQueueKnown || rules.Reason == "" {
			t.Fatalf("RequiredChecks without a client = %+v", rules)
		}
		if _, err := g.CheckRuns(ctx, testRepo, "h"); err == nil {
			t.Fatal("CheckRuns without a client")
		}
		if err := g.UpdateBranch(ctx, testRepo, 1, "h"); err == nil {
			t.Fatal("UpdateBranch without a client")
		}
		if _, err := g.Merge(ctx, testRepo, 1, "h"); err == nil {
			t.Fatal("Merge without a client")
		}
		if _, err := g.CommitParents(ctx, testRepo, "h"); err == nil {
			t.Fatal("CommitParents without a client")
		}
		if _, err := g.PullRequestsForCommit(ctx, testRepo, "h"); err == nil {
			t.Fatal("PullRequestsForCommit without a client")
		}
	}
}

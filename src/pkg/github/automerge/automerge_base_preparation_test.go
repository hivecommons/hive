package automerge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

type preparationAPI struct {
	mu      sync.Mutex
	prs     map[int]*gh.PullRequest
	behind  map[string]int
	pending map[string]bool
	updates map[int]int
	merges  map[int]int
}

func preparationPR(number int, base, head string) *gh.PullRequest {
	return &gh.PullRequest{
		Number: gh.Ptr(number), State: gh.Ptr("open"), Title: gh.Ptr("prepare fixture"),
		Mergeable: gh.Ptr(true), MergeableState: gh.Ptr("clean"),
		User: &gh.User{Login: gh.Ptr(testHiveAppBotLogin)},
		Head: &gh.PullRequestBranch{SHA: gh.Ptr(head), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
		Base: &gh.PullRequestBranch{Ref: gh.Ptr(base), SHA: gh.Ptr("base1"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
}

func newPreparationAPI(t *testing.T) (*preparationAPI, *httptest.Server) {
	t.Helper()
	api := &preparationAPI{
		prs: map[int]*gh.PullRequest{}, behind: map[string]int{}, pending: map[string]bool{},
		updates: map[int]int{}, merges: map[int]int{},
	}
	for n := 1; n <= 3; n++ {
		head := fmt.Sprintf("head%d", n)
		api.prs[n] = preparationPR(n, "main", head)
		api.behind[head] = 1
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.mu.Lock()
		defer api.mu.Unlock()
		path := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(path) < 4 || path[0] != "repos" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch path[3] {
		case "pulls":
			n, _ := strconv.Atoi(path[4])
			if len(path) == 5 {
				if api.prs[n] == nil {
					w.WriteHeader(http.StatusNotFound)
				} else {
					write(api.prs[n])
				}
				return
			}
			switch path[5] {
			case "update-branch":
				var opts gh.PullRequestBranchUpdateOptions
				_ = json.NewDecoder(r.Body).Decode(&opts)
				if opts.GetExpectedHeadSHA() != api.prs[n].GetHead().GetSHA() {
					w.WriteHeader(http.StatusUnprocessableEntity)
					write(map[string]string{"message": "expected_head_sha does not match"})
					return
				}
				api.updates[n]++
				w.WriteHeader(http.StatusAccepted)
				write(map[string]string{"message": "Updating pull request branch"})
			case "reviews":
				write([]any{})
			case "merge":
				api.merges[n]++
				write(map[string]any{"merged": true, "sha": "merged"})
			default:
				t.Errorf("unexpected pull request %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		case "compare":
			_, head, _ := strings.Cut(path[4], "...")
			write(map[string]any{"behind_by": api.behind[head], "base_commit": map[string]string{"sha": "base1"}})
		case "branches":
			if len(path) > 5 {
				w.WriteHeader(http.StatusNotFound)
				write(map[string]string{"message": "Branch not protected"})
			} else {
				write(map[string]any{"name": path[4], "commit": map[string]string{"sha": "base1"}})
			}
		case "rules":
			write([]any{})
		case "commits":
			head := path[4]
			state, conclusion := "success", "success"
			if api.pending[head] {
				state, conclusion = "pending", ""
			}
			if path[5] == "status" {
				write(map[string]any{"state": state, "total_count": 1, "statuses": []map[string]string{{"context": "build", "state": state}}})
			} else {
				status := "completed"
				if conclusion == "" {
					status = "queued"
				}
				write(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "build", "status": status, "conclusion": conclusion}}})
			}
		case "actions":
			write(map[string]any{"total_count": 0, "workflow_runs": []any{}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return api, srv
}

func preparationEngine(url string, store *BasePreparationStore) *Engine {
	c := newAutoMergeSweepClient(url)
	c.basePreparation = store
	c.requireCurrentBase = func(string) bool { return true }
	c.requireCurrentBaseStrict = func(string) bool { return true }
	return c
}

func preparationGate(c *Engine, n int, base, head string) (AutoMergeSweepEvent, string, bool, error) {
	return c.currentBaseGate(context.Background(), "acme/widget", "acme", "widget", n, base, head, true, "behind", false)
}

func TestBasePreparationStickyAcrossLanesSweepsAndRestart(t *testing.T) {
	api, srv := newPreparationAPI(t)
	dir := filepath.Join(t.TempDir(), "preparation")
	c := preparationEngine(srv.URL, NewBasePreparationStore(dir))
	if ev, reason, stop, err := preparationGate(c, 1, "main", "head1"); err != nil || !stop || !ev.BranchUpdated || reason != basePreparationReady {
		t.Fatalf("first refresh: %+v %q %v %v", ev, reason, stop, err)
	}
	for _, lane := range []string{stageLaneSelf, stageLaneTrusted, stageLaneContributor} {
		// Fresh stores simulate one-shot lane engines and a pod restart.
		c = preparationEngine(srv.URL, NewBasePreparationStore(dir))
		c.recordStage(lane, "acme/widget", "acme", "widget", preparationPR(1, "main", "new-refresh-head"), "required-check-pending:Sign-off survives the squash", false, nil)
		for _, n := range []int{2, 3, 1} {
			_, reason, stop, err := preparationGate(c, n, "main", fmt.Sprintf("head%d", n))
			want := preparationQueued(1)
			if n == 1 {
				want = basePreparationReady
			}
			if err != nil || !stop || reason != want {
				t.Fatalf("lane %s PR %d: reason=%q stop=%v err=%v", lane, n, reason, stop, err)
			}
		}
	}
	if api.updates[1] != 1 || api.updates[2] != 0 || api.updates[3] != 0 {
		t.Fatalf("refresh stampede: %v", api.updates)
	}
}

func TestBasePreparationReleasesInvalidFront(t *testing.T) {
	for _, why := range []string{"closed", "gone", "held", "draft", "conflicting", "retargeted", "expired", "ci-red", "review-rejected", "withdrawn"} {
		t.Run(why, func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			now := time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC)
			c.now = func() time.Time { return now }
			if _, _, _, err := preparationGate(c, 1, "main", "head1"); err != nil {
				t.Fatal(err)
			}
			switch why {
			case "closed":
				api.prs[1].State = gh.Ptr("closed")
			case "gone":
				delete(api.prs, 1)
			case "held":
				api.prs[1].Labels = []*gh.Label{{Name: gh.Ptr("hold")}}
			case "draft":
				api.prs[1].Draft = gh.Ptr(true)
			case "conflicting":
				api.prs[1].MergeableState = gh.Ptr("dirty")
			case "retargeted":
				api.prs[1].Base.Ref = gh.Ptr("other")
			case "expired":
				now = now.Add(c.preparationWindow())
			case "ci-red", "review-rejected", "withdrawn":
				reason := "required-check-failing:build"
				if why == "review-rejected" {
					reason = autoMergeReasonReviewGatePrefix + ReviewGateOutcomeChangesRequested
				} else if why == "withdrawn" {
					reason = "untrusted-author-role"
				}
				c.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", api.prs[1], reason, false, nil)
			}
			if ev, reason, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
				t.Fatalf("successor blocked after %s: %+v %q %v", why, ev, reason, err)
			}
		})
	}
}

func TestBasePreparationCurrentReadySuccessorBypassesPendingOwnerWithoutBypassingGates(t *testing.T) {
	for _, blocker := range []string{"", "ci-pending", "review-owed"} {
		t.Run(blocker, func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			approved := true
			c.reviewGate = &ReviewGate{
				RequireApproval: func() bool { return true },
				Verdict: func(_ string, n int, _ string) ReviewGateVerdict {
					return ReviewGateVerdict{Loaded: true, Found: true, Approved: n == 1 || approved}
				},
			}
			if ev, _, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 1, true); err != nil || !ev.BranchUpdated {
				t.Fatalf("admit owner: %+v %v", ev, err)
			}
			// A refresh head is pending on runners, not red or missing DCO.
			api.prs[1].Head.SHA = gh.Ptr("refreshed1")
			api.pending["refreshed1"] = true
			api.behind["head2"] = 0 // successor is already tested on current base
			if blocker == "ci-pending" {
				api.pending["head2"] = true
			}
			if blocker == "review-owed" {
				approved = false
			}
			if _, _, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 1, true); err != nil {
				t.Fatal(err)
			}
			ev, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 2, true)
			if err != nil {
				t.Fatal(err)
			}
			if blocker == "" {
				if ev.MergeSHA == "" || reason != "" || api.merges[2] != 1 {
					t.Fatalf("ready successor blocked by pending preparation: %+v %q", ev, reason)
				}
			} else if ev.MergeSHA != "" || api.merges[2] != 0 || reason == "" {
				t.Fatalf("bypassed %s: %+v %q", blocker, ev, reason)
			}
			if api.updates[1] != 1 || api.updates[2] != 0 {
				t.Fatalf("unexpected refreshes: %v", api.updates)
			}
		})
	}
}

func TestBasePreparationConcurrentDuplicateAndIndependentDestinations(t *testing.T) {
	api, srv := newPreparationAPI(t)
	dir := filepath.Join(t.TempDir(), "preparation")
	var wg sync.WaitGroup
	const sweeps = 8
	for range sweeps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := preparationEngine(srv.URL, NewBasePreparationStore(dir))
			if _, _, stop, err := preparationGate(c, 1, "main", "head1"); err != nil || !stop {
				t.Errorf("concurrent gate stop=%v err=%v", stop, err)
			}
		}()
	}
	wg.Wait()
	if api.updates[1] != 1 {
		t.Fatalf("duplicate refreshes: %v", api.updates)
	}
	api.prs[2].Base.Ref = gh.Ptr("release")
	c := preparationEngine(srv.URL, NewBasePreparationStore(dir))
	if ev, _, _, err := preparationGate(c, 2, "release", "head2"); err != nil || !ev.BranchUpdated {
		t.Fatalf("separate base blocked: %+v %v", ev, err)
	}
	// Repository case aliases share ownership; ref case remains distinct.
	if reason, err := c.admitBasePreparation(context.Background(), "ACME", "WIDGET", "main", 3, "head3", stageLaneContributor, false); err != nil || reason != preparationQueued(1) {
		t.Fatalf("canonical repo gate: %q %v", reason, err)
	}
	if reason, err := c.admitBasePreparation(context.Background(), "acme", "other", "main", 3, "head3", stageLaneTrusted, false); err != nil || reason != "" {
		t.Fatalf("independent repo blocked: %q %v", reason, err)
	}
}

func TestBasePreparationHeadBaseChangesAndFairRelease(t *testing.T) {
	api, srv := newPreparationAPI(t)
	c := preparationEngine(srv.URL, NewBasePreparationStore(""))
	for _, n := range []int{1, 2, 3} {
		if _, _, _, err := preparationGate(c, n, "main", fmt.Sprintf("head%d", n)); err != nil {
			t.Fatal(err)
		}
	}
	// A changed head cannot be updated through a stale snapshot.
	api.prs[1].Head.SHA = gh.Ptr("pushed1")
	if err := c.executeUpdateBranchEffect(context.Background(), "acme", "widget", 1, "head1", "another-lane"); preparationWaitReason(err) != basePreparationPrefix+"snapshot-changed" {
		t.Fatalf("stale head update: %v", err)
	}
	// A new external base tip may be coalesced once the owner qualifies again.
	api.prs[1].Base.SHA = gh.Ptr("base2")
	api.behind["pushed1"] = 1
	if ev, _, _, err := preparationGate(c, 1, "main", "pushed1"); err != nil || !ev.BranchUpdated {
		t.Fatalf("new qualified head/base: %+v %v", ev, err)
	}
	c.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", api.prs[1], "", true, nil)
	if _, reason, _, err := preparationGate(c, 3, "main", "head3"); err != nil || reason != preparationQueued(2) {
		t.Fatalf("arrival order lost after merge: %q %v", reason, err)
	}
	if ev, _, _, err := preparationGate(c, 2, "main", "head2"); err != nil || !ev.BranchUpdated {
		t.Fatalf("oldest waiting candidate did not advance: %+v %v", ev, err)
	}
}

func TestBasePreparationIgnoresOtherLanesNonAdmission(t *testing.T) {
	api, srv := newPreparationAPI(t)
	c := preparationEngine(srv.URL, NewBasePreparationStore(""))
	for _, n := range []int{1, 2} {
		if _, _, _, err := preparationGate(c, n, "main", fmt.Sprintf("head%d", n)); err != nil {
			t.Fatal(err)
		}
	}
	for _, otherLane := range []string{stageLaneTrusted, stageLaneContributor} {
		for _, reason := range []string{"trusted-bot-lane", "untrusted-author-role", "not-app-authored", "contributor-lane:app-authored"} {
			for _, n := range []int{1, 2} {
				c.recordStage(otherLane, "acme/widget", "acme", "widget", api.prs[n], reason, false, nil)
			}
		}
	}
	if _, reason, _, err := preparationGate(c, 3, "main", "head3"); err != nil || reason != preparationQueued(1) {
		t.Fatalf("another lane revoked the pending owner: %q %v", reason, err)
	}
	c.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", api.prs[1], "", true, nil)
	if _, reason, _, err := preparationGate(c, 3, "main", "head3"); err != nil || reason != preparationQueued(2) {
		t.Fatalf("another lane erased FIFO tickets: %q %v", reason, err)
	}
}

func TestBasePreparationNeverPromotesChangedOrUnqualifiedWaitingHead(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			api, srv := newPreparationAPI(t)
			c := preparationEngine(srv.URL, NewBasePreparationStore(""))
			for _, n := range []int{1, 2} {
				if _, _, _, err := preparationGate(c, n, "main", fmt.Sprintf("head%d", n)); err != nil {
					t.Fatal(err)
				}
			}
			api.prs[1].State = gh.Ptr("closed")
			if changed {
				api.prs[2].Head.SHA = gh.Ptr("pushed2")
			}
			ev, reason, _, err := preparationGate(c, 3, "main", "head3")
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				if !ev.BranchUpdated {
					t.Fatalf("changed waiting head blocked successor: %q", reason)
				}
			} else {
				if reason != preparationQueued(2) {
					t.Fatalf("lost FIFO: %q", reason)
				}
				var owner int
				_ = c.preparationStore().change(preparationKey("acme", "widget", "main"), func(st *basePreparation) error {
					owner = st.Owner.Number
					return nil
				})
				if owner != 0 {
					t.Fatalf("old ticket became an unqualified owner: %d", owner)
				}
				c.recordStage(stageLaneSelf, "acme/widget", "acme", "widget", api.prs[2], "required-check-pending:build", false, nil)
				if ev, _, _, err := preparationGate(c, 3, "main", "head3"); err != nil || !ev.BranchUpdated {
					t.Fatalf("pending waiting ticket did not yield: %+v %v", ev, err)
				}
			}
		})
	}
}

func TestBasePreparationPersistenceFailsClosedAndNoFalseReadyStage(t *testing.T) {
	api, srv := newPreparationAPI(t)
	dir := t.TempDir()
	// Use a file as the store directory, deterministically refusing persistence.
	path := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(path, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := preparationEngine(srv.URL, NewBasePreparationStore(path))
	if _, reason, stop, err := preparationGate(c, 1, "main", "head1"); err == nil || !stop || reason != basePreparationPrefix+"state-unavailable" {
		t.Fatalf("persistence failure did not fail closed: %q %v %v", reason, stop, err)
	}
	for _, reason := range []string{preparationQueued(1), basePreparationReady} {
		v := classifyStage(stageObservation{Lane: stageLaneSelf, Reason: reason})
		if v.Stage != stagelabels.ReviewPending || v.Stage.Number() >= stagelabels.AwaitingMergeAdmission.Number() {
			t.Fatalf("preparation advertised ready: %+v", v)
		}
	}
	if api.updates[1] != 0 {
		t.Fatalf("unexpected update %v", api.updates)
	}
}

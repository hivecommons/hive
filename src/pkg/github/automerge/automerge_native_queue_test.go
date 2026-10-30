package automerge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

type nativeQueueFake struct {
	Transport
	state               hgithub.NativeMergeQueueState
	readErr, enqueueErr error
	methodErr           error
	enqueues, dequeues  int
	methodReads         int
	expectedHead        string
}

func (f *nativeQueueFake) NativeMergeQueueAvailable() bool { return true }
func (f *nativeQueueFake) ReadNativeMergeQueue(context.Context, string) (hgithub.NativeMergeQueueState, error) {
	return f.state, f.readErr
}
func (f *nativeQueueFake) EnqueueNativePullRequest(_ context.Context, _, head string) (*hgithub.NativeMergeQueueEntry, error) {
	f.enqueues++
	f.expectedHead = head
	if f.enqueueErr != nil {
		return nil, f.enqueueErr
	}
	f.state.IsInMergeQueue = true
	f.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
	return f.state.MergeQueueEntry, nil
}
func (f *nativeQueueFake) DequeueNativePullRequest(context.Context, string) error {
	f.dequeues++
	f.state.IsInMergeQueue = false
	f.state.MergeQueueEntry = nil
	return nil
}
func queueEntry(login string) *hgithub.NativeMergeQueueEntry {
	e := &hgithub.NativeMergeQueueEntry{ID: "entry1", Position: 2, State: "AWAITING_CHECKS"}
	// GraphQL Bot.login omits the REST "[bot]" suffix.
	e.Enqueuer.Login = strings.TrimSuffix(login, "[bot]")
	e.Enqueuer.Typename = "User"
	if strings.HasSuffix(login, "[bot]") {
		e.Enqueuer.Typename = "Bot"
	}
	return e
}

// enableNativeQueue opts every repo into the native queue; the zero policy
// is off (hivecommons/hive#15308).
func enableNativeQueue(c *Engine, policy ...NativeMergeQueuePolicy) {
	p := NativeMergeQueuePolicy{Enabled: true}
	if len(policy) > 0 {
		p = policy[0]
	}
	c.nativeMergeQueuePolicy = func() NativeMergeQueuePolicy { return p }
}

func nativeQueueEngine() (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim) {
	c := newAutoMergeSweepClient("http://unused.invalid")
	enableNativeQueue(c)
	f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
	c.transport = f
	pr := &gh.PullRequest{Number: gh.Ptr(7), NodeID: gh.Ptr("PR_7"), User: &gh.User{Login: gh.Ptr(testHiveAppBotLogin)}}
	claim := effects.Claim{Repo: "acme/widget", Kind: effects.KindPullRequestMerge, Target: "7", Actor: "automerge", Inputs: mergeClaimInputs("squash", "", "sha7", stageLaneSelf)}
	return c, f, pr, claim
}

func TestNativeQueueEnqueueIsPinnedAndIdempotent(t *testing.T) {
	c, f, pr, claim := nativeQueueEngine()
	b := &recordingBoundary{}
	c.mutation = b
	for i := 0; i < 2; i++ {
		reason, err := c.tryNativeQueue(context.Background(), pr, claim)
		if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") {
			t.Fatalf("reason=%q error=%v", reason, err)
		}
	}
	if f.enqueues != 1 || f.expectedHead != "sha7" || len(b.claims) != 1 || b.claims[0].Inputs["operation"] != "enqueue" {
		t.Fatalf("enqueues=%d head=%q claims=%+v", f.enqueues, f.expectedHead, b.claims)
	}
}

func TestNativeQueueFailureModes(t *testing.T) {
	for _, mode := range []string{"no-queue", "head-moved", "lookup-error", "enqueue-error", "denied", "human-queued"} {
		t.Run(mode, func(t *testing.T) {
			c, f, pr, claim := nativeQueueEngine()
			switch mode {
			case "no-queue":
				f.state.IsMergeQueueEnabled = false
			case "head-moved":
				f.state.HeadRefOid = "new-head"
			case "lookup-error":
				f.readErr = errors.New("unavailable")
			case "enqueue-error":
				f.enqueueErr = errors.New("expectedHeadOid mismatch")
			case "denied":
				c.mutation = denyNativeQueueBoundary{}
			case "human-queued":
				f.state.IsInMergeQueue = true
				f.state.MergeQueueEntry = queueEntry("maintainer")
			}
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if mode == "no-queue" && (reason != "" || err != nil) {
				t.Fatalf("direct merge should proceed: %q %v", reason, err)
			}
			if mode == "head-moved" && reason != "head-changed-since-eval" {
				t.Fatalf("reason=%q", reason)
			}
			if (mode == "lookup-error" || mode == "enqueue-error" || mode == "denied") && err == nil {
				t.Fatal("expected failure")
			}
			wantCalls := 0
			if mode == "enqueue-error" {
				wantCalls = 1
			}
			if f.enqueues != wantCalls || f.dequeues != 0 {
				t.Fatalf("enqueues=%d dequeues=%d", f.enqueues, f.dequeues)
			}
		})
	}
}

type denyNativeQueueBoundary struct{}

func (denyNativeQueueBoundary) Execute(context.Context, effects.Claim, func(context.Context) (effects.Result, error)) (effects.Result, error) {
	return effects.Result{}, effects.ErrDenied
}

func TestNativeQueueGateWithdrawsOnlyHiveEntries(t *testing.T) {
	for _, reason := range []string{"held", "label:hold", "label:do-not-merge", "changes-requested", autoMergeReasonHumanChangesRequested, autoMergeReasonReviewGatePrefix + ReviewGateOutcomeChangesRequested, autoMergeReasonNoHiveQueueApproval, "queue-approval-head-changed", "label-removed", nativeQueueReasonPaused, "draft", "closed"} {
		// A user account named like the App's slug is not the App.
		for _, actor := range []string{testHiveAppBotLogin, "maintainer", "another-app[bot]", "", botSlug(testHiveAppBotLogin)} {
			t.Run(reason+"/"+actor, func(t *testing.T) {
				c, f, pr, _ := nativeQueueEngine()
				f.state.IsInMergeQueue = true
				f.state.MergeQueueEntry = queueEntry(actor)
				c.reconcileNativeQueueGate(context.Background(), stageLaneSelf, "acme", "widget", pr, reason)
				want := 0
				if actor == testHiveAppBotLogin {
					want = 1
				}
				if f.dequeues != want {
					t.Fatalf("dequeues=%d want=%d", f.dequeues, want)
				}
			})
		}
	}
}

func TestNativeQueueDequeueHonorsBoundaryAndLaneOwnership(t *testing.T) {
	for _, mode := range []string{"denied", "different-lane", "eligible"} {
		t.Run(mode, func(t *testing.T) {
			c, f, pr, _ := nativeQueueEngine()
			f.state.IsInMergeQueue = true
			f.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
			lane, reason := stageLaneSelf, "held"
			if mode == "denied" {
				c.mutation = denyNativeQueueBoundary{}
			}
			if mode == "different-lane" {
				lane = stageLaneContributor
			}
			if mode == "eligible" {
				reason = nativeQueueReason
			}
			c.reconcileNativeQueueGate(context.Background(), lane, "acme", "widget", pr, reason)
			if f.dequeues != 0 {
				t.Fatal("must preserve queued entry")
			}
		})
	}
}

// Exercise the real lane and its cheap prefilter, not only the queue helper.
func TestSelfAuthoredSweepNativeQueueAndHold(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "eligible", true: "held"}[held], func(t *testing.T) {
			var merged []int
			labels := []string(nil)
			if held {
				labels = []string{"hold"}
			}
			api := newAutoMergeSweepAPI(t, "", []sweepPR{{number: 7, nodeID: "PR_7", author: testHiveAppBotLogin, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success", extraLabels: labels}}, &merged)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
			c.transport = f
			enableNativeQueue(c)
			if held {
				f.state.IsInMergeQueue = true
				f.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
			}
			for pass := 0; pass < 2; pass++ {
				res, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
				if err != nil || len(res.Merged) != 0 || len(merged) != 0 {
					t.Fatalf("res=%+v direct=%v error=%v", res, merged, err)
				}
			}
			if held && (f.enqueues != 0 || f.dequeues != 1) {
				t.Fatalf("held enqueues=%d dequeues=%d", f.enqueues, f.dequeues)
			}
			if !held && f.enqueues != 1 {
				t.Fatalf("enqueues=%d", f.enqueues)
			}
		})
	}
}

func TestHumanMergeLanesEnqueueWithoutDirectMerge(t *testing.T) {
	for _, lane := range []string{"queued", "trusted-author"} {
		t.Run(lane, func(t *testing.T) {
			var merged []int
			api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{number: 7, nodeID: "PR_7", author: "alice", queuedBy: "bob", label: true, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"}}, &merged)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
			c.transport = f
			enableNativeQueue(c)
			c.trustedAuthorizer = func(string, string) TrustedAuthorDecision {
				return TrustedAuthorDecision{Allowed: true, Role: "merger"}
			}
			var result *AutoMergeSweepResult
			var err error
			if lane == "queued" {
				result, err = c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
			} else {
				c.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"} }
				result, err = c.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{})
			}
			if err != nil || len(merged) != 0 || len(result.Merged) != 0 || f.enqueues != 1 {
				t.Fatalf("result=%+v merged=%v enqueues=%d error=%v", result, merged, f.enqueues, err)
			}
		})
	}
}

func (f *nativeQueueFake) ActiveRepositories() []string {
	if p, ok := f.Transport.(interface{ ActiveRepositories() []string }); ok {
		return p.ActiveRepositories()
	}
	return f.Repositories()
}

func TestNativeQueuePauseWithdrawsOwnEntry(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, "", []sweepPR{{number: 7, nodeID: "PR_7", author: testHiveAppBotLogin}}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	client := c.transport.(*hgithub.Client)
	client.SetRepoPausedFunc(func(string) bool { return true })
	f := &nativeQueueFake{Transport: client, state: hgithub.NativeMergeQueueState{ID: "PR_7", IsMergeQueueEnabled: true, IsInMergeQueue: true, MergeQueueEntry: queueEntry(testHiveAppBotLogin)}}
	c.transport = f
	enableNativeQueue(c)
	c.reconcileInactiveNativeQueues(context.Background())
	if f.dequeues != 1 || f.enqueues != 0 || len(merged) != 0 {
		t.Fatalf("dequeues=%d enqueues=%d merged=%v", f.dequeues, f.enqueues, merged)
	}
}

func (f *nativeQueueFake) NativeMergeQueueMethod(context.Context, string, string) (string, error) {
	f.methodReads++
	if f.methodErr != nil {
		return "", f.methodErr
	}
	return "SQUASH", nil
}

// A failure to read the queue's merge method is a read failure, not a
// merge-method refusal: it must keep the PR at stage/100 and must not be
// remembered as a refusal for the head.
func TestNativeQueueMethodReadFailureIsReadReason(t *testing.T) {
	c, f, pr, claim := nativeQueueEngine()
	claim.Inputs["method_reason"] = mergeMethodReasonMixedAuthorship
	f.methodErr = errors.New("graphql: timeout")
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if reason != nativeQueueReasonRead || err == nil || f.enqueues != 0 {
		t.Fatalf("reason=%q error=%v enqueues=%d, want %q with error", reason, err, f.enqueues, nativeQueueReasonRead)
	}
	f.methodErr = nil
	if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != nativeQueueReasonMethod || f.methodReads != 2 {
		t.Fatalf("after read failure: reason=%q method reads=%d, want a re-read reporting %q", reason, f.methodReads, nativeQueueReasonMethod)
	}
}

func TestNativeQueuePreservesMixedAuthorshipGate(t *testing.T) {
	c, f, pr, claim := nativeQueueEngine()
	claim.Inputs["method_reason"] = mergeMethodReasonMixedAuthorship
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if reason != "native-merge-queue-method" || err == nil || f.enqueues != 0 {
		t.Fatalf("reason=%q error=%v enqueues=%d", reason, err, f.enqueues)
	}
}

// A SQUASH queue's refusal of a mixed-authorship PR is remembered like a
// merge-method 405: the next sweep at the same head reports the same reason
// without re-reading the queue's method, and a new head or the recheck
// window re-reads it.
func TestNativeQueueMethodRefusalIsRemembered(t *testing.T) {
	c, f, pr, claim := nativeQueueEngine()
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	claim.Inputs["method_reason"] = mergeMethodReasonMixedAuthorship
	sweep := func() (string, error) { return c.tryNativeQueue(context.Background(), pr, claim) }
	if reason, err := sweep(); reason != "native-merge-queue-method" || err == nil {
		t.Fatalf("first sweep: reason=%q error=%v", reason, err)
	}
	if reason, err := sweep(); reason != "native-merge-queue-method" || err != nil {
		t.Fatalf("second sweep: reason=%q error=%v", reason, err)
	}
	if f.methodReads != 1 || f.enqueues != 0 {
		t.Fatalf("two sweeps at one head: method reads=%d enqueues=%d, want 1 and 0", f.methodReads, f.enqueues)
	}

	f.state.HeadRefOid = "sha8"
	claim.Inputs["expect_sha"] = "sha8"
	if reason, _ := sweep(); reason != "native-merge-queue-method" || f.methodReads != 2 {
		t.Fatalf("new head: reason=%q method reads=%d, want a re-read", reason, f.methodReads)
	}

	now = now.Add(mergeRefusalRecheck)
	if reason, _ := sweep(); reason != "native-merge-queue-method" || f.methodReads != 3 {
		t.Fatalf("after recheck: reason=%q method reads=%d, want a re-read", reason, f.methodReads)
	}
}

func TestContributorLaneEnqueuesAfterGates(t *testing.T) {
	fx := validContributorFixture()
	fx.nodeID = "PR_7"
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	c := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
	c.transport = f
	enableNativeQueue(c)
	event, reason, err := c.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 1 || fx.merges != 0 || event.MergeSHA != "" {
		t.Fatalf("event=%+v reason=%q error=%v enqueues=%d merges=%d", event, reason, err, f.enqueues, fx.merges)
	}
}

func TestNativeQueueNewCycleAfterRemoval(t *testing.T) {
	c, f, pr, claim := nativeQueueEngine()
	b := &recordingBoundary{}
	c.mutation = b
	if _, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil {
		t.Fatal(err)
	}
	f.state.IsInMergeQueue = false
	f.state.MergeQueueEntry = nil
	f.state.TimelineItems.Nodes = append(f.state.TimelineItems.Nodes, struct{ ID string }{ID: "removal1"})
	if _, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil {
		t.Fatal(err)
	}
	if f.enqueues != 2 || len(b.claims) != 2 || b.claims[0].Inputs["after_removal"] == b.claims[1].Inputs["after_removal"] {
		t.Fatalf("enqueues=%d claims=%+v", f.enqueues, b.claims)
	}
}

func TestInactiveQueueReconcileIsThrottled(t *testing.T) {
	c := &Engine{now: time.Now}
	if !c.inactiveQueueDue("o/r") || c.inactiveQueueDue("o/r") || !c.inactiveQueueDue("o/other") {
		t.Fatal("expected first call due, immediate repeat throttled, other repo due")
	}
}

func TestNativeQueueErrorReasonsPreserveOwnEntry(t *testing.T) {
	for _, reason := range []string{nativeQueueReasonRead, nativeQueueReasonEnqueue, nativeQueueReasonMethod} {
		t.Run(reason, func(t *testing.T) {
			c, f, pr, _ := nativeQueueEngine()
			f.state.IsInMergeQueue = true
			f.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
			c.reconcileNativeQueueGate(context.Background(), stageLaneSelf, "acme", "widget", pr, reason)
			if f.dequeues != 0 {
				t.Fatalf("dequeues=%d, error reason must not withdraw own entry", f.dequeues)
			}
		})
	}
}

func TestSelfAuthoredSweepReadErrorKeepsQueuedEntry(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, "", []sweepPR{{number: 7, nodeID: "PR_7", author: testHiveAppBotLogin, mergeableState: "clean", statusState: "success", checkStatus: "completed", checkConclusion: "success"}}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true, IsInMergeQueue: true, MergeQueueEntry: queueEntry(testHiveAppBotLogin)}, readErr: errors.New("graphql 502")}
	c.transport = f
	enableNativeQueue(c)
	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.dequeues != 0 {
		t.Fatalf("dequeues=%d, transient read error must not withdraw own entry", f.dequeues)
	}
}

// Only eligibility loss withdraws the App's entry (#15198); transient and
// non-gate reasons must leave it for GitHub's queue to settle.
func TestNativeQueueTransientReasonsPreserveOwnEntry(t *testing.T) {
	for _, reason := range []string{"behind", "not-mergeable", "conflicting", "fetch-pr-recheck", "head-changed-since-eval", "check-pending", "update-branch", "queue-approval-check", "review-state-check", "author-permission-check", autoMergeReasonCurrentBaseCheck, autoMergeReasonReviewGatePrefix + ReviewGateOutcomeAwaiting, "not-app-authored", "untrusted-author-role", "merge-failed", ""} {
		t.Run(reason, func(t *testing.T) {
			c, f, pr, _ := nativeQueueEngine()
			f.state.IsInMergeQueue = true
			f.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
			for _, lane := range []string{stageLaneSelf, nativeQueueLaneQueued} {
				c.reconcileNativeQueueGate(context.Background(), lane, "acme", "widget", pr, reason)
			}
			if f.dequeues != 0 {
				t.Fatalf("dequeues=%d, %q must not withdraw own entry", f.dequeues, reason)
			}
		})
	}
}

// A queue-required base reports blocked until the PR merges through the
// queue, so the queued lane must enqueue instead of stopping at
// not-mergeable; without a queue, blocked still stops the lane.
func TestQueuedLaneEnqueuesBlockedPROnQueueBase(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "queue", false: "no-queue"}[enabled], func(t *testing.T) {
			var merged []int
			api := newAutoMergeSweepAPI(t, hgithub.AutoMergeQueuedLabel, []sweepPR{{number: 7, nodeID: "PR_7", author: "alice", queuedBy: "bob", label: true, mergeableState: "blocked", statusState: "success", checkStatus: "completed", checkConclusion: "success"}}, &merged)
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: enabled}}
			c.transport = f
			enableNativeQueue(c)
			c.trustedAuthorizer = func(string, string) TrustedAuthorDecision {
				return TrustedAuthorDecision{Allowed: true, Role: "merger"}
			}
			result, err := c.SweepQueuedAutoMerges(context.Background(), AutoMergeSweepOptions{})
			want := 0
			if enabled {
				want = 1
			}
			if err != nil || len(merged) != 0 || len(result.Merged) != 0 || f.enqueues != want || f.dequeues != 0 {
				t.Fatalf("result=%+v merged=%v enqueues=%d dequeues=%d error=%v", result, merged, f.enqueues, f.dequeues, err)
			}
		})
	}
}

// The current-base gate stays in force on a queue-enabled base: a behind
// head is updated (and so not enqueued this tick) exactly as on any other
// base. Only the explicit auto_merge.native_merge_queue.skip_current_base_gate
// lets the merge group test the merged result instead, because a new head
// would drop the PR from the queue.
func TestNativeQueueCurrentBaseGateIsExplicit(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, lane := range []string{stageLaneSelf, stageLaneTrusted} {
			for _, state := range []string{"behind", "clean"} {
				t.Run(map[bool]string{false: "gate-kept", true: "gate-skipped"}[skip]+"/"+lane+"/"+state, func(t *testing.T) {
					var merged []int
					author := testHiveAppBotLogin
					if lane == stageLaneTrusted {
						author = "alice"
					}
					updates := 0
					api := newAutoMergeSweepAPI(t, "", []sweepPR{{number: 7, nodeID: "PR_7", author: author, baseRef: "main", baseSHA: "base1", mergeableState: state, statusState: "success", checkStatus: "completed", checkConclusion: "success"}}, &merged, func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main":
							_ = json.NewEncoder(w).Encode(map[string]any{"name": "main", "commit": map[string]string{"sha": "base1"}})
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/base1/status":
							_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main/protection/required_status_checks":
							w.WriteHeader(http.StatusNotFound)
							_ = json.NewEncoder(w).Encode(map[string]string{"message": "Branch not protected"})
						case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/rules/branches/main":
							_ = json.NewEncoder(w).Encode([]any{})
						case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/update-branch"):
							updates++
							w.WriteHeader(http.StatusAccepted)
							_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
						case strings.Contains(r.URL.Path, "/compare/"):
							behind := 0
							if state == "behind" {
								behind = 2
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": behind, "ahead_by": 1})
						default:
							t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
							w.WriteHeader(http.StatusNotFound)
						}
					})
					defer api.Close()
					c := newAutoMergeSweepClient(api.URL)
					c.requireCurrentBase = func(string) bool { return true }
					f := &nativeQueueFake{Transport: c.transport, state: hgithub.NativeMergeQueueState{ID: "PR_7", HeadRefOid: "sha7", IsMergeQueueEnabled: true}}
					c.transport = f
					enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, SkipCurrentBaseGate: skip})
					var err error
					if lane == stageLaneSelf {
						_, err = c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
					} else {
						c.trustedAuthorizer = func(string, string) TrustedAuthorDecision {
							return TrustedAuthorDecision{Allowed: true, Role: "merger"}
						}
						c.trustedAuthorPolicy = func() TrustedAuthorPolicy { return TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"} }
						_, err = c.SweepTrustedAuthorAutoMerges(context.Background(), AutoMergeSweepOptions{})
					}
					wantUpdates, wantEnqueues := 0, 0
					if !skip && state == "behind" {
						wantUpdates = 1
					}
					if skip || state == "clean" {
						wantEnqueues = 1
					}
					if err != nil || updates != wantUpdates || f.enqueues != wantEnqueues || len(merged) != 0 {
						t.Fatalf("updates=%d enqueues=%d merged=%v error=%v, want updates=%d enqueues=%d", updates, f.enqueues, merged, err, wantUpdates, wantEnqueues)
					}
				})
			}
		}
	}
}

// Without auto_merge.native_merge_queue.enabled (or with the repo outside
// its allow-list) no lane reads, enqueues or withdraws a queue entry: the
// hive behaves exactly as before the queue hand-off existed.
func TestNativeQueueIsOptInPerRepo(t *testing.T) {
	type readCounter struct{ reads int }
	for _, mode := range []string{"default-off", "other-repo-only", "listed-repo"} {
		t.Run(mode, func(t *testing.T) {
			c, f, pr, claim := nativeQueueEngine()
			counter := &readCounter{}
			reading := &countingNativeQueueFake{nativeQueueFake: f, reads: &counter.reads}
			c.transport = reading
			f.state.IsInMergeQueue = true
			f.state.MergeQueueEntry = queueEntry(testHiveAppBotLogin)
			switch mode {
			case "default-off":
				c.nativeMergeQueuePolicy = nil
			case "other-repo-only":
				enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, Repos: map[string]bool{"acme/other": true}})
			case "listed-repo":
				enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, Repos: map[string]bool{"acme/widget": true}})
			}
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			c.reconcileNativeQueueGate(context.Background(), stageLaneSelf, "acme", "widget", pr, "held")
			enabled := c.nativeQueueEnabled(context.Background(), "acme/widget", pr)
			if mode == "listed-repo" {
				if err != nil || !strings.HasPrefix(reason, nativeQueueReason) || f.dequeues != 1 || !enabled || counter.reads == 0 {
					t.Fatalf("listed repo: reason=%q error=%v dequeues=%d enabled=%v reads=%d", reason, err, f.dequeues, enabled, counter.reads)
				}
				return
			}
			if reason != "" || err != nil || f.enqueues != 0 || f.dequeues != 0 || enabled || counter.reads != 0 {
				t.Fatalf("%s: reason=%q error=%v enqueues=%d dequeues=%d enabled=%v reads=%d, want no queue traffic", mode, reason, err, f.enqueues, f.dequeues, enabled, counter.reads)
			}
		})
	}
}

type countingNativeQueueFake struct {
	*nativeQueueFake
	reads *int
}

func (f *countingNativeQueueFake) ReadNativeMergeQueue(ctx context.Context, id string) (hgithub.NativeMergeQueueState, error) {
	*f.reads++
	return f.nativeQueueFake.ReadNativeMergeQueue(ctx, id)
}

// The isMergeQueueEnabled probe is read once per repo#base and reused for
// probe_ttl (default 30m); a read failure is not cached, and a new base
// branch is its own probe.
func TestNativeQueueProbeIsCachedPerBase(t *testing.T) {
	c, f, pr, _ := nativeQueueEngine()
	reads := 0
	c.transport = &countingNativeQueueFake{nativeQueueFake: f, reads: &reads}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	pr.Base = &gh.PullRequestBranch{Ref: gh.Ptr("main")}
	other := &gh.PullRequest{Number: gh.Ptr(8), NodeID: gh.Ptr("PR_8"), Base: &gh.PullRequestBranch{Ref: gh.Ptr("main")}}
	for i := 0; i < 3; i++ {
		if !c.nativeQueueEnabled(context.Background(), "acme/widget", pr) || !c.nativeQueueEnabled(context.Background(), "acme/widget", other) {
			t.Fatal("base has a queue")
		}
	}
	if reads != 1 {
		t.Fatalf("reads=%d, want one probe per base across PRs and sweeps", reads)
	}
	release := &gh.PullRequest{Number: gh.Ptr(9), NodeID: gh.Ptr("PR_9"), Base: &gh.PullRequestBranch{Ref: gh.Ptr("release-1")}}
	f.state.IsMergeQueueEnabled = false
	if c.nativeQueueEnabled(context.Background(), "acme/widget", release) || reads != 2 {
		t.Fatalf("other base: reads=%d, want its own probe reporting no queue", reads)
	}
	if !c.nativeQueueEnabled(context.Background(), "acme/widget", pr) || reads != 2 {
		t.Fatalf("main stays cached: reads=%d", reads)
	}

	now = now.Add(DefaultNativeMergeQueueProbeTTL)
	if c.nativeQueueEnabled(context.Background(), "acme/widget", pr) || reads != 3 {
		t.Fatalf("after TTL: reads=%d, want a re-read seeing the queue gone", reads)
	}

	enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, ProbeTTL: time.Minute})
	f.state.IsMergeQueueEnabled = true
	now = now.Add(time.Minute)
	f.readErr = errors.New("graphql 502")
	if c.nativeQueueEnabled(context.Background(), "acme/widget", pr) || reads != 4 {
		t.Fatalf("read failure: reads=%d, want false without caching", reads)
	}
	f.readErr = nil
	if !c.nativeQueueEnabled(context.Background(), "acme/widget", pr) || reads != 5 {
		t.Fatalf("after failure: reads=%d, want a fresh probe", reads)
	}
}

// A paused repo's open PRs are only listed when the repo opted in.
func TestInactiveQueueReconcileSkipsUnlistedRepos(t *testing.T) {
	var merged []int
	api := newAutoMergeSweepAPI(t, "", []sweepPR{{number: 7, nodeID: "PR_7", author: testHiveAppBotLogin}}, &merged)
	defer api.Close()
	c := newAutoMergeSweepClient(api.URL)
	client := c.transport.(*hgithub.Client)
	client.SetRepoPausedFunc(func(string) bool { return true })
	f := &nativeQueueFake{Transport: client, state: hgithub.NativeMergeQueueState{ID: "PR_7", IsMergeQueueEnabled: true, IsInMergeQueue: true, MergeQueueEntry: queueEntry(testHiveAppBotLogin)}}
	c.transport = f
	enableNativeQueue(c, NativeMergeQueuePolicy{Enabled: true, Repos: map[string]bool{"acme/other": true}})
	c.reconcileInactiveNativeQueues(context.Background())
	if f.dequeues != 0 {
		t.Fatalf("dequeues=%d, unlisted repo must not be reconciled", f.dequeues)
	}
}

package automerge

import (
	"context"
	"errors"
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
	return e
}
func nativeQueueEngine() (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim) {
	c := newAutoMergeSweepClient("http://unused.invalid")
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
	for _, reason := range []string{"held", "changes-requested", "no-approval", "stale-approval", "label:hold"} {
		for _, actor := range []string{testHiveAppBotLogin, "maintainer", "another-app[bot]", ""} {
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
	if _, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.dequeues != 0 {
		t.Fatalf("dequeues=%d, transient read error must not withdraw own entry", f.dequeues)
	}
}

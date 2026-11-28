package automerge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
)

// reviewRequiredMessage is GitHub's REST merge wording for a missing
// approving review. The GraphQL enqueue wording has not been seen live, so
// the tests reuse the wording the direct merge path already matches.
const reviewRequiredMessage = "At least 1 approving review is required by reviewers with write access."

// graphQLErrorValue returns the error the GitHub client produces for a
// GraphQL response whose errors array holds one entry of type typ. The
// client's GraphQL error type is not exported, so the helper gets a real
// one from a client pointed at a test server.
func graphQLErrorValue(t *testing.T, typ, msg string) error {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		entry := map[string]string{"message": msg}
		if typ != "" {
			entry["type"] = typ
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": []map[string]string{entry}})
	}))
	defer api.Close()
	_, err := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL).ReadNativeMergeQueue(context.Background(), "PR_7")
	if err == nil {
		t.Fatal("the test server's errors array must come back as an error")
	}
	return err
}

// enqueueRefusalEngine is nativeQueueEngine with the enqueue failing with
// err, a fixed clock the test advances, and a boundary that keeps what the
// effect returned.
func enqueueRefusalEngine(err error, now *time.Time) (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim, *effectErrBoundary) {
	c, f, pr, claim := nativeQueueEngine()
	f.enqueueErr = err
	c.now = func() time.Time { return *now }
	b := &effectErrBoundary{}
	c.mutation = b
	return c, f, pr, claim, b
}

// unnamedRefusal is an enqueue refusal in words the hive matches nowhere:
// GitHub's GraphQL wording for a missing review has not been seen live.
const unnamedRefusal = "Pull request was turned down for a reason the hive has no name for"

// Refuse-first: the hive asks GitHub once for a head, even while GitHub's
// reviewDecision says a review is outstanding. A refusal on one of GitHub's
// rules for the PR (UNPROCESSABLE) under that decision is the review
// refusal whatever its wording: NotApplied at the boundary, the direct
// merge path's reason, no error, and no second enqueue for the head while
// the decision stands, until the direct path's recheck window has passed.
// The approval changes the decision, and the next pass enqueues. On the old
// code the same head was enqueued, and refused, on every pass.
func TestNativeQueueRefusalUnderOutstandingReviewIsReviewRefusal(t *testing.T) {
	for _, decision := range []string{"REVIEW_REQUIRED", "CHANGES_REQUESTED"} {
		t.Run(decision, func(t *testing.T) {
			now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			c, f, pr, claim, b := enqueueRefusalEngine(graphQLErrorValue(t, "UNPROCESSABLE", unnamedRefusal), &now)
			f.state.ReviewDecision = decision
			for pass := 0; pass < 3; pass++ {
				reason, err := c.tryNativeQueue(context.Background(), pr, claim)
				if err != nil || reason != autoMergeReasonMergeRefusedReview {
					t.Fatalf("pass %d: reason=%q err=%v, want %q and no error", pass, reason, err, autoMergeReasonMergeRefusedReview)
				}
				now = now.Add(time.Minute)
			}
			if f.enqueues != 1 {
				t.Fatalf("enqueues=%d, want the one enqueue GitHub refused and no second one for the head", f.enqueues)
			}
			if len(b.errs) != 1 || !errors.Is(b.errs[0], effects.ErrNotApplied) {
				t.Fatalf("the refused enqueue must reach the boundary as NotApplied, got %v", b.errs)
			}
			// The same window as the direct merge path, which the 085
			// note promises.
			now = now.Add(mergeRefusalRecheck - 4*time.Minute)
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonMergeRefusedReview || f.enqueues != 1 {
				t.Fatalf("just inside the recheck window: reason=%q enqueues=%d, want the head still held", reason, f.enqueues)
			}
			now = now.Add(time.Minute)
			if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || reason != autoMergeReasonMergeRefusedReview || f.enqueues != 2 {
				t.Fatalf("after the recheck window: reason=%q err=%v enqueues=%d, want one more attempt", reason, err, f.enqueues)
			}
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonMergeRefusedReview || f.enqueues != 2 {
				t.Fatalf("inside the next window: reason=%q enqueues=%d, want no attempt", reason, f.enqueues)
			}
			// A person's approval is not a ready front for a merge train
			// to wait behind, and the hold blocks the head.
			if !mergeTrainBlockingReason(autoMergeReasonMergeRefusedReview) || mergeTrainReadinessOf(autoMergeReasonMergeRefusedReview) == mergeTrainReadinessReady {
				t.Fatal("the review hold must block a merge train's front, not read as ready")
			}
			f.state.ReviewDecision = "APPROVED"
			f.enqueueErr = nil
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") || f.enqueues != 3 {
				t.Fatalf("after the approval: reason=%q err=%v enqueues=%d, want the PR queued on the next pass", reason, err, f.enqueues)
			}
		})
	}
}

// GitHub's review state names only a refusal on one of its rules for the
// PR. FORBIDDEN (the App may not enqueue) and NOT_FOUND are refusals of
// another kind: under an outstanding review they still back off with their
// own text and a WARN, and never read as "a person owes an approval".
func TestNativeQueueOtherRefusalUnderOutstandingReviewBacksOff(t *testing.T) {
	for typ, msg := range map[string]string{
		"FORBIDDEN": "Resource not accessible by integration",
		"NOT_FOUND": "Could not resolve to a node with the global id of 'PR_7'",
	} {
		t.Run(typ, func(t *testing.T) {
			now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			c, f, pr, claim, b := enqueueRefusalEngine(graphQLErrorValue(t, typ, msg), &now)
			var logs bytes.Buffer
			c.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			f.state.ReviewDecision = "REVIEW_REQUIRED"
			for pass := 0; pass < 2; pass++ {
				reason, err := c.tryNativeQueue(context.Background(), pr, claim)
				if err != nil || !strings.HasPrefix(reason, nativeQueueReasonEnqueue+": ") || !strings.Contains(reason, msg) {
					t.Fatalf("pass %d: reason=%q err=%v, want the backoff reason with GitHub's text and no error", pass, reason, err)
				}
			}
			if f.enqueues != 1 {
				t.Fatalf("enqueues=%d, want one attempt and then the wait", f.enqueues)
			}
			if len(b.errs) != 1 || !errors.Is(b.errs[0], effects.ErrNotApplied) {
				t.Fatalf("a definite refusal must reach the boundary as NotApplied, got %v", b.errs)
			}
			if got := strings.Count(logs.String(), "automerge native queue enqueue refused, backing off"); got != 1 {
				t.Fatalf("want one WARN for the wait, not one per pass, got %d in:\n%s", got, logs.String())
			}
			if got := c.knownMergeRefusal(nativeQueueMergeAttempt(claim.Repo, pr, f.state)); got != "" {
				t.Fatalf("no review refusal may be remembered for it, got %q", got)
			}
		})
	}
}

// A new head under the same outstanding review gets its own one enqueue.
func TestNativeQueueReviewRefusalRearmsOnNewHead(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	c, f, pr, claim, _ := enqueueRefusalEngine(graphQLErrorValue(t, "UNPROCESSABLE", unnamedRefusal), &now)
	f.state.ReviewDecision = "REVIEW_REQUIRED"
	for pass := 0; pass < 2; pass++ {
		if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || reason != autoMergeReasonMergeRefusedReview {
			t.Fatalf("pass %d: reason=%q err=%v", pass, reason, err)
		}
	}
	f.state.HeadRefOid = "sha8"
	claim.Inputs["expect_sha"] = "sha8"
	for pass := 0; pass < 2; pass++ {
		if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || reason != autoMergeReasonMergeRefusedReview {
			t.Fatalf("new head, pass %d: reason=%q err=%v", pass, reason, err)
		}
	}
	if f.enqueues != 2 {
		t.Fatalf("enqueues=%d, want one per head", f.enqueues)
	}
}

// GitHub's reviewDecision alone never holds a head. An App the base's
// rules let past the review is enqueued although the decision reads
// REVIEW_REQUIRED (or CHANGES_REQUESTED): the first pass sends the enqueue,
// GitHub accepts it, and nothing is remembered, exactly as before.
func TestNativeQueueOutstandingReviewDoesNotHoldAnAcceptedEnqueue(t *testing.T) {
	for _, decision := range []string{"REVIEW_REQUIRED", "CHANGES_REQUESTED"} {
		t.Run(decision, func(t *testing.T) {
			c, f, pr, claim := nativeQueueEngine()
			b := &recordingBoundary{}
			c.mutation = b
			f.state.ReviewDecision = decision
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") {
				t.Fatalf("reason=%q err=%v, want the PR queued on the first pass", reason, err)
			}
			if f.enqueues != 1 || f.expectedHead != "sha7" || len(b.claims) != 1 {
				t.Fatalf("enqueues=%d head=%q claims=%+v, want the one enqueue", f.enqueues, f.expectedHead, b.claims)
			}
			attempt := nativeQueueMergeAttempt(claim.Repo, pr, f.state)
			if got := c.knownNativeQueueEnqueueRefusal(attempt); got != "" {
				t.Fatalf("an accepted enqueue must leave nothing remembered, got %q", got)
			}
			if got := c.knownMergeRefusal(attempt); got != "" {
				t.Fatalf("an accepted enqueue must leave no merge refusal remembered, got %q", got)
			}
		})
	}
}

// reviewDecision names a refusal only when GitHub definitely refused. A
// failure that may or may not have queued the PR stays an error and an
// uncertain outcome, whatever the decision says.
func TestNativeQueueUncertainFailureUnderOutstandingReviewIsNotAReviewRefusal(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	failed := errors.New(`Post "https://api.github.invalid/graphql": dial tcp: connection refused`)
	c, f, pr, claim, b := enqueueRefusalEngine(failed, &now)
	f.state.ReviewDecision = "REVIEW_REQUIRED"
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if !errors.Is(err, failed) || reason != nativeQueueReasonEnqueue || f.enqueues != 1 {
		t.Fatalf("reason=%q err=%v enqueues=%d, want %q with the error", reason, err, f.enqueues, nativeQueueReasonEnqueue)
	}
	if len(b.errs) != 1 || errors.Is(b.errs[0], effects.ErrNotApplied) {
		t.Fatalf("an unproven failure must stay an uncertain outcome at the boundary, got %v", b.errs)
	}
}

// A base that requires no review reports no reviewDecision, and an approved
// PR reports APPROVED: both enqueue exactly as before.
func TestNativeQueueReviewDecisionApprovedOrAbsentEnqueues(t *testing.T) {
	for _, decision := range []string{"", "APPROVED"} {
		t.Run("decision="+decision, func(t *testing.T) {
			c, f, pr, claim := nativeQueueEngine()
			f.state.ReviewDecision = decision
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") || f.enqueues != 1 {
				t.Fatalf("reason=%q err=%v enqueues=%d, want the PR queued", reason, err, f.enqueues)
			}
		})
	}
}

// Nothing here touches a PR already in the queue. Whatever its
// reviewDecision says (a person queued it, or the hive did before a review
// was dismissed), it keeps its entry and its queued reason, and the review
// hold is not a reason to withdraw the hive's own entry.
func TestNativeQueueReviewDecisionLeavesQueuedPRAlone(t *testing.T) {
	for _, enqueuer := range []string{"maintainer", testHiveAppBotLogin} {
		t.Run(enqueuer, func(t *testing.T) {
			c, f, pr, claim := nativeQueueEngine()
			f.state.ReviewDecision = "REVIEW_REQUIRED"
			f.state.IsInMergeQueue = true
			f.state.MergeQueueEntry = queueEntry(enqueuer)
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || !strings.HasPrefix(reason, nativeQueueReason) {
				t.Fatalf("reason=%q err=%v, want the queued reason", reason, err)
			}
			c.reconcileNativeQueueGate(context.Background(), stageLaneSelf, "acme", "widget", pr, autoMergeReasonMergeRefusedReview)
			if f.enqueues != 0 || f.dequeues != 0 {
				t.Fatalf("enqueues=%d dequeues=%d, want the entry left alone", f.enqueues, f.dequeues)
			}
		})
	}
}

// reviewDecision can lag or miss a rule, so GitHub may still refuse the
// enqueue for a missing review. The refusal is definite (NotApplied at the
// boundary, no uncertain outcome in the journal), reported as the review
// hold with no error, and remembered for the head: later passes send no
// mutation until the recheck window passes or the head moves. On the old
// code it came back as native-merge-queue-enqueue with the error and the
// same head was enqueued on every pass.
func TestNativeQueueEnqueueReviewRefusalIsRememberedPerHead(t *testing.T) {
	for name, refusal := range map[string]func(*testing.T) error{
		"graphql error value": func(t *testing.T) error {
			return graphQLErrorValue(t, "UNPROCESSABLE", "Pull request "+reviewRequiredMessage)
		},
		"flat string": func(*testing.T) error {
			return errors.New("graphql: UNPROCESSABLE: Pull request " + reviewRequiredMessage)
		},
		"two reviews": func(t *testing.T) error {
			return graphQLErrorValue(t, "UNPROCESSABLE", "At least 2 approving reviews are required by reviewers with write access.")
		},
		"changes requested": func(t *testing.T) error {
			return graphQLErrorValue(t, "UNPROCESSABLE", "1 review requesting changes by reviewers with write access.")
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			c, f, pr, claim, b := enqueueRefusalEngine(refusal(t), &now)
			for pass := 0; pass < 3; pass++ {
				reason, err := c.tryNativeQueue(context.Background(), pr, claim)
				if err != nil || reason != autoMergeReasonMergeRefusedReview {
					t.Fatalf("pass %d: reason=%q err=%v, want %q and no error", pass, reason, err, autoMergeReasonMergeRefusedReview)
				}
				now = now.Add(time.Minute)
			}
			if f.enqueues != 1 {
				t.Fatalf("enqueues=%d, want one attempt for the refused head", f.enqueues)
			}
			if len(b.errs) != 1 || !errors.Is(b.errs[0], effects.ErrNotApplied) {
				t.Fatalf("the refused enqueue must reach the boundary as NotApplied, got %v", b.errs)
			}
			now = now.Add(mergeRefusalRecheck)
			if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || reason != autoMergeReasonMergeRefusedReview || f.enqueues != 2 {
				t.Fatalf("after the recheck window: reason=%q err=%v enqueues=%d, want one more attempt", reason, err, f.enqueues)
			}
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonMergeRefusedReview || f.enqueues != 2 {
				t.Fatalf("inside the next window: reason=%q enqueues=%d, want no attempt", reason, f.enqueues)
			}
			f.state.HeadRefOid = "sha8"
			claim.Inputs["expect_sha"] = "sha8"
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonMergeRefusedReview || f.enqueues != 3 {
				t.Fatalf("new head: reason=%q enqueues=%d, want a fresh attempt", reason, f.enqueues)
			}
		})
	}
}

// A remembered review refusal re-arms at once when what GitHub says about
// the PR changes: its mergeable_state, as on the direct merge path, or its
// reviewDecision, which the queue path reads on every pass.
func TestNativeQueueEnqueueReviewRefusalRearmsOnStateChange(t *testing.T) {
	refusal := errors.New("graphql: UNPROCESSABLE: Pull request " + reviewRequiredMessage)
	for name, change := range map[string]func(*nativeQueueFake, *gh.PullRequest){
		"mergeable_state": func(_ *nativeQueueFake, pr *gh.PullRequest) { pr.MergeableState = gh.Ptr("clean") },
		"reviewDecision":  func(f *nativeQueueFake, _ *gh.PullRequest) { f.state.ReviewDecision = "APPROVED" },
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			c, f, pr, claim, _ := enqueueRefusalEngine(refusal, &now)
			pr.MergeableState = gh.Ptr("blocked")
			for pass := 0; pass < 2; pass++ {
				if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || reason != autoMergeReasonMergeRefusedReview {
					t.Fatalf("pass %d: reason=%q err=%v", pass, reason, err)
				}
			}
			if f.enqueues != 1 {
				t.Fatalf("enqueues=%d, want one attempt before the change", f.enqueues)
			}
			// GitHub computing the merge again says nothing about a review.
			pr.MergeableState = gh.Ptr("unknown")
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonMergeRefusedReview || f.enqueues != 1 {
				t.Fatalf("mergeable_state unknown: reason=%q enqueues=%d, want the refusal kept", reason, f.enqueues)
			}
			pr.MergeableState = gh.Ptr("blocked")
			change(f, pr)
			f.enqueueErr = nil
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || !strings.Contains(reason, "queued") || f.enqueues != 2 {
				t.Fatalf("after the change: reason=%q err=%v enqueues=%d, want the PR queued at once", reason, err, f.enqueues)
			}
		})
	}
}

// An enqueue GitHub definitely refuses for a reason the hive has no name
// for, while its reviewDecision shows no review outstanding, is not sent
// again on every pass: the same head waits out a delay that
// doubles from nativeQueueEnqueueBackoffBase, and the reason carries the
// error text. A definite refusal is NotApplied at the boundary and is not
// an error to the lane. A new head starts over. On the old code every pass
// sent the mutation and returned the error.
func TestNativeQueueUnclassifiedRefusalBacksOff(t *testing.T) {
	const unknown = unnamedRefusal
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	c, f, pr, claim, b := enqueueRefusalEngine(graphQLErrorValue(t, "UNPROCESSABLE", unknown), &now)
	pass := func(step string, wantEnqueues int) string {
		t.Helper()
		reason, err := c.tryNativeQueue(context.Background(), pr, claim)
		if err != nil || !strings.HasPrefix(reason, nativeQueueReasonEnqueue+": ") || !strings.Contains(reason, unknown) {
			t.Fatalf("%s: reason=%q err=%v, want the %s family with GitHub's error text and no error", step, reason, err, nativeQueueReasonEnqueue)
		}
		if f.enqueues != wantEnqueues {
			t.Fatalf("%s: enqueues=%d, want %d", step, f.enqueues, wantEnqueues)
		}
		return reason
	}
	first := pass("first pass", 1)
	if len(b.errs) != 1 || !errors.Is(b.errs[0], effects.ErrNotApplied) {
		t.Fatalf("a definite refusal must reach the boundary as NotApplied, got %v", b.errs)
	}
	if !stageengine.MergeQueueReason(first) || nativeQueueWithdrawReason(first) {
		t.Fatalf("reason %q must stay in the merge-queue family and never withdraw an entry", first)
	}
	if !strings.HasSuffix(first, "), next try 2m after it") {
		t.Fatalf("reason %q must end in the hive's own words, with GitHub's text inside it", first)
	}
	// Stage 100, but the head is not a ready front for a merge train.
	if got := mergeTrainReadinessOf(first); got != mergeTrainReadinessUnready {
		t.Fatalf("mergeTrainReadinessOf(%q) = %d, want unready", first, got)
	}
	if again := pass("second pass, immediately after", 1); again != first {
		t.Fatalf("the reason must be stable inside one wait: %q then %q", first, again)
	}
	now = now.Add(nativeQueueEnqueueBackoffBase - time.Second)
	pass("just inside the first wait", 1)
	now = now.Add(time.Second)
	second := pass("after the first wait", 2)
	if second == first {
		t.Fatalf("the reason must name the longer wait, got %q twice", second)
	}
	// The wait doubled: the base delay is no longer enough.
	now = now.Add(nativeQueueEnqueueBackoffBase)
	pass("one base delay after the second refusal", 2)
	now = now.Add(nativeQueueEnqueueBackoffBase)
	pass("two base delays after the second refusal", 3)

	// A new head starts over: tried at once, then the base delay again.
	f.state.HeadRefOid = "sha8"
	claim.Inputs["expect_sha"] = "sha8"
	if reason := pass("new head", 4); reason != first {
		t.Fatalf("new head: reason=%q, want the first wait again (%q)", reason, first)
	}
	pass("new head, immediately after", 4)
	now = now.Add(nativeQueueEnqueueBackoffBase)
	pass("new head, after the base delay", 5)

	// So does a change in what GitHub says about the PR's reviews.
	f.state.ReviewDecision = "APPROVED"
	if reason := pass("reviewDecision changed", 6); reason != first {
		t.Fatalf("reviewDecision changed: reason=%q, want the first wait again (%q)", reason, first)
	}

	// Once GitHub accepts the enqueue nothing is left to wait out.
	f.enqueueErr = nil
	now = now.Add(nativeQueueEnqueueBackoffBase)
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") || f.enqueues != 7 {
		t.Fatalf("accepted: reason=%q err=%v enqueues=%d, want the PR queued", reason, err, f.enqueues)
	}
	if got := c.knownNativeQueueEnqueueRefusal(nativeQueueMergeAttempt(claim.Repo, pr, f.state)); got != "" {
		t.Fatalf("an accepted enqueue must clear the backoff, got %q", got)
	}
}

// The delay doubles from the base and stops at a cap: an hour after an
// enqueue GitHub definitely refused, and 15 minutes after a failure that
// may yet pass, which keeps a waiting retry under the mutation journal's
// 30-minute stuck-operation report.
func TestNativeQueueEnqueueBackoffDoublesToCap(t *testing.T) {
	for _, tc := range []struct {
		refused bool
		limit   time.Duration
	}{
		{true, time.Hour},
		{false, 15 * time.Minute},
	} {
		want := nativeQueueEnqueueBackoffBase
		for n := 1; n <= 40; n++ {
			if got := nativeQueueEnqueueBackoff(n, tc.refused); got != want {
				t.Fatalf("refused=%v, failed try %d: wait=%s, want %s", tc.refused, n, got, want)
			}
			if want *= 2; want > tc.limit {
				want = tc.limit
			}
		}
		if got := nativeQueueEnqueueBackoff(0, tc.refused); got != nativeQueueEnqueueBackoffBase {
			t.Fatalf("refused=%v, no failed try: wait=%s, want the base", tc.refused, got)
		}
	}
	if nativeQueueEnqueueUncertainMax >= mutation.DefaultStuckAfter {
		t.Fatalf("an uncertain failure may wait %s, which must stay under the stuck-operation report (%s)", nativeQueueEnqueueUncertainMax, mutation.DefaultStuckAfter)
	}
}

// A failure that keeps an uncertain outcome is retried on the short cap
// however long it lasts: 2, 4, 8, then every 15 minutes.
func TestNativeQueueUncertainFailureWaitsAtMostFifteenMinutes(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	c, f, pr, claim, _ := enqueueRefusalEngine(errors.New("dial tcp: connection reset by peer"), &now)
	for i, wait := range []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		if _, err := c.tryNativeQueue(context.Background(), pr, claim); err == nil || f.enqueues != i+1 {
			t.Fatalf("try %d: err=%v enqueues=%d, want the failed attempt", i+1, err, f.enqueues)
		}
		now = now.Add(wait - time.Second)
		reason, err := c.tryNativeQueue(context.Background(), pr, claim)
		if err != nil || !strings.HasPrefix(reason, nativeQueueReasonEnqueue+": ") || f.enqueues != i+1 {
			t.Fatalf("try %d, just inside its %s wait: reason=%q err=%v enqueues=%d, want no mutation", i+1, wait, reason, err, f.enqueues)
		}
		now = now.Add(time.Second)
	}
}

// GitHub's text sits inside the reason, never at its end, so text that
// happens to end like a CI reason ("-pending", "-skipped") is not read as
// one.
func TestNativeQueueEnqueueBackoffReasonEndsInFixedWords(t *testing.T) {
	for _, tail := range []string{"-pending", "-neutral", "-skipped"} {
		reason := nativeQueueEnqueueBackoffReason(4*time.Minute, "graphql: UNPROCESSABLE: required check is build"+tail)
		if !strings.HasSuffix(reason, "), next try 4m after it") || classifyCIReason(reason) != stageengine.CIReasonNone {
			t.Fatalf("reason %q must not be read as a CI reason", reason)
		}
		if !stageengine.MergeQueueReason(reason) {
			t.Fatalf("reason %q must stay in the merge-queue family", reason)
		}
	}
}

// The error text a backoff reason carries is one bounded line that cannot
// close a code span or mention anyone.
func TestNativeQueueEnqueueDetailIsOneSafeLine(t *testing.T) {
	got := nativeQueueEnqueueDetail(errors.New("graphql: UNPROCESSABLE: first line\n\tsecond `line` from user@example.invalid"))
	if want := "graphql: UNPROCESSABLE: first line second 'line' from user(at)example.invalid"; got != want {
		t.Fatalf("detail=%q, want %q", got, want)
	}
	long := nativeQueueEnqueueDetail(errors.New(strings.Repeat("x", 3*nativeQueueEnqueueDetailMax)))
	if want := strings.Repeat("x", nativeQueueEnqueueDetailMax) + "..."; long != want {
		t.Fatalf("a long error must be cut to %d characters, got %d", nativeQueueEnqueueDetailMax, len(long))
	}
}

// A failure that does not prove GitHub turned the enqueue down (a network
// error, a GraphQL errors array with no refusal type, a rate limit) stays an
// uncertain outcome at the boundary and is still an error to the lane, as
// before. It backs off like any other failed enqueue, so a failure that
// persists is not one mutation per pass.
func TestNativeQueueUncertainEnqueueErrorStaysUncertain(t *testing.T) {
	for name, failure := range map[string]func(*testing.T) error{
		"network error": func(*testing.T) error {
			return errors.New(`Post "https://api.github.invalid/graphql": dial tcp: connection refused`)
		},
		"untyped graphql error": func(t *testing.T) error {
			return graphQLErrorValue(t, "", "the server could not answer")
		},
		"rate limited": func(t *testing.T) error {
			return graphQLErrorValue(t, "RATE_LIMITED", "API rate limit exceeded")
		},
		"server error": func(*testing.T) error {
			return fmt.Errorf("enqueue: %w", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusBadGateway}, Message: "Server Error"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
			failed := failure(t)
			c, f, pr, claim, b := enqueueRefusalEngine(failed, &now)
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if !errors.Is(err, failed) || reason != nativeQueueReasonEnqueue || f.enqueues != 1 {
				t.Fatalf("first pass: reason=%q err=%v enqueues=%d, want %q with the error", reason, err, f.enqueues, nativeQueueReasonEnqueue)
			}
			if len(b.errs) != 1 || b.errs[0] == nil || errors.Is(b.errs[0], effects.ErrNotApplied) {
				t.Fatalf("an unproven failure must stay an uncertain outcome at the boundary, got %v", b.errs)
			}
			reason, err = c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || !strings.HasPrefix(reason, nativeQueueReasonEnqueue+": ") || f.enqueues != 1 {
				t.Fatalf("second pass: reason=%q err=%v enqueues=%d, want the wait and no mutation", reason, err, f.enqueues)
			}
			wait := reason
			// One transient failure costs one base wait, no more: the
			// next try is accepted and the backoff is gone.
			now = now.Add(nativeQueueEnqueueBackoffBase)
			f.enqueueErr = nil
			reason, err = c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") || f.enqueues != 2 {
				t.Fatalf("after the base wait: reason=%q err=%v enqueues=%d, want the PR queued", reason, err, f.enqueues)
			}
			if got := c.knownNativeQueueEnqueueRefusal(nativeQueueMergeAttempt(claim.Repo, pr, f.state)); got != "" {
				t.Fatalf("an accepted enqueue must clear the backoff, got %q", got)
			}
			// A later failure on the same head starts from the base wait
			// again, not from a doubled one.
			f.state.IsInMergeQueue, f.state.MergeQueueEntry = false, nil
			f.enqueueErr = failed
			if _, err = c.tryNativeQueue(context.Background(), pr, claim); !errors.Is(err, failed) || f.enqueues != 3 {
				t.Fatalf("a later failure: err=%v enqueues=%d, want one attempt", err, f.enqueues)
			}
			if reason, err = c.tryNativeQueue(context.Background(), pr, claim); err != nil || reason != wait || f.enqueues != 3 {
				t.Fatalf("a later failure, next pass: reason=%q err=%v enqueues=%d, want the base wait again (%q)", reason, err, f.enqueues, wait)
			}
		})
	}
}

// A failed enqueue whose outcome was uncertain may have queued the PR after
// all. Once the PR is seen in the queue the backoff is dropped, so a later
// removal of the same head is enqueued at once.
func TestNativeQueueBackoffDroppedOnceQueued(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	c, f, pr, claim, _ := enqueueRefusalEngine(errors.New("dial tcp: connection reset by peer"), &now)
	if _, err := c.tryNativeQueue(context.Background(), pr, claim); err == nil || f.enqueues != 1 {
		t.Fatalf("first pass: err=%v enqueues=%d, want the failure", err, f.enqueues)
	}
	f.state.IsInMergeQueue, f.state.MergeQueueEntry = true, queueEntry(testHiveAppBotLogin)
	if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || !strings.HasPrefix(reason, nativeQueueReason) || f.enqueues != 1 {
		t.Fatalf("queued after all: reason=%q err=%v enqueues=%d, want the queued reason and no mutation", reason, err, f.enqueues)
	}
	f.state.IsInMergeQueue, f.state.MergeQueueEntry = false, nil
	f.enqueueErr = nil
	if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || !strings.Contains(reason, "queued") || f.enqueues != 2 {
		t.Fatalf("removed again: reason=%q err=%v enqueues=%d, want it enqueued at once", reason, err, f.enqueues)
	}
}

// The backoff counts enqueue calls GitHub answered or failed. A mutation
// boundary that denies the write sent nothing, so it starts no wait: the
// next pass may enqueue.
func TestNativeQueueDeniedBoundaryStartsNoBackoff(t *testing.T) {
	c, f, pr, claim := nativeQueueEngine()
	c.mutation = denyNativeQueueBoundary{}
	for pass := 0; pass < 2; pass++ {
		if reason, err := c.tryNativeQueue(context.Background(), pr, claim); !errors.Is(err, effects.ErrDenied) || reason != nativeQueueReasonEnqueue {
			t.Fatalf("pass %d: reason=%q err=%v, want the boundary's denial", pass, reason, err)
		}
	}
	c.mutation = &recordingBoundary{}
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if err != nil || !strings.Contains(reason, "queued") || f.enqueues != 1 {
		t.Fatalf("after the denial lifts: reason=%q err=%v enqueues=%d, want the PR queued at once", reason, err, f.enqueues)
	}
}

// The review matcher sees every form the refusal can reach the lane in, and
// nothing else.
func TestNativeQueueEnqueueRefusedReview(t *testing.T) {
	gql := graphQLErrorValue(t, "UNPROCESSABLE", "Pull request "+reviewRequiredMessage)
	rest := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusMethodNotAllowed}, Message: reviewRequiredMessage}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{gql, true},
		{effects.NotApplied(gql), true},
		{fmt.Errorf("effect outcome uncertain, reconciliation required: %w", gql), true},
		{rest, true},
		{fmt.Errorf("merging PR: %w", rest), true},
		{errors.New("graphql: UNPROCESSABLE: Pull request " + reviewRequiredMessage), true},
		{errors.New("graphql: UNPROCESSABLE: At least 2 approving reviews are required by reviewers with write access."), true},
		{errors.New("graphql: UNPROCESSABLE: 1 review requesting changes by reviewers with write access."), true},
		{errUnresolvedConversations, false},
		{graphQLErrorValue(t, "UNPROCESSABLE", "Pull request Required status check \"build\" is expected."), false},
		{errors.New("expectedHeadOid mismatch"), false},
		{nil, false},
	} {
		if got := nativeQueueEnqueueRefusedReview(tc.err); got != tc.want {
			t.Errorf("nativeQueueEnqueueRefusedReview(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

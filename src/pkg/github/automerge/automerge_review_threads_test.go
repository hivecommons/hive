package automerge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// errUnresolvedConversations is GitHub's GraphQL enqueue refusal as the
// client's graphql error type renders it (seen on
// projectbluefin/ps-printer-app#221, 2026-11-20).
var errUnresolvedConversations = errors.New("graphql: UNPROCESSABLE: Pull request All comments must be resolved")

// errConversationMustBeResolved is GitHub's other wording of the same
// enqueue refusal, as the client renders it (seen in a production hive's
// contributor lane, 2026-11-27). It did not match, so the refusal came back
// as "effect outcome uncertain, reconciliation required" and the lane
// enqueued the same head on every sweep.
var errConversationMustBeResolved = errors.New("graphql: UNPROCESSABLE: Pull request A conversation must be resolved before this pull request can be merged.")

// threadResolverFake is a native-queue transport that can also resolve the
// hive's own review threads, the way *hgithub.Client does.
type threadResolverFake struct {
	*nativeQueueFake
	outcome hgithub.OwnReviewThreadsOutcome
	err     error
	calls   int
	// clearOnResolve makes a Cleared outcome lift the enqueue refusal, as
	// resolving the last open thread does on GitHub.
	clearOnResolve bool
	// evidence is what the engine passed on each call.
	evidence []hgithub.OwnThreadsEvidence
}

func (f *threadResolverFake) ResolveOwnReviewThreadsForMerge(_ context.Context, _ string, _ int, evidence hgithub.OwnThreadsEvidence) (hgithub.OwnReviewThreadsOutcome, error) {
	f.calls++
	f.evidence = append(f.evidence, evidence)
	if f.err == nil && f.clearOnResolve && f.outcome.Cleared() {
		f.enqueueErr = nil
	}
	return f.outcome, f.err
}

// effectErrBoundary runs the effect and keeps the error it returned, so a
// test can see whether the boundary was told the effect definitely did not
// apply.
type effectErrBoundary struct{ errs []error }

func (b *effectErrBoundary) Execute(ctx context.Context, _ effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	out, err := effect(ctx)
	b.errs = append(b.errs, err)
	return out, err
}

func threadResolverEngine(outcome hgithub.OwnReviewThreadsOutcome) (*Engine, *threadResolverFake, *gh.PullRequest, effects.Claim, *effectErrBoundary) {
	c, f, pr, claim := nativeQueueEngine()
	f.enqueueErr = errUnresolvedConversations
	r := &threadResolverFake{nativeQueueFake: f, outcome: outcome, clearOnResolve: true}
	c.transport = r
	b := &effectErrBoundary{}
	c.mutation = b
	return c, r, pr, claim, b
}

// Every open thread is the hive's own and resolvable: the lane resolves
// them, reports review-threads-resolved (no error, so no "effect outcome
// uncertain" retry loop), and the next pass enqueues the same head. The
// refused enqueue reaches the boundary as a definite NotApplied. On the old
// code the refusal came back as native-merge-queue-enqueue with the error,
// on every pass, and nothing resolved the threads.
func TestNativeQueueUnresolvedConversationsResolvesOwnThreadsThenEnqueues(t *testing.T) {
	c, r, pr, claim, b := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{Unresolved: 2, Resolvable: 2, Resolved: 2})
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if err != nil || reason != autoMergeReasonReviewThreadsResolved {
		t.Fatalf("first pass: reason=%q err=%v, want %q and no error", reason, err, autoMergeReasonReviewThreadsResolved)
	}
	if r.calls != 1 || r.enqueues != 1 {
		t.Fatalf("resolver calls=%d enqueues=%d, want 1 and 1", r.calls, r.enqueues)
	}
	if len(b.errs) != 1 || !errors.Is(b.errs[0], effects.ErrNotApplied) {
		t.Fatalf("the refused enqueue must reach the boundary as NotApplied, got %v", b.errs)
	}
	if got := c.knownUnresolvedThreads(claim.Repo, 7, "sha7"); got != "" {
		t.Fatalf("a cleared refusal must not be remembered, got %q", got)
	}
	reason, err = c.tryNativeQueue(context.Background(), pr, claim)
	if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") || r.enqueues != 2 {
		t.Fatalf("second pass: reason=%q err=%v enqueues=%d, want the PR queued", reason, err, r.enqueues)
	}
}

// The PR this rule comes from: the only open thread was the hive's own
// outdated low note, answered by the PR's author with "Addressed in <sha>",
// and the hive's review of the head approves. The client resolves it under
// the author-reply rule (own_author_answered 1), the lane reports
// review-threads-resolved and the next pass enqueues the same head. The
// lane hands the client the head it tried to merge and when the hive's
// review approved it, from the reviewer verdict for exactly that head. On
// the old rule the reply made the thread own_answered and the PR parked at
// 085 for a person.
func TestNativeQueueUnresolvedConversationsAuthorAnsweredNoteResolvesThenEnqueues(t *testing.T) {
	approvedAt := time.Date(2026, 11, 27, 17, 9, 0, 0, time.UTC)
	c, r, pr, claim, _ := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Resolvable: 1, OwnAuthorAnswered: 1, Resolved: 1})
	r.enqueueErr = errConversationMustBeResolved
	var asked []string
	c.contributorLaneVerdict = func(repo string, number int, head string) ContributorLaneVerdict {
		asked = append(asked, fmt.Sprintf("%s#%d@%s", repo, number, head))
		return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve", ReviewAskedAt: approvedAt}
	}
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if err != nil || reason != autoMergeReasonReviewThreadsResolved {
		t.Fatalf("first pass: reason=%q err=%v, want %q and no error", reason, err, autoMergeReasonReviewThreadsResolved)
	}
	if len(r.evidence) != 1 || r.evidence[0].HeadSHA != "sha7" || !r.evidence[0].HeadReviewAskedAt.Equal(approvedAt) {
		t.Fatalf("evidence = %+v, want head sha7 approved at %s", r.evidence, approvedAt)
	}
	if want := []string{"acme/widget#7@sha7"}; fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Fatalf("verdict asked for %v, want %v", asked, want)
	}
	if got := c.knownUnresolvedThreads(claim.Repo, 7, "sha7"); got != "" {
		t.Fatalf("a cleared refusal must not be remembered, got %q", got)
	}
	reason, err = c.tryNativeQueue(context.Background(), pr, claim)
	if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") || r.enqueues != 2 {
		t.Fatalf("second pass: reason=%q err=%v enqueues=%d, want the PR queued", reason, err, r.enqueues)
	}
}

// The evidence names an approval only when the reviewer verdict for the
// head is an approve with a recorded time. Anything else names the head
// alone, and the client then accepts only an outdated thread. Every merge
// path that can be refused for conversations passes the same evidence: the
// direct merge and the serialized lane here, the native queue above.
func TestUnresolvedThreadsRefusalPassesHeadEvidence(t *testing.T) {
	approvedAt := time.Date(2026, 11, 27, 17, 9, 0, 0, time.UTC)
	refusal := unresolvedConversations405("Repository rule violations found\n\nAll comments must be resolved.")
	for _, tc := range []struct {
		name    string
		verdict *ContributorLaneVerdict
		want    time.Time
	}{
		{"approved", &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve", ReviewAskedAt: approvedAt}, approvedAt},
		{"approve carried with no time", &ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}, time.Time{}},
		{"changes requested", &ContributorLaneVerdict{Configured: true, Verdict: "changes_requested", ReviewAskedAt: approvedAt}, time.Time{}},
		{"no verdict reader", nil, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := func() (*Engine, *threadResolverFake) {
				c := newAutoMergeSweepClient("http://unused.invalid")
				r := &threadResolverFake{nativeQueueFake: &nativeQueueFake{Transport: c.transport}, outcome: hgithub.OwnReviewThreadsOutcome{Unresolved: 1, OwnAnswered: 1}}
				c.transport = r
				c.contributorLaneVerdict = nil
				if tc.verdict != nil {
					v := *tc.verdict
					c.contributorLaneVerdict = func(string, int, string) ContributorLaneVerdict { return v }
				}
				return c, r
			}
			check := func(path string, r *threadResolverFake) {
				t.Helper()
				if len(r.evidence) != 1 || r.evidence[0].HeadSHA != "sha7" || !r.evidence[0].HeadReviewAskedAt.Equal(tc.want) {
					t.Fatalf("%s: evidence = %+v, want head sha7 approved at %s", path, r.evidence, tc.want)
				}
			}
			c, r := engine()
			attempt := mergeAttempt{owner: "acme", repo: "widget", base: "main", number: 7, head: "sha7", state: "blocked", method: "squash"}
			if got := c.refusedMerge(context.Background(), attempt, refusal); got != autoMergeReasonUnresolvedReviewThreads {
				t.Fatalf("refusedMerge = %q, want %q", got, autoMergeReasonUnresolvedReviewThreads)
			}
			check("direct merge", r)

			c, r = engine()
			gate := func(context.Context, hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
				return hgithub.LaneMergeResult{Outcome: hgithub.LaneOutcomeLeft, Reason: "merge refused by GitHub: " + refusal.Error(), MergeError: refusal}, nil
			}
			event := AutoMergeSweepEvent{Repo: "acme/widget", Number: 7, HeadSHA: "sha7"}
			if _, reason, err := c.mergeThroughLane(context.Background(), gate, "acme", "widget", "main", hgithub.PRAuditPathSweep, "squash", event, nil); err != nil || reason != autoMergeReasonUnresolvedReviewThreads {
				t.Fatalf("serialized lane: reason=%q err=%v, want %q", reason, err, autoMergeReasonUnresolvedReviewThreads)
			}
			check("serialized lane", r)
		})
	}
}

// A thread the hive may not resolve (a person's, another bot's, one someone
// replied in, a blocking finding) parks the PR with the named reason and no
// error, and the lane does not ask GitHub again for that head until it moves
// or the recheck window passes.
func TestNativeQueueUnresolvedConversationsParksAndBacksOff(t *testing.T) {
	now := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	for name, outcome := range map[string]hgithub.OwnReviewThreadsOutcome{
		"person's thread":      {Unresolved: 2, Resolvable: 1, Others: 1, Skipped: "not-every-thread-resolvable"},
		"blocking hive thread": {Unresolved: 1, OwnBlocking: 1},
		"hive thread answered": {Unresolved: 1, OwnAnswered: 1},
	} {
		t.Run(name, func(t *testing.T) {
			c, r, pr, claim, _ := threadResolverEngine(outcome)
			c.now = func() time.Time { return now }
			for pass := 0; pass < 3; pass++ {
				reason, err := c.tryNativeQueue(context.Background(), pr, claim)
				if err != nil || reason != autoMergeReasonUnresolvedReviewThreads {
					t.Fatalf("pass %d: reason=%q err=%v, want %q and no error", pass, reason, err, autoMergeReasonUnresolvedReviewThreads)
				}
			}
			if r.enqueues != 1 || r.calls != 1 {
				t.Fatalf("enqueues=%d resolver calls=%d, want one attempt for the parked head", r.enqueues, r.calls)
			}
			now = now.Add(mergeRefusalRecheck)
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonUnresolvedReviewThreads || r.enqueues != 2 {
				t.Fatalf("after the recheck window: reason=%q enqueues=%d, want one more attempt", reason, r.enqueues)
			}
			r.state.HeadRefOid = "sha8"
			claim.Inputs["expect_sha"] = "sha8"
			r.enqueueErr = nil
			if reason, err := c.tryNativeQueue(context.Background(), pr, claim); err != nil || !strings.Contains(reason, "queued") || r.enqueues != 3 {
				t.Fatalf("new head: reason=%q err=%v enqueues=%d, want it queued", reason, err, r.enqueues)
			}
		})
	}
}

// A refusal nothing explains — the thread read failed, GitHub still counts
// a thread a person just resolved (nothing unresolved is visible), or every
// resolve call failed, or the transport cannot read threads at all — is not
// parked at once: the PR waits at review-threads-pending and the lane asks
// GitHub again after unresolvedThreadsRetryAfter, not on every pass. After
// unresolvedThreadsRetryLimit such refusals on one head it parks for a
// person. A new head starts over.
func TestNativeQueueUnresolvedConversationsUnexplainedRetriesThenParks(t *testing.T) {
	for name, setup := range map[string]func() (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim, func() int){
		"nothing visible": func() (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim, func() int) {
			c, r, pr, claim, _ := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{})
			return c, r.nativeQueueFake, pr, claim, func() int { return r.calls }
		},
		"resolve calls failed": func() (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim, func() int) {
			c, r, pr, claim, _ := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Resolvable: 1, Failed: 1})
			return c, r.nativeQueueFake, pr, claim, func() int { return r.calls }
		},
		"thread read failed": func() (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim, func() int) {
			c, r, pr, claim, _ := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{})
			r.err = errors.New("graphql: rate limited")
			return c, r.nativeQueueFake, pr, claim, func() int { return r.calls }
		},
		"no resolver": func() (*Engine, *nativeQueueFake, *gh.PullRequest, effects.Claim, func() int) {
			c, f, pr, claim := nativeQueueEngine()
			f.enqueueErr = errUnresolvedConversations
			return c, f, pr, claim, func() int { return 0 }
		},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
			c, f, pr, claim, _ := setup()
			c.now = func() time.Time { return now }
			for attempt := 1; attempt <= unresolvedThreadsRetryLimit; attempt++ {
				want := autoMergeReasonReviewThreadsPending
				if attempt == unresolvedThreadsRetryLimit {
					want = autoMergeReasonUnresolvedReviewThreads
				}
				reason, err := c.tryNativeQueue(context.Background(), pr, claim)
				if err != nil || reason != want || f.enqueues != attempt {
					t.Fatalf("attempt %d: reason=%q err=%v enqueues=%d, want %q", attempt, reason, err, f.enqueues, want)
				}
				// Inside the wait, the lane does not ask GitHub again.
				if again, _ := c.tryNativeQueue(context.Background(), pr, claim); again != want || f.enqueues != attempt {
					t.Fatalf("attempt %d, same pass window: reason=%q enqueues=%d", attempt, again, f.enqueues)
				}
				now = now.Add(unresolvedThreadsRetryAfter)
			}
			// Parked: the retry window no longer re-arms it.
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonUnresolvedReviewThreads || f.enqueues != unresolvedThreadsRetryLimit {
				t.Fatalf("parked: reason=%q enqueues=%d", reason, f.enqueues)
			}
			// When the park expires the head gets one more try, and the
			// next unexplained refusal parks again at once: no fresh
			// pending retries, so no 085 → 100 → 085 cycle.
			for cycle := 1; cycle <= 2; cycle++ {
				now = now.Add(mergeRefusalRecheck)
				reason, err := c.tryNativeQueue(context.Background(), pr, claim)
				if err != nil || reason != autoMergeReasonUnresolvedReviewThreads || f.enqueues != unresolvedThreadsRetryLimit+cycle {
					t.Fatalf("after park %d expired: reason=%q err=%v enqueues=%d, want an immediate re-park", cycle, reason, err, f.enqueues)
				}
			}
			f.state.HeadRefOid = "sha8"
			claim.Inputs["expect_sha"] = "sha8"
			if reason, _ := c.tryNativeQueue(context.Background(), pr, claim); reason != autoMergeReasonReviewThreadsPending {
				t.Fatalf("new head: reason=%q, want a fresh retry", reason)
			}
		})
	}
}

// Nothing the hive may resolve: with review.severity.block_at: P3 (the
// opt-out) every hive thread is blocking, and a repo paused meanwhile
// resolves nothing either. The refusal parks at once rather than spending
// retries, and stays parked across park expiries.
func TestNativeQueueUnresolvedConversationsNothingResolvableParksAtOnce(t *testing.T) {
	for name, outcome := range map[string]hgithub.OwnReviewThreadsOutcome{
		"block_at P3": {Unresolved: 2, OwnBlocking: 2},
		"paused":      {Skipped: "paused"},
	} {
		t.Run(name, func(t *testing.T) { parksAtOnce(t, outcome) })
	}
}

func parksAtOnce(t *testing.T, outcome hgithub.OwnReviewThreadsOutcome) {
	t.Helper()
	now := time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC)
	c, r, pr, claim, _ := threadResolverEngine(outcome)
	c.now = func() time.Time { return now }
	for cycle := 1; cycle <= 3; cycle++ {
		reason, err := c.tryNativeQueue(context.Background(), pr, claim)
		if err != nil || reason != autoMergeReasonUnresolvedReviewThreads || r.enqueues != cycle {
			t.Fatalf("cycle %d: reason=%q err=%v enqueues=%d, want an immediate park", cycle, reason, err, r.enqueues)
		}
		if again, _ := c.tryNativeQueue(context.Background(), pr, claim); again != autoMergeReasonUnresolvedReviewThreads || r.enqueues != cycle {
			t.Fatalf("cycle %d inside the park: reason=%q enqueues=%d", cycle, again, r.enqueues)
		}
		now = now.Add(mergeRefusalRecheck)
	}
}

// The "A conversation must be resolved" wording is the same refusal: the
// enqueue reaches the boundary as a definite NotApplied (no uncertain
// outcome), and the hive's own outdated non-blocking note, the only open
// thread, is resolved so the next pass enqueues the same head. A person's
// thread parks the PR for a person, and the lane does not enqueue that head
// again while the park is remembered.
func TestNativeQueueConversationMustBeResolvedWording(t *testing.T) {
	t.Run("own outdated note only", func(t *testing.T) {
		c, r, pr, claim, b := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Resolvable: 1, Resolved: 1})
		r.enqueueErr = errConversationMustBeResolved
		reason, err := c.tryNativeQueue(context.Background(), pr, claim)
		if err != nil || reason != autoMergeReasonReviewThreadsResolved {
			t.Fatalf("first pass: reason=%q err=%v, want %q and no error", reason, err, autoMergeReasonReviewThreadsResolved)
		}
		if r.calls != 1 || r.enqueues != 1 {
			t.Fatalf("resolver calls=%d enqueues=%d, want 1 and 1", r.calls, r.enqueues)
		}
		if len(b.errs) != 1 || !errors.Is(b.errs[0], effects.ErrNotApplied) {
			t.Fatalf("the refused enqueue must reach the boundary as NotApplied, got %v", b.errs)
		}
		reason, err = c.tryNativeQueue(context.Background(), pr, claim)
		if err != nil || !strings.Contains(reason, "queued (position 2, AWAITING_CHECKS)") || r.enqueues != 2 {
			t.Fatalf("second pass: reason=%q err=%v enqueues=%d, want the PR queued", reason, err, r.enqueues)
		}
	})
	t.Run("person's thread", func(t *testing.T) {
		now := time.Date(2026, 11, 28, 7, 0, 0, 0, time.UTC)
		c, r, pr, claim, b := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{Unresolved: 2, Resolvable: 1, Others: 1, Skipped: "not-every-thread-resolvable"})
		r.enqueueErr = errConversationMustBeResolved
		c.now = func() time.Time { return now }
		for pass := 0; pass < 3; pass++ {
			reason, err := c.tryNativeQueue(context.Background(), pr, claim)
			if err != nil || reason != autoMergeReasonUnresolvedReviewThreads {
				t.Fatalf("pass %d: reason=%q err=%v, want %q and no error", pass, reason, err, autoMergeReasonUnresolvedReviewThreads)
			}
		}
		if r.enqueues != 1 || r.calls != 1 {
			t.Fatalf("enqueues=%d resolver calls=%d, want one attempt for the parked head", r.enqueues, r.calls)
		}
		if len(b.errs) != 1 || !errors.Is(b.errs[0], effects.ErrNotApplied) {
			t.Fatalf("the refused enqueue must reach the boundary as NotApplied, got %v", b.errs)
		}
		if got := c.knownUnresolvedThreads(claim.Repo, 7, "sha7"); got != autoMergeReasonUnresolvedReviewThreads {
			t.Fatalf("knownUnresolvedThreads = %q, want the park remembered", got)
		}
	})
}

// Any other enqueue failure is unchanged: still an error with the old
// reason, and nothing is asked of the thread resolver.
func TestNativeQueueOtherEnqueueErrorUnchanged(t *testing.T) {
	c, r, pr, claim, _ := threadResolverEngine(hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Resolvable: 1, Resolved: 1})
	r.enqueueErr = errors.New("expectedHeadOid mismatch")
	reason, err := c.tryNativeQueue(context.Background(), pr, claim)
	if err == nil || reason != nativeQueueReasonEnqueue || r.calls != 0 {
		t.Fatalf("reason=%q err=%v calls=%d", reason, err, r.calls)
	}
}

func unresolvedConversations405(msg string) error {
	return fmt.Errorf("merging: %w", &gh.ErrorResponse{
		Response: &http.Response{StatusCode: http.StatusMethodNotAllowed},
		Message:  msg,
	})
}

// The REST merge 405 on a base that requires conversation resolution but no
// merge queue: named and remembered for the head like the other permanent
// refusals, and resolved away when the open threads are the hive's own. A
// base that also requires the merge queue is still merge-queue-required:
// the lane enqueues it next and the queue path takes over.
func TestRefusedMergeUnresolvedConversations(t *testing.T) {
	attempt := mergeAttempt{owner: "acme", repo: "widget", base: "main", number: 7, head: "sha7", state: "blocked", method: "squash"}
	c := newAutoMergeSweepClient("http://unused.invalid")
	// A person's thread is open: the refusal parks and is remembered.
	c.transport = &threadResolverFake{nativeQueueFake: &nativeQueueFake{Transport: c.transport}, outcome: hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Others: 1}}
	if got := c.refusedMerge(context.Background(), attempt, unresolvedConversations405("Repository rule violations found\n\nAll comments must be resolved.")); got != autoMergeReasonUnresolvedReviewThreads {
		t.Fatalf("refusedMerge = %q, want %q", got, autoMergeReasonUnresolvedReviewThreads)
	}
	if got := c.knownMergeRefusal(attempt); got != autoMergeReasonUnresolvedReviewThreads {
		t.Fatalf("knownMergeRefusal = %q, want the park remembered", got)
	}
	next := attempt
	next.head = "sha8"
	if got := c.knownMergeRefusal(next); got != "" {
		t.Fatalf("a new head must re-arm, got %q", got)
	}
	if !isPermanentMergeRefusal(autoMergeReasonUnresolvedReviewThreads) {
		t.Fatal("unresolved-review-threads is a permanent refusal for the merge train and the hold log")
	}

	c2 := newAutoMergeSweepClient("http://unused.invalid")
	c2.transport = &threadResolverFake{nativeQueueFake: &nativeQueueFake{Transport: c2.transport}, outcome: hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Resolvable: 1, Resolved: 1}}
	if got := c2.refusedMerge(context.Background(), attempt, unresolvedConversations405("Repository rule violations found\n\nAll comments must be resolved.")); got != autoMergeReasonReviewThreadsResolved {
		t.Fatalf("own threads only: refusedMerge = %q, want %q", got, autoMergeReasonReviewThreadsResolved)
	}
	if got := c2.knownMergeRefusal(attempt); got != "" {
		t.Fatalf("a cleared refusal must not be remembered, got %q", got)
	}

	c3 := newAutoMergeSweepClient("http://unused.invalid")
	c3.transport = &nativeQueueFake{Transport: c3.transport}
	if got := c3.refusedMerge(context.Background(), attempt, unresolvedConversations405("Repository rule violations found\n\nChanges must be made through the merge queue\n\nAll comments must be resolved.")); got != autoMergeReasonMergeQueueRequired {
		t.Fatalf("merge-queue base: refusedMerge = %q, want %q", got, autoMergeReasonMergeQueueRequired)
	}
	if got := c3.refusedMerge(context.Background(), attempt, &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity}, Message: "All comments must be resolved."}); got != "" {
		t.Fatalf("only a 405 is the merge refusal, got %q", got)
	}
}

// The serialized lane merges through its own REST call; GitHub's
// unresolved-conversations 405 comes back on the lane result, and the lane
// path answers it with the same helper as the other merge paths. A parked
// head is not handed to the lane again.
func TestMergeThroughLaneUnresolvedConversations(t *testing.T) {
	refusal := unresolvedConversations405("Repository rule violations found\n\nAll comments must be resolved.")
	event := AutoMergeSweepEvent{Repo: "acme/widget", Number: 7, HeadSHA: "sha7"}
	for _, tc := range []struct {
		name    string
		outcome hgithub.OwnReviewThreadsOutcome
		want    string
	}{
		{"own threads only", hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Resolvable: 1, Resolved: 1}, autoMergeReasonReviewThreadsResolved},
		{"person's thread", hgithub.OwnReviewThreadsOutcome{Unresolved: 1, Others: 1}, autoMergeReasonUnresolvedReviewThreads},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newAutoMergeSweepClient("http://unused.invalid")
			c.transport = &threadResolverFake{nativeQueueFake: &nativeQueueFake{Transport: c.transport}, outcome: tc.outcome}
			calls := 0
			gate := func(context.Context, hgithub.LaneMergeRequest) (hgithub.LaneMergeResult, error) {
				calls++
				return hgithub.LaneMergeResult{Outcome: hgithub.LaneOutcomeLeft, Reason: "merge refused by GitHub: " + refusal.Error(), MergeError: refusal}, nil
			}
			_, reason, err := c.mergeThroughLane(context.Background(), gate, "acme", "widget", "main", hgithub.PRAuditPathSweep, "squash", event, nil)
			if err != nil || reason != tc.want {
				t.Fatalf("reason=%q err=%v, want %q", reason, err, tc.want)
			}
			_, again, _ := c.mergeThroughLane(context.Background(), gate, "acme", "widget", "main", hgithub.PRAuditPathSweep, "squash", event, nil)
			wantCalls := 2
			if tc.want == autoMergeReasonUnresolvedReviewThreads {
				wantCalls = 1
				if again != autoMergeReasonUnresolvedReviewThreads {
					t.Fatalf("parked head: reason=%q", again)
				}
			}
			if calls != wantCalls {
				t.Fatalf("lane calls=%d, want %d", calls, wantCalls)
			}
		})
	}
}

package automerge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
)

// The 405 bodies seen on projectbluefin, 2026-10-25 (fsdk-containers#463,
// utah#892).
const (
	reviewRequiredTestMessage = "At least 1 approving review is required by reviewers with write access."
	mergeCommitsTestMessage   = "Merge commits are not allowed on this repository."
)

func TestMergeRefusalReason(t *testing.T) {
	resp := func(status int, msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: status}, Message: msg}
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil},
		{name: "plain error", err: errors.New(reviewRequiredTestMessage)},
		{name: "one approving review", err: resp(http.StatusMethodNotAllowed, reviewRequiredTestMessage), want: autoMergeReasonMergeRefusedReview},
		{name: "two approving reviews", err: resp(http.StatusMethodNotAllowed, "At least 2 approving reviews are required by reviewers with write access."), want: autoMergeReasonMergeRefusedReview},
		{name: "ruleset approving review", err: resp(http.StatusMethodNotAllowed, "Repository rule violations found\n\nAt least 1 approving review is required by reviewers with write access."), want: autoMergeReasonMergeRefusedReview},
		{name: "ruleset review requesting changes", err: resp(http.StatusMethodNotAllowed, "Repository rule violations found\n\n1 review requesting changes by reviewers with write access.\n\n"), want: autoMergeReasonMergeRefusedReview},
		{name: "approving review wrapped not-applied", err: effects.NotApplied(resp(http.StatusMethodNotAllowed, reviewRequiredTestMessage)), want: autoMergeReasonMergeRefusedReview},
		{name: "merge commits not allowed", err: resp(http.StatusMethodNotAllowed, mergeCommitsTestMessage), want: autoMergeReasonMergeRefusedMethod},
		{name: "squash not allowed", err: resp(http.StatusMethodNotAllowed, "Squash merges are not allowed on this repository."), want: autoMergeReasonMergeRefusedMethod},
		{name: "rebase not allowed wrapped", err: fmt.Errorf("merge: %w", resp(http.StatusMethodNotAllowed, "Rebase merges are not allowed on this repository.")), want: autoMergeReasonMergeRefusedMethod},
		// Classified by their own matchers, or left as merge-failed.
		{name: "merge queue", err: resp(http.StatusMethodNotAllowed, mergeQueueRequiredTestMessage)},
		{name: "required checks expected", err: resp(http.StatusMethodNotAllowed, "2 of 3 required status checks are expected.")},
		{name: "other 405", err: resp(http.StatusMethodNotAllowed, "Pull Request is not mergeable")},
		{name: "approving review text on 409", err: resp(http.StatusConflict, reviewRequiredTestMessage)},
		{name: "method text on 422", err: resp(http.StatusUnprocessableEntity, mergeCommitsTestMessage)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeRefusalReason(tc.err); got != tc.want {
				t.Fatalf("mergeRefusalReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

func TestKnownMergeRefusal(t *testing.T) {
	refusal := func(msg string) error {
		return &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusMethodNotAllowed}, Message: msg}
	}
	base := mergeAttempt{owner: "Acme", repo: "widget", base: "main", number: 7, head: "sha7", state: "blocked", method: "merge"}
	for _, tc := range []struct {
		name    string
		refused error
		next    func(mergeAttempt) mergeAttempt
		advance time.Duration
		want    string
	}{
		{name: "review same head", refused: refusal(reviewRequiredTestMessage), want: autoMergeReasonMergeRefusedReview},
		{name: "review unknown state is no change", refused: refusal(reviewRequiredTestMessage), next: func(a mergeAttempt) mergeAttempt { a.state = "unknown"; return a }, want: autoMergeReasonMergeRefusedReview},
		{name: "review new head re-arms", refused: refusal(reviewRequiredTestMessage), next: func(a mergeAttempt) mergeAttempt { a.head = "sha8"; return a }},
		{name: "review unblocked re-arms", refused: refusal(reviewRequiredTestMessage), next: func(a mergeAttempt) mergeAttempt { a.state = "clean"; return a }},
		{name: "review recheck re-arms", refused: refusal(reviewRequiredTestMessage), advance: mergeRefusalRecheck},
		{name: "review is per PR", refused: refusal(reviewRequiredTestMessage), next: func(a mergeAttempt) mergeAttempt { a.number = 8; return a }},
		{name: "method same repo other PR", refused: refusal(mergeCommitsTestMessage), next: func(a mergeAttempt) mergeAttempt { a.number, a.head = 8, "sha8"; return a }, want: autoMergeReasonMergeRefusedMethod},
		{name: "method repo case-insensitive", refused: refusal(mergeCommitsTestMessage), next: func(a mergeAttempt) mergeAttempt { a.owner = "acme"; return a }, want: autoMergeReasonMergeRefusedMethod},
		{name: "method other method", refused: refusal(mergeCommitsTestMessage), next: func(a mergeAttempt) mergeAttempt { a.method = "squash"; return a }},
		{name: "method other repo", refused: refusal(mergeCommitsTestMessage), next: func(a mergeAttempt) mergeAttempt { a.repo = "gadget"; return a }},
		{name: "method recheck re-arms", refused: refusal(mergeCommitsTestMessage), advance: mergeRefusalRecheck},
		{name: "merge queue same base", refused: refusal(mergeQueueRequiredTestMessage), next: func(a mergeAttempt) mergeAttempt { a.number = 8; return a }, want: autoMergeReasonMergeQueueRequired},
		{name: "unclassified is not remembered", refused: refusal("Pull Request is not mergeable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Date(2026, 10, 25, 10, 34, 0, 0, time.UTC)
			c := &Engine{now: func() time.Time { return clock }}
			c.refusedMerge(base, tc.refused)
			clock = clock.Add(tc.advance)
			next := base
			if tc.next != nil {
				next = tc.next(base)
			}
			if got := c.knownMergeRefusal(next); got != tc.want {
				t.Fatalf("knownMergeRefusal = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSweepSelfAuthoredPermanentMergeRefusal: a review-required or
// method-not-allowed 405 is a skip logged once per head, and the next pass
// does not call merge again for the same head — unless GitHub's
// mergeable_state moved (an approving review landed).
func TestSweepSelfAuthoredPermanentMergeRefusal(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mergeMessage string
		secondState  string
		wantReason   string
		wantAttempts int
		wantHolds    int
	}{
		{name: "review required is one merge call", mergeMessage: reviewRequiredTestMessage, wantReason: autoMergeReasonMergeRefusedReview, wantAttempts: 1, wantHolds: 1},
		{name: "approval re-arms the merge", mergeMessage: reviewRequiredTestMessage, secondState: "clean", wantReason: autoMergeReasonMergeRefusedReview, wantAttempts: 2, wantHolds: 1},
		{name: "method not allowed is one merge call", mergeMessage: mergeCommitsTestMessage, wantReason: autoMergeReasonMergeRefusedMethod, wantAttempts: 1, wantHolds: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var merged []int
			var attempts int
			prs := []selfAuthoredPR{{
				number: 90, author: testHiveAppBotLogin, mergeableState: "blocked",
				statusState: "success", checkStatus: "completed", checkConclusion: "success",
				mergeStatus: http.StatusMethodNotAllowed, mergeMessage: tc.mergeMessage, mergeAttempts: &attempts,
			}}
			api := newSelfAuthoredAutoMergeAPI(t, prs, &merged)
			defer api.Close()

			var logs bytes.Buffer
			c := newAutoMergeSweepClient(api.URL)
			c.logger = slog.New(slog.NewTextHandler(&logs, nil))
			clock := time.Date(2026, 10, 25, 10, 34, 0, 0, time.UTC)
			c.now = func() time.Time { return clock }

			for pass := 0; pass < 2; pass++ {
				if pass == 1 {
					clock = clock.Add(3 * time.Minute)
					if tc.secondState != "" {
						prs[0].mergeableState = tc.secondState
					}
				}
				result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
				if err != nil {
					t.Fatalf("pass %d: SweepSelfAuthoredAutoMerges returned error: %v", pass, err)
				}
				if len(result.Merged) != 0 || result.Skipped != 1 {
					t.Fatalf("pass %d: result=%+v, want one skip and no merge", pass, result)
				}
			}
			gotLogs := logs.String()
			if attempts != tc.wantAttempts {
				t.Fatalf("merge attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if !strings.Contains(gotLogs, "reason="+tc.wantReason) {
				t.Fatalf("logs = %q, want reason=%s", gotLogs, tc.wantReason)
			}
			if got := strings.Count(gotLogs, "msg=\"automerge hold\""); got != tc.wantHolds {
				t.Fatalf("automerge hold logs = %d, want %d; logs = %q", got, tc.wantHolds, gotLogs)
			}
			if strings.Contains(gotLogs, "level=WARN msg=\"self-authored automerge sweep skipped PR\"") {
				t.Fatalf("a classified refusal must not WARN as merge-failed; logs = %q", gotLogs)
			}
		})
	}
}

// The trusted-author and contributor lanes remember the refusal too: two
// passes over the same head make one merge call. (The trusted fixture's PR
// carries no base ref, so the per-base merge-queue memory is exercised on
// the contributor lane.)
func TestTrustedAuthorPermanentMergeRefusalOneMergeCall(t *testing.T) {
	for _, msg := range []string{reviewRequiredTestMessage, mergeCommitsTestMessage} {
		t.Run(msg, func(t *testing.T) {
			fx := trustedSweepFixture{mergeStatus: http.StatusMethodNotAllowed, mergeMessage: msg}
			api := newTrustedSweepAPI(t, &fx)
			defer api.Close()
			policy := TrustedAuthorPolicy{Enabled: true, RequireRole: "merger"}
			engine := newTrustedSweepEngine(api.URL, policy, Options{})
			for pass := 0; pass < 2; pass++ {
				_, reason, err := engine.trySweepTrustedAuthorPR(context.Background(), "widget", "acme", "widget", 7, policy)
				if err != nil || !isPermanentMergeRefusal(reason) {
					t.Fatalf("pass %d: reason = %q, err = %v; want a classified refusal skip", pass, reason, err)
				}
			}
			if fx.merges != 1 {
				t.Fatalf("merges = %d, want 1 across two passes", fx.merges)
			}
		})
	}
}

func TestContributorLanePermanentMergeRefusalOneMergeCall(t *testing.T) {
	for _, msg := range []string{reviewRequiredTestMessage, mergeCommitsTestMessage, mergeQueueRequiredTestMessage} {
		t.Run(msg, func(t *testing.T) {
			fx := validContributorFixture()
			fx.mergeStatus = http.StatusMethodNotAllowed
			fx.mergeMessage = msg
			fx.baseRef = "main"
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 25, 10, 34, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
			for pass := 0; pass < 2; pass++ {
				_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
				if err != nil || !isPermanentMergeRefusal(reason) {
					t.Fatalf("pass %d: reason = %q, err = %v; want a classified refusal skip", pass, reason, err)
				}
			}
			if fx.merges != 1 {
				t.Fatalf("merges = %d, want 1 across two passes", fx.merges)
			}
		})
	}
}

// TestRequiredChecksExpectedRemembered: GitHub's 405 "N of M required status
// checks are expected" (hivecommons/docs#512, 2026-10-29) is remembered per
// head, and the next pass skips the merge call as check-pending while the
// PR is still blocked — a new head, an unblocked state or the recheck
// window re-arms it; other errors are not remembered.
func TestRequiredChecksExpectedRemembered(t *testing.T) {
	expected := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusMethodNotAllowed}, Message: "4 of 4 required status checks are expected."}
	base := mergeAttempt{owner: "hivecommons", repo: "docs", base: "main", number: 512, head: "sha512", state: "blocked", method: "squash"}
	for _, tc := range []struct {
		name    string
		err     error
		next    func(mergeAttempt) mergeAttempt
		advance time.Duration
		want    string
	}{
		{name: "same head still blocked", err: expected, want: autoMergeReasonChecksExpected},
		{name: "new head re-arms", err: expected, next: func(a mergeAttempt) mergeAttempt { a.head = "sha513"; return a }},
		{name: "checks reported re-arms", err: expected, next: func(a mergeAttempt) mergeAttempt { a.state = "clean"; return a }},
		{name: "state not yet computed re-arms", err: expected, next: func(a mergeAttempt) mergeAttempt { a.state = "unknown"; return a }},
		{name: "recheck re-arms", err: expected, advance: mergeRefusalRecheck},
		{name: "per PR", err: expected, next: func(a mergeAttempt) mergeAttempt { a.number = 513; return a }},
		{name: "other 405 is not remembered", err: &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusMethodNotAllowed}, Message: "Pull Request is not mergeable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := time.Date(2026, 10, 29, 15, 4, 0, 0, time.UTC)
			c := &Engine{now: func() time.Time { return clock }}
			if got := c.requiredChecksExpected(base, tc.err); got != requiredChecksExpectedMergeError(tc.err) {
				t.Fatalf("requiredChecksExpected = %v", got)
			}
			clock = clock.Add(tc.advance)
			next := base
			if tc.next != nil {
				next = tc.next(base)
			}
			if got := c.knownMergeRefusal(next); got != tc.want {
				t.Fatalf("knownMergeRefusal = %q, want %q", got, tc.want)
			}
		})
	}
}

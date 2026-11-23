package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/reviewfeedback"
)

var retryTestPolicy = reviewfeedback.RetryPolicy{MaxRetries: 2, Backoff: time.Hour}

// cncf/techdocs#424 (#19320): feedback routed to an agent whose run ended
// with no push and no reply is routed again after the backoff, twice, and
// then handed to a person with the reason.
func TestReviewFeedbackSweep_StrandedFeedbackRetriedThenNeedsHuman(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	s.Limits.WindowCap = 10
	s.Retry = retryTestPolicy
	run := func(at time.Time) reviewFeedbackResult {
		s.Now = at
		return s.run(context.Background(), parseReviewFeedbackKey)
	}
	if res := run(now); res.Kicked != 1 {
		t.Fatalf("first pass = %+v, want one kick", res)
	}
	// Before the backoff: nothing.
	if res := run(now.Add(30 * time.Minute)); res.Kicked != 0 || res.Retried != 0 {
		t.Fatalf("re-routed before the backoff: %+v", res)
	}
	// Backoff passed, head unchanged, no reply: retry 1.
	t1 := now.Add(61 * time.Minute)
	if res := run(t1); res.Retried != 1 || res.Kicked != 1 {
		t.Fatalf("retry 1 = %+v", res)
	}
	if got := len(kicker.kicks["quality"]); got != 2 {
		t.Fatalf("kicks = %d, want 2", got)
	}
	for _, id := range []string{"inline:11", "inline:12"} {
		if !strings.Contains(kicker.kicks["quality"][1], id) {
			t.Fatalf("retry brief missing %s", id)
		}
	}
	// The second retry waits twice as long.
	if res := run(t1.Add(90 * time.Minute)); res.Retried != 0 {
		t.Fatalf("retry 2 before the doubled backoff: %+v", res)
	}
	t2 := t1.Add(121 * time.Minute)
	if res := run(t2); res.Retried != 1 || res.Kicked != 1 {
		t.Fatalf("retry 2 = %+v", res)
	}
	// Cap spent: a person is asked, once, with the reason.
	t3 := t2.Add(5 * time.Hour)
	res := run(t3)
	if res.Stranded != 1 || res.Kicked != 0 || len(src.comments) != 1 {
		t.Fatalf("exhausted pass = %+v comments = %d", res, len(src.comments))
	}
	if !strings.Contains(src.comments[0], "no push and no reply") || !strings.Contains(src.comments[0], "3 time(s)") {
		t.Fatalf("needs-human note lacks the reason: %s", src.comments[0])
	}
	if res := run(t3.Add(24 * time.Hour)); res.Kicked != 0 || len(src.comments) != 1 {
		t.Fatalf("after the hand-off: %+v comments = %d", res, len(src.comments))
	}
}

// An agent that pushed after the kick acted on it: no retry.
func TestReviewFeedbackSweep_StrandedNotRetriedAfterPush(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	s.Retry = retryTestPolicy
	_ = s.run(context.Background(), parseReviewFeedbackKey)
	fb.HeadSHA = "bbb"
	src.byKey[feedbackTestKey] = fb
	s.Now = now.Add(3 * time.Hour)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Retried != 0 || res.Kicked != 0 || res.Stranded != 0 {
		t.Fatalf("pushed kick re-routed: %+v", res)
	}
}

// An agent that replied on the PR after the kick acted on it: no retry.
func TestReviewFeedbackSweep_StrandedNotRetriedAfterReply(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	s.Retry = retryTestPolicy
	_ = s.run(context.Background(), parseReviewFeedbackKey)
	fb.LastHiveReplyAt = now.Add(10 * time.Minute)
	src.byKey[feedbackTestKey] = fb
	s.Now = now.Add(3 * time.Hour)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Retried != 0 || res.Kicked != 0 || res.Stranded != 0 {
		t.Fatalf("answered kick re-routed: %+v", res)
	}
}

// max_retries 0: stranded feedback goes straight to a person after the
// backoff, never re-kicked.
func TestReviewFeedbackSweep_StrandedZeroRetriesHandsToPerson(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	s.Retry = reviewfeedback.RetryPolicy{MaxRetries: 0, Backoff: time.Hour}
	_ = s.run(context.Background(), parseReviewFeedbackKey)
	s.Now = now.Add(2 * time.Hour)
	res := s.run(context.Background(), parseReviewFeedbackKey)
	if res.Stranded != 1 || len(kicker.kicks["quality"]) != 1 || len(src.comments) != 1 {
		t.Fatalf("res = %+v kicks = %d comments = %d", res, len(kicker.kicks["quality"]), len(src.comments))
	}
}

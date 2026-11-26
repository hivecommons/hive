package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	fb.Items[1].URL = "https://example.invalid/review/12"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	kicker := &recordingKicker{}
	storeDir := t.TempDir()
	store, err := reviewfeedback.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	s.Store = store
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
	// Cap spent: a failed hand-off stays pending without another agent kick.
	t3 := t2.Add(5 * time.Hour)
	flaky := &flakyCommentSource{fakeFeedbackSource: src, fail: true}
	s.Source = flaky
	if res := run(t3); res.Stranded != 1 || res.Kicked != 0 || res.NeedsHuman != 0 || len(flaky.attempts) != 1 || len(src.comments) != 0 {
		t.Fatalf("failed hand-off = %+v comments = %d", res, len(src.comments))
	}
	s.Store, err = reviewfeedback.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	s.Retry = reviewfeedback.RetryPolicy{MaxRetries: 3, Backoff: 2 * time.Hour}
	headless := fb
	headless.HeadSHA = ""
	src.byKey[feedbackTestKey] = headless
	if res := run(t3.Add(5 * time.Minute)); res.Kicked != 0 || res.NeedsHuman != 0 || len(flaky.attempts) != 1 {
		t.Fatalf("headless pass = %+v attempts = %d", res, len(flaky.attempts))
	}
	withFresh := fb
	withFresh.Items = append(withFresh.Items, github.FeedbackItem{ID: "inline:13", CreatedAt: t3})
	src.byKey[feedbackTestKey] = withFresh
	if res := run(t3.Add(10 * time.Minute)); res.Kicked != 1 || res.NeedsHuman != 0 || len(flaky.attempts) != 2 {
		t.Fatalf("fresh feedback while hand-off fails = %+v attempts = %d", res, len(flaky.attempts))
	}
	brief := kicker.kicks["quality"][3]
	if !strings.Contains(brief, "inline:13") || strings.Contains(brief, "inline:11") || strings.Contains(brief, "inline:12") {
		t.Fatalf("expected only the fresh thread in the new kick: %s", brief)
	}
	s.Store, err = reviewfeedback.Open(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	flaky.fail = false
	// The fresh kick pushed a commit and one original thread was resolved.
	remaining := fb
	remaining.HeadSHA = "bbb"
	remaining.Items = fb.Items[:1]
	remaining.RuleCandidates = withFresh.Items
	src.byKey[feedbackTestKey] = remaining
	res := run(t3.Add(15 * time.Minute))
	if res.Stranded != 1 || res.Kicked != 0 || res.NeedsHuman != 1 || len(flaky.attempts) != 3 || len(src.comments) != 1 {
		t.Fatalf("exhausted pass = %+v comments = %d", res, len(src.comments))
	}
	if flaky.attempts[0] != flaky.attempts[1] || flaky.attempts[0] != flaky.attempts[2] {
		t.Fatal("hand-off body changed across attempts")
	}
	if !strings.Contains(src.comments[0], "no push and no reply") || !strings.Contains(src.comments[0], "3 time(s)") ||
		!strings.Contains(src.comments[0], fb.Items[1].URL) {
		t.Fatalf("needs-human note lacks the reason: %s", src.comments[0])
	}
	if res := run(t3.Add(24 * time.Hour)); res.Stranded != 0 || res.Kicked != 0 || len(src.comments) != 1 {
		t.Fatalf("after the hand-off: %+v comments = %d", res, len(src.comments))
	}
}

func TestReviewFeedbackSweep_HandoffSurvivesPartialFeedback(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	flaky := &flakyCommentSource{fakeFeedbackSource: src, fail: true}
	s := feedbackSweepFor(t, src, &recordingKicker{}, routeTo("quality"), now)
	s.Source = flaky
	s.Retry = reviewfeedback.RetryPolicy{Backoff: time.Hour}
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 1 {
		t.Fatalf("initial kick = %+v", res)
	}
	s.Now = now.Add(2 * time.Hour)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.NeedsHuman != 0 || len(flaky.attempts) != 1 {
		t.Fatalf("failed hand-off = %+v attempts = %d", res, len(flaky.attempts))
	}
	// The bounded read omitted the old threads. The response gives no evidence
	// that they were answered, so keep the original body eligible for posting.
	fb.Items = nil
	src.byKey[feedbackTestKey] = fb
	flaky.fail = false
	s.Now = now.Add(3 * time.Hour)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.NeedsHuman != 1 || res.Kicked != 0 || len(flaky.attempts) != 2 ||
		flaky.attempts[0] != flaky.attempts[1] {
		t.Fatalf("hand-off after a partial read = %+v attempts = %v", res, flaky.attempts)
	}
}

func TestReviewFeedbackSweep_ResolvedHandoffChecksForPriorPost(t *testing.T) {
	t.Run("refused note releases the next handoff", func(t *testing.T) {
		checkResolvedHandoff(t, false)
	})
	t.Run("accepted note covers the PR", func(t *testing.T) {
		checkResolvedHandoff(t, true)
	})
}

func checkResolvedHandoff(t *testing.T, alreadyPosted bool) {
	t.Helper()
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	fb.Items[0].URL, fb.Items[1].URL = "https://example.invalid/review/11", "https://example.invalid/review/12"
	first := fb
	first.Items = fb.Items[:1]
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: first}}
	flaky := &flakyCommentSource{fakeFeedbackSource: src, fail: true}
	s := feedbackSweepFor(t, src, &recordingKicker{}, routeTo("quality"), now)
	s.Source = flaky
	s.Retry = reviewfeedback.RetryPolicy{Backoff: time.Hour}
	run := func(at time.Time) reviewFeedbackResult {
		s.Now = at
		return s.run(context.Background(), parseReviewFeedbackKey)
	}
	if res := run(now); res.Kicked != 1 {
		t.Fatalf("initial kick = %+v", res)
	}
	if res := run(now.Add(2 * time.Hour)); res.Kicked != 0 || len(flaky.attempts) != 1 {
		t.Fatalf("first hand-off = %+v attempts = %d", res, len(flaky.attempts))
	}
	src.byKey[feedbackTestKey] = fb
	if res := run(now.Add(3 * time.Hour)); res.Kicked != 1 || len(flaky.attempts) != 2 {
		t.Fatalf("fresh kick = %+v attempts = %d", res, len(flaky.attempts))
	}
	if res := run(now.Add(5 * time.Hour)); res.Kicked != 0 || len(flaky.attempts) != 3 || flaky.attempts[0] != flaky.attempts[2] {
		t.Fatalf("second hand-off waiting = %+v attempts = %v", res, flaky.attempts)
	}
	if alreadyPosted {
		// GitHub applied the first note but Hive has only seen failed responses.
		src.comments = append(src.comments, flaky.attempts[0])
	}
	flaky.fail = false
	second := fb
	second.Items = fb.Items[1:]
	second.RuleCandidates = fb.Items
	src.byKey[feedbackTestKey] = second
	res := run(now.Add(6 * time.Hour))
	if alreadyPosted {
		if res.Kicked != 0 || len(flaky.attempts) != 3 || len(src.comments) != 1 || !s.Store.NeedsHumanNoted(feedbackTestKey) {
			t.Fatalf("accepted hand-off duplicated = %+v comments = %v", res, src.comments)
		}
	} else if res.Kicked != 0 || res.NeedsHuman != 1 || len(src.comments) != 1 ||
		!strings.Contains(src.comments[0], fb.Items[1].URL) || strings.Contains(src.comments[0], fb.Items[0].URL) {
		t.Fatalf("still-needed hand-off = %+v comments = %v", res, src.comments)
	}
}

func TestReviewFeedbackSweep_LoweredRetryCapClosesRefusedRetry(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	s.Retry = retryTestPolicy
	run := func(at time.Time) reviewFeedbackResult {
		s.Now = at
		return s.run(context.Background(), parseReviewFeedbackKey)
	}
	if res := run(now); res.Kicked != 1 {
		t.Fatalf("initial kick = %+v", res)
	}
	kicker.err = errors.New("agent busy")
	if res := run(now.Add(2 * time.Hour)); res.Retried != 1 || res.Kicked != 0 {
		t.Fatalf("refused retry = %+v", res)
	}
	kicker.err = nil
	s.Retry.MaxRetries = 0
	headless := fb
	headless.HeadSHA = ""
	headless.Items = append(headless.Items, github.FeedbackItem{ID: "inline:13", CreatedAt: now.Add(150 * time.Minute)})
	src.byKey[feedbackTestKey] = headless
	if res := run(now.Add(3 * time.Hour)); res.Kicked != 1 || len(src.comments) != 0 {
		t.Fatalf("reduced-cap hand-off without a head = %+v comments = %d", res, len(src.comments))
	}
	brief := kicker.kicks["quality"][1]
	if !strings.Contains(brief, "inline:13") || strings.Contains(brief, "inline:11") || strings.Contains(brief, "inline:12") {
		t.Fatalf("expected only fresh feedback after lowering the retry cap: %s", brief)
	}
	src.byKey[feedbackTestKey] = fb
	s.Retry.Backoff = 5 * time.Hour
	if res := run(now.Add(4 * time.Hour)); res.Kicked != 0 || len(src.comments) != 0 {
		t.Fatalf("reduced-cap hand-off before new backoff = %+v comments = %d", res, len(src.comments))
	}
	if res := run(now.Add(6 * time.Hour)); res.NeedsHuman != 1 || res.Kicked != 0 {
		t.Fatalf("reduced-cap hand-off = %+v", res)
	}
	if res := run(now.Add(7 * time.Hour)); res.Kicked != 0 || res.Stranded != 0 || len(src.comments) != 1 {
		t.Fatalf("after reduced-cap hand-off = %+v comments = %d", res, len(src.comments))
	}
}

func TestReviewFeedbackSweep_HandoffWaitsForDurableNote(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.HeadSHA = "aaa"
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	s := feedbackSweepFor(t, src, &recordingKicker{}, routeTo("quality"), now)
	dir := t.TempDir()
	var err error
	s.Store, err = reviewfeedback.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Retry = reviewfeedback.RetryPolicy{Backoff: time.Hour}
	run := func(at time.Time) reviewFeedbackResult {
		s.Now = at
		return s.run(context.Background(), parseReviewFeedbackKey)
	}
	if res := run(now); res.Kicked != 1 {
		t.Fatalf("initial kick = %+v", res)
	}
	blocker := filepath.Join(dir, "state.json.tmp")
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatal(err)
	}
	if res := run(now.Add(2 * time.Hour)); res.Stranded != 1 || res.Kicked != 0 || len(src.comments) != 0 {
		t.Fatalf("failed persistence = %+v comments = %d", res, len(src.comments))
	}
	s.Retry.MaxRetries = 2
	fb.Items = append(fb.Items, github.FeedbackItem{ID: "inline:13", CreatedAt: now.Add(time.Hour)})
	src.byKey[feedbackTestKey] = fb
	if res := run(now.Add(3 * time.Hour)); res.Kicked != 0 || len(src.comments) != 0 {
		t.Fatalf("blocked with fresh feedback = %+v comments = %d", res, len(src.comments))
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if res := run(now.Add(4 * time.Hour)); res.NeedsHuman != 1 || res.Kicked != 1 || len(src.comments) != 1 {
		t.Fatalf("recovered hand-off = %+v comments = %d", res, len(src.comments))
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

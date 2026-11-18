package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/reviewfeedback"
)

type fakeFeedbackSource struct {
	byKey    map[string]github.PRFeedback
	err      error
	comments []string
}

func (f *fakeFeedbackSource) FetchPRFeedback(_ context.Context, repo string, number int) (github.PRFeedback, error) {
	if f.err != nil {
		return github.PRFeedback{}, f.err
	}
	return f.byKey[reviewfeedback.Key(repo, number)], nil
}

func (f *fakeFeedbackSource) CreateIssueComment(_ context.Context, _ string, _ int, body string) error {
	f.comments = append(f.comments, body)
	return nil
}

type recordingKicker struct {
	kicks map[string][]string
	err   error
}

func (k *recordingKicker) Kick(agent, message string) error {
	if k.err != nil {
		return k.err
	}
	if k.kicks == nil {
		k.kicks = map[string][]string{}
	}
	k.kicks[agent] = append(k.kicks[agent], message)
	return nil
}

const feedbackTestKey = "cncf/techdocs#424"

func feedbackSweepFor(t *testing.T, src *fakeFeedbackSource, kicker fixKicker, route func(string) reviewFeedbackRoute, now time.Time) reviewFeedbackSweep {
	t.Helper()
	store, _ := reviewfeedback.Open(t.TempDir())
	return reviewFeedbackSweep{
		Source: src, Store: store, Kicker: kicker, Route: route, Now: now, Logger: discardLogger(),
		Owners: map[string]string{feedbackTestKey: "quality"},
	}
}

func openDraftFeedback() github.PRFeedback {
	return github.PRFeedback{State: "open", Draft: true, HeadRef: "quality/x", Source: "graphql", Items: []github.FeedbackItem{
		{ID: "inline:11", ThreadID: "T1", Kind: github.PRCommentInline, Author: "nate", Body: "score is too high", Path: "a.md", CreatedAt: time.Now()},
		{ID: "inline:12", ThreadID: "T2", Kind: github.PRCommentInline, Author: "nate", Body: "misleading phrase", Path: "a.md", CreatedAt: time.Now()},
	}}
}

func routeTo(agent string) func(string) reviewFeedbackRoute {
	return func(string) reviewFeedbackRoute { return reviewFeedbackRoute{Agent: agent} }
}

// A draft PR in a remote repo (not one the hive manages) with unanswered
// inline feedback kicks the authoring agent once, and the same comments never
// kick it again, even after the rate-limit interval.
func TestReviewFeedbackSweep_DraftRemotePRKicksOnceAndNeverAgain(t *testing.T) {
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: openDraftFeedback()}}
	kicker := &recordingKicker{}
	now := time.Now()
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)

	res := s.run(context.Background(), parseReviewFeedbackKey)
	if res.Kicked != 1 || len(kicker.kicks["quality"]) != 1 {
		t.Fatalf("res = %+v kicks = %v, want one kick to quality", res, kicker.kicks)
	}
	brief := kicker.kicks["quality"][0]
	for _, want := range []string{"cncf/techdocs#424", "inline:11", "inline:12", "thread T1", "whether or not the PR is a draft"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}
	s.Now = now.Add(reviewfeedback.MinInterval + time.Hour)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 0 || len(kicker.kicks["quality"]) != 1 {
		t.Fatalf("handled comments re-kicked: %+v", res)
	}
}

func TestReviewFeedbackSweep_RateLimitsNewFeedbackOnSamePR(t *testing.T) {
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: openDraftFeedback()}}
	kicker := &recordingKicker{}
	now := time.Now()
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	s.run(context.Background(), parseReviewFeedbackKey)

	fb := openDraftFeedback()
	fb.Items = append(fb.Items, github.FeedbackItem{ID: "inline:13", Kind: github.PRCommentInline, Author: "nate", Body: "one more"})
	src.byKey[feedbackTestKey] = fb
	s.Now = now.Add(time.Minute)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.RateLimited != 1 || res.Kicked != 0 {
		t.Fatalf("res = %+v, want rate limited", res)
	}
	s.Now = now.Add(reviewfeedback.MinInterval + time.Minute)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 1 {
		t.Fatalf("res = %+v, want the new comment kicked after the interval", res)
	}
}

func TestReviewFeedbackSweep_PausedAgentStaysPaused_ClosedPRSkipped(t *testing.T) {
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: openDraftFeedback()}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, func(string) reviewFeedbackRoute { return reviewFeedbackRoute{Paused: true} }, time.Now())
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Checked != 0 || len(kicker.kicks) != 0 {
		t.Fatalf("paused agent was reached: %+v", res)
	}
	closed := openDraftFeedback()
	closed.State = "merged"
	src.byKey[feedbackTestKey] = closed
	s.Route = routeTo("quality")
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 0 {
		t.Fatalf("merged PR kicked: %+v", res)
	}
}

func TestReviewFeedbackSweep_UndeliverableKickIsRetried_FetchErrorSkips(t *testing.T) {
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: openDraftFeedback()}}
	kicker := &recordingKicker{err: errors.New("agent busy")}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), time.Now())
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 0 {
		t.Fatalf("res = %+v", res)
	}
	kicker.err = nil
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 1 {
		t.Fatalf("refused kick must be retried, res = %+v", res)
	}
	src.err = errors.New("rate limited")
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Errors != 1 {
		t.Fatalf("res = %+v, want a counted fetch error", res)
	}
}

// No agent can take it: a person is told once, with a needs-human note.
func TestReviewFeedbackSweep_NoAgentPostsNeedsHumanOnce(t *testing.T) {
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: openDraftFeedback()}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, func(string) reviewFeedbackRoute {
		return reviewFeedbackRoute{Reason: "no fallback agent is configured"}
	}, time.Now())
	s.run(context.Background(), parseReviewFeedbackKey)
	s.run(context.Background(), parseReviewFeedbackKey)
	if len(src.comments) != 1 || !strings.Contains(src.comments[0], github.FeedbackNeedsHumanMarker) || strings.Contains(src.comments[0], "@") {
		t.Fatalf("comments = %q, want exactly one marked note without mentions", src.comments)
	}
	if len(kicker.kicks) != 0 {
		t.Fatal("nothing should have been kicked")
	}
}

func TestParseReviewFeedbackKey(t *testing.T) {
	if r, n, ok := parseReviewFeedbackKey("cncf/techdocs#424"); !ok || r != "cncf/techdocs" || n != 424 {
		t.Fatalf("got %q %d %v", r, n, ok)
	}
	for _, bad := range []string{"techdocs#424", "a/b#", "a/b#x", "#3", "a/b"} {
		if _, _, ok := parseReviewFeedbackKey(bad); ok {
			t.Errorf("%q must not parse", bad)
		}
	}
}

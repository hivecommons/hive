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

var wireTestLimits = reviewfeedback.Limits{Debounce: 3 * time.Minute, Window: 24 * time.Hour, WindowCap: 2, MaxItems: 30}

const feedbackTestKey = "cncf/techdocs#424"

func feedbackSweepFor(t *testing.T, src *fakeFeedbackSource, kicker fixKicker, route func(string) reviewFeedbackRoute, now time.Time) reviewFeedbackSweep {
	t.Helper()
	store, _ := reviewfeedback.Open(t.TempDir())
	return reviewFeedbackSweep{
		Source: src, Store: store, Kicker: kicker, Route: route, Now: now, Logger: discardLogger(), Limits: wireTestLimits,
		Owners: map[string]string{feedbackTestKey: "quality"},
	}
}

func openDraftFeedback() github.PRFeedback {
	return github.PRFeedback{State: "open", Draft: true, HeadRef: "quality/x", Source: "graphql", Items: []github.FeedbackItem{
		{ID: "inline:11", ThreadID: "T1", Kind: github.PRCommentInline, Author: "nate", Body: "score is too high", Path: "a.md", CreatedAt: time.Now().Add(-time.Hour)},
		{ID: "inline:12", ThreadID: "T2", Kind: github.PRCommentInline, Author: "nate", Body: "misleading phrase", Path: "a.md", CreatedAt: time.Now().Add(-time.Hour)},
	}}
}

func routeTo(agent string) func(string) reviewFeedbackRoute {
	return func(string) reviewFeedbackRoute { return reviewFeedbackRoute{Agent: agent} }
}

// A draft PR in a remote repo (not one the hive manages) with unanswered
// inline feedback kicks the authoring agent once, and the same comments never
// kick it again, even much later.
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
	s.Now = now.Add(24 * time.Hour)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 0 || len(kicker.kicks["quality"]) != 1 {
		t.Fatalf("handled comments re-kicked: %+v", res)
	}
}

// Nine existing comments go out as one coalesced kick, and a comment posted
// afterwards (while the agent may still be working) is held for the debounce
// and then triggers a second kick carrying only it.
func TestReviewFeedbackSweep_NineCommentsOneKick_ThenFollowUpKick(t *testing.T) {
	now := time.Now()
	fb := openDraftFeedback()
	fb.Items = nil
	for i := 0; i < 9; i++ {
		fb.Items = append(fb.Items, github.FeedbackItem{ID: "inline:" + string(rune('a'+i)), Kind: github.PRCommentInline, Author: "nate", Body: "c", CreatedAt: now.Add(-time.Hour)})
	}
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{feedbackTestKey: fb}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 1 || len(kicker.kicks["quality"]) != 1 {
		t.Fatalf("res = %+v, want one kick", res)
	}
	for _, id := range []string{"inline:a", "inline:i"} {
		if !strings.Contains(kicker.kicks["quality"][0], id) {
			t.Fatalf("brief missing %s", id)
		}
	}

	later := fb
	later.Items = append(append([]github.FeedbackItem{}, fb.Items...), github.FeedbackItem{ID: "inline:new", Kind: github.PRCommentInline, Author: "nate", Body: "one more", CreatedAt: now.Add(time.Minute)})
	src.byKey[feedbackTestKey] = later
	s.Now = now.Add(time.Minute + 30*time.Second)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Debounced != 1 || res.Kicked != 0 {
		t.Fatalf("res = %+v, want the new comment held for the debounce", res)
	}
	s.Now = now.Add(5 * time.Minute)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 1 || len(kicker.kicks["quality"]) != 2 {
		t.Fatalf("res = %+v, want a second kick within minutes", res)
	}
	second := kicker.kicks["quality"][1]
	if !strings.Contains(second, "inline:new") || strings.Contains(second, "inline:a") {
		t.Fatalf("follow-up brief must carry only the new comment")
	}
}

// Hitting the rolling cap is a visible needs-human note, not silence, and a
// person's next comment resumes kicks.
func TestReviewFeedbackSweep_CapIsVisibleAndResetsOnNewFeedback(t *testing.T) {
	now := time.Now()
	mk := func(id string, at time.Time) github.PRFeedback {
		fb := openDraftFeedback()
		fb.Items = []github.FeedbackItem{{ID: id, Kind: github.PRCommentInline, Author: "nate", Body: "x", CreatedAt: at}}
		return fb
	}
	src := &fakeFeedbackSource{byKey: map[string]github.PRFeedback{}}
	kicker := &recordingKicker{}
	s := feedbackSweepFor(t, src, kicker, routeTo("quality"), now)
	for i := 0; i < wireTestLimits.WindowCap; i++ {
		src.byKey[feedbackTestKey] = mk("inline:"+string(rune('a'+i)), now.Add(-time.Hour))
		s.Now = now.Add(time.Duration(i) * time.Minute)
		s.run(context.Background(), parseReviewFeedbackKey)
	}
	src.byKey[feedbackTestKey] = mk("inline:capped", now.Add(-time.Hour))
	s.Now = now.Add(10 * time.Minute)
	res := s.run(context.Background(), parseReviewFeedbackKey)
	if res.CapReached != 1 || res.NeedsHuman != 1 || len(src.comments) != 1 || !strings.Contains(src.comments[0], "resets") {
		t.Fatalf("res = %+v comments = %q, want a visible cap note", res, src.comments)
	}
	src.byKey[feedbackTestKey] = mk("inline:fresh", now.Add(12*time.Minute))
	s.Now = now.Add(20 * time.Minute)
	if res := s.run(context.Background(), parseReviewFeedbackKey); res.Kicked != 1 {
		t.Fatalf("res = %+v, want kicks to resume after new human feedback", res)
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

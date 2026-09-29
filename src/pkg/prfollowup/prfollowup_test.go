package prfollowup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/turn"
)

const (
	testRepo  = "hivecommons/hive"
	testPR    = 42
	testAgent = "scanner"
	testSHA   = "0123456789abcdef"
	testURL   = "https://github.com/hivecommons/hive/pull/42"
)

// fakeResumer models the agent manager: one live session per agent, which
// moves on every delivered kick exactly like the real LastKick-based ID.
type fakeResumer struct {
	sessions map[string]string
	sendErr  error
	sent     []string
}

func newFakeResumer(session string) *fakeResumer {
	return &fakeResumer{sessions: map[string]string{testAgent: session}}
}

func (f *fakeResumer) SessionID(agent string) (string, bool) {
	s, ok := f.sessions[agent]
	return s, ok
}

func (f *fakeResumer) SendResumeKick(agent, message, sessionID string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	if f.sessions[agent] != sessionID {
		return errors.New("session mismatch")
	}
	f.sent = append(f.sent, message)
	f.sessions[agent] = sessionID + "+kick"
	return nil
}

func redPR() github.PullRequest {
	return github.PullRequest{
		Repo:             testRepo,
		Number:           testPR,
		URL:              testURL,
		CIStatus:         "failure",
		HeadSHA:          testSHA,
		FailingChecks:    []string{"build", "coverage"},
		CIFailureExcerpt: "pkg/x: undefined: Foo",
	}
}

func record(t *testing.T, dir, session string, now time.Time) {
	t.Helper()
	if err := Record(context.Background(), dir, testAgent, testRepo, testPR, testURL, session, now); err != nil {
		t.Fatalf("Record: %v", err)
	}
}

func load(t *testing.T, dir string) turn.SessionEnvelope {
	t.Helper()
	env, err := turn.FileStore{Dir: dir}.Load(context.Background(), PointerID(testRepo, testPR))
	if err != nil {
		t.Fatalf("load pointer: %v", err)
	}
	return env
}

func opts(dir string) Options {
	return Options{Dir: dir, MaxAge: time.Hour}
}

func TestRoute_ResumesAuthoringSessionOnCIFailure(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")

	out := Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now.Add(time.Minute))
	if len(out) != 1 || out[0].Route != RouteResumed || out[0].Agent != testAgent {
		t.Fatalf("outcomes = %+v, want one resumed for %s", out, testAgent)
	}
	if len(r.sent) != 1 {
		t.Fatalf("kicks = %d, want 1", len(r.sent))
	}
	msg := r.sent[0]
	for _, want := range []string{testRepo + "#42", testURL, "CI failed on 0123456", "build, coverage", "undefined: Foo", "Do not open a new PR"} {
		if !strings.Contains(msg, want) {
			t.Errorf("follow-up message missing %q:\n%s", want, msg)
		}
	}

	env := load(t, dir)
	if env.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1 (the follow-up is the session's next turn)", env.TurnCount)
	}
	last := env.Messages[len(env.Messages)-1]
	if last.Role != turn.RoleUser || last.Metadata[messageTriggerKey] != TriggerFollowUp {
		t.Errorf("last message = %+v, want user turn tagged %s", last, TriggerFollowUp)
	}
	if env.Variables[varCLISession] != "s1+kick" {
		t.Errorf("cli_session = %q, want it to follow the delivered kick", env.Variables[varCLISession])
	}
	if n := env.Journal.EffectCount(turn.OpFollowUpKick); n != 1 {
		t.Errorf("settled follow-ups = %d, want 1", n)
	}

	// Same fact on the next tick: already delivered, never delivered twice.
	if out := Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now.Add(2*time.Minute)); len(out) != 0 {
		t.Fatalf("second pass outcomes = %+v, want none", out)
	}
	if len(r.sent) != 1 {
		t.Fatalf("duplicate delivery: %d kicks", len(r.sent))
	}

	// A new red head is a new event, and it resumes the SAME conversation
	// (the pointer followed the session across the first follow-up).
	next := redPR()
	next.HeadSHA = "fedcba9876543210"
	if out := Route(context.Background(), []github.PullRequest{next}, r, opts(dir), now.Add(3*time.Minute)); len(out) != 1 || out[0].Route != RouteResumed {
		t.Fatalf("new red SHA outcomes = %+v, want resumed", out)
	}
}

// The #6908 class: nothing may be routed for a PR this hive did not open.
func TestRoute_OnlyPRsThisHiveAuthoredAreEligible(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	r := newFakeResumer("s1")

	humanPR := redPR()
	humanPR.Author = "some-human"
	if out := Route(context.Background(), []github.PullRequest{humanPR}, r, opts(dir), now); len(out) != 0 || len(r.sent) != 0 {
		t.Fatalf("PR without a pointer was routed: outcomes=%+v kicks=%d", out, len(r.sent))
	}

	// The store sanitises IDs, so "a/b_c#1" and "a_b/c#1" share a file. A
	// pointer for one must never make the other eligible.
	if err := Record(context.Background(), dir, testAgent, "a/b_c", 1, "", "s1", now); err != nil {
		t.Fatal(err)
	}
	other := redPR()
	other.Repo, other.Number = "a_b/c", 1
	if out := Route(context.Background(), []github.PullRequest{other}, r, opts(dir), now); len(out) != 0 || len(r.sent) != 0 {
		t.Fatalf("colliding pointer made a foreign PR eligible: outcomes=%+v", out)
	}
}

func TestRoute_Exclusions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")

	draft, fork, running := redPR(), redPR(), redPR()
	draft.Draft = true
	fork.FromFork = true
	running.CIChecksRunning = true
	for name, pr := range map[string]github.PullRequest{"draft": draft, "fork": fork, "ci still running": running} {
		if out := Route(context.Background(), []github.PullRequest{pr}, r, opts(dir), now); len(out) != 0 {
			t.Errorf("%s: outcomes = %+v, want none", name, out)
		}
	}
	o := opts(dir)
	o.Skip = func(repo string, number int) bool { return repo == testRepo && number == testPR }
	if out := Route(context.Background(), []github.PullRequest{redPR()}, r, o, now); len(out) != 0 {
		t.Errorf("escalated PR routed: %+v", out)
	}
	if len(r.sent) != 0 {
		t.Fatalf("excluded PRs were kicked: %d", len(r.sent))
	}
}

func TestRoute_FallsBackWhenSessionCannotResume(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name   string
		r      Resumer
		setup  func(t *testing.T, dir string)
		at     time.Time
		reason string
	}{
		{name: "expired", r: newFakeResumer("s1"), at: now.Add(2 * time.Hour), reason: ReasonExpired},
		{name: "moved on", r: newFakeResumer("s2"), at: now, reason: ReasonSessionMoved},
		{name: "gone", r: &fakeResumer{sessions: map[string]string{}}, at: now, reason: ReasonSessionGone},
		{name: "no resumer", r: nil, at: now, reason: ReasonNoResumer},
		{name: "cap reached", r: newFakeResumer("s1"), at: now, reason: ReasonCapReached, setup: func(t *testing.T, dir string) {
			env := load(t, dir)
			env.TurnCount = MaxFollowUpsPerPR
			if err := (turn.FileStore{Dir: dir}).Persist(context.Background(), env); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "kick refused", r: &fakeResumer{sessions: map[string]string{testAgent: "s1"}, sendErr: errors.New("tmux gone")}, at: now, reason: "tmux gone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			record(t, dir, "s1", now)
			if tc.setup != nil {
				tc.setup(t, dir)
			}
			out := Route(context.Background(), []github.PullRequest{redPR()}, tc.r, opts(dir), tc.at)
			if len(out) != 1 || out[0].Route != RouteFallback || out[0].Reason != tc.reason {
				t.Fatalf("outcomes = %+v, want fallback %q", out, tc.reason)
			}
			env := load(t, dir)
			if len(env.Journal.Ambiguous()) != 0 || env.Journal.EffectCount() != 0 {
				t.Fatalf("fallback must settle the event as not-resumed: %+v", env.Journal.Entries)
			}
			if env.TurnCount != 0 && tc.name != "cap reached" {
				t.Fatalf("fallback must not append a turn: TurnCount=%d", env.TurnCount)
			}
			// Handed to the fresh-dispatch path once, not re-evaluated.
			if again := Route(context.Background(), []github.PullRequest{redPR()}, tc.r, opts(dir), tc.at); len(again) != 0 {
				t.Fatalf("fallback event re-routed: %+v", again)
			}
		})
	}
}

// A busy session defers; the intent survives on disk (a restart re-reads it)
// and the next pass delivers it exactly once.
func TestRoute_BusyDefersAndRequeues(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")
	r.sendErr = fmt.Errorf("%w: mid-turn", ErrBusy)

	out := Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now)
	if len(out) != 1 || out[0].Route != RouteDeferred {
		t.Fatalf("outcomes = %+v, want deferred", out)
	}
	deferred := load(t, dir)
	if amb := deferred.Journal.Ambiguous(); len(amb) != 1 {
		t.Fatalf("deferred follow-up must stay queued (intended) on disk: %+v", amb)
	}

	r.sendErr = nil
	out = Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now.Add(time.Minute))
	if len(out) != 1 || out[0].Route != RouteResumed || len(r.sent) != 1 {
		t.Fatalf("re-queued follow-up not resumed: outcomes=%+v kicks=%d", out, len(r.sent))
	}
	env := load(t, dir)
	if len(env.Journal.Ambiguous()) != 0 || env.Journal.EffectCount(turn.OpFollowUpKick) != 1 {
		t.Fatalf("journal after resume = %+v", env.Journal.Entries)
	}
}

// Restart safety: an intent left on disk by a process that died mid-delivery
// is picked up by the next process. The old CLI session is gone with the old
// process, so it falls back instead of being dropped or double-delivered.
func TestRoute_RestartRequeuesInFlightFollowUp(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	record(t, dir, "s1", now)
	busy := newFakeResumer("s1")
	busy.sendErr = ErrBusy
	Route(context.Background(), []github.PullRequest{redPR()}, busy, opts(dir), now)

	restarted := newFakeResumer("new-process-session")
	out := Route(context.Background(), []github.PullRequest{redPR()}, restarted, opts(dir), now.Add(time.Minute))
	if len(out) != 1 || out[0].Route != RouteFallback || out[0].Reason != ReasonSessionMoved {
		t.Fatalf("in-flight follow-up after restart = %+v, want fallback (not dropped)", out)
	}
	if len(restarted.sent) != 0 {
		t.Fatal("a follow-up must never be typed into a session that did not author the PR")
	}
}

func TestRoute_SupersededIntentIsSettled(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")
	r.sendErr = ErrBusy
	Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now)

	green := redPR()
	green.CIStatus = "success"
	if out := Route(context.Background(), []github.PullRequest{green}, r, opts(dir), now); len(out) != 0 {
		t.Fatalf("green PR outcomes = %+v", out)
	}
	env := load(t, dir)
	if len(env.Journal.Ambiguous()) != 0 {
		t.Fatalf("a follow-up whose fact vanished must be settled, not left queued: %+v", env.Journal.Entries)
	}
	if !strings.Contains(env.Journal.Entries[0].Error, ReasonSuperseded) {
		t.Fatalf("settle reason = %q", env.Journal.Entries[0].Error)
	}
}

func TestRoute_ReviewEventsAndThreads(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")

	pr := redPR()
	pr.CIStatus = "success"
	pr.Protection = &github.ProtectionFacts{
		ReviewDecision:     github.ReviewDecisionChangesRequested,
		ChangesRequestedBy: []string{"bob", "alice"},
	}
	report := github.ReviewThreadsReport{PRs: []github.ReviewThreadPR{
		{Repo: testRepo, Number: testPR, Threads: []github.ReviewThread{
			{ThreadID: "PRRT_1", Path: "src/a.go", Line: 12, Author: "review-bot", Body: "nil check missing"},
			{ThreadID: "PRRT_2", Path: "README.md", Author: "review-bot", Body: strings.Repeat("x", threadBodyRunes+10)},
			{ThreadID: "", Body: "no id, ignored"},
		}},
		{Repo: testRepo, Number: 7, Escalated: true, Threads: []github.ReviewThread{{ThreadID: "PRRT_9"}}},
		{Repo: testRepo, Number: 8},
	}}
	o := opts(dir)
	o.Threads = ThreadsFromReport(report)
	if len(o.Threads) != 1 {
		t.Fatalf("ThreadsFromReport kept %d PRs, want only the non-escalated one with threads", len(o.Threads))
	}

	out := Route(context.Background(), []github.PullRequest{pr}, r, o, now)
	if len(out) != 1 || len(out[0].Events) != 3 {
		t.Fatalf("outcomes = %+v, want one outcome with 3 events", out)
	}
	msg := r.sent[0]
	for _, want := range []string{"Changes requested by alice, bob", "review-bot on src/a.go:12 (thread PRRT_1): nil check missing", "on README.md (thread PRRT_2)", "..."} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}

	// A resolved thread drops out of the report; a new one is a new event.
	o.Threads = map[string][]github.ReviewThread{ThreadsKey(testRepo, testPR): {{ThreadID: "PRRT_3", Path: "b.go", Author: "review-bot", Body: "typo"}}}
	out = Route(context.Background(), []github.PullRequest{pr}, r, o, now)
	if len(out) != 1 || len(out[0].Events) != 1 || out[0].Events[0].Key != "thread:PRRT_3" {
		t.Fatalf("new-thread outcomes = %+v", out)
	}
}

func TestDetectEvents_ChangesRequestedWithoutReviewers(t *testing.T) {
	pr := github.PullRequest{Protection: &github.ProtectionFacts{ReviewDecision: github.ReviewDecisionChangesRequested}}
	ev := detectEvents(&pr, nil)
	if len(ev) != 1 || ev[0].Detail != "A reviewer requested changes." {
		t.Fatalf("events = %+v", ev)
	}
}

func TestRecord_ValidationRepointAndErrors(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if err := Record(context.Background(), dir, "", testRepo, testPR, "", "s1", now); err == nil {
		t.Fatal("Record without an agent must fail")
	}
	if err := Record(context.Background(), dir, testAgent, testRepo, 0, "", "s1", now); err == nil {
		t.Fatal("Record without a PR number must fail")
	}

	record(t, dir, "s1", now)
	env := load(t, dir)
	firstCreated := env.CreatedAt
	env.Journal.RecordIntent("k", turn.OpIntent{Kind: turn.OpFollowUpKick}, now)
	if err := (turn.FileStore{Dir: dir}).Persist(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	record(t, dir, "s9", now.Add(time.Minute))
	env = load(t, dir)
	if env.Variables[varCLISession] != "s9" || len(env.Journal.Entries) != 1 || !env.CreatedAt.Equal(firstCreated) {
		t.Fatalf("re-point must update the session and keep history: %+v", env)
	}

	notADir := filepath.Join(dir, "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Record(context.Background(), notADir, testAgent, testRepo, testPR, "", "s1", now); err == nil {
		t.Fatal("Record into an unreadable store must fail")
	}
}

func TestRoute_UnreadablePointerIsSkipped(t *testing.T) {
	dir := t.TempDir()
	// safeEnvelopeName("pr-followup:o/r#5") == "pr-followup_o_r_5".
	if err := os.WriteFile(filepath.Join(dir, "pr-followup_o_r_5.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	pr := redPR()
	pr.Repo, pr.Number = "o/r", 5
	r := newFakeResumer("s1")
	if out := Route(context.Background(), []github.PullRequest{pr}, r, Options{Dir: dir}, time.Now()); len(out) != 0 || len(r.sent) != 0 {
		t.Fatalf("corrupt pointer routed: %+v", out)
	}
}

func TestDirAndMessageFallbacks(t *testing.T) {
	t.Setenv(DirEnvVar, "")
	if Dir() != DefaultDir {
		t.Fatalf("Dir() = %q, want default", Dir())
	}
	t.Setenv(DirEnvVar, "/tmp/elsewhere")
	if Dir() != "/tmp/elsewhere" {
		t.Fatalf("Dir() = %q, want env override", Dir())
	}

	pr := github.PullRequest{Repo: testRepo, Number: testPR, URL: testURL, HeadSHA: "abc", CIFailureExcerpt: strings.Repeat("e", ciExcerptRunes+5)}
	msg := BuildMessage(&pr, "", []Event{{Detail: ciDetail(&pr)}})
	if !strings.Contains(msg, testURL) || !strings.Contains(msg, "CI failed on abc.") || !strings.HasSuffix(strings.Split(msg, "\n")[3], "...") {
		t.Fatalf("message = %q", msg)
	}
	noURL := github.PullRequest{Repo: testRepo, Number: testPR}
	if strings.Contains(BuildMessage(&noURL, "", nil), "(") {
		t.Fatal("a PR without a URL must not render an empty link")
	}
}

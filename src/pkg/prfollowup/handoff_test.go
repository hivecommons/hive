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

const testNote = "Why: nil deref when the config has no repos\nApproach: guard in loadRepos\nRejected alternatives: defaulting repos to the org list"

func recordNote(t *testing.T, dir, session string, now time.Time) {
	t.Helper()
	if err := RecordWithNote(context.Background(), dir, testAgent, testRepo, testPR, testURL, session, testNote, now); err != nil {
		t.Fatalf("RecordWithNote: %v", err)
	}
}

func greenPR() github.PullRequest {
	pr := redPR()
	pr.CIStatus = "success"
	return pr
}

func humanComment(id string, at time.Time) github.PRComment {
	return github.PRComment{ID: "conversation:" + id, Kind: github.PRCommentConversation, Author: "alice", Body: "please rename Foo to Bar", CreatedAt: at}
}

func withComments(o Options, comments ...github.PRComment) Options {
	o.Comments = map[string][]github.PRComment{ThreadsKey(testRepo, testPR): comments}
	return o
}

func stats(t *testing.T, dir string) Stats {
	t.Helper()
	s, err := ReadStats(dir)
	if err != nil {
		t.Fatalf("ReadStats: %v", err)
	}
	return s
}

// Restart: the new process's session can never match the pointer, so the
// follow-up falls back. The fresh kick that picks it up must still carry the
// PR's original reasoning.
func TestHandoff_RestartFallbackCarriesNote(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)

	restarted := newFakeResumer("new-process-session")
	out := Route(context.Background(), []github.PullRequest{redPR()}, restarted, opts(dir), now.Add(time.Minute))
	if len(out) != 1 || out[0].Route != RouteFallback || out[0].Reason != ReasonSessionMoved {
		t.Fatalf("outcomes = %+v, want fallback (session moved)", out)
	}
	section := HandoffSection(context.Background(), dir, testAgent)
	for _, want := range []string{"PR HANDOFF", testRepo + "#42", testURL, "nil deref when the config has no repos", "Rejected alternatives: defaulting repos"} {
		if !strings.Contains(section, want) {
			t.Errorf("handoff section missing %q:\n%s", want, section)
		}
	}
	if other := HandoffSection(context.Background(), dir, "someone-else"); other != "" {
		t.Fatalf("another agent got this PR's handoff:\n%s", other)
	}
	if s := stats(t, dir); s.Fallback[ReasonSessionMoved] != 1 || s.Resumed != 0 {
		t.Fatalf("stats = %+v, want one session-moved fallback", s)
	}
}

// A regular kick in between (the agent's session moved on and /clear'd the
// conversation) is the same situation as a restart: fallback plus note. The
// note stops being handed out once the PR has nothing left to follow up.
func TestHandoff_RegularKickInBetweenCarriesNoteUntilSettled(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)
	r := newFakeResumer("s1")

	// The agent's next cadence kick lands before CI settles red.
	r.sessions[testAgent] = "s2-after-cadence-kick"
	out := Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now.Add(time.Minute))
	if len(out) != 1 || out[0].Route != RouteFallback || out[0].Reason != ReasonSessionMoved {
		t.Fatalf("outcomes = %+v, want fallback", out)
	}
	if len(r.sent) != 0 {
		t.Fatal("a follow-up must never be typed into a session that did not author the PR")
	}
	if section := HandoffSection(context.Background(), dir, "other", testAgent); !strings.Contains(section, "guard in loadRepos") {
		t.Fatalf("fresh kick lacks the handoff note:\n%s", section)
	}

	// Still red on the next tick: nothing new to route, note still handed out.
	Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now.Add(2*time.Minute))
	if HandoffSection(context.Background(), dir, testAgent) == "" {
		t.Fatal("note dropped while CI is still red")
	}

	// CI goes green: nothing left to follow up, so no more note.
	Route(context.Background(), []github.PullRequest{greenPR()}, r, opts(dir), now.Add(3*time.Minute))
	if section := HandoffSection(context.Background(), dir, testAgent); section != "" {
		t.Fatalf("settled PR still handed off:\n%s", section)
	}
}

// Even a resumed follow-up leaves the PR live: the agent's NEXT regular kick
// clears that conversation, so it must carry the note too.
func TestHandoff_ResumedPRStillCarriesNoteForNextKick(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)
	r := newFakeResumer("s1")
	if out := Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now); len(out) != 1 || out[0].Route != RouteResumed {
		t.Fatalf("outcomes = %+v, want resumed", out)
	}
	if !strings.Contains(HandoffSection(context.Background(), dir, testAgent), "guard in loadRepos") {
		t.Fatal("a resumed but still-red PR must hand its note to the next fresh kick")
	}
	if s := stats(t, dir); s.Resumed != 1 {
		t.Fatalf("stats = %+v, want one resumed", s)
	}
}

// A human comment resumes the live authoring session, exactly once per
// comment id; a comment older than the pointer is never routed.
func TestRoute_HumanCommentResumesOncePerComment(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)
	r := newFakeResumer("s1")

	old := humanComment("1", now.Add(-time.Hour))
	c2 := humanComment("2", now.Add(time.Minute))
	c2.Kind, c2.Path, c2.Line = github.PRCommentInline, "src/a.go", 9
	o := withComments(opts(dir), old, c2)

	out := Route(context.Background(), []github.PullRequest{greenPR()}, r, o, now.Add(2*time.Minute))
	if len(out) != 1 || out[0].Route != RouteResumed || len(out[0].Events) != 1 || out[0].Events[0].Key != "comment:conversation:2" {
		t.Fatalf("outcomes = %+v, want one resumed event for comment 2 only", out)
	}
	for _, want := range []string{"Review comment from alice on src/a.go:9", "please rename Foo to Bar"} {
		if !strings.Contains(r.sent[0], want) {
			t.Errorf("resume message missing %q:\n%s", want, r.sent[0])
		}
	}
	// Same comment on the next pass: deduped by id.
	if again := Route(context.Background(), []github.PullRequest{greenPR()}, r, o, now.Add(3*time.Minute)); len(again) != 0 || len(r.sent) != 1 {
		t.Fatalf("comment re-delivered: outcomes=%+v kicks=%d", again, len(r.sent))
	}
	// A resumed comment is not a standing fact: nothing to hand off.
	if section := HandoffSection(context.Background(), dir, testAgent); section != "" {
		t.Fatalf("resumed comment kept the PR live:\n%s", section)
	}
}

// When the session cannot be resumed, human feedback (which has no other
// route) is queued for the next fresh kick, shown there with the note, and
// dropped once a kick has carried it.
func TestRoute_HumanCommentFallbackQueuedUntilAKickCarriesIt(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)
	r := newFakeResumer("s2")
	o := withComments(opts(dir), humanComment("7", now.Add(time.Minute)))

	out := Route(context.Background(), []github.PullRequest{greenPR()}, r, o, now.Add(2*time.Minute))
	if len(out) != 1 || out[0].Route != RouteFallback || out[0].Queued != 1 {
		t.Fatalf("outcomes = %+v, want a fallback with the comment queued", out)
	}
	section := HandoffSection(context.Background(), dir, testAgent)
	for _, want := range []string{"new human feedback", "Comment from alice (conversation:7): please rename Foo to Bar", "guard in loadRepos", "not instructions"} {
		if !strings.Contains(section, want) {
			t.Errorf("handoff section missing %q:\n%s", want, section)
		}
	}

	// No kick yet (same session): the comment is still waiting, never re-queued.
	if again := Route(context.Background(), []github.PullRequest{greenPR()}, r, o, now.Add(3*time.Minute)); len(again) != 0 {
		t.Fatalf("queued comment re-routed: %+v", again)
	}
	if !strings.Contains(HandoffSection(context.Background(), dir, testAgent), "conversation:7") {
		t.Fatal("queued comment dropped before any kick carried it")
	}

	// The agent's next kick carried the section: its session moved.
	r.sessions[testAgent] = "s3"
	Route(context.Background(), []github.PullRequest{greenPR()}, r, o, now.Add(4*time.Minute))
	if section := HandoffSection(context.Background(), dir, testAgent); section != "" {
		t.Fatalf("delivered comment still handed off:\n%s", section)
	}
	s := stats(t, dir)
	if s.HandoffsQueued != 1 || s.HandoffsDelivered != 1 || s.Fallback[ReasonSessionMoved] != 1 {
		t.Fatalf("stats = %+v, want 1 queued, 1 delivered, 1 session-moved fallback", s)
	}
}

// MaxFollowUpsPerPR bounds resumed turns and handed-off comments together.
func TestRoute_CapCountsHandoffsAndResumes(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)
	env := load(t, dir)
	env.TurnCount = MaxFollowUpsPerPR - 1
	if err := (turn.FileStore{Dir: dir}).Persist(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	r := newFakeResumer("moved-on")
	o := withComments(opts(dir), humanComment("1", now.Add(time.Minute)), humanComment("2", now.Add(2*time.Minute)))

	out := Route(context.Background(), []github.PullRequest{greenPR()}, r, o, now.Add(3*time.Minute))
	if len(out) != 1 || out[0].Queued != 1 {
		t.Fatalf("outcomes = %+v, want exactly one comment queued under the cap", out)
	}
	if got := followUpsUsed(ptr(load(t, dir))); got != MaxFollowUpsPerPR {
		t.Fatalf("follow-ups used = %d, want the cap %d", got, MaxFollowUpsPerPR)
	}

	// At the cap even a live session is not resumed any more.
	r.sessions[testAgent] = load(t, dir).Variables[varCLISession]
	third := withComments(opts(dir), humanComment("3", now.Add(4*time.Minute)))
	out = Route(context.Background(), []github.PullRequest{greenPR()}, r, third, now.Add(5*time.Minute))
	if len(out) != 1 || out[0].Route != RouteFallback || out[0].Reason != ReasonCapReached || out[0].Queued != 0 || len(r.sent) != 0 {
		t.Fatalf("over-cap outcomes = %+v kicks=%d, want a cap fallback, nothing queued or sent", out, len(r.sent))
	}
}

func ptr(env turn.SessionEnvelope) *turn.SessionEnvelope { return &env }

// A skip is counted and audited once per transition, never per tick, and it
// journals nothing, so the PR routes normally once it is eligible again.
func TestRoute_SkipTransitionsCountedOnceAndAudited(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)
	r := newFakeResumer("s1")
	var audits []string
	o := opts(dir)
	o.Audit = func(action, agent string, fields map[string]any) {
		audits = append(audits, fmt.Sprintf("%s|%s|%v|%v", action, agent, fields["outcome"], fields["reason"]))
	}
	draft := redPR()
	draft.Draft = true
	for i := 0; i < 3; i++ {
		if out := Route(context.Background(), []github.PullRequest{draft}, r, o, now); len(out) != 0 {
			t.Fatalf("draft routed: %+v", out)
		}
	}
	if s := stats(t, dir); s.Skipped[SkipDraft] != 1 {
		t.Fatalf("skipped counters = %+v, want the draft counted once", s.Skipped)
	}
	if env := load(t, dir); len(env.Journal.Entries) != 0 {
		t.Fatalf("a skipped PR must journal nothing: %+v", env.Journal.Entries)
	}
	// Ready for review: the same red SHA now resumes.
	if out := Route(context.Background(), []github.PullRequest{redPR()}, r, o, now); len(out) != 1 || out[0].Route != RouteResumed {
		t.Fatalf("un-drafted PR outcomes = %+v, want resumed", out)
	}
	fork := redPR()
	fork.FromFork = true
	Route(context.Background(), []github.PullRequest{fork}, r, o, now)
	want := []string{
		AuditActionSkipped + "|" + testAgent + "|skipped|" + SkipDraft,
		AuditActionRouted + "|" + testAgent + "|" + RouteResumed + "|<nil>",
		AuditActionSkipped + "|" + testAgent + "|skipped|" + SkipFork,
	}
	if strings.Join(audits, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audits =\n%s\nwant\n%s", strings.Join(audits, "\n"), strings.Join(want, "\n"))
	}
}

// A refusal from the agent manager is kept verbatim on the outcome but
// counted under one fixed key.
func TestRoute_KickRefusedCountedUnderFixedKey(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	record(t, dir, "s1", now)
	r := newFakeResumer("s1")
	r.sendErr = errors.New("tmux session for scanner not found")
	out := Route(context.Background(), []github.PullRequest{redPR()}, r, opts(dir), now)
	if len(out) != 1 || out[0].Reason != "tmux session for scanner not found" {
		t.Fatalf("outcomes = %+v", out)
	}
	if s := stats(t, dir); s.Fallback[ReasonKickRefused] != 1 || len(s.Fallback) != 1 {
		t.Fatalf("fallback counters = %+v, want one %q", s.Fallback, ReasonKickRefused)
	}
	// Deferred attempts are counted too (each retry counts).
	r.sendErr = ErrBusy
	next := redPR()
	next.HeadSHA = "feedfacefeedface"
	for i := 0; i < 2; i++ {
		if out := Route(context.Background(), []github.PullRequest{next}, r, opts(dir), now); len(out) != 1 || out[0].Route != RouteDeferred {
			t.Fatalf("busy session outcomes = %+v, want deferred", out)
		}
	}
	if s := stats(t, dir); s.Deferred != 2 {
		t.Fatalf("deferred = %d, want 2", s.Deferred)
	}
}

func TestBuildHandoffNote(t *testing.T) {
	body := strings.Join([]string{
		"Fixes the crash on an empty config.",
		"",
		"## Summary",
		"loadRepos dereferences a nil slice.",
		"",
		"## Approach",
		"Guard the slice and return an empty list.",
		"",
		"## Alternatives considered",
		"Defaulting to the org's repos: hides misconfiguration.",
		"",
		"**Steps to reproduce:**",
		"Run hive with `repos: []`.",
		"",
		"## Files touched",
		"- `src/pkg/config/repos.go`",
		"* src/pkg/config/repos_test.go",
		"",
		"## Unrelated heading",
		"this text is not part of any field",
		"",
		github.AttributionTrailerPrefix + " agent=scanner backend=claude",
		"## Why",
		"after the trailer: ignored",
	}, "\n")
	note := BuildHandoffNote(nil, body)
	for _, want := range []string{
		"Why: loadRepos dereferences a nil slice.",
		"Approach: Guard the slice and return an empty list.",
		"Rejected alternatives: Defaulting to the org's repos: hides misconfiguration.",
		"Repro: Run hive with `repos: []`.",
		"Files touched: src/pkg/config/repos.go, src/pkg/config/repos_test.go",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note missing %q:\n%s", want, note)
		}
	}
	for _, banned := range []string{"not part of any field", "after the trailer", "agent=scanner"} {
		if strings.Contains(note, banned) {
			t.Errorf("note contains %q:\n%s", banned, note)
		}
	}

	// The agent's own summary wins field by field; the body fills the rest.
	agent := BuildHandoffNote(&github.PRHandoff{Why: "agent why", Files: []string{"a.go"}}, body)
	if !strings.Contains(agent, "Why: agent why") || !strings.Contains(agent, "Files touched: a.go") || !strings.Contains(agent, "Approach: Guard the slice") {
		t.Fatalf("merged note = %q", agent)
	}
	agentAll := BuildHandoffNote(&github.PRHandoff{Why: "w", Approach: "a", Rejected: "r", Repro: "p"}, "")
	if agentAll != "Why: w\nApproach: a\nRejected alternatives: r\nRepro: p" {
		t.Fatalf("agent-only note = %q", agentAll)
	}

	// No headings at all: the body itself is the "why".
	if got := BuildHandoffNote(nil, "Just a sentence.\nFixes #3"); got != "Why: Just a sentence. Fixes #3" {
		t.Fatalf("heading-less note = %q", got)
	}
	if got := BuildHandoffNote(nil, "   "); got != "" {
		t.Fatalf("empty body note = %q", got)
	}

	// Bounds: each field and the whole note.
	huge := strings.Repeat("word ", MaxHandoffNoteRunes)
	big := BuildHandoffNote(&github.PRHandoff{Why: huge, Approach: huge, Rejected: huge, Repro: huge}, "")
	if n := len([]rune(big)); n > MaxHandoffNoteRunes+len("...") {
		t.Fatalf("note is %d runes, over the %d bound", n, MaxHandoffNoteRunes)
	}
	for _, line := range strings.Split(big, "\n") {
		if n := len([]rune(line)); n > len("Rejected alternatives: ")+handoffFieldRunes+len("...") {
			t.Fatalf("field line is %d runes, over the per-field bound", n)
		}
	}
	var files []string
	for i := 0; i < maxHandoffFiles+5; i++ {
		files = append(files, fmt.Sprintf("f%d.go", i))
	}
	listed := BuildHandoffNote(&github.PRHandoff{Files: append([]string{"  "}, files...)}, "")
	if strings.Count(listed, ".go") != maxHandoffFiles {
		t.Fatalf("files listed = %d, want the %d bound", strings.Count(listed, ".go"), maxHandoffFiles)
	}
}

func TestRecordWithNote_KeepsNoteOnRepoint(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordNote(t, dir, "s1", now)
	record(t, dir, "s2", now.Add(time.Minute)) // re-point without a note
	env := load(t, dir)
	if env.Variables[varNote] != testNote || env.Variables[varCLISession] != "s2" {
		t.Fatalf("re-point lost the note or kept the old session: %v", env.Variables)
	}
}

func TestHandoffSection_EdgeCases(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	if HandoffSection(context.Background(), dir) != "" || HandoffSection(context.Background(), "", testAgent) != "" {
		t.Fatal("no names or no dir must render nothing")
	}
	if HandoffSection(context.Background(), filepath.Join(dir, "missing"), testAgent) != "" {
		t.Fatal("a missing dir must render nothing")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if HandoffSection(cancelled, dir, testAgent) != "" {
		t.Fatal("a cancelled context must render nothing")
	}

	// More live PRs than one kick details: the rest are counted.
	store := turn.FileStore{Dir: dir}
	for n := 1; n <= maxHandoffPRsPerKick+2; n++ {
		if err := RecordWithNote(context.Background(), dir, testAgent, testRepo, n, "", "s1", "Why: pr "+fmt.Sprint(n), now); err != nil {
			t.Fatal(err)
		}
		env, err := store.Load(context.Background(), PointerID(testRepo, n))
		if err != nil {
			t.Fatal(err)
		}
		env.Variables[varLive] = liveFlag
		if err := store.Persist(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	// Non-pointer files and a corrupt pointer are ignored.
	if err := os.WriteFile(filepath.Join(dir, "pr-followup_broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "pr-followup_dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	section := HandoffSection(context.Background(), dir, testAgent)
	if !strings.Contains(section, "(7)") || !strings.Contains(section, "... and 2 more PRs") || strings.Contains(section, testRepo+"#6\n") {
		t.Fatalf("section = %s", section)
	}

	// A live pointer with neither a note nor queued feedback has nothing to say.
	bare := t.TempDir()
	record(t, bare, "s1", now)
	env := load(t, bare)
	env.Variables[varLive] = liveFlag
	env.Variables[varPending] = "not json"
	if err := (turn.FileStore{Dir: bare}).Persist(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if got := HandoffSection(context.Background(), bare, testAgent); got != "" {
		t.Fatalf("empty handoff rendered: %s", got)
	}
}

func TestReconcilePending_NoResumerKeepsQueue(t *testing.T) {
	env := turn.SessionEnvelope{Variables: map[string]string{}}
	setPendingHandoffs(&env, []pendingHandoff{{Key: "comment:x", Session: "s1"}})
	var d Stats
	if reconcilePending(&env, nil, &d) || len(pendingHandoffs(&env)) != 1 {
		t.Fatal("without a resumer nothing can be known delivered")
	}
	setPendingHandoffs(&env, nil)
	if _, ok := env.Variables[varPending]; ok {
		t.Fatal("an empty queue must be removed")
	}
}

func TestStats_ReadAndCorruption(t *testing.T) {
	dir := t.TempDir()
	if s, err := ReadStats(dir); err != nil || !s.isZero() {
		t.Fatalf("missing stats = %+v, %v; want zero, nil", s, err)
	}
	if err := os.WriteFile(filepath.Join(dir, StatsFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStats(dir); err == nil {
		t.Fatal("corrupt stats must be an error")
	}
	d := Stats{Resumed: 2}
	d.addPruned(PruneMerged)
	if err := addStats(dir, d, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s := stats(t, dir); s.Resumed != 2 || s.Pruned[PruneMerged] != 1 {
		t.Fatalf("stats after restart-from-corrupt = %+v", s)
	}
	if err := addStats(dir, Stats{}, time.Now()); err != nil {
		t.Fatal("a zero delta is a no-op")
	}
	notADir := filepath.Join(dir, StatsFile)
	if err := addStats(notADir, Stats{Resumed: 1}, time.Now()); err == nil {
		t.Fatal("writing under a file must fail")
	}
	if _, err := ReadStats(notADir); err == nil {
		t.Fatal("reading under a file must fail")
	}
}

package prfollowup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// Backend-native resume handles (hivecommons/hive#9606): the handle is
// captured when the PR opens, stored beside the pointer, and offered to the
// session that picks the PR up after the authoring conversation is gone.

func testResumeHandle(t *testing.T, at time.Time) ResumeHandle {
	t.Helper()
	transcript := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return ResumeHandle{
		Backend:    "copilot",
		SessionID:  "sess-0001",
		Transcript: transcript,
		Command:    "copilot --resume sess-0001",
		CapturedAt: at,
	}
}

func recordResume(t *testing.T, dir string, h ResumeHandle, now time.Time) {
	t.Helper()
	if err := RecordWithResume(context.Background(), dir, testAgent, testRepo, testPR, testURL, "s1", testNote, h, now); err != nil {
		t.Fatalf("RecordWithResume: %v", err)
	}
}

func TestResumeHandle_RecordedOnPointerAndOfferedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	handle := testResumeHandle(t, now)
	recordResume(t, dir, handle, now)

	env := load(t, dir)
	if got := env.Variables[varResumeID]; got != handle.SessionID {
		t.Errorf("pointer resume id = %q, want %q", got, handle.SessionID)
	}
	if got := env.Variables[varResumeCommand]; got != handle.Command {
		t.Errorf("pointer resume command = %q, want %q", got, handle.Command)
	}
	if got := resumeHandleOf(&env); !got.CapturedAt.Equal(handle.CapturedAt.UTC().Truncate(time.Second)) {
		t.Errorf("captured at = %v, want %v (RFC3339)", got.CapturedAt, handle.CapturedAt)
	}

	// A restart: the follow-up cannot be resumed live, so the next fresh kick
	// carries the note AND the command that reopens the authoring transcript.
	restarted := newFakeResumer("new-process-session")
	out := Route(context.Background(), []github.PullRequest{redPR()}, restarted, opts(dir), now.Add(time.Minute))
	if len(out) != 1 || out[0].Route != RouteFallback {
		t.Fatalf("outcomes = %+v, want a fallback", out)
	}
	section := HandoffSectionWithResume(context.Background(), dir, time.Hour, now.Add(time.Minute), testAgent)
	for _, want := range []string{handle.Command, handle.Transcript, "resume the conversation that opened it"} {
		if !strings.Contains(section, want) {
			t.Errorf("handoff section missing %q:\n%s", want, section)
		}
	}
}

func TestResumeHandle_StaleHandleIsNotOffered(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordResume(t, dir, testResumeHandle(t, now.Add(-96*time.Hour)), now)
	Route(context.Background(), []github.PullRequest{redPR()}, newFakeResumer("other"), opts(dir), now)

	section := HandoffSectionWithResume(context.Background(), dir, 72*time.Hour, now, testAgent)
	if section == "" || strings.Contains(section, "copilot --resume") {
		t.Errorf("stale handle offered (or note lost):\n%s", section)
	}
	// With the staleness check disabled the same handle is offered again.
	if s := HandoffSectionWithResume(context.Background(), dir, 0, now, testAgent); !strings.Contains(s, "copilot --resume") {
		t.Errorf("handle withheld with staleness disabled:\n%s", s)
	}
}

func TestResumeHandle_DeletedTranscriptIsNotOffered(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	handle := testResumeHandle(t, now)
	recordResume(t, dir, handle, now)
	Route(context.Background(), []github.PullRequest{redPR()}, newFakeResumer("other"), opts(dir), now)
	if err := os.Remove(handle.Transcript); err != nil {
		t.Fatalf("remove transcript: %v", err)
	}

	section := HandoffSectionWithResume(context.Background(), dir, time.Hour, now, testAgent)
	if strings.Contains(section, "copilot --resume") {
		t.Errorf("handle for a deleted transcript offered:\n%s", section)
	}
	if !strings.Contains(section, "nil deref when the config has no repos") {
		t.Errorf("handoff note lost with the handle:\n%s", section)
	}
}

// A re-point from a backend with no capturable handle (or before the manager
// is wired) must not erase a handle captured earlier.
func TestResumeHandle_ZeroHandleKeepsTheStoredOne(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	handle := testResumeHandle(t, now)
	recordResume(t, dir, handle, now)
	recordNote(t, dir, "s2", now.Add(time.Minute))

	env := load(t, dir)
	if got := resumeHandleOf(&env); got.SessionID != handle.SessionID {
		t.Errorf("resume handle after a handle-less re-point = %+v, want %q kept", got, handle.SessionID)
	}
}

func TestResumeHandle_FreshnessAndValidity(t *testing.T) {
	now := time.Now()
	if (ResumeHandle{}).Valid() {
		t.Error("zero handle reports valid")
	}
	if (ResumeHandle{SessionID: "x"}).Valid() {
		t.Error("handle with no command reports valid")
	}
	h := ResumeHandle{SessionID: "x", Command: "claude --resume x", CapturedAt: now.Add(-2 * time.Hour)}
	if !h.Fresh(3*time.Hour, now) {
		t.Error("handle inside the window reported stale")
	}
	if h.Fresh(time.Hour, now) {
		t.Error("handle outside the window reported fresh")
	}
	if !(ResumeHandle{SessionID: "x", Command: "c"}).Fresh(time.Hour, now) {
		t.Error("handle with no capture time should not expire")
	}
}

// HandoffSection keeps its signature and its default staleness limit.
func TestResumeHandle_DefaultSectionUsesDefaultMaxAge(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	recordResume(t, dir, testResumeHandle(t, now), now)
	Route(context.Background(), []github.PullRequest{redPR()}, newFakeResumer("other"), opts(dir), now)
	if !strings.Contains(HandoffSection(context.Background(), dir, testAgent), "copilot --resume sess-0001") {
		t.Error("default HandoffSection did not offer a freshly captured handle")
	}
}

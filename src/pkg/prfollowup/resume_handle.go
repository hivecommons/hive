package prfollowup

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/turn"
)

// Backend-native resume handles (hivecommons/hive#9606).
//
// The live resume path (Route -> Resumer.SendResumeKick) only reaches a
// conversation that is still in its tmux pane. A pod restart kills it, and
// #9583 shipped the compact handoff note as the thing that survives. Some of
// the backend CLIs the hive drives persist the conversation itself and can be
// pointed back at it by id (claude --resume, copilot --resume, codex resume,
// gemini --resume, pi --session, omp --resume).
// When the PR opens, cmd/hive captures that id (agent.CaptureResumeHandle)
// and stores it here beside the pointer.
//
// The handle is then offered, not executed: a fresh session that picks the PR
// up is told the command that reopens the authoring transcript, and where the
// transcript file is, alongside the note. Reopening a whole transcript
// resumes its full token cost too, so whether to do it is the agent's call.
// A handle older than the configured staleness limit ("transcript gone or too
// old") is not offered at all, and a handle whose transcript has since been
// deleted is dropped when it is read.

// DefaultResumeIDMaxAge is the staleness limit used when a caller has no
// configured one. It mirrors config.DefaultPRFollowUpResumeIDMaxAge;
// HandoffSectionWithResume takes the configured value, and this package does
// not import pkg/config (the kick builder passes it in).
const DefaultResumeIDMaxAge = 72 * time.Hour

// ResumeHandle is the backend-native handle saved for one PR. It mirrors
// agent.ResumeHandle; this package does not import pkg/agent (the agent
// manager reaches it through the Resumer interface, not the other way round).
type ResumeHandle struct {
	// Backend is the CLI that owns the conversation ("claude", "copilot",
	// "codex", "gemini", "pi", "omp").
	Backend string
	// SessionID is that CLI's own conversation id.
	SessionID string
	// Transcript is the file the CLI persisted the conversation in.
	Transcript string
	// Command reopens the conversation from a shell.
	Command string
	// CapturedAt is when the handle was captured (PR-open time).
	CapturedAt time.Time
}

// Valid reports whether the handle names a conversation at all.
func (h ResumeHandle) Valid() bool {
	return strings.TrimSpace(h.SessionID) != "" && strings.TrimSpace(h.Command) != ""
}

// Fresh reports whether the handle is still worth offering: it names a
// conversation and was captured within maxAge. A zero or negative maxAge
// disables the staleness check.
func (h ResumeHandle) Fresh(maxAge time.Duration, now time.Time) bool {
	if !h.Valid() {
		return false
	}
	if maxAge <= 0 || h.CapturedAt.IsZero() {
		return true
	}
	return now.Sub(h.CapturedAt) <= maxAge
}

// setResumeHandle stores h on the pointer. A zero handle leaves whatever is
// already stored in place: a re-point from a backend with no capturable
// handle must not erase one captured earlier.
func setResumeHandle(env *turn.SessionEnvelope, h ResumeHandle) {
	if !h.Valid() {
		return
	}
	env.Variables[varResumeBackend] = h.Backend
	env.Variables[varResumeID] = h.SessionID
	env.Variables[varResumeTranscript] = h.Transcript
	env.Variables[varResumeCommand] = h.Command
	if !h.CapturedAt.IsZero() {
		env.Variables[varResumeAt] = h.CapturedAt.UTC().Format(time.RFC3339)
	}
}

// resumeHandleOf reads the handle stored on a pointer.
func resumeHandleOf(env *turn.SessionEnvelope) ResumeHandle {
	h := ResumeHandle{
		Backend:    env.Variables[varResumeBackend],
		SessionID:  env.Variables[varResumeID],
		Transcript: env.Variables[varResumeTranscript],
		Command:    env.Variables[varResumeCommand],
	}
	if at, err := time.Parse(time.RFC3339, env.Variables[varResumeAt]); err == nil {
		h.CapturedAt = at
	}
	return h
}

// renderResumeHandle renders the handle's lines for a PR's handoff entry,
// indented to sit under it. Empty when there is nothing to offer.
func renderResumeHandle(h ResumeHandle, maxAge time.Duration, now time.Time) string {
	if !h.Fresh(maxAge, now) {
		return ""
	}
	var b strings.Builder
	b.WriteString("    resume the conversation that opened it (optional; it costs its own tokens):\n")
	fmt.Fprintf(&b, "      %s\n", h.Command)
	if h.Transcript != "" {
		fmt.Fprintf(&b, "      transcript: %s\n", h.Transcript)
	}
	return b.String()
}

package crux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// Hive-provided capabilities (hivecommons/hive#14138, stage 1 of the
// sidecar plan). These are values the host hands the runtime, not things the
// runtime does: the GitHub App token minter, the opt-in agent mint, the
// Linear credential and the PR creator. They are defined here so a host can
// name them without importing crux/agent, and they are deliberately NOT
// methods of Runtime; Host is how an in-process runtime receives them, and a
// sidecar runtime will receive them over a hive-side callback endpoint
// (stage 2 decides the shape). crux/agent aliases every one of them.

// AppTokenMinter is implemented by the host's GitHub App auth
// (pkg/github.AppAuth) to mint per-agent scoped tokens.
type AppTokenMinter interface {
	WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error
}

// AgentMintIssuer is implemented by mint.AgentMinter. It is the OPT-IN,
// ADDITIONAL short-lived-credential path: when the mint is enabled it issues a
// scoped OIDC/JWT for an agent (subject=agent, scopes from its tier) that the
// agent can present ALONGSIDE its GitHub App token to a WIF broker. It never
// replaces WriteAgentToken. Enabled() reports false (and MintAgentToken returns
// "" with no error) when the mint is off, so wiring stays a strict no-op by
// default.
type AgentMintIssuer interface {
	Enabled() bool
	MintAgentToken(agentName, tier string) (string, error)
}

// LinearCredential is what an agent authenticates to api.linear.app with.
// Exactly one of the two is expected to be set: AccessToken is the installed
// Linear agent app's OAuth token (sent as `Authorization: ****** so writes
// are authored by the app identity — the parity of GitHub's App bot); APIKey
// is the work-source personal API key (sent bare as `Authorization: …`), the
// fallback when no workspace is connected. Empty means "inject nothing".
type LinearCredential struct {
	AccessToken string
	APIKey      string
}

// Empty reports whether no credential is available.
func (c LinearCredential) Empty() bool { return c.AccessToken == "" && c.APIKey == "" }

// PRCreator opens a pull request on behalf of a sandboxed agent. The host's
// GitHub client implements it; the runtime never holds the credential.
type PRCreator interface {
	CreatePR(ctx context.Context, repo, head, base, title, body string) (PRResult, error)
}

// ResumeHandle names a backend-native conversation that can be resumed after
// the authoring tmux pane is gone.
type ResumeHandle struct {
	// Backend is the CLI the handle belongs to ("claude", "copilot", ...).
	Backend string
	// SessionID is the backend's own conversation id.
	SessionID string
	// Transcript is the file the backend persists that conversation in, so a
	// fresh session can read the reasoning even if resume itself fails. For
	// goose, whose conversations live in a SQLite store rather than a file
	// named by id, it is the per-launch CLI log that names the id instead.
	Transcript string
	// Command resumes the conversation from a shell in the agent's pane.
	Command string
	// LastActive is the transcript's modification time when it was captured.
	LastActive time.Time
	// CapturedAt is when the hive captured the handle (PR-open time); the
	// staleness clock runs from here.
	CapturedAt time.Time
}

// String renders a handle for logs.
func (h ResumeHandle) String() string {
	if h.SessionID == "" {
		return ""
	}
	return fmt.Sprintf("%s session %s (%s)", h.Backend, h.SessionID, h.Transcript)
}

// TranscriptExists reports whether the captured transcript is still on disk.
func (h ResumeHandle) TranscriptExists() bool {
	if h.Transcript == "" {
		return false
	}
	_, err := os.Stat(h.Transcript)
	return err == nil
}

// ErrResumeBusy means the authoring session still exists but cannot take a
// follow-up right now (mid-turn, provider backoff, restart hold). It is
// transient: the caller keeps the follow-up queued and retries.
var ErrResumeBusy = errors.New("agent session busy; follow-up deferred")

// ErrResumeSessionGone means the session that authored the PR no longer
// exists, or the agent has moved on to other work since (a newer kick, a
// restart, a relaunch). The caller must fall back to a fresh dispatch.
var ErrResumeSessionGone = errors.New("authoring session no longer live")

// ErrResumeUnsupported means this agent's execution mode has no resumable
// conversation to type into (sandbox jobs, the headless agy runner) or the
// operator paused it. The caller falls back to a fresh dispatch.
var ErrResumeUnsupported = errors.New("agent cannot resume a session")

// StartFailureSnapshot is an agent's current start-failure record as seen
// from outside the runtime (the dashboard status builder, the heartbeat).
// Blocked is the fleet-visible bit: the agent has failed the same way enough
// times that the runtime no longer relaunches it on the tick.
type StartFailureSnapshot struct {
	Reason       string
	Count        int
	LastAt       time.Time
	LastExitCode *int
	LastSignal   string
	Blocked      bool
}

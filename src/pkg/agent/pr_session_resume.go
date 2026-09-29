package agent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// PR follow-up session resume (hivecommons/hive#9583).
//
// A fleet agent is a long-lived CLI in a tmux pane, and deliverKickLocked
// types /clear before every kick (ClearOnKick defaults to true). So when a
// reviewer comments on, or CI fails on, a PR an agent opened, the follow-up
// reaches the author as a FRESH kick that has already forgotten why the PR
// looks the way it does. The two methods below let a caller (the
// pkg/prfollowup router) instead feed the follow-up in as the next turn of
// the SAME conversation, but only while that conversation provably still
// exists: SessionID names it, and SendResumeKick refuses unless the agent is
// still on exactly that session.

// sessionIDBootNonceBytes sizes the per-process nonce that prefixes every
// session ID. launchGen and kickEpoch restart from zero in a new process, so
// without the nonce a pointer saved before a pod restart could match a
// brand-new CLI that has none of the authoring context.
const sessionIDBootNonceBytes = 8

// resumeKickTrigger is the kick-history trigger recorded for a resumed
// follow-up, so an operator reading kick logs can tell it from a cadence kick.
const resumeKickTrigger = "pr-follow-up-resume"

// sessionBootNonce is generated once per process. A var only so tests can
// simulate a restart.
var sessionBootNonce = newSessionBootNonce()

func newSessionBootNonce() string {
	b := make([]byte, sessionIDBootNonceBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; the time fallback
		// still differs across restarts, which is the only property needed.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
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

// sessionIDLocked renders the identity of the conversation the agent's CLI
// currently holds. It changes whenever that conversation could have been
// replaced or cleared: a new process (boot nonce), a relaunch (launchGen), a
// restart teardown (kickEpoch), or any delivered kick (LastKick, which is
// preceded by /clear under ClearOnKick). Callers hold m.mu.
func sessionIDLocked(agent *AgentProcess) (string, bool) {
	if agent == nil || agent.State != StateRunning || agent.LastKick == nil {
		return "", false
	}
	return fmt.Sprintf("%s:%d:%d:%d", sessionBootNonce, agent.launchGen, agent.kickEpoch, agent.LastKick.UnixNano()), true
}

// SessionID returns the identity of the named agent's live CLI conversation,
// or false when it has none (unknown agent, not running, never kicked).
func (m *Manager) SessionID(name string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return sessionIDLocked(m.agents[name])
}

// SendResumeKick delivers message to the named agent as the next turn of the
// session identified by sessionID, WITHOUT the /clear a normal kick types.
//
// It never blocks waiting for the agent and never restarts it: unlike
// SendKick, a crashed CLI or a changed session is a refusal
// (ErrResumeSessionGone), because a restarted CLI has none of the context the
// follow-up is meant to reach. A busy pane is ErrResumeBusy.
func (m *Manager) SendResumeKick(name, message, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("%w: agent %s not found", ErrResumeSessionGone, name)
	}
	if m.agentSandboxEnabledLocked(agent) || agentUsesAgyHeadless(effectiveBackend(agent), agent) {
		return fmt.Errorf("%w: agent %s has no interactive session", ErrResumeUnsupported, name)
	}
	if agent.Paused || agent.State == StatePaused {
		return fmt.Errorf("%w: agent %s is paused", ErrResumeUnsupported, name)
	}
	current, ok := sessionIDLocked(agent)
	if !ok || current != sessionID {
		return fmt.Errorf("%w: agent %s is on a different session", ErrResumeSessionGone, name)
	}
	now := time.Now()
	if remaining := m.providerErrorBackoffRemainingLocked(agent, now); remaining > 0 {
		return fmt.Errorf("%w: agent %s in provider backoff for %v", ErrResumeBusy, name, remaining.Round(time.Second))
	}
	if err := m.restartKickHoldErrLocked(agent, now); err != nil {
		return fmt.Errorf("%w: %v", ErrResumeBusy, err)
	}
	if !m.tmuxSessionExistsForAgent(agent) {
		return fmt.Errorf("%w: tmux session for %s not found", ErrResumeSessionGone, name)
	}
	pane := m.captureVisiblePaneForAgent(agent)
	if !paneHasCLIMarker(pane) || paneShowsConsentScreen(pane) {
		return fmt.Errorf("%w: agent %s CLI is not running", ErrResumeSessionGone, name)
	}
	if !paneShowsInputPrompt(pane) || paneShowsAgentWorking(pane) {
		return fmt.Errorf("%w: agent %s is mid-turn", ErrResumeBusy, name)
	}

	before := agent.LastKick
	agent.resumeSkipClear = true
	m.deliverKickLocked(agent, message, resumeKickTrigger)
	// deliverKickLocked skips silently (logging why) when the pane changed
	// under it; only a recorded delivery moves LastKick.
	if agent.LastKick == before {
		return fmt.Errorf("%w: agent %s did not accept the follow-up", ErrResumeBusy, name)
	}
	return nil
}

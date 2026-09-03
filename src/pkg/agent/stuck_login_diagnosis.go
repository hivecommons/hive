package agent

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// diagnoseStuckLogin explains why an agent is still sitting at a login prompt
// after the token-triggered restart has been tried and has not worked.
//
// It exists because that state is genuinely unreadable from the outside. The
// operator sees an ordinary login menu — byte-identical to the one a
// never-authenticated agent shows — while every file-level check hive performs
// reports success. #4596 burned several rounds of investigation on exactly this
// (including two retractions), and the thing that would have shortened it is a
// single line naming which of the two Claude auth files is the problem.
//
// The message is written for whoever reads the container log or the agent's
// LastError in the dashboard, so it says what was observed, what it means, and
// what will not fix it. It never claims more than it measured: where the
// session state cannot be classified it says so rather than guessing.
func (m *Manager) diagnoseStuckLogin(agent *AgentProcess) string {
	backend := effectiveBackend(agent)
	uid := agent.UID
	home := AgentHome(agent.Name, uid, backend)

	// Only claude splits auth across a token file and a session-state file, so
	// only claude gets the specific diagnosis. Everything else gets an honest
	// generic one rather than a claude-shaped guess.
	if backend != "claude" {
		return fmt.Sprintf(
			"still at a login prompt after %d restarts with a valid shared credential present; "+
				"restarting does not clear it, so the agent needs an interactive login "+
				"(backend %q, HOME %q)",
			tokenRestartMaxAttempts, backend, home)
	}

	// HasUsableToken, matching the gate the heal itself used to get here
	// (configHasTokens): the diagnosis must describe the credential the
	// restarts were attempted against, and a routinely-expired-but-refreshable
	// one is a credential those restarts could legitimately have used.
	credPath := ""
	credValid := false
	for _, p := range agentClaudeCredentialPaths(agent.Name, uid, backend) {
		if claude.HasUsableToken(p) {
			credPath, credValid = p, true
			break
		}
	}

	session := inspectClaudeSession(claudeSessionFile(home))

	switch {
	case credValid && session.State == claudeSessionSkeleton:
		// The #4596 signature. Both halves measured, so this can be stated
		// plainly rather than hedged.
		return fmt.Sprintf(
			"credential at %s is present and valid, but the Claude session state at %s carries no "+
				"signed-in identity (%s missing), so the CLI re-runs onboarding and shows the login menu. "+
				"Restarting cannot fix this — it re-reads the same file. Interactive-auth agents share "+
				"HOME %s while running under per-agent UIDs, and Claude Code rewrites .claude.json "+
				"wholesale, so another agent starting up can overwrite a signed-in state. "+
				"See kubestellar/hive#4596",
			credPath, session.Path, claudeOAuthAccountKey, home)

	case credValid && session.State == claudeSessionUnreadable:
		// Unreadable means unreadable BY THE HIVE PROCESS. Under the
		// per-agent home layout that is the NORMAL state of a healthy
		// signed-in agent — the CLI rewrites its session file agent-owned at
		// 0600, which the hive user cannot read — so nothing about the
		// agent's own view can be concluded from here. An earlier version of
		// this message claimed the CLI itself could not load the identity,
		// and that claim sent a real investigation down a permissions
		// rabbit hole on a file the agent could read fine.
		return fmt.Sprintf(
			"credential at %s is present and valid, but the Claude session state at %s is not readable "+
				"by the hive process (normal when the agent's CLI owns it at 0600), so whether it carries "+
				"a signed-in identity cannot be determined from here — the agent itself may read it fine. "+
				"Check the agent terminal before treating this as a permission problem. "+
				"See kubestellar/hive#4596",
			credPath, session.Path)

	case credValid && session.State == claudeSessionSignedIn:
		// Both files look right, so the cause is elsewhere (a CLI that read
		// them before they were written, an expired-but-parseable token the
		// server rejects, a version that changed the schema). Say exactly
		// that instead of inventing a mechanism.
		return fmt.Sprintf(
			"credential at %s is valid and the session state at %s carries a signed-in identity, yet the "+
				"agent is still at a login prompt after %d restarts. Both files look correct, so the cause "+
				"is not on-disk state; check the CLI's own output in the agent terminal",
			credPath, session.Path, tokenRestartMaxAttempts)

	case credValid:
		return fmt.Sprintf(
			"credential at %s is present and valid, but the Claude session state at %s could not be "+
				"classified (%s). The agent is still at a login prompt after %d restarts",
			credPath, session.Path, session.State, tokenRestartMaxAttempts)

	default:
		// configHasTokens() gated the restart on the SHARED path, which this
		// agent may not resolve to. Worth naming: it is why the restart fired
		// for an agent that has no credential of its own.
		return fmt.Sprintf(
			"no valid credential found for this agent (searched %v) although a shared credential exists; "+
				"the agent needs an interactive login. Session state at %s is %s",
			agentClaudeCredentialPaths(agent.Name, uid, backend), session.Path, session.State)
	}
}

// tokenRestartAction is what the pane poller should do about an agent sitting
// at a login prompt while a valid shared credential exists.
type tokenRestartAction int

const (
	// tokenRestartWait — the cooldown has not elapsed yet. Do nothing.
	tokenRestartWait tokenRestartAction = iota
	// tokenRestartFire — issue a restart. The attempt has already been
	// recorded by decideTokenRestart, so the caller must actually restart.
	tokenRestartFire
	// tokenRestartGiveUp — the restart theory has failed
	// tokenRestartMaxAttempts times; stop restarting and diagnose instead.
	tokenRestartGiveUp
)

// decideTokenRestart records and answers whether to issue another
// token-triggered restart.
//
// Split out of pollTmuxOutputForAgent so the rule can be tested without a tmux
// pane: the loop it guards only reaches this point after a real pane capture,
// which made the previous inline version reachable only in production. The
// accounting lives here (not in the caller) so "decided to fire" and "counted
// an attempt" cannot drift apart — the bug this guards against is precisely an
// attempt that is retried without ever being counted.
//
// Caller holds no lock; these fields belong to this agent's pane poller, the
// same goroutine that owns lastTokenRestart.
func (a *AgentProcess) decideTokenRestart(now time.Time) tokenRestartAction {
	if a.tokenRestartAttempts >= tokenRestartMaxAttempts {
		return tokenRestartGiveUp
	}
	if now.Sub(a.lastTokenRestart).Seconds() < float64(tokenRestartCooldownSec) {
		return tokenRestartWait
	}
	a.tokenRestartAttempts++
	a.lastTokenRestart = now
	return tokenRestartFire
}

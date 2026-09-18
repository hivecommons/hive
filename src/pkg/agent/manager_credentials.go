package agent

// Credential plumbing for pkg/agent — the shared credential/config file
// locations, the Copilot user-token path, reachability probes, the
// definitive-auth-rejection matcher and the Copilot diagnostic run. Split
// verbatim out of manager.go (#7303); token accounting lives in
// manager_tokens.go and the watchers in credentials_watchdog.go / authprobe.go.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

// sharedCopilotConfigPath and sharedClaudeCredentialPath are vars (not consts)
// solely so tests can redirect them to temp files and exercise the config/token
// helpers (copilotConfigHasTokens, clearExpiredTokens, configHasTokens,
// fixSharedConfigPerms) without a real /data volume. Production values are
// unchanged; nothing on the launch path mutates them.
var (
	sharedCopilotConfigPath    = "/data/home/.copilot/config.json"
	sharedClaudeCredentialPath = "/data/home/.claude/.credentials.json"
)

// CopilotUserTokenPath is where the dashboard's device-flow login persists
// the Copilot OAuth token; injected into agents as COPILOT_GITHUB_TOKEN.
const CopilotUserTokenPath = "/data/copilot-user-token"

var copilotUserTokenWatchPath = CopilotUserTokenPath

// copilotUserTokenProbePath is the same location as consulted by the
// AgentAuthState file probe. A var (not the const directly) purely as a TEST
// SEAM, matching sharedCopilotConfigPath above: on a live hive host the real
// /data/copilot-user-token exists, and a probe that cannot be redirected makes
// every "no copilot credentials" test assert against production login state.
var copilotUserTokenProbePath = CopilotUserTokenPath

// claudeCredentialReachable reports whether a usable Claude credential exists
// at the locations this agent's CLI will look — its per-UID home first, then
// the shared path its ~/.claude symlink resolves to.
//
// It is a REACHABILITY check, not a permission check, and the distinction is
// worth stating: this runs in the hive process, so it proves the file is there
// and parseable, not that the agent's UID can open it. The deployment keeps
// those the same — the entrypoint's perm guard holds /data/home/.claude
// group-readable on every write, precisely so every agent UID can read it
// (#4619).
//
// This comment used to end by saying that a drift there would be "a loud,
// alerting state, not a silent one". It was not. The drift happened (#5730): a
// token refresh rewrote the shared credential 0600 as one agent's uid, the
// entrypoint guard that would have reopened it had died silently under `set -e`
// hours earlier, and five of six agents dropped to login prompts while the
// credential watchdog reported an expired login for a credential holding a live
// access token and a valid refresh grant. Nothing in the loop was loud.
//
// What makes it loud NOW is deliberate, and neither part is this function:
// claudeTokenUsable separates "cannot read it" from "it is spent" and reports
// the mode, the owner and the chmod; and the permissions watcher logs at ERROR
// when it finds a shared credential it cannot reopen. This check remains what
// its name says, so read it as one input, not as evidence the agent's uid is
// fine.
//
// HasUsableToken, not HasValidToken: an access token that has aged out is
// exactly the case the CLI fixes for itself on start, by redeeming the refresh
// grant beside it. Treating that state as "no credential here" would re-inject
// the static override precisely when the CLI was about to recover, which is the
// failure this guard exists to prevent.
func claudeCredentialReachable(agent *AgentProcess, backend string) bool {
	if agent == nil {
		return false
	}
	for _, p := range agentClaudeCredentialPaths(agent.Name, agent.UID, backend) {
		if claude.HasUsableToken(p) {
			return true
		}
	}
	return false
}

// Diagnostic pacing. Vars, not consts, so the pkg/agent TestMain can shrink
// them (see the pacing block near deliverStartupKick). Production values
// unchanged.
var (
	diagnosticTimeoutSec = 20
	diagnosticPollSec    = 2
)

// authErrorPatterns indicate the stored token was DEFINITIVELY rejected by the
// server and should be cleared. These are server-side rejections, not CLI
// prompts. A bare interactive login/"re-authenticate" prompt is intentionally
// NOT here: on a slow cold start after an upgrade the Copilot CLI can surface a
// login/device-flow prompt while the token on disk is still valid, and clearing
// it there destroys a good token and forces the user to re-login on every
// upgrade. Only a genuine credential rejection purges the token.
var authErrorPatterns = []string{
	"Bad credentials",
	"401 Unauthorized",
	"token found but could not be validated",
	"Failed to fetch OAuth user login",
}

// matchesAuthError reports whether copilot diagnostic output shows a definitive
// server-side credential rejection that justifies purging the stored token. A
// bare login/"re-authenticate" prompt does NOT match — that is handled by
// paneShowsLoginPrompt (a non-destructive "needs login" UI signal) so a slow
// cold start after an upgrade cannot destroy a still-valid token.
func matchesAuthError(output string) bool {
	for _, pat := range authErrorPatterns {
		if strings.Contains(output, pat) {
			return true
		}
	}
	return false
}

func (m *Manager) runCopilotDiagnostic(ctx context.Context, agent *AgentProcess) {
	m.tmuxSendKeysForAgent(agent, "C-c", "")
	time.Sleep(paneCaptureSleep)
	// Only sweep by UID when isolation gave this agent a real per-agent UID.
	// agent.UID==0 (isolation off or agent missing from the UID map) would
	// otherwise ask killAgentProcesses to match root — the internal floor guard
	// blocks it, but skipping the call makes the intent explicit.
	if agent.UID > 0 {
		killAgentProcesses(agent.UID, m.logger)
	}
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()

	if err := m.ensureTmuxSession(agent); err != nil {
		m.logger.Warn("diagnostic: failed to create tmux session", "agent", agent.Name, "error", err)
		return
	}

	binary, err := backendBinary("copilot")
	if err != nil {
		m.logger.Warn("diagnostic: copilot binary not found", "error", err)
		return
	}
	m.tmuxSendLiteralForAgent(agent, fmt.Sprintf("HOME=/data/home %s", binary))
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	m.logger.Info("diagnostic: launched bare copilot to capture error", "agent", agent.Name)

	deadline := time.After(time.Duration(diagnosticTimeoutSec) * time.Second)
	ticker := time.NewTicker(time.Duration(diagnosticPollSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			m.logger.Warn("diagnostic: timed out waiting for copilot error output", "agent", agent.Name)
			agent.LastError = "copilot hung with no output (diagnostic timed out)"
			agent.State = StateFailed
			// The honest residual class (#5958): we know it did not start and the
			// diagnostic could not say why. It still counts toward the block —
			// an unexplained failure repeating identically is no more fixable by
			// relaunching than a named one.
			m.mu.Lock()
			m.recordStartFailureLocked(agent, "copilot", StartFailureNoOutput, "diagnostic timed out")
			m.mu.Unlock()
			m.audit(AuditAgentStartFailed, agent.Name, auditFields(
				"outcome", "failure",
				"backend", agent.effectiveBackend(),
				"model", agent.effectiveModel(),
				"error", agent.LastError,
			))
			return
		case <-ticker.C:
			output := m.captureTmuxPaneForAgent(agent)
			if output == "" {
				continue
			}
			if matchesAuthError(output) {
				agent.LastError = "auth token expired or invalid"
				// #5958: the reason the fleet page could never show. The token
				// restore/clear below may fix it, so this is recorded but the
				// relaunch at the end of this branch is deliberately still
				// attempted — the backoff paces the NEXT one if it fails again.
				m.mu.Lock()
				m.recordStartFailureLocked(agent, "copilot", StartFailureCredentialRejected, "server rejected the stored token")
				m.mu.Unlock()
				// Prefer to RESTORE the stored token over merely clearing it: an
				// empty copilotTokens leaves CLI 1.0.78 stuck at /login (it does
				// not re-populate from the injected env token), and every roll
				// re-hits this. If we hold a durable user token, seed it so the
				// relaunch below comes up authenticated; otherwise fall back to
				// the historical clear (which lets the CLI reach /login instead
				// of hanging on the MITM proxy on a stale token).
				m.mu.RLock()
				userTok := m.copilotAuthToken
				m.mu.RUnlock()
				if strings.TrimSpace(userTok) != "" {
					m.logger.Warn("diagnostic: auth error detected, restoring token from durable user token",
						"agent", agent.Name, "output_snippet", truncateStr(output, 200))
					if err := restoreCopilotTokens(sharedCopilotConfigPath, userTok); err != nil {
						m.logger.Warn("diagnostic: failed to restore tokens, clearing instead", "error", err)
						_ = clearExpiredTokens()
					}
				} else {
					m.logger.Warn("diagnostic: auth error detected, clearing token (no durable token to restore)",
						"agent", agent.Name, "output_snippet", truncateStr(output, 200))
					if err := clearExpiredTokens(); err != nil {
						m.logger.Warn("diagnostic: failed to clear tokens", "error", err)
					}
				}
			} else if paneShowsCLIReady(strings.Split(output, "\n")) {
				m.logger.Info("diagnostic: copilot started successfully in bare mode", "agent", agent.Name)
				agent.LastError = ""
				// Bare copilot works, so the credential is fine and the fault is
				// in how THIS agent launches. Do not clear the record: the
				// relaunch below is the retry, and if it hangs again the poller
				// will land here once more and the count must survive to reach
				// the block. Clearing on a diagnostic's success rather than the
				// agent's own readiness is what would make the ladder unable to
				// ever reach its threshold.
			} else {
				continue
			}

			if agent.UID > 0 {
				killAgentProcesses(agent.UID, m.logger)
			}
			_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
			agent.forceRelaunch = true
			if err := m.RestartWithReason(ctx, agent.Name, "hung with no CLI prompt"); err != nil {
				m.logger.Warn("diagnostic: restart failed", "agent", agent.Name, "error", err)
			}
			return
		}
	}
}

// fixSharedConfigPerms ensures /data/home/.copilot/config.json is group-readable
// before launching an agent. Copilot CLI rewrites this file with 600 perms on
// token refresh, locking out other agent UIDs that share the same HOME.
func (m *Manager) fixSharedConfigPerms(agent *AgentProcess) {
	info, err := os.Stat(sharedCopilotConfigPath)
	if err != nil {
		return
	}
	if info.Mode().Perm() == sharedConfigDesiredMode {
		return
	}
	m.logger.Warn("fixing shared config.json perms before launch",
		"agent", agent.Name,
		"was", fmt.Sprintf("%04o", info.Mode().Perm()),
		"fix", fmt.Sprintf("%04o", sharedConfigDesiredMode))
	if err := os.Chmod(sharedCopilotConfigPath, sharedConfigDesiredMode); err != nil {
		m.logger.Warn("failed to fix config.json perms", "error", err)
	}
}

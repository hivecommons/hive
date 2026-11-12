package agent

import (
	"context"
	"errors"
	"net"
	"net/url"

	"github.com/hivecommons/hive/crux"
)

// mintAgentTokenUnlocked mints the per-agent tier-scoped GitHub App token
// (WriteAgentToken) and the opt-in short-lived mint token for the agent. It is
// the single mint step every (re)launch path — Start, Resume, Restart,
// RestartWithBootstrap — runs before launchInTmux (#3962: previously only
// Start minted, so an agent relaunched via Resume/Restart never got a token
// until the whole process restarted; its cache file stayed 0 bytes and gh/git
// push failed silently).
//
// MUST be called with m.mu RELEASED. WriteAgentToken makes an outbound GitHub
// API call (through the MITM egress proxy, which the production incident
// showed can hang), and issueAgentMintToken can do the same. Holding m.mu
// across a hang stalls AllStatuses()/the heartbeat for the WHOLE fleet — the
// exact liveness flap Start's phase split was built to avoid. The function
// reads only immutable agent identity (Name, UID) and Config via agentMode,
// and writes no m.mu-guarded AgentProcess field, so running it lock-free is
// race-free. m.appAuth is read without the lock exactly as Start's unlocked
// phase always did: it is injected once at wiring time, before agents start.
func (m *Manager) mintAgentTokenUnlocked(ctx context.Context, agent *AgentProcess) {
	if m.appAuth == nil || !m.agentTokenCacheEnabled(agent) {
		return
	}
	tier := m.agentTokenTier(agent)
	if err := m.appAuth.WriteAgentToken(ctx, agent.Name, tier, agent.UID); err != nil && errors.Is(err, crux.ErrGitHubAppNotInstalled) {
		// Static misconfiguration: every tick would fail identically, so log
		// once per agent until a mint succeeds instead of every supervisor tick.
		if _, seen := m.appNotInstalledWarned.LoadOrStore(agent.Name, struct{}{}); !seen {
			m.logger.Warn("github app installation id is unset — per-agent scoped token NOT delivered, this agent's GitHub writes (gh + git push) will FAIL; set github.installation_id or install the App on the hive's org so discovery resolves it",
				"agent", agent.Name, "tier", tier, "error", err)
		}
		m.observeCondition(crux.ClassGitHubAppNotInstalled, agent.Name, "github app installation id is unset: "+err.Error(), "")
	} else if err != nil && isForgeTransportErr(err) {
		// The GitHub host did not answer (dial timeout, connection refused,
		// deadline exceeded). Nothing about the App key or installation can
		// be concluded from that, so report it as a forge-reachability
		// condition rather than a mint failure — the latter tells the
		// operator to re-check App permissions, which is wrong during an
		// egress outage. The mint-failed condition is cleared so a hive that
		// slid from a real mint error into an outage does not hold both open.
		m.logger.Warn("per-agent scoped token NOT delivered — GitHub host unreachable from the pod; this agent's GitHub writes (gh + git push) will FAIL until egress is restored",
			"agent", agent.Name, "tier", tier, "error", err)
		m.clearCondition(crux.ClassGitHubAppTokenMintFailed, agent.Name)
		m.observeCondition(crux.ClassForgeUnreachable, agent.Name, "github host unreachable during token mint: "+err.Error(), "")
	} else if err != nil {
		// Be precise about the blast radius. Since audit H3 the shared-cache
		// fallback is GONE: gh-wrapper.sh and git-credential-hive.sh no
		// longer fall back to /var/run/hive-metrics/gh-app-token.cache (the
		// FULL installation token, now owner-only 0600). They FAIL LOUD when
		// the per-agent scoped token is absent rather than silently
		// escalating this agent to full privilege. So a delivery failure here
		// means this agent's GitHub writes (gh + git push) will fail until
		// token delivery is repaired — not a silent privilege escalation.
		m.logger.Warn("per-agent scoped token NOT delivered — this agent's GitHub writes (gh + git push) will FAIL (no shared-cache fallback by design; see audit H3)",
			"agent", agent.Name, "tier", tier, "error", err)
		m.clearCondition(crux.ClassForgeUnreachable, agent.Name)
		m.observeCondition(crux.ClassGitHubAppTokenMintFailed, agent.Name, "github app token mint failed: "+err.Error(), "")
	} else {
		m.appNotInstalledWarned.Delete(agent.Name)
		m.clearCondition(crux.ClassGitHubAppNotInstalled, agent.Name)
		m.clearCondition(crux.ClassGitHubAppTokenMintFailed, agent.Name)
		m.clearCondition(crux.ClassForgeUnreachable, agent.Name)
	}
	// Additionally issue an opt-in short-lived mint token (no-op when the mint
	// is disabled). This is additive and fail-safe — it never blocks launch.
	if agent.UID > 0 {
		m.issueAgentMintToken(agent.Name, tier, agent.UID)
	}
}

// isForgeTransportErr reports whether a mint error never got an answer from
// the GitHub host: a deadline, a *url.Error (dial/TLS/timeout from the HTTP
// client) or any net.Error. An HTTP status from GitHub (401/404/422) is not
// a transport error and keeps its mint-failed classification. Mirrors the
// dashboard's forge probe (#17427) so both report the same outage.
func isForgeTransportErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

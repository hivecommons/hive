// Agent mode, capabilities, and authorization: mode files, capability
// resolution, PR/issue/merge authorization, and invocation metadata.
package agent

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ClearAllModeOverrides clears the per-agent Config.Mode for all agents so that
// DefaultAgentMode determines the mode based on the ACMM level. This should be
// called before SyncModeFiles when switching levels, because Config.Mode may
// have been set by the initial config or a previous pack and would otherwise
// override the new level's expected default.
func (m *Manager) ClearAllModeOverrides() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, agent := range m.agents {
		agent.Config.Mode = ""
	}
}

// SyncModeFiles rewrites /tmp/.hive-mode-* for all running agents to reflect the given ACMM level.
func (m *Manager) SyncModeFiles(level int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for name, agent := range m.agents {
		if agent.Paused {
			continue
		}
		mode := DefaultAgentMode(name, level)
		if modeStr := agent.Config.Mode; modeStr != "" {
			if parsed, ok := ParseAgentMode(modeStr); ok {
				m.logger.Info("SyncModeFiles: Config.Mode override",
					"agent", name, "level", level,
					"default", DefaultAgentMode(name, level).String(),
					"override", modeStr)
				mode = parsed
			}
		}
		modeFile := filepath.Join(agentStateDir, ".hive-mode-"+name)
		if err := writeAgentStateFile(modeFile, []byte(mode.String())); err != nil {
			m.logger.Warn("SyncModeFiles: write failed", "file", modeFile, "error", err)
		}
		// The capability file rides the same sync (#4492). It is level-independent
		// today, but writing it here is what makes a `converse` change take effect
		// on the next reconcile instead of only at the next agent launch.
		caps := DefaultCapabilities(mode, level)
		if agent.Config.Converse != nil {
			caps.Converse = *agent.Config.Converse
		}
		m.writeAgentCapsFile(name, caps)
	}
}

// agentCapabilities returns the ORTHOGONAL capabilities for a given agent
// (#4492). Unlike agentMode there is no per-level default table: `converse` is
// opt-in everywhere, so an agent whose config says nothing gets the zero value
// and behaves exactly as it did before capabilities existed.
func (m *Manager) agentCapabilities(agent *AgentProcess) AgentCapabilities {
	caps := DefaultCapabilities(m.agentMode(agent), m.project.ACMMLevel)
	if agent.Config.Converse != nil {
		caps.Converse = *agent.Config.Converse
	}
	return caps
}

// writeAgentCapsFile persists the capability set the proxy reads on the request
// path. It is written for EVERY agent, including those with no capabilities, so
// a cleared `converse` actually revokes: leaving a stale file behind would keep
// granting the capability after the operator turned it off.
func (m *Manager) writeAgentCapsFile(name string, caps AgentCapabilities) {
	capsFile := filepath.Join(agentStateDir, ".hive-caps-"+name)
	if err := writeAgentStateFile(capsFile, []byte(caps.String())); err != nil {
		m.logger.Warn("caps file write failed", "file", capsFile, "error", err)
	}
}

// agentMode returns the GitHub interaction mode for a given agent at the current ACMM level.
// If the agent has an explicit Mode in its config (hive.yaml or pack YAML), that takes precedence.
// Otherwise, the default table by ACMM level is used.
func (m *Manager) agentMode(agent *AgentProcess) AgentMode {
	if modeStr := agent.Config.Mode; modeStr != "" {
		if parsed, ok := ParseAgentMode(modeStr); ok {
			return parsed
		}
	}
	return DefaultAgentMode(agent.Name, m.project.ACMMLevel)
}

// DefaultAgentMode returns the default mode for a given agent name and ACMM level,
// ignoring any hive.yaml override. Used by the dashboard to show "(default)" indicators.
func DefaultAgentMode(agentName string, level int) AgentMode {
	if agentName == "supervisor" {
		return ModeAdvisory
	}
	switch level {
	case 1:
		return ModeAdvisory
	case 2:
		return ModeAdvisory
	case 3:
		if agentName == "quality" {
			return ModeIssuesAndPRs
		}
		return ModeAdvisory
	case 4:
		switch agentName {
		case "quality", "sec-check", "ci-maintainer":
			return ModeIssuesAndPRs
		case "scanner", "guide":
			return ModeIssuesOnly
		default:
			return ModeAdvisory
		}
	case 5:
		return ModeIssuesAndPRs
	case 6:
		if agentName == "scanner" {
			return ModeIssuesPRsMerge
		}
		return ModeIssuesAndPRs
	default:
		return ModeAdvisory
	}
}

// AuthorizePROpen enforces the policy for the hive-opens-PR watcher: an agent
// may open a PR (by dropping a request file) only if BOTH hold:
//
//  1. Forge-resistance — the request file's owning UID (fileUID) maps to the
//     agent it claims to be (via the uid-map). One agent cannot open a PR "as"
//     another, and a non-agent process (unknown UID) is refused. When per-agent
//     UIDs are not in play (fileUID <= 0, e.g. shared-dev-UID mode with no map),
//     ownership is unverifiable, so we fall back to the ACMM check alone rather
//     than hard-failing — the same posture the credential helper takes.
//  2. ACMM write-gate — the agent must be push-capable at the hive's current
//     ACMM level, i.e. exactly the CanPush() check that governs `gh pr create`.
//
// Returns nil to authorize, or an error describing the denial. This mirrors the
// direct PR path's policy so the request-file route grants no extra privilege.
func (m *Manager) AuthorizePROpen(agentName string, fileUID int) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	// Forge check: when we have a UID map and a real owning UID, the file owner
	// must BE this agent.
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	// ACMM write-gate: resolve the agent and check CanPush.
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if !m.agentMode(agent).CanPush() {
		return fmt.Errorf("agent %q is not push-capable at this ACMM level (mode %s) — advisory agents may not open PRs",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

// AuthorizeIssueOpen enforces the policy for the issue-request watcher,
// mirroring AuthorizePROpen with the mode gates that govern the direct gh
// paths: "issue" requests need CanCreateIssues() (mode >= ISSUES_ONLY);
// "comment" and "claim" requests need the same (commenting and claiming an
// issue are both issue-writes under the same tier). The same UID
// forge-resistance applies: the request file's owner must BE the claimed
// agent. A nil manager or unknown agent is denied.
func (m *Manager) AuthorizeIssueOpen(agentName string, fileUID int, kind string) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if !m.agentMode(agent).CanCreateIssues() {
		return fmt.Errorf("agent %q may not create issues or comments at this ACMM level (mode %s)",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

// AuthorizeMerge enforces the policy for the hive-merges-PR watcher, mirroring
// AuthorizePROpen but with the stricter CanMerge() gate: the request's agent
// must own the request file (forge-resistance) AND be merge-capable at the
// hive's current ACMM level (ModeIssuesPRsMerge). This keeps the file-based
// merge relay under the exact same authority as a direct merge would require —
// an issues/PRs agent that can open PRs still cannot merge them unless its mode
// grants merge. A nil manager or unknown agent is denied.
func (m *Manager) AuthorizeMerge(agentName string, fileUID int) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	// Forge check: when we have a UID map and a real owning UID, the file owner
	// must BE this agent.
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	// ACMM merge-gate: resolve the agent and check CanMerge.
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if !m.agentMode(agent).CanMerge() {
		return fmt.Errorf("agent %q is not merge-capable at this ACMM level (mode %s) — only ISSUES_PRS_MERGE agents may merge PRs",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

// AgentCapabilities reports whether the named agent is ABLE — at the hive's
// current ACMM level and the agent's effective mode — to create issues, open
// PRs, and merge PRs. These are the EXACT gates AuthorizePROpen (CanPush) and
// AuthorizeMerge (CanMerge) enforce, so a hub capability badge derived from
// these can never claim a capability the merge/PR relay would actually refuse.
// ok=false when the agent is unknown to the manager (the caller then reports
// "unknown", not a false negative). Read-only under RLock.
func (m *Manager) AgentCapabilities(agentName string) (canOpenIssue, canOpenPR, canMerge, ok bool) {
	m.mu.RLock()
	agent, exists := m.agents[agentName]
	m.mu.RUnlock()
	if !exists || agent == nil {
		return false, false, false, false
	}
	mode := m.agentMode(agent)
	return mode.CanCreateIssues(), mode.CanPush(), mode.CanMerge(), true
}

// EffectiveBackend reports the named agent's effective backend, honoring any
// runtime BackendOverride (see effectiveBackend). ok=false when the agent is
// unknown. Read-only under RLock — a small exported wrapper so callers outside
// the package (the heartbeat builder) need not reach into unexported state.
func (m *Manager) EffectiveBackend(agentName string) (backend string, ok bool) {
	m.mu.RLock()
	agent, exists := m.agents[agentName]
	m.mu.RUnlock()
	if !exists || agent == nil {
		return "", false
	}
	return effectiveBackend(agent), true
}

// InvocationMetadata reports the effective backend, model, and reasoning effort
// the hive invokes for the named agent, accounting for runtime overrides — the
// launch-time truth the invocation-attribution trail records (see pkg/github/attribution
// .go). ok=false when the agent is unknown to the manager (the caller then
// falls back to static config). Read-only under RLock; called from the
// PR-request watcher goroutine, never from the launch path.
func (m *Manager) InvocationMetadata(agentName string) (backend, model, effort string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, exists := m.agents[agentName]
	if !exists {
		return "", "", "", false
	}
	backend = effectiveBackend(agent)
	model = agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	return backend, model, ResolveReasoningEffort(backend, model), true
}

// ResolveReasoningEffort reports the reasoning effort the hive actually launches
// a given backend/model pair with. Exported because the attribution trail is
// resolved in TWO places — Manager.InvocationMetadata above for a running agent,
// and cmd/hive's fallback that reads straight from config when the Manager does
// not know the agent — and both must give the same answer.
//
// Before this existed the fallback carried its own hardcoded "low", so changing
// agyDefaultEffort here would have left cmd/hive silently stamping PRs with an
// effort agy was no longer being launched with. An attribution trail that
// misreports is worse than one that says nothing.
//
// agy is the only backend with a rule: it REQUIRES --effort whenever --model is
// given (without it agy ignores the model outright), and it is given no --effort
// at all when no model is set. Every other backend takes its effort from its own
// config, which the hive does not resolve here, so the honest answer is "".
func ResolveReasoningEffort(backend, model string) string {
	if backend == "agy" && model != "" {
		return agyDefaultEffort
	}
	return ""
}

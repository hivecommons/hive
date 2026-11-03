package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
)

func (m *Manager) markProviderErrorLocked(agent *AgentProcess, match providerErrorMatch, now time.Time) time.Duration {
	if status, ok := classifyBackendAuthStatus(match.Class, match.Line); ok {
		agent.markBackendAuthLocked(status, match.Line, now)
		// #6767: an unlicensed verdict against a copilot agent is direct
		// upstream evidence that whatever Copilot token this agent is
		// actually using has no license. If the hive currently treats a
		// token as AUTHORITATIVE, that same token is the one being pinned
		// into every agent's environment (COPILOT_GITHUB_TOKEN) and into
		// the shared CLI config, so the "not licensed" verdict IS a verdict
		// on the authoritative token. Latch that here so syncCopilotToken
		// stops clobbering an operator's recovery /login with the known-bad
		// authoritative token. Cleared as soon as setCopilotToken installs
		// a different value (recovery succeeded, or dashboard re-login).
		if status == BackendAuthUnlicensed && agent.Config.Backend == "copilot" && m.copilotAuthTokenAuthoritative {
			m.copilotAuthTokenRejected = true
		}
	}
	if !agent.ProviderErrorBackoffUntil.IsZero() && now.Before(agent.ProviderErrorBackoffUntil) &&
		agent.ProviderErrorClass == match.Class && agent.ProviderErrorLine == match.Line {
		return agent.ProviderErrorBackoffUntil.Sub(now)
	}
	agent.providerErrorBackoffAttempt++
	delay := providerErrorBackoffDelay(agent.providerErrorBackoffAttempt)
	agent.ProviderErrorBackoffUntil = now.Add(delay)
	agent.ProviderErrorClass = match.Class
	agent.ProviderErrorLine = match.Line
	agent.LastError = match.Line
	agent.lastInferKickPane = ""
	agent.actionNudgeSent = false
	return delay
}

func (m *Manager) clearProviderErrorLocked(agent *AgentProcess, now time.Time) {
	if agent.ProviderErrorClass == "" && agent.ProviderErrorLine == "" && agent.ProviderErrorBackoffUntil.IsZero() {
		return
	}
	if agent.LastError == agent.ProviderErrorLine {
		agent.LastError = ""
	}
	agent.ProviderErrorClass = ""
	agent.ProviderErrorLine = ""
	agent.ProviderErrorBackoffUntil = time.Time{}
	agent.providerErrorBackoffAttempt = 0
	agent.clearBackendAuthLocked(now)
}

func (m *Manager) providerErrorBackoffRemainingLocked(agent *AgentProcess, now time.Time) time.Duration {
	if agent == nil || agent.ProviderErrorBackoffUntil.IsZero() || !now.Before(agent.ProviderErrorBackoffUntil) {
		return 0
	}
	return agent.ProviderErrorBackoffUntil.Sub(now)
}

// ProviderErrorBackoffRemaining reports the active inference-provider backoff
// for a dashboard/governor caller that wants to avoid even attempting a kick.
func (m *Manager) ProviderErrorBackoffRemaining(name string) (time.Duration, string, string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, ok := m.agents[name]
	if !ok {
		return 0, "", "", false
	}
	remaining := m.providerErrorBackoffRemainingLocked(agent, time.Now())
	if remaining <= 0 {
		return 0, agent.ProviderErrorClass, agent.ProviderErrorLine, false
	}
	return remaining, agent.ProviderErrorClass, agent.ProviderErrorLine, true
}

// GetStatusFast returns an agent snapshot without ever waiting on the global
// manager lock.
//
// m.mu is a SINGLE GLOBAL lock, and restartWithReason holds it in write mode
// across the entire tmux relaunch — kill-session, ensureTmuxSession, the
// caveman install, send-keys and their sleeps. Measured on a live spoke that
// is an 11.25s hold, and three restarts can land inside 30s. Any handler whose
// first action is GetStatus therefore stalls for the length of somebody else's
// restart, which is why every agent settings dialog sat on "Loading..." while
// one unrelated agent was relaunching (#7417). Agents do not have independent
// locks: restarting one blocks reads for all of them.
//
// So: try the lock, and if a writer holds it (or is waiting — Go's RWMutex
// makes TryRLock fail for a pending writer, which is exactly what we want),
// fall back to the last snapshot instead of blocking. Callers that only need
// display fields get a value that is at worst one restart stale, which beats a
// 12-second spinner. If no snapshot has been taken yet this returns an error,
// and every current caller already degrades to its configured values on error.
//
// This does NOT fix the lock hold itself — restartWithReason still needs to be
// phased so the relaunch happens outside m.mu. It stops that hold from being
// visible to readers who never needed to be serialized against it.
func (m *Manager) GetStatusFast(name string) (*crux.Process, error) {
	if m.mu.TryRLock() {
		agent, ok := m.agents[name]
		if !ok {
			m.mu.RUnlock()
			return nil, fmt.Errorf("agent %s not found", name)
		}
		snap := agent.snapshot()
		m.mu.RUnlock()

		m.statusSnapMu.Lock()
		if m.statusSnaps == nil {
			m.statusSnaps = make(map[string]*crux.Process)
		}
		// Cache the same pointer we return. Snapshots are read-only
		// display values by contract; a later refresh replaces the map
		// entry rather than mutating this one.
		m.statusSnaps[name] = &snap
		m.statusSnapMu.Unlock()

		return &snap, nil
	}

	m.statusSnapMu.RLock()
	cached, ok := m.statusSnaps[name]
	m.statusSnapMu.RUnlock()
	if ok && cached != nil {
		// Returned as-is (no copy); stale snapshots are read-only.
		return cached, nil
	}
	return nil, fmt.Errorf("agent %s status unavailable: manager busy", name)
}

func (m *Manager) GetStatus(name string) (*crux.Process, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}
	snap := agent.snapshot()
	return &snap, nil
}

// CountAgentsWithModel returns how many agents have an effective method
// (backend) or model assigned, resolving overrides ahead of config exactly as
// the launcher does. Reported to the hub so it can tell whether this hive has
// completed the "assign a method/model to an agent" adoption step.
//
// An agent counts if EITHER a backend or a model is set: "claude with the
// default model" and "the governor's default backend pinned to a specific
// model" are both real assignments. Values like "auto" and "default" are
// deliberate routing selections, not absences, so they count too — only a
// wholly empty backend AND model reads as unassigned.
func (m *Manager) CountAgentsWithModel() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	count := 0
	for _, a := range m.agents {
		if a == nil {
			continue
		}
		backend := a.Config.Backend
		if a.BackendOverride != "" {
			backend = a.BackendOverride
		}
		model := a.Config.Model
		if a.ModelOverride != "" {
			model = a.ModelOverride
		} else if a.PinnedModel != "" {
			model = a.PinnedModel
		}
		if strings.TrimSpace(backend) != "" || strings.TrimSpace(model) != "" {
			count++
		}
	}
	return count
}

// AllStatuses snapshots every agent. It reads only in-memory state: the
// Copilot transcript liveness it reports is refreshed by
// StartTranscriptActivityRefresh on its own goroutine, never walked here,
// so callers on a deadline (the hub heartbeat collect, the dashboard status
// build) cannot be stalled by a slow /data.
func (m *Manager) AllStatuses() map[string]*crux.Process {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	result := make(map[string]*crux.Process, len(m.agents))
	for k, v := range m.agents {
		m.refreshBusyVisibilityLocked(v, now)
		snap := v.snapshot()
		result[k] = &snap
	}
	return result
}

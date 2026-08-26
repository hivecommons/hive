package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/watchdog"
)

// This file adapts the Manager to the watchdog reconciler (RFC #4665). The
// adapter is intentionally thin: every observation reuses the manager's
// existing machinery — the visible-pane capture (same seam the pane poller
// uses), the cliPaneMarkers ready-signature tables, paneShowsLoginPrompt, the
// owner-aware per-agent auth probe (AgentAuthState, #4619/#4641), and the
// Restart/Pause paths — so the watchdog is a consumer of the manager's truth,
// never a second source of it.

// WatchdogFleet implements watchdog.Fleet over a Manager.
type WatchdogFleet struct {
	M *Manager
}

// AgentNames lists agents the watchdog should reconcile: agents the manager
// BELIEVES are running. That scoping is deliberate — the RFC's target is
// "dead while the dashboard reports running". Agents that are stopped,
// failed, or never launched already belong to the existing crash-recovery
// loop (CheckAndRestartCrashedAgents) and the #4606 token-restart path;
// reconciling them here would create the second uncoordinated restart loop
// the RFC warns against.
//
// Two further classes are excluded for the same reason the stall checks and
// the fleet breaker exclude them (manager.go: inference stall sweep,
// EngageBreaker), because for them a quiet pane is correct behavior rather
// than a fault — the house rule that a facet lighting up constantly gets
// ignored (hub/agent_inactivity.go):
//
//   - OnDemand agents are MEANT to sit idle until summoned. Their pane
//     legitimately shows no activity for days; classifying that as no-output
//     and restarting it would fight the feature.
//   - Sandboxed agents do not run a persistent tmux CLI session at all —
//     execution is per-kick in a container. Every sweep would observe
//     no-session, read it as dead, and restart an agent that was never
//     supposed to hold a pane.
func (f WatchdogFleet) AgentNames() []string {
	f.M.mu.RLock()
	defer f.M.mu.RUnlock()
	names := make([]string, 0, len(f.M.agents))
	for name, a := range f.M.agents {
		if a == nil || a.State != StateRunning || !a.HasLaunched {
			continue
		}
		if a.Config.OnDemand {
			continue
		}
		if f.M.agentSandboxEnabledLocked(a) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// Observe gathers one agent's observed truth using the manager's existing
// capture and classification helpers.
func (f WatchdogFleet) Observe(name string) (watchdog.Observation, error) {
	f.M.mu.RLock()
	agent, ok := f.M.agents[name]
	f.M.mu.RUnlock()
	if !ok {
		return watchdog.Observation{}, fmt.Errorf("agent %s not found", name)
	}

	// Pane capture and session probe run OUTSIDE m.mu: they shell out to
	// tmux and must never block AllStatuses()/the heartbeat.
	sessionExists := f.M.tmuxSessionExistsForAgent(agent)
	pane := ""
	if sessionExists {
		// Visible pane only — stale ready markers in scrollback must not
		// vouch for a dead CLI (same rule as tmuxPaneHasCLIForAgent).
		pane = f.M.captureVisiblePaneForAgent(agent)
	}

	agent.paneMu.RLock()
	needsLogin := agent.NeedsLogin
	lastChange := agent.LastPaneChange
	agent.paneMu.RUnlock()

	backend := effectiveBackend(agent)
	running := agent.State == StateRunning
	authAvailable, authKnown := f.M.AgentAuthState(name, agent.UID, backend, running, needsLogin)

	return watchdog.Observation{
		Backend:          backend,
		SessionExists:    sessionExists,
		Pane:             pane,
		HasCLIMarker:     paneHasCLIMarker(pane),
		ShowsLoginPrompt: needsLogin || paneShowsLoginPrompt(strings.Split(pane, "\n")),
		LastChange:       lastChange,
		AuthAvailable:    authAvailable,
		AuthKnown:        authKnown,
	}, nil
}

// IsPaused reports the operator/escalation pause state.
func (f WatchdogFleet) IsPaused(name string) bool {
	f.M.mu.RLock()
	defer f.M.mu.RUnlock()
	agent, ok := f.M.agents[name]
	return ok && agent.Paused
}

// Restart delegates to the manager's session restart — the same path the
// dashboard restart button and the token-restart recovery use, so restart
// accounting (RestartCount) stays in one place.
func (f WatchdogFleet) Restart(ctx context.Context, name string) error {
	return f.M.Restart(ctx, name)
}

// Pause delegates to the manager's system pause (no human actor).
func (f WatchdogFleet) Pause(name, trigger, reason string) error {
	return f.M.Pause(name, trigger, reason)
}

// SetConditions publishes the watchdog's condition set onto the agent so the
// dashboard payload carries observed truth beside the state echo.
func (f WatchdogFleet) SetConditions(name string, conds []watchdog.Condition) {
	f.M.mu.RLock()
	agent, ok := f.M.agents[name]
	f.M.mu.RUnlock()
	if !ok {
		return
	}
	agent.paneMu.Lock()
	agent.WatchdogConditions = conds
	agent.paneMu.Unlock()
}

// productionEvidenceDirs names the per-backend state/conversation directories
// (relative to the agent's HOME) whose file mtimes are production evidence.
// v1 evidence sources are state-file mtimes plus pane activity; issue/PR
// mutation evidence is a documented follow-up (see RFC #4665).
var productionEvidenceDirs = map[string][]string{
	"claude":  {".claude/projects", ".claude"},
	"codex":   {".codex/sessions", ".codex"},
	"agy":     {".antigravity-cli", ".gemini"},
	"gemini":  {".gemini/tmp", ".gemini"},
	"copilot": {".copilot"},
}

const (
	// productionScanMaxEntries bounds the evidence walk so a pathological
	// state dir can never stall a watchdog sweep.
	productionScanMaxEntries = 512
	// productionScanMaxDepth bounds the walk depth for the same reason.
	productionScanMaxDepth = 3
)

// LastProduction returns the newest production evidence for the agent: the
// most recent of (a) state/conversation file mtimes under the agent's own
// HOME (owner-aware — stat needs no read permission, so #4668's 0600 files
// still date correctly) and (b) the last observed pane change. ok=false means
// no evidence source exists for this backend — reported honestly as unknown
// by the reconciler, never as healthy.
func (f WatchdogFleet) LastProduction(name string) (time.Time, bool) {
	f.M.mu.RLock()
	agent, ok := f.M.agents[name]
	f.M.mu.RUnlock()
	if !ok {
		return time.Time{}, false
	}

	agent.paneMu.RLock()
	newest := agent.LastPaneChange
	agent.paneMu.RUnlock()
	found := !newest.IsZero()

	backend := effectiveBackend(agent)
	home := AgentHome(name, agent.UID, backend)
	for _, rel := range productionEvidenceDirs[backend] {
		if t, ok := newestMtime(filepath.Join(home, rel)); ok {
			found = true
			if t.After(newest) {
				newest = t
			}
		}
	}
	return newest, found
}

// newestMtime returns the newest file mtime under root, bounded by
// productionScanMaxEntries/productionScanMaxDepth.
func newestMtime(root string) (time.Time, bool) {
	var newest time.Time
	entries := 0
	rootDepth := strings.Count(root, string(os.PathSeparator))
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable entries are skipped, not fatal: evidence gathering
			// is best-effort and partial evidence is still evidence.
			return nil
		}
		entries++
		if entries > productionScanMaxEntries {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if strings.Count(path, string(os.PathSeparator))-rootDepth >= productionScanMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return newest, !newest.IsZero()
}

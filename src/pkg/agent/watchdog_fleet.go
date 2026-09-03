package agent

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/watchdog"
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
	// Queued reports the hive's actionable work depth. Nil means the queue
	// cannot be read from here, which the reconciler reports as Unknown
	// rather than assuming an empty queue (silencing real faults) or a full
	// one (manufacturing them). Injected because queue depth lives in the
	// governor, which pkg/agent does not import.
	Queued func() (int, bool)
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
	// Positive-evidence-only probe (#5291), deliberately separate from
	// AgentAuthState above: it answers "is this backend demonstrably able to
	// authenticate?" without letting the pane's own login chrome outrank the
	// credential. The reconciler needs that unclouded answer to tell a
	// credential a restart can fix from one only a human can.
	//
	// CLAUDE ONLY, and the restriction is the point. credentialFileProves
	// verifies an EXPIRY only for claude; it answers copilot and codex by the
	// PRESENCE of a token file, and presence is not proof of usability. The
	// reconciler uses this to decide whether to page an operator, so a
	// stale-but-present copilot token reading as "proven" would silence the
	// alert that is the only thing telling a human their fleet is logged out.
	// #5291's login detector can live with presence-only evidence because
	// suppressing a PAUSE hands the pane to the restart heal; suppressing a
	// PAGE hands it to nobody. Every non-claude backend therefore keeps its
	// pre-existing behaviour exactly.
	credentialProven := backend == "claude" && f.M.AgentHasValidCredential(name)

	// StartedAt dates the current launch so the reconciler can suppress dead
	// verdicts during boot. Copied by value: the field is a pointer the
	// manager mutates on relaunch.
	var startedAt time.Time
	f.M.mu.RLock()
	if agent.StartedAt != nil {
		startedAt = *agent.StartedAt
	}
	f.M.mu.RUnlock()

	return watchdog.Observation{
		Backend:          backend,
		SessionExists:    sessionExists,
		Pane:             pane,
		HasCLIMarker:     paneHasCLIMarker(pane),
		ShowsLoginPrompt: needsLogin || paneShowsLoginPrompt(strings.Split(pane, "\n")),
		LastChange:       lastChange,
		StartedAt:        startedAt,
		AuthAvailable:    authAvailable,
		AuthKnown:        authKnown,
		CredentialProven: credentialProven,
	}, nil
}

// QueuedWork reports the hive's actionable work depth for the readiness gate.
// Advisory-tier agents are reported as having no queue: they cannot drain
// issues or PRs by design, so counting the backlog against them would turn a
// healthy advisory stream into "idle with work queued" — the same carve-out
// the hub makes for write-incapable agents (agent_inactivity.go).
func (f WatchdogFleet) QueuedWork(name string) (int, bool) {
	if f.Queued == nil {
		return 0, false
	}
	f.M.mu.RLock()
	agent, ok := f.M.agents[name]
	advisory := ok && agentIsAdvisoryOnly(agent)
	f.M.mu.RUnlock()
	if !ok {
		return 0, false
	}
	if advisory {
		return 0, true
	}
	return f.Queued()
}

// agentIsAdvisoryOnly reports whether an agent is barred from opening issues
// or PRs, and therefore cannot drain the queue no matter how deep it gets.
// Both the explicit mode and the tools-derived mode are checked, because an
// agent can reach the advisory tier either way.
func agentIsAdvisoryOnly(agent *AgentProcess) bool {
	const advisoryMode = "ADVISORY"
	if strings.EqualFold(strings.TrimSpace(agent.Config.Mode), advisoryMode) {
		return true
	}
	return agent.Config.Tools.EffectiveMode() == advisoryMode
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

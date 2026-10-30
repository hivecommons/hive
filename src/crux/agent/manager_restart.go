package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var errMissingSessionAlreadyAlive = errors.New("missing-session recovery found an existing tmux session")

// inferencePaneCheck pairs an inference agent name with its captured visible
// pane for post-kick stall inspection outside the manager read lock.
type inferencePaneCheck struct {
	name string
	pane string
}

// deadSessionRecoveryOwner renders the owner for the log line, so an operator
// reading "agent tmux session missing" can tell at a glance whether this loop
// is about to restart it or whether the watchdog's bounded ladder has it.
func deadSessionRecoveryOwner(ownedElsewhere bool) string {
	if ownedElsewhere {
		return "watchdog"
	}
	return "crash-loop"
}

// SetDeadSessionRecoveryOwner declares whether some other component owns
// restarting agents whose tmux session is missing or whose pane has gone bare
// (RFC #4665). When owned elsewhere, CheckAndRestartCrashedAgents observes and
// logs those two conditions but does not restart them.
//
// The transfer exists because this loop restarts on every eval tick with no
// backoff and no cap, while the watchdog counts the same failures toward a
// bounded ladder and a crash-loop pause. Both running meant the ladder
// throttled nothing: the manager re-restarted an unfixable agent at tick rate
// while the watchdog merely counted. One owner, one bounded ladder.
//
// Only those two conditions move. Consent-screen dismissal and the inference
// stall nudge are NOT restarts, are not conditions the watchdog classifies,
// and keep running here unconditionally — including for agents whose restart
// this loop no longer performs.
func (m *Manager) SetDeadSessionRecoveryOwner(ownedElsewhere bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deadSessionRecoveryOwnedElsewhere = ownedElsewhere
}

// CheckAndRestartCrashedAgents checks all running agents for crashed CLI
// processes (bare shell prompt with no child process) and restarts them.
// Returns the names of agents that were successfully restarted so the
// caller can send them a kick with their prompt template.
//
// When SetDeadSessionRecoveryOwner(true) has been called, the restart half is
// the watchdog's; this loop still observes and logs, and still performs the
// consent/stall work that is nobody else's.
func (m *Manager) CheckAndRestartCrashedAgents(ctx context.Context) []string {
	m.runCopilotCacheJanitor()

	m.mu.RLock()
	type crashCandidate struct {
		name           string
		missingSession bool
	}
	var crashed []crashCandidate
	var consentStuck, consentCleared []string
	var stallChecks []inferencePaneCheck
	type sessionRestart struct {
		name, message, reason string
		busyNoActivity        bool
	}
	var hungSessions []sessionRestart
	var recycleCandidates []sessionRecycleCandidate
	for name, agent := range m.agents {
		if agent.State != StateRunning {
			continue
		}
		if agent.Paused {
			continue
		}
		if agent.Config.OnDemand {
			continue
		}
		if m.agentSandboxEnabledLocked(agent) {
			continue
		}
		if !m.tmuxSessionExistsForAgent(agent) {
			var uptimeSeconds float64
			if agent.StartedAt != nil {
				uptimeSeconds = time.Since(*agent.StartedAt).Seconds()
			}
			m.logger.Error("agent tmux session missing",
				"name", name,
				"session", agent.tmuxSession,
				"restart_count", agent.RestartCount,
				"uptime_seconds", int(uptimeSeconds),
				"recovery_owner", deadSessionRecoveryOwner(m.deadSessionRecoveryOwnedElsewhere),
			)
			if !m.deadSessionRecoveryOwnedElsewhere {
				crashed = append(crashed, crashCandidate{name: name, missingSession: true})
			}
			// Either way there is no session to capture a pane from, so the
			// consent/stall checks below cannot run for this agent.
			continue
		}
		pane := m.captureVisiblePaneForAgent(agent)
		if !paneHasCLIMarker(pane) {
			var uptimeSeconds float64
			if agent.StartedAt != nil {
				uptimeSeconds = time.Since(*agent.StartedAt).Seconds()
			}
			// Don't declare a freshly-launched agent crashed: the CLI needs a
			// few seconds to render its UI marker (longer for inference, which
			// may sit on a consent screen or first-token latency). Restarting
			// inside this window spawns a second CLI before the first has even
			// finished booting — the exact race that let three claude processes
			// on three models coexist after a fresh pod boot. Wait past the
			// grace period before treating a bare pane as a crash.
			if agent.StartedAt != nil && uptimeSeconds < cliBootGraceSeconds {
				m.logger.Debug("agent pane bare but within boot grace; not restarting",
					"name", name,
					"uptime_seconds", int(uptimeSeconds),
					"grace_seconds", cliBootGraceSeconds,
				)
				continue
			}
			// Carry the evidence with the verdict: the last pane lines hold the
			// CLI's own exit message (e.g. `error: unknown option '--mcp-server'`),
			// which otherwise only an operator attached to the tmux session
			// ever sees — 230 restarts of one agent went by without it.
			m.logger.Warn("agent CLI crashed (bare shell detected)",
				"name", name,
				"session", agent.tmuxSession,
				"restart_count", agent.RestartCount,
				"uptime_seconds", int(uptimeSeconds),
				"recovery_owner", deadSessionRecoveryOwner(m.deadSessionRecoveryOwnedElsewhere),
				"exit_code", crashExitCode(pane),
				"pane_tail", crashPaneTail(pane),
			)
			if !m.deadSessionRecoveryOwnedElsewhere {
				crashed = append(crashed, crashCandidate{name: name})
				continue
			}
			// Recovery is the watchdog's, but a bare pane is still a live
			// session: fall through so the consent/stall checks below — which
			// are nobody else's job — still run for this agent.
		}
		// An inference agent parked on a consent screen has a live CLI, so
		// it is not "crashed" — but it is stuck. Restarting would loop back
		// to the same screen; re-running prompt dismissal recovers it.
		if reason := m.sessionStalled(agent, pane, time.Now()); reason != "" {
			hungSessions = append(hungSessions, sessionRestart{name: name, message: agent.LastKickMessage, reason: reason})
		} else if reason := busyNoActivityStall(agent, pane, time.Now()); reason != "" {
			// No replayed kick: the governor re-kicks once the pane is
			// idle, and the storm breaker bounds repeat restarts.
			hungSessions = append(hungSessions, sessionRestart{name: name, reason: reason, busyNoActivity: true})
		} else if agent.Config.SessionRecycle.IsEnabled() {
			// #14842: a live, non-stalled session is sampled for RSS and
			// may be recycled if over budget and idle (session_recycle.go).
			recycleCandidates = append(recycleCandidates, sessionRecycleCandidate{name: name, pane: pane})
		}
		// Plain claude agents launch with --dangerously-skip-permissions too,
		// so they can park on the same bypass consent screen (#14307).
		if backend := effectiveBackend(agent); claudeConsentBackend(backend) {
			if paneShowsConsentScreen(pane) {
				consentStuck = append(consentStuck, name)
			} else {
				if !agent.consentSeenAt.IsZero() {
					consentCleared = append(consentCleared, name)
				}
				if IsInferenceBackend(backend) {
					stallChecks = append(stallChecks, inferencePaneCheck{name: name, pane: pane})
				}
			}
		}
	}
	m.mu.RUnlock()

	crashedNames := make([]string, 0, len(crashed))
	for _, c := range crashed {
		crashedNames = append(crashedNames, c.name)
	}
	m.noteOOMKillsForCrashes(crashedNames)

	for _, name := range consentCleared {
		m.clearConsentTracking(name)
	}
	for _, name := range consentStuck {
		m.dismissConsentIfStuck(name)
	}
	for _, check := range stallChecks {
		m.nudgeIfKickStalled(check.name, check.pane)
	}

	for _, h := range hungSessions {
		m.logger.Error("kick stalled: "+h.reason, "name", h.name)
		m.mu.Lock()
		if agent, ok := m.agents[h.name]; ok {
			agent.lastSessionStallRestart = time.Now()
			agent.LastError = "stalled: " + h.reason
			if h.busyNoActivity {
				// Marked before the restart so the teardown's reset keeps
				// it; kick_failed_repeatedly is only self-reported while
				// this is set (#12787).
				m.markBusyNoActivityRestartLocked(agent, time.Now())
			}
		}
		m.mu.Unlock()
		var err error
		if h.message != "" {
			err = m.RestartThenSendKick(ctx, h.name, h.message)
		} else {
			_, err = m.restartWithStormBreaker(ctx, h.name, "stalled session recovery", "stalled: "+h.reason)
		}
		if err != nil {
			m.logger.Error("failed to restart stalled session", "name", h.name, "error", err)
			if h.busyNoActivity {
				m.reportBusyNoActivityRestartFailed(h.name, h.reason, err)
			}
		}
	}

	m.sampleAndRecycleSessions(ctx, recycleCandidates)

	var restarted []string
	for _, crash := range crashed {
		name := crash.name
		m.logger.Info("restarting crashed agent", "name", name)
		var didRestart bool
		var err error
		if crash.missingSession {
			didRestart, err = m.recoverMissingSessionWithStormBreaker(ctx, name, "crashed agent recovery", "tmux session disappeared")
		} else {
			didRestart, err = m.restartWithStormBreaker(ctx, name, "crashed agent recovery", "agent CLI crashed or tmux session disappeared")
		}
		if err != nil {
			m.logger.Error("failed to restart crashed agent", "name", name, "error", err)
		} else if !didRestart {
			continue
		} else {
			m.mu.RLock()
			agent := m.agents[name]
			m.mu.RUnlock()
			if agent == nil || agent.Paused {
				continue
			}
			m.logger.Info("agent recovered from crash",
				"name", name,
				"restart_count", agent.RestartCount,
				"backend", agent.Config.Backend,
			)
			restarted = append(restarted, name)
		}
	}
	return restarted
}

func (m *Manager) KillSession(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
	m.logger.Info("agent tmux session killed", "name", name, "session", agent.tmuxSession)
	return nil
}

// SetBootstrapOverride sets a one-shot bootstrap prompt override. On the next
// restart, this message replaces the standard boot prompt. Cleared after use.
func (m *Manager) SetBootstrapOverride(name, prompt string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	agent.BootstrapOverride = prompt
	m.logger.Info("bootstrap override set", "agent", name, "len", len(prompt))
	return nil
}

// RestartWithBootstrap atomically sets the bootstrap override and restarts
// the agent under a single lock. This prevents the governor or other
// components from restarting the agent between the override set and the
// restart, which would consume the override with a standard boot.
func (m *Manager) RestartWithBootstrap(ctx context.Context, name, prompt string) error {
	m.mu.Lock()

	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}

	if err := m.applyAgentSpec(agent); err != nil {
		m.mu.Unlock()
		return err
	}

	agent.BootstrapOverride = prompt
	agent.Paused = false
	m.logger.Info("bootstrap override set (atomic)", "agent", name, "len", len(prompt))

	if agent.State == StateRunning {
		m.tmuxSendKeysForAgent(agent, "C-c", "")
		if agent.cancel != nil {
			agent.cancel()
		}
	}

	// Archive the outgoing session's kick output before the session is killed
	// below — kill-session destroys the scrollback (#4295/#4296) — and record
	// what the teardown discarded (#4002).
	m.tearDownTurnLocked(agent, "restart")

	// The bootstrap prompt IS the next turn, so pending kicks are cancelled
	// (#7363) but the breaker is not armed: nothing here is a replay.
	m.invalidateKicksOnRestartLocked(agent, "bootstrap", false, true)

	// Terminate the agent's CLI process(es) before recreating the session.
	// reapAgentCLI matches by the HIVE_AGENT env marker, so it works whether or
	// not UID isolation is enabled. killAgentProcesses (UID-based) is only safe
	// to call with a real per-agent UID: it now floor-guards uid < minAgentUID
	// and refuses to run for system/shared UIDs (as root it would otherwise
	// SIGKILL root-owned processes), so we gate the call on agent.UID > 0 below.
	reaped := m.reapAgentCLI(agent)
	if agent.UID > 0 {
		// UID isolation on: also sweep any non-CLI helper processes (MCP
		// servers, hung copilot binaries) owned exclusively by this agent.
		killed := killAgentProcesses(agent.UID, m.logger)
		m.logger.Info("killed orphaned agent processes",
			"name", name, "uid", agent.UID, "killed", killed, "reaped_cli", reaped)
	} else if reaped > 0 {
		m.logger.Info("reaped agent CLI on restart", "name", name, "reaped_cli", reaped)
	}

	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()

	m.recordRestartLocked(agent, "operator")
	agent.forceRelaunch = true

	if err := m.ensureTmuxSession(agent); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	// #3962: mint the per-agent token in this already-unlocked window,
	// mirroring Start's phase split. RestartWithBootstrap relaunches via
	// launchInTmux directly, so without this the relaunched agent kept (or
	// never got) a token — see mintAgentTokenUnlocked.
	m.mintAgentTokenUnlocked(ctx, agent)
	m.bobPreflightUnlocked(agent)
	// The relaunch needs a live-session lease exactly like Start.
	leaseID, freshLease, leaseErr := m.acquireSharedPoolLeaseUnlocked(ctx, agent, name)
	if leaseErr != nil {
		return leaseErr
	}

	// Wait for the new shell to initialize before sending the launch command.
	// Without this, $(cat /tmp/.hive-bootstrap-*.txt) can fail because the
	// shell isn't ready to process command substitution yet.
	// Released the lock before sleeping so other manager operations aren't blocked.
	time.Sleep(sessionReadyDelay)

	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-verify under the re-acquired lock: a concurrent Stop/Remove during
	// the unlocked window could have deleted this agent from the map (same
	// guard Start applies after its unlocked phase).
	if cur, ok := m.agents[name]; !ok || cur != agent {
		m.abandonFreshSharedPoolLeaseLocked(name, leaseID, freshLease)
		return fmt.Errorf("agent %s removed during restart", name)
	}
	return m.launchInTmuxLeasedLocked(ctx, agent, name, leaseID, freshLease)
}

// RestartThenSendKick restarts the agent with a clean slate (no bootstrap
// override), waits for the CLI to become ready, then delivers the message
// via SendKick. This combines the clean-context benefit of restart with
// the reliable prompt-waited delivery of SendKick — avoiding the fragile
// $(cat file) shell expansion that RestartWithBootstrap uses.
func (m *Manager) RestartThenSendKick(ctx context.Context, name, message string) error {
	// Step 1: Restart with NO bootstrap override — clean slate launch.
	if err := m.Restart(ctx, name); err != nil {
		return fmt.Errorf("restart failed: %w", err)
	}

	// Step 2: Wait for CLI to be ready (input prompt visible).
	m.mu.RLock()
	agent, ok := m.agents[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("agent %s not found after restart", name)
	}
	if !m.waitForCLIReadyForAgent(agent) {
		return fmt.Errorf("agent %s CLI not ready after restart", name)
	}

	// Step 3: Send the message via SendKick — waits for prompt, chunks reliably.
	return m.SendKick(name, message)
}

// cliProcessMarkers are substrings that identify a CLI process in its
// /proc/<pid>/cmdline. The Claude CLI (and inference backends, which also use
// it) runs as `claude` (often re-exec'd via node); copilot/gemini/goose/bob run
// under their own names. Matching cmdline substrings catches the CLI regardless
// of the interpreter the process reports as its comm name.
var cliProcessMarkers = []string{
	"claude",
	"copilot",
	"gemini",
	"goose",
	"bob",
}

// reapAgentCLI finds and SIGKILLs every CLI process belonging to the given
// agent, matched by the HIVE_AGENT=<name> marker in /proc/<pid>/environ. This
// marker is inlined into every launch command (buildEnvPrefix) and set on the
// tmux session (ensureTmuxSession), so it uniquely identifies an agent's CLI
// processes — unlike UID matching, which cannot distinguish agents that share
// the dev UID (UID isolation disabled). Returns the number of processes killed.
//
// This is the single-CLI guarantee: before every (re)launch, any pre-existing
// or leaked CLI for the agent is terminated, so an agent can never accumulate
// concurrent claude processes on different models. tmux kill-session alone is
// insufficient — a detached node/claude child can survive the session's SIGHUP
// and keep hitting the gateway (403-flooding the pane on a stale model).
// procRoot is the /proc mount the reaper scans. A var (not const) so tests can
// point it at a fake proc tree on non-Linux hosts; production value is "/proc"
// and nothing on the launch path mutates it.
var procRoot = "/proc"

func (m *Manager) Restart(ctx context.Context, name string) error {
	return m.RestartWithReason(ctx, name, "operator")
}

func (m *Manager) RestartWithReason(ctx context.Context, name, reason string) error {
	return m.restartWithReason(ctx, name, reason, false)
}

func (m *Manager) RestartMissingSessionWithReason(ctx context.Context, name, reason string) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return false, fmt.Errorf("agent %s not found", name)
	}
	m.clearStartFailureLocked(agent)
	if err := m.applyAgentSpec(agent); err != nil {
		return false, err
	}
	if agent.cancel != nil {
		agent.cancel()
	}
	agent.forceRelaunch = true
	if err := m.ensureTmuxSession(agent); err != nil {
		if isTmuxDuplicateSessionError(err) {
			m.logger.Warn("missing-session recovery found an existing tmux session; treating agent as alive without incrementing restart_count",
				"name", name,
				"session", agent.tmuxSession,
				"error", err)
			m.restoreExistingSessionMonitorLocked(ctx, agent)
			return false, nil
		}
		return false, err
	}

	m.recordRestartLocked(agent, reason)
	m.logger.Info("audit: agent restarting", "name", name, "reason", sanitizeRestartReason(reason), "restart_count", agent.RestartCount)
	if agent.Paused {
		agent.State = StatePaused
		m.logger.Info("agent restart preserving paused state", "name", name)
		return true, nil
	}

	genBeforeMint := agent.launchGen
	m.mu.Unlock()
	m.mintAgentTokenUnlocked(ctx, agent)
	m.bobPreflightUnlocked(agent)
	// The relaunch needs a live-session lease exactly like Start: an evicted
	// agent recovered here must not run unleased and uncapped.
	leaseID, freshLease, leaseErr := m.acquireSharedPoolLeaseUnlocked(ctx, agent, name)
	m.mu.Lock()
	if leaseErr != nil {
		return false, leaseErr
	}
	if cur, ok := m.agents[name]; !ok || cur != agent {
		m.abandonFreshSharedPoolLeaseLocked(name, leaseID, freshLease)
		return false, fmt.Errorf("agent %s removed during missing-session recovery", name)
	}
	if agent.launchGen != genBeforeMint {
		m.abandonFreshSharedPoolLeaseLocked(name, leaseID, freshLease)
		return false, nil
	}

	return true, m.launchInTmuxLeasedLocked(ctx, agent, name, leaseID, freshLease)
}

// restartForKick is the restart a kick delivery performs on its OWN behalf
// when it finds the CLI crashed or wedged on a consent screen (SendKick /
// deliverKickAsync). It still invalidates every OTHER pending kick for the
// agent, but it neither fails the caller's dispatch record nor arms the
// restart/kick breaker: the caller captures the new kick epoch after this
// returns and is the intended next delivery, not a replay (#7363). Recovery is
// still automatic: use the shared storm budget rather than treating each kick
// as an operator retry that can restart an unbootable CLI indefinitely.
func (m *Manager) restartForKick(ctx context.Context, name string) error {
	const reason = "kick CLI recovery"
	plan := m.planAutomaticRestart(name, reason, "CLI crashed or stuck on consent screen before kick", time.Now())
	_, restarted, err := m.applyRestartStormPlan(name, reason, plan, func() error {
		return m.restartWithReason(ctx, name, reason, true)
	})
	if err != nil {
		return err
	}
	if !restarted {
		if plan.backoff > 0 {
			return fmt.Errorf("agent %s kick recovery suppressed by restart-storm breaker; retry in %v", name, plan.backoff.Round(time.Second))
		}
		return fmt.Errorf("agent %s kick recovery suppressed by restart-storm breaker", name)
	}
	return nil
}

// restartWithReason is RestartWithReason's body. kickInitiated marks a
// restart performed by a kick delivery for itself — see restartForKick.
func (m *Manager) restartWithReason(ctx context.Context, name, reason string, kickInitiated bool) error {
	// Detach from the caller's cancellation. Restart is routinely invoked from
	// goroutines whose OWN context is the per-launch agentCtx this function is
	// about to cancel (pollTmuxOutputForAgent's token-detected and TLS-error
	// restarts): agent.cancel() below kills that parent, so the relaunch's new
	// WithCancel context was born dead — launchInTmux still typed the CLI, but
	// pollTmuxOutputForAgent and watchForTrustPromptForAgent exited instantly,
	// leaving every restarted agent with NO pane monitors. Live signature
	// (hivecommons/hive, 2026-08-22): exactly one auto-answered trust prompt
	// per agent per pod boot, then wedged panes forever after the first
	// token-detected restart. A relaunch must never be aborted by the
	// cancellation of the launch it replaces. (Nil-guarded: WithoutCancel
	// panics on a nil parent, and callers pass nil for "no context".)
	if ctx == nil {
		ctx = context.Background()
	} else {
		ctx = context.WithoutCancel(ctx)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	// A restart is a deliberate retry — by an operator at the dashboard button,
	// or by a recovery path that believes it has changed the conditions. The
	// backoff exists to pace the AUTOMATIC loop, never to make a human wait, so
	// clear the record and let this attempt run. If it fails the same way, the
	// ladder starts again from the first rung (#5958).
	m.clearStartFailureLocked(agent)
	// The operator explicitly asked for the agent to run again, so the
	// governor may recover it from a later crash.
	agent.OperatorStopped = false

	if err := m.applyAgentSpec(agent); err != nil {
		return err
	}

	if agent.State == StateRunning {
		m.tmuxSendKeysForAgent(agent, "C-c", "")
		if agent.cancel != nil {
			agent.cancel()
		}
	}

	// Archive the outgoing session's kick output BEFORE anything below kills
	// the CLI or the session — kill-session destroys the scrollback and with
	// it the only record of the previous run (#4295/#4296). The same funnel
	// records what this teardown cost the in-flight turn (#4002).
	_, producing := m.tearDownTurnLocked(agent, "restart")

	// A restart cancels the agent's pending kicks (#7363): the session the
	// message was composed for is about to be destroyed, and an operator who
	// clicked restart must not have the interrupted work replayed into the
	// relaunched CLI. When the teardown just destroyed a PRODUCING turn, also
	// arm the breaker so a kick cannot follow within seconds and be restarted
	// in its turn — the loop observed live. A kick's own recovery restart is
	// exempt from both the dispatch failure and the breaker (restartForKick).
	m.invalidateKicksOnRestartLocked(agent, reason, producing && !kickInitiated, !kickInitiated)

	// Terminate the agent's CLI process(es) before recreating the session.
	// reapAgentCLI matches by the HIVE_AGENT env marker, so it works whether or
	// not UID isolation is enabled. killAgentProcesses (UID-based) is only safe
	// to call with a real per-agent UID: it now floor-guards uid < minAgentUID
	// and refuses to run for system/shared UIDs (as root it would otherwise
	// SIGKILL root-owned processes), so we gate the call on agent.UID > 0 below.
	reaped := m.reapAgentCLI(agent)
	if agent.UID > 0 {
		// UID isolation on: also sweep any non-CLI helper processes (MCP
		// servers, hung copilot binaries) owned exclusively by this agent.
		killed := killAgentProcesses(agent.UID, m.logger)
		m.logger.Info("killed orphaned agent processes",
			"name", name, "uid", agent.UID, "killed", killed, "reaped_cli", reaped)
	} else if reaped > 0 {
		m.logger.Info("reaped agent CLI on restart", "name", name, "reaped_cli", reaped)
	}

	agent.forceRelaunch = true
	if output, err := m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).CombinedOutput(); err != nil {
		m.logger.Warn("tmux kill-session before restart failed",
			"name", name,
			"session", agent.tmuxSession,
			"reason", sanitizeRestartReason(reason),
			"error", err,
			"stderr", strings.TrimSpace(string(output)))
	}

	m.recordRestartLocked(agent, reason)
	m.logger.Info("audit: agent restarting", "name", name, "reason", sanitizeRestartReason(reason), "restart_count", agent.RestartCount)

	if err := m.ensureTmuxSession(agent); err != nil {
		return err
	}

	if agent.Paused {
		agent.State = StatePaused
		m.logger.Info("agent restart preserving paused state", "name", name)
		return nil
	}

	// #3962: mint the per-agent token in an UNLOCKED window before the
	// relaunch, mirroring Start's phase split. Restart used to call
	// launchInTmux without ever minting, so a restarted agent ran with an
	// empty/stale token cache. The mint must not run under m.mu — see
	// mintAgentTokenUnlocked — so release, mint, then re-verify under the
	// re-acquired lock exactly as Start does. The deferred Unlock above still
	// releases the lock re-acquired here on every return path.
	//
	// The lost-race check compares launchGen, NOT agent.State. Restart —
	// unlike Start, which refuses running agents in its phase 1 — is
	// routinely called ON a running agent (dashboard restart, SendKick's
	// crashed-CLI recovery), and nothing on the restart path clears the
	// pre-restart StateRunning. The original `State == StateRunning` form of
	// this guard therefore misread the agent's OWN old state as "another
	// path won the launch race" and returned nil after the CLI had already
	// been killed and its session recreated — every restart of a running
	// agent became a silent kill-without-relaunch, and a kick to a crashed
	// CLI timed out waiting for a CLI that was never going to launch.
	// launchGen increments on every completed launch, so a changed gen — and
	// only a changed gen — proves a racing path actually launched while the
	// lock was released.
	genBeforeMint := agent.launchGen
	m.mu.Unlock()
	m.mintAgentTokenUnlocked(ctx, agent)
	m.bobPreflightUnlocked(agent)
	// The relaunch needs a live-session lease exactly like Start: an evicted
	// agent restarted from the dashboard or recovery must not run unleased
	// and uncapped.
	leaseID, freshLease, leaseErr := m.acquireSharedPoolLeaseUnlocked(ctx, agent, name)
	m.mu.Lock()
	if leaseErr != nil {
		return leaseErr
	}
	if cur, ok := m.agents[name]; !ok || cur != agent {
		m.abandonFreshSharedPoolLeaseLocked(name, leaseID, freshLease)
		return fmt.Errorf("agent %s removed during restart", name)
	}
	if agent.launchGen != genBeforeMint {
		// Another path won the launch race while we were unlocked.
		m.abandonFreshSharedPoolLeaseLocked(name, leaseID, freshLease)
		return nil
	}

	return m.launchInTmuxLeasedLocked(ctx, agent, name, leaseID, freshLease)
}

func isTmuxDuplicateSessionError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate session")
}

func (m *Manager) restoreExistingSessionMonitorLocked(ctx context.Context, agent *AgentProcess) {
	if agent.cancel != nil {
		agent.cancel()
	}
	agent.forceRelaunch = false
	agent.State = StateRunning
	if agent.StartedAt == nil {
		now := time.Now()
		agent.StartedAt = &now
	}
	agentCtx, cancel := context.WithCancel(ctx)
	agent.cancel = cancel
	go m.pollTmuxOutputForAgent(agent, agentCtx)
	if backendHasBlockingPrompts(agent.effectiveBackend()) {
		go m.watchForTrustPromptForAgent(agent, agentCtx)
	}
}

func (m *Manager) ResetRestartCount(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.RestartCount = 0
	agent.RestartEvents = nil
	agent.LastRestartReason = "operator"
	agent.restartStormBackoffUntil = time.Time{}
	agent.clearTokenRestartCap()
	// Resetting the counter is the operator saying "I have looked at this";
	// lift the restart/kick breaker with it (#7363) so a deliberate kick
	// need not wait out the hold.
	agent.kickHoldUntil = time.Time{}
	agent.kickHoldReason = ""
	return nil
}

func (m *Manager) SeedRestartCount(name string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.RestartCount = count
	}
}

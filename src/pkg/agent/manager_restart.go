// Crash recovery and restarts: crashed-agent detection and restart,
// bob relaunch-awaiting-key handling, session kills, CLI process
// reaping, and restart-count bookkeeping.
package agent

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

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
	m.mu.RLock()
	var crashed []string
	var consentStuck, consentCleared []string
	var stallChecks []inferencePaneCheck
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
				crashed = append(crashed, name)
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
			m.logger.Warn("agent CLI crashed (bare shell detected)",
				"name", name,
				"session", agent.tmuxSession,
				"restart_count", agent.RestartCount,
				"uptime_seconds", int(uptimeSeconds),
				"recovery_owner", deadSessionRecoveryOwner(m.deadSessionRecoveryOwnedElsewhere),
			)
			if !m.deadSessionRecoveryOwnedElsewhere {
				crashed = append(crashed, name)
				continue
			}
			// Recovery is the watchdog's, but a bare pane is still a live
			// session: fall through so the consent/stall checks below — which
			// are nobody else's job — still run for this agent.
		}
		// An inference agent parked on a consent screen has a live CLI, so
		// it is not "crashed" — but it is stuck. Restarting would loop back
		// to the same screen; re-running prompt dismissal recovers it.
		if IsInferenceBackend(effectiveBackend(agent)) {
			if paneShowsConsentScreen(pane) {
				consentStuck = append(consentStuck, name)
			} else {
				if !agent.consentSeenAt.IsZero() {
					consentCleared = append(consentCleared, name)
				}
				stallChecks = append(stallChecks, inferencePaneCheck{name: name, pane: pane})
			}
		}
	}
	m.mu.RUnlock()

	for _, name := range consentCleared {
		m.clearConsentTracking(name)
	}
	for _, name := range consentStuck {
		m.dismissConsentIfStuck(name)
	}
	for _, check := range stallChecks {
		m.nudgeIfKickStalled(check.name, check.pane)
	}

	var restarted []string
	for _, name := range crashed {
		m.logger.Info("restarting crashed agent", "name", name)
		if err := m.Restart(ctx, name); err != nil {
			m.logger.Error("failed to restart crashed agent", "name", name, "error", err)
		} else {
			m.mu.RLock()
			agent := m.agents[name]
			m.mu.RUnlock()
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

// RelaunchBobAgentsAwaitingKey restarts the bob-backend agents that
// launchInTmux parked in StateFailed because no API key was configured, and
// returns their names. It is the "absent → present" half of the key lifecycle:
// the resolver makes a newly-saved key visible to the NEXT launch, but nothing
// carries it into a session whose shell already exists, so an agent parked for
// a missing key would otherwise sit at bob's key prompt until someone
// intervened.
//
// Each relaunch RECREATES the tmux session rather than just re-typing the
// launch command — see killSessionForRelaunch for why that is required and not
// merely tidy.
//
// Call it after a key is stored. It is a no-op — returning nil — when the key
// still does not resolve, so a clear (or a failed save) never launches
// anything.
//
// Selection is deliberately narrow: awaitingBobKey is set on exactly one
// branch of launchInTmux and cleared on every launch attempt, so a running
// agent, a non-bob agent, and an agent failed for any other reason are all
// skipped. An operator-paused agent is skipped too — a key save must not
// override a deliberate pause. That also makes a double save harmless: the
// first relaunch clears the flag, so the second finds nothing to do.
//
// Locking: candidates are collected under RLock, the lock is RELEASED, and
// only then is Start called — Start takes m.mu.Lock() itself, and m.mu is a
// non-reentrant RWMutex, so calling it under the read lock would deadlock
// (incident #1980→#1988). This mirrors CheckAndRestartCrashedAgents.
func (m *Manager) RelaunchBobAgentsAwaitingKey(ctx context.Context) []string {
	// The key must actually resolve now; otherwise every relaunch would just
	// re-park on the same branch. Read lock-free via the atomic resolver, and
	// never bind or log the value — only its presence matters here.
	if m.bobAPIKey() == "" {
		return nil
	}

	candidates := m.bobAgentsAwaitingKey()

	var relaunched []string
	for _, name := range candidates {
		m.logger.Info("relaunching bob agent parked for missing API key", "name", name)
		// Kill the stale tmux session FIRST. This is load-bearing, not
		// hygiene: BOBSHELL_API_KEY is Secret, so buildEnvPrefix omits it from
		// the typed command line and it is delivered ONLY by tmux
		// set-environment — which updates the SESSION environment and is
		// inherited just by shells created afterwards. The pane's bash was
		// started before the key existed, so it does not have the variable and
		// never will; reapAgentCLI kills the bob CLI but leaves that bash
		// alive, so a plain relaunch retypes the command into the same
		// key-less shell and bob prompts for a key again (observed on
		// hosted-available-vllmd-01: BOBSHELL_API_KEY present in the tmux
		// session env, absent from every bob /proc/<pid>/environ).
		// Killing the session makes ensureTmuxSession build a new one and
		// re-run set-environment before any shell exists, so the fresh bash —
		// and the bob it spawns — inherit the key.
		if err := m.killSessionForRelaunch(name); err != nil {
			m.logger.Warn("could not kill stale session before bob relaunch; launching anyway",
				"name", name, "error", err)
		}
		if err := m.Start(ctx, name); err != nil {
			// One agent's failure must never abort the rest of the fleet,
			// exactly as in the crash-restart loop above.
			m.logger.Error("failed to relaunch bob agent after key save", "name", name, "error", err)
			continue
		}
		relaunched = append(relaunched, name)
	}
	return relaunched
}

// killSessionForRelaunch tears down an agent's tmux session so the next
// ensureTmuxSession creates a fresh one whose shell inherits the current
// set-environment values (including a newly-saved BOBSHELL_API_KEY).
//
// This is KillSession's behaviour, but it deliberately does not call
// KillSession: that method takes m.mu.Lock() and is a public entry point.
// Keeping a private helper with the same lock discipline (acquire, act,
// release — never held across Start) keeps the relaunch loop's locking
// obvious and avoids any temptation to hold a lock across the launch, which
// is the deadlock class from incident #1980→#1988.
func (m *Manager) killSessionForRelaunch(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}
	// Not an error if the session is already gone — the goal is "no stale
	// shell", which a missing session already satisfies.
	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
	// State must not stay Running: Start refuses to launch a running agent,
	// and the session backing that state no longer exists.
	if agent.State == StateRunning {
		agent.State = StateStopped
	}
	m.logger.Info("killed stale tmux session so the fresh shell inherits the bob key",
		"name", name, "session", agent.tmuxSession)
	return nil
}

// bobAgentsAwaitingKey returns the names of agents parked by launchInTmux for a
// missing bob API key and eligible to be started now. Split out from
// RelaunchBobAgentsAwaitingKey so the selection rules can be tested without
// tmux: this is the part that must never pick up a running, paused, or non-bob
// agent.
//
// Takes m.mu.RLock and releases it before returning; the caller must NOT hold
// m.mu (non-reentrant RWMutex).
func (m *Manager) bobAgentsAwaitingKey() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var candidates []string
	for name, agent := range m.agents {
		if agent == nil || !agent.awaitingBobKey {
			continue
		}
		// Defensive: awaitingBobKey is only ever set on the bob branch, but
		// re-check the effective backend so a backend override applied after
		// parking cannot smuggle a non-bob agent into this path.
		if effectiveBackend(agent) != bobBackend {
			continue
		}
		// Never disturb a healthy agent, and never override a pause — a key
		// save is not a resume.
		if agent.State == StateRunning || agent.Paused {
			continue
		}
		candidates = append(candidates, name)
	}
	return candidates
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
		return fmt.Errorf("agent %s removed during restart", name)
	}
	return m.launchInTmux(ctx, agent)
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

func (m *Manager) reapAgentCLI(agent *AgentProcess) int {
	procPath := procRoot
	marker := "HIVE_AGENT=" + agent.Name

	entries, err := os.ReadDir(procPath)
	if err != nil {
		m.logger.Warn("reapAgentCLI: failed to read /proc", "agent", agent.Name, "error", err)
		return 0
	}

	killed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if pid == os.Getpid() {
			continue
		}

		// cmdline is NUL-separated; read it raw and check for a CLI binary.
		cmdlineRaw, err := os.ReadFile(filepath.Join(procPath, entry.Name(), "cmdline"))
		if err != nil || len(cmdlineRaw) == 0 {
			continue
		}
		cmdline := strings.ReplaceAll(string(cmdlineRaw), "\x00", " ")
		if !containsCLIMarker(cmdline) {
			continue
		}

		// environ is NUL-separated KEY=VALUE pairs. Match the exact agent so we
		// never kill another agent's CLI when UIDs are shared.
		environRaw, err := os.ReadFile(filepath.Join(procPath, entry.Name(), "environ"))
		if err != nil {
			continue
		}
		if !environHasMarker(string(environRaw), marker) {
			continue
		}

		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
			m.logger.Info("reaped agent CLI process",
				"agent", agent.Name, "pid", pid, "cmdline", truncateStr(cmdline, 120))
		}
	}
	if killed > 0 {
		m.logger.Info("reapAgentCLI complete", "agent", agent.Name, "killed", killed)
	}
	return killed
}

// containsCLIMarker reports whether a /proc cmdline names a known CLI binary.
func containsCLIMarker(cmdline string) bool {
	for _, marker := range cliProcessMarkers {
		if strings.Contains(cmdline, marker) {
			return true
		}
	}
	return false
}

// environHasMarker reports whether a raw NUL-separated /proc environ blob
// contains the exact HIVE_AGENT=<name> pair. Splitting on NUL and comparing
// whole entries avoids a prefix collision between "scanner" and "scanner-2".
func environHasMarker(environ, marker string) bool {
	for _, pair := range strings.Split(environ, "\x00") {
		if pair == marker {
			return true
		}
	}
	return false
}

// minAgentUID is the lowest UID killAgentProcesses will ever target. Per-agent
// UIDs are allocated from baseAgentUID (2001) upward, so any uid below this
// belongs to the system (root=0, the proxy user, etc.). Refusing to match on a
// sub-range UID guarantees that a stray killAgentProcesses(0) — which happens
// when UID isolation is off or an agent is missing from the UID map — can never
// SIGKILL root-owned processes (hive itself, PID 1, the shared tmux server).
const minAgentUID = baseAgentUID

// killAgentProcesses finds all processes owned by the given UID via /proc and
// sends SIGKILL to each. Hung copilot binaries ignore SIGINT, so brute-force
// cleanup is needed to prevent orphan accumulation on the shared SQLite store.
//
// The function is EPERM-safe only for unprivileged callers; hive runs as root,
// so it is NOT inherently a no-op for shared/system UIDs. A floor guard
// (uid >= minAgentUID) plus a self-skip (never SIGKILL our own PID) are the
// real defenses that keep uid < minAgentUID from touching system processes.
func killAgentProcesses(uid int, logger *slog.Logger) int {
	// Floor guard: refuse to sweep by a system-range UID. uid==0 (root) reaches
	// here when UID isolation is disabled or LookupByName missed the agent; as
	// root, matching ownerUID==0 would SIGKILL every root process. This is a real
	// bug signal, so warn loudly and kill nothing.
	if uid < minAgentUID {
		if logger != nil {
			logger.Warn("refusing to kill by uid, would target system/root processes",
				"uid", uid, "min_agent_uid", minAgentUID)
		}
		return 0
	}

	procPath := procRoot
	entries, err := os.ReadDir(procPath)
	if err != nil {
		logger.Warn("failed to read /proc for process cleanup", "uid", uid, "error", err)
		return 0
	}

	killed := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// Belt-and-suspenders: never SIGKILL the hive process itself, mirroring
		// reapAgentCLI's self-skip.
		if pid == os.Getpid() {
			continue
		}

		statusPath := filepath.Join(procPath, entry.Name(), "status")
		f, err := os.Open(statusPath)
		if err != nil {
			continue
		}

		ownerUID := -1
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "Uid:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					if parsed, err := strconv.Atoi(fields[1]); err == nil {
						ownerUID = parsed
					}
				}
				break
			}
		}
		_ = f.Close() // read-only /proc status fd; nothing to lose on close error

		if ownerUID != uid {
			continue
		}

		if err := syscall.Kill(pid, syscall.SIGKILL); err == nil {
			killed++
		}
	}
	return killed
}

func (m *Manager) Restart(ctx context.Context, name string) error {
	return m.RestartWithReason(ctx, name, "operator")
}

func (m *Manager) RestartWithReason(ctx context.Context, name, reason string) error {
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
	m.tearDownTurnLocked(agent, "restart")

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

	m.recordRestartLocked(agent, reason)
	agent.forceRelaunch = true
	m.logger.Info("audit: agent restarting", "name", name, "restart_count", agent.RestartCount)

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
	m.mu.Lock()
	if cur, ok := m.agents[name]; !ok || cur != agent {
		return fmt.Errorf("agent %s removed during restart", name)
	}
	if agent.launchGen != genBeforeMint {
		// Another path won the launch race while we were unlocked.
		return nil
	}

	return m.launchInTmux(ctx, agent)
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
	return nil
}

func (m *Manager) SeedRestartCount(name string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.RestartCount = count
	}
}

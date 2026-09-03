// The tmux terminal seams: session creation/existence, pane capture
// (visible, scrollback, full log), literal/key sends, CLI/input-prompt
// readiness waits, and tmux argv plumbing. These are the eight terminal
// seam methods #5638 names for the TerminalSession extraction.
package agent

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// paneCaptureSleep is a pacing var, not a const, for the same reason as the
// pacing block near deliverStartupKick: the pkg/agent TestMain shrinks it so
// the suite fits the default `go test` timeout. Production value unchanged.
var paneCaptureSleep = 500 * time.Millisecond

var defaultTmuxSocket string

// tmuxBaseArgs returns the base tmux command args for an agent. When the agent
// has a per-agent tmux socket (UID isolation), it returns ["tmux", "-L", socketName].
// Otherwise it returns ["tmux"] for the shared tmux server.
func (m *Manager) tmuxBaseArgs(agent *AgentProcess) []string {
	if agent.tmuxSocket != "" {
		return []string{"tmux", "-L", agent.tmuxSocket}
	}
	if defaultTmuxSocket != "" {
		return []string{"tmux", "-L", defaultTmuxSocket}
	}
	return []string{"tmux"}
}

// tmuxHistoryLimit returns the scrollback depth (in lines) agent tmux sessions
// are created with: HIVE_TMUX_HISTORY_LIMIT when set to a positive integer,
// defaultTmuxHistoryLimit otherwise.
func tmuxHistoryLimit() int {
	if v := os.Getenv(tmuxHistoryLimitEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultTmuxHistoryLimit
}

// tmuxPaneWidth returns the column count agent tmux panes are created with:
// HIVE_TMUX_PANE_WIDTH when set to a positive integer, defaultTmuxPaneWidth
// otherwise. Operators who need even wider panes (or who want to reproduce the
// old 80-column rendering) can set the env var; see defaultTmuxPaneWidth for
// why the default is wide.
func tmuxPaneWidth() int {
	if v := os.Getenv(tmuxPaneWidthEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultTmuxPaneWidth
}

// newSessionCommands returns the tmux command sequence (after the base
// socket args) that creates a detached agent session with a deep scrollback
// buffer.
//
// Ordering is the whole point (#3694, #3693): tmux reads history-limit at PANE
// creation time, so it must be raised BEFORE new-session forks the session's
// first pane. Raising it on an existing pane — as the ttyd attach wrapper does
// on attach — never deepens a buffer that was created shallow; with tmux's
// 2000-line default that capped both browser copy-mode scrollback and the
// "full log" capture at ~2000 lines. Both commands run in ONE client
// invocation ("; " is tmux's command separator in argv): the client
// auto-starts the server if needed, applies the global option, then creates
// the session — before the server's exit-empty logic could tear down a
// sessionless server and discard the option.
//
// The same creation-time reasoning applies to pane GEOMETRY (#3878): a
// detached session has no attached client to size itself from, so tmux would
// default the pane to 80x24 and the agent CLI would truncate every long tool
// invocation to fit. -x/-y must therefore be passed to new-session itself —
// resizing later cannot restore text the CLI already elided.
func newSessionCommands(session, dir string) []string {
	return []string{
		"set-option", "-g", "history-limit", strconv.Itoa(tmuxHistoryLimit()), ";",
		// #4399: set the status line BEFORE new-session so the pane carries it
		// from its first frame. Global (-g) rather than per-session because each
		// agent runs on its own tmux socket under its own UID
		// (/tmp/tmux-2007/hive-scanner), so "global" is scoped to that one
		// agent's server — and a global set also reaches panes created later in
		// the session, which a per-session set would not.
		"set-option", "-g", "status-right", tmuxStatusRight, ";",
		// #4399 follow-up: tmux truncates status-right to status-right-length,
		// whose DEFAULT is 40 columns — which cut the message above off at
		// "[SCROLLBACK - not following live outp". Must be raised or the label
		// ships truncated.
		"set-option", "-g", "status-right-length", strconv.Itoa(tmuxStatusRightLength), ";",
		"set-option", "-g", "status-interval", strconv.Itoa(tmuxStatusInterval), ";",
		"new-session", "-d", "-s", session, "-c", dir,
		"-x", strconv.Itoa(tmuxPaneWidth()), "-y", strconv.Itoa(defaultTmuxPaneHeight), ";",
		// #4399: hide tmux's unlabelled black-on-yellow copy-mode marker (its
		// "<top-line write time> [pos/total]" is what the issue could not
		// parse) — the labelled status line above carries the position now.
		// AFTER new-session on purpose: bind-key is server-wide and reaches
		// wheel events whenever it runs, but if a pre-3.2 tmux rejects the
		// `-H` flag the session itself must already exist.
		"bind-key", "-n", tmuxWheelBindingKey,
		"if-shell", "-F", tmuxWheelBindingCond, tmuxWheelBindingThen, tmuxWheelBindingElse,
	}
}

func (m *Manager) agentExecUserSpec(agent *AgentProcess) string {
	if agent.UID <= 0 {
		return ""
	}
	agentUser := fmt.Sprintf("hive-%s", agent.Name)
	if _, err := user.Lookup(agentUser); err == nil {
		return agentUser
	}
	return fmt.Sprintf("%d:%d", agent.UID, os.Getgid())
}

func outputErr(prefix string, err error, output []byte) error {
	msg := strings.TrimSpace(string(output))
	if msg == "" {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return fmt.Errorf("%s: %w: %s", prefix, err, msg)
}

func (m *Manager) tmuxCmd(agent *AgentProcess, args ...string) *exec.Cmd {
	if err := validateTmuxKillSessionArgs(args); err != nil {
		agentName := ""
		if agent != nil {
			agentName = agent.Name
		}
		if m.logger != nil {
			m.logger.Warn("refusing unsafe tmux kill-session", "agent", agentName, "error", err)
		}
		return exec.Command("false")
	}

	base := m.tmuxBaseArgs(agent)
	tmuxArgs := append(base[1:], args...)
	if agent.UID > 0 {
		suExecArgs := append([]string{m.agentExecUserSpec(agent), base[0]}, tmuxArgs...)
		return exec.Command("su-exec", suExecArgs...)
	}
	return exec.Command(base[0], tmuxArgs...)
}

func validateTmuxKillSessionArgs(args []string) error {
	if len(args) == 0 || args[0] != "kill-session" {
		return nil
	}

	target := ""
	for i := 1; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "-t="):
			target = strings.TrimPrefix(arg, "-t=")
		case strings.HasPrefix(arg, "-target="):
			target = strings.TrimPrefix(arg, "-target=")
		case arg == "-t" || arg == "-target":
			if i+1 < len(args) {
				target = args[i+1]
			}
		case strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(strings.TrimPrefix(arg, "-"), "a"):
			return fmt.Errorf("kill-session -a is not allowed")
		}
	}

	if target == "" {
		return fmt.Errorf("missing target")
	}
	if !strings.HasPrefix(target, "hive-") {
		return fmt.Errorf("target %q is not hive-namespaced", target)
	}
	return nil
}

func (m *Manager) ensureTmuxSession(agent *AgentProcess) error {
	if m.tmuxSessionExistsForAgent(agent) {
		return nil
	}

	agentDir := m.workDir + "/" + agent.Name
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("creating agent work dir %s: %w", agentDir, err)
	}

	var cmd *exec.Cmd
	if agent.UID > 0 {
		suExecArgs := []string{"su-exec", m.agentExecUserSpec(agent)}
		tmuxArgs := append(m.tmuxBaseArgs(agent), newSessionCommands(agent.tmuxSession, agentDir)...)
		cmd = exec.Command(suExecArgs[0], append(suExecArgs[1:], tmuxArgs...)...)
	} else {
		base := m.tmuxBaseArgs(agent)
		tmuxArgs := append(base[1:], newSessionCommands(agent.tmuxSession, agentDir)...)
		cmd = exec.Command(base[0], tmuxArgs...)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return outputErr(fmt.Sprintf("creating tmux session for %s", agent.Name), err, output)
	}

	// tmux creates /tmp/tmux-{uid}/ with mode 700; ttyd runs as dev (uid 1001,
	// node group) and needs to traverse into these dirs to attach to sockets.
	// os.Chmod doesn't work here because the Go binary runs as dev, not as the
	// agent user who owns the directory. Use su-exec to chmod as the agent.
	if agent.UID > 0 {
		tmuxDir := fmt.Sprintf("/tmp/tmux-%d", agent.UID)
		_ = exec.Command("su-exec", m.agentExecUserSpec(agent), "chmod", "710", tmuxDir).Run()
	}

	// Pre-create the agent-owned CODEX_HOME before launch (codex won't create
	// it and needs to own it). Only for the codex backend.
	{
		launchBackend := agent.Config.Backend
		if agent.BackendOverride != "" {
			launchBackend = agent.BackendOverride
		}
		if launchBackend == codexBackend {
			m.setupCodexHome(agent)
		}
		// Provision the per-agent interactive HOME (#4596): directory, shared-
		// state bridges, signed-in Claude session adoption, legacy tmp sweep.
		m.setupInteractiveHome(agent, launchBackend)
	}

	// Set per-session env vars via tmux set-environment (raw values, no shell quoting).
	for _, p := range m.agentEnvPairs(agent) {
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, p.Key, p.Value).Run()
	}
	// Strip the shared FULL installation token (HIVE_GITHUB_TOKEN) from EVERY
	// agent session (audit H3 follow-up, CWE-522). The hive process env carries
	// HIVE_GITHUB_TOKEN (exported by entrypoint.sh from the full-token cache, and
	// legitimately read by the hive itself as a config fallback), and a tmux
	// server started by this process inherits that env — so without this strip it
	// would leak into every pane the server forks, handing agents the full-
	// privilege installation token. Agents must use ONLY their per-agent SCOPED
	// token (gh-token-<agent>.cache via HIVE_AGENT_TOKEN_CACHE + the gh wrapper),
	// so unset the full-token env in the session for all agents regardless of
	// push capability. This does not touch the hive process env.
	_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", "HIVE_GITHUB_TOKEN").Run()
	// Strip gh/git tokens from advisory agent sessions.
	if !m.agentMode(agent).CanPush() {
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", "GH_TOKEN").Run()
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", "GITHUB_TOKEN").Run()
	}
	// Same for the Linear credential below the ISSUES_ONLY floor: the hive
	// process env may carry LINEAR_API_KEY (the work-source key, referenced
	// from hive.yaml as ${LINEAR_API_KEY}) and a tmux server started by this
	// process inherits it, so an advisory agent would otherwise be handed a
	// write-capable key the proxy gate would have to catch on every call.
	if !m.agentMode(agent).CanCreateIssues() {
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", linearAccessTokenEnvVar).Run()
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", linearAPIKeyEnvVar).Run()
	}

	// Every set-environment above updated the SESSION environment, which tmux
	// only copies into processes it forks AFTERWARDS. `new-session -d` already
	// forked this session's pane shell, so that bash predates all of it and
	// will never see a single one of those variables — including the Secret
	// pairs (BOBSHELL_API_KEY, CLAUDE_CODE_OAUTH_TOKEN) that buildEnvPrefix
	// deliberately keeps off the command line. Every CLI launched by send-keys
	// into this pane therefore inherits an environment missing exactly the
	// credentials that are only delivered this way, which is why bob still
	// prompted for an API key with the key demonstrably present in the session
	// env (`show-environment` listed it; no bob /proc/<pid>/environ had it).
	//
	// Respawning the pane replaces that stale shell with a fresh one forked by
	// the server after the environment was populated, so it inherits the full
	// set. This is the only ordering that works in all three states the launch
	// path actually hits: cold server, warm server with other sessions, and a
	// session recreated by killSessionForRelaunch. Passing the vars in the
	// environment of the `tmux new-session` client process does NOT work once
	// the server is already running (the server, not the client, forks the
	// pane), and `set-environment -g` before `new-session` cannot run at all on
	// a cold server ("error connecting to <socket>") — both verified.
	//
	// Secrets stay off the command line: respawn-pane takes no arguments here,
	// so nothing is typed into the pane or visible in `ps`.
	respawnArgs := []string{"respawn-pane", "-k", "-t", agent.tmuxSession}
	if err := m.tmuxCmd(agent, respawnArgs...).Run(); err != nil {
		// Non-fatal: the pane still exists with the pre-env shell. Log it so a
		// later "CLI cannot see its credentials" report has a breadcrumb
		// instead of being silent, then continue — a degraded session is still
		// better than refusing to launch the agent at all.
		m.logger.Warn("tmux pane respawn failed; pane shell will not inherit session env (CLI may prompt for credentials)",
			"name", agent.Name, "session", agent.tmuxSession, "error", err)
	}

	m.logger.Info("tmux session created", "name", agent.Name, "session", agent.tmuxSession, "uid", agent.UID, "socket", agent.tmuxSocket)

	// Attach pluk publisher if available — streams structured events
	// from the agent's tmux output to a JSONL log for subscribers.
	if plukPath, err := exec.LookPath("pluk"); err == nil {
		if err := ensurePlukRunDirs(plukRunDir); err != nil {
			m.logger.Warn("pluk run directory setup failed; pluk publisher may be degraded", "error", err)
		}
		backend := agent.Config.Backend
		if agent.BackendOverride != "" {
			backend = agent.BackendOverride
		}
		if backend == "" || m.routableBackend(backend) {
			backend = "claude"
		}
		logFile, err := ensurePlukLogFile(plukRunDir, agent.tmuxSession)
		if err != nil {
			// Non-fatal, and deliberately not a reason to skip the attach: the
			// shell's own `>>` will still create the file. It may land 0600 under
			// a tight umask, which costs peer readability but not the agent's own
			// logging, and that is strictly better than no publisher at all.
			m.logger.Warn("pluk log file setup failed; peer agents may not be able to read this session's log",
				"agent", agent.Name, "error", err)
			logFile = plukSessionLogPath(plukRunDir, agent.tmuxSession)
		}
		pipePaneCmd := plukPipePaneCmd(plukPath, agent.tmuxSession, backend, logFile)
		_ = m.tmuxCmd(agent, "pipe-pane", "-t", agent.tmuxSession, "-o", pipePaneCmd).Run()
		m.logger.Info("pluk publisher attached", "agent", agent.Name, "cli", backend, "log", logFile)
	}

	return nil
}

// tmuxSessionExists probes for a live tmux session. It is a function variable
// solely as a TEST SEAM: production never assigns it, and the default below is
// the real probe.
//
// Without the seam, any test reaching this line executes a REAL `tmux
// has-session` against the developer's or the CI runner's own tmux server.
// That is the same class of hazard as a test shelling to a real kubectl, which
// previously created ~196 stray namespaces on live clusters: the outcome
// depends on machine state rather than on the code under test, so a test can
// pass for the wrong reason. Tests that need to assert WHETHER the probe was
// consulted — SessionMissing's early returns exist precisely so it is not —
// replace this and observe the calls.
var tmuxSessionExists = func(m *Manager, agent *AgentProcess) bool {
	cmd := m.tmuxCmd(agent, "has-session", "-t", agent.tmuxSession)
	return cmd.Run() == nil
}

func (m *Manager) tmuxSessionExistsForAgent(agent *AgentProcess) bool {
	return tmuxSessionExists(m, agent)
}

// tmuxPaneHasCLIForAgent checks for CLI markers using the agent's tmux socket.
// Uses visible pane only (no scrollback) to avoid false positives from stale
// markers left in scroll history after a CLI exits.
func (m *Manager) tmuxPaneHasCLIForAgent(agent *AgentProcess) bool {
	return paneHasCLIMarker(m.captureVisiblePaneForAgent(agent))
}

// samePaneCapture reports whether two pane captures are identical line for
// line. Used to decide whether the agent produced anything since the last
// poll; equality means the pane is static, which is the observable signature
// of an agent that is running but not working.
func samePaneCapture(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// waitForCLIReadyForAgent polls the agent's tmux pane (using its socket)
// until the CLI shows its ready prompt or the timeout expires.
func (m *Manager) waitForCLIReadyForAgent(agent *AgentProcess) bool {
	deadline := time.After(cliReadyTimeout)
	ticker := time.NewTicker(cliReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			if m.tmuxPaneHasCLIForAgent(agent) {
				return true
			}
		}
	}
}

// waitForInputPromptForAgent polls until the CLI shows its input prompt (❯)
// using the agent's tmux socket.

func truncateHead(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func truncateTail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return "..." + string(runes[len(runes)-n:])
}

func (m *Manager) waitForInputPromptForAgent(agent *AgentProcess) bool {
	deadline := time.After(inputPromptTimeout)
	ticker := time.NewTicker(inputPromptPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			m.logger.Warn("prompt timeout — dumping pane",
				"agent", agent.Name,
				"session", agent.tmuxSession)
			output := m.captureTmuxPaneForAgent(agent)
			m.logger.Warn("pane content at timeout",
				"agent", agent.Name,
				"len", len(output),
				"has_goose_ready", strings.Contains(output, "goose is ready"),
				"has_enter", strings.Contains(output, "> Enter to send"),
				"has_arrow", strings.Contains(output, "❯"),
				"has_bob_placeholder", strings.Contains(output, bobInputPlaceholder),
				"has_codex_ready", strings.Contains(output, codexInputPromptMarker),
				"head_500", truncateHead(output, 500), "tail_500", truncateTail(output, 500))
			return false
		case <-ticker.C:
			// A consent/selection screen also contains "❯" but is NOT a
			// ready input prompt — sending a kick there feeds the menu.
			// Check the visible pane only: a dismissed consent screen
			// lingers in the scrollback that captureTmuxPaneForAgent sees.
			if paneShowsConsentScreen(m.captureVisiblePaneForAgent(agent)) {
				continue
			}
			output := m.captureTmuxPaneForAgent(agent)
			if paneShowsInputPrompt(output) {
				return true
			}
		}
	}
}

// captureTmuxPaneForAgent captures pane content using the agent's tmux socket.
// Includes scrollback for diff-based output signal detection.
func (m *Manager) captureTmuxPaneForAgent(agent *AgentProcess) string {
	if m.paneCapture != nil {
		return m.paneCapture(agent)
	}
	cmd := m.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p",
		"-S", fmt.Sprintf("-%d", tmuxCaptureLines))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// CaptureFullLog returns the agent's full retained tmux scrollback for its
// current (latest) session, as plain text. It backs the dashboard's
// "download / view full log" controls (issue #3693): the browser Terminal only
// shows the last screenful, so this pulls the whole retained buffer — from the
// tail up to fullLogCaptureLines — using the SAME per-agent socket + su-exec
// path as every other capture, so it works under per-UID isolation.
//
// The capture is bounded to the current tmux session, so it is scoped to the
// agent's latest run (a restart kills and recreates the session, dropping the
// prior run's scrollback). It is NOT delimited to a run boundary WITHIN a
// long-lived session; when an agent has been kicked repeatedly without a
// restart, the buffer holds multiple kicks' output back to the tmux
// history-limit. That is an accepted v1 limitation — the whole retained
// session is returned.
func (m *Manager) CaptureFullLog(name string) (string, error) {
	m.mu.RLock()
	agent, ok := m.agents[name]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("agent %s not found", name)
	}
	if agent.tmuxSession == "" {
		return "", fmt.Errorf("agent %s has no active session", name)
	}
	return m.captureScrollbackForAgent(agent)
}

// captureVisiblePaneForAgent captures only the visible pane (no scrollback).
func (m *Manager) captureVisiblePaneForAgent(agent *AgentProcess) string {
	if m.visiblePaneCapture != nil {
		return m.visiblePaneCapture(agent)
	}
	cmd := m.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (m *Manager) tmuxSessionHasAttachedClientForAgent(agent *AgentProcess) bool {
	if m.sessionAttached != nil {
		return m.sessionAttached(agent)
	}
	if agent == nil || agent.tmuxSession == "" {
		return true
	}
	out, err := m.tmuxCmd(agent, "display-message", "-p", "-t", agent.tmuxSession, "#{session_attached}").Output()
	if err != nil {
		return true
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return true
	}
	return n > 0
}

// tmuxSendLiteralForAgent sends text using the agent's tmux socket.
func (m *Manager) tmuxSendLiteralForAgent(agent *AgentProcess, text string) {
	if m.sendLiteralForAgent != nil {
		m.sendLiteralForAgent(agent, text)
		return
	}
	_ = m.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "-l", text).Run()
}

// tmuxSendEntersForAgent sends Enter presses using the agent's tmux socket.
func (m *Manager) tmuxSendEntersForAgent(agent *AgentProcess) {
	for i := 0; i < enterCount; i++ {
		// The repeat exists only to make sure a typed line actually RAN — it is
		// insurance against a pane that swallowed the first Enter. Once
		// something is on screen asking a question, further Enters stop being
		// insurance and become an answer.
		//
		// This was not theoretical. Enter #1 runs the launch line, codex boots
		// and renders "✨ Update available!" with "1. Update now" PRE-SELECTED,
		// and Enters #2 and #3 confirmed it — running `npm install -g` as the
		// agent UID, which fails with EACCES and kills the CLI. It recurred on
		// every launch, and it happens within milliseconds, so no poll-based
		// watcher can get there first.
		if i > 0 && paneHasBlockingPrompt(m.captureVisiblePaneForAgent(agent)) {
			m.logger.Info("stopping repeat Enter: a prompt is awaiting an answer",
				"agent", agent.Name, "sent", i)
			return
		}
		_ = m.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "Enter").Run()
		if i < enterCount-1 {
			time.Sleep(enterDelay)
		}
	}
}

// tmuxSendKeysForAgent sends key sequences (C-c, C-u, etc.) using the agent's tmux socket.
func (m *Manager) tmuxSendKeysForAgent(agent *AgentProcess, keys ...string) {
	if m.sendKeysForAgent != nil {
		m.sendKeysForAgent(agent, keys...)
		return
	}
	args := append([]string{"send-keys", "-t", agent.tmuxSession}, keys...)
	_ = m.tmuxCmd(agent, args...).Run()
}

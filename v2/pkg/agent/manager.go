package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

type ProcessState string

const (
	StateIdle     ProcessState = "idle"
	StateRunning  ProcessState = "running"
	StateStopped  ProcessState = "stopped"
	StateFailed   ProcessState = "failed"
	StatePaused   ProcessState = "paused"
)

type KickRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent"`
	Snippet   string    `json:"snippet"`
}

const (
	outputBufferCapacity = 500
	kickHistoryCapacity  = 50
	tmuxCaptureLines     = 2000
	paneCaptureSleep     = 500 * time.Millisecond
	proxyListenPort      = 18443
	proxyCACertPath      = "/data/proxy-ca.pem"
)

type AgentProcess struct {
	Name            string
	ID              string
	Config          config.AgentConfig
	State           ProcessState
	PID             int
	UID             int
	StartedAt       *time.Time
	LastKick        *time.Time
	Paused          bool
	PausedAt        time.Time
	PausedReason    string
	PausedTrigger   string
	PinnedCLI       string
	PinnedModel     string
	ModelOverride   string
	BackendOverride string
	RestartCount    int
	OutputBuffer    *RingBuffer
	lastPaneCapture []string
	paneMu          sync.RWMutex
	KickHistory     []KickRecord
	LastKickMessage    string
	KickRefused        bool
	KickRefusalReason  string
	LaunchedMode       AgentMode
	HasLaunched     bool
	tmuxSession     string
	tmuxSocket      string
	cancel context.CancelFunc
	forceRelaunch       bool
	BootstrapOverride   string // when set, replaces buildBootstrapPrompt output
	LastError           string // captured from bare copilot diagnostic launch
	lastTokenRestart    time.Time // cooldown for auto-restart after token detection
}

// ProjectContext holds project-level config injected into agent boot prompts.
type ProjectContext struct {
	Org        string
	Repos      []string
	ACMMLevel  int
	PRsAllowed bool
	PolicyDir  string
}

type Manager struct {
	agents           map[string]*AgentProcess
	idToName         map[string]string
	mu               sync.RWMutex
	logger           *slog.Logger
	workDir          string
	project          ProjectContext
	copilotAuthToken string
	uidMap           *UIDMap

	inferenceRouteCallback      func(agentName, backend, model string)
	clearInferenceRouteCallback func(agentName string)
}

// IsInferenceBackend returns true if the backend is a self-hosted inference
// backend (vllm, llm-d) rather than a CLI tool.
func IsInferenceBackend(backend string) bool {
	return backend == "vllm" || backend == "llm-d"
}

// SetInferenceCallbacks registers callbacks that the manager uses to
// configure/clear inference routing on the proxy when launching agents.
func (m *Manager) SetInferenceCallbacks(
	setRoute func(agentName, backend, model string),
	clearRoute func(agentName string),
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inferenceRouteCallback = setRoute
	m.clearInferenceRouteCallback = clearRoute
}

func truncateStr(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

// SetACMMLevel updates the cached ACMM level used by agentMode() when
// launching agents. Call this whenever the ACMM level changes.
func (m *Manager) SetACMMLevel(level int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.project.ACMMLevel = level
}

func (m *Manager) GetACMMLevel() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.project.ACMMLevel
}

func NewManager(agents map[string]config.AgentConfig, logger *slog.Logger, project ProjectContext) *Manager {
	workDir := os.Getenv("HIVE_WORK_DIR")
	if workDir == "" {
		workDir = "/data/agents"
	}

	// Save COPILOT_GITHUB_TOKEN for explicit injection via tmux set-environment.
	// The token stays in the process env so all agents can authenticate for AI
	// completions; write access is gated by --enable-all-github-mcp-tools flag.
	copilotToken := os.Getenv("COPILOT_GITHUB_TOKEN")

	var uidMap *UIDMap
	if loaded, err := LoadUIDMap(UIDMapPath); err == nil {
		uidMap = loaded
		logger.Info("UID map loaded", "agents", len(uidMap.Agents), "iptables", uidMap.IptablesActive)
	} else {
		logger.Info("no UID map found, agents will share dev UID", "path", UIDMapPath)
	}

	m := &Manager{
		agents:           make(map[string]*AgentProcess),
		idToName:         make(map[string]string),
		logger:           logger,
		workDir:          workDir,
		project:          project,
		copilotAuthToken: copilotToken,
		uidMap:           uidMap,
	}

	for name, cfg := range agents {
		agentID := cfg.ID
		if agentID == "" {
			agentID = name
		}
		agentUID := 0
		tmuxSocket := ""
		if uidMap != nil {
			agentUID = uidMap.LookupByName(name)
			if agentUID > 0 {
				tmuxSocket = "hive-" + name
			}
		}
		m.agents[name] = &AgentProcess{
			Name:         name,
			ID:           agentID,
			Config:       cfg,
			State:        StateStopped,
			UID:          agentUID,
			OutputBuffer: NewRingBuffer(outputBufferCapacity),
			tmuxSession:  "hive-" + name,
			tmuxSocket:   tmuxSocket,
		}
		m.idToName[agentID] = name
	}

	return m
}

// ResolveAgent returns the YAML key (name) for a given name or ID.
// If the input matches neither, it returns the input unchanged (callers
// will get a "not found" error from the specific method).
func (m *Manager) ResolveAgent(nameOrID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.agents[nameOrID]; ok {
		return nameOrID
	}
	if name, ok := m.idToName[nameOrID]; ok {
		return name
	}
	return nameOrID
}

func (m *Manager) Start(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if agent.State == StateRunning {
		return fmt.Errorf("agent %s already running", name)
	}

	m.sanitizeGitRemotes(agent)

	if err := m.ensureTmuxSession(agent); err != nil {
		return err
	}

	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}
	if agent.Paused {
		if IsInferenceBackend(backend) {
			agent.Paused = false
			m.logger.Info("auto-unpaused inference agent", "name", agent.Name, "backend", backend)
		} else {
			agent.State = StatePaused
			return nil
		}
	}

	return m.launchInTmux(ctx, agent)
}

// tmuxBaseArgs returns the base tmux command args for an agent. When the agent
// has a per-agent tmux socket (UID isolation), it returns ["tmux", "-L", socketName].
// Otherwise it returns ["tmux"] for the shared tmux server.
func (m *Manager) tmuxBaseArgs(agent *AgentProcess) []string {
	if agent.tmuxSocket != "" {
		return []string{"tmux", "-L", agent.tmuxSocket}
	}
	return []string{"tmux"}
}

func (m *Manager) tmuxCmd(agent *AgentProcess, args ...string) *exec.Cmd {
	base := m.tmuxBaseArgs(agent)
	tmuxArgs := append(base[1:], args...)
	if agent.UID > 0 {
		agentUser := fmt.Sprintf("hive-%s", agent.Name)
		suExecArgs := append([]string{agentUser, base[0]}, tmuxArgs...)
		return exec.Command("su-exec", suExecArgs...)
	}
	return exec.Command(base[0], tmuxArgs...)
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
		agentUser := fmt.Sprintf("hive-%s", agent.Name)
		suExecArgs := []string{"su-exec", agentUser}
		tmuxArgs := append(m.tmuxBaseArgs(agent), "new-session", "-d", "-s", agent.tmuxSession, "-c", agentDir)
		cmd = exec.Command(suExecArgs[0], append(suExecArgs[1:], tmuxArgs...)...)
	} else {
		cmd = exec.Command("tmux", "new-session", "-d", "-s", agent.tmuxSession, "-c", agentDir)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("creating tmux session for %s: %w", agent.Name, err)
	}

	// tmux creates /tmp/tmux-{uid}/ with mode 700; ttyd runs as dev (uid 1001,
	// node group) and needs to traverse into these dirs to attach to sockets.
	// os.Chmod doesn't work here because the Go binary runs as dev, not as the
	// agent user who owns the directory. Use su-exec to chmod as the agent.
	if agent.UID > 0 {
		tmuxDir := fmt.Sprintf("/tmp/tmux-%d", agent.UID)
		agentUser := fmt.Sprintf("hive-%s", agent.Name)
		_ = exec.Command("su-exec", agentUser, "chmod", "710", tmuxDir).Run()
	}

	// Set per-session env vars via tmux set-environment (raw values, no shell quoting).
	for _, p := range m.agentEnvPairs(agent) {
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, p.Key, p.Value).Run()
	}
	// Strip gh/git tokens from advisory agent sessions.
	if !m.agentMode(agent).CanPush() {
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", "GH_TOKEN").Run()
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", "GITHUB_TOKEN").Run()
	}

	m.logger.Info("tmux session created", "name", agent.Name, "session", agent.tmuxSession, "uid", agent.UID, "socket", agent.tmuxSocket)

	// Attach pluk publisher if available — streams structured events
	// from the agent's tmux output to a JSONL log for subscribers.
	if plukPath, err := exec.LookPath("pluk-publish"); err == nil {
		_ = os.MkdirAll("/var/run/pluk/logs", 0o1777)
		_ = os.MkdirAll("/var/run/pluk/commands", 0o1777)
		backend := agent.Config.Backend
		if agent.BackendOverride != "" {
			backend = agent.BackendOverride
		}
		if backend == "" || IsInferenceBackend(backend) {
			backend = "claude"
		}
		pipePaneCmd := fmt.Sprintf("%s --session %s --cli %s", plukPath, agent.tmuxSession, backend)
		_ = m.tmuxCmd(agent, "pipe-pane", "-t", agent.tmuxSession, "-o", pipePaneCmd).Run()
		m.logger.Info("pluk publisher attached", "agent", agent.Name, "cli", backend)
	}

	return nil
}

func (m *Manager) tmuxSessionExists(session string) bool {
	cmd := exec.Command("tmux", "has-session", "-t", session)
	return cmd.Run() == nil
}

func (m *Manager) tmuxSessionExistsForAgent(agent *AgentProcess) bool {
	cmd := m.tmuxCmd(agent, "has-session", "-t", agent.tmuxSession)
	return cmd.Run() == nil
}

// cliPaneMarkers are strings that appear in a tmux pane when a CLI (claude,
// copilot, gemini, goose, aider) is running. A bare bash prompt has none of
// these. Checking pane content is more reliable than inspecting /proc/comm
// because CLIs may run as node, python, or other interpreters whose process
// name doesn't match the CLI binary.
var cliPaneMarkers = []string{
	"❯",
	"esc cancel",
	"/ commands",
	"? help",
	"Claude",
	"Copilot",
	"Gemini",
	"goose",
}

// tmuxPaneHasCLI reports whether a CLI is running in the pane by inspecting
// the visible pane content for known CLI UI markers.
func (m *Manager) tmuxPaneHasCLI(session string) bool {
	output := m.captureTmuxPane(session)
	if output == "" {
		return false
	}
	for _, marker := range cliPaneMarkers {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

// tmuxPaneHasCLIForAgent checks for CLI markers using the agent's tmux socket.
func (m *Manager) tmuxPaneHasCLIForAgent(agent *AgentProcess) bool {
	output := m.captureTmuxPaneForAgent(agent)
	if output == "" {
		return false
	}
	for _, marker := range cliPaneMarkers {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}


func (m *Manager) launchInTmux(ctx context.Context, agent *AgentProcess) error {
	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}

	binary, err := backendBinary(backend)
	if err != nil {
		agent.State = StateFailed
		m.logger.Warn("backend binary not found", "name", agent.Name, "backend", backend, "error", err)
		return nil
	}

	launchCmd := binary
	model := agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	model = normalizeModelName(model)

	bootstrapPrompt := agent.BootstrapOverride
	if bootstrapPrompt != "" {
		m.logger.Info("using bootstrap override", "agent", agent.Name, "len", len(bootstrapPrompt))
		agent.BootstrapOverride = ""
	} else {
		bootstrapPrompt = m.buildBootstrapPrompt(agent)
	}

	mode := m.agentMode(agent)
	agent.LaunchedMode = mode
	agent.HasLaunched = true

	// Inference backends (vllm, llm-d) use Claude Code as the CLI tool
	// and route API traffic through the proxy to the self-hosted endpoint.
	isInference := IsInferenceBackend(backend)
	if isInference {
		binary = "claude"
		m.ensureClaudeSettings()
		if m.inferenceRouteCallback != nil {
			m.inferenceRouteCallback(agent.Name, backend, model)
		}
		backend = "claude"
	} else if m.clearInferenceRouteCallback != nil {
		m.clearInferenceRouteCallback(agent.Name)
	}

	switch backend {
	case "claude":
		bareFlag := ""
		if isInference {
			bareFlag = fmt.Sprintf(" --bare --settings %s", claudeInferenceSettingsPath)
		}
		base := fmt.Sprintf("%s --model %s --dangerously-skip-permissions%s", binary, model, bareFlag)
		switch {
		case mode >= ModeIssuesAndPRs:
			launchCmd = base
		case mode == ModeIssuesOnly:
			launchCmd = base +
				" --disallowed-tools 'mcp__github__create_pull_request'" +
				" --disallowed-tools 'mcp__github__merge_pull_request'"
		default:
			launchCmd = base +
				" --disallowed-tools 'mcp__github__create_pull_request'" +
				" --disallowed-tools 'mcp__github__create_issue'" +
				" --disallowed-tools 'mcp__github__update_issue'" +
				" --disallowed-tools 'mcp__github__merge_pull_request'"
		}
	case "copilot":
		switch {
		case mode >= ModeIssuesAndPRs:
			launchCmd = fmt.Sprintf("%s --model %s --allow-all --enable-all-github-mcp-tools",
				binary, model)
		case mode == ModeIssuesOnly:
			launchCmd = fmt.Sprintf("%s --model %s --allow-all --enable-all-github-mcp-tools"+
				" --deny-tool='github(create_pull_request)'"+
				" --deny-tool='github(merge_pull_request)'",
				binary, model)
		default:
			launchCmd = fmt.Sprintf("%s --model %s --allow-all"+
				" --deny-tool='github(create_pull_request)'"+
				" --deny-tool='github(create_issue)'"+
				" --deny-tool='github(update_issue)'"+
				" --deny-tool='github(merge_pull_request)'",
				binary, model)
		}
	case "gemini":
		launchCmd = fmt.Sprintf("%s --model %s", binary, model)
	case "goose":
		launchCmd = fmt.Sprintf("%s run -s", binary)
		if model != "" {
			launchCmd = fmt.Sprintf("%s --model %s", launchCmd, model)
		}
	case "bob":
		launchCmd = binary
	default:
		launchCmd = binary
	}

	if bootstrapPrompt == "" && isInference {
		bootstrapPrompt = "You are an AI agent. Await further instructions."
	}

	if bootstrapPrompt != "" {
		now := time.Now()
		agent.LastKick = &now
		agent.LastKickMessage = bootstrapPrompt
		snippet := bootstrapPrompt
		const maxBootstrapSnippet = 200
		snippet = truncateStr(snippet, maxBootstrapSnippet)
		agent.KickHistory = append(agent.KickHistory, KickRecord{Timestamp: now, Agent: agent.Name, Snippet: snippet})
		m.logger.Info("audit: agent kicked",
			"name", agent.Name,
			"message_len", len(bootstrapPrompt),
			"preview", snippet,
			"trigger", "startup",
		)

		promptFile := fmt.Sprintf("/tmp/.hive-bootstrap-%s.txt", agent.Name)
		if err := os.WriteFile(promptFile, []byte(bootstrapPrompt), 0o644); err != nil {
			m.logger.Warn("failed to write bootstrap prompt", "name", agent.Name, "error", err)
		} else {
			switch backend {
			case "copilot":
				launchCmd += fmt.Sprintf(" -i \"$(cat %s)\"", promptFile)
			case "claude":
				// Write a launcher script instead of using $(cat) in send-keys.
				// $(cat file) fails when the tmux shell hasn't fully initialized.
				// Use -- to separate options from the positional prompt argument,
				// otherwise --disallowed-tools consumes the prompt as tool names.
				launcherFile := fmt.Sprintf("/tmp/.hive-launch-%s.sh", agent.Name)
				launcherContent := fmt.Sprintf("#!/bin/sh\nexec %s -- \"$(cat %s)\"\n", launchCmd, promptFile)
				if err := os.WriteFile(launcherFile, []byte(launcherContent), 0o755); err != nil {
					m.logger.Warn("failed to write launcher script", "error", err)
					launchCmd += fmt.Sprintf(" \"$(cat %s)\"", promptFile)
				} else {
					launchCmd = launcherFile
				}
			case "gemini":
				launchCmd += fmt.Sprintf(" -i \"$(cat %s)\"", promptFile)
			case "goose":
				launchCmd += fmt.Sprintf(" --text \"$(cat %s)\"", promptFile)
			}
		}
	}

	// Goose 1.37 requires --instructions or --text to stay interactive.
	// Without bootstrap, use a minimal --text prompt so goose output is
	// visible to tmux capture-pane (--instructions - uses hidden TUI).
	if backend == "goose" && bootstrapPrompt == "" {
		minimalPrompt := fmt.Sprintf("/tmp/.hive-bootstrap-%s.txt", agent.Name)
		os.WriteFile(minimalPrompt, []byte("You are an AI agent. Wait for instructions from the supervisor."), 0o644)
		launchCmd += fmt.Sprintf(" --text \"$(cat %s)\"", minimalPrompt)
	}

	if !agent.forceRelaunch && m.tmuxPaneHasCLIForAgent(agent) {
		m.logger.Info("CLI already running in tmux pane, skipping launch", "name", agent.Name, "session", agent.tmuxSession)
		now := time.Now()
		agent.State = StateRunning
		agent.StartedAt = &now

		agentCtx, cancel := context.WithCancel(ctx)
		agent.cancel = cancel
		go m.pollTmuxOutputForAgent(agent, agentCtx)

		if backend == "copilot" {
			go m.watchForTrustPromptForAgent(agent, agentCtx)
		}
		return nil
	}
	agent.forceRelaunch = false

	m.fixSharedConfigPerms(agent)

	envCmd := m.buildEnvPrefix(agent)
	fullCmd := envCmd + launchCmd

	m.tmuxSendLiteralForAgent(agent, fullCmd)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	if isInference {
		go m.dismissInferencePrompts(agent)
	}

	now := time.Now()
	agent.State = StateRunning
	agent.StartedAt = &now
	m.logger.Info("audit: agent started",
		"name", agent.Name,
		"backend", backend,
		"model", model,
		"mode", mode.String(),
		"session", agent.tmuxSession,
	)

	agentCtx, cancel := context.WithCancel(ctx)
	agent.cancel = cancel
	go m.pollTmuxOutputForAgent(agent, agentCtx)

	if backend == "copilot" {
		go m.watchForTrustPromptForAgent(agent, agentCtx)
	}

	return nil
}

// pollTmuxOutputForAgent is pollTmuxOutput using the agent's tmux socket.
func (m *Manager) pollTmuxOutputForAgent(agent *AgentProcess, ctx context.Context) {
	const pollInterval = 3 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var prevLines []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Scrollback capture for dashboard display + ring buffer diff
			output := m.captureTmuxPaneForAgent(agent)
			if output == "" {
				continue
			}
			var filtered []string
			for _, line := range strings.Split(output, "\n") {
				trimmed := strings.TrimRight(line, " \t")
				if trimmed != "" {
					filtered = append(filtered, trimmed)
				}
			}
			if len(filtered) == 0 {
				continue
			}

			agent.paneMu.Lock()
			agent.lastPaneCapture = filtered
			agent.paneMu.Unlock()

			// Auto-restart agents stuck on the login prompt when a valid
			// token exists in the shared config.json. This handles the case
			// where a user authenticates via one agent's terminal and other
			// agents don't pick up the new token automatically.
			if paneShowsLoginPrompt(filtered) && configHasTokens() {
				sinceLastRestart := time.Since(agent.lastTokenRestart).Seconds()
				if sinceLastRestart >= float64(tokenRestartCooldownSec) {
					m.logger.Info("auto-restarting agent after token detected in shared config",
						"agent", agent.Name,
						"cooldown_elapsed_sec", int(sinceLastRestart),
					)
					agent.lastTokenRestart = time.Now()
					go func() {
						if err := m.Restart(ctx, agent.Name); err != nil {
							m.logger.Warn("token-triggered restart failed",
								"agent", agent.Name,
								"error", err,
							)
						}
					}()
					return // stop polling; Restart will spawn a new goroutine
				}
			}

			// Detect copilot hung: if running long enough with no CLI prompt,
			// launch bare `copilot` to diagnose the error. Only clear the
			// token if the diagnostic shows an auth error.
			// Skip for inference backends — they use Claude -p mode (non-interactive).
			if agent.Config.Backend == "copilot" && !IsInferenceBackend(agent.BackendOverride) && agent.StartedAt != nil &&
				time.Since(*agent.StartedAt).Seconds() >= expiredTokenHangTimeoutSec &&
				!paneShowsCLIReady(filtered) {
				sinceLastRestart := time.Since(agent.lastTokenRestart).Seconds()
				if sinceLastRestart >= float64(tokenRestartCooldownSec) {
					m.logger.Warn("copilot hung with no CLI prompt, running diagnostic",
						"agent", agent.Name,
						"uptime_sec", int(time.Since(*agent.StartedAt).Seconds()),
					)
					agent.lastTokenRestart = time.Now()
					go m.runCopilotDiagnostic(ctx, agent)
					return
				}
			}

			if prevLines == nil {
				if agent.OutputBuffer.Count() == 0 {
					for _, l := range filtered {
						if !isBufferNoise(l) {
							agent.OutputBuffer.Write(l)
						}
					}
				}
				prevLines = filtered
				continue
			}
			newLines := diffNewLines(prevLines, filtered)
			for _, l := range newLines {
				if !isBufferNoise(l) {
					agent.OutputBuffer.Write(l)
				}
				m.logOutputSignals(agent.Name, l)
				if !agent.KickRefused {
					m.checkKickRefusal(agent, l)
				}
			}
			prevLines = filtered
		}
	}
}

// watchForTrustPromptForAgent monitors a tmux session for Copilot's "Confirm folder trust"
// prompt using the agent's tmux socket.
func (m *Manager) watchForTrustPromptForAgent(agent *AgentProcess, ctx context.Context) {
	const (
		trustPollInterval = 2 * time.Second
		trustMaxWait      = 120 * time.Second
		trustCooldown     = 3 * time.Second
	)
	deadline := time.After(trustMaxWait)
	ticker := time.NewTicker(trustPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			output := m.captureTmuxPaneForAgent(agent)
			if strings.Contains(output, "Confirm folder trust") || strings.Contains(output, "Do you trust the files") {
				time.Sleep(paneCaptureSleep)
				m.tmuxSendKeysForAgent(agent, "2")
				time.Sleep(enterDelay)
				m.tmuxSendKeysForAgent(agent, "Enter")
				m.logger.Info("auto-answered folder trust prompt", "agent", agent.Name)
				time.Sleep(trustCooldown)
			}
		}
	}
}

// watchForTrustPrompt monitors a tmux session for Copilot's "Confirm folder trust"
// prompt and auto-selects "Yes, and remember for future sessions" (option 2).
func (m *Manager) watchForTrustPrompt(session string, ctx context.Context) {
	const (
		trustPollInterval = 2 * time.Second
		trustMaxWait      = 120 * time.Second
		trustCooldown     = 3 * time.Second
	)
	deadline := time.After(trustMaxWait)
	ticker := time.NewTicker(trustPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			output := m.captureTmuxPane(session)
			if strings.Contains(output, "Confirm folder trust") || strings.Contains(output, "Do you trust the files") {
				time.Sleep(paneCaptureSleep)
				_ = exec.Command("tmux", "send-keys", "-t", session, "2").Run()
				time.Sleep(enterDelay)
				_ = exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
				m.logger.Info("auto-answered folder trust prompt", "session", session)
				time.Sleep(trustCooldown)
			}
		}
	}
}

// acmmLevelNames maps ACMM level numbers to human-readable names.
var acmmLevelNames = map[int]string{
	1: "Idea",
	2: "Development",
	3: "CI/CD",
	4: "Managed",
	5: "Guarded Autonomy",
	6: "Full Autonomy",
}

func (m *Manager) buildBootstrapPrompt(agent *AgentProcess) string {
	// Look for policy files in priority order.
	// For advisory agents (non-quality at L3+), prefer <agent>-advisory.md
	// over <agent>.md so they get the correct advisory-only instructions.
	policyDir := m.project.PolicyDir
	if policyDir == "" {
		policyDir = "/data/policies/agents"
	}
	policiesRoot := filepath.Dir(policyDir)
	if policiesRoot == "." || policiesRoot == "" {
		policiesRoot = "/data/policies"
	}
	mode := m.agentMode(agent)
	suffix := mode.SuffixForLevel(m.project.ACMMLevel)

	var paths []string
	if agent.Config.KickTemplate != "" {
		paths = append(paths, fmt.Sprintf("%s/%s", policyDir, agent.Config.KickTemplate))
	}
	paths = append(paths,
		fmt.Sprintf("%s/%s%s.md", policyDir, agent.Name, suffix),
		fmt.Sprintf("%s/%s.md", policyDir, agent.Name),
		fmt.Sprintf("/data/agents/%s/CLAUDE.md", agent.Name),
		filepath.Join(policiesRoot, "examples", "agents", agent.Name+suffix+".md"),
		filepath.Join(policiesRoot, "examples", "agents", agent.Name+".md"),
		fmt.Sprintf("/opt/hive/examples/agents/%s.md", agent.Name),
	)
	// No boot prompt — the governor's first eval cycle (10s after startup)
	// kicks all due agents via BuildKickMessages with fully substituted
	// templates. Sending a boot prompt here caused unsubstituted ${ISSUE_LIST}
	// and other vars to reach the agent.
	return ""
}

// findACMMFragments returns paths to ACMM policy files the agent should read.
// Order: base.md (shared rules) then l<N>.md (level-specific).
func (m *Manager) findACMMFragments() []string {
	level := m.project.ACMMLevel
	if level <= 0 {
		return nil
	}

	// Look for ACMM fragments in the policies directory first, then fallback to baked-in paths.
	policiesRoot := filepath.Dir(m.project.PolicyDir)
	if policiesRoot == "." || policiesRoot == "" {
		policiesRoot = "/data/policies"
	}

	acmmDirs := []string{
		filepath.Join(policiesRoot, "examples", "acmm"),
		"/data/policies/examples/acmm",
		"/opt/hive/examples/acmm",
	}

	var acmmDir string
	for _, d := range acmmDirs {
		if _, err := os.Stat(d); err == nil {
			acmmDir = d
			break
		}
	}
	if acmmDir == "" {
		return nil
	}

	var files []string
	basePath := filepath.Join(acmmDir, "base.md")
	if _, err := os.Stat(basePath); err == nil {
		files = append(files, basePath)
	}
	levelPath := filepath.Join(acmmDir, fmt.Sprintf("l%d.md", level))
	if _, err := os.Stat(levelPath); err == nil {
		files = append(files, levelPath)
	}
	return files
}


func (m *Manager) buildProjectPreamble(agent *AgentProcess) string {
	p := m.project
	if p.Org == "" || len(p.Repos) == 0 {
		return ""
	}

	repos := make([]string, len(p.Repos))
	for i, r := range p.Repos {
		repos[i] = fmt.Sprintf("%s/%s", p.Org, r)
	}

	levelName := acmmLevelNames[p.ACMMLevel]
	if levelName == "" {
		levelName = fmt.Sprintf("Level %d", p.ACMMLevel)
	}

	mode := m.agentMode(agent)
	var prPolicy string
	if !p.PRsAllowed {
		prPolicy = "PRs NOT allowed (project-wide)."
	} else {
		switch mode {
		case ModeAdvisory:
			prPolicy = "\U0001F4DD Advisory only — beads, no issues/PRs."
		case ModeIssuesOnly:
			prPolicy = "\U0001F3AB Issues ONLY — can open issues. NO PRs."
		case ModeIssuesAndPRs:
			if p.ACMMLevel == 5 {
				prPolicy = "\U0001F527 Issues + PRs allowed (hold-labeled, human merges)."
			} else {
				prPolicy = "\U0001F527 Issues + PRs allowed."
			}
		case ModeIssuesPRsMerge:
			prPolicy = "\U0001F680 Issues + PRs + auto-merge on green CI."
		default:
			prPolicy = "\U0001F4DD Advisory only — beads, no issues/PRs."
		}
	}

	return fmt.Sprintf("[PROJECT] Org: %s | Repos: %s | ACMM: L%d (%s) | Mode: %s %s | %s ",
		p.Org, strings.Join(repos, ", "), p.ACMMLevel, levelName,
		mode.Emoji(), mode.String(), prPolicy)
}

const metricsCachePath = "/data/metrics/agent-metrics-cache.json"

func (m *Manager) readCoveragePreamble() string {
	data, err := os.ReadFile(metricsCachePath)
	if err != nil {
		return ""
	}
	var metrics map[string]map[string]json.Number
	if err := json.Unmarshal(data, &metrics); err != nil {
		return ""
	}
	ci, ok := metrics["ci-maintainer"]
	if !ok {
		return ""
	}
	cov, err := ci["coverage"].Int64()
	if err != nil {
		return ""
	}
	target, err := ci["coverageTarget"].Int64()
	if err != nil {
		target = 91
	}
	return fmt.Sprintf("[COVERAGE] Current: %d%% | Target: %d%%.", cov, target)
}

// shellEnvVar formats KEY='value' with single-quoting so values containing
// spaces, parentheses, or other shell metacharacters are safe in inline env
// var assignments sent to tmux via send-keys.
func shellEnvVar(key, value string) string {
	quoted := strings.ReplaceAll(value, "'", "'\"'\"'")
	return fmt.Sprintf("%s='%s'", key, quoted)
}

func (m *Manager) buildEnvPrefix(agent *AgentProcess) string {
	pairs := m.agentEnvPairs(agent)
	var parts []string
	for _, p := range pairs {
		if p.Secret {
			continue
		}
		parts = append(parts, shellEnvVar(p.Key, p.Value))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ") + " "
}

// outputSignalPatterns are substrings in agent output that indicate meaningful
// events worth logging. Each pattern maps to a short event label.
var outputSignalPatterns = map[string]string{
	"[HEARTBEAT]":  "heartbeat",
	"[STATUS]":     "status",
	"[FINDING]":    "finding",
	"[COMPLETE]":   "task_complete",
	"[ERROR]":      "agent_error",
	"PASS":         "pass_marker",
	"git commit":   "git_commit",
	"git checkout": "git_branch",
	"git push":     "git_push",
	"created file": "file_created",
	"Wrote":        "file_written",
	"test:":        "test_activity",
	"FAIL":         "test_failure",
	"coverage":     "coverage_report",
}

func (m *Manager) pollTmuxOutput(name, session string, buf *RingBuffer, ctx context.Context) {
	const pollInterval = 3 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var prevLines []string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			output := m.captureTmuxPane(session)
			if output == "" {
				continue
			}
			var filtered []string
			for _, line := range strings.Split(output, "\n") {
				trimmed := strings.TrimRight(line, " \t")
				if trimmed != "" {
					filtered = append(filtered, trimmed)
				}
			}
			if len(filtered) == 0 {
				continue
			}
			if prevLines == nil {
				// First capture after (re)start — seed prevLines so subsequent
				// diffs work. Only write to the buffer if it's empty (fresh
				// start); skip if it already has content (restart) to avoid
				// duplicating the scrollback.
				if buf.Count() == 0 {
					for _, l := range filtered {
						buf.Write(l)
					}
				}
				prevLines = filtered
				continue
			}
			newLines := diffNewLines(prevLines, filtered)
			for _, l := range newLines {
				buf.Write(l)
				m.logOutputSignals(name, l)
			}
			prevLines = filtered
		}
	}
}

// logOutputSignals checks a line of agent output for meaningful patterns
// and emits a structured slog entry for each match.
func (m *Manager) logOutputSignals(agent, line string) {
	for pattern, event := range outputSignalPatterns {
		if strings.Contains(line, pattern) {
			preview := line
			const maxPreviewLen = 200
			preview = truncateStr(preview, maxPreviewLen)
			m.logger.Info("agent output signal",
				"agent", agent,
				"event", event,
				"content", preview,
			)
			return
		}
	}
}

var kickRefusalPatterns = []string{
	"I'm declining to execute",
	"I'm declining this",
	"prompt injection",
	"I won't act on bulk automated",
	"credential handling concern",
	"autonomous orchestration prompt",
	"I shouldn't follow autonomously",
	"characteristic of a prompt injection attack",
}

func (m *Manager) checkKickRefusal(agent *AgentProcess, line string) {
	lower := strings.ToLower(line)
	for _, pattern := range kickRefusalPatterns {
		if strings.Contains(lower, strings.ToLower(pattern)) {
			agent.KickRefused = true
			const maxReasonRunes = 200
			reason := line
			if runes := []rune(reason); len(runes) > maxReasonRunes {
				reason = string(runes[:maxReasonRunes])
			}
			agent.KickRefusalReason = reason
			m.logger.Warn("agent refused kick",
				"agent", agent.Name,
				"pattern", pattern,
				"line", reason,
			)
			return
		}
	}
}

func diffNewLines(prev, curr []string) []string {
	if len(prev) == 0 {
		return curr
	}
	overlap := findOverlap(prev, curr)
	if overlap >= 0 {
		return curr[overlap:]
	}
	return curr
}

var spinnerReplacer = strings.NewReplacer(
	"◐", "○", "◑", "○", "◒", "○", "◓", "○",
	"◎", "○", "◉", "○", "●", "○",
)

var creditsRe = regexp.MustCompile(`AI Credits: [\d.]+`)

func normalizeLine(s string) string {
	s = strings.TrimRight(s, " \t")
	s = spinnerReplacer.Replace(s)
	s = creditsRe.ReplaceAllString(s, "AI Credits: _")
	return s
}

func findOverlap(prev, curr []string) int {
	maxTail := len(prev)
	if maxTail > len(curr) {
		maxTail = len(curr)
	}
	for tail := maxTail; tail > 0; tail-- {
		match := true
		for i := range tail {
			if normalizeLine(prev[len(prev)-tail+i]) != normalizeLine(curr[i]) {
				match = false
				break
			}
		}
		if match {
			return tail
		}
	}
	return -1
}


// waitForCLIReady polls the tmux pane until the CLI shows its ready prompt
// or the timeout expires. Returns true if the CLI became ready.
func (m *Manager) waitForCLIReady(session string) bool {
	deadline := time.After(cliReadyTimeout)
	ticker := time.NewTicker(cliReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			if m.tmuxPaneHasCLI(session) {
				return true
			}
		}
	}
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
				"head_500", truncateHead(output, 500), "tail_500", truncateTail(output, 500))
			return false
		case <-ticker.C:
			output := m.captureTmuxPaneForAgent(agent)
			if strings.Contains(output, "❯") || strings.Contains(output, "goose is ready") || strings.Contains(output, "> Enter to send") || strings.Contains(output, "\n>\n") {
				return true
			}
		}
	}
}

// waitForInputPrompt polls until the CLI shows its input prompt (❯),
// indicating it is ready to accept a kick. This is stricter than
// waitForCLIReady which matches any CLI marker (including trust prompts).
func (m *Manager) waitForInputPrompt(session string) bool {
	deadline := time.After(inputPromptTimeout)
	ticker := time.NewTicker(inputPromptPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			output := m.captureTmuxPane(session)
			if strings.Contains(output, "❯") || strings.Contains(output, "goose is ready") || strings.Contains(output, "> Enter to send") || strings.Contains(output, "\n>\n") {
				return true
			}
		}
	}
}

func (m *Manager) captureTmuxPane(session string) string {
	cmd := exec.Command("tmux", "capture-pane", "-t", session, "-p",
		"-S", fmt.Sprintf("-%d", tmuxCaptureLines))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// captureTmuxPaneForAgent captures pane content using the agent's tmux socket.
// Includes scrollback for diff-based output signal detection.
func (m *Manager) captureTmuxPaneForAgent(agent *AgentProcess) string {
	cmd := m.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p",
		"-S", fmt.Sprintf("-%d", tmuxCaptureLines))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// captureVisiblePaneForAgent captures only the visible pane (no scrollback).
func (m *Manager) captureVisiblePaneForAgent(agent *AgentProcess) string {
	cmd := m.tmuxCmd(agent, "capture-pane", "-t", agent.tmuxSession, "-p")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (m *Manager) Stop(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if agent.State != StateRunning {
		return nil
	}

	if agent.cancel != nil {
		agent.cancel()
	}

	m.tmuxSendKeysForAgent(agent, "C-c", "")

	agent.State = StateStopped
	m.logger.Info("audit: agent stopped", "name", name)

	return nil
}

func (m *Manager) AddAgent(name string, cfg config.AgentConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.agents[name]; exists {
		return
	}

	agentID := cfg.ID
	if agentID == "" {
		agentID = name
	}
	agentUID := 0
	tmuxSocket := ""
	if m.uidMap != nil {
		agentUID = m.uidMap.AllocateUID(name)
		if agentUID > 0 {
			tmuxSocket = "hive-" + name
		}
		_ = m.uidMap.Save(UIDMapPath)
	}
	m.agents[name] = &AgentProcess{
		Name:         name,
		ID:           agentID,
		Config:       cfg,
		State:        StateStopped,
		UID:          agentUID,
		OutputBuffer: NewRingBuffer(outputBufferCapacity),
		tmuxSession:  "hive-" + name,
		tmuxSocket:   tmuxSocket,
	}
	m.idToName[agentID] = name
	m.logger.Info("audit: agent added", "name", name, "id", agentID, "uid", agentUID)
}

// UpdateConfig updates the stored config for a running agent process so that
// status builders (which read from AgentProcess.Config) reflect changes made
// via the config dialog (which writes to the global Config.Agents map).
func (m *Manager) UpdateConfig(name string, cfg config.AgentConfig) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.Config = cfg
	return nil
}

func (m *Manager) RemoveAgent(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return
	}

	if agent.cancel != nil {
		agent.cancel()
	}

	delete(m.idToName, agent.ID)
	delete(m.agents, name)
	m.logger.Info("audit: agent removed", "name", name, "id", agent.ID)
}

// CheckAndRestartCrashedAgents checks all running agents for crashed CLI
// processes (bare shell prompt with no child process) and restarts them.
// Returns the names of agents that were successfully restarted so the
// caller can send them a kick with their prompt template.
func (m *Manager) CheckAndRestartCrashedAgents(ctx context.Context) []string {
	m.mu.RLock()
	var crashed []string
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
			)
			crashed = append(crashed, name)
			continue
		}
		if !m.tmuxPaneHasCLIForAgent(agent) {
			var uptimeSeconds float64
			if agent.StartedAt != nil {
				uptimeSeconds = time.Since(*agent.StartedAt).Seconds()
			}
			m.logger.Warn("agent CLI crashed (bare shell detected)",
				"name", name,
				"session", agent.tmuxSession,
				"restart_count", agent.RestartCount,
				"uptime_seconds", int(uptimeSeconds),
			)
			crashed = append(crashed, name)
		}
	}
	m.mu.RUnlock()

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

func (m *Manager) SendKick(name string, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if agent.State != StateRunning {
		return fmt.Errorf("agent %s not running", name)
	}

	if !m.tmuxSessionExistsForAgent(agent) {
		return fmt.Errorf("tmux session %s not found", agent.tmuxSession)
	}

	// Detect crashed CLI and restart before sending kick
	if !m.tmuxPaneHasCLIForAgent(agent) {
		m.logger.Warn("agent CLI crashed, restarting before kick", "name", name)
		m.mu.Unlock()
		if err := m.Restart(context.Background(), name); err != nil {
			m.mu.Lock()
			return fmt.Errorf("failed to restart crashed agent %s: %w", name, err)
		}
		if !m.waitForCLIReadyForAgent(agent) {
			m.mu.Lock()
			return fmt.Errorf("agent %s CLI did not become ready after restart", name)
		}
		m.mu.Lock()
		agent, ok = m.agents[name]
		if !ok {
			return fmt.Errorf("agent %s disappeared after restart", name)
		}
	}

	// Wait for the input prompt (❯) before sending — the CLI may be
	// showing a trust prompt or still initializing even though
	// tmuxPaneHasCLI matched a broad marker like "Copilot".
	m.mu.Unlock()
	if !m.waitForInputPromptForAgent(agent) {
		m.mu.Lock()
		return fmt.Errorf("agent %s CLI did not reach input prompt", name)
	}
	m.mu.Lock()
	agent, ok = m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s disappeared while waiting for input prompt", name)
	}

	// Clear stale input before kick (Ctrl+C then Ctrl+U).
	// Goose 1.37 exits on ^C — skip clear for goose backend.
	if agent.Config.Backend != "goose" && agent.BackendOverride != "goose" {
		m.tmuxSendKeysForAgent(agent, "C-c")
		time.Sleep(staleCheckDelay)
		m.tmuxSendKeysForAgent(agent, "C-u")
		time.Sleep(staleCheckDelay)
	}

	if agent.Config.ClearOnKick {
		m.tmuxSendLiteralForAgent(agent, "/clear")
		time.Sleep(textToEnterDelay)
		m.tmuxSendEntersForAgent(agent)
		time.Sleep(clearBeforeKickDelay)
	}

	// Send message in chunks (400 rune max per chunk, rune-safe)
	runes := []rune(message)
	if len(runes) <= chunkSize {
		m.tmuxSendLiteralForAgent(agent, message)
	} else {
		for offset := 0; offset < len(runes); offset += chunkSize {
			end := offset + chunkSize
			if end > len(runes) {
				end = len(runes)
			}
			m.tmuxSendLiteralForAgent(agent, string(runes[offset:end]))
			time.Sleep(chunkDelay)
		}
	}

	// Text and Enter must always be separate calls with a delay between
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	now := time.Now()
	agent.LastKick = &now
	agent.LastKickMessage = message
	agent.KickRefused = false
	agent.KickRefusalReason = ""

	snippet := message
	const maxSnippetLen = 120
	snippet = truncateStr(snippet, maxSnippetLen)
	record := KickRecord{Timestamp: now, Agent: name, Snippet: snippet}
	if len(agent.KickHistory) >= kickHistoryCapacity {
		agent.KickHistory = agent.KickHistory[1:]
	}
	agent.KickHistory = append(agent.KickHistory, record)

	kickPreview := message
	const maxKickPreviewLen = 200
	if len(kickPreview) > maxKickPreviewLen {
		kickPreview = truncateStr(kickPreview, maxKickPreviewLen)
	}
	m.logger.Info("audit: agent kicked",
		"name", name,
		"message_len", len(message),
		"preview", kickPreview,
	)

	return nil
}

// tmuxSendLiteralForAgent sends text using the agent's tmux socket.
func (m *Manager) tmuxSendLiteralForAgent(agent *AgentProcess, text string) {
	_ = m.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "-l", text).Run()
}

// dismissInferencePrompts polls the tmux pane for Claude Code interactive
// prompts and auto-dismisses them. Works dynamically regardless of prompt
// text changes between Claude Code versions by:
//  1. Detecting "Enter to confirm" (universal prompt footer)
//  2. Finding the selected option (line with "❯" marker)
//  3. If selected option looks negative (contains "No" or "exit"), navigate
//     away from it before confirming
//  4. For "Press Enter to continue" screens, just press Enter
//
// Stops when the main Claude Code input prompt appears ("esc to interrupt").
func (m *Manager) dismissInferencePrompts(agent *AgentProcess) {
	const (
		promptPollInterval   = 1 * time.Second
		promptDismissTimeout = 60 * time.Second
		postKeystrokeDelay   = 500 * time.Millisecond
	)

	deadline := time.Now().Add(promptDismissTimeout)
	lastPane := ""

	for time.Now().Before(deadline) {
		time.Sleep(promptPollInterval)

		pane := m.captureVisiblePaneForAgent(agent)
		if pane == "" || pane == lastPane {
			continue
		}
		lastPane = pane

		// Main prompt visible — agent is ready
		if strings.Contains(pane, "bypass permissions on") || strings.Contains(pane, "esc to interrupt") {
			m.logger.Info("inference agent ready", "agent", agent.Name)
			return
		}

		// "Press Enter to continue" screens
		if strings.Contains(pane, "Press Enter to continue") {
			m.logger.Info("inference prompt: press enter", "agent", agent.Name)
			m.tmuxSendKeysForAgent(agent, "Enter")
			continue
		}

		// Selection prompts have "Enter to confirm" footer
		if !strings.Contains(pane, "Enter to confirm") {
			continue
		}

		// Find the currently selected option (marked with ❯)
		selected := ""
		for _, line := range strings.Split(pane, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "❯") {
				selected = trimmed
				break
			}
		}

		m.logger.Info("inference prompt detected",
			"agent", agent.Name,
			"selected", selected,
		)

		// If current selection looks negative, navigate away from it
		selectedLower := strings.ToLower(selected)
		if strings.Contains(selectedLower, "no,") || strings.Contains(selectedLower, "no ") ||
			strings.Contains(selectedLower, "exit") {
			// Try moving down first (most prompts put the positive option below)
			m.tmuxSendKeysForAgent(agent, "Down")
			time.Sleep(postKeystrokeDelay)
		} else if strings.Contains(selectedLower, "fix with") {
			// Settings error: skip past "Fix with Claude" and "Exit" to "Continue without"
			m.tmuxSendKeysForAgent(agent, "Down")
			time.Sleep(postKeystrokeDelay)
			m.tmuxSendKeysForAgent(agent, "Down")
			time.Sleep(postKeystrokeDelay)
		}

		m.tmuxSendKeysForAgent(agent, "Enter")
	}

	m.logger.Warn("inference prompt dismissal timed out", "agent", agent.Name)
}

// tmuxSendEntersForAgent sends Enter presses using the agent's tmux socket.
func (m *Manager) tmuxSendEntersForAgent(agent *AgentProcess) {
	for i := 0; i < enterCount; i++ {
		_ = m.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "Enter").Run()
		if i < enterCount-1 {
			time.Sleep(enterDelay)
		}
	}
}

// tmuxSendKeysForAgent sends key sequences (C-c, C-u, etc.) using the agent's tmux socket.
func (m *Manager) tmuxSendKeysForAgent(agent *AgentProcess, keys ...string) {
	args := append([]string{"send-keys", "-t", agent.tmuxSession}, keys...)
	_ = m.tmuxCmd(agent, args...).Run()
}

const (
	clearBeforeKickDelay  = 2 * time.Second
	enterCount            = 3
	enterDelay            = 300 * time.Millisecond
	textToEnterDelay      = 1 * time.Second
	chunkSize             = 400
	chunkDelay            = 1 * time.Second
	staleCheckDelay       = 1 * time.Second
	cliReadyPollInterval  = 2 * time.Second
	cliReadyTimeout       = 60 * time.Second
	inputPromptPollInterval = 2 * time.Second
	inputPromptTimeout      = 120 * time.Second
)

func (m *Manager) SeedLastKick(name string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.LastKick = &t
	}
}

func (m *Manager) SeedKickHistory(name string, records []KickRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		if len(records) > kickHistoryCapacity {
			records = records[len(records)-kickHistoryCapacity:]
		}
		agent.KickHistory = make([]KickRecord, len(records))
		copy(agent.KickHistory, records)
	}
}

func (m *Manager) GetStatus(name string) (*AgentProcess, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}
	snap := agent.snapshot()
	return &snap, nil
}

func (m *Manager) AllStatuses() map[string]*AgentProcess {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make(map[string]*AgentProcess, len(m.agents))
	for k, v := range m.agents {
		snap := v.snapshot()
		result[k] = &snap
	}
	return result
}

func (a *AgentProcess) snapshot() AgentProcess {
	history := make([]KickRecord, len(a.KickHistory))
	copy(history, a.KickHistory)
	a.paneMu.RLock()
	pane := make([]string, len(a.lastPaneCapture))
	copy(pane, a.lastPaneCapture)
	a.paneMu.RUnlock()
	return AgentProcess{
		Name:            a.Name,
		ID:              a.ID,
		Config:          a.Config,
		State:           a.State,
		PID:             a.PID,
		UID:             a.UID,
		StartedAt:       a.StartedAt,
		LastKick:        a.LastKick,
		Paused:          a.Paused,
		PausedAt:        a.PausedAt,
		PausedReason:    a.PausedReason,
		PausedTrigger:   a.PausedTrigger,
		PinnedCLI:       a.PinnedCLI,
		PinnedModel:     a.PinnedModel,
		ModelOverride:   a.ModelOverride,
		BackendOverride: a.BackendOverride,
		RestartCount:    a.RestartCount,
		KickHistory:     history,
		LastKickMessage: a.LastKickMessage,
		tmuxSession:     a.tmuxSession,
		tmuxSocket:      a.tmuxSocket,
		OutputBuffer:    a.OutputBuffer,
		lastPaneCapture: pane,
	}
}

// PaneLines returns the last n lines from the most recent tmux pane capture,
// preferring content from the current CLI session (after the last ❯ prompt).
// Falls back to showing the full tail if the current session has too few lines.
func (a *AgentProcess) PaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

func isVisualNoise(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	if strings.Trim(t, "─━─") == "" {
		return true
	}
	if strings.HasPrefix(t, "/data/agents/") && !strings.Contains(t, " ") {
		return true
	}
	return false
}

func isCLIChrome(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	if strings.HasPrefix(t, "/ commands") ||
		strings.HasPrefix(t, "? help") ||
		strings.HasPrefix(t, "@ files") ||
		strings.HasPrefix(t, "# issues") {
		return true
	}
	// Copilot/Claude/Gemini status bar: contains "esc cancel" or model name
	if strings.Contains(t, "esc cancel") {
		return true
	}
	// Model name in status bar (short line with model identifier)
	if (strings.Contains(t, "Claude ") && !strings.Contains(t, "Claude Code")) ||
		strings.Contains(t, "Copilot v") ||
		strings.Contains(t, "Gemini ") {
		// Only match if it looks like a status bar (has spinner or command hints)
		for _, prefix := range []string{"◎", "◉", "●", "○", "◐", "◑", "◒", "◓"} {
			if strings.Contains(t, prefix) {
				return true
			}
		}
	}
	return false
}

func isBufferNoise(s string) bool {
	if isCLIChrome(s) || isVisualNoise(s) {
		return true
	}
	t := strings.TrimSpace(s)
	if t == "❯" || t == "›" || t == ">" {
		return true
	}
	for _, banner := range []string{"╭─╮", "╰─╯", "█ ▘▝ █", "▔▔▔▔", "Copilot v", "Check for mistakes"} {
		if strings.Contains(t, banner) {
			return true
		}
	}
	if strings.HasPrefix(t, "● Tip:") || strings.HasPrefix(t, "└ ") || strings.HasPrefix(t, "↑/↓ to navigate") {
		return true
	}
	if strings.Contains(t, "copilot-instructions.md") && strings.Contains(t, "/init") {
		return true
	}
	if strings.Contains(t, "Do you trust the files in this folder") {
		return true
	}
	if strings.HasPrefix(t, "› ") && (strings.Contains(t, "Yes") || strings.Contains(t, "No (Esc)")) {
		return true
	}
	if strings.HasPrefix(t, "●") && strings.Contains(t, "Folder") && strings.Contains(t, "trusted") {
		return true
	}
	if strings.HasPrefix(t, "✗ Model") && strings.Contains(t, "not available") {
		return true
	}
	return false
}

func filterPaneOutput(lines []string, n int) []string {
	lastPrompt := -1
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "❯" || trimmed == "›" || trimmed == ">" {
			lastPrompt = i
			break
		}
	}
	if lastPrompt >= 0 && lastPrompt < len(lines)-1 {
		afterPrompt := lines[lastPrompt+1:]
		hasContent := false
		for _, l := range afterPrompt {
			if !isCLIChrome(l) && !isVisualNoise(l) {
				hasContent = true
				break
			}
		}
		if hasContent {
			lines = afterPrompt
		} else {
			lines = lines[:lastPrompt]
		}
	}
	var cleaned []string
	for _, l := range lines {
		if !isVisualNoise(l) {
			cleaned = append(cleaned, l)
		}
	}
	lines = cleaned
	lines = DeduplicateBlocks(lines)
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := make([]string, len(lines))
	copy(out, lines)
	return out
}

// DeduplicateBlocks removes repeated blocks from pane output.
// It finds the longest suffix that also appears earlier and removes the earlier copy.
func DeduplicateBlocks(lines []string) []string {
	if len(lines) < 4 {
		return lines
	}
	// Try block sizes from half the total down to 2 lines.
	maxBlock := len(lines) / 2
	for blockSize := maxBlock; blockSize >= 2; blockSize-- {
		// Extract the last blockSize lines as the candidate block.
		candidate := lines[len(lines)-blockSize:]
		// Scan backwards for an earlier occurrence.
		for start := len(lines) - blockSize - 1; start >= 0; start-- {
			if start+blockSize > len(lines)-blockSize {
				continue
			}
			match := true
			for j := 0; j < blockSize; j++ {
				if normalizeLine(lines[start+j]) != normalizeLine(candidate[j]) {
					match = false
					break
				}
			}
			if match {
				// Remove the earlier duplicate block.
				result := make([]string, 0, len(lines)-blockSize)
				result = append(result, lines[:start]...)
				result = append(result, lines[start+blockSize:]...)
				return DeduplicateBlocks(result)
			}
		}
	}
	return lines
}

func (a *AgentProcess) FilteredPaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

func backendBinary(backend string) (string, error) {
	binaries := map[string]string{
		"claude":  "claude",
		"copilot": "copilot",
		"gemini":  "gemini",
		"goose":   "goose",
		"pi":      "goose",
		"bob":     "bob",
		"vllm":    "claude",
		"llm-d":   "claude",
	}

	binary, ok := binaries[backend]
	if !ok {
		return "", fmt.Errorf("unknown backend: %s", backend)
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("backend %s not found in PATH: %w", backend, err)
	}

	return path, nil
}

const (
	sharedCopilotConfigPath    = "/data/home/.copilot/config.json"
	sharedConfigDesiredMode    = 0o660
	tokenRestartCooldownSec    = 60  // minimum seconds between token-triggered restarts per agent
	expiredTokenHangTimeoutSec = 180 // blank pane after this many seconds triggers token purge + restart
)

// loginPromptPatterns are substrings that indicate an agent is stuck on the
// Copilot login/authentication screen.
var loginPromptPatterns = []string{
	"/login",
	"sign in to use",
	"Sign in to use",
	"authenticate to use",
	"Authenticate to use",
	"log in to use",
	"Log in to use",
}

// configHasTokens reads the shared Copilot config.json, strips single-line
// // comments (which Copilot CLI sometimes writes), parses the JSON, and returns
// true if the "copilotTokens" field has at least one entry.
func configHasTokens() bool {
	data, err := os.ReadFile(sharedCopilotConfigPath)
	if err != nil {
		return false
	}

	// Strip single-line // comments that Copilot CLI sometimes adds.
	var cleaned []byte
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		cleaned = append(cleaned, []byte(line+"\n")...)
	}

	var cfg map[string]interface{}
	if err := json.Unmarshal(cleaned, &cfg); err != nil {
		return false
	}
	tokens, ok := cfg["copilotTokens"]
	if !ok {
		return false
	}
	tokensMap, ok := tokens.(map[string]interface{})
	if !ok {
		return false
	}
	return len(tokensMap) > 0
}

// clearExpiredTokens removes stored copilot tokens from config.json.
// An expired gho_ token causes copilot to hang during auth through the
// MITM proxy instead of falling through to the /login prompt.
func clearExpiredTokens() error {
	data, err := os.ReadFile(sharedCopilotConfigPath)
	if err != nil {
		return err
	}
	var cleaned []byte
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		cleaned = append(cleaned, []byte(line+"\n")...)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(cleaned, &cfg); err != nil {
		return err
	}
	cfg["copilotTokens"] = map[string]interface{}{}
	cfg["loggedInUsers"] = []interface{}{}
	delete(cfg, "lastLoggedInUser")
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	content := "// User settings belong in settings.json.\n// This file is managed automatically.\n" + string(out)
	return os.WriteFile(sharedCopilotConfigPath, []byte(content), sharedConfigDesiredMode)
}

// cliReadyIndicators prove copilot finished startup.
var cliReadyIndicators = []string{
	"❯",
	"/ commands",
	"? help",
	"/login",
	"sign in",
	"Sign in",
	"Copilot v",
	"Tip: /init",
	"Loading:",
	"● Loading",
}

// paneShowsCLIReady returns true if the pane shows any indicator that
// copilot finished initializing (prompt, help text, or login request).
func paneShowsCLIReady(lines []string) bool {
	for _, line := range lines {
		for _, ind := range cliReadyIndicators {
			if strings.Contains(line, ind) {
				return true
			}
		}
	}
	return false
}

// paneShowsLoginPrompt returns true if any line in the pane output matches a
// known login/authentication prompt pattern.
func paneShowsLoginPrompt(lines []string) bool {
	for _, line := range lines {
		for _, pat := range loginPromptPatterns {
			if strings.Contains(line, pat) {
				return true
			}
		}
	}
	return false
}

const (
	diagnosticTimeoutSec = 20
	diagnosticPollSec    = 2
)

// authErrorPatterns indicate the token is bad and should be cleared.
var authErrorPatterns = []string{
	"Bad credentials",
	"401 Unauthorized",
	"token found but could not be validated",
	"Failed to fetch OAuth user login",
	"re-authenticate",
}

func (m *Manager) runCopilotDiagnostic(ctx context.Context, agent *AgentProcess) {
	m.tmuxSendKeysForAgent(agent, "C-c", "")
	time.Sleep(paneCaptureSleep)
	killAgentProcesses(agent.UID, m.logger)
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
			return
		case <-ticker.C:
			output := m.captureTmuxPaneForAgent(agent)
			if output == "" {
				continue
			}
			isAuthError := false
			for _, pat := range authErrorPatterns {
				if strings.Contains(output, pat) {
					isAuthError = true
					break
				}
			}
			if isAuthError {
				m.logger.Warn("diagnostic: auth error detected, clearing token",
					"agent", agent.Name, "output_snippet", truncateStr(output, 200))
				agent.LastError = "auth token expired or invalid"
				if err := clearExpiredTokens(); err != nil {
					m.logger.Warn("diagnostic: failed to clear tokens", "error", err)
				}
			} else if paneShowsCLIReady(strings.Split(output, "\n")) {
				m.logger.Info("diagnostic: copilot started successfully in bare mode", "agent", agent.Name)
				agent.LastError = ""
			} else {
				continue
			}

			killAgentProcesses(agent.UID, m.logger)
			_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()
			agent.forceRelaunch = true
			if err := m.Restart(ctx, agent.Name); err != nil {
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

const (
	claudeInferenceSettingsPath = "/tmp/.claude-inference-settings.json"
	claudeInferenceHomePath     = "/tmp/.claude-inference-home"
)

// ensureClaudeSettings creates a writable HOME directory for inference agents
// with pre-populated .claude/settings.json. This avoids ALL interactive prompts
// (settings error, theme, API key, bypass permissions, trust folder, onboarding)
// because Claude Code reads hasCompletedOnboarding/hasAcknowledgedDisclaimer/
// bypassPermissions from the settings file at $HOME/.claude/settings.json.
// The default /data/home is root:root 770 so agent UIDs can't access it.
func (m *Manager) ensureClaudeSettings() {
	settingsDir := filepath.Join(claudeInferenceHomePath, ".claude")
	settingsFile := filepath.Join(settingsDir, "settings.json")
	if _, err := os.Stat(settingsFile); err == nil {
		return
	}
	if err := os.MkdirAll(settingsDir, 0o777); err != nil {
		m.logger.Warn("failed to create claude inference home", "error", err)
		return
	}
	settings := `{"permissions":{"allow":[],"deny":[]},"hasCompletedOnboarding":true,"bypassPermissions":true,"hasAcknowledgedDisclaimer":true}`
	if err := os.WriteFile(settingsFile, []byte(settings), 0o644); err != nil {
		m.logger.Warn("failed to write claude settings", "error", err)
	}
	// Also write the standalone settings file for --settings flag
	_ = os.WriteFile(claudeInferenceSettingsPath, []byte(settings), 0o644)
	// Pre-populate .claude.json to skip first-run setup (GrowthBook, migrations)
	userConfig := filepath.Join(claudeInferenceHomePath, ".claude.json")
	if _, err := os.Stat(userConfig); err != nil {
		cfg := `{"hasCompletedOnboarding":true,"opusProMigrationComplete":true,"sonnet1m45MigrationComplete":true,"migrationVersion":13}`
		_ = os.WriteFile(userConfig, []byte(cfg), 0o644)
	}
}

// normalizeModelName converts YAML-friendly model names (claude-sonnet-4-6) to
// the format CLIs expect (claude-sonnet-4.6). The last hyphen before a trailing
// digit group becomes a dot.
func normalizeModelName(model string) string {
	idx := strings.LastIndex(model, "-")
	if idx < 0 || idx == len(model)-1 {
		return model
	}
	suffix := model[idx+1:]
	allDigits := true
	for _, c := range suffix {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return model[:idx] + "." + suffix
	}
	return model
}

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
		modeFile := fmt.Sprintf("/tmp/.hive-mode-%s", name)
		if err := os.WriteFile(modeFile, []byte(mode.String()), 0o644); err != nil {
			m.logger.Warn("SyncModeFiles: write failed", "file", modeFile, "error", err)
		}
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

// agentCanWrite returns true if this agent is allowed to push branches and create PRs.
// Deprecated: use agentMode() for granular mode checks.
func (m *Manager) agentCanWrite(agent *AgentProcess) bool {
	return m.agentMode(agent).CanPush()
}

// filteredEnv returns os.Environ() with write-capable tokens removed for advisory agents.
// COPILOT_GITHUB_TOKEN is kept for all agents (needed for AI auth); write access is
// gated by --enable-all-github-mcp-tools flag. GH_TOKEN and GITHUB_TOKEN are stripped
// from non-quality agents to enforce gh-wrapper and credential helper policies.
func (m *Manager) filteredEnv(agent *AgentProcess) []string {
	env := os.Environ()
	if m.agentMode(agent).CanPush() {
		return env
	}
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "GH_TOKEN=") ||
			strings.HasPrefix(e, "GITHUB_TOKEN=") {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// embeddedTokenRe matches git remote URLs with embedded credentials:
// https://x-access-token:TOKEN@github.com/org/repo.git
var embeddedTokenRe = regexp.MustCompile(`^https://[^@]+@(github\.com/.+)$`)

// sanitizeGitRemotes strips embedded tokens from git remote URLs in all repos
// under the agent's work directory. Copilot CLI embeds the GitHub App token
// directly in the remote URL when it clones, bypassing both the credential
// helper (Layer 1) and env var filtering (Layer 2).
func (m *Manager) sanitizeGitRemotes(agent *AgentProcess) {
	if m.agentMode(agent).CanPush() {
		return
	}
	agentDir := m.workDir + "/" + agent.Name
	_ = filepath.WalkDir(agentDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.Name() != ".git" || !d.IsDir() {
			return nil
		}
		repoDir := filepath.Dir(path)
		out, err := exec.Command("git", "-C", repoDir, "remote", "get-url", "origin").Output()
		if err != nil {
			return filepath.SkipDir
		}
		url := strings.TrimSpace(string(out))
		if match := embeddedTokenRe.FindStringSubmatch(url); match != nil {
			clean := "https://" + match[1]
			_ = exec.Command("git", "-C", repoDir, "remote", "set-url", "origin", clean).Run()
			m.logger.Info("stripped embedded token from git remote",
				"agent", agent.Name, "repo", repoDir)
		}
		return filepath.SkipDir
	})
}

// agentEnvPair is an unquoted key-value environment variable.
type agentEnvPair struct {
	Key   string
	Value string
	// Secret vars are set via tmux set-environment only, never on the command line.
	Secret bool
}

func (m *Manager) agentEnvPairs(agent *AgentProcess) []agentEnvPair {
	model := agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}
	displayName := agent.Config.DisplayName
	if displayName == "" {
		displayName = agent.Name
	}
	vars := []agentEnvPair{
		{"HIVE_AGENT", agent.Name, false},
		{"HIVE_AGENT_DISPLAY_NAME", displayName, false},
		{"HIVE_BACKEND", backend, false},
		{"HIVE_MODEL", model, false},
	}
	if hiveID := os.Getenv("HIVE_ID"); hiveID != "" {
		vars = append(vars, agentEnvPair{"HIVE_ID", hiveID, false})
	}
	vars = append(vars, agentEnvPair{"HIVE_ACMM_LEVEL", fmt.Sprintf("%d", m.project.ACMMLevel), false})

	mode := m.agentMode(agent)
	vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", mode.String(), false})
	modeFile := fmt.Sprintf("/tmp/.hive-mode-%s", agent.Name)
	if err := os.WriteFile(modeFile, []byte(mode.String()), 0o644); err != nil {
		m.logger.Warn("agentBootstrapEnv: mode file write failed", "file", modeFile, "error", err)
	}
	proxyURL := fmt.Sprintf("http://%s@127.0.0.1:%d", agent.Name, proxyListenPort)
	vars = append(vars, agentEnvPair{"HTTPS_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HTTP_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HIVE_PROXY_AGENT", agent.Name, false})
	vars = append(vars, agentEnvPair{"NODE_EXTRA_CA_CERTS", proxyCACertPath, false})
	if sha := os.Getenv("HIVE_SHA"); sha != "" {
		vars = append(vars, agentEnvPair{"HIVE_SHA", sha, false})
	}
	if advisory := os.Getenv("HIVE_ADVISORY_ISSUE"); advisory != "" {
		vars = append(vars, agentEnvPair{"HIVE_ADVISORY_ISSUE", advisory, false})
	}
	if IsInferenceBackend(backend) {
		const inferenceTranslatePort = 18444
		vars = append(vars, agentEnvPair{"ANTHROPIC_API_KEY", "sk-hive-" + agent.Name, false})
		baseURL := fmt.Sprintf("http://127.0.0.1:%d", inferenceTranslatePort)
		vars = append(vars, agentEnvPair{"ANTHROPIC_BASE_URL", baseURL, false})
		vars = append(vars, agentEnvPair{"NO_PROXY", "127.0.0.1,localhost", false})
	}
	if m.copilotAuthToken != "" {
		vars = append(vars, agentEnvPair{"COPILOT_GITHUB_TOKEN", m.copilotAuthToken, true})
	}
	// BD_DIR tells the `bd` CLI where to read/write beads. Without this,
	// bd falls back to cwd (/data/agents/<name>) instead of the configured
	// beads_dir (/data/beads/<name>), causing a path mismatch with the
	// dashboard and advisory digest.
	if agent.Config.BeadsDir != "" {
		vars = append(vars, agentEnvPair{"BD_DIR", agent.Config.BeadsDir, false})
	}
	// GIT_SSL_CAINFO only — NOT SSL_CERT_FILE (that breaks Copilot API TLS)
	vars = append(vars, agentEnvPair{"GIT_SSL_CAINFO", proxyCACertPath, false})
	if agent.UID > 0 {
		home := "/data/home"
		if IsInferenceBackend(backend) {
			home = claudeInferenceHomePath
		}
		vars = append(vars, agentEnvPair{"HOME", home, false})
	}
	return vars
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

func (m *Manager) Pause(name, trigger, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.Paused = true
	agent.PausedAt = time.Now()
	agent.PausedReason = reason
	agent.PausedTrigger = trigger
	if agent.State == StateRunning {
		m.tmuxSendKeysForAgent(agent, "C-c", "")
	}
	agent.State = StatePaused
	m.logger.Info("audit: agent paused",
		"name", name,
		"trigger", trigger,
		"reason", reason,
		"backend", agent.Config.Backend,
		"restart_count", agent.RestartCount,
	)
	return nil
}

func (m *Manager) SeedPauseState(name string, pausedAt time.Time, trigger, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.PausedAt = pausedAt
		agent.PausedTrigger = trigger
		agent.PausedReason = reason
	}
}

func (m *Manager) Resume(ctx context.Context, name, trigger, reason string) error {
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}

	prevTrigger := agent.PausedTrigger
	prevReason := agent.PausedReason
	agent.Paused = false
	agent.PausedAt = time.Time{}
	agent.PausedReason = ""
	agent.PausedTrigger = ""
	needsRelaunch := agent.State == StatePaused
	if needsRelaunch {
		agent.forceRelaunch = true
	}
	// Release the global agents-map lock before slow per-agent tmux
	// operations (ensureTmuxSession + launchInTmux ~2s each). This allows
	// concurrent Resume() calls for different agents to run in parallel
	// instead of serializing on the mutex.
	m.mu.Unlock()

	m.logger.Info("audit: agent resumed",
		"name", name,
		"trigger", trigger,
		"reason", reason,
		"prev_trigger", prevTrigger,
		"prev_reason", prevReason,
	)
	if needsRelaunch {
		if err := m.ensureTmuxSession(agent); err != nil {
			return err
		}
		return m.launchInTmux(ctx, agent)
	}
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

	agent.BootstrapOverride = prompt
	agent.Paused = false
	m.logger.Info("bootstrap override set (atomic)", "agent", name, "len", len(prompt))

	if agent.State == StateRunning {
		m.tmuxSendKeysForAgent(agent, "C-c", "")
		if agent.cancel != nil {
			agent.cancel()
		}
	}

	if agent.UID > 0 {
		killed := killAgentProcesses(agent.UID, m.logger)
		m.logger.Info("killed orphaned agent processes",
			"name", name, "uid", agent.UID, "killed", killed)
	}

	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()

	agent.RestartCount++
	agent.forceRelaunch = true

	if err := m.ensureTmuxSession(agent); err != nil {
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()

	// Wait for the new shell to initialize before sending the launch command.
	// Without this, $(cat /tmp/.hive-bootstrap-*.txt) can fail because the
	// shell isn't ready to process command substitution yet.
	// Released the lock before sleeping so other manager operations aren't blocked.
	const sessionReadyDelay = 2 * time.Second
	time.Sleep(sessionReadyDelay)

	m.mu.Lock()
	defer m.mu.Unlock()
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

// killAgentProcesses finds all processes owned by the given UID via /proc and
// sends SIGKILL to each. Hung copilot binaries ignore SIGINT, so brute-force
// cleanup is needed to prevent orphan accumulation on the shared SQLite store.
func killAgentProcesses(uid int, logger *slog.Logger) int {
	const procPath = "/proc"
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
		f.Close()

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
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if agent.State == StateRunning {
		m.tmuxSendKeysForAgent(agent, "C-c", "")
		if agent.cancel != nil {
			agent.cancel()
		}
	}

	if agent.UID > 0 {
		killed := killAgentProcesses(agent.UID, m.logger)
		m.logger.Info("killed orphaned agent processes",
			"name", name, "uid", agent.UID, "killed", killed)
	}

	_ = m.tmuxCmd(agent, "kill-session", "-t", agent.tmuxSession).Run()

	agent.RestartCount++
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
	return nil
}

func (m *Manager) SeedRestartCount(name string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.RestartCount = count
	}
}

func (m *Manager) PinCLI(name, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedCLI = version
	m.logger.Info("agent CLI pinned", "name", name, "version", version)
	return nil
}

func (m *Manager) UnpinCLI(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedCLI = ""
	m.logger.Info("agent CLI unpinned", "name", name)
	return nil
}

func (m *Manager) PinModel(name, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedModel = model
	agent.ModelOverride = model
	m.logger.Info("agent model pinned", "name", name, "model", model)
	return nil
}

func (m *Manager) UnpinModel(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedModel = ""
	m.logger.Info("agent model unpinned", "name", name)
	return nil
}

func (m *Manager) SetModelOverride(name, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if agent.PinnedModel != "" {
		return fmt.Errorf("agent %s model is pinned to %s", name, agent.PinnedModel)
	}

	agent.ModelOverride = model
	m.logger.Info("agent model override set", "name", name, "model", model)
	return nil
}

func (m *Manager) SetBackendOverride(name, backend string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.BackendOverride = backend
	m.logger.Info("agent backend override set", "name", name, "backend", backend)
	return nil
}

// GetBufferOutput returns output from the ring buffer directly, bypassing
// the tmux pane capture. The ring buffer accumulates all output over time
// (up to 500 lines) while the pane capture only has visible lines.
func (m *Manager) GetBufferOutput(name string, lines int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}

	if agent.OutputBuffer != nil && agent.OutputBuffer.Count() > 0 {
		return agent.OutputBuffer.Last(lines), nil
	}

	if pane := agent.FilteredPaneLines(lines); len(pane) > 0 {
		return pane, nil
	}

	return nil, nil
}

func (m *Manager) GetOutput(name string, lines int) ([]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}

	if pane := agent.FilteredPaneLines(lines); len(pane) > 0 {
		return pane, nil
	}

	if agent.OutputBuffer != nil {
		return agent.OutputBuffer.Last(lines), nil
	}

	return nil, nil
}

func (m *Manager) IsPaused(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return false
	}
	return agent.Paused
}

func (m *Manager) TmuxSession(name string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	agent, ok := m.agents[name]
	if !ok {
		return ""
	}
	return agent.tmuxSession
}

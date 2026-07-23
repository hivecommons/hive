package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
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

	"github.com/kubestellar/hive/v2/pkg/claude"
	"github.com/kubestellar/hive/v2/pkg/config"
	ghpkg "github.com/kubestellar/hive/v2/pkg/github"
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
	NeedsLogin          bool   // true when pane shows a login prompt
	consentSeenAt       time.Time // watcher: when a consent screen was first seen in the pane
	lastConsentDismiss  time.Time // watcher: cooldown for re-running dismissInferencePrompts
	lastInferKickAt     time.Time // stall watchdog: when the last kick was delivered to an inference agent
	lastInferKickPane   string    // stall watchdog: hash of the visible pane just after kick delivery
	stallNudgeSent      bool      // stall watchdog: at most one nudge per kick
	StallNudges         int       // total post-kick stall nudges sent (surfaced to the dashboard)
	launchGen           int       // increments per launch; stale deliverStartupKick goroutines check it and drop
	lastInferKickMarks  int       // no-action watchdog: tool-marker count in pane+scrollback just after kick delivery
	actionNudgeSent     bool      // no-action watchdog: at most one action nudge per kick
	ActionNudges        int       // total prose-only-response action nudges sent (surfaced to the dashboard)
}

// effectiveBackend returns the agent's backend accounting for any override.
func effectiveBackend(agent *AgentProcess) string {
	if agent.BackendOverride != "" {
		return agent.BackendOverride
	}
	return agent.Config.Backend
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
	claudeAuthToken  string
	uidMap           *UIDMap
	appAuth          AppTokenMinter

	inferenceRouteCallback      func(agentName, backend, model string)
	clearInferenceRouteCallback func(agentName string)

	// persistPauseCallback, when set, persists an agent's paused state to
	// the on-disk config so it survives restarts. Nil in tests / bare setups.
	persistPauseCallback func(name string, paused bool)
}

// SetPersistPauseCallback wires a function that persists an agent's paused
// state to config (called on pause/resume). Idempotent saves are the
// caller's responsibility.
func (m *Manager) SetPersistPauseCallback(fn func(name string, paused bool)) {
	m.mu.Lock()
	m.persistPauseCallback = fn
	m.mu.Unlock()
}

// AppTokenMinter is implemented by github.AppAuth to mint per-agent scoped tokens.
type AppTokenMinter interface {
	WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error
}

// IsInferenceBackend returns true if the backend is a self-hosted inference
// backend (vllm, llm-d, litellm) rather than a CLI tool. Delegates to the
// canonical list in the config package (shared with the proxy package,
// which cannot be imported from here without a cycle).
func IsInferenceBackend(backend string) bool {
	return config.IsInferenceBackend(backend)
}

// ReloadClaudeToken re-reads the Claude credentials file and updates the
// cached token. Called by the dashboard after a successful OAuth login.
func (m *Manager) ReloadClaudeToken() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.claudeAuthToken = claude.ReadAccessToken(claude.CredentialsPath)
}

// SetCopilotToken updates the cached Copilot token injected into agent
// environments as COPILOT_GITHUB_TOKEN. Called by the dashboard after a
// successful device-flow login.
func (m *Manager) SetCopilotToken(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.copilotAuthToken = token
}

// CopilotToken returns the cached Copilot (GitHub OAuth) token, or "" if
// none is set. Used by the dashboard's model-discovery probe to authenticate
// against the Copilot models endpoint. The value is a secret and must never
// be logged.
func (m *Manager) CopilotToken() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.copilotAuthToken
}

// BackendAuthAvailable reports whether shared credentials exist for a CLI
// backend, so the dashboard can show honest auth state even for agents with
// no running pane (e.g. on-demand agents that never launched). Claude checks
// the credentials file (with expiry); Copilot checks the cached token. For
// backends we cannot introspect it returns (false, false) = unknown.
func (m *Manager) BackendAuthAvailable(backend string) (available, known bool) {
	switch backend {
	case "claude":
		return claude.HasValidToken(claude.CredentialsPath), true
	case "copilot":
		m.mu.RLock()
		tok := m.copilotAuthToken
		m.mu.RUnlock()
		if tok != "" {
			return true, true
		}
		return configHasTokens(), true
	default:
		return false, false
	}
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

// SetAppAuth injects the GitHub App auth provider for per-agent scoped tokens.
func (m *Manager) SetAppAuth(auth AppTokenMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appAuth = auth
}

const agentTokenRefreshInterval = 40 * time.Minute

// StartAgentTokenRefresh refreshes per-agent scoped tokens for all running
// agents on a timer. Tokens expire after 1 hour; this refreshes at 40-minute
// intervals so there's always a valid token on disk.
func (m *Manager) StartAgentTokenRefresh(ctx context.Context) {
	ticker := time.NewTicker(agentTokenRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshAgentTokens(ctx)
		}
	}
}

func (m *Manager) refreshAgentTokens(ctx context.Context) {
	m.mu.RLock()
	auth := m.appAuth
	agents := make([]*AgentProcess, 0, len(m.agents))
	for _, a := range m.agents {
		if a.State == StateRunning && a.UID > 0 {
			agents = append(agents, a)
		}
	}
	m.mu.RUnlock()

	if auth == nil {
		return
	}

	for _, a := range agents {
		tier := m.agentMode(a).TokenTier()
		if err := auth.WriteAgentToken(ctx, a.Name, tier, a.UID); err != nil {
			m.logger.Warn("agent token refresh failed", "agent", a.Name, "error", err)
		}
	}
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
	if copilotToken == "" {
		// Fall back to the token persisted by the dashboard's device-flow login.
		if data, err := os.ReadFile(CopilotUserTokenPath); err == nil {
			copilotToken = strings.TrimSpace(string(data))
		}
	}
	claudeToken := claude.ReadAccessToken(claude.CredentialsPath)

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
		claudeAuthToken:  claudeToken,
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
			// Restore a persisted operator pause so a restart/upgrade
			// doesn't silently un-pause the agent.
			Paused:       cfg.Paused,
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
		// Auto-unpause inference agents that were only transiently paused —
		// but NEVER override a persisted operator pause (Config.Paused set via
		// the dashboard and saved to hive.yaml). Previously this cleared EVERY
		// inference-backend pause on startup, so an operator pause of a
		// litellm/vllm/llm-d agent was silently undone on every restart AND
		// the auto-unpause overwrote the persisted flag — corrupting the saved
		// pause set (issue: kellyaa's pauses reverted on restart despite being
		// on inference backends).
		// Auto-unpause inference agents ONLY for a non-operator (transient/
		// system) pause. An operator pause — dashboard-api trigger, or the
		// persisted Config.Paused flag — must ALWAYS survive, exactly like a
		// copilot-backed agent does. Keying on the backend alone wiped
		// operator pauses of litellm/vllm/llm-d agents on every restart
		// (kellyaa: her litellm-routed agents un-paused while the copilot
		// strategist stayed paused, which is what exposed this).
		operatorPaused := agent.Config.Paused || agent.PausedTrigger == "dashboard-api"
		if IsInferenceBackend(backend) && !operatorPaused {
			agent.Paused = false
			m.logger.Info("auto-unpaused inference agent (transient pause, not operator)", "name", agent.Name, "backend", backend, "trigger", agent.PausedTrigger)
		} else {
			agent.State = StatePaused
			m.logger.Info("agent starting paused", "name", agent.Name, "backend", backend, "trigger", agent.PausedTrigger, "persisted", agent.Config.Paused)
			return nil
		}
	}

	if m.appAuth != nil && agent.UID > 0 {
		tier := m.agentMode(agent).TokenTier()
		if err := m.appAuth.WriteAgentToken(ctx, agent.Name, tier, agent.UID); err != nil {
			m.logger.Warn("failed to mint per-agent token, agent will use shared cache",
				"agent", agent.Name, "tier", tier, "error", err)
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
	if plukPath, err := exec.LookPath("pluk"); err == nil {
		_ = os.MkdirAll("/var/run/pluk/logs", 0o1777)
		_ = os.MkdirAll("/var/run/pluk/commands", 0o1777)
		backend := agent.Config.Backend
		if agent.BackendOverride != "" {
			backend = agent.BackendOverride
		}
		if backend == "" || IsInferenceBackend(backend) {
			backend = "claude"
		}
		pipePaneCmd := fmt.Sprintf("%s watch %s --cli=%s", plukPath, agent.tmuxSession, backend)
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

// paneHasCLIMarker reports whether the given pane content contains any known
// CLI UI marker.
func paneHasCLIMarker(output string) bool {
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

// tmuxPaneHasCLI reports whether a CLI is running in the pane by inspecting
// the visible pane content for known CLI UI markers.
func (m *Manager) tmuxPaneHasCLI(session string) bool {
	return paneHasCLIMarker(m.captureTmuxPane(session))
}

// tmuxPaneHasCLIForAgent checks for CLI markers using the agent's tmux socket.
// Uses visible pane only (no scrollback) to avoid false positives from stale
// markers left in scroll history after a CLI exits.
func (m *Manager) tmuxPaneHasCLIForAgent(agent *AgentProcess) bool {
	return paneHasCLIMarker(m.captureVisiblePaneForAgent(agent))
}

const (
	// consentConfirmFooter appears at the bottom of Claude Code interactive
	// selection screens (consent dialogs, settings-error menus).
	consentConfirmFooter = "Enter to confirm"
	// bypassConsentTitle is the heading of the --dangerously-skip-permissions
	// consent screen. Its default selection is "No, exit" — confirming it
	// terminates the CLI and leaves a bare bash pane.
	bypassConsentTitle = "Bypass Permissions mode"
	// bypassConsentDefaultOption is the default (negative) option on the
	// bypass-permissions consent screen.
	bypassConsentDefaultOption = "No, exit"
	// bypassConsentAcceptOption is the affirmative option on the
	// bypass-permissions consent screen. Its position varies between CLI
	// versions, so acceptance navigates by matching the selected-line text.
	bypassConsentAcceptOption = "Yes, I accept"
	// apiKeyPromptTitle is the heading of the custom-API-key approval prompt,
	// shown when ANTHROPIC_API_KEY is not in customApiKeyResponses.approved.
	// Its default selection is "No (recommended)" with the affirmative option
	// above it.
	apiKeyPromptTitle = "Detected a custom API key"
	// apiKeyPromptAcceptOption is the affirmative option on the
	// custom-API-key approval prompt.
	apiKeyPromptAcceptOption = "Yes"
	// cliWorkingMarker is shown while Claude Code is actively processing a
	// request; a pane in this state is never a consent screen.
	cliWorkingMarker = "esc to interrupt"
)

// paneShowsConsentScreen reports whether the pane is showing an interactive
// consent/selection screen rather than a ready CLI input prompt. Such screens
// contain a "❯"-selected menu option (e.g. "❯ 1. No, exit"), so they satisfy
// marker-based CLI presence checks ("❯" is also a cliPaneMarkers entry) — a
// kick typed into one is consumed by the menu, or by bash once the default
// "No, exit" selection terminates the CLI. Callers should pass the visible
// pane only (no scrollback): dismissed consent screens linger in history.
func paneShowsConsentScreen(pane string) bool {
	if pane == "" || strings.Contains(pane, cliWorkingMarker) {
		return false
	}
	if strings.Contains(pane, bypassConsentTitle) && strings.Contains(pane, bypassConsentDefaultOption) {
		return true
	}
	if !strings.Contains(pane, consentConfirmFooter) {
		return false
	}
	for _, line := range strings.Split(pane, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "❯") {
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
	modelIn := model
	model = normalizeModelName(model, backend)

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
		m.ensureClaudeSettings(agent.Name, agent.UID)
		if m.inferenceRouteCallback != nil {
			// inference-model-passthrough: the model set here becomes the
			// outbound OpenAI "model" field that the gateway checks for
			// entitlement, so it must equal the configured model verbatim.
			// Log in->out (never keys) so a mismatch is greppable.
			m.logger.Info("inference route model passthrough",
				"agent", agent.Name, "backend", backend,
				"model_in", modelIn, "model_out", model)
			m.inferenceRouteCallback(agent.Name, backend, model)
		}
		backend = "claude"
	} else if m.clearInferenceRouteCallback != nil {
		m.clearInferenceRouteCallback(agent.Name)
	}

	if agent.Config.CavemanMode != "" {
		m.installCavemanForAgent(agent, backend)
	}

	if agent.Config.Tools != nil {
		launchCmd = toolRulesToLaunchCmd(binary, model, backend, agent.Config.Tools, isInference)
		if agent.Config.Tools != nil && agent.Config.Mode != "" {
			m.logger.Warn("agent has both tools and mode set; tools takes precedence", "agent", agent.Name)
		}
	} else {
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
				launchCmd = fmt.Sprintf("%s --model %s --no-auto-update --allow-all --enable-all-github-mcp-tools",
					binary, model)
			case mode == ModeIssuesOnly:
				launchCmd = fmt.Sprintf("%s --model %s --no-auto-update --allow-all --enable-all-github-mcp-tools"+
					" --deny-tool='github(create_pull_request)'"+
					" --deny-tool='github(merge_pull_request)'",
					binary, model)
			default:
				launchCmd = fmt.Sprintf("%s --model %s --no-auto-update --allow-all"+
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
	}

	if mcpFlags := connectionMCPFlags(agent.Config.Connections, backend); mcpFlags != "" {
		launchCmd += mcpFlags
	}

	if bootstrapPrompt == "" && isInference {
		bootstrapPrompt = "You are an AI agent. Await further instructions."
	}

	// Interactive backends get the bootstrap prompt delivered AFTER the CLI
	// is ready (deliverStartupKick, spawned below) instead of embedding it in
	// the launch command. Embedding raced the CLI boot: the prompt-bearing
	// launch line was typed into the pane in the same second as the (re)start
	// (observed live: `audit: agent kicked trigger=startup` 60ms after
	// `audit: agent restarting`), before the CLI — or even bash — was ready
	// to consume it, so the kick text landed in bash and an unbalanced quote
	// left the shell in PS2 continuation. Goose keeps the embedded --text
	// prompt: goose run needs it to stay interactive and exits on the ^C that
	// readiness-gated delivery sends.
	deferredStartupKick := ""
	if bootstrapPrompt != "" {
		switch backend {
		case "claude", "copilot", "gemini":
			deferredStartupKick = bootstrapPrompt
			bootstrapPrompt = ""
		}
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

		// Only goose (and unknown backends, which never embed) reach this
		// block — claude/copilot/gemini bootstrap prompts were deferred to
		// deliverStartupKick above.
		promptFile := fmt.Sprintf("/tmp/.hive-bootstrap-%s.txt", agent.Name)
		if err := os.WriteFile(promptFile, []byte(bootstrapPrompt), 0o644); err != nil {
			m.logger.Warn("failed to write bootstrap prompt", "name", agent.Name, "error", err)
		} else if backend == "goose" {
			launchCmd += fmt.Sprintf(" --text \"$(cat %s)\"", promptFile)
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
		if isInference {
			// The surviving pane may be parked on a consent screen (e.g.
			// the hub restarted while the CLI awaited consent). The marker
			// check above cannot tell the difference, so re-arm dismissal;
			// it exits quickly once the main prompt is visible.
			go m.dismissInferencePrompts(agent)
		}
		return nil
	}
	agent.forceRelaunch = false

	// Single-CLI guarantee: reap any pre-existing or leaked CLI for this agent
	// before launching a new one. Without this a relaunch (model/backend switch,
	// crash-restart) spawns a second claude alongside the old one — the old
	// process keeps hitting the gateway on a stale model and 403-floods the
	// pane. The reaper matches by HIVE_AGENT env, so it also catches a process
	// that survived tmux kill-session by detaching from the pane. Runs on every
	// real launch (the CLI-already-running early return above skips it, keeping
	// the healthy single CLI).
	if reaped := m.reapAgentCLI(agent); reaped > 0 {
		m.logger.Info("reaped stale CLI before launch",
			"name", agent.Name, "reaped", reaped, "session", agent.tmuxSession)
		// Give the kernel a moment to tear down the killed process so the new
		// launch starts from a clean slate (no lingering socket on the gateway).
		time.Sleep(preLaunchShellClearDelay)
	}

	m.fixSharedConfigPerms(agent)

	envCmd := m.buildEnvPrefix(agent)
	fullCmd := envCmd + launchCmd

	// A previously spilled kick can leave bash in PS2 quote-continuation
	// (an unbalanced quote): anything typed next is appended to the open
	// string literal instead of executing, so the launch command would be
	// silently eaten. Abort any pending continuation or partially typed
	// line before typing the launch command. The pane holds only bash at
	// this point (the CLI-already-running check above returned early), so
	// C-c cannot kill a live CLI.
	m.tmuxSendKeysForAgent(agent, "C-c")
	time.Sleep(preLaunchShellClearDelay)

	m.tmuxSendLiteralForAgent(agent, fullCmd)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)

	if isInference {
		go m.dismissInferencePrompts(agent)
	}

	now := time.Now()
	agent.State = StateRunning
	agent.StartedAt = &now
	agent.launchGen++
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

	// Deliver the bootstrap prompt once the CLI is ready — fire-and-forget,
	// same semantics as the old embedded delivery but gated on readiness.
	if deferredStartupKick != "" {
		go m.deliverStartupKick(agent, deferredStartupKick, agent.launchGen)
	}

	if agent.Config.CavemanMode != "" {
		switch backend {
		case "goose", "codex", "aider":
			go func(a *AgentProcess, cavemanMode string) {
				// Same readiness gate as kicks: a fixed post-launch delay
				// raced the CLI boot and could type /caveman into bash.
				if !m.waitForInputPromptForAgent(a) {
					m.logger.Warn("caveman activation skipped: CLI never reached input prompt",
						"agent", a.Name, "mode", cavemanMode)
					return
				}
				m.tmuxSendLiteralForAgent(a, "/caveman "+cavemanMode)
				time.Sleep(textToEnterDelay)
				m.tmuxSendEntersForAgent(a)
				m.logger.Info("sent caveman activation", "agent", a.Name, "mode", cavemanMode)
			}(agent, agent.Config.CavemanMode)
		}
	}

	return nil
}

// installCavemanForAgent runs the backend-specific caveman installer before
// the agent CLI starts. Auto-activating backends (claude, copilot, gemini)
// get caveman wired in so it's active from message one. Per-session backends
// (goose, codex, aider) get the skill pre-installed; activation happens via
// /caveman command sent after launch.
func (m *Manager) installCavemanForAgent(agent *AgentProcess, backend string) {
	mode := agent.Config.CavemanMode
	if mode == "" {
		return
	}

	home := "/data/home"
	if agent.UID == 0 {
		home = os.Getenv("HOME")
		if home == "" {
			home = "/root"
		}
	}

	m.logger.Info("installing caveman", "agent", agent.Name, "backend", backend, "mode", mode)

	var cmd *exec.Cmd
	switch backend {
	case "claude":
		cmd = exec.Command("npx", "-y", "github:JuliusBrussee/caveman", "--", "--only", "claude", "--mode", mode)
	case "copilot":
		cmd = exec.Command("npx", "-y", "github:JuliusBrussee/caveman", "--", "--only", "copilot", "--with-init", "--mode", mode)
	case "gemini":
		cmd = exec.Command("npx", "-y", "github:JuliusBrussee/caveman", "--", "--only", "gemini", "--mode", mode)
	case "goose":
		cmd = exec.Command("npx", "-y", "skills", "add", "JuliusBrussee/caveman", "-a", "goose")
	case "codex":
		cmd = exec.Command("npx", "-y", "skills", "add", "JuliusBrussee/caveman", "-a", "codex")
	case "aider":
		cmd = exec.Command("npx", "-y", "skills", "add", "JuliusBrussee/caveman", "-a", "aider-desk")
	default:
		m.logger.Info("caveman not supported for backend", "backend", backend)
		return
	}

	cmd.Dir = filepath.Join("/data/agents", agent.Name)
	cmd.Env = append(os.Environ(), "HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("caveman install failed", "agent", agent.Name, "error", err, "output", string(out))
	}
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

			showsLogin := paneShowsLoginPrompt(filtered)

			agent.paneMu.Lock()
			agent.lastPaneCapture = filtered
			agent.NeedsLogin = showsLogin
			agent.paneMu.Unlock()

			// Auto-restart agents stuck on the login prompt when a valid
			// token exists in the shared config.json. This handles the case
			// where a user authenticates via one agent's terminal and other
			// agents don't pick up the new token automatically.
			if showsLogin && configHasTokens() {
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

			// Detect fatal TLS/network errors that leave the agent visually
			// "ready" (the Copilot chrome shows ❯ and / commands) but actually
			// dead. These errors are transient — a restart will succeed once
			// the network recovers.
			if agent.Config.Backend == "copilot" && paneShowsFatalNetworkError(filtered) {
				sinceLastRestart := time.Since(agent.lastTokenRestart).Seconds()
				if sinceLastRestart >= float64(tlsErrorRestartCooldownSec) {
					m.logger.Warn("fatal network/TLS error detected, restarting agent",
						"agent", agent.Name,
					)
					agent.lastTokenRestart = time.Now()
					agent.LastError = "transient TLS/network error"
					go func() {
						if err := m.Restart(ctx, agent.Name); err != nil {
							m.logger.Warn("tls-error-triggered restart failed",
								"agent", agent.Name,
								"error", err,
							)
						}
					}()
					return
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
			// A consent/selection screen also contains "❯" but is NOT a
			// ready input prompt — sending a kick there feeds the menu.
			// Check the visible pane only: a dismissed consent screen
			// lingers in the scrollback that captureTmuxPaneForAgent sees.
			if paneShowsConsentScreen(m.captureVisiblePaneForAgent(agent)) {
				continue
			}
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

// inferencePaneCheck pairs an inference agent name with its captured visible
// pane for post-kick stall inspection outside the manager read lock.
type inferencePaneCheck struct {
	name string
	pane string
}

// CheckAndRestartCrashedAgents checks all running agents for crashed CLI
// processes (bare shell prompt with no child process) and restarts them.
// Returns the names of agents that were successfully restarted so the
// caller can send them a kick with their prompt template.
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
			)
			crashed = append(crashed, name)
			continue
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

	// Detect a crashed CLI (bare shell) or a CLI stuck on a consent screen
	// and restart before sending the kick. A consent pane contains "❯" so it
	// passes the marker check, but a kick typed into it is consumed by the
	// menu — or by bash once the default "No, exit" selection exits the CLI
	// (observed live: "-bash: NEVER: command not found").
	pane := m.captureVisiblePaneForAgent(agent)
	if !paneHasCLIMarker(pane) || paneShowsConsentScreen(pane) {
		m.logger.Warn("agent CLI crashed or stuck on consent screen, restarting before kick",
			"name", name, "consent_screen", paneShowsConsentScreen(pane))
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

	m.deliverKickLocked(agent, message, "send-kick")

	return nil
}

// deliverKickLocked types a message into the agent's CLI and records the
// kick bookkeeping (LastKick, history, stall watchdog, audit log). Callers
// must hold m.mu and must already have verified the CLI is ready for input
// (crash detect + waitForCLIReadyForAgent + waitForInputPromptForAgent) —
// this function does no readiness checking of its own.
func (m *Manager) deliverKickLocked(agent *AgentProcess, message, trigger string) {
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

	// Weak OSS models served over inference backends often answer a kick
	// with a prose plan addressed to a reader and execute zero tool calls
	// (observed live: litellm/vllm + deepseek-r1-14b produced a coherent
	// PLAN and returned to the idle prompt without running anything).
	// Append an action-forcing block here — where the effective backend is
	// knowable — instead of editing the kick templates, which are shared
	// with commercial CLI backends that do not need it.
	if IsInferenceBackend(effectiveBackend(agent)) {
		message += "\n\n" + inferenceKickActionSuffix
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

	// Arm the post-kick watchdog for inference agents: the watcher loop
	// sends a "continue" nudge if the pane freezes at an idle prompt, and
	// an action nudge if the model responds with prose but runs no tools.
	if IsInferenceBackend(effectiveBackend(agent)) {
		m.recordInferenceKick(agent, now)
	}

	snippet := message
	const maxSnippetLen = 120
	snippet = truncateStr(snippet, maxSnippetLen)
	record := KickRecord{Timestamp: now, Agent: agent.Name, Snippet: snippet}
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
		"name", agent.Name,
		"message_len", len(message),
		"preview", kickPreview,
		"trigger", trigger,
	)
}

// deliverStartupKick delivers a bootstrap prompt to a freshly launched agent
// once its CLI is actually ready for input, mirroring SendKick's readiness
// chain (CLI marker visible, input prompt shown, not parked on a consent
// screen — the consent check lives inside waitForInputPromptForAgent). It
// runs fire-and-forget in a goroutine, bounded by cliReadyTimeout +
// inputPromptTimeout. If the CLI never becomes ready the prompt is dropped
// with a warning rather than typed into a bare bash pane — the crash
// detector restarts the agent and the next launch builds a fresh bootstrap.
// gen is the agent's launch generation at spawn time; a mismatch at delivery
// time means the agent was relaunched while we waited (the new launch owns
// its own startup kick, so this one is stale and dropped).
func (m *Manager) deliverStartupKick(agent *AgentProcess, prompt string, gen int) {
	if !m.waitForCLIReadyForAgent(agent) {
		m.logger.Warn("startup kick dropped: CLI never became ready",
			"name", agent.Name, "session", agent.tmuxSession, "trigger", "startup")
		return
	}
	if !m.waitForInputPromptForAgent(agent) {
		m.logger.Warn("startup kick dropped: CLI never reached input prompt",
			"name", agent.Name, "session", agent.tmuxSession, "trigger", "startup")
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.agents[agent.Name]
	if !ok || current != agent || agent.State != StateRunning || agent.launchGen != gen {
		m.logger.Warn("startup kick dropped: agent restarted or stopped while waiting",
			"name", agent.Name, "trigger", "startup")
		return
	}
	m.deliverKickLocked(agent, prompt, "startup")
}

// tmuxSendLiteralForAgent sends text using the agent's tmux socket.
func (m *Manager) tmuxSendLiteralForAgent(agent *AgentProcess, text string) {
	_ = m.tmuxCmd(agent, "send-keys", "-t", agent.tmuxSession, "-l", text).Run()
}

// dismissInferencePrompts polls the tmux pane for Claude Code interactive
// prompts and auto-dismisses them. The "Bypass Permissions mode" consent
// screen and the custom-API-key approval prompt are handled first and
// explicitly (see confirmMenuOption): their default selections are negative
// ("No, exit" / "No (recommended)"), so confirming blind terminates the CLI
// or declines the seeded key.
// Other prompts are handled dynamically regardless of prompt text changes
// between Claude Code versions by:
//  1. Detecting "Enter to confirm" (universal prompt footer)
//  2. Finding the selected option (line with "❯" marker)
//  3. If selected option looks negative (contains "No" or "exit"), navigate
//     away from it before confirming
//  4. For "Press Enter to continue" screens, just press Enter
//
// The pane is polled fast for the first 10s — the consent screen appears
// within ~5-8s of launch and every second it lingers is a window for a kick
// to be swallowed by the menu — then at a relaxed interval.
//
// Stops when the main Claude Code input prompt appears ("esc to interrupt").
func (m *Manager) dismissInferencePrompts(agent *AgentProcess) {
	const (
		// promptFastPollWindow covers the launch window in which the consent
		// screen normally appears (~5-8s after CLI start).
		promptFastPollWindow   = 10 * time.Second
		promptFastPollInterval = 250 * time.Millisecond
		promptPollInterval     = 1 * time.Second
		promptDismissTimeout   = 60 * time.Second
		postKeystrokeDelay     = 500 * time.Millisecond
	)

	start := time.Now()
	deadline := start.Add(promptDismissTimeout)
	lastPane := ""

	for time.Now().Before(deadline) {
		interval := promptPollInterval
		if time.Since(start) < promptFastPollWindow {
			interval = promptFastPollInterval
		}
		time.Sleep(interval)

		pane := m.captureVisiblePaneForAgent(agent)
		if pane == "" {
			continue
		}

		// Bypass-permissions consent screen: handle first and explicitly,
		// even if the pane is unchanged since the last poll (a mistimed
		// keystroke must be retried, not skipped). The affirmative option
		// sits below the default "No, exit".
		if strings.Contains(pane, bypassConsentTitle) && !strings.Contains(pane, cliWorkingMarker) {
			m.logger.Info("accepting bypass-permissions consent", "agent", agent.Name)
			m.confirmMenuOption(agent, bypassConsentTitle, bypassConsentAcceptOption, "Down")
			lastPane = "" // re-capture fresh on the next pass
			continue
		}

		// Custom-API-key approval prompt: the affirmative "Yes" sits ABOVE
		// the default "No (recommended)" selection, so the generic
		// Down-then-Enter fallback below would decline it.
		if strings.Contains(pane, apiKeyPromptTitle) && !strings.Contains(pane, cliWorkingMarker) {
			m.logger.Info("approving seeded inference API key", "agent", agent.Name)
			m.confirmMenuOption(agent, apiKeyPromptTitle, apiKeyPromptAcceptOption, "Up")
			lastPane = ""
			continue
		}

		if pane == lastPane {
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
		selected := selectedMenuOption(pane)

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

// selectedMenuOption returns the trimmed text of the "❯"-selected line of an
// interactive CLI menu, or "" if no line is selected.
func selectedMenuOption(pane string) string {
	for _, line := range strings.Split(pane, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "❯") {
			return trimmed
		}
	}
	return ""
}

// confirmMenuOption drives an interactive CLI menu identified by title to the
// option whose text contains want, then confirms it with Enter. Navigation
// matches the "❯"-selected line text rather than pressing a fixed number of
// keys, so it lands on the right option whichever position it occupies (menu
// option order differs between Claude CLI versions). navKey is the arrow key
// to step with ("Down" or "Up"). Returns true once the option was confirmed
// or the screen is gone.
func (m *Manager) confirmMenuOption(agent *AgentProcess, title, want, navKey string) bool {
	const (
		// menuMaxNavigateSteps bounds arrow-key navigation; the handled menus
		// have 2 options, extra headroom covers future variants.
		menuMaxNavigateSteps = 4
		postKeystrokeDelay   = 500 * time.Millisecond
	)
	for step := 0; step < menuMaxNavigateSteps; step++ {
		pane := m.captureVisiblePaneForAgent(agent)
		if !strings.Contains(pane, title) || strings.Contains(pane, cliWorkingMarker) {
			return true // screen already dismissed
		}
		if strings.Contains(selectedMenuOption(pane), want) {
			m.tmuxSendKeysForAgent(agent, "Enter")
			time.Sleep(postKeystrokeDelay)
			return true
		}
		m.tmuxSendKeysForAgent(agent, navKey)
		time.Sleep(postKeystrokeDelay)
	}
	m.logger.Warn("inference menu: wanted option not reached",
		"agent", agent.Name, "title", title, "want", want)
	return false
}

const (
	// consentStuckGracePeriod is how long a consent screen must stay visible
	// across watcher passes before the agent counts as stuck. The launch-time
	// dismissal goroutine runs for 60s, so a screen still visible this long
	// after first being seen by the watcher means dismissal lost the race.
	consentStuckGracePeriod = 30 * time.Second
	// consentDismissCooldown is the minimum interval between watcher-triggered
	// dismissal passes for one agent, so a stubborn screen can't spam
	// keystroke goroutines (each dismissal pass itself polls for 60s).
	consentDismissCooldown = 2 * time.Minute
)

// clearConsentTracking resets the consent-stuck timer for an agent whose pane
// no longer shows a consent screen.
func (m *Manager) clearConsentTracking(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[name]; ok {
		agent.consentSeenAt = time.Time{}
	}
}

// dismissConsentIfStuck re-runs dismissInferencePrompts for an inference agent
// whose pane has shown a consent screen for longer than the grace period,
// subject to a per-agent cooldown. Called from the watcher loop
// (CheckAndRestartCrashedAgents) so an agent that lands on a consent screen
// after launch — e.g. a crash-recovery restart whose launch-time dismissal
// timed out — recovers instead of sitting stuck while kicks appear to succeed.
func (m *Manager) dismissConsentIfStuck(name string) {
	now := time.Now()
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return
	}
	if agent.consentSeenAt.IsZero() {
		agent.consentSeenAt = now
		m.mu.Unlock()
		return
	}
	stuckFor := now.Sub(agent.consentSeenAt)
	if stuckFor < consentStuckGracePeriod || now.Sub(agent.lastConsentDismiss) < consentDismissCooldown {
		m.mu.Unlock()
		return
	}
	agent.lastConsentDismiss = now
	m.mu.Unlock()

	m.logger.Warn("inference agent stuck on consent screen, re-running prompt dismissal",
		"name", name, "stuck_seconds", int(stuckFor.Seconds()))
	go m.dismissInferencePrompts(agent)
}

// inferenceKickActionSuffix is appended to every kick sent to an agent whose
// effective backend is a self-hosted inference backend (vllm/llm-d/litellm).
// Weak OSS models tend to answer a kick conversationally — describing steps
// for someone else to follow — instead of acting; this block demands
// immediate tool execution. Commercial CLI backends don't receive it.
const inferenceKickActionSuffix = "IMPORTANT — EXECUTE, DO NOT NARRATE: " +
	"You have real tools (Bash, file edit, gh). Perform the work NOW in " +
	"this session. Do not describe steps for someone else, do not summarize " +
	"a plan and stop. Begin immediately by running your first command. " +
	"Every response that contains no tool execution is a failure."

const (
	// inferenceKickStallTimeout is how long after a kick an unchanged, idle
	// pane counts as a stalled kick (message swallowed without a response).
	inferenceKickStallTimeout = 5 * time.Minute
	// inferenceStallNudgeMessage is the literal message typed into the CLI to
	// unstick a stalled kick.
	inferenceStallNudgeMessage = "continue"
	// cliInputPromptMarker is the CLI's idle input prompt indicator.
	cliInputPromptMarker = "❯"
	// inferenceActionNudgeGrace is the minimum time after a kick before the
	// no-action check may fire, so the watcher never misreads the brief
	// post-Enter window (kick echoed, spinner not yet rendered) as a
	// completed prose-only response.
	inferenceActionNudgeGrace = 2 * time.Minute
	// inferenceActionNudgeMessage is typed into the CLI when the model
	// answered a kick with prose only — a plan addressed to a reader with
	// zero tool executions (observed live with weak OSS models on
	// inference backends, e.g. deepseek-r1-14b via litellm/vllm).
	inferenceActionNudgeMessage = "You produced a plan but executed nothing. Execute it yourself NOW using your tools, starting with step 1. Do not reply with prose only."
	// inferenceMaxOutputTokensDefault caps CLAUDE_CODE_MAX_OUTPUT_TOKENS for
	// inference-backend agents. 16384 is a safe universal floor across the
	// commercial models operators point litellm at: Azure GPT-4o allows at
	// most 16384 completion tokens and 400s ("max_tokens is too large:
	// 128000. This model supports at most 16384 completion tokens") on
	// anything higher; GPT-4.1/GPT-5 and most vLLM/Claude backends meet or
	// exceed 16384. A previous 128000 value (chosen so verbose OSS models
	// would not truncate) made every request to a capped commercial model
	// fail. 16384 output tokens is still generous for agent work, so we
	// trade "never truncate huge OSS outputs" for "works on capped
	// commercial models" — the correct default.
	inferenceMaxOutputTokensDefault = 16384
	// cliActiveCounterMarker appears inside Claude Code's live activity
	// spinner, e.g. "✶ Infusing… (18s · ↓ 94 tokens)" (verified against
	// Claude Code v2.1.204). The completed form ("✻ Worked for 26s") has no
	// counter, so this distinguishes an in-flight response from a finished
	// one on versions whose footer no longer shows cliWorkingMarker.
	cliActiveCounterMarker = "s · ↓"
)

// toolSummaryRe matches Claude Code's collapsed tool-activity summary lines,
// rendered only when tools actually executed (verified against Claude Code
// v2.1.204): "Running 1 shell command…" while a Bash call is in flight, and
// "Ran 1 shell command" / "Read 1 file, ran 2 shell commands" once done.
// Edit/write variants are included for completeness across versions.
var toolSummaryRe = regexp.MustCompile(`(?i)\b(?:ran|running) \d+ shell command|\bread \d+ file|\bedited \d+ file|\bwrote \d+ file|\bupdated \d+ file`)

// expandedToolCallMarkers are literal fragments of Claude Code's expanded
// per-tool rendering. "⎿" is the result elbow drawn under a tool call
// (verified live on v2.1.204: "⎿  $ sleep 15 && echo probe3"); the
// "⏺ Name(" forms are the expanded tool-call headers older CLI versions
// render. A bare "⏺" is NOT a tool marker — v2.1.204 uses it as the bullet
// for every assistant response block, including pure prose.
var expandedToolCallMarkers = []string{
	"⎿",
	"⏺ Bash(",
	"⏺ Read(",
	"⏺ Write(",
	"⏺ Edit(",
	"⏺ Update(",
	"⏺ Search(",
	"⏺ Fetch(",
	"⏺ Task(",
}

// countToolMarkers counts tool-execution markers in captured pane content.
// The no-action watchdog compares the count after a kick against the count
// recorded at kick delivery: scrollback keeps markers from work done before
// the kick, so only an increase proves the model executed tools since.
func countToolMarkers(pane string) int {
	n := len(toolSummaryRe.FindAllStringIndex(pane, -1))
	for _, marker := range expandedToolCallMarkers {
		n += strings.Count(pane, marker)
	}
	return n
}

// paneShowsActiveWork reports whether the CLI is mid-response: either the
// legacy "esc to interrupt" footer hint or the live spinner counter is
// visible. The idle input prompt "❯" alone proves nothing on v2.1.204 —
// the input box stays rendered while a response streams.
func paneShowsActiveWork(pane string) bool {
	return strings.Contains(pane, cliWorkingMarker) || strings.Contains(pane, cliActiveCounterMarker)
}

// paneContentHash returns a short stable hash of pane content, used to detect
// whether a pane has changed since a kick was delivered.
func paneContentHash(pane string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(pane))
	return fmt.Sprintf("%016x", h.Sum64())
}

// recordInferenceKick arms the post-kick stall watchdog for an inference
// agent: remembers when the kick was delivered and what the pane looked like
// right after delivery. Caller must hold m.mu.
func (m *Manager) recordInferenceKick(agent *AgentProcess, at time.Time) {
	agent.lastInferKickAt = at
	agent.lastInferKickPane = paneContentHash(m.captureVisiblePaneForAgent(agent))
	agent.stallNudgeSent = false
	// Baseline for the no-action check: markers already in scrollback from
	// work done before this kick must not count as post-kick tool activity.
	agent.lastInferKickMarks = countToolMarkers(m.captureTmuxPaneForAgent(agent))
	agent.actionNudgeSent = false
}

// nudgeIfKickStalled watches an inference agent after a kick and corrects
// two distinct failure modes, sending at most one nudge each (so at most two
// combined nudges per kick):
//
//   - Frozen pane: the pane has not changed since the kick was delivered and
//     the stall timeout elapsed — the CLI swallowed the message. Sends the
//     "continue" nudge (counted in StallNudges).
//   - Prose-only response: the pane changed (the CLI consumed the kick), the
//     response completed back at the idle input prompt, but the tool-marker
//     count has not risen above the baseline recorded at kick delivery — the
//     model narrated a plan instead of acting. Sends the action nudge
//     (counted in ActionNudges).
//
// A CLI that is mid-response (paneShowsActiveWork) is always left alone, and
// post-kick tool activity disarms the watchdog entirely.
func (m *Manager) nudgeIfKickStalled(name, pane string) {
	now := time.Now()
	m.mu.Lock()
	agent, ok := m.agents[name]
	if !ok || agent.lastInferKickAt.IsZero() || agent.lastInferKickPane == "" {
		m.mu.Unlock()
		return
	}
	if paneShowsActiveWork(pane) || !strings.Contains(pane, cliInputPromptMarker) {
		m.mu.Unlock()
		return
	}
	sinceKick := now.Sub(agent.lastInferKickAt)

	if paneContentHash(pane) == agent.lastInferKickPane {
		// Frozen pane: the CLI never consumed the kick.
		if agent.stallNudgeSent || sinceKick < inferenceKickStallTimeout {
			m.mu.Unlock()
			return
		}
		agent.stallNudgeSent = true
		agent.StallNudges++
		totalNudges := agent.StallNudges
		m.mu.Unlock()

		m.logger.Warn("inference agent stalled after kick, sending continue nudge",
			"name", name,
			"minutes_since_kick", int(sinceKick.Minutes()),
			"total_nudges", totalNudges)
		m.tmuxSendLiteralForAgent(agent, inferenceStallNudgeMessage)
		time.Sleep(textToEnterDelay)
		m.tmuxSendEntersForAgent(agent)
		return
	}

	// The pane moved since the kick — the CLI consumed it and the response
	// completed (idle prompt, no active-work indicator). Check whether any
	// tools ran since the kick before declaring the response prose-only.
	if agent.actionNudgeSent || sinceKick < inferenceActionNudgeGrace {
		m.mu.Unlock()
		return
	}
	if countToolMarkers(m.captureTmuxPaneForAgent(agent)) > agent.lastInferKickMarks {
		// Real tool activity since the kick — the agent is acting. Disarm.
		agent.lastInferKickPane = ""
		m.mu.Unlock()
		return
	}
	agent.actionNudgeSent = true
	agent.ActionNudges++
	totalActionNudges := agent.ActionNudges
	m.mu.Unlock()

	m.logger.Warn("inference agent answered kick with prose only, sending action nudge",
		"name", name,
		"minutes_since_kick", int(sinceKick.Minutes()),
		"total_action_nudges", totalActionNudges)
	m.tmuxSendLiteralForAgent(agent, inferenceActionNudgeMessage)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)
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
	cliReadyPollInterval    = 2 * time.Second
	cliReadyTimeout         = 60 * time.Second
	inputPromptPollInterval = 2 * time.Second
	inputPromptTimeout      = 120 * time.Second
	// preLaunchShellClearDelay gives bash time to process the C-c that
	// clears stale PS2 quote-continuation state before the launch command
	// is typed into the pane.
	preLaunchShellClearDelay = 500 * time.Millisecond
	// cliBootGraceSeconds is how long after StartedAt a bare pane (no CLI
	// marker) is tolerated before CheckAndRestartCrashedAgents treats it as a
	// crash. It matches cliReadyTimeout (60s) so a still-booting CLI is never
	// restarted underneath itself, which would spawn a second concurrent CLI.
	cliBootGraceSeconds = 60
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
	// NeedsLogin is written by the pane poller under paneMu.
	needsLogin := a.NeedsLogin
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
		NeedsLogin:      needsLogin,
		StallNudges:     a.StallNudges,
		ActionNudges:    a.ActionNudges,
		HasLaunched:     a.HasLaunched,
		LaunchedMode:    a.LaunchedMode,
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
		"litellm": "claude",
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
	sharedClaudeCredentialPath = "/data/home/.claude/.credentials.json"
	sharedConfigDesiredMode    = 0o660
	tokenRestartCooldownSec    = 60  // minimum seconds between token-triggered restarts per agent
	expiredTokenHangTimeoutSec = 180 // blank pane after this many seconds triggers token purge + restart
	tlsErrorRestartCooldownSec = 120 // minimum seconds between TLS-error-triggered restarts per agent
)

// CopilotUserTokenPath is where the dashboard's device-flow login persists
// the Copilot OAuth token; injected into agents as COPILOT_GITHUB_TOKEN.
const CopilotUserTokenPath = "/data/copilot-user-token"

// loginPromptPatterns are substrings that indicate an agent is stuck on a
// login/authentication screen (Copilot text prompts, Claude Code OAuth flow,
// GitHub device flow). Each must be distinctive enough to never appear in
// ordinary agent output.
var loginPromptPatterns = []string{
	"/login",
	"sign in to use",
	"Sign in to use",
	"authenticate to use",
	"Authenticate to use",
	"log in to use",
	"Log in to use",
	// Claude Code OAuth sign-in screen
	"Use the url below to sign in",
	"Paste code here if prompted",
	"Select login method",
	"/cai/oauth/authorize",
	// GitHub device-flow screen (Copilot CLI)
	"Enter one-time code",
	"github.com/login/device",
}

// fatalNetworkErrorPatterns are substrings that indicate a transient TLS or
// network failure killed the agent at startup. These errors leave the Copilot
// chrome visible (❯, / commands) so paneShowsCLIReady returns true, but the
// agent is dead and will never recover without a restart.
var fatalNetworkErrorPatterns = []string{
	"invalid peer certificate",
	"BadSignature",
	"fetch failed",
}

// paneShowsFatalNetworkError returns true if any line contains a fatal
// TLS/network error pattern that requires an agent restart.
func paneShowsFatalNetworkError(lines []string) bool {
	for _, line := range lines {
		for _, pat := range fatalNetworkErrorPatterns {
			if strings.Contains(line, pat) {
				return true
			}
		}
	}
	return false
}

// configHasTokens returns true if either the Copilot config or Claude
// credentials file contains a valid token. Used to decide whether an agent
// stuck on a login prompt can be auto-restarted.
func configHasTokens() bool {
	if claude.HasValidToken(sharedClaudeCredentialPath) {
		return true
	}
	return copilotConfigHasTokens()
}

// copilotConfigHasTokens reads the shared Copilot config.json, strips single-line
// // comments (which Copilot CLI sometimes writes), parses the JSON, and returns
// true if the "copilotTokens" field has at least one entry.
func copilotConfigHasTokens() bool {
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
			if matchesAuthError(output) {
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
	claudeInferenceSettingsPath  = "/tmp/.claude-inference-settings.json"
	claudeInferenceHomePrefix = "/tmp/.claude-inference-home-"
)

// inferenceHomePath returns the per-agent inference HOME directory.
func inferenceHomePath(agentName string) string {
	return claudeInferenceHomePrefix + agentName
}

// codexBackend is the backend name for the OpenAI Codex CLI.
const codexBackend = "codex"

// codexHomePrefix is the per-agent CODEX_HOME directory prefix. Each agent gets
// its own dir so Codex's owner-gated app-server sees a directory the agent UID
// actually owns (a shared, merely group-writable dir is not sufficient for
// Codex, unlike claude/copilot). Lives on the persistent /data volume.
const codexHomePrefix = "/data/home/.codex-"

// codexHomePath returns the per-agent CODEX_HOME directory.
func codexHomePath(agentName string) string {
	return codexHomePrefix + agentName
}

// codexSharedAuthFile is the login credential that a `codex login` (ChatGPT
// sign-in) or an OPENAI_API_KEY setup writes: a user running codex without
// CODEX_HOME set lands in $HOME/.codex, i.e. /data/home/.codex (group-writable
// so any agent UID can refresh the token). Because each agent has its OWN
// CODEX_HOME (for the app-server owner check), that per-agent dir would start
// with no auth — so a single sign-in would not reach the agents, and they would
// prompt for sign-in again. setupCodexHome bridges this by symlinking each
// agent's auth.json to this shared file, so ONE login propagates to every agent
// and token refreshes are shared.
const codexSharedAuthFile = "/data/home/.codex/auth.json"

// setupCodexHome pre-creates the agent's CODEX_HOME directory AS the agent, so
// it is owned by the agent UID. Codex 0.144.1 refuses to create CODEX_HOME
// itself ("CODEX_HOME ... does not exist") and its app-server requires the
// current UID to own the dir. The manager runs as dev and cannot chown, so —
// mirroring the tmux-dir setup — it runs mkdir via su-exec as the agent user.
// It also symlinks the agent's auth.json to the shared login file so a single
// `codex login` reaches every agent. No-op for root agents (UID 0), which own
// /data/home already.
func (m *Manager) setupCodexHome(agent *AgentProcess) {
	if agent.UID <= 0 {
		return
	}
	dir := codexHomePath(agent.Name)
	agentUser := fmt.Sprintf("hive-%s", agent.Name)
	if err := exec.Command("su-exec", agentUser, "mkdir", "-p", dir).Run(); err != nil {
		m.logger.Warn("failed to pre-create codex home", "agent", agent.Name, "dir", dir, "error", err)
	}
	// Bridge auth: symlink the per-agent auth.json to the shared login file so a
	// single sign-in propagates to all agents. `ln -sfn` is idempotent and
	// overwrites a stale regular-file auth.json left by an earlier codex version.
	// The symlink target need not exist yet (a later `codex login` creates it).
	authLink := filepath.Join(dir, "auth.json")
	if err := exec.Command("su-exec", agentUser, "ln", "-sfn", codexSharedAuthFile, authLink).Run(); err != nil {
		m.logger.Warn("failed to link codex auth", "agent", agent.Name, "link", authLink, "error", err)
	}
}

// inferenceConfigMigrationVersion matches the Claude CLI internal config
// migration version so the CLI skips first-run migration prompts.
const inferenceConfigMigrationVersion = 13

// inferenceUserConfigSeed returns the required top-level keys for an inference
// agent's ~/.claude.json. These skip the first-run setup (onboarding,
// migrations) and pre-approve the per-agent inference API key.
//
// NOTE: "bypassPermissionsModeAccepted" does NOT suppress the interactive
// "Bypass Permissions mode" consent dialog — verified live against Claude CLI
// v2.1.190 and v2.1.204, the dialog is gated only on the settings key
// "skipDangerousModePermissionPrompt" (see inferenceSettingsSeed). The
// .claude.json key is still read by non-interactive CLI paths (e.g. the --bg
// bypass check), so it stays in the seed for those.
// apiKeyApprovalSuffixLen is how many trailing characters of an API key the
// Claude CLI compares against customApiKeyResponses.approved entries
// (key.slice(-20), verified in CLI v2.1.190). Seeding only the full key
// leaves keys longer than this unapproved — "sk-hive-" plus an agent name
// over 12 chars — so the CLI shows the "Detected a custom API key" prompt,
// whose default selection is "No (recommended)".
const apiKeyApprovalSuffixLen = 20

func inferenceUserConfigSeed(agentName string) map[string]any {
	apiKey := "sk-hive-" + agentName
	approved := []string{apiKey}
	if len(apiKey) > apiKeyApprovalSuffixLen {
		approved = append(approved, apiKey[len(apiKey)-apiKeyApprovalSuffixLen:])
	}
	return map[string]any{
		"hasCompletedOnboarding":        true,
		"opusProMigrationComplete":      true,
		"sonnet1m45MigrationComplete":   true,
		"migrationVersion":              inferenceConfigMigrationVersion,
		"bypassPermissionsModeAccepted": true,
		"customApiKeyResponses": map[string]any{
			"approved": approved,
			"rejected": []string{},
		},
	}
}

// inferenceSettingsSeed returns the required keys for an inference agent's
// Claude settings.json — both ~/.claude/settings.json (userSettings) and the
// standalone file passed via --settings (flagSettings).
//
// "skipDangerousModePermissionPrompt" is the key that actually suppresses the
// "Bypass Permissions mode" consent dialog. Verified live against Claude CLI
// v2.1.190 and v2.1.204 with a scratch HOME: the interactive dialog is gated
// only on this settings key (honored from userSettings, localSettings,
// flagSettings, or policySettings), and accepting the dialog interactively
// persists this same key into ~/.claude/settings.json. Seeding
// bypassPermissionsModeAccepted in .claude.json does NOT suppress the dialog
// on either version, and IS_SANDBOX=1 does not suppress it either. Without
// this key every --dangerously-skip-permissions launch shows a consent menu
// whose default selection is "No, exit" — if dismissal loses the race, the
// CLI exits and the pane degrades to bare bash.
func inferenceSettingsSeed() map[string]any {
	return map[string]any{
		"permissions":                       map[string]any{"allow": []any{}, "deny": []any{}},
		"hasCompletedOnboarding":            true,
		"bypassPermissions":                 true,
		"hasAcknowledgedDisclaimer":         true,
		"skipDangerousModePermissionPrompt": true,
	}
}

// seedClaudeUserConfig writes or repairs the inference agent's .claude.json.
// Unlike a plain exists-check, this merges required keys into an existing
// file that is missing some of them (e.g. seeded by an older hive version
// without bypassPermissionsModeAccepted) and rewrites a file that fails to
// parse. A complete, parseable file is left untouched.
func (m *Manager) seedClaudeUserConfig(agentName, path string) {
	m.seedJSONFile(agentName, path, inferenceUserConfigSeed(agentName))
	m.mergeApprovedAPIKeys(agentName, path)
}

// mergeApprovedAPIKeys ensures every seeded approved API key form is present
// in an existing customApiKeyResponses.approved list. The top-level merge in
// seedJSONFile skips a key that already exists, which would leave configs
// seeded by older hive versions without the truncated key form the CLI
// actually matches against (see apiKeyApprovalSuffixLen).
func (m *Manager) mergeApprovedAPIKeys(agentName, path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	existing := map[string]any{}
	if err := json.Unmarshal(data, &existing); err != nil {
		return
	}
	responses, ok := existing["customApiKeyResponses"].(map[string]any)
	if !ok {
		return
	}
	approved, _ := responses["approved"].([]any)
	present := make(map[string]bool, len(approved))
	for _, v := range approved {
		if s, ok := v.(string); ok {
			present[s] = true
		}
	}
	seedResponses, _ := inferenceUserConfigSeed(agentName)["customApiKeyResponses"].(map[string]any)
	seedApproved, _ := seedResponses["approved"].([]string)
	changed := false
	for _, key := range seedApproved {
		if !present[key] {
			approved = append(approved, key)
			changed = true
		}
	}
	if !changed {
		return
	}
	responses["approved"] = approved
	out, err := json.Marshal(existing)
	if err != nil {
		return
	}
	if err := os.WriteFile(path, out, 0o666); err != nil {
		m.logger.Warn("failed to write inference config", "agent", agentName, "path", path, "error", err)
	}
}

// seedClaudeSettingsFile writes or repairs a Claude settings.json, merging in
// the keys from inferenceSettingsSeed (e.g. a file seeded by an older hive
// version without skipDangerousModePermissionPrompt gains the key instead of
// being skipped by an exists-check).
func (m *Manager) seedClaudeSettingsFile(agentName, path string) {
	m.seedJSONFile(agentName, path, inferenceSettingsSeed())
}

// seedJSONFile merges required top-level keys into the JSON object stored at
// path. Missing keys are added, existing keys are never overwritten, and a
// file that fails to parse is rewritten from the seed alone. A complete,
// parseable file is left untouched.
func (m *Manager) seedJSONFile(agentName, path string, seed map[string]any) {
	existing := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if jsonErr := json.Unmarshal(data, &existing); jsonErr != nil {
			m.logger.Warn("inference config unparseable, rewriting",
				"agent", agentName, "path", path, "error", jsonErr)
			existing = map[string]any{}
		}
	}

	needsWrite := false
	for key, value := range seed {
		if _, ok := existing[key]; !ok {
			existing[key] = value
			needsWrite = true
		}
	}
	if !needsWrite {
		return
	}

	data, err := json.Marshal(existing)
	if err != nil {
		m.logger.Warn("failed to marshal inference config", "agent", agentName, "path", path, "error", err)
		return
	}
	if err := os.WriteFile(path, data, 0o666); err != nil {
		m.logger.Warn("failed to write inference config", "agent", agentName, "path", path, "error", err)
	}
}

// ensureClaudeSettings creates a per-agent writable HOME directory for inference
// agents with pre-populated .claude/settings.json. Each agent gets its own dir
// to avoid cross-agent permission conflicts when Claude Code creates session
// files. Directories use 0o777 so the agent UID can create subdirs freely
// (hive runs as dev, not root, so chown is not available).
//
// The .claude.json and settings.json seeds are repaired on every call
// (missing keys merged in, corrupt files rewritten) so agents launched by
// older hive versions pick up newly required keys instead of being skipped
// by an exists-check.
func (m *Manager) ensureClaudeSettings(agentName string, uid int) {
	homePath := inferenceHomePath(agentName)
	settingsDir := filepath.Join(homePath, ".claude")
	settingsFile := filepath.Join(settingsDir, "settings.json")

	if err := os.MkdirAll(settingsDir, 0o777); err != nil {
		m.logger.Warn("failed to create claude inference home", "agent", agentName, "error", err)
		return
	}
	// Write (or repair) both settings files: the HOME userSettings file and
	// the standalone file passed via --settings. The CLI honors
	// skipDangerousModePermissionPrompt from either source.
	m.seedClaudeSettingsFile(agentName, settingsFile)
	m.seedClaudeSettingsFile(agentName, claudeInferenceSettingsPath)
	// Pre-populate (or repair) .claude.json so the CLI skips first-run setup.
	m.seedClaudeUserConfig(agentName, filepath.Join(homePath, ".claude.json"))
	m.ensureWorldWritable(homePath)
}

// ensureWorldWritable walks the tree and sets dirs to 0o777, files to 0o666.
func (m *Manager) ensureWorldWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Mode().Perm() != 0o777 {
				_ = os.Chmod(path, 0o777)
			}
		} else {
			if info.Mode().Perm() != 0o666 {
				_ = os.Chmod(path, 0o666)
			}
		}
		return nil
	})
}

// normalizeModelName converts YAML-friendly model names to the format each
// CLI backend expects. Claude CLI uses hyphens (claude-opus-4-7), while
// Copilot and other backends use dots (claude-opus-4.7).
//
// Self-hosted inference backends (vllm, llm-d, litellm) are the outbound
// gateway model id verbatim — the string must match an entitled model on the
// gateway EXACTLY (prefixes like "Azure/", dots vs hyphens, case). Rewriting
// it (e.g. "Azure/gpt-4" -> "Azure/gpt.4", "gpt-4o-2024-08-06" ->
// "gpt-4o-2024-08.06") produces a model the team is not entitled to and the
// gateway 403s ("team not allowed to access model") even for entitled models.
// So never normalize inference model names — pass them through untouched.
func normalizeModelName(model, backend string) string {
	if backend == "claude" || IsInferenceBackend(backend) {
		return model
	}
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
	if agent.Config.Tools != nil {
		if effectiveMode := agent.Config.Tools.EffectiveMode(); effectiveMode != "" {
			vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", effectiveMode, false})
		} else {
			vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", mode.String(), false})
		}
	} else {
		vars = append(vars, agentEnvPair{"HIVE_AGENT_MODE", mode.String(), false})
	}
	modeFile := fmt.Sprintf("/tmp/.hive-mode-%s", agent.Name)
	if err := os.WriteFile(modeFile, []byte(mode.String()), 0o644); err != nil {
		m.logger.Warn("agentBootstrapEnv: mode file write failed", "file", modeFile, "error", err)
	}
	// Plain proxy URL without userinfo — Claude Code's native binary fails
	// to open a socket when the URL contains username:password@ (FailedToOpenSocket).
	// Agent identification uses UID-based /proc/net/tcp lookup instead of
	// Proxy-Authorization headers. GIT_TERMINAL_PROMPT=0 prevents git from
	// prompting for proxy credentials.
	proxyURL := fmt.Sprintf("http://127.0.0.1:%d", proxyListenPort)
	vars = append(vars, agentEnvPair{"HTTPS_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HTTP_PROXY", proxyURL, false})
	vars = append(vars, agentEnvPair{"HIVE_PROXY_AGENT", agent.Name, false})
	vars = append(vars, agentEnvPair{"GIT_TERMINAL_PROMPT", "0", false})
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
		// Cap the CLI output-token budget at a value every commercial model
		// litellm may front will accept. A prior 128000 (chosen so verbose
		// OSS models would not truncate) exceeds Azure GPT-4o's 16384
		// completion-token cap, so every request 400s with
		// "max_tokens is too large: 128000. This model supports at most
		// 16384 completion tokens". See inferenceMaxOutputTokensDefault.
		// TODO: the gateway 400 body names the model's real cap ("supports
		// at most N completion tokens"); a future enhancement could parse it
		// to auto-adjust per-model instead of using a universal floor.
		vars = append(vars, agentEnvPair{"CLAUDE_CODE_MAX_OUTPUT_TOKENS", strconv.Itoa(inferenceMaxOutputTokensDefault), false})
	}
	if m.copilotAuthToken != "" {
		vars = append(vars, agentEnvPair{"COPILOT_GITHUB_TOKEN", m.copilotAuthToken, true})
	}
	if m.claudeAuthToken != "" && backend == "claude" {
		vars = append(vars, agentEnvPair{"CLAUDE_CODE_OAUTH_TOKEN", m.claudeAuthToken, true})
	}
	// BD_DIR tells the `bd` CLI where to read/write beads. Without this,
	// bd falls back to cwd (/data/agents/<name>) instead of the configured
	// beads_dir (/data/beads/<name>), causing a path mismatch with the
	// dashboard and advisory digest.
	if agent.Config.BeadsDir != "" {
		vars = append(vars, agentEnvPair{"BD_DIR", agent.Config.BeadsDir, false})
	}
	if agent.Config.CavemanMode != "" {
		vars = append(vars, agentEnvPair{"HIVE_CAVEMAN_MODE", agent.Config.CavemanMode, false})
	}
	// GIT_SSL_CAINFO only — NOT SSL_CERT_FILE (that breaks Copilot API TLS)
	vars = append(vars, agentEnvPair{"GIT_SSL_CAINFO", proxyCACertPath, false})
	if agent.UID > 0 {
		vars = append(vars, agentEnvPair{"HIVE_AGENT_TOKEN_CACHE", ghpkg.AgentTokenCachePath(agent.Name), false})
	}
	if agent.UID > 0 {
		home := "/data/home"
		if IsInferenceBackend(backend) {
			home = inferenceHomePath(agent.Name)
		}
		vars = append(vars, agentEnvPair{"HOME", home, false})
	}

	// Codex CLI 0.144.1's in-process app-server performs OWNER-gated operations
	// on files under CODEX_HOME (helper-binary "PATH alias" symlinks under
	// tmp/arg0, sqlite state). The shared /data/home/.codex is owned by dev
	// (the entrypoint chowns it group-writable + setgid), which claude/copilot
	// tolerate but Codex does not — every non-owner agent UID fails with
	// "failed to start embedded app server: Operation not permitted (os error 1)".
	// The manager launches the codex binary DIRECTLY (not via agent-launch.sh),
	// so CODEX_HOME must be set here. Give each agent its own dir; codex will
	// NOT create it (it errors "CODEX_HOME ... does not exist"), so it is
	// pre-created AS the agent below in setupCodexHome.
	if backend == codexBackend {
		vars = append(vars, agentEnvPair{"CODEX_HOME", codexHomePath(agent.Name), false})
	}

	for _, conn := range agent.Config.Connections {
		if conn.Type != "api" {
			continue
		}
		envName := conn.EnvName
		if envName == "" {
			envName = "HIVE_CONN_" + strings.ToUpper(strings.ReplaceAll(conn.Name, "-", "_")) + "_URL"
		}
		vars = append(vars, agentEnvPair{envName, conn.URI, false})
		if conn.Auth != nil && conn.Auth.Type == "env" && conn.Auth.EnvVar != "" {
			if tokenVal := os.Getenv(conn.Auth.EnvVar); tokenVal != "" {
				vars = append(vars, agentEnvPair{conn.Auth.EnvVar, tokenVal, true})
			}
		}
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
	agent.Config.Paused = true
	if m.persistPauseCallback != nil {
		m.persistPauseCallback(name, true)
	}
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
	agent.Config.Paused = false
	if m.persistPauseCallback != nil {
		m.persistPauseCallback(name, false)
	}
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

	// Terminate the agent's CLI process(es) before recreating the session.
	// reapAgentCLI matches by the HIVE_AGENT env marker, so it works whether or
	// not UID isolation is enabled — killAgentProcesses (UID-based) is a no-op
	// when agents share the dev UID, which let stale claude processes on old
	// models survive a model/backend switch and keep hitting the gateway.
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
func (m *Manager) reapAgentCLI(agent *AgentProcess) int {
	const procPath = "/proc"
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

	// Terminate the agent's CLI process(es) before recreating the session.
	// reapAgentCLI matches by the HIVE_AGENT env marker, so it works whether or
	// not UID isolation is enabled — killAgentProcesses (UID-based) is a no-op
	// when agents share the dev UID, which let stale claude processes on old
	// models survive a model/backend switch and keep hitting the gateway.
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

	// A pin blocks the governor's auto-selection, never a user's explicit
	// switch: retarget the pin to the new model so the pin state is
	// unchanged (still pinned) while the change takes effect.
	if agent.PinnedModel != "" {
		agent.PinnedModel = model
		m.logger.Info("agent model pin retargeted by user switch", "name", name, "model", model)
	}

	agent.ModelOverride = model
	m.logger.Info("agent model override set", "name", name, "model", model)

	effectiveBackend := agent.Config.Backend
	if agent.BackendOverride != "" {
		effectiveBackend = agent.BackendOverride
	}
	if IsInferenceBackend(effectiveBackend) && m.inferenceRouteCallback != nil {
		m.inferenceRouteCallback(name, effectiveBackend, model)
	}
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

	if IsInferenceBackend(backend) && m.inferenceRouteCallback != nil {
		model := agent.ModelOverride
		if model == "" {
			model = agent.Config.Model
		}
		m.inferenceRouteCallback(name, backend, model)
	} else if !IsInferenceBackend(backend) && m.clearInferenceRouteCallback != nil {
		m.clearInferenceRouteCallback(name)
	}
	return nil
}

// RefreshInferenceRoutes re-fires the inference route callback for every
// agent whose effective backend matches, so endpoint or credential changes
// (e.g. a governor LiteLLM config save) take effect on live agents without
// a restart.
func (m *Manager) RefreshInferenceRoutes(backend string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inferenceRouteCallback == nil || !IsInferenceBackend(backend) {
		return
	}
	for name, agent := range m.agents {
		effective := agent.Config.Backend
		if agent.BackendOverride != "" {
			effective = agent.BackendOverride
		}
		if effective != backend {
			continue
		}
		model := agent.ModelOverride
		if model == "" {
			model = agent.Config.Model
		}
		m.inferenceRouteCallback(name, backend, model)
	}
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

// toolRulesToLaunchCmd builds a backend-specific CLI command from ToolsConfig.
func toolRulesToLaunchCmd(binary, model, backend string, tools *config.ToolsConfig, isInference bool) string {
	denies := tools.DenyPatterns()

	switch backend {
	case "claude":
		bareFlag := ""
		if isInference {
			bareFlag = fmt.Sprintf(" --bare --settings %s", claudeInferenceSettingsPath)
		}
		cmd := fmt.Sprintf("%s --model %s --dangerously-skip-permissions%s", binary, model, bareFlag)
		for _, p := range denies {
			cmd += fmt.Sprintf(" --disallowed-tools '%s'", p)
		}
		return cmd
	case "copilot":
		hasGHDeny := false
		for _, p := range denies {
			if strings.Contains(p, "github") {
				hasGHDeny = true
				break
			}
		}
		cmd := fmt.Sprintf("%s --model %s --no-auto-update --allow-all", binary, model)
		if !hasGHDeny {
			cmd += " --enable-all-github-mcp-tools"
		}
		for _, p := range denies {
			copilotPattern := strings.ReplaceAll(p, "mcp__github__", "github(")
			if strings.HasPrefix(copilotPattern, "github(") {
				copilotPattern += ")"
			}
			cmd += fmt.Sprintf(" --deny-tool='%s'", copilotPattern)
		}
		return cmd
	default:
		cmd := binary
		if model != "" {
			cmd = fmt.Sprintf("%s --model %s", binary, model)
		}
		return cmd
	}
}

// connectionMCPFlags builds MCP-related launch flags from connection configs.
func connectionMCPFlags(conns []config.ConnectionConfig, backend string) string {
	var flags string
	for _, conn := range conns {
		if conn.Type != "mcp" || conn.URI == "" {
			continue
		}
		switch backend {
		case "claude":
			flags += fmt.Sprintf(" --mcp-server '%s'", conn.URI)
		}
	}
	return flags
}

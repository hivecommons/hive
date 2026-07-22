package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

const (
	specialistReadinessLeasePrefix = "hive-specialist-readiness:"
	specialistCommandTimeout       = 10 * time.Second
	specialistDispatchReadyPrefix  = "HIVE_SPECIALIST_DISPATCH_READY_"
)

var (
	// ErrSpecialistTaskNotDelivered is returned only when Manager proved that
	// the task message never reached deliverKickLocked. Starting the persistent
	// CLI alone does not count as task delivery.
	ErrSpecialistTaskNotDelivered = errors.New("specialist task was not delivered")
	// ErrSpecialistTaskLeaseAbsent distinguishes a restarted controller with no
	// in-memory lease from a dangerous lease held for a different task.
	ErrSpecialistTaskLeaseAbsent = errors.New("specialist task lease is absent")
	// ErrSpecialistTaskDeliveryAmbiguous means the exact task bytes reached the
	// specialist pane but Hive could not prove that the final submit key was
	// accepted. The durable lease must remain held and must not be redispatched.
	ErrSpecialistTaskDeliveryAmbiguous = errors.New("specialist task delivery is ambiguous")
)

// SpecialistDispatchReadyMarker returns the short task-specific terminal
// marker that ends an inline specialist prompt. Manager requires the marker
// before delivery so the exact complete prompt remains task-bound even though
// Codex compacts large bracketed pastes in the rendered composer.
func SpecialistDispatchReadyMarker(taskID string) string {
	if !workOrderIDPattern.MatchString(taskID) {
		return ""
	}
	digest := strings.TrimPrefix(taskID, "swo-")
	return specialistDispatchReadyPrefix + digest[:16]
}

var (
	specialistAgentNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	specialistRoleAllowlist    = map[SpecialistRole]struct{}{
		SpecialistQuality:      {},
		SpecialistCIMaintainer: {},
		SpecialistSecurity:     {},
		SpecialistArchitect:    {},
		SpecialistScanner:      {},
	}
	specialistGitHubCredentialNames = []string{
		"GH_TOKEN",
		"GITHUB_TOKEN",
		"HIVE_GITHUB_TOKEN",
		"GH_ENTERPRISE_TOKEN",
		"GITHUB_ENTERPRISE_TOKEN",
		"HIVE_AGENT_TOKEN_CACHE",
		"GH_APP_ID",
		"GH_APP_INSTALLATION_ID",
		"GH_APP_KEY_FILE",
		"GH_APP_PRIVATE_KEY",
		"GITHUB_APP_ID",
		"GITHUB_APP_INSTALLATION_ID",
		"GITHUB_APP_KEY_FILE",
		"GITHUB_APP_PRIVATE_KEY",
		"GITHUB_APP_PRIVATE_KEY_FILE",
		"GITHUB_APP_CLIENT_ID",
		"GITHUB_APP_CLIENT_SECRET",
		"HIVE_GITHUB_APP_ID",
		"HIVE_GITHUB_APP_INSTALLATION_ID",
		"HIVE_GITHUB_APP_KEY_FILE",
		"HIVE_GITHUB_APP_PRIVATE_KEY",
	}
)

// SpecialistSessionIdentity is the stable identity of one existing Hive
// specialist's persistent tmux session. Work orders bind this value into the
// durable mailbox lease and receipt.
type SpecialistSessionIdentity struct {
	Specialist              SpecialistRole `json:"specialist"`
	AgentName               string         `json:"agent_name"`
	AgentID                 string         `json:"agent_id"`
	Backend                 string         `json:"backend"`
	ConfiguredBackend       string         `json:"configured_backend,omitempty"`
	ExecutorBackend         string         `json:"executor_backend,omitempty"`
	BackendParityClaimed    bool           `json:"backend_parity_claimed"`
	ContainmentProfile      string         `json:"containment_profile,omitempty"`
	ExecutorModel           string         `json:"executor_model,omitempty"`
	ExecutorConfigSHA256    string         `json:"executor_config_sha256,omitempty"`
	ExecutorAuthorizationID string         `json:"-"`
	SessionID               string         `json:"session_id"`
	TmuxSession             string         `json:"tmux_session"`
	TmuxSocket              string         `json:"tmux_socket,omitempty"`
	WorkDir                 string         `json:"work_dir"`
	ProviderSHA256          string         `json:"provider_sha256"`
}

// SpecialistDispatchRequest assigns one already-persisted, immutable mailbox
// work order to the matching existing Hive specialist.
type SpecialistDispatchRequest struct {
	TaskID                  string              `json:"task_id"`
	Specialist              SpecialistRole      `json:"specialist"`
	Message                 string              `json:"message"`
	Order                   SpecialistWorkOrder `json:"order,omitempty"`
	Lease                   SpecialistLease     `json:"lease,omitempty"`
	RequestSHA256           string              `json:"request_sha256,omitempty"`
	SessionID               string              `json:"session_id,omitempty"`
	AgentName               string              `json:"agent_name,omitempty"`
	AgentID                 string              `json:"agent_id,omitempty"`
	ConfiguredBackend       string              `json:"configured_backend,omitempty"`
	ExecutorBackend         string              `json:"executor_backend,omitempty"`
	BackendParityClaimed    bool                `json:"backend_parity_claimed"`
	ContainmentProfile      string              `json:"containment_profile,omitempty"`
	ProviderSHA256          string              `json:"provider_sha256,omitempty"`
	ExecutorModel           string              `json:"executor_model,omitempty"`
	ExecutorConfigSHA256    string              `json:"executor_config_sha256,omitempty"`
	ExecutorAuthorizationID string              `json:"-"`
}

type SpecialistDispatchResult struct {
	SpecialistSessionIdentity
	Started bool `json:"started"`
	Reused  bool `json:"reused"`
}

type specialistDispatchRuntime struct {
	start     func(context.Context, string) error
	restart   func(context.Context, string) error
	send      func(string, string, string) error
	configure func(*AgentProcess) error
	resolve   func(*AgentProcess) (string, error)
}

// SpecialistManagerOptions binds the proposal-only specialist runtime to its
// private work root and exact native Codex executable.
type SpecialistManagerOptions struct {
	WorkDir      string
	CodexCommand string
	CodexHome    string
}

// NewSpecialistManager constructs the existing agent manager with a caller-
// owned absolute work root and a sealed copy of the exact native Codex
// executable already selected by integrated setup. It never consults or
// mutates HIVE_WORK_DIR, PATH, or AgentConfig.LaunchCmd.
func NewSpecialistManager(agents map[string]config.AgentConfig, logger *slog.Logger, project ProjectContext, options SpecialistManagerOptions) (*Manager, error) {
	workDir := options.WorkDir
	if strings.TrimSpace(workDir) == "" || !filepath.IsAbs(workDir) {
		return nil, errors.New("specialist agent work directory must be absolute")
	}
	workDir = filepath.Clean(workDir)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create specialist agent work directory: %w", err)
	}
	if err := os.Chmod(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("secure specialist agent work directory: %w", err)
	}
	provider, err := sealSpecialistProvider(options.CodexCommand, workDir)
	if err != nil {
		return nil, err
	}
	keepProvider := false
	defer func() {
		if !keepProvider {
			_ = cleanupSpecialistProvider(provider)
		}
	}()
	codexHome, err := prepareSpecialistCodexHome(options.CodexHome, workDir)
	if err != nil {
		return nil, err
	}
	keepCodexHome := false
	defer func() {
		if !keepCodexHome {
			_ = cleanupSpecialistCodexHome(codexHome)
		}
	}()
	for name := range agents {
		if !specialistAgentNamePattern.MatchString(name) {
			return nil, fmt.Errorf("agent name %q is not safe for an isolated work directory", name)
		}
		directory := filepath.Join(workDir, name)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create specialist work directory for %s: %w", name, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, fmt.Errorf("secure specialist work directory for %s: %w", name, err)
		}
	}

	manager := newManager(agents, logger, project, workDir)
	manager.specialistProvider = provider
	manager.specialistCodexHome = codexHome
	// The controller and its proposal-only specialist must share ownership of
	// the private 0700 mailbox below WorkDir. The ordinary manager's optional
	// per-agent UID map points at entrypoint-owned /data/agents directories and
	// cannot safely chown an arbitrary controller state path after privileges
	// are dropped. Keep this explicitly rooted manager on the controller's OS
	// account; Codex workspace containment and the stripped GitHub authority
	// remain the specialist boundary.
	manager.useControllerOwnershipForSpecialists()
	sessionRoot := filepath.ToSlash(workDir) + "\x00" + provider.SHA256
	if runtime.GOOS == "windows" {
		sessionRoot = strings.ToLower(sessionRoot)
	}
	digest := sha256.Sum256([]byte(sessionRoot))
	namespace := hex.EncodeToString(digest[:6])
	manager.specialistNamespace = namespace
	for name, process := range manager.agents {
		process.tmuxSession = fmt.Sprintf("hive-%s-%s", name, namespace)
		process.specialistProviderSHA256 = provider.SHA256
	}
	keepProvider = true
	keepCodexHome = true
	return manager, nil
}

func (m *Manager) useControllerOwnershipForSpecialists() {
	m.uidMap = nil
	for _, process := range m.agents {
		process.UID = 0
		process.tmuxSocket = ""
	}
}

// SpecialistProviderSHA256 returns the content identity shared by every
// proposal-only specialist session without exposing its local sealed path.
func (m *Manager) SpecialistProviderSHA256() string {
	if m == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.specialistProvider == nil {
		return ""
	}
	return m.specialistProvider.SHA256
}

// CheckSpecialistRole verifies role/config/backend/session readiness without
// sending a task. Starting or restarting the CLI is prompt-free, so this can
// run before provider model accounting.
func (m *Manager) CheckSpecialistRole(ctx context.Context, role SpecialistRole) (SpecialistSessionIdentity, error) {
	if m.specialistChildDispatcherConfigured() {
		return m.checkSpecialistChildRole(ctx, role)
	}
	if !m.legacySpecialistManagerConfigured() {
		return SpecialistSessionIdentity{}, errors.New("ordinary manager has no explicitly injected contained specialist child executor")
	}
	return m.checkSpecialistRole(ctx, role, m.specialistRuntime())
}

func (m *Manager) legacySpecialistManagerConfigured() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	configured := m.specialistProvider != nil || m.specialistNamespace != ""
	m.mu.RUnlock()
	return configured
}

func (m *Manager) checkSpecialistRole(ctx context.Context, role SpecialistRole, runtime specialistDispatchRuntime) (SpecialistSessionIdentity, error) {
	leaseID := specialistReadinessLeasePrefix + string(role)
	result, err := m.prepareSpecialist(ctx, role, leaseID, runtime)
	if err != nil {
		return SpecialistSessionIdentity{}, err
	}
	m.releaseSpecialistLease(string(role), leaseID)
	return result.SpecialistSessionIdentity, nil
}

// DispatchSpecialistTask acquires an in-memory task lease, lazily starts the
// persistent specialist, and delivers the task through Manager.SendKick's
// readiness path. The lease remains held until ReleaseSpecialistTask.
func (m *Manager) DispatchSpecialistTask(ctx context.Context, request SpecialistDispatchRequest) (SpecialistDispatchResult, error) {
	if m.specialistChildDispatcherConfigured() {
		result, err := m.dispatchSpecialistChildTask(ctx, request)
		if err != nil {
			if errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) {
				return result, err
			}
			return result, fmt.Errorf("%w: %v", ErrSpecialistTaskNotDelivered, err)
		}
		return result, nil
	}
	if !m.legacySpecialistManagerConfigured() {
		return SpecialistDispatchResult{}, fmt.Errorf("%w: ordinary manager has no explicitly injected contained specialist child executor", ErrSpecialistTaskNotDelivered)
	}
	result, err := m.dispatchSpecialistTask(ctx, request, m.specialistRuntime())
	if err != nil {
		if errors.Is(err, ErrSpecialistTaskDeliveryAmbiguous) {
			return result, err
		}
		// The checked specialist paste path reports post-paste failures with the
		// typed ambiguous error above. Every other Manager error therefore proves
		// the task bytes were not delivered, even if Codex was started first.
		return result, fmt.Errorf("%w: %v", ErrSpecialistTaskNotDelivered, err)
	}
	return result, nil
}

// InspectSpecialistRole verifies the exact existing namespaced session and
// sealed provider without starting, restarting, configuring, or kicking it.
// Recovery of a live durable lease uses this path so a new controller cannot
// destroy the still-running specialist before consuming its completion.
func (m *Manager) InspectSpecialistRole(ctx context.Context, role SpecialistRole) (SpecialistSessionIdentity, error) {
	if m.specialistChildDispatcherConfigured() {
		return m.inspectSpecialistChildRole(ctx, role)
	}
	if !m.legacySpecialistManagerConfigured() {
		return SpecialistSessionIdentity{}, errors.New("ordinary manager has no explicitly injected contained specialist child executor")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	commandCtx, cancel := context.WithTimeout(ctx, specialistCommandTimeout)
	defer cancel()
	return m.inspectSpecialistRole(commandCtx, role, m.specialistTmuxSessionExists)
}

func (m *Manager) inspectSpecialistRole(ctx context.Context, role SpecialistRole, sessionExists func(context.Context, *AgentProcess) bool) (SpecialistSessionIdentity, error) {
	if !isAllowedSpecialistRole(role) {
		return SpecialistSessionIdentity{}, fmt.Errorf("unsupported specialist role %q", role)
	}
	if sessionExists == nil {
		return SpecialistSessionIdentity{}, errors.New("specialist session inspector is required")
	}
	name := string(role)
	m.mu.RLock()
	if m.specialistsDown {
		m.mu.RUnlock()
		return SpecialistSessionIdentity{}, errors.New("specialist manager is shut down")
	}
	agent := m.agents[name]
	if agent == nil {
		m.mu.RUnlock()
		return SpecialistSessionIdentity{}, fmt.Errorf("specialist agent %s is not configured", role)
	}
	snapshot := AgentProcess{
		Name: agent.Name, ID: agent.ID, Config: agent.Config, State: agent.State, PID: agent.PID, UID: agent.UID,
		HasLaunched: agent.HasLaunched, specialistReady: agent.specialistReady,
		BackendOverride: agent.BackendOverride, specialistProviderSHA256: agent.specialistProviderSHA256,
		tmuxSession: agent.tmuxSession, tmuxSocket: agent.tmuxSocket,
	}
	provider := m.specialistProvider
	namespace := m.specialistNamespace
	root := m.workDir
	m.mu.RUnlock()
	if !snapshot.Config.Enabled || effectiveBackend(&snapshot) != "codex" || m.agentMode(&snapshot) != ModeAdvisory {
		return SpecialistSessionIdentity{}, fmt.Errorf("specialist agent %s is not an enabled advisory Codex role", role)
	}
	if provider == nil || snapshot.specialistProviderSHA256 == "" || snapshot.specialistProviderSHA256 != provider.SHA256 {
		return SpecialistSessionIdentity{}, errors.New("specialist session is not bound to the current sealed provider")
	}
	if namespace == "" || snapshot.tmuxSession != fmt.Sprintf("hive-%s-%s", role, namespace) {
		return SpecialistSessionIdentity{}, errors.New("specialist session namespace is invalid")
	}
	if err := revalidateSpecialistProvider(provider); err != nil {
		return SpecialistSessionIdentity{}, err
	}
	if !sessionExists(ctx, &snapshot) {
		return SpecialistSessionIdentity{}, fmt.Errorf("specialist agent %s has no existing namespaced session", role)
	}
	return specialistResult(&snapshot, role, false, true, root).SpecialistSessionIdentity, nil
}

func (m *Manager) specialistTmuxSessionExists(ctx context.Context, agent *AgentProcess) bool {
	base := m.tmuxBaseArgs(agent)
	args := append([]string(nil), base[1:]...)
	args = append(args, "has-session", "-t", agent.tmuxSession)
	if agent.UID > 0 {
		user := fmt.Sprintf("hive-%s", agent.Name)
		return exec.CommandContext(ctx, "su-exec", append([]string{user, base[0]}, args...)...).Run() == nil
	}
	return exec.CommandContext(ctx, base[0], args...).Run() == nil
}

func (m *Manager) dispatchSpecialistTask(ctx context.Context, request SpecialistDispatchRequest, runtime specialistDispatchRuntime) (SpecialistDispatchResult, error) {
	if !workOrderIDPattern.MatchString(request.TaskID) {
		return SpecialistDispatchResult{}, errors.New("specialist task ID must be an immutable work-order ID")
	}
	if strings.TrimSpace(request.Message) == "" || strings.IndexByte(request.Message, 0) >= 0 {
		return SpecialistDispatchResult{}, errors.New("specialist task message is required")
	}
	if len(request.Message) > maxSpecialistDispatchMessageBytes {
		return SpecialistDispatchResult{}, errors.New("specialist task message exceeds size limit")
	}

	result, err := m.prepareSpecialist(ctx, request.Specialist, request.TaskID, runtime)
	if err != nil || result.Reused {
		return result, err
	}
	if err := runtime.send(result.AgentName, request.Message, request.TaskID); err != nil {
		m.releaseSpecialistLease(result.AgentName, request.TaskID)
		return SpecialistDispatchResult{}, fmt.Errorf("dispatch specialist task: %w", err)
	}
	return result, nil
}

// ReleaseSpecialistTask explicitly relinquishes the role's in-memory task
// lease after the controller has durably consumed the specialist receipt.
func (m *Manager) ReleaseSpecialistTask(role SpecialistRole, taskID string) error {
	if m.specialistChildDispatcherConfigured() {
		return m.releaseSpecialistChildTask(role, taskID)
	}
	if !m.legacySpecialistManagerConfigured() {
		return errors.New("ordinary manager has no explicitly injected contained specialist child executor")
	}
	if !isAllowedSpecialistRole(role) {
		return fmt.Errorf("unsupported specialist role %q", role)
	}
	if !workOrderIDPattern.MatchString(taskID) {
		return errors.New("specialist task ID must be an immutable work-order ID")
	}
	name := string(role)
	m.mu.Lock()
	defer m.mu.Unlock()
	leased := m.specialistLeases[name]
	if leased == "" {
		return fmt.Errorf("%w: specialist %s", ErrSpecialistTaskLeaseAbsent, role)
	}
	if leased != taskID {
		return fmt.Errorf("specialist %s is leased to task %s", role, leased)
	}
	delete(m.specialistLeases, name)
	return nil
}

func (m *Manager) specialistRuntime() specialistDispatchRuntime {
	return specialistDispatchRuntime{
		start:     m.Start,
		restart:   m.Restart,
		send:      m.sendKick,
		configure: m.configureSpecialistSession,
		resolve:   m.specialistBackendBinary,
	}
}

func (m *Manager) prepareSpecialist(ctx context.Context, role SpecialistRole, taskID string, runtime specialistDispatchRuntime) (result SpecialistDispatchResult, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !isAllowedSpecialistRole(role) {
		return result, fmt.Errorf("unsupported specialist role %q", role)
	}
	name := string(role)

	m.mu.Lock()
	if m.specialistsDown {
		m.mu.Unlock()
		return result, errors.New("specialist manager is shut down")
	}
	if m.specialistLeases == nil {
		m.specialistLeases = make(map[string]string)
	}
	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s is not configured", role)
	}
	if !agent.Config.Enabled {
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s is disabled", role)
	}
	if agent.Config.Role != "" && agent.Config.Role != name {
		m.mu.Unlock()
		return result, fmt.Errorf("agent %s is configured for role %s, not %s", name, agent.Config.Role, role)
	}
	if agent.Config.CavemanMode != "" {
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s cannot install or activate runtime skills", role)
	}
	if mode := m.agentMode(agent); mode != ModeAdvisory {
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s effective mode is %s; ADVISORY is required", role, mode)
	}
	if err := validateSpecialistConnections(agent.Config.Connections); err != nil {
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s: %w", role, err)
	}
	if leased := m.specialistLeases[name]; leased != "" {
		if leased == taskID && !strings.HasPrefix(taskID, specialistReadinessLeasePrefix) {
			if agent.State != StateRunning || !agent.specialistReady || !agent.HasLaunched || agent.LaunchedMode != ModeAdvisory {
				m.mu.Unlock()
				return result, fmt.Errorf("specialist agent %s has task %s leased without a ready advisory session", role, taskID)
			}
			result = specialistResult(agent, role, false, true, m.workDir)
			m.mu.Unlock()
			return result, nil
		}
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s is leased to task %s", role, leased)
	}
	if agent.State == StateRunning && (!agent.HasLaunched || agent.LaunchedMode != ModeAdvisory) {
		launchedMode := "unknown"
		if agent.HasLaunched {
			launchedMode = agent.LaunchedMode.String()
		}
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s is already running in %s mode; refusing dispatch", role, launchedMode)
	}
	if agent.State == StatePaused || agent.Paused {
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s is paused", role)
	}

	backend := effectiveBackend(agent)
	wasRunning := agent.State == StateRunning
	needsRestart := wasRunning && !agent.specialistReady
	needsStart := agent.State == StateStopped || agent.State == StateFailed
	if !wasRunning && !needsStart {
		m.mu.Unlock()
		return result, fmt.Errorf("specialist agent %s is in unsupported state %s", role, agent.State)
	}
	agent.specialistReady = true
	if needsStart {
		// A stable tmux session can survive a controller restart. Do not adopt an
		// unknown pre-isolation CLI merely because its pane still has a marker.
		agent.forceRelaunch = true
	}
	m.specialistLeases[name] = taskID
	m.mu.Unlock()

	keepLease := false
	defer func() {
		if err != nil && !keepLease {
			m.releaseSpecialistLease(name, taskID)
		}
	}()
	if _, resolveErr := runtime.resolve(agent); resolveErr != nil {
		return result, fmt.Errorf("specialist backend %s is unavailable: %w", backend, resolveErr)
	}
	if needsRestart {
		if err = runtime.restart(ctx, name); err != nil {
			return result, fmt.Errorf("restart specialist agent %s with isolated authority: %w", role, err)
		}
	} else if needsStart {
		if err = runtime.start(ctx, name); err != nil {
			return result, fmt.Errorf("start specialist agent %s: %w", role, err)
		}
	}

	m.mu.RLock()
	agent = m.agents[name]
	valid := agent != nil && m.specialistLeases[name] == taskID && agent.State == StateRunning && agent.specialistReady && agent.HasLaunched && agent.LaunchedMode == ModeAdvisory
	m.mu.RUnlock()
	if !valid {
		return result, fmt.Errorf("specialist agent %s did not reach a running advisory session", role)
	}
	if err = runtime.configure(agent); err != nil {
		return result, fmt.Errorf("isolate specialist agent %s environment: %w", role, err)
	}
	result = specialistResult(agent, role, needsStart || needsRestart, false, m.workDir)
	keepLease = true
	return result, nil
}

// specialistBackendBinary binds proposal-only specialist sessions to the
// exact native provider executable that integrated setup already selected and
// authenticated. Ordinary Manager agents retain their existing backend/PATH
// resolution and arbitrary AgentConfig.LaunchCmd strings are never evaluated.
func (m *Manager) specialistBackendBinary(agent *AgentProcess) (string, error) {
	if agent == nil {
		return "", errors.New("specialist agent is required")
	}
	backend := effectiveBackend(agent)
	if !agent.specialistReady {
		return backendBinary(backend)
	}
	if backend != "codex" {
		return "", fmt.Errorf("exact specialist provider executable is supported only for codex, not %s", backend)
	}
	if m.specialistProvider == nil {
		return "", errors.New("specialist codex executable is not bound")
	}
	if err := revalidateSpecialistProvider(m.specialistProvider); err != nil {
		return "", err
	}
	return m.specialistProvider.Path, nil
}

func specialistResult(agent *AgentProcess, role SpecialistRole, started, reused bool, root string) SpecialistDispatchResult {
	return SpecialistDispatchResult{
		SpecialistSessionIdentity: SpecialistSessionIdentity{
			Specialist:     role,
			AgentName:      agent.Name,
			AgentID:        agent.ID,
			Backend:        effectiveBackend(agent),
			SessionID:      agent.tmuxSession,
			TmuxSession:    agent.tmuxSession,
			TmuxSocket:     agent.tmuxSocket,
			WorkDir:        filepath.Join(root, agent.Name),
			ProviderSHA256: agent.specialistProviderSHA256,
		},
		Started: started,
		Reused:  reused,
	}
}

func (m *Manager) releaseSpecialistLease(name, taskID string) {
	m.mu.Lock()
	if m.specialistLeases[name] == taskID {
		delete(m.specialistLeases, name)
	}
	m.mu.Unlock()
}

func isAllowedSpecialistRole(role SpecialistRole) bool {
	_, ok := specialistRoleAllowlist[role]
	return ok
}

func validateSpecialistConnections(connections []config.ConnectionConfig) error {
	for _, connection := range connections {
		if connection.Type == "mcp" {
			return fmt.Errorf("MCP connection %q is not permitted for proposal-only specialist dispatch", connection.Name)
		}
		if connection.Auth != nil {
			return fmt.Errorf("authenticated connection %q is not permitted for proposal-only specialist dispatch", connection.Name)
		}
		identity := strings.ToLower(connection.Name + " " + connection.URI)
		if strings.Contains(identity, "github") {
			return fmt.Errorf("GitHub connection %q is not permitted for proposal-only specialist dispatch", connection.Name)
		}
	}
	return nil
}

func isSpecialistGitHubCredential(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	for _, exact := range specialistGitHubCredentialNames {
		if name == exact {
			return true
		}
	}
	return strings.HasPrefix(name, "GH_APP_") || strings.HasPrefix(name, "GITHUB_APP_") || strings.HasPrefix(name, "HIVE_GITHUB_APP_")
}

func (m *Manager) specialistEnvironmentPairs(agent *AgentProcess) []agentEnvPair {
	isolationRoot := filepath.Join(m.workDir, ".hive-specialist-isolation", agent.Name)
	return []agentEnvPair{
		{Key: "GH_CONFIG_DIR", Value: filepath.Join(isolationRoot, "gh"), Secret: false},
		{Key: "GH_PROMPT_DISABLED", Value: "1", Secret: false},
		{Key: "GIT_TERMINAL_PROMPT", Value: "0", Secret: false},
		{Key: "GCM_INTERACTIVE", Value: "Never", Secret: false},
		{Key: "GIT_ASKPASS", Value: "", Secret: false},
		{Key: "SSH_ASKPASS_REQUIRE", Value: "never", Secret: false},
		{Key: "GIT_CONFIG_NOSYSTEM", Value: "1", Secret: false},
		{Key: "GIT_CONFIG_GLOBAL", Value: filepath.Join(isolationRoot, "gitconfig"), Secret: false},
		{Key: "GIT_CONFIG_COUNT", Value: "2", Secret: false},
		{Key: "GIT_CONFIG_KEY_0", Value: "credential.interactive", Secret: false},
		{Key: "GIT_CONFIG_VALUE_0", Value: "false", Secret: false},
		{Key: "GIT_CONFIG_KEY_1", Value: "credential.helper", Secret: false},
		{Key: "GIT_CONFIG_VALUE_1", Value: "", Secret: false},
	}
}

func (m *Manager) configureSpecialistSession(agent *AgentProcess) error {
	if agent == nil || !agent.specialistReady {
		return errors.New("specialist session is not marked ready")
	}
	isolationRoot := filepath.Join(m.workDir, ".hive-specialist-isolation", agent.Name)
	ghConfig := filepath.Join(isolationRoot, "gh")
	home := filepath.Join(isolationRoot, "home")
	for _, directory := range []string{
		isolationRoot,
		ghConfig,
		home,
		filepath.Join(home, ".config"),
		filepath.Join(home, ".cache"),
		filepath.Join(home, ".local", "share"),
		filepath.Join(home, "tmp"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	gitConfig := filepath.Join(isolationRoot, "gitconfig")
	if err := os.WriteFile(gitConfig, nil, 0o600); err != nil {
		return err
	}

	names := append([]string(nil), specialistGitHubCredentialNames...)
	show := m.tmuxCmd(agent, "show-environment", "-t", agent.tmuxSession)
	if output, err := show.Output(); err == nil {
		for _, line := range strings.Split(string(output), "\n") {
			name := strings.TrimPrefix(strings.SplitN(line, "=", 2)[0], "-")
			if isSpecialistGitHubCredential(name) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	previous := ""
	for _, name := range names {
		if name == previous {
			continue
		}
		previous = name
		if err := m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, "-u", name).Run(); err != nil {
			return fmt.Errorf("unset %s in specialist session: %w", name, err)
		}
	}
	for _, pair := range m.specialistEnvironmentPairs(agent) {
		if err := m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, pair.Key, pair.Value).Run(); err != nil {
			return fmt.Errorf("set %s in specialist session: %w", pair.Key, err)
		}
	}
	return nil
}

// ShutdownSpecialists permanently closes every exact session in this
// specialist manager's namespace. It deliberately checks all five existing
// specialist roles, including a stale session left by an earlier controller
// process before the current Manager marked that role prepared.
func (m *Manager) ShutdownSpecialists(ctx context.Context) error {
	return m.shutdownSpecialists(ctx, m.killSpecialistSession)
}

func (m *Manager) shutdownSpecialists(ctx context.Context, kill func(context.Context, *AgentProcess) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	if m.specialistsClosed {
		m.mu.Unlock()
		return nil
	}
	m.specialistsDown = true
	agents := make([]*AgentProcess, 0, len(specialistRoleAllowlist))
	for role := range specialistRoleAllowlist {
		if process := m.agents[string(role)]; process != nil && process.specialistProviderSHA256 != "" &&
			m.specialistNamespace != "" && process.tmuxSession == fmt.Sprintf("hive-%s-%s", role, m.specialistNamespace) {
			if process.cancel != nil {
				process.cancel()
			}
			agents = append(agents, process)
		}
	}
	m.mu.Unlock()

	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	var failures []string
	for _, process := range agents {
		commandCtx, cancel := context.WithTimeout(ctx, specialistCommandTimeout)
		err := kill(commandCtx, process)
		cancel()
		// A detached Codex child can outlive tmux. Reap only the exact
		// HIVE_AGENT + HIVE_SPECIALIST_NAMESPACE identity owned by this manager.
		m.reapAgentCLI(process)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", process.Name, err))
			continue
		}
		m.mu.Lock()
		process.State = StateStopped
		process.PID = 0
		process.StartedAt = nil
		process.cancel = nil
		delete(m.specialistLeases, process.Name)
		m.mu.Unlock()
	}
	if len(failures) != 0 {
		return fmt.Errorf("shutdown specialist sessions: %s", strings.Join(failures, "; "))
	}
	providerErr := cleanupSpecialistProvider(m.specialistProvider)
	codexHomeErr := cleanupSpecialistCodexHome(m.specialistCodexHome)
	if providerErr != nil || codexHomeErr != nil {
		return errors.Join(
			wrapSpecialistCleanupError("provider seal", providerErr),
			wrapSpecialistCleanupError("Codex home", codexHomeErr),
		)
	}
	m.mu.Lock()
	m.specialistProvider = nil
	m.specialistCodexHome = ""
	m.specialistsClosed = true
	m.mu.Unlock()
	return nil
}

func wrapSpecialistCleanupError(name string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("cleanup specialist %s: %w", name, err)
}

func (m *Manager) killSpecialistSession(ctx context.Context, agent *AgentProcess) error {
	base := m.tmuxBaseArgs(agent)
	args := append([]string(nil), base[1:]...)
	args = append(args, "kill-session", "-t", agent.tmuxSession)
	var command *exec.Cmd
	if agent.UID > 0 {
		user := fmt.Sprintf("hive-%s", agent.Name)
		command = exec.CommandContext(ctx, "su-exec", append([]string{user, base[0]}, args...)...)
	} else {
		command = exec.CommandContext(ctx, base[0], args...)
	}
	if err := command.Run(); err != nil {
		// A stopped/no-longer-present session is already the desired state. A
		// running session must fail closed so cleanup never reports a false stop.
		// A manager that never launched a role may also be closed on a platform
		// without tmux; there is no session to clean in that case.
		if errors.Is(err, exec.ErrNotFound) && agent.State != StateRunning {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && agent.State != StateRunning {
			return nil
		}
		return fmt.Errorf("kill tmux session %s: %w", agent.tmuxSession, err)
	}
	return nil
}

var codexSpecialistDisabledFeatures = []string{
	"apps",
	"auth_elicitation",
	"browser_use",
	"browser_use_external",
	"browser_use_full_cdp_access",
	"code_mode",
	"code_mode_host",
	"code_mode_only",
	"computer_use",
	"enable_fanout",
	"enable_mcp_apps",
	"hooks",
	"image_generation",
	"multi_agent",
	"multi_agent_v2",
	"plugins",
	"remote_plugin",
	"search_tool",
	"skill_mcp_dependency_install",
	"standalone_web_search",
	"tool_call_mcp_elicitation",
	"tool_search",
	"tool_suggest",
	"web_search_cached",
	"web_search_request",
}

// codexSpecialistLaunchCmd keeps Codex interactive inside the tmux pane with no
// model-tool write surface. Hive supplies the complete work order inline and
// consumes Codex's controller-owned session journal; the model never writes a
// mailbox result. The minimal read profile deliberately excludes the target
// checkout, controller state, ambient home, and credentials from model tools.
// Interactive Codex does not
// accept the exec-only --ignore-user-config, --ignore-rules, or --ephemeral
// flags; the private CODEX_HOME and strict inline configuration provide those
// isolation properties for the persistent specialist session.
func codexSpecialistLaunchCmd(binary, model, workDir string) string {
	command := specialistShellArgument(binary)
	if model != "" {
		command += " --model " + specialistShellArgument(model)
	}
	for _, feature := range codexSpecialistDisabledFeatures {
		command += " --disable " + feature
	}
	command += " --cd " + specialistShellArgument(workDir)
	command += " --ask-for-approval never"
	command += " --config " + specialistShellArgument(`default_permissions="hive_specialist_mailbox"`)
	filesystem := `permissions.hive_specialist_mailbox.filesystem={":minimal"="read"}`
	command += " --config " + specialistShellArgument(filesystem)
	command += " --config " + specialistShellArgument(`permissions.hive_specialist_mailbox.network.enabled=false`)
	command += " --config 'web_search=\"disabled\"'"
	command += " --config 'project_doc_max_bytes=0' --config 'project_doc_fallback_filenames=[]' --strict-config"
	return command
}

func specialistShellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

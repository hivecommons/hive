package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/kubestellar/hive/v2/pkg/claude"
	"github.com/kubestellar/hive/v2/pkg/config"
	ghpkg "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/pushbroker"
	"github.com/kubestellar/hive/v2/pkg/sandbox"
	"github.com/kubestellar/hive/v2/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
)

type ProcessState string

const (
	StateIdle    ProcessState = "idle"
	StateRunning ProcessState = "running"
	StateStopped ProcessState = "stopped"
	StateFailed  ProcessState = "failed"
	StatePaused  ProcessState = "paused"
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

var defaultTmuxSocket string

// BreakerTrigger is the distinct PausedTrigger stamped on every pause the fleet
// breaker performs. It serves two jobs: the audit log attributes the pause to
// the breaker, and ReleaseBreaker uses it as the guard that distinguishes a
// pause the breaker still owns (safe to auto-resume) from one an operator
// re-applied during the breaker window (must stay paused). Anything whose
// current PausedTrigger != BreakerTrigger was last paused by something else.
const BreakerTrigger = "fleet-breaker"

type AgentProcess struct {
	Name              string
	ID                string
	Config            config.AgentConfig
	State             ProcessState
	PID               int
	UID               int
	StartedAt         *time.Time
	LastKick          *time.Time
	Paused            bool
	PausedAt          time.Time
	PausedReason      string
	PausedTrigger     string
	PinnedCLI         string
	PinnedModel       string
	ModelOverride     string
	BackendOverride   string
	RestartCount      int
	OutputBuffer      *RingBuffer
	lastPaneCapture   []string
	paneMu            sync.RWMutex
	KickHistory       []KickRecord
	LastKickMessage   string
	KickRefused       bool
	KickRefusalReason string
	LaunchedMode      AgentMode
	HasLaunched       bool
	tmuxSession       string
	tmuxSocket        string
	cancel            context.CancelFunc
	forceRelaunch     bool
	BootstrapOverride string    // when set, replaces buildBootstrapPrompt output
	LastError         string    // captured from bare copilot diagnostic launch
	lastTokenRestart  time.Time // cooldown for auto-restart after token detection
	NeedsLogin        bool      // true when pane shows a login prompt
	// LastPaneChange is when the agent's tmux pane content last CHANGED, as
	// observed by the 3s pane poller. It is the spoke's only evidence of an
	// agent actually doing something: State says what the manager intends,
	// StartedAt says when the CLI launched, and LastKick says when the
	// governor last spoke to it — none of them move when a running,
	// authenticated CLI sits there producing nothing. Written under paneMu by
	// pollTmuxOutputForAgent alongside lastPaneCapture; zero until the poller
	// has seen two differing captures, which reads as "unknown", never "idle".
	LastPaneChange     time.Time
	consentSeenAt      time.Time // watcher: when a consent screen was first seen in the pane
	lastConsentDismiss time.Time // watcher: cooldown for re-running dismissInferencePrompts
	lastInferKickAt    time.Time // stall watchdog: when the last kick was delivered to an inference agent
	lastInferKickPane  string    // stall watchdog: hash of the visible pane just after kick delivery
	stallNudgeSent     bool      // stall watchdog: at most one nudge per kick
	StallNudges        int       // total post-kick stall nudges sent (surfaced to the dashboard)
	launchGen          int       // increments per launch; stale deliverStartupKick goroutines check it and drop
	lastInferKickMarks int       // no-action watchdog: tool-marker count in pane+scrollback just after kick delivery
	actionNudgeSent    bool      // no-action watchdog: at most one action nudge per kick
	ActionNudges       int       // total prose-only-response action nudges sent (surfaced to the dashboard)
	// sandboxResumeAfterCancel is set when an operator resumes a paused
	// sandbox agent while the canceled sandbox goroutine is still draining.
	// The completion handler then turns the expected cancellation into Idle
	// instead of Failed.
	sandboxResumeAfterCancel bool

	// awaitingBobKey marks an agent that launchInTmux parked in StateFailed
	// for the single, fully-recoverable reason "bob backend with no API key".
	// It is what makes RelaunchBobAgentsAwaitingKey precise: StateFailed alone
	// is ambiguous (a missing backend binary, a copilot auth timeout, and a
	// hung diagnostic all land there too), and relaunching those on a bob-key
	// save would restart agents whose problem the key does not fix.
	// Set only on the missing-key branch, cleared on every launch attempt.
	awaitingBobKey bool

	// lastLaunchFailureBanner is the exact in-pane shell line typed by the most
	// recent aborted launch (see announceLaunchFailureInPane), "" after a
	// successful launch. A launch aborted before send-keys used to leave a
	// BARE interactive shell with the only explanation in the hive log — an
	// operator attached via ttyd saw a silent prompt and nothing else
	// (observed live: backend "bob" on a hive whose launch was refused). The
	// pane itself is the production surface for the banner; this field exists
	// so tests can assert the announcement actually happened without a tmux
	// server.
	lastLaunchFailureBanner string
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
	Org             string
	Repos           []string
	PrimaryRepoName string
	ACMMLevel       int
	PRsAllowed      bool
	PolicyDir       string
	// AppAuthoredPRs mirrors config github.app_authored_prs: when true, push-
	// capable agents get the App installation token as GITHUB_TOKEN so the GitHub
	// MCP server authors PRs/commits as the App bot. Default false → no token is
	// injected and behavior is unchanged (opt-in per hive).
	AppAuthoredPRs bool
}

func (p ProjectContext) PrimaryRepo() string {
	if strings.TrimSpace(p.PrimaryRepoName) != "" {
		return strings.TrimPrefix(p.PrimaryRepoName, p.Org+"/")
	}
	if len(p.Repos) > 0 {
		return strings.TrimPrefix(p.Repos[0], p.Org+"/")
	}
	return ""
}

type Manager struct {
	agents   map[string]*AgentProcess
	idToName map[string]string
	mu       sync.RWMutex
	// thrashMu guards thrash — its own mutex, NEVER m.mu: the breaker runs on
	// the output-capture goroutines, and taking m.mu there risks the startup
	// re-entrancy deadlock class (see the 2026-07 provisionWG incident).
	thrashMu         sync.Mutex
	thrash           map[string]*thrashState
	logger           *slog.Logger
	workDir          string
	project          ProjectContext
	copilotAuthToken string
	claudeAuthToken  string
	uidMap           *UIDMap
	appAuth          AppTokenMinter
	agentMint        AgentMintIssuer // optional, opt-in mint credential (nil ⇒ off)

	// bobAPIKeyResolver resolves the IBM bobshell API key at LAUNCH time (not
	// boot), so a key an operator adds via a Secret/PVC file or the config UI
	// takes effect without restarting the hive. Returns "" when unconfigured.
	//
	// Stored as an atomic.Pointer, NOT under m.mu, for the same reason as
	// isGatewayBackend above: it is read from launchInTmux/agentEnvPairs, which
	// already hold m.mu.Lock(). Re-locking a non-reentrant RWMutex on the same
	// goroutine would deadlock startup before MarkReady and crash-loop every
	// spoke. An atomic read is lock-free and safe from any lock context.
	bobAPIKeyResolver atomic.Pointer[func() string]

	// bobKeySourceResolver reports WHERE the key was found ("file:<path>" or
	// "env:<NAME>"), never the value. The launch path needs the PATH so it can
	// verify the file is readable by the AGENT UID — the hive process can read
	// it as dev even when the agent cannot, so key presence alone is a false
	// positive (see verifyBobKeyReadable). Same atomic.Pointer discipline and
	// same deadlock reasoning as bobAPIKeyResolver above.
	bobKeySourceResolver atomic.Pointer[func() string]

	// auditSink, when set, receives agent lifecycle events (start, stop,
	// launch failure, backend/model change) for durable, queryable recording
	// in the dashboard's audit store. Nil in tests / non-dashboard setups, in
	// which case every audit call is a no-op. See pkg/agent/audit.go for why
	// this is an injected interface rather than a direct pkg/dashboard import,
	// and why it is an atomic.Pointer rather than m.mu-guarded state.
	auditSink atomic.Pointer[AuditSink]

	inferenceRouteCallback      func(agentName, backend, model string)
	clearInferenceRouteCallback func(agentName string)

	// isGatewayBackend reports whether a backend string names a configured model
	// gateway (in addition to the built-in inference backends vllm/llm-d/litellm).
	// Injected from config so an agent whose backend is a gateway name (e.g.
	// "openrouter") is treated as inference-routable and its route resolved.
	// Nil in tests/bare setups → only built-in inference backends route.
	//
	// Stored as an atomic.Pointer, NOT under m.mu: routableBackend() is called
	// from the agent-launch path (launchInTmux via Start), which ALREADY holds
	// m.mu.Lock(). Reading this under m.mu.RLock() there would re-lock a
	// non-reentrant RWMutex on the same goroutine and DEADLOCK the whole startup
	// — the process never reaches MarkReady, /api/health stays "starting", and
	// the startup probe kills the pod (a cluster-wide crash-loop we hit live).
	// An atomic read is lock-free and safe from any lock context.
	isGatewayBackend atomic.Pointer[func(backend string) bool]

	// persistPauseCallback, when set, persists an agent's paused state to
	// the on-disk config so it survives restarts. Nil in tests / bare setups.
	//
	// Invocation contract: Pause/Resume snapshot this under m.mu but invoke
	// it only AFTER releasing m.mu. The callback does config disk I/O and is
	// allowed to re-enter the manager (AllStatuses, GetStatus, ...); m.mu is
	// a non-reentrant sync.RWMutex, so invoking it with the write lock held
	// would deadlock the pause path and wedge every operation queued behind
	// m.mu (heartbeat AllStatuses, SendKick, terminal ResolveAgent) — the
	// same failure class as the mint-issuer deadlock fixed in ca5f0f00.
	persistPauseCallback func(name string, paused bool)

	// breakerEngaged and breakerPaused hold the fleet-breaker's state, guarded
	// by m.mu. When an operator throws the breaker, EngageBreaker pauses every
	// running, non-on-demand agent and records the set of names it paused here.
	// Releasing resumes ONLY that set, and only for agents whose pause is still
	// attributable to the breaker (PausedTrigger == BreakerTrigger) — an agent
	// an operator re-paused during the breaker window keeps its manual pause.
	// Both fields persist into /data/hive-state.json (see cmd/hive persistState)
	// so an engaged breaker survives the frequent pod restarts: on boot the
	// agents restore paused from their own persisted pause, and RestoreBreaker
	// re-associates them with the breaker so a later release resumes them.
	breakerEngaged bool
	breakerPaused  map[string]bool

	// recordPromptCallback, when set, persists the fully-expanded prompt text
	// delivered to an agent so owners can review it later.
	//
	// Held as an atomic.Pointer rather than behind m.mu because the kick path
	// (deliverKickLocked) already holds m.mu when it fires this. m.mu is a
	// non-reentrant RWMutex, so reading the callback under a second Lock there
	// would deadlock the kick path.
	recordPromptCallback atomic.Pointer[func(agent, trigger, prompt string)]
	sandboxConfig        config.AgentSandboxConfig
	sandboxLauncher      sandbox.Launcher
	sandboxRunner        sandboxCommandRunner
	sandboxPushMinter    pushbroker.TokenMinter
	sandboxPRClient      PRCreator
	sandboxAuditCallback atomic.Pointer[func(agent, action, detail string)]

	visiblePaneCapture   func(agent *AgentProcess) string
	sendKeysForAgent     func(agent *AgentProcess, keys ...string)
	promptDismissSleep   func(time.Duration)
	promptDismissTimeout time.Duration
}

// SetPersistPauseCallback wires a function that persists an agent's paused
// state to config (called on pause/resume). Idempotent saves are the
// caller's responsibility.
func (m *Manager) SetPersistPauseCallback(fn func(name string, paused bool)) {
	m.mu.Lock()
	m.persistPauseCallback = fn
	m.mu.Unlock()
}

// SetRecordPromptCallback wires a function that persists a delivered kick
// prompt (agent, trigger, full text). Safe to call at any time; a nil fn
// clears it. Never takes m.mu — see recordPromptCallback.
func (m *Manager) SetRecordPromptCallback(fn func(agent, trigger, prompt string)) {
	if fn == nil {
		m.recordPromptCallback.Store(nil)
		return
	}
	m.recordPromptCallback.Store(&fn)
}

// SetSandboxConfig wires the disabled-by-default sandbox kick executor gate.
func (m *Manager) SetSandboxConfig(cfg config.AgentSandboxConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxConfig = cfg
}

func (m *Manager) SetSandboxLauncher(l sandbox.Launcher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxLauncher = l
}

func (m *Manager) setSandboxRunnerForTest(r sandboxCommandRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxRunner = r
}

func (m *Manager) SetSandboxPushMinter(minter pushbroker.TokenMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxPushMinter = minter
}

func (m *Manager) SetSandboxPRClient(client PRCreator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sandboxPRClient = client
}

func (m *Manager) SetSandboxAuditCallback(fn func(agent, action, detail string)) {
	if fn == nil {
		m.sandboxAuditCallback.Store(nil)
		return
	}
	m.sandboxAuditCallback.Store(&fn)
}

// recordPrompt fires the record-prompt callback if one is wired. Safe to call
// with m.mu held.
func (m *Manager) recordPrompt(agent, trigger, prompt string) {
	if fn := m.recordPromptCallback.Load(); fn != nil && *fn != nil {
		(*fn)(agent, trigger, prompt)
	}
}

// AppTokenMinter is implemented by github.AppAuth to mint per-agent scoped tokens.
type AppTokenMinter interface {
	WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error
}

// AgentMintIssuer is implemented by mint.AgentMinter. It is the OPT-IN,
// ADDITIONAL short-lived-credential path: when the mint is enabled it issues a
// scoped OIDC/JWT for an agent (subject=agent, scopes from its tier) that the
// agent can present ALONGSIDE its GitHub App token to a WIF broker. It never
// replaces WriteAgentToken. Enabled() reports false (and MintAgentToken returns
// "" with no error) when the mint is off, so wiring stays a strict no-op by
// default.
type AgentMintIssuer interface {
	Enabled() bool
	MintAgentToken(agentName, tier string) (string, error)
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
	case bobBackend:
		// bob has exactly one usable credential in a pod: the API key. Its
		// presence is therefore a complete answer, so report it as known.
		return m.bobAPIKey() != "", true
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

// SetGatewayBackendChecker injects a predicate that reports whether a backend
// string names a configured model gateway. This makes an agent whose backend is
// a gateway name inference-routable, so its route is resolved via the inference
// callback exactly like the built-in litellm/vllm/llm-d backends.
func (m *Manager) SetGatewayBackendChecker(fn func(backend string) bool) {
	// Atomic store — no m.mu — so routableBackend can read it lock-free from the
	// lock-holding launch path without deadlocking (see isGatewayBackend docs).
	m.isGatewayBackend.Store(&fn)
}

// SetBobAPIKeyResolver injects the resolver for the IBM bobshell API key.
// Called from main.go with a closure over the live config, so a key added
// after boot is picked up on the next agent launch. The resolver must return
// the key VALUE or "" — the value is never logged.
func (m *Manager) SetBobAPIKeyResolver(fn func() string) {
	// Atomic store — no m.mu — so bobAPIKey can be read lock-free from the
	// lock-holding launch path (see bobAPIKeyResolver docs).
	m.bobAPIKeyResolver.Store(&fn)
}

// bobAPIKey returns the configured bobshell API key, or "" when none is
// configured (or no resolver was injected, as in tests/bare setups).
// Safe to call while holding m.mu.
func (m *Manager) bobAPIKey() string {
	fnp := m.bobAPIKeyResolver.Load()
	if fnp == nil || *fnp == nil {
		return ""
	}
	return (*fnp)()
}

// SetBobKeySourceResolver injects the resolver reporting WHERE the bob key was
// found. The returned string is safe to log ("file:<path>" / "env:<NAME>") and
// never contains the key value.
func (m *Manager) SetBobKeySourceResolver(fn func() string) {
	m.bobKeySourceResolver.Store(&fn)
}

// bobKeyFilePath returns the FILE the bob key resolved from, or "" when it came
// from an env var (nothing to permission-check) or is unconfigured.
// Safe to call while holding m.mu.
func (m *Manager) bobKeyFilePath() string {
	fnp := m.bobKeySourceResolver.Load()
	if fnp == nil || *fnp == nil {
		return ""
	}
	source := (*fnp)()
	// Only "file:" sources have a path whose permissions can be checked; an
	// env-var key is inherited through tmux set-environment and needs none.
	const filePrefix = "file:"
	if !strings.HasPrefix(source, filePrefix) {
		return ""
	}
	return strings.TrimPrefix(source, filePrefix)
}

// routableBackend reports whether a backend should be routed through the
// inference proxy: either a built-in inference backend, or a configured gateway
// name. Safe to call while holding m.mu (isGatewayBackend is read atomically).
func (m *Manager) routableBackend(backend string) bool {
	if IsInferenceBackend(backend) {
		return true
	}
	// Lock-free atomic read: this is invoked from the launch path while m.mu is
	// already held, so it MUST NOT take m.mu (non-reentrant RWMutex → deadlock).
	fnp := m.isGatewayBackend.Load()
	return fnp != nil && *fnp != nil && (*fnp)(backend)
}

// validateBackendName reports whether backend is one the launcher can actually
// dispatch: an agentic CLI, a model-gateway backend, or a configured gateway
// name. An empty backend is valid (it means "the hive default").
//
// This is the manager-side half of the accept-then-fail fix. It dispatches on
// the SAME canonical lists as config.ValidateBackend and backendBinary, so a
// backend accepted by any write path is one the launch path can start.
// Safe to call while holding m.mu (routableBackend reads atomically).
func (m *Manager) validateBackendName(backend string) error {
	if backend == "" || config.IsCLIBackend(backend) || m.routableBackend(backend) {
		return nil
	}
	return fmt.Errorf("unsupported backend %q (supported: %s; or the name of a configured model gateway)",
		backend, strings.Join(config.SupportedBackends(), ", "))
}

// SetAppAuth injects the GitHub App auth provider for per-agent scoped tokens.
func (m *Manager) SetAppAuth(auth AppTokenMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appAuth = auth
}

// SetAgentMint injects the opt-in mint issuer for additional short-lived
// scoped OIDC tokens. Passing a disabled/nil issuer keeps the mint path a strict
// no-op — the GitHub App token path is unaffected either way.
func (m *Manager) SetAgentMint(issuer AgentMintIssuer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agentMint = issuer
}

// agentMintIssuerLocked reads the attached mint issuer WITHOUT taking m.mu, so
// it is safe from callers that already hold m.mu (e.g. Start holds
// m.mu.Lock()). m.agentMint is injected once via SetAgentMint at wiring time —
// before any agent Start — and never mutated afterward, so the lock-free read is
// race-free. A locked accessor would DEADLOCK on the Start path: Start →
// issueAgentMintToken → (locked read) re-locks a non-reentrant RWMutex on the
// same goroutine, wedging every Manager operation (SendKick, AllStatuses/
// heartbeat, ResolveAgent) behind the held write lock → heartbeat stall →
// liveness 503 → crash-loop.
func (m *Manager) agentMintIssuerLocked() AgentMintIssuer {
	return m.agentMint
}

const (
	// agentTokenCacheDir is the directory holding per-agent credential caches. It
	// mirrors the App-token cache dir (pkg/github) so both credentials live under
	// one agent-owned tree.
	agentTokenCacheDir = "/var/run/hive-metrics/agent-tokens"
	// agentTokenCachePerms restricts a per-agent credential file to owner-only.
	agentTokenCachePerms = 0o600
)

// AgentMintTokenCachePath returns the per-agent mint-token cache file path. It
// sits beside the GitHub App token cache but is a distinct file so the two
// credentials never collide — an agent reads the App token for GitHub and the
// mint token for WIF exchange.
func AgentMintTokenCachePath(agentName string) string {
	return agentTokenCacheDir + "/mint-token-" + agentName + ".cache"
}

// issueAgentMintToken mints an additional scoped OIDC token for the agent (when
// the mint is enabled) and writes it to a per-agent, agent-owned cache file. It
// is fail-safe and additive: a disabled mint, a mint error, or a write error is
// logged and swallowed — it NEVER blocks the GitHub App token path or the
// launch. tier is the same trust tier used for the App token, so scopes stay
// consistent across both credentials.
// issueAgentMintToken resolves the mint issuer WITHOUT holding m.mu itself, so
// it is safe to call from Start (which holds m.mu.Lock()). m.agentMint is set
// once at wiring time and never mutated after agents start, so the lock-free
// read is race-free. See agentMintIssuerLocked for the deadlock this avoids.
func (m *Manager) issueAgentMintToken(agentName, tier string, agentUID int) {
	issuer := m.agentMintIssuerLocked()
	if issuer == nil || !issuer.Enabled() {
		return
	}
	token, err := issuer.MintAgentToken(agentName, tier)
	if err != nil {
		m.logger.Warn("mint token issuance failed (App token unaffected)",
			"agent", agentName, "tier", tier, "error", err)
		return
	}
	if token == "" {
		return
	}
	if err := writeAgentCredFile(AgentMintTokenCachePath(agentName), token, agentUID); err != nil {
		m.logger.Warn("writing mint token cache failed (App token unaffected)",
			"agent", agentName, "error", err)
		return
	}
	m.logger.Info("per-agent mint token issued", "agent", agentName, "tier", tier, "uid", agentUID)
}

// writeAgentCredFile atomically writes a credential to path with 0600 perms,
// chowning to agentUID (>0) so only that agent can read it. It mirrors the
// App-token write path (temp file + chown + rename) so a partial write is never
// left in place.
func writeAgentCredFile(path, token string, agentUID int) error {
	if err := os.MkdirAll(agentTokenCacheDir, 0o755); err != nil {
		return fmt.Errorf("creating agent token dir: %w", err)
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(token), agentTokenCachePerms); err != nil {
		return fmt.Errorf("writing cred cache: %w", err)
	}
	if agentUID > 0 {
		if err := os.Chown(tmpPath, agentUID, -1); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf("chown cred cache: %w", err)
		}
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename cred cache: %w", err)
	}
	return nil
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
			continue
		}
		// Refresh the opt-in mint token alongside the App token (no-op when off).
		m.issueAgentMintToken(a.Name, tier, a.UID)
		// Re-inject the freshly-minted App token into the running session so the
		// GitHub MCP server keeps authenticating as the App bot. The scoped token
		// expires hourly; WriteAgentToken above rewrites the cache FILE (which the
		// git credential helper re-reads per call), but the Copilot CLI reads
		// GITHUB_TOKEN once from its env at startup — so without this push the MCP
		// token would go stale an hour after launch and GitHub writes would start
		// 401ing. tmux set-environment only affects processes started AFTER it,
		// which is fine: the Copilot CLI is (re)spawned per agent turn, so each new
		// turn picks up the current token. Gated on the opt-in flag + CanPush() to
		// match the launch injection — advisory agents stay GITHUB_TOKEN-less, and
		// hives that have not opted in get nothing injected.
		if m.project.AppAuthoredPRs && a.tmuxSession != "" && m.agentMode(a).CanPush() {
			if data, err := os.ReadFile(ghpkg.AgentTokenCachePath(a.Name)); err == nil {
				if tok := strings.TrimSpace(string(data)); tok != "" {
					_ = m.tmuxCmd(a, "set-environment", "-t", a.tmuxSession, "GITHUB_TOKEN", tok).Run()
				}
			}
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
			Name:   name,
			ID:     agentID,
			Config: cfg,
			State:  StateStopped,
			UID:    agentUID,
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

	if m.agentSandboxEnabledLocked(agent) {
		if agent.Paused {
			agent.State = StatePaused
			m.logger.Info("sandbox agent starting paused", "name", agent.Name, "trigger", agent.PausedTrigger, "persisted", agent.Config.Paused)
			return nil
		}
		now := time.Now()
		agent.State = StateIdle
		agent.StartedAt = &now
		agent.HasLaunched = true
		agent.LaunchedMode = m.agentMode(agent)
		m.logger.Info("audit: sandbox agent ready", "name", name)
		return nil
	}

	m.sanitizeGitRemotes(agent)

	if err := m.ensureTmuxSession(agent); err != nil {
		// No tmux session means no pane to announce into, so this failure
		// cannot ride announceLaunchFailureInPane like the park-and-return
		// branches do — record it here or it stays invisible.
		m.audit(AuditAgentStartFailed, name, auditFields(
			"outcome", "failure",
			"backend", agent.effectiveBackend(),
			"model", agent.effectiveModel(),
			"error", err.Error(),
			"stage", "tmux_session",
		))
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
			// Be precise about the blast radius. Since audit H3 the shared-cache
			// fallback is GONE: gh-wrapper.sh and git-credential-hive.sh no
			// longer fall back to /var/run/hive-metrics/gh-app-token.cache (the
			// FULL installation token, now owner-only 0600). They FAIL LOUD when
			// the per-agent scoped token is absent rather than silently
			// escalating this agent to full privilege. So a delivery failure here
			// means this agent's GitHub writes (gh + git push) will fail until
			// token delivery is repaired — not a silent privilege escalation.
			m.logger.Warn("per-agent scoped token NOT delivered — this agent's GitHub writes (gh + git push) will FAIL (no shared-cache fallback by design; see audit H3)",
				"agent", agent.Name, "tier", tier, "error", err)
		}
		// Additionally issue an opt-in short-lived mint token (no-op when the mint
		// is disabled). This is additive and fail-safe — it never blocks launch.
		m.issueAgentMintToken(agent.Name, tier, agent.UID)
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
	if defaultTmuxSocket != "" {
		return []string{"tmux", "-L", defaultTmuxSocket}
	}
	return []string{"tmux"}
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
		tmuxArgs := append(m.tmuxBaseArgs(agent), "new-session", "-d", "-s", agent.tmuxSession, "-c", agentDir)
		cmd = exec.Command(suExecArgs[0], append(suExecArgs[1:], tmuxArgs...)...)
	} else {
		base := m.tmuxBaseArgs(agent)
		tmuxArgs := append(base[1:], "new-session", "-d", "-s", agent.tmuxSession, "-c", agentDir)
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
	cmd := m.tmuxRawCmd("has-session", "-t", session)
	return cmd.Run() == nil
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
	// pi's marker. pi renders a TUI status bar showing model context usage
	// (e.g. "↑37k ↓20k R756k CH99.6% $0.013 5.9%/1.0M (auto)") instead of a
	// "❯"/"goose is ready" prompt, so none of the entries above match a
	// running pi: waitForCLIReadyForAgent would never see it as ready and the
	// startup kick would be dropped after cliReadyTimeout even though pi is
	// healthy. "%/" matches the fixed "%%/1.0M" context-meter suffix pi
	// renders at every context size.
	piContextMarker,
	// bob's markers. NONE of the entries above match a running bob: verified
	// against the installed bundle (bobshell 1.0.6 bundle/bob.js), which
	// contains zero "❯" characters and no "esc cancel" / "/ commands" /
	// "? help" / "goose" strings. ("Claude"/"Gemini" occur only as model-name
	// data, never as UI chrome.) Without these two entries
	// waitForCLIReadyForAgent can never see a booted bob, so its startup kick
	// would be dropped after cliReadyTimeout even though bob is healthy.
	bobInputPlaceholder,
	bobInputPlaceholderDefault,
	bobProductMarker,
	// codex's markers. Codex 0.144.1's TUI renders NONE of the entries above:
	// verified live (daviddiaz "Visual Hive", hive-oke) — an idle codex pane
	// contained no "❯", "goose", "Claude"/"Gemini" chrome, or bob strings, only
	// the "›" (U+203A) input caret and the "OpenAI Codex" banner. Without these
	// two entries waitForCLIReadyForAgent can never see a booted codex, so its
	// kick is dropped after cliReadyTimeout even though codex is healthy.
	codexInputPromptMarker,
	codexProductMarker,
}

const (
	// bobInputPlaceholder is the placeholder bob renders inside its input box
	// when it is idle and accepting input. This is bob's equivalent of the "❯"
	// prompt for the other TUIs and is the PRIMARY readiness signal: the
	// bundle renders it on the same component whose presence is gated by
	// `isInputActive`, so seeing it means the input is live, not merely
	// painted. Copied verbatim from bobshell 1.0.6 — see TestBobPaneMarkers.
	bobInputPlaceholder = "Type your message or @path/to/file"
	// bobInputPlaceholderDefault is the OTHER placeholder bob renders in that
	// same input box. The two are alternatives chosen by editor mode, not
	// versions: the bundle picks bobInputPlaceholder only when vim-style
	// modal editing is on, and this string in every other case — which is the
	// default, so it is what a stock bob actually shows when idle and ready.
	//
	// Verified live on bobshell 1.0.6: a healthy authenticated bob pane
	// contained this string and ZERO occurrences of bobInputPlaceholder, so
	// waitForCLIReadyForAgent never saw it as ready and every governor kick
	// was dropped with "CLI did not become ready after restart" while bob sat
	// perfectly healthy at its prompt. Both strings are present in the 1.0.6
	// bundle, so match either rather than replacing one with the other.
	bobInputPlaceholderDefault = "Enter your prompt, / for commands"
	// bobProductMarker is bob's product name, which appears in its banner and
	// dialogs. It is a weaker, secondary signal than bobInputPlaceholder — it
	// also shows on trust/auth/license dialogs, which are NOT ready states —
	// so it is used only for coarse CLI-presence detection (is anything other
	// than bash in this pane?), never as the input-ready gate.
	bobProductMarker = "Bob-Shell"
	// piContextMarker is pi's context-meter suffix ("5.9%/1.0M (auto)" in
	// its TUI status bar). It is the PRIMARY readiness signal for a running
	// pi: the status bar renders only when the agent TUI is live and has a
	// model configured, and it is the only marker pi renders in common with
	// no other CLI (pi never shows "❯"/"goose is ready"). Matching on "%/"
	// rather than a context size keeps it valid at any model/context
	// configuration.
	piContextMarker = "%/"
)

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

// backendDefersStartupKick reports whether a backend's bootstrap prompt is
// delivered AFTER the CLI is ready (deliverStartupKick) instead of being
// embedded in the launch command.
//
// Embedding raced the CLI boot: the prompt-bearing launch line was typed into
// the pane in the same second as the (re)start (observed live: `audit: agent
// kicked trigger=startup` 60ms after `audit: agent restarting`), before the
// CLI — or even bash — was ready to consume it, so the kick text landed in
// bash and an unbalanced quote left the shell in PS2 continuation.
//
// goose is deliberately NOT in this set: `goose run` needs the embedded --text
// prompt to stay interactive at all, and it exits on the ^C that
// readiness-gated delivery sends (see deliverKickLocked). Unknown backends are
// likewise excluded — they never embed and have no verified readiness signal.
//
// bob and pi belong in this set, not the goose set. Both are long-lived
// interactive TUIs that sit at their prompts with no
// prompt argument at all, so it has no goose-style reason to embed — and
// embedding would expose it to exactly the PS2 race above. Before this it was
// in NEITHER group: it fell through to the write-a-file branch in launchInTmux,
// so its bootstrap prompt was serialized to /tmp/.hive-bootstrap-<name>.txt and
// then never read, leaving the CLI idle at its prompt after every launch.
func backendDefersStartupKick(backend string) bool {
	switch backend {
	case "claude", "copilot", "gemini", "pi", bobBackend:
		return true
	default:
		return false
	}
}

// copilotGitHubWriteDenyFlags and claudeGitHubWriteDenyFlags are defined together
// near the bottom of this file (alongside the codex/bob backend constants). v2
// independently added a copy of copilotGitHubWriteDenyFlags here; the v4 grouped
// definition (which also carries claudeGitHubWriteDenyFlags) is kept as the single
// source of truth, so this duplicate was dropped in the v2→v4 sync merge.

func (m *Manager) launchInTmux(ctx context.Context, agent *AgentProcess) error {
	if ctx == nil {
		ctx = context.Background()
	}
	backend := agent.Config.Backend
	if agent.BackendOverride != "" {
		backend = agent.BackendOverride
	}

	binary, err := backendBinary(backend)
	if err != nil {
		agent.State = StateFailed
		agent.LastError = err.Error()
		m.logger.Warn("backend binary not found", "name", agent.Name, "backend", backend, "error", err)
		// The tmux session already exists (Start/Restart ran ensureTmuxSession
		// before this), so without a banner the pane is a silent bare shell —
		// the operator attaches and sees a prompt, not a failure. Say which
		// binary was attempted, that it is missing, and what to do.
		m.announceLaunchFailureInPane(agent, fmt.Sprintf(
			"backend %s did not launch: %v. The CLI for this backend is not installed in this hive image — upgrade the hive image or switch this agent to a different backend.",
			backend, err))
		return nil
	}

	// bob cannot authenticate without an API key in a pod: its default W3ID SSO
	// flow opens a browser and polls a localhost callback port, then fails with
	// "Authentication timeout (3 minutes)". Refuse to launch instead of burning
	// three minutes per attempt on a flow that cannot succeed here. Mirrors the
	// missing-binary handling above: mark the agent failed, log actionably, and
	// return nil so one misconfigured agent never aborts the fleet.
	// The key VALUE is not bound to a local here — only its presence is checked.
	// It is delivered to the CLI via tmux set-environment in agentEnvPairs.
	// Any launch attempt supersedes a previous missing-key parking: either the
	// key is present now (cleared below) or we re-park on this same branch.
	// Clearing unconditionally keeps the flag from outliving the condition it
	// describes — a stale true would make a later key-save relaunch an agent
	// that is actually failed for some other reason.
	agent.awaitingBobKey = false
	if backend == bobBackend {
		if m.bobAPIKey() == "" {
			agent.State = StateFailed
			agent.awaitingBobKey = true
			agent.LastError = "no bob API key configured (" + config.BobAPIKeyEnvVar + ")"
			m.logger.Warn("bob requires "+config.BobAPIKeyEnvVar+" for headless operation; ask your hub admin to configure it",
				"name", agent.Name,
				"backend", backend,
				"remedy", "set governor.bob.api_key_file (e.g. "+config.DefaultBobAPIKeyFile+
					") or the "+config.DefaultBobAPIKeyEnv+" env var on the hive pod",
			)
			// Same silent-bare-shell hazard as the missing-binary branch above:
			// the session exists, nothing was typed, and the log line is
			// invisible from the terminal. Name the missing credential (never
			// its value) and the remedy in the pane itself.
			m.announceLaunchFailureInPane(agent, fmt.Sprintf(
				"backend bob did not launch: no API key is configured (%s). Save an IBM bob API key in the dashboard under Governor -> Bob — parked bob agents relaunch automatically when a key is saved.",
				config.BobAPIKeyEnvVar))
			return nil
		}
		// Re-assert hive's ownership of the SHARED /data/home/.bob/settings.json
		// auth block BEFORE bob starts. A persisted selectedType beats
		// BOBSHELL_DEFAULT_AUTH_TYPE, so without this one agent that ever picked
		// SSO leaves every bob agent on the hive stuck at the auth prompt.
		// Pure filesystem work on locals only — takes no locks, so it is safe on
		// this m.mu-holding path.
		m.ensureBobAuthSettings(agent.Name, bobSharedHome)
		// The key resolved above was read by the HIVE process as dev. bob will
		// read it as the AGENT UID, which is a different question — and the one
		// that actually failed in production. Probe it and log actionably.
		// Advisory only: the Secret-mounted copy may still be readable, so this
		// never blocks a launch that might succeed.
		m.verifyBobKeyReadable(agent.Name, m.bobKeyFilePath(), agent.UID)
		// bob reports unwritable state dirs only inside its own TUI, so probe
		// them here as the agent UID and surface any failure in the hive log.
		// Advisory, like the key probe above — never blocks a launch.
		_ = m.verifyBobStateDirsWritable(agent.Name, bobSharedHome, m.workDir+"/"+agent.Name, agent.UID)
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
	isInference := m.routableBackend(backend)
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
			// Deny ALL GitHub MCP write tools in EVERY mode: agents author via the
			// App-gated gh wrapper, never as the user via the MCP. Mode governs the
			// gh-wrapper/proxy layer only, not what the MCP may write.
			launchCmd = base + claudeGitHubWriteDenyFlags
		case "copilot":
			// model is passed verbatim to `copilot --model %s`. It may be a
			// concrete id OR the auto-selection sentinel "auto" (copilotAutoModel
			// in cli_models.go), which lets the Copilot CLI pick/adjust the model
			// per task. Nothing here assumes a concrete id, so the sentinel flows
			// through unchanged.
			// PRIMARY defense against authoring as the login USER via the MCP:
			// we do NOT pass --enable-all-github-mcp-tools. Copilot CLI's built-in
			// GitHub MCP server is READ-ONLY BY DEFAULT (v0.0.350+), so the write
			// tools (create_issue/create_pull_request/…) are never registered.
			// READ tools (get_issue/list/search) stay available in that read-only
			// default, so nothing here disables useful lookups. All GitHub writes
			// must go through the App-gated gh wrapper / hive-open-pr.
			// copilotGitHubWriteDenyFlags is applied as belt-and-suspenders (with
			// the CORRECT `github-mcp-server(` server name) on top of the read-only
			// default. This is identical across ModeIssuesAndPRs / ModeIssuesOnly /
			// advisory — the mode never changes what the MCP can write (it never
			// legitimately should), it only governs the separate, unchanged
			// gh-wrapper/proxy layer that still reads Mode for the App-gated writes.
			launchCmd = fmt.Sprintf("%s --model %s --no-auto-update --allow-all%s",
				binary, model, copilotGitHubWriteDenyFlags)
		case "gemini":
			launchCmd = fmt.Sprintf("%s --model %s", binary, model)
		case "pi":
			// pi takes the model as a CLI flag, not a subcommand. Without
			// this case the launch command never receives the configured
			// model (previously it also hit the goose binary via the alias).
			launchCmd = fmt.Sprintf("%s --model %s", binary, model)
		case "goose":
			launchCmd = fmt.Sprintf("%s run -s", binary)
			if model != "" {
				launchCmd = fmt.Sprintf("%s --model %s", launchCmd, model)
			}
		case bobBackend:
			launchCmd = bobLaunchCmd(binary)
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

	// See backendDefersStartupKick for why each backend is or is not deferred.
	deferredStartupKick := ""
	if bootstrapPrompt != "" && backendDefersStartupKick(backend) {
		deferredStartupKick = bootstrapPrompt
		bootstrapPrompt = ""
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
		m.recordPrompt(agent.Name, "startup", bootstrapPrompt)

		// Only goose (and unknown backends, which never embed) reach this
		// block — claude/copilot/gemini/bob bootstrap prompts were deferred to
		// deliverStartupKick above, so no /tmp/.hive-bootstrap-<name>.txt is
		// written for them. bob used to land here and get a file nothing ever
		// read; that dead write is gone with the deferral above.
		promptFile := fmt.Sprintf("/tmp/.hive-bootstrap-%s.txt", agent.Name)
		// N15: owner-only + O_NOFOLLOW (see writeAgentStateFile).
		if err := writeAgentStateFile(promptFile, []byte(bootstrapPrompt)); err != nil {
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
		if err := writeAgentStateFile(minimalPrompt, []byte("You are an AI agent. Wait for instructions from the supervisor.")); err != nil {
			m.logger.Warn("failed to write minimal bootstrap prompt", "name", agent.Name, "error", err)
		}
		launchCmd += fmt.Sprintf(" --text \"$(cat %s)\"", minimalPrompt)
	}

	if !agent.forceRelaunch && m.tmuxPaneHasCLIForAgent(agent) {
		m.logger.Info("CLI already running in tmux pane, skipping launch", "name", agent.Name, "session", agent.tmuxSession)
		now := time.Now()
		agent.State = StateRunning
		agent.LastError = ""
		agent.lastLaunchFailureBanner = ""
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

	// Re-apply SECRET env vars before every launch. ensureTmuxSession sets the
	// full env via tmux set-environment, but it returns early when the session
	// already exists, so on a relaunch (restart, model change, crash recovery)
	// those values are never refreshed. Non-secret vars survive that because
	// buildEnvPrefix re-types them on the command line each launch, and
	// COPILOT_GITHUB_TOKEN survives because it is also in the hive process env
	// and inherited — but a key resolved from a Secret/PVC FILE is in neither,
	// so without this it would reach the CLI on a session's first launch only.
	// set-environment is idempotent and never appears in the pane.
	m.applySecretEnv(agent)

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
	// A prior aborted launch is history the moment a real one succeeds; a
	// stale "no API key" reason lingering after a key-save relaunch would
	// send an operator chasing a problem that no longer exists.
	agent.LastError = ""
	agent.lastLaunchFailureBanner = ""
	agent.StartedAt = &now
	agent.launchGen++
	m.logger.Info("audit: agent started",
		"name", agent.Name,
		"backend", backend,
		"model", model,
		"mode", mode.String(),
		"session", agent.tmuxSession,
	)
	m.audit(AuditAgentStarted, agent.Name, auditFields(
		"outcome", "success",
		"backend", backend,
		"model", model,
		"mode", mode.String(),
	))

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

	// Pinned: unpinned HEAD broke every install on 2026-07-27 when upstream
	// removed the --mode flag. Bump deliberately, after checking `--help`.
	const cavemanRef = "github:JuliusBrussee/caveman#0d95a81d35a9"
	// Upstream replaced `--mode full|minimal` with `--all` / `--minimal`
	// (--all = hooks + init).
	modeFlag := "--all"
	if mode == "minimal" {
		modeFlag = "--minimal"
	}

	var cmd *exec.Cmd
	switch backend {
	case "claude":
		cmd = exec.Command("npx", "-y", cavemanRef, "--", "--only", "claude", modeFlag)
	case "copilot":
		cmd = exec.Command("npx", "-y", cavemanRef, "--", "--only", "copilot", "--with-init", modeFlag)
	case "gemini":
		cmd = exec.Command("npx", "-y", cavemanRef, "--", "--only", "gemini", modeFlag)
	case "goose":
		cmd = exec.Command("npx", "-y", "skills", "add", "JuliusBrussee/caveman#0d95a81d35a9", "-a", "goose")
	case "codex":
		cmd = exec.Command("npx", "-y", "skills", "add", "JuliusBrussee/caveman#0d95a81d35a9", "-a", "codex")
	case "aider":
		cmd = exec.Command("npx", "-y", "skills", "add", "JuliusBrussee/caveman#0d95a81d35a9", "-a", "aider-desk")
	default:
		m.logger.Info("caveman not supported for backend", "backend", backend)
		return
	}

	cmd.Dir = filepath.Join("/data/agents", agent.Name)
	// The shared npm cache under /data/home accumulates content-addressed
	// entries owned by whichever agent UID wrote them first; npx run as the
	// hive user then fails with EACCES on those shards and the agent launches
	// without its proxy. A per-agent cache can never collide across UIDs.
	npmCache := filepath.Join("/data/agents", agent.Name, ".npm-caveman-cache")
	cmd.Env = append(os.Environ(), "HOME="+home, "npm_config_cache="+npmCache)
	if out, err := cmd.CombinedOutput(); err != nil {
		m.logger.Warn("caveman install failed", "agent", agent.Name, "error", err, "output", string(out))
	}
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
			// Advance the activity clock only when the pane actually changed.
			// Comparing against the PREVIOUS capture (not prevLines, which the
			// ring-buffer diff below consumes and rewrites) is what separates
			// "the CLI is producing output" from "the CLI is sitting at an idle
			// prompt": a static pane re-captured every 3s is byte-identical, so
			// an agent that renders nothing new never moves this timestamp.
			if !samePaneCapture(agent.lastPaneCapture, filtered) {
				agent.LastPaneChange = time.Now()
			}
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
			// effectiveBackend, not Config.Backend: an agent configured for
			// copilot but overridden to another CLI at runtime still reported
			// "copilot" here, so this copilot-only detector ran against a
			// different TUI's output. Observed in production on a bob agent
			// whose config said copilot — bob printed one of the generic
			// patterns below ("fetch failed"), the agent was restarted as if
			// its TLS had died, and the governor kick delivered seconds
			// earlier died with the session. It looped every ~60s, so no kick
			// ever survived long enough to run.
			if effectiveBackend(agent) == "copilot" && paneShowsFatalNetworkError(filtered) {
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
				_ = m.tmuxRawCmd("send-keys", "-t", session, "2").Run()
				time.Sleep(enterDelay)
				_ = m.tmuxRawCmd("send-keys", "-t", session, "Enter").Run()
				m.logger.Info("auto-answered folder trust prompt", "session", session)
				time.Sleep(trustCooldown)
			}
		}
	}
}

// acmmLevelNames maps ACMM level numbers to human-readable names. Kept in
// sync with the canonical pack definitions in v2/pkg/config/packs/level-*.yaml.
var acmmLevelNames = map[int]string{
	1: "Inception",
	2: "Advisory",
	3: "Quality-Gated",
	4: "Security-Aware",
	5: "Semi-Autonomous",
	6: "Fully Autonomous",
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

// metricsCachePath is a var (not const) so tests can point it at a temp file
// to exercise readCoveragePreamble without a real /data volume. Production
// value is unchanged.
var metricsCachePath = "/data/metrics/agent-metrics-cache.json"

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

// applySecretEnv pushes only the Secret pairs into the agent's tmux session via
// set-environment. Values are passed as exec args (never through a shell), so
// they are not word-split and never land in the pane or in bash history.
// Failures are ignored for the same reason ensureTmuxSession ignores them: a
// missing session is handled by the launch path, not here.
func (m *Manager) applySecretEnv(agent *AgentProcess) {
	if agent == nil || agent.tmuxSession == "" {
		return
	}
	for _, p := range m.agentEnvPairs(agent) {
		if !p.Secret {
			continue
		}
		_ = m.tmuxCmd(agent, "set-environment", "-t", agent.tmuxSession, p.Key, p.Value).Run()
	}
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
				m.checkBlockedThrash(name, l)
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

// Blocked-action thrash breaker: an agent that keeps hammering a policy wall
// (e.g. git push in ADVISORY mode, blocked every ~3s by git-credential-hive)
// burns model tokens indefinitely with zero possible output — observed live
// 2026-08-04 on a hosted L2 hive whose guide agent retried a blocked push
// every 3 seconds. The hub, not the model, breaks the loop: thrashThreshold
// blocked-action lines within thrashWindow pauses the session (visible,
// reversible, stops governor kicks) with the reason spelled out.
const (
	thrashWindow    = 60 * time.Second
	thrashThreshold = 5
	thrashCooldown  = 10 * time.Minute
)

// blockedActionMarkers are the policy-wall stderr lines that can never
// succeed by retrying. Keep in sync with bin/git-credential-hive.sh and the
// proxy's hard-deny responses.
var blockedActionMarkers = []string{
	"git push blocked:",
	"blocked by hive policy",
}

type thrashState struct {
	times    []time.Time
	lastTrip time.Time
}

// checkBlockedThrash records a blocked-action output line for the agent and,
// past the threshold, pauses the agent asynchronously (never inline: this is
// called from the output-capture goroutine and Pause takes m.mu).
func (m *Manager) checkBlockedThrash(agent, line string) {
	matched := false
	for _, marker := range blockedActionMarkers {
		if strings.Contains(line, marker) {
			matched = true
			break
		}
	}
	if !matched {
		return
	}
	now := time.Now()
	m.thrashMu.Lock()
	if m.thrash == nil {
		m.thrash = map[string]*thrashState{}
	}
	st := m.thrash[agent]
	if st == nil {
		st = &thrashState{}
		m.thrash[agent] = st
	}
	trip := recordBlockedAndCheck(st, now, thrashWindow, thrashThreshold, thrashCooldown)
	m.thrashMu.Unlock()
	if !trip {
		return
	}
	reason := fmt.Sprintf("blocked-action loop: %d+ policy-blocked attempts in %s — the block is terminal in this mode; paused to stop token burn", thrashThreshold, thrashWindow)
	m.logger.Warn("thrash breaker tripped", "agent", agent, "line", truncateStr(line, 160))
	go func() {
		if err := m.Pause(agent, "thrash-breaker", reason); err != nil {
			m.logger.Warn("thrash breaker pause failed", "agent", agent, "error", err)
		}
	}()
}

// recordBlockedAndCheck is the pure sliding-window decision: append now, drop
// entries older than window, and report whether the threshold is crossed
// outside the cooldown. Split out for direct unit testing.
func recordBlockedAndCheck(st *thrashState, now time.Time, window time.Duration, threshold int, cooldown time.Duration) bool {
	st.times = append(st.times, now)
	cutoff := now.Add(-window)
	kept := st.times[:0]
	for _, t := range st.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.times = kept
	if len(st.times) < threshold {
		return false
	}
	if !st.lastTrip.IsZero() && now.Sub(st.lastTrip) < cooldown {
		return false
	}
	st.lastTrip = now
	st.times = nil
	return true
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

// paneShowsInputPrompt reports whether the pane content shows a CLI input
// prompt that is ready to accept a kick.
//
// The first four markers are the pre-existing set, preserved verbatim so
// claude/copilot/gemini/goose readiness is bit-for-bit unchanged. The bob
// placeholder is additive: bob's TUI renders none of the other four (verified
// against bobshell 1.0.6 — the bundle contains no "❯" at all), so without it a
// healthy bob never registers as ready and its startup kick is dropped.
//
// The codex caret (U+203A "›") is likewise additive: Codex 0.144.1's TUI
// renders none of the markers above (verified live — its idle pane contains no
// "❯" at all), so without it a healthy codex pane never registers as ready and
// its kick is dropped with "did not reach input prompt".
//
// Callers pass captured pane text; empty input is not a prompt.
func paneShowsInputPrompt(output string) bool {
	if output == "" {
		return false
	}
	return strings.Contains(output, "❯") ||
		strings.Contains(output, "goose is ready") ||
		strings.Contains(output, "> Enter to send") ||
		strings.Contains(output, "\n>\n") ||
		strings.Contains(output, bobInputPlaceholder) ||
		strings.Contains(output, bobInputPlaceholderDefault) ||
		strings.Contains(output, codexInputPromptMarker) ||
		strings.Contains(output, piContextMarker)
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
			if paneShowsInputPrompt(output) {
				return true
			}
		}
	}
}

func (m *Manager) captureTmuxPane(session string) string {
	cmd := m.tmuxRawCmd("capture-pane", "-t", session, "-p",
		"-S", fmt.Sprintf("-%d", tmuxCaptureLines))
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func (m *Manager) tmuxRawCmd(args ...string) *exec.Cmd {
	base := m.tmuxBaseArgs(&AgentProcess{})
	tmuxArgs := append(base[1:], args...)
	return exec.Command(base[0], tmuxArgs...)
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
	m.audit(AuditAgentStopped, name, auditFields(
		"outcome", "success",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
	))

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
	m.audit(AuditAgentAdded, name, auditFields(
		"outcome", "success",
		"backend", cfg.Backend,
		"model", cfg.Model,
		"id", agentID,
	))
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

// ReconcileAgents makes the manager's name-keyed process table match the
// enabled config set. New agents are added, existing agents get fresh config,
// and removed agents have only their own hive-<name> tmux session retired.
func (m *Manager) ReconcileAgents(configs map[string]config.AgentConfig) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var added []string

	for name, cfg := range configs {
		if existing, ok := m.agents[name]; ok {
			delete(m.idToName, existing.ID)
			existing.Config = cfg
			if cfg.ID != "" {
				existing.ID = cfg.ID
			} else {
				existing.ID = name
			}
			m.idToName[existing.ID] = name
			continue
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
			Paused:       cfg.Paused,
			OutputBuffer: NewRingBuffer(outputBufferCapacity),
			tmuxSession:  "hive-" + name,
			tmuxSocket:   tmuxSocket,
		}
		m.idToName[agentID] = name
		added = append(added, name)
		m.logger.Info("audit: agent added by reconcile", "name", name, "id", agentID, "uid", agentUID)
	}

	for name, existing := range m.agents {
		if _, ok := configs[name]; ok {
			continue
		}
		if existing.cancel != nil {
			existing.cancel()
		}
		_ = m.tmuxCmd(existing, "kill-session", "-t", existing.tmuxSession).Run()
		delete(m.idToName, existing.ID)
		delete(m.agents, name)
		m.logger.Info("audit: agent removed by reconcile", "name", name, "id", existing.ID, "session", existing.tmuxSession)
	}
	return added
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
	m.audit(AuditAgentRemoved, name, auditFields(
		"outcome", "success",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
		"id", agent.ID,
	))
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

// notRunningReason explains WHY an agent is not in StateRunning, in terms an
// operator can act on.
//
// The old wording — "agent <name> not running" — came from this same
// State != StateRunning check and conflated three unrelated situations: an
// agent the operator deliberately paused, one that failed to launch, and one
// that was stopped or never started. During a live incident it read as a
// launch failure on agents that were paused exactly as intended, and sent the
// investigation after a tmux problem that did not exist. Naming the state, and
// the pause trigger where there is one, is the whole fix.
//
// Caller holds m.mu.
func notRunningReason(agent *AgentProcess) string {
	switch {
	case agent.Paused || agent.State == StatePaused:
		reason := "it is paused"
		if t := strings.TrimSpace(agent.PausedTrigger); t != "" {
			reason += " (by " + t + ")"
		}
		if r := strings.TrimSpace(agent.PausedReason); r != "" {
			reason += ": " + r
		}
		return reason
	case agent.State == StateFailed:
		reason := "it failed to start"
		if e := strings.TrimSpace(agent.LastError); e != "" {
			reason += ": " + e
		}
		return reason
	case agent.State == StateStopped:
		return "it is stopped"
	case agent.State == StateIdle:
		return "it has not been started yet"
	default:
		return "it is in state " + string(agent.State)
	}
}

func (m *Manager) SendKick(name string, message string) error {
	// Agent-kick span. No-op with zero export cost when tracing is disabled
	// (the default). SendKick has no context parameter, so this span roots at
	// Background; it still captures the kick leg of the governor→agent
	// lifecycle. Ended before delivery bookkeeping via defer.
	_, span := tracing.StartSpan(context.Background(), "agent.send_kick",
		attribute.String("agent.name", name))
	defer span.End()

	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	if m.agentSandboxEnabledLocked(agent) {
		return m.startSandboxKickLocked(agent, message)
	}

	if agent.State != StateRunning {
		return fmt.Errorf("agent %s cannot be kicked: %s", name, notRunningReason(agent))
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

	// Persist the FULL prompt text. The log line above and KickRecord.Snippet
	// only carry a truncated preview, which is not enough to answer "what was
	// my agent asked to do?".
	m.recordPrompt(agent.Name, trigger, message)
}

func (m *Manager) agentSandboxEnabledLocked(agent *AgentProcess) bool {
	return agent != nil && agent.Config.SandboxEnabled(m.sandboxConfig)
}

func (m *Manager) startSandboxKickLocked(agent *AgentProcess, message string) error {
	if agent.Paused || agent.State == StatePaused {
		return fmt.Errorf("agent %s cannot be kicked: %s", agent.Name, notRunningReason(agent))
	}
	if agent.State == StateStopped {
		return fmt.Errorf("agent %s cannot be kicked: %s", agent.Name, notRunningReason(agent))
	}
	if agent.State == StateRunning {
		return fmt.Errorf("agent %s cannot be kicked: sandbox execution already running", agent.Name)
	}
	if strings.TrimSpace(m.sandboxConfig.Image) == "" && strings.TrimSpace(agent.Config.SandboxImage(m.sandboxConfig)) == "" {
		return fmt.Errorf("agent %s sandbox image is not configured", agent.Name)
	}
	repo := m.project.PrimaryRepo()
	if repo == "" {
		return fmt.Errorf("agent %s sandbox execution requires a primary repo", agent.Name)
	}
	now := time.Now()
	agent.State = StateRunning
	agent.StartedAt = &now
	agent.LastKick = &now
	agent.LastKickMessage = message
	agent.KickRefused = false
	agent.KickRefusalReason = ""
	agent.LastError = ""
	agent.LaunchedMode = m.agentMode(agent)
	agent.HasLaunched = true
	snippet := truncateStr(message, 120)
	if len(agent.KickHistory) >= kickHistoryCapacity {
		agent.KickHistory = agent.KickHistory[1:]
	}
	agent.KickHistory = append(agent.KickHistory, KickRecord{Timestamp: now, Agent: agent.Name, Snippet: snippet})
	if agent.OutputBuffer != nil {
		agent.OutputBuffer.Write("sandbox kick started")
	}
	m.recordPrompt(agent.Name, "sandbox-kick", message)
	m.logger.Info("audit: sandbox agent kicked", "name", agent.Name, "repo", repo)

	spec := SandboxKickSpec{
		Agent: agent.Name,
		AgentConfig: configSnapshot{
			Backend:   effectiveBackend(agent),
			Model:     agent.Config.Model,
			LaunchCmd: agent.Config.LaunchCmd,
		},
		Message:      message,
		Org:          m.project.Org,
		Repo:         repo,
		WorkspaceDir: m.sandboxWorkspaceDirLocked(),
		Image:        agent.Config.SandboxImage(m.sandboxConfig),
		EnvAllowlist: agent.Config.SandboxEnvAllowlist(m.sandboxConfig),
		NetworkMode:  agent.Config.SandboxNetworkMode(m.sandboxConfig),
		Timeout:      time.Duration(agent.Config.SandboxTimeoutS(m.sandboxConfig)) * time.Second,
	}
	runCtx, cancel := context.WithCancel(context.Background())
	agent.cancel = cancel
	launcher, runner := m.sandboxLauncher, m.sandboxRunner
	cloneMinter := m.tieredSandboxMinterLocked(m.agentMode(agent).TokenTier())
	var pushMinter pushbroker.TokenMinter
	var prClient PRCreator
	pushEnabled := false
	if m.agentMode(agent).CanPush() && m.project.PRsAllowed {
		pushMinter = cloneMinter
		prClient = m.sandboxPRClient
		pushEnabled = true
	}
	go m.runSandboxKick(runCtx, agent.Name, spec, launcher, runner, cloneMinter, pushMinter, pushEnabled, prClient)
	return nil
}

func (m *Manager) sandboxWorkspaceDirLocked() string {
	if strings.TrimSpace(m.sandboxConfig.WorkspaceDir) != "" {
		return m.sandboxConfig.WorkspaceDir
	}
	return filepath.Join(m.workDir, "sandbox")
}

func (m *Manager) tieredSandboxMinterLocked(tier string) pushbroker.TokenMinter {
	switch minter := m.sandboxPushMinter.(type) {
	case pushbroker.GitHubAppMinter:
		minter.Tier = tier
		return minter
	case *pushbroker.GitHubAppMinter:
		if minter == nil {
			return nil
		}
		cp := *minter
		cp.Tier = tier
		return cp
	default:
		return m.sandboxPushMinter
	}
}

func (m *Manager) runSandboxKick(ctx context.Context, name string, spec SandboxKickSpec, launcher sandbox.Launcher, runner sandboxCommandRunner, cloneMinter, minter pushbroker.TokenMinter, pushEnabled bool, prClient PRCreator) {
	exec := &SandboxExecutor{
		Launcher:    launcher,
		Runner:      runner,
		CloneMinter: cloneMinter,
		Minter:      minter,
		PushEnabled: pushEnabled,
		PRClient:    prClient,
		Logger:      m.logger,
	}
	res, err := exec.Run(ctx, spec)
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[name]
	if !ok {
		return
	}
	agent.cancel = nil
	if err != nil {
		if agent.sandboxResumeAfterCancel {
			agent.sandboxResumeAfterCancel = false
			agent.State = StateIdle
			agent.LastError = ""
			if agent.OutputBuffer != nil {
				agent.OutputBuffer.Write("sandbox kick cancelled during resume")
			}
			m.auditSandbox(name, "sandbox_cancelled", "sandbox kick cancelled while resuming")
			return
		}
		if agent.Paused || agent.State == StatePaused {
			agent.State = StatePaused
		} else if agent.State != StateStopped {
			agent.State = StateFailed
		}
		agent.LastError = err.Error()
		if agent.OutputBuffer != nil {
			agent.OutputBuffer.Write("sandbox kick failed: " + err.Error())
		}
		m.auditSandbox(name, "sandbox_failed", err.Error())
		if res.Broker != nil && res.Broker.Error != "" {
			m.auditSandbox(name, "sandbox_broker_rejected", res.Broker.Error)
		}
		return
	}
	agent.sandboxResumeAfterCancel = false
	if agent.Paused || agent.State == StatePaused {
		agent.State = StatePaused
	} else if agent.State != StateStopped {
		agent.State = StateIdle
	}
	agent.LastError = ""
	if agent.OutputBuffer != nil {
		msg := fmt.Sprintf("sandbox kick complete: commits=%d", res.CommitCount)
		if res.PR != nil && res.PR.URL != "" {
			msg += " pr=" + res.PR.URL
		}
		agent.OutputBuffer.Write(msg)
	}
	detail := fmt.Sprintf("workspace=%s commits=%d branch=%s", res.Workspace, res.CommitCount, res.Branch)
	if res.PR != nil && res.PR.URL != "" {
		detail += " pr=" + res.PR.URL
	}
	m.auditSandbox(name, "sandbox_complete", detail)
}

func (m *Manager) auditSandbox(agent, action, detail string) {
	if fn := m.sandboxAuditCallback.Load(); fn != nil && *fn != nil {
		(*fn)(agent, action, detail)
	}
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

	// bob needs an extra settle window that the other TUIs do not. Its UI is a
	// React/Ink app (its crashes surface as React reconciler stack traces), and
	// Ink paints the input box on an early render pass — so the placeholder
	// that waitForInputPromptForAgent matches can be visible before the
	// reconciler has finished mounting the input component and attached its
	// stdin handler. Text typed in that window is painted into the pane but
	// never reaches component state, so the kick is silently swallowed and bob
	// stays idle — the exact failure this change exists to fix. claude/copilot/
	// gemini do not need this: their input handlers are live as soon as the
	// prompt renders, which is why the delay is bob-only rather than a new
	// universal pause in the shared path.
	//
	// Read outside m.mu: effectiveBackend is a pure field read and this path
	// must not hold the lock while sleeping.
	if effectiveBackend(agent) == bobBackend {
		time.Sleep(bobInputHandlerSettleDelay)
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

// launchFailurePrefix opens every in-pane launch-failure banner so the line is
// unmistakable in a pane full of shell prompts and greppable in scrollback.
const launchFailurePrefix = "HIVE LAUNCH FAILED: "

// launchFailureBanner composes the exact shell line typed into a pane when a
// launch is aborted before any CLI starts. The message is wrapped in single
// quotes; any single quotes, newlines, or carriage returns inside it are
// replaced with spaces so the line can never break out of the quoting or
// leave bash in PS2 continuation (the same hazard the C-c before every real
// launch guards against).
func launchFailureBanner(msg string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch r {
		case '\'', '\n', '\r':
			return ' '
		}
		return r
	}, msg)
	return "echo '" + launchFailurePrefix + sanitized + "'"
}

// announceLaunchFailureInPane makes an aborted launch visible IN the agent's
// tmux pane. launchInTmux's park-and-return branches (missing backend binary,
// bob with no API key) run after ensureTmuxSession has already created a
// fresh shell, so returning silently leaves a bare prompt: the operator
// attaches via ttyd, sees nothing wrong, and the only explanation is in the
// hive log they are not reading. Typing an echo into the pane puts the reason
// and the remedy exactly where the operator is looking.
//
// Best-effort by design: the send-keys errors are ignored like every other
// pane write on the launch path — a missing session must not turn a parked
// agent into a crashed manager. Caller holds m.mu (same discipline as
// launchInTmux, which is its only caller).
// It is also the single chokepoint every park-and-return branch already
// passes through, so recording the durable audit event here — rather than
// once per branch — means no launch failure can be added later that silently
// skips the audit log. This is the watsonx case: an agent configured with a
// backend the image does not support failed at every launch for a day, WARN-
// logged inside the pod and invisible in the Audit Log UI.
func (m *Manager) announceLaunchFailureInPane(agent *AgentProcess, msg string) {
	agent.lastLaunchFailureBanner = launchFailureBanner(msg)
	m.tmuxSendLiteralForAgent(agent, agent.lastLaunchFailureBanner)
	m.tmuxSendKeysForAgent(agent, "Enter")

	m.audit(AuditAgentStartFailed, agent.Name, auditFields(
		"outcome", "failure",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
		"error", agent.LastError,
	))
}

// effectiveBackend is the backend this agent will actually launch with: the
// per-agent override when set, otherwise its configured backend.
func (a *AgentProcess) effectiveBackend() string {
	if a.BackendOverride != "" {
		return a.BackendOverride
	}
	return a.Config.Backend
}

// effectiveModel is the model this agent will actually launch with: the
// per-agent override when set, otherwise its configured model. Returns the
// raw (un-normalized) name — the audit log should show what was ASKED for,
// since a bad model name is exactly the kind of misconfiguration being
// audited.
func (a *AgentProcess) effectiveModel() string {
	if a.ModelOverride != "" {
		return a.ModelOverride
	}
	return a.Config.Model
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
	timeout := promptDismissTimeout
	if m.promptDismissTimeout > 0 {
		timeout = m.promptDismissTimeout
	}
	deadline := start.Add(timeout)
	lastPane := ""

	for time.Now().Before(deadline) {
		interval := promptPollInterval
		if time.Since(start) < promptFastPollWindow {
			interval = promptFastPollInterval
		}
		m.sleepDuringPromptDismiss(interval)

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
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
		} else if strings.Contains(selectedLower, "fix with") {
			// Settings error: skip past "Fix with Claude" and "Exit" to "Continue without"
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
			m.tmuxSendKeysForAgent(agent, "Down")
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
		}

		m.tmuxSendKeysForAgent(agent, "Enter")
	}

	m.logger.Warn("inference prompt dismissal timed out", "agent", agent.Name)
}

func (m *Manager) sleepDuringPromptDismiss(d time.Duration) {
	if m.promptDismissSleep != nil {
		m.promptDismissSleep(d)
		return
	}
	time.Sleep(d)
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
			m.sleepDuringPromptDismiss(postKeystrokeDelay)
			return true
		}
		m.tmuxSendKeysForAgent(agent, navKey)
		m.sleepDuringPromptDismiss(postKeystrokeDelay)
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
	if m.sendKeysForAgent != nil {
		m.sendKeysForAgent(agent, keys...)
		return
	}
	args := append([]string{"send-keys", "-t", agent.tmuxSession}, keys...)
	_ = m.tmuxCmd(agent, args...).Run()
}

const (
	clearBeforeKickDelay    = 2 * time.Second
	enterCount              = 3
	enterDelay              = 300 * time.Millisecond
	textToEnterDelay        = 1 * time.Second
	chunkSize               = 400
	chunkDelay              = 1 * time.Second
	staleCheckDelay         = 1 * time.Second
	cliReadyPollInterval    = 2 * time.Second
	cliReadyTimeout         = 60 * time.Second
	inputPromptPollInterval = 2 * time.Second
	inputPromptTimeout      = 120 * time.Second
	// preLaunchShellClearDelay gives bash time to process the C-c that
	// clears stale PS2 quote-continuation state before the launch command
	// is typed into the pane.
	preLaunchShellClearDelay = 500 * time.Millisecond
	// bobInputHandlerSettleDelay is an extra pause applied ONLY to the bob
	// backend after its input prompt becomes visible, before its startup kick
	// is typed. bob's TUI is React/Ink: Ink paints the input box on an early
	// render pass, so the placeholder can be on screen before the reconciler
	// finishes mounting the input component and attaching its stdin handler.
	// Typing in that gap paints characters that never reach component state —
	// the kick is swallowed and bob sits idle. It is deliberately NOT reused
	// from textToEnterDelay/staleCheckDelay (1s each): those cover tmux
	// keystroke pacing, a different concern, and a value that merely happens
	// to work is what this constant exists to avoid. 3s is a conservative
	// multiple of the observed 1s pacing unit, chosen because over-waiting
	// costs one agent a few seconds once per launch while under-waiting
	// silently loses the bootstrap prompt entirely.
	bobInputHandlerSettleDelay = 3 * time.Second
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
	// NeedsLogin and LastPaneChange are written by the pane poller under paneMu.
	needsLogin := a.NeedsLogin
	lastPaneChange := a.LastPaneChange
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
		LastPaneChange:  lastPaneChange,
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

// backendBinaryAliases names the backends whose binary is NOT simply the
// backend name. Only genuine aliases belong here: every other CLI backend is
// derived from config.CLIBackends by identity, and every model-gateway backend
// from config.InferenceBackends. Keeping this map to aliases only is what makes
// the accept-then-fail class of bug structurally impossible — see backendBinary.
var backendBinaryAliases = map[string]string{
	// pi was previously aliased to "goose", which made every pi-configured
	// agent exec the goose CLI instead of pi (the backend launch command
	// switch now has a real pi case). pi is a first-class CLI backend
	// (config.CLIBackends includes "pi"), so identity mapping applies.
}

// backendBinaryName maps an agent backend to the NAME of the CLI binary that is
// exec'd for it, without touching the filesystem. Split out from backendBinary
// so the "every supported backend resolves" invariant can be tested without
// requiring each CLI to be installed on the test machine.
//
// Both canonical lists are derived rather than written out here:
//
//   - config.CLIBackends (claude, copilot, goose, codex, pi, bob, aider, gemini)
//     each launch a binary of the same name, except for the aliases above.
//   - config.InferenceBackends (vllm, llm-d, litellm, watsonx) all launch the
//     SAME claude CLI, pointed at hive's local OpenAI-compatible translator via
//     ANTHROPIC_BASE_URL — the backend name selects the upstream route, not the
//     binary.
//
// Deriving both means a backend added to either list can never again be
// accepted by config.ValidateBackend and then rejected hours later at kick time
// with "unknown backend". Previously only InferenceBackends was derived, so
// codex and aider were valid config values that failed at launch.
func backendBinaryName(backend string) (string, error) {
	binaries := make(map[string]string, len(config.CLIBackends)+len(config.InferenceBackends))
	for _, b := range config.CLIBackends {
		binaries[b] = b
	}
	for _, b := range config.InferenceBackends {
		binaries[b] = "claude"
	}
	for backend, binary := range backendBinaryAliases {
		binaries[backend] = binary
	}

	binary, ok := binaries[backend]
	if !ok {
		return "", fmt.Errorf("unknown backend: %s", backend)
	}
	return binary, nil
}

// backendBinary resolves an agent backend to the absolute path of the CLI
// binary that is actually exec'd for it.
func backendBinary(backend string) (string, error) {
	binary, err := backendBinaryName(backend)
	if err != nil {
		return "", err
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("backend %s not found in PATH: %w", backend, err)
	}

	return path, nil
}

// sharedCopilotConfigPath and sharedClaudeCredentialPath are vars (not consts)
// solely so tests can redirect them to temp files and exercise the config/token
// helpers (copilotConfigHasTokens, clearExpiredTokens, configHasTokens,
// fixSharedConfigPerms) without a real /data volume. Production values are
// unchanged; nothing on the launch path mutates them.
var (
	sharedCopilotConfigPath    = "/data/home/.copilot/config.json"
	sharedClaudeCredentialPath = "/data/home/.claude/.credentials.json"
)

const (
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
	// A BARE "/login" is deliberately NOT here — it is handled separately by
	// lineHasLoginDirective below. "/login" alone is a substring of ordinary
	// agent output (an agent reviewing an auth route writes "POST /login"; a
	// CLI printing its slash-command list renders "/login" beside "/help"), and
	// matching it painted the 🔑 badge on agents that were authenticated and
	// mid-work.
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
	return copilotCredentialFileHasTokens(sharedCopilotConfigPath)
}

// copilotCredentialFileHasTokens is copilotConfigHasTokens for an ARBITRARY
// path, so the per-agent auth probe can read the same file shapes under an
// agent's own per-UID home instead of only the shared legacy location.
//
// Two shapes are accepted because the Copilot CLI uses both:
//   - .copilot/config.json — token map under the "copilotTokens" key.
//   - .config/github-copilot/{apps,hosts}.json — a flat map keyed by host,
//     each entry carrying an oauth_token. Any non-empty top-level map counts.
func copilotCredentialFileHasTokens(path string) bool {
	data, err := os.ReadFile(path)
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
	if tokens, ok := cfg["copilotTokens"]; ok {
		tokensMap, ok := tokens.(map[string]interface{})
		if !ok {
			return false
		}
		return len(tokensMap) > 0
	}
	// apps.json / hosts.json shape: host -> {oauth_token: ...}
	if strings.HasSuffix(path, "apps.json") || strings.HasSuffix(path, "hosts.json") {
		for _, v := range cfg {
			entry, ok := v.(map[string]interface{})
			if !ok {
				continue
			}
			if tok, ok := entry["oauth_token"].(string); ok && tok != "" {
				return true
			}
		}
	}
	return false
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
// loginDirectiveVerbs are the imperative words a CLI uses when it is TELLING
// the operator to authenticate ("Please /login to continue", "Run /login",
// "Type /login to sign in"). A line containing "/login" counts as a login
// prompt only when one of these also appears, which is what separates a real
// login screen from an agent discussing an auth route ("POST /login returns
// 302") or a CLI listing its slash commands ("/help  /login  /model").
var loginDirectiveVerbs = []string{
	"please", "run", "type", "use", "enter", "try", "must", "need",
}

// lineHasLoginDirective reports whether a line both mentions "/login" AND
// carries an imperative that makes it a directive to the operator.
func lineHasLoginDirective(line string) bool {
	if !strings.Contains(line, "/login") {
		return false
	}
	lower := strings.ToLower(line)
	for _, verb := range loginDirectiveVerbs {
		if strings.Contains(lower, verb) {
			return true
		}
	}
	return false
}

func paneShowsLoginPrompt(lines []string) bool {
	for _, line := range lines {
		if lineHasLoginDirective(line) {
			return true
		}
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
	// Only sweep by UID when isolation gave this agent a real per-agent UID.
	// agent.UID==0 (isolation off or agent missing from the UID map) would
	// otherwise ask killAgentProcesses to match root — the internal floor guard
	// blocks it, but skipping the call makes the intent explicit.
	if agent.UID > 0 {
		killAgentProcesses(agent.UID, m.logger)
	}
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
			m.audit(AuditAgentStartFailed, agent.Name, auditFields(
				"outcome", "failure",
				"backend", agent.effectiveBackend(),
				"model", agent.effectiveModel(),
				"error", agent.LastError,
			))
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

			if agent.UID > 0 {
				killAgentProcesses(agent.UID, m.logger)
			}
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
	claudeInferenceHomePrefix   = "/tmp/.claude-inference-home-"
)

// inferenceHomePrefixOverride redirects the per-agent inference HOME prefix.
// TEST SEAM ONLY — empty in production, where inferenceHomePath always returns
// claudeInferenceHomePrefix+name. It exists so the auth probe's per-UID home
// resolution can be exercised against a temp dir instead of /tmp.
var inferenceHomePrefixOverride string

// inferenceHomePath returns the per-agent inference HOME directory.
func inferenceHomePath(agentName string) string {
	if inferenceHomePrefixOverride != "" {
		return inferenceHomePrefixOverride + agentName
	}
	return claudeInferenceHomePrefix + agentName
}

// codexBackend is the backend name for the OpenAI Codex CLI.
const codexBackend = "codex"

// codexInputPromptMarker is the caret Codex renders on its input line when it
// is idle and awaiting input. It is Codex's equivalent of claude/gemini's "❯"
// and bob's placeholder — the PRIMARY readiness signal for a codex agent.
//
// It is a SINGLE-ANGLE-QUOTATION-MARK (U+203A "›"), deliberately distinct from
// the "❯" (U+276F) used by the other TUIs and by the consent-screen menu, so it
// never collides with paneShowsConsentScreen's "❯"-selected-line check.
//
// Verified live on Codex 0.144.1 (daviddiaz "Visual Hive", hive-oke): an idle
// scanner pane sitting at its prompt rendered this caret with placeholder
// ghost-text ("› Improve documentation in @filename", "› Explain this
// codebase") and contained ZERO "❯", "goose is ready", "> Enter to send", or
// bob placeholders — so without this marker a healthy codex pane never
// registers as ready and every kick is dropped with "did not reach input
// prompt", leaving the advisory issue stale.
//
// Matching the caret alone (not any specific placeholder string) is robust to
// the ghost-text varying between Codex tips/versions, and stays tight: Codex
// shows this idle input caret ONLY while awaiting input — a running turn
// renders streaming output and a working indicator instead, not the "›" caret.
const codexInputPromptMarker = "›"

// codexProductMarker is Codex's product name, rendered in its splash banner
// ("OpenAI Codex (v0.144.1)"). Like bobProductMarker it is a coarse
// CLI-presence signal only (it also shows on the splash before the input caret
// is live), never the input-ready gate — that is codexInputPromptMarker.
const codexProductMarker = "OpenAI Codex"

// bobBackend is the backend name for the IBM bobshell ("bob") CLI.
const bobBackend = "bob"

// copilotGitHubWriteDenyFlags denies EVERY GitHub MCP write tool for the copilot
// CLI. Agents must never author issues/PRs via the GitHub MCP — those calls run
// with the logged-in user's OAuth token, bypass the proxy, and skip the App gate.
// All GitHub writes route through the App-gated `gh` wrapper / hive-open-pr, which
// authors as kubestellar-hive[bot].
//
// PRIMARY defense is dropping --enable-all-github-mcp-tools from the launch
// command: Copilot CLI's built-in GitHub MCP server is READ-ONLY BY DEFAULT
// (v0.0.350+), so the write tools are never registered in the first place. These
// deny flags are belt-and-suspenders on top of that.
//
// The built-in server is named `github-mcp-server` (per `copilot --help`:
// `--disable-builtin-mcps ... (currently: github-mcp-server)`), NOT `github`.
// The prior deny names used the bare `github` server prefix, which was a SILENT
// NO-OP: `github` is only the name of a separately-added server, so a deny on it
// matched nothing and the writes stayed live. These use the CORRECT server name so the
// belt-and-suspenders denies actually match. Read tools (get_issue/list/search)
// stay available (read-only default), so no enable-all flag is needed.
const copilotGitHubWriteDenyFlags = " --deny-tool='github-mcp-server(create_pull_request)'" +
	" --deny-tool='github-mcp-server(create_pull_request_with_copilot)'" +
	" --deny-tool='github-mcp-server(merge_pull_request)'" +
	" --deny-tool='github-mcp-server(create_issue)'" +
	" --deny-tool='github-mcp-server(update_issue)'" +
	" --deny-tool='github-mcp-server(add_issue_comment)'"

// claudeGitHubWriteDenyFlags denies EVERY GitHub MCP write tool for the claude
// CLI — the same logical set as copilotGitHubWriteDenyFlags, in claude's
// --disallowed-tools 'mcp__github__<tool>' syntax. Same rationale: agents author
// as the App via the gh wrapper, never as the user via the MCP. Read tools stay
// enabled. Applied in EVERY agent mode.
const claudeGitHubWriteDenyFlags = " --disallowed-tools 'mcp__github__create_pull_request'" +
	" --disallowed-tools 'mcp__github__create_pull_request_with_copilot'" +
	" --disallowed-tools 'mcp__github__merge_pull_request'" +
	" --disallowed-tools 'mcp__github__create_issue'" +
	" --disallowed-tools 'mcp__github__update_issue'" +
	" --disallowed-tools 'mcp__github__add_issue_comment'"

// bobLaunchCmd builds bob's interactive launch command.
//
// The launch stays INTERACTIVE — no -p/--prompt — so the agent drives bob in a
// tmux pane exactly like every other CLI backend and a human can attach to it.
//
// Two flags are passed:
//
//   - --auth-method api-key: this flag DOES exist in bobshell 1.0.6. It was
//     previously removed after `bob --help | grep auth-method` returned
//     nothing, but that test is misleading: the bundle registers the option and
//     then explicitly HIDES it from help output —
//     `t.option("auth-method",{choices:[fr.W3ID_SSO,fr.USE_BOBSHELL]})` followed
//     by `["debug",...,"auth-method"].forEach(c=>t.hide(c))`. Verified by
//     running the real 1.0.6 bundle: `--help` is 67 lines with 0 matches for
//     auth-method, yet the parser distinguishes it from a typo —
//     $ bob --definitely-not-a-flag x -p hi
//     Unknown arguments: definitely-not-a-flag, definitelyNotAFlag
//     $ bob --auth-method bogus-value -p hi
//     Invalid values: Argument: auth-method, Given: "bogus-value",
//     Choices: "sso", "api-key"
//     while `--auth-method api-key` is accepted silently. An unknown flag under
//     yargs .strict() would have errored, so the option is live.
//
//     It matters because it is the ONLY input that outranks the persisted,
//     fleet-shared settings file: bob stores it as globalThis.authMethodByCliArg
//     and resolves
//     `authMethodByCliArg || merged.security.auth.selectedType || <default>`,
//     and it also suppresses the setValue() write-back that would otherwise
//     persist a competing value. BOBSHELL_DEFAULT_AUTH_TYPE
//     (config.BobAuthTypeEnvVar) is only the FALLBACK default and loses to a
//     persisted selectedType. Neither is the primary bug — an unreadable key
//     file was (see verifyBobKeyReadable) — but both are real, and this flag is
//     the cheapest guarantee that a stale settings file cannot re-break bob.
//     Note IBM's public docs also document `--auth-method api-key`.
//
//   - --accept-license: bob hard-errors ("A license agreement is required.
//     Please accept the license terms before proceeding.") before doing any
//     work unless licenseConsent is already persisted in its settings. That
//     consent is normally collected from a human at an interactive prompt that
//     nobody can answer in an unattended pod, and it is stored under
//     $HOME/.bob, so it is lost whenever the PVC is reset. Passing it on every
//     launch is idempotent (it only sets licenseConsent=true) and is the
//     vendor-documented non-interactive path. An operator configuring an API
//     key for unattended use is the act of acceptance; the text stays
//     reviewable via `bob --show-license`.
//
//   - --approval-mode yolo (config.BobApprovalModeFlag/BobApprovalModeYolo):
//     without it bob runs in its "default" approval mode, the TUI reports
//     `Auto-approve: Off`, and the agent blocks forever on its FIRST tool call
//     waiting for a human who is not attached. Verified live on a spoke: with
//     the flag the TUI reports `Auto-approve: Full` and bob executed a shell
//     tool unattended.
//
//     This is deliberately FLAT — not gated on m.agentMode(agent) — because it
//     matches the existing fleet posture rather than inventing a new one for
//     bob. Every other backend already auto-approves tools at EVERY ACMM level:
//     claude gets --dangerously-skip-permissions and copilot gets --allow-all
//     in all three mode branches of launchInTmux. Hive does not restrain agents
//     by making them ask permission for local tool calls; it restrains them by
//     (a) denying specific GitHub write tools per mode and (b) unsetting
//     GH_TOKEN/GITHUB_TOKEN for agents whose mode fails CanPush(). Both of
//     those controls apply to bob unchanged and are where an advisory bob is
//     actually contained. Giving bob a per-mode approval policy would make it
//     the only backend that stalls at low ACMM levels — less capable than its
//     peers, and stalled rather than safely limited.
//
//   - --trust (config.BobTrustFlag): bob otherwise treats the agent workdir as
//     untrusted ("This folder is not trusted. Some features may be disabled.")
//     and gates tool availability on it. See BobTrustFlag for why the flag is
//     preferred over seeding the shared $HOME/.bob/trustedFolders.json.
//
// No --model is passed, and that is load-bearing. bob auto-selects its own
// model, and hive's normalizeModelName rewrites a trailing -<digits> to
// .<digits> for every backend except claude/inference, so a configured
// `claude-sonnet-4-6` reached bob as `claude-sonnet-4.6` — an id bob's backend
// does not know. Its model config came back undefined and every prompt died
// with "🛑 Cannot read properties of undefined (reading 'maxTokens')". Verified
// live: the same bob with no --model runs inference successfully.
//
// The API key itself is NOT a flag — it is delivered out-of-band via
// tmux set-environment (see agentEnvPairs) so it never lands in the command
// line, `ps` output, or pane scrollback.
//
// The model parameter is intentionally absent from the signature so no future
// caller can reintroduce the crash by passing one.
func bobLaunchCmd(binary string) string {
	return fmt.Sprintf("%s --accept-license %s %s %s %s %s",
		binary,
		config.BobAuthMethodFlag, config.BobAuthTypeAPIKey,
		config.BobApprovalModeFlag, config.BobApprovalModeYolo,
		config.BobTrustFlag)
}

// codexHomePrefix is the per-agent CODEX_HOME directory prefix. Each agent gets
// its own dir so Codex's owner-gated app-server sees a directory the agent UID
// actually owns (a shared, merely group-writable dir is not sufficient for
// Codex, unlike claude/copilot). Lives on the persistent /data volume.
var codexHomePrefix = "/data/home/.codex-"

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
var codexSharedAuthFile = "/data/home/.codex/auth.json"

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
	agentUser := m.agentExecUserSpec(agent)
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
	data, err := readInferenceConfigFile(path)
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
	if err := writeInferenceConfigFile(path, out); err != nil {
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
	if data, err := readInferenceConfigFile(path); err == nil {
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
	if err := writeInferenceConfigFile(path, data); err != nil {
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

	if err := os.MkdirAll(settingsDir, inferenceHomeDirMode); err != nil {
		m.logger.Warn("failed to create claude inference home", "agent", agentName, "error", err)
		return
	}
	// SECURITY (audit F12): prefer an agent-OWNED 0700 home over a world-writable
	// one. The audit's fix is "UID-owned 0700 homes"; the surrounding code
	// assumed that was impossible ("hive runs as dev, not root, so chown is not
	// available"), but the hosted container runs as root and chown succeeds
	// there — verified live. Where it does succeed, no other UID can enter the
	// directory and the symlink cannot be planted in the first place.
	//
	// Falls back to the historical world-writable mode when chown is refused
	// (a genuinely unprivileged deployment), because a home the agent's own UID
	// cannot write is a hard launch failure. The O_NOFOLLOW guards on the
	// writers hold in BOTH cases — this narrows who can reach the path, it is
	// not what closes the finding.
	m.tightenInferenceHome(agentName, homePath, settingsDir, uid)
	// Write (or repair) both settings files: the HOME userSettings file and
	// the standalone file passed via --settings. The CLI honors
	// skipDangerousModePermissionPrompt from either source.
	m.seedClaudeSettingsFile(agentName, settingsFile)
	m.seedClaudeSettingsFile(agentName, claudeInferenceSettingsPath)
	// Pre-populate (or repair) .claude.json so the CLI skips first-run setup.
	m.seedClaudeUserConfig(agentName, filepath.Join(homePath, ".claude.json"))
	// Only widen when the home is NOT agent-owned. tightenInferenceHome may have
	// just established a 0700 home owned by the agent UID; running the widening
	// walk unconditionally would chmod it straight back to 0777 and silently
	// undo the hardening (audit F12).
	if !m.inferenceHomeIsOwnedBy(homePath, uid) {
		m.ensureWorldWritable(homePath)
	}
}

// ensureWorldWritable walks the tree and sets dirs to 0o777, files to 0o666.
//
// SECURITY (audit F12, CWE-59/CWE-61): this walk must never follow a symlink.
//
// The tree it repairs is an agent's own HOME and is world-writable by design,
// so the agent can plant entries in it. os.Chmod FOLLOWS symlinks, so a link
// planted here pointed the hive's own chmod at any file the hive user can
// reach — hive.yaml, key files, state — and turned it 0666. The agent supplies
// the link; the hive supplies the privilege. Verified: a 0600 file outside the
// tree became 0666 through a planted link, and stays 0600 with this guard.
//
// filepath.WalkDir reports entries via Lstat, so the symlink is visible as a
// symlink here; the danger is only in acting on it. Non-regular files (fifos,
// sockets, devices) are skipped for the same reason: chmod on them is never
// something this repair loop intends.
func (m *Manager) ensureWorldWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if d.IsDir() {
			if info.Mode().Perm() != 0o777 {
				_ = os.Chmod(path, 0o777)
			}
		} else if info.Mode().IsRegular() {
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
//
// bob is likewise excluded. bobLaunchCmd passes no --model at all (bob
// auto-selects), so this is defense-in-depth rather than the fix: the value is
// still computed and logged on the bob launch path, and the dot-rewrite is
// what turned a configured `claude-sonnet-4-6` into the unknown
// `claude-sonnet-4.6` that made bob die with "Cannot read properties of
// undefined (reading 'maxTokens')". Leaving it unrewritten keeps logs honest
// about what was configured and stops the corrupted id from being handed to a
// future bob consumer.
func normalizeModelName(model, backend string) string {
	if backend == "claude" || backend == bobBackend || IsInferenceBackend(backend) {
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

// agentStateFileMode is owner-only. These files sit in the pod-shared /tmp and
// carry per-agent CONTROL state — the enforcement mode the gh wrapper reads, and
// the bootstrap prompt goose is launched with — so anything group- or
// world-writable lets one agent steer another.
const agentStateFileMode = 0o600

// writeAgentStateFile writes per-agent control state to a shared-/tmp path
// without following symlinks.
//
// SECURITY (audit N15, CWE-367/732/20). These files were written with a plain
// os.WriteFile at 0o644, which:
//
//   - follows symlinks — os.WriteFile opens O_WRONLY|O_CREATE|O_TRUNC with no
//     O_NOFOLLOW, so a pre-planted symlink at the (predictable) path redirects
//     the hive's own write to any file the process can reach; and
//   - left the result world-readable, and the containing /tmp world-writable,
//     so any agent UID could replace another agent's file afterwards.
//
// Both matter because the path is derived from the agent NAME, which is
// guessable, and because the readers treat these files as authoritative:
// bin/gh-wrapper.sh takes its enforcement mode from .hive-mode-<name> in
// preference to the trustworthy env var, and goose is launched with whatever
// .hive-bootstrap-<name>.txt contains as its first instruction.
//
// O_NOFOLLOW makes a planted symlink fail loudly instead of redirecting the
// write. The file is deliberately NOT O_EXCL: these are rewritten on every
// mode change and every relaunch, so failing when the file already exists would
// break the normal path.
// inferenceHomeDirMode / inferenceHomeSharedDirMode are the two shapes a
// per-agent inference HOME can take (audit F12).
//
// The 0700 form is preferred and is what tightenInferenceHome establishes once
// the directory is chowned to the agent UID: nobody else can even enter it, so
// the predictable path stops being reachable. The 0777 form is the historical
// fallback for a deployment where chown is refused — the agent must be able to
// write its own HOME, and a home it cannot write is a hard launch failure.
const (
	inferenceHomeDirMode       = 0o700
	inferenceHomeSharedDirMode = 0o777
)

// tightenInferenceHome tries to give the agent UID sole ownership of its
// inference HOME, falling back to the historical world-writable mode when the
// hive lacks the privilege to chown. Best-effort by design: every failure path
// leaves a WORKING home, because the O_NOFOLLOW writers — not this — are what
// close audit F12.
func (m *Manager) tightenInferenceHome(agentName, homePath, settingsDir string, uid int) {
	if uid <= 0 {
		// No per-agent UID: the hive itself is the only writer, so 0700 owned
		// by the hive is already correct.
		return
	}
	for _, dir := range []string{homePath, settingsDir} {
		if err := os.Chown(dir, uid, -1); err != nil {
			// Unprivileged deployment. Restore the shared mode so the agent can
			// still use its HOME, and say so once at debug level rather than
			// warning on every launch.
			m.logger.Debug("inference home left world-writable (chown unavailable)",
				"agent", agentName, "dir", dir, "error", err)
			if cerr := os.Chmod(dir, inferenceHomeSharedDirMode); cerr != nil {
				m.logger.Warn("failed to restore inference home mode",
					"agent", agentName, "dir", dir, "error", cerr)
			}
			continue
		}
		if err := os.Chmod(dir, inferenceHomeDirMode); err != nil {
			m.logger.Warn("failed to tighten inference home mode",
				"agent", agentName, "dir", dir, "error", err)
		}
	}
}

// inferenceHomeIsOwnedBy reports whether homePath is already owned by uid, i.e.
// tightenInferenceHome succeeded and the widening walk must be skipped.
//
// Fails SAFE for availability rather than for security: on any doubt (uid<=0,
// stat error, or a platform without Stat_t) it returns false, so the caller
// widens and the agent keeps a usable HOME. The O_NOFOLLOW writers are what
// close F12 in that case.
func (m *Manager) inferenceHomeIsOwnedBy(homePath string, uid int) bool {
	if uid <= 0 {
		return false
	}
	info, err := os.Stat(homePath)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(st.Uid) == uid
}

// inferenceConfigFileMode is the mode for a per-agent inference config file.
//
// NOT 0600 like agentStateFileMode: these live in a per-agent HOME that the
// AGENT's own UID must be able to rewrite (the CLI updates its own config), and
// the hive cannot chown them to that UID on every deployment shape. 0666 in a
// directory only that agent can enter is the trade-off the surrounding code
// already makes; the symlink guard below is what actually closes audit F12,
// because the exploit was redirection out of the directory, not the mode.
const inferenceConfigFileMode = 0o666

// readInferenceConfigFile reads a per-agent inference config, refusing to
// traverse a symlink (audit F12).
//
// os.ReadFile follows symlinks, so a planted link let an attacker feed the
// CONTENTS of any hive-readable file into the merge logic below, which then
// wrote the merged result back out — a read-and-echo primitive on top of the
// clobber. O_NOFOLLOW makes that fail loudly instead.
//
// A missing file is the normal first-run case; callers already treat any error
// as "start from the seed", so failing closed here costs nothing.
func readInferenceConfigFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// writeInferenceConfigFile writes a per-agent inference config without
// following a symlink (audit F12, CWE-59/61).
//
// SECURITY: the inference HOME is a predictable path
// (/tmp/.claude-inference-home-<agent>) inside world-writable /tmp, and the
// directory itself is created world-writable so the agent UID can use it. Both
// writers here used a plain os.WriteFile, which opens O_WRONLY|O_CREATE|O_TRUNC
// with NO O_NOFOLLOW — so another local UID could pre-plant a symlink at the
// config path and have the hive overwrite any file it can reach.
//
// Verified before and after: a planted link at <home>/.claude.json had the
// VICTIM file's contents replaced with the seeded JSON; with this guard the
// write fails with ELOOP and the victim file is untouched.
//
// Note #3432 fixed only the sibling half of F12 — the chmod walk in
// ensureWorldWritable, which WIDENED a linked file's mode. This closes the
// write path, which CLOBBERED its contents. The audit named three sinks; all
// three are now covered.
func writeInferenceConfigFile(path string, data []byte) error {
	f, err := os.OpenFile(path,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW,
		inferenceConfigFileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func writeAgentStateFile(path string, data []byte) error {
	f, err := os.OpenFile(path,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW,
		agentStateFileMode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// O_CREATE honours the mode only when the file did not already exist, so an
	// existing 0644 file from a previous release keeps its old mode without
	// this. Chmod on the path is acceptable here because O_NOFOLLOW above has
	// already established it is not a symlink.
	return os.Chmod(path, agentStateFileMode)
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
		if err := writeAgentStateFile(modeFile, []byte(mode.String())); err != nil {
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

// InvocationMetadata reports the effective backend and model the hive invokes
// for the named agent, accounting for runtime overrides — the launch-time
// truth the invocation-attribution trail records (see pkg/github/attribution
// .go). ok=false when the agent is unknown to the manager (the caller then
// falls back to static config). Read-only under RLock; called from the
// PR-request watcher goroutine, never from the launch path.
func (m *Manager) InvocationMetadata(agentName string) (backend, model string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agent, exists := m.agents[agentName]
	if !exists {
		return "", "", false
	}
	backend = effectiveBackend(agent)
	model = agent.Config.Model
	if agent.ModelOverride != "" {
		model = agent.ModelOverride
	}
	return backend, model, true
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
	if err := writeAgentStateFile(modeFile, []byte(mode.String())); err != nil {
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
	// Point the GitHub MCP server at the App installation token so PRs, issue
	// comments, and merges are authored by the App bot ("<slug>[bot]") — NOT by
	// the Copilot login user. COPILOT_GITHUB_TOKEN above stays as the Copilot
	// OAuth token because it authenticates the AI model (a separate concern from
	// GitHub write identity); leaving it untouched keeps the Copilot CLI login
	// working. The Copilot CLI reads GH_TOKEN / GITHUB_TOKEN for GitHub API auth
	// (per its README: GH_TOKEN or GITHUB_TOKEN, in that precedence), so setting
	// GITHUB_TOKEN here makes the built-in GitHub MCP server act as the App bot.
	//
	// Gated on the opt-in flag first (default OFF → no behavior change on any
	// hive that has not explicitly enabled App-bot authorship), then on CanPush():
	// advisory agents are deliberately kept GITHUB_TOKEN-less (see the -u
	// GITHUB_TOKEN strip after the env loop) so they cannot write; only push-
	// capable tiers — the ones that legitimately open/merge PRs — get the App
	// token. m.appAuth != nil means an App is configured. The value is the
	// per-agent tier-SCOPED App token, and refreshAgentTokens re-pushes it hourly
	// so it never goes stale.
	if m.project.AppAuthoredPRs && m.appAuth != nil && agent.UID > 0 && m.agentMode(agent).CanPush() {
		if data, err := os.ReadFile(ghpkg.AgentTokenCachePath(agent.Name)); err == nil {
			if tok := strings.TrimSpace(string(data)); tok != "" {
				vars = append(vars, agentEnvPair{"GITHUB_TOKEN", tok, true})
			}
		}
	}
	if m.claudeAuthToken != "" && backend == "claude" {
		vars = append(vars, agentEnvPair{"CLAUDE_CODE_OAUTH_TOKEN", m.claudeAuthToken, true})
	}
	// bob reads its key from BOBSHELL_API_KEY. Secret: true keeps the value off
	// the shell command line (out of `ps`, bash history, and pane scrollback);
	// it reaches the CLI via tmux set-environment only. Gated on the backend so
	// no other CLI's environment carries an IBM credential it has no use for.
	if backend == bobBackend {
		if key := m.bobAPIKey(); key != "" {
			vars = append(vars, agentEnvPair{config.BobAPIKeyEnvVar, key, true})
		}
		// BOBSHELL_DEFAULT_AUTH_TYPE is what actually selects API-key auth;
		// without it bob defaults to W3ID SSO and parks at the interactive key
		// prompt forever. Deliberately NOT Secret: the value is the literal
		// non-credential string "api-key", and secret pairs only reach a
		// freshly-created pane shell via tmux set-environment, whereas
		// non-secret pairs are re-applied on EVERY launch through
		// buildEnvPrefix. That asymmetry is exactly what caused the sibling
		// bug fixed in #2228, so the auth type must ride the always-reapplied
		// path or a relaunch into an existing session loses it.
		vars = append(vars, agentEnvPair{config.BobAuthTypeEnvVar, config.BobAuthTypeAPIKey, false})
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

		// Under the per-agent-UID layout the global npm prefix is owned by the
		// image's build user, so the Claude Code CLI's self-updater fails on
		// every launch with "✘ Auto-update failed: no write permission to npm
		// prefix" — a red line in every agent pane for an update the agent must
		// not perform anyway (the CLI version is managed by the image, not by
		// an in-pod npm write). Disabling the updater removes the failure at its
		// source; a per-agent npm prefix would instead let an agent drift off
		// the pinned image version.
		vars = append(vars, agentEnvPair{"DISABLE_AUTOUPDATER", "1", false})
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

	agent, ok := m.agents[name]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("agent %s not found", name)
	}

	agent.Paused = true
	agent.PausedAt = time.Now()
	agent.PausedReason = reason
	agent.PausedTrigger = trigger
	if agent.State == StateRunning {
		if agent.cancel != nil {
			agent.cancel()
		}
		agent.sandboxResumeAfterCancel = false
		if !m.agentSandboxEnabledLocked(agent) {
			m.tmuxSendKeysForAgent(agent, "C-c", "")
		}
	}
	agent.State = StatePaused
	agent.Config.Paused = true
	// Snapshot the persistence callback under m.mu but invoke it only after
	// the unlock below: it does config disk I/O and may re-enter the manager,
	// and m.mu is a non-reentrant RWMutex — calling it here would deadlock
	// (see the persistPauseCallback field docs).
	persistPause := m.persistPauseCallback
	m.logger.Info("audit: agent paused",
		"name", name,
		"trigger", trigger,
		"reason", reason,
		"backend", agent.Config.Backend,
		"restart_count", agent.RestartCount,
	)
	m.audit(AuditAgentPaused, name, auditFields(
		"outcome", "success",
		"backend", agent.effectiveBackend(),
		"model", agent.effectiveModel(),
		"trigger", trigger,
		"reason", reason,
	))
	m.mu.Unlock()

	if persistPause != nil {
		persistPause(name, true)
	}
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
	// Snapshot backend/model while m.mu is held so audit details stay stable
	// even when Resume relaunches the agent before returning.
	resumeBackend := agent.effectiveBackend()
	resumeModel := agent.effectiveModel()
	agent.Paused = false
	agent.Config.Paused = false
	if persistPause := m.persistPauseCallback; persistPause != nil {
		// Deferred so it runs after this function's explicit m.mu.Unlock on
		// every return path: the callback does config disk I/O and may
		// re-enter the manager, and m.mu is a non-reentrant RWMutex —
		// invoking it here with the lock held deadlocks Resume and wedges
		// everything queued behind m.mu (see persistPauseCallback docs).
		defer persistPause(name, false)
	}
	agent.PausedAt = time.Time{}
	agent.PausedReason = ""
	agent.PausedTrigger = ""
	needsRelaunch := agent.State == StatePaused
	if m.agentSandboxEnabledLocked(agent) {
		if needsRelaunch {
			if agent.cancel != nil {
				agent.sandboxResumeAfterCancel = true
			} else {
				agent.State = StateIdle
				if agent.StartedAt == nil {
					now := time.Now()
					agent.StartedAt = &now
				}
			}
		}
		m.mu.Unlock()
		m.logger.Info("audit: sandbox agent resumed",
			"name", name,
			"trigger", trigger,
			"reason", reason,
			"prev_trigger", prevTrigger,
			"prev_reason", prevReason,
		)
		return nil
	}
	if needsRelaunch {
		agent.forceRelaunch = true
	}

	m.logger.Info("audit: agent resumed",
		"name", name,
		"trigger", trigger,
		"reason", reason,
		"prev_trigger", prevTrigger,
		"prev_reason", prevReason,
	)
	m.audit(AuditAgentResumed, name, auditFields(
		"outcome", "success",
		"backend", resumeBackend,
		"model", resumeModel,
		"trigger", trigger,
		"reason", reason,
	))
	if needsRelaunch {
		if err := m.ensureTmuxSession(agent); err != nil {
			m.mu.Unlock()
			return err
		}
		err := m.launchInTmux(ctx, agent)
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	return nil
}

// EngageBreaker throws the fleet-wide kill-switch: it pauses every agent that
// is currently RUNNING and not OnDemand, records exactly that set, and returns
// the names it paused. Agents that are on-demand (e.g. brainstorm) or already
// paused (a prior operator/manual pause) are skipped entirely and never enter
// the recorded set, so releasing the breaker later cannot un-pause them.
//
// Idempotent: if the breaker is already engaged, it re-pauses nothing and
// returns the existing recorded set unchanged.
//
// Pausing is done by calling Pause with BreakerTrigger. Pause takes m.mu
// itself, so this method collects the target names under m.mu, releases it,
// then pauses each — mirroring the lock discipline the dashboard uses when it
// pauses agents one at a time.
func (m *Manager) EngageBreaker() (paused []string) {
	m.mu.Lock()
	if m.breakerEngaged {
		existing := make([]string, 0, len(m.breakerPaused))
		for name := range m.breakerPaused {
			existing = append(existing, name)
		}
		m.mu.Unlock()
		sort.Strings(existing)
		return existing
	}

	targets := make([]string, 0, len(m.agents))
	for name, agent := range m.agents {
		if agent == nil {
			continue
		}
		// Skip on-demand agents (they are meant to sit idle until summoned) and
		// any agent that is already paused — a pause the operator owns, which
		// the breaker must not adopt and must not later reverse.
		if agent.Config.OnDemand {
			continue
		}
		if agent.Paused || agent.State != StateRunning {
			continue
		}
		targets = append(targets, name)
	}

	set := make(map[string]bool, len(targets))
	for _, name := range targets {
		set[name] = true
	}
	m.breakerEngaged = true
	m.breakerPaused = set
	m.mu.Unlock()

	sort.Strings(targets)
	for _, name := range targets {
		if err := m.Pause(name, BreakerTrigger, "fleet breaker engaged"); err != nil {
			m.logger.Warn("fleet breaker: pause failed", "agent", name, "error", err)
		}
	}
	m.logger.Info("fleet breaker engaged", "paused", len(targets))
	return targets
}

// ReleaseBreaker disengages the fleet-wide kill-switch. It resumes ONLY the
// agents the breaker itself paused (the recorded set) and, within that set,
// only those whose pause is STILL attributable to the breaker: current
// PausedTrigger == BreakerTrigger and still paused. An agent an operator
// re-paused during the breaker window has a different trigger and is left
// paused; an on-demand agent could never enter the set in the first place.
// Returns the names it resumed.
func (m *Manager) ReleaseBreaker(ctx context.Context) (resumed []string) {
	m.mu.Lock()
	if !m.breakerEngaged {
		m.mu.Unlock()
		return nil
	}

	candidates := make([]string, 0, len(m.breakerPaused))
	for name := range m.breakerPaused {
		agent, ok := m.agents[name]
		if !ok || agent == nil {
			continue
		}
		// Only resume agents still paused BY the breaker. If the operator
		// re-paused (trigger changed) or resumed-then-repaused during the
		// window, leave the agent exactly as the operator left it.
		if agent.Paused && agent.PausedTrigger == BreakerTrigger {
			candidates = append(candidates, name)
		}
	}
	m.breakerEngaged = false
	m.breakerPaused = nil
	m.mu.Unlock()

	sort.Strings(candidates)
	for _, name := range candidates {
		if err := m.Resume(ctx, name, BreakerTrigger, "fleet breaker released"); err != nil {
			m.logger.Warn("fleet breaker: resume failed", "agent", name, "error", err)
			continue
		}
		resumed = append(resumed, name)
	}
	m.logger.Info("fleet breaker released", "resumed", len(resumed))
	return resumed
}

// BreakerState reports whether the fleet breaker is engaged and the sorted set
// of agent names it currently holds paused.
func (m *Manager) BreakerState() (engaged bool, paused []string) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	engaged = m.breakerEngaged
	paused = make([]string, 0, len(m.breakerPaused))
	for name := range m.breakerPaused {
		paused = append(paused, name)
	}
	sort.Strings(paused)
	return engaged, paused
}

// RestoreBreaker re-establishes the breaker's in-memory state from persisted
// snapshot data on boot. The agents themselves are restored paused via their
// own persisted pause (with PausedTrigger == BreakerTrigger), so this only has
// to re-mark the breaker engaged and re-record the set. It does NOT re-pause or
// resume anything — a boot restore must never change agent state, only reattach
// the breaker so a later ReleaseBreaker knows which agents to resume. Only
// names still present AND still paused-by-the-breaker are re-adopted.
func (m *Manager) RestoreBreaker(engaged bool, paused []string) {
	if !engaged {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	set := make(map[string]bool, len(paused))
	for _, name := range paused {
		agent, ok := m.agents[name]
		if !ok || agent == nil {
			continue
		}
		if agent.Paused && agent.PausedTrigger == BreakerTrigger {
			set[name] = true
		}
	}
	m.breakerEngaged = true
	m.breakerPaused = set
	m.logger.Info("fleet breaker restored", "engaged", true, "held", len(set))
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

	prevModel := agent.effectiveModel()
	agent.PinnedModel = model
	agent.ModelOverride = model
	m.logger.Info("agent model pinned", "name", name, "model", model)
	if prevModel != model {
		m.audit(AuditAgentModelSet, name, auditFields(
			"outcome", "success",
			"backend", agent.effectiveBackend(),
			"model", model,
			"previous_model", prevModel,
			"trigger", "pin",
		))
	}
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

	prevModel := agent.effectiveModel()
	agent.ModelOverride = model
	m.logger.Info("agent model override set", "name", name, "model", model)
	// State CHANGES only — the governor re-asserts the current model on every
	// evaluation cycle, so auditing unchanged writes would flood the ring.
	if prevModel != model {
		m.audit(AuditAgentModelSet, name, auditFields(
			"outcome", "success",
			"backend", agent.effectiveBackend(),
			"model", model,
			"previous_model", prevModel,
		))
	}

	effectiveBackend := agent.Config.Backend
	if agent.BackendOverride != "" {
		effectiveBackend = agent.BackendOverride
	}
	if m.routableBackend(effectiveBackend) && m.inferenceRouteCallback != nil {
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

	// Refuse a backend the launcher cannot dispatch, at SET time. Previously
	// any string was accepted here and the agent was then restarted into it,
	// failing only at launch with "unknown backend: <x>" — the agent stops
	// working and the operator gets no signal at the moment of the change.
	// routableBackend covers configured gateway names (resolved live), so a
	// gateway-named backend still passes.
	if err := m.validateBackendName(backend); err != nil {
		return err
	}

	// Captured after validation so a rejected switch records no audit event:
	// the override is only mutated below, once the backend is known routable.
	prevBackend := agent.effectiveBackend()
	agent.BackendOverride = backend
	m.logger.Info("agent backend override set", "name", name, "backend", backend)
	// Record only a real transition: /switch/{backend} is also re-applied on
	// config reload with the value already in effect, and auditing those
	// no-ops would bury the actual operator changes.
	if prevBackend != backend {
		m.audit(AuditAgentBackendSet, name, auditFields(
			"outcome", "success",
			"backend", backend,
			"model", agent.effectiveModel(),
			"previous_backend", prevBackend,
		))
	}

	if m.routableBackend(backend) && m.inferenceRouteCallback != nil {
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
	if m.inferenceRouteCallback == nil || !m.routableBackend(backend) {
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

// SessionMissing reports whether an agent the manager believes is RUNNING has
// no live tmux session — the zombie case, where in-memory state and reality
// have diverged.
//
// It is deliberately false for any agent that is not StateRunning: a paused,
// stopped or never-started agent legitimately has no session, and reporting
// those as missing would turn every deliberate pause into a fault.
//
// The session check must go through the agent's OWN tmux socket. Each agent
// runs under its own UID on its own socket (e.g. /tmp/tmux-2007/hive-scanner),
// so a query against the default socket answers "no server running" even when
// every session is alive — the exact false reading that has sent live
// diagnosis down the wrong path.
func (m *Manager) SessionMissing(name string) bool {
	m.mu.RLock()
	agent, ok := m.agents[name]
	if !ok || agent.State != StateRunning || agent.Paused {
		m.mu.RUnlock()
		return false
	}
	m.mu.RUnlock()
	// The exec runs outside the lock: it shells out to tmux, and holding a
	// manager lock across a subprocess is how the startup path has deadlocked
	// before.
	return !m.tmuxSessionExistsForAgent(agent)
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
	case bobBackend:
		// bob has no deny-tool flag, so ToolsConfig cannot be expressed here.
		// It must still go through bobLaunchCmd rather than falling to the
		// default branch below, which would append `--model` and crash bob
		// with "Cannot read properties of undefined (reading 'maxTokens')".
		// A bob agent with tools configured therefore launches identically to
		// one without; the deny patterns are silently inapplicable, exactly as
		// they already were before this branch existed.
		return bobLaunchCmd(binary)
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
		// Never pass --enable-all-github-mcp-tools: Copilot CLI's built-in GitHub
		// MCP server is READ-ONLY BY DEFAULT (v0.0.350+), so the write tools are
		// never registered and read tools stay available. Enabling it would turn
		// writes on and let agents author as the login USER via the MCP.
		cmd := fmt.Sprintf("%s --model %s --no-auto-update --allow-all", binary, model)
		for _, p := range denies {
			// Translate claude-style `mcp__github__<tool>` deny patterns to
			// copilot's built-in server syntax `github-mcp-server(<tool>)`. The
			// server is named `github-mcp-server` (per `copilot --help`), NOT
			// `github` — the old `github(` name matched nothing (silent no-op).
			copilotPattern := strings.ReplaceAll(p, "mcp__github__", "github-mcp-server(")
			if strings.HasPrefix(copilotPattern, "github-mcp-server(") {
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

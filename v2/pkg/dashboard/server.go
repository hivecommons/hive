package dashboard

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/hub"
	"github.com/kubestellar/hive/v2/pkg/openrouter"
)

//go:embed static
var staticFS embed.FS

func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

const agentSkipAfterFullBroadcastS = 5 * time.Second
const maxSSEClients = 100
const sessionCookieName = "hive_session"
const sessionCookieMaxAge = 30 * 24 * 60 * 60 // 30 days

type Server struct {
	port       int
	authToken  string
	statusMu   sync.RWMutex
	status     *StatusPayload
	sseClients map[chan []byte]struct{}
	sseMu      sync.Mutex
	logger     *slog.Logger
	mux        *http.ServeMux
	deps       *Dependencies
	sidebar    interface{}
	sidebarMu  sync.RWMutex

	// startedAt marks process start, used by /api/livez to bound the
	// startup-grace window before the first heartbeat has to have succeeded.
	startedAt time.Time

	agentPipelines map[string]map[string]bool
	agentHooks     map[string]map[string][]any
	pipelineMu     sync.RWMutex
	hooksMu        sync.RWMutex
	knowledgeMu    sync.Mutex
	levelMu        sync.Mutex
	restartMu      sync.Mutex // serializes concurrent agent restart operations

	acmmEvalMu       sync.RWMutex
	acmmEvalCache    *ACMMEvaluation
	acmmEvalCachedAt time.Time

	// Sparkline histories, all backed by the generic timeSeries ring buffer
	// (see timeseries.go). Lazily constructed via the tokenSeries()/factSeries()
	// /costSeries() accessors so the zero-value Server needs no constructor
	// change. Each keeps its own typed entry struct and JSON contract, so the
	// unification is internal — on-disk files and /api/* endpoints are unchanged.
	histOnce  sync.Once
	tokenHist *timeSeries[TokenSparklineEntry]
	factHist  *timeSeries[FactHistoryEntry]
	costHist  *timeSeries[CostHistoryEntry]

	// lastFullBroadcast is guarded by statusMu (set/read alongside s.status).
	lastFullBroadcast time.Time

	// fact/cost histories were migrated to the generic timeSeries store (#2041);
	// the trend history (#2039) remains a dedicated buffer for now.
	trendHistoryMu sync.RWMutex
	trendHistory   []TrendHistoryEntry

	advisoryMu     sync.RWMutex
	advisoryDigest any

	deviceFlowMu    sync.Mutex
	deviceFlowState *github.DeviceFlowState

	// userSessions maps a random opaque session id (stored in the client's
	// hive_session cookie on direct-route spokes) to the authenticated user.
	// This replaces the previous single shared s.authToken cookie so two
	// different people get two distinct sessions and each sees THEMSELVES.
	sessionMu    sync.RWMutex
	userSessions map[string]*userSession
	// sessionStorePath, when non-empty, persists userSessions across restarts
	// (see EnableSessionPersistence). Guarded by sessionMu.
	sessionStorePath string

	claudeOAuthFlow claudeOAuthFlow

	copilotAuthFlow copilotAuthFlow

	// openRouterStateStore holds in-progress OpenRouter "scan-to-fund" PKCE
	// flows (single-use state → verifier/hive/model). Lazily initialized via
	// openRouterState() so the zero-value Server needs no constructor change.
	openRouterStateOnce  sync.Once
	openRouterStateStore *openrouter.StateStore

	audit *AuditLog

	versionMu           sync.RWMutex
	cachedLatestHash    string
	cachedLatestMessage string
	cachedLatestAt      time.Time

	contributeHub *ContributeWSHub

	inferenceMu        sync.RWMutex
	inferenceEndpoints map[string][]string // backend id → list of base URLs

	// cliModels caches best-effort runtime model discovery for the CLI
	// backends (copilot/claude/gemini/goose), each with its own discovery
	// source and static fallback. See cli_models.go.
	cliModels *cliModelCache

	ready   bool
	readyAt time.Time

	githubAppMu               sync.RWMutex
	githubAppRequired         bool
	githubAppInstallURL       string
	githubAppPermIssue        string // non-empty when app is installed but lacks required permissions
	pendingGitHubAppInstall   bool
	pendingGitHubAppInstallAt time.Time

	systemAlertsMu sync.RWMutex
	systemAlerts   []SystemAlert

	hubBannerMu sync.RWMutex
	hubBanner   *HubBannerState

	githubAppRecheckFn func() bool

	listenerMu      sync.RWMutex
	listener        net.Listener
	listenerAddr    string
	listenerReady   chan struct{}
	listenerStopped chan struct{}
	listenerErr     error
	listenerServing bool
}

// StatusPayload matches the JSON contract the dashboard frontend render() expects.
type StatusPayload struct {
	Timestamp           string                 `json:"timestamp"`
	HiveID              string                 `json:"hiveId"`
	Agents              []FrontendAgent        `json:"agents"`
	Governor            FrontendGovernor       `json:"governor"`
	Tokens              FrontendTokens         `json:"tokens"`
	Repos               []FrontendRepo         `json:"repos"`
	Beads               FrontendBeads          `json:"beads"`
	Health              map[string]any         `json:"health"`
	Budget              FrontendBudget         `json:"budget"`
	CadenceMatrix       []FrontendCadence      `json:"cadenceMatrix"`
	GHRateLimits        map[string]any         `json:"ghRateLimits"`
	AgentMetrics        map[string]any         `json:"agentMetrics"`
	Hold                FrontendHold           `json:"hold"`
	IssueToMerge        map[string]any         `json:"issueToMerge"`
	ACMMLevel           int                    `json:"acmmLevel"`
	ACMMPackAgents      []string               `json:"acmmPackAgents"`
	AdvisoryDigest      any                    `json:"advisoryDigest,omitempty"`
	ContributorPool     *ContributorPoolStatus `json:"contributorPool,omitempty"`
	SystemResources     *SystemResources       `json:"systemResources,omitempty"`
	GitHubAppRequired   bool                   `json:"githubAppRequired,omitempty"`
	GitHubAppInstallURL string                 `json:"githubAppInstallURL,omitempty"`
	GitHubAppPermIssue  string                 `json:"githubAppPermIssue,omitempty"`
	GitHubBaseURL       string                 `json:"githubBaseURL,omitempty"`
	InferenceBackends   []InferenceBackend     `json:"inferenceBackends,omitempty"`
	SystemAlerts        []SystemAlert          `json:"systemAlerts,omitempty"`
	HubBanner           *HubBannerState        `json:"hubBanner,omitempty"`
}

// HubBannerState is a banner message from the hub admin displayed on spoke dashboards.
type HubBannerState struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Color   string `json:"color"`
}

// SystemAlert represents a critical runtime problem surfaced to the dashboard.
type SystemAlert struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// InferenceBackend describes a live inference endpoint and its available models.
type InferenceBackend struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Inference bool     `json:"inference"`
	Models    []string `json:"models"`
}

type FrontendAgent struct {
	Name             string `json:"name"`
	ID               string `json:"id"`
	DisplayName      string `json:"displayName,omitempty"`
	Description      string `json:"description,omitempty"`
	Role             string `json:"role,omitempty"`
	SortOrder        int    `json:"sortOrder"`
	Emoji            string `json:"emoji,omitempty"`
	Color            string `json:"color,omitempty"`
	BeadRole         string `json:"beadRole,omitempty"`
	Managed          bool   `json:"managed,omitempty"`
	Session          string `json:"session"`
	State            string `json:"state"`
	Busy             string `json:"busy"`
	Paused           bool   `json:"paused"`
	PausedAt         string `json:"pausedAt,omitempty"`
	PausedReason     string `json:"pausedReason,omitempty"`
	PausedTrigger    string `json:"pausedTrigger,omitempty"`
	OffByCadence     bool   `json:"offByCadence"`
	NeedsLogin       bool   `json:"needsLogin"`
	AuthAvailable    bool   `json:"authAvailable"`
	AuthKnown        bool   `json:"authKnown"`
	CLI              string `json:"cli"`
	Model            string `json:"model"`
	Cadence          string `json:"cadence"`
	Doing            string `json:"doing"`
	PinnedCli        bool   `json:"pinnedCli"`
	PinnedModel      bool   `json:"pinnedModel"`
	PinnedBoth       bool   `json:"pinnedBoth"`
	Pinned           bool   `json:"pinned"`
	LastKick         string `json:"lastKick,omitempty"`
	NextKick         string `json:"nextKick,omitempty"`
	Restarts         int    `json:"restarts"`
	LiveSummary      string `json:"liveSummary,omitempty"`
	DetailSummary    string `json:"detailSummary,omitempty"`
	StructuredStatus string `json:"structuredStatus,omitempty"`
	StatusEvidence   string `json:"statusEvidence,omitempty"`
	SummaryUpdated   string `json:"summaryUpdated,omitempty"`
	GovBackend       string `json:"govBackend"`
	GovModel         string `json:"govModel"`
	GovCostWeight    int    `json:"govCostWeight"`
	GovReason        string `json:"govReason,omitempty"`
	StatsConfig      []any  `json:"statsConfig"`
	Mode             string `json:"mode,omitempty"`
	ModeEmoji        string `json:"modeEmoji,omitempty"`
	DefaultMode      string `json:"defaultMode,omitempty"`
	IsCustomMode     bool   `json:"isCustomMode,omitempty"`
	NeedsRestart     bool   `json:"needsRestart,omitempty"`
	ProxyViolations  int    `json:"proxyViolations"`
	OnDemand         bool   `json:"onDemand,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	StallNudges      int    `json:"stallNudges,omitempty"`
	ActionNudges     int    `json:"actionNudges,omitempty"`
}

type FrontendGovernor struct {
	Active     bool               `json:"active"`
	Mode       string             `json:"mode"`
	Issues     int                `json:"issues"`
	PRs        int                `json:"prs"`
	Thresholds FrontendThresholds `json:"thresholds"`
	NextKick   string             `json:"nextKick,omitempty"`
}

type FrontendThresholds struct {
	Quiet int `json:"quiet"`
	Busy  int `json:"busy"`
	Surge int `json:"surge"`
}

type FrontendTokens struct {
	LookbackHours int                            `json:"lookbackHours"`
	Sessions      []FrontendSession              `json:"sessions"`
	Totals        FrontendTokenTotals            `json:"totals"`
	ByAgent       map[string]FrontendTokenBucket `json:"byAgent"`
	ByModel       map[string]FrontendTokenBucket `json:"byModel"`
}

type FrontendTokenTotals struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheCreate int64 `json:"cacheCreate"`
	Messages    int   `json:"messages"`
	Sessions    int   `json:"sessions"`
}

type FrontendTokenBucket struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cacheRead"`
	CacheCreate   int64 `json:"cacheCreate,omitempty"`
	Messages      int   `json:"messages,omitempty"`
	Sessions      int   `json:"sessions,omitempty"`
	AvgPerSession int64 `json:"avgPerSession,omitempty"`
}

// FrontendSession represents an individual CLI session for the Active Sessions list.
type FrontendSession struct {
	ID         string `json:"id"`
	Agent      string `json:"agent"`
	Model      string `json:"model"`
	Total      int64  `json:"total"`
	Messages   int    `json:"messages"`
	LastActive string `json:"lastActive,omitempty"`
	Estimated  bool   `json:"estimated,omitempty"`
}

type FrontendRepo struct {
	Name             string `json:"name"`
	Full             string `json:"full"`
	Issues           int    `json:"issues"`
	PRs              int    `json:"prs"`
	ActionableIssues []any  `json:"actionableIssues"`
	OpenPrs          []any  `json:"openPrs"`
}

type FrontendBeads struct {
	Workers    int `json:"workers"`
	Supervisor int `json:"supervisor"`
}

type FrontendBudget struct {
	WeeklyBudget    int64   `json:"BUDGET_WEEKLY"`
	Used            int64   `json:"BUDGET_USED"`
	Remaining       int64   `json:"BUDGET_REMAINING"`
	PctUsed         float64 `json:"BUDGET_PCT_USED"`
	BurnRateHourly  float64 `json:"BURN_RATE_HOURLY"`
	BurnRateInstant float64 `json:"BURN_RATE_INSTANT"`
	HoursElapsed    float64 `json:"HOURS_ELAPSED"`
	HoursRemaining  float64 `json:"HOURS_REMAINING"`
	ProjectedWeekly int64   `json:"PROJECTED_WEEKLY"`
	ProjectedPct    float64 `json:"PROJECTED_PCT"`
	LastUpdated     string  `json:"LAST_UPDATED"`
	// Exhausted is true when the weekly limit is set and window spend has
	// reached it — the governor is suppressing kicks for non-exempt agents.
	Exhausted bool `json:"BUDGET_EXHAUSTED"`
	// WindowEndsAt is when the current budget window rolls (RFC3339);
	// empty unless a weekly limit is set and a window is open.
	WindowEndsAt string `json:"WINDOW_ENDS_AT"`
}

type FrontendCadence struct {
	Agent string `json:"agent"`
	Idle  string `json:"idle"`
	Quiet string `json:"quiet"`
	Busy  string `json:"busy"`
	Surge string `json:"surge"`
}

type FrontendHold struct {
	Issues int   `json:"issues"`
	PRs    int   `json:"prs"`
	Total  int   `json:"total"`
	Items  []any `json:"items"`
}

// TokenSparklineEntry is a single timestamped snapshot of token metrics,
// persisted to disk so sparklines survive container restarts.
type TokenSparklineEntry struct {
	Timestamp   int64            `json:"t"`
	Input       int64            `json:"tokenInput"`
	Output      int64            `json:"tokenOutput"`
	CacheRead   int64            `json:"tokenCacheRead"`
	CacheCreate int64            `json:"tokenCacheCreate"`
	Messages    int              `json:"tokenMessages"`
	ByAgent     map[string]int64 `json:"tokens,omitempty"`
	ByModel     map[string]int64 `json:"tokenModels,omitempty"`
}

// tokenSparklineMaxEntries caps the on-disk history to ~24h at 5-min intervals.
const tokenSparklineMaxEntries = 288

// FactHistoryEntry records a total-facts snapshot at a point in time.
type FactHistoryEntry struct {
	Timestamp int64 `json:"t"`
	Count     int   `json:"count"`
}

// factHistoryMaxEntries caps the fact sparkline to ~30 days at 5-min intervals.
const factHistoryMaxEntries = 8640

// factHistoryMinIntervalMs prevents recording more than once per 5 minutes (ms).
const factHistoryMinIntervalMs = 300_000

// CostHistoryEntry records an estimated-cost ($) snapshot at a point in time.
// USD is the all-time cumulative estimated total (token counts × list prices),
// the same figure GET /api/cost returns.
type CostHistoryEntry struct {
	Timestamp int64   `json:"t"`
	USD       float64 `json:"usd"`
	// Agents maps agent name → cumulative estimated $ at this snapshot,
	// enabling per-agent spend-over-window on the client. Omitted on entries
	// recorded before this field existed.
	Agents map[string]float64 `json:"agents,omitempty"`
	// Models maps model name → cumulative token/cost snapshot, feeding the
	// per-model mini sparklines in the cost table. Omitted on older entries.
	Models map[string]CostModelSnap `json:"models,omitempty"`
}

// CostModelSnap is one model's cumulative counters at a history snapshot.
type CostModelSnap struct {
	Input  int64   `json:"i"`
	Output int64   `json:"o"`
	USD    float64 `json:"usd"`
}

// costHistoryMaxEntries caps the cost sparkline to ~30 days at 5-min intervals,
// mirroring factHistoryMaxEntries.
const costHistoryMaxEntries = 8640

// costHistoryMinIntervalMs prevents recording more than once per 5 minutes (ms),
// mirroring factHistoryMinIntervalMs.
const costHistoryMinIntervalMs = 300_000

// TrendHistoryEntry records a point-in-time snapshot of the governor / per-repo
// / beads / system-gauge trends that were previously kept only in the browser's
// localStorage (hive_sparkline_history) and thus lost on a pod restart or a new
// viewer. Persisting these server-side (same ring-buffer + PVC-seed treatment
// as the fact/cost histories) makes the corresponding sparklines survive
// restarts and render immediately for any viewer.
type TrendHistoryEntry struct {
	Timestamp int64 `json:"t"`
	// Governor actionable counts.
	GovIssues int `json:"govIssues"`
	GovPrs    int `json:"govPrs"`
	GovTotal  int `json:"govTotal"`
	GovHold   int `json:"govHold"`
	// Beads worker/supervisor counts.
	BeadsWorkers    int `json:"beadsWorkers"`
	BeadsSupervisor int `json:"beadsSupervisor"`
	// Repos maps repo name → issues/prs at this snapshot. Omitted when empty.
	Repos map[string]TrendRepoSnap `json:"repos,omitempty"`
	// System gauges (disk/mem/cpu percent). Pointer so an entry recorded when
	// systemResources was unavailable omits the field rather than reporting 0.
	System *TrendSystemSnap `json:"system,omitempty"`
}

// TrendRepoSnap is one repo's actionable issue/PR counts at a snapshot.
type TrendRepoSnap struct {
	Issues int `json:"issues"`
	PRs    int `json:"prs"`
}

// TrendSystemSnap is the disk/mem/cpu percentages at a snapshot.
type TrendSystemSnap struct {
	DiskPct float64 `json:"diskPct"`
	MemPct  float64 `json:"memPct"`
	CpuPct  float64 `json:"cpuPct"`
}

// trendHistoryMaxEntries caps the trend sparklines to ~30 days at 5-min
// intervals, mirroring factHistoryMaxEntries / costHistoryMaxEntries.
const trendHistoryMaxEntries = 8640

// trendHistoryMinIntervalMs prevents recording more than once per 5 minutes (ms),
// mirroring factHistoryMinIntervalMs / costHistoryMinIntervalMs.
const trendHistoryMinIntervalMs = 300_000

const sseRetryMs = 3000

func NewServer(port int, logger *slog.Logger) *Server {
	s := &Server{
		port:            port,
		sseClients:      make(map[chan []byte]struct{}),
		logger:          logger,
		mux:             http.NewServeMux(),
		agentPipelines:  make(map[string]map[string]bool),
		agentHooks:      make(map[string]map[string][]any),
		audit:           newAuditLog(),
		userSessions:    make(map[string]*userSession),
		cliModels:       newCLIModelCache(),
		listenerReady:   make(chan struct{}),
		listenerStopped: make(chan struct{}),
		startedAt:       time.Now(),
	}
	s.registerCoreRoutes()
	return s
}

func NewServerWithAuth(port int, authToken string, logger *slog.Logger) *Server {
	s := &Server{
		port:            port,
		authToken:       authToken,
		sseClients:      make(map[chan []byte]struct{}),
		logger:          logger,
		mux:             http.NewServeMux(),
		agentPipelines:  make(map[string]map[string]bool),
		agentHooks:      make(map[string]map[string][]any),
		audit:           newAuditLog(),
		userSessions:    make(map[string]*userSession),
		cliModels:       newCLIModelCache(),
		listenerReady:   make(chan struct{}),
		listenerStopped: make(chan struct{}),
		startedAt:       time.Now(),
	}
	s.registerCoreRoutes()
	return s
}

// SetInferenceEndpoints registers base URLs for inference backends
// so the dashboard can query them for available models at runtime.
func (s *Server) SetInferenceEndpoints(endpoints map[string][]string) {
	s.inferenceMu.Lock()
	defer s.inferenceMu.Unlock()
	s.inferenceEndpoints = endpoints
}

// UpdateInferenceEndpoint registers or replaces the endpoint list for a
// single inference backend at runtime (e.g. after a governor LiteLLM
// config save). An empty list unregisters the backend.
func (s *Server) UpdateInferenceEndpoint(backend string, endpoints []string) {
	s.inferenceMu.Lock()
	defer s.inferenceMu.Unlock()
	if s.inferenceEndpoints == nil {
		s.inferenceEndpoints = make(map[string][]string)
	}
	if len(endpoints) == 0 {
		delete(s.inferenceEndpoints, backend)
		return
	}
	s.inferenceEndpoints[backend] = endpoints
}

// getInferenceEndpoints returns the registered base URLs for a backend.
func (s *Server) getInferenceEndpoints(backend string) ([]string, bool) {
	s.inferenceMu.RLock()
	defer s.inferenceMu.RUnlock()
	endpoints, ok := s.inferenceEndpoints[backend]
	return endpoints, ok
}

func (s *Server) buildInferenceBackends() []InferenceBackend {
	var backends []InferenceBackend
	for _, b := range []struct{ id, name string }{
		{"vllm", "vLLM (self-hosted)"},
		{"llm-d", "llm-d (self-hosted)"},
	} {
		models := s.queryInferenceModels(b.id)
		backends = append(backends, InferenceBackend{
			ID: b.id, Name: b.name, Inference: true, Models: models,
		})
	}
	// litellm has no in-cluster default — include it only when an endpoint
	// is registered, so an unconfigured backend isn't SSE-pushed empty.
	if endpoints, ok := s.getInferenceEndpoints("litellm"); ok && len(endpoints) > 0 {
		backends = append(backends, InferenceBackend{
			ID: "litellm", Name: "LiteLLM (proxy)", Inference: true,
			Models: s.queryInferenceModels("litellm"),
		})
	}
	return backends
}

// SetSkipReloadFunc is retained for embedded callers that still use the
// legacy callback. Production config persistence uses SetConfigSaveFunc.
func (s *Server) SetSkipReloadFunc(fn func()) {
	if s.deps != nil {
		s.deps.SkipReloadFunc = fn
	}
}

// SetConfigSaveFunc installs the digest-aware persistence wrapper used by the
// runtime config coordinator. Call after constructing the config watcher.
func (s *Server) SetConfigSaveFunc(fn ConfigSaveFunc) {
	if s.deps != nil && s.deps.ConfigCoordinator != nil {
		s.deps.ConfigCoordinator.SetSaveFunc(fn)
	}
}

func (s *Server) registerCoreRoutes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/health/deep", s.handleHealthDeep)
	s.mux.HandleFunc("GET /api/livez", s.handleLivez)
	// Prometheus scrape endpoint for estimated LLM cost — opt-in, since it
	// exposes cost data unauthenticated (Prometheus can't do device-flow auth).
	// Enabled only when HIVE_METRICS_ENABLED is truthy; see isPublicPath.
	if metricsEnabled() {
		s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	}
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/events", s.handleSSE)
	s.mux.HandleFunc("POST /api/github-app/recheck", s.handleGitHubAppRecheck)
	s.mux.HandleFunc("POST /api/github-app/install-clicked", s.handleGitHubAppInstallClicked)
	// SSO handoff: exchange a hub-minted, HMAC-signed token for a local session
	// so a hub-authenticated user opens this (direct-route) spoke without a
	// second GitHub device-flow login. Public path (see isPublicPath) because
	// the caller has no session yet — the token IS the credential.
	s.mux.HandleFunc("GET /sso", s.handleSSO)
}

func (s *Server) Start() error {
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		s.markListenerStopped(err)
		return fmt.Errorf("loading embedded static files: %w", err)
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(staticContent)))

	// authenticate is outermost so the identity headers it injects from a
	// per-user session are visible to roleEnforcement's read-only write-gate.
	handler := s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))

	const dashboardReadTimeout = 30 * time.Second
	const dashboardIdleTimeout = 120 * time.Second
	addr := fmt.Sprintf(":%d", s.port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		s.markListenerStopped(err)
		return fmt.Errorf("bind dashboard listener %s: %w", addr, err)
	}
	if err := s.markListenerStarted(listener); err != nil {
		_ = listener.Close()
		s.markListenerStopped(err)
		return err
	}
	s.logger.Info("dashboard starting", "addr", addr)
	srv := &http.Server{
		Addr:        listener.Addr().String(),
		Handler:     handler,
		ReadTimeout: dashboardReadTimeout,
		IdleTimeout: dashboardIdleTimeout,
	}
	err = srv.Serve(listener)
	s.markListenerStopped(err)
	return err
}

func (s *Server) ensureListenerLifecycleLocked() {
	if s.listenerReady == nil {
		s.listenerReady = make(chan struct{})
	}
	if s.listenerStopped == nil {
		s.listenerStopped = make(chan struct{})
	}
}

func (s *Server) markListenerStarted(listener net.Listener) error {
	if listener == nil {
		return errors.New("dashboard listener is nil")
	}
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	s.ensureListenerLifecycleLocked()
	if s.listener != nil || s.listenerServing || s.listenerAddr != "" {
		return errors.New("dashboard listener was already started")
	}
	s.listener = listener
	s.listenerAddr = listener.Addr().String()
	s.listenerServing = true
	close(s.listenerReady)
	return nil
}

func (s *Server) markListenerStopped(err error) {
	s.listenerMu.Lock()
	s.ensureListenerLifecycleLocked()
	s.listenerServing = false
	s.listenerErr = err
	select {
	case <-s.listenerStopped:
	default:
		close(s.listenerStopped)
	}
	s.listenerMu.Unlock()

	s.statusMu.Lock()
	s.ready = false
	s.statusMu.Unlock()
}

// WaitForListener waits for the real socket bind, not merely process startup.
func (s *Server) WaitForListener(ctx context.Context) error {
	s.listenerMu.Lock()
	s.ensureListenerLifecycleLocked()
	ready, stopped := s.listenerReady, s.listenerStopped
	serving, listenerErr := s.listenerServing, s.listenerErr
	s.listenerMu.Unlock()
	if serving {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
		if s.ListenerServing() {
			return nil
		}
		return s.listenerFailure()
	case <-stopped:
		if listenerErr != nil {
			return listenerErr
		}
		return s.listenerFailure()
	}
}

// WaitForHTTP proves that the bound listener is serving the dashboard handler.
// Any HTTP response is sufficient before MarkReady; /api/health intentionally
// returns 503 during this startup phase.
func (s *Server) WaitForHTTP(ctx context.Context) error {
	if err := s.WaitForListener(ctx); err != nil {
		return err
	}
	client := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	defer client.CloseIdleConnections()
	for {
		if !s.ListenerServing() {
			return s.listenerFailure()
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ListenerURL()+"/api/health", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Server) ListenerServing() bool {
	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()
	return s.listenerServing
}

func (s *Server) ListenerPort() int {
	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()
	if s.listener == nil {
		return 0
	}
	if address, ok := s.listener.Addr().(*net.TCPAddr); ok {
		return address.Port
	}
	return 0
}

func (s *Server) ListenerURL() string {
	port := s.ListenerPort()
	if port <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

func (s *Server) listenerFailure() error {
	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()
	if s.listenerErr != nil {
		return s.listenerErr
	}
	return errors.New("dashboard listener is not serving HTTP")
}

// Stop closes the active listener. Runtime shutdown normally happens through
// process cancellation; this method also keeps focused listener tests bounded.
func (s *Server) Stop() error {
	s.listenerMu.RLock()
	listener := s.listener
	s.listenerMu.RUnlock()
	if listener == nil {
		return nil
	}
	return listener.Close()
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self' ws: wss:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves the caller's identity and enforces authentication. It
// runs OUTERMOST — before roleEnforcement — so that the X-Hive-User/X-Hive-Role
// it injects from a per-user session are visible to roleEnforcement's read-only
// write-gate. (If this ran inside roleEnforcement, roleEnforcement would read an
// empty role and never block a read-only viewer's writes.)
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directRouteAuthz := s.directRouteAuthzEnabled()

		// FAIL CLOSED: a spoke with an authorized_users allowlist (direct-route)
		// MUST enforce auth even if authToken is empty. Previously an empty
		// authToken short-circuited ALL auth here, silently leaving the dashboard
		// WIDE OPEN despite the allowlist — anyone with the URL got in. That was a
		// real exposure on direct-route spokes provisioned without an auth_token.
		// The allowlist is the security boundary on these standalone spokes, so
		// its mere presence must force authentication; identity then comes only
		// from a server-side session (device-flow login), never a bypass.
		//
		// The authToken=="" bypass remains for spokes that are genuinely open by
		// design (no allowlist AND no token) — e.g. a local/dev dashboard.
		if isPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if s.authToken == "" && !directRouteAuthz {
			next.ServeHTTP(w, r)
			return
		}

		// On a direct-route spoke (per-hive allowlist present) we MUST NOT trust
		// client-supplied X-Hive-User/X-Hive-Role: there is no hub nginx in
		// front to strip forged identity headers. Strip them here so identity
		// can only come from a server-side session we minted.
		if directRouteAuthz {
			r.Header.Del("X-Hive-User")
			r.Header.Del("X-Hive-Role")
		}

		// Internal automation authenticates with the shared token via the
		// X-Hive-Internal header; this is a trusted server-to-server path
		// (the local proxy injects it) and carries no browser user identity.
		// Guard against an empty authToken: subtle.ConstantTimeCompare("","")
		// is TRUE, so without this an absent/empty header would authenticate on
		// a direct-route spoke that has no token. The shared-token paths are only
		// valid when a real token is configured.
		trusted := s.authToken != "" && secureCompare(r.Header.Get("X-Hive-Internal"), s.authToken)

		// Hub-proxied path: nginx injects both headers from the hub's
		// per-user/per-hive auth-check. Only trust them when this spoke is NOT a
		// direct-route spoke (see strip above). Requiring both headers prevents
		// trivial bypass via a single forged header.
		if !trusted && !directRouteAuthz &&
			r.Header.Get("X-Hive-User") != "" && r.Header.Get("X-Hive-Role") != "" {
			trusted = true
		}

		// Per-user session path (device flow): resolve the session id in the
		// hive_session cookie to THIS request's user and inject that user's
		// identity. Two different people therefore get two different sessions
		// and each sees themselves — no shared identity.
		if !trusted {
			if sess := s.sessionFromRequest(r); sess != nil {
				r.Header.Set("X-Hive-User", sess.Username)
				r.Header.Set("X-Hive-Role", sess.Role)
				trusted = true
			}
		}

		// Bearer/query shared-token path for programmatic API clients. This is
		// an internal credential, not a browser session, so it is only accepted
		// from the Authorization header or ?token= — never from the session
		// cookie. On a direct-route spoke it is DISABLED: the shared token grants
		// no per-user identity, so accepting it would let any holder act as an
		// unscoped owner and defeat the per-hive allowlist. Direct-route callers
		// must use a per-user session instead.
		if !trusted && !directRouteAuthz && s.authToken != "" {
			token := r.Header.Get("Authorization")
			if token == "" {
				token = r.URL.Query().Get("token")
			}
			expected := "Bearer " + s.authToken
			if secureCompare(token, expected) || secureCompare(token, s.authToken) {
				trusted = true
			}
		}

		if !trusted {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			} else {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(loginPage))
			}
			return
		}

		next.ServeHTTP(w, r)
	})
}

// directRouteAuthzEnabled reports whether this spoke enforces per-user
// authorization on device-flow logins (a per-hive authorized-users allowlist is
// configured). When true the spoke is reached directly (no hub nginx), so
// client-supplied identity headers are untrusted and identity comes only from a
// server-side session.
func (s *Server) directRouteAuthzEnabled() bool {
	return s.deps != nil && s.deps.Config != nil &&
		s.deps.Config.Dashboard.IsDirectRouteAuthzEnabled()
}

// isPublicPath returns true for paths that should be accessible without
// authentication even when DASHBOARD_AUTH_TOKEN is set. This covers health
// checks, the snapshot preview, the contribute flow, and auth negotiation.
func isPublicPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/api/health"):
		return true
	case path == "/api/livez":
		// The kubelet liveness probe hits this UNAUTHENTICATED — it must be
		// public like /api/health, or every probe 401s, the probe never fails
		// on a stale heartbeat (it fails on auth instead), and a dead-heartbeat
		// pod is never restarted (the exact bug this endpoint was added to fix).
		return true
	case path == "/api/auth/token":
		return true
	case path == "/metrics" && metricsEnabled():
		// Prometheus scrape target — public only when explicitly enabled via
		// HIVE_METRICS_ENABLED. Prometheus cannot authenticate via device flow.
		return true
	case path == "/snapshot" || strings.HasPrefix(path, "/snapshot/"):
		return true
	case strings.HasPrefix(path, "/api/snapshot"):
		return true
	case path == "/contribute" || strings.HasPrefix(path, "/contribute/"):
		return true
	case strings.HasPrefix(path, "/api/contribute"):
		return true
	case path == "/leaderboard" || strings.HasPrefix(path, "/leaderboard/"):
		return true
	case strings.HasPrefix(path, "/api/leaderboard"):
		return true
	case strings.HasPrefix(path, "/api/gh-user-auth/"):
		return true
	case path == openRouterCallbackPath:
		// OpenRouter OAuth PKCE return: the sponsor's browser comes back with no
		// session, so the path must be public. The single-use state token in the
		// query IS the credential — the handler verifies it (unknown/expired/
		// replayed states are rejected) before storing anything.
		return true
	case path == "/sso":
		// SSO handoff exchange: the caller has no session yet, the signed hub
		// token IS the credential. The handler itself verifies the token and
		// the authorized_users allowlist before minting a session, so exposing
		// the path unauthenticated does not weaken the allowlist gate.
		return true
	default:
		return false
	}
}

// loginPage is a self-contained HTML page served to unauthenticated browser
// requests. It drives the GitHub Device Flow so users can log in without
// the full dashboard SPA being publicly accessible.
const loginPage = `<!DOCTYPE html>
<html lang="en"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Hive — Sign In</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,sans-serif;
  background:#0f172a;color:#e2e8f0;display:flex;align-items:center;justify-content:center;min-height:100vh}
.card{background:#1e293b;border-radius:12px;padding:40px;max-width:420px;width:90%;text-align:center;
  box-shadow:0 4px 24px rgba(0,0,0,.4)}
h1{font-size:1.5rem;margin-bottom:8px;color:#f8fafc}
.subtitle{color:#94a3b8;margin-bottom:28px;font-size:.9rem}
.logo{font-size:2.5rem;margin-bottom:16px}
button{background:#238636;color:#fff;border:none;padding:12px 24px;border-radius:8px;font-size:1rem;
  cursor:pointer;width:100%;font-weight:600;transition:background .15s}
button:hover{background:#2ea043}
button:disabled{background:#374151;cursor:wait}
.code-wrap{position:relative;display:inline-block;margin:16px 0}
.code-box{font-family:monospace;font-size:2rem;font-weight:800;color:#60a5fa;letter-spacing:4px;
  padding:16px 48px 16px 16px;background:#0f172a;border-radius:8px;user-select:all}
.copy-btn{position:absolute;top:4px;right:4px;background:#238636;color:#fff;border:none;
  padding:3px 10px;border-radius:4px;cursor:pointer;font-size:.7rem;font-weight:600;width:auto;
  transition:background .15s}
.copy-btn:hover{background:#2ea043}
.copy-btn.copied{background:#166534}
.instructions{color:#94a3b8;font-size:.85rem;line-height:1.6;margin-bottom:16px}
a{color:#60a5fa;text-decoration:none}
a:hover{text-decoration:underline}
.status{margin-top:16px;font-size:.85rem;color:#94a3b8}
.spinner{display:inline-block;width:16px;height:16px;border:2px solid #475569;
  border-top-color:#60a5fa;border-radius:50%;animation:spin .8s linear infinite;vertical-align:middle;margin-right:6px}
@keyframes spin{to{transform:rotate(360deg)}}
.error{color:#f87171}
</style></head><body>
<div class="card">
  <div class="logo">🐝</div>
  <h1>Hive</h1>
  <p class="subtitle">Sign in with GitHub to access this dashboard</p>
  <div id="step-start">
    <button id="btn-start" onclick="startFlow()">Sign in with GitHub</button>
  </div>
  <div id="step-code" style="display:none">
    <p class="instructions">Enter this code at GitHub:</p>
    <div class="code-wrap">
      <div class="code-box" id="user-code">--------</div>
      <button class="copy-btn" id="copy-btn" onclick="copyAndOpen()">Copy &amp; Open</button>
    </div>
    <p class="instructions"><a id="verify-link" href="#" target="_blank" rel="noopener">Open GitHub verification page ↗</a></p>
    <p class="status"><span class="spinner"></span> Waiting for authorization…</p>
  </div>
  <div id="step-done" style="display:none">
    <p style="font-size:1.2rem;color:#4ade80;margin-bottom:16px">✓ Signed in</p>
    <p class="instructions">Redirecting…</p>
  </div>
  <div id="step-error" style="display:none">
    <p class="error" id="error-msg"></p>
    <button onclick="location.reload()" style="margin-top:16px">Try again</button>
  </div>
</div>
<script>
function showStep(id){['step-start','step-code','step-done','step-error'].forEach(
  s=>document.getElementById(s).style.display=s===id?'block':'none')}
function showError(msg){document.getElementById('error-msg').textContent=msg;showStep('step-error')}
function copyAndOpen(){
  var code=document.getElementById('user-code').textContent;
  var url=document.getElementById('verify-link').href;
  var btn=document.getElementById('copy-btn');
  function open(){
    // Open the GitHub verification page in a new tab where the code is
    // already on the clipboard, ready to paste — one click, not two.
    if(url && url!=='#'){window.open(url,'_blank','noopener')}
    btn.textContent='Copied!';btn.classList.add('copied');
    setTimeout(function(){btn.textContent='Copy \u0026 Open';btn.classList.remove('copied')},2000);
  }
  if(navigator.clipboard && navigator.clipboard.writeText){
    navigator.clipboard.writeText(code).then(open,open);
  } else { open(); }
}
async function startFlow(){
  document.getElementById('btn-start').disabled=true;
  try{
    var r=await fetch('/api/gh-user-auth/start',{method:'POST'});
    var d=await r.json();
    if(!r.ok){showError(d.error||'Failed to start login');return}
    document.getElementById('user-code').textContent=d.user_code;
    document.getElementById('verify-link').href=d.verification_uri;
    showStep('step-code');
    poll(d.interval||5);
  }catch(e){showError('Network error: '+e.message)}
}
async function poll(interval){
  var ms=interval*1000;
  async function check(){
    try{
      var r=await fetch('/api/gh-user-auth/poll',{method:'POST'});
      var d=await r.json();
      if(d.status==='complete'){showStep('step-done');setTimeout(function(){location.href='/api/gh-user-auth/session'},1000);return}
      if(d.status==='error'){showError(d.error||'Authorization failed');return}
      if(d.status==='slow_down'){ms=Math.min(ms+5000,30000);setTimeout(check,ms);return}
      if(d.status==='pending'){setTimeout(check,ms);return}
      // Any other shape is terminal (e.g. an HTTP error body with no status) —
      // surface it instead of silently polling forever.
      if(!r.ok||d.error){showError(d.error||('Login failed ('+r.status+')'));return}
      setTimeout(check,ms);
    }catch(e){setTimeout(check,ms)}
  }
  setTimeout(check,ms);
}
</script></body></html>`

func (s *Server) Handler() http.Handler {
	return s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))
}

func (s *Server) roleEnforcement(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role := r.Header.Get("X-Hive-Role")
		if role == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Hive-Role", role)
		w.Header().Set("X-Hive-User", r.Header.Get("X-Hive-User"))
		if role == "read" && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !strings.HasPrefix(r.URL.Path, "/api/contribute") && r.URL.Path != "/api/gh-user-auth/status" {
				http.Error(w, `{"error":"your permissions on this hive are read-only, so changes are not allowed. Contact the owner of this hive to ask for write permissions."}`, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) UpdateStatus(status *StatusPayload) {
	if s.deps != nil && s.deps.Config != nil {
		status.ACMMLevel = detectACMMLevel(s.deps.Config)
		status.ACMMPackAgents = buildACMMPackAgents(s.deps.Config)
		status.GitHubBaseURL = s.deps.Config.GitHub.ResolvedBaseURL()
	}
	status.ContributorPool = s.BuildContributorPoolStatus()

	s.githubAppMu.RLock()
	status.GitHubAppRequired = s.githubAppRequired
	status.GitHubAppInstallURL = s.githubAppInstallURL
	status.GitHubAppPermIssue = s.githubAppPermIssue
	s.githubAppMu.RUnlock()

	status.InferenceBackends = s.buildInferenceBackends()

	s.systemAlertsMu.RLock()
	if len(s.systemAlerts) > 0 {
		status.SystemAlerts = make([]SystemAlert, len(s.systemAlerts))
		copy(status.SystemAlerts, s.systemAlerts)
	}
	s.systemAlertsMu.RUnlock()

	s.hubBannerMu.RLock()
	if s.hubBanner != nil {
		b := *s.hubBanner
		status.HubBanner = &b
	}
	s.hubBannerMu.RUnlock()

	s.statusMu.Lock()
	status.Timestamp = time.Now().UTC().Format(time.RFC3339)
	s.status = status
	s.lastFullBroadcast = time.Now()
	s.statusMu.Unlock()

	s.AppendTokenSparkline(status)
	s.AppendTrendHistory(status)

	data, err := json.Marshal(status)
	if err != nil {
		s.logger.Warn("failed to marshal status for SSE", "error", err)
		return
	}

	s.broadcastFrame(fmt.Sprintf("data: %s\n\n", data))
}

// AddSystemAlert adds a critical alert visible on the dashboard.
func (s *Server) AddSystemAlert(id, severity, message string) {
	s.systemAlertsMu.Lock()
	defer s.systemAlertsMu.Unlock()
	for i, a := range s.systemAlerts {
		if a.ID == id {
			s.systemAlerts[i].Message = message
			s.systemAlerts[i].Severity = severity
			return
		}
	}
	s.systemAlerts = append(s.systemAlerts, SystemAlert{ID: id, Severity: severity, Message: message})
}

// ClearSystemAlert removes an alert by ID.
func (s *Server) ClearSystemAlert(id string) {
	s.systemAlertsMu.Lock()
	defer s.systemAlertsMu.Unlock()
	for i, a := range s.systemAlerts {
		if a.ID == id {
			s.systemAlerts = append(s.systemAlerts[:i], s.systemAlerts[i+1:]...)
			return
		}
	}
}

// SetHubBanner sets the hub admin banner displayed on the spoke dashboard.
func (s *Server) SetHubBanner(id, message, color string) {
	s.hubBannerMu.Lock()
	defer s.hubBannerMu.Unlock()
	s.hubBanner = &HubBannerState{ID: id, Message: message, Color: color}
}

// ClearHubBanner removes the hub admin banner.
func (s *Server) ClearHubBanner() {
	s.hubBannerMu.Lock()
	defer s.hubBannerMu.Unlock()
	s.hubBanner = nil
}

func (s *Server) SetGitHubAppRequired(required bool) {
	s.githubAppMu.Lock()
	defer s.githubAppMu.Unlock()
	s.githubAppRequired = required
	if required && s.deps != nil && s.deps.Config != nil {
		s.githubAppInstallURL = s.deps.Config.GitHub.AppInstallURL()
	} else if required {
		s.githubAppInstallURL = "https://github.com/apps/kubestellar-hive/installations/new"
	} else {
		s.githubAppInstallURL = ""
		s.githubAppPermIssue = ""
	}
}

// SetGitHubAppPermIssue records that the app IS installed but lacks a specific
// write permission. The banner shows an "Insufficient Permissions" message
// instead of "Not Installed". Pass "" to clear.
func (s *Server) SetGitHubAppPermIssue(issue string) {
	s.githubAppMu.Lock()
	defer s.githubAppMu.Unlock()
	s.githubAppPermIssue = issue
}

func (s *Server) GetGitHubAppPermIssue() string {
	s.githubAppMu.RLock()
	defer s.githubAppMu.RUnlock()
	return s.githubAppPermIssue
}

func (s *Server) IsGitHubAppRequired() bool {
	s.githubAppMu.RLock()
	defer s.githubAppMu.RUnlock()
	return s.githubAppRequired
}

func (s *Server) SetPendingGitHubAppInstall() {
	s.githubAppMu.Lock()
	defer s.githubAppMu.Unlock()
	s.pendingGitHubAppInstall = true
	s.pendingGitHubAppInstallAt = time.Now()
}

func (s *Server) IsPendingGitHubAppInstall() bool {
	s.githubAppMu.RLock()
	defer s.githubAppMu.RUnlock()
	if !s.pendingGitHubAppInstall {
		return false
	}
	const pendingInstallExpiry = 10 * time.Minute
	if time.Since(s.pendingGitHubAppInstallAt) > pendingInstallExpiry {
		return false
	}
	return true
}

func (s *Server) ClearPendingGitHubAppInstall() {
	s.githubAppMu.Lock()
	defer s.githubAppMu.Unlock()
	s.pendingGitHubAppInstall = false
}

// UpdateGitHubClient swaps the GitHub client and app auth references stored in
// the dashboard dependencies. Called after the config API reinitializes app auth.
func (s *Server) UpdateGitHubClient(client *github.Client, auth *github.AppAuth) {
	if s.deps != nil {
		s.deps.GHClient = client
		s.deps.GHAppAuth = auth
	}
}

func (s *Server) SetGitHubAppRecheckFn(fn func() bool) {
	s.githubAppRecheckFn = fn
}

// RecheckGitHubApp runs the configured GitHub App verification (read + write,
// the same check the manual "Re-check" button performs) and, on success, clears
// the "App not installed" banner and its related pending/permission state.
// Returns true when the app is installed and write-verified. It is safe to call
// from a background loop as well as the HTTP handler; returns false (a no-op) if
// no recheck function has been wired up. This is the single place the banner is
// cleared on a successful recheck, so the periodic self-heal loop and the manual
// button stay in lockstep.
func (s *Server) RecheckGitHubApp() bool {
	if s.githubAppRecheckFn == nil {
		return false
	}
	ok := s.githubAppRecheckFn()
	if ok {
		s.SetGitHubAppRequired(false)
		s.SetGitHubAppPermIssue("")
		s.ClearPendingGitHubAppInstall()
	}
	return ok
}

func (s *Server) handleGitHubAppRecheck(w http.ResponseWriter, r *http.Request) {
	if s.githubAppRecheckFn == nil {
		http.Error(w, "recheck not configured", http.StatusNotImplemented)
		return
	}
	ok := s.RecheckGitHubApp()
	w.Header().Set("Content-Type", "application/json")
	if ok {
		w.Write([]byte(`{"status":"installed"}`))
	} else {
		s.githubAppMu.RLock()
		permIssue := s.githubAppPermIssue
		s.githubAppMu.RUnlock()
		if permIssue != "" {
			detail, _ := json.Marshal(permIssue)
			w.Write([]byte(`{"status":"insufficient_permissions","detail":` + string(detail) + `}`))
		} else {
			w.Write([]byte(`{"status":"not_installed"}`))
		}
	}
}

// BroadcastAgentStatus sends a lightweight agent-only SSE event on a fast
// cadence. Skipped if a full status was broadcast within the last 5 seconds
// to avoid redundant renders on the frontend.
func (s *Server) BroadcastAgentStatus(payload *AgentStatusPayload) {
	s.statusMu.RLock()
	recentFull := time.Since(s.lastFullBroadcast) < agentSkipAfterFullBroadcastS
	s.statusMu.RUnlock()
	if recentFull {
		return
	}

	data, err := json.Marshal(payload)
	if err != nil {
		s.logger.Warn("failed to marshal agent status for SSE", "error", err)
		return
	}

	s.broadcastFrame(fmt.Sprintf("event: agent-status\ndata: %s\n\n", data))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	ready := s.ready
	s.statusMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "starting"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

const (
	// heartbeatSendInterval mirrors the fixed 2-minute cadence StartHeartbeat
	// is launched with in cmd/hive/main.go (see the comment there — it's
	// deliberately independent of the governor eval interval so every hive,
	// regardless of ACMM level, beats comfortably under the hub's 5-minute
	// staleness window). Duplicated as a constant here rather than plumbed
	// through from main.go because the value is a cross-package contract
	// (hub-side staleness marking, spoke-side send cadence, and now this
	// liveness threshold all need to agree on it) and hub.heartbeatSendInterval
	// isn't exported. If that interval ever changes, update both.
	heartbeatSendInterval = 2 * time.Minute

	// livezHeartbeatStaleMax is how old the last successful heartbeat may be
	// before /api/livez reports unhealthy. Set to 3x the send interval (6
	// minutes): generous enough to absorb one or two transient hub-side
	// failures (network blip, hub restart, rate limit) without flapping the
	// pod, but tight enough that a genuinely dead heartbeat goroutine gets
	// caught and the pod restarted well within a single human-observable
	// "gray dot" investigation window.
	livezHeartbeatStaleMax = 3 * heartbeatSendInterval

	// livezStartupGrace bounds how long a freshly started process is treated
	// as healthy before it has sent its first successful heartbeat. Covers
	// waitForReady's own up-to-3-minute wait for the dashboard to come up
	// plus one heartbeat send attempt and hub round trip. Without this, a
	// pod would fail liveness during normal startup, before the heartbeat
	// loop ever got a chance to succeed.
	livezStartupGrace = 4 * time.Minute
)

// handleLivez is the liveness-only counterpart to /api/health. It includes
// everything /api/health checks (the HTTP server itself is responsive) PLUS,
// for hub-connected hives, a check that the heartbeat goroutine is actually
// still beating. The heartbeat goroutine can silently die (panic recovered
// upstream, deadlock, stuck HTTP call) while this HTTP server keeps serving
// fine — that's the bug this endpoint exists to catch: before this endpoint
// existed, /api/health stayed green in that case and kubelet had no reason
// to ever restart the pod, so the hub kept showing the hive as offline
// (gray dot) indefinitely even though the pod was 1/1 Running.
//
// Only the livenessProbe should point here. Readiness stays on /api/health:
// a transient hub outage would otherwise pull a perfectly-serving pod out of
// the Service's endpoints for no benefit (restarting won't fix a hub that's
// down). Liveness failing here IS the intended remedy for a dead heartbeat
// goroutine, since a restart revives it.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	ready := s.ready
	s.statusMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "starting"})
		return
	}

	// Hives with no hub configured never run a heartbeat loop at all, so
	// there is nothing to go stale — never gate their liveness on this.
	if hub.HeartbeatEnabled() {
		lastSuccess, hasSucceededOnce := hub.LastHeartbeatSuccess()
		switch {
		case !hasSucceededOnce:
			if age := time.Since(s.startedAt); age > livezStartupGrace {
				w.WriteHeader(http.StatusServiceUnavailable)
				json.NewEncoder(w).Encode(map[string]string{
					"status": "unhealthy",
					"detail": "no successful heartbeat since startup",
				})
				return
			}
		case time.Since(lastSuccess) > livezHeartbeatStaleMax:
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"status":            "unhealthy",
				"detail":            "heartbeat stale",
				"last_heartbeat_at": lastSuccess.UTC().Format(time.RFC3339),
			})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleHealthDeep(w http.ResponseWriter, r *http.Request) {
	checks := map[string]any{}
	overall := "ok"
	failCount := 0

	// 1. Basic readiness
	s.statusMu.RLock()
	ready := s.status != nil && s.ready
	s.statusMu.RUnlock()
	if ready {
		checks["ready"] = map[string]any{"status": "pass"}
	} else {
		checks["ready"] = map[string]any{"status": "fail", "detail": "status not yet available"}
		overall = "degraded"
		failCount++
	}

	// 2. GitHub auth
	if s.deps != nil && s.deps.GHAppAuth != nil {
		if _, err := s.deps.GHAppAuth.Token(s.deps.Ctx); err == nil {
			checks["github_auth"] = map[string]any{"status": "pass"}
		} else {
			checks["github_auth"] = map[string]any{"status": "fail", "detail": err.Error()}
			overall = "degraded"
			failCount++
		}
	} else if s.deps != nil && s.deps.GHClient != nil {
		checks["github_auth"] = map[string]any{"status": "pass", "detail": "token-based"}
	} else {
		checks["github_auth"] = map[string]any{"status": "fail", "detail": "no GitHub auth configured"}
		overall = "degraded"
		failCount++
	}

	// 3. Agents
	if s.deps != nil && s.deps.AgentMgr != nil {
		agentChecks := map[string]any{}
		for name, proc := range s.deps.AgentMgr.AllStatuses() {
			ac := map[string]any{
				"state": string(proc.State),
			}
			if proc.Paused {
				ac["paused"] = true
				ac["status"] = "skip"
			} else if proc.State == agent.StateRunning {
				ac["status"] = "pass"
				if proc.LastKick != nil {
					ac["last_kick"] = proc.LastKick.Format(time.RFC3339)
					ac["last_kick_age"] = time.Since(*proc.LastKick).Round(time.Second).String()
				}
				if proc.LastKickMessage != "" {
					ac["last_prompt_len"] = len(proc.LastKickMessage)
					hasRawVars := false
					for _, v := range []string{"${ISSUE_LIST}", "${PR_LIST}", "${HIVE_REPO}", "${KNOWLEDGE}"} {
						if strings.Contains(proc.LastKickMessage, v) {
							hasRawVars = true
							break
						}
					}
					if hasRawVars {
						ac["status"] = "warn"
						ac["detail"] = "unsubstituted template variables in last kick"
					}
				} else {
					ac["status"] = "warn"
					ac["detail"] = "no kick message recorded"
				}
				if proc.KickRefused {
					ac["status"] = "warn"
					ac["detail"] = "refused kick: " + proc.KickRefusalReason
				}
			} else {
				ac["status"] = "fail"
				failCount++
			}
			agentChecks[name] = ac
		}
		checks["agents"] = agentChecks
	}

	// 4. Governor
	if s.deps != nil && s.deps.Governor != nil {
		state := s.deps.Governor.GetState()
		govCheck := map[string]any{
			"status": "pass",
			"mode":   string(state.Mode),
			"issues": state.QueueIssues,
			"prs":    state.QueuePRs,
			"hold":   state.QueueHold,
		}
		checks["governor"] = govCheck
	}

	// 5. Contribute
	if s.contributeHub != nil {
		active := s.contributeHub.ActiveCount()
		checks["contribute"] = map[string]any{
			"status":              "pass",
			"active_contributors": active,
		}
	}

	// 6. Config
	if s.deps != nil && s.deps.Config != nil {
		cfg := s.deps.Config
		checks["config"] = map[string]any{
			"status":  "pass",
			"org":     cfg.Project.Org,
			"repos":   len(cfg.Project.Repos),
			"hive_id": cfg.HiveID,
		}
		if cfg.ACMMLevel != nil {
			checks["config"].(map[string]any)["acmm_level"] = *cfg.ACMMLevel
		}
	}

	// 7. Token consumption (progress signal)
	if s.deps != nil && s.deps.Tokens != nil {
		summary := s.deps.Tokens.Summary()
		if summary != nil {
			tokenCheck := map[string]any{
				"status":       "pass",
				"total_tokens": summary.TotalTokens,
				"sessions":     summary.TotalMessages,
				"by_agent":     summary.ByAgent,
			}
			if summary.TotalTokens == 0 {
				tokenCheck["status"] = "warn"
				tokenCheck["detail"] = "zero tokens consumed — agents may not be working"
			}
			checks["tokens"] = tokenCheck
		}
	}

	// 8. MTTR (progress signal)
	if s.deps != nil && s.deps.MetricsCollector != nil {
		mttr := s.deps.MetricsCollector.GetMTTR()
		if mttr != nil && mttr.Count > 0 {
			checks["mttr"] = map[string]any{
				"status":         "pass",
				"median_minutes": mttr.MedianMinutes,
				"avg_minutes":    mttr.AvgMinutes,
				"count":          mttr.Count,
			}
		}
	}

	// 9. Agent output freshness (stall detection)
	if s.deps != nil && s.deps.AgentMgr != nil {
		const staleOutputThreshold = 30 * time.Minute
		stalled := []string{}
		for name, proc := range s.deps.AgentMgr.AllStatuses() {
			if proc.State != agent.StateRunning || proc.Paused {
				continue
			}
			if proc.OutputBuffer != nil && proc.OutputBuffer.Count() == 0 && proc.LastKick != nil {
				if time.Since(*proc.LastKick) > staleOutputThreshold {
					stalled = append(stalled, name)
				}
			}
		}
		if len(stalled) > 0 {
			checks["stall_detection"] = map[string]any{
				"status": "warn",
				"detail": "agents kicked but no output for 30+ min",
				"agents": stalled,
			}
			if overall == "ok" {
				overall = "degraded"
			}
		} else {
			checks["stall_detection"] = map[string]any{"status": "pass"}
		}
	}

	// 10. Queue trend (is work being processed?)
	s.statusMu.RLock()
	if s.status != nil {
		totalActionable := 0
		for _, repo := range s.status.Repos {
			totalActionable += len(repo.ActionableIssues)
		}
		checks["queue"] = map[string]any{
			"status":     "pass",
			"actionable": totalActionable,
		}
	}
	s.statusMu.RUnlock()

	if failCount > 2 {
		overall = "critical"
	}

	w.Header().Set("Content-Type", "application/json")
	if overall != "ok" {
		w.WriteHeader(http.StatusOK)
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status": overall,
		"checks": checks,
		"fails":  failCount,
	})
}

func (s *Server) MarkReady() {
	s.statusMu.Lock()
	s.ready = true
	s.readyAt = time.Now()
	s.statusMu.Unlock()
	s.logger.Info("dashboard marked ready")
}

// MarkReadyIfListening closes the probe/ready race: either the listener is
// still serving while readiness is committed, or readiness remains false.
func (s *Server) MarkReadyIfListening() bool {
	s.listenerMu.RLock()
	defer s.listenerMu.RUnlock()
	if !s.listenerServing {
		return false
	}
	s.statusMu.Lock()
	s.ready = true
	s.readyAt = time.Now()
	s.statusMu.Unlock()
	s.logger.Info("dashboard marked ready")
	return true
}

const healthGracePeriod = 90 * time.Second

func (s *Server) inHealthGrace() bool {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return !s.readyAt.IsZero() && time.Since(s.readyAt) < healthGracePeriod
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if status == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "initializing"})
		return
	}
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// No CORS header — SSE is same-origin only.
	// The dashboard loads from the same host, so no cross-origin needed.

	ch := make(chan []byte, 16)
	s.sseMu.Lock()
	if len(s.sseClients) >= maxSSEClients {
		s.sseMu.Unlock()
		http.Error(w, "too many SSE connections", http.StatusServiceUnavailable)
		return
	}
	s.sseClients[ch] = struct{}{}
	s.sseMu.Unlock()

	defer func() {
		s.sseMu.Lock()
		delete(s.sseClients, ch)
		s.sseMu.Unlock()
	}()

	fmt.Fprintf(w, "retry: %d\n\n", sseRetryMs)
	flusher.Flush()

	s.statusMu.RLock()
	if s.status != nil {
		data, _ := json.Marshal(s.status)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	s.statusMu.RUnlock()

	for {
		select {
		case frame := <-ch:
			_, _ = w.Write(frame)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) broadcastFrame(frame string) {
	raw := []byte(frame)
	s.sseMu.Lock()
	defer s.sseMu.Unlock()

	for ch := range s.sseClients {
		select {
		case ch <- raw:
		default:
			s.logger.Warn("SSE client too slow, dropping event")
		}
	}
}

// initHistories lazily builds the three sparkline ring buffers. Called (once)
// by each history accessor so a zero-value Server — as used in tests that do
// `&Server{}` — works without a constructor. Token history has no throttle
// (append every broadcast); fact/cost throttle to ~5 min. Each buffer's cap and
// interval match the previous bespoke implementation exactly, so behavior and
// on-disk shape are preserved.
func (s *Server) initHistories() {
	s.histOnce.Do(func() {
		s.tokenHist = newTimeSeries(tokenSparklineMaxEntries, 0,
			func(e TokenSparklineEntry) int64 { return e.Timestamp })
		s.factHist = newTimeSeries(factHistoryMaxEntries, factHistoryMinIntervalMs,
			func(e FactHistoryEntry) int64 { return e.Timestamp })
		s.costHist = newTimeSeries(costHistoryMaxEntries, costHistoryMinIntervalMs,
			func(e CostHistoryEntry) int64 { return e.Timestamp })
	})
}

func (s *Server) tokenSeries() *timeSeries[TokenSparklineEntry] {
	s.initHistories()
	return s.tokenHist
}

func (s *Server) factSeries() *timeSeries[FactHistoryEntry] {
	s.initHistories()
	return s.factHist
}

func (s *Server) costSeries() *timeSeries[CostHistoryEntry] {
	s.initHistories()
	return s.costHist
}

// AppendTokenSparkline extracts token metrics from the current status and
// appends a timestamped entry to the token sparkline history (no throttle).
func (s *Server) AppendTokenSparkline(status *StatusPayload) {
	if status == nil {
		return
	}

	entry := TokenSparklineEntry{
		Timestamp:   nowMillis(),
		Input:       status.Tokens.Totals.Input,
		Output:      status.Tokens.Totals.Output,
		CacheRead:   status.Tokens.Totals.CacheRead,
		CacheCreate: status.Tokens.Totals.CacheCreate,
		Messages:    status.Tokens.Totals.Messages,
		ByAgent:     make(map[string]int64),
		ByModel:     make(map[string]int64),
	}

	for name, bucket := range status.Tokens.ByAgent {
		entry.ByAgent[name] = bucket.Input + bucket.Output + bucket.CacheRead
	}
	for name, bucket := range status.Tokens.ByModel {
		entry.ByModel[name] = bucket.Input + bucket.Output + bucket.CacheRead
	}

	s.tokenSeries().append(entry)
}

// TokenSparklineHistory returns a copy of the current token sparkline history.
func (s *Server) TokenSparklineHistory() []TokenSparklineEntry {
	return s.tokenSeries().snapshot()
}

// SeedTokenSparklineHistory restores persisted token history on startup.
func (s *Server) SeedTokenSparklineHistory(entries []TokenSparklineEntry) {
	s.tokenSeries().seed(entries)
}

// AppendFactHistory records a total-facts count if enough time has passed.
func (s *Server) AppendFactHistory(count int) {
	s.factSeries().append(FactHistoryEntry{
		Timestamp: nowMillis(),
		Count:     count,
	})
}

// FactHistory returns a copy of the fact count history.
func (s *Server) FactHistory() []FactHistoryEntry {
	return s.factSeries().snapshot()
}

// SeedFactHistory restores persisted fact history on startup.
func (s *Server) SeedFactHistory(entries []FactHistoryEntry) {
	s.factSeries().seed(entries)
}

// AppendCostHistory records an estimated-cost ($) snapshot if enough time has
// passed since the last one. Mirrors AppendFactHistory: same cadence throttle
// and same ring-buffer cap so the two histories stay aligned. The optional
// agents map carries per-agent cumulative $ so the UI can derive per-agent
// spend over a time window (agent cards); variadic to keep old callers valid.
func (s *Server) AppendCostHistory(usd float64, agents ...map[string]float64) {
	var a map[string]float64
	if len(agents) > 0 {
		a = agents[0]
	}
	s.AppendCostHistoryFull(usd, a, nil)
}

// AppendCostHistoryFull is AppendCostHistory plus the per-model snapshot map
// that feeds the cost table's mini sparklines.
func (s *Server) AppendCostHistoryFull(usd float64, agents map[string]float64, models map[string]CostModelSnap) {
	entry := CostHistoryEntry{
		Timestamp: nowMillis(),
		USD:       usd,
	}
	if len(agents) > 0 {
		entry.Agents = agents
	}
	if len(models) > 0 {
		entry.Models = models
	}
	s.costSeries().append(entry)
}

// CostHistory returns a copy of the estimated-cost history.
func (s *Server) CostHistory() []CostHistoryEntry {
	return s.costSeries().snapshot()
}

// SeedCostHistory restores persisted cost history on startup.
func (s *Server) SeedCostHistory(entries []CostHistoryEntry) {
	s.costSeries().seed(entries)
}

// AppendTrendHistory samples the governor / per-repo / beads / system-gauge
// trends from the current status and records a timestamped entry if enough time
// has passed since the last one. Mirrors AppendFactHistory / AppendCostHistory:
// same 5-min cadence throttle and same ring-buffer cap so all the persisted
// histories stay aligned. No-op on a nil status.
func (s *Server) AppendTrendHistory(status *StatusPayload) {
	if status == nil {
		return
	}
	now := time.Now().UnixMilli()

	s.trendHistoryMu.Lock()
	defer s.trendHistoryMu.Unlock()

	if len(s.trendHistory) > 0 {
		last := s.trendHistory[len(s.trendHistory)-1]
		if now-last.Timestamp < trendHistoryMinIntervalMs {
			return
		}
	}

	entry := TrendHistoryEntry{
		Timestamp:       now,
		GovIssues:       status.Governor.Issues,
		GovPrs:          status.Governor.PRs,
		GovTotal:        status.Governor.Issues + status.Governor.PRs,
		GovHold:         status.Hold.Total,
		BeadsWorkers:    status.Beads.Workers,
		BeadsSupervisor: status.Beads.Supervisor,
	}
	if len(status.Repos) > 0 {
		repos := make(map[string]TrendRepoSnap, len(status.Repos))
		for _, r := range status.Repos {
			repos[r.Name] = TrendRepoSnap{Issues: r.Issues, PRs: r.PRs}
		}
		entry.Repos = repos
	}
	if status.SystemResources != nil {
		entry.System = &TrendSystemSnap{
			DiskPct: status.SystemResources.DiskPct,
			MemPct:  status.SystemResources.MemPct,
			CpuPct:  status.SystemResources.CpuPct,
		}
	}

	s.trendHistory = append(s.trendHistory, entry)
	if len(s.trendHistory) > trendHistoryMaxEntries {
		s.trendHistory = s.trendHistory[len(s.trendHistory)-trendHistoryMaxEntries:]
	}
}

// TrendHistory returns a copy of the trend history.
func (s *Server) TrendHistory() []TrendHistoryEntry {
	s.trendHistoryMu.RLock()
	defer s.trendHistoryMu.RUnlock()
	out := make([]TrendHistoryEntry, len(s.trendHistory))
	copy(out, s.trendHistory)
	return out
}

// SeedTrendHistory restores persisted trend history on startup.
func (s *Server) SeedTrendHistory(entries []TrendHistoryEntry) {
	s.trendHistoryMu.Lock()
	defer s.trendHistoryMu.Unlock()
	if len(entries) > trendHistoryMaxEntries {
		entries = entries[len(entries)-trendHistoryMaxEntries:]
	}
	s.trendHistory = entries
}

// SetAdvisoryDigest stores the latest advisory digest for SSE broadcast.
func (s *Server) SetAdvisoryDigest(digest any) {
	s.advisoryMu.Lock()
	defer s.advisoryMu.Unlock()
	s.advisoryDigest = digest
}

// GetAdvisoryDigest returns the latest advisory digest.
func (s *Server) GetAdvisoryDigest() any {
	s.advisoryMu.RLock()
	defer s.advisoryMu.RUnlock()
	return s.advisoryDigest
}

// HealthSummary returns a deep-health summary with individual check results for heartbeats.
func (s *Server) HealthSummary() map[string]any {
	type check struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
	}
	checks := []check{}
	fails := 0
	warns := 0

	// 1. Readiness
	s.statusMu.RLock()
	ready := s.status != nil && s.ready
	s.statusMu.RUnlock()
	if ready {
		checks = append(checks, check{Name: "ready", Status: "pass"})
	} else {
		checks = append(checks, check{Name: "ready", Status: "fail", Detail: "not ready"})
		fails++
	}

	// 2. GitHub auth
	if s.deps != nil && s.deps.GHAppAuth != nil {
		if _, err := s.deps.GHAppAuth.Token(s.deps.Ctx); err != nil {
			checks = append(checks, check{Name: "github_auth", Status: "fail", Detail: "token error"})
			fails++
		} else {
			checks = append(checks, check{Name: "github_auth", Status: "pass"})
		}
	} else if s.deps != nil && s.deps.GHClient != nil {
		checks = append(checks, check{Name: "github_auth", Status: "pass", Detail: "token"})
	} else {
		checks = append(checks, check{Name: "github_auth", Status: "fail", Detail: "no auth"})
		fails++
	}

	// 3. Agents
	if s.deps != nil && s.deps.AgentMgr != nil {
		grace := s.inHealthGrace()
		const staleOutputThreshold = 30 * time.Minute
		running := 0
		paused := 0
		stalled := 0
		unsubstituted := 0
		down := 0
		for _, proc := range s.deps.AgentMgr.AllStatuses() {
			if proc.Paused {
				paused++
				continue
			}
			if proc.State == agent.StateRunning {
				running++
				if !grace && proc.LastKickMessage != "" {
					for _, v := range []string{"${ISSUE_LIST}", "${PR_LIST}", "${HIVE_REPO}", "${KNOWLEDGE}"} {
						if strings.Contains(proc.LastKickMessage, v) {
							unsubstituted++
							break
						}
					}
				}
				if !grace && proc.OutputBuffer != nil && proc.OutputBuffer.Count() == 0 && proc.LastKick != nil {
					if time.Since(*proc.LastKick) > staleOutputThreshold {
						stalled++
					}
				}
			} else if !grace {
				down++
			}
		}
		detail := fmt.Sprintf("%d running", running)
		if paused > 0 {
			detail += fmt.Sprintf(", %d paused", paused)
		}
		if down > 0 {
			detail += fmt.Sprintf(", %d down", down)
		}
		st := "pass"
		if down > 0 {
			st = "fail"
			fails++
		}
		checks = append(checks, check{Name: "agents", Status: st, Detail: detail})

		if stalled > 0 {
			checks = append(checks, check{Name: "stall_detection", Status: "warn", Detail: fmt.Sprintf("%d stalled (no output 30+ min)", stalled)})
			warns++
		} else {
			checks = append(checks, check{Name: "stall_detection", Status: "pass"})
		}

		if unsubstituted > 0 {
			checks = append(checks, check{Name: "template_vars", Status: "warn", Detail: fmt.Sprintf("%d with raw ${VARS}", unsubstituted)})
			warns++
		}

		refused := []string{}
		for name, proc := range s.deps.AgentMgr.AllStatuses() {
			if proc.KickRefused {
				refused = append(refused, name)
			}
		}
		if len(refused) > 0 {
			checks = append(checks, check{Name: "kick_refusal", Status: "warn", Detail: fmt.Sprintf("%s refused kick", strings.Join(refused, ", "))})
			warns++
		}
	}

	// 4. Governor
	if s.deps != nil && s.deps.Governor != nil {
		state := s.deps.Governor.GetState()
		checks = append(checks, check{Name: "governor", Status: "pass", Detail: string(state.Mode)})
	}

	// 5. Contribute
	if s.contributeHub != nil {
		active := s.contributeHub.ActiveCount()
		checks = append(checks, check{Name: "contribute", Status: "pass", Detail: fmt.Sprintf("%d active", active)})
	}

	// 6. Token consumption
	if s.deps != nil && s.deps.Tokens != nil {
		ts := s.deps.Tokens.Summary()
		if ts != nil {
			if ts.TotalTokens == 0 {
				checks = append(checks, check{Name: "tokens", Status: "warn", Detail: "zero consumed"})
				warns++
			} else {
				checks = append(checks, check{Name: "tokens", Status: "pass", Detail: fmt.Sprintf("%d total", ts.TotalTokens)})
			}
		}
	}

	// 7. Queue
	s.statusMu.RLock()
	if s.status != nil {
		total := 0
		for _, repo := range s.status.Repos {
			total += len(repo.ActionableIssues)
		}
		checks = append(checks, check{Name: "queue", Status: "pass", Detail: fmt.Sprintf("%d actionable", total)})
	}
	s.statusMu.RUnlock()

	overall := "ok"
	if fails > 2 {
		overall = "critical"
	} else if fails > 0 {
		overall = "degraded"
	} else if warns > 0 {
		overall = "warning"
	}

	return map[string]any{
		"status": overall,
		"fails":  fails,
		"warns":  warns,
		"checks": checks,
	}
}

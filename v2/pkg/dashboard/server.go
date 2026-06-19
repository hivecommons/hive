package dashboard

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/agent"
	"github.com/kubestellar/hive/v2/pkg/github"
)

//go:embed static
var staticFS embed.FS

func secureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

const agentSkipAfterFullBroadcastS = 5 * time.Second
const maxSSEClients = 100

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

	agentPipelines map[string]map[string]bool
	agentHooks     map[string]map[string][]any
	pipelineMu     sync.RWMutex
	hooksMu        sync.RWMutex
	knowledgeMu    sync.Mutex
	levelMu        sync.Mutex
	restartMu      sync.Mutex // serializes concurrent agent restart operations

	tokenHistoryMu    sync.RWMutex
	tokenHistory      []TokenSparklineEntry
	lastFullBroadcast time.Time

	advisoryMu     sync.RWMutex
	advisoryDigest any

	deviceFlowMu    sync.Mutex
	deviceFlowState *github.DeviceFlowState

	audit *AuditLog

	versionMu           sync.RWMutex
	cachedLatestHash    string
	cachedLatestMessage string
	cachedLatestAt      time.Time

	contributeHub *ContributeWSHub

	inferenceEndpoints map[string][]string // backend id → list of base URLs

	ready   bool
	readyAt time.Time

	githubAppMu         sync.RWMutex
	githubAppRequired   bool
	githubAppInstallURL string

	githubAppRecheckFn func() bool
}

// StatusPayload matches the JSON contract the dashboard frontend render() expects.
type StatusPayload struct {
	Timestamp     string              `json:"timestamp"`
	HiveID        string              `json:"hiveId"`
	Agents        []FrontendAgent     `json:"agents"`
	Governor      FrontendGovernor    `json:"governor"`
	Tokens        FrontendTokens      `json:"tokens"`
	Repos         []FrontendRepo      `json:"repos"`
	Beads         FrontendBeads       `json:"beads"`
	Health        map[string]any      `json:"health"`
	Budget        FrontendBudget      `json:"budget"`
	CadenceMatrix []FrontendCadence   `json:"cadenceMatrix"`
	GHRateLimits  map[string]any      `json:"ghRateLimits"`
	AgentMetrics  map[string]any      `json:"agentMetrics"`
	Hold          FrontendHold        `json:"hold"`
	IssueToMerge  map[string]any      `json:"issueToMerge"`
	ACMMLevel        int                 `json:"acmmLevel"`
	ACMMPackAgents   []string            `json:"acmmPackAgents"`
	AdvisoryDigest   any                 `json:"advisoryDigest,omitempty"`
	ContributorPool    *ContributorPoolStatus `json:"contributorPool,omitempty"`
	SystemResources     *SystemResources    `json:"systemResources,omitempty"`
	GitHubAppRequired   bool               `json:"githubAppRequired,omitempty"`
	GitHubAppInstallURL string             `json:"githubAppInstallURL,omitempty"`
	InferenceBackends   []InferenceBackend `json:"inferenceBackends,omitempty"`
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
}

type FrontendGovernor struct {
	Active     bool                    `json:"active"`
	Mode       string                  `json:"mode"`
	Issues     int                     `json:"issues"`
	PRs        int                     `json:"prs"`
	Thresholds FrontendThresholds      `json:"thresholds"`
	NextKick   string                  `json:"nextKick,omitempty"`
}

type FrontendThresholds struct {
	Quiet int `json:"quiet"`
	Busy  int `json:"busy"`
	Surge int `json:"surge"`
}

type FrontendTokens struct {
	LookbackHours  int                            `json:"lookbackHours"`
	Sessions       []FrontendSession              `json:"sessions"`
	Totals         FrontendTokenTotals             `json:"totals"`
	ByAgent        map[string]FrontendTokenBucket  `json:"byAgent"`
	ByModel        map[string]FrontendTokenBucket  `json:"byModel"`
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
	Name             string        `json:"name"`
	Full             string        `json:"full"`
	Issues           int           `json:"issues"`
	PRs              int           `json:"prs"`
	ActionableIssues []any         `json:"actionableIssues"`
	OpenPrs          []any         `json:"openPrs"`
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
	Timestamp    int64                       `json:"t"`
	Input        int64                       `json:"tokenInput"`
	Output       int64                       `json:"tokenOutput"`
	CacheRead    int64                       `json:"tokenCacheRead"`
	CacheCreate  int64                       `json:"tokenCacheCreate"`
	Messages     int                         `json:"tokenMessages"`
	ByAgent      map[string]int64            `json:"tokens,omitempty"`
	ByModel      map[string]int64            `json:"tokenModels,omitempty"`
}

// tokenSparklineMaxEntries caps the on-disk history to ~24h at 5-min intervals.
const tokenSparklineMaxEntries = 288

const sseRetryMs = 3000

func NewServer(port int, logger *slog.Logger) *Server {
	s := &Server{
		port:           port,
		sseClients:     make(map[chan []byte]struct{}),
		logger:         logger,
		mux:            http.NewServeMux(),
		agentPipelines: make(map[string]map[string]bool),
		agentHooks:     make(map[string]map[string][]any),
		audit:          newAuditLog(),
	}
	s.registerCoreRoutes()
	return s
}

func NewServerWithAuth(port int, authToken string, logger *slog.Logger) *Server {
	s := &Server{
		port:           port,
		authToken:      authToken,
		sseClients:     make(map[chan []byte]struct{}),
		logger:         logger,
		mux:            http.NewServeMux(),
		agentPipelines: make(map[string]map[string]bool),
		agentHooks:     make(map[string]map[string][]any),
		audit:          newAuditLog(),
	}
	s.registerCoreRoutes()
	return s
}

// SetInferenceEndpoints registers base URLs for inference backends
// so the dashboard can query them for available models at runtime.
func (s *Server) SetInferenceEndpoints(endpoints map[string][]string) {
	s.inferenceEndpoints = endpoints
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
	return backends
}

// SetSkipReloadFunc sets the callback used by saveConfig to skip the
// config watcher's next reload after a programmatic save. Call after
// the watcher is created but before it starts.
func (s *Server) SetSkipReloadFunc(fn func()) {
	if s.deps != nil {
		s.deps.SkipReloadFunc = fn
	}
}

func (s *Server) registerCoreRoutes() {
	s.mux.HandleFunc("GET /api/health", s.handleHealth)
	s.mux.HandleFunc("GET /api/health/deep", s.handleHealthDeep)
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/events", s.handleSSE)
	s.mux.HandleFunc("POST /api/github-app/recheck", s.handleGitHubAppRecheck)
}

func (s *Server) Start() error {
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("loading embedded static files: %w", err)
	}
	s.mux.Handle("GET /", http.FileServer(http.FS(staticContent)))

	handler := s.roleEnforcement(s.securityHeaders(s.mux))

	const dashboardReadTimeout = 30 * time.Second
	const dashboardIdleTimeout = 120 * time.Second
	addr := fmt.Sprintf(":%d", s.port)
	s.logger.Info("dashboard starting", "addr", addr)
	srv := &http.Server{
		Addr:        addr,
		Handler:     handler,
		ReadTimeout: dashboardReadTimeout,
		IdleTimeout: dashboardIdleTimeout,
	}
	return srv.ListenAndServe()
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self' ws: wss:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		if s.authToken != "" && strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/api/health") && r.URL.Path != "/api/auth/token" {
			trusted := secureCompare(r.Header.Get("X-Hive-Internal"), s.authToken)
			if !trusted && r.Header.Get("X-Hive-User") != "" && r.Header.Get("X-Hive-Role") != "" {
				// Trust nginx auth-url proxied requests that have both user
				// and role headers (set by the hub's auth endpoint). Requiring
				// both headers prevents trivial bypass via a single forged header.
				trusted = true
			}
			if !trusted {
				token := r.Header.Get("Authorization")
				if token == "" {
					token = r.URL.Query().Get("token")
				}
				expected := "Bearer " + s.authToken
				if !secureCompare(token, expected) && !secureCompare(token, s.authToken) {
					http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) Handler() http.Handler {
	return s.roleEnforcement(s.securityHeaders(s.mux))
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
				http.Error(w, `{"error":"read-only access"}`, http.StatusForbidden)
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
	}
	status.ContributorPool = s.BuildContributorPoolStatus()

	s.githubAppMu.RLock()
	status.GitHubAppRequired = s.githubAppRequired
	status.GitHubAppInstallURL = s.githubAppInstallURL
	s.githubAppMu.RUnlock()

	status.InferenceBackends = s.buildInferenceBackends()

	s.statusMu.Lock()
	status.Timestamp = time.Now().UTC().Format(time.RFC3339)
	s.status = status
	s.lastFullBroadcast = time.Now()
	s.statusMu.Unlock()

	s.AppendTokenSparkline(status)

	data, err := json.Marshal(status)
	if err != nil {
		s.logger.Warn("failed to marshal status for SSE", "error", err)
		return
	}

	s.broadcastFrame(fmt.Sprintf("data: %s\n\n", data))
}

const GitHubAppInstallURL = "https://github.com/apps/kubestellar-hive/installations/new"

func (s *Server) SetGitHubAppRequired(required bool) {
	s.githubAppMu.Lock()
	defer s.githubAppMu.Unlock()
	s.githubAppRequired = required
	if required {
		s.githubAppInstallURL = GitHubAppInstallURL
	} else {
		s.githubAppInstallURL = ""
	}
}

func (s *Server) IsGitHubAppRequired() bool {
	s.githubAppMu.RLock()
	defer s.githubAppMu.RUnlock()
	return s.githubAppRequired
}

func (s *Server) SetGitHubAppRecheckFn(fn func() bool) {
	s.githubAppRecheckFn = fn
}

func (s *Server) handleGitHubAppRecheck(w http.ResponseWriter, r *http.Request) {
	if s.githubAppRecheckFn == nil {
		http.Error(w, "recheck not configured", http.StatusNotImplemented)
		return
	}
	ok := s.githubAppRecheckFn()
	w.Header().Set("Content-Type", "application/json")
	if ok {
		s.SetGitHubAppRequired(false)
		w.Write([]byte(`{"status":"installed"}`))
	} else {
		w.Write([]byte(`{"status":"not_installed"}`))
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
			"status":             "pass",
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
				"status":  "warn",
				"detail":  "agents kicked but no output for 30+ min",
				"agents":  stalled,
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

// AppendTokenSparkline extracts token metrics from the current status and
// appends a timestamped entry to the in-memory token sparkline history.
func (s *Server) AppendTokenSparkline(status *StatusPayload) {
	if status == nil {
		return
	}

	entry := TokenSparklineEntry{
		Timestamp:   time.Now().UnixMilli(),
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

	s.tokenHistoryMu.Lock()
	s.tokenHistory = append(s.tokenHistory, entry)
	if len(s.tokenHistory) > tokenSparklineMaxEntries {
		s.tokenHistory = s.tokenHistory[len(s.tokenHistory)-tokenSparklineMaxEntries:]
	}
	s.tokenHistoryMu.Unlock()
}

// TokenSparklineHistory returns a copy of the current token sparkline history.
func (s *Server) TokenSparklineHistory() []TokenSparklineEntry {
	s.tokenHistoryMu.RLock()
	defer s.tokenHistoryMu.RUnlock()
	out := make([]TokenSparklineEntry, len(s.tokenHistory))
	copy(out, s.tokenHistory)
	return out
}

// SeedTokenSparklineHistory restores persisted token history on startup.
func (s *Server) SeedTokenSparklineHistory(entries []TokenSparklineEntry) {
	s.tokenHistoryMu.Lock()
	defer s.tokenHistoryMu.Unlock()
	if len(entries) > tokenSparklineMaxEntries {
		entries = entries[len(entries)-tokenSparklineMaxEntries:]
	}
	s.tokenHistory = entries
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

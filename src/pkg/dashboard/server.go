package dashboard

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/acmmadvisor"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/openrouter"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/watchdog"
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

// terminalAssertionCookieName carries the short-lived, HMAC-signed
// {user,hive,role,exp} assertion the Node proxy verifies as the PRIMARY per-hive
// terminal gate (finding C3 follow-up). It is Path=/terminal-scoped and NOT
// domain-widened to .hive.kubestellar.io (unlike the hub-wide hive_hub_user
// cookie), so the browser only ever sends it to THIS hive's own terminal path.
const terminalAssertionCookieName = "hive_terminal_assertion"

// proxyAuthHeader is the proof-of-proxy header the hub's auth-check injects
// (value = this hive's dashboard token) so a hub-proxied spoke can verify a
// request actually transited the hub nginx rather than being forged on the pod
// network. See authenticate() (F2). Must match the hub's constant.
const proxyAuthHeader = "X-Hive-Proxy-Auth"

// ownerRoleVerifiedHeader is a server-only request marker set by authenticate
// when the owner role came from a trusted source, not an inbound client header.
const ownerRoleVerifiedHeader = "X-Hive-Owner-Role-Verified"

func isOwnerRole(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), config.RoleOwner)
}

// proxyProofRequired controls F2 enforcement strictness. Default true =
// fail-closed: a request with no X-Hive-Proxy-Auth proof header (or a wrong
// one) is rejected even if it carries X-Hive-User/X-Hive-Role identity.
// A WRONG proof header is ALWAYS rejected regardless of this setting.
var proxyProofRequired = true

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
	// acmmLinearBaseURL overrides the Linear GraphQL endpoint the ACMM
	// "Open Issue" path posts issueCreate to. Empty = production; tests
	// point it at an httptest server.
	acmmLinearBaseURL string

	// snapshotDir overrides the directory handleSnapshotPage/buildSnapshot
	// read and write snapshot-{mode}.html under (#5235). Empty = production
	// default "/data/snapshots"; tests point it at t.TempDir() so the
	// stale-threshold rebuild decision can be exercised against real mtimes
	// without touching the host filesystem. Read via s.snapshotDirOrDefault().
	snapshotDir string
	// buildSnapshotFn, if non-nil, replaces the Node builder invocation in
	// buildSnapshot (#5235) — the same nil-in-production hook convention as
	// pkg/hub's afterGenerationsReadAttempt (#5080). Production leaves this
	// nil, in which case buildSnapshot runs the real `node
	// build-snapshot.mjs` subprocess; tests set it to a fake that writes a
	// fixture file instead, so the CSP-stamping/rewrite pipeline in
	// handleSnapshotPage can be exercised without a Node toolchain on the
	// test host.
	buildSnapshotFn func(s *Server, outputFile, mode string)

	// Sparkline histories, all backed by the generic timeSeries ring buffer
	// (see timeseries.go). Lazily constructed via the tokenSeries()/factSeries()
	// /costSeries() accessors so the zero-value Server needs no constructor
	// change. Each keeps its own typed entry struct and JSON contract, so the
	// unification is internal — on-disk files and /api/* endpoints are unchanged.
	histOnce  sync.Once
	tokenHist *timeSeries[TokenSparklineEntry]
	factHist  *timeSeries[FactHistoryEntry]
	costHist  *timeSeries[CostHistoryEntry]
	// budgetWindowHist records one row per CLOSED budget window (#4298). Lazily
	// built like the sparkline rings so a zero-value Server works in tests.
	budgetWindowOnce sync.Once
	budgetWindowHist *budgetWindowTracker

	// convergenceModeTrk captures one (mode, generation) pair per enrolled
	// eval pass and detects transitions (#4263). convergenceSoakTrk records the
	// bounded per-pass soak telemetry. Both lazily built like the rings above.
	convergenceModeOnce sync.Once
	convergenceModeTrk  *convergenceModeTracker
	convergenceSoakOnce sync.Once
	convergenceSoakTrk  *convergenceSoakTracker

	// lastFullBroadcast is guarded by statusMu (set/read alongside s.status).
	lastFullBroadcast time.Time

	// statusSeq / statusMutationEpoch power the stale-snapshot guard (#4348).
	// statusSeq increments on every published full snapshot and travels in the
	// payload so the frontend can drop out-of-order status responses.
	// statusMutationEpoch increments on every state mutation; a snapshot
	// rebuild that STARTED before the latest mutation is dropped at publish
	// time, so no published snapshot can revert a mutation the operator has
	// already been told succeeded. Both guarded by statusMu.
	statusSeq           uint64
	statusMutationEpoch uint64

	// fact/cost histories were migrated to the generic timeSeries store (#2041);
	// the trend history (#2039) remains a dedicated buffer for now.
	trendHistoryMu sync.RWMutex
	trendHistory   []TrendHistoryEntry

	advisoryMu     sync.RWMutex
	advisoryDigest any
	// advisoryLastPostedAt / advisoryLastFindings / advisoryLastError record the
	// outcome of the most recent advisory-digest post ATTEMPT, so the heartbeat
	// builder can report it to the hub and a hive whose digest has quietly gone
	// stale (working App, advisory agents, but the digest stopped updating)
	// becomes visible in My Hives instead of needing a per-hive log sweep.
	//
	// advisoryLastPostedAt stays zero until the spoke SUCCESSFULLY posts a
	// digest at least once. That zero is what makes "this hive is not in the
	// advisory-posting business" (pure PR/merge mode, no advisory agents — it
	// never reaches the post path) indistinguishable on the wire from "old
	// spoke": both report an empty timestamp, which the hub must read as UNKNOWN
	// and never as a stale alarm. Only a hive that HAS posted at least once, and
	// then stopped, can trip the hub's staleness gate.
	//
	// advisoryLastError holds the log-safe error string from the most recent
	// FAILED post attempt (403 issues:write, rate limit, auth failure), or "" on
	// success. It is set from the same error the spoke already logs, so it never
	// carries key material.
	advisoryLastPostedAt time.Time
	advisoryLastFindings int
	// advisoryLastOverflow is how many findings the top-N cap held BACK from the
	// digest that was last posted (0 when nothing was capped). It rides to the
	// hub alongside the finding count so My Hives can say "12 findings (top 10)"
	// rather than implying the ten shown are all there are.
	advisoryLastOverflow int
	advisoryLastError    string

	// decomposeKickerOverride is a test-only seam for the Phase 4 plan-from-issue
	// decompose handoff; production leaves it nil and uses deps.AgentMgr.
	decomposeKickerOverride planning.DecomposeKicker

	deviceFlowMu    sync.Mutex
	deviceFlowState *github.DeviceFlowState
	// deviceFlowID binds the in-progress device flow to the caller who started
	// it: /start returns it, /poll must present it (both routes are public, and
	// the session cookie is minted on the poll response — without this secret
	// any anonymous poller could race the operator and steal the session).
	deviceFlowID string

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

	// Linear agent integration (RFC #4492 Part 2): lazily-built service
	// bundling the install store, control-plane client, webhook receiver, and
	// session responder. See api_linear_agent.go.
	linearAgentOnce sync.Once
	linearAgentSvc  *linearAgentService

	audit *AuditLog

	// presenceEngagedAt maps a username to the last time their browser
	// reported ENGAGED presence (tab visible + recent input; see presence.go).
	// Lazily initialized under presenceMu, freshness-pruned on read.
	presenceMu        sync.Mutex
	presenceEngagedAt map[string]time.Time

	// promptHistory stores the fully-expanded kick prompts delivered to each
	// agent, so an owner can review what their agents were actually told.
	promptHistory *PromptHistory

	versionMu           sync.RWMutex
	cachedLatestHash    string
	cachedLatestMessage string
	cachedLatestAt      time.Time
	cachedStableV4Hash  string
	cachedStableV4At    time.Time
	commitBehindCache   map[string]int

	contributeHub *ContributeWSHub

	// contributeMetrics holds the persistent hourly time-series behind the
	// Operations + Leaderboard sparklines (queue depth, tasks/hour, fleet size,
	// per-user completions). Lazily built via contributeMetricsStore() so the
	// zero-value Server needs no constructor change; the rollup goroutine is
	// started by StartContributeMetrics(ctx). See contribute_metrics.go.
	contributeMetricsOnce sync.Once
	contributeMetrics     *metricsStore

	// contributePRLink is the live, best-effort PR→issue link projection behind the
	// Operations triage view + queue PR badges (#2612 part c). It memoises "does a
	// Fixes/Closes PR exist for owner/repo#number, open or merged?" behind a short
	// TTL over the hive's existing GitHub client — NO new persistent store. Lazily
	// built via contributePRLinkResolver(); see contribute_prlink.go.
	contributePRLinkOnce sync.Once
	contributePRLink     *prLinkResolver

	inferenceMu        sync.RWMutex
	inferenceEndpoints map[string][]string // backend id → list of base URLs

	// cliModels caches best-effort runtime model discovery for the CLI
	// backends (copilot/claude/gemini/goose/codex/agy), each with its own discovery
	// source and static fallback. See cli_models.go.
	cliModels *cliModelCache

	ready   bool
	readyAt time.Time

	githubAppMu         sync.RWMutex
	githubConfigMu      sync.Mutex
	githubAppRequired   bool
	githubAppInstallURL string
	githubAppPermIssue  string // non-empty when app is installed but lacks required permissions
	// githubAppState is the classified reason App auth is failing (a
	// github.AppAuthState wire token: "key-missing", "not-installed", …).
	// It exists so the UI can tell an OPERATOR-side failure (the hive's key
	// was never delivered, or belongs to a different App) apart from a
	// USER-side one (App not installed, wrong installation_id) and stop
	// blaming users for problems only an admin can fix. Empty means the
	// state was never classified.
	githubAppState            string
	pendingGitHubAppInstall   bool
	pendingGitHubAppInstallAt time.Time

	systemAlertsMu sync.RWMutex
	systemAlerts   []SystemAlert

	hubBannerMu sync.RWMutex
	hubBanner   *HubBannerState

	githubAppRecheckFn func() bool

	// forgeAppInventoryFn supplies the Forge App tab's key inventory from
	// cmd/hive (resolved active key path + per-app-id PVC keys, fingerprints
	// only). Set once at startup via SetForgeAppInventoryFn; nil in tests and
	// early boot, which the handler tolerates.
	forgeAppInventoryFn func() ForgeAppInventory
}

// StatusPayload matches the JSON contract the dashboard frontend render() expects.
type StatusPayload struct {
	Timestamp string `json:"timestamp"`
	// StatusSeq is a monotonic publish sequence (#4348): the frontend drops
	// any status payload whose seq is older than the last one it rendered,
	// so a stale in-flight poll/SSE response can never repaint over a newer
	// snapshot (e.g. reverting a just-reset restart counter).
	StatusSeq uint64 `json:"statusSeq"`
	// StatusInstance identifies the server process that produced the seq.
	// Seqs restart at 1 when the spoke restarts; the frontend resets its
	// guard counters when the instance changes instead of dropping forever.
	StatusInstance   string                    `json:"statusInstance"`
	HiveID           string                    `json:"hiveId"`
	Agents           []FrontendAgent           `json:"agents"`
	ConfiguredAgents []FrontendConfiguredAgent `json:"configuredAgents"`
	Governor         FrontendGovernor          `json:"governor"`
	Tokens           FrontendTokens            `json:"tokens"`
	Repos            []FrontendRepo            `json:"repos"`
	Beads            FrontendBeads             `json:"beads"`
	Planning         FrontendPlanning          `json:"planning"`
	Health           map[string]any            `json:"health"`
	// DeepHealth carries the spoke's own deep health checks (HealthSummary:
	// ready, github_auth, agents, …) — the same checks the heartbeat reports
	// to the hub. The dashboard's header Health pill renders from these, NOT
	// from the shallow /api/health liveness signal or the repo-workflow
	// Health map above, so the pill can never show "Health OK" while the
	// spoke's own agents are down (#2465).
	DeepHealth          map[string]any    `json:"deepHealth,omitempty"`
	Budget              FrontendBudget    `json:"budget"`
	CadenceMatrix       []FrontendCadence `json:"cadenceMatrix"`
	GHRateLimits        map[string]any    `json:"ghRateLimits"`
	AgentMetrics        map[string]any    `json:"agentMetrics"`
	Hold                FrontendHold      `json:"hold"`
	IssueToMerge        map[string]any    `json:"issueToMerge"`
	ACMMLevel           int               `json:"acmmLevel"`
	ACMMLevelConfigured bool              `json:"acmmLevelConfigured"`
	ACMMPackAgents      []string          `json:"acmmPackAgents"`
	// ACMMAdvice is the advisory level-up recommendation (#5225), derived from
	// the same live signals the /api/acmm-recommendation endpoint serves so the
	// two surfaces can never drift. It is ADVISORY ONLY: nothing acts on it
	// automatically — a human approves a level change via handlePackSetLevel.
	// Omitted when it could not be computed (e.g. no config yet).
	ACMMAdvice          *acmmadvisor.Recommendation `json:"acmmAdvice,omitempty"`
	AdvisoryDigest      any                         `json:"advisoryDigest,omitempty"`
	ContributorPool     *ContributorPoolStatus      `json:"contributorPool,omitempty"`
	SystemResources     *SystemResources            `json:"systemResources,omitempty"`
	GitHubAppRequired   bool                        `json:"githubAppRequired,omitempty"`
	GitHubAppInstallURL string                      `json:"githubAppInstallURL,omitempty"`
	GitHubAppPermIssue  string                      `json:"githubAppPermIssue,omitempty"`
	GitHubAppState      string                      `json:"githubAppState,omitempty"`
	// GitHubAppInstallMissing is CONFIG TRUTH, independent of any auth probe
	// or classification: a real App is named (app_id set, not the placeholder)
	// but installation_id is 0. That state alone means every token is a
	// countdown, so the install banner keys off this field FIRST — no recheck,
	// classification, or raw-URL sniffing may gate it (the vllmd-13 reset left
	// blank raw fields suppressing the banner while auth was not-installed).
	GitHubAppInstallMissing bool               `json:"githubAppInstallMissing,omitempty"`
	RepoTargetMisconfigured bool               `json:"repoTargetMisconfigured,omitempty"`
	RepoTargetIssue         string             `json:"repoTargetIssue,omitempty"`
	GitHubBaseURL           string             `json:"githubBaseURL,omitempty"`
	InferenceBackends       []InferenceBackend `json:"inferenceBackends,omitempty"`
	SystemAlerts            []SystemAlert      `json:"systemAlerts,omitempty"`
	HubBanner               *HubBannerState    `json:"hubBanner,omitempty"`
	// Platform surfaces the v4 spoke capabilities — the configured forge, the
	// mint token service state, and the skills registry. It is additive and
	// nil-safe: a github-only, mint-off hive with no skills dir still gets a
	// populated (all-zero) block, so existing status output is unchanged.
	Platform *FrontendPlatform `json:"platform,omitempty"`
	Security *FrontendSecurity `json:"security,omitempty"`
}

// FrontendSecurity summarizes the effective operator security posture for compact dashboard display.
type FrontendSecurity struct {
	IntentEnforced        bool   `json:"intentEnforced"`
	IoscanEnabled         bool   `json:"ioscanEnabled"`
	IoscanFailMode        string `json:"ioscanFailMode"`
	IoscanCanaries        bool   `json:"ioscanCanaries"`
	ReviewRequireApproval bool   `json:"reviewRequireApproval"`
	ReviewFanOut          bool   `json:"reviewFanOut"`
	ReviewCapableAgents   int    `json:"reviewCapableAgents"`
	RetroEnabled          bool   `json:"retroEnabled"`
	OTelEnabled           bool   `json:"otelEnabled"`
	SandboxEnabled        bool   `json:"sandboxEnabled"`
	SandboxedAgents       int    `json:"sandboxedAgents"`
	TotalAgents           int    `json:"totalAgents"`
}

// FrontendPlatform reports the v4 spoke capabilities (forge, mint, skills) for
// the dashboard Platform card. Every field is honest-empty when unconfigured;
// building it never panics on a nil or zero-value config.
type FrontendPlatform struct {
	Forge  FrontendForge  `json:"forge"`
	Mint   FrontendMint   `json:"mint"`
	Skills FrontendSkills `json:"skills"`
}

// FrontendForge describes the configured source forge. Kind is one of
// "github" | "gitlab" | "gitea". InstanceURL is set only for a non-default
// self-managed instance (empty for github.com / gitlab.com defaults).
type FrontendForge struct {
	Kind        string   `json:"kind"`
	InstanceURL string   `json:"instanceUrl,omitempty"`
	PrimaryRepo string   `json:"primaryRepo,omitempty"`
	Repos       []string `json:"repos"`
	RepoCount   int      `json:"repoCount"`
}

// FrontendMint reports the mint token-service state. No secret or key material
// is ever exposed — only whether it is enabled, its issuer, and whether the
// signing key file is present on disk.
type FrontendMint struct {
	Enabled    bool   `json:"enabled"`
	Issuer     string `json:"issuer,omitempty"`
	KeyPresent bool   `json:"keyPresent"`
}

// FrontendSkills reports the skills registry. Available is false with Loaded=0
// when no skills directory is configured/derivable, so the UI can honestly show
// "not configured" rather than a fabricated count.
type FrontendSkills struct {
	Available bool   `json:"available"`
	Loaded    int    `json:"loaded"`
	Dir       string `json:"dir,omitempty"`
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
	// ModelsFallback is true when Models is NOT an authoritative census of
	// what the backend offers: the static alias list substituted because
	// live /v1/models discovery failed (endpoint down, 403, etc), a PARTIAL
	// sweep in which some endpoints answered and others did not, or the
	// HIVE_*_MODELS env list standing in for a registered endpoint that
	// failed to answer. The UI must not treat any of these as evidence that
	// models were actually added or removed (#4426, #4438) — diffing against
	// one is what produced the "Model removed from litellm: …" toast storms.
	ModelsFallback bool `json:"models_fallback,omitempty"`
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
	ReplicaBase      string `json:"replicaBase,omitempty"`
	ReplicaIndex     int    `json:"replicaIndex,omitempty"`
	ReplicaCount     int    `json:"replicaCount,omitempty"`
	Session          string `json:"session"`
	State            string `json:"state"`
	Busy             string `json:"busy"`
	Paused           bool   `json:"paused"`
	PausedAt         string `json:"pausedAt,omitempty"`
	PausedReason     string `json:"pausedReason,omitempty"`
	PausedTrigger    string `json:"pausedTrigger,omitempty"`
	PausedBy         string `json:"pausedBy,omitempty"`
	OffByCadence     bool   `json:"offByCadence"`
	NoCadence        bool   `json:"noCadence"`
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
	NextKickIn       string `json:"nextKickIn,omitempty"`
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
	Sandboxed        bool   `json:"sandboxed,omitempty"`
	LastError        string `json:"lastError,omitempty"`
	StallNudges      int    `json:"stallNudges,omitempty"`
	ActionNudges     int    `json:"actionNudges,omitempty"`
	TransientNudges  int    `json:"transientNudges,omitempty"`
	// Conditions is the watchdog reconciler's observed-truth condition set
	// (Ready/Authenticated/Producing — RFC #4665), replacing trust in the
	// State config echo. Empty until the watchdog's first sweep.
	Conditions []watchdog.Condition `json:"conditions,omitempty"`
	// WatchdogMode is the authority the watchdog was running under when it
	// published Conditions. The UI needs it to say whether a condition implied
	// an action that was actually TAKEN: in observe mode a "crash-looping"
	// verdict means the agent was NOT paused, and a reader must not conclude
	// otherwise.
	WatchdogMode string `json:"watchdogMode,omitempty"`
	// Enabled mirrors the CONFIG flag, not a runtime state, and is the only
	// thing that distinguishes "switched off on purpose" from "stopped because
	// something broke" — State reads "stopped" for both.
	//
	// A disabled agent reaches this payload at all because ApplyPack adds every
	// pack agent to the runtime roster and gates only the Start on Enabled
	// (api_packs.go), which is deliberate: the card is how an operator turns
	// the agent on. But without this field the card cannot say so, and it
	// offers a terminal and a full log for an agent that has never had a tmux
	// session — a dead end that reports "no tmux socket found for session
	// hive-<name>".
	//
	// NOT omitempty: false is the meaningful value here, and omitempty would
	// erase exactly the case this exists to report.
	Enabled bool `json:"enabled"`
}

// FrontendConfiguredAgent is the secret-free config inventory used by the
// dashboard to surface agents that have no runtime process (notably agents
// configured with enabled: false). Runtime details remain in FrontendAgent.
type FrontendConfiguredAgent struct {
	Name         string `json:"name"`
	DisplayName  string `json:"displayName,omitempty"`
	Description  string `json:"description,omitempty"`
	SortOrder    int    `json:"sortOrder"`
	Emoji        string `json:"emoji,omitempty"`
	Color        string `json:"color,omitempty"`
	Enabled      bool   `json:"enabled"`
	ModelOwner   string `json:"modelOwner,omitempty"`
	BackendOwner string `json:"backendOwner,omitempty"`
	// Mode/ModeEmoji are resolved the same way buildAgents resolves them for a
	// running agent: the agent's own Mode override if set, else the ACMM level
	// default. A disabled agent has no runtime entry, so without these the
	// sidebar cannot say what the agent WOULD do once enabled — which is the
	// one thing an operator wants to check before enabling it.
	Mode      string `json:"mode,omitempty"`
	ModeEmoji string `json:"modeEmoji,omitempty"`
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
	// ByAgentModel maps an agent name to the RESOLVED model of its most
	// recently active session. An agent whose config model is a routing alias
	// ("auto"/"default") resolves at runtime to a concrete model (e.g. copilot
	// "auto" → gpt-5.3-codex); the resolved id is parsed from session logs into
	// SessionSummary.Model. The frontend uses this so an auto-mode agent's
	// "running agents" badge lands on the resolved-model row where its tokens
	// actually accrue, instead of showing nothing on that row.
	ByAgentModel map[string]string `json:"byAgentModel"`
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

// FrontendPlanning is the governor-facing PLANNING metric block (Phase 2
// planning intelligence). It is computed from bead metadata (parent_epic +
// plan_status) across all bead stores.
type FrontendPlanning struct {
	// Available is true only when planning is usable at the current ACMM level
	// (>= 5, where the architect that decomposes plans is scheduled). The
	// governor PLANNING tile renders only when this is true, so it never shows a
	// misleading "0 plans" at levels where planning can't run at all.
	Available bool `json:"available"`
	// ActivePlans counts epics that have been decomposed (carry a plan_status),
	// i.e. plans currently in flight (draft or approved).
	ActivePlans int `json:"active_plans"`
	// AwaitingReview counts epics whose plan_status is draft — the
	// human-action-required state the governor tile highlights.
	AwaitingReview int `json:"awaiting_review"`
	// Decomposing counts approved plans that still have open (unfinished)
	// children — plans actively being executed by agents.
	Decomposing int `json:"decomposing"`
	// Replans24h counts plans re-decomposed in the last 24h by the Phase 3
	// governor stall-replan loop, derived from each epic's last_replan_at
	// metadata.
	Replans24h int `json:"replans_24h"`
	// PendingDecompose counts epics minted from an issue (Phase 4) that are
	// accepted but not yet decomposed by the architect (decompose_pending). While
	// >0 there is planning work queued for the architect.
	PendingDecompose int `json:"pending_decompose"`
	// ArchitectPaused is true when >=1 plan is pending AND the architect is paused,
	// so nothing will be built until the operator resumes it. The tile shows the
	// "architect is paused" message when this is set.
	ArchitectPaused bool `json:"architect_paused"`
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
	// WindowHoursRemaining is wall-clock time until the current budget period
	// resets. HoursRemaining above is instead how long the remaining token
	// allowance would last at the observed burn rate.
	WindowHoursRemaining float64 `json:"WINDOW_HOURS_REMAINING"`
	LastUpdated          string  `json:"LAST_UPDATED"`
	// Exhausted is true when the weekly limit is set and window spend has
	// reached it — the governor is suppressing kicks for non-exempt agents.
	Exhausted bool `json:"BUDGET_EXHAUSTED"`
	// WindowEndsAt is when the current budget window rolls (RFC3339);
	// empty unless a weekly limit is set and a window is open.
	WindowEndsAt string `json:"WINDOW_ENDS_AT"`
	// WindowStartsAt is when the current window opened (RFC3339), i.e. the last
	// reset. Additive (#4298): it is what lets a usage graph mark where the
	// resets fell, and what bounds each row of the per-window history. Empty
	// under exactly the same conditions as WindowEndsAt.
	WindowStartsAt string `json:"WINDOW_STARTS_AT,omitempty"`
}

type FrontendCadence struct {
	Agent      string `json:"agent"`
	Idle       string `json:"idle"`
	Quiet      string `json:"quiet"`
	Busy       string `json:"busy"`
	Surge      string `json:"surge"`
	IdleTitle  string `json:"idleTitle,omitempty"`
	QuietTitle string `json:"quietTitle,omitempty"`
	BusyTitle  string `json:"busyTitle,omitempty"`
	SurgeTitle string `json:"surgeTitle,omitempty"`
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
		port:           port,
		sseClients:     make(map[chan []byte]struct{}),
		logger:         logger,
		mux:            http.NewServeMux(),
		agentPipelines: make(map[string]map[string]bool),
		agentHooks:     make(map[string]map[string][]any),
		audit:          newAuditLog(),
		promptHistory:  newPromptHistory(),
		userSessions:   make(map[string]*userSession),
		cliModels:      newCLIModelCache(),
		startedAt:      time.Now(),
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
		promptHistory:  newPromptHistory(),
		userSessions:   make(map[string]*userSession),
		cliModels:      newCLIModelCache(),
		startedAt:      time.Now(),
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
		models, fallback := s.queryInferenceModelsDetailed(b.id)
		backends = append(backends, InferenceBackend{
			ID: b.id, Name: b.name, Inference: true, Models: models, ModelsFallback: fallback,
		})
	}
	// litellm has no in-cluster default — include it only when an endpoint
	// is registered, so an unconfigured backend isn't SSE-pushed empty.
	if endpoints, ok := s.getInferenceEndpoints("litellm"); ok && len(endpoints) > 0 {
		models, fallback := s.queryInferenceModelsDetailed("litellm")
		backends = append(backends, InferenceBackend{
			ID: "litellm", Name: "LiteLLM (proxy)", Inference: true,
			Models: models, ModelsFallback: fallback,
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
	s.mux.HandleFunc("GET /api/livez", s.handleLivez)
	// Prometheus scrape endpoint for estimated LLM cost — opt-in via
	// HIVE_METRICS_ENABLED, and the handler additionally requires the
	// HIVE_METRICS_TOKEN bearer token (fails closed with 403 when the token is
	// unset, #3785) since the series expose cost/agent data. See isPublicPath.
	if metricsEnabled() {
		s.mux.HandleFunc("GET /metrics", s.handleMetrics)
		if metricsToken() == "" && s.logger != nil {
			s.logger.Warn("HIVE_METRICS_ENABLED is set but HIVE_METRICS_TOKEN is not; /metrics will refuse every request (403) until a token is configured — set HIVE_METRICS_TOKEN and the scraper's bearer_token to match")
		}
	}
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/events", s.handleSSE)
	s.mux.HandleFunc("POST /api/github-app/recheck", s.handleGitHubAppRecheck)
	s.mux.HandleFunc("POST /api/github-app/install-clicked", s.handleGitHubAppInstallClicked)
	s.mux.HandleFunc("GET /gh-setup", s.handleGitHubAppSetupCallback)
	// SSO handoff: exchange a hub-minted, HMAC-signed token for a local session
	// so a hub-authenticated user opens this (direct-route) spoke without a
	// second GitHub device-flow login. Public path (see isPublicPath) because
	// the caller has no session yet — the token IS the credential.
	s.mux.HandleFunc("GET /sso", s.handleSSO)
	// Terminal-assertion renewal (audit F4 residual). Re-mints the host-only,
	// Path=/terminal assertion cookie from the caller's SPOKE-LOCAL session, so a
	// live session can sustain a live terminal grant without the domain-scoped
	// hive_hub_user cookie being consulted. NOT a public path: it authenticates
	// on hive_session and 401s without one.
	s.mux.HandleFunc("POST "+renewTerminalAssertionPath, s.handleRenewTerminalAssertion)
	// /terminal → in-container ttyd, so the dashboard's "▶ terminal" links
	// work even when the cluster route sends the whole host to this server
	// (see registerTerminalProxy).
	s.registerTerminalProxy()
}

func (s *Server) Start() error {
	staticContent, err := fs.Sub(staticFS, "static")
	if err != nil {
		return fmt.Errorf("loading embedded static files: %w", err)
	}
	// The SPA document gets a dedicated handler with startup-precomputed gzip
	// and a strong ETag (see static_index.go): http.FileServer would serve the
	// ~1.3 MB inline document uncompressed with no cache validators (embed.FS
	// has a zero ModTime, so not even Last-Modified), forcing a full re-download
	// on every visit. "/{$}" matches the root path exactly; every other static
	// path falls through to the plain file server below.
	if rawIndex, err := fs.ReadFile(staticContent, "index.html"); err == nil {
		// Strings are baked in ONCE here, unlike custom.css which is read per
		// request: the document carries a precomputed gzip body and a strong
		// ETag, so its content cannot vary per request without discarding both.
		// Editing branding.json therefore needs a restart; editing the
		// stylesheet does not. That asymmetry is documented in branding.md.
		idx := newIndexDocument(rawIndex, s.loadBranding())
		// Hand the FINAL served bytes to the CSP layer explicitly, rather than
		// having newIndexDocument reach out and set global state: constructing a
		// document should not silently change the process-wide CSP, and a test
		// building a throwaway document must not shrink the real allowlist.
		setBrandedIndex(idx.raw)
		s.mux.Handle("GET /{$}", idx)
		s.mux.Handle("GET /index.html", idx)
	} else {
		s.logger.Warn("embedded index.html unavailable; falling back to plain file serving", "error", err)
	}
	// Operator branding override: an optional stylesheet on the data volume,
	// served at the path the index document links to. Lets a deployment carry
	// its own colours/logo without forking the embedded SPA or rebuilding the
	// image — the override is data, not code.
	//
	// Read per request (not cached at startup) so dropping a file in takes
	// effect on reload. It is a single small stylesheet on local disk; the
	// index document itself remains startup-precompressed.
	s.mux.HandleFunc("GET /branding/custom.css", s.handleBrandingCSS)

	s.mux.Handle("GET /", http.FileServer(http.FS(staticContent)))

	// authenticate is outermost so the identity headers it injects from a
	// per-user session are visible to roleEnforcement's read-only write-gate.
	handler := s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))

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
		frameAncestors := "'none'"
		snapshotAllowlist := false
		if r.URL.Path == "/snapshot" && s.deps != nil && s.deps.Config != nil && len(s.deps.Config.Dashboard.SnapshotFrameAncestors) > 0 {
			frameAncestors = s.deps.Config.Dashboard.SnapshotFrameAncestorsCSP()
			snapshotAllowlist = true
		}
		// X-Frame-Options cannot express an origin allowlist. When /snapshot has
		// an explicit CSP frame-ancestors allowlist, omit XFO for that document
		// only; every other route keeps DENY, and an empty allowlist fails closed.
		if !snapshotAllowlist {
			w.Header().Set("X-Frame-Options", "DENY")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		// SECURITY (#3315 → #3848 part 1, #3907): script-src is scoped into its
		// element and attribute halves, the same decomposition ADR-0015 applied
		// to style-src, because the same asymmetry decides it — see ADR-0016 and
		// csp_script_src.go for the full rationale:
		//
		//   script-src-elem 'self' 'sha256-…'  — inline <script> ELEMENTS.
		//     CLOSED. Every inline script this server sends is hash-allowlisted:
		//     the embedded SPA and the device-flow login page at startup
		//     (baseScriptSrcElem), the per-response documents (/contribute,
		//     /snapshot) by applyDocumentScriptSrcElem over the exact bytes
		//     served. No 'unsafe-inline': an injected inline <script> cannot
		//     match a hash and does not execute in any CSP3 browser. Hashes, not
		//     nonces, so the #3863 startup-pre-gzip + strong-ETag design for the
		//     SPA document is untouched.
		//
		//   script-src-attr 'none'  — inline on*= HANDLER attributes.
		//     CLOSED by the #3848 event-delegation refactor: every former on*=
		//     attribute in static/index.html and in Go-generated HTML now uses
		//     data-action / data-* attributes dispatched by central document
		//     listeners, so no markup-level handler attribute remains and an
		//     injected on*= attribute never executes.
		//     TestCSPScriptSrcAttrUnsafeInlineIsAbsent pins this closed.
		//
		//   script-src 'self'  — the CSP2 fallback, now also without
		//     'unsafe-inline' (nothing inline-attribute-based remains to allow),
		//     and it must NEVER carry the hashes: a hash in this directive makes
		//     hash-aware browsers ignore 'unsafe-inline' here, and a browser
		//     that knows hashes but not script-src-elem/-attr (Firefox < 108)
		//     would then block every inline handler and blank the dashboard.
		//
		// The secret that 'unsafe-inline' used to expose is gone regardless: the
		// dashboard token is never rendered into the page (see serveIndex in
		// proxy/server.js and handleAuthToken in api.go), so an inline-script
		// XSS has no token to steal from the served HTML. form-action 'self'
		// stops an injected <form> from posting credentials off-origin.
		//
		// /terminal is excluded from the split: those responses are ttyd's own
		// UI streamed through a reverse proxy, this server never sees the
		// document bytes to hash, and a wrong allowlist there would brick the
		// terminal. It keeps the blanket CSP2 policy it has always had.
		//
		// style-src (#3848 part 2) is scoped as TWO directives rather than one
		// blanket allowance, because the two halves have different futures:
		//
		//   style-src-attr 'unsafe-inline'  — the ~2,055 inline style="" attributes.
		//     ACCEPTED, permanently, and not a staging compromise (ADR-0015/0016):
		//     CSP has no nonce or hash form for attribute-level styles, so there
		//     is no policy that both allows them and constrains them, and CSS
		//     injection is not an XSS vector — an injected style attribute can
		//     deface the page but cannot execute script. Closing this
		//     would mean deleting every style attribute in the UI. See
		//     ADR-0015 for the rationale and the residual risk.
		//
		//   style-src-elem 'self' 'unsafe-inline' — the 7 inline <style> elements.
		//     CLOSABLE, unlike the attributes: <style> elements do take hashes.
		//     Left open today because tightening it while script-src still
		//     allows inline script buys nothing — anyone who can inject <style>
		//     can inject <script> — so it is sequenced AFTER script-src, where
		//     it starts to matter. TestCSPStyleSrcElemUnsafeInlineIsStaged is
		//     its tripwire, mirroring the script-src one.
		//
		// Both are listed AFTER the style-src fallback, which browsers without
		// CSP3 support use instead; the effective policy is identical either way,
		// so this split names the decision without changing behaviour.
		scriptDirectives := "script-src 'self'; script-src-elem " + baseScriptSrcElem() + "; script-src-attr 'none'"
		if r.URL.Path == "/terminal" || strings.HasPrefix(r.URL.Path, "/terminal/") {
			scriptDirectives = "script-src 'self' 'unsafe-inline'"
		}
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+scriptDirectives+"; style-src 'self' 'unsafe-inline'; style-src-elem 'self' 'unsafe-inline'; style-src-attr 'unsafe-inline'; img-src 'self' data: https:; font-src 'self' data:; connect-src 'self' ws: wss:; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors "+frameAncestors)
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
		inboundUser := r.Header.Get("X-Hive-User")
		inboundRole := r.Header.Get("X-Hive-Role")

		// Treat identity headers as an internal request attribute. Preserve inbound
		// values only after the request has proved it transited the trusted hub
		// proxy; this must happen before public-path dispatch because several public
		// contribute handlers consult X-Hive-User/X-Hive-Role for optional identity.
		r.Header.Del("X-Hive-User")
		r.Header.Del("X-Hive-Role")
		r.Header.Del(ownerRoleVerifiedHeader)

		trustProxyIdentity := func(markOwner bool) (bool, string) {
			if directRouteAuthz || inboundUser == "" || inboundRole == "" {
				return false, ""
			}
			proof := r.Header.Get(proxyAuthHeader)
			switch {
			case proof != "" && s.authToken != "" && secureCompare(proof, s.authToken):
				r.Header.Set("X-Hive-User", inboundUser)
				r.Header.Set("X-Hive-Role", inboundRole)
				if markOwner && isOwnerRole(inboundRole) {
					r.Header.Set(ownerRoleVerifiedHeader, "true")
				}
				return true, ""
			case proof == "" && !proxyProofRequired:
				r.Header.Set("X-Hive-User", inboundUser)
				r.Header.Set("X-Hive-Role", inboundRole)
				return true, ""
			case proof == "":
				return false, "missing proxy proof header"
			default:
				return false, "invalid proxy proof header"
			}
		}

		if isPublicPath(r.URL.Path) {
			// Public endpoints remain reachable anonymously, but identity headers are
			// visible to handlers only when backed by the hub's proxy proof.
			trustProxyIdentity(true)
			next.ServeHTTP(w, r)
			return
		}
		if s.authToken == "" && !directRouteAuthz {
			// Open by design — but still resolve a session if the caller has one,
			// so an SSO handoff on a token-less spoke yields a request that KNOWS
			// who the user is. Without this the handoff sets a cookie, the next
			// request is let through anonymously, the UI sees no identity and
			// sends the user back to log in — a bounce. This grants no extra
			// access: everyone is already admitted on this branch.
			if sess := s.sessionFromRequest(r); sess != nil {
				// Role comes from liveSessionRole, never sess.Role directly: a
				// Manage Access change must not stay frozen in a 30-day session.
				if role, ok := s.liveSessionRole(sess); ok {
					r.Header.Set("X-Hive-User", sess.Username)
					r.Header.Set("X-Hive-Role", role)
					if isOwnerRole(role) {
						r.Header.Set(ownerRoleVerifiedHeader, "true")
					}
				}
			} else if inboundRole != "" {
				// Open/dev spokes have no auth boundary; preserve an explicit role so
				// local role-gate tests and read-only demos can still exercise least
				// privilege. Public paths returned above with unproved identity stripped.
				if inboundUser != "" {
					r.Header.Set("X-Hive-User", inboundUser)
				}
				r.Header.Set("X-Hive-Role", inboundRole)
				if isOwnerRole(inboundRole) {
					r.Header.Set(ownerRoleVerifiedHeader, "true")
				}
			} else {
				r.Header.Set("X-Hive-Role", config.RoleOwner)
				r.Header.Set(ownerRoleVerifiedHeader, "true")
			}
			next.ServeHTTP(w, r)
			return
		}
		// Internal automation authenticates with the shared token via the
		// X-Hive-Internal header; this is a trusted server-to-server path
		// (the local proxy injects it) and carries no browser user identity.
		// Guard against an empty authToken: subtle.ConstantTimeCompare("","")
		// is TRUE, so without this an absent/empty header would authenticate on
		// a direct-route spoke that has no token. The shared-token paths are only
		// valid when a real token is configured.
		internalTrusted := s.authToken != "" && secureCompare(r.Header.Get("X-Hive-Internal"), s.authToken)
		trusted := internalTrusted
		if internalTrusted {
			if sess := s.sessionFromRequest(r); sess != nil {
				// Scope the shared-token request down to the session's user, at
				// their LIVE allowlist role (liveSessionRole): a hub-side grant,
				// downgrade, or revocation must take effect now, not when the
				// 30-day session happens to expire. A revoked user (ok=false)
				// gets no identity injected at all — possession of the internal
				// token still authenticates the request, but it must not carry
				// the revoked user's name or any role.
				if role, ok := s.liveSessionRole(sess); ok {
					r.Header.Set("X-Hive-User", sess.Username)
					r.Header.Set("X-Hive-Role", role)
					if isOwnerRole(role) {
						r.Header.Set(ownerRoleVerifiedHeader, "true")
					}
				}
			} else {
				// No per-user session to scope this request down: the shared
				// internal token IS the operator credential on this path (the
				// local gateway authenticates the browser with the same token,
				// strips client identity headers, then injects X-Hive-Internal).
				// Grant owner with the server-set verified marker so owner-only
				// mutations (budget save, pause, etc.) work for the operator.
				// Inbound identity headers were already stripped above, so this
				// cannot be reached by spoofing — only by presenting the secret,
				// which is owner-equivalent by definition. Without this, the F14
				// provenance hardening locked the real owner out of owner-gated
				// endpoints on every shared-token deployment (#4134).
				r.Header.Set("X-Hive-Role", config.RoleOwner)
				r.Header.Set(ownerRoleVerifiedHeader, "true")
			}
		}

		// Hub-proxied path: nginx injects the identity headers from the hub's
		// per-user/per-hive auth-check. Only trust them when this spoke is NOT a
		// direct-route spoke (see strip above).
		//
		// SECURITY (F2): identity headers alone are forgeable by anything on the
		// pod network that reaches :3002 directly, bypassing the hub nginx. The
		// hub now also emits X-Hive-Proxy-Auth = this hive's dashboard token (the
		// same secret we hold as authToken) on the auth-check success path, which
		// only the trusted proxy can produce. Require it as PROOF the request
		// transited the hub.
		//
		// Missing or incorrect proof fails closed. Older hub images that do not
		// inject X-Hive-Proxy-Auth must be upgraded before their identity headers
		// are accepted.
		proxyProofRejectReason := ""
		if !trusted {
			if ok, reason := trustProxyIdentity(true); ok {
				trusted = true
			} else {
				proxyProofRejectReason = reason
			}
		}

		// Per-user session path (device flow): resolve the session id in the
		// hive_session cookie to THIS request's user and inject that user's
		// identity. Two different people therefore get two different sessions
		// and each sees themselves — no shared identity.
		if !trusted {
			if sess := s.sessionFromRequest(r); sess != nil {
				// The role is re-resolved against the LIVE allowlist on every
				// request (liveSessionRole), never read from the session record:
				// the hub heartbeat keeps cfg.Dashboard.AuthorizedUsers in sync
				// with Manage Access, and a grant/downgrade/revocation must bind
				// now — not after the 30-day session expires. This is the fix for
				// the recurring "owner access required for a granted owner" class
				// (3rd report; see session_live_role.go). ok=false means the
				// allowlist is enforced and the user was REVOKED: fall through
				// unauthenticated instead of honoring the stale session.
				if role, ok := s.liveSessionRole(sess); ok {
					r.Header.Set("X-Hive-User", sess.Username)
					r.Header.Set("X-Hive-Role", role)
					if isOwnerRole(role) {
						r.Header.Set(ownerRoleVerifiedHeader, "true")
					}
					trusted = true
				}
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
				// Same reasoning as the X-Hive-Internal path above: the shared
				// dashboard token is the operator credential, and the dashboard
				// UI itself authenticates with it (Authorization: Bearer from
				// localStorage) on token-secured spokes reached directly. The
				// per-user session path already ran and did not match, and
				// inbound identity headers were stripped, so granting owner here
				// requires possession of the secret — nothing less (#4134).
				r.Header.Set("X-Hive-Role", config.RoleOwner)
				r.Header.Set(ownerRoleVerifiedHeader, "true")
			}
		}

		if !trusted {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				if proxyProofRejectReason != "" {
					http.Error(w, fmt.Sprintf(`{"error":%q}`, proxyProofRejectReason), http.StatusUnauthorized)
					return
				}
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			} else {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(loginPage))
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

// requestRoleAllowsOwner reports whether the request should be treated as an
// owner for owner-gated endpoints. It reads X-Hive-Role, which the authenticate
// middleware injects from a per-user session or from a proof-verified hub header.
//
// SECURITY (F9, CWE-862): a MISSING role must default to LEAST privilege on any
// spoke that has an auth boundary (a shared token or a direct-route allowlist).
// On such a spoke an empty role means no identity was established — either a
// dev/misconfig, or a request that reached the handler without being
// authenticated — and must NOT be silently promoted to owner (which previously
// let a missing-header request download the raw hive.yaml verbatim). The
// legitimate owner always arrives with X-Hive-Role set.
//
// The empty-role==owner convenience is preserved ONLY for a genuinely open spoke
// (no auth_token AND no allowlist), where there is no security boundary at all
// and the whole dashboard is already admitted anonymously.
func (s *Server) requestRoleAllowsOwner(r *http.Request) bool {
	role := r.Header.Get("X-Hive-Role")
	if role == "owner" {
		return true
	}
	if role == "" {
		// Least privilege: only an open/dev spoke (no boundary) may treat a
		// missing role as owner.
		return s.authToken == "" && !s.directRouteAuthzEnabled()
	}
	return false
}

// hubProxied reports whether this spoke sits behind the hub's nginx, which
// injects per-user X-Hive-User/X-Hive-Role identity headers. There the shared
// dashboard token is a server-to-server credential only and must never be
// requested from (or disclosed to) a browser.
func (s *Server) hubProxied() bool {
	return s.deps != nil && s.deps.Config != nil && s.deps.Config.Dashboard.HubProxied
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
		// Prometheus scrape target — bypasses dashboard auth only when
		// explicitly enabled via HIVE_METRICS_ENABLED (Prometheus cannot
		// authenticate via device flow). NOT actually open: handleMetrics
		// requires the HIVE_METRICS_TOKEN bearer token and fails closed (403)
		// when no token is configured (#3785).
		return true
	case path == "/snapshot" || strings.HasPrefix(path, "/snapshot/"):
		return true
	case strings.HasPrefix(path, "/api/snapshot"):
		return true
	case path == "/api/style":
		// Sanitized, same-origin CSS for public snapshot/read-only preview links.
		return true
	case path == "/contribute" || strings.HasPrefix(path, "/contribute/"):
		return true
	case path == "/api/contribute" || strings.HasPrefix(path, "/api/contribute/"):
		// SECURITY (C5): match ONLY the contribute flow (/api/contribute and its
		// subtree /api/contribute/...), NOT the sibling admin routes under
		// /api/contributors/... (trust/revoke/requeue/delete). A bare HasPrefix
		// of "/api/contribute" also matched "/api/contributors/..." because the
		// letters after the prefix ("rs/...") don't start with a boundary — so
		// those mutation routes were exempted from authenticate entirely and were
		// reachable anonymously on OpenShift Routes / in-cluster (no auth proxy).
		// The trailing-slash boundary excludes /api/contributors while keeping the
		// real public routes (register, the WS upgrade, status, etc.) public.
		return true
	case path == "/api/v1" || strings.HasPrefix(path, "/api/v1/"):
		// GitHub bearer authentication is enforced by handleAPIv1. Keeping this
		// versioned prefix outside dashboard session auth lets non-browser clients
		// reach that handler without trusting cookies or X-Hive-* headers.
		return true
	case path == "/leaderboard" || strings.HasPrefix(path, "/leaderboard/"):
		return true
	case strings.HasPrefix(path, "/api/leaderboard"):
		return true
	case strings.HasPrefix(path, "/api/gh-user-auth/"):
		return path != "/api/gh-user-auth/logout"
	case path == openRouterCallbackPath:
		// OpenRouter OAuth PKCE return: the sponsor's browser comes back with no
		// session, so the path must be public. The single-use state token in the
		// query IS the credential — the handler verifies it (unknown/expired/
		// replayed states are rejected) before storing anything.
		return true
	case path == linearAgentCallbackPath:
		// Linear agent OAuth return (RFC #4492 Part 2): the installing admin's
		// browser comes back from linear.app with no hive session. The
		// single-use state token is the credential, verified server-side.
		return true
	case path == linearAgentWebhookPath:
		// Linear AgentSessionEvent webhooks: Linear's servers cannot hold a
		// dashboard session. NOT actually open — the handler fails closed
		// without LINEAR_WEBHOOK_SECRET and verifies the HMAC signature over
		// the raw body plus the signed timestamp's replay window.
		return true
	case path == "/sso":
		// SSO handoff exchange: the caller has no session yet, the signed hub
		// token IS the credential. The handler itself verifies the token and
		// the authorized_users allowlist before minting a session, so exposing
		// the path unauthenticated does not weaken the allowlist gate.
		return true
	case path == "/gh-setup":
		// GitHub App Setup URL return: GitHub redirects a fresh browser here
		// after install, often without a hive session. The handler accepts only
		// IDs verified by an App-JWT lookup against this app and this hive's org.
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
    <button id="btn-start" data-action="startFlow">Sign in with GitHub</button>
  </div>
  <div id="step-code" style="display:none">
    <p class="instructions">Enter this code at GitHub:</p>
    <div class="code-wrap">
      <div class="code-box" id="user-code">--------</div>
      <button class="copy-btn" id="copy-btn" data-action="copyAndOpen">Copy &amp; Open</button>
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
    <button data-action="reload" style="margin-top:16px">Try again</button>
  </div>
</div>
<script>
// Event delegation (no inline on*= attributes; CSP script-src-attr is 'none').
document.addEventListener('click',function(e){
  var el=e.target.closest('[data-action]');
  if(!el)return;
  switch(el.getAttribute('data-action')){
    case 'startFlow':startFlow();break;
    case 'copyAndOpen':copyAndOpen();break;
    case 'reload':location.reload();break;
  }
});
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
    poll(d.interval||5,d.flow_id||'');
  }catch(e){showError('Network error: '+e.message)}
}
async function poll(interval,flowId){
  var ms=interval*1000;
  async function check(){
    try{
      // flow_id proves this poll belongs to the flow WE started — the server
      // refuses to mint the session for any poll that cannot present it.
      var r=await fetch('/api/gh-user-auth/poll',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({flow_id:flowId})});
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
			// SECURITY (C5): use the same trailing-slash boundary as isPublicPath so
			// the read-only write-gate exemption covers ONLY the contribute flow
			// (/api/contribute[/...]) and NOT the /api/contributors/... admin
			// mutation routes. A bare HasPrefix("/api/contribute") also matched
			// "/api/contributors/...", letting a "read" viewer promote/revoke/delete
			// contributors past this gate.
			isContributeFlow := r.URL.Path == "/api/contribute" || strings.HasPrefix(r.URL.Path, "/api/contribute/")
			if !isContributeFlow && r.URL.Path != "/api/gh-user-auth/status" {
				http.Error(w, `{"error":"your permissions on this hive are read-only, so changes are not allowed. Contact the owner of this hive to ask for write permissions."}`, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// BeginStatusSnapshot returns the current mutation epoch. Callers that build
// a full status snapshot should capture this BEFORE reading any state and
// pass it to UpdateStatusIfFresh, which drops the snapshot if a mutation
// landed while it was being built (#4348).
func (s *Server) BeginStatusSnapshot() uint64 {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.statusMutationEpoch
}

// noteStatusMutation records that server-side state just changed. It returns
// the minimum StatusSeq a published snapshot must carry to be guaranteed to
// reflect the mutation — any snapshot published with a lower seq was built
// before it. Mutation handlers hand this floor to the frontend so it can
// discard stale in-flight status responses (#4348).
func (s *Server) noteStatusMutation() uint64 {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	s.statusMutationEpoch++
	return s.statusSeq + 1
}

// UpdateStatus publishes a snapshot unconditionally (epoch captured at entry).
// Prefer BeginStatusSnapshot + UpdateStatusIfFresh when the snapshot build is
// slow enough for a mutation to race it.
func (s *Server) UpdateStatus(status *StatusPayload) {
	s.UpdateStatusIfFresh(status, s.BeginStatusSnapshot())
}

// UpdateStatusIfFresh publishes the snapshot unless a state mutation happened
// after buildEpoch was captured — a stale build must not overwrite (and then
// broadcast) pre-mutation values the operator has already seen change.
// Returns whether the snapshot was published; the mutation's own
// refresh-after-mutation rebuild repaints shortly after a drop.
func (s *Server) UpdateStatusIfFresh(status *StatusPayload, buildEpoch uint64) bool {
	if s.deps != nil && s.deps.Config != nil {
		status.ACMMLevel = detectACMMLevel(s.deps.Config)
		status.ACMMPackAgents = buildACMMPackAgents(s.deps.Config)
		status.GitHubBaseURL = s.deps.Config.GitHub.ResolvedBaseURL()
		if issue := config.ValidateRepoTargets(s.deps.Config); issue != nil {
			status.RepoTargetMisconfigured = true
			status.RepoTargetIssue = issue.Message
		} else {
			status.RepoTargetMisconfigured = false
			status.RepoTargetIssue = ""
		}
	}
	status.ContributorPool = s.BuildContributorPoolStatus()

	s.githubAppMu.RLock()
	status.GitHubAppRequired = s.githubAppRequired
	status.GitHubAppInstallURL = s.githubAppInstallURL
	status.GitHubAppPermIssue = s.githubAppPermIssue
	status.GitHubAppState = s.githubAppState
	s.githubAppMu.RUnlock()

	// CONFIG-TRUTH OVERRIDE, applied last so no probe-derived field can veto
	// it: a real App with installation_id 0 must light the install banner the
	// moment the page loads. The recheck loop's SetGitHubAppRequired state is
	// probe-derived and can lag (or, after a "Reset Forge App", hold an empty
	// install URL because the raw app_slug/base_url went blank); the RESOLVED
	// config — the same values the Forge App tab displays — is what the banner
	// must render. Operator-side classifications (key-missing/key-invalid/
	// no-app-assigned) are preserved: only an empty state is filled in, and
	// the placeholder app_id never reaches here (ConfiguredButUninstalled
	// requires a real App).
	if s.deps != nil && s.deps.Config != nil {
		g := s.deps.Config.GitHub
		if g.ConfiguredButUninstalled() {
			status.GitHubAppInstallMissing = true
			status.GitHubAppRequired = true
			if status.GitHubAppState == "" {
				status.GitHubAppState = githubAppStateNotInstalledToken
			}
		}
		if status.GitHubAppRequired && status.GitHubAppInstallURL == "" {
			status.GitHubAppInstallURL = g.AppInstallURL()
		}
	}

	status.InferenceBackends = s.buildInferenceBackends()

	// Attach the advisory ACMM level-up recommendation (#5225) from the SAME
	// signal-collection path the /api/acmm-recommendation endpoint uses, so the
	// endpoint and the status payload cannot report different advice. Pure
	// computation over already-collected signals — no I/O on this hot path.
	// ADVISORY ONLY: this must never auto-apply a level.
	if advice := acmmadvisor.RecommendFromStatus(s.buildACMMStatusInputs()); advice.CurrentLevel > 0 {
		status.ACMMAdvice = &advice
	}

	// Deep checks travel inside the status payload so every dashboard surface
	// (header pill included) renders the same truth the heartbeat sends the
	// hub. Judged against the payload in hand: it becomes s.status moments
	// from now, so "ready" must not fail merely because the first beat's
	// payload has not been assigned yet. Computed before statusMu is taken
	// below (healthSummaryFor must not run under it).
	s.statusMu.RLock()
	readyForDeep := s.ready
	s.statusMu.RUnlock()
	status.DeepHealth = s.healthSummaryFor(status, readyForDeep)

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
	if buildEpoch < s.statusMutationEpoch {
		// A mutation landed after this snapshot's build began: its data may
		// predate the mutation. Drop it — the mutation's own refresh rebuild
		// (which began after the epoch bump) publishes the fresh state.
		curEpoch := s.statusMutationEpoch
		s.statusMu.Unlock()
		s.logger.Debug("dropping stale status snapshot built before a mutation",
			"buildEpoch", buildEpoch, "mutationEpoch", curEpoch)
		return false
	}
	s.statusSeq++
	status.StatusSeq = s.statusSeq
	status.StatusInstance = strconv.FormatInt(s.startedAt.UnixNano(), 10)
	status.Timestamp = time.Now().UTC().Format(time.RFC3339)
	s.status = status
	s.lastFullBroadcast = time.Now()
	s.statusMu.Unlock()

	s.AppendTokenSparkline(status)
	s.AppendTrendHistory(status)
	// #4298: fold the budget window into per-window history so a closed window's
	// consumption survives the reset that erases the live number.
	s.ObserveBudgetWindow(status)

	data, err := json.Marshal(status)
	if err != nil {
		s.logger.Warn("failed to marshal status for SSE", "error", err)
		return true
	}

	s.broadcastFrame(fmt.Sprintf("data: %s\n\n", data))
	return true
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

// SetHubBanner sets the hub admin banner displayed on the spoke dashboard. It is
// called on every heartbeat that carries a banner, so it logs the first display
// only when the banner ID actually changes (a new banner became active), not on
// every re-delivery of the same one.
func (s *Server) SetHubBanner(id, message, color string) {
	s.hubBannerMu.Lock()
	isNew := s.hubBanner == nil || s.hubBanner.ID != id
	s.hubBanner = &HubBannerState{ID: id, Message: message, Color: color}
	s.hubBannerMu.Unlock()
	if isNew && s.logger != nil {
		s.logger.Info("hub banner displayed", "banner_id", id, "message", message, "color", color)
	}
}

// ClearHubBanner removes the hub admin banner.
func (s *Server) ClearHubBanner() {
	s.hubBannerMu.Lock()
	defer s.hubBannerMu.Unlock()
	s.hubBanner = nil
}

// handleBannerDismissed records that an authenticated user dismissed the hub
// banner. Dismissal remains a client-side action (the banner stays in the hub
// and re-appears for other viewers) — this endpoint only produces an audit line
// attributing the dismissal to a specific user, so an admin can tell who acted
// on a banner and when.
//
// Only authenticated users with write/owner permissions may dismiss: the
// roleEnforcement middleware already rejects a "read"-role viewer's POST with
// 403, and this handler additionally 403s any request it cannot attribute to a
// signed-in user (no session, no proxy-injected identity). The frontend keeps
// the banner visible on a non-2xx so a rejected dismissal does not hide it.
func (s *Server) handleBannerDismissed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		jsonError(w, "missing banner id", http.StatusBadRequest)
		return
	}

	// Resolve the acting user: a direct-route spoke has a per-user session
	// cookie; a hub-proxied spoke gets identity injected as X-Hive-User /
	// X-Hive-Role by nginx. Either establishes an authenticated user. The role
	// is the LIVE allowlist role (session_live_role.go): a revoked session
	// must not keep dismissing banners under its stale identity.
	username, role := "", ""
	if sess := s.sessionFromRequest(r); sess != nil {
		if live, ok := s.liveSessionRole(sess); ok {
			username, role = sess.Username, live
		}
	} else if hubUser := r.Header.Get("X-Hive-User"); hubUser != "" {
		username, role = hubUser, r.Header.Get("X-Hive-Role")
	}
	if username == "" {
		jsonError(w, "you must be signed in to dismiss this banner", http.StatusForbidden)
		return
	}
	// Defense in depth: reject a read-only user even if the middleware let this
	// through (e.g. no X-Hive-Role header on a direct-route read session).
	if role == "read" {
		jsonError(w, "read-only users cannot dismiss banners", http.StatusForbidden)
		return
	}

	if s.logger != nil {
		s.logger.Info("hub banner dismissed", "banner_id", body.ID, "by", username, "role", role)
	}
	jsonResponse(w, map[string]bool{"ok": true})
}

// githubAppStateNotInstalledToken is the pkg/github AppAuthState wire token
// for "genuinely not installed" (AppStateNotInstalled.String()), kept as the
// string the setter receives so this package needs no classifier dependency.
const githubAppStateNotInstalledToken = "not-installed"

// githubAppNotInstalled reports the config-truth state that must fail
// github_auth in BOTH health surfaces regardless of which client object still
// exists: an App with no installation cannot mint, so any working client is
// riding a cached token that cannot be renewed. Waiting for the first failed
// mint kept the banner down and the hub green for up to an hour after an
// installation was cleared — exactly when the operator needed the opposite.
func (s *Server) githubAppNotInstalled() bool {
	s.githubAppMu.RLock()
	defer s.githubAppMu.RUnlock()
	return s.githubAppRequired && s.githubAppState == githubAppStateNotInstalledToken
}

// Operator-side pkg/github AppAuthState wire tokens (AppAuthState.
// OperatorActionable()): credential failures only the hub operator can fix.
// Kept as literals for the same reason as githubAppStateNotInstalledToken.
const (
	githubAppStateKeyMissingToken    = "key-missing"
	githubAppStateKeyInvalidToken    = "key-invalid"
	githubAppStateNoAppAssignedToken = "no-app-assigned"
)

// githubAppCredsUndelivered is the config-truth rule for the OPERATOR-SIDE
// credential states, the exact sibling of githubAppNotInstalled: an App whose
// private key never arrived (or cannot sign for it) can never mint, so
// github_auth must fail in BOTH health surfaces even while a token-based
// GHClient still works. Before this, a hive stuck on key-missing showed
// github_auth ✓ ("token-based") for 8 days while its agents could not act as
// the App at all — the green check is what let kelly-headwaters sit degraded
// and unexamined (2026-08-12 → 2026-08-20). Returns the failure detail, or ""
// when no operator-side state is in force.
func (s *Server) githubAppCredsUndelivered() string {
	s.githubAppMu.RLock()
	defer s.githubAppMu.RUnlock()
	if !s.githubAppRequired {
		return ""
	}
	switch s.githubAppState {
	case githubAppStateKeyMissingToken, githubAppStateNoAppAssignedToken:
		return "GitHub App credentials not delivered by the hub — agents cannot act as the App (operator action; no action needed from the hive owner)"
	case githubAppStateKeyInvalidToken:
		return "GitHub App private key does not match the App it authenticates as — GitHub rejects its JWT (operator must push the correct key)"
	}
	return ""
}

func (s *Server) SetGitHubAppRequired(required bool) {
	s.githubAppMu.Lock()
	defer s.githubAppMu.Unlock()
	s.githubAppRequired = required
	if required && s.deps != nil && s.deps.Config != nil {
		s.githubAppInstallURL = s.deps.Config.GitHub.AppInstallURL()
	} else if required {
		// No config loaded (tests, early boot): fall back to the zero-value
		// GitHubConfig, which resolves to the public github.com install URL.
		// Derive it rather than hardcoding so the two paths can never drift.
		s.githubAppInstallURL = config.GitHubConfig{}.AppInstallURL()
	} else {
		s.githubAppInstallURL = ""
		s.githubAppPermIssue = ""
		s.githubAppState = ""
	}
}

// SetGitHubAppState records the classified reason App auth is failing, as a
// github.AppAuthState wire token. The banner and the hub both branch on this
// to decide whether the failure is the user's to fix or the operator's, so it
// must be set alongside every SetGitHubAppPermIssue call. Pass "" to clear.
func (s *Server) SetGitHubAppState(state string) {
	s.githubAppMu.Lock()
	defer s.githubAppMu.Unlock()
	s.githubAppState = state
}

// GetGitHubAppState returns the classified App auth state ("" when unknown).
func (s *Server) GetGitHubAppState() string {
	s.githubAppMu.RLock()
	defer s.githubAppMu.RUnlock()
	return s.githubAppState
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
	return time.Since(s.pendingGitHubAppInstallAt) <= pendingInstallExpiry
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
	ctx := r.Context()
	if s.deps != nil && s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	_, _ = s.AutoDiscoverGitHubInstallationID(ctx, true)
	ok := s.RecheckGitHubApp()
	w.Header().Set("Content-Type", "application/json")
	if ok {
		_, _ = w.Write([]byte(`{"status":"installed"}`))
	} else {
		s.githubAppMu.RLock()
		permIssue := s.githubAppPermIssue
		s.githubAppMu.RUnlock()
		if permIssue != "" {
			detail, _ := json.Marshal(permIssue)
			_, _ = w.Write([]byte(`{"status":"insufficient_permissions","detail":` + string(detail) + `}`))
		} else {
			_, _ = w.Write([]byte(`{"status":"not_installed"}`))
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
		jsonResponse(w, map[string]string{"status": "starting"})
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
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

	// livezHeartbeatStallMax is how old the last heartbeat *attempt* may be
	// before /api/livez reports unhealthy. Set to 3x the send interval (6
	// minutes): long enough to absorb a couple of missed ticks under CPU
	// starvation or a slow hub round trip without flapping the pod, but tight
	// enough that a genuinely wedged loop is caught within a single
	// human-observable "gray dot" investigation window.
	//
	// Deliberately measured against attempts, not successes: a hub that is
	// down or unreachable leaves successes arbitrarily stale while the loop
	// keeps beating happily, and restarting the pod cannot fix a network
	// partition. See handleLivez.
	livezHeartbeatStallMax = 3 * heartbeatSendInterval

	// livezStartupGrace bounds how long a freshly started process is treated
	// as healthy before the heartbeat loop has made its first attempt. Covers
	// waitForReady's own up-to-3-minute wait for the dashboard to come up
	// plus one heartbeat send attempt. Without this, a pod would fail
	// liveness during normal startup, before the heartbeat loop ever got a
	// chance to run.
	livezStartupGrace = 4 * time.Minute
)

// handleLivez is the liveness-only counterpart to /api/health. It includes
// everything /api/health checks (the HTTP server itself is responsive) PLUS,
// for hub-connected hives, a check that the heartbeat goroutine is actually
// still running its loop. The heartbeat goroutine can silently die (panic
// recovered upstream, deadlock, stuck HTTP call) while this HTTP server keeps
// serving fine — that's the bug this endpoint exists to catch: /api/health
// stays green in that case and kubelet has no reason to ever restart the pod,
// so the hub keeps showing the hive as offline (gray dot) indefinitely even
// though the pod is 1/1 Running.
//
// Crucially, "still running its loop" is measured by heartbeat *attempts*,
// not *successes*. Those are very different conditions:
//
//   - Attempts stalled  => the goroutine is wedged. A restart revives it, so
//     failing liveness is the correct and only remedy.
//   - Successes stalled but attempts advancing => the hub is unreachable or
//     rejecting. The process is perfectly healthy; restarting it cannot fix
//     a network partition, and killing the pod every failureThreshold window
//     just produces a crash-loop that outlives the outage. Routine on
//     firewalled clusters that reach the hub only intermittently.
//
// Gating liveness on success staleness (as this endpoint originally did) made
// every hub outage or firewall hiccup look like a dead process and got
// healthy pods killed in a loop. Heartbeat *freshness* is a connectivity
// signal, so it is reported via /api/health/deep and the hub's own
// staleness marking (which already greys the dot) rather than via liveness.
//
// Only the livenessProbe should point here. Readiness stays on /api/health:
// a transient hub outage would otherwise pull a perfectly-serving pod out of
// the Service's endpoints for no benefit.
func (s *Server) handleLivez(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	ready := s.ready
	s.statusMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")

	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		jsonResponse(w, map[string]string{"status": "starting"})
		return
	}

	// Hives with no hub configured never run a heartbeat loop at all, so
	// there is nothing to stall — never gate their liveness on this.
	if hub.HeartbeatEnabled() {
		lastAttempt, hasAttemptedOnce := hub.LastHeartbeatAttempt()
		switch {
		case !hasAttemptedOnce:
			// The loop has not reached its first send yet. Healthy during the
			// startup grace (waitForReady legitimately holds it off for
			// minutes); past that, the goroutine never got going.
			if age := time.Since(s.startedAt); age > livezStartupGrace {
				w.WriteHeader(http.StatusServiceUnavailable)
				jsonResponse(w, map[string]string{
					"status": "unhealthy",
					"detail": "heartbeat loop never started sending",
				})
				return
			}
		case time.Since(lastAttempt) > livezHeartbeatStallMax:
			// Attempts have stopped advancing entirely — the loop is wedged,
			// not merely unable to reach the hub. A restart is the remedy.
			w.WriteHeader(http.StatusServiceUnavailable)
			jsonResponse(w, map[string]string{
				"status":                    "unhealthy",
				"detail":                    "heartbeat loop stalled",
				"last_heartbeat_attempt_at": lastAttempt.UTC().Format(time.RFC3339),
			})
			return
		}
	}

	jsonResponse(w, map[string]string{"status": "ok"})
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

	// 2. GitHub auth — config truth first (see githubAppNotInstalled and
	// githubAppCredsUndelivered).
	if s.githubAppNotInstalled() {
		checks["github_auth"] = map[string]any{"status": "fail", "detail": "GitHub App not installed — no installation for this org"}
		overall = "degraded"
		failCount++
	} else if detail := s.githubAppCredsUndelivered(); detail != "" {
		checks["github_auth"] = map[string]any{"status": "fail", "detail": detail}
		overall = "degraded"
		failCount++
	} else if s.deps != nil && s.deps.GHAppAuth != nil {
		if _, err := s.deps.GHAppAuth.Token(s.deps.Ctx); err == nil {
			checks["github_auth"] = map[string]any{"status": "pass"}
		} else {
			checks["github_auth"] = map[string]any{"status": "fail", "detail": err.Error()}
			overall = "degraded"
			failCount++
		}
	} else if s.deps != nil && s.deps.GHClient != nil {
		// Stays "pass": a too-narrow PAT authenticates fine, so the auth is not
		// what is broken — a capability is. Reporting it as a fail would send
		// an operator to re-issue working credentials. The detail carries the
		// specific missing scope so the diagnosis is not left to a runtime 403.
		detail := "token-based"
		if s.deps.GHTokenScopes.Status == github.ScopeStatusMissing {
			detail = "token-based — " + s.deps.GHTokenScopes.Detail
		}
		checks["github_auth"] = map[string]any{"status": "pass", "detail": detail}
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
			} else if agentDisabledInConfig(s.deps.Config, name, proc) {
				// Switched off in config, so nothing ever starts it and it has
				// no session — the same DELIBERATE non-running state the paused
				// branch above already scores as "skip". Scoring it "fail"
				// meant every hive that turns an agent off reports a permanent
				// health failure it can never clear, and (via healthSummaryFor)
				// raises a standing hub alert for an agent nobody wants running.
				ac["disabled"] = true
				ac["status"] = "skip"
				ac["detail"] = "disabled in config — not started"
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

	// 10. Hub heartbeat (connectivity signal).
	//
	// Deliberately reported here rather than via /api/livez: a stale
	// heartbeat means the hub is unreachable or rejecting, which a pod
	// restart cannot fix. It surfaces as "warn" so operators (and the hub
	// dashboard's gray dot) still see the connectivity problem without the
	// kubelet killing an otherwise-healthy pod. See handleLivez.
	if hub.HeartbeatEnabled() {
		hbCheck := map[string]any{"status": "pass"}
		if lastSuccess, ok := hub.LastHeartbeatSuccess(); ok {
			hbCheck["last_success"] = lastSuccess.UTC().Format(time.RFC3339)
			hbCheck["last_success_age"] = time.Since(lastSuccess).Round(time.Second).String()
			if time.Since(lastSuccess) > livezHeartbeatStallMax {
				hbCheck["status"] = "warn"
				hbCheck["detail"] = "hub has not accepted a heartbeat recently — check hub reachability"
			}
		} else {
			hbCheck["status"] = "warn"
			hbCheck["detail"] = "no heartbeat accepted by the hub since startup"
		}
		if lastAttempt, ok := hub.LastHeartbeatAttempt(); ok {
			hbCheck["last_attempt"] = lastAttempt.UTC().Format(time.RFC3339)
			hbCheck["last_attempt_age"] = time.Since(lastAttempt).Round(time.Second).String()
		}
		if hbCheck["status"] == "warn" && overall == "ok" {
			overall = "degraded"
		}
		checks["hub_heartbeat"] = hbCheck
	}

	// 11. Queue trend (is work being processed?)
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
	jsonResponse(w, map[string]any{
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

// healthBootGrace bounds how long after process start the agents health check
// suppresses the *boot-transient* degraded conditions — agents still launching
// (0 running), a running agent sitting at a login prompt, or a non-running
// agent whose CLI has not re-authenticated yet. On a fresh start or a pod roll
// these are all expected: the supervisor relaunches each agent's tmux pane, the
// Copilot/Claude CLIs re-run their device-login handshake, and the MITM
// proxy/CA settles — none of which has completed in the first few minutes.
//
// Measured from s.startedAt (process construction), mirroring livezStartupGrace
// so both startup windows share one clock. Set to 5 minutes: an operator (Mike
// Spreitzer) saw the console hive flip to "Degraded — 4 agents need login"
// seconds after a legitimate pod roll and self-recover within ~5 minutes, so
// the window must cover the observed agent-relaunch + CLI-reauth settling time.
// It is deliberately longer than the original readyAt-based 90s grace, which
// was too short to outlast Copilot re-auth. Genuinely-persistent failures (a stale
// hub heartbeat, a 30-min output stall, a real repo/workflow failure) are NOT
// gated by this — they live outside the agents-coming-up path and keep
// degrading throughout, so a real problem is never masked by boot grace.
const healthBootGrace = 5 * time.Minute

// inHealthGrace reports whether the process is still inside the post-boot
// settling window, and how long it has been up. The age is surfaced in the
// agents check detail so an operator inspecting the health JSON sees WHY a
// not-yet-degraded hive is being graced, and can tell it apart from a healthy
// one. Once healthBootGrace elapses, a persisting need-login/down condition
// flips to the real "degraded" verdict.
func (s *Server) inHealthGrace() (bool, time.Duration) {
	age := time.Since(s.startedAt)
	return !s.startedAt.IsZero() && age < healthBootGrace, age
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if status == nil {
		jsonResponse(w, map[string]string{"status": "initializing"})
		return
	}
	jsonResponse(w, status)
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

	if _, err := fmt.Fprintf(w, "retry: %d\n\n", sseRetryMs); err != nil {
		return
	}
	flusher.Flush()

	s.statusMu.RLock()
	if s.status != nil {
		data, _ := json.Marshal(s.status)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			s.statusMu.RUnlock()
			return
		}
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

// RecordAdvisoryPost marks that the spoke SUCCESSFULLY posted/updated the
// advisory digest just now, carrying the finding count that went out. It clears
// any prior error, since a fresh success means the advisory path is healthy
// again. Called only from the digest-posting path — a hive with no advisory
// agents never reaches it, so its advisoryLastPostedAt stays zero and the hub
// reads it as UNKNOWN (never a false stale alarm).
func (s *Server) RecordAdvisoryPost(findings int) {
	s.advisoryMu.Lock()
	defer s.advisoryMu.Unlock()
	s.advisoryLastPostedAt = time.Now()
	s.advisoryLastFindings = findings
	s.advisoryLastError = ""
}

// RecordAdvisoryOverflow records how many findings the top-N cap withheld from
// the digest just posted. Kept separate from RecordAdvisoryPost so the
// long-standing "a post happened, with this many findings" contract (and every
// caller of it) is untouched; a hive that never caps simply reports 0.
func (s *Server) RecordAdvisoryOverflow(n int) {
	s.advisoryMu.Lock()
	defer s.advisoryMu.Unlock()
	s.advisoryLastOverflow = n
}

// AdvisoryCounts returns the finding count that went out in the last posted
// digest and how many further findings the cap withheld.
func (s *Server) AdvisoryCounts() (findings, overflow int) {
	s.advisoryMu.RLock()
	defer s.advisoryMu.RUnlock()
	return s.advisoryLastFindings, s.advisoryLastOverflow
}

// RecordAdvisoryError records that a digest post ATTEMPT failed, with a
// log-safe error string (the same one the spoke logs — never key material). It
// does NOT advance advisoryLastPostedAt: the last SUCCESSFUL post time must
// keep ageing so the hub still sees the digest going stale, while the error
// gives the operator the specific cause (403 issues:write, rate limit, …).
func (s *Server) RecordAdvisoryError(errMsg string) {
	s.advisoryMu.Lock()
	defer s.advisoryMu.Unlock()
	s.advisoryLastError = errMsg
}

// AdvisoryState returns the last successful advisory-post time (zero if the
// spoke has never posted one), the finding count that went out then, and the
// most recent post error ("" when the last attempt succeeded). The heartbeat
// builder reports these so the hub can flag a stale advisory digest.
//
// An inference-backend AUTH failure is folded into lastError so an advisory
// digest that cannot be produced BECAUSE every inference call is 401'ing trips
// the hub's staleness gate 3a IMMEDIATELY (with the auth cause) instead of
// waiting 90 minutes for the last-post time to age out. This is deliberately
// gated on the hive being an advisory PARTICIPANT — advisoryLastPostedAt
// non-zero, i.e. it has successfully posted at least once — so a pure PR/merge
// hive with no advisory agents (which never posts, so the hub reads it as
// UNKNOWN) is never false-alarmed by an inference-auth blip on some other path.
// A real advisory-post error already recorded takes precedence, since it is the
// more specific cause; the inference-auth fold only fills an otherwise-empty
// error. It self-clears the moment inference recovers, because the provider
// stops reporting the signal.
//
// Bead-store LOAD failures fold the same way (fma incident: /data/beads dirs
// group-owned by a foreign gid locked the server out at startup, so every
// digest built empty and the advisory silently aged for 3 days while the
// agents kept writing findings). Gated on participation like the inference
// fold, and outranked by both a real post error and the inference cause —
// which are more specific. Self-clears on a restart that loads all stores.
func (s *Server) AdvisoryState() (lastPostedAt time.Time, lastFindings int, lastError string) {
	s.advisoryMu.RLock()
	postedAt, findings, errMsg := s.advisoryLastPostedAt, s.advisoryLastFindings, s.advisoryLastError
	s.advisoryMu.RUnlock()

	if errMsg == "" && !postedAt.IsZero() {
		if infErr, _ := InferenceAuthError(); infErr != "" {
			errMsg = infErr
		} else if s.deps != nil && s.deps.BeadStoreLoadFailures > 0 {
			errMsg = fmt.Sprintf("%d bead store(s) failed to load at startup (check /data/beads ownership) — advisory digest is built from the stores that DID load", s.deps.BeadStoreLoadFailures)
		}
	}
	return postedAt, findings, errMsg
}

// InferenceAuthState returns the spoke's current inference-backend auth-failure
// signal — a non-empty, log-safe cause string and the time it first latched
// while every inference call is being rejected (a stale gateway key), empty
// otherwise. The heartbeat builder reports it as a DEDICATED field so the hub
// can raise an "inference auth failing" alert whose ROOT cause an operator sees
// directly, distinct from a GitHub-post advisory staleness. Unlike the
// AdvisoryState fold, this is NOT gated on advisory participation: a hive whose
// inference key is dead is broken whether or not it also posts advisories, and
// the hub-side alert is the right place to surface that. Self-clears when
// inference recovers.
func (s *Server) InferenceAuthState() (errMsg string, since time.Time) {
	return InferenceAuthError()
}

// HealthSummary returns a deep-health summary with individual check results for heartbeats.
// agentCLIUnauthenticated reports whether an agent's CLI backend lacks working
// credentials — the exact signal the agent panel uses to render its AUTH/Login
// button: the pane poller saw a login prompt (proc.NeedsLogin), or the
// shared-credential probe registered via SetBackendAuthProvider (wired to
// agent.Manager.BackendAuthAvailable in main.go) reports the backend's auth
// state as known-and-unavailable. Backend resolution mirrors buildAgents:
// BackendOverride wins over the configured backend. For backends the probe
// cannot introspect (known=false) this returns false — an unknown auth state
// must not reclassify a genuinely crashed agent out of "down".
// healthAgentStatuses returns the agent snapshots the health check classifies.
// A test seam mirroring SetBackendAuthProvider: AllStatuses returns value
// snapshots, so tests cannot stage states (running-at-login-prompt) by
// mutating manager internals — they override this instead. nil ⇒ live manager.
var healthAgentStatuses func() map[string]*agent.AgentProcess

func agentCLIUnauthenticated(proc *agent.AgentProcess, authFn func(backend string) (available, known bool)) bool {
	if proc.NeedsLogin {
		return true
	}
	backend := proc.Config.Backend
	if proc.BackendOverride != "" {
		backend = proc.BackendOverride
	}
	// METHOD GATE. An inference backend (litellm/vllm/llm-d) authenticates with
	// an API key supplied by config — there is no interactive login and so no
	// "needs login" state an operator could act on. Checking this BEFORE the
	// probe stops a shared-credential miss from flagging an inference-backed
	// agent that is running fine. See pkg/agent/authprobe.go for the full
	// precedence rule.
	if agent.IsInferenceBackend(backend) {
		return false
	}
	// POSITIVE EVIDENCE beats absence-of-file: a running agent that the pane
	// poller has NOT seen at a login prompt is working (the same signal deep
	// health folds into `pass`), so a missing credentials file must not
	// reclassify it. Only a non-running agent falls through to the probe. This
	// is what stopped the empty shared credential path (per-agent-UID layout)
	// from reporting healthy agents as needing login.
	if proc.State == agent.StateRunning {
		return false
	}
	if authFn == nil {
		return false
	}
	available, known := authFn(backend)
	return known && !available
}

func (s *Server) HealthSummary() map[string]any {
	s.statusMu.RLock()
	status := s.status
	ready := s.status != nil && s.ready
	s.statusMu.RUnlock()
	return s.healthSummaryFor(status, ready)
}

// healthSummaryFor computes the deep-health checks against an explicit status
// payload and readiness verdict, so UpdateStatus can embed a snapshot judged
// against the payload it is ABOUT to install — judging s.status there would
// report a false "ready: fail" on the first beat after boot, before any
// payload has been assigned. Callers must not hold statusMu (the readiness
// and queue inputs are passed in instead of read here).
func (s *Server) healthSummaryFor(status *StatusPayload, ready bool) map[string]any {
	type check struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail,omitempty"`
	}
	checks := []check{}
	fails := 0
	warns := 0

	// 1. Readiness
	if ready {
		checks = append(checks, check{Name: "ready", Status: "pass"})
	} else {
		checks = append(checks, check{Name: "ready", Status: "fail", Detail: "not ready"})
		fails++
	}

	// 2. GitHub auth — config truth first (see githubAppNotInstalled and
	// githubAppCredsUndelivered).
	if s.githubAppNotInstalled() {
		checks = append(checks, check{Name: "github_auth", Status: "fail", Detail: "GitHub App not installed — no installation for this org"})
		fails++
	} else if detail := s.githubAppCredsUndelivered(); detail != "" {
		checks = append(checks, check{Name: "github_auth", Status: "fail", Detail: detail})
		fails++
	} else if s.deps != nil && s.deps.GHAppAuth != nil {
		if _, err := s.deps.GHAppAuth.Token(s.deps.Ctx); err != nil {
			// Surface the underlying error, not a bare "token error": this
			// detail travels to the hub and into the dashboard tooltip, and a
			// swallowed error string is exactly what left past github_auth flaps
			// undiagnosable. Token() now serves a still-valid cached token
			// through a transient mint blip, so reaching here means no usable
			// token remains — a genuine, sustained auth failure worth showing.
			checks = append(checks, check{Name: "github_auth", Status: "fail", Detail: "token error: " + err.Error()})
			fails++
		} else {
			checks = append(checks, check{Name: "github_auth", Status: "pass"})
		}
	} else if s.deps != nil && s.deps.GHClient != nil {
		// See the /health counterpart above: pass with a scope-specific detail.
		detail := "token"
		if s.deps.GHTokenScopes.Status == github.ScopeStatusMissing {
			detail = "token — " + s.deps.GHTokenScopes.Detail
		}
		checks = append(checks, check{Name: "github_auth", Status: "pass", Detail: detail})
	} else {
		checks = append(checks, check{Name: "github_auth", Status: "fail", Detail: "no auth"})
		fails++
	}

	if status != nil && status.RepoTargetMisconfigured {
		checks = append(checks, check{Name: "repo_target", Status: "warn", Detail: status.RepoTargetIssue})
		warns++
	}

	// 3. Agents
	if s.deps != nil && s.deps.AgentMgr != nil {
		grace, bootAge := s.inHealthGrace()
		const staleOutputThreshold = 30 * time.Minute
		running := 0
		paused := 0
		disabled := 0
		stalled := 0
		unsubstituted := 0
		down := 0
		needLogin := 0
		// The names behind the counts. "1 down" alone is unactionable — the
		// operator's next question is always WHICH one, and the answer was
		// dropped right here where it was known.
		var downNames, stalledNames, needLoginNames []string
		authFn := getBackendAuthFn()
		statuses := s.deps.AgentMgr.AllStatuses()
		if healthAgentStatuses != nil {
			statuses = healthAgentStatuses()
		}
		for name, proc := range statuses {
			if proc.Paused {
				paused++
				continue
			}
			if proc.State == agent.StateRunning {
				// A RUNNING agent sitting at a login prompt is alive but cannot
				// work — the pane poller has literally seen "/login" on its
				// terminal (proc.NeedsLogin). Counting it as running rendered a
				// wedged agent healthy (green dot, Health OK) while its pane
				// begged for authentication. Only the pane-poller signal moves a
				// running agent here: the shared-credential probe can lag a
				// just-completed login, and a running agent that is actually
				// working must never be reclassified by a stale probe.
				if !grace && proc.NeedsLogin {
					needLogin++
					needLoginNames = append(needLoginNames, name)
					continue
				}
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
						stalledNames = append(stalledNames, name)
					}
				}
			} else if !grace {
				// Disabled in config is a deliberate off-state, not a fault:
				// nothing starts the agent, so "down" would be a permanent
				// false alarm on any hive that switched an agent off. Counted
				// separately (like paused) so the detail line still says the
				// agent is not running, without failing the check.
				if agentDisabledInConfig(s.deps.Config, name, proc) {
					disabled++
					continue
				}
				// A non-running agent whose CLI has no credentials is not
				// crashed — it is waiting for a human to click Login on the
				// agent panel. Bucket it separately so the operator reads an
				// owner-actionable "need login" instead of chasing a "down"
				// agent that will keep exiting until someone authenticates.
				if agentCLIUnauthenticated(proc, authFn) {
					needLogin++
					needLoginNames = append(needLoginNames, name)
				} else {
					down++
					downNames = append(downNames, name)
				}
			}
		}
		// Map iteration order is random; sorted names keep the detail line
		// stable across beats so the hub does not see a "changed" status that
		// is really the same agents in a different order.
		sort.Strings(downNames)
		sort.Strings(stalledNames)
		sort.Strings(needLoginNames)
		detail := fmt.Sprintf("%d running", running)
		if paused > 0 {
			detail += fmt.Sprintf(", %d paused", paused)
		}
		if disabled > 0 {
			detail += fmt.Sprintf(", %d disabled", disabled)
		}
		if down > 0 {
			detail += fmt.Sprintf(", %d down: %s", down, strings.Join(downNames, ", "))
		}
		if needLogin > 0 {
			verb := "need"
			if needLogin == 1 {
				verb = "needs"
			}
			detail += fmt.Sprintf(", %d %s login: %s", needLogin, verb, strings.Join(needLoginNames, ", "))
		}
		st := "pass"
		// need-login keeps the same fail semantics as down: the agent still
		// cannot work, and the hub row must keep surfacing it until a human
		// clicks Login — the detail line tells them which kind of broken it is.
		if down > 0 || needLogin > 0 {
			st = "fail"
			fails++
		}
		// During the boot-grace window the transient conditions above (agents
		// still launching, running-at-login-prompt, CLI not re-authenticated)
		// have been suppressed from the down/need-login counts, so the check
		// reads "pass" instead of the false "degraded" that scared operators
		// after a legitimate pod roll. Make the suppression observable rather
		// than silent: an operator reading the health JSON must see that grace
		// is the reason it is not degraded, and that it will flip to a real
		// verdict once the window elapses if the condition persists.
		if grace {
			detail += fmt.Sprintf(" — within boot grace (agents re-authenticating), age=%s", bootAge.Round(time.Second))
		}
		checks = append(checks, check{Name: "agents", Status: st, Detail: detail})

		if stalled > 0 {
			checks = append(checks, check{Name: "stall_detection", Status: "warn", Detail: fmt.Sprintf("%d stalled (no output 30+ min): %s", stalled, strings.Join(stalledNames, ", "))})
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
	if status != nil {
		total := 0
		for _, repo := range status.Repos {
			total += len(repo.ActionableIssues)
		}
		checks = append(checks, check{Name: "queue", Status: "pass", Detail: fmt.Sprintf("%d actionable", total)})
	}

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

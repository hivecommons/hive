package dashboard

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/taskmcp"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/celtrigger"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	convergenceaudit "github.com/hivecommons/hive/pkg/convergence/audit"
	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/mention"
	"github.com/hivecommons/hive/pkg/nous"
	"github.com/hivecommons/hive/pkg/review/autoscale"
	"github.com/hivecommons/hive/pkg/review/eventdispatch"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/tokens"
)

type ChatOutbound struct {
	Seq      uint64 `json:"seq"`
	Text     string `json:"text"`
	Role     string `json:"role"`
	AuthorID string `json:"author_id,omitempty"`
}

// ChatPoll mirrors dashchat.Poll for GET /api/chat/messages: the retained
// entries newer than the client's cursor plus the metadata that makes the
// cursor safe across restarts and ring eviction (hivecommons/hive#9135).
type ChatPoll struct {
	Messages []ChatOutbound `json:"messages"`
	Next     uint64         `json:"next"`
	Epoch    string         `json:"epoch"`
	Gap      bool           `json:"gap,omitempty"`
}

type Dependencies struct {
	Config *config.Config
	// AgentMgr is the agent runtime (hivecommons/hive#13536). The dashboard
	// programs against the crux.Runtime surface, not *agent.Manager; tests that
	// need manager-only seams unwrap it with agentManager.
	AgentMgr crux.Runtime
	// TaskMCPActiveLaunches is the optional narrow dashboard seam for the
	// manager's active hub-launched agent scopes. Nil falls back to AgentMgr.
	TaskMCPActiveLaunches func() []taskmcp.LaunchScope
	Governor              *governor.Governor
	GHClient              *ghpkg.Client
	GHAppAuth             *ghpkg.AppAuth
	// CruxRuntimeHealth reports the agent runtime's reachability for
	// /api/health (hivecommons/hive#14138 stage 4). nil (in-process, or a
	// test without a runtime) leaves the field out of the body.
	CruxRuntimeHealth func() CruxRuntimeHealth
	// GHTokenScopes is the boot-time PAT scope probe result (see
	// ghpkg.CheckTokenScopes). It is set only on the token-auth path; the App
	// path leaves it at ScopeStatusSkipped because Apps carry permissions, not
	// OAuth scopes. github_auth reports it as a DETAIL on an otherwise-passing
	// check: a narrow token still authenticates, so failing the check would
	// misreport the fault — what is broken is a capability, not the auth.
	GHTokenScopes ghpkg.ScopeResult
	// StageEngineStatus reports the ADR-0025 central stage engine's mode and
	// counters for /api/status.stageEngine and the activity tracker's
	// status strip. nil leaves the block out of /api/status.
	StageEngineStatus func() stageengine.Status
	// ReviewerAutoscaleStatus reports the reviewer-lane autoscaler
	// (hivecommons/hive#14685) for /api/status.reviewerAutoscale. nil, or a
	// Status with Enabled=false, leaves the block out.
	ReviewerAutoscaleStatus func() autoscale.Status
	Tokens                  *tokens.Collector
	Knowledge               *knowledge.KnowledgeAPI
	Inception               *knowledge.InceptionEngine
	Nous                    *NousState
	Scheduler               SchedulerControl
	MetricsCollector        *MetricsCollector
	// KnowledgePrimer builds, publishes and reports the scheduler's kick
	// primer — the one thing knowledge.enabled actually gates (#9231). Nil
	// (bare test deps) means the toggle can only persist the flag, so it
	// answers restart_required.
	KnowledgePrimer KnowledgePrimerControl
	// KnowledgeConnectors is the running connector syncer; nil when none is wired.
	KnowledgeConnectors KnowledgeConnectorRuntime
	// KnowledgePublish is the running knowledge.publish mirror; nil when
	// publishing is not configured or failed to start.
	KnowledgePublish KnowledgePublishRuntime
	// RotationMgr is the provider-rotation headroom reporter (RFC #3958). Nil
	// when rotation is disabled; the headroom endpoint then reports
	// enabled=false.
	RotationMgr rotation.HeadroomReporter
	// HeadroomPublisher is the publish-only headroom source used when rotation
	// is disabled but contributor quota readings are still being probed.
	HeadroomPublisher rotation.HeadroomReporter
	// FleetStats is the spoke's fleet-stat contribution collector (PRs
	// merged/rejected over the trailing 90-day window, cached, refreshed on a
	// 30-minute timer). The ACMM advisor derives its baseline merge-success
	// rate from this cache (#3972), so wiring the SAME collector the heartbeat
	// already uses keeps the advisor read-only and adds zero GitHub traffic.
	// Nil is safe: Snapshot() on a nil collector reports ready=false and the
	// advisor leaves the signal at its conservative zero.
	FleetStats *collect.FleetStatsCollector
	Activity   *collect.ActivityCollector
	// RepoCost caches the /api/repo-cost interval join on a ticker (see
	// repo_cost_collector.go), mirroring Activity's caching of
	// /api/repo-activity. Nil is safe: handleRepoCost falls back to computing
	// inline (used by tests that construct a bare Dependencies).
	RepoCost *collect.RepoCostCollector
	// LaunchCost caches the /api/launch-cost join (#12128). Nil is safe:
	// handleLaunchCost falls back to computing inline from Tokens.
	LaunchCost      *collect.LaunchCostCollector
	BeadSynthesizer *knowledge.BeadSynthesizer
	BeadStores      map[string]*beads.Store
	// ChatResponder handles dashboard Hive Chat free-text messages that are
	// not satisfied by local deterministic intents. Nil means no responder is
	// configured, so /api/chat must return a visible unavailable message rather
	// than leaving the browser waiting.
	ChatResponder func(ctx context.Context, query string, history []any) (string, error)
	// DelegateDecider runs the owner-delegate decider turn behind "Let hive
	// decide" for items the rules leave at the reversible default
	// (decisions.delegate.llm.enabled). Nil keeps the rules-first decider
	// alone.
	DelegateDecider delegatedecision.TurnRunner
	// ChatGrounder runs the read-only grounding turn behind Hive Chat's
	// grounded answers (chat.grounded_answers.enabled, api_chat_grounded.go):
	// the question plus a compact hive-state snapshot in, the answer out.
	// Nil means no inference backend is wired, so chat falls back to the
	// deterministic handlers / command usage.
	ChatGrounder delegatedecision.TurnRunner
	// BeadStoreLoadFailures counts configured bead stores that failed to open at
	// startup and were therefore LEFT OUT of BeadStores entirely. The dependency
	// admission gate (contribute_admission_deps.go) needs this because it cannot
	// otherwise tell a hive with three stores from a hive with four where one
	// would not load: the map looks identical, so a dependent whose blocking bead
	// lived in the dropped store reads as "declared nothing" and is admitted.
	// Only the producer knows a store is missing, so only the producer can say so.
	BeadStoreLoadFailures int
	Logger                *slog.Logger
	AuditLedger           *mutation.Ledger
	AuditJournal          *mutation.Journal
	AuditProofs           *proof.Store
	AuditPublisher        convergenceaudit.FindingPublisher
	AuditPublisherFunc    func() convergenceaudit.FindingPublisher
	AuditOutcomes         *outcome.Ledger
	AuditOutcomesFunc     func() *outcome.Ledger
	Ctx                   context.Context
	RefreshFunc           func()
	PersistFunc           func()
	SkipReloadFunc        func()
	ReInitFunc            func()
	EnumerateFunc         func()
	// OnACMMLevelChanged is called after an in-process runtime level change
	// has been persisted/applied. Nil means no live component needs to react.
	OnACMMLevelChanged func(prev, next int)
	// ReviewConfigApplied is called after PUT /api/config/review has mutated
	// Config.Review, so the owner of any component that caches review
	// settings at boot (the GitHub client's revise allowlist and perspective
	// set, and the review-thread reconciler's classification.review_bots,
	// whose min_priority the same PUT edits) can push the new values into
	// it. Nil means nothing caches.
	ReviewConfigApplied func(config.ReviewConfig)
	// ChannelsConfigApplied is called after PUT /api/config/channels has
	// mutated the inbound-trigger sections (github.mentions, github.actions,
	// escalation sinks), so the mention poller can start or stop and the
	// escalation dispatcher can rebuild its sinks without a restart. Nil
	// means nothing hot-reloads; the tab then shows the restart note.
	ChannelsConfigApplied func()
	// RescanReposFunc re-enumerates every watched repository's open issues
	// and pull requests against the forge, enriches PR CI status, applies the
	// duplicate-PR claim guard and republishes the dashboard status snapshot
	// — the read-only half of the governor eval cycle, with none of its side
	// effects (no mode change, no agent kicks, no advisory posting). It backs
	// the REPOSITORIES "Rescan" button, which must be safe to press at any
	// time. Nil (the shape most tests construct) makes POST /api/repos/rescan
	// answer 503 rather than pretending to have scanned.
	RescanReposFunc   func(ctx context.Context) (*ghpkg.ActionableResult, error)
	AdvisoryResetFunc func(newPrimaryRepo string)
	ReinitGitHubFunc  func(appID, installationID int64, keyFile string) error
	// ResolveAppKeyFileFunc resolves which App private key the process would
	// actually sign with, given the configured key_file and app_id — the SAME
	// resolution the boot and heartbeat-apply paths use (config value, then
	// $GH_APP_KEY_FILE, then the per-app-id delivered file, then the generic
	// fallbacks). Hub-delivered keys deliberately leave key_file empty, so any
	// handler that gates on the raw config value alone dead-ends on exactly the
	// fleet-standard hosted-spoke configuration (#2459).
	ResolveAppKeyFileFunc func(configured string, appID int64) string
	// IssueClaimed reports whether an open PR — hive-authored OR from an
	// external contributor — already claims to fix the given issue
	// (kubestellar/hive#3768). It is backed by the governor's duplicate-PR
	// claim ledger, which parses `Fixes #N` / `Closes #N` references (a
	// branch-name heuristic, and since #3980 non-closing `Refs #N` references
	// as weak claims) out of every open PR each eval cycle and persists them
	// across restarts. The contribute selectTask consults it so an issue with a
	// fix already in flight is never offered to another contributor — including
	// one whose PR deliberately did not claim to close it.
	// repo is tried in the same spelling the ledger keys on (the config repo
	// form); a nil func means "no claim data" and disables the check.
	IssueClaimed func(repo string, number int) (ghpkg.IssueClaim, bool)
	// OpenPRLinkingIssue is the live companion to IssueClaimed
	// (hivecommons/hive#14799): it asks the forge — one cached open-PR
	// listing per repo per sweep — whether an OPEN pull request already
	// links the issue with a closing keyword, labels the issue
	// `has-open-pr` and logs the refusal. The ledger IssueClaimed reads is
	// refreshed by the eval cycle, so a PR a person opened by hand stayed
	// invisible to the contribute lane until the next scan and the issue was
	// dispatched again in that window. labels are the issue's current labels
	// so the label write is idempotent. Nil disables the live check; the
	// client honours the scanner open-PR switch itself.
	OpenPRLinkingIssue func(repo string, number int, labels []string) (ghpkg.OpenPRLink, bool)

	// Watsonx / OpenRouter are the LLM-provider gateway views (#5565 slice 3):
	// narrow consumer-defined interfaces over pkg/watsonx and pkg/openrouter,
	// with concrete adapters constructed in cmd/hive. Nil is safe — the
	// affected routes then answer with explicit errors instead of panicking.
	Watsonx    WatsonxGateway
	OpenRouter OpenRouterGateway
	// NewLinearAgent constructs the Linear agent service bundle
	// (pkg/linearagent) behind the LinearAgentGateway interface; the dashboard
	// calls it lazily, once, passing its kick/session-agent callbacks. Nil
	// means Linear integration is unavailable (bare test servers).
	NewLinearAgent func(LinearAgentPorts) LinearAgentGateway
	// MentionWebhook is the public GitHub mention webhook accelerator. It is
	// HMAC-verified by pkg/mention and only triggers a poll cycle.
	MentionWebhook http.Handler
	// MentionStore is shared by GitHub mention polling and Actions dispatch so
	// run_id/run_attempt dedupe is transport-independent.
	MentionStore *mention.Store
	// ActionDispatch hooks let tests drive the Actions dispatch endpoint without
	// constructing a full agent manager. Production leaves them nil.
	ActionDispatchKick   func(agentName, message, source string) error
	ActionDispatchAgents func() []mention.AgentInfo
	ActionsJWKSFetcher   func(ctx context.Context, url string) ([]byte, error)
	ActionsClock         func() time.Time
	DashboardChatSubmit  func(user, role, text string) (uint64, error)
	DashboardChatPoll    func(since uint64) ChatPoll
	// LinearStoredViewerID reports the persisted Linear install's viewer id
	// ("" when none) plus the store path, for the assigned_only validation
	// message. Nil disables the check's install probe.
	LinearStoredViewerID func() (viewerID, storePath string)

	// ApprovalDesk is the single tool-approval decision point (RFC #4000). Nil
	// when `tool_approval.enabled` is false — the default — in which case every
	// legacy gate stays authoritative and the Approvals panel renders as "not
	// enabled". Never construct a desk here as a fallback: a nil desk IS the
	// signal that the feature is off.
	ApprovalDesk *toolapprove.Desk
	// ApprovalInbox is the durable operator-lane queue backing the desk. Nil
	// exactly when ApprovalDesk is nil (both come from toolapprove.DeskFromConfig).
	ApprovalInbox *toolapprove.Inbox
	// RecordIssueClaim writes one claim into the same ledger IssueClaimed
	// reads (hivecommons/hive#7871). The contribute hub calls it after
	// verifying, against GitHub, that a PR or commit a no_work_needed verdict
	// cited really does settle the issue — so a fix that landed WITHOUT
	// referencing the issue suppresses it the same way a referencing PR would.
	// A nil func disables recording; verdicts are still honoured by the
	// hub's own (activity-voidable) verdict ledger exactly as before.
	RecordIssueClaim func(ghpkg.IssueClaim) error
	// IssueChurn reports how many pull requests the same ledger has observed
	// against an issue and how they ended (hivecommons/hive#7995). The
	// contribute admission path consults it so an issue that has already
	// absorbed several merged or abandoned PRs without settling is withheld
	// for a maintainer instead of being offered for another cycle of the same.
	// repo is tried in the same spellings as IssueClaimed; a nil func means
	// "no churn data" and disables the guard.
	IssueChurn func(repo string, number int) (ghpkg.IssueChurn, bool)
	// IssueClaims is the worker-claim ledger (hivecommons/hive#8380): who is
	// actively working an issue right now — a human session, a hub-kicked
	// agent, a relay contributor, or an external author — ranked so a higher
	// kind takes the issue over and the displaced holder is told to stop.
	// Distinct from IssueClaimed (open-PR claims, rebuilt each scan). Nil
	// disables recording, enforcement and the /api/claims routes.
	IssueClaims *claims.Ledger
	HookFire    func(context.Context, hooks.Payload)
	CELTrigger  func(context.Context, celtrigger.NormalizedEvent, string)
	// RunBurndown optionally projects cheap convergence progress for one run
	// detail page. It is intentionally absent from the list path; sources may
	// need to inspect a graph or in-memory campaign state for the requested key.
	RunBurndown func(context.Context, string) (*RunBurndown, error)
	// RunFanout optionally fans an approved multi-repo plan out into
	// implementation waves. Nil is the default/off state.
	RunFanout func(context.Context, string, []string) ([]string, error)
	// WavefrontComplete records a completed Wavefront run-stage item. Nil means
	// wavefront completion is not wired.
	WavefrontComplete func(context.Context, string, string, string, time.Time) error
	// WavefrontUnknown records a stale in-flight Wavefront item whose owner did
	// not return after a restart/crash. Nil means unknown transitions are off.
	WavefrontUnknown func(context.Context, string, string, time.Time) error
	// ExternalExec reports which external-execution engines are compiled
	// into this binary (#8361). cmd/hive backs it with the extwork registry;
	// nil means none, and the Features panel says so.
	ExternalExec ExternalExecution
	// QuestionAutoclose is the live question auto-close schedule (#9584),
	// read only by GET /api/config/governor/question-autoclose/schedule;
	// every write to governor.question_autoclose goes through Config like
	// any other setting. cmd/hive adapts questionclose.Manager to this
	// consumer-defined interface so pkg/dashboard does not import it. Nil in
	// bare test Dependencies.
	QuestionAutoclose QuestionAutocloseSchedule

	// ReviewEvents queues webhook-triggered review dispatch
	// (hivecommons/hive#11091). Nil leaves reviews on the cadence path.
	ReviewEvents *eventdispatch.Dispatcher
	// WebhookWake asks the governor loop for an early, debounced eval cycle
	// after a verified GitHub webhook invalidated cached PR or issue state.
	// reason names the delivery ("pull_request_review org/repo#12"). Nil
	// leaves invalidated items waiting for the next cadence tick.
	WebhookWake func(reason string)
	// SweepNudge asks the automerge lanes for an early pass over one PR
	// (repo is "owner/name"). The operator row actions that leave a
	// durable mark for the sweep (Adopt now) call it so the pass runs now
	// rather than on cadence. Nil leaves the mark for the next sweep.
	SweepNudge func(repo string, number int)
	// ApplyWebhookItems hands the targeted re-reads a webhook delivery
	// caused (webhook_tracker_refresh.go) to the governor so its cached
	// actionable snapshot is patched for those items only; the wake cycle
	// then plans from the patched snapshot without re-listing. Nil leaves
	// the governor's snapshot to the reconcile pass.
	ApplyWebhookItems func(snaps []ghpkg.WebhookItemSnapshot)
	// WebhookDelivery receives every verified GitHub delivery for a managed
	// repo after the PR caches applied it: the forgestate cache patches the
	// named item and the automerge lanes schedule a pass for the affected
	// PRs (hivecommons/hive#13377). nil installs no receiver.
	WebhookDelivery func(event, deliveryID string, payload []byte, res ghpkg.WebhookEventResult)
	// EvalLoop reports the governor loop's live pacing (poll vs webhook mode
	// and the last early wake) for /api/status. Nil omits the fields.
	EvalLoop func() EvalLoopStatus
	// SentinelPREvent evaluates one PR with the sentinel after a verified
	// `pull_request` opened/synchronize/reopened delivery named it (#13379);
	// repo is the configured slug. It must not block the webhook response.
	// Nil leaves PRs to the sentinel's reconcile cadence.
	SentinelPREvent func(repo string, number int)
	// CIFailureKick reacts to one webhook CI-failure signal on a
	// hive-authored PR: attributes it to the owning agent and, when the
	// agent is enabled, unpaused, governor-eligible and outside the per-head
	// cooldown, kicks it now with the fix-before-new section. It must not
	// block the webhook response. Nil leaves the signal to the sweep.
	CIFailureKick func(sig ghpkg.CIFailureSignal) CIFailureKickOutcome
	// ProjectsV2ItemEvent applies a verified `projects_v2_item` delivery to
	// the GitHub Projects work source cache as a targeted patch (#13379).
	// These deliveries are organization-scoped and carry no repository. Nil
	// leaves the project list to its reconcile cadence.
	ProjectsV2ItemEvent func(payload []byte) WebhookPatchOutcome
}

// WebhookPatchOutcome is what a targeted webhook patch did, for the
// delivery response and the log line.
type WebhookPatchOutcome struct {
	Applied bool   `json:"applied"`
	Detail  string `json:"detail,omitempty"`
}

// EvalLoopStatus is the governor loop's pacing block served by /api/status:
// Mode is "webhook" while healthy GitHub webhooks stretch the base tick to
// governor.eval_interval_webhook_s and "poll" otherwise; LastWakeReason and
// LastWakeAt name the most recent webhook-triggered early cycle.
type EvalLoopStatus struct {
	Mode           string     `json:"evalMode"`
	IntervalS      int        `json:"evalIntervalS,omitempty"`
	LastWakeReason string     `json:"lastWakeReason,omitempty"`
	LastWakeAt     *time.Time `json:"lastWakeAt,omitempty"`
	WebhookWakes   int        `json:"webhookWakes,omitempty"`
	// CachedCycles counts eval cycles that planned from the cached snapshot
	// (webhook and re-kick wakes) instead of re-listing; WebhookPatches
	// counts items webhook re-reads upserted into that snapshot.
	CachedCycles   int `json:"cachedCycles,omitempty"`
	WebhookPatches int `json:"webhookPatches,omitempty"`
}

// NousScopeRepo is the only experiment scope the Experiment Lab supports.
// Governor-scope experiments are out of scope for the lab.
const NousScopeRepo = "repo"

type NousState struct {
	Mode string `json:"mode"`
	// Scope is always NousScopeRepo; it is kept as a field for state
	// compatibility with the experiment loop's status publishing.
	Scope        string                   `json:"scope"`
	Phase        string                   `json:"phase"`
	Status       map[string]interface{}   `json:"status"`
	Ledger       []map[string]interface{} `json:"ledger"`
	Principles   []NousPrinciple          `json:"principles"`
	Config       map[string]interface{}   `json:"config"`
	GatePending  map[string]interface{}   `json:"gate_pending,omitempty"`
	GateResponse map[string]interface{}   `json:"gate_response,omitempty"`
	SnapshotDir  string                   `json:"-"`
	// PersistPanel, when set, receives the operator-set panel state (Mode,
	// Scope, Config) after every mutating /api/nous call so it survives a
	// restart (#11694); cmd/hive wires it to pkg/nousstore. Nil keeps the
	// pre-#11694 in-memory-only behaviour.
	PersistPanel func(NousPanelSnapshot) error `json:"-"`
	// GateStore, when set, is the durable gate protocol store (#11696):
	// GatePending mirrors its pending gate and gate decisions are recorded
	// through it. Nil keeps the pre-#11696 in-memory-only behaviour.
	GateStore NousGateStore `json:"-"`
	// GateNotify, when set, is called once per newly pending gate id with
	// a title and message (cmd/hive wires the notifier). Nil disables pings.
	GateNotify func(title, message string) `json:"-"`
	// GateLogger receives gate protocol warnings; nil silences them.
	GateLogger *slog.Logger `json:"-"`
	// KnowledgeStore, when set, serves the ledger and principles live
	// (#11697): the read handlers refresh Ledger/Principles from it on
	// every request. Nil keeps the boot-snapshot behaviour.
	KnowledgeStore NousKnowledgeStore `json:"-"`
	// Verdict is the campaign's final recommendation, nil until there is
	// one (#11699). It is served inside /api/nous/status as "verdict" and
	// is never acted on automatically; see POST /api/nous/promote.
	Verdict *NousVerdict `json:"verdict,omitempty"`
	// Promotions are the owner promotions recorded so far (all campaigns).
	Promotions []NousPromotion `json:"promotions,omitempty"`
	// VerdictStore, when set, serves Verdict live (cmd/hive wires the
	// engine's Verdict face). Nil leaves Verdict as set in memory.
	VerdictStore NousVerdictStore `json:"-"`
	// PromotionStore, when set, is the durable promotion record (#11699);
	// Promotions mirrors it. Nil keeps promotions in memory only.
	PromotionStore NousPromotionStore `json:"-"`
	// ResetStore, when set, performs the on-disk part of POST
	// /api/nous/reset (RFC #11585); cmd/hive wires pkg/nousstore. Nil
	// answers the endpoint with 404.
	ResetStore NousResetStore `json:"-"`
	// ArtifactSync, when set, triggers one immediate pass of the optional
	// GitHub artifact mirror (#11701) and returns its result for the
	// response body; cmd/hive wires it to pkg/nousartifacts only when
	// nous.artifact_repo is configured. Nil means the mirror is disabled
	// and POST /api/nous/artifacts/sync answers 404.
	ArtifactSync func(ctx context.Context) (interface{}, error) `json:"-"`
	// Now is the clock for decision timestamps; nil means time.Now.
	// GovernorDir is where the external experiment loop writes ledger.json,
	// principles.json and its state (/var/run/nous/governor). Empty means
	// the loop's activity cannot be observed (#12541).
	GovernorDir string `json:"-"`
	// CampaignFile is the per-campaign baseline store (pkg/nous). Empty
	// means "<dir of SnapshotDir>/campaigns.json"; with no SnapshotDir
	// either, campaigns are disabled and the baseline is global.
	CampaignFile string `json:"-"`
	// Now is the clock for decision timestamps; nil means time.Now.
	Now func() time.Time `json:"-"`
	Mu  sync.Mutex       `json:"-"`

	gateNotifyMu     sync.Mutex
	lastNotifiedGate string

	stateOnce  sync.Once
	stateStore *hivestate.Store

	campaignOnce  sync.Once
	campaignStore *nous.Store
}

// NousPrinciple is one learned principle as served by GET /api/nous/principles.
// Type and Status follow the shared schema (NousPrincipleType*,
// NousPrincipleStatus*); Source is the writer's raw category, kept for
// provenance.
type NousPrinciple struct {
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
	Type       string  `json:"type"`
	Status     string  `json:"status"`
}

// NousBaselineTarget is how many hive-state snapshots a campaign's baseline
// needs: one every governor evaluation cycle (NousBaselinePerHour at the
// default 15-minute interval) for NousBaselineDays — 4 × 24 × 7 = 672, about
// one week of observations, so the baseline sees every weekday and weekend
// regime once.
const (
	NousBaselinePerHour = 4
	NousBaselineDays    = 7
	NousBaselineTarget  = NousBaselinePerHour * 24 * NousBaselineDays
)

// NousBaselineTargetExplainer is the plain-language meaning of
// NousBaselineTarget shown next to every "N / 672" in the Experiment Lab.
const NousBaselineTargetExplainer = "≈ 7 days of observations (4/hour)"

// NousPrinciplesExplainer says what the "Principles" tile counts and how the
// number grows (#12532): principles are not a prerequisite for starting an
// experiment, so a new lab legitimately shows 0.
const NousPrinciplesExplainer = "Principles are durable lessons the lab distils from judged experiments (principles.json). A new lab starts at 0 and the count grows as experiments conclude; none are needed to start an experiment."

// Per-campaign lifecycle stages (#12532). They are derived on every status
// read from the campaign's baseline, the lab mode and the experiment rows
// attributed to the campaign, so the card always says what happens next.
const (
	NousStageBaseline  = "baseline"  // collecting baseline snapshots
	NousStageReady     = "ready"     // baseline complete, no proposals yet
	NousStageProposed  = "proposed"  // a proposal awaits operator review
	NousStageRunning   = "running"   // an experiment is in flight
	NousStageConcluded = "concluded" // archived, or every experiment judged
	NousStagePaused    = "paused"
)

// NousActionGenerateProposals is the single primary card action offered
// when a campaign's baseline is complete but the lab is still in observe
// mode: it flips the trust-ladder mode to suggest so the experiment loop
// may design proposals for review.
const NousActionGenerateProposals = "generate_proposals"

// NousLoopIdleCycles is how many governor evaluation cycles may pass without
// any file write under NousState.GovernorDir before the Experiment Lab says
// the external experiment loop is not running on this hive (#12541). The
// hive designs no proposals itself, so a ready card in suggest/evolve mode
// would otherwise wait forever. 24 cycles is 2 h at the default 300 s
// interval.
const NousLoopIdleCycles = 24

// ExperimentLoopLastWrite returns the newest modification time of the files
// directly under GovernorDir — the directory the hive reads the external
// experiment loop's output from — or the zero time when nothing was ever
// written there.
func (ns *NousState) ExperimentLoopLastWrite() time.Time {
	var last time.Time
	if ns == nil || ns.GovernorDir == "" {
		return last
	}
	entries, err := os.ReadDir(ns.GovernorDir)
	if err != nil {
		return last
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if mt := info.ModTime(); mt.After(last) {
			last = mt
		}
	}
	return last.UTC()
}

// NousSnapshotRetention bounds the snapshots kept on disk: the newest
// 4x baseline target, so baseline_pct stays at 100 while old files go
// (hivecommons/hive#12030).
const NousSnapshotRetention = 4 * NousBaselineTarget

// NousBaselinePct reports baseline-collection progress as a percentage,
// bounded to 0..100. the count is bounded by NousSnapshotRetention, but "percent complete" should not (hivecommons/hive#10026).
func NousBaselinePct(count int) float64 {
	if count <= 0 {
		return 0
	}
	pct := float64(count) * 100 / NousBaselineTarget
	if pct > 100 {
		return 100
	}
	return pct
}

// NousSnapshot is one per-eval-cycle hive state record. The series now lives
// in pkg/hivestate (#12339); the alias keeps existing callers compiling.
type NousSnapshot = hivestate.Snapshot

// HiveState returns the hive state time series backed by SnapshotDir, or nil
// when no directory is configured. Collection is independent of Nous mode.
func (ns *NousState) HiveState() *hivestate.Store {
	if ns == nil || ns.SnapshotDir == "" {
		return nil
	}
	ns.stateOnce.Do(func() { ns.stateStore = hivestate.NewStore(ns.SnapshotDir) })
	return ns.stateStore
}

func (ns *NousState) RecordSnapshot(govState governor.State, actionable *ghpkg.ActionableResult, agentsKicked []string, agentStatuses map[string]*crux.Process, tokenSummary *tokens.AggregateSummary) error {
	store := ns.HiveState()
	if store == nil {
		return nil
	}

	snap := NousSnapshot{
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		Mode:         string(govState.Mode),
		QueueIssues:  govState.QueueIssues,
		QueuePRs:     govState.QueuePRs,
		QueueHold:    govState.QueueHold,
		AgentsKicked: agentsKicked,
		AgentStates:  make(map[string]string),
	}
	if actionable != nil {
		snap.SLAViolations = actionable.Issues.SLAViolations
	}
	for name, proc := range agentStatuses {
		snap.AgentStates[name] = string(proc.State)
	}
	if tokenSummary != nil {
		snap.TotalTokens = tokenSummary.TotalTokens
	}

	err := store.Record(snap)
	ns.refreshStatus()
	return err
}

func (ns *NousState) refreshStatus() {
	ns.Mu.Lock()
	defer ns.Mu.Unlock()

	count := 0
	if entries, err := os.ReadDir(ns.SnapshotDir); err == nil {
		count = len(entries) - pruneNousSnapshots(ns.SnapshotDir, entries, NousSnapshotRetention)
	}

	if count > 0 && ns.Phase == "collecting" {
		ns.Phase = "observing"
	}

	// snapshots/snapshotCount stay the whole observation history; the
	// baseline percentage is scoped to the active campaign (#12501).
	baselineCount := count
	if c, ok := ns.ActiveCampaign(); ok {
		baselineCount = ns.CampaignSnapshots(c)
	}
	ns.Status["snapshots"] = count
	ns.Status["snapshotCount"] = count
	ns.Status["baseline_pct"] = NousBaselinePct(baselineCount)
	ns.Status["baseline_target"] = NousBaselineTarget
	ns.Status["snapshotTarget"] = NousBaselineTarget
	ns.Status["phase"] = ns.Phase
	ns.Status["principleCount"] = len(ns.Principles)
}

// pruneNousSnapshots removes the oldest snapshot files beyond keep and
// returns how many were removed. entries is the name-sorted ReadDir listing;
// snapshot names are fixed-width unix milliseconds, so name order is age order.
func pruneNousSnapshots(dir string, entries []os.DirEntry, keep int) int {
	removed := 0
	for i := 0; i < len(entries)-keep; i++ {
		if entries[i].IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entries[i].Name())); err == nil {
			removed++
		}
	}
	return removed
}

type SchedulerControl interface {
	BuildAgentMessage(agentName string, issues []ghpkg.Issue, actionable *ghpkg.ActionableResult) string
	BuildAgentMessageFromLastActionable(agentName string) string
	FirstScanDone() bool
	DeferKickUntilFirstScan(agentName string, deliver func(msg string))
	GetLastActionable() *ghpkg.ActionableResult
	GetLaneDepths() map[string]int
	// ResolveTemplate / TemplateExists report kick_template provenance for
	// the prompt editor and the general-settings guard (#7390).
	ResolveTemplate(agentName string) scheduler.TemplateResolution
	TemplateExists(templateName string) (source string, ok bool)
}

// KnowledgePrimerStatus reports whether kicks are primed with knowledge
// (#9231). It is what the dashboard's knowledge "enabled" state means: the
// KnowledgeAPI object exists regardless of knowledge.enabled (the bead
// synthesizer and vault browsing need it), so its existence says nothing
// about whether any kick receives ${KNOWLEDGE}.
type KnowledgePrimerStatus struct {
	// Registered is true when the scheduler holds a primer, i.e. kicks
	// carry a knowledge section.
	Registered bool `json:"registered"`
	// Sources lists what the registered primer queries; empty when not
	// registered.
	Sources []knowledge.PrimerSource `json:"sources"`
	// RestartRequired is set when the running process cannot fully apply
	// knowledge.enabled live; RestartReason says what waits for a restart.
	RestartRequired bool   `json:"restart_required"`
	RestartReason   string `json:"restart_reason,omitempty"`
}

// KnowledgePrimerControl is implemented by the process that owns the
// scheduler and the connected knowledge stores (cmd/hive).
type KnowledgePrimerControl interface {
	// SetKnowledgePrimer builds a primer from the configured layers,
	// registers every connected store with it and publishes it to the
	// scheduler (enabled), or unregisters it (disabled).
	SetKnowledgePrimer(enabled bool) KnowledgePrimerStatus
	// KnowledgePrimerStatus reports the current primer registration.
	KnowledgePrimerStatus() KnowledgePrimerStatus
}

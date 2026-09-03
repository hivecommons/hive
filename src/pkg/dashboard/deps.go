package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/kubestellar/hive/pkg/agent"
	"github.com/kubestellar/hive/pkg/beads"
	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/dashboard/collect"
	ghpkg "github.com/kubestellar/hive/pkg/github"
	"github.com/kubestellar/hive/pkg/governor"
	"github.com/kubestellar/hive/pkg/hooks"
	"github.com/kubestellar/hive/pkg/knowledge"
	"github.com/kubestellar/hive/pkg/rotation"
	"github.com/kubestellar/hive/pkg/tokens"
	"github.com/kubestellar/hive/pkg/toolapprove"
)

type SchedulerControl interface {
	BuildAgentMessage(agentName string, issues []ghpkg.Issue, actionable *ghpkg.ActionableResult) string
	BuildAgentMessageFromLastActionable(agentName string) string
	GetLastActionable() *ghpkg.ActionableResult
}

type Dependencies struct {
	Config    *config.Config
	AgentMgr  *agent.Manager
	Governor  *governor.Governor
	GHClient  *ghpkg.Client
	GHAppAuth *ghpkg.AppAuth
	// GHTokenScopes is the boot-time PAT scope probe result (see
	// ghpkg.CheckTokenScopes). It is set only on the token-auth path; the App
	// path leaves it at ScopeStatusSkipped because Apps carry permissions, not
	// OAuth scopes. github_auth reports it as a DETAIL on an otherwise-passing
	// check: a narrow token still authenticates, so failing the check would
	// misreport the fault — what is broken is a capability, not the auth.
	GHTokenScopes    ghpkg.ScopeResult
	Tokens           *tokens.Collector
	Knowledge        *knowledge.KnowledgeAPI
	Inception        *knowledge.InceptionEngine
	Nous             *NousState
	Scheduler        SchedulerControl
	MetricsCollector *MetricsCollector
	// RotationMgr is the provider-rotation manager (RFC #3958). Nil when
	// rotation is disabled; the headroom endpoint then reports enabled=false.
	RotationMgr *rotation.Manager
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
	RepoCost        *collect.RepoCostCollector
	BeadSynthesizer *knowledge.BeadSynthesizer
	BeadStores      map[string]*beads.Store
	// BeadStoreLoadFailures counts configured bead stores that failed to open at
	// startup and were therefore LEFT OUT of BeadStores entirely. The dependency
	// admission gate (contribute_admission_deps.go) needs this because it cannot
	// otherwise tell a hive with three stores from a hive with four where one
	// would not load: the map looks identical, so a dependent whose blocking bead
	// lived in the dropped store reads as "declared nothing" and is admitted.
	// Only the producer knows a store is missing, so only the producer can say so.
	BeadStoreLoadFailures int
	Logger                *slog.Logger
	Ctx                   context.Context
	RefreshFunc           func()
	PersistFunc           func()
	SkipReloadFunc        func()
	ReInitFunc            func()
	EnumerateFunc         func()
	AdvisoryResetFunc     func(newPrimaryRepo string)
	ReinitGitHubFunc      func(appID, installationID int64, keyFile string) error
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
	// exactly when ApprovalDesk is nil (both come from DeskFromConfig).
	ApprovalInbox *toolapprove.Inbox
	HookFire      func(context.Context, hooks.Payload)
}

type NousState struct {
	Mode         string                   `json:"mode"`
	Scope        string                   `json:"scope"`
	Phase        string                   `json:"phase"`
	Status       map[string]interface{}   `json:"status"`
	Ledger       []map[string]interface{} `json:"ledger"`
	Principles   []NousPrinciple          `json:"principles"`
	Config       map[string]interface{}   `json:"config"`
	GatePending  map[string]interface{}   `json:"gate_pending,omitempty"`
	GateResponse map[string]interface{}   `json:"gate_response,omitempty"`
	SnapshotDir  string                   `json:"-"`
	Mu           sync.Mutex               `json:"-"`
}

type NousPrinciple struct {
	ID         string  `json:"id"`
	Text       string  `json:"text"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"`
}

const NousBaselineTarget = 672

type NousSnapshot struct {
	Timestamp     string            `json:"timestamp"`
	Mode          string            `json:"mode"`
	QueueIssues   int               `json:"queue_issues"`
	QueuePRs      int               `json:"queue_prs"`
	QueueHold     int               `json:"queue_hold"`
	SLAViolations int               `json:"sla_violations"`
	AgentsKicked  []string          `json:"agents_kicked,omitempty"`
	AgentStates   map[string]string `json:"agent_states,omitempty"`
	TotalTokens   int64             `json:"total_tokens"`
}

func (ns *NousState) RecordSnapshot(govState governor.State, actionable *ghpkg.ActionableResult, agentsKicked []string, agentStatuses map[string]*agent.AgentProcess, tokenSummary *tokens.AggregateSummary) error {
	if ns.SnapshotDir == "" {
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

	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling snapshot: %w", err)
	}

	filename := fmt.Sprintf("%s/%d.json", ns.SnapshotDir, time.Now().UnixMilli())
	if err := os.WriteFile(filename, data, 0o644); err != nil {
		return fmt.Errorf("writing snapshot: %w", err)
	}

	ns.refreshStatus()
	return nil
}

func (ns *NousState) refreshStatus() {
	ns.Mu.Lock()
	defer ns.Mu.Unlock()

	count := 0
	if entries, err := os.ReadDir(ns.SnapshotDir); err == nil {
		count = len(entries)
	}

	if count > 0 && ns.Phase == "collecting" {
		ns.Phase = "observing"
	}

	ns.Status["snapshots"] = count
	ns.Status["snapshotCount"] = count
	ns.Status["baseline_pct"] = float64(count) * 100 / NousBaselineTarget
	ns.Status["baseline_target"] = NousBaselineTarget
	ns.Status["snapshotTarget"] = NousBaselineTarget
	ns.Status["phase"] = ns.Phase
	ns.Status["principleCount"] = len(ns.Principles)
}

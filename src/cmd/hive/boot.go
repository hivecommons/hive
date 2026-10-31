package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/crux/watchdog"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	"github.com/hivecommons/hive/pkg/convergence/publish"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/knowledge/connector"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/priorart"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/questionclose"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/review/eventdispatch"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/writeback"
)

// boot is the state main() used to keep in ~40 local variables, made explicit
// so main() can be split into phases (#7571, step 1 — the follow-up to the
// runEvalCycle and boot_seams extractions of #7232). Each boot* method is a
// contiguous slice of the old main() body, called in the original order; the
// fields are grouped by the phase that sets them, and a later phase reads
// only fields an earlier phase has set.
//
// Why one shared struct rather than a return value per phase: three of the
// phases install closures that REASSIGN ghClient and appAuth long after boot
// (the dashboard's ReinitGitHubFunc, the config watcher's reload handler, and
// the heartbeat's GitHub App config callback), and every later reader — the
// eval cycle, refreshDashboard, the Re-check button, the canary scanner —
// must see the replacement. In main() that worked because they all captured
// the same local; here it works because they all read b.ghClient through the
// same pointer. Per-phase return structs would have to be threaded by
// pointer through every subsequent phase to preserve that, which is this
// struct with more plumbing.
//
// Behaviour is unchanged by construction: nothing is reordered, no goroutine
// starts earlier or later, and the deferred cleanups run in the order they
// always did (see cleanup).
type boot struct {
	// cleanup collects what main() used to `defer` so it still runs LIFO on
	// main()'s return, not on the return of the phase that registered it.
	cleanup deferStack

	// cfgReloadMu serializes the config watcher's reload callback (which runs
	// on a timer goroutine and swaps/mutates *cfg, including cfg.Agents) with
	// the governor loop's tick bodies that read cfg. Without it, a reload
	// racing the fast agent-status tick crashed the process with "concurrent
	// map read and map write" in buildConfiguredAgents.
	cfgReloadMu sync.Mutex

	// ghClientLive is the GitHub client provider that long-lived consumers
	// (collectors, the scheduler's triage commenter) read on every use, and
	// requestRelays runs the request watchers and the self-authored merge
	// sweep on the current client. adoptGitHubClient swaps both on every
	// rebuild (#9621); see github_client_provider.go.
	ghClientLive atomic.Pointer[githubClientRef]
	// priorArtSearcher is the prior-art gate's Searcher, shared by the
	// scheduler's park and the issue relay's filing verdict (#13879).
	priorArtSearcher atomic.Pointer[priorart.Searcher]

	// forgeState is the per-repo GitHub state cache the governor work
	// source reads from when `forgestate.enabled` is on (#13375). Built
	// once by the first configureGitHubClient and attached to every
	// rebuilt client after that; forgeStateOnce guards the build. Held in
	// an atomic because the dashboard's count wiring reads it late, from
	// the metrics collector goroutine (#13376).
	forgeState                        atomic.Pointer[forgestate.Store]
	forgeStateOnce                    sync.Once
	requestRelays                     *requestRelaySupervisor
	integrationEligibility            automerge.IntegrationEligibilityGate
	integrationPreparationEligibility automerge.IntegrationEligibilityGate

	// bootConfig
	startTime                     time.Time
	configPath                    string
	logger                        *slog.Logger
	cfg                           *config.Config
	ctx                           context.Context
	preShutdownHooks              shutdownHooks
	ghAuth                        githubAuth
	ghClient                      *github.Client
	appAuth                       *github.AppAuth
	gov                           *governor.Governor
	sched                         *scheduler.Scheduler
	notifier                      *notify.Notifier
	repoTargetMisconfigured       func() bool
	repoTargetIssueMessage        func() string
	advisoryIssues                map[string]int
	advisoryStore                 *advisory.Store
	agentMgr                      hostedRuntime
	approvalDesk                  *toolapprove.Desk
	approvalInbox                 *toolapprove.Inbox
	issueClaims                   *claims.Ledger
	dashSrv                       *dashboard.Server
	writeback                     *writeback.Gate
	writebackOnce                 sync.Once
	beadStores                    map[string]*beads.Store
	beadStoreLoadFailures         int
	tokenCollector                *tokens.Collector
	metricsCollector              *dashboard.MetricsCollector
	fleetStatsCollector           *collect.FleetStatsCollector
	activityCollector             *collect.ActivityCollector
	repoCostCollector             *collect.RepoCostCollector
	launchCostCollector           *collect.LaunchCostCollector
	lastActionable                atomic.Pointer[github.ActionableResult]
	evalCycles                    uint64
	knowledgeAPI                  *knowledge.KnowledgeAPI
	beadSynth                     *knowledge.BeadSynthesizer
	nousState                     *dashboard.NousState
	inceptionEngine               *knowledge.InceptionEngine
	rotationMgr                   *rotation.Manager
	questionAutoclose             atomic.Pointer[questionclose.Manager]
	wd                            *watchdog.Reconciler
	onDemandFromPack              map[string]bool
	refreshDashboard              func()
	mutationBoundary              effects.Boundary
	findingPublisher              *publish.Publisher
	outcomeLedger                 *outcome.Ledger
	heartbeatFleetStats           func() (*int, *int, *int, string)
	heartbeatRepoActivity         func() ([]spoke.RepoActivityWire, string, int, int)
	heartbeatBudgetWindow         func() (*int64, *int64, *bool, string, string)
	heartbeatACMMLevel            func() int
	heartbeatAgents               func(govState governor.State, currentMode string, includeBlockingChecks bool) []spoke.AgentSummary
	dashboardURLForFreshHeartbeat func() string
	leaderboardForHeartbeat       func() []spoke.LeaderboardEntry
	ownerForHeartbeat             func() string
	dashboardURLForHeartbeat      func() string
	dashboardDependencies         func() *dashboard.Dependencies
	wireSessionPrune              func()

	// bootGitHub
	appAuthFailure string
	appAuthState   github.AppAuthState

	// bootGovernor
	promptFetcher              promptsrc.Fetcher
	defFetcher                 defsrc.Fetcher
	definitionResolver         *defsrc.Resolver
	pendingTokenSeed           []dashboard.TokenSparklineEntry
	pendingFactSeed            []dashboard.FactHistoryEntry
	pendingCostSeed            []dashboard.CostHistoryEntry
	pendingCostRollupSeed      []dashboard.CostDailyRollup
	pendingBudgetWindowSeed    []collect.BudgetWindowEntry
	pendingConvergenceSoakSeed []dashboard.ConvergenceSoakEntry
	pendingTrendSeed           []dashboard.TrendHistoryEntry

	// bootAdvisory
	acmmLevel         int
	githubAppDiag     string
	githubAppState    github.AppAuthState
	githubAppRequired bool
	policyDirPath     string
	projectCtx        crux.ProjectContext
	mutationStats     *effects.Recorder

	// bootAgents
	agentMinter crux.AgentMintIssuer

	// bootState
	saved *snapshot.PersistedState

	// bootKnowledge
	gitSyncer            *knowledge.GitSyncer
	knowledgeConnectors  *connector.Syncer
	knowledgePublish     *connector.Mirror
	promotionScheduler   *knowledge.PromotionScheduler
	quotaAccount         string
	quotaPoolDir         string
	explicitQuotaPoolDir bool
	// knowledgeAPIFallback records that knowledgeAPI is the file-only API
	// bootKnowledge auto-enables when knowledge.enabled is false, not one
	// built from the configured layers/engine/curator (#9231).
	knowledgeAPIFallback bool
	// knowledgePrimerMu serializes building/publishing the scheduler's kick
	// primer (knowledgePrimerControl) with the async graph-store wiring.
	knowledgePrimerMu sync.Mutex

	// bootSupervision
	quotaReadingPublisher    *rotation.Manager
	linearCredentialResolver func() crux.LinearCredential

	// bootWatchers
	configWatcher *config.Watcher

	// bootProxy
	githubProxy *proxy.GitHubProxy
	// inferenceSink is the one per-agent inference usage ledger; the proxy
	// records into it in-process and the hive callback's usage capability
	// records into it for a sidecar runtime (#14138 stage 4).
	inferenceSink   *tokens.InferenceSink
	inferenceSinkMu sync.Mutex

	// bootHeartbeat
	reporterName     string
	processStartedAt time.Time
	hubURL           string

	// bootLanes
	trajLane   *trajectory.Lane
	replanLane *planning.ReplanLane
	retroLane  *retro.Lane

	// runLoop
	lastAutoMergeSweep     time.Time
	lastTaskListSweep      time.Time
	lastDuplicateSweep     time.Time
	lastSupersessionSweep  time.Time
	lastDuplicateFixSweep  time.Time
	lastSharedCIRerunSweep time.Time
	lastBlockedIssuesSweep time.Time
	lastIssueUnparkSweep   time.Time
	lastClosedIssueSweep   time.Time
	// lastClarityResumeSweep gates runClarityGateResumeSweepIfDue
	// (hivecommons/hive#13588).
	lastClarityResumeSweep time.Time
	// lastPriorArtResumeSweep gates runPriorArtGateResumeSweepIfDue
	// (hivecommons/hive#13575).
	lastPriorArtResumeSweep time.Time
	lastSentinelSweep       time.Time
	lastGFISweep            time.Time
	// lastLegacyFleetReportSweep gates runLegacyFleetReportSweepIfDue
	// (hivecommons/hive#13470).
	lastLegacyFleetReportSweep time.Time
	// gfi is good-first-issue curation state (good_first_issue.*).
	gfi *gfiState
	// firstTimer is first-time contributor welcome state
	// (governor.first_timer_welcome.*).
	firstTimer *firstTimerState
	// rfcCoverage is the triage existing-coverage check state
	// (issues.rfc_existing_check.*).
	rfcCoverage *rfcCoverageState
	// rfcNotify is the RFC lifecycle Discord notice state
	// (notifications.discord.rfc_channel_id).
	rfcNotify *rfcNotifyState

	// reviewEvents queues webhook-triggered review dispatch and reviewWake
	// carries its wakes to the governor loop (hivecommons/hive#11091).
	reviewEvents *eventdispatch.Dispatcher
	reviewWake   chan struct{}
	// verdictWaker turns dispatched review verdicts the relay records into
	// early eval cycles, at most one per governor.webhook_wake_min_gap_s
	// (hivecommons/hive#14705).
	verdictWaker *webhookWaker

	// webhookWaker coalesces cache-invalidating GitHub webhooks into early
	// eval cycles and evalLoop is the pacing block /api/status reports.
	webhookWaker *webhookWaker
	evalLoop     *evalLoopState
	// snapshotPatch applies webhook re-reads to lastActionable so wake
	// cycles plan from a patched snapshot instead of re-listing.
	snapshotPatch *snapshotPatcher

	// sentinelEvents serializes the per-PR sentinel evaluations that
	// `pull_request` webhooks request (#13379).
	sentinelEvents *sentinelEventQueue
	// forgeView is the owner/name view over forgeState the automerge lanes
	// read and sweepTrigger their webhook trigger over it (#13377).
	forgeView    *github.ForgeStateView
	sweepTrigger *automerge.SweepTrigger
	// cruxSupervision is the sidecar runtime's child supervisor and probe
	// loop (hivecommons/hive#14138 stage 4); nil in-process.
	cruxSupervision *cruxRuntimeSupervision
}

// deferStack stands in for the `defer` statements that used to sit in
// main(). A phase pushes what it would have deferred; main() defers run()
// once, so everything still fires LIFO when main() returns — a phase's own
// return does not trigger it, and os.Exit still skips it, exactly as before.
type deferStack struct {
	fns []func()
}

// push registers fn to run when main() returns. A nil fn is ignored.
func (d *deferStack) push(fn func()) {
	if fn == nil {
		return
	}
	d.fns = append(d.fns, fn)
}

// run calls the pushed functions last-in first-out, the order `defer` uses.
func (d *deferStack) run() {
	for i := len(d.fns) - 1; i >= 0; i-- {
		d.fns[i]()
	}
}

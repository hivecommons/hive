package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/mention"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/proxy"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/tokens"
	"github.com/hivecommons/hive/pkg/toolapprove"
	"github.com/hivecommons/hive/pkg/trajectory"
	"github.com/hivecommons/hive/pkg/watchdog"
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
	agentMgr                      *agent.Manager
	approvalDesk                  *toolapprove.Desk
	approvalInbox                 *toolapprove.Inbox
	dashSrv                       *dashboard.Server
	beadStores                    map[string]*beads.Store
	beadStoreLoadFailures         int
	tokenCollector                *tokens.Collector
	metricsCollector              *dashboard.MetricsCollector
	fleetStatsCollector           *collect.FleetStatsCollector
	activityCollector             *collect.ActivityCollector
	repoCostCollector             *collect.RepoCostCollector
	lastActionable                atomic.Pointer[github.ActionableResult]
	knowledgeAPI                  *knowledge.KnowledgeAPI
	beadSynth                     *knowledge.BeadSynthesizer
	nousState                     *dashboard.NousState
	inceptionEngine               *knowledge.InceptionEngine
	rotationMgr                   *rotation.Manager
	wd                            *watchdog.Reconciler
	onDemandFromPack              map[string]bool
	mentionWebhook                http.Handler
	mentionStore                  *mention.Store
	refreshDashboard              func()
	mutationBoundary              effects.Boundary
	heartbeatFleetStats           func() (*int, *int, *int, string)
	heartbeatRepoActivity         func() ([]spoke.RepoActivityWire, string, int, int)
	heartbeatBudgetWindow         func() (*int64, *int64, *bool, string, string)
	heartbeatACMMLevel            func() int
	heartbeatAgents               func(govState governor.State, currentMode string, includeBlockingChecks bool) []spoke.AgentSummary
	dashboardURLForFreshHeartbeat func() string
	leaderboardForHeartbeat       func() []spoke.LeaderboardEntry
	ownerForHeartbeat             func() string
	dashboardURLForHeartbeat      func() string
	installMutationBoundary       func(client interface{ SetMutationBoundary(effects.Boundary) })
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
	pendingBudgetWindowSeed    []collect.BudgetWindowEntry
	pendingConvergenceSoakSeed []dashboard.ConvergenceSoakEntry
	pendingTrendSeed           []dashboard.TrendHistoryEntry
	primer                     *knowledge.Primer

	// bootAdvisory
	acmmLevel         int
	githubAppDiag     string
	githubAppState    github.AppAuthState
	githubAppRequired bool
	policyDirPath     string
	projectCtx        agent.ProjectContext
	mutationStats     *effects.Recorder

	// bootAgents
	agentMinter agent.AgentMintIssuer

	// bootState
	saved *snapshot.PersistedState

	// bootKnowledge
	gitSyncer            *knowledge.GitSyncer
	promotionScheduler   *knowledge.PromotionScheduler
	quotaAccount         string
	quotaPoolDir         string
	explicitQuotaPoolDir bool

	// bootSupervision
	quotaReadingPublisher    *rotation.Manager
	linearCredentialResolver func() agent.LinearCredential

	// bootWatchers
	configWatcher *config.Watcher

	// bootProxy
	githubProxy *proxy.GitHubProxy

	// bootHeartbeat
	reporterName     string
	processStartedAt time.Time
	hubURL           string

	// bootLanes
	trajLane   *trajectory.Lane
	replanLane *planning.ReplanLane
	retroLane  *retro.Lane

	// runLoop
	lastAutoMergeSweep time.Time
	lastTaskListSweep  time.Time
	lastDuplicateSweep time.Time
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

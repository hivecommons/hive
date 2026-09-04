package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	// automaxprocs sets GOMAXPROCS to match the container's CPU quota (Linux
	// CFS) at init. Without it the Go runtime sizes its P count to the whole
	// NODE's core count, so on a many-core IKS worker a pod limited to a few
	// CPUs spawns far more runnable Ps than its CFS quota can service; when the
	// quota is exhausted mid-period EVERY goroutine — including the netpoller
	// that answers the :3002 liveness probe and the heartbeat loop — is
	// throttled until the next CFS period, which stacks on top of the NFS
	// stalls to push probe latency past the kubelet timeout. Matching GOMAXPROCS
	// to the quota removes that self-inflicted throttling.
	//
	// This is called explicitly rather than via the package's blank import
	// because that import's init writes a line to the default logger (stderr)
	// unconditionally. `hive` re-execs itself as a Git transport shim, and the
	// setup path captures a child's stdout and stderr into a single buffer to
	// parse (e.g. `symbolic-ref --short origin/HEAD`), so an init-time banner
	// is indistinguishable from Git's answer and corrupts the parsed branch
	// name. Setting it with a no-op logger keeps the GOMAXPROCS behaviour and
	// drops the banner.

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/dashboard/collect"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/mint"
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

// wireSpokeSubsystems runs the spoke-mode subsystem wiring after the common
// process, config path, logging, and hub-mode preamble has completed. It keeps
// the original startup order byte-for-byte inside the spoke path while main()
// becomes an ordered dispatcher. Follow-up phase-2c PRs should peel contiguous
// sections from this function into narrower wireX constructors.
type spokeWire struct {
	configPath                 string
	logger                     *slog.Logger
	startTime                  time.Time
	cfg                        *config.Config
	ctx                        context.Context
	cancel                     context.CancelFunc
	preShutdownHooks           shutdownHooks
	ghAuth                     githubAuth
	ghClient                   *github.Client
	appAuth                    *github.AppAuth
	appAuthFailure             string
	appAuthState               github.AppAuthState
	gov                        *governor.Governor
	sched                      *scheduler.Scheduler
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
	notifier                   *notify.Notifier
	acmmLevel                  int
	githubAppDiag              string
	githubAppState             github.AppAuthState
	githubAppRequired          bool
	repoTargetMisconfigured    func() bool
	repoTargetIssueMessage     func() string
	advisoryIssues             map[string]int
	advisoryStore              *advisory.Store
	policyDir                  string
	projectCtx                 agent.ProjectContext
	agentMgr                   *agent.Manager
	approvalDesk               *toolapprove.Desk
	approvalInbox              *toolapprove.Inbox
	agentMinter                *mint.AgentMinter
	saved                      *snapshot.PersistedState
	dashSrv                    *dashboard.Server
	beadStores                 map[string]*beads.Store
	beadStoreLoadFailures      int
	tokenCollector             *tokens.Collector
	metricsCollector           *dashboard.MetricsCollector
	fleetStatsCollector        *collect.FleetStatsCollector
	activityCollector          *collect.ActivityCollector
	repoCostCollector          *collect.RepoCostCollector
	lastActionable             atomic.Pointer[github.ActionableResult]
	knowledgeAPI               *knowledge.KnowledgeAPI
	gitSyncer                  *knowledge.GitSyncer
	beadSynth                  *knowledge.BeadSynthesizer
	promotionScheduler         *knowledge.PromotionScheduler
	nousState                  *dashboard.NousState
	inceptionEngine            *knowledge.InceptionEngine
	rotationMgr                *rotation.Manager
	wd                         *watchdog.Reconciler
	configWatcher              *config.Watcher
	onDemandFromPack           map[string]bool
	githubProxy                *proxy.GitHubProxy
	reporterName               string
	processStartedAt           time.Time
	lastAutoMergeSweep         time.Time
	refreshDashboard           func()
	hubURL                     string
	trajLane                   *trajectory.Lane
	replanLane                 *planning.ReplanLane
	retroLane                  *retro.Lane
	cleanups                   []func()
}

const spokeStatePath = "/data/hive-state.json"

func wireSpokeSubsystems(configPath string, logger *slog.Logger, startTime time.Time) {
	w := &spokeWire{configPath: configPath, logger: logger, startTime: startTime}
	defer w.runCleanups()
	w.run()
}

func (w *spokeWire) addCleanup(fn func()) {
	if fn != nil {
		w.cleanups = append(w.cleanups, fn)
	}
}

func (w *spokeWire) runCleanups() {
	for i := len(w.cleanups) - 1; i >= 0; i-- {
		w.cleanups[i]()
	}
}

func (w *spokeWire) run() {
	w.wireSpokeConfigAndSignals()
	w.wireSpokeAuthGovernor()
	w.wireSpokeAgentsAndRequests()
	w.wireSpokeStateDashboard()
	w.wireSpokeMetricsAndKnowledgeAPI()
	w.wireSpokeKnowledgeSources()
	w.wireSpokeManagersAndLinear()
	w.wireSpokeAppCallbacks()
	w.wireSpokeConfigReloadAndHooks()
	w.wireSpokeProxyReadyAndLaunch()
	w.wireHubHeartbeat()
	w.wireSpokeLanesAndLoop()
}

package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/defsrc"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/notify"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/rotation"
	"github.com/hivecommons/hive/pkg/scheduler"
	"github.com/hivecommons/hive/pkg/snapshot"
	"github.com/hivecommons/hive/pkg/tokens"
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
//
// Step 2 gives each phase a deps struct for the collaborators that touch the
// process, the network, tmux or /data, so a test can drive the phase against
// fakes: bootConfigWith(bootConfigDeps) is the first (boot_config_deps.go);
// the pattern is the same as hubDeps/runHubWithDeps.
type boot struct {
	// cleanup collects what main() used to `defer` so it still runs LIFO on
	// main()'s return, not on the return of the phase that registered it.
	cleanup deferStack

	// bootConfig
	startTime               time.Time
	configPath              string
	cfg                     *config.Config
	logger                  *slog.Logger
	ctx                     context.Context
	preShutdownHooks        shutdownHooks
	repoTargetMisconfigured func() bool
	repoTargetIssueMessage  func() string

	// bootGitHub. ghClient and appAuth are reassigned at runtime by the
	// closures named in the type comment; read them through b, never copy
	// them into a local that a closure could capture.
	ghAuth         githubAuth
	ghClient       *github.Client
	appAuth        *github.AppAuth
	appAuthFailure string
	appAuthState   github.AppAuthState

	// bootGovernor
	gov                        *governor.Governor
	sched                      *scheduler.Scheduler
	definitionResolver         *defsrc.Resolver
	pendingTokenSeed           []dashboard.TokenSparklineEntry
	pendingFactSeed            []dashboard.FactHistoryEntry
	pendingCostSeed            []dashboard.CostHistoryEntry
	pendingBudgetWindowSeed    []dashboard.BudgetWindowEntry
	pendingConvergenceSoakSeed []dashboard.ConvergenceSoakEntry
	pendingTrendSeed           []dashboard.TrendHistoryEntry

	// bootAdvisory
	notifier          *notify.Notifier
	acmmLevel         int
	githubAppRequired bool
	githubAppDiag     string
	githubAppState    github.AppAuthState
	advisoryIssues    map[string]int
	advisoryStore     *advisory.Store
	policyDirPath     string

	// bootAgents
	agentMgr *agent.Manager

	// bootState
	saved *snapshot.PersistedState

	// bootDashboard
	dashSrv *dashboard.Server

	// bootStores. beadStores is captured by closures bootDashboard installs
	// before bootStores runs; they read it through b so they see the map once
	// it exists, as they saw the local once it was assigned.
	beadStores            map[string]*beads.Store
	beadStoreLoadFailures int

	// bootCollectors. lastActionable is addressed in place (&b.lastActionable)
	// because atomic.Pointer must not be copied.
	tokenCollector      *tokens.Collector
	metricsCollector    *dashboard.MetricsCollector
	fleetStatsCollector *dashboard.FleetStatsCollector
	activityCollector   *dashboard.ActivityCollector
	repoCostCollector   *dashboard.RepoCostCollector
	lastActionable      atomic.Pointer[github.ActionableResult]
	refreshDashboard    func()

	// bootKnowledge
	knowledgeAPI    *knowledge.KnowledgeAPI
	beadSynth       *knowledge.BeadSynthesizer
	nousState       *dashboard.NousState
	inceptionEngine *knowledge.InceptionEngine

	// bootSupervision
	rotationMgr              *rotation.Manager
	wd                       *watchdog.Reconciler
	linearCredentialResolver func() agent.LinearCredential

	// bootLaunch
	onDemandFromPack map[string]bool

	// bootLanes
	trajLane   *trajectory.Lane
	replanLane *planning.ReplanLane
	retroLane  *retro.Lane
}

// hiveStatePath is the persisted state snapshot on the /data PVC, read by
// bootState and rewritten by persistState after every eval.
const hiveStatePath = "/data/hive-state.json"

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

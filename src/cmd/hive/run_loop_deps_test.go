package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/cruxtest"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/watchdog"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/governor"
)

// fakeTicker is a loopTicker the test fires by hand.
type fakeTicker struct {
	ch     chan time.Time
	period time.Duration
	resets []time.Duration
	stops  int
}

func (t *fakeTicker) Chan() <-chan time.Time { return t.ch }
func (t *fakeTicker) Stop()                  { t.stops++ }
func (t *fakeTicker) Reset(d time.Duration)  { t.resets = append(t.resets, d) }
func (t *fakeTicker) fire()                  { t.ch <- time.Now() }
func newFakeTicker() *fakeTicker             { return &fakeTicker{ch: make(chan time.Time)} }

// loopHarness records the per-tick work units runLoop delegates to. Tickers
// are pre-built and handed out in creation order (governor, then agent) so
// the test holds them before the loop starts; every fire() is an unbuffered
// send that rendezvous with the loop's select, so no polling is needed.
type loopHarness struct {
	pending   []*fakeTicker
	tickers   []*fakeTicker
	evals     [][]string
	sources   []evalCycleSource
	rotations int
	sweeps    int
	persists  int
	crashed   []string // returned from restartCrashed on the next tick
	startupOK bool
	persisted chan struct{} // one send per persist; the test's sync point
}

func (h *loopHarness) deps() runLoopDeps {
	return runLoopDeps{
		waitCLIStartup: func(context.Context) bool { return h.startupOK },
		newTicker: func(d time.Duration) loopTicker {
			t := h.pending[0]
			h.pending = h.pending[1:]
			t.period = d
			h.tickers = append(h.tickers, t)
			return t
		},
		runEval: func(_ *boot, restarted []string, source evalCycleSource) {
			h.evals = append(h.evals, restarted)
			h.sources = append(h.sources, source)
		},
		runRotation: func(*boot) { h.rotations++ },
		startSweeps: func(*boot) func() { h.sweeps++; return func() {} },
		persist: func(*boot) {
			h.persists++
			if h.persisted != nil {
				h.persisted <- struct{}{}
			}
		},
		restartCrashed: func(context.Context, crux.Runtime) []string {
			out := h.crashed
			h.crashed = nil
			return out
		},
	}
}

func newLoopBoot(t *testing.T, evalS, agentPollS int) (*boot, *strings.Builder, context.CancelFunc) {
	t.Helper()
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"alpha": {Enabled: true}}}}
	cfg.Governor.EvalIntervalS = evalS
	cfg.Dashboard.AgentPollIntervalS = agentPollS
	b, _ := newDepsTestBoot(t, cfg)
	var sb strings.Builder
	b.logger = slog.New(slog.NewTextHandler(&sb, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.ctx = ctx
	b.gov = governor.New(cfg.Governor, cfg.EnabledAgents(), b.logger)
	b.agentMgr = cruxtest.NewRuntime(cfg.Agents, b.logger, crux.ProjectContext{})
	b.dashSrv = dashboard.NewServer(0, b.logger)
	return b, &sb, cancel
}

// runLoopAsync runs the loop and returns a channel closed when it exits.
func runLoopAsync(b *boot, deps runLoopDeps) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.runLoopWith(deps)
	}()
	return done
}

func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLoop did not exit")
	}
}

func TestRunLoopWithReturnsWhenShutdownDuringCLIStartup(t *testing.T) {
	b, _, _ := newLoopBoot(t, 60, 0)
	h := &loopHarness{startupOK: false, pending: []*fakeTicker{newFakeTicker()}}
	b.runLoopWith(h.deps())
	if len(h.evals) != 0 || h.persists != 0 {
		t.Fatalf("no work expected before CLI startup: evals=%v persists=%d", h.evals, h.persists)
	}
	if len(h.tickers) != 1 || h.tickers[0].period != 60*time.Second || h.tickers[0].stops != 1 {
		t.Fatalf("governor ticker = %+v", h.tickers)
	}
}

func TestRunLoopWithRunsFirstCycleThenTicks(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 60, 5)
	gov, agentTick := newFakeTicker(), newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov, agentTick}, persisted: make(chan struct{}, 8)}
	done := runLoopAsync(b, h.deps())
	<-h.persisted // first cycle done; the loop is parked on its select

	// Crash recovery on the next tick feeds the eval cycle and audits restarts.
	h.crashed = []string{"alpha"}
	b.cfg.Governor.EvalIntervalS = 30 // changed from the dashboard mid-run
	gov.fire()
	agentTick.fire() // exercised for coverage; broadcast has no failure path
	cancel()
	waitDone(t, done)

	if gov.period != 60*time.Second || agentTick.period != 5*time.Second {
		t.Fatalf("ticker periods = %v/%v", gov.period, agentTick.period)
	}
	if len(h.evals) != 2 || h.evals[0] != nil || strings.Join(h.evals[1], ",") != "alpha" {
		t.Fatalf("evals = %v", h.evals)
	}
	if h.rotations != 2 || h.sweeps != 1 {
		t.Fatalf("rotations=%d sweep workers started=%d, want 2/1", h.rotations, h.sweeps)
	}
	// first cycle + tick + shutdown
	if h.persists != 3 {
		t.Fatalf("persists = %d, want 3", h.persists)
	}
	if len(gov.resets) != 1 || gov.resets[0] != 30*time.Second {
		t.Fatalf("ticker resets = %v, want one 30s reset", gov.resets)
	}
	if gov.stops != 1 || agentTick.stops != 1 {
		t.Fatal("both tickers must be stopped on exit")
	}
	acts := auditActions(b.dashSrv)
	if len(acts["restart"]) != 1 || acts["restart"][0].Agent != "alpha" || acts["restart"][0].Detail != "trigger=crash-recovery" {
		t.Fatalf("restart audit = %+v", acts["restart"])
	}
	for _, want := range []string{"entering governor loop", "fast agent status enabled", "eval interval changed", "shutting down, persisting state"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("missing %q in log:\n%s", want, log.String())
		}
	}
}

// hivecommons/hive#17467: a hot-reload delivered while a governor tick holds
// cfgReloadMu must not wait for the tick and must be applied before that
// tick's persist, so the persist saves the reloaded config (not the one the
// tick started from) and the governor state is not deferred a tick. The
// shutdown persist also saves the reloaded config.
func TestRunLoopWithAppliesMidCycleReloadWithoutClobbering(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 60, 0)
	b.reloadApply = func(newCfg *config.Config) (int, int) {
		*b.cfg = *newCfg
		return 0, 0
	}
	gov := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	deps := h.deps()

	var persistedRepos [][]string
	basePersist := deps.persist
	deps.persist = func(b *boot) {
		persistedRepos = append(persistedRepos, append([]string(nil), b.cfg.Project.Repos...))
		basePersist(b)
	}
	baseEval := deps.runEval
	deps.runEval = func(b *boot, restarted []string, source evalCycleSource) {
		baseEval(b, restarted, source)
		if len(h.evals) != 2 {
			return
		}
		// The watcher fires on its own goroutine while this tick holds
		// cfgReloadMu; it must return without waiting for the tick.
		newCfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{Agents: map[string]config.AgentConfig{"alpha": {Enabled: true}}}}
		newCfg.Governor.EvalIntervalS = 60
		newCfg.Project.Repos = []string{"reloaded"}
		submitted := make(chan struct{})
		go func() {
			b.submitConfigReload(newCfg)
			close(submitted)
		}()
		select {
		case <-submitted:
		case <-time.After(5 * time.Second):
			t.Error("config reload blocked behind the governor tick")
		}
		if sameStringSlice(b.cfg.Project.Repos, []string{"reloaded"}) {
			t.Error("reload mutated cfg while the eval cycle was running")
		}
	}

	done := runLoopAsync(b, deps)
	<-h.persisted // startup cycle
	gov.fire()    // reload lands mid-tick and is applied before its persist
	<-h.persisted
	cancel()
	waitDone(t, done)

	if !sameStringSlice(b.cfg.Project.Repos, []string{"reloaded"}) {
		t.Fatalf("repos = %v, want the reloaded config applied", b.cfg.Project.Repos)
	}
	// startup cycle, the tick the reload landed in, shutdown.
	if len(persistedRepos) != 3 || len(persistedRepos[0]) != 0 ||
		!sameStringSlice(persistedRepos[1], []string{"reloaded"}) || !sameStringSlice(persistedRepos[2], []string{"reloaded"}) {
		t.Fatalf("persisted repos per save = %v, want [[] [reloaded] [reloaded]]", persistedRepos)
	}
	if !strings.Contains(log.String(), "config reload deferred") {
		t.Fatalf("deferred reload not logged:\n%s", log.String())
	}
}

// hivecommons/hive#17467: the shutdown persist takes cfgReloadMu, so it cannot
// save while a reload is being applied.
func TestRunLoopWithShutdownPersistTakesConfigLock(t *testing.T) {
	b, _, cancel := newLoopBoot(t, 60, 0)
	gov := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	done := runLoopAsync(b, h.deps())
	<-h.persisted // startup cycle

	b.lockCfg()
	cancel()
	select {
	case <-h.persisted:
		b.unlockCfg()
		t.Fatal("shutdown persist ran while cfgReloadMu was held")
	case <-time.After(100 * time.Millisecond):
	}
	b.unlockCfg()
	select {
	case <-h.persisted:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown persist did not run after cfgReloadMu was released")
	}
	waitDone(t, done)
}

// staticFleet is a watchdog.Fleet with no agents, so Tick is a no-op and the
// test only exercises runLoop's mode re-resolution.
type staticFleet struct{}

func (staticFleet) AgentNames() []string                         { return nil }
func (staticFleet) Observe(string) (watchdog.Observation, error) { return watchdog.Observation{}, nil }
func (staticFleet) IsPaused(string) bool                         { return false }
func (staticFleet) Restart(context.Context, string) error        { return nil }
func (staticFleet) Pause(string, string, string) error           { return nil }
func (staticFleet) LastProduction(string) (time.Time, bool)      { return time.Time{}, false }
func (staticFleet) QueuedWork(string) (int, bool)                { return 0, false }
func (staticFleet) SetConditions(string, []watchdog.Condition)   {}

func TestRunLoopWithReappliesWatchdogModeOnTick(t *testing.T) {
	b, log, cancel := newLoopBoot(t, 60, 0)
	obs, _ := watchdog.SettingsFrom(config.WatchdogConfig{Mode: string(watchdog.ModeObserve)})
	b.wd = watchdog.New(obs, staticFleet{}, nil, b.logger)
	gov := newFakeTicker()
	h := &loopHarness{startupOK: true, pending: []*fakeTicker{gov}, persisted: make(chan struct{}, 8)}
	done := runLoopAsync(b, h.deps())
	<-h.persisted

	b.cfg.Governor.Watchdog.Mode = string(watchdog.ModeHeal)
	gov.fire()
	cancel()
	waitDone(t, done)

	if b.wd.Mode() != watchdog.ModeHeal {
		t.Fatalf("watchdog mode = %s, want heal after config change", b.wd.Mode())
	}
	acts := auditActions(b.dashSrv)
	if len(acts["watchdog-mode"]) != 1 || acts["watchdog-mode"][0].Detail != "from=observe, to=heal" {
		t.Fatalf("watchdog-mode audit = %+v", acts["watchdog-mode"])
	}
	if !strings.Contains(log.String(), "watchdog mode changed") {
		t.Fatalf("missing mode-change log:\n%s", log.String())
	}
}

func TestDefaultRunLoopDepsWaitCLIStartupHonorsContext(t *testing.T) {
	deps := defaultRunLoopDeps()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if deps.waitCLIStartup(ctx) {
		t.Fatal("waitCLIStartup must return false once ctx is done")
	}
	tk := deps.newTicker(time.Hour)
	if tk.Chan() == nil {
		t.Fatal("real ticker must expose its channel")
	}
	tk.Reset(time.Hour)
	tk.Stop()
}

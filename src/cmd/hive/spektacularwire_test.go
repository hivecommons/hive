package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/planengine"
	"github.com/hivecommons/hive/pkg/pushbroker"
)

func TestWireSpektacularRunner_DefaultOffAndOptIn(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	if wireSpektacularRunner(&config.Config{}, srv, logger) {
		t.Fatal("runner installed with the feature off")
	}
	if wireSpektacularRunner(nil, srv, logger) || wireSpektacularRunner(&config.Config{}, nil, logger) {
		t.Fatal("nil inputs installed a runner")
	}
	cfg := &config.Config{Runs: config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true, Binary: "false"}}}
	if !wireSpektacularRunner(cfg, srv, logger) {
		t.Fatal("runner not installed with the feature on")
	}
	if !wireSpektacularRunner(cfg, srv, nil) {
		t.Fatal("nil logger prevented installation")
	}
}

func TestDefaultAgentBackendDeterministic(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"zeta":  {Enabled: true, Backend: "claude"},
		"alpha": {Enabled: true, Backend: "copilot"},
		"off":   {Enabled: false, Backend: "bob"},
	}}
	if got := defaultAgentBackend(cfg); got != "copilot" {
		t.Fatalf("defaultAgentBackend = %q, want alphabetically first enabled backend", got)
	}
	if got := defaultAgentBackend(&config.Config{}); got != config.DefaultSpektacularHubExecutorBackend {
		t.Fatalf("empty backend = %q", got)
	}
}

// busyExecutor stands in for a hub executor mid-stage.
type busyExecutor struct{ running int }

func (b *busyExecutor) Tick(context.Context, time.Time) {}
func (b *busyExecutor) Status() dashboard.FrontendSpektacularHubExecutor {
	return dashboard.FrontendSpektacularHubExecutor{Running: b.running}
}

// #9172: toggling Spektacular after boot installs / removes the runner, the
// executor and the binary probe instead of waiting for the next boot.
func TestRewireSpektacular_EnableDisableAfterBoot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	cfg := &config.Config{}
	wireSpektacularRunner(cfg, srv, logger) // boot with the feature off

	hubOn := true
	cfg.Runs.Spektacular = config.SpektacularConfig{Enabled: true, Binary: "false", HubExecutor: config.SpektacularHubExecutorConfig{Enabled: &hubOn}}
	if !rewireSpektacular(cfg, srv, logger, nil) {
		t.Fatal("enable was not applied live")
	}
	if srv.StageRunner() == nil || srv.StageExecutor() == nil || srv.SpektacularStatus() == nil {
		t.Fatalf("after enable: runner=%v executor=%v status=%v", srv.StageRunner(), srv.StageExecutor(), srv.SpektacularStatus())
	}

	// The executor snapshots cfg.Runs, so a runs change rebuilds an idle one
	// with the new settings...
	before := srv.StageExecutor()
	cfg.Runs.Spektacular.HubExecutor.MaxConcurrent = 3
	if !rewireSpektacular(cfg, srv, logger, nil) {
		t.Fatal("hub executor change was not applied live")
	}
	exec, ok := srv.StageExecutor().(*dashboard.SpekHubExecutor)
	if !ok || dashboard.StageExecutor(exec) == before || exec.Config.Spektacular.HubExecutor.MaxConcurrent != 3 {
		t.Fatalf("executor not rebuilt with the new settings: %+v", srv.StageExecutor())
	}
	// ...while an identical config keeps it, with its failure counters.
	if !rewireSpektacular(cfg, srv, logger, nil) || srv.StageExecutor() != dashboard.StageExecutor(exec) {
		t.Fatal("identical config replaced the executor")
	}
	// hivecommons/hive#10069: the keep path must not call Stop() on the
	// executor it means to keep running.
	if exec.Stopped() {
		t.Fatal("identical config stopped the kept executor")
	}

	cfg.Runs.Spektacular.Enabled = false
	if !rewireSpektacular(cfg, srv, logger, nil) {
		t.Fatal("disable was not applied live")
	}
	if srv.StageRunner() != nil || srv.StageExecutor() != nil || srv.SpektacularStatus() != nil {
		t.Fatalf("after disable: runner=%v executor=%v status=%v", srv.StageRunner(), srv.StageExecutor(), srv.SpektacularStatus())
	}
}

// #10069: a rewire that finds the installed hub executor already up to date
// (runs config reverted to the executor's build-time snapshot) kept the same
// executor installed but left it stopped, because the keep path replaced it
// with a freshly built one (stopping the kept executor as the old value) and
// then restored the kept one as the new value (stopping the freshly built
// one instead) — the installed pointer survived but Stop() had already fired
// on it. It must still be running and able to launch stages afterward.
func TestRewireSpektacular_KeepPathDoesNotStopExecutor(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	cfg := &config.Config{}
	wireSpektacularRunner(cfg, srv, logger) // boot with the feature off

	hubOn := true
	cfg.Runs.Spektacular = config.SpektacularConfig{Enabled: true, Binary: "false", HubExecutor: config.SpektacularHubExecutorConfig{Enabled: &hubOn}}
	if !rewireSpektacular(cfg, srv, logger, nil) {
		t.Fatal("enable was not applied live")
	}
	exec, ok := srv.StageExecutor().(*dashboard.SpekHubExecutor)
	if !ok {
		t.Fatalf("executor not installed: %v", srv.StageExecutor())
	}

	// Repeated rewires with the same, already-up-to-date config must keep
	// reaching the keep path without ever stopping the installed executor.
	for i := 0; i < 3; i++ {
		if !rewireSpektacular(cfg, srv, logger, nil) {
			t.Fatalf("rewire %d: identical config was not applied", i)
		}
		if srv.StageExecutor() != dashboard.StageExecutor(exec) {
			t.Fatalf("rewire %d: identical config replaced the executor", i)
		}
		if exec.Stopped() {
			t.Fatalf("rewire %d: keep path stopped the kept executor", i)
		}
	}
}

// ADR-0022 AC-4(d): with runs.engine unset the default engine is wired and
// named on the status card; an unknown runs.engine installs no stage runner
// at boot or on a live rewire, and never falls back to another engine.
func TestWirePlanningEngine_UnknownEngineInstallsNoRunner(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	cfg := &config.Config{Runs: config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true, Binary: "false"}}}
	if !wireSpektacularRunner(cfg, srv, logger) || srv.StageRunner() == nil {
		t.Fatal("default engine: runner not installed")
	}
	if st := srv.SpektacularStatus(); st == nil || st.Engine != config.DefaultRunsEngine || st.Binary != "false" {
		t.Fatalf("default engine status = %+v", st)
	}

	cfg.Runs.Engine = "no-such-engine"
	if !rewireSpektacular(cfg, srv, logger, nil) {
		t.Fatal("unknown engine rewire was not applied")
	}
	if srv.StageRunner() != nil {
		t.Fatalf("unknown engine left a stage runner installed: %v", srv.StageRunner())
	}

	fresh := dashboard.NewServer(0, logger)
	if wirePlanningEngine(cfg, fresh, logger, nil, true) {
		t.Fatal("unknown engine reported a runner installed at boot")
	}
	if fresh.StageRunner() != nil || fresh.StageExecutor() != nil {
		t.Fatalf("unknown engine at boot: runner=%v executor=%v", fresh.StageRunner(), fresh.StageExecutor())
	}
}

// #9172: a busy hub executor is never swapped out from under its running
// stage; the change is deferred until it is idle.
func TestRewireSpektacular_DefersWhileExecutorBusy(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	busy := &busyExecutor{running: 1}
	srv.SetStageExecutor(busy)
	cfg := &config.Config{}

	if rewireSpektacular(cfg, srv, logger, nil) {
		t.Fatal("busy executor: rewire reported applied")
	}
	if srv.StageExecutor() != dashboard.StageExecutor(busy) {
		t.Fatal("busy executor was replaced")
	}
	busy.running = 0
	if !rewireSpektacular(cfg, srv, logger, nil) || srv.StageExecutor() != nil {
		t.Fatal("idle executor was not removed once the stage finished")
	}
}

type stubCloneMinter struct{ token string }

func (m stubCloneMinter) MintPushToken(context.Context, string) (string, error) { return m.token, nil }

func TestLazySpektacularCloneAuthResolvesMinterPerLaunch(t *testing.T) {
	var current pushbroker.TokenMinter
	auth := lazySpektacularCloneAuth(func() pushbroker.TokenMinter { return current })
	dir := t.TempDir()

	args, token, cleanup, err := auth(context.Background(), "o/r", dir)
	if err != nil || args != nil || token != "" {
		t.Fatalf("no minter: args=%v token=%q err=%v, want anonymous clone", args, token, err)
	}
	cleanup()

	current = stubCloneMinter{token: "tok-1"}
	args, token, cleanup, err = auth(context.Background(), "o/r", dir)
	if err != nil || token != "tok-1" || len(args) != 2 {
		t.Fatalf("minter arrived late: args=%v token=%q err=%v", args, token, err)
	}
	cleanup()

	current = stubCloneMinter{token: "tok-2"}
	_, token, cleanup, err = auth(context.Background(), "o/r", dir)
	if err != nil || token != "tok-2" {
		t.Fatalf("rebuilt minter not used: token=%q err=%v", token, err)
	}
	cleanup()
}

// probeFailureEngine is a planning engine whose binary is not installed: it
// answers every document question, but Probe reports the failure.
type probeFailureEngine struct{ name string }

func (e probeFailureEngine) Name() string             { return e.name }
func (e probeFailureEngine) ContractRevision() string { return e.name + "-status/v1" }

func (e probeFailureEngine) Probe(context.Context) (planengine.ProbeResult, error) {
	return planengine.ProbeResult{Binary: e.name}, errors.New("executable file not found in $PATH")
}

func (probeFailureEngine) Status(context.Context, string, string, string) (planengine.ArtifactStatus, error) {
	return planengine.ArtifactStatus{}, nil
}

func (probeFailureEngine) ResolveArtifact(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (probeFailureEngine) ExportPlan(context.Context, string, string) (planengine.Plan, error) {
	return planengine.Plan{}, nil
}

func (probeFailureEngine) ReadSpec(context.Context, string, string) (string, error) { return "", nil }

// ADR-0022 AC-4(b), last row: an engine whose Probe fails is shown absent on
// the Extensions card, but the stage observer is still installed — a missing
// binary today must not leave the run stages unobserved when it appears.
func TestWirePlanningEngine_ProbeFailureStillInstallsTheObserver(t *testing.T) {
	const name = "probe-failure-engine"
	planengine.Register(name, func(config.RunsConfig, *slog.Logger) (planengine.Engine, error) {
		return probeFailureEngine{name: name}, nil
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	cfg := &config.Config{Runs: config.RunsConfig{
		Engine:      name,
		Spektacular: config.SpektacularConfig{Enabled: true, Binary: name},
	}}

	if !wirePlanningEngine(cfg, srv, logger, nil, false) {
		t.Fatal("a failing probe prevented the stage runner from being installed")
	}
	if srv.StageRunner() == nil {
		t.Fatal("no stage runner installed for an engine whose probe failed")
	}
	st := srv.SpektacularStatus()
	if st == nil || st.Engine != name || st.Present {
		t.Fatalf("status card = %+v, want the engine named and reported absent", st)
	}
}

// An engine the registry cannot build installs no runner and never falls
// back to another engine.
func TestWirePlanningEngine_BuildFailureInstallsNoRunner(t *testing.T) {
	const name = "build-failure-engine"
	planengine.Register(name, func(config.RunsConfig, *slog.Logger) (planengine.Engine, error) {
		return nil, errors.New("binary not configured")
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := dashboard.NewServer(0, logger)
	cfg := &config.Config{Runs: config.RunsConfig{
		Engine:      name,
		Spektacular: config.SpektacularConfig{Enabled: true, Binary: name},
	}}

	if wirePlanningEngine(cfg, srv, logger, nil, true) {
		t.Fatal("a failed engine build reported a runner installed")
	}
	if srv.StageRunner() != nil || srv.StageExecutor() != nil {
		t.Fatalf("failed build left runner=%v executor=%v", srv.StageRunner(), srv.StageExecutor())
	}
}

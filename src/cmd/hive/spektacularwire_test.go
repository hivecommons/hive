package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
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

	cfg.Runs.Spektacular.Enabled = false
	if !rewireSpektacular(cfg, srv, logger, nil) {
		t.Fatal("disable was not applied live")
	}
	if srv.StageRunner() != nil || srv.StageExecutor() != nil || srv.SpektacularStatus() != nil {
		t.Fatalf("after disable: runner=%v executor=%v status=%v", srv.StageRunner(), srv.StageExecutor(), srv.SpektacularStatus())
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

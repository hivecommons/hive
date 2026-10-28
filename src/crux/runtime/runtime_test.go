package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/client"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/crux/watchdog"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newInProcess(t *testing.T, defs map[string]config.AgentConfig) crux.Runtime {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	rt, err := cruxruntime.New(config.RuntimeConfig{},
		cruxruntime.WithDefinitions(defs),
		cruxruntime.WithLogger(quietLogger()),
		cruxruntime.WithProject(crux.ProjectContext{ACMMLevel: 2}),
		cruxruntime.WithUIDMapPath(filepath.Join(t.TempDir(), "uid-map.json")),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func TestNewDefaultsToInProcess(t *testing.T) {
	rt := newInProcess(t, map[string]config.AgentConfig{
		"fixture": {ID: "fixture-id", Enabled: true, Backend: "claude", Model: "sonnet"},
	})
	proc, err := rt.GetStatusFast("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if proc.State != crux.StateStopped || proc.Config.Model != "sonnet" {
		t.Fatalf("state/model = %s/%s, want stopped/sonnet", proc.State, proc.Config.Model)
	}
	host, ok := cruxruntime.HostOf(rt)
	if !ok || host == nil {
		t.Fatal("in-process runtime must expose crux.Host")
	}
	// Host wiring is accepted and takes effect through the Runtime surface.
	host.SetKickLogSourceResolver(func() string { return config.KickLogSourceTranscript })
	if _, isTest := rt.(cruxtest.Runtime); !isTest {
		t.Fatal("in-process runtime should also satisfy the cruxtest fixture surface")
	}
	if got := rt.(cruxtest.Runtime).KickLogSource(); got != config.KickLogSourceTranscript {
		t.Fatalf("KickLogSource = %q after Host wiring, want transcript", got)
	}
}

func TestNewWithoutOptionsIsUsable(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	rt, err := cruxruntime.New(config.RuntimeConfig{Mode: config.ModeInProcess})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rt.AllStatuses()); n != 0 {
		t.Fatalf("empty runtime has %d agents", n)
	}
}

// TestNewSidecarDrivesAServer builds the sidecar runtime against a serve
// handler over a Recorder and checks calls land there, the Host seam is
// absent, and the watchdog fleet composes from the wire source.
func TestNewSidecarDrivesAServer(t *testing.T) {
	rec := cruxtest.NewRecorder()
	rec.Add("fixture", "fixture-id", crux.StateRunning)
	rec.WatchdogNames = []string{"fixture"}
	srv := serve.New(rec)
	srv.Token = "tok"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	rt, err := cruxruntime.New(config.RuntimeConfig{Mode: config.ModeSidecar, Server: ts.URL, Token: "tok"}, cruxruntime.WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.(*client.Runtime); !ok {
		t.Fatalf("New(sidecar) = %T, want *client.Runtime", rt)
	}
	if h, ok := cruxruntime.HostOf(rt); ok || h != nil {
		t.Fatalf("HostOf(sidecar) = %v, %v; want nil, false", h, ok)
	}
	if err := rt.Stop("fixture"); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Last("Stop"); !ok {
		t.Fatal("Stop did not reach the server")
	}
	if n := len(rt.AllStatuses()); n != 1 {
		t.Fatalf("AllStatuses over the wire = %d agents, want 1", n)
	}

	queued := 0
	fleet, err := cruxruntime.NewWatchdogFleet(rt, cruxruntime.WatchdogHooks{
		Queued:         func() (int, bool) { queued++; return 3, true },
		KickSuppressed: func(string) string { return "quiet" },
		CadencePeriod:  func(string) (time.Duration, bool) { return time.Hour, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := fleet.AgentNames(); len(got) != 1 || got[0] != "fixture" {
		t.Fatalf("AgentNames = %v", got)
	}
	if fleet.IsPaused("fixture") {
		t.Fatal("fresh agent reported paused")
	}
	if err := fleet.Pause("fixture", "watchdog", "test"); err != nil {
		t.Fatal(err)
	}
	if !fleet.IsPaused("fixture") {
		t.Fatal("pause over the wire not reflected")
	}
	if err := fleet.Restart(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.Last("Restart"); !ok {
		t.Fatal("Restart did not reach the server")
	}
	if n, ok := fleet.QueuedWork("fixture"); !ok || n != 3 || queued != 1 {
		t.Fatalf("QueuedWork = %d, %v (hook calls %d)", n, ok, queued)
	}
	if n, ok := fleet.QueuedWork("ghost"); ok || n != 0 {
		t.Fatalf("QueuedWork(unknown) = %d, %v; want 0, false", n, ok)
	}
	rec.Procs["fixture"].Config.Mode = "advisory"
	if n, ok := fleet.QueuedWork("fixture"); !ok || n != 0 || queued != 1 {
		t.Fatalf("QueuedWork(advisory) = %d, %v (hook calls %d); want 0, true, 1", n, ok, queued)
	}
	if got := fleet.(watchdog.KickSuppressionReporter).KickSuppressedReason("fixture"); got != "quiet" {
		t.Fatalf("KickSuppressedReason = %q", got)
	}
	if p, ok := fleet.(watchdog.KickPeriodReporter).KickPeriod("fixture"); !ok || p != time.Hour {
		t.Fatalf("KickPeriod = %v, %v", p, ok)
	}
	bare, err := cruxruntime.NewWatchdogFleet(rt, cruxruntime.WatchdogHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bare.QueuedWork("fixture"); ok {
		t.Fatal("QueuedWork without a hook must be unknown")
	}
	if got := bare.(watchdog.KickSuppressionReporter).KickSuppressedReason("fixture"); got != "" {
		t.Fatalf("KickSuppressedReason without a hook = %q", got)
	}
	if _, ok := bare.(watchdog.KickPeriodReporter).KickPeriod("fixture"); ok {
		t.Fatal("KickPeriod without a hook must be unknown")
	}
}

func TestNewSidecarRejectsBadServer(t *testing.T) {
	rt, err := cruxruntime.New(config.RuntimeConfig{Mode: config.ModeSidecar, Server: "://nope"})
	if rt != nil || err == nil || errors.Is(err, cruxruntime.ErrModeUnavailable) {
		t.Fatalf("New(sidecar, bad server) = %v, %v; want an address error", rt, err)
	}
	if !strings.Contains(err.Error(), config.ModeEnvVar) {
		t.Errorf("error %q should mention %s", err, config.ModeEnvVar)
	}
}

// nilSource has the watchdog seam but nothing behind it.
type nilSource struct{ crux.Runtime }

func (nilSource) WatchdogSource() watchdog.Source { return nil }

func TestNewWatchdogFleetNilSource(t *testing.T) {
	if _, err := cruxruntime.NewWatchdogFleet(nilSource{}, cruxruntime.WatchdogHooks{}); !errors.Is(err, cruxruntime.ErrNotInProcess) {
		t.Fatalf("NewWatchdogFleet(nil source) err = %v, want ErrNotInProcess", err)
	}
}

func TestNewRejectsUnknownMode(t *testing.T) {
	rt, err := cruxruntime.New(config.RuntimeConfig{Mode: "remote"})
	if rt != nil || err == nil || errors.Is(err, cruxruntime.ErrModeUnavailable) {
		t.Fatalf("New(remote) = %v, %v; want a validation error, not ErrModeUnavailable", rt, err)
	}
}

type notInProcess struct{ crux.Runtime }

func TestHostOfOtherImplementations(t *testing.T) {
	if h, ok := cruxruntime.HostOf(notInProcess{}); ok || h != nil {
		t.Fatalf("HostOf(non-manager) = %v, %v; want nil, false", h, ok)
	}
	if h, ok := cruxruntime.HostOf(nil); ok || h != nil {
		t.Fatalf("HostOf(nil) = %v, %v; want nil, false", h, ok)
	}
}

func TestNewWatchdogFleet(t *testing.T) {
	rt := newInProcess(t, map[string]config.AgentConfig{
		"fixture": {ID: "fixture-id", Enabled: true, Backend: "claude"},
	})
	var queuedCalls int
	fleet, err := cruxruntime.NewWatchdogFleet(rt, cruxruntime.WatchdogHooks{
		Queued:         func() (int, bool) { queuedCalls++; return 3, true },
		KickSuppressed: func(string) string { return "" },
		CadencePeriod:  func(string) (time.Duration, bool) { return time.Hour, true },
	})
	if err != nil {
		t.Fatal(err)
	}
	// A stopped agent is not reconciled (the fleet scopes to running ones).
	if names := fleet.AgentNames(); len(names) != 0 {
		t.Fatalf("AgentNames = %v for a stopped fleet, want none", names)
	}
	if fleet.IsPaused("fixture") {
		t.Fatal("fresh agent reported paused")
	}
	if _, err := cruxruntime.NewWatchdogFleet(notInProcess{}, cruxruntime.WatchdogHooks{}); !errors.Is(err, cruxruntime.ErrNotInProcess) {
		t.Fatalf("NewWatchdogFleet(non-manager) err = %v, want ErrNotInProcess", err)
	}
	if _, err := cruxruntime.NewWatchdogFleet(nil, cruxruntime.WatchdogHooks{}); !errors.Is(err, cruxruntime.ErrNotInProcess) {
		t.Fatalf("NewWatchdogFleet(nil) err = %v, want ErrNotInProcess", err)
	}
	_ = queuedCalls
}

func TestRunAgyTurnHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cruxruntime.RunAgyTurn([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Fatalf("RunAgyTurn(-h) = %d, stderr %q", code, stderr.String())
	}
}

type stubSharedSessionPool struct{}

func (stubSharedSessionPool) Acquire(context.Context, string) (string, error) { return "lease", nil }
func (stubSharedSessionPool) Renew(context.Context, string) error             { return nil }
func (stubSharedSessionPool) Release(context.Context, string) error           { return nil }

func TestWithSharedSessionPool(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	pool := stubSharedSessionPool{}
	rt, err := cruxruntime.New(config.RuntimeConfig{Mode: config.ModeInProcess},
		cruxruntime.WithSharedSessionPool(pool),
	)
	if err != nil {
		t.Fatalf("in-process with shared pool: %v", err)
	}
	if rt == nil {
		t.Fatal("expected non-nil runtime")
	}

	_, err = cruxruntime.New(config.RuntimeConfig{Mode: config.ModeSidecar, Server: "http://127.0.0.1:9", Token: "t"},
		cruxruntime.WithSharedSessionPool(pool),
	)
	if err == nil || !strings.Contains(err.Error(), "shared session pool is not supported") {
		t.Fatalf("sidecar with shared pool err = %v, want unsupported error", err)
	}
}

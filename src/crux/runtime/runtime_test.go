package runtime_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	cruxruntime "github.com/hivecommons/hive/crux/runtime"
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

func TestNewSidecarIsNotYetAvailable(t *testing.T) {
	rt, err := cruxruntime.New(config.RuntimeConfig{Mode: config.ModeSidecar, Server: "unix:///tmp/crux.sock"})
	if rt != nil || !errors.Is(err, cruxruntime.ErrModeUnavailable) {
		t.Fatalf("New(sidecar) = %v, %v; want nil, ErrModeUnavailable", rt, err)
	}
	for _, want := range []string{"sidecar", "unix:///tmp/crux.sock", "#14138", config.ModeEnvVar} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
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

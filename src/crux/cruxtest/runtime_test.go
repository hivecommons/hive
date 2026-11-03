package cruxtest_test

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
)

func TestNewRuntimeUsesSuppliedUIDMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		uid  int
	}{
		{name: "missing map"},
		{name: "isolated agent", uid: 2345},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "uid-map.json")
			if tc.uid != 0 {
				uids := cruxtest.NewUIDMap()
				uids.Agents["fixture"] = tc.uid
				if err := uids.Save(path); err != nil {
					t.Fatal(err)
				}
			}
			rt := cruxtest.NewRuntime(map[string]config.AgentConfig{
				"fixture": {ID: "fixture-id", Enabled: true, Backend: "claude", Model: "sonnet"},
			}, slog.New(slog.NewTextHandler(io.Discard, nil)), crux.ProjectContext{}, cruxtest.WithUIDMapPath(path))
			proc, err := rt.GetStatusFast("fixture")
			if err != nil {
				t.Fatal(err)
			}
			if proc.UID != tc.uid || proc.State != crux.StateStopped || proc.Config.Model != "sonnet" {
				t.Fatalf("UID/state/model = %d/%s/%s, want %d/%s/sonnet", proc.UID, proc.State, proc.Config.Model, tc.uid, crux.StateStopped)
			}
			if got := rt.ResolveAgent("fixture-id"); got != "fixture" {
				t.Fatalf("ResolveAgent(fixture-id) = %q, want fixture", got)
			}
		})
	}
}

func TestFixtureControlsShareRuntimeState(t *testing.T) {
	fixture := cruxtest.NewRuntime(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), crux.ProjectContext{},
		cruxtest.WithUIDMapPath(filepath.Join(t.TempDir(), "uid-map.json")))
	var rt crux.Runtime = fixture
	controls := rt.(cruxtest.Runtime)
	queuedAt := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	controls.RecordKickDispatchForTest(crux.KickDispatch{
		Agent: "fixture", Phase: crux.KickPhaseDelivered, QueuedAt: queuedAt,
	})
	got, ok := rt.KickDispatchState("fixture")
	if !ok || got.Phase != crux.KickPhaseDelivered || !got.QueuedAt.Equal(queuedAt) {
		t.Fatalf("dispatch through production surface = %+v/%v, want injected delivery", got, ok)
	}
}

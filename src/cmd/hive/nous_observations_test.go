package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/hivestate"
)

type nousObservationHarness struct {
	deps      nousObservationDeps
	logs      *bytes.Buffer
	logger    *slog.Logger
	detected  int
	filed     []string
	now       time.Time
	upgrading bool
}

func newNousObservationHarness(t *testing.T, found int, maxPerWeek int) *nousObservationHarness {
	t.Helper()
	dir := t.TempDir()
	h := &nousObservationHarness{logs: &bytes.Buffer{}, now: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
	h.logger = slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.deps = nousObservationDeps{
		enabled:     true,
		maxPerWeek:  maxPerWeek,
		hiveID:      "hive-x",
		snapshotDir: filepath.Join(dir, "snapshots"),
		budgetPath:  filepath.Join(dir, nousObservationBudgetFile),
		now:         func() time.Time { return h.now },
		upgradeWindow: func(time.Time) (string, bool) {
			if h.upgrading {
				return "abc123", true
			}
			return "", false
		},
		load: func(string, time.Time) ([]hivestate.Bucket, error) { return nil, nil },
		detect: func(_ []hivestate.Bucket, _ time.Time, hiveID string) []hivestate.Observation {
			h.detected++
			out := make([]hivestate.Observation, found)
			for i := range out {
				kind := fmt.Sprintf("kind-%d", i)
				out[i] = hivestate.Observation{Kind: kind, Fingerprint: hiveID + ":" + kind}
			}
			return out
		},
		sink: func(_ context.Context, obs hivestate.Observation) error {
			h.filed = append(h.filed, obs.Fingerprint)
			return nil
		},
	}
	return h
}

func (h *nousObservationHarness) run() int {
	return runNousObservations(context.Background(), h.logger, h.deps)
}

func TestNousObservationsDisabledRunsNothing(t *testing.T) {
	h := newNousObservationHarness(t, 2, 5)
	h.deps.enabled = false
	if got := h.run(); got != 0 || h.detected != 0 || len(h.filed) != 0 {
		t.Fatalf("disabled: filed=%d detected=%d sink=%v, want nothing", got, h.detected, h.filed)
	}
}

func TestNousObservationsDefaultConfigIsOff(t *testing.T) {
	d := newNousObservationDeps(&config.Config{}, &dashboard.NousState{SnapshotDir: filepath.Join(t.TempDir(), "snapshots")})
	if d.enabled {
		t.Fatal("observations must be off without nous.observations.enabled")
	}
	if d.maxPerWeek != config.DefaultNousObservationsMaxPerWeek {
		t.Fatalf("maxPerWeek = %d, want %d", d.maxPerWeek, config.DefaultNousObservationsMaxPerWeek)
	}
	if filepath.Base(d.budgetPath) != nousObservationBudgetFile {
		t.Fatalf("budgetPath = %q", d.budgetPath)
	}
}

func TestNousObservationsThrottle(t *testing.T) {
	for _, tc := range []struct {
		name       string
		found      int
		maxPerWeek int
		wantFiled  int
		wantDrop   bool
	}{
		{"under budget", 2, 5, 2, false},
		{"exactly budget", 3, 3, 3, false},
		{"N+1 files N and logs the drop", 4, 3, 3, true},
		{"budget of one", 2, 1, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNousObservationHarness(t, tc.found, tc.maxPerWeek)
			if got := h.run(); got != tc.wantFiled || len(h.filed) != tc.wantFiled {
				t.Fatalf("filed %d (sink %v), want %d", got, h.filed, tc.wantFiled)
			}
			dropped := strings.Contains(h.logs.String(), "weekly budget exhausted")
			if dropped != tc.wantDrop {
				t.Fatalf("drop logged = %v, want %v; logs:\n%s", dropped, tc.wantDrop, h.logs.String())
			}
			if strings.Contains(h.logs.String(), "level=ERROR") {
				t.Fatalf("budget exhaustion must not log an error:\n%s", h.logs.String())
			}
		})
	}
}

func TestNousObservationsUpgradeWindowFilesNothing(t *testing.T) {
	h := newNousObservationHarness(t, 2, 5)
	h.upgrading = true
	if got := h.run(); got != 0 || len(h.filed) != 0 {
		t.Fatalf("upgrade window: filed %d (sink %v), want 0", got, h.filed)
	}
	if h.detected != 1 {
		t.Fatalf("detectors ran %d times, want 1", h.detected)
	}
	if !strings.Contains(h.logs.String(), "upgrade window open") {
		t.Fatalf("upgrade-window skip not logged:\n%s", h.logs.String())
	}
	budget, err := loadNousObservationBudget(h.deps.budgetPath)
	if err != nil || len(budget.Filed) != 0 {
		t.Fatalf("upgrade window consumed budget: %+v, %v", budget, err)
	}
}

func TestNousObservationsBudgetSurvivesRestart(t *testing.T) {
	h := newNousObservationHarness(t, 2, 3)
	if got := h.run(); got != 2 {
		t.Fatalf("first process filed %d, want 2", got)
	}

	// A new process reading the same file: two new fingerprints, one slot left.
	restarted := newNousObservationHarness(t, 4, 3)
	restarted.deps.budgetPath = h.deps.budgetPath
	restarted.now = h.now.Add(time.Hour)
	if got := restarted.run(); got != 1 {
		t.Fatalf("after restart filed %d (sink %v), want 1", got, restarted.filed)
	}
	if want := "hive-x:kind-2"; len(restarted.filed) != 1 || restarted.filed[0] != want {
		t.Fatalf("after restart filed %v, want [%s] (already-filed fingerprints are skipped)", restarted.filed, want)
	}
	if !strings.Contains(restarted.logs.String(), "weekly budget exhausted") {
		t.Fatalf("drop after restart not logged:\n%s", restarted.logs.String())
	}

	// Once the window rolls past the first filings, budget frees up again.
	later := newNousObservationHarness(t, 4, 3)
	later.deps.budgetPath = h.deps.budgetPath
	later.now = h.now.Add(nousObservationWindow + 2*time.Hour)
	if got := later.run(); got != 3 {
		t.Fatalf("after the window rolled filed %d (sink %v), want 3", got, later.filed)
	}
}

func TestNousObservationsSinkFailureDoesNotSpendBudget(t *testing.T) {
	h := newNousObservationHarness(t, 1, 1)
	h.deps.sink = func(context.Context, hivestate.Observation) error { return fmt.Errorf("relay down") }
	if got := h.run(); got != 0 {
		t.Fatalf("filed %d, want 0", got)
	}
	budget, err := loadNousObservationBudget(h.deps.budgetPath)
	if err != nil || len(budget.Filed) != 0 {
		t.Fatalf("failed filing consumed budget: %+v, %v", budget, err)
	}
}

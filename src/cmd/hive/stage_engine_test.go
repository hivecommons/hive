package main

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Both sweeps hand Client.StageReconciler the one Options this process
// builds, so the PR sweep and the issue sweep share a single reconciler
// instance (ADR-0025: one engine, one writer).
func TestStageEngineOneReconcilerSharedBySweeps(t *testing.T) {
	cfg := &config.Config{}
	cfg.Governor.Labels.Stage.Engine.Mode = "shadow"
	logger := discardLogger()
	client := fakeGitHubClient(t)

	optsA := stageEngineOptions(cfg, logger)
	optsB := stageEngineOptions(cfg, logger)
	if optsA != optsB {
		t.Fatal("stageEngineOptions built two Options; the engine must be constructed once")
	}
	if optsA.Shadow == nil || optsA.Metrics == nil || optsA.Issue == nil || optsA.PR == nil || optsA.Store != stagelabels.Shared() {
		t.Fatalf("options missing a required collaborator: %+v", optsA)
	}

	boot := stageEngineReconciler(cfg, client, logger)
	// The PR sweep path (automerge.Options.StageEngine) and the issue sweep
	// path (SyncIssueStageLabelsWith) both resolve through StageReconciler
	// with the shared Options.
	fromPRSweep, _ := client.StageReconciler(stagelabels.Shared(), optsA, logger)
	fromIssueSweep, _ := client.StageReconciler(stagelabels.Shared(), optsB, logger)
	if boot == nil || fromPRSweep != boot || fromIssueSweep != boot {
		t.Fatalf("sweeps got different reconcilers: boot=%p pr=%p issue=%p", boot, fromPRSweep, fromIssueSweep)
	}
	if _, ok := boot.(stageengine.ModeSwitch); !ok {
		t.Fatalf("reconciler %T is not a ModeSwitch", boot)
	}
}

// A config change flips the shared reconciler's mode on the next sweep
// without a rebuild, and the status block reports it.
func TestStageEngineModeFollowsConfig(t *testing.T) {
	logger := discardLogger()
	cfg := &config.Config{}
	client := fakeGitHubClient(t)

	syncStageEngineMode(cfg, client, logger)
	sw := stageEngineReconciler(cfg, client, logger).(stageengine.ModeSwitch)
	if got := sw.Mode(); got != stageengine.ModeShadow {
		t.Fatalf("default mode = %v, want shadow", got)
	}

	cfg.Governor.Labels.Stage.Engine.Mode = "enforce"
	syncStageEngineMode(cfg, client, logger)
	if got := sw.Mode(); got != stageengine.ModeEnforce {
		t.Fatalf("mode after reload = %v, want enforce", got)
	}
	if st := stageEngineStatus(cfg, client, logger); st.Mode != "enforce" || st.LastMismatchExamples == nil {
		t.Fatalf("status = %+v, want mode enforce with non-nil examples", st)
	}
	// The transition WARN itself is pinned in pkg/stageengine
	// (TestModeSwitchHotReload); the process-wide engine keeps the logger
	// of its first construction, so this test only checks the flip.

	// Unknown values never reach the reconciler: EffectiveMode falls back.
	cfg.Governor.Labels.Stage.Engine.Mode = "bogus"
	syncStageEngineMode(cfg, client, logger)
	if got := sw.Mode(); got != stageengine.ModeShadow {
		t.Fatalf("mode after invalid value = %v, want shadow fallback", got)
	}
	// A nil client (hive booted without GitHub) still yields a usable status.
	if st := stageEngineStatus(cfg, (*github.Client)(nil), logger); st.Mode == "" {
		t.Fatal("status with nil client lost its mode")
	}
}

package main

import (
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The central stage engine (ADR-0025, hivecommons/hive#12999) is built once
// per process: one stageengine.Options every sweep hands to
// Client.StageReconciler, so the PR sweep (automerge.Options.StageEngine)
// and the issue sweep (SyncIssueStageLabelsWith) share the reconciler the
// first call built over the client's single StageWriter and the shared
// stagelabels store. The PR-B adapters are its derivers and the process
// counters its metrics. governor.labels.stage.engine.mode is re-read before every sweep
// (syncStageEngineMode), so a dashboard PUT or a hive.yaml reload flips the
// mode on the next pass without a restart.
//
// The out-of-cycle fact sink (Client.StageFacts, plan PR-D) is deliberately
// not this reconciler: a submitted fact must land a label whatever the
// sweeps' mode, so it runs enforce over the same StageWriter, Store and
// counters; the sweeps' mode gates derivation of the whole fleet.
var stageEngine struct {
	once sync.Once
	opts *stageengine.Options
}

// stageEngineOptions returns the process's one stageengine.Options. The
// mode is the configured one at first call; later changes go through
// syncStageEngineMode, never by rebuilding.
func stageEngineOptions(cfg *config.Config, logger *slog.Logger) *stageengine.Options {
	stageEngine.once.Do(func() {
		if logger == nil {
			logger = slog.Default()
		}
		stageEngine.opts = &stageengine.Options{
			Mode:    stageEngineConfiguredMode(cfg),
			Store:   stagelabels.Shared(),
			Issue:   github.IssueDeriver(),
			PR:      stageengine.PRDeriver(),
			Logger:  logger,
			Metrics: stageengine.DefaultCounters(),
		}
	})
	return stageEngine.opts
}

func stageEngineConfiguredMode(cfg *config.Config) stageengine.Mode {
	if cfg == nil {
		return config.DefaultStageEngineMode
	}
	return cfg.Governor.Labels.Stage.Engine.EffectiveMode()
}

// stageEngineReconciler returns the shared reconciler for client, building
// it on the first call (cmd/hive's boot calls this once so the instance
// exists before either sweep runs).
func stageEngineReconciler(cfg *config.Config, client *github.Client, logger *slog.Logger) stageengine.Reconciler {
	if client == nil {
		return nil
	}
	return client.StageReconciler(stagelabels.Shared(), stageEngineOptions(cfg, logger), logger)
}

// syncStageEngineMode applies the configured mode to the shared reconciler.
// Called before each sweep and from the config reload handler; the
// reconciler logs one WARN per actual transition.
func syncStageEngineMode(cfg *config.Config, client *github.Client, logger *slog.Logger) {
	if sw, ok := stageEngineReconciler(cfg, client, logger).(stageengine.ModeSwitch); ok && sw != nil {
		sw.SetMode(stageEngineConfiguredMode(cfg))
	}
}

// stageEngineStatus is the dashboard's stageEngine status block.
func stageEngineStatus(cfg *config.Config, client *github.Client, logger *slog.Logger) stageengine.Status {
	var r stageengine.Reconciler
	if client != nil {
		r = stageEngineReconciler(cfg, client, logger)
	}
	st := stageengine.StatusOf(r, stageengine.DefaultCounters())
	if st.Mode == "" {
		st.Mode = stageEngineConfiguredMode(cfg).String()
	}
	return st
}

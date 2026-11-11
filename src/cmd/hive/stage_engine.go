package main

import (
	"log/slog"
	"sync"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/workflowmodel"
	wfstore "github.com/hivecommons/hive/pkg/workflowmodel/store"
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
		var wf *wfstore.Store
		if cfg != nil {
			wf = wfstore.Open(wfstore.Root(cfg.Data.WorkflowsDir))
		}
		stageEngine.opts = &stageengine.Options{
			Mode:        stageEngineConfiguredMode(cfg),
			Store:       stagelabels.Shared(),
			Issue:       github.IssueDeriver(),
			PR:          stageengine.PRDeriver(),
			Logger:      logger,
			Metrics:     stageengine.DefaultCounters(),
			WorkflowFor: stageEngineWorkflowFor(cfg, wf, logger),
		}
		if cfg != nil {
			stageEngine.opts.WorkflowProvisioned = wf
			stageEngine.opts.WorkflowLabelGC = func() bool { return cfg.Workflow.LabelGCEnabled() }
		}
	})
	return stageEngine.opts
}

// stageEngineWorkflowFor resolves the workflow model bound to a repository
// (hivecommons/hive#17620). cfg is read on every call — hive.yaml reloads
// and dashboard binding edits mutate it in place — so a binding or the
// workflow.enabled flag takes effect on the next derivation. A bound model
// that is missing or no longer compiles is logged once per key and the
// repository falls back to hive-default rather than going unlabelled.
func stageEngineWorkflowFor(cfg *config.Config, st *wfstore.Store, logger *slog.Logger) func(owner, repo string) *workflowmodel.Profile {
	if cfg == nil || st == nil {
		return nil
	}
	var warned sync.Map
	return func(owner, repo string) *workflowmodel.Profile {
		if !cfg.Workflow.Enabled {
			return nil
		}
		full := repo
		if owner != "" {
			full = owner + "/" + repo
		}
		b, ok := cfg.WorkflowBindingFor(full)
		if !ok {
			return nil
		}
		p, err := st.Profile(b.Name, b.Version)
		if err != nil {
			if _, dup := warned.LoadOrStore(b.Key()+"|"+err.Error(), true); !dup && logger != nil {
				logger.Warn("workflow binding unusable; repository falls back to hive-default", "repo", full, "workflow", b.Key(), "err", err)
			}
			return nil
		}
		return p
	}
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

package main

import (
	"path/filepath"

	"github.com/hivecommons/hive/pkg/convergence/mutation"
	"github.com/hivecommons/hive/pkg/effects"
)

const mutationStateDir = "/data/convergence/mutation"

func (w *spokeWire) wireMutationBoundary() {
	if w == nil || w.cfg == nil {
		return
	}
	mode := w.cfg.ConvergenceMode()
	stats := &effects.Recorder{}
	stats.SetMode(mode)
	w.mutationStats = stats
	ledger, err := mutation.OpenLedger(filepath.Join(mutationStateDir, "claims.json"), mutation.DefaultMaxWritersPerRepo)
	if err != nil {
		w.logger.Error("mutation convergence ledger unavailable; external mutation fencing disabled", "mode", mode, "error", err)
		return
	}
	journal, err := mutation.OpenJournal(filepath.Join(mutationStateDir, "journal.json"))
	if err != nil {
		w.logger.Error("mutation convergence journal unavailable; external mutation fencing disabled", "mode", mode, "error", err)
		return
	}
	boundary := &mutation.Boundary{
		Executor: mutation.Executor{Ledger: ledger, Journal: journal, Mode: mode},
		Holder:   "hive:" + w.cfg.HiveID,
		Logger:   w.logger,
		Stats:    stats,
		Mode:     w.cfg.ConvergenceMode,
	}
	w.mutationBoundary = boundary
	if w.ghClient != nil {
		w.ghClient.SetMutationBoundary(boundary)
	}
	w.logger.Info("mutation convergence boundary wired", "mode", mode, "state_dir", mutationStateDir)
}

func (w *spokeWire) installMutationBoundary(client interface{ SetMutationBoundary(effects.Boundary) }) {
	if w == nil || client == nil {
		return
	}
	client.SetMutationBoundary(w.mutationBoundary)
}

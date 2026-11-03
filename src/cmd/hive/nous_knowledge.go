package main

import (
	"log/slog"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// nousKnowledgeStore adapts nousstore's live ledger/principles reader to the
// dashboard's NousKnowledgeStore seam (RFC #11585 part 4, #11697). Schema
// problems the reader finds (an unknown type or status on a row) are
// logged once per reload — the row is still served with the default.
type nousKnowledgeStore struct {
	reader *nousstore.KnowledgeReader
	logger *slog.Logger
}

func (k nousKnowledgeStore) LoadKnowledge() (dashboard.NousKnowledge, bool, error) {
	know, changed, err := k.reader.Load()
	if changed && k.logger != nil {
		for _, problem := range know.Schema {
			k.logger.Warn("nous: ledger/principles entry off schema", "dir", know.Dir, "error", problem)
		}
		k.logger.Info("nous knowledge loaded", "dir", know.Dir, "iterations", len(know.Ledger), "principles", len(know.Principles))
	}
	return dashboard.NousKnowledge{Ledger: know.Ledger, Principles: nousPrinciplesForPanel(know.Principles)}, changed, err
}

// nousPrinciplesForPanel maps the store's principles onto the panel's
// shape. Source keeps the writer's raw category for provenance; Type and
// Status are the validated schema fields.
func nousPrinciplesForPanel(in []nousstore.Principle) []dashboard.NousPrinciple {
	if in == nil {
		return nil
	}
	out := make([]dashboard.NousPrinciple, len(in))
	for i, p := range in {
		out[i] = dashboard.NousPrinciple{
			ID:         p.ID,
			Text:       p.Statement,
			Confidence: p.Confidence,
			Source:     p.Category,
			Type:       p.Type,
			Status:     p.Status,
		}
	}
	return out
}

// wireNousKnowledge attaches a live reader over layout's governor dir (with
// legacyDir as the read-only fallback) to state and performs the initial
// load, so the boot snapshot and every later request come from one path.
// The same reader backs the reset store so a knowledge reset can drop its
// cache (RFC #11585, POST /api/nous/reset).
func wireNousKnowledge(state *dashboard.NousState, layout nousstore.Layout, legacyDir string, logger *slog.Logger) {
	wireNousKnowledgeWith(state, layout, legacyDir, logger, time.Now)
}

func wireNousKnowledgeWith(state *dashboard.NousState, layout nousstore.Layout, legacyDir string, logger *slog.Logger, now func() time.Time) {
	if state == nil {
		return
	}
	reader := layout.KnowledgeReader(legacyDir)
	state.KnowledgeStore = nousKnowledgeStore{reader: reader, logger: logger}
	state.ResetStore = nousResetStore{layout: layout, reader: reader, now: now}
	if err := state.SyncKnowledge(); err != nil && logger != nil {
		logger.Warn("nous: ledger/principles unreadable at boot", "error", err)
	}
}

// nousResetStore adapts nousstore's reset operation to the dashboard's
// NousResetStore seam. A knowledge reset also drops the live reader's
// cache so the next load re-reads the emptied files.
type nousResetStore struct {
	layout nousstore.Layout
	reader *nousstore.KnowledgeReader
	now    func() time.Time
}

func (r nousResetStore) Reset(scope dashboard.NousResetScope) (dashboard.NousResetCounts, error) {
	res, err := r.layout.Reset(nousstore.ResetScope{Baseline: scope.Baseline, Knowledge: scope.Knowledge, Gates: scope.Gates}, r.now())
	if scope.Knowledge && r.reader != nil {
		r.reader.Invalidate()
	}
	return dashboard.NousResetCounts{Snapshots: res.Snapshots, LedgerEntries: res.LedgerEntries, Principles: res.Principles}, err
}

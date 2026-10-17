package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/dashboard"
	"github.com/hivecommons/hive/pkg/hivestate"
)

// Nous observation filing (#12654): opt-in, a durable rolling weekly budget,
// and no filing while a self-upgrade drain is open.

const (
	// nousObservationWindow is the rolling budget window.
	nousObservationWindow = 7 * 24 * time.Hour
	// nousObservationLookback is how much of the snapshot series the
	// detectors need for their week-over-week checks.
	nousObservationLookback = 14 * 24 * time.Hour
	// nousObservationBudgetFile lives next to the snapshot directory, under
	// data.nous_dir, so a restart does not reset the budget.
	nousObservationBudgetFile = "observations-budget.json"
)

// nousObservationSink files one observation upstream. nil means this build
// has no observation sink; detectors still run but nothing is filed.
type nousObservationSink func(ctx context.Context, obs hivestate.Observation) error

type nousObservationDeps struct {
	enabled     bool
	maxPerWeek  int
	hiveID      string
	snapshotDir string
	budgetPath  string
	now         func() time.Time
	// upgradeWindow reports an open self-upgrade drain, the signal the
	// governor eval loop already honours to withhold new kicks.
	upgradeWindow func(now time.Time) (target string, active bool)
	load          func(dir string, since time.Time) ([]hivestate.Bucket, error)
	detect        func(buckets []hivestate.Bucket, now time.Time, hiveID string) []hivestate.Observation
	sink          nousObservationSink
}

func newNousObservationDeps(cfg *config.Config, nousState *dashboard.NousState) nousObservationDeps {
	d := nousObservationDeps{
		now:           time.Now,
		upgradeWindow: func(now time.Time) (string, bool) { return selfUpgradeDrain.Active(now) },
		load:          hivestate.Load,
		detect:        hivestate.Observe,
	}
	if cfg != nil {
		d.enabled = cfg.Nous.Observations.Enabled
		d.maxPerWeek = cfg.Nous.Observations.ResolvedMaxPerWeek()
		d.hiveID = cfg.HiveID
	}
	if nousState != nil && nousState.SnapshotDir != "" {
		d.snapshotDir = nousState.SnapshotDir
		d.budgetPath = filepath.Join(filepath.Dir(nousState.SnapshotDir), nousObservationBudgetFile)
	}
	// The hub-relayed observation sink (#12653) is wired here once it is
	// present on this line; until then sink stays nil and nothing is filed.
	return d
}

// nousObservationFiling is one filed observation in the budget ledger.
type nousObservationFiling struct {
	Fingerprint string    `json:"fingerprint"`
	FiledAt     time.Time `json:"filed_at"`
}

// nousObservationBudget is the on-disk rolling weekly filing ledger.
type nousObservationBudget struct {
	Filed []nousObservationFiling `json:"filed"`
}

// loadNousObservationBudget reads the ledger at path. A missing file is an
// empty budget; an unreadable or corrupt one is returned with its error so
// the caller can refuse to file rather than silently reset the budget.
// nousObservationLedgerMu serializes the budget ledger's load/append/save so
// overlapping eval cycles cannot file the same fingerprint twice or exceed
// max_per_week.
var nousObservationLedgerMu sync.Mutex

func loadNousObservationBudget(path string) (nousObservationBudget, error) {
	var b nousObservationBudget
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return b, nil
	}
	if err != nil {
		return b, err
	}
	err = json.Unmarshal(data, &b)
	return b, err
}

func (b *nousObservationBudget) prune(now time.Time) {
	kept := b.Filed[:0]
	for _, f := range b.Filed {
		if now.Sub(f.FiledAt) < nousObservationWindow {
			kept = append(kept, f)
		}
	}
	b.Filed = kept
}

func (b *nousObservationBudget) filedRecently(fingerprint string) bool {
	for _, f := range b.Filed {
		if f.Fingerprint == fingerprint {
			return true
		}
	}
	return false
}

func (b nousObservationBudget) save(path string) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, 0o644)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

// runNousObservations runs the hive-state detectors once and files what they
// find through the budget and upgrade-window guard. It never returns an
// error: every failure is logged so the eval cycle carries on. It returns the
// number of observations filed.
func runNousObservations(ctx context.Context, logger *slog.Logger, d nousObservationDeps) int {
	if !d.enabled || d.snapshotDir == "" || d.load == nil || d.detect == nil {
		return 0
	}
	now := d.now().UTC()
	buckets, err := d.load(d.snapshotDir, now.Add(-nousObservationLookback))
	if err != nil {
		logger.Debug("nous observations: snapshot series unavailable", "error", err)
		return 0
	}
	found := d.detect(buckets, now, d.hiveID)
	if len(found) == 0 {
		return 0
	}
	if d.upgradeWindow != nil {
		if target, active := d.upgradeWindow(now); active {
			logger.Info("nous observations: upgrade window open; not filing", "target", target, "observations", len(found))
			return 0
		}
	}
	if d.sink == nil {
		logger.Debug("nous observations: no observation sink on this build; not filing", "observations", len(found))
		return 0
	}
	nousObservationLedgerMu.Lock()
	defer nousObservationLedgerMu.Unlock()
	budget, err := loadNousObservationBudget(d.budgetPath)
	if err != nil {
		logger.Warn("nous observations: weekly budget unreadable; not filing", "path", d.budgetPath, "error", err)
		return 0
	}
	budget.prune(now)
	filed := 0
	for _, obs := range found {
		if budget.filedRecently(obs.Fingerprint) {
			continue
		}
		if len(budget.Filed) >= d.maxPerWeek {
			logger.Info("nous observations: weekly budget exhausted; dropping observation",
				"fingerprint", obs.Fingerprint, "kind", obs.Kind, "max_per_week", d.maxPerWeek)
			continue
		}
		if err := d.sink(ctx, obs); err != nil {
			logger.Warn("nous observations: filing failed", "fingerprint", obs.Fingerprint, "kind", obs.Kind, "error", err)
			continue
		}
		budget.Filed = append(budget.Filed, nousObservationFiling{Fingerprint: obs.Fingerprint, FiledAt: now})
		filed++
		if err := budget.save(d.budgetPath); err != nil {
			// Without a durable ledger a restart could over-file; stop here.
			logger.Warn("nous observations: could not persist weekly budget; not filing more this cycle", "path", d.budgetPath, "error", err)
			return filed
		}
	}
	return filed
}

package main

import (
	"context"
	"log/slog"
	"math"
	"path/filepath"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/nous"
	nousscheduler "github.com/hivecommons/hive/pkg/nous/scheduler"
	"github.com/hivecommons/hive/pkg/nousstore"
)

// Background-only Hive-self scheduler tick (hivecommons/hive#12397, #12768).
// Each tick loads the local Hive-self experiment ledger, maps every unresolved
// experiment and this hive's snapshot series through the scheduler adapter,
// and hands each decision to the executor, which latches fast-fails and
// reverts treatments durably. It is gated by self_experiments.scheduler
// (enabled + mode: background), default off; when off nothing is loaded and
// no goroutine is started. It never reads or writes governor configuration.

// nousSelfLedgerFile is the Hive-self experiment ledger at the root of the
// durable nous layout, next to the snapshot directory and separate from the
// Experiment Lab's campaigns.json.
const nousSelfLedgerFile = "self-experiments.json"

func nousSelfLedgerPath(layout nousstore.Layout) string {
	return filepath.Join(layout.Root, nousSelfLedgerFile)
}

// nousSelfMissingReason explains a measurement with no snapshot evidence.
const nousSelfMissingReason = "no snapshot evidence in the baseline or treatment window"

// nousSelfMissingBaselineReason explains a measurement whose treatment window
// was observed but whose baseline window has no snapshot evidence.
const nousSelfMissingBaselineReason = "no snapshot evidence in the baseline window"

// nousSelfTick is one scheduler pass; every effect is a seam so tests run it
// hermetically.
type nousSelfTick struct {
	enabled bool
	mode    string
	load    func() ([]nous.Experiment, error)
	series  func(since time.Time) ([]nousscheduler.HiveSeries, error)
	// evaluate defaults to nousscheduler.EvaluateExperiment, which wraps
	// scheduler.Evaluate with the adapter's guardrails and judgement.
	evaluate func(enabled bool, mode string, now time.Time, e nous.Experiment, series []nousscheduler.HiveSeries) (nousscheduler.Decision, error)
	execute  func(id string, d nousscheduler.Decision, measurements []nous.Measurement) (*nous.Result, error)
	now      func() time.Time
	logger   *slog.Logger
}

func (t *nousSelfTick) active() bool {
	return t.enabled && t.mode == config.SelfExperimentSchedulerModeBackground
}

// run evaluates and executes every unresolved experiment once. It is a no-op
// unless the scheduler is enabled in background mode.
func (t *nousSelfTick) run() {
	if !t.active() {
		return
	}
	ledger, err := t.load()
	if err != nil {
		t.logger.Warn("nous self scheduler: load experiment ledger failed", "error", err)
		return
	}
	var pending []nous.Experiment
	var since time.Time
	for _, e := range ledger {
		if e.Result != nil {
			continue
		}
		if since.IsZero() || e.CreatedAt.Before(since) {
			since = e.CreatedAt
		}
		pending = append(pending, e)
	}
	if len(pending) == 0 {
		return
	}
	series, err := t.series(since)
	if err != nil {
		// Missing snapshots are missing evidence, never zero: evaluate anyway so
		// time-based phases (and their reverts) still advance.
		t.logger.Warn("nous self scheduler: load snapshot series failed", "error", err)
		series = nil
	}
	now := t.now()
	for _, e := range pending {
		d, err := t.evaluate(t.enabled, t.mode, now, e, series)
		if err != nil {
			t.logger.Warn("nous self scheduler: evaluate failed", "experiment", e.ID, "error", err)
			continue
		}
		if d.Phase == nousscheduler.Disabled {
			continue
		}
		res, err := t.execute(e.ID, d, nousSelfMeasurements(e, series, now))
		if err != nil {
			t.logger.Warn("nous self scheduler: execute failed", "experiment", e.ID, "phase", d.Phase, "error", err)
			continue
		}
		if res != nil {
			t.logger.Info("nous self scheduler: experiment resolved", "experiment", e.ID, "verdict", res.Verdict, "reason", d.Reason)
		}
	}
}

// nousSelfMeasurements builds the evidence stored with a result: one
// measurement per treatment cohort and metric, comparing the baseline window
// mean with the treatment window mean. A metric without samples in either
// window is recorded as missing with a reason, never as zero. When only the
// baseline is missing the observed treatment mean is still kept, so a
// guardrail fast-fail (judged against absolute bounds) has durable evidence.
func nousSelfMeasurements(e nous.Experiment, series []nousscheduler.HiveSeries, now time.Time) []nous.Measurement {
	baselineEnd := e.CreatedAt.Add(time.Duration(e.Window.BaselineHours) * time.Hour)
	experimentEnd := baselineEnd.Add(time.Duration(e.Window.ExperimentDurationHours) * time.Hour)
	var out []nous.Measurement
	for _, c := range e.Cohorts {
		if c.Role != nous.RoleTreatment {
			continue
		}
		scoped := e
		scoped.Cohorts = []nous.Cohort{c}
		samples := nousscheduler.SamplesFor(scoped, series)
		for _, m := range e.Metrics {
			ms := nous.Measurement{MetricID: m.ID, CohortID: c.ID}
			baseline, _ := nousSelfWindowMean(samples, m.ID, e.CreatedAt, baselineEnd, now)
			observed, n := nousSelfWindowMean(samples, m.ID, baselineEnd, experimentEnd, now)
			switch {
			case observed == nil:
				ms.MissingReason = nousSelfMissingReason
			case baseline == nil:
				ms.Observed, ms.Samples = observed, n
				ms.MissingReason = nousSelfMissingBaselineReason
			default:
				ms.Baseline, ms.Observed, ms.Samples = baseline, observed, n
			}
			out = append(out, ms)
		}
	}
	return out
}

// nousSelfWindowMean averages metric over samples in [start, end) at or before now.
func nousSelfWindowMean(samples []nousscheduler.Sample, metric string, start, end, now time.Time) (*float64, int) {
	var sum float64
	n := 0
	for _, s := range samples {
		if s.At.Before(start) || !s.At.Before(end) || s.At.After(now) {
			continue
		}
		if v, ok := s.Values[metric]; ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			sum += v
			n++
		}
	}
	if n == 0 {
		return nil, 0
	}
	mean := sum / float64(n)
	return &mean, n
}

// newNousSelfTick wires the tick to the on-disk ledger, this hive's snapshot
// series and the process-wide self-flag gate.
func newNousSelfTick(cfg *config.Config, executor *nousscheduler.Executor, store *nous.ExperimentStore, snapshotDir string, logger *slog.Logger) *nousSelfTick {
	sc := cfg.SelfExperiments.Scheduler
	hiveID := cfg.HiveID
	return &nousSelfTick{
		enabled: sc.Enabled,
		mode:    sc.Mode,
		load:    store.Load,
		series: func(since time.Time) ([]nousscheduler.HiveSeries, error) {
			buckets, err := hivestate.Load(snapshotDir, since)
			if err != nil {
				return nil, err
			}
			return []nousscheduler.HiveSeries{{HiveID: hiveID, Buckets: buckets}}, nil
		},
		evaluate: nousscheduler.EvaluateExperiment,
		execute:  executor.Execute,
		now:      time.Now,
		logger:   logger,
	}
}

// startNousSelfScheduler restores durable reverts and starts the periodic
// tick. It returns without loading anything or starting a goroutine unless
// self_experiments.scheduler is enabled in background mode. Call it before the
// hub heartbeat starts so restored reverts precede the first flag assignment.
func startNousSelfScheduler(ctx context.Context, cfg *config.Config, layout nousstore.Layout, logger *slog.Logger) {
	if cfg == nil || !cfg.SelfExperiments.Scheduler.Active() {
		return
	}
	store := nous.NewExperimentStore(nousSelfLedgerPath(layout))
	// No release-channel revert hook exists yet, so RevertChannel stays nil.
	// A channel subject's result is recorded before its revert is attempted and
	// run() skips resolved experiments, so the failed revert (ErrNoChannelRevert)
	// is only re-attempted by Restore() at the next boot. Flag subjects revert
	// locally.
	executor := &nousscheduler.Executor{Store: store, Gate: &nous.SelfExperiments}
	if err := executor.Restore(); err != nil {
		logger.Warn("nous self scheduler: restore reverts failed", "error", err)
	}
	tick := newNousSelfTick(cfg, executor, store, layout.SnapshotDir(), logger)
	interval := cfg.SelfExperiments.Scheduler.Interval()
	logger.Info("nous self scheduler started", "mode", cfg.SelfExperiments.Scheduler.Mode, "interval", interval)
	go runNousSelfScheduler(ctx, tick, interval)
}

func runNousSelfScheduler(ctx context.Context, tick *nousSelfTick, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick.run()
		}
	}
}

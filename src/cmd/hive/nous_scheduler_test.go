package main

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/nous"
	nousscheduler "github.com/hivecommons/hive/pkg/nous/scheduler"
	"github.com/hivecommons/hive/pkg/nousstore"
)

var nousSelfCreated = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func nousSelfExperiment(id string) nous.Experiment {
	delta := 1.0
	return nous.Experiment{
		ID: id, Hypothesis: "smaller queues", CreatedAt: nousSelfCreated,
		Subject: nous.Subject{Kind: nous.SubjectFlag, Reference: "review-v2"},
		Cohorts: []nous.Cohort{
			{ID: "c", Role: nous.RoleControl, HiveIDs: []string{"h1"}, OptIn: true, Backend: "copilot", Model: "m"},
			{ID: "t", Role: nous.RoleTreatment, HiveIDs: []string{"h2"}, OptIn: true, Backend: "copilot", Model: "m"},
		},
		Window: nous.Window{BaselineHours: 1, ExperimentDurationHours: 2, CooldownHours: 1},
		Metrics: []nous.Metric{{
			ID: "queue", QuestionID: "quality", DefinitionID: "queue_depth_p90", SourceField: "queue_issues",
			Direction: nous.DirectionDecrease, MinimumDelta: &delta,
		}},
	}
}

func nousSelfSeries(hiveID string, baseline, treatment int) []nousscheduler.HiveSeries {
	at := func(d time.Duration, q int) hivestate.Bucket {
		return hivestate.FromSnapshot(nousSelfCreated.Add(d), hivestate.Snapshot{QueueIssues: q})
	}
	return []nousscheduler.HiveSeries{{HiveID: hiveID, Buckets: []hivestate.Bucket{
		at(0, baseline), at(30*time.Minute, baseline), at(90*time.Minute, treatment),
	}}}
}

// failingNousSelfTick fails the test if any seam is reached.
func failingNousSelfTick(t *testing.T, enabled bool, mode string) *nousSelfTick {
	t.Helper()
	return &nousSelfTick{
		enabled: enabled,
		mode:    mode,
		load: func() ([]nous.Experiment, error) {
			t.Fatal("ledger loaded while scheduler is off")
			return nil, nil
		},
		series: func(time.Time) ([]nousscheduler.HiveSeries, error) {
			t.Fatal("snapshots loaded while scheduler is off")
			return nil, nil
		},
		evaluate: func(bool, string, time.Time, nous.Experiment, []nousscheduler.HiveSeries) (nousscheduler.Decision, error) {
			t.Fatal("evaluate called while scheduler is off")
			return nousscheduler.Decision{}, nil
		},
		execute: func(string, nousscheduler.Decision, []nous.Measurement) (*nous.Result, error) {
			t.Fatal("execute called while scheduler is off")
			return nil, nil
		},
		now:    func() time.Time { return nousSelfCreated },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestNousSelfTickNoOpWhenDisabledOrNotBackground(t *testing.T) {
	for _, tc := range []struct {
		enabled bool
		mode    string
	}{
		{false, ""},
		{false, "background"},
		{true, ""},
		{true, "foreground"},
		{true, "Background"},
	} {
		failingNousSelfTick(t, tc.enabled, tc.mode).run()
	}
}

func TestNousSelfSchedulerConfigDefaultOff(t *testing.T) {
	var cfg config.Config
	if cfg.SelfExperiments.Scheduler.Active() {
		t.Fatal("zero-value config activated the self scheduler")
	}
	cfg.SelfExperiments.Scheduler.Enabled = true
	if cfg.SelfExperiments.Scheduler.Active() {
		t.Fatal("enabled without mode: background activated the self scheduler")
	}
	cfg.SelfExperiments.Scheduler.Mode = config.SelfExperimentSchedulerModeBackground
	if !cfg.SelfExperiments.Scheduler.Active() {
		t.Fatal("enabled + background did not activate the self scheduler")
	}
	if got := (config.SelfExperimentSchedulerConfig{}).Interval(); got != config.DefaultSelfExperimentSchedulerInterval {
		t.Fatalf("default interval = %v", got)
	}
	// Off: returns before touching the ledger, the gate or a logger.
	startNousSelfScheduler(t.Context(), &config.Config{}, nousstore.Layout{}, nil)
	startNousSelfScheduler(t.Context(), nil, nousstore.Layout{}, nil)
}

func TestNousSelfTickEvaluatesAndExecutesUnresolved(t *testing.T) {
	open := nousSelfExperiment("open")
	done := nousSelfExperiment("done")
	b, o := 1.0, 2.0
	done.Result = &nous.Result{RecordedAt: nousSelfCreated, Verdict: nous.VerdictRegressed,
		Measurements: []nous.Measurement{{MetricID: "queue", CohortID: "t", Baseline: &b, Observed: &o, Samples: 1}}}
	now := nousSelfCreated.Add(2 * time.Hour)
	series := nousSelfSeries("h2", 5, 3)

	var evaluated, executed []string
	var gotSince time.Time
	var gotMeasurements []nous.Measurement
	want := nousscheduler.Decision{Phase: nousscheduler.Experiment, Revert: true, FastFail: true, Reason: "fast-fail"}
	tick := &nousSelfTick{
		enabled: true,
		mode:    config.SelfExperimentSchedulerModeBackground,
		load:    func() ([]nous.Experiment, error) { return []nous.Experiment{done, open}, nil },
		series: func(since time.Time) ([]nousscheduler.HiveSeries, error) {
			gotSince = since
			return series, nil
		},
		evaluate: func(enabled bool, mode string, at time.Time, e nous.Experiment, s []nousscheduler.HiveSeries) (nousscheduler.Decision, error) {
			if !enabled || mode != "background" || !at.Equal(now) || len(s) != 1 {
				t.Fatalf("evaluate args: enabled=%v mode=%q now=%v series=%d", enabled, mode, at, len(s))
			}
			evaluated = append(evaluated, e.ID)
			return want, nil
		},
		execute: func(id string, d nousscheduler.Decision, m []nous.Measurement) (*nous.Result, error) {
			if d != want {
				t.Fatalf("execute decision = %+v, want %+v", d, want)
			}
			executed = append(executed, id)
			gotMeasurements = m
			return nil, nil
		},
		now:    func() time.Time { return now },
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	tick.run()

	if len(evaluated) != 1 || evaluated[0] != "open" || len(executed) != 1 || executed[0] != "open" {
		t.Fatalf("evaluated=%v executed=%v, want only the unresolved experiment", evaluated, executed)
	}
	if !gotSince.Equal(open.CreatedAt) {
		t.Fatalf("series since = %v, want %v", gotSince, open.CreatedAt)
	}
	if len(gotMeasurements) != 1 {
		t.Fatalf("measurements = %+v", gotMeasurements)
	}
	m := gotMeasurements[0]
	if m.CohortID != "t" || m.Baseline == nil || *m.Baseline != 5 || m.Observed == nil || *m.Observed != 3 || m.Samples != 1 {
		t.Fatalf("measurement = %+v", m)
	}
}

func TestNousSelfTickSkipsExecuteWhenDecisionDisabled(t *testing.T) {
	tick := failingNousSelfTick(t, true, config.SelfExperimentSchedulerModeBackground)
	tick.load = func() ([]nous.Experiment, error) { return []nous.Experiment{nousSelfExperiment("x")}, nil }
	tick.series = func(time.Time) ([]nousscheduler.HiveSeries, error) { return nil, nil }
	tick.evaluate = func(bool, string, time.Time, nous.Experiment, []nousscheduler.HiveSeries) (nousscheduler.Decision, error) {
		return nousscheduler.Decision{Phase: nousscheduler.Disabled}, nil
	}
	tick.run()
}

func TestNousSelfMeasurementsMissingEvidence(t *testing.T) {
	ms := nousSelfMeasurements(nousSelfExperiment("x"), nil, nousSelfCreated.Add(4*time.Hour))
	if len(ms) != 1 || ms[0].Baseline != nil || ms[0].Observed != nil || ms[0].MissingReason == "" {
		t.Fatalf("missing evidence must be recorded with a reason, never zero: %+v", ms)
	}
}

// TestNousSelfTickFastFailLatchesThroughExecutor drives the real adapter and
// executor against a ledger in t.TempDir: a guardrail breach is latched as
// fast_failed and the flag stays reverted across a later heartbeat.
func TestNousSelfTickFastFailLatchesThroughExecutor(t *testing.T) {
	e := nousSelfExperiment("exp-1")
	upper := 4.0
	e.Guardrails = []nous.Guardrail{{MetricID: "queue", Upper: &upper}}
	store := nous.NewExperimentStore(filepath.Join(t.TempDir(), nousSelfLedgerFile))
	if err := store.Save([]nous.Experiment{e}); err != nil {
		t.Fatal(err)
	}
	gate := &nous.SelfFlagGate{}
	assignment := &nous.SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true}
	gate.Apply(true, assignment)
	now := nousSelfCreated.Add(2 * time.Hour)
	executor := &nousscheduler.Executor{Store: store, Gate: gate, Now: func() time.Time { return now }}

	cfg := &config.Config{HiveID: "h2"}
	cfg.SelfExperiments.Scheduler = config.SelfExperimentSchedulerConfig{Enabled: true, Mode: "background"}
	tick := newNousSelfTick(cfg, executor, store, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	tick.now = func() time.Time { return now }
	tick.series = func(time.Time) ([]nousscheduler.HiveSeries, error) { return nousSelfSeries("h2", 3, 9), nil }
	tick.run()

	ledger, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if ledger[0].Result == nil || ledger[0].Result.Verdict != nous.VerdictFastFailed {
		t.Fatalf("fast-fail not latched: %+v", ledger[0].Result)
	}
	gate.Apply(true, assignment)
	if gate.Enabled("review-v2") {
		t.Fatal("heartbeat re-enabled a fast-failed treatment")
	}

	restarted := &nous.SelfFlagGate{}
	if err := (&nousscheduler.Executor{Store: store, Gate: restarted}).Restore(); err != nil {
		t.Fatal(err)
	}
	restarted.Apply(true, assignment)
	if restarted.Enabled("review-v2") {
		t.Fatal("restore did not keep the fast-failed treatment off")
	}
}

// TestNousSelfTickFastFailWithoutBaselinePersists covers a guardrail breach
// when the baseline window has no snapshot: the fast-fail must still be
// recorded in the store so Restore can rebuild the revert after a restart.
func TestNousSelfTickFastFailWithoutBaselinePersists(t *testing.T) {
	e := nousSelfExperiment("exp-1")
	upper := 4.0
	e.Guardrails = []nous.Guardrail{{MetricID: "queue", Upper: &upper}}
	store := nous.NewExperimentStore(filepath.Join(t.TempDir(), nousSelfLedgerFile))
	if err := store.Save([]nous.Experiment{e}); err != nil {
		t.Fatal(err)
	}
	gate := &nous.SelfFlagGate{}
	assignment := &nous.SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true}
	gate.Apply(true, assignment)
	now := nousSelfCreated.Add(2 * time.Hour)
	executor := &nousscheduler.Executor{Store: store, Gate: gate, Now: func() time.Time { return now }}

	cfg := &config.Config{HiveID: "h2"}
	cfg.SelfExperiments.Scheduler = config.SelfExperimentSchedulerConfig{Enabled: true, Mode: "background"}
	tick := newNousSelfTick(cfg, executor, store, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	tick.now = func() time.Time { return now }
	tick.series = func(time.Time) ([]nousscheduler.HiveSeries, error) {
		return []nousscheduler.HiveSeries{{HiveID: "h2", Buckets: []hivestate.Bucket{
			hivestate.FromSnapshot(nousSelfCreated.Add(90*time.Minute), hivestate.Snapshot{QueueIssues: 9}),
		}}}, nil
	}
	tick.run()

	ledger, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	res := ledger[0].Result
	if res == nil || res.Verdict != nous.VerdictFastFailed {
		t.Fatalf("fast-fail without baseline not persisted: %+v", res)
	}
	if len(res.Measurements) != 1 || res.Measurements[0].Baseline != nil || res.Measurements[0].Observed == nil || res.Measurements[0].MissingReason == "" {
		t.Fatalf("baseline must stay missing with a reason, observed kept: %+v", res.Measurements)
	}

	restarted := &nous.SelfFlagGate{}
	if err := (&nousscheduler.Executor{Store: store, Gate: restarted}).Restore(); err != nil {
		t.Fatal(err)
	}
	restarted.Apply(true, assignment)
	if restarted.Enabled("review-v2") {
		t.Fatal("restore did not keep the fast-failed treatment off")
	}
}

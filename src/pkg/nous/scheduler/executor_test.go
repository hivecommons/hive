package scheduler

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/nous"
)

var execCreated = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func execExperiment(id, kind string) nous.Experiment {
	delta := 1.0
	return nous.Experiment{
		ID: id, Hypothesis: "smaller queues", CreatedAt: execCreated,
		Subject: nous.Subject{Kind: kind, Reference: "review-v2"},
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

func execEvidence() []nous.Measurement {
	b, o := 10.0, 30.0
	return []nous.Measurement{{MetricID: "queue", CohortID: "t", Baseline: &b, Observed: &o, Samples: 3}}
}

func newExecutor(t *testing.T, experiments ...nous.Experiment) (*Executor, *nous.SelfFlagGate) {
	t.Helper()
	store := nous.NewExperimentStore(filepath.Join(t.TempDir(), "nous.json"))
	if err := store.Save(experiments); err != nil {
		t.Fatal(err)
	}
	gate := &nous.SelfFlagGate{}
	now := execCreated.Add(2 * time.Hour)
	return &Executor{Store: store, Gate: gate, Now: func() time.Time { return now }}, gate
}

func storedResult(t *testing.T, x *Executor, id string) *nous.Result {
	t.Helper()
	ledger, err := x.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ledger {
		if e.ID == id {
			return e.Result
		}
	}
	t.Fatalf("experiment %q missing", id)
	return nil
}

func TestExecutorFastFailLatchesAndReverts(t *testing.T) {
	x, gate := newExecutor(t, execExperiment("exp-1", nous.SubjectFlag))
	assignment := &nous.SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true}
	gate.Apply(true, assignment)
	if !gate.Enabled("review-v2") {
		t.Fatal("setup: flag not enabled")
	}
	res, err := x.Execute("exp-1", Decision{Phase: Experiment, Revert: true, FastFail: true, Reason: "fast-fail: queue"}, execEvidence())
	if err != nil || res == nil || res.Verdict != nous.VerdictFastFailed {
		t.Fatalf("fast-fail: res=%+v err=%v", res, err)
	}
	if gate.Enabled("review-v2") {
		t.Fatal("fast-fail did not revert the flag")
	}
	stored := storedResult(t, x, "exp-1")
	if stored == nil || stored.Verdict != nous.VerdictFastFailed {
		t.Fatalf("fast-fail not persisted: %+v", stored)
	}

	// The next heartbeat re-enables the same experiment; the local revert wins.
	gate.Apply(true, assignment)
	if gate.Enabled("review-v2") || gate.Assignment().ExperimentID != "exp-1" {
		t.Fatalf("heartbeat undid revert: %+v", gate.Assignment())
	}
	// Other experiments are unaffected by the deny-list.
	gate.Apply(true, &nous.SelfFlagAssignment{ExperimentID: "exp-2", Flag: "other", Enabled: true})
	if !gate.Enabled("other") {
		t.Fatal("revert leaked to another experiment")
	}

	// Retries and later non-failing evaluations are idempotent and never re-enable.
	x.Now = func() time.Time { return execCreated.Add(5 * time.Hour) }
	for _, d := range []Decision{
		{Phase: Experiment, Revert: true, FastFail: true},
		{Phase: Experiment},
		{Phase: Complete, Revert: true, Judged: true, Passed: true},
	} {
		again, err := x.Execute("exp-1", d, execEvidence())
		if err != nil || again == nil || again.Verdict != nous.VerdictFastFailed || !again.RecordedAt.Equal(res.RecordedAt) {
			t.Fatalf("%+v: latched result changed: %+v err=%v", d, again, err)
		}
	}
	gate.Apply(true, assignment)
	if gate.Enabled("review-v2") {
		t.Fatal("retry re-enabled the flag")
	}

	// After a restart the gate starts empty; Restore rebuilds the latch from the ledger.
	fresh := &nous.SelfFlagGate{}
	restarted := &Executor{Store: x.Store, Gate: fresh}
	if err := restarted.Restore(); err != nil {
		t.Fatal(err)
	}
	fresh.Apply(true, assignment)
	if fresh.Enabled("review-v2") || !fresh.Reverted("exp-1") {
		t.Fatal("restored gate re-enabled a fast-failed experiment")
	}
}

func TestExecutorStoreFailureReportsNoResult(t *testing.T) {
	x, gate := newExecutor(t, execExperiment("exp-1", nous.SubjectFlag))
	gate.Apply(true, &nous.SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true})
	bad := []nous.Measurement{{MetricID: "unregistered", CohortID: "t", MissingReason: "x"}}
	res, err := x.Execute("exp-1", Decision{Phase: Experiment, Revert: true, FastFail: true}, bad)
	if err == nil || res != nil {
		t.Fatalf("store failure reported a result: res=%+v err=%v", res, err)
	}
	if storedResult(t, x, "exp-1") != nil {
		t.Fatal("failed write left a result")
	}
	if gate.Enabled("review-v2") {
		t.Fatal("fail-safe revert not applied after store failure")
	}
	res, err = x.Execute("exp-1", Decision{Phase: Experiment, Revert: true, FastFail: true}, execEvidence())
	if err != nil || res == nil || res.Verdict != nous.VerdictFastFailed {
		t.Fatalf("retry after store failure: res=%+v err=%v", res, err)
	}
}

func TestExecutorChannelRevertHook(t *testing.T) {
	x, _ := newExecutor(t, execExperiment("exp-c", nous.SubjectChannel))
	d := Decision{Phase: Experiment, Revert: true, FastFail: true}
	if _, err := x.Execute("exp-c", d, execEvidence()); !errors.Is(err, ErrNoChannelRevert) {
		t.Fatalf("missing hook: err=%v", err)
	}
	if r := storedResult(t, x, "exp-c"); r == nil || r.Verdict != nous.VerdictFastFailed {
		t.Fatal("fast-fail must be latched before the channel revert is attempted")
	}
	var calls []string
	x.RevertChannel = func(e nous.Experiment) error { calls = append(calls, e.Subject.Reference); return nil }
	if res, err := x.Execute("exp-c", d, execEvidence()); err != nil || res == nil || res.Verdict != nous.VerdictFastFailed {
		t.Fatalf("retry with hook: res=%+v err=%v", res, err)
	}
	if len(calls) != 1 || calls[0] != "review-v2" {
		t.Fatalf("channel hook calls = %v", calls)
	}
}

func TestExecutorJudgedVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Decision
		want string
	}{
		{"improved", Decision{Phase: Complete, Revert: true, Judged: true, Passed: true}, nous.VerdictImproved},
		{"regressed", Decision{Phase: Complete, Revert: true, Judged: true}, nous.VerdictRegressed},
		{"inconclusive", Decision{Phase: Complete, Revert: true, Reason: "missing evidence: queue"}, nous.VerdictInconclusive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, gate := newExecutor(t, execExperiment("exp-1", nous.SubjectFlag))
			gate.Apply(true, &nous.SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true})
			res, err := x.Execute("exp-1", tc.d, execEvidence())
			if err != nil || res == nil || res.Verdict != tc.want || res.Summary != tc.d.Reason {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if r := storedResult(t, x, "exp-1"); r == nil || r.Verdict != tc.want {
				t.Fatalf("stored=%+v", r)
			}
			if gate.Enabled("review-v2") {
				t.Fatal("completed experiment not reverted")
			}
		})
	}
}

func TestExecutorNonTerminalDecisions(t *testing.T) {
	x, gate := newExecutor(t, execExperiment("exp-1", nous.SubjectFlag), execExperiment("exp-c", nous.SubjectChannel))
	gate.Apply(true, &nous.SelfFlagAssignment{ExperimentID: "exp-1", Flag: "review-v2", Enabled: true})
	for _, d := range []Decision{{Phase: Disabled}, {Phase: Baseline}, {Phase: Experiment}} {
		if res, err := x.Execute("exp-1", d, execEvidence()); err != nil || res != nil {
			t.Fatalf("%+v: res=%+v err=%v", d, res, err)
		}
	}
	if !gate.Enabled("review-v2") || gate.Reverted("exp-1") {
		t.Fatal("non-terminal decision reverted the flag")
	}
	// Cooldown reverts but does not record a result yet.
	if res, err := x.Execute("exp-1", Decision{Phase: Cooldown, Revert: true}, execEvidence()); err != nil || res != nil {
		t.Fatalf("cooldown: res=%+v err=%v", res, err)
	}
	if gate.Enabled("review-v2") || storedResult(t, x, "exp-1") != nil {
		t.Fatal("cooldown must revert without recording a result")
	}
	// A failed channel revert at completion records nothing, so the caller retries.
	x.RevertChannel = func(nous.Experiment) error { return errors.New("upgrade busy") }
	if res, err := x.Execute("exp-c", Decision{Phase: Complete, Revert: true, Judged: true, Passed: true}, execEvidence()); err == nil || res != nil {
		t.Fatalf("failed revert reported a result: res=%+v err=%v", res, err)
	}
	if storedResult(t, x, "exp-c") != nil {
		t.Fatal("result recorded before revert succeeded")
	}
	if _, err := x.Execute("missing", Decision{Phase: Complete}, nil); err == nil {
		t.Fatal("unknown experiment accepted")
	}
}

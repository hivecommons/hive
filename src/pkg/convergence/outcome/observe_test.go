package outcome

import (
	"testing"

	"github.com/hivecommons/hive/pkg/convergence"
)

// The default-off guarantee, made structural: with the convergence mode off —
// or anything unrecognised — a record projects NOTHING into the Evaluate
// seam, so admission cannot even see that an outcome exists.
func TestAdmissionStatus_DefaultOffProjectsNothing(t *testing.T) {
	rec := Record{Ref: Ref{Project: "default", Repo: "o/r", Outcome: "x"}, Generation: 3}
	for _, mode := range []string{"", ModeOff, "OFF", "typo", "on", "enforce", "shadoww"} {
		if got := rec.AdmissionStatus(mode, 3); got != nil {
			t.Fatalf("mode %q projected %+v, want nil (default-off)", mode, got)
		}
		if ModeEnabled(mode) {
			t.Fatalf("ModeEnabled(%q) = true", mode)
		}
	}
}

func TestAdmissionStatus_ShadowProjectsCopiedValues(t *testing.T) {
	rec := Record{Ref: Ref{Project: "default", Repo: "hivecommons/hive", Outcome: "nightly-green"}, Generation: 2}
	for _, mode := range []string{ModeShadow, " Shadow "} {
		st := rec.AdmissionStatus(mode, 1)
		if st == nil {
			t.Fatalf("mode %q projected nothing", mode)
		}
		if st.Key != "default/hivecommons/hive@nightly-green" ||
			st.DesiredGeneration != 2 || st.ObservedGeneration != 1 {
			t.Fatalf("projection = %+v", st)
		}
	}
}

// End-to-end through the seam: a ledger record at desired generation 2 with
// evidence from generation 1 never admits; the same evidence at generation 2
// falls through to ordinary admission. With the toggle off the exact same
// candidate is byte-identically legacy.
func TestAdmissionStatus_EndToEndThroughEvaluate(t *testing.T) {
	l, _ := testLedger(t, Options{})
	r := ref("default", "hivecommons/hive", "nightly-green")
	if _, err := l.Create(r, "g1", nil, maintainer); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := l.Accept(r, 1, maintainer); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := l.Supersede(r, 1, "g2", nil, maintainer); err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	rec, _ := l.Get(r)

	subject := convergence.Subject{Repo: "hivecommons/hive", Number: 42}

	stale := convergence.Evaluate(convergence.Observation{
		Subject: subject, Outcome: rec.AdmissionStatus(ModeShadow, 1),
	})
	if stale.Admitted || stale.Reason != convergence.ReasonOutcomeGenerationStale {
		t.Fatalf("generation-1 evidence against desired 2: %+v", stale)
	}

	current := convergence.Evaluate(convergence.Observation{
		Subject: subject, Outcome: rec.AdmissionStatus(ModeShadow, rec.Generation),
	})
	if !current.Admitted {
		t.Fatalf("matched generation blocked ordinary admission: %+v", current)
	}
	cond, ok := current.Condition(convergence.ConditionOutcomeGenerationObserved)
	if !ok || cond.Status != convergence.ConditionTrue {
		t.Fatalf("match condition = %+v", cond)
	}

	off := convergence.Evaluate(convergence.Observation{
		Subject: subject, Outcome: rec.AdmissionStatus(ModeOff, 1),
	})
	if !off.Admitted || off.Outcome != nil {
		t.Fatalf("toggle off is not byte-identical legacy: %+v", off)
	}
}

package proof

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/convergence"
)

// The #4254 strict RED matrix, exercised through the production seams: the
// durable store (Open/Put/Get), the current-truth judgment (VerifyCurrent),
// the mode-gated dependency projection (AdmissionDependency), and the
// production evaluator gate (convergence.Evaluate).

const freshness = time.Hour

// gateDecision runs one candidate through the REAL dependent-transition gate:
// verdict → mode-gated dependency edge → convergence.Evaluate.
func gateDecision(t *testing.T, s *Store, ctx Context, inputs InputObservation, mode string) convergence.Decision {
	t.Helper()
	v := VerifyCurrent(s, ctx, inputs)
	obs := convergence.Observation{
		Subject:  convergence.Subject{Repo: ctx.Required.Repo, Number: ctx.Required.PRNumber},
		Found:    true,
		RecordID: "intent-" + ctx.Required.Repo,
	}
	if dep := v.AdmissionDependency(mode, ctx); dep != nil {
		obs.Dependencies = append(obs.Dependencies, *dep)
	}
	return convergence.Evaluate(obs)
}

func seededStore(t *testing.T) (*Store, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proofs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()
	if _, err := s.Put(greenRecord(t, fpA(), now)); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if _, err := s.Put(greenRecord(t, fpB(), now)); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	return s, now
}

// Row 1: proof current at desired generation G with declared input I — its
// dependent transition is admitted; the unrelated proof and candidate remain
// admitted as positive controls.
func TestRow_CurrentProofAdmits_WithPositiveControls(t *testing.T) {
	s, now := seededStore(t)
	inputs := currentInputs()
	ctxA := Context{Required: fpA(), Now: now, MaxAge: freshness}
	ctxB := Context{Required: fpB(), Now: now, MaxAge: freshness}

	if d := gateDecision(t, s, ctxA, inputs, ModeEnforce); !d.Admitted {
		t.Fatalf("current proof must admit its transition: %+v", d)
	}
	if d := gateDecision(t, s, ctxB, inputs, ModeEnforce); !d.Admitted {
		t.Fatalf("positive-control proof must admit: %+v", d)
	}
}

// Row 2: ONLY declared input I changes (base moves) while the historical work
// record remains closed — the dependent predicate becomes Unknown and its
// transition is withheld. Historical closure is not an input to the judgment,
// so nothing can latch the outcome true.
func TestRow_DeclaredInputChange_InvalidatesAndWithholds(t *testing.T) {
	s, now := seededStore(t)
	moved := currentInputs()
	moved.Current[AssumptionName(AssumptionBaseSHA, "acme/widgets")] = shaBaseA2
	ctxA := Context{Required: fpA(), Now: now, MaxAge: freshness}

	v := VerifyCurrent(s, ctxA, moved)
	if v.Status != convergence.ConditionUnknown || v.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("moved declared input must invalidate: %+v", v)
	}
	if !strings.Contains(v.Detail, shaBaseA2) || !strings.Contains(v.Detail, shaBaseA) {
		t.Fatalf("verdict must name the movement: %s", v.Detail)
	}
	d := gateDecision(t, s, ctxA, moved, ModeEnforce)
	if d.Admitted {
		t.Fatalf("invalidated predicate must withhold its transition: %+v", d)
	}
	if d.Reason != convergence.ReasonDependencyUnknown {
		t.Fatalf("withheld as Unknown, never False satisfaction: %+v", d)
	}
}

// Row 3: the same input change does not touch a proof that never declared it.
func TestRow_UndeclaredProofSurvivesInputChange(t *testing.T) {
	s, now := seededStore(t)
	moved := currentInputs()
	moved.Current[AssumptionName(AssumptionBaseSHA, "acme/widgets")] = shaBaseA2
	ctxB := Context{Required: fpB(), Now: now, MaxAge: freshness}

	v := VerifyCurrent(s, ctxB, moved)
	if !v.Satisfied() {
		t.Fatalf("proof not declaring the moved input must stay current: %+v", v)
	}
	if d := gateDecision(t, s, ctxB, moved, ModeEnforce); !d.Admitted {
		t.Fatalf("unrelated transition must remain admitted: %+v", d)
	}
}

// Row 4: new exact-subject evidence for the new input value restores exactly
// the dependent predicate — a new receipt under a new fingerprint, never an
// edit of history.
func TestRow_NewEvidenceRestoresOnlyThatPredicate(t *testing.T) {
	s, now := seededStore(t)
	moved := currentInputs()
	baseName := AssumptionName(AssumptionBaseSHA, "acme/widgets")
	moved.Current[baseName] = shaBaseA2

	// Re-observation at the new base: new fingerprint (new head after rebase
	// is typical, but even same-head/new-base is a distinct subject binding).
	fpNew := fpA()
	fpNew.BaseSHA = shaBaseA2
	fpNew.HeadSHA = shaHeadA2
	later := now.Add(time.Minute)
	if _, err := s.Put(greenRecord(t, fpNew, later)); err != nil {
		t.Fatalf("Put new evidence: %v", err)
	}

	ctxNew := Context{Required: fpNew, Now: later, MaxAge: freshness}
	if v := VerifyCurrent(s, ctxNew, moved); !v.Satisfied() {
		t.Fatalf("new exact-subject evidence must restore the predicate: %+v", v)
	}
	// The OLD receipt remains durably stored, still invalidated — history is
	// retained, never latched back to true.
	ctxOld := Context{Required: fpA(), Now: later, MaxAge: freshness}
	if v := VerifyCurrent(s, ctxOld, moved); v.Status != convergence.ConditionUnknown {
		t.Fatalf("old judgment must stay non-current: %+v", v)
	}
}

// Rows 5+6: subject identity is untouched by #4254 — head X never authorizes
// Y and generation G never satisfies G+1, through the same VerifyCurrent seam.
func TestRow_HeadAndGenerationStillFence(t *testing.T) {
	s, now := seededStore(t)
	inputs := currentInputs()

	headMoved := fpA()
	headMoved.HeadSHA = shaHeadA2
	if v := VerifyCurrent(s, Context{Required: headMoved, Now: now, MaxAge: freshness}, inputs); v.Satisfied() {
		t.Fatalf("X's receipt must never authorize Y: %+v", v)
	}

	genMoved := fpA()
	genMoved.DesiredGeneration = 2
	if v := VerifyCurrent(s, Context{Required: genMoved, Now: now, MaxAge: freshness}, inputs); v.Satisfied() {
		t.Fatalf("G's receipt must never satisfy G+1: %+v", v)
	}
}

// Row 7: duplicate or out-of-order input events cannot regress or latch
// truth — the judgment is a pure function of the CURRENT snapshot, so
// re-evaluating with the current snapshot after any event sequence yields the
// current answer, and an event replaying an OLD value is just a snapshot the
// next evaluation overrides.
func TestRow_ArrivalOrderCannotLatch(t *testing.T) {
	s, now := seededStore(t)
	ctxA := Context{Required: fpA(), Now: now, MaxAge: freshness}
	baseName := AssumptionName(AssumptionBaseSHA, "acme/widgets")

	moved := currentInputs()
	moved.Current[baseName] = shaBaseA2
	stale := currentInputs() // an out-of-order replay of the old value

	// moved → stale replay → moved again: each evaluation answers ONLY for
	// its snapshot; nothing persists a verdict anywhere.
	if v := VerifyCurrent(s, ctxA, moved); v.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("moved snapshot must invalidate: %+v", v)
	}
	if v := VerifyCurrent(s, ctxA, stale); !v.Satisfied() {
		t.Fatalf("judgment is level-triggered on the snapshot in hand: %+v", v)
	}
	if v := VerifyCurrent(s, ctxA, moved); v.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("current authoritative state must win after any order: %+v", v)
	}
}

// Row 8: restart. The invalidated/current status reconstructs from the
// durable receipt and a fresh observation alone — no process memory.
func TestRow_RestartReconstructsStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proofs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	now := time.Now()
	if _, err := s.Put(greenRecord(t, fpA(), now)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	moved := currentInputs()
	moved.Current[AssumptionName(AssumptionBaseSHA, "acme/widgets")] = shaBaseA2
	ctxA := Context{Required: fpA(), Now: now, MaxAge: freshness}

	before := VerifyCurrent(s, ctxA, moved)

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	after := VerifyCurrent(reopened, ctxA, moved)
	if before != after || after.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("restart must reconstruct the same status: before=%+v after=%+v", before, after)
	}
	healthy := VerifyCurrent(reopened, ctxA, currentInputs())
	if !healthy.Satisfied() {
		t.Fatalf("restart must not invent invalidation either: %+v", healthy)
	}
}

// Row 9: racing input writers and verifiers serialize on the store's existing
// key seams; run under -race in CI.
func TestRow_ConcurrentJudgmentsAreSafe(t *testing.T) {
	s, now := seededStore(t)
	moved := currentInputs()
	moved.Current[AssumptionName(AssumptionBaseSHA, "acme/widgets")] = shaBaseA2
	ctxA := Context{Required: fpA(), Now: now, MaxAge: freshness}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(alt bool) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if alt {
					_ = VerifyCurrent(s, ctxA, moved)
				} else {
					_ = VerifyCurrent(s, ctxA, currentInputs())
				}
			}
		}(i%2 == 0)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		fpNew := fpA()
		fpNew.BaseSHA = shaBaseA2
		fpNew.HeadSHA = shaHeadA2
		_, _ = s.Put(greenRecord(t, fpNew, now.Add(time.Second)))
	}()
	wg.Wait()
}

// Row 10: a declared input whose observer is unavailable makes ONLY the
// declaring receipt Unknown; the unrelated positive control stays available.
func TestRow_DegradedInputObserverIsLocal(t *testing.T) {
	s, now := seededStore(t)
	inputs := currentInputs()
	baseName := AssumptionName(AssumptionBaseSHA, "acme/widgets")
	delete(inputs.Current, baseName)
	inputs.Degraded = map[string]string{baseName: "base observer unavailable"}

	vA := VerifyCurrent(s, Context{Required: fpA(), Now: now, MaxAge: freshness}, inputs)
	if vA.Status != convergence.ConditionUnknown || vA.Reason != ReasonAssumptionUnknown {
		t.Fatalf("degraded declared input must be Unknown, never satisfaction: %+v", vA)
	}
	vB := VerifyCurrent(s, Context{Required: fpB(), Now: now, MaxAge: freshness}, inputs)
	if !vB.Satisfied() {
		t.Fatalf("failure domain must stay local: %+v", vB)
	}
}

// A definite invalidation outranks a degraded observer, mirroring the
// evaluator's "definite blocker first" ordering.
func TestJudgeAssumptions_DefiniteMismatchBeatsDegraded(t *testing.T) {
	s, now := seededStore(t)
	rec, _ := s.Get(fpA().Key())
	_ = now
	inputs := currentInputs()
	inputs.Current[AssumptionName(AssumptionCheckPolicy, "acme/widgets")] = "sha256:other"
	baseName := AssumptionName(AssumptionBaseSHA, "acme/widgets")
	delete(inputs.Current, baseName)
	inputs.Degraded = map[string]string{baseName: "unavailable"}

	v, demoted := rec.JudgeAssumptions(inputs)
	if !demoted || v.Reason != ReasonAssumptionInvalidated {
		t.Fatalf("definite movement must be reported over degradation: %+v", v)
	}
}

// An input outside the snapshot (never observed) changes nothing: a partial
// observation cannot silently confirm OR deny assumptions nobody re-checked.
func TestJudgeAssumptions_UnobservedInputIsOutsideTheSnapshot(t *testing.T) {
	s, now := seededStore(t)
	rec, _ := s.Get(fpA().Key())
	inputs := InputObservation{
		// A stale map value exists but the input was NOT marked observed.
		Current: map[string]string{AssumptionName(AssumptionBaseSHA, "acme/widgets"): shaBaseA2},
	}
	if v, demoted := rec.JudgeAssumptions(inputs); demoted {
		t.Fatalf("unobserved input must not reach the judgment: %+v", v)
	}
	_ = now
}

// Row 11 (bypass RED): the checked seams must actually be load-bearing.
func TestRow_BypassesFail(t *testing.T) {
	s, now := seededStore(t)
	moved := currentInputs()
	moved.Current[AssumptionName(AssumptionBaseSHA, "acme/widgets")] = shaBaseA2
	ctxA := Context{Required: fpA(), Now: now, MaxAge: freshness}

	// If VerifyCurrent skipped the assumption judgment it would answer True
	// here — the invalidation seam is proven load-bearing by row 2; this row
	// proves the DEPENDENT GATE is load-bearing: with the edge projected, the
	// production evaluator withholds; with the mode not exactly enforce, no
	// edge exists and admission is byte-identical to today.
	if d := gateDecision(t, s, ctxA, moved, ModeEnforce); d.Admitted {
		t.Fatalf("enforce mode must withhold on an invalidated proof: %+v", d)
	}
	for _, mode := range []string{ModeOff, ModeShadow, "", "ENFORCE", "on"} {
		if d := gateDecision(t, s, ctxA, moved, mode); !d.Admitted {
			t.Fatalf("mode %q must be inert (default-off / shadow-never-enforces): %+v", mode, d)
		}
	}
}

// Row 12: a nil store or missing receipt keeps the existing #4253 verdicts —
// selective invalidation adds no new authority over absent evidence.
func TestVerifyCurrent_MissingEvidenceUnchanged(t *testing.T) {
	now := time.Now()
	ctxA := Context{Required: fpA(), Now: now, MaxAge: freshness}
	if v := VerifyCurrent(nil, ctxA, currentInputs()); v.Reason != ReasonProofMissing {
		t.Fatalf("nil store: %+v", v)
	}
	path := filepath.Join(t.TempDir(), "proofs.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if v := VerifyCurrent(s, ctxA, currentInputs()); v.Reason != ReasonProofMissing {
		t.Fatalf("empty store: %+v", v)
	}
}

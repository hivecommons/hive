package convergence

import "strconv"

// This file is the generation-comparison seam added for kubestellar/hive#4251
// (parent #3845), consuming the outcome authority accepted on #4249. It is
// deliberately additive: an Observation whose Outcome field is nil — every
// observation produced today, and every observation produced while the
// convergence mode toggle is off — flows through Evaluate exactly as before,
// byte-identical conditions and all. Only an observer that both holds a
// declared outcome record AND has the toggle enabled ever populates Outcome
// (see pkg/convergence/outcome.Record.AdmissionStatus, which returns nil when
// the mode is off).

// OutcomeStatus binds one candidate to one declared outcome for admission:
// the canonical outcome key, WHICH generation of desired state is current,
// and WHICH generation the observer authoritatively read. Values are copied
// out of the ledger, never shared, so evaluation can never race a writer.
type OutcomeStatus struct {
	// Key is the canonical "project/repo@outcome" identity string.
	Key string
	// DesiredGeneration is the ledger's current desired generation.
	DesiredGeneration int
	// ObservedGeneration is the generation of the declaration the observer
	// authoritatively read this pass; zero means it was never observed.
	ObservedGeneration int
}

// Outcome condition vocabulary. Deliberately narrow: matching generations
// establishes ONLY that the current declaration generation was observed. It
// never establishes predicate truth, proof validity, repository Converged, or
// quiescence — those are later increments (B2/B3), and emitting them here
// would fabricate satisfaction before anything computes them.
const (
	// ConditionOutcomeGenerationObserved reports whether the observed
	// generation equals the current desired generation of the bound outcome.
	ConditionOutcomeGenerationObserved = "OutcomeGenerationObserved"

	// ReasonOutcomeGenerationMatch: the declaration was authoritatively
	// observed at exactly the current desired generation.
	ReasonOutcomeGenerationMatch = "OutcomeGenerationMatch"
	// ReasonOutcomeGenerationStale: the observed generation differs from the
	// current desired generation (including "never observed"). Evidence for
	// any other generation can never authorize a transition on this one.
	ReasonOutcomeGenerationStale = "OutcomeGenerationStale"
)

// evaluateOutcomeGate applies the generation comparison for an
// outcome-bound observation. It reports (decision, true) when the gate blocks
// — observed generation differs from desired — and (zero, false) when the
// gate passes and the ordinary admission rules should run.
//
// A mismatch yields Ready=Unknown, not False: nothing is established about
// the candidate except that the evidence in hand belongs to some other
// revision of desired state, so the caller should re-observe, not conclude.
// The stale observed generation remains visible in the decision rather than
// being erased, so an operator can see exactly which revision was judged.
func evaluateOutcomeGate(obs Observation) (Decision, bool) {
	oc := obs.Outcome
	if oc == nil || oc.ObservedGeneration == oc.DesiredGeneration {
		return Decision{}, false
	}
	msg := outcomeGenerationMessage(oc)
	d := Decision{
		ObservedRecord:     obs.RecordID,
		ObservedGeneration: obs.Generation,
		Reason:             ReasonOutcomeGenerationStale,
		Blockers:           []string{oc.Key},
		Outcome:            copyOutcomeStatus(oc),
		Conditions: []Condition{
			{Type: ConditionOutcomeGenerationObserved, Status: ConditionFalse,
				Reason: ReasonOutcomeGenerationStale, Message: msg},
			{Type: ConditionReady, Status: ConditionUnknown,
				Reason:  ReasonOutcomeGenerationStale,
				Message: "not admitted: " + msg},
		},
	}
	return d, true
}

// appendOutcomeMatch records, on an already-computed decision, that the bound
// outcome's declaration was observed at the current desired generation. It
// adds exactly one condition and the echoed status; it never flips Admitted,
// because a matched generation authorizes nothing by itself — the ordinary
// admission rules already ran and their verdict stands.
func appendOutcomeMatch(d Decision, oc *OutcomeStatus) Decision {
	d.Outcome = copyOutcomeStatus(oc)
	d.Conditions = append(d.Conditions, Condition{
		Type:   ConditionOutcomeGenerationObserved,
		Status: ConditionTrue,
		Reason: ReasonOutcomeGenerationMatch,
		Message: outcomeGenerationMessage(oc) +
			"; generation match does not establish predicate truth or convergence",
	})
	return d
}

// copyOutcomeStatus detaches the echoed status so the decision holds
// immutable values of its own.
func copyOutcomeStatus(oc *OutcomeStatus) *OutcomeStatus {
	cp := *oc
	return &cp
}

func outcomeGenerationMessage(oc *OutcomeStatus) string {
	if oc.ObservedGeneration == oc.DesiredGeneration {
		return "outcome " + oc.Key + " observed at current desired generation " + itoa(oc.DesiredGeneration)
	}
	if oc.ObservedGeneration == 0 {
		return "outcome " + oc.Key + " declaration never observed; desired generation is " + itoa(oc.DesiredGeneration)
	}
	return "outcome " + oc.Key + " observed at generation " + itoa(oc.ObservedGeneration) +
		" but desired generation is " + itoa(oc.DesiredGeneration)
}

func itoa(n int) string { return strconv.Itoa(n) }

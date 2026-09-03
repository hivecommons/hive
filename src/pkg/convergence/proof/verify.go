package proof

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/convergence"
)

// Convergence mode values, mirroring the convergence.mode /
// HIVE_CONVERGENCE_MODE toggle surface (#4246, formalised by #4263). The
// resolution rule is the same default-off one used by config.ConvergenceMode
// and pkg/convergence/outcome: anything that is not an exact recognised mode
// is OFF. Callers pass config.ConvergenceMode()'s resolved value through.
const (
	// ModeOff disables every proof surface: nothing is recorded, verified,
	// reported, or gated. Byte-identical behavior to today.
	ModeOff = "off"
	// ModeShadow records and reports proof verdicts but NEVER enforces one:
	// no transition is withheld on a proof in shadow.
	ModeShadow = "shadow"
	// ModeEnforce lets a proof verdict gate ONLY the one dependent transition
	// explicitly declared to depend on it. It is never a default and never
	// selected by fallback.
	ModeEnforce = "enforce"
)

// RecordingEnabled reports whether the resolved convergence mode enables
// observing and recording proof receipts at all (shadow or enforce).
// Default-off: only an exact recognised enabling mode answers true.
func RecordingEnabled(mode string) bool {
	return mode == ModeShadow || mode == ModeEnforce
}

// EnforcementEnabled reports whether a proof verdict may gate its one
// declared dependent transition. ONLY the exact enforce mode answers true —
// shadow reports, never enforces.
func EnforcementEnabled(mode string) bool {
	return mode == ModeEnforce
}

// Verdict reasons. Stable machine-readable strings; callers log and test
// against them, so treat them as API.
const (
	// ReasonProofCurrent: a valid receipt matches the complete current
	// fingerprint, its result is success, and its freshness rule holds.
	ReasonProofCurrent = "ProofCurrent"
	// ReasonProofMissing: no receipt exists for the required subject.
	ReasonProofMissing = "ProofMissing"
	// ReasonProofMalformed: the receipt in hand fails validation.
	ReasonProofMalformed = "ProofMalformed"
	// ReasonFingerprintMismatch: at least one load-bearing fingerprint field
	// differs from the current evaluation context — a repaired head, a
	// superseded generation, a moved base, a changed check policy, a
	// different outcome/predicate/producer. Evidence for another subject
	// never transfers.
	ReasonFingerprintMismatch = "FingerprintMismatch"
	// ReasonProofExpired: the receipt's observation is older than the
	// freshness window; it must be re-observed before it can satisfy again.
	ReasonProofExpired = "ProofExpired"
	// ReasonPredicateFailed: current evidence establishes the predicate as
	// NOT satisfied (a definite red, not an unknown).
	ReasonPredicateFailed = "PredicateFailed"
	// ReasonEvidencePending: the producer has not finished producing evidence
	// for the exact subject.
	ReasonEvidencePending = "EvidencePending"
)

// Context is the CURRENT evaluation context a receipt must match to satisfy
// the predicate: the fingerprint current truth requires (current desired
// generation, current head, current base observation, current check policy),
// the evaluation instant, and the freshness window. It is supplied fresh on
// every evaluation — proofs are level-triggered, never event-latched.
type Context struct {
	// Required is the complete fingerprint the CURRENT state of the world
	// demands. Every field must match the receipt exactly.
	Required Fingerprint
	// Now is the evaluation instant.
	Now time.Time
	// MaxAge is the freshness window: a receipt observed more than MaxAge
	// before Now is expired and must be re-observed. Zero or negative means
	// no receipt is ever fresh — the conservative direction.
	MaxAge time.Duration
}

// Verdict is the tri-state judgment of one receipt against one Context, in
// the same True/False/Unknown vocabulary convergence.Evaluate consumes.
// Satisfied is true ONLY for a current, complete, matching, successful proof.
type Verdict struct {
	// Status is True (predicate proven), False (predicate established
	// unsatisfied by current evidence), or Unknown (nothing established:
	// missing, mismatched, stale, malformed, or pending evidence).
	Status convergence.ConditionStatus
	// Reason is one of the Reason* constants above.
	Reason string
	// Detail is a short human-readable note naming the mismatched field or
	// instants for logs and operator-facing messages.
	Detail string
}

// Satisfied reports whether the verdict proves the predicate. It is the ONLY
// thing a dependent gate may key on.
func (v Verdict) Satisfied() bool { return v.Status == convergence.ConditionTrue }

// VerifyAgainst judges a receipt held in hand against the current context.
// The rules, in order — every early return is a refusal to satisfy:
//
//  1. A malformed receipt establishes nothing (Unknown, ProofMalformed).
//  2. ANY fingerprint field mismatch establishes nothing for THIS subject
//     (Unknown, FingerprintMismatch, naming the first differing field).
//     Head X→Y, generation G→G+1, base movement, policy change, another
//     outcome/predicate/producer — all land here, unconditionally.
//  3. An expired receipt establishes nothing (Unknown, ProofExpired).
//  4. A fresh matching receipt then maps its result: success → True,
//     failure → False (PredicateFailed), pending → Unknown (EvidencePending).
func (r Record) VerifyAgainst(ctx Context) Verdict {
	if err := r.Validate(); err != nil {
		return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonProofMalformed, Detail: err.Error()}
	}
	if err := ctx.Required.Validate(); err != nil {
		return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonProofMalformed,
			Detail: "required fingerprint is malformed: " + err.Error()}
	}
	if field, ok := firstMismatch(r.Fingerprint, ctx.Required); ok {
		return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonFingerprintMismatch,
			Detail: "receipt does not bind the current subject: " + field}
	}
	if ctx.MaxAge <= 0 || ctx.Now.Sub(r.ObservedAt) > ctx.MaxAge {
		return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonProofExpired,
			Detail: fmt.Sprintf("observed %s is outside the freshness window ending %s",
				r.ObservedAt.Format(time.RFC3339), ctx.Now.Format(time.RFC3339))}
	}
	switch r.Result {
	case ResultSuccess:
		return Verdict{Status: convergence.ConditionTrue, Reason: ReasonProofCurrent,
			Detail: "all required checks green for exact head " + r.Fingerprint.HeadSHA}
	case ResultFailure:
		return Verdict{Status: convergence.ConditionFalse, Reason: ReasonPredicateFailed,
			Detail: "required checks failed for exact head " + r.Fingerprint.HeadSHA}
	default:
		return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonEvidencePending,
			Detail: "required checks incomplete for exact head " + r.Fingerprint.HeadSHA}
	}
}

// firstMismatch names the first load-bearing field on which the receipt's
// fingerprint differs from the required one. Field-by-field (rather than a
// struct compare) so the verdict can SAY which assumption moved — the seed
// selective invalidation (B3) grows from.
func firstMismatch(got, want Fingerprint) (string, bool) {
	switch {
	case got.OutcomeKey != want.OutcomeKey:
		return fmt.Sprintf("outcome key %q != required %q", got.OutcomeKey, want.OutcomeKey), true
	case got.PredicateID != want.PredicateID:
		return fmt.Sprintf("predicate %q != required %q", got.PredicateID, want.PredicateID), true
	case got.DesiredGeneration != want.DesiredGeneration:
		return fmt.Sprintf("desired generation %d != required %d", got.DesiredGeneration, want.DesiredGeneration), true
	case got.Repo != want.Repo:
		return fmt.Sprintf("repo %q != required %q", got.Repo, want.Repo), true
	case got.PRNumber != want.PRNumber:
		return fmt.Sprintf("pr %d != required %d", got.PRNumber, want.PRNumber), true
	case got.HeadSHA != want.HeadSHA:
		return fmt.Sprintf("head %s != required %s", got.HeadSHA, want.HeadSHA), true
	case got.BaseSHA != want.BaseSHA:
		return fmt.Sprintf("base %s != required %s", got.BaseSHA, want.BaseSHA), true
	case got.CheckPolicyID != want.CheckPolicyID:
		return fmt.Sprintf("check policy %s != required %s", got.CheckPolicyID, want.CheckPolicyID), true
	case got.Producer != want.Producer:
		return fmt.Sprintf("producer %q != required %q", got.Producer, want.Producer), true
	}
	return "", false
}

// Verify looks up the receipt for the required subject in the store and
// judges it against the context. A missing store or missing receipt is
// Unknown (ProofMissing) — an absent proof can never satisfy, and a missing
// or unreadable store is a visible failure, never satisfaction.
func Verify(s *Store, ctx Context) Verdict {
	if s == nil {
		return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonProofMissing,
			Detail: "no proof store is available"}
	}
	key := ctx.Required.Key()
	rec, ok := s.Get(key)
	if !ok {
		return Verdict{Status: convergence.ConditionUnknown, Reason: ReasonProofMissing,
			Detail: "no receipt exists for " + key}
	}
	return rec.VerifyAgainst(ctx)
}

// AdmissionDependency projects a verdict into the existing convergence
// Evaluate seam as one Dependency edge, gated by the convergence mode toggle.
//
// It returns nil unless the mode is EXACTLY enforce — and a nil Dependency
// contributes nothing to an Observation, which is the default-off and
// shadow-never-enforces guarantee in one place: with the toggle off nothing
// is consulted at all, and in shadow the verdict is recorded and reportable
// (the caller may surface it) but can never withhold a transition. Only the
// one candidate whose observer explicitly declares this proof dependency is
// ever gated; every other candidate never receives the edge and retains
// existing admission behavior through convergence.Evaluate's ordinary rules —
// True passes, False blocks (WaitingForDependency), Unknown blocks as
// Unknown (DependencyUnknown), exactly rule 3/4 of the existing evaluator.
func (v Verdict) AdmissionDependency(mode string, ctx Context) *convergence.Dependency {
	if !EnforcementEnabled(mode) {
		return nil
	}
	return &convergence.Dependency{
		ID:     "proof:" + ctx.Required.Key(),
		Status: v.Status,
		Detail: v.Reason + ": " + v.Detail,
	}
}

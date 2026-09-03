package mutation

import (
	"fmt"
	"time"

	"github.com/hivecommons/hive/pkg/convergence/proof"
)

// Convergence mode gating mirrors the proof package's resolved-mode contract
// (#4246/#4263): anything that is not an exact recognised enabling mode is
// OFF. The mode values are shared with pkg/convergence/proof so the two
// verticals can never disagree about what "off" means.

// JournalingEnabled reports whether the resolved convergence mode records
// mutation claims and journal entries at all (shadow or enforce).
func JournalingEnabled(mode string) bool { return proof.RecordingEnabled(mode) }

// FencingEnabled reports whether a stale epoch may actually withhold an
// external effect. ONLY the exact enforce mode fences — shadow records and
// reports, never withholds.
func FencingEnabled(mode string) bool { return proof.EnforcementEnabled(mode) }

// EffectFunc performs the one external effect and returns bounded provenance
// of the applied result (e.g. the created PR URL). An error means the effect
// outcome is UNCERTAIN — it may or may not have taken place externally — and
// the operation must be reconciled before any retry.
type EffectFunc func() (result string, err error)

// Executor binds the durable claim ledger and operation journal around the
// actual mutation boundary for the selected effect. It adds fencing and
// idempotency; it never replaces the effect's own guards (CreatePR's
// open-PR-by-head dedupe and 422 recovery remain mandatory defense-in-depth).
type Executor struct {
	Ledger  *Ledger
	Journal *Journal
	// Mode is the resolved convergence mode; the executor is a transparent
	// passthrough at off, records-but-never-fences at shadow, and fences at
	// enforce.
	Mode string
	// Now supplies the clock; nil means time.Now.
	Now func() time.Time
}

func (x Executor) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now()
}

// Execute performs one logical operation under one claimed epoch:
//
//  1. mode off → the effect runs directly; no ledger or journal is touched
//     (byte-identical legacy behavior, the default);
//  2. epoch validation at the ACTUAL mutation boundary (fenced at enforce;
//     recorded but not fenced at shadow);
//  3. operation intent persisted durably BEFORE the effect (Begin), finding
//     the same logical entry on retry/reassignment;
//  4. the external effect;
//  5. epoch validation AGAIN before authoritative acknowledgment — a hold
//     that expired or was reassigned mid-effect may not acknowledge at
//     enforce: the result is recorded as Unknown for reconciliation instead;
//  6. the observed result persisted (RecordResult).
//
// An effect error records Unknown — never NotApplied, because an errored call
// may still have taken effect externally — and returns the error; Reconcile
// against authoritative external state then resolves the same logical
// operation before any retry.
func (x Executor) Execute(e Effect, epoch uint64, holder string, effect EffectFunc) (Operation, error) {
	if !JournalingEnabled(x.Mode) {
		result, err := effect()
		if err != nil {
			return Operation{}, err
		}
		return Operation{Status: StatusApplied, Result: result}, nil
	}
	if x.Ledger == nil || x.Journal == nil {
		return Operation{}, fmt.Errorf("mutation executor requires a ledger and journal when mode is enabled")
	}
	claimKey := e.ClaimKey
	now := x.now()

	// Boundary check 1: only the current durable epoch may authorize.
	if err := x.Ledger.ValidateEpoch(claimKey, epoch, now); err != nil {
		if FencingEnabled(x.Mode) {
			return Operation{}, fmt.Errorf("mutation boundary fenced: %w", err)
		}
		// Shadow observes the violation but never withholds.
	}

	// Intent before effect, durably.
	op, err := x.Journal.Begin(e, epoch, holder, now)
	if err != nil {
		return Operation{}, err
	}

	result, effectErr := effect()
	after := x.now()

	if effectErr != nil {
		// Uncertain: the call failed but the effect may exist externally.
		if _, recErr := x.Journal.RecordResult(op.LogicalID, epoch, StatusUnknown, "", after); recErr != nil {
			return Operation{}, fmt.Errorf("recording uncertain effect: %v (effect error: %w)", recErr, effectErr)
		}
		return Operation{}, fmt.Errorf("effect outcome uncertain, reconciliation required: %w", effectErr)
	}

	// Boundary check 2: only the current durable epoch may ACKNOWLEDGE.
	if err := x.Ledger.ValidateEpoch(claimKey, epoch, after); err != nil && FencingEnabled(x.Mode) {
		// The effect happened but this owner may no longer speak for it:
		// record Unknown so the CURRENT owner reconciles the same logical
		// operation from authoritative external state — never a duplicate.
		if _, recErr := x.Journal.RecordResult(op.LogicalID, epoch, StatusUnknown, "", after); recErr != nil {
			return Operation{}, fmt.Errorf("recording unacknowledgeable effect: %v (fence: %w)", recErr, err)
		}
		return Operation{}, fmt.Errorf("acknowledgment fenced, reconciliation required: %w", err)
	}

	return x.Journal.RecordResult(op.LogicalID, epoch, StatusApplied, result, after)
}

package mutation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

const defaultBoundaryTTL = 10 * time.Minute

// DefaultStuckAfter is how long a journaled operation may sit unresolved
// (Planned/Unknown) before a refused retry is reported through OnStuck. It is
// three boundary TTLs: long enough for the owner's own reconciliation to run,
// short enough that an operator learns about a clogged queue the same hour.
const DefaultStuckAfter = 30 * time.Minute

// Boundary adapts Executor to the narrow pkg/effects seam used by GitHub and
// push call sites. It acquires a per-(repo, kind, target) claim for each
// external mutation, journals the operation, and releases the claim after the
// boundary returns.
type Boundary struct {
	Executor Executor
	Holder   string
	TTL      time.Duration
	Logger   *slog.Logger
	Stats    *effects.Recorder
	Mode     func() string
	// OnStuck, when set, is called each time Execute refuses an effect because
	// its journaled operation has been unresolved for at least StuckAfter
	// (DefaultStuckAfter when zero). The caller keeps retrying the same
	// logical ID forever otherwise (observed on a live hive: one Unknown op,
	// 175 refused retries over six hours), so this is the queue-health hook.
	OnStuck func(op Operation, age time.Duration)
	// OnResolved, when set, is called with the logical ID once an operation
	// leaves the unresolved state: applied, already applied, or reconciled.
	OnResolved func(logicalID string)
	StuckAfter time.Duration
}

// ExecuteJournaled preserves legacy Execute's off-mode passthrough while
// providing an explicit fail-closed contract for new journal-required effects.
func (b *Boundary) ExecuteJournaled(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	if b == nil {
		return effects.Result{}, fmt.Errorf("%w: mutation journal unavailable", effects.ErrDenied)
	}
	frozen := *b
	if b.Mode != nil {
		frozen.Executor.Mode = b.Mode()
	}
	frozen.Mode = nil
	if !JournalingEnabled(frozen.Executor.Mode) || frozen.Executor.Ledger == nil || frozen.Executor.Journal == nil ||
		frozen.Executor.Journal.path == "" {
		return effects.Result{}, fmt.Errorf("%w: active persistent mutation journal required", effects.ErrDenied)
	}
	invoked := false
	out, err := frozen.Execute(ctx, claim, func(ctx context.Context) (effects.Result, error) {
		invoked = true
		if err := frozen.Executor.Journal.syncPersistent(); err != nil {
			return effects.Result{}, effects.NotApplied(fmt.Errorf("syncing mutation intent before effect: %w", err))
		}
		return effect(ctx)
	})
	if invoked || err == nil || out.Provenance != "" {
		if syncErr := frozen.Executor.Journal.syncPersistent(); syncErr != nil {
			return out, fmt.Errorf("mutation journal acknowledgement not synced: %w (execution: %v)", syncErr, err)
		}
	}
	return out, err
}

func (b *Boundary) Execute(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	if b == nil {
		return effect(ctx)
	}
	executor := b.Executor
	if b.Mode != nil {
		executor.Mode = b.Mode()
	}
	if b.Stats != nil {
		b.Stats.SetMode(executor.Mode)
	}
	if !JournalingEnabled(executor.Mode) {
		return effect(ctx)
	}
	if err := claim.Validate(); err != nil {
		return effects.Result{}, fmt.Errorf("%w: invalid mutation claim: %v", effects.ErrDenied, err)
	}
	if executor.Ledger == nil || executor.Journal == nil {
		return effects.Result{}, fmt.Errorf("mutation boundary requires a ledger and journal when mode is enabled")
	}
	_ = RepoClaim(claim.Repo).Key()
	holder := strings.TrimSpace(claim.Actor)
	if holder == "" {
		holder = strings.TrimSpace(b.Holder)
	}
	if holder == "" {
		holder = "hive"
	}
	ttl := b.TTL
	if ttl <= 0 {
		ttl = defaultBoundaryTTL
	}
	now := executor.now()
	mclaim := TaskClaim(claim.Repo, claimSubject(claim))
	entry, acquireErr := executor.Ledger.Acquire(mclaim, holder, ttl, now)
	if acquireErr != nil {
		if FencingEnabled(executor.Mode) {
			if b.Stats != nil {
				b.Stats.IncDenied()
			}
			return effects.Result{}, fmt.Errorf("%w: %v", effects.ErrDenied, acquireErr)
		}
		if b.Logger != nil {
			b.Logger.Warn("shadow fence would have denied external mutation",
				"repo", claim.Repo, "kind", claim.Kind, "target", claim.Target, "error", acquireErr)
		}
		if b.Stats != nil {
			b.Stats.IncFenced()
		}
		entry = Entry{Claim: mclaim, Holder: holder, Epoch: 0}
	}
	mutEffect := boundaryEffect(claim)
	var out effects.Result
	op, execErr := executor.Execute(mutEffect, entry.Epoch, holder, func() (string, error) {
		res, err := effect(ctx)
		out = res
		return res.Provenance, err
	})
	// The claim guards the window of the external call only. Every exit path
	// must release it: a claim left ActiveMutation after a refused or failed
	// effect holds its writer slot until TTL, and at enforce that denies
	// every other mutation in the same writer scope for ten minutes (observed
	// on a live hive as "repo ... writers exhausted (max 1)" behind one stuck
	// review while the slot was per repo; with TargetWriterScope it is
	// "target owner/repo#N writers exhausted" and only that item waits).
	b.release(executor, claim, mclaim, entry)
	logicalID := mutEffect.LogicalID()
	if execErr != nil {
		if errors.Is(execErr, ErrAlreadyApplied) {
			b.noteResolved(logicalID)
			// The journal says this exact effect already happened. The
			// caller's desired state holds, so this is idempotent success:
			// hand back the recorded provenance (Execute returns the
			// journaled operation on a replay) rather than an error that a
			// retry loop would replay forever.
			out.Provenance = op.Result
			if b.Logger != nil {
				b.Logger.Info("external mutation already applied; treating as idempotent success",
					"repo", claim.Repo, "kind", claim.Kind, "target", claim.Target)
			}
			return out, nil
		}
		if errors.Is(execErr, ErrClaimHeld) || errors.Is(execErr, ErrStaleEpoch) {
			if b.Stats != nil {
				b.Stats.IncDenied()
			}
			return out, fmt.Errorf("%w: %v", effects.ErrDenied, execErr)
		}
		if errors.Is(execErr, ErrNeedsReconciliation) {
			b.noteStuck(executor, logicalID)
			return out, fmt.Errorf("%w: %w", effects.ErrNeedsReconciliation, execErr)
		}
		return out, execErr
	}
	if b.Stats != nil && op.LogicalID != "" {
		b.Stats.IncJournaled()
	}
	b.noteResolved(logicalID)
	return out, nil
}

// noteStuck reports the unresolved operation behind a refused retry through
// OnStuck once it has been unresolved for StuckAfter.
func (b *Boundary) noteStuck(executor Executor, logicalID string) {
	if b.OnStuck == nil || executor.Journal == nil {
		return
	}
	op, ok := executor.Journal.Get(logicalID)
	if !ok || (op.Status != StatusUnknown && op.Status != StatusPlanned) {
		return
	}
	after := b.StuckAfter
	if after <= 0 {
		after = DefaultStuckAfter
	}
	age := executor.now().Sub(op.UpdatedAt)
	if age < after {
		return
	}
	b.OnStuck(op, age)
}

func (b *Boundary) noteResolved(logicalID string) {
	if b.OnResolved != nil && logicalID != "" {
		b.OnResolved(logicalID)
	}
}

// Reconcile resolves the journaled operation for claim from an authoritative
// observation of external state (effects.Reconciler): Applied records the
// found effect without repeating it, !Applied authorizes the same logical
// operation to retry. Off mode journals nothing, so there is nothing to do.
func (b *Boundary) Reconcile(_ context.Context, claim effects.Claim, state effects.ExternalState) error {
	if b == nil {
		return nil
	}
	executor := b.Executor
	if b.Mode != nil {
		executor.Mode = b.Mode()
	}
	if !JournalingEnabled(executor.Mode) {
		return nil
	}
	if err := claim.Validate(); err != nil {
		return fmt.Errorf("invalid mutation claim: %v", err)
	}
	if executor.Journal == nil {
		return fmt.Errorf("mutation boundary requires a journal when mode is enabled")
	}
	op, err := executor.Journal.Reconcile(boundaryEffect(claim).LogicalID(), ExternalState{
		Known:   true,
		Applied: state.Applied,
		Result:  state.Provenance,
	}, executor.now())
	if err == nil && op.Status != StatusUnknown && op.Status != StatusPlanned {
		b.noteResolved(op.LogicalID)
	}
	return err
}

// ExpireUnresolved implements effects.Expirer: when claim's journaled
// operation is still Planned or Unknown and its last transition is older
// than ttl, it is reconciled to NotApplied so the same logical operation may
// be attempted again. A Known external observation (Reconcile) is always
// preferred; this is for the case where none can be obtained and the entry
// would otherwise fence every retry forever. Off mode journals nothing.
func (b *Boundary) ExpireUnresolved(_ context.Context, claim effects.Claim, ttl time.Duration) (bool, error) {
	if b == nil || ttl <= 0 {
		return false, nil
	}
	executor := b.Executor
	if b.Mode != nil {
		executor.Mode = b.Mode()
	}
	if !JournalingEnabled(executor.Mode) {
		return false, nil
	}
	if err := claim.Validate(); err != nil {
		return false, fmt.Errorf("invalid mutation claim: %v", err)
	}
	if executor.Journal == nil {
		return false, fmt.Errorf("mutation boundary requires a journal when mode is enabled")
	}
	id := boundaryEffect(claim).LogicalID()
	op, ok := executor.Journal.Get(id)
	if !ok || (op.Status != StatusPlanned && op.Status != StatusUnknown) {
		return false, nil
	}
	now := executor.now()
	if now.Sub(op.UpdatedAt) < ttl {
		return false, nil
	}
	if _, err := executor.Journal.Reconcile(id, ExternalState{Known: true, Applied: false}, now); err != nil {
		return false, err
	}
	if b.Logger != nil {
		b.Logger.Warn("external mutation unresolved past TTL; expired to not-applied so a retry may proceed",
			"repo", claim.Repo, "kind", claim.Kind, "target", claim.Target,
			"status", op.Status, "age", now.Sub(op.UpdatedAt).String())
	}
	return true, nil
}

// LookupApplied implements effects.Lookup: it reports claim's journaled
// provenance when the SAME logical ID Execute would derive is already
// Applied. It reads only; it never acquires the ledger claim, journals an
// attempt or runs the effect. Off mode journals nothing, so nothing is found.
func (b *Boundary) LookupApplied(_ context.Context, claim effects.Claim) (effects.Result, bool) {
	if b == nil {
		return effects.Result{}, false
	}
	executor := b.Executor
	if b.Mode != nil {
		executor.Mode = b.Mode()
	}
	if !JournalingEnabled(executor.Mode) || executor.Journal == nil || claim.Validate() != nil {
		return effects.Result{}, false
	}
	op, ok := executor.Journal.Get(boundaryEffect(claim).LogicalID())
	if !ok || op.Status != StatusApplied {
		return effects.Result{}, false
	}
	return effects.Result{Provenance: op.Result}, true
}

// boundaryEffect is the journaled description of claim's external effect;
// Execute and Reconcile must derive the SAME logical ID from it.
func boundaryEffect(claim effects.Claim) Effect {
	mclaim := TaskClaim(claim.Repo, claimSubject(claim))
	inputs := map[string]string{
		"repo":   claim.Repo,
		"kind":   claim.Kind,
		"target": claim.Target,
	}
	for k, v := range claim.Inputs {
		inputs[k] = v
	}
	return Effect{
		OutcomeKey:        claim.Repo + "@mutation",
		DesiredGeneration: 1,
		Transition:        "external." + claim.Kind,
		Subject:           mclaim.Subject,
		ClaimKey:          mclaim.Key(),
		Kind:              claim.Kind,
		Inputs:            inputs,
	}
}

func (b *Boundary) release(executor Executor, claim effects.Claim, mclaim Claim, entry Entry) {
	if entry.Epoch == 0 {
		return
	}
	if _, err := executor.Ledger.Release(mclaim.Key(), entry.Epoch, executor.now()); err != nil && b.Logger != nil {
		b.Logger.Warn("mutation claim release failed", "repo", claim.Repo, "kind", claim.Kind, "target", claim.Target, "error", err)
	}
}

func claimSubject(claim effects.Claim) string {
	return claim.Repo + "#" + claim.Kind + "/" + claim.Target
}

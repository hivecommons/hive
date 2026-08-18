package turn

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// EffectResult is the outcome of one side-effectful operation against the
// remote. ExternalRef is the remote-assigned identity of the effect.
//
// AlreadyExisted mirrors github.CreatePRResult.AlreadyExisted deliberately:
// pkg/github already dedupes PR creation on the head branch and reports reuse
// with that field, so the journal reuses the vocabulary rather than inventing
// a parallel one. A sink may set it when the remote itself recognized the
// effect as pre-existing; the journal treats reuse and fresh creation
// identically for exactly-once purposes.
type EffectResult struct {
	ExternalRef    string
	Output         string
	AlreadyExisted bool
}

// EffectSink performs the actual external write. Implementations are the thin
// GitHub surface (create PR, post comment, push, label); tests substitute a
// counting fake so exactly-once can be asserted at the call boundary rather
// than inferred from the journal alone.
type EffectSink interface {
	Perform(ctx context.Context, in OpIntent) (EffectResult, error)
}

// Reconciler resolves the ambiguous OpIntended window: intent was journaled,
// then the process died before the outcome was. The effect may or may not
// exist remotely, and the local journal cannot know which. Reconcile asks the
// remote.
//
// This is where the "natural GitHub idempotency surrogate" lives: an
// implementation searches for an open PR from the head branch, or lists
// comments matching the body, and returns the existing ref if found. It is a
// query, not a key — see DeriveIdempotencyKey for why the key itself must be
// computable offline.
type Reconciler interface {
	// Reconcile reports whether the effect already exists remotely, and its
	// reference if so.
	Reconcile(ctx context.Context, in OpIntent) (ref string, found bool, err error)
}

// Persister durably stores the envelope at an operation boundary. The journal
// only protects against replay if the intent record actually reaches disk
// before the effect is attempted — that ordering is the entire guarantee.
type Persister interface {
	Persist(ctx context.Context, env SessionEnvelope) error
}

// ErrKilled is returned by a Persister that simulates a process death at a
// chosen operation boundary. The replay test injects it to cut a turn at each
// boundary in turn; production Persisters never return it.
var ErrKilled = errors.New("turn: process killed at operation boundary")

// JournaledExecutor performs side-effectful operations under the journal's
// before/after protocol. It is deliberately separate from Runner: this is a
// prototype surface exercised by tests, and nothing in the live agent loop or
// the running contribute path constructs one.
type JournaledExecutor struct {
	Sink       EffectSink
	Reconciler Reconciler
	Persister  Persister
	// Now is injectable for deterministic tests; defaults to time.Now().UTC.
	Now func() time.Time
}

func (x *JournaledExecutor) now() time.Time {
	if x.Now != nil {
		return x.Now()
	}
	return time.Now().UTC()
}

// Do executes one side-effectful operation exactly once across any number of
// re-entries, mutating env's journal in place.
//
// The protocol, in the order that makes it safe:
//
//  1. Derive the key from the intent (offline, stable).
//  2. Consult the journal. A succeeded entry short-circuits — the effect is
//     already done, return its recorded ref without touching the remote.
//  3. An ambiguous entry (intent written, outcome not) is reconciled against
//     the remote before anything else. Found means the pre-crash attempt
//     landed; settle it as succeeded and return. Not found means it did not;
//     fall through and perform it.
//  4. Otherwise write the OpIntended record and PERSIST IT. If persistence
//     fails, the effect is never attempted — an unpersisted intent would be
//     an unprotected write on the next re-entry.
//  5. Perform the effect.
//  6. Settle the journal with the outcome and persist again.
//
// The window between 4 and 6 is the only place a crash can leave ambiguity,
// and step 3 is what closes it on the next entry.
func (x *JournaledExecutor) Do(ctx context.Context, env *SessionEnvelope, in OpIntent) (EffectResult, error) {
	if env == nil {
		return EffectResult{}, fmt.Errorf("turn: nil envelope")
	}
	if !in.Kind.sideEffectful() {
		return EffectResult{}, fmt.Errorf("turn: %s is not a side-effectful op", in.Kind)
	}

	key := DeriveIdempotencyKey(env.SessionID, in)

	// (2) Already done — the exactly-once short circuit.
	if prior, ok := env.Journal.Lookup(key); ok && prior.Done() {
		return EffectResult{ExternalRef: prior.ExternalRef}, nil
	}

	// (3) Ambiguous window — ask the remote what actually happened.
	if prior, ok := env.Journal.Lookup(key); ok && prior.Ambiguous() && x.Reconciler != nil {
		ref, found, err := x.Reconciler.Reconcile(ctx, in)
		if err != nil {
			return EffectResult{}, fmt.Errorf("reconcile %s: %w", key, err)
		}
		if found {
			env.Journal.Settle(key, OpSucceeded, ref, "", x.now())
			if perr := x.persist(ctx, *env); perr != nil {
				return EffectResult{}, perr
			}
			return EffectResult{ExternalRef: ref}, nil
		}
	}

	// (4) Record intent and persist BEFORE performing.
	env.Journal.RecordIntent(key, in, x.now())
	if err := x.persist(ctx, *env); err != nil {
		return EffectResult{}, err
	}

	// (5) Perform.
	if x.Sink == nil {
		return EffectResult{}, fmt.Errorf("turn: no EffectSink configured")
	}
	res, err := x.Sink.Perform(ctx, in)
	if err != nil {
		env.Journal.Settle(key, OpFailed, "", err.Error(), x.now())
		if perr := x.persist(ctx, *env); perr != nil {
			return EffectResult{}, perr
		}
		return EffectResult{}, err
	}

	// (6) Settle and persist.
	env.Journal.Settle(key, OpSucceeded, res.ExternalRef, "", x.now())
	if perr := x.persist(ctx, *env); perr != nil {
		return EffectResult{}, perr
	}
	return res, nil
}

func (x *JournaledExecutor) persist(ctx context.Context, env SessionEnvelope) error {
	if x.Persister == nil {
		return nil
	}
	return x.Persister.Persist(ctx, env)
}

// SuspendForApproval journals a pending-approval position (#4000) without
// performing any external effect. An operator-approval wait is a suspended
// turn, which is exactly why re-entrancy had to come first: the envelope must
// be able to name "we are stopped here, awaiting a human" as durable state
// rather than a blocked goroutine.
//
// This is the SHAPE only. No approvals UI, no inbox, no routing — those are
// #4000's scope and are not built here. What exists is the guarantee that a
// turn suspended on approval is a journaled, re-enterable position.
func (x *JournaledExecutor) SuspendForApproval(ctx context.Context, env *SessionEnvelope, in OpIntent) (JournalEntry, error) {
	if env == nil {
		return JournalEntry{}, fmt.Errorf("turn: nil envelope")
	}
	in.Kind = OpApprovalWait
	key := DeriveIdempotencyKey(env.SessionID, in)
	if prior, ok := env.Journal.Lookup(key); ok {
		return prior, nil
	}
	e, _, _ := env.Journal.RecordIntent(key, in, x.now())
	if err := x.persist(ctx, *env); err != nil {
		return JournalEntry{}, err
	}
	return e, nil
}

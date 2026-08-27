package turn

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"
)

type EffectResult struct {
	ExternalRef    string
	Output         string
	AlreadyExisted bool
}

type EffectSink interface {
	Perform(context.Context, OpIntent) (EffectResult, error)
}

type Reconciler interface {
	Reconcile(context.Context, OpIntent) (ref string, found bool, err error)
}

type Persister interface {
	Persist(context.Context, SessionEnvelope) error
}

var ErrAmbiguous = errors.New("turn: effect outcome is ambiguous and no reconciler is available")

// JournaledExecutor owns the intent-persist / effect / settle-persist protocol.
type JournaledExecutor struct {
	Sink       EffectSink
	Reconciler Reconciler
	Persister  Persister
	Now        func() time.Time
}

func (x *JournaledExecutor) now() time.Time {
	if x.Now != nil {
		return x.Now().UTC()
	}
	return time.Now().UTC()
}

func (x *JournaledExecutor) Do(ctx context.Context, env *SessionEnvelope, op PlannedOperation) (EffectResult, error) {
	if env == nil {
		return EffectResult{}, fmt.Errorf("turn: nil envelope")
	}
	if op.IdempotencyKey == "" {
		return EffectResult{}, fmt.Errorf("turn: operation has no idempotency key")
	}
	if prior, ok := env.Journal.Lookup(op.IdempotencyKey); ok && prior.Done() {
		return EffectResult{ExternalRef: prior.ExternalRef, AlreadyExisted: true}, nil
	}
	if prior, ok := env.Journal.Lookup(op.IdempotencyKey); ok && prior.Ambiguous() {
		if x.Reconciler == nil {
			return EffectResult{}, fmt.Errorf("%w: %s", ErrAmbiguous, op.IdempotencyKey)
		}
		ref, found, err := x.Reconciler.Reconcile(ctx, op.Intent)
		if err != nil {
			return EffectResult{}, fmt.Errorf("turn: reconcile %s: %w", op.IdempotencyKey, err)
		}
		if found {
			env.Journal.settle(op.IdempotencyKey, OpSucceeded, ref, "", x.now())
			if err := x.persist(ctx, *env); err != nil {
				return EffectResult{}, err
			}
			return EffectResult{ExternalRef: ref, AlreadyExisted: true}, nil
		}
	}
	if x.Sink == nil {
		return EffectResult{}, fmt.Errorf("turn: no effect sink configured")
	}
	if x.Persister == nil {
		return EffectResult{}, fmt.Errorf("turn: no persister configured")
	}
	env.Journal.recordIntent(op.IdempotencyKey, op.Intent, x.now())
	if err := x.persist(ctx, *env); err != nil {
		return EffectResult{}, err
	}
	result, err := x.Sink.Perform(ctx, op.Intent)
	if err != nil {
		env.Journal.settle(op.IdempotencyKey, OpFailed, "", err.Error(), x.now())
		if persistErr := x.persist(ctx, *env); persistErr != nil {
			return EffectResult{}, errors.Join(err, persistErr)
		}
		return EffectResult{}, err
	}
	env.Journal.settle(op.IdempotencyKey, OpSucceeded, result.ExternalRef, "", x.now())
	if err := x.persist(ctx, *env); err != nil {
		return EffectResult{}, err
	}
	return result, nil
}

func (x *JournaledExecutor) persist(ctx context.Context, env SessionEnvelope) error {
	if x.Persister == nil {
		return fmt.Errorf("turn: no persister configured")
	}
	env.UpdatedAt = x.now()
	return x.Persister.Persist(ctx, env)
}

// Runner executes one contribute-shaped turn as a re-entrant function. The
// first call binds input into the envelope. Later calls need only the envelope;
// supplying a different plan is rejected instead of silently changing work.
type Runner struct {
	Executor *JournaledExecutor
	Now      func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) Step(ctx context.Context, env SessionEnvelope, input *TurnPlan) (SessionEnvelope, TurnOutput, error) {
	outEnv := env.Clone()
	if err := outEnv.Validate(); err != nil {
		return outEnv, TurnOutput{}, err
	}
	if r.Executor == nil {
		return outEnv, TurnOutput{}, fmt.Errorf("turn: no journaled executor configured")
	}
	if outEnv.CreatedAt.IsZero() {
		outEnv.CreatedAt = r.now()
	}
	if outEnv.Plan == nil {
		if input == nil {
			return outEnv, TurnOutput{}, fmt.Errorf("turn: initial plan is required")
		}
		outEnv.Plan = bindPlan(outEnv.SessionID, input)
	} else if input != nil && !reflect.DeepEqual(outEnv.Plan, bindPlan(outEnv.SessionID, input)) {
		return outEnv, TurnOutput{}, fmt.Errorf("turn: input plan differs from persisted plan")
	}
	outEnv.Status = StatusActive
	for _, operation := range outEnv.Plan.Operations {
		if _, err := r.Executor.Do(ctx, &outEnv, operation); err != nil {
			return outEnv, outputFor(outEnv), err
		}
	}
	outEnv.Status = StatusCompleted
	outEnv.UpdatedAt = r.now()
	if err := r.Executor.persist(ctx, outEnv); err != nil {
		return outEnv, outputFor(outEnv), err
	}
	return outEnv, outputFor(outEnv), nil
}

func bindPlan(sessionID string, input *TurnPlan) *TurnPlan {
	bound := &TurnPlan{Verdict: input.Verdict, Rationale: input.Rationale}
	bound.Operations = make([]PlannedOperation, len(input.Operations))
	for i, operation := range input.Operations {
		bound.Operations[i] = operation
		// The caller does not choose operation identity. Binding always derives
		// it from the session and semantic intent so a stale or model-supplied
		// key cannot suppress unrelated work.
		bound.Operations[i].IdempotencyKey = DeriveIdempotencyKey(sessionID, operation.Intent)
	}
	return bound
}

func outputFor(env SessionEnvelope) TurnOutput {
	out := TurnOutput{Status: env.Status}
	if env.Plan != nil {
		out.Verdict = env.Plan.Verdict
		out.Rationale = env.Plan.Rationale
	}
	for _, entry := range env.Journal.Entries {
		if entry.Done() {
			out.Effects = append(out.Effects, entry)
		}
	}
	out.Done = env.Status == StatusCompleted
	return out
}

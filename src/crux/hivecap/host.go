package hivecap

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/effects"
)

// This file carries the two crux.Host setters that are calls into hive
// rather than values (hivecommons/hive#14138 stage 4): SetInferenceCallbacks
// becomes InferenceRoutes, a desired-state pusher over
// callback.InferenceRouter, and SetSandboxMutationBoundary becomes
// MutationBoundary, an effects.Boundary over callback.EffectBoundary.

// DefaultRouteResync is how often InferenceRoutes re-pushes every route it
// holds, so a hive that restarted (and so lost its proxy's route table)
// learns them again within it.
const DefaultRouteResync = time.Minute

// routeRetry is the backoff after a failed push; the next change or resync
// tick retries sooner.
const routeRetry = 2 * time.Second

// InferenceRoutes is the sidecar's SetInferenceCallbacks: Set and Clear are
// the two callbacks the manager installs, and they return at once. The
// manager calls them on its launch path, sometimes under its own lock, so
// they must not wait on hive; instead they record the desired route and a
// worker (Run) pushes it, retrying until hive has it. A route hive never
// receives is fail-closed by construction: the proxy has no upstream for
// the agent and refuses its inference traffic. A hive without the
// capability (callback.ErrNotConfigured) is treated as the nil callback:
// the routes are kept but never pushed again until the next resync.
type InferenceRoutes struct {
	caps    callback.InferenceRouter
	timeout time.Duration

	mu      sync.Mutex
	desired map[string]*callback.InferenceRoute // nil value = cleared
	dirty   map[string]bool
	wake    chan struct{}
	pushes  int
	lastErr error
}

// NewInferenceRoutes builds the pusher over caps; nil caps (or one that is
// not an InferenceRouter) records routes and pushes nothing. Non-positive
// timeout selects callback.DefaultTimeout.
func NewInferenceRoutes(caps callback.Capabilities, timeout time.Duration) *InferenceRoutes {
	if timeout <= 0 {
		timeout = callback.DefaultTimeout
	}
	r := &InferenceRoutes{timeout: timeout, desired: map[string]*callback.InferenceRoute{}, dirty: map[string]bool{}, wake: make(chan struct{}, 1)}
	if caps != nil {
		r.caps, _ = caps.(callback.InferenceRouter)
	}
	return r
}

// Set is the setRoute callback: agent's inference traffic goes to backend
// and model.
func (r *InferenceRoutes) Set(agent, backend, model string) {
	r.mu.Lock()
	r.desired[agent] = &callback.InferenceRoute{Backend: backend, Model: model}
	r.dirty[agent] = true
	r.mu.Unlock()
	r.signal()
}

// Clear is the clearRoute callback: agent has no inference route.
func (r *InferenceRoutes) Clear(agent string) {
	r.mu.Lock()
	r.desired[agent] = nil
	r.dirty[agent] = true
	r.mu.Unlock()
	r.signal()
}

func (r *InferenceRoutes) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Pending reports how many routes hive does not have yet, and the last push
// error (nil when the last push succeeded).
func (r *InferenceRoutes) Pending() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dirty), r.lastErr
}

// Run pushes dirty routes until ctx ends: at once on every Set/Clear, after
// routeRetry while a push fails, and every resync (non-positive selects
// DefaultRouteResync) for every route held, dirty or not.
func (r *InferenceRoutes) Run(ctx context.Context, resync time.Duration) {
	if r == nil || r.caps == nil {
		return
	}
	if resync <= 0 {
		resync = DefaultRouteResync
	}
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	var retry <-chan time.Time
	for {
		failed := r.flush(ctx)
		retry = nil
		if failed {
			retry = time.After(routeRetry)
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-retry:
		case <-ticker.C:
			r.mu.Lock()
			for agent := range r.desired {
				r.dirty[agent] = true
			}
			r.mu.Unlock()
		}
	}
}

// flush pushes every dirty route once; it reports whether any push failed
// (and so is still dirty).
func (r *InferenceRoutes) flush(ctx context.Context) bool {
	r.mu.Lock()
	batch := make(map[string]*callback.InferenceRoute, len(r.dirty))
	for agent := range r.dirty {
		batch[agent] = r.desired[agent]
	}
	r.mu.Unlock()
	failed := false
	for agent, route := range batch {
		err := r.push(ctx, agent, route)
		r.mu.Lock()
		r.pushes++
		r.lastErr = err
		if err == nil && batchStill(r.desired[agent], route) {
			delete(r.dirty, agent)
			// A cleared route hive has acknowledged needs no further entry.
			if route == nil {
				delete(r.desired, agent)
			}
		}
		r.mu.Unlock()
		if err != nil && !errors.Is(err, callback.ErrNotConfigured) {
			failed = true
		}
	}
	return failed
}

// batchStill reports whether the desired route is still the one just
// pushed (a Set during the push leaves the entry dirty).
func batchStill(current, pushed *callback.InferenceRoute) bool {
	if current == nil || pushed == nil {
		return current == pushed
	}
	return *current == *pushed
}

func (r *InferenceRoutes) push(ctx context.Context, agent string, route *callback.InferenceRoute) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if route == nil {
		return r.caps.ClearInferenceRoute(ctx, agent)
	}
	return r.caps.SetInferenceRoute(ctx, agent, *route)
}

// MutationBoundary implements effects.Boundary over the callback: Execute
// begins the claim on hive, runs the effect here, and ends the claim with
// the outcome, so hive's journal and fence see a sidecar's sandbox pushes
// exactly as an in-process runtime's. It fails closed: an unreachable hive
// refuses the effect (the push does not happen); a hive without the
// capability runs the effect unjournaled, which is the nil boundary the
// in-process wiring also tolerates.
type MutationBoundary struct {
	caps callback.EffectBoundary
	// EndTimeout bounds the end call, which must get through even when the
	// caller's context has already been cancelled by the effect's failure;
	// zero means callback.DefaultTimeout.
	EndTimeout time.Duration
}

// NewMutationBoundary builds the boundary over caps; a caps that is not an
// EffectBoundary runs effects unjournaled.
func NewMutationBoundary(caps callback.Capabilities) MutationBoundary {
	var b MutationBoundary
	if caps != nil {
		b.caps, _ = caps.(callback.EffectBoundary)
	}
	return b
}

var _ effects.Boundary = MutationBoundary{}

// Execute is effects.Boundary.Execute.
func (b MutationBoundary) Execute(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	if b.caps == nil {
		return effect(ctx)
	}
	begun, err := b.caps.BeginEffect(ctx, callback.EffectClaim{Repo: claim.Repo, Kind: claim.Kind, Target: claim.Target, Actor: claim.Actor, Inputs: claim.Inputs})
	if err != nil {
		if errors.Is(err, callback.ErrNotConfigured) {
			return effect(ctx)
		}
		if errors.Is(err, callback.ErrEffectDenied) {
			return effects.Result{}, fmt.Errorf("%w: %v", effects.ErrDenied, err)
		}
		return effects.Result{}, fmt.Errorf("hivecap: mutation boundary unavailable, effect refused: %w", err)
	}
	res, effErr := effect(ctx)
	end := callback.EndEffectRequest{Provenance: res.Provenance}
	if effErr != nil {
		end.Error = effErr.Error()
	}
	timeout := b.EndTimeout
	if timeout <= 0 {
		timeout = callback.DefaultTimeout
	}
	// The end call carries the outcome hive must journal even when the
	// caller's ctx is already done, so it gets its own deadline.
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	ended, endErr := b.caps.EndEffect(endCtx, begun.ID, end)
	switch {
	case effErr != nil:
		return res, effErr
	case endErr != nil:
		return res, fmt.Errorf("hivecap: effect ran but hive could not journal it: %w", endErr)
	case ended.Error != "":
		return res, errors.New(ended.Error)
	}
	if ended.Provenance != "" {
		res.Provenance = ended.Provenance
	}
	return res, nil
}

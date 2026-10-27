package cruxhost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/effects"
)

// This file is hive's end of the two stage-4 call-shaped Host setters
// (hivecommons/hive#14138): the sidecar's SetInferenceCallbacks arrive as
// callback.InferenceRouter calls and its SetSandboxMutationBoundary as a
// two-phase callback.EffectBoundary that the broker below turns back into
// one effects.Boundary.Execute on hive's journaling boundary.

// InferenceRouter is the pair of closures cmd/hive hands an in-process
// manager through SetInferenceCallbacks, restated as an interface so the
// callback can carry them.
type InferenceRouter interface {
	SetInferenceRoute(agent, backend, model string)
	ClearInferenceRoute(agent string)
}

// InferenceRouterFuncs adapts the two closures to InferenceRouter.
type InferenceRouterFuncs struct {
	Set   func(agent, backend, model string)
	Clear func(agent string)
}

func (f InferenceRouterFuncs) SetInferenceRoute(agent, backend, model string) {
	if f.Set != nil {
		f.Set(agent, backend, model)
	}
}

func (f InferenceRouterFuncs) ClearInferenceRoute(agent string) {
	if f.Clear != nil {
		f.Clear(agent)
	}
}

// DefaultEffectTTL bounds how long a begun effect may stay open without its
// end call: a sidecar that died mid-push leaves hive's boundary waiting,
// and after the TTL the broker ends it as failed-unknown so the journal
// records the attempt and the fence is released.
const DefaultEffectTTL = 15 * time.Minute

// EffectBroker runs sidecar mutations through hive's effects.Boundary.
// BeginEffect starts boundary.Execute in a goroutine whose effect closure
// blocks until EndEffect delivers the outcome (or the TTL expires), so the
// boundary sees exactly the Execute it would see in-process: journal
// entry, fence, idempotency and reconciliation all apply.
type EffectBroker struct {
	boundary effects.Boundary
	ttl      time.Duration

	mu      sync.Mutex
	pending map[string]*pendingEffect
}

type pendingEffect struct {
	outcome chan callback.EndEffectRequest
	done    chan struct{}
	result  effects.Result
	err     error
}

// NewEffectBroker builds the broker over boundary; a nil boundary makes
// Configured report the capability absent. Non-positive ttl selects
// DefaultEffectTTL.
func NewEffectBroker(boundary effects.Boundary, ttl time.Duration) *EffectBroker {
	if ttl <= 0 {
		ttl = DefaultEffectTTL
	}
	return &EffectBroker{boundary: boundary, ttl: ttl, pending: map[string]*pendingEffect{}}
}

// ErrEffectAbandoned is the error the boundary journals when a begun
// effect's end never arrived within the TTL.
var ErrEffectAbandoned = errors.New("cruxhost: sidecar effect abandoned, outcome unknown")

// Pending reports how many begun effects await their end call.
func (b *EffectBroker) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.pending)
}

// BeginEffect is callback.EffectBoundary.BeginEffect.
func (b *EffectBroker) BeginEffect(ctx context.Context, claim callback.EffectClaim) (callback.BeginEffectResponse, error) {
	if b == nil || b.boundary == nil {
		return callback.BeginEffectResponse{}, callback.ErrNotConfigured
	}
	id, err := newEffectID()
	if err != nil {
		return callback.BeginEffectResponse{}, err
	}
	p := &pendingEffect{outcome: make(chan callback.EndEffectRequest, 1), done: make(chan struct{})}
	started := make(chan struct{})
	b.mu.Lock()
	b.pending[id] = p
	b.mu.Unlock()
	go b.run(id, p, claim, started)
	select {
	case <-started:
		return callback.BeginEffectResponse{ID: id}, nil
	case <-p.done:
		// Execute returned without running the effect: denied, needs
		// reconciliation, or another boundary refusal.
		b.forget(id)
		if errors.Is(p.err, effects.ErrDenied) {
			return callback.BeginEffectResponse{}, fmt.Errorf("%w: %v", callback.ErrEffectDenied, p.err)
		}
		if p.err == nil {
			p.err = errors.New("cruxhost: boundary completed without running the effect")
		}
		return callback.BeginEffectResponse{}, p.err
	case <-ctx.Done():
		// The runtime gave up waiting; the boundary goroutine still owns
		// the claim and will be ended by the TTL.
		return callback.BeginEffectResponse{}, ctx.Err()
	}
}

// run is the boundary goroutine: the effect closure signals started, then
// waits for the end call or the TTL.
func (b *EffectBroker) run(id string, p *pendingEffect, claim callback.EffectClaim, started chan<- struct{}) {
	defer close(p.done)
	// The boundary's own context: detached from the begin request, which
	// returns long before the effect ends.
	ctx, cancel := context.WithTimeout(context.Background(), b.ttl+time.Minute)
	defer cancel()
	p.result, p.err = b.boundary.Execute(ctx, effects.Claim{Repo: claim.Repo, Kind: claim.Kind, Target: claim.Target, Actor: claim.Actor, Inputs: claim.Inputs}, func(ctx context.Context) (effects.Result, error) {
		close(started)
		timer := time.NewTimer(b.ttl)
		defer timer.Stop()
		select {
		case end := <-p.outcome:
			res := effects.Result{Provenance: end.Provenance}
			if end.Error != "" {
				return res, errors.New(end.Error)
			}
			return res, nil
		case <-timer.C:
			b.forget(id)
			return effects.Result{}, ErrEffectAbandoned
		case <-ctx.Done():
			b.forget(id)
			return effects.Result{}, ctx.Err()
		}
	})
}

// EndEffect is callback.EffectBoundary.EndEffect: it delivers the outcome
// to the waiting boundary and returns what the boundary made of it.
func (b *EffectBroker) EndEffect(ctx context.Context, id string, req callback.EndEffectRequest) (callback.EndEffectResponse, error) {
	if b == nil || b.boundary == nil {
		return callback.EndEffectResponse{}, callback.ErrNotConfigured
	}
	b.mu.Lock()
	p, ok := b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return callback.EndEffectResponse{}, callback.ErrEffectUnknown
	}
	p.outcome <- req
	select {
	case <-p.done:
	case <-ctx.Done():
		return callback.EndEffectResponse{}, ctx.Err()
	}
	out := callback.EndEffectResponse{Provenance: p.result.Provenance}
	if p.err != nil && p.err.Error() != req.Error {
		// The boundary added something (fence lost, journal failure): the
		// runtime learns it; the effect's own error it already has.
		out.Error = p.err.Error()
	}
	return out, nil
}

func (b *EffectBroker) forget(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

func newEffectID() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("cruxhost: effect id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

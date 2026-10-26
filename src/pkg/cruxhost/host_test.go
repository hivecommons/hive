package cruxhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/hivecap"
	"github.com/hivecommons/hive/internal/testutil"
)

// journal is a stand-in for hive's mutation boundary: it records every
// Execute, denies when told, and stamps provenance like the real journal.
type journal struct {
	mu      sync.Mutex
	claims  []effects.Claim
	results []effects.Result
	errs    []error
	deny    bool
	fail    error
}

func (j *journal) Execute(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	j.mu.Lock()
	j.claims = append(j.claims, claim)
	deny, fail := j.deny, j.fail
	j.mu.Unlock()
	if fail != nil {
		return effects.Result{}, fail
	}
	if deny {
		return effects.Result{}, effects.ErrDenied
	}
	res, err := effect(ctx)
	j.mu.Lock()
	j.results = append(j.results, res)
	j.errs = append(j.errs, err)
	j.mu.Unlock()
	if err != nil {
		return res, err
	}
	res.Provenance = "journal:" + res.Provenance
	return res, nil
}

func (j *journal) last() (effects.Result, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.results) == 0 {
		return effects.Result{}, nil
	}
	return j.results[len(j.results)-1], j.errs[len(j.errs)-1]
}

type routeTable struct {
	mu     sync.Mutex
	routes map[string][2]string
}

func (r *routeTable) SetInferenceRoute(agent, backend, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.routes == nil {
		r.routes = map[string][2]string{}
	}
	r.routes[agent] = [2]string{backend, model}
}

func (r *routeTable) ClearInferenceRoute(agent string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.routes, agent)
}

func (r *routeTable) get(agent string) ([2]string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.routes[agent]
	return v, ok
}

// A sidecar manager's SetInferenceCallbacks reach hive's proxy route table
// over the socket; the closure adapter covers cmd/hive's two funcs.
func TestInferenceRoutesOverSocket(t *testing.T) {
	table := &routeTable{}
	c, _ := serve(t, Deps{Inference: InferenceRouterFuncs{Set: table.SetInferenceRoute, Clear: table.ClearInferenceRoute}})
	ctx := context.Background()
	if err := c.SetInferenceRoute(ctx, "a", callback.InferenceRoute{Backend: "litellm", Model: "gpt"}); err != nil {
		t.Fatal(err)
	}
	if got, ok := table.get("a"); !ok || got != [2]string{"litellm", "gpt"} {
		t.Fatalf("route = %v %v", got, ok)
	}
	if err := c.ClearInferenceRoute(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := table.get("a"); ok {
		t.Fatal("route not cleared")
	}
	InferenceRouterFuncs{}.SetInferenceRoute("x", "y", "z") // nil funcs are tolerated
	InferenceRouterFuncs{}.ClearInferenceRoute("x")

	bare := New(Deps{})
	if err := bare.SetInferenceRoute(ctx, "a", callback.InferenceRoute{Backend: "b"}); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("no table: %v", err)
	}
	if err := bare.ClearInferenceRoute(ctx, "a"); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("no table: %v", err)
	}
	if bare.Configured(callback.CapabilityInferenceRoutes) || bare.Configured(callback.CapabilityEffects) {
		t.Fatal("configured without deps")
	}
}

// The full sidecar path: hivecap.MutationBoundary on the runtime side,
// the socket, the broker, hive's journaling boundary. The journal sees one
// Execute with the sidecar's claim and the effect's result exactly as it
// would in-process, and the sidecar gets the journal's provenance back.
func TestMutationBoundaryEndToEnd(t *testing.T) {
	j := &journal{}
	c, _ := serve(t, Deps{Mutation: j})
	b := hivecap.NewMutationBoundary(c)
	claim := effects.Claim{Repo: "o/r", Kind: effects.KindBranchPush, Target: "feat", Actor: "agent", Inputs: map[string]string{"commit": "abc", "remote": "origin"}}
	res, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		return effects.Result{Provenance: "abc"}, nil
	})
	if err != nil || res.Provenance != "journal:abc" {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	j.mu.Lock()
	got := j.claims[0]
	j.mu.Unlock()
	if got.Repo != "o/r" || got.Kind != effects.KindBranchPush || got.Target != "feat" || got.Actor != "agent" || got.Inputs["commit"] != "abc" || got.Inputs["remote"] != "origin" {
		t.Fatalf("journal saw %+v", got)
	}
	if last, lerr := j.last(); last.Provenance != "abc" || lerr != nil {
		t.Fatalf("journal result = %+v, %v", last, lerr)
	}

	// A failed push: the journal records the failure, the sidecar gets its
	// own error back and nothing extra.
	_, err = b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
		return effects.Result{}, errors.New("rejected by remote")
	})
	if err == nil || err.Error() != "rejected by remote" {
		t.Fatalf("failed push = %v", err)
	}
	if _, lerr := j.last(); lerr == nil || lerr.Error() != "rejected by remote" {
		t.Fatalf("journal error = %v", lerr)
	}

	// Denied by the journal: the sidecar never runs the effect.
	j.mu.Lock()
	j.deny = true
	j.mu.Unlock()
	ran := false
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { ran = true; return effects.Result{}, nil }); !errors.Is(err, effects.ErrDenied) || ran {
		t.Fatalf("denied = %v ran=%v", err, ran)
	}
	// Any other refusal is passed through as an unavailable boundary.
	j.mu.Lock()
	j.deny, j.fail = false, effects.ErrNeedsReconciliation
	j.mu.Unlock()
	if _, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) { ran = true; return effects.Result{}, nil }); err == nil || errors.Is(err, effects.ErrDenied) || ran {
		t.Fatalf("refused = %v ran=%v", err, ran)
	}
	if New(Deps{Mutation: j}).Effects().Pending() != 0 {
		t.Fatal("pending leak")
	}
}

// The broker's own edges: unknown id, abandoned effect after the TTL, a
// boundary that completes without running the effect, and a begin whose
// caller gives up.
func TestEffectBrokerEdges(t *testing.T) {
	ctx := context.Background()
	j := &journal{}
	br := NewEffectBroker(j, 30*time.Millisecond)
	if _, err := br.EndEffect(ctx, "nope", callback.EndEffectRequest{}); !errors.Is(err, callback.ErrEffectUnknown) {
		t.Fatalf("unknown = %v", err)
	}
	begun, err := br.BeginEffect(ctx, callback.EffectClaim{Repo: "o/r", Kind: "k", Target: "t"})
	if err != nil || begun.ID == "" || br.Pending() != 1 {
		t.Fatalf("begin = %+v, %v pending=%d", begun, err, br.Pending())
	}
	// No end arrives: the TTL ends it as abandoned and the journal records
	// the unknown outcome.
	testutil.Eventually(t, 5*time.Second, func() bool {
		_, lerr := j.last()
		return br.Pending() == 0 && lerr != nil
	}, "abandoned effect journaled")
	if _, lerr := j.last(); !errors.Is(lerr, ErrEffectAbandoned) {
		t.Fatalf("abandoned journal error = %v", lerr)
	}
	if _, err := br.EndEffect(ctx, begun.ID, callback.EndEffectRequest{}); !errors.Is(err, callback.ErrEffectUnknown) {
		t.Fatalf("late end = %v", err)
	}

	// Hive's boundary adds an error of its own after the effect: the
	// sidecar learns it on End.
	br = NewEffectBroker(fenceLost{j}, time.Second)
	begun, err = br.BeginEffect(ctx, callback.EffectClaim{Repo: "o/r", Kind: "k", Target: "t"})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := br.EndEffect(ctx, begun.ID, callback.EndEffectRequest{Provenance: "p"})
	if err != nil || ended.Error != "fence lost" {
		t.Fatalf("ended = %+v, %v", ended, err)
	}

	// A boundary that completes without ever running the effect.
	br = NewEffectBroker(skipBoundary{}, time.Second)
	if _, err := br.BeginEffect(ctx, callback.EffectClaim{Repo: "o/r", Kind: "k", Target: "t"}); err == nil || errors.Is(err, callback.ErrEffectDenied) {
		t.Fatalf("skipped = %v", err)
	}

	// The begin caller gives up before the effect starts.
	br = NewEffectBroker(slowBoundary{}, time.Second)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, err := br.BeginEffect(cctx, callback.EffectClaim{Repo: "o/r", Kind: "k", Target: "t"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow begin = %v", err)
	}

	// The end caller gives up before the boundary finishes.
	br = NewEffectBroker(slowAfter{}, time.Second)
	begun, err = br.BeginEffect(ctx, callback.EffectClaim{Repo: "o/r", Kind: "k", Target: "t"})
	if err != nil {
		t.Fatal(err)
	}
	cctx2, cancel2 := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel2()
	if _, err := br.EndEffect(cctx2, begun.ID, callback.EndEffectRequest{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow end = %v", err)
	}

	var nilBroker *EffectBroker
	if _, err := nilBroker.BeginEffect(ctx, callback.EffectClaim{}); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("nil broker = %v", err)
	}
	if _, err := NewEffectBroker(nil, 0).EndEffect(ctx, "x", callback.EndEffectRequest{}); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("no boundary = %v", err)
	}
	if NewEffectBroker(nil, 0).ttl != DefaultEffectTTL {
		t.Fatal("default ttl")
	}
}

type fenceLost struct{ *journal }

func (f fenceLost) Execute(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	res, err := f.journal.Execute(ctx, claim, effect)
	if err != nil {
		return res, err
	}
	return res, errors.New("fence lost")
}

type skipBoundary struct{}

func (skipBoundary) Execute(context.Context, effects.Claim, func(context.Context) (effects.Result, error)) (effects.Result, error) {
	return effects.Result{}, nil
}

type slowBoundary struct{}

func (slowBoundary) Execute(ctx context.Context, _ effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	<-time.After(50 * time.Millisecond)
	return effect(ctx)
}

type slowAfter struct{}

func (slowAfter) Execute(ctx context.Context, _ effects.Claim, effect func(context.Context) (effects.Result, error)) (effects.Result, error) {
	res, err := effect(ctx)
	<-time.After(50 * time.Millisecond)
	return res, err
}

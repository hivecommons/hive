package hivecap

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/effects"
)

// hostDirect is a direct that also owns the proxy route table and the
// mutation boundary, hive's end of the stage-4 families.
type hostDirect struct {
	*direct
	hmu     sync.Mutex
	routes  map[string]callback.InferenceRoute
	rfail   error
	begun   []callback.EffectClaim
	ended   map[string]callback.EndEffectRequest
	deny    bool
	efail   error
	endFail error
}

func newHostDirect() *hostDirect {
	return &hostDirect{direct: &direct{}, routes: map[string]callback.InferenceRoute{}, ended: map[string]callback.EndEffectRequest{}}
}

func (h *hostDirect) SetInferenceRoute(_ context.Context, agent string, route callback.InferenceRoute) error {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.rfail != nil {
		return h.rfail
	}
	h.routes[agent] = route
	return nil
}

func (h *hostDirect) ClearInferenceRoute(_ context.Context, agent string) error {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.rfail != nil {
		return h.rfail
	}
	delete(h.routes, agent)
	return nil
}

func (h *hostDirect) BeginEffect(_ context.Context, claim callback.EffectClaim) (callback.BeginEffectResponse, error) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.efail != nil {
		return callback.BeginEffectResponse{}, h.efail
	}
	if h.deny {
		return callback.BeginEffectResponse{}, callback.ErrEffectDenied
	}
	h.begun = append(h.begun, claim)
	return callback.BeginEffectResponse{ID: "e1"}, nil
}

func (h *hostDirect) EndEffect(_ context.Context, id string, req callback.EndEffectRequest) (callback.EndEffectResponse, error) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	if h.endFail != nil {
		return callback.EndEffectResponse{}, h.endFail
	}
	h.ended[id] = req
	return callback.EndEffectResponse{Provenance: "journal:" + req.Provenance, Error: req.Error}, nil
}

func (h *hostDirect) route(agent string) (callback.InferenceRoute, bool) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	r, ok := h.routes[agent]
	return r, ok
}

func (h *hostDirect) setRouteFail(err error) {
	h.hmu.Lock()
	defer h.hmu.Unlock()
	h.rfail = err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// Set/Clear return at once and the worker delivers the route to hive in
// both modes; a cleared route disappears from hive.
func TestInferenceRoutesPushAndClear(t *testing.T) {
	for _, mode := range []string{"direct", "socket"} {
		t.Run(mode, func(t *testing.T) {
			h := newHostDirect()
			var caps callback.Capabilities = h
			if mode == "socket" {
				caps = overSocket(t, h)
			}
			r := NewInferenceRoutes(caps, time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); r.Run(ctx, time.Hour) }()
			r.Set("a", "litellm", "gpt")
			waitFor(t, "route pushed", func() bool {
				got, ok := h.route("a")
				return ok && got == callback.InferenceRoute{Backend: "litellm", Model: "gpt"}
			})
			if n, err := r.Pending(); n != 0 || err != nil {
				t.Fatalf("Pending = %d, %v", n, err)
			}
			r.Clear("a")
			waitFor(t, "route cleared", func() bool { _, ok := h.route("a"); return !ok })
			r.mu.Lock()
			_, kept := r.desired["a"]
			r.mu.Unlock()
			if kept {
				t.Fatal("acknowledged clear should drop the desired entry")
			}
			cancel()
			<-done
		})
	}
}

// A push that fails stays pending and is retried until hive takes it; a
// Set during the outage supersedes the earlier route.
func TestInferenceRoutesRetryUntilDelivered(t *testing.T) {
	h := newHostDirect()
	h.setRouteFail(errors.New("proxy down"))
	r := NewInferenceRoutes(h, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, time.Hour)
	r.Set("a", "old", "")
	waitFor(t, "failed push recorded", func() bool { n, err := r.Pending(); return n == 1 && err != nil })
	r.Set("a", "new", "m")
	h.setRouteFail(nil)
	waitFor(t, "route delivered after recovery", func() bool {
		got, ok := h.route("a")
		return ok && got.Backend == "new"
	})
	// The host records the route before the worker's bookkeeping runs, so
	// Pending must be awaited, not sampled.
	waitFor(t, "pending drained after recovery", func() bool { n, err := r.Pending(); return n == 0 && err == nil })
}

// The periodic resync re-pushes every held route so a hive that lost its
// table gets it back without a manager change.
func TestInferenceRoutesResync(t *testing.T) {
	h := newHostDirect()
	r := NewInferenceRoutes(h, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, 20*time.Millisecond)
	r.Set("a", "litellm", "")
	waitFor(t, "initial push", func() bool { _, ok := h.route("a"); return ok })
	h.hmu.Lock()
	delete(h.routes, "a") // hive restarted and forgot
	h.hmu.Unlock()
	waitFor(t, "resync re-push", func() bool { _, ok := h.route("a"); return ok })
}

// Without the capability the pusher holds the routes and never pushes:
// nil caps, a Capabilities that is not an InferenceRouter, and a hive that
// answers not_configured all behave the same and none blocks.
func TestInferenceRoutesWithoutCapability(t *testing.T) {
	var nilRoutes *InferenceRoutes
	nilRoutes.Run(context.Background(), 0) // returns at once
	for _, caps := range []callback.Capabilities{nil, &direct{}} {
		r := NewInferenceRoutes(caps, 0)
		r.Set("a", "b", "")
		r.Run(context.Background(), 0) // no router: returns at once
		if n, _ := r.Pending(); n != 1 {
			t.Fatalf("Pending without router = %d, want held", n)
		}
	}
	h := newHostDirect()
	h.absent = map[string]bool{callback.CapabilityInferenceRoutes: true}
	r := NewInferenceRoutes(overSocket(t, h), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx, time.Hour)
	r.Set("a", "b", "")
	waitFor(t, "not_configured push", func() bool { n, err := r.Pending(); return n == 1 && errors.Is(err, callback.ErrNotConfigured) })
	if _, ok := h.route("a"); ok {
		t.Fatal("not_configured must not deliver")
	}
}

func TestBatchStill(t *testing.T) {
	a, b := &callback.InferenceRoute{Backend: "a"}, &callback.InferenceRoute{Backend: "b"}
	if !batchStill(nil, nil) || batchStill(a, nil) || batchStill(nil, a) || batchStill(a, b) || !batchStill(a, &callback.InferenceRoute{Backend: "a"}) {
		t.Fatal("batchStill misjudged")
	}
}

// The mutation boundary journals a sandbox push through hive in both modes:
// begin before, end after with provenance or the effect's error.
func TestMutationBoundaryJournals(t *testing.T) {
	for _, mode := range []string{"direct", "socket"} {
		t.Run(mode, func(t *testing.T) {
			h := newHostDirect()
			var caps callback.Capabilities = h
			if mode == "socket" {
				caps = overSocket(t, h)
			}
			b := NewMutationBoundary(caps)
			claim := effects.Claim{Repo: "o/r", Kind: effects.KindBranchPush, Target: "feat", Actor: "agent", Inputs: map[string]string{"commit": "abc"}}
			res, err := b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				return effects.Result{Provenance: "abc"}, nil
			})
			if err != nil || res.Provenance != "journal:abc" {
				t.Fatalf("Execute = %+v, %v", res, err)
			}
			h.hmu.Lock()
			begun, ended := h.begun, h.ended["e1"]
			h.hmu.Unlock()
			if len(begun) != 1 || begun[0].Repo != "o/r" || begun[0].Kind != effects.KindBranchPush || begun[0].Inputs["commit"] != "abc" {
				t.Fatalf("begun = %+v", begun)
			}
			if ended.Provenance != "abc" || ended.Error != "" {
				t.Fatalf("ended = %+v", ended)
			}
			// The effect's failure is reported to hive and returned.
			_, err = b.Execute(context.Background(), claim, func(context.Context) (effects.Result, error) {
				return effects.Result{}, errors.New("push rejected")
			})
			if err == nil || err.Error() != "push rejected" {
				t.Fatalf("failed effect = %v", err)
			}
			h.hmu.Lock()
			ended = h.ended["e1"]
			h.hmu.Unlock()
			if ended.Error != "push rejected" {
				t.Fatalf("ended after failure = %+v", ended)
			}
		})
	}
}

// Denied, unreachable, and not_configured each resolve as the in-process
// boundary would: ErrDenied, refuse, run unjournaled.
func TestMutationBoundaryFailureModes(t *testing.T) {
	ran := 0
	effect := func(context.Context) (effects.Result, error) { ran++; return effects.Result{Provenance: "p"}, nil }
	claim := effects.Claim{Repo: "o/r", Kind: effects.KindBranchPush, Target: "t"}
	ctx := context.Background()

	h := newHostDirect()
	h.deny = true
	if _, err := NewMutationBoundary(overSocket(t, h)).Execute(ctx, claim, effect); !errors.Is(err, effects.ErrDenied) || ran != 0 {
		t.Fatalf("denied = %v ran=%d", err, ran)
	}
	h.deny, h.efail = false, errors.New("journal broken")
	if _, err := NewMutationBoundary(h).Execute(ctx, claim, effect); err == nil || errors.Is(err, effects.ErrDenied) || ran != 0 {
		t.Fatalf("begin failure = %v ran=%d", err, ran)
	}
	dead, err := callback.NewClient("unix:///nonexistent/"+callback.SocketName, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMutationBoundary(dead).Execute(ctx, claim, effect); !errors.Is(err, callback.ErrUnavailable) || ran != 0 {
		t.Fatalf("unreachable = %v ran=%d, want refused", err, ran)
	}
	h.efail = nil
	h.absent = map[string]bool{callback.CapabilityEffects: true}
	res, err := NewMutationBoundary(overSocket(t, h)).Execute(ctx, claim, effect)
	if err != nil || res.Provenance != "p" || ran != 1 {
		t.Fatalf("not_configured = %+v, %v ran=%d, want unjournaled run", res, err, ran)
	}
	for _, caps := range []callback.Capabilities{nil, &direct{}} {
		if _, err := NewMutationBoundary(caps).Execute(ctx, claim, effect); err != nil {
			t.Fatalf("no boundary = %v", err)
		}
	}
	if ran != 3 {
		t.Fatalf("ran = %d", ran)
	}
	// The effect ran but the end call failed: the caller learns the journal
	// is incomplete, and the end call survives a cancelled caller context.
	h.absent = nil
	h.endFail = errors.New("journal write failed")
	cctx, cancel := context.WithCancel(ctx)
	b := NewMutationBoundary(h)
	b.EndTimeout = time.Second
	_, err = b.Execute(cctx, claim, func(context.Context) (effects.Result, error) { cancel(); return effects.Result{}, nil })
	if err == nil || !errors.Is(err, h.endFail) {
		t.Fatalf("end failure = %v", err)
	}
	// hive's end answer may carry an error of its own.
	h.endFail = nil
	hiveErr := &hostDirectEndError{hostDirect: h}
	if _, err := NewMutationBoundary(hiveErr).Execute(ctx, claim, effect); err == nil || err.Error() != "fence lost" {
		t.Fatalf("hive end error = %v", err)
	}
}

type hostDirectEndError struct{ *hostDirect }

func (hostDirectEndError) EndEffect(context.Context, string, callback.EndEffectRequest) (callback.EndEffectResponse, error) {
	return callback.EndEffectResponse{Error: "fence lost"}, nil
}

package main

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// kickDispatchRecorder captures every effect dispatchAgentKicks performs, in
// order, so a test can assert not just WHAT happened but WHEN — the ordering
// is part of the contract this extraction had to preserve.
//
// Delivery lanes run concurrently across agents, so the recorder is locked:
// start/send land from lane goroutines while the effects land from the caller.
// Per-agent order is still exact; the relative order of two agents' sends is
// not, and tests that span agents compare those as sets.
type kickDispatchRecorder struct {
	mu         sync.Mutex
	calls      []string
	sent       []string
	spansEnded []string
	spanErrs   []error
	probeStamp []time.Time
	// deliveredAt is what onDelivered received per delivered message.
	deliveredAt []time.Time
	sendErr     map[string]error
	startErr    map[string]error
	started     []string
	backoff     map[string]bool
}

func newKickDispatchRecorder() *kickDispatchRecorder {
	return &kickDispatchRecorder{sendErr: map[string]error{}, startErr: map[string]error{}, backoff: map[string]bool{}}
}

func (r *kickDispatchRecorder) deps() kickDispatchDeps {
	fixedNow := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	return kickDispatchDeps{
		backoffRemaining: func(agent string) (time.Duration, string, string, bool) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.backoff[agent] {
				r.calls = append(r.calls, "backoff-hit:"+agent)
				return 90 * time.Second, "rate_limit", "429 from provider", true
			}
			r.calls = append(r.calls, "backoff-miss:"+agent)
			return 0, "", "", false
		},
		sendKick: func(agent, message string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "send:"+agent)
			if err := r.sendErr[agent]; err != nil {
				return err
			}
			r.sent = append(r.sent, agent)
			return nil
		},
		startStoppedAgent: func(agent string) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "start:"+agent)
			if err := r.startErr[agent]; err != nil {
				return err
			}
			r.started = append(r.started, agent)
			return nil
		},
		startKickSpan: func(agent string) func(error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "span-start:"+agent)
			return func(err error) {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.calls = append(r.calls, "span-end:"+agent)
				r.spansEnded = append(r.spansEnded, agent)
				r.spanErrs = append(r.spanErrs, err)
			}
		},
		onReviewDelivered: func(msg scheduler.KickMessage) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "review:"+msg.Agent)
		},
		onDelivered: func(msg scheduler.KickMessage, deliveredAt time.Time) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "delivered:"+msg.Agent)
			r.deliveredAt = append(r.deliveredAt, deliveredAt)
		},
		markProbeReleased: func(at time.Time) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.calls = append(r.calls, "probe-stamp")
			r.probeStamp = append(r.probeStamp, at)
		},
		now: func() time.Time { return fixedNow },
	}
}

// TestDispatchAgentKicks_BackoffWithholdsWithoutAttempting pins the first skip
// rule. An agent inside a provider-error backoff must not be sent to — and
// must not even have a span opened, because a withheld kick is not an
// attempted kick and recording it as one would make traces claim the hive
// tried something it deliberately did not.
func TestDispatchAgentKicks_BackoffWithholdsWithoutAttempting(t *testing.T) {
	r := newKickDispatchRecorder()
	r.backoff["beta"] = true

	delivered := dispatchAgentKicks(kickMsgs("alpha", "beta", "gamma"), false, r.deps(), discardLogger())

	if want := []string{"alpha", "gamma"}; !equalStrings(delivered, want) {
		t.Fatalf("delivered = %v, want %v", delivered, want)
	}
	if !equalStringSets(r.sent, []string{"alpha", "gamma"}) {
		t.Fatalf("sent = %v, want alpha and gamma only", r.sent)
	}
	for _, c := range r.calls {
		if c == "span-start:beta" || c == "send:beta" {
			t.Fatalf("a backed-off agent must not be attempted, but calls were %v", r.calls)
		}
	}
}

// TestDispatchAgentKicks_FailedSendPerformsNoDeliveredEffects pins the second
// skip rule. A kick whose send fails must record nothing downstream: an audit
// entry or a lifecycle timeline row for a kick that never landed makes the
// dashboard assert something that did not happen.
//
// The span is still closed — with the error attached — because unlike a
// withheld kick, a failed one WAS attempted and is worth tracing.
func TestDispatchAgentKicks_FailedSendPerformsNoDeliveredEffects(t *testing.T) {
	r := newKickDispatchRecorder()
	boom := errors.New("tmux pane gone")
	r.sendErr["beta"] = boom

	delivered := dispatchAgentKicks(kickMsgs("alpha", "beta"), false, r.deps(), discardLogger())

	if !equalStrings(delivered, []string{"alpha"}) {
		t.Fatalf("delivered = %v, want alpha only", delivered)
	}
	for _, c := range r.calls {
		if c == "delivered:beta" || c == "review:beta" {
			t.Fatalf("a failed kick must perform no delivered-effects, calls were %v", r.calls)
		}
	}
	if !equalStrings(r.spansEnded, []string{"alpha", "beta"}) {
		t.Fatalf("both attempted kicks must close their span, got %v", r.spansEnded)
	}
	if len(r.spanErrs) != 2 || r.spanErrs[0] != nil || !errors.Is(r.spanErrs[1], boom) {
		t.Fatalf("span errors = %v, want [nil, tmux pane gone]", r.spanErrs)
	}
}

func TestDispatchAgentKicks_StartsStoppedAgentBeforeSend(t *testing.T) {
	r := newKickDispatchRecorder()

	delivered := dispatchAgentKicks(kickMsgs("alpha"), false, r.deps(), discardLogger())

	if !equalStrings(delivered, []string{"alpha"}) {
		t.Fatalf("delivered = %v, want alpha", delivered)
	}
	if !equalStrings(r.started, []string{"alpha"}) {
		t.Fatalf("started = %v, want alpha", r.started)
	}
	if want := []string{"backoff-miss:alpha", "span-start:alpha", "start:alpha", "send:alpha"}; !hasOrderedPrefix(r.calls, want) {
		t.Fatalf("calls = %v, want prefix %v", r.calls, want)
	}
}

func TestDispatchAgentKicks_StartFailurePerformsNoDeliveredEffects(t *testing.T) {
	r := newKickDispatchRecorder()
	boom := errors.New("tmux unavailable")
	r.startErr["alpha"] = boom

	delivered := dispatchAgentKicks(kickMsgs("alpha"), false, r.deps(), discardLogger())

	if len(delivered) != 0 {
		t.Fatalf("delivered = %v, want none", delivered)
	}
	for _, c := range r.calls {
		if c == "send:alpha" || c == "review:alpha" || c == "delivered:alpha" {
			t.Fatalf("a start failure must perform no kick or delivered-effects, calls were %v", r.calls)
		}
	}
	if len(r.spanErrs) != 1 || !errors.Is(r.spanErrs[0], boom) {
		t.Fatalf("span errors = %v, want start error", r.spanErrs)
	}
}

func TestStoppedAgentStartErrorIsBenign(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", want: true},
		{name: "already running", err: errors.New("agent reviewer already running"), want: true},
		{name: "launch in progress", err: errors.New("agent reviewer launch already in progress"), want: true},
		{name: "tmux failure", err: errors.New("tmux unavailable"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stoppedAgentStartErrorIsBenign(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDispatchAgentKicks_ProbeStampedExactlyOnceAfterARealSend is the rule
// with the most expensive failure mode.
//
// Releasing a probe kick means deliberately spending one run to find out
// whether the provider is serving again. The stamp re-arms suppression, so:
//
//   - stamping more than once would be harmless to the stamp but means the
//     caller released more than one probe, spending runs to learn the same
//     answer;
//   - stamping when NOTHING was actually sent is worse — suppression re-arms
//     having learned nothing at all, so the hive waits out another full probe
//     interval for no reason.
//
// Both are checked, including the case where every candidate is skipped.
func TestDispatchAgentKicks_ProbeStampedExactlyOnceAfterARealSend(t *testing.T) {
	t.Run("once, even with several kicks", func(t *testing.T) {
		r := newKickDispatchRecorder()
		dispatchAgentKicks(kickMsgs("alpha", "beta", "gamma"), true, r.deps(), discardLogger())
		if len(r.probeStamp) != 1 {
			t.Fatalf("probe stamped %d times, want exactly 1", len(r.probeStamp))
		}
		if want := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC); !r.probeStamp[0].Equal(want) {
			t.Fatalf("probe stamped at %v, want the injected clock %v", r.probeStamp[0], want)
		}
	})

	t.Run("not at all when the only kick is withheld", func(t *testing.T) {
		r := newKickDispatchRecorder()
		r.backoff["alpha"] = true
		dispatchAgentKicks(kickMsgs("alpha"), true, r.deps(), discardLogger())
		if len(r.probeStamp) != 0 {
			t.Fatal("a withheld kick must not stamp the probe: suppression would re-arm having learned nothing")
		}
	})

	t.Run("not at all when the only kick fails", func(t *testing.T) {
		r := newKickDispatchRecorder()
		r.sendErr["alpha"] = errors.New("send failed")
		dispatchAgentKicks(kickMsgs("alpha"), true, r.deps(), discardLogger())
		if len(r.probeStamp) != 0 {
			t.Fatal("a failed kick must not stamp the probe: nothing reached the provider")
		}
	})

	t.Run("stamps on the first agent that actually sends", func(t *testing.T) {
		r := newKickDispatchRecorder()
		r.backoff["alpha"] = true
		r.sendErr["beta"] = errors.New("send failed")
		dispatchAgentKicks(kickMsgs("alpha", "beta", "gamma"), true, r.deps(), discardLogger())
		if len(r.probeStamp) != 1 {
			t.Fatalf("probe stamped %d times, want 1 (on gamma)", len(r.probeStamp))
		}
		if idx := indexOf(r.calls, "probe-stamp"); idx == -1 || r.calls[idx-1] != "span-end:gamma" {
			t.Fatalf("probe must be stamped right after gamma's send, calls were %v", r.calls)
		}
	})

	t.Run("never stamps when no probe was authorised", func(t *testing.T) {
		r := newKickDispatchRecorder()
		dispatchAgentKicks(kickMsgs("alpha", "beta"), false, r.deps(), discardLogger())
		if len(r.probeStamp) != 0 {
			t.Fatalf("releaseProbe=false must never stamp, got %d stamps", len(r.probeStamp))
		}
	})
}

// TestDispatchAgentKicks_EffectOrderIsPreserved is the guard that makes this
// an extraction rather than a rewrite.
//
// The original inlined loop ran review-dispatch persistence BEFORE closing the
// span and before the probe stamp, and the governor/audit/timeline effects
// AFTER it. That order is not obviously load-bearing, which is exactly why it
// is pinned: a refactor is only behaviour-preserving if the observable
// sequence survives it, and "I reordered two independent-looking effects" is
// how a refactor quietly stops being one.
func TestDispatchAgentKicks_EffectOrderIsPreserved(t *testing.T) {
	r := newKickDispatchRecorder()
	dispatchAgentKicks(kickMsgs("alpha"), true, r.deps(), discardLogger())

	want := []string{
		"backoff-miss:alpha",
		"span-start:alpha",
		"start:alpha",
		"send:alpha",
		"review:alpha",
		"span-end:alpha",
		"probe-stamp",
		"delivered:alpha",
	}
	if !equalStrings(r.calls, want) {
		t.Fatalf("effect order changed.\n got: %v\nwant: %v", r.calls, want)
	}
}

// TestDispatchAgentKicks_EmptyAndAllWithheld covers the boundaries: no work is
// not an error, and a fully-suppressed cycle must report nothing delivered
// rather than an empty-but-non-nil result the caller might misread.
func TestDispatchAgentKicks_EmptyAndAllWithheld(t *testing.T) {
	r := newKickDispatchRecorder()
	if got := dispatchAgentKicks(nil, true, r.deps(), discardLogger()); len(got) != 0 {
		t.Fatalf("no messages must deliver nothing, got %v", got)
	}
	if len(r.calls) != 0 {
		t.Fatalf("no messages must perform no effects, got %v", r.calls)
	}

	r2 := newKickDispatchRecorder()
	r2.backoff["alpha"] = true
	r2.backoff["beta"] = true
	if got := dispatchAgentKicks(kickMsgs("alpha", "beta"), true, r2.deps(), discardLogger()); len(got) != 0 {
		t.Fatalf("an entirely backed-off cycle must deliver nothing, got %v", got)
	}
	if len(r2.sent) != 0 {
		t.Fatalf("an entirely backed-off cycle must send nothing, got %v", r2.sent)
	}
}

// TestDispatchAgentKicks_AgentsDeliverConcurrently pins the fix for the
// 20-minute governor tick: kicks to DIFFERENT agents must be in flight at the
// same time, so the tick waits for the slowest agent rather than the sum of
// every agent's prompt wait.
//
// Each agent's sendKick parks until every lane has arrived. Serial delivery
// would park the first lane forever, so the dispatch completing at all IS the
// assertion; the timeout only turns a regression into a clean failure.
func TestDispatchAgentKicks_AgentsDeliverConcurrently(t *testing.T) {
	const agents = 3
	var arrived sync.WaitGroup
	arrived.Add(agents)
	r := newKickDispatchRecorder()
	deps := r.deps()
	deps.sendKick = func(agent, message string) error {
		arrived.Done()
		arrived.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		r.sent = append(r.sent, agent)
		return nil
	}

	done := make(chan []string, 1)
	go func() { done <- dispatchAgentKicks(kickMsgs("alpha", "beta", "gamma"), false, deps, discardLogger()) }()
	select {
	case delivered := <-done:
		if want := []string{"alpha", "beta", "gamma"}; !equalStrings(delivered, want) {
			t.Fatalf("delivered = %v, want %v in message order", delivered, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch did not complete: kicks to different agents are being delivered serially")
	}
}

// A successful fix kick starts a turn, so the governor kick must be deferred.
func TestDispatchAgentKicks_SameAgentDefersAfterDelivery(t *testing.T) {
	r := newKickDispatchRecorder()
	var msgsSeen []string
	deps := r.deps()
	deps.sendKick = func(agent, message string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		msgsSeen = append(msgsSeen, message)
		return nil
	}
	msgs := []scheduler.KickMessage{
		{Agent: "alpha", Message: "fix first"},
		{Agent: "alpha", Message: "governor second"},
	}
	delivered := dispatchAgentKicks(msgs, false, deps, discardLogger())
	if want := []string{"alpha"}; !equalStrings(delivered, want) {
		t.Fatalf("delivered = %v, want %v", delivered, want)
	}
	if want := []string{"fix first"}; !equalStrings(msgsSeen, want) {
		t.Fatalf("same-agent kicks reordered: %v, want %v", msgsSeen, want)
	}
}

// TestDispatchAgentKicks_EffectsStayInMessageOrderAcrossAgents pins that the
// caller-side effects (span close, probe stamp, delivered bookkeeping) are
// applied in MESSAGE order even when a later agent's lane finishes first —
// the effect closures in runEvalCycle are unlocked and expect one goroutine.
func TestDispatchAgentKicks_EffectsStayInMessageOrderAcrossAgents(t *testing.T) {
	r := newKickDispatchRecorder()
	alphaMayFinish := make(chan struct{})
	deps := r.deps()
	deps.sendKick = func(agent, message string) error {
		if agent == "alpha" {
			<-alphaMayFinish
		}
		if agent == "beta" {
			close(alphaMayFinish)
		}
		return nil
	}
	delivered := dispatchAgentKicks(kickMsgs("alpha", "beta"), true, deps, discardLogger())
	if want := []string{"alpha", "beta"}; !equalStrings(delivered, want) {
		t.Fatalf("delivered = %v, want %v", delivered, want)
	}
	if !equalStrings(r.spansEnded, []string{"alpha", "beta"}) {
		t.Fatalf("spans closed out of message order: %v", r.spansEnded)
	}
	if idx := indexOf(r.calls, "probe-stamp"); idx == -1 || r.calls[idx-1] != "span-end:alpha" || r.calls[idx+1] != "delivered:alpha" {
		t.Fatalf("probe must be stamped on alpha (first message), calls were %v", r.calls)
	}
}

// equalStringSets compares ignoring order: delivery lanes for different agents
// run concurrently, so which agent's send lands first is not part of the
// contract — only WHICH agents were sent to is.
func equalStringSets(a, b []string) bool {
	a = append([]string(nil), a...)
	b = append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	return equalStrings(a, b)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasOrderedPrefix(a, prefix []string) bool {
	if len(a) < len(prefix) {
		return false
	}
	for i := range prefix {
		if a[i] != prefix[i] {
			return false
		}
	}
	return true
}

func indexOf(hay []string, needle string) int {
	for i, v := range hay {
		if v == needle {
			return i
		}
	}
	return -1
}

func TestDispatchAgentKicks_BusyDefersLaneWithoutFailure(t *testing.T) {
	for _, busy := range []bool{true, false} {
		t.Run(fmt.Sprint(busy), func(t *testing.T) {
			r := newKickDispatchRecorder()
			if busy {
				r.sendErr["alpha"] = fmt.Errorf("prompt wait: %w", crux.ErrAgentBusy)
			} else {
				r.sendErr["alpha"] = errors.New("CLI stalled")
			}
			deps := r.deps()
			var failures []string
			deps.onFailed = func(msg scheduler.KickMessage, err error) { failures = append(failures, msg.Agent) }
			delivered := dispatchAgentKicks(kickMsgs("alpha", "alpha", "beta"), true, deps, discardLogger())
			if !equalStrings(delivered, []string{"beta"}) || len(r.probeStamp) != 1 {
				t.Fatalf("delivered=%v probe stamps=%v", delivered, r.probeStamp)
			}
			sends := 0
			for _, call := range r.calls {
				if call == "send:alpha" {
					sends++
				}
			}
			if busy {
				if len(failures) != 0 || sends != 1 {
					t.Fatalf("busy: failures=%v sends=%d", failures, sends)
				}
			} else if len(failures) != 2 || sends != 2 {
				t.Fatalf("genuine failure: failures=%v sends=%d", failures, sends)
			}
			if len(r.spansEnded) != 3 {
				t.Fatalf("unclosed spans: %v", r.spansEnded)
			}
		})
	}
}

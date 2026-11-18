package proxy

import (
	"fmt"
	"time"
)

// The per-request write gates (repo pause, agent repo scope, repo auto-merge
// policy, write_surface.enforce) each consult a live predicate the hive
// installs through a Set*Func setter. Those predicates are host code reading
// live config, and the proxy used to call them bare on the request goroutine.
// That left two failure branches on the decision path that did not deny:
//
//   - A panic. No goroutine above the MITM request loop recovers, so a panic
//     in a predicate took down the whole proxy process (every agent's egress)
//     instead of refusing the one request it was evaluating.
//   - A hang. A predicate blocked on a lock or a slow store held the agent's
//     request open indefinitely with no answer at all.
//
// The predicates return a plain bool, so there is no error value to
// mishandle; a panic or a timeout is how such a callback fails. Every gate
// now calls its predicate through callPolicyPredicate and treats either
// failure as the deny answer for that gate: paused, not scoped, auto-merge
// off, lane enforced. Fail closed, same direction as the dial-time
// classifier in plain_forward_guard.go refusing an address it cannot parse.
//
// Intentional exceptions, not routed through this guard:
//
//   - githubBudgetCap / githubReserveFloor / githubReserveSnapshot are rate
//     limits, not authorization; they default to config values when unset
//     and a wrong answer costs quota, not a policy bypass.
//   - agentTokenSource / linearCredential supply credentials rather than
//     decide; a missing credential already makes the upstream refuse.
//   - writeRefusedAudit and canaryLeakFunc are sinks called after a decision
//     has been made; they cannot turn a deny into an allow.

// policyCallbackTimeout bounds one policy predicate call. The predicates read
// in-memory config, so anything near this is already a fault. A var only so
// the fault-injection test can shorten it.
var policyCallbackTimeout = 2 * time.Second

// policyCallbackFailedReason is the agent-facing 403 body when a gate could
// not be evaluated. It deliberately does not reuse the gate's own directive
// ("repository X is PAUSED by the operator"), which would assert an operator
// decision nobody made.
func policyCallbackFailedReason(gate string) string {
	return "the proxy could not evaluate the " + gate + " policy for this request (the policy check failed or timed out), so the write is refused: policy checks fail closed. Retry shortly; if it keeps happening, report it to the operator as a hive fault."
}

// callPolicyPredicate runs fn with panic recovery and policyCallbackTimeout.
// ok is false when fn panicked or did not return in time; the caller must
// then take its deny branch. On timeout the goroutine running fn is left to
// finish on its own (Go cannot cancel it); the buffered channel lets it exit
// without a reader.
func (p *GitHubProxy) callPolicyPredicate(gate string, fn func() bool) (result, ok bool) {
	type outcome struct {
		value    bool
		panicked bool
		cause    any
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- outcome{panicked: true, cause: r}
			}
		}()
		done <- outcome{value: fn()}
	}()
	timer := time.NewTimer(policyCallbackTimeout)
	defer timer.Stop()
	select {
	case o := <-done:
		if o.panicked {
			p.logPolicyCallbackFailure(gate, fmt.Sprintf("panic: %v", o.cause))
			return false, false
		}
		return o.value, true
	case <-timer.C:
		p.logPolicyCallbackFailure(gate, "timed out after "+policyCallbackTimeout.String())
		return false, false
	}
}

func (p *GitHubProxy) logPolicyCallbackFailure(gate, cause string) {
	if p == nil || p.logger == nil {
		return
	}
	p.logger.Error("proxy: policy callback failed; denying the request", "gate", gate, "cause", cause)
}

// guardRepoPredicate wraps a func(repo) bool gate predicate so a failed call
// answers onFail (the gate's deny value) and sets *failed. A nil fn stays nil
// so the Refusal functions keep their "no predicate installed" semantics.
func (p *GitHubProxy) guardRepoPredicate(gate string, fn func(string) bool, onFail bool, failed *bool) func(string) bool {
	if fn == nil {
		return nil
	}
	return func(repo string) bool {
		v, ok := p.callPolicyPredicate(gate, func() bool { return fn(repo) })
		if !ok {
			*failed = true
			return onFail
		}
		return v
	}
}

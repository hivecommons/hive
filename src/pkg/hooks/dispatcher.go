package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// AuditSink records every hook firing durably. It mirrors agent.AuditSink's
// shape so the dashboard's existing recorder satisfies both without an adapter,
// and so hook firings land in /data/audit.jsonl alongside every other
// privileged action — which is the RFC's requirement that hooks be as
// auditable as a dashboard button.
type AuditSink interface {
	Record(actor, action, agentName string, fields map[string]any)
}

// Audit action names for hook firings. Snake_case to match the dashboard's
// existing audit vocabulary so the Audit Log UI's filter treats them uniformly.
const (
	// AuditHookFired records a hook whose action completed successfully.
	AuditHookFired = "hook_fired"
	// AuditHookFailed records a hook whose action returned an error. The
	// transition and every other hook are unaffected.
	AuditHookFailed = "hook_failed"
	// AuditHookRateLimited records a firing suppressed by the rate limit.
	// Recorded rather than dropped silently: a hook that stops firing because
	// it hit its ceiling must be diagnosable.
	AuditHookRateLimited = "hook_rate_limited"
	// AuditHookSuppressed records hooks not dispatched because the transition
	// was itself hook-caused (the depth-1 causation guard). One entry per
	// transition, not per hook, since the whole dispatch is skipped.
	AuditHookSuppressed = "hook_suppressed_depth"
)

// auditActorSystem attributes a firing to the hive process rather than a
// person. Hooks fire off durable state transitions, not HTTP requests, so
// there is no authenticated user; the human who WROTE the hook is recorded in
// config provenance, and the hook name is in every audit entry.
const auditActorSystem = "system"

// hookActionTimeout bounds how long a single hook action may take. Hooks fire
// off the post-commit path in a background goroutine, so a hung action cannot
// block a transition — but without a bound it would leak a goroutine and, for
// the pause action, hold a slot indefinitely. The budget is generous relative
// to the actions in the vetted set (notify's HTTP client already has its own
// 10s timeout; pause and annotate are local).
const hookActionTimeout = 30 * time.Second

// Dispatcher fires hooks on committed state transitions.
//
// # Guarantees
//
//  1. POST-COMMIT ONLY. Fire is called by an emitting site AFTER the durable
//     write succeeds. The dispatcher never sits inline in the critical path:
//     Fire hands work to a goroutine and returns immediately, so no hook can
//     slow, block, or fail the transition that triggered it. A hook fired
//     before the persist could act on a transition that never durably
//     happened, which recent incident history says is not hypothetical.
//  2. DEPTH-1 CAUSATION. A transition caused by a hook action does not fire
//     hooks. Checked in Fire, the single entry point, so no emitter or action
//     can bypass it.
//  3. PER-HOOK RATE LIMIT. Every hook has a firings-per-minute ceiling, so a
//     flapping transition cannot become a notification storm.
//  4. FAILURE ISOLATION. Each hook runs independently with its own recovered
//     panic boundary; one bad hook affects neither the transition nor any
//     other hook.
//  5. EVERY FIRING IS AUDITED — success, failure, rate-limit, and depth
//     suppression all land in the audit log.
//
// The zero value is not usable; construct with NewDispatcher.
type Dispatcher struct {
	// mu guards registry and limiter. Held only briefly to snapshot the
	// matching hooks — never across an action, which would serialize every
	// hook in the fleet behind the slowest one.
	mu       sync.Mutex
	registry *Registry
	limiter  *rateLimiter

	// Sinks. Each is optional; a nil sink makes its action a reported error
	// rather than a panic or a silent success.
	notifier  Notifier
	pauser    Pauser
	annotator Annotator
	approvals ApprovalQueue

	audit  AuditSink
	logger *slog.Logger

	// now is the clock, injectable so rate-limit tests do not sleep.
	now func() time.Time

	// wg tracks in-flight hook goroutines so tests (and a graceful shutdown)
	// can wait for dispatch to settle. Without it, an async dispatcher is
	// untestable except by sleeping, and sleep-based tests are flaky tests.
	wg sync.WaitGroup
}

// Option configures a Dispatcher.
type Option func(*Dispatcher)

// WithNotifier wires the notification fanout for the notify action.
func WithNotifier(n Notifier) Option { return func(d *Dispatcher) { d.notifier = n } }

// WithPauser wires the audited pause API for the pause action.
func WithPauser(p Pauser) Option { return func(d *Dispatcher) { d.pauser = p } }

// WithAnnotator wires the lifecycle timeline for the annotate action.
func WithAnnotator(a Annotator) Option { return func(d *Dispatcher) { d.annotator = a } }

// WithApprovalQueue wires the #4000 tool-approval queue for the
// enqueue-approval action. See the ApprovalQueue doc for the wiring point.
func WithApprovalQueue(q ApprovalQueue) Option { return func(d *Dispatcher) { d.approvals = q } }

// WithAuditSink wires durable audit recording of every firing.
func WithAuditSink(s AuditSink) Option { return func(d *Dispatcher) { d.audit = s } }

// WithClock overrides the dispatcher's clock. For tests.
func WithClock(now func() time.Time) Option {
	return func(d *Dispatcher) {
		if now != nil {
			d.now = now
		}
	}
}

// NewDispatcher builds a Dispatcher over a compiled registry. A nil registry is
// valid and means "no hooks configured" — every Fire is then a cheap no-op.
func NewDispatcher(reg *Registry, logger *slog.Logger, opts ...Option) *Dispatcher {
	if logger == nil {
		logger = slog.Default()
	}
	d := &Dispatcher{
		registry: reg,
		limiter:  newRateLimiter(),
		logger:   logger,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// SetRegistry atomically swaps the hook set, for config hot reload. In-flight
// dispatches finish against the registry they started with; subsequent Fires
// use the new one. The rate limiter is deliberately PRESERVED across a swap:
// resetting it would let an operator (or a reload loop) clear the storm
// ceiling by touching the config.
func (d *Dispatcher) SetRegistry(reg *Registry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.registry = reg
}

// Len reports the number of registered hooks. Nil-safe.
func (d *Dispatcher) Len() int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.registry.Len()
}

// Wait blocks until all in-flight hook actions complete. Intended for tests
// and graceful shutdown; it does not prevent new Fires.
func (d *Dispatcher) Wait() {
	if d == nil {
		return
	}
	d.wg.Wait()
}

// Fire dispatches the hooks registered for a committed transition.
//
// CALL THIS ONLY AFTER THE DURABLE WRITE SUCCEEDS. Fire returns immediately;
// hook actions run in the background, so the caller's critical path is never
// extended and a hook can never fail the transition.
//
// Nil-safe and cheap when no hooks match: a Dispatcher with an empty registry
// costs one map lookup.
func (d *Dispatcher) Fire(ctx context.Context, p Payload) {
	if d == nil {
		return
	}
	if p.At == 0 {
		p.At = d.now().UnixMilli()
	}

	// Defensively copy Attrs. Fire takes Payload by value, but a map field is
	// only a header: the caller and every hook goroutine would otherwise share
	// one map. Because dispatch is ASYNCHRONOUS, an emitting site that reuses
	// or mutates its attrs map after Fire returns — entirely reasonable-looking
	// code, since Fire appears to have finished — would race the hooks reading
	// it, and a racy read on the post-commit path is exactly the kind of bug
	// that shows up only under production concurrency.
	//
	// Copying here rather than documenting a "do not touch this map afterwards"
	// rule keeps the hazard out of every current and future emitter.
	if p.Attrs != nil {
		attrs := make(map[string]string, len(p.Attrs))
		for k, v := range p.Attrs {
			attrs[k] = v
		}
		p.Attrs = attrs
	}

	// GUARANTEE 2 — the depth-1 causation guard. Checked before anything else
	// and in the one place every firing must pass through, so a hook-caused
	// transition can never cascade regardless of which emitter produced it.
	if !p.Causation.MayFireHooks() {
		d.logger.Debug("hooks: transition is hook-caused; not firing hooks (depth-1 guard)",
			"transition", p.Transition,
			"causedBy", p.Causation.HookName,
			"origin", p.Causation.OriginTransition)
		d.record(AuditHookSuppressed, "", p.Agent, map[string]any{
			"transition": string(p.Transition),
			"reason":     "depth-1 causation guard",
			"caused_by":  p.Causation.HookName,
			"origin":     string(p.Causation.OriginTransition),
		})
		return
	}

	// Snapshot the matching hooks and their rate-limit decisions under the
	// lock, then release it before running any action.
	//
	// NOTE the ordering: the rate limit is consumed HERE, before the `when:`
	// predicate is evaluated in runOne, so a firing that the predicate later
	// declines still costs a slot. That is deliberate — the limit bounds the
	// work a flapping transition can induce, and predicate evaluation is
	// itself part of that work, so checking the cheap counter first is what
	// keeps a storm from turning into a CEL-evaluation storm. The tradeoff is
	// that a hook with a highly selective predicate on a noisy transition can
	// exhaust its quota without ever firing; such a hook wants a higher
	// rate_limit_per_minute, and the suppression is visible in the audit log
	// as hook_rate_limited rather than being silent.
	type dispatch struct {
		hook Hook
		pred *predicate
	}
	var runnable []dispatch
	var limited []Hook

	d.mu.Lock()
	now := d.now()
	for _, ch := range d.registry.For(p.Transition) {
		if !d.limiter.allow(ch.hook.Name, ch.hook.effectiveRateLimit(), now) {
			limited = append(limited, ch.hook)
			continue
		}
		runnable = append(runnable, dispatch(ch))
	}
	d.mu.Unlock()

	for _, h := range limited {
		d.logger.Warn("hooks: firing suppressed by rate limit",
			"hook", h.Name, "transition", p.Transition, "limit", h.effectiveRateLimit())
		d.record(AuditHookRateLimited, h.Name, p.Agent, map[string]any{
			"transition": string(p.Transition),
			"limit":      h.effectiveRateLimit(),
		})
	}

	if len(runnable) == 0 {
		return
	}

	// GUARANTEE 1 — post-commit and off the critical path. Each hook runs in
	// its own goroutine so the emitting site returns immediately and one slow
	// action cannot delay another hook.
	for _, dp := range runnable {
		d.wg.Add(1)
		go func(h Hook, pred *predicate) {
			defer d.wg.Done()
			d.runOne(ctx, h, pred, p)
		}(dp.hook, dp.pred)
	}
}

// runOne evaluates one hook's predicate and executes its action, isolating any
// failure. This is GUARANTEE 4: the recovered panic boundary plus the returned
// error mean a hook that misbehaves — including one that panics inside a sink
// implementation — cannot take down the dispatch goroutine, the transition, or
// another hook.
func (d *Dispatcher) runOne(ctx context.Context, h Hook, pred *predicate, p Payload) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("hooks: action panicked; isolated",
				"hook", h.Name, "transition", p.Transition, "panic", r)
			d.record(AuditHookFailed, h.Name, p.Agent, map[string]any{
				"transition": string(p.Transition),
				"action":     string(h.Action),
				"error":      fmt.Sprintf("panic: %v", r),
			})
		}
	}()

	// The `when:` predicate. Fail-closed: an unevaluable predicate is a
	// no-match, so a broken expression can never cause an action to run.
	if !pred.matches(p, d.logger) {
		d.logger.Debug("hooks: when-predicate did not match; skipping",
			"hook", h.Name, "transition", p.Transition)
		return
	}

	// The action gets its own bounded context so a hung sink cannot leak a
	// goroutine forever. Derived from the caller's ctx so shutdown still
	// cancels promptly.
	actionCtx, cancel := context.WithTimeout(ctx, hookActionTimeout)
	defer cancel()

	fields := map[string]any{
		"transition": string(p.Transition),
		"action":     string(h.Action),
	}
	// Carry the model metadata into the audit entry when the transition has
	// it, so the audit log alone answers "which model produced the rejected
	// output" without a join against tracing.
	if p.Model != "" {
		fields["model"] = p.Model
	}
	if p.Backend != "" {
		fields["backend"] = p.Backend
	}
	if p.Pin != "" {
		fields["pin"] = p.Pin
	}

	if err := d.execute(actionCtx, h, p); err != nil {
		d.logger.Error("hooks: action failed; transition and other hooks unaffected",
			"hook", h.Name, "transition", p.Transition, "action", h.Action, "error", err)
		fields["error"] = err.Error()
		fields["outcome"] = "failure"
		d.record(AuditHookFailed, h.Name, p.Agent, fields)
		return
	}

	d.logger.Info("hooks: fired",
		"hook", h.Name, "transition", p.Transition, "action", h.Action)
	fields["outcome"] = "success"
	d.record(AuditHookFired, h.Name, p.Agent, fields)
}

// record writes one audit entry, tolerating an unwired sink. GUARANTEE 5.
// The hook name is carried in fields rather than the agent slot so an entry
// always names the responsible config rule even for a transition with no agent.
func (d *Dispatcher) record(action, hookName, agentName string, fields map[string]any) {
	if d.audit == nil {
		return
	}
	if fields == nil {
		fields = map[string]any{}
	}
	if hookName != "" {
		fields["hook"] = hookName
	}
	d.audit.Record(auditActorSystem, action, agentName, fields)
}

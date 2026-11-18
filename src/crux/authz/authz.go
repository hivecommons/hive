// Package authz is the seam for the authorization decision point described in
// src/docs/design/authorization-decision-point.md (RFC #18988).
//
// This is phase 0/1 of that design and it is SHADOW MODE ONLY:
//
//   - Decide is a pure, in-process evaluator. It mirrors one existing decision,
//     the push broker's changed-files guard (empty push, then protected paths).
//   - A Shadow compares that evaluation with what the existing gate decided,
//     records one versioned decision event per call on its own bounded stream,
//     and logs divergences. It is OFF by default and gated by HIVE_AUTHZ_SHADOW.
//   - Nothing here enforces anything. No caller reads a Decision to allow or
//     refuse an action; the existing gate remains the only authority, so
//     enabling or disabling the shadow changes no behaviour.
//
// Fail-closed semantics (documented, not enforcing). When a later phase lets
// the seam decide, an evaluator error or panic is a deny with reason
// authz_error, and a request that lacks a required attribute is a deny with
// reason missing_attribute. Decide already returns exactly those decisions so
// the semantics are testable now, but in this phase a shadow deny that the
// existing gate did not issue is only a logged divergence.
//
// The package is a leaf on purpose. The proxy and the push broker live in the
// same module and must be able to import it, so it imports nothing from them.
// Parity with the existing gates is proven from an external test package.
//
// Events carry attributes only: names, counts and codes. Never request bodies,
// diffs, file contents, credentials or full path lists.
package authz

// SchemaVersion is the version of the decision event schema (Event.V). New
// fields are additive; a change of meaning bumps it.
const SchemaVersion = 1

// FloorVersion identifies the built-in rule set that Decide evaluates. It is
// part of every decision's PolicyVersion, so an event states which rules
// produced it. Bump it whenever a built-in rule changes.
const FloorVersion = "floor-1"

// Effect is the outcome of a decision.
type Effect string

const (
	// EffectAllow means the request may proceed.
	EffectAllow Effect = "allow"
	// EffectDeny means the request must not proceed.
	EffectDeny Effect = "deny"
	// EffectHold means a human must decide. It is part of the schema from the
	// start so later phases add no new effect, but no built-in rule returns it
	// in this phase.
	EffectHold Effect = "hold"
)

// Surface is the enforcement point a request arrives at.
type Surface string

const (
	// SurfaceProxy is the GitHub MITM proxy.
	SurfaceProxy Surface = "proxy"
	// SurfaceRelay is a hive-side request relay.
	SurfaceRelay Surface = "relay"
	// SurfaceBroker is the sandbox push broker.
	SurfaceBroker Surface = "broker"
)

// KindBranchPush is the action kind for pushing a branch. It is the same
// spelling as effects.KindBranchPush; a test in this package pins the two
// together so there is one vocabulary, not two.
const KindBranchPush = "branch_push"

// ForgeGitHub is the only forge the broker pushes to today.
const ForgeGitHub = "github"

// Reason codes. Codes that exist in the proxy keep their spelling (see
// src/crux/proxy/denial_message.go); the ones below are new and namespaced
// to the broker or the seam.
const (
	// ReasonOK is the reason on an allow.
	ReasonOK = "ok"
	// ReasonEmptyPush is a push with no committed changes.
	ReasonEmptyPush = "empty_push"
	// ReasonProtectedPath is a push that touches a protected path.
	ReasonProtectedPath = "protected_path"
	// ReasonMissingAttribute is a request that lacks a required attribute.
	ReasonMissingAttribute = "missing_attribute"
	// ReasonUnsupportedAction is a request no built-in rule covers.
	ReasonUnsupportedAction = "unsupported_action"
	// ReasonAuthzError is an evaluator error or panic.
	ReasonAuthzError = "authz_error"
)

// Stable rule ids of the built-in floor. A decision names the rule that
// produced it.
const (
	// RuleBrokerEmptyPush is the broker's empty-push refusal.
	RuleBrokerEmptyPush = "floor.broker.changed_files.empty_push"
	// RuleBrokerProtectedPath is the broker's protected-path refusal.
	RuleBrokerProtectedPath = "floor.broker.changed_files.protected_path"
	// RuleBrokerChangedFilesPass is the broker's changed-files guard passing.
	RuleBrokerChangedFilesPass = "floor.broker.changed_files.pass"
	// RuleMissingAttribute is the required-attribute check.
	RuleMissingAttribute = "floor.request.missing_attribute"
	// RuleUnsupportedAction is the answer for an action with no built-in rule.
	RuleUnsupportedAction = "floor.request.unsupported_action"
	// RuleEvaluatorError is the answer when the evaluator itself failed.
	RuleEvaluatorError = "floor.evaluator.error"
)

// Request is the flat, typed input of a decision. Flat on purpose: it is the
// stable surface later operator rules are written against. It never carries
// request bodies, diffs, file contents or credentials, only attributes derived
// from them.
type Request struct {
	Actor    Actor
	Action   Action
	Resource Resource
	Context  Context
}

// Actor is who is acting.
type Actor struct {
	// Agent is the agent name, empty when the caller cannot name one.
	Agent string
	// Mode is the agent permission mode when the surface knows it.
	Mode string
	// ACMMLevel is the ACMM level when the surface knows it, else zero.
	ACMMLevel int
	// Identified is true when the agent was attributed to a known name.
	Identified bool
}

// Action is what is being attempted.
type Action struct {
	// Kind is the action vocabulary entry, for example KindBranchPush.
	Kind string
	// Surface is the enforcement point.
	Surface Surface
	// Forge names the forge, for example ForgeGitHub.
	Forge string
}

// Resource is what the action targets.
type Resource struct {
	// Repo is owner/name.
	Repo string
	// Branch is the target branch.
	Branch string
	// Paths are the changed files for a push. Only a count of them is ever
	// recorded in an event.
	Paths []string
}

// Context is the surrounding policy input the caller already holds.
type Context struct {
	// RequestID correlates the event with the caller's own log line.
	RequestID string
	// ProtectedPaths is the effective protected path list the caller applies.
	// A guard ending in "/" protects a directory tree; any other guard
	// protects exactly that file.
	ProtectedPaths []string
}

// Decision is the output of Decide.
type Decision struct {
	// Effect is allow, deny or hold.
	Effect Effect
	// RuleID is the stable id of the rule that produced the decision.
	RuleID string
	// ReasonCode is the machine-readable reason.
	ReasonCode string
	// PolicyVersion identifies the rule set and policy inputs used.
	PolicyVersion string
	// MatchedPaths is how many changed paths a path rule matched.
	MatchedPaths int
	// Shadow is true when the decision was computed but not enforced. A Shadow
	// observer sets it; in this phase every recorded decision is shadow.
	Shadow bool
}

// Verdict is what the existing gate decided, expressed in the seam's terms so
// the two can be compared. The existing gates have no rule ids or policy
// version, so the comparison is on effect and reason code only.
type Verdict struct {
	Effect     Effect
	ReasonCode string
}

// Matches reports whether the evaluation agrees with the existing gate: the
// same effect and the same reason code.
func (v Verdict) Matches(d Decision) bool {
	return v.Effect == d.Effect && v.ReasonCode == d.ReasonCode
}

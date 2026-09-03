package toolapprove

// The approval desk: the ONE decision point every approval-shaped request
// resolves through.
//
// RFC #4000 diagnosed four independently-grown gates, each the same shape —
// (requested action, ACMM level, identity) → verdict — with four different
// failure modes:
//
//   - config.SelfMergeMinACMMLevel / AutoMergeConfig.SelfAuthoredAutoMergeAllowed
//   - config.PlanAutoApproveForLevel        (decomposition path only)
//   - config.PlanningConfig.PlanFromLabelEnabled
//   - github.isTrustedMerger + latestHiveQueueApproval (inside the sweep)
//
// Desk.Resolve is the convergence point those four were reaching for. It does
// not delete them — call sites migrate onto the desk incrementally, and
// MigrationParity (parity.go) pins that the desk's answer equals the legacy
// gate's answer for every level, so a migration can never silently downgrade a
// hive.
//
// Three layers, evaluated in a fixed order:
//
//  1. The BASE verdict from Decide (hard guardrails, tool policy, ACMM level).
//  2. Operator RULES as data — CEL expressions from config, evaluated through
//     the existing celtrigger posture (compile-time rejection of malformed
//     rules, runtime error = no-match). A rule may relax the base verdict.
//  3. The ACMM CEILING, applied LAST and unconditionally. A rule can never
//     approve above the hive's level. This ordering is the whole safety
//     argument: rules are advisory input to a decision the ceiling always has
//     the final word on. Test-pinned in desk_ceiling_test.go.
//
// The L6 throughput contract (#4000 comment 5321778076) is a property of this
// file, not of the caller: Resolve is synchronous and allocation-light, and a
// request that resolves to auto-approve returns DIRECTLY from here having
// touched no queue, no store, and no external process. Only the operator lane
// reaches the durable inbox, and only via the caller's explicit Enqueue.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RuleAction is the resolution an operator rule requests when its expression
// matches. The set is deliberately smaller than Decision: a rule may steer a
// request into a lane, but "deny" is the base policy's and the ceiling's
// prerogative, never a rule's alone.
type RuleAction string

const (
	// RuleActionAutoApprove asks that a matching request resolve immediately,
	// subject to the ACMM ceiling.
	RuleActionAutoApprove RuleAction = "auto-approve"
	// RuleActionSecurityScan routes a matching request through the scanner.
	RuleActionSecurityScan RuleAction = "security-scan"
	// RuleActionOperatorApprove sends a matching request to the operator inbox.
	// Always permitted: escalation is never gated by the ceiling.
	RuleActionOperatorApprove RuleAction = "operator-approve"
)

// Valid reports whether the action is one the desk understands.
func (a RuleAction) Valid() bool {
	switch a {
	case RuleActionAutoApprove, RuleActionSecurityScan, RuleActionOperatorApprove:
		return true
	}
	return false
}

// decisionForAction maps a rule action onto the decision it requests.
func (a RuleAction) decision() Decision {
	switch a {
	case RuleActionAutoApprove:
		return DecisionAutoApprove
	case RuleActionSecurityScan:
		return DecisionSecurityScan
	default:
		return DecisionOperatorApprove
	}
}

// Rule is one operator-authored approval rule: when Expr matches the request,
// the desk applies Action, subject to the ACMM ceiling. Rules are data — they
// live in config (`tool_approval.rules:`), are compiled at load, and are
// versioned and audited like any other config.
type Rule struct {
	// Name identifies the rule in verdicts, audit records, and the dashboard's
	// rule chips. Operators see this string, so it should read like a policy.
	Name string
	// Expr is the CEL expression evaluated against the "request" activation.
	Expr string
	// Action is the resolution requested when Expr matches.
	Action RuleAction
	// Priority orders competing rules; higher wins. Ties break on declaration
	// order, so a config file reads top-to-bottom as written.
	Priority int
	// MinACMMLevel optionally restricts the rule to hives at or above a level.
	// This is a rule-scoping convenience, NOT the safety mechanism — the
	// ceiling below is what actually bounds a rule's reach.
	MinACMMLevel int
}

// RuleMatch records that a rule fired, for the verdict and the dashboard.
type RuleMatch struct {
	Name   string     `json:"name"`
	Action RuleAction `json:"action"`
}

// ACMMCeiling returns the most permissive decision a hive at the given ACMM
// level may reach for a side-effectful request, regardless of what any rule
// asks for. This is the fail-closed heart of the desk.
//
// The mapping mirrors what each level ALREADY permits today (see parity.go):
//
//	L0-L2  operator-approve — nothing side-effectful resolves without a human
//	L3-L5  security-scan    — may reach auto-approve only via a green scan
//	L6+    auto-approve     — full autonomy; the desk is an audit record, not a gate
//
// An unknown/negative level clamps to the most restrictive lane, so a
// mis-parsed or absent level can never widen authority.
func ACMMCeiling(acmmLevel int) Decision {
	switch {
	case acmmLevel >= 6:
		return DecisionAutoApprove
	case acmmLevel >= 3:
		return DecisionSecurityScan
	default:
		return DecisionOperatorApprove
	}
}

// permissiveness ranks decisions so the ceiling can be applied as a clamp.
// Higher is more permissive. Deny sits below everything: it is never something
// a ceiling raises a verdict UP to.
func permissiveness(d Decision) int {
	switch d {
	case DecisionAutoApprove:
		return 3
	case DecisionSecurityScan:
		return 2
	case DecisionOperatorApprove:
		return 1
	default: // DecisionDeny
		return 0
	}
}

// applyCeiling clamps a decision to what the hive's ACMM level permits. It only
// ever moves a verdict toward LESS authority: a request the base policy already
// sent to the operator lane is not promoted because the ceiling is higher.
func applyCeiling(d Decision, acmmLevel int) (Decision, bool) {
	ceiling := ACMMCeiling(acmmLevel)
	if permissiveness(d) > permissiveness(ceiling) {
		return ceiling, true
	}
	return d, false
}

// Desk resolves approval requests. Construct it with NewDesk. A Desk is safe
// for concurrent Resolve calls: the compiled rule programs are stateless and
// the desk holds no per-request mutable state.
type Desk struct {
	rules   *RuleEngine
	scanner SecurityScanner
}

// NewDesk builds a desk over a compiled rule engine and a scanner. Both may be
// nil: a nil engine means "no operator rules" (base policy only), and a nil
// scanner installs the DefaultSecurityScanner.
func NewDesk(rules *RuleEngine, scanner SecurityScanner) *Desk {
	if scanner == nil {
		scanner = NewDefaultSecurityScanner()
	}
	return &Desk{rules: rules, scanner: scanner}
}

// Request is one approval-shaped request arriving at the desk. It generalizes
// beyond agent tool calls so the four legacy gates can migrate onto the same
// decision point: Kind distinguishes an agent tool call from a self-merge, a
// plan approval, or a queued merge, while Tool/Arguments carry the payload.
type Request struct {
	// Kind names the lane the request came from. See the Kind* constants.
	Kind string `json:"kind"`
	// Tool is the tool or action name being requested.
	Tool ToolRequest `json:"tool"`
	// Agent is the requesting identity.
	Agent AgentIdentity `json:"agent"`
	// Repo is the target repository ("org/name"), when the request has one.
	Repo string `json:"repo,omitempty"`
	// Number is the issue/PR number, when the request has one.
	Number int `json:"number,omitempty"`
	// Labels are the labels on the target issue/PR, for rule matching.
	Labels []string `json:"labels,omitempty"`
	// Author is the login that authored the target (PR author, issue opener).
	Author string `json:"author,omitempty"`
	// Title is the target's title, for rule matching.
	Title string `json:"title,omitempty"`
	// ChecksGreen reports whether required CI is green on the target. The
	// canonical bulk-approve case ("fifty green dependabot PRs") is a rule over
	// this field plus author and title.
	ChecksGreen bool `json:"checks_green,omitempty"`
	// IdempotencyKey de-duplicates a re-delivered request. Empty means the
	// caller accepts a derived key (see DeriveIdempotencyKey).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// LegacyAllowed carries the call site's existing gate result when migrating
	// a non-tool lane. It lets the desk preserve day-one behavior exactly while
	// adding rules, ceiling, inbox, and audit semantics around that result.
	LegacyAllowed bool `json:"legacy_allowed,omitempty"`
	// HasLegacyAllowed distinguishes an explicit false legacy gate result from
	// a request that did not provide one.
	HasLegacyAllowed bool `json:"has_legacy_allowed,omitempty"`
}

// Request kinds. Each corresponds to one of the gates RFC #4000 inventoried,
// plus the agent tool-call lane the desk was originally specified for.
const (
	// KindAgentTool is an agent's in-turn tool invocation.
	KindAgentTool = "agent-tool"
	// KindSelfMerge is the App self-merging a PR it authored
	// (SelfAuthoredAutoMergeAllowed / SelfMergeMinACMMLevel).
	KindSelfMerge = "self-merge"
	// KindQueuedMerge is a human-queued merge awaiting sweep
	// (isTrustedMerger + latestHiveQueueApproval).
	KindQueuedMerge = "queued-merge"
	// KindPlanApproval is a decomposition plan awaiting approval
	// (PlanAutoApproveForLevel).
	KindPlanApproval = "plan-approval"
	// KindPlanFromLabel is a label-triggered planning run
	// (PlanningConfig.PlanFromLabelEnabled).
	KindPlanFromLabel = "plan-from-label"
)

// Resolve is THE decision point. It returns the final verdict for a request,
// having applied base policy, operator rules, the ACMM ceiling, and — when the
// verdict lands in the scan lane — the security scanner.
//
// L6 throughput contract: for a request that resolves to auto-approve this
// function performs no I/O, acquires no lock, and enqueues nothing. The
// returned verdict is final and the caller proceeds in-loop.
func (d *Desk) Resolve(ctx context.Context, req Request, acmmLevel int) Verdict {
	v := d.decide(ctx, req, acmmLevel)

	// The scan lane is the only path that may consult an external scanner. A
	// verdict that is already auto-approve or operator-approve returns without
	// touching it — this is what keeps a slow scanner from taxing every L6 turn
	// whose answer is always yes.
	if v.Decision != DecisionSecurityScan {
		return v
	}
	return d.runScan(ctx, req, acmmLevel, v)
}

// Decide returns the verdict WITHOUT running the security scanner, leaving a
// scan-lane request as DecisionSecurityScan. Callers that want to schedule the
// scan asynchronously (the L6 post-hoc path) use this and resolve the scan
// themselves; callers that want the whole answer use Resolve.
func (d *Desk) Decide(ctx context.Context, req Request, acmmLevel int) Verdict {
	return d.decide(ctx, req, acmmLevel)
}

// decide runs the three layers: base policy, operator rules, ACMM ceiling.
func (d *Desk) decide(ctx context.Context, req Request, acmmLevel int) Verdict {
	// Layer 1 — base policy. For agent tool calls this is the existing Decide
	// (hard guardrails, tool policy, mode, repo allowlist, level gating). For
	// the migrated gate kinds it is the level mapping those gates encoded.
	v := d.baseVerdict(ctx, req, acmmLevel)

	// A hard deny is terminal. Guardrails (direct PR creation/merge, explicit
	// tool denies, repo allowlist, mode restrictions) are not rule-overridable:
	// letting config relax them would reopen exactly the holes they were added
	// to close.
	if v.Decision == DecisionDeny {
		v.Kind = req.Kind
		return v
	}

	// Layer 2 — operator rules.
	if d.rules != nil {
		if m, ok := d.rules.Match(req, acmmLevel); ok {
			requested := m.Action.decision()
			// A rule that asks for LESS authority than the base verdict is
			// always honored (escalation is free). A rule asking for MORE is
			// honored here and then clamped by the ceiling below.
			v.Decision = requested
			v.Rule = m.Name
			v.RuleAction = m.Action
			v.Rationale = fmt.Sprintf("operator rule %q → %s", m.Name, m.Action)
		}
	}

	// Layer 3 — the ACMM ceiling, applied LAST and unconditionally.
	if clamped, wasClamped := applyCeiling(v.Decision, acmmLevel); wasClamped {
		prior := v.Decision
		v.Decision = clamped
		v.CeilingApplied = true
		if v.Rule != "" {
			v.Rationale = fmt.Sprintf(
				"operator rule %q requested %s but ACMM L%d ceiling permits at most %s — clamped (fail-closed)",
				v.Rule, prior, acmmLevel, clamped)
		} else {
			v.Rationale = fmt.Sprintf(
				"ACMM L%d ceiling permits at most %s — clamped from %s (fail-closed)",
				acmmLevel, clamped, prior)
		}
	}

	v.Kind = req.Kind
	v.ACMMLevel = acmmLevel
	if v.Tool == "" {
		v.Tool = req.Tool.Tool
	}
	if v.Agent == "" {
		v.Agent = req.Agent.Name
	}
	return v
}

// baseVerdict produces the pre-rule, pre-ceiling verdict for a request.
func (d *Desk) baseVerdict(ctx context.Context, req Request, acmmLevel int) Verdict {
	if req.Kind == "" || req.Kind == KindAgentTool {
		// The agent tool lane keeps its existing, well-tested policy verbatim.
		return Decide(ctx, req.Tool, acmmLevel, req.Agent)
	}
	// The migrated gate kinds carry no tool-policy surface; their base verdict
	// is the level mapping the legacy gate encoded, which LegacyBaseDecision
	// derives from the same constants the legacy gate reads.
	return LegacyBaseDecision(req, acmmLevel)
}

// runScan resolves a scan-lane verdict through the scanner and re-applies the
// ceiling to the post-scan decision — a green scan must not be able to lift a
// request above its level either.
func (d *Desk) runScan(ctx context.Context, req Request, acmmLevel int, v Verdict) Verdict {
	scanner := d.scanner
	if scanner == nil {
		scanner = NewDefaultSecurityScanner()
	}

	res, err := scanner.Scan(ctx, req.Tool)
	if err != nil {
		// Fail closed: a scanner that errors denies rather than waves through.
		v.Decision = DecisionDeny
		v.Rationale = fmt.Sprintf("security scan error: %v", err)
		return v
	}
	v.ScanResult = &res

	if !res.Passed {
		v.Decision = DecisionDeny
		v.Rationale = fmt.Sprintf("security scan failed: %s", strings.Join(res.Violations, "; "))
		return v
	}

	// Green scan. Passing the scan is precisely how a level whose ceiling is
	// DecisionSecurityScan reaches auto-approve — the scan IS the route, so the
	// ceiling is satisfied here rather than violated. Levels whose ceiling is
	// the operator lane still land there: a green scan is not a substitute for
	// a human at a level that requires one.
	if AutoApproveOnGreenScan(acmmLevel) {
		v.Decision = DecisionAutoApprove
		v.Rationale = fmt.Sprintf("ACMM L%d: auto-approved on green security scan", acmmLevel)
	} else {
		v.Decision = DecisionOperatorApprove
		v.Rationale = fmt.Sprintf("security scan green; ACMM L%d requires operator approval", acmmLevel)
	}
	return v
}

// AutoApproveOnGreenScan reports whether a level resolves a green security scan
// to auto-approve. This is the second half of the ceiling: ACMMCeiling says the
// most permissive LANE a request may enter, and this says whether traversing
// the scan lane successfully reaches auto-approve.
//
// Levels at or above the scan-lane ceiling (L3+) auto-approve on green — that
// is what "routes through a pre-execution security scan; auto-approves on
// green" has always meant. Below that the ceiling is the operator lane and no
// scan result can lift a request out of it.
func AutoApproveOnGreenScan(acmmLevel int) bool {
	return permissiveness(ACMMCeiling(acmmLevel)) >= permissiveness(DecisionSecurityScan)
}

// ResolveBatch evaluates N requests through the SAME Resolve call used for a
// single request, returning one verdict per request in input order.
//
// This is the only bulk primitive the desk exposes, and it exists specifically
// so a bulk approve can never become a parallel decision path (RFC #4000: "a
// bulk approve is N individual evaluations through the same decision function
// every agent request goes through"). Callers MUST NOT implement their own
// fan-out over a different predicate. bulk_parity_test.go pins that
// ResolveBatch(reqs) == [Resolve(r) for r in reqs].
func (d *Desk) ResolveBatch(ctx context.Context, reqs []Request, acmmLevel int) []Verdict {
	out := make([]Verdict, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, d.Resolve(ctx, req, acmmLevel))
	}
	return out
}

// RuleNames returns the configured rule names in evaluation order. The
// dashboard renders these as filter chips.
func (d *Desk) RuleNames() []string {
	if d == nil || d.rules == nil {
		return nil
	}
	return d.rules.Names()
}

// WouldMatchRule reports which rule, if any, would fire for a request at a
// level — without resolving it. The Approvals panel calls this to show, per
// row, "which rule would resolve it".
func (d *Desk) WouldMatchRule(req Request, acmmLevel int) (RuleMatch, bool) {
	if d == nil || d.rules == nil {
		return RuleMatch{}, false
	}
	return d.rules.Match(req, acmmLevel)
}

// sortRuleNames keeps chip ordering stable for the UI.
func sortRuleNames(names []string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

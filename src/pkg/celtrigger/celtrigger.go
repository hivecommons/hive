// Package celtrigger provides CEL-based declarative agent triggering over a
// forge-neutral normalized event model.
//
// Operators declare, in config, CEL (Common Expression Language) rules that
// decide when an agent should be triggered for an event (issue opened, labeled,
// PR opened, comment created, etc.). This runs alongside — not in place of —
// Hive's built-in label/governor triggering.
//
// The evaluator fails closed: a repo's malformed rule is rejected at Compile
// time (returning an error) rather than crashing the fleet at match time, and a
// runtime evaluation error is treated as "no match" rather than propagating.
package celtrigger

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/interpreter"
)

// Event kind constants. These are the forge-neutral event kinds that a
// NormalizedEvent may carry. Forge-specific adapters (GitHub, GitLab, …) map
// their native webhook events onto these.
const (
	KindIssueOpened    = "issue.opened"
	KindIssueLabeled   = "issue.labeled"
	KindIssueClosed    = "issue.closed"
	KindIssueReopened  = "issue.reopened"
	KindPROpened       = "pr.opened"
	KindPRLabeled      = "pr.labeled"
	KindPRReadyForRev  = "pr.ready_for_review"
	KindPRClosed       = "pr.closed"
	KindPRMerged       = "pr.merged"
	KindCommentCreated = "comment.created"
)

// maxEvalCost bounds CEL runtime work per rule evaluation. The budget is high
// enough for realistic operator rules that combine field checks, string
// predicates, and modest label scans, while still stopping pathological nested
// comprehensions. A rule that exceeds this budget fails closed as no-match and
// emits a warning so operators can diagnose why it stopped firing.
const maxEvalCost uint64 = 10_000

// activationVarEvent is the top-level CEL variable name under which the
// normalized event fields are exposed to expressions.
const activationVarEvent = "event"

// NormalizedEvent is a forge-neutral representation of a source-control event.
// It is the sole activation exposed to CEL rule expressions, under the variable
// name "event".
type NormalizedEvent struct {
	// Kind is the event kind, e.g. KindIssueOpened. Exposed as event.kind.
	Kind string `cel:"kind"`
	// Repo is the fully-qualified repository, e.g. "org/name". Exposed as event.repo.
	Repo string `cel:"repo"`
	// Labels is the set of labels attached to the issue/PR. Exposed as event.labels.
	Labels []string `cel:"labels"`
	// Title is the issue/PR title. Exposed as event.title.
	Title string `cel:"title"`
	// Author is the login/handle of the actor that produced the event. Exposed as event.author.
	Author string `cel:"author"`
	// Body is the issue/PR/comment body. Exposed as event.body.
	Body string `cel:"body"`
	// IsDraft indicates a draft PR. Exposed as event.is_draft.
	IsDraft bool `cel:"is_draft"`
	// Number is the issue/PR number. Exposed as event.number.
	Number int `cel:"number"`
	// State is the current state, e.g. "open"/"closed". Exposed as event.state.
	State string `cel:"state"`
	// BaseBranch is the target branch for a PR. Exposed as event.base_branch.
	BaseBranch string `cel:"base_branch"`
	// HeadBranch is the source branch for a PR. Exposed as event.head_branch.
	HeadBranch string `cel:"head_branch"`
	// Assignees are the currently-assigned logins. Exposed as event.assignees.
	Assignees []string `cel:"assignees"`
	// Comment is the body of the triggering comment (comment.created). Exposed as event.comment.
	Comment string `cel:"comment"`
}

// activation renders the event as a CEL activation keyed by the top-level
// "event" variable. The value is the struct itself; field access resolves via
// the registered native type (see newEnv).
func (e NormalizedEvent) activation() map[string]any {
	return map[string]any{activationVarEvent: e}
}

// Rule is a single declarative trigger: when Expr evaluates true against a
// NormalizedEvent, Agent should be kicked. Priority orders competing rules
// (higher first).
type Rule struct {
	// Name is a human-readable identifier for the rule (for logs/diagnostics).
	Name string
	// Expr is the CEL expression evaluated against the "event" activation.
	Expr string
	// Agent names the agent to trigger when Expr matches.
	Agent string
	// Priority orders matched rules; higher sorts first. A rule that declares
	// no priority gets the zero value, so declared order decides ties.
	Priority int
}

// compiledRule pairs a Rule with its compiled+checked CEL program.
type compiledRule struct {
	rule    Rule
	program cel.Program
}

// Engine holds the compiled rule set and the CEL environment used to evaluate
// events. Construct it with Compile. An Engine is safe for concurrent Match
// calls (CEL programs are stateless once compiled).
type Engine struct {
	env   *cel.Env
	rules []compiledRule
}

// newEnv builds the CEL environment that exposes the normalized event and safe
// helpers. All event fields live under the "event" map variable.
func newEnv() (*cel.Env, error) {
	return cel.NewEnv(
		// Register NormalizedEvent as a native type so field access is
		// type-checked at Compile time (unknown fields become compile errors,
		// keeping bad rules out of the fleet). cel:"..." tags select field names.
		ext.NativeTypes(reflect.TypeOf(NormalizedEvent{}), ext.ParseStructTags(true)),
		cel.Variable(activationVarEvent, cel.ObjectType("celtrigger.NormalizedEvent")),
		// hasLabel(event.labels, "bug") — convenience over `"bug" in event.labels`.
		cel.Function("hasLabel",
			cel.Overload("hasLabel_list_string",
				[]*cel.Type{cel.ListType(cel.DynType), cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(hasLabelImpl),
			),
		),
	)
}

// hasLabelImpl implements hasLabel(list, needle): true if needle is present in
// the list (string comparison). Non-list / non-string inputs yield false rather
// than erroring, keeping evaluation fail-closed.
func hasLabelImpl(listVal, needleVal ref.Val) ref.Val {
	needle, ok := needleVal.Value().(string)
	if !ok {
		return types.Bool(false)
	}
	lister, ok := listVal.(traits.Lister)
	if !ok {
		return types.Bool(false)
	}
	it := lister.Iterator()
	for it.HasNext() == types.True {
		item := it.Next()
		if s, ok := item.Value().(string); ok && s == needle {
			return types.Bool(true)
		}
	}
	return types.Bool(false)
}

// Compile compiles each rule's CEL expression against the NormalizedEvent
// activation and returns a ready-to-use Engine. It fails closed: if any rule's
// expression fails to parse, type-check, or produce a boolean result, Compile
// returns an error and no Engine — so a bad rule is caught before it can run,
// and the caller can reject the config rather than crash the fleet.
//
// An empty rule set yields a valid Engine that never matches.
func Compile(rules []Rule) (*Engine, error) {
	env, err := newEnv()
	if err != nil {
		return nil, fmt.Errorf("celtrigger: build env: %w", err)
	}

	compiled := make([]compiledRule, 0, len(rules))
	for i, r := range rules {
		name := r.Name
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("rule[%d]", i)
		}
		if strings.TrimSpace(r.Expr) == "" {
			return nil, fmt.Errorf("celtrigger: rule %q: empty expression", name)
		}

		ast, iss := env.Compile(r.Expr)
		if iss != nil && iss.Err() != nil {
			return nil, fmt.Errorf("celtrigger: rule %q: compile: %w", name, iss.Err())
		}
		if ast.OutputType() != cel.BoolType {
			return nil, fmt.Errorf("celtrigger: rule %q: expression must return bool, got %s", name, ast.OutputType())
		}

		prg, err := env.Program(ast, cel.CostLimit(maxEvalCost), cel.InterruptCheckFrequency(100))
		if err != nil {
			return nil, fmt.Errorf("celtrigger: rule %q: program: %w", name, err)
		}

		compiled = append(compiled, compiledRule{rule: r, program: prg})
	}

	return &Engine{env: env, rules: compiled}, nil
}

// Match evaluates every compiled rule against the event and returns those whose
// expression evaluated to true, sorted by descending Priority (ties preserve
// declaration order). A rule whose evaluation errors at runtime is skipped
// (fail-closed: it simply does not match), never panicking.
func (e *Engine) Match(event NormalizedEvent) []Rule {
	if e == nil || len(e.rules) == 0 {
		return nil
	}
	act := event.activation()

	type hit struct {
		rule Rule
		idx  int
	}
	var hits []hit
	for i, cr := range e.rules {
		out, _, err := cr.program.Eval(act)
		if err != nil {
			if isCostLimitExceeded(err) {
				slog.Warn("celtrigger: rule exceeded evaluation cost budget; treating as no-match", "rule", cr.rule.Name, "budget", maxEvalCost)
			}
			// Fail closed: a runtime error means "did not match".
			continue
		}
		b, ok := out.Value().(bool)
		if !ok || !b {
			continue
		}
		hits = append(hits, hit{rule: cr.rule, idx: i})
	}

	sort.SliceStable(hits, func(a, b int) bool {
		return hits[a].rule.Priority > hits[b].rule.Priority
	})

	matched := make([]Rule, 0, len(hits))
	for _, h := range hits {
		matched = append(matched, h.rule)
	}
	return matched
}

func isCostLimitExceeded(err error) bool {
	var cancelled interpreter.EvalCancelledError
	return errors.As(err, &cancelled) && cancelled.Cause == interpreter.CostLimitExceeded
}

// Len reports the number of compiled rules held by the engine.
func (e *Engine) Len() int {
	if e == nil {
		return 0
	}
	return len(e.rules)
}

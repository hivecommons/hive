package toolapprove

// Approval rules as data.
//
// Operators declare approval policy as CEL expressions in config rather than as
// code. This reuses the posture the fleet already settled on in pkg/celtrigger
// (RFC #4000: "we already have a declarative, fail-closed rule engine … approval
// rules should be the same engine and the same posture"):
//
//   - Rules compile at config load. A malformed expression is an error the
//     operator sees at load time, never a fleet crash at decision time.
//   - A runtime evaluation error is treated as NO-MATCH, never as a match. A
//     broken rule silently stops widening authority; it can never accidentally
//     grant it.
//   - Evaluation cost is bounded, so a pathological expression cannot stall the
//     decision point that sits in every agent turn.
//
// What a rule may NOT do is escape the hive's ACMM level: Desk.decide applies
// the ceiling after rule evaluation, unconditionally. A rule is an input to the
// decision, not an override of it.

import (
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
)

// maxRuleEvalCost bounds CEL runtime work per rule evaluation. The decision
// point runs in every agent turn, so the budget is deliberately tighter than
// celtrigger's event-time budget: approval rules are field comparisons and
// small label scans, not comprehensions over large collections. A rule that
// exceeds this fails closed as no-match and warns so the operator can see why
// it stopped firing.
const maxRuleEvalCost uint64 = 5_000

// activationVarRequest is the CEL variable name the request is exposed under.
const activationVarRequest = "request"

// RuleRequest is the flattened, CEL-visible view of an approval request. It is
// a separate type from Request on purpose: it is a stable, documented policy
// surface that operator rules are written against, so internal refactors of
// Request cannot silently break every deployed rule.
type RuleRequest struct {
	// Kind is the request lane, e.g. "agent-tool", "self-merge". event.kind analogue.
	Kind string `cel:"kind"`
	// Tool is the tool or action name being requested.
	Tool string `cel:"tool"`
	// Agent is the requesting agent's name.
	Agent string `cel:"agent"`
	// Repo is the target repository, "org/name".
	Repo string `cel:"repo"`
	// Number is the target issue/PR number, 0 when not applicable.
	Number int `cel:"number"`
	// Labels are the labels on the target.
	Labels []string `cel:"labels"`
	// Author is the login that authored the target.
	Author string `cel:"author"`
	// Title is the target's title.
	Title string `cel:"title"`
	// ChecksGreen reports whether required CI is green on the target.
	ChecksGreen bool `cel:"checks_green"`
	// ReadOnly reports whether the requested tool is a read-only inspection tool.
	ReadOnly bool `cel:"read_only"`
	// SideEffectful is the negation of ReadOnly, exposed for readable rules.
	SideEffectful bool `cel:"side_effectful"`
	// Command is the shell command, for bash-shaped requests ("" otherwise).
	Command string `cel:"command"`
	// FilePath is the file target, for write-shaped requests ("" otherwise).
	FilePath string `cel:"file_path"`
}

// ruleRequest flattens a Request into its CEL-visible form.
func ruleRequest(req Request) RuleRequest {
	labels := req.Labels
	if labels == nil {
		labels = []string{}
	}
	ro := req.Tool.IsReadOnly()
	return RuleRequest{
		Kind:          req.Kind,
		Tool:          req.Tool.Tool,
		Agent:         req.Agent.Name,
		Repo:          req.Repo,
		Number:        req.Number,
		Labels:        labels,
		Author:        req.Author,
		Title:         req.Title,
		ChecksGreen:   req.ChecksGreen,
		ReadOnly:      ro,
		SideEffectful: !ro,
		Command:       req.Tool.GetCommand(),
		FilePath:      req.Tool.GetFilePath(),
	}
}

// activation renders the request as a CEL activation keyed by "request".
func (r RuleRequest) activation() map[string]any {
	return map[string]any{activationVarRequest: r}
}

// compiledApprovalRule pairs a Rule with its compiled CEL program.
type compiledApprovalRule struct {
	rule    Rule
	program cel.Program
}

// RuleEngine holds the compiled approval rule set. Construct it with
// CompileRules. Safe for concurrent Match calls.
type RuleEngine struct {
	env    *cel.Env
	rules  []compiledApprovalRule
	logger *slog.Logger
}

// newRuleEnv builds the CEL environment exposing the request. Registering
// RuleRequest as a native type means an unknown field is a COMPILE error, so a
// typo in a rule is caught at config load rather than silently never matching.
func newRuleEnv() (*cel.Env, error) {
	return cel.NewEnv(
		// Register RuleRequest as a native type so field access is type-checked
		// at Compile time — a typo like `request.checks_gren` is a config-load
		// error, not a rule that silently never fires. cel:"..." tags name the
		// fields, exactly as celtrigger does for NormalizedEvent.
		ext.NativeTypes(reflect.TypeOf(RuleRequest{}), ext.ParseStructTags(true)),
		cel.Variable(activationVarRequest, cel.ObjectType("toolapprove.RuleRequest")),
		// hasLabel(request.labels, "dependabot") — reads better than the `in` form.
		cel.Function("hasLabel",
			cel.Overload("approval_hasLabel_list_string",
				[]*cel.Type{cel.ListType(cel.DynType), cel.StringType},
				cel.BoolType,
				cel.BinaryBinding(approvalHasLabel),
			),
		),
	)
}

// approvalHasLabel implements hasLabel(list, needle). Non-list / non-string
// inputs yield false rather than erroring, keeping evaluation fail-closed.
func approvalHasLabel(listVal, needleVal ref.Val) ref.Val {
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
		if s, isStr := it.Next().Value().(string); isStr && s == needle {
			return types.Bool(true)
		}
	}
	return types.Bool(false)
}

// CompileRules builds a RuleEngine from operator rules. It fails closed: any
// malformed expression, unknown action, or empty name returns an error and NO
// engine, so the operator rejects the config instead of running a fleet with a
// rule set that half-parsed. An empty rule list yields a valid engine that
// never matches — preserving pre-feature behavior exactly.
func CompileRules(rules []Rule, logger *slog.Logger) (*RuleEngine, error) {
	env, err := newRuleEnv()
	if err != nil {
		return nil, fmt.Errorf("build approval rule CEL environment: %w", err)
	}
	eng := &RuleEngine{env: env, logger: logger}
	if len(rules) == 0 {
		return eng, nil
	}

	seen := make(map[string]struct{}, len(rules))
	compiled := make([]compiledApprovalRule, 0, len(rules))
	for i, r := range rules {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			return nil, fmt.Errorf("approval rule #%d: name is required", i+1)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("approval rule %q: duplicate name", name)
		}
		seen[name] = struct{}{}

		if !r.Action.Valid() {
			return nil, fmt.Errorf("approval rule %q: unknown action %q (want auto-approve, security-scan, or operator-approve)", name, r.Action)
		}
		if strings.TrimSpace(r.Expr) == "" {
			return nil, fmt.Errorf("approval rule %q: expr is required", name)
		}

		ast, iss := env.Compile(r.Expr)
		if iss != nil && iss.Err() != nil {
			return nil, fmt.Errorf("approval rule %q: %w", name, iss.Err())
		}
		if ast.OutputType() != cel.BoolType {
			return nil, fmt.Errorf("approval rule %q: expression must evaluate to bool, got %s", name, ast.OutputType())
		}
		prg, err := env.Program(ast,
			cel.CostLimit(maxRuleEvalCost),
			cel.InterruptCheckFrequency(100),
		)
		if err != nil {
			return nil, fmt.Errorf("approval rule %q: build program: %w", name, err)
		}
		r.Name = name
		compiled = append(compiled, compiledApprovalRule{rule: r, program: prg})
	}

	// Higher priority first; declaration order breaks ties so a config file
	// reads top-to-bottom as written.
	sort.SliceStable(compiled, func(i, j int) bool {
		return compiled[i].rule.Priority > compiled[j].rule.Priority
	})
	eng.rules = compiled
	return eng, nil
}

// Match returns the first (highest-priority) rule whose expression matches the
// request, or ok=false when none do.
//
// Fail-closed on every error path: a rule whose MinACMMLevel excludes this hive
// is skipped, an evaluation error is a no-match, a non-bool result is a
// no-match, and a cost-budget interrupt is a no-match with a warning. None of
// these can produce a match, so a broken rule never grants authority.
func (e *RuleEngine) Match(req Request, acmmLevel int) (RuleMatch, bool) {
	if e == nil || len(e.rules) == 0 {
		return RuleMatch{}, false
	}
	act := ruleRequest(req).activation()

	for _, cr := range e.rules {
		if cr.rule.MinACMMLevel > 0 && acmmLevel < cr.rule.MinACMMLevel {
			continue
		}
		out, _, err := cr.program.Eval(act)
		if err != nil {
			e.warn("approval rule evaluation failed — treating as no-match",
				"rule", cr.rule.Name, "error", err)
			continue
		}
		b, ok := out.Value().(bool)
		if !ok {
			e.warn("approval rule did not evaluate to bool — treating as no-match",
				"rule", cr.rule.Name)
			continue
		}
		if b {
			return RuleMatch{Name: cr.rule.Name, Action: cr.rule.Action}, true
		}
	}
	return RuleMatch{}, false
}

// Names returns the configured rule names in evaluation order.
func (e *RuleEngine) Names() []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.rules))
	for _, cr := range e.rules {
		out = append(out, cr.rule.Name)
	}
	return out
}

// Len reports how many rules are compiled.
func (e *RuleEngine) Len() int {
	if e == nil {
		return 0
	}
	return len(e.rules)
}

func (e *RuleEngine) warn(msg string, args ...any) {
	if e == nil || e.logger == nil {
		return
	}
	e.logger.Warn(msg, args...)
}

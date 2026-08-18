package hooks

import (
	"errors"
	"fmt"
	"log/slog"
	"reflect"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/interpreter"
)

// This file implements the optional `when:` predicate on a hook, reusing the
// CEL engine and — more importantly — the FAIL-CLOSED POSTURE that
// pkg/celtrigger established:
//
//   - A malformed or non-boolean expression is rejected at COMPILE time, so a
//     bad rule never reaches the fleet.
//   - A runtime evaluation error, including exceeding the cost budget, is
//     treated as NO-MATCH rather than propagating or defaulting to fire.
//     For a hook that means "did not fire", which is the safe direction: a
//     predicate hive cannot evaluate must not be able to trigger a pause.
//   - Evaluation cost is bounded, so a pathological expression cannot stall
//     the post-commit dispatch path.
//
// The predicate operates over the transition Payload rather than
// celtrigger.NormalizedEvent, so it needs its own environment; the two engines
// are intentionally separate because their activations are different shapes.

// maxPredicateCost bounds CEL runtime work per predicate evaluation. Matches
// pkg/celtrigger's budget: high enough for realistic operator predicates
// combining field and map checks, low enough to stop pathological expressions
// on the post-commit path.
const maxPredicateCost uint64 = 10_000

// activationVarTransition is the CEL variable name the payload is bound to.
// `t` rather than `transition` because predicates are written inline in YAML
// and read better short: `t.agent == "reviewer" && t.model.startsWith("opus")`.
const activationVarTransition = "t"

// celPayload is the CEL-facing projection of a Payload. It exists separately
// from Payload because CEL native-type registration keys field names off
// struct tags, and Payload's json tags carry `omitempty` suffixes that would
// leak into expression field names. Keeping a dedicated `cel:` tagged view
// also means the expression surface is an explicit, reviewable contract rather
// than whatever Payload happens to expose.
type celPayload struct {
	Transition string            `cel:"transition"`
	From       string            `cel:"from"`
	To         string            `cel:"to"`
	Agent      string            `cel:"agent"`
	Repo       string            `cel:"repo"`
	Actor      string            `cel:"actor"`
	Trigger    string            `cel:"trigger"`
	Reason     string            `cel:"reason"`
	Model      string            `cel:"model"`
	Backend    string            `cel:"backend"`
	Pin        string            `cel:"pin"`
	ACMMLevel  int               `cel:"acmm_level"`
	Attrs      map[string]string `cel:"attrs"`
}

// forCEL projects a Payload into its CEL view. Attrs is never nil in the
// projection so an expression like `t.attrs["pr"] != ""` evaluates cleanly
// against a transition that set no attrs, instead of erroring on a null map.
func (p Payload) forCEL() celPayload {
	attrs := p.Attrs
	if attrs == nil {
		attrs = map[string]string{}
	}
	return celPayload{
		Transition: string(p.Transition),
		From:       p.From,
		To:         p.To,
		Agent:      p.Agent,
		Repo:       p.Repo,
		Actor:      p.Actor,
		Trigger:    p.Trigger,
		Reason:     p.Reason,
		Model:      p.Model,
		Backend:    p.Backend,
		Pin:        p.Pin,
		ACMMLevel:  p.ACMMLevel,
		Attrs:      attrs,
	}
}

// predicate is a compiled `when:` expression.
type predicate struct {
	expr    string
	program cel.Program
}

// predicateEnv builds the CEL environment exposing the transition payload.
// Registering celPayload as a native type means an expression referencing an
// unknown field is a COMPILE error — an operator's typo (`t.agnet`) is caught
// at config load rather than becoming a predicate that silently never matches.
//
// It also registers attr(), because CEL's native map index raises "no such
// key" for an absent key rather than yielding the zero value. Under the
// fail-closed rule a raised error means no-match, so the natural-looking
// `t.attrs["pr"] != ""` would SILENTLY NEVER FIRE for exactly the transitions
// that omit that attr — the same "hook you believe is armed" trap the closed
// vocabularies exist to prevent. attr() gives operators a total accessor.
func predicateEnv() (*cel.Env, error) {
	return cel.NewEnv(
		ext.NativeTypes(reflect.TypeOf(celPayload{}), ext.ParseStructTags(true)),
		cel.Variable(activationVarTransition, cel.ObjectType("hooks.celPayload")),
		// attr(t.attrs, "pr") — "" when the key is absent, never an error.
		cel.Function("attr",
			cel.Overload("attr_map_string",
				[]*cel.Type{cel.MapType(cel.StringType, cel.StringType), cel.StringType},
				cel.StringType,
				cel.BinaryBinding(attrImpl),
			),
		),
	)
}

// attrImpl implements attr(map, key): the value, or "" when absent. Non-map or
// non-string inputs yield "" rather than erroring, keeping evaluation total.
func attrImpl(mapVal, keyVal ref.Val) ref.Val {
	key, ok := keyVal.Value().(string)
	if !ok {
		return types.String("")
	}
	mapper, ok := mapVal.(traits.Mapper)
	if !ok {
		return types.String("")
	}
	found, ok := mapper.Find(types.String(key))
	if !ok || found == nil {
		return types.String("")
	}
	s, ok := found.Value().(string)
	if !ok {
		return types.String("")
	}
	return types.String(s)
}

// compilePredicate compiles a `when:` expression, rejecting anything that does
// not parse, type-check, or yield a bool.
func compilePredicate(expr string) (*predicate, error) {
	env, err := predicateEnv()
	if err != nil {
		return nil, fmt.Errorf("build env: %w", err)
	}
	ast, iss := env.Compile(expr)
	if iss != nil && iss.Err() != nil {
		return nil, fmt.Errorf("compile: %w", iss.Err())
	}
	if ast.OutputType() != cel.BoolType {
		return nil, fmt.Errorf("expression must return bool, got %s", ast.OutputType())
	}
	prg, err := env.Program(ast, cel.CostLimit(maxPredicateCost), cel.InterruptCheckFrequency(100))
	if err != nil {
		return nil, fmt.Errorf("program: %w", err)
	}
	return &predicate{expr: expr, program: prg}, nil
}

// matches evaluates the predicate against a payload. A nil predicate (no
// `when:`) always matches. Any runtime error is fail-closed no-match, logged
// so an operator can diagnose a predicate that stopped firing rather than
// having it disappear silently.
func (p *predicate) matches(payload Payload, logger *slog.Logger) bool {
	if p == nil {
		return true
	}
	out, _, err := p.program.Eval(map[string]any{activationVarTransition: payload.forCEL()})
	if err != nil {
		if logger != nil {
			msg := "hooks: when-predicate evaluation failed; treating as no-match"
			if isPredicateCostExceeded(err) {
				msg = "hooks: when-predicate exceeded evaluation cost budget; treating as no-match"
			}
			logger.Warn(msg, "expr", p.expr, "error", err)
		}
		return false
	}
	b, ok := out.Value().(bool)
	return ok && b
}

// isPredicateCostExceeded reports whether an eval error was the cost limit,
// which is worth distinguishing in logs from a genuine expression error.
func isPredicateCostExceeded(err error) bool {
	var cancelled interpreter.EvalCancelledError
	return errors.As(err, &cancelled) && cancelled.Cause == interpreter.CostLimitExceeded
}

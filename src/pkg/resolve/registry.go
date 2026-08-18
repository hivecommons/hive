package resolve

import (
	"context"
	"log/slog"
	"sync"
)

// factories holds the registered resolver Factory per type name. Populated by
// each built-in's init() and by any RegisterResolver call. Guarded because
// registration can happen from init() across packages.
var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// RegisterResolver registers a Factory under a type name (e.g. "env", "script").
// This is the extensibility seam: a new resolver type is one file with an
// init() that calls RegisterResolver — no changes to the call sites or the
// Registry. Re-registering a type overwrites the prior factory.
func RegisterResolver(typ string, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[typ] = f
}

func lookupFactory(typ string) (Factory, bool) {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	f, ok := factories[typ]
	return f, ok
}

// defKind describes how a compiled operator variable is dispatched.
type compiledDef struct {
	spec     VarSpec
	scope    Scope
	resolver Resolver
}

// Registry is an immutable, built-from-config set of operator variable
// definitions plus a policy. It is safe for concurrent use by many goroutines
// (e.g. parallel kicks) — Build happens once, Expand is read-only.
type Registry struct {
	defs   map[string]compiledDef // by variable name
	policy Policy
	logger *slog.Logger
}

// Build compiles a Registry from operator VarSpecs and a Policy. Specs whose
// type is unknown, or whose factory refuses construction (e.g. script/http when
// the policy disallows it), are logged and skipped — they will resolve to their
// literal ${VAR}, never crash the load. A nil logger discards diagnostics.
func Build(specs []VarSpec, pol Policy, logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	pol = pol.withDefaults()
	r := &Registry{defs: map[string]compiledDef{}, policy: pol, logger: logger}
	for _, spec := range specs {
		typ := spec.Type
		if typ == "" {
			if spec.Value != "" {
				typ = "static"
			} else {
				typ = "env"
			}
		}
		factory, ok := lookupFactory(typ)
		if !ok {
			logger.Warn("unknown variable resolver type, leaving literal", "var", spec.Name, "type", typ)
			continue
		}
		res, err := factory(spec, pol)
		if err != nil {
			logger.Warn("variable resolver disabled, leaving literal", "var", spec.Name, "type", typ, "error", err)
			continue
		}
		r.defs[spec.Name] = compiledDef{spec: spec, scope: spec.Scope, resolver: res}
	}
	return r
}

// EnvOnly returns a Registry with no operator defs and a zero policy. Its Expand
// reproduces the legacy behavior exactly: ${NAME} → os.LookupEnv(NAME), unset
// left literal. Used when no `variables:` block is configured, guaranteeing a
// byte-identical config load.
func EnvOnly() *Registry {
	return &Registry{defs: map[string]compiledDef{}, policy: Policy{}.withDefaults(), logger: slog.New(slog.NewTextHandler(discard{}, nil))}
}

// Expand replaces every ${VAR} in s using the resolution chain for the given
// scope. rt may be nil (config scope). Resolution order per match:
//  1. a runtime built-in producer (rt.Vars[name]) — always wins, non-overridable;
//  2. an operator VarSpec whose scope matches `want`;
//  3. the bare environment variable of the same name;
//  4. otherwise the literal ${VAR} is left in place.
//
// A runtime built-in producer is memoized within a single Expand call so it
// runs at most once even if the template references it multiple times.
func (r *Registry) Expand(ctx context.Context, s string, want Scope, rt *RuntimeContext) string {
	if ctx == nil {
		ctx = context.Background()
	}
	memo := map[string]string{}
	return VarPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1] // strip ${ and }
		if v, ok := memo[name]; ok {
			return v
		}
		out := r.resolveOne(ctx, name, match, want, rt)
		memo[name] = out
		return out
	})
}

func (r *Registry) resolveOne(ctx context.Context, name, raw string, want Scope, rt *RuntimeContext) string {
	req := Request{Name: name, Raw: raw, Ctx: ctx, Runtime: rt}

	// 1. Runtime built-ins win and are non-overridable — this preserves the
	//    exact per-kick output of the legacy strings.NewReplacer.
	if fn, ok := rt.lookup(name); ok {
		return fn()
	}

	// 2. Operator-defined variable, if in scope.
	if def, ok := r.defs[name]; ok && def.scope.matches(want) {
		res, err := def.resolver.Resolve(req)
		if err == nil {
			return res.Value
		}
		if err != ErrUnresolved {
			r.logger.Warn("variable resolver error, leaving literal", "var", name, "error", err)
		}
		return raw
	}

	// 3. Bare environment variable of the same name — but only in config scope.
	//    Config-load substitution (System A) has always fallen back to
	//    os.LookupEnv for any ${NAME}; per-kick templating (System B) has not
	//    (its legacy strings.NewReplacer left unknown ${VAR} literal, never
	//    consulting the environment). Restricting the fallback to config scope
	//    keeps both systems byte-identical to their legacy behavior.
	if want == ScopeConfig {
		res, err := bareEnvResolver{}.Resolve(req)
		if err == nil {
			return res.Value
		}
	}

	// 4. Unresolved: leave the literal ${VAR}.
	return raw
}

// discard is an io.Writer that drops everything (for the nil-logger case).
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

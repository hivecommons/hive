package resolve

import (
	"context"
	"os"
	"testing"
)

// TestEnvOnly_LegacyParity verifies the env-only registry reproduces the exact
// legacy config.expandEnvVars behavior: ${SET} -> value, ${UNSET} -> literal,
// ${EMPTY} -> "" (set-but-empty resolves to empty, not literal).
func TestEnvOnly_LegacyParity(t *testing.T) {
	t.Setenv("HIVE_TEST_SET", "value1")
	t.Setenv("HIVE_TEST_EMPTY", "")
	os.Unsetenv("HIVE_TEST_UNSET")

	reg := EnvOnly()
	cases := []struct{ in, want string }{
		{"${HIVE_TEST_SET}", "value1"},
		{"${HIVE_TEST_UNSET}", "${HIVE_TEST_UNSET}"}, // literal passthrough
		{"${HIVE_TEST_EMPTY}", ""},                   // set-but-empty -> empty
		{"a ${HIVE_TEST_SET} b ${HIVE_TEST_UNSET} c", "a value1 b ${HIVE_TEST_UNSET} c"},
		{"no vars here", "no vars here"},
		{"${A}${HIVE_TEST_SET}", "${A}value1"},
	}
	for _, c := range cases {
		got := reg.Expand(context.Background(), c.in, ScopeConfig, nil)
		if got != c.want {
			t.Errorf("Expand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStaticAndEnvDefaults covers operator-defined static vars and env vars with
// defaults (the ${VAR:-x} equivalent hive never had).
func TestStaticAndEnvDefaults(t *testing.T) {
	os.Unsetenv("HIVE_TEST_REGION")
	dflt := "us-east-1"
	specs := []VarSpec{
		{Name: "DEPLOY", Type: "static", Value: "production", Scope: ScopeBoth},
		{Name: "REGION", Type: "env", Env: "HIVE_TEST_REGION", HasDefault: true, Default: dflt, Scope: ScopeConfig},
	}
	reg := Build(specs, Policy{}, nil)

	if got := reg.Expand(context.Background(), "${DEPLOY}", ScopeConfig, nil); got != "production" {
		t.Errorf("static config-scope: got %q", got)
	}
	if got := reg.Expand(context.Background(), "${DEPLOY}", ScopeTemplate, nil); got != "production" {
		t.Errorf("static both-scope in template: got %q", got)
	}
	if got := reg.Expand(context.Background(), "${REGION}", ScopeConfig, nil); got != dflt {
		t.Errorf("env default: got %q want %q", got, dflt)
	}
	// REGION is config-scoped only; in template scope it should fall through to
	// bare-env (unset) -> literal.
	if got := reg.Expand(context.Background(), "${REGION}", ScopeTemplate, nil); got != "${REGION}" {
		t.Errorf("out-of-scope var should be literal: got %q", got)
	}
	// Env default kicks in only when unset; a set value wins.
	t.Setenv("HIVE_TEST_REGION", "eu-west-1")
	reg2 := Build(specs, Policy{}, nil)
	if got := reg2.Expand(context.Background(), "${REGION}", ScopeConfig, nil); got != "eu-west-1" {
		t.Errorf("set env should override default: got %q", got)
	}
}

// TestRuntimeBuiltinsWin ensures a runtime built-in producer wins over an
// operator def and over env, and is memoized (runs once).
func TestRuntimeBuiltinsWin(t *testing.T) {
	t.Setenv("AGENT_NAME", "from-env")
	calls := 0
	rt := &RuntimeContext{Vars: map[string]func() string{
		"AGENT_NAME": func() string { calls++; return "scanner" },
	}}
	// Even with an operator def of the same name, the built-in wins.
	specs := []VarSpec{{Name: "AGENT_NAME", Type: "static", Value: "override", Scope: ScopeBoth}}
	reg := Build(specs, Policy{}, nil)

	got := reg.Expand(context.Background(), "${AGENT_NAME}/${AGENT_NAME}", ScopeTemplate, rt)
	if got != "scanner/scanner" {
		t.Errorf("built-in should win: got %q", got)
	}
	if calls != 1 {
		t.Errorf("producer should be memoized to 1 call, ran %d", calls)
	}
}

// TestRegistrationSeam proves a new resolver type registered via
// RegisterResolver resolves without touching Expand or the call sites.
func TestRegistrationSeam(t *testing.T) {
	RegisterResolver("test-upper", func(spec VarSpec, _ Policy) (Resolver, error) {
		return resolverFunc(func(req Request) (Result, error) {
			return Result{Value: "UP:" + spec.Value, Source: "test"}, nil
		}), nil
	})
	reg := Build([]VarSpec{{Name: "X", Type: "test-upper", Value: "hi", Scope: ScopeConfig}}, Policy{}, nil)
	if got := reg.Expand(context.Background(), "${X}", ScopeConfig, nil); got != "UP:hi" {
		t.Errorf("custom resolver: got %q", got)
	}
}

// TestUnknownTypeLeavesLiteral: a def with an unregistered type is skipped and
// its ${VAR} left literal, never crashing the build.
func TestUnknownTypeLeavesLiteral(t *testing.T) {
	os.Unsetenv("NOPE")
	reg := Build([]VarSpec{{Name: "NOPE", Type: "does-not-exist", Scope: ScopeConfig}}, Policy{}, nil)
	if got := reg.Expand(context.Background(), "${NOPE}", ScopeConfig, nil); got != "${NOPE}" {
		t.Errorf("unknown type should leave literal: got %q", got)
	}
}

type resolverFunc func(Request) (Result, error)

func (f resolverFunc) Resolve(r Request) (Result, error) { return f(r) }

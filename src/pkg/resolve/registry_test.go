package resolve

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// --- Scope.matches tests ---

func TestScope_Matches_EmptyDefaultsToTemplate(t *testing.T) {
	// An empty scope is treated as ScopeTemplate.
	s := Scope("")
	if !s.matches(ScopeTemplate) {
		t.Error("empty scope should match ScopeTemplate")
	}
	if s.matches(ScopeConfig) {
		t.Error("empty scope should not match ScopeConfig")
	}
}

func TestScope_Matches_Template(t *testing.T) {
	if !ScopeTemplate.matches(ScopeTemplate) {
		t.Error("ScopeTemplate should match ScopeTemplate")
	}
	if ScopeTemplate.matches(ScopeConfig) {
		t.Error("ScopeTemplate should not match ScopeConfig")
	}
}

func TestScope_Matches_Config(t *testing.T) {
	if !ScopeConfig.matches(ScopeConfig) {
		t.Error("ScopeConfig should match ScopeConfig")
	}
	if ScopeConfig.matches(ScopeTemplate) {
		t.Error("ScopeConfig should not match ScopeTemplate")
	}
}

func TestScope_Matches_Both(t *testing.T) {
	if !ScopeBoth.matches(ScopeTemplate) {
		t.Error("ScopeBoth should match ScopeTemplate")
	}
	if !ScopeBoth.matches(ScopeConfig) {
		t.Error("ScopeBoth should match ScopeConfig")
	}
}

func TestScope_Matches_UnknownScope(t *testing.T) {
	s := Scope("bogus")
	if s.matches(ScopeTemplate) {
		t.Error("unknown scope should not match ScopeTemplate")
	}
	if s.matches(ScopeConfig) {
		t.Error("unknown scope should not match ScopeConfig")
	}
}

// --- Policy.withDefaults tests ---

func TestPolicy_WithDefaults_ZeroValues(t *testing.T) {
	p := Policy{}
	got := p.withDefaults()
	if got.ExecTimeoutS != DefaultExecTimeoutS {
		t.Errorf("ExecTimeoutS = %d, want %d", got.ExecTimeoutS, DefaultExecTimeoutS)
	}
	if got.HTTPTimeoutS != DefaultHTTPTimeoutS {
		t.Errorf("HTTPTimeoutS = %d, want %d", got.HTTPTimeoutS, DefaultHTTPTimeoutS)
	}
}

func TestPolicy_WithDefaults_NegativeValues(t *testing.T) {
	p := Policy{ExecTimeoutS: -1, HTTPTimeoutS: -3}
	got := p.withDefaults()
	if got.ExecTimeoutS != DefaultExecTimeoutS {
		t.Errorf("negative ExecTimeoutS should default, got %d", got.ExecTimeoutS)
	}
	if got.HTTPTimeoutS != DefaultHTTPTimeoutS {
		t.Errorf("negative HTTPTimeoutS should default, got %d", got.HTTPTimeoutS)
	}
}

func TestPolicy_WithDefaults_PreservesExplicit(t *testing.T) {
	p := Policy{ExecTimeoutS: 10, HTTPTimeoutS: 20, AllowExec: true}
	got := p.withDefaults()
	if got.ExecTimeoutS != 10 {
		t.Errorf("should preserve explicit ExecTimeoutS=10, got %d", got.ExecTimeoutS)
	}
	if got.HTTPTimeoutS != 20 {
		t.Errorf("should preserve explicit HTTPTimeoutS=20, got %d", got.HTTPTimeoutS)
	}
	if !got.AllowExec {
		t.Error("should preserve AllowExec=true")
	}
}

// --- RuntimeContext.lookup tests ---

func TestRuntimeContext_Nil(t *testing.T) {
	var rc *RuntimeContext
	_, ok := rc.lookup("anything")
	if ok {
		t.Error("nil RuntimeContext should return false")
	}
}

func TestRuntimeContext_NilVars(t *testing.T) {
	rc := &RuntimeContext{Vars: nil}
	_, ok := rc.lookup("anything")
	if ok {
		t.Error("nil Vars map should return false")
	}
}

func TestRuntimeContext_EmptyVars(t *testing.T) {
	rc := &RuntimeContext{Vars: map[string]func() string{}}
	_, ok := rc.lookup("missing")
	if ok {
		t.Error("empty Vars map should return false for missing key")
	}
}

func TestRuntimeContext_Hit(t *testing.T) {
	rc := &RuntimeContext{Vars: map[string]func() string{
		"AGENT": func() string { return "quality" },
	}}
	fn, ok := rc.lookup("AGENT")
	if !ok {
		t.Fatal("should find AGENT in Vars")
	}
	if fn() != "quality" {
		t.Errorf("producer returned %q, want quality", fn())
	}
}

// --- Registry.Build tests ---

func TestBuild_UnknownType_Skipped(t *testing.T) {
	specs := []VarSpec{{Name: "V", Type: "nonexistent", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	// Unknown type is skipped — variable left literal. With no env var V
	// set, the bare env fallback also fails → literal survives.
	os.Unsetenv("V")
	got := reg.Expand(context.Background(), "${V}", ScopeConfig, nil)
	if got != "${V}" {
		t.Errorf("unknown type should leave literal, got %q", got)
	}
}

func TestBuild_FactoryError_Skipped(t *testing.T) {
	// Register a factory that always errors.
	RegisterResolver("always-fail", func(spec VarSpec, pol Policy) (Resolver, error) {
		return nil, errors.New("refused")
	})
	defer func() {
		factoriesMu.Lock()
		delete(factories, "always-fail")
		factoriesMu.Unlock()
	}()

	specs := []VarSpec{{Name: "X", Type: "always-fail", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	os.Unsetenv("X")
	got := reg.Expand(context.Background(), "${X}", ScopeConfig, nil)
	if got != "${X}" {
		t.Errorf("factory error should leave literal, got %q", got)
	}
}

func TestBuild_TypeInference_Static(t *testing.T) {
	// Empty Type with Value set → inferred "static".
	specs := []VarSpec{{Name: "APP", Value: "hive", Scope: ScopeBoth}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), "${APP}", ScopeTemplate, nil)
	if got != "hive" {
		t.Errorf("inferred static should resolve, got %q", got)
	}
}

func TestBuild_TypeInference_Env(t *testing.T) {
	// Empty Type, no Value → inferred "env".
	t.Setenv("HIVE_INFER_TEST", "from-env")
	specs := []VarSpec{{Name: "HIVE_INFER_TEST", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), "${HIVE_INFER_TEST}", ScopeConfig, nil)
	if got != "from-env" {
		t.Errorf("inferred env should resolve, got %q", got)
	}
}

// --- Expand resolution priority tests ---

func TestExpand_RuntimeBuiltinWins(t *testing.T) {
	// Even if a VarSpec exists, RuntimeContext wins.
	t.Setenv("PRIORITY_TEST", "env-value")
	specs := []VarSpec{{Name: "PRIORITY_TEST", Type: "static", Value: "static-val", Scope: ScopeBoth}}
	reg := Build(specs, Policy{}, nil)
	rt := &RuntimeContext{Vars: map[string]func() string{
		"PRIORITY_TEST": func() string { return "runtime-wins" },
	}}
	got := reg.Expand(context.Background(), "${PRIORITY_TEST}", ScopeTemplate, rt)
	if got != "runtime-wins" {
		t.Errorf("runtime built-in should win, got %q", got)
	}
}

func TestExpand_OperatorDefBeatsEnvFallback(t *testing.T) {
	// Operator static def should win over bare env fallback in config scope.
	t.Setenv("REGION", "env-region")
	specs := []VarSpec{{Name: "REGION", Type: "static", Value: "us-west-2", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), "${REGION}", ScopeConfig, nil)
	if got != "us-west-2" {
		t.Errorf("operator def should beat env fallback, got %q", got)
	}
}

func TestExpand_BareEnvFallback_ConfigScope(t *testing.T) {
	// In config scope, unknown ${VAR} falls back to os env.
	t.Setenv("HIVE_BARE_FALLBACK", "from-os")
	reg := Build(nil, Policy{}, nil) // no specs
	got := reg.Expand(context.Background(), "${HIVE_BARE_FALLBACK}", ScopeConfig, nil)
	if got != "from-os" {
		t.Errorf("config scope should fall back to bare env, got %q", got)
	}
}

func TestExpand_NoBareEnvFallback_TemplateScope(t *testing.T) {
	// In template scope, unknown ${VAR} does NOT fall back to os env — left literal.
	t.Setenv("HIVE_NO_FALLBACK", "should-not-appear")
	reg := Build(nil, Policy{}, nil)
	got := reg.Expand(context.Background(), "${HIVE_NO_FALLBACK}", ScopeTemplate, nil)
	if got != "${HIVE_NO_FALLBACK}" {
		t.Errorf("template scope should leave literal, got %q", got)
	}
}

func TestExpand_ScopeMismatch_LeftLiteral(t *testing.T) {
	// A var defined for config scope should be left literal when asked in template scope.
	specs := []VarSpec{{Name: "ONLY_CONFIG", Type: "static", Value: "secret", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), "${ONLY_CONFIG}", ScopeTemplate, nil)
	if got != "${ONLY_CONFIG}" {
		t.Errorf("scope mismatch should leave literal, got %q", got)
	}
}

// --- Expand memoization test ---

func TestExpand_Memoization(t *testing.T) {
	callCount := 0
	rt := &RuntimeContext{Vars: map[string]func() string{
		"EXPENSIVE": func() string {
			callCount++
			return "result"
		},
	}}
	reg := EnvOnly()
	// Reference the same variable twice in one Expand call.
	got := reg.Expand(context.Background(), "${EXPENSIVE} and ${EXPENSIVE}", ScopeTemplate, rt)
	if got != "result and result" {
		t.Errorf("unexpected expansion: %q", got)
	}
	if callCount != 1 {
		t.Errorf("producer called %d times, want 1 (memoized)", callCount)
	}
}

// --- Expand nil context safety ---

func TestExpand_NilContext_BareEnvFallback(t *testing.T) {
	t.Setenv("HIVE_NILCTX", "works")
	reg := EnvOnly()
	// nil ctx should not panic — Expand documents and handles ctx == nil by
	// falling back to context.Background(); this test exists specifically to
	// exercise that fallback, so passing nil here is intentional.
	got := reg.Expand(nil, "${HIVE_NILCTX}", ScopeConfig, nil) //nolint:staticcheck // SA1012: nil ctx is the case under test — see Expand's documented nil-safety fallback.
	if got != "works" {
		t.Errorf("nil ctx should default to background, got %q", got)
	}
}

// --- Expand with resolver error (non-ErrUnresolved) ---

func TestExpand_ResolverHardError_LeftLiteral(t *testing.T) {
	RegisterResolver("flaky", func(spec VarSpec, pol Policy) (Resolver, error) {
		return &flakyResolver{}, nil
	})
	defer func() {
		factoriesMu.Lock()
		delete(factories, "flaky")
		factoriesMu.Unlock()
	}()

	specs := []VarSpec{{Name: "FLAKY", Type: "flaky", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	os.Unsetenv("FLAKY")
	got := reg.Expand(context.Background(), "${FLAKY}", ScopeConfig, nil)
	if got != "${FLAKY}" {
		t.Errorf("hard resolver error should leave literal, got %q", got)
	}
}

type flakyResolver struct{}

func (*flakyResolver) Resolve(_ Request) (Result, error) {
	return Result{}, errors.New("network timeout")
}

// --- RegisterResolver / lookupFactory ---

func TestRegisterResolver_Overwrites(t *testing.T) {
	RegisterResolver("test-overwrite", func(spec VarSpec, pol Policy) (Resolver, error) {
		return &staticResolver{value: "first"}, nil
	})
	RegisterResolver("test-overwrite", func(spec VarSpec, pol Policy) (Resolver, error) {
		return &staticResolver{value: "second"}, nil
	})
	defer func() {
		factoriesMu.Lock()
		delete(factories, "test-overwrite")
		factoriesMu.Unlock()
	}()

	specs := []VarSpec{{Name: "OW", Type: "test-overwrite", Scope: ScopeBoth}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), "${OW}", ScopeTemplate, nil)
	if got != "second" {
		t.Errorf("re-registration should overwrite, got %q", got)
	}
}

// --- EnvOnly tests ---

func TestEnvOnly_NoVars(t *testing.T) {
	reg := EnvOnly()
	got := reg.Expand(context.Background(), "no vars here", ScopeConfig, nil)
	if got != "no vars here" {
		t.Errorf("text without vars should pass through unchanged, got %q", got)
	}
}

func TestEnvOnly_MultipleVars(t *testing.T) {
	t.Setenv("A_TEST_VAR", "alpha")
	t.Setenv("B_TEST_VAR", "beta")
	reg := EnvOnly()
	got := reg.Expand(context.Background(), "${A_TEST_VAR}-${B_TEST_VAR}", ScopeConfig, nil)
	if got != "alpha-beta" {
		t.Errorf("got %q, want alpha-beta", got)
	}
}

// --- VarPattern ---

func TestVarPattern_Matches(t *testing.T) {
	cases := []struct {
		input string
		names []string
	}{
		{"${FOO}", []string{"FOO"}},
		{"a${X}b${Y}c", []string{"X", "Y"}},
		{"${UNDER_SCORE_123}", []string{"UNDER_SCORE_123"}},
		{"no vars", nil},
		{"$NOT_A_VAR", nil},
		{"${}", nil}, // empty name — regex requires [^}]+
	}
	for _, c := range cases {
		matches := VarPattern.FindAllStringSubmatch(c.input, -1)
		var names []string
		for _, m := range matches {
			names = append(names, m[1])
		}
		if len(names) == 0 {
			names = nil
		}
		if !strSliceEqual(names, c.names) {
			t.Errorf("VarPattern(%q) names = %v, want %v", c.input, names, c.names)
		}
	}
}

// --- Expand with multiple resolvers via chaining ---

func TestExpand_MultipleSpecs(t *testing.T) {
	t.Setenv("HIVE_HOST", "prod.example.com")
	specs := []VarSpec{
		{Name: "APP_NAME", Type: "static", Value: "hive-dashboard", Scope: ScopeBoth},
		{Name: "HIVE_HOST", Type: "env", Env: "HIVE_HOST", Scope: ScopeConfig},
	}
	reg := Build(specs, Policy{}, nil)
	input := "https://${HIVE_HOST}/${APP_NAME}"
	got := reg.Expand(context.Background(), input, ScopeConfig, nil)
	if got != "https://prod.example.com/hive-dashboard" {
		t.Errorf("multi-spec expansion got %q", got)
	}
}

// --- Edge case: env var with default ---

func TestExpand_EnvWithDefault_Unset(t *testing.T) {
	os.Unsetenv("HIVE_OPTIONAL_PORT")
	specs := []VarSpec{{Name: "PORT", Type: "env", Env: "HIVE_OPTIONAL_PORT", HasDefault: true, Default: "8080", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), ":${PORT}", ScopeConfig, nil)
	if got != ":8080" {
		t.Errorf("env with default (unset) got %q, want :8080", got)
	}
}

func TestExpand_EnvWithDefault_Set(t *testing.T) {
	t.Setenv("HIVE_OPTIONAL_PORT", "9090")
	specs := []VarSpec{{Name: "PORT", Type: "env", Env: "HIVE_OPTIONAL_PORT", HasDefault: true, Default: "8080", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), ":${PORT}", ScopeConfig, nil)
	if got != ":9090" {
		t.Errorf("env with default (set) got %q, want :9090", got)
	}
}

func TestExpand_EnvWithEmptyDefault(t *testing.T) {
	os.Unsetenv("HIVE_EMPTY_DEFAULT")
	specs := []VarSpec{{Name: "ED", Type: "env", Env: "HIVE_EMPTY_DEFAULT", HasDefault: true, Default: "", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), "pre${ED}post", ScopeConfig, nil)
	if got != "prepost" {
		t.Errorf("empty default should resolve to empty string, got %q", got)
	}
}

// --- Edge case: resolveOne ErrUnresolved from operator def falls through to bare env ---

func TestExpand_OperatorUnresolved_FallsToEnv(t *testing.T) {
	// Operator env resolver with no default and env unset → ErrUnresolved
	// → bare env fallback (same variable name, different env var set).
	// Actually, bare env uses the same name, so both paths see the same env.
	// Test that when both operator resolver and bare env are unset, literal is preserved.
	os.Unsetenv("HIVE_DOUBLE_MISS")
	specs := []VarSpec{{Name: "HIVE_DOUBLE_MISS", Type: "env", Env: "HIVE_DOUBLE_MISS", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	got := reg.Expand(context.Background(), "${HIVE_DOUBLE_MISS}", ScopeConfig, nil)
	if got != "${HIVE_DOUBLE_MISS}" {
		t.Errorf("double miss should leave literal, got %q", got)
	}
}

// --- helpers ---

func strSliceEqual(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Expand with large input (performance sanity) ---

func TestExpand_LargeInput_NoVars(t *testing.T) {
	reg := EnvOnly()
	large := strings.Repeat("no vars here ", 1000)
	got := reg.Expand(context.Background(), large, ScopeConfig, nil)
	if got != large {
		t.Error("large input without vars should pass through unchanged")
	}
}

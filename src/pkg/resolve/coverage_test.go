package resolve

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TestEnvResolver_NameFallback covers the branch where spec.Env is empty and
// the resolver falls back to req.Name (the substituted variable's own name).
func TestEnvResolver_NameFallback(t *testing.T) {
	t.Setenv("HIVE_TEST_NAMEFALLBACK", "via-name")
	specs := []VarSpec{{Name: "HIVE_TEST_NAMEFALLBACK", Type: "env", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	if got := reg.Expand(context.Background(), "${HIVE_TEST_NAMEFALLBACK}", ScopeConfig, nil); got != "via-name" {
		t.Errorf("got %q want via-name", got)
	}
}

// TestEnvResolver_UnsetNoDefault covers the ErrUnresolved (no default) branch
// of the "env" type resolver directly (as opposed to the bare-env fallback).
func TestEnvResolver_UnsetNoDefault(t *testing.T) {
	os.Unsetenv("HIVE_TEST_ENVTYPE_UNSET")
	specs := []VarSpec{{Name: "V", Type: "env", Env: "HIVE_TEST_ENVTYPE_UNSET", Scope: ScopeConfig}}
	reg := Build(specs, Policy{}, nil)
	if got := reg.Expand(context.Background(), "${V}", ScopeConfig, nil); got != "${V}" {
		t.Errorf("unset env with no default should be literal, got %q", got)
	}
}

// TestScriptResolver_NoCommand covers the factory's "no command" error branch.
func TestScriptResolver_NoCommand(t *testing.T) {
	specs := []VarSpec{{Name: "EMPTY", Type: "script", Command: nil, Scope: ScopeConfig}}
	reg := Build(specs, Policy{AllowExec: true}, nil)
	if got := reg.Expand(context.Background(), "${EMPTY}", ScopeConfig, nil); got != "${EMPTY}" {
		t.Errorf("script with no command should leave literal, got %q", got)
	}
}

// TestScriptResolver_NilContext exercises Resolve when req.Ctx is nil (the
// resolver falls back to context.Background()). Reached via a direct
// resolver.Resolve call since Registry.Expand always supplies a context.
func TestScriptResolver_NilContext(t *testing.T) {
	skipIfWindows(t)
	f, ok := lookupFactory("script")
	if !ok {
		t.Fatal("script factory not registered")
	}
	r, err := f(VarSpec{Name: "X", Command: []string{"echo", "nilctx"}}, Policy{AllowExec: true, ExecTimeoutS: 5})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	res, err := r.Resolve(Request{Name: "X", Raw: "${X}", Ctx: nil})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Value != "nilctx" {
		t.Errorf("got %q want nilctx", res.Value)
	}
}

// TestHTTPResolver_InvalidURL covers the url.Parse error / empty-host branch
// of newHTTPResolver's factory.
func TestHTTPResolver_InvalidURL(t *testing.T) {
	cases := []string{
		"://bad-url",       // unparsable
		"not-a-url-at-all", // parses but has no host
	}
	for _, u := range cases {
		specs := []VarSpec{{Name: "V", Type: "http", URL: u, Scope: ScopeConfig}}
		reg := Build(specs, Policy{AllowHTTP: true, HTTPAllowlist: []string{"example.com"}}, nil)
		if got := reg.Expand(context.Background(), "${V}", ScopeConfig, nil); got != "${V}" {
			t.Errorf("invalid url %q should leave literal, got %q", u, got)
		}
	}
}

// TestHTTPResolver_NoURL covers the "has no url" branch of newHTTPResolver.
func TestHTTPResolver_NoURL(t *testing.T) {
	specs := []VarSpec{{Name: "V", Type: "http", URL: "", Scope: ScopeConfig}}
	reg := Build(specs, Policy{AllowHTTP: true, HTTPAllowlist: []string{"example.com"}}, nil)
	if got := reg.Expand(context.Background(), "${V}", ScopeConfig, nil); got != "${V}" {
		t.Errorf("http with no url should leave literal, got %q", got)
	}
}

// TestHTTPResolver_NilContext exercises Resolve's ctx==nil branch and the
// fallback-with-no-default path (Resolve error -> ErrUnresolved) directly.
func TestHTTPResolver_NilContext(t *testing.T) {
	f, ok := lookupFactory("http")
	if !ok {
		t.Fatal("http factory not registered")
	}
	// Use a bogus but well-formed host that resolves to a private/blocked IP so
	// the request fails deterministically without a live network dependency:
	// 127.0.0.1 is always dial-blocked by the SSRF guard.
	r, err := f(VarSpec{Name: "V", URL: "http://127.0.0.1:1/x"}, Policy{AllowHTTP: true, HTTPAllowlist: []string{"127.0.0.1"}, HTTPTimeoutS: 5})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	res, err := r.Resolve(Request{Name: "V", Raw: "${V}", Ctx: nil})
	if !errors.Is(err, ErrUnresolved) {
		t.Errorf("expected ErrUnresolved, got err=%v res=%+v", err, res)
	}
}

// TestScope_MatchesUnknownAndCrossScope exercises the remaining branches of
// Scope.matches: an unrecognized scope value (default case) and the
// ScopeConfig/ScopeTemplate cross-mismatch cases.
func TestScope_MatchesUnknownAndCrossScope(t *testing.T) {
	cases := []struct {
		scope Scope
		want  Scope
		out   bool
	}{
		{Scope("bogus"), ScopeTemplate, false}, // default branch
		{Scope("bogus"), ScopeConfig, false},   // default branch
		{ScopeConfig, ScopeTemplate, false},    // config var out of scope in template
		{ScopeTemplate, ScopeConfig, false},    // template var out of scope in config
		{ScopeBoth, ScopeConfig, true},
		{ScopeBoth, ScopeTemplate, true},
	}
	for _, c := range cases {
		if got := c.scope.matches(c.want); got != c.out {
			t.Errorf("Scope(%q).matches(%q) = %v, want %v", c.scope, c.want, got, c.out)
		}
	}
}

// TestBuild_EnvTypeInference covers the branch of Build where an empty Type
// with an empty Value infers "env" (as opposed to "static" when Value is set,
// which TestStaticAndEnvDefaults already covers).
func TestBuild_EnvTypeInference(t *testing.T) {
	t.Setenv("HIVE_TEST_INFER_ENV", "inferred")
	specs := []VarSpec{{Name: "HIVE_TEST_INFER_ENV", Scope: ScopeConfig}} // Type and Value both empty
	reg := Build(specs, Policy{}, nil)
	if got := reg.Expand(context.Background(), "${HIVE_TEST_INFER_ENV}", ScopeConfig, nil); got != "inferred" {
		t.Errorf("got %q want inferred", got)
	}
}

// TestResolveOne_RealErrorLogged covers the branch in resolveOne where a
// resolver returns a genuine (non-ErrUnresolved) error: the registry still
// leaves the literal in place, but takes the logging path rather than the
// silent-fallthrough path.
func TestResolveOne_RealErrorLogged(t *testing.T) {
	boom := errors.New("boom")
	RegisterResolver("test-realerror", func(spec VarSpec, _ Policy) (Resolver, error) {
		return resolverFunc(func(req Request) (Result, error) {
			return Result{}, boom
		}), nil
	})
	reg := Build([]VarSpec{{Name: "ERR", Type: "test-realerror", Scope: ScopeConfig}}, Policy{}, nil)
	if got := reg.Expand(context.Background(), "${ERR}", ScopeConfig, nil); got != "${ERR}" {
		t.Errorf("resolver real error should leave literal, got %q", got)
	}
}

// TestExpand_NilContext covers Registry.Expand's ctx==nil branch (falls back
// to context.Background()).
func TestExpand_NilContext(t *testing.T) {
	t.Setenv("HIVE_TEST_EXPAND_NILCTX", "ok")
	reg := EnvOnly()
	//nolint:staticcheck // intentionally passing nil to exercise the ctx==nil fallback branch
	got := reg.Expand(nil, "${HIVE_TEST_EXPAND_NILCTX}", ScopeConfig, nil)
	if got != "ok" {
		t.Errorf("got %q want ok", got)
	}
}

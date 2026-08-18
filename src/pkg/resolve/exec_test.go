package resolve

import (
	"context"
	"runtime"
	"testing"
)

func skipIfWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell utilities not available on Windows")
	}
}

// TestScriptResolver_Gated: the factory refuses to build when AllowExec is false,
// so a script def with exec disabled resolves to its literal ${VAR}.
func TestScriptResolver_Gated(t *testing.T) {
	specs := []VarSpec{{Name: "SHA", Type: "script", Command: []string{"echo", "hi"}, Scope: ScopeConfig}}
	reg := Build(specs, Policy{AllowExec: false}, nil)
	// Config scope so bare-env fallback could apply — but SHA isn't an env var,
	// so it must remain literal.
	if got := reg.Expand(context.Background(), "${SHA}", ScopeConfig, nil); got != "${SHA}" {
		t.Errorf("disabled script should leave literal, got %q", got)
	}
}

// TestScriptResolver_RunsAndTrims: with AllowExec, stdout is captured and
// trailing newline trimmed.
func TestScriptResolver_RunsAndTrims(t *testing.T) {
	skipIfWindows(t)
	specs := []VarSpec{{Name: "GREET", Type: "script", Command: []string{"echo", "hello world"}, Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowExec: true, ExecTimeoutS: 5}, nil)
	got := reg.Expand(context.Background(), "[${GREET}]", ScopeTemplate, nil)
	if got != "[hello world]" {
		t.Errorf("got %q want [hello world]", got)
	}
}

// TestScriptResolver_NonZeroExitLeavesLiteral: a failing command yields
// ErrUnresolved (literal), not a crash or partial value.
func TestScriptResolver_NonZeroExitLeavesLiteral(t *testing.T) {
	skipIfWindows(t)
	specs := []VarSpec{{Name: "FAIL", Type: "script", Command: []string{"false"}, Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowExec: true, ExecTimeoutS: 5}, nil)
	if got := reg.Expand(context.Background(), "x=${FAIL}", ScopeTemplate, nil); got != "x=${FAIL}" {
		t.Errorf("non-zero exit should leave literal, got %q", got)
	}
}

// TestScriptResolver_NonZeroExitUsesDefault: a failing command with a default
// resolves to the default.
func TestScriptResolver_NonZeroExitUsesDefault(t *testing.T) {
	skipIfWindows(t)
	d := "fallback"
	specs := []VarSpec{{Name: "FAIL", Type: "script", Command: []string{"false"}, HasDefault: true, Default: d, Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowExec: true, ExecTimeoutS: 5}, nil)
	if got := reg.Expand(context.Background(), "x=${FAIL}", ScopeTemplate, nil); got != "x=fallback" {
		t.Errorf("failing script with default: got %q", got)
	}
}

// TestScriptResolver_Timeout: a command that runs longer than the timeout is
// killed and the variable left literal.
func TestScriptResolver_Timeout(t *testing.T) {
	skipIfWindows(t)
	specs := []VarSpec{{Name: "SLOW", Type: "script", Command: []string{"sleep", "5"}, Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowExec: true, ExecTimeoutS: 1}, nil)
	if got := reg.Expand(context.Background(), "${SLOW}", ScopeTemplate, nil); got != "${SLOW}" {
		t.Errorf("timed-out script should leave literal, got %q", got)
	}
}

// TestScriptResolver_NoShellInjection: argv is executed directly, so shell
// metacharacters are passed as literal arguments, not interpreted.
func TestScriptResolver_NoShellInjection(t *testing.T) {
	skipIfWindows(t)
	// If this went through a shell, "; echo pwned" would run a second command.
	// As argv, echo prints the whole string literally.
	specs := []VarSpec{{Name: "INJ", Type: "script", Command: []string{"echo", "safe; echo pwned"}, Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowExec: true, ExecTimeoutS: 5}, nil)
	got := reg.Expand(context.Background(), "${INJ}", ScopeTemplate, nil)
	if got != "safe; echo pwned" {
		t.Errorf("argv must not be shell-interpreted, got %q", got)
	}
}

// TestScriptResolver_ScrubbedEnv: the child does not inherit hive's process
// environment, so a secret in the parent env is not visible to the script.
func TestScriptResolver_ScrubbedEnv(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("HIVE_SECRET_XYZ", "topsecret")
	specs := []VarSpec{{Name: "LEAK", Type: "script", Command: []string{"printenv", "HIVE_SECRET_XYZ"}, Scope: ScopeTemplate}}
	reg := Build(specs, Policy{AllowExec: true, ExecTimeoutS: 5}, nil)
	// printenv exits non-zero when the var is absent -> ErrUnresolved -> literal.
	if got := reg.Expand(context.Background(), "${LEAK}", ScopeTemplate, nil); got != "${LEAK}" {
		t.Errorf("scrubbed env should hide the secret (expected literal), got %q", got)
	}
}

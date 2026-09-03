package hooks

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// This file asserts the SECURITY INVARIANTS of RFC #4001 against the source
// and the API, not merely against behavior a future refactor could quietly
// drop. Several tests read the package's own AST: a test that only exercised
// the happy path would keep passing if someone added an exec action or a
// runtime registration endpoint, which is exactly the regression that matters.

// ---------------------------------------------------------------------------
// Operator-only registration (authz)
// ---------------------------------------------------------------------------

// TestRegistrationIsConfigOnly_NoRuntimeRegistrationAPI is the authz test.
//
// Hooks are operator-only BY CONSTRUCTION: the sole way to register one is to
// write the `hooks:` block in the PVC-backed config, which already requires
// operator authz and records layer provenance. This test proves the package
// exposes no alternative path — no exported Register/Add/Install function that
// an HTTP handler, an agent-reachable API, or any non-operator caller could
// use to add a hook at runtime.
//
// A non-operator therefore cannot register a hook because there is no API to
// call, which is a stronger guarantee than a permission check that could be
// mounted on the wrong route.
func TestRegistrationIsConfigOnly_NoRuntimeRegistrationAPI(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	// Names that would constitute a runtime registration path. SetRegistry is
	// deliberately excluded: it swaps a WHOLE COMPILED REGISTRY built from
	// validated config (hot reload), and cannot add an individual unvalidated
	// hook.
	forbidden := []string{
		"Register", "RegisterHook", "AddHook", "InstallHook",
		"AppendHook", "CreateHook", "NewHookFromRequest",
	}

	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || !fn.Name.IsExported() {
					continue
				}
				for _, bad := range forbidden {
					if fn.Name.Name == bad {
						t.Errorf("%s declares exported %s: hooks must be registrable "+
							"ONLY through operator config, never a runtime API",
							filepath.Base(path), fn.Name.Name)
					}
				}
			}
		}
	}
}

// TestOnlyConfigCanProduceARegistry documents the single supported ingress:
// a Registry comes from Compile over validated hooks, and the config path
// (CompileFromConfig) is the only one wired into the running system.
func TestOnlyConfigCanProduceARegistry(t *testing.T) {
	// The config path works…
	reg, err := CompileFromConfig(&config.Config{Hooks: []config.HookRule{{
		Name: "ok", On: "review_rejected", Action: "notify",
	}}})
	if err != nil || reg.Len() != 1 {
		t.Fatalf("config registration should work: %v", err)
	}

	// …and it is gated by the same fail-closed validation regardless of who
	// wrote the YAML, so a malicious or mistaken config cannot smuggle in an
	// unvetted action.
	if _, err := CompileFromConfig(&config.Config{Hooks: []config.HookRule{{
		Name: "evil", On: "review_rejected", Action: "exec",
	}}}); err == nil {
		t.Error("validation must apply to every config-registered hook")
	}
}

// TestHooksAppearInConfigProvenance: because a hook can pause an agent, WHICH
// LAYER declared it is security-relevant and must be visible in the provenance
// report — the same accountability every other operator config field has.
func TestHooksAppearInConfigProvenance(t *testing.T) {
	cfg := &config.Config{Hooks: []config.HookRule{
		{Name: "pause-on-red", On: "escalation_red", Action: "pause"},
	}}
	prov := config.NewProvenance()
	prov.Set("hooks.pause-on-red", config.LayerDashboardOverlay)

	var found bool
	for _, fo := range prov.Report(cfg) {
		if fo.Field == "hooks.pause-on-red" {
			found = true
			if !strings.Contains(fo.Value, "escalation_red") ||
				!strings.Contains(fo.Value, "pause") {
				t.Errorf("provenance should describe the rule, got %q", fo.Value)
			}
		}
	}
	if !found {
		t.Error("a registered hook must appear in the config provenance report")
	}
}

// ---------------------------------------------------------------------------
// No arbitrary code execution
// ---------------------------------------------------------------------------

// TestNoExecCapabilityInActionSet is the "why no exec" invariant, asserted
// three ways so it cannot regress silently.
func TestNoExecCapabilityInActionSet(t *testing.T) {
	// 1. The vetted set contains exactly the four declarative actions.
	got := KnownActions()
	want := []Action{ActionAnnotate, ActionEnqueueApproval, ActionNotify, ActionPause}
	if len(got) != len(want) {
		t.Fatalf("action set changed to %v — adding an action is a security review", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("action %d: got %q, want %q", i, got[i], want[i])
		}
	}

	// 2. Every plausible spelling of code execution is refused at validation.
	for _, name := range []string{
		"exec", "Exec", "EXEC", "script", "shell", "run", "command",
		"bash", "sh", "eval", "webhook", "http",
	} {
		if IsVettedAction(Action(name)) {
			t.Errorf("%q must not be a vetted action", name)
		}
		if _, err := Compile([]Hook{{
			Name: "h", On: TransitionReviewRejected, Action: Action(name),
		}}); err == nil {
			t.Errorf("a hook with action %q must be rejected", name)
		}
	}
}

// TestPackageDoesNotImportProcessExecution proves the absence of exec
// structurally: pkg/hooks must never import os/exec or syscall, so no action —
// present or future — can shell out without this test failing first.
func TestPackageDoesNotImportProcessExecution(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	banned := map[string]string{
		`"os/exec"`:       "process execution",
		`"syscall"`:       "raw syscalls",
		`"plugin"`:        "dynamic code loading",
		`"net/http/cgi"`:  "CGI execution",
		`"text/template"`: "template execution (injection surface)",
	}

	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			for _, imp := range file.Imports {
				if why, bad := banned[imp.Path.Value]; bad {
					t.Errorf("%s imports %s (%s): pkg/hooks must have no code-execution "+
						"capability — exec is a separate RFC with its own sandbox story",
						filepath.Base(path), imp.Path.Value, why)
				}
			}
		}
	}
}

// TestMutationSurfaceIsExactlyTheDeclaredSinks pins the complete set of ways a
// hook can affect the system. Every mutating action must go through one of
// these narrow, injected, audited interfaces — never a raw state write.
func TestMutationSurfaceIsExactlyTheDeclaredSinks(t *testing.T) {
	// A dispatcher with NO sinks wired can do nothing at all: every action
	// fails rather than falling back to some direct write.
	audit := &fakeAudit{}
	d := NewDispatcher(
		mustRegistry(t,
			Hook{Name: "a", On: TransitionReviewRejected, Action: ActionNotify},
			Hook{Name: "b", On: TransitionReviewRejected, Action: ActionAnnotate},
			Hook{Name: "c", On: TransitionReviewRejected, Action: ActionEnqueueApproval},
			Hook{Name: "d", On: TransitionReviewRejected, Action: ActionPause,
				Params: map[string]string{"agent": "x"}},
		),
		quietLogger(), WithAuditSink(audit))

	d.Fire(context.Background(), Payload{Transition: TransitionReviewRejected, Agent: "a"})
	d.Wait()

	// All four must FAIL — there is no unwired fallback path that mutates.
	if got := len(audit.withAction(AuditHookFailed)); got != 4 {
		t.Errorf("with no sinks wired every action must fail (no direct-write fallback), got %d failures", got)
	}
	if got := len(audit.withAction(AuditHookFired)); got != 0 {
		t.Errorf("nothing should succeed with no sinks wired, got %d", got)
	}
}

// TestPauserInterfaceOffersNoUnpause: a hook may stop work, but resuming it is
// a human decision, so the injected pause API deliberately has no resume.
func TestPauserInterfaceOffersNoUnpause(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "action.go", nil, 0)
	if err != nil {
		t.Fatalf("parse action.go: %v", err)
	}

	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Pauser" {
			return true
		}
		iface, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		for _, m := range iface.Methods.List {
			for _, name := range m.Names {
				if name.Name != "PauseAgent" {
					t.Errorf("Pauser exposes %q: it must offer pause only — "+
						"resuming an agent is a human decision", name.Name)
				}
			}
		}
		return false
	})
}

// ---------------------------------------------------------------------------
// Fail-closed posture
// ---------------------------------------------------------------------------

// TestBrokenPredicateCannotCauseAnAction: an unevaluable `when:` must be a
// no-match. Failing OPEN here would let a broken expression trigger a pause.
func TestBrokenPredicateCannotCauseAnAction(t *testing.T) {
	pauser := &fakePauser{}
	// A predicate that compiles but blows the cost budget at runtime.
	expensive := `t.attrs.all(k, t.attrs.all(k2, t.attrs.all(k3, k.size() + k2.size() + k3.size() >= 0)))`

	reg, err := Compile([]Hook{{
		Name: "risky", On: TransitionAgentPaused, Action: ActionPause,
		Params: map[string]string{"agent": "victim"}, When: expensive,
	}})
	if err != nil {
		// Rejected at compile time is an even better outcome — fail-closed at
		// the earliest possible point.
		return
	}

	d := NewDispatcher(reg, quietLogger(), WithPauser(pauser))
	attrs := map[string]string{}
	for i := 0; i < 60; i++ {
		attrs[strings.Repeat("k", i+1)] = strings.Repeat("v", i+1)
	}
	d.Fire(context.Background(), Payload{
		Transition: TransitionAgentPaused, Agent: "a", Attrs: attrs,
	})
	d.Wait()

	if len(pauser.all()) != 0 {
		t.Error("a predicate that could not be evaluated must never cause an action")
	}
}

// TestRateLimitHasNoUnlimitedSetting: every hook has a ceiling. This is what
// stops a flapping transition from becoming a notification storm even when the
// operator did not think about it.
func TestRateLimitHasNoUnlimitedSetting(t *testing.T) {
	// Unset → the default ceiling, not infinity.
	if (Hook{}).effectiveRateLimit() != DefaultRateLimitPerMinute {
		t.Error("an unset rate limit must fall back to the default ceiling")
	}
	// Zero is "use default", not "unlimited".
	if (Hook{RateLimitPerMinute: 0}).effectiveRateLimit() <= 0 {
		t.Error("zero must mean the default ceiling, never unlimited")
	}
	// The configured maximum is itself bounded.
	if MaxRateLimitPerMinute <= 0 || MaxRateLimitPerMinute > 1000 {
		t.Errorf("MaxRateLimitPerMinute (%d) should be a genuinely limiting ceiling",
			MaxRateLimitPerMinute)
	}
}

// TestDepthCapIsOne pins the documented depth-1 policy so raising it is a
// deliberate, reviewed change rather than an incidental edit.
func TestDepthCapIsOne(t *testing.T) {
	if maxHookDepth != 1 {
		t.Errorf("maxHookDepth is %d: the RFC specifies depth-1 (hook-caused "+
			"transitions do not fire hooks); raising it needs a loop-safety argument",
			maxHookDepth)
	}
}

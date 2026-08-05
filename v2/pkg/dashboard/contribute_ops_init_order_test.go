package dashboard

import (
	"regexp"
	"strings"
	"testing"
)

// These tests pin the fix for the Operations-tab init-order crash (the runtime
// regression from interleaving #2603 / #2604 / #2606). On the served page the
// confirm-modal buttons (#admin-confirm-cancel / #admin-confirm-ok) are emitted
// AFTER the command-center <script> block closes, so at script-eval time
// document.getElementById('admin-confirm-cancel') was null. The old code called
// .addEventListener on that null directly:
//
//	TypeError: null is not an object
//	  (evaluating 'document.getElementById('admin-confirm-cancel').addEventListener')
//
// That uncaught throw ABORTED the rest of the inline block — which is where
// ADMIN_TIER_ORDER, ccActivity and ccActivitySeen are initialized — leaving them
// undefined for the whole page. The downstream symptoms were:
//
//	ADMIN_TIER_ORDER.map  -> undefined  (renderAdminTierLimits)
//	ccActivitySeen[k]     -> undefined  (ccIngestActivity, via ccHydrate)
//	ccActivity.length     -> undefined  (ccCompletedWorkItems, every opsPoll)
//
// so the Live Activity rail never populated ("Watching the hive…") and the My-work
// "Done" filter threw on every poll. The fix is an ordering/guard fix (no feature
// change): null-guard/defer the listener wiring so the block can never abort, and
// declare+initialize the shared state at the top of the IIFE, before first use.

// mainOpsScript returns the largest inline <script> block from the served page —
// the command-center block that owns the ops-tab init.
func mainOpsScript(t *testing.T) string {
	t.Helper()
	body := renderContributePage(t)
	re := regexp.MustCompile(`(?s)<script>(.*?)</script>`)
	best := ""
	for _, mm := range re.FindAllStringSubmatch(body, -1) {
		if len(mm[1]) > len(best) {
			best = mm[1]
		}
	}
	if best == "" {
		t.Fatal("no <script> block found in served /contribute page")
	}
	return best
}

// stripJSComments removes // line comments so a comment that merely MENTIONS a var
// name or a getElementById call cannot fool the source-order / guard assertions.
// (Block comments in this file's JS do not wrap code we assert on, so line-comment
// stripping is sufficient and avoids mis-parsing '*/'-like tokens inside strings.)
func stripJSComments(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if i := strings.Index(ln, "//"); i >= 0 {
			ln = ln[:i]
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}

// TestOpsConfirmModalListenerNeverThrowsAtEval pins that the confirm-modal buttons
// are wired safely: never a raw document.getElementById('admin-confirm-cancel')
// .addEventListener at top level (that is the exact throw), and instead guarded
// and/or deferred until the element exists.
func TestOpsConfirmModalListenerNeverThrowsAtEval(t *testing.T) {
	code := stripJSComments(mainOpsScript(t))

	// The precise fatal call must be gone.
	for _, forbidden := range []string{
		`document.getElementById('admin-confirm-cancel').addEventListener`,
		`document.getElementById('admin-confirm-ok').addEventListener`,
	} {
		if strings.Contains(code, forbidden) {
			t.Errorf("unguarded confirm-modal listener still present (throws at script-eval, aborts block): %q", forbidden)
		}
	}

	// The wiring must be deferred until the DOM has parsed AND null-guarded.
	if !strings.Contains(code, "DOMContentLoaded") {
		t.Error("confirm-modal wiring is not deferred to DOMContentLoaded")
	}
	if !strings.Contains(code, "getElementById('admin-confirm-cancel')") {
		t.Error("confirm-modal cancel button is no longer wired at all")
	}
	// Guard shape: the reference to admin-confirm-cancel must be followed by an
	// `if(<var>)` guard before .addEventListener (never a direct deref).
	if !regexp.MustCompile(`var \w+=document\.getElementById\('admin-confirm-cancel'\);\s*if\(\w+\)`).MatchString(code) {
		t.Error("confirm-modal cancel listener is not null-guarded (expected var x=getElementById(...); if(x)x.addEventListener)")
	}
}

// TestOpsNoUnguardedTopLevelListeners pins that NO top-level (column-0, i.e. not
// inside a function) statement calls document.getElementById('x').addEventListener
// directly. Any such call throws if x is not yet parsed and aborts the whole inline
// block — the class of bug this fix eliminates. Guarded helpers (onEl / if-checks)
// and post-render wiring are fine.
func TestOpsNoUnguardedTopLevelListeners(t *testing.T) {
	code := stripJSComments(mainOpsScript(t))
	// Top-level statements in this IIFE are written flush-left (column 0).
	unguarded := regexp.MustCompile(`(?m)^document\.getElementById\((['"][^'"]+['"])\)\.(addEventListener|classList|value|checked|innerHTML|textContent|style|src|href|disabled|onclick)`)
	if locs := unguarded.FindAllString(code, -1); len(locs) > 0 {
		t.Errorf("found %d unguarded top-level getElementById(...).<prop> access(es) that can throw at eval and abort the block:\n  %s",
			len(locs), strings.Join(locs, "\n  "))
	}
}

// TestOpsSharedStateDeclaredBeforeUse pins the source-order invariant: the var
// declarations for ccActivity, ccActivitySeen and ADMIN_TIER_ORDER must appear
// BEFORE their first real (non-declaration) use in the emitted script. This is the
// structural guard against the value-hoisting trap that made them undefined at use.
func TestOpsSharedStateDeclaredBeforeUse(t *testing.T) {
	code := stripJSComments(mainOpsScript(t))

	cases := []struct {
		name string
		decl string
		use  string
	}{
		{"ADMIN_TIER_ORDER", "var ADMIN_TIER_ORDER=", "ADMIN_TIER_ORDER.map"},
		{"ccActivity", "var ccActivity=[]", "ccActivity.length"},
		{"ccActivitySeen", "var ccActivitySeen={}", "ccActivitySeen[k]"},
	}
	for _, c := range cases {
		decl := strings.Index(code, c.decl)
		use := strings.Index(code, c.use)
		if decl < 0 {
			t.Errorf("%s: declaration %q not found", c.name, c.decl)
			continue
		}
		if use < 0 {
			t.Errorf("%s: use %q not found", c.name, c.use)
			continue
		}
		if decl >= use {
			t.Errorf("%s: declaration (@%d) does not precede first use (@%d) — value-hoisting trap can make it undefined at use time",
				c.name, decl, use)
		}
	}
}

// TestOpsSharedStateDeclaredAtTopOfIIFE pins that the shared state is initialized
// near the top of the command-center IIFE (before the tab-switching setup), so no
// reachable code path can run a function that uses it before the value is assigned.
func TestOpsSharedStateDeclaredAtTopOfIIFE(t *testing.T) {
	code := stripJSComments(mainOpsScript(t))
	iife := strings.Index(code, "(function(){")
	if iife < 0 {
		t.Fatal("command-center IIFE '(function(){' not found")
	}
	tabs := strings.Index(code, "var tabs=document.querySelectorAll('.page-tab')")
	if tabs < 0 {
		t.Fatal("tab-switching setup marker not found")
	}
	for _, decl := range []string{"var ADMIN_TIER_ORDER=", "var ccActivity=[]", "var ccActivitySeen={}"} {
		d := strings.Index(code, decl)
		if d < 0 || d < iife || d > tabs {
			t.Errorf("%q is not hoisted to the top of the IIFE (iife@%d decl@%d tabs@%d)", decl, iife, d, tabs)
		}
	}
}

// TestOpsRailHydrationChainIntact is a belt-and-suspenders check that the fix left
// the rail-population chain (ccStart -> ccPollActivity -> ccIngestActivity ->
// ccRebuildLogFromActivity -> ccRenderLog) and the Done-filter path
// (ccCompletedWorkItems) wired, so the rail can actually show the backlog and the
// Done filter no longer throws.
func TestOpsRailHydrationChainIntact(t *testing.T) {
	code := mainOpsScript(t)
	for _, fn := range []string{
		"function ccStart(",
		"function ccPollActivity(",
		"function ccIngestActivity(",
		"function ccRebuildLogFromActivity(",
		"function ccRenderLog(",
		"function ccCompletedWorkItems(",
		"function renderAdminTierLimits(",
	} {
		if !strings.Contains(code, fn) {
			t.Errorf("rail/Done-filter hydration chain missing %q", fn)
		}
	}
}

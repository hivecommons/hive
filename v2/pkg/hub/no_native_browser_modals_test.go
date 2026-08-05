package hub

import (
	"regexp"
	"strings"
	"testing"
)

// TestNoNativeBrowserModals guards against reintroduction of native browser
// dialog calls (window.confirm, window.prompt, window.alert) in the hub's
// dashboardHTML. The dashboard convention is to use themed async modals
// (hiveConfirm, hiveNotify, hivePrompt) instead.
func TestNoNativeBrowserModals(t *testing.T) {
	html := dashScript(t)

	// Known legacy: the saved-view functions (saveCurrentView, renameSavedView,
	// deleteSavedView) still use native window.confirm/window.prompt. Track
	// how many native calls remain so new ones are caught immediately.
	const knownNativeConfirms = 2 // saveCurrentView overwrite + deleteSavedView
	const knownNativePrompts = 2  // saveCurrentView name + renameSavedView

	if n := strings.Count(html, "window.confirm("); n > knownNativeConfirms {
		t.Errorf("found %d window.confirm() calls (expected at most %d known legacy) — new code must use hiveConfirm()", n, knownNativeConfirms)
	}
	if n := strings.Count(html, "window.prompt("); n > knownNativePrompts {
		t.Errorf("found %d window.prompt() calls (expected at most %d known legacy) — new code must use hivePrompt()", n, knownNativePrompts)
	}
	if strings.Contains(html, "window.alert(") {
		t.Error("dashboardHTML contains window.alert() — use hiveNotify() or hiveToast() instead")
	}
}

// TestNoAwaitOutsideAsync guards await/async consistency: every call to
// `await hiveConfirm(...)` (or hiveNotify/hivePrompt) must appear inside
// an `async function`. Using await in a non-async function is either a
// SyntaxError (strict mode) or silently broken (sloppy mode). See #2533.
func TestNoAwaitOutsideAsync(t *testing.T) {
	html := dashScript(t)
	// Find every line that calls await hiveConfirm/hiveNotify/hivePrompt
	// and verify the enclosing function is declared async.
	awaitRe := regexp.MustCompile(`(?m)await\s+(hiveConfirm|hiveNotify|hivePrompt)\(`)
	// funcDeclRe matches JS function declarations (named or assigned).
	// We look for `async function` vs plain `function` walking backward.
	funcDeclRe := regexp.MustCompile(`(?m)^\s*(async\s+)?function\s+(\w+)\s*\(`)

	lines := strings.Split(html, "\n")
	for i, line := range lines {
		if !awaitRe.MatchString(line) {
			continue
		}
		// Walk backward to find the enclosing function declaration.
		found := false
		for j := i; j >= 0; j-- {
			m := funcDeclRe.FindStringSubmatch(lines[j])
			if m == nil {
				continue
			}
			asyncPrefix := m[1]
			funcName := m[2]
			if !strings.Contains(asyncPrefix, "async") {
				t.Errorf("line %d: await hiveConfirm/hiveNotify/hivePrompt in non-async function %s() — add 'async' keyword", i+1, funcName)
			}
			found = true
			break
		}
		if !found {
			t.Errorf("line %d: await hiveConfirm/hiveNotify/hivePrompt with no enclosing function declaration found", i+1)
		}
	}
}

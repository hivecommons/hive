package hub

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestNoNativeBrowserModals keeps window.confirm/prompt/alert out of the hub UI.
//
// Native dialogs cannot be styled, block the whole tab, and read as a browser
// warning rather than as part of the product — jarring in a themed dashboard
// that already has its own overlay modals (assign, access, timeline) and a
// hiveToast notifier.
//
// Use hiveConfirm / hiveNotify / hivePrompt instead. They follow the same
// overlay pattern, are promise-based so they drop into await-style call sites,
// and resolve false/null on Escape or a backdrop click.
//
// The check scans the rendered dashboard source rather than asserting on
// behaviour: the calls are spread across unrelated handlers, and what matters
// is that no NEW one appears anywhere — a behavioural test would enumerate the
// handlers that exist today and pass while a new one did it.
func TestNoNativeBrowserModals(t *testing.T) {
	// Word-boundary so hiveConfirm/hivePrompt/hiveNotify do not match, and the
	// optional window. prefix so both spellings are caught.
	native := regexp.MustCompile(`(^|[^\w.])(window\.)?(confirm|prompt|alert)\s*\(`)

	var offenders []string
	scanned := 0
	for _, line := range strings.Split(dashboardHTML, "\n") {
		scanned++
		trimmed := strings.TrimSpace(line)
		// Skip comment lines: the helpers document what they replace by name.
		if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "//") {
			continue
		}
		if native.MatchString(line) {
			offenders = append(offenders, "line "+strconv.Itoa(scanned)+": "+truncate(trimmed, 100))
		}
	}

	// A scan that reads nothing passes for the wrong reason. dashboardHTML is
	// thousands of lines; if this ever drops to near zero the constant moved or
	// was emptied, and a green result would prove nothing.
	if scanned < 500 {
		t.Fatalf("only %d lines scanned — dashboardHTML is not being read, so a pass proves nothing", scanned)
	}
	if len(offenders) > 0 {
		t.Errorf("native browser modal(s) in the hub dashboard. Use hiveConfirm / hiveNotify / hivePrompt — they match the existing overlay modals, are styleable, and do not block the tab:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestAwaitHiveModalInAsyncFunction guards against using await hiveConfirm /
// hiveNotify / hivePrompt in a non-async function.
//
// In strict-mode JavaScript, await in a non-async function is a SyntaxError.
// In sloppy mode, await may be parsed as an identifier, silently discarding the
// dialog result (e.g. making a confirmation dialog un-cancellable).
//
// The check finds every `await hiveConfirm/hiveNotify/hivePrompt` call, then
// walks backward to find the enclosing function declaration and asserts it is
// declared `async function`.
func TestAwaitHiveModalInAsyncFunction(t *testing.T) {
	awaitModal := regexp.MustCompile(`\bawait\s+(hiveConfirm|hiveNotify|hivePrompt)\s*\(`)
	// Matches function declarations: named functions, methods, arrow assigned.
	// We look for the nearest `function` keyword walking backward.
	funcDecl := regexp.MustCompile(`\bfunction\b`)
	asyncFunc := regexp.MustCompile(`\basync\s+function\b`)

	lines := strings.Split(dashboardHTML, "\n")

	var offenders []string
	found := 0
	for i, line := range lines {
		if !awaitModal.MatchString(line) {
			continue
		}
		found++

		// Walk backward to find the enclosing function declaration.
		foundFunc := false
		for j := i; j >= 0; j-- {
			if funcDecl.MatchString(lines[j]) {
				foundFunc = true
				if !asyncFunc.MatchString(lines[j]) {
					offenders = append(offenders, "line "+strconv.Itoa(i+1)+
						": await in non-async function (declared at line "+strconv.Itoa(j+1)+"): "+
						truncate(strings.TrimSpace(line), 80))
				}
				break
			}
		}
		if !foundFunc {
			// No function keyword found at all — top-level await in a classic
			// script (not a module) silently evaluates to the expression value.
			offenders = append(offenders, "line "+strconv.Itoa(i+1)+
				": await with no enclosing function: "+truncate(strings.TrimSpace(line), 80))
		}
	}

	if found == 0 {
		t.Fatal("no await hiveConfirm/hiveNotify/hivePrompt calls found — the guard scanned nothing")
	}
	if len(offenders) > 0 {
		t.Errorf("await hiveConfirm/hiveNotify/hivePrompt used outside async function. "+
			"The enclosing function must be declared 'async function' or the await is silently ignored:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

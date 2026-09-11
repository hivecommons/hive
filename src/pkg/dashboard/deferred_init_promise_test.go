package dashboard

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// #6581: the dashboard's deferred-init block calls each entry as
// `fn().then(resolve, resolve)`. Twelve of its thirteen entries are
// `async function`s, which return a promise for free. The thirteenth,
// fetchTimeline, was an ordinary function whose body never returned the
// promise it created, so `fetchTimeline().then(...)` was `.then` on undefined:
//
//	Uncaught TypeError: Cannot read properties of undefined (reading 'then')
//	    at (index):24760:32
//
// The throw escaped a setTimeout callback with nothing to catch it, so that
// entry's promise never settled, so Promise.all never settled, so
// _deferredLoadPromise stayed pending for the life of the page. _afterPaint
// awaits that promise, so first-load view restoration -- making
// #oc-agent-detail active, navigating to a saved section -- silently never
// ran, and setInterval(fetchTimeline, ...) was never installed either.
//
// Neither of the repository's JavaScript gates could see it: `node --check`
// parses it fine and ESLint's no-undef pass is satisfied because fetchTimeline
// IS defined. The property has to be asserted structurally, which is what this
// file does.

// deferredEntryCall matches one entry of the deferred array that immediately
// calls .then on a bare function call: `() => someFetch().then(`. Those are the
// entries whose callee must hand back a promise.
var deferredEntryCall = regexp.MustCompile(`\(\)\s*=>\s*([A-Za-z_$][\w$]*)\(\)\s*\.then\(`)

// deferredBlock isolates the deferred-init array so the scan cannot wander into
// unrelated `x().then(` elsewhere in a 24k-line file.
func deferredBlock(t *testing.T, html string) string {
	t.Helper()
	const startMark = "const deferred = ["
	start := strings.Index(html, startMark)
	if start < 0 {
		t.Fatalf("deferred-init array not found in index.html; this guard is checking nothing")
	}
	rest := html[start:]
	end := strings.Index(rest, "const allDone = deferred.map(")
	if end < 0 {
		t.Fatalf("deferred-init array has no allDone loop after it; this guard is checking nothing")
	}
	return rest[:end]
}

// TestDeferredInitEntriesReturnPromises is the general property, not a pin on
// one function name: every entry that calls .then on its callee must have a
// callee that actually returns something thenable. Adding another ordinary
// function to that list reintroduces #6581, and this fails when it happens.
func TestDeferredInitEntriesReturnPromises(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	matches := deferredEntryCall.FindAllStringSubmatch(deferredBlock(t, html), -1)
	if len(matches) == 0 {
		t.Fatal("no `() => fn().then(` entries found in the deferred-init array; this guard is checking nothing")
	}

	for _, m := range matches {
		name := m[1]
		t.Run(name, func(t *testing.T) {
			// An `async function` returns a promise whatever its body does, so
			// it needs no further proof.
			asyncDecl := regexp.MustCompile(`\basync\s+function\s+` + regexp.QuoteMeta(name) + `\s*\(`)
			if asyncDecl.MatchString(html) {
				return
			}

			// Otherwise it is an ordinary function, and the body has to return
			// the promise explicitly. Find the declaration and look for a
			// `return` before the next top-level function declaration.
			decl := regexp.MustCompile(`\bfunction\s+` + regexp.QuoteMeta(name) + `\s*\(`)
			loc := decl.FindStringIndex(html)
			if loc == nil {
				t.Fatalf("%s is called in the deferred-init array but is not declared as a function in index.html", name)
			}
			body := html[loc[1]:]
			if next := regexp.MustCompile(`\n\s*(async\s+)?function\s`).FindStringIndex(body); next != nil {
				body = body[:next[0]]
			}
			if !strings.Contains(body, "return ") {
				t.Errorf("%s is not `async` and its body never returns, so `%s().then(...)` is "+
					".then on undefined — an uncaught TypeError that strands the whole deferred "+
					"chain and blocks first-load view restoration (#6581). Either make it `async` "+
					"or return the promise.", name, name)
			}
		})
	}
}

// TestFetchTimelineReturnsItsPromise pins the specific regression, so a future
// edit that drops the `return` fails by name rather than only through the
// general scan above.
func TestFetchTimelineReturnsItsPromise(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	decl := regexp.MustCompile(`function fetchTimeline\(\)\s*\{\s*\n?\s*([^\n]*)`)
	m := decl.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("fetchTimeline is no longer declared as a plain function in index.html")
	}
	if !strings.HasPrefix(strings.TrimSpace(m[1]), "return ") {
		t.Errorf("fetchTimeline's first statement is %q, want it to `return` the fetch chain: "+
			"_deferredInit calls fetchTimeline().then(...) (#6581)", strings.TrimSpace(m[1]))
	}
}

// TestDeferredInitSurvivesOneBadEntry pins the hardening rather than the
// symptom. Even with every callee returning a promise today, calling `fn()`
// bare inside the setTimeout callback means the NEXT entry that throws
// synchronously strands the chain again -- the failure is silent, page-wide
// and permanent, which is far out of proportion to one broken feature.
//
// The wrapper turns a synchronous throw into a rejection the existing
// two-argument .then already handles, so a bad entry costs its own feature and
// nothing else.
func TestDeferredInitSurvivesOneBadEntry(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	loop := "const allDone = deferred.map("
	start := strings.Index(html, loop)
	if start < 0 {
		t.Fatal("the deferred-init allDone loop was not found; this guard is checking nothing")
	}
	block := html[start:]
	if end := strings.Index(block, "Promise.all(allDone)"); end > 0 {
		block = block[:end]
	}

	if !strings.Contains(block, "Promise.resolve().then(fn)") {
		t.Error("the deferred-init loop no longer wraps each entry in Promise.resolve().then(fn): " +
			"an entry that throws synchronously would escape the setTimeout callback, leave its " +
			"promise unsettled, and strand _deferredLoadPromise for the life of the page (#6581)")
	}

	// Bare `fn()` is the shape that caused it. Guard the exact regression.
	if regexp.MustCompile(`setTimeout\(\(\)\s*=>\s*fn\(\)\.then\(`).MatchString(block) {
		t.Error("the deferred-init loop calls fn() bare inside setTimeout again (#6581)")
	}

	// A failing entry must still be reported. Swallowing it would trade a
	// page-wide stall for an invisible one, which is how this stayed unfound.
	if !strings.Contains(block, "console.error") {
		t.Error("a failing deferred-init entry is no longer reported to the console; " +
			"the throw that caused #6581 was only ever found because it was visible there")
	}
}

// TestDeferredInitGuardsCoverEveryEntry is a meta-check: the scan above is only
// worth anything if it sees the whole list. If the array is reshaped so the
// regex stops matching most entries, this notices rather than passing quietly.
func TestDeferredInitGuardsCoverEveryEntry(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	block := deferredBlock(t, string(b))

	// Count TOP-LEVEL entries only: one per line, at the array's indentation.
	// Counting every `() =>` in the block would also count the nested arrows
	// inside each entry's own .then chains.
	entryLine := regexp.MustCompile(`(?m)^\s+\(\)\s*=>`)
	entries := len(entryLine.FindAllString(block, -1))
	matched := len(deferredEntryCall.FindAllString(block, -1))
	if entries == 0 {
		t.Fatal("no arrow entries in the deferred-init array; this guard is checking nothing")
	}
	// Not every entry has the `fn().then(` shape -- some are bare calls, some
	// inline a fetch -- but if the scan covers almost none of them it has
	// stopped being a guard.
	if matched*2 < entries {
		t.Errorf("the deferred-entry scan matched %d of %d top-level entries; the array shape has "+
			"drifted far enough that %s is no longer guarding it",
			matched, entries, fmt.Sprintf("%q", deferredEntryCall.String()))
	}
}

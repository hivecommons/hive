package dashboard

import (
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// csp_inline_handlers_test.go closes the gap that let kubestellar/hive#4680
// ship: the dashboard serves `script-src-attr 'none'` (#3848, ADR-0016), which
// makes an inline on*= handler attribute DEAD — the browser silently refuses to
// run it. TestCSPScriptSrcAttrUnsafeInlineIsAbsent pins the policy half and its
// comment asserts "every inline on*= handler attribute in static/index.html
// ... was replaced with data-action", but nothing checked the HTML, so four
// attributes survived the refactor and their controls quietly stopped working.
//
// The Explain Mode select on an agent's General tab was one of them: changing
// it never reached markDirty, so _configState.dirty stayed empty and Save
// answered "No changes to save" (#4680). There is no console error and no
// failed request — a CSP-blocked handler attribute simply never fires — so this
// class of break is invisible until a user reports it. Hence a test.
//
// This guards the DASHBOARD only. The hub (pkg/hub/static) sends no CSP at all,
// so its inline handlers still run and are not in scope here.

// inlineHandlerAttrPattern matches an inline HTML event-handler attribute:
// a handler name introduced by whitespace, `"`, `'`, or a backtick (so it is an
// attribute, not a property), followed by `=` and a quote.
//
// The leading character class is what keeps JS property assignment out of the
// results: `el.onclick = fn` and `el.onchange="x"` are preceded by `.`, are not
// attributes, and are NOT blocked by CSP — flagging them would be a false
// positive. TestInlineHandlerDetectorFlagsKnownBad pins both directions.
//
// The handler list is explicit rather than a loose `on[a-z]+=` so that a JS
// identifier that merely starts with "on" (`online="..."`) cannot trip it.
var inlineHandlerAttrPattern = regexp.MustCompile(
	"[\\s\"'`]on(" + strings.Join([]string{
		"abort", "animationend", "animationiteration", "animationstart",
		"beforeinput", "beforeunload", "blur", "cancel", "canplay", "change",
		"click", "close", "contextmenu", "copy", "cut", "dblclick", "drag",
		"dragend", "dragenter", "dragleave", "dragover", "dragstart", "drop",
		"durationchange", "ended", "error", "focus", "focusin", "focusout",
		"formdata", "input", "invalid", "keydown", "keypress", "keyup", "load",
		"loadeddata", "loadedmetadata", "loadstart", "mousedown", "mouseenter",
		"mouseleave", "mousemove", "mouseout", "mouseover", "mouseup", "paste",
		"pause", "play", "playing", "progress", "ratechange", "reset", "resize",
		"scroll", "search", "seeked", "seeking", "select", "submit", "suspend",
		"timeupdate", "toggle", "touchend", "touchmove", "touchstart",
		"transitionend", "volumechange", "waiting", "wheel",
	}, "|") + ")\\s*=\\s*[\"']")

// scanInlineHandlers returns "line N: <excerpt>" for every inline handler
// attribute in src, so a failure names the exact place to fix.
func scanInlineHandlers(src string) []string {
	var hits []string
	for i, line := range strings.Split(src, "\n") {
		for _, m := range inlineHandlerAttrPattern.FindAllString(line, -1) {
			hits = append(hits, "line "+strconv.Itoa(i+1)+": ..."+strings.TrimSpace(m)+"...")
		}
	}
	return hits
}

// TestEmbeddedStaticHasNoInlineEventHandlers walks every HTML/JS asset the
// dashboard embeds and fails on any inline on*= handler attribute.
//
// Do not silence a failure by adding an exception. Under `script-src-attr
// 'none'` the attribute does not run, so an exception would only be pinning a
// broken control in place. Wire the handler through the data-action dispatcher
// instead:
//
//	<select data-change-action="markDirty"
//	        data-arg0="general" data-arg1="explainMode" data-arg-types="s,s,v">
//
// where data-arg-types maps positionally — "s" passes the matching data-argN
// verbatim and "v" passes the element's live value. See hiveResolveActionArgs
// and hiveDispatchAction in static/index.html.
func TestEmbeddedStaticHasNoInlineEventHandlers(t *testing.T) {
	scanned := 0
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch path.Ext(p) {
		case ".html", ".js":
		default:
			return nil
		}
		body, readErr := fs.ReadFile(staticFS, p)
		if readErr != nil {
			return readErr
		}
		scanned++
		if hits := scanInlineHandlers(string(body)); len(hits) > 0 {
			t.Errorf("%s has %d inline on*= handler attribute(s), which CSP "+
				"script-src-attr 'none' blocks — the control is dead and its Save "+
				"silently does nothing (#4680):\n  %s\n"+
				"Convert each to data-action dispatch; see this test's doc comment.",
				p, len(hits), strings.Join(hits, "\n  "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking embedded static assets: %v", err)
	}
	// A detector that silently stops finding files would pass forever.
	if scanned == 0 {
		t.Fatal("scanned no embedded HTML/JS assets — the walk is broken, not the assets clean")
	}
}

// TestInlineHandlerDetectorFlagsKnownBad is the positive control: it proves the
// detector above can actually fail, and that it does not fire on the JS
// property assignments that legitimately appear throughout the SPA.
//
// Without this, a regex that quietly stopped matching would turn the guard into
// a permanently-green no-op — the exact failure mode that let #4680 through.
func TestInlineHandlerDetectorFlagsKnownBad(t *testing.T) {
	shouldFlag := []string{
		// The four attributes #4680 fixed, verbatim.
		`<select id="cfg-explain-mode" onchange="markDirty('general','explainMode',this.value)">`,
		`<input type="text" onchange="saveInferenceAuthField('vllm', 'endpoint', this.value)">`,
		// Spacing and quoting variants a future edit might use.
		`<button onclick='doThing()'>x</button>`,
		`<div onmouseover = "hover()">x</div>`,
		"<span onkeyup=\"go()\">x</span>",
	}
	for _, s := range shouldFlag {
		if len(scanInlineHandlers(s)) == 0 {
			t.Errorf("detector missed an inline handler attribute: %s", s)
		}
	}

	shouldNotFlag := []string{
		// JS property assignment: not an attribute, not blocked by CSP.
		`el.onclick = function () { go(); };`,
		`img.onerror="fallback()";`,
		`node.onchange = handler;`,
		// The replacement pattern must never be flagged.
		`<select data-change-action="markDirty" data-arg0="general" data-arg1="explainMode" data-arg-types="s,s,v">`,
		// An identifier that merely starts with "on".
		`let online="yes";`,
		// Prose and unrelated attributes.
		`<div class="config-field" title="Turn on change tracking">x</div>`,
	}
	for _, s := range shouldNotFlag {
		if hits := scanInlineHandlers(s); len(hits) > 0 {
			t.Errorf("detector false-positived on %s → %v", s, hits)
		}
	}
}

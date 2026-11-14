package dashboard

import (
	"bytes"
	"net/http"
	"strings"
)

// terminal_theme.go — makes the Sessions pane's embedded terminal match the
// dashboard theme.
//
// ttyd renders xterm.js on its own black document, so the Sessions section
// (which frames /terminal?arg=hive-sess-<id> in an iframe) showed a black
// rectangle inside a themed card: wrong in dark mode, where the dashboard's
// terminal surface is a near-black, and jarring in light mode and under every
// theme preset ("background does not match theme"). The ttyd binary is a
// pinned release, so as with the clipboard fix (terminal_clipboard.go) the
// proxied document is the one place the client can be adjusted.
//
// The script below runs only in a FRAMED document. The frame is same-origin
// (the Sessions terminal is the one /terminal response that allows same-origin
// framing, see securityHeaders), so the script resolves the dashboard's live
// CSS tokens -- --terminal-bg, --terminal-fg, --accent -- from the parent
// document with a probe element, hands xterm.js a theme derived from them and
// paints ttyd's own <body> the same colour so nothing black shows while xterm
// mounts or when the pane is wider than the grid. Resolving at runtime, rather
// than baking colours into this file, means a theme preset or a light/dark
// toggle in the dashboard is followed: a MutationObserver on the parent
// re-applies the theme when its class, style or stylesheets change.
//
// Only user-session documents receive the script (modifyTerminalResponse
// checks the proxied request's ?arg=): agent terminals open as a top-level tab
// and keep ttyd's stock look. No colour literal lives here; every value comes
// from the dashboard's tokens.

// terminalThemeMarker identifies an already-injected document so a re-proxied
// response is never given the script twice.
const terminalThemeMarker = "hive-terminal-theme"

// terminalThemeScript is appended to a user-session ttyd document. Plain ES5
// like terminalClipboardScript, so it runs wherever a self-hosted hive is
// reached from.
const terminalThemeScript = `<script id="` + terminalThemeMarker + `">
(function () {
  /* Only the Sessions iframe has a dashboard to match; a top-level agent
     terminal keeps ttyd's own look. A cross-origin parent throws here, and
     that too means "nothing to match". */
  var parentDoc;
  try {
    if (window.parent === window) return;
    parentDoc = window.parent.document;
  } catch (e) { return; }
  if (!parentDoc || !parentDoc.body) return;
  var SELECTION_ALPHA = 0.4;
  var SELECTION_INACTIVE_ALPHA = 0.25;
  var RETRY_MS = 100;
  var MAX_RETRIES = 100;
  /* A probe element turns a token (var() chains, color-mix, anything the
     parent's stylesheets resolve) into the browser's computed rgb(). */
  function resolveToken(name) {
    var probe = parentDoc.createElement('span');
    probe.style.color = 'var(' + name + ')';
    probe.style.position = 'absolute';
    probe.style.visibility = 'hidden';
    parentDoc.body.appendChild(probe);
    var rgb = parentDoc.defaultView.getComputedStyle(probe).color;
    parentDoc.body.removeChild(probe);
    var m = /^rgba?\(\s*(\d+)\s*,\s*(\d+)\s*,\s*(\d+)/.exec(rgb || '');
    if (!m) return null;
    return [Number(m[1]), Number(m[2]), Number(m[3])];
  }
  function hex2(n) { var s = Math.max(0, Math.min(255, Math.round(n))).toString(16); return s.length < 2 ? '0' + s : s; }
  function toHex(rgb, alpha) {
    var out = '#' + hex2(rgb[0]) + hex2(rgb[1]) + hex2(rgb[2]);
    if (typeof alpha === 'number') out += hex2(alpha * 255);
    return out;
  }
  function buildTheme() {
    var bg = resolveToken('--terminal-bg');
    var fg = resolveToken('--terminal-fg');
    var accent = resolveToken('--accent');
    if (!bg || !fg) return null;
    var theme = { background: toHex(bg), foreground: toHex(fg), cursor: toHex(fg), cursorAccent: toHex(bg) };
    if (accent) {
      theme.selectionBackground = toHex(accent, SELECTION_ALPHA);
      theme.selectionInactiveBackground = toHex(accent, SELECTION_INACTIVE_ALPHA);
    }
    return theme;
  }
  function paintDocument(theme) {
    document.documentElement.style.backgroundColor = theme.background;
    if (document.body) document.body.style.backgroundColor = theme.background;
  }
  function applyToTerm(theme) {
    var term = window.term;
    if (!term) return false;
    try {
      if (term.options) term.options.theme = theme;
      else if (typeof term.setOption === 'function') term.setOption('theme', theme);
    } catch (e) { /* an xterm that refuses the theme keeps ttyd's default */ }
    return true;
  }
  var current = null;
  function apply() {
    var theme = buildTheme();
    if (!theme) return;
    current = theme;
    paintDocument(theme);
    applyToTerm(theme);
  }
  /* xterm.js mounts after ttyd's bundle runs; the body can be painted now. */
  function waitForTerm(attempt) {
    if (current && applyToTerm(current)) return;
    if (attempt < MAX_RETRIES) setTimeout(function () { waitForTerm(attempt + 1); }, RETRY_MS);
  }
  apply();
  waitForTerm(0);
  /* Follow the dashboard: light/dark toggles flip a body class, theme presets
     swap stylesheets in <head>. */
  if (typeof MutationObserver === 'function') {
    var observer = new MutationObserver(function () { apply(); });
    observer.observe(parentDoc.body, { attributes: true, attributeFilter: ['class', 'style'] });
    observer.observe(parentDoc.documentElement, { attributes: true, attributeFilter: ['class', 'style', 'data-theme'] });
    if (parentDoc.head) observer.observe(parentDoc.head, { childList: true, subtree: true, characterData: true });
  }
})();
</script>`

// terminalSessionRequest reports whether req (the request proxied to ttyd) is
// for a user-session terminal: its first ttyd arg names a hive-sess-* tmux
// session. guardTerminalSession has already authorised such a request and the
// proxy's Rewrite has appended the attach token as the second arg.
func terminalSessionRequest(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	args := req.URL.Query()["arg"]
	return len(args) > 0 && strings.HasPrefix(args[0], sessionTmuxPrefix)
}

// injectTerminalThemeScript appends terminalThemeScript to doc, before the
// closing </body> when there is one. An already-injected document is returned
// unchanged.
func injectTerminalThemeScript(doc []byte) []byte {
	return injectTerminalScript(doc, terminalThemeMarker, terminalThemeScript)
}

// injectTerminalScript appends script to doc before the closing </body> (or at
// the end when there is none), unless doc already carries marker.
func injectTerminalScript(doc []byte, marker, script string) []byte {
	if bytes.Contains(doc, []byte(marker)) {
		return doc
	}
	s := []byte(script)
	idx := bytes.LastIndex(doc, []byte("</body>"))
	if idx < 0 {
		return append(append([]byte{}, doc...), s...)
	}
	out := make([]byte, 0, len(doc)+len(s))
	out = append(out, doc[:idx]...)
	out = append(out, s...)
	out = append(out, doc[idx:]...)
	return out
}

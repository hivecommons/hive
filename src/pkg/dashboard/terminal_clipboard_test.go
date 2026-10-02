package dashboard

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// startFakeTtydDocument runs a stand-in ttyd that serves an HTML document at
// the root and a JS asset beside it, recording the Accept-Encoding it was
// asked with so the plaintext-document requirement can be asserted.
func startFakeTtydDocument(t *testing.T, doc string) *string {
	t.Helper()
	var docAcceptEncoding string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = io.WriteString(w, "console.log('bundle');")
			return
		}
		docAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Etag", `"ttyd-doc"`)
		_, _ = io.WriteString(w, doc)
	}))
	t.Cleanup(backend.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(backend.URL, "http://"))
	if err != nil {
		t.Fatalf("splitting backend addr: %v", err)
	}
	t.Setenv("HIVE_TTYD_PORT", port)
	return &docAcceptEncoding
}

const fakeTtydDocument = `<!doctype html><html><head><title>ttyd</title></head><body><div id="terminal"></div><script src="/app.js"></script></body></html>`

// #9941: ttyd 1.7.7 only copies on SELECTION CHANGE, through an
// execCommand('copy') that Firefox refuses outside a user gesture, so the
// operator's ⌘C took whatever the clipboard already held instead of the
// pane selection. The proxy must hand the browser a document that answers the
// copy gesture itself.
func TestTerminalProxyInjectsClipboardScript(t *testing.T) {
	acceptEncoding := startFakeTtydDocument(t, fakeTtydDocument)
	h := newTerminalTestHandler(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/?arg=hive-quality", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("terminal document status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		terminalClipboardMarker,
		"var term = window.term;",
		"term.getSelection()",
		"navigator.clipboard.writeText(text)",
		"document.addEventListener('copy'",
		"e.metaKey ? !e.ctrlKey : (e.ctrlKey && e.shiftKey)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("proxied terminal document is missing %q", want)
		}
	}
	if !strings.Contains(body, "</script></body></html>") {
		t.Error("clipboard script was not injected before the closing </body>")
	}
	// A stale validator would let the browser cache ttyd's un-injected
	// document and silently undo the fix.
	if etag := rec.Header().Get("Etag"); etag != "" {
		t.Errorf("rewritten document kept upstream Etag %q", etag)
	}
	if got := rec.Header().Get("Content-Length"); got != "" && got != strconv.Itoa(len(body)) {
		t.Errorf("Content-Length = %s, want %d (the rewritten length)", got, len(body))
	}
	if *acceptEncoding != "identity" {
		t.Errorf("document fetched with Accept-Encoding %q, want identity so the HTML can be rewritten", *acceptEncoding)
	}
}

// Plain Ctrl+C must stay SIGINT: an operator who cannot interrupt a runaway
// command is worse off than one who cannot copy.
func TestTerminalClipboardScriptLeavesPlainCtrlCAlone(t *testing.T) {
	if strings.Contains(terminalClipboardScript, "e.ctrlKey && !e.shiftKey") {
		t.Fatal("clipboard script claims a plain Ctrl+C, which the pane needs for SIGINT")
	}
	if !strings.Contains(terminalClipboardScript, "if (!text) return;") {
		t.Error("clipboard script must fall through to the browser when nothing is selected")
	}
}

// ttyd's assets and its websocket upgrade must pass through untouched — a
// rewritten bundle is a dead terminal.
func TestTerminalProxyLeavesAssetsUntouched(t *testing.T) {
	startFakeTtydDocument(t, fakeTtydDocument)
	h := newTerminalTestHandler(t, "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/terminal/app.js", nil))

	if got := rec.Body.String(); got != "console.log('bundle');" {
		t.Errorf("asset body = %q, want it proxied byte for byte", got)
	}
}

func TestInjectTerminalClipboardScriptIsIdempotent(t *testing.T) {
	once := injectTerminalClipboardScript([]byte(fakeTtydDocument))
	twice := injectTerminalClipboardScript(once)
	if string(once) != string(twice) {
		t.Error("injecting twice duplicated the clipboard script")
	}
	if n := strings.Count(string(twice), terminalClipboardMarker); n != 1 {
		t.Errorf("clipboard script appears %d times, want 1", n)
	}
	// A document with no </body> still has to get the script.
	bare := injectTerminalClipboardScript([]byte("<html><body>no closing tag"))
	if !strings.Contains(string(bare), terminalClipboardMarker) {
		t.Error("document without </body> did not receive the clipboard script")
	}
}

func TestTerminalDocumentPath(t *testing.T) {
	for _, p := range []string{"", "/", "/index.html"} {
		if !terminalDocumentPath(p) {
			t.Errorf("terminalDocumentPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/app.js", "/token", "/ws", "/favicon.png"} {
		if terminalDocumentPath(p) {
			t.Errorf("terminalDocumentPath(%q) = true, want false", p)
		}
	}
}

// #9941 follow-up: the agent CLI owns the mouse, so an ordinary drag is the
// application's selection and arrives as an OSC 52 copy that ttyd 1.7.7 has
// no handler for. The injected script must handle OSC 52 and let a later copy
// gesture fall back to the application's last copy.
func TestTerminalClipboardScriptHandlesOsc52(t *testing.T) {
	for _, want := range []string{
		"term.parser.registerOscHandler(52,",
		"if (!payload || payload === '?') return '';",
		"new TextDecoder('utf-8').decode(bytes)",
		"return selectedText() || appCopiedText;",
		"term.hiveOsc52Registered = true;",
	} {
		if !strings.Contains(terminalClipboardScript, want) {
			t.Errorf("clipboard script is missing %q", want)
		}
	}
}

// OSC 52 is plain pane output, so any agent, tool or printed file can emit
// it. The script must only act on it shortly after a real operator gesture in
// the document, and must drop — not write, not remember — anything else, or
// pane output can replace the operator's clipboard behind their back.
func TestTerminalClipboardScriptGatesOsc52OnOperatorGesture(t *testing.T) {
	for _, want := range []string{
		"var GESTURE_WINDOW_MS = 2000;",
		"['mousedown', 'mouseup', 'touchend', 'keydown'].forEach(function (name) {",
		"document.addEventListener(name, noteGesture, true);",
		"if (!operatorGestureRecent()) return true;",
	} {
		if !strings.Contains(terminalClipboardScript, want) {
			t.Errorf("clipboard script is missing %q", want)
		}
	}
	handler := terminalClipboardScript[strings.Index(terminalClipboardScript, "registerOscHandler(52,"):]
	gate := strings.Index(handler, "if (!operatorGestureRecent()) return true;")
	decode := strings.Index(handler, "decodeOsc52(data)")
	remember := strings.Index(handler, "appCopiedText = text;")
	if gate < 0 || decode < 0 || remember < 0 || gate > decode || gate > remember {
		t.Fatal("the gesture gate must run before the OSC 52 payload is decoded, remembered or written")
	}
}

package dashboard

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// terminal_clipboard.go — makes a browser copy actually take the terminal's
// selection (issue #9941).
//
// THE DEFECT. The terminal is ttyd 1.7.7 (pinned in src/Dockerfile) serving
// xterm.js. xterm.js keeps its selection in its own model, not in the DOM, so
// the browser's Copy command has nothing of the pane to copy; ttyd's only
// answer is a copy-on-SELECT hook that calls document.execCommand('copy')
// from xterm's onSelectionChange handler (html/src/components/terminal/xterm/
// index.ts in the 1.7.7 tag). That call runs outside a user gesture, so
// Firefox refuses it, ttyd swallows the exception, and the operator's ⌘C
// copies whatever the clipboard already held — "what gets pasted is not what I
// had selected", the reported symptom, on the highest-stakes flow we have
// (lifting a /login URL out of a wedged agent's pane).
//
// THE FIX, and why it lives here. The ttyd binary is a pinned release
// download, so the client cannot be patched at the source; this server already
// proxies every terminal byte (registerTerminalProxy), so the document it
// streams is the one place a fix can be added without forking or re-pinning
// ttyd. A small inline script is appended to ttyd's HTML that answers the two
// events ttyd leaves unhandled — the `copy` event (browser Edit ▸ Copy) and a
// copy keystroke — by writing window.term.getSelection() to the clipboard from
// inside the gesture, where every browser permits it.
//
// Inline, not an asset: /terminal is the one path whose CSP deliberately keeps
// the blanket `script-src 'self' 'unsafe-inline'` (see securityHeaders in
// server.go) precisely because this server never holds ttyd's document bytes
// to hash, and an inline element needs no second proxied request.
//
// Plain Ctrl+C is deliberately NOT intercepted: it is SIGINT, and an operator
// who cannot interrupt a runaway command in an agent's pane is worse off than
// one who cannot copy. Only ⌘C and Ctrl+Shift+C — neither of which a terminal
// application consumes — are claimed, and only while a selection exists, so a
// pane with nothing selected keeps the browser's own behaviour.

// maxTerminalDocumentBytes bounds how much of a proxied response is buffered
// for injection. ttyd's index.html is a few kilobytes; anything larger is not
// the document and is streamed straight through.
const maxTerminalDocumentBytes = 1 << 20

// terminalClipboardMarker identifies an already-injected document so a
// re-proxied response is never given the script twice.
const terminalClipboardMarker = "hive-terminal-clipboard"

// terminalClipboardScript is appended to ttyd's document. It is written in the
// same plain ES5-with-arrows-avoided style as the rest of the served UI so it
// runs in whatever browser reaches a self-hosted hive.
const terminalClipboardScript = `<script id="` + terminalClipboardMarker + `">
(function () {
  function selectedText() {
    var term = window.term;
    if (!term || typeof term.getSelection !== 'function') return '';
    return term.getSelection() || '';
  }
  /* execCommand on a throwaway textarea is the fallback for a hive reached
     over plain http:// on a LAN address: navigator.clipboard exists only in a
     secure context, which that is not. Mirrors copyToClipboard() in
     static/index.html. */
  function legacyCopy(text) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand('copy'); } catch (e) { /* nothing else to try */ }
    document.body.removeChild(ta);
    var term = window.term;
    if (term && typeof term.focus === 'function') term.focus();
  }
  function copyText(text) {
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).catch(function () { legacyCopy(text); });
      return;
    }
    legacyCopy(text);
  }
  /* Browser Edit menu / context menu copy: the event fires, but the selection
     it would serialise is empty because xterm.js draws its own. */
  document.addEventListener('copy', function (e) {
    var text = selectedText();
    if (!text || !e.clipboardData) return;
    e.clipboardData.setData('text/plain', text);
    e.preventDefault();
  }, true);
  /* Keystroke copy. Firefox never fires a copy event for a keystroke with no
     DOM selection, so the write is done here rather than deferred to the
     handler above; preventDefault keeps the two from both firing. */
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'c' && e.key !== 'C') return;
    var isCopyCombo = e.metaKey ? !e.ctrlKey : (e.ctrlKey && e.shiftKey);
    if (!isCopyCombo) return;
    var text = selectedText();
    if (!text) return;
    e.preventDefault();
    e.stopPropagation();
    copyText(text);
  }, true);
})();
</script>`

// terminalDocumentPath reports whether p (already stripped of the /terminal
// prefix) is the ttyd document rather than one of its assets. Only the
// document is fetched uncompressed, so the megabyte JS bundle keeps its
// gzip on the wire.
func terminalDocumentPath(p string) bool {
	return p == "" || p == "/" || p == "/index.html"
}

// injectTerminalClipboardScript appends terminalClipboardScript to doc, before
// the closing </body> when there is one. An already-injected document is
// returned unchanged.
func injectTerminalClipboardScript(doc []byte) []byte {
	if bytes.Contains(doc, []byte(terminalClipboardMarker)) {
		return doc
	}
	script := []byte(terminalClipboardScript)
	idx := bytes.LastIndex(doc, []byte("</body>"))
	if idx < 0 {
		return append(append([]byte{}, doc...), script...)
	}
	out := make([]byte, 0, len(doc)+len(script))
	out = append(out, doc[:idx]...)
	out = append(out, script...)
	out = append(out, doc[idx:]...)
	return out
}

// bodyReadCloser pairs a replacement reader with the response body's original
// Closer, so a pass-through that has already consumed a prefix still releases
// the upstream connection.
type bodyReadCloser struct {
	io.Reader
	io.Closer
}

// modifyTerminalResponse injects the clipboard script into ttyd's HTML
// document. Every other response — assets, the websocket upgrade, errors, and
// anything that arrived compressed — is passed through byte for byte: a
// terminal that fails open is a terminal that still works.
func modifyTerminalResponse(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return nil
	}
	if resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	orig := resp.Body
	body, err := io.ReadAll(io.LimitReader(orig, maxTerminalDocumentBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxTerminalDocumentBytes {
		resp.Body = bodyReadCloser{Reader: io.MultiReader(bytes.NewReader(body), orig), Closer: orig}
		return nil
	}
	_ = orig.Close()
	out := injectTerminalClipboardScript(body)
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	// The bytes no longer match ttyd's entity, so its validators would make a
	// browser cache the un-injected document.
	resp.Header.Del("Etag")
	resp.Header.Del("Last-Modified")
	return nil
}

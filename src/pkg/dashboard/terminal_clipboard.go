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
// OSC 52 is honoured only within a short window after an operator gesture in
// the document (see the script): the escape is plain pane output, so without
// that gate any agent, tool or printed file could set the operator's
// clipboard — paste-jacking on the very flow this exists for.
//
// Plain Ctrl+C is deliberately NOT intercepted: it is SIGINT, and an operator
// who cannot interrupt a runaway command in an agent's pane is worse off than
// one who cannot copy. Only ⌘C and Ctrl+Shift+C — neither of which a terminal
// application consumes — are claimed, and only while a selection exists, so a
// pane with nothing selected keeps the browser's own behaviour.
//
// THE AGENT CLI'S OWN SELECTION (#9941 follow-up). Every agent CLI turns mouse
// reporting on, so tmux forwards an ordinary drag to the APPLICATION: the
// highlight the operator sees is the CLI's selection, which xterm.js knows
// nothing about. The CLI copies it with an OSC 52 escape, and per tmux(1)
// EITHER set-clipboard mode forwards that escape to the attached client only
// when its terminfo entry carries an `Ms` capability. ttyd's default TERM,
// xterm-256color, has no such entry, so the escape dead-ended with no `Ms` to
// act on — set-clipboard's default of `external` was never the defect; a
// missing `Ms` makes `on` just as inert. ttyd-tmux.sh now declares the
// `clipboard` terminal-feature outright (supplying `Ms` without depending on
// tmux's own secondary-DA auto-detection through this proxy chain) and sets
// `set-clipboard on`, so the escape reaches the attached client, and the
// script below handles OSC 52 — ttyd 1.7.7 loads no
// clipboard addon — by writing the decoded text to the browser clipboard.

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
  /* OSC 52: the agent CLI owns the mouse (it turns mouse reporting on), so an
     ordinary drag is the APPLICATION's selection, not xterm.js's. The CLI
     copies it with an OSC 52 escape that tmux (set-clipboard on, see
     ttyd-tmux.sh) forwards here — and ttyd 1.7.7 ships no handler for it, so
     the copy dead-ended in tmux's paste buffer (#9941). Write it to the
     clipboard now; the mouseup that ended the drag is still a live user
     activation in every browser. Also remember it, so a later ⌘C /
     Ctrl+Shift+C with no xterm.js selection copies it from inside a gesture
     if the immediate write was refused. */
  var appCopiedText = '';
  /* OSC 52 is just bytes in the pane: the agent CLI, any tool it runs, or
     any file or issue body it prints can emit it, and the /terminal tab is
     top-level, so a focused tab writes the clipboard with no activation at
     all. Only honour the escape shortly after a real operator gesture in
     this document — the mouseup that ends a drag-copy in the CLI is such a
     gesture — and drop (neither write nor remember) anything else, so pane
     output cannot swap the operator's clipboard behind their back. */
  var GESTURE_WINDOW_MS = 2000;
  var lastGestureAt = 0;
  function noteGesture() { lastGestureAt = Date.now(); }
  ['mousedown', 'mouseup', 'touchend', 'keydown'].forEach(function (name) {
    document.addEventListener(name, noteGesture, true);
  });
  function operatorGestureRecent() {
    return lastGestureAt > 0 && (Date.now() - lastGestureAt) <= GESTURE_WINDOW_MS;
  }
  function decodeOsc52(data) {
    var semi = data.indexOf(';');
    if (semi < 0) return '';
    var payload = data.slice(semi + 1);
    if (!payload || payload === '?') return '';
    try {
      var bin = atob(payload);
      var bytes = new Uint8Array(bin.length);
      for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      return new TextDecoder('utf-8').decode(bytes);
    } catch (e) {
      return '';
    }
  }
  function registerOsc52(attempt) {
    var term = window.term;
    if (!term || !term.parser || typeof term.parser.registerOscHandler !== 'function') {
      if (attempt < 100) setTimeout(function () { registerOsc52(attempt + 1); }, 100);
      return;
    }
    if (term.hiveOsc52Registered) return;
    term.hiveOsc52Registered = true;
    term.parser.registerOscHandler(52, function (data) {
      if (!operatorGestureRecent()) return true;
      var text = decodeOsc52(data);
      if (text) {
        appCopiedText = text;
        copyText(text);
      }
      return true;
    });
  }
  registerOsc52(0);
  function textToCopy() {
    return selectedText() || appCopiedText;
  }
  /* Browser Edit menu / context menu copy: the event fires, but the selection
     it would serialise is empty because xterm.js draws its own. */
  document.addEventListener('copy', function (e) {
    var text = textToCopy();
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
    var text = textToCopy();
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

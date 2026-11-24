package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// TestExportViewerLinkDialog pins the in-app link/export viewer after #18729:
// the Claude authorization URL (and every other hiveExportViewer link) is
// rendered as one full-width, read-only, selectable line in the dialog's own
// type and colour tokens, not a resizable <textarea>; a click selects the
// whole value; Open in new tab leads as the primary action, Copy is secondary
// and Close a ghost button; both actions carry aria-labels; the old
// implementation-detail note is gone.
func TestExportViewerLinkDialog(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		// Markup: a <code> line for links, a <pre> for multi-line exports, both read-only + focusable.
		`'<code class="hive-export-link" tabindex="0" aria-readonly="true"></code>'`,
		`'<pre class="hive-export-text" tabindex="0" aria-readonly="true"></pre>'`,
		`const isLink = !!sourceURL && text.trim().indexOf('\n') < 0;`,
		`textEl.setAttribute('aria-label', title);`,
		// Click selects all.
		`function hiveSelectAllText(el)`,
		`textEl.addEventListener('click', function() { hiveSelectAllText(textEl); });`,
		// Actions, in visual order: ghost Close, secondary Copy, primary Open.
		`<button type="button" class="hv-btn btn-ghost hive-dialog-btn hive-export-close">Close</button>`,
		`<button type="button" class="hv-btn btn-secondary hive-export-copy">Copy to clipboard</button>`,
		`<a class="hv-btn btn-primary hive-export-open" target="_blank" rel="noopener noreferrer" hidden>Open in new tab</a>`,
		`copy.setAttribute('aria-label', 'Copy ' + title + ' to clipboard');`,
		`open.setAttribute('aria-label', 'Open ' + title + ' in a new tab');`,
		// Open takes initial focus when it is shown; otherwise Copy becomes primary.
		`open.setAttribute('data-initial-focus', '');`,
		`copy.classList.add('btn-primary');`,
		// User-facing copy replaces the implementation note; the bearer-token case keeps its meaning.
		`noteText = canOpen ? 'Open this link to continue, then come back here when you are done.'`,
		`'Shown here because this dashboard signs in with a token that a new tab would not carry. Copy what you need from it.'`,
		// CSS: dialog tokens, full width, wraps mid-URL cleanly, selectable.
		`.hive-export-link, .hive-export-text { display: block; width: 100%; box-sizing: border-box; margin: 0; padding: var(--sp-4) var(--sp-5); font: inherit; color: var(--text);`,
		`user-select: all; -webkit-user-select: all; }`,
		`.hive-export-link { word-break: break-all; overflow-wrap: anywhere;`,
		`.hive-export-dialog .hive-dialog-title { margin-bottom: var(--sp-3); }`,
		// Callers that hand the user a URL still go through the shared builder.
		`hiveExportViewer({title:'Claude authorization URL', text:authorizeUrl, sourceURL:authorizeUrl, allowOpen:true});`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing export viewer wiring %q", want)
		}
	}
	for _, gone := range []string{
		`<textarea class="hive-export-text"`,
		`Rendered in-app so the browser does not prompt for downloads.`,
		`Rendered in-app because this dashboard uses a bearer token`,
	} {
		if strings.Contains(html, gone) {
			t.Errorf("index.html still contains the pre-#18729 export viewer fragment %q", gone)
		}
	}
	// Token-only CSS (ADR-0018) for the viewer block.
	start := strings.Index(html, "/* Export / link viewer (hiveExportViewer)")
	end := strings.Index(html, ".hive-export-link:focus-visible")
	if start < 0 || end < start {
		t.Fatal("export viewer CSS block not found")
	}
	if css := html[start:end]; strings.Contains(css, "#") || strings.Contains(css, "rgb(") || strings.Contains(css, "rgba(") {
		t.Errorf("export viewer CSS must use tokens only, found a literal colour:\n%s", css)
	}
}

// TestExportViewerCopyStatus pins the Copy result on the export popup itself.
// The Overview band download (and every other hiveExportViewer caller) used to
// report the copy with a toast, but #toast-container sits under
// .hive-dialog-overlay and its blur, so the confirmation could not be read.
// The popup now carries an empty role="status" line next to its buttons and
// hiveExportCopy writes the outcome there in plain words.
func TestExportViewerCopyStatus(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`<p class="hive-export-status muted-xs" role="status" aria-live="polite"></p>`,
		`const copyStatus = overlay.querySelector('.hive-export-status');`,
		`hiveExportCopy(text, textEl, copyStatus);`,
		`.hive-export-status { flex: 1 1 auto; min-width: 0; margin: 0; align-self: center; }`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing export viewer copy status wiring %q", want)
		}
	}
	viewer := jsFunc(t, html, "hiveExportViewer")
	copyFn := jsFunc(t, html, "hiveExportCopy")
	if strings.Contains(viewer, "showToast") || strings.Contains(copyFn, "showToast") {
		t.Error("export viewer copy reports through showToast again; the toast renders under the dialog overlay")
	}
	if strings.ContainsAny(copyFn, "\u2013\u2014") {
		t.Error("export viewer copy status uses an en or em dash")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH: the export viewer copy status was NOT executed by this run; the structure checks above still ran")
	}
	script := `
let navigator = {};
const timers = [];
function setTimeout(fn, ms) { timers.push({ fn, ms, cleared: false }); return timers.length; }
function clearTimeout(id) { if (timers[id - 1]) timers[id - 1].cleared = true; }
let selected = 0;
function hiveSelectAllText() { selected++; }
function fakeEl() { return { textContent: '', focused: 0, focus() { this.focused++; } }; }
function check(cond, msg) { if (!cond) throw new Error(msg); }
` + copyFn + `
(async () => {
  const textEl = fakeEl(), status = fakeEl();
  let written = null;
  navigator = { clipboard: { writeText(s) { written = s; return Promise.resolve(); } } };
  let ok = await hiveExportCopy('{"a":1}', textEl, status);
  check(ok === true && written === '{"a":1}', 'success: clipboard not written: ' + written);
  check(status.textContent === 'Copied to clipboard.', 'success status: ' + status.textContent);
  check(textEl.focused === 0 && selected === 0, 'success must not select the text for manual copy');
  check(timers.length === 1 && timers[0].ms > 0, 'success must schedule its own clear');
  timers[0].fn();
  check(status.textContent === '', 'success status not cleared by its timer: ' + status.textContent);

  ok = await hiveExportCopy('x', textEl, status);
  check(ok === true && timers.length === 2, 'second success did not schedule a clear');
  navigator = { clipboard: { writeText() { return Promise.reject(new Error('NotAllowedError')); } } };
  const pending = hiveExportCopy('x', textEl, status);
  check(status.textContent === '', 'a new click must reset the last outcome: ' + status.textContent);
  check(timers[1].cleared, 'a new click must cancel the pending success clear');
  ok = await pending;
  check(ok === false, 'blocked clipboard reported success');
  check(status.textContent === 'Your browser blocked the clipboard. The text is selected; press Ctrl+C (or Cmd+C) to copy it.', 'blocked status: ' + status.textContent);
  check(textEl.focused === 1 && selected === 1, 'blocked clipboard must select the text for manual copy');
  check(timers.length === 2, 'a blocked copy must stay on screen, not clear itself');

  navigator = {};
  ok = await hiveExportCopy('x', textEl, status);
  check(ok === false, 'no clipboard API reported success');
  check(status.textContent === 'The text is selected; press Ctrl+C (or Cmd+C) to copy it.', 'no-API status: ' + status.textContent);
  check(textEl.focused === 2 && selected === 2, 'no clipboard API must select the text for manual copy');
})().catch(e => { console.error(e.message); process.exit(1); });
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node export viewer copy status assertions failed: %v\n%s", err, out)
	}
}

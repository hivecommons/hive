package dashboard

import (
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

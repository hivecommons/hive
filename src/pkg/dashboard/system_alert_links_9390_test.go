package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// #9390: system-alert banners (e.g. "Merge blocked for hivecommons/pluk: ...
// Approve the runs manually or relax the repo setting at
// https://github.com/hivecommons/pluk/settings/actions") rendered their
// message as plain escaped text, so the operator had to select/copy/paste the
// URL the alert was telling them to open. renderSystemAlerts now escapes the
// message first — same as before — and then auto-links http(s) tokens into
// <a> tags, so a message can never inject raw markup even though it also
// grows a clickable link.
func TestSystemAlertMessagesAreEscapedThenLinkified(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the system-alert linkify rule was NOT executed")
	}
	html := indexHTML(t)

	for _, snippet := range []string{
		"function escapeHtmlAndLinkify(",
		"escapeHtmlAndLinkify(a.message)",
		"system-alert-link",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("index.html is missing %q", snippet)
		}
	}
	if strings.Contains(html, "a.message.replace(/</g, '&lt;')") {
		t.Fatal("renderSystemAlerts still renders a.message with the old bare escape — URLs will not be clickable")
	}

	var script strings.Builder
	script.WriteString("const assert = require('node:assert/strict');\n")
	script.WriteString(jsFunc(t, html, "escapeHtml") + "\n")
	script.WriteString(jsFunc(t, html, "escapeHtmlAndLinkify") + "\n")
	script.WriteString(`
// A message containing a URL produces a clickable, new-tab anchor.
const withUrl = escapeHtmlAndLinkify(
  'Merge blocked for hivecommons/pluk: approve the runs manually or relax the repo setting at https://github.com/hivecommons/pluk/settings/actions.'
);
assert.match(withUrl, /<a class="system-alert-link" href="https:\/\/github\.com\/hivecommons\/pluk\/settings\/actions" target="_blank" rel="noopener noreferrer">https:\/\/github\.com\/hivecommons\/pluk\/settings\/actions<\/a>/);
// Trailing sentence punctuation must not be swallowed into the href.
assert.ok(withUrl.endsWith('actions<\/a>.'), 'trailing period must stay outside the anchor: ' + withUrl);

// A <script> tag embedded in an untrusted message must stay escaped, even
// though the same message also contains a URL to linkify.
const hostile = escapeHtmlAndLinkify('<script>alert(1)</script> see https://example.com/x for details');
assert.ok(!hostile.includes('<script>'), 'raw <script> must never survive escaping: ' + hostile);
assert.match(hostile, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
assert.match(hostile, /<a class="system-alert-link" href="https:\/\/example\.com\/x" target="_blank" rel="noopener noreferrer">https:\/\/example\.com\/x<\/a>/);

// A message with no URL is left byte-identical to plain escaping.
const plain = 'GitHub App permission missing: contents:write';
assert.equal(escapeHtmlAndLinkify(plain), escapeHtml(plain));
console.log('ok');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("system-alert escape+linkify check failed:\n%s", strings.TrimSpace(string(out)))
	}
}

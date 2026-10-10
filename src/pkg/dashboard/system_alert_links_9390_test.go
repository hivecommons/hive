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
// URL/reference the alert was telling them to open. renderSystemAlerts now
// escapes the message first — same as before — and then auto-links http(s)
// tokens and GitHub owner/repo#N references into <a> tags, so a message can
// never inject raw markup even though it also grows clickable links.
func TestSystemAlertMessagesAreEscapedThenLinkified(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the system-alert linkify rule was NOT executed")
	}
	html := indexHTML(t)

	for _, snippet := range []string{
		"function escapeHtmlAndLinkify(",
		"function linkifyGitHubRefs(",
		"linkifyGitHubRefs(a.message, window._githubBaseUrl)",
		"stopSystemAlertLinkClicks(banner)",
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
	script.WriteString("globalThis.window = { _githubBaseUrl: 'https://github.com' };\n")
	script.WriteString(jsFunc(t, html, "escapeHtml") + "\n")
	script.WriteString(jsFunc(t, html, "githubRefsBaseURL") + "\n")
	script.WriteString(jsFunc(t, html, "normalizeGitHubRepoContext") + "\n")
	script.WriteString(jsFunc(t, html, "linkifyGitHubRefs") + "\n")
	script.WriteString(jsFunc(t, html, "escapeHtmlAndLinkify") + "\n")
	script.WriteString(`
// A message containing a URL produces a clickable, new-tab anchor.
const withUrl = escapeHtmlAndLinkify(
  'Merge blocked for hivecommons/pluk: approve the runs manually or relax the repo setting at https://github.com/hivecommons/pluk/settings/actions.'
);
assert.match(withUrl, /<a class="system-alert-link" href="https:\/\/github\.com\/hivecommons\/pluk\/settings\/actions" target="_blank" rel="noopener noreferrer">https:\/\/github\.com\/hivecommons\/pluk\/settings\/actions<\/a>/);
// Trailing sentence punctuation must not be swallowed into the href.
assert.ok(withUrl.endsWith('actions<\/a>.'), 'trailing period must stay outside the anchor: ' + withUrl);

// A stalled-plan alert containing owner/repo#N produces a GitHub issue/PR link
// that opens in a new tab. GitHub redirects /issues/N to /pull/N for PRs.
const withRepoRef = linkifyGitHubRefs('plan "kubestellar/console#23616" stalled for 288h44m0s and hit the replan cap (5) — needs human review', 'github.com');
assert.match(withRepoRef, /<a class="system-alert-link" href="https:\/\/github\.com\/kubestellar\/console\/issues\/23616" target="_blank" rel="noopener noreferrer">kubestellar\/console#23616<\/a>/);

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

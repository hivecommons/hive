package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPublicKnowledgeUIConfirmationAndRoles(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	html := indexHTML(t)
	start := strings.Index(html, "    // Public sharing is independent")
	end := strings.Index(html, "    // ── Knowledge Base ──")
	if start < 0 || end < start {
		t.Fatal("public sharing UI missing")
	}
	script := `
const assert = require('node:assert/strict');
const window = {_hiveRole: 'read'};
const dashboardRoleAtLeast = role => role === 'owner';
const escapeHtml = s => String(s).replaceAll('"', '&quot;').replaceAll('<', '&lt;');
let enabled = true, allow = false, calls = 0, confirmations = 0;
const button = {disabled: false};
const container = {querySelector: selector => selector.includes('enabled') ? {checked: enabled} : selector.includes('tags') ? {value: 'linux, public'} : button};
const document = {getElementById: id => id === 'controls' ? container : null, querySelector: () => null, activeElement: null};
const hiveConfirm = async () => {confirmations++; return allow;};
const showToast = () => {};
const fetch = async (url, opts) => { calls++; assert.equal(url, '/api/knowledge/public'); assert.equal(opts.method, 'PUT'); return {ok: true, json: async () => ({enabled, tags: [], url: 'https://example/mcp/knowledge', source: 'config'})}; };
` + html[start:end] + `
(async () => {
 _publicKnowledge = {enabled: false, tags: ['<tag>'], url: 'https://example/mcp/knowledge', source: 'env'};
 let markup = publicKnowledgeControls('controls');
 assert.ok(markup.includes(' disabled'));
 assert.ok(markup.includes('Only the owner'));
 assert.ok(!markup.includes('data-action="savePublicKnowledge"'));
 assert.ok(!markup.includes('value="<tag>"'));
 await savePublicKnowledge('controls');
 assert.equal(calls, 0);
 window._hiveRole = 'owner';
 markup = publicKnowledgeControls('controls');
 assert.ok(markup.includes('data-action="savePublicKnowledge"'));
 await savePublicKnowledge('controls');
 assert.equal(confirmations, 1);
 assert.equal(calls, 0, 'cancel must not publish');
 allow = true;
 await savePublicKnowledge('controls');
 assert.equal(calls, 1);
 assert.equal(button.disabled, false);
 enabled = false; allow = false;
 await savePublicKnowledge('controls');
 assert.equal(calls, 2, 'disable must not require enabling confirmation');
 assert.equal(confirmations, 2);
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("public sharing UI: %v\n%s", err, out)
	}
}

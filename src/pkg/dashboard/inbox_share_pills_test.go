package dashboard

import (
	"strings"
	"testing"
)

// Share history pills: one per record, latest per destination with a +N
// expander, links only for viewers who can open them. Helpers run under node.
func TestInboxSharePills(t *testing.T) {
	html := indexHTML(t)
	script := "(function () {\n" + strings.Join([]string{
		"const assert = require('assert');",
		"function escapeHtml(s) { return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/\"/g,'&quot;'); }",
		"var window = { _hiveRole: 'owner' }; var rendered = 0;",
		"function relativeTimeShort() { return '2m ago'; } function formatDashboardDateTime(s) { return 'at ' + s; } function renderInbox() { rendered++; }",
		jsConstLine(t, html, "_inboxHistoryOpen"),
		jsConstLine(t, html, "INBOX_SHARE_LABELS"),
		jsFunc(t, html, "inboxViewerCanPlay"),
		jsFunc(t, html, "inboxSharePillText"),
		jsFunc(t, html, "inboxShareTargetHref"),
		jsFunc(t, html, "inboxSharePillHtml"),
		jsFunc(t, html, "inboxSharePillsHtml"),
		jsFunc(t, html, "inboxToggleHistory"),
		`
const rec = (o) => Object.assign({ destination: 'rfc', actor: 'clubanderson', started_at: '2026-11-16T10:00:00Z', status: 'done' }, o);
const item = (hist) => ({ source: 'plaud', id: 'r1', fact_slug: 'plaud-r1', layer: 'personal', share_history: hist });
assert.strictEqual(inboxSharePillsHtml({ source: 'plaud', id: 'r1' }, 'plaud/r1'), '');
// Per destination and status.
let h = inboxSharePillsHtml(item([rec({ ref: { number: 18412, url: 'https://github.com/o/r/issues/18412' } })]), 'plaud/r1');
assert(h.includes('→ RFC #18412 · clubanderson · 2m ago') && h.includes('data-status="ok"'));
assert(h.includes('<a ') && h.includes('href="https://github.com/o/r/issues/18412"') && h.includes('noopener') && !h.includes('<button'));
h = inboxSharePillsHtml(item([rec({ destination: 'spek', status: 'in_progress', actor: 'andy', ref: { id: 'sess-1', name: 'nate-v2' } })]), 'plaud/r1');
assert(h.includes('→ spek: nate-v2 … · andy') && h.includes('data-status="syncing"') && h.includes('inbox-tx-spinner') && h.includes('href="#section-sessions-section?session=sess-1"'));
h = inboxSharePillsHtml(item([rec({ destination: 'dibs', ref: { url: 'https://dibs.example/i/7', id: '7' } })]), 'plaud/r1');
assert(h.includes('→ DIBS · ') && h.includes('href="https://dibs.example/i/7"'));
h = inboxSharePillsHtml(item([rec({ destination: 'knowledge', actor: 'inbox-auto', ref: { id: 'plaud-r1' } })]), 'plaud/r1');
assert(h.includes('→ Knowledge · inbox-auto') && h.includes('data-action="inboxOpenFact"'));
h = inboxSharePillsHtml(item([rec({ status: 'transcribing' })]), 'plaud/r1');
assert(h.includes('→ RFC (transcribing) · ') && h.includes('inbox-tx-spinner'));
// Failed: danger status, reason in the title, no link.
h = inboxSharePillsHtml(item([rec({ destination: 'dibs', status: 'failed', reason: 'dibs: 502 bad "gateway"' })]), 'plaud/r1');
assert(h.includes('data-status="error"') && h.includes('→ DIBS failed · ') && h.includes('title="dibs: 502 bad &quot;gateway&quot; · ') && h.includes('<span ') && !h.includes('href='));
// Read role: same pills, no links.
window._hiveRole = 'read';
h = inboxSharePillsHtml(item([rec({ ref: { number: 1, url: 'https://github.com/o/r/issues/1' } }), rec({ destination: 'knowledge', ref: { id: 'x' } })]), 'plaud/r1');
assert(h.includes('→ RFC #1') && h.includes('→ Knowledge') && !h.includes('href=') && !h.includes('<a '));
window._hiveRole = 'read-write';
assert(inboxSharePillsHtml(item([rec({ ref: { url: 'https://github.com/o/r/issues/1' } })]), 'plaud/r1').includes('href='));
window._hiveRole = 'owner';
// Collapse to the latest record per destination, "+N" expands the history.
const hist = [rec({ status: 'failed', reason: 'x', started_at: '2026-11-16T09:00:00Z' }), rec({ ref: { number: 5 } }), rec({ destination: 'dibs' })];
h = inboxSharePillsHtml(item(hist), 'plaud/r1');
assert.strictEqual((h.match(/inbox-share-pill/g) || []).length, 2, 'latest per destination only');
assert(h.includes('>+1<') && h.includes('data-action="inboxToggleHistory"') && !h.includes('failed'));
inboxToggleHistory('plaud', 'r1'); assert.strictEqual(rendered, 1);
h = inboxSharePillsHtml(item(hist), 'plaud/r1');
assert.strictEqual((h.match(/inbox-share-pill/g) || []).length, 3, 'full history when expanded');
assert(h.includes('>less<') && h.includes('failed'));
inboxToggleHistory('plaud', 'r1');
assert(!_inboxHistoryOpen.has('plaud/r1'));
`,
	}, "\n") + "\n})();"
	runStageTrackerRowActionsScript(t, script)
}

// The spek pill's deep link opens the Inception section and selects the
// session's tab: the hash is parsed once and claimed when the session exists.
func TestSessionsFocusDeepLink(t *testing.T) {
	html := indexHTML(t)
	script := "(function () {\n" + strings.Join([]string{
		"const assert = require('assert');",
		jsConstLine(t, html, "SESSIONS_FOCUS_HASH"),
		jsFunc(t, html, "sessionsFocusIdFromHash"),
		jsFunc(t, html, "sessionsClaimFocus"),
		`
assert.strictEqual(sessionsFocusIdFromHash('#section-sessions-section?session=ab%2Fc'), 'ab/c');
assert.strictEqual(sessionsFocusIdFromHash('#section-sessions-section'), '');
assert.strictEqual(sessionsFocusIdFromHash('#section-inbox-section?item=x'), '');
assert.strictEqual(sessionsFocusIdFromHash('#section-sessions-section?session=%E0%A4%A'), '', 'malformed escape is ignored');
var _sessionsFocusId = sessionsFocusIdFromHash('https://h.example/#section-sessions-section?session=s2');
assert.strictEqual(sessionsClaimFocus([{ id: 's1' }]), '', 'unknown session: keep waiting');
assert.strictEqual(sessionsClaimFocus([{ id: 's1' }, { id: 's2' }]), 's2');
assert.strictEqual(sessionsClaimFocus([{ id: 's2' }]), '', 'claimed once');
`,
	}, "\n") + "\n})();"
	runStageTrackerRowActionsScript(t, script)
}

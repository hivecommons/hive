package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func indexSliceBetween(t *testing.T, html, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("start marker %q not found", startMarker)
	}
	end := strings.Index(html[start:], endMarker)
	if end < 0 {
		t.Fatalf("end marker %q not found after %q", endMarker, startMarker)
	}
	return html[start : start+end]
}

func runNodeScript(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: Hive Chat JavaScript behavior was not executed")
	}
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node failed: %v\n%s", err, out)
	}
}

func TestHiveChatEmptyFencedBlocksRenderNoCopyButton(t *testing.T) {
	formatChatHtml := indexSliceBetween(t, indexHTML(t), "function formatChatHtml(raw)", "\n\n    // --- Hive Chat ---")
	runNodeScript(t, `
const assert = require('node:assert/strict');
function sanitizeChatText(s) { return String(s || ''); }
function escapeHtml(s) { return String(s || '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])); }
`+formatChatHtml+`
const empty = formatChatHtml('🔍 [scanner] Completed\n`+"```"+`\n   \n`+"```"+`');
assert(!empty.includes('chat-copy-btn'), empty);
assert(!empty.includes('chat-code-wrap'), empty);
const filled = formatChatHtml('🔍 [scanner] Completed\n`+"```"+`\nready\n`+"```"+`');
assert(filled.includes('chat-copy-btn'), filled);
assert(filled.includes('ready'), filled);
`)
}

func TestHiveChatForwardCommandDoesNotAppendNormalizedUserBubble(t *testing.T) {
	forwardCommand := indexSliceBetween(t, indexHTML(t), "function chatForwardCommand(scope)", "\n\n    async function chatRunCommand(q)")
	trackEcho := indexSliceBetween(t, indexHTML(t), "function chatTrackPendingUserEcho(text, delta)", "\n\n    function chatRenderOutbound(msg)")
	renderOutbound := indexSliceBetween(t, indexHTML(t), "function chatRenderOutbound(msg)", "\n\n    async function chatPollMessages()")
	runNodeScript(t, `
const assert = require('node:assert/strict');
let asked = [];
let rendered = [];
function chatAddMsg(text, cls) { rendered.push([text, cls]); }
async function chatAskBackend(q, suppressUserEcho) { asked.push([q, suppressUserEcho]); return 'ok'; }
`+forwardCommand+`
let chatLastSeq = 0;
let chatContext = [];
let chatPendingUserEchoes = new Map();
const CHAT_CONTEXT_LIMIT = 6;
const chatPanel = { classList: { contains: () => true } };
const chatFab = { classList: { add: () => {} } };
`+trackEcho+`
`+renderOutbound+`
(async () => {
  const result = await chatForwardCommand('spek')('spec runs');
  assert.equal(result, 'ok');
  assert.deepEqual(asked, [['spek: spec runs', true]]);
  chatTrackPendingUserEcho('spek: spec runs', 2);
  chatRenderOutbound({ seq: 1, role: 'user', text: 'spek: spec runs' });
  assert.deepEqual(rendered, []);
  chatRenderOutbound({ seq: 2, role: 'user', text: 'spek: spec runs' });
  assert.deepEqual(rendered, []);
  chatRenderOutbound({ seq: 3, role: 'user', text: 'spek: spec runs' });
  assert.deepEqual(rendered, [['spek: spec runs', 'user']]);
})().catch(err => { console.error(err); process.exit(1); });
`)
}

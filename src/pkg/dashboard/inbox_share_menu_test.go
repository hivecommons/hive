package dashboard

import (
	"strings"
	"testing"
)

// The Inbox item row carries only Play, Transcript and a Share menu (icon
// buttons named by aria-label, #18700); the
// menu has a Hive group (RFC…, spek…, Knowledge, DIBS, plus registered entries) and a
// Link group (Copy link, Share…). Helpers run under node with the globals
// injected.
func TestInboxShareMenuRowIsThreeControls(t *testing.T) {
	html := indexHTML(t)
	row := jsFunc(t, html, "inboxItemHtml")
	for _, gone := range []string{`data-arg2="spek_rfc"`, `data-action="inboxDismiss"`, `data-action="inboxUndismiss"`, `data-arg2="dibs"`} {
		if strings.Contains(row, gone) {
			t.Fatalf("item row still carries %s", gone)
		}
	}
	for _, want := range []string{`data-action="inboxPlay"`, `data-action="inboxToggleTranscript"`, `data-action="inboxShare"`, `aria-haspopup="menu"`} {
		if !strings.Contains(row, want) {
			t.Fatalf("item row missing %s", want)
		}
	}
}

func TestInboxShareMenuBehaviour(t *testing.T) {
	html := indexHTML(t)
	script := "(function () {\n" + strings.Join([]string{
		"const assert = require('assert');",
		"function escapeHtml(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/\"/g,'&quot;'); }",
		"var window = { _hiveRole: 'owner' }; var navigator = {}; var _inboxCache = null;",
		"var calls = [];",
		"function inboxOpenRoute(s, i, d) { calls.push(['route', s, i, d]); }",
		"function inboxDismiss(s, i) { calls.push(['dismiss', s, i]); } function inboxUndismiss(s, i) { calls.push(['undismiss', s, i]); }",
		"function inboxShareCopy(s, i) { calls.push(['copy', s, i]); } async function inboxShareNative(s, i) { calls.push(['native', s, i]); }",
		"function inboxDismissSelected() { calls.push(['bulk-dismiss']); } const _inboxSelected = new Set(['plaud/r1']);",
		"function inboxFind(s, i) { return ((_inboxCache || {}).items || []).find(it => it.source === s && it.id === i); }",
		"function inboxOpenSettings() { calls.push(['settings']); }",
		"var sessionDialogs = []; function inboxOpenSessionDialog(keys) { sessionDialogs.push(keys); } function inboxItemKey(it) { return it.source + '/' + it.id; }",
		jsConstBlock(t, html, "inboxShareHiveActions"),
		jsConstBlock(t, html, "inboxShareLinkActions"),
		jsFunc(t, html, "inboxDibsReady"),
		jsFunc(t, html, "inboxCanShare"),
		jsFunc(t, html, "inboxTxBlocked"),
		jsFunc(t, html, "inboxTxUnavailableText"),
		jsFunc(t, html, "inboxShareGate"),
		jsFunc(t, html, "inboxIntentLabel"),
		jsFunc(t, html, "inboxIntentThen"),
		jsFunc(t, html, "inboxTxEtaText"),
		jsFunc(t, html, "inboxTxElapsedText"),
		jsFunc(t, html, "inboxTxProgressHtml"),
		jsFunc(t, html, "inboxShareMenuGroups"),
		jsFunc(t, html, "inboxShareMenuHtml"),
		jsFunc(t, html, "inboxShareMenuRun"),
		jsFunc(t, html, "inboxBulkMenuGroups"),
		jsFunc(t, html, "inboxBulkMenuRun"),
		`
const item = (o) => Object.assign({ source: 'plaud', id: 'r1', title: 'T', routes: {} }, o || {});
const ready = (items) => ({ items: items || [item()], repos: ['o/r'], settings: { dibs_url: 'https://d.example', dibs_token_set: true } });
const ids = (gs) => gs.map(g => g.id + ':' + g.items.map(i => i.id).join(','));
(async () => {
  // Groups and order; Share… only with navigator.share.
  _inboxCache = ready(); navigator = { share: () => {} };
  let gs = inboxShareMenuGroups(item());
  assert.deepStrictEqual(ids(gs), ['hive:rfc,spek,knowledge,dibs', 'link:copy,native']);
  const markup = inboxShareMenuHtml(gs, 'plaud', 'r1');
  assert(markup.includes('role="menu"') && markup.includes('role="group" aria-label="Hive"') && markup.includes('aria-label="Link"'));
  assert(markup.indexOf('Hive') < markup.indexOf('>Link<'));
  assert.strictEqual((markup.match(/role="menuitem"/g) || []).length, 6);
  navigator = {};
  assert.deepStrictEqual(ids(inboxShareMenuGroups(item())), ['hive:rfc,spek,knowledge,dibs', 'link:copy']);
  // Role gating: read gets only the Link group; read-write and owner get Hive too.
  window._hiveRole = 'read'; assert.deepStrictEqual(ids(inboxShareMenuGroups(item())), ['link:copy']);
  assert.deepStrictEqual(inboxBulkMenuGroups(), []);
  window._hiveRole = 'read-write';
  assert.deepStrictEqual(ids(inboxShareMenuGroups(item())), ['hive:rfc,spek,knowledge,dibs', 'link:copy']);
  assert.deepStrictEqual(ids(inboxBulkMenuGroups()), ['hive:spek,knowledge']);
  window._hiveRole = 'owner';
  // Disabled reasons.
  _inboxCache = { items: [], repos: [], settings: {} };
  // Audio-only item with transcription unavailable: RFC and Knowledge are
  // disabled with the reason and a link to the Transcription settings; DIBS is not.
  _inboxCache = { items: [], repos: ['o/r'], settings: { dibs_url: 'u', dibs_token_set: true } };
  let hive = inboxShareMenuGroups(item({ pending: true, transcribe_unavailable: 'disabled' }))[0].items;
  for (const id of ['rfc', 'spek', 'knowledge']) { const e = hive.find(i => i.id === id); assert(e.disabled && e.hint.includes('Transcription is off'), id); }
  assert(!hive.find(i => i.id === 'dibs').disabled, 'DIBS always sends straight away');
  assert(hive.some(i => i.id === 'tx-settings'));
  window._hiveRole = 'read-write';
  assert(!inboxShareMenuGroups(item({ pending: true, transcribe_unavailable: 'budget' }))[0].items.some(i => i.id === 'tx-settings'), 'settings link is owner-only');
  window._hiveRole = 'owner';
  _inboxCache.items = [item()];
  await inboxShareMenuRun('hive', 'tx-settings', 'plaud', 'r1');
  assert.deepStrictEqual(calls.splice(0), [['settings']]);
  // Audio-only item that can be transcribed: enabled (transcribe first, then share).
  hive = inboxShareMenuGroups(item({ pending: true, transcribable: true }))[0].items;
  assert(!hive.find(i => i.id === 'rfc').disabled && !hive.find(i => i.id === 'spek').disabled && !hive.find(i => i.id === 'knowledge').disabled);
  assert(!hive.some(i => i.id === 'tx-settings'));
  // A waiting intent shows as disabled: Transcribing, then → RFC.
  hive = inboxShareMenuGroups(item({ pending: true, share_intents: { spek_rfc: { status: 'waiting' } } }))[0].items;
  assert.strictEqual(hive.find(i => i.id === 'rfc').hint, 'Transcribing, then → RFC');
  // A failed intent offers Retry.
  hive = inboxShareMenuGroups(item({ share_intents: { dismiss: { status: 'failed', error: 'x' } } }))[0].items;
  assert.strictEqual(hive.find(i => i.id === 'knowledge').label, 'Retry Knowledge');
  // Progress label.
  assert.strictEqual(inboxIntentThen(item({ share_intents: { spek_rfc: { status: 'waiting' } } })), 'RFC');
  const bar = inboxTxProgressHtml({ status: 'running', started_at: '2026-11-16T10:00:00Z', phase: 'transcribing', progress: 0.4, eta_seconds: 240 }, Date.parse('2026-11-16T10:01:00Z'), 'RFC');
  assert(bar.includes('Transcribing, then → RFC · 40%'), bar);
  assert(!inboxTxProgressHtml({ status: 'running', started_at: '2026-11-16T10:00:00Z', phase: 'transcribing', progress: 0.4 }, 0).includes('then'));
  assert(inboxTxProgressHtml({ status: 'queued' }, 0, 'Knowledge').includes('Transcribing, then → Knowledge'));
  _inboxCache = { items: [], repos: [], settings: {} };
  hive = inboxShareMenuGroups(item())[0].items;
  assert(hive.find(i => i.id === 'rfc').hint.includes('RFC repository'));
  const dibs = hive.find(i => i.id === 'dibs');
  assert(dibs.disabled && dibs.hint.includes('Settings → Inbox → dibs URL'));
  assert(inboxShareMenuHtml(inboxShareMenuGroups(item()), 'plaud', 'r1').includes('aria-disabled="true"'));
  // Failed route labels a retry; dismissed items offer Back to inbox.
  _inboxCache = ready();
  hive = inboxShareMenuGroups(item({ routes: { spek_rfc: { status: 'failed' } }, dismissed: true }))[0].items;
  assert.strictEqual(hive[0].label, 'Retry RFC…'); assert.strictEqual(hive[2].label, 'Back to inbox');
  // Each entry calls its existing handler.
  _inboxCache = ready([item()]); navigator = { share: () => {} };
  await inboxShareMenuRun('hive', 'rfc', 'plaud', 'r1');
  await inboxShareMenuRun('hive', 'dibs', 'plaud', 'r1');
  await inboxShareMenuRun('hive', 'knowledge', 'plaud', 'r1');
  await inboxShareMenuRun('link', 'copy', 'plaud', 'r1');
  await inboxShareMenuRun('link', 'native', 'plaud', 'r1');
  assert.deepStrictEqual(calls, [['route', 'plaud', 'r1', 'spek_rfc'], ['route', 'plaud', 'r1', 'dibs'], ['dismiss', 'plaud', 'r1'], ['copy', 'plaud', 'r1'], ['native', 'plaud', 'r1']]);
  // Bulk menu: Knowledge runs the bulk handler.
  calls.length = 0;
  assert.deepStrictEqual(ids(inboxBulkMenuGroups()), ['hive:spek,knowledge']);
  inboxBulkMenuRun('knowledge'); assert.deepStrictEqual(calls, [['bulk-dismiss']]);
  // Registration point: another feature adds one entry.
  const ran = [];
  inboxShareHiveActions.splice(2, 0, { id: 'extra', label: 'extra…', enabled: (it, role) => role === 'owner' ? true : false, run: it => ran.push(it.id) });
  assert.deepStrictEqual(ids(inboxShareMenuGroups(item()))[0], 'hive:rfc,spek,extra,knowledge,dibs');
  await inboxShareMenuRun('hive', 'extra', 'plaud', 'r1'); assert.deepStrictEqual(ran, ['r1']);
  // spek… opens the session dialog for the item, and for the selection in bulk.
  await inboxShareMenuRun('hive', 'spek', 'plaud', 'r1');
  inboxBulkMenuRun('spek');
  assert.deepStrictEqual(sessionDialogs, [['plaud/r1'], ['plaud/r1']]);
  // A waiting spek share reads "Transcribing, then → spek".
  hive = inboxShareMenuGroups(item({ pending: true, share_intents: { spek: { status: 'waiting' } } }))[0].items;
  assert.strictEqual(hive.find(i => i.id === 'spek').hint, 'Transcribing, then → spek');
})().catch(e => { console.error(e); process.exit(1); });
`,
	}, "\n") + "\n})();"
	runStageTrackerRowActionsScript(t, script)
}

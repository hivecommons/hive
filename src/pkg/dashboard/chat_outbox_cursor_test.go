package dashboard

import "testing"

// chatPollHarness executes the real Hive Chat poll slice from index.html
// (cursor persistence, chatRenderOutbound, chatApplyPoll, the scheduler and
// chatPollMessages) under node with fetch/setTimeout/localStorage doubles, the
// same technique as hive_chat_echo_empty_copy_test.go. The trailing top-level
// chatPollMessages() call in the slice fires on load, so callers queue the
// first response before the slice runs (hivecommons/hive#9135).
func chatPollHarness(t *testing.T, firstResponse, scenario string) string {
	t.Helper()
	html := indexHTML(t)
	cursor := indexSliceBetween(t, html, "function chatLoadCursor()", "\n    function chatPersistCommandHistory()")
	trackEcho := indexSliceBetween(t, html, "function chatTrackPendingUserEcho(text, delta)", "\n\n    function chatRenderOutbound(msg, viewer)")
	poll := indexSliceBetween(t, html, "function chatRenderOutbound(msg, viewer)", "\n\n    // GitHub Device Flow Auth")
	return `
const assert = require('node:assert/strict');
const store = new Map();
const localStorage = { getItem: k => store.has(k) ? store.get(k) : null, setItem: (k, v) => store.set(k, String(v)) };
function safeJsonParse(s, fb) { try { return JSON.parse(s); } catch (_) { return fb; } }
const CHAT_CURSOR_KEY = 'hive-chat-cursor-v1';
const CHAT_POLL_OPEN_MS = 2000, CHAT_POLL_IDLE_MS = 10000, CHAT_POLL_MAX_BACKOFF_MS = 60000, CHAT_CONTEXT_LIMIT = 6;
let chatLastSeq = 0, chatEpoch = '', chatPollTimer = null, chatPollFailures = 0, chatContext = [], chatPendingUserEchoes = new Map();
let panelOpen = false, unread = false;
const chatPanel = { classList: { contains: c => c === 'open' && panelOpen } };
const chatFab = { classList: { add: c => { if (c === 'has-unread') unread = true; } } };
const document = { hidden: false, listeners: {}, addEventListener(ev, fn) { this.listeners[ev] = fn; } };
function _inceptionAuthHeaders() { return {}; }
const rendered = [];
function chatAddMsg(text, cls, opts) { rendered.push({ text: String(text), cls, author: (opts && opts.author) || '', gap: !!(opts && opts.gap) }); }
const scheduled = [];
globalThis.setTimeout = (fn, ms) => { scheduled.push(ms); return scheduled.length; };
globalThis.clearTimeout = () => {};
const urls = [];
const responses = [];
globalThis.fetch = async (url) => {
  urls.push(String(url));
  const r = responses.shift();
  if (!r) throw new Error('no response queued for ' + url);
  return { ok: r.status < 300, status: r.status, json: async () => r.body };
};
function respond(body, status) { responses.push({ status: status || 200, body }); }
function sinceOf(url) { return Number(new URL(url, 'http://h').searchParams.get('since')); }
function flush() { return new Promise(r => setImmediate(r)); }
function cursor() { return JSON.parse(store.get(CHAT_CURSOR_KEY) || 'null'); }
` + firstResponse + `
` + cursor + `
` + trackEcho + `
` + poll + `
(async () => {
` + scenario + `
})().catch(err => { console.error(err); process.exit(1); });
`
}

// Reload used to replay the whole outbox (chatLastSeq started at 0 on every
// page load); a hive restart used to drop the next N replies silently (seq
// restarted at 1 below the stale cursor); ring eviction was invisible.
func TestHiveChatPollCursorSurvivesReloadRestartAndEviction(t *testing.T) {
	runNodeScript(t, chatPollHarness(t, `
respond({ messages: [
  { seq: 1, role: 'bot', text: 'reply one' },
  { seq: 2, role: 'bot', text: 'reply two' },
  { seq: 3, role: 'bot', text: 'reply three' },
], next: 3, epoch: 'gen-A', gap: false, viewer: 'bob' });
`, `
  await flush();
  assert.deepEqual(urls.map(sinceOf), [0]);
  assert.deepEqual(rendered.map(m => m.text), ['reply one', 'reply two', 'reply three']);
  assert.equal(chatLastSeq, 3);
  assert.deepEqual(cursor(), { epoch: 'gen-A', seq: 3 });

  // Reload: only the persisted cursor survives; the transcript is restored
  // from localStorage elsewhere, so the poll must NOT replay seq 1..3.
  chatLastSeq = 0; chatEpoch = ''; rendered.length = 0;
  chatLoadCursor();
  assert.equal(chatLastSeq, 3);
  assert.equal(chatEpoch, 'gen-A');
  respond({ messages: [], next: 3, epoch: 'gen-A', gap: false, viewer: 'bob' });
  await chatPollMessages();
  assert.equal(sinceOf(urls[urls.length - 1]), 3);
  assert.deepEqual(rendered, [], 'reload replayed the outbox');

  // Restart: the new process numbers from 1 and filters since=3 to nothing.
  // The client must notice the epoch change, reset, and re-poll from 0.
  respond({ messages: [], next: 1, epoch: 'gen-B', gap: false, viewer: 'bob' });
  respond({ messages: [{ seq: 1, role: 'bot', text: '4th msg' }], next: 1, epoch: 'gen-B', gap: false, viewer: 'bob' });
  await chatPollMessages();
  assert.deepEqual(urls.slice(-2).map(sinceOf), [3, 0]);
  assert.deepEqual(rendered.map(m => m.text), [rendered[0].text, '4th msg']);
  assert.ok(rendered[0].gap && /restart/i.test(rendered[0].text), 'restart was not announced: ' + JSON.stringify(rendered[0]));
  assert.equal(chatLastSeq, 1);
  assert.deepEqual(cursor(), { epoch: 'gen-B', seq: 1 });

  // Eviction: the ring dropped seq 2..11 before this poll; the server says so.
  // The marker follows the retained tail so the DOM cap cannot trim it away.
  rendered.length = 0;
  respond({ messages: [{ seq: 12, role: 'bot', text: 'after the burst' }], next: 12, epoch: 'gen-B', gap: true, viewer: 'bob' });
  await chatPollMessages();
  assert.equal(rendered.length, 2, JSON.stringify(rendered));
  assert.equal(rendered[0].text, 'after the burst');
  assert.ok(rendered[1].gap && rendered[1].cls === 'system', 'gap marker missing: ' + JSON.stringify(rendered[1]));
  assert.equal(chatLastSeq, 12);

  // A fresh cursor (since=0) has nothing to miss: no gap marker even when the
  // ring already rolled, and no re-poll loop when the epoch is first adopted.
  chatLastSeq = 0; chatEpoch = ''; rendered.length = 0; urls.length = 0;
  respond({ messages: [{ seq: 300, role: 'bot', text: 'late joiner' }], next: 300, epoch: 'gen-B', gap: true, viewer: 'bob' });
  await chatPollMessages();
  assert.deepEqual(urls.map(sinceOf), [0]);
  assert.deepEqual(rendered.map(m => m.text), ['late joiner']);
  assert.equal(chatEpoch, 'gen-B');
`))
}

// Alice's input used to land in Bob's panel as Bob's own bubble and in Bob's
// chatContext (posted back as history). The channel is shared, so only a line
// whose author is the viewer is "mine"; peers and unattributed lines are
// rendered separately and kept out of the viewer's context.
func TestHiveChatPollAttributesPeerInputToItsAuthor(t *testing.T) {
	runNodeScript(t, chatPollHarness(t, `
respond({ messages: [
  { seq: 1, role: 'user', author_id: 'alice', text: '!runs reject acme/widgets#7 wrong scope' },
  { seq: 2, role: 'bot', text: 'Run acme/widgets#7 rejected.' },
  { seq: 3, role: 'user', author_id: 'bob', text: '!status' },
  { seq: 4, role: 'user', text: 'unattributed line' },
  { seq: 5, role: 'bot', text: '' },
], next: 5, epoch: 'gen-A', gap: false, viewer: 'bob' });
`, `
  await flush();
  assert.deepEqual(rendered.map(m => [m.cls, m.author, m.text]), [
    ['peer', 'alice', '!runs reject acme/widgets#7 wrong scope'],
    ['system', '', 'Run acme/widgets#7 rejected.'],
    ['user', '', '!status'],
    ['peer', 'unknown', 'unattributed line'],
  ]);
  assert.deepEqual(chatContext.map(c => c.text), ['Run acme/widgets#7 rejected.', '!status'], "a non-self line leaked into bob's history");
  assert.equal(chatLastSeq, 5, 'an empty-text entry must still advance the cursor');
  assert.equal(unread, true, 'a peer line while the panel is closed must light the unread badge');

  // Bob's own pending echo is still de-duplicated; alice's identical text is not his echo.
  rendered.length = 0;
  chatTrackPendingUserEcho('!help', 1);
  respond({ messages: [
    { seq: 6, role: 'user', author_id: 'alice', text: '!help' },
    { seq: 7, role: 'user', author_id: 'bob', text: '!help' },
  ], next: 7, epoch: 'gen-A', gap: false, viewer: 'bob' });
  await chatPollMessages();
  assert.deepEqual(rendered.map(m => [m.cls, m.author]), [['peer', 'alice']]);
  assert.equal(chatPendingUserEchoes.size, 0);

  // Overlapping callers (timer, FAB open, visibilitychange, /api/chat) share
  // one in-flight poll: the second call must not issue a second fetch.
  urls.length = 0;
  respond({ messages: [], next: 7, epoch: 'gen-A', gap: false, viewer: 'bob' });
  await Promise.all([chatPollMessages(), chatPollMessages(), chatPollMessages()]);
  assert.equal(urls.length, 1, 'overlapping polls must collapse to one request');
`))
}

// The poll used to be a fixed 2 s setInterval that swallowed every non-OK
// status and kept polling in hidden tabs.
func TestHiveChatPollBacksOffAndPausesWhenHidden(t *testing.T) {
	runNodeScript(t, chatPollHarness(t, `
respond({ messages: [], next: 0, epoch: 'gen-A', gap: false, viewer: 'bob' });
`, `
  await flush();
  assert.deepEqual(scheduled, [10000], 'closed panel polls at the idle cadence');

  panelOpen = true;
  respond({ messages: [], next: 0, epoch: 'gen-A', gap: false, viewer: 'bob' });
  await chatPollMessages();
  assert.equal(scheduled[scheduled.length - 1], 2000, 'open panel polls at 2 s');

  respond({ error: 'unauthorized' }, 401);
  respond({ error: 'unauthorized' }, 401);
  respond({ error: 'boom' }, 500);
  respond({ error: 'boom' }, 500);
  respond({ error: 'boom' }, 500);
  respond({ error: 'boom' }, 500);
  for (let i = 0; i < 6; i++) await chatPollMessages();
  assert.deepEqual(scheduled.slice(-6), [4000, 8000, 16000, 32000, 60000, 60000], 'non-2xx must back off exponentially to the 60 s cap');
  assert.equal(chatLastSeq, 0);

  // A network failure counts as a failure too; a 2xx resets the backoff.
  await chatPollMessages(); // queue empty -> fetch throws
  assert.equal(scheduled[scheduled.length - 1], 60000);
  respond({ messages: [{ seq: 1, role: 'bot', text: 'back' }], next: 1, epoch: 'gen-A', gap: false, viewer: 'bob' });
  await chatPollMessages();
  assert.equal(scheduled[scheduled.length - 1], 2000, 'success must reset the backoff');
  assert.deepEqual(rendered.map(m => m.text), ['back']);

  // Hidden tab: nothing is scheduled; becoming visible polls immediately.
  const before = scheduled.length;
  document.hidden = true;
  document.listeners.visibilitychange();
  respond({ messages: [], next: 1, epoch: 'gen-A', gap: false, viewer: 'bob' });
  await chatPollMessages();
  assert.equal(scheduled.length, before, 'hidden tab must not schedule the next poll');
  const polls = urls.length;
  document.hidden = false;
  respond({ messages: [], next: 1, epoch: 'gen-A', gap: false, viewer: 'bob' });
  document.listeners.visibilitychange();
  await flush();
  assert.equal(urls.length, polls + 1, 'becoming visible must poll at once');
  assert.equal(scheduled[scheduled.length - 1], 2000);
`))
}

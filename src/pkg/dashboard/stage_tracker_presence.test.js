// The presence indicator on the Activity Monitor's waiting-on pill
// (hivecommons/hive#15051): a row waiting on a person says whether their
// clanker is online (dot), offline (square) or not registered (hollow ring),
// and what the hive does next (wait / adopt at …). The helpers are lifted
// out of index.html so this runs against the shipped source; the fixtures
// are the live /api/stage-tracker shape with the new `presence` field.
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');

function pick(re, what) {
  const m = html.match(re);
  assert.ok(m, what + ' not found in index.html');
  return m[0];
}

const src = [
  pick(/function agentNowAgo\(ms\) \{[\s\S]*?\n    \}\n/, 'agentNowAgo'),
  pick(/function stageTrackerAdoptionTip\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptionTip'),
  pick(/function stageTrackerAdoptOffered\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptOffered'),
  pick(/function stageTrackerAdoptionChipText\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptionChipText'),
  pick(/function stageTrackerAdoptionChipHtml\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerAdoptionChipHtml'),
  pick(/const STAGE_TRACKER_PRESENCE_STATES = \[[^\n]*\];/, 'STAGE_TRACKER_PRESENCE_STATES'),
  pick(/const STAGE_TRACKER_PRESENCE_WORDS = \{[^\n]*\};/, 'STAGE_TRACKER_PRESENCE_WORDS'),
  pick(/function stageTrackerPresenceOf\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceOf'),
  pick(/function stageTrackerPresenceGone\(p, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceGone'),
  pick(/function stageTrackerPresenceAdoptAt\(p, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceAdoptAt'),
  pick(/function stageTrackerPresenceTip\(item, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceTip'),
  pick(/function stageTrackerPresenceAria\(item, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceAria'),
  pick(/function stageTrackerPresenceDotHtml\(item, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceDotHtml'),
  pick(/function stageTrackerPresenceInPillHtml\(item, text\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceInPillHtml'),
  pick(/function stageTrackerPresenceChipText\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceChipText'),
  pick(/function stageTrackerPresenceChipHtml\(item\) \{[\s\S]*?\n    \}\n/, 'stageTrackerPresenceChipHtml'),
].join('\n');

// The clock the tooltips are rendered against, and the stubs the lifted
// helpers lean on (HTML escaping, the display-time-zone clock).
const NOW = Date.parse('2026-10-28T14:32:00Z');
const stubs = `
  const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  const agentNowClockTime = (ms) => new Date(ms).toISOString().slice(11, 16) + ' UTC';
  const stageTrackerDecisionOf = () => null;
  Date.now = () => ${NOW};
`;
const api = new Function(stubs + src + `; return { of: stageTrackerPresenceOf, tip: stageTrackerPresenceTip, aria: stageTrackerPresenceAria, dot: stageTrackerPresenceDotHtml,
  inPill: stageTrackerPresenceInPillHtml, chipText: stageTrackerPresenceChipText, chip: stageTrackerPresenceChipHtml, adoptionChip: stageTrackerAdoptionChipHtml };`)();

const iso = ms => new Date(ms).toISOString();
const MIN = 60000, HOUR = 60 * MIN;

// hivecommons/hive#14989 as the tracker emits it with @Danathar's clanker connected.
const online14989 = { repo: 'hivecommons/hive', number: 14989, kind: 'pr', stage: 71, author: 'Danathar', author_kind: 'clanker',
  presence: { login: 'Danathar', state: 'online', kind: 'clanker', since: iso(NOW - 3 * HOUR), last_seen: iso(NOW - 2 * MIN), next: 'wait' } };
// hivecommons/hive#14524 at 072, nudged, @hanthor's clanker gone 26h, adoption due in 38m.
const offline14524 = { repo: 'hivecommons/hive', number: 14524, kind: 'pr', stage: 72, author: 'hanthor', author_kind: 'clanker',
  adoption: { author: 'hanthor', due: false, since: iso(NOW - 26 * HOUR), nudges: 2, author_online: false, author_registered: true, policy: 'adopt', text: 'waiting on @hanthor (offline 26h, 2 nudges)' },
  presence: { login: 'hanthor', state: 'offline', kind: 'clanker', since: iso(NOW - 26 * HOUR), last_seen: iso(NOW - 26 * HOUR), next: 'adopt', adopt_at: iso(NOW + 38 * MIN) } };
// A human's PR at 071: nobody's clanker to wait for.
const human = { repo: 'hivecommons/hive', number: 15000, kind: 'pr', stage: 71, author: 'zoe', author_kind: 'human',
  presence: { login: 'zoe', state: 'unknown', kind: 'human', since: iso(NOW - 40 * MIN), next: 'wait' } };

test('online clanker: filled dot, "hive is waiting for them", last seen in words', () => {
  assert.strictEqual(api.tip(online14989, NOW), "@Danathar's clanker: online, last seen 2 min ago — hive is waiting for them");
  const dot = api.dot(online14989, NOW);
  assert.match(dot, /class="ham-presence ham-presence-online"/);
  assert.match(dot, /role="img"/);
  assert.match(dot, /aria-label="@Danathar: clanker online — hive is waiting for them"/);
  assert.match(dot, /data-ham-presence="online"/);
  // The online grace still projects an adopt-at: the tooltip says so after the wait.
  const graced = Object.assign({}, online14989, { presence: Object.assign({}, online14989.presence, { adopt_at: iso(NOW + 2 * HOUR) }) });
  assert.strictEqual(api.tip(graced, NOW), "@Danathar's clanker: online, last seen 2 min ago — hive is waiting for them; adopts at 16:32 UTC (in 2h) if nothing moves");
});

test('offline clanker: filled square, offline span, adopt-at clock with countdown', () => {
  assert.strictEqual(api.tip(offline14524, NOW), "@hanthor's clanker: offline 26h — hive will adopt at 15:10 UTC (in 38m)");
  assert.match(api.dot(offline14524, NOW), /ham-presence-offline/);
  assert.match(api.dot(offline14524, NOW), /aria-label="@hanthor: clanker offline — hive will adopt at 15:10 UTC \(in 38m\)"/);
  // Past due.
  const due = Object.assign({}, offline14524, { presence: Object.assign({}, offline14524.presence, { adopt_at: iso(NOW - MIN) }) });
  assert.strictEqual(api.tip(due, NOW), "@hanthor's clanker: offline 26h — hive will adopt due now");
  // Held (maintainer_policy ask): no adopt_at, next=wait — the dot does not promise an adoption the text denies.
  const held = Object.assign({}, offline14524, { presence: Object.assign({}, offline14524.presence, { next: 'wait', adopt_at: '' }) });
  assert.strictEqual(api.tip(held, NOW), "@hanthor's clanker: offline 26h — hive is waiting for them");
  // Long absences read in days.
  const days = Object.assign({}, offline14524, { presence: Object.assign({}, offline14524.presence, { since: iso(NOW - 80 * HOUR), last_seen: iso(NOW - 80 * HOUR) }) });
  assert.match(api.tip(days, NOW), /offline 3d — /);
});

test('human without a clanker: hollow ring, "waiting for a human reply", pill says human · no clanker', () => {
  assert.strictEqual(api.tip(human, NOW), '@zoe: no clanker registered — waiting for a human reply');
  const dot = api.dot(human, NOW);
  assert.match(dot, /ham-presence-unknown/);
  assert.match(dot, /aria-label="@zoe: human, no clanker — waiting for a human reply"/);
  assert.strictEqual(api.chipText(human), 'waiting on @zoe (human · no clanker)');
  const chip = api.chip(human);
  assert.match(chip, /^<span class="ham-badge ham-badge-waiting" title="@zoe: no clanker registered — waiting for a human reply" aria-label="Waiting on — @zoe: no clanker registered — waiting for a human reply">/);
  assert.match(chip, /waiting on <span class="ham-presence ham-presence-unknown"[^>]*><\/span>@zoe \(human · no clanker\)<\/span>$/);
  // An unregistered clanker (another hive's PR) is unknown too, but adoption's clock runs.
  const foreign = { presence: { login: 'bot-x', state: 'unknown', kind: 'clanker', since: iso(NOW - 10 * MIN), next: 'adopt', adopt_at: iso(NOW + 5 * MIN) } };
  assert.strictEqual(api.tip(foreign, NOW), '@bot-x: no clanker registered here — hive will adopt at 14:37 UTC (in 5m)');
});

test('the indicator sits inside the adoption pill just before the @login and its line joins the tooltip', () => {
  const chip = api.adoptionChip(offline14524);
  assert.match(chip, /^<span class="ham-badge ham-badge-adoption" title="waiting on @hanthor \(offline 26h, 2 nudges\) · @hanthor's clanker: offline 26h — hive will adopt at 15:10 UTC \(in 38m\)"/);
  assert.match(chip, /aria-label="Adoption — waiting on @hanthor \(offline 26h, 2 nudges\) · @hanthor's clanker: offline 26h/);
  assert.match(chip, />waiting on <span class="ham-presence ham-presence-offline"[^>]*><\/span>@hanthor<\/span>$/);
  // A 071 row has no adoption line: the muted waiting pill carries the dot instead, never both.
  assert.strictEqual(api.chip(offline14524), '', 'no second pill beside the adoption one');
  assert.match(api.chip(online14989), /^<span class="ham-badge ham-badge-waiting" [^>]*>waiting on <span class="ham-presence ham-presence-online"[^>]*><\/span>@Danathar<\/span>$/);
  // Text naming nobody gets the indicator in front.
  assert.match(api.inPill(online14989, 'adopt?'), /^<span class="ham-presence ham-presence-online"[^>]*><\/span>adopt\?$/);
});

test('rows without presence, or with a malformed one, render as before', () => {
  assert.strictEqual(api.of({ kind: 'pr' }), null);
  assert.strictEqual(api.of({ presence: {} }), null);
  assert.strictEqual(api.of({ presence: { state: 'online' } }), null, 'a login is required');
  assert.strictEqual(api.of(null), null);
  assert.strictEqual(api.tip({ kind: 'pr' }, NOW), '');
  assert.strictEqual(api.dot({}, NOW), '');
  assert.strictEqual(api.chip({ kind: 'pr', stage: 71 }), '');
  assert.strictEqual(api.inPill({}, 'waiting on @x'), 'waiting on @x');
  // Unknown words fall back safely.
  const odd = api.of({ presence: { login: 'q', state: 'asleep', kind: 'robot', next: 'later' } });
  assert.deepStrictEqual([odd.state, odd.kind, odd.next], ['unknown', 'human', 'wait']);
  // The adoption pill without presence is unchanged.
  const bare = { kind: 'pr', adoption: { author: 'hanthor', text: 'waiting on @hanthor (online, 1 nudge)' } };
  assert.strictEqual(api.adoptionChip(bare), '<span class="ham-badge ham-badge-adoption" title="waiting on @hanthor (online, 1 nudge)" aria-label="Adoption — waiting on @hanthor (online, 1 nudge)">waiting on @hanthor</span>');
});

test('style ratchet: the indicator uses classes and tokens, no inline styles; reduced motion stills the breathe', () => {
  assert.ok(!/ham-presence[^\n]*style="/.test(html), 'no inline style on the indicator');
  assert.match(html, /\.ham-presence-online \{[^\n]*var\(--cue-decision\)/);
  assert.match(html, /\.ham-presence-offline \{[^\n]*var\(--status-error\)/);
  assert.match(html, /\.ham-presence-unknown \{[^\n]*border: var\(--line-width\) solid var\(--text-faint\)[^\n]*background: transparent/);
  assert.match(html, /@media \(prefers-reduced-motion: reduce\) \{ \.ham-presence-online \{ animation: none; \} \}/);
});

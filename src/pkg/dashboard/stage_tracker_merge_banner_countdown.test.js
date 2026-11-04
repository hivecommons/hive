// Activity Monitor "#N merged" banners: the auto-dismiss goes through
// toastCountdownBar, so the banner drains the same .toast-bar every other
// timed toast shows (CSS keys on .toast-bar + --toast-ttl), hover holds it,
// and the manual × dismiss clears the pending timer. The functions are
// lifted straight out of index.html so this runs against the shipped source.
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

// A minimal DOM: enough for toastCountdownBar and the banner builder.
function makeDom() {
  const timers = [];
  class El {
    constructor(tag) {
      this.tagName = tag; this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {};
      this.style = { props: {}, setProperty(k, v) { this.props[k] = v; } };
      this.className = ''; this.innerHTML = ''; this.id = '';
    }
    get classList() { const self = this; return { add(c) { self.className += ' ' + c; }, remove() {}, contains(c) { return self.className.split(' ').includes(c); } }; }
    appendChild(c) { if (c.parentNode) c.parentNode.removeChild(c); c.parentNode = this; this.children.push(c); return c; }
    removeChild(c) { this.children = this.children.filter(x => x !== c); c.parentNode = null; }
    remove() { if (this.parentNode) this.parentNode.removeChild(this); }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    getAttribute(k) { return this.attrs[k] === undefined ? null : this.attrs[k]; }
    addEventListener(n, fn) { (this.listeners[n] = this.listeners[n] || []).push(fn); }
    fire(n, e) { for (const fn of this.listeners[n] || []) fn(e || {}); }
    matches() { return false; }
    contains(x) { return x === this || this.children.some(c => c.contains(x)); }
    querySelector() { return null; }
    closest(sel) { for (let n = this; n; n = n.parentNode) if (sel === '.merge-banner' && n.className && n.className.split(' ').includes('merge-banner')) return n; return null; }
  }
  const body = new El('body');
  const byId = {};
  const document = {
    body, activeElement: null,
    createElement: t => new El(t),
    getElementById: id => byId[id] || null,
  };
  const origAppend = body.appendChild.bind(body);
  body.appendChild = c => { if (c.id) byId[c.id] = c; return origAppend(c); };
  const setTimeout = (fn, ms) => { const t = { fn, ms, cleared: false }; timers.push(t); return t; };
  const clearTimeout = t => { if (t) t.cleared = true; };
  return { document, timers, setTimeout, clearTimeout, El };
}

const BANNER_MS = 30000;
const src = [
  'const window = { _primaryRepo: "hivecommons/hive" };',
  'const _stageTrackerFilter = { repo: "" };',
  'function esc(s) { return typeof s === "string" ? s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;") : ""; }',
  'const _stageTrackerMerged = { server: { recent: [] } };',
  'const _stageTrackerPRTitle = {};',
  'function stageTrackerMergedByText() { return ""; }',
  'function stageTrackerReducedMotion() { return true; }',
  'function stageTrackerAfter(el, ev, ms, fn) { fn(); }',
  `function stageTrackerMergeBannerMs() { return ${BANNER_MS}; }`,
  pick(/const STAGE_TRACKER_MERGE_BANNER_MAX = [^\n]*\n/, 'STAGE_TRACKER_MERGE_BANNER_MAX'),
  pick(/function toastCountdownBar\(el, ms, onExpire\) \{[\s\S]*?\n    \}\n/, 'toastCountdownBar'),
  pick(/function toastCountdownCancel\(el\) \{[\s\S]*?\n    \}\n/, 'toastCountdownCancel'),
  pick(/function stageTrackerMergeBannerNumber\(b\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMergeBannerNumber'),
  pick(/function stageTrackerMergeBannerRepo\(b\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMergeBannerRepo'),
  pick(/const STAGE_TRACKER_REPO_TINTS = [^\n]*\n/, 'STAGE_TRACKER_REPO_TINTS'),
  pick(/function stageTrackerRepoTint\(repo\) \{[\s\S]*?\n    \}\n/, 'stageTrackerRepoTint'),
  pick(/function stageTrackerRepoShort\(repo\) \{[\s\S]*?\n    \}\n/, 'stageTrackerRepoShort'),
  pick(/function stageTrackerIsPrimary\(repo, primary\) \{[\s\S]*?\n    \}\n/, 'stageTrackerIsPrimary'),
  pick(/function stageTrackerRepoChipHtml\(repo, ctx\) \{[\s\S]*?\n    \}\n/, 'stageTrackerRepoChipHtml'),
  pick(/function stageTrackerPrimaryRepo\(\) \{[^\n]*\n/, 'stageTrackerPrimaryRepo'),
  pick(/function stageTrackerMergerText\(by\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMergerText'),
  pick(/function stageTrackerMergedByOf\(key, d\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMergedByOf'),
  pick(/function stageTrackerMergeBannerText\(b, nowMs\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMergeBannerText'),
  pick(/function stageTrackerMergeBanner\(b\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMergeBanner'),
  pick(/function stageTrackerMergeBannerDismiss\(el\) \{[\s\S]*?\n    \}\n/, 'stageTrackerMergeBannerDismiss'),
  pick(/function hamBannerDismiss\(btn\) \{[\s\S]*?\n    \}\n/, 'hamBannerDismiss'),
].join('\n');

function load(tracker) {
  const dom = makeDom();
  const api = new Function('document', 'setTimeout', 'clearTimeout', '_stageTracker', src + '; return { banner: stageTrackerMergeBanner, dismiss: stageTrackerMergeBannerDismiss, hamBannerDismiss, merged: _stageTrackerMerged, filter: _stageTrackerFilter, chip: stageTrackerRepoChipHtml, mergerText: stageTrackerMergerText, mergedByOf: stageTrackerMergedByOf, bannerText: stageTrackerMergeBannerText };')(dom.document, dom.setTimeout, dom.clearTimeout, tracker || null);
  return Object.assign(dom, api);
}

const merge = { key: 'hivecommons/hive/pr#15834', repo: 'hivecommons/hive', number: 15834, title: 'fix the thing', at: Date.now() };

test('merge banner carries the countdown bar and --toast-ttl the CSS drains over', () => {
  const d = load();
  const el = d.banner(merge);
  const bar = el.children.find(c => c.className === 'toast-bar');
  assert.ok(bar, 'a .toast-bar child is appended to the banner');
  assert.strictEqual(bar.getAttribute('aria-hidden'), 'true');
  assert.strictEqual(el.style.props['--toast-ttl'], BANNER_MS + 'ms', 'bar duration equals the banner duration');
  assert.ok(el._countdown, 'countdown state is armed');
  assert.strictEqual(d.timers.length, 1);
  assert.strictEqual(d.timers[0].ms, BANNER_MS, 'dismiss timer uses stageTrackerMergeBannerMs()');
  assert.strictEqual(el._timer, undefined, 'no bare setTimeout handle remains');
});

test('timer expiry removes the banner', () => {
  const d = load();
  const el = d.banner(merge);
  assert.ok(el.parentNode);
  d.timers[0].fn();
  assert.strictEqual(el.parentNode, null, 'banner left the stack when the countdown fired');
});

test('hover holds the countdown and the bar together; leaving re-arms the remainder', () => {
  const d = load();
  const el = d.banner(merge);
  const bar = el.children.find(c => c.className === 'toast-bar');
  assert.strictEqual(d.timers.length, 1);
  el.fire('mouseenter');
  assert.ok(d.timers[0].cleared, 'timer cleared on hover');
  assert.strictEqual(bar.style.animationPlayState, 'paused');
  el.fire('mouseleave');
  assert.strictEqual(d.timers.length, 2, 'a fresh timer is armed on leave');
  assert.ok(d.timers[1].ms <= BANNER_MS && d.timers[1].ms > 0, 'remainder re-armed');
  assert.strictEqual(bar.style.animationPlayState, 'running');
});

test('manual × dismiss clears the pending timer and the bar', () => {
  const d = load();
  const el = d.banner(merge);
  const close = new d.El('button');
  el.appendChild(close);
  d.hamBannerDismiss(close);
  assert.strictEqual(el.parentNode, null, 'banner removed');
  assert.ok(d.timers[0].cleared, 'auto-dismiss timer cleared so it cannot fire stale');
  assert.strictEqual(el._countdown, null);
  assert.ok(!el.children.some(c => c.className === 'toast-bar'), 'bar removed with the countdown');
});

test('merge banners reuse tracker chips and preserve repo-scoped actions for colliding PR numbers', () => {
  const d = load();
  d.merged.server.recent = [
    { repo: 'other/hive', number: 528, title: 'other merge', url: 'https://github.com/other/hive/pull/528', lane: 'other lane' },
    { repo: 'hivecommons/hive', number: 528, title: 'actual merge', url: 'https://github.com/hivecommons/hive/pull/528', lane: 'actual lane' },
    { repo: 'acme/console', number: 528, title: 'console merge', url: 'https://github.com/acme/console/pull/528' },
  ];
  d.filter.repo = 'other/hive';
  for (const repo of ['hivecommons/hive', 'other/hive', 'acme/console']) {
    const rowKey = repo + '/issue#42';
    const el = d.banner({ key: repo + '/pr#528', repo, number: 528, rowKey });
    const chip = d.chip(repo, { repoChips: { primary: 'hivecommons/hive', active: d.filter.repo } });
    assert.ok(el.innerHTML.includes(chip + ' <span class="merge-banner-num">#528</span> merged'), 'exact existing chip markup');
    assert.strictEqual(el.getAttribute('data-ham-row'), rowKey);
    assert.ok(el.innerHTML.includes(`data-action="hamBannerShowRow" data-arg0="${rowKey}" data-stop="1">Show row`));
    assert.ok(el.innerHTML.includes(`href="https://github.com/${repo}/pull/528" target="_blank" rel="noopener">Open on GitHub`));
    assert.ok(el.innerHTML.includes('data-action="hamMergedOpen" data-stop="1">All merges'));
    const expected = d.merged.server.recent.find(e => e.repo === repo);
    assert.ok(el.innerHTML.includes(expected.title), 'metadata belongs to actual event repo');
    for (const other of d.merged.server.recent.filter(e => e.repo !== repo)) {
      assert.ok(!el.innerHTML.includes(other.url), 'no other repo URL borrowed');
      assert.ok(!el.innerHTML.includes(other.title), 'no other repo title borrowed');
    }
  }
});

test('banner identity comes from event fields, never the primary repo or current filter', () => {
  const d = load();
  d.filter.repo = 'unrelated/selected';
  const events = [
    { repo: 'acme/console', number: 528 },
    { pr: { repo: 'acme/console', number: 528 } },
    { key: 'acme/console/pr#528' },
    { repo: 'console', key: 'acme/console/pr#528' },
    { url: 'https://github.com/acme/console/pull/528' },
    { pr: { url: 'https://github.com/acme/console/pull/528', number: 528 } },
  ];
  for (const event of events) {
    const el = d.banner(event);
    assert.ok(el.innerHTML.includes('data-action="hamRepoChip" data-arg0="acme/console"'));
    assert.ok(el.innerHTML.includes('href="https://github.com/acme/console/pull/528"'));
    assert.ok(!el.innerHTML.includes('hivecommons/hive'));
    assert.ok(!el.innerHTML.includes('unrelated/selected'));
  }
  const unknown = d.banner({ number: 528 });
  assert.ok(!unknown.innerHTML.includes('ham-repo-chip'), 'no repository identity means no chip');
  assert.ok(!unknown.innerHTML.includes('Open on GitHub'), 'no guessed repository link');
  assert.ok(unknown.innerHTML.includes('All merges'), 'merged list remains available');
  const short = d.banner({ repo: 'console', number: 528 });
  assert.ok(short.innerHTML.includes('data-arg0="console"'), 'explicit short audit identity is retained');
  assert.ok(!short.innerHTML.includes('Open on GitHub'), 'short identity does not invent an owner');
});

test('merge repo chips use the existing escaping and preview behavior', () => {
  const d = load();
  const repo = 'acme/<console"&>';
  const el = d.banner({ repo, number: 528, preview: true, rowKey: repo + '/pr#528' });
  assert.ok(el.innerHTML.includes(d.chip(repo, { repoChips: { primary: 'hivecommons/hive', active: '' } })));
  assert.ok(el.innerHTML.includes('data-arg0="acme/&lt;console&quot;&amp;&gt;"'));
  assert.ok(!el.innerHTML.includes('<console'));
  assert.ok(!el.innerHTML.includes('Open on GitHub'), 'preview does not invent a GitHub URL');
  assert.ok(el.innerHTML.includes('(test)'));
});

// hivecommons/hive#16658: a PR merged by anyone celebrates, and the head
// names the merger — "merged by @login" for a human, "merged by 🐝 hive"
// for the hive's own merge — so an external merge reads like the hive's.
test('#16658: merger text names a human by @login and the hive by 🐝', () => {
  const d = load();
  assert.strictEqual(d.mergerText('clubanderson'), '@clubanderson');
  assert.strictEqual(d.mergerText('hive'), '🐝 hive');
  assert.strictEqual(d.mergerText(''), '');
  assert.strictEqual(d.mergerText(undefined), '');
  assert.strictEqual(d.mergerText('  '), '');
});

test('#16658: banner head reads "#N merged by <merger>" and falls back to plain "merged" when unknown', () => {
  const d = load();
  const at = Date.now();
  const human = d.bannerText({ key: 'hivecommons/hive/pr#16644', repo: 'hivecommons/hive', number: 16644, merged_by: 'clubanderson', at }, at);
  assert.strictEqual(human.head, '#16644 merged by @clubanderson');
  assert.strictEqual(human.merger, '@clubanderson');
  const hive = d.bannerText({ key: 'hivecommons/hive/pr#16645', repo: 'hivecommons/hive', number: 16645, merged_by: 'hive', at }, at);
  assert.strictEqual(hive.head, '#16645 merged by 🐝 hive');
  const unknown = d.bannerText({ key: 'hivecommons/hive/pr#1', repo: 'hivecommons/hive', number: 1, at }, at);
  assert.strictEqual(unknown.head, '#1 merged');
  assert.strictEqual(unknown.merger, '');
  const preview = d.bannerText({ key: 'hivecommons/hive/pr#2', repo: 'hivecommons/hive', number: 2, merged_by: 'hive', preview: true, at }, at);
  assert.strictEqual(preview.head, '#2 merged by 🐝 hive (test)');
});

test('#16658: mergedByOf resolves the merger from recently_merged, then the ledger, then the held terminal fact', () => {
  const tracker = {
    recently_merged: [{ repo: 'hivecommons/hive', number: 16644, merged_by: 'clubanderson' }],
    merged: { recent: [{ repo: 'hive', number: 16645, merged_by: 'hive' }, { repo: 'acme/other', number: 16644, merged_by: 'someone-else' }] },
  };
  const d = load(tracker);
  assert.strictEqual(d.mergedByOf('hivecommons/hive/pr#16644', tracker), 'clubanderson', 'audit/ledger union wins, repo-scoped');
  assert.strictEqual(d.mergedByOf('hivecommons/hive/pr#16645', tracker), 'hive', 'short repo name in the ledger matches the key');
  assert.strictEqual(d.mergedByOf('acme/other/pr#16644', tracker), 'someone-else', 'colliding numbers stay repo-scoped');
  assert.strictEqual(d.mergedByOf('hivecommons/hive/pr#9999', tracker), '', 'unknown PR is empty, never a guess');
  assert.strictEqual(d.mergedByOf('', tracker), '');
  assert.strictEqual(d.mergedByOf('hivecommons/hive/pr#16644', null), '', 'no payload, no held fact → empty');
});

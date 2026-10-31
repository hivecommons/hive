// Activity Monitor display playlists (hivecommons/hive#15932): in display
// mode, repeated `am.view` params (each a URL-encoded `am.*` query string)
// rotate the in-memory tracker view every `interval` (Ns / Nm, 10 s–1 h,
// default 60 s). The index derives from wall-clock time, so a reload or an
// SSE reconnect resumes the same view, and rotation never writes
// localStorage or navigates. The helpers are lifted out of index.html so this
// runs against the shipped source. Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const pick = (begin, end) => {
  const m = html.match(new RegExp('// ' + begin + '\\n([\\s\\S]*?)// ' + end));
  assert.ok(m, begin + ' not found');
  return m[1];
};
const constLine = name => {
  const m = html.match(new RegExp('^\\s*const ' + name + ' = .*$', 'm'));
  assert.ok(m, name + ' not found');
  return m[0];
};
const fnSrc = name => {
  const start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};

const src = [
  constLine('STAGE_TRACKER_FILTER_KEY'), constLine('STAGE_TRACKER_PAGE_SIZES'), constLine('STAGE_TRACKER_DEFAULT_PAGE_SIZE'),
  constLine('STAGE_TRACKER_AGE_MS'), constLine('STAGE_TRACKER_SORTS'), constLine('STAGE_TRACKER_ACTED_FILTERS'),
  constLine('STAGE_TRACKER_GROUP_KEY'), constLine('STAGE_TRACKER_GROUPS'), constLine('STAGE_TRACKER_GROUP_DEFAULT'),
  fnSrc('stageTrackerDefaultFilter'), fnSrc('stageTrackerNormalizeFilter'),
  pick('tracker url view helpers begin', 'tracker url view helpers end'),
  pick('tracker display playlist helpers begin', 'tracker display playlist helpers end'),
  'var _stageTrackerFilter = stageTrackerDefaultFilter();',
  'var _stageTrackerGroup = STAGE_TRACKER_GROUP_DEFAULT;',
  'var _stageTracker = null;',
  'var _stageTrackerUrlView;',
  fnSrc('stageTrackerUrlView'), fnSrc('stageTrackerLoadFilter'), fnSrc('stageTrackerSaveFilter'),
  fnSrc('stageTrackerGroupSetting'),
  constLine('STAGE_TRACKER_PLAYLIST_TICK_MS'),
  'var _stageTrackerPlaylist = null;',
  'var _stageTrackerPlaylistIndex = -1;',
  fnSrc('stageTrackerDisplayMode'),
  fnSrc('stageTrackerPlaylistApply'), fnSrc('stageTrackerPlaylistRenderProgress'),
  fnSrc('stageTrackerPlaylistTick'), fnSrc('stageTrackerPlaylistInit'),
  'function renderStageTracker() { renders.push(Object.assign({}, _stageTrackerFilter, { group: _stageTrackerGroup })); }',
  'return { stageTrackerParsePlaylist, stageTrackerPlaylistIndex, stageTrackerPlaylistIntervalMs, stageTrackerPlaylistRemainingMs, stageTrackerPlaylistNextLabel, stageTrackerPlaylistInit, stageTrackerPlaylistTick, stageTrackerLoadFilter, stageTrackerGroupSetting, filter: () => _stageTrackerFilter, group: () => _stageTrackerGroup, setData: d => { _stageTracker = d; } };',
].join('\n');

function load(href, display) {
  const writes = [];
  const renders = [];
  const navigations = [];
  const intervals = [];
  const appended = [];
  const localStorage = { getItem: () => null, setItem(k) { writes.push(k); } };
  const fakeEl = () => {
    const kids = {};
    return {
      attrs: {}, props: {},
      setAttribute(k, v) { this.attrs[k] = v; },
      set innerHTML(v) { this._html = v; kids.bar = { style: { props: {}, setProperty(k, val) { this.props[k] = val; } } }; kids.next = { textContent: '' }; },
      get innerHTML() { return this._html; },
      querySelector(sel) { return sel === '.ham-playlist-bar' ? kids.bar : sel === '.ham-playlist-next' ? kids.next : null; },
      kids,
    };
  };
  const document = {
    createElement: () => fakeEl(),
    getElementById: id => appended.find(e => e.id === id) || null,
    body: { appendChild(el) { appended.push(el); } },
  };
  const window = { location: { href, assign(u) { navigations.push(u); } }, hiveDisplayMode: () => display !== false };
  // The shipped code registers its tick with the dashboard's shared
  // dashboardSetInterval (paused while hidden), never a bare setInterval.
  const dashboardSetInterval = (key, fn, ms) => { intervals.push({ key, fn, ms }); return { fn, ms, timer: intervals.length }; };
  const setInterval = () => { throw new Error('bare setInterval is not allowed; use dashboardSetInterval'); };
  const api = new Function('window', 'document', 'localStorage', 'URL', 'URLSearchParams', 'setInterval', 'dashboardSetInterval', 'renders', src)(window, document, localStorage, URL, URLSearchParams, setInterval, dashboardSetInterval, renders);
  return { api, writes, renders, navigations, intervals, appended };
}

const v = q => 'am.view=' + encodeURIComponent(q);
const BASE = 'https://hive.example/?display=1&';

test('a playlist parses repeated am.view params into tracker views and the interval', () => {
  const { api } = load('https://hive.example/');
  const pl = api.stageTrackerParsePlaylist(BASE + v('am.needsHuman=1') + '&' + v('am.kind=pr&am.repo=hivecommons/hive') + '&' + v('am.group=repo') + '&interval=45s');
  assert.strictEqual(pl.views.length, 3);
  assert.strictEqual(pl.views[0].filter.needsHuman, true);
  assert.strictEqual(pl.views[1].filter.kind, 'pr');
  assert.strictEqual(pl.views[1].filter.repo, 'hivecommons/hive');
  assert.strictEqual(pl.views[1].group, 'none');
  assert.strictEqual(pl.views[2].group, 'repo');
  assert.strictEqual(pl.intervalMs, 45000);
  // The example from the issue, already percent-encoded once.
  const ex = api.stageTrackerParsePlaylist(BASE + 'am.view=am.kind%3Dpr%26am.repo%3Dhivecommons%2Fhive&am.view=am.kind%3Dissue');
  assert.deepStrictEqual(ex.views.map(x => x.filter.kind), ['pr', 'issue']);
  assert.strictEqual(ex.views[0].filter.repo, 'hivecommons/hive');
});

test('interval accepts Ns or Nm, clamps to 10 s–1 h and defaults to 60 s', () => {
  const { api } = load('https://hive.example/');
  const ms = api.stageTrackerPlaylistIntervalMs;
  assert.strictEqual(ms('30s'), 30000);
  assert.strictEqual(ms('2m'), 120000);
  assert.strictEqual(ms('5s'), 10000, 'clamped up to 10 s');
  assert.strictEqual(ms('0s'), 10000);
  assert.strictEqual(ms('90m'), 3600000, 'clamped down to 1 h');
  assert.strictEqual(ms('99999s'), 3600000);
  for (const bad of [null, undefined, '', '60', 'abc', '1h', '-5s', '1.5m', '10 seconds']) assert.strictEqual(ms(bad), 60000, String(bad));
  const { api: a2 } = load('https://hive.example/');
  assert.strictEqual(a2.stageTrackerParsePlaylist(BASE + v('am.kind=pr') + '&' + v('am.kind=issue')).intervalMs, 60000, 'default');
  assert.strictEqual(a2.stageTrackerParsePlaylist(BASE + v('am.kind=pr') + '&' + v('am.kind=issue') + '&interval=1s').intervalMs, 10000);
});

test('invalid views are skipped: no known am.* key, a nested am.view, or garbage', () => {
  const { api } = load('https://hive.example/');
  const pl = api.stageTrackerParsePlaylist(BASE + [v('am.kind=pr'), v(''), v('kind=pr'), v('am.bogus=1'), v('am.view=am.kind%3Dpr'), 'am.view=%%%', v('am.kind=issue')].join('&'));
  assert.deepStrictEqual(pl.views.map(x => x.filter.kind), ['pr', 'issue']);
  assert.deepStrictEqual(api.stageTrackerParsePlaylist('not a url'), { views: [], intervalMs: 60000 });
});

test('the index follows wall-clock time, so a reload or SSE reconnect resumes the same view', () => {
  const { api } = load('https://hive.example/');
  const idx = api.stageTrackerPlaylistIndex;
  const I = 60000;
  assert.strictEqual(idx(0, 0, I, 2), 0);
  assert.strictEqual(idx(0, I - 1, I, 2), 0);
  assert.strictEqual(idx(0, I, I, 2), 1);
  assert.strictEqual(idx(0, 2 * I, I, 2), 0);
  assert.strictEqual(idx(0, 7 * I + 5, I, 3), 1);
  // A reconnect (or reload) at any instant computes the same index as a tab
  // that never dropped: there is no per-session counter.
  const t = Date.UTC(2026, 9, 31, 4, 52, 19);
  for (const at of [t, t + 30000, t + 61000, t + 3 * I]) assert.strictEqual(idx(0, at, I, 2), Math.floor(at / I) % 2);
  assert.strictEqual(idx(0, t, I, 1), 0, 'one view never rotates');
  assert.strictEqual(idx(0, t, I, 0), 0);
  assert.strictEqual(idx(0, t, 0, 2), 0);
  assert.strictEqual(idx(0, -1, I, 2), 1, 'never negative');
  assert.strictEqual(api.stageTrackerPlaylistRemainingMs(0, 15000, I), 45000);
  assert.strictEqual(api.stageTrackerPlaylistRemainingMs(0, I, I), I);
  assert.strictEqual(api.stageTrackerPlaylistNextLabel(45000), 'next in 45s');
  assert.strictEqual(api.stageTrackerPlaylistNextLabel(1), 'next in 1s');
});

test('a two-view playlist alternates in memory at the interval, survives a refetch after reconnect, and never writes localStorage or navigates', () => {
  const href = BASE + v('am.kind=pr&am.repo=hivecommons/hive') + '&' + v('am.kind=issue&am.group=repo') + '&interval=10s';
  const env = load(href);
  const { api } = env;
  api.setData({ items: [] });
  const pl = api.stageTrackerPlaylistInit();
  assert.ok(pl && pl.views.length === 2);
  assert.strictEqual(env.intervals.length, 1, 'one ticker');
  assert.strictEqual(env.appended.length, 1, 'progress bar appended');
  // Init ticks once at Date.now(), so which view it rendered depends on the
  // wall clock: when it already showed view 0 the first explicit tick below
  // is not a swap and renders nothing.
  assert.strictEqual(env.renders.length, 1, 'init rendered the current view');
  const initKind = env.renders[0].kind;
  env.renders.length = 0;
  const t0 = 1000 * 10000; // a multiple of the interval → view 0
  assert.strictEqual(api.stageTrackerPlaylistTick(t0), 0);
  assert.strictEqual(api.filter().kind, 'pr');
  assert.strictEqual(api.filter().repo, 'hivecommons/hive');
  assert.strictEqual(api.group(), 'none');
  assert.strictEqual(api.stageTrackerPlaylistTick(t0 + 10000), 1);
  assert.strictEqual(api.filter().kind, 'issue');
  assert.strictEqual(api.group(), 'repo');
  // An SSE drop + reconnect makes the first fetch reload the filter from the
  // URL view; it must still read the rotated view, not view 0 or defaults.
  api.stageTrackerLoadFilter();
  assert.strictEqual(api.filter().kind, 'issue');
  assert.strictEqual(api.stageTrackerGroupSetting(), 'repo');
  assert.strictEqual(api.stageTrackerPlaylistTick(t0 + 20000), 0);
  assert.strictEqual(api.filter().kind, 'pr');
  assert.strictEqual(api.stageTrackerPlaylistTick(t0 + 25000), 0, 'no swap mid-interval');
  assert.deepStrictEqual(env.renders.map(r => r.kind), (initKind === 'pr' ? [] : ['pr']).concat(['issue', 'pr']), 're-rendered once per swap, from memory');
  assert.deepStrictEqual(env.writes, [], 'no localStorage writes');
  assert.deepStrictEqual(env.navigations, [], 'never navigates');
  const bar = env.appended[0].kids;
  assert.strictEqual(bar.bar.style.props['--ham-playlist-left'], '0.5000');
  assert.strictEqual(bar.next.textContent, '1/2 · next in 5s');
});

test('no rotation without am.view, with fewer than two valid views, or outside display mode', () => {
  for (const [href, display] of [
    ['https://hive.example/?display=1&am.kind=pr', true],
    [BASE + v('am.kind=pr'), true],
    [BASE + v('am.kind=pr') + '&' + v('nothing=1'), true],
    [BASE.replace('display=1&', '') + v('am.kind=pr') + '&' + v('am.kind=issue'), false],
  ]) {
    const env = load(href, display);
    const before = JSON.stringify(env.api.filter());
    assert.strictEqual(env.api.stageTrackerPlaylistInit(), null, href);
    assert.strictEqual(env.api.stageTrackerPlaylistTick(Date.now()), -1);
    assert.strictEqual(JSON.stringify(env.api.filter()), before, 'filter untouched');
    assert.deepStrictEqual(env.intervals, []);
    assert.deepStrictEqual(env.appended, []);
    assert.deepStrictEqual(env.renders, []);
  }
});

test('playlist wiring: init runs on DOMContentLoaded, the reduced-motion label replaces the bar, and nothing is persisted', () => {
  assert.ok(html.includes('stageTrackerDisplayControlsInit();\n      stageTrackerPlaylistInit();\n    });'));
  assert.ok(html.includes('.ham-playlist-bar { display: block; height: 100%; background: var(--blue); transform-origin: left center; transform: scaleX(var(--ham-playlist-left, 1)); transition: transform 1s linear; }'));
  assert.ok(/@media \(prefers-reduced-motion: reduce\) \{\s*body\.display-mode \.ham-playlist-track \{ display: none; \}\s*body\.display-mode \.ham-playlist-next \{ display: inline-block; \}/.test(html));
  assert.ok(html.includes('body.kiosk-mode .ham-display-controls, body.kiosk-mode .ham-playlist { display: none !important; }'));
  for (const fn of ['stageTrackerPlaylistApply', 'stageTrackerPlaylistTick', 'stageTrackerPlaylistInit']) {
    assert.ok(!/localStorage|location\.assign|fetch\(/.test(fnSrc(fn)), fn + ' must not persist, navigate or refetch');
  }
});

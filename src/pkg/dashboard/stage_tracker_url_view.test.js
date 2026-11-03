// Activity Monitor URL views (hivecommons/hive#15924): `am.*` query
// parameters describe a filter and group, win over localStorage, and make the
// tab session-only so two screens on one profile cannot clobber each other.
// The helpers are lifted out of index.html so this runs against the shipped
// source. Run: node --test src/pkg/dashboard/*.test.js
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
  'var _stageTrackerFilter = stageTrackerDefaultFilter();',
  'var _stageTrackerUrlView;',
  fnSrc('stageTrackerUrlView'), fnSrc('stageTrackerLoadFilter'), fnSrc('stageTrackerSaveFilter'),
  fnSrc('stageTrackerGroupSetting'), fnSrc('stageTrackerSetGroup'),
  'return { stageTrackerFilterFromUrl, stageTrackerViewUrl, stageTrackerDefaultFilter, stageTrackerLoadFilter, stageTrackerSaveFilter, stageTrackerGroupSetting, stageTrackerSetGroup, filter: () => _stageTrackerFilter, setFilter: f => { _stageTrackerFilter = f; } };',
].join('\n');

function load(href, store) {
  store = store || {};
  const writes = [];
  const localStorage = {
    getItem: k => (k in store ? store[k] : null),
    setItem(k, v) { writes.push(k); store[k] = String(v); },
  };
  const window = { location: { href } };
  const api = new Function('window', 'localStorage', 'URL', src)(window, localStorage, URL);
  return { api, store, writes };
}

const BASE = 'https://hive.example/';

test('am.kind / am.repo / am.group render that filter in a fresh profile and leave localStorage untouched', () => {
  const { api, store, writes } = load(BASE + '?am.kind=pr&am.repo=hivecommons/hive&am.group=repo#section-activity-monitor-section');
  api.stageTrackerLoadFilter();
  assert.strictEqual(api.filter().kind, 'pr');
  assert.strictEqual(api.filter().repo, 'hivecommons/hive');
  assert.strictEqual(api.stageTrackerGroupSetting(), 'repo');
  api.stageTrackerSaveFilter();
  api.stageTrackerSetGroup('none');
  assert.deepStrictEqual(writes, [], 'an am.* tab never writes localStorage');
  assert.deepStrictEqual(store, {});
});

test('am.* wins over the stored filter and group, and edits stay in the tab', () => {
  const stored = { 'hive-activity-monitor-filter': JSON.stringify({ kind: 'issue', q: 'stored', sort: 'newest' }), 'hive.tracker.group': 'repo' };
  const before = Object.assign({}, stored);
  const { api, store, writes } = load(BASE + '?am.q=wall', stored);
  api.stageTrackerLoadFilter();
  assert.strictEqual(api.filter().q, 'wall');
  assert.strictEqual(api.filter().kind, 'all', 'the URL describes the whole view; stored values do not leak in');
  assert.strictEqual(api.filter().sort, 'furthest');
  assert.strictEqual(api.stageTrackerGroupSetting(), 'none');
  // A change in this tab survives a reload of the filter but is never persisted.
  api.filter().kind = 'pr';
  api.stageTrackerSaveFilter();
  api.stageTrackerLoadFilter();
  assert.strictEqual(api.filter().kind, 'pr');
  assert.strictEqual(api.stageTrackerSetGroup('repo'), 'repo');
  assert.strictEqual(api.stageTrackerGroupSetting(), 'repo');
  assert.deepStrictEqual(writes, []);
  assert.deepStrictEqual(store, before);
});

test('without am.* load and save behave exactly as before', () => {
  const { api, store, writes } = load(BASE + '?theme=light&filter=waiting-on-you', { 'hive-activity-monitor-filter': JSON.stringify({ kind: 'issue' }), 'hive.tracker.group': 'repo' });
  api.stageTrackerLoadFilter();
  assert.strictEqual(api.filter().kind, 'issue');
  assert.strictEqual(api.stageTrackerGroupSetting(), 'repo');
  api.filter().q = 'x';
  api.stageTrackerSaveFilter();
  assert.strictEqual(JSON.parse(store['hive-activity-monitor-filter']).q, 'x');
  assert.ok(!('page' in JSON.parse(store['hive-activity-monitor-filter'])));
  api.stageTrackerSetGroup('none');
  assert.strictEqual(store['hive.tracker.group'], 'none');
  assert.deepStrictEqual(writes, ['hive-activity-monitor-filter', 'hive.tracker.group']);
});

test('filterFromUrl: null without am.*, every narrowing filter parses, invalid values fall back', () => {
  const { api } = load(BASE);
  assert.strictEqual(api.stageTrackerFilterFromUrl(BASE + '?theme=light&filter=waiting-on-you'), null);
  assert.strictEqual(api.stageTrackerFilterFromUrl('not a url'), null);
  const v = api.stageTrackerFilterFromUrl(BASE + '?am.kind=issue&am.repo=a/b&am.q=flaky%20test&am.age=1d&am.author=human&am.acted=stuck&am.sort=newest&am.dir=asc&am.labels=Bug,%20stage/050-review,&am.needsHuman=1&am.onHold=true&am.attention=1&am.group=repo');
  assert.deepStrictEqual(Object.assign({}, v.filter, { page: 1 }), Object.assign(api.stageTrackerDefaultFilter(), {
    kind: 'issue', repo: 'a/b', q: 'flaky test', age: '1d', author: 'human', acted: 'stuck', sort: 'newest', dir: 'asc',
    labels: ['bug', 'stage/050-review'], needsHuman: true, onHold: true, attention: true,
  }));
  assert.strictEqual(v.group, 'repo');
  const bad = api.stageTrackerFilterFromUrl(BASE + '?am.kind=nope&am.age=forever&am.author=x&am.acted=y&am.sort=z&am.dir=up&am.needsHuman=0&am.group=stage&am.waitingOnYou=1&am.unknown=1');
  assert.deepStrictEqual(bad.filter, api.stageTrackerDefaultFilter(), 'unknown values fall back; waitingOnYou is not an am.* key');
  assert.strictEqual(bad.group, 'none');
});

test('viewUrl round-trips through filterFromUrl, omits defaults, keeps other params and the hash, drops credentials', () => {
  const { api } = load(BASE);
  const f = Object.assign(api.stageTrackerDefaultFilter(), { kind: 'pr', repo: 'hivecommons/hive', q: 'a b', labels: ['bug', 'help wanted'], onHold: true, attention: true, sort: 'number', dir: 'asc', age: 'stale', author: 'hive', acted: 'awaiting' });
  const href = api.stageTrackerViewUrl('https://user:pw@hive.example/?theme=light&token=s3cret&access_token=t&am.q=old&display=1#section-activity-monitor-section', f, 'repo');
  const u = new URL(href);
  assert.strictEqual(u.searchParams.get('theme'), 'light');
  assert.strictEqual(u.searchParams.get('display'), '1');
  assert.strictEqual(u.hash, '#section-activity-monitor-section');
  assert.strictEqual(u.searchParams.get('token'), null);
  assert.strictEqual(u.searchParams.get('access_token'), null);
  assert.strictEqual(u.username + u.password, '');
  assert.strictEqual(u.searchParams.get('am.needsHuman'), null, 'defaults are omitted');
  assert.strictEqual(u.searchParams.get('am.size'), null);
  const back = api.stageTrackerFilterFromUrl(href);
  assert.deepStrictEqual(back.filter, Object.assign({}, f, { page: 1 }));
  assert.strictEqual(back.group, 'repo');
  assert.strictEqual(api.stageTrackerViewUrl(href, back.filter, back.group), href, 'a second round-trip is stable');
  // The default view carries no am.* at all.
  const plain = new URL(api.stageTrackerViewUrl(BASE + '?am.kind=pr&theme=dark', api.stageTrackerDefaultFilter(), 'none'));
  assert.deepStrictEqual(Array.from(plain.searchParams.keys()), ['theme']);
  assert.strictEqual(api.stageTrackerViewUrl('not a url', f, 'none'), '');
});

// Fleet activity board (#16063, part of #15965 / RFC #15861).
//
// Polls GET /api/hub/activity with If-None-Match every 15 s and re-renders
// only when the ETag changes. The endpoint scopes every hive to the viewer
// server-side; this page shows exactly what it returns. Visibility only: rows
// link to GitHub and the hive chip to that hive's own tracker; there are no
// action controls. Titles are set as text (textContent), never as HTML.
//
// Rows use the shared tracker row asset (pkg/webassets/trackerrow, loaded
// before this file): stageTrackerAnchor picks the strip column and
// stageTrackerSplitTitleTag splits the "[agent]" tag off a title. Both read
// the STAGE_TRACKER_* constants below as globals; their values must equal the
// spoke's (TestActivityPage_DefinesTrackerRowGlobalsWithSpokeValues).
//
// Display mode (#16064) reuses the spoke's #15860 Part A as-is: `?kiosk=1`
// hides the chrome for an unattended stream, `?display=1` does so for an
// attended wall screen (Esc or d leaves it), `am.*` describes the view, and
// in display mode `am.view` + `interval` rotate hub-activity views. The URL
// and playlist parsing is the spoke's own code (pkg/webassets/trackerview).

const STAGE_TRACKER_PATH = [0, 10, 20, 30, 50, 80];
const STAGE_TRACKER_PR_COLUMN = 30;
const STAGE_TRACKER_MERGE_COLUMN = 80;
const STAGE_TRACKER_NEEDS_DECISION = 85;
const STAGE_TRACKER_ISSUE_NEEDS_DECISION = 45;
const STAGE_TRACKER_ISSUE_DECISION_COLUMN = 10;
const STAGE_TRACKER_TRIAGE_NEEDED = 15;
const STAGE_TRACKER_POSSIBLY_EXISTS = 17;
const STAGE_TRACKER_PRE_MERGE_SOAK = 95;
const STAGE_TRACKER_TERMINAL = 110;

// The digest carries stage numbers but not the spoke's stage table, so the
// detours' branch_from (stageTrackerBranchFrom in pkg/dashboard) is listed
// here: review-side detours hang off CI / Review, merge-side ones off the
// Merge gate.
const ACTIVITY_STAGE_DETOURS = [
  { number: 70, branch_from: 50 }, { number: 71, branch_from: 50 },
  { number: 72, branch_from: 50 }, { number: 74, branch_from: 50 },
  { number: 85, branch_from: 80 }, { number: 90, branch_from: 80 },
];
const ACTIVITY_COLUMN_LABELS = { 0: 'Open', 10: 'Actionable', 20: 'Claimed', 30: 'PR in progress', 50: 'CI / Review', 80: 'Merge gate', 110: 'Merged' };

const ACTIVITY_API = '/api/hub/activity';
// The anonymous public-safe view (#15966) serves this same page at
// ACTIVITY_PUBLIC_PAGE and polls only its own endpoint, without credentials,
// so it can never show the signed-in view's rows.
const ACTIVITY_PUBLIC_PAGE = '/activity/public';
const ACTIVITY_PUBLIC_API = '/api/hub/activity/public';
const ACTIVITY_POLL_MS = 15000;

// Display mode (#16064): #15860 Part A reused unchanged. `am.*` URL state and
// the `am.view` + `interval` playlist are read by the spoke's own helpers
// (pkg/webassets/trackerview, loaded before this file), which read the
// vocabulary below as globals. Every value equals the spoke's except the
// group vocabulary: this board groups by hive (its default) or not at all
// (TestActivityDisplay_DefinesTrackerViewGlobalsWithSpokeValues).
const STAGE_TRACKER_PAGE_SIZES = [25, 50, 100];
const STAGE_TRACKER_DEFAULT_PAGE_SIZE = 50;
const STAGE_TRACKER_AGE_MS = { '1h': 3600000, '1d': 86400000, '1w': 604800000 };
const STAGE_TRACKER_ACTED_FILTERS = ['any', 'awaiting', 'acted', 'stuck'];
const STAGE_TRACKER_SORTS = ['furthest', 'waiting', 'newest', 'number'];
const STAGE_TRACKER_GROUPS = ['hive', 'none'];
const STAGE_TRACKER_GROUP_DEFAULT = 'hive';
// The view kind every playlist entry on this page is: the Fleet activity
// board under that entry's am.* state.
const ACTIVITY_DISPLAY_VIEW = 'hub-activity';
const ACTIVITY_PLAYLIST_TICK_MS = 1000;
const ACTIVITY_DISPLAY_EXIT_KEY = 'd';

// ---- Display mode (pure) ----

// ?kiosk=1 and ?display=1 exactly as the spoke's boot.js reads them: only
// "1" turns a flag on, and kiosk wins when both are present.
function activityDisplayFlags(href) {
  let kiosk = false, display = false;
  try {
    const p = new URL(href).searchParams;
    kiosk = p.get('kiosk') === '1';
    display = p.get('display') === '1';
  } catch (e) {
    return { kiosk: false, display: false };
  }
  return { kiosk: kiosk, display: display && !kiosk };
}

// The { filter, group } a URL's am.* state describes, or the defaults.
function activityUrlView(href) {
  return stageTrackerFilterFromUrl(href) || { filter: stageTrackerDefaultFilter(), group: STAGE_TRACKER_GROUP_DEFAULT };
}

// href with am.* rewritten for filter + group; other params kept, credentials
// dropped (the spoke's stageTrackerViewUrl).
function activityViewHref(href, filter, group) {
  return stageTrackerViewUrl(href, filter, group);
}

// The am.* keys a filter sets that this board cannot apply: the digest
// carries no labels, needs-human/on-hold/attention verdicts, acted state or
// creation time. They stay in the URL (so a spoke link keeps working there)
// and are listed on the board instead of silently ignored.
function activityUnappliedKeys(filter) {
  const f = stageTrackerNormalizeFilter(filter);
  const out = [];
  if (f.labels.length) out.push('am.labels');
  if (f.needsHuman) out.push('am.needsHuman');
  if (f.onHold) out.push('am.onHold');
  if (f.attention) out.push('am.attention');
  if (f.age !== 'any') out.push('am.age');
  if (f.acted !== 'any') out.push('am.acted');
  if (f.sort === 'newest') out.push('am.sort');
  return out;
}

// Does one digest item pass the narrowing filters this board applies? The
// same tests the spoke's stageTrackerMatches makes for kind, repo, q (number
// or title) and author.
function activityItemMatches(it, f) {
  if (f.kind !== 'all' && it.kind !== f.kind) return false;
  if (f.repo && it.repo !== f.repo) return false;
  if (f.q) {
    const q = f.q.trim().toLowerCase();
    const num = q.replace(/^#/, '');
    const byNumber = /^\d+$/.test(num) && String(it.n) === num;
    const byTitle = String(it.title || '').toLowerCase().indexOf(q) >= 0;
    if (!byNumber && !byTitle) return false;
  }
  if (f.author !== 'any' && (it.author_kind || '') !== f.author) return false;
  return true;
}

// The spoke's attention tier within a column (stageTrackerAttentionTier) on
// the digest's fields: held or draft rows sink, rows with a modifier or
// parked on a person come first. The values mirror the spoke's
// STAGE_TRACKER_SUNK_MODS / STAGE_TRACKER_ATTENTION_STAGES.
const ACTIVITY_SUNK_MODS = ['hold', 'draft'];
const ACTIVITY_ATTENTION_STAGES = [45, 71, 72, 85, 90];
function activityAttentionTier(it) {
  const mods = Array.isArray(it.mods) ? it.mods : [];
  if (ACTIVITY_SUNK_MODS.some(function (m) { return mods.indexOf(m) >= 0; })) return 2;
  return mods.length > 0 || ACTIVITY_ATTENTION_STAGES.indexOf(Number(it.stage_n) || 0) >= 0 ? 0 : 1;
}

// The spoke's sort orders (stageTrackerSort) on the digest's fields:
// furthest strip column first, attention tier, then longest in stage;
// longest in stage; or repo + number. dir flips the column and age order,
// never the tier. newest needs a creation time the digest lacks, so it sorts
// as furthest (and activityUnappliedKeys says so).
function activitySortItems(items, f) {
  const sign = f.dir === 'asc' ? -1 : 1;
  const since = function (it) { return Date.parse(it.since || '') || Infinity; };
  const byRepoNumber = function (x, y) { return String(x.repo || '').localeCompare(String(y.repo || '')) || ((Number(x.n) || 0) - (Number(y.n) || 0)); };
  const byTier = function (x, y) { return activityAttentionTier(x) - activityAttentionTier(y); };
  const anchor = function (it) { return stageTrackerAnchor(Number(it.stage_n) || 0, ACTIVITY_STAGE_DETOURS, it.kind === 'pr' ? 'pr' : 'issue', null); };
  let cmp;
  switch (f.sort) {
    case 'waiting': cmp = function (x, y) { return byTier(x, y) || sign * ((since(x) - since(y)) || byRepoNumber(x, y)); }; break;
    case 'number': cmp = function (x, y) { return sign * byRepoNumber(y, x); }; break;
    default: cmp = function (x, y) { return sign * (anchor(y) - anchor(x)) || byTier(x, y) || sign * ((since(x) - since(y)) || byRepoNumber(x, y)); };
  }
  return items.slice().sort(cmp);
}

// The polled view narrowed and ordered by an am.* filter. The polled view
// itself is never changed, so a rotation re-renders without a refetch.
function activityVisibleView(view, filter) {
  const f = stageTrackerNormalizeFilter(filter);
  const items = (view.items || []).filter(function (it) { return activityItemMatches(it, f); });
  return Object.assign({}, view, { items: activitySortItems(items, f) });
}

// The display playlist: with ?display=1 and two or more valid am.view
// entries, the spoke's parsed playlist with each entry tagged as a
// hub-activity view; otherwise null (kiosk and the plain page never rotate).
function activityPlaylist(href) {
  if (!activityDisplayFlags(href).display) return null;
  const pl = stageTrackerParsePlaylist(href);
  if (pl.views.length < 2) return null;
  return {
    views: pl.views.map(function (v) { return { kind: ACTIVITY_DISPLAY_VIEW, filter: v.filter, group: v.group }; }),
    intervalMs: pl.intervalMs,
  };
}

// Which view shows at nowMs and the progress label, from wall-clock time
// like the spoke, so a reload resumes on the same view.
function activityPlaylistAt(pl, nowMs) {
  const index = stageTrackerPlaylistIndex(0, nowMs, pl.intervalMs, pl.views.length);
  const remainingMs = stageTrackerPlaylistRemainingMs(0, nowMs, pl.intervalMs);
  return { index: index, remainingMs: remainingMs, label: (index + 1) + '/' + pl.views.length + ' · ' + stageTrackerPlaylistNextLabel(remainingMs) };
}

// Leaving display mode keeps the view: only display=1 goes.
function activityDisplayExitHref(href) {
  try {
    const url = new URL(href);
    url.searchParams.delete('display');
    return url.toString();
  } catch (e) {
    return '';
  }
}

// Esc (unless browser full screen consumes it) or a bare d leaves display
// mode, as on the spoke; never while typing or with a modifier.
function activityDisplayExitKey(ev, typing, fullscreen) {
  if (!ev || typing) return false;
  if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.shiftKey) return false;
  if (ev.key === 'Escape') return !fullscreen;
  return ev.key === ACTIVITY_DISPLAY_EXIT_KEY;
}

// esc is the HTML escaper tracker-row-title.js expects from its host. This
// page never builds HTML strings; it is defined so the shared asset loads
// with its full contract.
function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
  });
}

function activityPlural(n, one, many) {
  return n + ' ' + (n === 1 ? one : many);
}

function activityAgo(iso, now) {
  const t = Date.parse(iso || '');
  if (!isFinite(t)) return 'never';
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 60) return 'just now';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  return Math.floor(s / 86400) + 'd ago';
}

function activityItemURL(it) {
  const parts = String(it.repo || '').split('/');
  const n = Number(it.n);
  if (parts.length !== 2 || !parts[0] || !parts[1] || !(n > 0)) return '';
  return 'https://github.com/' + parts.map(encodeURIComponent).join('/') + '/' + (it.kind === 'pr' ? 'pull' : 'issues') + '/' + n;
}

// The hub's existing hive route: signs the viewer in to the hive's own
// dashboard (its tracker), or explains why it cannot.
function activityHiveURL(hiveId) {
  const id = String(hiveId);
  // "." and ".." survive encodeURIComponent and would resolve to another path.
  if (!id || id === '.' || id === '..') return '';
  return '/api/saas/hives/' + encodeURIComponent(id) + '/open';
}

// A stable hue per hive id (FNV-1a), so a hive keeps its chip colour across
// polls and viewers.
function activityHiveHue(hiveId) {
  let h = 0x811c9dc5;
  const s = String(hiveId);
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h % 360;
}

function activityHiveDisplay(hv) {
  return hv.name && hv.name !== hv.hive_id ? hv.name + ' (' + hv.hive_id + ')' : String(hv.hive_id);
}

function activityColumn(it) {
  const col = stageTrackerAnchor(Number(it.stage_n) || 0, ACTIVITY_STAGE_DETOURS, it.kind === 'pr' ? 'pr' : 'issue', null);
  return ACTIVITY_COLUMN_LABELS[col] || ACTIVITY_COLUMN_LABELS[0];
}

function activityFreshness(hv) {
  switch (hv.status) {
    case 'missing': return 'missing';
    case 'not_reporting': return 'not reporting';
    case 'stale': return 'stale';
  }
  return hv.stale ? 'stale' : 'fresh';
}

function activityHiveHeader(hv, count, now) {
  const freshness = activityFreshness(hv);
  let note = '';
  if (freshness === 'missing') {
    note = 'not in the hub registry';
  } else if (freshness === 'not reporting') {
    note = 'heartbeats but sends no activity — upgrade to report activity';
  } else if (freshness === 'stale') {
    note = 'stale — last activity report ' + activityAgo(hv.received_at, now) + ', over two missed heartbeats';
  } else {
    note = 'last activity report ' + activityAgo(hv.received_at, now);
  }
  return {
    hiveId: String(hv.hive_id),
    label: hv.name || String(hv.hive_id),
    count: count,
    total: hv.totals && Number(hv.totals.items) || 0,
    truncated: Number(hv.truncated) || 0,
    freshness: freshness,
    note: note,
    hue: activityHiveHue(hv.hive_id),
    linkable: freshness !== 'missing',
  };
}

function activityRows(view) {
  const names = {};
  (view.hives || []).forEach(function (hv) { names[hv.hive_id] = hv.name || ''; });
  return (view.items || []).map(function (it) {
    const parts = stageTrackerSplitTitleTag(it.title);
    return {
      hiveId: String(it.hive_id),
      hiveLabel: names[it.hive_id] || String(it.hive_id),
      hue: activityHiveHue(it.hive_id),
      hiveURL: activityHiveURL(it.hive_id),
      number: Number(it.n),
      kind: it.kind === 'pr' ? 'PR' : 'Issue',
      repo: String(it.repo || ''),
      url: activityItemURL(it),
      tag: parts.tag,
      title: parts.text,
      fullTitle: String(it.title || ''),
      column: activityColumn(it),
      stage: String(it.stage || ''),
      author: it.author_kind || '',
    };
  });
}

// activityGroups returns one group per visible hive, in the endpoint's
// order, each with its header and rows; a hive with no rows still gets its
// header so freshness and "upgrade to report activity" stay on the board.
function activityGroups(view, now) {
  const rows = activityRows(view);
  const byHive = {};
  rows.forEach(function (r) { (byHive[r.hiveId] = byHive[r.hiveId] || []).push(r); });
  const groups = (view.hives || []).map(function (hv) {
    const mine = byHive[hv.hive_id] || [];
    delete byHive[hv.hive_id];
    return { header: activityHiveHeader(hv, mine.length, now), rows: mine };
  });
  Object.keys(byHive).forEach(function (id) {
    groups.push({ header: activityHiveHeader({ hive_id: id, status: 'ok' }, byHive[id].length, now), rows: byHive[id] });
  });
  return groups;
}

// activityDisclosures spells out everything the board does not show: items
// left out by the hub's response cap or a hive's digest cap, dropped events,
// and stale, not-reporting or missing hives. It is rendered as a visible
// list on every poll.
function activityDisclosures(view, now) {
  const out = [];
  const cut = Number(view.truncated) || 0;
  if (cut > 0) out.push(activityPlural(cut, 'more item', 'more items') + ' matched but were left out by the hub’s response cap');
  (view.hives || []).forEach(function (hv) {
    const who = activityHiveDisplay(hv);
    const freshness = activityFreshness(hv);
    if (freshness === 'missing') out.push(who + ': not in the hub registry (access granted, but the hub does not know this hive)');
    if (freshness === 'not reporting') out.push(who + ': upgrade to report activity (it heartbeats but sends no activity)');
    if (freshness === 'stale') out.push(who + ': stale — last activity report ' + activityAgo(hv.received_at, now));
    if (Number(hv.truncated) > 0) out.push(who + ': ' + activityPlural(Number(hv.truncated), 'item', 'items') + ' left out by the hive’s digest cap');
    if (Number(hv.events_dropped) > 0) out.push(who + ': ' + activityPlural(Number(hv.events_dropped), 'event', 'events') + ' dropped before reaching the hub');
  });
  if (out.length === 0) {
    const n = (view.hives || []).length;
    out.push(n ? 'All ' + activityPlural(n, 'hive', 'hives') + ' reporting; nothing left out' : 'You cannot see any hive yet');
  }
  return out;
}

// ---- DOM (browser only) ----

function activityEl(tag, cls, text) {
  const el = document.createElement(tag);
  if (cls) el.className = cls;
  if (text != null) el.textContent = String(text);
  return el;
}

function activityChip(hiveId, label, hue, href) {
  const chip = activityEl(href ? 'a' : 'span', 'activity-chip', label);
  chip.style.setProperty('--hive-hue', String(hue));
  chip.title = hiveId;
  if (href) {
    chip.href = href;
    chip.title = 'Open ' + hiveId + '’s own tracker';
  }
  return chip;
}

function activityLink(href, cls, text) {
  if (!href) return activityEl('span', cls, text);
  const a = activityEl('a', cls, text);
  a.href = href;
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  return a;
}

function activityRowEl(r, missing) {
  const tr = activityEl('tr', 'activity-row');
  const hive = activityEl('td', 'activity-cell-hive');
  hive.appendChild(activityChip(r.hiveId, r.hiveLabel, r.hue, missing ? '' : r.hiveURL));
  const item = activityEl('td', 'activity-cell-item');
  item.appendChild(activityLink(r.url, 'activity-number', r.repo + '#' + r.number));
  item.appendChild(activityEl('span', 'activity-kind', r.kind));
  const title = activityEl('td', 'activity-cell-title');
  if (r.tag) title.appendChild(activityEl('span', 'activity-tag', r.tag));
  const t = activityLink(r.url, 'activity-title', r.title);
  t.title = r.fullTitle;
  title.appendChild(t);
  const stage = activityEl('td', 'activity-cell-stage');
  stage.appendChild(activityEl('span', 'activity-column', r.column));
  if (r.stage) stage.appendChild(activityEl('span', 'activity-stage', r.stage));
  tr.append(hive, item, title, stage, activityEl('td', 'activity-cell-author', r.author || '—'));
  return tr;
}

function activityHeaderEl(h) {
  const tr = activityEl('tr', 'activity-group activity-group-' + h.freshness.replace(' ', '-'));
  const cell = activityEl('th', '');
  cell.colSpan = 5;
  cell.appendChild(activityChip(h.hiveId, h.label, h.hue, h.linkable ? activityHiveURL(h.hiveId) : ''));
  let count = activityPlural(h.count, 'item', 'items');
  if (h.total > h.count) count += ' shown of ' + h.total + ' tracked';
  cell.appendChild(activityEl('span', 'activity-group-count', count));
  cell.appendChild(activityEl('span', 'activity-fresh activity-fresh-' + h.freshness.replace(' ', '-'), h.freshness));
  cell.appendChild(activityEl('span', 'activity-group-note', h.note));
  tr.appendChild(cell);
  return tr;
}

const ACTIVITY_FETCH_TIMEOUT_MS = 10000;
// filter / group are the am.* view (or the defaults); a display playlist
// replaces them in memory on each rotation.
const activityState = { etag: '', view: null, filter: null, group: STAGE_TRACKER_GROUP_DEFAULT, playlist: null, playlistIndex: -1, timer: 0, updatedAt: 0, inflight: false };

function activityRender() {
  const polled = activityState.view;
  if (!polled) return;
  const now = Date.now();
  const view = activityVisibleView(polled, activityState.filter);
  const missing = {};
  (view.hives || []).forEach(function (hv) { if (hv.status === 'missing') missing[hv.hive_id] = true; });

  const notes = document.createDocumentFragment();
  activityDisclosures(polled, now).forEach(function (text) { notes.appendChild(activityEl('li', '', text)); });
  const unapplied = activityUnappliedKeys(activityState.filter);
  if (unapplied.length) notes.appendChild(activityEl('li', '', 'Not applied on the hub (the activity digest has no data for it): ' + unapplied.join(', ')));
  document.getElementById('activity-disclosures').replaceChildren(notes);

  const out = document.createDocumentFragment();
  if (activityState.group === 'hive') {
    activityGroups(view, now).forEach(function (g) {
      out.appendChild(activityHeaderEl(g.header));
      g.rows.forEach(function (r) { out.appendChild(activityRowEl(r, missing[r.hiveId])); });
    });
  } else {
    activityRows(view).forEach(function (r) { out.appendChild(activityRowEl(r, missing[r.hiveId])); });
  }
  document.getElementById('activity-rows').replaceChildren(out);
  const empty = document.getElementById('activity-empty');
  empty.hidden = view.items.length > 0;
  empty.textContent = (polled.items || []).length ? 'No item in the hives you can see matches this view.' : 'No open items in the hives you can see.';
}

function activitySetStatus(text, bad) {
  const el = document.getElementById('activity-status');
  el.textContent = text;
  el.classList.toggle('activity-status-bad', !!bad);
}

function activityClock(t) {
  return new Date(t).toLocaleTimeString();
}

function activityIsPublicPage(pathname) {
  return String(pathname || '').replace(/\/+$/, '') === ACTIVITY_PUBLIC_PAGE;
}

function activityApiFor(pathname) {
  return activityIsPublicPage(pathname) ? ACTIVITY_PUBLIC_API : ACTIVITY_API;
}

function activityFetchOptions(headers, isPublic) {
  const opts = { headers: headers, credentials: isPublic ? 'omit' : 'same-origin', cache: 'no-store' };
  if (typeof AbortSignal !== 'undefined' && typeof AbortSignal.timeout === 'function') {
    opts.signal = AbortSignal.timeout(ACTIVITY_FETCH_TIMEOUT_MS);
  }
  return opts;
}

// activityPoll fetches once. It never throws: every failure is reported in
// the status line and the next poll retries. The ETag is kept only once its
// view has rendered, so a view that failed to render is fetched again rather
// than pinned by a 304.
async function activityPoll() {
  if (activityState.inflight) return;
  activityState.inflight = true;
  try {
    const headers = {};
    if (activityState.etag) headers['If-None-Match'] = activityState.etag;
    const isPublic = activityIsPublicPage(window.location.pathname);
    let res;
    try {
      res = await fetch(activityApiFor(window.location.pathname), activityFetchOptions(headers, isPublic));
    } catch (e) {
      activitySetStatus('Hub unreachable — retrying every 15 s', true);
      return;
    }
    if (res.status === 401 && !isPublic) {
      window.location.assign('/login');
      return;
    }
    const checked = 'checked ' + activityClock(Date.now());
    if (res.status === 304) {
      // Same data, but "N m ago" and staleness move with the clock.
      activityRender();
      activitySetStatus('Updated ' + activityClock(activityState.updatedAt) + ' · ' + checked, false);
      return;
    }
    if (!res.ok) {
      activitySetStatus('Could not load activity (HTTP ' + res.status + ') — retrying every 15 s', true);
      return;
    }
    const etag = res.headers.get('ETag') || '';
    const view = await res.json();
    if (!view || !Array.isArray(view.hives) || !Array.isArray(view.items)) throw new Error('unexpected activity payload');
    activityState.view = view;
    activityRender();
    activityState.etag = etag;
    activityState.updatedAt = Date.now();
    activitySetStatus('Updated ' + activityClock(activityState.updatedAt) + ' · ' + checked, false);
  } catch (e) {
    activityState.etag = '';
    activitySetStatus('The hub sent an unreadable response — retrying every 15 s', true);
  } finally {
    activityState.inflight = false;
  }
}

function activitySchedule() {
  clearTimeout(activityState.timer);
  activityState.timer = setTimeout(async function tick() {
    try {
      if (!document.hidden) await activityPoll();
    } finally {
      activitySchedule();
    }
  }, ACTIVITY_POLL_MS);
}

// The playlist bar: time left before the next view (a static "N/M · next in
// Ns" label under reduced motion, via activity.css), as on the spoke.
function activityPlaylistRenderProgress(at) {
  const pl = activityState.playlist;
  const el = document.getElementById('activity-playlist');
  if (!pl || !el) return;
  el.querySelector('.activity-playlist-bar').style.setProperty('--activity-playlist-left', (at.remainingMs / pl.intervalMs).toFixed(4));
  el.querySelector('.activity-playlist-next').textContent = at.label;
}

// One playlist tick: switch the in-memory view when the wall clock says so.
// Rotation never navigates, never refetches and never rewrites the URL.
function activityPlaylistTick() {
  const pl = activityState.playlist;
  if (!pl || document.hidden) return;
  const at = activityPlaylistAt(pl, Date.now());
  if (at.index !== activityState.playlistIndex) {
    const v = pl.views[at.index];
    activityState.playlistIndex = at.index;
    activityState.filter = v.filter;
    activityState.group = v.group;
    activityRender();
  }
  activityPlaylistRenderProgress(at);
}

function activityDisplayBoot(flags) {
  if (flags.kiosk) document.body.classList.add('kiosk-mode');
  if (!flags.display) return;
  document.body.classList.add('display-mode');
  document.body.dataset.displayView = ACTIVITY_DISPLAY_VIEW;
  document.addEventListener('keydown', function (e) {
    const tag = ((e.target && e.target.tagName) || '').toLowerCase();
    const typing = tag === 'input' || tag === 'textarea' || tag === 'select' || !!(e.target && e.target.isContentEditable);
    if (!activityDisplayExitKey(e, typing, !!document.fullscreenElement)) return;
    const href = activityDisplayExitHref(window.location.href);
    if (!href) return;
    e.preventDefault();
    window.location.assign(href);
  });
  activityState.playlist = activityPlaylist(window.location.href);
  if (!activityState.playlist) return;
  document.getElementById('activity-playlist').hidden = false;
  activityPlaylistTick();
  setInterval(activityPlaylistTick, ACTIVITY_PLAYLIST_TICK_MS);
}

function activityBoot() {
  const flags = activityDisplayFlags(window.location.href);
  const urlView = activityUrlView(window.location.href);
  activityState.filter = urlView.filter;
  activityState.group = urlView.group;
  if (activityIsPublicPage(window.location.pathname)) {
    const sub = document.querySelector('.activity-sub');
    if (sub) sub.textContent = 'Public view: open issues and pull requests in public repositories of hives listed as public. Private repositories and per-user access are never shown.';
  }
  const toggle = document.getElementById('activity-group');
  toggle.checked = activityState.group === 'hive';
  // The toggle is am.group: the address bar stays a link to this view
  // (credentials never ride along; stageTrackerViewUrl drops them).
  toggle.addEventListener('change', function () {
    activityState.group = toggle.checked ? 'hive' : 'none';
    if (!activityState.playlist) history.replaceState(null, '', activityViewHref(window.location.href, activityState.filter, activityState.group));
    activityRender();
  });
  activityDisplayBoot(flags);
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) activityPoll().finally(activitySchedule);
  });
  activityPoll().finally(activitySchedule);
}

if (typeof document !== 'undefined') {
  document.addEventListener('DOMContentLoaded', activityBoot);
}

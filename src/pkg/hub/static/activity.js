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
const ACTIVITY_POLL_MS = 15000;

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
const activityState = { etag: '', view: null, grouped: true, timer: 0, updatedAt: 0, inflight: false };

function activityRender() {
  const view = activityState.view;
  if (!view) return;
  const now = Date.now();
  const missing = {};
  (view.hives || []).forEach(function (hv) { if (hv.status === 'missing') missing[hv.hive_id] = true; });

  const notes = document.createDocumentFragment();
  activityDisclosures(view, now).forEach(function (text) { notes.appendChild(activityEl('li', '', text)); });
  document.getElementById('activity-disclosures').replaceChildren(notes);

  const out = document.createDocumentFragment();
  if (activityState.grouped) {
    activityGroups(view, now).forEach(function (g) {
      out.appendChild(activityHeaderEl(g.header));
      g.rows.forEach(function (r) { out.appendChild(activityRowEl(r, missing[r.hiveId])); });
    });
  } else {
    activityRows(view).forEach(function (r) { out.appendChild(activityRowEl(r, missing[r.hiveId])); });
  }
  document.getElementById('activity-rows').replaceChildren(out);
  document.getElementById('activity-empty').hidden = (view.items || []).length > 0;
}

function activitySetStatus(text, bad) {
  const el = document.getElementById('activity-status');
  el.textContent = text;
  el.classList.toggle('activity-status-bad', !!bad);
}

function activityClock(t) {
  return new Date(t).toLocaleTimeString();
}

function activityFetchOptions(headers) {
  const opts = { headers: headers, credentials: 'same-origin', cache: 'no-store' };
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
    let res;
    try {
      res = await fetch(ACTIVITY_API, activityFetchOptions(headers));
    } catch (e) {
      activitySetStatus('Hub unreachable — retrying every 15 s', true);
      return;
    }
    if (res.status === 401) {
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

function activityBoot() {
  const toggle = document.getElementById('activity-group');
  activityState.grouped = toggle.checked;
  toggle.addEventListener('change', function () {
    activityState.grouped = toggle.checked;
    activityRender();
  });
  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) activityPoll().finally(activitySchedule);
  });
  activityPoll().finally(activitySchedule);
}

if (typeof document !== 'undefined') {
  document.addEventListener('DOMContentLoaded', activityBoot);
}

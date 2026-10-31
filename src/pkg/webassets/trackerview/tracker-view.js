// Shared tracker URL view and display-playlist helpers (hivecommons/hive#16064,
// part of #15965 / RFC #15861). Every declaration below is a verbatim copy of
// the spoke Activity Monitor source (#15924 / #15932, merged in #15948), so the
// hub Fleet activity page reads am.*, am.view and interval exactly as a spoke
// display does. TestTrackerViewSharedAssetCompat in pkg/dashboard fails when a
// copy drifts from what the spoke serves: edit the spoke first, then re-copy.
//
// Pure functions and constants only. A host page supplies the globals they
// read: STAGE_TRACKER_DEFAULT_PAGE_SIZE, STAGE_TRACKER_PAGE_SIZES,
// STAGE_TRACKER_AGE_MS, STAGE_TRACKER_ACTED_FILTERS, STAGE_TRACKER_SORTS,
// STAGE_TRACKER_GROUPS and STAGE_TRACKER_GROUP_DEFAULT.
    function stageTrackerDefaultFilter() {
      return { kind: 'all', repo: '', q: '', labels: [], needsHuman: false, onHold: false, waitingOnYou: false, age: 'any', author: 'any', attention: false, acted: 'any', sort: 'furthest', dir: 'desc', size: STAGE_TRACKER_DEFAULT_PAGE_SIZE, page: 1 };
    }
    function stageTrackerNormalizeFilter(raw) {
      const d = stageTrackerDefaultFilter();
      const f = Object.assign(d, raw && typeof raw === 'object' ? raw : {});
      if (['all', 'pr', 'issue'].indexOf(f.kind) < 0) f.kind = 'all';
      f.repo = typeof f.repo === 'string' ? f.repo : '';
      f.q = typeof f.q === 'string' ? f.q : '';
      f.labels = Array.isArray(f.labels) ? f.labels.filter(l => typeof l === 'string' && l).map(l => l.toLowerCase()) : [];
      f.needsHuman = !!f.needsHuman;
      f.onHold = !!f.onHold;
      f.waitingOnYou = !!f.waitingOnYou;
      if (!(f.age in STAGE_TRACKER_AGE_MS) && f.age !== 'stale') f.age = 'any';
      if (['human', 'clanker', 'hive'].indexOf(f.author) < 0) f.author = 'any';
      f.attention = !!f.attention;
      if (STAGE_TRACKER_ACTED_FILTERS.indexOf(f.acted) < 0) f.acted = 'any';
      if (STAGE_TRACKER_SORTS.indexOf(f.sort) < 0) f.sort = 'furthest';
      f.dir = f.dir === 'asc' ? 'asc' : 'desc';
      f.size = STAGE_TRACKER_PAGE_SIZES.indexOf(Number(f.size)) >= 0 ? Number(f.size) : STAGE_TRACKER_DEFAULT_PAGE_SIZE;
      f.page = Math.max(1, Math.floor(Number(f.page)) || 1);
      return f;
    }
    // tracker url view helpers begin
    const STAGE_TRACKER_URL_PREFIX = 'am.';
    const STAGE_TRACKER_URL_KEYS = ['kind', 'repo', 'q', 'age', 'author', 'acted', 'sort', 'dir', 'labels', 'needsHuman', 'onHold', 'attention'];
    const STAGE_TRACKER_URL_BOOLS = ['needsHuman', 'onHold', 'attention'];
    const STAGE_TRACKER_URL_CREDENTIAL_PARAMS = ['token', 'access_token'];
    // Pure: the { filter, group } an `am.*` URL describes, or null when it
    // carries no `am.*` parameter. Values go through
    // stageTrackerNormalizeFilter, so an unknown one falls back to its default.
    function stageTrackerFilterFromUrl(href) {
      let url;
      try { url = new URL(href); } catch { return null; }
      const p = url.searchParams;
      if (!Array.from(p.keys()).some(k => k.indexOf(STAGE_TRACKER_URL_PREFIX) === 0)) return null;
      const raw = {};
      for (const k of STAGE_TRACKER_URL_KEYS) {
        const v = p.get(STAGE_TRACKER_URL_PREFIX + k);
        if (v === null) continue;
        if (k === 'labels') raw.labels = v.split(',').map(l => l.trim()).filter(Boolean);
        else if (STAGE_TRACKER_URL_BOOLS.indexOf(k) >= 0) raw[k] = v === '1' || v === 'true';
        else raw[k] = v;
      }
      const g = p.get(STAGE_TRACKER_URL_PREFIX + 'group');
      return { filter: stageTrackerNormalizeFilter(raw), group: STAGE_TRACKER_GROUPS.indexOf(g) >= 0 ? g : STAGE_TRACKER_GROUP_DEFAULT };
    }
    // Pure: href with `am.*` set to describe filter + group. Defaults are
    // omitted; other query params and the hash are kept; credentials never
    // ride along.
    function stageTrackerViewUrl(href, filter, group) {
      let url;
      try { url = new URL(href); } catch { return ''; }
      for (const k of Array.from(url.searchParams.keys())) {
        if (k.indexOf(STAGE_TRACKER_URL_PREFIX) === 0 || STAGE_TRACKER_URL_CREDENTIAL_PARAMS.indexOf(k) >= 0) url.searchParams.delete(k);
      }
      url.username = '';
      url.password = '';
      const f = stageTrackerNormalizeFilter(filter);
      const d = stageTrackerDefaultFilter();
      for (const k of STAGE_TRACKER_URL_KEYS) {
        if (k === 'labels') { if (f.labels.length) url.searchParams.set(STAGE_TRACKER_URL_PREFIX + k, f.labels.join(',')); continue; }
        if (f[k] === d[k]) continue;
        url.searchParams.set(STAGE_TRACKER_URL_PREFIX + k, STAGE_TRACKER_URL_BOOLS.indexOf(k) >= 0 ? '1' : String(f[k]));
      }
      if (STAGE_TRACKER_GROUPS.indexOf(group) >= 0 && group !== STAGE_TRACKER_GROUP_DEFAULT) url.searchParams.set(STAGE_TRACKER_URL_PREFIX + 'group', group);
      return url.toString();
    }
    // tracker url view helpers end
    // tracker display playlist helpers begin
    const STAGE_TRACKER_PLAYLIST_PARAM = 'am.view';
    const STAGE_TRACKER_PLAYLIST_INTERVAL_PARAM = 'interval';
    const STAGE_TRACKER_PLAYLIST_INTERVAL_DEFAULT_MS = 60000;
    const STAGE_TRACKER_PLAYLIST_INTERVAL_MIN_MS = 10000;
    const STAGE_TRACKER_PLAYLIST_INTERVAL_MAX_MS = 3600000;
    // Pure: `interval` (`45s`, `5m`) in ms, clamped; anything else is the default.
    function stageTrackerPlaylistIntervalMs(raw) {
      const m = /^\s*(\d{1,7})\s*([sm])\s*$/i.exec(raw == null ? '' : String(raw));
      if (!m) return STAGE_TRACKER_PLAYLIST_INTERVAL_DEFAULT_MS;
      const ms = Number(m[1]) * (m[2].toLowerCase() === 'm' ? 60000 : 1000);
      return Math.min(STAGE_TRACKER_PLAYLIST_INTERVAL_MAX_MS, Math.max(STAGE_TRACKER_PLAYLIST_INTERVAL_MIN_MS, ms));
    }
    // Pure: one decoded `am.view` value as { filter, group }, or null when it
    // names no known `am.*` key (or nests another am.view) — such a view is
    // skipped.
    function stageTrackerPlaylistView(raw) {
      let p;
      try { p = new URLSearchParams(String(raw == null ? '' : raw).replace(/^\?/, '')); } catch { return null; }
      const keys = Array.from(p.keys());
      if (keys.indexOf(STAGE_TRACKER_PLAYLIST_PARAM) >= 0) return null;
      const known = STAGE_TRACKER_URL_KEYS.concat(['group']).map(k => STAGE_TRACKER_URL_PREFIX + k);
      if (!keys.some(k => known.indexOf(k) >= 0)) return null;
      return stageTrackerFilterFromUrl('https://hive.invalid/?' + p.toString());
    }
    // Pure: { views: [{ filter, group }…], intervalMs } for a URL. Fewer than
    // two views means the caller does not rotate.
    function stageTrackerParsePlaylist(href) {
      let url;
      try { url = new URL(href); } catch { return { views: [], intervalMs: STAGE_TRACKER_PLAYLIST_INTERVAL_DEFAULT_MS }; }
      const views = url.searchParams.getAll(STAGE_TRACKER_PLAYLIST_PARAM).map(stageTrackerPlaylistView).filter(Boolean);
      return { views, intervalMs: stageTrackerPlaylistIntervalMs(url.searchParams.get(STAGE_TRACKER_PLAYLIST_INTERVAL_PARAM)) };
    }
    // Pure: which of n views is showing. Derived from wall-clock time (pass
    // startMs 0), so a reload or an SSE reconnect lands on the same view.
    function stageTrackerPlaylistIndex(startMs, nowMs, intervalMs, n) {
      if (!(n >= 2) || !(intervalMs > 0)) return 0;
      const k = Math.floor((Number(nowMs) - Number(startMs)) / intervalMs);
      if (!Number.isFinite(k)) return 0;
      return ((k % n) + n) % n;
    }
    // Pure: ms left before the next view.
    function stageTrackerPlaylistRemainingMs(startMs, nowMs, intervalMs) {
      if (!(intervalMs > 0)) return 0;
      const e = (((Number(nowMs) - Number(startMs)) % intervalMs) + intervalMs) % intervalMs;
      return Number.isFinite(e) ? intervalMs - e : 0;
    }
    // Pure: the reduced-motion label.
    function stageTrackerPlaylistNextLabel(remainingMs) {
      return 'next in ' + Math.max(0, Math.ceil((Number(remainingMs) || 0) / 1000)) + 's';
    }
    // tracker display playlist helpers end

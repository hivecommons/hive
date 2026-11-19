// Pure helpers for the UI render-evidence job (#18915): which dashboard pages
// and sections a PR touches, what to capture, how the capture is indexed, and
// how the single sticky PR comment is worded.
//
// Everything here is deterministic and has no browser or network access, so
// the invariants (a time budget, no markup injection from artifact data, a
// stable comment marker) are asserted in render-evidence.test.mjs.

// Total wall-clock budget for capturing. Shots not started before it runs out
// are reported as skipped and the run is reported "partial" (neutral), never
// failed. The workflow's timeout-minutes is deliberately larger than this so
// the harness always gets to write its index.
export const RENDER_EVIDENCE_BUDGET_MS = 8 * 60 * 1000;
// Upper bound for one screenshot including page readiness.
export const RENDER_EVIDENCE_SHOT_TIMEOUT_MS = 30 * 1000;
// At most this many dashboard sections are captured per viewport and theme.
export const RENDER_EVIDENCE_MAX_SECTIONS = 4;
// Hidden marker that makes the PR comment sticky: it is found and updated in
// place, never duplicated.
export const RENDER_EVIDENCE_MARKER = '<!-- hive-ui-render-evidence -->';
// Longest reason or file name accepted from an artifact when composing the
// comment. Artifact content is data from the PR author, so it is bounded.
export const RENDER_EVIDENCE_MAX_TEXT = 160;
export const RENDER_EVIDENCE_MAX_SHOTS = 64;

export const STATUS_OK = 'ok';
export const STATUS_PARTIAL = 'partial';
export const STATUS_NEUTRAL = 'neutral';

export const VIEWPORTS = [
  { id: 'desktop', width: 1440, height: 900, dpr: 1, mobile: false },
  { id: 'phone', width: 390, height: 844, dpr: 2, mobile: true },
];
export const THEMES = ['light', 'dark'];

// The pages that can be rendered offline. staticRoot is served at "/" so the
// root-relative tokens.css/components.css links resolve exactly as in
// production; triggers say which changed files make the page "affected".
export const PAGES = [
  {
    id: 'hive',
    label: 'Hive dashboard',
    file: 'src/pkg/dashboard/static/index.html',
    staticRoot: 'src/pkg/dashboard/static',
    triggers: [/^src\/pkg\/dashboard\/static(-src)?\//],
    sections: true,
  },
  {
    id: 'hub-fleet',
    label: 'Hub fleet page',
    file: 'src/pkg/hub/static/my-hives.html',
    staticRoot: 'src/pkg/hub/static',
    triggers: [/^src\/pkg\/hub\/static\/(my-hives\.html|tokens\.css|components\.css)$/],
    sections: false,
  },
  {
    id: 'hub-dashboard',
    label: 'Hub dashboard',
    file: 'src/pkg/hub/assets/dashboard.html',
    staticRoot: 'src/pkg/hub/static',
    triggers: [/^src\/pkg\/hub\/assets\//, /^src\/pkg\/hub\/static\/(tokens|components)\.css$/],
    sections: false,
  },
];

// Files the evidence job reacts to. Mirrors the workflow's paths filter.
export function isUIFile(file) {
  return /^src\/pkg\/dashboard\/(static|static-src)\//.test(file) ||
    /^src\/pkg\/hub\/(static|assets)\//.test(file);
}

// A change to the evidence tooling itself renders every page, as a smoke test
// of the tooling.
const TOOLING = /^(src\/scripts\/(dashboard-render-evidence\.mjs|render-evidence-comment\.mjs|lib\/render-evidence[^/]*|fixtures\/render-evidence\/.*)|\.github\/workflows\/dashboard-render-evidence[^/]*)$/;

export function affectedPages(changedFiles) {
  const files = (changedFiles || []).filter(Boolean);
  if (!files.length || files.some((f) => TOOLING.test(f))) return PAGES.slice();
  return PAGES.filter((page) => files.some((f) => page.triggers.some((re) => re.test(f))));
}

const SECTION_ATTR = /data-dashboard-section="([A-Za-z0-9_-]+)"/g;

export function sectionIdsInHTML(html) {
  const ids = [];
  for (const m of String(html || '').matchAll(SECTION_ATTR)) if (!ids.includes(m[1])) ids.push(m[1]);
  return ids;
}

// Maps one changed dashboard source file to the section ids it can affect.
// html partials name their sections directly; css and js files are matched by
// file stem ("sessions.css" -> "sessions-section"). A file that matches
// nothing (shared base styles) yields [], and the collapsed full-page shot
// that is always captured covers it.
export function sectionsForFile(file, knownIds, readFile) {
  const base = file.split('/').pop() || '';
  const stem = base.replace(/\.[^.]+$/, '');
  if (/^src\/pkg\/dashboard\/static-src\/html\//.test(file) && typeof readFile === 'function') {
    let html = '';
    try { html = readFile(file); } catch (_) { html = ''; }
    return sectionIdsInHTML(html).filter((id) => (knownIds || []).includes(id));
  }
  if (!/^src\/pkg\/dashboard\/static-src\/(css|js)\//.test(file)) return [];
  const tokens = stem.split(/[-_.]/);
  return (knownIds || []).filter((id) => {
    const name = id.replace(/-(section|panel)$/, '');
    return stem === name || stem.startsWith(name + '-') || tokens.includes(name);
  });
}

export function planSections(changedFiles, knownIds, readFile) {
  const picked = [];
  if ((changedFiles || []).some(f => /static-src\/(js|css)\/(agent-detail|kick-outcomes)\./.test(f))) picked.push('kick-outcomes');
  for (const f of changedFiles || []) {
    for (const id of sectionsForFile(f, knownIds, readFile)) if (!picked.includes(id)) picked.push(id);
  }
  return picked.slice(0, RENDER_EVIDENCE_MAX_SECTIONS);
}

// File names are generated from ids, but ids come from page markup, so they
// are reduced to a conservative alphabet.
export function shotFileName({ page, section, viewport, theme }) {
  const raw = [page, section || 'overview', viewport, theme].join('-');
  return raw.replace(/[^A-Za-z0-9_-]/g, '_') + '.png';
}

export function classifyStatus({ planned, captured, budgetExceeded, harnessError }) {
  if (harnessError) return { status: STATUS_NEUTRAL, reason: harnessError };
  if (planned === 0) return { status: STATUS_NEUTRAL, reason: 'no dashboard page was affected by this change' };
  if (captured === 0) return { status: STATUS_NEUTRAL, reason: 'no screenshot could be captured' };
  if (budgetExceeded) {
    return { status: STATUS_PARTIAL, reason: 'time budget reached; ' + captured + ' of ' + planned + ' screenshots captured' };
  }
  if (captured < planned) {
    return { status: STATUS_PARTIAL, reason: captured + ' of ' + planned + ' screenshots captured; the rest failed to render' };
  }
  return { status: STATUS_OK, reason: '' };
}

export function buildIndex({ pr, head, status, reason, shots, skipped, budgetMs, elapsedMs }) {
  return {
    schema: 1,
    pr: Number.isInteger(pr) ? pr : 0,
    head: String(head || ''),
    status,
    reason: reason || '',
    fixturesNotLive: true,
    budgetMs,
    elapsedMs,
    shots: shots || [],
    skipped: skipped || [],
  };
}

// ---- Comment composition: the artifact is untrusted data ----------------

const SAFE_FILE = /^[A-Za-z0-9_-]{1,100}\.png$/;
const SAFE_ID = /^[A-Za-z0-9_-]{1,64}$/;
const SAFE_SHA = /^[0-9a-f]{40}$/;

// Reduces free text from an artifact to plain, bounded text that cannot open
// markup, mention anyone or break out of a table cell.
export function cleanText(value) {
  return String(value == null ? '' : value)
    .replace(/[^\x20-\x7e]/g, ' ')
    .replace(/[<>`|\\[\]()@#!*_~:\/]/g, ' ')
    .replace(/\s+/g, ' ')
    .trim()
    .slice(0, RENDER_EVIDENCE_MAX_TEXT);
}

// Returns a sanitised copy of an index, or null when it is not a valid one.
export function validateIndex(raw) {
  if (!raw || typeof raw !== 'object' || raw.schema !== 1) return null;
  const status = [STATUS_OK, STATUS_PARTIAL, STATUS_NEUTRAL].includes(raw.status) ? raw.status : STATUS_NEUTRAL;
  const shots = [];
  for (const s of Array.isArray(raw.shots) ? raw.shots.slice(0, RENDER_EVIDENCE_MAX_SHOTS) : []) {
    if (!s || !SAFE_FILE.test(String(s.file)) || !SAFE_ID.test(String(s.page)) ||
        !SAFE_ID.test(String(s.section || 'overview')) || !SAFE_ID.test(String(s.viewport)) ||
        !['light', 'dark'].includes(s.theme)) continue;
    shots.push({ file: s.file, page: s.page, section: String(s.section || 'overview'), viewport: s.viewport, theme: s.theme,
      bytes: Number.isInteger(s.bytes) && s.bytes >= 0 ? s.bytes : 0 });
  }
  const skipped = [];
  for (const s of Array.isArray(raw.skipped) ? raw.skipped.slice(0, RENDER_EVIDENCE_MAX_SHOTS) : []) {
    if (s) skipped.push({ what: cleanText(s.what), reason: cleanText(s.reason) });
  }
  return {
    pr: Number.isInteger(raw.pr) && raw.pr > 0 ? raw.pr : 0,
    head: SAFE_SHA.test(String(raw.head)) ? raw.head : '',
    status,
    reason: cleanText(raw.reason),
    shots,
    skipped,
  };
}

function kb(bytes) { return Math.max(1, Math.round(bytes / 1024)) + ' KB'; }

const STATUS_LINE = {
  [STATUS_OK]: 'Render evidence captured.',
  [STATUS_PARTIAL]: 'Render evidence is partial (neutral, not a failure).',
  [STATUS_NEUTRAL]: 'No render evidence this time (neutral, not a failure).',
};

// The one sticky comment. links.run is the workflow run page and
// links.artifact the artifact download page; both are built by the caller from
// trusted workflow context, never from artifact content.
export function renderCommentBody(index, links) {
  const lines = [RENDER_EVIDENCE_MARKER, '### UI render evidence', ''];
  lines.push(STATUS_LINE[index.status] + (index.reason ? ' ' + index.reason + '.' : ''));
  lines.push('');
  if (index.head) lines.push('Built from `' + index.head.slice(0, 12) + '`.');
  if (links && links.artifact) lines.push('Screenshots: [download the artifact](' + links.artifact + ') (sign-in to GitHub required).');
  if (links && links.run) lines.push('Run: [' + links.run + '](' + links.run + ')');
  lines.push('');
  if (index.shots.length) {
    lines.push('| Page | Section | Viewport | Theme | File | Size |');
    lines.push('|---|---|---|---|---|---|');
    for (const s of index.shots) {
      lines.push('| ' + [s.page, s.section, s.viewport, s.theme, '`' + s.file + '`', kb(s.bytes)].join(' | ') + ' |');
    }
    lines.push('');
  }
  if (index.skipped.length) {
    lines.push('Skipped: ' + index.skipped.map((s) => s.what + ' (' + s.reason + ')').join('; ') + '.');
    lines.push('');
  }
  lines.push('These are **fixture renders, not live data**: the page was built from the PR head and filled with synthetic data from `src/scripts/fixtures/render-evidence/`. They show layout and theming, not how real hives behave. How to read them: `src/docs/ui-render-evidence.md`.');
  return lines.join('\n') + '\n';
}

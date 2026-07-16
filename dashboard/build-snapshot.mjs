#!/usr/bin/env node
// Build a static HTML snapshot of the hive dashboard.
// Reads index.html, fetches live data from the dashboard API,
// and produces a self-contained static HTML file.
//
// Usage: node build-snapshot.mjs [--mode light|classic] [DASHBOARD_URL] [OUTPUT_FILE]

import { readFileSync, writeFileSync } from 'fs';
import { join, dirname } from 'path';
import { fileURLToPath } from 'url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const FETCH_TIMEOUT_MS = 10_000;

const args = process.argv.slice(2);
const modeIdx = args.indexOf('--mode');
const snapshotMode = modeIdx >= 0 ? args[modeIdx + 1] : 'classic';
const basePathIdx = args.indexOf('--base-path');
const basePath = basePathIdx >= 0 ? args[basePathIdx + 1] : '/live/hive';
const htmlIdx = args.indexOf('--html');
const htmlSource = htmlIdx >= 0 ? args[htmlIdx + 1] : join(__dirname, 'index.html');
const skipIdxSet = new Set();
if (modeIdx >= 0) { skipIdxSet.add(modeIdx); skipIdxSet.add(modeIdx + 1); }
if (basePathIdx >= 0) { skipIdxSet.add(basePathIdx); skipIdxSet.add(basePathIdx + 1); }
if (htmlIdx >= 0) { skipIdxSet.add(htmlIdx); skipIdxSet.add(htmlIdx + 1); }
const positional = args.filter((_, i) => !skipIdxSet.has(i));
const dashboardUrl = positional[0] || process.env.HIVE_DASHBOARD_URL || 'http://localhost:3001';
const outputFile = positional[1] || 'snapshot.html';
const authToken = process.env.DASHBOARD_AUTH_TOKEN || '';

async function fetchJson(endpoint, fallback = '{}') {
  try {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);
    const headers = {};
    if (authToken) {
      // The snapshot builder is trusted server-to-server automation running
      // inside the hive pod. Authenticate with X-Hive-Internal, NOT
      // `Authorization: Bearer`: on a direct-route spoke (one with a per-hive
      // authorized_users allowlist) the Bearer/shared-token path is disabled by
      // the dashboard auth middleware, so a Bearer request to /api/status is
      // rejected 401 and the snapshot bakes `render({"error":"unauthorized"})`
      // → every stat renders as "--". X-Hive-Internal is honored regardless of
      // direct-route mode. Bearer is kept as a fallback for older servers.
      headers['X-Hive-Internal'] = authToken;
      headers['Authorization'] = `Bearer ${authToken}`;
    }
    const res = await fetch(`${dashboardUrl}${endpoint}`, { signal: controller.signal, headers });
    clearTimeout(timer);
    return await res.text();
  } catch {
    return fallback;
  }
}

async function main() {
  console.log(`Fetching data from ${dashboardUrl} (mode: ${snapshotMode})...`);

  const [statusRaw, historyRaw, trendsRaw, timelineRaw, configRaw, versionRaw, nousStatusRaw, nousLedgerRaw, nousPrinciplesRaw, kbStatsRaw, kbFactsRaw, contributorsRaw, leaderboardRaw] = await Promise.all([
    fetchJson('/api/status'),
    fetchJson('/api/history', '[]'),
    fetchJson('/api/trends?range=week', '[]'),
    fetchJson('/api/timeline', '[]'),
    fetchJson('/api/config'),
    fetchJson('/api/version'),
    fetchJson('/api/nous/status'),
    fetchJson('/api/nous/ledger', '[]'),
    fetchJson('/api/nous/principles', '[]'),
    fetchJson('/api/knowledge/stats'),
    fetchJson('/api/knowledge', '{"facts":[]}'),
    fetchJson('/api/contributors', '[]'),
    fetchJson('/api/leaderboard', '{"leaderboard":[]}'),
  ]);

  // Validate status
  try { JSON.parse(statusRaw); } catch {
    console.error(`ERROR: Invalid JSON from ${dashboardUrl}/api/status`);
    process.exit(1);
  }

  const config = JSON.parse(configRaw || '{}');
  const projectName = config.projectName || 'Hive';
  const snapshotTs = new Date().toISOString();

  console.log(`Building ${snapshotMode} snapshot for ${projectName} (${snapshotTs})...`);

  const sourceHtml = readFileSync(htmlSource, 'utf-8');

  // Split at key boundaries — the live HTML has two <script> blocks:
  //   1. Small layout pre-apply script right after <body>
  //   2. Main script with all render functions and connect()
  // We need the LAST <script> for render functions, and everything
  // between <body> and that last <script> as body content (includes
  // the small script and all HTML structure).
  const styleEnd = sourceHtml.indexOf('</style>');
  const bodyStart = sourceHtml.indexOf('<body>');
  const mainScriptStart = sourceHtml.lastIndexOf('<script>');
  const mainScriptEnd = sourceHtml.lastIndexOf('</script>');

  const headAndStyles = sourceHtml.slice(0, styleEnd);
  const bodyContent = sourceHtml.slice(bodyStart + 6, mainScriptStart);
  const originalScript = sourceHtml.slice(mainScriptStart + 8, mainScriptEnd);

  // Include the entire script but neutralize live-connection code.
  // Replace connect() body with no-op, and strip the trailing init calls
  // (connect(), checkGHAuth(), fetchAgentLogs) that auto-fire on load.
  const CONNECT_MARKER = 'function connect()';
  const connectIdx = originalScript.indexOf(CONNECT_MARKER);

  // Find the end of connect() by matching its closing brace
  let braceDepth = 0;
  let connectEnd = connectIdx;
  let foundOpen = false;
  for (let i = connectIdx; i < originalScript.length; i++) {
    if (originalScript[i] === '{') { braceDepth++; foundOpen = true; }
    if (originalScript[i] === '}') { braceDepth--; }
    if (foundOpen && braceDepth === 0) { connectEnd = i + 1; break; }
  }

  // Strip trailing init calls: everything after the last function definition
  // (the last '}' before 'connect();' or 'checkGHAuth();' at script end)
  const INIT_MARKER = 'connect();';
  const initCallIdx = originalScript.lastIndexOf(INIT_MARKER);
  // Walk backward to find start of init block (after last function/event listener closing brace)
  let initBlockStart = initCallIdx;
  while (initBlockStart > 0 && originalScript[initBlockStart - 1] !== '\n') initBlockStart--;
  // Go back further past checkGHAuth and setInterval lines
  const checkGHIdx = originalScript.lastIndexOf('checkGHAuth();');
  if (checkGHIdx >= 0 && checkGHIdx < initCallIdx) {
    initBlockStart = checkGHIdx;
    while (initBlockStart > 0 && originalScript[initBlockStart - 1] !== '\n') initBlockStart--;
  }

  // Build the render functions: everything before connect() + everything after connect()
  // up to the init block, with connect() replaced by a no-op
  const renderFunctions = originalScript.slice(0, connectIdx)
    + 'function connect() {} // neutralized for snapshot\n'
    + originalScript.slice(connectEnd, initBlockStart);

  const AUTO_REFRESH_SECONDS = 300;

  const metaRefresh = `<meta http-equiv="refresh" content="${AUTO_REFRESH_SECONDS}">`;

  const isLight = snapshotMode === 'light';

  const bannerBg = isLight
    ? 'background: linear-gradient(135deg, #f0f4ff 0%, #ffffff 100%); border: 1px solid #e5e7eb; color: #6b7280;'
    : 'background: linear-gradient(135deg, #1a1f2e 0%, #161b22 100%); border: 1px solid #30363d; color: #8b949e;';
  const bannerLabelColor = isLight ? 'color: #2563eb;' : 'color: #58a6ff;';
  const bannerTimeColor = isLight ? 'color: #1a1a2e;' : 'color: #e6edf3;';
  const bannerRefreshColor = isLight ? 'color: #6b7280;' : 'color: #8b949e;';

  const staticCss = `
    /* Static snapshot overrides — hide all interactive elements */
    .connection { display: none !important; }
    .agent-actions { display: none !important; }
    .kick-row { display: none !important; }
    .widget-dl { display: none !important; }
    .btn-toggle { display: none !important; }
    .restart-btn { display: none !important; }
    .restart-reset { display: none !important; }
    .config-gear { display: none !important; }
    .pin-toggle { display: none !important; }
    .terminal-link { display: none !important; }
    .config-overlay { display: none !important; }
    .layout-toggle { display: none !important; }
    .oc-chat-prompt { display: none !important; }
    .oc-detail-actions { display: none !important; }
    a[href="/contribute"] { display: none !important; }
    button[onclick] { pointer-events: none !important; opacity: 0.5 !important; }
    /* Hide interactive KB action buttons in snapshot */
    button[onclick*="kbOpenImport"],
    button[onclick*="kbOpenSubscriptions"],
    button[onclick*="kbOpenVaults"],
    button[onclick*="kbOpenObsidianSetup"],
    button[onclick*="kbOpenCreate"],
    button[onclick*="toggleKnowledge"],
    button[onclick*="kbOpenEdit"],
    button[onclick*="kbDelete"],
    button[onclick*="kbPromote"] { display: none !important; }
    /* Re-enable read-only KB buttons (How it works, layer nav, fact select, modal close) */
    button[onclick*="kbOpenHowItWorks"],
    #knowledge-panel .kb-layer-btn,
    .kb-fact-item,
    .nous-config-overlay button[onclick*="kbCloseModal"] { pointer-events: auto !important; opacity: 1 !important; }
    .snapshot-banner {
      ${bannerBg}
      border-radius: 8px;
      padding: 12px 20px; margin-bottom: 16px;
      display: flex; align-items: center; gap: 12px;
      font-size: 0.8rem;
    }
    .snapshot-banner .snap-icon { font-size: 1.2rem; }
    .snapshot-banner .snap-label { ${bannerLabelColor} font-weight: 600; }
    .snapshot-banner .snap-time { ${bannerTimeColor} }
    .snapshot-banner .snap-refresh { ${bannerRefreshColor} margin-left: auto; font-size: 0.75rem; font-variant-numeric: tabular-nums; min-width: 10ch; text-align: right; white-space: nowrap; }
    .snapshot-banner .snap-links { margin-left: 12px; font-size: 0.75rem; }
    .snapshot-banner .snap-links a { ${bannerLabelColor} text-decoration: none; margin: 0 6px; }
    .snapshot-banner .snap-links a:hover { text-decoration: underline; }
  `;

  const altMode = isLight ? 'classic' : 'light';
  const altLabel = isLight ? 'Classic' : 'Light';
  const banner = `
  <div class="snapshot-banner">
    <span class="snap-icon">${isLight ? '📊' : '📸'}</span>
    <span><span class="snap-label">Read-only snapshot</span> &mdash; captured <span class="snap-time" id="snap-time"></span></span>
    <span class="snap-links"><a href="${basePath}/${altMode}">${altLabel} mode</a></span>
    <span class="snap-refresh" id="snap-refresh"></span>
  </div>`;

  const layoutInit = isLight
    ? `applyLayout('light');`
    : `applyLayout('classic');`;

  const initScript = `
    // ── Static snapshot initialization ──
    historyData = ${historyRaw};
    // Trends API returns {evals: [...]}, but _trendData must be the array
    var _rawTrends = ${trendsRaw};
    window._trendData = _rawTrends && _rawTrends.evals ? _rawTrends.evals : (Array.isArray(_rawTrends) ? _rawTrends : []);
    window._timelineData = ${timelineRaw};

    // Set project name — match the live dashboard's pattern
    const _cfg = ${configRaw};
    const _projEl = document.getElementById('project-name');
    if (_projEl && _cfg.primaryRepo) {
      _projEl.textContent = 'for ' + _cfg.primaryRepo;
      document.title = '\\u{1F41D} Hive Dashboard for ' + _cfg.primaryRepo + ' (Snapshot)';
    }
    const _ocProjEl = document.getElementById('oc-project-name');
    if (_ocProjEl && _cfg.primaryRepo) _ocProjEl.textContent = _cfg.primaryRepo;
    if (_cfg.primaryRepo) window._primaryRepo = _cfg.primaryRepo;
    if (_cfg.repo) window._hiveRepo = _cfg.repo;

    // Apply layout mode
    ${layoutInit}

    // Render baked status
    render(${statusRaw});

    // Render Strategy Lab (Nous)
    _nousCache = {
      status: ${nousStatusRaw},
      ledger: ${nousLedgerRaw},
      principles: ${nousPrinciplesRaw},
    };
    renderNous();

    // Knowledge Base — assign facts and override kbSearch BEFORE
    // renderKnowledge(), which internally calls kbSearch() → renderKnowledgeFacts().
    var _snapKbData = ${kbFactsRaw};
    _kbFacts = Array.isArray(_snapKbData) ? _snapKbData : (_snapKbData.facts || []);
    var _snapshotAllFacts = _kbFacts.slice();
    function kbSearch() {
      var query = _kbSearchQuery;
      var layer = _kbSelectedLayer;
      var typeFilter = _kbSelectedType;
      var filtered = _snapshotAllFacts;
      if (layer && layer !== 'all') filtered = filtered.filter(function(f) { return f.layer === layer || (f.type || '').startsWith(layer); });
      if (typeFilter) filtered = filtered.filter(function(f) { return f.type === typeFilter; });
      if (query) { var q = query.toLowerCase(); filtered = filtered.filter(function(f) { return ((f.title || '') + ' ' + (f.body || '') + ' ' + (f.slug || '')).toLowerCase().indexOf(q) >= 0; }); }
      _kbFacts = filtered;
      _kbSelectedFact = null;
      renderKnowledgeFacts();
    }
    _kbCache = ${kbStatsRaw};
    if (_kbCache && _kbCache.enabled) _kbSetupView = 'none';
    _kbInitialLoad = false;
    _kbRendered = true;
    renderKnowledge();

    // Contributors — API returns {contributors: [...]}
    var _snapContributors = ${contributorsRaw};
    _cachedContributors = _snapContributors && _snapContributors.contributors ? _snapContributors.contributors : (Array.isArray(_snapContributors) ? _snapContributors : []);
    renderContributorFilters(_cachedContributors);
    renderContributors(_cachedContributors);

    // Leaderboard
    var _snapLeaderboard = ${leaderboardRaw};
    var _lbEntries = _snapLeaderboard && _snapLeaderboard.leaderboard ? _snapLeaderboard.leaderboard : (Array.isArray(_snapLeaderboard) ? _snapLeaderboard : []);
    renderLeaderboard(_lbEntries);

    // Fix leaderboard link to use snapshot base path
    document.querySelectorAll('a[href="/leaderboard"]').forEach(function(a) {
      a.href = '${basePath}/leaderboard';
    });

    // Fix API docs link to use snapshot base path
    document.querySelectorAll('a[href="/api-docs"]').forEach(function(a) {
      a.href = '${basePath}/api-docs';
    });

    // Git version
    const _v = ${versionRaw};
    const _gv = document.getElementById('git-version');
    if (_gv && _v.short) {
      let _html = '<span style="color:inherit">' + _v.short + '</span>';
      if (_v.dirty) _html += ' <span class="git-dirty">*</span>';
      if (_v.behind > 0) _html += ' <span class="git-behind">' + _v.behind + ' behind</span>';
      _gv.innerHTML = _html;
    }

    // Format snapshot timestamp
    const _snapTs = '${snapshotTs}';
    const _snapEl = document.getElementById('snap-time');
    if (_snapEl) {
      const d = new Date(_snapTs);
      _snapEl.textContent = d.toLocaleDateString([], {month:'short',day:'numeric',year:'numeric'}) +
        ' ' + d.toLocaleTimeString([], {hour:'numeric',minute:'2-digit',hour12:true});
    }

    // Auto-refresh countdown
    (function() {
      const REFRESH_SEC = ${AUTO_REFRESH_SECONDS};
      const el = document.getElementById('snap-refresh');
      if (!el) return;
      let remaining = REFRESH_SEC;
      function fmt(s) {
        const m = Math.floor(s / 60);
        const sec = s % 60;
        return m > 0 ? m + 'm ' + (sec < 10 ? '0' : '') + sec + 's' : sec + 's';
      }
      function tick() {
        el.textContent = '\\u{1F504} refreshes in ' + fmt(remaining);
        if (remaining <= 0) return;
        remaining--;
        setTimeout(tick, 1000);
      }
      tick();
    })();

    // Disable all interactive functions in snapshot mode
    function kick() {}
    function ocSendKick() {}
    function switchCli() {}
    function switchModel() {}
    function toggleAgent() {}
    function restartAgent() {}
    function resetRestarts() {}
    function togglePin() {}
    function openConfigDialog() {}
    function closeConfigDialog() {}
    function saveConfig() {}
    function toggleLayout() {}
    function toggleKnowledge() {}
    function fetchKnowledgeStats() {}
    function nousSetMode() {}
    function nousSetScope() {}
    function nousApprove() {}
    function nousReject() {}
    function nousAbort() {}
  `;

  const output = [
    headAndStyles,
    staticCss,
    '\n  </style>\n  ' + metaRefresh + '\n</head>\n<body>',
    banner,
    bodyContent,
    '  <script>',
    renderFunctions,
    initScript,
    '  </script>\n</body>\n</html>',
  ].join('\n');

  writeFileSync(outputFile, output);
  const size = Buffer.byteLength(output);
  console.log(`${snapshotMode} snapshot written to ${outputFile} (${size} bytes)`);
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});

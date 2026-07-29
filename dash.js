window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");gtag("event","access_denied",{hive_id:"%s"});
;
window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");
;

    function esc(s) { var d = document.createElement('div'); d.textContent = s || ''; return d.innerHTML; }

    // sameShaJS: two git SHAs are the same commit even if one is a short
    // prefix of the other. The hub stores 7-char short SHAs while a spoke may
    // report a longer one; a raw === left the "Upgrading" badge spinning
    // forever on a hive already at the target. Mirrors hub sameCommit().
    function sameShaJS(a, b) {
      if (!a || !b) return false;
      var n = Math.min(a.length, b.length);
      return a.slice(0, n).toLowerCase() === b.slice(0, n).toLowerCase();
    }

    function dismissBanner(key, btn) {
      var dismissed = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}');
      dismissed[key] = Date.now();
      localStorage.setItem('hive-dismissed-banners', JSON.stringify(dismissed));
      btn.parentNode.remove();
    }

    function hiveToast(msg, type) {
      var t = document.createElement('div');
      t.className = 'hive-toast ' + (type || 'info');
      t.textContent = msg;
      var toastBaseTop = 70;
      var toastGap = 8;
      var existing = document.querySelectorAll('.hive-toast');
      var offset = 0;
      existing.forEach(function(e) { offset += e.offsetHeight + toastGap; });
      t.style.top = (toastBaseTop + offset) + 'px';
      document.body.appendChild(t);
      setTimeout(function() { t.remove(); }, 4000);
    }

    function hiveConfirm(msg, rawHTML) {
      return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.className = 'hive-confirm-overlay';
        overlay.innerHTML = '<div class="hive-confirm"><p>' + (rawHTML ? msg : esc(msg)) + '</p><div class="hive-confirm-btns">' +
          '<button style="padding:8px 16px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>' +
          '<button style="padding:8px 16px;background:var(--red);color:#fff;border:none;border-radius:6px;cursor:pointer" id="hive-confirm-ok">Confirm</button></div></div>';
        document.body.appendChild(overlay);
        var done = false;
        function finish(val) { if (done) return; done = true; overlay.remove(); document.removeEventListener('keydown', onKey); resolve(val); }
        function onKey(e) { if (e.key === 'Escape') finish(false); if (e.key === 'Enter') finish(true); }
        document.addEventListener('keydown', onKey);
        overlay.querySelector('#hive-confirm-ok').onclick = function() { finish(true); };
        overlay.querySelector('button:first-child').onclick = function() { finish(false); };
        overlay.querySelector('#hive-confirm-ok').focus();
      });
    }

    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      var createModal = document.getElementById('create-modal');
      if (createModal && createModal.style.display === 'flex') { createModal.style.display = 'none'; return; }
      var requestModal = document.getElementById('request-modal');
      if (requestModal && requestModal.style.display === 'flex') { requestModal.style.display = 'none'; return; }
      var accessOverlay = document.querySelector('.hive-confirm-overlay');
      if (accessOverlay) { accessOverlay.remove(); return; }
      var accessModal = document.getElementById('access-modal');
      if (accessModal && accessModal.style.display === 'flex') { accessModal.style.display = 'none'; }
    });

    var ACMM_LABELS = {1:'L1 Assisted',2:'L2 Instructed',3:'L3 Measured',4:'L4 Adaptive',5:'L5 Semi-Automated',6:'L6 Autonomous'};
    function sparkline(points, color, w, h) {
      if (!points || points.length < 2) return '';
      var vals = points.map(function(p) { return p.v; });
      var mn = Math.min.apply(null, vals);
      var mx = Math.max.apply(null, vals);
      var range = mx - mn || 1;
      var sw = w || 60;
      var sh = h || 16;
      var step = sw / (vals.length - 1);
      var pts = vals.map(function(v, i) {
        return (i * step).toFixed(1) + ',' + (sh - ((v - mn) / range) * sh).toFixed(1);
      }).join(' ');
      return '<svg width="' + sw + '" height="' + sh + '" style="vertical-align:middle;margin-right:4px"><polyline points="' + pts + '" fill="none" stroke="' + (color || '#6b7280') + '" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
    }

    function acmmBadge(level) {
      var l = level || 0;
      var tips = {1:'L1 Assisted — Advisory only.',2:'L2 Instructed — Advisory beads, no GitHub writes.',3:'L3 Measured — Hold-gated PRs, CI gates.',4:'L4 Adaptive — Agents open issues, sec-check.',5:'L5 Semi-Automated — PRs with hold label, batch review.',6:'L6 Autonomous — Auto-merge on green CI.'};
      return '<span class="acmm-badge acmm-' + l + '" title="' + esc(tips[l] || '') + '">' + (ACMM_LABELS[l] || 'L' + l) + '</span>';
    }
    function roleBadge(role) {
      var cls = role === 'owner' ? 'role-owner' : role === 'read-write' ? 'role-read-write' : 'role-read';
      return '<span class="role-badge ' + cls + '">' + esc(role) + '</span>';
    }
    function fmtTokens(n) {
      n = Number(n) || 0;
      if (n <= 0) return '<span style="color:var(--muted)">—</span>';
      if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
      if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
      if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
      return String(n);
    }
    function clusterBadge(clusterId, clusterName) {
      var cid = clusterId || 'hive-oke';
      var isGPU = cid === 'vllm-d' || (clusterName || '').toLowerCase().indexOf('gpu') >= 0;
      var bg = isGPU ? 'rgba(34,197,94,0.15)' : 'rgba(59,130,246,0.15)';
      var color = isGPU ? '#3fb950' : '#60a5fa';
      var border = isGPU ? 'rgba(34,197,94,0.3)' : 'rgba(59,130,246,0.3)';
      var title = clusterName || cid;
      return '<span class="cluster-badge" style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:' + bg + ';color:' + color + ';border:1px solid ' + border + '" title="' + esc(title) + '">' + esc(cid) + '</span>';
    }
    function modeBadge(mode) {
      var m = (mode || 'idle').toUpperCase();
      var levels = {IDLE:0, QUIET:1, BUSY:2, SURGE:3};
      var colors = {IDLE:'#6b7280', QUIET:'#3b82f6', BUSY:'#f59e0b', SURGE:'#ef4444'};
      var fill = levels[m] !== undefined ? levels[m] : 0;
      var c = colors[m] || '#6b7280';
      var bars = '';
      for (var i = 0; i < 4; i++) {
        var h = 6 + i * 4;
        var bc = i <= fill ? c : '#1e1e2e';
        bars += '<rect x="' + (i * 6) + '" y="' + (20 - h) + '" width="4" height="' + h + '" rx="1" fill="' + bc + '"/>';
      }
      return '<span title="' + m + '" style="display:inline-flex;align-items:center;gap:4px"><svg width="24" height="20" viewBox="0 0 24 20">' + bars + '</svg><span style="font-size:0.7rem;color:' + c + ';font-weight:600">' + m + '</span></span>';
    }
    /* Status-filter keys for the My Hives list. Kept as named constants so the
       filter chips, the classifier and the persisted selection can never drift
       apart on a typo'd string literal. */
    var HIVE_FILTER_APP_MISSING = 'app-missing';
    var HIVE_FILTER_NO_TOKENS = 'no-tokens';
    var HIVE_FILTER_DEGRADED = 'degraded';
    var HIVE_FILTER_OK = 'ok';

    /* A hive has "used no tokens" when its last heartbeat reported a cumulative
       token total of zero — the same value the Tokens column renders as "—". */
    var NO_TOKENS_THRESHOLD = 0;

    /* hiveStatusFlags derives the four filterable states from data the hub
       ALREADY has per hive (all reported over the spoke heartbeat):
         - githubAppRequired / githubAppPermIssue  (RegistryEntry, server.go)
         - health.status                            (RegistryEntry.Health)
         - totalTokens24h                           (RegistryEntry)
         - online                                   (RegistryEntry)
       The GitHub-App and degraded rules deliberately mirror healthBadge()
       exactly, so a row's dot and the chip it matches can never disagree. */
    function hiveStatusFlags(h) {
      h = h || {};
      var hp = h.health || {};
      var st = hp.status || 'unknown';
      /* githubAppRequired means the spoke wants the App but it is not usable:
         with a perm issue it is installed-but-insufficient, without one it is
         not installed at all. healthBadge() forces "degraded" in both cases. */
      var appMissing = !!h.githubAppRequired && !h.githubAppPermIssue;
      var degraded = !!h.githubAppRequired || st === 'degraded' || st === 'critical';
      /* An offline hive has no live reading at all, so it is not "OK" even if
         its last stored health snapshot said ok. */
      var ok = !degraded && st === 'ok' && !!h.online;
      return {
        appMissing: appMissing,
        noTokens: (Number(h.totalTokens24h) || 0) <= NO_TOKENS_THRESHOLD,
        degraded: degraded,
        ok: ok
      };
    }

    // uptimeCell renders process uptime for the Uptime column.
    //
    // This used to be an inline pill next to the hive name, which wrapped long
    // org names onto a second line. In its own column it can always show a
    // value, so the column reads as real data rather than an occasional
    // warning — but the COLOUR still carries the signal, since a hive can sit
    // at 1/1 Running while restarting every few minutes and otherwise look
    // perfectly healthy.
    function uptimeCell(h) {
      // Restart just happened; red. Matches the old inline pill's threshold.
      var RECENT_RESTART_SECS = 600;   // 10 min
      // Below this, still worth noticing — the pod has not settled yet.
      var SETTLING_SECS = 3600;        // 1 hour
      var SHOW_SECONDS_BELOW = 90;     // finer granularity while very fresh
      var HOUR_SECS = 3600, DAY_SECS = 86400;
      if (!h.startedAt) return '<span style="color:var(--muted)">—</span>';
      var secs = (Date.now() - new Date(h.startedAt).getTime()) / 1000;
      if (!isFinite(secs) || secs < 0) return '<span style="color:var(--muted)">—</span>';
      var label;
      if (secs < SHOW_SECONDS_BELOW) label = Math.round(secs) + 's';
      else if (secs < HOUR_SECS) label = Math.round(secs / 60) + 'm';
      else if (secs < DAY_SECS) label = (secs / HOUR_SECS).toFixed(1).replace(/\.0$/, '') + 'h';
      else label = Math.floor(secs / DAY_SECS) + 'd';
      var c = secs < RECENT_RESTART_SECS ? 'var(--red)'
            : (secs < SETTLING_SECS ? 'var(--yellow, #d29922)' : 'var(--muted)');
      var title = secs < SETTLING_SECS
        ? 'Restarted recently — a short value that keeps resetting means the pod is restarting'
        : 'Process uptime since the last restart';
      return '<span title="' + esc(title) + '" style="font-size:0.72rem;color:' + c + ';cursor:help">' + esc(label) + '</span>';
    }
    function healthBadge(h) {
      var hp = h.health || {};
      var st = hp.status || 'unknown';
      var colors = {ok:'#3fb950',warning:'#d29922',degraded:'#f85149',critical:'#ff4040',unknown:'#6b7280'};
      var icons = {ok:'✓',warning:'⚠',degraded:'⚠',critical:'✕',unknown:'?'};
      var checkIcons = {pass:'✓',fail:'✕',warn:'⚠',skip:'–'};
      var c = colors[st] || colors.unknown;
      var ic = icons[st] || '?';
      var isUpgrading = _upgradingHives[h.id];
      var statusLabel = isUpgrading ? 'Starting up after upgrade' : st.charAt(0).toUpperCase() + st.slice(1);
      var checks = hp.checks || [];
      var lines = [statusLabel];
      for (var i = 0; i < checks.length; i++) {
        var ck = checks[i];
        var ci = checkIcons[ck.status] || '?';
        var line = ci + ' ' + ck.name;
        if (ck.detail) line += ': ' + ck.detail;
        lines.push(line);
      }
      if (h.githubAppRequired && h.githubAppPermIssue) { lines.push('✓ GitHub App installed'); lines.push('⚠ GitHub App: permissions insufficient'); st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel; }
      else if (h.githubAppRequired) { lines.push('✕ GitHub App not installed'); st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel; }
      else if (!h.githubAppRequired) { lines.push('✓ GitHub App installed'); }
      if (!checks.length) lines.push('No check data');
      // Heartbeat freshness — so a reading that is minutes old isn't mistaken
      // for current health (the source of "stuck Degraded" reports: the last
      // heartbeat carried a transient failure and never got refreshed).
      if (h.lastHeartbeat) {
        var ageMs = Date.now() - new Date(h.lastHeartbeat).getTime();
        if (!isNaN(ageMs) && ageMs >= 0) {
          var ageMin = Math.floor(ageMs / 60000);
          var ageStr = ageMin < 1 ? 'just now' : (ageMin === 1 ? '1 min ago' : ageMin + ' min ago');
          lines.push('— as of ' + ageStr);
          // A reading older than 2× the heartbeat interval is stale; don't
          // present it as a live status.
          var staleAfterMs = 5 * 60000;
          if (ageMs > staleAfterMs && st !== 'unknown') {
            statusLabel = statusLabel + ' (stale)';
            lines[0] = statusLabel;
            c = colors.warning; ic = icons.warning;
          }
        }
      }
      var access = h.access || [];
      var dotMarkup = '<span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:' + c + '"></span>' +
        '<span style="font-size:0.7rem;color:' + c + ';font-weight:600">' + ic + '</span>';

      // No access list (not an owner of this row): nothing to render beyond the
      // health lines, so the native title tooltip is enough and cheapest.
      if (!access.length) {
        return '<span title="' + esc(lines.join('\n')) + '" style="display:inline-flex;align-items:center;gap:4px;cursor:help;white-space:pre-line">' + dotMarkup + '</span>';
      }

      // ONE panel holding health AND access. Setting a title attribute here too
      // would make the browser draw its own tooltip on top of this panel — two
      // overlapping boxes saying different things, which is exactly what the
      // first cut of this did.
      var healthRows = (lines || []).map(function(l, i) {
        // lines[0] is the status word ("Warning", "Degraded", ...); render it as
        // the panel's title in the status colour, the rest as check lines.
        if (i === 0) {
          return '<span style="display:block;color:' + c + ';font-weight:600;margin-bottom:4px">' + esc(l) + '</span>';
        }
        return '<div style="padding:1px 0;color:var(--muted)">' + esc(l) + '</div>';
      }).join('');

      var accessRows = access.map(function(a) {
        var rc = a.role === 'owner' ? '#d29922' : (a.role === 'read-write' ? '#3fb950' : '#6b7280');
        return '<div style="display:flex;align-items:center;gap:6px;padding:2px 0">' +
          '<img src="https://github.com/' + esc(a.username) + '.png?size=40" alt="" ' +
          'style="width:18px;height:18px;border-radius:50%;flex:0 0 auto" ' +
          'onerror="this.style.visibility=\'hidden\'">' +
          '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(a.username) + '</span>' +
          '<span style="color:' + rc + ';font-size:0.62rem;font-weight:600;white-space:nowrap">' + esc(a.role) + '</span>' +
          '</div>';
      }).join('');
      var heading = access.length === 1 ? '1 user with access' : access.length + ' users with access';

      return '<span class="hive-access-wrap" style="position:relative;display:inline-flex;align-items:center;gap:4px;cursor:help">' + dotMarkup +
        '<span class="hive-access-pop" style="display:none;position:absolute;left:0;top:calc(100% + 6px);z-index:60;' +
        'min-width:210px;max-width:300px;padding:8px 10px;border-radius:8px;border:1px solid var(--border);' +
        'background:var(--surface);box-shadow:0 6px 20px rgba(0,0,0,0.35);font-size:0.72rem;text-align:left;font-weight:400;white-space:normal">' +
        healthRows +
        '<span style="display:block;border-top:1px solid var(--border);margin:6px 0 4px"></span>' +
        '<span style="display:block;color:var(--muted);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.04em;margin-bottom:4px">' + esc(heading) + '</span>' +
        accessRows + '</span></span>';
    }
    function dashboardLink(h) {
      var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      // Open hosted spokes via the hub's SSO handoff endpoint so the user's hub
      // login follows them to the spoke (no second GitHub login), including for
      // direct-route/firewalled spokes that the hub nginx can't front. The
      // endpoint mints a signed, single-hive token and 302s to the spoke's /sso;
      // if SSO can't be used it falls back to the plain dashboard URL. The
      // visible label still shows the spoke host.
      if (isHosted && h.id) {
        var ssoHref = '/api/saas/hives/' + encodeURIComponent(h.id) + '/open';
        var label = (h.dashboardUrl && !h.dashboardUrl.includes('localhost'))
          ? h.dashboardUrl.replace(/^https?:\/\//, '').substring(0, 30) + '...'
          : esc(h.id) + '.hive...';
        return '<a href="' + ssoHref + '" target="_blank" class="dash-link">' + esc(label) + '</a>';
      }
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost'))
        return '<a href="' + esc(h.dashboardUrl) + '" target="_blank" class="dash-link">' + esc(h.dashboardUrl.replace(/^https?:\/\//,'').substring(0,30)) + '...</a>';
      return '<span style="color:var(--muted);font-size:0.75rem">—</span>';
    }
    function snapshotLink(h) {
      if (h.snapshotUrl) return '<a href="' + esc(h.snapshotUrl) + '" target="_blank" class="dash-link">↗</a>';
      return '';
    }
    function apiLink(h) {
      var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      var base = '';
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost')) {
        base = esc(h.dashboardUrl);
      } else if (isHosted) {
        base = 'https://' + esc(h.id) + '.hive.kubestellar.io';
      }
      if (!base) return '';
      return '<a href="' + base + '/api/docs" target="_blank" style="padding:3px 10px;background:rgba(88,166,255,0.15);color:#58a6ff;border:1px solid rgba(88,166,255,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;white-space:nowrap">API ↗</a>';
    }
    function resolvedBase(h) {
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost')) return h.dashboardUrl;
      var isH = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      if (isH) return 'https://' + h.id + '.hive.kubestellar.io';
      return '';
    }

    async function loadUser() {
      try {
        var resp = await fetch('/api/auth/user');
        var data = await resp.json();
        if (data.authenticated) {
          _isAdmin = !!data.hub_admin;
          var roleText = data.hub_admin ? 'Hub Admin' : 'User';
          document.getElementById('nav-user').innerHTML =
            '<img src="' + esc(data.avatar_url) + '" class="nav-avatar" title="' + esc(data.login) + ' — ' + roleText + '">' +
            '<span style="font-size:0.85rem">' + esc(data.login) + '</span>' +
            '<span style="font-size:0.65rem;color:var(--muted);margin-left:6px">' + roleText + '</span>';
        }
      } catch(e) {}
    }

    var _userQuota = 0, _userUsed = 0, _isAdmin = false;
    var _latestSHA = '';
    var _latestSHAs = {};
    var _latestSHAMessages = {};
    var _latestImageStatus = {};
    var _trackedBranchesList = [];
    var _clusterList = [];
    var _commitMessages = {};
    var _allDashHives = [];
    var _dashSortKey = '', _dashSortAsc = true;
    var _hivesLoading = false;
    var _lastHivesJSON = '';
    var _lastUsersJSON = '';

    /* Active status filters, keyed by HIVE_FILTER_*. Multi-select: several may
       be on at once and a hive matches if it satisfies ANY active one (OR), so
       turning on more chips widens the list rather than narrowing it to nothing
       — "Degraded" and "OK" are mutually exclusive and an AND would always be
       empty. Empty object = no filtering, show everything. */
    var _dashStatusFilters = {};

    /* Chip definitions, in display order. Colours reuse the health palette from
       healthBadge() so a chip reads as the same state as the row dot. */
    var HIVE_FILTER_CHIPS = [
      {key: HIVE_FILTER_APP_MISSING, label: 'GitHub App not installed', color: '#f85149'},
      {key: HIVE_FILTER_NO_TOKENS, label: 'No tokens used', color: '#6b7280'},
      {key: HIVE_FILTER_DEGRADED, label: 'Degraded', color: '#f85149'},
      {key: HIVE_FILTER_OK, label: 'OK', color: '#3fb950'}
    ];

    /* hiveMatchesFilters answers whether a hive survives the active chips. */
    function hiveMatchesFilters(h) {
      var active = Object.keys(_dashStatusFilters || {}).filter(function(k) { return _dashStatusFilters[k]; });
      if (!active.length) return true;
      var f = hiveStatusFlags(h);
      var byKey = {};
      byKey[HIVE_FILTER_APP_MISSING] = f.appMissing;
      byKey[HIVE_FILTER_NO_TOKENS] = f.noTokens;
      byKey[HIVE_FILTER_DEGRADED] = f.degraded;
      byKey[HIVE_FILTER_OK] = f.ok;
      for (var i = 0; i < active.length; i++) {
        if (byKey[active[i]]) return true;
      }
      return false;
    }

    /* applyDashFilters filters the hives the caller wants rendered. It applies
       the status chips AND, independently, the active alert-type filter — the
       two compose as an AND (chips narrow by state, the alert filter narrows to
       the hives carrying that alert), which is what "click an alert type to see
       those hives" has to mean when a chip is already on. */
    function applyDashFilters(hives) {
      return (hives || []).filter(function(h) {
        return hiveMatchesFilters(h) && hiveMatchesAlertFilter(h);
      });
    }

    /* ── Fleet alerts ("Attention needed") ────────────────────────────────
       Server-evaluated (see alerts.go); the client only renders and filters.
       Deliberately NOT recomputed here: a second, drifting implementation of
       "what is wrong" is exactly how the panel and the rows start disagreeing. */

    /* EMPTY_ALERT_SUMMARY is the shape every consumer can assume. Frozen so a
       caller cannot accidentally mutate the shared fallback. */
    var EMPTY_ALERT_SUMMARY = {alerts: [], countsBySeverity: {}, countsByType: {}, total: 0, acknowledgedTotal: 0};

    var _fleetAlerts = EMPTY_ALERT_SUMMARY;
    /* Active alert-type filter: '' = no alert filtering. Single-select, unlike
       the status chips — "show me the crash-looping hives" is a drill-down, and
       OR-ing several alert types back together just reproduces the full list. */
    var _alertTypeFilter = '';
    /* Whether the acknowledged alerts are expanded into view. */
    var _alertShowAcked = false;

    /* Severity display order + colour, mirroring alerts.go's ranking. */
    var ALERT_SEVERITIES = [
      {key: 'critical', label: 'Critical', color: '#f85149'},
      {key: 'warning', label: 'Warning', color: '#f59e0b'},
      {key: 'info', label: 'Info', color: '#60a5fa'}
    ];

    /* Human labels for the alert type chips. Keys MUST match the AlertType*
       constants in alerts.go; an unknown type falls back to its raw key rather
       than rendering blank. */
    var ALERT_TYPE_LABELS = {
      'crash-loop': 'Crash-looping',
      'offline': 'Offline',
      'stuck-upgrade': 'Stuck upgrade',
      'health-check-failing': 'Health check failing',
      'token-burn': 'Token burn',
      'provision-error': 'Provision error'
    };

    /* How many alert rows are listed before the panel collapses the remainder
       behind a "show all" affordance. Enough to act on, few enough that the
       panel never pushes the hive list off the screen. */
    var ALERT_ROWS_SHOWN = 6;

    function alertTypeLabel(t) { return ALERT_TYPE_LABELS[t] || t || 'Unknown'; }

    /* alertsForType returns the UNACKNOWLEDGED alerts of one type. Acknowledged
       ones are excluded so filtering by a type never selects hives whose alert
       the operator has already dealt with. */
    function alertsForType(t) {
      return ((_fleetAlerts && _fleetAlerts.alerts) || []).filter(function(a) {
        return a && !a.acknowledged && a.type === t;
      });
    }

    /* hiveMatchesAlertFilter answers whether a hive survives the active alert
       drill-down. No filter = everything passes. */
    function hiveMatchesAlertFilter(h) {
      if (!_alertTypeFilter) return true;
      if (!h || !h.id) return false;
      var matches = alertsForType(_alertTypeFilter);
      for (var i = 0; i < matches.length; i++) {
        if (matches[i].hiveId === h.id) return true;
      }
      return false;
    }

    /* toggleAlertFilter drills into (or back out of) one alert type. Clicking
       the active chip again clears it. */
    function toggleAlertFilter(t) {
      _alertTypeFilter = (_alertTypeFilter === t) ? '' : t;
      renderHives(_allDashHives, true);
    }

    function clearAlertFilter() {
      _alertTypeFilter = '';
      renderHives(_allDashHives, true);
    }

    /* clearAllHiveFilters resets BOTH filter mechanisms at once. The empty-state
       escape hatch uses it, because either one alone can empty the list. */
    function clearAllHiveFilters() {
      _dashStatusFilters = {};
      _alertTypeFilter = '';
      renderHives(_allDashHives, true);
    }

    function toggleAlertAcked() {
      _alertShowAcked = !_alertShowAcked;
      renderHives(_allDashHives, true);
    }

    /* alertAge renders how long a condition has been firing, from the server's
       firstSeen. Rounded coarsely so it does not churn every poll. */
    function alertAge(firstSeen) {
      if (!firstSeen) return '';
      var ms = Date.now() - new Date(firstSeen).getTime();
      if (!isFinite(ms) || ms < 0) return '';
      var MIN = 60000, HOUR = 3600000, DAY = 86400000;
      if (ms < MIN) return 'just now';
      if (ms < HOUR) return Math.round(ms / MIN) + 'm';
      if (ms < DAY) return Math.round(ms / HOUR) + 'h';
      return Math.floor(ms / DAY) + 'd';
    }

    /* ackAlert silences (or un-silences) one alert. Admin-only server-side; the
       button is only rendered for admins, but the server is the real gate. */
    async function ackAlert(hiveId, type, clear) {
      try {
        var resp = await fetch('/api/saas/admin/alert-ack', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({hiveId: hiveId, type: type, clear: !!clear})
        });
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) {
          hiveToast(data.error || 'Failed to update alert', 'error');
          return;
        }
        hiveToast(clear ? 'Alert restored' : 'Alert acknowledged', 'success');
        /* Re-fetch rather than mutating locally: the server owns whether an ack
           actually applies (it refuses one for a condition that is not live). */
        loadHives();
      } catch (e) {
        hiveToast('Error: ' + e.message, 'error');
      }
    }

    /* renderAlertsPanel draws the "Attention needed" panel above the hive list.
       When the fleet is clean it renders one quiet line instead of hiding
       entirely, so the operator can trust that the check actually ran. */
    function renderAlertsPanel() {
      var panel = document.getElementById('fleet-alerts-panel');
      if (!panel) return;
      var summary = _fleetAlerts || EMPTY_ALERT_SUMMARY;
      var all = summary.alerts || [];
      var counts = summary.countsBySeverity || {};
      var byType = summary.countsByType || {};
      var total = Number(summary.total) || 0;
      var ackedTotal = Number(summary.acknowledgedTotal) || 0;

      /* Nothing at all to say — and no hives yet — stay out of the way. */
      if (!total && !ackedTotal && !(_allDashHives || []).length) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';

      if (!total) {
        var cleanNote = ackedTotal
          ? ' <button type="button" class="alert-panel-more" onclick="toggleAlertAcked()">' +
            ackedTotal + ' acknowledged</button>'
          : '';
        var ackedRows = (_alertShowAcked && ackedTotal) ? renderAlertRows(all, true) : '';
        panel.innerHTML = '<div class="alert-panel">' +
          '<div class="alert-panel-clean"><span style="color:#3fb950">&#10003;</span>' +
          '<span>Nothing needs your attention.</span>' + cleanNote + '</div>' +
          ackedRows + '</div>';
        return;
      }

      var cls = 'alert-panel';
      if (counts.critical) cls += ' has-critical';
      else if (counts.warning) cls += ' has-warning';

      var pills = (ALERT_SEVERITIES || []).map(function(s) {
        var n = Number(counts[s.key]) || 0;
        if (!n) return '';
        return '<span class="alert-sev-pill" style="--alert-color:' + s.color + '">' +
          n + ' ' + esc(s.label) + '</span>';
      }).join('');

      /* Type chips, ordered by the severity of the alerts they represent so the
         most urgent drill-down is leftmost. */
      var typeKeys = Object.keys(byType).filter(function(k) { return Number(byType[k]) > 0; });
      typeKeys.sort(function(a, b) {
        var ra = alertTypeWorstSeverityRank(a), rb = alertTypeWorstSeverityRank(b);
        if (ra !== rb) return ra - rb;
        return a < b ? -1 : (a > b ? 1 : 0);
      });
      var chips = typeKeys.map(function(k) {
        var on = _alertTypeFilter === k;
        var color = ALERT_SEVERITY_COLORS[alertTypeWorstSeverity(k)] || '#8b949e';
        return '<button type="button" class="filter-chip' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleAlertFilter(\'' + esc(k) + '\')" style="--chip-color:' + color + '">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          esc(alertTypeLabel(k)) +
          '<span class="filter-chip-count">' + (Number(byType[k]) || 0) + '</span></button>';
      }).join('');
      var clearChip = _alertTypeFilter
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAlertFilter()">Show all hives</button>'
        : '';

      var ackNote = ackedTotal
        ? '<button type="button" class="alert-panel-more" onclick="toggleAlertAcked()">' +
          (_alertShowAcked ? 'Hide' : 'Show') + ' ' + ackedTotal + ' acknowledged</button>'
        : '';

      panel.innerHTML = '<div class="' + cls + '">' +
        '<div class="alert-panel-head">' +
          '<div class="alert-panel-title"><span>&#9888;</span><span>Attention needed</span></div>' +
          '<div style="display:flex;gap:6px;flex-wrap:wrap">' + pills + '</div>' +
        '</div>' +
        '<div class="alert-type-chips">' + chips + clearChip + '</div>' +
        renderAlertRows(all, false) +
        (_alertShowAcked ? renderAlertRows(all, true) : '') +
        ackNote +
      '</div>';
    }

    /* ALERT_SEVERITY_COLORS maps a severity to its chip colour, derived from
       ALERT_SEVERITIES so the two can never disagree. */
    var ALERT_SEVERITY_COLORS = (function() {
      var m = {};
      (ALERT_SEVERITIES || []).forEach(function(s) { m[s.key] = s.color; });
      return m;
    })();

    /* alertTypeWorstSeverity / ...Rank find the most urgent severity currently
       present for a type, so a type chip is coloured by the worst thing in it. */
    function alertTypeWorstSeverity(t) {
      var worst = '', worstRank = ALERT_SEVERITIES.length;
      alertsForType(t).forEach(function(a) {
        var r = alertSeverityRankJS(a.severity);
        if (r < worstRank) { worstRank = r; worst = a.severity; }
      });
      return worst;
    }
    function alertTypeWorstSeverityRank(t) {
      var r = alertSeverityRankJS(alertTypeWorstSeverity(t));
      return r;
    }
    /* alertSeverityRankJS mirrors alerts.go's ordering; unknown sorts last. */
    function alertSeverityRankJS(sev) {
      for (var i = 0; i < ALERT_SEVERITIES.length; i++) {
        if (ALERT_SEVERITIES[i].key === sev) return i;
      }
      return ALERT_SEVERITIES.length;
    }

    /* renderAlertRows lists individual alerts. acked selects which half of the
       list is drawn: the live alerts, or the acknowledged ones. */
    function renderAlertRows(all, acked) {
      var rows = (all || []).filter(function(a) { return a && !!a.acknowledged === !!acked; });
      /* When a type drill-down is active, the rows follow it too — otherwise the
         panel would still list alerts for hives the table is no longer showing. */
      if (_alertTypeFilter) {
        rows = rows.filter(function(a) { return a.type === _alertTypeFilter; });
      }
      if (!rows.length) return '';
      var shown = rows.slice(0, ALERT_ROWS_SHOWN);
      var html = shown.map(function(a) {
        var color = ALERT_SEVERITY_COLORS[a.severity] || '#8b949e';
        var age = alertAge(a.firstSeen);
        /* Every interpolated value below is spoke- or admin-supplied and is
           escaped: hive names, reasons (which embed check names and provision
           error text), and the acking username. */
        var ackBtn = '';
        if (_isAdmin) {
          ackBtn = acked
            ? '<button type="button" class="alert-ack-btn" onclick="ackAlert(\'' +
              esc(a.hiveId) + '\',\'' + esc(a.type) + '\',true)">Restore</button>'
            : '<button type="button" class="alert-ack-btn" onclick="ackAlert(\'' +
              esc(a.hiveId) + '\',\'' + esc(a.type) + '\',false)">Acknowledge</button>';
        }
        var ackedBy = (acked && a.ackBy) ? '<span class="alert-row-reason">— ack by ' + esc(a.ackBy) + '</span>' : '';
        return '<div class="alert-row' + (acked ? ' acked' : '') + '">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          '<span class="alert-row-hive">' + esc(a.hiveName || a.hiveId) + '</span>' +
          '<span class="alert-row-reason">' + esc(a.reason) + '</span>' + ackedBy +
          '<span class="alert-row-age">' + esc(age) + '</span>' + ackBtn +
        '</div>';
      }).join('');
      var more = rows.length > ALERT_ROWS_SHOWN
        ? '<div class="alert-row-reason" style="font-size:0.7rem;padding-left:8px">+' +
          (rows.length - ALERT_ROWS_SHOWN) + ' more</div>'
        : '';
      return '<div class="alert-rows">' + html + more + '</div>';
    }

    /* toggleStatusFilter flips one chip and re-renders from the unfiltered
       cache, so chips compose with whatever sort is currently applied. */
    function toggleStatusFilter(key) {
      _dashStatusFilters[key] = !_dashStatusFilters[key];
      if (!_dashStatusFilters[key]) delete _dashStatusFilters[key];
      renderHives(_allDashHives, true);
    }

    function clearStatusFilters() {
      _dashStatusFilters = {};
      renderHives(_allDashHives, true);
    }

    /* renderStatusFilterBar draws the chip row plus the match count. Counts are
       computed over the FULL hive list (not the filtered one) so each chip
       always advertises how many hives it would show. */
    function renderStatusFilterBar(allHives, shownCount) {
      var bar = document.getElementById('hive-filter-bar');
      if (!bar) return;
      var counts = {};
      counts[HIVE_FILTER_APP_MISSING] = 0;
      counts[HIVE_FILTER_NO_TOKENS] = 0;
      counts[HIVE_FILTER_DEGRADED] = 0;
      counts[HIVE_FILTER_OK] = 0;
      (allHives || []).forEach(function(h) {
        var f = hiveStatusFlags(h);
        if (f.appMissing) counts[HIVE_FILTER_APP_MISSING]++;
        if (f.noTokens) counts[HIVE_FILTER_NO_TOKENS]++;
        if (f.degraded) counts[HIVE_FILTER_DEGRADED]++;
        if (f.ok) counts[HIVE_FILTER_OK]++;
      });
      var chips = (HIVE_FILTER_CHIPS || []).map(function(c) {
        var on = !!_dashStatusFilters[c.key];
        var cls = on ? 'filter-chip on' : 'filter-chip';
        return '<button type="button" class="' + cls + '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleStatusFilter(\'' + esc(c.key) + '\')" style="--chip-color:' + c.color + '">' +
          '<span class="filter-chip-dot" style="background:' + c.color + '"></span>' +
          esc(c.label) + '<span class="filter-chip-count">' + counts[c.key] + '</span></button>';
      }).join('');
      var anyActive = Object.keys(_dashStatusFilters || {}).length > 0;
      /* allHives here is the ASSIGNED set — the caller scopes it, because the
         chips never filter unassigned placeholders. Say "assigned" so the count
         is not read as the whole fleet. */
      var total = (allHives || []).length;
      var summary = anyActive
        ? 'Showing ' + shownCount + ' of ' + total + ' assigned hives'
        : total + (total === 1 ? ' assigned hive' : ' assigned hives');
      var clearBtn = anyActive
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearStatusFilters()">Clear filters</button>'
        : '';
      bar.innerHTML = '<div class="filter-chips">' + chips + clearBtn + '</div>' +
        '<span class="filter-summary">' + summary + '</span>';
    }

    function sortDashHives(key) {
      if (_dashSortKey === key) { _dashSortAsc = !_dashSortAsc; } else { _dashSortKey = key; _dashSortAsc = true; }
      var sorted = _allDashHives.slice().sort(function(a, b) {
        var va = a[key] || '', vb = b[key] || '';
        if (typeof va === 'number' && typeof vb === 'number') return _dashSortAsc ? va - vb : vb - va;
        return _dashSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
      renderHives(sorted, true);
    }

    async function loadHives() {
      if (_hivesLoading) return;
      _hivesLoading = true;
      try {
        var resp = await fetch('/api/saas/my-hives');
        if (resp.status === 401) {
          window.location.href = '/login';
          return;
        }
        var data = await resp.json();
        _userQuota = data.saas_quota || 0;
        _userUsed = data.saas_used || 0;
        _allDashHives = data.hives || [];
        _hiveRegistry = data.hives || [];
        /* Alerts ride along on the same payload — see handleMyHives. Normalise
           to the empty summary so every consumer can iterate without guarding. */
        _fleetAlerts = data.alerts || EMPTY_ALERT_SUMMARY;
        _latestSHA = data.latest_sha || _latestSHA;
        if (data.latest_shas) _latestSHAs = data.latest_shas;
        if (data.tracked_branches) _trackedBranchesList = data.tracked_branches;
        if (data.latest_sha_messages) _latestSHAMessages = data.latest_sha_messages;
        if (data.latest_sha_image_status) _latestImageStatus = data.latest_sha_image_status;
        if (data.commit_messages) _commitMessages = data.commit_messages;
        if (data.hub_auto_upgrade !== undefined) _hubAutoUpgrade = data.hub_auto_upgrade;
        var shaEl = document.getElementById('latest-image-sha');
        if (shaEl) {
          var lines = '';
          var branches = Object.keys(_latestSHAs).sort();
          if (branches.length) {
            for (var bi = 0; bi < branches.length; bi++) {
              var br = branches[bi];
              var brMsg = _latestSHAMessages[br] || '';
              var brStatus = _latestImageStatus[br] || 'ready';
              var brStatusHTML = '';
              if (brStatus === 'building') {
                brStatusHTML = '<span style="display:inline-block;flex:none;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite" title="Container image for this commit is still building"></span><span style="font-size:0.65rem;color:var(--muted);opacity:0.7;white-space:nowrap">building image…</span>';
              } else if (brStatus === 'failed') {
                brStatusHTML = '<span style="color:var(--red);font-size:0.7rem;cursor:help" title="Image build failed for this commit — upgrades keep using the previous image">✗</span>';
              }
              lines += '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px"><span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(br) + '</span><span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHAs[br]) + '</span>' + (brMsg ? '<span style="font-size:0.7rem;color:var(--muted);opacity:0.7">: ' + esc(brMsg) + '</span>' : '') + brStatusHTML + '</div>';
            }
          } else if (_latestSHA) {
            lines = '<span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHA) + '</span>';
          }
          shaEl.innerHTML = lines ? '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:2px">Latest available images:</div>' + lines : '<div style="display:flex;align-items:center;gap:6px;font-size:0.7rem;color:var(--muted)"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite"></span>Resolving latest available images…</div>';
        }
        var hubHash = data.hub_git_hash || '';
        var hubBranch = data.hub_git_branch || 'v2';
        if (hubHash) {
          var el = document.getElementById('hub-version');
          if (el) {
            var hubBranchLatest = _latestSHAs[hubBranch] || _latestSHA;
            var hubLatestUnknown = !hubBranchLatest;
            var isCurrent = hubBranchLatest && sameShaJS(hubHash, hubBranchLatest);
            var hubUpgradeBtn = '';
            // Distinguish two states, matching the per-hive badges:
            //   - "queued"    = behind latest with auto-upgrade ON, but the
            //     rollout hasn't started yet (auto-upgrade will apply shortly).
            //   - "Upgrading" = a rollout is ACTUALLY in progress (auto OR admin).
            // The server now tells us which via hub_upgrade_state, so an AUTO
            // rollout in progress shows "Upgrading" too — previously it was
            // mislabeled "queued" because the frontend could only see the
            // admin-click flag (_hubUpgrading) and had no signal for an auto roll.
            var hubState = data.hub_upgrade_state || '';
            // Trust the server state; fall back to the optimistic admin-click flag
            // so the badge flips to "Upgrading" immediately on click, before the
            // next /api/saas/my-hives poll reports "upgrading".
            var hubIsUpgrading = hubState === 'upgrading' || _hubUpgrading;
            var hubQueued = !hubIsUpgrading && (hubState === 'queued' ||
              (hubState === '' && !isCurrent && hubBranchLatest && _hubAutoUpgrade));
            if (!isCurrent && hubBranchLatest && _isAdmin && !hubIsUpgrading && !hubQueued) {
              hubUpgradeBtn = ' <button id="hub-upgrade-btn" onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem;margin-left:6px;white-space:nowrap">Upgrade</button>';
            } else if (hubIsUpgrading) {
              hubUpgradeBtn = ' <span title="Upgrading to ' + esc(hubBranchLatest || '?') + '" style="display:inline-block;padding:2px 8px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;opacity:0.8"><span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Upgrading</span>';
            } else if (hubQueued) {
              hubUpgradeBtn = ' <span title="Auto-upgrade will apply ' + esc(hubBranchLatest || '?') + ' shortly' + (_isAdmin ? ' — click to upgrade now' : '') + '"' + (_isAdmin ? ' onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="cursor:pointer;' : ' style="') + 'display:inline-block;padding:2px 8px;background:var(--surface);color:var(--muted);border:1px dashed var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap">queued</span>';
            } else if (hubLatestUnknown && _isAdmin) {
              hubUpgradeBtn = ' <button disabled title="Waiting for latest version…" style="padding:2px 8px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</button>';
            }
            if (isCurrent) { _hubUpgrading = false; }
            var hubAutoCheck = '';
            if (_isAdmin) {
              hubAutoCheck = ' <label style="margin-left:6px;font-size:0.6rem;color:var(--muted);cursor:pointer;white-space:nowrap" title="Auto-upgrade hub when a new image is available"><input type="checkbox" ' + (_hubAutoUpgrade ? 'checked' : '') + ' onchange="toggleHubAutoUpgrade(this.checked)" style="vertical-align:middle;margin-right:2px;cursor:pointer">auto</label>';
            }
            var hubStatusIcon = hubLatestUnknown
              ? ' <span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-left:3px" title="Resolving latest version…"></span>'
              : isCurrent ? '<span style="color:var(--green);margin-left:3px" title="hub is on latest">✓</span>' : '<span style="color:var(--red);margin-left:3px" title="hub is behind latest ' + esc(hubBranchLatest) + '">↑</span>';
            var hubBranchPill = '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);margin-right:4px">' + esc(hubBranch) + '</span>';
            var hubMsg = _latestSHAMessages[hubBranch] || '';
            el.innerHTML = hubBranchPill + '<span style="font-family:monospace;font-size:0.7rem;color:var(--muted)" title="' + esc(hubMsg) + '">' + esc(hubHash) + '</span>' +
              hubStatusIcon + hubUpgradeBtn + hubAutoCheck;
          }
        }
        var canCreate = _userQuota < 0 || _userQuota > _userUsed;
        var addBtn = document.getElementById('btn-add-hive');
        if (addBtn) {
          addBtn.disabled = !canCreate;
          addBtn.title = canCreate ? '' : 'No hosted quota — contact hub admin';
        }
        renderHives(data.hives || []);
        renderPendingBanner(data.hives || []);
        renderUserAccessBanner();
        renderProvisionRequestBanner(data.my_provision_request || null);
        renderAdminProvisionRequests(data.provision_requests || []);
        renderRequestHiveButton(data);
        loadPublicHives(data.hives || []);
      } catch(e) {
        if (!_allDashHives.length) {
          document.getElementById('hives-container').innerHTML = '<div class="loading">Failed to load hives</div>';
        }
      } finally {
        _hivesLoading = false;
      }
    }

    async function loadPublicHives(myHives) {
      try {
        var resp = await fetch('/api/registry');
        var data = await resp.json();
        var allPublic = (data.hives || []).filter(function(h) { return h.isPublic !== false && h.hiveType === 'hosted'; });
        var myIds = {};
        (myHives || []).forEach(function(h) { myIds[h.id] = true; });
        var otherHives = allPublic.filter(function(h) { return !myIds[h.id]; });
        var section = document.getElementById('public-hives-section');
        if (!otherHives.length) { section.style.display = 'none'; return; }
        section.style.display = '';
        var statusResp = await fetch('/api/saas/access-status');
        var statusData = await statusResp.json();
        var accessMap = statusData.hives || {};
        var rows = otherHives.map(function(h) {
          var repoPath = h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || '';
          var repoLink = repoPath ? '<a href="https://github.com/' + esc(repoPath) + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '';
          var actionCell = '';
          var access = accessMap[h.id];
          if (access && access.status === 'accepted') {
            // Use the hive's heartbeat-reported dashboard URL (resolvedBase),
            // NOT a hardcoded <id>.hive.kubestellar.io host. Firewalled spokes
            // (e.g. vllm-d on *.apps.fmaas-vllm-d.fmaas.res.ibm.com) live on a
            // different domain, so the hardcoded host 503'd/failed to resolve.
            var cBase = resolvedBase(h);
            var actionCell2 = cBase
              ? '<a href="' + cBase + '/contribute" target="_blank" style="padding:3px 10px;background:rgba(34,197,94,0.15);color:#4ade80;border:1px solid rgba(34,197,94,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none">Contribute</a>'
              : '<span style="padding:3px 10px;color:var(--muted);font-size:0.7rem" title="hive has not reported its dashboard URL yet">Contribute unavailable</span>';
            actionCell = actionCell2;
          } else if (access && access.status === 'pending') {
            actionCell = '<span style="padding:3px 10px;background:rgba(245,158,11,0.15);color:#fbbf24;border:1px solid rgba(245,158,11,0.3);border-radius:4px;font-size:0.7rem">Pending</span>';
          } else {
            actionCell = '<button onclick="dashRequestAccess(\'' + esc(h.id) + '\',this)" style="padding:3px 10px;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);border-radius:4px;font-size:0.7rem;cursor:pointer;border:1px solid rgba(59,130,246,0.3)">Request Access</button>';
          }
          return '<tr>' +
            '<td style="text-align:left">' + esc(h.name || h.id) + '</td>' +
            '<td>' + repoLink + '</td>' +
            '<td>' + acmmBadge(h.acmmLevel) + '</td>' +
            '<td>' + actionCell + '</td>' +
            '</tr>';
        }).join('');
        document.getElementById('public-hives-container').innerHTML =
          '<table class="hive-table"><thead><tr><th style="text-align:left">Hive</th><th>Repo</th><th>ACMM</th><th></th></tr></thead><tbody>' + rows + '</tbody></table>';
      } catch(e) {}
    }

    var _requestAccessHiveId = null;
    var _requestAccessBtn = null;

    function dashRequestAccess(hiveId, btn) {
      // A justification note is required, so collect it via the modal
      // rather than firing the request immediately.
      _requestAccessHiveId = hiveId;
      _requestAccessBtn = btn || null;
      var label = document.getElementById('request-access-hive-label');
      if (label) label.textContent = hiveId;
      var ta = document.getElementById('request-access-note');
      if (ta) ta.value = '';
      var submit = document.getElementById('request-access-submit');
      if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
      document.getElementById('request-access-modal').style.display = 'flex';
      if (ta) ta.focus();
    }

    function closeRequestAccessModal() {
      document.getElementById('request-access-modal').style.display = 'none';
      _requestAccessHiveId = null;
      _requestAccessBtn = null;
    }

    async function submitRequestAccess() {
      var hiveId = _requestAccessHiveId;
      if (!hiveId) { closeRequestAccessModal(); return; }
      var ta = document.getElementById('request-access-note');
      var note = ta ? ta.value.trim() : '';
      if (!note) { hiveToast('Please explain why you need access', 'error'); if (ta) ta.focus(); return; }
      var submit = document.getElementById('request-access-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Sending...'; }
      var btn = _requestAccessBtn;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/request-access', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({note: note})
        });
        var data = await resp.json();
        if (!resp.ok) {
          hiveToast(data.error || 'Request failed', 'error');
          if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
          return;
        }
        if (btn) btn.outerHTML = '<span style="padding:3px 10px;background:rgba(245,158,11,0.15);color:#fbbf24;border:1px solid rgba(245,158,11,0.3);border-radius:4px;font-size:0.7rem">Pending</span>';
        hiveToast('Access request sent!', 'success');
        closeRequestAccessModal();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
      }
    }

    /* isPlaceholderHive identifies an UNASSIGNED pool slot: provStatus
       'available' is authoritative, with an 'available-*' org prefix as the
       fallback for placeholders that have not reported provStatus yet. Shared by
       the status-filter scoping and the Assigned/Unassigned section split so the
       two can never disagree about what counts as a placeholder. */
    function isPlaceholderHive(h) {
      if (!h) return false;
      return h.provStatus === 'available' || (!!h.org && h.org.indexOf('available-') === 0);
    }

    function renderHives(allHives, force) {
      allHives = allHives || [];
      /* The signature must include the active filters, otherwise toggling a
         chip while the hive data is unchanged would be treated as a no-op. */
      /* The alert state joins the signature for the same reason the status
         filters do: drilling into an alert type (or expanding the acknowledged
         list) changes what renders while the hive data is unchanged, and would
         otherwise be treated as a no-op. */
      var sig = JSON.stringify(allHives) + '|' + JSON.stringify(_dashStatusFilters) +
        '|' + _alertTypeFilter + '|' + _alertShowAcked + '|' + JSON.stringify(_fleetAlerts);
      if (!force && sig === _lastHivesJSON) return;
      _lastHivesJSON = sig;
      /* Status filters describe ASSIGNED hives only. An unassigned placeholder
         has no GitHub App, no tokens and no real health to speak of, so every
         chip would appear to "hide" the whole pool — and filtering to e.g.
         Degraded made the Unassigned section vanish, which reads as the
         placeholders having been deleted. Split first, filter only the assigned
         side, and leave the pool alone. */
      var assignedAll = [], unassignedAll = [];
      for (var _si = 0; _si < allHives.length; _si++) {
        (isPlaceholderHive(allHives[_si]) ? unassignedAll : assignedAll).push(allHives[_si]);
      }
      var hives = applyDashFilters(assignedAll).concat(unassignedAll);
      var filterBar = document.getElementById('hive-filter-bar');
      if (filterBar) filterBar.style.display = allHives.length ? '' : 'none';
      /* Counts are over the assigned set only, matching what the chips filter. */
      renderStatusFilterBar(assignedAll, hives.length - unassignedAll.length);
      /* Drawn BEFORE the empty-state early-returns below: a fleet whose every
         hive is filtered out still has alerts worth showing, and the panel is
         how the operator gets back out of a drill-down. */
      renderAlertsPanel();
      if (!allHives.length) {
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives yet</p>' +
          '<p>Log in to a local hive dashboard to see it here, or create a hosted hive.</p>' +
          '</div>';
        return;
      }
      if (!hives.length) {
        /* Hives exist, but every one was filtered out — say so, and offer the
           way back rather than looking like the list failed to load. Only
           assigned hives can be hidden, so report that count, not the total. */
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives match these filters</p>' +
          '<p>' + assignedAll.length + (assignedAll.length === 1 ? ' hive is' : ' hives are') + ' hidden by the filters above.</p>' +
          /* clearAllHiveFilters, not clearStatusFilters: an alert drill-down can
             empty the list too, and a button that only clears the chips would
             leave the operator stuck looking at an empty table. */
          '<p style="margin-top:12px"><button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear filters</button></p>' +
          '</div>';
        return;
      }
      var repoPath = function(h) { return h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || ''; };
      var buildRow = function(h, i) {
        var dot = h.online ? healthBadge(h) : '<span class="online-dot off"></span>';
        var rp = repoPath(h);
        var repoLink = rp ? '<a href="https://github.com/' + esc(rp) + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '';
        var repoCount = (h.repos || []).length;
        var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
        var isLocal = !isHosted;
        var canConvert = isLocal && h.role === 'owner' && (_userQuota < 0 || _userQuota > _userUsed);
        var modeCell = h.provStatus === 'error'
          ? '<span style="color:var(--red);cursor:help;white-space:nowrap" title="' + esc(h.provError || '') + '">⚠ ERROR</span>'
          : h.assigning
          ? '<span style="color:var(--accent);white-space:nowrap" title="Waiting for the spoke to report the new project via heartbeat"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Assigning to ' + esc(h.assigningTo || '?') + '</span>'
          : h.provStatus === 'provisioning'
          ? '<span style="color:var(--accent);white-space:nowrap">⏳ Provisioning</span>'
          : h.migrationStatus === 'migrating'
          ? '<span style="color:var(--accent);white-space:nowrap"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Migrating to ' + esc(h.migrationTo || '?') + '</span>'
          : h.migrationStatus === 'failed'
          ? '<span style="color:var(--red);cursor:help;white-space:nowrap" title="' + esc(h.provError || '') + '">⚠ Migration failed</span>'
          : modeBadge(h.governorMode);
        var rb = resolvedBase(h);
        var contributeUrl = rb ? rb + '/contribute' : '';
        var actions = '';
        if (canConvert) {
          actions = '<button onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="padding:3px 10px;background:var(--accent);color:#000;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap">Convert to Hosted</button>';
          if (h.role === 'owner') {
            actions += '<br style="margin-bottom:4px"><button onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="margin-top:6px;padding:3px 10px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;cursor:pointer;font-size:0.65rem;white-space:nowrap" title="Remove from registry (does not delete the hive)">Remove</button>';
          }
        } else if (isHosted && (h.role === 'owner' || h.role === 'read-write')) {
          actions = '<button onclick="openAccessModal(\'' + esc(h.id) + '\',\'' + esc(h.dashboardUrl || '') + '\')" style="padding:3px 10px;background:var(--blue);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap;margin-right:4px">Permissions</button>';
          if (h.role === 'owner') {
            actions += '<button onclick="deleteHive(\'' + esc(h.id) + '\')" style="padding:3px 10px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap">Delete</button>';
          }
        }
        var menuId = 'hive-menu-' + i;
        var dashUrl = dashboardLink(h);
        var snapUrl = snapshotLink(h);
        var apiUrl = apiLink(h);
        var menuItems = [];
        var mi = 'display:block;padding:7px 14px;color:#c9d1d9;text-decoration:none;font-size:0.78rem;cursor:pointer';
        if (_isAdmin && h.provStatus === 'available' && !h.assigning) menuItems.push('<div onclick="openAssignModal(\'' + esc(h.id) + '\')" style="' + mi + ';color:#3fb950;font-weight:600">Assign / Claim</div><div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (contributeUrl) menuItems.push('<a href="' + contributeUrl + '" target="_blank" style="' + mi + '">Contribute</a>');
        if (h.snapshotUrl) menuItems.push('<a href="' + esc(h.snapshotUrl) + '" target="_blank" style="' + mi + '">Preview</a>');
        var apiBase = rb ? esc(rb) : '';
        if (apiBase) menuItems.push('<a href="' + apiBase + '/api/docs" target="_blank" style="' + mi + '">API Docs</a>');
        if (menuItems.length > 0 && (canConvert || isHosted || isLocal)) menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (canConvert) menuItems.push('<div onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="' + mi + '">Convert to Hosted</div>');
        if (isHosted && (h.role === 'owner' || h.role === 'read-write')) menuItems.push('<div onclick="openAccessModal(\'' + esc(h.id) + '\',\'' + esc(h.dashboardUrl || '') + '\')" style="' + mi + '">Permissions</div>');
        if (h.role === 'owner' || h.role === 'read-write' || _isAdmin) menuItems.push('<div onclick="openOpenRouterFundModal(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">⚡ Fund with OpenRouter</div>');
        if (_isAdmin && isHosted) menuItems.push('<div onclick="openBannerForHive(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Send Banner</div>');
        if (isLocal && h.role === 'owner') menuItems.push('<div onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="' + mi + '">Remove</div>');
        if (isHosted && h.role === 'owner' && _clusterList && _clusterList.length > 1 && h.migrationStatus !== 'migrating') menuItems.push('<div onclick="openMigrateModal(\'' + esc(h.id) + '\',\'' + esc(h.clusterId || '') + '\')" style="' + mi + '">Move to cluster</div>');
        if (isHosted && h.role === 'owner') menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div><div onclick="deleteHive(\'' + esc(h.id) + '\')" style="' + mi + ';color:#f85149">Delete</div>');
        var sha = h.gitHash || '';
        var versionCell = '';
        if (sha) {
          var branchName = h.gitBranch || 'v2';
          var branchLatest = _latestSHAs[branchName] || _latestSHA;
          var _trackedBranches = _trackedBranchesList.length > 0 ? _trackedBranchesList : Object.keys(_latestSHAs);
          if (_trackedBranches.length === 0) _trackedBranches = ['v2'];
          var canSwitchBranch = isHosted && h.role === 'owner' && _trackedBranches.length > 1 && !h.upgrading;
          var branchOptions = '';
          if (canSwitchBranch) {
            for (var bi = 0; bi < _trackedBranches.length; bi++) {
              var tb = _trackedBranches[bi];
              if (tb !== branchName) {
                branchOptions += '<div onclick="event.stopPropagation();switchBranch(\'' + esc(h.id) + '\',\'' + esc(tb) + '\',this)" style="padding:4px 10px;cursor:pointer;font-size:0.65rem;white-space:nowrap;color:#c9d1d9;border-radius:4px" onmouseover="this.style.background=\'rgba(59,130,246,0.2)\'" onmouseout="this.style.background=\'transparent\'">' + esc(tb) + '</div>';
              }
            }
          }
          var branch = canSwitchBranch
            ? '<span id="branch-pill-' + esc(h.id) + '" style="display:inline-block;position:relative;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);margin-right:4px;cursor:pointer" onclick="toggleBranchMenu(\'' + esc(h.id) + '\')" title="Click to switch branch">' + esc(branchName) + ' ▾<div id="branch-menu-' + esc(h.id) + '" style="display:none;position:absolute;top:100%;left:0;margin-top:4px;background:#1c2128;border:1px solid #30363d;border-radius:6px;padding:4px 0;z-index:1000;min-width:60px;box-shadow:0 4px 12px rgba(0,0,0,0.4)">' + branchOptions + '</div></span>'
            : '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);margin-right:4px">' + esc(branchName) + '</span>';
          var latestUnknown = !branchLatest;
          var isCurrent = branchLatest && sameShaJS(sha, branchLatest);
          /* Branch switch in flight: the hive still reports the OLD branch
             (often current on it) until the new pod heartbeats — without
             this, isCurrent suppresses every progress indicator. */
          /* Switch-vs-upgrade is decided in ONE place (hiveUpgradeState) so the
             spinner, label and title always agree. Only a target on a DIFFERENT
             branch is a switch; a plain-SHA (auto-upgrade) target always reads
             "Upgrading", even if a stale switch sentinel lingers. */
          var upgradeState = hiveUpgradeState(h, branchName);
          var isSwitching = upgradeState.isSwitching;
          var targetBranch = upgradeState.targetBranch;
          /* Drop a stale switch sentinel (resolved to a same-branch SHA) so it
             stops forcing the upgrading state on later auto-upgrades. */
          if (upgradeState.switchSentinelStale) delete _upgradingHives[h.id];
          var sentinel = _upgradingHives[h.id];
          var isUpgrading = isSwitching ||
            (sentinel && sha === sentinel) || (h.upgrading && !isCurrent && !latestUnknown);
          if (sentinel && sha !== sentinel && !isSwitching) { delete _upgradingHives[h.id]; delete _switchStartedAt[h.id]; }
          if (isCurrent && h.upgrading && !isSwitching) { h.upgrading = false; }
          var imageBuilding = (_latestImageStatus[branchName] || '') === 'building';
          var buildingHint = imageBuilding ? ' (image still building — upgrading now pulls the previous image)' : '';
          var status = latestUnknown
            ? ' <span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-left:3px" title="Resolving latest version…"></span>'
            : isCurrent ? '<span style="color:var(--green);margin-left:3px" title="latest">✓</span>' : '<span style="color:var(--red);margin-left:3px" title="behind latest ' + esc(branchLatest) + '">↑</span>';
          var upgradeIcon = '';
          if (isUpgrading) {
            var switchStale = isSwitching && _switchStartedAt[h.id] && (Date.now() - _switchStartedAt[h.id] > SWITCH_STALE_MS);
            var progressLabel = isSwitching ? (switchStale ? 'Switching to ' + esc(targetBranch) + ' — taking longer than expected' : 'Switching to ' + esc(targetBranch)) : 'Upgrading';
            var progressTitle = isSwitching ? (switchStale ? 'The hive has not reported branch ' + esc(targetBranch) + ' yet — it may be offline or its build predates in-cluster switch support. It will apply on its next successful check-in.' : 'Rolling out ' + esc(h.upgradeTarget || '') + ' — the pill updates when the hive reports the new branch') : 'Upgrading to ' + esc(branchLatest || h.upgradeTarget || '?');
            upgradeIcon = ' <span title="' + progressTitle + '" style="display:inline-block;padding:3px 10px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-size:0.7rem;margin-left:6px;white-space:nowrap;opacity:0.8"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>' + progressLabel + '</span>';
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner' && h.autoUpgrade) {
            upgradeIcon = ' <span id="upgrade-' + esc(h.id) + '" onclick="upgradeHive(\'' + esc(h.id) + '\',\'' + esc(sha) + '\',\'' + esc(branchName) + '\')" title="Auto-upgrade will apply ' + esc(branchLatest) + ' shortly — click to upgrade now' + esc(buildingHint) + '" style="display:inline-block;padding:3px 10px;background:var(--surface);color:var(--muted);border:1px dashed var(--border);border-radius:4px;cursor:pointer;font-size:0.7rem;margin-left:6px;white-space:nowrap">queued</span>';
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner') {
            upgradeIcon = ' <button id="upgrade-' + esc(h.id) + '" onclick="upgradeHive(\'' + esc(h.id) + '\',\'' + esc(sha) + '\',\'' + esc(branchName) + '\')" title="Current: ' + esc(sha) + ' → Latest: ' + esc(branchLatest) + esc(buildingHint) + '" style="padding:3px 10px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;margin-left:6px;white-space:nowrap">Upgrade</button>';
          } else if (latestUnknown && isHosted && h.role === 'owner') {
            upgradeIcon = ' <button disabled title="Waiting for latest version…" style="padding:3px 10px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;font-size:0.7rem;margin-left:6px;white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</button>';
          }
          var autoUpgradeCheck = '';
          if (isHosted && h.role === 'owner') {
            autoUpgradeCheck = ' <label style="margin-left:8px;font-size:0.65rem;color:var(--muted);cursor:pointer;white-space:nowrap" title="Automatically upgrade when a new version is available"><input type="checkbox" ' + (h.autoUpgrade ? 'checked' : '') + ' onchange="toggleAutoUpgrade(\'' + esc(h.id) + '\',this.checked)" style="vertical-align:middle;margin-right:2px;cursor:pointer">auto</label>';
          }
          var shaMsg = _commitMessages[sha] || _latestSHAMessages[branchName] || '';
          versionCell = branch + '<span style="font-family:monospace;color:var(--muted)" title="' + esc(shaMsg) + '">' + esc(sha) + '</span>' + status + upgradeIcon + autoUpgradeCheck;
        } else { versionCell = '<span style="color:var(--muted)">—</span>'; }
        var pendingBadge = (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write'))
          ? '<span style="position:absolute;top:-2px;right:-2px;background:var(--blue);color:#fff;border-radius:50%;width:16px;height:16px;font-size:0.6rem;display:flex;align-items:center;justify-content:center;font-weight:700">' + h.pendingRequestCount + '</span>'
          : '';
        var pendingPill = '';
        if (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write')) {
          pendingPill = '<a href="#" onclick="togglePendingRow(\'' + esc(h.id) + '\');return false" style="display:inline-flex;align-items:center;gap:4px;padding:3px 10px;background:rgba(59,130,246,0.12);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;cursor:pointer;white-space:nowrap">&#x1F514; ' + h.pendingRequestCount + ' pending</a>';
        }
        // 14 = the 13 original columns plus Uptime.
        var TOTAL_COLUMNS = 14;
        var pendingExpandRow = '';
        if (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write') && (h.pending_requests || []).length > 0) {
          var prItems = (h.pending_requests || []).map(function(pr) {
            var avatar = '<img src="https://github.com/' + esc(pr.username) + '.png" style="width:20px;height:20px;border-radius:50%;vertical-align:middle;margin-right:6px">';
            var note = (pr.note || '').trim();
            var noteHtml = note
              ? '<div style="margin-top:4px;font-size:0.75rem;color:var(--text);white-space:pre-wrap;word-break:break-word;background:rgba(0,0,0,0.15);border-left:2px solid var(--accent);padding:4px 8px;border-radius:2px">' + esc(note) + '</div>'
              : '<div style="margin-top:4px;font-size:0.72rem;color:var(--muted);font-style:italic">(no note)</div>';
            return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
              '<div style="display:flex;align-items:center;justify-content:space-between">' +
              '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(pr.username) + '</span></div>' +
              '<div style="display:flex;gap:4px">' +
              '<button onclick="inlineApproveAccess(\'' + esc(h.id) + '\',\'' + esc(pr.username) + '\',this)" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Approve</button>' +
              '<button onclick="inlineDenyAccess(\'' + esc(h.id) + '\',\'' + esc(pr.username) + '\',this)" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Deny</button>' +
              '</div></div>' + noteHtml + '</div>';
          }).join('');
          pendingExpandRow = '<tr id="pending-row-' + esc(h.id) + '" style="display:none"><td colspan="' + TOTAL_COLUMNS + '"><div style="padding:8px 16px;background:rgba(59,130,246,0.05);border-radius:6px;margin:4px 0">' + prItems + '</div></td></tr>';
        }
        return '<tr>' +
          '<td class="hive-menu-cell" style="position:relative;width:30px;text-align:center;overflow:visible">' + (h.migrationStatus === 'migrating' ? '<span style="font-size:1.1rem;color:var(--border);user-select:none;cursor:not-allowed" title="Disabled during migration">⋮</span>' : '<span style="cursor:pointer;font-size:1.1rem;color:var(--muted);user-select:none">⋮</span>' + pendingBadge + '<div class="hive-menu-dropdown" style="display:none;position:absolute;left:0;bottom:auto;background:#1c2128;border:1px solid #30363d;border-radius:8px;min-width:160px;padding:4px 0;z-index:1000;box-shadow:0 8px 24px rgba(0,0,0,0.5)">' + menuItems.join('') + '</div>') + '</td>' +
          '<td style="text-align:left;line-height:1.4">' + (function() { var isHostedRow = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-'))); var dh = isHostedRow && h.id ? ('/api/saas/hives/' + encodeURIComponent(h.id) + '/open') : (rb ? esc(rb) : ''); var displayName = h.name || h.id; var parts = displayName.split('/'); var orgName = parts.length > 1 ? parts[0] : ''; var repoName = parts.length > 1 ? parts.slice(1).join('/') : displayName; var rp = h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : ''; var ghIcon = rp ? '<a href="https://github.com/' + esc(rp) + '" target="_blank" style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></a>' : ''; var link = function(text, bold) { if (dh) { return '<a href="' + dh + '" target="_blank" class="' + (bold ? 'hive-name-link' : 'hive-sub-link') + '" title="Open dashboard">' + esc(text) + '</a>'; } var s = bold ? 'font-weight:700;color:inherit' : 'color:#6b7280;font-weight:400'; return '<span style="' + s + '">' + esc(text) + '</span>'; }; var line1 = dot + ' ' + link(orgName || repoName, true); var line2 = orgName ? '<div style="padding-left:18px;font-size:0.8rem">' + link(repoName, false) + ' ' + ghIcon + ' ' + roleBadge(h.role) + '</div>' : '<div style="padding-left:18px">' + ghIcon + ' ' + roleBadge(h.role) + '</div>'; var line3 = pendingPill ? '<div style="margin-top:4px;padding-left:18px">' + pendingPill + '</div>' : ''; return line1 + line2 + line3; })() + '</td>' +
          '<td>' + (isLocal ? '<span style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(107,114,128,0.15);color:#9ca3af;border:1px solid rgba(107,114,128,0.3)">local</span>' : clusterBadge(h.clusterId, h.clusterName)) + '</td>' +
          '<td style="white-space:nowrap">' + uptimeCell(h) + '</td>' +
          '<td>' + (function() { var pub = !!h.isPublic; var tid = 'vis-' + esc(h.id); if (isHosted && h.role === 'owner') { return '<label style="position:relative;display:inline-block;width:36px;height:20px;cursor:pointer"><input type="checkbox" id="' + tid + '" ' + (pub ? 'checked' : '') + ' onchange="toggleVisibility(\'' + esc(h.id) + '\',this.checked)" style="opacity:0;width:0;height:0"><span style="position:absolute;inset:0;background:' + (pub ? 'var(--green)' : 'var(--border)') + ';border-radius:10px;transition:background 0.2s"></span><span style="position:absolute;top:2px;left:' + (pub ? '18px' : '2px') + ';width:16px;height:16px;background:#fff;border-radius:50%;transition:left 0.2s"></span></label>'; } if (isLocal) { var dh = h.dashboardUrl && !h.dashboardUrl.includes('localhost') ? h.dashboardUrl : ''; var badge = pub ? '<span style="color:var(--green)">Public</span>' : '<span style="color:var(--muted)">Private</span>'; return dh ? '<a href="' + esc(dh) + '#config/governor/Hub" target="_blank" title="Change in Governor Config → Hub tab" style="text-decoration:none;cursor:pointer">' + badge + ' <span style="font-size:0.6rem;color:var(--muted)">↗</span></a>' : badge; } return pub ? '<span style="color:var(--green)">✓</span>' : '<span style="color:var(--muted)">—</span>'; })() + '</td>' +
          '<td style="font-size:0.7rem;white-space:nowrap">' + versionCell + '</td>' +
          '<td title="' + esc((h.repos || []).join('\n')) + '" style="cursor:' + (repoCount > 0 ? 'help' : 'default') + '">' + repoCount + '</td>' +
          '<td>' + acmmBadge(h.acmmLevel) + '</td>' +
          '<td title="' + esc((h.agents || []).map(function(a){ var label = a.name + ' (' + a.state + ')'; if (a.mode === 'on_demand') label += ' — on demand'; return label; }).join('\n')) + '" style="cursor:' + ((h.agentCount || 0) > 0 ? 'help' : 'default') + '">' + (h.agentCount || 0) + '</td>' +
          '<td title="Cumulative tokens consumed, as of the last heartbeat" style="white-space:nowrap;cursor:help">' + fmtTokens(h.totalTokens24h || 0) + '</td>' +
          '<td>' + modeCell + '</td>' +
          '<td>' + sparkline(h.issueHistory, '#f59e0b', 50, 14) + (h.actionableIssues || 0) + '</td>' +
          '<td>' + sparkline(h.prHistory, '#3b82f6', 50, 14) + (h.actionablePRs || 0) + '</td>' +
          '<td>' + (h.activeContributors || 0) + '</td>' +
          '</tr>' + pendingExpandRow;
      };
      /* Section-header row: a labeled separator spanning all columns, styled to
         match the table's muted uppercase heading treatment (see .hive-table th). */
      var TOTAL_COLUMNS_HEADER = 14;
      var sectionHeader = function(label, count) {
        return '<tr class="hive-section-head"><td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:14px 12px 6px;color:var(--muted);font-weight:600;font-size:0.75rem;' +
          'text-transform:uppercase;letter-spacing:0.5px;text-align:left">' +
          esc(label) + ' (' + count + ')</td></tr>';
      };
      var rows;
      if (_isAdmin) {
        /* Admin-only organizational aid: split into assigned (real, claimed)
           hives and unassigned placeholders. A placeholder is signalled by
           provStatus === 'available' (primary), with an org 'available-*'
           prefix as a fallback for placeholders that have not yet reported
           provStatus. Preserve incoming order so each section stays sorted. */
        var assigned = [], unassigned = [];
        for (var _hi = 0; _hi < hives.length; _hi++) {
          var _h = hives[_hi];
          if (isPlaceholderHive(_h)) unassigned.push(_h); else assigned.push(_h);
        }
        /* Global running index across BOTH groups so menu ids (hive-menu-<i>)
           never collide between sections and the ⋮ dropdowns keep working. */
        var _idx = 0;
        rows = '';
        if (assigned.length > 0) {
          rows += sectionHeader('Assigned hives', assigned.length);
          for (var _ai = 0; _ai < assigned.length; _ai++) { rows += buildRow(assigned[_ai], _idx++); }
        }
        if (unassigned.length > 0) {
          rows += sectionHeader('Unassigned hives', unassigned.length);
          for (var _ui = 0; _ui < unassigned.length; _ui++) { rows += buildRow(unassigned[_ui], _idx++); }
        }
      } else {
        /* Non-admin: single flat list, exactly as before. */
        rows = hives.map(buildRow).join('');
      }
      document.getElementById('hives-container').innerHTML =
        '<div class="table-wrap"><table class="hive-table"><thead><tr>' +
        '<th></th><th onclick="sortDashHives(\'name\')" style="cursor:pointer">Hive ⇅</th><th onclick="sortDashHives(\'clusterId\')" style="cursor:pointer">Location ⇅</th><th onclick="sortDashHives(\'startedAt\')" style="cursor:pointer" title="Process uptime since the last restart — a short value that keeps resetting means the pod is restarting">Uptime ⇅</th><th>Public</th><th>Version</th><th>Repos</th><th onclick="sortDashHives(\'acmmLevel\')" style="cursor:pointer">ACMM ⇅</th><th onclick="sortDashHives(\'agentCount\')" style="cursor:pointer">Agents ⇅</th><th onclick="sortDashHives(\'totalTokens24h\')" style="cursor:pointer" title="Cumulative tokens consumed, as of the last heartbeat">Tokens ⇅</th><th onclick="sortDashHives(\'governorMode\')" style="cursor:pointer">Mode ⇅</th><th onclick="sortDashHives(\'actionableIssues\')" style="cursor:pointer">Issues ⇅</th><th onclick="sortDashHives(\'actionablePRs\')" style="cursor:pointer">PRs ⇅</th><th onclick="sortDashHives(\'activeContributors\')" style="cursor:pointer">Contributors ⇅</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table></div>';
      setTimeout(function() {
        var tw = document.querySelector('.table-wrap');
        if (tw && tw.scrollWidth > tw.clientWidth) tw.classList.add('has-scroll');
      }, 0);
    }

    async function toggleVisibility(id, isPublic) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/visibility', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({is_public: isPublic})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to change visibility', 'error'); loadHives(); return; }
        hiveToast(id + ' is now ' + (isPublic ? 'public' : 'private'), 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    async function toggleAutoUpgrade(id, enabled) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed', 'error'); loadHives(); return; }
        hiveToast(id + ' auto-upgrade ' + (enabled ? 'enabled' : 'disabled'), 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    var _hubUpgrading = false;
    var _hubAutoUpgrade = false;
    async function toggleHubAutoUpgrade(enabled) {
      try {
        var resp = await fetch('/api/saas/hub/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed', 'error'); return; }
        _hubAutoUpgrade = enabled;
        hiveToast('Hub auto-upgrade ' + (enabled ? 'enabled' : 'disabled'), 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
    async function upgradeHub(currentSHA) {
      var toSHA = _latestSHA ? _latestSHA.substring(0, 7) : 'latest';
      var fromSHA = currentSHA ? currentSHA.substring(0, 7) : '?';
      if (!await hiveConfirm('Upgrade Hive Hub?<br><br><span style="font-family:monospace;font-size:0.85rem;color:var(--muted)">' + fromSHA + '</span> → <span style="font-family:monospace;font-size:0.85rem;color:var(--green)">' + toSHA + '</span>', true)) return;
      var btn = document.getElementById('hub-upgrade-btn');
      if (btn) { btn.disabled = true; btn.innerHTML = '<span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Upgrading'; btn.style.opacity = '0.6'; }
      try {
        var resp = await fetch('/api/saas/hub/upgrade', {method: 'POST'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Hub upgrade failed', 'error'); return; }
        _hubUpgrading = true;
        hiveToast('Hub upgrade started — page will refresh when ready', 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    var _upgradingHives = {};
    var _switchStartedAt = {}; // hiveId → ms timestamp the switch was initiated
    var SWITCH_STALE_MS = 8 * 60 * 1000; // warn if a switch hasn't landed in 8 min

    /* Prefix marking a client-side branch-switch sentinel in _upgradingHives.
       The intended target branch follows the prefix (e.g. "switch:v3") so the
       label can tell a genuine branch switch apart from a same-branch upgrade
       purely from state — a bare sentinel could not. */
    var SWITCH_SENTINEL_PREFIX = 'switch:';
    /* Suffix the hub appends to a branch-switch upgrade target. */
    var BRANCH_TARGET_SUFFIX = '-latest';

    /* Single source of truth for switch-vs-upgrade, consumed by the spinner,
       the label and the title so they can never disagree. Resolves the target
       branch from the server's "<branch>-latest" upgradeTarget or, before the
       server reflects it, from the optimistic client sentinel ("switch:<branch>").
       A genuine branch switch means a target branch that is present AND differs
       from the branch the hive currently reports. A plain-SHA target — an
       auto-upgrade — yields no target branch and is therefore an upgrade, never
       a switch, regardless of any sticky sentinel. */
    function hiveUpgradeState(h, branchName) {
      var sentinel = _upgradingHives[h.id];
      var hasSwitchSentinel = typeof sentinel === 'string'
        && sentinel.indexOf(SWITCH_SENTINEL_PREFIX) === 0;
      var upgradeTarget = h.upgradeTarget || '';
      var isBranchTarget = upgradeTarget.length > BRANCH_TARGET_SUFFIX.length
        && upgradeTarget.slice(-BRANCH_TARGET_SUFFIX.length) === BRANCH_TARGET_SUFFIX;
      var targetBranch = '';
      if (isBranchTarget) {
        targetBranch = upgradeTarget.slice(0, -BRANCH_TARGET_SUFFIX.length);
      } else if (hasSwitchSentinel) {
        targetBranch = sentinel.slice(SWITCH_SENTINEL_PREFIX.length);
      }
      /* A same-branch target is not a switch — drop it so nothing downstream
         mistakes an auto-upgrade for one. */
      var isSwitching = !!(targetBranch && targetBranch !== branchName);
      if (!isSwitching) targetBranch = '';
      /* A switch sentinel that no longer resolves to a switch (target became a
         same-branch SHA / auto-upgrade) is stale and must not force upgrading. */
      var switchSentinelStale = hasSwitchSentinel && !isSwitching;
      return { isSwitching: isSwitching, targetBranch: targetBranch, switchSentinelStale: switchSentinelStale };
    }

    async function upgradeHive(id, currentSHA, branch) {
      var fromSHA = currentSHA ? currentSHA.substring(0, 7) : '?';
      var branchLatest = (branch && _latestSHAs[branch]) || _latestSHA;
      var toSHA = branchLatest ? branchLatest.substring(0, 7) : 'latest';
      if (!await hiveConfirm('Upgrade ' + id + '?<br><br><span style="font-family:monospace;font-size:0.85rem;color:var(--muted)">' + fromSHA + '</span> → <span style="font-family:monospace;font-size:0.85rem;color:var(--green)">' + toSHA + '</span>', true)) return;
      var btn = document.getElementById('upgrade-' + id);
      if (btn) { btn.disabled = true; btn.innerHTML = '<span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Upgrading'; btn.style.opacity = '0.6'; }
      try {
        hiveToast('Upgrading ' + id + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/upgrade', {method: 'POST'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Upgrade failed', 'error'); delete _upgradingHives[id]; loadHives(); return; }
        _upgradingHives[id] = currentSHA;
        hiveToast('Upgrade started for ' + id + ' — waiting for rollout', 'success');
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
        setTimeout(loadHives, 90000);
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); delete _upgradingHives[id]; loadHives(); }
    }

    function toggleBranchMenu(hiveId) {
      var menu = document.getElementById('branch-menu-' + hiveId);
      if (!menu) return;
      var isOpen = menu.style.display !== 'none';
      document.querySelectorAll('[id^="branch-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      if (!isOpen) {
        menu.style.display = 'block';
        var closeHandler = function(e) {
          if (!e.target.closest('#branch-pill-' + hiveId)) {
            menu.style.display = 'none';
            document.removeEventListener('click', closeHandler);
          }
        };
        setTimeout(function() { document.addEventListener('click', closeHandler); }, 0);
      }
    }

    var _switchTimers = {};
    function switchBranch(hiveId, newBranch, el) {
      if (el) el.closest('[id^="branch-menu-"]').style.display = 'none';
      if (_switchTimers[hiveId]) {
        clearInterval(_switchTimers[hiveId].interval);
        delete _switchTimers[hiveId];
      }
      var SWITCH_DELAY_SEC = 5;
      var remaining = SWITCH_DELAY_SEC;
      var pill = document.getElementById('branch-pill-' + hiveId);
      if (!pill) return;
      var origHTML = pill.innerHTML;
      pill.style.background = 'rgba(234,179,8,0.2)';
      pill.style.borderColor = 'rgba(234,179,8,0.5)';
      pill.style.color = '#eab308';
      pill.onclick = null;
      // Install the cancel handler after the selecting click finishes
      // bubbling — the menu lives inside the pill, so the same click
      // would otherwise cancel the switch it just started.
      setTimeout(function() {
        pill.onclick = function() { cancelBranchSwitch(hiveId, origHTML); };
      }, 0);
      pill.innerHTML = esc(newBranch) + ' in ' + remaining + 's ✕';
      pill.title = 'Click to cancel';
      var interval = setInterval(function() {
        remaining--;
        if (remaining <= 0) {
          clearInterval(interval);
          delete _switchTimers[hiveId];
          pill.innerHTML = '<span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Switching to ' + esc(newBranch);
          pill.style.cursor = 'default';
          pill.onclick = null;
          // Set the switching sentinel BEFORE the request so any loadHives()
          // re-render during the POST round-trip keeps showing "Switching to
          // <branch>" — previously the sentinel was set only after the fetch
          // resolved, leaving a gap where the pill reverted to plain status.
          _upgradingHives[hiveId] = SWITCH_SENTINEL_PREFIX + newBranch;
          _switchStartedAt[hiveId] = Date.now();
          doSwitchBranch(hiveId, newBranch);
          return;
        }
        pill.innerHTML = esc(newBranch) + ' in ' + remaining + 's ✕';
      }, 1000);
      _switchTimers[hiveId] = { interval: interval, origHTML: origHTML };
    }

    function cancelBranchSwitch(hiveId, origHTML) {
      if (_switchTimers[hiveId]) {
        clearInterval(_switchTimers[hiveId].interval);
        delete _switchTimers[hiveId];
      }
      var pill = document.getElementById('branch-pill-' + hiveId);
      if (!pill) return;
      pill.style.background = 'rgba(59,130,246,0.15)';
      pill.style.borderColor = 'rgba(59,130,246,0.3)';
      pill.style.color = '#60a5fa';
      pill.innerHTML = origHTML;
      pill.onclick = function() { toggleBranchMenu(hiveId); };
      pill.title = 'Click to switch branch';
      hiveToast('Branch switch cancelled', 'info');
    }

    async function doSwitchBranch(hiveId, newBranch) {
      try {
        hiveToast('Switching ' + hiveId + ' to ' + newBranch + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/switch-branch', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ branch: newBranch })
        });
        var data = await resp.json();
        if (!resp.ok) {
          delete _upgradingHives[hiveId]; delete _switchStartedAt[hiveId]; // clear switching state on failure
          hiveToast(data.error || 'Branch switch failed', 'error');
          loadHives();
          return;
        }
        // Sentinel already set before the fetch; keep it until the hive
        // reports the new branch (cleared in the render path).
        hiveToast('Switching ' + hiveId + ' to ' + newBranch + ' — applies on next check-in', 'success');
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
      } catch(e) {
        delete _upgradingHives[hiveId]; delete _switchStartedAt[hiveId]; // clear switching state on error
        hiveToast('Error: ' + e.message, 'error');
        loadHives();
      }
    }

    function autoRequestAccessFromUrl() {
      var params = new URLSearchParams(window.location.search);
      var hiveId = params.get('request_hive');
      if (!hiveId) return;
      window.history.replaceState({}, '', '/dashboard');
      // A justification note is required, so open the request modal to
      // collect it instead of firing a note-less request.
      dashRequestAccess(hiveId, null);
    }

    function togglePendingRow(hiveId) {
      var row = document.getElementById('pending-row-' + hiveId);
      if (row) row.style.display = row.style.display === 'none' ? '' : 'none';
    }

    async function inlineApproveAccess(hiveId, username, btn) {
      btn.disabled = true;
      btn.textContent = '...';
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/approve-access/' + encodeURIComponent(username), {method: 'PUT'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Approve failed', 'error'); btn.disabled = false; btn.textContent = 'Approve'; return; }
        hiveToast(username + ' approved', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Approve'; }
    }

    async function inlineDenyAccess(hiveId, username, btn) {
      btn.disabled = true;
      btn.textContent = '...';
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/deny-access/' + encodeURIComponent(username), {method: 'DELETE'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Deny failed', 'error'); btn.disabled = false; btn.textContent = 'Deny'; return; }
        hiveToast(username + ' denied', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Deny'; }
    }

    function renderPendingBanner(hives) {
      var existing = document.getElementById('pending-banner');
      if (existing) existing.remove();
      var pending = (hives || []).filter(function(h) { return (h.role === 'owner' || h.role === 'read-write') && h.pendingRequestCount > 0; });
      if (!pending.length) return;
      var total = pending.reduce(function(sum, h) { return sum + h.pendingRequestCount; }, 0);
      var banner = document.createElement('div');
      banner.id = 'pending-banner';
      banner.style.cssText = 'background:rgba(59,130,246,0.12);border:1px solid rgba(59,130,246,0.3);border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px';
      banner.innerHTML = '<span style="font-size:1.1rem">📬</span><span style="font-size:0.85rem;color:var(--text)">' + total + ' pending access request' + (total > 1 ? 's' : '') + ' across ' + pending.length + ' hive' + (pending.length > 1 ? 's' : '') + '. Open <strong>Permissions</strong> on each hive to approve or deny.</span>';
      var container = document.getElementById('hives-container');
      container.parentNode.insertBefore(banner, container);
    }

    async function renderUserAccessBanner() {
      var existing = document.getElementById('user-access-banner');
      if (existing) existing.remove();
      try {
        var resp = await fetch('/api/saas/access-status');
        var data = await resp.json();
        var hives = data.hives || {};
        var pendingIds = [];
        var acceptedIds = [];
        for (var hid in hives) {
          var info = hives[hid];
          if (info.status === 'pending') pendingIds.push(hid);
          if (info.status === 'accepted' && info.role !== 'owner') acceptedIds.push(hid);
        }
        if (!pendingIds.length && !acceptedIds.length) return;
        var container = document.getElementById('hives-container');
        var banner = document.createElement('div');
        banner.id = 'user-access-banner';
        banner.style.cssText = 'margin-bottom:16px';
        var html = '';
        var dismissed = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}');
        if (pendingIds.length) {
          var pKey = 'pending:' + pendingIds.sort().join(',');
          if (!dismissed[pKey]) {
            html += '<div style="background:rgba(245,158,11,0.12);border:1px solid rgba(245,158,11,0.3);border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;gap:10px">' +
              '<span style="font-size:1.1rem">&#x1F514;</span><span style="flex:1;font-size:0.85rem;color:var(--text)">Access pending: <strong>' + pendingIds.map(esc).join(', ') + '</strong></span>' +
              '<button onclick="dismissBanner(\'' + pKey.replace(/'/g,'') + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button></div>';
          }
        }
        if (acceptedIds.length) {
          var aKey = 'accepted:' + acceptedIds.sort().join(',');
          if (!dismissed[aKey]) {
            html += '<div style="background:rgba(34,197,94,0.12);border:1px solid rgba(34,197,94,0.3);border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;gap:10px">' +
              '<span style="font-size:1.1rem">&#x2705;</span><span style="flex:1;font-size:0.85rem;color:var(--text)">Access granted: <strong>' + acceptedIds.map(esc).join(', ') + '</strong> — Start contributing!</span>' +
              '<button onclick="dismissBanner(\'' + aKey.replace(/'/g,'') + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button></div>';
          }
        }
        banner.innerHTML = html;
        container.parentNode.insertBefore(banner, container);
      } catch(e) {}
    }

    function renderRequestHiveButton(data) {
      var btn = document.getElementById('btn-request-hive');
      if (!btn) return;
      btn.style.display = '';
    }

    function renderProvisionRequestBanner(req) {
      var el = document.getElementById('provision-request-banner');
      if (!el) return;
      if (!req) { el.style.display = 'none'; return; }
      el.style.display = '';
      var project = esc(req.org) + '/' + esc(req.primary_repo || req.repos);
      var status = req.status || 'pending';
      var icon, bg, border, msg;
      if (status === 'approved') {
        icon = '&#x2705;';
        bg = 'rgba(34,197,94,0.12)'; border = 'rgba(34,197,94,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> has been approved! Click <strong>Provision</strong> to set up your hive.' +
          ' <button onclick="openProvisionDialog(\'' + esc(req.org) + '\',\'' + esc(req.repos) + '\',\'' + esc(req.primary_repo || '') + '\',' + (req.acmm_level || 1) + ')" style="margin-left:8px;padding:4px 12px;background:#238636;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.8rem;font-weight:600">Provision</button>';
      } else if (status === 'denied') {
        icon = '&#x274C;';
        bg = 'rgba(239,68,68,0.12)'; border = 'rgba(239,68,68,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> was denied by an admin.';
      } else {
        icon = '&#x1F3D7;&#xFE0F;';
        bg = 'rgba(245,158,11,0.12)'; border = 'rgba(245,158,11,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> is pending admin approval.';
      }
      el.innerHTML = '<div style="background:' + bg + ';border:1px solid ' + border + ';border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px">' +
        '<span style="font-size:1.1rem">' + icon + '</span>' +
        '<span style="flex:1;font-size:0.85rem;color:var(--text)">' + msg + '</span>' +
        '</div>';
    }

    // renderRequestHistory renders every already-decided request as a table:
    // who asked, for what, what was decided, by whom, and — for an approval —
    // which hive they actually got. Pending requests stay as action cards above;
    // this is the audit trail, not a work queue, so it is deliberately a dense
    // table rather than more cards.
    function renderRequestHistory(requests) {
      var host = document.getElementById('admin-request-history');
      if (!host) return;
      var decided = (requests || []).filter(function(pr) {
        return pr && pr.status && pr.status !== 'pending';
      });
      if (!decided.length) {
        host.innerHTML = '<div style="color:var(--muted);font-size:0.75rem;padding:6px 0">No decided requests yet.</div>';
        return;
      }
      // Most recently decided first; fall back to requested_at on legacy records
      // that predate decided_at so they still sort somewhere sensible.
      decided.sort(function(a, b) {
        var av = a.decided_at || a.requested_at || '';
        var bv = b.decided_at || b.requested_at || '';
        return bv.localeCompare(av);
      });
      var rows = decided.map(function(pr) {
        var approved = pr.status === 'approved';
        var color = approved ? 'var(--green)' : 'var(--red)';
        var repoLabel = esc(pr.org || '') + '/' + esc(pr.primary_repo || pr.repos || '');
        var host_ = pr.github_host || 'github.com';
        // For an approval the outcome is the hive they received; for a denial it
        // is the reason, if one was given. Legacy records have neither.
        var outcome = approved
          ? (pr.assigned_hive
              ? '<span style="font-family:ui-monospace,monospace;font-size:0.7rem">' + esc(pr.assigned_hive) + '</span>'
              : '<span style="color:var(--muted)">—</span>')
          : (pr.deny_reason
              ? '<span style="color:var(--muted)">' + esc(pr.deny_reason) + '</span>'
              : '<span style="color:var(--muted)">—</span>');
        return '<tr>' +
          '<td style="white-space:nowrap"><img src="https://github.com/' + esc(pr.username) + '.png?size=40" alt="" style="width:18px;height:18px;border-radius:50%;vertical-align:middle;margin-right:6px" onerror="this.style.visibility=\'hidden\'">' + esc(pr.username) + '</td>' +
          '<td>' + repoLabel + ' <span style="font-size:0.65rem;color:var(--muted)">' + esc(host_) + '</span></td>' +
          '<td style="white-space:nowrap">' + acmmBadge(pr.acmm_level) + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.requested_at || '').substring(0, 10)) + '</td>' +
          '<td style="white-space:nowrap"><span style="color:' + color + ';font-weight:600;font-size:0.72rem">' + esc(pr.status) + '</span></td>' +
          '<td style="white-space:nowrap">' + esc(pr.decided_by || '—') + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.decided_at || '').substring(0, 10) || '—') + '</td>' +
          '<td>' + outcome + '</td>' +
          '</tr>';
      }).join('');
      host.innerHTML =
        '<table class="hive-table" style="width:100%;font-size:0.78rem">' +
        '<thead><tr><th>User</th><th>Requested</th><th>ACMM</th><th>On</th>' +
        '<th>Decision</th><th>By</th><th>When</th><th>Assigned / reason</th></tr></thead>' +
        '<tbody>' + rows + '</tbody></table>';
    }

    function renderAdminProvisionRequests(requests) {
      var section = document.getElementById('admin-provision-requests');
      var list = document.getElementById('admin-provision-list');
      if (!section || !list) return;
      // History covers decided requests; the cards below cover pending ones. Do
      // this before the pending early-return, or the history disappears the
      // moment the queue empties — which is exactly when it matters most.
      renderRequestHistory(requests);
      var pending = (requests || []).filter(function(pr) {
        return pr && (!pr.status || pr.status === 'pending');
      });
      if (!pending.length) {
        // Keep the section visible when there is history to show, so the table
        // does not vanish along with the empty queue.
        var anyDecided = (requests || []).some(function(pr) { return pr && pr.status && pr.status !== 'pending'; });
        list.innerHTML = '<div style="color:var(--muted);font-size:0.75rem;padding:6px 0">No pending requests.</div>';
        section.style.display = anyDecided ? '' : 'none';
        return;
      }
      section.style.display = '';
      // Stash by username so the approve-picker modal can read the full request
      // (org/repos/acmm) without threading every field through an onclick string.
      _provisionRequestsByUser = {};
      pending.forEach(function(pr) { _provisionRequestsByUser[pr.username] = pr; });
      var rows = pending.map(function(pr) {
        var avatar = '<img src="https://github.com/' + esc(pr.username) + '.png" style="width:24px;height:24px;border-radius:50%;vertical-align:middle;margin-right:8px">';
        return '<div style="display:flex;align-items:center;justify-content:space-between;padding:10px 14px;background:var(--surface);border:1px solid var(--border);border-radius:8px;margin-bottom:8px">' +
          '<div style="display:flex;align-items:center;gap:8px">' +
          avatar +
          '<div>' +
          '<span style="font-size:0.85rem;font-weight:600">' + esc(pr.username) + '</span>' +
          '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' + esc(pr.org) + '/' + esc(pr.primary_repo || pr.repos) + '</span>' +
          // Show which GitHub instance the request targets. Blank github_host
          // means public github.com; a GHE host (github.ibm.com, …) is called
          // out so the admin can place the hive on a cluster that can reach it.
          ' <span style="font-size:0.68rem;padding:1px 7px;border-radius:999px;border:1px solid ' + (pr.github_host ? 'var(--accent);color:var(--accent)' : 'var(--border);color:var(--muted)') + '">' + esc(pr.github_host || 'github.com') + '</span>' +
          ' ' + acmmBadge(pr.acmm_level) +
          '<div style="font-size:0.7rem;color:var(--muted)">' + esc((pr.requested_at || '').substring(0, 10)) + '</div>' +
          '</div></div>' +
          '<div style="display:flex;gap:6px">' +
          '<button onclick="openApproveModal(\'' + esc(pr.username) + '\')" style="padding:5px 14px;background:var(--green);color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:0.78rem;font-weight:600">Approve</button>' +
          '<button onclick="denyProvision(\'' + esc(pr.username) + '\',this)" style="padding:5px 14px;background:var(--red);color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:0.78rem;font-weight:600">Deny</button>' +
          '</div></div>';
      }).join('');
      list.innerHTML = rows;
    }

    var _provisionRequestsByUser = {};
    async function openApproveModal(username) {
      var pr = _provisionRequestsByUser[username] || {username: username};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var reposText = pr.primary_repo || pr.repos || '';
      var summary =
        '<div style="padding:10px 12px;background:var(--surface);border:1px solid var(--border);border-radius:6px;font-size:0.8rem;margin-bottom:4px">' +
        '<div><span style="color:var(--muted)">User:</span> <strong>' + esc(username) + '</strong></div>' +
        '<div><span style="color:var(--muted)">Org:</span> ' + esc(pr.org || '') + '</div>' +
        '<div><span style="color:var(--muted)">Repos:</span> ' + esc(pr.repos || '') + '</div>' +
        '<div><span style="color:var(--muted)">Primary:</span> ' + esc(pr.primary_repo || '') + '</div>' +
        '<div><span style="color:var(--muted)">ACMM:</span> ' + (pr.acmm_level != null ? pr.acmm_level : '—') + '</div>' +
        '</div>';
      var content =
        summary +
        '<label style="' + lbl + '">Placeholder to assign</label>' +
        '<select id="approve-hive" style="' + fld + '"><option value="">Loading placeholders…</option></select>' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-top:6px">Auto-pick chooses an available placeholder from the request&#39;s pool.</div>' +
        '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
        '<button onclick="closeApproveModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button id="approve-submit" onclick="confirmApprove(\'' + esc(username) + '\')" style="padding:6px 16px;background:#3fb950;color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Approve</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'approve-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:440px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">Approve &amp; assign placeholder</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeApproveModal(); });
      document.body.appendChild(overlay);
      // Populate the dropdown with available placeholders (auto-pick default).
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        var data = await resp.json();
        var sel = document.getElementById('approve-hive');
        if (!sel) return;
        var opts = '<option value="">Auto-pick</option>';
        (data.placeholders || []).forEach(function(p) {
          opts += '<option value="' + esc(p.id) + '">' + esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        });
        sel.innerHTML = opts;
      } catch(e) {
        var sel2 = document.getElementById('approve-hive');
        if (sel2) sel2.innerHTML = '<option value="">Auto-pick</option>';
      }
    }
    function closeApproveModal() {
      var ov = document.getElementById('approve-overlay');
      if (ov) ov.remove();
    }
    async function confirmApprove(username) {
      var sel = document.getElementById('approve-hive');
      var hiveId = sel ? sel.value : '';
      var submit = document.getElementById('approve-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Assigning...'; }
      try {
        // Approving assigns a placeholder (the chosen one, or auto-pick from the
        // correct pool when hive_id is empty) and marks the request fulfilled.
        var resp = await fetch('/api/saas/approve-provision/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({hive_id: hiveId})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Assign failed', 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Approve'; } return; }
        closeApproveModal();
        hiveToast('Approved ' + username + ' → ' + (hiveId || 'auto') + ' (' + (data.hive_id || 'a hive') + ')', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Approve'; } }
    }

    async function denyProvision(username, btn) {
      if (!await hiveConfirm('Deny provision request from ' + username + '?')) return;
      btn.disabled = true;
      btn.textContent = 'Denying...';
      try {
        var resp = await fetch('/api/saas/deny-provision/' + encodeURIComponent(username), {method: 'DELETE'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Deny failed', 'error'); btn.disabled = false; btn.textContent = 'Deny'; return; }
        hiveToast('Provision request denied for ' + username, 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Deny'; }
    }

    var _requestInProgress = false;
    async function submitProvisionRequest() {
      if (_requestInProgress) return;
      _requestInProgress = true;
      var btn = document.getElementById('btn-request-go');
      btn.disabled = true;
      btn.textContent = 'Submitting...';
      var org = document.getElementById('rq-org').value.trim();
      var repos = document.getElementById('rq-repos').value.trim();
      var primary = document.getElementById('rq-primary').value.trim();
      var level = parseInt(document.getElementById('rq-level').value) || 1;

      if (!org || !repos) { hiveToast('Org and repos are required', 'error'); _requestInProgress = false; btn.disabled = false; btn.textContent = 'Submit Request'; return; }

      try {
        var resp = await fetch('/api/saas/request-provision', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({org: org, repos: repos, primary_repo: primary || repos.split(',')[0].trim(), acmm_level: level})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Request failed', 'error'); return; }
        document.getElementById('request-modal').style.display = 'none';
        hiveToast('Provision request submitted — awaiting admin approval', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { _requestInProgress = false; btn.disabled = false; btn.textContent = 'Submit Request'; }
    }

    // --- Cluster Health Panel ---
    var CLUSTER_HEALTH_POLL_MS = 30000;
    var CLUSTER_CPU_WARN_PCT = 60;
    var CLUSTER_CPU_DANGER_PCT = 80;
    var CLUSTER_MEM_WARN_PCT = 60;
    var CLUSTER_MEM_DANGER_PCT = 80;
    var _clusterHealthCollapsed = localStorage.getItem('hive-cluster-health-collapsed') !== 'false';

    function toggleClusterHealth() {
      _clusterHealthCollapsed = !_clusterHealthCollapsed;
      localStorage.setItem('hive-cluster-health-collapsed', _clusterHealthCollapsed ? 'true' : 'false');
      var body = document.getElementById('cluster-health-body');
      var toggle = document.getElementById('cluster-health-toggle');
      if (body) body.style.display = _clusterHealthCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _clusterHealthCollapsed ? '' : 'rotate(90deg)';
    }

    function healthBarColor(pct, warnThreshold, dangerThreshold) {
      if (pct >= dangerThreshold) return 'var(--red)';
      if (pct >= warnThreshold) return 'var(--accent)';
      return 'var(--green)';
    }

    var SPARKLINE_BLOCKS = '▁▂▃▄▅▆▇█';
    var SPARKLINE_HISTORY_LEN = 10;
    var _clusterSparkHistory = {};

    function pushSparkPoint(nodeKey, metric, value) {
      var key = nodeKey + ':' + metric;
      if (!_clusterSparkHistory[key]) _clusterSparkHistory[key] = [];
      _clusterSparkHistory[key].push(value);
      if (_clusterSparkHistory[key].length > SPARKLINE_HISTORY_LEN) _clusterSparkHistory[key].shift();
    }

    function renderUnicodeSparkline(nodeKey, metric, color) {
      var key = nodeKey + ':' + metric;
      var pts = _clusterSparkHistory[key] || [];
      if (pts.length < 2) return '';
      var max = Math.max.apply(null, pts);
      if (max === 0) max = 1;
      var blocks = SPARKLINE_BLOCKS;
      var spark = pts.map(function(v) {
        var idx = Math.round((v / max) * (blocks.length - 1));
        return blocks[Math.min(idx, blocks.length - 1)];
      }).join('');
      return '<span style="font-family:monospace;font-size:0.7rem;color:' + color + ';letter-spacing:-1px">' + spark + '</span>';
    }

    function renderHealthMetric(label, used, total, unit, pct, warnThreshold, dangerThreshold, nodeKey, metric) {
      var color = healthBarColor(pct, warnThreshold, dangerThreshold);
      pushSparkPoint(nodeKey, metric, pct);
      var spark = renderUnicodeSparkline(nodeKey, metric, color);
      return '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:4px">' +
        '<span style="font-size:0.7rem;color:var(--muted)">' + label + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' +
        spark +
        '<span style="font-family:monospace;font-size:0.75rem;color:' + color + '">' + used + ' / ' + total + ' ' + unit + '</span>' +
        '<span style="font-size:0.7rem;min-width:28px;text-align:right;color:' + color + '">' + pct + '%</span>' +
        '</span></div>';
    }

    function renderNodeCard(n) {
      var nk = n.name;
      var readyBadge = (n.conditions || []).indexOf('Ready') >= 0
        ? '<span style="color:var(--green);font-size:0.7rem;font-weight:600">Ready</span>'
        : '<span style="color:var(--red);font-size:0.7rem;font-weight:600">NotReady</span>';
      var diskWarn = n.disk_pressure
        ? '<div style="margin-top:6px;padding:4px 8px;background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.3);border-radius:4px;font-size:0.7rem;color:var(--red)">⚠ Disk Pressure</div>'
        : '';
      var cpuUsed = (n.cpu_used_millicores / 1000).toFixed(1);
      var memUsedGB = (n.mem_used_mb / 1024).toFixed(1);
      var memTotalGB = Math.round(n.mem_total_mb / 1024);
      var hiveCount = n.hive_count || 0;
      var hivePill = '<span style="padding:2px 8px;background:var(--bg);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;color:var(--muted)">' + hiveCount + (hiveCount === 1 ? ' hive' : ' hives') + '</span>';
      return '<div style="background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:14px">' +
        '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">' +
        '<span style="font-family:monospace;font-size:0.8rem;color:var(--text)">' + esc(nk) + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' + readyBadge +
        hivePill +
        '<span style="padding:2px 8px;background:var(--bg);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;color:var(--muted)">' + (n.pods || 0) + '/' + (n.pod_capacity || 0) + ' pods</span>' +
        '</span></div>' +
        renderHealthMetric('CPU', cpuUsed, n.cpu_cores, 'cores', n.cpu_percent, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT, nk, 'cpu') +
        renderHealthMetric('MEM', memUsedGB, memTotalGB, 'GB', n.mem_percent, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT, nk, 'mem') +
        diskWarn +
        '</div>';
    }

    async function loadClusterHealth() {
      if (!_isAdmin) return;
      try {
        var resp = await fetch('/api/saas/cluster-health');
        if (resp.status === 403) {
          document.getElementById('cluster-health-section').style.display = 'none';
          return;
        }
        if (!resp.ok) return;
        var data = await resp.json();
        document.getElementById('cluster-health-section').style.display = '';

        var s = data.summary || {};
        var summaryBar = document.getElementById('cluster-health-summary-bar');
        if (summaryBar) {
          pushSparkPoint('_cluster', 'cpu', s.total_cpu_percent || 0);
          pushSparkPoint('_cluster', 'mem', s.total_mem_percent || 0);
          var cpuColor = healthBarColor(s.total_cpu_percent || 0, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT);
          var memColor = healthBarColor(s.total_mem_percent || 0, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT);
          var clusterCount = (data.clusters || []).length;
          summaryBar.innerHTML = clusterCount + ' cluster' + (clusterCount !== 1 ? 's' : '') + ' · ' +
            (s.total_nodes || 0) + ' nodes · ' + (s.total_cpu_cores || 0) + ' vCPU · ' +
            renderUnicodeSparkline('_cluster', 'cpu', cpuColor) + ' <span style="color:' + cpuColor + '">' + (s.total_cpu_percent || 0) + '% cpu</span> · ' +
            renderUnicodeSparkline('_cluster', 'mem', memColor) + ' <span style="color:' + memColor + '">' + (s.total_mem_percent || 0) + '% mem</span> · ' +
            (s.hive_count || 0) + ' hives';
        }

        var body = document.getElementById('cluster-health-body');
        var toggle = document.getElementById('cluster-health-toggle');
        if (!_clusterHealthCollapsed) {
          if (body) body.style.display = '';
          if (toggle) toggle.style.transform = 'rotate(90deg)';
        }

        var grid = document.getElementById('cluster-health-grid');
        if (!grid) return;

        // Render per-cluster sections if available, otherwise fall back to flat nodes.
        var clusters = data.clusters || [];
        if (clusters.length > 0) {
          grid.style.display = 'block';
          grid.innerHTML = clusters.map(function(c) {
            var cLabel = c.name || c.id;
            var cs = c.summary || {};
            var cCpuColor = healthBarColor(cs.total_cpu_percent || 0, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT);
            var cMemColor = healthBarColor(cs.total_mem_percent || 0, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT);
            var gpuLine = '';
            if (c.gpu_summary) {
              gpuLine = ' · <span style="color:var(--green)">' + c.gpu_summary.allocatable_gpus + '/' + c.gpu_summary.total_gpus + ' GPUs</span>';
            }
            // Remaining hive capacity: omitted entirely when the collector had
            // no pod request data (field absent); 0 means the cluster is full.
            var capacityLine = '';
            if (cs.hive_capacity_remaining != null) {
              var capRemaining = cs.hive_capacity_remaining;
              capacityLine = ' · <span title="Estimated headroom: per-hive CPU/memory request footprint bin-packed into free (allocatable minus requested) capacity on Ready, schedulable nodes only">room for ~' + capRemaining + ' more hive' + (capRemaining === 1 ? '' : 's') + '</span>';
            }
            var errorLine = c.error ? '<div style="margin:8px 0;padding:6px 10px;background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.3);border-radius:6px;font-size:0.75rem;color:var(--red)">' + esc(c.error) + '</div>' : '';
            var headerHtml = '<div style="display:flex;align-items:center;gap:8px;margin:16px 0 8px">' +
              clusterBadge(c.id, c.name) +
              '<span style="font-size:0.85rem;color:var(--text);font-weight:600">' + esc(cLabel) + '</span>' +
              '<span style="font-size:0.75rem;color:var(--muted)">' +
              (cs.total_nodes || 0) + ' nodes · ' + (cs.total_cpu_cores || 0) + ' vCPU · ' +
              '<span style="color:' + cCpuColor + '">' + (cs.total_cpu_percent || 0) + '% cpu</span> · ' +
              '<span style="color:' + cMemColor + '">' + (cs.total_mem_percent || 0) + '% mem</span> · ' +
              (c.hive_count || 0) + ' hives' + capacityLine + gpuLine +
              '</span></div>';
            var nodesHtml = (c.nodes || []).length > 0
              ? '<div style="display:grid;grid-template-columns:repeat(2,1fr);gap:12px">' + (c.nodes || []).map(renderNodeCard).join('') + '</div>'
              : '';
            return headerHtml + errorLine + nodesHtml;
          }).join('');
        } else {
          // Fallback: flat node list (single cluster / backward compat).
          grid.style.display = 'grid';
          grid.style.gridTemplateColumns = 'repeat(2,1fr)';
          var nodes = data.nodes || [];
          if (!nodes.length) {
            grid.innerHTML = '<div style="color:var(--muted);font-size:0.85rem;grid-column:span 2">No node data available</div>';
            return;
          }
          grid.innerHTML = nodes.map(renderNodeCard).join('');
        }
      } catch(e) {
        console.error('cluster health error:', e);
      }
    }

    async function loadClusters() {
      try {
        var resp = await fetch('/api/hub/clusters');
        if (!resp.ok) return;
        var clusters = await resp.json();
        _clusterList = clusters || [];
        var sel = document.getElementById('f-cluster');
        if (!sel || !clusters || !clusters.length) return;
        sel.innerHTML = clusters.map(function(c) {
          var caps = [];
          if (c.has_gpu) caps.push('GPU');
          if (c.arch) caps.push(c.arch);
          var label = c.name || c.id;
          if (caps.length) label += ' (' + caps.join(', ') + ')';
          return '<option value="' + esc(c.id) + '">' + esc(label) + '</option>';
        }).join('');
      } catch(e) { /* cluster dropdown stays at default */ }
    }

    // OpenRouter scan-to-fund return: toast the result and clear the query flag
    // so a reload doesn't re-toast.
    function handleOpenRouterReturn() {
      try {
        var p = new URLSearchParams(window.location.search);
        var or = p.get('openrouter');
        if (or === 'connected') hiveToast('OpenRouter funded — the key is being delivered to the hive', 'success');
        else if (or === 'error') hiveToast('OpenRouter funding failed — please try again', 'error');
        if (or) {
          p.delete('openrouter');
          var qs = p.toString();
          history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : ''));
        }
      } catch (e) { /* non-fatal */ }
    }

    async function init() {
      await loadUser();
      await autoRequestAccessFromUrl();
      await loadHives();
      await loadAdminUsers();
      if (!_adminLoaded) setTimeout(loadAdminUsers, 2000);
      loadClusterHealth();
      loadClusters();
      handleOpenRouterReturn();
    }
    init();
    var POLL_INTERVAL_MS = 30000;
    setInterval(loadHives, POLL_INTERVAL_MS);
    setInterval(loadAdminUsers, POLL_INTERVAL_MS);
    setInterval(loadClusterHealth, CLUSTER_HEALTH_POLL_MS);
    var _refreshTimer = null;
    var REFRESH_DEBOUNCE_MS = 500;
    function debouncedRefresh() {
      if (_refreshTimer) return;
      _refreshTimer = setTimeout(function() { _refreshTimer = null; loadHives(); loadAdminUsers(); }, REFRESH_DEBOUNCE_MS);
    }
    document.addEventListener('visibilitychange', function() { if (!document.hidden) debouncedRefresh(); });
    window.addEventListener('focus', debouncedRefresh);

    var _allUsers = [];
    var _adminLoaded = false;
    var _adminExpandedUsers = {};
    var _hiveRegistry = [];
    var _userSortKey = 'created_at', _userSortAsc = false;

    function fmtUserTS(ts) {
      if (!ts) return '';
      var d = new Date(ts);
      if (isNaN(d.getTime())) return ts.substring(0, 10);
      // 12-hour clock with a compact single-letter meridiem (e.g. "9:21p").
      var parts = d.toLocaleString('en-US', {year:'numeric',month:'2-digit',day:'2-digit',hour:'numeric',minute:'2-digit',hour12:true,timeZone:'America/New_York'}).replace(',','').split(' ');
      var meridiem = (parts.pop() || '').toLowerCase().charAt(0); // "AM"->"a", "PM"->"p"
      return parts.join(' ') + meridiem + ' EDT';
    }

    function sortUsers(key) {
      if (_userSortKey === key) { _userSortAsc = !_userSortAsc; } else { _userSortKey = key; _userSortAsc = true; }
      applySortUsers();
    }

    function applySortUsers() {
      var key = _userSortKey;
      var q = (document.getElementById('user-search') ? document.getElementById('user-search').value : '').toLowerCase();
      var filtered = _allUsers.filter(function(u) { return !q || u.github_username.toLowerCase().includes(q); });
      var sorted = filtered.slice().sort(function(a, b) {
        var va, vb;
        if (key === 'hiveCount') {
          var regIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
          va = Object.keys(a.hives || {}).filter(function(h) { return regIds.has(h); }).length;
          vb = Object.keys(b.hives || {}).filter(function(h) { return regIds.has(h); }).length;
        } else if (key === 'status') {
          va = a.blocked ? 1 : 0;
          vb = b.blocked ? 1 : 0;
        } else {
          va = a[key] || ''; vb = b[key] || '';
        }
        if (typeof va === 'number' && typeof vb === 'number') return _userSortAsc ? va - vb : vb - va;
        return _userSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
      renderUsers(sorted, true);
    }

    function toggleAdminExpand(username) {
      _adminExpandedUsers[username] = !_adminExpandedUsers[username];
      var el = document.getElementById('expand-' + username);
      if (el) el.style.display = _adminExpandedUsers[username] ? '' : 'none';
    }
    var _adminLoading = false;
    async function loadAdminUsers() {
      if (_adminLoading) return;
      _adminLoading = true;
      try {
        var resp = await fetch('/api/saas/admin/users');
        if (resp.status === 403) {
          if (!_adminLoaded) { document.getElementById('admin-section').style.display = 'none'; document.getElementById('hub-banner-section').style.display = 'none'; document.getElementById('btn-send-banner-top').style.display = 'none'; }
          return;
        }
        _adminLoaded = true;
        document.getElementById('admin-section').style.display = '';
        document.getElementById('hub-banner-section').style.display = '';
        document.getElementById('btn-send-banner-top').style.display = '';
        loadActiveBanner();
        var data = await resp.json();
        _allUsers = data.users || [];
        try { applySortUsers(); } catch(re) { console.error('renderUsers error:', re); }
      } catch(e) {
        if (!_adminLoaded) document.getElementById('admin-section').style.display = 'none';
      } finally {
        _adminLoading = false;
      }
    }

    function filterUsers() {
      applySortUsers();
    }

    function renderUsers(users, force) {
      var sig = JSON.stringify(users);
      if (!force && sig === _lastUsersJSON) return;
      _lastUsersJSON = sig;
      if (!users.length) { document.getElementById('users-container').innerHTML = '<div class="loading">No users found</div>'; return; }
      var rows = users.map(function(u) {
        var blocked = u.blocked ? '<span style="color:var(--red);font-weight:600">BLOCKED</span>' : '<span style="color:var(--green)">active</span>';
        var avatar = '<img src="https://github.com/' + esc(u.github_username) + '.png" style="width:24px;height:24px;border-radius:50%;vertical-align:middle;margin-right:6px">';
        var isAdmin = u.github_username === 'clubanderson';
        var hivesObj = u.hives || {};
        var registryIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
        var hiveIds = Object.keys(hivesObj).filter(function(hid) { return registryIds.has(hid); });
        var hiveCount = hiveIds.length;
        var expandId = 'expand-' + esc(u.github_username);
        var isExpanded = _adminExpandedUsers && _adminExpandedUsers[u.github_username];

        var hiveRows = '';
        if (hiveCount > 0) {
          hiveRows = '<tr id="' + expandId + '" style="display:' + (isExpanded ? '' : 'none') + '"><td colspan="7"><div style="padding:8px 12px 8px 40px;font-size:0.75rem">';
          hiveRows += '<table style="width:100%;border-collapse:collapse"><thead><tr style="color:var(--muted);font-size:0.7rem"><th style="text-align:left;padding:4px 8px">Hive</th><th>Role</th><th>Type</th><th>Link</th></tr></thead><tbody>';
          hiveIds.forEach(function(hid) {
            var role = hivesObj[hid];
            var isHosted = hid.startsWith('hosted-') || hid.startsWith('saas-');
            var regEntry = (_hiveRegistry || []).find(function(h) { return h.id === hid; });
            var hiveName = regEntry ? (regEntry.name || hid) : hid;
            // Prefer the hive's heartbeat-reported dashboard URL so firewalled
            // spokes (vllm-d etc.) link to their real route, not a dead
            // <id>.hive.kubestellar.io host. Fall back to the hive-oke pattern.
            var linkBase = (regEntry && regEntry.dashboardUrl && !regEntry.dashboardUrl.includes('localhost'))
              ? regEntry.dashboardUrl : (isHosted ? 'https://' + esc(hid) + '.hive.kubestellar.io' : '');
            var linkLabel = linkBase.replace(/^https?:\/\//, '');
            var link = linkBase ? '<a href="' + esc(linkBase) + '" target="_blank" class="dash-link">' + esc(linkLabel) + '</a>' : '<span style="color:var(--muted)">local</span>';
            var typeBadge = isHosted ? '<span style="color:#60a5fa">hosted</span>' : '<span style="color:#9ca3af">local</span>';
            hiveRows += '<tr><td style="padding:4px 8px">' + esc(hiveName) + '</td><td style="text-align:center">' + esc(role) + '</td><td style="text-align:center">' + typeBadge + '</td><td>' + link + '</td></tr>';
          });
          hiveRows += '</tbody></table></div></td></tr>';
        }

        return '<tr>' +
          '<td>' + avatar + '<a href="https://github.com/' + esc(u.github_username) + '" target="_blank">' + esc(u.github_username) + '</a>' + (isAdmin ? ' <span style="color:var(--accent);font-size:0.7rem">admin</span>' : '') + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.created_at)) + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.last_login)) + '</td>' +
          '<td>' + blocked + '</td>' +
          '<td><input type="number" min="0" max="10" value="' + (u.saas_quota || 0) + '" style="width:50px;padding:4px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);text-align:center" onchange="updateUser(\'' + esc(u.github_username) + '\',{saas_quota:parseInt(this.value)||0})"></td>' +
          '<td>' + (hiveCount > 0 ? '<a href="#" onclick="toggleAdminExpand(\'' + esc(u.github_username) + '\');return false" style="color:var(--blue);font-size:0.8rem">' + hiveCount + ' hive' + (hiveCount > 1 ? 's' : '') + '</a>' : '<span style="color:var(--muted)">0</span>') + '</td>' +
          '<td>' + (isAdmin ? '' : '<button onclick="updateUser(\'' + esc(u.github_username) + '\',{blocked:' + (!u.blocked) + '})" style="padding:3px 10px;background:' + (u.blocked ? 'var(--green)' : 'var(--amber)') + ';color:' + (u.blocked ? '#fff' : '#1a1a1a') + ';border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">' + (u.blocked ? 'Unblock' : 'Block') + '</button> <button onclick="deleteUser(\'' + esc(u.github_username) + '\',' + hiveCount + ')" style="padding:3px 10px;background:#b02a2a;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">Delete</button>') + '</td>' +
          '</tr>' + hiveRows;
      }).join('');
      document.getElementById('users-container').innerHTML =
        '<table class="hive-table"><thead><tr>' +
        '<th onclick="sortUsers(\'github_username\')" style="cursor:pointer">User ⇅</th><th onclick="sortUsers(\'created_at\')" style="cursor:pointer">Joined ⇅</th><th onclick="sortUsers(\'last_login\')" style="cursor:pointer">Last Login ⇅</th><th onclick="sortUsers(\'status\')" style="cursor:pointer">Status ⇅</th><th onclick="sortUsers(\'saas_quota\')" style="cursor:pointer">Quota ⇅</th><th onclick="sortUsers(\'hiveCount\')" style="cursor:pointer">Hives ⇅</th><th>Actions</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
    }

    async function updateUser(username, updates) {
      try {
        await fetch('/api/saas/admin/users/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(updates)
        });
        loadAdminUsers();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function deleteUser(username, hiveCount) {
      if (hiveCount > 0) {
        hiveToast('Cannot delete ' + username + ' — they still own ' + hiveCount + ' hive(s). Delete or reassign those first.', 'error');
        return;
      }
      if (!await hiveConfirm('Delete hub user ' + username + '? This removes their hub account record (login, quota, saved token). It does not touch GitHub.')) return;
      try {
        var resp = await fetch('/api/saas/admin/users/' + encodeURIComponent(username), { method: 'DELETE' });
        if (!resp.ok) {
          var e = await resp.json().catch(function(){ return {}; });
          hiveToast('Delete failed: ' + (e.error || resp.status), 'error');
          return;
        }
        hiveToast('Deleted ' + username, 'success');
        loadAdminUsers();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function deleteHive(id) {
      if (!await hiveConfirm('Delete ' + id + '? This removes the namespace, PV, OCI storage, and all data.')) return;
      var btns = document.querySelectorAll('button[onclick*="deleteHive"]');
      btns.forEach(function(b) { b.disabled = true; b.textContent = 'Deleting...'; b.style.opacity = '0.6'; });
      try {
        gtag('event','hive_deleted',{hive_id:id});
        hiveToast('Deleting ' + id + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Delete failed', 'error'); return; }
        hiveToast('Deleted ' + id, 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { btns.forEach(function(b) { b.disabled = false; b.textContent = 'Delete'; b.style.opacity = '1'; }); }
    }

    function toggleHiveMenu(menuId) {
      var menu = document.getElementById(menuId);
      var wasOpen = menu.style.display !== 'none';
      document.querySelectorAll('[id^="hive-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      if (!wasOpen) menu.style.display = 'block';
    }
    document.addEventListener('click', function(e) {
      if (!e.target.closest('[id^="hive-menu-"]') && !e.target.closest('[onclick*="toggleHiveMenu"]')) {
        document.querySelectorAll('[id^="hive-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      }
    });

    function openMigrateModal(hiveId, currentClusterId) {
      var targets = (_clusterList || []).filter(function(c) { return c.id !== currentClusterId; });
      if (!targets.length) { hiveToast('No other clusters available', 'error'); return; }
      var currentName = (_clusterList || []).reduce(function(n, c) { return c.id === currentClusterId ? (c.name || c.id) : n; }, currentClusterId);
      var options = targets.map(function(c) {
        var caps = [];
        if (c.has_gpu) caps.push('GPU');
        if (c.arch) caps.push(c.arch);
        var label = c.name || c.id;
        if (caps.length) label += ' (' + caps.join(', ') + ')';
        return '<option value="' + esc(c.id) + '">' + esc(label) + '</option>';
      }).join('');
      var content = '<div style="margin-bottom:12px">Move <strong>' + esc(hiveId) + '</strong> from <strong>' + esc(currentName) + '</strong> to:</div>' +
        '<select id="migrate-target" style="width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;margin-bottom:12px">' + options + '</select>' +
        '<div style="padding:8px 12px;background:rgba(234,179,8,0.1);border:1px solid rgba(234,179,8,0.3);border-radius:6px;font-size:0.8rem;color:#eab308;margin-bottom:12px">The hive will be reprovisioned on the target cluster. This may take a few minutes. The hive will rebuild its state from GitHub.</div>' +
        '<div style="display:flex;gap:8px;justify-content:flex-end">' +
        '<button onclick="closeMigrateModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button onclick="confirmMigrate(\'' + esc(hiveId) + '\')" style="padding:6px 16px;background:var(--accent);color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Move</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'migrate-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:420px;width:90%">' +
        '<h3 style="margin:0 0 16px 0;font-size:1rem">Move to cluster</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeMigrateModal(); });
      document.body.appendChild(overlay);
    }
    function closeMigrateModal() {
      var ov = document.getElementById('migrate-overlay');
      if (ov) ov.remove();
    }
    async function confirmMigrate(hiveId) {
      var sel = document.getElementById('migrate-target');
      if (!sel) return;
      var targetId = sel.value;
      closeMigrateModal();
      hiveToast('Migrating ' + hiveId + '...', 'info');
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/migrate', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({target_cluster_id: targetId})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Migration failed', 'error'); return; }
        gtag('event', 'hive_migrate', {hive_id: hiveId, from: data.from, to: data.to});
        hiveToast('Migration started: ' + hiveId + ' moving to ' + targetId, 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    var ASSIGN_DEFAULT_ACMM = 2;
    function openAssignModal(hiveId) {
      var h = (_allDashHives || []).reduce(function(m, x) { return x.id === hiveId ? x : m; }, null) || {};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var acmmOpts = '';
      for (var lv = 0; lv <= 6; lv++) { acmmOpts += '<option value="' + lv + '"' + (lv === ASSIGN_DEFAULT_ACMM ? ' selected' : '') + '>' + lv + '</option>'; }
      var orgVal = esc(h.org || '');
      var reposVal = esc((h.repos || []).join(', '));
      var content =
        '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim placeholder <strong style="color:var(--fg)">' + esc(hiveId) + '</strong> for a real project.</div>' +
        '<label style="' + lbl + '">Owner (GitHub login) *</label>' +
        '<input id="assign-owner" style="' + fld + '" placeholder="octocat">' +
        '<label style="' + lbl + '">Org *</label>' +
        '<input id="assign-org" style="' + fld + '" value="' + orgVal + '" placeholder="my-org">' +
        '<label style="' + lbl + '">Repos * (comma-separated)</label>' +
        '<input id="assign-repos" style="' + fld + '" value="' + reposVal + '" placeholder="repo-a, repo-b">' +
        '<label style="' + lbl + '">Primary repo (optional, defaults to first)</label>' +
        '<input id="assign-primary" style="' + fld + '" placeholder="repo-a">' +
        '<label style="' + lbl + '">Project name (optional)</label>' +
        '<input id="assign-name" style="' + fld + '" placeholder="My Project">' +
        '<label style="' + lbl + '">ACMM level</label>' +
        '<select id="assign-acmm" style="' + fld + '">' + acmmOpts + '</select>' +
        '<label style="display:flex;align-items:center;gap:8px;margin:12px 0 4px;font-size:0.8rem;cursor:pointer"><input type="checkbox" id="assign-public"> Public (visible to everyone)</label>' +
        '<div onclick="toggleAssignAdvanced()" id="assign-adv-toggle" style="margin-top:12px;font-size:0.78rem;color:var(--accent);cursor:pointer;user-select:none">▸ GitHub App credentials (optional)</div>' +
        '<div id="assign-adv" style="display:none;margin-top:8px">' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-bottom:8px">Optional — the owner can install the App from their dashboard after assignment.</div>' +
        '<label style="' + lbl + '">App ID</label>' +
        '<input id="assign-app-id" style="' + fld + '" placeholder="123456">' +
        '<label style="' + lbl + '">Installation ID</label>' +
        '<input id="assign-install-id" style="' + fld + '" placeholder="654321">' +
        '<label style="' + lbl + '">App private key (PEM)</label>' +
        '<textarea id="assign-app-key" rows="4" style="' + fld + ';font-family:monospace;font-size:0.7rem" placeholder="-----BEGIN RSA PRIVATE KEY-----"></textarea>' +
        '</div>' +
        '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
        '<button onclick="closeAssignModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button id="assign-submit" onclick="confirmAssign(\'' + esc(hiveId) + '\')" style="padding:6px 16px;background:#3fb950;color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Assign</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'assign-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:440px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">Assign placeholder hive</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeAssignModal(); });
      document.body.appendChild(overlay);
    }
    function toggleAssignAdvanced() {
      var adv = document.getElementById('assign-adv');
      var tog = document.getElementById('assign-adv-toggle');
      if (!adv || !tog) return;
      var open = adv.style.display === 'none';
      adv.style.display = open ? 'block' : 'none';
      tog.textContent = (open ? '▾' : '▸') + ' GitHub App credentials (optional)';
    }
    function closeAssignModal() {
      var ov = document.getElementById('assign-overlay');
      if (ov) ov.remove();
    }
    async function confirmAssign(hiveId) {
      var owner = document.getElementById('assign-owner').value.trim();
      var org = document.getElementById('assign-org').value.trim();
      var repos = document.getElementById('assign-repos').value.trim();
      var primary = document.getElementById('assign-primary').value.trim();
      var name = document.getElementById('assign-name').value.trim();
      var acmm = parseInt(document.getElementById('assign-acmm').value, 10) || 0;
      var isPublic = document.getElementById('assign-public').checked;
      var appId = document.getElementById('assign-app-id').value.trim();
      var installId = document.getElementById('assign-install-id').value.trim();
      var appKey = document.getElementById('assign-app-key').value.trim();
      if (!owner) { hiveToast('Owner is required', 'error'); return; }
      if (!org) { hiveToast('Org is required', 'error'); return; }
      if (!repos) { hiveToast('Repos are required', 'error'); return; }
      var submit = document.getElementById('assign-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Assigning...'; }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/assign', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({owner: owner, org: org, repos: repos, primary_repo: primary, project_name: name, acmm_level: acmm, is_public: isPublic, app_id: appId, installation_id: installId, app_private_key: appKey})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Assignment failed', 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Assign'; } return; }
        closeAssignModal();
        hiveToast('Assigned ' + hiveId + ' to ' + owner, 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        if (submit) { submit.disabled = false; submit.textContent = 'Assign'; }
      }
    }

    // ---- OpenRouter scan-to-fund (hub side) ---------------------------------
    // Fund a SPECIFIC hive: pick a default model, scan a QR (or open the link)
    // to authorize on OpenRouter. The hub exchanges the code for a scoped key and
    // delivers it to the hive as the "openrouter" gateway over the next heartbeat.
    var _orFundPoll = null;
    async function openOpenRouterFundModal(hiveId, hiveName) {
      var models = { suggested: [], models: [], default: '' };
      try {
        var r = await fetch('/api/openrouter/models');
        if (r.ok) models = await r.json();
      } catch (e) { /* fall back to manual entry */ }
      var opts = (models.suggested || []).map(function(m) {
        return '<option value="' + esc(m.id) + '">' + esc(m.label) + '</option>';
      }).join('') + '<option value="__manual__">Enter a model id manually…</option>';
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var content =
        '<div style="font-size:0.8rem;color:var(--muted);margin-bottom:10px">Fund <strong style="color:var(--fg)">' + esc(hiveName) + '</strong> with an OpenRouter key. Scan the QR (or open the link) to authorize on OpenRouter — the scoped key is delivered to the hive as its <code>openrouter</code> gateway.</div>' +
        '<label style="display:block;font-size:0.75rem;color:var(--muted);margin-bottom:4px">Default model</label>' +
        '<select id="orf-model" onchange="orfModelChange()" style="' + fld + ';margin-bottom:8px">' + opts + '</select>' +
        '<input id="orf-model-manual" placeholder="e.g. anthropic/claude-opus-4.8" style="display:none;' + fld + ';margin-bottom:8px">' +
        '<div style="display:flex;gap:8px;margin-top:4px">' +
        '<button onclick="orfStart(\'' + esc(hiveId) + '\')" style="padding:7px 16px;background:var(--accent,#58a6ff);color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Generate QR</button>' +
        '<button onclick="closeOrFundModal()" style="padding:7px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Close</button>' +
        '</div>' +
        '<div id="orf-qr" style="margin-top:12px"></div>' +
        '<div id="orf-status" style="margin-top:8px;font-size:0.78rem"></div>';
      var overlay = document.createElement('div');
      overlay.id = 'orf-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:460px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">⚡ Fund with OpenRouter</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeOrFundModal(); });
      document.body.appendChild(overlay);
    }
    function orfModelChange() {
      var sel = document.getElementById('orf-model');
      var man = document.getElementById('orf-model-manual');
      if (sel && man) man.style.display = sel.value === '__manual__' ? 'block' : 'none';
    }
    function orfChosenModel() {
      var sel = document.getElementById('orf-model');
      if (!sel) return '';
      if (sel.value === '__manual__') {
        var man = document.getElementById('orf-model-manual');
        return (man && man.value.trim()) || '';
      }
      return sel.value;
    }
    async function orfStart(hiveId) {
      var model = orfChosenModel();
      var qr = document.getElementById('orf-qr');
      var status = document.getElementById('orf-status');
      if (qr) qr.innerHTML = '<span style="color:var(--muted);font-size:0.78rem">Preparing…</span>';
      try {
        var res = await fetch('/api/openrouter/connect/start?hive_id=' + encodeURIComponent(hiveId) + '&model=' + encodeURIComponent(model));
        if (!res.ok) { var e = await res.json().catch(function(){return{};}); throw new Error(e.error || ('HTTP ' + res.status)); }
        var data = await res.json();
        var authURL = data.authorize_url;
        var qrSrc = '/api/openrouter/qr?data=' + encodeURIComponent(authURL);
        if (qr) qr.innerHTML =
          '<div style="display:flex;gap:16px;align-items:center;flex-wrap:wrap">' +
          '<img src="' + esc(qrSrc) + '" alt="OpenRouter QR" width="180" height="180" style="border:6px solid #fff;border-radius:8px;background:#fff">' +
          '<div style="font-size:0.78rem"><div style="margin-bottom:6px">Scan with your phone, or on this device:</div>' +
          '<a href="' + esc(authURL) + '" target="_blank" rel="noopener" style="color:var(--accent,#58a6ff);font-weight:600">Open OpenRouter authorization ↗</a></div></div>';
        if (status) status.innerHTML = '<span style="color:var(--muted)">Waiting for authorization…</span>';
        orfStartPolling(hiveId);
      } catch (e) {
        if (qr) qr.innerHTML = '<span style="color:#f85149;font-size:0.78rem">Failed: ' + esc(e.message) + '</span>';
      }
    }
    function orfStartPolling(hiveId) {
      if (_orFundPoll) clearInterval(_orFundPoll);
      var ORF_POLL_MS = 4000;
      _orFundPoll = setInterval(function() { orfCheck(hiveId); }, ORF_POLL_MS);
    }
    async function orfCheck(hiveId) {
      var status = document.getElementById('orf-status');
      try {
        var res = await fetch('/api/openrouter/credit?hive_id=' + encodeURIComponent(hiveId));
        if (!res.ok) return;
        var d = await res.json();
        // pending_delivery flips true the moment the fund completes on the hub;
        // it then flips back to false once the hive drains it on a heartbeat.
        if (d.pending_delivery) {
          if (status) status.innerHTML = '<span style="color:var(--green,#3fb950);font-weight:600">✓ Funded — delivering to the hive on its next heartbeat…</span>';
        }
      } catch (e) { /* keep polling */ }
    }
    function closeOrFundModal() {
      if (_orFundPoll) { clearInterval(_orFundPoll); _orFundPoll = null; }
      var ov = document.getElementById('orf-overlay');
      if (ov) ov.remove();
    }

    async function removeLocalHive(id) {
      if (!await hiveConfirm('Remove ' + id + ' from the registry? The hive itself is not affected — it will reappear if it sends another heartbeat.')) return;
      try {
        var resp = await fetch('/api/hub/registry/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Remove failed', 'error'); return; }
        hiveToast('Removed ' + id + ' from registry', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    function openConvert(btn) {
      document.getElementById('f-org').value = btn.dataset.org || '';
      document.getElementById('f-repos').value = btn.dataset.repos || '';
      document.getElementById('f-primary').value = btn.dataset.primary || '';
      document.getElementById('f-name').value = btn.dataset.name || '';
      document.getElementById('f-level').value = btn.dataset.level || '1';
      document.getElementById('create-modal').style.display = 'flex';
      var dashUrl = (btn.dataset.dashUrl || '').replace(/\/$/, '');
      var dlLink = document.getElementById('yaml-download-link');
      var dlHref = document.getElementById('yaml-download-href');
      if (dashUrl && dlLink && dlHref) {
        dlHref.href = dashUrl + '/api/config/download';
        dlLink.style.display = '';
      } else if (dlLink) {
        dlLink.style.display = 'none';
      }
    }

    var _createInProgress = false;
    function openProvisionDialog(org, repos, primaryRepo, acmmLevel) {
      document.getElementById('f-org').value = org || '';
      document.getElementById('f-repos').value = repos || '';
      document.getElementById('f-primary').value = primaryRepo || '';
      var levelSelect = document.getElementById('f-level');
      if (levelSelect && acmmLevel) levelSelect.value = String(acmmLevel);
      document.getElementById('create-modal').style.display = 'flex';
    }

    async function createHive() {
      if (_createInProgress) return;
      _createInProgress = true;
      document.getElementById('btn-go').disabled = true;
      document.getElementById('btn-go').textContent = 'Provisioning...';
      var org = document.getElementById('f-org').value.trim();
      var repos = document.getElementById('f-repos').value.trim();
      var primary = document.getElementById('f-primary').value.trim();
      var name = document.getElementById('f-name').value.trim();
      var level = parseInt(document.getElementById('f-level').value) || 1;
      var clusterSel = document.getElementById('f-cluster');
      var clusterId = clusterSel ? clusterSel.value : '';
      var method = document.querySelector('input[name="auth-method"]:checked').value;
      var token = document.getElementById('f-token').value.trim();
      var appId = (document.getElementById('f-app-id') || {}).value || '';
      var installId = (document.getElementById('f-install-id') || {}).value || '';
      var appKey = (document.getElementById('f-app-key') || {}).value || '';

      gtag('event','hive_create_started',{org:org,primary_repo:primary,acmm_level:level,cluster_id:clusterId});
      if (!org || !repos) { hiveToast('Org and repos are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'pat' && !token) { hiveToast('GitHub token is required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'app' && (!appId || !installId || !appKey)) { hiveToast('App ID, Installation ID, and Private Key are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'later') { method = 'app'; appId = '3568013'; installId = ''; appKey = ''; }

      try {
        var body = {org: org, repos: repos, primary_repo: primary || repos.split(',')[0].trim(), project_name: name, acmm_level: level, cluster_id: clusterId, auth_method: method, is_public: document.getElementById('f-public').checked};
        if (method === 'pat') body.github_token = token;
        else { body.app_id = appId.trim(); body.installation_id = installId.trim(); body.app_private_key = appKey.trim(); }

        var resp = await fetch('/api/saas/hives', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(body)
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to create hive', 'error'); return; }

        document.getElementById('create-modal').style.display = 'none';
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';

        hiveToast('Hive ' + data.id + ' is provisioning!', 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
      } finally {
        _createInProgress = false;
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';
      }
    }

    function parseHiveYaml(text) {
      var cfg = {};
      var lines = text.split('\n');
      var section = '';
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i];
        var trimmed = line.replace(/\s+$/, '');
        if (/^project:/.test(trimmed)) { section = 'project'; continue; }
        if (/^github:/.test(trimmed)) { section = 'github'; continue; }
        if (/^governor:/.test(trimmed)) { section = 'governor'; continue; }
        if (/^\S/.test(trimmed) && /:/.test(trimmed)) { section = ''; continue; }
        if (section === 'project') {
          var m;
          if ((m = trimmed.match(/^\s+org:\s*(.+)/))) cfg.org = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+repos:\s*$/))) { cfg.repos = []; for (var j = i + 1; j < lines.length && /^\s+-\s/.test(lines[j]); j++) { cfg.repos.push(lines[j].replace(/^\s+-\s*/, '').trim().replace(/^["']|["']$/g, '')); } }
          if ((m = trimmed.match(/^\s+repos:\s*\[(.+)\]/))) cfg.repos = m[1].split(',').map(function(r) { return r.trim().replace(/^["']|["']$/g, ''); });
          if ((m = trimmed.match(/^\s+primary_repo:\s*(.+)/))) cfg.primary = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+name:\s*(.+)/))) cfg.name = m[1].trim().replace(/^["']|["']$/g, '');
        }
        if (section === 'github') {
          var m;
          if ((m = trimmed.match(/^\s+token:\s*(.+)/))) cfg.token = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+app_id:\s*(\d+)/))) cfg.appId = m[1];
          if ((m = trimmed.match(/^\s+installation_id:\s*(\d+)/)) && !trimmed.match(/docs_installation_id/)) cfg.installId = m[1];
        }
        if (section === 'governor') {
          var m;
          if ((m = trimmed.match(/^\s+acmm_level:\s*(\d+)/))) cfg.level = parseInt(m[1]);
        }
      }
      return cfg;
    }

    function applyYamlConfig(cfg) {
      if (cfg.org) document.getElementById('f-org').value = cfg.org;
      if (cfg.repos) document.getElementById('f-repos').value = cfg.repos.join(', ');
      if (cfg.primary) document.getElementById('f-primary').value = cfg.primary;
      if (cfg.name) document.getElementById('f-name').value = cfg.name;
      if (cfg.level) document.getElementById('f-level').value = cfg.level;
      if (cfg.appId) {
        document.querySelector('input[name="auth-method"][value="app"]').checked = true;
        document.getElementById('auth-pat').style.display = 'none';
        document.getElementById('auth-app').style.display = '';
        document.getElementById('f-app-id').value = cfg.appId;
        if (cfg.installId) document.getElementById('f-install-id').value = cfg.installId;
      } else if (cfg.token) {
        document.getElementById('f-token').value = cfg.token;
      }
      var drop = document.getElementById('yaml-drop');
      drop.innerHTML = '<div style="font-size:0.82rem;color:var(--green)">✓ Config loaded</div>';
    }

    function readYamlFile(file) {
      var reader = new FileReader();
      reader.onload = function() {
        var cfg = parseHiveYaml(reader.result);
        applyYamlConfig(cfg);
        hiveToast('Config loaded from ' + file.name, 'success');
      };
      reader.readAsText(file);
    }
  
;

    var _accessHiveId = '';

    async function openAccessModal(hiveId, dashUrl) {
      _accessHiveId = hiveId;
      // Show the hive's URL alongside its id. The raw placeholder id
      // (hosted-available-oke-05-placeholder-6q84) says nothing about where the
      // hive actually lives, and the vanity URL is what an owner recognises.
      var label = document.getElementById('access-hive-label');
      label.textContent = 'Hive: ' + hiveId;
      var urlEl = document.getElementById('access-hive-url');
      if (!urlEl) {
        urlEl = document.createElement('div');
        urlEl.id = 'access-hive-url';
        urlEl.style.cssText = 'padding:0 32px 4px;font-size:0.8rem';
        label.parentNode.insertBefore(urlEl, label.nextSibling);
      }
      if (dashUrl && dashUrl.indexOf('localhost') === -1) {
        urlEl.innerHTML = '<a href="' + esc(dashUrl) + '" target="_blank" rel="noopener" style="color:var(--blue);text-decoration:none">' +
          esc(dashUrl.replace(/^https?:\/\//, '')) + '</a>';
        urlEl.style.display = '';
      } else {
        urlEl.textContent = '';
        urlEl.style.display = 'none';
      }
      document.getElementById('access-modal').style.display = 'flex';
      await loadAccessList();
      await loadAccessUserDropdown();
      await loadPendingRequests();
    }

    async function loadPendingRequests() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests');
        if (!resp.ok) return;
        var data = await resp.json();
        var reqs = data.requests || [];
        var el = document.getElementById('pending-requests');
        if (!el) return;
        if (!reqs.length) { el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">No pending requests</span>'; return; }
        el.innerHTML = reqs.map(function(r) {
          var avatar = '<img src="https://github.com/' + esc(r.username) + '.png" style="width:20px;height:20px;border-radius:50%;vertical-align:middle;margin-right:6px">';
          var note = (r.note || '').trim();
          var noteHtml = note
            ? '<div style="margin-top:4px;font-size:0.75rem;color:var(--text);white-space:pre-wrap;word-break:break-word;background:var(--bg);border-left:2px solid var(--accent);padding:4px 8px;border-radius:2px">' + esc(note) + '</div>'
            : '<div style="margin-top:4px;font-size:0.72rem;color:var(--muted);font-style:italic">(no note)</div>';
          return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
            '<div style="display:flex;align-items:center;justify-content:space-between">' +
            '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(r.username) + '</span> <span style="font-size:0.7rem;color:var(--muted)">' + esc(r.requested_at.substring(0,10)) + '</span></div>' +
            '<div style="display:flex;gap:4px">' +
            '<select id="req-role-' + esc(r.username) + '" style="padding:2px 6px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.7rem"><option value="read">Read</option><option value="read-write">Read-Write</option></select>' +
            '<button onclick="approveRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Approve</button>' +
            '<button onclick="denyRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Deny</button>' +
            '</div></div>' + noteHtml + '</div>';
        }).join('');
      } catch(e) {}
    }

    async function approveRequest(username) {
      var role = (document.getElementById('req-role-' + username) || {}).value || 'read';
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/approve', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({role: role})
        });
        loadPendingRequests();
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function denyRequest(username) {
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/deny', {method: 'POST'});
        loadPendingRequests();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function loadAccessUserDropdown() {
      var sel = document.getElementById('access-username');
      if (!sel) return;
      try {
        // grantable-users, NOT admin/users: the latter is admin-only, so every
        // non-admin owner got a 403 and an empty dropdown with no explanation.
        var resp = await fetch('/api/saas/grantable-users');
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        var data = await resp.json();
        var users = (data.users || []);
        if (!users.length) {
          sel.innerHTML = '<option value="">No users yet — they must sign in to the hub once</option>';
          return;
        }
        sel.innerHTML = '<option value="">Select user...</option>' + users.map(function(u) {
          return '<option value="' + esc(u) + '">' + esc(u) + '</option>';
        }).join('');
      } catch(e) {
        // Never leave the control looking merely empty — an empty dropdown is
        // indistinguishable from "no such users", which is what made this
        // confusing in the first place.
        sel.innerHTML = '<option value="">Could not load users</option>';
      }
    }

    async function loadAccessList() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access');
        var data = await resp.json();
        var users = data.access || [];
        if (!users.length) {
          document.getElementById('access-list').innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No users have access yet</div>';
          return;
        }
        var ownerCount = users.filter(function(u) { return u.role === 'owner'; }).length;
        var rows = users.map(function(u) {
          var avatar = '<img src="https://github.com/' + esc(u.username) + '.png" style="width:20px;height:20px;border-radius:50%;vertical-align:middle;margin-right:6px">';
          // The last owner can be neither removed nor demoted — doing so would
          // orphan the hive with no one able to manage access.
          var isLastOwner = (u.role === 'owner' && ownerCount <= 1);
          var canRemove = !isLastOwner;
          var removeBtn = canRemove ?
            '<button onclick="removeAccess(\'' + esc(u.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Remove</button>' :
            '<span style="font-size:0.6rem;color:var(--muted)">last owner</span>';
          // The role pill is an editable dropdown: changing it POSTs the new role
          // (the add endpoint upserts). The last owner's role is locked (shown as
          // a static pill) so the hive can't be left without an owner.
          var ROLES = ['read', 'read-write', 'owner'];
          var roleControl = isLastOwner ?
            '<span class="role-badge role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem" title="The last owner\'s role cannot be changed">' + esc(u.role) + '</span>' :
            '<select class="role-select role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem;padding:2px 6px;border-radius:9999px;cursor:pointer" title="Change this user\'s permission" onchange="changeAccessRole(\'' + esc(u.username) + '\', this.value, \'' + esc(u.role) + '\')">' +
              ROLES.map(function(r) { return '<option value="' + r + '"' + (r === u.role ? ' selected' : '') + '>' + r + '</option>'; }).join('') +
            '</select>';
          return '<div style="display:flex;align-items:center;justify-content:space-between;padding:8px 0;border-bottom:1px solid var(--border)">' +
            '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(u.username) + '</span></div>' +
            '<div style="display:flex;align-items:center;gap:8px">' +
            roleControl +
            removeBtn +
            '</div></div>';
        }).join('');
        document.getElementById('access-list').innerHTML = rows;
      } catch(e) {
        document.getElementById('access-list').innerHTML = '<div style="color:var(--red)">Failed to load</div>';
      }
    }

    async function addAccess() {
      var username = document.getElementById('access-username').value;
      var role = document.getElementById('access-role').value;
      if (!username) { hiveToast('Select a user', 'error'); return; }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: role})
        });
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Failed', 'error'); return; }
        document.getElementById('access-username').value = '';
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function changeAccessRole(username, newRole, oldRole) {
      if (newRole === oldRole) return;
      // Granting owner is significant — confirm it.
      if (newRole === 'owner' && !await hiveConfirm('Make ' + username + ' an owner? Owners can manage access and delete the hive.')) {
        loadAccessList();
        return;
      }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: newRole})
        });
        if (!resp.ok) { var d = await resp.json().catch(function(){return {};}); hiveToast(d.error || 'Failed to change role', 'error'); loadAccessList(); return; }
        hiveToast(username + ' is now ' + newRole, 'success');
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadAccessList(); }
    }

    async function removeAccess(username) {
      if (!await hiveConfirm('Remove access for ' + username + '?')) return;
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access/' + encodeURIComponent(username), {method: 'DELETE'});
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* Banner text is NEUTRAL (matches the spoke's banner-contrast rule);
       the color choice shows in the tint and border only. */
    var _bannerColorStyles = {
      green: {bg: 'rgba(22,163,74,0.12)', border: '1px solid rgba(22,163,74,0.3)', color: 'var(--text)'},
      blue:  {bg: 'rgba(59,130,246,0.12)', border: '1px solid rgba(59,130,246,0.3)', color: 'var(--text)'},
      amber: {bg: 'rgba(245,158,11,0.12)', border: '1px solid rgba(245,158,11,0.3)', color: 'var(--text)'},
      gray:  {bg: 'rgba(107,114,128,0.12)', border: '1px solid rgba(107,114,128,0.3)', color: 'var(--text)'}
    };
    /* Set by the per-hive "Send Banner" menu item so the banner modal targets a
       single spoke; sendHubBanner() still reads .banner-hive-cb:checked, so this
       is only bookkeeping for the open path — the checked cb is the source of truth. */
    var _bannerTargetHive = null;

    (function() {
      var msgEl = document.getElementById('banner-message');
      if (msgEl) {
        msgEl.addEventListener('input', function() {
          document.getElementById('banner-char-count').textContent = this.value.length;
          updateBannerPreview();
        });
      }
      var radios = document.querySelectorAll('input[name="banner-color"]');
      radios.forEach(function(r) { r.addEventListener('change', updateBannerPreview); });
    })();

    // Same safe minimal-markdown as the spoke's renderHubBanner: escape first,
    // then re-introduce only **bold** and [text](http(s)://url) links. Keep this
    // in sync with bannerMarkdown() in the spoke dashboard.
    function bannerMarkdown(text, linkColor) {
      var out = String(text || '')
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      out = out.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)"']+)\)/g, function(_, label, url) {
        return '<a href="' + url + '" target="_blank" rel="noopener noreferrer" style="color:'
          + (linkColor || 'inherit') + ';text-decoration:underline">' + label + '</a>';
      });
      return out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    }

    function updateBannerPreview() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var s = _bannerColorStyles[color];
      var preview = document.getElementById('banner-preview');
      preview.style.background = s.bg;
      preview.style.border = s.border;
      preview.style.color = s.color;
      preview.innerHTML = msg ? ('📢 ' + bannerMarkdown(msg, s.color)) : '<em style="opacity:0.6">Type a message above to preview...</em>';
    }

    // Toolbar helpers: wrap the current selection (or insert a template) at the
    // cursor, then refresh the char count + preview.
    function bannerInsert(before, after, placeholder) {
      var el = document.getElementById('banner-message');
      var start = el.selectionStart, end = el.selectionEnd;
      var sel = el.value.slice(start, end) || placeholder;
      el.value = el.value.slice(0, start) + before + sel + after + el.value.slice(end);
      // Re-select the inserted content so the user can type over the placeholder.
      el.focus();
      el.selectionStart = start + before.length;
      el.selectionEnd = start + before.length + sel.length;
      document.getElementById('banner-char-count').textContent = el.value.length;
      updateBannerPreview();
    }
    function bannerFmtBold() { bannerInsert('**', '**', 'bold text'); }
    function bannerFmtLink() { bannerInsert('[', '](https://)', 'link text'); }

    function loadBannerHiveList() {
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '';
      var hives = _hiveRegistry || [];
      if (!hives.length) {
        container.innerHTML = '<div style="padding:12px;color:var(--muted);font-size:0.8rem;text-align:center">No hives found</div>';
        return;
      }
      hives.forEach(function(h) {
        var label = h.name || h.id;
        var div = document.createElement('div');
        div.style.cssText = 'display:flex;align-items:center;gap:8px;padding:6px 10px;border-bottom:1px solid var(--border)';
        div.innerHTML = '<label style="display:flex;align-items:center;gap:8px;cursor:pointer;flex:1;font-size:0.82rem;color:var(--text)">' +
          '<input type="checkbox" class="banner-hive-cb" value="' + esc(h.id) + '" checked style="accent-color:var(--accent)"> ' + esc(label) +
          '</label>';
        container.appendChild(div);
      });
    }

    function toggleAllBannerHives(checked) {
      document.querySelectorAll('.banner-hive-cb').forEach(function(cb) { cb.checked = checked; });
    }

    /* Per-hive entry point: opens the SAME banner modal but pre-scoped to one
       spoke. Instead of loadBannerHiveList()'s multi-hive checklist, we render a
       single non-editable target line plus one hidden checked .banner-hive-cb so
       the unchanged sendHubBanner() (which reads .banner-hive-cb:checked) posts
       exactly this hive_id to POST /api/saas/admin/hub-banner. */
    function openBannerForHive(hiveId, hiveName) {
      _bannerTargetHive = hiveId;
      document.getElementById('banner-modal').style.display = 'flex';
      /* Reset message + color to match the global open path's fresh state. */
      document.getElementById('banner-message').value = '';
      document.getElementById('banner-char-count').textContent = '0';
      document.querySelector('input[name="banner-color"][value="green"]').checked = true;
      updateBannerPreview();
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '<div style="display:flex;align-items:center;gap:8px;padding:10px;color:var(--text);font-size:0.82rem">' +
        '<span style="color:var(--muted)">Sending to:</span> <strong>' + esc(hiveName) + '</strong>' +
        '<input type="checkbox" class="banner-hive-cb" value="' + esc(hiveId) + '" checked style="display:none">' +
        '</div>';
    }

    async function sendHubBanner() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      if (!msg) { hiveToast('Message is required', 'error'); return; }
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var hiveIDs = [];
      document.querySelectorAll('.banner-hive-cb:checked').forEach(function(cb) { hiveIDs.push(cb.value); });
      if (!hiveIDs.length) { hiveToast('Select at least one hive', 'error'); return; }
      var btn = document.getElementById('btn-send-banner');
      btn.disabled = true;
      btn.textContent = 'Sending...';
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({message: msg, color: color, hive_ids: hiveIDs})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to send', 'error'); return; }
        hiveToast('Banner sent to ' + data.hive_count + ' hive(s)', 'success');
        document.getElementById('banner-modal').style.display = 'none';
        document.getElementById('banner-message').value = '';
        document.getElementById('banner-char-count').textContent = '0';
        document.querySelector('input[name="banner-color"][value="green"]').checked = true;
        updateBannerPreview();
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { btn.disabled = false; btn.textContent = 'Send Banner'; }
    }

    async function loadActiveBanner() {
      try {
        var resp = await fetch('/api/saas/admin/hub-banner');
        if (!resp.ok) return;
        var data = await resp.json();
        var banners = data.banners || [];
        var display = document.getElementById('active-banner-display');
        var clearBtn = document.getElementById('btn-clear-banner');
        if (!banners.length) {
          display.style.display = 'none';
          clearBtn.style.display = 'none';
          return;
        }
        var first = banners[0];
        var s = _bannerColorStyles[first.color] || _bannerColorStyles.green;
        var preview = document.getElementById('active-banner-preview');
        preview.style.background = s.bg;
        preview.style.border = s.border;
        preview.style.color = s.color;
        preview.textContent = first.message;
        var targets = document.getElementById('active-banner-targets');
        var hiveNames = banners.map(function(b) { return b.hive_id; });
        targets.textContent = 'Sent to ' + banners.length + ' hive(s): ' + hiveNames.join(', ');
        display.style.display = '';
        clearBtn.style.display = '';
      } catch(e) { /* ignore */ }
    }

    async function clearHubBanner() {
      if (!await hiveConfirm('Clear all active hub banners?')) return;
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {method: 'DELETE'});
        if (!resp.ok) { hiveToast('Failed to clear', 'error'); return; }
        hiveToast('All banners cleared', 'success');
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
  
package hub

const dashboardHTMLViewScripts = `    function onSavedViewPick(name) {
      if (!name) { _dashActiveView = ''; renderHives(_allDashHives, true); return; }
      applySavedView(name);
    }

    /* renameSavedView renames a view in place, keeping the default-view pointer
       in sync so a renamed default stays the default. */
    async function renameSavedView() {
      if (!_dashActiveView) { hiveToast('Select a view to rename', 'error'); return; }
      var v = findSavedView(_dashActiveView);
      if (!v) return;
      var raw = await hivePrompt('Rename view', v.name);
      if (raw === null) return;
      var name = String(raw).trim().slice(0, HIVE_VIEW_NAME_MAX_LEN);
      if (!name) { hiveToast('View name cannot be empty', 'error'); return; }
      if (name !== v.name && findSavedView(name)) { hiveToast('A view named "' + name + '" already exists', 'error'); return; }
      var wasDefault = _dashDefaultView === v.name;
      v.name = name;
      _dashActiveView = name;
      persistSavedViews();
      if (wasDefault) {
        _dashDefaultView = name;
        try { localStorage.setItem(LS_HIVE_DEFAULT_VIEW, name); } catch (e) { /* storage full or disabled */ }
      }
      renderHives(_allDashHives, true);
    }

    /* deleteSavedView removes the selected view, clearing the default pointer
       if it pointed at the deleted view. */
    async function deleteSavedView() {
      if (!_dashActiveView) { hiveToast('Select a view to delete', 'error'); return; }
      if (!await hiveConfirm('Delete view "' + _dashActiveView + '"?')) return;
      var kept = [];
      for (var i = 0; i < (_dashSavedViews || []).length; i++) {
        if (_dashSavedViews[i].name !== _dashActiveView) kept.push(_dashSavedViews[i]);
      }
      _dashSavedViews = kept;
      if (_dashDefaultView === _dashActiveView) {
        _dashDefaultView = '';
        try { localStorage.removeItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { /* storage disabled */ }
      }
      _dashActiveView = '';
      persistSavedViews();
      renderHives(_allDashHives, true);
    }

    /* toggleDefaultView marks/unmarks the selected view as the one applied on
       load. */
    function toggleDefaultView() {
      if (!_dashActiveView) { hiveToast('Select a view first', 'error'); return; }
      if (_dashDefaultView === _dashActiveView) {
        _dashDefaultView = '';
        try { localStorage.removeItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { /* storage disabled */ }
        hiveToast('Default view cleared', 'success');
      } else {
        _dashDefaultView = _dashActiveView;
        try { localStorage.setItem(LS_HIVE_DEFAULT_VIEW, _dashDefaultView); } catch (e) { /* storage full or disabled */ }
        hiveToast('"' + _dashDefaultView + '" is now the default view', 'success');
      }
      renderHives(_allDashHives, true);
    }

    /* hiveMatchesFilters answers whether a hive survives the active chips. */
    function hiveMatchesFilters(h) {
      if (_dashFailingCheckFilter) {
        var hit = failingChecks(h).some(function(ck) { return ck.name === _dashFailingCheckFilter; });
        if (!hit) return false;
      }
      if (!hiveMatchesDriftFilter(h)) return false;
      if (!hiveMatchesUpgradeFilter(h)) return false;
      if (!hiveMatchesAdvisoryStaleFilter(h)) return false;
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

    /* ── Fleet alerts ("Attention needed") ────────────────────────────────
       Server-evaluated (see alerts.go); the client only renders and filters.
       Deliberately NOT recomputed here: a second, drifting implementation of
       "what is wrong" is exactly how the panel and the rows start disagreeing. */

    /* EMPTY_ALERT_SUMMARY is the shape every consumer can assume. Frozen so a
       caller cannot accidentally mutate the shared fallback. */
    var EMPTY_ALERT_SUMMARY = {alerts: [], countsBySeverity: {}, countsByType: {}, total: 0, acknowledgedTotal: 0};

    var _fleetAlerts = EMPTY_ALERT_SUMMARY;
    /* Server-computed fleet inventory counts (hives_summary on the my-hives
       payload). Null until the first load; renderSummaryTiles guards. */
    var _hivesSummary = null;
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
      'provision-error': 'Provision error',
      /* Added by the hub in #2308. Without a label here the chip for a
         genuinely failed upgrade would render as the raw key 'failed-upgrade'. */
      'failed-upgrade': 'Failed upgrade',
      'advisory-stale': 'Stale advisory',
      /* Agents that are up but doing nothing — a session that has gone, a
         login prompt, or no output while work is queued. Deliberately paused
         agents are excluded server-side and never counted here. */
      'agents-inactive': 'Idle agents',
      /* Raised by the auth-audit loop since #2306 but never labelled, so its
         chip rendered the raw key. */
      'url-unreachable': 'Dashboard URL unreachable',
      'url-private-network': 'Private dashboard URL',
      /* The hive's inference gateway is rejecting every call with 401 (a stale
         key) — the ROOT cause of an otherwise silent outage where the hive
         looks online but every agent is dead in the water. */
      'inference-auth-failed': 'Inference auth failing',
      /* GitHub App credentials are in an operator-side state (key-missing /
         key-invalid / no-app-assigned). The owner cannot fix these — the App
         key is hub-distributed — so this alert is the operator's only active
         signal (#4316). The reason carries the PUT remedy. */
      'app-creds-undelivered': 'App credentials undelivered'
    };

    /* How many alert rows are listed before the panel collapses the remainder
       behind the "+N more" toggle. Enough to act on, few enough that the panel
       never pushes the hive list off the screen. */
    var ALERT_ROWS_SHOWN_MIN = 6;

    /* Which halves of the panel are expanded past ALERT_ROWS_SHOWN_MIN. Keyed
       'live'/'acked' so the two lists expand independently. Not persisted: an
       expanded list is a momentary "show me the rest", not a view preference,
       and a panel that reloads 50 rows deep would bury the hive table. */
    var _alertRowsExpanded = {live: false, acked: false};

    /* toggleAlertRowsExpanded flips one half of the panel open or closed. */
    function toggleAlertRowsExpanded(which) {
      var key = (which === 'acked') ? 'acked' : 'live';
      _alertRowsExpanded[key] = !_alertRowsExpanded[key];
      renderHives(_allDashHives, true);
    }

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

    /* clearAllHiveFilters resets EVERY narrowing control at once — status chips,
       the failing-check and drift filters, the alert-type filter, the search box
       and the facets. The empty-state escape hatch uses it, because any one of
       them alone can empty the list and the user usually cannot tell which is
       responsible. */
    function clearAllHiveFilters() {
      _dashStatusFilters = {};
      _dashFailingCheckFilter = '';
      _dashDriftFilter = '';
      /* Everything is being cleared, search included — a search stashed at
         drift-pill-selection time must not come back later. */
      _driftSearchStash = null;
      _dashUpgradeFilter = '';
      _dashAdvisoryStaleFilter = false;
      _alertTypeFilter = '';
      _dashFacets = {};
      _dashSearchQuery = '';
      var searchEl = document.getElementById('hive-search');
      if (searchEl) searchEl.value = '';
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

    /* ── Jumping from an alert row to the hive's row in the table ──

       hiveRowDomId builds the anchor id renderHives puts on each <tr>. Prefixed
       so it can never collide with another element id, and encodeURIComponent'd
       because a hive id is user-influenced and may contain characters that are
       not valid raw in an id/selector. */
    var HIVE_ROW_DOM_ID_PREFIX = 'hive-row-';
    function hiveRowDomId(hiveId) {
      return HIVE_ROW_DOM_ID_PREFIX + encodeURIComponent(String(hiveId || ''));
    }

    /* How long the targeted row stays highlighted. Long enough to find the row
       after the smooth scroll finishes, short enough that a stale highlight does
       not linger and get mistaken for a status. */
    var HIVE_ROW_TARGET_HIGHLIGHT_MS = 2600;

    /* Handle of the pending highlight-clear timer, so two jumps in quick
       succession do not leave the first row highlighted forever. */
    var _hiveRowTargetTimer = null;

    /* focusHiveRow scrolls one hive's table row into view and highlights it.
       Returns false when the row is not currently in the DOM. */
    function focusHiveRow(hiveId) {
      var row = document.getElementById(hiveRowDomId(hiveId));
      if (!row) return false;
      /* Clear any previous target first — only ever one highlighted row. */
      if (_hiveRowTargetTimer) { clearTimeout(_hiveRowTargetTimer); _hiveRowTargetTimer = null; }
      var prev = document.querySelectorAll('.hive-row-targeted');
      (prev || []).forEach(function(el) { el.classList.remove('hive-row-targeted'); });

      row.scrollIntoView({behavior: 'smooth', block: 'center'});
      row.classList.add('hive-row-targeted');
      /* Move keyboard focus to the row as well, so a keyboard user lands there
         rather than being scrolled somewhere their focus ring is not. */
      if (!row.hasAttribute('tabindex')) row.setAttribute('tabindex', '-1');
      try { row.focus({preventScroll: true}); } catch (e) { /* focus is best-effort */ }

      _hiveRowTargetTimer = setTimeout(function() {
        row.classList.remove('hive-row-targeted');
        _hiveRowTargetTimer = null;
      }, HIVE_ROW_TARGET_HIGHLIGHT_MS);
      return true;
    }

    /* jumpToHiveRow is what an "Attention needed" row does when clicked.

       The hard case is a target the current view is HIDING. The hive list now
       carries persisted filters, facets, a search box, an alert drill-down and
       collapsible sections/groups (#2309, #2317), so the alerted hive may well
       not be on screen. Scrolling to a row that is not in the DOM would look
       like the click did nothing.

       The choice made here: try the current view FIRST, and only if the row is
       genuinely absent do we widen the view — clearing every narrowing control
       and expanding the collapsed sections/groups. Filters are the user's state,
       so they are never discarded silently: we clear them only when that is the
       one thing that can satisfy the click, and we say so in a toast that names
       the hive. The alternative (refusing to navigate and just warning) leaves
       the operator to work out which of six controls is hiding the row, which is
       the problem the panel exists to avoid. */
    function jumpToHiveRow(hiveId, hiveName) {
      if (!hiveId) return;
      if (focusHiveRow(hiveId)) return;

      /* Not rendered under the current view. Widen everything that can hide a
         row, re-render synchronously, then try again. */
      clearAllHiveFilters();
      expandAllHiveSections();
      _dashGroupCollapsed = {};
      persistGroupCollapsed();
      renderHives(_allDashHives, true);

      if (focusHiveRow(hiveId)) {
        hiveToast('Filters cleared to show ' + (hiveName || hiveId), 'info');
        return;
      }
      /* Still absent: the hive is genuinely not in the loaded list (for example
         it was deleted between the alert being evaluated and now). Say so rather
         than leaving a click that silently did nothing. */
      hiveToast('Could not find ' + (hiveName || hiveId) + ' in the hive list', 'error');
    }

    /* SUMMARY_TILES_MIN_HIVES gates the fleet tiles strip: below this many
       visible hives the tiles restate what the eye already sees in the table,
       so they stay hidden and the page keeps its small-user simplicity. */
    var SUMMARY_TILES_MIN_HIVES = 8;

    /* renderSummaryTiles draws the fleet inventory strip above the alerts
       panel from the server-computed hives_summary — always the caller's FULL
       visible set, never the active filter, so the numbers stay truthful
       during any drill-down. Zero-count exception tiles self-suppress. */
    function renderSummaryTiles() {
      var el = document.getElementById('fleet-summary-tiles');
      if (!el) return;
      var s = _hivesSummary;
      if (!s || (Number(s.total) || 0) < SUMMARY_TILES_MIN_HIVES) {
        el.style.display = 'none';
        return;
      }
      /* [key, label, css-class-when-nonzero, always-show] */
      var defs = [
        ['total', 'Hives', '', true],
        ['online', 'Online', '', true],
        ['offline', 'Offline', 'warn', true],
        ['pool_available', 'Pool', '', false],
        ['assigned_unclaimed', 'Unclaimed', 'warn', false],
        ['provisioning', 'Provisioning', '', false],
        ['upgrading', 'Upgrading', '', false],
        ['upgrade_failed', 'Upgrade failed', 'bad', false],
        ['errors', 'Errors', 'bad', false]
      ];
      var tiles = '';
      for (var i = 0; i < defs.length; i++) {
        var n = Number(s[defs[i][0]]) || 0;
        if (!n && !defs[i][3]) continue;
        var cls = n && defs[i][2] ? ' ' + defs[i][2] : '';
        tiles += '<div class="fleet-tile' + cls + '"><div class="fleet-tile-n">' + n +
          '</div><div class="fleet-tile-label">' + esc(defs[i][1]) + '</div></div>';
      }
      el.innerHTML = '<div class="fleet-tiles">' + tiles + '</div>';
      el.style.display = '';
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
      /* Each half of the panel (live vs acknowledged) expands independently, so
         expanding the acknowledged list does not also dump every live alert. */
      var expanded = !!_alertRowsExpanded[acked ? 'acked' : 'live'];
      var limit = expanded ? rows.length : ALERT_ROWS_SHOWN_MIN;
      var shown = rows.slice(0, limit);
      var html = shown.map(function(a) {
        var color = ALERT_SEVERITY_COLORS[a.severity] || '#8b949e';
        var age = alertAge(a.firstSeen);
        /* Every interpolated value below is spoke- or admin-supplied and is
           escaped: hive names, reasons (which embed check names and provision
           error text), and the acking username. */
        var ackBtn = '';
        if (_isAdmin) {
          /* jsArg for the same reason as the jump handler below: esc() leaves an
             apostrophe intact, which would close the handler's quote early. */
          ackBtn = acked
            ? '<button type="button" class="alert-ack-btn" onclick="ackAlert(' +
              jsArg(a.hiveId) + ',' + jsArg(a.type) + ',true)">Restore</button>'
            : '<button type="button" class="alert-ack-btn" onclick="ackAlert(' +
              jsArg(a.hiveId) + ',' + jsArg(a.type) + ',false)">Acknowledge</button>';
        }
        var ackedBy = (acked && a.ackBy) ? '<span class="alert-row-reason">— ack by ' + esc(a.ackByName || a.ackBy) + '</span>' : '';
        /* The row itself is a <button> that jumps to this hive's row in the
           table below. A real button (not a div with role/tabindex) gets Enter,
           Space and the focus ring for free.

           The Acknowledge button is a SIBLING, not a child: nesting a button
           inside a button is invalid HTML and browsers drop the inner one, which
           would silently break acknowledging. Both sit in a flex wrapper so the
           row still reads as one line. */
        var label = a.hiveName || a.hiveId;
        var jump = '<button type="button" class="alert-row' + (acked ? ' acked' : '') +
          '" title="Show ' + escAttr(label) + ' in the hive list below"' +
          /* jsArg, not esc: a hive or org name containing an apostrophe would
             close the handler's quote early and break the click. */
          ' onclick="jumpToHiveRow(' + jsArg(a.hiveId) + ',' + jsArg(label) + ')">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          '<span class="alert-row-hive">' + esc(label) + '</span>' +
          '<span class="alert-row-reason">' + esc(a.reason) + '</span>' + ackedBy +
          '<span class="alert-row-age">' + esc(age) + '</span>' +
        '</button>';
        return '<div class="alert-row-wrap">' + jump + ackBtn + '</div>';
      }).join('');
      /* "+N more" used to be inert text, which was actively misleading once the
         rows above it became clickable: it looked like the same kind of thing
         and did nothing. It is now a real toggle that expands the full list in
         place (and collapses it again), so every alerted hive is reachable —
         which is the point, since the hive the operator needs is as likely to be
         #7 as #1. */
      var more = '';
      if (rows.length > limit) {
        more = '<button type="button" class="alert-panel-more" onclick="toggleAlertRowsExpanded(' +
          jsArg(acked ? 'acked' : 'live') + ')">+' + (rows.length - limit) +
          ' more</button>';
      } else if (expanded && rows.length > ALERT_ROWS_SHOWN_MIN) {
        more = '<button type="button" class="alert-panel-more" onclick="toggleAlertRowsExpanded(' +
          jsArg(acked ? 'acked' : 'live') + ')">Show fewer</button>';
      }
      return '<div class="alert-rows">' + html + more + '</div>';
    }

    /* ── Free-text search over the hive list ──
       OR semantics: whitespace-separated terms, a hive matches if ANY term is a
       case-insensitive substring of its searchable text. Like the status chips,
       search only narrows ASSIGNED hives — the unassigned placeholder pool is
       inventory, not a search result, and making it vanish as an admin types
       would hide the very slots they are about to assign. See _dashSearchQuery
       and hiveMatchesSearch below. */

    /* ── Collapsible admin sections ──
       Keys follow the 'hive-*' localStorage convention already used by
       hive-dismissed-banners / hive-cluster-health-collapsed. */
    var HIVE_SECTION_ASSIGNED = 'assigned';
    var HIVE_SECTION_UNASSIGNED = 'unassigned';
    var LS_SECTION_ASSIGNED_COLLAPSED = 'hive-section-assigned-collapsed';
    var LS_SECTION_UNASSIGNED_COLLAPSED = 'hive-section-unassigned-collapsed';

    /* Defaults: Assigned EXPANDED (the hives you actually operate), Unassigned
       COLLAPSED (a pool that can run to dozens of identical slots). Stored as
       the string 'true'/'false'; anything else falls back to the default, so a
       first visit and a corrupted value behave the same. */
    var _dashSectionCollapsed = {};
    _dashSectionCollapsed[HIVE_SECTION_ASSIGNED] =
      localStorage.getItem(LS_SECTION_ASSIGNED_COLLAPSED) === 'true';
    _dashSectionCollapsed[HIVE_SECTION_UNASSIGNED] =
      localStorage.getItem(LS_SECTION_UNASSIGNED_COLLAPSED) !== 'false';

    /* expandAllHiveSections opens both sections and PERSISTS that, matching
       toggleHiveSection. An in-memory-only reset would silently re-collapse on
       the next reload, so the row the operator was just sent to would vanish
       again — the collapsed Unassigned pool is the default, so this matters for
       any alert on an unassigned placeholder. */
    function expandAllHiveSections() {
      _dashSectionCollapsed[HIVE_SECTION_ASSIGNED] = false;
      _dashSectionCollapsed[HIVE_SECTION_UNASSIGNED] = false;
      try {
        localStorage.setItem(LS_SECTION_ASSIGNED_COLLAPSED, 'false');
        localStorage.setItem(LS_SECTION_UNASSIGNED_COLLAPSED, 'false');
      } catch(e) {} /* private-browsing quota — expansion still works in-session */
    }

    function toggleHiveSection(sectionKey) {
      _dashSectionCollapsed[sectionKey] = !_dashSectionCollapsed[sectionKey];
      var lsKey = sectionKey === HIVE_SECTION_ASSIGNED
        ? LS_SECTION_ASSIGNED_COLLAPSED : LS_SECTION_UNASSIGNED_COLLAPSED;
      try {
        localStorage.setItem(lsKey, _dashSectionCollapsed[sectionKey] ? 'true' : 'false');
      } catch(e) {} /* private-browsing quota — collapse still works in-session */
      renderHives(_allDashHives, true);
    }

    var _dashSearchQuery = '';
    /* Debounce keystrokes so a large hive list is not re-rendered per character.
       120ms is below the ~200ms threshold where typing starts to feel laggy. */
    var HIVE_SEARCH_DEBOUNCE_MS = 120;
    var _hiveSearchTimer = null;

    /* hiveSearchText flattens a hive's user-visible metadata into one lowercase
       haystack. Includes the access list usernames so an admin can find "which
       hive did I grant octocat?" — access is only populated on owned rows. */
    function hiveSearchText(h) {
      if (!h) return '';
      var parts = [
        h.id, h.name, h.org, h.primaryRepo, h.clusterId, h.clusterName,
        h.role, h.gitBranch, h.gitHash, h.dashboardUrl,
        /* The tracked release channel is what the version pill shows for a
           channel-pinned hive, so "stable" has to find those rows even though
           their gitBranch reports the underlying release branch. */
        h.trackedChannel,
        /* The GitHub host is shown as a pill in the Location column, so it has
           to be searchable too — "github.ibm.com" is how an admin narrows to
           the GHE fleet. Absent means public GitHub, which is what the pill
           renders, so search on that same default rather than nothing. */
        h.githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST
      ];
      (h.repos || []).forEach(function(r) { parts.push(r); });
      (h.access || []).forEach(function(a) {
        if (!a) return;
        parts.push(a.username);
        parts.push(a.role);
      });
      return parts.filter(function(p) { return p; }).join(' ').toLowerCase();
    }

    /* hiveMatchesSearch answers whether a hive survives the search box. */
    function hiveMatchesSearch(h) {
      var terms = (_dashSearchQuery || '').toLowerCase().split(/\s+/).filter(function(t) { return t; });
      if (!terms.length) return true;
      var hay = hiveSearchText(h);
      for (var i = 0; i < terms.length; i++) {
        if (hay.indexOf(terms[i]) !== -1) return true; /* OR */
      }
      return false;
    }

    /* onHiveSearchInput debounces then re-renders from the unfiltered cache so
       search composes with the chips, the facets and the current sort. */
    function onHiveSearchInput() {
      var el = document.getElementById('hive-search');
      var next = el ? el.value : '';
      if (_hiveSearchTimer) clearTimeout(_hiveSearchTimer);
      /* Typing while a drift pill is active supersedes the search stashed at
         pill-selection time: drop the stash immediately (not in the debounce)
         so a quick type-then-deselect cannot resurrect the old text. */
      if (_dashDriftFilter) _driftSearchStash = null;
      _hiveSearchTimer = setTimeout(function() {
        _dashSearchQuery = next;
        renderHives(_allDashHives, true);
      }, HIVE_SEARCH_DEBOUNCE_MS);
    }

    function clearHiveSearch() {
      var el = document.getElementById('hive-search');
      if (el) el.value = '';
      _dashSearchQuery = '';
      /* An explicit Clear is a statement the user wants NO search — do not
         resurrect a stashed one when the drift pills are later deselected. */
      _driftSearchStash = null;
      renderHives(_allDashHives, true);
    }

    /* ── Faceted search ──
       Facet groups are derived from the assigned hives themselves, so a group
       only appears when the data has something to offer. Within one group the
       selected values are OR'd (pick two clusters => hives in either); across
       groups they are AND'd (that cluster AND that role), which is the standard
       faceted-search contract users expect from e-commerce-style filters. */
    var FACET_CLUSTER = 'cluster';
    var FACET_ACMM = 'acmm';
    var FACET_ROLE = 'role';
    var FACET_STATUS = 'status';
    var FACET_BRANCH = 'branch';
    /* GitHub instance the hive targets. Worth its own facet because the
       GHE-vs-public split is a real operational boundary — a github.ibm.com
       hive can only be reached from a cluster that can route to it. */
    var FACET_GITHUB_HOST = 'github-host';

    /* Value shown when a hive has no value for a facet, so those rows stay
       reachable instead of silently dropping out of every facet count. */
    var FACET_UNKNOWN = '—';

    /* Group keys for the two filters moved into the tray from the old chip bar.
       They are NOT in HIVE_FACET_GROUPS — those are derived from hive fields via
       hiveFacetValues, whereas these keep their own pre-existing state
       (_dashStatusFilters / _dashFailingCheckFilter) and matching semantics.
       They share _dashFacetCollapsed only for the collapse chrome. */
    var FACET_GROUP_HEALTH = 'health';
    var FACET_GROUP_FAILING_CHECK = 'failing-check';
    /* Upgrade-state group. Follows the HEALTH/FAILING_CHECK pattern rather than
       the derived-facet one: the values are a fixed, hand-written lifecycle
       ('upgrading', 'queued') with their own matching rule and their own state
       (_dashUpgradeFilter), not a dimension enumerated from the data like
       cluster or branch, so it stays OUT of HIVE_FACET_GROUPS. */
    var FACET_GROUP_UPGRADE = 'upgrade-state';
    /* Stale-advisory group. Follows the HEALTH/FAILING_CHECK pattern rather
       than the derived-facet one: "is this hive's advisory digest stale" is a
       boolean health signal read off a server-computed flag, not an enumerated
       dimension of the hive like cluster or branch, so it keeps its own state
       (_dashAdvisoryStaleFilter) and its own matching rule and stays OUT of
       HIVE_FACET_GROUPS. */
    var FACET_GROUP_ADVISORY_STALE = 'advisory-stale';

    var HIVE_FACET_GROUPS = [
      {key: FACET_CLUSTER, label: 'Location'},
      {key: FACET_ACMM, label: 'ACMM level'},
      {key: FACET_ROLE, label: 'Your role'},
      {key: FACET_STATUS, label: 'Status'},
      {key: FACET_BRANCH, label: 'Branch'},
      {key: FACET_GITHUB_HOST, label: 'GitHub'}
    ];

    /* Selected facet values: {facetKey: {value: true}}. */
    var _dashFacets = {};
    /* Collapsed facet GROUPS (chrome only, not a filter): {facetKey: true}. */
    var _dashFacetCollapsed = {};

    /* ── Facet tray open/closed ──
       Click-toggled, persisted, and COLLAPSED BY DEFAULT so the dashboard is
       uncluttered until filters are asked for. Key follows the same 'hive-*'
       localStorage convention as hive-section-assigned-collapsed and
       hive-cluster-health-collapsed. Open only when explicitly stored 'true',
       so a first visit and a corrupted value both fall back to closed. */
    var LS_FACET_TRAY_OPEN = 'hive-facet-tray-open';
    var _dashFacetTrayOpen = false;
    try {
      _dashFacetTrayOpen = localStorage.getItem(LS_FACET_TRAY_OPEN) === 'true';
    } catch (e) { _dashFacetTrayOpen = false; } /* storage disabled */

    /* applyFacetTrayState paints the DOM from _dashFacetTrayOpen. Split out so
       the initial paint and every toggle go through exactly one code path and
       can never disagree about aria-expanded. */
    function applyFacetTrayState() {
      var shell = document.getElementById('hive-facet-shell');
      var toggle = document.getElementById('hive-facet-toggle');
      if (shell) shell.classList.toggle('facet-open', _dashFacetTrayOpen);
      if (toggle) {
        toggle.setAttribute('aria-expanded', _dashFacetTrayOpen ? 'true' : 'false');
        toggle.title = _dashFacetTrayOpen ? 'Hide filters' : 'Show filters';
      }
    }

    function setFacetTrayOpen(open) {
      _dashFacetTrayOpen = !!open;
      try {
        localStorage.setItem(LS_FACET_TRAY_OPEN, _dashFacetTrayOpen ? 'true' : 'false');
      } catch (e) {} /* private-browsing quota — the toggle still works in-session */
      applyFacetTrayState();
    }

    function toggleFacetTray() { setFacetTrayOpen(!_dashFacetTrayOpen); }

    /* activeFilterCount is what the collapsed rail's badge shows. EVERY control
       that can narrow the list is counted, because the badge is the only thing
       telling the operator why the table is short while the tray is shut. Each
       selected facet value counts individually — "3" must mean three choices,
       not three groups. */
    function activeFilterCount() {
      var n = 0;
      n += Object.keys(_dashStatusFilters || {}).length;
      if (_dashFailingCheckFilter) n++;
      if (_dashDriftFilter) n++;
      if (_dashUpgradeFilter) n++;
      if (_dashAdvisoryStaleFilter) n++;
      if (_alertTypeFilter) n++;
      if (_dashSearchQuery) n++;
      var groups = Object.keys(_dashFacets || {});
      for (var i = 0; i < groups.length; i++) {
        n += Object.keys(_dashFacets[groups[i]] || {}).length;
      }
      return n;
    }

    /* renderFacetTrayAffordance keeps the collapsed rail honest: accent border
       plus a count badge whenever anything is filtering. */
    function renderFacetTrayAffordance() {
      var toggle = document.getElementById('hive-facet-toggle');
      var badge = document.getElementById('hive-facet-active-badge');
      var n = activeFilterCount();
      if (toggle) toggle.classList.toggle('has-active', n > 0);
      if (badge) {
        badge.style.display = n > 0 ? '' : 'none';
        badge.textContent = n > 0 ? String(n) : '';
        badge.title = n === 1 ? '1 active filter' : n + ' active filters';
      }
      if (toggle) {
        var base = _dashFacetTrayOpen ? 'Hide filters' : 'Show filters';
        toggle.title = n > 0 ? base + ' — ' + (n === 1 ? '1 filter active' : n + ' filters active') : base;
        toggle.setAttribute('aria-label', toggle.title);
      }
    }

    /* Bound once at parse time via delegation on document, so the handlers
       survive every re-render of the tray's contents and no inline onclick has
       to interpolate anything. */
    document.addEventListener('click', function(ev) {
      var t = ev.target;
      if (!t || !t.closest) return;
      if (t.closest('#hive-facet-toggle')) { toggleFacetTray(); return; }
      if (t.closest('#hive-facet-close')) { setFacetTrayOpen(false); return; }
    });
    /* Escape closes the tray when focus is inside it, and returns focus to the
       toggle so the keyboard user is not stranded on a hidden control. */
    document.addEventListener('keydown', function(ev) {
      if (ev.key !== 'Escape' || !_dashFacetTrayOpen) return;
      var tray = document.getElementById('hive-facet-tray');
      if (!tray || !tray.contains(document.activeElement)) return;
      setFacetTrayOpen(false);
      var toggle = document.getElementById('hive-facet-toggle');
      if (toggle) toggle.focus();
    });
    /* Paint the persisted state immediately. This script runs after the tray
       markup, so the elements already exist and there is no open-then-snap-shut
       flash on a reload with the tray remembered open. */
    applyFacetTrayState();

    /* hiveFacetValues returns the facet values a single hive belongs to. A hive
       contributes at most one value per group. */
    function hiveFacetValues(h) {
      h = h || {};
      var f = {};
      f[FACET_CLUSTER] = h.clusterName || h.clusterId || 'local';
      f[FACET_ACMM] = (h.acmmLevel != null && h.acmmLevel !== '') ? ('L' + h.acmmLevel) : FACET_UNKNOWN;
      f[FACET_ROLE] = h.role || FACET_UNKNOWN;
      f[FACET_BRANCH] = h.gitBranch || FACET_UNKNOWN;
      /* Absent host means public GitHub — bucket it under the same label the
         pill renders, not FACET_UNKNOWN, so old spokes group with github.com
         rather than forming a meaningless "—" bucket. */
      f[FACET_GITHUB_HOST] = h.githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST;
      var flags = hiveStatusFlags(h);
      f[FACET_STATUS] = flags.degraded ? 'Degraded' : (h.online ? 'Online' : 'Offline');
      return f;
    }

    /* hiveMatchesFacets: OR within a group, AND across groups. */
    function hiveMatchesFacets(h) {
      var vals = hiveFacetValues(h);
      var groups = Object.keys(_dashFacets || {});
      for (var g = 0; g < groups.length; g++) {
        var picked = Object.keys(_dashFacets[groups[g]] || {}).filter(function(v) { return _dashFacets[groups[g]][v]; });
        if (!picked.length) continue;
        if (picked.indexOf(String(vals[groups[g]])) === -1) return false;
      }
      return true;
    }

    function toggleFacetValue(facetKey, value) {
      if (!_dashFacets[facetKey]) _dashFacets[facetKey] = {};
      if (_dashFacets[facetKey][value]) {
        delete _dashFacets[facetKey][value];
        if (!Object.keys(_dashFacets[facetKey]).length) delete _dashFacets[facetKey];
      } else {
        _dashFacets[facetKey][value] = true;
      }
      renderHives(_allDashHives, true);
    }

    function clearHiveFacets() {
      _dashFacets = {};
      renderHives(_allDashHives, true);
    }

    function toggleFacetGroup(facetKey) {
      _dashFacetCollapsed[facetKey] = !_dashFacetCollapsed[facetKey];
      renderHives(_allDashHives, true);
    }

    /* facetGroupShell wraps one collapsible group so the status/failing-check
       groups moved in from the old chip bar are visually and behaviourally
       identical to the derived facet groups. */
    function facetGroupShell(key, label, collapsed, bodyHTML) {
      return '<div class="facet-group">' +
        '<button type="button" class="facet-group-head" aria-expanded="' + (collapsed ? 'false' : 'true') +
        '" onclick="toggleFacetGroup(\'' + esc(key) + '\')">' +
        '<span>' + esc(label) + '</span><span>' + (collapsed ? '▸' : '▾') + '</span></button>' +
        (collapsed ? '' : bodyHTML) + '</div>';
    }

    /* renderStatusFacetGroup renders the four health chips — GitHub App not
       installed / No tokens used / Degraded / OK — as a facet group inside the
       tray, replacing the old standalone chip row. Semantics are UNCHANGED:
       these still OR among themselves via _dashStatusFilters and
       hiveMatchesFilters, exactly like a facet group ORs within itself.
       Counts are over the FULL assigned set (placeholders already excluded by
       the caller), matching what the old bar advertised. */
    function renderStatusFacetGroup(assignedNoPlaceholders) {
      var counts = {};
      counts[HIVE_FILTER_APP_MISSING] = 0;
      counts[HIVE_FILTER_NO_TOKENS] = 0;
      counts[HIVE_FILTER_DEGRADED] = 0;
      counts[HIVE_FILTER_OK] = 0;
      (assignedNoPlaceholders || []).forEach(function(h) {
        var f = hiveStatusFlags(h);
        if (f.appMissing) counts[HIVE_FILTER_APP_MISSING]++;
        if (f.noTokens) counts[HIVE_FILTER_NO_TOKENS]++;
        if (f.degraded) counts[HIVE_FILTER_DEGRADED]++;
        if (f.ok) counts[HIVE_FILTER_OK]++;
      });
      var body = '<div class="facet-values">' + (HIVE_FILTER_CHIPS || []).map(function(c) {
        var on = !!_dashStatusFilters[c.key];
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleStatusFilter(\'' + esc(c.key) + '\')" title="' + esc(c.label) + '">' +
          '<span class="facet-value-label">' + esc(c.label) + '</span>' +
          '<span class="facet-value-count">' + counts[c.key] + '</span></button>';
      }).join('') +
      /* Group-scoped reset for the health + failing-check pair, so a user can
         undo just these without also dropping their search term and facets. */
      (Object.keys(_dashStatusFilters || {}).length || _dashFailingCheckFilter || _dashDriftFilter || _dashUpgradeFilter || _dashAdvisoryStaleFilter
        ? '<button type="button" class="facet-value" onclick="clearStatusFilters()" title="Clear the health, failing-check, drift, upgrade-state and stale-advisory filters">' +
          '<span class="facet-value-label">Clear health filters</span></button>'
        : '') + '</div>';
      return facetGroupShell(FACET_GROUP_HEALTH, 'Health',
        !!_dashFacetCollapsed[FACET_GROUP_HEALTH], body);
    }

    /* renderFailingCheckFacetGroup renders the failing-check picker as a facet
       group. Semantics are UNCHANGED: still SINGLE-select and still ANDed
       against the health chips by hiveMatchesFilters. That is deliberately not
       the OR-within-group rule the derived facets use — "degraded hives" and
       "github_auth is failing" are different questions whose useful answer is
       the intersection — so the group head says so, rather than silently
       looking like the others. Returns '' on a healthy fleet. */
    function renderFailingCheckFacetGroup(assignedNoPlaceholders) {
      var fcCounts = failingCheckCounts(assignedNoPlaceholders);
      var fcNames = Object.keys(fcCounts).sort(function(a, b) {
        return fcCounts[b] - fcCounts[a] || a.localeCompare(b);
      }).slice(0, MAX_FAILING_CHECK_FILTER_OPTIONS);
      if (!fcNames.length && !_dashFailingCheckFilter) return '';
      /* Keep a selected check listed even at count 0 so it can be clicked off. */
      if (_dashFailingCheckFilter && fcNames.indexOf(_dashFailingCheckFilter) === -1) {
        fcNames.unshift(_dashFailingCheckFilter);
        if (fcCounts[_dashFailingCheckFilter] == null) fcCounts[_dashFailingCheckFilter] = 0;
      }
      var body = '<div class="facet-values">' + fcNames.map(function(nm) {
        var on = _dashFailingCheckFilter === nm;
        var n = fcCounts[nm] || 0;
        var tip = n >= FLEET_CHECK_SIGNAL_MIN_HIVES
          ? nm + ' failing on ' + n + ' hives — likely fleet-wide'
          : nm + ' failing on ' + n + (n === 1 ? ' hive' : ' hives');
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + esc(tip) + '"' +
          ' onclick="setFailingCheckFilter(' + (on ? "''" : "'" + esc(nm).replace(/'/g, '&#39;') + "'") + ')">' +
          '<span class="facet-value-label">' + esc(nm) + '</span>' +
          '<span class="facet-value-count">' + n + '</span></button>';
      }).join('') + '</div>';
      return facetGroupShell(FACET_GROUP_FAILING_CHECK, 'Failing check (one at a time)',
        !!_dashFacetCollapsed[FACET_GROUP_FAILING_CHECK], body);
    }

    /* How long a hive must have been upgrading before the facet's tooltip
       calls the fleet out as wedged rather than busy. Same limit the counter
       and the hub's stuck-upgrade alert use — one definition of "too long",
       reused, so the rail, the row and the alert panel cannot disagree. */
    var UPGRADE_FACET_STUCK_MS = UPGRADE_ELAPSED_RED_MS;

    /* renderUpgradeFacetGroup renders the upgrade-state picker. Single-select,
       ANDed against the health chips like the failing-check group.

       The 'upgrading' value additionally reports how many of those hives are
       past the stuck limit, because the whole point of this filter is catching
       a rollout that has stopped moving — a bare count of 15 upgrading hives
       looks identical whether they are 10 seconds or 40 minutes in.

       Returns '' when nothing is upgrading or queued AND no filter is on, so a
       settled fleet sees no extra chrome; renders while the filter is on even
       at count 0 so it can always be clicked back off. */
    function renderUpgradeFacetGroup(assignedNoPlaceholders) {
      var hives = assignedNoPlaceholders || [];
      var counts = {};
      counts[UPGRADE_FILTER_UPGRADING] = 0;
      counts[UPGRADE_FILTER_QUEUED] = 0;
      var stuck = 0;
      var now = Date.now();
      for (var i = 0; i < hives.length; i++) {
        var st = hiveUpgradeState(hives[i]);
        if (!st) continue;
        counts[st]++;
        if (st === UPGRADE_FILTER_UPGRADING) {
          var ms = upgradeElapsedMs(hives[i], now);
          /* null (unknown start time) is NOT counted as stuck — an unknown
             elapsed is not evidence of a fault, and inflating this number
             would make the warning untrustworthy. */
          if (ms !== null && ms >= UPGRADE_FACET_STUCK_MS) stuck++;
        }
      }
      var total = counts[UPGRADE_FILTER_UPGRADING] + counts[UPGRADE_FILTER_QUEUED];
      if (!total && !_dashUpgradeFilter) return '';

      var values = [
        {key: UPGRADE_FILTER_UPGRADING, label: 'Upgrading'},
        {key: UPGRADE_FILTER_QUEUED, label: 'Queued for auto-upgrade'}
      ];
      var body = '<div class="facet-values">' + (values || []).map(function(v) {
        var on = _dashUpgradeFilter === v.key;
        var n = counts[v.key] || 0;
        var tip;
        if (v.key === UPGRADE_FILTER_UPGRADING) {
          tip = n + (n === 1 ? ' hive is' : ' hives are') + ' upgrading now';
          if (stuck) {
            tip += ' — ' + stuck + ' past ' + Math.round(UPGRADE_FACET_STUCK_MS / 60000) +
              ' minutes and likely wedged rather than slow';
          }
        } else {
          tip = n + (n === 1 ? ' hive is' : ' hives are') +
            ' behind latest with auto-upgrade on, waiting to be instructed';
        }
        /* The count turns red when any upgrade is past the stuck limit, so the
           fault is visible in the rail without opening the filter. */
        var countStyle = (v.key === UPGRADE_FILTER_UPGRADING && stuck)
          ? ' style="color:var(--red);font-weight:600"' : '';
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + escAttr(tip) + '"' +
          ' onclick="toggleUpgradeFilter(' + jsArg(v.key) + ')">' +
          '<span class="facet-value-label">' + esc(v.label) + '</span>' +
          '<span class="facet-value-count"' + countStyle + '>' + n + '</span></button>';
      }).join('') + '</div>';
      return facetGroupShell(FACET_GROUP_UPGRADE, 'Upgrade state (one at a time)',
        !!_dashFacetCollapsed[FACET_GROUP_UPGRADE], body);
    }

    /* renderAdvisoryStaleFacetGroup renders the stale-advisory toggle as a
       facet group. One value, because the signal is boolean: picking it means
       "only hives whose advisory digest has gone stale".

       Returns '' when no hive is affected AND the filter is off, so a fleet
       with healthy digests sees no new chrome at all — the same self-
       suppressing contract advisoryStaleSummary() uses for the row pill. When
       the filter IS on the group always renders, even at count 0, so it can
       always be clicked back off (the failing-check group does the same). */
    function renderAdvisoryStaleFacetGroup(assignedNoPlaceholders) {
      var n = (assignedNoPlaceholders || []).filter(function(h) {
        return !!(h && h.advisoryStale);
      }).length;
      if (!n && !_dashAdvisoryStaleFilter) return '';
      var on = _dashAdvisoryStaleFilter;
      var tip = n === 1
        ? '1 hive should be posting advisory digests but its digest has gone stale'
        : n + ' hives should be posting advisory digests but their digests have gone stale';
      var body = '<div class="facet-values">' +
        '<button type="button" class="facet-value' + (on ? ' on' : '') +
        '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + esc(tip) + '"' +
        ' onclick="toggleAdvisoryStaleFilter()">' +
        '<span class="facet-value-label">Stale advisory</span>' +
        '<span class="facet-value-count">' + n + '</span></button></div>';
      return facetGroupShell(FACET_GROUP_ADVISORY_STALE, 'Advisory digest',
        !!_dashFacetCollapsed[FACET_GROUP_ADVISORY_STALE], body);
    }

    /* renderFacetRail draws the tray's body: the health chips and failing-check
       picker moved in from the old standalone bar, then the derived facet
       groups. Derived-group counts are computed over the assigned hives after
       the chips and the search box, but BEFORE the facets in the same group are
       applied — so a group keeps showing its alternatives once you pick one,
       rather than collapsing to the single value you chose. */
    function renderFacetRail(assignedAll) {
      var rail = document.getElementById('hive-facet-rail');
      /* The rail's affordance reflects filter state even when the rail body
         itself has nothing to draw, so update it before any early return. */
      renderFacetTrayAffordance();
      if (!rail) return;
      /* The health and failing-check groups filter ASSIGNED hives only — the
         unassigned pool is inventory. The caller already scopes to assigned;
         strip any placeholder defensively so the counts match the old bar. */
      var assignedReal = (assignedAll || []).filter(function(h) { return !isPlaceholderHive(h); });
      var base = (assignedAll || []).filter(hiveMatchesFilters).filter(hiveMatchesSearch);
      var head = renderStatusFacetGroup(assignedReal) + renderFailingCheckFacetGroup(assignedReal) +
        renderUpgradeFacetGroup(assignedReal) + renderAdvisoryStaleFacetGroup(assignedReal);
      if (!base.length && !Object.keys(_dashFacets || {}).length) {
        rail.innerHTML = head + clearFacetsButton();
        return;
      }
      var html = head;
      (HIVE_FACET_GROUPS || []).forEach(function(grp) {
        /* Count within this group ignoring this group's own selections. */
        var others = {};
        Object.keys(_dashFacets || {}).forEach(function(k) { if (k !== grp.key) others[k] = _dashFacets[k]; });
        var saved = _dashFacets;
        _dashFacets = others;
        var pool = base.filter(hiveMatchesFacets);
        _dashFacets = saved;

        var counts = {};
        pool.forEach(function(h) {
          var v = String(hiveFacetValues(h)[grp.key]);
          counts[v] = (counts[v] || 0) + 1;
        });
        /* Always surface a currently-selected value, even at count 0, so the
           user can always click it off again. */
        Object.keys((_dashFacets[grp.key] || {})).forEach(function(v) { if (counts[v] == null) counts[v] = 0; });
        var values = Object.keys(counts).sort();
        if (!values.length) return;
        var collapsed = !!_dashFacetCollapsed[grp.key];
        var valuesHTML = '';
        if (!collapsed) {
          valuesHTML = '<div class="facet-values">' + values.map(function(v) {
            var on = !!(_dashFacets[grp.key] && _dashFacets[grp.key][v]);
            return '<button type="button" class="facet-value' + (on ? ' on' : '') + '" aria-pressed="' + (on ? 'true' : 'false') +
              '" onclick="toggleFacetValue(\'' + esc(grp.key) + '\',\'' + esc(v).replace(/'/g, '&#39;') + '\')" title="' + esc(v) + '">' +
              '<span class="facet-value-label">' + esc(v) + '</span>' +
              '<span class="facet-value-count">' + counts[v] + '</span></button>';
          }).join('') + '</div>';
        }
        html += facetGroupShell(grp.key, grp.label, collapsed, valuesHTML);
      });
      rail.innerHTML = html + clearFacetsButton();
    }

    /* One Clear button for the whole tray. It calls clearAllHiveFilters(), not
       clearHiveFacets(), because the health chips and the failing-check picker
       now live in the same tray: a button labelled "Clear" that left two of the
       three groups still filtering would be a lie. */
    function clearFacetsButton() {
      return activeFilterCount() > 0
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear all filters</button>'
        : '';
    }

    /* applyDashFilters filters the assigned hives the caller wants rendered.
       Unassigned pool placeholders are handled separately in renderHives:
       health/status/facet filters do not hide inventory, but the search box
       still narrows every displayed row so typing text cannot leave unrelated
       placeholders visible under an active search.
       For assigned rows all four narrowing mechanisms compose as an AND: the
       status chips (by state), the alert-type filter (hives carrying that
       alert), the search box and the facets. That is what "click an alert type
       to see those hives" has to mean when a chip or a search term is already
       active. */
    function applyDashFilters(hives) {
      return (hives || []).filter(function(h) {
        return hiveMatchesFilters(h) && hiveMatchesAlertFilter(h) &&
          hiveMatchesSearch(h) && hiveMatchesFacets(h);
      });
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
      /* "Clear filters" is offered from the empty state, so it must clear
         EVERY filter — leaving one on would keep the list empty and make the
         button look broken. */
      _dashFailingCheckFilter = '';
      _dashDriftFilter = '';
      /* The drift filter is being force-cleared without going through
         toggleDriftFilter, so drop any stashed search with it. */
      _driftSearchStash = null;
      _dashUpgradeFilter = '';
      _dashAdvisoryStaleFilter = false;
      renderHives(_allDashHives, true);
    }

    /* toggleUpgradeFilter selects an upgrade state, or clears it when the
       already-selected value is clicked again (single-select, like the drift
       filter). */
    function toggleUpgradeFilter(state) {
      _dashUpgradeFilter = (_dashUpgradeFilter === state) ? '' : (state || '');
      renderHives(_allDashHives, true);
    }

    /* toggleAdvisoryStaleFilter flips the stale-advisory narrowing on or off. */
    function toggleAdvisoryStaleFilter() {
      _dashAdvisoryStaleFilter = !_dashAdvisoryStaleFilter;
      renderHives(_allDashHives, true);
    }

    /* toggleDriftFilter selects (or deselects) one drift kind in the fleet
       exceptions summary.

       Pill selection also owns the search box: selecting the first pill
       stashes and clears the search (so a stale search cannot silently
       intersect with the pill filter), switching between pills keeps the
       stash, and deselecting the last pill restores the stashed text —
       unless the user typed a new search while a pill was active, in which
       case the stash was dropped and their new text wins. */
    function toggleDriftFilter(kind) {
      var prev = _dashDriftFilter;
      _dashDriftFilter = (_dashDriftFilter === kind) ? '' : kind;
      var searchEl = document.getElementById('hive-search');
      if (!prev && _dashDriftFilter) {
        /* First pill selected: stash the search and clear it. */
        _driftSearchStash = _dashSearchQuery;
        _dashSearchQuery = '';
        if (searchEl) searchEl.value = '';
      } else if (prev && !_dashDriftFilter) {
        /* Last pill deselected: restore the stashed search, if it is still
           ours to restore. */
        if (_driftSearchStash !== null) {
          _dashSearchQuery = _driftSearchStash;
          if (searchEl) searchEl.value = _dashSearchQuery;
        }
        _driftSearchStash = null;
      }
      renderHives(_allDashHives, true);
    }

    /* renderDriftSummary draws the "N hives need attention" strip above the
       table, broken down by drift kind and clickable to filter.

       Counts are computed over the ASSIGNED set the caller passes, matching
       the status chips: an unassigned placeholder is never flagged for the
       claimed-hive concerns (no App, ACMM 0, no agents) server-side, and
       scoping the summary the same way keeps the headline count equal to the
       number of rows a human can actually act on. */
    function renderDriftSummary(assignedHives) {
      var el = document.getElementById('hive-drift-summary');
      if (!el) return;
      var hives = assignedHives || [];

      var hivesWithDrift = 0;
      var worstOverall = '';
      var kindCounts = {};   // kind -> number of HIVES carrying it
      var kindWorst = {};    // kind -> worst severity seen for that kind
      for (var i = 0; i < hives.length; i++) {
        var d = driftOf(hives[i]);
        if (!d.count) continue;
        hivesWithDrift++;
        if (DRIFT_SEVERITY_ORDER.indexOf(d.worstSeverity) >= 0 &&
            (worstOverall === '' || DRIFT_SEVERITY_ORDER.indexOf(d.worstSeverity) < DRIFT_SEVERITY_ORDER.indexOf(worstOverall))) {
          worstOverall = d.worstSeverity;
        }
        /* A hive can legitimately report the same kind once only, but count
           distinct kinds per hive defensively so a duplicated signal cannot
           inflate the breakdown past the headline. */
        var seenKinds = {};
        for (var j = 0; j < d.signals.length; j++) {
          var s = d.signals[j] || {};
          if (!s.kind || seenKinds[s.kind]) continue;
          seenKinds[s.kind] = true;
          kindCounts[s.kind] = (kindCounts[s.kind] || 0) + 1;
          var prev = kindWorst[s.kind];
          if (!prev || DRIFT_SEVERITY_ORDER.indexOf(s.severity) < DRIFT_SEVERITY_ORDER.indexOf(prev)) {
            kindWorst[s.kind] = s.severity;
          }
        }
      }

      // Hide the strip only when there is nothing to attend to AND no attention
      // filter is currently applied. If a filter IS active (e.g. the user clicked
      // "upgrading" and the count has since dropped to 0), the strip MUST stay so
      // its chip + "Show all" escape remain reachable — otherwise the filter keeps
      // narrowing the list with no visible way to turn it off.
      if (!hivesWithDrift && !_dashDriftFilter) {
        el.style.display = 'none';
        el.innerHTML = '';
        return;
      }
      el.style.display = '';

      /* Sort the breakdown worst-severity-first, then by how many hives are
         affected, then by label — so the most urgent, most widespread problem
         is always the first thing read. */
      var kinds = Object.keys(kindCounts).sort(function(a, b) {
        var sa = DRIFT_SEVERITY_ORDER.indexOf(kindWorst[a]);
        var sb = DRIFT_SEVERITY_ORDER.indexOf(kindWorst[b]);
        if (sa !== sb) return sa - sb;
        if (kindCounts[b] !== kindCounts[a]) return kindCounts[b] - kindCounts[a];
        return driftKindLabel(a).localeCompare(driftKindLabel(b));
      });
      // Keep the ACTIVE filter's chip listed even after its count falls to 0, so
      // it can always be clicked off (mirrors renderFailingCheckFacetGroup). Without
      // this the chip vanishes the moment its last hive clears while the filter is
      // still applied, stranding the user on an empty, un-clearable filtered view.
      if (_dashDriftFilter && kinds.indexOf(_dashDriftFilter) === -1) {
        kinds.unshift(_dashDriftFilter);
        if (kindCounts[_dashDriftFilter] == null) kindCounts[_dashDriftFilter] = 0;
        if (kindWorst[_dashDriftFilter] == null) kindWorst[_dashDriftFilter] = 'info';
      }

      var chips = kinds.map(function(k) {
        var color = DRIFT_SEVERITY_COLORS[kindWorst[k]] || DRIFT_SEVERITY_COLORS.info;
        var on = _dashDriftFilter === k;
        var cls = on ? 'filter-chip on' : 'filter-chip';
        return '<button type="button" class="' + cls + '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleDriftFilter(\'' + esc(k) + '\')" style="--chip-color:' + color + '">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          esc(driftKindLabel(k)) + '<span class="filter-chip-count">' + kindCounts[k] + '</span></button>';
      }).join('');

      var headColor = DRIFT_SEVERITY_COLORS[worstOverall] || DRIFT_SEVERITY_COLORS.info;
      var headline = hivesWithDrift === 1
        ? '1 hive needs attention'
        : hivesWithDrift + ' hives need attention';
      var clearBtn = _dashDriftFilter
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="toggleDriftFilter(\'' + esc(_dashDriftFilter) + '\')">Show all</button>'
        : '';

      el.innerHTML =
        '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">' +
        '<span style="color:' + headColor + ';font-weight:600;font-size:0.8rem">⚠ ' + esc(headline) + '</span>' +
        '<span style="color:var(--muted);font-size:0.7rem">configuration drift detected from the fleet norm</span>' +
        '</div>' +
        '<div class="filter-chips" style="margin-top:8px">' + chips + clearBtn + '</div>';
    }

    /* renderStatusFilterBar draws the "Showing X of Y" summary and the Clear
       button. The health chips and the failing-check picker used to live here as
       standalone chip rows; they now render inside the facet tray
       (renderStatusFacetGroup / renderFailingCheckFacetGroup). What stays is the
       one thing that must NOT be hidden behind a collapsed tray: the statement
       that the list is currently narrowed, and the way back out.

       Unassigned placeholders are excluded from the totals — they are
       inventory, never filtered by these controls. */
    function renderStatusFilterBar(allHivesIn, shownCount) {
      var bar = document.getElementById('hive-filter-bar');
      if (!bar) return;
      var allHives = (allHivesIn || []).filter(function(h) { return !isPlaceholderHive(h); });
      /* activeFilterCount covers EVERY narrowing control — the health chips,
         the failing-check picker, drift, the alert drill-down, the search box
         and the facets. Omitting one would hide the Clear button while the list
         is still filtered, which reads as hives having disappeared. */
      var activeN = activeFilterCount();
      var anyActive = activeN > 0;
      var total = (allHives || []).length;
      var summary = anyActive
        ? 'Showing ' + shownCount + ' of ' + total + ' assigned hives' +
          ' &middot; ' + activeN + (activeN === 1 ? ' filter active' : ' filters active')
        : total + (total === 1 ? ' assigned hive' : ' assigned hives');
      var clearBtn = anyActive
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear filters</button>'
        : '';
      bar.innerHTML = '<span class="filter-summary">' + summary + '</span>' + clearBtn;
      /* The bar no longer carries chip rows, so on a fleet with no assigned
         hives it would render as an empty flex box still claiming its
         margin-bottom. Collapse it outright in that case rather than leaving a
         gap above the table. */
      bar.style.display = total ? '' : 'none';
    }

    /* renderViewBar draws the group-by <select> and the saved-view controls.
       Every user-controlled string here — saved-view names the operator typed —
       is escaped on render; group-by labels are our own constants but go
       through esc() too so the rule needs no exceptions. */
    function renderViewBar() {
      var bar = document.getElementById('hive-view-bar');
      if (!bar) return;
      var opts = (HIVE_GROUP_DIMENSIONS || []).map(function(d) {
        return '<option value="' + esc(d.key) + '"' + (d.key === _dashGroupBy ? ' selected' : '') + '>' + esc(d.label) + '</option>';
      }).join('');
      var groupCtl = '<label class="view-ctl"><span class="view-ctl-label">Group by</span>' +
        '<select id="hive-group-by" class="view-select" onchange="setDashGroupBy(this.value)">' + opts + '</select></label>';

      var viewOpts = '<option value="">No saved view</option>' + (_dashSavedViews || []).map(function(v) {
        var isDefault = v.name === _dashDefaultView;
        return '<option value="' + esc(v.name) + '"' + (v.name === _dashActiveView ? ' selected' : '') + '>' +
          esc(v.name) + (isDefault ? ' ★' : '') + '</option>';
      }).join('');
      var hasSel = !!_dashActiveView && !!findSavedView(_dashActiveView);
      var dis = hasSel ? '' : ' disabled';
      var defaultLabel = (hasSel && _dashDefaultView === _dashActiveView) ? 'Unset default' : 'Set default';
      var viewCtl = '<label class="view-ctl"><span class="view-ctl-label">View</span>' +
        '<select id="hive-saved-view" class="view-select" onchange="onSavedViewPick(this.value)">' + viewOpts + '</select></label>' +
        '<button type="button" class="view-btn" onclick="saveCurrentView()" title="Save the current grouping, filters and sort as a named view">Save view</button>' +
        '<button type="button" class="view-btn" onclick="renameSavedView()"' + dis + '>Rename</button>' +
        '<button type="button" class="view-btn" onclick="deleteSavedView()"' + dis + '>Delete</button>' +
        '<button type="button" class="view-btn" onclick="toggleDefaultView()"' + dis +
        ' title="Apply this view automatically the next time My Hives loads">' + defaultLabel + '</button>';

      bar.innerHTML = '<div class="view-ctls">' + groupCtl + '</div><div class="view-ctls">' + viewCtl + '</div>';
    }

    /* hiveProvisionTime returns when this hive came into existence, as epoch
       ms, or null when that is genuinely unknown.

       Why registeredAt and NOT the per-hive meta.json created_at: created_at is
       declared on SaaSHive and written on exactly one provisioning path, so
       every hive that predates it — or that was created any other way — carries
       the empty string. On the live fleet at the time of writing, 26 of 50
       meta.json files have "created_at": "", including long-running assigned
       hives, while registeredAt was populated on 50 of 50 registry entries.
       registeredAt is also preserved across heartbeats (server.go copies it
       from the previous entry rather than restamping it), so it stays the
       first-seen time instead of drifting to "now" on every beat.

       Returns null — never NaN and never a rendered 'Invalid Date' — for a
       missing or unparseable value, so the caller can order those rows
       explicitly instead of comparing against NaN, which is false in both
       directions and would make the sort non-deterministic. */
    function hiveProvisionTime(h) {
      var raw = h && h.registeredAt;
      if (typeof raw !== 'string' || !raw) return null;
      var t = Date.parse(raw);
      return isNaN(t) ? null : t;
    }

    /* sortedDashHives applies the CURRENT sort key/direction to the unfiltered
       cache. Split out of sortDashHives so restoring a saved view (which sets
       the key/direction directly rather than by clicking a header) reproduces
       the same ordering without re-implementing the comparator. */
    function sortedDashHives() {
      var key = _dashSortKey;
      if (!key) return _allDashHives;
      return (_allDashHives || []).slice().sort(function(a, b) {
        if (key === 'journey') {
          /* journey is an object, so sort by how urgent it is rather than by
             stringifying it. Higher = needs attention sooner, so the default
             ascending sort puts on-track hives first and the most-escalated
             last; click again to bring the problems to the top. */
          var ja = journeySortValue(a && a.journey);
          var jb = journeySortValue(b && b.journey);
          return _dashSortAsc ? ja - jb : jb - ja;
        }
        if (key === 'registeredAt') {
          /* Provision time. Compared as epoch ms, not as text: registeredAt is
             RFC3339 so it happens to sort lexically today, but a value that
             ever arrives with an offset ('...+02:00') or without the 'Z' would
             collate wrong, and a string compare cannot express "unknown last".

             Hives with no parseable timestamp sort LAST in both directions
             rather than clumping at whichever end '' collates to — they are
             the least informative rows, so they stay out of the way whether
             the operator asked for oldest-first or newest-first. */
          var ra = hiveProvisionTime(a);
          var rb2 = hiveProvisionTime(b);
          if (ra === null && rb2 === null) return 0;
          if (ra === null) return 1;
          if (rb2 === null) return -1;
          return _dashSortAsc ? ra - rb2 : rb2 - ra;
        }
        if (key === 'quadrant' || key.indexOf('quadrant') === 0) {
          /* Quadrant sorts rank by composite or by one axis. A hive with no
             quadrant — unscored, or one the viewer may not see — sorts LAST in
             both directions rather than clumping at whichever end 0 collates
             to. It is the least informative row either way, so it stays out of
             the operator's path whether they asked for strongest or weakest.
             Sorting by weakest axis is the point of the column: it turns the
             table into a worklist. */
          var qa = quadrantSortValue(a, key);
          var qb = quadrantSortValue(b, key);
          if (qa === null && qb === null) return 0;
          if (qa === null) return 1;
          if (qb === null) return -1;
          return _dashSortAsc ? qa - qb : qb - qa;
        }
        var va = key === 'name' ? hiveNameSortValue(a) : ((a && a[key]) || '');
        var vb = key === 'name' ? hiveNameSortValue(b) : ((b && b[key]) || '');
        if (typeof va === 'number' && typeof vb === 'number') return _dashSortAsc ? va - vb : vb - va;
        return _dashSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
    }

    /* nameEditAffordance renders the inline pencil that lets an OWNER (or the hub
       admin) rename a hive from its top line. It is a display-only convenience —
       the server's owner-or-admin gate on PUT /name is the real boundary — so a
       non-owner simply gets no pencil, and even if they forged the request the
       handler rejects it 403. Empty string for anyone who cannot rename, so those
       rows are pixel-identical to today. Gated the same way as the row's other
       owner-only actions (h.role === 'owner' || _isAdmin — see isBulkEligible).
       stopPropagation keeps the click from triggering the row's own open/SSO. */
    function canRenameHive(h) {
      return !!h && (h.role === 'owner' || _isAdmin);
    }
    function nameEditAffordance(h) {
      if (!canRenameHive(h)) return '';
      return ' <span onclick="event.stopPropagation();dashRenameHive(' + jsArg(h.id) + ')" ' +
        'title="Rename this hive" role="button" tabindex="0" ' +
        'style="cursor:pointer;opacity:0.45;font-size:0.75rem;vertical-align:middle;margin-left:2px">' +
        '&#9998;</span>';
    }

    /* dashRenameHive prompts for a new name and PUTs it to the rename endpoint.
       A blank submission clears the custom name (the row falls back to the
       org/repo label), which the server accepts deliberately. Cancel (null)
       leaves the hive untouched. On success we reload so every viewer-derived
       surface (label, sort value) re-renders from the fresh registry. */
    async function dashRenameHive(id) {
      var h = (_allDashHives || []).reduce(function(m, x) { return x && x.id === id ? x : m; }, null) || {};
      var current = (h.projectName || h.project_name || '');
      var next = await hivePrompt('Rename hive', current, {ok: 'Rename'});
      if (next === null) return; /* cancelled */
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/name', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({project_name: next})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to rename hive', 'error'); loadHives(); return; }
        hiveToast(next ? ('Renamed to ' + next) : 'Name cleared', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    /* hiveLabel derives the two-line name-cell label from the hive's PROJECT
       (org + primary repo) rather than by splitting h.name.

       Why not h.name: the hub synthesises name as org + "/" + primaryRepo on
       every heartbeat (see RegistryEntry construction in server.go), so
       splitting it back apart is a lossy round-trip of the two fields we
       already have on the row — and it breaks whenever either field itself
       contains a slash. Live fleet evidence: a URL-parsing bug put a HOST in
       the org position, so the listing showed 'github.ibm.com / forthehive'.
       Reading org/primaryRepo directly makes the label reflect the project.

       Precedence, most specific first:
         1. org + primaryRepo  — the real project, the whole point
         2. h.name             — a hive that reported a name but no project
         3. h.id               — last resort, never blank

       Returns {line1, line2}. line2 is '' whenever there is no SECOND thing
       worth saying, so the caller never renders an empty link or a stray '/'.
       An unassigned placeholder (org 'available-*' or 'placeholder', no
       primaryRepo) therefore keeps showing its org on line 1 with no empty
       second line — the same headline it shows today.

       primaryRepo is rendered VERBATIM, including any embedded slash
       ('enricom-ibm/jackrabbit' is a real live value produced by the same
       corruption). Taking only the last segment would silently hide half of
       a legitimate value, and the repo path is what the adjacent GitHub link
       resolves to — the label and the link must agree. */
    function hiveLabel(h) {
      if (!h) return { line1: '', line2: '' };
      var org = h.org || '';
      var repo = h.primaryRepo || '';
      /* Second line is the PRIMARY REPO rendered as a real GitHub path, so the
         row states the full project. Fall back to repos[0] when primaryRepo is
         unset, and never emit a dangling slash: with only one half known, show
         that half alone; with neither, leave line2 empty.

         DOUBLING GUARD (mirrors Go repoDisplayLine): primaryRepo is stored
         verbatim and, for some hives, already carries a full 'owner/repo' path.
         Live fleet evidence: a github.io/GHE hive was recorded with
         org='castrojo.github.io' (a HOST wrongly parsed into the org field) and
         primaryRepo='castrojo/endusers' (already owner/repo). Joining org + '/' +
         repo produced 'castrojo.github.io/castrojo/endusers' — the host posing as
         an org, owner doubled. When repo already contains a slash it is a
         complete path and is used as-is; only a bare repo name is qualified with
         the org. Keep this in sync with hub.repoDisplayLine. */
      var repoLine = '';
      var fallbackRepo = repo || ((h.repos || [])[0] || '');
      if (fallbackRepo && fallbackRepo.indexOf('/') !== -1) { repoLine = fallbackRepo; }
      else if (org && fallbackRepo) { repoLine = org + '/' + fallbackRepo; }
      else if (org || fallbackRepo) { repoLine = org || fallbackRepo; }
      /* Top line is the operator-editable hive name (project_name / projectName)
         when set. Blank keeps EXACTLY today's fallback: the org, else the repo,
         else the synthesized name, else the id — so an un-renamed hive is
         pixel-identical to before this feature. */
      var custom = String(h.projectName || h.project_name || '').trim();
      if (custom) return { line1: custom, line2: repoLine };
      var fallback = org || repo || h.name || h.id || '';
      /* When line1 falls back to the org/repo identity, avoid repeating org on
         both lines: if the fallback IS the org and we already have a repoLine,
         line1 keeps the org and line2 shows org/repo — the same two facts the
         old label showed (org on top, repo below), just with the repo now
         qualified by its org. */
      return { line1: fallback, line2: (repoLine === fallback ? '' : repoLine) };
    }

    /* hiveNameSortValue is the value the Hive column SORTS on. It must track
       the label the name cell DISPLAYS, so it delegates to hiveLabel —
       otherwise clicking "Hive ⇅" orders rows by text nobody can see.

       Sorting on the raw h.name is what diverges: the hub synthesises name as
       org + "/" + primaryRepo, so a hive with no org sorts under "/repo" (the
       leading slash collates before every letter) and a hive with neither
       sorts under "" (always first), while both DISPLAY an ordinary word. This
       reproduces hiveLabel's precedence and joins the two lines with a space,
       so the sort reads top line first, then second line — exactly how the
       cell is read. */
    function hiveNameSortValue(h) {
      var label = hiveLabel(h);
      return label.line2 ? label.line1 + ' ' + label.line2 : label.line1;
    }

    /* quadrantSortValue resolves a quadrant sort key to a number, or null when
       the hive has nothing to rank on that key.

       Null rather than 0 for an unscored axis, deliberately: zero is a real
       score and would place an unmeasured hive among the genuinely weak ones,
       which is precisely the confusion the whole scored/unscored split exists
       to prevent. */
    function quadrantSortValue(h, key) {
      var q = h && h.quadrant;
      if (!q || !q.scored_axes) return null;
      if (key === 'quadrant') return q.composite;
      var name = key.slice('quadrant'.length).toLowerCase();
      var a = quadrantAxis(q, name);
      return a.scored ? a.score : null;
    }

    function sortDashHives(key) {
      if (_dashSortKey === key) { _dashSortAsc = !_dashSortAsc; } else { _dashSortKey = key; _dashSortAsc = true; }
      persistHiveSort();
      renderHives(sortedDashHives(), true);
    }

    /* subSort renders ONE inline, clickable sort control for a combined-column
       header. A folded column (Maturity, Activity) hosts two or three sort keys,
       so the whole <th> can no longer own a single onclick — each key gets its
       own ⇅ span instead, keeping every folded sort reachable directly from the
       header exactly as the standalone columns' ⇅ were.

       key      — the sortDashHives sort key (a fixed literal, never user input);
       label    — the visible ⇅ text (e.g. 'Prov ⇅');
       titleRaw — the hover text, inserted VERBATIM into the title attribute to
                  match how the original standalone headers embedded literals
                  (some carry pre-encoded entities like &apos;). Callers pass
                  trusted string constants only.

       event.stopPropagation() so a sub-sort inside a <th> that ALSO has its own
       onclick (Uptime hosts the Prov sub-sort) fires only its own key, not the
       parent's. jsArg supplies the quotes around the key so an apostrophe could
       never break out of the handler. */
    function subSort(key, label, titleRaw) {
      return '<span onclick="event.stopPropagation();sortDashHives(' + jsArg(key) + ')" ' +
        'style="cursor:pointer;font-weight:400;color:var(--muted);margin-left:2px"' +
        (titleRaw ? ' title="' + titleRaw + '"' : '') + '>' + label + '</span>';
    }

    /* stackHeader lays a combined column's header out on TWO lines instead of one
       inline string: the title on line 1, its sub-sort ⇅ chips on line 2. The
       inline form ("Uptime ⇅ Prov ⇅", "Activity Iss ⇅ PRs ⇅ Ctr ⇅") set each
       folded column's width from the full concatenated string, which made the
       fleet table far wider than it needed to be. Stacking makes the column width
       track the widest SINGLE line instead.

       .hive-table th sets white-space:nowrap, which would keep everything on one
       line; a flex column overrides that for layout, and the chip row is allowed
       to wrap (flex-wrap) so three chips fold onto a further line on a truly
       narrow table rather than forcing the column wide. Colours come from the
       existing --muted token and the inherited th colour, so both themes track.

       titleHTML    — the line-1 content. May be plain text (e.g. 'Maturity') or
                      an already-built clickable span; inserted VERBATIM, callers
                      pass trusted markup only.
       subSortsHTML — the line-2 content: one or more subSort() spans (each already
                      carries its own onclick + event.stopPropagation()). Passed
                      through unchanged, so every folded sort stays reachable and
                      the stopPropagation contract is untouched. */
    function stackHeader(titleHTML, subSortsHTML) {
      return '<span style="display:inline-flex;flex-direction:column;align-items:center;gap:1px;line-height:1.15">' +
        '<span style="white-space:nowrap">' + titleHTML + '</span>' +
        '<span style="display:inline-flex;flex-wrap:wrap;justify-content:center;gap:2px;font-size:0.9em;color:var(--muted)">' +
        subSortsHTML + '</span></span>';
    }

    /* persistHiveSort records the operator's EXPLICIT sort choice. Written only
       from sortDashHives (a header click), never from loadHiveSortPrefs, so
       restoring a preference can't rewrite it and a first visit leaves the key
       absent rather than storing the default — which is what lets
       loadHiveSortPrefs tell "never chose" apart from "chose the default". */
    function persistHiveSort() {
      lsSetJSON(LS_HIVE_SORT, {key: _dashSortKey, asc: _dashSortAsc});
    }

    /* ---- Scroll position across refresh --------------------------------
       "Location on page" is the WINDOW's scroll offset, not a container's:
       .table-wrap is overflow:visible at desktop widths (it only becomes an
       overflow-x scroller in the narrow media query, and that axis is
       horizontal), so the hive list scrolls the document itself.

       Writes are throttled through requestAnimationFrame because scroll fires
       at input rate and localStorage.setItem is synchronous — persisting on
       every event would jank the scroll it is trying to record. */
    var HIVE_SCROLL_MAX_AGE_MS = 30 * 60 * 1000; /* 30 min — see readHiveScroll */
    var HIVE_SCROLL_RESTORE_FRAMES = 3;          /* see restoreHiveScroll */
    var _hiveScrollWritePending = false;
    var _hiveScrollRestored = false;

    function persistHiveScroll() {
      if (_hiveScrollWritePending) return;
      _hiveScrollWritePending = true;
      window.requestAnimationFrame(function() {
        _hiveScrollWritePending = false;
        var y = window.pageYOffset || document.documentElement.scrollTop || 0;
        lsSetJSON(LS_HIVE_SCROLL, {y: y, t: Date.now()});
      });
    }

    /* readHiveScroll returns the stored offset, or 0 when there is nothing
       usable. Stale entries are discarded: coming back to the dashboard after
       hours, the fleet has changed underneath and the old offset points at
       unrelated rows, so landing at the top is the honest result. Guards the
       shape too — a hand-edited or truncated value must not yield NaN, which
       window.scrollTo would silently treat as 0 anyway but which would also
       poison the comparison below. */
    function readHiveScroll() {
      var s = lsGetJSON(LS_HIVE_SCROLL, null);
      if (!s || typeof s !== 'object') return 0;
      var y = Number(s.y), t = Number(s.t);
      if (!isFinite(y) || y <= 0) return 0;
      if (!isFinite(t) || (Date.now() - t) > HIVE_SCROLL_MAX_AGE_MS) return 0;
      return y;
    }

    /* restoreHiveScroll re-applies the saved offset ONCE per page load.

       Why not a single scrollTo at init: the rows are painted asynchronously
       (paintCachedHives, then loadHives' await, then renderHives), and the
       document is only as tall as what has been painted so far. Scrolling to
       y before the table exists clamps to the current maximum — silently
       landing at the top, which is exactly the bug this is meant to fix.

       So it retries across a few animation frames and stops as soon as the
       document is actually tall enough for the offset to survive, which is the
       observable definition of "the rows have rendered". The frame budget is
       small and bounded so a genuinely shorter list (hives removed since) costs
       three frames and then gives up rather than fighting the user forever. */
    function restoreHiveScroll() {
      if (_hiveScrollRestored) return;
      var y = readHiveScroll();
      if (!y) { _hiveScrollRestored = true; return; }
      var attempts = 0;
      var tryScroll = function() {
        var maxY = Math.max(0, document.documentElement.scrollHeight - window.innerHeight);
        if (maxY >= y) {
          window.scrollTo(0, y);
          _hiveScrollRestored = true;
          return;
        }
        if (++attempts >= HIVE_SCROLL_RESTORE_FRAMES) {
          /* Never tall enough — go as far as the page allows so the operator
             at least lands near where they were. */
          window.scrollTo(0, maxY);
          _hiveScrollRestored = true;
          return;
        }
        window.requestAnimationFrame(tryScroll);
      };
      window.requestAnimationFrame(tryScroll);
    }

    /* loadHiveSortPrefs resolves the stored sort against the A-Z default.

       Precedence: a PERSISTED choice wins, the default applies only when there
       is nothing usable stored — first visit, cleared storage, or a value that
       no longer validates. Applying A-Z unconditionally would undo the operator's
       choice on every refresh, which is the exact complaint this fixes.

       Every failure mode degrades to the default rather than throwing: lsGetJSON
       already swallows disabled storage and malformed JSON, and isHiveSortKey
       rejects a stale key so a removed column cannot leave the table sorting on
       a property no row has. */
    function loadHiveSortPrefs() {
      var stored = lsGetJSON(LS_HIVE_SORT, null);
      if (stored && typeof stored === 'object' && isHiveSortKey(stored.key)) {
        _dashSortKey = stored.key;
        /* Only an explicit false flips direction — a stored object missing
           'asc' still means ascending. */
        _dashSortAsc = stored.asc !== false;
        return;
      }
      _dashSortKey = HIVE_SORT_DEFAULT_KEY;
      _dashSortAsc = HIVE_SORT_DEFAULT_ASC;
    }

    /* ---- Usage attribution panel ----------------------------------------
       Token rollups by org / owner / cluster. TOKENS ONLY: the hub receives a
       single blended token total per hive with no per-model split, so no
       dollar figure is derivable here (see usage.go). The panel shows the
       server's currencyNote rather than inventing a rate. */

    /* USAGE_TOP_N is how many rollup rows each dimension shows collapsed. At
       42+ hives the full org/owner lists do not fit on screen; 5 keeps the
       three tables side-by-side and readable, with "Show all" to expand. */
    var USAGE_TOP_N = 5;
    /* USAGE_ZERO_TOP_N bounds the zero-consumption list the same way. Slightly
       larger than USAGE_TOP_N because this list is the actionable one — these
       are claimed hives burning nothing, i.e. possibly broken. */
    var USAGE_ZERO_TOP_N = 8;
    /* USAGE_SPARK_MIN_POINTS is the minimum sampled points before a trend line
       is drawn. Two points is the honest minimum for a line — with one sample
       there is no trend, only a dot, and drawing it would imply history the
       hub does not have. */
    var USAGE_SPARK_MIN_POINTS = 2;

    var _usageData = null;
    var _usageExpanded = {};

    function toggleUsageSection(key) {
      _usageExpanded[key] = !_usageExpanded[key];
      renderUsagePanel();
    }

    /* jumpToUsageBucket links a usage rollup row back to the fleet table.
       One contributing hive → jump straight to (and highlight) its row via
       the same jumpToHiveRow the alerts panel uses. Several → put the bucket
       key in the hive search box (it matches org, repo, cluster and user)
       so the table narrows to exactly that bucket's hives, then scroll to
       the table. Falls back to the first id when the key is the synthetic
       "(unattributed)" bucket, which the search text would never match. */
    function jumpToUsageBucket(key, hiveIds) {
      hiveIds = hiveIds || [];
      if (hiveIds.length === 1) { jumpToHiveRow(hiveIds[0], key); return; }
      if (!hiveIds.length) return;
      var isSynthetic = !key || key.charAt(0) === '(';
      if (isSynthetic) { jumpToHiveRow(hiveIds[0], key); return; }
      var el = document.getElementById('hive-search');
      if (el) el.value = key;
      _dashSearchQuery = key;
      /* A search only narrows what is rendered — make sure hidden sections
         cannot swallow the result, mirroring jumpToHiveRow's widening. */
      expandAllHiveSections();
      renderHives(_allDashHives, true);
      var table = document.querySelector('.hive-table') || document.getElementById('hive-search-row');
      if (table) table.scrollIntoView({behavior: 'smooth', block: 'start'});
      hiveToast('Hive list filtered to ' + key + ' (' + hiveIds.length + ' hives) — Clear to reset', 'info');
    }

    /* Renders one rollup dimension as a compact table. rows are already sorted
       by consumption descending server-side. */
    function renderUsageSection(title, rows, key) {
      rows = rows || [];
      if (!rows.length) {
        return '<div style="flex:1;min-width:200px">' +
          '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px">' + esc(title) + '</div>' +
          '<div style="color:var(--muted);font-size:0.75rem;padding:4px 0">No data</div></div>';
      }
      var expanded = !!_usageExpanded[key];
      var shown = expanded ? rows.length : Math.min(rows.length, USAGE_TOP_N);
      var html = '';
      for (var i = 0; i < shown; i++) {
        var r = rows[i] || {};
        var pct = Number(r.sharePct) || 0;
        /* label (when present) is the server-resolved display name for an
           opaque OIDC key. Display only — jumpToUsageBucket and hive-id
           filtering keep operating on the RAW r.key. */
        var rLabel = String(r.label || r.key || '');
        /* The key cell links back to the fleet table: one hive jumps to its
           row, several filter the table to the bucket. encodeURIComponent
           then esc() so a user-influenced key can neither break out of the
           attribute nor the onclick string. */
        var ids = r.hiveIds || [];
        var keyCell;
        if (ids.length) {
          keyCell = '<a href="#" onclick="jumpToUsageBucket(decodeURIComponent(\'' + esc(encodeURIComponent(String(r.key || ''))) + '\'),JSON.parse(decodeURIComponent(\'' + esc(encodeURIComponent(JSON.stringify(ids))) + '\')));return false"' +
            ' style="color:inherit;text-decoration:none;border-bottom:1px dotted var(--muted)"' +
            ' title="' + esc(rLabel) + ' — show ' + (ids.length === 1 ? 'this hive' : 'these ' + ids.length + ' hives') + ' in the table">' + esc(rLabel) + '</a>';
        } else {
          keyCell = esc(rLabel);
        }
        html += '<tr>' +
          '<td style="padding:2px 6px 2px 0;max-width:150px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(rLabel) + '">' + keyCell + '</td>' +
          '<td style="padding:2px 6px;text-align:right;white-space:nowrap">' + fmtTokens(r.tokens) + '</td>' +
          '<td style="padding:2px 6px;text-align:right;color:var(--muted);white-space:nowrap">' + (Number(r.hives) || 0) + '</td>' +
          /* Inline share bar — cheap visual ranking without a chart library. */
          '<td style="padding:2px 0 2px 6px;width:64px">' +
            '<div style="position:relative;height:9px;background:var(--surface);border-radius:2px;overflow:hidden" title="' + pct.toFixed(1) + '% of total">' +
            '<div style="position:absolute;left:0;top:0;bottom:0;width:' + Math.max(0, Math.min(100, pct)).toFixed(1) + '%;background:var(--accent);opacity:0.65"></div>' +
            '</div></td>' +
          '<td style="padding:2px 0 2px 6px;text-align:right;color:var(--muted);font-size:0.68rem;white-space:nowrap">' + pct.toFixed(1) + '%</td>' +
        '</tr>';
      }
      var more = '';
      if (rows.length > USAGE_TOP_N) {
        more = '<div style="margin-top:4px"><a href="#" onclick="toggleUsageSection(\'' + esc(key) + '\');return false" style="font-size:0.7rem;color:var(--accent);text-decoration:none">' +
          (expanded ? 'Show top ' + USAGE_TOP_N : 'Show all ' + rows.length) + '</a></div>';
      }
      return '<div style="flex:1;min-width:200px">' +
        '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px">' + esc(title) + '</div>' +
        '<table style="width:100%;border-collapse:collapse;font-size:0.75rem"><tbody>' + html + '</tbody></table>' + more +
        '</div>';
    }

    function renderUsagePanel() {
      var el = document.getElementById('usage-panel');
      if (!el) return;
      var d = _usageData;
      if (!d) { el.style.display = 'none'; return; }
      el.style.display = 'block';

      var isFleet = d.scope === 'fleet';
      var scopeLabel = isFleet
        ? 'Fleet-wide'
        : 'Your hives only';

      /* Trend: only drawn from genuinely sampled history. With fewer than
         USAGE_SPARK_MIN_POINTS points we say so instead of drawing a line. */
      var hist = d.history || [];
      var trend = '';
      if (hist.length >= USAGE_SPARK_MIN_POINTS) {
        var pts = [];
        for (var i = 0; i < hist.length; i++) {
          /* sparkline() reads .v — map the snapshot shape onto it. */
          pts.push({ t: hist[i].t, v: Number(hist[i].v) || 0 });
        }
        trend = '<span title="Fleet total, sampled every 15 min (cumulative)">' + sparkline(pts, '#3fb950', 90, 16) + '</span>';
      } else if (isFleet) {
        trend = '<span style="font-size:0.68rem;color:var(--muted)" title="Snapshots are sampled every 15 minutes; a trend line needs at least two.">trend building…</span>';
      }

      var zero = d.zeroConsumption || [];
      var zeroHtml = '';
      if (zero.length) {
        var zExpanded = !!_usageExpanded['zero'];
        var zShown = zExpanded ? zero.length : Math.min(zero.length, USAGE_ZERO_TOP_N);
        var chips = '';
        for (var z = 0; z < zShown; z++) {
          var h = zero[z] || {};
          var dot = h.online
            ? '<span style="color:#3fb950" title="Online but consuming nothing — idle or stuck">●</span>'
            : '<span style="color:var(--muted)" title="Offline">○</span>';
          chips += '<span onclick="jumpToHiveRow(decodeURIComponent(\'' + esc(encodeURIComponent(String(h.id || ''))) + '\'),decodeURIComponent(\'' + esc(encodeURIComponent(String(h.name || h.id || ''))) + '\'))" style="display:inline-block;padding:2px 7px;margin:2px 4px 2px 0;background:var(--surface);border:1px solid var(--border);border-radius:10px;font-size:0.7rem;cursor:pointer" title="' + esc((h.org || '') + ((h.ownerName || h.owner) ? ' · ' + (h.ownerName || h.owner) : '')) + ' — show this hive in the table">' + dot + ' ' + esc(h.name || h.id) + '</span>';
        }
        var zMore = '';
        if (zero.length > USAGE_ZERO_TOP_N) {
          zMore = ' <a href="#" onclick="toggleUsageSection(\'zero\');return false" style="font-size:0.7rem;color:var(--accent);text-decoration:none">' +
            (zExpanded ? 'show fewer' : 'show all ' + zero.length) + '</a>';
        }
        zeroHtml = '<div style="margin-top:12px;padding-top:10px;border-top:1px solid var(--border)">' +
          '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px" title="Claimed hives reporting no tokens. Unassigned pool slots are excluded — those are supposed to consume nothing.">' +
          'Zero consumption (' + zero.length + ')</div>' + chips + zMore + '</div>';
      }

      el.innerHTML =
        '<div style="background:var(--card,var(--surface));border:1px solid var(--border);border-radius:8px;padding:14px 16px">' +
          '<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:12px">' +
            '<h3 style="font-size:0.95rem;color:var(--accent);margin:0">Usage</h3>' +
            '<span style="font-size:0.7rem;color:var(--muted);border:1px solid var(--border);border-radius:10px;padding:1px 7px">' + esc(scopeLabel) + '</span>' +
            '<span style="flex:1"></span>' +
            trend +
            '<span style="font-size:0.8rem;color:var(--text)"><strong>' + fmtTokens(d.totalTokens) + '</strong> tokens' +
            ' <span style="color:var(--muted);font-size:0.72rem">across ' + (Number(d.hiveCount) || 0) + ' hive' + ((Number(d.hiveCount) || 0) === 1 ? '' : 's') + '</span></span>' +
          '</div>' +
          '<div style="display:flex;gap:20px;flex-wrap:wrap">' +
            /* byOrg buckets on org/repo (usageOrgRepoKey in usage.go), so the
               label says so; the JSON field keeps its original name. */
            renderUsageSection('By org / repo', d.byOrg, 'org') +
            renderUsageSection('By owner', d.byOwner, 'owner') +
            renderUsageSection('By cluster', d.byCluster, 'cluster') +
          '</div>' +
          zeroHtml +
          '<div style="margin-top:10px;font-size:0.66rem;color:var(--muted);line-height:1.4">' + esc(d.currencyNote || '') + '</div>' +
        '</div>';
    }

    async function loadUsage() {
      try {
        var resp = await fetch('/api/saas/usage');
        if (!resp.ok) { _usageData = null; renderUsagePanel(); return; }
        _usageData = await resp.json();
      } catch (e) {
        _usageData = null;
      }
      renderUsagePanel();
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
        /* The fleet reference polygon behind every kite. Captured here rather
           than derived in the browser: it is the average over the SAME
           population the server scored, and recomputing it client-side from
           the rows the caller may see would silently exclude the ones they may
           not, quietly moving the reference. */
        _fleetQuadrant = data.fleet_quadrant || null;
        /* Who is logged into their hive right now (admin-only in the payload) →
           the green-dashed avatar border. A Set of lowercased usernames so the
           avatar lookup is case-insensitive, matching GitHub handle semantics. */
        _liveHiveUsers = new Set((data.live_hive_users || []).map(function(n) { return String(n).toLowerCase(); }));
        _liveEngagedUsers = new Set((data.live_engaged_users || []).map(function(n) { return String(n).toLowerCase(); }));
        /* Alerts ride along on the same payload — see handleMyHives. Normalise
           to the empty summary so every consumer can iterate without guarding. */
        _fleetAlerts = data.alerts || EMPTY_ALERT_SUMMARY;
        _hivesSummary = data.hives_summary || null;
        _latestSHA = data.latest_sha || _latestSHA;
        _stableV4SHA = data.stable_v4_sha || _stableV4SHA;
        if (data.latest_shas) _latestSHAs = data.latest_shas;
        if (data.tracked_branches) _trackedBranchesList = data.tracked_branches;
        if (data.release_channels) _releaseChannels = data.release_channels;
        if (data.channel_targets) _channelTargets = data.channel_targets;
        if (data.latest_sha_messages) _latestSHAMessages = data.latest_sha_messages;
        if (data.latest_sha_image_status) _latestImageStatus = data.latest_sha_image_status;
        _latestBuildStarted = data.latest_sha_build_started || {};
        if (data.commit_messages) _commitMessages = data.commit_messages;
        if (data.hub_auto_upgrade !== undefined) _hubAutoUpgrade = data.hub_auto_upgrade;
        if (data.upgrade_pause) _upgradePause = data.upgrade_pause;
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
                var _bStart = _latestBuildStarted[br];
                /* Elapsed build timer: a span the live ticker (_tickBuildTimers)
                   refreshes each second. data-build-start carries the epoch-ms
                   start so the ticker can recompute without a server round-trip.
                   Omitted when the hub hasn't reported a start time yet. */
                var _timerHTML = _bStart
                  ? '<span class="build-timer" data-build-start="' + _bStart + '" style="font-size:0.65rem;color:var(--muted);opacity:0.7;white-space:nowrap;font-variant-numeric:tabular-nums">' + fmtBuildElapsed(_bStart) + '</span>'
                  : '';
                brStatusHTML = '<span style="display:inline-block;flex:none;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite" title="Container image for this commit is still building"></span><span style="font-size:0.65rem;color:var(--muted);opacity:0.7;white-space:nowrap">building image…</span>' + _timerHTML;
              } else if (brStatus === 'failed') {
                brStatusHTML = '<span style="color:var(--red);font-size:0.7rem;cursor:help" title="Image build failed for this commit — upgrades keep using the previous image">✗</span>';
              }
              lines += '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px"><span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(br) + '</span><span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHAs[br]) + '</span>' + (brMsg ? '<span style="font-size:0.7rem;color:var(--muted);opacity:0.7">: ' + esc(brMsg) + '</span>' : '') + brStatusHTML +
                /* Per-line pulls mini chart: a .line-pulls span each
                   row, filled from the cached /api/hub/image-pulls payload now
                   and refreshed by loadImagePulls() on every dashboard poll.
                   A line with no pull data renders "—", never an error. */
                '<span class="line-pulls" data-branch="' + escAttr(br) + '" style="display:inline-flex;align-items:center;margin-left:4px">' + linePullSparkHTML(br) + '</span>' +
                '</div>';
            }
          } else if (_latestSHA) {
            lines = '<span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHA) + '</span>';
          }
          /* Channel block, rendered ABOVE the per-branch rows as
                 stable    ->  v4 3d31590 : <subject>
             so the moving tags read as pointers INTO the branch list below
             rather than as extra branches. The arrow column is fixed-width so
             the three channel names left-align and the targets line up.
             Nothing here assumes a channel tracks any particular branch: the
             target comes from the server's digest match, and a channel with no
             match renders its digest (or "unknown") instead of a branch. */
          var chLines = '';
          for (var ci = 0; ci < _channelTargets.length; ci++) {
            var ct = _channelTargets[ci] || {};
            var ctTarget;
            if (ct.branch) {
              var ctMsg = _latestSHAMessages[ct.branch] || '';
              ctTarget = '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(ct.branch) + '</span><span style="font-family:monospace;color:var(--muted);margin-left:6px">' + esc(ct.sha || '') + '</span>' + (ctMsg ? '<span style="font-size:0.7rem;color:var(--muted);opacity:0.7">: ' + esc(ctMsg) + '</span>' : '');
            } else if (ct.digest) {
              /* Resolved, but to something no tracked branch points at — a
                 pinned or mid-promotion build. Show the digest so the operator
                 can still identify it; do NOT attribute it to a branch. */
              ctTarget = '<span style="font-family:monospace;color:var(--muted)" title="' + escAttr(ct.digest) + '">' + esc(shortDigest(ct.digest)) + '</span>';
            } else {
              ctTarget = '<span style="color:var(--muted);opacity:0.7;font-size:0.7rem" title="Channel tag could not be resolved on GHCR">unknown</span>';
            }
            chLines += '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px"><span style="display:inline-block;min-width:' + CHANNEL_NAME_MIN_W_PX + 'px;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(34,197,94,0.15);color:#4ade80;border:1px solid rgba(34,197,94,0.3);text-align:center" title="Release channel — a moving tag that follows whichever build is currently promoted to this track">' + esc(ct.channel) + '</span><span style="color:var(--muted);opacity:0.6;font-size:0.7rem">-&gt;</span>' + ctTarget + '</div>';
          }
          if (chLines) {
            chLines = '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:2px">Release channels:</div>' + chLines +
              '<div style="height:6px"></div>';
          }
          /* The branch rows keep their original header; the channel block gets
             its own above it, so neither reads as a subheading of the other. */
          if (lines) lines = '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:2px">Latest available images:</div>' + lines;
          lines = chLines + lines;
          shaEl.innerHTML = lines ? lines :'<div style="display:flex;align-items:center;gap:6px;font-size:0.7rem;color:var(--muted)"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite"></span>Resolving latest available images…</div>';
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
            // The container image for the latest hub SHA can still be building
            // (same signal the per-hive rows use). While it builds there is nothing
            // to upgrade TO yet, so the pill must read "queued" (matching the per-hive
            // "building image…" state) rather than offer a clickable green "Upgrade"
            // that would pull the PREVIOUS image. Only "Upgrading" (an active rollout)
            // outranks it.
            var hubImageBuilding = (_latestImageStatus[hubBranch] || '') === 'building';
            var hubQueued = !hubIsUpgrading && (hubState === 'queued' || hubImageBuilding ||
              (hubState === '' && !isCurrent && hubBranchLatest && _hubAutoUpgrade));
            if (!isCurrent && hubBranchLatest && _isAdmin && !hubIsUpgrading && !hubQueued) {
              hubUpgradeBtn = ' <button id="hub-upgrade-btn" onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem;margin-left:6px;white-space:nowrap">Upgrade</button>';
            } else if (hubIsUpgrading) {
              hubUpgradeBtn = ' <span title="Upgrading to ' + esc(hubBranchLatest || '?') + '" style="display:inline-block;padding:2px 8px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;opacity:0.8"><span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Upgrading</span>';
            } else if (hubQueued) {
              // While the image is still building we do NOT offer click-to-upgrade-now
              // (there is no built image to roll to); once built, an admin can click.
              var hubQueuedTitle = hubImageBuilding
                ? 'Image for ' + esc(hubBranchLatest || '?') + ' is still building — upgrade will be available once it is published'
                : 'Auto-upgrade will apply ' + esc(hubBranchLatest || '?') + ' shortly' + (_isAdmin ? ' — click to upgrade now' : '');
              var hubQueuedClickable = _isAdmin && !hubImageBuilding;
              hubUpgradeBtn = ' <span title="' + hubQueuedTitle + '"' + (hubQueuedClickable ? ' onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="cursor:pointer;' : ' style="') + 'display:inline-block;padding:2px 8px;background:var(--surface);color:var(--muted);border:1px dashed var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap">queued</span>';
            } else if (hubLatestUnknown && _isAdmin) {
              hubUpgradeBtn = ' <button disabled title="Waiting for latest version…" style="padding:2px 8px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</button>';
            }
            if (isCurrent) { _hubUpgrading = false; }
            var hubAutoCheck = '';
            if (_isAdmin) {
              hubAutoCheck = ' <label style="margin-left:6px;font-size:0.6rem;color:var(--muted);cursor:pointer;white-space:nowrap" title="Auto-upgrade hub when a new image is available"><input type="checkbox" ' + (_hubAutoUpgrade ? 'checked' : '') + ' onchange="toggleHubAutoUpgrade(this.checked)" style="vertical-align:middle;margin-right:2px;cursor:pointer">auto</label>';
              /* Upgrade kill switches (admin-only): checked = paused. Titles
                 carry who/when; the dashboard banner repeats it prominently.
                 Rendered as ONE compact bordered column so the pair reads as a
                 control cluster, stays vertically aligned with the header row,
                 and wraps as a unit instead of the second toggle dangling onto
                 its own line. */
              hubAutoCheck += ' <span style="display:inline-flex;flex-direction:column;align-items:flex-start;gap:2px;margin-left:8px;padding:3px 8px;border:1px solid var(--border);border-radius:6px;background:var(--surface);vertical-align:middle;line-height:1.2">' +
                upgradePauseToggleHTML('hub', 'hub upgrades',
                'Kill switch: freeze the hub on its current build — no self-upgrades (auto or manual) until resumed') +
                upgradePauseToggleHTML('spokes', 'spoke upgrades',
                'Kill switch: freeze ALL automatic image changes to spokes, fleet-wide, until resumed') +
                '</span>';
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
        var requestBtn = document.getElementById('btn-request-hive');
        if (addBtn) {
          addBtn.disabled = !canCreate;
          addBtn.title = canCreate ? '' : 'No hosted quota — contact hub admin';
          /* No hosted quota: hide the dead-end self-create button entirely and
             surface the active "Request a hive" link in its place. With quota,
             keep the working + Add Hosted Hive button and hide the request link. */
          addBtn.style.display = canCreate ? '' : 'none';
        }
        if (requestBtn) {
          requestBtn.style.display = canCreate ? 'none' : '';
        }
        /* Render through sortedDashHives() rather than the raw payload so an
           active sort — whether the operator clicked a column or restored a
           saved view that carries one — survives each poll instead of snapping
           back to server order every REFRESH cycle. With no sort key set this
           returns _allDashHives unchanged, i.e. the previous behaviour. */
        renderHives(sortedDashHives());
        /* Persist AFTER a successful render: a payload we could not render must
           never be cached, or the next page load would replay the same crash
           from localStorage before the network could correct it. */
        writeHivesCache(data.hives || []);
        renderUpgradePauseBanner();
        renderPendingBanner(data.hives || []);
        renderUserAccessBanner();
        renderProvisionRequestBanner(data.my_provision_request || null);
        renderAdminProvisionRequests(data.provision_requests || []);
        loadPublicHives(data.hives || []);
        loadUsage();
        loadImagePulls();
      } catch(e) {
        /* Surface EVERY failure in the load→render path, not just fetch errors.
           The old guard only painted a message when _allDashHives was empty —
           but _allDashHives is assigned from the payload BEFORE renderHives()
           runs, so a throw inside rendering left the container showing
           "Loading your hives..." forever with nothing in the console. That is
           how an unbalanced brace in the inline JS (which silently nested a
           third of the dashboard's top-level declarations inside
           renderAlertRows) reached production unnoticed. Never swallow: log the
           real error AND give the operator a way to retry. */
        console.error('[hive] my-hives load/render failed:', e);
        renderHivesError(e);
      } finally {
        _hivesLoading = false;
      }
    }

    /* loadImagePulls fetches the per-RELEASE pull series and paints (a) a small
       inline-SVG bar chart near the header for the ACTIVE release line — the
       line the "stable" channel currently resolves to, reported by the server
       as data.line, so a v4→v5 rollover re-labels and re-buckets the chart with
       no frontend change — and (b) a mini per-line chart in each
       "Latest available images" row from data.lines. One bar per release,
       newest on the right. Gauges external adoption of the public spoke image
       (ghcr.io/hivecommons/hive) beyond the hosted fleet. Honest labelling:
       derived from GitHub's cumulative download counter (pulls, NOT unique
       downloads; GitHub publishes one package-wide counter, not per-tag). Cold
       start (fewer than two release snapshots → no window can be closed yet)
       shows "collecting…". */
    var _imagePullLines = {};   // branch → per-line series from /api/hub/image-pulls

    /* pullBarsSVG renders a per-release pull series as an inline SVG bar
       chart, newest bar highlighted, per-bar <title> tooltips with the exact
       numbers. Shared by the header widget and the per-row mini charts so the
       two can never drift apart visually. */
    function pullBarsSVG(points, BAR_W, BAR_GAP, BAR_H) {
      var BAR_PAD = 2; // px, top breathing room so the tallest bar isn't clipped
      var vals = points.map(function(p) { return Math.max(0, Number(p.pulls) || 0); });
      var maxV = Math.max.apply(null, vals);
      if (maxV <= 0) maxV = 1; // avoid divide-by-zero on an all-zero window
      var chartW = points.length * BAR_W + (points.length - 1) * BAR_GAP;
      var usableH = BAR_H - BAR_PAD;
      var bars = '';
      points.forEach(function(p, i) {
        var v = vals[i];
        var h = Math.max(1, (v / maxV) * usableH); // min 1px so a zero-pull release is still visible
        var x = i * (BAR_W + BAR_GAP);
        var y = BAR_H - h;
        var sha = esc(String(p.sha || ''));
        var tip = sha + ': ' + v + ' pulls' + (p.date ? ' (since ' + esc(String(p.date)) + ')' : '');
        // The newest bar (last) is highlighted; older ones muted.
        var fill = (i === points.length - 1) ? '#60a5fa' : 'rgba(96,165,250,0.45)';
        bars += '<rect x="' + x.toFixed(1) + '" y="' + y.toFixed(1) + '" width="' + BAR_W + '" height="' + h.toFixed(1) + '" rx="1.5" fill="' + fill + '"><title>' + tip + '</title></rect>';
      });
      return '<svg width="' + chartW + '" height="' + BAR_H + '" viewBox="0 0 ' + chartW + ' ' + BAR_H + '" ' +
        'style="display:block;overflow:visible">' + bars + '</svg>';
    }

    /* linePullSparkHTML is one branch row's mini pulls chart: compact bars plus
       the newest release's count, wrapped in a tooltip with the exact totals.
       No data for the line (a retired line pre-history, or a freshly cut line
       like v5 with no closed release window yet) renders a muted "—". */
    function linePullSparkHTML(br) {
      var s = _imagePullLines[br];
      var points = (s && s.points) || [];
      if (!s || s.collecting || points.length < 1) {
        return '<span style="color:var(--muted);opacity:0.5;font-size:0.65rem;cursor:help" title="No image-pull data for this line yet — a bar appears once two releases have been published on it">—</span>';
      }
      var latest = Number(s.latest) || 0;
      var total = Number(s.total_window) || 0;
      var tip = 'Image pulls per ' + esc(br) + ' release: ' + latest + ' on ' +
        esc(String(points[points.length - 1].sha || '')) + ' \u00b7 ' + total +
        ' over last ' + points.length + ' release' + (points.length === 1 ? '' : 's') +
        ' (package-wide pulls landing during each release window)';
      return '<span style="display:inline-flex;align-items:center;gap:4px" title="' + escAttr(tip) + '">' +
        pullBarsSVG(points, 5, 2, 14) +
        '<span style="font-size:0.65rem;color:#60a5fa;font-variant-numeric:tabular-nums">' + esc(String(latest)) + '</span>' +
        '</span>';
    }

    /* fillLinePullSparks refreshes every rendered .line-pulls placeholder from
       the cached series — called after each loadImagePulls fetch so the row
       charts update even though the rows themselves rendered earlier in the
       poll from the (possibly stale) cache. */
    function fillLinePullSparks() {
      var spans = document.querySelectorAll('.line-pulls');
      for (var i = 0; i < spans.length; i++) {
        spans[i].innerHTML = linePullSparkHTML(spans[i].getAttribute('data-branch') || '');
      }
    }

    async function loadImagePulls() {
      var host = document.getElementById('image-pulls-spark');
      if (!host) return;
      var data;
      try {
        var resp = await fetch('/api/hub/image-pulls');
        if (!resp.ok) { host.style.display = 'none'; return; }
        data = await resp.json();
      } catch (e) {
        /* Adoption chart is non-critical chrome — never let it break the
           dashboard. Just leave it hidden on any failure. */
        host.style.display = 'none';
        return;
      }
      _imagePullLines = (data && data.lines) || {};
      fillLinePullSparks();
      var points = (data && data.points) || [];
      /* The server names the ACTIVE line (stable channel's branch). Fall back
         to a lineless label rather than guessing a branch. */
      var line = (data && data.line) ? esc(String(data.line)) : '';
      var lineLabel = 'Pulls per ' + (line ? line + ' ' : '') + 'release';
      host.style.display = 'block';

      /* Cold start: needs at least two release snapshots to close one window. */
      if ((data && data.collecting) || points.length < 1) {
        host.innerHTML = '<div style="font-size:0.7rem;color:var(--muted)">Image ' + lineLabel.toLowerCase() + ': <span style="opacity:0.7">collecting… (a bar appears once a second release is published)</span></div>';
        return;
      }

      /* Bar-chart geometry. Named so there are no bare magic numbers below. */
      var BAR_W = 14;      // px, per-release bar width
      var BAR_GAP = 3;     // px, gap between bars
      var BAR_H = 30;      // px, drawing height

      var svg = pullBarsSVG(points, BAR_W, BAR_GAP, BAR_H);

      var latest = (data && Number(data.latest)) || 0;
      var total = (data && Number(data.total_window)) || 0;
      var newestSHA = points.length ? esc(String(points[points.length - 1].sha || '')) : '';
      host.innerHTML =
        '<div style="display:flex;align-items:center;gap:10px">' +
          '<div style="font-size:0.7rem;color:var(--muted);white-space:nowrap">' + lineLabel + '</div>' +
          svg +
          '<div style="font-size:0.7rem;color:var(--muted);white-space:nowrap">' +
            '<span style="color:#60a5fa;font-weight:600">' + esc(String(latest)) + '</span> on ' + newestSHA +
            ' &middot; ' + esc(String(total)) + ' over last ' + points.length + ' releases' +
          '</div>' +
        '</div>';
    }

    /* renderHivesError replaces the eternal spinner with an actionable message.
       Written with DOM APIs + addEventListener rather than innerHTML with an
       inline handler so the error text can never be interpreted as markup. */
    function renderHivesError(err) {
      var container = document.getElementById('hives-container');
      if (!container) return;
      /* Keep a good list on screen if we already have one — a failed refresh
         should not blank out the fleet the operator is looking at. */
      if (_allDashHives.length && !/Loading your hives/.test(container.innerHTML) &&
          !container.querySelector('.hives-load-error')) {
        return;
      }
      var box = document.createElement('div');
      box.className = 'empty-state hives-load-error';
      var title = document.createElement('p');
      title.style.cssText = 'font-size:1.1rem;margin-bottom:8px;color:var(--red)';
      title.textContent = 'Could not load your hives';
      var detail = document.createElement('p');
      detail.style.cssText = 'font-size:0.8rem;color:var(--muted);word-break:break-word';
      detail.textContent = (err && (err.message || String(err))) || 'Unknown error';
      var hint = document.createElement('p');
      hint.style.cssText = 'font-size:0.75rem;color:var(--muted);opacity:0.8;margin-top:6px';
      hint.textContent = 'Details were logged to the browser console.';
      var wrap = document.createElement('p');
      wrap.style.marginTop = '12px';
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'filter-chip filter-chip-clear';
      btn.textContent = 'Retry';
      btn.addEventListener('click', function() {
        container.innerHTML = '<div class="loading">Loading your hives...</div>';
        /* Force the next render past the signature short-circuit — the cached
           signature may still match the payload that failed to render. */
        _lastHivesJSON = '';
        loadHives();
      });
      wrap.appendChild(btn);
      box.appendChild(title);
      box.appendChild(detail);
      box.appendChild(hint);
      box.appendChild(wrap);
      container.innerHTML = '';
      container.appendChild(box);
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
          // Link on the hive's own GitHub instance (github_host) — a GHE repo must
          // point at github.ibm.com, not 404 on github.com. Falls back to plain
          // text when there is no org/repo to build a valid link from.
          var repoHref = ghRepoURL(hiveForgeHost(h), h.org, h.primaryRepo);
          var repoLink = repoPath ? (repoHref ? '<a href="' + repoHref + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '<span class="repo-link">' + esc(h.primaryRepo) + '</span>') : '';
          var actionCell = '';
          var access = accessMap[h.id];
          // /contribute is a PUBLIC endpoint (see publicPaths) — lending compute
          // does NOT require access — so Contribute is the PRIMARY action for
          // EVERYONE on a public hive, regardless of access status. Use the
          // hive's heartbeat-reported dashboard URL (resolvedBase), NOT a
          // hardcoded <id>.hive.kubestellar.io host. Firewalled spokes (e.g.
          // the heartbeat-only cluster on its OpenShift-route domain) live on a different
          // domain, so the hardcoded host 503'd/failed to resolve.
          var cBase = resolvedBase(h);
          var contributeAction = cBase
            ? '<a href="' + cBase + '/contribute" target="_blank" style="padding:3px 10px;background:rgba(34,197,94,0.15);color:#4ade80;border:1px solid rgba(34,197,94,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none">Contribute</a>'
            : '<span style="padding:3px 10px;color:var(--muted);font-size:0.7rem" title="hive has not reported its dashboard URL yet">Contribute unavailable</span>';
          // Access is the SECONDARY, less-prominent action: a small link for the
          // user who genuinely wants sign-in / manage access. Pending state is
          // preserved (no re-request while one is outstanding).
          var accessSecondary;
          if (access && access.status === 'accepted') {
            accessSecondary = '<span style="font-size:0.65rem;color:var(--green)" title="you have access to this hive">✓ access</span>';
          } else if (access && access.status === 'pending') {
            accessSecondary = '<span style="font-size:0.65rem;color:#fbbf24" title="access request pending">Pending</span>';
          } else {
            accessSecondary = '<a href="#" onclick="dashRequestAccess(\'' + esc(h.id) + '\',this);return false" style="font-size:0.65rem;color:#60a5fa;text-decoration:none" title="request sign-in / manage access to this hive">Request access</a>';
          }
          actionCell = '<div style="display:flex;gap:8px;align-items:center;justify-content:flex-end">' + contributeAction + accessSecondary + '</div>';
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
      if (h.provStatus === 'available') return true;
      /* An assigned slot is NOT inventory, even while its org still reads
         "available-<id>". The assign path writes the real org/repo to meta
         immediately, but entry.org is spoke-reported and only catches up on the
         next heartbeat + config adoption. Keying on the org prefix alone parked
         a freshly-approved hive under "Unassigned hives" for that whole window,
         which reads as the approval having silently failed. provStatus and
         assignedUnclaimed both come from meta, so either one settles it the
         instant the approval lands. */
      if (h.provStatus === 'assigned' || h.assignedUnclaimed) return false;
      return !!h.org && h.org.indexOf('available-') === 0;
    }

    /* hiveNamespace returns the Kubernetes namespace a hosted hive's spoke
       runs in, or '' for a non-hosted (local) hive, which has no such
       namespace. Mirrors hostedNamespaceForHive in
       pkg/hub/hosted_namespace_identity.go — "hive-hosted-" + id — computed
       here rather than shipped as its own field so it can never disagree
       with the one place (server-side) that decides the namespace name. */
    function hiveNamespace(h) {
      if (!h || !h.id) return '';
      var isHosted = h.hiveType === 'hosted' || h.id.indexOf('hosted-') === 0 || h.id.indexOf('saas-') === 0;
      if (!isHosted) return '';
      return 'hive-hosted-' + h.id;
    }

    /* ---------- Bulk multi-select actions ----------
       _bulkSelected is the authoritative selection, keyed by hive id, so it
       survives re-render, re-sort and filter changes: the checkbox state is
       derived from this map on every render rather than read back out of the
       DOM. Ids that are no longer visible are pruned in syncBulkSelection so a
       hidden hive can never be swept into a bulk action the user cannot see. */
    var _bulkSelected = {};

    /* Client-side mirror of the server's maxBulkHivesPerRequest. The server is
       the enforcing authority; this only gives a better message than a 400. */
    var BULK_MAX_HIVES = 100;

    /* Actions that disrupt a running hive and therefore require confirmation
       naming the count. Auto-upgrade toggles only change a stored preference,
       so they are not in this set. */
    var BULK_DISRUPTIVE_ACTIONS = {'restart': 1, 'upgrade': 1, 'switch-branch': 1};

    /* Only hosted hives the user owns (or any hosted hive, for the hub admin)
       can be targeted — the same predicate the single-hive buttons use. Local
       hives have no hub-managed deployment to restart, and unassigned
       placeholders have nothing running yet. */
    function isBulkEligible(h) {
      if (!h || !h.id) return false;
      if (isPlaceholderHive(h)) return false;
      var isHosted = h.hiveType === 'hosted' || (h.id.indexOf('hosted-') === 0 || h.id.indexOf('saas-') === 0);
      if (!isHosted) return false;
      return h.role === 'owner' || _isAdmin;
    }

    function bulkSelectedIds() {
      var out = [];
      for (var k in _bulkSelected) { if (_bulkSelected[k]) out.push(k); }
      return out;
    }

    /* Drop selections for hives that are no longer present/eligible in the
       current view. Called at the top of every render so the count in the bulk
       bar always matches what is actually on screen. */
    function syncBulkSelection(visibleHives) {
      var live = {};
      var list = visibleHives || [];
      for (var i = 0; i < list.length; i++) {
        if (isBulkEligible(list[i])) live[list[i].id] = true;
      }
      for (var k in _bulkSelected) {
        if (!live[k]) delete _bulkSelected[k];
      }
    }

    function toggleBulkHive(id, checked) {
      if (checked) _bulkSelected[id] = true; else delete _bulkSelected[id];
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
    }

    /* Select-all is per section (Assigned / Unassigned) so an admin cannot
       accidentally sweep the whole page from one box. section is the key used
       by the header checkbox id. */
    function toggleBulkSection(section, checked) {
      var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-section="' + section + '"]');
      for (var i = 0; i < boxes.length; i++) {
        var id = boxes[i].getAttribute('data-bulk-id');
        if (!id) continue;
        boxes[i].checked = checked;
        if (checked) _bulkSelected[id] = true; else delete _bulkSelected[id];
      }
      renderBulkBar();
    }

    /* Put each section header box into checked / indeterminate / unchecked to
       match its rows. */
    function syncBulkSectionHeaderBoxes() {
      var heads = document.querySelectorAll('input[type=checkbox][data-bulk-section-head]');
      for (var i = 0; i < heads.length; i++) {
        var section = heads[i].getAttribute('data-bulk-section-head');
        var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-section="' + section + '"]');
        var total = boxes.length, sel = 0;
        for (var j = 0; j < boxes.length; j++) {
          if (_bulkSelected[boxes[j].getAttribute('data-bulk-id')]) sel++;
        }
        heads[i].checked = total > 0 && sel === total;
        heads[i].indeterminate = sel > 0 && sel < total;
      }
    }

    function clearBulkSelection() {
      _bulkSelected = {};
      var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-id]');
      for (var i = 0; i < boxes.length; i++) { boxes[i].checked = false; }
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
    }

    /* Row checkbox markup. section scopes it to a select-all group. */
    function bulkCheckboxCell(h, section) {
      if (!isBulkEligible(h)) {
        return '<td style="width:26px;text-align:center"></td>';
      }
      var checked = _bulkSelected[h.id] ? ' checked' : '';
      return '<td style="width:26px;text-align:center">' +
        '<input type="checkbox" data-bulk-id="' + esc(h.id) + '" data-bulk-section="' + esc(section) + '"' + checked +
        ' onclick="event.stopPropagation()" onchange="toggleBulkHive(\'' + esc(h.id) + '\',this.checked)"' +
        ' title="Select for bulk actions" style="cursor:pointer"></td>';
    }

    function bulkSectionCheckbox(section) {
      /* stopPropagation because the section-header row is itself a click target
         that expands/collapses the section — without this, ticking select-all
         would also collapse the very rows it just selected. */
      return '<input type="checkbox" data-bulk-section-head="' + esc(section) + '"' +
        ' onclick="event.stopPropagation()"' +
        ' onchange="toggleBulkSection(\'' + esc(section) + '\',this.checked)"' +
        ' title="Select all in this section" style="cursor:pointer;margin-right:8px;vertical-align:middle">';
    }

    function renderBulkBar() {
      var bar = document.getElementById('bulk-action-bar');
      if (!bar) return;
      var ids = bulkSelectedIds();
      if (!ids.length) { bar.style.display = 'none'; bar.innerHTML = ''; return; }
      var btn = 'padding:5px 12px;border-radius:4px;cursor:pointer;font-size:0.72rem;white-space:nowrap;border:1px solid var(--border);background:var(--surface);color:var(--text)';
      var over = ids.length > BULK_MAX_HIVES;
      var branchOpts = '';
      var branches = _trackedBranchesList.length > 0 ? _trackedBranchesList : Object.keys(_latestSHAs);
      for (var bi = 0; bi < branches.length; bi++) {
        branchOpts += '<option value="' + esc(branches[bi]) + '">' + esc(branches[bi]) + '</option>';
      }
      var branchPicker = branches.length > 0
        ? '<select id="bulk-branch-select" style="' + btn + ';padding:4px 8px"><option value="">Switch branch…</option>' + branchOpts + '</select>'
        : '';
      bar.innerHTML =
        '<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:14px;padding:10px 14px;' +
        'background:rgba(59,130,246,0.08);border:1px solid rgba(59,130,246,0.3);border-radius:8px">' +
        '<strong style="font-size:0.8rem;color:#60a5fa">' + ids.length + (ids.length === 1 ? ' hive' : ' hives') + ' selected</strong>' +
        (over ? '<span style="font-size:0.72rem;color:var(--red)">Over the ' + BULK_MAX_HIVES + '-hive limit — deselect some.</span>' : '') +
        '<span style="flex:1"></span>' +
        '<button type="button" onclick="runBulkAction(\'restart\')" style="' + btn + '">Restart</button>' +
        '<button type="button" onclick="runBulkAction(\'upgrade\')" style="' + btn + '">Upgrade to latest</button>' +
        '<button type="button" onclick="runBulkAction(\'enable-auto-upgrade\')" style="' + btn + '">Auto: instant</button>' +
        '<button type="button" onclick="runBulkAction(\'daily-auto-upgrade\')" style="' + btn + '" title="Upgrade at most once a day, midday — keeps a stable hive from being restarted mid-work, and puts a bad roll in staffed hours">Auto: daily 1pm ET</button>' +
        '<button type="button" onclick="runBulkAction(\'weekly-auto-upgrade\')" style="' + btn + '" title="Upgrade at most once a week, Tuesday midday — the least disruptive cadence that still keeps the hive current">Auto: Tue 1pm ET</button>' +
        '<button type="button" onclick="runBulkAction(\'disable-auto-upgrade\')" style="' + btn + '">Auto-upgrade off</button>' +
        branchPicker +
        '<button type="button" onclick="clearBulkSelection()" style="' + btn + ';color:var(--muted)">Clear</button>' +
        '</div>';
      bar.style.display = '';
      var sel = document.getElementById('bulk-branch-select');
      if (sel) {
        sel.onchange = function() {
          var b = sel.value;
          sel.value = '';
          if (b) runBulkAction('switch-branch', b);
        };
      }
    }

    var BULK_ACTION_LABELS = {
      'restart': 'Restart',
      'upgrade': 'Upgrade to latest',
      'enable-auto-upgrade': 'Enable instant auto-upgrade on',
      'daily-auto-upgrade': 'Enable daily 1pm ET auto-upgrade on',
      'weekly-auto-upgrade': 'Enable weekly Tuesday 1pm ET auto-upgrade on',
      'disable-auto-upgrade': 'Disable auto-upgrade on',
      'switch-branch': 'Switch branch for'
    };

    async function runBulkAction(action, branch) {
      var ids = bulkSelectedIds();
      if (!ids.length) { hiveToast('No hives selected', 'error'); return; }
      if (ids.length > BULK_MAX_HIVES) {
        hiveToast('Select at most ' + BULK_MAX_HIVES + ' hives per bulk action', 'error');
        return;
      }
      var label = BULK_ACTION_LABELS[action] || action;
      var noun = ids.length === 1 ? 'hive' : 'hives';
      /* Confirmation names BOTH the action and the count — restarting a batch
         of live hives must never be one misclick. Disruptive actions also list
         the hives so the user can see exactly what is in the batch. */
      if (BULK_DISRUPTIVE_ACTIONS[action]) {
        var names = ids.slice(0, 10).map(function(i) { return esc(i); }).join('<br>');
        if (ids.length > 10) names += '<br>… and ' + (ids.length - 10) + ' more';
        var msg = '<strong>' + esc(label) + (branch ? ' ' + esc(branch) : '') + '</strong> on <strong>' +
          ids.length + ' ' + noun + '</strong>?<br><br>' +
          '<div style="font-family:monospace;font-size:0.75rem;color:var(--muted);text-align:left;max-height:180px;overflow:auto">' + names + '</div>';
        if (!await hiveConfirm(msg, true)) return;
      }
      hiveToast(label + ' — ' + ids.length + ' ' + noun + '…', 'info');
      try {
        var resp = await fetch('/api/saas/hives/bulk', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({action: action, hive_ids: ids, branch: branch || ''})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast((data && data.error) || 'Bulk action failed', 'error'); return; }
        showBulkResults(label, data);
        clearBulkSelection();
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
      }
    }

    /* Partial failure is the normal case, so always show the per-hive
       breakdown rather than a single success toast: the user needs to know
       WHICH hives failed and why. */
    function showBulkResults(label, data) {
      var results = (data && data.results) || [];
      var ok = data.succeeded || 0, failed = data.failed || 0;
      var failedRows = '';
      var heartbeatCount = 0;
      for (var i = 0; i < results.length; i++) {
        var r = results[i] || {};
        if (r.ok) { if (r.via === 'heartbeat') heartbeatCount++; continue; }
        failedRows += '<div style="display:flex;gap:8px;padding:3px 0;font-size:0.72rem">' +
          '<span style="font-family:monospace;color:var(--red);flex:none">' + esc(r.hive_id || '?') + '</span>' +
          '<span style="color:var(--muted)">' + esc(r.error || 'failed') + '</span></div>';
      }
      if (!failed) {
        var extra = heartbeatCount
          ? ' (' + heartbeatCount + ' queued for delivery on next check-in)'
          : '';
        hiveToast(label + ': ' + ok + ' succeeded' + extra, 'success');
        return;
      }
      hiveConfirm('<strong>' + esc(label) + '</strong><br><br>' +
        '<span style="color:var(--green)">' + ok + ' succeeded</span>' +
        (heartbeatCount ? ' <span style="color:var(--muted);font-size:0.75rem">(' + heartbeatCount + ' via heartbeat)</span>' : '') +
        ' &nbsp; <span style="color:var(--red)">' + failed + ' failed</span>' +
        '<div style="margin-top:10px;text-align:left;max-height:220px;overflow:auto">' + failedRows + '</div>', true);
    }

    /* True while any row's branch/channel dropdown is open. renderHives
       rebuilds the row DOM, which would destroy the open menu mid-click —
       the fleet heartbeats change some hive field on nearly every poll, so
       without this guard the dropdown closed itself within seconds of
       opening. */
    function branchMenuOpen() {
      var menus = document.querySelectorAll('[id^="branch-menu-"]');
      for (var i = 0; i < menus.length; i++) {
        if (menus[i].style.display !== 'none') return true;
      }
      return false;
    }
    /* One phrase answering WHO paused an agent, WHY and WHEN, from the pause
       provenance the spoke reports over the heartbeat (#4041) — e.g.
       "paused by bketelsen via dashboard, 3d ago" or "paused (login-detector:
       login required detected), 2h ago". A deliberate owner quiesce must read
       differently from a malfunction in the fleet view. Old cached rows and
       old spokes lack the fields; falls back to a bare "paused". */
    function agentPauseProvenance(a) {
      var line;
      if (a.pausedTrigger === 'dashboard-api') {
        line = 'paused by ' + (a.pausedBy || 'an operator') + ' via dashboard';
      } else if (a.pausedTrigger) {
        line = 'paused (' + a.pausedTrigger + (a.pausedReason ? ': ' + a.pausedReason : '') + ')';
      } else {
        line = 'paused';
      }
      if (a.pausedAt) {
        /* Self-contained relative age (coarse on purpose): timelineAgo lives
           in a later <script> block, and renderHives can run during this
           block's init (paintCachedHives) before that block is parsed. */
        var t = Date.parse(a.pausedAt);
        if (!isNaN(t)) {
          var PAUSE_MS_PER_MIN = 60000, PAUSE_MIN_PER_HOUR = 60, PAUSE_HOURS_PER_DAY = 24;
          var mins = Math.floor((Date.now() - t) / PAUSE_MS_PER_MIN);
          if (mins < 1) line += ', just now';
          else if (mins < PAUSE_MIN_PER_HOUR) line += ', ' + mins + 'm ago';
          else if (mins < PAUSE_MIN_PER_HOUR * PAUSE_HOURS_PER_DAY) line += ', ' + Math.floor(mins / PAUSE_MIN_PER_HOUR) + 'h ago';
          else line += ', ' + Math.floor(mins / (PAUSE_MIN_PER_HOUR * PAUSE_HOURS_PER_DAY)) + 'd ago';
        }
      }
      return line;
    }
    function renderHives(allHives, force) {
      allHives = allHives || [];
      /* Defer the whole render while a branch/channel menu is open. Skipping
         BEFORE the signature is stored means the change is not swallowed: the
         next render call (poll tick, or the catch-up fired when the menu
         closes) sees a stale _lastHivesJSON and repaints normally. */
      if (branchMenuOpen()) return;
      /* The signature must include EVERY piece of render-affecting view state,
         otherwise changing it while the hive data is unchanged is silently a
         no-op — toggling a chip, drilling into an alert type, expanding the
         acknowledged list, typing in the search box, picking a facet,
         collapsing a section, switching group-by, collapsing a group or
         applying a saved view would all appear to do nothing. */
      var sig = JSON.stringify(allHives) + '|' + JSON.stringify(_dashStatusFilters) +
        '|' + _dashFailingCheckFilter + '|' + _dashDriftFilter + '|' + _dashUpgradeFilter +
        '|' + _dashAdvisoryStaleFilter +
        '|' + _alertTypeFilter + '|' + _alertShowAcked + '|' + JSON.stringify(_fleetAlerts) +
        /* Without this, expanding "+N more" changes no hive data and the
           early-return below would make the click a silent no-op. */
        '|' + JSON.stringify(_alertRowsExpanded) +
        '|' + _dashSearchQuery + '|' + JSON.stringify(_dashFacets) +
        '|' + JSON.stringify(_dashFacetCollapsed) + '|' + JSON.stringify(_dashSectionCollapsed) +
        '|' + _dashGroupBy + '|' + JSON.stringify(_dashGroupCollapsed) +
        '|' + _dashActiveView + '|' + _dashDefaultView + '|' + JSON.stringify(_dashSavedViews) +
        '|' + _dashFacetTrayOpen;
      if (!force && sig === _lastHivesJSON) return;
      _lastHivesJSON = sig;
      /* Status/facet filters describe ASSIGNED hives only. An unassigned
         placeholder has no GitHub App, no tokens and no real health to speak of,
         so every chip would appear to "hide" the whole pool — and filtering to
         e.g. Degraded made the Unassigned section vanish, which reads as the
         placeholders having been deleted. The free-text search is different:
         it promises to filter the displayed rows by visible metadata, so it
         also scopes placeholders by name/id/cluster/repo instead of leaving
         unrelated inventory visible while the header says a search is active. */
      var assignedAll = [], unassignedAll = [];
      for (var _si = 0; _si < allHives.length; _si++) {
        (isPlaceholderHive(allHives[_si]) ? unassignedAll : assignedAll).push(allHives[_si]);
      }
      /* Expire client-side upgrade sentinels ONCE, before anything reads them.
         Filtering, facet counting and row drawing are all pure readers of
         _upgradingHives from here on, so they cannot observe each other's
         mutations and the pill can no longer disagree with the badge. */
      normalizeUpgradeStates(assignedAll);
      var filteredAssigned = applyDashFilters(assignedAll);
      var filteredUnassigned = (_dashSearchQuery || '').trim()
        ? unassignedAll.filter(hiveMatchesSearch)
        : unassignedAll;
      var hives = filteredAssigned.concat(filteredUnassigned);
      var filterBar = document.getElementById('hive-filter-bar');
      if (filterBar) filterBar.style.display = allHives.length ? '' : 'none';
      var searchRow = document.getElementById('hive-search-row');
      if (searchRow) searchRow.style.display = allHives.length ? '' : 'none';
      /* Nothing renderable below means no rows to act on — drop the selection
         so the bulk bar can't linger over an empty or fully-filtered table. */
      if (!allHives.length || !hives.length) {
        _bulkSelected = {};
        renderBulkBar();
      }
      /* The view bar shows whenever there are hives at all — including when the
         status filters currently match none of them, so the operator can still
         switch grouping or restore a saved view from the empty state. */
      var viewBar = document.getElementById('hive-view-bar');
      if (viewBar) viewBar.style.display = allHives.length ? '' : 'none';
      renderViewBar();
      /* Counts are over the assigned set only, matching what the chips filter. */
      renderStatusFilterBar(assignedAll, filteredAssigned.length);
      /* Facets are offered over the assigned set for the same reason the chips
         are: a placeholder carries no cluster, role or branch worth faceting. */
      renderFacetRail(assignedAll);
      /* Drift exceptions are scoped to assigned hives for the same reason: a
         placeholder is never flagged for claimed-hive concerns server-side. */
      renderDriftSummary(assignedAll);
      /* Drawn BEFORE the empty-state early-returns below: a fleet whose every
         hive is filtered out still has alerts worth showing, and the panel is
         how the operator gets back out of a drill-down. */
      renderSummaryTiles();
      renderAlertsPanel();
      if (!allHives.length) {
        var driftEl0 = document.getElementById('hive-drift-summary');
        if (driftEl0) driftEl0.style.display = 'none';
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives yet</p>' +
          '<p style="margin-bottom:16px">Get started in one of two ways:</p>' +
          '<div style="display:flex;gap:12px;justify-content:center;flex-wrap:wrap;margin-bottom:12px">' +
          /* Request-a-hive → the EXISTING Request-a-Hive wizard at /get-started
             (POST /api/saas/request-provision). We host it for you. */
          '<a class="btn-primary" href="/get-started" style="text-decoration:none" title="We host the hive for you">Request a hive</a>' +
          /* Register-your-own-hive → the EXISTING self-host guide at
             /get-started#self-host. Not self-service: the operator must issue
             the spoke a HIVE_HUB_SECRET or its heartbeats 401 (see #1077 and
             the note on the other copy of this CTA above). */
          '<a class="btn-primary" href="/get-started#self-host" style="background:var(--surface);color:var(--text);border:1px solid var(--border);text-decoration:none" title="You self-host the hive and attach it to this hub">Register your own hive</a>' +
          '</div>' +
          '<p style="color:var(--muted);font-size:0.85rem">…or contribute to a public hive below.</p>' +
          '</div>';
        return;
      }
      if (!hives.length) {
        /* Hives exist, but every one was filtered out — say so, and offer the
           way back rather than looking like the list failed to load. */
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives match these filters</p>' +
          '<p>' + allHives.length + (allHives.length === 1 ? ' hive is' : ' hives are') + ' hidden by the search, facets or status filters.</p>' +
          /* clearAllHiveFilters, not clearStatusFilters: a search term, a facet
             or an alert drill-down can empty the list too, and a button that
             only clears the chips would leave the operator stuck looking at an
             empty table. */
          '<p style="margin-top:12px"><button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear filters</button></p>' +
          '</div>';
        return;
      }
      /* Prune selections for hives that are no longer visible BEFORE building
         rows, so the checkbox state and the bulk bar's count agree. */
      syncBulkSelection(hives);
      var repoPath = function(h) { return h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || ''; };
      var buildRow = function(h, i, section) {
        // While a rollout is in flight, the status dot becomes the distinct
        // blue glowing/slow-blinking "upgrading" dot (see .online-dot.upgrading) —
        // it outranks the normal online/offline health dot so an upgrade reads at
        // a glance and is never mistaken for the ok/degraded/critical/unknown state.
        // The OFFLINE dot (grey, no heartbeat) previously carried no tooltip, so
        // an operator hovering a dead row learned nothing \u2014 not even which
        // Kubernetes namespace the hive lives in. healthBadge() already gives the
        // ONLINE dot a rich hover (status + ns: + heartbeat age); this mirrors the
        // reference facts an operator needs to go find the offline hive on the
        // cluster. Built defensively \u2014 every part is skipped when its field is
        // empty \u2014 and escaped with escAttr since it lands inside a quoted title.
        var offlineDotTitle = (function() {
          var parts = ['Offline'];
          var ns = hiveNamespace(h);            // 'hive-hosted-<id>' or '' for local
          if (ns) parts.push('ns: ' + ns);
          var cluster = h.clusterName || h.clusterId || '';
          if (cluster) parts.push('cluster: ' + cluster);
          if (h.lastHeartbeat) parts.push('last seen: ' + fmtUserTS(h.lastHeartbeat));
          return parts.join('\n');
        })();
        var dot = h.upgrading
          ? '<span class="online-dot upgrading" title="Upgrading \u2014 a rollout is in progress"></span>'
          : (h.online ? healthBadge(h) : '<span class="online-dot off" title="' + escAttr(offlineDotTitle) + '" style="cursor:help"></span>');
        var rp = repoPath(h);
        // Link on the hive's own GitHub instance (github_host) so a GHE repo
        // points at github.ibm.com, not 404 on github.com.
        var repoHref = ghRepoURL(hiveForgeHost(h), h.org, h.primaryRepo);
        var repoLink = rp ? (repoHref ? '<a href="' + repoHref + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '<span class="repo-link">' + esc(h.primaryRepo) + '</span>') : '';
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
          : modeBadge(h.governorMode);
        /* A placeholder wedged at assigned && !claim_delivered gets a subtle
           live-ticking "claim pending · Nm" pill so a stuck assignment is
           obvious. Self-suppresses (empty) for every other row. */
        modeCell += claimPendingPill(h);
        var rb = resolvedBase(h);
        var contributeUrl = rb ? rb + '/contribute' : '';
        var actions = '';
        if (canConvert) {
          actions = '<button onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="padding:3px 10px;background:var(--accent);color:#000;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap">Convert to Hosted</button>';
          if (h.role === 'owner') {
            actions += '<br style="margin-bottom:4px"><button onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="margin-top:6px;padding:3px 10px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;cursor:pointer;font-size:0.65rem;white-space:nowrap" title="Remove from registry (does not delete the hive)">Remove</button>';
          }
        } else if (isHosted && (roleAtLeast(h.role, 'read-write'))) {
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
        /* Reset assignment: escape hatch for a placeholder wedged at
           assigned && !claim_delivered (assignedUnclaimed) — its spoke never
           reported the project back, so it can be neither re-approved (needs a
           pending request) nor re-assigned (needs an available slot). This
           returns the slot to the pool so it can be re-armed. Admin-only,
           mirroring the endpoint's own guard. */
        if (_isAdmin && h.assignedUnclaimed) menuItems.push('<div onclick="resetAssignment(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + ';color:#d29922;font-weight:600">Reset assignment</div><div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (contributeUrl) menuItems.push('<a href="' + contributeUrl + '" target="_blank" style="' + mi + '">Contribute</a>');
        if (h.snapshotUrl) menuItems.push('<a href="' + esc(h.snapshotUrl) + '" target="_blank" style="' + mi + '">Preview</a>');
        var apiBase = rb ? esc(rb) : '';
        if (apiBase) menuItems.push('<a href="' + apiBase + '/api/docs" target="_blank" style="' + mi + '">API Docs</a>');
        if (menuItems.length > 0 && (canConvert || isHosted || isLocal)) menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (canConvert) menuItems.push('<div onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="' + mi + '">Convert to Hosted</div>');
        if (isHosted && (roleAtLeast(h.role, 'read-write'))) menuItems.push('<div onclick="openAccessModal(\'' + esc(h.id) + '\',\'' + esc(h.dashboardUrl || '') + '\')" style="' + mi + '">Permissions</div>');
        /* Timeline is owner-or-admin, matching the API's authorization. */
        if (isHosted && (h.role === 'owner' || _isAdmin)) menuItems.push('<div onclick="openTimelineModal(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Activity Timeline</div>');
        if (roleAtLeast(h.role, 'read-write') || _isAdmin) menuItems.push('<div onclick="openOpenRouterFundModal(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">⚡ Fund with OpenRouter</div>');
        if (_isAdmin && isHosted) menuItems.push('<div onclick="openBannerForHive(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Send Banner</div>');
        /* Reset App clears ONLY the spoke's installation_id, which makes
           HasUsableApp() false and prompts the owner to install the App again.
           Admin-only and hosted-only, matching the endpoint's own guard. The
           case it exists for: a hive provisioned with its own GitHub App keeps
           that App's installation_id after its identity is moved to the fleet
           App, so every field reads correct while freshly minted tokens 404. */
        if (_isAdmin && isHosted) menuItems.push('<div onclick="resetHiveApp(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Reset Forge App</div>');
        /* Restart Spoke rolling-restarts every instance reporting as this
           hive. The case it exists for: two instances alternating as one hive
           (conflictingReporters drift) on a cluster the hub cannot reach —
           the heartbeat is the only channel, and a restart sheds the stale
           instance while costing the healthy one a single rolling restart. */
        if (_isAdmin && isHosted) menuItems.push('<div onclick="restartHiveSpoke(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Restart Spoke</div>');
        if (isLocal && h.role === 'owner') menuItems.push('<div onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="' + mi + '">Remove</div>');
        if (isHosted && h.role === 'owner') menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div><div onclick="deleteHive(\'' + esc(h.id) + '\')" style="' + mi + ';color:#f85149">Delete</div>');
        var sha = h.gitHash || '';
        /* Drift folded ONTO Version: config drift is overwhelmingly "this hive's
           version/branch differs from the fleet", so the drift indicator reads as
           a property of the Version cell rather than earning its own column. Only
           emitted when there IS drift (driftOf().count > 0); driftBadge() returns
           its own colored count pill with the full per-signal hover panel, so no
           datum is lost — hover the dot for every drift reason exactly as before.
           A clean hive shows nothing here, keeping the cell as tight as it was.
           driftBadge carries its own hive-access-pop panel and NO title, so the
           single-hover-panel invariant holds (it is a badge, not an inline face). */
        var driftDot = driftOf(h).count > 0 ? driftBadge(h) : '';
        var versionCell = '';
        if (sha) {
          var branchName = h.gitBranch || 'v2';
          /* Effective version SELECTION, distinct from the running branch. A
             hive following a release channel heartbeats the channel image's
             baked-in branch (a "stable" retag of a v4 build reports
             gitBranch "v4"), so gitBranch alone forgets the channel within
             one beat of the switch — the pill read "v4" on a stable hive.
             trackedChannel is the hub-persisted selection
             (SaaSHive.TrackedChannel: written only by a branch/channel
             switch, never by heartbeats). When set it is what the pill shows
             and what the picker treats as currently selected; branchName
             keeps driving everything about the code actually running
             (latest/behind, drift, upgrade state). */
          var versionSel = h.trackedChannel || branchName;
          var branchLatest = _latestSHAs[branchName] || _latestSHA;
          var _trackedBranches = _trackedBranchesList.length > 0 ? _trackedBranchesList : Object.keys(_latestSHAs);
          if (_trackedBranches.length === 0) _trackedBranches = ['v2'];
          /* A release channel is as valid an upgrade target as a branch — the
             hive gets pinned to the moving tag and follows promotions — so the
             same menu offers both. Channels are listed in their own section
             below the branches, because picking a channel is a different KIND
             of choice (track a promotion policy vs. track a git branch) and
             mixing them into one flat list hides that. */
          var canSwitchBranch = isHosted && h.role === 'owner' &&
            (_trackedBranches.length > 1 || _releaseChannels.length > 0) && !h.upgrading;
          var branchOptions = '';
          if (canSwitchBranch) {
            for (var bi = 0; bi < _trackedBranches.length; bi++) {
              var tb = _trackedBranches[bi];
              /* Compared against the SELECTION, not the running branch: a hive
                 tracking "stable" (currently a v4 image) must still offer v4
                 here — picking it is a real action, un-tracking the channel. */
              if (tb !== versionSel) {
                branchOptions += '<div onclick="event.stopPropagation();switchBranch(\'' + esc(h.id) + '\',\'' + esc(tb) + '\',this)" style="padding:4px 10px;cursor:pointer;font-size:0.65rem;white-space:nowrap;color:#c9d1d9;border-radius:4px" onmouseover="this.style.background=\'rgba(59,130,246,0.2)\'" onmouseout="this.style.background=\'transparent\'">' + esc(tb) + '</div>';
              }
            }
            var chOpts = '';
            for (var cbi = 0; cbi < _releaseChannels.length; cbi++) {
              var cb = _releaseChannels[cbi];
              /* The channel the hive already tracks is the current selection —
                 omit it exactly as the running branch is omitted above. Keyed
                 on versionSel so "stable" reads as selected, not "v4". */
              if (cb === versionSel) continue;
              /* Name what the channel resolves to RIGHT NOW in the hover, so
                 the operator is not picking a word with no visible meaning.
                 Falls back to the channel's own description when unresolved. */
              var cbT = null;
              for (var cti = 0; cti < _channelTargets.length; cti++) {
                if (_channelTargets[cti] && _channelTargets[cti].channel === cb) { cbT = _channelTargets[cti]; break; }
              }
              var cbTitle = cbT && cbT.branch
                ? 'Currently ' + cbT.branch + ' ' + (cbT.sha || '') + ' — the hive follows this channel as it is re-pointed'
                : 'Moving release channel — the hive follows it as it is re-pointed';
              /* Label carries the channel's CURRENT branch — "stable (v4)" —
                 while the switch VALUE stays the bare channel name: the hive
                 is pinned to the moving tag, not to the branch it happens to
                 resolve to today. */
              chOpts += '<div onclick="event.stopPropagation();switchBranch(\'' + esc(h.id) + '\',\'' + esc(cb) + '\',this)" title="' + escAttr(cbTitle) + '" style="padding:4px 10px;cursor:pointer;font-size:0.65rem;white-space:nowrap;color:#4ade80;border-radius:4px" onmouseover="this.style.background=\'rgba(34,197,94,0.2)\'" onmouseout="this.style.background=\'transparent\'">' + esc(versionLabel(cb)) + '</div>';
            }
            if (chOpts) {
              branchOptions += '<div style="border-top:1px solid #30363d;margin:4px 0"></div>' +
                '<div style="padding:2px 10px;font-size:0.55rem;color:var(--muted);text-transform:uppercase;letter-spacing:0.05em">Channels</div>' + chOpts;
            }
          }
          /* Pill text is the SELECTION (versionSel) through versionLabel, so
             a hive tracking a channel reads "stable (v4)" — the channel plus
             the branch it currently resolves to — and keeps reading that
             after the spoke heartbeats its baked-in branch. A plain-branch
             hive renders unchanged. Display-only: versionSel/branchName stay
             bare values, and every switch payload sends the bare name. */
          var branch = canSwitchBranch
            ? '<span id="branch-pill-' + esc(h.id) + '" style="display:inline-block;position:relative;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);cursor:pointer" onclick="toggleBranchMenu(\'' + esc(h.id) + '\')" title="Click to switch branch">' + esc(versionLabel(versionSel)) + ' ▾<div id="branch-menu-' + esc(h.id) + '" style="display:none;position:absolute;top:100%;left:0;margin-top:4px;background:#1c2128;border:1px solid #30363d;border-radius:6px;padding:4px 0;z-index:1000;min-width:60px;box-shadow:0 4px 12px rgba(0,0,0,0.4)">' + branchOptions + '</div></span>'
            : '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(versionLabel(versionSel)) + '</span>';
          var latestUnknown = !branchLatest;
          var isCurrent = branchLatest && sameShaJS(sha, branchLatest);
          /* Branch switch in flight: the hive still reports the OLD branch
             (often current on it) until the new pod heartbeats — without
             this, isCurrent suppresses every progress indicator. */
          /* Switch-vs-upgrade is decided in ONE place (hiveUpgradeState) so the
             spinner, label and title always agree. Only a target on a DIFFERENT
             branch is a switch; a plain-SHA (auto-upgrade) target always reads
             "Upgrading", even if a stale switch sentinel lingers. */
          var upgradeState = hiveSwitchState(h, branchName);
          var isSwitching = upgradeState.isSwitching;
          var targetBranch = upgradeState.targetBranch;
          /* Sentinel expiry used to happen HERE, mid-render. That is what kept
             the pill and the badge disagreeing even after they were given a
             shared predicate: applyDashFilters (and the facet counter) run over
             the whole list BEFORE this loop body executes for any row, so the
             pill read the pre-expiry sentinel map while the row read the
             post-expiry one — and on the next paint the pill read a map mutated
             by the PREVIOUS render. Same function, different inputs, different
             answers. Expiry now happens in normalizeUpgradeState(), once, ahead
             of filtering; this loop is a pure reader. */
          /* ONE predicate, shared with the filter pill (hiveUpgradeState ->
             hiveIsUpgradingNow), so a spinning row is always matched by the
             "Upgrading" facet and vice versa. */
          var isUpgrading = hiveIsUpgradingNow(h, branchName, branchLatest);
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
            /* In-flight state is not clickable, so it loses the box entirely:
               a border around a non-target was the padding that made this the
               tall line. The spinner is the affordance-free progress signal. */
            /* The elapsed counter rides INSIDE the pill so a long-running
               upgrade cannot be read as progress. It self-suppresses when the
               start time is unknown or the rollout is still fast (see
               renderUpgradeElapsed), so a healthy fleet looks unchanged. */
            upgradeIcon = '<span title="' + progressTitle + '" style="font-size:0.7rem;white-space:nowrap;opacity:0.8;color:var(--muted)"><span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:currentColor;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>' + progressLabel + '</span>' + renderUpgradeElapsed(h);
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner' && h.autoUpgrade) {
            /* STATE and ACTION are split. The old single "queued" pill WAS the
               click-to-upgrade affordance, and owners could not find it (real
               feedback: "that line that says 'queued' is the upgrade button —
               it's not straightforward"). The badge now only NAMES the state
               and its target; the manual escape hatch — never gated by the
               schedule — is its own labelled "Upgrade now" action beside it.
               A daily-mode hive is NOT upgrading "shortly" — saying so would be
               a lie the operator would notice hours later, so the window is
               still named on the badge. */
            var queuedDaily = h.autoUpgradeMode === AUTO_UPGRADE_DAILY;
            var queuedLabel = queuedDaily ? 'Queued for auto-upgrade · 1pm ET' : 'Queued for auto-upgrade';
            /* The tooltip names the TARGET — SHA plus the tracked
               branch/channel label — so "queued for what?" never needs a
               second click to answer. */
            var queuedTitle = queuedDaily
              ? 'Auto-upgrade will apply ' + branchLatest + ' (' + versionLabel(versionSel) + ') at the next 1pm ET window' + buildingHint
              : 'Auto-upgrade will apply ' + branchLatest + ' (' + versionLabel(versionSel) + ') shortly' + buildingHint;
            /* REFUSED, not queued. autoUpgradeBlocked is the hub's own evaluated
               decision (upgradeCollectible false), sent by the fleet API rather
               than re-derived here — the badge must never claim an upgrade is
               coming for a hive triggerAutoUpgrades() declines every cycle and
               will keep declining until an operator intervenes. The mode-derived
               label above says nothing about eligibility, which is exactly how a
               hive sat 89 commits behind still advertising "· 1pm ET".
               "Upgrade now" stays beside it: the manual path is pushed through a
               different route and remains the operator's escape hatch. */
            if (h.autoUpgradeBlocked) {
              queuedLabel = 'Auto-upgrade blocked';
              queuedTitle = 'The hub will not arm an auto-upgrade for this hive: ' +
                (h.autoUpgradeBlockedReason || 'it cannot collect the upgrade instruction') +
                '. It will not resolve on its own.' + buildingHint;
            }
            /* escAttr, not esc, for the title: esc() leaves quotes intact and a
               branch name or commit subject carrying one would break out of the
               attribute. jsArg supplies its own quotes for the handler args. */
            upgradeIcon = '<span title="' + escAttr(queuedTitle) + '" style="' + UPGRADE_STATE_BADGE_STYLE + '">' + esc(queuedLabel) + '</span>' +
              '<span id="upgrade-' + escAttr(h.id) + '" role="button" tabindex="0"' +
              ' onclick="upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')"' +
              ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')}"' +
              ' title="' + escAttr('Upgrade to ' + branchLatest + ' now instead of waiting for auto-upgrade' + buildingHint) + '" style="' + UPGRADE_LINK_STYLE + ';color:var(--green);font-weight:600;font-size:0.7rem">Upgrade now</span>';
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner') {
            /* Manual path (auto-upgrade OFF): a real primary-ish button, not
               text-with-an-underline. The bare word "Upgrade" read as a label,
               not an action; naming the target ("Upgrade available → <sha>")
               plus the box, the ↑ glyph and a hover state make it unmissably
               the thing to click. */
            var latestShort = branchLatest ? String(branchLatest).substring(0, 7) : 'latest';
            upgradeIcon = '<span id="upgrade-' + escAttr(h.id) + '" role="button" tabindex="0"' +
              ' onclick="upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')"' +
              ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')}"' +
              ' onmouseover="this.style.background=\'' + UPGRADE_BTN_HOVER_BG + '\'" onmouseout="this.style.background=\'' + UPGRADE_BTN_BG + '\'"' +
              ' title="' + escAttr('Current: ' + sha + ' → Latest: ' + branchLatest + buildingHint) + '" style="' + UPGRADE_BTN_STYLE + '">↑ Upgrade available → ' + esc(latestShort) + '</span>';
          } else if (!isCurrent && !latestUnknown && isHosted) {
            /* Non-owners get the STATE with no action: they cannot trigger the
               upgrade, so a button would be a lie, but saying nothing made a
               behind hive read as current. Passive badge, no handler. */
            upgradeIcon = '<span title="' + escAttr('A newer build is available (' + branchLatest + ') — the hive owner or an admin can trigger the upgrade') + '" style="' + UPGRADE_STATE_BADGE_STYLE + '">Update available</span>';
          } else if (latestUnknown && isHosted && h.role === 'owner') {
            upgradeIcon = '<span title="Waiting for latest version…" style="font-size:0.7rem;color:var(--muted);white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</span>';
          }
          /* Auto-upgrade control: a single select replaces the old checkbox.
             The dense table has no room for a checkbox PLUS a mode dropdown,
             and two controls for one setting invites the illegal-looking
             "off but daily" state. One select shows the effective mode at a
             glance — no dialog, no hover needed — which is the whole point:
             an operator scanning the fleet must be able to see which hives
             restart on sight of a new commit and which wait for the evening.
             The id travels in a data-* attribute and the handler is bound by
             delegation (see bindAutoUpgradeSelects) because esc() does NOT
             escape quotes and is unsafe to interpolate into an attribute. */
          var autoUpgradeCheck = '';
          if (isHosted && h.role === 'owner') {
            var mode = h.autoUpgrade
              ? ((h.autoUpgradeMode === AUTO_UPGRADE_DAILY || h.autoUpgradeMode === AUTO_UPGRADE_WEEKLY) ? h.autoUpgradeMode : AUTO_UPGRADE_INSTANT)
              : AUTO_UPGRADE_OFF;
            var opts = '';
            for (var oi = 0; oi < AUTO_UPGRADE_OPTIONS.length; oi++) {
              var opt = AUTO_UPGRADE_OPTIONS[oi];
              /* Each OPTION carries its own full-meaning title too, so opening
                 the list explains the three icons without a legend elsewhere. */
              opts += '<option value="' + escAttr(opt.value) + '" title="' + escAttr(opt.ariaLabel) + '"' + (mode === opt.value ? ' selected' : '') + '>' + esc(opt.label) + '</option>';
            }
            var d = document.createElement('select');
            d.className = 'auto-upgrade-select';
            d.setAttribute('data-hive-id', h.id);
            /* The label is now icons only, so the meaning has to live in the
               accessible name and the tooltip or it is simply lost. aria-label
               names the CURRENT mode; title explains what the modes are. */
            d.setAttribute('aria-label', autoUpgradeAriaLabel(mode));
            d.title = autoUpgradeAriaLabel(mode) + ' — ' + AUTO_UPGRADE_TITLE;
            d.setAttribute('style', 'font-size:0.65rem;color:var(--muted);background:var(--surface);border:1px solid var(--border);border-radius:4px;' +
              'height:' + AUTO_SELECT_H_PX + 'px;min-width:' + AUTO_SELECT_MIN_W_PX + 'px;padding:0 ' + AUTO_SELECT_PAD_X_PX + 'px;cursor:pointer;vertical-align:middle;line-height:' + AUTO_SELECT_H_PX + 'px');
            d.innerHTML = opts;
            autoUpgradeCheck = d.outerHTML;
          }
          var shaMsg = _commitMessages[sha] || _latestSHAMessages[branchName] || '';
          /* FOUR stacked lines, ONE element per line. The column is as wide as
             its widest LINE, so as long as no two controls share a line the
             width collapses to the widest single element — which, now that the
             select is icon-only, is the 56px select rather than the ~150px
             "[queued · 1pm ET] [Auto: daily 1pm ET]" pair that used to set it.
               1. the branch pill, which the owner can CHANGE (it is a switcher,
                  so it owns a line and stays a full tap target);
               2. the SHA and its current/behind glyph — two views of the same
                  immutable fact, "what commit is this hive on". These are one
                  element for width purposes: the glyph is 10px and rides
                  alongside the hash rather than setting the line's width.
               3. what happens NEXT: the upgrade affordance alone;
               4. the icon-only auto-upgrade mode select alone.
             Lines 3 and 4 are omitted entirely when empty — a read-only viewer
             gets two lines, not four with two blanks. Every fragment below is
             embedded verbatim, so ids, handlers and tooltips are preserved. */
          /* The drift dot rides on the SHA line, right of the current/behind
             glyph: "what commit is this hive on, and does it match the fleet" is
             one thought. It sets no extra line, so a drifting hive is no taller. */
          var behindKnown = h.commitsBehindStableV4 !== undefined && h.commitsBehindStableV4 !== null;
          var behindBadge = '';
          if (behindKnown && h.commitsBehindStableV4 > 0) {
            behindBadge = ' <span style="display:inline-block;padding:1px 6px;border-radius:999px;font-size:0.6rem;background:rgba(210,153,34,0.14);color:var(--yellow);border:1px solid rgba(210,153,34,0.35);white-space:nowrap" title="' + escAttr(h.commitsBehindStableV4 + ' commits behind stable v4 tip ' + (_stableV4SHA || '')) + '">' + esc(h.commitsBehindStableV4) + ' behind</span>';
          } else if (!behindKnown && _stableV4SHA && sha && !sameShaJS(sha, _stableV4SHA)) {
            behindBadge = ' <span style="display:inline-block;padding:1px 6px;border-radius:999px;font-size:0.6rem;background:rgba(210,153,34,0.10);color:var(--yellow);border:1px solid rgba(210,153,34,0.25);white-space:nowrap" title="' + escAttr('Could not compare this commit with stable v4 tip ' + _stableV4SHA) + '">? behind</span>';
          }
          var shaLine = '<span style="font-family:monospace;color:var(--muted)" title="' + escAttr(shaMsg) + '">' + esc(sha) + '</span>' + status + behindBadge + (driftDot ? ' ' + driftDot : '');
          versionCell = '<div style="' + STACKED_CELL_STYLE + '">' +
            '<div style="' + STACKED_LINE_STYLE + '">' + branch + '</div>' +
            '<div style="' + STACKED_LINE_STYLE + '">' + shaLine + '</div>' +
            (upgradeIcon ? '<div style="' + STACKED_LINE_STYLE + '">' + upgradeIcon + '</div>' : '') +
            (autoUpgradeCheck ? '<div style="' + STACKED_LINE_STYLE + '">' + autoUpgradeCheck + '</div>' : '') +
            '</div>';
        /* No version reported, but drift can still exist (e.g. heartbeat-stale on a
           spoke too old to report a SHA) — surface the dot beside the dash so
           folding Drift into Version never hides a signal. */
        } else { versionCell = '<span style="color:var(--muted)">—</span>' + (driftDot ? ' ' + driftDot : ''); }
        var pendingBadge = (h.pendingRequestCount > 0 && (roleAtLeast(h.role, 'read-write')))
          ? '<span style="position:absolute;top:-2px;right:-2px;background:var(--blue);color:#fff;border-radius:50%;width:16px;height:16px;font-size:0.6rem;display:flex;align-items:center;justify-content:center;font-weight:700">' + h.pendingRequestCount + '</span>'
          : '';
        var pendingPill = '';
        if (h.pendingRequestCount > 0 && (roleAtLeast(h.role, 'read-write'))) {
          pendingPill = '<a href="#" onclick="togglePendingRow(\'' + esc(h.id) + '\');return false" style="display:inline-flex;align-items:center;gap:4px;padding:3px 10px;background:rgba(59,130,246,0.12);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;cursor:pointer;white-space:nowrap">&#x1F514; ' + h.pendingRequestCount + ' pending</a>';
        }
        // 12 columns after the 15-to-9 fold. The visible cells are: bulk-select,
        // the ⋮ menu, Hive, Location, Uptime, Version, Repos, Maturity, Agents,
        // Tokens, Mode, Activity. Four folds collapsed six columns:
        //   PROV    → date lives in the status hover; sort rides the Uptime header
        //   DRIFT   → dot on the Version cell (driftBadge, full hover preserved)
        //   ACMM+JOURNEY  → one Maturity cell (both badges stacked, both sorts kept)
        //   ISSUES+PRS+CONTRIB → one Activity cell (all 3 stats + sparklines, 3 sorts kept)
        // Counted against the <th> cells in the header and the <td> cells emitted
        // below (bulkCheckboxCell contributes one).
        var TOTAL_COLUMNS = 13;
        /* Visibility moved OUT of its own column and under Location: "where
           does this hive run" and "who can see it" are both facts about the
           hive's placement, so they read as one cell, and folding them saves a
           whole column in a table that had 17.

           All THREE original branches survive unchanged — the owner toggle
           (with its 'vis-<id>' element id and onchange handler), the local
           link into Settings, and the read-only ✓/— for everyone else.
           They are deliberately NOT flattened: each answers a different
           question about who may change the setting. */
        var visibilityCell = (function() {
          var pub = !!h.isPublic;
          /* escAttr, not esc: this id lands inside a quoted attribute and a
             hive id carrying a quote would otherwise break out of it. Still
             'vis-' + the hive id, so it stays unique per row exactly as before. */
          var tid = 'vis-' + escAttr(h.id);
          /* Branch 1 — hosted hive the viewer OWNS: a live toggle. The owner is
             the only person who may change listing, so they get the control. */
          if (isHosted && h.role === 'owner') {
            return '<label style="position:relative;display:inline-block;width:' + VIS_SWITCH_W_PX + 'px;height:' + VIS_SWITCH_H_PX + 'px;cursor:pointer">' +
              '<input type="checkbox" id="' + tid + '"' + (pub ? ' checked' : '') +
              /* jsArg (not esc) for the handler argument: esc leaves an
                 apostrophe intact, which would close the attribute quote early.
                 jsArg supplies its own surrounding quotes. */
              ' onchange="toggleVisibility(' + jsArg(h.id) + ',this.checked)" style="opacity:0;width:0;height:0">' +
              '<span style="position:absolute;inset:0;background:' + (pub ? 'var(--green)' : 'var(--border)') + ';border-radius:' + VIS_SWITCH_RADIUS_PX + 'px;transition:background 0.2s"></span>' +
              '<span style="position:absolute;top:' + VIS_KNOB_INSET_PX + 'px;left:' + (pub ? VIS_KNOB_ON_LEFT_PX : VIS_KNOB_INSET_PX) + 'px;width:' + VIS_KNOB_PX + 'px;height:' + VIS_KNOB_PX + 'px;background:#fff;border-radius:50%;transition:left 0.2s"></span>' +
              '</label>';
          }
          /* Branch 2 — LOCAL hive: the hub cannot write its config, so this is
             a signpost to where the setting actually lives (Governor → Hub),
             not a control. Falls back to a plain badge with no dashboard URL. */
          if (isLocal) {
            var dh = h.dashboardUrl && !h.dashboardUrl.includes('localhost') ? h.dashboardUrl : '';
            var badge = pub ? '<span style="color:var(--green)">Public</span>' : '<span style="color:var(--muted)">Private</span>';
            return dh
              ? '<a href="' + escAttr(dh) + '#config/governor/Hub" target="_blank" title="Change in Settings → Hub tab" style="text-decoration:none;cursor:pointer">' + badge + ' <span style="font-size:0.6rem;color:var(--muted)">↗</span></a>'
              : badge;
          }
          /* Branch 3 — everyone else: read-only state, no affordance implied. */
          return pub ? '<span style="color:var(--green)">✓</span>' : '<span style="color:var(--muted)">—</span>';
        })();
        var locationBadge = isLocal ? '<span style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(107,114,128,0.15);color:#9ca3af;border:1px solid rgba(107,114,128,0.3)">local</span>' : clusterBadge(h.clusterId, h.clusterName);
        /* Location on top, visibility beneath, GitHub host last — three stacked
           lines, matching the three-line Version treatment. The owner toggle is
           a 36x20 switch and keeps its own box on its own line, so it stays an
           easy tap target.

           The host line reuses githubHostPill — the same helper the pending
           action cards and the Past Requests table use — so a GHE hive reads
           identically wherever it appears. An absent host renders as
           "github.com" rather than a blank chip, which is what an old
           heartbeat-only spoke that cannot yet report its host should show. */
        /* The Kubernetes namespace is NOT shown in the location column — it is
           low-frequency reference metadata (an operator only needs it to build a
           kubectl -n <ns> exec), so it lives in the status hover as an "ns:" line
           (see hiveNamespace(h) / the lines[] builder) rather than competing for
           space in a permanent column. */
        var locationCell = '<div style="' + STACKED_CELL_STYLE + '">' +
          '<div style="' + STACKED_LINE_STYLE + '">' + locationBadge + '</div>' +
          '<div style="' + STACKED_LINE_STYLE + ';font-size:0.7rem">' + visibilityCell + '</div>' +
          '</div>';
        var pendingExpandRow = '';
        if (h.pendingRequestCount > 0 && (roleAtLeast(h.role, 'read-write')) && (h.pending_requests || []).length > 0) {
          var prItems = (h.pending_requests || []).map(function(pr) {
            var avatar = linkedAvatar(pr.username, LIST_AVATAR_PX, pr.username, 'margin-right:6px');
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
          pendingExpandRow = '<tr id="pending-row-' + esc(h.id) + '"' + ((i % 2 === 1) ? ' class="hive-row-alt"' : '') + ' style="display:none"><td colspan="' + TOTAL_COLUMNS + '"><div style="padding:8px 16px;background:rgba(59,130,246,0.05);border-radius:6px;margin:4px 0">' + prItems + '</div></td></tr>';
        }
        /* Stable per-hive anchor so the "Attention needed" panel can scroll a
           specific row into view and highlight it. Built from the hive id, which
           is unique across both the assigned and unassigned sections. */
        return '<tr id="' + escAttr(hiveRowDomId(h.id)) + '"' + ((i % 2 === 1) ? ' class="hive-row-alt"' : '') + '>' +
          bulkCheckboxCell(h, section || 'all') +
          '<td class="hive-menu-cell" style="position:relative;width:30px;text-align:center;overflow:visible">' + ('<span style="cursor:pointer;font-size:1.1rem;color:var(--muted);user-select:none">⋮</span>' + pendingBadge + '<div class="hive-menu-dropdown" style="display:none;position:absolute;left:0;bottom:auto;background:#1c2128;border:1px solid #30363d;border-radius:8px;min-width:160px;padding:4px 0;z-index:1000;box-shadow:0 8px 24px rgba(0,0,0,0.5)">' + menuItems.join('') + '</div>') + '</td>' +
          '<td style="text-align:left;line-height:1.4">' + (function() { var isHostedRow = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-'))); var dh = isHostedRow && h.id ? ('/api/saas/hives/' + encodeURIComponent(h.id) + '/open') : (rb ? esc(rb) : ''); /* Label derived from the PROJECT (org + primary repo), not by splitting h.name — see hiveLabel. */ var label = hiveLabel(h); var orgName = label.line1; var repoName = label.line2; /* rp is the repo path shown in the GitHub-icon tooltip; use the same doubling-safe path the label shows (repoDisplayLine / hiveLabel), never org + '/' + primaryRepo which doubles the owner on a github.io/GHE hive whose primaryRepo is already 'owner/repo'. */ var rp = repoName || ''; /* The icon href must resolve to a real GitHub path. When primaryRepo already carries 'owner/repo', that pair IS the owner/repo the URL needs (not org, which may be a mis-parsed host); otherwise fall back to org + primaryRepo. */ var hasRepoPath = h.primaryRepo && h.primaryRepo.indexOf('/') !== -1; var rpOwner = hasRepoPath ? h.primaryRepo.split('/')[0] : h.org; var rpName = hasRepoPath ? h.primaryRepo.split('/').slice(1).join('/') : h.primaryRepo; var rpHref = ghRepoURL(hiveForgeHost(h), rpOwner, rpName); var ghIcon = (rp && rpHref) ? '<a href="' + rpHref + '" target="_blank" style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></a>' : (rp ? '<span style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></span>' : ''); /* vanityDisplay is the friendly host shown in the status bar on hover. rb is resolvedBase(h), which the hub has already overlaid with the claimed vanity_url; it is only a DISPLAY url here, never the click target — see ssoDisplayLink. Empty for a row with no vanity host, which falls back to today's /open href. Only meaningful on hosted rows: a non-hosted row's dh IS rb already. */ var vanityDisplay = isHostedRow && rb ? rb : ''; var link = function(text, bold) { if (dh) { return ssoDisplayLink(dh, vanityDisplay, text, bold ? 'hive-name-link' : 'hive-sub-link'); } var s = bold ? 'font-weight:700;color:inherit' : 'color:#6b7280;font-weight:400'; return '<span style="' + s + '">' + esc(text) + '</span>'; }; /* repoLink wraps the second line (the org/repo) in a link to the ACTUAL FORGE REPO (github.com / GHE host), NOT the spoke dashboard: the top line already opens the dashboard, so the repo path should take an operator to the code. rpHref is the same doubling-safe forge URL the GitHub icon uses (ghRepoURL(hiveForgeHost(h), rpOwner, rpName)); when it is empty (BYO/mis-parsed host) fall back to plain non-link text, never to the dashboard. */ var repoLink = function(text) { if (rpHref) { return '<a href="' + rpHref + '" target="_blank" rel="noopener" class="hive-sub-link" title="Open repository on ' + escAttr((hiveForgeHost(h) || 'github.com')) + '">' + esc(text) + '</a>'; } return '<span style="color:#6b7280;font-weight:400">' + esc(text) + '</span>'; }; var line1 = dot + ' ' + link(orgName, true) + heartbeatHeart(h) + nameEditAffordance(h); var fcPill = h.online ? failingCheckSummary(h) : ''; /* Advisory-staleness pill sits right beside the failing-check pill: both are "something is quietly wrong with this working hive" signals, and advisoryStaleSummary already self-suppresses (empty string) unless the hub flagged the digest stale, so unaffected rows are pixel-identical. */ var advPill = h.online ? advisoryStaleSummary(h) : ''; /* Advisory finding-count pill rides beside the staleness pill and carries the "(top N)" marker when the spoke capped its digest. */ var advCountPill = h.online ? advisoryFindingsSummary(h) : ''; /* Dead-link pill is deliberately NOT gated on h.online: the entire point is a hive that IS online and heartbeating while its public URL is broken, so gating it the way the other pills are gated would hide exactly the case it exists to surface. */ var dlPill = deadLinkSummary(h); var privatePill = privateURLSummary(h); /* Inference-auth pill: like the dead-link pill it is NOT gated on h.online, because the hive being online while every inference call 401s is exactly the case it surfaces. */ var iaPill = inferenceAuthSummary(h); /* Inline access faces sit on the name cell's second line, immediately after this row's own role badge: the badge already answers "what am I on this hive", so the co-members read as the natural continuation of the same thought, in the one cell that is left-aligned and has room to grow. It also keeps them out of the 16 dense metric columns, none of which is about people. Empty string when the viewer is the only member, so those rows are pixel-identical to today. */ var accessFaces = hiveAccessAvatars(h); /* Keyed off repoName, not orgName: line 1 now always carries SOME identity, so the presence of a second line is decided purely by whether there is a repo to put on it. Without a repo the row still shows the GitHub icon, role badge, faces and failing-check pill on the compact variant. */ var line2 = repoName ? '<div style="padding-left:18px;font-size:0.8rem">' + repoLink(repoName) + ' ' + ghIcon + ' ' + roleBadge(h.role) + accessFaces + fcPill + advPill + advCountPill + dlPill + privatePill + iaPill + '</div>' : '<div style="padding-left:18px">' + ghIcon + ' ' + roleBadge(h.role) + accessFaces + fcPill + advPill + advCountPill + dlPill + privatePill + iaPill + '</div>'; var line3 = pendingPill ? '<div style="margin-top:4px;padding-left:18px">' + pendingPill + '</div>' : ''; return line1 + line2 + line3; })() + '</td>' +
          '<td>' + locationCell + '</td>' +
          '<td style="white-space:nowrap">' + uptimeCell(h) + '</td>' +
          /* No white-space:nowrap on the cell itself: the stacked lines each
             carry their own nowrap, so the SHA still never breaks mid-hash,
             but the COLUMN is no longer forced to the width of every part laid
             end to end. That nowrap was the main reason this column was wide. */
          '<td style="font-size:0.7rem">' + versionCell + '</td>' +
          /* Repo count over the GitHub-instance pill: the host qualifies WHICH
             GitHub these repos live on, so the two belong together. Stacked
             rather than inline to keep the numeric column narrow. */
          /* Repos cell. AI Author is stacked as the third line here (it used to own
             a column): who this hive authors PRs as is naturally repo/identity
             context, and folding it in reclaims a column. aiAuthorEffective already
             folds in ai_author (it returns ai_author when set, else the App bot
             "<slug>[bot]", else empty); a hive with no usable GitHub App renders
             "—", never a stale personal ai_author it can't act as. Prefixed "as:"
             so the line reads as the authoring identity, not another repo. */
          '<td title="' + esc((h.repos || []).join('\n')) + '" style="cursor:' + (repoCount > 0 ? 'help' : 'default') + '">' +
            '<div style="' + STACKED_CELL_STYLE + '">' +
              '<div style="' + STACKED_LINE_STYLE + '">' + repoCount + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '" title="GitHub instance these repos live on">' + githubHostPill(hiveForgeHost(h)) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + ';color:var(--muted)" title="GitHub identity this hive opens PRs/commits as (— = no GitHub App installed yet)">as: ' + esc(h.aiAuthorEffective || '—') + '</div>' +
            '</div>' +
          '</td>' +
          /* MATURITY: the ACMM level badge and the Journey status stacked into
             ONE cell. Both answer "how far along is this hive's adoption" — the
             ACMM level is where it IS, the journey is the next step it OWES — so
             they read as one column instead of two. Each badge keeps its own
             hover (acmmBadge's title explains the level, journeyBadge's the
             stage), so no datum is lost; both sorts stay reachable from the
             MATURITY header's two ⇅ controls (acmmLevel and journey). */
          '<td>' +
            '<div style="' + STACKED_CELL_STYLE + '">' +
              '<div style="' + STACKED_LINE_STYLE + '">' + acmmBadge(h.acmmLevel) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '">' + journeyBadge(h.journey) + '</div>' +
            '</div>' +
          '</td>' +
          '<td title="' + esc((h.agents || []).map(function(a){ var label = a.name + ' (' + a.state + ')'; if (a.mode === 'on_demand') label += ' — on demand'; if (a.paused) label += ' — ' + agentPauseProvenance(a); return label; }).join('\n')) + '" style="cursor:' + ((h.agentCount || 0) > 0 ? 'help' : 'default') + '">' + (h.agentCount || 0) + '</td>' +
          '<td title="Cumulative tokens consumed, as of the last heartbeat" style="white-space:nowrap;cursor:help">' + fmtTokens(h.totalTokens24h || 0) + '</td>' +
          '<td>' + modeCell + '</td>' +
          /* ACTIVITY: Issues, PRs and Contributors — three mini-stats that were
             three columns — stacked densely into ONE cell. Every number and both
             sparklines survive verbatim; each line is labelled (I/PR/C) and
             carries a native title so a folded value is never ambiguous. All
             three sorts stay reachable from the ACTIVITY header's three ⇅
             controls (actionableIssues, actionablePRs, activeContributors). */
          '<td style="font-size:0.72rem">' +
            '<div style="' + STACKED_CELL_STYLE + ';align-items:flex-start">' +
              '<div style="' + STACKED_LINE_STYLE + '" title="Actionable issues"><span style="color:var(--muted);min-width:24px;display:inline-block">Iss</span>' + sparkline(h.issueHistory, '#f59e0b', 40, 12) + (h.actionableIssues || 0) + wsBadge(h.workSource) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '" title="Actionable PRs"><span style="color:var(--muted);min-width:24px;display:inline-block">PRs</span>' + sparkline(h.prHistory, '#3b82f6', 40, 12) + (h.actionablePRs || 0) + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '" title="Active contributors"><span style="color:var(--muted);min-width:24px;display:inline-block">Ctr</span>' + (h.activeContributors || 0) + '</div>' +
            '</div>' +
          '</td>' +
          /* QUADRANT: the small kite. Shape only at this size — the numbers
             live in its hover, which is the same chart drawn large. */
          '<td>' + quadrantCell(h) + '</td>' +
          '</tr>' + pendingExpandRow;
      };
      /* Section-header row: a labeled separator spanning all columns, styled to
         match the table's muted uppercase heading treatment (see .hive-table th). */
      /* Count of <th> cells in the hive table header below. The section-header
         row spans all of them; a stale value would leave the separator short
         and the table visibly ragged. 12 after the 15-to-9 fold (PROV, DRIFT,
         ACMM/JOURNEY→Maturity, ISSUES/PRS/CONTRIB→Activity), then 13 with the
         Quadrant column. Must stay equal to TOTAL_COLUMNS. */
      var TOTAL_COLUMNS_HEADER = 13;
      /* The header is a click target that expands/collapses its section. The
         caret mirrors aria-expanded so the affordance and the a11y state can
         never disagree. sectionKey also scopes the select-all checkbox to THIS
         section's rows only (Assigned and Unassigned select independently);
         the checkbox stops propagation so selecting does not also collapse.
         selectable=false suppresses the box for sections whose rows are never
         bulk-eligible (unassigned placeholders), where it would be a control
         that visibly does nothing. */
      var sectionHeader = function(label, count, sectionKey, selectable) {
        var collapsed = !!_dashSectionCollapsed[sectionKey];
        return '<tr class="hive-section-head" role="button" tabindex="0" aria-expanded="' + (collapsed ? 'false' : 'true') + '" ' +
          'onclick="toggleHiveSection(\'' + esc(sectionKey) + '\')" ' +
          'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();toggleHiveSection(\'' + esc(sectionKey) + '\')}" ' +
          'title="Click to ' + (collapsed ? 'expand' : 'collapse') + '">' +
          '<td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:14px 12px 6px;color:var(--muted);font-weight:600;font-size:0.75rem;' +
          'text-transform:uppercase;letter-spacing:0.5px;text-align:left">' +
          '<span class="hive-section-caret" aria-hidden="true">' + (collapsed ? '▸' : '▾') + '</span>' +
          (sectionKey && selectable !== false ? bulkSectionCheckbox(sectionKey) : '') +
          esc(label) + ' (' + count + ')</td></tr>';
      };
      /* Group-header row: nested one indent step inside a section header, with
         a caret showing collapse state and a count. Clicking toggles. Labels are
         user-controlled (org, owner, cluster and branch names all come from
         spoke heartbeats) so they are escaped here — and again in the onclick
         argument, where the id is additionally quote-escaped.

         This deliberately mirrors sectionHeader above — same caret glyphs, same
         role/tabindex/aria-expanded, same keyboard activation — because a group
         header and a section header are the same affordance at two nesting
         levels. Only the persistence key differs: sections store one flag each
         under hive-section-*-collapsed, groups store a set under
         hive-group-collapsed (their ids are dynamic and unbounded). */
      var GROUP_HEADER_INDENT_PX = 26;
      var groupHeader = function(label, count, collapsed, groupId) {
        var toggle = 'toggleDashGroup(' + jsArg(groupId) + ')';
        return '<tr class="hive-group-head" role="button" tabindex="0" aria-expanded="' + (collapsed ? 'false' : 'true') + '" ' +
          'onclick="' + toggle + '" ' +
          'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();' + toggle + '}" ' +
          'title="Click to ' + (collapsed ? 'expand' : 'collapse') + '">' +
          '<td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:8px 12px 6px ' + GROUP_HEADER_INDENT_PX + 'px;color:var(--muted);font-weight:600;' +
          'font-size:0.72rem;letter-spacing:0.3px;text-align:left">' +
          '<span class="hive-group-caret" aria-hidden="true">' + (collapsed ? '▸' : '▾') + '</span>' +
          esc(label) + ' <span style="opacity:0.7">(' + count + ')</span></td></tr>';
      };

      /* renderSection emits one Assigned/Unassigned section: its header, then
         either a flat run of rows or the grouped subsections. It takes the
         running index by reference through the counter object so the caller's
         GLOBAL index keeps advancing across sections AND groups — menu ids
         (hive-menu-<i>) must stay unique across the whole table or the ⋮
         dropdowns open the wrong row's menu.

         Critically, the index advances for rows inside COLLAPSED groups too:
         the index is a per-hive identity, not a per-visible-row position, so
         expanding a group must not renumber anything below it. */
      var renderSection = function(label, sectionKey, list, counter, selectable) {
        if (!(list || []).length) return '';
        var out = sectionHeader(label, list.length, sectionKey, selectable);
        /* The section is the OUTER collapse: when it is closed nothing inside
           it renders — not its rows and not its group headers. The counter
           still advances over every hive so menu ids stay stable. */
        var sectionOpen = !_dashSectionCollapsed[sectionKey];
        if (_dashGroupBy === HIVE_GROUP_NONE) {
          for (var fi = 0; fi < list.length; fi++) {
            var flatRow = buildRow(list[fi], counter.i++, sectionKey);
            if (sectionOpen) out += flatRow;
          }
          return out;
        }
        /* groupHives only ever emits non-empty buckets, so no empty group
           header can render. */
        var groups = groupHives(list, sectionKey);
        for (var gi = 0; gi < groups.length; gi++) {
          var g = groups[gi];
          var isCollapsed = !!_dashGroupCollapsed[g.id];
          if (sectionOpen) out += groupHeader(g.label, g.hives.length, isCollapsed, g.id);
          for (var ri = 0; ri < g.hives.length; ri++) {
            var rowHTML = buildRow(g.hives[ri], counter.i++, sectionKey);
            if (sectionOpen && !isCollapsed) out += rowHTML;
          }
        }
        return out;
      };

      var rows;
      if (_isAdmin) {
        /* Admin-only organizational aid: split into assigned (real, claimed)
           hives and unassigned placeholders — see isPlaceholderHive. Preserve
           incoming order so each section stays sorted.

           This split is the OUTER structure — grouping subdivides each side,
           it never replaces the split. */
        var assigned = [], unassigned = [];
        for (var _hi = 0; _hi < hives.length; _hi++) {
          var _h = hives[_hi];
          if (isPlaceholderHive(_h)) unassigned.push(_h); else assigned.push(_h);
        }
        /* Global running index across BOTH sections so menu ids (hive-menu-<i>)
           never collide between sections and the ⋮ dropdowns keep working.
           It advances over EVERY row, including rows inside a collapsed section
           or a collapsed group, so collapsing anything never renumbers the
           menus below it. renderSection carries the counter by reference. */
        var _counter = {i: 0};
        rows = renderSection('Assigned hives', HIVE_SECTION_ASSIGNED, assigned, _counter) +
          /* Unassigned placeholders are never bulk-eligible (nothing is running
             yet), so the section collapses but gets no select-all box. */
          renderSection('Unassigned hives', HIVE_SECTION_UNASSIGNED, unassigned, _counter, false);
      } else if (_dashGroupBy !== HIVE_GROUP_NONE) {
        /* Non-admin with grouping on: one flat set of groups, no section
           headers (a non-admin sees no placeholders to split off). The same
           global counter rule applies. */
        var _flatCounter = {i: 0};
        var _flatGroups = groupHives(hives, 'all');
        rows = '';
        for (var _fgi = 0; _fgi < _flatGroups.length; _fgi++) {
          var _fg = _flatGroups[_fgi];
          var _fgCollapsed = !!_dashGroupCollapsed[_fg.id];
          rows += groupHeader(_fg.label, _fg.hives.length, _fgCollapsed, _fg.id);
          for (var _fri = 0; _fri < _fg.hives.length; _fri++) {
            var _fgRow = buildRow(_fg.hives[_fri], _flatCounter.i++, 'all');
            if (!_fgCollapsed) rows += _fgRow;
          }
        }
      } else {
        /* Non-admin, no grouping: single flat list, exactly as before. */
        rows = hives.map(function(h, i) { return buildRow(h, i, 'all'); }).join('');
      }
      document.getElementById('hives-container').innerHTML =
        fleetQuadrantHeaderHTML() +
        '<div class="table-wrap"><table class="hive-table"><thead><tr>' +
        /* Non-admin lists have no section headers, so the flat list's
           select-all lives in the table head instead. */
        '<th style="width:26px;text-align:center">' + (_isAdmin ? '' : bulkSectionCheckbox('all')) + '</th>' +
        /* A combined-column header carries ONE sort control per folded field: the
           header cell can no longer own a single onclick once it hosts two or three
           sort keys, so each key gets its own inline clickable span. subSort() is
           the shared helper; every folded sort therefore stays reachable directly
           from the header, exactly as the standalone columns were. */
        '<th></th><th onclick="sortDashHives(\'name\')" style="cursor:pointer">Hive ⇅</th><th onclick="sortDashHives(\'clusterId\')" style="cursor:pointer;vertical-align:middle" title="Where this hive runs, and whether it is listed publicly">' + stackHeader('Location /', 'Public ⇅') + '</th>' +
        /* Uptime hosts BOTH temporal sorts: live uptime (startedAt) and the folded
           provision-date sort (registeredAt, the old "Prov ⇅"). The date itself now
           lives in the status hover; only its sort trigger rides here. */
        '<th onclick="sortDashHives(\'startedAt\')" style="cursor:pointer;vertical-align:middle" title="Process uptime since the last restart — a short value that keeps resetting means the pod is restarting">' + stackHeader('Uptime ⇅', subSort('registeredAt', 'Prov ⇅', 'Sort by when this hive was first provisioned (the hub&apos;s first-seen time). The date itself now lives in the status hover.')) + '</th>' +
        '<th title="Version, branch and any configuration drift from the fleet norm — a coloured dot appears beside the commit when this hive drifts; hover it for the specific signals">Version</th><th>Repos</th>' +
        /* MATURITY folds ACMM (where it is) and Journey (what it owes next); both
           sorts survive as inline ⇅ controls. */
        '<th style="vertical-align:middle" title="Adoption maturity: ACMM level and the next journey step">' + stackHeader('Maturity', subSort('acmmLevel', 'ACMM ⇅', 'Sort by ACMM level') + subSort('journey', 'Journey ⇅', 'Where this hive is on the adoption journey: install the GitHub App, assign a method/model (or run ClankeR, the contributor relay), then raise the ACMM level')) + '</th>' +
        '<th onclick="sortDashHives(\'agentCount\')" style="cursor:pointer">Agents ⇅</th><th onclick="sortDashHives(\'totalTokens24h\')" style="cursor:pointer" title="Cumulative tokens consumed, as of the last heartbeat">Tokens ⇅</th><th onclick="sortDashHives(\'governorMode\')" style="cursor:pointer">Mode ⇅</th>' +
        /* ACTIVITY folds Issues, PRs and Contrib; each keeps its own sort ⇅. */
        '<th style="vertical-align:middle" title="Actionable issues, actionable PRs and active contributors">' + stackHeader('Activity', subSort('actionableIssues', 'Iss ⇅', 'Sort by actionable issues') + subSort('actionablePRs', 'PRs ⇅', 'Sort by actionable PRs') + subSort('activeContributors', 'Ctr ⇅', 'Sort by active contributors')) + '</th>' +
        /* QUADRANT folds the four axis sorts plus the composite. Sorting by a
           single axis is the point of the column: "weakest efficiency" turns
           the table into a worklist rather than a picture. */
        '<th style="vertical-align:middle" title="Trust, Efficiency, Satisfaction and Productivity, each scored against the hives in the current view. Hover a kite for the numbers.">' + stackHeader('Quadrant', subSort('quadrant', 'All ⇅', 'Sort by the composite of every scored axis') + subSort('quadrantTrust', 'T ⇅', 'Sort by Trust: autonomy level, governor posture, merge acceptance and enrolled scope') + subSort('quadrantEfficiency', 'E ⇅', 'Sort by Efficiency: token burn rate, spend per merged PR, rework and output per agent') + subSort('quadrantProductivity', 'P ⇅', 'Sort by Productivity: merged PRs, relay throughput, work-source autonomy and work stalled on a human')) + '</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table></div>';
      /* Delegated, so binding once is enough no matter how often the table is
         re-rendered. The guard keeps repeated renders from stacking listeners. */
      if (!_autoUpgradeSelectsBound) { bindAutoUpgradeSelects(); _autoUpgradeSelectsBound = true; }
      /* Re-derive the bar and the select-all tri-state from _bulkSelected now
         that the fresh checkboxes exist in the DOM. */
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
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

    /* Auto-upgrade select values. AUTO_UPGRADE_OFF is a UI-only sentinel: on
       the wire "off" is auto_upgrade:false, and the two real modes match the
       server's allowlist ("instant" / "daily"). */
    var _autoUpgradeSelectsBound = false;
    var AUTO_UPGRADE_OFF = 'off';
    var AUTO_UPGRADE_INSTANT = 'instant';
    var AUTO_UPGRADE_DAILY = 'daily';
    var AUTO_UPGRADE_WEEKLY = 'weekly';
    /* Copy is framed around DISRUPTION, not cron mechanics: the operator is
       choosing when it is acceptable to interrupt a hive that is working. */
    var AUTO_UPGRADE_TITLE = 'When to apply new versions. Instant restarts the hive as soon as a new version lands; Daily restarts it at most once a day at 1pm ET; Weekly restarts it at most once a week, Tuesday at 1pm ET, so a stable hive is disturbed as rarely as possible.';
    /* Icon-only labels. The "Auto:" prefix repeated on every row was the widest
       single element in the Version column and said nothing a scanning operator
       did not already know from the column it sits in — so it is carried by the
       title/aria-label instead of being spelled out on every line.

       The three glyphs are chosen to be DISTINCT SHAPES, not intensities of one
       shape: 'off' must not read as a greyed-out 'instant'.
         ⦸  off     — a struck-through circle: the universal "disabled" mark,
                      and the only glyph here with a diagonal stroke.
         ⚡ instant  — a bolt: applies the moment a version lands.
         🕔 5p      — a clock face, plus the literal window "5p", because the
                      hour is the whole point of the mode and no icon conveys it.
       All three are plain text glyphs rather than SVG or background images, so
       they inherit the select's currentColor. The hub renders on one dark
       surface today, but nothing here hard-codes a fill or stroke colour, so a
       light surface would not make any of the three vanish. The label text
       after the glyph is the shortest string that still disambiguates — and it
       is there precisely so the control never degrades to "a picture only":
       ⚡ and 🕔 are emoji whose rendering varies by platform, so the word
       carries the meaning if the glyph renders as a box.

       ariaLabel carries the FULL meaning for screen readers and for the
       per-option tooltip, so nothing is lost by dropping the inline words. */
    var AUTO_UPGRADE_OPTIONS = [
      {value: AUTO_UPGRADE_OFF, label: '⦸ off', ariaLabel: 'Auto-upgrade: off'},
      {value: AUTO_UPGRADE_INSTANT, label: '⚡ instant', ariaLabel: 'Auto-upgrade: instantly when a new version lands'},
      {value: AUTO_UPGRADE_DAILY, label: '🕐 1p', ariaLabel: 'Auto-upgrade: daily at 1pm ET'},
      {value: AUTO_UPGRADE_WEEKLY, label: '🗓 tue 1p', ariaLabel: 'Auto-upgrade: weekly on Tuesday at 1pm ET'}
    ];
    /* Accessible name for the select itself, resolved from the CURRENT mode so
       the control announces what it is set to rather than only what it does. */
    function autoUpgradeAriaLabel(mode) {
      for (var ai = 0; ai < AUTO_UPGRADE_OPTIONS.length; ai++) {
        if (AUTO_UPGRADE_OPTIONS[ai].value === mode) return AUTO_UPGRADE_OPTIONS[ai].ariaLabel;
      }
      return 'Auto-upgrade';
    }

    /* One delegated listener on the container, bound once at startup, so every
       re-render of the table keeps working without rebinding per row. */
    function bindAutoUpgradeSelects() {
      var container = document.getElementById('hives-container');
      if (!container) return;
      container.addEventListener('change', function(e) {
        var el = e.target;
        if (!el || !el.classList || !el.classList.contains('auto-upgrade-select')) return;
        var id = el.getAttribute('data-hive-id');
        if (!id) return;
        setAutoUpgradeMode(id, el.value);
      });
    }

    /* Maps the select's value onto the existing endpoint's two fields. "off"
       sends auto_upgrade:false; the mode is still sent so the server records a
       concrete preference rather than leaving a blank to be re-interpreted. */
    async function setAutoUpgradeMode(id, value) {
      var enabled = value !== AUTO_UPGRADE_OFF;
      var mode = (value === AUTO_UPGRADE_DAILY || value === AUTO_UPGRADE_WEEKLY) ? value : AUTO_UPGRADE_INSTANT;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled, auto_upgrade_mode: mode})
        });
        if (!resp.ok) { hiveToast('Failed to update auto-upgrade', 'error'); loadHives(); return; }
        var label = !enabled ? 'off'
          : (mode === AUTO_UPGRADE_DAILY ? 'daily at 1pm ET'
          : (mode === AUTO_UPGRADE_WEEKLY ? 'weekly on Tuesday at 1pm ET' : 'instant'));
        hiveToast(id + ' auto-upgrade: ' + label, 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        loadHives();
      }
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

    /* Admin upgrade kill switch. _upgradePause mirrors the server's persisted
       state ({hub:{paused,by,at}, spokes:{...}}); it rides the my-hives
       top-level payload. Checked = PAUSED. */
    var _upgradePause = {hub: {paused: false}, spokes: {paused: false}};
    function upgradePauseToggleHTML(target, label, title) {
      var sw = (_upgradePause && _upgradePause[target]) || {paused: false};
      var t = title + (sw.paused ? ' — paused by ' + (sw.by || '?') + ' since ' + (sw.at || '?') : '');
      /* Pause/play ICON button with the plain-text label beside it. The icon
         shows the ACTION a click performs: running → ⏸ (pause), paused → ▶
         (resume, red). Text-variation selector (\uFE0E) keeps the glyphs
         monochrome instead of emoji. The bordered flex-column wrapper at the
         call site owns spacing/alignment for the pair. */
      var icon = sw.paused ? '\u25B6\uFE0E' : '\u23F8\uFE0E';
      var btnStyle = 'width:18px;height:18px;display:inline-flex;align-items:center;justify-content:center;' +
        'padding:0;font-size:0.62rem;line-height:1;border-radius:4px;cursor:pointer;' +
        (sw.paused
          ? 'background:color-mix(in srgb, var(--red) 18%, transparent);border:1px solid var(--red);color:var(--red)'
          : 'background:var(--surface);border:1px solid var(--border);color:var(--muted)');
      return '<span style="display:inline-flex;align-items:center;gap:5px;white-space:nowrap" title="' + escAttr(t) + '">' +
        '<button onclick="toggleUpgradePause(\'' + target + '\', ' + (sw.paused ? 'false' : 'true') + ')" style="' + btnStyle + '" aria-label="' + escAttr((sw.paused ? 'Resume ' : 'Pause ') + label) + '">' + icon + '</button>' +
        '<span style="font-size:0.6rem;color:' + (sw.paused ? 'var(--red)' : 'var(--muted)') + ';font-weight:' + (sw.paused ? '700' : '400') + '">' + esc(label) + '</span>' +
        '</span>';
    }
    async function toggleUpgradePause(target, paused) {
      try {
        var resp = await fetch('/api/saas/upgrade-pause', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({target: target, paused: paused})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to update upgrade pause', 'error'); loadHives(); return; }
        if (data.state) _upgradePause = data.state;
        hiveToast((target === 'hub' ? 'Hub' : 'Spoke') + ' upgrades ' + (paused ? 'PAUSED' : 'resumed'), 'success');
        renderUpgradePauseBanner();
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
    /* renderUpgradePauseBanner paints a prominent banner above the hive list
       while EITHER kill switch is engaged, so a paused fleet is impossible to
       miss. Same placement/styling family as renderPendingBanner. */
    function renderUpgradePauseBanner() {
      var existing = document.getElementById('upgrade-pause-banner');
      if (existing) existing.remove();
      var hubSw = (_upgradePause && _upgradePause.hub) || {};
      var spokeSw = (_upgradePause && _upgradePause.spokes) || {};
      if (!hubSw.paused && !spokeSw.paused) return;
      var container = document.getElementById('hives-container');
      if (!container || !container.parentNode) return;
      var line = function(sw, label, detail) {
        return '<div style="display:flex;align-items:center;gap:10px">' +
          '<span style="font-size:1.1rem">&#x23F8;&#xFE0F;</span>' +
          '<span style="font-size:0.85rem;color:var(--text)"><strong>' + label + '</strong> ' + detail +
          ' — paused by <strong>' + esc(sw.by || 'an admin') + '</strong>' +
          (sw.at ? ' since <span style="font-family:monospace;font-size:0.8rem">' + esc(sw.at) + '</span>' : '') +
          '</span></div>';
      };
      var html = '';
      if (hubSw.paused) html += line(hubSw, 'Hub upgrades are PAUSED', '(the hub stays on its current build)');
      if (spokeSw.paused) html += line(spokeSw, 'Spoke upgrades are PAUSED fleet-wide', '(no automatic image changes reach any hive)');
      var banner = document.createElement('div');
      banner.id = 'upgrade-pause-banner';
      banner.style.cssText = 'background:rgba(239,68,68,0.12);border:1px solid rgba(239,68,68,0.4);border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;flex-direction:column;gap:6px';
      banner.innerHTML = html;
      container.parentNode.insertBefore(banner, container);
    }

    var _upgradingHives = {};
    // _deletingHives[id] = true while a hive delete is in flight, so the hub
    // table shows a "Deleting…" status on that row instead of the row just
    // silently vanishing (or looking normal) mid-teardown.
    var _deletingHives = {};
    var _switchStartedAt = {}; // hiveId → ms timestamp the switch was initiated
    var SWITCH_STALE_MS = 8 * 60 * 1000; // warn if a switch hasn't landed in 8 min

    /* ── Upgrade elapsed-time counter ──
       A hive that has been "Upgrading" for a long time is a FAULT SIGNAL, not
       progress: the incident this exists to prevent had 15 hives pinned at
       upgrading:true for 20+ minutes while their spokes exited 0 in a restart
       loop, and a static spinner made a permanently-wedged fleet look
       identical to a merely slow one.

       The thresholds below escalate the counter's colour so that state is
       obvious at a glance instead of requiring the operator to notice a
       number. UPGRADE_ELAPSED_RED_MS deliberately equals staleUpgradeTimeout
       in server.go (10m) — the same instant the hub raises its stuck-upgrade
       alert. Turning red EARLIER would accuse a hive the hub still considers
       healthy; turning red LATER would leave a window where the alert panel
       says stuck and the row still looks fine. The amber step sits at half of
       that, an early warning while the upgrade is still plausibly in flight. */
    var UPGRADE_ELAPSED_RED_MS = 10 * 60 * 1000;  // == staleUpgradeTimeout (server.go): hub calls this stuck
    var UPGRADE_ELAPSED_AMBER_MS = 5 * 60 * 1000; // half the stuck limit: early warning, still plausible

    /* Below this the counter is suppressed entirely. Every upgrade begins at
       0s, and a counter that flickers "1s, 2s…" on every healthy rollout would
       train the operator to ignore it — which is exactly the habit that let the
       incident go unnoticed. It only appears once an upgrade is slow enough to
       be worth a human's attention. */
    var UPGRADE_ELAPSED_MIN_SHOW_MS = 30 * 1000; // don't nag during a normal fast rollout

    /* Clocks are not synchronised. The browser's Date.now() and the hub's
       upgradeStartedAt can disagree by seconds, which yields a small NEGATIVE
       elapsed on a freshly-started upgrade. Tolerate that as "just started"
       rather than rendering a negative duration; anything more negative than
       this is real clock skew and the counter is suppressed instead of shown
       as a lie. */
    var UPGRADE_ELAPSED_SKEW_TOLERANCE_MS = 60 * 1000; // treat small negatives as 0

    /* Go serialises a zero time.Time as "0001-01-01T00:00:00Z" and — because
       json:"...,omitempty" does NOT omit a zero struct — the hub emits exactly
       that string for a hive whose UpgradeStartedAt was never set or was reset.
       Date.parse() accepts it happily, and now − year 0001 is ~17.7 MILLION
       hours: the live "Upgrading 17755944h28m" wedge on ibm-alchemy. Any
       upgradeStartedAt at or before this cutoff is not a real start time but a
       lost/zero one, and the counter must be suppressed rather than rendered as
       a two-thousand-year duration. 2020-01-01 predates the project itself, so
       no genuine upgrade can legitimately fall before it. */
    var UPGRADE_STARTED_MIN_SANE_MS = Date.UTC(2020, 0, 1); // any earlier start time is a lost/zero timestamp

    /* upgradeElapsedMs returns how long a hive has been upgrading, in ms, or
       null when that cannot be known — a MISSING, EMPTY, UNPARSEABLE or
       PRE-EPOCH (zero/lost) upgradeStartedAt, or a negative elapsed beyond the
       skew tolerance. Callers must render nothing on null; they must never
       render NaN, a negative duration, a two-thousand-year duration, or a
       fabricated "0s" that implies the upgrade just started when in truth the
       timestamp was lost.

       Note a real server behaviour this relies on: upgradeStartedAt is stamped
       ONLY where Upgrading is set true, so a hive that is merely QUEUED has no
       timestamp and correctly gets null here rather than a bogus counter. */
    function upgradeElapsedMs(h, nowMs) {
      if (!h) return null;
      var started = h.upgradeStartedAt;
      if (!started || typeof started !== 'string') return null;
      var t = Date.parse(started);
      if (isNaN(t)) return null; /* unparseable timestamp — show nothing, never NaN */
      if (t < UPGRADE_STARTED_MIN_SANE_MS) return null; /* zero/lost start time (year 0001) — never a real "17.7M hour" upgrade */
      var elapsed = (typeof nowMs === 'number' ? nowMs : Date.now()) - t;
      if (elapsed < 0) {
        /* Clock skew. A small negative is the browser being marginally behind
           the hub on an upgrade that genuinely just started; clamp it to zero.
           A large negative means the timestamp is not trustworthy at all. */
        if (elapsed > -UPGRADE_ELAPSED_SKEW_TOLERANCE_MS) return 0;
        return null;
      }
      return elapsed;
    }

    /* formatUpgradeElapsed renders a compact duration: "45s", "3m", "1h12m".
       Minutes are the operator's working unit here, so past an hour it keeps
       the minutes rather than collapsing to a bare "1h" that would hide a
       72-minute wedge behind the same string as a 61-minute one. */
    function formatUpgradeElapsed(ms) {
      if (typeof ms !== 'number' || isNaN(ms) || ms < 0) return '';
      var totalSec = Math.floor(ms / 1000);
      if (totalSec < 60) return totalSec + 's';
      var totalMin = Math.floor(totalSec / 60);
      if (totalMin < 60) return totalMin + 'm';
      var hours = Math.floor(totalMin / 60);
      return hours + 'h' + (totalMin % 60) + 'm';
    }

    /* upgradeElapsedColor escalates the counter past the thresholds above.
       Muted while the rollout is plausibly healthy, amber as an early warning,
       red once the hub itself would call the upgrade stuck. */
    function upgradeElapsedColor(ms) {
      if (typeof ms !== 'number' || isNaN(ms)) return 'var(--muted)';
      if (ms >= UPGRADE_ELAPSED_RED_MS) return 'var(--red)';
      if (ms >= UPGRADE_ELAPSED_AMBER_MS) return 'var(--amber, #f59e0b)';
      return 'var(--muted)';
    }

    /* renderUpgradeElapsed builds the counter fragment appended to the
       "Upgrading" pill. Returns '' whenever the elapsed time is unknown or
       still below the nag threshold, so the common healthy case renders
       exactly what it renders today. */
    function renderUpgradeElapsed(h, nowMs) {
      var ms = upgradeElapsedMs(h, nowMs);
      if (ms === null) return '';
      if (ms < UPGRADE_ELAPSED_MIN_SHOW_MS) return '';
      var label = formatUpgradeElapsed(ms);
      if (!label) return '';
      var stuck = ms >= UPGRADE_ELAPSED_RED_MS;
      /* Past the red line the wording stops describing progress and names the
         fault, because at that point the hub has raised its stuck-upgrade
         alert and "still upgrading" would be the same misleading reassurance
         the incident turned on. */
      var tip = stuck
        ? 'Upgrading for ' + label + ' — past the ' + Math.round(UPGRADE_ELAPSED_RED_MS / 60000) +
          ' minute limit. This upgrade is almost certainly wedged, not slow; check the hive for a failed self-upgrade.'
        : 'Upgrading for ' + label;
      return '<span title="' + escAttr(tip) + '" style="margin-left:4px;font-variant-numeric:tabular-nums;color:' +
        upgradeElapsedColor(ms) + (stuck ? ';font-weight:600' : '') + '">' + esc(label) + '</span>';
    }

    /* Version and Location cells stack their contents vertically so neither
       column is forced as wide as the sum of its parts. The Version cell used
       to lay branch pill + SHA + status + upgrade affordance + auto-upgrade
       select on ONE nowrap line, which made it the widest column in a table
       that already carries 16 of them.

       STACKED_LINE_HEIGHT is deliberately tighter than the table's default so
       the stack stays as short as four lines can be.

       ONE ELEMENT PER LINE. The three-line arrangement did not actually narrow
       the column, because its last line carried TWO controls side by side (the
       upgrade affordance and the auto-upgrade select). A column is as wide as
       its widest line, so that pair — not the short lines above it — set the
       width, and the stacking bought nothing. Splitting them onto their own
       lines is what makes the widest line a single element.

       THE HONEST NUMBERS — do not "simplify" these away and do not restate
       them from arithmetic alone. A previous change to this cell shipped a
       claim of height-neutrality that was false. These were MEASURED in
       Chromium on the rendered cell (getBoundingClientRect), for an owner row
       that is behind latest with auto-upgrade set to daily — the widest and
       tallest variant:
         column width   266.4px -> 110.4px   (-156.0px, -58.6%)
         cell height     66.8px ->  79.8px   (+13.0px)
         stack height    50.8px ->  63.8px   (+13.0px)
       The four lines measure 15.0 / 12.9 / 12.9 / 20.0 px plus 3 gaps of
       STACKED_ROW_GAP_PX. So ROWS DO GROW, by 13px on owner rows. That is the
       price of the width win and it is stated rather than hidden. Two things
       hold it down: the upgrade affordance is now text-with-an-underline rather
       than a padded button (8px of padding and border removed), and the select
       is pinned to AUTO_SELECT_H_PX rather than left to the UA's default
       control height, which is taller.

       What actually sets the width now is the LONGEST upgrade label — the
       "Queued for auto-upgrade · 1pm ET" badge plus its "Upgrade now" action
       (the state/action split that answers the "the 'queued' line IS the
       upgrade button?" confusion) — not the select, which measures 77px.
       The old width was set by the "queued · 1pm ET" label and the
       "Auto: daily 1pm ET" select sitting on ONE line together.

       Non-owner rows are UNCHANGED: they still get at most two lines (28.9px),
       under the two-line name cell's 40.3px, which continues to set their
       height. Only owner rows pay.

       STACKED_ROW_GAP_PX separates the stacked lines without adding a full line
       box. 1px proved too tight in practice: the branch pill, SHA, upgrade link
       and auto-upgrade select read as one crowded block, and the Location cell's
       cluster badge sat flush against its visibility toggle. 4px is enough to
       group them as distinct rows while still costing far less than a line box. */
    var STACKED_LINE_HEIGHT = 1.15;
    var STACKED_ROW_GAP_PX = 4;
    /* Shared style for a stacked cell: a vertical flex column, centred to match
       the table's default text-align:center for non-first cells. */
    var STACKED_CELL_STYLE = 'display:flex;flex-direction:column;align-items:center;justify-content:center;gap:' + STACKED_ROW_GAP_PX + 'px;line-height:' + STACKED_LINE_HEIGHT;
    /* Horizontal gap between the items that share one stacked line (e.g. the
       SHA and its current/behind glyph). Small enough that a line stays visibly
       one group rather than reading as separate columns. */
    var STACKED_ITEM_GAP_PX = 4;
    /* One line inside a stacked cell. Items within a line stay on one row (the
       SHA must never wrap mid-hash) but the LINES themselves are what narrow
       the column. */
    var STACKED_LINE_STYLE = 'display:flex;align-items:center;justify-content:center;gap:' + STACKED_ITEM_GAP_PX + 'px;white-space:nowrap';

    /* Icon-only auto-upgrade select geometry. The control lost its "Auto:"
       prefix, so its box must be sized deliberately rather than left to shrink
       to a glyph: 20px tall and at least 56px wide keeps it a comfortable
       pointer and touch target (the row is 63.8px tall, so 20px is roughly a
       third of it — visibly a control, not a decoration) while still being far
       narrower than the "Auto: daily 1pm ET" string it replaces. Widening it
       would give back the column width this change exists to reclaim; making it
       smaller would make it fiddly to hit. */
    var AUTO_SELECT_H_PX = 20;
    var AUTO_SELECT_MIN_W_PX = 56;
    /* Horizontal padding inside the select. 4px, not the previous 3px, because
       an icon needs a little breathing room from the border to read as a glyph
       rather than as part of the frame. */
    var AUTO_SELECT_PAD_X_PX = 4;
    /* The upgrade affordance is text-with-an-underline, not a padded button:
       on its own line a button's 3px padding and 1px border added 8px of row
       height for no extra affordance, since the whole line is already the
       click target. The underline plus the pointer cursor is what says
       "clickable"; colour alone would not, and would fail in one theme. */
    var UPGRADE_LINK_STYLE = 'cursor:pointer;text-decoration:underline;text-underline-offset:2px;white-space:nowrap';
    /* The manual-path upgrade affordance (auto-upgrade OFF) is a real
       primary-ish button again: real users could not find the upgrade action
       when it was quiet text, so this trades a few px of row height for an
       unmissable target. Background is declared separately so the hover state
       can restore it without re-stating the whole style. */
    var UPGRADE_BTN_BG = 'rgba(63,185,80,0.15)';
    var UPGRADE_BTN_HOVER_BG = 'rgba(63,185,80,0.3)';
    var UPGRADE_BTN_STYLE = 'cursor:pointer;white-space:nowrap;display:inline-block;padding:2px 8px;border-radius:4px;' +
      'background:' + UPGRADE_BTN_BG + ';color:var(--green);border:1px solid rgba(63,185,80,0.4);font-weight:600;font-size:0.7rem';
    /* Passive upgrade-STATE badge: "Queued for auto-upgrade" and the
       non-owner "Update available". Deliberately affordance-free (no cursor,
       no underline, no handler) — the state/action split only works if the
       state never looks clickable. */
    var UPGRADE_STATE_BADGE_STYLE = 'display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.65rem;' +
      'background:var(--surface);color:var(--muted);border:1px solid var(--border);white-space:nowrap';

    /* Visibility toggle switch geometry, used by the Location cell's owner
       branch. A 36x20 track with a 16px knob inset 2px is the standard iOS-style
       switch proportion: the knob travels W - knob - 2*inset = 18px, which is
       why the "on" position is 18px from the left. Keeping these as constants
       means the track, the knob and the travel distance can never drift apart. */
    var VIS_SWITCH_W_PX = 36;
    var VIS_SWITCH_H_PX = 20;
    var VIS_KNOB_PX = 16;
    var VIS_KNOB_INSET_PX = 2;
    var VIS_SWITCH_RADIUS_PX = VIS_SWITCH_H_PX / 2; // fully rounded track
    var VIS_KNOB_ON_LEFT_PX = VIS_SWITCH_W_PX - VIS_KNOB_PX - VIS_KNOB_INSET_PX;

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
    function hiveSwitchState(h, branchName) {
      var sentinel = _upgradingHives[h.id];
      var hasSwitchSentinel = typeof sentinel === 'string'
        && sentinel.indexOf(SWITCH_SENTINEL_PREFIX) === 0;
      var upgradeTarget = h.upgradeTarget || '';
      var isBranchTarget = upgradeTarget.length > BRANCH_TARGET_SUFFIX.length
        && upgradeTarget.slice(-BRANCH_TARGET_SUFFIX.length) === BRANCH_TARGET_SUFFIX;
      var targetBranch = '';
      if (isBranchTarget) {
        targetBranch = upgradeTarget.slice(0, -BRANCH_TARGET_SUFFIX.length);
      } else if (upgradeTarget && (_releaseChannels || []).indexOf(upgradeTarget) !== -1) {
        /* A channel switch arms the CHANNEL NAME as the target ("stable"),
           not a "-latest" tag, so the suffix parse above never sees it —
           after a page refresh (client sentinel gone) the "Switching to
           stable" indicator silently vanished while the switch was still
           running server-side. */
        targetBranch = upgradeTarget;
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
            /* Catch up on the renders the open menu deferred (see
               renderHives guard) so the rows reflect current data
               immediately instead of waiting for the next poll. */
            renderHives(sortedDashHives(), true);
          }
        };
        setTimeout(function() { document.addEventListener('click', closeHandler); }, 0);
      }
    }

    var _switchTimers = {};
    /* Reset App: clears the spoke's installation_id so the owner is prompted
       to install the GitHub App again. Confirmed first because it costs the
       owner a re-install, and the reset is delivered on the spoke's next
       heartbeat rather than immediately. */
    async function resetHiveApp(hiveId, hiveName) {
      var ok = await hiveConfirm('Reset the Forge App for "' + hiveName + '"?\n\nThe spoke clears its installation ID on the next heartbeat and the owner is prompted to install the App again. The App ID, slug and key are left alone.');
      if (!ok) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/reset-app', {method: 'POST'});
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) { await hiveNotify('Reset failed', String(data.error || resp.status)); return; }
        await hiveNotify('Forge App reset armed',
          'Hive: ' + hiveName + '\n' +
          'The spoke clears its installation on the next heartbeat (about 30 seconds), then shows the install prompt.');
        if (typeof loadHives === 'function') loadHives();
      } catch (e) {
        await hiveNotify('Reset failed', String(e));
      }
    }

    /* Reset assignment: returns an assigned-but-unclaimed placeholder to the
       available pool so it can be re-armed. Shown only on a wedged row
       (assigned && !claim_delivered), matching the endpoint's guard — a
       delivered claim is a live hive and the server refuses to reset it. */
    async function resetAssignment(hiveId, hiveName) {
      var ok = await hiveConfirm('Reset the assignment for "' + hiveName + '"?\n\n' +
        'This slot was assigned but its spoke never reported the project back, so it is stuck "Assigning" and cannot be re-approved or re-assigned. ' +
        'Resetting returns it to the available pool — its project, owner and org are cleared — so it can be assigned again cleanly. ' +
        'A live, fully-claimed hive is never affected.');
      if (!ok) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/reset-assignment', {method: 'POST'});
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) { await hiveNotify('Reset failed', String(data.error || resp.status)); return; }
        var extra = data.reopened_request_for ? '\nThe provision request for ' + data.reopened_request_for + ' was reopened so you can re-approve it.' : '';
        await hiveNotify('Assignment reset',
          'Hive: ' + hiveName + '\n' +
          'The slot is back in the available pool and can be assigned again.' + extra);
        if (typeof loadHives === 'function') loadHives();
      } catch (e) {
        await hiveNotify('Reset failed', String(e));
      }
    }

    /* Restart Spoke: arms a rolling restart delivered to every spoke instance
       reporting as this hive on their next heartbeats (bounded window). Used
       to shed a stale duplicate instance the hub cannot delete directly. */
    async function restartHiveSpoke(hiveId, hiveName) {
      if (!await hiveConfirm('Restart the spoke for "' + hiveName + '"?\n\nEvery instance reporting as this hive rolling-restarts on its next heartbeat (up to 5 minutes). The image and configuration are unchanged.')) return;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/restart-spoke', {method: 'POST'});
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) { await hiveNotify('Restart failed', String(data.error || resp.status)); return; }
        await hiveNotify('Spoke restart armed',
          'Hive: ' + hiveName + '\n' +
          'Instances restart on their next heartbeat within the 5-minute window.');
        if (typeof loadHives === 'function') loadHives();
      } catch (e) {
        await hiveNotify('Restart failed', String(e));
      }
    }

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
      // Deep link from an access-request notification (access_notify.go):
      // open Manage Access directly so the owner lands on the pending request.
      var manageId = params.get('manage_access');
      if (manageId) {
        window.history.replaceState({}, '', '/dashboard');
        openAccessModal(manageId, '');
        return;
      }
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
      var pending = (hives || []).filter(function(h) { return (roleAtLeast(h.role, 'read-write')) && h.pendingRequestCount > 0; });
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
        msg = 'Your hive request for <strong>' + project + '</strong> has been approved! An admin will assign your hive shortly.';
      } else if (status === 'denied') {
        icon = '&#x274C;';
        bg = 'rgba(239,68,68,0.12)'; border = 'rgba(239,68,68,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> was denied by an admin.';
      } else {
        icon = '&#x1F3D7;&#xFE0F;';
        bg = 'rgba(245,158,11,0.12)'; border = 'rgba(245,158,11,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> is pending admin approval.';
      }
      /* The banner is purely informational in EVERY state now that the
         Provision button is gone (hives are assigned by an admin), so all
         three states are dismissable. The earlier carve-out that kept
         'approved' non-dismissable existed only because that state carried the
         Provision button — the sole place the action was offered — and
         dismissals never expire (see dismissBanner: the stored timestamp is
         written but no reader ever compares it), so a stray click would have
         stranded the user. With no unique action left in any state, there is
         nothing to strand and the carve-out no longer applies.

         The key embeds BOTH the request identity and its status, so a
         dismissed 'pending' banner re-raises on its own once the request flips
         to approved or denied — a stray click can never permanently hide a
         status change. ProvisionRequest carries no id, so identity is the
         org/primary-repo pair it targets: the same thing the banner text
         names. */
      var dismissKey = ('provision:' + (req.org || '') + '/' +
        (req.primary_repo || req.repos || '') + ':' + status).replace(/'/g, '');
      var dismissedReqs = {};
      try {
        dismissedReqs = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}') || {};
      } catch (e) { dismissedReqs = {}; } /* corrupted value — show the banner */
      if (dismissedReqs[dismissKey]) { el.style.display = 'none'; return; }
      el.innerHTML = '<div style="background:' + bg + ';border:1px solid ' + border + ';border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px">' +
        '<span style="font-size:1.1rem">' + icon + '</span>' +
        '<span style="flex:1;font-size:0.85rem;color:var(--text)">' + msg + '</span>' +
        '<button onclick="dismissBanner(\'' + esc(dismissKey) + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button>' +
        '</div>';
    }

    // normalizeProvisionStatus trims and lowercases a request status so the
    // pending/decided split cannot be defeated by casing or stray whitespace in
    // a stored record. Returns '' for a missing status, which legacy records
    // written before the field existed have — those are treated as pending.
    function normalizeProvisionStatus(status) {
      return String(status == null ? '' : status).trim().toLowerCase();
    }

    // --- Past Requests (decided provision requests) ---
    // Collapsed by default: this is an audit trail, not a work queue, so it
    // should never push the pending action cards off the fold. The choice is
    // remembered per browser under the same 'hive-*-collapsed' key convention
    // used by the cluster health panel.
    var PAST_REQUESTS_COLLAPSED_KEY = 'hive-past-requests-collapsed';
    // A blank github_host on a request means the public instance; a GitHub
    // Enterprise request carries its host (github.ibm.com, …). Used both as the
    // pill label and as the origin for repo links, so Enterprise requesters get
    // a link that actually resolves instead of a dead github.com URL.
    var PAST_REQUESTS_DEFAULT_GITHUB_HOST = 'github.com';
    // Above this many other hives the cell shows a count instead of the full
    // list, with every hive still readable in the title tooltip. Four keeps a
    // typical footprint inline while stopping a heavy user from turning one
    // table cell into a wall of hive IDs.
    var PAST_REQUESTS_MAX_INLINE_OTHER_HIVES = 4;
    // Absent key means collapsed; only an explicit 'false' expands the section.
    var _pastRequestsCollapsed = localStorage.getItem(PAST_REQUESTS_COLLAPSED_KEY) !== 'false';

    // applyPastRequestsCollapsed pushes _pastRequestsCollapsed onto the DOM.
    // Split out from the toggle so the initial render can restore the persisted
    // state without duplicating the show/hide and aria bookkeeping.
    function applyPastRequestsCollapsed() {
      var body = document.getElementById('past-requests-body');
      var toggle = document.getElementById('past-requests-toggle');
      var header = document.getElementById('past-requests-header');
      if (body) body.style.display = _pastRequestsCollapsed ? 'none' : '';
      // ▸ collapsed / ▾ expanded
      if (toggle) toggle.innerHTML = _pastRequestsCollapsed ? '&#9656;' : '&#9662;';
      if (header) header.setAttribute('aria-expanded', _pastRequestsCollapsed ? 'false' : 'true');
    }

    function togglePastRequests() {
      _pastRequestsCollapsed = !_pastRequestsCollapsed;
      localStorage.setItem(PAST_REQUESTS_COLLAPSED_KEY, _pastRequestsCollapsed ? 'true' : 'false');
      applyPastRequestsCollapsed();
    }

    // githubHostPill renders the GitHub instance a request targets. Blank
    // github_host means public github.com; a GHE host is called out with the
    // accent treatment so the admin can place the hive on a cluster that can
    // reach it. Shared by the pending action cards and the Past Requests table
    // so the same concept does not drift into two different styles.
    function githubHostPill(githubHost) {
      /* Public github.com reads green, GitHub Enterprise reads blue, so the
         two instances are distinguishable at a glance without reading the
         text. An absent host is treated as public, matching the label below. */
      var isGHE = !!githubHost && githubHost !== PAST_REQUESTS_DEFAULT_GITHUB_HOST;
      var border = isGHE ? 'var(--blue);color:var(--blue)' : 'var(--green);color:var(--green)';
      return '<span style="font-size:0.68rem;padding:1px 7px;border-radius:999px;border:1px solid ' + border + '">' +
        esc(githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST) + '</span>';
    }

    // otherHivesCell summarises the rest of a requester's fleet: which other
    // hives they can sign in to and their role on each, so an admin reading one
    // row sees the person's whole footprint. The list arrives on the payload
    // (admin-only) so this never costs an API call per row.
    function otherHivesCell(otherHives) {
      var list = (otherHives || []).filter(function(e) { return e && e.hive_id; });
      if (!list.length) return '<span style="color:var(--muted);font-size:0.7rem">—</span>';
      // Plain text for the tooltip — a title attribute is not HTML, and esc()
      // does not escape quotes, so nothing user-controlled may be interpolated
      // into the attribute string. Set it via the DOM instead.
      var full = list.map(function(e) { return e.hive_id + ' · ' + (e.role || '?'); }).join('\n');
      var span = document.createElement('span');
      span.style.fontSize = '0.7rem';
      if (list.length > PAST_REQUESTS_MAX_INLINE_OTHER_HIVES) {
        span.textContent = list.length + ' hives';
        span.style.color = 'var(--muted)';
        span.style.borderBottom = '1px dotted var(--border)';
        span.style.cursor = 'help';
      } else {
        span.textContent = list.map(function(e) {
          return e.hive_id + ' · ' + (e.role || '?');
        }).join(', ');
        span.style.color = 'var(--muted)';
        span.style.fontFamily = 'ui-monospace,monospace';
      }
      span.title = full;
      // outerHTML of a textContent-populated node is escaped by the DOM, which
      // covers quotes and apostrophes that esc() would leave through.
      return span.outerHTML;
    }

    // renderRequestHistory renders every already-decided request as a table:
    // who asked, for what, what was decided, by whom, and — for an approval —
    // which hive they actually got. Pending requests stay as action cards above;
    // this is the audit trail, not a work queue, so it is deliberately a dense
    // table rather than more cards.
    // provisionRepoLabel renders a provision request's org/repo as a link to
    // the repository on the GitHub instance the request actually targets.
    // github_host is authoritative: '' means public github.com, a value like
    // github.ibm.com means GitHub Enterprise — hardcoding github.com would send
    // an admin to a 404 (or worse, an unrelated public repo of the same name).
    // Requests with no repo recorded fall back to plain escaped text.
    /* provisionRequesterPrimary returns the identifier admins should scan first
       on provision-request rows. The durable request key (username) may be an
       opaque native provider subject such as "ibmid:695000VVZ9"; user_id is the
       GitHub/GHE login or best human-readable identity captured/enriched by the
       server. Older records without user_id fall back to username. */
    function provisionRequesterPrimary(pr) {
      if (!pr) return '';
      var id = String(pr.user_id || '').trim();
      return id || String(pr.username || '').trim();
    }

    function provisionRequesterNativeSubject(pr) {
      if (!pr) return '';
      var native = String(pr.username || '').trim();
      var primary = provisionRequesterPrimary(pr);
      return native && primary && native !== primary ? native : '';
    }

    function provisionNativeSubjectChip(pr) {
      var native = provisionRequesterNativeSubject(pr);
      if (!native) return '';
      return '<span title="Native provider subject" style="font-size:0.68rem;padding:1px 6px;border-radius:999px;border:1px solid var(--border);color:var(--muted);font-family:ui-monospace,monospace">' +
        esc(native) + '</span>';
    }

    function provisionRequesterAvatar(pr, px, extraStyle) {
      var primary = provisionRequesterPrimary(pr);
      if (pr && pr.user_id_source === 'github') {
        return linkedAvatar(primary, px, primary, extraStyle);
      }
      return userAvatar({display_name: primary, github_username: String((pr && pr.username) || '')}, px, extraStyle);
    }

    /* provisionRequesterLabel renders the requester's real name, Slack ID, and
       native provider subject beside the primary user ID on the review card.
       Mapping a login to a person is the reason the wizard asks for a name at
       all, and this is where the operator deciding the request needs it.

       Both are free text a user typed. esc() everywhere, and the whole label is
       built as a text node's escaped HTML rather than interpolated into an
       attribute — nothing here is ever placed inside an inline handler, where
       esc() alone would leave an apostrophe live. Older requests predate these
       fields and simply render nothing. */
    function provisionRequesterLabel(pr) {
      if (!pr) return '';
      var name = (pr.full_name || '').trim();
      var slack = (pr.slack_id || '').trim();
      var native = provisionRequesterNativeSubject(pr);
      if (!name && !slack && !native) return '';
      var primary = provisionRequesterPrimary(pr);
      var bits = [];
      if (name && name !== primary) bits.push(esc(name));
      if (slack) bits.push('slack: ' + esc(slack));
      if (native) bits.push(provisionNativeSubjectChip(pr));
      if (!bits.length) return '';
      return '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' +
        bits.join(' &middot; ') + '</span>';
    }

    function provisionRepoLabel(pr) {
      if (!pr) return '';
      var org = pr.org || '';
      var repo = pr.primary_repo || pr.repos || '';
      var text = esc(org) + '/' + esc(repo);
      // repos may be a comma-separated list; only a single concrete repo can be
      // turned into a URL, so leave a multi-repo value as plain text.
      var url = (repo.indexOf(',') === -1) ? ghRepoURL(pr.github_host, org, repo) : '';
      if (!url) return text;
      return '<a href="' + escAttr(url) + '" target="_blank" rel="noopener noreferrer"' +
        ' style="color:inherit;text-decoration:underline;text-decoration-style:dotted">' + text + '</a>';
    }

    function renderRequestHistory(requests) {
      var host = document.getElementById('admin-request-history');
      if (!host) return;
      var decided = (requests || []).filter(function(pr) {
        return pr && normalizeProvisionStatus(pr.status) !== '' && normalizeProvisionStatus(pr.status) !== 'pending';
      });
      // Restore the persisted collapse state on every render — the section is
      // rebuilt on each dashboard poll, so applying it once at load is not enough.
      applyPastRequestsCollapsed();
      var countEl = document.getElementById('past-requests-count');
      if (countEl) countEl.textContent = decided.length ? '(' + decided.length + ')' : '';
      if (!decided.length) {
        host.innerHTML = '<div style="color:var(--muted);font-size:0.75rem;padding:6px 0">No past requests yet.</div>';
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
        var primary = provisionRequesterPrimary(pr);
        // Link the requester avatar only when the server says the primary user
        // ID is a GitHub login. Email/name fallbacks get an initials avatar
        // instead, avoiding bogus github.com links for OIDC subjects.
        //
        // The AVATAR carries that link now. The username used to be a second
        // anchor to the same profile; two controls for one destination is noise,
        // so the name is plain text and the face is the affordance.
        var userCell =
          provisionRequesterAvatar(pr, PANEL_ACCESS_AVATAR_PX, 'margin-right:6px') +
          (primary
            ? '<span>' + esc(primary) + '</span>' + provisionRequesterLabel(pr)
            : '<span style="color:var(--muted)">—</span>');
        // Repo link, built by the shared provisionRepoLabel(): github_host is
        // empty for public github.com and otherwise a GitHub Enterprise host
        // (github.ibm.com, …), so the origin comes from the record rather than
        // being hardcoded — hardcoding github.com would produce dead links for
        // every Enterprise requester. The helper also declines to link a
        // comma-separated multi-repo value, which has no single URL, and falls
        // back to plain escaped text when there is nothing to link.
        var org = pr.org || '';
        var repo = pr.primary_repo || pr.repos || '';
        var repoCell = (org || repo)
          ? provisionRepoLabel(pr)
          : '<span style="color:var(--muted)">—</span>';
        // For an approval the outcome is the hive they received — linked to the
        // existing SSO handoff endpoint, the same affordance My Hives uses —
        // plus their role on it. For a denial it is the reason, if one was
        // given. Legacy records have neither.
        var outcome;
        if (approved && pr.assigned_hive) {
          var roleSuffix = pr.assigned_role
            ? ' <span style="color:var(--muted);font-size:0.68rem">&middot; ' + esc(pr.assigned_role) + '</span>'
            : '';
          outcome =
            '<a href="/api/saas/hives/' + encodeURIComponent(pr.assigned_hive) + '/open" target="_blank" rel="noopener noreferrer" style="font-family:ui-monospace,monospace;font-size:0.7rem;color:inherit" title="Open dashboard">' +
            esc(pr.assigned_hive) + '</a>' + roleSuffix;
        } else if (!approved && pr.deny_reason) {
          outcome = '<span style="color:var(--muted)">' + esc(pr.deny_reason) + '</span>';
        } else {
          outcome = '<span style="color:var(--muted)">—</span>';
        }
        return '<tr>' +
          '<td style="white-space:nowrap">' + userCell + '</td>' +
          '<td>' + repoCell + '</td>' +
          '<td style="white-space:nowrap">' + githubHostPill(pr.github_host) + '</td>' +
          '<td style="white-space:nowrap">' + acmmBadge(pr.acmm_level) + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.requested_at || '').substring(0, 10)) + '</td>' +
          '<td style="white-space:nowrap"><span style="color:' + color + ';font-weight:600;font-size:0.72rem">' + esc(pr.status) + '</span></td>' +
          '<td style="white-space:nowrap">' + esc(pr.decided_by || '—') + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.decided_at || '').substring(0, 10) || '—') + '</td>' +
          '<td>' + outcome + '</td>' +
          '<td>' + otherHivesCell(pr.other_hives) + '</td>' +
          '</tr>';
      }).join('');
      // Header cells here MUST stay in lockstep with the <td> cells emitted
      // above — 10 columns: User, Requested, Host, ACMM, On, Decision, By,
      // When, Assigned / reason, Other hives.
      host.innerHTML =
        '<table class="hive-table" style="width:100%;font-size:0.78rem">' +
        '<thead><tr><th>User</th><th>Requested</th><th>Host</th><th>ACMM</th><th>On</th>' +
        '<th>Decision</th><th>By</th><th>When</th><th>Assigned / reason</th><th>Other hives</th></tr></thead>' +
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
      // Only genuinely pending requests get Approve/Deny cards. Anything already
      // approved or denied belongs in Past Requests, never in the action queue.
      var pending = (requests || []).filter(function(pr) {
        if (!pr) return false;
        var st = normalizeProvisionStatus(pr.status);
        return st === '' || st === 'pending';
      });
      if (!pending.length) {
        // Keep the section visible when there is history to show, so the table
        // does not vanish along with the empty queue.
        var anyDecided = (requests || []).some(function(pr) {
          if (!pr) return false;
          var st = normalizeProvisionStatus(pr.status);
          return st !== '' && st !== 'pending';
        });
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
        var primary = provisionRequesterPrimary(pr);
        var avatar = provisionRequesterAvatar(pr, TABLE_AVATAR_PX, 'margin-right:8px');
        return '<div style="display:flex;align-items:center;justify-content:space-between;padding:10px 14px;background:var(--surface);border:1px solid var(--border);border-radius:8px;margin-bottom:8px">' +
          '<div style="display:flex;align-items:center;gap:8px">' +
          avatar +
          '<div>' +
          '<span style="font-size:0.85rem;font-weight:600">' + esc(primary || pr.username) + '</span>' +
          // Who the requester actually IS. Mapping a login to a person is the
          // reason the wizard asks for a name, and this review card is where an
          // operator needs it. esc() only — free text, never markup.
          provisionRequesterLabel(pr) +
          // org/repo links to the repo on the instance the request targets.
          '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' + provisionRepoLabel(pr) + '</span>' +
          // Show which GitHub instance the request targets — same pill the Past
          // Requests table uses, so pending and decided rows read alike.
          ' ' + githubHostPill(pr.github_host) +
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

    /* openAssignForUser is the entry point from a click on a user in the admin
       users table. It routes to whichever existing flow fits, rather than adding
       a third assign path:
         - the user has a PENDING provision request  → openApproveModal, which
           approves the request AND assigns a placeholder in one step;
         - otherwise → openAssignModal on an available placeholder, pre-filled
           with the user as owner. openAssignModal needs a concrete placeholder
           id, so we ask the same endpoint the approve picker uses.
       Authorization is unchanged: both modals post to admin-only endpoints
       (/api/saas/approve-provision, /api/saas/hives/{id}/assign), which
       re-check the caller server-side, so this is purely a UI shortcut. */
    async function openAssignForUser(username) {
      if (!_isAdmin) return;
      if (_provisionRequestsByUser[username]) { openApproveModal(username); return; }
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        if (!resp.ok) { hiveToast('Could not load available placeholders', 'error'); return; }
        var data = await resp.json();
        var placeholders = data.placeholders || [];
        if (!placeholders.length) {
          hiveToast('No available placeholder hives to assign — provision one first.', 'error');
          return;
        }
        openAssignModal(placeholders[0].id, username, placeholders);
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

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
        '<div><span style="color:var(--muted)">GitHub:</span> ' + esc(pr.github_host || PUBLIC_GITHUB_HOST) + '</div>' +
        '</div>';
      /* The requester's own github_host pre-fills the picker — they already
         told us which GitHub their org lives on. It is only the cluster
         default when the request carries none. The cluster is unknown until a
         placeholder is picked (auto-pick resolves it server-side), so the
         picker opens on "Cluster default" and the note fills in on selection. */
      var approveRequestHost = pr.github_host || '';
      var content =
        summary +
        '<label style="' + lbl + '">Placeholder to assign</label>' +
        '<select id="approve-hive" style="' + fld + '"><option value="">Loading placeholders…</option></select>' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-top:6px">Auto-pick chooses an available placeholder from the request&#39;s pool.</div>' +
        githubHostChoiceMarkup('approve', '', approveRequestHost, fld, lbl) +
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
      /* No cluster is known yet (auto-pick is the default selection), so the
         "Cluster default" note stays generic until a placeholder is chosen. */
      wireGitHubHostChoice('approve', '');
      // Populate the dropdown with available placeholders (auto-pick default).
      var _approvePlaceholders = [];
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        var data = await resp.json();
        var sel = document.getElementById('approve-hive');
        if (!sel) return;
        _approvePlaceholders = data.placeholders || [];
        var opts = '<option value="">Auto-pick</option>';
        _approvePlaceholders.forEach(function(p) {
          opts += '<option value="' + esc(p.id) + '">' + esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        });
        sel.innerHTML = opts;
        /* Picking a concrete placeholder reveals its cluster, so the "Cluster
           default" option can finally name the GitHub instance the hive will
           land on. Auto-pick ('') leaves it unnamed — the server resolves the
           cluster and backfills the host itself. */
        sel.addEventListener('change', function() {
          var ph = (_approvePlaceholders || []).reduce(function(m, p) { return (p && p.id === sel.value) ? p : m; }, null);
          var host = ph ? clusterGitHubHost(ph.cluster_id || '') : '';
          var hostSel = document.getElementById('approve-ghhost');
          if (hostSel && hostSel.options && hostSel.options.length) {
            hostSel.options[0].textContent = host ? 'Cluster default — ' + host : 'Cluster default';
          }
          wireGitHubHostChoice('approve', host);
        });
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
          /* github_host: '' = use the request's host, else the cluster
             default (server-side); 'public' = force public github.com. */
          body: JSON.stringify({hive_id: hiveId, github_host: githubHostChoiceValue('approve')})
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


    // --- Cluster Health Panel ---
    var CLUSTER_HEALTH_POLL_MS = 30000;
    var CLUSTER_CPU_WARN_PCT = 60;
    var CLUSTER_CPU_DANGER_PCT = 80;
    var CLUSTER_MEM_WARN_PCT = 60;
    var CLUSTER_MEM_DANGER_PCT = 80;
    // Disk thresholds mirror kubelet's own behaviour on these nodes rather
    // than round numbers: image garbage collection starts at
    // imageGCHighThresholdPercent (85% used) and hard eviction fires at
    // evictionHard nodefs.available<10% (90% used). Amber therefore means
    // "kubelet is already reclaiming disk", red means "pods are being evicted".
    var CLUSTER_DISK_WARN_PCT = 85;
    var CLUSTER_DISK_DANGER_PCT = 90;
    // MiB per GiB, for rendering disk_*_mb values as GiB.
    var MB_PER_GB = 1024;
    var _clusterHealthCollapsed = localStorage.getItem('hive-cluster-health-collapsed') !== 'false';

    function toggleClusterHealth() {
      _clusterHealthCollapsed = !_clusterHealthCollapsed;
      localStorage.setItem('hive-cluster-health-collapsed', _clusterHealthCollapsed ? 'true' : 'false');
      var body = document.getElementById('cluster-health-body');
      var toggle = document.getElementById('cluster-health-toggle');
      if (body) body.style.display = _clusterHealthCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _clusterHealthCollapsed ? '' : 'rotate(90deg)';
    }

    /* ---- Scale Controls (admin) ----
       Renders inputs from GET /api/saas/admin/scale-settings and saves the
       whole document via POST. The GET payload carries saved values,
       EFFECTIVE values (after the saved > env > default chain) and the
       built-in defaults, so each input can show what is actually in force. */
    var _scaleControlsCollapsed = localStorage.getItem('hive-scale-controls-collapsed') !== 'false';
    var _scaleData = null;
    function toggleScaleControls() {
      _scaleControlsCollapsed = !_scaleControlsCollapsed;
      localStorage.setItem('hive-scale-controls-collapsed', _scaleControlsCollapsed ? 'true' : 'false');
      var body = document.getElementById('scale-controls-body');
      var toggle = document.getElementById('scale-controls-toggle');
      if (body) body.style.display = _scaleControlsCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _scaleControlsCollapsed ? '' : 'rotate(90deg)';
    }
    var SCALE_GLOBAL_KNOBS = [
      { key: 'upgrade_wave_size', label: 'Upgrade wave size', hint: 'auto-upgrades per cluster per tick' },
      { key: 'provision_workers', label: 'Provision workers', hint: 'total concurrent provisions (grows live; shrink needs restart)' },
      { key: 'provision_per_cluster', label: 'Provisions per cluster', hint: 'concurrent provisions per target cluster' },
      { key: 'kubectl_per_cluster', label: 'kubectl per cluster', hint: 'concurrent kubectl processes per cluster' }
    ];
    function scaleNumInput(id, value, placeholder) {
      return '<input type="number" min="0" max="10000" id="' + id + '" value="' + (value || '') + '"' +
        ' placeholder="' + placeholder + '"' +
        ' style="width:90px;padding:6px 8px;background:var(--surface);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;text-align:right">';
    }
    async function loadScaleSettings() {
      if (!_isAdmin) return;
      try {
        var resp = await fetch('/api/saas/admin/scale-settings');
        if (!resp.ok) { document.getElementById('scale-controls-section').style.display = 'none'; return; }
        var data = await resp.json();
        _scaleData = data;
        document.getElementById('scale-controls-section').style.display = '';
        var body = document.getElementById('scale-controls-body');
        var toggle = document.getElementById('scale-controls-toggle');
        if (body) body.style.display = _scaleControlsCollapsed ? 'none' : '';
        if (toggle) toggle.style.transform = _scaleControlsCollapsed ? '' : 'rotate(90deg)';
        var eff = data.effective || {};
        document.getElementById('scale-controls-summary').textContent =
          'wave ' + eff.upgrade_wave_size + ' · workers ' + eff.provision_workers +
          ' · per-cluster ' + eff.provision_per_cluster + ' · kubectl ' + eff.kubectl_per_cluster;
        /* Don't clobber in-progress edits: only re-render when collapsed or
           on first load. */
        if (document.activeElement && document.activeElement.id && document.activeElement.id.indexOf('scale-') === 0) return;
        var saved = data.saved || {};
        var defaults = data.defaults || {};
        document.getElementById('scale-globals').innerHTML = SCALE_GLOBAL_KNOBS.map(function(k) {
          return '<div style="padding:12px;border:1px solid var(--border);border-radius:8px;background:var(--surface)">' +
            '<div style="font-size:0.8rem;font-weight:600;margin-bottom:2px">' + k.label + '</div>' +
            '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:8px">' + k.hint + '</div>' +
            '<div style="display:flex;align-items:center;gap:8px">' +
            scaleNumInput('scale-' + k.key, saved[k.key], 'default ' + defaults[k.key]) +
            '<span style="font-size:0.75rem;color:var(--muted)">in force: ' + (eff[k.key] != null ? eff[k.key] : '?') + '</span>' +
            '</div></div>';
        }).join('');
        var overrides = saved.clusters || {};
        document.getElementById('scale-clusters-body').innerHTML = (data.clusters || []).map(function(c) {
          var o = overrides[c.id] || {};
          function cell(field, effective) {
            var savedVal = (o[field] != null) ? o[field] : '';
            return '<td style="text-align:right;padding:6px 4px">' +
              scaleNumInput('scale-c-' + c.id + '-' + field, savedVal, String(effective)) + '</td>';
          }
          return '<tr><td style="padding:6px 4px">' + c.id + '</td>' +
            '<td style="text-align:right;padding:6px 4px">' + c.hives + '</td>' +
            '<td style="text-align:right;padding:6px 4px">' + c.available_placeholders + '</td>' +
            cell('max_hives', c.max_hives) + cell('pool_min', c.pool_min) + cell('pool_target', c.pool_target) + '</tr>';
        }).join('');
      } catch (e) { /* transient — next poll retries */ }
    }
    async function saveScaleSettings() {
      var status = document.getElementById('scale-save-status');
      function intOrZero(id) {
        var el = document.getElementById(id);
        if (!el || el.value === '') return 0;
        var n = parseInt(el.value, 10);
        return isNaN(n) ? 0 : n;
      }
      var body = {
        upgrade_wave_size: intOrZero('scale-upgrade_wave_size'),
        provision_workers: intOrZero('scale-provision_workers'),
        provision_per_cluster: intOrZero('scale-provision_per_cluster'),
        kubectl_per_cluster: intOrZero('scale-kubectl_per_cluster'),
        clusters: {}
      };
      ((_scaleData && _scaleData.clusters) || []).forEach(function(c) {
        var row = {};
        ['max_hives', 'pool_min', 'pool_target'].forEach(function(f) {
          var el = document.getElementById('scale-c-' + c.id + '-' + f);
          /* Blank = no override (fall back to clusters.json). An explicit
             number — including 0 — is an override. */
          if (el && el.value !== '') {
            var n = parseInt(el.value, 10);
            if (!isNaN(n)) row[f] = n;
          }
        });
        if (Object.keys(row).length > 0) body.clusters[c.id] = row;
      });
      try {
        var resp = await fetch('/api/saas/admin/scale-settings', {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
        });
        if (!resp.ok) {
          var err = {};
          try { err = await resp.json(); } catch (_) {}
          status.textContent = 'Save failed: ' + (err.error || resp.status);
          status.style.color = 'var(--red)';
          return;
        }
        status.textContent = 'Saved — in effect now.';
        status.style.color = 'var(--green)';
        setTimeout(function() { status.textContent = ''; }, 5000);
        loadScaleSettings();
      } catch (e) {
        status.textContent = 'Save failed: ' + e;
        status.style.color = 'var(--red)';
      }
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

    // Renders a metric row whose value could not be collected. Deliberately
    // dashed rather than 0%, so an unreachable node never reads as healthy.
    function renderHealthMetricUnknown(label, reason) {
      return '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:4px" title="' + esc(reason || 'No data') + '">' +
        '<span style="font-size:0.7rem;color:var(--muted)">' + label + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' +
        '<span style="font-family:monospace;font-size:0.75rem;color:var(--muted)">— / — GiB</span>' +
        '<span style="font-size:0.7rem;min-width:28px;text-align:right;color:var(--muted)">—</span>' +
        '</span></div>';
    }

    // Disk usage is optional: the collector omits it when the kubelet
    // stats endpoint was unreachable, so absent means unknown, not zero.
    function renderNodeDiskMetric(n, nk) {
      if (n.disk_percent == null || n.disk_total_mb == null || n.disk_used_mb == null) {
        return renderHealthMetricUnknown('DISK', 'Disk usage unavailable for this node');
      }
      var diskUsedGB = (n.disk_used_mb / MB_PER_GB).toFixed(1);
      var diskTotalGB = Math.round(n.disk_total_mb / MB_PER_GB);
      return renderHealthMetric('DISK', diskUsedGB, diskTotalGB, 'GiB', n.disk_percent,
        CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT, nk, 'disk');
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
        renderNodeDiskMetric(n, nk) +
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
          // Disk is omitted when no node reported usage; showing 0% there
          // would claim the fleet is healthy on no evidence at all.
          var diskSegment = '';
          if (s.total_disk_percent != null) {
            pushSparkPoint('_cluster', 'disk', s.total_disk_percent);
            var diskColor = healthBarColor(s.total_disk_percent, CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT);
            diskSegment = renderUnicodeSparkline('_cluster', 'disk', diskColor) +
              ' <span style="color:' + diskColor + '">' + s.total_disk_percent + '% disk</span> · ';
          } else {
            diskSegment = '<span style="color:var(--muted)" title="No node reported live disk usage">— disk</span> · ';
          }
          summaryBar.innerHTML = clusterCount + ' cluster' + (clusterCount !== 1 ? 's' : '') + ' · ' +
            (s.total_nodes || 0) + ' nodes · ' + (s.total_cpu_cores || 0) + ' vCPU · ' +
            renderUnicodeSparkline('_cluster', 'cpu', cpuColor) + ' <span style="color:' + cpuColor + '">' + (s.total_cpu_percent || 0) + '% cpu</span> · ' +
            renderUnicodeSparkline('_cluster', 'mem', memColor) + ' <span style="color:' + memColor + '">' + (s.total_mem_percent || 0) + '% mem</span> · ' +
            diskSegment +
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
            // Absent disk data (e.g. an unreachable cluster) renders dashed,
            // never as 0% — a full disk and an unknown disk must not look alike.
            var cDiskSegment;
            if (cs.total_disk_percent != null) {
              var cDiskColor = healthBarColor(cs.total_disk_percent, CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT);
              cDiskSegment = '<span style="color:' + cDiskColor + '">' + cs.total_disk_percent + '% disk</span> · ';
            } else {
              cDiskSegment = '<span style="color:var(--muted)" title="No node in this cluster reported live disk usage">— disk</span> · ';
            }
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
            // Stuck (orphaned Terminating) hive pods — the residue of nodes
            // disappearing without draining (#5328). ABSENT means the hub could
            // not determine it and nothing is claimed; a present zero is
            // deliberately silent so a healthy fleet stays quiet. Only a real
            // non-zero count renders, because the whole cost of this incident
            // was that 27 orphans across 15 namespaces accumulated for three
            // weeks with nothing reporting them.
            var stuckLine = '';
            if (c.stuck_pods && c.stuck_pods.total > 0) {
              var sp = c.stuck_pods;
              var nsList = (sp.namespaces || []).map(function(x) { return x.namespace + ' (' + x.count + ')'; }).join(', ');
              if (sp.truncated) { nsList += ', …'; }
              var stuckTitle = 'Hive pods stuck Terminating past the reaper threshold: deletionTimestamp set, no finalizers, not Running. ' +
                'Signature of a node removed without draining. Affected namespaces: ' + nsList;
              stuckLine = ' · <span style="color:var(--red)" title="' + esc(stuckTitle) + '">' +
                sp.total + ' stuck pod' + (sp.total === 1 ? '' : 's') +
                ' in ' + sp.namespaces_affected + ' ns</span>';
            }
            var errorLine = c.error ? '<div style="margin:8px 0;padding:6px 10px;background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.3);border-radius:6px;font-size:0.75rem;color:var(--red)">' + esc(c.error) + '</div>' : '';
            var headerHtml = '<div style="display:flex;align-items:center;gap:8px;margin:16px 0 8px">' +
              clusterBadge(c.id, c.name) +
              '<span style="font-size:0.85rem;color:var(--text);font-weight:600">' + esc(cLabel) + '</span>' +
              '<span style="font-size:0.75rem;color:var(--muted)">' +
              (cs.total_nodes || 0) + ' nodes · ' + (cs.total_cpu_cores || 0) + ' vCPU · ' +
              '<span style="color:' + cCpuColor + '">' + (cs.total_cpu_percent || 0) + '% cpu</span> · ' +
              '<span style="color:' + cMemColor + '">' + (cs.total_mem_percent || 0) + '% mem</span> · ' +
              cDiskSegment +
              (c.hive_count || 0) + ' hives' + capacityLine + gpuLine + stuckLine +
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

    // --- PR Reach Panel (#3995, phase 2c of #3973) ---
    // Admin-only, same contract as the Cluster Health panel above: hidden
    // until /api/reach answers OK, hidden again on 403, collapse state in
    // localStorage. Polls far slower than cluster health — every load fans
    // out merged-PR + ancestry lookups hub-side (GitHub API), so 5 minutes
    // is the courtesy floor, and reach itself moves at heartbeat cadence.
    var REACH_POLL_MS = 300000;
    var _reachCollapsed = localStorage.getItem('hive-reach-collapsed') !== 'false';

    function toggleReach() {
      _reachCollapsed = !_reachCollapsed;
      localStorage.setItem('hive-reach-collapsed', _reachCollapsed ? 'true' : 'false');
      var body = document.getElementById('reach-body');
      var toggle = document.getElementById('reach-toggle');
      if (body) body.style.display = _reachCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _reachCollapsed ? '' : 'rotate(90deg)';
    }

    /* Renders merge→first-execution latency compactly; null means nothing
       qualifying has executed yet and must read as absent, never as 0. */
    function reachLatency(seconds) {
      if (seconds == null || !isFinite(seconds) || seconds < 0) return '<span style="color:var(--muted)">—</span>';
      var SEC_PER_MIN = 60, SEC_PER_HOUR = 3600, SEC_PER_DAY = 86400;
      if (seconds < SEC_PER_HOUR) return Math.round(seconds / SEC_PER_MIN) + 'm';
      if (seconds < SEC_PER_DAY) return (seconds / SEC_PER_HOUR).toFixed(1) + 'h';
      return (seconds / SEC_PER_DAY).toFixed(1) + 'd';
    }

    /* Renders the error-delta cell from a report's per-component deltas.
       The measured flag is the contract (#3995): an unmeasured delta must be
       DISTINGUISHABLE from a measured zero, so unmeasured renders as the word
       "unmeasured", never as 0.0. Measured deltas show the worst (largest
       magnitude) component, red when errors rose, green when they fell; the
       tooltip itemizes every component either way. */
    function reachDeltaCell(deltas) {
      deltas = deltas || [];
      var measured = deltas.filter(function(d) { return d && d.measured; });
      var PCT = 100;
      var tip = deltas.map(function(d) {
        if (!d) return '';
        if (!d.measured) return d.component + ': unmeasured';
        return d.component + ': ' + (d.delta >= 0 ? '+' : '') + (d.delta * PCT).toFixed(2) +
          'pp over ' + d.windows_compared + 'w (' + (d.error_rate_before * PCT).toFixed(2) +
          '% -> ' + (d.error_rate_after * PCT).toFixed(2) + '%)';
      }).filter(Boolean).join('\n');
      if (!measured.length) {
        return '<span style="color:var(--muted)" title="' + esc(tip || 'No deploy window observed yet, or not enough before/after history') + '">unmeasured</span>';
      }
      var worst = measured[0];
      measured.forEach(function(d) { if (Math.abs(d.delta) > Math.abs(worst.delta)) worst = d; });
      var color = worst.delta > 0 ? 'var(--red)' : (worst.delta < 0 ? 'var(--green)' : 'var(--muted)');
      var sign = worst.delta >= 0 ? '+' : '';
      return '<span style="color:' + color + ';font-family:monospace" title="' + esc(tip) + '">' +
        sign + (worst.delta * PCT).toFixed(2) + 'pp</span>';
    }

    function renderReachRow(r) {
      var comps = (r.attribution && r.attribution.components) || [];
      var coveragePct = Math.round(((r.attribution && r.attribution.coverage) || 0) * 100);
      var compCell = comps.length
        ? '<span title="Attribution coverage ' + coveragePct + '% of changed files">' + esc(comps.join(', ')) + '</span>'
        : '<span style="color:var(--muted)" title="No changed file maps to an instrumented component (docs/workflows/deploy)">unattributable</span>';
      var hives = r.reach_hives || [];
      var reachCell = r.reach_count
        ? '<span title="' + esc(hives.join(', ')) + '">' + r.reach_count + '</span>'
        : '<span style="color:var(--muted)">0</span>';
      var flags = [];
      if (r.never_ran) flags.push('<span style="padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(239,68,68,0.15);color:var(--red);border:1px solid rgba(239,68,68,0.3)" title="Merged and deployed, but no touched component has executed anywhere in ' + (r.never_ran_threshold_days || 0) + ' days — the category acceptance-rate cannot see">never ran</span>');
      else if (!r.deployed) flags.push('<span style="padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(107,114,128,0.15);color:#9ca3af;border:1px solid rgba(107,114,128,0.3)" title="No hive reports running a commit containing this PR yet (merged is not deployed)">not deployed</span>');
      if ((r.shared_with || []).length) flags.push('<span style="padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(128,191,255,0.15);color:#80bfff;border:1px solid rgba(128,191,255,0.3)" title="Co-deployed with PR ' + esc((r.shared_with || []).map(function(n) { return '#' + n; }).join(', ')) + ' — reach and deltas are shared by construction (D4)">shared</span>');
      return '<tr>' +
        '<td style="white-space:nowrap"><a href="https://github.com/hivecommons/hive/pull/' + r.pr + '" target="_blank">#' + r.pr + '</a>' +
        (r.title ? ' <span style="color:var(--muted);font-size:0.75rem" title="' + esc(r.title) + '">' + esc(r.title.length > 48 ? r.title.slice(0, 48) + '…' : r.title) + '</span>' : '') + '</td>' +
        '<td>' + compCell + '</td>' +
        '<td>' + reachCell + '</td>' +
        '<td>' + reachLatency(r.first_execution_latency_seconds) + '</td>' +
        '<td>' + reachDeltaCell(r.error_deltas) + '</td>' +
        '<td style="white-space:nowrap">' + (flags.join(' ') || '<span style="color:var(--muted)">—</span>') + '</td>' +
        '</tr>';
    }

    async function loadReach() {
      if (!_isAdmin) return;
      try {
        var resp = await fetch('/api/reach');
        if (resp.status === 403) {
          document.getElementById('reach-section').style.display = 'none';
          return;
        }
        /* 503 = hub running without GitHub credentials: no PR facts exist, so
           the section stays hidden rather than rendering an empty shell. */
        if (!resp.ok) return;
        var data = await resp.json();
        document.getElementById('reach-section').style.display = '';

        var reports = data.reports || [];
        var summaryBar = document.getElementById('reach-summary-bar');
        if (summaryBar) {
          var neverRan = reports.filter(function(r) { return r.never_ran; }).length;
          var reached = reports.filter(function(r) { return (r.reach_count || 0) > 0; }).length;
          summaryBar.textContent = reports.length + ' merged PR' + (reports.length !== 1 ? 's' : '') +
            ' · ' + (data.hives_reporting || 0) + ' hives reporting · ' + reached + ' reached' +
            (neverRan ? ' · ' + neverRan + ' never ran' : '');
        }

        var body = document.getElementById('reach-body');
        var toggle = document.getElementById('reach-toggle');
        if (!_reachCollapsed) {
          if (body) body.style.display = '';
          if (toggle) toggle.style.transform = 'rotate(90deg)';
        }

        var container = document.getElementById('reach-table-container');
        if (!container) return;
        if (!reports.length) {
          container.innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No recently merged PRs to report on</div>';
          return;
        }
        container.innerHTML =
          '<table class="hive-table"><thead><tr>' +
          '<th style="text-align:left">PR</th><th>Components</th><th title="Distinct hives running a commit containing the PR whose touched components executed since merge">Reach (hives)</th><th title="Merge to first qualifying execution anywhere in the fleet">First exec</th><th title="Span error-rate after vs before the PR&#39;s deploy window; unmeasured when either side lacks data (#3995)">Error Δ</th><th>Flags</th>' +
          '</tr></thead><tbody>' + reports.map(renderReachRow).join('') + '</tbody></table>';
      } catch(e) {
        console.error('reach error:', e);
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
          /* Surface the GitHub host: on a GitHub Enterprise cluster the App
             lives on that GHE instance, and picking the wrong cluster is the
             difference between an install request the org admin can see and
             one that silently lands on public github.com. */
          if (c.github_host && c.github_host !== PUBLIC_GITHUB_HOST) caps.push(c.github_host);
          var label = c.name || c.id;
          if (caps.length) label += ' (' + caps.join(', ') + ')';
          return '<option value="' + esc(c.id) + '">' + esc(label) + '</option>';
        }).join('');
        sel.removeEventListener('change', syncCreateModalInstallLinks);
        sel.addEventListener('change', syncCreateModalInstallLinks);
        syncCreateModalInstallLinks();
      } catch(e) { /* cluster dropdown stays at default */ }
    }

    /* PUBLIC_GITHUB_HOST is the bare hostname the hub reports for a cluster
       with no GitHub Enterprise override. Anything else is a GHE instance. */
    var PUBLIC_GITHUB_HOST = 'github.com';

    /* selectedClusterEntry returns the ClusterListEntry for the cluster chosen
       in the create-hive modal, or null when the list has not loaded. */
    function selectedClusterEntry() {
      var sel = document.getElementById('f-cluster');
      if (!sel || !sel.value) return null;
      var list = _clusterList || [];
      for (var i = 0; i < list.length; i++) {
        if (list[i] && list[i].id === sel.value) return list[i];
      }
      return null;
    }

    /* PUBLIC_GITHUB_SENTINEL is the value the assign/approve APIs accept to
       FORCE public github.com on a cluster whose defaults point at a GitHub
       Enterprise instance. It mirrors githubHostPublic on the server, where
       effectiveGitHubBaseURL resolves it to "" (public). It is deliberately
       NOT a hostname — the server exempts it from the hostname validator. */
    var PUBLIC_GITHUB_SENTINEL = 'public';

    /* clusterEntryById returns the ClusterListEntry for a cluster id, or null
       when the cluster list has not loaded (or the id is unknown). An empty id
       is how the hub records "the default cluster", so fall through to a
       lookup rather than guessing a host. */
    function clusterEntryById(clusterId) {
      if (!clusterId) return null;
      var list = _clusterList || [];
      for (var i = 0; i < list.length; i++) {
        if (list[i] && list[i].id === clusterId) return list[i];
      }
      return null;
    }

    /* clusterGitHubHost returns the bare GitHub host a cluster's hives default
       to ('github.com' for public GitHub), or '' when the cluster is unknown /
       the list has not loaded yet. Never guess github.com for an unknown
       cluster: silently claiming "public" is the exact bug this control
       exists to surface. */
    function clusterGitHubHost(clusterId) {
      var c = clusterEntryById(clusterId);
      return (c && c.github_host) || '';
    }

    /* githubHostChoiceMarkup renders the GitHub-instance picker shared by the
       assign and approve modals.

       Options are: the cluster's own host (the default, and the right answer
       in almost every case), explicit public github.com, and a free-text host
       for the rare cross-instance assignment. clusterHost may be '' when the
       cluster list has not loaded — the control then opens on "Use cluster
       default", which submits nothing and lets the SERVER's cluster backfill
       decide, exactly as if the control did not exist. */
    function githubHostChoiceMarkup(idPrefix, clusterHost, requestHost, fld, lbl) {
      var isGHE = clusterHost && clusterHost !== PUBLIC_GITHUB_HOST;
      var defaultLabel = clusterHost
        ? 'Cluster default — ' + esc(clusterHost)
        : 'Cluster default';
      /* A provision request's github_host, when present, is what the REQUESTER
         told us their org lives on. Prefer it over the cluster default: they
         know which GitHub their org is on and we should not silently override
         them. */
      var preselectCustom = !!(requestHost && requestHost !== clusterHost && requestHost !== PUBLIC_GITHUB_HOST);
      var opts =
        '<option value="">' + defaultLabel + '</option>' +
        '<option value="' + esc(PUBLIC_GITHUB_SENTINEL) + '">Public github.com' + (isGHE ? ' (override)' : '') + '</option>' +
        '<option value="__custom__"' + (preselectCustom ? ' selected' : '') + '>Other GitHub Enterprise host…</option>';
      return '<label style="' + lbl + '">GitHub instance</label>' +
        '<select id="' + esc(idPrefix) + '-ghhost" style="' + fld + '">' + opts + '</select>' +
        '<input id="' + esc(idPrefix) + '-ghhost-custom" style="' + fld + ';margin-top:6px;display:' + (preselectCustom ? 'block' : 'none') + '" ' +
          'placeholder="github.example.com" value="' + esc(preselectCustom ? requestHost : '') + '">' +
        '<div id="' + esc(idPrefix) + '-ghhost-note" style="font-size:0.72rem;color:var(--muted);margin-top:6px"></div>';
    }

    /* wireGitHubHostChoice attaches the behaviour for a picker rendered by
       githubHostChoiceMarkup: show the free-text box only for "Other", and
       keep a live note of exactly which GitHub instance the hive will target.
       Uses addEventListener rather than inline handlers because the values
       involved (hosts, org names) are user-controlled. */
    function wireGitHubHostChoice(idPrefix, clusterHost) {
      var sel = document.getElementById(idPrefix + '-ghhost');
      var custom = document.getElementById(idPrefix + '-ghhost-custom');
      var note = document.getElementById(idPrefix + '-ghhost-note');
      if (!sel) return;
      function render() {
        var isCustom = sel.value === '__custom__';
        if (custom) custom.style.display = isCustom ? 'block' : 'none';
        if (!note) return;
        var target;
        if (isCustom) {
          target = (custom && custom.value.trim()) || '';
        } else if (sel.value === PUBLIC_GITHUB_SENTINEL) {
          target = PUBLIC_GITHUB_HOST;
        } else {
          target = clusterHost || '';
        }
        if (!target) {
          note.textContent = 'This hive will use its cluster’s GitHub instance.';
          note.style.color = 'var(--muted)';
          return;
        }
        var ghe = target !== PUBLIC_GITHUB_HOST;
        note.textContent = 'This hive will target ' + target + '.' +
          (ghe ? ' The GitHub App must be installed on the org on ' + target + ' — a public github.com App will not authenticate there.' : '');
        note.style.color = ghe ? 'var(--accent, #58a6ff)' : 'var(--muted)';
      }
      /* Idempotent: this is re-invoked when the assign modal's placeholder
         picker changes the cluster, and binding a second listener each time
         would leave a stale closure (holding the OLD clusterHost) racing the
         new one. Store the current renderer and swap it out. */
      if (sel._ghHostRender) {
        sel.removeEventListener('change', sel._ghHostRender);
        if (custom) custom.removeEventListener('input', sel._ghHostRender);
      }
      sel._ghHostRender = render;
      sel.addEventListener('change', render);
      if (custom) custom.addEventListener('input', render);
      render();
    }

    /* githubHostChoiceValue returns what to send as github_host: '' for
       "cluster default" (the server backfills), the 'public' sentinel, or a
       typed GHE hostname. */
    function githubHostChoiceValue(idPrefix) {
      var sel = document.getElementById(idPrefix + '-ghhost');
      if (!sel) return '';
      if (sel.value !== '__custom__') return sel.value;
      var custom = document.getElementById(idPrefix + '-ghhost-custom');
      return (custom && custom.value.trim()) || '';
    }

    /* syncCreateModalInstallLinks points every "install the app" link in the
       create-hive modal at the SELECTED cluster's GitHub host. The hub serves
       one page to admins provisioning onto both public GitHub and GitHub
       Enterprise clusters, so a static github.com anchor is wrong for half of
       them: a GHE user following it lands on public github.com and their org
       admin never sees the pending install request.

       The URL comes from the server (config.GitHubConfig.AppInstallURL), which
       also honours a per-cluster app slug — a GHE instance hosts its own App
       registration and it is rarely named "kubestellar-hive". */
    function syncCreateModalInstallLinks() {
      var c = selectedClusterEntry();
      var url = (c && c.app_install_url) || '';
      var host = (c && c.github_host) || PUBLIC_GITHUB_HOST;
      var isGHE = host !== PUBLIC_GITHUB_HOST;
      var ids = ['auth-info-app-link', 'auth-info-later-link', 'auth-later-install-link'];
      for (var i = 0; i < ids.length; i++) {
        var el = document.getElementById(ids[i]);
        if (!el) continue;
        if (url) {
          el.href = url;
          el.removeAttribute('aria-disabled');
        } else {
          /* No URL yet (clusters still loading). Do not fall back to a
             github.com guess — a dead link is safer than one that sends a
             GHE admin to the wrong GitHub. */
          el.removeAttribute('href');
          el.setAttribute('aria-disabled', 'true');
        }
      }
      var appLink = document.getElementById('auth-info-later-link');
      if (appLink) appLink.textContent = '→ Install app on ' + host + ' now';
      var btn = document.getElementById('auth-later-install-link');
      if (btn) btn.textContent = 'Install app on your ' + (isGHE ? host + ' ' : '') + 'org';
      var note = document.getElementById('auth-later-ghe-note');
      if (note) {
        if (isGHE) {
          note.textContent = 'This cluster targets ' + host + '. The GitHub App must be registered and'
            + ' installed on that GitHub Enterprise instance — a public github.com install will not'
            + ' reach it, and the ' + host + ' org admin will see no pending request.';
          note.style.display = '';
        } else {
          note.textContent = '';
          note.style.display = 'none';
        }
      }
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

`

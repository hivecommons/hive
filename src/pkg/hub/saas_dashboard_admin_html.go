package hub

const dashboardHTMLAdminScripts = `    async function init() {
      /* Restore grouping, collapse state and saved views (including applying
         the default view) BEFORE the first loadHives(), so the initial render
         already reflects the operator's view rather than flashing the ungrouped
         list and then rearranging. */
      loadDashViewPrefs();
      /* Sort is resolved AFTER loadDashViewPrefs because applying a default
         saved view sets a key/direction of its own: the operator's explicitly
         stored sort is the more specific signal and must have the last word.
         Both run before the first paint so no render happens in the wrong
         order. */
      loadHiveSortPrefs();
      /* Progressive paint: put the last known fleet on screen immediately so a
         returning operator sees their hives instead of a spinner. The network
         path below still runs unconditionally and reconciles the list; this
         only changes what fills the gap while it is in flight. Must come after
         loadDashViewPrefs() so the cached rows honour the active view. */
      paintCachedHives();
      /* Record the offset from here on. Attached before the awaits below so a
         scroll during a slow load is still captured. */
      window.addEventListener('scroll', persistHiveScroll, {passive: true});
      await loadUser();
      await autoRequestAccessFromUrl();
      await loadHives();
      /* Restore AFTER the real rows are in the DOM — loadHives' renderHives is
         what gives the document its full height, and restoreHiveScroll still
         waits for that height before it commits (see its frame loop). Calling
         it any earlier clamps the offset to a short page and lands at the top. */
      restoreHiveScroll();
      await loadAdminUsers();
      if (!_adminLoaded) setTimeout(loadAdminUsers, 2000);
      loadClusterHealth();
      loadScaleSettings();
      loadReach();
      loadClusters();
      handleOpenRouterReturn();
    }
    init();
    var POLL_INTERVAL_MS = 30000;
    setInterval(loadHives, POLL_INTERVAL_MS);
    /* Live-tick any claim-pending counters once a second so an assigned-but-
       unclaimed row's "claim pending · Nm" advances between the 30s polls. */
    startAssignCounterTicker();
    setInterval(loadAdminUsers, POLL_INTERVAL_MS);
    setInterval(loadClusterHealth, CLUSTER_HEALTH_POLL_MS);
    setInterval(loadScaleSettings, CLUSTER_HEALTH_POLL_MS);
    setInterval(loadReach, REACH_POLL_MS);
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
    /* Lowercased usernames with a live hive session right now (admin payload).
       Drives the green-dashed avatar border via avatarImg. Empty for non-admins. */
    var _liveHiveUsers = new Set();
    /* The honest subset of _liveHiveUsers: focused tab + recent input, per the
       spokes' presence reports. live minus engaged = idle open tabs. */
    var _liveEngagedUsers = new Set();
    var _userSortKey = 'created_at', _userSortAsc = false;

    /* --- Collapsible "Hub Admin — Users" section ---
       The roster runs to ~90 records, so an expanded users table pushes the
       Hub Banner and Cluster Health sections far below the fold. An operator
       is normally here for hives, not for the roster, so this follows the same
       convention as the other long admin sections (Past Requests, Cluster
       Health): collapsed by default, remembered per browser under the same
       'hive-*-collapsed' localStorage key convention. Absent key means
       collapsed; only an explicit 'false' expands, so a first visit and a
       corrupted value behave identically. */
    var ADMIN_USERS_COLLAPSED_KEY = 'hive-admin-users-collapsed';
    var _adminUsersCollapsed = localStorage.getItem(ADMIN_USERS_COLLAPSED_KEY) !== 'false';

    /* applyAdminUsersCollapsed pushes _adminUsersCollapsed onto the DOM. Split
       out from the toggle so the initial render and expandAdminUsersSection()
       can restore state without duplicating the show/hide + aria bookkeeping. */
    function applyAdminUsersCollapsed() {
      var body = document.getElementById('admin-users-body');
      var toggle = document.getElementById('admin-users-toggle');
      var header = document.getElementById('admin-users-header');
      if (body) body.style.display = _adminUsersCollapsed ? 'none' : '';
      /* ▸ collapsed / ▾ expanded */
      if (toggle) toggle.innerHTML = _adminUsersCollapsed ? '&#9656;' : '&#9662;';
      if (header) header.setAttribute('aria-expanded', _adminUsersCollapsed ? 'false' : 'true');
    }

    /* persistAdminUsersCollapsed writes the current state. localStorage throws
       under private-browsing quota; collapsing must still work in-session, so
       the write is best-effort. */
    function persistAdminUsersCollapsed() {
      try {
        localStorage.setItem(ADMIN_USERS_COLLAPSED_KEY, _adminUsersCollapsed ? 'true' : 'false');
      } catch(e) {}
    }

    function toggleAdminUsers() {
      _adminUsersCollapsed = !_adminUsersCollapsed;
      persistAdminUsersCollapsed();
      applyAdminUsersCollapsed();
    }

    /* expandAdminUsersSection opens the section and PERSISTS that, mirroring
       expandAllHiveSections (#2348). Anything that reveals a row inside this
       section must go through here rather than flipping display directly: an
       in-memory-only expand silently re-collapses on the next reload, so the
       row the operator was just sent to would vanish again. */
    function expandAdminUsersSection() {
      if (!_adminUsersCollapsed) return;
      _adminUsersCollapsed = false;
      persistAdminUsersCollapsed();
      applyAdminUsersCollapsed();
    }

    /* The count beside the header is the only signal of roster size while the
       section is collapsed, so it is kept current on every render. */
    function updateAdminUsersCount(shown, total) {
      var el = document.getElementById('admin-users-count');
      if (!el) return;
      var n = Number(total) || 0;
      el.textContent = (Number(shown) === n)
        ? '(' + n + ')'
        : '(' + (Number(shown) || 0) + ' of ' + n + ')';
    }

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
      // Search matches the GitHub login and the contact fields, so an admin can
      // find someone by real name or by something they wrote in the notes —
      // which is the point of keeping notes here in the first place.
      var filtered = (_allUsers || []).filter(function(u) {
        if (!q) return true;
        if (!u) return false;
        var hay = [u.github_username, u.display_name, u.email, u.full_name, u.slack_id, u.company, u.notes]
          .filter(function(v) { return !!v; }).join(' ').toLowerCase();
        return hay.includes(q);
      });
      var sorted = filtered.slice().sort(function(a, b) {
        var va, vb;
        if (key === 'hiveCount') {
          var regIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
          va = Object.keys(a.hives || {}).filter(function(h) { return regIds.has(h); }).length;
          vb = Object.keys(b.hives || {}).filter(function(h) { return regIds.has(h); }).length;
        } else if (key === 'status') {
          va = userTierRank(a);
          vb = userTierRank(b);
        } else {
          va = a[key] || ''; vb = b[key] || '';
        }
        if (typeof va === 'number' && typeof vb === 'number') return _userSortAsc ? va - vb : vb - va;
        return _userSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
      updateAdminUsersCount(sorted.length, (_allUsers || []).length);
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
        var navFleet = document.getElementById('nav-fleet');
        if (navFleet) navFleet.style.display = '';
        /* The section is display:none until the admin check passes, so this is
           the first point at which the persisted collapse state can be pushed
           onto real DOM. Idempotent, so running it on every poll is fine. */
        applyAdminUsersCollapsed();
        document.getElementById('hub-banner-section').style.display = '';
        document.getElementById('btn-send-banner-top').style.display = '';
        loadActiveBanner();
        var data = await resp.json();
        _allUsers = data.users || [];
        try { applySortUsers(); } catch(re) { console.error('renderUsers error:', re); }
        /* Rollup rides the same admin poll as the table it sits under, so the
           two can never disagree about the roster. Not awaited: a slow or
           failed rollup must not delay or break the users table. */
        loadUserCountries();
      } catch(e) {
        if (!_adminLoaded) document.getElementById('admin-section').style.display = 'none';
      } finally {
        _adminLoading = false;
      }
    }

    function filterUsers() {
      applySortUsers();
    }

    /* ---- Fleet country rollup -------------------------------------------
       Where the hub's user base is, in aggregate. Reads
       /api/saas/admin/user-countries, which is requireAdmin and returns COUNTS
       only — no usernames — so this surface cannot be used to look up an
       individual.

       Deliberately NOT a map and NOT a chart: the hub ships no external CDN and
       no external images, so a map would mean vendoring geometry and a charting
       library would mean a script tag we cannot serve. A ranked bar list says
       the same thing in less space and is readable at a glance.

       The unknown bucket is rendered as its own row, always, with the same
       weight as a country. Country is optional and best-effort, so early on
       unknown IS the majority; hiding it would turn "3 of 200 users told us
       they're in DE" into something that looks like "the fleet is German". */

    /* Width of the widest bar, in percent of the list. The bars are scaled
       against the LARGEST bucket rather than the total so the smaller
       countries stay visible when one bucket dominates — which it will, since
       unknown starts at ~100%. */
    var COUNTRY_BAR_MAX_PCT = 100;

    /* countryRollupRow renders one bucket. label is pre-built HTML (a flag+code
       chip, or the plain "Unknown" text); everything else is numeric. */
    function countryRollupRow(label, count, max, total) {
      /* max is guaranteed >= 1 by the caller — see renderCountryRollup, which
         returns early on an empty population. Guarding again here anyway
         because a zero would silently produce NaN% and a bar that vanishes
         rather than an error anyone would notice. */
      var pct = (max > 0) ? Math.round((count / max) * COUNTRY_BAR_MAX_PCT) : 0;
      var share = (total > 0) ? Math.round((count / total) * 100) : 0;
      return '<div style="display:flex;align-items:center;gap:8px;margin-bottom:4px">' +
        '<div style="flex:none;width:72px;font-size:0.72rem;color:var(--text)">' + label + '</div>' +
        '<div style="flex:1;height:8px;background:var(--border);border-radius:4px;overflow:hidden">' +
          '<div style="width:' + pct + '%;height:100%;background:var(--accent)"></div>' +
        '</div>' +
        '<div style="flex:none;width:84px;text-align:right;font-size:0.7rem;color:var(--muted)">' +
          count + ' &middot; ' + share + '%</div>' +
      '</div>';
    }

    function renderCountryRollup(data) {
      var el = document.getElementById('user-countries-container');
      if (!el) return;
      var countries = (data && data.countries) || [];
      var unknown = Number(data && data.unknown) || 0;
      var total = Number(data && data.total) || 0;
      /* No users at all is the only case with nothing to say. An all-unknown
         population is NOT this case: it still renders, as a single 100% Unknown
         bar, which is exactly the state the operator needs to see on day one. */
      if (total <= 0) {
        el.innerHTML = '<div style="font-size:0.75rem;color:var(--muted)">No users yet.</div>';
        return;
      }
      /* Scale against the biggest bucket, unknown included, so no bar can ever
         exceed 100%. Floored at 1 so the divide is safe even if a future caller
         reaches here with all-zero counts. */
      var max = unknown;
      countries.forEach(function(c) { if (Number(c.count) > max) max = Number(c.count); });
      if (max < 1) max = 1;

      var rows = countries.map(function(c) {
        /* The server normalizes before sending; normalizing again is what
           guarantees a legacy or hand-edited record can never emit half a code
           point into the flag. A code that fails the check is dropped rather
           than rendered raw. */
        var code = normalizeCountryCode(c && c.code);
        if (!code) return '';
        var label = '<span style="display:inline-flex;align-items:center;gap:4px">' +
          countryFlagHTML(code) + esc(code) + '</span>';
        return countryRollupRow(label, Number(c.count) || 0, max, total);
      }).join('');

      /* Unknown is last (it is not a country and should not head a ranked list
         of them) but always present, and labelled in --muted so it reads as an
         absence rather than as a place. */
      var unknownLabel = '<span style="color:var(--muted)">Unknown</span>';
      rows += countryRollupRow(unknownLabel, unknown, max, total);

      el.innerHTML =
        '<div style="font-size:0.85rem;color:var(--accent);font-weight:600;margin-bottom:8px">' +
          'Where users are &mdash; ' + countries.length + ' countr' + (countries.length === 1 ? 'y' : 'ies') +
          ', ' + unknown + ' unknown of ' + total +
        '</div>' + rows;
    }

    async function loadUserCountries() {
      try {
        var resp = await fetch('/api/saas/admin/user-countries');
        /* 403 means not admin. Leave the container empty rather than writing an
           error: a non-admin should learn nothing about this surface, not even
           that it exists and refused them. */
        if (!resp.ok) return;
        renderCountryRollup(await resp.json());
      } catch(e) {}
    }

    // --- Admin Users: contact / CRM fields -------------------------------
    // Full name, Slack ID and notes are admin-maintained free text on the user
    // record. They are edited in a per-user panel row rather than inline in the
    // main row: notes is expected to be the longest field and a multi-line box
    // simply does not fit in a table cell next to quota and the action buttons.
    // The main row shows a one-line summary plus an Edit toggle, so the common
    // case (glance at who someone is) costs nothing and the edit case is one
    // click away.

    // Number of columns in the admin users table. Panel/expand rows span the
    // full width, so this must track the <th> count in renderUsers.
    // 9 since the Country column landed.
    var USERS_TABLE_COLSPAN = 9;
    // Rows of the notes textarea. Big enough for a short paragraph without
    // pushing the next user off screen. It is the only free-form prose field,
    // so it gets the height as well as the width; still user-resizable.
    var CONTACT_NOTES_ROWS = 4;
    // Characters of a note shown in the collapsed summary before ellipsis.
    var CONTACT_NOTES_PREVIEW_CHARS = 40;
    // Client-side maxlength on each field. Mirrors the server caps in saas.go
    // (maxContactNameLen / maxContactSlackIDLen / maxContactNotesLen) — the
    // server still truncates; this is only so the admin sees the limit.
    var CONTACT_MAX_NAME = 128;
    var CONTACT_MAX_SLACK = 64;
    var CONTACT_MAX_NOTES = 8192;

    // Field widths in the contact editor panel. The panel is its own full-width
    // row spanning every column (USERS_TABLE_COLSPAN), so widening these does
    // NOT add horizontal pressure to the already-dense main table — the fields
    // only compete with each other for the row's width.
    //
    // Expressed as flex "grow basis" pairs in percent so the row reflows with
    // the viewport instead of locking to pixel widths. The three bases sum to
    // less than 100% so the gap between fields has room; the grow factors then
    // divide the remainder, giving Notes the lion's share because it is
    // free-form prose while the other two are short identifiers.
    var CONTACT_W_NAME_BASIS = '22%';
    var CONTACT_W_NAME_MIN = '190px';
    var CONTACT_W_SLACK_BASIS = '18%';
    var CONTACT_W_SLACK_MIN = '150px';
    // Notes grows ~3x faster than the single-line fields and starts widest.
    var CONTACT_W_NOTES_BASIS = '48%';
    var CONTACT_W_NOTES_MIN = '320px';
    var CONTACT_NOTES_GROW = 3;
    /* Country is a two-letter code plus a live flag preview, so it is the
       narrowest field in the panel and the only one with a fixed-ish basis: it
       can never need more room than "GB  United Kingdom" takes to echo. */
    var CONTACT_W_COUNTRY_BASIS = '12%';
    var CONTACT_W_COUNTRY_MIN = '150px';
    /* ISO 3166-1 alpha-2. Mirrors countryCodeLen in user_country.go — the
       maxlength is a courtesy that stops a third letter being typed at all;
       normalizeCountryCode on both sides is the actual control. */
    var CONTACT_MAX_COUNTRY = 2;
    var CONTACT_MAX_COMPANY = 128;
    var CONTACT_W_COMPANY_BASIS = '18%';
    var CONTACT_W_COMPANY_MIN = '170px';

    /* ISO 3166-1 alpha-2 codes for the Country dropdown. We do NOT ship a
       250-row name table (the reason the field used to be free text): the codes
       are a compact closed list, and countryDisplayName() turns each into its
       localized English name via the browser's Intl.DisplayNames at render
       time — so the <select> options are labeled without a duplicated table and
       stay in step with the same name source the flag/preview already use. */
    var ISO_COUNTRY_CODES = ("AD AE AF AG AI AL AM AO AQ AR AS AT AU AW AX AZ BA BB BD BE BF BG BH BI BJ BL BM BN BO BQ BR BS BT BV BW BY BZ CA CC CD CF CG CH CI CK CL CM CN CO CR CU CV CW CX CY CZ DE DJ DK DM DO DZ EC EE EG EH ER ES ET FI FJ FK FM FO FR GA GB GD GE GF GG GH GI GL GM GN GP GQ GR GS GT GU GW GY HK HM HN HR HT HU ID IE IL IM IN IO IQ IR IS IT JE JM JO JP KE KG KH KI KM KN KP KR KW KY KZ LA LB LC LI LK LR LS LT LU LV LY MA MC MD ME MF MG MH MK ML MM MN MO MP MQ MR MS MT MU MV MW MX MY MZ NA NC NE NF NG NI NL NO NP NR NU NZ OM PA PE PF PG PH PK PL PM PN PR PS PT PW PY QA RE RO RS RU RW SA SB SC SD SE SG SH SI SJ SK SL SM SN SO SR SS ST SV SX SY SZ TC TD TF TG TH TJ TK TL TM TN TO TR TT TV TW TZ UA UG UM US UY UZ VA VC VE VG VI VN VU WF WS YE YT ZA ZM ZW").split(" ");

    /* Minimum number of users sharing a country before it floats to the
       "frequent" group at the top of the Country dropdown. "More than one":
       a single assignment is not yet a pattern worth reordering the list for. */
    var COUNTRY_FREQUENT_MIN_USERS = 2;
    /* The disabled divider between the frequent group and the full
       alphabetical list. value-less and disabled, so it can never be picked. */
    var COUNTRY_FREQUENT_SEPARATOR = '──────────';

    // frequentCountryCodes returns the codes held by at least
    // COUNTRY_FREQUENT_MIN_USERS of the given users, ordered by count
    // descending then localized name — the group countrySelectOptionsHTML
    // floats to the top of the dropdown. Empty or malformed countries never
    // count (normalizeCountryCode gates them out), so a roster full of
    // country-less users produces no group at all. Recomputed on every panel
    // render from the roster the admin poll keeps fresh (_allUsers), so the
    // ordering tracks assignments live as countries are added or cleared.
    function frequentCountryCodes(users) {
      var counts = {};
      (users || []).forEach(function (u) {
        var c = normalizeCountryCode(u && u.country);
        if (c) counts[c] = (counts[c] || 0) + 1;
      });
      return Object.keys(counts)
        .filter(function (c) { return counts[c] >= COUNTRY_FREQUENT_MIN_USERS; })
        .map(function (c) { return { code: c, count: counts[c], name: countryDisplayName(c) || c }; })
        .sort(function (a, b) {
          if (a.count !== b.count) return b.count - a.count;
          return a.name.localeCompare(b.name);
        })
        .map(function (o) { return o.code; });
    }

    // countrySelectOptionsHTML builds the <option>s for the Country dropdown,
    // sorted by localized display name, marking the current code selected and
    // a leading blank ("— none —") so an admin can clear a country.
    //
    // Countries already assigned to more than one loaded user float to a
    // "frequent" group right under "— none —" (frequentCountryCodes), followed
    // by a disabled separator and then the FULL alphabetical list — frequent
    // codes stay duplicated there on purpose, so an admin whose muscle memory
    // says "scroll to U" still finds United States where it always was. The
    // current code is marked selected exactly once (the frequent copy when it
    // has one), because duplicate selected attributes would make the browser
    // pick the later, alphabetical copy and scroll the closed select there.
    function countrySelectOptionsHTML(current) {
      var cur = (normalizeCountryCode(current) || '');
      var opts = ISO_COUNTRY_CODES.map(function (code) {
        return { code: code, name: countryDisplayName(code) || code };
      }).sort(function (a, b) { return a.name.localeCompare(b.name); });
      var html = '<option value=""' + (cur ? '' : ' selected') + '>— none —</option>';
      var frequent = frequentCountryCodes(_allUsers);
      var curInFrequent = frequent.indexOf(cur) !== -1;
      frequent.forEach(function (code) {
        html += '<option value="' + escAttr(code) + '"' + (code === cur ? ' selected' : '') +
          '>' + esc(countryDisplayName(code) || code) + ' (' + esc(code) + ')</option>';
      });
      if (frequent.length) {
        html += '<option value="" disabled>' + COUNTRY_FREQUENT_SEPARATOR + '</option>';
      }
      opts.forEach(function (o) {
        html += '<option value="' + escAttr(o.code) + '"' + (o.code === cur && !curInFrequent ? ' selected' : '') +
          '>' + esc(o.name) + ' (' + esc(o.code) + ')</option>';
      });
      return html;
    }

    // companyDatalistOptionsHTML builds the autocomplete suggestions for the
    // Company combobox from the DISTINCT company values already entered across
    // all loaded users — so the list "compiles as you enter them" without any
    // server-side vocabulary. _allUsers is the admin roster the Users table
    // already holds. The field stays a free-text <input list=…>, so a brand-new
    // company can always be typed and becomes a suggestion on the next render.
    function companyDatalistOptionsHTML() {
      var seen = {};
      (_allUsers || []).forEach(function (u) {
        var c = (u && u.company ? String(u.company).trim() : '');
        if (c) seen[c] = true;
      });
      return Object.keys(seen).sort(function (a, b) { return a.localeCompare(b); })
        .map(function (c) { return '<option value="' + escAttr(c) + '"></option>'; }).join('');
    }

    // Which users currently have their contact panel open, keyed by username.
    // Re-rendering on the admin poll must not slam a panel shut mid-edit.
    var _contactExpandedUsers = {};

    // --- Protecting in-progress contact edits from the admin poll ---------
    // renderUsers() rebuilds the whole users table via innerHTML, which
    // destroys and recreates every contact input. The contact fields save on
    // blur, so a value that has been typed but not yet blurred lives ONLY in
    // the DOM node the re-render is about to throw away — losing it is real
    // data loss, not a cosmetic reflow.
    //
    // Approach: defer the render while an edit is in progress, rather than
    // trying to restore focus/caret/selection afterwards. Caret restoration
    // across an innerHTML swap is fiddly and fails in exactly the cases that
    // matter (IME composition, native undo history, a textarea scrolled
    // mid-note), and it cannot restore the browser's undo stack at all. The
    // users table is admin-only, low-churn data; postponing a refresh for a
    // few seconds while someone types costs nothing.
    //
    // "In progress" deliberately means anywhere in the table, not just the
    // focused node: an admin who typed a name, tabbed to Slack ID and is
    // still typing has an unsaved name too, and yanking the row out from
    // under them loses it just the same.

    // How long after the last contact keystroke/blur the table is still
    // considered "being edited". Long enough to cover thinking-pauses while
    // composing a note, short enough that the table is not stale for long
    // after the admin walks away mid-edit.
    var CONTACT_EDIT_QUIET_MS = 5000;
    // How often a deferred render re-checks whether editing has finished.
    // Also the ceiling on how late a deferred render can be beyond the quiet
    // window above.
    var CONTACT_DEFER_RECHECK_MS = 1000;

    // Timestamp of the most recent contact-field interaction, and the handle
    // of the pending re-check timer (null when no render is deferred).
    var _contactLastEditAt = 0;
    var _contactDeferTimer = null;
    // Set while a deferred render is waiting. The poll keeps updating _allUsers
    // regardless; this only gates the DOM write, so the deferred render always
    // paints the newest data rather than a stale snapshot.
    var _contactRenderPending = false;

    // Per-field dirty values: what the admin has typed but not yet saved,
    // keyed by "username::field". Survives a render so that if one does slip
    // through, the typed text is re-applied rather than lost. Multiple users'
    // fields can be dirty at once (type a note, click into another user's
    // Slack ID without blurring back), so this is a map, not a single slot.
    var _contactDirty = {};

    function contactDirtyKey(username, field) { return username + '::' + field; }

    // markContactEditing records activity so the poll backs off.
    function markContactEditing() { _contactLastEditAt = Date.now(); }

    // contactEditInProgress is true when the table must not be rebuilt:
    // either a contact control currently has focus, or something is typed and
    // unsaved, or a keystroke landed within the quiet window.
    function contactEditInProgress() {
      var container = document.getElementById('users-container');
      if (container) {
        var active = document.activeElement;
        if (active && active.getAttribute && active.getAttribute('data-contact-field') &&
            container.contains(active)) {
          return true;
        }
      }
      for (var k in _contactDirty) {
        if (Object.prototype.hasOwnProperty.call(_contactDirty, k)) return true;
      }
      return (Date.now() - _contactLastEditAt) < CONTACT_EDIT_QUIET_MS;
    }

    // contactPanelId maps a username to a DOM id. Usernames are GitHub logins
    // (alphanumerics and hyphens) but this is defensive: anything outside that
    // set is replaced so a stored username can never inject markup through an
    // id attribute or break querySelector.
    function contactPanelId(username) {
      return 'contact-panel-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    /* Same sanitizing rule as contactPanelId, for the country field's live
       preview node. Separate id per user because every panel row is emitted for
       every user up front (hidden until expanded), so a shared id would collide
       across hundreds of rows and every lookup would find the first one. */
    function contactCountryPreviewId(username) {
      return 'contact-country-preview-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    /* countryPreviewHTML: what a typed code resolves to, as flag + English name.

       Reuses countryFlagEmoji / countryDisplayName from the shared block above
       rather than restating them, so the admin editor and the self-service
       editor can never disagree about what a code means.

       Three distinct states, deliberately worded so none of them reads as an
       error the admin caused:
         blank   -> "No flag" (a legitimate value: clearing the country)
         partial -> "Two-letter code" (still typing; not a failure)
         valid   -> the flag and the country's name

       The code is escaped even though normalizeCountryCode has already reduced
       it to [A-Z]{2}, and the name is escaped because Intl.DisplayNames returns
       a localized string this render path does not own. */
    function countryPreviewHTML(raw) {
      var typed = String(raw == null ? '' : raw).trim();
      if (!typed) return 'No flag';
      var c = normalizeCountryCode(typed);
      if (!c) return 'Two-letter code';
      return countryFlagEmoji(c) + ' ' + esc(countryDisplayName(c));
    }

    /* refreshContactCountryPreview re-renders one user's preview from whatever
       is currently in their input. Called on every keystroke, so it reads the
       DOM rather than _allUsers — the point is to reflect the UNSAVED text. */
    function refreshContactCountryPreview(username, value) {
      var el = document.getElementById(contactCountryPreviewId(username));
      if (!el) return;
      el.innerHTML = countryPreviewHTML(value);
    }

    /* providerBadge renders the user's login method (auth provider) as a small
       chip next to their name in the admin Users table, so an admin can see at a
       glance who signed in with GitHub vs Google vs IBMid vs Red Hat vs
       Microsoft. The provider rides the users payload as u.provider (derived
       server-side via userProvider). A legacy record with no provider resolves
       to github, so every existing row shows the GitHub chip — no blank. */
    // providerLogoSVG returns a small inline brand SVG per login provider, matching
    // the marks used on the /login picker, so the badge is recognizable at a glance.
    function providerLogoSVG(p) {
      var s = 'width="12" height="12" style="flex:none" aria-hidden="true"';
      switch (p) {
        case 'github':
          return '<svg viewBox="0 0 16 16" fill="currentColor" ' + s + '><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg>';
        case 'google':
          return '<svg viewBox="0 0 18 18" ' + s + '><path fill="#4285F4" d="M17.64 9.2c0-.64-.06-1.25-.16-1.84H9v3.48h4.84a4.14 4.14 0 0 1-1.8 2.72v2.26h2.92c1.7-1.57 2.68-3.88 2.68-6.62z"/><path fill="#34A853" d="M9 18c2.43 0 4.47-.8 5.96-2.18l-2.92-2.26c-.8.54-1.84.86-3.04.86-2.34 0-4.32-1.58-5.03-3.7H.96v2.33A9 9 0 0 0 9 18z"/><path fill="#FBBC05" d="M3.97 10.72a5.4 5.4 0 0 1 0-3.44V4.95H.96a9 9 0 0 0 0 8.1l3.01-2.33z"/><path fill="#EA4335" d="M9 3.58c1.32 0 2.5.45 3.44 1.35l2.58-2.58C13.46.89 11.42 0 9 0A9 9 0 0 0 .96 4.95l3.01 2.33C4.68 5.16 6.66 3.58 9 3.58z"/></svg>';
        case 'ibmid':
          // Text-based IBM mark (license-safe; the striped-wordmark SVG rendered
          // as a broken glyph at badge size). IBM blue, bold, sized to match the
          // other 12px brand marks.
          return '<svg viewBox="0 0 26 14" width="20" height="12" style="flex:none" aria-hidden="true">' +
            '<text x="13" y="11" text-anchor="middle" font-family="system-ui,-apple-system,sans-serif" ' +
            'font-size="11" font-weight="700" fill="#0f62fe">IBM</text></svg>';
        case 'redhat':
          return '<svg viewBox="0 0 24 24" fill="#EE0000" ' + s + '><path d="M16.35 14.4c1.6 0 3.9-.33 3.9-2.24a1.8 1.8 0 0 0-.04-.44l-.95-4.12c-.22-.9-.41-1.31-2-2.12-1.24-.63-3.94-1.67-4.74-1.67-.74 0-.96.96-1.85.94-.86-.02-1.5-.74-2.3-.74-.77 0-1.27.52-1.66 1.6 0 0-1.08 3.05-1.22 3.49a.83.83 0 0 0-.03.25c0 1.2 4.71 5.32 10.88 5.32M20.47 12.94c.22 1.05.22 1.16.22 1.3 0 1.8-2.02 2.79-4.67 2.79-6 0-11.25-3.51-11.25-5.83 0-.32.07-.63.18-.93C2.94 10.36 1 10.63 1 12.34c0 2.8 6.63 6.26 11.87 6.26 4.02 0 5.03-1.82 5.03-3.25 0-1.13-.97-2.4-2.43-2.41"/></svg>';
        case 'microsoft':
          return '<svg viewBox="0 0 23 23" ' + s + '><path fill="#F25022" d="M0 0h11v11H0z"/><path fill="#7FBA00" d="M12 0h11v11H12z"/><path fill="#00A4EF" d="M0 12h11v11H0z"/><path fill="#FFB900" d="M12 12h11v11H12z"/></svg>';
        default:
          return '';
      }
    }

    function providerBadge(u) {
      var p = String((u && u.provider) || 'github').toLowerCase();
      var meta = {
        github: {label: 'GitHub', color: '#c9d1d9', bg: 'rgba(110,118,129,0.25)'},
        google: {label: 'Google', color: '#e8eaed', bg: 'rgba(66,133,244,0.22)'},
        ibmid:  {label: 'IBMid',  color: '#e8eaed', bg: 'rgba(15,98,254,0.22)'},
        redhat: {label: 'Red Hat',color: '#f5c2c7', bg: 'rgba(238,0,0,0.22)'},
        microsoft: {label: 'Microsoft', color: '#e8eaed', bg: 'rgba(0,120,215,0.22)'}
      }[p] || {label: p || 'unknown', color: 'var(--muted)', bg: 'rgba(110,118,129,0.25)'};
      var logo = providerLogoSVG(p);
      return '<span title="Signed in with ' + escAttr(meta.label) + '"' +
        ' style="display:inline-flex;align-items:center;gap:4px;padding:1px 7px;border-radius:9999px;' +
        'font-size:0.6rem;font-weight:600;color:' + meta.color + ';background:' + meta.bg + '">' +
        (logo ? logo : '') + esc(meta.label) + '</span>';
    }

    /* countryCell renders the user's country in the admin Users table as
       flag + alpha-2 code, following the providerBadge chip above rather than
       inventing a second visual language for a per-row attribute: same
       inline-flex chip, same 4px gap, same small type.

       Unknown or unset renders NOTHING — an empty cell, no globe, no dash, no
       "unknown" chip. That is #4371's rule and it matters most here, where a
       column of placeholders would read as data we have. The rollup below the
       table is where the size of the unknown bucket is stated explicitly; the
       row is not the place to repeat it 200 times.

       countryFlagHTML already normalizes and escapes; the code is escaped again
       for the visible text because the render path must not depend on the
       validator upstream of it staying correct. Color is the --muted theme
       token so the code recedes next to the name in both light and dark. */
    function countryCell(u) {
      var code = normalizeCountryCode(u && u.country);
      if (!code) return '';
      return '<span style="display:inline-flex;align-items:center;gap:4px;font-size:0.7rem;color:var(--muted)">' +
        countryFlagHTML(code) + esc(code) + '</span>';
    }

    // renderContactCell is the collapsed summary shown in the main user row.
    function renderContactCell(u) {
      var bits = [];
      if (u.full_name) bits.push('<span style="font-size:0.78rem">' + esc(u.full_name) + '</span>');
      if (u.company) bits.push('<span style="font-size:0.7rem;color:var(--muted)">' + esc(u.company) + '</span>');
      if (u.slack_id) bits.push('<span style="font-size:0.7rem;color:var(--muted)">slack: ' + esc(u.slack_id) + '</span>');
      if (u.notes) {
        var preview = u.notes.length > CONTACT_NOTES_PREVIEW_CHARS
          ? u.notes.substring(0, CONTACT_NOTES_PREVIEW_CHARS) + '…' : u.notes;
        // title= carries the full note on hover, so it needs attribute escaping.
        bits.push('<span title="' + escAttr(u.notes) + '" style="font-size:0.7rem;color:var(--muted);font-style:italic">' + esc(preview) + '</span>');
      }
      // align-items:flex-start + text-align:left keep the contact lines
      // left-justified. Without them the cell inherits the users table's
      // centred alignment, so name/slack/notes rendered ragged-centre — each
      // line a different width, which is hard to scan down a column.
      var summary = bits.length
        ? '<div style="display:flex;flex-direction:column;align-items:flex-start;text-align:left;gap:1px;max-width:220px;overflow:hidden">' + bits.join('') + '</div>'
        : '<span style="color:var(--muted);font-size:0.72rem">—</span>';
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[u.github_username]);
      var btn = '<button type="button" data-contact-toggle="' + escAttr(u.github_username) + '"' +
        ' id="' + contactToggleId(u.github_username) + '"' +
        ' aria-controls="' + contactPanelId(u.github_username) + '"' +
        ' aria-expanded="' + (open ? 'true' : 'false') + '"' +
        ' aria-label="' + escAttr(contactToggleAriaLabel(u.github_username, open)) + '"' +
        ' style="' + contactToggleStyle(open) + '">' + contactToggleLabel(open) + '</button>';
      return summary + btn;
    }

    /* --- The Edit/Save toggle ---
       The button in the CONTACT column is the operator's primary affordance:
       "Edit" while the panel is shut, "Save" while it is open. Clicking Save
       commits every pending field and closes the panel.

       Save is styled as the primary action (accent border and text) so the eye
       lands on it; the × and the Close button inside the panel remain the
       escape hatches, and Escape still works. Making the row button the commit
       is what gives an explicit save point WITHOUT fighting the per-field blur
       saves: blur keeps storing silently as the admin tabs around, and this
       button is the only thing that closes on success. */

    /* Style is derived from state in one place so the initial markup and the
       live in-place update below can never drift apart. */
    function contactToggleStyle(open) {
      var color = open ? 'var(--accent)' : 'var(--muted)';
      return 'margin-top:2px;padding:1px 7px;background:none;border:1px solid ' + color +
        ';border-radius:4px;color:' + color + ';cursor:pointer;font-size:0.65rem' +
        (open ? ';font-weight:600' : '');
    }

    function contactToggleLabel(open) { return open ? 'Save' : 'Edit'; }

    /* The accessible name carries the username as well as the verb, because a
       screen-reader user tabbing a table of ~90 identical "Save" buttons needs
       to know which row they are on. */
    function contactToggleAriaLabel(username, open) {
      return (open ? 'Save contact details for ' : 'Edit contact details for ') + String(username || '');
    }

    function contactToggleId(username) {
      return 'contact-toggle-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    /* refreshContactToggleLabel re-derives the button from _contactExpandedUsers
       and writes label, style, aria-expanded and the accessible name TOGETHER.
       Updating the visible text without the aria state would leave a screen
       reader announcing "Edit, collapsed" over an open editor. */
    function refreshContactToggleLabel(username) {
      if (!username) return;
      var btn = document.getElementById(contactToggleId(username));
      if (!btn) return;
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[username]);
      btn.textContent = contactToggleLabel(open);
      btn.setAttribute('aria-expanded', open ? 'true' : 'false');
      btn.setAttribute('aria-label', contactToggleAriaLabel(username, open));
      btn.setAttribute('style', contactToggleStyle(open));
    }

    // renderContactPanelRow is the expandable editor row that follows each user
    // row. It is always emitted (hidden unless expanded) so toggling is a
    // display flip rather than a re-render, which keeps focus and caret intact.
    function renderContactPanelRow(u) {
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[u.github_username]);
      var fld = 'padding:5px 7px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.78rem;box-sizing:border-box;width:100%';
      var lbl = 'display:block;font-size:0.66rem;color:var(--muted);margin-bottom:3px;text-transform:uppercase;letter-spacing:0.04em';
      var user = escAttr(u.github_username);
      return '<tr id="' + contactPanelId(u.github_username) + '" style="display:' + (open ? '' : 'none') + '">' +
        /* position:relative anchors the absolutely-positioned × below. */
        '<td class="contact-panel-cell" colspan="' + USERS_TABLE_COLSPAN + '" style="background:var(--surface);position:relative">' +
        '<div style="padding:10px 14px 12px 40px;display:flex;gap:14px;flex-wrap:wrap;align-items:flex-start">' +
          '<div style="flex:1 1 ' + CONTACT_W_NAME_BASIS + ';min-width:' + CONTACT_W_NAME_MIN + '">' +
            '<label style="' + lbl + '">Full name</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="full_name"' +
              ' maxlength="' + CONTACT_MAX_NAME + '" value="' + escAttr(u.full_name || '') + '" style="' + fld + '">' +
          '</div>' +
          '<div style="flex:1 1 ' + CONTACT_W_SLACK_BASIS + ';min-width:' + CONTACT_W_SLACK_MIN + '">' +
            '<label style="' + lbl + '">Slack ID</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="slack_id"' +
              ' maxlength="' + CONTACT_MAX_SLACK + '" value="' + escAttr(u.slack_id || '') + '" style="' + fld + '">' +
          '</div>' +
          /* Company. Admin-entered CRM free text, NOT collected in the hive
             request form (operator fills it manually). A combobox: a plain
             <input> backed by a <datalist> of the company names already entered
             across users (companyDatalistOptionsHTML), so it autocompletes from
             the growing set as they are entered while still accepting a new one. */
          '<div style="flex:1 1 ' + CONTACT_W_COMPANY_BASIS + ';min-width:' + CONTACT_W_COMPANY_MIN + '">' +
            '<label style="' + lbl + '">Company</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="company"' +
              ' list="contact-company-suggestions"' +
              ' maxlength="' + CONTACT_MAX_COMPANY + '" value="' + escAttr(u.company || '') + '" style="' + fld + '">' +
          '</div>' +
          /* Country. An ADMIN ASSIGNMENT on the user's behalf (recorded as
             CountrySource "admin"). Now a <select> of ISO 3166-1 codes labeled
             with their localized names via Intl.DisplayNames — no duplicated
             250-row table (countrySelectOptionsHTML), a blank "— none —" clears
             it. The live preview still echoes the flag + name for the current
             selection so the row's Country column agrees on open. */
          '<div style="flex:1 1 ' + CONTACT_W_COUNTRY_BASIS + ';min-width:' + CONTACT_W_COUNTRY_MIN + '">' +
            '<label style="' + lbl + '">Country</label>' +
            '<select data-contact-user="' + user + '" data-contact-field="country"' +
              ' aria-describedby="' + contactCountryPreviewId(u.github_username) + '"' +
              ' style="' + fld + '">' + countrySelectOptionsHTML(u.country) + '</select>' +
            '<div id="' + contactCountryPreviewId(u.github_username) + '"' +
              ' style="margin-top:4px;font-size:0.66rem;color:var(--muted);min-height:1.1em">' +
              countryPreviewHTML(u.country) + '</div>' +
          '</div>' +
          '<div style="flex:' + CONTACT_NOTES_GROW + ' 1 ' + CONTACT_W_NOTES_BASIS + ';min-width:' + CONTACT_W_NOTES_MIN + '">' +
            '<label style="' + lbl + '">Notes</label>' +
            '<textarea data-contact-user="' + user + '" data-contact-field="notes" rows="' + CONTACT_NOTES_ROWS + '"' +
              ' maxlength="' + CONTACT_MAX_NOTES + '" style="' + fld + ';resize:vertical;font-family:inherit">' + esc(u.notes || '') + '</textarea>' +
          '</div>' +
          '<div style="align-self:flex-end;display:flex;align-items:center;gap:10px;padding-bottom:4px">' +
            '<span style="font-size:0.65rem;color:var(--muted)">Save closes &middot; Esc cancels</span>' +
            '<button type="button" data-contact-close="' + user + '"' +
              ' style="padding:4px 12px;background:var(--bg);border:1px solid var(--border);border-radius:4px;' +
              'color:var(--muted);cursor:pointer;font-size:0.7rem">Cancel</button>' +
          '</div>' +
        '</div>' +
        /* The × sits top-right of the panel, the conventional place to look for
           a dismiss control, and duplicates the Close button so the affordance
           is visible without reading to the end of a wide row. */
        '<button type="button" data-contact-close="' + user + '" aria-label="Close editor for ' + user + '"' +
          ' style="position:absolute;top:6px;right:10px;background:none;border:none;color:var(--muted);' +
          'cursor:pointer;font-size:1rem;line-height:1;padding:2px 6px">&#10005;</button>' +
        '</td></tr>';
    }

    /* contactPanelHasUnsavedEdits reports whether anything in this user's panel
       has been typed but not yet handed to the save path. The fields save on
       blur, so text that has never been blurred lives ONLY in the DOM node —
       closing the panel without a warning would be real data loss. */
    function contactPanelHasUnsavedEdits(username) {
      if (!username) return false;
      for (var k in _contactDirty) {
        if (Object.prototype.hasOwnProperty.call(_contactDirty, k) &&
            k.indexOf(username + '::') === 0) {
          return true;
        }
      }
      return false;
    }

    /* closeContactPanel is the single exit for the editor: the ×, the Close
       button, Escape, and a successful save all funnel through here.

       Unsaved text is committed rather than discarded when the admin confirms:
       blur normally saves, but Escape and the × can fire while a field still
       has focus and has never blurred, so the value would otherwise be lost. */
    async function closeContactPanel(username, opts) {
      if (!username) return;
      var force = !!(opts && opts.force);
      if (!force && contactPanelHasUnsavedEdits(username)) {
        var keep = await hiveConfirm('You have unsaved changes for ' + username +
          '. Save them and close?');
        if (!keep) return;
        /* Save first, and close only if the save actually landed. A failed
           save keeps the editor open with the typed text still in the fields
           so the admin can read the toast, fix the cause and retry — closing
           here would discard the very edit the server just rejected. */
        var saved = await commitContactPanelEdits(username);
        if (!saved) return;
      }
      setContactPanelOpen(username, false);
    }

    /* commitContactPanelEdits flushes every dirty field for one user through
       the normal save path and resolves true only when every one succeeded.
       saveContactField is a no-op for unchanged values, so this is safe to
       call broadly. Dirty marks are cleared only for fields that saved, so a
       rejected field stays dirty and the poll keeps backing off rather than
       rebuilding the table over text the admin has not managed to store. */
    async function commitContactPanelEdits(username) {
      var prefix = username + '::';
      var keys = Object.keys(_contactDirty || {}).filter(function(k) {
        return k.indexOf(prefix) === 0;
      });
      /* Also flush whatever is currently in the panel's inputs. The commit can
         be triggered by a click on Save while a field still holds focus and has
         never blurred, so its text may not be in _contactDirty yet. */
      var panel = document.getElementById(contactPanelId(username));
      if (panel) {
        var live = panel.querySelectorAll('[data-contact-field]');
        Array.prototype.forEach.call(live || [], function(el) {
          var f = el.getAttribute('data-contact-field');
          if (!f) return;
          var k = prefix + f;
          _contactDirty[k] = el.value;
          if (keys.indexOf(k) < 0) keys.push(k);
        });
      }
      /* Saves are sequential, not Promise.all: the handler is a read-modify-write
         over one JSON file per user, so three concurrent PUTs for the same user
         can interleave and lose a field. */
      var allOK = true;
      for (var i = 0; i < (keys || []).length; i++) {
        var k = keys[i];
        var field = k.substring(prefix.length);
        /* Not silent: this IS the explicit commit, so a failure here is exactly
           what the admin needs to see. */
        var ok = await saveContactField(username, field, _contactDirty[k]);
        if (ok) { delete _contactDirty[k]; } else { allOK = false; }
      }
      /* A field can be clean here yet still have failed earlier during a silent
         blur-save. Surface that recorded reason rather than closing on what
         would look like a no-op success. */
      if (allOK && _contactLastError[username]) {
        hiveToast('Could not save ' + username + ': ' + _contactLastError[username], 'error');
        return false;
      }
      return allOK;
    }

    /* openContactPanelUsername returns the username of the contact editor that
       currently owns focus, or '' when focus is elsewhere. Escape must only
       close the editor the admin is actually working in — a stray Escape with
       focus on the page body should fall through to the global handler that
       closes create-modal and friends (#2321). */
    function openContactPanelUsername() {
      var active = document.activeElement;
      if (!active || !active.closest) return '';
      var cell = active.closest('.contact-panel-cell');
      if (!cell) return '';
      var field = cell.querySelector('[data-contact-user]');
      return field ? (field.getAttribute('data-contact-user') || '') : '';
    }

    /* Escape-to-close for the contact editor. Registered in the CAPTURE phase
       so it can decide before the global Escape handler (#2321) runs, and it
       stops propagation ONLY when it actually closed an editor — otherwise the
       global handler keeps its existing behaviour for create-modal, the
       request modal, the confirm overlay and the timeline/access modals. */
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      /* hiveConfirm puts a modal overlay on top and binds its own Escape; while
         one is up it owns the key, so never steal it. */
      if (document.querySelector('.hive-confirm-overlay')) return;
      var username = openContactPanelUsername();
      if (!username) return;
      e.stopPropagation();
      e.preventDefault();
      closeContactPanel(username);
    }, true);

    // bindContactPanels attaches listeners after renderUsers writes the table.
    // Listeners rather than inline on* attributes: esc() leaves apostrophes
    // alone, so a value like O'Brien inside onchange="save('...')" would break
    // the handler (and be an injection vector). Nothing user-controlled is ever
    // interpolated into executable markup here.
    function bindContactPanels() {
      var container = document.getElementById('users-container');
      if (!container) return;
      var toggles = container.querySelectorAll('[data-contact-toggle]');
      Array.prototype.forEach.call(toggles || [], function(btn) {
        btn.addEventListener('click', function() {
          toggleContactPanel(btn.getAttribute('data-contact-toggle'));
        });
      });
      /* The × and the Close button share data-contact-close, so one binding
         covers both. Bound here rather than as inline on* attributes for the
         same reason as the toggles: nothing user-controlled is interpolated
         into executable markup. */
      var closers = container.querySelectorAll('[data-contact-close]');
      Array.prototype.forEach.call(closers || [], function(btn) {
        btn.addEventListener('click', function() {
          closeContactPanel(btn.getAttribute('data-contact-close'));
        });
      });
      var fields = container.querySelectorAll('[data-contact-field]');
      Array.prototype.forEach.call(fields || [], function(el) {
        var user = el.getAttribute('data-contact-user');
        var field = el.getAttribute('data-contact-field');
        var key = contactDirtyKey(user, field);
        // Re-apply anything typed but not yet saved. Belt-and-braces: the
        // render gate should mean we never rebuild mid-edit, but if a render
        // does land (e.g. one already in flight when typing began), the typed
        // text is restored instead of silently reverting to the server value.
        if (Object.prototype.hasOwnProperty.call(_contactDirty, key)) {
          el.value = _contactDirty[key];
        }
        // Any keystroke marks the table as being edited and records the
        // in-progress value, so the poll backs off and nothing typed is only
        // ever held in a DOM node that is about to be replaced.
        var onEdit = function() {
          markContactEditing();
          _contactDirty[key] = el.value;
          /* Country is the one field whose stored form (two letters) does not
             say what it means, so it echoes as you type/select. The other
             fields are their own preview. */
          if (field === 'country') refreshContactCountryPreview(user, el.value);
        };
        el.addEventListener('input', onEdit);
        // A <select> (the Country dropdown) fires 'change', not always 'input'.
        // Also save on change so a dropdown pick commits without needing a blur.
        el.addEventListener('change', function() {
          onEdit();
          if (el.tagName === 'SELECT') {
            var pending = el.value;
            saveContactField(user, field, pending, {silent: true}).then(function(ok) {
              if (ok && _contactDirty[key] === pending) delete _contactDirty[key];
              refreshContactToggleLabel(user);
            });
          }
        });
        // focus/blur also count as activity: tabbing between fields must not
        // leave a gap the poll can render into.
        el.addEventListener('focus', markContactEditing);
        // Save on blur so an admin can tab between fields and type freely
        // without a request per keystroke.
        //
        // Blur-saves are SILENT: no toast on success, and on failure the value
        // stays dirty so the field keeps the typed text and the Save button
        // keeps reading "Save". Blur fires constantly (tabbing between the
        // three fields, clicking the Save button itself), so a toast per blur
        // would be noise, and a blur-triggered error would fire while the
        // admin is mid-sentence in the next field. The Save button is the
        // explicit commit point where failures are reported — see
        // commitContactPanelEdits.
        el.addEventListener('blur', function() {
          markContactEditing();
          // Clear dirty only once the save has actually landed, so there is no
          // window where the value is neither dirty nor stored. A rejected
          // blur-save therefore leaves the panel dirty and Save still armed.
          var pending = el.value;
          saveContactField(user, field, pending, {silent: true}).then(function(ok) {
            if (ok && _contactDirty[key] === pending) delete _contactDirty[key];
            refreshContactToggleLabel(user);
          });
        });
      });
    }

    /* The row button is stateful: "Edit" when shut, "Save" when open.

       Shut  -> open the panel (a plain display flip, which is what keeps focus
                and caret intact) and relabel to Save.
       Open  -> this IS the Save action: commit every pending field, and close
                only if the commit succeeded. A failure keeps the panel open,
                keeps the label on Save, and lets the error toast stand — with
                a visible Save button, a false success is the worst outcome, so
                nothing here closes on an unverified save.

       Note this deliberately does NOT go through closeContactPanel: that path
       is for the escape hatches (×, Close, Escape), where the right question is
       "you have unsaved changes, save them?". Pressing Save has already
       answered that question. */
    async function toggleContactPanel(username) {
      if (!username) return;
      if (_contactExpandedUsers[username]) {
        var saved = await commitContactPanelEdits(username);
        if (!saved) return;
        setContactPanelOpen(username, false);
        return;
      }
      setContactPanelOpen(username, true);
    }

    /* setContactPanelOpen is the ONLY place panel visibility changes, so the
       row/panel display and the button's label + aria state can never fall out
       of step with _contactExpandedUsers. */
    function setContactPanelOpen(username, open) {
      _contactExpandedUsers[username] = !!open;
      var row = document.getElementById(contactPanelId(username));
      if (row) row.style.display = open ? '' : 'none';
      refreshContactToggleLabel(username);
    }

    // Last value saved per user+field, so a blur that changed nothing (tabbing
    // through, or a re-render restoring focus) does not fire a pointless PUT.
    var _contactLastSaved = {};

    // Always returns a Promise<boolean> so callers can sequence on the outcome;
    // a no-op resolves true because "nothing to do" is not a failure.
    // opts.silent suppresses the failure toast, for blur-saves where the Save
    // button is the reporting point. The boolean result is unaffected.
    function saveContactField(username, field, value, opts) {
      if (!username || !field) return Promise.resolve(true);
      var key = username + '::' + field;
      var current = _allUsers ? (_allUsers.find(function(x) { return x.github_username === username; }) || {}) : {};
      var previous = (_contactLastSaved[key] !== undefined) ? _contactLastSaved[key] : (current[field] || '');
      var next = (value || '').trim();
      /* Country is stored upper-cased by the server, so normalize it to the
         SAME form here before the equality check. Without this, re-opening the
         panel and typing "gb" over a stored "GB" would read as a change and fire
         a pointless PUT, and the optimistic cache below would hold a value the
         server never wrote. A half-typed or invalid code is left alone rather
         than blanked, so the field still holds what was typed while the server
         does the actual rejecting. */
      if (field === 'country') {
        var norm = normalizeCountryCode(next);
        if (norm) next = norm;
      }
      if (next === previous) return Promise.resolve(true);
      _contactLastSaved[key] = next;
      // Keep the in-memory copy in step so the next poll's signature check does
      // not treat our own edit as an external change and re-render mid-typing.
      if (current) current[field] = next;
      var payload = {};
      payload[field] = next;
      // On failure, roll the optimistic bookkeeping back to the previous value.
      // Without this the cache claims the value was stored, so the
      // next-equals-previous short-circuit above would skip the retry of the very
      // same value — the edit could never be saved again without a reload.
      return updateUser(username, payload, opts).then(function(ok) {
        if (!ok) {
          _contactLastSaved[key] = previous;
          if (current) current[field] = previous;
        }
        return ok;
      });
    }

    // scheduleDeferredUsersRender keeps re-checking until editing has stopped
    // and then renders. This is what guarantees a deferred refresh actually
    // happens: the timer re-arms itself on every check that still sees an edit
    // in progress, so the only way out of the loop is an actual render. It is
    // never cleared without rendering, and only one timer is ever in flight.
    function scheduleDeferredUsersRender() {
      _contactRenderPending = true;
      if (_contactDeferTimer) return;
      _contactDeferTimer = setTimeout(function() {
        _contactDeferTimer = null;
        if (contactEditInProgress()) { scheduleDeferredUsersRender(); return; }
        _contactRenderPending = false;
        // Re-derive from the latest _allUsers rather than replaying the
        // snapshot that was deferred, so the render that finally lands shows
        // current data and not whatever the poll saw minutes ago.
        try { applySortUsers(); } catch (e) { console.error('deferred renderUsers error:', e); }
      }, CONTACT_DEFER_RECHECK_MS);
    }

    /* ── User engagement / lifecycle card ────────────────────────────────
       Shown on hover of a user's name in the admin Users table. It exists to
       answer one operator question — "what should I DO with this person?" — from
       real signals rather than a single last-login date:
         • ELEVATE   — logging in, real time in their hive, tasks landing, and a
                       hive resting at its ACMM level (ready to graduate).
         • NEEDS HELP— logs in but stuck on an early journey stage or barely any
                       activity; a journey nudge is the lever.
         • AT RISK   — assigned a hive yet ~no logins, ~no hive time, no tasks, or
                       a hive already at deprovision-warning; pressure / reclaim.
       The verdict is ADVISORY: it classifies and points at existing levers (ACMM,
       journey snooze/nudge, de-provision) — it changes no state. All inputs are
       admin-only (this table is requireAdmin, and leaderboard is scrubbed for
       non-admins server-side). */
    var USERSTAT_ACTIVE_LOGIN_MIN = 3;        // legacy fallback: logins that count as "engaged"
    var USERSTAT_ACTIVE_HOURS_MIN = 1;        // hive-hours (engaged when available) that count as "engaged"
    var USERSTAT_SECS_PER_HOUR = 3600;
    var USERSTAT_ACTIVE_WINDOW_MS = 7 * 24 * 3600 * 1000;  // mirrors engagementActiveWindow (7 days) hub-side
    var USERSTAT_DORMANT_HOURS_MAX = 0.25;    // under this much hive time still reads as "basically none"

    /* Status tier presentation. The tier ITSELF (u.status_tier) is computed
       hub-side by userStatusTier from the honest signals - last audited real
       action, focus-engaged presence, live/engaged-now sets - so this map only
       styles and explains it. Keys mirror the Go tier constants. */
    var USER_TIER_META = {
      live:    {color: 'var(--green)', weight: 700, tip: 'Connected right now AND engaged: tab focused with input in the last minute.'},
      active:  {color: 'var(--green)', weight: 600, tip: 'A real audited action or focus-engaged time within the last 7 days.'},
      idle:    {color: 'var(--amber)', weight: 600, tip: 'Has logins/sessions - often just an open tab - but nothing real within the last 7 days.'},
      dormant: {color: 'var(--muted)', weight: 600, tip: 'No signal at all (no login, session, or action) within the last 30 days.'},
      never:   {color: 'var(--muted)', weight: 400, tip: 'Never completed a hub login.'},
      blocked: {color: 'var(--red)',   weight: 600, tip: 'Blocked by an admin - cannot sign in. Unblock to restore access.'}
    };
    /* Sort order for the Status column: most engaged first on ascending. */
    var USER_TIER_RANK = {live: 0, active: 1, idle: 2, dormant: 3, never: 4, blocked: 5};

    /* userTier resolves a user's tier with a fallback for a payload that
       predates status_tier (or a mid-rollout hub): blocked stays blocked and
       everyone else reads as idle - the honest default when we cannot prove
       engagement - never as "active". */
    function userTier(u) {
      return u.status_tier || (u.blocked ? 'blocked' : 'idle');
    }
    function userTierRank(u) {
      var r = USER_TIER_RANK[userTier(u)];
      return (r == null) ? USER_TIER_RANK.idle : r;
    }
    function statusTierBadge(u) {
      var tier = userTier(u);
      var meta = USER_TIER_META[tier] || USER_TIER_META.idle;
      var label = (tier === 'blocked') ? 'BLOCKED' : tier;
      return '<span title="' + escAttr(meta.tip) + '" style="color:' + meta.color +
        ';font-weight:' + meta.weight + ';cursor:help">' + esc(label) + '</span>';
    }

    /* userTaskActivity sums this user's per-hive leaderboard entries across every
       hive in _hiveRegistry, matching on github_username. leaderboard is present
       only for an admin (scrubbed otherwise), so a non-admin simply gets zeros. */
    function userTaskActivity(username) {
      var done = 0, failed = 0, activeNow = false;
      (_hiveRegistry || []).forEach(function(h) {
        (h.leaderboard || []).forEach(function(e) {
          if (e && e.github_username === username) {
            done += (e.tasksCompleted || e.TasksCompleted || 0);
            failed += (e.tasksFailed || e.TasksFailed || 0);
            if (e.active || e.Active) activeNow = true;
          }
        });
      });
      return {done: done, failed: failed, activeNow: activeNow};
    }

    /* userHiveJourneys returns [{name, role, journey, acmm, worstSeverity}] for the
       user's known hives, reusing _hiveRegistry (which carries journey + acmmLevel
       per hive). Used both to render per-hive badges and to derive the verdict. */
    function userHiveJourneys(u) {
      var hivesObj = u.hives || {};
      var out = [];
      Object.keys(hivesObj).forEach(function(hid) {
        var reg = (_hiveRegistry || []).find(function(h) { return h.id === hid; });
        if (!reg) return;
        out.push({
          id: hid,
          name: reg.name || hid,
          role: hivesObj[hid],
          journey: reg.journey || null,
          acmm: reg.acmmLevel || reg.acmm_level || 0
        });
      });
      return out;
    }

    /* userVerdict derives the lifecycle recommendation from the engagement signals
       and the user's hives' journey stages. Returns {key, label, color, tip}. */
    function userVerdict(u, act, hives) {
      var logins = u.login_count || 0;
      var hours = (u.session_seconds || 0) / USERSTAT_SECS_PER_HOUR;
      /* Honest signals, when the record has them: engaged_seconds accrues only
         while the tab was focused with recent input, and last_action_at is the
         newest audit-logged real action. Records that predate these fields (or
         whose spokes do not report presence yet) have NEITHER - for them fall
         back to the old open-tab inputs rather than treating absence as
         zero-engagement-now. */
      var engagedHours = (u.engaged_seconds || 0) / USERSTAT_SECS_PER_HOUR;
      var lastActionMs = u.last_action_at ? Date.parse(u.last_action_at) : NaN;
      var actedRecently = !isNaN(lastActionMs) && (Date.now() - lastActionMs) <= USERSTAT_ACTIVE_WINDOW_MS;
      var hasHonestData = !!(u.engaged_seconds || u.last_action_at);
      var anyDeprovision = hives.some(function(h) { return h.journey && h.journey.severity === 'deprovision-warning'; });
      var anyEarlyStage = hives.some(function(h) { return h.journey && (h.journey.stage === 'github-app' || h.journey.stage === 'method-model'); });
      var anyRestingACMM = hives.some(function(h) { return h.journey && h.journey.stage === 'acmm-level'; });
      var engaged = hasHonestData
        ? (actedRecently || engagedHours >= USERSTAT_ACTIVE_HOURS_MIN)
        : (logins >= USERSTAT_ACTIVE_LOGIN_MIN && hours >= USERSTAT_ACTIVE_HOURS_MIN);
      var dormant = hives.length > 0 && act.done === 0 && (hasHonestData
        ? (!actedRecently && engagedHours < USERSTAT_DORMANT_HOURS_MAX)
        : (logins <= 1 && hours < USERSTAT_DORMANT_HOURS_MAX));

      if (dormant || anyDeprovision) {
        return {key: 'at-risk', label: 'At risk — pressure or de-provision', color: 'var(--red)',
          tip: 'Assigned a hive but little/no engagement (or already at a de-provision warning). Consider a firm nudge or reclaiming the spoke.'};
      }
      if (engaged && act.done > 0 && anyRestingACMM) {
        return {key: 'elevate', label: 'Elevate — ready to graduate', color: 'var(--green)',
          tip: 'Active, spending real time in their hive, and tasks are landing while a hive rests at its ACMM level. Good candidate to raise autonomy / quota.'};
      }
      if (anyEarlyStage || act.done === 0) {
        return {key: 'needs-help', label: 'Needs help — nudge the journey', color: 'var(--accent)',
          tip: 'Signed in but stuck early on the adoption path or barely any activity. A journey nudge (not auto-sent) is the lever.'};
      }
      return {key: 'on-track', label: 'On track', color: 'var(--muted)',
        tip: 'Engaged and progressing; nothing to do.'};
    }

    function fmtHours(secs) {
      var h = (secs || 0) / USERSTAT_SECS_PER_HOUR;
      if (h <= 0) return '—';
      if (h < 1) return (Math.round(h * 10) / 10) + ' h';
      return (Math.round(h * 10) / 10).toString().replace(/\.0$/, '') + ' h';
    }

    function renderUserStatsCard(u, hasPendingReq) {
      var act = userTaskActivity(u.github_username);
      var hives = userHiveJourneys(u);
      var verdict = userVerdict(u, act, hives);
      var row = function(k, v) {
        return '<div style="display:flex;justify-content:space-between;gap:12px;padding:1px 0">' +
          '<span style="color:var(--muted)">' + esc(k) + '</span>' +
          '<span style="color:var(--text);text-align:right">' + v + '</span></div>';
      };
      var liveDot = act.activeNow
        ? '<span title="active on a task now" style="display:inline-block;width:7px;height:7px;border-radius:50%;background:var(--green);margin-left:6px;vertical-align:middle"></span>'
        : '';
      /* "Time in hive" is the legacy open-tab total; the two rows after it are
         the honest signals - focus-engaged time and the last audit-logged real
         action - so an admin reading the card sees exactly why the tier is
         what it is. Presence distinguishes "engaged now" from a merely-open
         idle tab (the old signal's blind spot). */
      var unameLower = String(u.github_username || '').toLowerCase();
      var presenceNow = (_liveEngagedUsers && _liveEngagedUsers.has(unameLower)) ? 'engaged now'
        : ((_liveHiveUsers && _liveHiveUsers.has(unameLower)) ? 'tab open (idle)' : '—');
      var stats =
        row('Hub logins', esc(String(u.login_count || 0)) + (u.last_login ? ' <span style="color:var(--muted)">(last ' + esc(fmtUserTS(u.last_login)) + ')</span>' : '')) +
        row('Time in hive', esc(fmtHours(u.session_seconds))) +
        row('Engaged time', esc(fmtHours(u.engaged_seconds))) +
        row('Last real action', esc(u.last_action_at ? fmtUserTS(u.last_action_at) : '—')) +
        row('Presence', esc(presenceNow)) +
        row('Joined', esc(fmtUserTS(u.created_at))) +
        row('Tasks', esc(String(act.done)) + ' done / ' + esc(String(act.failed)) + ' failed' + liveDot);

      var hiveLines = hives.length
        ? hives.map(function(h) {
            return '<div style="display:flex;align-items:center;gap:6px;padding:2px 0">' +
              '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(h.name) + '</span>' +
              '<span style="color:var(--muted);font-size:0.62rem">' + esc(h.role) + '</span>' +
              journeyBadge(h.journey) + '</div>';
          }).join('')
        : '<div style="color:var(--muted)">No hives assigned</div>';

      var verdictBlock = '<div style="margin:6px 0 2px;padding:4px 6px;border-radius:6px;border:1px solid ' + verdict.color + ';color:' + verdict.color + ';font-weight:700" title="' + escAttr(verdict.tip) + '">' + esc(verdict.label) + '</div>';

      // Assign flow relocated here as an explicit button — the click that used to
      // live on the (now inert) name. Approve-vs-assign copy mirrors the old title.
      var assignBtn = '<button onclick="openAssignForUser(\'' + escAttr(u.github_username) + '\');return false" ' +
        'style="margin-top:6px;width:100%;padding:5px 8px;border:1px solid var(--accent);background:transparent;color:var(--accent);border-radius:6px;cursor:pointer;font-size:0.72rem;font-weight:600">' +
        (hasPendingReq ? 'Approve request &amp; assign a hive' : 'Assign an available hive') + '</button>';

      return '<span class="hive-access-pop" style="display:none;position:absolute;left:0;top:calc(100% + 6px);z-index:60;' +
        'min-width:240px;max-width:320px;padding:9px 11px;border-radius:8px;border:1px solid var(--border);' +
        'background:var(--surface);box-shadow:0 6px 20px rgba(0,0,0,0.35);font-size:0.72rem;text-align:left;font-weight:400;white-space:normal">' +
        '<div style="font-weight:700;margin-bottom:4px">' + esc(userDisplayLabel(u)) +
          (userDisplayLabel(u) !== u.github_username
            ? '<span style="display:block;font-weight:400;font-size:0.62rem;color:var(--muted)">' + esc(u.github_username) + '</span>'
            : '') +
        '</div>' +
        verdictBlock +
        stats +
        '<span style="display:block;border-top:1px solid var(--border);margin:6px 0 4px"></span>' +
        '<span style="display:block;color:var(--muted);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.04em;margin-bottom:4px">Hives &amp; journey</span>' +
        hiveLines +
        assignBtn +
        '</span>';
    }

    function renderUsers(users, force) {
      var sig = JSON.stringify(users);
      if (!force && sig === _lastUsersJSON) return;
      // Never rebuild the table out from under an in-progress contact edit —
      // the inputs save on blur, so unblurred text exists only in the DOM.
      // The data is already in _allUsers; only the DOM write is postponed.
      if (contactEditInProgress()) { scheduleDeferredUsersRender(); return; }
      _lastUsersJSON = sig;
      if (!users.length) { document.getElementById('users-container').innerHTML = '<div class="loading">No users found</div>'; return; }
      var rows = users.map(function(u) {
        /* Engagement tier, hub-computed (see statusTierBadge / userStatusTier).
           The old cell rendered "active" for EVERY non-blocked user - an idle
           open tab, a never-logged-in user, and a daily driver all looked the
           same. */
        var statusCell = statusTierBadge(u);
        /* GitHub users keep the linked github.com avatar; OIDC users get the
           stored provider avatar (or initials from their real name) and no
           GitHub profile link — provider:sub is not a github.com login. */
        var isGitHubUser = String(u.provider || 'github').toLowerCase() === 'github';
        var avatar = isGitHubUser
          ? linkedAvatar(u.github_username, TABLE_AVATAR_PX,
              u.github_username + ' — GitHub profile', 'margin-right:6px')
          : userAvatar(u, TABLE_AVATAR_PX, 'margin-right:6px');
        var isAdmin = u.github_username === 'clubanderson';
        var hivesObj = u.hives || {};
        var registryIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
        var hiveIds = Object.keys(hivesObj).filter(function(hid) { return registryIds.has(hid); });
        var hiveCount = hiveIds.length;
        var expandId = 'expand-' + esc(u.github_username);
        var isExpanded = _adminExpandedUsers && _adminExpandedUsers[u.github_username];

        var hiveRows = '';
        if (hiveCount > 0) {
          hiveRows = '<tr id="' + expandId + '" style="display:' + (isExpanded ? '' : 'none') + '"><td colspan="' + USERS_TABLE_COLSPAN + '"><div style="padding:8px 12px 8px 40px;font-size:0.75rem">';
          hiveRows += '<table style="width:100%;border-collapse:collapse"><thead><tr style="color:var(--muted);font-size:0.7rem"><th style="text-align:left;padding:4px 8px">Hive</th><th>Role</th><th>Type</th><th>Link</th></tr></thead><tbody>';
          hiveIds.forEach(function(hid) {
            var role = hivesObj[hid];
            var isHosted = hid.startsWith('hosted-') || hid.startsWith('saas-');
            var regEntry = (_hiveRegistry || []).find(function(h) { return h.id === hid; });
            var hiveName = regEntry ? (regEntry.name || hid) : hid;
            // Prefer the hive's heartbeat-reported dashboard URL so firewalled
            // spokes (the heartbeat-only cluster etc.) link to their real route, not a dead
            // <id>.hive.kubestellar.io host. Fall back to the hub-reachable-cluster pattern.
            var linkBase = (regEntry && regEntry.dashboardUrl && !regEntry.dashboardUrl.includes('localhost'))
              ? regEntry.dashboardUrl : (isHosted ? 'https://' + esc(hid) + '.hive.kubestellar.io' : '');
            var linkLabel = linkBase.replace(/^https?:\/\//, '');
            var link = linkBase ? '<a href="' + esc(linkBase) + '" target="_blank" class="dash-link">' + esc(linkLabel) + '</a>' : '<span style="color:var(--muted)">local</span>';
            var typeBadge = isHosted ? '<span style="color:#60a5fa">hosted</span>' : '<span style="color:#9ca3af">local</span>';
            hiveRows += '<tr><td style="padding:4px 8px">' + esc(hiveName) + '</td><td style="text-align:center">' + esc(role) + '</td><td style="text-align:center">' + typeBadge + '</td><td>' + link + '</td></tr>';
          });
          hiveRows += '</tbody></table></div></td></tr>';
        }

        /* The name no longer NAVIGATES. Hovering it opens an engagement/lifecycle
           card (renderUserStatsCard) so an admin can read who is active enough to
           elevate, who is stuck and needs a nudge, and who is dormant and at risk
           of de-provision — WITHOUT a click that jumps somewhere. The old assign
           flow moves INTO the card as an explicit button, so it is still one hover
           away but never fires by accident. The GitHub profile stays on the avatar.
           The wrapper reuses the hive-access-wrap/pop hover panel and carries NO
           title= (TestUserStatsCardNoTitle / the single-hover-panel invariant). */
        var hasPendingReq = !!_provisionRequestsByUser[u.github_username];
        var nameCell = avatar +
          '<span class="hive-access-wrap" style="position:relative;display:inline-flex;align-items:center;gap:4px;cursor:help">' +
            '<span style="color:var(--text);font-weight:600">' + esc(userDisplayLabel(u)) + '</span>' +
            providerBadge(u) +
            (hasPendingReq ? ' <span style="color:var(--accent);font-size:0.65rem">&#9679; request</span>' : '') +
            renderUserStatsCard(u, hasPendingReq) +
          '</span>';
        return '<tr>' +
          '<td>' + nameCell + (isAdmin ? ' <span style="color:var(--accent);font-size:0.7rem">admin</span>' : '') + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.created_at)) + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.last_login)) + '</td>' +
          '<td style="text-align:left">' + renderContactCell(u) + '</td>' +
          '<td>' + countryCell(u) + '</td>' +
          '<td>' + statusCell + '</td>' +
          '<td><input type="number" min="0" max="10" value="' + (u.saas_quota || 0) + '" style="width:50px;padding:4px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);text-align:center" onchange="updateUser(\'' + esc(u.github_username) + '\',{saas_quota:parseInt(this.value)||0})"></td>' +
          '<td>' + (hiveCount > 0 ? '<a href="#" onclick="toggleAdminExpand(\'' + esc(u.github_username) + '\');return false" style="color:var(--blue);font-size:0.8rem">' + hiveCount + ' hive' + (hiveCount > 1 ? 's' : '') + '</a>' : '<span style="color:var(--muted)">0</span>') + '</td>' +
          '<td>' + (isAdmin ? '' : '<button onclick="startImpersonation(\'' + esc(u.github_username) + '\')" title="See the hub exactly as this user does — read-only" style="padding:3px 10px;background:#b45309;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">View as</button> <button onclick="updateUser(\'' + esc(u.github_username) + '\',{blocked:' + (!u.blocked) + '})" style="padding:3px 10px;background:' + (u.blocked ? 'var(--green)' : 'var(--amber)') + ';color:' + (u.blocked ? '#fff' : '#1a1a1a') + ';border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">' + (u.blocked ? 'Unblock' : 'Block') + '</button> <button onclick="deleteUser(\'' + esc(u.github_username) + '\',' + hiveCount + ')" style="padding:3px 10px;background:#b02a2a;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">Delete</button>') + '</td>' +
          '</tr>' + renderContactPanelRow(u) + hiveRows;
      }).join('');
      document.getElementById('users-container').innerHTML =
        // Shared autocomplete vocabulary for the Company combobox, rebuilt each
        // render from the distinct company values across all users so it grows
        // as new companies are entered.
        '<datalist id="contact-company-suggestions">' + companyDatalistOptionsHTML() + '</datalist>' +
        '<table class="hive-table"><thead><tr>' +
        '<th onclick="sortUsers(\'github_username\')" style="cursor:pointer">User ⇅</th><th onclick="sortUsers(\'created_at\')" style="cursor:pointer">Joined ⇅</th><th onclick="sortUsers(\'last_login\')" style="cursor:pointer">Last Login ⇅</th><th onclick="sortUsers(\'full_name\')" style="cursor:pointer">Contact ⇅</th><th onclick="sortUsers(\'country\')" style="cursor:pointer">Country ⇅</th><th onclick="sortUsers(\'status\')" style="cursor:pointer">Status ⇅</th><th onclick="sortUsers(\'saas_quota\')" style="cursor:pointer">Quota ⇅</th><th onclick="sortUsers(\'hiveCount\')" style="cursor:pointer">Hives ⇅</th><th>Actions</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
      // The contact panels are built as raw HTML above; wire their listeners
      // once the rows are actually in the DOM. Inline on* handlers are avoided
      // for these fields because esc() does not escape apostrophes, so a name
      // or note containing one would break out of an inline handler string.
      bindContactPanels();
    }

    /* updateUser previously ignored the response entirely: a 404/400/500 was
       indistinguishable from success, so a rejected edit looked like it had
       been saved until the next poll quietly reverted the field. It now checks
       resp.ok and reports the server's reason, and returns a boolean so
       callers can decide whether to close the editor.

       readErrorMessage is used rather than a bare resp.json(): the handler
       writes errors with writeJSONError so the body IS JSON, but a proxy or
       middleware fault can still return HTML/plain text, and resp.json() on
       that throws — which is exactly how #2348's handleAlertAck turned a real
       cause into a generic "failed". */
    async function readErrorMessage(resp, fallback) {
      try {
        var text = await resp.text();
        if (text) {
          try {
            var parsed = JSON.parse(text);
            if (parsed && parsed.error) return String(parsed.error);
          } catch(je) {
            /* Not JSON — show the raw text, which is still more useful than
               a bare status code. */
            return text.trim().substring(0, ERROR_TEXT_MAX_CHARS);
          }
        }
      } catch(e) {}
      return fallback + ' (HTTP ' + resp.status + ')';
    }

    /* Longest slice of a non-JSON error body shown in a toast. Long enough for
       a real proxy/server message, short enough not to fill the screen. */
    var ERROR_TEXT_MAX_CHARS = 200;

    /* Reason for the most recent failed save, per user. A silent blur-save
       records the cause here instead of toasting it, so the Save button can
       report the REAL reason rather than a generic "failed" when the admin
       finally commits. Cleared on any success for that user. */
    var _contactLastError = {};

    /* opts.silent suppresses the toast (blur-saves); the failure is still
       recorded in _contactLastError and still returns false. */
    async function updateUser(username, updates, opts) {
      var silent = !!(opts && opts.silent);
      function fail(reason) {
        _contactLastError[username] = reason;
        if (!silent) hiveToast('Could not save ' + username + ': ' + reason, 'error');
        return false;
      }
      try {
        var resp = await fetch('/api/saas/admin/users/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(updates)
        });
        if (!resp.ok) {
          return fail(await readErrorMessage(resp, 'update failed'));
        }
        delete _contactLastError[username];
        loadAdminUsers();
        return true;
      } catch(e) {
        return fail(e.message);
      }
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
      // Confirm with the friendly hive NAME the operator sees in the table, not
      // the raw id / vanity-URL slug. Fall back to the id if the row is gone.
      var _row = document.querySelector('[data-hive-id="' + (window.CSS && CSS.escape ? CSS.escape(id) : id) + '"]');
      var name = (_row && _row.getAttribute('data-hive-name')) || id;
      if (!await hiveConfirm('Delete "' + name + '"? This removes the namespace, PV, OCI storage, and all data.')) return;
      var btns = document.querySelectorAll('button[onclick*="deleteHive"]');
      btns.forEach(function(b) { b.disabled = true; b.textContent = 'Deleting...'; b.style.opacity = '0.6'; });
      // Mark the row as deleting so its status shows "Deleting…" until the next
      // refresh removes it (or clears the flag on failure).
      _deletingHives[id] = true;
      try {
        gtag('event','hive_deleted',{hive_id:id});
        hiveToast('Deleting "' + name + '"…', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) {
          var d = await resp.json().catch(function(){ return {}; });
          hiveToast(d.error || 'Delete failed', 'error');
          delete _deletingHives[id];
          // Refresh regardless: the hub removes the registry entry even when
          // teardown fails, so the listing may already be correct.
          loadHives();
          return;
        }
        var ok = await resp.json().catch(function(){ return {}; });
        if (ok.status === 'partially_deleted') {
          /* partially_deleted is a SUCCESS, not a failure: the registry row is
             gone (which is what the user asked for and what they can see), but
             no cluster config was available to tear down the namespace/PV/OCI
             storage, so those may survive. Rendering it as an 'error' toast made
             a partial success look like the delete had failed even though the
             row vanished. Show it as an informational notice instead. */
          hiveToast('Removed "' + name + '" from the hub; some cloud resources may need manual cleanup' + (ok.warning ? ' (' + ok.warning + ')' : ''), 'info');
        } else {
          hiveToast('Deleted "' + name + '"', 'success');
        }
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); delete _deletingHives[id]; }
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

    /* "View all activity" in the status hover panel. Delegated rather than an
       inline onclick because the hive name is user-controlled and esc() does
       not escape apostrophes — a name containing one would break out of an
       inline handler string. The values are read back from data attributes,
       where esc() IS sufficient. */
    document.addEventListener('click', function(e) {
      var btn = e.target.closest ? e.target.closest('.hover-view-timeline') : null;
      if (!btn) return;
      openTimelineModal(btn.getAttribute('data-hive-id') || '', btn.getAttribute('data-hive-name') || '');
    });


    var ASSIGN_DEFAULT_ACMM = 2;
    /* openAssignModal claims one placeholder for a real project.
       prefillOwner (optional) pre-fills the Owner field — used when the flow is
       started by clicking a user in the admin users table.
       placeholders (optional) turns the fixed hive id into a dropdown, so that
       user-first entry point can retarget without backing out to the hive list.
       Both are omitted by the original ⋮ → "Assign / Claim" call site. */
    /* hiveNotify (below) and hivePrompt replace the native alert and prompt
       dialogs; the native confirm dialog is replaced by the existing hiveConfirm
       defined earlier. Native dialogs are jarring in a themed dashboard, cannot
       be styled, and block the whole tab; they also read as a browser warning
       rather than as part of the product. These follow the same overlay pattern
       the assign, access and timeline modals already use.

       Promise-based so they drop into the existing await-style call sites
       without restructuring them. Escape and a backdrop click both resolve
       false, matching what a user expects from a dismissable dialog. */
    function _hiveDialog(opts) {
      return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:3000;display:flex;align-items:center;justify-content:center';
        var btn = 'padding:7px 14px;border-radius:6px;border:1px solid var(--border);cursor:pointer;font-size:0.8rem';
        var body = (opts.body || '').split('\n').map(function(line) {
          return line ? '<p style="margin:0 0 8px 0;color:var(--muted);font-size:0.85rem;line-height:1.5">' + esc(line) + '</p>' : '';
        }).join('');
        overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:22px;max-width:460px;width:90%">' +
          '<h3 style="margin:0 0 10px 0;font-size:1rem">' + esc(opts.title || '') + '</h3>' + body +
          '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:18px">' +
          (opts.cancel ? '<button data-act="no" style="' + btn + ';background:transparent;color:var(--fg)">' + esc(opts.cancel) + '</button>' : '') +
          '<button data-act="yes" style="' + btn + ';background:' + (opts.danger ? '#da3633' : 'var(--accent,#3fb950)') + ';color:#fff;border-color:transparent;font-weight:600">' + esc(opts.ok || 'OK') + '</button>' +
          '</div></div>';
        function done(v) {
          document.removeEventListener('keydown', onKey);
          overlay.remove();
          resolve(v);
        }
        function onKey(e) { if (e.key === 'Escape') done(false); }
        overlay.addEventListener('click', function(e) {
          if (e.target === overlay) { done(false); return; }
          var act = e.target.getAttribute && e.target.getAttribute('data-act');
          if (act) done(act === 'yes');
        });
        document.addEventListener('keydown', onKey);
        document.body.appendChild(overlay);
        var y = overlay.querySelector('[data-act="yes"]');
        if (y) y.focus();
      });
    }
    /* hiveConfirm already exists above (msg, rawHTML) -> Promise<bool>, added by
       the themed-overlay work this dashboard already relies on; this feature
       reuses it rather than redefining it. hiveNotify below is the native-alert
       replacement, following the same overlay pattern. */
    function hiveNotify(title, body) {
      return _hiveDialog({title: title, body: body, ok: 'OK'});
    }
    /* hivePrompt replaces window.prompt(). Resolves to the trimmed string, or
       null when cancelled — same contract as the native call it replaces, so
       existing null checks keep working. */
    function hivePrompt(title, defaultValue, opts) {
      opts = opts || {};
      return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:3000;display:flex;align-items:center;justify-content:center';
        var btn = 'padding:7px 14px;border-radius:6px;border:1px solid var(--border);cursor:pointer;font-size:0.8rem';
        overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:22px;max-width:420px;width:90%">' +
          '<h3 style="margin:0 0 12px 0;font-size:1rem">' + esc(title || '') + '</h3>' +
          '<input id="_hive-prompt-input" type="text" value="' + esc(defaultValue || '') + '" style="width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box;font-size:0.85rem">' +
          '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
          '<button data-act="no" style="' + btn + ';background:transparent;color:var(--fg)">Cancel</button>' +
          '<button data-act="yes" style="' + btn + ';background:var(--accent,#3fb950);color:#fff;border-color:transparent;font-weight:600">' + esc(opts.ok || 'Save') + '</button>' +
          '</div></div>';
        function done(v) {
          document.removeEventListener('keydown', onKey);
          overlay.remove();
          resolve(v);
        }
        function submit() {
          var el = document.getElementById('_hive-prompt-input');
          done(el ? el.value.trim() : null);
        }
        function onKey(e) {
          if (e.key === 'Escape') done(null);
          if (e.key === 'Enter') submit();
        }
        overlay.addEventListener('click', function(e) {
          if (e.target === overlay) { done(null); return; }
          var act = e.target.getAttribute && e.target.getAttribute('data-act');
          if (act === 'yes') submit();
          else if (act === 'no') done(null);
        });
        document.addEventListener('keydown', onKey);
        document.body.appendChild(overlay);
        var inp = document.getElementById('_hive-prompt-input');
        if (inp) { inp.focus(); inp.select(); }
      });
    }

    function openAssignModal(hiveId, prefillOwner, placeholders) {
      var h = (_allDashHives || []).reduce(function(m, x) { return x.id === hiveId ? x : m; }, null) || {};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var acmmOpts = '';
      for (var lv = 0; lv <= 6; lv++) { acmmOpts += '<option value="' + lv + '"' + (lv === ASSIGN_DEFAULT_ACMM ? ' selected' : '') + '>' + lv + '</option>'; }
      var orgVal = esc(h.org || '');
      var reposVal = esc((h.repos || []).join(', '));
      /* With a placeholder list, let the admin retarget in place; without one,
         keep the original fixed-hive wording. */
      var hasPicker = !!(placeholders && placeholders.length);
      var hiveField;
      if (hasPicker) {
        var phOpts = (placeholders || []).map(function(p) {
          return '<option value="' + esc(p.id) + '"' + (p.id === hiveId ? ' selected' : '') + '>' +
            esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        }).join('');
        hiveField =
          '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim an available placeholder for a real project.</div>' +
          '<label style="' + lbl + '">Placeholder to assign *</label>' +
          '<select id="assign-hive-pick" style="' + fld + '">' + phOpts + '</select>';
      } else {
        hiveField = '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim placeholder <strong style="color:var(--fg)">' + esc(hiveId) + '</strong> for a real project.</div>';
      }
      /* The GitHub instance defaults to the SELECTED placeholder's cluster.
         With a picker the selection can change, so resolve the cluster of the
         initially-selected placeholder and re-resolve on change below. */
      function clusterIdForSelection(id) {
        var fromList = (placeholders || []).reduce(function(m, p) { return (p && p.id === id) ? p : m; }, null);
        if (fromList) return fromList.cluster_id || '';
        var fromDash = (_allDashHives || []).reduce(function(m, x) { return (x && x.id === id) ? x : m; }, null);
        return (fromDash && fromDash.clusterId) || '';
      }
      var assignClusterHost = clusterGitHubHost(clusterIdForSelection(hiveId));
      var content =
        hiveField +
        '<label style="' + lbl + '">Owner (GitHub login) *</label>' +
        '<input id="assign-owner" style="' + fld + '" value="' + esc(prefillOwner || '') + '" placeholder="octocat">' +
        '<label style="' + lbl + '">Org *</label>' +
        '<input id="assign-org" style="' + fld + '" value="' + orgVal + '" placeholder="my-org">' +
        githubHostChoiceMarkup('assign', assignClusterHost, '', fld, lbl) +
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
      wireGitHubHostChoice('assign', assignClusterHost);
      /* Retargeting the placeholder can change the cluster, and with it the
         default GitHub instance. Re-resolve so the note never claims the
         previous cluster's host. An explicit override the admin already made
         (public / a typed host) is left alone — only the "cluster default"
         label and note follow the selection. */
      var phPick = document.getElementById('assign-hive-pick');
      if (phPick) {
        phPick.addEventListener('change', function() {
          assignClusterHost = clusterGitHubHost(clusterIdForSelection(phPick.value));
          var sel = document.getElementById('assign-ghhost');
          if (sel && sel.options && sel.options.length) {
            sel.options[0].textContent = assignClusterHost
              ? 'Cluster default — ' + assignClusterHost
              : 'Cluster default';
          }
          wireGitHubHostChoice('assign', assignClusterHost);
        });
      }
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
      /* When the modal was opened with a placeholder picker, the dropdown — not
         the id baked into this onclick — is the authority on the target. */
      var pick = document.getElementById('assign-hive-pick');
      if (pick && pick.value) hiveId = pick.value;
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
          /* github_host: '' = let the server backfill from the cluster,
             'public' = force public github.com, otherwise a GHE hostname. */
          body: JSON.stringify({owner: owner, org: org, github_host: githubHostChoiceValue('assign'), repos: repos, primary_repo: primary, project_name: name, acmm_level: acmm, is_public: isPublic, app_id: appId, installation_id: installId, app_private_key: appKey})
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
`

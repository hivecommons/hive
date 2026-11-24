    function overviewRepoName(repo) {
      return (repo && (repo.full || repo.name)) || '';
    }

    function overviewAllRepoNames(repos) {
      return Array.from(new Set((repos || []).map(overviewRepoName).filter(Boolean))).sort((a, b) => a.localeCompare(b));
    }

    function overviewSavedRepoNames() {
      try {
        const raw = localStorage.getItem(OVERVIEW_REPOS_KEY);
        if (raw === null) return null;
        const parsed = JSON.parse(raw);
        return Array.isArray(parsed) ? parsed.map(v => String(v)).filter(Boolean) : null;
      } catch { return null; }
    }

    function saveOverviewRepoNames(repos) {
      try { localStorage.setItem(OVERVIEW_REPOS_KEY, JSON.stringify(repos || [])); } catch {}
    }

    function overviewSelectedRepoNames(repos) {
      const all = overviewAllRepoNames(repos);
      const saved = overviewSavedRepoNames();
      if (saved === null) return all;
      const allowed = new Set(all);
      return saved.filter(repo => allowed.has(repo));
    }

    function overviewFilterRepos(repos) {
      const selected = overviewSelectedRepoNames(repos);
      const selectedSet = new Set(selected);
      return (repos || []).filter(repo => selectedSet.has(overviewRepoName(repo)));
    }

    function overviewRepoSubtitleSuffix(repos, selected) {
      const all = overviewAllRepoNames(repos);
      if (!all.length) return 'no configured repos';
      if ((selected || []).length === all.length) return 'all repos';
      if ((selected || []).length === 1) return selected[0];
      return 'across ' + (selected || []).length + ' of ' + all.length + ' repos';
    }

    function overviewRepoOrgs(repos) {
      return Array.from(new Set(overviewAllRepoNames(repos).map(name => name.split('/')[0]).filter(Boolean))).sort((a, b) => a.localeCompare(b));
    }

    // The chart slices come from the server's ownership decision: every row
    // carries its slice as chart_slice (Your turn, To close, Waiting on someone
    // else, otherwise a band with the waiting, agent-filed and done rules
    // skipped) and window._lastStatus.overview_slices lists them in plot
    // order, so every plotted item sits in exactly one slice (#11507).
    function overviewSliceSpecs(kind) {
      const specs = ((window._lastStatus || {}).overview_slices || {})[kind === 'issue' ? 'issues' : 'prs'];
      return Array.isArray(specs) ? specs : [];
    }

    // The Overview card (window._lastStatus.overview_card) sorts every open
    // issue and PR into one band from what the hive already decided: each row
    // carries card_band and card_group, the card lists the band specs in plot
    // order, the groups (one per term tile) and the six tiles with their
    // breakdowns. An older server sends no card; the chart slices above stand
    // in, so every caller below checks for the card first.
    function overviewCard() {
      const card = (window._lastStatus || {}).overview_card;
      return card && typeof card === 'object' ? card : null;
    }

    function overviewCardSpecs(kind) {
      const specs = (overviewCard() || {})[kind === 'issue' ? 'issues' : 'prs'];
      return Array.isArray(specs) ? specs : [];
    }

    function overviewCardGroups() {
      const groups = (overviewCard() || {}).groups;
      return Array.isArray(groups) ? groups : [];
    }

    // The one test for card mode, shared by the slices, the tiles, the
    // Projects filters, the navigation and the charts: the status carries the
    // card and the card lists bands. Without it every caller draws today's
    // card exactly.
    function overviewCardMode() {
      return overviewCardSpecs('issue').length > 0 || overviewCardSpecs('pr').length > 0;
    }

    function overviewCardTiles() {
      const tiles = (overviewCard() || {}).tiles;
      return Array.isArray(tiles) ? tiles : [];
    }

    // A repo's card rows, as [row, held]. Draft PRs the hive sidelines are not
    // open PR rows; the card still counts them (Draft, in Outside), so their
    // nonActionablePrs rows are placed too. The server stamps every copy of
    // an item listed twice (the claim guard's copy of an actionable issue,
    // an App stale draft on both PR lists) but counts it once, the first copy
    // in this order winning, so only that copy is returned here.
    function overviewCardRows(repo, kind) {
      const all = kind === 'issue'
        ? [].concat(
          ((repo && repo.actionableIssues) || []).map(row => [row, false]),
          ((repo && repo.heldIssues) || []).map(row => [row, true]),
          ((repo && repo.nonActionableIssues) || []).map(row => [row, false]))
        : [].concat(
          ((repo && repo.openPrs) || []).map(row => [row, false]),
          ((repo && repo.heldPrs) || []).map(row => [row, true]),
          ((repo && repo.nonActionablePrs) || []).map(row => [row, false]));
      const seen = new Set();
      return all.filter(([row]) => {
        if (!row || !row.card_band) return false;
        const number = Number(row.number);
        if (seen.has(number)) return false;
        seen.add(number);
        return true;
      });
    }

    function overviewCardBandSlices(kind, repos, specs) {
      const items = new Map(specs.map(spec => [spec.key, []]));
      const field = kind === 'issue' ? 'issue' : 'pr';
      (repos || []).forEach(r => {
        const repo = overviewRepoName(r);
        overviewCardRows(r, kind).forEach(([row, held]) => {
          const list = items.get(row && row.card_band);
          if (!list) return;
          list.push(Object.assign({}, row || {}, {
            item: row,
            [field]: row,
            repo,
            held,
            info: row,
            updated_at: (row && row.updated_at) || '',
            created_at: (row && row.created_at) || '',
            number: row && row.number
          }));
        });
      });
      const raw = entry => entry.item || entry[field] || entry;
      const order = kind === 'issue'
        ? (a, b) => issueUpdatedAt(raw(a)) - issueUpdatedAt(raw(b)) || Number(raw(a).number || 0) - Number(raw(b).number || 0)
        : (a, b) => prUpdatedAt(raw(a)) - prUpdatedAt(raw(b)) || prReviewClassRank(raw(a)) - prReviewClassRank(raw(b)) || prCreatedAt(raw(a)) - prCreatedAt(raw(b)) || Number(raw(a).number || 0) - Number(raw(b).number || 0);
      return specs.map(spec => {
        const sorted = items.get(spec.key).sort(order);
        return { key: spec.key, label: spec.label, rule: spec.rule, group: spec.group, keep: !!spec.keep, hideZero: !!spec.hide_zero, card: true, count: sorted.length, className: 'overview-card-' + kind + '-' + spec.key, items: sorted };
      });
    }

    function overviewIssueBandSlices(repos) {
      if (overviewCardMode()) return overviewCardBandSlices('issue', repos, overviewCardSpecs('issue'));
      const specs = overviewSliceSpecs('issue');
      const items = new Map(specs.map(spec => [spec.key, []]));
      (repos || []).forEach(r => {
        const repo = overviewRepoName(r);
        const place = (issue, held) => {
          const list = items.get(issue && issue.chart_slice);
          if (!list) return;
          list.push(Object.assign({}, issue || {}, {
            item: issue,
            issue,
            repo,
            held,
            info: issue,
            updated_at: (issue && issue.updated_at) || '',
            created_at: (issue && issue.created_at) || '',
            number: issue && issue.number
          }));
        };
        (r.actionableIssues || []).forEach(issue => place(issue, false));
        (r.heldIssues || []).forEach(issue => place(issue, true));
        (r.nonActionableIssues || []).forEach(issue => place(issue, false));
      });
      return specs.map(spec => {
        const sorted = items.get(spec.key).sort((a, b) => issueUpdatedAt(a.item || a.issue || a) - issueUpdatedAt(b.item || b.issue || b) || Number((a.item || a.issue || a).number || 0) - Number((b.item || b.issue || b).number || 0));
        return { key: spec.key, label: spec.label, rule: spec.rule, count: sorted.length, className: 'overview-issue-' + spec.key, items: sorted };
      });
    }

    function overviewPRBandSlices(repos) {
      if (overviewCardMode()) return overviewCardBandSlices('pr', repos, overviewCardSpecs('pr'));
      const specs = overviewSliceSpecs('pr');
      const items = new Map(specs.map(spec => [spec.key, []]));
      (repos || []).forEach(r => {
        const repo = overviewRepoName(r);
        const place = (pr, held) => {
          const list = items.get(pr && pr.chart_slice);
          if (!list) return;
          list.push(Object.assign({}, pr || {}, {
            item: pr,
            pr,
            repo,
            held,
            info: pr,
            updated_at: (pr && pr.updated_at) || '',
            created_at: (pr && pr.created_at) || '',
            number: pr && pr.number
          }));
        };
        (r.openPrs || []).forEach(pr => place(pr, false));
        (r.heldPrs || []).forEach(pr => place(pr, true));
      });
      return specs.map(spec => {
        const sorted = items.get(spec.key).sort((a, b) => prUpdatedAt(a.item || a.pr || a) - prUpdatedAt(b.item || b.pr || b) || prReviewClassRank(a.item || a.pr || a) - prReviewClassRank(b.item || b.pr || b) || prCreatedAt(a.item || a.pr || a) - prCreatedAt(b.item || b.pr || b) || Number((a.item || a.pr || a).number || 0) - Number((b.item || b.pr || b).number || 0));
        return { key: spec.key, label: spec.label, rule: spec.rule, count: sorted.length, className: 'overview-pr-' + spec.key, items: sorted };
      });
    }

    // Each Overview tile filters the Projects cards to the rows the server
    // stamped with that equation term (overview_term), or with the card group
    // (card_group) when the status carries the Overview card; a term tile's
    // key is its card group. The browser never re-derives a term from labels
    // or bands.
    const OVERVIEW_PROJECT_FILTERS = {
      'open-issues': { label: 'Total open issues', kinds: ['issue'], bucketKind: 'issues' },
      'open-prs': { label: 'Total open PRs', kinds: ['pr'], bucketKind: 'prs' },
      'actionable-now': { label: 'Actionable now', kinds: ['issue', 'pr'], term: 'actionable' },
      'your-turn': { label: 'Your turn', kinds: ['issue', 'pr'], term: 'your_turn' },
      'waiting-elsewhere': { label: 'Waiting on someone else', kinds: ['issue', 'pr'], term: 'waiting_elsewhere' },
      'held': { label: 'Held', kinds: ['issue', 'pr'], term: 'held' },
      'blocked': { label: 'Blocked', kinds: ['pr'], term: 'blocked' },
      'outside': { label: 'Outside', kinds: ['issue', 'pr'], term: 'outside', outside: true }
    };
    let _overviewProjectFilterKey = null;
    let _overviewProjectFilterCount = null;

    function overviewProjectFilterSpec(key) {
      const spec = OVERVIEW_PROJECT_FILTERS[String(key || '')] || null;
      // With the card the actionable tile is "Hive has it" and the your-turn
      // tile "Needs a person"; their filter chips say so.
      if (spec && spec.term === 'actionable' && overviewCardMode()) return Object.assign({}, spec, { label: 'Hive has it' });
      if (spec && spec.term === 'your_turn' && overviewCardMode()) return Object.assign({}, spec, { label: 'Needs a person' });
      return spec;
    }

    function overviewProjectFilterKeyFromURL() {
      const read = raw => {
        try {
          const q = String(raw || '');
          const params = new URLSearchParams(q.startsWith('?') ? q.slice(1) : q);
          return params.get('band') || params.get('kpi') || '';
        } catch { return ''; }
      };
      const hash = String(location.hash || '').replace(/^#/, '');
      if (hash.startsWith('projects')) {
        const idx = hash.indexOf('?');
        const key = idx >= 0 ? read(hash.slice(idx + 1)) : '';
        if (overviewProjectFilterSpec(key)) return key;
      }
      const key = read(location.search || '');
      return overviewProjectFilterSpec(key) ? key : '';
    }

    function overviewProjectFilterState() {
      const key = _overviewProjectFilterKey == null ? overviewProjectFilterKeyFromURL() : _overviewProjectFilterKey;
      const spec = overviewProjectFilterSpec(key);
      return spec ? Object.assign({ key, selectedCount: _overviewProjectFilterCount }, spec) : null;
    }

    // Parked rows (server band `waiting` on a non-actionable row) are ordinary
    // issue rows on the card, so they are matched by the band filters like any
    // other. `parked` keeps them out of the Overview's outside-bucket maths,
    // which counts them through the forge totals.
    function overviewRepoTrackedIssueEntries(repo) {
      const name = overviewRepoName(repo);
      return (repo && repo.actionableIssues || []).map(issue => ({ kind: 'issue', item: issue, issue, repo: name, held: false, band: issue && issue.band }))
        .concat((repo && repo.heldIssues || []).map(issue => ({ kind: 'issue', item: issue, issue, repo: name, held: true, band: issue && issue.band })))
        .concat(repoCardParkedIssues(repo).map(issue => ({ kind: 'issue', item: issue, issue, repo: name, held: false, band: issue.band, parked: true })));
    }

    function overviewRepoTrackedPREntries(repo) {
      const name = overviewRepoName(repo);
      return (repo && repo.openPrs || []).map(pr => ({ kind: 'pr', item: pr, pr, repo: name, held: false, band: pr && pr.band }))
        .concat((repo && repo.heldPrs || []).map(pr => ({ kind: 'pr', item: pr, pr, repo: name, held: true, band: pr && pr.band })));
    }

    function overviewProjectBucketCount(repo, kind, keys) {
      const bd = (repo && repo.workBreakdown && repo.workBreakdown[kind]) || {};
      return (keys || []).reduce((n, key) => n + Number(bd[key] || 0), 0);
    }

    // Non-actionable issue rows the decision moved out of Outside (they are
    // your turn or waiting on someone else); the rest stay in Outside.
    function overviewNonActionableMoved(repo) {
      return ((repo && repo.nonActionableIssues) || []).filter(issue => issue && issue.overview_term && issue.overview_term !== 'outside');
    }

    function overviewProjectOutsideIssueCount(repo) {
      const raw = overviewRepoOpenIssueCount(repo);
      const tracked = (repo && repo.actionableIssues || []).length + (repo && repo.heldIssues || []).length;
      return Math.max(0, Number(raw || tracked) - tracked - overviewNonActionableMoved(repo).length);
    }

    function overviewProjectOutsidePRCount(repo) {
      const raw = overviewRepoOpenPRCount(repo);
      const tracked = (repo && repo.openPrs || []).length + (repo && repo.heldPrs || []).length;
      return Math.max(0, Number(raw || tracked) - tracked);
    }

    function overviewProjectFilterMatchesEntry(entry, filter) {
      if (!filter) return true;
      if (filter.kinds && !filter.kinds.includes(entry.kind)) return false;
      if (filter.term && overviewCardMode()) return String((entry.item && entry.item.card_group) || '') === (filter.group || filter.key);
      if (filter.term) return String((entry.item && entry.item.overview_term) || '') === filter.term;
      return true;
    }

    // Card rows a repo has (card_band set) and the part of its displayed
    // open count that no row stands for: items a source counted but could not
    // list, and sidelined PRs that are not drafts. That remainder is in
    // Outside, like the server's overview_card.remainder.
    function overviewCardRepoRemainder(repo) {
      const shown = overviewRepoForgeTotals([repo]);
      const rows = kind => overviewCardRows(repo, kind).filter(([row]) => row && row.card_band).length;
      return { issues: Math.max(0, Number(shown.issues.forge || 0) - rows('issue')), prs: Math.max(0, Number(shown.prs.forge || 0) - rows('pr')) };
    }

    // With the card, a tile's Projects filter counts exactly what the tile
    // counts for this repo: the matching rows listed above, the card rows a
    // Projects card does not list (non-actionable issues without a band,
    // sidelined drafts) and, for Outside, the remainder.
    function overviewCardFilterBuckets(repo, filter, issueEntries, prEntries) {
      const group = filter.group || filter.key;
      const listed = new Set(issueEntries.concat(prEntries).map(entry => entry.item));
      const unlisted = kind => overviewCardRows(repo, kind).filter(([row]) => row && row.card_group === group && !listed.has(row)).length;
      const buckets = [];
      const issues = filter.kinds.includes('issue') ? unlisted('issue') : 0;
      const prs = filter.kinds.includes('pr') ? unlisted('pr') : 0;
      if (issues) buckets.push({ kind: 'issue', count: issues, reason: 'the Overview counts it; this repo card does not list it as a row', label: filter.label + ' · issues not listed here' });
      if (prs) buckets.push({ kind: 'pr', count: prs, reason: 'the Overview counts it; this repo card does not list it as a row', label: filter.label + ' · PRs not listed here' });
      if (group === 'outside') {
        const rest = overviewCardRepoRemainder(repo);
        if (rest.issues) buckets.push({ kind: 'issue', count: rest.issues, reason: 'open on GitHub, not listed by the scanner', label: 'Issues counted but not listed' });
        if (rest.prs) buckets.push({ kind: 'pr', count: rest.prs, reason: 'open on GitHub, not listed by the scanner', label: 'PRs counted but not listed' });
      }
      return buckets;
    }

    function overviewProjectFilterRepoMatches(repo, filter) {
      filter = filter || overviewProjectFilterState();
      if (!filter) return { issues: overviewRepoTrackedIssueEntries(repo), prs: overviewRepoTrackedPREntries(repo), buckets: [], count: 0 };
      const issueEntries = overviewRepoTrackedIssueEntries(repo).filter(entry => overviewProjectFilterMatchesEntry(entry, filter));
      const prEntries = overviewRepoTrackedPREntries(repo).filter(entry => overviewProjectFilterMatchesEntry(entry, filter));
      if (filter.term && overviewCardMode()) {
        const cardBuckets = overviewCardFilterBuckets(repo, filter, issueEntries, prEntries);
        return { issues: issueEntries, prs: prEntries, buckets: cardBuckets, count: issueEntries.length + prEntries.length + cardBuckets.reduce((n, b) => n + Number(b.count || 0), 0) };
      }
      const buckets = [];
      if (filter.key === 'open-issues') {
        const missing = Math.max(0, Number(overviewRepoOpenIssueCount(repo) || 0) - issueEntries.length);
        if (missing) buckets.push({ kind: 'issue', count: missing, reason: 'outside buckets', label: 'Open issues outside Overview bands' });
      } else if (filter.key === 'open-prs') {
        const missing = Math.max(0, Number(overviewRepoOpenPRCount(repo) || 0) - prEntries.length);
        if (missing) buckets.push({ kind: 'pr', count: missing, reason: 'outside buckets', label: 'Open PRs outside Overview bands' });
      } else if (filter.key === 'outside') {
        // Parked rows the decision left in Outside are listed as entries above;
        // the bucket counts only the rest.
        const issueOutside = Math.max(0, overviewProjectOutsideIssueCount(repo) - issueEntries.filter(entry => entry.parked).length);
        const prOutside = overviewProjectOutsidePRCount(repo);
        if (issueOutside) buckets.push({ kind: 'issue', count: issueOutside, reason: 'outside buckets', label: 'Issues outside Overview bands' });
        if (prOutside) buckets.push({ kind: 'pr', count: prOutside, reason: 'outside buckets', label: 'PRs outside Overview bands' });
      } else if (filter.term && filter.kinds.includes('issue')) {
        // Parked rows carry a band and are already listed as entries; the other
        // non-actionable rows in this term have no band, so a count stands in.
        const bandless = overviewNonActionableMoved(repo).filter(issue => issue.overview_term === filter.term && !issue.band).length;
        if (bandless) buckets.push({ kind: 'issue', count: bandless, reason: 'no band on the card', label: filter.label + ' · issues without a band' });
      }
      const count = issueEntries.length + prEntries.length + buckets.reduce((n, b) => n + Number(b.count || 0), 0);
      return { issues: issueEntries, prs: prEntries, buckets, count };
    }

    function overviewProjectFilterSummary(key, repos) {
      const filter = overviewProjectFilterSpec(key) ? Object.assign({ key }, overviewProjectFilterSpec(key)) : null;
      if (!filter) return { key: '', label: '', count: 0 };
      let count = (repos || []).reduce((n, repo) => n + overviewProjectFilterRepoMatches(repo, filter).count, 0);
      if (key === 'open-issues') count = (repos || []).reduce((n, r) => n + Number(overviewRepoOpenIssueCount(r) || 0), 0);
      else if (key === 'open-prs') count = (repos || []).reduce((n, r) => n + Number(overviewRepoOpenPRCount(r) || 0), 0);
      return { key, label: filter.label, count };
    }

    function overviewSetProjectFilterHash(key) {
      const hash = key ? ('#projects?band=' + encodeURIComponent(key)) : '#section-repos-section';
      try {
        const u = new URL(window.location.href);
        u.searchParams.delete('band');
        u.searchParams.delete('kpi');
        const qs = u.searchParams.toString();
        history.replaceState(null, '', u.pathname + (qs ? '?' + qs : '') + hash);
      } catch {
        history.replaceState(null, '', window.hiveURLWithHash ? window.hiveURLWithHash(hash) : hash);
      }
    }

    function overviewApplyKPIProjectFilter(key, count) {
      if (!overviewProjectFilterSpec(key)) return;
      _overviewProjectFilterKey = key;
      const n = Number(count);
      _overviewProjectFilterCount = Number.isFinite(n) ? n : null;
      overviewSetProjectFilterHash(key);
      repaintReposForWidth();
      ocNavigate('repos-section');
      overviewSetProjectFilterHash(key);
    }

    function clearOverviewProjectFilter() {
      _overviewProjectFilterKey = '';
      _overviewProjectFilterCount = null;
      overviewSetProjectFilterHash('');
      repaintReposForWidth();
    }

    function clearProjectsFilter() {
      writeRepoPillFilterState({ mode: repoPillFilterState().mode, kinds: [], repo: '' });
      _overviewProjectFilterKey = '';
      _overviewProjectFilterCount = null;
      overviewSetProjectFilterHash('');
      repaintReposForWidth();
    }

    function overviewProjectFilterChip(filter, count) {
      if (!filter) return '';
      return `Filtered by: ${esc(filter.label)} (${Number(count || 0)}) <button type="button" data-action="clearOverviewProjectFilter" data-stop="1" data-prevent="1" aria-label="Clear Overview KPI filter" title="Clear filter">✕</button>`;
    }

    function overviewProjectBucketRowsHTML(buckets) {
      return (buckets || []).map(bucket => `<div class="repo-pill-row repo-overview-filter-bucket"><span class="repo-pill-status repo-issue-pill held">${esc(bucket.kind === 'pr' ? 'PRs' : 'issues')}</span><span class="repo-pill-main repo-issue-pill held"><span class="pill-num">${esc(String(Number(bucket.count || 0)))}</span><span class="pill-title">${esc(bucket.label || 'Outside bucket')}</span><span class="pill-state" title="${esc(bucket.reason || 'bucket count')}">ⓘ</span></span></div>`).join('');
    }

    function overviewItemUrl(item, kind) {
      const raw = (item && (item.item || item.issue || item.pr)) || item || {};
      if (raw.url) return raw.url;
      const repo = (item && item.repo) || raw.repo || '';
      if (!repo || !raw.number) return '';
      const base = String(window._githubBaseUrl || 'https://github.com').replace(/\/$/, '');
      return base + '/' + repo + '/' + (kind === 'pr' ? 'pull' : 'issues') + '/' + raw.number;
    }

    // key is a card band by default; by='slice' names a chart slice instead,
    // so a slice's export link returns exactly the rows the slice counts.
    // by='card_band' or 'card_group' names an Overview card band or group the
    // same way.
    function overviewExportURL(kind, key, by) {
      let format = 'csv';
      try {
        const saved = JSON.parse(localStorage.getItem(OVERVIEW_CHARTS_KEY) || '{}') || {};
        if (OVERVIEW_EXPORT_FORMATS.includes(saved.exportFormat)) format = saved.exportFormat;
      } catch {}
      const path = '/api/overview/' + (kind === 'pr' || kind === 'prs' ? 'prs' : 'issues') + '.' + format;
      const params = new URLSearchParams();
      if (key) params.set(['slice', 'card_band', 'card_group'].includes(by) ? by : 'band', key);
      const repos = typeof _overviewLastRepos !== 'undefined' ? _overviewLastRepos : [];
      const selected = overviewSelectedRepoNames(repos);
      const all = overviewAllRepoNames(repos);
      if (all.length && selected.length < all.length) {
        if (selected.length) selected.forEach(repo => params.append('repo', repo));
        else params.set('repo', '__none__');
      }
      // Links use the same token accepted by the API; cookie auth needs no query.
      let token = '';
      try { token = localStorage.getItem('hive-token') || ''; } catch {}
      if (token) params.set('token', token);
      return path + (params.size ? '?' + params.toString() : '');
    }

    async function overviewOpenExport(url) {
      try {
        const resp = await fetch(url, { credentials: 'same-origin', cache: 'no-store' });
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        const fallback = String(url || '').indexOf('.json') !== -1 ? 'hive-overview.json' : 'hive-overview.csv';
        const name = hiveFilenameFromDisposition(resp.headers.get('Content-Disposition') || '', fallback);
        const text = await resp.text();
        hiveExportViewer({title:name,filename:name,text:text,mime:resp.headers.get('Content-Type') || '',sourceURL:url});
      } catch (err) {
        showToast('Overview export failed: ' + (err && err.message ? err.message : err), 'error');
      }
    }

    function overviewChartLabelPoint(percentOffset, percentSize) {
      const midPercent = percentOffset + (percentSize / 2);
      const angle = (midPercent / OVERVIEW_CHART_PERCENT_SCALE) * OVERVIEW_CHART_DEGREES + OVERVIEW_CHART_START_ANGLE;
      const radians = angle * Math.PI / (OVERVIEW_CHART_DEGREES / 2);
      const cos = Math.cos(radians);
      const nearPole = Math.abs(cos) < Math.sin(OVERVIEW_CHART_LABEL_POLE_DEGREES * Math.PI / (OVERVIEW_CHART_DEGREES / 2));
      return {
        x: OVERVIEW_CHART_CENTER + OVERVIEW_CHART_LABEL_RADIUS * cos,
        y: OVERVIEW_CHART_CENTER + OVERVIEW_CHART_LABEL_RADIUS * Math.sin(radians),
        anchor: nearPole ? 'middle' : (cos > 0 ? 'start' : 'end')
      };
    }

    function overviewSliceLabel(count, percentLabel, state) {
      const mode = (state && state.labelMode) || 'both';
      if (mode === 'counts') return String(count);
      if (mode === 'percent') return percentLabel;
      return count + ' · ' + percentLabel;
    }

    function renderOverviewDonut(title, subtitle, slices, history, state) {
      const visible = (slices || []).filter(s => s.count > 0);
      const total = visible.reduce((n, s) => n + Number(s.count || 0), 0);
      if (!total) {
        return `<div class="overview-chart-card"><div><div class="overview-chart-title">${esc(title)}</div><div class="overview-chart-subtitle">${esc(subtitle)}</div><div class="overview-empty">Nothing actionable — the hive is caught up</div></div></div>`;
      }
      let offset = 0;
      const sliceHtml = visible.map(s => {
        const percent = (s.count / total) * OVERVIEW_CHART_PERCENT_SCALE;
        const dashOffset = -offset;
        const point = overviewChartLabelPoint(offset, percent);
        offset += percent;
        const percentLabel = percent.toFixed(OVERVIEW_CHART_DECIMAL_PLACES) + '%';
        const text = percent >= OVERVIEW_CHART_LABEL_MIN_PERCENT
          ? `<text class="overview-chart-label" text-anchor="${point.anchor}" x="${point.x.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)}" y="${point.y.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)}">${esc(overviewSliceLabel(s.count, percentLabel, state))}</text>`
          : '';
        return `<circle class="overview-chart-slice ${esc(s.className)}" cx="${OVERVIEW_CHART_CENTER}" cy="${OVERVIEW_CHART_CENTER}" r="${OVERVIEW_CHART_RADIUS}" pathLength="${OVERVIEW_CHART_FULL}" stroke-width="${OVERVIEW_CHART_STROKE}" stroke-dasharray="${percent.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)} ${OVERVIEW_CHART_FULL}" stroke-dashoffset="${dashOffset.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)}" transform="rotate(${OVERVIEW_CHART_START_ANGLE} ${OVERVIEW_CHART_CENTER} ${OVERVIEW_CHART_CENTER})">${s.card ? `<title>${esc(overviewCardRuleTip(s.label + ' (' + s.count + ')', s.rule))}</title>` : `<title>${esc(s.label)}: ${s.count}${s.rule ? ' — ' + esc(s.rule) : ''}</title>`}</circle>${text}`;
      }).join('');
      const legendHtml = (slices || []).some(s => s && s.card) ? overviewCardLegendHTML(slices, '', state) : visible.map(s => {
        const percent = (s.count / total) * OVERVIEW_CHART_PERCENT_SCALE;
        const percentLabel = percent.toFixed(OVERVIEW_CHART_DECIMAL_PLACES) + '%';
        return `<div class="overview-chart-legend-row" data-action="overviewNavigateBand" data-arg0="" data-arg1="${esc(s.key)}" data-stop="1" title="${esc(s.label + (s.rule ? ': ' + s.rule : ''))}"><span class="overview-chart-swatch ${esc(s.className)}"></span><span class="overview-chart-legend-label">${esc(s.label)}</span><span class="overview-chart-legend-count">${esc(overviewSliceLabel(s.count, percentLabel, state))}</span></div>`;
      }).join('');
      return `<div class="overview-chart-card"><svg class="overview-chart-svg" viewBox="0 0 ${OVERVIEW_CHART_VIEWBOX} ${OVERVIEW_CHART_VIEWBOX}" role="img" aria-label="${esc(title)}"><circle class="overview-chart-track" cx="${OVERVIEW_CHART_CENTER}" cy="${OVERVIEW_CHART_CENTER}" r="${OVERVIEW_CHART_RADIUS}" stroke-width="${OVERVIEW_CHART_STROKE}"></circle>${sliceHtml}<text class="overview-chart-total" x="${OVERVIEW_CHART_CENTER}" y="${OVERVIEW_CHART_CENTER + OVERVIEW_CHART_TOTAL_VALUE_Y_OFFSET}">${total}</text><text class="overview-chart-total-caption" x="${OVERVIEW_CHART_CENTER}" y="${OVERVIEW_CHART_CENTER + OVERVIEW_CHART_TOTAL_CAPTION_Y_OFFSET}">total</text></svg><div><div class="overview-chart-title">${esc(title)}</div><div class="overview-chart-subtitle">${esc(subtitle)}</div><div class="overview-chart-legend">${legendHtml}</div></div></div>`;
    }

    const OVERVIEW_CHARTS_KEY = 'hive-overview-charts';
    const OVERVIEW_CHART_TYPES = ['donut', 'pie', 'bar', 'stacked', 'line', 'histogram'];
    const OVERVIEW_CHART_TYPE_LABELS = { donut: 'Donut', pie: 'Pie', bar: 'Horizontal bar', stacked: '100% stacked bar', line: 'Line / spark', histogram: 'Age histogram' };
    const OVERVIEW_TRANSITIONS = ['fade', 'rise-from-bottom', 'lower-from-top', 'slide-from-left', 'slide-from-right', 'zoom', 'flip', 'blur', 'random'];
    const OVERVIEW_DURATIONS = ['fast', 'normal', 'slow'];
    const OVERVIEW_TRANSITION_DURATION_MS = { fast: 1000, normal: 2000, slow: 3600 };
    const OVERVIEW_CAROUSEL_MIN_MS = 5000;
    const OVERVIEW_CAROUSEL_MAX_MS = 300000;
    const OVERVIEW_CAROUSEL_DEFAULT_MS = 30000;
    const OVERVIEW_HISTORY_MAX_SAMPLES = 288;
    const OVERVIEW_HISTORY_MIN_SAMPLE_MS = 60000;
    const OVERVIEW_REPOS_KEY = 'hive.overview.repos';
    const OVERVIEW_EXPORT_FORMATS = ['csv', 'json'];
    const OVERVIEW_LABEL_MODES = ['both', 'counts', 'percent'];
    const OVERVIEW_BAR_MIN_PERCENT = 1.5;
    const OVERVIEW_STACK_W = 220, OVERVIEW_STACK_H = 98, OVERVIEW_STACK_X = 10, OVERVIEW_STACK_Y = 48, OVERVIEW_STACK_BAR_W = 200, OVERVIEW_STACK_BAR_H = 18, OVERVIEW_STACK_TOTAL_Y = 24;
    const OVERVIEW_LINE_W = 220, OVERVIEW_LINE_H = 150, OVERVIEW_LINE_PAD = 12;
    const OVERVIEW_AGE_BUCKETS = [
      { key: 'lt1d', label: '<1d', maxMinutes: 1440 },
      { key: '1to7d', label: '1-7d', maxMinutes: 10080 },
      { key: '7to14d', label: '7-14d', maxMinutes: 20160 },
      { key: 'gt14d', label: '>14d', maxMinutes: Number.POSITIVE_INFINITY }
    ];
    const _overviewCarouselTimers = [];
    const _overviewHoverState = { issue: false, pr: false };
    const _overviewCarouselState = { issue: 0, pr: 0 };
    const _overviewKPIValues = {};
    let _overviewAnimationDataSignature = '';
    let _overviewLastRepos = [];

    function overviewPrefersReducedMotion() {
      return !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
    }

    function overviewCssDurationMs(name, fallback) {
      const root = document.documentElement;
      const raw = root && window.getComputedStyle ? window.getComputedStyle(root).getPropertyValue(name).trim() : '';
      const match = /^([0-9.]+)(ms|s)$/.exec(raw);
      if (!match) return fallback;
      const value = Number(match[1]);
      if (!Number.isFinite(value)) return fallback;
      return match[2] === 's' ? value * 1000 : value;
    }

    function overviewAnimationSignature(issueSlices, prSlices, selectedNames) {
      const pack = slices => (slices || []).map(s => [s.key, Number(s.count || 0)]);
      return JSON.stringify({ repos: selectedNames || [], issues: pack(issueSlices), prs: pack(prSlices) });
    }

    function overviewShouldAnimateData(signature) {
      const changed = signature !== _overviewAnimationDataSignature;
      _overviewAnimationDataSignature = signature;
      return changed;
    }

    function triggerOverviewAnimations(reason) {
      const el = document.getElementById('overview-card');
      if (!el) return;
      el.classList.remove('overview-animate');
      void el.offsetWidth;
      const reduced = overviewPrefersReducedMotion();
      if (!reduced) el.classList.add('overview-animate');
      animateOverviewKPIs(el, { fromZero: reason === 'expand', reduced });
    }

    const _overviewKPIAnimationFrames = new WeakMap();
    function animateOverviewKPIs(root, options) {
      const opts = options || {};
      const reduced = !!opts.reduced || overviewPrefersReducedMotion();
      const fromZero = !!opts.fromZero;
      const nodes = root ? root.querySelectorAll('[data-overview-kpi-value]') : [];
      nodes.forEach(node => {
        const key = node.getAttribute('data-overview-kpi-key') || '';
        const target = Number(node.getAttribute('data-overview-kpi-value'));
        if (!key || !Number.isFinite(target)) return;
        const kpiFrames = (typeof _overviewKPIAnimationFrames !== 'undefined') ? _overviewKPIAnimationFrames : null;
        const priorFrame = kpiFrames && kpiFrames.get(node);
        if (priorFrame && window.cancelAnimationFrame) window.cancelAnimationFrame(priorFrame);
        if (kpiFrames) kpiFrames.delete(node);
        const display = node.getAttribute('data-overview-kpi-display') || String(target);
        const prior = Number(_overviewKPIValues[key]);
        const start = fromZero || !Number.isFinite(prior) ? 0 : prior;
        _overviewKPIValues[key] = target;
        if (reduced || start === target || !window.requestAnimationFrame) {
          node.textContent = display;
          return;
        }
        const duration = overviewCssDurationMs('--anim-kpi-count', 800);
        const started = performance.now();
        const easeOutCubic = t => 1 - Math.pow(1 - t, 3);
        const tick = now => {
          const t = Math.min(1, (now - started) / duration);
          if (!node.isConnected) { if (kpiFrames) kpiFrames.delete(node); return; }
          node.textContent = String(Math.round(start + (target - start) * easeOutCubic(t)));
          if (t < 1) {
            const nextFrame = window.requestAnimationFrame(tick);
            if (kpiFrames) kpiFrames.set(node, nextFrame);
          } else {
            if (kpiFrames) kpiFrames.delete(node);
            node.textContent = display;
          }
        };
        node.textContent = String(Math.round(start));
        {
          const firstFrame = window.requestAnimationFrame(tick);
          if (kpiFrames) kpiFrames.set(node, firstFrame);
        }
      });
    }

    function overviewDefaultChartState() {
      const enabled = {};
      OVERVIEW_CHART_TYPES.forEach(type => { enabled[type] = type === 'donut'; });
      return { enabled, carousel: false, intervalMs: OVERVIEW_CAROUSEL_DEFAULT_MS, transition: 'fade', duration: 'normal', active: { issue: 'donut', pr: 'donut' }, history: { issue: [], pr: [] }, labelMode: 'both', showKPIs: true, showUsageGauge: true, exportFormat: 'csv' };
    }

    function overviewChartsState() {
      const base = overviewDefaultChartState();
      let saved = {};
      try { saved = JSON.parse(localStorage.getItem(OVERVIEW_CHARTS_KEY) || '{}') || {}; } catch { saved = {}; }
      const enabled = Object.assign({}, base.enabled, saved.enabled || {});
      if (!OVERVIEW_CHART_TYPES.some(type => enabled[type])) enabled.donut = true;
      const active = Object.assign({}, base.active, saved.active || {});
      ['issue', 'pr'].forEach(kind => { if (!OVERVIEW_CHART_TYPES.includes(active[kind])) active[kind] = 'donut'; });
      const intervalMs = Math.min(OVERVIEW_CAROUSEL_MAX_MS, Math.max(OVERVIEW_CAROUSEL_MIN_MS, Number(saved.intervalMs || base.intervalMs)));
      const transition = OVERVIEW_TRANSITIONS.includes(saved.transition) ? saved.transition : base.transition;
      const duration = OVERVIEW_DURATIONS.includes(saved.duration) ? saved.duration : base.duration;
      const history = saved.history && typeof saved.history === 'object' ? saved.history : base.history;
      const labelMode = OVERVIEW_LABEL_MODES.includes(saved.labelMode) ? saved.labelMode : base.labelMode;
      const exportFormat = OVERVIEW_EXPORT_FORMATS.includes(saved.exportFormat) ? saved.exportFormat : base.exportFormat;
      return { enabled, carousel: !!saved.carousel, intervalMs, transition, duration, active, history, labelMode, showKPIs: saved.showKPIs !== false, showUsageGauge: saved.showUsageGauge !== false, exportFormat };
    }

    const OVERVIEW_KPI_WINDOWS = { '24h': 24 * 3600e3, '7d': 7 * 24 * 3600e3 };
    const OVERVIEW_KPI_WINDOW_KEY = 'hive.overviewKPI.window';
    const OVERVIEW_KPI_LOCAL_PREFIX = 'hive.overviewKPI.local.';
    const OVERVIEW_KPI_LOCAL_MAX = 2016;
    const OVERVIEW_KPI_MIN_SAMPLE_MS = 5 * 60 * 1000;
    let _overviewKPIHistory = [];
    let _overviewKPIWindow = (() => {
      try { return localStorage.getItem(OVERVIEW_KPI_WINDOW_KEY) || '24h'; } catch { return '24h'; }
    })();
    let _overviewKPILocalScope = 'all';

    function overviewKPILocalKey() {
      const hive = String(((window._lastStatus || {}).hiveId) || 'default').trim() || 'default';
      return OVERVIEW_KPI_LOCAL_PREFIX + hive + '.' + _overviewKPILocalScope;
    }
    function overviewKPIRepoScope(repos) {
      const selected = (repos || []).map(r => r.full || r.name || '').filter(Boolean).sort();
      const all = (_overviewLastRepos || []).map(r => r.full || r.name || '').filter(Boolean).sort();
      if (!all.length || selected.length === all.length) return 'all';
      return selected.join('|') || 'none';
    }
    function overviewKPILoadLocal() {
      try {
        const parsed = JSON.parse(localStorage.getItem(overviewKPILocalKey()) || '[]');
        return Array.isArray(parsed) ? parsed : [];
      } catch { return []; }
    }
    function overviewKPISaveLocal(entries) {
      try { localStorage.setItem(overviewKPILocalKey(), JSON.stringify((entries || []).slice(-OVERVIEW_KPI_LOCAL_MAX))); } catch {}
    }
    function overviewKPIRecordLocal(sample) {
      if (!sample || !sample.t) return;
      const entries = overviewKPILoadLocal();
      const last = entries[entries.length - 1];
      if (last && Math.abs(Number(last.t || 0) - Number(sample.t || 0)) < OVERVIEW_KPI_MIN_SAMPLE_MS) {
        entries[entries.length - 1] = sample;
      } else {
        entries.push(sample);
      }
      overviewKPISaveLocal(entries);
    }
    async function fetchOverviewKPIHistory() {
      try {
        const since = Date.now() - OVERVIEW_KPI_WINDOWS['7d'];
        const r = await fetch('/api/' + 'overview/history?since=' + encodeURIComponent(String(since)));
        const data = await r.json();
        _overviewKPIHistory = capDashboardArray(data, DASHBOARD_SERVER_HISTORY_CAP);
        if (window._lastStatus && Array.isArray(window._lastStatus.repos)) renderOverviewCharts(window._lastStatus.repos);
      } catch { /* non-critical: local fallback below still records samples */ }
    }
    function setOverviewKPIWindow(win) {
      if (!Object.prototype.hasOwnProperty.call(OVERVIEW_KPI_WINDOWS, win)) return;
      _overviewKPIWindow = win;
      try { localStorage.setItem(OVERVIEW_KPI_WINDOW_KEY, win); } catch {}
      renderOverviewCharts(_overviewLastRepos || []);
    }
    function overviewKPIHistoryEntries(useServer) {
      const cutoff = Date.now() - (OVERVIEW_KPI_WINDOWS[_overviewKPIWindow] || OVERVIEW_KPI_WINDOWS['24h']);
      const byT = new Map();
      const sources = overviewKPILoadLocal().concat(useServer ? (_overviewKPIHistory || []) : []);
      sources.forEach(e => {
        const t = Number(e && e.t);
        if (Number.isFinite(t) && t >= cutoff) byT.set(t, e);
      });
      return Array.from(byT.entries()).sort((a, b) => a[0] - b[0]).map(pair => pair[1]);
    }
    function overviewKPICurrentSample(cards) {
      const sample = { t: Date.now() };
      (cards || []).forEach(card => {
        const value = Number(card.rawValue);
        if (Number.isFinite(value)) sample[card.field] = value;
      });
      return sample;
    }
    function overviewKPISparkTitle(values, fmt) {
      const finite = values.map(Number).filter(Number.isFinite);
      if (!finite.length) return 'min — · max — · latest —';
      const min = Math.min(...finite);
      const max = Math.max(...finite);
      const latest = finite[finite.length - 1];
      return `min ${fmt(min)} · max ${fmt(max)} · latest ${fmt(latest)}`;
    }
    function overviewKPISpark(card, useServer) {
      const entries = overviewKPIHistoryEntries(useServer);
      const values = entries.map(e => Number(e[card.field])).filter(Number.isFinite);
      if (!values.length) return '';
      const title = overviewKPISparkTitle(values, card.sparkFmt || fmtSparkVal);
      return `<span class="overview-kpi-spark">${renderSparkline(null, values, {
        width: 92, height: 24, color: card.color, title, ariaLabel: `${card.label}: ${title}`,
        label: `overview ${card.label}`, id: `overview-kpi:${card.key}`,
        wrapperClass: 'sparkline--flush', wrapperTitle: title,
      })}</span>`;
    }

    function overviewCoverageInfo() {
      const info = window._lastStatus?.overviewCoverage;
      const coverage = Number(info?.coverage);
      if (!Number.isFinite(coverage)) return null;
      const target = Number(info?.target);
      return {
        coverage,
        target: Number.isFinite(target) && target > 0 ? target : 100,
        source: String(info?.source || 'coverage badge'),
        repo: String(info?.repo || '').trim()
      };
    }

    function overviewCoverageState(info) {
      if (!info) return 'danger';
      if (info.coverage >= info.target) return 'success';
      return info.coverage >= info.target - 5 ? 'warn' : 'danger';
    }

    function overviewCoverageColor(state) {
      if (state === 'success') return 'var(--green)';
      if (state === 'warn') return 'var(--yellow)';
      return 'var(--red)';
    }

    function overviewCoverageInfoHTML(info) {
      const source = info?.source || 'coverage badge';
      const repo = info?.repo ? ` for ${info.repo}` : '';
      const tip = `${source}${repo}. Coverage at or above goal is an ACMM Level 3→4 readiness signal.`;
      return `<span class="config-info overview-term-info" tabindex="0" data-action="gh3" data-stop="1" aria-describedby="overview-coverage-tip" aria-label="Test coverage details">ⓘ<span id="overview-coverage-tip" class="config-tooltip" role="tooltip">${esc(tip)}</span></span>`;
    }

    function overviewCoverageBar(info) {
      return `<span class="overview-coverage-bar" aria-hidden="true"><progress class="overview-coverage-progress" value="${esc(String(Math.max(0, info.coverage)))}" max="${esc(String(Math.max(1, info.target)))}"></progress><span class="overview-coverage-bar-caption">${esc(String(info.coverage))}% / ${esc(String(info.target))}%</span></span>`;
    }

    function overviewCoverageCard() {
      const info = overviewCoverageInfo();
      if (!info) return null;
      const state = overviewCoverageState(info);
      const repo = info.repo || 'primary repo';
      return {
        key: 'test-coverage',
        field: 'overviewCoverage',
        label: 'Test coverage',
        value: `${info.coverage}%`,
        rawValue: info.coverage,
        color: overviewCoverageColor(state),
        state,
        tip: `${info.source} for ${repo}. Click to open the quality agent card.`,
        subline: `goal ${esc(String(info.target))}% · ${esc(repo)}`,
        infoHTML: overviewCoverageInfoHTML(info),
        barHTML: overviewCoverageBar(info)
      };
    }

    function renderOverviewCoverageKPI(card, useServerHistory) {
      if (!card) return '';
      const n = Number(card.rawValue);
      const attrs = Number.isFinite(n) ? ` data-overview-kpi-key="${esc(card.key)}" data-overview-kpi-value="${n}" data-overview-kpi-display="${esc(String(card.value ?? '—'))}"` : '';
      const coverageSeries = overviewKPIHistoryEntries(useServerHistory).map(e => Number(e[card.field])).filter(Number.isFinite);
      const spark = coverageSeries.length >= 2 ? overviewKPISpark(card, useServerHistory) : '';
      return `<div class="overview-kpi overview-kpi-coverage" role="button" tabindex="0" data-action="openAgentCollapsedTile" data-keydown-action="openAgentCollapsedTile" data-keys="Enter, " data-prevent="1" data-agent="quality" data-stop="1" data-coverage-state="${esc(card.state)}" aria-label="${esc(card.tip)}"><span class="overview-kpi-value"${attrs}>${esc(String(card.value ?? '—'))}</span><span class="overview-kpi-label">${esc(card.label)}${card.infoHTML || ''}</span><span class="overview-kpi-subline">${card.subline}</span>${spark || card.barHTML || ''}</div>`;
    }

    function overviewKPIWindowControls() {
      return `<div class="gov-pr-models-toggle window-pills-toolbar overview-window-toolbar" aria-label="Overview KPI history range" data-stop="1">${Object.keys(OVERVIEW_KPI_WINDOWS).map(win => `<button type="button" class="${win === _overviewKPIWindow ? 'active' : ''}" data-action="setOverviewKPIWindow" data-arg0="${esc(win)}" data-stop="1" aria-pressed="${win === _overviewKPIWindow ? 'true' : 'false'}" title="Show ${esc(win)} KPI history">${esc(win)}</button>`).join('')}</div>`;
    }

    function saveOverviewChartsState(state) {
      try { localStorage.setItem(OVERVIEW_CHARTS_KEY, JSON.stringify(state)); } catch {}
    }

    function overviewEnabledTypes(state) {
      const enabled = OVERVIEW_CHART_TYPES.filter(type => state.enabled && state.enabled[type]);
      return enabled.length ? enabled : ['donut'];
    }

    // Card bands are sampled under "card:<band>", so the line chart never
    // joins a chart slice's history to a card band of the same key.
    function overviewHistoryCounts(slices) {
      const counts = {};
      (slices || []).forEach(s => { counts[s.card ? 'card:' + s.key : s.key] = s.count || 0; });
      return counts;
    }

    function overviewSampleHistory(kind, slices, state) {
      const now = Date.now();
      const history = Array.isArray(state.history && state.history[kind]) ? state.history[kind].slice() : [];
      const last = history[history.length - 1];
      if (last && now - Number(last.ts || 0) < OVERVIEW_HISTORY_MIN_SAMPLE_MS) return false;
      history.push({ ts: now, counts: overviewHistoryCounts(slices) });
      state.history[kind] = history.slice(-OVERVIEW_HISTORY_MAX_SAMPLES);
      return true;
    }

    function overviewTransitionClass(state) {
      let name = state.transition || 'fade';
      if (name === 'random') {
        const choices = OVERVIEW_TRANSITIONS.filter(t => t !== 'random');
        name = choices[Math.floor(Math.random() * choices.length)] || 'fade';
      }
      return 'overview-transition-' + name;
    }

    function installOverviewTransitionDurationVars() {
      const rootStyle = document.documentElement && document.documentElement.style;
      if (!rootStyle) return;
      OVERVIEW_DURATIONS.forEach(duration => {
        rootStyle.setProperty('--overview-transition-' + duration, overviewTransitionDurationMs({ duration }) + 'ms');
      });
    }

    // A rule in a tooltip reads as one sentence after its label:
    // "Waiting on another item: another issue or PR must finish first."
    function overviewCardRuleTip(label, rule) {
      const text = String(rule || '').trim();
      if (!text) return String(label || '');
      // Lower-case only an ordinary first word ("Another" but not "PR" or
      // "GitHub").
      const lead = /^[A-Z][a-z]*\b/.test(text) && !/^[A-Z][a-z]*[A-Z]/.test(text) ? text.charAt(0).toLowerCase() + text.slice(1) : text;
      return String(label || '') + ': ' + lead;
    }

    // The card legend in plot order, in two sections. The first is the Your
    // turn group (card_group your-turn), headed and totalled with the group's
    // label, "Needs a person": that heading, its bands as plain rows, then a
    // bold subtotal in the group's colour, which takes the place of the
    // group row. Status is every other group, drawn as
    // before (a headed group such as Waiting on someone else, Blocked or Hive
    // has it is one bold subtotal row with its bands indented under it; the
    // other bands are plain rows), then a bold Status subtotal. The groups
    // come from overview_card.groups; a band whose group is not headed stays
    // plain. The split reads only card_group, so it is the same for any hive.
    function overviewCardLegendRows(slices) {
      const todoGroup = 'your-turn';
      const groups = new Map(overviewCardGroups().map(g => [g.key, g]));
      const headed = key => {
        const g = groups.get(key);
        return g ? !!g.headed : ['your-turn', 'waiting-elsewhere', 'blocked', 'actionable-now'].includes(key);
      };
      const sum = list => list.reduce((n, p) => n + Number(p.count || 0), 0);
      const todo = (slices || []).filter(s => s.group === todoGroup);
      const status = (slices || []).filter(s => s.group !== todoGroup);
      const todoLabel = (groups.get(todoGroup) || {}).label || 'Needs a person';
      const rows = [{ type: 'section', label: todoLabel }];
      todo.forEach(s => rows.push({ type: 'plain', slice: s }));
      rows.push({ type: 'subtotal', key: todoGroup, groups: [todoGroup], label: todoLabel, rule: (groups.get(todoGroup) || {}).rule || '', count: sum(todo), className: 'overview-card-group-' + todoGroup });
      rows.push({ type: 'section', label: 'Status', note: 'nothing for a person to do' });
      const done = new Set();
      status.forEach(s => {
        if (!headed(s.group)) { rows.push({ type: 'plain', slice: s }); return; }
        if (done.has(s.group)) return;
        done.add(s.group);
        const parts = status.filter(p => p.group === s.group);
        const g = groups.get(s.group) || {};
        rows.push({ type: 'head', key: s.group, label: g.label || s.group, rule: g.rule || '', count: sum(parts), className: 'overview-card-group-' + s.group });
        parts.forEach(p => rows.push({ type: 'part', slice: p }));
      });
      rows.push({ type: 'subtotal', key: '', groups: [...new Set(status.map(s => s.group).filter(Boolean))], label: 'Status', rule: 'Nothing for a person to do. Someone else or the hive has the next move, or the item is not work.', count: sum(status), className: '' });
      return rows;
    }

    // Every band and group row keeps its download button and its plain-words
    // rule as the tooltip. Zero bands stay listed but dimmed, except the keep
    // bands (Queued for agents, Unexplained), which stay at full strength, and
    // Held, which is hidden at zero; a group or subtotal row never dims.
    // Section headings are role=heading level 4 under the chart title (level
    // 3). The Needs a person subtotal opens and exports the Your turn group,
    // as its group row did; the Status subtotal exports card_group=<every
    // other group> (the export reads a comma list) and opens nothing, since
    // no tile counts exactly those rows. Clicks go through data-action (the
    // dashboard CSP forbids inline handlers).
    function overviewCardLegendHTML(slices, kind, state) {
      const total = (slices || []).reduce((n, s) => n + Number(s.count || 0), 0);
      if (!total) return '<div class="overview-empty">Nothing open here; the hive is caught up</div>';
      const format = ((state && state.exportFormat) || 'csv').toUpperCase();
      return overviewCardLegendRows(slices).map(row => {
        if (row.type === 'section') {
          // The comma is for screen readers: "Status, nothing for a person to do".
          const note = row.note ? `<span class="overview-chart-legend-sr">,</span> <span class="overview-chart-legend-section-note">${esc(row.note)}</span>` : '';
          return `<div class="overview-chart-legend-section" role="heading" aria-level="4">${esc(row.label)}${note}</div>`;
        }
        const head = row.type === 'head';
        const subtotal = row.type === 'subtotal';
        const s = head || subtotal ? row : row.slice;
        const count = Number(s.count || 0);
        if (!head && !subtotal && !count && s.hideZero) return '';
        const percent = (count / total) * OVERVIEW_CHART_PERCENT_SCALE;
        const percentLabel = percent.toFixed(OVERVIEW_CHART_DECIMAL_PLACES) + '%';
        const classes = ['overview-chart-legend-row'];
        if (head) classes.push('overview-chart-legend-group');
        if (subtotal) classes.push('overview-chart-legend-subtotal');
        if (subtotal && !s.key) classes.push('overview-chart-legend-static');
        if (row.type === 'part') classes.push('overview-chart-legend-part');
        if (!head && !subtotal && !count && !s.keep) classes.push('overview-chart-legend-zero');
        const by = head || subtotal ? 'card_group' : 'card_band';
        const exportKey = subtotal ? s.groups.join(',') : s.key;
        const what = head || subtotal ? s.label + ' (all)' : s.label;
        const download = kind && exportKey ? `<button type="button" class="overview-band-download" data-action="overviewOpenExport" data-prevent="1" data-stop="1" data-arg0="${esc(overviewExportURL(kind, exportKey, by))}" title="Export ${esc(what)} as ${format}" aria-label="Export ${esc(what)} as ${format}">⬇</button>` : '';
        const action = s.key ? ` data-action="overviewNavigateBand" data-arg0="${esc(kind || '')}" data-arg1="${esc(s.key)}" data-stop="1"` : '';
        const swatch = s.className ? `<span class="overview-chart-swatch ${esc(s.className)}"></span>` : '<span class="overview-chart-swatch overview-chart-swatch-none" aria-hidden="true"></span>';
        const spoken = subtotal ? '<span class="overview-chart-legend-sr"> total</span>' : '';
        return `<div class="${classes.join(' ')}"${action} title="${esc(overviewCardRuleTip(s.label, s.rule))}">${swatch}<span class="overview-chart-legend-label">${esc(s.label)}</span>${spoken}<span class="overview-chart-legend-count">${esc(overviewSliceLabel(count, percentLabel, state))}</span>${download}</div>`;
      }).join('');
    }

    // Without the card (an older server) the legend below is today's chart
    // slice list, unchanged: it has no card groups, so it is not split into
    // To do and Status.
    function overviewLegendHTML(slices, kind, state) {
      if ((slices || []).some(s => s && s.card)) return overviewCardLegendHTML(slices, kind, state);
      const total = (slices || []).reduce((n, s) => n + Number(s.count || 0), 0);
      const visible = (slices || []).filter(s => s.count > 0);
      if (!total) return '<div class="overview-empty">Nothing actionable — the hive is caught up</div>';
      return visible.map(s => {
        const percent = (s.count / total) * OVERVIEW_CHART_PERCENT_SCALE;
        const percentLabel = percent.toFixed(OVERVIEW_CHART_DECIMAL_PLACES) + '%';
        const format = ((state && state.exportFormat) || 'csv').toUpperCase();
        const download = kind ? `<button type="button" class="overview-band-download" data-action="overviewOpenExport" data-prevent="1" data-stop="1" data-arg0="${esc(overviewExportURL(kind, s.key, 'slice'))}" title="Export ${esc(s.label)} as ${format}" aria-label="Export ${esc(s.label)} as ${format}">⬇</button>` : '';
        return `<div class="overview-chart-legend-row" data-action="overviewNavigateBand" data-arg0="${esc(kind || '')}" data-arg1="${esc(s.key)}" data-stop="1" title="${esc(s.label + (s.rule ? ': ' + s.rule : ''))}"><span class="overview-chart-swatch ${esc(s.className)}"></span><span class="overview-chart-legend-label">${esc(s.label)}</span><span class="overview-chart-legend-count">${esc(overviewSliceLabel(s.count, percentLabel, state))}</span>${download}</div>`;
      }).join('');
    }

    function overviewTransitionOptions(selected) {
      return OVERVIEW_TRANSITIONS.map(t => `<option value="${esc(t)}"${t === selected ? ' selected' : ''}>${esc(t.replace(/-/g, ' '))}</option>`).join('');
    }

    function overviewChartTypeOptions(selected) {
      return OVERVIEW_CHART_TYPES.map(type => `<option value="${esc(type)}"${type === selected ? ' selected' : ''}>${esc(OVERVIEW_CHART_TYPE_LABELS[type])}</option>`).join('');
    }

    function renderOverviewPie(title, subtitle, slices) {
      const visible = (slices || []).filter(s => s.count > 0);
      const total = visible.reduce((n, s) => n + s.count, 0);
      if (!total) return renderOverviewDonut(title, subtitle, slices);
      let offset = 0;
      const radius = OVERVIEW_CHART_RADIUS / 2;
      const strokeWidth = OVERVIEW_CHART_RADIUS;
      const sliceHtml = visible.map(s => {
        const percent = (s.count / total) * OVERVIEW_CHART_PERCENT_SCALE;
        const dashOffset = -offset;
        offset += percent;
        return `<circle class="overview-chart-slice ${esc(s.className)}" cx="${OVERVIEW_CHART_CENTER}" cy="${OVERVIEW_CHART_CENTER}" r="${radius}" pathLength="${OVERVIEW_CHART_FULL}" stroke-width="${strokeWidth}" stroke-dasharray="${percent.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)} ${OVERVIEW_CHART_FULL}" stroke-dashoffset="${dashOffset.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)}" transform="rotate(${OVERVIEW_CHART_START_ANGLE} ${OVERVIEW_CHART_CENTER} ${OVERVIEW_CHART_CENTER})">${s.card ? `<title>${esc(overviewCardRuleTip(s.label + ' (' + s.count + ')', s.rule))}</title>` : `<title>${esc(s.label)}: ${s.count}${s.rule ? ' — ' + esc(s.rule) : ''}</title>`}</circle>`;
      }).join('');
      return `<svg class="overview-chart-svg" viewBox="0 0 ${OVERVIEW_CHART_VIEWBOX} ${OVERVIEW_CHART_VIEWBOX}" role="img" aria-label="${esc(title)}"><circle class="overview-chart-track" cx="${OVERVIEW_CHART_CENTER}" cy="${OVERVIEW_CHART_CENTER}" r="${radius}" stroke-width="${strokeWidth}"></circle>${sliceHtml}<text class="overview-chart-total" x="${OVERVIEW_CHART_CENTER}" y="${OVERVIEW_CHART_CENTER + OVERVIEW_CHART_TOTAL_VALUE_Y_OFFSET}">${total}</text><text class="overview-chart-total-caption" x="${OVERVIEW_CHART_CENTER}" y="${OVERVIEW_CHART_CENTER + OVERVIEW_CHART_TOTAL_CAPTION_Y_OFFSET}">total</text></svg>`;
    }

    // Bars are HTML rows in one grid (label | track | count) so every label
    // shares a column, uses the legend's font size, and can never run into
    // its bar the way SVG text scaled by the viewBox did.
    function overviewHBarRow(label, tooltip, fills, value) {
      const cells = fills.map(f => `<span class="overview-hbar-fill ${esc(f.className)}" style="width:${f.percent.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)}%" title="${esc(f.tooltip)}"></span>`).join('');
      return `<span class="overview-hbar-label" title="${esc(tooltip)}">${esc(label)}</span><span class="overview-hbar-track${fills.length > 1 ? ' overview-hbar-stacked' : ''}">${cells}</span><span class="overview-hbar-value" title="${esc(tooltip)}">${value}</span>`;
    }

    function renderOverviewBar(title, subtitle, slices) {
      // A card band hidden at zero (Held) is hidden here too, as in the legend.
      slices = (slices || []).filter(s => !(s && s.hideZero && !s.count));
      const max = Math.max(1, ...(slices || []).map(s => s.count || 0));
      const rows = (slices || []).map(s => {
        const tooltip = s.card ? overviewCardRuleTip(s.label + ' (' + s.count + ')', s.rule) : s.label + ': ' + s.count + (s.rule ? ' — ' + s.rule : '');
        const percent = s.count ? Math.max(OVERVIEW_BAR_MIN_PERCENT, (s.count / max) * OVERVIEW_CHART_PERCENT_SCALE) : 0;
        return overviewHBarRow(s.label, tooltip, [{ className: s.className, percent, tooltip }], s.count);
      }).join('');
      return `<div class="overview-hbar" role="img" aria-label="${esc(title)}">${rows}</div>`;
    }

    function renderOverviewStacked(title, subtitle, slices) {
      const visible = (slices || []).filter(s => s.count > 0);
      const total = visible.reduce((n, s) => n + s.count, 0);
      if (!total) return renderOverviewBar(title, subtitle, slices);
      let x = OVERVIEW_STACK_X;
      const bars = visible.map((s, idx) => {
        const w = (s.count / total) * OVERVIEW_STACK_BAR_W;
        const out = `<rect class="overview-chart-fill overview-stack-segment overview-stagger-${idx} ${esc(s.className)}" x="${x.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)}" y="${OVERVIEW_STACK_Y}" width="${w.toFixed(OVERVIEW_CHART_DECIMAL_PLACES)}" height="${OVERVIEW_STACK_BAR_H}" rx="3">${s.card ? `<title>${esc(overviewCardRuleTip(s.label + ' (' + s.count + ')', s.rule))}</title>` : `<title>${esc(s.label)}: ${s.count}${s.rule ? ' — ' + esc(s.rule) : ''}</title>`}</rect>`;
        x += w;
        return out;
      }).join('');
      return `<svg class="overview-chart-svg" viewBox="0 0 ${OVERVIEW_STACK_W} ${OVERVIEW_STACK_H}" role="img" aria-label="${esc(title)}"><rect class="overview-chart-track" x="${OVERVIEW_STACK_X}" y="${OVERVIEW_STACK_Y}" width="${OVERVIEW_STACK_BAR_W}" height="${OVERVIEW_STACK_BAR_H}" rx="3"></rect>${bars}<text class="overview-chart-total" x="${OVERVIEW_STACK_W / 2}" y="${OVERVIEW_STACK_TOTAL_Y}">${total} total</text></svg>`;
    }

    function renderOverviewLine(title, subtitle, slices, history) {
      // With the card, only card samples are drawn and each band reads its
      // own "card:<band>" history key; Held at zero is hidden.
      const card = (slices || []).some(s => s && s.card);
      const hk = s => (s.card ? 'card:' + s.key : s.key);
      if (card) slices = slices.filter(s => !(s.hideZero && !s.count));
      const samples = (Array.isArray(history) ? history : []).filter(sample => !card || Object.keys((sample && sample.counts) || {}).some(k => k.startsWith('card:')));
      if (samples.length < 2) return renderOverviewBar(title, subtitle, slices);
      const max = Math.max(1, ...samples.flatMap(sample => (slices || []).map(s => Number((sample.counts || {})[hk(s)] || 0))));
      const denom = Math.max(1, samples.length - 1);
      const lines = (slices || []).map((s, idx) => {
        const points = samples.map((sample, idx) => {
          // A card band a sample never recorded (taken before the band
          // existed) is a gap, not a 0.
          if (s.card && !Object.prototype.hasOwnProperty.call(sample.counts || {}, hk(s))) return '';
          const x = OVERVIEW_LINE_PAD + idx * ((OVERVIEW_LINE_W - OVERVIEW_LINE_PAD * 2) / denom);
          const y = OVERVIEW_LINE_H - OVERVIEW_LINE_PAD - (Number((sample.counts || {})[hk(s)] || 0) / max) * (OVERVIEW_LINE_H - OVERVIEW_LINE_PAD * 2);
          return x.toFixed(OVERVIEW_CHART_DECIMAL_PLACES) + ',' + y.toFixed(OVERVIEW_CHART_DECIMAL_PLACES);
        }).filter(Boolean).join(' ');
        return `<polyline class="overview-chart-line overview-line-draw overview-stagger-${idx} ${esc(s.className)}" pathLength="100" points="${points}"><title>${esc(s.card ? overviewCardRuleTip(s.label, s.rule) : s.label + (s.rule ? ' — ' + s.rule : ''))}</title></polyline>`;
      }).join('');
      return `<svg class="overview-chart-svg" viewBox="0 0 ${OVERVIEW_LINE_W} ${OVERVIEW_LINE_H}" role="img" aria-label="${esc(title)}">${lines}</svg>`;
    }

    function overviewItemAgeMinutes(item) {
      const explicit = Number(item && (item.age_minutes ?? item.ageMinutes));
      if (Number.isFinite(explicit) && explicit >= 0) return explicit;
      const value = (item && (item.updated_at || item.created_at)) || '';
      const t = Date.parse(value);
      return Number.isFinite(t) ? Math.max(0, (Date.now() - t) / 60000) : 0;
    }

    function renderOverviewHistogram(title, subtitle, slices) {
      const rows = OVERVIEW_AGE_BUCKETS.map((bucket, idx) => {
        const bySlice = (slices || []).map(s => ({ slice: s, count: (s.items || []).filter(item => overviewItemAgeMinutes(item) <= bucket.maxMinutes && (idx === 0 || overviewItemAgeMinutes(item) > OVERVIEW_AGE_BUCKETS[idx - 1].maxMinutes)).length }));
        const total = bySlice.reduce((n, x) => n + x.count, 0);
        return { bucket, bySlice, total };
      });
      const max = Math.max(1, ...rows.map(r => r.total));
      const htmlRows = rows.map(row => {
        const fills = row.bySlice.filter(x => x.count > 0).map(xr => ({
          className: xr.slice.className,
          percent: (xr.count / max) * OVERVIEW_CHART_PERCENT_SCALE,
          tooltip: xr.slice.card ? overviewCardRuleTip(row.bucket.label + ' · ' + xr.slice.label + ' (' + xr.count + ')', xr.slice.rule) : row.bucket.label + ' · ' + xr.slice.label + ': ' + xr.count + (xr.slice.rule ? ' — ' + xr.slice.rule : '')
        }));
        return overviewHBarRow(row.bucket.label, row.bucket.label + ': ' + row.total, fills, row.total);
      }).join('');
      return `<div class="overview-hbar" role="img" aria-label="${esc(title)}">${htmlRows}</div>`;
    }

    function renderOverviewDonutSVG(title, subtitle, slices, history, state) {
      const html = renderOverviewDonut(title, subtitle, slices, history, state);
      const start = html.indexOf('<svg');
      const end = html.indexOf('</svg>');
      return start >= 0 && end >= start ? html.slice(start, end + '</svg>'.length) : '<div class="overview-empty">Nothing actionable — the hive is caught up</div>';
    }

    const overviewChartRenderers = {
      donut: renderOverviewDonutSVG,
      pie: renderOverviewPie,
      bar: renderOverviewBar,
      stacked: renderOverviewStacked,
      line: renderOverviewLine,
      histogram: renderOverviewHistogram
    };

    function renderOverviewChartBody(type, title, subtitle, slices, kind, state) {
      const renderer = overviewChartRenderers[type] || overviewChartRenderers.donut;
      return renderer(title, subtitle, slices, kind === 'issue' ? state.history.issue : state.history.pr, state);
    }

    // One dot per chart type (the ◀ ▶ domain); ticked types are the ones auto-play rotates through.
    function renderOverviewDots(enabled, active) {
      const ticked = new Set(enabled || []);
      return `<span class="overview-carousel-dots" aria-label="Overview carousel position">${OVERVIEW_CHART_TYPES.map(type => {
        const cls = 'overview-carousel-dot' + (type === active ? ' active' : '') + (ticked.has(type) ? ' ticked' : '');
        const hint = ticked.has(type) ? ' — in auto-play rotation' : ' — not in auto-play (tick it in ⚙️)';
        return `<span class="${cls}" title="${esc(OVERVIEW_CHART_TYPE_LABELS[type] + hint)}"></span>`;
      }).join('')}</span>`;
    }

    function renderOverviewPanel(kind, title, subtitle, slices, state) {
      const enabled = overviewEnabledTypes(state);
      const active = enabled.includes(state.active[kind]) ? state.active[kind] : enabled[0];
      state.active[kind] = active;
      const panelClass = `overview-panel-in ${overviewTransitionClass(state)} overview-duration-${state.duration}`;
      const body = renderOverviewChartBody(active, title, subtitle, slices, kind, state);
      const total = (slices || []).reduce((n, s) => n + s.count, 0);
      const csvKind = kind === 'pr' ? 'prs' : 'issues';
      const format = (state.exportFormat || 'csv').toUpperCase();
      const csvTitle = total ? ('Export ' + (kind === 'pr' ? 'PRs' : 'issues') + ' as ' + format) : 'No matching items to export';
      const download = `<button type="button" class="hv-btn btn-icon btn-sm overview-export-csv overview-chart-download" data-action="overviewOpenExport" data-prevent="1" data-stop="1" data-arg0="${esc(overviewExportURL(csvKind))}" title="${esc(csvTitle)}" aria-label="${esc(csvTitle)}">⬇︎</button>`;
      return `<div class="overview-chart-card ${panelClass}" data-mouseover-action="overviewPanelHover" data-mouseout-action="overviewPanelUnhover" data-arg0="${kind}">${download}${body}<div class="overview-chart-meta"><div class="overview-chart-title" role="heading" aria-level="3">${esc(title)}</div><div class="overview-chart-subtitle">${esc(subtitle)}</div><div class="overview-chart-legend">${overviewLegendHTML(slices, csvKind, state)}</div></div></div>`;
    }

    function overviewMedianAgeLabel(slices, state) {
      const ages = (slices || []).flatMap(s => (s.items || []).map(item => {
        const raw = (item && (item.item || item.issue || item.pr)) || item || {};
        const basis = state.timeBasis === 'created' ? (raw.created_at || raw.updated_at) : (raw.updated_at || raw.created_at);
        const t = Date.parse(basis || '');
        return Number.isFinite(t) ? Math.max(0, (Date.now() - t) / 60000) : overviewItemAgeMinutes(item);
      })).filter(n => Number.isFinite(n)).sort((a, b) => a - b);
      if (!ages.length) return '—';
      const median = ages[Math.floor(ages.length / 2)];
      if (median < 60) return Math.round(median) + 'm';
      if (median < 1440) return Math.round(median / 60) + 'h';
      return Math.round(median / 1440) + 'd';
    }

    function overviewRepoNumber(repo, fields) {
      for (const field of fields) {
        if (!repo || !Object.prototype.hasOwnProperty.call(repo, field)) continue;
        const raw = repo[field];
        if (Array.isArray(raw)) continue;
        const n = Number(raw);
        if (Number.isFinite(n)) return n;
      }
      return null;
    }

    function overviewRepoOpenIssueCount(repo) {
      return overviewRepoNumber(repo, ['openIssues', 'open_issues', 'issues']);
    }

    function overviewRepoOpenPRCount(repo) {
      return overviewRepoNumber(repo, ['openPRs', 'open_prs', 'prs']);
    }

    const OVERVIEW_ISSUE_BREAKDOWN_LABELS = {
      needs_human: 'needs-human',
      needs_decision: 'needs human decision',
      needs_spec: 'needs-spec',
      reporter_confirmation: 'waiting on reporter',
      exempt: 'exempt',
      filtered: 'filtered',
      reporter_triage: 'reporter triage',
      hive_advisory: 'hive advisory',
      dependency_dashboard: 'dependency dashboard',
      other: 'other'
    };
    const OVERVIEW_PR_BREAKDOWN_LABELS = {
      hold: 'held',
      draft: 'draft',
      filtered: 'filtered',
      other: 'other'
    };

    function overviewBreakdownTotal(bd, keys) {
      return (keys || []).reduce((n, key) => n + Number((bd || {})[key] || 0), 0);
    }

    function overviewRepoForgeTotals(repos) {
      const totals = {
        issues: { forge: 0, tracked: 0, held: 0, outside: 0, breakdown: {} },
        prs: { forge: 0, tracked: 0, held: 0, outside: 0, breakdown: {} }
      };
      (repos || []).forEach(r => {
        const ib = r.workBreakdown?.issues || {};
        const pb = r.workBreakdown?.prs || {};
        const trackedIssues = (r.actionableIssues || []).length + (r.heldIssues || []).length;
        const trackedPRs = (r.openPrs || []).length + (r.heldPrs || []).length;
        const rawIssues = overviewRepoOpenIssueCount(r);
        const rawPRs = overviewRepoOpenPRCount(r);
        const issueForge = Object.keys(ib).length ? ['actionable', 'hold', 'hive_advisory', 'dependency_dashboard', 'filtered', 'reporter_triage', 'other'].reduce((n, key) => n + Number(ib[key] || 0), 0) : (rawIssues ?? trackedIssues);
        const prForge = Object.keys(pb).length ? ['actionable', 'hold', 'draft', 'filtered', 'other'].reduce((n, key) => n + Number(pb[key] || 0), 0) : (rawPRs ?? trackedPRs);
        totals.issues.forge += issueForge;
        totals.issues.tracked += trackedIssues;
        totals.issues.held += (r.heldIssues || []).length;
        totals.prs.forge += prForge;
        totals.prs.tracked += trackedPRs;
        totals.prs.held += (r.heldPrs || []).length;
        Object.keys(OVERVIEW_ISSUE_BREAKDOWN_LABELS).forEach(key => {
          let v = Number(ib[key] || 0);
          if (key === 'filtered') {
            v = Math.max(0, Number(ib.filtered || 0) - overviewBreakdownTotal(ib, ['needs_human', 'needs_decision', 'needs_spec', 'reporter_confirmation', 'exempt']));
          }
          if (v > 0) totals.issues.breakdown[key] = (totals.issues.breakdown[key] || 0) + v;
        });
        Object.keys(OVERVIEW_PR_BREAKDOWN_LABELS).forEach(key => {
          const v = Number(pb[key] || 0);
          if (v > 0) totals.prs.breakdown[key] = (totals.prs.breakdown[key] || 0) + v;
        });
      });
      totals.issues.outside = Math.max(0, totals.issues.forge - totals.issues.tracked);
      totals.prs.outside = Math.max(0, totals.prs.forge - totals.prs.tracked);
      return totals;
    }

    function overviewKPIForgeTotals(repos) {
      if (_overviewKPILocalScope === 'all' && window._lastStatus?.overviewTotals?.issues && window._lastStatus?.overviewTotals?.prs) {
        return window._lastStatus.overviewTotals;
      }
      if (!(repos || []).some(r => r && (r.workBreakdown || overviewRepoOpenIssueCount(r) !== null || overviewRepoOpenPRCount(r) !== null))) {
        return null;
      }
      return overviewRepoForgeTotals(repos);
    }

    function overviewKPIBreakdownSubline(total, labels) {
      const parts = Object.entries(labels || {}).map(([key, label]) => {
        const n = Number(total?.breakdown?.[key] || 0);
        return n > 0 ? `${n} ${label}` : '';
      }).filter(Boolean);
      const outside = Number(total?.outside || 0);
      const held = Number(total?.held || 0);
      const tracked = Number(total?.tracked || 0);
      const bits = [`${tracked} in Overview bands`];
      if (held > 0) bits.push(`${held} held`);
      if (outside > 0) bits.push(`${outside} outside${parts.length ? ': ' + parts.join(' · ') : ''}`);
      return bits.join(' · ');
    }

    function overviewKPITerm(equation, key, fallback) {
      const term = (equation?.terms || []).find(t => t && t.key === key);
      const n = Number(term?.count);
      return Number.isFinite(n) ? n : fallback;
    }

    function overviewPartitionDocsLink(row) {
      const href = row && row.docsHref;
      return href ? `<a href="${esc(dashboardDocsHrefFromPath(href))}" target="_blank" rel="noopener" data-stop="1">Learn more ↗</a>` : '';
    }

    function overviewPartitionSettingsButton(row) {
      const tab = row && row.settingsTab;
      // Not a <button>: this tooltip is rendered inside the <button class="overview-kpi"> card, and the HTML
      // parser force-closes an open <button> when it meets a nested one, spilling the tooltip inline.
      return tab ? `<span role="button" tabindex="0" class="hv-btn btn-secondary btn-sm" data-action="openConfigDialog" data-keydown-action="openConfigDialog" data-keys="Enter, " data-prevent="1" data-arg0="governor" data-arg2="${esc(tab)}" data-stop="1">Settings → ${esc(tab)}</span>` : '';
    }

    function overviewPartitionSummaryLine(term) {
      switch (term?.key) {
        case 'outside':
        case 'other':
          return 'Outside: items Hive is configured to leave alone.';
        case 'held':
          return 'Held: work intentionally paused by a hold label.';
        case 'your_turn':
          return 'Your turn: Hive is stuck on a person for these items.';
        case 'waiting_elsewhere':
          return 'Waiting on someone else: gated on a person\'s hold, a pause, the adjudicator, a reporter or an open PR.';
        case 'blocked':
          return 'Blocked: pull requests with a blocked verdict, merge conflicts or failing CI.';
        case 'actionable':
          return 'Hive has it: the hive moves these on its own.';
        case 'waiting-on-reporter':
          return 'Waiting on reporter: issues labeled needs-reporter-confirmation (search label:needs-reporter-confirmation).';
        default:
          return `${term?.label || term?.key || 'Bucket'}: items in this bucket.`;
      }
    }

    function overviewPartitionGloss(row) {
      switch (row?.key) {
        case 'needs-decision': return 'needs decision';
        case 'needs-spec': return 'needs spec';
        case 'waiting-on-reporter': return 'waiting on reporter';
        case 'exempt-labels':
        case 'exempt-pr-labels': return 'exempt label match';
        case 'reporter-triage': return 'untrusted reporter';
        case 'project-issue-filter': return 'missing required label';
        case 'standing-meta-advisory': return 'standing advisory';
        case 'dependency-dashboard': return 'dependency control panel';
        case 'draft-prs': return 'draft until ready';
        case 'issue-hold-labels':
        case 'pr-hold-labels': return 'hold label';
        case 'pr-blocked-band': return 'blocked PR gate';
        default: return 'fixed bucket';
      }
    }

    function overviewPartitionPreferredDocs(term, rows) {
      const found = (rows || []).find(row => row && row.docsHref);
      if (found) return found;
      if (term?.docsHref) return term;
      return null;
    }

    function overviewPartitionPreferredSettings(rows) {
      return (rows || []).find(row => row && row.settingsTab) || null;
    }

    function overviewPartitionRowHTML(row) {
      const hasCount = row && Object.prototype.hasOwnProperty.call(row, 'count') && Number.isFinite(Number(row.count));
      const value = hasCount ? esc(String(Number(row.count))) : esc(overviewPartitionGloss(row));
      const zero = hasCount && Number(row.count) === 0 ? ' muted' : '';
      if (row && row.card) {
        // An Overview card row: its band's swatch, and its rule on hover. A
        // keep band at zero stays at full strength, as in the legend.
        const dim = hasCount && Number(row.count) === 0 && !row.keep ? ' muted' : '';
        const title = row.rule ? ` title="${esc(row.rule)}"` : '';
        const swatch = row.swatch ? `<span class="overview-chart-swatch ${esc(row.swatch)}"></span>` : '';
        return `<div class="overview-partition-row${dim}"${title}><span class="overview-partition-label">${swatch}${esc(row.label || row.key || 'other')}</span><span class="overview-partition-count">${value}</span></div>`;
      }
      return `<div class="overview-partition-row${zero}"><span class="overview-partition-label">${esc(row?.label || row?.key || 'other')}</span><span class="overview-partition-count">${value}</span></div>`;
    }

    function overviewPartitionTooltip(term, context) {
      if (term && term.card) {
        // An Overview card tile: its name and count, its rule as the summary
        // (Your turn adds the not-in-the-inbox sentence), then its bands.
        const summary = [term.rule || overviewPartitionSummaryLine(term), term.note || ''].filter(Boolean).join(' ');
        const rows = (Array.isArray(term.breakdown) ? term.breakdown : []).map(overviewPartitionRowHTML).join('');
        const links = Array.isArray(term.links) ? term.links : [];
        const footerParts = [overviewPartitionSettingsButton(overviewPartitionPreferredSettings(links)), overviewPartitionDocsLink(overviewPartitionPreferredDocs(term, links))].filter(Boolean).join('');
        const footer = footerParts ? `<span class="overview-partition-footer">${footerParts}</span>` : '';
        return `<span id="${esc(context)}-tip" class="config-tooltip overview-partition-tooltip" role="tooltip"><span class="overview-partition-title">${esc((term.label || term.key) + ' · ' + Number(term.count || 0))}</span><span class="overview-partition-summary">${esc(summary)}</span>${rows}${footer}</span>`;
      }
      const MAX_TOOLTIP_ROWS = 8;
      const rows = Array.isArray(term?.breakdown) ? term.breakdown : [];
      const orderedRows = rows.slice().sort((a, b) => {
        const ac = Number(a?.count || 0) > 0 ? 0 : 1;
        const bc = Number(b?.count || 0) > 0 ? 0 : 1;
        return ac - bc;
      });
      const visibleRows = orderedRows.slice(0, MAX_TOOLTIP_ROWS);
      const hiddenCount = Math.max(0, orderedRows.length - visibleRows.length);
      const body = visibleRows.length
        ? visibleRows.map(overviewPartitionRowHTML).join('')
        : overviewPartitionRowHTML({ label: term?.label || term?.key || 'items', count: term?.count || 0 });
      const docsRow = overviewPartitionPreferredDocs(term, rows);
      const settingsRow = overviewPartitionPreferredSettings(rows);
      const fixed = rows.some(row => row && !row.settingsTab) ? '<span class="text-muted">fixed buckets</span>' : '';
      const more = hiddenCount > 0 && docsRow?.docsHref
        ? `<a class="overview-partition-more" href="${esc(dashboardDocsHrefFromPath(docsRow.docsHref))}" target="_blank" rel="noopener" data-stop="1">+${esc(String(hiddenCount))} more</a>`
        : (hiddenCount > 0 ? `<span class="overview-partition-more text-muted">+${esc(String(hiddenCount))} more</span>` : '');
      const footerParts = [overviewPartitionSettingsButton(settingsRow), overviewPartitionDocsLink(docsRow), fixed].filter(Boolean).join('');
      const footer = footerParts ? `<span class="overview-partition-footer">${footerParts}</span>` : '';
      return `<span id="${esc(context)}-tip" class="config-tooltip overview-partition-tooltip" role="tooltip"><span class="overview-partition-summary">${esc(overviewPartitionSummaryLine(term))}</span>${body}${more}${footer}</span>`;
    }

    function overviewPartitionInfo(term, context) {
      if (!term || (!term.rule && !(term.breakdown || []).length)) return '';
      return `<span class="config-info overview-term-info" tabindex="0" data-action="gh3" data-stop="1" aria-describedby="${esc(context)}-tip" aria-label="${esc((term.label || term.key) + ' details')}">ⓘ${overviewPartitionTooltip(term, context)}</span>`;
    }

    function overviewActionableTermHTML(term, context) {
      const text = `${Number(term?.count || 0)} ${term?.label || ''}`;
      if (!term || !['outside', 'held', 'blocked', 'your_turn', 'waiting_elsewhere'].includes(term.key)) return esc(text);
      return `<span class="overview-equation-term overview-equation-term-${esc(term.key)}">${esc(text)}${overviewPartitionInfo(term, context)}</span>`;
    }

    function renderActionableEquationSubline(equation) {
      if (!equation || !Array.isArray(equation.terms)) return esc(equation?.text || '');
      const contextBase = `overview-partition-${esc(equation.kind || 'combined')}`;
      if (equation.open !== undefined && equation.kind) {
        const terms = equation.terms.filter(t => Number(t?.count || 0) > 0).map((t, i) => `− ${overviewActionableTermHTML(t, contextBase + '-' + esc(t.key || i))}`).join(' ');
        return `${esc(String(Number(equation.open || 0)))} open${terms ? ' ' + terms : ''} = ${esc(String(Number(equation.result || 0)))}`;
      }
      const terms = equation.terms.filter(t => Number(t?.count || 0) > 0).map((t, i) => overviewActionableTermHTML(t, contextBase + '-' + esc(t.key || i))).join(' + ');
      const openIssues = Number(equation.openIssues ?? 0);
      const openPRs = Number(equation.openPrs ?? equation.openPRs ?? 0);
      return `${esc(String(openIssues))} issues + ${esc(String(openPRs))} PRs = ${terms || '0 actionable'}`;
    }

    function overviewEquationTermCount(equation, key) {
      const term = (equation?.terms || []).find(t => t && t.key === key);
      const n = Number(term?.count || 0);
      return Number.isFinite(n) ? n : 0;
    }

    function overviewOutsideBreakdownText(equation) {
      const outside = (equation?.terms || []).find(t => t && t.key === 'outside');
      const rows = Array.isArray(outside?.breakdown) ? outside.breakdown : [];
      if (!rows.length) return '';
      return rows.map(row => `${Number(row?.count || 0)} ${row?.label || row?.key || 'other'}`).join(' · ');
    }

    // overviewStaleSince is the earliest countsStaleSince over the selected
    // repos, or the server's staleSince when every repo is selected.
    function overviewStaleSince(repos) {
      if (_overviewKPILocalScope === 'all' && window._lastStatus?.staleSince) return window._lastStatus.staleSince;
      let earliest = '';
      (repos || []).forEach(r => {
        if (!r || !r.countsStale || !r.countsStaleSince) return;
        if (!earliest || r.countsStaleSince < earliest) earliest = r.countsStaleSince;
      });
      return earliest;
    }
    /* Pure: the post-restart window the server reports while it still
       serves the snapshot restored from disk (status.restartRefresh,
       status_builder.go restartRefreshInfo). Null outside that window. */
    function overviewRestartRefresh() {
      if (_overviewKPILocalScope !== 'all') return null;
      const rr = window._lastStatus?.restartRefresh;
      return rr && rr.restoredAt ? rr : null;
    }
    /* Pure: the stale badge's text and tooltip. During the post-restart
       window the figures are not "stale since <previous pod's time>" — this
       process restarted and its first full scan is still running — so the
       badge says so with the process's own clock and the scan progress. */
    function overviewStaleBadgeText(staleSince, rr) {
      if (rr) {
        const ago = relativeTimeShort(rr.restoredAt);
        const total = Number(rr.reposTotal || 0);
        const scanned = Math.min(Number(rr.reposScanned || 0), total);
        const snapAt = rr.snapshotAt || staleSince;
        const text = `refreshing after restart${ago ? ` (${ago})` : ''}${total > 0 ? ` · ${scanned}/${total} repos scanned` : ''}${snapAt ? ` · showing snapshot from ${fmtOverviewStaleSince(snapAt)}` : ''}`;
        const title = `Hive restarted${ago ? ` ${ago}` : ''} and its first full scan of every repo has not finished yet${total > 0 ? ` (${scanned} of ${total} repos read so far)` : ''}. Until it does, these figures are the snapshot the previous process saved${snapAt ? ` at ${fmtOverviewStaleSince(snapAt)}` : ''} plus webhook updates since; the scan clears this.`;
        return { icon: '⟳', text, title };
      }
      const ago = relativeTimeShort(staleSince);
      return {
        icon: '⚠',
        text: `stale since ${fmtOverviewStaleSince(staleSince)}${ago ? ` (${ago})` : ''} · serving last-good`,
        title: `GitHub refused the latest enumeration or count reconcile (rate limit or outage). These figures are the last successful read plus webhook updates since, as of ${fmtOverviewStaleSince(staleSince)}; the next enumeration or hourly reconcile clears this.`
      };
    }
    function fmtOverviewStaleSince(ts) {
      if (typeof formatDashboardDateTime === 'function') return formatDashboardDateTime(ts, undefined, String(ts || ''));
      const t = new Date(ts || '').getTime();
      return Number.isFinite(t) ? new Date(t).toISOString() : String(ts || '');
    }

    // With the Overview card (card = { kind, counts by term }) the total
    // tiles read "54 needs a person · 2 waiting · 7 blocked · 2 hive has it ·
    // 4 outside": blocked on PRs only, held only above zero.
    function renderOverviewTotalPartitionSubline(equation, card) {
      if (card && card.counts) {
        const n = key => Number(card.counts[key] || 0);
        const parts = [[n('your_turn'), 'needs a person'], [n('waiting_elsewhere'), 'waiting']];
        if (n('held') > 0) parts.push([n('held'), 'held']);
        if (card.kind === 'prs') parts.push([n('blocked'), 'blocked']);
        parts.push([n('actionable'), 'hive has it'], [n('outside'), 'outside']);
        return parts.map(([count, label]) => `${esc(String(count))} ${esc(label)}`).join(' · ');
      }
      if (!equation) return '';
      const count = key => overviewEquationTermCount(equation, key);
      const parts = [`${esc(String(Number(equation.result || 0)))} actionable`];
      [['your_turn', 'your turn'], ['waiting_elsewhere', 'waiting on someone else'], ['held', 'held'], ['blocked', 'blocked']].forEach(([key, label]) => {
        if (count(key) > 0) parts.push(`${esc(String(count(key)))} ${esc(label)}`);
      });
      const outside = count('outside') + count('other');
      const detail = overviewOutsideBreakdownText(equation);
      parts.push(`${esc(String(outside))} outside${detail ? ': ' + esc(detail) : ''}`);
      return parts.join(' + ');
    }

    function renderOverviewSplitSubline(total, issueCount, prCount, label) {
      return `${esc(String(Number(total || 0)))} = ${esc(String(Number(issueCount || 0)))} issues + ${esc(String(Number(prCount || 0)))} PRs`;
    }

    // Equation terms in the order the first-match rule applies; actionable is
    // what is left.
    function overviewEquationTermDefs() {
      return [
        { key: 'your_turn', label: 'Your turn' },
        { key: 'waiting_elsewhere', label: 'Waiting on someone else' },
        { key: 'held', label: 'held' },
        { key: 'blocked', label: 'blocked' },
        { key: 'outside', label: 'outside' }
      ];
    }

    function overviewKPIKindEquation(kind, open, actionable, terms) {
      const kept = (terms || []).filter(t => Number(t?.count || 0) > 0);
      const sum = kept.reduce((n, t) => n + Number(t.count || 0), Number(actionable || 0));
      if (Number(open || 0) > sum) kept.push({ key: 'other', label: 'other', count: Number(open || 0) - sum });
      const middle = kept.map(t => `− ${Number(t.count || 0)} ${t.label}`).join(' ');
      const text = `${Number(open || 0)} open${middle ? ' ' + middle : ''} = ${Number(actionable || 0)}`;
      return { kind, open, result: actionable, terms: kept, text, title: text };
    }

    function overviewKPICombinedEquation(issueEq, prEq) {
      const terms = overviewEquationTermDefs().map(def => ({ key: def.key, label: def.label, count: overviewEquationTermCount(issueEq, def.key) + overviewEquationTermCount(prEq, def.key) }));
      const result = Number(issueEq?.result || 0) + Number(prEq?.result || 0);
      terms.push({ key: 'actionable', label: 'actionable', count: result });
      const other = overviewEquationTermCount(issueEq, 'other') + overviewEquationTermCount(prEq, 'other');
      if (other > 0) terms.push({ key: 'other', label: 'other', count: other });
      const openIssues = Number(issueEq?.open || 0);
      const openPRs = Number(prEq?.open || 0);
      const text = `${openIssues} issues + ${openPRs} PRs = ` + terms.filter(t => Number(t.count || 0) > 0).map(t => `${Number(t.count || 0)} ${t.label}`).join(' + ');
      return { openIssues, openPrs: openPRs, totalOpen: openIssues + openPRs, result, terms, text, title: text };
    }

    // The same partition for the repos selected in the Overview, counted from
    // the server-stamped overview_term of each row. Outside is what no other
    // term claims.
    function overviewKPILocalEquations(repos, forgeTotals) {
      const counts = { issues: {}, prs: {} };
      const bump = (kind, row) => { const term = String((row && row.overview_term) || ''); if (term) counts[kind][term] = (counts[kind][term] || 0) + 1; };
      let listedIssues = 0;
      let listedPRs = 0;
      (repos || []).forEach(r => {
        (r.actionableIssues || []).concat(r.heldIssues || [], r.nonActionableIssues || []).forEach(row => { listedIssues++; bump('issues', row); });
        (r.openPrs || []).concat(r.heldPrs || []).forEach(row => { listedPRs++; bump('prs', row); });
      });
      const build = (kind, open, c, listed) => {
        const terms = overviewEquationTermDefs().filter(def => def.key !== 'outside' && (kind === 'PRs' || def.key !== 'blocked')).map(def => ({ key: def.key, label: def.label, count: c[def.key] || 0 }));
        const actionable = c.actionable || 0;
        const named = terms.reduce((n, t) => n + t.count, actionable);
        terms.push({ key: 'outside', label: 'outside', count: Math.max(0, Math.max(open, listed) - named) });
        return overviewKPIKindEquation(kind, Math.max(open, named), actionable, terms);
      };
      const issueEq = build('issues', Number(forgeTotals?.issues?.forge ?? listedIssues), counts.issues, listedIssues);
      const prEq = build('PRs', Number(forgeTotals?.prs?.forge ?? listedPRs), counts.prs, listedPRs);
      return { equation: overviewKPICombinedEquation(issueEq, prEq), issueEq, prEq };
    }

    // The governor card's equations when the status carries none: held and
    // actionable from the totals, the rest shown as one remainder.
    function overviewKPIGovernorFallbackEquations(data, issueCount, prCount) {
      const totals = data?.overviewTotals || {};
      const issueEq = overviewKPIKindEquation('issues', Number(totals.issues?.forge || issueCount || 0), Number(issueCount || 0), [
        { key: 'held', label: 'held', count: Number(totals.issues?.held || 0) },
      ]);
      const prEq = overviewKPIKindEquation('PRs', Number(totals.prs?.forge || prCount || 0), Number(prCount || 0), [
        { key: 'held', label: 'held', count: Number(totals.prs?.held || 0) },
      ]);
      return { issueEq, prEq };
    }

    // The six term tiles in tile order: the key (also the card group and the
    // Projects filter), the equation term, the label and the sparkline
    // field. Card samples use their own history fields, so a sample of
    // today's equation never draws into a card tile's sparkline.
    function overviewCardTileDefs() {
      return [
        { key: 'your-turn', term: 'your_turn', label: 'Needs a person', field: 'overviewCardYourTurn' },
        { key: 'waiting-elsewhere', term: 'waiting_elsewhere', label: 'Waiting on someone else', field: 'overviewCardWaiting' },
        { key: 'held', term: 'held', label: 'Held', field: 'overviewCardHeld' },
        { key: 'blocked', term: 'blocked', label: 'Blocked', field: 'overviewCardBlocked' },
        { key: 'actionable-now', term: 'actionable', label: 'Hive has it', field: 'overviewCardActionable' },
        { key: 'outside', term: 'outside', label: 'Outside', field: 'overviewCardOutside' }
      ];
    }

    // The card tiles for the repos selected in the Overview. With every repo
    // selected they are the server's overview_card.tiles. For a subset they
    // are counted here the same way: each row's card_group and card_band,
    // plus each repo's remainder in Outside (the twin of
    // overviewKPILocalEquations).
    function overviewCardKPI(repos) {
      const card = overviewCard();
      if (!card) return null;
      const kinds = v => ({ issues: Number((v && v.issues) || 0), prs: Number((v && v.prs) || 0) });
      const issueSpecs = overviewCardSpecs('issue');
      const prSpecs = overviewCardSpecs('pr');
      const both = new Set(issueSpecs.map(spec => spec.key).filter(key => prSpecs.some(spec => spec.key === key)));
      const serverTiles = new Map(overviewCardTiles().map(tile => [tile.key, tile]));
      if (_overviewKPILocalScope === 'all' && serverTiles.size) {
        const tiles = overviewCardTileDefs().map(def => {
          const tile = serverTiles.get(def.key) || {};
          return Object.assign({}, def, { label: tile.label || def.label, rule: tile.rule || '', count: Number(tile.count || 0), issues: Number(tile.issues || 0), prs: Number(tile.prs || 0), breakdown: Array.isArray(tile.breakdown) ? tile.breakdown : [] });
        });
        return { tiles, both, notInInbox: kinds(card.your_turn_not_in_inbox), unexplained: kinds(card.unexplained), remainder: kinds(card.remainder) };
      }
      const groups = { issues: {}, prs: {} };
      const bands = { issues: {}, prs: {} };
      const notInInbox = { issues: 0, prs: 0 };
      const unexplained = { issues: 0, prs: 0 };
      const remainder = { issues: 0, prs: 0 };
      (repos || []).forEach(r => {
        [['issue', 'issues'], ['pr', 'prs']].forEach(([kind, k]) => {
          overviewCardRows(r, kind).forEach(([row]) => {
            if (!row || !row.card_band) return;
            groups[k][row.card_group] = (groups[k][row.card_group] || 0) + 1;
            bands[k][row.card_band] = (bands[k][row.card_band] || 0) + 1;
            // Every Your turn band but Unexplained, as the server counts
            // your_turn_not_in_inbox.
            if (row.card_group === 'your-turn' && row.card_band !== 'unexplained' && !(row.ownership && row.ownership.your_turn)) notInInbox[k]++;
            if (row.card_band === 'unexplained') unexplained[k]++;
          });
        });
        const rest = overviewCardRepoRemainder(r);
        remainder.issues += rest.issues;
        remainder.prs += rest.prs;
      });
      const specs = issueSpecs.concat(prSpecs.filter(spec => !both.has(spec.key)));
      const tiles = overviewCardTileDefs().map(def => {
        const tile = serverTiles.get(def.key) || {};
        // The server's breakdown order (every band in tile order), so a
        // PR-only band is not appended after the issue donut's bands.
        const rank = new Map((Array.isArray(tile.breakdown) ? tile.breakdown : []).map((row, i) => [row && row.key, i]));
        const at = key => (rank.has(key) ? rank.get(key) : rank.size);
        const extra = def.key === 'outside' ? remainder : { issues: 0, prs: 0 };
        const issues = Number(groups.issues[def.key] || 0) + extra.issues;
        const prs = Number(groups.prs[def.key] || 0) + extra.prs;
        const breakdown = specs.filter(spec => spec.group === def.key).map(spec => {
          const bi = Number(bands.issues[spec.key] || 0), bp = Number(bands.prs[spec.key] || 0);
          return { key: spec.key, label: spec.label, rule: spec.rule, keep: !!spec.keep, count: bi + bp, issues: bi, prs: bp };
        }).sort((a, b) => at(a.key) - at(b.key));
        return Object.assign({}, def, { label: tile.label || def.label, rule: tile.rule || '', count: issues + prs, issues, prs, breakdown });
      });
      return { tiles, both, notInInbox, unexplained, remainder };
    }

    function overviewCardKinds(issues, prs) {
      const i = Number(issues || 0), p = Number(prs || 0);
      return `${i} ${i === 1 ? 'issue' : 'issues'}, ${p} ${p === 1 ? 'PR' : 'PRs'}`;
    }

    // A tile's ⓘ: its bands with counts. Zero bands are left out except the
    // keep bands (Queued for agents, Unexplained). A band on both donuts is one
    // row, "Human placed on hold (2 issues, 2 PRs)" (a kept band at zero is
    // just its name). Outside adds the
    // remainder; Your turn says how many of its items the inbox does not show.
    function overviewCardTermInfo(tile, kpi) {
      const issueSpecs = new Map(overviewCardSpecs('issue').map(spec => [spec.key, spec]));
      const prSpecs = new Map(overviewCardSpecs('pr').map(spec => [spec.key, spec]));
      // Unexplained is listed in Your turn only when it is above zero; the
      // legend keeps it at zero as a keep band.
      const shown = row => Number(row.count || 0) > 0 || (row.keep && row.key !== 'unexplained');
      const rows = (tile.breakdown || []).filter(shown).map(row => {
        const count = Number(row.count || 0);
        const both = kpi.both.has(row.key) && count > 0;
        const issueRule = (issueSpecs.get(row.key) || {}).rule || '';
        const prRule = (prSpecs.get(row.key) || {}).rule || '';
        // A row that combines issues and PRs names both rules when they differ.
        const rule = both && Number(row.issues || 0) > 0 && Number(row.prs || 0) > 0 && issueRule && prRule && issueRule !== prRule
          ? `Issues: ${issueRule} PRs: ${prRule}`
          : (row.rule || issueRule || prRule);
        const kind = issueSpecs.has(row.key) && (Number(row.issues || 0) > 0 || !prSpecs.has(row.key)) ? 'issue' : 'pr';
        return {
          key: row.key,
          label: both ? `${row.label} (${overviewCardKinds(row.issues, row.prs)})` : row.label,
          count,
          rule,
          keep: !!row.keep,
          card: true,
          swatch: 'overview-card-' + kind + '-' + row.key
        };
      });
      const rest = Number(kpi.remainder.issues || 0) + Number(kpi.remainder.prs || 0);
      if (tile.key === 'outside' && rest > 0) rows.push({ key: 'remainder', label: `Open but not listed (${overviewCardKinds(kpi.remainder.issues, kpi.remainder.prs)})`, count: rest, rule: 'Open on GitHub but not listed by the scanner, or a PR the hive sidelines that is not a draft.', card: true, swatch: 'overview-card-issue-not-work' });
      const hidden = Number(kpi.notInInbox.issues || 0) + Number(kpi.notInInbox.prs || 0);
      const note = tile.key === 'your-turn' && hidden > 0 ? `Includes ${hidden} the hive holds for a person that ${hidden === 1 ? 'is' : 'are'} not in the inbox.` : '';
      // Outside keeps the server equation's Settings and Learn more links:
      // which settings keep items off the queue does not change with the card.
      const outsideTerm = tile.key === 'outside' ? ((window._lastStatus?.actionableNow?.equation?.terms || []).find(t => t && t.key === 'outside') || {}) : null;
      const links = outsideTerm ? (Array.isArray(outsideTerm.breakdown) ? outsideTerm.breakdown : []) : [];
      return { key: tile.term, label: tile.label, count: Number(tile.count || 0), rule: tile.rule || '', note, card: true, breakdown: rows, links, docsHref: outsideTerm ? (outsideTerm.docsHref || '/docs/dashboard-sections/overview-and-metrics.md#overview') : '' };
    }

    // Card mode for the tiles renderOverviewKPIs built: the term tiles take
    // the card's sparkline fields, plain-words tips and an ⓘ each; the two
    // total tiles take the card's partition sublines.
    function overviewCardApplyTiles(cards, kpi, issueEq, prEq) {
      const byKey = new Map(kpi.tiles.map(tile => [tile.key, tile]));
      const perKind = k => Object.fromEntries(kpi.tiles.map(tile => [tile.term, Number(tile[k] || 0)]));
      (cards || []).forEach(card => {
        if (card.key === 'open-issues') {
          card.subline = renderOverviewTotalPartitionSubline(issueEq, { kind: 'issues', counts: perKind('issues') });
          card.tip = 'All open GitHub issues (pull requests excluded) across the selected configured repos. Each one counts in exactly one tile: needs a person, waiting on someone else, held, hive has it, or outside. Click to filter Projects to matching issues.';
          return;
        }
        if (card.key === 'open-prs') {
          card.subline = renderOverviewTotalPartitionSubline(prEq, { kind: 'prs', counts: perKind('prs') });
          card.tip = 'All open GitHub pull requests across the selected configured repos, including drafts. Each one counts in exactly one tile: needs a person, waiting on someone else, held, blocked, hive has it, or outside (drafts). Click to filter Projects to matching PRs.';
          return;
        }
        const tile = byKey.get(card.key);
        if (!tile) return;
        card.field = tile.field;
        card.label = tile.label;
        // Hive has it is the mock-up's blue in both themes (--accent turns
        // red in light mode).
        if (card.key === 'actionable-now') card.color = 'var(--band-unclaimed)';
        card.tip = `${tile.count} ${tile.label.toLowerCase()} = ${tile.issues} issues + ${tile.prs} PRs.${tile.rule ? ' ' + tile.rule : ''} Click to filter Projects.`;
        card.infoHTML = overviewPartitionInfo(overviewCardTermInfo(tile, kpi), 'overview-kpi-' + card.key);
      });
    }

    function renderOverviewKPIs(repos, issueSlices, prSlices, state) {
      _overviewKPILocalScope = overviewKPIRepoScope(repos);
      const forgeTotals = overviewKPIForgeTotals(repos);
      // With the Overview card the term tiles count card groups; without it
      // (an older server) they count today's equation terms.
      const cardKPI = overviewCardMode() ? overviewCardKPI(repos) : null;
      const serverActionable = window._lastStatus?.actionableNow;
      const useServerEquation = _overviewKPILocalScope === 'all' && !!(serverActionable?.equation && serverActionable?.issueEquation && serverActionable?.prEquation);
      const local = useServerEquation ? null : overviewKPILocalEquations(repos, forgeTotals);
      const equation = useServerEquation ? serverActionable.equation : local.equation;
      const issueEq = useServerEquation ? serverActionable.issueEquation : local.issueEq;
      const prEq = useServerEquation ? serverActionable.prEquation : local.prEq;
      const openIssues = Number(forgeTotals?.issues?.forge ?? issueEq.open);
      const openPRs = Number(forgeTotals?.prs?.forge ?? prEq.open);
      const cardTerm = key => (cardKPI ? cardKPI.tiles.find(t => t.term === key) : null) || {};
      const term = key => cardKPI ? Number(cardTerm(key).count || 0) : overviewKPITerm(equation, key, 0);
      const split = key => cardKPI ? [Number(cardTerm(key).issues || 0), Number(cardTerm(key).prs || 0)] : [overviewEquationTermCount(issueEq, key), overviewEquationTermCount(prEq, key)];
      const actionableIssueSplit = cardKPI ? split('actionable')[0] : Number(issueEq.result || 0);
      const actionablePRSplit = cardKPI ? split('actionable')[1] : Number(prEq.result || 0);
      const actionableNow = actionableIssueSplit + actionablePRSplit;
      const yourTurn = term('your_turn');
      const waiting = term('waiting_elsewhere');
      const held = term('held');
      const blocked = term('blocked');
      const equationText = equation?.text || '';
      const equationHTML = equationText ? renderActionableEquationSubline(equation) : '';
      const equationTitle = equation?.title || equationText || 'Items in bands that can move without waiting.';
      const [yourTurnIssues, yourTurnPRs] = split('your_turn');
      const [waitingIssues, waitingPRs] = split('waiting_elsewhere');
      const [heldIssues, heldPRs] = split('held');
      const [, blockedPRs] = split('blocked');
      const outside = cardKPI ? term('outside') : overviewEquationTermCount(equation, 'outside') + overviewEquationTermCount(equation, 'other');
      const outsideIssues = cardKPI ? split('outside')[0] : overviewEquationTermCount(issueEq, 'outside') + overviewEquationTermCount(issueEq, 'other');
      const outsidePRs = cardKPI ? split('outside')[1] : overviewEquationTermCount(prEq, 'outside') + overviewEquationTermCount(prEq, 'other');
      const outsideTermBase = (equation?.terms || []).find(t => t && t.key === 'outside');
      const outsideTerm = Object.assign({ key: 'outside', label: 'outside', count: outside, rule: "Open items excluded before they enter Overview bands by this hive's scanner settings." }, outsideTermBase || {});
      const outsideInfo = overviewPartitionInfo(outsideTerm, 'overview-kpi-outside');
      const termInfo = (key, context) => overviewPartitionInfo((equation?.terms || []).find(t => t && t.key === key), context);
      const useServerHistory = _overviewKPILocalScope === 'all';
      const cards = [
        { key: 'open-issues', field: 'overviewOpenIssues', label: 'Total open issues', value: openIssues, rawValue: openIssues, color: 'var(--orange)', tip: 'All open GitHub issues (pull requests excluded) across the selected configured repos. Each one counts in exactly one term: your turn, waiting on someone else, held, outside, or actionable. Click to filter Projects to matching issues.', subline: renderOverviewTotalPartitionSubline(issueEq) },
        { key: 'open-prs', field: 'overviewOpenPrs', label: 'Total open PRs', value: openPRs, rawValue: openPRs, color: 'var(--green)', tip: 'All open GitHub pull requests across the selected configured repos, including drafts. Each one counts in exactly one term: your turn, waiting on someone else, held, blocked, outside (drafts are outside until ready), or actionable. Click to filter Projects to matching PRs.', subline: renderOverviewTotalPartitionSubline(prEq) },
        { key: 'actionable-now', field: 'overviewActionable', label: 'Actionable now', value: actionableNow, rawValue: actionableNow, color: 'var(--accent)', tip: equationTitle + ' Click to filter Projects to this term.', subline: renderOverviewSplitSubline(actionableNow, actionableIssueSplit, actionablePRSplit, 'actionable') },
        { key: 'your-turn', field: 'overviewYourTurn', label: 'Your turn', value: yourTurn, rawValue: yourTurn, color: 'var(--red)', tip: `${yourTurn} your turn = ${yourTurnIssues} issues + ${yourTurnPRs} PRs. Items Hive is stuck on a person for, including the ones to close. Click to filter Projects.`, subline: renderOverviewSplitSubline(yourTurn, yourTurnIssues, yourTurnPRs, 'your turn'), infoHTML: termInfo('your_turn', 'overview-kpi-your-turn') },
        { key: 'waiting-elsewhere', field: 'overviewWaiting', label: 'Waiting on someone else', value: waiting, rawValue: waiting, color: 'var(--muted)', tip: `${waiting} waiting on someone else = ${waitingIssues} issues + ${waitingPRs} PRs. Gated on a person's hold or a pause, the adjudicator, a reporter, or an open PR. Click to filter Projects.`, subline: renderOverviewSplitSubline(waiting, waitingIssues, waitingPRs, 'waiting'), infoHTML: termInfo('waiting_elsewhere', 'overview-kpi-waiting') },
        { key: 'held', field: 'overviewHeld', label: 'Held', value: held, rawValue: held, color: 'color-mix(in srgb, var(--orange) 65%, var(--muted))', tip: `${held} held = ${heldIssues} issues + ${heldPRs} PRs. Issues and PRs paused by a hold that no earlier term claimed. Click to filter Projects.`, subline: renderOverviewSplitSubline(held, heldIssues, heldPRs, 'held') },
        { key: 'blocked', field: 'overviewBlocked', label: 'Blocked', value: blocked, rawValue: blocked, color: 'var(--red)', tip: `${blocked} blocked PRs. Pull requests with a blocked verdict, merge conflicts or failing CI. Click to filter Projects.`, subline: renderOverviewSplitSubline(blocked, 0, blockedPRs, 'blocked') },
        { key: 'outside', field: 'overviewOutside', label: 'Outside', value: outside, rawValue: outside, color: 'var(--muted)', tip: `${outside} outside = ${outsideIssues} issues + ${outsidePRs} PRs. Open items excluded before they enter Overview bands by this hive's scanner settings, and draft PRs. Click to filter Projects.`, subline: renderOverviewSplitSubline(outside, outsideIssues, outsidePRs, 'outside'), infoHTML: outsideInfo }
      ];
      if (cardKPI) overviewCardApplyTiles(cards, cardKPI, issueEq, prEq);
      const coverageCard = overviewCoverageCard();
      const snapshotComplete = !(repos || []).some(r => r && (r.countsIncomplete || r.countsStale)) && !(_overviewKPILocalScope === 'all' && window._lastStatus?.overviewKPIIncomplete);
      if (snapshotComplete) overviewKPIRecordLocal(overviewKPICurrentSample(coverageCard ? cards.concat([coverageCard]) : cards));
      const ops = ['+', '=', '+', '+', '+', '+', '+'];
      const equationLabel = `${openIssues} total open issues + ${openPRs} total open PRs = ${actionableNow} ${cardKPI ? 'hive has it' : 'actionable now'} + ${yourTurn} ${cardKPI ? 'needs a person' : 'your turn'} + ${waiting} waiting on someone else + ${held} held + ${blocked} blocked + ${outside} outside`;
      // Last-good figures after a refused enumeration (#13382) or a failed
      // forgestate reconcile (#13376) are current as of staleSince, not
      // zero; say so beside the KPIs.
      const staleSince = overviewStaleSince(repos);
      // Open items a source reported in its totals but could not list are
      // not in the counts (the tile, pill and activity monitor count the
      // listed items — open_items.go); they are named beside them instead.
      const unlisted = overviewUnlistedCounts(repos);
      // The sidebar Overview pill is the same two totals the tiles print,
      // from the same snapshot, on every tile render — never a separately
      // counted or separately cached number.
      if (window._lastStatus) {
        const pill = overviewSidebarPill(openIssues, openPRs, equationLabel, staleSince, unlisted);
        setSidebarPairBadge(document.getElementById('overview-sidebar-badge'), pill.issues, pill.prs, pill.title, pill.prefix);
      }
      if (!state.showKPIs) return '';
      if (snapshotComplete) overviewKPIRecordLocal(overviewKPICurrentSample(coverageCard ? cards.concat([coverageCard]) : cards));
      const staleText = staleSince ? overviewStaleBadgeText(staleSince, overviewRestartRefresh()) : null;
      const staleBadge = staleText ? `<span class="overview-kpi-stale" role="status" title="${esc(staleText.title)}">${esc(staleText.icon)} ${esc(staleText.text)}</span>` : '';
      const unlistedBadge = unlisted.issues + unlisted.prs > 0 ? `<span class="overview-kpi-stale" role="status" title="${esc(overviewUnlistedNote(unlisted))}">+${unlisted.issues + unlisted.prs} not yet listed</span>` : '';
      // A repo listing open PRs but no issues while its issue side is
      // stale or unlisted (repos[].issuesSyncing) is a pending issue fetch,
      // not an empty backlog: the issue total is a lower bound until the
      // list comes back (open_items.go markIssuesSyncing).
      const syncingBadge = unlisted.syncing > 0 ? `<span class="overview-kpi-stale" role="status" title="${esc(overviewSyncingNote(unlisted))}">⏳ issues syncing · ${unlisted.syncing} ${unlisted.syncing === 1 ? 'repo' : 'repos'}</span>` : '';
      return `<div class="overview-headline sec-headline" data-collapsed-keep>${overviewKPIWindowControls()}${staleBadge}${syncingBadge}${unlistedBadge}<div class="overview-kpis" aria-label="${esc(equationLabel)}">${cards.map((card, i) => {
        const value = card.value;
        const n = Number(card.rawValue);
        const attrs = Number.isFinite(n) ? ` data-overview-kpi-key="${esc(card.key)}" data-overview-kpi-value="${n}" data-overview-kpi-display="${esc(String(value ?? '—'))}"` : '';
        const subline = card.subline ? `<span class="overview-kpi-subline">${card.subline}</span>` : '';
        const label = `<span class="overview-kpi-label">${esc(card.label)}${card.infoHTML || ''}</span>`;
        const tile = `<div class="overview-kpi" role="button" tabindex="0" data-action="overviewApplyKPIProjectFilter" data-keydown-action="overviewApplyKPIProjectFilter" data-keys="Enter, " data-prevent="1" data-arg0="${esc(card.key)}" data-arg1="${Number.isFinite(n) ? n : 0}" data-arg-types="s,j" data-stop="1" aria-label="${esc(card.tip)}"><span class="overview-kpi-value"${attrs}>${esc(String(value ?? '—'))}</span>${label}${subline}${overviewKPISpark(card, useServerHistory)}</div>`;
        return tile + (ops[i] ? `<span class="overview-kpi-operator" aria-hidden="true">${ops[i]}</span>` : '');
      }).join('')}${coverageCard ? `<span class="overview-kpi-coverage-spacer" aria-hidden="true"></span>${renderOverviewCoverageKPI(coverageCard, useServerHistory)}` : ''}</div></div>`;
    }

    /* Pure: the sidebar Overview pill mirrors the "Total open issues" /
       "Total open PRs" tiles exactly; the title carries the tile equation
       and the last-good "stale" note. overview_sidebar_pill_test.go runs
       it under node. */
    // Pure: the sidebar Overview pill. The text is the open-item totals the
    // tiles print, named "board pressure" (BP) — the open issues and PRs
    // the configured repos put on this hive's board — so it reads
    // "BP = issues/PRs" (#17240); the tooltip names each half and says that
    // the Hive activity monitor pill counts rows of the same set — every
    // issue plus each PR not drawn under a listed issue — so 33/13 beside
    // Overview and 37 beside the activity monitor are one population, not
    // two disagreeing counts (overviewSidebarPillLead).
    function overviewSidebarPill(openIssues, openPRs, equationLabel, staleSince, unlisted) {
      const issues = Number(openIssues) || 0;
      const prs = Number(openPRs) || 0;
      let title = overviewSidebarPillLead(issues, prs) + ' · ' + (equationLabel || `${issues} total open issues + ${prs} total open PRs`);
      if (staleSince) title += ` (${overviewStaleBadgeText(staleSince, overviewRestartRefresh()).text})`;
      const syncing = overviewSyncingNote(unlisted);
      if (syncing) title += ` · ${syncing}`;
      const note = overviewUnlistedNote(unlisted);
      if (note) title += ` · ${note}`;
      return { issues, prs, title, prefix: 'BP = ' };
    }
    function overviewSidebarPillLead(issues, prs) {
      const i = Number(issues) || 0, p = Number(prs) || 0;
      return `Board pressure (BP): ${i} open ${i === 1 ? 'issue' : 'issues'} / ${p} open ${p === 1 ? 'PR' : 'PRs'} — the Hive activity monitor pill counts rows of this same set (every issue, plus each PR not shown under a listed issue), so it can read differently`;
    }

    /* Pure: open items the enumeration or forgestate cache reported in its
       totals without an item row behind them (repos[].unlistedIssues /
       unlistedPrs). Counted beside the totals, never inside them. */
    function overviewUnlistedCounts(repos) {
      const out = { issues: 0, prs: 0, syncing: 0 };
      (repos || []).forEach(r => { if (!r) return; out.issues += Number(r.unlistedIssues || 0); out.prs += Number(r.unlistedPrs || 0); if (r.issuesSyncing) out.syncing++; });
      return out;
    }
    /* Pure: repos whose issue side has not been listed for real yet
       (repos[].issuesSyncing): they show open PRs and no issues while
       stale or unlisted, so their issue count is pending, not zero. */
    function overviewSyncingNote(unlisted) {
      const n = Number(unlisted?.syncing || 0);
      if (n <= 0) return '';
      return `issues still syncing for ${n} ${n === 1 ? 'repo' : 'repos'} — the issue total is a lower bound until the issue list comes back`;
    }
    function overviewUnlistedNote(unlisted) {
      const issues = Number(unlisted?.issues || 0), prs = Number(unlisted?.prs || 0);
      if (issues + prs <= 0) return '';
      return `${issues} ${issues === 1 ? 'issue' : 'issues'} / ${prs} ${prs === 1 ? 'PR' : 'PRs'} reported open but not yet listed by the scanner`;
    }

    function overviewRepoFilterHTML(repos) {
      const all = overviewAllRepoNames(repos);
      const selected = new Set(overviewSelectedRepoNames(repos));
      const orgs = overviewRepoOrgs(repos);
      const orgLinks = orgs.length > 1 ? `<div class="overview-settings-links">${orgs.map(org => `<button type="button" class="hv-btn btn-sm" data-action="setOverviewRepoOrg" data-arg0="${esc(org)}" data-stop="1">Org: ${esc(org)}</button>`).join('')}</div>` : '';
      const rows = all.map(repo => `<label class="overview-settings-row"><input type="checkbox" data-change-action="setOverviewRepoFilter" data-arg0="${esc(repo)}" data-arg-types="s,c"${selected.has(repo) ? ' checked' : ''}>${esc(repo)}</label>`).join('');
      return `<div class="overview-settings-section"><div class="overview-settings-card-title">Repos</div><div class="overview-settings-links"><button type="button" class="hv-btn btn-sm" data-action="setOverviewReposAll" data-stop="1">All</button><button type="button" class="hv-btn btn-sm" data-action="setOverviewReposNone" data-stop="1">None</button></div>${orgLinks}<div id="overview-repo-filter" class="overview-repo-filter" role="group" aria-label="Repos">${rows || '<div class="overview-empty">No repos configured.</div>'}</div></div>`;
    }

    function renderOverviewSettingsPopover(state) {
      const el = document.getElementById('overview-chart-settings-body');
      if (!el) return;
      const typeRows = OVERVIEW_CHART_TYPES.map(type => `<label class="overview-settings-row"><input type="checkbox" data-change-action="setOverviewChartRotation" data-arg0="${esc(type)}" data-arg-types="s,c"${state.enabled[type] ? ' checked' : ''}>${esc(OVERVIEW_CHART_TYPE_LABELS[type])}<span>in rotation</span></label>`).join('');
      const durationOptions = OVERVIEW_DURATIONS.map(d => `<option value="${esc(d)}"${state.duration === d ? ' selected' : ''}>${esc(d)}</option>`).join('');
      const exportOptions = OVERVIEW_EXPORT_FORMATS.map(f => `<option value="${esc(f)}"${state.exportFormat === f ? ' selected' : ''}>${esc(f.toUpperCase())}</option>`).join('');
      const labelOptions = OVERVIEW_LABEL_MODES.map(m => `<option value="${esc(m)}"${state.labelMode === m ? ' selected' : ''}>${esc(m === 'both' ? 'counts + percentages' : m)}</option>`).join('');
      const seconds = Math.round(state.intervalMs / 1000);
      const cardControls = (kind, label) => {
        const enabled = overviewEnabledTypes(state);
        const active = enabled.includes(state.active[kind]) ? state.active[kind] : enabled[0];
        const autoTitle = state.carousel ? 'Auto-play on — rotates every ' + Math.round(overviewCarouselDwellMs(state) / 1000) + 's through the ticked charts. Click to pause.' : 'Auto-play off — click to rotate through the ticked charts every ' + Math.round(overviewCarouselDwellMs(state) / 1000) + 's';
        return `<div class="overview-settings-section" aria-label="${esc(label)} chart controls"><div class="overview-settings-card-title">${esc(label)}</div><div class="overview-chart-controls"><button type="button" class="overview-carousel-arrow" data-action="overviewCarouselPrev" data-arg0="${kind}" data-stop="1" title="Previous ${esc(label)} chart" aria-label="Previous ${esc(label)} chart">◀</button><label class="overview-settings-field">Chart type<select class="overview-chart-select" data-change-action="setOverviewChartType" data-arg0="${kind}" data-arg-types="s,v" title="${esc(label)} chart type">${overviewChartTypeOptions(active)}</select></label><button type="button" class="overview-carousel-arrow" data-action="overviewCarouselNext" data-arg0="${kind}" data-stop="1" title="Next ${esc(label)} chart" aria-label="Next ${esc(label)} chart">▶</button>${renderOverviewDots(enabled, active)}<span class="overview-chart-controls-sep"></span><button type="button" class="overview-carousel-arrow" data-action="toggleOverviewCarousel" data-stop="1" aria-pressed="${state.carousel ? 'true' : 'false'}" title="${esc(autoTitle)}" aria-label="${esc(autoTitle)}">${state.carousel ? '⏸ auto' : '⏵ auto'}</button><label class="overview-settings-field">Transition<select class="overview-chart-select" data-change-action="setOverviewTransition" data-arg-types="v" title="Transition between charts">${overviewTransitionOptions(state.transition)}</select></label></div></div>`;
      };
      setIfChanged(el, `<div class="overview-settings-section"><div class="overview-settings-card-title">Overview charts</div><div class="overview-settings-grid">${typeRows}</div><div class="overview-settings-grid"><label class="overview-settings-field">Interval (seconds)<input type="number" min="5" max="300" step="5" value="${seconds}" data-change-action="setOverviewCarouselInterval" data-arg-types="N"></label><label class="overview-settings-field">Duration<select data-change-action="setOverviewDuration" data-arg-types="v">${durationOptions}</select></label><label class="overview-settings-field">Export format<select data-change-action="setOverviewExportFormat" data-arg-types="v">${exportOptions}</select></label><label class="overview-settings-field">Donut labels<select data-change-action="setOverviewLabelMode" data-arg-types="v">${labelOptions}</select></label><label class="overview-settings-row"><input type="checkbox" data-change-action="setOverviewKPIs" data-arg-types="c"${state.showKPIs ? ' checked' : ''}>Show KPI strip</label>${typeof subscriptionGaugeOwner === 'function' && subscriptionGaugeOwner() ? `<label class="overview-settings-row"><input type="checkbox" data-change-action="setOverviewUsageGauge" data-arg-types="c"${state.showUsageGauge ? ' checked' : ''}>Show usage gauge</label>` : ''}</div><div class="overview-settings-links"><button type="button" class="hv-btn btn-sm" data-action="resetOverviewSettings" data-stop="1">Reset to defaults</button></div></div>${overviewRepoFilterHTML(_overviewLastRepos || [])}${cardControls('issue', 'Issues')}${cardControls('pr', 'PRs')}`);
    }

    function rerenderOverviewChartsFromState(state) {
      saveOverviewChartsState(state);
      renderOverviewCharts(_overviewLastRepos || []);
    }

    function setOverviewChartRotation(type, checked) {
      const state = overviewChartsState();
      if (OVERVIEW_CHART_TYPES.includes(type)) state.enabled[type] = !!checked;
      if (!OVERVIEW_CHART_TYPES.some(t => state.enabled[t])) state.enabled.donut = true;
      rerenderOverviewChartsFromState(state);
    }

    function toggleOverviewCarousel() {
      const state = overviewChartsState();
      state.carousel = !state.carousel;
      if (state.carousel && overviewEnabledTypes(state).length < 2) OVERVIEW_CHART_TYPES.forEach(t => { state.enabled[t] = true; });
      rerenderOverviewChartsFromState(state);
    }

    function setOverviewCarouselEnabled(checked) {
      const state = overviewChartsState();
      state.carousel = !!checked;
      rerenderOverviewChartsFromState(state);
    }

    function setOverviewCarouselInterval(seconds) {
      const state = overviewChartsState();
      state.intervalMs = Math.min(OVERVIEW_CAROUSEL_MAX_MS, Math.max(OVERVIEW_CAROUSEL_MIN_MS, Number(seconds || 30) * 1000));
      rerenderOverviewChartsFromState(state);
    }

    function overviewTransitionDurationMs(state) {
      return OVERVIEW_TRANSITION_DURATION_MS[(state && state.duration) || 'normal'] || OVERVIEW_TRANSITION_DURATION_MS.normal;
    }

    function overviewCarouselDwellMs(state) {
      return Math.max(Number((state && state.intervalMs) || OVERVIEW_CAROUSEL_DEFAULT_MS), overviewTransitionDurationMs(state));
    }

    function setOverviewTransition(value) {
      const state = overviewChartsState();
      if (OVERVIEW_TRANSITIONS.includes(value)) state.transition = value;
      rerenderOverviewChartsFromState(state);
    }

    function setOverviewDuration(value) {
      const state = overviewChartsState();
      if (OVERVIEW_DURATIONS.includes(value)) state.duration = value;
      rerenderOverviewChartsFromState(state);
    }

    function setOverviewExportFormat(value) {
      const state = overviewChartsState();
      if (OVERVIEW_EXPORT_FORMATS.includes(value)) state.exportFormat = value;
      rerenderOverviewChartsFromState(state);
    }

    function setOverviewLabelMode(value) {
      const state = overviewChartsState();
      if (OVERVIEW_LABEL_MODES.includes(value)) state.labelMode = value;
      rerenderOverviewChartsFromState(state);
    }

    function setOverviewKPIs(checked) {
      const state = overviewChartsState();
      state.showKPIs = !!checked;
      rerenderOverviewChartsFromState(state);
    }

    function setOverviewUsageGauge(checked) {
      const state = overviewChartsState();
      state.showUsageGauge = !!checked;
      rerenderOverviewChartsFromState(state);
      if (typeof subscriptionGaugeRefresh === 'function') subscriptionGaugeRefresh();
    }

    function setOverviewRepoFilter(repo, checked) {
      const all = overviewAllRepoNames(_overviewLastRepos || []);
      const selected = new Set(overviewSelectedRepoNames(_overviewLastRepos || []));
      if (checked) selected.add(repo); else selected.delete(repo);
      saveOverviewRepoNames(all.filter(name => selected.has(name)));
      renderOverviewCharts(_overviewLastRepos || []);
    }

    function setOverviewReposAll() {
      saveOverviewRepoNames(overviewAllRepoNames(_overviewLastRepos || []));
      renderOverviewCharts(_overviewLastRepos || []);
    }

    function setOverviewReposNone() {
      saveOverviewRepoNames([]);
      renderOverviewCharts(_overviewLastRepos || []);
    }

    function setOverviewRepoOrg(org) {
      const selected = overviewAllRepoNames(_overviewLastRepos || []).filter(repo => repo.split('/')[0] === org);
      saveOverviewRepoNames(selected);
      renderOverviewCharts(_overviewLastRepos || []);
    }

    function resetOverviewSettings() {
      saveOverviewChartsState(overviewDefaultChartState());
      try { localStorage.removeItem(OVERVIEW_REPOS_KEY); } catch {}
      renderOverviewCharts(_overviewLastRepos || []);
    }

    // A legend row opens the card filter of the tile that counts the same rows.
    // With the Overview card a band row goes to its group's tile (the spec's
    // group) and a group row to its own; old slice keys keep their map.
    function overviewNavigateBand(kind, slice) {
      const cardKey = overviewCardMode() ? overviewCardTileForKey(slice) : '';
      const key = cardKey || { 'your-turn': 'your-turn', 'to-close': 'your-turn', 'waiting-elsewhere': 'waiting-elsewhere', blocked: 'blocked', draft: 'outside' }[slice] || 'actionable-now';
      overviewApplyKPIProjectFilter(key);
    }

    function overviewCardTileForKey(key) {
      if (overviewCardTileDefs().some(def => def.key === key)) return key;
      const spec = overviewCardSpecs('issue').concat(overviewCardSpecs('pr')).find(s => s && s.key === key);
      return (spec && spec.group) || '';
    }

    function setOverviewChartType(kind, type) {
      const state = overviewChartsState();
      if ((kind === 'issue' || kind === 'pr') && OVERVIEW_CHART_TYPES.includes(type)) {
        state.active[kind] = type;
        state.enabled[type] = true;
      }
      rerenderOverviewChartsFromState(state);
    }

    // Manual ◀ ▶ steps through every chart type (same domain as the <select>);
    // the auto-play timer only rotates through the ticked types (#9036).
    function overviewCarouselStep(kind, delta, types) {
      const state = overviewChartsState();
      const domain = Array.isArray(types) && types.length ? types : OVERVIEW_CHART_TYPES;
      const current = domain.indexOf(state.active[kind]);
      const next = (Math.max(0, current) + delta + domain.length) % domain.length;
      state.active[kind] = domain[next];
      rerenderOverviewChartsFromState(state);
    }

    function overviewCarouselPrev(kind) { overviewCarouselStep(kind, -1, OVERVIEW_CHART_TYPES); }
    function overviewCarouselNext(kind) { overviewCarouselStep(kind, 1, OVERVIEW_CHART_TYPES); }
    function overviewCarouselAutoNext(kind) { overviewCarouselStep(kind, 1, overviewEnabledTypes(overviewChartsState())); }
    function overviewPanelHover(kind) { if (kind === 'issue' || kind === 'pr') _overviewHoverState[kind] = true; }
    function overviewPanelUnhover(kind) { if (kind === 'issue' || kind === 'pr') _overviewHoverState[kind] = false; }

    let _overviewSettingsReturnFocus = null;
    function overviewSettingsGearButton() {
      return document.querySelector('[data-action="toggleOverviewChartSettings"]');
    }
    function overviewSettingsFocusables() {
      const dialog = document.getElementById('overview-chart-settings');
      if (!dialog) return [];
      return Array.from(dialog.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')).filter(el => !el.disabled && el.offsetParent !== null);
    }
    function openOverviewChartSettings() {
      const overlay = document.getElementById('overview-chart-settings-overlay');
      const btn = overviewSettingsGearButton();
      if (!overlay) return;
      _overviewSettingsReturnFocus = btn || document.activeElement;
      renderOverviewSettingsPopover(overviewChartsState());
      overlay.classList.remove('hidden');
      hiveRememberModalOpen(overlay);
      if (btn) btn.setAttribute('aria-expanded', 'true');
      const focusables = overviewSettingsFocusables();
      const initial = focusables.find(el => el.classList && el.classList.contains('config-close')) || focusables[0];
      if (initial) setTimeout(() => initial.focus(), 0);
    }
    function closeOverviewChartSettings() {
      const overlay = document.getElementById('overview-chart-settings-overlay');
      if (overlay) overlay.classList.add('hidden');
      const btn = overviewSettingsGearButton();
      if (btn) btn.setAttribute('aria-expanded', 'false');
      const ret = _overviewSettingsReturnFocus || btn;
      _overviewSettingsReturnFocus = null;
      if (ret && ret.focus) setTimeout(() => ret.focus({ preventScroll: true }), 0);
    }
    function toggleOverviewChartSettings() {
      const overlay = document.getElementById('overview-chart-settings-overlay');
      if (!overlay) return;
      if (overlay.classList.contains('hidden')) openOverviewChartSettings();
      else closeOverviewChartSettings();
    }
    document.addEventListener('click', function(e) {
      const overlay = document.getElementById('overview-chart-settings-overlay');
      if (overlay && e.target === overlay) closeOverviewChartSettings();
    });
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Tab') return;
      const overlay = document.getElementById('overview-chart-settings-overlay');
      if (!overlay || overlay.classList.contains('hidden')) return;
      const nodes = overviewSettingsFocusables();
      if (!nodes.length) return;
      const first = nodes[0], last = nodes[nodes.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }, true);

    function overviewCarouselVisibilityChanged() {
      if (document.visibilityState === 'visible') renderOverviewCharts(_overviewLastRepos || []);
    }
    document.addEventListener('visibilitychange', overviewCarouselVisibilityChanged);

    function scheduleOverviewCarousel(kind, state) {
      if (!state.carousel || overviewEnabledTypes(state).length < 2) return;
      _overviewCarouselTimers.push(setInterval(() => {
        if (document.visibilityState !== 'visible' || _overviewHoverState[kind]) return;
        overviewCarouselAutoNext(kind);
      }, overviewCarouselDwellMs(state)));
    }

    function renderOverviewCharts(repos) {
      const el = document.getElementById('overview-card');
      if (!el) return;
      _overviewLastRepos = Array.isArray(repos) ? repos : [];
      _overviewCarouselTimers.splice(0).forEach(id => clearInterval(id));
      const state = overviewChartsState();
      installOverviewTransitionDurationVars();
      const selectedRepos = overviewFilterRepos(_overviewLastRepos);
      const selectedNames = overviewSelectedRepoNames(_overviewLastRepos);
      const repoSuffix = overviewRepoSubtitleSuffix(_overviewLastRepos, selectedNames);
      const issueSlices = overviewIssueBandSlices(selectedRepos);
      const prSlices = overviewPRBandSlices(selectedRepos);
      const animationSignature = overviewAnimationSignature(issueSlices, prSlices, selectedNames);
      const dataChanged = overviewShouldAnimateData(animationSignature);
      const issueSampled = overviewSampleHistory('issue', issueSlices, state);
      const prSampled = overviewSampleHistory('pr', prSlices, state);
      const sampled = issueSampled || prSampled;
      const total = issueSlices.concat(prSlices).reduce((n, s) => n + s.count, 0);
      const kpis = renderOverviewKPIs(selectedRepos, issueSlices, prSlices, state);
      // The card's legend rows are bands (the approved mock-up's words).
      const hover = overviewCardMode() ? ' · hover a band for its rule' : ' · hover a slice for its rule';
      const html = total
        ? `${kpis}<div class="overview-grid">${renderOverviewPanel('issue', 'Issues by band', 'Open issues ' + repoSuffix + hover, issueSlices, state)}${renderOverviewPanel('pr', 'PRs by band', 'Open and held pull requests ' + repoSuffix + hover, prSlices, state)}</div>`
        : `${kpis}<div class="overview-empty">Nothing actionable — the hive is caught up</div>`;
      if (sampled) saveOverviewChartsState(state);
      setIfChanged(el, html);
      if (dataChanged && !isSectionCollapsed('overview-section')) {
        triggerOverviewAnimations('data');
      } else {
        animateOverviewKPIs(el, { reduced: true });
      }
      renderOverviewSettingsPopover(state);
      scheduleOverviewCarousel('issue', state);
      scheduleOverviewCarousel('pr', state);
      applySectionCollapse('overview-section');
    }

    function issueTip(issue, info) {
      const labels = ((issue && issue.labels) || []).join(', ');
      const assignees = ((issue && issue.assignees) || []).join(', ');
      const parts = [];
      if (issue && issue.title) parts.push(issue.title);
      if (issue && issue.author) parts.push('@' + issue.author);
      parts.push('band: ' + issueBandLabel(info.band));
      if (labels) parts.push('labels: ' + labels);
      if (assignees) parts.push('assignees: ' + assignees);
      if (issue && issue.created_at) parts.push('created: ' + issue.created_at);
      if (issue && issue.updated_at) parts.push('last activity: ' + issue.updated_at);
      if (info.signals.length) parts.push('signals: ' + info.signals.map(s => s.label).join(', '));
      return parts.join(' — ');
    }

    function issueSignalHTML(info) {
      return (info.signals || []).map(s => s.role
        ? `<span class="pill-role" title="${esc(s.label)}">${esc(s.role)}</span>`
        : `<span class="pill-state" title="${esc(s.label)}">${esc(s.glyph)}</span>`).join('');
    }


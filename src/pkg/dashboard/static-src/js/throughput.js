    // ── Delivery SLA (Throughput) ─────────────────────────────────────────────
    // Reads /api/status .sla (fleet) and repos[].sla, derived server-side from
    // the lifecycle timeline — no extra fetch. Spans are hours; p50 is coloured
    // against the sla.targets budgets (Settings → Thresholds).
    const PRT_SLA_SPANS = [
      { key: 'open_to_triage', target: '', label: 'Open → triage', color: 'var(--muted)', wait: 'collecting — needs issue created_at (recorded at the next enumeration)', tip: 'Hours from the issue being opened on GitHub to the hive classifying it.' },
      { key: 'triage_to_start', target: '', label: 'Triage → start', color: 'var(--muted)', wait: 'collecting — first numbers after an agent starts a classified issue', tip: 'Hours from the hive classifying the issue to an agent starting work on it.' },
      { key: 'start_to_pr', hist: 'start_to_pr_p50', target: 'start_to_pr_h', label: 'Start → PR', color: 'var(--accent)', wait: 'collecting — first numbers after the next linked PR (hive-open-pr --issues) opens', tip: 'Hours from the stage/030-pr-in-progress start mark (an agent claimed, commented or opened a PR on the issue) to the PR that fixes it being opened.' },
      { key: 'pr_to_merge', hist: 'pr_to_merge_p50', target: 'pr_to_merge_h', label: 'PR → merge', color: 'var(--green)', wait: 'collecting — first numbers after the next linked PR merges', tip: 'Hours from the fixing PR opening to its merge (hive-driven or observed).' },
      { key: 'open_to_merge', hist: 'open_to_merge_p50', target: 'open_to_merge_h', label: 'Open → merge', color: 'var(--orange)', wait: 'collecting — needs issue created_at (next enumeration) and a linked PR merge', tip: 'Hours from the issue being opened on GitHub to the fixing PR merging — the end-to-end delivery time.' },
    ];
    const PRT_SLA_WARN_RATIO = 1.5;
    let _prtSLAExpanded = false;
    // The SLA window follows the Throughput period pills: 7d percentiles for
    // every pill except `all`, which switches to the 30d window.
    function prtSLAWindow() { return prtHours > 0 && prtHours <= 168 ? '7d' : '30d'; }

    function prtSLAState(p50, n, target) {
      if (!Number.isFinite(n) || n <= 0 || !Number.isFinite(p50)) return 'none';
      if (!Number.isFinite(target) || target <= 0) return 'none';
      if (p50 <= target) return 'ok';
      if (p50 <= target * PRT_SLA_WARN_RATIO) return 'warn';
      return 'breach';
    }

    function fmtSLAHours(h) {
      if (!Number.isFinite(h)) return '—';
      if (h < 1) return `${Math.round(h * 60)}m`;
      if (h < 48) return `${h >= 10 ? Math.round(h) : h.toFixed(1)}h`;
      return `${(h / 24).toFixed(1)}d`;
    }

    // prtSLASeries turns the 30 daily buckets into a dense sparkline
    // series: days with no closed span carry the previous day's p50 forward so
    // a quiet weekend does not read as "zero hours".
    function prtSLASeries(history, field) {
      const out = [];
      let last = 0;
      (history || []).forEach(d => {
        const v = Number(d && d[field]);
        if (Number.isFinite(v) && v > 0) last = v;
        out.push(last);
      });
      return out;
    }

    function prtSLASpark(span, report, state, width) {
      const series = prtSLASeries(report && report.history, span.hist);
      if (!series.some(v => v > 0)) return '';
      const title = `30-day daily p50 · ${overviewKPISparkTitle(series.filter(v => v > 0), fmtSLAHours)}`;
      return `<span class="prt-metric-spark">${renderSparkline(null, series, {
        width: width || 240, height: 28, min: 0, color: state === 'none' ? span.color : 'var(--prt-sla-color)', title,
        ariaLabel: `${span.label}: ${title}`, label: `sla ${span.label}`, id: `prt-sla:${span.key}`,
        wrapperClass: 'sparkline--flush', wrapperTitle: title, strokeWidth: 1.5,
      })}</span>`;
    }

    function prtSLAInfoHTML(id, label, tip) {
      return `<span class="config-info overview-term-info" tabindex="0" data-action="gh3" data-stop="1" aria-describedby="${esc(id)}" aria-label="${esc(label)} details">ⓘ<span id="${esc(id)}" class="config-tooltip" role="tooltip">${esc(tip)}</span></span>`;
    }

    // prtSLAReport follows the Throughput repo filter: the fleet block for
    // "All repos", otherwise that repo's own windows/history/stale list.
    function prtSLAReport() {
      const status = window._lastStatus || {};
      const fleet = status.sla || null;
      if (!fleet) return null;
      if (!prtRepo) return Object.assign({}, fleet, { stale_count: (fleet.stale_in_progress || []).length });
      const match = (status.repos || []).find(r => r && overviewRepoName(r).toLowerCase() === String(prtRepo).toLowerCase());
      if (!match || !match.sla) return Object.assign({}, fleet, { windows: {}, history: [], stale_in_progress: [], stale_count: 0, merged_7d: 0, missing: true });
      return Object.assign({}, match.sla, { targets: fleet.targets, claim_ttl_s: fleet.claim_ttl_s, stale_count: (match.sla.stale_in_progress || []).length });
    }

    function prtSLAIssueURL(repoShort, number) {
      const repos = (window._lastStatus || {}).repos || [];
      const match = repos.find(r => r && (r.name === repoShort || (r.full || '').split('/').pop() === repoShort));
      const full = (match && (match.full || match.name)) || repoShort;
      return `https://github.com/${full.split('/').map(encodeURIComponent).join('/')}/issues/${encodeURIComponent(String(number))}`;
    }

    function prtSLAStaleBadge(report) {
      const stale = (report && report.stale_in_progress) || [];
      const n = stale.length;
      const ttl = Number(report && report.claim_ttl_s);
      const ttlText = Number.isFinite(ttl) && ttl > 0 ? fmtSLAHours(ttl / 3600) : 'the claim TTL';
      const tip = n
        ? `${n} issue${n === 1 ? '' : 's'} marked stage/030-pr-in-progress for longer than ${ttlText} with no PR yet. Click to list them.`
        : `No issue has carried stage/030-pr-in-progress past ${ttlText} without a PR.`;
      if (n === 1) {
        const s0 = stale[0];
        return `<a class="prt-sla-stale" data-sla-state="breach" href="${esc(prtSLAIssueURL(s0.repo, s0.number))}" target="_blank" rel="noopener" title="${esc(tip)}" aria-label="${esc(tip)}">⚠ 1 stale in-progress · ${esc(s0.repo)}#${esc(String(s0.number))}</a>`;
      }
      return `<button type="button" class="prt-sla-stale" data-sla-state="${n ? 'breach' : 'none'}" data-action="togglePRThroughputSLADetail" data-stop="1" title="${esc(tip)}" aria-label="${esc(tip)}" aria-expanded="${_prtSLAExpanded ? 'true' : 'false'}">${n ? '⚠ ' : '✓ '}${n} stale in-progress</button>`;
    }

    function prtSLATile(span, report, targets) {
      const winKey = prtSLAWindow();
      const win = ((report && report.windows) || {})[winKey] || {};
      const stat = win[span.key] || {};
      const p50 = Number(stat.p50_h), p95 = Number(stat.p95_h), n = Number(stat.n) || 0;
      const target = Number(targets && targets[span.target]);
      const state = prtSLAState(p50, n, target);
      const hasTarget = Number.isFinite(target) && target > 0;
      const targetTip = hasTarget ? ` Target ${fmtSLAHours(target)}; green within target, amber up to ${PRT_SLA_WARN_RATIO}× target, red beyond.` : ' No target set for this span.';
      const tip = `${span.tip}${targetTip} p50 ${fmtSLAHours(n ? p50 : NaN)} · p95 ${fmtSLAHours(n ? p95 : NaN)} · n=${n} over ${winKey}.`;
      const value = n ? fmtSLAHours(p50) : '—';
      // No native title= on the tile: the ⓘ's .config-tooltip already carries
      // this exact text, and a title on an ancestor would surface a second,
      // browser-drawn copy while hovering the ⓘ.
      return `<div class="prt-metric" data-sla-state="${state}" data-sla-span="${esc(span.key)}"><div class="mval" data-prt-sla-p50="${Number.isFinite(p50) ? p50 : ''}">${esc(value)}</div><div class="mlbl">${esc(span.label)} p50${prtSLAInfoHTML(`prt-sla-tip-${span.key}`, span.label, tip)}</div><div class="prt-caption">${n ? `n=${n} · p95 ${esc(fmtSLAHours(p95))}` : esc(span.wait || 'n=0')}${hasTarget ? ` · <span class="prt-sla-target">target ${esc(fmtSLAHours(target))}</span>` : ''}</div>${prtSLASpark(span, report, state)}</div>`;
    }

    function prtSLARepoRows(repos, targets) {
      const rows = (repos || []).filter(r => r && r.sla).map(r => {
        const rep = r.sla;
        const win = (rep.windows || {})[prtSLAWindow()] || {};
        const cell = span => {
          const st = win[span.key] || {};
          const n = Number(st.n) || 0;
          const state = prtSLAState(Number(st.p50_h), n, Number(targets && targets[span.target]));
          return `<td data-sla-state="${state}" title="n=${n}">${n ? `${esc(fmtSLAHours(Number(st.p50_h)))} / ${esc(fmtSLAHours(Number(st.p95_h)))}` : '—'}</td>`;
        };
        const stale = (rep.stale_in_progress || []).length;
        const spark = prtSLASpark(PRT_SLA_SPANS.find(x => x.key === 'open_to_merge'), rep, 'none', 72) || '<span class="overview-kpi-subline">—</span>';
        return `<tr><td>${esc(overviewRepoName(r))}${stale ? ` <span class="prt-sla-stale" data-sla-state="breach" title="${stale} stale in-progress">⚠ ${stale}</span>` : ''}</td>${PRT_SLA_SPANS.filter(x => x.key !== 'open_to_merge').map(cell).join('')}<td>${Number(rep.merged_7d) || 0}</td><td>${spark}</td></tr>`;
      });
      if (!rows.length) return '<div class="overview-empty">No per-repo SLA data yet — spans appear as issues move start → PR → merge.</div>';
      return `<table class="prt-sla-table" aria-label="Delivery SLA per repo"><thead><tr><th>Repo</th>${PRT_SLA_SPANS.filter(x => x.key !== 'open_to_merge').map(x => `<th title="p50 / p95 over ${esc(prtSLAWindow())}">${esc(x.label)}</th>`).join('')}<th>Merged 7d</th><th>Open → merge 30d</th></tr></thead><tbody>${rows.join('')}</tbody></table>`;
    }

    function prtSLAStaleList(report) {
      const stale = (report && report.stale_in_progress) || [];
      if (!stale.length) return '';
      return `<div class="prt-sla-stale-list" aria-label="Stale in-progress issues"><span class="overview-kpi-subline">Stale in-progress:</span>${stale.map(s => `<a href="${esc(prtSLAIssueURL(s.repo, s.number))}" target="_blank" rel="noopener" title="started ${esc(s.started_at || '')}${s.agent ? ` by ${esc(s.agent)}` : ''}">${esc(s.repo)}#${esc(String(s.number))}</a>`).join('')}</div>`;
    }

    function renderThroughputSLA() {
      const report = prtSLAReport();
      if (!report) return '';
      const targets = report.targets || {};
      const winKey = prtSLAWindow();
      const tiles = PRT_SLA_SPANS.map(span => prtSLATile(span, report, targets)).join('');
      const repos = prtRepo ? ((window._lastStatus || {}).repos || []).filter(r => r && overviewRepoName(r).toLowerCase() === String(prtRepo).toLowerCase()) : ((window._lastStatus || {}).repos || []);
      const detail = _prtSLAExpanded
        ? `<div class="prt-sla-detail" id="prt-sla-detail">${prtSLAStaleList(report)}${prtSLARepoRows(repos, targets)}</div>`
        : '';
      const headTip = 'Delivery SLA: how long issues take to move from start to PR, PR to merge, and open to merge. Derived from the lifecycle timeline the hive already records — no extra GitHub calls. Percentiles use the 7d window, or 30d when the Throughput period is set to all; targets live in Settings → Governor → Thresholds.';
      return `<div class="prt-sla" data-prt-sla data-sla-window="${esc(winKey)}"><div class="prt-sla-head"><strong>Delivery SLA${prtSLAInfoHTML('prt-sla-tip-head', 'Delivery SLA', headTip)}</strong><span class="prt-sla-window">${esc(winKey)} percentiles · 30-day sparklines</span><span class="prt-sla-head-spacer"></span>${prtSLAStaleBadge(report)}<button type="button" class="hv-btn btn-secondary btn-sm" data-action="togglePRThroughputSLADetail" data-stop="1" aria-expanded="${_prtSLAExpanded ? 'true' : 'false'}" aria-controls="prt-sla-detail" title="${_prtSLAExpanded ? 'Hide' : 'Show'} the per-repo breakdown">${_prtSLAExpanded ? 'Repos ▴' : 'Repos ▾'}</button></div><div class="prt-sla-tiles">${tiles}</div>${detail}</div>`;
    }

    // prtSLASummary is the fragment appended to the collapsed Throughput line.
    function prtSLASummary(report) {
      const stat = (((report || {}).windows || {})[prtSLAWindow()] || {}).open_to_merge || {};
      const n = Number(stat.n) || 0;
      const stale = Number((report || {}).stale_count) || 0;
      const parts = [];
      if (n) parts.push(`open→merge p50 ${fmtSLAHours(Number(stat.p50_h))}`);
      if (stale) parts.push(`${stale} stale in-progress`);
      return parts.length ? ' · ' + parts.join(' · ') : '';
    }

    // refreshThroughputSLA re-renders only the SLA block when a status poll
    // lands between Throughput fetches, so the tiles track /api/status.
    function refreshThroughputSLA() {
      if (!prtLast) return;
      const card = document.getElementById('pr-throughput-card');
      const el = card && card.querySelector('[data-prt-sla]');
      const html = renderThroughputSLA();
      if (!el) { if (html) renderPRThroughput(prtLast); return; }
      if (html) el.outerHTML = html; else el.remove();
    }

    function togglePRThroughputSLADetail() {
      _prtSLAExpanded = !_prtSLAExpanded;
      if (prtLast) renderPRThroughput(prtLast);
    }

    async function fetchPRThroughput() {
      fetchEvidenceSummary();
      const hours = prtHours;
      const repo = prtRepo;
      const role = prtRole;
      try {
        let url = `/api/pr-throughput?hours=${encodeURIComponent(hours)}&role=${encodeURIComponent(role)}`;
        if (repo) url += `&repo=${encodeURIComponent(repo)}`;
        const r = await fetch(url);
        if (!r.ok) return;
        const dto = await r.json();
        if (hours !== prtHours || repo !== prtRepo || role !== prtRole) return; // a newer selection is in flight
        prtLast = dto;
        renderPRThroughput(dto);
      } catch (e) { /* transient — leave last render in place */ }
    }

    function setPRThroughputHours(hours) {
      const h = Number(hours);
      prtHours = PRT_WINDOWS.some(([v]) => v === h) ? h : 24;
      if (prtLast) renderPRThroughput(prtLast);
      return fetchPRThroughput();
    }
    function setPRThroughputRepo(repo) {
      prtRepo = String(repo || '').trim();
      try { prtRepo ? localStorage.setItem(PRT_REPO_STORAGE_KEY, prtRepo) : localStorage.removeItem(PRT_REPO_STORAGE_KEY); } catch {}
      if (prtLast) renderPRThroughput(prtLast);
      return fetchPRThroughput();
    }
    function setPRThroughputRole(role) {
      prtRole = ['created','reviewed','merged','closed'].includes(String(role)) ? String(role) : 'merged';
      return fetchPRThroughput();
    }
    function setPRThroughputTrendView(view) {
      prtTrendView = PRT_TREND_VIEWS.some(([v]) => v === String(view)) ? String(view) : 'counts';
      try { localStorage.setItem(PRT_TREND_VIEW_STORAGE_KEY, prtTrendView); } catch {}
      if (prtLast) renderPRThroughput(prtLast);
    }
    function exportPRThroughputCSV() {
      const dto = prtLast || {};
      const rows = [['t','opened','merged','closed']].concat((dto.buckets || []).map(b => [b.t || '', b.opened || 0, b.merged || 0, b.closed || 0]));
      const csv = rows.map(r => r.map(v => `"${String(v).replace(/"/g, '""')}"`).join(',')).join('\n') + '\n';
      const name = 'pr-throughput-' + (prtRepo ? prtRepo.replace(/[^a-z0-9._-]+/ig, '-') : 'all') + '.csv';
      hiveExportViewer({title:name,filename:name,text:csv,mime:'text/csv'});
    }
    window.setPRThroughputHours = setPRThroughputHours;
    window.setPRThroughputRepo = setPRThroughputRepo;
    window.setPRThroughputRole = setPRThroughputRole;
    window.setPRThroughputTrendView = setPRThroughputTrendView;
    window.exportPRThroughputCSV = exportPRThroughputCSV;


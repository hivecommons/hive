    // ── v4: ACMM level-up recommendation (advisory) + lifecycle timeline ──
    // Both are additive, read-only panels polled on their own modest interval.
    // They fail quietly: a transient error leaves the last render in place and
    // never blocks the main status poll. Every JSON field is guarded so a
    // freshly-booted hive (nil status, empty timeline) renders calmly, not a
    // stack trace.
    const V4_PANELS_POLL_MS = 20000;

    // Format a UnixMilli timestamp as a compact relative "time ago" string.
    function v4RelTime(ms) {
      const n = lcParseTime(ms);
      if (!Number.isFinite(n) || n <= 0) return '';
      const diff = Date.now() - n;
      if (diff < 0) return 'just now';
      const sec = Math.floor(diff / 1000);
      if (sec < 60) return sec + 's ago';
      const min = Math.floor(sec / 60);
      if (min < 60) return min + 'm ago';
      const hr = Math.floor(min / 60);
      if (hr < 24) return hr + 'h ago';
      const day = Math.floor(hr / 24);
      return day + 'd ago';
    }

    // Map a lifecycle event kind to a badge color class.
    function v4KindClass(kind) {
      switch (kind) {
        case 'kicked':
        case 'started':
        case 'pr_opened': return 'blue';
        case 'merged': return 'green';
        case 'blocked': return 'red';
        default: return 'neutral'; // enumerated / classified / unknown
      }
    }

    // PANEL A — "Ready to level up?" — advisory ACMM recommendation.
    let _platformStatus = null;
    function platformFactTiles(plat) {
      if (!plat) return '<div class="muted" style="font-size:var(--fs-sm)">No platform status reported yet.</div>';
      const tiles = [];
      const forge = plat.forge || {};
      const forgeKind = (forge.kind || 'github').toLowerCase();
      const FORGE_META = {
        github: { icon: '🐙', name: 'GitHub' },
        gitlab: { icon: '🦊', name: 'GitLab' },
        gitea:  { icon: '🍵', name: 'Gitea' }
      };
      const fm = FORGE_META[forgeKind] || { icon: '🔀', name: forgeKind || 'Forge' };
      const repoCount = Number(forge.repoCount) || 0;
      const forgeSub = forge.instanceUrl
        ? escapeHtml(forge.instanceUrl.replace(/^https?:\/\//, ''))
        : (repoCount > 0 ? repoCount + ' repo' + (repoCount === 1 ? '' : 's') : 'Forge');
      const forgeTitle = forge.instanceUrl
        ? escapeHtml(forge.instanceUrl) + ' — ' + repoCount + ' repo' + (repoCount === 1 ? '' : 's')
        : repoCount + ' repo' + (repoCount === 1 ? '' : 's') + ' monitored';
      tiles.push(`<div class="kb-stat" title="${forgeTitle}"><div class="val">${fm.icon} ${escapeHtml(fm.name)}</div><div class="lbl">${forgeSub}</div></div>`);

      const mint = plat.mint || {};
      if (mint.enabled) {
        const keyMark = mint.keyPresent ? 'key ✓' : 'key —';
        const issuer = mint.issuer ? escapeHtml(mint.issuer.replace(/^https?:\/\//, '')) : keyMark;
        tiles.push(`<div class="kb-stat" title="Mint token service enabled${mint.issuer ? ' — issuer ' + escapeHtml(mint.issuer) : ''}; signing ${mint.keyPresent ? 'key present' : 'key not found'}"><div class="val text-success">Mint on</div><div class="lbl">${issuer} · ${keyMark}</div></div>`);
      } else {
        tiles.push('<div class="kb-stat" title="Mint token service disabled"><div class="val text-muted">Mint off</div><div class="lbl">token service</div></div>');
      }

      const skills = plat.skills || {};
      if (skills.available) {
        const loaded = Number(skills.loaded) || 0;
        tiles.push(`<div class="kb-stat" title="Skills registry loaded from ${escapeHtml(skills.dir || 'the skills directory')}"><div class="val">${loaded}</div><div class="lbl">skill${loaded === 1 ? '' : 's'} loaded</div></div>`);
      } else {
        tiles.push('<div class="kb-stat" title="No skills directory configured"><div class="val text-muted">—</div><div class="lbl">skills not configured</div></div>');
      }
      return '<div class="kb-stat-grid platform-diagnostics-grid">' + tiles.join('') + '</div>';
    }
    function renderPlatform(plat) {
      _platformStatus = plat || null;
      renderDebugSection();
    }


    function renderHiveAdvice(dto) {
      refreshSectionCardShell('hive-advice-section');
      const section = document.getElementById('hive-advice-section');
      const card = document.getElementById('hive-advice-card');
      if (!section || !card) return;
      dto = dto || {};
      _lastHiveAdvice = dto;
      const recs = Array.isArray(dto.recommendations) ? dto.recommendations : [];
      setSectionSummary('advisory-section', `${recs.length} advice item${recs.length === 1 ? '' : 's'} this week`, 'Hive Advice');
      setSectionSummary('hive-advice-section', `${recs.length} recommendation${recs.length === 1 ? '' : 's'}`);
      if (recs.length === 0) { section.style.display = 'none'; return; }
      const epoch = dto.epoch || {};
      // Which items show is picked once a week (the epoch) and kept so the
      // list does not jump around; what is under them is recomputed on every
      // status refresh. A change of governor mode picks a new list early.
      const mode = escapeHtml(epoch.mode || 'IDLE');
      const picked = epoch.start ? escapeHtml(formatDashboardDate(epoch.start)) : '';
      const until = epoch.end ? escapeHtml(formatDashboardDate(epoch.end)) : '';
      const kept = picked && until ? `Hive picked this list on ${picked} and keeps it until ${until}` : until ? `Hive keeps this list until ${until}` : 'Hive keeps this list for a week';
      const review = `${kept}, so it does not jump around. The numbers under each item update every few minutes. If Hive's mode changes from ${mode}, it picks a new list sooner.`;
      const counts = dto.counts || {};
      const countChip = (label, tip, prs, issues, issueLabel) => `<div class="hive-advice-count-chip" title="${escapeHtml(tip)}"><dt>${escapeHtml(label)}</dt><dd><span class="count-value">${escapeHtml(String(prs ?? 0))}</span><span class="count-label">${escapeHtml(label === 'Governor queue' ? 'PRs' : 'open PRs')}</span><span class="count-value">${escapeHtml(String(issues ?? 0))}</span><span class="count-label">${escapeHtml(issueLabel)}</span></dd></div>`;
      const countsHtml = `<dl class="hive-advice-counts" aria-label="Advice count scopes">${countChip('Governor queue', 'Governor actionable queue: mode & cadence', counts.governorPRs, counts.governorIssues, 'issues')}${countChip('Overview chart', 'Overview chart: every open or held item; queue-health advice counts these', counts.overviewPRs, counts.overviewIssues, 'issues')}</dl>`;
      const items = recs.map((r, i) => {
        r = r || {};
        if (r.cleared) {
          return `<li class="reco-crit met" id="hive-advice-rec-${i}">
          <span class="mark">✓</span>
          <div class="crit-body"><div class="crit-name"><s>${escapeHtml(r.title || r.id || 'Recommendation')}</s></div>
          <div class="crit-detail">${escapeHtml(r.rationale || '')}</div></div>
        </li>`;
        }
        // The raw signals are the evidence behind the numbers, not advice:
        // folded away so the card reads as why / how to help / where to start.
        const sigs = Array.isArray(r.signals) ? r.signals : [];
        const sigHtml = sigs.length ? `<details class="hive-advice-signals"><summary>Numbers behind this</summary>${sigs.map((s) => `${escapeHtml(s.name || '')}=${escapeHtml(s.value || '')}`).join(', ')}</details>` : '';
        const why = Array.isArray(r.why) ? r.why : [];
        const whyHtml = why.length ? `<div class="hive-advice-sub">Why they wait</div><ul class="hive-advice-why">${why.map((w) => `<li>${escapeHtml(w)}</li>`).join('')}</ul>` : '';
        const helpHtml = r.help ? `<div class="hive-advice-help"><strong>How to help:</strong> ${escapeHtml(r.help)}.</div>` : '';
        const its = Array.isArray(r.items) ? r.items : [];
        const itemHtml = its.length ? `<div class="hive-advice-sub">First</div><ul class="hive-advice-items">${its.map((it) => {
          const ref = `${escapeHtml(it.repo || '')}#${escapeHtml(String(it.number || ''))}`;
          const link = it.url ? `<a href="${escapeHtml(it.url)}" target="_blank" rel="noopener noreferrer">${ref}</a>` : ref;
          return `<li>${it.note ? `${link} — ${escapeHtml(it.note)}` : link}</li>`;
        }).join('')}</ul>` : '';
        const links = Array.isArray(r.links) ? r.links : [];
        const linkHtml = links.length ? `<div class="crit-cmp">Full list: ${links.map((l) => `<a href="${escapeHtml(l.url || '')}" target="_blank" rel="noopener noreferrer">${escapeHtml(l.label || l.url || '')}</a>`).join(', ')}</div>` : '';
        return `<li class="reco-crit met" id="hive-advice-rec-${i}">
          <span class="mark">${i + 1}</span>
          <div class="crit-body"><div class="crit-name">${escapeHtml(r.title || r.id || 'Recommendation')}</div>
          <div class="crit-detail">${escapeHtml(r.rationale || '')}</div>${whyHtml}${helpHtml}${itemHtml}${linkHtml}${sigHtml}</div>
        </li>`;
      }).join('');
      const bands = Array.isArray(dto.bands) ? dto.bands : [];
      const bandPillClass = (b) => {
        const key = String((b && b.key) || '').replace(/^(issue|pr)\//, '');
        if ((b && b.kind) === 'issue') return 'repo-issue-pill' + (key === 'ready' ? '' : ' ' + key);
        if (key === 'eligible') return 'repo-pr-pill mergeable';
        if (key === 'blocked') return 'repo-pr-pill merge-outstanding';
        return 'repo-pr-pill' + (key === 'waiting' ? ' held' : '');
      };
      const bandRows = bands.map((b) => `<dt><span class="${escapeHtml(bandPillClass(b))}">${escapeHtml(b.label || b.key || '')}</span></dt><dd>${escapeHtml(b.rule || '')}</dd>`).join('');
      const bandOpen = bands.length > 2 ? '' : ' open';
      const bandHtml = bands.length ? `<details class="hive-advice-bands"${bandOpen}><summary><span class="hive-advice-band-title">Band definitions</span><span class="hive-advice-band-toggle" aria-hidden="true"></span></summary><dl class="hive-advice-band-list">${bandRows}</dl></details>` : '';
      section.style.display = '';
      card.innerHTML = `<div class="reco-level-flow"><span class="reco-badge raise">owner advice</span><span class="reco-rationale hive-advice-review">${review}</span></div>${countsHtml}<ul class="reco-checklist">${items}</ul>${bandHtml}`;
      applySectionCollapse('hive-advice-section');
    }



    function renderFleetReport(dto) {
      refreshSectionCardShell('fleet-report-section');
      const section = document.getElementById('fleet-report-section');
      const card = document.getElementById('fleet-report-card');
      if (!section || !card) return;
      dto = dto || {};
      const reports = Array.isArray(dto.reports) ? dto.reports : [];
      const recoveries = Array.isArray(dto.recoveries) ? dto.recoveries : [];
      const selfReports = fleetReportSelfReportRow(dto);
      setSectionSummary('fleet-report-section', `${reports.length} reports · ${recoveries.length} recovered${selfReports.count ? ` · ${selfReports.count} filed` : ''}`);
      if (reports.length === 0 && recoveries.length === 0 && selfReports.count === 0) { section.style.display = 'none'; return; }
      const dryRun = dto.dry_run !== false;
      const mk = (r, i, recovered) => {
        r = r || {};
        const evidence = Array.isArray(r.evidence) ? r.evidence : [];
        const ev = evidence[0] || null;
        const trigger = r.trigger || (r.criterion ? 'acmm-shortfall' : 'runtime-error');
        const subject = r.criterion || fleetReportClassLabel(r.class) || trigger;
        const title = recovered ? 'Recovered: ' : '';
        // A recovery carries no evidence by construction (fleetreport.go
        // RecoveryReport builds it from fingerprint/instance/criterion/trigger
        // only) — it records that a symptom STOPPED being observed. Rendering
        // the evidence line anyway printed every fallback at once, as
        // "medium hive / unknown; count 0 in n/a", which reads like measured
        // data. Say what the backend already says in Body instead.
        let detail = '';
        if (recovered) {
          detail = r.criterion
            ? `No longer observing the ACMM shortfall for ${escapeHtml(r.criterion)}.`
            : 'No longer observing the reported runtime condition.';
        } else if (ev) {
          const periodic = ev.periodicity ? `; periodic ${escapeHtml(ev.periodicity)}` : '';
          const where = ev.window ? ` in ${escapeHtml(ev.window)}` : '';
          const count = Number(ev.count) > 0 ? `; count ${escapeHtml(String(ev.count))}${where}` : '';
          detail = `${escapeHtml(ev.severity || 'medium')} ${escapeHtml(ev.component || 'hive')} / ${escapeHtml(ev.error_class || 'unknown')}${count}${periodic}`;
        }
        const fp = r.fingerprint || '';
        const inst = r.instance_id || '';
        const ids = (fp || inst) ? ` title="fingerprint: ${escapeHtml(fp)}&#10;instance: ${escapeHtml(inst)}"` : '';
        return `<li class="reco-crit ${recovered ? 'met' : 'unmet'}"${ids}>
          <span class="mark">${recovered ? '✓' : i + 1}</span>
          <div class="crit-body"><div class="crit-name">${title}${escapeHtml(subject)}</div>
          ${detail ? `<div class="crit-detail">${detail}</div>` : ''}
          <div class="crit-cmp">trigger: ${escapeHtml(trigger)}${r.rollup_of ? '; rolls up onto open self-report' : ''}${fp ? `; fingerprint ${escapeHtml(fp.slice(0, 12))}…` : ''}</div></div>
        </li>`;
      };
      const reportHtml = reports.map((r, i) => mk(r, i, false)).join('');
      const recoveryHtml = recoveries.map((r, i) => mk(r, i, true)).join('');
      section.style.display = '';
      card.innerHTML = `<div class="reco-level-flow"><span class="reco-badge ${dryRun ? 'stay' : 'raise'}">${dryRun ? 'dry-run preview' : 'filing enabled'}</span><span class="reco-rationale">Persistent ACMM shortfalls and hive-owned runtime conditions (stalls, crash loops, kick failures, panics, stuck relay requests and ledger ops, repeated unclassified errors). Reports contain shape-only scrubbed evidence.</span></div>${selfReports.html}<ul class="reco-checklist">${reportHtml}${recoveryHtml}</ul>`;
      applySectionCollapse('fleet-report-section');
    }

    // fleetReportClassLabel renders a runtime-error class id as the label the
    // operator reads on the self-report card; classes without an entry keep
    // their id (pkg/fleetreport/runtime.go Classes is the source of truth).
    const FLEET_REPORT_CLASS_LABELS = {
      restart_storm: 'restart storm',
      kick_failed_repeatedly: 'kicks failing repeatedly',
      panic_recovered: 'panic recovered',
      model_discovery_degraded: 'model discovery degraded',
      inference_auth_failed: 'inference auth failed',
      advisory_post_failed: 'advisory post failed',
      agent_stall: 'agent stall',
      agent_crash_loop: 'agent crash loop',
      ci_poll_nudge: 'CI-poll nudge',
      issue_request_stuck: 'issue request stuck in relay queue',
      effect_ledger_op_stuck: 'effect ledger op stuck (needs reconciliation)',
      unclassified_error_repeated: 'repeated unclassified error',
    };
    function fleetReportClassLabel(cls) {
      if (!cls) return '';
      const label = FLEET_REPORT_CLASS_LABELS[cls];
      return label ? `${label} (${cls})` : cls;
    }

    // fleetReportSelfReportRow summarises the hivecommons/hive issues this
    // hive has filed or joined (hivecommons/hive#11868): "Self-reports: N
    // open (last: <class> → #NNN)". Every value comes from GitHub or the
    // local state file and is escaped; links open the issue in a new tab.
    function fleetReportSelfReportRow(dto) {
      const open = (dto && dto.state && dto.state.open) || {};
      const entries = Object.keys(open).map(fp => Object.assign({ fingerprint: fp }, open[fp] || {}))
        .filter(e => Number(e.number) > 0 && !e.recovered && !e.closed_at);
      if (entries.length === 0) return { count: 0, html: '' };
      entries.sort((x, y) => String(y.posted_at || '').localeCompare(String(x.posted_at || '')));
      const last = entries[0];
      const lastLabel = `${escapeHtml(last.class || last.criterion || last.trigger || 'report')} → ${last.url ? `<a href="${escapeHtml(last.url)}" target="_blank" rel="noopener">#${escapeHtml(String(last.number))}</a>` : `#${escapeHtml(String(last.number))}`}${last.joined ? ' (joined)' : ''}`;
      const links = entries.slice(0, 8).map(e => e.url
        ? `<a href="${escapeHtml(e.url)}" target="_blank" rel="noopener" title="${escapeHtml(e.class || e.trigger || '')}">#${escapeHtml(String(e.number))}</a>`
        : `#${escapeHtml(String(e.number))}`).join(', ');
      return { count: entries.length, html: `<div class="crit-detail" data-fleet-selfreports>Self-reports: <strong>${entries.length} open</strong> (last: ${lastLabel})${entries.length > 1 ? ` · ${links}` : ''}</div>` };
    }

    async function fetchHiveAdvice() {
      try {
        const r = await fetch('/api/hive-advice');
        if (!r.ok) return;
        const dto = await r.json();
        renderHiveAdvice(dto);
      } catch (e) { /* transient — leave last render in place */ }
    }

    async function toggleACMMRepoPin(repo, pinned) {
      try {
        await fetch('/api/acmm-recommendation/repo-pin', {
          method: 'PUT',
          headers: {'Content-Type':'application/json'},
          body: JSON.stringify({ repo, pinned })
        });
        fetchACMMReco();
      } catch (e) { /* transient; next refresh will reconcile */ }
    }

    function renderACMMReco(rec) {
      refreshSectionCardShell('acmm-reco-section');
      const section = document.getElementById('acmm-reco-section');
      const card = document.getElementById('acmm-reco-card');
      if (!section || !card) return;
      rec = rec || {};
      const advise = rec.Advise === 'raise' ? 'raise' : 'stay';
      const cur = Number.isFinite(Number(rec.CurrentLevel)) ? Number(rec.CurrentLevel) : null;
      const tgt = Number.isFinite(Number(rec.TargetLevel)) ? Number(rec.TargetLevel) : null;
      const met = Array.isArray(rec.Met) ? rec.Met : [];
      const unmet = Array.isArray(rec.Unmet) ? rec.Unmet : [];
      const signals = Array.isArray(rec.AutonomySignals) ? rec.AutonomySignals : [];
      const levels = Array.isArray(rec.AutonomyLevels) ? rec.AutonomyLevels : [];
      const acmmSummary = cur !== null
        ? `L${cur}${advise === 'raise' && tgt !== null ? ' → L' + tgt : ''} · ${advise}`
        : `${advise}`;
      setSectionSummary('acmm-reco-section', acmmSummary);
      const levelsHtml = levels.length
        ? `<div class="reco-fact-heading">Automatic autonomy level decisions</div><ul class="reco-checklist">` + levels.map((l) => {
          l = l || {};
          const repo = l.repo || 'repo unknown';
          const level = Number.isFinite(Number(l.level)) ? `L${Number(l.level)}` : 'level unknown';
          const pinned = !!l.pinned;
          const evidence = Array.isArray(l.evidenceIds) && l.evidenceIds.length ? `evidence: ${l.evidenceIds.map(escapeHtml).join(', ')}` : 'no automatic evidence yet';
          const change = l.lastChange ? `last ${escapeHtml(l.direction || 'change')} at ${escapeHtml(l.lastChange)}` : 'no automatic change yet';
          return `<li class="reco-crit ${pinned ? 'unmet' : 'met'}">
            <span class="mark">${pinned ? '📌' : '✓'}</span>
            <div class="crit-body"><div class="crit-name">${escapeHtml(repo)} current automatic level ${escapeHtml(level)}</div>
            <div class="crit-detail">${change}</div><div class="crit-cmp">${evidence}</div>
            <button class="reco-action" type="button" data-action="toggleACMMRepoPin" data-repo="${escapeHtml(repo)}" data-pinned="${!pinned}">${pinned ? 'Unpin' : 'Pin'} automatic movement</button></div>
          </li>`;
        }).join('') + `</ul>`
        : '';
      const signalHtml = signals.length
        ? `<div class="reco-fact-heading">Retro autonomy signal facts</div><ul class="reco-checklist">` + signals.map((s) => {
          s = s || {};
          const dir = s.direction || '';
          const mark = dir === 'should lose' ? '!' : '✓';
          const cls = dir === 'should lose' ? 'unmet' : 'met';
          const scope = [s.scopeType, s.scope].filter(Boolean).join(': ') || 'scope unknown';
          const level = s.level || 'level pending';
          const detail = s.detail ? `<div class="crit-detail">${escapeHtml(s.detail)}</div>` : '';
          return `<li class="reco-crit ${cls}">
            <span class="mark">${mark}</span>
            <div class="crit-body"><div class="crit-name">${escapeHtml(scope)} ${escapeHtml(dir || 'signals')} ${escapeHtml(level)}</div>
            ${detail}<div class="crit-cmp">fact recorded for the default-off automatic promotion/demotion policy.</div></div>
          </li>`;
        }).join('') + `</ul>`
        : `<div class="reco-fact-heading">Retro autonomy signal facts</div><div class="reco-calm">No qualifying autonomy signals right now; that is an acceptable terminal state.</div>`;

      // Minimal calm state: no criteria to show and already advising stay
      // (e.g. topped out at L6 or a nil-status hive). Keep it quiet.
      if (advise === 'stay' && met.length === 0 && unmet.length === 0) {
        section.style.display = '';
        const flow = (cur !== null)
          ? `<div class="reco-level-flow">L${cur} <span class="reco-badge stay">stay</span></div>`
          : '';
        card.innerHTML = `${flow}<div class="reco-calm">${escapeHtml(rec.Rationale || 'No level-up recommendation right now.')}</div>${levelsHtml}${signalHtml}`;
        applySectionCollapse('acmm-reco-section');
        return;
      }

      section.style.display = '';
      const badgeLabel = advise === 'raise' ? 'raise' : 'stay';
      const flowHtml = (cur !== null && tgt !== null)
        ? `<div class="reco-level-flow">L${cur} <span class="arrow">→</span> L${tgt} <span class="reco-badge ${advise}">${badgeLabel}</span></div>`
        : `<div class="reco-level-flow"><span class="reco-badge ${advise}">${badgeLabel}</span></div>`;

      const critHtml = (list, isMet) => list.map((c) => {
        c = c || {};
        const name = escapeHtml(c.Name || '');
        const cmp = escapeHtml(c.Comparator || '');
        const req = escapeHtml(c.Required || '');
        const act = escapeHtml(c.Actual || '');
        const detail = escapeHtml(c.Detail || '');
        const cmpLine = (req !== '' || act !== '')
          ? `<div class="crit-cmp"><span class="actual">${act || '—'}</span> ${cmp} ${req || '—'}</div>`
          : '';
        const detailLine = detail ? `<div class="crit-detail">${detail}</div>` : '';
        return `<li class="reco-crit ${isMet ? 'met' : 'unmet'}">
          <span class="mark">${isMet ? '✓' : '✗'}</span>
          <div class="crit-body"><div class="crit-name">${name}</div>${cmpLine}${detailLine}</div>
        </li>`;
      }).join('');

      const readyHtml = rec.Ready
        ? `<div class="reco-ready">✓ Eligible to raise${tgt !== null ? ` to L${tgt}` : ''} — a human approves the change.</div>`
        : '';
      const rationaleHtml = rec.Rationale
        ? `<div class="reco-rationale">${escapeHtml(rec.Rationale)}</div>`
        : '';
      const checklistHtml = (unmet.length || met.length)
        ? `<ul class="reco-checklist">${critHtml(unmet, false)}${critHtml(met, true)}</ul>`
        : '';

      card.innerHTML = `${flowHtml}${rationaleHtml}${readyHtml}${checklistHtml}
        ${levelsHtml}
        ${signalHtml}
        <div class="reco-advisory-note">Advisor recommendations are still human-approved; the separate autonomy policy only changes levels when explicitly enabled.</div>`;
      applySectionCollapse('acmm-reco-section');
    }

    async function fetchACMMReco() {
      try {
        const r = await fetch('/api/acmm-recommendation');
        if (!r.ok) return;
        const rec = await r.json();
        renderACMMReco(rec);
      } catch (e) { /* transient — leave last render in place */ }
    }

    // Short chip labels for the journey stage axis; keys are the closed Kind
    // set served by /api/lifecycle-timeline.
    const LC_STAGE_AXIS = ['enumerated', 'classified', 'kicked', 'started', 'pr_opened', 'merged', 'blocked'];
    const LC_STAGE_LABEL = {
      enumerated: 'enum', classified: 'class', kicked: 'kick', started: 'start',
      pr_opened: 'pr', merged: 'merged', blocked: 'blocked'
    };

    // Format the fleet-health coverage note: counts are honest only relative
    // to how much history the store actually holds (coveredMs), which can be
    // shorter than the requested window right after first deploy.
    function lcWindowLabel(fleet) {
      const windowMs = Number(fleet.windowMs) || 0;
      const coveredMs = Number(fleet.coveredMs) || 0;
      const fmt = (ms) => {
        const min = Math.round(ms / 60000);
        if (min < 60) return min + 'm';
        const hr = Math.round(min / 60);
        if (hr < 48) return hr + 'h';
        return Math.round(hr / 24) + 'd';
      };
      if (!windowMs) return '';
      if (coveredMs > 0 && coveredMs < windowMs) {
        return `over the last ${fmt(coveredMs)} of recorded history (window ${fmt(windowMs)})`;
      }
      return `over the last ${fmt(windowMs)}`;
    }

    const lcExpandedJourneys = new Set();
    const LC_COLLAPSED_ROW_LIMIT = 40;
    const LC_BAR_MIN_WIDTH_PCT = 1.5;
    const LC_MARKER_MERGE_PCT = 1.1;
    let lcLastDTO = null;
    let lcScrollTop = 0;
    let lcShowAllRows = false;
    let lcResizeObserver = null;
    let lcLastObservedWidth = 0;
    let lcLayoutRAF = 0;

    function lcJourneyKey(j) {
      return String((j && (j.id || j.ref)) || '');
    }

    function lcJourneyHasContent(j) {
      if (!j) return false;
      const stages = j.stages || {};
      return !!(j.ref || j.id || j.current || j.agent || j.lastAt || Object.keys(stages).length);
    }

    function lcStageLabel(kind) {
      const labels = {
        enumerated: 'Enumerated', classified: 'Classified', kicked: 'Kicked', started: 'Started', pr_opened: 'PR opened',
        merged: 'Merged', blocked: 'Blocked', stage_completed: 'Stage completed', stage_receipt: 'Stage receipt',
        stage_approval: 'Stage approval'
      };
      return labels[kind] || String(kind || '').replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase());
    }

    function lcAbsTime(ms) {
      const n = lcParseTime(ms) || 0;
      if (!n) return '';
      try { return formatDashboardDateTime(n); } catch (e) { return ''; }
    }

    function lcParseTime(value) {
      if (value === undefined || value === null || value === '') return 0;
      if (value instanceof Date) {
        const ms = value.getTime();
        return Number.isFinite(ms) ? ms : 0;
      }
      if (typeof value === 'string') {
        const trimmed = value.trim();
        if (trimmed === '') return 0;
        if (!/^-?\d+(\.\d+)?$/.test(trimmed)) {
          const parsed = Date.parse(trimmed);
          return Number.isFinite(parsed) ? parsed : 0;
        }
        value = Number(trimmed);
      }
      const n = Number(value);
      if (!Number.isFinite(n) || n <= 0) return 0;
      if (n < 100000000000) return Math.round(n * 1000);
      if (n > 100000000000000) return Math.round(n / 1000000);
      return Math.round(n);
    }

    function lcStageLastAt(stage) {
      return lcParseTime(stage && (stage.lastAt ?? stage.LastAt ?? stage.at));
    }

    function lcJourneyFirstAt(j) {
      const direct = lcParseTime(j && (j.firstAt ?? j.FirstAt));
      if (direct) return direct;
      const stages = (j && j.stages) || {};
      return Object.keys(stages).reduce((min, k) => {
        const st = stages[k] || {};
        const at = lcParseTime(st.firstAt ?? st.FirstAt ?? st.lastAt ?? st.LastAt);
        return at && (!min || at < min) ? at : min;
      }, 0);
    }

    function lcJourneyLastAt(j) {
      const direct = lcParseTime(j && (j.lastAt ?? j.LastAt));
      if (direct) return direct;
      const stages = (j && j.stages) || {};
      return Object.keys(stages).reduce((max, k) => Math.max(max, lcStageLastAt(stages[k])), 0);
    }

    function lcStateClass(current) {
      if (current === 'merged') return 'merged';
      if (current === 'blocked') return 'blocked';
      return 'inflight';
    }

    function lcTitleFromJourney(j) {
      const attrs = Object.assign({},
        (j && j.details && j.details.attrs) || {},
        ...Object.values((j && j.stages) || {}).map((st) => (st && st.attrs) || {}));
      return attrs.pr_title || attrs.pull_title || attrs.issue_title || attrs.title || attrs.subject || '';
    }

    function lcPRLabel(j) {
      const d = (j && j.details) || {};
      const links = Array.isArray(d.links) ? d.links : [];
      const prLink = links.find((l) => l && l.kind === 'pr' && l.number);
      if (prLink) return `PR #${prLink.number}`;
      const attrs = Object.assign({},
        d.attrs || {},
        ...Object.values((j && j.stages) || {}).map((st) => (st && st.attrs) || {}));
      const pr = attrs.pr_number || attrs.pr || attrs.pull_number || attrs.pull_request;
      return pr ? `PR #${String(pr).replace(/^#/, '')}` : (j.ref || '');
    }

    function lcTimelineBounds(journeys) {
      const times = [];
      journeys.forEach((j) => {
        const first = lcJourneyFirstAt(j);
        const last = lcJourneyLastAt(j);
        if (first) times.push(first);
        if (last) times.push(last);
      });
      if (times.length === 0) {
        const now = Date.now();
        return { start: now - 6 * 3600000, end: now };
      }
      let start = Math.min(...times);
      let end = Math.max(...times);
      if (end <= start) end = start + 60000;
      const pad = Math.max(60000, Math.round((end - start) * 0.04));
      return { start: start - pad, end: end + pad };
    }

    function lcPct(ms, bounds) {
      const span = Math.max(1, bounds.end - bounds.start);
      return Math.max(0, Math.min(100, ((ms - bounds.start) / span) * 100));
    }

    function lcAxisHtml(bounds) {
      const ticks = [0, 0.25, 0.5, 0.75, 1].map((frac) => {
        const at = bounds.start + (bounds.end - bounds.start) * frac;
        const label = formatDashboardTime(at, { hour:'2-digit', minute:'2-digit', timeZoneName:'short' });
        const x = (frac * 100).toFixed(2);
        return { line: `<line class="lc-axis-tick-line" x1="${x}" x2="${x}" y1="10" y2="20"></line>`, label: `<span class="lc-axis-tick-label" style="left:${x}%">${escapeHtml(label)}</span>` };
      });
      const lines = ticks.map((t) => t.line).join('');
      const labels = ticks.map((t) => t.label).join('');
      return `<div class="lc-axis" aria-hidden="true"><div>Work item</div><div class="lc-axis-track"><svg class="lc-axis-svg" viewBox="0 0 100 24" preserveAspectRatio="none">${lines}</svg>${labels}</div><div class="lc-axis-meta">Status</div></div>`;
    }

    function lcBarHtml(j, bounds) {
      const start = lcJourneyFirstAt(j) || bounds.start;
      const end = lcJourneyLastAt(j) || start;
      const left = lcPct(start, bounds);
      const right = lcPct(end, bounds);
      const width = Math.max(LC_BAR_MIN_WIDTH_PCT, right - left);
      const cls = lcStateClass(j.current || '');
      const title = [
        lcPRLabel(j),
        lcTitleFromJourney(j),
        lcStageLabel(j.current || 'in-flight'),
        `${lcAbsTime(start)} → ${lcAbsTime(end)}`
      ].filter(Boolean).join(' · ');
      const grid = [25, 50, 75].map((x) => `<line class="lc-grid-line" x1="${x}" x2="${x}" y1="0" y2="24"></line>`).join('');
      const markerPoints = [{ x: left, label: `Start · ${lcAbsTime(start)}` }];
      if (Math.abs(right - left) >= LC_MARKER_MERGE_PCT) markerPoints.push({ x: right, label: `End · ${lcAbsTime(end)}` });
      Object.keys((j && j.stages) || {}).forEach((k) => {
        const at = lcStageLastAt(j.stages[k]);
        if (!at) return;
        markerPoints.push({ x: lcPct(at, bounds), label: lcStageLabel(k) + ' · ' + lcAbsTime(at) });
      });
      const mergedMarkers = [];
      markerPoints
        .filter((m) => Number.isFinite(m.x))
        .sort((a, b) => a.x - b.x)
        .forEach((m) => {
          const prev = mergedMarkers[mergedMarkers.length - 1];
          if (prev && Math.abs(m.x - prev.x) < LC_MARKER_MERGE_PCT) {
            prev.x = (prev.x + m.x) / 2;
            if (m.label && !prev.labels.includes(m.label)) prev.labels.push(m.label);
          } else {
            mergedMarkers.push({ x: m.x, labels: m.label ? [m.label] : [] });
          }
        });
      const markers = mergedMarkers.map((m) => {
        const markerTitle = m.labels.join(' · ');
        return `<span class="lc-marker" data-lc-marker-left="${m.x.toFixed(2)}" title="${escapeHtml(markerTitle)}" aria-hidden="true"></span>`;
      }).join('');
      return `<div class="lc-track" title="${escapeHtml(title)}"><svg class="lc-track-svg" viewBox="0 0 100 24" preserveAspectRatio="none" aria-hidden="true">${grid}<rect class="lc-bar ${cls}" x="${left.toFixed(2)}" y="6" width="${width.toFixed(2)}" height="12" rx="3" ry="3"><title>${escapeHtml(title)}</title></rect></svg>${markers}</div>`;
    }

    function lcApplyMarkerPositions(root) {
      if (!root || typeof root.querySelectorAll !== 'function') return;
      root.querySelectorAll('.lc-marker[data-lc-marker-left]').forEach((marker) => {
        const x = Number(marker.getAttribute('data-lc-marker-left'));
        if (Number.isFinite(x)) marker.style.setProperty('--lc-marker-left', x.toFixed(2) + '%');
      });
    }

    function lcScheduleLayout() {
      if (lcLayoutRAF) return;
      const raf = typeof requestAnimationFrame === 'function' ? requestAnimationFrame : (fn) => setTimeout(fn, 0);
      lcLayoutRAF = raf(() => {
        lcLayoutRAF = 0;
        const card = document.getElementById('lifecycle-card');
        if (lcLastDTO && card && card.clientWidth > 0) renderLifecycle(lcLastDTO);
      });
    }

    function lcEnsureResizeObserver() {
      if (lcResizeObserver || typeof ResizeObserver === 'undefined') return;
      const card = document.getElementById('lifecycle-card');
      if (!card) return;
      lcResizeObserver = new ResizeObserver((entries) => {
        const width = Math.round((entries[0] && entries[0].contentRect && entries[0].contentRect.width) || 0);
        if (width && Math.abs(width - lcLastObservedWidth) > 2) {
          lcLastObservedWidth = width;
          lcScheduleLayout();
        }
      });
      lcResizeObserver.observe(card);
    }

    function lcDuration(ms) {
      const n = Number(ms) || 0;
      if (n <= 0) return '';
      const sec = Math.round(n / 1000);
      if (sec < 60) return sec + 's';
      const min = Math.round(sec / 60);
      if (min < 60) return min + 'm';
      const hr = Math.round(min / 60);
      if (hr < 48) return hr + 'h';
      return Math.round(hr / 24) + 'd';
    }

    function lcCaptureScroll() {
      const list = document.querySelector('#lifecycle-card .lc-journeys');
      if (list) lcScrollTop = list.scrollTop;
    }

    function lcRestoreScroll() {
      const list = document.querySelector('#lifecycle-card .lc-journeys');
      if (list) list.scrollTop = lcScrollTop;
    }

    function toggleLifecycleJourney(key) {
      if (!key) return;
      lcCaptureScroll();
      if (lcExpandedJourneys.has(key)) lcExpandedJourneys.delete(key);
      else lcExpandedJourneys.add(key);
      if (lcLastDTO) renderLifecycle(lcLastDTO);
    }

    function lifecycleExpandAll() {
      lcCaptureScroll();
      const journeys = Array.isArray(lcLastDTO && lcLastDTO.journeys) ? lcLastDTO.journeys : [];
      journeys.forEach((j) => { const key = lcJourneyKey(j); if (key) lcExpandedJourneys.add(key); });
      if (lcLastDTO) renderLifecycle(lcLastDTO);
    }

    function lifecycleCollapseAll() {
      lcCaptureScroll();
      const journeys = Array.isArray(lcLastDTO && lcLastDTO.journeys) ? lcLastDTO.journeys : [];
      journeys.forEach((j) => { const key = lcJourneyKey(j); if (key) lcExpandedJourneys.delete(key); });
      if (lcLastDTO) renderLifecycle(lcLastDTO);
    }

    function lifecycleToggleRows() {
      lcCaptureScroll();
      lcShowAllRows = !lcShowAllRows;
      if (lcLastDTO) renderLifecycle(lcLastDTO);
    }

    function lcLinkHtml(link) {
      if (!link || !link.url) return '';
      const label = link.label || link.kind || link.url;
      return `<a href="${escapeHtml(link.url)}" target="_blank" rel="noopener">${escapeHtml(label)}</a>`;
    }

    function lcDetailField(label, value) {
      if (value === undefined || value === null || value === '' || (Array.isArray(value) && value.length === 0)) return '';
      const html = Array.isArray(value) ? value.map((v) => String(v)).join(' · ') : String(value);
      return `<div class="lc-detail-field"><div class="lc-detail-label">${escapeHtml(label)}</div><div class="lc-detail-value">${html}</div></div>`;
    }

    function lcRenderDetails(j) {
      const d = (j && j.details) || {};
      const cls = d.classification || {};
      const clsParts = [];
      if (cls.band) clsParts.push(`band ${escapeHtml(cls.band)}`);
      if (cls.lane) clsParts.push(`lane ${escapeHtml(cls.lane)}`);
      if (cls.tier && cls.tier !== cls.band) clsParts.push(`tier ${escapeHtml(cls.tier)}`);
      if (cls.model) clsParts.push(`model ${escapeHtml(cls.model)}`);
      const links = Array.isArray(d.links) ? d.links.map(lcLinkHtml).filter(Boolean) : [];
      const fields = [
        lcDetailField('Agent', escapeHtml(d.agent || j.agent || '—')),
        lcDetailField('Current stage age', escapeHtml(lcDuration(d.timeInCurrentStageMs) || '—')),
        lcDetailField('Classification', clsParts.join(' · ')),
        lcDetailField('Links', links),
        lcDetailField('Kick IDs', (d.kickIds || []).map(escapeHtml)),
        lcDetailField('Run/log IDs', (d.runLogIds || []).map(escapeHtml)),
        lcDetailField('Blocked reason', escapeHtml(d.blockedReason || ''))
      ].filter(Boolean).join('');
      const stageDetails = Array.isArray(d.stages) ? d.stages : [];
      const stagesHtml = stageDetails.map((st) => {
        const kind = st.kind || '';
        const when = [lcAbsTime(st.lastAt), v4RelTime(st.lastAt)].filter(Boolean).join(' · ');
        const delta = lcDuration(st.durationSincePreviousMs);
        const agent = st.agent ? ` · ${escapeHtml(st.agent)}` : '';
        const count = Number(st.count) > 1 ? ` ×${Number(st.count)}` : '';
        return `<li class="lc-stage-detail">
          <span class="lc-kind ${v4KindClass(kind)}">${escapeHtml(st.label || lcStageLabel(kind))}${count}</span>
          <span class="stamp">${escapeHtml(when || '—')}</span>
          <span class="delta">${delta ? '+' + escapeHtml(delta) + ' from previous' : 'first stage'}${agent}</span>
        </li>`;
      }).join('');
      const reasonHtml = cls.reason ? `<div class="lc-detail-note"><strong>Reason:</strong> ${escapeHtml(cls.reason)}</div>` : '';
      const gridHtml = fields ? `<div class="lc-detail-grid">${fields}</div>` : '';
      const timelineHtml = stagesHtml ? `<ul class="lc-stage-list">${stagesHtml}</ul>` : '<div class="lc-empty">No stage detail recorded yet.</div>';
      return `<div class="lc-detail">${gridHtml}${timelineHtml}${reasonHtml}</div>`;
    }

    // PANEL B — lifecycle journeys + fleet health. One row per work item:
    // reached stage chips in journey order (current stage colored, earlier
    // ones dimmed), latest agent, last activity. Rows expand inline with
    // server-derived details from journey.details while keeping the collapsed
    // density unchanged.
    function renderLifecycle(dto) {
      refreshSectionCardShell('lifecycle-section');
      const section = document.getElementById('lifecycle-section');
      const card = document.getElementById('lifecycle-card');
      if (!section || !card) return;
      lcCaptureScroll();
      dto = dto || {};
      lcLastDTO = dto;
      const fleet = dto.fleet || {};
      const journeys = (Array.isArray(dto.journeys) ? dto.journeys : []).filter(lcJourneyHasContent);

      section.style.display = '';

      const inFlight = Number(fleet.inFlight) || 0;
      const merged = Number(fleet.merged) || 0;
      const blocked = Number(fleet.blocked) || 0;
      setSectionSummary('lifecycle-section', `${inFlight} in flight · ${merged} merged${blocked ? ' · ' + blocked + ' blocked' : ''}`);
      const controlsHtml = `<div class="gov-pr-models-toggle lc-controls" aria-label="Lifecycle row controls">
        <button type="button" data-action="lifecycleExpandAll" data-stop="1" title="Show details for every visible lifecycle row">Expand all</button>
        <button type="button" data-action="lifecycleCollapseAll" data-stop="1" title="Hide details for every visible lifecycle row">Collapse all</button>
      </div>`;
      const fleetHtml = `<div class="lc-head"><div class="lc-fleet">
        <div class="lc-fleet-stat"><div class="val inflight">${inFlight}</div><div class="lbl">In flight</div></div>
        <div class="lc-fleet-stat"><div class="val merged">${merged}</div><div class="lbl">Merged</div></div>
        <div class="lc-fleet-stat"><div class="val blocked">${blocked}</div><div class="lbl">Blocked</div></div>
      </div>${controlsHtml}</div>`;
      const windowLabel = lcWindowLabel(fleet);
      const windowHtml = windowLabel ? `<div class="lc-window">${escapeHtml(windowLabel)}</div>` : '';

      let listHtml;
      if (journeys.length === 0) {
        listHtml = lifecycleEmptyStateHtml();
      } else {
        const visibleJourneys = lcShowAllRows ? journeys : journeys.slice(0, LC_COLLAPSED_ROW_LIMIT);
        const bounds = lcTimelineBounds(visibleJourneys);
        const hiddenCount = journeys.length - visibleJourneys.length;
        const rowLimitHtml = hiddenCount > 0 || lcShowAllRows
          ? `<div class="lc-show-rows"><button type="button" data-action="lifecycleToggleRows" data-stop="1" aria-pressed="${lcShowAllRows ? 'true' : 'false'}">${lcShowAllRows ? 'Show first ' + LC_COLLAPSED_ROW_LIMIT : 'Expand ' + hiddenCount + ' more'}</button></div>`
          : '';
        listHtml = lcAxisHtml(bounds) + `<ul class="lc-journeys">${visibleJourneys.map((j) => {
          j = j || {};
          const current = j.current || '';
          const rawRef = j.ref || '';
          const ref = escapeHtml(lcPRLabel(j) || rawRef);
          const title = escapeHtml(lcTitleFromJourney(j));
          const key = lcJourneyKey(j);
          const expanded = key && lcExpandedJourneys.has(key);
          const agent = j.agent ? escapeHtml(j.agent) : '';
          const rel = v4RelTime(lcJourneyLastAt(j));
          const agentHtml = agent ? `<span class="lc-agent">${agent}</span>` : '';
          const timeHtml = rel ? `<span class="lc-time">${escapeHtml(rel)}</span>` : '';
          const state = current === 'merged' || current === 'blocked' ? current : 'in-flight';
          const stateClass = current === 'merged' ? 'green' : (current === 'blocked' ? 'red' : 'amber');
          return `<li class="lc-journey">
            <div class="lc-journey-summary" role="button" tabindex="0" data-action="toggleLifecycleJourney" data-keydown-action="toggleLifecycleJourney" data-keys="Enter, " data-prevent="1" data-arg0="${escapeHtml(key)}" aria-expanded="${expanded ? 'true' : 'false'}" aria-label="${expanded ? 'Collapse' : 'Expand'} lifecycle details for ${ref || 'journey'}">
              <span class="lc-row-heading"><span class="lc-row-chevron" aria-hidden="true">›</span><span><span class="lc-ref">${ref || '—'}</span>${title ? `<span class="lc-title">${title}</span>` : ''}</span></span>
              ${lcBarHtml(j, bounds)}
              <span class="lc-meta"><span class="lc-kind ${stateClass}">${escapeHtml(state)}</span>${agentHtml}${timeHtml}</span>
            </div>${expanded ? lcRenderDetails(j) : ''}
          </li>`;
        }).join('')}</ul>${rowLimitHtml}`;
      }

      card.innerHTML = fleetHtml + windowHtml + listHtml;
      lcApplyMarkerPositions(card);
      lcRestoreScroll();
      applySectionCollapse('lifecycle-section');
      lcEnsureResizeObserver();
    }

    async function fetchLifecycle() {
      try {
        const r = await fetch('/api/lifecycle-timeline?limit=50');
        if (!r.ok) return;
        const dto = await r.json();
        renderLifecycle(dto);
      } catch (e) { /* transient — leave last render in place */ }
    }

    // Change throughput card (#9427): how many PRs the hive opened, merged and
    // closed without merging over a selectable window, from
    // /api/pr-throughput. hours=0 is all time (durable counters).
    const PRT_WINDOWS = [[1, '1h'], [6, '6h'], [12, '12h'], [24, '24h'], [48, '48h'], [168, '7d'], [0, 'all']];
    const PRT_REPO_STORAGE_KEY = 'hive.prThroughput.repo';
    let prtHours = 24;
    let prtRole = 'merged';
    let prtRepo = (() => { try { return localStorage.getItem(PRT_REPO_STORAGE_KEY) || ''; } catch { return ''; } })();
    let prtLast = null;
    const PRT_TREND_VIEWS = [['counts', 'Counts'], ['share', 'Hive share %'], ['area', 'Share area']];
    const PRT_TREND_VIEW_STORAGE_KEY = 'hive.prThroughput.trendView';
    let prtTrendView = (() => { try { const v = localStorage.getItem(PRT_TREND_VIEW_STORAGE_KEY); return PRT_TREND_VIEWS.some(([k]) => k === v) ? v : 'counts'; } catch { return 'counts'; } })();

    // Mirrors lcWindowLabel: counts are honest only relative to how much
    // history backs them, so say so when the data is shorter than the window.
    function prtWindowLabel(dto) {
      const fmt = (ms) => {
        const min = Math.round(ms / 60000);
        if (min < 60) return min + 'm';
        const hr = Math.round(min / 60);
        if (hr < 48) return hr + 'h';
        return Math.round(hr / 24) + 'd';
      };
      const recorded = dto.recorded_since ? Date.parse(dto.recorded_since) : NaN;
      const coveredMs = Number.isNaN(recorded) ? 0 : Math.max(0, Date.now() - recorded);
      const repo = dto.repo ? ` · ${dto.repo}` : '';
      if (dto.all_time) {
        return (coveredMs > 0 ? `all time — over the last ${fmt(coveredMs)} of recorded history` : 'all time — nothing recorded yet') + repo;
      }
      const windowMs = (Number(dto.hours) || 0) * 3600000;
      if (!windowMs) return repo;
      if (coveredMs > 0 && coveredMs < windowMs) {
        return `over the last ${fmt(coveredMs)} of recorded history (window ${fmt(windowMs)})${repo}`;
      }
      return `over the last ${fmt(windowMs)}${repo}`;
    }

    function prtRepoOptions() {
      const names = new Set();
      ((window._lastStatus || {}).repos || []).forEach(r => { const n = r.full || r.name; if (n) names.add(n); });
      if (prtRepo) names.add(prtRepo);
      const opts = ['<option value="">All repos</option>'];
      [...names].sort((a,b) => a.localeCompare(b)).forEach(n => opts.push(`<option value="${escapeHtml(n)}"${n.toLowerCase() === String(prtRepo).toLowerCase() ? ' selected' : ''}>${escapeHtml(n)}</option>`));
      return `<select class="prt-repo-select" data-change-action="setPRThroughputRepo" data-arg-types="v" title="Filter Throughput to one repository">${opts.join('')}</select>`;
    }

    function prtSeriesSpark(dto, field, color) {
      const buckets = Array.isArray(dto.buckets) ? dto.buckets : [];
      const values = buckets.map(b => Number(b[field]) || 0);
      const times = buckets.map(b => Date.parse(b.t));
      if (values.length < 2) return '';
      const details = buckets.slice(-8).map(b => `${fmtSparkTime(Date.parse(b.t))}: ${Number(b[field]) || 0}`).join(' · ');
      return renderSparkline(null, values, {
        color, title: details, svgTitle: details, ariaLabel: details,
        label: `change throughput ${field}`, id: `change-throughput:${field}`,
      });
    }

    function prtFmtRate(v) { return (Number(v) || 0).toFixed((Number(v) || 0) >= 10 ? 0 : 1) + '/day'; }
    function prtFmtHours(v) {
      if (v == null || !isFinite(Number(v))) return '—';
      const h = Number(v);
      if (h < 1) return Math.round(h * 60) + 'm';
      if (h < 48) return h.toFixed(h >= 10 ? 0 : 1) + 'h';
      return (h / 24).toFixed(1) + 'd';
    }
    function prtFmtPct(v) { return v == null || !isFinite(Number(v)) ? '—' : Math.round(Number(v) * 100) + '%'; }
    function prtTrend(v, inverse) {
      if (v == null || !isFinite(Number(v))) return '—';
      const n = Number(v);
      const good = inverse ? n <= 0 : n >= 0;
      return `<span class="${good ? 'trend-good' : 'trend-bad'}">${n >= 0 ? '▲' : '▼'} ${Math.abs(n).toFixed(0)}%</span>`;
    }
    function prtFmtNumber(v) {
      if (v == null || !isFinite(Number(v))) return '—';
      const n = Number(v);
      return Math.abs(n) >= 10 ? n.toFixed(0) : n.toFixed(1);
    }
    function prtPctChange(current, previous) {
      if (current == null || previous == null) return null;
      const c = Number(current), p = Number(previous);
      if (!isFinite(c) || !isFinite(p) || p === 0) return null;
      return ((c - p) / p) * 100;
    }
    function prtFiniteSeries(values, times) {
      const out = { values: [], times: [] };
      values.forEach((v, i) => {
        const n = Number(v);
        if (!isFinite(n)) return;
        out.values.push(n);
        out.times.push(times[i]);
      });
      return out;
    }
    function prtSparkTitle(values, fmt) {
      const finite = values.map(Number).filter(Number.isFinite);
      if (!finite.length) return 'min — · max — · latest —';
      const min = Math.min(...finite);
      const max = Math.max(...finite);
      const latest = finite[finite.length - 1];
      return `min ${fmt(min)} · max ${fmt(max)} · latest ${fmt(latest)}`;
    }
    function prtMetricSpark(values, times, color, label, fmt, id) {
      const series = prtFiniteSeries(values, times);
      if (series.values.length < 2) return '';
      const title = prtSparkTitle(series.values, fmt);
      return `<span class="prt-metric-spark">${renderSparkline(null, series.values, {
        color,
        title,
        ariaLabel: `${label}: ${title}`,
        label,
        id: `pr-throughput:${id}`,
        wrapperClass: 'sparkline--flush',
        wrapperTitle: title,
      })}</span>`;
    }
    function prtHiveShareSpark(series) {
      return prtTrendSvg(series, {
        width: 150,
        height: 30,
        pad: 1,
        ariaLabel: 'miniature 100% stacked area trend for hive, human, and other automation actor shares',
      });
    }
    function prtBucketMetricSeries(dto) {
      const buckets = Array.isArray(dto.buckets) ? dto.buckets : [];
      const bucketDays = Math.max((Number(dto.bucket_seconds) || 3600) / 86400, 1 / 1440);
      const times = buckets.map(b => Date.parse(b.t));
      const opened = buckets.map(b => Number(b.opened) || 0);
      const merged = buckets.map(b => Number(b.merged) || 0);
      const closed = buckets.map(b => Number(b.closed) || 0);
      const ttmP50 = buckets.map(b => b.ttm_p50 == null ? null : Number(b.ttm_p50));
      const ttmP90 = buckets.map(b => b.ttm_p90 == null ? null : Number(b.ttm_p90));
      let backlog = 0;
      const netBacklog = buckets.map((b, i) => {
        backlog += opened[i] - merged[i] - closed[i];
        return backlog;
      });
      const mergedTrend = merged.map((v, i) => i === 0 ? null : prtPctChange(v, merged[i - 1]));
      const ttmTrend = [];
      let previousTTM = null;
      for (const current of ttmP50) {
        ttmTrend.push(previousTTM == null ? null : prtPctChange(current, previousTTM));
        if (current != null && isFinite(current)) previousTTM = current;
      }
      return {
        times,
        mergedVelocity: merged.map(v => v / bucketDays),
        openedVelocity: opened.map(v => v / bucketDays),
        medianTTM: ttmP50,
        p90TTM: ttmP90,
        mergeRatio: buckets.map((b, i) => opened[i] > 0 ? merged[i] / opened[i] : 0),
        netBacklog,
        mergedTrend,
        ttmTrend,
      };
    }
    function prtTrendColor(values, inverse) {
      const finite = values.map(Number).filter(Number.isFinite);
      if (!finite.length) return 'var(--muted)';
      const latest = finite[finite.length - 1];
      return (inverse ? latest <= 0 : latest >= 0) ? 'var(--green)' : 'var(--red)';
    }
    function prtTopList(title, items) {
      if (!Array.isArray(items) || !items.length) return '';
      return `<div class="prt-breakdown"><b>${escapeHtml(title)}</b><ol>${items.map(i => `<li>${escapeHtml(i.name)} · ${Number(i.count) || 0}</li>`).join('')}</ol></div>`;
    }
    function prtActorCounts(dto, kind, role) {
      const actors = (((dto.by_actor || {})[kind] || {})[role] || {});
      return { hive: Number(actors.hive) || 0, human: Number(actors.human) || 0, other: Number(actors.other) || 0 };
    }
    function prtActorCell(c) {
      const total = c.hive + c.human + c.other;
      const w = k => total ? Math.max(2, Math.round((c[k] / total) * 100)) : 0;
      const hw = w('hive'), uw = w('human'), ow = w('other');
      return `<div>${c.hive} · ${c.human} · ${c.other}<svg class="prt-stack" viewBox="0 0 100 5" preserveAspectRatio="none" title="hive · human · other"><rect class="hive" x="0" y="0" width="${hw}" height="5"/><rect class="human" x="${hw}" y="0" width="${uw}" height="5"/><rect class="other" x="${hw+uw}" y="0" width="${ow}" height="5"/></svg></div>`;
    }
    function prtActorMatrix(dto) {
      const rows = [
        ['Created', 'created', 'created'],
        ['Reviewed', 'reviewed', 'reviewed'],
        ['Merged·Closed', 'merged', 'closed']
      ];
      const any = rows.some(r => ['pr','issue'].some(k => { const c = prtActorCounts(dto, k, r[k === 'pr' ? 1 : 2]); return c.hive + c.human + c.other; }));
      if (!any) return '<div class="prt-matrix"><strong>Actor attribution</strong><p class="prt-caption">No attribution data yet — counters start now.</p></div>';
      return `<div class="prt-matrix"><strong>Actor attribution</strong><table><thead><tr><th>Role</th><th>PRs<br><small>hive · human · other</small></th><th>Issues<br><small>hive · human · other</small></th></tr></thead><tbody>${rows.map(r => `<tr><th>${r[0]}</th><td>${prtActorCell(prtActorCounts(dto, 'pr', r[1]))}</td><td>${prtActorCell(prtActorCounts(dto, 'issue', r[2]))}</td></tr>`).join('')}</tbody></table></div>`;
    }
    function prtSeriesAllZero(series, key) {
      return Array.isArray(series) && series.length > 0 && !series.some(p => (Number(p && p[key]) || 0) !== 0);
    }
    function prtTrendSvg(series, opts) {
      if (!Array.isArray(series) || series.length < 2) return '';
      opts = opts || {};
      const w = Number(opts.width) || 360, h = Number(opts.height) || 120, pad = opts.pad == null ? 10 : Number(opts.pad);
      const innerH = Math.max(1, h - pad*2);
      const pts = series.map((p, i) => {
        const hive = Math.max(0, Number(p.hive) || 0);
        const human = Math.max(0, Number(p.human) || 0);
        const other = Math.max(0, Number(p.other) || 0);
        const total = hive + human + other;
        const x = pad + (i * (w - pad*2) / Math.max(1, series.length - 1));
        const shares = total ? { hive: hive / total, human: human / total, other: other / total } : { hive: 0, human: 0, other: 0 };
        let acc = 0;
        const bands = {};
        ['hive','human','other'].forEach(key => {
          const y0 = h - pad - (acc * innerH);
          acc += shares[key];
          const y1 = h - pad - (acc * innerH);
          bands[key] = { y0, y1 };
        });
        return { x, bands };
      });
      const area = key => {
        if (prtSeriesAllZero(series, key)) return '';
        const top = pts.map((p,i) => `${i?'L':'M'}${p.x.toFixed(1)},${p.bands[key].y1.toFixed(1)}`).join(' ');
        const bottom = pts.slice().reverse().map(p => `L${p.x.toFixed(1)},${p.bands[key].y0.toFixed(1)}`).join(' ');
        return `<path class="area ${key}" d="${top}${bottom}Z"><title>${key === 'other' ? 'other automation' : key} share</title></path>`;
      };
      const label = opts.ariaLabel || '100% stacked area trend for hive, human, and other automation actor shares';
      return `<svg class="prt-trend-svg" viewBox="0 0 ${w} ${h}" role="img" aria-label="${escapeHtml(label)}">${['other','human','hive'].map(area).join('')}</svg>`;
    }
    // Hive vs human trend views. The share area above scales every bucket to
    // 100%, so one merge and sixteen look the same and empty buckets drop to
    // zero; "counts" (default) stacks the real counts per bucket and "share"
    // draws the hive share over a trailing window. Both get axes and a hover
    // title per bucket. The plot SVG stretches to its box, so labels are HTML.
    function prtTrendBucketSeconds(dto, series) {
      const s = Number(dto && dto.bucket_seconds);
      if (s > 0) return s;
      const t0 = Date.parse(series[0] && series[0].t), t1 = Date.parse(series[1] && series[1].t);
      return isFinite(t0) && isFinite(t1) && t1 > t0 ? (t1 - t0) / 1000 : 3600;
    }
    function prtDurationLabel(sec) {
      if (sec >= 86400 && sec % 86400 === 0) return `${sec / 86400}d`;
      const min = Math.round(sec / 60);
      if (min < 60) return `${min}m`;
      return min % 60 ? `${Math.floor(min / 60)}h ${min % 60}m` : `${min / 60}h`;
    }
    function prtTrendTimeLabel(ms, bucketSec, spanSec) {
      if (!isFinite(ms)) return '';
      const d = new Date(ms);
      const months = ['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'];
      // Day buckets are UTC days on the server: name them by their UTC date,
      // which local time would shift to the day before west of UTC.
      if (bucketSec >= 86400) return months[d.getUTCMonth()] + ' ' + d.getUTCDate();
      const day = months[d.getMonth()] + ' ' + d.getDate();
      const hm = String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
      return spanSec > 86400 ? `${day} ${hm}` : hm;
    }
    function prtTrendRangeLabel(startMs, endMs, bucketSec, spanSec) {
      const a = prtTrendTimeLabel(startMs, bucketSec, spanSec), b = prtTrendTimeLabel(endMs, bucketSec, spanSec);
      return a === b || !b ? a : `${a}–${b}`;
    }
    function prtNiceTicks(max) {
      if (!(max > 0)) return [0, 1];
      const raw = max / 4;
      const mag = Math.pow(10, Math.floor(Math.log10(raw)));
      const step = Math.max(1, [1, 2, 5, 10].map(m => m * mag).find(s => s >= raw));
      const ticks = [];
      for (let v = 0; v < max + step; v += step) ticks.push(v);
      return ticks;
    }
    function prtTrendPlot(series, dto, view) {
      if (!Array.isArray(series) || series.length < 2) return '';
      const n = series.length, W = 1000, H = 100, cw = W / n;
      const bucketSec = prtTrendBucketSeconds(dto, series);
      const spanSec = bucketSec * n;
      const times = series.map(p => Date.parse(p && p.t));
      const range = i => prtTrendRangeLabel(times[i], times[i] + bucketSec * 1000, bucketSec, spanSec);
      const vals = series.map(p => ({ hive: Math.max(0, Number(p && p.hive) || 0), human: Math.max(0, Number(p && p.human) || 0), other: Math.max(0, Number(p && p.other) || 0) }));
      const yOf = (v, top) => H - (v / top) * H;
      let ticks, marks = '', titles, label, note;
      if (view === 'share') {
        const win = Math.max(1, Math.round(n / 6));
        const winLabel = prtDurationLabel(bucketSec * win);
        const pts = vals.map((_, i) => {
          let hive = 0, total = 0;
          for (let j = Math.max(0, i - win + 1); j <= i; j++) { hive += vals[j].hive; total += vals[j].hive + vals[j].human + vals[j].other; }
          return total ? { pct: hive / total, hive, total } : null;
        });
        ticks = [0, 0.25, 0.5, 0.75, 1];
        let d = '';
        pts.forEach((p, i) => {
          if (!p) return;
          const y = yOf(p.pct, 1).toFixed(1);
          d += `${pts[i - 1] ? 'L' : 'M'}${(i * cw).toFixed(1)},${y} L${((i + 1) * cw).toFixed(1)},${y} `;
        });
        marks = `<line class="ref" x1="0" x2="${W}" y1="${yOf(0.5, 1)}" y2="${yOf(0.5, 1)}" vector-effect="non-scaling-stroke"></line>` + (d ? `<path class="share-line" d="${d.trim()}" vector-effect="non-scaling-stroke"></path>` : '');
        titles = pts.map((p, i) => `${range(i)} (trailing ${winLabel}) · ` + (p ? `hive ${Math.round(p.pct * 100)}% · ${p.hive} of ${p.total}` : 'no activity in window'));
        label = `Hive share over a trailing ${winLabel} window, one step per ${prtDurationLabel(bucketSec)} bucket`;
        note = `Line: hive share over a trailing ${winLabel} window · gaps mean no activity in that window · hover for details`;
      } else {
        ticks = prtNiceTicks(Math.max(0, ...vals.map(v => v.hive + v.human + v.other)));
        const top = ticks[ticks.length - 1];
        const bw = cw * 0.7;
        vals.forEach((v, i) => {
          let acc = 0;
          ['hive', 'human', 'other'].forEach(key => {
            if (!v[key]) return;
            marks += `<rect class="bar ${key}" x="${(i * cw + (cw - bw) / 2).toFixed(1)}" y="${yOf(acc + v[key], top).toFixed(2)}" width="${bw.toFixed(1)}" height="${((v[key] / top) * H).toFixed(2)}"></rect>`;
            acc += v[key];
          });
        });
        titles = vals.map((v, i) => `${range(i)} · ` + (v.hive + v.human + v.other ? `hive ${v.hive} · human ${v.human} · other ${v.other}` : 'none'));
        label = `Stacked bars: hive, human and other automation counts per ${prtDurationLabel(bucketSec)} bucket`;
        note = `Bars: count per ${prtDurationLabel(bucketSec)} bucket · hover a bar for its time and counts`;
      }
      const top = ticks[ticks.length - 1];
      const fmtTick = v => view === 'share' ? `${Math.round(v * 100)}%` : String(v);
      const grid = ticks.map(v => `<line class="grid" x1="0" x2="${W}" y1="${yOf(v, top).toFixed(2)}" y2="${yOf(v, top).toFixed(2)}" vector-effect="non-scaling-stroke"></line>`).join('');
      const hits = titles.map((t, i) => `<rect class="hit" x="${(i * cw).toFixed(1)}" y="0" width="${cw.toFixed(1)}" height="${H}"><title>${escapeHtml(t)}</title></rect>`).join('');
      const yLabels = ticks.map(v => `<span style="bottom:${((v / top) * 100).toFixed(1)}%">${fmtTick(v)}</span>`).join('');
      // Six or seven x labels on round times: the smallest whole-bucket step
      // from this list, placed on buckets that start on a multiple of it.
      const stepSec = [300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800, 604800, 2592000]
        .find(s => s % bucketSec === 0 && s * 7 >= spanSec) || bucketSec * Math.ceil(n / 7);
      let xIdx = times.map((_, i) => i).filter(i => isFinite(times[i]) && (times[i] / 1000) % stepSec === 0);
      if (!xIdx.length) xIdx = times.map((_, i) => i).filter(i => i % Math.round(stepSec / bucketSec) === 0);
      const xLabels = [];
      for (const i of xIdx) xLabels.push(`<span style="left:${((i / n) * 100).toFixed(1)}%">${escapeHtml(prtTrendTimeLabel(times[i], bucketSec, spanSec))}</span>`);
      return `<div class="prt-trend-plot prt-trend-${view === 'share' ? 'share' : 'counts'}"><div class="prt-trend-y" aria-hidden="true">${yLabels}</div><svg class="prt-trend-svg" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img" aria-label="${escapeHtml(label)}">${grid}${marks}${hits}</svg><div class="prt-trend-x" aria-hidden="true">${xLabels.join('')}</div></div><div class="prt-caption prt-trend-note">${escapeHtml(note)}</div>`;
    }
    function prtRoleNoun(role) {
      role = role || prtRole;
      switch (role) {
      case 'created': return { noun: 'PRs/issues opened by hive', short: 'hive-opened PRs/issues' };
      case 'reviewed': return { noun: 'PR reviews and issue triage', short: 'reviews/triage' };
      case 'closed': return { noun: 'PRs closed without merging + issues closed', short: 'closed PRs/issues' };
      default: return { noun: 'PRs merged + issues closed', short: 'PR merges + issue closes' };
      }
    }
    function prtWindowPhrase(dto, previous) {
      const h = Number(dto && dto.hours) || 0;
      if (!h) return previous ? 'previous recorded window' : 'recorded history';
      const label = h % 24 === 0 ? `${h / 24}d` : `${h}h`;
      return previous ? `previous ${label}` : `last ${label}`;
    }
    function prtActorDefinition() {
      return "Hive means this hive's sweep/queue/relay/forge or hive/App bot actor; human means a non-bot login; other automation means a non-hive bot. Unknown actors are excluded.";
    }
    function prtTrendStats(dto) {
      const series = Array.isArray(dto.series) ? dto.series : [];
      const totals = series.reduce((a,p) => ({ hive:a.hive+(Number(p.hive)||0), human:a.human+(Number(p.human)||0), other:a.other+(Number(p.other)||0) }), {hive:0,human:0,other:0});
      const total = totals.hive + totals.human + totals.other;
      const roleNoun = prtRoleNoun();
      const windowPhrase = prtWindowPhrase(dto, false);
      const prevPoint = dto && dto.previous_actor ? dto.previous_actor : null;
      const prevTotal = prevPoint ? (Number(prevPoint.hive)||0) + (Number(prevPoint.human)||0) + (Number(prevPoint.other)||0) : 0;
      const prevPct = prevTotal ? Math.round(((Number(prevPoint.hive)||0) / prevTotal) * 100) : null;
      const prevWindow = prtWindowPhrase(dto, true);
      if (!total) return { totals, total, cur: null, prevPct, noun: roleNoun.noun, shortNoun: roleNoun.short, windowPhrase, prevWindow, caption: 'No attribution data yet — counters start now.', definition: prtActorDefinition() };
      const cur = Math.round((totals.hive / total) * 100);
      const movement = prevPct == null ? '' : `, ${cur >= prevPct ? 'up' : 'down'} from ${prevPct}% in ${prevWindow}`;
      const caption = `hive ${cur}% of ${roleNoun.noun} in ${windowPhrase}${movement}`;
      return { totals, total, cur, prevPct, noun: roleNoun.noun, shortNoun: roleNoun.short, windowPhrase, prevWindow, caption, definition: prtActorDefinition() };
    }
    function prtTrendCaption(dto) {
      return prtTrendStats(dto).caption;
    }
    function prtHiveHumanTile(dto) {
      const opts = [['created','Created'],['reviewed','Reviewed'],['merged','Merged'],['closed','Closed']].map(([v,l]) => `<option value="${v}"${prtRole===v?' selected':''}>${l}</option>`).join('');
      const series = Array.isArray(dto.series) ? dto.series : [];
      const stats = prtTrendStats(dto);
      const tooltip = stats.total
        ? `${stats.cur}% = hive share of ${stats.noun} in ${stats.windowPhrase}. ${stats.definition} Previous compares with ${stats.prevWindow}. Counts: hive ${stats.totals.hive} · human ${stats.totals.human} · other automation ${stats.totals.other}.`
        : `Hive share of ${stats.noun}. ${stats.definition} ${stats.caption}`;
      const sub = stats.total ? `hive share of ${stats.shortNoun} · ${stats.windowPhrase}${stats.prevPct == null ? '' : ` · prev ${stats.prevPct}%`}` : 'No attribution data yet';
      return `<div class="lc-fleet-stat prt-share-tile" title="${escapeHtml(tooltip)}"><select class="prt-role-select" data-change-action="setPRThroughputRole" data-arg-types="v" data-stop="1" title="Hive vs human mode">${opts}</select><div class="val merged">${stats.cur == null ? '—' : stats.cur + '%'}</div><div class="lbl">Hive vs human</div><div class="prt-caption">${escapeHtml(sub)}</div><span class="prt-tile-spark">${prtHiveShareSpark(series)}</span></div>`;
    }
    function prtActorTrend(dto) {
      const opts = [['created','Created'],['reviewed','Reviewed'],['merged','Merged'],['closed','Closed']].map(([v,l]) => `<option value="${v}"${prtRole===v?' selected':''}>${l}</option>`).join('');
      const series = Array.isArray(dto.series) ? dto.series : [];
      const legend = [['hive', 'hive', ''], ['human', 'human', ''], ['other', 'other automation', 'non-hive bots (dependabot, renovate, GitHub Actions, …)']].map(([key, label, title]) => `<span${title ? ` title="${escapeHtml(title)}"` : ''}><i class="${key}"></i>${label}${prtSeriesAllZero(series, key) ? ' · 0' : ''}</span>`).join('');
      const stats = prtTrendStats(dto);
      const trendTitle = stats.total ? `${stats.caption}. ${stats.definition} Previous compares with ${stats.prevWindow}.` : `${stats.caption} ${stats.definition}`;
      const viewOpts = PRT_TREND_VIEWS.map(([v,l]) => `<option value="${v}"${prtTrendView===v?' selected':''}>${l}</option>`).join('');
      const chart = prtTrendView === 'area' ? prtTrendSvg(series) : prtTrendPlot(series, dto, prtTrendView);
      return `<div class="prt-trend" title="${escapeHtml(trendTitle)}"><div class="prt-trend-head"><strong>Hive vs human trend</strong><div class="prt-trend-controls"><select class="prt-role-select prt-view-select" data-change-action="setPRThroughputTrendView" data-arg-types="v" title="Chart view: counts per bucket, hive share over a trailing window, or the 100% share area" aria-label="Chart view">${viewOpts}</select><select class="prt-role-select" data-change-action="setPRThroughputRole" data-arg-types="v">${opts}</select></div></div>${chart || '<div class="prt-caption">No trend series yet.</div>'}<div class="prt-legend">${legend}</div><div class="prt-caption">${escapeHtml(prtTrendCaption(dto))}</div></div>`;
    }
    function prtWindowControls() {
      const buttons = PRT_WINDOWS.map(([h, label]) => `<button type="button" class="${prtHours === h ? 'active' : ''}" data-action="setPRThroughputHours" data-arg0="${h}" data-stop="1" title="${h === 0 ? 'All time' : 'Last ' + label}">${label}</button>`).join('');
      return `<div class="gov-pr-models-toggle window-pills-toolbar prt-window-controls" data-stop="1">${buttons}</div>`;
    }

    function renderPRThroughput(dto) {
      refreshSectionCardShell('pr-throughput-section');
      const section = document.getElementById('pr-throughput-section');
      const card = document.getElementById('pr-throughput-card');
      if (!section || !card) return;
      dto = dto || {};
      section.hidden = false;

      const opened = Number(dto.opened) || 0;
      const merged = Number(dto.merged) || 0;
      const closed = Number(dto.closed) || 0;
      const byPath = dto.merged_by_path || {};
      const mergedActors = prtActorCounts(dto, 'pr', 'merged');
      const mergedActorTotal = mergedActors.hive + mergedActors.human + mergedActors.other;
      const hiveMergedPct = mergedActorTotal ? Math.round((mergedActors.hive / mergedActorTotal) * 100) : 0;
      const windowName = PRT_WINDOWS.find(([h]) => h === prtHours);
      const slaReport = prtSLAReport();
      const summary = `${merged} merged · ${opened} opened (${windowName ? windowName[1] : 'window'})${prtSLASummary(slaReport)}`;
      setSectionSummary('pr-throughput-section', summary, `Merged and opened PRs in the selected window; ${hiveMergedPct}% hive-attributed merged share`);
      const pathLabels = { sweep: 'auto-merge sweep', queue: 'auto-merge sweep', relay: 'merge relay', forge: 'forge adapter', hive: 'hive identity', human: 'human', other_automation: 'other automation', unknown: 'unrecorded path' };
      const pathParts = Object.keys(byPath).filter((k) => Number(byPath[k]) > 0).sort()
        .map((k) => `${Number(byPath[k])} ${escapeHtml(pathLabels[k] || k)}`);
      const pathsHtml = pathParts.length ? `<div class="prt-paths">Merged by: ${pathParts.join(' · ')}</div>` : '';
      const m = dto.metrics || {};
      const metricSeries = prtBucketMetricSeries(dto);
      const metricsHtml = `<div class="prt-metrics sec-headline" data-collapsed-keep>
        <div class="prt-metric" title="Merged PRs divided by each throughput bucket, normalized to one day."><div class="mval">${escapeHtml(prtFmtRate(m.merged_per_day))}</div><div class="mlbl">merge velocity</div>${prtMetricSpark(metricSeries.mergedVelocity, metricSeries.times, 'var(--green)', 'merge velocity', prtFmtRate, 'merge-velocity')}</div>
        <div class="prt-metric" title="Opened PRs divided by each throughput bucket, normalized to one day."><div class="mval">${escapeHtml(prtFmtRate(m.opened_per_day))}</div><div class="mlbl">open velocity</div>${prtMetricSpark(metricSeries.openedVelocity, metricSeries.times, 'var(--blue)', 'open velocity', prtFmtRate, 'open-velocity')}</div>
        <div class="prt-metric" title="Per-bucket median elapsed time from PR open audit event to merge audit event. Shows — until matching open timestamps exist."><div class="mval">${escapeHtml(prtFmtHours(m.median_time_to_merge_hours))}</div><div class="mlbl">median TTM</div>${prtMetricSpark(metricSeries.medianTTM, metricSeries.times, 'var(--yellow)', 'median TTM', prtFmtHours, 'median-ttm')}</div>
        <div class="prt-metric" title="Per-bucket 90th percentile elapsed time from PR open to merge."><div class="mval">${escapeHtml(prtFmtHours(m.p90_time_to_merge_hours))}</div><div class="mlbl">p90 TTM</div>${prtMetricSpark(metricSeries.p90TTM, metricSeries.times, 'var(--red)', 'p90 TTM', prtFmtHours, 'p90-ttm')}</div>
        <div class="prt-metric" title="Merged divided by opened per throughput bucket."><div class="mval">${escapeHtml(prtFmtPct(m.merge_ratio))}</div><div class="mlbl">merge ratio</div>${prtMetricSpark(metricSeries.mergeRatio, metricSeries.times, 'var(--green)', 'merge ratio', prtFmtPct, 'merge-ratio')}</div>
        <div class="prt-metric" title="Cumulative opened minus merged minus closed without merging across the selected buckets; positive means backlog grew."><div class="mval">${Number(m.net_backlog_change) || 0}</div><div class="mlbl">net backlog</div>${prtMetricSpark(metricSeries.netBacklog, metricSeries.times, prtTrendColor(metricSeries.netBacklog, true), 'net backlog', prtFmtNumber, 'net-backlog')}</div>
        <div class="prt-metric" title="Each bucket's merged count compared with the previous bucket."><div class="mval">${prtTrend(m.trend && m.trend.merged_pct, false)}</div><div class="mlbl">merged trend</div>${prtMetricSpark(metricSeries.mergedTrend, metricSeries.times, prtTrendColor(metricSeries.mergedTrend, false), 'merged trend', v => `${prtFmtNumber(v)}%`, 'merged-trend')}</div>
        <div class="prt-metric" title="Each bucket's median time-to-merge compared with the previous bucket that had TTM samples; down is better."><div class="mval">${prtTrend(m.trend && m.trend.median_time_to_merge_pct, true)}</div><div class="mlbl">TTM trend</div>${prtMetricSpark(metricSeries.ttmTrend, metricSeries.times, prtTrendColor(metricSeries.ttmTrend, true), 'TTM trend', v => `${prtFmtNumber(v)}%`, 'ttm-trend')}</div>
      </div>`;
      const breakdowns = [prtTopList('Merged by agent', dto.merged_by_agent), prtTopList('Merged by repo', dto.top_repos), prtTopList('Close reasons', dto.closed_reasons)].filter(Boolean).join('');
      const label = prtWindowLabel(dto);
      card.innerHTML = `<div class="prt-head"><div class="lc-window">${escapeHtml(label)}</div><div class="prt-controls collapsed-control" data-collapsed-control>${prtRepoOptions()}<button type="button" class="hv-btn btn-secondary btn-sm" data-action="exportPRThroughputCSV" title="Download bucket series as CSV">CSV</button></div></div>
        <div class="lc-fleet prt-headline sec-headline" data-collapsed-keep>
          ${prtWindowControls()}
          <div class="lc-fleet-stat" title="Pull/merge requests opened by hive agents in the selected range"><div class="val opened">${opened}</div><div class="lbl">Opened</div><span class="prt-tile-spark">${prtSeriesSpark(dto, 'opened', 'var(--blue)')}</span></div>
          <div class="lc-fleet-stat" title="Pull/merge requests observed merged in the selected range"><div class="val merged">${merged}</div><div class="lbl">Merged</div><span class="prt-tile-spark">${prtSeriesSpark(dto, 'merged', 'var(--green)')}</span></div>
          <div class="lc-fleet-stat" title="Pull/merge requests observed closed without merging in the selected range"><div class="val closed">${closed}</div><div class="lbl">Closed without merging</div><span class="prt-tile-spark">${prtSeriesSpark(dto, 'closed', 'var(--red)')}</span></div>
          ${prtHiveHumanTile(dto)}
        </div>${metricsHtml}${renderThroughputSLA()}<div class="prt-attribution">${prtActorMatrix(dto)}${prtActorTrend(dto)}</div>${pathsHtml}${breakdowns ? `<div class="prt-breakdowns">${breakdowns}</div>` : ''}`;
      applySectionCollapse('pr-throughput-section');
    }


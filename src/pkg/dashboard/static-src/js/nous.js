    // ── Experiment Lab (Nous) rendering ──────────────────────────────────────
    let _nousCache = null;
    let _nousPollTimer = null;
    let _nousPollStarted = false;
    const NOUS_POLL_MS = 30000;
    const NOUS_CONFIDENCE_THRESHOLD = 0.8;
    const NOUS_PRINCIPLE_HIGH_CONFIDENCE = 5;
    // Shared ledger/principle schema (#11697, pkg/nousstore): every entry
    // carries `type` (what it is) and `status` (where it is). Status maps
    // onto the design system's status tones; type is always neutral.
    const NOUS_STATUS_TONES = {
      pending: 'warn', active: 'info', completed: 'ok', aborted: 'error',
      updated: 'info', pruned: 'neutral',
    };
    const NOUS_RECENT_ROUNDS = 8;
    function nousEntryLabel(entry) {
      if (entry.id) return String(entry.id);
      const parts = [];
      if (entry.iteration != null) parts.push('round ' + entry.iteration);
      if (entry.candidate_id) parts.push(String(entry.candidate_id));
      else if (entry.family) parts.push(String(entry.family));
      return parts.join(' · ') || '?';
    }
    function nousChip(kind, value) {
      if (!value) return '';
      const tone = kind === 'status' ? (NOUS_STATUS_TONES[value] || 'neutral') : 'neutral';
      return `<span class="status-badge nous-${kind}-chip" data-status="${tone}" data-nous-${kind}="${escapeHtml(value)}" title="${kind}: ${escapeHtml(value)}">${escapeHtml(String(value).replace(/_/g, ' '))}</span>`;
    }

    async function fetchNousStatus() {
      if (!experimentLabEnabled()) return;
      try {
        const [statusRes, ledgerRes, principlesRes] = await Promise.all([
          fetch('/api/nous/status'), fetch('/api/nous/ledger'), fetch('/api/nous/principles'),
        ]);
        _nousCache = {
          status: await statusRes.json(),
          ledger: await ledgerRes.json(),
          principles: await principlesRes.json(),
        };
        renderNous();
      } catch (_) {
        // Nous endpoints may not exist yet — show the explainer rather
        // than a blank section on new hives.
        const panel = document.getElementById('nous-panel');
        if (panel && !panel.innerHTML.trim()) panel.innerHTML = _nousExplainerCard();
      }
    }

    function ensureNousPolling() {
      if (!experimentLabEnabled() || _nousPollStarted) return Promise.resolve();
      _nousPollStarted = true;
      return fetchNousStatus().then(() => { if (experimentLabEnabled() && !_nousPollTimer) _nousPollTimer = dashboardSetInterval('nous-status', fetchNousStatus, NOUS_POLL_MS); });
    }

    function _nousExplainerCard() {
      return emptyStateCard('🧪', 'Experiment Lab — controlled experiments on your repositories',
        'The Experiment Lab is the hive’s strategist: it designs controlled experiments on your repositories’ code — tests, CI config, automation scripts — and measures each one against a recorded baseline of hive behavior. Every repo experiment is proposed as a pull request and always needs your approval before it lands. ' +
        '<strong>Observe</strong> only collects baseline data, <strong>Suggest</strong> proposes experiments for your approval, and <strong>Evolve</strong> runs them autonomously, distilling proven changes into principles. ' +
        'To get started, leave it in Observe while it builds a baseline (it needs a few days of snapshots), then switch to Suggest to review its first proposals.');
    }

    function nousFmtDuration(sec) {
      const n = Number(sec) || 0;
      if (n <= 0) return 'now';
      const h = Math.ceil(n / 3600);
      if (h >= 1) return `~${h}h`;
      return `~${Math.max(1, Math.ceil(n / 60))}m`;
    }

    function nousProgressBar(pct, label) {
      const w = Math.max(0, Math.min(100, Math.round(Number(pct) || 0)));
      return `<div class="nous-progress" role="progressbar" aria-label="${escapeHtml(label || 'Progress')}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${w}"><div class="nous-progress-fill" style="width:${w}%"></div></div>`;
    }

    function nousMetricValue(v, unit) {
      if (v == null || Number.isNaN(Number(v))) return '—';
      const n = Number(v);
      const body = Math.abs(n) >= 10 ? String(Math.round(n)) : String(Math.round(n * 10) / 10);
      return `${body}${unit || ''}`;
    }

    function nousMetricLine(m) {
      const b = nousMetricValue(m.baseline, m.unit);
      const e = nousMetricValue(m.experiment, m.unit);
      const d = m.delta == null ? '' : ` Δ ${nousMetricValue(m.delta, m.unit)}`;
      return `<span>${escapeHtml(m.label || m.name)}: ${escapeHtml(b)} → ${escapeHtml(e)}${escapeHtml(d)}</span>`;
    }

    function nousOutputLinks(outputs) {
      const links = (outputs || []).map(o => {
        const title = o.title || o.type || 'output';
        if (o.url) return `<a href="${escapeHtml(o.url)}" target="_blank" rel="noopener">${escapeHtml(title)}</a>`;
        return `<span>${escapeHtml(title)}</span>`;
      });
      return links.length ? `<div class="nous-output-links">${links.join('')}</div>` : '';
    }

    // nousExperimentProgressRows lists proposals / runs attributed to the
    // cards; with none it renders nothing — the cards carry the empty state.
    function nousExperimentProgressRows(experiments) {
      if (!experiments || !experiments.length) return '';
      return `<div class="nous-exp-list">${experiments.map(exp => {
        const title = exp.title || exp.hypothesis || exp.id || 'experiment';
        const phase = exp.phase || exp.status || 'defined';
        const eta = exp.nextOutputEta || (exp.nextOutputEtaSeconds ? nousFmtDuration(exp.nextOutputEtaSeconds) : '');
        const counts = exp.snapshotCounts || {};
        const countsText = ['baseline','experiment','cooldown','judging'].map(k => `${k} ${Number(counts[k]) || 0}`).join(' · ');
        const metrics = (exp.metricsSoFar || []).slice(0, 4).map(nousMetricLine).join('');
        const remaining = exp.remainingSeconds ? `${nousFmtDuration(exp.remainingSeconds)} left in ${phase}` : (phase === 'judged' ? 'output available' : 'waiting for timestamps');
        return `<div class="nous-exp-row">
          <div class="nous-exp-head"><div class="nous-exp-title">${escapeHtml(title)}</div><span class="nous-status-pill">${escapeHtml(phase)}</span></div>
          ${exp.hypothesis && exp.hypothesis !== title ? `<div class="nous-progress-copy">${escapeHtml(exp.hypothesis)}</div>` : ''}
          ${nousProgressBar(exp.progressPct, `${title} phase progress`)}
          <div class="nous-exp-meta">${escapeHtml(remaining)} · snapshots: ${escapeHtml(countsText)}${eta ? ` · next output ${escapeHtml(eta)}` : ''}</div>
          ${metrics ? `<div class="nous-exp-metrics">${metrics}</div>` : ''}
          ${nousOutputLinks(exp.outputs)}
        </div>`;
      }).join('')}</div>`;
    }

    function nousResultsRows(results) {
      if (!results || !results.length) return '';
      return `<div class="nous-card mt-5"><h3>Results</h3><div class="nous-results-list">${results.map(r => {
        const title = r.title || r.hypothesis || r.id || 'experiment';
        const verdict = r.verdict || r.status || 'judged';
        const delta = r.delta ? ` · ${r.delta}` : '';
        return `<div class="nous-result-row"><div class="nous-result-head"><strong>${escapeHtml(title)}</strong><span class="nous-status-pill">${escapeHtml(verdict)}</span></div><div class="nous-progress-copy">${escapeHtml((r.hypothesis || '') + delta)}</div>${nousOutputLinks(r.outputs)}</div>`;
      }).join('')}</div></div>`;
    }

    // nousResearchQuestionText normalises a stored research question for the
    // experiment cards: a pasted YAML key ("research_question:") and folded /
    // blockquote markers ("> ", "|") are dropped so only the question shows.
    function nousResearchQuestionText(raw) {
      let text = String(raw || '').trim();
      text = text.replace(/^research[_ -]?question\s*:\s*/i, '');
      text = text.replace(/^(?:[>|][-+]?\s*)+/, '');
      return text.replace(/\s+/g, ' ').trim();
    }

    // nousSectionSummaryText is the collapsed Experiment Lab summary: lab mode
    // plus experiment count. Research questions live on the experiment cards.
    function nousSectionSummaryText(cache) {
      const s = (cache && cache.status) || {};
      if (!cache || s.status === 'not_configured') return '—';
      const ledger = Array.isArray(cache.ledger) ? cache.ledger : [];
      // With campaign cards (#12532) the count is the experiments that are
      // running or paused; otherwise the ledger length as before.
      const open = Array.isArray(s.campaigns) ? s.campaigns.filter(c => c && c.status !== 'archived') : null;
      const n = open ? open.length : ledger.length;
      return `${s.mode || 'observe'} · ${n} experiment${n === 1 ? '' : 's'}`;
    }

    function nousCampaignDate(ts) {
      if (!ts) return '—';
      try { return formatDashboardDateTime(ts, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }); } catch (_) { return String(ts); }
    }

    // Muted secondary line under the stat tiles: the question-independent
    // hive-state history is never hidden by a campaign restart (#12501).
    function nousObservationLine(obs, camp) {
      const total = Number(obs && obs.snapshots) || 0;
      const since = obs && obs.since ? ` since ${escapeHtml(nousCampaignDate(obs.since))}` : '';
      const prior = camp ? Math.max(0, total - (Number(camp.snapshots) || 0)) : 0;
      return `<div class="nous-observation-note text-muted">Observation history: ${total} snapshots${since}${prior ? ` (${prior} before this experiment)` : ''}.</div>`;
    }

    // nousDataCollectionNote is the compact muted copy under the cards: the
    // observation-history total, what is collected, cadence and retention.
    // It replaces the old global baseline bar and "Experiment Progress" box.
    function nousDataCollectionNote(s, camp) {
      const st = s || {};
      const base = st.baseline || {};
      const obs = st.observation || { snapshots: st.snapshotCount || 0 };
      let h = '<div class="nous-data-note">';
      h += nousObservationLine(obs, camp || null);
      h += `<div class="nous-progress-copy">Collecting: ${escapeHtml(((base.collecting || ['queue issues','queue PRs','queue on hold','SLA violations','agents kicked','agent states','total tokens']).join(', ')))}. One hive-state snapshot per governor evaluation cycle (4/hour). Retention: ${escapeHtml(base.retention || '24 h raw snapshots, then hourly buckets for 30 days')}.</div>`;
      h += '<div class="nous-progress-copy">Each experiment card above carries its own baseline bar (snapshots since that experiment started, out of ' + NOUS_BASELINE_TARGET_DEFAULT + '). Observe records data only; experiments wait for Suggest/Evolve and operator approval before repo changes run.</div>';
      h += '</div>';
      return h;
    }

    function nousArchivedCampaigns(campaigns) {
      if (!campaigns || !campaigns.length) return '';
      return `<div class="nous-card mt-5"><h3>Previous experiments <span class="nous-lab-h3-note">archived · read-only</span></h3><div class="nous-archived-list campaigns-list">${campaigns.map(c => {
        const q = nousResearchQuestionText(c.question) || '(no research question)';
        const when = `${nousCampaignDate(c.startedAt)} → ${nousCampaignDate(c.endedAt)}`;
        const counts = `${Number(c.snapshots) || 0} snapshots · ${Number(c.experiments) || 0} hypotheses · ${Number(c.principles) || 0} principles`;
        const results = (c.results || []).map(r => {
          const title = r.title || r.hypothesis || r.id || 'experiment';
          const verdict = r.verdict || r.status || 'judged';
          return `<div class="nous-result-row"><div class="nous-result-head"><strong>${escapeHtml(title)}</strong><span class="nous-status-pill">${escapeHtml(verdict)}</span></div>${r.delta ? `<div class="nous-progress-copy">${escapeHtml(r.delta)}</div>` : ''}${nousOutputLinks(r.outputs)}</div>`;
        }).join('');
        return `<div class="campaign-row nous-exp-row nous-archived-row" data-campaign-id="${Number(c.id) || 0}" data-status="archived">
          <div>
            <div class="campaign-row-title">#${Number(c.id) || 0} · ${escapeHtml(q)}</div>
            <div class="campaign-row-meta"><span class="campaign-pill nous-exp-pill" data-status="archived">archived</span>${nousScopePills(c.scope)}<span>${escapeHtml(when)}</span><span>${escapeHtml(counts)}</span></div>
            ${results ? `<div class="nous-results-list mt-2">${results}</div>` : '<div class="nous-progress-copy">No judged results.</div>'}
          </div>
        </div>`;
      }).join('')}</div></div>`;
    }

    // What "672" means, said in the UI (#12532): one snapshot per governor
    // cycle, 4/hour × 24 × 7 ≈ one week, so the baseline sees every weekday
    // and weekend regime once.
    const NOUS_BASELINE_TARGET_DEFAULT = 672;
    const NOUS_BASELINE_TARGET_EXPLAINER = '≈ 7 days of observations (4/hour)';
    function nousBaselineTargetTip(target, explainer) {
      const t = Number(target) || NOUS_BASELINE_TARGET_DEFAULT;
      return `${t} baseline snapshots ${explainer || NOUS_BASELINE_TARGET_EXPLAINER}: one hive-state snapshot per governor evaluation cycle, 4 an hour × 24 hours × 7 days, so the baseline covers every weekday and weekend regime once before experiments are judged against it.`;
    }
    function nousBaselineInfo(id, target, explainer) {
      const tipId = `nous-baseline-tip-${escapeHtml(String(id))}`;
      return `<span class="config-info overview-term-info" tabindex="0" data-action="gh3" data-stop="1" aria-describedby="${tipId}" aria-label="What the baseline target means">ⓘ<span id="${tipId}" class="config-tooltip" role="tooltip">${escapeHtml(nousBaselineTargetTip(target, explainer))}</span></span>`;
    }

    function nousScopePills(scope) {
      const sc = scope || {};
      const repos = Array.isArray(sc.repos) ? sc.repos : [];
      let h = '';
      if (!repos.length || sc.hiveWide) {
        h += '<span class="campaign-pill nous-scope-pill" title="This experiment may touch every repo, so no other experiment can run beside it">hive-wide</span>';
      } else {
        for (const r of repos) {
          const short = String(r).includes('/') ? String(r).split('/')[1] : String(r);
          h += `<span class="campaign-pill nous-scope-pill" title="${escapeHtml(r)}">${escapeHtml(short)}</span>`;
        }
      }
      if (sc.intervention) h += `<span class="campaign-pill nous-scope-pill" title="Intervention kind">${escapeHtml(sc.intervention)}</span>`;
      for (const t of (Array.isArray(sc.tags) ? sc.tags : [])) h += `<span class="campaign-pill nous-scope-pill">#${escapeHtml(t)}</span>`;
      return h;
    }

    // One running/paused experiment as an Inception-style campaign row:
    // question, scope pills, its own baseline bar "N / 672 (≈ 7 days)",
    // status and pause/resume/archive (#12532).
    // nousExperimentStage renders the lifecycle line of a card from the
    // server-derived stage (collecting baseline → ready → proposals awaiting
    // review → running → concluded). Older servers without `stage` fall
    // back to the baseline count so the card never goes blank.
    function nousExperimentStage(c, status, n, target) {
      let stage = c.stage || '';
      let label = c.stageLabel || '';
      if (!stage) {
        if (status === 'paused') { stage = 'paused'; label = 'paused'; }
        else if (c.complete) { stage = 'ready'; label = 'ready — generating proposals'; }
        else { stage = 'baseline'; label = `collecting baseline ${n} / ${target}`; }
      }
      const detail = c.stageDetail ? `<span class="nous-exp-stage-detail">${escapeHtml(c.stageDetail)}</span>` : '';
      // The hive designs no proposals itself: when the server reports the
      // external experiment loop idle, point at how to run it (#12541).
      const loopDocs = c.experimentLoopIdle
        ? '<a class="nous-exp-stage-docs" href="https://github.com/hivecommons/hive/blob/v5/src/docs/dashboard-sections.md#experiment-lab" target="_blank" rel="noopener noreferrer">How to run the experiment loop</a>'
        : '';
      return `<div class="nous-exp-stage" data-stage="${escapeHtml(stage)}"${c.experimentLoopIdle ? ' data-loop-idle="1"' : ''}><span class="nous-exp-stage-label">${escapeHtml(label)}</span>${detail}${loopDocs}</div>`;
    }

    const NOUS_PRINCIPLES_EXPLAINER = 'Principles are durable lessons the lab distils from judged experiments (principles.json). A new lab starts at 0 and the count grows as experiments conclude; none are needed to start an experiment.';

    function nousPrinciplesInfo(explainer) {
      return `<span class="config-info overview-term-info" tabindex="0" data-action="gh3" data-stop="1" aria-describedby="nous-principles-tip" aria-label="What principles are">ⓘ<span id="nous-principles-tip" class="config-tooltip" role="tooltip">${escapeHtml(explainer || NOUS_PRINCIPLES_EXPLAINER)}</span></span>`;
    }

    // nousAllowedRepos is the repo allow-list from the ⚙ dialog ("Repos
    // experiments may target"); an empty allow-list means any project repo.
    function nousAllowedRepos(s) {
      const st = s || {};
      const allow = Array.isArray(st.repoTargets) ? st.repoTargets : [];
      const all = Array.isArray(st.projectRepos) ? st.projectRepos : (Array.isArray(st.availableRepos) ? st.availableRepos : []);
      return allow.length ? allow : all;
    }

    // nousScopeWarning flags, in muted text, scope repos that the allow-list
    // no longer contains. The experiment keeps its scope; only new ones are
    // limited to the current allow-list.
    function nousScopeWarning(c, s) {
      const allow = Array.isArray(s && s.repoTargets) ? s.repoTargets.map(r => String(r).toLowerCase()) : [];
      if (!allow.length) return '';
      const repos = Array.isArray(c && c.scope && c.scope.repos) ? c.scope.repos : [];
      const gone = repos.filter(r => !allow.includes(String(r).toLowerCase()));
      if (!gone.length) return '';
      return `<div class="nous-exp-scope-warn text-muted" role="note">${gone.map(escapeHtml).join(', ')} ${gone.length === 1 ? 'is' : 'are'} no longer in the lab's allow-list (⚙ Repos experiments may target). This experiment keeps its scope; new experiments cannot choose ${gone.length === 1 ? 'it' : 'them'}.</div>`;
    }

    // Compact icon action for a card row: aria-label + title carry the
    // verb, the glyph is decorative. Sized by .nous-exp-icon (≥ 28px).
    function nousExpIconButton(action, id, arg1, label, glyph, extraClass) {
      const arg = arg1 ? ` data-arg1="${arg1}"` : '';
      return `<button type="button" class="hv-btn btn-icon btn-sm nous-exp-icon${extraClass ? ' ' + extraClass : ''}" data-action="${action}" data-arg0="${id}"${arg} aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}"><span aria-hidden="true">${glyph}</span></button>`;
    }

    function nousExperimentCard(c, s) {
      const id = Number(c.id) || 0;
      const status = c.status === 'paused' ? 'paused' : 'active';
      const target = Number(c.target) || NOUS_BASELINE_TARGET_DEFAULT;
      const n = Math.min(Number(c.snapshots) || 0, target);
      const pct = c.percent != null ? Number(c.percent) : (n * 100 / target);
      const q = nousResearchQuestionText(c.question) || '(no research question)';
      const hyp = c.hypothesis ? `<div class="nous-exp-hypothesis">Hypothesis: ${escapeHtml(c.hypothesis)}</div>` : '';
      const version = Number(c.version) || 1;
      const lineage = (version > 1 || c.restartedFrom)
        ? `<div class="nous-exp-version text-muted">v${version}${c.restartedFrom ? ` · restarted from #${Number(c.restartedFrom) || 0}` : ''}</div>`
        : '';
      const toggle = status === 'paused'
        ? nousExpIconButton('nousCampaignAction', id, 'resume', 'Resume experiment', '▶', 'nous-exp-icon-resume')
        : nousExpIconButton('nousCampaignAction', id, 'pause', 'Pause experiment', '⏸', 'nous-exp-icon-pause');
      // Single primary action when the server says one is needed: the
      // baseline is complete but the lab is still in Observe mode, so the
      // operator never has to discover the mode knob (#12532).
      const primary = c.nextAction === 'generate_proposals'
        ? `<button class="hv-btn btn-primary btn-sm" data-action="nousGenerateProposals" data-arg0="${id}" title="Switch the lab to Suggest so proposals can be designed for your review">Generate proposals</button>`
        : '';
      return `<div class="campaign-row nous-exp-row" data-campaign-id="${id}" data-status="${status}">
        <div>
          <div class="campaign-row-title">#${id} · ${escapeHtml(q)}</div>
          ${lineage}
          ${hyp}
          <div class="campaign-row-meta"><span class="campaign-pill nous-exp-pill" data-status="${status}">${status}</span>${nousScopePills(c.scope)}<span>started ${escapeHtml(nousCampaignDate(c.startedAt))}</span><span>${Number(c.experiments) || 0} hypotheses</span></div>
          ${nousScopeWarning(c, s)}
          ${nousExperimentStage(c, status, n, target)}
          <div class="nous-exp-baseline">Baseline ${n} / ${target} (${escapeHtml(c.targetExplainer || NOUS_BASELINE_TARGET_EXPLAINER)})${c.complete ? ' · complete' : ''}${nousBaselineInfo(id, target, c.targetExplainer)}${nousProgressBar(pct, `Baseline for experiment #${id}`)}</div>
        </div>
        <div class="campaign-row-actions nous-exp-actions">
          ${primary}
          ${nousExpIconButton('nousEditExperiment', id, '', 'Edit experiment', '✎', 'nous-exp-icon-edit')}
          ${toggle}
          ${nousExpIconButton('nousCampaignAction', id, 'archive', 'Archive experiment', '🗄', 'nous-exp-icon-archive')}
        </div>
      </div>`;
    }

    // Add / edit experiment dialog state. editId > 0 means the dialog edits
    // that experiment; original holds its pre-edit values so a question /
    // scope change can be told from a hypothesis-only one; returnFocus is
    // the button that opened it, re-focused on close (#12697).
    let _nousNewExp = { open: false, conflict: null, pending: false, editId: 0, original: null, returnFocus: null };
    const NOUS_EXP_DIALOG_ID = 'nous-exp-overlay';
    const NOUS_EXP_EDIT_HELP = 'Changing the question or scope restarts it; the hypothesis may change freely.';
    const NOUS_EXP_NEW_HELP = 'Enter the research question and pick the repos it may touch; the baseline starts at 0 immediately.';
    const NOUS_EXP_DIALOG_BUSY = 'Finish or cancel the open experiment form first';

    // The one place an experiment is defined (new or edited): question,
    // hypothesis and scope, rendered as the panel of a modal dialog. The
    // scope pills are the allow-list from the ⚙ dialog (all project repos
    // when it is empty) plus, when editing, any repo the experiment already
    // targets so it can still be deselected.
    function nousNewExperimentForm(allowedRepos, presetRepos, conflict, edit) {
      const repos = Array.isArray(allowedRepos) ? allowedRepos.slice() : [];
      const preset = Array.isArray(presetRepos) ? presetRepos : [];
      for (const r of preset) if (!repos.includes(r)) repos.push(r);
      const editing = !!edit;
      const editId = editing ? (Number(edit.id) || 0) : 0;
      const title = editing ? `Edit experiment #${editId}` : 'New experiment';
      let h = `<div class="hive-dialog nous-exp-dialog" role="dialog" aria-modal="true" aria-labelledby="nous-exp-dialog-title" aria-describedby="nous-exp-dialog-help"${editing ? ` data-edit-id="${editId}"` : ''}>`;
      h += `<div id="nous-exp-dialog-title" class="hive-dialog-title">${escapeHtml(title)}</div>`;
      h += `<div id="nous-exp-dialog-help" class="nous-exp-dialog-help">${escapeHtml(editing ? NOUS_EXP_EDIT_HELP : NOUS_EXP_NEW_HELP)}</div>`;
      h += `<div class="nous-new-exp" id="nous-new-exp" role="form" aria-label="${escapeHtml(title)}">`;
      h += `<div><label for="nous-ne-question">Research question</label><textarea id="nous-ne-question" rows="2" placeholder="Can we cut CI time by 20% by sharding the slowest test package?">${editing ? escapeHtml(nousResearchQuestionText(edit.question)) : ''}</textarea></div>`;
      h += `<div><label for="nous-ne-hypothesis">Hypothesis <span class="nous-lab-h3-note">(optional)</span></label><input type="text" id="nous-ne-hypothesis" placeholder="What you expect to change, and by how much" value="${editing ? escapeHtml(edit.hypothesis || '') : ''}"></div>`;
      h += '<div><label>Scope <span class="nous-lab-h3-note">(repos this experiment may touch, from the lab\'s allow-list; none selected = hive-wide)</span></label>';
      if (repos.length === 0) {
        h += '<div class="nous-lab-empty">No project repos available &mdash; the experiment will be hive-wide.</div>';
      } else {
        h += '<div class="nous-lab-chips" role="group" aria-label="Experiment scope">';
        for (const repo of repos) {
          const on = preset.includes(repo);
          const short = repo.includes('/') ? repo.split('/')[1] : repo;
          h += `<button type="button" class="${nousRepoPillClass(on)} nous-ne-pill" aria-pressed="${on}" data-repo="${escapeHtml(repo)}" data-action="toggleNousRepo" data-arg-types="t" title="${escapeHtml(nousRepoPillTitle(repo, on))}"><span>${escapeHtml(short)}</span></button>`;
        }
        h += '</div>';
      }
      h += '</div></div>';
      if (conflict) h += nousConflictWarning(conflict);
      h += '<div class="nous-new-exp-actions">';
      h += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="nousCancelNewExperiment">Cancel</button>';
      h += `<button type="button" class="hv-btn btn-primary btn-sm" data-action="nousSubmitNewExperiment"${_nousNewExp.pending ? ' disabled' : ''}>${editing ? 'Save changes' : 'Start experiment'}</button>`;
      h += '</div></div>';
      return h;
    }

    // Styled in-app warning for a 409 scope_conflict: says why (concurrent
    // interventions on the same scope confound each other's outcomes) and
    // offers to pause the other experiment or narrow the scope.
    function nousConflictWarning(conf) {
      const other = conf.conflict || {};
      const id = Number(other.id) || 0;
      const shared = Array.isArray(conf.shared) && conf.shared.length ? conf.shared.join(', ') : 'hive-wide';
      return `<div class="nous-conflict-warn" role="alert" data-conflict-id="${id}">
        <div><strong>Scope conflict with experiment #${id}</strong> — ${escapeHtml(other.question || '(no research question)')}</div>
        <div>Both would intervene on <strong>${escapeHtml(shared)}</strong>. Two experiments changing the same scope at the same time confound each other's outcomes, so only one may run there.</div>
        <div class="nous-new-exp-actions">
          <button type="button" class="hv-btn btn-secondary btn-sm" data-action="nousPauseConflictAndStart" data-arg0="${id}" title="Pause experiment #${id}, then start this one">Pause #${id} and start</button>
          <button type="button" class="hv-btn btn-secondary btn-sm" data-action="nousNarrowScope" title="Deselect the shared repos above and try again">Narrow scope</button>
        </div>
      </div>`;
    }

    // The experiments card list, Inception-style (#12532).
    function nousExperimentCards(campaigns, s) {
      const st = s || {};
      const open = (campaigns || []).filter(c => c && c.status !== 'archived');
      let h = '<div class="campaigns-card nous-exp-cards">';
      h += '<div class="campaigns-head"><div><div class="campaigns-title">Experiments</div><div class="campaigns-subtitle">Each running experiment has its own research question, scope and baseline. Experiments may run side by side when their scopes do not overlap.</div></div>';
      h += '<button class="hv-btn btn-primary btn-sm" data-action="nousOpenNewExperiment" title="Start another experiment with its own question, scope and baseline">+ New experiment</button></div>';
      h += '<div class="campaigns-list" id="nous-exp-list">';
      if (open.length === 0) {
        h += '<div class="campaign-empty">Add an experiment: enter a research question and pick the repos it may touch.</div>';
      } else {
        h += open.map(c => nousExperimentCard(c, st)).join('');
      }
      h += '</div></div>';
      return h;
    }

    function nousNewExperimentValues() {
      const form = document.getElementById('nous-new-exp');
      if (!form) return null;
      return {
        question: (document.getElementById('nous-ne-question') || {}).value || '',
        hypothesis: (document.getElementById('nous-ne-hypothesis') || {}).value || '',
        repos: Array.from(form.querySelectorAll('.nous-ne-pill.is-on')).map(p => p.dataset.repo),
      };
    }

    function nousRestoreNewExperimentValues(v) {
      if (!v) return;
      const q = document.getElementById('nous-ne-question');
      const hy = document.getElementById('nous-ne-hypothesis');
      if (q) q.value = v.question;
      if (hy) hy.value = v.hypothesis;
      document.querySelectorAll('#nous-new-exp .nous-ne-pill').forEach(p => nousRepoPillSetState(p, v.repos.includes(p.dataset.repo)));
    }

    // Render (or re-render) the add/edit dialog into its body-level overlay.
    // It lives outside #nous-panel so status re-renders never touch it; the
    // hive-dialog-overlay class puts it on the shared Esc handler
    // (hiveDismissTopOverlay → hiveDialogClose) under any hiveConfirm on top.
    function nousRenderExperimentDialog() {
      const s = (_nousCache && _nousCache.status) || {};
      const edit = _nousNewExp.editId ? (s.campaigns || []).find(c => c && Number(c.id) === _nousNewExp.editId) : null;
      const preset = edit ? ((edit.scope && edit.scope.repos) || []) : [];
      let overlay = document.getElementById(NOUS_EXP_DIALOG_ID);
      if (!overlay) {
        overlay = document.createElement('div');
        overlay.id = NOUS_EXP_DIALOG_ID;
        overlay.className = 'hive-dialog-overlay nous-exp-overlay';
        hiveRememberModalOpen(overlay);
        overlay.hiveDialogClose = nousCancelNewExperiment;
        overlay.addEventListener('click', (e) => { if (e.target === overlay) nousCancelNewExperiment(); });
        overlay.addEventListener('keydown', nousExperimentDialogTrapTab);
        document.body.appendChild(overlay);
      }
      overlay.innerHTML = nousNewExperimentForm(nousAllowedRepos(s), preset, _nousNewExp.conflict, edit);
      return overlay;
    }

    // Keep Tab inside the dialog while it is open (same loop as hiveWireDialog).
    function nousExperimentDialogTrapTab(e) {
      if (e.key !== 'Tab') return;
      const overlay = e.currentTarget;
      const nodes = Array.from(overlay.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')).filter(el => !el.disabled && el.offsetParent !== null);
      if (!nodes.length) return;
      const first = nodes[0];
      const last = nodes[nodes.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }

    function nousFocusExperimentQuestion() {
      const q = document.getElementById('nous-ne-question');
      if (q) q.focus();
    }

    // Only one add/edit dialog at a time: a second trigger while one is open
    // (e.g. ✎ while a New draft is being typed) is blocked, the open dialog
    // is re-focused and a toast says why, so no draft is ever silently
    // replaced (#12697).
    function nousExperimentDialogBusy() {
      if (!_nousNewExp.open) return false;
      showToast(NOUS_EXP_DIALOG_BUSY, 'info');
      nousFocusExperimentQuestion();
      return true;
    }

    function nousOpenNewExperiment() {
      if (!experimentLabEnabled()) return;
      if (nousExperimentDialogBusy()) return;
      _nousNewExp = { open: true, conflict: null, pending: false, editId: 0, original: null, returnFocus: document.activeElement };
      nousRenderExperimentDialog();
      nousFocusExperimentQuestion();
    }

    // Edit opens the same dialog pre-filled from the card (#12532 follow-up).
    function nousEditExperiment(id) {
      if (!experimentLabEnabled()) return;
      const s = (_nousCache && _nousCache.status) || {};
      const c = (s.campaigns || []).find(x => x && Number(x.id) === Number(id));
      if (!c) { showToast(`Experiment #${id} not found`, 'error'); return; }
      if (nousExperimentDialogBusy()) return;
      _nousNewExp = { open: true, conflict: null, pending: false, editId: Number(id), original: { question: nousResearchQuestionText(c.question), repos: ((c.scope && c.scope.repos) || []).slice() }, returnFocus: document.activeElement };
      nousRenderExperimentDialog();
      nousFocusExperimentQuestion();
    }

    // Close + reset, then return focus to the button that opened the dialog.
    // The panel may have re-rendered meanwhile, so fall back to the matching
    // trigger by selector when the remembered element is gone.
    function nousCancelNewExperiment() {
      const { editId, returnFocus } = _nousNewExp;
      _nousNewExp = { open: false, conflict: null, pending: false, editId: 0, original: null, returnFocus: null };
      const overlay = document.getElementById(NOUS_EXP_DIALOG_ID);
      if (overlay) overlay.remove();
      let target = returnFocus && returnFocus.isConnected ? returnFocus : null;
      if (!target) target = editId ? document.querySelector(`.nous-exp-icon-edit[data-arg0="${editId}"]`) : document.querySelector('[data-action="nousOpenNewExperiment"]');
      if (target && typeof target.focus === 'function') target.focus();
    }

    function nousSetExperimentDialogPending(pending) {
      _nousNewExp.pending = !!pending;
      const btn = document.querySelector(`#${NOUS_EXP_DIALOG_ID} [data-action="nousSubmitNewExperiment"]`);
      if (btn) btn.disabled = !!pending;
    }

    function nousNarrowScope() {
      const conf = _nousNewExp.conflict || {};
      const shared = new Set((Array.isArray(conf.shared) ? conf.shared : []).map(r => String(r).toLowerCase()));
      document.querySelectorAll('#nous-new-exp .nous-ne-pill').forEach(p => {
        const repo = String(p.dataset.repo || '').toLowerCase();
        if (shared.size === 0 || shared.has(repo)) nousRepoPillSetState(p, false);
      });
      const warn = document.querySelector(`#${NOUS_EXP_DIALOG_ID} .nous-conflict-warn`);
      if (warn) warn.remove();
      _nousNewExp.conflict = null;
      nousFocusExperimentQuestion();
    }

    // nousEditIsMaterial: a changed question or repo set invalidates the
    // baseline; a hypothesis-only edit does not.
    function nousEditIsMaterial(original, v) {
      const norm = (q) => String(q || '').trim().toLowerCase().split(/\s+/).join(' ');
      if (norm(original.question) !== norm(v.question)) return true;
      const a = new Set((original.repos || []).map(r => String(r).toLowerCase()));
      const b = new Set((v.repos || []).map(r => String(r).toLowerCase()));
      if (a.size !== b.size) return true;
      for (const r of a) if (!b.has(r)) return true;
      return false;
    }

    const NOUS_RESTART_CONFIRM = {
      title: 'Restart experiment?',
      message: `Changing the question or scope restarts this experiment: baseline resets to 0/${NOUS_BASELINE_TARGET_DEFAULT} (≈7 days) and hypotheses/proposals so far are archived with the old version. Continue?`,
      confirmLabel: 'Restart experiment',
      cancelLabel: 'Keep as is',
      danger: false,
    };

    async function nousSubmitNewExperiment() {
      if (!experimentLabEnabled()) return;
      const v = nousNewExperimentValues();
      if (!v) return;
      if (!v.question.trim()) { showToast('Enter a research question first', 'error'); return; }
      const editId = _nousNewExp.editId || 0;
      let restart = false;
      if (editId && _nousNewExp.original && nousEditIsMaterial(_nousNewExp.original, v)) {
        if (!(await hiveConfirm(NOUS_RESTART_CONFIRM))) { showToast('Experiment kept as is', 'info'); return; }
        restart = true;
      }
      nousSetExperimentDialogPending(true);
      try {
        const body = { question: v.question, hypothesis: v.hypothesis, scope: { repos: v.repos } };
        let r, j;
        if (editId) {
          if (restart) body.restart = true;
          ({ r, j } = await nousPatchCampaign(editId, body));
          // The server saw a material change we did not (stale card): ask now.
          if (r.status === 409 && j.code === 'restart_required') {
            nousSetExperimentDialogPending(false);
            if (!(await hiveConfirm(NOUS_RESTART_CONFIRM))) { showToast('Experiment kept as is', 'info'); return; }
            nousSetExperimentDialogPending(true);
            body.restart = true;
            ({ r, j } = await nousPatchCampaign(editId, body));
          }
        } else {
          ({ r, j } = await nousPostCampaign(body));
        }
        if (r.status === 409 && j.code === 'scope_conflict') {
          _nousNewExp.conflict = j;
          _nousNewExp.pending = false;
          nousRenderExperimentDialog();
          nousRestoreNewExperimentValues(v);
          return;
        }
        if (r.ok && j.ok) {
          const cid = (j.campaign && j.campaign.id) || '';
          if (!editId) showToast(`Experiment #${cid} started — baseline starts at 0`, 'success');
          else if (j.status === 'restarted') showToast(`Experiment #${editId} archived as v${(j.archived && j.archived.version) || 1}; #${cid} restarted at baseline 0`, 'success');
          else showToast(`Experiment #${editId} updated — baseline kept`, 'success');
          nousCancelNewExperiment();
          fetchNousStatus();
          return;
        }
        showToast(j.error || (editId ? 'Failed to update experiment' : 'Failed to start experiment'), 'error');
      } catch (e) { showToast('Failed: ' + e.message, 'error'); }
      nousSetExperimentDialogPending(false);
    }

    // nousPostCampaign files a new experiment; callers branch on r.status
    // (409 scope_conflict) and r.ok like every other Lab mutation (#12039).
    async function nousPostCampaign(body) {
      const r = await fetch('/api/nous/campaigns', {
        method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(body),
      });
      const j = await r.json().catch(() => ({}));
      return { r, j, ok: r.ok && !!j.ok };
    }

    async function nousPatchCampaign(id, body) {
      const r = await fetch('/api/nous/campaigns/' + encodeURIComponent(id), {
        method: 'PATCH', headers: _inceptionAuthHeaders(), body: JSON.stringify(body),
      });
      const j = await r.json().catch(() => ({}));
      return { r, j };
    }

    async function nousPauseConflictAndStart(id) {
      const v = nousNewExperimentValues();
      try {
        const { r, j } = await nousPatchCampaign(id, { action: 'pause' });
        if (!r.ok || !j.ok) { showToast(j.error || `Failed to pause experiment #${id}`, 'error'); return; }
        showToast(`Experiment #${id} paused`, 'info');
      } catch (e) { showToast('Failed: ' + e.message, 'error'); return; }
      _nousNewExp.conflict = null;
      nousRenderExperimentDialog();
      nousRestoreNewExperimentValues(v);
      await nousSubmitNewExperiment();
    }

    const NOUS_CAMPAIGN_ACTION_CONFIRM = {
      archive: 'Archive this experiment? It stops observing and its results become read-only under Previous experiments.',
    };

    async function nousCampaignAction(id, action) {
      if (!experimentLabEnabled()) return;
      if (NOUS_CAMPAIGN_ACTION_CONFIRM[action] && !(await hiveConfirm(NOUS_CAMPAIGN_ACTION_CONFIRM[action]))) return;
      try {
        const { r, j } = await nousPatchCampaign(id, { action });
        if (r.status === 409 && j.code === 'scope_conflict') {
          const other = j.conflict || {};
          showToast(`Cannot resume #${id}: scope conflicts with running experiment #${other.id || '?'}. Pause it or narrow the scope first.`, 'error');
          return;
        }
        if (r.ok && j.ok) {
          showToast(`Experiment #${id} ${action === 'archive' ? 'archived' : action + 'd'}`, 'success');
          fetchNousStatus();
        } else {
          showToast(j.error || `Failed to ${action} experiment`, 'error');
        }
      } catch (e) { showToast('Failed: ' + e.message, 'error'); }
    }

    // nousGenerateProposals is the card's primary action once a baseline is
    // complete in Observe mode: it moves the trust ladder to Suggest (never
    // further) so the experiment loop may design proposals for review.
    async function nousGenerateProposals(id) {
      if (!experimentLabEnabled()) return;
      try {
        const r = await fetch('/api/nous/mode', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ mode: 'suggest' }),
        });
        const j = await r.json().catch(() => ({}));
        if (!r.ok) { showToast(j.error || 'Could not switch the lab to Suggest', 'error'); return; }
        showToast(`Lab switched to Suggest — proposals for experiment #${id} appear here for review once designed`, 'success');
        fetchNousStatus();
      } catch (e) { showToast('Could not switch the lab to Suggest: ' + e.message, 'error'); }
    }

    function renderNous() {
      if (!experimentLabEnabled()) return;
      const panel = document.getElementById('nous-panel');
      if (!panel || !_nousCache) return;
      const s = _nousCache.status || {};
      const ledger = _nousCache.ledger || [];
      const principles = _nousCache.principles || [];
      const mode = s.mode || 'observe';
      const phases = s.phases || {};
      const modeColors = { observe: 'var(--blue)', suggest: 'var(--yellow)', evolve: 'var(--green)' };

      // Not configured server-side: explain what the lab is instead of a
      // control surface whose actions would all fail.
      if (s.status === 'not_configured') {
        panel.innerHTML = _nousExplainerCard();
        return;
      }
      // The research question lives on each experiment card, not in the
      // section header (#12532); the header keeps the mode · N summary.
      if (typeof refreshDashboardSectionSummaries === 'function' && window._lastStatus) refreshDashboardSectionSummaries(window._lastStatus);

      // Update sidebar config — only show at ACMM L4+
      const NOUS_SIDEBAR_MIN_LEVEL = 4;
      const currentACMM = window._lastStatus ? (window._lastStatus.acmmLevel || 0) : 0;
      const sbConfig = document.getElementById('nous-sidebar-config');
      const sbMode = document.getElementById('nous-sb-mode');
      const sbPhase = document.getElementById('nous-sb-phase');
      if (sbConfig) {
        sbConfig.style.display = currentACMM >= NOUS_SIDEBAR_MIN_LEVEL ? '' : 'none';
        if (sbMode) { sbMode.textContent = mode; sbMode.style.color = modeColors[mode] || 'var(--muted)'; }
        if (sbPhase) {
          const rp = (phases.repo || {}).phase || 'IDLE';
          sbPhase.textContent = 'repo:' + rp;
        }
      }

      let html = '';

      // Fresh lab (no baseline, no experiments, no principles yet):
      // lead with the explainer so a new hive isn't greeted by bare zeros.
      const isFreshLab = (s.snapshotCount || 0) === 0 && ledger.length === 0 && principles.length === 0;
      if (isFreshLab) {
        html += `<div style="margin-bottom:14px">${_nousExplainerCard()}</div>`;
      }

      // Mode toggle. Mode is owner-only on the server
      // (requireOwnerRole), so a viewer without that role gets a disabled
      // button with the reason instead of a click that silently 403s (#12039).
      const canSetNous = nousCanChangeMode();
      html += '<div class="nous-mode-toggle">';
      for (const m of NOUS_MODES) {
        const active = m === mode ? ` active-${m}` : '';
        html += `<button type="button" class="nous-mode-btn${active}" title="${escapeHtml(canSetNous ? NOUS_MODE_TITLES[m] : NOUS_OWNER_ONLY_TITLE)}" aria-pressed="${m === mode}" data-action="nousSetMode" data-mode="${m}"${canSetNous ? '' : ' disabled'}>${m.charAt(0).toUpperCase() + m.slice(1)}</button>`;
      }
      html += '</div>';

      // Phase progress (the lab is repo-only, #12207)
      const NOUS_PHASES = ['INIT', 'FRAMING', 'DESIGN', 'DESIGN_REVIEW', 'EXECUTING', 'ANALYSIS', 'FINDINGS_REVIEW', 'EXTRACTION'];
      const NOUS_PHASE_COUNT = NOUS_PHASES.length;
      {
        const phaseInfo = phases.repo || { phase: 'IDLE', iteration: 0 };
        if (phaseInfo.phase !== 'IDLE') {
          const phaseIdx = NOUS_PHASES.indexOf(phaseInfo.phase);
          const phasePct = phaseIdx >= 0 ? Math.round(((phaseIdx + 1) / NOUS_PHASE_COUNT) * 100) : 0;
          html += `<div style="font-size:0.72rem;color:var(--muted);margin-bottom:var(--sp-4)">`;
          html += `<strong>repo</strong>: ${phaseInfo.phase} (iteration ${phaseInfo.iteration})`;
          html += `<div class="nous-progress"><div class="nous-progress-fill" style="width:${phasePct}%;background:#7c3aed"></div></div>`;
          html += '</div>';
        }
      }

      // Stats row. With a campaign (#12501) the snapshot tile is the active
      // campaign's baseline, not the whole observation history: a new
      // research question starts a new experiment at 0 / 672.
      const camp = s.campaign || null;
      html += '<div class="nous-stat-grid">';
      if (camp) {
        html += `<div class="nous-stat"><div class="val">${Number(camp.snapshots) || 0}</div><div class="lbl">Baseline snapshots · experiment #${Number(camp.id) || 0}</div></div>`;
        html += `<div class="nous-stat"><div class="val">${s.principleCount || 0}</div><div class="lbl">Principles${nousPrinciplesInfo(s.principlesExplainer)}</div></div>`;
        html += `<div class="nous-stat"><div class="val">${Number(camp.experiments) || 0}</div><div class="lbl">Hypotheses this experiment</div></div>`;
      } else {
        html += `<div class="nous-stat"><div class="val">${s.snapshotCount || 0}</div><div class="lbl">State snapshots</div></div>`;
        html += `<div class="nous-stat"><div class="val">${s.principleCount || 0}</div><div class="lbl">Principles${nousPrinciplesInfo(s.principlesExplainer)}</div></div>`;
        html += `<div class="nous-stat"><div class="val">${ledger.length}</div><div class="lbl">Experiments</div></div>`;
      }
      html += '</div>';

      // Running experiments as Inception-style cards (#12532).
      if (Array.isArray(s.campaigns)) html += nousExperimentCards(s.campaigns, s);

      // Snapshot summary — what's being collected
      const ss = s.snapshotSummary;
      if (ss) {
        html += '<div class="nous-card mt-5">';
        html += '<h3>State history</h3>';
        const timeRange = ss.firstTs && ss.latestTs
          ? `${formatDashboardDate(ss.firstTs, {month:'short',day:'numeric'})} – ${formatDashboardDateTime(ss.latestTs, {month:'short',day:'numeric',hour:'numeric',minute:'2-digit',timeZoneName:'short'})}`
          : '';
        if (timeRange) html += `<div style="font-size:0.7rem;color:var(--muted);margin-bottom:var(--sp-4)">${timeRange} · ${ss.recentWindow} recent samples</div>`;

        html += '<div style="display:grid;grid-template-columns:1fr 1fr;gap:var(--sp-4);font-size:0.78rem">';

        // Current values
        const cur = ss.latest || {};
        const regimeBadge = cur.mode ? `<span style="display:inline-block;padding:1px 6px;border-radius:var(--r-sm);font-size:0.7rem;background:${cur.mode==='quiet'?'#dbeafe':cur.mode==='busy'?'#fef3c7':'#fee2e2'};color:${cur.mode==='quiet'?'#1e40af':cur.mode==='busy'?'#92400e':'#991b1b'}">${cur.mode}</span>` : '';
        html += `<div style="padding:var(--sp-4);background:var(--bg);border-radius:var(--r)"><div style="font-size:0.7rem;color:var(--muted)">Current Regime</div><div style="font-weight:600;margin-top:var(--sp-1)">${regimeBadge}</div></div>`;
        html += `<div style="padding:var(--sp-4);background:var(--bg);border-radius:var(--r)"><div style="font-size:0.7rem;color:var(--muted)">Queue Depth</div><div style="font-weight:600;margin-top:var(--sp-1)">${cur.queue_depth != null ? cur.queue_depth : '–'}</div></div>`;
        html += `<div style="padding:var(--sp-4);background:var(--bg);border-radius:var(--r)"><div style="font-size:0.7rem;color:var(--muted)">MTTR (avg)</div><div style="font-weight:600;margin-top:var(--sp-1)">${cur.mttr_avg != null ? cur.mttr_avg + ' min' : '–'}</div></div>`;
        html += `<div style="padding:var(--sp-4);background:var(--bg);border-radius:var(--r)"><div style="font-size:0.7rem;color:var(--muted)">Budget Used</div><div style="font-weight:600;margin-top:var(--sp-1)">${cur.budget_pct != null ? cur.budget_pct + '%' : '–'}</div></div>`;
        html += '</div>';

        // Ranges from recent window
        const qd = ss.queue_depth || {};
        const mt = ss.mttr_avg || {};
        if (qd.min != null || mt.min != null) {
          html += '<div style="margin-top:var(--sp-4);font-size:0.72rem;color:var(--muted)">';
          html += '<strong>Recent ranges:</strong> ';
          if (qd.min != null) html += `Queue ${qd.min}–${qd.max} (avg ${qd.avg})`;
          if (qd.min != null && mt.min != null) html += ' · ';
          if (mt.min != null) html += `MTTR ${mt.min}–${mt.max} min (avg ${mt.avg})`;
          html += '</div>';
        }

        // Regime distribution
        const regimes = ss.regimes || {};
        const regimeKeys = Object.keys(regimes);
        if (regimeKeys.length > 0) {
          html += '<div style="margin-top:var(--sp-3);font-size:0.72rem;color:var(--muted)">';
          html += '<strong>Regime distribution:</strong> ';
          html += regimeKeys.map(r => `${r} ${regimes[r]}/${ss.recentWindow}`).join(', ');
          html += '</div>';
        }

        html += '</div>';
      }

      // Data collection note (no global progress bar: the observation
      // history is a running total, not progress — each card carries the
      // real per-experiment baseline).
      html += '<div class="nous-card mt-5">';
      html += '<h3>Data collection</h3>';
      html += nousDataCollectionNote(s, camp);
      // Proposals and runs, only when there are any: the empty state lives
      // on the cards (stage line + next action).
      if (Array.isArray(s.experiments) && s.experiments.length) {
        html += '<h3 class="mt-5">Proposals and runs</h3>';
        html += nousExperimentProgressRows(s.experiments);
      }
      // Show experiment proposals from ledger
      const dryRuns = ledger.filter(e => e.type === 'dry_run').slice(-5);
      if (dryRuns.length > 0) {
        html += `<h3 class="mt-5">${mode === 'observe' ? 'Would Have Tested' : 'Recent Proposals'}</h3>`;
        for (const dr of dryRuns) {
          const paramKeys = dr.params ? Object.entries(dr.params).map(([k,v]) => `<code style="font-size:0.7rem;background:var(--bg);padding:1px 4px;border-radius:3px">${escapeHtml(String(k))}=${escapeHtml(String(v))}</code>`).join(' ') : '';
          html += `<div style="font-size:var(--fs-sm);padding:var(--sp-3) var(--sp-0);border-bottom:1px solid var(--border)">`;
          html += `<div><strong>${escapeHtml(dr.id || '?')}</strong></div>`;
          html += `<div class="text-muted mt-1">${escapeHtml(dr.hypothesis || 'no description')}</div>`;
          if (paramKeys) html += `<div class="mt-2">${paramKeys}</div>`;
          if (dr.predicted) {
            const preds = Object.entries(dr.predicted).map(([k,v]) => `${escapeHtml(k.replace(/_/g,' '))}: ${v > 0 ? '+' : ''}${v}%`).join(', ');
            html += `<div style="font-size:0.7rem;color:#7c3aed;margin-top:var(--sp-1)">Predicted: ${preds}</div>`;
          }
          html += '</div>';
        }
      }
      html += '</div>';

      // Active experiment card
      if (s.activeExperiment) {
        const exp = s.activeExperiment;
        html += '<div class="nous-experiment-live">';
        html += `<h4>Active Experiment: ${escapeHtml(exp.id)}</h4>`;
        html += `<div class="nous-progress"><div class="nous-progress-fill" style="width:${exp.progressPct}%;background:var(--green)"></div></div>`;
        html += `<div style="font-size:0.72rem;color:var(--muted)">${exp.progressPct}% complete — ${Math.round((exp.ttlSec - exp.elapsed) / 60)}min remaining</div>`;
        html += '<div style="display:flex;gap:var(--sp-6);margin-top:var(--sp-4);font-size:var(--fs-sm)">';
        html += `<span>Queue limit: ${exp.fastFail.queueMax}</span>`;
        html += `<span>MTTR limit: ${exp.fastFail.mttrMax}min</span>`;
        html += '</div>';
        html += `<button class="hv-btn btn-danger nous-btn nous-btn-abort" data-action="nousAbort" title="Immediately stop the running experiment and withdraw its proposed repo change. Results collected so far will be saved for analysis.">Abort Experiment</button>`;
        html += '</div>';
      }

      // Pending experiment (suggest mode)
      if (s.pending && mode === 'suggest') {
        const p = s.pending;
        const runLink = p.run || (p.run_key ? { key: p.run_key, stage: p.run_stage || 'spec' } : null);
        html += '<div class="nous-pending-card">';
        html += `<h4>Pending Proposal: ${escapeHtml(p.id || 'experiment')}</h4>`;
        html += `<div style="font-size:0.78rem;margin-bottom:var(--sp-4)">${escapeHtml(p.hypothesis || 'No hypothesis description')}</div>`;
        if (runLink && runLink.key) {
          html += `<div class="text-muted">Spektacular run: <button class="btn" data-action="openRunDetail" data-arg0="${escapeHtml(runLink.key)}">${escapeHtml(runLink.key)}</button> <span class="status-badge">${escapeHtml(runLink.stage || 'spec')}</span></div>`;
        }
        if (p.params) {
          html += '<table class="nous-pending-params"><tbody>';
          for (const [k, v] of Object.entries(p.params)) {
            html += `<tr><td class="text-muted">${escapeHtml(k)}</td><td><strong>${escapeHtml(String(v))}</strong></td></tr>`;
          }
          html += '</tbody></table>';
        }
        if (p.predicted) {
          html += `<div style="font-size:0.72rem;color:var(--yellow)">Predicted: ${escapeHtml(JSON.stringify(p.predicted))}</div>`;
        }
        if (runLink && runLink.key) {
          html += '<div class="text-muted" style="margin-top:10px">Approved — the proposal is running as the Spektacular run above.</div>';
        } else {
          html += '<div style="margin-top:10px">';
          html += '<button class="hv-btn btn-primary nous-btn nous-btn-approve" data-action="nousApprove" title="Approve this experiment proposal and start running it. Nous will open the proposed repo change and measure it for the configured experiment duration.">Apply</button>';
          html += '<button class="hv-btn btn-danger nous-btn nous-btn-reject" data-action="nousReject" title="Reject this experiment proposal. Nous will discard it and generate a new hypothesis based on collected data.">Reject</button>';
          html += '</div>';
        }
        html += '</div>';
      }

      html += renderNousVerdictCard(s.verdict, mode);
      html += nousResultsRows(s.results || []);
      html += nousArchivedCampaigns((s.campaigns || []).filter(c => c && c.status === 'archived'));

      // Principles
      if (principles.length > 0) {
        html += '<div class="nous-card mt-5">';
        html += '<h3>Principles</h3>';
        const sorted = [...principles].sort((a, b) => (b.confidence || 0) - (a.confidence || 0));
        for (const p of sorted) {
          const conf = Math.round((p.confidence || 0) * 100);
          const barColor = conf >= 80 ? 'var(--green)' : conf >= 50 ? 'var(--yellow)' : 'var(--red)';
          html += `<div class="nous-principle" data-nous-principle="${escapeHtml(p.id || '')}">`;
          html += `<div class="nous-confidence-bar"><div class="nous-confidence-fill" style="width:${conf}%;background:${barColor}"></div></div>`;
          html += `<div class="nous-principle-text">${escapeHtml(p.text || p.id)}</div>`;
          html += nousChip('type', p.type) + nousChip('status', p.status);
          html += `<div class="nous-principle-score">${conf}%</div>`;
          html += '</div>';
        }
        html += '</div>';
      }

      // Experiment timeline
      if (ledger.length > 0) {
        html += '<div class="nous-card mt-5">';
        html += '<h3>Experiment History</h3>';
        html += '<div class="nous-timeline">';
        const TIMELINE_MAX_BARS = 30;
        const recent = ledger.slice(-TIMELINE_MAX_BARS);
        for (const entry of recent) {
          const statusColors = { pending: 'var(--muted)', active: 'var(--blue)', completed: 'var(--green)', aborted: 'var(--red)' };
          const typeColors = { dry_run: 'var(--muted)', analysis: '#7c3aed', shadow_analysis: '#a78bfa' };
          const color = typeColors[entry.type] || statusColors[entry.status] || 'var(--muted)';
          const outcome = entry.outcome || entry.status || entry.type;
          html += `<div class="nous-timeline-bar" data-nous-type="${escapeHtml(entry.type || '')}" data-nous-status="${escapeHtml(entry.status || '')}" style="height:${20 + Math.random() * 20}px;background:${color}" title="${escapeHtml(nousEntryLabel(entry) + ': ' + outcome)}"></div>`;
        }
        html += '</div>';
        html += '<div style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2);display:flex;gap:var(--sp-5)">';
        html += '<span><span class="text-muted">■</span> dry run / pending</span>';
        html += '<span><span class="text-info">■</span> active</span>';
        html += '<span><span class="text-success">■</span> completed</span>';
        html += '<span><span class="text-danger">■</span> aborted</span>';
        html += '<span><span style="color:#7c3aed">■</span> analysis</span>';
        html += '</div>';
        // Recent rounds, newest first, with the schema's type and status.
        html += '<div class="nous-recent-rounds mt-4">';
        for (const entry of ledger.slice(-NOUS_RECENT_ROUNDS).reverse()) {
          html += '<div class="nous-principle nous-round">';
          html += `<div class="nous-principle-text">${escapeHtml(nousEntryLabel(entry))}`;
          if (entry.outcome) html += ` <span class="text-muted">— ${escapeHtml(String(entry.outcome).replace(/_/g, ' '))}</span>`;
          html += '</div>';
          html += nousChip('type', entry.type) + nousChip('status', entry.status);
          html += '</div>';
        }
        html += '</div></div>';
      }

      // Recommendations badge
      if (s.hasRecommendations && s.recommendations) {
        const recs = s.recommendations.recommendations || [];
        html += '<div class="nous-card" style="margin-top:var(--sp-5);border-color:#7c3aed">';
        html += `<h3 style="color:#7c3aed">Pending Recommendations (${recs.length})</h3>`;
        for (const r of recs) {
          html += `<div style="font-size:0.78rem;padding:var(--sp-2) var(--sp-0);border-bottom:1px solid var(--border)">`;
          html += `<strong>${escapeHtml(r.var)}</strong>: ${escapeHtml(String(r.current || '?'))} → <strong>${escapeHtml(String(r.proposed))}</strong>`;
          html += `<div style="font-size:0.7rem;color:var(--muted)">${escapeHtml(r.rationale)} (${Math.round((r.confidence || 0) * 100)}% confidence)</div>`;
          html += '</div>';
        }
        html += '</div>';
      }

      // The add/edit dialog lives outside the panel, so a re-render never
      // disturbs a draft (#12697).
      setIfChanged(panel, html);
    }

    // Verdict recommendations and the tone each takes on the card. Mirrors
    // nousengine's vocabulary (deploy / deploy_with_caveats / fall_back_to_baseline).
    const NOUS_VERDICT_TONES = { deploy: 'ok', deploy_with_caveats: 'warn', fall_back_to_baseline: 'error' };
    const NOUS_PROMOTE_TARGETS = {
      issue: { label: 'Promote → Issue', confirm: 'Open an issue with this verdict', desc: 'Open an issue in the target repo carrying the verdict, its evidence and caveats. Nothing is applied; the issue is where the change gets discussed.' },
      'spektacular-run': { label: 'Promote → Spektacular run', confirm: 'Start a Spektacular run for this verdict', desc: 'Open (or reuse) the verdict issue and admit a Spektacular spec-stage run against it. The run follows the normal review path; nothing merges on its own.' },
    };

    function nousVerdictLabel(recommendation) {
      return String(recommendation || 'no recommendation').replace(/_/g, ' ');
    }

    // renderNousVerdictCard shows the engine's final recommendation for the
    // campaign and the owner's manual Promote buttons (#11699). The server
    // never promotes on its own: every button goes through a confirm dialog
    // and an audited, owner-only POST /api/nous/promote.
    function renderNousVerdictCard(verdict, mode) {
      if (!verdict || !verdict.recommendation) return '';
      const v = verdict;
      const tone = NOUS_VERDICT_TONES[v.recommendation] || 'neutral';
      const promotions = v.promotions || {};
      const targets = Array.isArray(v.targets) && v.targets.length ? v.targets : Object.keys(NOUS_PROMOTE_TARGETS);
      let html = `<div class="nous-card nous-verdict-card mt-5" data-nous-verdict="${escapeHtml(v.recommendation)}" data-nous-campaign="${escapeHtml(v.campaign || '')}">`;
      html += `<h3>Verdict <span class="status-badge nous-verdict-chip" data-status="${tone}" title="recommendation: ${escapeHtml(v.recommendation)}">${escapeHtml(nousVerdictLabel(v.recommendation))}</span></h3>`;
      html += '<div class="nous-verdict-head">';
      if (v.measured_against_baseline) {
        html += `<span class="nous-verdict-score">Score <strong>${escapeHtml(String(v.score))}</strong> vs. baseline <strong>${escapeHtml(String(v.baseline))}</strong></span>`;
      } else {
        html += `<span class="nous-verdict-score">Score <strong>${escapeHtml(String(v.score))}</strong></span>`;
        html += '<span class="status-badge nous-verdict-unmeasured" data-status="warn" title="The campaign had no objective or no baseline measurements, so this score does not show whether the change beats what is running today.">not measured against baseline</span>';
      }
      if (v.campaign) html += `<span class="text-muted nous-verdict-label">campaign: ${escapeHtml(v.campaign)}</span>`;
      html += '</div>';
      if (v.summary) html += `<div class="nous-verdict-summary">${escapeHtml(v.summary)}</div>`;
      const list = (title, items, cls) => {
        if (!Array.isArray(items) || items.length === 0) return '';
        return `<div class="text-muted nous-verdict-label">${title}</div><ul class="nous-verdict-list ${cls}">` + items.map(i => `<li>${escapeHtml(String(i))}</li>`).join('') + '</ul>';
      };
      html += list('Evidence', v.citations, 'nous-verdict-citations');
      html += list('Caveats', v.caveats, 'nous-verdict-caveats');
      if (v.report) html += `<div class="text-muted nous-verdict-label">Report: <code>${escapeHtml(v.report)}</code></div>`;

      const promoted = Object.keys(promotions);
      if (promoted.length) {
        html += '<div class="nous-verdict-promotions">';
        for (const target of promoted) {
          const p = promotions[target] || {};
          const who = p.promoted_by ? ` by ${escapeHtml(p.promoted_by)}` : '';
          let where = '';
          if (p.run_key) where = `Spektacular run <button class="btn" data-action="openRunDetail" data-arg0="${escapeHtml(p.run_key)}">${escapeHtml(p.run_key)}</button>`;
          else if (p.url) where = `<a href="${escapeHtml(p.url)}" target="_blank" rel="noopener">${escapeHtml(p.repo || '')}#${escapeHtml(String(p.number || ''))}</a>`;
          else if (p.number) where = `${escapeHtml(p.repo || '')}#${escapeHtml(String(p.number))}`;
          html += `<div class="nous-verdict-promotion" data-nous-promoted="${escapeHtml(target)}">Promoted to ${escapeHtml(target.replace(/-/g, ' '))}${who}${where ? ': ' + where : ''}</div>`;
        }
        html += '</div>';
      }

      html += '<div class="nous-verdict-actions">';
      for (const target of targets) {
        const meta = NOUS_PROMOTE_TARGETS[target];
        if (!meta) continue;
        const done = !!promotions[target];
        html += `<button class="hv-btn btn-primary nous-btn nous-btn-promote" data-action="nousPromote" data-arg0="${escapeHtml(target)}" data-nous-target="${escapeHtml(target)}" title="${escapeHtml(done ? 'Already promoted to ' + target.replace(/-/g, ' ') : meta.desc)}"${done ? ' disabled' : ''}>${escapeHtml(meta.label)}</button>`;
      }
      html += '<span class="text-muted nous-verdict-note">Promotion is manual, owner-only and audited.</span>';
      html += '</div>';
      html += '</div>';
      return html;
    }

    async function nousPromote(target) {
      if (!experimentLabEnabled()) return;
      const meta = NOUS_PROMOTE_TARGETS[target];
      if (!meta) { showToast('Unknown promotion target: ' + target, 'error'); return; }
      const v = (_nousCache && _nousCache.status && _nousCache.status.verdict) || {};
      const repo = v.target_repo ? ` in ${v.target_repo}` : '';
      const ok = await hiveConfirm({
        title: meta.confirm,
        message: `${meta.desc} This promotes the "${nousVerdictLabel(v.recommendation)}" verdict${repo} and is recorded in the audit log.`,
        confirmLabel: 'Promote',
      });
      if (!ok) return;
      try {
        const r = await fetch('/api/nous/promote', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ target }),
        });
        const j = await r.json();
        if (r.ok && j.ok) {
          const p = j.promotion || {};
          showToast(p.run_key ? 'Spektacular run admitted: ' + p.run_key : 'Issue opened: ' + (p.repo || '') + '#' + (p.number || ''), 'success');
        } else if (r.status === 409 && j.status === 'already_promoted') {
          showToast('Already promoted to ' + target.replace(/-/g, ' '), 'info');
        } else {
          showToast(j.error || 'Promote failed', 'error');
        }
        fetchNousStatus();
      } catch (e) { showToast('Promote failed: ' + e.message, 'error'); }
    }

    // Experiment Lab mode vocabulary, shared by the section's segmented
    // buttons and the Trust Ladder in the ⚙️ config dialog so both surfaces
    // dispatch the same nousSetMode handler (#12039).
    const NOUS_MODES = ['observe', 'suggest', 'evolve'];
    const NOUS_MODE_ORDER = { observe: 0, suggest: 1, evolve: 2 };
    const NOUS_MODE_TITLES = {
      observe: 'Data collection only — no changes proposed',
      suggest: 'Experiments require your approval',
      evolve: 'Fully autonomous experimentation',
    };
    const NOUS_OWNER_ONLY_TITLE = 'Owner access required to change the Experiment Lab mode';

    // Same role check the Promote buttons and the fleet breaker use: the
    // server enforces requireOwnerRole on PUT /api/nous/mode.
    function nousCanChangeMode() {
      return dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
    }

    // Parse a JSON error body without throwing on an empty/non-JSON reply.
    async function nousErrorFrom(r, fallback) {
      let j = null;
      try { j = await r.json(); } catch (_) {}
      if (j && j.error) return j.error;
      if (r.status === 403) return 'Owner access required';
      if (r.status === 401) return 'Not signed in';
      return fallback + ' (HTTP ' + r.status + ')';
    }

    // Both entry points (section buttons and dialog Trust Ladder) re-render
    // from the server's status so the active state can never drift from it.
    function nousAfterModeChange(status) {
      if (_nousCache && _nousCache.status && status) {
        if (status.mode) _nousCache.status.mode = status.mode;
        renderNous();
      }
      fetchNousStatus();
      if (status && status.mode) nousRefreshTrustLadder(status.mode);
    }

    // Update the config dialog's Trust Ladder in place so an open dialog
    // reflects the new level without re-rendering (and losing) unsaved edits.
    function nousRefreshTrustLadder(mode) {
      document.querySelectorAll('#nous-config-overlay .nous-trust-step').forEach(btn => {
        const isCurrent = btn.dataset.mode === mode;
        btn.classList.toggle('current', isCurrent);
        btn.setAttribute('aria-pressed', String(isCurrent));
      });
    }

    async function nousSetMode(mode) {
      if (!experimentLabEnabled()) return;
      if (NOUS_MODES.indexOf(mode) < 0) { showToast('Unknown mode: ' + mode, 'error'); return; }
      if (!nousCanChangeMode()) { showToast(NOUS_OWNER_ONLY_TITLE, 'error'); return; }
      const current = (_nousCache && _nousCache.status && _nousCache.status.mode) || 'observe';
      if (mode === current) return;
      let force = false;
      if (NOUS_MODE_ORDER[mode] < NOUS_MODE_ORDER[current]) {
        if (!(await hiveConfirm({ title: 'Lower the trust level?', message: `Switch from ${current} to ${mode}? This will stop any active experiment.`, confirmLabel: 'Switch to ' + mode, danger: true }))) return;
        force = true;
      }
      try {
        const r = await fetch('/api/nous/mode', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ mode, force }),
        });
        if (!r.ok) { showToast('Failed to change mode: ' + (await nousErrorFrom(r, 'request rejected')), 'error'); return; }
        const j = await r.json().catch(() => ({}));
        showToast('Experiment Lab mode set to ' + (j.mode || mode), 'success');
        nousAfterModeChange({ mode: j.mode || mode });
      } catch (e) { showToast('Failed to change mode: ' + e.message, 'error'); }
    }

    async function nousApprove() {
      if (!experimentLabEnabled()) return;
      try {
        const r = await fetch('/api/nous/approve', { method: 'POST', headers: _inceptionAuthHeaders() });
        if (!r.ok) { showToast('Approve failed: ' + (await nousErrorFrom(r, 'request rejected')), 'error'); return; }
        const j = await r.json();
        if (j.ok) { showToast('Experiment approved and started', 'success'); fetchNousStatus(); }
        else showToast(j.error || 'Approve failed', 'error');
      } catch (e) { showToast('Approve failed: ' + e.message, 'error'); }
    }

    async function nousReject() {
      if (!experimentLabEnabled()) return;
      if (!(await hiveConfirm('Reject this experiment proposal?'))) return;
      try {
        const r = await fetch('/api/nous/abort', { method: 'POST', headers: _inceptionAuthHeaders() });
        if (!r.ok) { showToast('Reject failed: ' + (await nousErrorFrom(r, 'request rejected')), 'error'); return; }
        showToast('Proposal rejected', 'success');
        fetchNousStatus();
      } catch (e) { showToast('Reject failed: ' + e.message, 'error'); }
    }

    async function nousAbort() {
      if (!experimentLabEnabled()) return;
      if (!(await hiveConfirm('Abort the active experiment? Its proposed repo change will be withdrawn.'))) return;
      try {
        const r = await fetch('/api/nous/abort', { method: 'POST', headers: _inceptionAuthHeaders() });
        if (!r.ok) { showToast('Abort failed: ' + (await nousErrorFrom(r, 'request rejected')), 'error'); return; }
        const j = await r.json();
        if (j.ok) { showToast('Experiment aborted: ' + j.aborted, 'success'); fetchNousStatus(); }
        else showToast(j.error || 'Abort failed', 'error');
      } catch (e) { showToast('Abort failed: ' + e.message, 'error'); }
    }

    // ── Nous Config Dialog ──────────────────────────────────────────────────
    let _nousConfig = null;

    async function fetchNousConfig() {
      if (!experimentLabEnabled()) return null;
      try {
        const r = await fetch('/api/nous/config');
        _nousConfig = await r.json();
      } catch (_) { _nousConfig = null; }
      return _nousConfig;
    }

    async function openNousConfig() {
      if (!experimentLabEnabled()) return;
      const overlay = document.getElementById('nous-config-overlay');
      if (!overlay) return;
      overlay.style.display = 'flex';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = nousLabShell('<div class="nous-lab-loading">Loading configuration...</div>', '');

      const cfg = await fetchNousConfig();
      if (!cfg) {
        overlay.innerHTML = nousLabShell('<div class="nous-lab-error">Failed to load config</div>',
          '<button class="hv-btn btn-secondary nous-config-btn nous-config-btn-cancel" data-action="closeNousConfig">Close</button>');
        return;
      }
      renderNousConfig(cfg);
    }

    function closeNousConfig() {
      const overlay = document.getElementById('nous-config-overlay');
      if (overlay) overlay.style.display = 'none';
    }

    // Dialog shell: fixed header (title + Close ×), the body as the single
    // scroller, footer for the action bar. Same recipe as the Knowledge
    // Sources modal (#12000); sizing lives on .nous-lab-dialog.
    function nousLabShell(bodyHtml, footerHtml) {
      let h = '<div class="nous-config-dialog nous-lab-dialog" role="dialog" aria-modal="true" aria-labelledby="nous-lab-title">';
      h += '<h2 id="nous-lab-title"><span class="nous-lab-title">⚙️ Experiment Lab Configuration</span>';
      h += '<button type="button" class="hv-btn btn-icon btn-sm" data-action="closeNousConfig" aria-label="Close Experiment Lab configuration">✕</button></h2>';
      h += `<div class="nous-config-body modal-scroll">${bodyHtml}</div>`;
      if (footerHtml) h += `<div class="nous-config-footer nous-config-btn-bar">${footerHtml}</div>`;
      h += '</div>';
      return h;
    }

    // Section card: eyebrow + heading + one-line description, then content.
    function nousLabCardHead(eyebrow, heading, desc, note) {
      let h = '<div class="nous-lab-card-head">';
      h += `<span class="nous-lab-eyebrow">${escapeHtml(eyebrow)}</span>`;
      h += `<h3>${heading}${note ? ` <span class="nous-lab-h3-note">${escapeHtml(note)}</span>` : ''}</h3>`;
      if (desc) h += `<p class="nous-lab-card-desc">${desc}</p>`;
      h += '</div>';
      return h;
    }

    function renderNousConfig(cfg) {
      const overlay = document.getElementById('nous-config-overlay');
      if (!overlay) return;

      const camp = cfg.campaign || {};
      const output = cfg.output || {};
      const ff = camp.fast_fail || {};
      const sched = camp.schedule || {};
      const principles = cfg.principles || [];
      const projectRepos = cfg.projectRepos || [];
      const repoTargets = cfg.repoTargets || [];
      // The served status is authoritative for the trust level; the campaign
      // file's mode is only a fallback when status has not loaded yet.
      const mode = (_nousCache && _nousCache.status && _nousCache.status.mode) || camp.mode || 'observe';

      const confThresh = configEnvVal(cfg, 'NOUS_CONFIDENCE_THRESHOLD', '0.8');
      const decayRate = configEnvVal(cfg, 'NOUS_DECAY_RATE_PER_WEEK', '0.05');

      let h = '';

      // Trust Ladder
      h += '<section class="nous-config-section">';
      h += nousLabCardHead('Autonomy', 'Trust Ladder', 'How much Nous may do on its own. The current level is highlighted; click a step to change it (owner-only, audited).');
      h += '<div class="nous-trust-ladder" role="group" aria-label="Trust level">';
      const trustSteps = [
        { id: 'observe', label: 'Observe', desc: 'Data collection only', color: 'var(--blue)' },
        { id: 'suggest', label: 'Suggest', desc: 'Experiments need approval', color: 'var(--yellow)' },
        { id: 'evolve', label: 'Evolve', desc: 'Fully autonomous', color: 'var(--green)' },
      ];
      // Each step is a real button wired to the same nousSetMode handler as
      // the section's segmented control; before #12039 these were inert divs.
      // The current step is coloured by .nous-lab-dialog [data-step] rules
      // rather than inline styles so the style ratchet stays flat.
      const canSetTrust = nousCanChangeMode();
      for (const step of trustSteps) {
        const isCurrent = step.id === mode;
        h += `<button type="button" class="nous-trust-step${isCurrent ? ' current' : ''}" data-step="${step.id}" data-action="nousSetMode" data-mode="${step.id}" data-color="${step.color}" aria-pressed="${isCurrent}" title="${escapeHtml(canSetTrust ? NOUS_MODE_TITLES[step.id] : NOUS_OWNER_ONLY_TITLE)}"${canSetTrust ? '' : ' disabled'}>`;
        h += `<span class="step-label">${escapeHtml(step.label)}</span>`;
        h += `<span class="step-desc">${escapeHtml(step.desc)}</span>`;
        h += '</button>';
      }
      h += '</div>';
      h += '<div class="nous-lab-trust-note">Click a step to change the trust level (owner-only, audited). The issue/PR output trail builds evidence for operators to advance it.</div>';
      h += '</section>';

      // Lab-wide guardrails only: experiments (question, hypothesis, scope)
      // are defined via + New experiment on the section, never here.

      // Output: promotion is manual (#11699). Verdicts are written to files
      // and read here; the owner picks Issue or Spektacular run on the
      // verdict card. Only the labels applied to promoted items are config.
      h += '<section class="nous-config-section">';
      h += nousLabCardHead('Verdicts & labels', '📤 Output', 'Where results land and which safety labels promoted items carry.');
      h += '<div class="text-muted nous-config-output-note" title="Nous writes its verdict to meta_findings.json and the dashboard reads it. Promoting a verdict to an issue or a Spektacular run is a manual, owner-only, audited action on the verdict card; there is no automatic output mode.">Verdicts are written to files and shown on the verdict card. Promotion is manual via the card\'s <strong>Promote</strong> buttons (Issue or Spektacular run); nothing is opened or run automatically.</div>';
      h += '<div class="nous-lab-checks">';
      h += `<label class="nous-config-check" title="When enabled, Nous automatically adds the 'hold' label to any issues or PRs it creates. This prevents agents from acting on Nous-generated work items until an operator reviews them."><input type="checkbox" id="nc-auto-hold" ${output.autoHold ? 'checked' : ''}><span>Auto-apply <code>hold</code> label to Nous issues/PRs</span></label>`;
      h += `<label class="nous-config-check" title="When enabled, Nous automatically adds the 'do-not-merge' label to any PRs it creates. This prevents automated merge workflows from merging experiment PRs without operator review."><input type="checkbox" id="nc-auto-dnm" ${output.autoDoNotMerge ? 'checked' : ''}><span>Auto-apply <code>do-not-merge</code> label to Nous PRs</span></label>`;
      h += '</div>';
      h += '</section>';

      // Repo allow-list: which repos an experiment's scope may pick from.
      h += '<section class="nous-config-section">';
      h += nousLabCardHead('Scope', NOUS_REPO_ALLOWLIST_HEADING, NOUS_REPO_ALLOWLIST_COPY + ' Click a repo to toggle it; changes apply on Save.');
      if (projectRepos.length === 0) {
        h += '<div class="nous-lab-empty">No project repos available &mdash; add repos to the hive before targeting experiments.</div>';
      } else {
        h += '<div class="nous-lab-chips" role="group" aria-label="Repos experiments may target">';
        for (const repo of projectRepos) {
          const isActive = repoTargets.includes(repo);
          const shortName = repo.includes('/') ? repo.split('/')[1] : repo;
          // Native <button> so Enter/Space activate via the click dispatcher;
          // the on/off look comes from .is-on/.is-off + aria-pressed (#12520).
          h += `<button type="button" class="${nousRepoPillClass(isActive)}" aria-pressed="${isActive}" data-repo="${escapeHtml(repo)}" data-action="toggleNousRepo" data-arg-types="t" title="${escapeHtml(nousRepoPillTitle(repo, isActive))}">`;
          h += `<span>${escapeHtml(shortName)}</span>`;
          h += '</button>';
        }
        h += '</div>';
        h += `<div class="nous-lab-chips-note" id="nc-repo-summary">${nousRepoSummary(repoTargets.filter(r => projectRepos.includes(r)).length, projectRepos.length)}</div>`;
      }
      h += '</section>';

      // Campaign Parameters
      h += '<section class="nous-config-section">';
      h += nousLabCardHead('Guardrails', '⚡ Fast-Fail Bounds', 'An experiment aborts and reverts the moment any of these bounds is crossed.');
      h += '<div class="nous-config-row">';
      h += `<div><label for="nc-ff-queue" title="If the issue queue depth exceeds this number during an experiment, Nous immediately aborts and reverts. Prevents experiments from causing unacceptable issue buildup. Default: 30.">Queue Depth Max</label><input type="number" id="nc-ff-queue" value="${ff.queue_depth_max || 30}" min="5" max="100" title="Maximum allowed actionable issues before auto-abort (5-100)"></div>`;
      h += `<div><label for="nc-ff-mttr" title="If the mean time to resolution exceeds this many minutes during an experiment, Nous immediately aborts. Prevents experiments from stalling issue resolution. Default: 180 (3 hours).">MTTR Max (min)</label><input type="number" id="nc-ff-mttr" value="${ff.mttr_max_minutes || 180}" min="30" max="1440" title="Maximum allowed MTTR in minutes before auto-abort (30-1440)"></div>`;
      h += `<div><label for="nc-ff-burn" title="If the token burn rate exceeds this percentage of the normal rate during an experiment, Nous aborts. Values above 100% allow higher-than-normal spending. Default: 110%.">Burn Rate Max %</label><input type="number" id="nc-ff-burn" value="${ff.budget_burn_rate_max_pct || 110}" min="80" max="200" title="Maximum burn rate as percentage of normal (80-200)"></div>`;
      h += '</div>';
      h += '</section>';

      h += '<section class="nous-config-section">';
      h += nousLabCardHead('Schedule', '⏱️ Experiment Timing', 'How long Nous observes, experiments, and rests between runs.');
      h += '<div class="nous-config-row">';
      h += `<div><label for="nc-sched-dur" title="How long each experiment runs before Nous collects results. Longer durations give more statistical confidence. Default: 4 hours.">Duration (hours)</label><input type="number" id="nc-sched-dur" value="${sched.experiment_duration_hours || 4}" min="1" max="48" step="0.5" title="Experiment duration in hours (1-48)"></div>`;
      h += `<div><label for="nc-sched-base" title="How many hours of baseline data Nous collects before its first experiment. More data means better comparison accuracy. Default: 4 hours.">Baseline (hours)</label><input type="number" id="nc-sched-base" value="${sched.baseline_hours || 4}" min="1" max="48" step="0.5" title="Baseline collection period in hours (1-48)"></div>`;
      h += `<div><label for="nc-sched-cool" title="Mandatory pause between experiments to let the system stabilize. Prevents confounding back-to-back results. Default: 2 hours.">Cooldown (hours)</label><input type="number" id="nc-sched-cool" value="${sched.cooldown_hours || 2}" min="0.5" max="24" step="0.5" title="Cooldown between experiments in hours (0.5-24)"></div>`;
      h += '</div>';
      h += '</section>';

      // Principle Store
      h += '<section class="nous-config-section">';
      h += nousLabCardHead('Learned', '🧠 Principle Store', 'What Nous has concluded so far, and how much evidence it needs before recommending a change permanently.');
      h += '<div class="nous-config-row-2">';
      h += `<div><label for="nc-prin-thresh" title="Minimum confidence score (0.0-1.0) a principle must reach before Nous recommends applying it permanently. Higher values require more experimental evidence. Default: 0.8.">Recommendation Threshold</label><input type="number" id="nc-prin-thresh" value="${confThresh}" min="0.1" max="1.0" step="0.05" title="Confidence threshold for permanent recommendations (0.1-1.0)"></div>`;
      h += `<div><label for="nc-prin-decay" title="How much a principle's confidence decays per week without reinforcing experimental evidence. Higher values require more frequent validation. Default: 0.05 (5% per week).">Decay Rate / Week</label><input type="number" id="nc-prin-decay" value="${decayRate}" min="0.01" max="0.5" step="0.01" title="Weekly confidence decay rate (0.01-0.5)"></div>`;
      h += '</div>';
      if (principles.length > 0) {
        h += '<div class="nous-lab-principles">';
        const sorted = [...principles].sort((a, b) => (b.confidence || 0) - (a.confidence || 0));
        for (const p of sorted) {
          const conf = Math.round((p.confidence || 0) * 100);
          h += '<div class="nous-config-principle-row">';
          h += `<span class="conf">${conf}%</span>`;
          h += `<span class="text">${escapeHtml(p.text || p.id)}</span>`;
          h += `<button class="del-btn" data-action="deleteNousPrinciple" data-arg0="${escapeHtml(p.id)}" title="Invalidate this principle">✕</button>`;
          h += '</div>';
        }
        h += '</div>';
      } else {
        h += '<div class="nous-lab-empty mt-6">No principles accumulated yet.</div>';
      }
      h += '</section>';

      h += renderNousResetSection();

      // Save / Cancel
      let footer = '';
      footer += '<button class="hv-btn btn-secondary nous-config-btn nous-config-btn-cancel" data-action="closeNousConfig" title="Discard all changes and close this dialog">Cancel</button>';
      footer += '<button class="hv-btn btn-primary nous-config-btn nous-config-btn-save" data-action="saveNousConfig" title="Save all lab-wide guardrails to the server: output mode, the repo allow-list, fast-fail bounds, timing and principle settings. Experiments themselves are defined with + New experiment.">Save Configuration</button>';

      overlay.innerHTML = nousLabShell(h, footer);
    }

    const NOUS_REPO_ALLOWLIST_HEADING = '📂 Repos experiments may target';
    const NOUS_REPO_ALLOWLIST_COPY = 'Experiments can only choose repos from this list; none selected = any repo.';

    function nousRepoSummary(active, total) {
      if (active === 0) return 'No repos selected — experiments may target any repo.';
      return `${active} of ${total} repo${total === 1 ? '' : 's'} allowed.`;
    }

    function configEnvVal(cfg, key, fallback) {
      if (cfg && cfg.campaign && cfg.campaign[key]) return cfg.campaign[key];
      return fallback;
    }

    // .active stays as the JS hook saveNousConfig() reads; .is-on/.is-off carry
    // the visual state so a selected pill is a filled ✓ chip and an excluded
    // one is a dimmed outline (#12520).
    function nousRepoPillClass(isOn) {
      return 'nous-config-pill ' + (isOn ? 'active is-on' : 'is-off');
    }

    function nousRepoPillTitle(repo, isOn) {
      return repo + ' — ' + (isOn ? 'Click to exclude' : 'Click to include');
    }

    function nousRepoPillSetState(pill, isOn) {
      pill.classList.toggle('active', isOn);
      pill.classList.toggle('is-on', isOn);
      pill.classList.toggle('is-off', !isOn);
      pill.setAttribute('aria-pressed', String(isOn));
      pill.title = nousRepoPillTitle(pill.dataset.repo || '', isOn);
    }

    function toggleNousRepo(pill) {
      nousRepoPillSetState(pill, !pill.classList.contains('is-on'));
      const summary = document.getElementById('nc-repo-summary');
      const pills = document.querySelectorAll('.nous-config-pill[data-repo]');
      if (summary) summary.textContent = nousRepoSummary(document.querySelectorAll('.nous-config-pill[data-repo].is-on').length, pills.length);
    }

    // Data section of the Experiment Lab Configuration dialog (RFC #11585):
    // what the lab has collected and the owner-only, audited resets.
    // Counts come from the cached /api/nous/status; every reset re-fetches.
    const NOUS_RESET_SNAPSHOT_BLOAT_FACTOR = 10;
    const NOUS_SNAPSHOT_RETENTION_ISSUE_URL = 'https://github.com/hivecommons/hive/issues/12030';
    const NOUS_RESET_SCOPES = {
      baseline: { label: 'Reset baseline', title: 'Delete every baseline snapshot and return the lab to Observe mode. Learned principles and the experiment ledger are kept. Owner only, audited.',
        confirm: 'Reset the baseline?', message: 'Every baseline snapshot is deleted and the mode returns to Observe so a fresh baseline is collected. The ledger and principles are kept. The previous files are moved to a trash folder on the hive volume, not destroyed.', ok: 'Reset baseline' },
      knowledge: { label: 'Clear knowledge', title: 'Empty the experiment ledger and the learned principles. Baseline snapshots are kept. Owner only, audited.',
        confirm: 'Clear learned knowledge?', message: 'The experiment ledger and every learned principle are emptied. Baseline snapshots are kept. The previous files are moved to a trash folder on the hive volume, not destroyed.', ok: 'Clear knowledge' },
      all: { label: 'Reset everything', title: 'Delete the baseline, the ledger, the principles and any pending gate. Mode, scope and goals are kept. Owner only, audited.', danger: true,
        confirm: 'Reset everything?', message: 'Deletes the baseline, the ledger, every principle and any pending gate, and returns the lab to Observe. Mode, scope and goals are configuration and are kept. Type RESET to confirm.', ok: 'Reset everything' },
    };
    function renderNousResetSection() {
      const s = (_nousCache && _nousCache.status) || {};
      const snapshots = Number(s.snapshotCount != null ? s.snapshotCount : s.snapshots) || 0;
      const target = Number(s.snapshotTarget || s.baseline_target) || 0;
      const principles = Number(s.principleCount != null ? s.principleCount : s.principles) || 0;
      const experiments = Number(s.iterations) || (Array.isArray(_nousCache && _nousCache.ledger) ? _nousCache.ledger.length : 0);
      const canReset = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      let h = '<div class="nous-config-section nous-reset-section" data-nous-reset-section>';
      h += '<h3>🗑️ Data</h3>';
      h += '<div class="nous-reset-counts">';
      h += `<span title="Baseline snapshots on disk (one per evaluation cycle)">Snapshots <strong data-nous-count="snapshots">${formatCount(snapshots)}</strong></span>`;
      h += `<span title="Learned principles in the store">Principles <strong data-nous-count="principles">${formatCount(principles)}</strong></span>`;
      h += `<span title="Rows in the experiment ledger">Experiments <strong data-nous-count="experiments">${formatCount(experiments)}</strong></span>`;
      h += '</div>';
      h += '<div class="nous-reset-actions">';
      for (const scope of Object.keys(NOUS_RESET_SCOPES)) {
        const meta = NOUS_RESET_SCOPES[scope];
        const title = canReset ? meta.title : 'Owner role required';
        h += `<button type="button" class="hv-btn ${meta.danger ? 'btn-danger' : 'btn-secondary'} nous-btn nous-btn-reset" data-action="nousReset" data-arg0="${escapeHtml(scope)}" data-nous-reset="${escapeHtml(scope)}" title="${escapeHtml(title)}"${canReset ? '' : ' disabled'}>${escapeHtml(meta.label)}</button>`;
      }
      h += '<span class="text-muted nous-verdict-note">Resets are owner-only and audited; configuration is kept.</span>';
      h += '</div>';
      if (target > 0 && snapshots > target * NOUS_RESET_SNAPSHOT_BLOAT_FACTOR) {
        h += `<div class="text-muted nous-reset-note" data-nous-reset-hint title="Snapshots keep accumulating after the baseline target is met">baseline target is ${formatCount(target)}; older snapshots are not pruned — see <a href="${NOUS_SNAPSHOT_RETENTION_ISSUE_URL}" target="_blank" rel="noopener" title="hivecommons/hive#12030">retention issue #12030</a></div>`;
      }
      h += '</div>';
      return h;
    }
    async function nousReset(scope) {
      if (!experimentLabEnabled()) return;
      const meta = NOUS_RESET_SCOPES[scope];
      if (!meta) { showToast('Unknown reset scope: ' + scope, 'error'); return; }
      let ok;
      if (scope === 'all') {
        const typed = await hivePrompt({
          title: meta.confirm, label: meta.message, placeholder: 'RESET', confirmLabel: meta.ok,
          validate: (v) => v === 'RESET' ? true : 'Type RESET (all capitals) to confirm.',
        });
        ok = typed === 'RESET';
      } else {
        ok = await hiveConfirm({ title: meta.confirm, message: meta.message, confirmLabel: meta.ok, danger: true });
      }
      if (!ok) return;
      try {
        const r = await fetch('/api/nous/reset', {
          method: 'POST', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ scope, confirm: 'RESET' }),
        });
        const j = await r.json();
        if (r.ok && j.ok) {
          const parts = [];
          if (j.removed_snapshots) parts.push(formatCount(j.removed_snapshots) + ' snapshots');
          if (j.removed_ledger_entries) parts.push(formatCount(j.removed_ledger_entries) + ' ledger entries');
          if (j.removed_principles) parts.push(formatCount(j.removed_principles) + ' principles');
          showToast((parts.length ? 'Removed ' + parts.join(', ') : 'Nothing to remove') + ' · mode: ' + (j.mode || 'observe'), 'success');
        } else if (r.status === 409) {
          showToast(j.error || 'An experiment is active — abort it first, or reset everything', 'error');
        } else {
          showToast(j.error || 'Reset failed', 'error');
        }
        await fetchNousStatus();
        openNousConfig();
      } catch (e) { showToast('Reset failed: ' + e.message, 'error'); }
    }

    async function deleteNousPrinciple(id) {
      if (!experimentLabEnabled()) return;
      if (!(await hiveConfirm('Invalidate this principle? It will be removed from the store.'))) return;
      try {
        const r = await fetch('/api/nous/principles/' + encodeURIComponent(id), { method: 'DELETE' });
        const j = await r.json();
        if (j.ok) {
          showToast('Principle invalidated', 'success');
          openNousConfig();
          fetchNousStatus();
        } else {
          showToast(j.error || 'Failed', 'error');
        }
      } catch (e) { showToast('Failed: ' + e.message, 'error'); }
    }

    // saveNousConfig writes the lab-wide guardrails. The research question
    // is not here: experiments are created and edited on their cards.
    async function saveNousConfig() {
      if (!experimentLabEnabled()) return;
      const errors = [];

      // Output labels (no mode: promotion is manual, #11699)
      const autoHold = document.getElementById('nc-auto-hold')?.checked || false;
      const autoDnm = document.getElementById('nc-auto-dnm')?.checked || false;
      try {
        const r = await fetch('/api/nous/config/output', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ autoHold, autoDoNotMerge: autoDnm }),
        });
        if (!r.ok) errors.push('output: ' + (await r.json()).error);
      } catch (e) { errors.push('output: ' + e.message); }

      // Repo allow-list
      const activeRepoPills = document.querySelectorAll('.nous-config-pill.active');
      const repos = Array.from(activeRepoPills).map(p => p.dataset.repo);
      try {
        const r = await fetch('/api/nous/config/repos', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ repos }),
        });
        if (!r.ok) errors.push('repos: ' + (await r.json()).error);
      } catch (e) { errors.push('repos: ' + e.message); }

      // Fast-fail
      const ffQueue = document.getElementById('nc-ff-queue')?.value;
      const ffMttr = document.getElementById('nc-ff-mttr')?.value;
      const ffBurn = document.getElementById('nc-ff-burn')?.value;
      try {
        const r = await fetch('/api/nous/config/fast-fail', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ queueDepthMax: Number(ffQueue), mttrMaxMinutes: Number(ffMttr), budgetBurnRateMaxPct: Number(ffBurn) }),
        });
        if (!r.ok) errors.push('fast-fail: ' + (await r.json()).error);
      } catch (e) { errors.push('fast-fail: ' + e.message); }

      // Schedule
      const schedDur = document.getElementById('nc-sched-dur')?.value;
      const schedBase = document.getElementById('nc-sched-base')?.value;
      const schedCool = document.getElementById('nc-sched-cool')?.value;
      try {
        const r = await fetch('/api/nous/config/schedule', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ experimentDurationHours: Number(schedDur), baselineHours: Number(schedBase), cooldownHours: Number(schedCool) }),
        });
        if (!r.ok) errors.push('schedule: ' + (await r.json()).error);
      } catch (e) { errors.push('schedule: ' + e.message); }

      // Principle settings
      const prinThresh = document.getElementById('nc-prin-thresh')?.value;
      const prinDecay = document.getElementById('nc-prin-decay')?.value;
      try {
        const r = await fetch('/api/nous/config/principles', {
          method: 'PUT', headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ confidenceThreshold: Number(prinThresh), decayRatePerWeek: Number(prinDecay) }),
        });
        if (!r.ok) errors.push('principles: ' + (await r.json()).error);
      } catch (e) { errors.push('principles: ' + e.message); }

      if (errors.length > 0) {
        showToast('Some settings failed: ' + errors.join('; '), 'error');
      } else {
        showToast('Configuration saved', 'success');
        closeNousConfig();
        fetchNousStatus();
      }
    }

    // Close config on overlay click (but not dialog click)
    document.getElementById('nous-config-overlay')?.addEventListener('click', (e) => {
      if (e.target === e.currentTarget) closeNousConfig();
    });

    // fetchNousStatus() — deferred to _deferredInit


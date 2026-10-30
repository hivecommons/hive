    // Per-repo agent pause (#6203): quiet ONE repo without stopping the hive.
    //
    // The reason prompt is not optional politeness. An unexplained pause is its
    // own support burden — a week later nobody can tell a deliberate freeze
    // from a malfunction — so the reason is collected here, at the only moment
    // the person who knows it is present, and stored with the acting user and
    // the timestamp. Cancelling the prompt cancels the pause.
    // Per-repo write-back mode on the Projects card (writeback.per_repo).
    // Reads the live gate state from /api/status.writeback.repos and writes
    // through the same Features PUT the Settings textarea uses, so the two
    // views share one source and both repaint on the next status render.
    const REPO_WRITEBACK_MODES = ['off', 'issues-only', 'labels-only', 'full'];
    const REPO_WRITEBACK_MODE_TIPS = {
      '': 'default — the per-kind ACMM floor and review opt-in label decide',
      'off': 'off — no unsolicited comments, labels or reviews here',
      'issues-only': 'issues-only — hive may file its own issues; nothing on PRs or issues it did not open',
      'labels-only': 'labels-only — labels but no comments or reviews on foreign items',
      'full': 'full — skip the level and opt-in-label gates for this repo'
    };
    function repoWritebackState(repo, status) {
      const wb = (status && status.writeback) || null;
      if (!wb || !wb.repos) return null;
      const key = String(repo || '').toLowerCase();
      for (const k of Object.keys(wb.repos)) { if (k.toLowerCase() === key) return { status: wb, repo: wb.repos[k] }; }
      return { status: wb, repo: null };
    }
    function repoWritebackControlHtml(repo, status) {
      const st = repoWritebackState(repo, status);
      if (!st) return '';
      const mode = (st.repo && st.repo.mode) || '';
      const level = st.repo ? Number(st.repo.level || 0) : 0;
      const untouched = st.repo ? Number(st.repo.pre_existing_untouched || 0) : 0;
      const tip = 'Write-back: ' + (REPO_WRITEBACK_MODE_TIPS[mode] || mode) + (level ? ' · ACMM L' + level : '') + (untouched ? ' · ' + untouched + ' pre-existing item(s) left untouched (backfill off)' : '') + '. Settings → Features → Write-back containment edits the same map.';
      const chip = `<span class="repo-writeback-chip" data-mode="${esc(mode || 'default')}" title="${esc(tip)}">writes: ${esc(mode || 'default')}</span>`;
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return chip;
      const opts = [''].concat(REPO_WRITEBACK_MODES).map(m => `<option value="${esc(m)}" ${m === mode ? 'selected' : ''}>${esc(m || 'default')}</option>`).join('');
      return chip + `<select class="repo-writeback-select" data-change-action="setRepoWritebackMode" data-arg0="${esc(repo)}" data-arg-types="s,v,t" data-stop="1" aria-label="${esc('Write-back mode for ' + repo)}" title="${esc(tip)}">${opts}</select>`;
    }
    async function setRepoWritebackMode(repo, mode, sel) {
      if (sel && sel.dataset.busy === '1') return;
      const st = repoWritebackState(repo, window._lastStatus || {});
      const perRepo = {};
      if (st && st.status && st.status.repos) {
        for (const k of Object.keys(st.status.repos)) { const m = st.status.repos[k] && st.status.repos[k].mode; if (m) perRepo[k] = m; }
      }
      for (const k of Object.keys(perRepo)) { if (k.toLowerCase() === String(repo).toLowerCase()) delete perRepo[k]; }
      if (mode) perRepo[repo] = mode;
      if (sel) sel.dataset.busy = '1';
      try {
        const resp = await fetch('/api/config/governor/features', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ writebackPerRepo: perRepo }) });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        showToast('Write-back for ' + repo + ': ' + (mode || 'default'), 'success');
        noteStatusMutation(data.minStatusSeq);
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
        if (document.getElementById('writeback-containment')) loadWritebackSuppressed();
      } catch (e) {
        showToast('Write-back mode not saved: ' + e.message, 'error', true);
        if (sel && st) sel.value = (st.repo && st.repo.mode) || '';
      } finally {
        if (sel) delete sel.dataset.busy;
      }
    }

    async function toggleRepoPause(repo, paused, btn) {
      if (btn && btn.dataset.busy === '1') return;
      const resuming = !!paused;
      let reason = '';
      if (!resuming) {
        reason = await hivePrompt({title: 'Pause ' + repo, label: 'Why?\n\nAgents stop writing to this repo and stop being handed work on it. It stays watched, scored and visible here. Recorded with your name and the time.'});
        if (reason === null) return;
      }
      const label = btn ? btn.textContent : '';
      if (btn) { btn.dataset.busy = '1'; btn.textContent = resuming ? 'Resuming…' : 'Pausing…'; }
      try {
        const resp = await fetch('/api/repos/' + (resuming ? 'resume' : 'pause'), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ repo: repo, reason: reason })
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        // persisted:false means the pause IS in force but could not be written
        // to config — it will not survive a restart. Saying "done" without that
        // would be a lie in the direction that matters.
        if (data.warning) showToast(data.warning, 'error', true);
        else showToast(resuming ? ('Resumed ' + repo) : ('Paused ' + repo + ' — agents will not write to it or be handed work on it'), 'success');
        noteStatusMutation(data.minStatusSeq);
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        showToast((resuming ? 'Resume' : 'Pause') + ' failed: ' + (e && e.message ? e.message : 'network error'), 'error');
        if (btn) { btn.dataset.busy = ''; btn.textContent = label || (resuming ? '▶ resume' : '⏸ pause'); }
      }
    }

    async function toggleRepoAutoMerge(repo, enabled, btn) {
      if (btn && btn.dataset.busy === '1') return;
      const nextEnabled = !enabled;
      const isSwitch = btn && btn.classList && btn.classList.contains('config-toggle-switch');
      const label = btn ? btn.textContent : '';
      if (btn) {
        btn.dataset.busy = '1';
        if (isSwitch) {
          btn.classList.toggle('on', nextEnabled);
          btn.setAttribute('aria-checked', nextEnabled ? 'true' : 'false');
        } else {
          btn.textContent = nextEnabled ? 'Enabling…' : 'Disabling…';
        }
      }
      try {
        const resp = await fetch('/api/repos/auto-merge', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ repo: repo, enabled: nextEnabled })
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        showToast((nextEnabled ? 'Enabled' : 'Disabled') + ' auto-merge for ' + repo, 'success');
        // The /api/status refetch usually beats the async rebuild and returns
        // the pre-mutation snapshot; raising the floor first stops it from
        // repainting the switch back to its old state (#11020).
        noteStatusMutation(data.minStatusSeq);
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        showToast('Auto-merge toggle failed: ' + (e && e.message ? e.message : 'network error'), 'error');
        if (btn) {
          btn.dataset.busy = '';
          if (isSwitch) {
            btn.classList.toggle('on', enabled);
            btn.setAttribute('aria-checked', enabled ? 'true' : 'false');
          } else {
            btn.textContent = label || (enabled ? 'auto-merge off' : 'auto-merge on');
          }
        }
      }
    }

    // Repo merge strategy (#10886) and serialized lane state (#10892). The
    // lane panel reads GET /api/repos/merge-lane per hive-serialized card,
    // cached for a minute so the governor's repaint does not refetch it.
    const repoMergeLaneCache = {};
    const REPO_MERGE_LANE_TTL_MS = 60000;
    function ensureRepoMergeLane(full) {
      const key = String(full || '').toLowerCase();
      if (!key) return;
      const cached = repoMergeLaneCache[key];
      if (cached && (cached.loading || Date.now() - cached.at < REPO_MERGE_LANE_TTL_MS)) return;
      repoMergeLaneCache[key] = Object.assign({}, cached || {}, { loading: true, at: Date.now() });
      fetch('/api/repos/merge-lane?repo=' + encodeURIComponent(full))
        .then(r => r.json().then(d => ({ ok: r.ok, data: d })).catch(() => ({ ok: r.ok, data: {} })))
        .then(({ ok, data }) => {
          repoMergeLaneCache[key] = ok ? { at: Date.now(), data: data } : { at: Date.now(), error: data.error || 'lane state unavailable' };
          repaintReposForWidth();
        })
        .catch(e => { repoMergeLaneCache[key] = { at: Date.now(), error: (e && e.message) || 'network error' }; });
    }
    function repoLanePRLink(repoUrl, pr, title, changePath) {
      // Follow the card's forge (GitLab/Gitea change paths) instead of GitHub's /pull/ (#14115).
      return `<a href="${esc(repoUrl + (changePath || '/pull') + '/' + Number(pr))}" target="_blank" rel="noopener"${title ? ` title="${esc(title)}"` : ''}>#${Number(pr)}</a>`;
    }
    function repoLanePanelHTML(full, repoUrl, changePath) {
      const cached = repoMergeLaneCache[String(full || '').toLowerCase()] || {};
      const head = '<div class="repo-lane-title">SERIALIZED LANE</div>';
      if (cached.error) return `<div class="repo-lane-panel">${head}<div class="repo-lane-muted">Lane state unavailable: ${esc(cached.error)}</div></div>`;
      const data = cached.data;
      if (!data) return `<div class="repo-lane-panel">${head}<div class="repo-lane-muted">Loading lane state…</div></div>`;
      if (data.merge_strategy !== 'hive-serialized') return '';
      const errLine = data.lane_error ? `<div class="repo-lane-muted">Lane record unreadable: ${esc(data.lane_error)}</div>` : '';
      const lanes = data.lanes || [];
      if (!lanes.length) return `<div class="repo-lane-panel">${head}${errLine}<div class="repo-lane-muted">No pull request has entered the lane yet.</div></div>`;
      const body = lanes.map(l => {
        const enfTip = l.enforcement === 'server-enforced'
          ? 'GitHub requires branches to be up to date on this branch (read from rulesets)'
          : (l.enforcement === 'hive-checked'
              ? 'GitHub does not require up-to-date branches here; Hive checks it before every merge'
              : (l.rules_unreadable
                  ? 'Hive could not read this branch\'s rules just now' + (l.enforcement_reason ? ' — ' + l.enforcement_reason : '')
                  : 'Hive cannot read whether GitHub requires up-to-date branches here (classic branch protection needs Administration permission)' + (l.enforcement_reason ? ' — ' + l.enforcement_reason : '')));
        const enf = `<span class="repo-lane-enforcement repo-lane-enforcement-${esc(l.enforcement || 'unknown')}" title="${esc(enfTip)}">${esc(l.enforcement_label || 'unknown')}</span>`;
        const front = l.front
          ? `<div class="repo-lane-row"><span class="repo-lane-key">front</span>${repoLanePRLink(repoUrl, l.front.pr, '', changePath)} <span class="repo-lane-stage">${esc(l.front.stage_label || l.front.stage)}</span>${l.front.reason ? `<div class="repo-lane-muted">${esc(l.front.reason)}</div>` : ''}</div>`
          : '<div class="repo-lane-row"><span class="repo-lane-key">front</span><span class="repo-lane-muted">empty</span></div>';
        const waiting = (l.waiting || []).length
          ? `<div class="repo-lane-row"><span class="repo-lane-key">waiting</span>${l.waiting.map(w => repoLanePRLink(repoUrl, w.pr, w.position + '. ' + w.reason, changePath)).join(' → ')}</div>`
          : '';
        const last = l.last_exit
          ? `<div class="repo-lane-row"><span class="repo-lane-key">last left</span>${repoLanePRLink(repoUrl, l.last_exit.pr, '', changePath)}<div class="repo-lane-muted">${esc(l.last_exit.reason)}</div></div>`
          : '';
        const reasons = (l.reasons || []).length
          ? `<details class="repo-lane-reasons"><summary>no-merge reasons (${l.reasons.length})</summary><ul>${l.reasons.map(x => `<li>${x.pr ? repoLanePRLink(repoUrl, x.pr, '', changePath) + ' ' : ''}${esc(x.reason)}</li>`).join('')}</ul></details>`
          : '';
        const reco = l.recommend_up_to_date_rule && l.recommendation ? `<div class="repo-lane-reco">⚠ ${esc(l.recommendation)}</div>` : '';
        return `<div class="repo-lane"><div class="repo-lane-row"><span class="repo-lane-key">branch</span><code>${esc(l.branch)}</code> ${enf}</div>${front}${waiting}${last}${reasons}${reco}</div>`;
      }).join('');
      return `<div class="repo-lane-panel">${head}${errLine}${body}</div>`;
    }
    async function setRepoMergeStrategy(repo, strategy, btn) {
      if (btn && btn.dataset.busy === '1') return;
      const label = btn ? btn.textContent : '';
      if (btn) { btn.dataset.busy = '1'; btn.textContent = 'Saving…'; }
      try {
        const resp = await fetch('/api/repos/merge-strategy', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ repo: repo, merge_strategy: strategy })
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        delete repoMergeLaneCache[String(repo || '').toLowerCase()];
        showToast('Merge strategy for ' + repo + ' is now ' + strategy, 'success');
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        showToast('Merge strategy change failed: ' + (e && e.message ? e.message : 'network error'), 'error');
        if (btn) { btn.dataset.busy = ''; btn.textContent = label; }
      }
    }

    async function startSwarm(repo, btn, force) {
      if (btn && btn.dataset.busy === '1') return;
      if (btn) { btn.dataset.busy = '1'; btn.textContent = 'Calling…'; }
      try {
        const resp = await fetch('/api/swarm', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ repo: repo, force: !!force })
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        showToast((data.display_name || 'Swarm') + ' started for ' + repo, 'success');
        refreshSwarmHistory();
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        showToast('Swarm failed: ' + (e && e.message ? e.message : 'network error'), 'error');
        if (btn) { btn.dataset.busy = ''; btn.textContent = force ? 'force swarm' : '🐝 swarm'; }
      }
    }

    async function endSwarm(btn) {
      if (btn && btn.dataset.busy === '1') return;
      if (btn) { btn.dataset.busy = '1'; btn.textContent = 'Ending…'; }
      try {
        const resp = await fetch('/api/swarm', { method: 'DELETE' });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        showToast('Swarm ended — score ' + ((data.score && ((data.score.issues_closed || 0) + (data.score.prs_merged || 0))) || 0), 'success');
        refreshSwarmHistory();
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        showToast('End swarm failed: ' + (e && e.message ? e.message : 'network error'), 'error');
        if (btn) { btn.dataset.busy = ''; btn.textContent = 'End swarm'; }
      }
    }

    let _swarmHistory = null;
    async function refreshSwarmHistory() {
      try {
        const res = await fetch('/api/swarm/history');
        if (!res.ok) return;
        _swarmHistory = await res.json();
        renderSwarm((window._lastStatus || {}).swarm || {});
      } catch (e) { /* next status tick will retry */ }
    }

    function renderSwarm(swarm) {
      const panel = document.getElementById('swarm-panel');
      if (!panel) return;
      const active = swarm && swarm.active;
      const theme = (active && active.theme) || (swarm && swarm.theme) || (_swarmHistory && _swarmHistory.theme) || {};
      const title = theme.event_name || (swarm && swarm.name) || (_swarmHistory && _swarmHistory.name) || 'Swarm';
      const leaders = (_swarmHistory && _swarmHistory.leaderboard) || [];
      const players = (_swarmHistory && _swarmHistory.top_players) || [];
      const leaderHtml = leaders.length
        ? leaders.slice(0, 5).map(l => `<div class="swarm-leader-row"><span>#${l.rank} ${esc(l.repo)}</span><strong>${l.score || 0}</strong></div>`).join('')
        : '<div class="swarm-meta">No completed swarms yet.</div>';
      const playerHtml = players.length
        ? `<div class="swarm-leaderboard-title mt-3">Players</div>` + players.slice(0, 5).map(p => {
            const trophies = (p.achievements || []).map(a => `<span title="${esc(a.title || a.id)}">🏆</span>`).join('');
            return `<div class="swarm-leader-row"><span>${esc(p.login)} ${trophies}</span><strong>${Number(p.swarms || 0)} swarms · ${Number(p.prs_merged || 0)} PRs</strong></div>`;
          }).join('')
        : '';
      const objectives = (active && active.objectives || []).map(o => `<div class="swarm-meta">${o.completed ? '✅' : '⬜'} ${esc(o.title || o.id)}</div>`).join('');
      const leaderboardLink = `<a href="#swarm-leaderboard" title="Stable swarm leaderboard link">record of triumph</a>`;
      const main = active
        ? `<div class="swarm-title">🐝 ${esc(active.display_name || title)} active on ${esc(active.repo)}</div>
           <div class="swarm-meta"><strong>${esc((active.theme && active.theme.call_to_arms) || theme.call_to_arms || 'All hands on deck!')}</strong></div>
           <div class="swarm-meta">Prioritizing this repo until ${esc(swarmTime(active.end || ''))}${swarm.expires_in ? ' · ' + esc(swarm.expires_in) + ' left' : ''}.</div>
           ${active.prep ? `<div class="swarm-meta">prep: ${Number(active.prep.actionable_issues || 0)} actionable · ${Number(active.prep.unlabeled_issues || 0)} unlabeled · ${Number(active.prep.duplicate_candidates || 0)} possible dups</div>` : ''}
           ${objectives ? `<div class="swarm-leaderboard-title mt-3">Objectives</div>${objectives}` : ''}
           <div class="mt-3"><button class="hv-btn btn-secondary btn-sm" data-action="endSwarm" data-arg-types="t" data-stop="1">End swarm</button></div>`
        : `<div class="swarm-title">🐝 ${esc(title)}</div><div class="swarm-meta">Start a 24h repo swarm from any repository card. Only one swarm can run at a time.</div>`;
      setIfChanged(panel, `<div>${main}</div><div id="swarm-leaderboard" class="swarm-leaderboard"><div class="swarm-leaderboard-title">${esc(theme.leaderboard_title || 'Swarm leaderboard')} · ${leaderboardLink}</div>${leaderHtml}${playerHtml}</div>`);
      if (!_swarmHistory) refreshSwarmHistory();
    }

    function swarmTime(value) {
      if (!value) return '';
      const d = new Date(value);
      if (Number.isNaN(d.getTime())) return String(value);
      return formatDashboardDateTime(d);
    }

    const REPO_ISSUE_BAND_DEFAULTS = {
      waitingLabels: ['needs-human', 'stage/045-needs-decision', 'stage/085-needs-decision', 'stage/017-possible-existing', 'needs-direction', 'needs-decision', 'needs-spec', 'blocked'],
      doneLabels: ['hive/already-done', 'hive/covered-by-pr', 'hive/likely-done'],
      staleDays: 14
    };
    const MS_PER_DAY = 24 * 60 * 60 * 1000;
    window._repoIssueBandConfig = Object.assign({}, REPO_ISSUE_BAND_DEFAULTS);
    const REPO_LEGEND_COLLAPSED_KEY_PREFIX = 'hive-repos-legend-collapsed:';
    const OVERVIEW_ISSUE_BAND_ORDER = ['ready', 'in-progress', 'agent-filed', 'waiting', 'done'];
    const OVERVIEW_CHART_FULL = 100;
    // 300-unit viewBox leaves ~46 units either side of the label ring so edge labels
    // stay inside the SVG box instead of spilling into the legend or past the card edge (#9036).
    const OVERVIEW_CHART_VIEWBOX = 300;
    const OVERVIEW_CHART_CENTER = OVERVIEW_CHART_VIEWBOX / 2;
    const OVERVIEW_CHART_RADIUS = 78;
    const OVERVIEW_CHART_STROKE = 34;
    const OVERVIEW_CHART_LABEL_RADIUS = 104;
    // Labels within this many degrees of straight up/down stay centred; others grow away from the ring.
    const OVERVIEW_CHART_LABEL_POLE_DEGREES = 12;
    const OVERVIEW_CHART_START_ANGLE = -90;
    const OVERVIEW_CHART_DEGREES = 360;
    const OVERVIEW_CHART_PERCENT_SCALE = 100;
    const OVERVIEW_CHART_LABEL_MIN_PERCENT = 7;
    const OVERVIEW_CHART_DECIMAL_PLACES = 1;
    const OVERVIEW_CHART_TOTAL_VALUE_Y_OFFSET = -8;
    const OVERVIEW_CHART_TOTAL_CAPTION_Y_OFFSET = 12;

    function normalizeIssueBandConfig(raw) {
      const cfg = raw || {};
      const uniq = (arr, fallback) => {
        const out = [];
        (Array.isArray(arr) && arr.length ? arr : fallback).forEach(v => {
          const s = String(v || '').trim();
          if (s && !out.some(x => x.toLowerCase() === s.toLowerCase())) out.push(s);
        });
        return out;
      };
      const staleDays = Number(cfg.stale_days || cfg.staleDays || REPO_ISSUE_BAND_DEFAULTS.staleDays);
      return {
        waitingLabels: uniq(cfg.waiting_labels || cfg.waitingLabels, REPO_ISSUE_BAND_DEFAULTS.waitingLabels),
        doneLabels: uniq(cfg.done_labels || cfg.doneLabels, REPO_ISSUE_BAND_DEFAULTS.doneLabels),
        staleDays: Number.isFinite(staleDays) && staleDays > 0 ? staleDays : REPO_ISSUE_BAND_DEFAULTS.staleDays
      };
    }

    function repoIssueBandConfig() {
      return normalizeIssueBandConfig(window._repoIssueBandConfig || {});
    }

    function repoLegendCollapsedKey() {
      return REPO_LEGEND_COLLAPSED_KEY_PREFIX + (((window._lastStatus || {}).hiveId) || 'default');
    }

    function isRepoLegendCollapsed() {
      try { return localStorage.getItem(repoLegendCollapsedKey()) === '1'; } catch { return false; }
    }

    function toggleRepoLegend() {
      const next = !isRepoLegendCollapsed();
      try { localStorage.setItem(repoLegendCollapsedKey(), next ? '1' : '0'); } catch {}
      renderRepoLegend();
    }

    const REPO_PILL_FILTER_KEY_PREFIX = 'hive-repos-pill-filter:';
    function repoPillFilterKey() {
      return REPO_PILL_FILTER_KEY_PREFIX + (((window._lastStatus || {}).hiveId) || 'default');
    }
    function normalizeRepoPillFilter(value) {
      const src = value && typeof value === 'object' ? value : {};
      const mode = src.mode === 'all' ? 'all' : 'any';
      const kinds = Array.isArray(src.kinds) ? src.kinds.map(k => String(k).trim()).filter(Boolean) : [];
      const repo = String(src.repo || '').trim();
      return { mode, kinds: Array.from(new Set(kinds)).sort(), repo };
    }
    function repoPillFilterState() {
      try { return normalizeRepoPillFilter(JSON.parse(localStorage.getItem(repoPillFilterKey()) || '{}')); } catch { return normalizeRepoPillFilter(null); }
    }
    function writeRepoPillFilterState(state) {
      state = normalizeRepoPillFilter(state);
      try {
        if (state.kinds.length || state.repo) localStorage.setItem(repoPillFilterKey(), JSON.stringify(state));
        else localStorage.removeItem(repoPillFilterKey());
      } catch {}
      return state;
    }
    function repoPillFilterActive(state) {
      state = normalizeRepoPillFilter(state || repoPillFilterState());
      return state.kinds.length > 0 || !!state.repo;
    }
    function repoPillFilterMatches(rowKinds, state) {
      state = normalizeRepoPillFilter(state || repoPillFilterState());
      if (!state.kinds.length) return true;
      const have = new Set((Array.isArray(rowKinds) ? rowKinds : String(rowKinds || '').split(/\s+/)).filter(Boolean));
      return state.mode === 'all' ? state.kinds.every(k => have.has(k)) : state.kinds.some(k => have.has(k));
    }
    function repoPillFilterAttrs(kind) {
      const active = repoPillFilterState().kinds.includes(kind);
      return ` data-action="toggleRepoPillFilter" data-arg0="${esc(kind)}" data-stop="1" data-prevent="1" aria-pressed="${active ? 'true' : 'false'}"`;
    }
    function repoPillKindAttr(kinds) {
      const list = Array.from(new Set((kinds || []).map(k => String(k || '').trim()).filter(Boolean))).sort();
      return ` data-pill-kinds="${esc(list.join(' '))}"`;
    }
    function repoPillFilterHiddenClass(kinds, state) {
      return repoPillFilterMatches(kinds, state) ? '' : ' repo-pill-row-filtered-hidden';
    }
    function repoPillFilterRepoMatches(repo, state) {
      state = normalizeRepoPillFilter(state || repoPillFilterState());
      if (!state.repo) return true;
      const full = String(repo || '').toLowerCase();
      const want = state.repo.toLowerCase();
      return full === want || full.endsWith('/' + want);
    }
    function toggleRepoPillFilter(kind) {
      const state = repoPillFilterState();
      const set = new Set(state.kinds);
      if (set.has(kind)) set.delete(kind); else set.add(kind);
      writeRepoPillFilterState({ mode: state.mode, kinds: Array.from(set), repo: state.repo });
      repaintReposForWidth();
    }
    function setRepoPillFilterMode(mode) {
      const state = repoPillFilterState();
      writeRepoPillFilterState({ mode: mode === 'all' ? 'all' : 'any', kinds: state.kinds, repo: state.repo });
      repaintReposForWidth();
    }
    function clearRepoPillFilter() {
      writeRepoPillFilterState({ mode: repoPillFilterState().mode, kinds: [], repo: '' });
      repaintReposForWidth();
    }
    function repoPillFilterRowCounts(rows, state) {
      const total = (rows || []).length;
      const shown = (rows || []).filter(row => repoPillFilterMatches(row.kinds, state)).length;
      return { shown, total };
    }
    function repoPillFilterClearChip(counts) {
      const state = repoPillFilterState();
      const repoText = state.repo ? `${state.repo} · ` : '';
      return `<span class="repo-filtered-chip">🔎 filtered · ${esc(repoText)}${counts.shown} of ${counts.total}<button type="button" data-action="clearRepoPillFilter" data-stop="1" data-prevent="1" aria-label="Clear repository pill filter" title="Clear filter">✕</button></span>`;
    }
    // One filter kind per chip word, shared by waiting-band and parked rows (#11505).
    function repoIssueChipKind(chip) {
      return 'issue:chip-' + String(chip || '').toLowerCase();
    }
    function repoNonActionableIssueKinds(spec, row) {
      const kinds = ['issue:non-actionable', 'issue:' + spec.key];
      if (spec.chip) kinds.push(repoIssueChipKind(spec.chip));
      if (issueCoveringPR(row)) kinds.push('issue:linked-pr');
      if (yourTurnOf(row)) kinds.push('your-turn');
      return kinds;
    }
    function repoIssueFilterKinds(issue, info, held) {
      const issueLabels = ((issue && issue.labels) || []).map(l => String(l).toLowerCase());
      const labels = new Set(issueLabels);
      const kinds = ['issue:' + ((info && info.band) || 'ready')];
      if (held) kinds.push('issue:held');
      if (info && info.chip) kinds.push(repoIssueChipKind(info.chip));
      if (labels.has('needs-human')) kinds.push('needs-human', 'issue:needs-human');
      if (labels.has('blocked')) kinds.push('issue:blocked');
      if (labels.has('stage/045-needs-decision') || labels.has('stage/085-needs-decision') || labels.has('needs-decision') || labels.has('2-discussing')) kinds.push('issue:needs-decision');
      if (labels.has('epic')) kinds.push('issue:epic');
      if (issueLabels.some(l => l === 'claimed' || l === 'hive/in-progress' || l === 'stage/020-claimed' || l === 'stage/030-pr-in-progress' || l.startsWith('hive/claimed-by-')) || ((issue && issue.assignees) || []).length) kinds.push('issue:claimed');
      if (info && info.role) kinds.push('issue:role');
      if (issueIsStale(issue)) kinds.push('issue:stale');
      if (Array.isArray(issue && issue.linked_prs) && issue.linked_prs.length) kinds.push('issue:linked-pr');
      if (yourTurnOf(issue)) kinds.push('your-turn');
      return kinds;
    }
    function repoPRFilterKinds(pr, held, info) {
      const labels = prLabelSet(pr || {});
      const kinds = [];
      const mergeState = prMergeState(pr || {});
      if (yourTurnOf(pr)) kinds.push('your-turn');
      if (mergeState === 'eligible') kinds.push('pr:eligible');
      if (mergeState === 'outstanding') kinds.push('pr:outstanding');
      if (labels.has('needs-human')) kinds.push('needs-human', 'pr:needs-human');
      if (prSentinelFlagged(pr || {})) kinds.push('pr:sentinel');
      if (held || holdLabels((pr && pr.labels) || []).length) kinds.push('pr:held');
      if (labels.has('stage/085-needs-decision') || labels.has('needs-decision') || labels.has('2-discussing')) kinds.push('pr:needs-decision');
      if ((pr && pr.review_class) === 'fix') kinds.push('pr:fix');
      const review = prGitHubReview(pr || {});
      if (review) {
        if (review.glyph === '👍') kinds.push('pr:approved');
        else if (review.glyph === '👎') kinds.push('pr:changes-requested');
        else if (review.glyph === '👀') kinds.push('pr:approval-required');
      }
      if (prRequestedReviews(pr || {}).length) kinds.push('pr:review-requested');
      if (prConversation(pr || {})) kinds.push('pr:comments');
      if (prLinkedIssues(pr || {}).length) kinds.push('pr:linked-issue');
      if (String((pr && pr.ci_status) || '').toLowerCase() === 'failure' || ((pr && pr.failing_checks) || []).length) kinds.push('pr:ci');
      if ((pr || {}).mergeable === 'no') kinds.push('pr:conflict');
      if (prQueued(pr || {})) kinds.push('pr:queued');
      if (info && info.role) kinds.push('pr:role');
      if (prIsStale(pr)) kinds.push('pr:stale');
      if ((pr || {}).review_url) kinds.push('pr:hive-review');
      return Array.from(new Set(kinds));
    }

    function renderRepoLegend() {
      const el = document.getElementById('repos-legend');
      if (!el) return;
      const collapsed = isRepoLegendCollapsed();
      el.className = 'repo-legend repo-legend-v2' + (collapsed ? ' collapsed' : '');
      const cfg = repoIssueBandConfig();
      const issueCaptions = {
        ready: 'nobody on it',
        'in-progress': 'assigned to a human or agent',
        'agent-filed': 'agent-filed, not yet acknowledged',
        waiting: 'parked by a label — known labels show their own chip below',
        done: 'agent thinks it landed'
      };
      const issueBandLegendEntries = OVERVIEW_ISSUE_BAND_ORDER.map(band => {
        const spec = issueBandSpec(band);
        return {
          cls: 'repo-issue-pill' + (band === 'ready' ? '' : ' ' + band),
          kind: 'issue:' + band,
          glyph: band === 'in-progress' ? '👤' : (band === 'done' ? '✓' : ''),
          label: spec.short,
          caption: issueCaptions[band] || spec.label,
          tip: issueBandTip(band)
        };
      });
      const legendGroups = [
        {
          title: 'Issues — left chip is the band',
          entries: issueBandLegendEntries.concat([
            { cls: 'repo-issue-pill held', kind: 'issue:filtered', label: 'FILTERED', caption: 'skipped by the issue filter or an exempt label', tip: 'Excluded by the project issue filter or an exempt label' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('human'), label: 'HUMAN', caption: 'needs-human — parked by label', tip: 'Labelled needs-human' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('direction'), label: 'DIRECTION', caption: 'needs-direction — parked by Hive', tip: 'Labelled needs-direction: Hive parked this issue' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('decision'), label: 'DECISION', caption: 'needs-decision — parked by Hive', tip: 'Labelled needs-decision: Hive parked this issue' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('spec'), label: 'SPEC', caption: 'needs-spec — parked until it has a spec', tip: 'Labelled needs-spec: Hive parked this issue until it has a spec' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('reporter'), label: 'REPORTER', caption: 'needs-reporter-confirmation — waiting on the reporter', tip: 'Waiting on the reporter to confirm' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('blocked'), label: 'BLOCKED', caption: 'blocked — labelled blocked; contributors skip it', tip: 'Labelled blocked. Hover a card\'s pill for its open "blocked by" links, if any; the label itself records no reason.' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('discussing'), label: 'DISCUSSING', caption: '2-discussing — still under discussion', tip: 'Labelled 2-discussing' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('epic'), label: 'EPIC', caption: 'Epic', tip: 'Labelled Epic' },
            { cls: 'repo-issue-pill waiting', kind: repoIssueChipKind('untriaged'), label: 'UNTRIAGED', caption: 'needs-triage — no maintainer triage yet', tip: 'Labelled needs-triage' },
            { cls: 'repo-issue-pill held', kind: 'issue:hive_advisory', label: 'ADVISORY', caption: 'Hive advisory report, not agent work', tip: 'Hive advisory report — counted, but not agent work' },
            { cls: 'repo-issue-pill agent-filed', kind: 'issue:role', label: 'STRATEGIST', labelCls: 'pill-role', caption: 'role of the agent that filed it', tip: 'Filed by an agent; the badge names the agent role and the tooltip says whether a human has acknowledged it' }
          ])
        },
        {
          title: 'Pull requests — merge state',
          entries: [
            { cls: 'repo-pr-pill mergeable', kind: 'pr:eligible', glyph: '✓', glyphCls: 'pill-merge-icon', label: 'ELIGIBLE', caption: 'sweep would merge now', tip: 'Merge-eligible on the sweep verdict' },
            { cls: 'repo-pr-pill merge-outstanding', kind: 'pr:outstanding', glyph: '◐', glyphCls: 'pill-merge-icon', label: 'OUTSTANDING', caption: 'mergeable, sweep still wants something', tip: 'GitHub says mergeable, but the sweep still wants something' },
            { cls: 'repo-pr-pill sentinel-flagged', kind: 'pr:sentinel', label: 'SENTINEL', caption: 'sentinel flagged it (possible security override, privilege escalation or codebase damage) — Hive will not merge it while the sentinel label is on; trusted-author PRs usually get a comment, not red', tip: 'The sentinel flagged this PR for a possible security override, privilege escalation or codebase damage. Hive will not merge it while the sentinel label is on. Red means the PR carries the label, so trusted-author PRs (which normally get a sentinel comment instead of the label) are usually not red.' },
            { cls: 'repo-pr-pill needs-human', kind: 'needs-human', glyph: '⚠', label: 'HUMAN', caption: 'labelled needs-human — fix attempts exhausted', tip: 'Labelled needs-human: automated fix attempts exhausted' },
            { cls: 'repo-pr-pill', kind: 'pr:held', glyph: '⏸', label: 'HELD', caption: 'held — automation skips it', tip: 'Held: automation skips it until the hold is released' },
            { cls: 'repo-pr-pill', kind: 'pr:ci', label: '✗ CI', caption: 'failing checks (hover lists them)', tip: 'Failing CI; tooltip lists failing checks' },
            { cls: 'repo-pr-pill', kind: 'pr:conflict', glyph: '⑂', label: 'CONFLICT', caption: 'merge conflict', tip: 'Conflicts or not mergeable on GitHub' },
            { cls: 'repo-pr-pill', kind: 'pr:fix', label: 'FIX', labelCls: 'pill-class fix', caption: 'review class — FIX prioritized over FEAT/DOCS/…', tip: 'Review class badge: FIX is red; FEAT/TESTS/DOCS/REFACTOR show why the PR is prioritized' }
          ]
        },
        {
          title: 'Review, links & actions',
          entries: [
            { cls: 'repo-pr-pill', kind: 'pr:approved', glyph: '👍', label: 'APPROVED', caption: 'approved on GitHub', tip: 'Approved on GitHub; tooltip counts the approvals' },
            { cls: 'repo-pr-pill', kind: 'pr:changes-requested', glyph: '👎', label: 'CHANGES', caption: 'changes requested', tip: 'Changes requested on GitHub; tooltip names the reviewers' },
            { cls: 'repo-pr-pill', kind: 'pr:approval-required', glyph: '👀', label: 'PENDING', caption: 'approval required, not yet given', tip: 'Approving review required by branch protection and not yet given' },
            { cls: 'repo-pr-pill', kind: 'pr:review-requested', glyph: '👥', label: 'REVIEWERS', caption: 'review requested from someone', tip: 'Review requested; tooltip lists the reviewers and teams' },
            { cls: 'repo-pr-pill pill-reviewed', kind: 'pr:hive-review', glyph: '💬', label: 'HIVE REVIEW', caption: 'hive posted a review', tip: 'Hive review posted; opens the recorded review' },
            { cls: 'repo-pr-pill', kind: 'pr:comments', glyph: '🗨', label: 'COMMENTS', caption: 'comment / thread count', tip: 'Comments and review threads on GitHub' },
            { cls: 'repo-issue-plan', kind: 'issue:linked-pr', glyph: '🔗', label: 'LINKED PR', caption: 'on an issue: verified linked PR', tip: 'Pull request hive verified covers the issue — closes via PR #N, or covered by PR #N (partial) for a Refs-only PR' },
            { cls: 'repo-pr-pill', kind: 'pr:linked-issue', glyph: '🔗', label: 'CLOSES', caption: 'on a PR: issue it closes on merge', tip: 'Issue this PR closes on merge (GitHub closing reference); opens the issue' },
            { cls: 'repo-issue-plan plan-review', kind: 'issue:plan', glyph: '📋 📐 ●', label: 'PLAN', caption: 'plan · design · plan awaiting review', tip: 'Plan this issue (clipboard icon); Design this issue before planning (ruler icon); Plan awaiting human review' },
            { cls: 'repo-pr-pill mergeable', kind: 'pr:queued', glyph: '🔀', label: 'QUEUED', caption: 'queued for auto-merge', tip: 'Queued for Hive auto-merge' },
            { cls: 'yt-legend-marker', kind: 'your-turn', glyph: YOUR_TURN_GLYPH, label: 'YOUR TURN', caption: 'your turn — hover or focus a row’s marker for the verb and reason, click it for options', tip: 'Your turn: the item waits on you. This entry filters to those rows; the marker on a row opens that item’s menu.' }
          ]
        }
      ];
      const renderLegendPill = item => {
        const glyph = item.glyph ? `<span${item.glyphCls ? ` class="${esc(item.glyphCls)}"` : ''}>${esc(item.glyph)}</span>` : '';
        const label = item.label ? `<span${item.labelCls ? ` class="${esc(item.labelCls)}"` : ''}>${esc(item.label)}</span>` : '';
        return `<button type="button" class="${esc(item.cls)} repo-pill-filter-toggle" title="${esc(item.tip)}"${repoPillFilterAttrs(item.kind)}>${glyph}${label}</button><span>${esc(item.caption)}</span>`;
      };
      const colorKey = [
        { label: 'waiting / gated', colorCls: 'needs-you' },
        { label: 'in progress / outstanding', colorCls: 'in-progress' },
        { label: 'ready / done', colorCls: 'ready-done' },
        { label: 'agent-filed', colorCls: 'agent-filed' },
        { label: 'held (agents skip it)', colorCls: 'held', dashed: true, tip: 'Held issue: grey dashed border; agents will not act until the hold label is removed' },
        { label: `🕒 dimmed = no activity > ${cfg.staleDays}d`, colorCls: 'stale', dimmed: true, tip: `No activity for more than ${cfg.staleDays} days` }
      ].map(item => `<span class="repo-legend-color${item.dimmed ? ' dimmed' : ''}"${item.tip ? ` title="${esc(item.tip)}"` : ''}><i class="repo-legend-swatch ${esc(item.colorCls)}${item.dashed ? ' dashed' : ''}"></i>${esc(item.label)}</span>`).join('');
      const groups = legendGroups.map(group => `
          <div class="repo-legend-group">
            <div class="repo-legend-group-title">${esc(group.title)}</div>
            <div class="repo-legend-items">${group.entries.map(renderLegendPill).join('')}</div>
          </div>`).join('');
      setIfChanged(el, `
        <div class="repo-legend-head">
          <div class="repo-legend-title">Pill legend</div>
          <div class="repo-pill-filter-toolbar">
            <span class="repo-pill-filter-mode" aria-label="Repository pill filter match mode">match <button type="button" data-action="setRepoPillFilterMode" data-arg0="any" data-stop="1" data-prevent="1" aria-pressed="${repoPillFilterState().mode !== 'all' ? 'true' : 'false'}">any</button><button type="button" data-action="setRepoPillFilterMode" data-arg0="all" data-stop="1" data-prevent="1" aria-pressed="${repoPillFilterState().mode === 'all' ? 'true' : 'false'}">all</button></span>
            <span class="repo-pill-filter-summary">${repoPillFilterActive() ? esc(repoPillFilterState().kinds.length + ' selected') : 'click a pill to filter'}</span>
            <button class="repo-legend-toggle" data-action="toggleRepoLegend" data-stop="1" type="button" aria-expanded="${collapsed ? 'false' : 'true'}" title="${collapsed ? 'Show' : 'Hide'} the repository pill legend (remembered in this browser)">${collapsed ? 'show' : 'hide'}</button>
          </div>
        </div>
        <div class="repo-legend-body">
          <div class="repo-legend-colors">${colorKey}</div>${groups}
        </div>`);
    }

    function repoBreakdownTip(kind, key) {
      const tips = {
        issues: {
          actionable: 'Issues in the automated work lane; held/exempt/filter buckets are excluded from this count',
          hold: 'Open issues carrying a hold label; agents will not act until the hold is removed',
          hive_advisory: 'Issues that belong to the Hive advisory/backlog lane, not the ordinary work lane',
          dependency_dashboard: 'Dependency-dashboard issues grouped separately from ordinary actionable work',
          filtered: 'Open issues skipped by the configured project.issue_filter label policy',
          reporter_triage: 'Open issues from reporters this hive does not trust (not an owner, member or collaborator) that carry needs-triage while waiting for a maintainer to add the configured triage label (#9665)',
          other: 'Open issues that did not land in an actionable, held, advisory, dependency dashboard or filtered bucket'
        },
        prs: {
          actionable: 'Open PRs in the automated review/merge lane',
          hold: 'Open PRs carrying a hold label; agents will not act until the hold is removed',
          draft: 'Draft PRs; visible but outside the automated merge lane',
          filtered: 'Open PRs skipped by repository or governor policy',
          other: 'Open PRs that did not land in an actionable, held, draft or filtered bucket'
        }
      };
      return ((tips[kind] || {})[key]) || '';
    }

    function formatRepoWorkBreakdown(breakdown, total, kind, staleCount) {
      if (!breakdown || total <= 0) return '';
      const escAttr = (s) => String(s || '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;', "'":'&#39;'}[c]));
      const breakdownTip = (k, key) => (typeof repoBreakdownTip === 'function' ? repoBreakdownTip(k, key) : '');
      const categories = kind === 'issues'
        ? [
            ['actionable', 'actionable'],
            ['hold', 'hold'],
            ['hive_advisory', 'advisory'],
            ['dependency_dashboard', 'dependency dashboard'],
            ['filtered', 'filtered'],
            ['reporter_triage', 'needs triage'],
            ['other', 'other']
          ]
        : [
            ['actionable', 'actionable'],
            ['hold', 'hold'],
            ['draft', 'draft'],
            ['filtered', 'filtered'],
            ['other', 'other']
          ];
      const parts = categories
        .filter(([key]) => key === 'actionable' || Number(breakdown[key] || 0) > 0)
        .map(([key, label]) => `<span${key === 'actionable' ? ' class="repo-breakdown-actionable"' : ''} title="${escAttr(breakdownTip(kind, key))}">${Number(breakdown[key] || 0)} ${label}</span>`);
      if (kind === 'issues' && Number(staleCount || 0) > 0) {
        const staleDays = (typeof repoIssueBandConfig === 'function' ? repoIssueBandConfig().staleDays : REPO_ISSUE_BAND_DEFAULTS.staleDays);
        parts.splice(1, 0, `<span title="${escAttr('Actionable issues with no activity for more than ' + staleDays + ' days')}">${Number(staleCount || 0)} no activity &gt; ${staleDays}d</span>`);
      }
      return `<div class="repo-breakdown">${parts.join(' · ')}</div>`;
    }

    // Which of an item's labels put it on hold (hivecommons/hive#7896/#8851).
    // Generic hold spellings are substring matches; the canonical hive hold is
    // exact so hive/<id> can remain a provenance label instead of a hold.
    const HOLD_LABEL_SPELLINGS = ['hold', 'on-hold', 'hold/review'];
    function canonicalHiveHoldLabel() {
      const hiveId = String(((window._lastStatus || {}).hiveId) || '').trim();
      return hiveId ? ('hive-pause/' + hiveId) : 'hold';
    }
    function holdLabels(labels) {
      const canonical = canonicalHiveHoldLabel().toLowerCase();
      return (labels || []).map(l => String(l)).filter(l => {
        const lower = l.toLowerCase();
        return lower === canonical || HOLD_LABEL_SPELLINGS.some(s => lower.includes(s));
      });
    }

    // The tooltip text for a held pill: WHY the item is parked, from what
    // the snapshot carries (labels and, for a PR, whether a hive agent
    // opened it). Two holds an operator meets look alike on the card and
    // are not: `hold` on an agent's own PR is usually the ACMM level gate
    // ("held for human review"), which the hive lifts itself once policy no
    // longer requires the checkpoint; `needs-human` beside the hold is the
    // fix-loop fallback, unless reporter_trust_reason carries the original
    // reporter-trust notice evidence. Neither reason changes the label-driven
    // hold gate; only a person clears a reporter-trust hold. Returned
    // without wrapping so the state chip can show it as-is and the pill can
    // append it in parentheses after the title.
    // blocked (a required check or review still pending) and behind (the base
    // moved) are states the merge sweep resolves itself, so queueing stays
    // available; only a verdict queueing cannot fix disables the button.
    function queueAutoMergeGate(p) {
      const state = String((p && p.mergeable_state) || '');
      if (state === 'blocked') return { disabled: false, tip: 'Queue auto-merge — checks still running or review pending: queues now, merges when they pass' };
      if (state === 'behind') return { disabled: false, tip: 'Queue auto-merge — behind the base branch: Hive brings it up to date before merging' };
      if (p && p.mergeable === 'no') return { disabled: true, tip: 'Not mergeable on GitHub' + (state ? ' (' + state + ')' : '') + ' — resolve that before queueing for auto-merge' };
      return { disabled: false, tip: 'Queue auto-merge when checks pass' };
    }

    function heldReason(item) {
      const labels = (item && item.labels ? item.labels : []).map(l => String(l));
      const held = holdLabels(labels);
      const needsHuman = labels.includes('needs-human');
      const parts = [];
      parts.push('On hold' + (held.length ? ' — label ' + held.map(l => '`' + l + '`').join(', ') : '') + ': agents will not act on this until the hold label is removed');
      if (item && item.reporter_trust_reason) {
        parts.push(item.reporter_trust_reason);
        return parts.join('; ');
      }
      if (item && item.hive_attributed) parts.push('opened by a hive agent — a generic `hold` here is usually the ACMM level gate; the dashboard hive-pause hold is only removed by an operator');
      if (needsHuman) parts.push('labelled needs-human: automated fix attempts exhausted');
      return parts.join('; ');
    }

    const repoHoldPermissionCache = {};
    function repoParts(full) {
      const parts = String(full || '').split('/');
      if (parts.length < 2) return { owner: '', repo: String(full || '') };
      return { owner: parts[0], repo: parts.slice(1).join('/') };
    }
    function repoHoldPermissionKnown(full) {
      if (dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return true;
      const key = String(full || '').toLowerCase();
      return repoHoldPermissionCache[key] === true;
    }
    function ensureRepoHoldPermission(full) {
      if (!window._hiveUser || dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return;
      const key = String(full || '').toLowerCase();
      if (!key || repoHoldPermissionCache[key] !== undefined) return;
      repoHoldPermissionCache[key] = 'loading';
      const p = repoParts(full);
      fetch('/api/repos/' + encodeURIComponent(p.owner) + '/' + encodeURIComponent(p.repo) + '/hold-permission')
        .then(r => r.json().then(d => ({ ok: r.ok, data: d })).catch(() => ({ ok: r.ok, data: {} })))
        .then(({ ok, data }) => { repoHoldPermissionCache[key] = !!(ok && data.allowed); repaintReposForWidth(); })
        .catch(() => { repoHoldPermissionCache[key] = false; });
    }
    function holdToggleChip(repoFull, item, type, held, interactive, tip) {
      const number = Number(item && item.number);
      const text = held ? '▶ Release' : '⏸ Hold';
      const label = held ? 'Release hold' : 'Hold';
      const title = held
        ? ((tip ? tip + '. ' : '') + 'Release hold — removes hold label(s) so agents may act on this item again')
        : ('Hold — applies label `' + canonicalHiveHoldLabel() + '` so agents will not act on this until the hold label is removed');
      const cls = (type === 'pr' ? 'repo-pr-pill' : 'repo-issue-pill') + (held ? ' held' : '') + ' pill-held-badge pill-icon';
      if (!interactive || !number) {
        if (!held) return '';
        return `<span class="${cls} repo-hold-static" title="${esc(title)}" aria-label="${esc(title)}">On hold</span>`;
      }
      return `<button class="${cls} repo-hold-toggle" data-action="toggleRepoItemHold" data-arg0="${esc(repoFull)}" data-arg1="${number}" data-arg2="${type}" data-arg3="${held ? 'false' : 'true'}" data-arg-types="s,j,s,j,t" data-stop="1" data-prevent="1" aria-pressed="${held ? 'true' : 'false'}" title="${esc(title)}" aria-label="${esc(label + ' #' + number)}">${text}</button>`;
    }
    function updateRepoBreakdownForHold(repo, type, toHeld) {
      const wb = repo && repo.workBreakdown && repo.workBreakdown[type === 'pr' ? 'prs' : 'issues'];
      if (!wb) return;
      wb.hold = Math.max(0, Number(wb.hold || 0) + (toHeld ? 1 : -1));
      wb.actionable = Math.max(0, Number(wb.actionable || 0) + (toHeld ? -1 : 1));
    }
    function moveRepoItemHold(repoFull, number, type, toHeld, label) {
      const status = window._lastStatus || {};
      const repos = status.repos || [];
      const repo = repos.find(r => (r.full || r.name || '').toLowerCase() === String(repoFull || '').toLowerCase());
      if (!repo) return null;
      const fromKey = type === 'pr' ? (toHeld ? 'openPrs' : 'heldPrs') : (toHeld ? 'actionableIssues' : 'heldIssues');
      const toKey = type === 'pr' ? (toHeld ? 'heldPrs' : 'openPrs') : (toHeld ? 'heldIssues' : 'actionableIssues');
      const from = repo[fromKey] || [];
      const idx = from.findIndex(x => Number(x && x.number) === Number(number));
      if (idx < 0) return null;
      const item = from.splice(idx, 1)[0];
      item.labels = (item.labels || []).map(l => String(l));
      if (toHeld) {
        if (!item.labels.some(l => l.toLowerCase() === String(label || canonicalHiveHoldLabel()).toLowerCase())) item.labels.push(label || canonicalHiveHoldLabel());
      } else {
        const removed = holdLabels(item.labels).map(l => l.toLowerCase());
        item.labels = item.labels.filter(l => !removed.includes(String(l).toLowerCase()));
      }
      repo[toKey] = repo[toKey] || [];
      repo[toKey].push(item);
      updateRepoBreakdownForHold(repo, type, toHeld);
      repaintReposForWidth();
      return { repo, item, fromKey, toKey };
    }
    async function toggleRepoItemHold(repoFull, number, type, held, btn) {
      if (btn && btn.dataset.busy === '1') return;
      const p = repoParts(repoFull);
      if (!p.owner || !p.repo || !number) return;
      const wantHeld = !!held;
      const prevText = btn ? btn.textContent : '';
      const moved = moveRepoItemHold(repoFull, number, type, wantHeld, canonicalHiveHoldLabel());
      if (btn) { btn.dataset.busy = '1'; btn.textContent = '…'; }
      try {
        const resp = await fetch('/api/repos/' + encodeURIComponent(p.owner) + '/' + encodeURIComponent(p.repo) + '/items/' + number + '/hold', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ held: wantHeld, type: type })
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        if (data.minStatusSeq != null) _statusSeqFloor = Math.max(_statusSeqFloor || 0, Number(data.minStatusSeq) || 0);
        showToast((wantHeld ? 'Held ' : 'Released ') + repoFull + '#' + number, 'success');
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        if (moved) moveRepoItemHold(repoFull, number, type, !wantHeld, canonicalHiveHoldLabel());
        showToast('Hold toggle failed: ' + (e && e.message ? e.message : 'network error'), 'error');
        if (btn) { btn.dataset.busy = ''; btn.textContent = prevText || (wantHeld ? '⏸ Hold' : '▶ Release'); }
      }
    }

    async function releaseIssueOwnerStep(repoFull, number, tag) {
      const p = repoParts(repoFull);
      if (!p.owner || !p.repo || !number) return;
      try {
        const resp = await fetch('/api/repos/' + encodeURIComponent(p.owner) + '/' + encodeURIComponent(p.repo) + '/issues/' + number + '/release', { method: 'POST' });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        if (data.minStatusSeq != null) _statusSeqFloor = Math.max(_statusSeqFloor || 0, Number(data.minStatusSeq) || 0);
        const releaseTag = data.tag || tag || 'release';
        showToast('Created release ' + releaseTag, 'success');
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        showToast('Release failed: ' + (e && e.message ? e.message : 'network error'), 'error');
      }
    }

    // ── Per-repo card width (hivecommons/hive#8000) ───────────────────────
    //
    // Every card used to be exactly as wide as every other one, because the
    // grid sized tracks rather than items. On a hive watching six repos with
    // ~30 pills on a card, a pill column read `#192 Pa…`, `FIX #1109 fix…`,
    // `TESTS #106…`, and the only way to find out what one was was to hover
    // it, one at a time. An operator can now drag the card they are triaging
    // wider — and the quiet ones narrower to make room.
    //
    // Per repo, not per column position: the width follows the repo when the
    // list reorders, which is what "I widened arch-bootc" means. Per browser,
    // not server-side: this is a view preference, not hive state, so it needs
    // no write permission and no round trip.
    const REPO_CARD_DEFAULT_W = 240;
    const REPO_CARD_MIN_W = 180;
    const REPO_CARD_MAX_W = 1400;
    const REPO_CARD_NUDGE_W = 16;
    const REPO_CARD_NUDGE_BIG_W = 64;
    const REPO_CARD_WIDTH_KEY_PREFIX = 'hive-repo-card-widths:';
    const REPO_ORDER_KEY = 'hive.repoOrder.v1';

    /** One stored map per hive, so two hives in one browser do not collide. */
    function repoCardWidthKey() {
      return REPO_CARD_WIDTH_KEY_PREFIX + (((window._lastStatus || {}).hiveId) || 'default');
    }

    /** px, clamped to the drag range; 0 means "no width set, use the default". */
    function clampRepoCardWidth(w) {
      const n = Math.round(Number(w));
      if (!isFinite(n) || n <= 0) return 0;
      return Math.max(REPO_CARD_MIN_W, Math.min(REPO_CARD_MAX_W, n));
    }

    /** The stored {repoFullName: width} map for this hive; always an object. */
    function repoCardWidths() {
      let raw = null;
      try { raw = localStorage.getItem(repoCardWidthKey()); } catch { return {}; }
      const parsed = safeJsonParse(raw || '{}', {});
      return (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) ? parsed : {};
    }

    /** Stored width for one repo, or 0 when it is at the default. */
    function repoCardWidth(full) {
      return clampRepoCardWidth(repoCardWidths()[full]);
    }

    // A reset deletes the entry rather than storing a zero, so a reset card
    // follows the default width if the default ever changes — and so the
    // whole key disappears once every card is back to default, which is what
    // makes "is any card sized?" a cheap question.
    function setRepoCardWidth(full, w) {
      if (!full) return;
      const widths = repoCardWidths();
      const clamped = clampRepoCardWidth(w);
      if (clamped) widths[full] = clamped; else delete widths[full];
      try {
        if (Object.keys(widths).length) localStorage.setItem(repoCardWidthKey(), JSON.stringify(widths));
        else localStorage.removeItem(repoCardWidthKey());
      } catch {}
      if (typeof dashboardUpdateLayoutMenuState === 'function') dashboardUpdateLayoutMenuState();
    }


    function repoPillActionSlot(html, index) {
      const cls = index === 3 ? 'repo-pill-action-slot repo-pill-hold-slot' : 'repo-pill-action-slot';
      return html ? `<span class="${cls}">${html}</span>` : `<span class="${cls} empty" aria-hidden="true">•</span>`;
    }

    function repoPillActionCluster(slots) {
      const filled = Array.isArray(slots) ? slots.slice(0, 4) : [];
      while (filled.length < 4) filled.push('');
      return `<span class="repo-pill-actions">${filled.map(repoPillActionSlot).join('')}</span>`;
    }

    function repoOrderName(full) { return String(full || '').trim().toLowerCase(); }
    function repoOrderDefault(repos) { return (repos || []).map(r => repoOrderName(r && (r.full || r.name))).filter(Boolean); }
    function repoOrderRead() {
      let raw = null;
      try { raw = localStorage.getItem(REPO_ORDER_KEY); } catch { return []; }
      const parsed = safeJsonParse(raw || '[]', []);
      return Array.isArray(parsed) ? parsed.map(repoOrderName).filter(Boolean) : [];
    }
    function repoOrderWrite(order, defaultOrder) {
      const clean = [];
      const seen = new Set();
      (order || []).forEach(name => { const key = repoOrderName(name); if (key && !seen.has(key)) { seen.add(key); clean.push(key); } });
      const def = defaultOrder || [];
      let custom = clean.length !== def.length;
      for (let i = 0; !custom && i < clean.length; i++) custom = clean[i] !== def[i];
      try {
        if (custom) localStorage.setItem(REPO_ORDER_KEY, JSON.stringify(clean));
        else localStorage.removeItem(REPO_ORDER_KEY);
      } catch {}
    }
    function repoOrderNormalize(repos) {
      const def = repoOrderDefault(repos);
      const valid = new Set(def);
      const out = [];
      const seen = new Set();
      repoOrderRead().forEach(name => { if (valid.has(name) && !seen.has(name)) { seen.add(name); out.push(name); } });
      def.forEach(name => { if (!seen.has(name)) { seen.add(name); out.push(name); } });
      repoOrderWrite(out, def);
      return out;
    }
    function repoOrderApply(repos) {
      repos = repos || [];
      const byName = new Map();
      repos.forEach(r => { const key = repoOrderName(r && (r.full || r.name)); if (key && !byName.has(key)) byName.set(key, r); });
      const out = [];
      repoOrderNormalize(repos).forEach(name => { if (byName.has(name)) out.push(byName.get(name)); });
      return out;
    }
    function repoOrderHasCustom(repos) {
      const def = repoOrderDefault(repos);
      const current = repoOrderNormalize(repos);
      if (current.length !== def.length) return true;
      for (let i = 0; i < current.length; i++) if (current[i] !== def[i]) return true;
      return false;
    }

    // The card being dragged right now, or null. renderRepos() holds its
    // repaint while this is set: a governor cycle landing mid-drag would
    // replace the node under the pointer and drop the pointer capture with
    // it, so the card would stop following the cursor.
    let _repoResizeDrag = null;
    let _repoOrderDrag = null;

    // planWaitingLines renders planning.waiting_on_human as one line per plan
    // ("review  owner/repo#12  Title"), for tooltips. Empty string when none.
    function planWaitingLines(plan) {
      return ((plan && plan.waiting_on_human) || []).map(w => {
        const reason = String(w.reason || 'review');
        const icon = reason.includes('design revisions') ? '✎ stuck ' : (reason.includes('design') ? '✎ design ' : (reason === 'stuck' ? '⚠ stuck ' : '● review '));
        return icon + (w.issue ? w.issue + ' ' : '') + (w.epicTitle || w.epicId || '') + (reason && reason !== 'review' && reason !== 'stuck' ? ' — ' + reason : '');
      }).join('\n');
    }

    // planIssueLinks indexes the status payload's planning.issues by
    // "owner/repo#N" so renderRepos can decorate pills in O(1) per issue.
    function planIssueLinks() {
      const out = {};
      ((window._lastStatus && window._lastStatus.planning && window._lastStatus.planning.issues) || []).forEach(l => {
        if (l && l.issueRepo && l.issueNumber) out[l.issueRepo + '#' + l.issueNumber] = l;
      });
      return out;
    }

    // planIssueChip renders one plan's state as a pill chip. Same vocabulary
    // as planStatusBadge, compressed to fit next to an issue number.
    function planIssueChip(l) {
      const total = l.childrenTotal || 0, open = l.childrenOpen || 0, done = total - open;
      let glyph, cls, tip;
      switch (l.state) {
        case 'design_stuck': glyph = '✎⚠';              cls = 'plan-stuck';   tip = 'Design revisions exhausted — needs a human'; break;
        case 'design_review': glyph = '✎';              cls = 'plan-review';  tip = l.designVia === 'spektacular' ? 'Design via Spektacular — Spec checkpoint awaiting approval' : 'Design posted — approve or request changes'; break;
        case 'design':    glyph = '✎';                 cls = 'plan-queued';  tip = l.designVia === 'spektacular' ? 'Design via Spektacular queued for a spec run' : 'Design queued for the architect'; break;
        case 'stuck':     glyph = '⚠';                cls = 'plan-stuck';   tip = 'Plan stuck — the architect produced nothing; open to retry'; break;
        case 'review':    glyph = '● ' + total;       cls = 'plan-review';  tip = total + ' task' + (total === 1 ? '' : 's') + ' drafted — awaiting your review'; break;
        case 'executing': glyph = '▶ ' + done + '/' + total; cls = 'plan-active'; tip = done + ' of ' + total + ' tasks done — plan approved and in progress'; break;
        case 'done':      glyph = '✓ ' + total;       cls = 'plan-done';    tip = 'Plan complete — all ' + total + ' tasks closed'; break;
        default:          glyph = '⧗';                cls = 'plan-queued';  tip = 'Queued for the architect to break into tasks';
      }
      return `<button class="repo-issue-plan pill-icon plan-chip ${cls}" data-action="openPlanReview" data-arg0="${esc(l.epicId)}" title="${esc(tip + ' — click to open the plan')}" aria-label="${esc(tip)}">${esc(glyph)}</button>`;
    }

    function issueUpdatedAt(issue) {
      const value = (issue && (issue.updated_at || issue.created_at)) || '';
      const t = Date.parse(value);
      return Number.isFinite(t) ? t : Number.POSITIVE_INFINITY;
    }

    function issueIsStale(issue) {
      return !!(issue && issue.stale);
    }

    function repoStaleIssueCount(issues) {
      return (issues || []).filter(issueIsStale).length;
    }

    const PR_BAND_ORDER = ['waiting', 'eligible', 'blocked', 'in-review', 'open', 'draft'];

    function prLabelSet(pr) {
      return new Set(((pr && pr.labels) || []).map(l => String(l).toLowerCase()));
    }

    // The configured sentinel label comes from /api/role; until it is known
    // no PR is treated as flagged rather than guessing a name.
    function prSentinelFlagged(pr) {
      const want = String(window._hiveSentinelLabel || '').trim().toLowerCase();
      return !!want && prLabelSet(pr).has(want);
    }

    function prQueued(pr) {
      const want = String(window._hiveAutoMergeLabel || 'lgtm').toLowerCase();
      return prLabelSet(pr).has(want);
    }

    function prUpdatedAt(pr) {
      const value = (pr && (pr.updated_at || pr.created_at)) || '';
      const t = Date.parse(value);
      return Number.isFinite(t) ? t : Number.POSITIVE_INFINITY;
    }

    function prCreatedAt(pr) {
      const value = (pr && pr.created_at) || '';
      const t = Date.parse(value);
      return Number.isFinite(t) ? t : Number.POSITIVE_INFINITY;
    }

    function prReviewClassRank(pr) {
      switch (String((pr && pr.review_class) || '')) {
        case 'fix': return 0;
        case 'tests': return 2;
        default: return 1;
      }
    }

    function prIsStale(pr) {
      return !!(pr && pr.stale);
    }

    // GitHub's own review verdict, read from the protection facts the sweep
    // already carries on the PR (#7515) — nothing is fetched by the card and
    // nothing is inferred: `decision` is set only from review_decision, and
    // an absent decision stays unknown. A base branch with no review rule
    // reports no decision, so the latest opinionated reviews are then shown
    // for what they are — reviewer opinions, not a verdict (#8968). Returns
    // null when GitHub said nothing at all.
    function prGitHubReview(pr) {
      const p = (pr && pr.protection) || {};
      const changesBy = Array.isArray(p.changes_requested_by) ? p.changes_requested_by : [];
      const approvals = Number(p.approvals_given || 0);
      const latestState = String(p.latest_human_review_state || '').toUpperCase();
      const latestBy = p.latest_human_review_by ? ' by @' + p.latest_human_review_by : '';
      const addressed = p.latest_human_review_addressed ? ' addressed' : ' unaddressed';
      const who = changesBy.length ? ' by ' + changesBy.map(l => '@' + l).join(', ') : '';
      const given = approvals ? approvals + ' approval' + (approvals === 1 ? '' : 's') : '';
      const latest = latestState === 'CHANGES_REQUESTED' ? ' (latest human review' + latestBy + ',' + addressed + ')' : (latestState === 'APPROVED' ? ' (latest human review approved' + latestBy + ')' : '');
      switch (String(p.review_decision || '').toUpperCase()) {
        case 'APPROVED': return { decision: 'approved', glyph: '👍', label: 'approved on GitHub' + (given ? ' (' + given + ')' : '') + latest };
        case 'CHANGES_REQUESTED': return { decision: 'changes-requested', glyph: '👎', label: 'changes requested' + who + latest };
        case 'REVIEW_REQUIRED': return { decision: 'review-required', glyph: '👀', label: 'approving review required by branch protection' + (given ? ' (' + given + ' given)' : '') };
        default: break;
      }
      if (changesBy.length) return { decision: '', glyph: '👎', label: 'changes requested' + who + ' — no review decision from GitHub' };
      if (approvals) return { decision: '', glyph: '👍', label: given + ' on GitHub — no review decision' };
      return null;
    }

    function prRequestedReviews(pr) {
      const logins = (Array.isArray(pr && pr.requested_reviewers) ? pr.requested_reviewers : []).map(l => '@' + l);
      const teams = (Array.isArray(pr && pr.requested_teams) ? pr.requested_teams : []).map(t => 'team ' + t);
      return logins.concat(teams);
    }

    // Conversation volume as "N comments, M review threads", or null when
    // GitHub reports neither. Both counts are totals from the snapshot.
    function prConversation(pr) {
      const comments = Number((pr && pr.comment_count) || 0);
      const threads = Number((pr && pr.review_thread_count) || 0);
      if (!comments && !threads) return null;
      const parts = [];
      if (comments) parts.push(comments + ' comment' + (comments === 1 ? '' : 's'));
      if (threads) parts.push(threads + ' review thread' + (threads === 1 ? '' : 's'));
      return { total: comments + threads, label: parts.join(', ') };
    }

    // The covering-PR glyph on an issue pill is derived from hive's own
    // PR-claim evidence (linked_prs, the same source that drives
    // `stage/040-covered-by-pr`), not from GitHub's closing-link data, so a partial
    // PR (`Refs #N`) shows the glyph without hive having to arm auto-close
    // on merge (hivecommons/hive#12144). One badge per issue: the open
    // closing PR wins, then any open PR, then a merged one. Returns null
    // when there is nothing to show.
    function issueCoveringPR(issue) {
      const linkedPRs = Array.isArray(issue && issue.linked_prs) ? issue.linked_prs.filter(pr => pr && pr.number) : [];
      if (!linkedPRs.length) return null;
      const primary = linkedPRs.find(pr => pr.closing && !pr.merged) || linkedPRs.find(pr => pr.state === 'open') || linkedPRs.find(pr => pr.merged) || linkedPRs[0];
      const describe = pr => {
        if (pr.merged) return 'merged via PR #' + pr.number;
        if (pr.closing) return 'closes via PR #' + pr.number;
        if (pr.reference) return 'covered by PR #' + pr.number + ' (partial)';
        return 'covered by PR #' + pr.number;
      };
      const others = linkedPRs.filter(pr => pr !== primary).map(describe);
      return { pr: primary, count: linkedPRs.length, tip: describe(primary) + (others.length ? '; ' + others.join('; ') : '') };
    }
    function issueCoveringPRBadge(issue) {
      const covering = issueCoveringPR(issue);
      if (!covering) return '';
      const count = covering.count > 1 ? '×' + covering.count : '';
      return `<a class="repo-issue-plan pill-icon" href="${esc(covering.pr.url || '')}" target="_blank" rel="noopener" data-stop="1" title="${esc(covering.tip)}" aria-label="${esc(covering.tip)}">🔗${count}</a>`;
    }

    // Issues GitHub reports the PR will close (closingIssuesReferences), as
    // carried on the snapshot — the PR-side mirror of the issue pill's
    // linked_prs badge.
    function prLinkedIssues(pr) {
      return Array.isArray(pr && pr.linked_issues) ? pr.linked_issues.filter(i => i && i.number) : [];
    }

    // One table per band kind drives the band header, the short status chip
    // and the rule text shown wherever the band is named — the repo-card
    // header, the Overview slice and legend row, and the pill legend — so
    // the three cannot drift (#9019). Band keys stay stable (CSS, tests,
    // config); only the operator-facing wording is action-oriented.
    function prBandSpec(band) {
      const specs = ((window._lastStatus || {}).overview_bands || {}).prs || [];
      return specs.find(spec => spec.key === band) || { label: band, short: band, rule: '' };
    }

    function prBandLabel(band) {
      return prBandSpec(band).label;
    }

    function prBandRule(band) {
      return prBandSpec(band).rule;
    }

    function prBandTip(band) {
      const spec = prBandSpec(band);
      return spec.label + ': ' + spec.rule;
    }

    function prBandRank(band) {
      const idx = PR_BAND_ORDER.indexOf(band);
      return idx >= 0 ? idx : PR_BAND_ORDER.length;
    }

    function repoItemNeedsHuman(item) {
      return ((item && item.labels) || []).some(l => String(l).toLowerCase() === 'needs-human');
    }

    function groupedRepoPRs(openPrs, heldPrs) {
      const groups = new Map();
      const entries = (openPrs || []).map(pr => ({ pr, held: false })).concat((heldPrs || []).map(pr => ({ pr, held: true })));
      entries.forEach(entry => {
        const info = entry.pr;
        const key = info.band;
        if (!groups.has(key)) groups.set(key, { key, band: info.band, label: prBandLabel(info.band), tip: prBandTip(info.band), prs: [] });
        groups.get(key).prs.push({ pr: entry.pr, held: entry.held, info });
      });
      const out = Array.from(groups.values());
      out.forEach(g => g.prs.sort((a, b) => yourTurnFirst(a.pr, b.pr) || prUpdatedAt(a.pr) - prUpdatedAt(b.pr) || prReviewClassRank(a.pr) - prReviewClassRank(b.pr) || prCreatedAt(a.pr) - prCreatedAt(b.pr) || Number(a.pr.number || 0) - Number(b.pr.number || 0)));
      out.sort((a, b) => prBandRank(a.band) - prBandRank(b.band));
      return out;
    }

    function prSignalHTML(info) {
      return (info.signals || []).map(s => s.role
        ? `<span class="pill-role" title="${esc(s.label)}">${esc(s.role)}</span>`
        : `<span class="pill-state" title="${esc(s.label)}">${esc(s.glyph)}</span>`).join('');
    }

    function issueBandSpec(band) {
      const specs = ((window._lastStatus || {}).overview_bands || {}).issues || [];
      return specs.find(spec => spec.key === band) || { label: band, short: band, rule: '' };
    }

    function issueBandLabel(band) {
      return issueBandSpec(band).label;
    }

    function issueBandShortLabel(band) {
      return issueBandSpec(band).short;
    }

    function issueBandRule(band) {
      return issueBandSpec(band).rule;
    }

    function issueBandTip(band) {
      const spec = issueBandSpec(band);
      return spec.label + ': ' + spec.rule;
    }

    // Hover text of an issue row's state pill. A non-held BLOCKED pill leads
    // with the server's blocked signal verbatim (its open "blocked by" links,
    // or that the label records no reason), then the row's other signals;
    // held rows (their pill says "held") and every other pill keep the band
    // rule and signals.
    function issueStatusTip(info, held) {
      const signals = info.signals || [];
      if (!held && info.chip === 'blocked') {
        const blocked = signals.find(s => s.glyph === '⛔');
        if (blocked) {
          const rest = signals.filter(s => s !== blocked);
          return blocked.label + (rest.length ? ' — ' + rest.map(s => s.label).join(', ') : '');
        }
      }
      return issueBandTip(info.band) + (signals.length ? ' — ' + signals.map(s => s.label).join(', ') : '');
    }

    function issueBandRank(band) {
      return { ready: 0, 'in-progress': 1, 'agent-filed': 2, waiting: 3, done: 4 }[band] ?? 9;
    }

    // The rows a card draws as ordinary issue rows: actionable, held, and the
    // parked ones (server band `waiting` on a non-actionable row), so a parked
    // issue gets the same plan, design, hold and linked-PR affordances and the
    // same stale dimming as any other.
    function repoCardParkedIssues(repo) {
      return ((repo && repo.nonActionableIssues) || []).filter(i => i && i.band === 'waiting');
    }
    function repoCardTrackedIssues(repo) {
      return ((repo && repo.actionableIssues) || []).concat((repo && repo.heldIssues) || [], repoCardParkedIssues(repo));
    }
    function repoCardUntrackedIssues(repo) {
      return ((repo && repo.nonActionableIssues) || []).filter(i => i && i.band !== 'waiting');
    }

    function groupedRepoIssues(issues) {
      const groups = new Map();
      (issues || []).forEach(issue => {
        const info = issue;
        const key = info.band;
        if (!groups.has(key)) groups.set(key, { key, band: info.band, label: issueBandLabel(info.band), tip: issueBandTip(info.band), issues: [] });
        groups.get(key).issues.push({ issue, info });
      });
      const out = Array.from(groups.values());
      out.forEach(g => g.issues.sort((a, b) => yourTurnFirst(a.issue, b.issue) || issueUpdatedAt(a.issue) - issueUpdatedAt(b.issue) || Number(a.issue.number || 0) - Number(b.issue.number || 0)));
      out.sort((a, b) => issueBandRank(a.band) - issueBandRank(b.band));
      return out;
    }

    function repoNonActionableIssueBucketSpec(issue) {
      const bucket = String((issue && issue.bucket) || 'other');
      if (bucket === 'filtered' && String((issue && issue.reason) || '').toLowerCase() === 'needs-reporter-confirmation') return { key: 'reporter_confirmation', label: 'Waiting on reporter', short: 'reporter', chip: 'reporter', rank: -1, waiting: true, tip: 'Waiting on the reporter to confirm — not dropped by a filter' };
      if (bucket === 'filtered' && repoItemNeedsHuman(issue)) return { key: 'needs_human', label: 'Labelled needs-human', short: 'human', chip: 'human', rank: -1, waiting: true, tip: 'Labelled needs-human — not dropped by a filter' };
      if (bucket === 'hive_advisory') return { key: bucket, label: 'Advisory', short: 'advisory', rank: 0, tip: 'Hive advisory report — counted, but not agent work' };
      if (bucket === 'filtered') return { key: bucket, label: 'Filtered', short: 'filtered', rank: 1, tip: 'Excluded by the project issue filter or an exempt label' };
      if (bucket === 'reporter_triage') return { key: bucket, label: 'Needs triage', short: 'triage', rank: 2, tip: 'Held back until a triage/admission label is added' };
      if (bucket === 'dependency_dashboard') return { key: bucket, label: 'Dependency dashboard', short: 'deps', rank: 3, tip: 'Bot control-panel issue — counted, but not work' };
      if (bucket === 'claimed_by_pr') return { key: bucket, label: 'Claimed by PR', short: 'claimed', rank: 4, tip: 'Originally actionable, but withheld from agent queues because an open PR already claims it' };
      if (bucket === 'claimed') return { key: bucket, label: 'Claimed', short: 'claimed', rank: 5, tip: 'Originally actionable, but withheld from agent queues because another live claim covers it' };
      return { key: bucket, label: 'Other counted', short: 'other', rank: 9, tip: 'Counted outside the actionable and held issue lists' };
    }

    function groupedRepoNonActionableIssues(issues) {
      const groups = new Map();
      (issues || []).forEach(issue => {
        if (!issue) return;
        const spec = repoNonActionableIssueBucketSpec(issue);
        if (!groups.has(spec.key)) groups.set(spec.key, Object.assign({ issues: [] }, spec));
        groups.get(spec.key).issues.push(issue);
      });
      const out = Array.from(groups.values());
      out.forEach(g => g.issues.sort((a, b) => yourTurnFirst(a, b) || issueUpdatedAt(a) - issueUpdatedAt(b) || Number(a.number || 0) - Number(b.number || 0)));
      out.sort((a, b) => a.rank - b.rank || a.label.localeCompare(b.label));
      return out;
    }

    function repoCardIssueGroups(repo, overviewIssueKeys) {
      const issueGroups = groupedRepoIssues(repoCardTrackedIssues(repo)).map(g => {
        const shown = overviewIssueKeys ? g.issues.filter(entry => overviewIssueKeys.has(String(entry.issue.number))) : g.issues;
        return Object.assign({}, g, { issues: shown });
      }).filter(g => g.issues.length);
      const nonActionableIssueGroups = overviewIssueKeys ? [] : groupedRepoNonActionableIssues(repoCardUntrackedIssues(repo));
      return { issueGroups, nonActionableIssueGroups };
    }

    function repoCardIssueRenderedCount(groups) {
      if (!groups) return 0;
      const actionHeld = (groups.issueGroups || []).reduce((n, g) => n + ((g.issues || []).length), 0);
      const nonActionable = (groups.nonActionableIssueGroups || []).reduce((n, g) => n + ((g.issues || []).length), 0);
      return actionHeld + nonActionable;
    }


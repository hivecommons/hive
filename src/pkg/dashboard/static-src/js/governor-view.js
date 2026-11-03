    const RUN_STALL_THRESHOLD_MS = 60 * 60 * 1000;
    const RUN_ACTIVITY_WARN_MS = 90 * 1000;
    const RUN_ACTIVITY_STALL_MS = 5 * 60 * 1000;

    function dashboardRunsFromStatus(data) {
      if (!data || !Object.prototype.hasOwnProperty.call(data, 'runs')) return null;
      const runs = Array.isArray(data.runs) ? data.runs.slice() : [];
      // runHistory (#8349) is the bounded finished-run block; each entry
      // carries an outcome so renderRuns files it under "Run history".
      // Absent on a spoke that could not project runs: nothing is invented.
      const history = data.runHistory && Array.isArray(data.runHistory.recent) ? data.runHistory.recent : [];
      const live = new Set(runs.map(r => r && r.key));
      history.forEach(run => { if (run && run.key && !live.has(run.key)) runs.push(run); });
      return runs;
    }

    function runDateMs(value) {
      if (!value) return 0;
      const ms = new Date(value).getTime();
      return Number.isFinite(ms) ? ms : 0;
    }

    function formatRunDurationSince(value) {
      const ms = runDateMs(value);
      if (!ms) return '—';
      const age = Math.max(0, Date.now() - ms);
      const mins = Math.floor(age / 60000);
      if (mins < 1) return '<1m';
      if (mins < 60) return mins + 'm';
      const hours = Math.floor(mins / 60);
      if (hours < 48) return hours + 'h ' + (mins % 60) + 'm';
      return Math.floor(hours / 24) + 'd ' + (hours % 24) + 'h';
    }

    function formatRunPreciseDuration(seconds) {
      seconds = Math.max(0, Math.floor(Number(seconds) || 0));
      const h = Math.floor(seconds / 3600);
      const m = Math.floor((seconds % 3600) / 60);
      const s = seconds % 60;
      if (h > 0) return h + 'h ' + m + 'm ' + s + 's';
      if (m > 0) return m + 'm ' + s + 's';
      return s + 's';
    }

    function runActivityLastLabel(lastMs, agentAlive, heartbeatMs, outputIdleSeconds) {
      if (!lastMs) return { text: 'last activity unknown', cls: 'stall' };
      const ageMs = Math.max(0, Date.now() - lastMs);
      const idleSeconds = Number(outputIdleSeconds);
      const thresholdMs = Number.isFinite(idleSeconds) && idleSeconds > 0 ? idleSeconds * 1000 : ageMs;
      let cls = '';
      let hint = '';
      if (thresholdMs >= RUN_ACTIVITY_STALL_MS) {
        cls = 'stall';
        hint = (agentAlive || heartbeatMs > Date.now() - RUN_ACTIVITY_STALL_MS) ? ' · still held; may be stalled' : ' · may be stalled';
      } else if (thresholdMs >= RUN_ACTIVITY_WARN_MS) {
        cls = 'warn';
      }
      if (agentAlive) hint += ' · agent alive';
      return { text: 'last activity ' + formatRunPreciseDuration(ageMs / 1000) + ' ago' + hint, cls };
    }

    function renderRunActivity(run) {
      const a = run && run.activity;
      if (!a || !a.alive) return run && run.activity_summary ? `<div class="run-activity-line">${escapeHtml(run.activity_summary)}</div>` : '';
      const startedMs = runDateMs(a.stage_started_at || run.stage_started_at);
      const elapsedRaw = Number(a.elapsed_seconds);
      const elapsed = Number.isFinite(elapsedRaw) ? elapsedRaw : (startedMs ? Math.max(0, Math.floor((Date.now() - startedMs) / 1000)) : 0);
      const lastMs = runDateMs(a.last_activity_at);
      const heartbeatMs = runDateMs(a.lease_heartbeat_at);
      const outputAtMs = runDateMs(a.agent_output_at);
      const outputIdleSeconds = Number(a.agent_output_idle_seconds);
      const last = runActivityLastLabel(lastMs, !!a.agent_pid_alive, heartbeatMs, outputIdleSeconds);
      const phase = a.phase || run.stage || 'stage';
      return `<div class="run-live-activity" aria-label="${escapeHtml(phase)} activity">
        <div class="run-live-head"><span class="run-live-dot active" aria-hidden="true"></span><span>${escapeHtml(phase)} · <span data-run-activity-elapsed data-start-ms="${startedMs}" data-rendered-ms="${Date.now()}" data-elapsed-seconds="${elapsed}">${escapeHtml(formatRunPreciseDuration(elapsed))}</span></span></div>
        <div class="run-live-last${last.cls ? ' ' + last.cls : ''}" data-run-activity-last data-last-ms="${lastMs}" data-agent-alive="${a.agent_pid_alive ? '1' : '0'}" data-heartbeat-ms="${heartbeatMs}" data-output-at-ms="${outputAtMs}" data-output-idle-seconds="${Number.isFinite(outputIdleSeconds) ? outputIdleSeconds : ''}">${escapeHtml(last.text)}</div>
        ${a.last_event ? `<div>${escapeHtml(a.last_event)}</div>` : ''}
      </div>`;
    }

    function updateRunActivityTimers() {
      document.querySelectorAll('[data-run-activity-elapsed]').forEach(el => {
        const started = Number(el.getAttribute('data-start-ms')) || 0;
        const rendered = Number(el.getAttribute('data-rendered-ms')) || Date.now();
        const base = Number(el.getAttribute('data-elapsed-seconds')) || 0;
        const elapsed = started ? Math.max(0, Math.floor((Date.now() - started) / 1000)) : base + Math.max(0, Math.floor((Date.now() - rendered) / 1000));
        el.textContent = formatRunPreciseDuration(elapsed);
      });
      document.querySelectorAll('[data-run-activity-last]').forEach(el => {
        const outputAt = Number(el.getAttribute('data-output-at-ms')) || 0;
        const outputIdle = outputAt ? Math.max(0, Math.floor((Date.now() - outputAt) / 1000)) : Number(el.getAttribute('data-output-idle-seconds'));
        const label = runActivityLastLabel(Number(el.getAttribute('data-last-ms')) || 0, el.getAttribute('data-agent-alive') === '1', Number(el.getAttribute('data-heartbeat-ms')) || 0, outputIdle);
        el.textContent = label.text;
        el.classList.toggle('warn', label.cls === 'warn');
        el.classList.toggle('stall', label.cls === 'stall');
      });
    }

    function updateBackgroundActivityIndicator(runs) {
      const el = document.getElementById('background-activity');
      const text = document.getElementById('background-activity-text');
      if (!el || !text) return;
      runs = Array.isArray(runs) ? runs : [];
      const active = runs.filter(r => r && r.activity && r.activity.alive).length;
      el.classList.toggle('active', active > 0);
      const dot = el.querySelector('.run-live-dot');
      if (dot) dot.classList.toggle('active', active > 0);
      text.textContent = active ? active + ' background ' + (active === 1 ? 'stage' : 'stages') : 'idle';
      el.setAttribute('aria-label', active ? active + ' Spek stages active in the background' : 'No background Spek activity');
      el.title = active ? 'Spek is working in the background' : 'No in-flight Spek stage activity';
    }

    function runCurrentAge(run) {
      return formatRunDurationSince(run.waiting_since || run.stage_started_at);
    }

    function runStageRail(run) {
      const names = ['spec', 'plan', 'implement'];
      const stages = Array.isArray(run.stages) ? run.stages : [];
      return '<div style="display:flex;gap:6px;align-items:center;flex-wrap:wrap">' + names.map(name => {
        const st = stages.find(s => s && s.name === name) || {};
        const lit = run.stage === name || st.status === 'current';
        const done = st.status === 'completed' || st.status === 'observed';
        const color = lit ? 'var(--green)' : done ? 'var(--blue)' : 'var(--muted)';
        const bg = lit ? 'color-mix(in srgb, var(--green) 18%, transparent)' : 'transparent';
        return `<span class="status-badge" style="border-color:${color};color:${color};background:${bg}" title="${escapeHtml(st.receipt || st.status || 'pending')}">${escapeHtml(name)}</span>`;
      }).join('<span style="color:var(--muted)">→</span>') + '</div>';
    }

    function runArtifactLinks(run) {
      const links = [];
      if (run.plan_epic_id) links.push(`<a href="#plan=${encodeURIComponent(run.plan_epic_id)}" data-action="gh3" title="Open plan review">plan ${escapeHtml(run.plan_epic_id)}</a>`);
      if (run.last_receipt) {
        const receipt = String(run.last_receipt);
        if (/^https?:\/\//.test(receipt)) links.push(`<a href="${escapeHtml(receipt)}" target="_blank" rel="noopener">receipt</a>`);
        else links.push(`<span title="${escapeHtml(receipt)}">receipt ${escapeHtml(receipt.slice(0, 18))}${receipt.length > 18 ? '…' : ''}</span>`);
      }
      const observed = (Array.isArray(run.stages) ? run.stages : []).filter(s => s && s.receipt && s.receipt !== run.last_receipt).slice(0, 3);
      for (const st of observed) links.push(`<span title="${escapeHtml(st.receipt)}">${escapeHtml(st.name || 'stage')} artifact</span>`);
      return links.length ? links.join(' · ') : '<span style="color:var(--muted)">No artifacts yet</span>';
    }

    function runMatchesAgent(run, agent) {
      const assignee = String(run.assignee || '').toLowerCase();
      if (!assignee || !agent) return false;
      return [agent.name, agent.id, agent.displayName].filter(Boolean).map(v => String(v).toLowerCase()).includes(assignee);
    }

    function currentRunForAgent(agent) {
      const runs = Array.isArray(window._lastRuns) ? window._lastRuns : [];
      return runs.find(run => runMatchesAgent(run, agent) && run.waiting_on !== 'none');
    }

    function agentCurrentRunRow(agent) {
      const run = currentRunForAgent(agent);
      if (!run) return '<div class="agent-field"><span class="label" title="Run lease currently held by this agent">current run</span><span class="value">—</span></div>';
      const label = run.key || run.title || 'run';
      return `<div class="agent-field"><span class="label" title="Run lease currently held by this agent">current run</span><span class="value"><button class="btn" data-action="openRunDetail" data-arg0="${escapeHtml(run.key)}" style="padding:2px 8px;font-size:0.7rem" title="${escapeHtml(run.title || label)}">${escapeHtml(label)}</button> <span class="status-badge">${escapeHtml(run.stage || 'run')}</span></span></div>`;
    }

    function renderRuns(runs) {
      const el = document.getElementById('runs');
      const badge = document.getElementById('runs-summary-badge');
      const sideBadge = document.getElementById('runs-sidebar-badge');
      if (!el) return;
      if (runs === null) {
        setIfChanged(el, '<div class="loading">runs: unknown</div>');
        if (badge) badge.textContent = 'unknown';
        if (sideBadge) sideBadge.style.display = 'none';
        updateBackgroundActivityIndicator([]);
        return;
      }
      runs = Array.isArray(runs) ? runs : [];
      const isFinishedRun = (run) => run && ((run.outcome && run.outcome !== 'active') || run.status === 'finished' || run.waiting_on === 'none');
      const activeRuns = runs.filter(run => !isFinishedRun(run));
      const finishedRuns = runs.filter(isFinishedRun);
      const human = activeRuns.filter(r => r && r.waiting_on === 'human').length;
      updateBackgroundActivityIndicator(activeRuns);
      if (badge) badge.textContent = activeRuns.length + ' active' + (human ? ' · ' + human + ' blocked on human' : '') + (finishedRuns.length ? ' · ' + finishedRuns.length + ' recent' : '');
      if (sideBadge) { sideBadge.style.display = activeRuns.length ? 'inline-flex' : 'none'; sideBadge.textContent = activeRuns.length; }
      if (!runs.length) {
        setIfChanged(el, '<div style="color:var(--muted);font-size:0.85rem">No active runs.</div>');
        updateBackgroundActivityIndicator([]);
        return;
      }
      const canApprove = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const cards = activeRuns.map(run => {
        const key = run.key || '';
        const waiting = run.waiting_on || 'unknown';
        const waitCls = waiting === 'human' ? 'needs-context' : waiting === 'agent' ? 'done' : '';
        const runGen = run.gen || 0;
        const runStage = run.stage || 'spec';
        const interview = renderSpekInterviewInline(run);
        const checkpointHeld = waiting === 'human' && run.waiting_reason === 'checkpoint_enabled' && (run.plan_epic_id || runStage === 'spec');
        const checkpointLabel = runStage === 'spec' ? 'Spec checkpoint held — review the design before Plan can start.' : 'Plan checkpoint held — review before Implement can start.';
        const actions = canApprove && checkpointHeld
          ? `<div class="campaign-empty"><strong>${escapeHtml(checkpointLabel)}</strong><div class="run-log-actions"><button class="btn" style="color:var(--green)" data-action="runCheckpointAction" data-arg0="${escapeHtml(key)}" data-arg1="${escapeHtml(String(runGen))}" data-arg2="approve">✓ Approve</button><button class="btn" style="color:var(--red)" data-action="runCheckpointAction" data-arg0="${escapeHtml(key)}" data-arg1="${escapeHtml(String(runGen))}" data-arg2="reject">✕ Reject</button><button class="btn" data-action="openRunDetail" data-arg0="${escapeHtml(key)}">Details</button></div></div>`
          : '';
        return `<div class="repo-card" style="min-width:260px">
          <div style="display:flex;justify-content:space-between;gap:8px;align-items:flex-start">
            <div><strong>${escapeHtml(run.title || key || 'run')}</strong><div style="color:var(--muted);font-size:0.75rem">${escapeHtml(key)}${run.repo ? ' · ' + escapeHtml(run.repo) : ''}</div></div>
            <button class="btn" data-action="openRunDetail" data-arg0="${escapeHtml(key)}" style="font-size:0.7rem;padding:3px 8px">detail</button>
          </div>
          <div style="margin-top:10px">${runStageRail(run)}</div>
          ${renderRunActivity(run)}
          <div style="margin-top:10px;display:flex;gap:6px;align-items:center;flex-wrap:wrap">
            <span class="status-badge ${waitCls}" title="Waiting on ${escapeHtml(waiting)}">waiting: ${escapeHtml(waiting)}</span>
            ${run.waiting_reason ? `<span class="status-badge needs-context" title="Waiting reason">${escapeHtml(run.waiting_reason)}</span>` : ''}
            ${run.waiting_reason === 'interview_questions' && run.interview ? `<span class="status-badge needs-context">Spek has ${escapeHtml(String(run.interview.pending || 0))} questions for you</span>` : ''}
            <span class="status-badge" title="${escapeHtml(run.waiting_since || run.stage_started_at || '')}">age ${escapeHtml(runCurrentAge(run))}</span>
            ${run.assignee ? `<span class="status-badge" title="Assignee">${escapeHtml(run.assignee)}</span>` : ''}
          </div>
          ${interview}
          <div style="margin-top:10px;font-size:0.78rem;color:var(--muted)">${runArtifactLinks(run)}</div>
          ${(run.artifact_id || run.artifact_name || run.document_status || run.current_step) ? `<div>${run.artifact_id ? `<span class="status-badge" title="Resolved artifact id">artifact id: ${escapeHtml(run.artifact_id)}</span>` : ''}${run.artifact_name ? `<span class="status-badge" title="Artifact name">artifact: ${escapeHtml(run.artifact_name)}</span>` : ''}${run.document_status ? `<span class="status-badge" title="Spektacular document_status">document: ${escapeHtml(run.document_status)}</span>` : ''}${run.current_step ? `<span class="status-badge" title="Spektacular current_step">step: ${escapeHtml(run.current_step)}</span>` : ''}</div>` : ''}
          <div class="run-log-actions"><button class="btn" data-action="toggleRunLog" data-arg0="${escapeHtml(key)}" data-arg1="${escapeHtml(runStage)}" data-arg2="${escapeHtml(String(runGen))}" data-arg3="run-log-${escapeHtml(safeDomId(key))}">Log</button></div><div id="run-log-${escapeHtml(safeDomId(key))}" class="run-log-panel"></div>
          <div class="text-muted">Powered by <a href="https://docs.hivecommons.dev/docs/hive/spektacular-runs" target="_blank" rel="noopener">Spektacular</a></div>
          ${actions}
        </div>`;
      }).join('');
      const history = finishedRuns.length
        ? `<details style="grid-column:1/-1;background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:10px"><summary style="cursor:pointer">Run history (${finishedRuns.length})</summary><div style="margin-top:8px">${finishedRuns.map(run => `<div style="font-size:0.8rem;padding:4px 0;border-top:1px solid var(--border)"><button class="btn" data-action="openRunDetail" data-arg0="${escapeHtml(run.key || '')}" style="font-size:0.68rem;padding:2px 6px">detail</button> ${escapeHtml(run.key || run.title || 'run')} <span class="status-badge">${escapeHtml(run.outcome || run.status || 'finished')}</span>${run.stage ? ` <span class="status-badge" title="Last completed stage (gen ${escapeHtml(String(run.gen || ''))})">${escapeHtml(run.stage)}</span>` : ''}${run.completed_at ? ` <span style="color:var(--muted)" title="${escapeHtml(run.completed_at)}">${escapeHtml(formatRunDurationSince(run.completed_at))} ago</span>` : ''}</div>`).join('')}</div></details>`
        : '';
      setIfChanged(el, (cards || '<div style="color:var(--muted);font-size:0.85rem">No active runs.</div>') + history);
    }

    const _runInterviewCache = new Map();
    const _runInterviewLoading = new Set();
    function renderSpekInterviewInline(run) {
      if (!run || run.waiting_reason !== 'interview_questions' || !run.interview || !run.key) return '';
      const key = run.key;
      const cached = _runInterviewCache.get(key);
      if (!cached && !_runInterviewLoading.has(key)) {
        setTimeout(() => loadRunInterview(key), 0);
      }
      const pending = run.interview.pending || (cached && cached.pending) || 0;
      if (!cached) {
        return `<div id="interview" class="spek-interview-card"><div class="spek-interview-head"><h4>Spek has ${escapeHtml(String(pending))} question${pending === 1 ? '' : 's'} for you</h4></div><div class="spek-question-context">Loading the in-app answer form…</div></div>`;
      }
      if (!cached.pending) {
        return `<div class="spek-interview-card"><div class="spek-interview-head"><h4>Interview answers sent</h4></div><div class="spek-question-context">Spek is resuming with your answers.</div></div>`;
      }
      const questions = (cached.questions || []).map((q, i) => {
        const id = escapeHtml(q.id || '');
        const inputName = 'answer-' + safeDomId(key) + '-' + safeDomId(q.id || '');
        const hasOptions = Array.isArray(q.options) && q.options.length > 0;
        const control = hasOptions
          ? `<div class="spek-interview-options" role="radiogroup" aria-label="Options for question ${i + 1}">${q.options.map(opt => `<label><input type="radio" name="${inputName}" value="${escapeHtml(opt)}" ${q.default === opt ? 'checked' : ''}><span>${escapeHtml(opt)}</span></label>`).join('')}</div>
             <div class="spek-interview-note"><span>Optional note for Spek (details, names, caveats)</span><textarea data-interview-note="${id}" placeholder="Add context if the option above needs qualifying"></textarea></div>`
          : `<textarea data-interview-answer="${id}" aria-label="Answer to question ${i + 1}" placeholder="${escapeHtml(q.default || 'Type your answer')}"></textarea>`;
        return `<div class="spek-question" data-interview-question="${id}" data-interview-input="${escapeHtml(inputName)}"><span class="spek-question-num" aria-hidden="true">${i + 1}</span><p class="spek-question-text">${escapeHtml(q.text || q.id)}</p>${q.context ? `<div class="spek-question-context">${escapeHtml(q.context)}</div>` : ''}${control}</div>`;
      }).join('');
      const n = cached.pending;
      return `<div id="interview" data-request-id="${escapeHtml(cached.request_id || '')}" class="spek-interview-card" aria-live="polite"><div class="spek-interview-head"><h4>Spek has ${escapeHtml(String(n))} question${n === 1 ? '' : 's'} for you</h4><span class="spek-interview-step">Stage: ${escapeHtml(cached.stage || run.stage || 'spec')} · step: ${escapeHtml(cached.step || 'clarification')}</span></div>${questions}<div class="spek-interview-actions"><span class="spek-interview-hint">Answers resume the paused Spek session and are recorded in the run transcript.</span><button class="hv-btn btn-secondary btn-sm" data-action="loadRunInterview" data-arg0="${escapeHtml(key)}">Refresh questions</button><button class="hv-btn btn-primary btn-sm" data-action="submitRunInterview" data-arg0="${escapeHtml(key)}" data-arg-types="s,t">Submit answers</button></div></div>`;
    }

    async function loadRunInterview(key) {
      if (!key || _runInterviewLoading.has(key)) return;
      _runInterviewLoading.add(key);
      try {
        const res = await fetch('/api/runs/' + encodeURIComponent(key) + '/interview', { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await res.text());
        _runInterviewCache.set(key, await res.json());
        refreshStatus();
      } catch (e) {
        showToast('Interview questions unavailable: ' + (e && e.message ? e.message : e), 'error');
      } finally {
        _runInterviewLoading.delete(key);
      }
    }
    window.loadRunInterview = loadRunInterview;

    async function submitRunInterview(key, button) {
      if (!key) return;
      const card = button && button.closest ? button.closest('.spek-interview-card') : null;
      if (!card) return;
      const answers = [];
      card.querySelectorAll('[data-interview-question]').forEach(qEl => {
        const id = qEl.getAttribute('data-interview-question') || '';
        const ta = qEl.querySelector('[data-interview-answer]');
        let answer = ta ? ta.value : '';
        if (!ta) {
          const checked = qEl.querySelector('input[type="radio"]:checked');
          answer = checked ? checked.value : '';
          const note = qEl.querySelector('[data-interview-note]');
          const noteText = note && note.value ? note.value.trim() : '';
          if (answer && noteText) answer = answer + ' — ' + noteText;
        }
        if (id && String(answer).trim()) answers.push({ id, answer: String(answer).trim() });
      });
      const expected = card.querySelectorAll('[data-interview-question]').length;
      if (!answers.length || answers.length < expected) {
        showToast('Answer every pending Spek question before submitting.', 'warning');
        return;
      }
      try {
        const res = await fetch('/api/runs/' + encodeURIComponent(key) + '/interview', {
          method: 'POST', headers: Object.assign({ 'Content-Type': 'application/json' }, _inceptionAuthHeaders()),
          body: JSON.stringify({ request_id: card.dataset.requestId, answers })
        });
        if (!res.ok) throw new Error(await res.text());
        _runInterviewCache.set(key, { ok: true, pending: 0, questions: [] });
        showToast('Interview answers sent; Spek is resuming.', 'success');
        refreshStatus();
      } catch (e) {
        showToast('Submitting interview answers failed: ' + (e && e.message ? e.message : e), 'error');
      }
    }
    window.submitRunInterview = submitRunInterview;

    async function runPlanAction(epicID, action) {
      if (!epicID || !['approve', 'reject'].includes(action)) return;
      try {
        const res = await fetch('/api/plan/' + encodeURIComponent(epicID) + '/' + action, { method: 'POST', headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await res.text());
        showToast(action === 'approve' ? 'Run plan approved.' : 'Run plan rejected.', 'success');
        refreshStatus();
      } catch (e) {
        showToast('Run plan action failed: ' + (e && e.message ? e.message : e), 'error');
      }
    }
    window.runPlanAction = runPlanAction;

    async function runCheckpointAction(key, gen, action) {
      if (!key || !['approve', 'reject'].includes(action)) return;
      try {
        const res = await fetch('/api/runs/' + encodeURIComponent(key) + '/checkpoint', {
          method: 'POST',
          headers: Object.assign({'Content-Type': 'application/json'}, _inceptionAuthHeaders()),
          body: JSON.stringify({ action, gen: Number(gen) || 0 })
        });
        if (!res.ok) throw new Error(await res.text());
        showToast(action === 'approve' ? 'Run checkpoint approved.' : 'Run checkpoint rejected.', 'success');
        refreshStatus();
      } catch (e) {
        showToast('Run checkpoint action failed: ' + (e && e.message ? e.message : e), 'error');
      }
    }
    window.runCheckpointAction = runCheckpointAction;

    async function openRunDetail(key) {
      const panel = document.getElementById('run-detail');
      if (!panel || !key) return;
      panel.style.display = 'block';
      panel.innerHTML = '<div class="loading">Loading run detail…</div>';
      try {
        const res = await fetch('/api/runs/' + encodeURIComponent(key), { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await res.text());
        const run = await res.json();
        let auditRows = '';
        try {
          const auditRes = await fetch('/api/audit', { headers: _inceptionAuthHeaders() });
          if (auditRes.ok) {
            const audit = await auditRes.json();
            const needle = String(key);
            const epic = String(run.plan_epic_id || '');
            auditRows = ((audit.entries || []).filter(e => {
              const text = JSON.stringify(e || {});
              return text.includes('run=' + needle) || text.includes(needle) || (epic && text.includes(epic));
            }).slice(0, 8).map(e => `<div style="font-size:0.75rem;padding:4px 0;border-top:1px solid var(--border)"><strong>${escapeHtml(e.action || 'audit')}</strong> ${escapeHtml(e.actor || '')} <span style="color:var(--muted)">${escapeHtml(e.at || e.time || '')}</span><div style="color:var(--muted)">${escapeHtml(e.detail || e.message || '')}</div></div>`).join(''));
          }
        } catch (_) {}
        const stages = (Array.isArray(run.stages) ? run.stages : []).map(st => `<div style="display:flex;justify-content:space-between;gap:10px;padding:4px 0;border-top:1px solid var(--border)"><span>${escapeHtml(st.name || 'stage')} <span class="status-badge">${escapeHtml(st.status || '')}</span></span><span style="color:var(--muted)">${escapeHtml(st.receipt || (st.gen ? 'gen ' + st.gen : ''))}</span></div>`).join('');
        const approval = auditRows || '<div style="color:var(--muted);font-size:0.8rem">No approval or audit records matched this run yet.</div>';
        const detailLogID = 'run-detail-log-' + safeDomId(key);
        panel.innerHTML = `<div style="display:flex;justify-content:space-between;gap:12px;align-items:flex-start"><div><h3 style="margin:0 0 4px">${escapeHtml(run.title || run.key || 'Run detail')}</h3><div style="color:var(--muted);font-size:0.8rem">${escapeHtml(run.key || '')}${run.repo ? ' · ' + escapeHtml(run.repo) : ''}</div></div><button class="btn" data-action="closeRunDetail">Close</button></div>
          <div style="margin-top:12px">${runStageRail(run)}</div>
          ${renderRunActivity(run)}
          <div style="margin-top:12px;font-size:0.82rem">Artifacts: ${runArtifactLinks(run)}</div>
          ${(run.artifact_id || run.artifact_name || run.document_status || run.current_step) ? `<div><span class="status-badge">artifact id: ${escapeHtml(run.artifact_id || 'unknown')}</span><span class="status-badge">artifact: ${escapeHtml(run.artifact_name || 'unknown')}</span><span class="status-badge">document: ${escapeHtml(run.document_status || 'unknown')}</span><span class="status-badge">step: ${escapeHtml(run.current_step || 'unknown')}</span></div>` : ''}
          <div class="run-log-actions"><button class="btn" data-action="toggleRunLog" data-arg0="${escapeHtml(run.key || key)}" data-arg1="${escapeHtml(run.stage || 'spec')}" data-arg2="${escapeHtml(String(run.gen || 0))}" data-arg3="${escapeHtml(detailLogID)}">Log</button></div><div id="${escapeHtml(detailLogID)}" class="run-log-panel"></div>
          <h4 style="margin:14px 0 6px">Artifact chain</h4>${stages || '<div style="color:var(--muted);font-size:0.8rem">No stage artifacts recorded.</div>'}
          <div class="text-muted">Powered by <a href="https://docs.hivecommons.dev/docs/hive/spektacular-runs" target="_blank" rel="noopener">Spektacular</a></div>
          <h4 style="margin:14px 0 6px">Approval and audit records</h4>${approval}`;
      } catch (e) {
        panel.innerHTML = `<div style="color:var(--red)">Failed to load run detail: ${escapeHtml(e && e.message ? e.message : e)}</div>`;
      }
    }
    window.openRunDetail = openRunDetail;

    function closeRunDetail() {
      const panel = document.getElementById('run-detail');
      if (panel) panel.style.display = 'none';
    }
    window.closeRunDetail = closeRunDetail;

    async function toggleRunLog(key, stage, gen, targetID) {
      const panel = document.getElementById(targetID || '');
      if (!panel || !key) return;
      if (panel.dataset.open === '1') {
        panel.dataset.open = '0';
        panel.innerHTML = '';
        return;
      }
      panel.dataset.open = '1';
      panel.innerHTML = '<div class="run-log-pre">Loading log…</div>';
      try {
        const qs = new URLSearchParams();
        if (stage) qs.set('stage', stage);
        if (gen && String(gen) !== '0') qs.set('gen', String(gen));
        qs.set('tail', '200');
        const res = await fetch('/api/runs/' + encodeURIComponent(key) + '/log?' + qs.toString(), { headers: _inceptionAuthHeaders() });
        const text = await res.text();
        if (!res.ok) throw new Error(text || ('HTTP ' + res.status));
        panel.innerHTML = '<pre class="run-log-pre">' + escapeHtml(text || '(empty log)') + '</pre>';
      } catch (e) {
        panel.innerHTML = '<pre class="run-log-pre">Log unavailable: ' + escapeHtml(e && e.message ? e.message : e) + '</pre>';
      }
    }
    window.toggleRunLog = toggleRunLog;

    function stalledRunHealthRows(runs) {
      if (!Array.isArray(runs)) return '';
      return runs.filter(run => run && run.waiting_since && (Date.now() - runDateMs(run.waiting_since)) >= RUN_STALL_THRESHOLD_MS).map(run => {
        const label = `Run ${run.key || run.title || ''} stalled`;
        return `<div class="health-item" title="${escapeHtml(run.waiting_since)}"><span class="health-dot fail"></span><span class="health-label">${escapeHtml(label)} <span class="status-badge">${escapeHtml(run.waiting_on || 'waiting')}</span> <span class="status-badge">${escapeHtml(formatRunDurationSince(run.waiting_since))}</span></span></div>`;
      }).join('');
    }



    function formatFixTime(minutes) {
      if (!minutes || minutes <= 0) return '—';
      if (minutes >= 1440) { const d = Math.floor(minutes / 1440); const h = Math.floor((minutes % 1440) / 60); return h > 0 ? `${d}d ${h}h` : `${d}d`; }
      if (minutes >= 60) { const h = Math.floor(minutes / 60); const m = Math.round(minutes % 60); return m > 0 ? `${h}h ${m}m` : `${h}h`; }
      return `${Math.round(minutes)}m`;
    }

    let govPRModelsWindow = '7d';
    let govPRModelsSort = 'effectiveness';
    let govPRModelsLoading = false;
    let govPRModelsData = null;
    let reviewerAccuracyLoading = false;
    let reviewerAccuracyData = null;

    function normalizeGovernorPRModelsWindowName(windowName) {
      return ['7d', '30d', 'all'].includes(windowName) ? windowName : '7d';
    }

    function renderGovernorFromLastStatus() {
      const lastStatus = window._lastStatus;
      if (!lastStatus) return;
      renderGovernor(lastStatus.governor || {}, lastStatus.cadenceMatrix || [], lastStatus);
    }

    function setGovernorPRModelsWindow(windowName) {
      govPRModelsWindow = normalizeGovernorPRModelsWindowName(windowName);
      return loadGovernorPRModels(true, govPRModelsWindow);
    }
    window.setGovernorPRModelsWindow = setGovernorPRModelsWindow;
    function setGovernorPRModelsSort(sortName) {
      govPRModelsSort = sortName === 'count' ? 'count' : 'effectiveness';
      if (window._lastStatus?.governor) renderGovernor(window._lastStatus.governor, window._lastStatus.cadenceMatrix || [], window._lastStatus);
    }
    window.setGovernorPRModelsSort = setGovernorPRModelsSort;

    function governorPRModelUnattributed(b) {
      return !!(b?.effectiveness?.unattributed) || !b?.model || String(b.model).toLowerCase() === 'unknown';
    }

    // Column definitions for the PRs-by-model table; rendered once as a sticky
    // header (with the ? tooltips) and repeated per cell only as data-label for
    // the narrow breakpoint.
    const GOV_PR_MODEL_COLUMNS = [
      ['merged', 'Merged', 'Merged PRs in the window. Rank requires at least the minimum shown in the subtitle.'],
      ['first_pass', '1st pass', 'Share of merged PRs that merged without any review-requested fix round. Rank uses the Wilson 95% lower bound of this rate, so a perfect score on a handful of PRs scores below a near-perfect score on many.'],
      ['pr_runs', 'PR runs', 'Share of recorded task runs (from the contributor run log) that ended with a verified pull request.'],
      ['fail', 'Fail', 'Share of recorded task runs that failed (crashed, timed out, or exited non-zero). Multiplies the rank score by (1 − fail).'],
      ['no_ship', 'No ship', 'Share of recorded task runs that completed but produced no pull request. Multiplies the rank score by (1 − no ship).'],
    ];

    function governorPRModelRows(data) {
      const rows = [...(data?.buckets || [])];
      if (govPRModelsSort === 'count') {
        return rows.sort((a, b) => (b.prs || 0) - (a.prs || 0) || String(a.model || '').localeCompare(String(b.model || '')) || String(a.backend || '').localeCompare(String(b.backend || '')));
      }
      return rows.sort((a, b) => {
        const ae = a.effectiveness || {}, be = b.effectiveness || {};
        // Unattributed ("unknown" model) rows are listed, never ranked: pin them last.
        const au = governorPRModelUnattributed(a), bu = governorPRModelUnattributed(b);
        if (au !== bu) return au ? 1 : -1;
        const ar = ae.rank || Number.MAX_SAFE_INTEGER, br = be.rank || Number.MAX_SAFE_INTEGER;
        if (ar !== br) return ar - br;
        if (!!be.eligible !== !!ae.eligible) return be.eligible ? 1 : -1;
        if ((b.merged || 0) !== (a.merged || 0)) return (b.merged || 0) - (a.merged || 0);
        return (b.prs || 0) - (a.prs || 0) || String(a.model || '').localeCompare(String(b.model || ''));
      });
    }

    async function loadGovernorPRModels(force, windowName = govPRModelsWindow) {
      const requestedWindow = normalizeGovernorPRModelsWindowName(windowName);
      if (govPRModelsLoading) return;
      if (!force && govPRModelsData && govPRModelsData.window === requestedWindow) return;
      govPRModelsWindow = requestedWindow;
      govPRModelsLoading = true;
      try {
        const res = await fetch(`/api/governor/pr-models?window=${encodeURIComponent(requestedWindow)}`);
        if (res.ok) {
          const data = await res.json();
          govPRModelsData = { ...data, window: data?.window || requestedWindow };
        }
      } catch (_) {
        govPRModelsData = { window: requestedWindow, total: 0, buckets: [] };
      } finally {
        govPRModelsLoading = false;
        renderGovernorFromLastStatus();
      }
    }

    async function loadReviewerAccuracy(force) {
      if (reviewerAccuracyLoading) return;
      if (!force && reviewerAccuracyData) return;
      reviewerAccuracyLoading = true;
      try {
        const res = await fetch('/api/reviewer/accuracy');
        reviewerAccuracyData = res.ok ? await res.json() : { samples: 0, perspectives: [], reviewer_models: [] };
      } catch (_) {
        reviewerAccuracyData = { samples: 0, perspectives: [], reviewer_models: [] };
      } finally {
        reviewerAccuracyLoading = false;
        if (window._lastStatus?.governor) renderGovernor(window._lastStatus.governor, window._lastStatus.cadenceMatrix || [], window._lastStatus);
      }
    }

    function renderReviewerAccuracyTile() {
      const data = reviewerAccuracyData;
      const pct = v => `${((Number(v) || 0) * 100).toFixed(0)}%`;
      if (!data) {
        return '<div class="gov-pr-models"><div class="gov-pr-models-head"><div><div class="gov-pr-models-title">Reviewer accuracy</div><div class="gov-pr-model-sub">Loading verdict calibration…</div></div></div></div>';
      }
      const perspectives = data.perspectives || [];
      const models = data.reviewer_models || [];
      const row = g => `<tr><td>${escapeHtml(g.name || 'unknown')}</td><td>${pct(g.false_approve_rate)} <span class="gov-pr-model-sub">(${g.false_approvals || 0}/${g.approvals || 0})</span></td><td>${pct(g.false_block_rate)} <span class="gov-pr-model-sub">(${g.false_blocks || 0}/${g.blocks || 0})</span></td><td>${g.samples || 0}</td></tr>`;
      const modelLine = models.slice(0, 4).map(g => `${escapeHtml(g.name || 'unknown')}: FA ${pct(g.false_approve_rate)} (${g.false_approvals || 0}/${g.approvals || 0}) · FB ${pct(g.false_block_rate)} (${g.false_blocks || 0}/${g.blocks || 0})`).join('<br>');
      const conf = models.flatMap(g => (g.confidence_buckets || []).map(b => ({...b, model: g.name || 'unknown'}))).slice(0, 8);
      const confLine = conf.length ? `<div class="gov-pr-model-reworked"><div class="gov-pr-model-reworked-title">Confidence calibration by model</div><ol>${conf.map(b => `<li>${escapeHtml(b.model)} score ${escapeHtml(b.bucket)}: merge ${pct(b.merge_rate)} · bad ${pct(b.bad_outcome_rate)} · n=${b.samples || 0}</li>`).join('')}</ol></div>` : '';
      const empty = (data.recorded_verdicts || 0) > 0
        ? `<div class="gov-pr-models-empty">${data.recorded_verdicts || 0} reviewer verdict${(data.recorded_verdicts || 0) === 1 ? '' : 's'} recorded, but no merged/closed PR outcomes resolved yet.</div>`
        : '<div class="gov-pr-models-empty">No reviewer verdicts recorded in the window yet.</div>';
      const table = perspectives.length ? `<table class="gov-pr-model-table" aria-label="Reviewer accuracy by perspective"><thead><tr><th>Perspective</th><th>false approve</th><th>false block</th><th>sample</th></tr></thead><tbody>${perspectives.map(row).join('')}</tbody></table>` : empty;
      return `<div class="gov-pr-models">
        <div class="gov-pr-models-head"><div><div class="gov-pr-models-title">Reviewer accuracy</div><div class="gov-pr-model-sub">${data.samples || 0} verdict sample${(data.samples || 0) === 1 ? '' : 's'} · last ${data.window_days || 30}d</div></div></div>
        ${table}
        ${modelLine ? `<div class="gov-pr-model-sub gov-pr-model-sub-spaced">${modelLine}</div>` : ''}
        ${confLine}
      </div>`;
    }

    function renderGovernorPRModelsTile() {
      const data = govPRModelsData && govPRModelsData.window === govPRModelsWindow ? govPRModelsData : null;
      const total = data?.total || 0;
      const buttons = ['7d', '30d', 'all'].map(w => `<button type="button" class="${govPRModelsWindow === w ? 'active' : ''}" data-action="setGovernorPRModelsWindow" data-arg0="${w}" title="Show PRs authored in ${w === 'all' ? 'all time' : 'the last ' + w}">${w}</button>`).join('');
      const sortButtons = [['effectiveness', 'rank'], ['count', 'PR count']].map(([v, label]) => `<button type="button" class="${govPRModelsSort === v ? 'active' : ''}" data-action="setGovernorPRModelsSort" data-arg0="${v}" title="Sort PRs by model by ${label}">${label}</button>`).join('');
      const modelRows = data ? governorPRModelRows(data) : [];
      const modelCount = data && Array.isArray(data.buckets) ? data.buckets.length : 0;
      const minMerged = data?.min_merged_prs || 5;
      const topModel = modelRows[0] && (modelRows[0].model || 'unknown');
      const collapsedSummary = topModel ? `#1 ${escapeHtml(topModel)} · ${modelCount} model${modelCount === 1 ? '' : 's'}` : `${modelCount} model${modelCount === 1 ? '' : 's'}`;
      const subtitle = `${total} agent-authored PR${total === 1 ? '' : 's'} · rank requires ${minMerged} merged`;
      const rows = !data
        ? '<div class="gov-pr-models-empty">Loading PRs by model…</div>'
        : total === 0
          ? '<div class="gov-pr-models-empty">No agent-authored PRs in window.</div>'
          : modelRows.map(b => {
              const prs = b.prs || 0;
              const share = total ? (prs / total * 100) : 0;
              const mergedPct = prs ? (b.merged || 0) / prs * 100 : 0;
              const openPct = prs ? (b.open || 0) / prs * 100 : 0;
              const closedPct = Math.max(0, 100 - mergedPct - openPct);
              const rw = b.rework || {};
              const eff = b.effectiveness || {};
              const pct = v => `${((Number(v) || 0) * 100).toFixed(0)}%`;
              const unattributed = governorPRModelUnattributed(b);
              const rank = eff.rank ? `<span class="gov-pr-model-rank">#${eff.rank}</span>` : (unattributed ? '<span class="gov-pr-model-rank unattributed" title="No model recorded on these PRs; listed but not ranked">unattributed</span>' : '');
              const values = {
                merged: String(eff.merged_prs ?? b.merged ?? 0),
                first_pass: pct(eff.first_pass_merge_rate ?? rw.first_pass_merge_rate),
                pr_runs: pct(eff.verified_pr_run_rate),
                fail: pct(eff.failure_rate),
                no_ship: pct(eff.nothing_to_ship_rate),
              };
              const metrics = `<div class="gov-pr-model-metrics" role="group" aria-label="Model effectiveness metrics">${GOV_PR_MODEL_COLUMNS.map(([key, label]) => `<div class="gov-pr-model-metric" data-label="${label}">${values[key]}</div>`).join('')}</div>`;
              const backends = Array.isArray(b.backends) && b.backends.length ? b.backends : String(b.backend || 'unknown').split(',').map(x => x.trim()).filter(Boolean);
              const backendChips = backends.map(x => `<span class="gov-pr-model-backend">${escapeHtml(x)}</span>`).join('');
              const sources = (b.sources || []).map(src => `${src.model || 'unknown'} / ${src.backend || 'unknown'}: ${src.prs || 0} PRs (${src.merged || 0} merged)`).join('\n');
              const agentBreakdown = (b.agents || []).map(a => `${a.agent || 'unknown'} / ${a.backend || b.backend || 'unknown'}: ${a.prs} PRs (${a.merged || 0} merged, ${a.open || 0} open, ${a.closed_unmerged || 0} closed-unmerged)`).join('\n');
              const scoreLine = eff.rank ? `\nRank #${eff.rank} · score ${(Number(eff.score) || 0).toFixed(3)} = Wilson lower bound(${eff.first_pass_merged ?? '?'}/${eff.merged_prs ?? b.merged ?? 0} first pass) × (1 − fail) × (1 − no ship)` : (unattributed ? '\nUnattributed: no model recorded, listed but not ranked' : `\nUnranked: fewer than ${data.min_merged_prs || 5} merged PRs`);
              const tip = `${b.model || 'unknown'} via ${backends.join(', ')}${scoreLine}\n${prs} PRs (${share.toFixed(1)}%)\n${b.merged || 0} merged · ${b.open || 0} open · ${b.closed_unmerged || 0} closed-unmerged\nFirst-pass ${values.first_pass} · PR run rate ${values.pr_runs} · failure ${values.fail} · nothing-to-ship ${values.no_ship}\nAvg rounds ${(rw.avg_review_rounds || 0).toFixed(1)} · avg fix attempts ${(rw.avg_fix_attempts || 0).toFixed(1)}${sources ? '\n\nRaw model ids folded into this row:\n' + sources : ''}${agentBreakdown ? '\n\n' + agentBreakdown : ''}`;
              return `<div class="gov-pr-model-row${unattributed ? ' unattributed' : ''}" title="${escapeHtml(tip)}">
                <div><div class="gov-pr-model-name">${rank}${escapeHtml(b.model || 'unknown')}</div><div class="gov-pr-model-sub">${backendChips}</div></div>
                <div class="gov-pr-model-bar" aria-label="${escapeHtml(tip)}">
                  <div class="gov-pr-model-seg merged" style="width:${mergedPct}%"></div>
                  <div class="gov-pr-model-seg open" style="width:${openPct}%"></div>
                  <div class="gov-pr-model-seg closed" style="width:${closedPct}%"></div>
                </div>
                ${metrics}
                <div class="gov-pr-model-share">${prs} · ${share.toFixed(1)}%</div>
              </div>`;
            }).join('');
      const reworkTable = data && total > 0 ? `<table class="gov-pr-model-table" aria-label="PR rework by model">
        <thead><tr><th>Model</th><th>First-pass</th><th>Review rounds</th><th>Fix attempts</th><th>Follow-up commits</th><th>Median merge</th><th>Sample</th></tr></thead>
        <tbody>${(data.buckets || []).map(b => {
          const rw = b.rework || {};
          return `<tr>
            <td>${escapeHtml(b.model || 'unknown')}<div class="gov-pr-model-sub">${escapeHtml(b.backend || 'unknown')}</div></td>
            <td>${((rw.first_pass_merge_rate || 0) * 100).toFixed(0)}%</td>
            <td>${(rw.avg_review_rounds || 0).toFixed(1)} / ${rw.worst_review_rounds || 0}</td>
            <td>${(rw.avg_fix_attempts || 0).toFixed(1)} / ${rw.worst_fix_attempts || 0}</td>
            <td>${(rw.avg_follow_up_commits || 0).toFixed(1)}</td>
            <td>${formatFixTime(rw.median_time_to_merge_minutes || 0)}</td>
            <td>${rw.sample_prs || 0}</td>
          </tr>`;
        }).join('')}</tbody>
      </table>` : '';
      const mostReworked = data && (data.most_reworked || []).length ? `<div class="gov-pr-model-reworked">
        <div class="gov-pr-model-reworked-title">Most reworked PRs</div>
        <ol>${(data.most_reworked || []).map(pr => {
          const label = `${pr.repo || ''}#${pr.number || ''}`;
          const href = pr.url ? ` href="${escapeHtml(pr.url)}" target="_blank" rel="noopener"` : '';
          const fixers = (pr.fixer_models || []).length ? ` · fixer ${escapeHtml((pr.fixer_models || []).join(', '))}` : '';
          return `<li><a${href}>${escapeHtml(label)}</a> ${escapeHtml(pr.model || 'unknown')} · ${pr.review_rounds || 0} rounds · ${pr.fix_attempts || 0} fixes · ${pr.follow_up_commits || 0} follow-up commits${fixers}</li>`;
        }).join('')}</ol>
      </div>` : '';
      const header = data && total > 0 ? `<div class="gov-pr-model-row gov-pr-model-head" aria-hidden="false">
          <div>Model</div>
          <div>Outcome</div>
          <div class="gov-pr-model-metrics">${GOV_PR_MODEL_COLUMNS.map(([, label, help]) => `<div class="gov-pr-model-metric"><span class="gov-pr-model-col" title="${escapeHtml(help)}">${label}<span class="gov-pr-model-help" aria-hidden="true">?</span></span></div>`).join('')}</div>
          <div class="gov-pr-model-share">PRs · share</div>
        </div>` : '';
      return `<div id="governor-pr-models-section" class="advisory-subsection governor-subsection">
        <div class="section-label section-header-toggle gov-pr-models-subheader" role="button" tabindex="0" aria-expanded="true" aria-controls="governor-pr-models-section-body" data-action="toggleSection" data-keydown-action="sectionHeaderKey" data-arg-types="e,s" data-arg0="governor-pr-models-section" data-arg1="governor-pr-models-section" data-keys="*">
          <span class="section-chevron" aria-hidden="true">▼</span>
          <span>PRS BY MODEL${dashboardSectionHelpMark('governor-pr-models-section')}<span class="gov-pr-models-subtitle">${escapeHtml(subtitle)}</span></span>
          <span class="gov-pr-models-summary">${collapsedSummary}</span>
          <span class="gov-pr-models-toggle" data-stop="1">${buttons}${sortButtons}</span>
        </div>
        <div class="section-body" id="governor-pr-models-section-body"><div class="gov-pr-models">
          ${header}
          ${rows}
          ${reworkTable}
          ${mostReworked}
          <div class="gov-pr-model-legend"><span class="merged">merged</span><span class="open">open</span><span class="closed">closed-unmerged</span></div>
        </div></div>
      </div>`;
    }

    function renderGovernor(gov, cadenceMatrix, data) {
      const el = document.getElementById('governor');
      const isStarting = !gov.active && !gov.mode;
      const cls = gov.active ? 'active' : (isStarting ? 'starting' : 'dead');
      el.className = `governor ${cls}`;
      const issuesSpark = sparkline('govIssues');
      const prsSpark = sparkline('govPrs');
      const totalSpark = sparkline('govTotal');
      const sharedActionable = (data && data.actionableNow) || {};
      const issueCount = Number(sharedActionable.issues ?? gov.issues ?? 0) || 0;
      const prCount = Number(sharedActionable.prs ?? gov.prs ?? 0) || 0;
      const govPressure = Number(sharedActionable.total ?? (issueCount + prCount)) || 0;
      if (data && data.actionableNow) setSidebarPairBadge(document.getElementById('governor-sidebar-badge'), issueCount, prCount, 'actionable: ' + issueCount + ' issues / ' + prCount + ' PRs');
      const fallbackGovEquations = overviewKPIGovernorFallbackEquations(data, issueCount, prCount);
      const issueEquation = sharedActionable.issueEquation || fallbackGovEquations.issueEq;
      const prEquation = sharedActionable.prEquation || fallbackGovEquations.prEq;
      const issueEquationText = issueEquation?.text || '';
      const prEquationText = prEquation?.text || '';
      const issueEquationHTML = issueEquationText ? renderActionableEquationSubline(issueEquation) : '';
      const prEquationHTML = prEquationText ? renderActionableEquationSubline(prEquation) : '';
      const issueEquationTitle = issueEquation?.title || issueEquationText || 'ACTIONABLE ISSUES — issue work that can move without waiting.';
      const prEquationTitle = prEquation?.title || prEquationText || 'ACTIONABLE PRS — pull requests that can move without waiting.';
      const currentMode = gov.mode || 'idle';
      const modes = ['idle', 'quiet', 'busy', 'surge'];

      // Gauge bar — thresholds from governor.env (dynamic via status API)
      const _gt = gov.thresholds || {};
      const CLASSIC_THRESH_QUIET = _gt.quiet ?? 2;
      const CLASSIC_THRESH_BUSY = _gt.busy ?? 10;
      const CLASSIC_THRESH_SURGE = _gt.surge ?? 20;
      const gaugeBounds = governorPressureBarBoundaries({ quiet: CLASSIC_THRESH_QUIET, busy: CLASSIC_THRESH_BUSY, surge: CLASSIC_THRESH_SURGE }, govPressure);
      const maxQ = gaugeBounds.max;
      const pct = gaugeBounds.pressurePct;
      const modeColor = GOVERNOR_MODE_COLORS[gov.mode] || 'var(--muted)';

      // Timeline strip from 24h persistent timeline data
      const rawTl = window._timelineData || {};
      const tlData = Array.isArray(rawTl) ? rawTl : (rawTl.modes || []);
      const tlModes = tlData.map(s => s.mode || 'unknown');
      const ticks = tlModes.map(m => `<div class="tick tick-${m}"></div>`).join('');
      const firstTime = tlData.length > 0 ? formatDashboardDateTime(tlData[0].t, {month:'numeric',day:'numeric',hour:'numeric',minute:'2-digit',hour12:true,timeZoneName:'short'}) : '';
      const lastTime = tlData.length > 0 ? formatDashboardDateTime(tlData[tlData.length-1].t, {month:'numeric',day:'numeric',hour:'numeric',minute:'2-digit',hour12:true,timeZoneName:'short'}) : '';

      const itm = data.issueToMerge || {};
      const itmAvg = itm.avg_minutes || 0;
      const itmMedian = itm.median_minutes || 0;
      const itmCount = itm.count || 0;
      const FIX_TIME_SPARK_COLOR = 'var(--muted)'; /* sparkline tint only — the value itself is neutral */
      const itmHistory = (itm.history || []).map(h => h.median || h.avg);
      const itmSpark = sparkSvg(itmHistory, FIX_TIME_SPARK_COLOR, (itm.history || []).map(h => h.t), 'governor:mttr');
      const itmExplain = 'MTTR — how fast issues get resolved.\n\nWHAT: The headline figure is the MEDIAN time from an issue being opened to the fixing PR being merged. Lower is better; the sparkline shows the recent trend.\n\nHOW: For every issue closed by a merged PR in the last 30 days, we measure (merge time − open time). The tile shows the median of those durations; the breakdown below lists avg / p90 / fastest / slowest and the sample size.\n\n';
      const itmTitle = itmCount > 0 ? itmExplain + `median: ${formatFixTime(itmMedian)} | avg: ${formatFixTime(itmAvg)} | p90: ${formatFixTime(itm.p90_minutes)} | fastest: ${formatFixTime(itm.fastest_minutes)} | slowest: ${formatFixTime(itm.slowest_minutes)} | ${itmCount} fixes in last 30d` : itmExplain + 'No fixes recorded in the last 30 days yet.';

      const holdData = data.hold || {};
      const holdTotal = holdData.total || 0;
      const holdItems = holdData.items || [];
      const holdExplain = 'ON HOLD — work intentionally paused, awaiting a human.\n\nWHAT: Issues and PRs that agents will NOT act on until a maintainer clears them. Excluded from the shared Actionable now count.\n\nHOW: Count of open issues/PRs carrying a hold label (hold / do-not-merge / blocked). The list below names each held item; the sparkline shows how the backlog of held work is trending.\n\n';
      // Hold items arrive pre-sorted by review class (fixes > refactors/docs
      // > tests) then age (#6183); the class tag makes that order legible.
      const holdTooltip = holdItems.length > 0 ? escapeHtml(holdExplain + holdItems.map(h => `${h.type === 'pr' ? 'PR' : 'Issue'} ${h.repo}#${h.number}${h.review_class ? ' [' + h.review_class + ']' : ''}: ${h.title}`).join('\n')) : escapeHtml(holdExplain + 'Nothing is currently on hold.');
      const holdSpark = sparkline('govHold');
      loadGovernorPRModels(false, govPRModelsWindow);
      loadReviewerAccuracy(false);

      const governorBody = `
        <div class="gov-title"><button type="button" class="dashboard-grip" data-dashboard-grip data-stop="1" aria-label="Move Governor section" aria-pressed="false">⠿</button>Governor</div>
        <div class="gov-grid sec-headline" data-collapsed-keep>
          <div class="gov-stat" title="TIMER — is the governor's evaluation loop alive?&#10;&#10;WHAT: The governor re-evaluates mode and kicks due agents on a fixed cadence. This shows whether that loop is running.&#10;&#10;HOW: 'active' if the governor reported a heartbeat within its expected interval; 'starting' during boot before the first tick; 'DEAD' if no heartbeat has arrived — agents will NOT be kicked automatically until it recovers."><div class="label">timer</div><div class="val ${cls}">${gov.active ? '● active' : (isStarting ? '⏳ starting' : '⚠ DEAD')}</div></div>
          <div class="gov-stat" title="MODE — the governor's current activity level.&#10;&#10;WHAT: Sets how aggressively agents are scheduled. idle → quiet → busy → surge, each with faster cadences.&#10;&#10;HOW: The mode comes from the governor's latest queue evaluation; the gauge uses the shared Overview Actionable now total so the Governor split and Overview KPI agree. Thresholds are configurable in Governor settings."><div class="label">mode</div><div class="val" style="color:${modeColor}">${escapeHtml(gov.mode)}</div></div>
          <div class="gov-stat" aria-label="${escapeHtml(issueEquationTitle)}"><div class="label">actionable issues</div><div class="spark-row"><span class="val">${issueCount}</span>${issuesSpark}</div>${issueEquationHTML ? `<div class="overview-kpi-subline">${issueEquationHTML}</div>` : ''}</div>
          <div class="gov-stat" aria-label="${escapeHtml(prEquationTitle)}"><div class="label">actionable PRs</div><div class="spark-row"><span class="val">${prCount}</span>${prsSpark}</div>${prEquationHTML ? `<div class="overview-kpi-subline">${prEquationHTML}</div>` : ''}</div>
          <div class="gov-stat" title="${itmTitle}"><div class="label">MTTR</div><div class="spark-row"><span class="val">${formatFixTime(itmMedian)}</span>${itmSpark}</div></div>
          ${fleetRunStatHtml()}
          <div class="gov-stat" title="${holdTooltip}"><div class="label">on hold</div><div class="spark-row"><span class="val">${holdTotal}</span>${holdSpark}</div></div>
          ${standbyPauseFactsHtml(gov, 'stat')}
          <div class="gov-stat" title="RUNS BLOCKED ON HUMAN — active long-running runs whose projection says waiting_on=human."><div class="label">blocked on human</div><div class="val">${(Array.isArray(data.runs) ? data.runs : []).filter(r => r && r.waiting_on === 'human').length}</div></div>
          ${(() => {
            // Merge hotspots tile (GET /api/hotspots): files concurrent PRs
            // keep colliding on; present only when the advisor is wired.
            const hs = data && data.hotspots;
            if (!hs || typeof hs.count !== 'number') return '';
            const tip = `MERGE HOTSPOTS — files concurrent PRs keep colliding on.&#10;&#10;WHAT: ${hs.count} file(s) above the hotspot thresholds in the window; ${hs.actionable || 0} warrant a split; ${hs.filed || 0} already have a modularize issue filed for the ci-maintainer.&#10;&#10;HOW: GET /api/hotspots (hivectl governor hotspots) lists each file with its PRs, concurrent pairs, conflicts, size, recommendation and issue.`;
            const label = hs.filed > 0 ? `merge hotspots · ${hs.filed} filed` : 'merge hotspots';
            return `<div class="gov-stat" title="${tip}"><div class="label">${label}</div><div class="val${hs.actionable > 0 ? ' starting' : ''}">${hs.count}</div></div>`;
          })()}
          ${(() => {
            // Planning tile: active plan-trees, with plans-awaiting-review highlighted
            // amber (human-action-required, like on-hold). Reads /api/status planning
            // block. Hidden entirely when planning is unavailable (ACMM < 5, where the
            // architect isn't scheduled) so it never shows a misleading 0.
            const plan = (data && data.planning) || {};
            if (!plan.available) return '';
            const active = plan.active_plans || 0;
            const awaiting = plan.awaiting_review || 0;
            const pending = plan.decomposing || 0;
            const alertCls = awaiting > 0 ? ' planning-alert' : '';
            const planExplain = 'PLANNING — goals being decomposed into work.\n\nWHAT: The architect agent breaks high-level goals (epics) into a tree of child issues before other agents execute them. Available only at ACMM level 5–6, where the architect is scheduled.\n\nHOW: Counts plan-trees by state from the planning ledger — active (approved, in progress), awaiting review (need your approval before work starts), and decomposing (being broken down now). Amber means human action is required.\n\n';
            const tip = planExplain + (awaiting > 0
              ? `${awaiting} plan(s) awaiting your review — approve them in the plan view before agents start the work`
              : `${active} active plan-tree(s)` + (pending ? `, ${pending} decomposing` : '')) + '\n\nClick to open the plan view.';
            const valTxt = awaiting > 0 ? `${awaiting} to review` : String(active);
            return `<div class="gov-stat" style="cursor:pointer" data-action="openPlanList" title="${escapeHtml(tip)}"><div class="label">planning</div><div class="val${alertCls}">${escapeHtml(valTxt)}${pending > 0 ? ` <span style="opacity:.6;font-size:.7em">+${pending}</span>` : ''}</div></div>`;
          })()}
          <div class="gov-stat" title="NEXT RUN — when the governor next wakes up.&#10;&#10;WHAT: The next scheduled moment the governor will re-evaluate mode and kick any agents that are due.&#10;&#10;HOW: Computed from the last governor evaluation plus the effective evaluation interval, including API-budget stretch. If that moment has already passed while the timer is active, this tile says delayed instead of showing a stale past timestamp."><div class="label">next run</div><div class="val">${escapeHtml(formatGovernorNextRun(gov))}</div></div>
        </div>
        <div class="temp-gauge" style="cursor:pointer" data-action="gh5" title="Click to adjust governor thresholds">
          <div class="temp-gauge-label"><span>Pressure: ${govPressure} (${issueCount} issues + ${prCount} PRs)</span><span>max ${maxQ}</span></div>
          <div class="temp-gauge-track" style="background:${governorModeGradient(gaugeBounds)}">
            <div class="temp-gauge-glow" style="width:100%"></div>
            <div class="temp-gauge-ticks"><span style="left:2%;-webkit-transform:translate(0,-50%);transform:translate(0,-50%)">0</span><span style="left:${gaugeBounds.quietPct}%;-webkit-transform:translate(-50%,-50%);transform:translate(-50%,-50%)">${CLASSIC_THRESH_QUIET}</span><span style="left:${gaugeBounds.busyPct}%;-webkit-transform:translate(-50%,-50%);transform:translate(-50%,-50%)">${CLASSIC_THRESH_BUSY}</span><span style="left:${gaugeBounds.surgePct}%;-webkit-transform:translate(-50%,-50%);transform:translate(-50%,-50%)">${CLASSIC_THRESH_SURGE}</span><span style="left:98%;-webkit-transform:translate(-100%,-50%);transform:translate(-100%,-50%)">${maxQ}</span></div>
            <div class="temp-gauge-needle" style="left:${pct}%;--gauge-position:${pct}%" data-val="${govPressure}"></div>
          </div>
          <div class="temp-gauge-labels">
            <span class="tl-idle" style="position:absolute;left:${gaugeBounds.quietPct / 2}%;transform:translateX(-50%)">idle</span>
            <span class="tl-quiet" style="position:absolute;left:${(gaugeBounds.quietPct + gaugeBounds.busyPct) / 2}%;transform:translateX(-50%)">quiet</span>
            <span class="tl-busy" style="position:absolute;left:${(gaugeBounds.busyPct + gaugeBounds.surgePct) / 2}%;transform:translateX(-50%)">busy</span>
            <span class="tl-surge" style="position:absolute;left:${(gaugeBounds.surgePct + 100) / 2}%;transform:translateX(-50%)">surge</span>
          </div>
        </div>
        <div class="gov-timeline">
          <div class="gov-timeline-label"><span>${firstTime}</span><span>Mode Timeline</span><span>${lastTime}</span></div>
          <div class="gov-timeline-strip">${ticks || '<div class="tick tick-unknown" style="flex:1"></div>'}</div>
          <div class="gov-timeline-legend">
            <span class="tl-idle">idle</span>
            <span class="tl-quiet">quiet</span>
            <span class="tl-busy">busy</span>
            <span class="tl-surge">surge</span>
          </div>
        </div>
        ${renderBudgetBar(data.budget || {})}
        ${renderGovernorPRModelsTile()}
        ${renderReviewerAccuracyTile()}
        ${renderCadenceMatrix(cadenceMatrix, currentMode, modes)}`;
      el.innerHTML = sectionCardShell({ id: 'governor', title: '🚦 Governor', gripLabel: 'Governor', panelClass: 'governor-card', settingsHtml: DASHBOARD_SECTION_CARD_CONFIG.governor.settingsHtml, summaryHtml: `${esc(currentMode)} · ${govPressure} pressure`, summaryTitle: 'Governor mode and queue pressure' }, governorBody.replace(/<div class="gov-title"[\s\S]*?<\/div>\n?/, ''));
      applySectionCollapse('governor');
      applySectionCollapse('governor-pr-models-section');
    }

    function renderCadenceMatrix(matrix, currentMode, modes) {
      if (!matrix || !matrix.length) return '';
      const agents = _sidebarAgents();
      // #7112: follow the sidebar-visible agent set — live status.agents plus
      // disabled config-only agents from configuredAgents — rather than the
      // ACMM pack rule AND its "active outside pack" escape hatch
      // (buildAgentsWithHidden) — instead of re-deriving the raw pack roster
      // from acmmPackAgents. The raw roster omits a running/enabled agent
      // outside the level's pack (e.g. a custom "review" agent), so filtering on
      // it dropped that agent's cadence row even though #7110 makes its nav item
      // and card render. Using _sidebarAgents() keeps the matrix aligned with
      // the sidebar for disabled config-only agents too: configuredAgents is
      // exactly how the sidebar shows rows whose enabled flag is false and who
      // therefore have no runtime status entry. acmmPackAgents keeps its raw
      // roster meaning for its other consumer (_packAgentNote).
      const statusAgentSet = new Set(agents.map(a => a.name));
      // Fail open when the agent list is unpopulated — mirroring the old
      // `!packSet ||` bypass. window._lastAgents can be empty independently of
      // _lastStatus.cadenceMatrix: an SSE agent-status delta may carry an empty
      // roster (it overwrites _lastAgents but not cadenceMatrix), or the first
      // governor paint may beat the first full status. With no authoritative
      // set to filter against, show every cadence row rather than blanking the
      // whole matrix. When the set IS populated (the normal case, including the
      // #7112 out-of-pack "review" agent and disabled sidebar agents), filtering
      // proceeds and only agents absent from the sidebar stay suppressed.
      const filterByStatus = statusAgentSet.size > 0;
      const { sorted: sortedAgents } = _sortAgentsBySidebar(agents);
      const sidebarOrder = sortedAgents.map(a => a.name);
      const matrixNames = matrix.map(r => r.agent).filter(n => !filterByStatus || statusAgentSet.has(n));
      const agentOrder = sidebarOrder.filter(n => matrixNames.includes(n));
      for (const n of matrixNames) { if (!agentOrder.includes(n)) agentOrder.push(n); }
      const rows = agentOrder.map(aname => {
        const row = matrix.find(r => r.agent === aname);
        if (!row) return '';
        const agentData = agents.find(a => a.name === aname);
        // Manual/persisted pause wins over offByCadence (matches the card/
        // sidebar/detail renderers) — the row badge must say "paused", not
        // "off", when the operator paused the agent.
        const agentPaused = agentData && agentData.paused === true;
        // #12015: name non-operator pause triggers (thrash-breaker, login-detector…)
        // inline so a breaker pause does not read like a cadence choice.
        const pausedTrigger = agentPaused ? String(agentData.pausedTrigger || '') : '';
        const pausedLabel = pausedTrigger && pausedTrigger !== 'dashboard-api' ? 'paused · ' + pausedTrigger : 'paused';
        const pausedBadgeHtml = agentPaused ? `<span class="pause-badge" data-status="warn" title="${esc(pauseProvenanceLine(agentData))}">${esc(pausedLabel)}</span>` : '<span class="pause-badge" data-status="warn">paused</span>';
        const agentOff = agentData && agentData.offByCadence === true && !agentPaused;
        const agentOnDemand = agentData && agentData.onDemand === true;
        // The green "working" dot means "the governor is running this agent
        // on schedule". An agent that is paused or has no cadence in the
        // current mode is NOT supposed to be running, so a lingering busy
        // flag (a run kicked before the mode/pack change, or a stale summary)
        // must not paint it green — that read as "L6 is kicking operations".
        // Same rule the sidebar/card renderers apply via isRunning.
        const isWorking = !!(agentData && agentData.busy === 'working' && !agentPaused && !agentOff);
        const agentDisabled = agentData && agentIsDisabled(agentData);
        const rowClass = agentDisabled ? ' class="agent-disabled"' : '';
        const continuousModes = agentData && Array.isArray(agentData.continuousModes) ? agentData.continuousModes : [];
        const cells = modes.map(m => {
            const val = row[m] || '?';
            const tip = row[`${m}Title`] || `Click to adjust ${aname} ${m} cadence`;
            const isActive = m === currentMode;
            const isContinuous = val === 'continuous';
            const isOff = val === 'off';
            // In the active mode the cell must agree with the row badge: when the
            // agent is off by cadence here, say "no cadence", not "paused".
            const showOff = (isOff || (isActive && agentOff)) && !agentOnDemand && !isContinuous;
            const showOnDemand = agentOnDemand || val === 'on demand';
            const showPaused = !showOff && !showOnDemand && !isContinuous && (val === 'paused' || agentPaused);
            const colCls = isActive ? `col-active mode-${m}` : '';
            const stateCls = isContinuous ? ' continuous' : showOnDemand ? ' on-demand' : showOff ? ' off' : showPaused ? ' paused' : '';
            const dot = (isActive && isWorking && !agentPaused && !showOff && !showOnDemand && !agentDisabled && !isContinuous) ? '<span class="active-dot"></span>' : '';
            const badge = isContinuous ? '<span class="on-demand-badge" data-status="info">∞ continuous</span>' : showOnDemand ? '<span class="on-demand-badge" data-status="info">on demand</span>' : showOff ? '<span class="off-badge" data-status="neutral" title="No cadence configured for this mode — the governor never kicks this agent while the hive is in it. The agent is still enabled; this is a per-mode schedule gap, not the 0/1 switch.">no cadence</span>' : showPaused ? pausedBadgeHtml : val;
            if (agentDisabled) {
              return `<td class="${colCls} cadence-disabled-cell" title="${esc((agentData && (agentData.displayName || agentData.name)) || aname)} is disabled; cadence intervals apply again when the agent is enabled">${badge}</td>`;
            }
            return `<td class="${colCls}${stateCls}" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(aname)}" data-tab="Cadences" title="${esc(tip)}">${dot}${badge}</td>`;
          }).join('');
        const nameDot = isWorking ? '<span class="agent-dot"></span>' : '';
        const pauseBadge = agentDisabled ? '<span class="off-badge" data-status="neutral" title="Disabled in config — the governor does not schedule this agent">off</span>' : agentOnDemand ? '<span class="on-demand-badge" data-status="info">on demand</span>' : agentPaused ? pausedBadgeHtml : agentOff ? `<span class="off-badge" data-status="neutral">${esc(agentCadenceOffLabel())}</span>` : '';
        const continuousBadge = agentData && agentData.continuous === true ? ` <span class="on-demand-badge" data-status="${agentData.continuousBlocked && agentData.continuousBlocked !== 'not_in_mode' ? 'warn' : 'info'}" title="Continuous mode — re-kick after session end plus cool-down${continuousModes.length ? '; modes ' + esc(continuousModes.join(', ')) : ''}${agentData.continuousBlocked === 'budget' ? '; held by budget guard' : ''}">${esc(agentCadenceDisplay(agentData))}${agentData.continuousBlocked && agentData.continuousBlocked !== 'not_in_mode' ? ' held' : ''}</span>` : '';
        const displayLabel = (agentData && agentData.displayName) || aname;
        const powerCell = agentData
          ? `<td class="power-col">${agentPowerSwitchHtml(agentData)}</td>`
          : '<td class="power-col">—</td>';
        const cliPinned = agentData && (agentData.pinnedBoth || agentData.pinnedCli);
        const modelPinned = agentData && (agentData.pinnedBoth || agentData.pinnedModel);
        const cChip = agentData && agentData.cli ? cliChip(agentData.cli, cliPinned, aname) : '?';
        const mChip = agentData && agentData.model ? modelChip(agentData.model, agentData.govReason, modelPinned, aname) : '';
        const cliPinBtn = (agentData && !cliPinned) ? `<button class="hv-btn pin-toggle btn-icon btn-sm" data-action="togglePin" data-agent="${aname}" data-pin-type="cli" data-unpin="false" title="Pin CLI">📌</button>` : '';
        const modelPinBtn = (agentData && !modelPinned) ? `<button class="hv-btn pin-toggle btn-icon btn-sm" data-action="togglePin" data-agent="${aname}" data-pin-type="model" data-unpin="false" title="Pin model">📌</button>` : '';
        return `<tr${rowClass}>${powerCell}<td class="name-col">${nameDot}<button type="button" class="gov-agent-name-link" data-action="ocSelectAgent" data-agent="${esc(aname)}" title="Open ${esc(displayLabel)}'s agent card">${esc(displayLabel)}</button>${pauseBadge}${continuousBadge}</td>${cells}<td>${cChip} ${cliPinBtn}</td><td>${mChip} ${modelPinBtn}</td></tr>`;
      }).join('');
      const headers = modes.map(m => {
        const isActive = m === currentMode;
        const cls = `mode-${m}${isActive ? ' mode-active' : ''}`;
        return `<th class="${cls}">${m}${isActive ? ' ◀' : ''}</th>`;
      }).join('');
      const templateBar = `<div class="gov-matrix-templates-bar">
        <span class="gov-matrix-templates-label">Cadence templates:</span>
        <div class="gov-matrix-templates-buttons">
          <button type="button" class="hv-btn btn-secondary btn-sm" data-action="applyCadenceTemplate" data-arg0="testing" title="New Level testing: sets conservative cadence (surge: pause, busy: 2h, quiet: 4h, idle: 8h) for all agents to reduce risk of token over-consumption">New Level testing</button>
          <button type="button" class="hv-btn btn-secondary btn-sm" data-action="applyCadenceTemplate" data-arg0="weekly" title="Weekly sprint: sets moderate cadence (surge: 1h, busy: 2h, quiet: 4h, idle: 8h) for all agents">Weekly sprint</button>
          <button type="button" class="hv-btn btn-secondary btn-sm" data-action="applyCadenceTemplate" data-arg0="biweekly" title="Biweekly sprint: sets relaxed cadence (surge: 2h, busy: 4h, quiet: 8h, idle: 16h) for all agents">Biweekly sprint</button>
          <button type="button" class="hv-btn btn-secondary btn-sm" data-action="applyCadenceTemplate" data-arg0="daily" title="Daily cadence: sets active daily cadence (surge: 30m, busy: 1h, quiet: 2h, idle: 4h) for all agents">Daily cadence</button>
        </div>
      </div>`;
      return `${templateBar}<div class="gov-matrix-wrap" tabindex="0" role="region" aria-label="Agent cadence table"><p class="mobile-table-hint">Scroll horizontally for more columns.</p><table class="gov-matrix">
        <thead><tr><th class="power-col" title="Agent power: 1 enabled, 0 disabled">Power</th><th></th>${headers}<th>method</th><th>model</th></tr></thead>
        <tbody>${rows}</tbody>
      </table></div>`;
    }

    /* --- Collapsible advisory sections (persisted in localStorage) --- */
    const ADVISORY_COLLAPSE_PREFIX = 'hive-collapse-'; /* localStorage key prefix */
    const COLLAPSE_TRANSITION_MS = 200; /* match CSS transition duration */

    function isAdvisoryCollapsed(key) {
      return localStorage.getItem(ADVISORY_COLLAPSE_PREFIX + key) === 'true';
    }

    function setAdvisoryCollapsed(key, collapsed) {
      localStorage.setItem(ADVISORY_COLLAPSE_PREFIX + key, collapsed ? 'true' : 'false');
    }

    function toggleAdvisorySection(sectionId) {
      const body = document.getElementById(sectionId + '-body');
      const chevron = document.getElementById(sectionId + '-chevron');
      if (!body || !chevron) return;
      const isCollapsed = body.classList.contains('collapsed');
      if (isCollapsed) {
        body.classList.remove('collapsed');
        body.style.maxHeight = body.scrollHeight + 'px';
        chevron.classList.remove('collapsed');
        /* After transition, remove max-height so content can resize naturally */
        setTimeout(() => { if (!body.classList.contains('collapsed')) body.style.maxHeight = 'none'; }, COLLAPSE_TRANSITION_MS);
      } else {
        body.style.maxHeight = body.scrollHeight + 'px';
        /* Force reflow before adding collapsed class so transition triggers */
        void body.offsetHeight;
        body.classList.add('collapsed');
        chevron.classList.add('collapsed');
      }
      setAdvisoryCollapsed(sectionId, !isCollapsed);
    }

    function advisoryFindingTime(iso) {
      const time = iso ? new Date(iso).getTime() : NaN;
      if (!Number.isFinite(time) || time <= 0) return 'unknown';
      const seconds = Math.max(0, Math.floor((Date.now() - time) / 1000));
      const age = seconds < 60 ? 'just now' : seconds < 3600 ? Math.floor(seconds / 60) + 'm ago'
        : seconds < 86400 ? Math.floor(seconds / 3600) + 'h ago' : Math.floor(seconds / 86400) + 'd ago';
      return `<time datetime="${escapeHtml(iso)}" title="${escapeHtml(formatDashboardDateTime(time))}">${age}</time>`;
    }

    function advisoryFindingStateHTML(f) {
      const warnings = [];
      if (f.provenance_stale) warnings.push('provenance stale — not re-verified at the analyzed snapshot');
      // Match Hive's existing cached-evidence caption: a pinned provenance
      // takes precedence over the no-provenance replay counter.
      if (f.cached_replays > 0 && !f.provenance_sha) warnings.push('cached replay — not re-verified');
      if (f.path_stale) warnings.push('referenced file/path stale');
      const evidence = f.provenance_sha ? ` @ <code title="${escapeHtml(f.provenance_sha)}">${escapeHtml(f.provenance_sha.slice(0, 12))}</code>` : '';
      let html = `<div class="advisory-finding-state" style="margin-top:5px;color:var(--muted);font-size:var(--fs-sm)">
        <div>Created ${advisoryFindingTime(f.timestamp)} · last seen ${advisoryFindingTime(f.last_seen_at)}</div>
        <div class="text-warn"${warnings.length ? '' : ''}>${warnings.length ? '⚠ ' : ''}Evidence${evidence} · ${warnings.length ? warnings.map(escapeHtml).join('; ') : 'no known freshness warning'}</div>`;
      const work = f.linked_work || [];
      if (!work.length) html += '<div>No linked remediation</div>';
      for (const ref of work) {
        const kind = ref.kind === 'pr' ? 'PR' : ref.kind === 'issue' ? 'Issue' : 'Work';
        const label = `${kind} #${ref.number} (${ref.owner}/${ref.repo})`;
        const url = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/(issues|pull)\/[1-9][0-9]*$/.test(ref.url || '') ? ref.url : '';
        const link = url ? `<a href="${escapeHtml(url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(label)}</a>` : escapeHtml(label);
        const state = ['OPEN', 'CLOSED', 'MERGED'].includes(ref.state) ? ref.state : 'UNKNOWN';
        html += `<div>${link} · ${state}${state === 'UNKNOWN' ? ' (state unavailable)' : ref.kind === 'pr' && state === 'OPEN' ? ' · in flight' : ''}${ref.checked_at ? ' · checked ' + advisoryFindingTime(ref.checked_at) : ''}</div>`;
      }
      return html + '</div>';
    }

    function renderAdvisoryDigest(digest) {
      refreshSectionCardShell('advisory-digest-section');
      const el = document.getElementById('advisory-digest');
      if (!el) return;
      if (!digest || !digest.total_count) {
        setSectionSummary('advisory-digest-section', '0 findings');
        setIfChanged(el, `<div id="advisory-digest-main-body" class="advisory-agent-body muted" style="font-size:var(--fs-base);text-align:left">No advisory findings yet. Agents will post findings here as they analyze the codebase.</div>`);
        applySectionCollapse('advisory-digest-section');
        return;
      }
      setSectionSummary('advisory-digest-section', `${digest.total_count} finding${digest.total_count === 1 ? '' : 's'}`);
      const sevIcon = s => ({ critical: '🔴', high: '🟠', medium: '🟡', low: '🔵' }[s] || '⚪');
      const agents = Object.keys(digest.by_agent || {}).sort();
      let html = `<div id="advisory-digest-main-body" class="advisory-agent-body" style="max-height:none">`;
      html += `<div style="font-size:0.78rem;color:var(--muted);margin-bottom:var(--sp-5)">
        <strong>Mode:</strong> ${escapeHtml(digest.mode)} | <strong>Findings:</strong> ${digest.total_count} | <strong>Updated:</strong> ${formatDashboardDateTime(digest.generated_at)}
      </div>`;
      for (const agent of agents) {
        const findings = digest.by_agent[agent] || [];
        const agentKey = 'advisory-agent-' + agent;
        const agentCollapsed = isAdvisoryCollapsed(agentKey);
        html += `<div class="mb-5">
          <div style="font-weight:600;font-size:0.85rem;margin-bottom:var(--sp-3);color:var(--text);cursor:pointer;user-select:none" data-action="toggleAdvisorySection" data-arg0="${agentKey}">
            <span id="${agentKey}-chevron" class="collapse-chevron${agentCollapsed ? ' collapsed' : ''}">▼</span>
            ${escapeHtml(agent)} <span style="color:var(--muted);font-weight:400">(${findings.length})</span>
          </div>
          <div id="${agentKey}-body" class="advisory-agent-body${agentCollapsed ? ' collapsed' : ''}" style="display:flex;flex-direction:column;gap:var(--sp-2);${agentCollapsed ? 'max-height:0' : 'max-height:none'}">`;
        for (const f of findings) {
          const loc = f.file ? ` <code style="font-size:0.72rem;color:var(--muted)">${escapeHtml(f.file)}${f.line ? ':' + f.line : ''}</code>` : '';
          html += `<div style="font-size:0.8rem;padding:6px 10px;background:var(--bg);border-radius:var(--r-sm);border-left:3px solid ${f.severity === 'critical' ? 'var(--red)' : f.severity === 'high' ? 'var(--orange)' : f.severity === 'medium' ? 'var(--yellow)' : 'var(--muted)'}">
            ${sevIcon(f.severity)} <strong>[${escapeHtml(f.type || 'finding')}]</strong> ${escapeHtml(f.title)}${loc}
            ${f.detail ? `<div class="mt-2 text-muted fs-sm">${escapeHtml(f.detail)}</div>` : ''}
            ${advisoryFindingStateHTML(f)}
          </div>`;
        }
        html += '</div></div>';
      }
      html += '</div>';
      setIfChanged(el, html);
      applySectionCollapse('advisory-digest-section');
    }

    // Planning (⧉ Plan button + `plan`-label trigger) requires ACMM L5+: the
    // architect agent that decomposes epics only has a cadence at L5/L6. Below
    // that, offering the button would mint an epic nothing ever decomposes, so
    // hide it entirely. Mirrors planning.PlanningMinACMMLevel on the backend.
    const PLANNING_MIN_ACMM_LEVEL = 5;
    function planningAllowed() {
      return ((window._lastStatus && window._lastStatus.acmmLevel) || 0) >= PLANNING_MIN_ACMM_LEVEL;
    }

    function dashboardRoleAtLeast(role, tier) {
      const rank = { 'read': 1, 'read-write': 2, 'merger': 3, 'owner': 4 };
      return (rank[role] || 0) >= (rank[tier] || 0);
    }

    // `mergeable` on an open PR is the tri-state string the Go side has sent
    // since #2365 — "yes", "no", or "" for not-yet-known — NOT a bool. A
    // truthiness test on the raw field reads "no" as eligible, which is
    // how ~90% of the pills on a hosted hive wore the green ✓ while GitHub
    // reported them DIRTY or BLOCKED (#7471). Every reader of the field goes
    // through here so the comparison cannot drift back to truthiness.
    function prMergeable(p) {
      return !!p && p.mergeable === 'yes';
    }

    // The pill's colour state (#7478). Green may mean one thing only: the
    // governor's merge-eligible classifier — the same rule that writes
    // merge-eligible.json for the sweep — put this PR in the eligible
    // bucket, so the sweep would merge it now. GitHub's own verdict is
    // looser: mergeable_state "unstable" is "no conflicts, nothing REQUIRED
    // red", which on a repo without branch protection includes red CI and no
    // review, and the sweep will not touch those. So:
    //   'eligible'    → green ✓   the sweep would merge this now
    //   'outstanding' → amber ◐   GitHub says mergeable; the sweep still
    //                             wants something (reason in the tooltip)
    //   ''            → no tint   dirty / blocked / draft / unknown
    // A payload without merge_verdict (an older governor, a replayed
    // snapshot) falls back to GitHub's flag — and lands on AMBER, never
    // green, because "the sweep would merge this" is exactly what such a PR
    // has not been shown to satisfy.
    function prMergeState(p) {
      const v = p && p.merge_verdict ? p.merge_verdict : null;
      if (v && typeof v.state === 'string') {
        if (v.state === 'eligible') return 'eligible';
        if (v.state === 'outstanding') return 'outstanding';
        return '';
      }
      return prMergeable(p) ? 'outstanding' : '';
    }

    // The tooltip fragment describing mergeability: the verdict's own reason
    // when the governor supplied one, else GitHub's raw mergeable_state. An
    // unknown verdict says so instead of saying nothing, and "no" names the
    // state (dirty, blocked, ...) that refused it.
    function prMergeNote(p) {
      const v = p && p.merge_verdict ? p.merge_verdict : null;
      const state = p && p.mergeable_state ? String(p.mergeable_state) : '';
      const reason = v && v.reason ? String(v.reason) : '';
      const tier = v && v.tier ? String(v.tier) : '';
      const tierNote = tier ? ' tier=' + tier + ': ' : ' ';
      switch (prMergeState(p)) {
        case 'eligible':
          return ' (merge eligible:' + tierNote + (reason || 'the sweep would merge this now') + ')';
        case 'outstanding':
          if (reason) return ' (mergeable on GitHub, not yet eligible: ' + reason + ')';
          if (state === 'unstable') return ' (mergeable on GitHub, sweep verdict not available — GitHub state: unstable; non-required checks may be red)';
          return ' (mergeable on GitHub, sweep verdict not available' + (state ? ' — GitHub state: ' + state : '') + ')';
      }
      if (reason) return ' (' + reason + ')';
      if (p && p.mergeable === 'no') return ' (not mergeable' + (state ? ' — GitHub state: ' + state : '') + ')';
      return ' (mergeability not yet known)';
    }

    async function queuePRAutoMerge(owner, repo, number, btn) {
      if (btn && btn.dataset.busy === '1') return;
      if (btn) { btn.dataset.busy = '1'; btn.textContent = '🔀'; }
      try {
        const resp = await fetch('/api/prs/' + encodeURIComponent(owner) + '/' + encodeURIComponent(repo) + '/' + number + '/queue-automerge', { method: 'POST' });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || ('HTTP ' + resp.status));
        showToast('Queued #' + number + ' for auto-merge when checks pass', 'success');
        if (btn) { btn.textContent = '🔀'; btn.disabled = true; btn.title = 'Queued for Hive auto-merge'; btn.setAttribute('aria-label', 'Queued for Hive auto-merge'); }
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
      } catch (e) {
        showToast('Queue failed: ' + (e && e.message ? e.message : 'network error'), 'error');
        if (btn) { btn.dataset.busy = ''; btn.textContent = '🔀'; }
      }
    }


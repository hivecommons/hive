    // ── Campaigns + Jam (Spek / inception campaign archive) ──
    // Moved verbatim out of inception.js so the Campaigns and Jam UI survives
    // the Inception wizard being replaced by the Sessions section. Plain
    // top-level declarations, shared with inception.js through the single
    // concatenated dashboard script; keep this file adjacent to it in manifest.txt.
    let _campaigns = [];
    let _campaignActionsInFlight = new Set();

    async function loadCampaigns() {
      try {
        const params = new URLSearchParams();
        ['search', 'repo', 'stage', 'status', 'owner'].forEach(function(name) {
          const el = document.getElementById('campaign-filter-' + name);
          if (el && el.value.trim()) params.set(name, el.value.trim());
        });
        const qs = params.toString();
        const r = await fetch('/api/campaigns' + (qs ? '?' + qs : ''), {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (data.ok) {
          _campaigns = data.campaigns || [];
          renderCampaignsList();
        }
      } catch (e) { console.error('campaign load failed', e); }
    }

    function renderCampaignsPanel() {
      return `
        <div class="campaigns-card">
          <div class="campaigns-head">
            <div><div class="campaigns-title">Campaigns</div><div class="campaigns-subtitle">Resumable Inception and Spek sessions keyed by stable spec ID.</div></div>
            <button class="hv-btn btn-secondary btn-sm" data-action="loadCampaigns">Refresh</button>
          </div>
          <div class="campaigns-filters">
            <input id="campaign-filter-search" class="campaigns-input" placeholder="Search campaigns" />
            <input id="campaign-filter-repo" class="campaigns-input" placeholder="Repo" />
            <input id="campaign-filter-stage" class="campaigns-input" placeholder="Stage" />
            <input id="campaign-filter-status" class="campaigns-input" placeholder="Status" />
            <input id="campaign-filter-owner" class="campaigns-input" placeholder="Owner" />
            <button class="hv-btn btn-secondary btn-sm" data-action="loadCampaigns">Apply</button>
          </div>
          <div id="campaigns-list" class="campaigns-list"><div class="campaign-empty">Loading campaigns…</div></div>
          <div id="campaign-jam-panel" class="campaign-jam"></div>
        </div>`;
    }

    // Revise chains (#12698): every Revise mints a linked `<root>-rev-N`
    // archive with the same title, so the API marks the older ones with
    // `superseded_by`. Show one row per chain (the newest revision) and fold
    // the earlier ones into a history disclosure under it.
    function campaignRevisionChains(campaigns) {
      const byId = {};
      (campaigns || []).forEach(function(c) { byId[c.id] = c; });
      const latest = (campaigns || []).filter(function(c) { return !c.superseded_by || !byId[c.superseded_by]; });
      return latest.map(function(head) {
        const earlier = [];
        const seen = {};
        let cur = head;
        while (cur && cur.revision_of && byId[cur.revision_of] && !seen[cur.revision_of]) {
          seen[cur.revision_of] = true;
          cur = byId[cur.revision_of];
          earlier.push(cur);
        }
        return { head: head, history: earlier };
      });
    }

    function campaignDeleteButton(c) {
      return nousExpIconButton('deleteCampaign', escapeHtml(c.id), '', 'Delete campaign', '🗑', 'nous-exp-icon-delete campaign-delete');
    }

    function renderCampaignRow(c, history) {
      const repo = (c.repos || []).join(', ') || 'no repo';
      const activity = c.activity_line || c.last_activity || 'no activity';
      const engine = c.engine || 'unknown engine';
      const logID = 'campaign-log-' + safeDomId(c.run_key || c.id);
      const detailID = 'campaign-detail-' + safeDomId(c.run_key || c.id);
      const canLog = !!c.run_key;
      const liveRun = canLog && Array.isArray(window._lastRuns) ? window._lastRuns.find(r => r && r.key === c.run_key) : null;
      const actionDisabled = _campaignActionsInFlight.has(c.id) ? ' disabled aria-busy="true"' : '';
      const interview = c.interview && c.run_key ? renderSpekInterviewInline({ key: c.run_key, waiting_reason: 'interview_questions', interview: c.interview }) : '';
      const rc = c.recheck || {};
      const recheckText = rc.enabled ? `Rechecks every ${escapeHtml(rc.interval || 'default')} · next ${escapeHtml(rc.next_at ? relativeAge(rc.next_at) : 'not scheduled')} · last delta ${Number(rc.last_delta_count || 0)}` : 'Rechecks off';
      const drift = c.drift || {};
      const external = Array.isArray(drift.external) ? drift.external : [];
      const failed = Array.isArray(rc.sources_failed) ? rc.sources_failed : (Array.isArray(drift.sources_failed) ? drift.sources_failed : []);
      const recheckExternal = external.length ? `<details class="campaign-row-meta"><summary>${Number(drift.external_count || external.length)} external signals</summary><div class="campaign-jam-list">${external.map(ev => `<div><strong>${escapeHtml(ev.source || '')}</strong> · ${escapeHtml(ev.kind || '')}: ${ev.url ? `<a href="${escapeHtml(ev.url)}" target="_blank" rel="noopener noreferrer">${escapeHtml(ev.title || ev.url)}</a>` : escapeHtml(ev.title || '')}${ev.summary ? `<br>${escapeHtml(ev.summary)}` : ''}</div>`).join('')}${failed.length ? `<div>Failed sources: ${failed.map(f => `${escapeHtml(f.name || '')} (${escapeHtml(f.reason || '')})`).join(', ')}</div>` : ''}</div></details>` : (Number(rc.external_count || 0) ? `<div class="campaign-row-meta"><span>${Number(rc.external_count || 0)} external signals</span></div>` : '');
      const recheckDisabled = rc.in_flight ? ' disabled' : '';
      const revisionPill = c.revision ? `<span class="campaign-pill campaign-revision-pill" title="Revision ${Number(c.revision)}${c.revision_of ? ' of ' + escapeHtml(c.revision_of) : ''}">rev ${Number(c.revision)}</span>` : '';
      const hist = Array.isArray(history) ? history : [];
      const historyBlock = hist.length ? `<details class="campaign-row-meta campaign-history"><summary>${hist.length} earlier ${hist.length === 1 ? 'revision' : 'revisions'}</summary><div class="campaign-history-list">${hist.map(function(h) {
        return `<div class="campaign-history-row" data-campaign-id="${escapeHtml(h.id)}"><span class="campaign-pill">${h.revision ? 'rev ' + Number(h.revision) : 'original'}</span><span>${escapeHtml(h.id)}</span><span>${escapeHtml(h.status || 'parked')}</span><span>${escapeHtml(h.last_activity || '')}</span><span class="campaign-row-actions">${nousExpIconButton('resumeCampaign', escapeHtml(h.id), '', 'Continue this revision', '▶', 'nous-exp-icon-resume')}${campaignDeleteButton(h)}</span></div>`;
      }).join('')}</div></details>` : '';
      return `<div class="campaign-row" data-campaign-id="${escapeHtml(c.id)}">
          <div>
            <div class="campaign-row-title">${canLog ? `<button class="clanker-link" data-action="toggleCampaignRunDetail" data-arg0="${escapeHtml(c.run_key)}" data-arg1="${escapeHtml(detailID)}">${escapeHtml(c.title || c.id)}</button>` : escapeHtml(c.title || c.id)}</div>
            <div class="campaign-row-meta">
              <span class="campaign-pill">${escapeHtml(c.status || 'parked')}</span>
              ${c.interview ? `<span class="campaign-pill">Spek has ${escapeHtml(String(c.interview.pending || 0))} questions for you</span>` : ''}
              ${revisionPill}
              <span>${escapeHtml(c.current_stage || '')}${c.current_step ? ' / ' + escapeHtml(c.current_step) : ''}</span>
              <span>${escapeHtml(repo)}</span>
              <span>${escapeHtml(engine)}</span>
              <span>${escapeHtml(activity)}</span>
              ${c.artifact_id ? `<span>${escapeHtml(c.artifact_id)}</span>` : ''}
              ${c.document_status ? `<span>${escapeHtml(c.document_status)}</span>` : ''}
            </div>
            ${liveRun ? renderRunActivity(liveRun) : ''}
            <div class="campaign-row-meta"><span>${recheckText}</span></div>
            ${recheckExternal}
            ${historyBlock}
          </div>
          <div class="campaign-row-actions">
            ${canLog ? `<button class="hv-btn btn-secondary btn-sm" data-action="toggleCampaignRunDetail" data-arg0="${escapeHtml(c.run_key)}" data-arg1="${escapeHtml(detailID)}">Details</button>` : ''}
            <button class="hv-btn btn-secondary btn-sm" data-action="openCampaignJam" data-arg0="${escapeHtml(c.id)}">Jam</button>
            ${canLog ? `<button class="hv-btn btn-secondary btn-sm" data-action="toggleRunLog" data-arg0="${escapeHtml(c.run_key)}" data-arg1="${escapeHtml(c.current_stage || 'spec')}" data-arg2="${escapeHtml(String(c.run_gen || 0))}" data-arg3="${escapeHtml(logID)}">Log</button>` : ''}
            <button class="hv-btn btn-secondary btn-sm" data-action="recheckCampaign" data-arg0="${escapeHtml(c.id)}"${recheckDisabled}>Recheck</button>
            <button class="hv-btn btn-primary btn-sm" data-action="resumeCampaign" data-arg0="${escapeHtml(c.id)}"${actionDisabled}>Continue</button>
            <button class="hv-btn btn-secondary btn-sm" data-action="releaseCampaign" data-arg0="${escapeHtml(c.id)}"${actionDisabled}>Release</button>
            <button class="hv-btn btn-secondary btn-sm" data-action="reviseCampaign" data-arg0="${escapeHtml(c.id)}"${actionDisabled}>Revise</button>
            ${campaignDeleteButton(c)}
          </div>
          ${interview}
          ${canLog ? `<div id="${escapeHtml(detailID)}" class="run-detail-panel" style="display:none"></div>` : ''}
          ${canLog ? `<div id="${escapeHtml(logID)}" class="run-log-panel"></div>` : ''}
        </div>`;
    }

    function renderCampaignsList() {
      const list = document.getElementById('campaigns-list');
      setSectionSummary('inception-section', inceptionSectionSummary());
      if (!list) return;
      if (!_campaigns || _campaigns.length === 0) {
        list.innerHTML = '<div class="campaign-empty">No campaigns yet. New Inception archives completed sessions here.</div>';
        return;
      }
      list.innerHTML = campaignRevisionChains(_campaigns).map(function(chain) { return renderCampaignRow(chain.head, chain.history); }).join('');
    }

    // Delete only removes the campaign record (saved inception state + wiki
    // snapshot). KB facts, Jam threads and scaffolded repo files are kept.
    // In-app hiveConfirm dialog; native browser dialogs are forbidden (#12698).
    const CAMPAIGN_DELETE_CONFIRM_TITLE = 'Delete this campaign?';
    function campaignDeleteConfirmMessage(c) {
      const label = (c && (c.title || c.id)) || 'this campaign';
      const rev = c && c.revision ? ` (revision ${Number(c.revision)})` : '';
      return `Removes the campaign record for "${label}"${rev}: its saved Inception state and the archived wiki snapshot. It disappears from this list and cannot be continued or revised.\n\nKept: knowledge-base facts, Jam threads, and anything already scaffolded or pushed to the repo. Other revisions in the same chain are not affected.`;
    }

    async function deleteCampaign(id) {
      if (!id) return;
      const c = (_campaigns || []).find(function(x) { return x.id === id; }) || { id: id };
      const ok = await hiveConfirm({ title: CAMPAIGN_DELETE_CONFIRM_TITLE, message: campaignDeleteConfirmMessage(c), confirmLabel: 'Delete campaign', cancelLabel: 'Keep', danger: true });
      if (!ok) return;
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id), { method: 'DELETE', headers: _inceptionAuthHeaders() });
        const data = await r.json().catch(function() { return {}; });
        if (!r.ok || !data.ok) { showToast(data.error || 'Failed to delete campaign', 'error'); return; }
        _campaigns = (_campaigns || []).filter(function(x) { return x.id !== id; });
        renderCampaignsList();
        showToast('Campaign deleted — knowledge-base facts and Jam threads kept', 'info');
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function toggleCampaignRunDetail(key, targetID) {
      const panel = document.getElementById(targetID || '');
      if (!panel || !key) return;
      if (panel.dataset.open === '1') {
        panel.dataset.open = '0';
        panel.style.display = 'none';
        panel.innerHTML = '';
        return;
      }
      panel.dataset.open = '1';
      panel.style.display = 'grid';
      panel.innerHTML = '<div class="campaign-empty">Loading run details…</div>';
      try {
        const res = await fetch('/api/runs/' + encodeURIComponent(key) + '/detail', {headers: _inceptionAuthHeaders()});
        const detail = await res.json();
        if (!res.ok || !detail.ok) throw new Error((detail && detail.error) || ('HTTP ' + res.status));
        panel.innerHTML = renderRunDetailPanel(detail);
      } catch (e) {
        panel.innerHTML = '<div class="campaign-empty">Run detail unavailable: ' + escapeHtml(e && e.message ? e.message : e) + '</div>';
      }
    }
    window.toggleCampaignRunDetail = toggleCampaignRunDetail;

    function renderRunDetailPanel(d) {
      const issue = d.issue || {}, run = d.run || {};
      const sourceKind = issue.source_type || (run.work_item && run.work_item.source_type) || 'github';
      // The badge prefers the configured external display_name (ADR-0020,
      // "Dashboard terminology", #10174) over the raw source_type key, which
      // for an external provider is its stable name ("acme"), not a label fit
      // for an operator to read.
      const sourceBadge = issue.source_display_name || sourceKind;
      const issueLabel = issue.repo && issue.number ? (issue.repo + '#' + issue.number) : (issue.external_id || (run.work_item && run.work_item.external_id) || issue.key || d.key || '');
      const issueLink = issue.url ? `<a href="${escapeHtml(issue.url)}" target="_blank" rel="noopener">${escapeHtml(issueLabel)}</a>` : escapeHtml(issueLabel);
      const repoLink = d.repo_url ? `<a href="${escapeHtml(d.repo_url)}" target="_blank" rel="noopener">${escapeHtml(run.repo || issue.repo || 'repo')}</a>` : escapeHtml(run.repo || issue.repo || 'repo');
      const plan = d.plan ? `<a href="${escapeHtml(d.plan.url || ('/api/plan/' + encodeURIComponent(d.plan.epic_id || '')))}" target="_blank" rel="noopener">${escapeHtml(d.plan.title || d.plan.epic_id || 'plan')}</a> <span class="campaign-pill">${escapeHtml(d.plan.status || (d.plan.approved ? 'approved' : ''))}</span>` : '<span class="text-muted">No plan epic recorded.</span>';
      const prs = (d.prs || []).length ? (d.prs || []).map(p => `<div><a href="${escapeHtml(p.url)}" target="_blank" rel="noopener">${escapeHtml(p.url)}</a> <span class="campaign-pill">${p.verified ? 'verified' : 'unverified'}</span>${p.reason ? ` <span class="text-muted">${escapeHtml(p.reason)}</span>` : ''}${p.model || p.backend ? ` <span class="text-muted">${escapeHtml([p.backend,p.model].filter(Boolean).join(' / '))}</span>` : ''}</div>`).join('') : '<span class="text-muted">No PR URL recorded for this run.</span>';
      const times = [['Admitted / stage start', run.stage_started_at], ['Completed', run.completed_at], ['Last activity', run.last_activity], ['Generated', d.generated_at]].filter(x => x[1]).map(x => `<div><strong>${escapeHtml(x[0])}:</strong> ${escapeHtml(x[1])}</div>`).join('') || '<span class="text-muted">No timestamps recorded.</span>';
      const stages = (d.stages || []).map(renderRunDetailStage).join('') || '<div class="text-muted">No stage timeline recorded.</div>';
      const notes = (d.notes || []).map(n => `<div class="campaign-empty" style="text-align:left;padding:var(--sp-3)">${escapeHtml(n)}</div>`).join('');
      return `<div class="run-detail-grid">
          <div class="run-detail-box"><h4>Source work item <span class="campaign-pill">${escapeHtml(sourceBadge)}</span></h4><div class="run-detail-linklist"><div>${issueLink}</div><div>${escapeHtml(issue.title || run.title || '(untitled)')}</div><div>${repoLink}</div></div></div>
          <div class="run-detail-box"><h4>Pull requests</h4><div class="run-detail-linklist">${prs}</div></div>
          <div class="run-detail-box"><h4>Plan / assignee</h4><div class="run-detail-linklist"><div>${plan}</div><div>${escapeHtml(run.assignee || 'unassigned')}${run.waiting_on ? ' · waiting on ' + escapeHtml(run.waiting_on) : ''}</div></div></div>
          <div class="run-detail-box"><h4>Timestamps</h4><div class="run-detail-linklist">${times}</div></div>
        </div>${notes}<div><h4>Stage timeline</h4>${stages}</div>
        <details class="run-stage-detail"><summary>Raw chronological log (${(d.events || []).length} events)</summary>
          <div class="run-log-actions"><button class="hv-btn btn-secondary btn-sm" data-action="copyRunRawLog" data-arg0="${escapeHtml(btoa(unescape(encodeURIComponent(d.raw_log || ''))))}">Copy raw log</button></div>
          <pre class="run-log-pre run-detail-raw">${escapeHtml(d.raw_log || '(empty)')}</pre>
        </details>`;
    }

    function renderRunDetailStage(st) {
      const meta = [st.status, st.gen ? 'gen ' + st.gen : '', st.actor, st.started_at, st.ended_at, st.duration].filter(Boolean).join(' · ');
      const receipt = st.receipt ? `<div class="text-muted">receipt: ${escapeHtml(st.receipt.digest || st.receipt.file || '')}</div>` : (st.receipt_sha ? `<div class="text-muted">receipt: ${escapeHtml(st.receipt_sha)}</div>` : '');
      const narrative = (st.narrative || []).length ? `<h4>What happened</h4><ol>${(st.narrative || []).map(n => `<li>${escapeHtml(n)}</li>`).join('')}</ol>` : '';
      const missing = (st.missing || []).map(n => `<div class="campaign-empty" style="text-align:left;padding:var(--sp-3)">${escapeHtml(n)}</div>`).join('');
      const interview = renderRunStageInterview(st);
      const docs = renderRunStageDocuments(st);
      const prompt = st.prompt && st.prompt.text ? renderRunTextDetails('Prompt sent to agent', st.prompt.text, st.prompt.truncated) : '';
      const agentBlock = (st.agent_transcript && st.agent_transcript.text) ? st.agent_transcript : ((st.agent_stdout_stderr && st.agent_stdout_stderr.text) ? st.agent_stdout_stderr : null);
      const agent = agentBlock ? renderRunTextDetails('Agent transcript', agentBlock.text, agentBlock.truncated, true) : '';
      const transcript = (st.transcript || []).length ? `<h4>Progress and relay events</h4><div class="run-transcript">${(st.transcript || []).map(renderRunTranscriptEntry).join('')}</div>` : '';
      const raw = st.receipt && st.receipt.raw ? `<details class="run-stage-detail"><summary>Raw receipt JSON</summary><pre class="run-log-pre run-detail-raw">${escapeHtml(JSON.stringify(st.receipt.raw, null, 2))}</pre></details>` : '';
      return `<details class="run-stage-detail"><summary>${escapeHtml(st.name || 'stage')} <span class="campaign-pill">${escapeHtml(meta || 'observed')}</span></summary>${receipt}${missing}${narrative}${interview}${docs}${prompt}${agent}${transcript}${raw}</details>`;
    }

    function renderRunStageInterview(st) {
      const rows = st.interview || [];
      if (!rows.length) return '<h4>Interview / Q&amp;A</h4><div class="text-muted">Interview Q&amp;A was not captured for this stage.</div>';
      return `<h4>Interview / Q&amp;A</h4><div class="run-transcript">${rows.map(function(q, i) {
        const title = q.step || ('Step ' + (i + 1));
        return `<div class="run-transcript-entry"><div><strong>${escapeHtml(title)}</strong>${q.answered_at ? ' <span class="text-muted">answered ' + escapeHtml(q.answered_at) + '</span>' : ''}</div><div><strong>Question:</strong> ${escapeHtml(q.question || '(not recorded)')}</div><div><strong>Answer:</strong><pre>${escapeHtml(q.answer || '(not recorded)')}</pre></div>${q.source ? '<div class="text-muted">source: ' + escapeHtml(q.source) + '</div>' : ''}</div>`;
      }).join('')}</div>`;
    }

    function renderRunStageDocuments(st) {
      const docs = st.documents || [];
      if (!docs.length) return '<h4>Final document</h4><div class="text-muted">Final spec/plan markdown was not captured for this stage.</div>';
      return docs.map(function(doc, i) {
        const title = (i === 0 ? 'Final document' : 'Additional document') + (doc.path ? ': ' + doc.path : '');
        return `<details class="run-stage-detail"><summary>${escapeHtml(title)}${doc.truncated ? ' (truncated)' : ''}</summary><div class="run-markdown">${renderSimpleMarkdown(doc.markdown || doc.content || '')}</div></details>`;
      }).join('');
    }

    function renderRunTextDetails(title, text, truncated, copyable) {
      const encoded = copyable ? escapeHtml(btoa(unescape(encodeURIComponent(text || '')))) : '';
      return `<details class="run-stage-detail"><summary>${escapeHtml(title)}${truncated ? ' (truncated)' : ''}</summary>${copyable ? `<div class="run-log-actions"><button class="hv-btn btn-secondary btn-sm" data-action="copyRunRawLog" data-arg0="${encoded}">Copy transcript</button></div>` : ''}<pre class="run-log-pre">${escapeHtml(text || '')}</pre></details>`;
    }

    function renderSimpleMarkdown(text) {
      const escLines = String(text || '').split('\n').map(escapeHtml);
      let html = '', inList = false, inCode = false, para = [];
      function flushPara() { if (para.length) { html += '<p>' + para.join('<br>') + '</p>'; para = []; } }
      function closeList() { if (inList) { html += '</ul>'; inList = false; } }
      escLines.forEach(function(line) {
        if (/^```/.test(line)) { flushPara(); closeList(); inCode = !inCode; html += inCode ? '<pre>' : '</pre>'; return; }
        if (inCode) { html += line + '\n'; return; }
        if (/^#{1,6}\s+/.test(line)) { flushPara(); closeList(); const lvl = Math.min((line.match(/^#+/) || [''])[0].length, 4); html += '<h' + lvl + '>' + line.replace(/^#{1,6}\s+/, '') + '</h' + lvl + '>'; return; }
        if (/^[-*]\s+/.test(line)) { flushPara(); if (!inList) { html += '<ul>'; inList = true; } html += '<li>' + line.replace(/^[-*]\s+/, '') + '</li>'; return; }
        if (!line.trim()) { flushPara(); closeList(); return; }
        para.push(line);
      });
      flushPara(); closeList(); if (inCode) html += '</pre>';
      return html || '<pre class="run-log-pre">(empty)</pre>';
    }

    function renderRunTranscriptEntry(t) {
      const fields = t.fields ? JSON.stringify(t.fields, null, 2) : '';
      return `<div class="run-transcript-entry"><div><strong>${escapeHtml(t.label || t.kind || 'event')}</strong>${t.at ? ' <span class="text-muted">' + escapeHtml(t.at) + '</span>' : ''}</div>${t.text ? `<pre>${escapeHtml(t.text)}</pre>` : ''}${fields ? `<pre>${escapeHtml(fields)}</pre>` : ''}</div>`;
    }

    function copyRunRawLog(encoded) {
      const text = decodeURIComponent(escape(atob(encoded || '')));
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(text).then(() => showToast('Run log copied.', 'success')).catch(() => showToast('Copy failed.', 'error'));
    }
    window.copyRunRawLog = copyRunRawLog;

    let _campaignJamSocket = null;
    let _campaignJamLiveRevision = '';

    async function openCampaignJam(id) {
      if (!id) return;
      const panel = document.getElementById('campaign-jam-panel');
      if (panel) panel.innerHTML = '<div class="campaign-empty">Loading Jam…</div>';
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/jam', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (!data.ok) { throw new Error(data.error || 'Failed to load Jam'); }
        renderCampaignJam(id, data.jam || {});
        connectCampaignJamLive(id);
      } catch (e) {
        if (panel) panel.innerHTML = '<div class="campaign-empty">Jam failed: ' + escapeHtml(e.message) + '</div>';
      }
    }

    function renderCampaignJam(id, jam) {
      const panel = document.getElementById('campaign-jam-panel');
      if (!panel) return;
      const threads = jam.threads || [];
      const suggestions = jam.suggestions || [];
      const polls = jam.polls || [];
      const revisions = jam.revisions || [];
      const projectSync = jam.project_sync || {};
      _campaignJamLiveRevision = jam.spec_revision_id || '';
      panel.innerHTML = `
        <div class="campaigns-head">
          <div><div class="campaigns-title">Jam: ${escapeHtml(id)}</div><div class="campaigns-subtitle">Async section threads, suggestions, polls, decisions and attributed spec revisions.</div></div>
          <button class="hv-btn btn-secondary btn-sm" data-action="openCampaignJam" data-arg0="${escapeHtml(id)}">Refresh Jam</button>
        </div>
        <div class="campaign-jam-box">
          <h4>Live session</h4>
          <div id="campaign-jam-live-status" class="campaign-jam-live">Connecting…</div>
          <div class="campaign-jam-form">
            <input id="jam-live-section" class="campaigns-input" placeholder="Section you are editing" />
            <textarea id="jam-live-content" class="campaigns-input" placeholder="Draft spec content for conflict-aware live edit">${escapeHtml(jam.spec_content || '')}</textarea>
            <div class="campaign-jam-actions">
              <button class="hv-btn btn-secondary btn-sm" data-action="focusCampaignJamLive" data-arg0="${escapeHtml(id)}">Share focus</button>
              <button class="hv-btn btn-primary btn-sm" data-action="submitCampaignJamLiveEdit" data-arg0="${escapeHtml(id)}">Submit live edit</button>
            </div>
          </div>
        </div>
        <div class="campaign-jam-grid">
          <div class="campaign-jam-box">
            <h4>Threads</h4>
            <div class="campaign-jam-list">${threads.length ? threads.map(t => `<div><strong>${escapeHtml(t.section || 'section')}</strong>: ${escapeHtml((t.comments && t.comments[0] && t.comments[0].body) || t.title || '')}<div class="campaign-jam-actions"><button class="hv-btn btn-secondary btn-sm" data-action="inviteCampaignJamAgent" data-arg0="${escapeHtml(id)}" data-arg1="${escapeHtml(t.id)}">Invite Spektacular</button></div></div>`).join('') : 'No threads yet.'}</div>
            <div class="campaign-jam-form">
              <input id="jam-thread-section" class="campaigns-input" placeholder="Spec section" />
              <textarea id="jam-thread-body" class="campaigns-input" placeholder="Comment"></textarea>
              <button class="hv-btn btn-primary btn-sm" data-action="createCampaignJamThread" data-arg0="${escapeHtml(id)}">Add thread</button>
            </div>
          </div>
          <div class="campaign-jam-box">
            <h4>Suggestions</h4>
            <div class="campaign-jam-list">${suggestions.length ? suggestions.map(s => `<div><strong>${escapeHtml(s.section || 'section')}</strong> · ${escapeHtml(s.status || 'open')}<br>${escapeHtml(s.proposed_text || '')}<div class="campaign-jam-actions">${s.status === 'open' ? `<button class="hv-btn btn-secondary btn-sm" data-action="resolveCampaignJamSuggestion" data-arg0="${escapeHtml(id)}" data-arg1="${escapeHtml(s.id)}" data-arg2="accept">Accept</button><button class="hv-btn btn-secondary btn-sm" data-action="resolveCampaignJamSuggestion" data-arg0="${escapeHtml(id)}" data-arg1="${escapeHtml(s.id)}" data-arg2="reject">Reject</button>` : ''}</div></div>`).join('') : 'No suggestions yet.'}</div>
            <div class="campaign-jam-form">
              <input id="jam-suggestion-section" class="campaigns-input" placeholder="Spec section" />
              <textarea id="jam-suggestion-text" class="campaigns-input" placeholder="Suggested text"></textarea>
              <button class="hv-btn btn-primary btn-sm" data-action="createCampaignJamSuggestion" data-arg0="${escapeHtml(id)}">Suggest text</button>
            </div>
          </div>
          <div class="campaign-jam-box">
            <h4>Polls & decisions</h4>
            <div class="campaign-jam-list">${polls.length ? polls.map(p => `<div><strong>${escapeHtml(p.question || '')}</strong><br>${(p.options || []).map(o => `${escapeHtml(o.label)}: ${o.count || 0}`).join(' · ')}${p.decision ? `<br>Decision: ${escapeHtml(p.decision.outcome)} — ${escapeHtml(p.decision.rationale)}` : ''}</div>`).join('') : 'No polls yet.'}</div>
            <div class="campaign-jam-form">
              <input id="jam-poll-section" class="campaigns-input" placeholder="Spec section" />
              <input id="jam-poll-question" class="campaigns-input" placeholder="Question" />
              <input id="jam-poll-options" class="campaigns-input" placeholder="Options separated by commas" />
              <button class="hv-btn btn-primary btn-sm" data-action="createCampaignJamPoll" data-arg0="${escapeHtml(id)}">Create poll</button>
            </div>
          </div>
        </div>
        <div class="campaign-jam-box">
          <h4>Revision history</h4>
          <div class="campaign-jam-list">${revisions.length ? revisions.map(r => `<div><strong>${escapeHtml(r.id)}</strong> by ${escapeHtml((r.author && r.author.name) || 'unknown')}${r.model ? ' · ' + escapeHtml(r.model) : ''}<br>${escapeHtml(r.reason || 'revision')}</div>`).join('') : 'No spec revisions yet.'}</div>
        </div>
        <div class="campaign-jam-box">
          <h4>GitHub Projects sync</h4>
          <div class="campaign-jam-list">
            <div>Status: ${escapeHtml(projectSync.last_status || (projectSync.enabled ? 'enabled' : 'disabled'))}</div>
            ${projectSync.project_url ? `<div>Project: ${escapeHtml(projectSync.project_url)}</div>` : ''}
            ${projectSync.last_error ? `<div>Last error: ${escapeHtml(projectSync.last_error)}<br>${escapeHtml(projectSync.retry_advice || 'Retry after checking the project link and token.')}</div>` : ''}
          </div>
          <div class="campaign-jam-form">
            <input id="jam-project-url" class="campaigns-input" placeholder="GitHub Projects URL or ID" value="${escapeHtml(projectSync.project_url || projectSync.project_id || '')}" />
            <div class="campaign-jam-actions">
              <button class="hv-btn btn-secondary btn-sm" data-action="enableCampaignJamProjectSync" data-arg0="${escapeHtml(id)}">Enable sync</button>
              <button class="hv-btn btn-primary btn-sm" data-action="syncCampaignJamProject" data-arg0="${escapeHtml(id)}">Sync now</button>
              <button class="hv-btn btn-secondary btn-sm" data-action="disableCampaignJamProjectSync" data-arg0="${escapeHtml(id)}">Disable</button>
            </div>
          </div>
        </div>`;
    }

    function connectCampaignJamLive(id) {
      if (_campaignJamSocket) {
        const prev = _campaignJamSocket;
        _campaignJamSocket = null;
        prev.onmessage = null;
        prev.onclose = null;
        prev.close();
      }
      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = protocol + '//' + window.location.host + '/api/campaigns/' + encodeURIComponent(id) + '/jam/ws';
      try {
        const ws = new WebSocket(url);
        _campaignJamSocket = ws;
        ws.onmessage = function(event) {
          const msg = JSON.parse(event.data || '{}');
          if (msg.jam && msg.jam.spec_revision_id) { _campaignJamLiveRevision = msg.jam.spec_revision_id; }
          if (msg.type === 'snapshot' || msg.type === 'presence') { renderCampaignJamPresence(msg.presence || []); }
          if (msg.type === 'edit_applied' && msg.jam) { renderCampaignJam(id, msg.jam); connectCampaignJamLive(id); }
          if (msg.type === 'conflict') { showToast('Jam edit conflict: refresh or merge against revision ' + (msg.expected_revision_id || 'current'), 'error'); }
          if (msg.type === 'error') { showToast('Jam live error: ' + (msg.error || 'unknown error'), 'error'); }
        };
        ws.onclose = function() { renderCampaignJamPresence([], 'Disconnected; refresh Jam to reconnect.'); };
      } catch (e) {
        renderCampaignJamPresence([], 'Live unavailable: ' + e.message);
      }
    }

    function renderCampaignJamPresence(presence, message) {
      const el = document.getElementById('campaign-jam-live-status');
      if (!el) return;
      if (message) { el.textContent = message; return; }
      el.innerHTML = presence.length ? presence.map(function(p) {
        const name = (p.actor && p.actor.name) || 'participant';
        return '<span class="campaign-jam-live-pill">' + escapeHtml(name) + (p.section ? ' · ' + escapeHtml(p.section) : '') + '</span>';
      }).join('') : 'No live participants yet.';
    }

    function focusCampaignJamLive() {
      if (!_campaignJamSocket || _campaignJamSocket.readyState !== WebSocket.OPEN) return;
      const section = document.getElementById('jam-live-section')?.value || '';
      _campaignJamSocket.send(JSON.stringify({ type: 'focus', section }));
    }

    function submitCampaignJamLiveEdit() {
      if (!_campaignJamSocket || _campaignJamSocket.readyState !== WebSocket.OPEN) { showToast('Jam live socket is not connected', 'error'); return; }
      const content = document.getElementById('jam-live-content')?.value || '';
      _campaignJamSocket.send(JSON.stringify({ type: 'edit', base_revision_id: _campaignJamLiveRevision, content, reason: 'live co-edit' }));
    }

    async function createCampaignJamThread(id) {
      const section = document.getElementById('jam-thread-section')?.value || '';
      const body = document.getElementById('jam-thread-body')?.value || '';
      await postCampaignJam(id, 'threads', { section, body });
    }

    async function inviteCampaignJamAgent(id, thread_id) {
      const prompt = await promptForJamAgent();
      if (prompt === null) return;
      await postCampaignJam(id, 'agents', { thread_id, agent: 'spektacular', prompt });
    }

    function promptForJamAgent() {
      return hivePrompt({title: 'Prompt for Jam agent', label: 'Prompt for the Jam agent reply and suggestion', defaultValue: 'Suggest spec text for this thread.'});
    }

    async function createCampaignJamSuggestion(id) {
      const section = document.getElementById('jam-suggestion-section')?.value || '';
      const proposed_text = document.getElementById('jam-suggestion-text')?.value || '';
      await postCampaignJam(id, 'suggestions', { section, proposed_text });
    }

    async function resolveCampaignJamSuggestion(id, suggestion_id, action) {
      await postCampaignJam(id, 'suggestions', { action, suggestion_id });
    }

    async function createCampaignJamPoll(id) {
      const section = document.getElementById('jam-poll-section')?.value || '';
      const question = document.getElementById('jam-poll-question')?.value || '';
      const options = (document.getElementById('jam-poll-options')?.value || '').split(',').map(s => s.trim()).filter(Boolean);
      await postCampaignJam(id, 'polls', { section, question, options });
    }

    async function enableCampaignJamProjectSync(id) {
      const value = document.getElementById('jam-project-url')?.value || '';
      const body = value.indexOf('http') === 0 ? { action: 'enable', project_url: value } : { action: 'enable', project_id: value };
      await postCampaignJam(id, 'project-sync', body);
    }

    async function syncCampaignJamProject(id) {
      await postCampaignJam(id, 'project-sync', { action: 'sync' });
    }

    async function disableCampaignJamProjectSync(id) {
      await postCampaignJam(id, 'project-sync', { action: 'disable' });
    }

    async function postCampaignJam(id, lane, body) {
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/jam/' + lane, { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify(body) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Jam update failed', 'error'); return; }
        renderCampaignJam(id, data.jam || {});
      } catch (e) { showToast('Jam error: ' + e.message, 'error'); }
    }

    async function resumeCampaign(id) {
      if (!id) return;
      if (_campaignActionsInFlight.has(id)) return;
      _campaignActionsInFlight.add(id);
      renderCampaignsList();
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/resume', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({ surface: 'dashboard' }) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Failed to resume campaign', 'error'); return; }
        if (data.state) { _inceptionState = data.state; renderInception(); }
        if (data.resume_command) { await hiveAlert('Resume command:\n' + data.resume_command, 'Resume command'); }
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
      finally { _campaignActionsInFlight.delete(id); renderCampaignsList(); }
    }

    async function releaseCampaign(id) {
      if (!id) return;
      if (_campaignActionsInFlight.has(id)) return;
      _campaignActionsInFlight.add(id);
      renderCampaignsList();
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/release', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({}) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Failed to release campaign', 'error'); return; }
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
      finally { _campaignActionsInFlight.delete(id); renderCampaignsList(); }
    }

    async function reviseCampaign(id) {
      if (!id) return;
      if (_campaignActionsInFlight.has(id)) return;
      _campaignActionsInFlight.add(id);
      renderCampaignsList();
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/revise', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({}) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Failed to revise campaign', 'error'); return; }
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
      finally { _campaignActionsInFlight.delete(id); renderCampaignsList(); }
    }

    async function recheckCampaign(id) {
      if (!id) return;
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/recheck', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({}) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Failed to recheck campaign', 'error'); return; }
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

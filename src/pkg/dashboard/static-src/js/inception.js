    // ── Inception section ──
    let _inceptionState = null;
    let _campaigns = [];
    let _campaignActionsInFlight = new Set();


    async function loadInceptionState() {
      try {
        const r = await fetch('/api/inception/state', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        _inceptionState = data.active ? data.state : null;
        renderInception();
        loadCampaigns();
      } catch (e) { console.error('inception state load failed', e); }
    }

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

    function renderInception() {
      refreshSectionCardShell('inception-section');
      setSectionSummary('inception-section', inceptionSectionSummary());
      const panel = document.getElementById('inception-panel');
      if (!panel) return;

      let wizard = '';
      if (!_inceptionState || !_inceptionState.phase) {
        wizard = renderInceptionModeSelect();
      } else {
        switch (_inceptionState.phase) {
          case 'capture':
            if (_inceptionState.idea_text) {
              wizard = renderInceptionProgress('idea') + renderInceptionWorking();
            } else {
              wizard = _inceptionState.mode === 'greenfield'
                ? renderInceptionCapture()
                : renderInceptionScanProgress();
            }
            break;
          case 'clarify':
            wizard = renderInceptionProgress('clarify') + renderInceptionClarify();
            break;
          case 'structure':
            wizard = renderInceptionProgress('structure') + renderInceptionStructure();
            break;
          case 'scaffold':
            wizard = renderInceptionProgress('scaffold') + renderInceptionScaffold();
            break;
          case 'complete':
            wizard = renderInceptionProgress('complete') + renderInceptionComplete();
            setTimeout(loadInceptionCompleteFiles, 100);
            break;
          default:
            wizard = renderInceptionModeSelect();
        }
      }
      // Rebuilding the panel resets its own scroll and the campaign filters;
      // carry both across so refreshes don't move the reader (#11032).
      const panelScrollTop = panel.scrollTop;
      const campaignFilters = {};
      panel.querySelectorAll('.campaigns-filters input').forEach(function(el) { campaignFilters[el.id] = el.value; });
      panel.innerHTML = '<div class="inception-spektacular-note">Project inception runs on <a href="https://github.com/jumppad-labs/spektacular" target="_blank" rel="noopener noreferrer">Spektacular</a>, a Hive Commons project.</div>' + renderCampaignsPanel() + '<div class="campaigns-card">' + wizard + '</div>';
      Object.keys(campaignFilters).forEach(function(id) {
        const el = document.getElementById(id);
        if (el) { el.defaultValue = campaignFilters[id]; el.value = campaignFilters[id]; }
      });
      panel.scrollTop = panelScrollTop;
      renderCampaignsList();
      if (!_inceptionPollTimer) startInceptionPoll();
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

    function renderInceptionProgress(phase) {
      var STAGES = [
        { id: 'idea', label: 'Idea', icon: '💡' },
        { id: 'clarify', label: 'Clarify', icon: '🔍' },
        { id: 'structure', label: 'Structure', icon: '📐' },
        { id: 'scaffold', label: 'Scaffold', icon: '🏗️' },
        { id: 'complete', label: 'Complete', icon: '✅' }
      ];

      var currentIndex = STAGES.findIndex(function(s) { return s.id === phase; });
      if (currentIndex === -1) { currentIndex = 0; }

      var isDark = document.body.classList.contains('dark') ||
        window.matchMedia('(prefers-color-scheme: dark)').matches ||
        (document.body.style.backgroundColor || '').indexOf('0d1117') !== -1;

      var COLORS = isDark
        ? { bg: 'var(--panel)', border: 'var(--line)', text: 'var(--text)', muted: 'var(--muted)', blue: 'var(--blue)', green: 'var(--green)', lineBg: 'var(--line)' }
        : { bg: '#ffffff', border: '#d0d7de', text: '#1f2328', muted: '#656d76', blue: '#0969da', green: '#1a7f37', lineBg: '#d0d7de' };

      var STAGE_COUNT = STAGES.length;
      var CONNECTOR_SEGMENTS = STAGE_COUNT - 1;

      var html = '';
      html += '<div style="';
      html += 'display:flex;align-items:flex-start;justify-content:center;';
      html += 'padding:var(--sp-6) var(--sp-8) var(--sp-5);';
      html += 'background:' + COLORS.bg + ';';
      html += 'border:1px solid ' + COLORS.border + ';';
      html += 'border-radius:8px;';
      html += 'font-family:var(--font-ui);';
      html += 'position:relative;';
      html += 'margin-bottom:var(--sp-6);';
      html += '">';

      for (var i = 0; i < STAGE_COUNT; i++) {
        var stage = STAGES[i];
        var isPast = i < currentIndex;
        var isCurrent = i === currentIndex;
        var isFuture = i > currentIndex;

        var circleSize = isCurrent ? '32px' : '28px';
        var circleBorder = isCurrent ? '3px' : '2px';
        var circleBg, circleColor, circleBorderColor;

        if (isPast) {
          circleBg = COLORS.green;
          circleColor = '#ffffff';
          circleBorderColor = COLORS.green;
        } else if (isCurrent) {
          circleBg = COLORS.blue;
          circleColor = '#ffffff';
          circleBorderColor = COLORS.blue;
        } else {
          circleBg = 'transparent';
          circleColor = COLORS.muted;
          circleBorderColor = COLORS.lineBg;
        }

        var labelColor = isCurrent ? COLORS.text : (isPast ? COLORS.green : COLORS.muted);
        var labelWeight = isCurrent ? '600' : '400';
        var labelSize = isCurrent ? '12px' : '11px';

        html += '<div style="';
        html += 'display:flex;flex-direction:column;align-items:center;';
        html += 'flex:1;position:relative;z-index:1;';
        html += '">';

        /* Circle */
        html += '<div style="';
        html += 'width:' + circleSize + ';height:' + circleSize + ';';
        html += 'border-radius:50%;';
        html += 'background:' + circleBg + ';';
        html += 'border:' + circleBorder + ' solid ' + circleBorderColor + ';';
        html += 'display:flex;align-items:center;justify-content:center;';
        html += 'font-size:' + (isCurrent ? '15px' : '13px') + ';';
        html += 'color:' + circleColor + ';';
        html += 'transition:all 0.3s ease;';
        html += 'box-shadow:' + (isCurrent ? '0 0 0 4px ' + COLORS.blue + '33' : 'none') + ';';
        html += '">';

        if (isPast) {
          html += '<svg class="d-block" width="14" height="14" viewBox="0 0 16 16" fill="none">';
          html += '<path d="M13.78 4.22a.75.75 0 010 1.06l-7.25 7.25a.75.75 0 01-1.06 0L2.22 9.28a.75.75 0 011.06-1.06L6 10.94l6.72-6.72a.75.75 0 011.06 0z" fill="#ffffff"/>';
          html += '</svg>';
        } else if (isCurrent) {
          html += '<span style="line-height:1;">' + stage.icon + '</span>';
        } else {
          html += '<span style="font-size:11px;color:' + COLORS.muted + ';">' + (i + 1) + '</span>';
        }

        html += '</div>';

        /* Label */
        html += '<div style="';
        html += 'margin-top:var(--sp-4);';
        html += 'font-size:' + labelSize + ';';
        html += 'font-weight:' + labelWeight + ';';
        html += 'color:' + labelColor + ';';
        html += 'text-align:center;';
        html += 'white-space:nowrap;';
        html += '">' + stage.label + '</div>';

        html += '</div>';

        /* Connector line between stages */
        if (i < CONNECTOR_SEGMENTS) {
          var lineColor = (i < currentIndex) ? COLORS.green : COLORS.lineBg;
          var lineHeight = '2px';

          html += '<div style="';
          html += 'flex:2;';
          html += 'height:' + lineHeight + ';';
          html += 'background:' + lineColor + ';';
          html += 'margin-top:' + (isCurrent ? '16px' : '14px') + ';';
          html += 'border-radius:1px;';
          html += 'align-self:flex-start;';
          html += 'transition:background 0.3s ease;';
          html += '">';

          /* Animated pulse on the active connector */
          if (i === currentIndex) {
            html += '<div style="';
            html += 'width:30%;height:100%;';
            html += 'background:linear-gradient(90deg,transparent,' + COLORS.blue + ',transparent);';
            html += 'border-radius:1px;';
            html += 'animation:inceptionPulse 2s ease-in-out infinite;';
            html += '"></div>';
          }

          html += '</div>';
        }
      }

      /* spec-kit attribution */
      html += '<div style="text-align:center;margin-top:var(--sp-2);margin-bottom:-6px;">';
      html += '<span style="font-size:10px;color:' + COLORS.muted + ';letter-spacing:0.3px;">';
      html += 'Powered by <a href="https://github.com/github/spec-kit" target="_blank" rel="noopener" style="color:' + COLORS.muted + ';text-decoration:underline;text-underline-offset:2px;">GitHub Spec Kit</a>';
      html += ' &middot; Spec-Driven Development';
      html += '</span></div>';

      html += '</div>';

      /* Inject keyframes if not already present */
      if (!document.getElementById('inception-progress-styles')) {
        var styleEl = document.createElement('style');
        styleEl.id = 'inception-progress-styles';
        styleEl.textContent = '@keyframes inceptionPulse{0%{transform:translateX(0)}50%{transform:translateX(233%)}100%{transform:translateX(0)}}';
        document.head.appendChild(styleEl);
      }

      if (_inceptionState && _inceptionState.idea_text) {
        var ideaText = escapeHtml(_inceptionState.idea_text);
        html += '<div style="';
        html += 'margin-top:var(--sp-5);margin-bottom:var(--sp-6);padding:10px 14px;';
        html += 'background:' + (isDark ? '#1c2333' : '#f6f8fa') + ';';
        html += 'border:1px solid ' + COLORS.border + ';';
        html += 'border-radius:var(--r);';
        html += 'font-size:13px;color:' + COLORS.text + ';';
        html += 'line-height:1.4;';
        html += '">';
        html += '<span style="color:' + COLORS.muted + ';font-size:11px;text-transform:uppercase;letter-spacing:0.5px;">Idea</span><br>';
        html += ideaText;
        html += '</div>';
      }

      return html;
    }

    function renderInceptionModeSelect() {
      return `
        <div style="max-width:700px;margin:0 auto 16px">${emptyStateCard('💡', 'Project Inception — bootstrap a project with your hive',
          'Inception takes a project from raw idea to structured knowledge and a scaffolded repo. ' +
          'It runs with Spektacular\'s spec-driven path: enable it from Settings → Extensions → Spektacular, or read the <a href="https://github.com/hivecommons/hive/blob/v6/src/docs/spektacular.md#configuration" target="_blank" rel="noopener noreferrer">Spektacular docs</a>. ' +
          'In greenfield mode a brainstorm agent captures your one-line idea, asks clarifying questions, structures the answers into vision, requirements, and constraints (stored as Knowledge Base facts), then scaffolds the project. ' +
          'In brownfield mode it scans an existing repo and captures its conventions and principles as KB facts instead. Pick a mode below to begin.')}</div>
        <div style="display:flex;gap:var(--sp-6);max-width:700px;margin:0 auto">
          <div data-action="startInceptionGreenfield" style="flex:1;background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-8);cursor:pointer;text-align:center;transition:border-color 0.2s" data-mouseover-action="gh20" data-mouseout-action="gh21">
            <div class="fs-3xl mb-4">🌱</div>
            <div style="font-weight:600;margin-bottom:var(--sp-2)">Start from an idea</div>
            <div style="font-size:0.78rem;color:var(--muted)">New project — describe what you want to build and we'll scaffold it</div>
          </div>
          <div data-action="startInceptionBrownfield" style="flex:1;background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-8);cursor:pointer;text-align:center;transition:border-color 0.2s" data-mouseover-action="gh20" data-mouseout-action="gh21">
            <div class="fs-3xl mb-4">🏗️</div>
            <div style="font-weight:600;margin-bottom:var(--sp-2)">Add to existing repo</div>
            <div style="font-size:0.78rem;color:var(--muted)">Scan an existing project and capture its principles as KB facts</div>
          </div>
        </div>`;
    }

    function renderInceptionCapture() {
      return `
        <div style="max-width:600px;margin:0 auto">
          <div style="font-size:0.85rem;color:var(--muted);margin-bottom:var(--sp-5)">Describe your project idea in a sentence or two.</div>
          <textarea id="inception-idea" placeholder="A CLI tool that syncs Obsidian vaults to a wiki server..." style="width:100%;min-height:80px;padding:var(--sp-5);border:1px solid var(--border);border-radius:8px;background:var(--card-bg);color:var(--fg);font-family:inherit;font-size:0.85rem;resize:vertical"></textarea>
          <div style="margin-top:var(--sp-5);display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="stopInception">Stop</button>
            <button class="hv-btn btn-danger" data-action="clearInception">Clear</button>
            <button class="hv-btn btn-primary" data-action="submitInceptionIdea">Start</button>
          </div>
        </div>`;
    }

    let _inceptionPollTimer = null;
    let _inceptionTailing = true;

    function onInceptionScroll() {
      const el = document.getElementById('inception-activity');
      const btn = document.getElementById('inception-follow-btn');
      if (!el) return;
      const SCROLL_THRESHOLD = 40;
      const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < SCROLL_THRESHOLD;
      _inceptionTailing = atBottom;
      if (btn) btn.classList.toggle('visible', !atBottom);
    }

    function inceptionResumeTail() {
      _inceptionTailing = true;
      const el = document.getElementById('inception-activity');
      if (el) el.scrollTop = el.scrollHeight;
      const btn = document.getElementById('inception-follow-btn');
      if (btn) btn.classList.remove('visible');
    }

    function renderInceptionWorking() {
      const phase = escapeHtml(_inceptionState.phase || 'capture');
      return `
        <div style="max-width:100%;margin:var(--sp-0)">
          <div style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-5)">
            <div style="width:8px;height:8px;border-radius:50%;background:var(--blue);animation:pulse 1.5s infinite"></div>
            <div style="font-size:var(--fs-base);font-weight:600">Brainstorm agent working</div>
            <div style="font-size:0.72rem;color:var(--muted);margin-left:auto">Phase: ${phase}</div>
          </div>
          <div style="position:relative">
          <div id="inception-activity" style="padding:var(--sp-6);font-size:var(--fs-sm);height:450px;overflow-y:auto;white-space:pre-wrap;resize:vertical;min-height:150px;max-height:calc(100vh - 200px)" data-scroll-action="onInceptionScroll">Waiting for brainstorm agent...</div>
          <button id="inception-follow-btn" class="oc-summary-follow-btn" data-action="inceptionResumeTail" title="Follow new output">⬇</button>
          </div>
          <div style="margin-top:var(--sp-5);display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="stopInception">Stop</button>
            <button class="hv-btn btn-danger" data-action="clearInception">Clear</button>
          </div>
        </div>`;
    }

    function startInceptionPoll() {
      stopInceptionPoll();
      _inceptionTailing = true;
      pollInceptionActivity();
      const INCEPTION_POLL_INTERVAL_MS = 3000;
      _inceptionPollTimer = dashboardSetInterval('inception-activity', pollInceptionActivity, INCEPTION_POLL_INTERVAL_MS);
    }

    function stopInceptionPoll() {
      if (_inceptionPollTimer) { dashboardClearInterval('inception-activity'); _inceptionPollTimer = null; }
    }

    function _inceptionAuthHeaders() {
      var h = {'Content-Type': 'application/json'};
      var t = localStorage.getItem('hive-token');
      if (t) h['Authorization'] = 'Bearer ' + t;
      return h;
    }

    function _inceptionStateKey(s) {
      if (!s) return 'inactive';
      return (s.idea_slug || '') + ':' + (s.phase || '') + ':' + ((s.proposed_facts || []).length);
    }

    async function pollInceptionActivity() {
      try {
        const r = await fetch('/api/pane/brainstorm?lines=500&source=buffer', {headers: _inceptionAuthHeaders()});
        if (!r.ok) return;
        const data = await r.json();
        const lines = data.lines || [];
        const el = document.getElementById('inception-activity');
        if (el) {
          el.textContent = lines.join('\n') || 'Waiting for brainstorm agent...';
          if (_inceptionTailing) el.scrollTop = el.scrollHeight;
        }
      } catch (e) { /* ignore poll errors */ }

      try {
        // Don't clobber the textarea while the user is typing their idea, nor
        // any panel field that is focused or holds unsaved input (#11032);
        // the next poll retries once the user is done.
        if (_inceptionState && _inceptionState.phase === 'capture' && !_inceptionState.idea_text) {
          return;
        }
        if (sectionHoldsUserInput(document.getElementById('inception-panel'), '.campaigns-filters input')) {
          return;
        }
        const sr = await fetch('/api/inception/state', {headers: _inceptionAuthHeaders()});
        const sd = await sr.json();
        if (!sd.ok) return;
        var oldKey = _inceptionStateKey(_inceptionState);
        var newKey = _inceptionStateKey(sd.state);
        if (oldKey !== newKey) {
          _inceptionState = sd.state || {};
          renderInception();
          if (typeof renderKnowledge === 'function') renderKnowledge();
        }
      } catch (e) { /* ignore */ }
    }

    function renderInceptionScanProgress() {
      return `
        <div style="max-width:600px;margin:0 auto;text-align:center;padding:40px 0">
          <div style="font-size:1.2rem;margin-bottom:var(--sp-5)">🔍</div>
          <div style="font-size:0.85rem;color:var(--muted)">Scanning project...</div>
          <div style="font-size:0.78rem;color:var(--muted);margin-top:var(--sp-2)">${escapeHtml(_inceptionState.repo_url || '')}</div>
          <div class="mt-6"><button class="hv-btn btn-danger btn-sm" data-action="resetInception">Cancel</button></div>
        </div>`;
    }

    function renderInceptionClarify() {
      const questions = _inceptionState.questions || [];
      let html = '<div style="max-width:600px;margin:0 auto">';
      html += '<div style="font-size:0.85rem;color:var(--muted);margin-bottom:var(--sp-6)">Answer these questions to refine your project definition.</div>';
      questions.forEach(q => {
        html += `<div class="mb-6">
          <label style="font-size:var(--fs-base);font-weight:600;display:block;margin-bottom:var(--sp-2)">${escapeHtml(q.text)}</label>
          <input type="text" id="inception-q-${q.id}" value="${escapeHtml(q.default || '')}"
            style="width:100%;padding:var(--sp-4) var(--sp-5);border:1px solid var(--border);border-radius:var(--r);background:var(--card-bg);color:var(--fg);font-size:var(--fs-base)" />
          <div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-1)">${escapeHtml(q.category || '')}</div>
        </div>`;
      });
      html += `<div style="display:flex;gap:var(--sp-4);justify-content:flex-end">
        <button class="hv-btn btn-danger" data-action="resetInception">Reset</button>
        <button class="hv-btn btn-primary" data-action="submitInceptionAnswers">Submit</button>
      </div>`;
      html += '</div>';
      return html;
    }

    function renderInceptionStructure() {
      return `
        <div style="max-width:100%;margin:var(--sp-0)">
          <div style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-5)">
            <div style="width:8px;height:8px;border-radius:50%;background:var(--blue);animation:pulse 1.5s infinite"></div>
            <div style="font-size:var(--fs-base);font-weight:600">Brainstorm agent structuring project</div>
            <div style="font-size:0.72rem;color:var(--muted);margin-left:auto">Phase: structure</div>
          </div>
          <div style="position:relative">
          <div id="inception-activity" style="padding:var(--sp-6);font-size:var(--fs-sm);height:450px;overflow-y:auto;white-space:pre-wrap;resize:vertical;min-height:150px;max-height:calc(100vh - 200px)" data-scroll-action="onInceptionScroll">Brainstorm agent is producing vision, constitution, requirements, and acceptance criteria...</div>
          <button id="inception-follow-btn" class="oc-summary-follow-btn" data-action="inceptionResumeTail" title="Follow new output">⬇</button>
          </div>
          ${renderInceptionProposedFacts()}
          <div style="margin-top:var(--sp-5);display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="stopInception">Stop</button>
            <button class="hv-btn btn-danger" data-action="clearInception">Clear</button>
          </div>
        </div>`;
    }

    function renderInceptionProposedFacts() {
      const facts = _inceptionState && _inceptionState.proposed_facts ? _inceptionState.proposed_facts : [];
      if (!facts.length) return '';
      let html = '<div style="margin-top:12px;background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:12px">';
      html += '<div style="font-size:0.84rem;font-weight:600;margin-bottom:6px">Transcript proposals need confirmation</div>';
      html += '<div style="font-size:0.74rem;color:var(--muted);margin-bottom:10px">Confirm only the facts that should enter the inception KB. Rejected proposals are discarded.</div>';
      facts.forEach((f, i) => {
        html += '<label style="display:block;border-top:1px solid var(--border);padding:8px 0;font-size:0.78rem">';
        html += '<input type="checkbox" class="inception-proposed-fact" value="' + i + '" style="margin-right:8px">';
        html += '<strong>[' + escapeHtml(f.type || '') + '] ' + escapeHtml(f.title || '') + '</strong>';
        if (f.source_ref) html += '<span style="color:var(--muted);margin-left:6px">' + escapeHtml(f.source_ref) + '</span>';
        html += '<div style="margin-left:24px;color:var(--muted);white-space:pre-wrap">' + escapeHtml(f.body || '') + '</div>';
        html += '</label>';
      });
      html += '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:8px">';
      html += '<button data-action="submitConfirmedInceptionFacts" style="padding:7px 14px;border:none;border-radius:6px;background:var(--blue);color:var(--bg);cursor:pointer;font-size:0.78rem;font-weight:600">Record confirmed facts</button>';
      html += '</div></div>';
      return html;
    }

    function renderInceptionScaffold() {
      setTimeout(loadInceptionScaffoldSummary, 100);
      return `
        <div style="max-width:700px;margin:0 auto">
          <div style="font-size:0.85rem;color:var(--muted);margin-bottom:var(--sp-5)">Your scaffold is ready. Review the generated files below, then approve to complete.</div>
          <div class="mb-6" id="inception-scaffold-summary"><div style="text-align:center;color:var(--muted);padding:var(--sp-7)">Loading scaffold summary...</div></div>
          <div style="display:flex;gap:var(--sp-4);justify-content:flex-end">
            <button class="hv-btn btn-danger" data-action="resetInception">Start Over</button>
            <button class="hv-btn btn-primary" data-action="approveInception">Approve</button>
          </div>
        </div>`;
    }

    async function loadInceptionScaffoldSummary() {
      const container = document.getElementById('inception-scaffold-summary');
      if (!container) return;
      try {
        const r = await fetch('/api/inception/scaffold', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (!data.ok || !data.scaffold || !data.scaffold.files || data.scaffold.files.length === 0) {
          container.innerHTML = '<div class="text-muted">No scaffold files generated.</div>';
          return;
        }
        const files = data.scaffold.files;
        const dirs = new Set();
        const exts = {};
        files.forEach(f => {
          const parts = f.path.split('/');
          if (parts.length > 1) dirs.add(parts[0]);
          const ext = f.path.includes('.') ? f.path.split('.').pop() : 'other';
          exts[ext] = (exts[ext] || 0) + 1;
        });
        let html = '<div style="background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-6)">';
        html += '<div style="font-size:0.9rem;font-weight:600;margin-bottom:var(--sp-5)">📦 Scaffold Summary</div>';
        html += '<div style="display:flex;gap:var(--sp-8);flex-wrap:wrap;margin-bottom:var(--sp-5)">';
        html += '<div><span style="font-size:1.2rem;font-weight:700">' + files.length + '</span> <span style="color:var(--muted);font-size:0.8rem">files</span></div>';
        html += '<div><span style="font-size:1.2rem;font-weight:700">' + dirs.size + '</span> <span style="color:var(--muted);font-size:0.8rem">directories</span></div>';
        const totalChars = files.reduce((sum, f) => sum + f.content.length, 0);
        html += '<div><span style="font-size:1.2rem;font-weight:700">' + (totalChars > 1000 ? (totalChars/1000).toFixed(1)+'K' : totalChars) + '</span> <span style="color:var(--muted);font-size:0.8rem">chars</span></div>';
        html += '</div>';
        html += '<div style="display:flex;gap:var(--sp-3);flex-wrap:wrap;margin-bottom:var(--sp-5)">';
        Object.entries(exts).sort((a,b) => b[1]-a[1]).forEach(([ext,count]) => {
          html += '<span style="padding:var(--sp-1) var(--sp-4);background:var(--bg);border:1px solid var(--border);border-radius:var(--r-lg);font-size:0.72rem;color:var(--muted)">.' + escapeHtml(ext) + ' ×' + count + '</span>';
        });
        html += '</div>';
        html += '<details><summary style="cursor:pointer;font-size:0.8rem;color:var(--blue)">View file tree</summary>';
        html += '<div class="mt-4">' + renderFileTree(files) + '</div>';
        html += '</details></div>';
        container.innerHTML = html;
      } catch (e) { container.innerHTML = '<div style="color:var(--error)">Error: ' + escapeHtml(e.message) + '</div>'; }
    }

    function renderInceptionComplete() {
      const factCount = (_inceptionState.fact_slugs || []).length;
      return `
        <div style="max-width:100%;margin:var(--sp-0)">
          <div style="text-align:center;margin-bottom:var(--sp-7)">
            <div style="font-size:1rem;font-weight:600;margin-bottom:var(--sp-2)">Inception Complete</div>
            <div style="font-size:0.85rem;color:var(--muted)">${factCount} knowledge facts created. Your project is ready to grow.</div>
          </div>
          <div class="mb-6" id="inception-complete-files"></div>
          <div style="display:flex;gap:var(--sp-4);justify-content:center">
            <button class="hv-btn btn-danger" data-action="resetInception">New Inception</button>
            <button class="hv-btn btn-primary" data-action="downloadInceptionZip">Download Project</button>
            <button class="hv-btn btn-secondary btn-sm" data-action="loadInceptionCompleteFiles">Refresh Files</button>
          </div>
        </div>`;
    }

    async function loadInceptionCompleteFiles() {
      const container = document.getElementById('inception-complete-files');
      if (!container) return;
      container.innerHTML = '<div style="color:var(--muted);text-align:center;padding:var(--sp-5)">Loading scaffold files...</div>';
      try {
        const r = await fetch('/api/inception/scaffold', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (!data.ok || !data.scaffold || !data.scaffold.files || data.scaffold.files.length === 0) {
          container.innerHTML = '<div style="color:var(--muted);text-align:center;padding:var(--sp-5)">No scaffold files generated. The brainstorm agent may not have produced KB facts.</div>';
          return;
        }
        container.innerHTML = renderFileTree(data.scaffold.files);
      } catch (e) {
        container.innerHTML = '<div style="color:var(--muted);text-align:center;padding:var(--sp-5)">Error loading files: ' + escapeHtml(e.message) + '</div>';
      }
    }

    function renderFileTree(files) {
      // Build tree structure from flat paths
      const root = {};
      for (const f of files) {
        const parts = f.path.split('/');
        let node = root;
        for (let i = 0; i < parts.length; i++) {
          const part = parts[i];
          if (i === parts.length - 1) {
            node[part] = { _file: f };
          } else {
            if (!node[part]) node[part] = {};
            node = node[part];
          }
        }
      }

      function renderNode(node, prefix, isLast) {
        let html = '';
        const entries = Object.keys(node).sort((a, b) => {
          const aIsDir = !node[a]._file;
          const bIsDir = !node[b]._file;
          if (aIsDir !== bIsDir) return aIsDir ? -1 : 1;
          return a.localeCompare(b);
        });

        entries.forEach((name, idx) => {
          const isLastEntry = idx === entries.length - 1;
          const connector = isLastEntry ? '└── ' : '├── ';
          const childPrefix = prefix + (isLastEntry ? '    ' : '│   ');
          const entry = node[name];

          if (entry._file) {
            const f = entry._file;
            html += '<details style="margin:var(--sp-0);border:none">';
            html += '<summary style="padding:var(--sp-1) var(--sp-0);cursor:pointer;font-family:var(--font-mono);font-size:0.8rem;list-style:none;display:flex;align-items:center;gap:var(--sp-2)">';
            html += '<span style="color:var(--muted);white-space:pre">' + escapeHtml(prefix + connector) + '</span>';
            html += '<span>📄 ' + escapeHtml(name) + '</span>';
            html += '<span style="font-size:var(--fs-xs);color:var(--muted);margin-left:var(--sp-2)">' + f.content.length + ' chars</span>';
            html += '</summary>';
            html += '<pre style="padding:12px 12px 12px 40px;margin:var(--sp-0);font-size:0.72rem;overflow-x:auto;background:var(--bg);white-space:pre-wrap;border-left:2px solid var(--border);margin-left:' + (prefix.length * 8 + 24) + 'px">' + escapeHtml(f.content) + '</pre>';
            html += '</details>';
          } else {
            html += '<div style="padding:var(--sp-1) var(--sp-0);font-family:var(--font-mono);font-size:0.8rem;display:flex;align-items:center;gap:var(--sp-2)">';
            html += '<span style="color:var(--muted);white-space:pre">' + escapeHtml(prefix + connector) + '</span>';
            html += '<span>📁 <strong>' + escapeHtml(name) + '/</strong></span>';
            html += '</div>';
            html += renderNode(entry, childPrefix, isLastEntry);
          }
        });
        return html;
      }

      let html = '<div style="background:var(--card-bg);border:1px solid var(--border);border-radius:8px;padding:var(--sp-6);font-family:var(--font-mono)">';
      html += '<div style="font-size:var(--fs-base);font-weight:600;margin-bottom:var(--sp-4)">📦 Project Files (' + files.length + ')</div>';
      html += renderNode(root, '', true);
      html += '</div>';
      return html;
    }

    function startInceptionGreenfield() {
      _inceptionState = { phase: 'capture', mode: 'greenfield' };
      renderInception();
    }

    async function startInceptionBrownfield() {
      const url = await hivePrompt({title: 'Add to existing repo', label: 'Enter the GitHub repo URL:', placeholder: 'https://github.com/owner/repo'});
      if (!url) return;
      try {
        const r = await fetch('/api/inception/scan', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ repo_url: url })
        });
        const data = await r.json();
        if (data.ok) { _inceptionState = data.state; renderInception(); loadCampaigns(); }
        else showToast(data.error || 'Failed to start scan', 'error');
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function submitInceptionIdea() {
      const el = document.getElementById('inception-idea');
      const idea = el ? el.value.trim() : '';
      if (!idea) { showToast('Please describe your idea', 'error'); return; }
      try {
        const r = await fetch('/api/inception/start', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ idea })
        });
        const data = await r.json();
        if (data.ok) { _inceptionState = data.state; renderInception(); loadCampaigns(); }
        else showToast(data.error || 'Failed to start inception', 'error');
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function submitInceptionAnswers() {
      const questions = _inceptionState.questions || [];
      const answers = {};
      questions.forEach(q => {
        const el = document.getElementById('inception-q-' + q.id);
        if (el) answers[q.id] = el.value.trim();
      });
      const btn = document.querySelector('#inception-panel button[onclick*="submitInceptionAnswers"]');
      if (btn) { btn.disabled = true; btn.textContent = 'Submitting...'; btn.style.opacity = '0.6'; }
      try {
        const r = await fetch('/api/inception/answer', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ answers })
        });
        const data = await r.json();
        if (data.ok) { _inceptionState = data.state; renderInception(); }
        else { showToast(data.error || 'Failed to submit answers', 'error'); if (btn) { btn.disabled = false; btn.textContent = 'Submit'; btn.style.opacity = '1'; } }
      } catch (e) { showToast('Error: ' + e.message, 'error'); if (btn) { btn.disabled = false; btn.textContent = 'Submit'; btn.style.opacity = '1'; } }
    }

    async function loadInceptionScaffoldPreview() {
      const preview = document.getElementById('inception-scaffold-preview');
      if (!preview) return;
      preview.innerHTML = '<div class="text-muted fs-base">Loading...</div>';
      try {
        const r = await fetch('/api/inception/scaffold', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (!data.ok || !data.scaffold) { preview.innerHTML = '<div class="text-muted">No scaffold available yet.</div>'; return; }
        preview.innerHTML = renderFileTree(data.scaffold.files || []);
      } catch (e) { preview.innerHTML = '<div style="color:var(--error)">Error: ' + escapeHtml(e.message) + '</div>'; }
    }

    async function loadInceptionIdeationFacts() {
      try {
        const r = await fetch('/api/inception/ideation-facts', {headers: _inceptionAuthHeaders()});
        const data = await r.json();
        if (data.ok && data.facts && data.facts.length > 0) {
          let msg = 'Ideation Facts (' + data.facts.length + '):\n\n';
          data.facts.forEach(f => { msg += '- [' + f.type + '] ' + f.title + '\n'; });
          await hiveAlert(msg, 'Ideation Facts');
        } else {
          showToast('No ideation facts yet. The brainstorm agent is still working.', 'info');
        }
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function submitConfirmedInceptionFacts() {
      const facts = _inceptionState && _inceptionState.proposed_facts ? _inceptionState.proposed_facts : [];
      const picked = Array.from(document.querySelectorAll('.inception-proposed-fact:checked')).map(el => {
        const f = Object.assign({}, facts[Number(el.value)]);
        f.confirmed = true;
        return f;
      }).filter(Boolean);
      if (!picked.length) { showToast('Select at least one proposal to confirm.', 'error'); return; }
      try {
        const r = await fetch('/api/inception/facts', {
          method: 'POST',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ facts: picked })
        });
        const data = await r.json();
        if (data.ok) { _inceptionState.phase = 'scaffold'; _inceptionState.proposed_facts = []; renderInception(); }
        else showToast(data.error || 'Failed to record confirmed facts', 'error');
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function approveInception() {
      try {
        const r = await fetch('/api/inception/approve', { method: 'POST', headers: _inceptionAuthHeaders() });
        const data = await r.json();
        if (data.ok) { _inceptionState.phase = 'complete'; renderInception(); }
        else showToast(data.error || 'Failed to approve', 'error');
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function stopInception() {
      stopInceptionPoll();
      try {
        await fetch('/api/pause/brainstorm', { method: 'POST' });
      } catch (e) { /* ignore */ }
      await loadInceptionState();
    }

    async function clearInception() {
      stopInceptionPoll();
      try {
        const hasFilesResp = await fetch('/api/inception/has-files', {headers: _inceptionAuthHeaders()});
        const hasFilesData = await hasFilesResp.json();
        if (hasFilesData.has_files) {
          const choice = await hiveConfirm({title: 'Download scaffold?', message: 'Download existing scaffold before clearing?', confirmLabel: 'Download'});
          if (choice) {
            await downloadInceptionZip();
          }
        }
        await fetch('/api/inception/reset', { method: 'POST', headers: _inceptionAuthHeaders() });
        _inceptionState = null;
        renderInception();
        loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function resetInception() {
      await clearInception();
    }

    async function downloadInceptionZip() {
      try {
        const r = await fetch('/api/inception/download', {headers: _inceptionAuthHeaders()});
        if (!r.ok) { showToast('Download failed', 'error'); return; }
        hiveBinaryExportNotice('Inception project ZIP', 'Inception scaffold ZIP files are binary archives.');
      } catch (e) { showToast('Download error: ' + e.message, 'error'); }
    }

    // Load inception state on section navigate
    const _origOcNavigate = typeof ocNavigate === 'function' ? ocNavigate : null;
    document.addEventListener('DOMContentLoaded', () => {
      loadInceptionState();
      applySectionCollapse('advisory-digest-section');
      /* OpenRouter scan-to-fund return: show the result and clear the flag so a
         reload doesn't re-toast. */
      try {
        const p = new URLSearchParams(window.location.search);
        const or = p.get('openrouter');
        if (or === 'connected') {
          showToast('OpenRouter connected — the "openrouter" gateway is now configured', 'success');
        } else if (or === 'error') {
          showToast('OpenRouter connection failed — please try funding again', 'error');
        }
        if (or) {
          p.delete('openrouter');
          const qs = p.toString();
          history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : ''));
        }
      } catch (e) { /* non-fatal */ }
    });

    function navbarHealthGroups(data) {
      const failing = [];
      const warning = [];
      const passing = [];
      const deepChecks = Array.isArray((data.deepHealth || {}).checks) ? data.deepHealth.checks : [];
      const deepFailNames = [];
      for (const c of deepChecks) {
        const line = c.detail ? `${c.name}: ${c.detail}` : c.name;
        if (c.status === 'fail') { deepFailNames.push(c.name); failing.push(line); }
        else if (c.status === 'warn') warning.push(line);
        else passing.push(line);
      }
      return { failing, warning, passing, deepFailNames };
    }

    function navbarHealthTitle(data) {
      const groups = navbarHealthGroups(data || {});
      const hoverLines = [];
      if (groups.failing.length) hoverLines.push('FAILING:\n' + groups.failing.map(f => '  ✗ ' + f).join('\n'));
      if (groups.warning.length) hoverLines.push('WARNINGS:\n' + groups.warning.map(wl => '  ⚠ ' + wl).join('\n'));
      if (groups.passing.length) hoverLines.push('PASSING:\n' + groups.passing.map(p => '  ✓ ' + p).join('\n'));
      return hoverLines.join('\n\n') || 'No health data';
    }

    function renderNavbarHealthBadge(ocHealth, data) {
      if (!ocHealth) return;
      const groups = navbarHealthGroups(data || {});
      ocHealth.title = navbarHealthTitle(data || {});
      if (groups.deepFailNames.length > 0) {
        // Any failing deep check ⇒ the spoke itself is degraded — same
        // verdict the hub renders for this hive's row. Repo workflow stats are
        // intentionally excluded: they are not this hive's runtime health.
        ocHealth.dataset.status = 'error';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Degraded</span>';
      } else if (groups.failing.length > 0) {
        ocHealth.dataset.status = 'error';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">' + groups.failing.length + ' Issue' + (groups.failing.length > 1 ? 's' : '') + '</span>';
      } else {
        ocHealth.dataset.status = 'ok';
        ocHealth.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Health OK</span>';
      }
      ocHealth.style.color = '';
      ocHealth.style.background = '';
      ocHealth.style.borderColor = '';
    }

    // Providers strip (ADR-0024): per-provider auth vs availability, two
    // separate dimensions so "needs login" never reads as "provider down".
    // Hidden while every provider is usable and available.
    function renderProviderStrip(el, providers) {
      if (!el) return;
      const bad = (providers || []).filter(p => p && (p.auth_state !== 'usable' || p.availability_state !== 'available'));
      if (!bad.length) {
        el.hidden = true;
        el.textContent = '';
        el.title = 'AI providers: all usable';
        el.setAttribute('aria-label', 'AI providers: all usable');
        return;
      }
      const severe = bad.some(p => p.auth_state === 'needs-login' || p.auth_state === 'store-inaccessible' ||
        p.availability_state === 'unavailable' || p.availability_state === 'quota-exhausted');
      el.dataset.status = severe ? 'error' : 'warn';
      el.innerHTML = '<span class="nav-chip__dot" aria-hidden="true">●</span> <span class="nav-chip__label">Providers: ' +
        escapeHtml(bad.map(p => p.provider).join(', ')) + '</span>';
      el.title = bad.map(p => p.provider + ': auth ' + p.auth_state + ', availability ' + p.availability_state +
        (p.next_action ? ' — ' + p.next_action : '')).join('\n');
      el.setAttribute('aria-label', 'AI providers needing attention: ' + bad.map(p => p.provider).join(', '));
      el.hidden = false;
    }

    function qualityDiagnosticsAgent(agents) {
      return (agents || []).find(a => a && a.name === 'quality') || null;
    }

    function renderQualityStatValue(stat, agentName) {
      const v = resolveStatValue(stat, agentName);
      const style = stat.style || 'number';
      const hasValue = v !== undefined && v !== null && v !== '';
      const icon = stat.icon ? escapeHtml(stat.icon) + ' ' : '';

      if (style === 'dot') {
        const skip = v === -1;
        const ok = v === true || v === 1;
        const dot = skip ? '⊘' : ok ? '✓' : hasValue ? '✗' : '—';
        const label = skip ? 'skipped' : ok ? 'ok' : hasValue ? 'fail' : 'unknown';
        const cls = skip ? 'skip' : ok ? 'ok' : hasValue ? 'fail' : 'unknown';
        return `<span class="quality-stat-value ${cls}">${dot} ${label}</span>`;
      }

      if (style === 'pct') {
        const PCT_GOOD = 90;
        const PCT_WARN = 70;
        const cls = !hasValue ? 'unknown' : Number(v) >= PCT_GOOD ? 'good' : Number(v) >= PCT_WARN ? 'warn' : 'bad';
        return `<span class="health-ci ${cls}">${hasValue ? escapeHtml(String(v)) + '%' : '—'}</span>`;
      }

      if (style === 'pct-bar') {
        const numericTarget = typeof stat.target === 'number' ? stat.target : 100;
        const pct = hasValue ? Number(v) : 0;
        const TARGET_WARN_OFFSET = 10;
        const covCls = !hasValue ? '' : pct >= numericTarget ? 'ind-ok' : pct >= numericTarget - TARGET_WARN_OFFSET ? 'ind-warn' : 'ind-err';
        return `<span class="ind-num ${covCls}">${hasValue ? escapeHtml(String(v)) + '%' : '—'}</span> <span class="ind-dlabel">goal: ${numericTarget}%</span>`;
      }

      if (style === 'spark') {
        const statKey = stat.key || stat.field;
        const trendField = stat.trendField || stat.field;
        const color = SPARK_COLORS[trendField] || DEFAULT_SPARK_COLOR;
        const histSeries = historyData.map(s => s.agentStats?.[agentName]?.[statKey] ?? 0);
        const hasHistData = histSeries.some(v => v > 0);
        const trendSeries = hasHistData ? histSeries : (window._trendData || []).map(s => s[trendField] || 0);
        const trendTimes = hasHistData ? getHistoryTimes() : (window._trendData || []).map(s => s.t ?? s.timestamp);
        return `<span class="ind-num">${icon}${hasValue ? escapeHtml(String(v)) : '0'}</span> ${sparkSvg(trendSeries, color, trendTimes, `agent-stat:${agentName}:${statKey}`)}`;
      }

      return `<span class="ind-num">${icon}${hasValue ? escapeHtml(String(v)) : '0'}</span>`;
    }

    function renderQualityStatsCard(agents) {
      const quality = qualityDiagnosticsAgent(agents);
      const stats = (quality && Array.isArray(quality.statsConfig)) ? quality.statsConfig : [];
      const emptyAction = '<button class="hv-btn btn-secondary btn-sm" data-action="openConfigDialog" data-config-type="agent" data-agent="quality" data-tab="Stats">Configure quality stats</button>';
      if (!quality || stats.length === 0) {
        return `<div class="quality-stat-card"><div class="quality-stat-title">Quality stats</div><div class="quality-stat-empty">No stats configured — add them under quality → Configuration → Stats.</div>${emptyAction}</div>`;
      }
      const rows = stats.map(stat => {
        const labelText = stat.label || stat.key || stat.field || 'Stat';
        const icon = stat.icon ? `${escapeHtml(stat.icon)} ` : '';
        const desc = stat.desc ? ` title="${escapeHtml(stat.desc)}"` : '';
        const href = statTargetHref(stat);
        const labelHtml = href
          ? `<a href="${escapeHtml(href)}" target="_blank" rel="noopener"${desc}>${icon}${escapeHtml(labelText)}</a>`
          : `<span${desc}>${icon}${escapeHtml(labelText)}</span>`;
        return `<div class="quality-stat-row">${labelHtml}<span>${renderQualityStatValue(stat, quality.name)}</span></div>`;
      }).join('');
      return `<div class="quality-stat-card"><div class="quality-stat-title">Quality stats</div>${rows}</div>`;
    }


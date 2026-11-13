    // ── Campaigns + Jam (Spek / inception campaign archive) ──
    // Moved verbatim out of inception.js so the Campaigns and Jam UI survives
    // the Inception wizard being replaced by the Sessions section. Plain
    // top-level declarations, shared with inception.js through the single
    // concatenated dashboard script; keep this file adjacent to it in manifest.txt.
    let _campaigns = [];

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
      const activity = c.last_activity || 'no activity';
      const engine = c.engine || 'unknown engine';
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
            <div class="campaign-row-title">${escapeHtml(c.title || c.id)}</div>
            <div class="campaign-row-meta">
              <span class="campaign-pill">${escapeHtml(c.status || 'parked')}</span>
              ${revisionPill}
              <span>${escapeHtml(c.current_stage || '')}${c.current_step ? ' / ' + escapeHtml(c.current_step) : ''}</span>
              <span>${escapeHtml(repo)}</span>
              <span>${escapeHtml(engine)}</span>
              <span>${escapeHtml(activity)}</span>
            </div>
            <div class="campaign-row-meta"><span>${recheckText}</span></div>
            ${recheckExternal}
            ${historyBlock}
          </div>
          <div class="campaign-row-actions">
            <button class="hv-btn btn-secondary btn-sm" data-action="openCampaignJam" data-arg0="${escapeHtml(c.id)}">Jam</button>
            <button class="hv-btn btn-secondary btn-sm" data-action="recheckCampaign" data-arg0="${escapeHtml(c.id)}"${recheckDisabled}>Recheck</button>
            <button class="hv-btn btn-primary btn-sm" data-action="resumeCampaign" data-arg0="${escapeHtml(c.id)}">Continue</button>
            <button class="hv-btn btn-secondary btn-sm" data-action="releaseCampaign" data-arg0="${escapeHtml(c.id)}">Release</button>
            <button class="hv-btn btn-secondary btn-sm" data-action="reviseCampaign" data-arg0="${escapeHtml(c.id)}">Revise</button>
            ${campaignDeleteButton(c)}
          </div>
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
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/resume', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({ surface: 'dashboard' }) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Failed to resume campaign', 'error'); return; }
        if (data.state) { _inceptionState = data.state; renderInception(); }
        if (data.resume_command) { await hiveAlert('Resume command:\n' + data.resume_command, 'Resume command'); }
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function releaseCampaign(id) {
      if (!id) return;
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/release', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({}) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Failed to release campaign', 'error'); return; }
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
    }

    async function reviseCampaign(id) {
      if (!id) return;
      try {
        const r = await fetch('/api/campaigns/' + encodeURIComponent(id) + '/revise', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({}) });
        const data = await r.json();
        if (!data.ok) { showToast(data.error || 'Failed to revise campaign', 'error'); return; }
        await loadCampaigns();
      } catch (e) { showToast('Error: ' + e.message, 'error'); }
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

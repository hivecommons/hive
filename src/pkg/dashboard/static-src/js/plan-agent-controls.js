    // ── Plan review view (#7537) ─────────────────────────────────────────
    // The governor PLANNING tile links here: openPlanList() enumerates every
    // plan (GET /api/plans, drafts first) and openPlanReview(epicID) shows one
    // plan's tree (GET /api/plan/{epicID}) with approve / reject / retag /
    // remove controls wired to the existing owner-gated endpoints.

    function planShowModal(title, bodyHtml) {
      let overlay = document.getElementById('plan-modal-overlay');
      if (!overlay) {
        overlay = document.createElement('div');
        overlay.id = 'plan-modal-overlay';
        overlay.className = 'nous-config-overlay';
        document.body.appendChild(overlay);
      }
      overlay.style.display = 'flex';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = `<div class="nous-config-dialog" style="width:680px">
        <h2 style="font-size:1rem;margin:var(--sp-0);padding:var(--sp-8) var(--sp-8) var(--sp-6);display:flex;align-items:center;justify-content:space-between;flex-shrink:0">
          ${title}
          <button class="hv-btn btn-icon btn-sm" data-action="planCloseModal">✕</button>
        </h2>
        <div class="nous-config-body modal-scroll">${bodyHtml}</div>
      </div>`;
    }

    function planCloseModal() {
      const overlay = document.getElementById('plan-modal-overlay');
      if (overlay) overlay.style.display = 'none';
    }

    function planStatusBadge(p) {
      // Stuck beats queued: the architect was kicked its full attempt budget
      // and produced nothing, so "queued" would read as progress (hivecommons/hive#8010).
      if (p.designStatus === 'needs_human') return '<span class="text-danger">✎ design stuck — revisions exhausted</span>';
      if (p.designStatus === 'requested') return '<span class="text-warn">✎ ' + (p.designVia === 'spektacular' ? 'design via Spektacular — Spec checkpoint' : 'design posted') + ' — awaiting approval</span>';
      if (p.designStatus === 'queued') return '<span class="text-info">✎ ' + (p.designVia === 'spektacular' ? 'design via Spektacular queued' : 'design queued') + '</span>';
      if (p.decomposeFailed) return '<span class="text-danger">⚠ stuck — architect produced no plan; click Plan on the issue to retry</span>';
      if (p.pendingDecompose) return '<span class="text-info">⧗ queued for architect' + (p.decomposeAttempts > 1 ? ' (attempt ' + p.decomposeAttempts + ')' : '') + '</span>';
      if (p.planStatus === 'draft') return '<span class="text-warn">● awaiting your review</span>';
      if (p.planStatus === 'approved') return '<span class="text-success">✓ approved</span>';
      return escapeHtml(p.planStatus || '—');
    }

    async function openPlanList() {
      planShowModal('Plans', '<p class="muted-note">Loading plans…</p>');
      try {
        const res = await fetch('/api/plans');
        const data = await res.json();
        if (!res.ok) throw new Error(data.error || res.statusText);
        const plans = data.plans || [];
        if (!plans.length) {
          planShowModal('Plans', '<p class="muted-note">No plans yet. Click <strong>⧉ Plan</strong> on any issue pill, or add the <code>hive-plan</code> label to an issue on GitHub, and the architect will decompose it into a reviewable plan.</p>');
          return;
        }
        const needsYou = plans.filter(p => p.state === 'review' || p.state === 'stuck' || p.state === 'design_review' || p.state === 'design_stuck').length;
        const header = needsYou
          ? `<p class="muted-note" style="margin:var(--sp-0) var(--sp-0) var(--sp-4)">${needsYou} plan${needsYou === 1 ? '' : 's'} need${needsYou === 1 ? 's' : ''} you — listed first.</p>`
          : '';
        const rows = header + plans.map(p => {
          const issue = p.issueRepo && p.issueNumber
            ? ` <a href="${escapeHtml(p.issueUrl || '#')}" target="_blank" rel="noopener" style="color:var(--muted);font-size:.85em" data-stop="1">${escapeHtml(p.issueRepo)}#${escapeHtml(p.issueNumber)}</a>` : '';
          const kids = p.childrenTotal ? ` · ${p.childrenOpen}/${p.childrenTotal} tasks open` : '';
          return `<div data-action="openPlanReview" data-arg0="${escapeHtml(p.epicId)}" style="cursor:pointer;padding:10px 12px;border:1px solid var(--border);border-radius:8px;margin-bottom:var(--sp-4)">
            <div style="display:flex;justify-content:space-between;gap:var(--sp-5);align-items:baseline">
              <strong>${escapeHtml(p.epicTitle || p.epicId)}</strong>
              <span style="white-space:nowrap;font-size:.85em">${planStatusBadge(p)}</span>
            </div>
            <div class="muted-note mt-1">${escapeHtml(p.epicId)}${issue}${escapeHtml(kids)}</div>
          </div>`;
        }).join('');
        planShowModal('Plans', rows);
      } catch (e) {
        planShowModal('Plans', `<p class="muted-note">Failed to load plans: ${escapeHtml(e.message)}</p>`);
      }
    }

    async function openPlanReview(epicID) {
      planShowModal('Plan review', '<p class="muted-note">Loading plan…</p>');
      try {
        const res = await fetch('/api/plan/' + encodeURIComponent(epicID));
        const data = await res.json();
        if (!res.ok) throw new Error(data.error || res.statusText);
        const plan = data.plan || {};
        const draft = !plan.approved;
        const children = (plan.children || []).map(c => {
          const deps = (c.dependsOn || []).length ? `<span class="muted-note" style="font-size:.8em"> deps: ${(c.dependsOn || []).map(escapeHtml).join(', ')}</span>` : '';
          const controls = draft ? `
              <select data-change-action="planChildRetag" data-arg0="${escapeHtml(plan.epicId)}" data-arg1="${escapeHtml(c.id)}" data-arg-types="s,s,v" class="hv-btn btn-secondary btn" title="Who should do this task — an agent, or a human?">
                <option value="agent_suitable"${c.execution === 'agent_suitable' ? ' selected' : ''}>agent</option>
                <option value="human_required"${c.execution === 'human_required' ? ' selected' : ''}>human</option>
              </select>
              <button class="hv-btn btn-secondary btn" data-action="planChildRemove" data-arg0="${escapeHtml(plan.epicId)}" data-arg1="${escapeHtml(c.id)}" title="Remove this task from the plan">✕</button>`
            : `<span class="muted-note" style="font-size:.85em">${escapeHtml(c.execution || '')}</span>`;
          // Who is on it and where the work is: the two questions the pill
          // chip can't answer (hivecommons/hive#8011).
          const who = c.claimedBy ? `<span class="muted-note" style="font-size:.8em"> · @${escapeHtml(String(c.claimedBy).replace(/^@/, ''))}</span>` : '';
          const pr = c.prUrl ? ` <a href="${escapeHtml(c.prUrl)}" target="_blank" rel="noopener" data-stop="1" style="font-size:.8em" title="Open the pull request">PR ↗</a>` : '';
          return `<div style="display:flex;justify-content:space-between;gap:10px;align-items:center;padding:6px 10px;border:1px solid var(--border);border-radius:var(--r);margin-bottom:var(--sp-3)">
            <div><span class="muted-note" style="font-size:.8em">${escapeHtml(c.planRef || '')}</span> ${escapeHtml(c.title)} <span class="muted-note" style="font-size:.8em">[${escapeHtml(String(c.status || ''))}]</span>${who}${pr}${deps}</div>
            <div style="display:flex;gap:var(--sp-3);align-items:center;white-space:nowrap">${controls}</div>
          </div>`;
        }).join('') || (plan.decomposeFailed
            ? '<p class="muted-note text-danger">⚠ Stuck — the architect was asked ' + escapeHtml(String(plan.decomposeAttempts || '')) + ' times and never ran <code>bd decompose</code> for this epic. Click 📋 Plan on the issue again to retry with a fresh budget.</p>'
            : '<p class="muted-note">No children yet — the architect has been asked to plan this epic and will hand the task list back with <code>bd decompose</code>. Check back once the PLANNING tile stops counting it as queued.</p>');
        const designBlocking = plan.designStatus && plan.designStatus !== 'approved';
        const gate = designBlocking
          ? ''
          : (draft
            ? `<button class="hv-btn btn-secondary btn btn-primary" data-action="planApprove" data-arg0="${escapeHtml(plan.epicId)}" title="Release the plan's tasks to the agent fleet">✓ Approve plan</button>`
            : `<button class="hv-btn btn-secondary btn" data-action="planReject" data-arg0="${escapeHtml(plan.epicId)}" title="Return the plan to draft, re-gating its tasks">↩ Return to draft</button>`);
        const designLine = plan.designStatus
          ? `<div class="muted-note" style="margin-bottom:10px">Design: <strong>${escapeHtml(plan.designStatus)}</strong>${plan.designVia === 'spektacular' ? ' · via Spektacular' : ''}${plan.runKey ? ' · <a href="#" data-action="openRunDetail" data-arg0="' + escapeHtml(plan.runKey) + '" data-stop="1" data-prevent="1">run ' + escapeHtml(plan.runKey) + '</a>' : ''}${plan.designRevision ? ' · revision ' + escapeHtml(String(plan.designRevision)) : ''}</div>`
          : '';
        const canApproveDesign = (plan.designStatus === 'requested' || plan.designStatus === 'needs_human') && dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
        const designGate = canApproveDesign
          ? `<button class="hv-btn btn-secondary btn btn-primary" data-action="designApprove" data-arg0="${escapeHtml(plan.epicId)}" title="Apply the design-approved label and let breakdown proceed">✓ Approve design</button>`
          : '';
        planShowModal(`Plan: ${escapeHtml(plan.epicTitle || epicID)}`, `
          <div class="muted-note" style="margin-bottom:10px">${escapeHtml(plan.epicId || '')} — status: <strong>${escapeHtml(plan.planStatus || 'pending')}</strong></div>
          ${designLine}
          ${children}
          <div style="display:flex;justify-content:space-between;margin-top:14px;gap:var(--sp-4)">
            <button class="hv-btn btn-secondary btn" data-action="openPlanList">← All plans</button>
            <span style="display:flex;gap:var(--sp-4)">${designGate}${gate}</span>
          </div>`);
      } catch (e) {
        planShowModal('Plan review', `<p class="muted-note">Failed to load plan: ${escapeHtml(e.message)}</p>`);
      }
    }

    async function planGateAction(epicID, verb) {
      const toast = showToast(`${verb === 'approve' ? 'Approving' : 'Rejecting'} plan…`, 'info', true);
      try {
        const res = await fetch(`/api/plan/${encodeURIComponent(epicID)}/${verb}`, { method: 'POST' });
        const data = await res.json();
        if (!res.ok || !data.ok) throw new Error(data.error || res.statusText);
        dismissToast(toast, verb === 'approve' ? 'Plan approved — tasks released to the fleet' : 'Plan returned to draft', 'success');
        openPlanReview(epicID);
      } catch (e) {
        dismissToast(toast, `Plan ${verb} failed: ` + e.message, 'error');
      }
    }
    function planApprove(epicID) { planGateAction(epicID, 'approve'); }
    function planReject(epicID) { planGateAction(epicID, 'reject'); }
    async function designApprove(epicID) {
      const toast = showToast('Approving design…', 'info', true);
      try {
        const res = await fetch(`/api/plan/${encodeURIComponent(epicID)}/design/approve`, { method: 'POST' });
        const data = await res.json();
        if (!res.ok || !data.ok) throw new Error(data.error || res.statusText);
        dismissToast(toast, 'Design approved — breakdown can proceed', 'success');
        openPlanReview(epicID);
      } catch (e) {
        dismissToast(toast, 'Design approval failed: ' + e.message, 'error');
      }
    }

    async function planChildEdit(epicID, childID, body, okMsg) {
      const toast = showToast('Updating plan…', 'info', true);
      try {
        const res = await fetch(`/api/plan/${encodeURIComponent(epicID)}/child/${encodeURIComponent(childID)}`, {
          method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
        });
        const data = await res.json();
        if (!res.ok || !data.ok) throw new Error(data.error || res.statusText);
        dismissToast(toast, okMsg, 'success');
        openPlanReview(epicID);
      } catch (e) {
        dismissToast(toast, 'Plan edit failed: ' + e.message, 'error');
      }
    }
    function planChildRetag(epicID, childID, execution) { planChildEdit(epicID, childID, { action: 'retag', execution }, 'Task retagged'); }
    function planChildRemove(epicID, childID) { planChildEdit(epicID, childID, { action: 'remove' }, 'Task removed from plan'); }

    /* setAgentEnabled flips the agent's master-power `enabled` flag.

       It PUTs only `enabled` to the general config section: handleAgentConfigGeneral
       applies each field independently ("if present"), so a one-key body is a
       genuine partial update and cannot clobber the display name, launch command
       or model pins the operator set elsewhere.

       The optimistic DOM flip is deliberately limited to the switch that was
       clicked. The authoritative repaint arrives over SSE within a few seconds;
       guessing at the rest of the card's derived state here is what made the old
       pause toggle render lies when the server disagreed. On failure the switch
       is put back exactly where it was. */
    async function setAgentEnabled(agent, enable) {
      const toast = showToast(`${enable ? 'Enabling' : 'Disabling'} ${agent}...`, 'info', true);
      const switches = Array.from(document.querySelectorAll(`.agent-power-switch[data-agent="${CSS.escape(agent)}"]`));
      const restore = switches.map(el => el.classList.contains('on'));
      const paint = (on) => {
        for (const el of switches) {
          el.classList.toggle('on', on);
          el.setAttribute('aria-checked', on ? 'true' : 'false');
          el.dataset.enabled = on ? 'true' : 'false';
        }
      };
      paint(enable);
      try {
        const res = await fetch(`/api/config/agent/${encodeURIComponent(agent)}/general`, {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ enabled: enable })
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok || data.ok === false) {
          switches.forEach((el, i) => {
            el.classList.toggle('on', restore[i]);
            el.setAttribute('aria-checked', restore[i] ? 'true' : 'false');
            el.dataset.enabled = restore[i] ? 'true' : 'false';
          });
          dismissToast(toast, `Could not ${enable ? 'enable' : 'disable'} ${agent}: ${data.error || 'HTTP ' + res.status}`, 'error');
          return;
        }
        const live = (window._lastAgents || []).find(a => a.name === agent);
        if (live) live.enabled = enable;
        dismissToast(toast, `${agent} ${enable ? 'enabled' : 'disabled'}`, 'success');
      } catch (e) {
        switches.forEach((el, i) => {
          el.classList.toggle('on', restore[i]);
          el.setAttribute('aria-checked', restore[i] ? 'true' : 'false');
          el.dataset.enabled = restore[i] ? 'true' : 'false';
        });
        dismissToast(toast, `Could not ${enable ? 'enable' : 'disable'} ${agent}: ${e.message}`, 'error');
      }
    }

    async function toggleAgent(agent, currentlyPaused) {
      // Server-authoritative toggle. The caller's belief (a stale DOM
      // dataset) once inverted the action: a persisted-paused agent rendered
      // un-paused, so the operator's "start it" click sent a silent no-op
      // re-pause and the agent stayed paused indefinitely. Two defenses:
      // (1) re-sync the real state from /api/agent-state before choosing
      // pause-vs-resume, (2) render from the response's authoritative
      // state/changed fields instead of assuming the action succeeded as
      // intended.
      let believedPaused = currentlyPaused;
      try {
        const st = await (await fetch(`/api/agent-state/${agent}`)).json();
        if (st && st.ok === true && typeof st.paused === 'boolean') believedPaused = st.paused;
      } catch (e) { /* best-effort — fall back to the caller's belief */ }
      const action = believedPaused ? 'resume' : 'pause';
      const label = believedPaused ? 'Resuming' : 'Pausing';
      const toast = showToast(`${label} ${agent}...`, 'info', true);
      const res = await fetch(`/api/${action}/${agent}`, { method: 'POST' });
      const data = await res.json();
      if (!data.ok) { dismissToast(toast, `${action} failed: ${data.error || 'unknown'}`, 'error'); return; }
      // Authoritative post-request state from the server. Older servers that
      // omit `state` fall back to the pre-fix assumption.
      const nowPaused = (data.state === 'paused' || data.state === 'running')
        ? data.state === 'paused'
        : action === 'pause';
      // DOM update from the AUTHORITATIVE state (not a blind optimistic
      // assumption) — reflect it immediately without waiting for SSE.
      const cards = document.querySelectorAll('.agent-card');
      for (const card of cards) {
        const nameEl = card.querySelector('.agent-name');
        if (!nameEl || !nameEl.textContent.includes(agent)) continue;
        card.className = card.className.replace(/\b(paused|off|idle|working|stopped)\b/g, '').trim() + (nowPaused ? ' paused' : ' idle');
        const stateEl = card.querySelector('.agent-state');
        if (stateEl) { stateEl.className = `agent-state ${nowPaused ? 'paused' : 'idle'}`; stateEl.textContent = nowPaused ? 'paused' : 'idle'; }
        const fields = card.querySelectorAll('.agent-field');
        for (const f of fields) {
          const lbl = f.querySelector('.label')?.textContent;
          const val = f.querySelector('.value');
          if (!lbl || !val) continue;
          if (lbl === 'interval' || lbl === 'next kick') val.textContent = nowPaused ? 'paused' : '—';
        }
        const btn = card.querySelector('.btn-toggle');
        if (btn) { btn.className = `hv-btn ${nowPaused ? 'btn-danger' : 'btn-secondary'} btn-sm btn-toggle ${nowPaused ? 'paused' : 'running'}`; btn.textContent = nowPaused ? '▶ resume' : '⏸ pause'; btn.dataset.action = 'toggleAgent'; btn.dataset.agent = agent; btn.dataset.paused = String(nowPaused); }
        break;
      }
      // Update in-memory agent state so cadence table re-renders correctly
      const agentObj = (window._lastAgents || []).find(a => a.name === agent);
      if (agentObj) { agentObj.paused = nowPaused; }
      // The blockers line and the toggle tooltip (#5594) both render from the
      // agent object, so patch them from the SAME authoritative state. Left
      // alone they would keep saying "paused" after a successful resume until
      // the next poll — the stale-control bug this function already exists to
      // prevent for the button label.
      if (agentObj) {
        document.querySelectorAll(`.agent-card[data-agent="${CSS.escape(agent)}"] .agent-blockers`)
          .forEach(el => { el.outerHTML = agentBlockerLineHtml(agentObj); });
        document.querySelectorAll(`.agent-card[data-agent="${CSS.escape(agent)}"] .btn-toggle`)
          .forEach(el => {
            // A just-paused agent gets the full combined-action rebuild —
            // the combined start-and-resume button when its session is also
            // down (#5594 follow-up); an unpaused one only needs its tooltip
            // refreshed.
            if (nowPaused) el.outerHTML = pausedAgentActionHtml(agentObj);
            else el.title = agentToggleTitle(agentObj, false, false);
          });
      }
      // The focused agent-detail panel renders its own toggle button — the
      // .agent-card loop above never reaches it, so re-render it from the
      // just-updated state or the button stays stale until navigation.
      if (typeof ocRenderAgentDetail === 'function') ocRenderAgentDetail();
      const lastData = window._lastStatus || {};
      if (lastData.governor) {
        renderGovernor(lastData.governor, lastData.cadenceMatrix || [], lastData);
      }
      renderDebugSection();
      if (data.changed === false) {
        // No-op transition: tell the operator the REAL state so the next
        // click does the right thing (the button above was just re-rendered
        // from that state).
        dismissToast(toast, nowPaused
          ? `${agent} was already paused — click ▶ resume to start it`
          : `${agent} was already running — click ⏸ pause to stop it`, 'info');
      } else {
        dismissToast(toast, `${agent} ${action}d`, 'success');
      }
    }

    // Combined start & resume for a paused agent whose session is also down
    // (#5594 follow-up to resume-leads). Client-side chain of the two
    // existing endpoints — resume FIRST so the pause is cleared before the
    // supervisor spawns the session (a session started while still paused
    // would just sit unkickable), then restart, which for a down session is
    // simply "start". No confirm dialog: unlike restartAgent there is no
    // live session to kill.
    async function startResumeAgent(agent) {
      const toast = showToast(`Starting & resuming ${agent}...`, 'info', true);
      try {
        const res = await fetch(`/api/resume/${agent}`, { method: 'POST' });
        const data = await res.json();
        if (!data.ok) { dismissToast(toast, `Resume failed: ${data.error || 'unknown'}`, 'error'); return; }
        const res2 = await fetch(`/api/restart/${agent}`, { method: 'POST' });
        const data2 = await res2.json();
        if (!data2.ok) { dismissToast(toast, `Resumed, but session start failed: ${data2.error || 'unknown'}`, 'error'); return; }
      } catch (e) {
        dismissToast(toast, `Start & resume failed: ${e}`, 'error');
        return;
      }
      // Same optimistic patch discipline as toggleAgent: the blockers line
      // and the action button render from the agent object, so update it and
      // repaint before the next poll confirms.
      const agentObj = (window._lastAgents || []).find(x => x.name === agent);
      if (agentObj) {
        agentObj.paused = false;
        document.querySelectorAll(`.agent-card[data-agent="${CSS.escape(agent)}"] .agent-blockers`)
          .forEach(el => { el.outerHTML = agentBlockerLineHtml(agentObj); });
      }
      if (typeof ocRenderAgentDetail === 'function') ocRenderAgentDetail();
      refreshStatus();
      dismissToast(toast, `${agent} resumed — supervisor is starting the session`, 'success');
    }

    async function restartAgent(agent) {
      if (!(await hiveConfirm(`Restart ${agent}? This will kill the current session and start fresh.`))) return;
      const toast = showToast(`Restarting ${agent}...`, 'info', true);
      const res = await fetch(`/api/restart/${agent}`, { method: 'POST' });
      const data = await res.json();
      if (!data.ok) { dismissToast(toast, `Restart failed: ${data.error || 'unknown'}`, 'error'); return; }
      // A restart cancels a kick that was still being delivered (#7363) —
      // say so, because the old behaviour replayed it into the new session.
      dismissToast(toast, data.kickCancelled
        ? `${agent} restarted — the pending kick was cancelled, not replayed`
        : `${agent} restarted — supervisor will respawn`, 'success');
    }

    async function resetRestarts(agent) {
      const toast = showToast(`Resetting ${agent} restart counter...`, 'info', true);
      const res = await fetch(`/api/reset-restarts/${agent}`, { method: 'POST' });
      const data = await res.json();
      if (!data.ok) { dismissToast(toast, `Reset failed: ${data.error || 'unknown'}`, 'error'); return; }
      // Raise the stale-snapshot floor BEFORE the optimistic patch so an
      // in-flight pre-reset poll/SSE frame can't flip the counter back to
      // the old value (#4348), then refetch so the real post-reset snapshot
      // repaints everything; errors swallowed inside refreshStatus.
      noteStatusMutation(data.minStatusSeq);
      const btn = document.querySelector(`.restart-reset[data-agent="${agent}"]`);
      if (btn) {
        const el = btn.closest('.value');
        if (el) { el.className = 'value'; el.innerHTML = '0<span class="restart-label">24h</span><span class="restart-spark"></span>'; }
      }
      refreshStatus();
      dismissToast(toast, `${agent} restart counter reset`, 'success');
    }

    // ensureGatewayMethodConfigured is the SHARED guard for the agent-config
    // dialog flows (CLI pin / create): picking a gateway-backed method whose
    // gateway is missing (or has no key) must not silently save a method the
    // agent cannot actually run — an operator once set backend: watsonx with no
    // watsonx gateway and the agent failed to launch for a day with only
    // "unknown backend: watsonx" in the log. litellm keeps its dedicated
    // endpoint+key input dialog (it has a legacy governor.litellm block the others do
    // not); every other gateway method routes to the Model Gateways tab, which
    // is where its key is entered. Resolves true when the method is (or has just
    // been) configured, false when the user backs out — same contract as
    // ensureLiteLLMConfigured, so all three call sites treat every method alike.
    async function ensureGatewayMethodConfigured(method) {
      if (method === 'litellm') return ensureLiteLLMConfigured();
      if (!GATEWAY_METHODS.includes(method)) return true;
      // A live (non-fallback) discovery proves the method is genuinely usable
      // (possibly via HIVE_*_ENDPOINT env, which never appears in the gateway
      // list) — mirrors the same check maybeOpenMethodConfig makes.
      const cached = _inferenceModelCache[method];
      if (cached && !cached.fallback && cached.models && cached.models.length) return true;
      let gateways = [];
      try {
        const r = await fetch('/api/config/governor/gateways');
        if (!r.ok) return true; // presence check is best-effort — never block on it
        const d = await r.json();
        gateways = d.gateways || [];
      } catch (e) { return true; }
      // Require a matching gateway; for key-requiring kinds (hosted services:
      // watsonx needs an IAM-minted token, openrouter a bearer key) also
      // require the key. Self-hosted vllm/llm-d gateways are commonly
      // unauthenticated, so mere presence (or an env-configured endpoint,
      // caught by the discovery check above) satisfies them.
      const gw = gateways.find(g => g && (g.kind === method || g.name === method));
      if (gw && (gw.hasKey || !GATEWAY_KEY_REQUIRED_METHODS.includes(method))) return true;
      const why = gw
        ? `The ${method} gateway has no API key — ${method} needs one before this agent can run.`
        : `No ${method} gateway is configured — ${method} needs ${GATEWAY_KEY_REQUIRED_METHODS.includes(method) ? 'an endpoint and API key' : 'an endpoint'} before this agent can run.`;
      if (!(await hiveConfirm(`${why}\n\nOpen Settings → Model Gateways to set it up? Unsaved changes in this dialog will be discarded.`))) {
        return false;
      }
      // Land directly on a NEW gateway form pre-set to this method's kind
      // (endpoint/preset already filled), consumed once by loadGateways().
      _pendingGatewayPresetKind = method;
      closeConfigDialog();
      openConfigDialog('governor', null, 'Model Gateways');
      return false;
    }

    // Prompts for the LiteLLM endpoint + API key when litellm is picked in
    // the agent-config dialog flows (CLI pin / create), where navigating to
    // Settings would discard unsaved edits. The agent-card METHOD
    // dropdown instead saves the choice and navigates (maybeOpenMethodConfig).
    // Resolves true when litellm is (or has just been) configured; false when
    // the user cancels. The
    // key VALUE goes to the server's private key store — never hive.yaml —
    // and the save runs a live /v1/models test whose result is shown
    // inline (endpoint/key mistakes fail here, not as agent 401s later).
    async function ensureLiteLLMConfigured() {
      try {
        const resp = await fetch('/api/config/governor');
        const data = await resp.json();
        if (data.litellm && data.litellm.endpoint) return true;
      } catch (e) { /* config unreachable — fall through to the prompt */ }
      return new Promise(resolve => {
        const overlay = document.createElement('div');
        // Compact first-selection prompt on the gh-auth-modal pattern (the
        // Claude login modal): centered amber title, tight labeled inputs,
        // centered buttons, 480px max width (#1787).
        overlay.className = 'gh-auth-overlay cred-dialog-overlay';
        overlay.innerHTML = `<div class="gh-auth-modal" style="max-width:480px;text-align:left">
          <h3 style="color:var(--amber);text-align:center">Configure LiteLLM</h3>
          <p style="margin:0 0 14px;line-height:1.5">No LiteLLM gateway is configured yet. Enter the gateway URL and your API key (editable later under Settings → Model Gateways → Legacy LiteLLM endpoint).</p>
          <div class="config-field">
            <label>Endpoint URL</label>
            <input type="text" id="litellm-setup-endpoint" placeholder="https://litellm.example.com" style="width:100%;box-sizing:border-box">
          </div>
          <div class="config-field">
            <label>API Key <span style="font-weight:400;text-transform:none">(stored privately on this hive — never in hive.yaml or logs)</span></label>
            <input type="password" id="litellm-setup-key" placeholder="sk-..." autocomplete="new-password" style="width:100%;box-sizing:border-box">
          </div>
          <details style="margin:var(--sp-0) var(--sp-0) var(--sp-5)">
            <summary style="cursor:pointer;font-size:var(--fs-sm);color:var(--muted)">Advanced — self-hosted key sources</summary>
            <div class="config-field mt-4">
              <label>Read Key From Environment Variable</label>
              <input type="text" id="litellm-setup-keyenv" placeholder="HIVE_LITELLM_API_KEY" style="width:100%;box-sizing:border-box">
              <div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-1)">Enter the NAME of an environment variable on the hive container that contains your key (self-hosted installs). Hosted users: paste your key in the API Key field above instead.</div>
            </div>
            <div class="config-field">
              <label>Read Key From File Path</label>
              <input type="text" id="litellm-setup-keyfile" placeholder="/secrets/litellm_api_key" style="width:100%;box-sizing:border-box">
              <div style="font-size:0.7rem;color:var(--muted);margin-top:var(--sp-1)">Path to a file that contains your key, such as a Kubernetes Secret mount (self-hosted installs).</div>
            </div>
          </details>
          <div id="litellm-setup-result" style="display:none;font-size:var(--fs-sm);margin:0 0 10px;white-space:pre-wrap;line-height:1.4"></div>
          <div style="display:flex;gap:var(--sp-4);justify-content:center">
            <button class="hv-btn btn-primary hive-dialog-btn primary">Save &amp; Test</button>
            <button class="hv-btn btn-secondary hive-dialog-btn cancel">Cancel</button>
          </div></div>`;
        document.body.appendChild(overlay);
        overlay.style.display = 'flex';
        const close = (val) => { overlay.remove(); resolve(val); };
        overlay.querySelector('.cancel').onclick = () => close(false);
        overlay.onclick = (e) => { if (e.target === overlay) close(false); };
        const resultEl = overlay.querySelector('#litellm-setup-result');
        const showResult = (msg, ok) => {
          resultEl.textContent = msg;
          resultEl.style.display = 'block';
          resultEl.style.color = ok ? 'var(--green)' : 'var(--red)';
        };
        overlay.querySelector('.primary').onclick = async () => {
          const endpoint = overlay.querySelector('#litellm-setup-endpoint').value.trim();
          if (!endpoint) { showResult('Endpoint URL is required', false); return; }
          const key = overlay.querySelector('#litellm-setup-key').value.trim();
          const keyEnv = overlay.querySelector('#litellm-setup-keyenv').value.trim();
          const keyFile = overlay.querySelector('#litellm-setup-keyfile').value.trim();
          // Same guardrails as the server: a key VALUE pasted into the
          // NAME/PATH fields would leak into hive.yaml and resolve to an
          // empty key at runtime.
          if (keyEnv && looksLikeApiKeyValue(keyEnv)) {
            showResult('This looks like an API key — paste it in the API Key field above; the advanced field takes an environment variable NAME (e.g. HIVE_LITELLM_API_KEY).', false);
            return;
          }
          if (keyFile && !keyFile.startsWith('/') && looksLikeApiKeyValue(keyFile)) {
            showResult('This looks like an API key — paste it in the API Key field above; the advanced field takes a file PATH (e.g. /secrets/litellm_api_key).', false);
            return;
          }
          const body = { endpoint };
          if (key) body.apiKey = key;
          if (keyEnv) body.apiKeyEnv = keyEnv;
          if (keyFile) body.apiKeyFile = keyFile;
          try {
            const res = await fetch('/api/config/governor/litellm', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify(body)
            });
            const data = await res.json().catch(() => ({}));
            if (!res.ok) {
              showResult(data.error || 'Failed to save LiteLLM config', false);
              return;
            }
            if (data.probe && !data.probe.ok) {
              // Saved, but the live /v1/models test failed — keep the
              // dialog open with the gateway's actual error so the user
              // can fix the key/endpoint and try again immediately.
              showResult('Saved, but the connection test failed: ' + data.probe.error + '\nFix the endpoint or key and Save & Test again.', false);
              return;
            }
            if (data.probe && data.probe.ok && data.probe.modelsListBlocked) {
              showToast(`LiteLLM configured — inference with ${data.probe.inferenceModel} works, but model listing (GET /v1/models) is blocked in front of the gateway`, 'info');
              close(true);
              return;
            }
            const n = data.probe && data.probe.ok ? data.probe.models : null;
            showToast(n === null ? 'LiteLLM endpoint saved' : `LiteLLM configured — ${n} model${n === 1 ? '' : 's'} available`, 'success');
            close(true);
          } catch (e) {
            showResult('Network error: ' + e.message, false);
          }
        };
        overlay.querySelector('#litellm-setup-endpoint').focus();
      });
    }


    // Methods that need hive-side configuration before they can actually run:
    // bob needs its API key (Settings → Bob); the gateway-backed
    // methods need a Model Gateway configured FOR THAT method (Settings
    // → Model Gateways). The check is per-method — an openrouter
    // gateway does not satisfy a vllm selection.
    const GATEWAY_METHODS = ['openrouter', 'litellm', 'vllm', 'llm-d', 'watsonx'];
    // Hosted gateway kinds that cannot work without an API key (watsonx mints
    // an IAM token from it; openrouter authenticates with a bearer key).
    // Self-hosted vllm/llm-d endpoints are commonly unauthenticated, so a
    // configured gateway (or env endpoint) without a key is legitimate there.
    const GATEWAY_KEY_REQUIRED_METHODS = ['openrouter', 'watsonx'];

    // maybeOpenMethodConfig runs AFTER a method switch is saved: the choice is
    // never blocked, but when the selected method's backing config is absent
    // the Settings dialog opens directly on the tab that fixes
    // it. Presence comes from data the dashboard already serves — the bob key
    // status endpoint (presence only, never material) and the configured
    // gateway list — plus the live model-discovery cache (a non-fallback
    // discovery proves an env-configured vllm/llm-d endpoint exists even
    // without a named gateway). Best-effort: any fetch failure just skips the
    // navigation rather than interrupting the switch.
    async function maybeOpenMethodConfig(backend) {
      try {
        if (backend === 'bob') {
          const r = await fetch('/api/config/governor/bob');
          if (!r.ok) return;
          const d = await r.json();
          if (d.configured !== true) {
            showToast('bob needs an API key — opening Settings → Bob', 'info');
            openConfigDialog('governor', null, GOVERNOR_BOB_TAB);
          }
          return;
        }
        if (!GATEWAY_METHODS.includes(backend)) return;
        // A live (non-fallback) model discovery for this method means it is
        // genuinely configured (possibly via HIVE_*_ENDPOINT env, which never
        // appears in the gateway list) — nothing to fix.
        const cached = _inferenceModelCache[backend];
        if (cached && !cached.fallback && cached.models && cached.models.length) return;
        const r = await fetch('/api/config/governor/gateways');
        if (!r.ok) return;
        const d = await r.json();
        // Same contract as ensureGatewayMethodConfigured: the method needs a
        // matching gateway, plus a key when the kind requires one (watsonx
        // mints an IAM token from it, openrouter authenticates with it).
        // Self-hosted vllm/llm-d are commonly unauthenticated — presence (or
        // the live-discovery check above) is enough for them.
        const gw = (d.gateways || []).find(g => g && (g.kind === backend || g.name === backend));
        if (!gw || (!gw.hasKey && GATEWAY_KEY_REQUIRED_METHODS.includes(backend))) {
          const why = gw ? 'The ' + backend + ' gateway has no API key'
            : 'No ' + backend + ' gateway configured';
          showToast(why + ' — opening Settings → Model Gateways', 'info');
          // Guided jump: land the user directly on a NEW gateway form pre-set to
          // this method's kind (endpoint/preset already filled) instead of an
          // empty list they must then "+ Add" from. Consumed by loadGateways().
          _pendingGatewayPresetKind = backend;
          openConfigDialog('governor', null, 'Model Gateways');
        }
      } catch (e) { /* presence check is best-effort; never surface as a switch error */ }
    }

    async function switchCli(agent, backend) {
      if (!backend) return;
      const label = isInferenceBackendName(backend) ? 'method' : 'CLI';
      const toast = showToast(`Switching ${agent} ${label} → ${backend} (restarting session)...`, 'info', true);
      const res = await fetch(`/api/switch/${agent}/${backend}`, { method: 'POST' });
      const data = await res.json();
      if (!data.ok) {
        // A gateway method can FAIL the switch itself when nothing routes it —
        // openrouter is valid only as a configured gateway NAME (it is not in
        // the server's InferenceBackends list), so with no openrouter gateway
        // the server rejects the switch with "unsupported backend". A dead-end
        // error toast tells the operator nothing actionable; route them to the
        // Model Gateways form pre-set to this kind instead, like every other
        // unconfigured gateway method.
        if (GATEWAY_METHODS.includes(backend)) {
          dismissToast(toast, `No ${backend} gateway configured — opening Settings → Model Gateways`, 'info');
          _pendingGatewayPresetKind = backend;
          openConfigDialog('governor', null, 'Model Gateways');
          return;
        }
        dismissToast(toast, `Switch failed: ${data.error || 'unknown'}`, 'error');
        return;
      }
      dismissToast(toast, `${agent} ${label} → ${backend} (session restarted)`, 'success');
      // The selection is saved; if its backing config is missing, land the
      // user on the Settings tab that fixes it (never blocks).
      maybeOpenMethodConfig(backend);
      await updateModelDropdown(agent, backend);
      if (window._lastAgents) {
        const a = window._lastAgents.find(x => x.name === agent);
        if (a) { a.cli = backend; renderAgents(window._lastAgents); }
      }
      // Reflect the switch everywhere it is shown (focused detail panel,
      // cadence-matrix method column), then confirm against the server.
      ocRenderAgentDetail();
      const lastData = window._lastStatus || {};
      renderGovernor(lastData.governor || {}, lastData.cadenceMatrix || [], lastData);
      fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
    }

    async function updateModelDropdown(agent, backend) {
      const modelSelects = document.querySelectorAll(`select[data-change-action="switchModel"][data-agent="${agent}"]`);
      const isInference = isInferenceBackendName(backend);
      const isCli = CLI_BACKENDS.includes(backend);
      if (isInference || isCli) {
        modelSelects.forEach(sel => {
          sel.innerHTML = '<option value="">loading models…</option>';
          sel.disabled = true;
        });
        const models = isInference ? await fetchInferenceModels(canonicalInferenceBackendName(backend)) : (await fetchBackendsConfig(), modelsForBackend(backend));
        modelSelects.forEach(sel => {
          sel.innerHTML = '<option value="">model ▾</option>' +
            models.map(m => `<option value="${m.value}">${m.label}</option>`).join('');
          sel.disabled = false;
          applyModelNoticeTitle(sel, backend);
        });
      } else {
        modelSelects.forEach(sel => {
          sel.innerHTML = '<option value="">model ▾</option>' +
            KNOWN_MODELS.map(m => `<option value="${m.value}">${m.label}</option>`).join('');
        });
      }
    }

    // Refresh a backend's model list whenever its dropdown is opened, so
    // options reflect runtime state (endpoint/key changes, new models) —
    // for BOTH inference backends (vllm/llm-d/litellm) and CLI backends
    // (copilot/claude/gemini/goose). fetchInferenceModels/fetchBackendsConfig
    // rate-limit via their own TTL caches, so this cannot hammer the API.
    document.addEventListener('focusin', async (e) => {
      const sel = e.target;
      if (!sel || !sel.matches || !sel.matches('select[data-change-action="switchModel"]')) return;
      const agentName = sel.dataset.agent;
      const a = (window._lastAgents || []).find(x => x.name === agentName);
      const backend = a && a.cli;
      if (!backend) return;
      const isInference = isInferenceBackendName(backend);
      const isCli = CLI_BACKENDS.includes(backend);
      if (!isInference && !isCli) return;
      const prev = JSON.stringify(BACKEND_MODELS[backend] || []);
      const models = isInference ? await fetchInferenceModels(canonicalInferenceBackendName(backend)) : (await fetchBackendsConfig(), modelsForBackend(backend));
      applyModelNoticeTitle(sel, backend);
      if (JSON.stringify(models) !== prev) {
        const current = sel.value;
        sel.innerHTML = '<option value="">model ▾</option>' +
          models.map(m => `<option value="${m.value}">${m.label}</option>`).join('');
        sel.value = current;
      }
    });

    async function switchModel(agent, model) {
      if (!model) return;
      const toast = showToast(`Switching ${agent} model → ${model} (restarting session)...`, 'info', true);
      const res = await fetch(`/api/model/${agent}/${encodeURIComponent(model)}`, { method: 'POST' });
      const data = await res.json();
      if (!data.ok) { dismissToast(toast, `Model switch failed: ${data.error || 'unknown'}`, 'error'); return; }
      dismissToast(toast, `${agent} model → ${model} (session restarted)`, 'success');
      if (window._lastAgents) {
        const a = window._lastAgents.find(x => x.name === agent);
        if (a) { a.model = model; renderAgents(window._lastAgents); }
      }
      // Reflect the switch everywhere it is shown (focused detail panel,
      // cadence-matrix model column), then confirm against the server.
      ocRenderAgentDetail();
      const lastData = window._lastStatus || {};
      renderGovernor(lastData.governor || {}, lastData.cadenceMatrix || [], lastData);
      fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
    }

    // switchEffort mirrors switchModel for the reasoning-effort dropdown:
    // persist via POST /api/effort (which restarts the session — the effort
    // only exists on the launch command line), update the local copy, and
    // confirm against the server. "default" clears the stored effort.
    async function switchEffort(agent, effort) {
      if (!effort) return;
      const toast = showToast(`Switching ${agent} reasoning effort → ${effort} (restarting session)...`, 'info', true);
      const res = await fetch(`/api/effort/${agent}/${encodeURIComponent(effort)}`, { method: 'POST' });
      const data = await res.json();
      if (!data.ok) { dismissToast(toast, `Effort switch failed: ${data.error || 'unknown'}`, 'error'); return; }
      dismissToast(toast, `${agent} reasoning effort → ${effort} (session restarted)`, 'success');
      if (window._lastAgents) {
        const a = window._lastAgents.find(x => x.name === agent);
        if (a) { a.reasoningEffort = effort === 'default' ? '' : effort; renderAgents(window._lastAgents); }
      }
      ocRenderAgentDetail();
      fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
    }

    const PIN_OVERRIDE_TTL_MS = 60000;
    const _pinOverrides = {};
    async function togglePin(agent, dimension, currentlyPinned) {
      const action = currentlyPinned ? 'unpin' : 'pin';
      const nowPinned = !currentlyPinned;
      const key = `${agent}:${dimension}`;
      _pinOverrides[key] = { value: nowPinned, expires: Date.now() + PIN_OVERRIDE_TTL_MS };
      const agents = window._lastAgents || [];
      applyPinOverrides(agents);
      renderAgents(agents);
      const lastData = window._lastStatus || {};
      renderGovernor(lastData.governor || {}, lastData.cadenceMatrix || [], lastData);
      const res = await fetch(`/api/${action}/${agent}/${dimension}`, { method: 'POST' });
      const data = await res.json();
      if (!data.ok) {
        showToast(`${action} failed: ` + (data.error || 'unknown'), 'error');
        delete _pinOverrides[key];
        fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
        return;
      }
      showToast(`${agent} ${dimension} ${action}ned`, 'success');
      fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
    }
    function applyPinOverrides(agents) {
      const now = Date.now();
      for (const key of Object.keys(_pinOverrides)) {
        if (_pinOverrides[key].expires < now) { delete _pinOverrides[key]; continue; }
        const [agent, dimension] = key.split(':');
        const a = (agents || []).find(x => x.name === agent);
        if (!a) continue;
        const v = _pinOverrides[key].value;
        if (dimension === 'cli') { a.pinnedCli = v; if (!v && a.pinnedBoth) { a.pinnedBoth = false; a.pinnedModel = true; } }
        else if (dimension === 'model') { a.pinnedModel = v; if (!v && a.pinnedBoth) { a.pinnedBoth = false; a.pinnedCli = true; } }
        else { a.pinnedBoth = v; a.pinnedCli = v; a.pinnedModel = v; }
      }
    }

    // ACMM Evaluation
    const ACMM_EVAL_INTERVAL_MS = 3600000; // 1 hour
    const ACMM_LEVEL_NAMES = {0:'Prerequisites not met',1:'Inception',2:'Advisory',3:'Quality-Gated',4:'Security-Aware',5:'Semi-Autonomous',6:'Autonomous'};
    const ACMM_LEVEL_DESCRIPTIONS = {
      0: 'Foundational tooling: test suites, CI/CD, templates, and code style enforcement.',
      2: 'AI agents receive explicit instructions via CLAUDE.md, copilot-instructions, cursor rules, and prompt catalogs.',
      3: 'PR acceptance metrics, review rubrics, quality dashboards, and safety model enforcement are in place.',
      4: 'Self-tuning QA, nightly compliance, automated review application, AI-fix workflows, and session continuity.',
      5: 'GitHub Actions AI integration, auto-QA self-tuning, public metrics, policy-as-code, and reflection logs.',
      6: 'Auto issue generation, multi-agent orchestration, strategic dashboards, a merge queue or equivalent serialized merging, and full governance.'
    };
    const ACMM_CRITERION_TIPS = {
      'acmm:prereq-test-suite': 'A test framework (Jest, Vitest, pytest, Go test) so agents can validate their changes before submitting.',
      'acmm:prereq-e2e': 'End-to-end tests (Playwright, Cypress) that verify full user flows — agents use these to catch integration regressions.',
      'acmm:prereq-cicd': 'A CI/CD pipeline (GitHub Actions, GitLab CI, etc.) that runs automatically on PRs so agent changes are validated.',
      'acmm:prereq-pr-template': 'A pull request template that guides agents to include context, test plans, and change descriptions.',
      'acmm:prereq-issue-template': 'Issue templates that give agents structured context when picking up work from the backlog.',
      'acmm:prereq-contrib-guide': 'A contributing guide that tells agents (and humans) the project\'s conventions, branching model, and review process.',
      'acmm:prereq-code-style': 'A linter/formatter config (ESLint, Prettier, golangci-lint, Ruff) so agents produce code matching project style.',
      'acmm:prereq-coverage-gate': 'A coverage threshold that prevents agents from merging changes that reduce test coverage.',
      'acmm:claude-md': 'CLAUDE.md gives Claude Code project-specific instructions — conventions, forbidden patterns, preferred libraries.',
      'acmm:copilot-instructions': 'Copilot instructions file that customizes GitHub Copilot\'s suggestions for this repo.',
      'acmm:agents-md': 'AGENTS.md documents which AI agents work on the repo, their roles, and coordination rules.',
      'acmm:cursor-rules': 'Cursor rules file that configures Cursor IDE\'s AI behavior for this project.',
      'acmm:prompts-catalog': 'A catalog of reusable prompts/commands that agents can invoke for common tasks.',
      'acmm:editor-config': 'EditorConfig ensures consistent formatting (indentation, line endings) across all editors and agents.',
      'acmm:simple-skills': 'Custom skills/commands that extend agent capabilities with project-specific automation.',
      'acmm:correction-capture': 'A memory/corrections system where agent mistakes are recorded to prevent repeating them.',
      'acmm:pr-acceptance-metric': 'Tracking PR acceptance rates over time — how often agent PRs are merged vs. rejected.',
      'acmm:pr-review-rubric': 'A review rubric or checklist that standardizes how agent PRs are evaluated.',
      'acmm:quality-dashboard': 'A quality dashboard or analytics page that visualizes agent contribution quality.',
      'acmm:ci-matrix': 'A CI test matrix that runs tests across multiple environments/configurations.',
      'acmm:layered-safety': 'Claude settings that restrict which tools/commands agents can use (permission boundaries).',
      'acmm:mechanical-enforcement': 'Mechanical enforcement of safety rules via settings, not just instructions.',
      'acmm:session-summary': 'Session summaries that capture what an agent learned, enabling continuity across sessions.',
      'acmm:structural-gates': 'Structural gates in settings that require human approval for risky operations.',
      'acmm:auto-qa-tuning': 'Auto-QA that self-tunes thresholds based on historical pass/fail patterns.',
      'acmm:nightly-compliance': 'Nightly compliance checks that catch drift, stale dependencies, or broken links.',
      'acmm:copilot-review-apply': 'Automated application of review feedback — agents fix issues flagged in reviews.',
      'acmm:auto-label': 'Automated issue/PR labeling that routes work to the right agents or reviewers.',
      'acmm:ai-fix-workflow': 'An AI-fix-requested workflow that assigns labeled issues to agents for automated resolution.',
      'acmm:tier-classifier': 'Change classification that categorizes PRs by risk level (trivial, standard, critical).',
      'acmm:security-ai-md': 'An AI security policy documenting how agents handle secrets, auth, and sensitive data.',
      'acmm:session-continuity': 'Session continuity files that let agents resume work across multiple sessions.',
      'acmm:cross-session-knowledge': 'A knowledge base that persists learnings across agent sessions and contributors.',
      'acmm:github-actions-ai': 'GitHub Actions integration that triggers AI agents for reviews, fixes, or issue triage.',
      'acmm:auto-qa-self-tuning': 'Auto-QA with self-tuning — thresholds and rules adapt based on project history.',
      'acmm:public-metrics': 'Publicly visible metrics that show agent contribution quality and project health.',
      'acmm:policy-as-code': 'Policies defined as code (OPA, Kyverno, Conftest) that agents must satisfy.',
      'acmm:reflection-log': 'Reflection logs where agents record what worked, what didn\'t, and what to try differently.',
      'acmm:auto-issue-gen': 'Automated issue generation — agents create issues from detected problems or improvements.',
      'acmm:multi-agent-orchestration': 'Multi-agent orchestration — a dispatcher coordinates multiple specialized agents.',
      'acmm:strategic-dashboard': 'A strategic dashboard showing ACMM progress, agent health, and maturity trends.',
      'acmm:merge-queue': 'A merge queue or equivalent serialized merging that validates agent PRs against the current base before they land on the default branch.',
      'acmm:risk-assessment-config': 'Risk assessment config that gates high-risk changes for human review.',
      'acmm:observability-runbook': 'An observability runbook documenting how to monitor and debug agent operations.',
      'aef:task-traceability': 'Task traceability — every agent action is linked back to a tracked issue or task.',
      'aef:structural-gates': 'CODEOWNERS or boundary files that restrict which paths agents can modify.',
      'aef:session-continuity': 'Agent context files (CLAUDE.md, AGENTS.md, etc.) that provide continuity across tools.',
      'aef:audit-trail': 'An audit trail workflow that logs agent actions for compliance and review.',
      'aef:cross-tool-config': 'Cross-tool agent config so multiple AI tools share the same project conventions.',
      'aef:change-classification': 'Change classification docs that define risk tiers for different types of modifications.'
    };
    let _acmmEvalData = null;
    let _acmmSelectedRepo = null; // null = aggregate view
    const _acmmOpenedIssues = {}; // key: "repo:criterionId" → {number, url, identifier, tracker}
    // Where "Open Issue" files: null = the server's configured default
    // (governor.acmm.issue_tracker), else 'github' | 'linear' chosen in the
    // level dialog's selector. Only offered when the work source is Linear.
    let _acmmTracker = null;


    // ── Config Interaction Helpers ──
    function markDirty(section, key, value) {
      if (!_configState.dirty[section]) _configState.dirty[section] = {};
      _configState.dirty[section][key] = value;
    }

    function toggleConfigSwitch(el, section, key) {
      if (!el || el.classList.contains('disabled') || el.dataset.disabled === 'true') return;
      const isOn = el.classList.toggle('on');
      if (el.hasAttribute('aria-checked')) el.setAttribute('aria-checked', isOn ? 'true' : 'false');
      markDirty(section, key, isOn);
      if (section === 'dashboard' && key === 'experiment_lab') noteExperimentLabToggle(isOn);
      if (section === 'review' && (key === 'fan_out' || key === 'combined_perspectives')) refreshReviewerAgentSummary();
    }

    function markCSVDirty(section, key, value) {
      const arr = String(value || '').split(',').map(s => s.trim()).filter(Boolean);
      markDirty(section, key, arr);
    }

    function markBoolDirty(section, key, value) {
      markDirty(section, key, value === true || value === 'true');
    }

    // Review label gate (Settings → Features → Review Gate → "Only review
    // labelled PRs"): the toggle shows/hides the label input; the saved value
    // is review.require_labels, where an empty list means off.
    function parseReviewRequireLabels(text) {
      const seen = new Set();
      return String(text || '').split(',').map(l => l.trim()).filter(l => {
        if (!l) return false;
        const k = l.toLowerCase();
        if (seen.has(k)) return false;
        seen.add(k);
        return true;
      });
    }
    function toggleReviewRequireLabels(el) {
      if (!el || el.classList.contains('disabled') || el.dataset.disabled === 'true') return;
      const isOn = el.classList.toggle('on');
      const field = document.getElementById('rv-require-labels-field');
      if (field) field.hidden = !isOn;
      const input = document.getElementById('rv-require-labels');
      markDirty('review', 'require_labels', isOn ? parseReviewRequireLabels(input && input.value) : []);
      if (isOn && input) input.focus();
    }
    function markDirtyReviewRequireLabels(text) {
      markDirty('review', 'require_labels', parseReviewRequireLabels(text));
    }

    // Reviewer agents pick-list (Settings → Features → Review Gate →
    // Reviewers, #13460). The checked rows become review.reviewer_agents; an
    // empty selection means "keyword scan", mirroring the server.
    function markDirtyReviewerAgents() {
      const names = Array.from(document.querySelectorAll('#rv-reviewer-agents .rv-reviewer-row'))
        .filter(row => { const cb = row.querySelector('.rv-reviewer-on'); return cb && cb.checked; })
        .map(row => row.dataset.reviewer).filter(Boolean);
      markDirty('review', 'reviewer_agents', names);
      refreshReviewerAgentSummary();
    }

    // reviewerAgentEligibility mirrors dashboardAgentReviewCapable: an
    // available agent is eligible when it is ticked, or when nothing is
    // ticked and it matches the "review" keyword scan.
    function reviewerAgentEligibility(rows) {
      const ticked = rows.filter(r => r.checked);
      const eligible = (ticked.length ? ticked : rows.filter(r => r.keyword)).filter(r => r.available);
      const engines = new Set(eligible.map(r => r.engine).filter(Boolean));
      return { explicit: ticked.length > 0, eligible, engines };
    }

    function refreshReviewerAgentSummary() {
      const root = document.getElementById('rv-reviewer-agents');
      if (!root) return;
      const rows = Array.from(root.querySelectorAll('.rv-reviewer-row')).map(row => {
        const cb = row.querySelector('.rv-reviewer-on');
        return { checked: !!(cb && cb.checked), engine: row.dataset.engine || '', available: row.dataset.available === '1', keyword: row.dataset.keyword === '1' };
      });
      const { explicit, eligible, engines } = reviewerAgentEligibility(rows);
      const isOn = key => { const sw = document.querySelector(`.config-toggle-switch[data-section="review"][data-key="${key}"]`); return !!(sw && sw.classList.contains('on')); };
      const fanOut = isOn('fan_out');
      const combined = isOn('combined_perspectives');
      const summary = root.querySelector('#rv-reviewer-summary');
      if (summary) {
        const n = eligible.length;
        const e = engines.size;
        summary.textContent = `${n} review-capable agent${n === 1 ? '' : 's'} ${explicit ? 'selected' : 'by keyword'} on ${e} engine${e === 1 ? '' : 's'}.`;
      }
      const warn = root.querySelector('#rv-reviewer-engine-warn');
      if (warn) warn.hidden = !(fanOut && eligible.length > 0 && engines.size < 2);
      const note = root.querySelector('#rv-reviewer-combined-note');
      if (note) note.hidden = !(fanOut && combined);
    }

    /* Sentinel dirty-tracking: the security PUT takes a nested `sentinel`
       object with "absent means unchanged" fields, so edits are merged into
       one bag rather than flattened into the security section. */
    function sentinelState() {
      const d = _configState.data || {};
      d.security = d.security || {};
      d.security.sentinel = d.security.sentinel || {};
      return d.security.sentinel;
    }
    function markSentinelDirty(key, value) {
      if (!_configState.dirty.security) _configState.dirty.security = {};
      const bag = _configState.dirty.security.sentinel || {};
      bag[key] = value;
      _configState.dirty.security.sentinel = bag;
      sentinelState()[key] = value;
    }
    function toggleSentinelEnabled(el) {
      if (!el) return;
      const isOn = el.classList.toggle('on');
      markSentinelDirty('enabled', isOn);
      renderConfigTab('Security');
    }
    function toggleSentinelBehavior(el) {
      if (!el || el.classList.contains('disabled')) return;
      const isOn = el.classList.toggle('on');
      const sn = sentinelState();
      sn.behaviors = (sn.behaviors || []).map(b => b.name === el.dataset.rule ? Object.assign({}, b, { enabled: isOn }) : b);
      markSentinelDirty('disabledBehaviors', sn.behaviors.filter(b => !b.enabled).map(b => b.name));
    }
    function toggleSentinelTrustedAuthorsBlock(el) {
      if (!el || el.classList.contains('disabled')) return;
      const isOn = el.classList.toggle('on');
      markSentinelDirty('trustedAuthorsBlock', isOn);
    }
    function resetSentinelPaths() {
      const sn = sentinelState();
      sn.sensitivePaths = (sn.defaultSensitivePaths || []).slice();
      sn.sensitivePathsDefault = true;
      /* An empty list tells the backend to drop the override. */
      markSentinelDirty('sensitivePaths', []);
      renderConfigTab('Security');
    }
    function markSentinelPathsFromText(text) {
      const lines = String(text || '').split('\n').map(l => l.trim()).filter(Boolean);
      /* An emptied box restores defaults rather than disabling the rule. */
      markSentinelDirty('sensitivePaths', lines);
    }

    function toggleContributorPRBaseSync(el) {
      if (!el || el.classList.contains('disabled') || el.dataset.disabled === 'true') return;
      const isOn = el.classList.toggle('on');
      markDirty('review', 'contributor_prs', { base_sync: isOn });
    }

    // markDirtyRecommendations reads the three Recommendations controls and
    // stages them as one object, because the server takes review
    // recommendations as a whole sub-object (an absent key there means "leave
    // the block alone", so a partial object would look like a deliberate
    // clear of the fields it omitted).
    // markDirtyPerspectives reads every perspective row and stages the
    // selection and the prompts as a pair. Selection is sent as the explicit
    // checked list; if every built-in is checked and nothing custom exists,
    // it is sent empty, which the server treats as "all built-ins" — so a hive
    // that never touched this keeps the default even after a save.
    // 'Severity & backlog' (#11088): review.severity and review.backlog are
    // sub-objects on the owner-only PUT /api/config/review. Presets only set
    // block_at; the destination selector offers, and shows the fields of,
    // the active work source only.
    const REVIEW_SEVERITY_PRESETS = [
      { blockAt: 'P2', label: 'Strict (P0–P2 block)' },
      { blockAt: 'P1', label: 'Ship fast (P0–P1 block)' },
      { blockAt: 'P3', label: 'Everything blocks' },
    ];
    const REVIEW_BACKLOG_DESTINATIONS = [
      { value: 'github_issue', label: 'GitHub issue', workSource: '' },
      { value: 'github_project', label: 'GitHub Projects column', workSource: 'github_projects' },
      { value: 'linear', label: 'Linear issue', workSource: 'linear' },
      { value: 'jira', label: 'Jira ticket', workSource: 'jira' },
    ];
    function reviewSeverityState() {
      const d = _configState.data || (_configState.data = {});
      d.review = d.review || {};
      d.review.severity = d.review.severity || {};
      d.review.backlog = d.review.backlog || {};
      return d.review;
    }
    function reviewBacklogDestinationsFor(workSource) {
      const ws = String(workSource || '').toLowerCase();
      return REVIEW_BACKLOG_DESTINATIONS.filter(o => !o.workSource || o.workSource === ws);
    }
    function renderReviewSeverityBacklog(rv) {
      const sev = (rv && rv.severity) || {};
      const bl = (rv && rv.backlog) || {};
      const ws = ((_configState.data || {}).work_source || {}).type || '';
      const canEdit = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const dis = canEdit ? '' : ' disabled';
      const blockAt = sev.block_at || '';
      const commentBelow = sev.comment_below !== false;
      const backlogBelow = sev.backlog_below !== false;
      const dests = reviewBacklogDestinationsFor(ws);
      const dest = dests.some(o => o.value === bl.destination) ? bl.destination : 'github_issue';
      const labels = Array.isArray(bl.labels) ? bl.labels : ['origin/reviewer'];
      const presetBtns = REVIEW_SEVERITY_PRESETS.map(p => `<button type="button" class="hv-btn ${blockAt === p.blockAt ? 'btn-primary' : 'btn-secondary'} btn-sm btn-small" data-action="applyReviewSeverityPreset" data-arg0="${p.blockAt}" aria-pressed="${blockAt === p.blockAt ? 'true' : 'false'}"${dis}>${esc(p.label)}</button>`).join(' ');
      const toggle = (key, on, label, tip) => `<div class="config-toggle mt-3">
              <div class="config-toggle-switch ${on ? 'on' : ''}${canEdit ? '' : ' disabled'}" data-action="toggleReviewSeverityFlag" data-arg0="${key}" data-arg-types="s" role="switch" aria-checked="${on ? 'true' : 'false'}"></div>
              <span class="config-toggle-label">${label} ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', label, `${tip}`)}</span>
            </div>`;
      let destFields = '';
      if (dest === 'github_project') {
        destFields = `<div class="config-field mt-3"><label>Projects column (status option) id ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', 'Projects column (status option) id', `review.backlog.project_column_id: the GitHub Projects v2 status option new backlog items land in. Required for this destination.`)}</label>
              <input type="text" id="review-backlog-project-column" value="${esc(bl.project_column_id || '')}" placeholder="e.g. f75ad846" data-change-action="markDirtyReviewBacklog" data-arg0="project_column_id" data-arg-types="s,w"${dis}></div>`;
      } else if (dest === 'linear') {
        destFields = `<div class="config-field mt-3"><label>Linear state ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', 'Linear state', `review.backlog.linear_state: the workflow state new backlog issues are created in, on the work source&apos;s Linear team. Blank uses the team&apos;s default state.`)}</label>
              <input type="text" id="review-backlog-linear-state" value="${esc(bl.linear_state || '')}" placeholder="Backlog" data-change-action="markDirtyReviewBacklog" data-arg0="linear_state" data-arg-types="s,w"${dis}></div>`;
      } else if (dest === 'jira') {
        destFields = `<div class="config-field mt-3"><label>Jira status ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', 'Jira status', `review.backlog.jira_status: the status new backlog tickets are moved to in the work source&apos;s Jira project. Blank leaves the project&apos;s initial status.`)}</label>
              <input type="text" id="review-backlog-jira-status" value="${esc(bl.jira_status || '')}" placeholder="To Do" data-change-action="markDirtyReviewBacklog" data-arg0="jira_status" data-arg-types="s,w"${dis}></div>`;
      }
      return `<div id="review-severity-backlog">
          <div class="config-subgroup">Severity &amp; backlog ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', 'Severity & backlog', `The blocking line for review findings (review.severity) and where findings below it go (review.backlog). Findings at or above the line block merge; below it they can be posted as non-blocking comments and filed as follow-up work. block_at also sets the external review-bot minimum priority unless classification.review_bots.min_priority is set explicitly. Owner only.`)}</div>
          <div class="config-field">
            <label>Blocking line</label>
            <div id="review-severity-presets">${presetBtns}</div>
            <select id="review-severity-block-at" class="mt-3" data-change-action="applyReviewSeverityPreset" data-arg-types="v"${dis}>
              ${[['', 'Not set (unchanged behaviour)'], ['P1', 'P0–P1 block'], ['P2', 'P0–P2 block'], ['P3', 'P0–P3 block (everything)']].map(([v, l]) => `<option value="${v}"${blockAt === v ? ' selected' : ''}>${l}</option>`).join('')}
            </select>
          </div>
          ${toggle('comment_below', commentBelow, 'Post findings below the line as non-blocking comments', 'review.severity.comment_below (default on): findings below the blocking line are still shown on the PR, marked non-blocking.')}
          ${toggle('backlog_below', backlogBelow, 'File findings below the line to the backlog', 'review.severity.backlog_below (default on): findings below the blocking line become follow-up work in the destination below.')}
          <div class="config-field mt-3">
            <label>Backlog destination ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', 'Backlog destination', `review.backlog.destination. Only destinations for the active work source (${esc(ws || 'github')}) are offered; GitHub issue is always available and is the fallback.`)}</label>
            <select id="review-backlog-destination" data-change-action="setReviewBacklogDestination" data-arg-types="v"${dis}>
              ${dests.map(o => `<option value="${o.value}"${dest === o.value ? ' selected' : ''}>${esc(o.label)}</option>`).join('')}
            </select>
          </div>
          ${destFields}
          <div class="config-field mt-3">
            <label>Backlog labels ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', 'Backlog labels', `review.backlog.labels: comma-separated labels on every backlog item. Default origin/reviewer (the retired from-review and origin/review are accepted and mapped to it).`)}</label>
            <input type="text" id="review-backlog-labels" value="${esc(labels.join(', '))}" placeholder="origin/reviewer" data-change-action="markDirtyReviewBacklogLabels" data-arg-types="v"${dis}>
          </div>
          <div class="config-field mt-3">
            <label>Max backlog items per PR per day ${settingHelpMark('review-swarm', 'blocking-line-reviewseverity-reviewbacklog', 'Max backlog items per PR per day', `review.backlog.max_per_pr_per_day: cap on items filed from one PR in a day. 0 or blank uses the default of 10.`)}</label>
            <input type="number" id="review-backlog-max" min="0" value="${esc(String(bl.max_per_pr_per_day || ''))}" placeholder="10" data-change-action="markDirtyReviewBacklog" data-arg0="max_per_pr_per_day" data-arg-types="s,N"${dis}>
          </div>
        </div>`;
    }
    function rerenderReviewSeverityBacklog() {
      const host = document.getElementById('review-severity-backlog');
      if (host) host.outerHTML = renderReviewSeverityBacklog(reviewSeverityState());
    }
    function markDirtyReviewSeverity(key, value) {
      const rv = reviewSeverityState();
      rv.severity[key] = value;
      markDirty('review', 'severity', Object.assign({}, (_configState.dirty.review || {}).severity, { [key]: value }));
    }
    function applyReviewSeverityPreset(blockAt) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return;
      markDirtyReviewSeverity('block_at', blockAt || '');
      rerenderReviewSeverityBacklog();
    }
    function toggleReviewSeverityFlag(key) {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return;
      const rv = reviewSeverityState();
      markDirtyReviewSeverity(key, rv.severity[key] === false);
      rerenderReviewSeverityBacklog();
    }
    function markDirtyReviewBacklog(key, value) {
      const rv = reviewSeverityState();
      if (key === 'max_per_pr_per_day') value = Number.isFinite(value) && value > 0 ? Math.floor(value) : 0;
      rv.backlog[key] = value;
      markDirty('review', 'backlog', Object.assign({}, (_configState.dirty.review || {}).backlog, { [key]: value }));
    }
    function markDirtyReviewBacklogLabels(value) {
      markDirtyReviewBacklog('labels', String(value || '').split(',').map(s => s.trim()).filter(Boolean));
    }
    function setReviewBacklogDestination(value) {
      markDirtyReviewBacklog('destination', value || 'github_issue');
      rerenderReviewSeverityBacklog();
    }

    // review.reviewers.autoscale (hivecommons/hive#14685) is a sub-object on
    // PUT /api/config/review, sent as reviewer_autoscale with only the edited
    // keys so an absent key leaves the file's value alone.
    const REVIEWER_AUTOSCALE_DEFAULTS = { min: 1, max: 4, target_backlog_per_reviewer: 2, scale_up_cycles: 2, scale_down_cycles: 5, cooldown: '5m', cpu_per_reviewer: 1, mem_per_reviewer_mb: 2048 };
    function reviewerAutoscaleState() {
      const d = _configState.data || (_configState.data = {});
      d.review = d.review || {};
      d.review.reviewers = d.review.reviewers || {};
      d.review.reviewers.autoscale = d.review.reviewers.autoscale || {};
      return d.review.reviewers.autoscale;
    }
    function markDirtyReviewerAutoscale(key, value) {
      const a = reviewerAutoscaleState();
      if (['min', 'max', 'target_backlog_per_reviewer', 'scale_up_cycles', 'scale_down_cycles', 'mem_per_reviewer_mb'].includes(key)) {
        value = Number.isFinite(value) && value > 0 ? Math.floor(value) : 0;
      } else if (key === 'cpu_per_reviewer') {
        value = Number.isFinite(value) && value > 0 ? value : 0;
      }
      a[key] = value;
      markDirty('review', 'reviewer_autoscale', Object.assign({}, (_configState.dirty.review || {}).reviewer_autoscale, { [key]: value }));
    }
    function toggleReviewerAutoscaleEnabled() {
      if (!dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) return;
      const a = reviewerAutoscaleState();
      markDirtyReviewerAutoscale('enabled', !a.enabled);
      const host = document.getElementById('review-reviewer-autoscale');
      if (host) host.outerHTML = renderReviewerAutoscale((_configState.data || {}).review);
    }
    function renderReviewerAutoscale(rv) {
      const a = (rv && rv.reviewers && rv.reviewers.autoscale) || {};
      const canEdit = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const dis = canEdit ? '' : ' disabled';
      const live = (window._lastStatus || {}).reviewerAutoscale;
      const liveLine = live && live.enabled
        ? `<p class="am-trusted-authors-note">Now: <strong>${live.current}</strong> reviewer${live.current === 1 ? '' : 's'} (target ${live.target}, band ${live.min}–${live.max})${live.draining && live.draining.length ? ', draining ' + live.draining.map(esc).join(', ') : ''}; backlog ${live.backlog}${live.oldestWait ? ', oldest ' + esc(live.oldestWait) : ''}${live.lastDecision ? ' — last: ' + esc(reviewerAutoscaleDecisionText(live.lastDecision)) : ''}</p>`
        : '';
      const num = (key, yamlPath, label, help, step) => `<div class="config-field"><label>${label} ${yamlHint(yamlPath)} ${settingHelpMark('review-swarm', 'reviewer-autoscale-reviewreviewersautoscale', label, help)}</label>
              <input type="number" min="0"${step ? ' step="' + step + '"' : ''} value="${esc(String(a[key] || ''))}" placeholder="${esc(String(REVIEWER_AUTOSCALE_DEFAULTS[key]))}" data-change-action="markDirtyReviewerAutoscale" data-arg0="${key}" data-arg-types="s,N"${dis}></div>`;
      const fields = a.enabled ? `
            <div class="config-field-row mt-3">
              ${num('min', 'review.reviewers.autoscale.min', 'Min reviewers', 'The floor of active reviewer replicas. The base reviewer agent is never retired.')}
              ${num('max', 'review.reviewers.autoscale.max', 'Max reviewers', 'The ceiling of active reviewer replicas; capped at the fleet replica limit (5). Each replica is an ordinary reviewer-N agent with its own pane, token and cadence.')}
              ${num('target_backlog_per_reviewer', 'review.reviewers.autoscale.target_backlog_per_reviewer', 'Target backlog per reviewer', 'How many PRs may wait for a reviewer, per active reviewer, before the lane is behind. A PR counts as waiting when it has perspectives to review and every eligible reviewer is busy.')}
            </div>
            <div class="config-field-row mt-3">
              ${num('scale_up_cycles', 'review.reviewers.autoscale.scale_up_cycles', 'Scale-up cycles', 'Consecutive governor cycles the lane must be behind (backlog over target, or the oldest wait past the review-pending stage dwell) before a reviewer is added.')}
              ${num('scale_down_cycles', 'review.reviewers.autoscale.scale_down_cycles', 'Scale-down cycles', 'Consecutive idle cycles (nothing waiting and at least one reviewer free) before the highest replica is drained. A draining reviewer gets no new work and is retired only once its turn ends.')}
              <div class="config-field"><label>Cooldown ${yamlHint('review.reviewers.autoscale.cooldown')} ${settingHelpMark('review-swarm', 'reviewer-autoscale-reviewreviewersautoscale', 'Cooldown', 'Minimum gap between two scaling actions in either direction, e.g. 5m.')}</label>
                <input type="text" value="${esc(a.cooldown || '')}" placeholder="5m" data-change-action="markDirtyReviewerAutoscale" data-arg0="cooldown" data-arg-types="s,w"${dis}></div>
            </div>
            <div class="config-field-row mt-3">
              ${num('cpu_per_reviewer', 'review.reviewers.autoscale.cpu_per_reviewer', 'CPU per reviewer (cores)', 'The CPU one reviewer is assumed to need. A scale-up is refused when the pod cgroup has less than this free; unknown limits (no cgroup) never block.', '0.1')}
              ${num('mem_per_reviewer_mb', 'review.reviewers.autoscale.mem_per_reviewer_mb', 'Memory per reviewer (MiB)', 'The memory one reviewer is assumed to need. A scale-up is refused when the pod cgroup has less than this free.')}
              <div class="config-field"><label>Lane agent ${yamlHint('review.reviewers.autoscale.agent')} ${settingHelpMark('review-swarm', 'reviewer-autoscale-reviewreviewersautoscale', 'Lane agent', 'The base reviewer agent whose replicas scale. Empty picks the first agent with role reviewer, then review.reviewer_agents[0].')}</label>
                <input type="text" value="${esc(a.agent || '')}" placeholder="reviewer" data-change-action="markDirtyReviewerAutoscale" data-arg0="agent" data-arg-types="s,w"${dis}></div>
            </div>` : '';
      return `<div id="review-reviewer-autoscale" class="mt-4">
            <div class="config-toggle">
              <div class="config-toggle-switch ${a.enabled ? 'on' : ''}${canEdit ? '' : ' disabled'}" data-action="toggleReviewerAutoscaleEnabled" role="switch" aria-checked="${a.enabled ? 'true' : 'false'}"></div>
              <span class="config-toggle-label">Scale reviewers with the backlog ${yamlHint('review.reviewers.autoscale.enabled')} ${settingHelpMark('review-swarm', 'reviewer-autoscale-reviewreviewersautoscale', 'Scale reviewers with the backlog', `Off by default. On, the reviewer lane grows from Min to Max replicas while PRs wait for a free reviewer, and drains back when idle. Replicas added this way are ordinary reviewer-N agents (same role, prompt, scoped token, pane and cadence as a configured replica). Scale-ups are refused when the pod cgroup lacks CPU or memory for another reviewer, when the provider budget latch is on or provider headroom is under 10%, or when reviewer kicks are failing to reach the CLI input prompt — a saturated pod, not demand. Each decision is logged as &quot;reviewer autoscale: 2→3 reason=…&quot;. Owner only.`)}</span>
            </div>
            ${liveLine}${fields}
          </div>`;
    }
    function reviewerAutoscaleDecisionText(d) {
      if (!d) return '';
      const when = d.at ? ' at ' + formatDashboardTime(d.at, { hour:'2-digit', minute:'2-digit', timeZoneName:'short' }) : '';
      if (d.action === 'blocked') return `${d.from}→${d.to} blocked by ${d.reason}${when}`;
      if (d.agent) return `${d.from}→${d.to} ${d.action} ${d.agent} (${d.reason})${when}`;
      return `${d.from}→${d.to} ${d.action} (${d.reason})${when}`;
    }

    // review_bots is a sub-object on PUT /api/config/review; only
    // min_priority is editable from the dashboard (logins stay config-only).
    function markDirtyReviewBotsMinPriority(value) {
      markDirty('review', 'review_bots', { min_priority: value });
    }

    function markDirtyTrustedBots() {
      const list = Array.from(document.querySelectorAll('#am-trusted-bots .am-bot-row'))
        .filter(r => { const sw = r.querySelector('.am-bot-switch'); return sw && sw.classList.contains('on'); })
        .map(r => r.dataset.botLogin)
        .filter(Boolean);
      markDirty('auto-merge', 'trusted_bot_authors', list);
    }

    function markDirtyTrustedAuthors() {
      const enabled = document.getElementById('am-trusted-authors-enabled');
      const permission = document.getElementById('am-trusted-authors-require-github-permission');
      const role = document.getElementById('am-trusted-authors-require-role');
      const labels = document.getElementById('am-trusted-authors-exclude-labels');
      const repos = document.getElementById('am-trusted-authors-repos');
      markDirty('auto-merge', 'trusted_authors', {
        enabled: !!(enabled && enabled.classList.contains('on')),
        require_role: role ? role.value : 'merger',
        require_github_permission: !!(permission && permission.classList.contains('on')),
        exclude_labels: labels ? labels.value.split(',').map(s => s.trim()).filter(Boolean) : [],
        repos: repos ? Array.from(repos.selectedOptions).map(o => o.value).filter(Boolean) : []
      });
    }

    // Settings → Features → Auto-merge → Per-repository merge policy. Each
    // control stages its key in full: an empty selection clears the list and
    // a repo with no patterns drops out of auto_merge.human_merge_paths.
    function markDirtyAutoMergeRepoPolicy() {
      const selected = (id) => {
        const el = document.getElementById(id);
        return el ? Array.from(el.selectedOptions).map(o => o.value).filter(Boolean) : [];
      };
      if (this && this.id === 'am-require-current-base') markDirty('auto-merge', 'require_current_base', selected('am-require-current-base'));
      if (this && this.id === 'am-squash-only') markDirty('auto-merge', 'squash_only', selected('am-squash-only'));
      if (this && this.classList && this.classList.contains('am-human-merge-path-input')) {
        const paths = {};
        document.querySelectorAll('#am-human-merge-paths .am-human-merge-path-row').forEach(row => {
          const input = row.querySelector('.am-human-merge-path-input');
          const patterns = splitConfigLines(input ? input.value : '');
          if (row.dataset.repo && patterns.length) paths[row.dataset.repo] = patterns;
        });
        markDirty('auto-merge', 'human_merge_paths', paths);
      }
    }

    function markDirtyContributorLane() {
      const enabled = document.getElementById('am-contributor-lane-enabled');
      const verdict = document.getElementById('am-contributor-lane-require-reviewer-verdict');
      const registry = document.getElementById('am-contributor-lane-registry-tiers-sufficient');
      const heldRuns = document.getElementById('am-contributor-lane-approve-held-runs');
      const workflowApproval = document.getElementById('am-contributor-lane-owner-approval-workflow-fence');
      const soak = document.getElementById('am-contributor-lane-soak-minutes');
      const labels = document.getElementById('am-contributor-lane-exclude-labels');
      const repos = document.getElementById('am-contributor-lane-repos');
      const tiers = document.getElementById('am-contributor-lane-require-tiers');
      const scope = document.getElementById('am-contributor-lane-auto-promotion-scope');
      const soakMinutes = soak ? parseInt(soak.value, 10) : 30;
      markDirty('auto-merge', 'contributor_lane', {
        enabled: !!(enabled && enabled.classList.contains('on')),
        soak_minutes: Number.isFinite(soakMinutes) && soakMinutes >= 0 ? soakMinutes : 30,
        require_reviewer_verdict: !!(verdict && verdict.classList.contains('on')),
        registry_tiers_sufficient: !!(registry && registry.classList.contains('on')),
        approve_held_runs_for_registry_trusted: !!(heldRuns && heldRuns.classList.contains('on')),
        owner_approval_releases_workflow_fence: !!(workflowApproval && workflowApproval.classList.contains('on')),
        auto_promotion_scope: scope && scope.value ? scope.value : 'hive',
        exclude_labels: labels ? labels.value.split(',').map(s => s.trim()).filter(Boolean) : [],
        repos: repos ? Array.from(repos.selectedOptions).map(o => o.value).filter(Boolean) : [],
        require_tiers: tiers ? tiers.value.split(',').map(s => s.trim().toLowerCase()).filter(Boolean) : []
      });
    }

    function markDirtyPerspectives() {
      const rows = Array.from(document.querySelectorAll('#rv-perspectives .rv-perspective'));
      const builtin = ((_configState.data || {}).review || {}).builtin_perspectives || [];
      const builtinNames = builtin.map(b => b.name);
      const selected = [], prompts = {};
      for (const row of rows) {
        const name = row.dataset.perspective;
        const on = row.querySelector('.rv-perspective-on');
        const ta = row.querySelector('.rv-perspective-focus');
        if (on && on.checked) selected.push(name);
        const text = ta ? ta.value.trim() : '';
        if (text) prompts[name] = text;
      }
      const allBuiltinOnly = selected.length === builtinNames.length && builtinNames.every(n => selected.includes(n));
      markDirty('review', 'perspectives', allBuiltinOnly ? [] : selected);
      markDirty('review', 'perspective_prompts', prompts);
    }

    function markDirtyRecommendations() {
      const sw = document.getElementById('rec-enabled-switch');
      const reposEl = document.getElementById('rec-repos-input');
      const minEl = document.getElementById('rec-minready-input');
      markDirty('review', 'recommendations', {
        enabled: !!(sw && sw.classList.contains('on')),
        repos: reposEl ? reposEl.value.split(',').map(s => s.trim()).filter(Boolean) : [],
        min_ready_to_open: minEl ? (Number(minEl.value) || 0) : 0
      });
    }

    // loadReviewOutcomes fetches the review-outcome ledger summary and
    // renders the reviewed-vs-unreviewed comparison in place. It is the
    // queue-reduction evidence for the review gate: same cohort split the
    // metrics endpoint publishes, read here on demand.
    async function loadReviewOutcomes() {
      const out = document.getElementById('review-outcomes-summary');
      if (!out) return;
      out.textContent = 'Loading…';
      try {
        const res = await fetch('/api/review/outcomes?days=30');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const d = await res.json();
        const total = (d.reviewed?.prs || 0) + (d.unreviewed?.prs || 0);
        if (!total) {
          out.textContent = 'No outcomes recorded yet — the ledger fills one row per open PR per eval cycle and resolves them as they merge or close, so give it a day.';
          return;
        }
        const hrs = v => (v && v > 0) ? `${v.toFixed(1)}h` : '—';
        const pct = (n, dnm) => dnm ? `${Math.round(100 * n / dnm)}%` : '—';
        const row = (label, g) => `<tr><td style="padding:var(--sp-1) var(--sp-4) var(--sp-1) var(--sp-0)"><strong>${esc(label)}</strong></td><td class="py-1-px-4">${g.prs}</td><td class="py-1-px-4">${g.merged} (${pct(g.merged, g.prs)})</td><td class="py-1-px-4">${g.closed}</td><td class="py-1-px-4">${g.open}</td><td class="py-1-px-4">${hrs(g.median_hours_seen_to_merge)}</td><td class="py-1-px-4">${pct(g.merged_within_72h, g.prs)}</td></tr>`;
        let html = `<table style="border-collapse:collapse;margin-top:var(--sp-2)"><thead><tr style="text-align:left"><th></th><th class="py-1-px-4">PRs</th><th class="py-1-px-4">merged</th><th class="py-1-px-4">closed</th><th class="py-1-px-4">open</th><th class="py-1-px-4">median seen→merge</th><th class="py-1-px-4">merged ≤72h</th></tr></thead><tbody>`;
        html += row('Reviewed by hive', d.reviewed || {});
        html += row('Not reviewed (control)', d.unreviewed || {});
        html += '</tbody></table>';
        const parts = [];
        if (d.reviewed?.median_hours_review_to_merge > 0) parts.push(`median review→merge ${hrs(d.reviewed.median_hours_review_to_merge)}`);
        if (d.agent_authored?.prs) parts.push(`agent-authored reviewed: ${d.agent_authored.merged}/${d.agent_authored.prs} merged`);
        if (d.human_authored?.prs) parts.push(`human-authored reviewed: ${d.human_authored.merged}/${d.human_authored.prs} merged`);
        const snaps = Array.isArray(d.snapshots) ? d.snapshots : [];
        if (snaps.length >= 2) {
          const first = snaps[0], last = snaps[snaps.length - 1];
          parts.push(`open queue ${first.open} → ${last.open} over ${snaps.length} days`);
        }
        if (parts.length) html += `<div class="mt-2">${esc(parts.join(' · '))}</div>`;
        html += `<div class="mt-2">Window: PRs first seen in the last ${d.window_days} days. Reviewed means the hive posted a review before the PR merged or closed; a review after the fact counts as control.</div>`;
        out.innerHTML = html;
      } catch (e) {
        out.textContent = `Could not load outcomes: ${e.message}`;
      }
    }

    // loadConvergenceSoak fetches the owner-only fixed-commit soak telemetry
    // (#4263) and renders a per-(commit, mode) summary so an operator can
    // compare off/shadow/enforce treatments of the same workload at a glance.
    async function loadConvergenceSoak() {
      const out = document.getElementById('convergence-soak-summary');
      if (!out) return;
      out.textContent = 'Loading…';
      try {
        const res = await fetch('/api/convergence/soak');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = await res.json();
        const entries = Array.isArray(data.entries) ? data.entries : [];
        if (!entries.length) {
          out.textContent = data.mode === 'off'
            ? 'No soak telemetry: mode is off, which records no evidence by design. Switch to shadow to start collecting it.'
            : 'No soak telemetry recorded yet — it accumulates one row per eval cycle, so give it a few cycles.';
          return;
        }
        const groups = {};
        for (const e of entries) {
          const k = `${e.commit || '?'} / ${e.mode || '?'} (gen ${e.generation || 0})`;
          if (!groups[k]) groups[k] = { passes: 0, admitted: 0, blocked: 0, unknown: 0, differ: 0, latency: 0 };
          const g = groups[k];
          g.passes++; g.admitted += e.admitted || 0; g.blocked += e.blocked || 0;
          g.unknown += e.unknown || 0; g.differ += e.would_differ ? 1 : 0;
          g.latency += e.decision_latency_ms || 0;
        }
        const rows = Object.entries(groups).map(([k, g]) =>
          `<div><strong>${esc(k)}</strong>: ${g.passes} passes, ${g.admitted} admitted, ${g.blocked} blocked, ${g.unknown} unknown, ${g.differ} would-differ, avg latency ${(g.latency / g.passes).toFixed(1)}ms</div>`
        );
        // Promotion hint (#7260). The whole point of shadow is to answer one
        // question -- "would enforcing have changed anything?" -- so answer it
        // directly instead of leaving the operator to infer it from counts.
        // Only offered for a FULL ring: a handful of clean passes is not
        // evidence, and this must never talk someone into enforce early.
        const totalPasses = entries.length;
        const totalDiffer = entries.reduce((n, e) => n + (e.would_differ ? 1 : 0), 0);
        const maxEntries = data.max_entries || 0;
        if (data.mode === 'shadow') {
          if (totalDiffer > 0) {
            rows.push(`<div class="mt-2">Enforcement would have differed on <strong>${totalDiffer}</strong> of ${totalPasses} passes. Review the withheld candidates in the log before switching to enforce.</div>`);
          } else if (maxEntries > 0 && totalPasses >= maxEntries) {
            rows.push('<div class="mt-2 text-success">Safe to enforce: a full soak ring with no pass where enforcement would have differed.</div>');
          } else {
            rows.push(`<div class="mt-2">No pass would have differed so far (${totalPasses} of ${maxEntries || '?'} rows). Let the ring fill before promoting to enforce.</div>`);
          }
        }
        out.innerHTML = rows.join('');
      } catch (err) {
        out.textContent = `Failed to load soak telemetry: ${err.message}`;
      }
    }

    // ── Upstream watch editor + divergence view (#9969, #11211) ─────────────
    // loadUpstreamWatch paints the editor (on/off, poll interval, add fork,
    // per-fork settings) and the per-repo divergence table from
    // /api/upstream-watch: upstream followed, watermark and last run,
    // surfaced/ported/dismissed counts, and the recent upstream refs with their
    // fork issue. Each recent PR row offers "port this" — the upstream patch
    // the fixer agent is handed when it picks the issue up. Edits go to the
    // owner-only PUT /api/config/upstream-watch.
    async function loadUpstreamWatch() {
      const out = document.getElementById('upstream-watch-panel');
      if (!out) return;
      out.textContent = 'Loading…';
      try {
        const res = await fetch('/api/upstream-watch');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        const data = await res.json();
        const rows = [];
        const repos = Array.isArray(data.repos) ? data.repos : [];
        rows.push(upstreamWatchControls(data, repos));
        if (!data.configured) {
          rows.push('<div class="mt-2">No forks watched yet — pick a repo under <strong>Add fork</strong> to start following its upstream (see docs/upstream-watch.md).</div>');
          out.innerHTML = rows.join('');
          return;
        }
        if (!data.enabled) rows.push('<div class="text-warn">Configured but switched off — turn the watch on above to resume polling. The figures below are the last recorded state.</div>');
        if (data.state_error) rows.push(`<div class="text-danger">State unreadable: ${esc(data.state_error)}</div>`);
        const when = t => t ? formatDashboardDateTime(t) : '—';
        for (const repo of repos) {
          rows.push(`<div class="mt-2"><strong>${esc(repo.repo)}</strong> follows ${repo.upstream ? `<code>${esc(repo.upstream)}</code>` : '<span class="text-warn">an unresolved upstream</span>'}</div>`);
          rows.push(upstreamWatchRepoEditor(repo));
          rows.push(`<div>Watermark ${esc(when(repo.watermark))} · last run ${esc(when(repo.last_run_at))} · surfaced ${repo.surfaced || 0} · ported ${repo.ported || 0} · dismissed ${repo.dismissed || 0} · skipped ${repo.skipped || 0}</div>`);
          const recent = Array.isArray(repo.recent) ? repo.recent : [];
          if (!recent.length) {
            rows.push('<div>No upstream items recorded yet.</div>');
            continue;
          }
          const cells = recent.map(it => {
            const ref = it.upstream_url ? `<a class="text-accent" href="${esc(it.upstream_url)}" target="_blank" rel="noopener">${esc(it.upstream_ref || it.ref)}</a>` : esc(it.upstream_ref || it.ref);
            const issue = it.issue_number ? (it.issue_url ? `<a class="text-accent" href="${esc(it.issue_url)}" target="_blank" rel="noopener">#${it.issue_number}</a>` : `#${it.issue_number}`) : '—';
            const port = it.diff_url ? `<a class="text-accent" href="${esc(it.diff_url)}" target="_blank" rel="noopener">port this</a>` : '—';
            return `<tr><td>${ref}</td><td>${issue}</td><td>${esc(it.state || '')}</td><td>${esc(when(it.recorded_at))}</td><td>${port}</td></tr>`;
          }).join('');
          rows.push(`<table class="hv-upstream-watch-table"><thead><tr><th>Upstream</th><th>Fork issue</th><th>State</th><th>Recorded</th><th>Patch</th></tr></thead><tbody>${cells}</tbody></table>`);
        }
        out.innerHTML = rows.join('');
      } catch (err) {
        out.textContent = `Failed to load upstream watch: ${err.message}`;
      }
    }

    // upstreamWatchBare drops an org prefix so a YAML key spelled "org/repo"
    // and the bare project.repos entry compare equal.
    function upstreamWatchBare(name) {
      const s = String(name || '');
      return s.includes('/') ? s.slice(s.lastIndexOf('/') + 1).toLowerCase() : s.toLowerCase();
    }

    // upstreamWatchControls renders the global switch, the poll interval and
    // the "Add fork" picker. The picker lists only project.repos entries not
    // already watched, because the server rejects any other key.
    function upstreamWatchControls(data, repos) {
      const interval = String(data.interval || '').replace(/(\D)0s$/, '$1').replace(/(\D)0m$/, '$1');
      const watched = new Set(repos.map(r => upstreamWatchBare(r.repo)));
      const addable = (Array.isArray(data.project_repos) ? data.project_repos : []).filter(r => !watched.has(upstreamWatchBare(r)));
      const options = addable.map(r => `<option value="${esc(r)}">${esc(r)}</option>`).join('');
      return `<div class="config-toggle mt-2">
          <div class="config-toggle-switch ${data.enabled ? 'on' : ''}" data-action="upstreamWatchToggle"></div>
          <span class="config-toggle-label">Watch enabled</span>
        </div>
        <div class="config-field mt-2">
          <label for="upstream-watch-interval">Poll interval</label>
          <input id="upstream-watch-interval" type="text" value="${esc(interval)}" placeholder="6h" title="How often the watch polls, e.g. 6h or 30m. Blank = 6h.">
          <button class="hv-btn btn-secondary btn" data-action="upstreamWatchSaveInterval">Save interval</button>
        </div>
        <div class="config-field mt-2">
          <label for="upstream-watch-add">Add fork</label>
          ${addable.length
            ? `<select id="upstream-watch-add" class="hv-btn btn-secondary btn">${options}</select>
          <button class="hv-btn btn-secondary btn" data-action="upstreamWatchAddFork">Add fork</button>`
            : '<span class="text-muted">Every repo in this hive is already watched — add repos in the Repos tab first.</span>'}
        </div>`;
    }

    // upstreamWatchRepoEditor renders one fork's settings. A blank upstream
    // means "resolve from the GitHub fork parent", which the watch already does.
    function upstreamWatchRepoEditor(repo) {
      const c = repo.config || {};
      const sources = Array.isArray(c.sources) && c.sources.length ? c.sources : ['releases', 'prs'];
      const labels = Array.isArray(c.pr_labels) ? c.pr_labels.join(', ') : '';
      const box = (v, label) => `<label><input type="checkbox" data-uw-field="source" value="${v}" ${sources.includes(v) ? 'checked' : ''}> ${label}</label>`;
      return `<div class="hv-upstream-watch-edit config-field" data-repo="${esc(repo.repo)}">
          <label>Upstream <input type="text" data-uw-field="upstream" value="${esc(c.upstream || '')}" placeholder="fork parent" title="owner/repo this fork follows. Blank = the GitHub fork parent."></label>
          ${box('releases', 'Releases')} ${box('prs', 'Merged PRs')}
          <label>PR labels <input type="text" data-uw-field="pr_labels" value="${esc(labels)}" placeholder="all merged PRs" title="Comma-separated. Only merged PRs carrying one of these labels are surfaced. Blank = all."></label>
          <label>Max issues per run <input type="number" min="0" data-uw-field="max_issues_per_run" value="${c.max_issues_per_run || 0}" title="0 = no cap"></label>
          <label>Issue label <input type="text" data-uw-field="label" value="${esc(c.label || '')}" placeholder="upstream/port"></label>
          <button class="hv-btn btn-secondary btn" data-action="upstreamWatchSaveRepo" data-repo="${esc(repo.repo)}">Save</button>
          <button class="hv-btn btn-secondary btn" data-action="upstreamWatchRemoveRepo" data-repo="${esc(repo.repo)}">Remove</button>
        </div>`;
    }

    // putUpstreamWatch sends one partial edit; absent keys stay unchanged
    // server-side. The panel reloads on success so it always shows what was
    // persisted, and on failure so a half-typed edit is not mistaken for saved.
    async function putUpstreamWatch(body, okMsg) {
      try {
        const res = await fetch('/api/config/upstream-watch', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        const data = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(data.error || `HTTP ${res.status}`);
        showToast(okMsg, 'success');
      } catch (err) {
        showToast(`Upstream watch not saved: ${err.message}`, 'error');
      }
      loadUpstreamWatch();
    }

    function upstreamWatchToggle(el) {
      if (!el) return;
      const on = !el.classList.contains('on');
      putUpstreamWatch({ enabled: on }, on ? 'Upstream watch enabled' : 'Upstream watch disabled');
    }

    function upstreamWatchSaveInterval() {
      const input = document.getElementById('upstream-watch-interval');
      putUpstreamWatch({ interval: input ? input.value.trim() : '' }, 'Upstream watch interval saved');
    }

    function upstreamWatchAddFork() {
      const sel = document.getElementById('upstream-watch-add');
      if (!sel || !sel.value) return;
      putUpstreamWatch({ repos: { [sel.value]: {} } }, `Now watching ${sel.value}`);
    }

    function upstreamWatchSaveRepo(el) {
      const row = el && el.closest('.hv-upstream-watch-edit');
      const repo = el && el.dataset.repo;
      if (!row || !repo) return;
      const field = name => row.querySelector(`[data-uw-field="${name}"]`);
      const sources = Array.from(row.querySelectorAll('[data-uw-field="source"]')).filter(b => b.checked).map(b => b.value);
      if (!sources.length) {
        showToast('Pick at least one source (releases or merged PRs)', 'error');
        return;
      }
      const cap = parseInt(field('max_issues_per_run').value, 10);
      putUpstreamWatch({ repos: { [repo]: {
        upstream: field('upstream').value.trim(),
        sources,
        pr_labels: field('pr_labels').value.split(',').map(v => v.trim()).filter(Boolean),
        max_issues_per_run: Number.isFinite(cap) ? cap : 0,
        label: field('label').value.trim(),
      } } }, `Saved upstream watch for ${repo}`);
    }

    async function upstreamWatchRemoveRepo(el) {
      const repo = el && el.dataset.repo;
      if (!repo) return;
      if (!await hiveConfirm({ title: `Stop watching ${repo}?`, message: 'The watch stops polling its upstream. Recorded state and issues already filed are kept.', confirmLabel: 'Stop watching', danger: true })) return;
      putUpstreamWatch({ repos: { [repo]: null } }, `Stopped watching ${repo}`);
    }


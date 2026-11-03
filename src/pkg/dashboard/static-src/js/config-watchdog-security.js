    // ── Watchdog activity strip (#7254) ──────────────────────────────────────
    // "Review the watchdog-*-observed entries in the Audit Log, then switch to
    // Heal" is only a data-driven decision if the data is on the page. The
    // strip under the mode banner shows how many actions the watchdog took
    // (heal) or WOULD have taken (observe) in the trailing 30 days, a daily
    // histogram of them, the current liveness verdict per probed agent, and —
    // in observe — whether anything in the window argues against promotion.
    //
    // It is filled in place (loadConvergenceSoak pattern) rather than by
    // re-rendering the tab, so a fetch landing after the operator has touched
    // the mode select never repaints their unsaved edit away.
    let _watchdogActivity = null;      // last GET /api/watchdog/activity body
    let _watchdogActivityAt = 0;       // epoch ms of that fetch; 0 = never / invalidated
    let _watchdogActivityErr = '';     // last fetch failure, '' when fine
    let _watchdogActivityLoading = false;
    const WATCHDOG_ACTIVITY_DAYS = 30;
    const WATCHDOG_ACTIVITY_TTL_MS = 60000;

    // Element ids the shell renders and fillWatchdogActivity targets. Named
    // once so the static-HTML guard and the fill code cannot drift apart.
    const WATCHDOG_ACTIVITY_IDS = {
      strip: 'watchdog-activity',
      count: 'watchdog-activity-count',
      spark: 'watchdog-activity-spark',
      actions: 'watchdog-activity-actions',
      liveness: 'watchdog-liveness',
      hint: 'watchdog-promotion-hint',
    };

    function invalidateWatchdogActivity() { _watchdogActivityAt = 0; }

    // renderWatchdogActivityShell is the synchronous placeholder: the strip's
    // frame with its element ids, awaiting the fetch. The mode is NOT read from
    // config here — the readout reports the mode actually in force (kill
    // switch applied), which is the one the headline must describe.
    function renderWatchdogActivityShell() {
      const I = WATCHDOG_ACTIVITY_IDS;
      return `
        <div id="${I.strip}" style="border:1px solid var(--line);border-radius:var(--r);padding:10px 12px;margin-bottom:var(--sp-5)">
          <div style="display:flex;align-items:baseline;justify-content:space-between;gap:var(--sp-4);flex-wrap:wrap">
            <div style="font-size:0.72rem;font-weight:600;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted)">Watchdog activity · last ${WATCHDOG_ACTIVITY_DAYS} d</div>
            <div id="${I.spark}"></div>
          </div>
          <div id="${I.count}" style="font-size:0.85rem;margin-top:var(--sp-2);color:var(--muted)">Loading…</div>
          <div id="${I.actions}" style="font-size:0.7rem;margin-top:var(--sp-2)"></div>
          <div class="mt-4" id="${I.liveness}"></div>
        </div>`;
    }

    // loadWatchdogActivity fetches the readout when the Health tab opens and
    // fills the strip once it lands. Cached for a minute: every re-render of
    // the tab calls this, and the strip is a file scan server-side.
    async function loadWatchdogActivity() {
      const fresh = _watchdogActivityAt && Date.now() - _watchdogActivityAt < WATCHDOG_ACTIVITY_TTL_MS;
      if (fresh) { fillWatchdogActivity(); return; }
      if (_watchdogActivityLoading) return;
      _watchdogActivityLoading = true;
      try {
        const res = await fetch(`/api/watchdog/activity?days=${WATCHDOG_ACTIVITY_DAYS}`);
        if (!res.ok) throw new Error(res.status === 403 ? 'read-write access is required to see watchdog activity' : `HTTP ${res.status}`);
        _watchdogActivity = await res.json();
        _watchdogActivityErr = '';
      } catch (err) {
        _watchdogActivity = null;
        _watchdogActivityErr = err && err.message ? err.message : String(err);
      } finally {
        _watchdogActivityAt = Date.now();
        _watchdogActivityLoading = false;
      }
      fillWatchdogActivity();
    }

    // Audit action short names (the `watchdog-` prefix stripped), as the API
    // keys byAction. Base names carry severity; the `-observed` twin of each
    // is the same decision, not taken.
    const WATCHDOG_ACTION_LABELS = {
      'restart': 'restart',
      'crashloop-pause': 'crash-loop pause',
      'giveup': 'give-up',
      'healthy-reset': 'healthy reset',
    };
    function watchdogActionBase(key) { return String(key).replace(/-observed$/, ''); }
    function watchdogActionObserved(key) { return /-observed$/.test(String(key)); }
    function watchdogActionSevere(key) {
      const b = watchdogActionBase(key);
      return b === 'crashloop-pause' || b === 'giveup';
    }
    // Colour by severity (#7254): observed = muted, restart = amber,
    // pause/give-up = red. A pause the watchdog only WOULD have taken is still
    // red — it is the verdict that decides promotion, taken or not.
    function watchdogActionColor(key) {
      if (watchdogActionSevere(key)) return 'var(--red)';
      if (watchdogActionObserved(key)) return 'var(--muted)';
      if (watchdogActionBase(key) === 'restart') return 'var(--yellow)';
      return 'var(--muted)';
    }
    // The strip's overall colour is its worst action's colour.
    function watchdogActivityColor(a) {
      const keys = Object.keys(a.byAction || {}).filter(k => (a.byAction[k] || 0) > 0);
      if (keys.some(watchdogActionSevere)) return 'var(--red)';
      if (keys.some(k => !watchdogActionObserved(k) && watchdogActionBase(k) === 'restart')) return 'var(--yellow)';
      return 'var(--muted)';
    }

    // watchdogActivitySparkSvg draws the daily histogram through the existing
    // axis sparkline (fact/cost pattern) so it reads like every other 30-day
    // trend on the dashboard. axisSparkSvg is used rather than sparkSvg
    // because sparkSvg carries the last non-zero value forward through zero
    // days — right for a gauge, wrong for a count of events where a zero day
    // IS the news.
    function watchdogActivitySparkSvg(a) {
      const daily = Array.isArray(a.daily) ? a.daily : [];
      if (daily.length < 2) return '';
      const color = watchdogActivityColor(a);
      const verb = a.acting ? 'taken' : 'would have been taken';
      return axisSparkSvg(daily, d => d.count || 0, d => Date.parse(d.date + 'T00:00:00Z'), fmtSparkVal, color,
        `Watchdog actions per day (last ${a.days} d) — actions ${verb}; vertical axis: actions/day, horizontal axis: date`);
    }

    // watchdogLivenessTable renders the per-agent verdict: the pane class the
    // reconciler last saw (or crash-loop when it paused the agent), how long
    // the agent has been in it, and the two other condition axes.
    function watchdogLivenessTable(agents) {
      if (!Array.isArray(agents) || !agents.length) {
        return `<div style="font-size:0.7rem;color:var(--muted)">No agent has been probed yet — liveness appears after the watchdog's first sweep.</div>`;
      }
      const stateColor = s =>
        s === 'ready' ? 'var(--ok, var(--green))'
        : s === 'crash-loop' ? 'var(--red)'
        : (s === 'shell-prompt' || s === 'stuck-overlay' || s === 'no-output' || s === 'no-session') ? 'var(--yellow)'
        : 'var(--muted)';
      const axis = (label, v) => v
        ? `<span title="${esc(label)}: ${esc(v)}" style="color:${v === 'True' ? 'var(--ok, var(--green))' : v === 'False' ? 'var(--danger, var(--red))' : 'var(--muted)'};margin-left:var(--sp-3)">${v === 'True' ? '✓' : v === 'False' ? '✕' : '?'} ${esc(label)}</span>`
        : '';
      const rows = agents.map(ag => {
        const since = ag.since ? relativeAge(ag.since) : '';
        const sinceCell = since ? `<span class="text-muted" title="${esc(ag.since)}">for ${esc(since.replace(/ ago$/, ''))}</span>` : '';
        const paused = ag.paused && ag.state !== 'crash-loop' ? `<span style="color:var(--muted);margin-left:var(--sp-3)">(paused)</span>` : '';
        return `<div style="display:flex;gap:10px;align-items:baseline;flex-wrap:wrap;padding:var(--sp-1) var(--sp-0)" title="${esc(ag.message || '')}">
            <span style="min-width:90px;font-weight:600">${esc(ag.name)}</span>
            <span style="color:${stateColor(ag.state)};font-family:var(--font-mono)">${esc(ag.state || 'unknown')}</span>
            ${sinceCell}${paused}
            <span>${axis('auth', ag.authenticated)}${axis('producing', ag.producing)}</span>
          </div>`;
      }).join('');
      return `<div style="font-size:0.72rem;color:var(--muted);margin-bottom:var(--sp-1)">Per-agent liveness (current watchdog verdict)</div><div style="font-size:0.72rem">${rows}</div>`;
    }

    // watchdogPromotionHintHTML is the Observe → Heal verdict beside the mode
    // select. In observe: "Safe to promote" or the reason it is not. In heal:
    // when the watchdog last acted, so a healing hive that has done nothing for
    // weeks is visibly quiet rather than silently trusted.
    function watchdogPromotionHintHTML(a) {
      if (a.mode === 'observe' && a.promotion) {
        const ok = !!a.promotion.safe;
        return `<span style="color:${ok ? 'var(--ok, var(--green))' : 'var(--yellow)'};font-weight:600">${ok ? 'Safe to promote to Heal' : 'Not yet safe to promote'}</span>
          <span class="text-muted"> — ${esc(a.promotion.reason || '')}</span>`;
      }
      if (a.mode === 'heal') {
        return a.lastActionAt
          ? `<span class="text-muted">Last action: <span title="${esc(a.lastActionAt)}">${esc(WATCHDOG_ACTION_LABELS[a.lastAction] || a.lastAction || 'action')} ${esc(relativeAge(a.lastActionAt))}</span></span>`
          : `<span class="text-muted">No action taken in ${a.days} d.</span>`;
      }
      return '';
    }

    // fillWatchdogActivity paints the fetched readout into the shell. A
    // failed fetch says so in the strip; it never leaves "Loading…" standing.
    function fillWatchdogActivity() {
      const I = WATCHDOG_ACTIVITY_IDS;
      const strip = document.getElementById(I.strip);
      if (!strip) return;
      const count = document.getElementById(I.count);
      const spark = document.getElementById(I.spark);
      const actions = document.getElementById(I.actions);
      const liveness = document.getElementById(I.liveness);
      const hint = document.getElementById(I.hint);
      const a = _watchdogActivity;
      if (!a) {
        if (count) count.innerHTML = `<span class="text-muted">Watchdog activity unavailable${_watchdogActivityErr ? ' — ' + esc(_watchdogActivityErr) : ''}.</span>`;
        if (spark) spark.innerHTML = '';
        if (actions) actions.innerHTML = '';
        if (liveness) liveness.innerHTML = '';
        if (hint) hint.innerHTML = '';
        return;
      }
      const total = a.total || 0;
      const color = total ? watchdogActivityColor(a) : 'var(--text)';
      const headline = a.mode === 'off'
        ? `Watchdog is off — nothing is being observed.`
        : a.acting
          ? `<strong style="color:${color};font-size:1.1rem">${total}</strong> action${total === 1 ? '' : 's'} taken by the watchdog in ${a.days} d`
          : `<strong style="color:${color};font-size:1.1rem">${total}</strong> action${total === 1 ? '' : 's'} the watchdog WOULD have taken in ${a.days} d`;
      if (count) count.innerHTML = `<span class="text-default">${headline}</span>`;
      if (spark) spark.innerHTML = watchdogActivitySparkSvg(a);
      if (actions) {
        const keys = Object.keys(a.byAction || {}).filter(k => (a.byAction[k] || 0) > 0)
          .sort((x, y) => (watchdogActionSevere(y) - watchdogActionSevere(x)) || (a.byAction[y] - a.byAction[x]));
        actions.innerHTML = keys.length
          ? keys.map(k => {
              const label = (WATCHDOG_ACTION_LABELS[watchdogActionBase(k)] || watchdogActionBase(k)) + (watchdogActionObserved(k) ? ' (observed)' : '');
              return `<span style="color:${watchdogActionColor(k)};margin-right:10px;white-space:nowrap" title="audit action watchdog-${esc(k)}">${a.byAction[k]} ${esc(label)}</span>`;
            }).join('')
          : `<span class="text-muted">No <code>watchdog-*</code> entries in the Audit Log for this window.</span>`;
      }
      if (liveness) liveness.innerHTML = watchdogLivenessTable(a.agents);
      if (hint) hint.innerHTML = watchdogPromotionHintHTML(a);
    }

    // Escalation breaker section on the Health tab. The config lives at the
    // TOP level of hive.yaml (Config.Escalation, not GovernorConfig), so it is
    // loaded from and saved to /api/config/escalation — see loadEscalationConfig
    // and the escalation special-case in saveConfig. The Disabled flag is shown
    // inverted: the toggle reads "enabled" and is ON when disabled=false.
    function renderEscalationSection(e) {
      if (!e) return `
        <h4 style="margin:18px 0 8px">Escalation Breaker</h4>
        <div class="fs-sm text-muted">Loading…</div>`;
      const enabled = !e.disabled;
      const tip = 'When an agent hits this many consecutive red attempts, Hive escalates to a human. Disable only in test environments.';
      return `
        <h4 style="margin:18px 0 8px">Escalation Breaker</h4>
        <div class="config-toggle">
          <div class="config-toggle-switch ${enabled ? 'on' : ''}" data-action="toggleEscalationEnabled"></div>
          <span class="config-toggle-label">Escalation breaker enabled ${settingHelpMark('settings', 'health', 'Escalation breaker enabled', `${tip}`)}</span>
        </div>
        <div class="config-field-row">
          <div class="config-field"><label>Red-attempt threshold ${yamlHint('escalation.threshold')} ${settingHelpMark('settings', 'health', 'Red-attempt threshold', `${tip}`)}</label><input type="number" min="0" placeholder="3" value="${e.threshold || ''}" data-change-action="markDirty" data-arg0="escalation" data-arg1="threshold" data-arg-types="s,s,N"></div>
        </div>`;
    }

    // watchdogConditionsRow renders the watchdog's OBSERVED truth for one agent
    // (RFC #4665): Ready / Authenticated / Producing, each with the reason
    // behind it. This is deliberately on the agent card rather than in the
    // Health tab — the condition set belongs next to the agent it describes,
    // while the watchdog's SETTINGS belong in Health.
    //
    // In observe mode the watchdog takes no action, so a False condition here
    // describes something it did NOT fix. The row says so explicitly: a reader
    // must never see "crash-looping" and conclude the agent was paused.
    function watchdogConditionsRow(a) {
      const conds = a.conditions || [];
      if (!conds.length) return '';
      const observing = a.watchdogMode === 'observe';

      const chips = conds.map(c => {
        const status = c.status || 'Unknown';
        // Colour by status, not by condition type: True is good for all three.
        const color = status === 'True' ? 'var(--ok, var(--accent))'
          : status === 'False' ? 'var(--danger)'
          : 'var(--muted)';
        const mark = status === 'True' ? '✓' : status === 'False' ? '✕' : '?';
        const tip = [c.type, status, c.reason, c.message].filter(Boolean).join(' — ');
        return `<span style="color:${color};margin-right:var(--sp-4);white-space:nowrap" title="${esc(tip)}">${mark} ${esc(c.type || '')}</span>`;
      }).join('');

      const notActedOn = observing && conds.some(c => c.status === 'False');
      const note = notActedOn
        ? `<div class="fs-xs text-muted mt-1">observe mode — reported only, no restart or pause was performed</div>`
        : '';

      const label = observing ? 'observed' : 'watchdog';
      const labelTip = observing
        ? 'What the watchdog observed. It is in observe mode, so it reported these and took no action.'
        : 'What the watchdog observed. It is in heal mode, so it acts on these.';

      return `<div class="agent-field"><span class="label" title="${esc(labelTip)}">${esc(label)}</span><span class="value">${chips}${note}</span></div>`;
    }

    // WATCHDOG_SAVE_KEYS maps the dirty keys the Watchdog section writes (it
    // renders inside the Health tab, so its keys live in the health bag) to the
    // field names /api/config/governor/watchdog expects.
    const WATCHDOG_SAVE_KEYS = {
      watchdogMode: 'mode',
      watchdogProbeIntervalS: 'probeIntervalS',
      watchdogCrashLoopAfter: 'crashLoopAfter',
      watchdogHealthyReset: 'healthyReset',
    };

    function hasWatchdogKeys(values) {
      return Object.keys(values || {}).some(k => k in WATCHDOG_SAVE_KEYS);
    }

    // splitWatchdogKeys separates one dirty health bag into the watchdog
    // payload and whatever genuinely belongs to the health endpoint.
    function splitWatchdogKeys(values) {
      const watchdog = {};
      const health = {};
      for (const [k, v] of Object.entries(values || {})) {
        if (k in WATCHDOG_SAVE_KEYS) watchdog[WATCHDOG_SAVE_KEYS[k]] = v;
        else health[k] = v;
      }
      return { watchdog, health };
    }

    // toggleEscalationEnabled inverts the toggle into the stored Disabled flag:
    // toggle ON ⇒ disabled=false.
    function toggleEscalationEnabled(el) {
      const isOn = el.classList.toggle('on');
      markDirty('escalation', 'disabled', !isOn);
    }

    // loadEscalationConfig fetches the top-level escalation section when the
    // Health tab opens, then re-renders the tab (if still on Health) so the
    // Escalation Breaker section replaces its loading placeholder.
    async function loadEscalationConfig() {
      if (_configState.data.escalation) return;
      try {
        const res = await fetch('/api/config/escalation');
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        _configState.data.escalation = await res.json();
        if (_configState.tab === 'Health') renderConfigTab('Health');
      } catch (err) { /* section stays in loading state; save still works via dirty */ }
    }

    function renderGovSensing(s) {
      const ghPatterns = (s.ghRatePatterns || []).map((p, i) =>
        `<div class="config-list-item"><code style="font-size:0.7rem">${esc(p)}</code><button class="remove-item" data-action="removeSensingItem" data-arg0="ghRatePatterns" data-arg1="${i}" data-arg-types="s,j">✕</button></div>`
      ).join('');
      const cliPatterns = (s.cliExcludePatterns || []).map((p, i) =>
        `<div class="config-list-item"><code style="font-size:0.7rem">${esc(p)}</code><button class="remove-item" data-action="removeSensingItem" data-arg0="cliExcludePatterns" data-arg1="${i}" data-arg-types="s,j">✕</button></div>`
      ).join('');
      const loginPatterns = (s.loginPatterns || []).map((p, i) =>
        `<div class="config-list-item"><code style="font-size:0.7rem">${esc(p)}</code><button class="remove-item" data-action="removeSensingItem" data-arg0="loginPatterns" data-arg1="${i}" data-arg-types="s,j">✕</button></div>`
      ).join('');
      return `
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:14px">Patterns scanned in agent tmux panes to detect rate limits, CLI usage caps, and login requirements.</p>
        <div style="margin-bottom:18px">
          <label style="font-weight:600;font-size:0.78rem;display:block;margin-bottom:var(--sp-3)">GitHub API Rate Limit Patterns <span style="font-weight:400;color:var(--muted)">(regex, matched against pane output)</span></label>
          <div class="config-list">
            ${ghPatterns || '<div class="fs-sm text-muted mb-3">No patterns configured</div>'}
            <div class="config-list-add">
              <input type="text" id="cfg-sensing-gh-input" placeholder="e.g. 403.*rate limit" style="font-family:var(--font-mono);font-size:var(--fs-sm)" title="Regular expression to match against agent tmux pane output. When matched, the governor triggers a rate-limit pullback for that agent.">
              <button class="hv-btn btn-primary" data-action="addSensingItem" data-arg0="ghRatePatterns" data-arg1="cfg-sensing-gh-input" title="Add this regex pattern to the GitHub rate limit detection list">+ Add</button>
            </div>
          </div>
        </div>
        <div style="margin-bottom:18px">
          <label style="font-weight:600;font-size:0.78rem;display:block;margin-bottom:var(--sp-3)">CLI Exclude Patterns <span style="font-weight:400;color:var(--muted)">(lines matching these are ignored before rate-limit check)</span></label>
          <div class="config-list">
            ${cliPatterns || '<div class="fs-sm text-muted mb-3">No exclude patterns</div>'}
            <div class="config-list-add">
              <input type="text" id="cfg-sensing-cli-input" placeholder="e.g. out of extra usage" style="font-family:var(--font-mono);font-size:var(--fs-sm)" title="Lines matching these patterns are filtered out before rate-limit detection. Use for known false positives in agent output.">
              <button class="hv-btn btn-primary" data-action="addSensingItem" data-arg0="cliExcludePatterns" data-arg1="cfg-sensing-cli-input" title="Add this pattern to the CLI exclusion list">+ Add</button>
            </div>
          </div>
        </div>
        <div style="margin-bottom:18px">
          <label style="font-weight:600;font-size:0.78rem;display:block;margin-bottom:var(--sp-3)">Login Required Patterns <span style="font-weight:400;color:var(--muted)">(regex, matched against pane output &mdash; pauses agent and notifies)</span></label>
          <div class="config-list">
            ${loginPatterns || '<div class="fs-sm text-muted mb-3">No login patterns configured</div>'}
            <div class="config-list-add">
              <input type="text" id="cfg-sensing-login-input" placeholder="e.g. session expired" style="font-family:var(--font-mono);font-size:var(--fs-sm)" title="Regular expression to match against agent tmux pane output. When matched, the agent is paused and a notification is sent prompting you to log in.">
              <button class="hv-btn btn-primary" data-action="addSensingItem" data-arg0="loginPatterns" data-arg1="cfg-sensing-login-input" title="Add this regex pattern to the login detection list">+ Add</button>
            </div>
          </div>
        </div>
        <div class="config-field-row">
          <div class="config-field"><label>Alert TTL (sec) ${yamlHint('governor.sensing.ttl_seconds')} ${settingHelpMark('settings', 'sensing', 'Alert TTL (sec)', `How long a rate-limit alert stays active before auto-clearing. Default is 900 seconds (15 minutes). After this time, the agent is considered safe to kick again.`)}</label><input type="number" value="${s.ttlSeconds || 900}" data-change-action="gh48"></div>
          <div class="config-field"><label>Pullback Duration (sec) ${yamlHint('governor.sensing.pullback_seconds')} ${settingHelpMark('settings', 'sensing', 'Pullback Duration (sec)', `How long to pause an agent after detecting a rate limit. During pullback, the governor skips kicks for this agent. Default is 900 seconds (15 minutes).`)}</label><input type="number" value="${s.pullbackSeconds || 900}" data-change-action="gh49"></div>
        </div>`;
    }

    // Kick log source (#11762, #9579): what per-kick agent logs are built from.
    // Mirrors config.KickLogSources(); the labels explain the choice because
    // the values alone ("auto") do not.
    const KICK_LOG_SOURCES = [
      { value: 'scrollback', label: 'Scrollback (tmux capture — default)' },
      { value: 'transcript', label: 'CLI transcript' },
      { value: 'auto', label: 'Auto (transcript when the backend has a reader, else scrollback)' },
    ];

    function renderGovKickLogSource(l) {
      const envLocked = !!l.kickLogSourceEnvLocked;
      const saved = l.kickLogSource || 'scrollback';
      const effective = l.kickLogSourceEffective || saved;
      const shown = envLocked ? effective : saved;
      const label = v => ((KICK_LOG_SOURCES.find(o => o.value === v) || {}).label || v);
      const options = KICK_LOG_SOURCES.map(o => '<option value="' + o.value + '"' + (shown === o.value ? ' selected' : '') + '>' + esc(o.label) + '</option>').join('');
      const lockNote = envLocked
        ? `<p class="fs-sm text-muted mt-3 mb-0">Set by <code>HIVE_KICK_LOG_SOURCE</code> on the deployment: kick logs currently use <b>${esc(label(effective))}</b>. The saved setting (<b>${esc(label(saved))}</b>) applies once that variable is unset.</p>`
        : '';
      return `
        <hr class="config-section-rule">
        <div class="config-subgroup">Kick logs</div>
        <div class="config-field">
          <label>Kick log source ${settingHelpMark('agent-logging', 'per-kick-log-history', 'Kick log source', `What each per-kick agent log — the archives on an agent&apos;s 🕘 kick history page and the live full log — is built from. <b>Scrollback</b> (default) is the tmux capture of what the CLI painted; tool output the CLI collapsed (&quot;(N lines hidden)&quot;) is not in it. <b>CLI transcript</b> renders the backend&apos;s own session transcript as plain text instead: every turn and every tool call with its full arguments (the shell command) and full output, timestamped, for this kick only. Transcript readers exist today for Copilot CLI and Claude Code; a backend without one, or a missing or unparseable transcript, falls back to the scrollback with one warning in the hive log. <b>Auto</b> uses the transcript when the backend has a reader and the transcript is usable, else the scrollback, silently. Applies to the next kick without a restart. Every log&apos;s header carries a <code>source:</code> line (also the source column on the kick history page) so you can verify which was used. <code>HIVE_KICK_LOG_SOURCE</code> on the deployment overrides this setting.`)}</label>
          <select data-change-action="markDirty" data-arg0="logging" data-arg1="kickLogSource" data-arg-types="s,s,v"${envLocked ? ' disabled' : ''}>${options}</select>
          ${lockNote}
        </div>`;
    }

    // Log read scan bound (#11942): governor.logging.read_scan_max_mb lowers
    // how much log one /api/logs/read or admin MCP log_read call may scan.
    // 0 keeps the built-in bound; the field cannot raise it.
    function renderGovLogRead(l) {
      const ceiling = Number(l.readScanMaxMBDefault) || 64;
      return `
        <hr class="config-section-rule">
        <div class="config-subgroup">Log read</div>
        <div class="config-field">
          <label>Read scan bound (MB) ${settingHelpMark('settings', 'logging', 'Read scan bound (MB)', `How many megabytes of log (counting decompressed bytes of compressed history) one hive-log read — the dashboard log viewer or the admin MCP <code>log_read</code> tool — may scan before it stops and hands back a cursor to resume from. The built-in bound is ${ceiling} MB; this setting can only lower it, never raise it. Leave 0 to keep the built-in bound. Applies to the next read without a restart.`)}</label>
          <input type="number" min="0" max="${ceiling}" value="${l.readScanMaxMB || 0}" placeholder="0 = built-in (${ceiling} MB)" data-change-action="gh105">
        </div>`;
    }

    function renderGovLogging(l) {
      const levels = ['debug', 'info', 'warn', 'error'];
      return `
        <p class="config-help">YAML keys: <code>governor.logging.dir</code>, <code>governor.logging.max_size_mb</code>, <code>governor.logging.max_age_days</code>, <code>governor.logging.max_backups</code>, <code>governor.logging.level</code>, <code>governor.logging.compress</code>.</p>
        <div class="config-field" data-setting-key="governor.logging.dir">
          <label>Log Directory ${settingHelpMark('settings', 'logging', 'Log Directory', `Directory where rolling log files are written. Stored on the persistent /data volume so logs survive container restarts. Read-only at runtime — change in hive.yaml and restart.`)}</label>
          <input type="text" value="${esc(l.dir || '/data/logs')}" disabled style="opacity:0.6">
        </div>
        <div class="config-field-row">
          <div class="config-field" data-setting-key="governor.logging.max_size_mb"><label>Max File Size (MB) ${settingHelpMark('settings', 'logging', 'Max File Size (MB)', `Maximum size of a single log file before rotation. When reached, the current file is rotated and a new one starts. Default 50 MB.`)}</label><input type="number" value="${l.maxSizeMB || 50}" data-change-action="gh50"></div>
          <div class="config-field" data-setting-key="governor.logging.max_age_days"><label>Retention (days) ${settingHelpMark('settings', 'logging', 'Retention (days)', `How many days to keep rotated log files. Files older than this are automatically deleted by lumberjack. Default 7 days.`)}</label><input type="number" value="${l.maxAgeDays || 7}" data-change-action="gh51"></div>
        </div>
        <div class="config-field-row">
          <div class="config-field" data-setting-key="governor.logging.max_backups"><label>Max Backups ${settingHelpMark('settings', 'logging', 'Max Backups', `Maximum number of rotated log files to keep. Works alongside retention days — whichever limit is hit first triggers cleanup. Default 10 files.`)}</label><input type="number" value="${l.maxBackups || 10}" data-change-action="gh52"></div>
          <div class="config-field" data-setting-key="governor.logging.level"><label>Log Level ${settingHelpMark('settings', 'logging', 'Log Level', `Minimum severity level for log output. DEBUG includes all messages. INFO is the default operational level. WARN and ERROR reduce noise. Takes effect on next container restart.`)}</label>
            <select data-change-action="markDirty" data-arg0="logging" data-arg1="level" data-arg-types="s,s,v">
              ${levels.map(lv => '<option value="' + lv + '" ' + ((l.level||'info')===lv?'selected':'') + '>' + lv.toUpperCase() + '</option>').join('')}
            </select>
          </div>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${l.compress !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="logging" data-key="compress"></div>
          <span class="config-toggle-label">Compress Rotated Files ${settingHelpMark('settings', 'logging', 'Compress Rotated Files', `Gzip-compress rotated log files to save disk space. Typically reduces log file size by ~90%. Enabled by default.`)}</span>
        </div>
        ${renderGovLogRead(l)}
        ${renderGovKickLogSource(l)}`;
    }

    function securityPostureStripHtml() {
      const sec = ((window._lastStatus || {}).security) || ((_configState.data || {}).security) || {};
      const level = (window._lastStatus || {}).acmmLevel || 0;
      const parts = [
        level > 0 ? ('L' + level) : 'L?',
        sec.intentEnforced || sec.intentEnforce ? 'intent enforced' : 'intent report-only',
        'ioscan fail-' + ((sec.ioscanFailMode || 'open') === 'closed' ? 'closed' : 'open'),
        (sec.ioscanCanaries ? 'canaries on' : 'canaries off'),
        'sandbox ' + (sec.sandboxedAgents || 0) + '/' + (sec.totalAgents || 0) + ' agents',
      ];
      // #9586: proxy GitHub auth injection (opt-in: on only when explicitly
      // set; unset is off) - the reason rides in the tooltip.
      const inj = sec.credentialInjection;
      let title = 'Effective security posture from the dashboard overlay and running config';
      if (inj && inj.source) {
        parts.push('GitHub auth ' + (inj.enabled ? 'proxy-injected' : 'agent-held'));
        title += '\nProxy GitHub auth injection: ' + (inj.enabled ? 'on' : 'off') + ' (' + (inj.reason || inj.source) + ')';
      }
      return '<div style="display:inline-flex;align-items:center;gap:var(--sp-4);flex-wrap:wrap;padding:6px 10px;border:1px solid var(--border);border-radius:var(--r-pill);background:var(--surface);font-size:0.72rem;color:var(--muted)" title="' + esc(title) + '">' + esc(parts.join(' · ')) + '</div>';
    }

    function securityWarningsHtml(sec) {
      const level = (window._lastStatus || {}).acmmLevel || 0;
      const warnings = [];
      if (level >= 6 && !sec.intentEnforce) warnings.push('L6 is autonomous; intent enforcement is recommended before merge automation.');
      if (level >= 5 && (sec.ioscanFailMode || 'open') === 'open') warnings.push('L5+ with ioscan fail-open keeps work flowing, but fail-closed gives stronger prompt-injection protection.');
      if (sec.reviewRequireApproval && (sec.reviewCapableAgents || 0) === 0) warnings.push('Review approval is required, but no enabled review-capable agents were detected.');
      // #4918: the sandbox opt-in is two-gated (global agent_sandbox.enabled AND
      // a per-agent sandbox.enabled), but this toggle only ever writes the
      // global flag — so flipping it on here can silently leave every agent
      // running unconfined on the operator's host. The backend already computes
      // this exact diagnosis (config.AgentSandboxGateWarnings, also logged at
      // WARN on boot/reload); surface it here too so the operator sees it at
      // the moment they flip the toggle, not only in a log they may not be
      // watching.
      (sec.sandboxWarnings || []).forEach(w => warnings.push(w));
      // #9586: an unrecognized HIVE_PROXY_INJECT_GH_AUTH value leaves proxy
      // credential injection OFF (agents keep their real GitHub token). Boot
      // only logs it (non-fatal, so auto-deployed upgrades never crash-loop);
      // this box is where the operator sees it.
      (sec.credentialWarnings || []).forEach(w => warnings.push(w));
      if (!warnings.length) return '';
      return '<div style="border:1px solid color-mix(in srgb, var(--amber) 45%, transparent);background:color-mix(in srgb, var(--amber) 10%, transparent);border-radius:8px;padding:10px 12px;margin:0 0 14px;color:var(--amber);font-size:0.76rem;line-height:1.5"><strong>Coherence warnings (non-blocking)</strong><ul style="margin:6px 0 0 18px;padding:var(--sp-0)">' + warnings.map(w => '<li>' + esc(w) + '</li>').join('') + '</ul></div>';
    }

    // Pure: the capability gaps line(s) for ⚙ → Security
    // (hivecommons/hive#15099): /api/status.capabilities lists the GitHub
    // App permissions items are blocked on and how many rows wait on them.
    // '' when the App has every permission the parked work needs.
    function capabilityGapsText(caps) {
      caps = caps || {};
      const missing = Array.isArray(caps.missing) ? caps.missing.filter(Boolean) : [];
      if (!missing.length) return '';
      const n = Number(caps.blocked_items) || 0;
      const by = caps.by_permission || {};
      const perms = missing.map(p => '`' + p + '`' + (by[p] ? ' (' + by[p] + ')' : '')).join(', ');
      return 'GitHub App permissions: missing ' + perms + ' · ' + n + ' item' + (n === 1 ? '' : 's') + ' blocked';
    }
    function securityCapabilitiesHtml(caps, appSettingsUrl) {
      const text = capabilityGapsText(caps);
      if (!text) return '<p class="config-help">GitHub App permissions: no item is blocked on a missing permission.</p>';
      const link = appSettingsUrl ? ' <a href="' + esc(appSettingsUrl) + '" target="_blank" rel="noopener">Grant it in the App settings ↗</a>' : '';
      return '<p class="config-help ham-capability-gap" role="status">' + esc(text) + '.' + link + ' Hive re-tries blocked items on its own once it sees the permission (Re-check permissions on a row probes it now).</p>';
    }

    function securityModelDatalistHtml() {
      const seen = new Set();
      const options = [];
      (KNOWN_MODELS || []).forEach(m => {
        const v = m.value || m.label || '';
        if (v && !seen.has(v)) { seen.add(v); options.push(v); }
      });
      Object.values(BACKEND_MODELS || {}).forEach(list => (list || []).forEach(m => {
        const v = m.value || m.label || '';
        if (v && !seen.has(v)) { seen.add(v); options.push(v); }
      }));
      return '<datalist id="security-model-options">' + options.map(v => '<option value="' + esc(v) + '"></option>').join('') + '</datalist>';
    }

    function securityModelInput(key, value, placeholder) {
      return '<input type="text" list="security-model-options" value="' + esc(value || '') + '" placeholder="' + esc(placeholder || 'model id') + '" data-change-action="markDirty" data-arg0="security" data-arg1="' + key + '" data-arg-types="s,s,w">';
    }

    function featureModelInput(key, value, placeholder) {
      return '<input type="text" list="security-model-options" value="' + esc(value || '') + '" placeholder="' + esc(placeholder || 'model id') + '" data-change-action="markDirty" data-arg0="features" data-arg1="' + key + '" data-arg-types="s,s,w">';
    }

    /* intentListField renders one intent.* list. Empty means the built-in
       defaults, which are shown as the placeholder. */
    function intentListField(label, yaml, key, defaultsKey, help) {
      const sec = (_configState.data && _configState.data.security) || {};
      const values = Array.isArray(sec[key]) ? sec[key] : [];
      const defaults = ((sec.intentDefaults || {})[defaultsKey]) || [];
      return '<div class="config-field"><label>' + esc(label) + ' ' + yamlHint(yaml) + ' ' + settingHelpMark('settings', 'security-and-access', label, esc(help) + ' One entry per line; leave empty to use the built-in defaults.') + '</label>' +
        '<textarea rows="4" class="mono" placeholder="' + esc(defaults.join('\n')) + '" data-change-action="markSecurityLines" data-arg0="' + esc(key) + '" data-arg-types="s,v">' + esc(values.join('\n')) + '</textarea></div>';
    }

    function markSecurityLines(key, value) {
      const list = String(value || '').split(/\r?\n/).map(s => s.trim()).filter(Boolean);
      if (_configState.data && _configState.data.security) _configState.data.security[key] = list;
      markDirty('security', key, list);
    }

    function markSecurityNumber(key, value) {
      const n = Math.max(0, Math.floor(Number(value) || 0));
      if (_configState.data && _configState.data.security) _configState.data.security[key] = n;
      markDirty('security', key, n);
    }

    function parseSecurityList(value) {
      return String(value || '').split(',').map(s => s.trim()).filter(Boolean);
    }

    function parseOtelHeaders(value) {
      const headers = {};
      String(value || '').split('\n').forEach(line => {
        const trimmed = line.trim();
        if (!trimmed) return;
        const idx = trimmed.indexOf('=');
        if (idx <= 0) return;
        headers[trimmed.slice(0, idx).trim()] = trimmed.slice(idx + 1).trim();
      });
      markDirty('features', 'otelHeaders', headers);
    }

    function rotationFeaturesState() {
      if (!_configState.data) _configState.data = {};
      if (!_configState.data.features) _configState.data.features = {};
      const f = _configState.data.features;
      if (!f.rotationProviders || typeof f.rotationProviders !== 'object') f.rotationProviders = {};
      if (!f.rotationAgents || typeof f.rotationAgents !== 'object') f.rotationAgents = {};
      return f;
    }

    function rotationBackendOptions() {
      const opts = (CONFIG_BACKEND_OPTIONS && CONFIG_BACKEND_OPTIONS.length) ? CONFIG_BACKEND_OPTIONS : KNOWN_BACKENDS;
      return opts.filter((v, i, a) => v && a.indexOf(v) === i);
    }

    function rotationProviderRows(providers) {
      return Object.entries(providers || {}).sort((a, b) => a[0].localeCompare(b[0]));
    }

    function renderRotationBackendSelect(provider, selected) {
      selected = Array.isArray(selected) ? selected : [];
      const selectedSet = new Set(selected);
      return '<select multiple size="4" data-provider="' + esc(provider) + '" data-change-action="rotationProviderChanged" style="min-width:150px">' +
        rotationBackendOptions().map(b => '<option value="' + esc(b) + '" ' + (selectedSet.has(b) ? 'selected' : '') + '>' + esc(b) + '</option>').join('') +
        '</select>';
    }

    function renderRotationHeadroomAge(observedAt) {
      if (!observedAt) return '';
      const observedMs = new Date(observedAt).getTime();
      if (Number.isNaN(observedMs)) return '';
      const ageSec = Math.max(0, Math.floor((Date.now() - observedMs) / 1000));
      const ageStr = ageSec < 60 ? ageSec + 's ago' : ageSec < 3600 ? Math.floor(ageSec / 60) + 'm ago' : Math.floor(ageSec / 3600) + 'h ago';
      const stale = ageSec >= 300;
      const color = stale ? 'var(--yellow)' : 'var(--muted)';
      const title = stale
        ? 'This reading has not refreshed recently — treat the headroom above as a floor, not a live value'
        : 'When this provider headroom reading was taken';
      return '<span style="color:' + color + ';font-size:var(--fs-xs)" title="' + esc(title) + '">measured ' + esc(ageStr) + '</span>';
    }

    function renderRotationHeadroomValue(reading) {
      if (!reading) return '<span class="text-muted">no reading yet</span>';
      const pct = reading.pct_remaining ?? reading.pctRemaining;
      const available = reading.available === true;
      const cause = reading.cause || reading.probe_error || reading.probeError || reading.probe_error_cause || reading.probeErrorCause || '';
      const value = pct !== undefined && pct !== null
        ? String(pct) + '% remaining'
        : (available ? 'available' : 'unknown');
      const color = available ? 'var(--green)' : 'var(--yellow)';
      const observed = reading.observed_at || reading.observedAt || reading.captured_at || reading.capturedAt;
      return '<span style="color:' + color + ';font-weight:600">' + esc(value) + '</span>' +
        (cause ? ' <span class="text-muted fs-xs" title="' + esc(cause) + '">(' + esc(cause) + ')</span>' : '') +
        '<div>' + renderRotationHeadroomAge(observed) + '</div>';
    }

    function renderRotationProvidersTable(providers, headroomByProvider) {
      const rows = rotationProviderRows(providers);
      const body = rows.map(([name, cfg]) => `
        <tr data-provider="${esc(name)}">
          <td><input type="text" value="${esc(name)}" data-provider="${esc(name)}" data-change-action="rotationProviderRenamed" style="width:120px"></td>
          <td><input type="text" value="${esc(cfg.class || '')}" placeholder="subscription" data-provider="${esc(name)}" data-change-action="rotationProviderChanged" style="width:120px"></td>
          <td>${renderRotationBackendSelect(name, cfg.backends || [])}</td>
          <td><input type="number" min="0" step="1" value="${cfg.monthly_allowance || cfg.monthlyAllowance || ''}" data-provider="${esc(name)}" data-change-action="rotationProviderChanged" style="width:110px"></td>
          <td class="rotation-headroom-cell">${renderRotationHeadroomValue((headroomByProvider || {})[name])}</td>
          <td><button type="button" class="hv-btn btn-secondary btn-sm btn-small" data-action="rotationRemoveProvider" data-arg0="${esc(name)}">remove</button></td>
        </tr>`).join('');
      return `<table id="rotation-providers-table" style="width:100%;border-collapse:collapse;font-size:var(--fs-sm);margin-top:var(--sp-4)">
        <thead><tr><th style="text-align:left">Provider</th><th style="text-align:left">Class</th><th style="text-align:left">Backends</th><th style="text-align:left">Allowance</th><th style="text-align:left">Live headroom</th><th></th></tr></thead>
        <tbody>${body || '<tr><td colspan="6" style="color:var(--muted);padding:var(--sp-4) var(--sp-0)">No providers configured.</td></tr>'}</tbody>
      </table>`;
    }

    function rotationStageProviders() {
      const f = rotationFeaturesState();
      markDirty('features', 'rotationProviders', f.rotationProviders);
    }

    function rotationProviderChanged() {
      const f = rotationFeaturesState();
      const table = document.getElementById('rotation-providers-table');
      if (!table) return;
      const providers = {};
      table.querySelectorAll('tbody tr[data-provider]').forEach(row => {
        const name = row.dataset.provider;
        if (!name) return;
        const inputs = row.querySelectorAll('input');
        const backendSelect = row.querySelector('select[multiple]');
        providers[name] = {
          class: (inputs[1]?.value || '').trim(),
          backends: backendSelect ? Array.from(backendSelect.selectedOptions).map(o => o.value) : [],
          monthly_allowance: Math.max(0, parseInt(inputs[2]?.value || '0', 10) || 0),
        };
      });
      f.rotationProviders = providers;
      rotationStageProviders();
    }

    function rotationProviderRenamed() {
      const oldName = this.dataset.provider || '';
      const newName = (this.value || '').trim();
      const f = rotationFeaturesState();
      if (!oldName || !newName || oldName === newName) return;
      if (f.rotationProviders[newName]) {
        showToast('Provider already exists: ' + newName, 'error');
        this.value = oldName;
        return;
      }
      f.rotationProviders[newName] = f.rotationProviders[oldName] || {};
      delete f.rotationProviders[oldName];
      rotationStageProviders();
      renderConfigTab('Features');
    }

    function rotationAddProvider() {
      const f = rotationFeaturesState();
      let i = Object.keys(f.rotationProviders).length + 1;
      let name = 'provider-' + i;
      while (f.rotationProviders[name]) name = 'provider-' + (++i);
      f.rotationProviders[name] = { class: 'subscription', backends: [], monthly_allowance: 0 };
      rotationStageProviders();
      renderConfigTab('Features');
    }

    function rotationRemoveProvider(name) {
      const f = rotationFeaturesState();
      delete f.rotationProviders[name];
      rotationStageProviders();
      renderConfigTab('Features');
    }

    function rotationAgentTierChanged(agent, tier) {
      const f = rotationFeaturesState();
      f.rotationAgents[agent] = tier;
      markDirty('features', 'rotationAgents', f.rotationAgents);
    }

    function rotationToggleSwitch(el) {
      el = el || this;
      const isOn = el.classList.toggle('on');
      const f = rotationFeaturesState();
      f.rotationEnabled = isOn;
      markDirty('features', 'rotationEnabled', isOn);
      renderConfigTab('Features');
    }

    // Claim lifetimes (#10981): duration inputs for governor.claims.*_ttl_s
    // under the Issue claims toggle. Reset sends 0, which the server treats as
    // "use the default"; a saved change applies to claims made or renewed
    // afterwards without a restart. Owner-only, like the PUT it feeds.
    const CLAIMS_TTL_FIELDS = [
      { key: 'claimsHumanTtlS', label: 'Human claims', yaml: 'human_ttl_s', yamlKey: 'governor.claims.human_ttl_s', tip: 'How long a person\'s claim holds an issue before it lapses unless renewed. Falls back to ttl_s, then 4 hours.' },
      { key: 'claimsAgentTtlS', label: 'Agent claims', yaml: 'agent_ttl_s', yamlKey: 'governor.claims.agent_ttl_s', tip: 'How long the claim recorded when one of this hive\'s agents starts on a kicked issue lasts. Defaults to 2 hours.' },
      { key: 'claimsContributorTtlS', label: 'Contributor claims', yaml: 'contributor_ttl_s', yamlKey: 'governor.claims.contributor_ttl_s', tip: 'How long a relay contributor\'s claim lasts. Defaults to 30 minutes, the relay lease length.' },
      { key: 'claimsMaxTtlS', label: 'Maximum claim', yaml: 'max_ttl_s', yamlKey: 'governor.claims.max_ttl_s', tip: 'Cap on any requested claim lifetime; the three lifetimes above may not exceed it. Defaults to 24 hours.' },
    ];

    function claimsTTLSplit(secs) {
      const n = Math.max(0, Math.round(Number(secs) || 0));
      if (n > 0 && n % 3600 === 0) return { value: n / 3600, unit: 'h' };
      return { value: Math.round(n / 60), unit: 'm' };
    }

    function claimsTTLText(secs) {
      const d = claimsTTLSplit(secs);
      const word = d.unit === 'h' ? 'hour' : 'minute';
      return d.value + ' ' + word + (d.value === 1 ? '' : 's');
    }

    function claimsTTLNote(key, isSet) {
      const f = (_configState.data && _configState.data.features) || {};
      const def = (f.claimsTtlDefaults || {})[key] || 0;
      return isSet ? 'Default: ' + claimsTTLText(def) : 'Using the default';
    }

    function renderClaimsTTLFields(f) {
      const canEdit = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const defaults = f.claimsTtlDefaults || {};
      const set = f.claimsTtlSet || {};
      const off = canEdit ? '' : ' disabled';
      const fields = CLAIMS_TTL_FIELDS.map(fd => {
        const d = claimsTTLSplit(f[fd.key] || defaults[fd.key] || 0);
        const isSet = !!set[fd.key];
        return `<div class="config-field" data-claims-ttl="${fd.key}">
            <label>${esc(fd.label)} ${yamlHint(fd.yamlKey)} ${settingHelpMark('settings', 'features', fd.label, `${esc(fd.tip)} (governor.claims.${fd.yaml})`)}</label>
            <div class="claims-ttl-controls">
              <input type="number" min="1" step="1" class="claims-ttl-input" value="${d.value}" aria-label="${esc(fd.label)} lifetime"${off} data-change-action="claimsTTLChanged" data-arg0="${fd.key}" data-arg-types="s">
              <select aria-label="${esc(fd.label)} unit"${off} data-change-action="claimsTTLChanged" data-arg0="${fd.key}" data-arg-types="s">
                <option value="m"${d.unit === 'm' ? ' selected' : ''}>minutes</option>
                <option value="h"${d.unit === 'h' ? ' selected' : ''}>hours</option>
              </select>
              ${canEdit ? `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="claimsTTLReset" data-arg0="${fd.key}" data-arg-types="s"${isSet ? '' : ' hidden'}>Reset to default</button>` : ''}
            </div>
            <p class="claims-ttl-note">${esc(claimsTTLNote(fd.key, isSet))}</p>
          </div>`;
      }).join('');
      return `<div class="config-field-row claims-ttl-fields">${fields}</div>
          <p class="claims-ttl-footnote">${canEdit
            ? 'Changes apply to claims made or renewed after Save; existing claims keep their expiry.'
            : 'Only the hive owner can change claim lifetimes.'}</p>`;
    }

    function claimsTTLRow(key) {
      return document.querySelector('#config-body [data-claims-ttl="' + key + '"]');
    }

    function claimsTTLStage(key, secs, isSet) {
      const f = rotationFeaturesState();
      const defaults = f.claimsTtlDefaults || {};
      f[key] = isSet ? secs : (defaults[key] || 0);
      f.claimsTtlSet = Object.assign({}, f.claimsTtlSet, { [key]: isSet });
      markDirty('features', key, isSet ? secs : 0);
      const row = claimsTTLRow(key);
      if (!row) return;
      const btn = row.querySelector('[data-action="claimsTTLReset"]');
      if (btn) btn.hidden = !isSet;
      const note = row.querySelector('.claims-ttl-note');
      if (note) note.textContent = claimsTTLNote(key, isSet);
    }

    function claimsTTLChanged(key) {
      const row = claimsTTLRow(key);
      if (!row) return;
      const input = row.querySelector('input[type="number"]');
      const unit = row.querySelector('select');
      const n = Math.round(Number(input && input.value));
      if (!input || input.value === '' || !(n > 0)) {
        claimsTTLReset(key);
        return;
      }
      claimsTTLStage(key, n * (unit && unit.value === 'h' ? 3600 : 60), true);
    }

    function claimsTTLReset(key) {
      claimsTTLStage(key, 0, false);
      const row = claimsTTLRow(key);
      if (!row) return;
      const f = rotationFeaturesState();
      const d = claimsTTLSplit((f.claimsTtlDefaults || {})[key] || 0);
      const input = row.querySelector('input[type="number"]');
      const unit = row.querySelector('select');
      if (input) input.value = d.value;
      if (unit) unit.value = d.unit;
    }

    async function loadRotationHeadroom() {
      const host = document.getElementById('rotation-headroom-host');
      if (!host) return;
      try {
        const resp = await fetch('/api/providers/headroom');
        const data = await resp.json();
        if (!resp.ok) throw new Error((data && data.error) || ('HTTP ' + resp.status));
        const byProvider = {};
        (data.providers || []).forEach(p => {
          if (!p.provider) return;
          if (!p.observed_at && !p.observedAt && data.updated_at) p.observed_at = data.updated_at;
          byProvider[p.provider] = p;
        });
        const f = rotationFeaturesState();
        host.innerHTML = renderRotationProvidersTable(f.rotationProviders, byProvider);
      } catch (e) {
        host.innerHTML = '<div style="color:var(--yellow);font-size:0.72rem;margin-top:var(--sp-4)">Headroom unavailable: ' + esc(e.message) + '</div>' +
          renderRotationProvidersTable(rotationFeaturesState().rotationProviders, {});
      }
    }

    function renderSecuritySection(title, html) {
      return '<section style="margin-top:18px;padding-top:14px;border-top:1px solid var(--border)">' +
        '<div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:var(--sp-4)">' + esc(title) + '</div>' +
        html + '</section>';
    }

    function renderGovSecurity(sec) {
      sec = sec || {};
      const failMode = sec.ioscanFailMode || 'open';
      const agentNames = ((_configState.data || {}).agents || []).map(a => typeof a === 'string' ? a : (a && a.name) || '').filter(Boolean);
      const agentList = '<datalist id="security-agent-options">' + agentNames.map(n => '<option value="' + esc(n) + '"></option>').join('') + '</datalist>';
      const classifierAdvanced = Object.prototype.hasOwnProperty.call(sec, 'ioscanClassifier')
        ? `<div class="config-field-row">
            <div class="config-field" data-setting-key="ioscan.classifier.model"><label>Classifier model</label>${securityModelInput('ioscanClassifierModel', (sec.ioscanClassifier || {}).model || '', 'e.g. gpt-4o-mini')}</div>
            <div class="config-field" data-setting-key="ioscan.classifier.warn_threshold"><label>Warn threshold</label><input type="number" min="0" max="1" step="0.01" value="${(sec.ioscanClassifier || {}).warnThreshold ?? ''}" data-change-action="gh53"></div>
            <div class="config-field" data-setting-key="ioscan.classifier.block_threshold"><label>Block threshold</label><input type="number" min="0" max="1" step="0.01" value="${(sec.ioscanClassifier || {}).blockThreshold ?? ''}" data-change-action="gh54"></div>
          </div>`
        : '<p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-4) var(--sp-0) var(--sp-0)">ioscan classifier fields are not present in this v4 backend yet; this disclosure will render them automatically when the API reports support.</p>';
      // The classifier switch and its fail mode. Without them an L5-L6 hive
      // with no model endpoint withholds all untrusted text and the owner
      // has no dashboard way out. Empty follows the default, so each select
      // says what the default currently resolves to.
      const classifierSetting = sec.ioscanClassifierSetting || '';
      const classifierFailSetting = sec.ioscanClassifierFailModeSetting || '';
      const classifierBackend = String(sec.ioscanClassifierBackend || '').trim();
      const classifierControls = Object.prototype.hasOwnProperty.call(sec, 'ioscanClassifierSetting')
        ? `<div class="config-field" data-setting-key="ioscan.classifier.enabled">
          <label>Semantic classifier ${settingHelpMark('ioscan', '', 'Semantic classifier', `A model scores the untrusted text the rules let through. The default is on at ACMM L4–L6 and off below. It needs a model gateway or the claude CLI backend. Switching it off takes effect on the next pass; switching it on takes effect after a restart.`)}</label>
          <select data-change-action="markDirty" data-arg0="security" data-arg1="ioscanClassifierSetting" data-arg-types="s,s,v">
            <option value="" ${classifierSetting === '' ? 'selected' : ''}>level default (now ${sec.ioscanClassifierEnabled ? 'on' : 'off'})</option>
            <option value="on" ${classifierSetting === 'on' ? 'selected' : ''}>on</option>
            <option value="off" ${classifierSetting === 'off' ? 'selected' : ''}>off</option>
          </select>
        </div>
        <div class="config-field" data-setting-key="ioscan.classifier.fail_mode">
          <label>Classifier fail mode ${settingHelpMark('ioscan', '', 'Classifier fail mode', `What happens when the classifier cannot answer (no model endpoint, timeout, unreadable reply): open passes the rule-cleaned text on; closed withholds it. The default follows ioscan fail mode.`)}</label>
          <select data-change-action="markDirty" data-arg0="security" data-arg1="ioscanClassifierFailMode" data-arg-types="s,s,v">
            <option value="" ${classifierFailSetting === '' ? 'selected' : ''}>follow ioscan fail mode (now ${esc(sec.ioscanClassifierFailMode || 'open')})</option>
            <option value="open" ${classifierFailSetting === 'open' ? 'selected' : ''}>open — pass rule-cleaned text on</option>
            <option value="closed" ${classifierFailSetting === 'closed' ? 'selected' : ''}>closed — withhold the text</option>
          </select>
        </div>
        <div class="config-field" data-setting-key="ioscan.classifier.backend">
          <label>Classifier backend ${settingHelpMark('ioscan', '', 'Classifier backend', `Model endpoint uses the governor's OpenAI-compatible gateway. claude and copilot run the classifier through that CLI with every tool disabled, no MCP servers, no GitHub token and an empty working directory. When this is empty and no model endpoint is configured, boot uses the tool-less CLI the hive's agents run on. codex and gemini cannot run without tools and are refused at boot. Takes effect after a restart.`)}</label>
          <select data-change-action="markDirty" data-arg0="security" data-arg1="ioscanClassifierBackend" data-arg-types="s,s,v">
            <option value="" ${classifierBackend === '' ? 'selected' : ''}>model endpoint (HTTP)</option>
            <option value="claude" ${classifierBackend === 'claude' ? 'selected' : ''}>claude — tool-less CLI</option>
            <option value="copilot" ${classifierBackend === 'copilot' ? 'selected' : ''}>copilot — tool-less CLI</option>
            ${classifierBackend !== '' && classifierBackend !== 'claude' && classifierBackend !== 'copilot' ? `<option value="${esc(classifierBackend)}" selected>${esc(classifierBackend)} — refused at boot</option>` : ''}
          </select>
        </div>
        <div class="config-field-row">
          <div class="config-field" data-setting-key="ioscan.classifier.cli_budget_per_eval"><label>CLI calls per pass ${settingHelpMark('ioscan', '', 'CLI calls per pass', `CLI backend only. Classifier calls per scheduler pass; text beyond it passes on unclassified. 0 uses the default, 8. At most 64.`)}</label><input type="number" min="0" max="64" value="${Number(sec.ioscanClassifierCliBudgetPerEval || 0)}" data-change-action="markSecurityNumber" data-arg0="ioscanClassifierCliBudgetPerEval" data-arg-types="s,v"></div>
          <div class="config-field" data-setting-key="ioscan.classifier.cli_daily_quota"><label>CLI calls per day ${settingHelpMark('ioscan', '', 'CLI calls per day', `CLI backend only. Classifier calls per UTC day; once reached, text passes on unclassified until the next UTC day. 0 uses the default, 500.`)}</label><input type="number" min="0" value="${Number(sec.ioscanClassifierCliDailyQuota || 0)}" data-change-action="markSecurityNumber" data-arg0="ioscanClassifierCliDailyQuota" data-arg-types="s,v"></div>
        </div>`
        : '';

      const inputDefense = `
        <p class="config-help">YAML keys: <code>ioscan.enabled</code>, <code>ioscan.fail_mode</code>, <code>ioscan.canaries</code>, <code>ioscan.classifier.enabled</code>, <code>ioscan.classifier.fail_mode</code>, <code>ioscan.classifier.model</code>, <code>ioscan.classifier.backend</code>, <code>ioscan.classifier.cli_budget_per_eval</code>, <code>ioscan.classifier.cli_daily_quota</code>, <code>ioscan.classifier.warn_threshold</code>, <code>ioscan.classifier.block_threshold</code>.</p>
        <div class="config-toggle" data-setting-key="ioscan.enabled">
          <div class="config-toggle-switch ${sec.ioscanEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="security" data-key="ioscanEnabled"></div>
          <span class="config-toggle-label">Input/Output scanning (ioscan) ${settingHelpMark('ioscan', '', 'Input/Output scanning (ioscan)', `Scan untrusted issue/PR text for prompt-injection and dangerous directives before agent kicks.`)}</span>
        </div>
        <div class="config-field">
          <label>ioscan fail mode ${settingHelpMark('ioscan', '', 'ioscan fail mode', `open redacts and continues; closed blocks kicks with critical injection findings.`)}</label>
          <select data-change-action="markDirty" data-arg0="security" data-arg1="ioscanFailMode" data-arg-types="s,s,v">
            <option value="open" ${failMode === 'open' ? 'selected' : ''}>open — redact and continue</option>
            <option value="closed" ${failMode === 'closed' ? 'selected' : ''}>closed — block critical findings</option>
          </select>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${sec.ioscanCanaries ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="security" data-key="ioscanCanaries"></div>
          <span class="config-toggle-label">ioscan canaries ${settingHelpMark('ioscan', '', 'ioscan canaries', `Plant per-kick exfiltration markers and scan egress for leaks.`)}</span>
        </div>
        ${classifierControls}
        <details style="margin:10px 0 0"><summary style="cursor:pointer;color:var(--muted);font-size:0.78rem;font-weight:600">Advanced ioscan classifier</summary>${classifierAdvanced}</details>`;

      const mergeGates = `
        <div class="config-toggle">
          <div class="config-toggle-switch ${sec.intentEnforce ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="security" data-key="intentEnforce"></div>
          <span class="config-toggle-label">Enforce intent authorization on merges ${yamlHint('intent.enforce')} ${settingHelpMark('settings', 'security-and-access', 'Enforce intent authorization', `Off (report-only) by default. When on, a PR whose intent verdict or alignment check denies it is held from merging instead of only being logged.`)}</span>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${sec.intentShadow ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="security" data-key="intentShadow"></div>
          <span class="config-toggle-label">Shadow-evaluate intent authorization (record only) ${yamlHint('intent.shadow')} ${settingHelpMark('settings', 'security-and-access', 'Shadow-evaluate intent authorization', `Off by default. While enforcement is off, evaluates every merge as if it were on and records whether intent verification would have blocked it on the PR&apos;s evidence bundle and in the log. Never blocks a merge; ignored when enforcement is on.`)}</span>
        </div>
        <details style="margin:10px 0 18px"><summary style="cursor:pointer;color:var(--muted);font-size:0.78rem;font-weight:600">Advanced intent</summary>
          <div class="config-field"><label>Alignment model ${yamlHint('intent.alignment_model')} ${settingHelpMark('settings', 'security-and-access', 'Intent alignment model', `Model that checks whether a PR&apos;s change matches the intent of the issue it fixes, using the configured reviewer endpoint. Empty leaves the alignment check off.`)}</label>${securityModelInput('intentAlignmentModel', sec.intentAlignmentModel || '', 'leave empty for default')}</div>
          ${intentListField('Test path patterns', 'intent.test_path_patterns[]', 'intentTestPathPatterns', 'testPathPatterns', 'Globs that mark a changed file as a test. A PR that deletes or weakens matching files needs stronger authorization.')}
          ${intentListField('Docs path patterns', 'intent.docs_path_patterns[]', 'intentDocsPathPatterns', 'docsPathPatterns', 'Globs that mark a changed file as documentation. Docs-only and test-only changes skip the alignment check.')}
          ${intentListField('Guardrail path patterns', 'intent.guardrail_path_patterns[]', 'intentGuardrailPathPatterns', 'guardrailPathPatterns', 'Globs for policy, workflow and ownership files. Any PR touching one is classified in the highest intent tier.')}
          ${intentListField('Feature signals', 'intent.feature_signals[]', 'intentFeatureSignals', 'featureSignals', 'Words in a PR title or label that mark it as a new feature, which needs an approved plan or human approval.')}
        </details>

        <div class="config-toggle">
          <div class="config-toggle-switch ${sec.reviewRequireApproval ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="security" data-key="reviewRequireApproval"></div>
          <span class="config-toggle-label">Require structured review approval before merge</span>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${sec.reviewFanOut ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="security" data-key="reviewFanOut"></div>
          <span class="config-toggle-label">Fan out review work to review-capable agents</span>
        </div>
        <details style="margin:10px 0 0"><summary style="cursor:pointer;color:var(--muted);font-size:0.78rem;font-weight:600">Advanced review gate</summary>
          <div class="config-field-row">
            <div class="config-field"><label>Max parallel reviews</label><input type="number" min="0" max="64" value="${sec.reviewMaxParallelReviews || 0}" data-change-action="gh55"><span style="font-size:0.65rem;color:var(--muted)">0 uses the default (${sec.reviewMaxParallelReviews || 5}).</span></div>
            <div class="config-field"><label>Fixer agent</label><input type="text" list="security-agent-options" value="${esc(sec.reviewFixerAgent || '')}" placeholder="scanner" data-change-action="markDirty" data-arg0="security" data-arg1="reviewFixerAgent" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field"><label>Reviewer agents <span style="font-weight:400;color:var(--muted)">(comma-separated allowlist)</span></label><input type="text" value="${esc((sec.reviewReviewerAgents || []).join(', '))}" placeholder="leave blank to auto-detect review-capable agents" data-change-action="gh56"></div>
          <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Review severity threshold is not present in this v4 backend config yet, so there is no safe field to persist.</p>
        </details>`;

      // #4918: this toggle writes ONLY the global gate. Sandboxing an agent
      // needs a second, per-agent opt-in on that agent's own General tab, so
      // "on" here does not mean "confined" — sandboxWarnings (computed
      // server-side by config.AgentSandboxGateWarnings) is non-empty whenever
      // that gap exists, and gets the same visual weight as a coherence
      // warning rather than the routine muted caption below it.
      const sandboxGateWarned = sec.agentSandboxEnabled && (sec.sandboxWarnings || []).length > 0;
      const isolationWarning = sandboxGateWarned
        ? `<p style="font-size:0.72rem;color:var(--yellow);margin:var(--sp-3) var(--sp-0) var(--sp-0)"><strong>⚠ Sandbox gate on, agents still unconfined:</strong> ${esc((sec.sandboxWarnings || [])[0] || '')}</p>`
        : '';
      const isolation = `
        <div class="config-toggle">
          <div class="config-toggle-switch ${sec.agentSandboxEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="security" data-key="agentSandboxEnabled"></div>
          <span class="config-toggle-label">Enable agent sandbox runner ${settingHelpMark('sandbox-isolation', '', 'Enable agent sandbox runner', `Global gate for per-agent sandbox opt-ins. Agents still opt in from their own General tab.`)}</span>
        </div>
        <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Per-agent opt-in lives on each agent's General tab. Current effective posture: ${Number(sec.sandboxedAgents || 0)}/${Number(sec.totalAgents || 0)} agents sandboxed.</p>
        ${isolationWarning}
        <details class="settings-subsection-details"><summary class="settings-subsection-summary">Advanced sandbox</summary>
        <div class="config-field"><label>Sandbox image ${yamlHint('agent_sandbox.image')} ${settingHelpMark('settings', 'security-and-access', 'Sandbox image', `Default container image for sandboxed kicks. Agents can override it.`)}</label><input type="text" value="${esc(sec.agentSandboxImage || '')}" placeholder="container image" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxImage" data-arg-types="s,s,v"></div>
        <div class="config-field"><label>Sandbox runtime ${yamlHint('agent_sandbox.runtime')} ${settingHelpMark('settings', 'security-and-access', 'Sandbox runtime', `podman runs a rootless container on the hive node; job runs a Kubernetes Job.`)}</label><input type="text" value="${esc(sec.agentSandboxRuntime || '')}" placeholder="podman (default) or job" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxRuntime" data-arg-types="s,s,v"></div>
        <div class="config-field"><label>Network mode ${yamlHint('agent_sandbox.network_mode')} ${settingHelpMark('settings', 'security-and-access', 'Network mode', `Network access for sandboxed kicks.`)}</label><input type="text" value="${esc(sec.agentSandboxNetworkMode || '')}" placeholder="none (default) or restricted" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxNetworkMode" data-arg-types="s,s,v"></div>
        <div class="config-field"><label>Timeout (seconds) ${yamlHint('agent_sandbox.timeout_s')} ${settingHelpMark('settings', 'security-and-access', 'Timeout (seconds)', `Per-kick sandbox timeout. 0 uses the default.`)}</label><input type="number" min="0" value="${Number(sec.agentSandboxTimeoutS || 0)}" data-change-action="markSecurityNumber" data-arg0="agentSandboxTimeoutS" data-arg-types="s,v"></div>
        <div class="config-field"><label>Workspace dir ${yamlHint('agent_sandbox.workspace_dir')} ${settingHelpMark('settings', 'security-and-access', 'Workspace dir', `Directory mounted as the sandbox workspace.`)}</label><input type="text" value="${esc(sec.agentSandboxWorkspaceDir || '')}" placeholder="default workspace dir" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxWorkspaceDir" data-arg-types="s,s,v"></div>
        <div class="config-field"><label>Env allowlist ${yamlHint('agent_sandbox.env_allowlist[]')} ${settingHelpMark('settings', 'security-and-access', 'Sandbox env allowlist', `Environment variable names passed into the sandbox. One per line.`)}</label><textarea rows="3" class="mono" data-change-action="markSecurityLines" data-arg0="agentSandboxEnvAllowlist" data-arg-types="s,v">${esc((sec.agentSandboxEnvAllowlist || []).join('\n'))}</textarea></div>
        <div class="config-field"><label>Job service account ${yamlHint('agent_sandbox.job.service_account')} ${settingHelpMark('settings', 'security-and-access', 'Job service account', `Kubernetes service account for sandbox Jobs.`)}</label><input type="text" value="${esc(sec.agentSandboxJobServiceAccount || '')}" placeholder="service account name" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxJobServiceAccount" data-arg-types="s,s,v"></div>
        <div class="config-field"><label>Job scheduler name ${yamlHint('agent_sandbox.job.scheduler_name')} ${settingHelpMark('settings', 'security-and-access', 'Job scheduler name', `Kubernetes schedulerName for sandbox Job pods. Leave empty for the cluster default; set when an admission webhook requires a custom scheduler for device-requesting pods (e.g. spyre-scheduler).`)}</label><input type="text" value="${esc(sec.agentSandboxJobSchedulerName || '')}" placeholder="cluster default" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxJobSchedulerName" data-arg-types="s,s,v"></div>
        <div class="config-field"><label>Job TTL (seconds) ${yamlHint('agent_sandbox.job.ttl_seconds')} ${settingHelpMark('settings', 'security-and-access', 'Job TTL (seconds)', `Seconds a finished sandbox Job is kept. 0 uses the default.`)}</label><input type="number" min="0" value="${Number(sec.agentSandboxJobTtlSeconds || 0)}" data-change-action="markSecurityNumber" data-arg0="agentSandboxJobTtlSeconds" data-arg-types="s,v"></div>
        <div class="config-field"><label>Job workspace claim ${yamlHint('agent_sandbox.job.workspace_claim')} ${settingHelpMark('settings', 'security-and-access', 'Job workspace claim', `PVC holding the workspace for the job runtime. Required for runtime job.`)}</label><input type="text" value="${esc(sec.agentSandboxJobWorkspaceClaim || '')}" placeholder="PersistentVolumeClaim name" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxJobWorkspaceClaim" data-arg-types="s,s,v"></div>
        <div class="config-field"><label>Job workspace claim mount ${yamlHint('agent_sandbox.job.workspace_claim_mount')} ${settingHelpMark('settings', 'security-and-access', 'Job workspace claim mount', `Where the workspace claim is mounted in the Job.`)}</label><input type="text" value="${esc(sec.agentSandboxJobWorkspaceClaimMount || '')}" placeholder="mount path" data-change-action="markDirty" data-arg0="security" data-arg1="agentSandboxJobWorkspaceClaimMount" data-arg-types="s,s,v"></div>
        </details>`;

      /* Backup encryption key: painted asynchronously from presence-only
         status so the key value never travels to the browser. */
      loadBackupKeyStatus();
      const backupKey = `
        <div id="gov-backup-key-host"><div style="color:var(--muted);font-size:0.8rem">Loading…</div></div>`;

      /* Lane write allowlist (#9587): painted asynchronously from its own
         owner-only endpoint, like the Backup panel. */
      loadWriteSurfaceAllowlist();
      const writeSurface = `
        <div id="gov-write-surface-host"><div style="color:var(--muted);font-size:var(--fs-base)">Loading…</div></div>`;

      /* Suspicious Activity (sentinel): flags PRs that look like security
         overrides, privilege escalation or codebase damage. Untrusted authors
         get the blocking alert label; trusted authors default to notice-only. */
      const sn = sec.sentinel || {};
      const snPaths = Array.isArray(sn.sensitivePaths) ? sn.sensitivePaths : [];
      const snBehaviors = (sn.behaviors || []).map(b => `
        <div class="config-toggle">
          <div class="config-toggle-switch ${b.enabled ? 'on' : ''} ${sn.enabled ? '' : 'disabled'}" data-action="toggleSentinelBehavior" data-rule="${esc(b.name)}"></div>
          <span class="config-toggle-label"><code>${esc(b.name)}</code> ${settingHelpMark('operator-reference', 'suspicious-activity-alerts-sentinel', b.name, `${esc(b.description || '')}`)}</span>
        </div>`).join('');
      const suspicious = `
        <div class="config-toggle">
          <div class="config-toggle-switch ${sn.enabled ? 'on' : ''}" data-action="toggleSentinelEnabled"></div>
          <span class="config-toggle-label">Suspicious-activity alerts ${settingHelpMark('operator-reference', 'suspicious-activity-alerts-sentinel', 'Suspicious-activity alerts', `Every sweep (~15 min) inspects open PRs from any author. A PR matching a behavior below gets one explanatory comment per push; untrusted authors also get the blocking alert label, while trusted Hive authors default to informational notices that do not block merge.`)}</span>
        </div>
        <div class="config-toggle">
          <div class="config-toggle-switch ${sn.trustedAuthorsBlock ? 'on' : ''} ${sn.enabled ? '' : 'disabled'}" data-action="toggleSentinelTrustedAuthorsBlock"></div>
          <span class="config-toggle-label">Block trusted authors ${yamlHint('sentinel.trusted_authors_block')} ${settingHelpMark('operator-reference', 'suspicious-activity-alerts-sentinel', 'Block trusted authors', `Default off: Hive App PRs, owner-authored PRs and allow-listed bot PRs still get a Sentinel notice and audit record, but Hive does not add the alert label or block merge. Turn on to add the label for trusted authors too.`)}</span>
        </div>
        <div class="config-field-row">
          <div class="config-field"><label>Alert label</label><input type="text" value="${esc(sn.label || 'sentinel-alert')}" placeholder="sentinel-alert" data-change-action="gh81" ${sn.enabled ? '' : 'disabled'}></div>
          <div class="config-field"><label>Max alerts per sweep ${yamlHint('sentinel.max_actions')} ${settingHelpMark('operator-reference', 'suspicious-activity-alerts-sentinel', 'Max alerts per sweep', `Caps label+comment actions per pass so a flood of PRs cannot spam a repo.`)}</label><input type="number" min="1" max="500" value="${Number(sn.maxActions) || 20}" data-change-action="gh82" ${sn.enabled ? '' : 'disabled'}></div>
        </div>
        <div class="config-field">
          <label>Sensitive files ${yamlHint('sentinel.sensitive_paths[]')} ${settingHelpMark('operator-reference', 'suspicious-activity-alerts-sentinel', 'Sensitive files', `One glob per line (** matches directories). Any PR touching a match is flagged with sensitive_path. Clear the box and save to restore the shipped defaults.`)}${sn.sensitivePathsDefault ? ' <span class="muted-xs">(shipped defaults)</span>' : ' <button type="button" class="hv-btn btn-secondary btn-sm" data-action="resetSentinelPaths">Reset to defaults</button>'}</label>
          <textarea rows="8" class="mono" data-change-action="gh83" ${sn.enabled ? '' : 'disabled'}>${esc(snPaths.join('\n'))}</textarea>
        </div>
        <div class="config-field">
          <label>Behaviors ${yamlHint('sentinel.disabled_behaviors[]')} ${settingHelpMark('operator-reference', 'suspicious-activity-alerts-sentinel', 'Behaviors', `Heuristics evaluated against each PR's title, author and diff. Switch one off to stop it raising alerts.`)}</label>
          ${snBehaviors}
        </div>
        <div class="config-field-row">
          <div class="config-field"><label>Exempt logins ${yamlHint('sentinel.exempt_logins[]')} ${settingHelpMark('operator-reference', 'suspicious-activity-alerts-sentinel', 'Exempt logins', `Comma-separated GitHub logins never flagged (e.g. the org's release bot). Use sparingly — maintainers should still be reviewed when touching OWNERS.`)}</label><input type="text" value="${esc((sn.exemptLogins || []).join(', '))}" placeholder="release-bot" data-change-action="gh84" ${sn.enabled ? '' : 'disabled'}></div>
          <div class="config-field"><label>Repos (optional allow-list) ${yamlHint('sentinel.repos[]')}</label><input type="text" value="${esc((sn.repos || []).join(', '))}" placeholder="all project repos" data-change-action="gh85" ${sn.enabled ? '' : 'disabled'}></div>
        </div>`;
      setTimeout(loadSentinelFindings, 0);
      const suspiciousFindings = `
        <div class="qa-schedule" id="sentinel-findings">
          <div class="qa-schedule-label">Sentinel findings ${settingHelpMark('settings', 'features', 'Write-back containment', `Every PR the sentinel flagged this process lifetime, including findings the write-back containment gate kept off GitHub (ACMM level below the sentinel floor, no opt-in label, or a PR that predates hive's write access). Suppressed findings exist only here and in /api/sentinel.`)}</div>
          <div id="sentinel-findings-host"><div class="qa-note">Loading…</div></div>
        </div>`;

      return `
        ${securityModelDatalistHtml()}${agentList}
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:10px">Security posture and access controls saved through the dashboard overlay.</p>
        <div style="margin-bottom:14px">${securityPostureStripHtml()}</div>
        ${securityWarningsHtml(sec)}
        ${renderSecuritySection('Access', renderGovAccess())}
        ${renderSecuritySection('Input Defense', inputDefense)}
        ${renderSecuritySection('Merge Gates', mergeGates)}
        ${renderSecuritySection('Suspicious Activity', suspicious + suspiciousFindings)}
        ${renderSecuritySection('Isolation', isolation)}
        ${renderSecuritySection('Write Surface', writeSurface)}
        ${renderSecuritySection('GitHub App permissions', securityCapabilitiesHtml((window._lastStatus || {}).capabilities, (window._lastStatus || {}).githubAppInstallURL))}
        ${renderSecuritySection('Backup', backupKey)}`;
    }

    /* Lane write allowlist editor (#9587): write_surface.allowlist, loaded
       from and saved to the owner-only /api/config/write-surface endpoint.
       Everything is rendered with DOM nodes and textContent only, so a lane
       or operation name from hive.yaml is never parsed as markup. The editor
       is plain text, one lane per line: "scanner: create_issue, comment".
       An empty editor clears the allowlist, which restricts nothing. */
    async function loadWriteSurfaceAllowlist() {
      let host = null;
      try {
        const r = await fetch('/api/config/write-surface');
        host = document.getElementById('gov-write-surface-host');
        if (!host) return;
        if (!r.ok) throw new Error('HTTP ' + r.status);
        renderWriteSurfacePanel(host, await r.json());
      } catch (e) {
        host = host || document.getElementById('gov-write-surface-host');
        if (!host) return;
        host.textContent = '';
        host.appendChild(writeSurfaceNode('div', 'color:var(--red);font-size:0.8rem',
          'Failed to load the write allowlist: ' + e.message));
      }
    }

    /* writeSurfaceNode builds one element whose content is plain text. */
    function writeSurfaceNode(tag, style, text) {
      const node = document.createElement(tag);
      if (style) node.style.cssText = style;
      if (text !== undefined) node.textContent = text;
      return node;
    }

    /* writeSurfaceAllowlistToText renders {lane: [ops]} as editor text. */
    function writeSurfaceAllowlistToText(allowlist) {
      allowlist = allowlist || {};
      return Object.keys(allowlist).sort()
        .map(lane => lane + ': ' + (allowlist[lane] || []).join(', '))
        .join('\n');
    }

    /* parseWriteSurfaceText turns editor text into {lane: [ops]}. It only
       splits lines; the server validates lane names and operations and its
       error message says what is wrong. Blank lines and "#" comments are
       skipped. "lane:" with nothing after it allows that lane nothing. */
    function parseWriteSurfaceText(text) {
      const allowlist = {};
      const lines = String(text || '').split('\n');
      for (let i = 0; i < lines.length; i++) {
        const line = lines[i].trim();
        if (!line || line.startsWith('#')) continue;
        const colon = line.indexOf(':');
        if (colon < 0) throw new Error('Line ' + (i + 1) + ': expected "lane: operation, operation"');
        const lane = line.slice(0, colon).trim();
        if (!lane) throw new Error('Line ' + (i + 1) + ': the lane name is empty');
        if (Object.prototype.hasOwnProperty.call(allowlist, lane)) {
          throw new Error('Line ' + (i + 1) + ': lane ' + lane + ' is listed twice');
        }
        allowlist[lane] = line.slice(colon + 1).split(',').map(op => op.trim()).filter(Boolean);
      }
      return allowlist;
    }

    /* renderWriteSurfacePanel paints the allowlist card and wires its Save. */
    function renderWriteSurfacePanel(host, d) {
      if (!host) return;
      d = d || {};
      host.textContent = '';
      const card = writeSurfaceNode('div', 'border:1px solid var(--border);border-radius:8px;padding:14px;background:var(--surface);max-width:640px');
      card.appendChild(writeSurfaceNode('div', 'font-weight:600;font-size:0.9rem;margin-bottom:var(--sp-4)', 'Lane write allowlist'));
      card.appendChild(writeSurfaceNode('div', 'font-size:0.74rem;color:var(--muted);line-height:1.5;margin-bottom:var(--sp-4)',
        'Limits which GitHub writes each agent lane may ask the hive to perform (open a PR, comment, merge and so on). ' +
        'A lane that is not listed is unrestricted, so an empty allowlist changes nothing. Listing a lane narrows it to the ' +
        'listed operations; "lane:" with nothing after it allows nothing, and "*" allows everything. ' +
        'One lane per line, for example "scanner: create_issue, comment, claim".'));
      card.appendChild(writeSurfaceNode('div', 'font-size:0.72rem;color:var(--muted);margin-bottom:var(--sp-3)',
        'Operations: ' + (d.ops || []).join(', ')));
      const editor = writeSurfaceNode('textarea', 'width:100%;min-height:96px;padding:var(--sp-4);border:1px solid var(--border);border-radius:var(--r);background:var(--bg);color:var(--fg);font-family:var(--font-mono);font-size:var(--fs-sm)');
      editor.id = 'gov-write-surface-editor';
      editor.spellcheck = false;
      editor.placeholder = 'scanner: create_issue, comment, claim';
      editor.value = writeSurfaceAllowlistToText(d.allowlist);
      card.appendChild(editor);
      const warnings = d.warnings || [];
      if (warnings.length) {
        const list = writeSurfaceNode('ul', 'font-size:0.72rem;color:var(--yellow);margin:var(--sp-3) var(--sp-0) var(--sp-0);padding-left:18px');
        warnings.forEach(w => list.appendChild(writeSurfaceNode('li', '', w)));
        card.appendChild(list);
      }
      const row = writeSurfaceNode('div', 'display:flex;align-items:center;gap:10px;margin-top:var(--sp-4)');
      const saveBtn = writeSurfaceNode('button', '', 'Save allowlist');
      saveBtn.className = 'hv-btn btn-primary btn-sm';
      const statusEl = writeSurfaceNode('div', 'font-size:0.74rem;min-height:18px');
      row.appendChild(saveBtn);
      row.appendChild(statusEl);
      card.appendChild(row);
      host.appendChild(card);

      const setStatus = (msg, color) => { statusEl.textContent = msg; statusEl.style.color = color || 'var(--muted)'; };
      saveBtn.addEventListener('click', async () => {
        let allowlist;
        try {
          allowlist = parseWriteSurfaceText(editor.value);
        } catch (e) {
          setStatus(e.message, 'var(--red)');
          return;
        }
        saveBtn.disabled = true;
        setStatus('Saving…');
        try {
          const res = await fetch('/api/config/write-surface', {
            method: 'PUT',
            headers: _inceptionAuthHeaders(),
            body: JSON.stringify({ allowlist })
          });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          renderWriteSurfacePanel(host, await res.json());
          showToast('Write allowlist saved', 'success');
        } catch (e) {
          saveBtn.disabled = false;
          setStatus(e.message, 'var(--red)');
        }
      });
    }

    /* loadBackupKeyStatus fetches presence-only backup-key status and paints
       the Backup panel. The response carries `configured`/`usable` and a safe
       `source` string ("file:<path>" / "env:<NAME>") — never the key value.
       The DOM target is resolved AFTER the await, because the caller assigns
       the tab HTML only once this function has already started. */
    async function loadBackupKeyStatus() {
      let host = null;
      try {
        const r = await fetch('/api/config/governor/backup');
        host = document.getElementById('gov-backup-key-host');
        if (!host) return;
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const d = await r.json();
        renderBackupKeyPanel(host, d);
      } catch (e) {
        host = host || document.getElementById('gov-backup-key-host');
        if (host) host.innerHTML = '<div style="color:var(--red);font-size:0.8rem">Failed to load backup key status: ' + esc(e.message) + '</div>';
      }
    }

    /* renderBackupKeyPanel paints the backup credential card. Only presence,
       the safe source label and the operator-chosen key NAME reach this
       function; the key value never does. */
    function renderBackupKeyPanel(host, d) {
      if (!host) return;
      d = d || {};
      const configured = d.configured === true;
      const usable = d.usable === true;
      const source = d.source || '';
      const nameLabel = configured && d.keyName
        ? ' <strong class="text-default">' + esc(d.keyName) + '</strong>'
        : '';
      let statusLine;
      if (configured && usable) {
        statusLine = '<span class="text-success">✓ Key set — backups are enabled</span>' + nameLabel +
          (source ? ' <span class="text-muted">(' + esc(source) + ')</span>' : '');
      } else if (configured) {
        statusLine = '<span class="text-danger">✗ Key is unusable — backups are refused.</span>' +
          (d.reason ? ' <span class="text-muted">' + esc(d.reason) + '</span>' : '');
      } else {
        statusLine = '<span class="text-muted">No key set — backups are refused, because an unencrypted backup would expose this hive\u2019s GitHub App private keys.</span>';
      }
      host.innerHTML = `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;background:var(--surface);max-width:640px">
          <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:var(--sp-4)">
            <span style="font-weight:600;font-size:0.9rem">Backup encryption key</span>
            <span style="font-size:var(--fs-xs);font-weight:600;padding:var(--sp-1) var(--sp-4);border-radius:10px;border:1px solid #4589ff;color:#4589ff">AES-256-GCM</span>
            <span style="flex:1"></span>
            ${configured ? '<button class="hv-btn btn-danger btn-sm" data-action="backupKeyClear">Clear key</button>' : ''}
            <button class="hv-btn btn-primary btn-sm" data-action="backupKeySet">${configured ? 'Replace key' : 'Set key'}</button>
          </div>
          <div style="font-size:0.78rem;margin-bottom:var(--sp-4)">${statusLine}</div>
          <div style="font-size:0.74rem;color:var(--muted);line-height:1.5">
            Sets the key that seals <b>Back up this hive</b> archives. Setting it here needs no
            deployment or cluster access, so hosted hive owners can turn on their own backups.
            The value is written to a 0600 file on this hive and never appears in hive.yaml,
            logs, or any API response — and it is <b>not inside the archive</b>, so escrow it
            somewhere safe: without it a backup cannot be restored. Clearing it disables
            backups again rather than producing an unencrypted one.
          </div>
          <div id="backup-key-inline-status" style="font-size:0.74rem;min-height:18px;margin-top:var(--sp-4)"></div>
        </div>`;

      const setBtn = host.querySelector('[data-action="backupKeySet"]');
      if (setBtn) setBtn.addEventListener('click', () => openBackupKeyDialog(configured));
      const clearBtn = host.querySelector('[data-action="backupKeyClear"]');
      if (clearBtn) clearBtn.addEventListener('click', clearBackupKey);
    }

    /* openBackupKeyDialog collects a 64-character hex key. The field is a
       password input and its value is sent once, over the same-origin PUT —
       it is never read back, so a replace is a blind overwrite by design. */
    function openBackupKeyDialog(configured) {
      let overlay = document.getElementById('backup-key-overlay');
      if (overlay) overlay.remove();
      overlay = document.createElement('div');
      overlay.id = 'backup-key-overlay';
      overlay.className = 'gh-auth-overlay cred-dialog-overlay';
      overlay.innerHTML =
        '<div class="gh-auth-modal" style="max-width:560px;text-align:left">' +
        '<h3 style="margin:var(--sp-0) var(--sp-0) var(--sp-4)">' + (configured ? 'Replace' : 'Set') + ' backup encryption key</h3>' +
        '<p style="color:var(--muted);font-size:0.8rem;line-height:1.5;margin:var(--sp-0) var(--sp-0) var(--sp-4)">' +
        'Generate one with <code>openssl rand -hex 32</code> and store it outside this hive. ' +
        'Backups are encrypted with it and the key is not included in the archive, so a lost key means an unrestorable backup.' +
        (configured ? ' Replacing the key does not re-encrypt existing archives — keep the old key to restore them.' : '') +
        '</p>' +
        '<label for="backup-key-name-input" style="display:block;font-size:0.78rem;color:var(--muted);margin:var(--sp-4) var(--sp-0) var(--sp-2)">Key label (optional, e.g. "escrowed in 1Password")</label>' +
        '<input id="backup-key-name-input" type="text" autocomplete="off" spellcheck="false" maxlength="128" ' +
        'style="width:100%;padding:var(--sp-4);border:1px solid var(--border);border-radius:var(--r);background:var(--bg);color:var(--fg)">' +
        '<label for="backup-key-input" style="display:block;font-size:0.78rem;color:var(--muted);margin:10px 0 4px">Encryption key (64 hex characters)</label>' +
        '<input id="backup-key-input" type="password" autocomplete="off" spellcheck="false" placeholder="64 hex characters" ' +
        'style="width:100%;padding:var(--sp-4);border:1px solid var(--border);border-radius:var(--r);background:var(--bg);color:var(--fg);font-family:var(--font-mono)">' +
        '<div id="backup-key-status" style="color:var(--muted);font-size:0.8rem;min-height:20px;margin:var(--sp-3) var(--sp-0)"></div>' +
        '<div style="display:flex;gap:var(--sp-4);justify-content:flex-end">' +
        '<button id="backup-key-cancel" class="hv-btn btn-secondary gh-cancel btn-primary">Cancel</button>' +
        '<button class="hv-btn btn-primary" id="backup-key-save">Save key</button>' +
        '</div></div>';
      document.body.appendChild(overlay);
      overlay.style.display = 'flex';

      const input = document.getElementById('backup-key-input');
      const statusEl = document.getElementById('backup-key-status');
      const setStatus = (msg, color) => {
        if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || 'var(--muted)'; }
      };
      if (input) input.focus();

      const close = () => { overlay.remove(); };
      const cancelBtn = document.getElementById('backup-key-cancel');
      if (cancelBtn) cancelBtn.addEventListener('click', close);

      const saveBtn = document.getElementById('backup-key-save');
      if (saveBtn) saveBtn.addEventListener('click', async () => {
        const key = (input && input.value || '').trim();
        if (!key) { setStatus('Paste a 64-character hex key.', 'var(--red)'); return; }
        if (!/^[0-9a-fA-F]{64}$/.test(key)) {
          setStatus('A backup key is exactly 64 hex characters (openssl rand -hex 32).', 'var(--red)');
          return;
        }
        const nameInput = document.getElementById('backup-key-name-input');
        saveBtn.disabled = true;
        setStatus('Saving…');
        try {
          const resp = await fetch('/api/config/governor/backup', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ encryptionKey: key, keyName: (nameInput && nameInput.value || '').trim() })
          });
          const d = await resp.json().catch(() => ({}));
          if (!resp.ok) throw new Error(d && d.error ? d.error : 'HTTP ' + resp.status);
          if (input) input.value = '';
          close();
          showToast('Backup encryption key saved — backups are enabled for this hive', 'success');
          refreshBackupKeyPanel();
        } catch (e) {
          setStatus('Save failed: ' + e.message, 'var(--red)');
        } finally {
          saveBtn.disabled = false;
        }
      });
    }

    /* clearBackupKey removes the stored key. Fail-closed: after this, backups
       are refused again (unless the deployment still supplies HIVE_BACKUP_KEY). */
    async function clearBackupKey() {
      if (!await hiveConfirm({title: 'Clear backup encryption key?', message: 'Backups will be refused until a new key is set, and archives sealed with the old key still need that old key to restore.', confirmLabel: 'Clear key', danger: true})) return;
      const statusEl = document.getElementById('backup-key-inline-status');
      const setStatus = (msg, color) => {
        if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || 'var(--muted)'; }
      };
      try {
        const resp = await fetch('/api/config/governor/backup', { method: 'DELETE' });
        const d = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(d && d.error ? d.error : 'HTTP ' + resp.status);
        setStatus('');
        showToast('Backup encryption key cleared — backups are now refused', 'success');
        refreshBackupKeyPanel();
      } catch (e) {
        setStatus('Clear failed: ' + e.message, 'var(--red)');
      }
    }

    /* refreshBackupKeyPanel repaints the Backup panel after a set/clear, but
       only if the Security tab is still mounted. */
    function refreshBackupKeyPanel() {
      if (document.getElementById('gov-backup-key-host')) loadBackupKeyStatus();
    }

    // FEATURES_SECTIONS is the ordered list of headed sections on Settings →
    // Features, most-touched first. Each renders as a `.config-section` with a
    // stable id (`features-section-<id>`) and data-settings-section so the
    // search "Open tab" affordance and the in-tab table of contents can scroll
    // straight to it. The search index entries for this tab use `title` as
    // their `section`; TestFeaturesTabSections pins the two together.
    const FEATURES_SECTIONS = [
      { id: 'auto-merge', title: 'Auto-merge', desc: 'Which green PRs Hive merges on its own: App-authored, trusted bots, trusted authors and the contributor lane.' },
      { id: 'review', title: 'Review gate', desc: 'Review-swarm approval before merge, reviewer perspectives, external review bots and merge recommendations.' },
      { id: 'autonomy', title: 'Autonomy & ACMM', desc: 'Automatic ACMM level changes and the capabilities that unlock at higher levels.' },
      { id: 'work-intake', title: 'Work intake & issue lifecycle', desc: 'How issues are classified, claimed, closed on merge, de-duplicated, auto-closed and re-planned.' },
      { id: 'agents', title: 'Agents & tooling', desc: 'Agent session supervision and resume, provider rotation, the policy repo, decision models and workload identity.' },
      { id: 'quality', title: 'Quality loops & release health', desc: 'Post-completion feedback lanes, audit publication and release CI repair.' },
      { id: 'integrations', title: 'Integrations & observability', desc: 'OpenTelemetry export plus the Linear, upstream-fork and self-report connections.' },
      { id: 'experimental', title: 'Experimental', desc: 'Pilots and report-only rollouts. Off by default; expect these to move or change.' },
    ];


    // ── One-shot attention shimmy ──────────────────────────────────────
    // Draw the eye, once per page load, to two interactive affordances users
    // often miss: the ACMM level badge and the topbar Guide (Getting Started
    // reopen) button. Fires after the page has settled, staggered so the two
    // wiggles read as one intentional gesture rather than a glitch. The class
    // is removed on animationend so it can re-trigger on the next page load.
    // Skipped entirely under prefers-reduced-motion.
    const SHIMMY_INITIAL_DELAY_MS = 1500; // let first paint + layout settle
    const SHIMMY_STAGGER_MS = 250;        // gap between badge and Guide button
    function _prefersReducedMotionShimmy() {
      return window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches;
    }
    function _shimmyOnce(el) {
      if (!el) return;
      el.classList.add('attn-shimmy');
      el.addEventListener('animationend', function handler() {
        el.classList.remove('attn-shimmy');
        el.removeEventListener('animationend', handler);
      });
    }
    function triggerAttentionShimmy() {
      if (_prefersReducedMotionShimmy()) return;
      setTimeout(function () {
        _shimmyOnce(document.getElementById('acmm-badge'));
        setTimeout(function () {
          _shimmyOnce(document.getElementById('welcome-topbar-btn'));
        }, SHIMMY_STAGGER_MS);
      }, SHIMMY_INITIAL_DELAY_MS);
    }
    triggerAttentionShimmy();

    checkClaudeAuth();
    var CLAUDE_AUTH_CHECK_INTERVAL_MS = 60000;
    dashboardSetInterval('welcome-claude-auth', checkClaudeAuth, CLAUDE_AUTH_CHECK_INTERVAL_MS);

    window.addEventListener('hashchange', function() {
      _overviewProjectFilterKey = overviewProjectFilterKeyFromURL();
      _overviewProjectFilterCount = null;
      var section = _ocSectionFromHash();
      if (section) ocNavigate(section);
      if (section === 'repos-section') repaintReposForWidth();
    });

    connect();

    // Focus-aware presence reporting. An open tab is NOT engagement: the spoke
    // only learns "a human is actually here" from these pings, which fire on a
    // fixed cadence and ONLY while the tab is visible AND input (mouse/key/
    // scroll/touch) happened within the idle window. No ping = idle — there is
    // no explicit idle message. The spoke's engaged set rides the heartbeat to
    // the hub, which accumulates honest engaged_seconds next to the legacy
    // open-tab session_seconds. Failures are swallowed: presence is advisory
    // and must never surface an error to the user.
    (function initPresenceReporting() {
      var PRESENCE_PING_MS = 30000;           // engaged-ping cadence; must match presencePingIntervalSecs (presence.go)
      var PRESENCE_IDLE_WINDOW_MS = 60000;    // no input for this long => idle, stop pinging
      var PRESENCE_INPUT_THROTTLE_MS = 5000;  // rate-limit input bookkeeping (mousemove fires constantly)
      var _presenceLastInput = Date.now();
      function notePresenceInput() {
        var now = Date.now();
        if (now - _presenceLastInput >= PRESENCE_INPUT_THROTTLE_MS) _presenceLastInput = now;
      }
      ['mousemove', 'mousedown', 'keydown', 'scroll', 'wheel', 'touchstart'].forEach(function(ev) {
        window.addEventListener(ev, notePresenceInput, { passive: true, capture: true });
      });
      function presenceEngaged() {
        return document.visibilityState === 'visible' &&
          (Date.now() - _presenceLastInput) < PRESENCE_IDLE_WINDOW_MS;
      }
      function sendPresencePing() {
        if (!presenceEngaged()) return;
        fetch('/api/presence', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ engaged: true })
        }).catch(function() {});
      }
      document.addEventListener('visibilitychange', function() {
        if (document.visibilityState === 'visible') {
          // Returning to the tab is itself input — restart the idle clock and
          // report immediately so presence recovers without waiting a cadence.
          _presenceLastInput = Date.now();
          sendPresencePing();
        }
      });
      dashboardSetInterval('presence-ping', sendPresencePing, PRESENCE_PING_MS);
      sendPresencePing();
    })();

    // Deferred initialization — non-critical fetches staggered after first paint.
    // Each entry returns a promise so anchor scrolling can wait for layout to settle.
    const DEFERRED_INIT_DELAY_MS = 1500;
    const DEFERRED_STAGGER_MS = 200;
    _deferredLoadPromise = new Promise(function(resolveAll) {
      setTimeout(() => {
        const deferred = [
          () => Promise.resolve(refreshBackendDiscovery()).then(() => fetchBackendsConfig()),
          () => fetchFactHistory(),
          () => fetchOverviewKPIHistory().then(() => { dashboardSetInterval('overview-kpi-history', fetchOverviewKPIHistory, HISTORY_REFRESH_MS); }),
          () => fetchTrendHistory().then(() => fetchHistory()).then(() => { dashboardSetInterval('history', fetchHistory, HISTORY_REFRESH_MS); }),
          () => fetch('/api/budget-ignore').then(r => r.json()).then(d => { budgetIgnored = d.ignored; }).catch(() => {}),
          () => fetchBudgetHistory().then(() => { dashboardSetInterval('budget-history', fetchBudgetHistory, HISTORY_REFRESH_MS); }),
          () => ensureNousPolling(),
          () => fetchPublicKnowledgeSetting(),
          () => fetchKnowledgeStats().then(() => { dashboardSetInterval('knowledge-stats', fetchKnowledgeStats, KB_POLL_MS); }),
          () => fetchAuditLog().then(() => { dashboardSetInterval('audit-log', fetchAuditLog, AUDIT_POLL_MS); }),
          () => fetchReviewQueue().then(() => { dashboardSetInterval('review-queue', fetchReviewQueue, REVIEW_QUEUE_POLL_MS); }),
          () => fetchReviewPipeline().then(() => { dashboardSetInterval('review-pipeline', fetchReviewPipeline, REVIEW_PIPELINE_POLL_MS); }),
          () => fetchStageTracker().then(() => { dashboardSetInterval('stage-tracker', fetchStageTracker, STAGE_TRACKER_POLL_MS); }),
          () => fetchContributors().then(() => { dashboardSetInterval('contributors', fetchContributors, CONTRIBUTOR_POLL_MS); }),
          () => fetchLeaderboard().then(() => { dashboardSetInterval('leaderboard', fetchLeaderboard, LEADERBOARD_POLL_MS); }),
          () => fetchGitVersion().then(() => { dashboardSetInterval('git-version', fetchGitVersion, GIT_VERSION_POLL_MS); }),
          () => fetchACMMEvaluation().then(() => { dashboardSetInterval('acmm-evaluation', fetchACMMEvaluation, ACMM_EVAL_INTERVAL_MS); }),
          () => Promise.resolve(startVersionCheck()),
          () => fetch('/api/trends?range=week').then(r => r.json()).then(applyTrends).catch(() => {}).then(() => { dashboardSetInterval('weekly-trends', () => { fetch('/api/trends?range=week').then(r => r.json()).then(applyTrends).catch(() => {}); }, TREND_REFRESH_MS); }),
          () => fetchTimeline().then(() => { dashboardSetInterval('timeline', fetchTimeline, TIMELINE_REFRESH_MS); }),
          () => fetchAgentLogs().then(() => { dashboardSetInterval('agent-logs', fetchAgentLogs, LOGS_REFRESH_MS); }),
        ];
        // `Promise.resolve().then(fn)` rather than `fn()`, so ONE bad entry
        // cannot strand the whole chain (#6581). `fn()` was called bare inside
        // the setTimeout callback, so an entry that threw synchronously — or
        // returned undefined and had `.then` called on it, which is what
        // fetchTimeline did — threw out of the callback with nothing to catch
        // it. That entry's promise then never settled, so Promise.all never
        // settled, so resolveAll never ran and _deferredLoadPromise stayed
        // pending for the life of the page. _afterPaint awaits it, so first-
        // load view restoration (making #oc-agent-detail active, navigating to
        // a saved section) silently never happened.
        //
        // Wrapping turns a synchronous throw into a rejection, which
        // `.then(resolve, resolve)` already handles: the entry is recorded as
        // finished-badly instead of never finishing, and the other twelve
        // entries still complete. The throw is still reported below rather
        // than swallowed — the point is that one broken entry degrades to one
        // broken feature, not to a dashboard that never finishes initialising.
        const allDone = deferred.map((fn, i) =>
          new Promise(resolve => setTimeout(() => {
            Promise.resolve().then(fn).then(resolve, err => {
              console.error('deferred init entry ' + i + ' failed', err);
              resolve();
            });
          }, i * DEFERRED_STAGGER_MS))
        );
        Promise.all(allDone).then(resolveAll);
      }, DEFERRED_INIT_DELAY_MS);
    });
    // ── Generated event-delegation handlers (#3848) ──
    // Each entry replaces a former inline on*= attribute body verbatim;
    // A[] carries the values that used to be interpolated into the
    // attribute, now read from data-arg* (so CSP script-src-attr can be
    // 'none' and injected handler attributes never execute).
    // Named helpers for former multi-statement/string-concat inline handlers.
    function ghCopyInviteLink(url) {
      navigator.clipboard.writeText(url);
      showToast('Contribute link copied \u2014 share it with your community!', 'success');
    }
    function ghToggleDisplayById(id) {
      const el = document.getElementById(id);
      if (el) el.style.display = el.style.display === 'none' ? 'block' : 'none';
    }
    function ghAcmmCreateIssueDirect(repo, id, level) {
      acmmCloseDialog();
      acmmCreateIssueDirectFromPicker(repo, id, level);
    }
    function ghAcmmCloseAndShowLevel(level) {
      acmmCloseDialog();
      acmmShowLevelDialog(level);
    }
    function ghOpenUrl(url) {
      hiveExportViewer({title:'External link',text:String(url || ''),sourceURL:String(url || ''),allowOpen:true});
    }
    const HIVE_GEN_HANDLERS = {
      gh70: function (event, A) { saveGovGeneralAdvanced('eval_interval_s', this.value === '' ? null : parseInt(this.value,10)); },
      // '' is a real value for the hive-wide explain default (it clears it), so
      // this must NOT map empty to null the way gh70 does.
      gh75: function (event, A) { saveGovGeneralAdvanced('explain_mode', this.value, { note: 'Default explain mode updated — applies from the next kick', refresh: function (data) { const n = document.getElementById('gov-explain-default-note'); if (n) n.innerHTML = explainDefaultNoteHtml(data); } }); },
      gh71: function (event, A) { markDirty('auto-merge','max_merges',Number(this.value)||0); },
      gh72: function (event, A) { markDirty('auto-merge','required_checks',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh73: function (event, A) { markDirty('review','max_parallel_reviews',Number(this.value)||0); },
      gh74: function (event, A) { markDirtyReviewerAgents(); },
      gh81: function (event, A) { markDirty('review','max_perspectives_per_pr',Number(this.value)||0); },
      // Perspectives and their prompts are staged together on every change:
      // the server validates them as a pair (a custom perspective is only
      // valid with its prompt), and an absent key means "leave alone", so a
      // partial write could orphan one half.
      gh91: function (event, A) { markDirtyPerspectives(); },
      gh92: function (event, A) { markDirtyPerspectives(); },
      gh93: function (event, A) { const row = document.querySelector(`.rv-perspective[data-perspective="${CSS.escape(A[0])}"]`); if (row) row.remove(); markDirtyPerspectives(); },
      gh94: function (event, A) { const row = document.querySelector(`.rv-perspective[data-perspective="${CSS.escape(A[0])}"]`); const ta = row && row.querySelector('.rv-perspective-focus'); if (ta) { ta.value = ''; } markDirtyPerspectives(); },
      gh96: function (event, A) { markNotificationEventsDirty(); },
      // Trusted bot authors are sent as ONE full list (an empty list is a
      // deliberate "App-only"), so every toggle/add/remove rebuilds it from
      // the rows currently on screen.
      gh97: function (event, A) {
        const isOn = this.classList.toggle('on');
        this.setAttribute('aria-checked', isOn ? 'true' : 'false');
        markDirtyTrustedBots();
      },
      gh98: function (event, A) {
        const input = document.getElementById('am-new-bot');
        const login = (input.value || '').trim();
        if (!login || /\s/.test(login)) { showToast('Enter a single GitHub login, e.g. renovate[bot]', 'error'); return; }
        const existing = Array.from(document.querySelectorAll('#am-trusted-bots .am-bot-row')).find(r => (r.dataset.botLogin || '').toLowerCase() === login.toLowerCase());
        if (existing) {
          const sw = existing.querySelector('.am-bot-switch');
          if (sw && !sw.classList.contains('on')) { sw.classList.add('on'); sw.setAttribute('aria-checked', 'true'); }
          input.value = ''; markDirtyTrustedBots(); return;
        }
        const anchorEl = input.parentElement;
        const row = document.createElement('div');
        row.className = 'config-toggle am-bot-row'; row.dataset.botLogin = login;
        row.innerHTML = `<div class="config-toggle-switch am-bot-switch on" data-action="gh97" data-arg0="${esc(login)}" data-arg-types="s" role="switch" aria-checked="true"></div><span class="config-toggle-label"><code>${esc(login)}</code> <span class="am-bot-source">added manually</span> <button type="button" class="hv-btn btn-secondary btn-sm btn-small" data-action="gh99" data-arg0="${esc(login)}" data-arg-types="s">remove</button></span>`;
        anchorEl.parentElement.insertBefore(row, anchorEl);
        input.value = '';
        markDirtyTrustedBots();
      },
      gh99: function (event, A) {
        const row = Array.from(document.querySelectorAll('#am-trusted-bots .am-bot-row')).find(r => (r.dataset.botLogin || '').toLowerCase() === String(A[0]).toLowerCase());
        if (row) row.remove();
        markDirtyTrustedBots();
      },
      gh100: function (event, A) {
        const isOn = this.classList.toggle('on');
        this.setAttribute('aria-checked', isOn ? 'true' : 'false');
        markDirtyTrustedAuthors();
      },
      gh101: function (event, A) { markDirtyTrustedAuthors(); },
      gh102: function (event, A) { markDirtyTrustedAuthors(); },
      gh103: function (event, A) {
        const isOn = this.classList.toggle('on');
        this.setAttribute('aria-checked', isOn ? 'true' : 'false');
        markDirtyContributorLane();
      },
      gh104: function (event, A) { markDirtyContributorLane(); },
      gh105: function (event, A) { markDirty('logging','readScanMaxMB',Math.max(0, Number(this.value) || 0)); },
      gh95: function (event, A) {
        const input = document.getElementById('rv-new-perspective');
        const name = (input.value || '').trim().toLowerCase();
        if (!/^[a-z][a-z0-9]*(-[a-z0-9]+)*$/.test(name)) { showToast('Perspective names are lowercase letters, digits and single dashes', 'error'); return; }
        if (document.querySelector(`.rv-perspective[data-perspective="${CSS.escape(name)}"]`)) { showToast(`${name} already exists`, 'error'); return; }
        const anchorEl = input.parentElement;
        const row = document.createElement('div');
        row.className = 'rv-perspective'; row.dataset.perspective = name;
        row.style.cssText = 'margin-top:var(--sp-4);padding:8px 10px;border:1px solid var(--border);border-radius:6px';
        row.innerHTML = `<label style="display:flex;align-items:center;gap:var(--sp-4);cursor:pointer;font-weight:600;font-size:0.8rem"><input type="checkbox" class="rv-perspective-on" checked data-change-action="gh91"><code>${esc(name)}</code> <span style="font-size:var(--fs-xs);color:var(--muted);font-weight:400">custom</span><button type="button" class="hv-btn btn-secondary btn-sm btn-small btn-primary" data-action="gh93" data-arg0="${esc(name)}" data-arg-types="s">remove</button></label><textarea class="rv-perspective-focus" rows="2" style="width:100%;margin-top:var(--sp-3);font-size:0.76rem" placeholder="what should this perspective look for?" data-change-action="gh92"></textarea>`;
        anchorEl.parentElement.insertBefore(row, anchorEl);
        input.value = '';
        row.querySelector('textarea').focus();
        markDirtyPerspectives();
      },
      // The recommendations block is sent to /api/config/review as ONE object,
      // so every control rebuilds it from all three inputs rather than
      // patching a single key — a per-key markDirty would drop whichever
      // sibling the operator did not touch.
      gh82: function (event, A) { this.classList.toggle('on'); markDirtyRecommendations(); },
      gh83: function (event, A) { markDirtyRecommendations(); },
      gh84: function (event, A) { markDirtyRecommendations(); },
      gh77: function (event, A) { projectObservabilityPlatformsChanged(this); },
      gh78: function (event, A) { projectObservabilityReferenceChanged(this); },
      gh79: function (event, A) { markDirty('repos','maxIssuesPerKick',Number(this.value)); },
      gh80: function (event, A) { markDirty('repos','maxPRsPerKick',Number(this.value)); },
      gh0: function (event, A) { document.getElementById('gh-setup-overlay').style.display='none'; },
      gh1: function (event, A) { openConfigDialog('governor',null,'Repos'); },
      gh2: function (event, A) { acmmSelectRepo(null); },
      gh3: function (event, A) { event.stopPropagation(); },
      gh5: function (event, A) { openConfigDialog('governor',null,'Thresholds'); },
      gh6: function (event, A) { this.parentElement._zoomIn(); },
      gh7: function (event, A) { this.parentElement._zoomReset(); },
      gh8: function (event, A) { this.parentElement._zoomOut(); },
      gh9: function (event, A) { this.parentElement._toggleFullscreen(); },
      gh10: function (event, A) { _kbSetupView='git';renderKnowledge(); },
      gh11: function (event, A) { _kbSetupView='webhook';renderKnowledge(); },
      gh12: function (event, A) { _kbSetupView='none';renderKnowledge(); },
      gh13: function (event, A) { _kbSetupView='choose';renderKnowledge(); },
      gh14: function (event, A) { _kbSelectedLayer='vault:'+A[0];kbSearchVault(A[0]); },
      gh15: function (event, A) { _kbSearchQuery=this.value;kbSearch(); },
      gh16: function (event, A) { _kbSearchQuery=this.value; },
      gh17: function (event, A) { kbReadFact(A[0]); },
      kbPreviousPage: function () { kbChangePage(-1); },
      kbNextPage: function () { kbChangePage(1); },
      gh18: function (event, A) { kbRemoveGitSource(A[0],A[1]); },
      gh19: function (event, A) { this.style.display='none'; },
      gh20: function (event, A) { this.style.borderColor='var(--blue)'; },
      gh21: function (event, A) { this.style.borderColor='var(--border)'; },
      gh22: function (event, A) { this.style.borderColor='var(--muted)'; },
      gh23: function (event, A) { acmmCloseDialog();acmmShowCodebaseDialog(); },
      gh24: function (event, A) { acmmCloseDialog();acmmShowOpsDialog(); },
      gh25: function (event, A) { this.style.background='var(--bg)'; },
      gh26: function (event, A) { this.style.background='transparent'; },
      gh27: function (event, A) { selfUpgrade(A[0]); },
      gh28: function (event, A) { this.style.filter='brightness(1.15)'; },
      gh29: function (event, A) { this.style.filter=''; },
      gh30: function (event, A) { document.getElementById('import-preview').style.display='none'; },
      gh31: function (event, A) { markDirty('hub','contribute_cooldown_hours',parseInt(this.value,10)||168); },
      gh32: function (event, A) { updateConfigAgentName(this.value);markDirty('general','displayName',this.value); },
      gh33: function (event, A) { markDirty('general','sortOrder',parseInt(this.value,10)); },
      gh34: function (event, A) { markDirty('general','replicas',Math.max(1,Math.min(MAX_AGENT_REPLICAS,parseInt(this.value,10)||1)));this.value=Math.max(1,Math.min(MAX_AGENT_REPLICAS,parseInt(this.value,10)||1)); },
      gh35: function (event, A) { markDirty('general','staleTimeout',cadenceFromHuman(this.value));this.value=cadenceToHuman(cadenceFromHuman(this.value)); },
      // Repo scope (#6204). An empty field is meaningful — it CLEARS the scope
      // and returns the agent to hive-wide — so it marks dirty with [] rather
      // than skipping, which is why it cannot just reuse gh36.
      gh39repos: function (event, A) { markDirty('general','repos',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh36: function (event, A) { markDirty('general','laneKeywords',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh37: function (event, A) { markDirty('general','detectKeywords',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh38: function (event, A) { markDirty('general','aliases',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh39: function (event, A) { cadenceChanged(A[0]);renderConfigTab(_configState.tab); },
      gh41: function (event, A) { closeConfigDialog();openACMMDialog(); },
      gh42: function (event, A) { saveTrajectoryField('intervalS', this.value === '' ? 0 : parseInt(this.value,10)); },
      gh43: function (event, A) { markDirty('budget','totalTokens',Number(this.value)); },
      gh44: function (event, A) { markDirty('budget','periodDays',Number(this.value)); },
      gh45: function (event, A) { markDirty('budget','criticalPct',Number(this.value)); },
      gh85: function (event, A) { markDirty('budget','usd',Number(this.value)); },
      gh86: function (event, A) { markDirty('budget','coinBudget',Number(this.value)); },
      gh87: function (event, A) { const id=this.value==='coins'?'gov-budget-coin':'gov-budget-usd'; const el=document.getElementById(id); if(el) el.focus(); },
      gh48: function (event, A) { markDirty('sensing','ttlSeconds',Number(this.value)); },
      gh49: function (event, A) { markDirty('sensing','pullbackSeconds',Number(this.value)); },
      gh50: function (event, A) { markDirty('logging','maxSizeMB',Number(this.value)); },
      gh51: function (event, A) { markDirty('logging','maxAgeDays',Number(this.value)); },
      gh52: function (event, A) { markDirty('logging','maxBackups',Number(this.value)); },
      gh53: function (event, A) { markDirty('security','ioscanClassifierWarnThreshold',Number(this.value)); },
      gh54: function (event, A) { markDirty('security','ioscanClassifierBlockThreshold',Number(this.value)); },
      gh55: function (event, A) { markDirty('security','reviewMaxParallelReviews',Number(this.value)); },
      gh56: function (event, A) { markDirty('security','reviewReviewerAgents',parseSecurityList(this.value)); },
      gh57: function (event, A) { markDirty('features','tracingSampleRatio',Number(this.value)); },
      gh58: function (event, A) { markDirty('features','otelHeaders',{});showToast('OTel headers will be cleared on Save','info'); },
      gh59: function (event, A) { markDirty('advisory','max_findings',Number(this.value)); },
      gh60: function (event, A) { markDirty('advisory','staleness_days',Number(this.value)); },
      gh76: function (event, A) { markDirty('advisory','update_interval_s',Number(this.value)||0); },
      gh61: function (event, A) { markWorkSourceDirty('type',this.value);renderConfigTab('Work Source'); },
      gh62: function (event, A) { markWorkSourceDirty('github_projects.project_number',Number(this.value)||0); },
      gh63: function (event, A) { markWorkSourceDirty('github_projects.states',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh64: function (event, A) { markWorkSourceDirty('linear.hold_labels',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh65: function (event, A) { markWorkSourceDirty('jira.project_keys',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh66: function (event, A) { markWorkSourceDirty('jira.hold_labels',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      gh77: function (event, A) { markWorkSourceDirty('jira.deployment',this.value);renderConfigTab('Work Source'); },
      gh78: function (event, A) { const v=!((_configState.data.work_source||{}).jira||{}).insecure_skip_verify; markWorkSourceDirty('jira.insecure_skip_verify',v);renderConfigTab('Work Source'); },
      wsGiteaReposChanged: function (event, A) { markWorkSourceDirty('gitea.repos',wsForgeReposFromText(this.value)); },
      wsGiteaLabelsChanged: function (event, A) { markWorkSourceDirty('gitea.labels',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsGiteaStatesChanged: function (event, A) { markWorkSourceDirty('gitea.states',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsGiteaHoldLabelsChanged: function (event, A) { markWorkSourceDirty('gitea.hold_labels',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsGitLabReposChanged: function (event, A) { markWorkSourceDirty('gitlab.repos',wsForgeReposFromText(this.value)); },
      wsGitLabLabelsChanged: function (event, A) { markWorkSourceDirty('gitlab.labels',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsGitLabStatesChanged: function (event, A) { markWorkSourceDirty('gitlab.states',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsGitLabHoldLabelsChanged: function (event, A) { markWorkSourceDirty('gitlab.hold_labels',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsExternalReposChanged: function (event, A) { markWorkSourceDirty('external.repos',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsExternalHoldLabelsChanged: function (event, A) { markWorkSourceDirty('external.hold_labels',this.value.split(',').map(s=>s.trim()).filter(Boolean)); },
      wsExternalTimeoutChanged: function (event, A) { markWorkSourceDirty('external.timeout_seconds',Number(this.value)||0); },
      gh79: function (event, A) { wsWavefrontSetSource(this.value); },
      gh80: function (event, A) { wsWavefrontSetLocation(this.dataset.source || 'path', this.value); },
      gh81: function (event, A) { markSentinelDirty('label', this.value.trim()); },
      gh82: function (event, A) { markSentinelDirty('maxActions', Number(this.value) || 0); },
      gh83: function (event, A) { markSentinelPathsFromText(this.value); },
      gh84: function (event, A) { markSentinelDirty('exemptLogins', parseSecurityList(this.value)); },
      gh85: function (event, A) { markSentinelDirty('repos', parseSecurityList(this.value)); },
    };


    // Configured per-agent stats (statsConfig). The old "Working on / PR open
    // / merged" rows that also lived here were fed by a script the Go
    // dashboard never ported (#128), so they never rendered; the agent's
    // "Now:" line below answers "what is it on?" instead (#10925).
    function agentIndicators(name) {
      const agentObj = ((window._lastStatus || {}).agents || []).find(a => a.name === name);
      const statsConfig = agentObj ? agentObj.statsConfig : null;
      if (statsConfig && statsConfig.length > 0) {
        return renderStatsFromConfig(statsConfig, name);
      }
      return '';
    }

    /* "Now:" line on agent cards and the agent detail panel (#10934, part of
       #10925). Built only from what the hive already sends: the agent's
       lastAction record, busy/lastKickAt, the payload timestamp (the hive's
       clock) and, when reservations are on, the claims ledger. Times are
       measured on the hive's clock so a wrong viewer clock never skews them. */
    const AGENT_NOW_UPDATE_MS = 10000;
    const AGENT_NOW_MISSED_UPDATES = 3;
    const AGENT_NOW_TICK_MS = 15000;
    const AGENT_NOW_CLAIMS_REFRESH_MS = 15000;
    const AGENT_NOW_HELP = 'What this agent is on, from what it does through the hive: the last issue or pull request it commented on, labelled, opened, reviewed or merged since it was last started. Work done outside the hive (a direct git push or command-line call) is not seen, so "no issue or pull request yet" does not mean it is doing nothing. "reserved until" is a reservation on the issue, not proof the agent is still working on it.';
    const AGENT_NOW_PR_ACTIONS = { agent_pr_created: true, pr_closed: true, agent_pr_reviewed: true, pr_merged: true, agent_review_requested: true };
    let _agentNowClock = null;
    let _agentNowClaims = { enabled: false, claims: [], fetchedAt: 0 };

    function agentNowClockFrom(timestamp, localMs) {
      const hiveMs = Date.parse(timestamp || '');
      return Number.isFinite(hiveMs) ? { hiveMs, localMs } : null;
    }

    function agentNowHiveMs(clock, localMs) {
      if (!clock) return localMs;
      return clock.hiveMs + Math.max(0, localMs - clock.localMs);
    }

    function agentNowIsOutdated(clock, localMs) {
      if (!clock) return false;
      return localMs - clock.localMs > AGENT_NOW_UPDATE_MS * AGENT_NOW_MISSED_UPDATES + AGENT_NOW_UPDATE_MS / 2;
    }

    function agentNowAgo(ms) {
      const sec = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
      if (sec < 60) return 'just now';
      const min = Math.floor(sec / 60);
      if (min < 60) return `${min} min ago`;
      const hours = Math.floor(min / 60);
      if (hours < 24) return hours === 1 ? '1 hour ago' : `${hours} hours ago`;
      const days = Math.floor(hours / 24);
      return days === 1 ? '1 day ago' : `${days} days ago`;
    }

    // HH:MM in the selected display time zone, with the date when it is not today.
    function agentNowClockTime(ms, hiveNowMs) {
      const d = new Date(ms);
      const time = formatDashboardTime(d, { hour:'2-digit', minute:'2-digit', timeZoneName:'short' });
      if (formatDashboardDate(d) === formatDashboardDate(hiveNowMs)) return time;
      return formatDashboardDateTime(d, { month:'short', day:'numeric', hour:'2-digit', minute:'2-digit', timeZoneName:'short' });
    }

    /* Resolves an item's repository against the repos this viewer is shown.
       The short name is used only when it names exactly one watched repo;
       otherwise the line says owner/repo. Titles come only from the issue and
       pull request lists already served for that repo, so a viewer who is not
       shown a repo's issues is not shown its titles here either. */
    function agentNowItem(repo, number, action, ctx) {
      const raw = String(repo || '').trim();
      const n = Number(number) || 0;
      if (!raw || n <= 0 || !Array.isArray(ctx.repos)) return null;
      const repos = ctx.repos;
      const lower = (v) => String(v || '').toLowerCase();
      let full = raw;
      let short = raw;
      if (raw.includes('/')) {
        short = raw.slice(raw.lastIndexOf('/') + 1);
      } else {
        const byName = repos.filter(r => lower(r.name) === lower(raw));
        full = byName.length === 1 && byName[0].full ? byName[0].full : (ctx.org ? ctx.org + '/' + raw : raw);
      }
      const watched = repos.find(r => lower(r.full) === lower(full)) || null;
      const sameShort = repos.filter(r => lower(r.name) === lower(short)).length;
      const label = (watched && sameShort === 1 ? short : full) + '#' + n;
      let title = '';
      let isPR = !!AGENT_NOW_PR_ACTIONS[action];
      if (watched) {
        for (const [key, pr] of [['actionableIssues', false], ['heldIssues', false], ['openPrs', true], ['heldPrs', true]]) {
          const hit = (watched[key] || []).find(i => i && Number(i.number) === n);
          if (hit) { title = hit.title || ''; isPR = isPR || pr; break; }
        }
      }
      const href = full.includes('/')
        ? `${ctx.ghBase || 'https://github.com'}/${full.split('/').map(encodeURIComponent).join('/')}/${isPR ? 'pull' : 'issues'}/${n}`
        : '';
      return { full, label, title, href };
    }

    function agentNowItemHtml(item, withTitle) {
      const ref = item.href
        ? `<a href="${esc(item.href)}" target="_blank" rel="noopener" data-stop="1">${esc(item.label)}</a>`
        : esc(item.label);
      return withTitle && item.title ? `${ref} — ${esc(item.title)}` : ref;
    }

    // Live reservations this agent holds, soonest end first. Empty when
    // reservations are off: nothing about them is shown then.
    function agentNowReservations(a, ctx) {
      const cl = ctx.claims || {};
      if (!cl.enabled || !Array.isArray(cl.claims)) return [];
      return cl.claims
        .filter(c => c && c.kind === 'agent' && (c.holder_id === a.name || c.holder === a.name))
        .map(c => ({ c, ends: Date.parse(c.expires_at || '') }))
        .filter(r => Number.isFinite(r.ends) && r.ends > ctx.hiveNowMs)
        .sort((x, y) => x.ends - y.ends);
    }

    /* st: { isPoweredOff, isOff, isPaused, isStopped, isStarting, stateLabel }
       ctx: { hiveNowMs, outdated, repos, org, ghBase, claims } */
    function agentNowLineHtml(a, st, ctx) {
      const outdated = !!ctx.outdated;
      const ago = (iso) => outdated ? '' : agentNowAgo(ctx.hiveNowMs - Date.parse(iso || ''));
      const parts = [];
      let lead = '';
      let showReservations = true;
      if (st.isPoweredOff || st.isOff) { lead = 'Off'; showReservations = false; }
      else if (st.isPaused) { lead = 'Paused'; showReservations = false; }
      else if (st.isStopped) {
        const sbLabel = agentSandboxDownLabel(a);
        lead = sbLabel ? sbLabel.charAt(0).toUpperCase() + sbLabel.slice(1) + " — see the agent's log" : "Stopped — see the agent's log";
        showReservations = false;
        const sandboxHtml = (sbLabel && !outdated) ? agentSandboxRunHtml(a, ctx.hiveNowMs) : '';
        if (sandboxHtml) parts.push(sandboxHtml);
      }
      else if (st.isStarting || (!a.lastKickAt && a.busy !== 'working')) { lead = 'Not started yet'; showReservations = false; }
      const la = a.lastAction || null;
      const item = (!lead && la) ? agentNowItem(la.repo, la.number, la.action, ctx) : null;
      const reservations = showReservations ? agentNowReservations(a, ctx) : [];
      let shownKey = '';
      if (!lead && a.busy === 'working') {
        if (item && la.current) {
          lead = 'Now:';
          shownKey = item.full.toLowerCase() + '#' + la.number;
          let seg = agentNowItemHtml(item, true);
          const age = ago(la.at);
          if (age) seg += ' · ' + age;
          const own = reservations.find(r => (String(r.c.repo || '').toLowerCase() + '#' + r.c.issue) === shownKey);
          if (own) seg += ' · reserved until ' + esc(agentNowClockTime(own.ends, ctx.hiveNowMs));
          parts.push(seg);
        } else {
          lead = 'Now:';
          let seg = 'working — no issue or pull request yet';
          if (a.lastKickAt && !outdated) seg += ' · started ' + agentNowAgo(ctx.hiveNowMs - Date.parse(a.lastKickAt));
          parts.push(seg);
        }
      } else if (!lead && a.busy === 'idle') {
        lead = 'Idle';
        if (item) {
          const age = ago(la.at);
          parts.push('last: ' + agentNowItemHtml(item, false) + (age ? ', ' + age : ''));
        }
        const sandboxHtml = outdated ? '' : agentSandboxRunHtml(a, ctx.hiveNowMs);
        if (sandboxHtml) parts.push(sandboxHtml);
      } else if (!lead) {
        lead = esc(st.stateLabel || a.busy || '');
        showReservations = false;
      }
      if (showReservations) {
        const others = reservations.filter(r => (String(r.c.repo || '').toLowerCase() + '#' + r.c.issue) !== shownKey);
        const first = others.length ? agentNowItem(others[0].c.repo, others[0].c.issue, '', ctx) : null;
        if (first) {
          let seg = 'still reserved: ' + agentNowItemHtml(first, false) + ' until ' + esc(agentNowClockTime(others[0].ends, ctx.hiveNowMs));
          if (others.length > 1) seg += ` and ${others.length - 1} more`;
          parts.push(seg);
        }
      }
      if (outdated) parts.push('information may be out of date');
      const body = lead === 'Now:' ? `<span class="an-lead">Now:</span> ${parts.join(' · ')}` : `<span class="an-lead">${lead}</span>${parts.length ? ' · ' + parts.join(' · ') : ''}`;
      return `<div class="agent-now${outdated ? ' outdated' : ''}" data-agent-now="${esc(a.name)}" title="${esc(AGENT_NOW_HELP)}">${body}</div>`;
    }

    // Same state derivation as the agent card (renderAgents) so the line
    // never disagrees with the state shown above it.
    function agentNowStateOf(a) {
      const isOnDemand = a.onDemand === true;
      const isPoweredOff = agentIsDisabled(a) && a.state !== 'running';
      const isPaused = a.paused === true && !isOnDemand;
      const isOff = agentHeldByMode(a) && !isPaused && !isOnDemand;
      const isStarting = !!a.starting && a.state !== 'running' && !isPaused && !isOff;
      const isStopped = agentSessionDown(a) && !isPaused && !isOff && !isOnDemand && !isStarting;
      return { isPoweredOff, isOff, isPaused, isStopped, isStarting, stateLabel: isOnDemand ? 'on demand' : a.busy };
    }

    // The state word on the agent card. The Agent activity row shows the same
    // word, so the two never disagree.
    function agentStateText(a) {
      const { isPoweredOff, isPaused, isOff } = agentNowStateOf(a);
      const isOnDemand = a.onDemand === true;
      return isPoweredOff ? 'powered off' : isOnDemand ? 'on demand' : isPaused ? 'paused' : isOff ? agentCadenceOffLabel() : (a.busy === 'working' && a.awaitingCI) ? 'Waiting on CI' : a.busy;
    }

    function agentNowContext(localMs) {
      return {
        hiveNowMs: agentNowHiveMs(_agentNowClock, localMs),
        outdated: agentNowIsOutdated(_agentNowClock, localMs),
        repos: (window._lastStatus || {}).repos,
        org: window._githubOrg || '',
        ghBase: window._githubBaseUrl || 'https://github.com',
        claims: _agentNowClaims,
      };
    }

    // House style (docs/house-style.md): which source the last kick's
    // "HOUSE STYLE" section came from. Empty until the agent is kicked.
    const STYLE_SOURCE_HELP = {
      'repo': 'HIVE_STYLE.md from the managed repository (overrides the hive default)',
      'hive': 'Hive-wide default from project.style / project.style_file in hive.yaml',
      'built-in': 'Built-in default: no repo HIVE_STYLE.md and no hive default configured',
    };
    function styleSourceHtml(a) {
      const src = a && a.styleSource;
      if (!src || !STYLE_SOURCE_HELP[src]) return '—';
      return `<span class="oc-style-source" data-source="${esc(src)}" title="${esc(STYLE_SOURCE_HELP[src])}">${esc(src)}</span>`;
    }

    function agentNowHtml(a) {
      return agentNowLineHtml(a, agentNowStateOf(a), agentNowContext(Date.now()));
    }

    // Live per-run line (#14576): "$0.42 · 38.1K tok · 4m 12s · tests ✓ · 3 files".
    // Rendered only while /api/status carries agents[].run (busy agents), from
    // the per-agent cumulative figures the token collector already keeps; the
    // server reports the delta since the kick's BusySince, so a reload does not
    // reset it. Missing signals render as "–" rather than a guess.
    const RUN_LINE_HELP = 'Live figures for the kick in progress: estimated cost and tokens since the kick started (token × list price, same basis as the Cost section), elapsed time, the last test-runner result visible in the transcript, and the last "N files changed" git summary. "–" means not observed yet.';
    function fmtRunElapsed(sec) {
      sec = Math.max(0, Math.floor(Number(sec) || 0));
      const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
      if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`;
      if (m > 0) return `${m}m ${String(s).padStart(2, '0')}s`;
      return `${s}s`;
    }
    function agentRunLineHtml(a) {
      const r = a && a.run;
      if (!r) return '';
      const sep = '<span class="rl-sep">·</span>';
      const cost = r.priced ? fmtUSD(r.costUSD || 0) : '$–';
      const toks = r.priced ? `${fmtTokens(r.tokens || 0)} tok` : '– tok';
      const tests = r.tests === 'pass' ? '<span class="rl-tests pass">tests ✓</span>'
        : r.tests === 'fail' ? '<span class="rl-tests fail">tests ✗</span>'
        : '<span class="rl-tests">tests –</span>';
      const files = (typeof r.filesChanged === 'number' && r.filesChanged >= 0)
        ? `${r.filesChanged} file${r.filesChanged === 1 ? '' : 's'}` : '– files';
      return `<div class="oc-run-line" data-agent-run="${esc(a.name)}" title="${esc(RUN_LINE_HELP)}"><span class="rl-cost">${cost}</span>${sep}<span class="rl-tokens">${toks}</span>${sep}<span class="rl-elapsed">${fmtRunElapsed(r.elapsedS)}</span>${sep}${tests}${sep}<span class="rl-files">${files}</span></div>`;
    }
    // --- fleet cost rotor (begin) ---
    // The RUNNING tile rotates its cost window — this hour → today → this
    // week → this month — one phrase at a time (#15614). The agent count
    // stays put; only the amount + window phrase crossfades. All four
    // phrases are rendered into the same grid cell so the tile is always as
    // wide as its widest phrase and never shifts or wraps. The rotation
    // index is module state so a status repaint keeps the current phrase.
    // The hour window keeps cents (it is small and moves fast); the longer
    // windows round to whole dollars (fmtUSDWhole) so the figure stays short.
    // Labels are short so the phrase fits the 140px gov-grid column.
    const FLEET_COST_WINDOWS = [
      { key: 'hour', label: 'this hr', usd: 'hourUSD', priced: 'hourPriced', fmt: fmtUSD },
      { key: 'day', label: 'today', usd: 'dayUSD', priced: 'windowsPriced', fmt: fmtUSDWhole },
      { key: 'week', label: 'this wk', usd: 'weekUSD', priced: 'windowsPriced', fmt: fmtUSDWhole },
      { key: 'month', label: 'this mo', usd: 'monthUSD', priced: 'windowsPriced', fmt: fmtUSDWhole },
    ];
    const FLEET_COST_DWELL_MS = 4000;
    const FLEET_COST_WINDOWS_HELP = 'today / this week / this month are the cost ledger summed from 00:00 UTC today, Monday 00:00 UTC (ISO week) and the 1st 00:00 UTC, plus the rise since its last reading.';
    const _fleetCost = { idx: 0, started: false, hover: false, lastAdvance: -Infinity, clock: () => Date.now() };
    function fleetCostPhrases(f) {
      return FLEET_COST_WINDOWS.map(w => {
        const amount = f && f[w.priced] ? w.fmt(f[w.usd] || 0) : '$–';
        return { key: w.key, label: w.label, amount, text: `${amount} ${w.label}` };
      });
    }
    function fleetCostTooltipLines(f) {
      return fleetCostPhrases(f).map(p => p.text).join(' · ');
    }
    function fleetCostReducedMotion() {
      return !!(window.matchMedia && window.matchMedia('(prefers-reduced-motion: reduce)').matches);
    }
    function fleetCostRotorHtml(f) {
      const phrases = fleetCostPhrases(f);
      const idx = _fleetCost.idx % phrases.length;
      const spans = phrases.map((p, i) => `<span class="fcr-phrase${i === idx ? ' is-cur' : ''}" data-fleet-cost-window="${p.key}"${i === idx ? '' : ' aria-hidden="true"'}><span class="fcr-amount">${esc(p.amount)}</span> <span class="fcr-window">${esc(p.label)}</span></span>`).join('');
      return `<span class="fleet-cost-rotor" data-fleet-cost-rotor aria-live="off">${spans}</span>`;
    }
    // fleetCostApply moves one rotor from prevIdx to idx. Under reduced
    // motion the outgoing phrase is never marked, so the swap is instant.
    function fleetCostApply(el, prevIdx, idx) {
      const reduced = fleetCostReducedMotion();
      el.querySelectorAll('.fcr-phrase').forEach((p, i) => {
        const cur = i === idx;
        p.classList.toggle('is-cur', cur);
        p.classList.toggle('is-out', !reduced && i === prevIdx && !cur);
        if (cur) p.removeAttribute('aria-hidden'); else p.setAttribute('aria-hidden', 'true');
      });
    }
    function fleetCostPaused() {
      return _fleetCost.hover || document.hidden === true;
    }
    // fleetCostTick advances every rotor on the page by one window; while
    // hovered or hidden it leaves the index alone so the cadence resumes in
    // step instead of jumping.
    function fleetCostTick() {
      if (fleetCostPaused()) return;
      // dashboardSetInterval re-fires the callback the moment the tab is
      // visible again; hold that early call so the cadence resumes in step.
      const t = _fleetCost.clock();
      if (t - _fleetCost.lastAdvance < FLEET_COST_DWELL_MS / 2) return;
      _fleetCost.lastAdvance = t;
      const prev = _fleetCost.idx;
      _fleetCost.idx = (prev + 1) % FLEET_COST_WINDOWS.length;
      document.querySelectorAll('[data-fleet-cost-rotor]').forEach(el => fleetCostApply(el, prev, _fleetCost.idx));
    }
    function fleetCostHostOf(node) {
      return node && node.closest ? node.closest('[data-fleet-cost-host]') : null;
    }
    // The rotor rides the shared dashboard interval registry (paused while
    // the tab is hidden) rather than adding a bare setInterval.
    function fleetCostRotorStart() {
      if (_fleetCost.started) return;
      _fleetCost.started = true;
      dashboardSetInterval('fleetCostRotor', fleetCostTick, FLEET_COST_DWELL_MS);
      document.addEventListener('pointerover', e => { _fleetCost.hover = !!fleetCostHostOf(e.target); });
      document.addEventListener('pointerout', e => { _fleetCost.hover = !!fleetCostHostOf(e.relatedTarget); });
    }
    // --- fleet cost rotor (end) ---

    // fleetRunStatHtml is the Governor-section twin of fleetRunStripHtml: the
    // compact strip it first landed in is display:none while the full
    // Governor section stays visible in focus mode, so without this stat the
    // fleet figure was never on screen.
    function fleetRunStatHtml() {
      const f = window._lastStatus?.fleetRun;
      if (!f) return '';
      fleetCostRotorStart();
      return `<div class="gov-stat fleet-run-stat" data-fleet-cost-host title="RUNNING — agents mid-kick right now · estimated spend accrued by the whole fleet (token × list price; $– when a running agent's model is unpriced).&#10;&#10;${esc(fleetCostTooltipLines(f))}&#10;&#10;${FLEET_COST_WINDOWS_HELP}&#10;&#10;Updates with every status poll; the per-agent breakdown is on each card's run line in Agents."><div class="label">running</div><div class="val" id="gov-fleet-run"><span class="fleet-run-count">${f.running || 0} · </span>${fleetCostRotorHtml(f)}</div></div>`;
    }
    function fleetRunStripHtml() {
      const f = window._lastStatus?.fleetRun;
      if (!f) return '';
      fleetCostRotorStart();
      return `<div class="gov-metric fleet-run-stat" data-fleet-cost-host title="Agents mid-kick right now · estimated spend accrued by the whole fleet (token × list price)&#10;&#10;${esc(fleetCostTooltipLines(f))}&#10;&#10;${FLEET_COST_WINDOWS_HELP}"><span class="gm-label">running</span><span class="gm-val" id="oc-fleet-run"><span class="fleet-run-count">${f.running || 0} · </span>${fleetCostRotorHtml(f)}</span></div>`;
    }

    // Repaints every "Now:" line in place so "ago" keeps counting up (and
    // turns into "information may be out of date") without new data.
    function refreshAgentNowLines() {
      renderAgentActivity();
      const agents = window._lastAgents || [];
      document.querySelectorAll('[data-agent-now]').forEach(el => {
        const a = agents.find(ag => ag.name === el.dataset.agentNow);
        if (!a) return;
        const html = agentNowHtml(a);
        if (el.outerHTML !== html) el.outerHTML = html;
      });
    }

    function refreshAgentNowClaims(localMs) {
      if (localMs - _agentNowClaims.fetchedAt < AGENT_NOW_CLAIMS_REFRESH_MS) return;
      _agentNowClaims.fetchedAt = localMs;
      fetch('/api/claims', { cache: 'no-store' })
        .then(r => r.ok ? r.json() : null)
        .then(d => {
          if (!d) return;
          _agentNowClaims = { enabled: d.enabled === true, claims: Array.isArray(d.claims) ? d.claims : [], fetchedAt: _agentNowClaims.fetchedAt };
          refreshAgentNowLines();
        })
        .catch(() => {});
    }

    // Called with every full or per-agent status payload.
    function noteAgentNowUpdate(timestamp) {
      const localMs = Date.now();
      _agentNowClock = agentNowClockFrom(timestamp, localMs)
        || (_agentNowClock ? { hiveMs: agentNowHiveMs(_agentNowClock, localMs), localMs } : null);
      refreshAgentNowClaims(localMs);
      if (!DASHBOARD_INTERVALS.agentNow) dashboardSetInterval('agentNow', refreshAgentNowLines, AGENT_NOW_TICK_MS);
    }

    /* Agent activity section (#10935, part of #10925): one row per agent,
       built from the same agent list as the cards and the same "Now:" helpers
       (agentStateText, agentNowHtml, agentNowItem, agentNowReservations), so a
       row and its card never disagree. It shows only agents the hive already
       sends this viewer in the status payload. */
    let _agentActivityAgents = [];

    // Time since the agent's last action, or since it started when it is
    // working with no issue or pull request yet. Empty when the item is not
    // shown to this viewer or the information may be out of date.
    function agentActivitySinceText(a, ctx) {
      if (ctx.outdated) return '';
      const la = a.lastAction || null;
      if (a.busy === 'working' && !(la && la.current) && a.lastKickAt) {
        return 'started ' + agentNowAgo(ctx.hiveNowMs - Date.parse(a.lastKickAt));
      }
      if (la && la.at && agentNowItem(la.repo, la.number, la.action, ctx)) {
        return agentNowAgo(ctx.hiveNowMs - Date.parse(la.at));
      }
      return '';
    }

    // Every live reservation the agent holds, soonest end first.
    function agentActivityReservationsHtml(a, ctx) {
      return agentNowReservations(a, ctx)
        .map(r => {
          const item = agentNowItem(r.c.repo, r.c.issue, '', ctx);
          return item ? agentNowItemHtml(item, false) + ' until ' + esc(agentNowClockTime(r.ends, ctx.hiveNowMs)) : '';
        })
        .filter(Boolean)
        .join('<br>');
    }

    function agentActivityRowHtml(a, nowHtml, ctx) {
      const since = agentActivitySinceText(a, ctx);
      const reserved = agentActivityReservationsHtml(a, ctx);
      return `<tr data-agent-activity="${esc(a.name)}">
        <td class="aa-name">${a.emoji ? esc(a.emoji) + ' ' : ''}${esc(a.displayName || a.name)}</td>
        <td class="aa-state">${esc(agentStateText(a))}</td>
        <td class="aa-now">${nowHtml}</td>
        <td class="aa-since">${since ? esc(since) : '—'}</td>
        <td class="aa-reserved">${reserved || '—'}</td>
      </tr>`;
    }

    // Called with the agent list the cards are built from, and with no
    // argument on the "Now:" tick so the times keep counting up.
    function renderAgentActivity(agents) {
      if (Array.isArray(agents)) _agentActivityAgents = agents;
      const el = document.getElementById('agent-activity-panel');
      if (!el) return;
      refreshSectionCardShell('agent-activity-section');
      const live = window._lastAgents || [];
      const list = _sortAgentsBySidebar(_agentActivityAgents).sorted
        .map(a => live.find(ag => ag.name === a.name) || a);
      setSectionSummary('agent-activity-section', `${list.filter(a => a.busy === 'working').length} working / ${list.length} total`);
      if (list.length === 0) {
        setIfChanged(el, '<div class="agent-activity-empty">No agents to show.</div>');
        return;
      }
      const ctx = agentNowContext(Date.now());
      const rows = list.map(a => agentActivityRowHtml(a, agentNowHtml(a), ctx)).join('');
      setIfChanged(el, `<div class="agent-activity-scroll"><table class="agent-activity-table">
        <thead><tr><th scope="col">Agent</th><th scope="col">State</th><th scope="col">Now</th><th scope="col">Since last action</th><th scope="col">Reserved</th></tr></thead>
        <tbody>${rows}</tbody>
      </table></div>`);
    }

    /* ---- Hive activity monitor (#12599) ----------------------------------
       One Domino's-style tracker strip per open work item from
       GET /api/stage-tracker. Issues and PRs share ONE strip:
       Open → 010 Actionable → 020 Claimed → 030 PR in progress →
       040 Covered by PR → 050 Review → 080 Merge admission → 095 Soaking →
       100 Merge queue → ✓ merged. An issue keeps advancing with its
       covering PR's stage; a PR filed without an issue shows the issue
       steps as passed. 060 Reviewed, 095 Soaking and 100 Merge queue light
       Merge admission (the chevron word and tooltip name the real stage);
       070/072/074 ride Review and 085/090 ride Merge admission as detour
       badges. An ISSUE parked on a
       person wears 045 (owner decision: 085 is PR-only) before any PR
       exists and so rides Actionable (the issue side), not the merge gate;
       a pre-045 issue still wearing 085 is drawn the same way until the
       reconciler re-stages it.
       stageTrackerRowHtml() is pure (no DOM) so stage_tracker_ui_rows_test.go can
       execute it under node. */
    const STAGE_TRACKER_POLL_MS = 30000;
    const STAGE_TRACKER_PATH = [0, 10, 20, 30, 50, 80];
    const STAGE_TRACKER_PR_COLUMN = 30;
    const STAGE_TRACKER_MERGE_COLUMN = 80;
    const STAGE_TRACKER_NEEDS_DECISION = 85;
    const STAGE_TRACKER_ISSUE_NEEDS_DECISION = 45;
    // 090 under a manual-merge lane rule (payload manual_merge: CI workflow /
    // action edits, removed tests, a human-merge path): the hive never merges
    // it itself, so the lit chevron says what is actually owed — the
    // maintainer's merge click — and names who already approved the head
    // (payload human_approver) instead of the generic stage word.
    const STAGE_TRACKER_READY_FOR_HUMAN_MERGE = 90;
    const STAGE_TRACKER_MANUAL_MERGE_WORD = 'Awaiting human merge';
    // Every 090 word also says WHY a person merges (payload
    // human_merge_reason, a prstage.HumanMergeReason* code such as
    // "workflow-files" / "untrusted-author" / "branch-protection", falling
    // back to "policy"); the tooltip carries the payload's one-sentence
    // human_merge_reason_text (hivecommons/hive#14658).
    const STAGE_TRACKER_HUMAN_MERGE_REASON_WORD = { 'workflow-files': 'workflow files', 'tests-removed': 'tests removed', 'manual-merge-policy': 'manual-merge policy', 'branch-protection': 'branch protection', 'merge-method': 'merge method', 'auto-merge-off': 'auto-merge off', 'untrusted-author': 'untrusted author', 'unauthorized': 'unauthorized', 'reviewer-deferred': 'reviewer deferred', 'breaking-change': 'human merge', 'policy': 'policy' };
    // 090 under the breaking-change rule (human_merge_reason
    // "breaking-change": a ⚠️ title or needs-owner-merge label,
    // hivecommons/hive#15442): no approval lets the hive merge it, so the
    // word is the stage's own, "Ready for human merge", and the chip says
    // "human merge".
    const STAGE_TRACKER_BREAKING_CHANGE_CODE = 'breaking-change';
    const STAGE_TRACKER_READY_FOR_HUMAN_MERGE_WORD = 'Ready for human merge';

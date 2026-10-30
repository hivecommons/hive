    function parseCadenceMinutes(cadence) {
      if (!cadence || cadence === 'paused' || cadence === 'off' || cadence === 'on demand') return 0;
      const m = cadence.match(/^(\d+)(m|h)$/);
      if (!m) return 0;
      const MINUTES_PER_HOUR = 60;
      return m[2] === 'h' ? parseInt(m[1]) * MINUTES_PER_HOUR : parseInt(m[1]);
    }

    function getAgentIssueCount(name) {
      const m = currentAgentMetrics[name];
      if (!m) return 0;
      if (name === 'scanner') {
        const pairs = m.pairs || [];
        return pairs.filter(p => p.state === 'merged').length + (m.mergedPrs || []).length;
      }
      if (name === 'outreach') return m.outreachMerged || 0;
      if (name === 'architect') return m.prs || m.closed || 0;
      return 0;
    }

    // Per-agent estimated spend over the Cost section's active timeframe.
    // The chip shows on the card header (visible in compact and expanded);
    // the row adds detail inside the expanded card. Both return '' when the
    // history has no per-agent data covering the window.
    function agentSpendChip(name) {
      const usd = agentWindowSpend(name);
      const lbl = _costRange === 'custom' ? 'custom range' : `${COST_RANGES[_costRange].label} range`;
      const own = usd == null ? '' : `<span class="agent-spend-chip" title="Estimated spend over the ${lbl} — follows the Cost section's timeframe selector (token × list price)">${fmtUSD(usd)}</span>`;
      return own + advisorSpendChip(name, lbl);
    }
    function agentSpendRow(name) {
      const usd = agentWindowSpend(name);
      const lbl = _costRange === 'custom' ? 'custom range' : COST_RANGES[_costRange].label;
      const own = usd == null ? '' : `<div class="agent-field"><span class="label" title="Estimated cost attributed to this agent over the timeframe selected in the Cost section (token × list price)">spend (${lbl})</span><span class="value">${fmtUSD(usd)}</span></div>`;
      const adv = _advisorSpend[name];
      if (!adv || !adv.reviews) return own;
      return own + `<div class="agent-field"><span class="label" title="Advisor reviews of this agent over the same timeframe — the sum of the cost on its advisor records (GET /api/advisor/spend)">advisor spend (${lbl})</span><span class="value">${fmtUSD(adv.cost_usd || 0)} · ${adv.reviews} review${adv.reviews === 1 ? '' : 's'}${adv.skipped ? ` (${adv.skipped} skipped)` : ''}</span></div>`;
    }

    // Aggregated advisor spend per agent over the Cost section's active
    // timeframe (#9725), shown beside the agent's own spend. Keyed by agent;
    // refetched with the cost poll and whenever the range selector changes.
    var _advisorSpend = {};
    async function fetchAdvisorSpend() {
      try {
        // The Cost window's span is operator-adjustable (± stepper per
        // range), so the exact bounds travel rather than a preset name.
        const win = costWindow();
        const q = `since=${encodeURIComponent(new Date(win.start).toISOString())}&until=${encodeURIComponent(new Date(Math.min(win.end, Date.now())).toISOString())}`;
        const r = await fetch('/api/advisor/spend?' + q);
        if (!r.ok) return;
        const data = await r.json();
        const next = {};
        for (const a of (data.agents || [])) next[a.agent] = a;
        _advisorSpend = next;
      } catch { /* non-critical */ }
    }
    function advisorSpendChip(name, lbl) {
      const adv = _advisorSpend[name];
      if (!adv || !adv.reviews) return '';
      return `<span class="agent-spend-chip advisor-spend-chip" title="Advisor spend on this agent over the ${lbl}: ${adv.reviews} review${adv.reviews === 1 ? '' : 's'}">advisor ${fmtUSD(adv.cost_usd || 0)}</span>`;
    }

    function agentTokenRow(name, cadence) {
      const byAgent = window._tokensByAgent || {};
      const t = byAgent[name];
      if (!t || (t.messages === 0 && t.sessions === 0)) return '';
      const total = (t.input || 0) + (t.output || 0) + (t.cacheRead || 0);
      if (total === 0) {
        return `<div class="agent-field"><span class="label">tokens (24h)</span><span class="value text-muted">${t.sessions} sess · ${t.messages} msgs</span></div>`;
      }
      const avg = t.avgPerSession || (t.sessions > 0 ? Math.floor(total / t.sessions) : 0);
      let rows = `<div class="agent-field"><span class="label">tokens (24h)</span><span class="value">${fmtTokens(total)} <span style="color:var(--muted);font-size:0.65rem">(${t.sessions} sess)</span></span></div>`;
      rows += `<div class="agent-field"><span class="label">avg/pass</span><span class="value">${fmtTokens(avg)}</span></div>`;
      const issueCount = getAgentIssueCount(name);
      if (issueCount > 0) {
        const tokensPerIssue = Math.floor(total / issueCount);
        rows += `<div class="agent-field"><span class="label">avg/issue</span><span class="value">${fmtTokens(tokensPerIssue)} <span style="color:var(--muted);font-size:0.65rem">(${issueCount} today)</span></span></div>`;
      }
      const intervalMin = parseCadenceMinutes(cadence);
      if (intervalMin > 0 && avg > 0) {
        const MINUTES_PER_HOUR = 60;
        const HOURS_PER_WEEK = 168;
        const passesPerHour = MINUTES_PER_HOUR / intervalMin;
        const tokensPerHour = avg * passesPerHour;
        const tokensPerWeek = tokensPerHour * HOURS_PER_WEEK;
        rows += `<div class="agent-field"><span class="label">burn rate</span><span class="value">${fmtTokens(tokensPerHour)}/hr · ${fmtTokens(tokensPerWeek)}/wk</span></div>`;
      }
      return rows;
    }

    const TMUX_SESSION_PREFIX = 'hive-';
    const TTYD_PORT = 7681;
    const TERMINAL_ASSERTION_RENEW_PATH = '/api/terminal/assertion/renew';
    const TERMINAL_HANDOFF_PATH = '/api/terminal/handoff';
    /* sandbox.runtime: job agents (#14798) run each kick as a Kubernetes Job
       in another pod, so there is no hive-<agent> tmux session for ttyd to
       attach to. Their terminal is the read-only Job pod log viewer, served
       by the Go server under the same /terminal prefix so openTerminal()'s
       handoff code and assertion cookie apply unchanged. */
    const JOB_SANDBOX_RUNTIME = 'job';
    function agentUsesJobTerminal(a) {
      return !!a && a.sandboxRuntime === JOB_SANDBOX_RUNTIME;
    }
    function terminalUrl(agentName, sandboxRuntime) {
      const proto = window.location.protocol;
      const host = window.location.host;
      if (sandboxRuntime === JOB_SANDBOX_RUNTIME) {
        return `${proto}//${host}/terminal/job/${encodeURIComponent(agentName)}`;
      }
      const session = TMUX_SESSION_PREFIX + agentName;
      return `${proto}//${host}/terminal/?arg=${encodeURIComponent(session)}`;
    }
    function terminalLinkTitle(a, tmuxTitle) {
      return agentUsesJobTerminal(a)
        ? 'Stream the log of this agent\'s sandbox Job pod (read-only: it runs as a Kubernetes Job, not a tmux session)'
        : tmuxTitle;
    }
    /* The hosted fleet has moved apex once already (hive.kubestellar.io ->
       hive.hivecommons.dev) and spokes are also served at vanity hostnames, so
       a single hardcoded suffix silently turns assertion renewal into a no-op
       on every host it does not know: the 15-minute hive_terminal_assertion
       then expires with nothing to refresh it and ttyd's next reconnect is
       rejected. Keep every known hosted apex here, and keep it in step with
       DEFAULT_HOSTED_SUFFIXES in src/proxy/server.js. */
    const HOSTED_HIVE_HOST_SUFFIXES = ['.hive.kubestellar.io', '.hive.hivecommons.dev'];
    function isHostedHiveHost() {
      let host = (window.location.hostname || '').toLowerCase();
      if (host.endsWith('.')) host = host.slice(0, -1);
      return HOSTED_HIVE_HOST_SUFFIXES.some(suffix => host.endsWith(suffix));
    }
    /* renewTerminalAssertion re-mints the spoke-local terminal assertion from
       the spoke-local hive_session. It resolves { ok } and, on a status the
       server answered, a deferred `message` — NOT a toast. Renewal is
       best-effort: a hosted user signed in only through hub SSO has no
       hive_session yet, so renewal 401s while the handoff that follows
       (createTerminalHandoff) mints the assertion and opens the terminal fine.
       Toasting here told exactly those users "Sign in to this hive before
       opening a terminal" as their terminal opened; openTerminal() shows the
       message only when the handoff fails too. */
    async function renewTerminalAssertion() {
      if (!isHostedHiveHost()) return { ok: true };
      try {
        const resp = await fetch(TERMINAL_ASSERTION_RENEW_PATH, {
          method: 'POST',
          credentials: 'same-origin',
          cache: 'no-store',
        });
        if (resp.status === 204) return { ok: true };
        let message;
        if (resp.status === 401) message = 'Sign in to this hive before opening a terminal.';
        else if (resp.status === 403) message = 'Your current hive role cannot open terminals.';
        else message = 'Could not refresh terminal access (HTTP ' + resp.status + ').';
        return { ok: false, status: resp.status, message };
      } catch (e) {
        // A rejected fetch is how an expired hosted session shows up (the
        // status branches above never see it); the shared handler decides
        // whether this is "sign in again" or a real failure (#7405).
        await reportFetchFailure('Could not refresh terminal access', e);
      }
      return { ok: false };
    }

    async function dashboardTokenConfigured() {
      try {
        const r = await fetch('/api/auth/token', { cache: 'no-store' });
        /* A hosted hive has no shared token at all and answers this endpoint
           with 404 (see _ensureAuthToken, which reads the same 404 as "hosted
           hive, no token needed"). Reading every non-OK response as
           "configured" inverted that on exactly the hives that cannot have a
           token: when the handoff failed, openTerminal() closed the tab it had
           just opened instead of falling back to the plain terminal URL, which
           is what an operator sees as "clicking terminal does nothing". */
        if (r.status === 404) return false;
        if (!r.ok) return true;
        const d = await r.json().catch(() => ({}));
        return d.configured === true || d.configured === 'true';
      } catch {
        return true;
      }
    }

    async function createTerminalHandoff() {
      try {
        const resp = await fetch(TERMINAL_HANDOFF_PATH, {
          method: 'POST',
          credentials: 'same-origin',
          cache: 'no-store',
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(data.error || `HTTP ${resp.status}`);
        if (!data.code) throw new Error('server returned no handoff code');
        return data.code;
      } catch (e) {
        await reportFetchFailure('Could not prepare terminal access', e);
        return '';
      }
    }
    /* A DISABLED agent (enabled:false) is never started, so it has no tmux
       session and no pane to capture. Its ▶ terminal dead-ends on
       "no tmux socket found for session hive-<name>" and its 📄 full log on
       "capturing pane for <name>: exit status 1".

       It has a card at all because ApplyPack adds every agent in the ACMM
       level's pack to the runtime roster and gates only the Start on enabled
       (pkg/dashboard/api_packs.go) — deliberate, since the card is how an
       operator turns the agent on. So the card must SAY it is off rather than
       offer session actions that cannot work.

       state is checked first on purpose: an agent disabled while it is still
       up keeps running until the next reconcile and genuinely does have a
       session until then. */
    function agentIsDisabled(a) {
      return !!a && a.enabled === false;
    }
    function agentSessionUnavailable(a) {
      return agentIsDisabled(a) && a.state !== 'running';
    }
    /* kickOutcomeBadgeHtml names how the last kicked turn ENDED when it did not
       end in work (#7421). A turn that ended on a clarifying question ("What
       should I focus on?") is a defect — the governor re-kicks it early — and
       an explicit nothing-produced report is a no-op; both used to render as
       a plain "working"/"idle" state indistinguishable from an agent that
       opened a PR. A policy stand-down arrives as the BLOCKED badge above and
       is not repeated here. */
    function kickOutcomeBadgeHtml(a) {
      if (!a.kickOutcome) return '';
      const reason = a.kickOutcomeReason ? esc(a.kickOutcomeReason) : '';
      if (a.kickOutcome === 'question') {
        return '<span class="status-badge needs-context" title="Last kick ended on a clarifying question instead of work; the governor re-kicks it early.' + (reason ? ' — ' + reason : '') + '">asked for direction</span>';
      }
      if (a.kickOutcome === 'no-op') {
        return '<span class="status-badge needs-context" title="Last kick ended reporting nothing was produced.' + (reason ? ' — ' + reason : '') + '">no-op</span>';
      }
      return '';
    }
    /* repeatFailureLineHtml renders the repeat-failure check (#14550): the
       check state ("on", "not available for this backend", "not receiving
       data") and, while a command keeps failing the same way, the note.
       Monitor only: the agent is never paused for it. Renders nothing while
       the check is off — the hive sends no state then. */
    function repeatFailureLineHtml(a) {
      if (!a || (!a.repeatFailureCheck && !a.repeatFailureNote)) return '';
      const warn = a.repeatFailureCheck === 'not_covered' || a.repeatFailureCheck === 'no_data';
      const checkTip = a.repeatFailureCheck === 'no_data'
        ? 'A turn in progress ran shell commands, but no command record has arrived for repeat_failure.no_data_after_s. The backend may have changed how it runs commands.'
        : a.repeatFailureCheck === 'not_covered'
        ? 'Hive cannot record this backend\'s shell commands yet, so repeated failures are not noticed. See the repeated-failure table in the backend support docs.'
        : 'Hive records this agent\'s shell commands and notes a command that keeps failing the same way.';
      const check = a.repeatFailureCheckMessage
        ? `<span class="${warn ? 'rf-warn' : ''}" data-repeat-failure-check="${esc(a.repeatFailureCheck)}" title="${esc(checkTip)}"><span class="rf-key">repeat-failure</span>${esc(a.repeatFailureCheckMessage)}</span>`
        : '';
      const note = a.repeatFailureNote
        ? `<span class="rf-note" data-repeat-failure-note title="Monitor only: the agent is not paused.">${esc(a.repeatFailureNote)}</span>`
        : '';
      return `<div class="agent-repeat-failure">${note}${check}</div>`;
    }
    /* agentPowerSwitchHtml renders the master-power 0/1 rocker for an agent's
       `enabled` flag.

       This is a SHARED helper on purpose, mirroring pausedAgentActionHtml: the
       switch renders on the card grid, on the detail panel and in the config
       dialog, and the three drifted apart the last time each site spelled its
       own affordance. A pin test counts the references so a new render site
       cannot quietly hand-roll a fourth variant.

       Enablement is deliberately NOT folded into the pause toggle. Pausing is
       an operator hold the governor honours in every mode; disabling removes
       the agent from scheduling altogether. They are stored separately and
       clear separately, so they get separate controls. */
    /* agentCadenceOffLabel names the mode that is actually holding the agent.

       The per-mode cadence blocker used to render as a bare "off". That word
       reads as master power, so an operator seeing it went looking for an on/off
       control and found none — the agent was enabled and unpaused the whole
       time, and the only lever was a per-mode interval buried in the config
       dialog.

       The wording deliberately avoids "disabled in <mode>". `enabled` is a
       CROSS-MODE flag, so borrowing its word for a per-mode schedule gap
       re-creates the exact ambiguity this is meant to remove. "no cadence in
       <mode>" names the thing that is actually missing and points at the tab
       that supplies it, leaving "disabled" to mean the 0/1 switch alone. */
    function agentCadenceOffLabel() {
      const mode = (window._lastStatus && window._lastStatus.governor && window._lastStatus.governor.mode || '').toLowerCase();
      return mode ? `no cadence in ${mode}` : 'no cadence in this mode';
    }
    function pwrSwitchHtml(on, attrs, ariaLabel, title, extraClass = '') {
      const cls = `pwr-switch${on ? ' on' : ''}${extraClass ? ' ' + extraClass : ''}`;
      const titleAttr = title ? ` title="${esc(title)}"` : '';
      return `<span class="${cls}"${attrs || ''} role="switch" aria-checked="${on ? 'true' : 'false'}" aria-label="${esc(ariaLabel)}"${titleAttr}><span class="pwr-mark">1</span><span class="pwr-lever"></span><span class="pwr-mark">0</span></span>`;
    }
    function agentPowerSwitchHtml(a) {
      const on = !agentIsDisabled(a);
      const who = a.displayName || a.name;
      const canEdit = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const title = !canEdit
        ? `${who} is ${on ? 'enabled' : 'disabled'}. Owner role is required to change it.`
        : on
          ? `${who} is enabled (1). Switch to 0 to disable it — the governor stops scheduling it in every mode, independently of pause.`
          : `${who} is disabled (0). Switch to 1 to enable it — it returns to its configured per-mode cadences.`;
      const action = canEdit
        ? ` data-action="toggleAgentEnabled" data-agent="${esc(a.name)}" data-enabled="${on ? 'true' : 'false'}" data-stop="1"`
        : '';
      return pwrSwitchHtml(on, action, `${who} enabled`, title, `agent-power-switch${canEdit ? '' : ' readonly'}`);
    }
    /* Returns linkHtml unchanged for any agent that can have a session, and an
       inert chip carrying the reason for one that cannot. */
    function sessionChip(a, label, linkHtml) {
      if (!agentSessionUnavailable(a)) return linkHtml;
      const who = esc(a.displayName || a.name);
      return `<span class="hv-btn btn-secondary btn-sm terminal-link inert" title="${who} is disabled in config and has never started, so there is no session or log to open. Enable it with \u2699\uFE0F first.">${label}</span>`;
    }

    const TERMINAL_COPY_HINT_LS_KEY = 'hive-terminal-copy-hint-dismissed';
    function terminalCopyHintDismissed() {
      try { return localStorage.getItem(TERMINAL_COPY_HINT_LS_KEY) === '1'; }
      catch { return false; }
    }
    /* ttyd 1.7.7 does not handle OSC52 writes in its browser client, while
       tmux mouse mode owns ordinary drag selection. Put the escape hatch next
       to the terminal control rather than in a first-open toast that is gone
       by the time it matters.

       #5327: the ⇧-drag / Ctrl+Shift+C advice is useful the first time and
       pure noise on every agent card thereafter, so the baseline row is now a
       single small "keyboard copy" affordance carrying the advice in its
       tooltip, dismissible for good. The login-blocked row keeps its full
       sentence — that one reports whether a login URL was actually seen, so it
       earns the space and always wins over the persisted dismissal. */
    const TERMINAL_KEYBOARD_COPY_HINT = 'Selecting text in the terminal: hold ⇧ (Shift) while dragging — tmux mouse mode owns an ordinary drag. Then ⌘C (macOS) or Ctrl+Shift+C copies, and ⌘V or Ctrl+Shift+V pastes.';
    function terminalCopyHintHtml(needsLogin, sessionUnavailable, agentName, loginURL, backendAuthStatus, backendAuthLastError, statusEvidence, inferenceLastError) {
      if (sessionUnavailable) return '';
      /* #11761: inferenceLastError is the FULL error hive's inference proxy
         returned to this agent. Claude Code clips the same text to one pane
         row beside its "Retrying in Ns" countdown, and the pane-derived
         backendAuthLastError/statusEvidence inherit that clipping — so the
         proxy's copy wins whenever the server supplies it. */
      const fullError = (inferenceLastError || '').trim();
      const backendDetail = backendAuthLastError || ((statusEvidence || '').includes('backend_auth_error') ? statusEvidence : '');
      if (backendDetail && /Inference backend (?:outage|rejected credentials) \(401\)/.test(backendDetail)) {
        const backendTitle = backendDetail.includes('Inference backend outage (401)') ? 'Inference backend outage (401).' : 'Inference backend rejected credentials (401).';
        const shown = fullError ? `<span class="terminal-copy-hint-detail">${esc(fullError)}</span>` : esc(backendDetail);
        return `<div class="terminal-copy-hint backend-auth-error" role="note"><strong>${backendTitle}</strong> ${shown} <span class="terminal-copy-hint-help">Check the inference proxy / API key; this is not an interactive login.</span></div>`;
      }
      if (fullError && !needsLogin) {
        return `<div class="terminal-copy-hint backend-auth-error" role="note"><strong>Inference backend error.</strong> <span class="terminal-copy-hint-detail">${esc(fullError)}</span> <span class="terminal-copy-hint-help">Full message from hive's inference proxy; the CLI may still be retrying.</span></div>`;
      }
      /* #5327: the copy control is for a LOGIN link. It is rendered only
         where one is expected — see terminalCopyUrlButtonHtml — rather than
         standing on every card offering whatever URL the pane happens to
         hold. */
      if (!needsLogin) {
        /* Dismissed means gone. Before #5327 the row survived dismissal to
           keep the copy button alive; now that button only exists on a
           login-blocked card, which never reaches this branch, so there is
           nothing left to preserve. */
        if (terminalCopyHintDismissed()) return '';
        const help = `<button type="button" class="terminal-copy-hint-help" data-action="showTerminalKeyboardHint" title="${esc(TERMINAL_KEYBOARD_COPY_HINT)}" aria-label="How to copy text out of this terminal">⌨ keyboard copy</button>`;
        const dismiss = '<button type="button" class="terminal-copy-hint-dismiss" data-action="dismissTerminalCopyHint" aria-label="Dismiss terminal copy hint" title="Don’t show this hint again">×</button>';
        return `<div class="terminal-copy-hint" role="note">${help}${dismiss}</div>`;
      }
      const hasLoginURL = typeof loginURL === 'string' && loginURL.trim() !== '';
      if (!hasLoginURL) {
        return `<div class="terminal-copy-hint needs-login no-login-url" role="note">This agent is waiting on a login, but no login URL is visible on its terminal right now. Open the terminal and run the login command; do not use a copy-URL button until a URL appears.</div>`;
      }
      const copyBtn = terminalCopyUrlButtonHtml(agentName, hasLoginURL);
      const message = 'This agent is waiting on a login. A login URL is visible on its terminal — use the button to copy it whole (wrapped lines are rejoined), or hold <strong>Shift</strong> while selecting.';
      return `<div class="terminal-copy-hint needs-login" role="note">${message}${copyBtn}</div>`;
    }
    /* The tooltip is the primary surface; this is the click-through for
       touch and keyboard users, who never get a hover. */
    function showTerminalKeyboardHint() {
      showToast(TERMINAL_KEYBOARD_COPY_HINT, 'info');
    }
    function dismissTerminalCopyHint() {
      try { localStorage.setItem(TERMINAL_COPY_HINT_LS_KEY, '1'); } catch {}
      /* Remove the baseline row, matching what terminalCopyHintHtml renders
         on the next full re-render — the two paths must agree or the hint
         reappears on the next poll. The :not(.needs-login) selector is what
         protects the login-blocked row, which carries the copy control and is
         never dismissible. */
      document.querySelectorAll('.terminal-copy-hint:not(.needs-login)').forEach(el => el.remove());
    }

    /* Copies the LOGIN URL off the agent's pane. The capture is server-side
       with `capture-pane -J`, which rejoins terminal-wrapped lines, so a
       wrapped OAuth URL comes back whole — Shift-drag cannot do that, it
       copies the wrap's newlines along with the URL, which is the failure
       mode that makes a pasted login URL silently invalid.

       #5327/#9576: rendered ONLY when the pane extractor found an actual
       sign-in URL. A login prompt without a URL still gets an honest banner,
       but no copy-URL control.
       Naming the case rather than the mechanism is the other half: an
       operator who sees "Copy login URL" beside a 🔑 agent does not have to
       hover a tooltip to find out what it is for. */
    function terminalCopyUrlButtonHtml(agentName, hasLoginURL) {
      if (!agentName || !hasLoginURL) return '';
      return `<button type="button" class="terminal-copy-url-btn" data-action="copyTerminalUrl" data-agent="${esc(agentName)}" title="Show this agent\u2019s sign-in URL, captured whole from its terminal — wrapped lines are rejoined so the OAuth link is not broken by the wrap">🔑 Copy login URL</button>`
        + `<button type="button" class="terminal-copy-url-btn" data-action="submitLoginCode" data-agent="${esc(agentName)}" title="Paste the authorization code the sign-in page gave you. It is typed into the agent\u2019s terminal by the server, so the browser terminal cannot mangle it.">↩ Submit login code</button>`;
    }

    /* Writes text to the clipboard, reporting honestly when it cannot.
       navigator.clipboard exists only in a secure context, and a hive reached
       over plain http:// on a LAN address is not one — which is exactly how
       self-hosted hives are usually reached, so this is the common case, not
       the edge case. The execCommand fallback works there; when BOTH fail the
       caller gets false and must say so rather than pretend it copied. */
    async function writeClipboardText(text) {
      if (navigator.clipboard && window.isSecureContext) {
        try { await navigator.clipboard.writeText(text); return true; } catch {}
      }
      const ta = document.createElement('textarea');
      ta.value = text;
      // Off-screen but still focusable: execCommand('copy') ignores a hidden
      // or display:none element.
      ta.setAttribute('readonly', '');
      ta.style.position = 'fixed';
      ta.style.top = '-1000px';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      let ok = false;
      try {
        ta.select();
        ta.setSelectionRange(0, ta.value.length);
        ok = document.execCommand('copy');
      } catch { ok = false; }
      ta.remove();
      return ok;
    }

    async function copyHiveIdFromMenu() {
      const hiveId = (document.getElementById('oc-hive-id') || {}).textContent || '';
      if (!hiveId.trim()) return;
      const ok = await writeClipboardText(hiveId.trim());
      showToast(ok ? 'Hive ID copied to clipboard' : 'Could not copy the hive ID — select and copy it by hand', ok ? 'success' : 'error');
    }

    function renderPRAuthorInMenu() {
      const el = document.getElementById('oc-gh-menu-pr-author');
      if (!el) return;
      el.textContent = (window._aiAuthor || '').trim() || '—';
    }

    async function copyPRAuthorFromMenu() {
      const author = (document.getElementById('oc-gh-menu-pr-author') || {}).textContent || '';
      if (!author.trim() || author.trim() === '—') return;
      const ok = await writeClipboardText(author.trim());
      showToast(ok ? 'PR author copied to clipboard' : 'Could not copy the PR author — select and copy it by hand', ok ? 'success' : 'error');
    }

    /* The write-side twin of copyTerminalUrl.

       Copying OUT of the dashboard terminal is broken (#5188) and this is the
       same defect in the other direction: an operator who has opened the
       sign-in URL must paste a code BACK, and ttyd/xterm.js over tmux mangles
       it. Without this the only route left is a host shell and `tmux
       send-keys`, which is not an answer for an operator whose interface is a
       web dashboard.

       The code goes to the server, which types it into the pane — the browser
       terminal is never involved. It is refused rather than sanitised if it
       carries whitespace or control characters, so a value that could submit a
       second command into the agent's shell cannot be typed at all; the reason
       is surfaced verbatim rather than as a generic failure. */
    async function submitLoginCode(agentName) {
      if (!agentName) return;
      const code = await hivePrompt({title: 'Submit login code', label: `Paste the authorization code for "${agentName}".\n\nIt is sent to the server and typed into the agent's terminal, so the browser terminal cannot corrupt it.`});
      if (code === null) return;
      if (!code.trim()) { showToast('No code entered', 'error'); return; }
      try {
        const r = await fetch(`/api/agents/${encodeURIComponent(agentName)}/login-code`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ code: code.trim() }),
        });
        const d = await r.json().catch(() => ({}));
        if (!r.ok) { showToast(d.error || `Could not submit the code (HTTP ${r.status})`, 'error'); return; }
        showToast(`Code submitted to ${agentName}`, 'success');
      } catch (err) {
        showToast(`Could not submit the code: ${err && err.message ? err.message : err}`, 'error');
      }
    }

    async function copyTerminalUrl(agentName) {
      let urls = [];
      try {
        const r = await fetch(`/api/agents/${encodeURIComponent(agentName)}/terminal-urls`);
        if (!r.ok) throw new Error(`HTTP ${r.status}`);
        const d = await r.json();
        /* #5327: authUrls is the sign-in subset. The unfiltered `urls` list
           is deliberately NOT used as a fallback — offering "the newest URL
           on the pane" under a button labelled "Copy login URL" is precisely
           the mislabelling being fixed. */
        urls = Array.isArray(d.authUrls) ? d.authUrls : [];
      } catch (e) {
        showToast(`Could not read ${agentName}'s terminal: ${e.message || e}`, 'error');
        return;
      }
      if (!urls.length) {
        showToast(`No login URL on ${agentName}'s terminal right now`, 'info');
        return;
      }
      /* #5327: lead with the pre-selected field rather than a programmatic
         write. A self-hosted hive is routinely reached over plain http:// on
         a LAN address, which is not a secure context, so the write is BLOCKED
         more often than it succeeds here — and a blocked write spends the
         operator's click and returns instructions. Showing the URL, selected
         and ready for one Cmd/Ctrl+C, always works; the automatic write is
         still attempted underneath, and when it lands the panel just says so.
         Either way the operator ends the click holding the URL. */
      showTerminalUrlPanel(agentName, urls[0]);
    }

    /* Presents the login URL in a selectable, pre-selected input and reports
       whether the clipboard write also succeeded. This is the PRIMARY surface
       (#5327), not a last resort: it is the only path that works in every
       browser context this dashboard is served from. */
    async function showTerminalUrlPanel(agentName, url) {
      const toast = showToast(`Login URL from ${agentName}'s terminal — press Cmd/Ctrl+C to copy the selected text.`, 'info');
      /* An info toast self-dismisses on a timer, which would take the URL
         away mid-read. Stick it explicitly: this panel is the operator's copy
         surface and closes only when they say so. Sticking it directly rather
         than raising the toast to 'error' keeps it from reading as a failure
         — in the common path nothing failed. */
      if (toast) makeToastSticky(toast);
      const input = document.createElement('input');
      input.type = 'text';
      input.readOnly = true;
      input.value = url;
      input.className = 'terminal-copy-url-fallback';
      input.setAttribute('aria-label', `Login URL from ${agentName}'s terminal`);
      /* An error toast is made sticky, and makeToastSticky puts a
         dismiss-on-click listener on the WHOLE toast. Without this the first
         click into the input — the one action this panel exists to invite —
         would close the toast and take the URL with it. */
      input.addEventListener('click', e => e.stopPropagation());
      if (toast) toast.appendChild(input);
      else document.body.appendChild(input);
      input.focus();
      input.select();

      /* Try the clipboard too, but only report the OUTCOME — never let a
         refused write become the whole result of the click. */
      const ok = await writeClipboardText(url);
      // Re-select: the off-screen textarea execCommand uses steals the
      // selection, which would leave the operator's Cmd/Ctrl+C copying
      // nothing if the write was refused.
      input.focus();
      input.select();
      if (!toast || !toast.isConnected) return;
      const note = document.createElement('div');
      note.className = 'terminal-copy-url-note';
      note.textContent = ok
        ? '✓ Also copied to your clipboard.'
        : (window.isSecureContext
          ? 'This browser blocked the automatic clipboard write, so copy it by hand above.'
          : 'Automatic copying needs HTTPS or localhost and this hive is served over plain http, so copy it by hand above.');
      note.addEventListener('click', e => e.stopPropagation());
      toast.appendChild(note);
    }

    async function openTerminal(agentName, href) {
      const baseUrl = href || terminalUrl(agentName);
      // Open the tab synchronously, while the click's user activation is
      // still live. Browsers treat a window.open issued after an await as
      // non-user-initiated and block it, so the tab is reserved first and
      // navigated once the handoff code is minted. The URL modal below is
      // only the fallback for a genuinely blocked popup — it is not the
      // primary path, and it is still an in-app dialog, never a native one.
      const tab = window.open('about:blank', '_blank');
      if (tab) tab.opener = null;
      const navigate = url => {
        if (tab && !tab.closed) tab.location.href = url;
        else hiveExportViewer({title:'Terminal URL for ' + agentName, text:url, sourceURL:url, allowOpen:true});
      };
      const renewal = await renewTerminalAssertion();
      // Token-secured self-hosted hives do not have a hive_session cookie for
      // assertion renewal, but the handoff POST authenticates with the
      // Authorization header that the fetch wrapper attaches to mutations.
      const code = await createTerminalHandoff();
      if (code) {
        const url = new URL(baseUrl, window.location.href);
        url.searchParams.set('code', code);
        navigate(url.toString());
        return;
      }
      // The handoff could not mint the assertion either, so the renewal's
      // verdict is now the actionable one — surface it.
      if (renewal.message) showToast(renewal.message, 'error');
      if (!(await dashboardTokenConfigured())) {
        navigate(baseUrl);
      } else if (tab && !tab.closed) {
        tab.close();
      }
    }



    window.hiveOpenAuthenticatedChildLink = function(href, download) {
      const url = new URL(href, window.location.href).toString();
      openAuthenticatedLink(url, download);
    };
    async function openAuthenticatedLink(href, download) {
      try {
        const headers = {};
        const t = localStorage.getItem('hive-token');
        if (t) headers['Authorization'] = 'Bearer ' + t;
        const resp = await fetch(href, { credentials: 'same-origin', cache: 'no-store', headers });
        if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
        const contentType = resp.headers.get('Content-Type') || '';
        if (!download && contentType.includes('text/html')) {
          const html = await resp.text();
          hiveExportViewer({title:'Authenticated HTML',text:html,mime:contentType,sourceURL:href});
          return;
        }
        if (download) {
          hiveBinaryExportNotice('Authenticated file', 'This authenticated link points to a file response.');
          return;
        }
        const text = await resp.text();
        hiveExportViewer({title:'Authenticated content',text:text,mime:contentType,sourceURL:href});
      } catch (e) {
        showToast('Could not open link: ' + (e.message || e), 'error');
      }
    }

    // fullLogUrl points at the server-side endpoint that captures the agent's
    // full retained tmux scrollback (issue #3693). The live "▶ terminal" only
    // shows the last screenful; this returns the whole latest-run buffer as
    // plain, selectable, searchable text. The save-as-file query remains an
    // explicit API opt-in for external clients. Dashboard navigations never
    // carry the shared token in the URL; token-secured spokes use
    // authenticated fetches for API calls instead.
    /* A sandboxed agent (Podman or runtime: job) has no tmux pane: its full
       log is the container/Job stdout+stderr hive archived when the last run
       ended, so the tooltip says what the reader will get. */
    function fullLogTitle(a, paneTitle) {
      if (!a || !a.sandboxed) return paneTitle;
      const rt = (a.sandboxRun && a.sandboxRun.runtime === 'job') ? 'Job' : 'sandbox container';
      const when = (a.sandboxRun && a.sandboxRun.lastRunAt) ? ' (last run ' + a.sandboxRun.lastRunAt + ')' : '';
      return 'Captured stdout/stderr of the last sandboxed run — the ' + rt + ' output hive archived when the run ended' + when + '. Earlier runs: 🕘 kick history.';
    }

    function fullLogUrl(agentName, download) {
      const proto = window.location.protocol;
      const host = window.location.host;
      const dl = download ? 'download=1' : '';
      return `${proto}//${host}/api/agents/${encodeURIComponent(agentName)}/log${dl ? '?' + dl : ''}`;
    }

    // kickHistoryUrl points at the server-rendered index of the agent's
    // durable per-kick log archives (issue #4296): the last several kicks'
    // logs, surviving agent restarts and hive upgrades. Dashboard navigations never
    // carry the shared token in the URL.
    function kickHistoryUrl(agentName) {
      const proto = window.location.protocol;
      const host = window.location.host;
      return `${proto}//${host}/agents/${encodeURIComponent(agentName)}/kicks`;
    }

    // showAdvisorRecords opens the advisor lane's per-agent record list
    // (#9722): every review the advisor made of this agent's finished turns —
    // severity and text visible without further clicks, newest first, skips
    // included — beside the same audit timeline trajectory verdicts land on.
    async function showAdvisorRecords(agentName) {
      kbShowModal('🗣 Advisor — ' + escapeHtml(agentName), '<div class="muted">Loading…</div>', { width: '720px' });
      let bodyHtml = '';
      try {
        const res = await fetch('/api/advisor/records?agent=' + encodeURIComponent(agentName) + '&limit=50', { headers: _inceptionAuthHeaders() });
        if (!res.ok) throw new Error(await res.text());
        const data = await res.json();
        if (data.active === false) {
          bodyHtml += `<div class="advisor-note">⚠ ${escapeHtml(data.active_reason || 'advisor is not active for this agent')}</div>`;
        }
        const records = data.records || [];
        if (!records.length) {
          bodyHtml += '<div class="advisor-note">No advisor records for this agent yet.</div>';
        }
        const sevClass = s => s === 'blocker' ? 'text-danger' : (s === 'concern' ? 'text-warn' : 'text-muted');
        bodyHtml += records.map(rec => {
          const label = rec.skipped ? ('skipped: ' + rec.skipped) : (rec.severity || 'aside');
          const badges = [];
          if (rec.interjected) badges.push('interjected');
          if (rec.downgraded) badges.push('downgraded');
          const tokens = (rec.input_tokens || 0) + (rec.output_tokens || 0);
          if (tokens) badges.push(tokens + ' tok');
          return `<div class="advisor-rec">
            <strong class="${sevClass(rec.severity)}">${escapeHtml(label)}</strong>
            ${badges.length ? `<span class="muted"> · ${escapeHtml(badges.join(' · '))}</span>` : ''}
            <span class="advisor-rec-ts">${escapeHtml(rec.ts || '')}</span>
            ${rec.text ? `<div class="advisor-rec-text">${escapeHtml(rec.text)}</div>` : ''}
          </div>`;
        }).join('');
      } catch (e) {
        bodyHtml = `<div class="text-danger">Failed to load advisor records: ${escapeHtml(String(e && e.message || e))}</div>`;
      }
      kbShowModal('🗣 Advisor — ' + escapeHtml(agentName), bodyHtml, { width: '720px' });
    }

    /* SINGLE source of truth for the 🔑 "needs login" badge. Mirrors the
       server-side precedence rule in pkg/agent/authprobe.go:

         1. a.needsLogin (the pane poller literally saw a login prompt on the
            agent's own terminal) is authoritative — a genuinely wedged agent
            always shows the badge.
         2. The weaker probe-based signal (authKnown && !authAvailable) only
            counts when the agent is NOT running. A running agent is, by
            observation, working — the same signal /api/health/deep reports as
            `pass` — and a credential-file miss must never outrank that.
            Under the per-agent-UID layout the shared credential path is empty
            for every agent, which is exactly how running, authenticated
            agents ended up wearing a 🔑 badge.

       The server now also gates the probe on "does this backend even have an
       interactive login", so inference/API-key backends report no opinion. */
    function _agentNeedsLogin(a, isDown) {
      if (a.needsLogin === true) return true;
      // Callers that already computed a down-state pass it in (paused / off /
      // on-demand agents are NOT down and must keep their own styling —
      // #2465). Callers that have no such notion fall back to the raw
      // process state.
      const down = (isDown === undefined) ? (a.state !== 'running') : isDown;
      return down && a.authKnown === true && a.authAvailable !== true;
    }

    /* Agent blockers (#5594). THREE orthogonal flags decide whether an agent
       will ever do work: is its session up, has an operator paused it, and is
       a cadence configured at all. The card used to reveal them one at a
       time — a single action button cleared the first, which uncovered the
       second, and the third (no cadence in any mode) was not on the card at
       all — so operators walked a serial treasure hunt and still ended up
       with an agent that never ran.

       These read the RAW server flags rather than the mutually-exclusive
       display state (isPaused/isOff/isStopped, which deliberately suppress
       one another for styling), so every live blocker is legible BEFORE the
       first click. agentSchedulingBlockers returns them in the order an
       operator must clear them; empty means the governor really will kick
       this agent. */
    function agentSchedulingBlockers(a) {
      const out = [];
      if (a.paused === true) out.push('paused');
      if (a.offByCadence === true) {
        const mode = (window._lastStatus?.governor?.mode || '').toLowerCase();
        out.push(mode ? `off in ${mode} mode` : 'off in this governor mode');
      }
      if (a.unscheduledInMode === true) {
        // Some mode schedules it, the current one does not (#7474). Name
        // the modes that do, so the operator sees the ladder's hole.
        const mode = (window._lastStatus?.governor?.mode || '').toLowerCase();
        out.push(`${mode ? `no cadence in ${mode}` : 'no cadence in this mode'} (only in ${agentCadenceModesLabel(a)})`);
      }
      if (a.noCadence === true) out.push('never scheduled');
      return out;
    }
    function agentToggleTitle(a, isPaused, isOff) {
      if (isPaused) {
        const rest = [];
        if (a.state !== 'running') rest.push('its session is down (use \u21bb restart)');
        if (a.offByCadence === true) rest.push('its cadence for the current governor mode is off (\u2699\uFE0F \u2192 Cadences)');
        if (a.unscheduledInMode === true) rest.push(`it has a cadence only in ${agentCadenceModesLabel(a)}, none for the current governor mode (\u2699\uFE0F \u2192 Cadences)`);
        if (a.noCadence === true) rest.push('no governor mode gives it a cadence (\u2699\uFE0F \u2192 Cadences)');
        const base = 'Resume this agent \u2014 it will start running on its configured cadence';
        if (rest.length === 0) return base;
        return `${base}. Resuming alone is NOT enough: ${rest.join('; ')}.`;
      }
      // No caller passes isOff any more — cadenceOffAgentActionHtml owns that
      // case and carries its own tooltip. Kept correct rather than deleted so a
      // future caller cannot resurrect the old "Start this agent" lie, which
      // described a button that actually POSTed /api/pause.
      if (isOff) return 'This agent is enabled, but the governor has no cadence for it in the current mode (\u2699\uFE0F \u2192 Cadences)';
      return 'Pause this agent \u2014 stops governor from kicking it until resumed';
    }
    /* Combined primary action for a PAUSED agent \u2014 the follow-up to
       resume-leads (#5594). Resume-leads guarantees the pause is never
       hidden, and its tooltip names the session as a blocker resuming will
       not clear; this removes that residual two-step dance: when the session
       is ALSO down, the one button reads '\u25b6 start & resume' and clears both
       flags in a single click via startResumeAgent (a client-side chain of
       the two existing endpoints \u2014 resume, then restart \u2014 no new backend
       surface). A session-up paused agent keeps the plain '\u25b6 resume' with
       agentToggleTitle's blocker-naming tooltip. Every render site (card
       grid, detail panel, detail fast path) builds the paused action here,
       which is the single place that guarantees a paused agent never shows
       a bare Start. */
    /* Primary action for an agent held by its PER-MODE CADENCE.

       This is the counterpart to pausedAgentActionHtml and exists to close a
       real inversion bug. The detail panel and its fast path used to render a
       cadence-held agent as '▶ start' wired to toggleAgent with
       data-paused="false". toggleAgent re-syncs the real pause flag and then
       picks `paused ? resume : pause` — so for an agent that was never paused,
       the button labelled "start" POSTed /api/pause and stopped it. The card
       grid had it right; the two detail sites had drifted.

       A cadence hold has no start/stop endpoint to call: the only fix is to set
       an interval for the mode, so every site now routes to the Cadences tab.
       Building that here — like the paused action — is what keeps the three
       sites from drifting apart again. */
    function cadenceOffAgentActionHtml(a) {
      const who = esc(a.displayName || a.name);
      return `<button class="hv-btn btn-secondary btn-sm btn-toggle off" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(a.name)}" data-tab="Cadences" title="${who} is enabled, but the governor has no cadence for it in the current mode — click to set one">⚙ ${esc(agentCadenceOffLabel())}</button>`;
    }
    function pausedAgentActionHtml(a) {
      if (a.state === 'running') {
        return `<button class="hv-btn btn-danger btn-sm btn-toggle paused" data-action="toggleAgent" data-agent="${esc(a.name)}" data-paused="true" title="${esc(agentToggleTitle(a, true, false))}">\u25b6 resume</button>`;
      }
      const rest = [];
      if (a.offByCadence === true) rest.push('its cadence for the current governor mode is off (\u2699\ufe0f \u2192 Cadences)');
      if (a.unscheduledInMode === true) rest.push(`it has a cadence only in ${agentCadenceModesLabel(a)}, none for the current governor mode (\u2699\ufe0f \u2192 Cadences)`);
      if (a.noCadence === true) rest.push('no governor mode gives it a cadence (\u2699\ufe0f \u2192 Cadences)');
      let title = 'This agent is paused AND its session is down \u2014 one click resumes it and asks the supervisor to start the session. No second Resume step.';
      if (rest.length > 0) title += ` Still not enough on its own: ${rest.join('; ')}.`;
      return `<button class="hv-btn btn-danger btn-sm btn-toggle paused" data-action="startResumeAgent" data-agent="${esc(a.name)}" data-paused="true" title="${esc(title)}">\u25b6 start &amp; resume</button>`;
    }
    function neverScheduledChipHtml(a, configTarget) {
      if (a.noCadence !== true) return '';
      const who = a.displayName || a.name;
      const tip = `${who} is enabled but NO governor mode gives it a cadence, and it has never been kicked \u2014 it will sit idle forever. Click to set cadences.`;
      return `<button class="never-scheduled-chip" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(configTarget)}" data-tab="Cadences" title="${esc(tip)}">\u23F1 never scheduled \u2014 set cadences</button>`;
    }
    /* Sandboxed agents (podman or Kubernetes Job runtime) run each kick in a
       throwaway container and sit in state 'idle' between kicks: there is no
       tmux session or in-pod process to be "down". Their health comes from
       the server-side sandboxRun verdict (last run + cadence), never from
       state !== 'running'. */
    function agentSandboxRun(a) {
      return (a && a.sandboxed && a.sandboxRun && a.sandboxRun.health) ? a.sandboxRun : null;
    }
    function agentSessionDown(a) {
      const sr = agentSandboxRun(a);
      if (sr) return sr.health === 'failed' || sr.health === 'overdue';
      return a.state !== 'running';
    }
    function agentSandboxDownLabel(a) {
      const sr = agentSandboxRun(a);
      if (!sr) return '';
      if (sr.health === 'failed') return sr.timedOut ? 'last run timed out' : 'last run failed';
      if (sr.health === 'overdue') return 'run overdue';
      return '';
    }
    function agentSandboxRunText(a, hiveNowMs) {
      const sr = agentSandboxRun(a);
      if (!sr || !sr.lastRunAt) return '';
      const ago = agentNowAgo((hiveNowMs || Date.now()) - Date.parse(sr.lastRunAt));
      const dur = sr.durationS >= 60 ? Math.round(sr.durationS / 60) + ' min' : (sr.durationS || 0) + ' s';
      let text = (sr.succeeded ? 'last run succeeded' : (sr.timedOut ? 'last run timed out' : 'last run failed')) + ' \u00b7 ' + ago + ' \u00b7 ' + dur;
      if (sr.commitCount) text += ' \u00b7 ' + sr.commitCount + (sr.commitCount === 1 ? ' commit' : ' commits');
      return text;
    }
    function agentSandboxRunHtml(a, hiveNowMs) {
      const sr = agentSandboxRun(a);
      const text = agentSandboxRunText(a, hiveNowMs);
      if (!text) return '';
      let html = esc(text);
      if (sr.prURL) html += ' \u00b7 <a href="' + esc(sr.prURL) + '" target="_blank" rel="noopener" data-stop="1">PR</a>';
      if (sr.prWithheld) {
        // First line is the reason (verify failed / report status); the
        // command's output tail follows on later lines and stays in the tooltip.
        var reason = sr.prWithheld.split('\n')[0];
        html += ' \u00b7 <span title="' + esc(sr.prWithheld) + '">no PR opened: ' + esc(reason.length > 120 ? reason.slice(0, 120) + '\u2026' : reason) + '</span>';
      } else if (sr.error && !sr.succeeded) {
        html += ' \u2014 ' + esc(sr.error.length > 160 ? sr.error.slice(0, 160) + '\u2026' : sr.error);
      }
      return html;
    }
    function agentBlockerLineHtml(a) {
      const onDemand = a.onDemand === true;
      let session = 'up', sessionCls = 'ab-ok';
      let sessionTip = 'The tmux session is alive.';
      const sandboxRun = agentSandboxRun(a);
      if (agentIsDisabled(a)) {
        session = 'disabled'; sessionCls = 'ab-bad';
        sessionTip = 'Disabled in config \u2014 the governor never starts this agent. Enable it with \u2699\uFE0F.';
      } else if (sandboxRun) {
        const rt = sandboxRun.runtime === 'job' ? 'Job' : 'sandbox';
        if (sandboxRun.health === 'running') { session = rt + ' running'; sessionCls = 'ab-ok'; sessionTip = 'A sandboxed kick is in flight.'; }
        else if (sandboxRun.health === 'failed') { session = agentSandboxDownLabel(a); sessionCls = 'ab-bad'; sessionTip = 'The last sandboxed run failed: ' + (sandboxRun.error || 'see the full log.'); }
        else if (sandboxRun.health === 'overdue') { session = 'run overdue'; sessionCls = 'ab-bad'; sessionTip = 'A scheduled kick was due' + (sandboxRun.dueAt ? ' at ' + sandboxRun.dueAt : '') + ' and no run has started.'; }
        else { session = sandboxRun.lastRunAt ? rt + ' ok' : rt + ' ready'; sessionCls = 'ab-ok'; sessionTip = 'Sandboxed agent: each kick runs in its own container, so there is no session between kicks. ' + (agentSandboxRunText(a) || 'No run yet.'); }
      } else if (a.state !== 'running') {
        session = 'down'; sessionCls = 'ab-bad';
        sessionTip = 'No live tmux session \u2014 \u21bb restart asks the supervisor to respawn it.';
      }
      const blockers = agentSchedulingBlockers(a);
      let sched = agentCadenceDisplay(a), schedCls = 'ab-ok';
      let schedTip = 'Governor cadence for the current mode.';
      if (onDemand) {
        sched = 'on demand'; schedCls = '';
        schedTip = 'On-demand agent \u2014 kicked manually, never by governor cadence.';
      } else if (blockers.length > 0) {
        sched = blockers.join(' + '); schedCls = 'ab-bad';
        schedTip = `EVERY reason the governor will not kick ${a.displayName || a.name} right now \u2014 all of them must be cleared: ${blockers.join('; ')}.`;
      }
      let next = a.nextKickIn ? `in ${a.nextKickIn}` : agentKickDisplay(a.nextKick, a.nextKickAt);
      let nextCls = '';
      if (onDemand) next = 'on demand';
      else if (blockers.length > 0) { next = 'never'; nextCls = 'ab-bad'; }
      const nextTip = (!onDemand && blockers.length === 0 && a.nextKick)
        ? `Next governor kick at ${agentKickDisplay(a.nextKick, a.nextKickAt)}.`
        : 'Nothing is scheduled to kick this agent.';
      return `<div class="agent-blockers">`
        + `<span class="ab-seg ${sessionCls}" title="${esc(sessionTip)}"><span class="ab-key">session</span>${esc(session)}</span>`
        + `<span class="ab-sep">\u00B7</span>`
        + `<span class="ab-seg ${schedCls}" title="${esc(schedTip)}"><span class="ab-key">scheduling</span>${esc(sched)}</span>`
        + `<span class="ab-sep">\u00B7</span>`
        + `<span class="ab-seg ${nextCls}" title="${esc(nextTip)}"><span class="ab-key">next kick</span>${esc(next)}</span>`
        + `</div>`;
    }
    function renderAgents(agents) {
      agents = (typeof _sidebarAgents === 'function') ? (() => {
        const prev = window._lastAgents;
        window._lastAgents = Array.isArray(agents) ? agents : [];
        const merged = _sidebarAgents();
        window._lastAgents = prev;
        return merged;
      })() : (Array.isArray(agents) ? agents : []);
      refreshSectionCardShell('agents-section');
      setSectionSummary('agents-section', agentsSectionSummary(agents));
      renderAgentActivity(agents);
      renderAgentNavbarUpNext(agents, Date.now());
      const el = document.getElementById('agents');
      // Preserve custom prompt input values across re-renders
      const savedInputs = {};
      const focusedId = document.activeElement?.id;
      el.querySelectorAll('.kick-prompt').forEach(inp => {
        if (inp.value) savedInputs[inp.id] = inp.value;
      });
      const { sorted: sortedAgents } = _sortAgentsBySidebar(agents);
      if (sortedAgents.length === 0) {
        renderAgentNavbarUpNext([], Date.now());
        setSectionSummary('agents-section', agentsSectionSummary([]));
        setIfChanged(el, emptyStateCard('🤖', 'Add your first agent',
          'Agents are the workers that scan repos, review changes, keep CI healthy, and write docs. Start from a quick template or build one from scratch. ' +
          '<br><button type="button" class="hv-btn btn-primary btn-sm" data-action="openAddAgentDialog" title="Add a new agent from a template or from scratch">＋ Add your first agent</button>'));
        return;
      }
      const orderedAgents = agentLayoutApplyOrder(sortedAgents);
      agentCardLayoutUpdateResetButton(sortedAgents);
      const cards = agentsFocusedFirst(orderedAgents).map(a => {
        const isDisabled = agentIsDisabled(a);
        const isPoweredOff = isDisabled && a.state !== 'running';
        const isOnDemand = a.onDemand === true;
        // Manual/persisted pause wins over offByCadence — see the agent-detail
        // renderer for the incident this ordering prevents.
        const isPaused = a.paused === true && !isOnDemand;
        const isOff = agentHeldByMode(a) && !isPaused && !isOnDemand;
        // Down = process not alive — anything but 'running'. 'stopped'-only
        // matching rendered crash-looping ('failed') agents green (#2465).
        const isStarting = !!a.starting && a.state !== 'running' && !isPaused && !isOff;
        const isStopped = agentSessionDown(a) && !isPaused && !isOff && !isOnDemand && !isStarting;
        const needsLogin = _agentNeedsLogin(a, isStopped);
        const STALE_MS = 1200000; // 20 minutes
        const isStale = a.summaryUpdated && !isPaused && a.busy === 'working' && (Date.now() - new Date(a.summaryUpdated).getTime()) > STALE_MS;
        let statusCls = '';
        if (a.structuredStatus === 'BLOCKED') statusCls = 'status-blocked';
        else if (a.structuredStatus === 'NEEDS_CONTEXT') statusCls = 'status-needs-context';
        else if (isStale) statusCls = 'status-stale';
        const isOnDemandIdle = isOnDemand && a.busy !== 'working';
        const cls = isDisabled ? 'disabled-agent' : needsLogin ? 'needs-login' : isStarting ? 'starting' : isOnDemandIdle ? 'on-demand' : isPaused ? 'paused' : isOff ? 'off' : isStopped ? 'stopped' : `${a.busy} ${statusCls}`.trim();
        // Down + unauthenticated CLI = attention-token needs-login (same formula as the
        // card's Login button above), not plain down-red (#2465).
        const dotCls = isPoweredOff ? 'off' : (isStopped && needsLogin) ? 'needs-login' : isStarting ? 'starting' : isStopped ? 'stopped' : isOnDemandIdle ? 'on-demand' : isPaused ? 'paused' : isOff ? 'off' : a.busy === 'working' ? 'running' : 'idle';
        // Healthy-but-governor-paused → hollow green dot (see detail-header renderer).
        const dotState = isOff ? 'off-healthy' : dotCls;
        const canToggle = a.beadRole !== 'supervisor';
        const canPauseToggle = canToggle && dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
        /* RESUME LEADS (#5594). A paused agent's card offers its pause toggle
           FIRST and unconditionally; the governor-cadence gear must never take
           the slot, or the pause stays hidden until the gear's blocker is
           cleared and a second button appears. isOff already excludes isPaused
           today, so this renders identically — stating the order here is what
           keeps it true if that derivation ever changes. The remaining
           blockers ride in the tooltip and on the blockers line below, so the
           operator learns them before the click, not after. */
        /* A DISABLED agent gets no scheduling button at all. canToggle only ever
           excluded the supervisor, so a disabled agent still rendered a green
           '⏸ pause' — offering to apply a temporary operator hold to an agent
           the governor already refuses to schedule. The click succeeded and
           changed nothing observable. The 0/1 switch beside this slot is the
           only lever that means anything here, so it is the only one shown. */
        const toggleBtn = (canToggle && !agentIsDisabled(a)) ? (isPaused && canPauseToggle
          ? pausedAgentActionHtml(a)
          : isOff
          ? cadenceOffAgentActionHtml(a)
          : isOnDemand
          ? `<button class="hv-btn btn-secondary btn-sm btn-toggle on-demand" title="On-demand agent — kicked manually, not by governor cadence">⚡ on demand</button>`
          : canPauseToggle
          ? `<button class="hv-btn btn-secondary btn-sm btn-toggle running" data-action="toggleAgent" data-agent="${esc(a.name)}" data-paused="false" title="${esc(agentToggleTitle(a, false, false))}">⏸ pause</button>`
          : '') : '';
        // liveSummary is pushed via SSE every 5s — no separate fetch needed
        let summaryEl = '';
        if (a.liveSummary) {
          let ageBadge = '';
          if (!a.doing && a.summaryUpdated) {
            const ageMs = Date.now() - new Date(a.summaryUpdated).getTime();
            const ageMin = Math.floor(ageMs / 60000);
            if (ageMin >= 5) {
              const ageStr = ageMin >= 60 ? `${Math.floor(ageMin/60)}h ${ageMin%60}m ago` : `${ageMin}m ago`;
              const ageCls = ageMin >= 120 ? 'age-stale' : 'age-recent';
              ageBadge = `<span class="summary-age ${ageCls}">${ageStr}</span>`;
            }
          }
          summaryEl = `<div class="doing">${ageBadge}${escPreserveNewlines(a.liveSummary)}</div>`;
        }
        const indEl = agentIndicators(a.name);
        const compactCls = isAgentCompact(a.name) ? ' compact' : '';
        const autoscale = (window._lastStatus || {}).reviewerAutoscale;
        const autoLane = autoscale && autoscale.enabled && autoscale.agent === (a.replicaBase || a.name);
        const autoTitle = autoLane ? ` — reviewer autoscale: ${autoscale.current} active, target ${autoscale.target}, band ${autoscale.min}–${autoscale.max}${autoscale.lastDecision ? '; last ' + reviewerAutoscaleDecisionText(autoscale.lastDecision) : ''}` : '';
        const autoDraining = autoLane && Array.isArray(autoscale.draining) && autoscale.draining.includes(a.name);
        const replicaBadge = (a.replicaBase && a.replicaIndex > 1)
          ? ` <span class="status-badge" title="Replica ${a.replicaIndex} of ${a.replicaCount} for ${esc(a.replicaBase)}${esc(autoTitle)}">${esc(a.replicaBase)} · replica ${a.replicaIndex} of ${a.replicaCount}${autoLane ? ' · auto' : ''}${autoDraining ? ' · draining' : ''}</span>`
          : (autoLane ? ` <span class="status-badge" title="Reviewer lane sized by backlog${esc(autoTitle)}">${autoscale.current} reviewer${autoscale.current === 1 ? '' : 's'} · auto</span>` : '');
        const configTarget = a.replicaBase || a.name;
        /* Says why the card is grey. Without it the only visible difference
           between "switched off" and "crashed" is the absence of a session,
           which is exactly what the reader cannot see from here. */
        const disabledBadge = agentIsDisabled(a) ? ' <span class="status-badge disabled" data-status="neutral" title="Disabled in config — the governor never starts this agent. Configure it with the gear to enable.">disabled</span>' : '';
        const continuousBlocked = a.continuousBlocked === 'budget' ? ' Budget guard is holding continuous re-kicks.' : '';
        const continuousBadge = a.continuous === true ? ` <span class="status-badge" data-status="${a.continuousBlocked ? 'warn' : 'info'}" title="Continuous mode: after a kicked session ends, Hive schedules the next kick after the configured cool-down.${continuousBlocked}">${esc(agentCadenceDisplay(a))}${a.continuousBlocked ? ' held' : ''}</span>` : '';
        const layoutSize = agentCardLayoutSize(a.name, sortedAgents);
        const layoutHeightAttr = layoutSize.height ? ` data-agent-height="${Math.round(layoutSize.height)}"` : '';
        const agentOrderHandle = `<button type="button" class="dashboard-grip agent-card-order-handle" draggable="true" data-agent-order-grip data-agent="${esc(a.name)}" data-stop="1" aria-label="Drag to reorder ${esc(a.displayName || a.name)}" title="Drag to reorder" aria-pressed="false">⠿</button>`;
        const agentSpanTip = `Drag or use ← / → to set ${a.displayName || a.name} to span 1, 2, or 3 columns; Esc resets width`;
        const agentSpanHandle = `<button type="button" class="agent-card-resize-col" data-agent-resize-col data-agent="${esc(a.name)}" data-stop="1" aria-label="${esc(agentSpanTip)}" title="${esc(agentSpanTip)}" aria-valuemin="1" aria-valuemax="3" aria-valuenow="${layoutSize.span || 1}"></button>`;
        const disabledEnableBtn = isDisabled ? `<button class="hv-btn btn-primary btn-sm oc-enable-agent" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(configTarget)}" data-tab="General" title="Configure ${esc(a.displayName || a.name)} — review its settings and enable it from the General tab">⚙ Enable…</button>` : '';
        const historyLink = `<a href="${kickHistoryUrl(a.name)}" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm terminal-link" data-action="openAuthenticatedLink" title="Archived logs of previous kicks — each run is archived when its turn ends; survives restarts and upgrades">🕘 past kicks</a>`;
        const liveSessionActions = `${sessionChip(a, '▶ terminal', `<a href="${terminalUrl(a.name, a.sandboxRuntime)}" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm terminal-link" data-action="openTerminal" data-agent="${esc(a.name)}" title="${esc(terminalLinkTitle(a, 'Open tmux session'))}">▶ terminal</a>`)} ${sessionChip(a, '📄 full log', `<a href="${fullLogUrl(a.name, false)}" target="_blank" rel="noopener" class="hv-btn btn-secondary btn-sm terminal-link" data-action="openAuthenticatedLink" title="${esc(fullLogTitle(a, 'Full log of the latest run as selectable, searchable text'))}">📄 full log</a>`)} ${historyLink}`;
        const sessionActions = isPoweredOff
          ? `${historyLink}`
          : isDisabled
          ? liveSessionActions
          : `${liveSessionActions} <button class="hv-btn btn-danger btn-sm restart-btn" data-action="restartAgent" data-agent="${esc(a.name)}" title="Kill tmux session — supervisor will respawn with fresh context">↻ restart</button>`;
        return `<div class="agent-card ${cls}${compactCls}" data-agent="${esc(a.name)}" data-agent-order-key="${esc(a.name)}" data-agent-span="${layoutSize.span || 1}">
          ${agentSpanHandle}
          <span class="working-indicator"></span>
          <div class="agent-name">${agentOrderHandle}<span class="dot ${dotState}" data-status="${statusAttr(dotState)}"${isOff ? ` title="${esc(agentOffTitle(a))}"` : ''}></span>${a.emoji ? a.emoji + ' ' : ''}${esc(a.displayName || a.name)}${needsLogin ? ' <span title="CLI needs login">\uD83D\uDD11</span>' : ''}${a.sandboxed ? ' ' + statusBadgeHTML('ready', 'sandboxed', 'Sandboxed runner active', 'done') : ''}${disabledBadge}${continuousBadge}${replicaBadge} ${agentPowerSwitchHtml(a)} ${isDisabled ? disabledEnableBtn : ''}${toggleBtn}${neverScheduledChipHtml(a, configTarget)} <button class="compact-toggle" data-action="toggleAgentCompact" data-arg0="${esc(a.name)}" data-stop="1" title="Toggle compact/expanded view for this agent card">${isAgentCompact(a.name) ? '&#9655; expand' : '&#9661; compact'}</button> ${sessionActions} <button class="hv-btn btn-secondary btn-sm" data-action="showAdvisorRecords" data-arg0="${esc(a.name)}" data-stop="1" title="Advisor reviews of this agent's finished turns — severity and text, newest first">🗣 advisor</button> <button class="config-gear" data-action="openConfigDialog" data-config-type="agent" data-agent="${esc(configTarget)}" title="Configure ${esc(a.replicaBase ? (a.replicaBase + ' (base for ' + a.name + ')') : (a.displayName || a.name))}">⚙️</button></div>
          <div class="agent-state ${isPoweredOff ? 'disabled' : isOnDemand ? 'on-demand' : isPaused ? 'paused' : isOff ? 'off' : a.busy}">${agentStateText(a)}${pauseInfoIcon(a)}${(() => {
            if (a.structuredStatus === 'BLOCKED') return statusBadgeHTML('blocked', 'blocked', a.statusEvidence, 'blocked');
            if (a.structuredStatus === 'NEEDS_CONTEXT') return statusBadgeHTML('needs_context', 'needs context', a.statusEvidence, 'needs-context');
            if (a.structuredStatus === 'DONE_WITH_CONCERNS') return statusBadgeHTML('needs_attention', 'concerns', a.statusEvidence, 'done-concerns');
            if (a.structuredStatus === 'DONE') return statusBadgeHTML('ready', 'done', '', 'done');
            if (isStale) return statusBadgeHTML('degraded', 'stale', '', 'needs-context');
            return '';
          })()}${kickOutcomeBadgeHtml(a)}${agentSpendChip(a.name)}</div>
          ${agentNowHtml(a)}
          ${agentRunLineHtml(a)}
          ${agentBlockerLineHtml(a)}
          ${repeatFailureLineHtml(a)}
          ${terminalCopyHintHtml(needsLogin, agentSessionUnavailable(a), a.name, a.loginURL, a.backendAuthStatus, a.backendAuthLastError, a.statusEvidence, a.inferenceLastError)}
          ${(() => {
            // Last upstream/launch error stays on its own line below the state
            // badge, never behind the working-indicator dot — previously the
            // agent kept retrying with the spinner visible while lastError was
            // silently dropped, so operators could not see e.g. a 429 until
            // retries were exhausted (#3604). Composing "retrying: <err>" keeps
            // both facts legible at once instead of one overlapping/hiding the
            // other. No per-attempt counter is tracked on the backend, so the
            // prefix names the state rather than fabricating a retry count.
            if (!a.lastError) return '';
            const retrying = a.busy === 'working' && !isPaused && !isOff && !isStopped;
            const prefix = retrying ? 'retrying: ' : '';
            return `<div class="agent-error" title="${esc(a.lastError)}">⚠ ${esc(prefix)}${esc(a.lastError)}</div>`;
          })()}
          <div class="agent-card-details" data-agent-resize-y data-agent="${esc(a.name)}"${layoutHeightAttr}>
          <div class="agent-field"><span class="label" title="${isInferenceBackendName(a.cli) ? 'Inference backend (self-hosted model server)' : 'The CLI backend running this agent (e.g. claude, copilot). Pin to prevent governor from switching it.'}">${isInferenceBackendName(a.cli) ? 'method' : 'cli'}</span><span class="value">${cliChip(a.cli, a.pinnedBoth || a.pinnedCli, a.name)}${(a.pinnedBoth || a.pinnedCli) ? '' : ` <button class="hv-btn pin-toggle btn-icon btn-sm" data-action="togglePin" data-agent="${esc(a.name)}" data-pin-type="cli" data-unpin="false" title="Pin CLI — lock current backend">📌</button>`}</span></div>
          <div class="agent-field"><span class="label" title="The LLM model powering this agent. Governor may change it based on budget or mode. Pin to lock.">model</span><span class="value">${modelChip(a.model, a.govReason, a.pinnedBoth || a.pinnedModel, a.name)}${(a.pinnedBoth || a.pinnedModel) ? '' : ` <button class="hv-btn pin-toggle btn-icon btn-sm" data-action="togglePin" data-agent="${esc(a.name)}" data-pin-type="model" data-unpin="false" title="Pin model — lock current model">📌</button>`}</span></div>
          ${agentCurrentRunRow(a)}
          <div class="agent-field"><span class="label" title="Agent operating mode — controls what GitHub actions this agent can take (advisory, issues, PRs, merge).">mode</span><span class="value">${modeLabel(a)}${a.needsRestart ? ' ' + statusBadgeHTML('needs_context', '⚠ restart needed', 'CLI flags are stale — restart agent to apply new mode', 'needs-context') : ''}</span></div>
          <div class="agent-field"><span class="label" title="Number of HTTP requests blocked by the ACMM proxy for this agent. Non-zero means the agent attempted actions above its mode.">proxy</span><span class="value">${a.proxyViolations > 0 ? '<span style="color:var(--danger)">⛔ ' + a.proxyViolations + ' blocked</span>' : '✓ clean'}</span></div>
          <div class="agent-field"><span class="label" title="Time between governor kicks for this agent in the current mode. Configured per governor mode (idle/quiet/busy/surge).">interval</span><span class="value">${isOnDemand ? 'on demand' : isPaused ? 'paused' : isOff ? esc(agentCadenceOffLabel()) : esc(agentCadenceDisplay(a))}</span></div>
          <div class="agent-field"><span class="label" title="When the governor last kicked (sent a work prompt to) this agent. A kicked work pass often keeps running long after this time — terminal activity between kicks is normally the previous pass still working, not an off-schedule run.">last kick</span><span class="value">${esc(agentKickDisplay(a.lastKick, a.lastKickAt))}</span></div>
          <div class="agent-field"><span class="label" title="Which house style the last kick carried: repo (HIVE_STYLE.md), hive (project.style) or built-in.">style</span><span class="value">${styleSourceHtml(a)}</span></div>
          <div class="agent-field"><span class="label" title="When the governor will next kick this agent based on its cadence interval. If the previous pass is still running at that time, the kick waits for the CLI's input dialog.">next kick</span><span class="value">${isOnDemand ? 'on demand' : isPaused ? 'paused' : isOff ? esc(agentCadenceOffLabel()) : esc(agentKickDisplay(a.nextKick, a.nextKickAt))}</span></div>
          ${agentTokenRow(a.name, a.cadence)}
          ${agentSpendRow(a.name)}
          <div class="agent-field"><span class="label" title="Number of times this agent has been restarted in the last 24 hours. High counts may indicate instability or rate limiting.">restarts</span><span class="value${(a.restarts || 0) > 5 ? ' restart-high' : (a.restarts || 0) > 0 ? ' restart-warn' : ''}">${a.restarts || 0}<span class="restart-label">24h</span>${(a.restarts || 0) > 0 ? `<button class="restart-reset" data-action="resetRestarts" data-agent="${esc(a.name)}" title="Reset the restart counter to zero — does not affect the agent">✕</button>` : ''}<span class="restart-spark">${restartSparkSvg(a.name)}</span></span></div>
          ${watchdogConditionsRow(a)}
          <div class="agent-field"><span class="label" title="CLI authentication status — log in to authenticate the agent's CLI tool">auth</span><span class="value">${isDisabled ? '—' : (() => {
            const isClaude = a.cli === 'claude';
            const isCopilot = a.cli === 'copilot';
            const claudeIcon = '<span class="cli-icon claude-icon"></span>';
            const copilotIcon = '<span class="cli-icon copilot-icon"></span>';
            if (needsLogin) {
              if (isClaude) return '<button class="hv-btn btn-primary btn-sm cli-login-btn claude" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="claude" data-stop="1" title="Authenticate Claude Code CLI">' + claudeIcon + ' Login</button>';
              if (isCopilot) return '<button class="hv-btn btn-primary btn-sm cli-login-btn copilot" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="copilot" data-stop="1" title="Authenticate GitHub Copilot CLI">' + copilotIcon + ' Login</button>';
              return '<button class="hv-btn btn-secondary btn-sm cli-login-btn disabled" disabled title="This backend requires manual /login in the terminal">Login</button>';
            }
            if (isClaude) return '<button class="hv-btn btn-secondary btn-sm cli-login-btn logged-in" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="claude" data-stop="1" title="Re-authenticate Claude Code CLI">' + claudeIcon + ' ✓ logged in</button>';
            if (isCopilot) return '<button class="hv-btn btn-secondary btn-sm cli-login-btn logged-in" data-action="loginAgent" data-arg0="' + esc(a.name) + '" data-arg1="copilot" data-stop="1" title="Re-authenticate GitHub Copilot CLI">' + copilotIcon + ' ✓ logged in</button>';
            return '<button class="hv-btn btn-secondary btn-sm cli-login-btn disabled" disabled title="This backend requires manual /login in the terminal">✓ logged in</button>';
          })()}</span></div>
          ${summaryEl}${indEl}
          ${isDisabled ? '' : `<div class="agent-actions">
            <input type="text" class="kick-prompt" id="kick-prompt-${esc(a.name)}" placeholder="custom instruction (optional)" data-enter-action="kick" data-agent="${esc(a.name)}" title="Type a custom prompt to send to this agent instead of its default kick dialog. Press Enter to send." />
            <button class="hv-btn btn-secondary btn" data-action="kick" data-agent="${esc(a.name)}" title="Send the custom prompt above to this agent immediately, bypassing the governor cadence timer">send</button>
            <button class="hv-btn btn-secondary btn" data-action="kickEmpty" data-agent="${esc(a.name)}" title="Kick this agent now with its default prompt, bypassing the governor cadence timer">kick</button>
            <select class="hv-btn btn-secondary btn backend-select" data-change-action="switchCli" data-agent="${esc(a.name)}" title="Switch this agent to a different method (CLI or inference backend). Free backends (copilot, goose) do not count toward token budget.">
              <option value="" disabled selected>${a.cli ? esc(backendLabel(a.cli)) + ' ▾' : 'method ▾'}</option>
              ${backendOptionList().map(b => `<option value="${b}" ${a.cli === b ? 'disabled' : ''}>${backendLabel(b)}</option>`).join('')}
            </select>
            <select class="hv-btn btn-secondary btn backend-select" data-change-action="switchModel" data-agent="${esc(a.name)}" title="Switch this agent to a different LLM model. Higher-tier models (Opus) cost more tokens; lower-tier (Haiku) are cheaper but less capable.">
              <option value="" disabled${a.model ? '' : ' selected'}>model ▾</option>
              ${modelOptionsHtml(a.cli, a.model)}
            </select>
            ${effortSelectHtml(a)}
          </div>`}
          </div>
        </div>`;
      });
      const html = cards.join('');
      if (el.innerHTML !== html) {
        // Patch the existing cards in place instead of replacing the grid's
        // innerHTML. A wholesale replacement destroyed every card node each
        // refresh, so (a) the browser's scroll anchor (Chrome) was lost with
        // it and (b) the terminal tail panes (.doing) of cards scrolled above
        // the viewport grew or shrank with new log lines and pushed the rows
        // the reader was looking at — the whole page appeared to jump on
        // every SSE tick, worst in Safari which has no scroll anchoring.
        const tailFollow = agentTailFollowState(el);
        morphAgentGrid(el, html);
        for (const [id, val] of Object.entries(savedInputs)) {
          const inp = document.getElementById(id);
          if (inp && inp.value !== val) inp.value = val;
        }
        if (focusedId && document.activeElement?.id !== focusedId) {
          const prev = document.getElementById(focusedId);
          if (prev) { try { prev.focus({ preventScroll: true }); } catch (e) { prev.focus(); } }
        }
        // Element-local scrollTop only, so the window never moves. Keep
        // following the tail only when the reader was already at the bottom
        // (or the pane is new); otherwise hold their place in the log.
        el.querySelectorAll('.doing').forEach(d => {
          const card = d.closest('.agent-card');
          const key = card ? card.getAttribute('data-agent') : '';
          if (!(key in tailFollow) || tailFollow[key]) d.scrollTop = d.scrollHeight;
        });
        el.querySelectorAll('[data-agent-resize-y][data-agent-height]').forEach(d => { d.style.height = `${d.dataset.agentHeight}px`; });
      }
    }

    // Per-agent "was the terminal tail scrolled to the bottom?" snapshot taken
    // before a re-render, so the post-render auto-follow respects a reader who
    // scrolled up inside a pane.
    const AGENT_TAIL_BOTTOM_SLACK_PX = 4;
    function agentTailFollowState(grid) {
      const out = {};
      grid.querySelectorAll('.agent-card').forEach(card => {
        const d = card.querySelector('.doing');
        if (!d) return;
        out[card.getAttribute('data-agent')] = d.scrollTop + d.clientHeight >= d.scrollHeight - AGENT_TAIL_BOTTOM_SLACK_PX;
      });
      return out;
    }

    // Keyed reconcile of the agents grid: cards are matched by data-agent,
    // kept in the DOM, moved into the new order, and patched node-by-node so
    // scroll positions (window, .agent-card-details, .doing), focus, typed
    // input and inline sizes survive a refresh. Cards no longer present are
    // removed; new ones are inserted at their position.
    function morphAgentGrid(grid, html) {
      const tpl = document.createElement('template');
      tpl.innerHTML = html;
      const existing = new Map();
      Array.from(grid.children).forEach(c => {
        const k = c.getAttribute && c.getAttribute('data-agent');
        if (k && c.classList.contains('agent-card') && !existing.has(k)) existing.set(k, c);
        else c.remove();
      });
      let cursor = grid.firstChild;
      Array.from(tpl.content.children).forEach(nextCard => {
        const k = nextCard.getAttribute('data-agent');
        let cur = k ? existing.get(k) : null;
        if (cur) {
          existing.delete(k);
          morphDomNode(cur, nextCard);
        } else {
          cur = nextCard;
        }
        if (cur !== cursor) grid.insertBefore(cur, cursor);
        else cursor = cursor.nextSibling;
      });
      existing.forEach(c => c.remove());
    }

    function morphDomNode(from, to) {
      if (from.nodeType !== to.nodeType || (from.nodeType === 1 && from.tagName !== to.tagName)) {
        from.replaceWith(to);
        return;
      }
      if (from.nodeType !== 1) {
        if (from.nodeValue !== to.nodeValue) from.nodeValue = to.nodeValue;
        return;
      }
      // Form controls carry live state (value, selectedness, open dropdown)
      // that attribute patching can't reconcile — swap them only when their
      // markup actually changed.
      if (from.tagName === 'SELECT' || from.tagName === 'INPUT' || from.tagName === 'TEXTAREA') {
        if (from.outerHTML !== to.outerHTML) from.replaceWith(to);
        return;
      }
      morphDomAttributes(from, to);
      const fromKids = Array.from(from.childNodes);
      const toKids = Array.from(to.childNodes);
      const n = Math.max(fromKids.length, toKids.length);
      for (let i = 0; i < n; i++) {
        const f = fromKids[i], t = toKids[i];
        if (!t) { f.remove(); continue; }
        if (!f) { from.appendChild(t); continue; }
        morphDomNode(f, t);
      }
    }

    // Patch a container's children in place from an HTML string (#12512).
    // `el.innerHTML = html` throws every node away, so a periodic re-render
    // loses scroll offsets, focus and typed input inside the container, and —
    // when the new markup is inserted expanded and only collapsed afterwards —
    // lets a forced layout in between measure the expanded height. Scroll
    // anchoring then shoves the page by that delta and rides the 200ms
    // collapse transition back: the "jumps every few seconds" seen below the
    // Cost panel. Patching keeps the nodes and their classes, so an unchanged
    // render is a no-op for layout.
    function morphInnerHtml(el, html) {
      if (!el) return;
      const next = el.cloneNode(false);
      next.innerHTML = html;
      morphDomNode(el, next);
    }

    function morphDomAttributes(from, to) {
      const toAttrs = to.attributes;
      for (let i = 0; i < toAttrs.length; i++) {
        const a = toAttrs[i];
        if (from.getAttribute(a.name) !== a.value) from.setAttribute(a.name, a.value);
      }
      Array.from(from.attributes).forEach(a => {
        if (to.hasAttribute(a.name)) return;
        // Inline style on a kept node was set from JS (resize-handle height,
        // data-agent-height) — the card template never emits one, so keep it.
        if (a.name === 'style') return;
        from.removeAttribute(a.name);
      });
    }

    function agentGridColumnCount(grid) {
      if (!grid || typeof getComputedStyle !== 'function') return 3;
      const cols = (getComputedStyle(grid).gridTemplateColumns || '').split(' ').filter(Boolean).length;
      return Math.max(1, Math.min(3, cols || 3));
    }

    function agentOrderSaveFromDom(grid) {
      const agents = ((window._lastStatus || {}).agents) || window._lastAgents || [];
      const layout = agentCardLayoutNormalize(agents);
      layout.order = Array.from(grid.querySelectorAll('.agent-card[data-agent-order-key]')).map(card => card.getAttribute('data-agent-order-key')).filter(Boolean);
      agentCardLayoutWrite(layout, agentLayoutDefaultOrder(agents));
      agentCardLayoutUpdateResetButton(agents);
      ocUpdateSidebarAgents();
    }

    function agentOrderNextCard(card) {
      let next = card ? card.nextElementSibling : null;
      while (next && !next.classList.contains('agent-card')) next = next.nextElementSibling;
      return next;
    }

    function agentOrderDropSlot(grid, x, y, skip) {
      const cards = Array.from(grid.querySelectorAll('.agent-card[data-agent-order-key]')).filter(card => card !== skip);
      let best = null, bestDist = Infinity;
      cards.forEach(card => {
        const rect = card.getBoundingClientRect();
        const horizontal = Math.abs(y - (rect.top + rect.height / 2)) < rect.height;
        const before = horizontal ? x < (rect.left + rect.width / 2) : y < (rect.top + rect.height / 2);
        const anchorX = before ? rect.left : rect.right;
        const anchorY = rect.top + rect.height / 2;
        const dist = Math.abs(x - anchorX) + Math.abs(y - anchorY);
        if (dist < bestDist) { bestDist = dist; best = { before: before ? card : agentOrderNextCard(card) }; }
      });
      return best || { before: null };
    }

    function initAgentCardLayoutControls() {
      const grid = document.getElementById('agents');
      if (!grid || grid.dataset.agentLayoutControls === '1') return;
      grid.dataset.agentLayoutControls = '1';
      const orderHandleOf = (e) => (e.target && e.target.closest) ? e.target.closest('[data-agent-order-grip]') : null;
      const spanHandleOf = (e) => (e.target && e.target.closest) ? e.target.closest('[data-agent-resize-col]') : null;
      function cleanupOrder() {
        if (!_agentOrderDrag) return;
        _agentOrderDrag.card.classList.remove('dashboard-drag-source');
        _agentOrderDrag.handle.classList.remove('dragging');
        _agentOrderDrag.handle.setAttribute('aria-pressed', 'false');
        grid.querySelectorAll(':scope > .dashboard-drop-indicator').forEach(el => el.remove());
        _agentOrderDrag = null;
      }
      grid.addEventListener('dragstart', (e) => {
        const handle = orderHandleOf(e);
        if (!handle) return;
        const card = handle.closest('.agent-card');
        if (!card) return;
        const indicator = document.createElement('div');
        indicator.className = 'dashboard-drop-indicator';
        _agentOrderDrag = { handle, card, indicator };
        card.classList.add('dashboard-drag-source');
        handle.classList.add('dragging');
        handle.setAttribute('aria-pressed', 'true');
        e.dataTransfer.effectAllowed = 'move';
        e.dataTransfer.setData('text/plain', card.getAttribute('data-agent-order-key') || '');
      });
      grid.addEventListener('dragover', (e) => {
        if (!_agentOrderDrag) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';
        const slot = agentOrderDropSlot(grid, e.clientX, e.clientY, _agentOrderDrag.card);
        _agentOrderDrag.slot = slot;
        grid.insertBefore(_agentOrderDrag.indicator, slot.before || null);
      });
      grid.addEventListener('drop', (e) => {
        if (!_agentOrderDrag) return;
        e.preventDefault();
        const drag = _agentOrderDrag;
        const slot = drag.slot || agentOrderDropSlot(grid, e.clientX, e.clientY, drag.card);
        grid.querySelectorAll(':scope > .dashboard-drop-indicator').forEach(el => el.remove());
        grid.insertBefore(drag.card, slot.before || null);
        agentOrderSaveFromDom(grid);
        showToast('Agent card order saved', 'info');
        cleanupOrder();
      });
      grid.addEventListener('dragend', cleanupOrder);
      grid.addEventListener('keydown', (e) => {
        const orderHandle = orderHandleOf(e);
        if (orderHandle) {
          const card = orderHandle.closest('.agent-card');
          if (!card) return;
          if (e.key === ' ' || e.key === 'Enter') {
            e.preventDefault();
            orderHandle.setAttribute('aria-pressed', orderHandle.getAttribute('aria-pressed') === 'true' ? 'false' : 'true');
            return;
          }
          if (e.key !== 'ArrowLeft' && e.key !== 'ArrowUp' && e.key !== 'ArrowRight' && e.key !== 'ArrowDown') return;
          e.preventDefault();
          if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
            const prev = card.previousElementSibling && card.previousElementSibling.classList.contains('agent-card') ? card.previousElementSibling : null;
            if (prev) grid.insertBefore(card, prev);
          } else {
            const next = card.nextElementSibling && card.nextElementSibling.classList.contains('agent-card') ? card.nextElementSibling : null;
            if (next) grid.insertBefore(card, next.nextElementSibling);
          }
          agentOrderSaveFromDom(grid);
          orderHandle.focus();
          return;
        }
        const spanHandle = spanHandleOf(e);
        if (!spanHandle) return;
        const card = spanHandle.closest('.agent-card');
        if (!card) return;
        let span = clampAgentCardSpan(card.getAttribute('data-agent-span') || 1);
        if (e.key === 'ArrowRight') span += 1;
        else if (e.key === 'ArrowLeft') span -= 1;
        else if (e.key === 'Home') span = 1;
        else if (e.key === 'End') span = agentGridColumnCount(grid);
        else if (e.key === 'Escape') span = 1;
        else return;
        e.preventDefault();
        span = Math.min(agentGridColumnCount(grid), clampAgentCardSpan(span));
        card.setAttribute('data-agent-span', String(span));
        spanHandle.setAttribute('aria-valuenow', String(span));
        agentCardLayoutSetSize(spanHandle.dataset.agent || card.dataset.agent || '', { span });
      });
      grid.addEventListener('pointerdown', (e) => {
        const handle = spanHandleOf(e);
        if (!handle || e.button > 0) return;
        const card = handle.closest('.agent-card');
        if (!card) return;
        _agentSpanDrag = { handle, card, agent: handle.dataset.agent || '', startX: e.clientX, startSpan: clampAgentCardSpan(card.getAttribute('data-agent-span') || 1), cols: agentGridColumnCount(grid) };
        try { handle.setPointerCapture(e.pointerId); } catch {}
        handle.classList.add('dragging');
        e.preventDefault();
      });
      grid.addEventListener('pointermove', (e) => {
        if (!_agentSpanDrag) return;
        const colW = Math.max(1, grid.getBoundingClientRect().width / Math.max(1, _agentSpanDrag.cols));
        const span = Math.min(_agentSpanDrag.cols, clampAgentCardSpan(_agentSpanDrag.startSpan + Math.round((e.clientX - _agentSpanDrag.startX) / colW)));
        _agentSpanDrag.span = span;
        _agentSpanDrag.card.setAttribute('data-agent-span', String(span));
        _agentSpanDrag.handle.setAttribute('aria-valuenow', String(span));
      });
      function endSpanDrag() {
        const drag = _agentSpanDrag;
        if (!drag) return;
        _agentSpanDrag = null;
        drag.handle.classList.remove('dragging');
        if (drag.span == null) return;
        agentCardLayoutSetSize(drag.agent, { span: drag.span });
      }
      grid.addEventListener('pointerup', endSpanDrag);
      grid.addEventListener('pointercancel', endSpanDrag);
      grid.addEventListener('dblclick', (e) => {
        const handle = spanHandleOf(e);
        if (!handle) return;
        e.preventDefault();
        const card = handle.closest('.agent-card');
        if (card) card.setAttribute('data-agent-span', '1');
        handle.setAttribute('aria-valuenow', '1');
        agentCardLayoutSetSize(handle.dataset.agent || '', { span: 1 });
      });
      grid.addEventListener('mouseup', (e) => {
        const details = e.target && e.target.closest ? e.target.closest('[data-agent-resize-y]') : null;
        if (!details) return;
        agentCardLayoutSetSize(details.dataset.agent || '', { height: details.getBoundingClientRect().height });
      });
    }
    initAgentCardLayoutControls();

    /* Classified GitHub App auth states, matching github.AppAuthState.String()
       on the spoke. Only the two OPERATOR_* states describe a failure the hive
       owner cannot fix: the App private key is distributed by the hub, so a
       missing or mismatched key is never the user's doing. Warnings for those
       states must not tell the user to install anything or edit config. */
    var GH_APP_STATE_KEY_MISSING = 'key-missing';
    var GH_APP_STATE_KEY_INVALID = 'key-invalid';
    /* User-actionable, but distinct from a permission problem: the App is not
       installed at all, so there are no settings to view — only an install to
       perform. Kept out of GH_APP_OPERATOR_STATES: installing is the user's
       job, not the operator's. */
    var GH_APP_STATE_NOT_INSTALLED = 'not-installed';
    /* User-actionable like not-installed: an installation ID is present but
       names an installation this App does not have (typically another org's).
       The fix is the same install/re-check flow, so the banner treats it as
       an install trigger rather than a permission problem. */
    var GH_APP_STATE_WRONG_INSTALLATION = 'wrong-installation';
    /* Operator-side: this hive still carries the placeholder app_id and was
       never assigned a real App. It is NOT 'not-installed' — the App may well be
       installed and the installation ID correct (it usually is), but an app_id
       that names no App can never authenticate. Treating it as user-actionable
       sent owners to the install link and the Set-ID box, i.e. to fix the one
       field that was already right. */
    var GH_APP_STATE_NO_APP_ASSIGNED = 'no-app-assigned';
    var GH_APP_OPERATOR_STATES = {};
    GH_APP_OPERATOR_STATES[GH_APP_STATE_KEY_MISSING] = true;
    GH_APP_OPERATOR_STATES[GH_APP_STATE_KEY_INVALID] = true;
    GH_APP_OPERATOR_STATES[GH_APP_STATE_NO_APP_ASSIGNED] = true;

    /* True when the App failure is the operator's to fix. An unreported or
       unrecognised state is deliberately NOT operator-side: a spoke too old to
       classify must keep the existing user-facing guidance rather than silently
       telling everyone to contact an admin. */

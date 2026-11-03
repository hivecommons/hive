    // ── Console-style dashboard feedback ───────────────────────────────────
    const FEEDBACK_DRAFT_KEY = 'hive-feedback-draft-v1';
    const FEEDBACK_IDENTITY_KEY = 'hive-feedback-github-username-v1';
    const FEEDBACK_MAX_SCREENSHOTS = 5;
    const FEEDBACK_MAX_SCREENSHOT_BYTES = 2 * 1024 * 1024;
    const FEEDBACK_POLL_MS = 5 * 60 * 1000;
    const FEEDBACK_DIAGNOSTIC_FIELDS = [
      { key: 'version', label: 'Version', why: 'so we can reproduce on the same dashboard release build' },
      { key: 'commit', label: 'Commit', why: 'so we can inspect the exact code revision' },
      { key: 'channel', label: 'Release channel', why: 'so we know which update stream you are running' },
      { key: 'acmm_level', label: 'ACMM level', why: 'so we can match the dashboard policy mode' },
      { key: 'hive_id', label: 'Hive ID', why: 'so hub operators can correlate this report with this hive only' },
      { key: 'hosted', label: 'Hosted flag', why: 'so we know whether this is a hosted or self-managed dashboard' },
      { key: 'hub_linked', label: 'Hub-linked flag', why: 'so we can follow the same relay path' },
      { key: 'agent_count', label: 'Agent count', why: 'so we can spot empty or unexpectedly large agent sets' },
      { key: 'agents', label: 'Agent names, backend, model, and state', why: 'so we can reproduce the affected agent configuration' },
      { key: 'include_project_repos', label: 'Whether project org/repos were included', why: 'so the report shows if the optional project context was shared' },
      { key: 'browser_user_agent', label: 'Browser user-agent', why: 'so we can reproduce browser-specific rendering bugs' },
      { key: 'browser_platform', label: 'Browser platform', why: 'so we know the operating system/browser platform family' },
      { key: 'browser_language', label: 'Browser language', why: 'so we can reproduce locale-sensitive formatting' },
      { key: 'screen_size', label: 'Screen size', why: 'so we can reproduce layout issues at the same display size' },
      { key: 'window_size', label: 'Window size', why: 'so we can reproduce the dashboard viewport' },
      { key: 'page', label: 'Dashboard section/path', why: 'so we know where you opened the form' },
      { key: 'agents[].repo', label: 'Project repo for each agent', why: 'so we can identify the affected project when you opt in', optionalProject: true },
      { key: 'agents[].org', label: 'Project org for each agent', why: 'so we can identify the affected organization when you opt in', optionalProject: true },
      { key: 'console_errors', label: 'Recent browser console errors', why: 'so we can see client-side failures that happened before submit', outsideDiagnostics: true },
      { key: 'failed_api_calls', label: 'Recent failed /api calls', why: 'so we can see backend requests that failed before submit', outsideDiagnostics: true }
    ];
    const feedbackConsoleErrors = [];
    const feedbackFailedApiCalls = [];
    function feedbackPushRing(arr, item) { arr.push(item); while (arr.length > 20) arr.shift(); }
    function feedbackUnreadCount(items) { return (items || []).filter(function(item) { return item && item.updated_at && item.last_seen_updated_at && item.updated_at > item.last_seen_updated_at; }).length; }
    function installFeedbackCapture() {
      const origError = console.error ? console.error.bind(console) : function(){};
      console.error = function() { feedbackPushRing(feedbackConsoleErrors, { timestamp: new Date().toISOString(), level: 'error', message: Array.from(arguments).map(String).join(' ') }); return origError.apply(console, arguments); };
      window.addEventListener('error', function(e) { feedbackPushRing(feedbackConsoleErrors, { timestamp: new Date().toISOString(), level: 'error', message: String(e.message || 'error'), source: feedbackStripQuery(e.filename || '') }); });
      window.addEventListener('unhandledrejection', function(e) { feedbackPushRing(feedbackConsoleErrors, { timestamp: new Date().toISOString(), level: 'unhandledrejection', message: String((e.reason && (e.reason.message || e.reason)) || 'rejected') }); });
      const prevFetch = window.fetch;
      window.fetch = async function(url, opts) {
        try {
          const res = await prevFetch(url, opts);
          const path = feedbackApiPath(url);
          if (path && res && res.status >= 400) feedbackPushRing(feedbackFailedApiCalls, { timestamp: new Date().toISOString(), status: String(res.status), path: path });
          return res;
        } catch (e) {
          const path = feedbackApiPath(url);
          if (path) feedbackPushRing(feedbackFailedApiCalls, { timestamp: new Date().toISOString(), status: 'network', path: path });
          throw e;
        }
      };
    }
    function feedbackApiPath(url) { try { const u = new URL(url, location.origin); return u.origin === location.origin && u.pathname.indexOf('/api/') === 0 ? u.pathname : ''; } catch { return ''; } }
    function feedbackStripQuery(s) { return String(s || '').split(/[?#]/)[0]; }
    function feedbackRedact(s) {
      return String(s || '')
        .replace(/(ghp_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+/gi, '[REDACTED_TOKEN]')
        .replace(/bearer\s+[A-Za-z0-9._-]+/gi, 'Bearer [REDACTED]')
        .replace(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g, '[REDACTED_EMAIL]')
        .replace(/(secret|token|password|key)\s*[:=]\s*[^\s,;]+/gi, '$1=[REDACTED]');
    }
    function feedbackDraft() { try { return JSON.parse(localStorage.getItem(FEEDBACK_DRAFT_KEY) || '{}') || {}; } catch { return {}; } }
    function feedbackGitHubLogin(login) {
      login = String(login || '').trim().replace(/^@+/, '');
      return /^[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38}(?:\[bot\])?$/.test(login) ? login : '';
    }
    function feedbackStoredGitHubUsername() { try { return feedbackGitHubLogin(localStorage.getItem(FEEDBACK_IDENTITY_KEY) || ''); } catch { return ''; } }
    function saveFeedbackGitHubUsername() { const v = feedbackGitHubLogin(feedbackVal('feedback-github-username')); try { if (v) localStorage.setItem(FEEDBACK_IDENTITY_KEY, v); else localStorage.removeItem(FEEDBACK_IDENTITY_KEY); } catch {} }
    function saveFeedbackDraft() {
      const overlay = document.getElementById('feedback-overlay'); if (!overlay) return;
      const d = { request_type: feedbackVal('feedback-type'), target_repo: feedbackVal('feedback-target'), title: feedbackVal('feedback-title'), description: feedbackVal('feedback-description'), include_diagnostics: document.getElementById('feedback-include-diagnostics')?.checked !== false, include_project_repos: document.getElementById('feedback-include-projects')?.checked === true };
      try { localStorage.setItem(FEEDBACK_DRAFT_KEY, JSON.stringify(d)); } catch {}
    }
    function feedbackVal(id) { const el = document.getElementById(id); return el ? el.value : ''; }
    function feedbackMention(login) { login = feedbackGitHubLogin(login); return login ? '@' + login : ''; }
    function feedbackHubLabel(status) { return status && status.hub_linked ? ('hub-linked: ' + (status.hub_name || 'hub')) : 'hub-less'; }
    async function loadFeedbackStatus() {
      try {
        const r = await fetch('/api/feedback/status');
        if (!r.ok) throw new Error('HTTP ' + r.status);
        window._feedbackStatus = await r.json();
      } catch {
        window._feedbackStatus = { credential_login: '', submitter_login: '', submitter_name: '', hive_id: '', hub_linked: false, hub_name: 'hub-less', needs_identity: true };
      }
      const input = document.getElementById('feedback-github-username');
      if (input) {
        const known = (window._feedbackStatus && window._feedbackStatus.submitter_login) || '';
        if (known) input.value = known;
        else if (!input.value) input.value = feedbackStoredGitHubUsername();
        input.placeholder = known ? known : 'your-github-login';
      }
      updateFeedbackAttributionLine();
      updateFeedbackPreview();
    }
    function updateFeedbackAttributionLine() {
      const el = document.getElementById('feedback-attribution-line'); if (!el) return;
      const status = window._feedbackStatus || {};
      const entered = feedbackGitHubLogin(feedbackVal('feedback-github-username'));
      // A verified login wins over the typed one; a typed login is only ever self-reported.
      const user = feedbackMention(status.submitter_login) || (entered ? '@' + entered + ' (self-reported, unverified)' : '') || String(status.submitter_name || '').trim() || 'an unidentified dashboard user';
      const hive = status.hive_id || ((window._lastStatus || {}).hiveId) || 'unknown';
      // Zero-scope handoff (#1927): the hive never files the issue. It stores the
      // diagnostic bundle with its GitHub App and opens a prefilled issue the
      // user submits as themselves — no OAuth scope is ever requested.
      el.textContent = 'Opens a prefilled GitHub issue for you to submit as yourself (attributed to ' + user + ') · diagnostic bundle stored by the hive\u2019s GitHub App · hive ' + hive + ' (' + feedbackHubLabel(status) + ')';
    }
    function feedbackPlaceholder(type) { return type === 'feature' ? 'What would you like Hive to do?\n\nWhy would it help your workflow?\n\nAny examples or constraints?' : 'What happened?\n\nWhat did you expect?\n\nSteps to reproduce:\n1.\n2.\n3.'; }
    function renderFeedbackDiagnosticsDisclosure() {
      const list = document.getElementById('feedback-disclosure-list'); if (!list) return;
      const includeProjects = document.getElementById('feedback-include-projects')?.checked === true;
      list.innerHTML = FEEDBACK_DIAGNOSTIC_FIELDS.map(function(f) {
        const marker = f.optionalProject ? (includeProjects ? 'Optional project field included: ' : 'Optional project field when checked: ') : '';
        return '<li><strong>' + escapeHtml(marker + f.label) + '</strong> — ' + escapeHtml(f.why) + '</li>';
      }).join('');
    }
    function feedbackDiagnosticPayloadFromValues(values) {
      const out = {};
      FEEDBACK_DIAGNOSTIC_FIELDS.forEach(function(f) {
        if (f.outsideDiagnostics || f.optionalProject) return;
        out[f.key] = values[f.key];
      });
      return out;
    }
    function openFeedbackModal(tab) {
      if (typeof closeGHUserMenu === 'function') closeGHUserMenu();
      if (document.getElementById('feedback-overlay')) { feedbackShowTab(tab || 'submit'); return; }
      const draft = feedbackDraft();
      const overlay = document.createElement('div');
      overlay.id = 'feedback-overlay';
      overlay.className = 'config-overlay';
      hiveRememberModalOpen(overlay);
      overlay.innerHTML = `<div class="config-modal feedback-modal" role="dialog" aria-modal="true" aria-labelledby="feedback-title-h">
        <header class="feedback-header">
          <div><h2 id="feedback-title-h" class="modal-h">Feedback</h2><p class="feedback-intro">Report a bug or request a feature. Submissions create GitHub issues in hivecommons/hive or hivecommons/docs.</p></div>
          <button class="config-close" data-action="closeFeedbackModal" aria-label="Close feedback modal">&times;</button>
        </header>
        <div class="feedback-body">
          <div class="feedback-tabs" role="tablist" aria-label="Feedback view">
            <button type="button" class="hv-btn btn-secondary btn-sm" id="feedback-submit-tab" role="tab" data-action="feedbackShowTab" data-arg0="submit">New report</button>
            <button type="button" class="hv-btn btn-secondary btn-sm" id="feedback-reports-tab" role="tab" data-action="feedbackShowTab" data-arg0="reports">My reports <span class="feedback-pill" id="feedback-modal-pill" hidden>0</span></button>
          </div>
          <section id="feedback-submit-panel" role="tabpanel" aria-labelledby="feedback-submit-tab">
            <div class="feedback-grid-row">
              <div class="feedback-field"><span>Type</span><div class="feedback-segmented" role="radiogroup" aria-label="Feedback type"><label><input type="radio" name="feedback-kind" value="bug"> Bug</label><label><input type="radio" name="feedback-kind" value="feature"> Feature</label></div></div>
              <label class="feedback-field"><span>Target</span><select id="feedback-target"><option value="hive">Hive</option><option value="docs">Documentation</option></select></label>
            </div>
            <label class="feedback-field"><span>Title</span><input id="feedback-title" type="text" maxlength="200"></label>
            <label class="feedback-field"><span>Description</span><textarea id="feedback-description" rows="8"></textarea></label>
            <label class="feedback-field"><span>GitHub username</span><input id="feedback-github-username" type="text" maxlength="39" pattern="@?[A-Za-z0-9](?:[A-Za-z0-9]|-(?=[A-Za-z0-9])){0,38}" autocomplete="username"><span class="feedback-option-extra">Optional. Used only to attribute you in the prefilled issue text; you submit the issue on GitHub as yourself.</span></label>
            <label class="feedback-field"><span>Screenshots</span><div class="feedback-drop" id="feedback-drop"><span class="feedback-drop-icon" aria-hidden="true">🖼️</span><strong>Paste, drag, or choose up to 5 screenshots (2 MiB each)</strong><input id="feedback-files" type="file" accept="image/*" multiple></div></label>
            <div class="feedback-thumbs" id="feedback-thumbs"></div>
            <fieldset class="feedback-diagnostics-fieldset"><legend>Diagnostics disclosure</legend><label class="feedback-check"><input id="feedback-include-diagnostics" type="checkbox" checked> <span><strong>Include diagnostics</strong><br><span class="feedback-option-extra">Collected fields are listed below before anything is sent.</span></span></label><label class="feedback-check feedback-option-extra"><input id="feedback-include-projects" type="checkbox"> <span>Also include project org/repos in diagnostics</span></label><ul class="feedback-disclosure-list" id="feedback-disclosure-list"></ul></fieldset>
            <details class="feedback-preview-shell"><summary>Preview exactly what will be sent</summary><pre id="feedback-preview" class="feedback-preview"></pre></details>
            <div id="feedback-result"></div>
          </section>
          <section id="feedback-reports-panel" role="tabpanel" aria-labelledby="feedback-reports-tab" hidden><div id="feedback-reports-list" class="feedback-preview">Loading…</div></section>
        </div>
        <footer class="feedback-footer"><div id="feedback-attribution-line" class="feedback-option-extra">Resolving feedback attribution…</div><button class="hv-btn btn-secondary" data-action="closeFeedbackModal">Cancel</button><button class="hv-btn btn-primary" id="feedback-submit" data-action="submitFeedbackReport">Submit feedback</button></footer>
      </div>`;
      document.body.appendChild(overlay);
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeFeedbackModal(); });
      document.body.classList.add('modal-open'); document.documentElement.classList.add('modal-open');
      const type = (tab === 'feature' || draft.request_type === 'feature') ? 'feature' : 'bug';
      overlay.querySelector('input[name="feedback-kind"][value="' + type + '"]').checked = true;
      document.getElementById('feedback-type')?.remove();
      const hidden = document.createElement('input'); hidden.type = 'hidden'; hidden.id = 'feedback-type'; hidden.value = type; overlay.appendChild(hidden);
      document.getElementById('feedback-target').value = draft.target_repo || 'hive';
      document.getElementById('feedback-title').value = draft.title || '';
      document.getElementById('feedback-description').value = draft.description || '';
      document.getElementById('feedback-github-username').value = feedbackStoredGitHubUsername();
      document.getElementById('feedback-description').placeholder = feedbackPlaceholder(type);
      document.getElementById('feedback-include-diagnostics').checked = draft.include_diagnostics !== false;
      document.getElementById('feedback-include-projects').checked = draft.include_project_repos === true;
      window._feedbackScreenshots = [];
      renderFeedbackDiagnosticsDisclosure();
      overlay.addEventListener('input', function(e) { if (e.target && e.target.name === 'feedback-kind') { hidden.value = e.target.value; document.getElementById('feedback-description').placeholder = feedbackPlaceholder(e.target.value); } if (e.target && e.target.id === 'feedback-github-username') { saveFeedbackGitHubUsername(); updateFeedbackAttributionLine(); } saveFeedbackDraft(); updateFeedbackPreview(); });
      overlay.addEventListener('change', function(e) { if (e.target && e.target.name === 'feedback-kind') { hidden.value = e.target.value; document.getElementById('feedback-description').placeholder = feedbackPlaceholder(e.target.value); } if (e.target && e.target.id === 'feedback-files') feedbackAddFiles(e.target.files); renderFeedbackDiagnosticsDisclosure(); saveFeedbackDraft(); updateFeedbackPreview(); });
      const desc = document.getElementById('feedback-description');
      desc.addEventListener('paste', function(e) { feedbackAddFiles(e.clipboardData && e.clipboardData.files); });
      const drop = document.getElementById('feedback-drop');
      drop.addEventListener('dragover', function(e) { e.preventDefault(); });
      drop.addEventListener('drop', function(e) { e.preventDefault(); feedbackAddFiles(e.dataTransfer && e.dataTransfer.files); });
      updateFeedbackPreview();
      loadFeedbackStatus();
      feedbackShowTab(tab === 'reports' ? 'reports' : 'submit');
    }
    async function feedbackShowTab(tab) {
      tab = tab === 'reports' ? 'reports' : 'submit';
      const submit = document.getElementById('feedback-submit-panel');
      const reports = document.getElementById('feedback-reports-panel');
      if (submit) submit.hidden = tab !== 'submit';
      if (reports) reports.hidden = tab !== 'reports';
      const submitBtn = document.getElementById('feedback-submit');
      if (submitBtn) submitBtn.hidden = tab === 'reports';
      ['feedback-submit-tab','feedback-reports-tab'].forEach(function(id) { const el = document.getElementById(id); if (!el) return; const active = (id === 'feedback-submit-tab') === (tab === 'submit'); el.classList.toggle('active', active); el.setAttribute('aria-selected', active ? 'true' : 'false'); });
      if (tab === 'reports') await loadFeedbackReports(true);
    }
    function closeFeedbackModal() { const el = document.getElementById('feedback-overlay'); if (el) el.remove(); }

    // ---- iPhone widget (Scriptable) hand-off -------------------------------
    // Opens Scriptable in context: first `scriptable:///add` imports the widget
    // script served by this spoke, then `scriptable:///run/Hive?url=<this hive>`
    // runs its setup with the hive URL pre-filled. If the custom scheme does not
    // take the page to the background within a short grace period, Scriptable is
    // not installed and the App Store link is revealed. No native dialogs.
    const IPHONE_WIDGET_SCRIPT_PATH = '/ios-widget/hive-widget.js';
    const IPHONE_WIDGET_SCRIPT_NAME = 'Hive';
    const IPHONE_WIDGET_APP_STORE_URL = 'https://apps.apple.com/app/scriptable/id1405459188';
    const IPHONE_WIDGET_DOCS_URL = 'https://github.com/hivecommons/hive/blob/v5/src/docs/ios-widget.md';
    const IPHONE_WIDGET_SCHEME_GRACE_MS = 1800;
    function iphoneWidgetIsIOS() {
      const ua = navigator.userAgent || '';
      return /iPhone|iPad|iPod/.test(ua) || (/Macintosh/.test(ua) && navigator.maxTouchPoints > 1);
    }
    function iphoneWidgetHiveURL() { return location.origin; }
    function openIPhoneWidgetModal() {
      if (typeof closeGHUserMenu === 'function') closeGHUserMenu();
      if (document.getElementById('iphone-widget-overlay')) return;
      const overlay = document.createElement('div');
      overlay.id = 'iphone-widget-overlay';
      overlay.className = 'config-overlay';
      hiveRememberModalOpen(overlay);
      const onIOS = iphoneWidgetIsIOS();
      const hiveURL = iphoneWidgetHiveURL();
      overlay.innerHTML = `<div class="config-modal iphone-widget-modal" role="dialog" aria-modal="true" aria-labelledby="iphone-widget-title-h">
        <header class="feedback-header">
          <div><h2 id="iphone-widget-title-h" class="modal-h">📱 iPhone widget</h2><p class="feedback-intro">Put <strong>${escapeHtml(hiveURL.replace(/^https?:\/\//, ''))}</strong> on your Home Screen: open issues, open PRs and your-turn counts with sparklines. Runs in the free Scriptable app.</p></div>
          <button class="config-close" data-action="closeIPhoneWidgetModal" aria-label="Close iPhone widget dialog">&times;</button>
        </header>
        <div class="feedback-body iphone-widget-body modal-scroll">
          ${onIOS ? '' : '<p class="iphone-widget-note">You are not on an iPhone. Open this hive on your phone and pick <em>iPhone widget</em> from the avatar menu there, or follow the <a href="' + IPHONE_WIDGET_DOCS_URL + '" target="_blank" rel="noopener noreferrer">manual steps ↗</a>.</p>'}
          <ol class="iphone-widget-steps">
            <li><strong>Add the widget script to Scriptable.</strong><br>
              <button type="button" class="hv-btn btn-primary btn-sm" data-action="iphoneWidgetAddScript">Add script to Scriptable</button>
              <button type="button" class="hv-btn btn-secondary btn-sm" data-action="iphoneWidgetCopyScriptURL">Copy script URL</button>
              <span class="iphone-widget-status" id="iphone-widget-add-status"></span>
              <div class="iphone-widget-fallback" id="iphone-widget-not-installed" hidden>Scriptable did not open — it is probably not installed. <a class="hv-btn btn-secondary btn-sm" href="${IPHONE_WIDGET_APP_STORE_URL}" target="_blank" rel="noopener noreferrer">Get Scriptable (free) ↗</a> then come back and tap the button again.</div>
            </li>
            <li><strong>Set it up for this hive.</strong> Owners: create a <strong>read-only widget token</strong> — it can only read this widget's numbers, never change anything — and scan the QR code with the iPhone camera (or tap <em>Open in Scriptable</em> on the phone). Scriptable stores the hive URL and token in the iOS Keychain.<br>
              <button type="button" class="hv-btn btn-primary btn-sm" data-action="iphoneWidgetMintToken">Create widget token &amp; QR</button>
              <button type="button" class="hv-btn btn-secondary btn-sm" data-action="iphoneWidgetRevokeToken">Revoke widget token</button>
              <span class="iphone-widget-status" id="iphone-widget-token-status"></span>
              <div class="iphone-widget-qr" id="iphone-widget-qr" hidden></div>
              <span class="iphone-widget-status">Not the owner? Use a GitHub token instead (fine-grained, no repository permissions — it just proves who you are):</span>
              <button type="button" class="hv-btn btn-secondary btn-sm" data-action="iphoneWidgetRunSetup">Set up this hive in Scriptable</button>
              <a class="iphone-widget-token-link" href="https://github.com/settings/personal-access-tokens/new" target="_blank" rel="noopener noreferrer">Create token ↗</a>
            </li>
            <li><strong>Add the widget.</strong> Long-press the Home Screen → <strong>+</strong> → <strong>Scriptable</strong> → medium size → Add. Then long-press it → <strong>Edit Widget</strong> → Script: <strong>${IPHONE_WIDGET_SCRIPT_NAME}</strong>.</li>
          </ol>
          <p class="iphone-widget-note">Several hives? Add one widget per hive and set its <em>Parameter</em> to that hive's URL. <a href="${IPHONE_WIDGET_DOCS_URL}" target="_blank" rel="noopener noreferrer">Full guide ↗</a></p>
        </div>
      </div>`;
      overlay.addEventListener('click', (e) => { if (e.target === overlay) closeIPhoneWidgetModal(); });
      document.body.appendChild(overlay);
      iphoneWidgetRefreshTokenStatus();
    }
    function closeIPhoneWidgetModal() { const el = document.getElementById('iphone-widget-overlay'); if (el) el.remove(); }
    // Navigate to a custom-scheme URL and report whether the page went to the
    // background (the only web-visible sign the app took the hand-off).
    function iphoneWidgetOpenScheme(url) {
      return new Promise((resolve) => {
        let settled = false;
        const done = (ok) => { if (settled) return; settled = true; document.removeEventListener('visibilitychange', onHide); resolve(ok); };
        const onHide = () => { if (document.visibilityState === 'hidden') done(true); };
        document.addEventListener('visibilitychange', onHide);
        setTimeout(() => done(document.visibilityState === 'hidden'), IPHONE_WIDGET_SCHEME_GRACE_MS);
        location.href = url;
      });
    }
    async function iphoneWidgetAddScript() {
      const status = document.getElementById('iphone-widget-add-status');
      const notInstalled = document.getElementById('iphone-widget-not-installed');
      if (notInstalled) notInstalled.hidden = true;
      if (status) status.textContent = 'Fetching script…';
      let script = '';
      try {
        const resp = await fetch(IPHONE_WIDGET_SCRIPT_PATH, { credentials: 'same-origin', cache: 'no-cache' });
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        script = await resp.text();
      } catch (err) {
        if (status) status.textContent = '';
        showToast('Could not load the widget script from this hive: ' + (err && err.message ? err.message : err), 'error');
        return;
      }
      if (status) status.textContent = 'Opening Scriptable…';
      const url = 'scriptable:///add?scriptName=' + encodeURIComponent(IPHONE_WIDGET_SCRIPT_NAME) + '&scriptContent=' + encodeURIComponent(script);
      const ok = await iphoneWidgetOpenScheme(url);
      if (status) status.textContent = ok ? 'Sent to Scriptable — tap “Add” there, then come back for step 2.' : '';
      if (!ok && notInstalled) notInstalled.hidden = false;
    }
    async function iphoneWidgetRunSetup() {
      const url = 'scriptable:///run/' + encodeURIComponent(IPHONE_WIDGET_SCRIPT_NAME) + '?url=' + encodeURIComponent(iphoneWidgetHiveURL()) + '&setup=1';
      const ok = await iphoneWidgetOpenScheme(url);
      if (!ok) {
        const notInstalled = document.getElementById('iphone-widget-not-installed');
        if (notInstalled) notInstalled.hidden = false;
        showToast('Scriptable did not open. Install it (step 1) and make sure the script is named “' + IPHONE_WIDGET_SCRIPT_NAME + '”.', 'error');
      }
    }
    // Read-only widget token (owner only): the server returns the plaintext
    // once, plus a QR of the scriptable:///run deep link carrying it. The token
    // is shown here and never stored by the browser.
    async function iphoneWidgetRefreshTokenStatus() {
      const status = document.getElementById('iphone-widget-token-status');
      if (!status) return;
      try {
        const resp = await fetch('/api/widget/token', { credentials: 'same-origin', cache: 'no-cache' });
        if (!resp.ok) { status.textContent = ''; return; }
        const d = await resp.json();
        status.textContent = d.configured ? ('A widget token exists (created ' + (d.createdAt || 'earlier') + '; last used ' + (d.lastUsedAt || 'never') + '). Creating a new one replaces it.') : 'No widget token yet.';
      } catch { status.textContent = ''; }
    }
    async function iphoneWidgetMintToken() {
      const box = document.getElementById('iphone-widget-qr');
      const status = document.getElementById('iphone-widget-token-status');
      if (status) status.textContent = 'Creating widget token…';
      let d = null;
      try {
        const resp = await fetch('/api/widget/token', { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ hiveUrl: iphoneWidgetHiveURL() }) });
        d = await resp.json().catch(() => null);
        if (!resp.ok || !d || !d.token) throw new Error((d && d.error) || ('HTTP ' + resp.status));
      } catch (err) {
        if (status) status.textContent = '';
        showToast('Could not create a widget token: ' + (err && err.message ? err.message : err), 'error');
        return;
      }
      if (status) status.textContent = 'Widget token created. It is shown only once — any earlier widget token no longer works.';
      if (!box) return;
      box.textContent = '';
      if (d.qrDataUrl) {
        const img = document.createElement('img');
        img.src = d.qrDataUrl;
        img.alt = 'QR code: open this hive in Scriptable with the widget token';
        box.appendChild(img);
      }
      const open = document.createElement('a');
      open.className = 'hv-btn btn-secondary btn-sm';
      open.href = d.setupUrl;
      open.textContent = 'Open in Scriptable';
      box.appendChild(open);
      const p = document.createElement('div');
      p.className = 'iphone-widget-status';
      p.append('Token (for manual setup): ');
      const code = document.createElement('code');
      code.textContent = d.token;
      p.appendChild(code);
      box.appendChild(p);
      box.hidden = false;
    }
    async function iphoneWidgetCopyScriptURL() {
      const url = iphoneWidgetHiveURL() + IPHONE_WIDGET_SCRIPT_PATH;
      try {
        await navigator.clipboard.writeText(url);
        showToast('Script URL copied: ' + url, 'success');
      } catch {
        showToast('Copy failed — the script is at ' + url, 'info', true);
      }
    }
    async function iphoneWidgetRevokeToken() {
      const box = document.getElementById('iphone-widget-qr');
      try {
        const resp = await fetch('/api/widget/token', { method: 'DELETE', credentials: 'same-origin' });
        const d = await resp.json().catch(() => null);
        if (!resp.ok) throw new Error((d && d.error) || ('HTTP ' + resp.status));
        if (box) { box.textContent = ''; box.hidden = true; }
        showToast(d && d.revoked ? 'Widget token revoked — widgets using it stop updating.' : 'There was no widget token to revoke.', 'success');
      } catch (err) {
        showToast('Could not revoke the widget token: ' + (err && err.message ? err.message : err), 'error');
      }
      iphoneWidgetRefreshTokenStatus();
    }
    async function feedbackAddFiles(files) {
      if (!files) return;
      for (const f of Array.from(files)) {
        if (!f.type || f.type.indexOf('image/') !== 0) continue;
        if (f.size > FEEDBACK_MAX_SCREENSHOT_BYTES) { showToast('Screenshot too large (max 2 MiB)', 'error'); continue; }
        if ((window._feedbackScreenshots || []).length >= FEEDBACK_MAX_SCREENSHOTS) { showToast('At most 5 screenshots can be attached', 'error'); break; }
        const data = await new Promise((resolve, reject) => { const r = new FileReader(); r.onload = () => resolve(String(r.result || '')); r.onerror = reject; r.readAsDataURL(f); });
        window._feedbackScreenshots.push(data);
      }
      renderFeedbackThumbs(); updateFeedbackPreview();
    }
    function renderFeedbackThumbs() {
      const wrap = document.getElementById('feedback-thumbs'); if (!wrap) return; wrap.textContent = '';
      (window._feedbackScreenshots || []).forEach((src, i) => { const d = document.createElement('div'); d.className = 'feedback-thumb'; const img = document.createElement('img'); img.src = src; img.alt = 'screenshot ' + (i+1); const b = document.createElement('button'); b.type = 'button'; b.textContent = '×'; b.setAttribute('aria-label', 'Remove screenshot'); b.addEventListener('click', () => { window._feedbackScreenshots.splice(i, 1); renderFeedbackThumbs(); updateFeedbackPreview(); }); d.appendChild(img); d.appendChild(b); wrap.appendChild(d); });
    }
    async function buildFeedbackPayload() {
      const diag = await collectFeedbackDiagnostics();
      const enteredLogin = feedbackGitHubLogin(feedbackVal('feedback-github-username'));
      return { request_type: feedbackVal('feedback-type') || 'bug', target_repo: feedbackVal('feedback-target') || 'hive', title: feedbackRedact(feedbackVal('feedback-title')), description: feedbackRedact(feedbackVal('feedback-description')), submitter: { github_login: enteredLogin }, screenshots: window._feedbackScreenshots || [], include_diagnostics: document.getElementById('feedback-include-diagnostics')?.checked !== false, diagnostics: diag, console_errors: feedbackConsoleErrors.map(e => ({...e, message: feedbackRedact(e.message), source: feedbackStripQuery(e.source || '')})), failed_api_calls: feedbackFailedApiCalls.map(c => ({...c, path: feedbackStripQuery(c.path || '')})) };
    }
    async function collectFeedbackDiagnostics() {
      const st = window._lastStatus || {}; let version = {};
      try { const r = await fetch('/api/version'); if (r.ok) version = await r.json(); } catch {}
      const includeRepos = document.getElementById('feedback-include-projects')?.checked === true;
      const agents = (st.agents || []).map(a => ({ name: a.name || '', backend: a.backend || a.activeBackend || '', model: a.model || '', state: a.state || a.status || '', repo: includeRepos ? (a.repo || '') : '', org: includeRepos ? (a.org || '') : '' }));
      return feedbackDiagnosticPayloadFromValues({ version: version.short || version.version || '', commit: version.hash || '', channel: version.channel || '', acmm_level: String(st.acmmLevel || st.acmm_level || ''), hive_id: st.hiveId || '', hosted: !!(st.hosted || st.hubProxied), hub_linked: !!(version.autoUpgrade || st.hubLinked), agent_count: agents.length, agents: agents, include_project_repos: includeRepos, browser_user_agent: navigator.userAgent || '', browser_platform: navigator.platform || '', browser_language: navigator.language || '', screen_size: (screen.width + 'x' + screen.height), window_size: (window.innerWidth + 'x' + window.innerHeight), page: location.pathname + location.hash });
    }
    async function updateFeedbackPreview() { const el = document.getElementById('feedback-preview'); if (!el) return; const payload = await buildFeedbackPayload(); const preview = Object.assign({}, payload); preview.screenshots = (payload.screenshots || []).map((_, i) => '[screenshot ' + (i+1) + ']'); if (!payload.include_diagnostics) { delete preview.diagnostics; delete preview.console_errors; delete preview.failed_api_calls; } el.textContent = JSON.stringify(preview, null, 2); }
    async function submitFeedbackReport() {
      const btn = document.getElementById('feedback-submit'); if (btn) btn.disabled = true;
      let responded = false;
      try {
        const usernameField = document.getElementById('feedback-github-username');
        const rawUsername = usernameField ? usernameField.value.trim() : '';
        if (rawUsername && !feedbackGitHubLogin(rawUsername)) {
          if (usernameField) { usernameField.focus(); usernameField.setCustomValidity('Enter a valid GitHub username, or leave this blank.'); usernameField.reportValidity(); usernameField.setCustomValidity(''); }
          showToast('Enter a valid GitHub username, or leave it blank', 'error');
          return;
        }
        const payload = await buildFeedbackPayload();
        if (!payload.title.trim() || !payload.description.trim()) { showToast('Title and description are required', 'error'); return; }
        if (!payload.include_diagnostics) { payload.diagnostics = null; payload.console_errors = []; payload.failed_api_calls = []; }
        const r = await fetch('/api/feedback/report', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(payload) });
        responded = true;
        const raw = await r.text();
        let data = {};
        try { data = JSON.parse(raw) || {}; } catch {}
        if (!r.ok) throw new Error('HTTP ' + r.status + (r.statusText ? ' ' + r.statusText : '') + ' from /api/feedback/report: ' + (data.error || String(raw).replace(/\s+/g, ' ').trim().slice(0, 200) || 'no error detail returned; check the hive server logs'));
        if (!data.prefill_url) throw new Error('the hive did not return a prefilled issue link');
        try { localStorage.removeItem(FEEDBACK_DRAFT_KEY); } catch {}
        renderFeedbackPrefillResult(data);
        loadFeedbackReports(false);
      } catch (e) { console.error('feedback submission failed', e); showToast('Feedback failed: ' + ((e && e.message) || String(e)) + (responded ? '' : ' (before the request completed; see the browser console)'), 'error'); }
      finally { if (btn) btn.disabled = false; }
    }
    // Zero-scope handoff (#1927): open the prefilled GitHub issue in a new tab so
    // the user submits it as themselves. Popup blockers (and users who are not
    // signed in to GitHub) get the same URL as a copyable link, never a silent
    // failure. The bundle link is shown too, so nothing depends on the popup.
    function renderFeedbackPrefillResult(data) {
      const result = document.getElementById('feedback-result'); if (!result) return;
      const prefill = String(data.prefill_url || '');
      let opened = null;
      // No 'noopener' feature here: Chrome returns null for it even on success, which
      // would misreport a blocked popup. Sever the opener manually instead.
      try { opened = window.open(prefill, '_blank'); if (opened) opened.opener = null; } catch (e) { opened = null; }
      const blocked = !opened;
      let html = '<div class="feedback-success">';
      html += blocked
        ? '<strong>Your browser blocked the new tab.</strong> Open the prefilled issue yourself and submit it on GitHub as you:'
        : 'A prefilled GitHub issue opened in a new tab — review it and press <strong>Submit new issue</strong> there. If the tab did not appear:';
      html += ' <a class="hv-btn btn-primary btn-sm" target="_blank" rel="noopener noreferrer" href="' + escapeHtml(prefill) + '">Open prefilled issue</a>';
      html += ' <button type="button" class="hv-btn btn-secondary btn-sm" data-action="copyFeedbackPrefillURL">Copy link</button>';
      html += '<input id="feedback-prefill-url" class="feedback-prefill-url" type="text" readonly value="' + escapeHtml(prefill) + '">';
      if (data.bundle_url) html += '<p>Diagnostic bundle (stored by the hive\u2019s GitHub App, linked from the issue): <a target="_blank" rel="noopener noreferrer" href="' + escapeHtml(String(data.bundle_url)) + '">' + escapeHtml(String(data.bundle_url)) + '</a></p>';
      if (data.warning) html += '<p class="feedback-option-extra">' + escapeHtml(String(data.warning)) + '</p>';
      html += '</div>';
      result.innerHTML = html;
      showToast(blocked ? 'Popup blocked — use the link to open the issue' : 'Prefilled issue opened on GitHub', blocked ? 'error' : 'success');
    }
    function copyFeedbackPrefillURL() {
      const input = document.getElementById('feedback-prefill-url'); if (!input) return;
      const v = input.value;
      const done = function() { showToast('Link copied', 'success'); };
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(v).then(done, function() { input.select(); document.execCommand('copy'); done(); });
      else { input.select(); document.execCommand('copy'); done(); }
    }
    async function loadFeedbackReports(markSeen) {
      try {
        const r = await fetch('/api/feedback/mine' + (markSeen ? '?mark_seen=true' : ''));
        if (!r.ok) throw new Error('HTTP ' + r.status);
        const data = await r.json();
        window._feedbackReports = data.items || [];
        updateFeedbackPills(data.unread || feedbackUnreadCount(window._feedbackReports));
        renderFeedbackReports(window._feedbackReports, data.warning || '');
      } catch (e) {
        updateFeedbackPills(0);
        const list = document.getElementById('feedback-reports-list');
        if (list) list.textContent = 'Could not load feedback reports.';
      }
    }
    function updateFeedbackPills(count) {
      ['feedback-unread-pill','feedback-modal-pill'].forEach(function(id) { const el = document.getElementById(id); if (!el) return; el.hidden = !count; el.textContent = String(count); });
    }
    function renderFeedbackReports(items, warning) {
      const list = document.getElementById('feedback-reports-list'); if (!list) return;
      list.textContent = '';
      if (warning) { const w = document.createElement('div'); w.textContent = warning; list.appendChild(w); }
      if (!items || !items.length) { const empty = document.createElement('div'); empty.textContent = 'No feedback reports filed from this hive yet.'; list.appendChild(empty); return; }
      items.forEach(function(item) {
        const row = document.createElement('div'); row.style.marginBottom = '12px';
        const a = document.createElement('a'); a.href = item.html_url || '#'; a.target = '_blank'; a.rel = 'noopener noreferrer'; a.textContent = (item.owner || '') + '/' + (item.repo || '') + '#' + item.number + ' — ' + (item.title || 'Feedback report');
        const meta = document.createElement('div'); meta.textContent = (item.state || 'unknown') + ' · last activity ' + (item.updated_at || item.submitted_at || 'unknown') + ' · comments ' + (item.comments || 0);
        row.appendChild(a); row.appendChild(meta); list.appendChild(row);
      });
    }
    installFeedbackCapture();
    setTimeout(function(){ loadFeedbackReports(false); }, 2000);
    setInterval(function(){ loadFeedbackReports(false); }, FEEDBACK_POLL_MS);

    function safeJsonParse(str, fallback) {
      try { return JSON.parse(str); } catch(e) { return fallback; }
    }

    /** Escape HTML special characters to prevent XSS in innerHTML sinks */
    function escapeHtml(str) {
      if (typeof str !== 'string') return '';
      return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;').replace(/'/g, '&#39;');
    }

    /**
     * Escape HTML special characters, then turn http(s) URLs into clickable
     * <a> tags. Escaping always runs first so linkification can never
     * introduce unescaped markup from untrusted text (hivecommons/hive#9390).
     */
    function escapeHtmlAndLinkify(str) {
      return linkifyGitHubRefs(str);
    }

    function githubRefsBaseURL(defaultHost) {
      var base = defaultHost || (typeof window !== 'undefined' && window._githubBaseUrl) || 'https://github.com';
      base = String(base || '').trim().replace(/\/+$/, '');
      if (!base) base = 'https://github.com';
      if (!/^https?:\/\//i.test(base)) base = 'https://' + base.replace(/^\/+/, '');
      return base;
    }

    function normalizeGitHubRepoContext(repoContext) {
      var repo = String(repoContext || '').trim();
      if (/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo)) return repo;
      return '';
    }

    function linkifyGitHubRefs(text, defaultHost, repoContext) {
      var s = escapeHtml(text);
      var links = [];
      var base = githubRefsBaseURL(defaultHost);
      function stash(html) {
        var idx = links.length;
        links.push(html);
        return '\u0000GH_LINK_' + idx + '\u0000';
      }
      function githubIssueAnchor(label, owner, repo, number) {
        var href = base + '/' + owner + '/' + repo + '/issues/' + number;
        return '<a class="system-alert-link" href="' + escapeHtml(href) + '" target="_blank" rel="noopener noreferrer">' + label + '</a>';
      }
      s = s.replace(/https?:\/\/[^\s<>"']+/g, function(url) {
        var trailing = '';
        var trailingMatch = url.match(/[).,;:!?]+$/);
        if (trailingMatch) {
          trailing = trailingMatch[0];
          url = url.slice(0, url.length - trailing.length);
        }
        return stash('<a class="system-alert-link" href="' + url + '" target="_blank" rel="noopener noreferrer">' + url + '</a>' + trailing);
      });
      s = s.replace(/\b([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)#(\d+)\b/g, function(match, owner, repo, number) {
        return stash(githubIssueAnchor(match, owner, repo, number));
      });
      var context = normalizeGitHubRepoContext(repoContext);
      if (context) {
        var parts = context.split('/');
        s = s.replace(/(^|[\s("'])#(\d+)\b/g, function(match, lead, number) {
          return lead + stash(githubIssueAnchor('#' + number, parts[0], parts[1], number));
        });
      }
      s = s.replace(/\u0000GH_LINK_(\d+)\u0000/g, function(_, idx) {
        return links[Number(idx)] || '';
      });
      return s;
    }

    function stopSystemAlertLinkClicks(root) {
      if (!root || !root.querySelectorAll) return;
      root.querySelectorAll('a.system-alert-link').forEach(function(a) {
        a.addEventListener('click', function(event) { event.stopPropagation(); });
      });
    }

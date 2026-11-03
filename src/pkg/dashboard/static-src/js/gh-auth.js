    // GitHub Device Flow Auth
    let ghPollTimer = null;
    let ghPollIntervalMs = 6000;
    // The opaque per-flow secret /api/gh-user-auth/start mints. #5748 made it
    // MANDATORY on every poll — a poll that cannot prove it started the flow is
    // refused 400 and never gets the session cookie — but this page was missed
    // in that change and kept polling with an empty body, so the overlay spun on
    // "Waiting for authorization..." forever while GitHub reported the device
    // activated (#6216). Keep it here for the same reason the standalone login
    // page threads it through poll(): it is the only thing that identifies this
    // browser as the one that asked.
    let ghFlowID = '';
    const GH_AUTH_CHECK_INTERVAL_MS = 300000;
    const GH_AUTH_SUCCESS_DISMISS_MS = 1500;

    function copyDeviceCode() {
      const el = document.getElementById('gh-device-code');
      const code = (el.textContent || '').trim();
      const btn = document.getElementById('gh-copy-btn');
      const range = document.createRange();
      range.selectNodeContents(el);
      const sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
      let ok = false;
      try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
      if (!ok && navigator.clipboard) {
        navigator.clipboard.writeText(code).catch(() => {});
        ok = true;
      }
      btn.textContent = ok ? 'Copied!' : 'Selected — press Cmd+C';
      btn.style.background = 'var(--green)';
      setTimeout(() => { btn.textContent = 'Copy'; btn.style.background = 'var(--green)'; }, 2000);
    }

    /* Data-URI SVG avatar showing the person's initial — used for OIDC
       identities ("ibmid:5500…") that have no GitHub avatar to fetch. */
    function initialsAvatarURI(name) {
      var base = String(name || '?');
      var colon = base.indexOf(':');
      if (colon !== -1 && colon + 1 < base.length) base = base.slice(colon + 1);
      var ch = (base.trim().charAt(0) || '?').toUpperCase();
      var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="56" height="56">' +
        '<rect width="56" height="56" rx="28" fill="#30363d"/>' +
        '<text x="28" y="37" font-family="system-ui,sans-serif" font-size="26" fill="#e6edf3" text-anchor="middle">' +
        ch.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;') + '</text></svg>';
      return 'data:image/svg+xml;charset=utf-8,' + encodeURIComponent(svg);
    }

    function showGHAuthState(loggedIn, username, avatarURL, role, displayName) {
      if (username) window._hiveUser = username;
      if (role) window._hiveRole = role;
      const banner = document.getElementById('gh-auth-banner');
      const topBtn = document.getElementById('oc-gh-login-btn');
      const avatarWrap = document.getElementById('oc-gh-avatar-wrap');
      /* OIDC identity keys carry a provider prefix ("ibmid:…", "google:…").
         They have no github.com avatar or profile page, and the menu should
         show the person's display name, not the opaque key. Every permission
         check elsewhere still runs on the raw key. */
      const isProviderKey = !!(username && String(username).indexOf(':') !== -1);
      const shownName = displayName || username;
      if (loggedIn) {
        banner.style.display = 'none';
        if (topBtn) topBtn.style.display = 'none';
        if (avatarWrap) {
          avatarWrap.hidden = false;
          const img = document.getElementById('oc-gh-avatar');
          if (avatarURL) {
            img.src = avatarURL + (avatarURL.includes('?') ? '&' : '?') + 's=56';
            img.alt = shownName;
          } else if (isProviderKey) {
            img.src = initialsAvatarURI(shownName);
            img.alt = shownName;
          }
          document.getElementById('oc-gh-menu-user').textContent = shownName;
          /* Point the menu's profile entry at the signed-in account, and show it
             only once there is a login to point at. href is assigned as a
             property (not interpolated into markup), so encodeURIComponent on
             the path segment is the whole escaping story here. OIDC keys have
             no forge profile page — hide the entry entirely. */
          const profEl = document.getElementById('oc-gh-menu-profile');
          if (profEl) {
            if (username && !isProviderKey) {
              profEl.href = ghProfileURL(username);
              profEl.hidden = false;
            } else {
              profEl.hidden = true;
            }
          }
          const roleEl = document.getElementById('oc-gh-menu-role');
          if (roleEl) {
            roleEl.textContent = role ? ' · ' + role : '';
          }
          const isOwner = role === 'owner';
          const dlEl = document.getElementById('oc-gh-menu-download');
          if (dlEl) dlEl.hidden = !isOwner;
          const exportEl = document.getElementById('oc-gh-menu-config-export');
          if (exportEl) exportEl.hidden = !isOwner;
          const exportLabel = document.getElementById('oc-gh-menu-export-label');
          if (exportLabel) exportLabel.hidden = false;
          const bkEl = document.getElementById('oc-gh-menu-backup');
          if (bkEl) bkEl.hidden = !isOwner;
          const bkDivider = document.getElementById('oc-gh-menu-backup-divider');
          if (bkDivider) bkDivider.hidden = !isOwner;
          const bkLabel = document.getElementById('oc-gh-menu-backup-label');
          if (bkLabel) bkLabel.hidden = !isOwner;
        }
      } else {
        banner.style.display = 'flex';
        if (topBtn) topBtn.style.display = '';
        if (avatarWrap) avatarWrap.hidden = true;
      }
      if (typeof renderPublicKnowledgeControls === 'function') renderPublicKnowledgeControls();
    }

    /* renderMyHivesMenuItem (#9696): point the user menu's "My hives" entry at
       the hub's My Hives page, or hide it when this hive has no hub link. The
       URL comes from our own /api/config (server-validated), is re-checked to
       be http(s) here, and is assigned as a property, never interpolated into
       markup. Called from the boot /api/config fetch; the entry sits in the
       signed-in-only menu, so no auth state is needed here. */
    function renderMyHivesMenuItem() {
      const el = document.getElementById('oc-gh-menu-my-hives');
      if (!el) return;
      const url = String(window._myHivesURL || '');
      if (/^https?:\/\//i.test(url)) {
        el.href = url;
        el.hidden = false;
      } else {
        el.removeAttribute('href');
        el.hidden = true;
      }
    }

    function hiveSyncGHUserMenuTrigger() {
      const menu = document.getElementById('oc-gh-user-menu');
      const btn = document.getElementById('oc-gh-avatar-btn');
      if (menu && btn) btn.setAttribute('aria-expanded', menu.hidden ? 'false' : 'true');
    }

    function toggleGHUserMenu() {
      const menu = document.getElementById('oc-gh-user-menu');
      menu.hidden = !menu.hidden;
      hiveSyncGHUserMenuTrigger();
    }

    document.addEventListener('click', function(e) {
      const wrap = document.getElementById('oc-gh-avatar-wrap');
      if (wrap && !wrap.contains(e.target)) {
        document.getElementById('oc-gh-user-menu').hidden = true;
        hiveSyncGHUserMenuTrigger();
      }
    });

    document.addEventListener('click', function(e) {
      const wrap = document.getElementById('oc-git-version');
      const menu = document.getElementById('oc-version-menu');
      if (wrap && !wrap.contains(e.target) && !(menu && menu.contains(e.target))) closeVersionMenu();
    });
    window.addEventListener('resize', positionVersionMenuIfOpen);
    window.addEventListener('scroll', positionVersionMenuIfOpen, true);

    document.addEventListener('keydown', function(e) {
      const menu = document.getElementById('oc-version-menu');
      const chip = document.getElementById('oc-version-chip');
      if (!menu || menu.hidden) return;
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
      const items = Array.from(menu.querySelectorAll('button:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])'));
      if (!items.length) return;
      e.preventDefault();
      const idx = items.indexOf(document.activeElement);
      const next = e.key === 'ArrowDown' ? (idx + 1) % items.length : (idx <= 0 ? items.length - 1 : idx - 1);
      items[next].focus();
    });

    /* Owner-only self-service backup.
       POSTs (not a plain link) because the response body carries GitHub App
       private keys: keeping it fetch-only keeps it off the cross-origin
       navigation path. The archive arrives already encrypted with this hive's
       backup encryption key — set in Settings → Security → Backup, or
       supplied by the deployment as HIVE_BACKUP_KEY. The browser never sees
       that key, and it is NOT in the file, so the owner must have it escrowed
       to restore. With no key configured the server refuses (412) rather than
       producing an unencrypted archive. */
    async function downloadHiveBackup() {
      const btn = document.getElementById('oc-gh-menu-backup');
      if (btn && btn.dataset.busy === '1') return;
      if (btn) {
        btn.dataset.busy = '1';
        btn.style.opacity = '0.6';
        const label = btn.querySelector('.avatar-menu-backup-label');
        if (label) label.textContent = 'Backing up…';
      }
      hiveCloseGHUserMenu();
      showToast('Building encrypted backup — this can take a moment…', 'info');
      let url = null;
      try {
        const resp = await fetch('/api/backup', { method: 'POST' });
        if (!resp.ok) {
          let msg = 'Backup failed (' + resp.status + ')';
          try { const d = await resp.json(); if (d && d.error) msg = d.error; } catch (e) { /* non-JSON body */ }
          showToast(msg, 'error');
          /* 412 is the fail-closed refusal: no encryption key is configured.
             Take the owner straight to the setting they can change themselves
             instead of leaving them with an error about a deployment they may
             have no access to. */
          if (resp.status === 412 && typeof welcomeShowConfigTab === 'function') {
            welcomeShowConfigTab('Security');
          }
          return;
        }
        /* The archive is binary; do not synthesize a browser save. */
        let name = 'hive-spoke-backup.tar.gz.enc';
        const cd = resp.headers.get('Content-Disposition') || '';
        const m = cd.match(/filename="([^"]+)"/);
        if (m && m[1]) name = m[1];
        const beadDirs = resp.headers.get('X-Hive-Backup-Bead-Dirs') || '0';
        const files = resp.headers.get('X-Hive-Backup-Files') || '0';
        hiveBinaryExportNotice(name, 'Encrypted backup archive prepared (' + files + ' files, ' + beadDirs + ' bead ledgers), but it is a binary file.');
      } catch (e) {
        showToast('Backup failed: ' + (e && e.message ? e.message : 'network error'), 'error');
      } finally {
        /* Revoke on the next tick so the click has already been dispatched. */
        if (url) setTimeout(function () { URL.revokeObjectURL(url); }, 60000);
        if (btn) {
          btn.dataset.busy = '';
          btn.style.opacity = '';
          const label = btn.querySelector('.avatar-menu-backup-label');
          if (label) label.textContent = 'Back up this hive';
        }
      }
    }

    async function ghLogout() {
      hiveCloseGHUserMenu();
      try {
        await fetch('/api/gh-user-auth/logout', { method: 'POST' });
      } catch (e) { /* ignore */ }
      showGHAuthState(false);
      showToast('Signed out of GitHub', 'info');
    }

    async function checkGHAuth() {
      try {
        const roleResp = await fetch('/api/role');
        const roleData = await roleResp.json();
        if (roleData.sentinel_label) window._hiveSentinelLabel = roleData.sentinel_label;
        if (roleData.user) {
          if (roleData.automerge_label) window._hiveAutoMergeLabel = roleData.automerge_label;
          /* OIDC identity keys ("ibmid:5500…") are not GitHub logins — don't
             fabricate a github.com/<key>.png avatar for them; showGHAuthState
             falls back to an initials avatar and the server-resolved
             display_name. */
          var ghAvatar = roleData.user.indexOf(':') === -1
            ? 'https://github.com/' + roleData.user + '.png'
            : '';
          showGHAuthState(true, roleData.user, ghAvatar, roleData.role, roleData.display_name);
          return;
        }
      } catch (e) {}
      try {
        const resp = await fetch('/api/gh-user-auth/status');
        const data = await resp.json();
        showGHAuthState(data.logged_in, data.username, data.avatar_url);
      } catch (e) {}
    }

    function showSetupDetails(which) {
      document.getElementById('setup-details-public').style.display = which === 'public' ? 'block' : 'none';
      document.getElementById('setup-details-custom').style.display = which === 'custom' ? 'block' : 'none';
    }

    async function startGHLogin() {
      try {
        const resp = await fetch('/api/gh-user-auth/start', { method: 'POST' });
        const data = await resp.json();
        if (data.error) {
          if (data.error.includes('oauth_client_id')) {
            var ghBase = window._lastStatus && window._lastStatus.githubBaseURL || 'https://github.com';
            var isGHE = ghBase !== 'https://github.com';
            var publicOption = document.getElementById('setup-option-public');
            if (publicOption) publicOption.style.display = isGHE ? 'none' : '';
            var customLink = document.getElementById('setup-custom-apps-link');
            if (customLink) { customLink.href = ghBase + '/settings/apps/new'; customLink.textContent = ghBase.replace('https://', '') + '/settings/apps/new'; }
            document.getElementById('gh-setup-overlay').style.display = 'flex';
            hiveRememberModalOpen(document.getElementById('gh-setup-overlay'));
            document.getElementById('setup-details-public').style.display = 'none';
            document.getElementById('setup-details-custom').style.display = isGHE ? 'block' : 'none';
          } else {
            showToast('Login error: ' + data.error, 'error');
          }
          return;
        }
        ghFlowID = data.flow_id || '';
        document.getElementById('gh-device-code').textContent = data.user_code;
        document.getElementById('gh-verify-link').href = data.verification_uri;
        document.getElementById('gh-poll-status').textContent = 'Waiting for authorization...';
        document.getElementById('gh-poll-status').style.color = 'var(--muted)';
        document.getElementById('gh-auth-modal-overlay').style.display = 'flex';
        hiveRememberModalOpen(document.getElementById('gh-auth-modal-overlay'));
        ghPollIntervalMs = data.interval ? (data.interval + 1) * 1000 : 6000;
        if (ghPollTimer) clearInterval(ghPollTimer);
        ghPollTimer = setInterval(pollGHAuth, ghPollIntervalMs);
      } catch (e) {
        showToast('Login error: ' + e.message, 'error');
      }
    }

    // Paint a terminal failure into the overlay and stop polling. Shared by the
    // server's own {"status":"error"} and by the unrecognized-response branch
    // below, so both look the same to the operator.
    function failGHAuth(msg) {
      if (ghPollTimer) { clearInterval(ghPollTimer); ghPollTimer = null; }
      ghFlowID = '';
      const status = document.getElementById('gh-poll-status');
      status.textContent = 'Error: ' + (msg || 'unknown');
      status.style.color = 'var(--red)';
    }

    async function pollGHAuth() {
      try {
        // flow_id proves this poll belongs to the flow WE started; without it
        // the server refuses every poll with 400 and the login can never
        // complete (#6216). Same contract as the standalone login page.
        const resp = await fetch('/api/gh-user-auth/poll', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ flow_id: ghFlowID }),
        });
        const data = await resp.json();
        if (data.status === 'complete') {
          clearInterval(ghPollTimer);
          ghPollTimer = null;
          const status = document.getElementById('gh-poll-status');
          status.textContent = 'Signed in as ' + data.username;
          status.style.color = 'var(--green)';
          status.style.fontWeight = '700';
          document.getElementById('gh-device-code').textContent = data.username;
          document.getElementById('gh-device-code').style.color = 'var(--green)';
          ghFlowID = '';
          setTimeout(() => {
            document.getElementById('gh-auth-modal-overlay').style.display = 'none';
            document.getElementById('gh-device-code').style.color = '';
            showGHAuthState(true, data.username, data.avatar_url);
          }, GH_AUTH_SUCCESS_DISMISS_MS);
          showToast('Signed in as ' + data.username, 'success');
        } else if (data.status === 'slow_down') {
          clearInterval(ghPollTimer);
          const SLOWDOWN_BACKOFF_MS = 5000;
          ghPollIntervalMs += SLOWDOWN_BACKOFF_MS;
          ghPollTimer = setInterval(pollGHAuth, ghPollIntervalMs);
        } else if (data.status === 'error') {
          failGHAuth(data.error);
        } else if (data.status === 'pending') {
          // The expected steady state while the operator is still at
          // github.com/login/device — keep the timer running.
        } else if (!resp.ok || data.error) {
          // Any other shape is terminal: an HTTP error body carries {"ok":false,
          // "error":"..."} and no `status` at all, so it matched none of the
          // branches above and the loop just fired again. That is exactly how
          // #6216 presented — an eternal spinner and no message, from #5748
          // through v4.18.1. Surface it, so the next protocol change shows up
          // as a visible error rather than a silent hang.
          failGHAuth(data.error || ('login failed (HTTP ' + resp.status + ')'));
        }
      } catch (e) {
        // network hiccup — keep polling
      }
    }

    function cancelGHLogin() {
      if (ghPollTimer) { clearInterval(ghPollTimer); ghPollTimer = null; }
      // Drop the abandoned flow's secret rather than carrying it into whatever
      // the operator does next.
      ghFlowID = '';
      document.getElementById('gh-auth-modal-overlay').style.display = 'none';
    }

    // ESC for GH auth/setup handled by consolidated keydown listener above

    // --- Claude Code OAuth login ---
    // loginAgent dispatches to the right login flow based on CLI backend.
    function loginAgent(agentName, cli) {
      if (cli === 'claude') {
        startClaudeLogin();
      } else if (cli === 'copilot') {
        startCopilotLogin();
      } else {
        showToast('Manual login required for ' + cli + ' — open the terminal and run /login', 'info');
      }
    }

    const COPILOT_AUTH_POLL_MS = 3000;

    async function startCopilotLogin() {
      try {
        var resp = await fetch('/api/copilot-auth/start', { method: 'POST' });
        var data = await resp.json();
        if (data.error) {
          showToast('Copilot login error: ' + data.error, 'error');
          return;
        }
        showCopilotDeviceModal(data.user_code, data.verification_uri);
      } catch (e) {
        showToast('Copilot login error: ' + e.message, 'error');
      }
    }

    function showCopilotDeviceModal(userCode, verificationUri) {
      var overlay = document.getElementById('copilot-auth-overlay');
      if (overlay) { overlay.remove(); }
      overlay = document.createElement('div');
      overlay.id = 'copilot-auth-overlay';
      overlay.className = 'gh-auth-overlay cred-dialog-overlay';
      overlay.innerHTML = '<div class="gh-auth-modal" style="max-width:480px;text-align:center">' +
        '<h3 class="text-success">Login GitHub Copilot</h3>' +
        '<p style="color:var(--muted);font-size:0.85rem;margin:var(--sp-5) var(--sp-0)">Enter this one-time code at github.com/login/device:</p>' +
        '<div id="copilot-user-code" style="font-size:var(--fs-3xl);font-weight:700;letter-spacing:4px;color:var(--green);' +
          'background:#f0fdf4;border:1px solid #bbf7d0;border-radius:8px;padding:14px;margin:var(--sp-5) var(--sp-0);cursor:pointer" ' +
          'title="Click to copy" data-action="copyCopilotCode">' + userCode + '</div>' +
        '<div id="copilot-auth-status" style="color:var(--muted);font-size:0.8rem;min-height:20px;margin:var(--sp-4) var(--sp-0)">Waiting for authorization…</div>' +
        '<div style="display:flex;gap:var(--sp-4);justify-content:center;margin-top:var(--sp-4)">' +
          '<button data-action="ghOpenUrl" data-arg0="' + esc(verificationUri) + '" ' +
            'style="background:var(--green);color:var(--bg);border:none;padding:var(--sp-3) var(--sp-6);border-radius:var(--r);font-weight:600;cursor:pointer">Open GitHub</button>' +
          '<button data-action="cancelCopilotLogin" class="hv-btn btn-secondary gh-cancel btn-primary">Cancel</button>' +
        '</div>' +
      '</div>';
      document.body.appendChild(overlay);
      overlay.style.display = 'flex';
      overlay._pollInterval = setInterval(pollCopilotAuth, COPILOT_AUTH_POLL_MS);
    }

    function copyCopilotCode() {
      var el = document.getElementById('copilot-user-code');
      if (!el) return;
      navigator.clipboard.writeText(el.textContent.trim()).then(function() {
        showToast('Code copied to clipboard', 'success');
      });
    }

    async function pollCopilotAuth() {
      var statusEl = document.getElementById('copilot-auth-status');
      try {
        var resp = await fetch('/api/copilot-auth/status');
        var data = await resp.json();
        if (data.error) {
          if (statusEl) { statusEl.textContent = data.error; statusEl.style.color = 'var(--red)'; }
          cancelCopilotLogin(true);
          return;
        }
        if (data.logged_in && !data.pending) {
          showCopilotSeatVerdict(statusEl, data.seat);
        }
      } catch (e) { /* keep polling */ }
    }

    // A stored token is not an entitlement: activation can fail server-side and
    // org policy can refuse the integration, both of which leave a token behind
    // that looks like a working login (issue 7309). The dialog therefore reports
    // the verified seat verdict, not the mere presence of a token.
    function showCopilotSeatVerdict(statusEl, seat) {
      var state = (seat && seat.state) || 'unknown';
      if (state === 'active') {
        if (statusEl) { statusEl.textContent = 'Logged in — Copilot seat verified'; statusEl.style.color = 'var(--green)'; }
        showToast('GitHub Copilot authenticated and seat verified', 'success');
        setTimeout(function() { cancelCopilotLogin(); }, 1500);
        return;
      }
      if (state === 'unknown') {
        if (statusEl) { statusEl.textContent = 'Logged in — seat not yet verified'; statusEl.style.color = 'var(--muted)'; }
        showToast('GitHub Copilot authenticated', 'success');
        setTimeout(function() { cancelCopilotLogin(); }, 1500);
        return;
      }
      // Verified failure: keep the dialog open with the actionable detail.
      // Closing here is what previously sent adopters away believing the
      // backend worked.
      var warn = (state === 'unreachable') ? 'var(--muted)' : 'var(--red)';
      if (statusEl) {
        statusEl.innerHTML = '<strong>Login saved, but this account cannot run inference yet.</strong>' +
          '<div style="margin-top:var(--sp-4);text-align:left;line-height:1.45">' + esc(seat.detail || '') + '</div>' +
          (seat.credential ? '<div style="margin-top:var(--sp-4);text-align:left;color:var(--muted)">Verified: ' +
            esc(seat.credential) + '</div>' : '') +
          '<button data-action="recheckCopilotSeat" style="margin-top:10px;background:var(--green);color:var(--bg);' +
            'border:none;padding:var(--sp-3) var(--sp-6);border-radius:var(--r);font-weight:600;cursor:pointer">Re-verify</button>';
        statusEl.style.color = warn;
      }
      cancelCopilotLogin(true);
    }

    // Re-verify from the login dialog, repainting it in place so an adopter who
    // has just activated a seat or had a policy lifted sees the new verdict
    // without restarting the device flow.
    async function recheckCopilotSeat() {
      var statusEl = document.getElementById('copilot-auth-status');
      if (statusEl) { statusEl.textContent = 'Verifying…'; statusEl.style.color = 'var(--muted)'; }
      var seat = await verifyCopilotSeat();
      if (seat) showCopilotSeatVerdict(document.getElementById('copilot-auth-status'), seat);
    }

    // Re-run verification on demand; used by the "Verify login" affordance so an
    // adopter can self-check after fixing a seat or an org policy, without
    // logging out and back in.
    async function verifyCopilotSeat() {
      try {
        var resp = await fetch('/api/copilot-auth/verify', { method: 'POST' });
        var data = await resp.json();
        var seat = data.seat || {};
        if (seat.state === 'active') {
          showToast('Copilot seat verified — inference is available', 'success');
        } else if (seat.state === 'unknown') {
          showToast('No Copilot credential to verify — log in first', 'info');
        } else {
          showToast(seat.detail || 'Copilot seat could not be verified', 'error');
        }
        return seat;
      } catch (e) {
        showToast('Copilot verification failed: ' + e.message, 'error');
      }
    }

    function cancelCopilotLogin(keepOverlay) {
      var overlay = document.getElementById('copilot-auth-overlay');
      if (!overlay) return;
      if (overlay._pollInterval) clearInterval(overlay._pollInterval);
      if (!keepOverlay) overlay.remove();
    }

    // --- bob (IBM bobshell) API key ---
    // bob cannot complete its browser SSO flow inside a pod: the callback is
    // http://localhost:35227/bob-callback, which resolves to the pod rather
    // than the operator's machine, and the CLI exposes no device-code flow.
    // The API key is therefore the only headless credential.
    //
    // The key is hive-wide, not per-agent — governor.bob.api_key_file is a
    // single hive-scoped setting resolving to one file — so this dialog is
    // reached from the governor's Bob tab, not from an agent card. The key is
    // PUT once and never read back; the dashboard only ever learns WHETHER one
    // is configured and the safe source string.
    //
    // `configured` / `source` are passed in by the caller (the Bob tab already
    // fetched them) so opening the dialog does not re-round-trip.
    // Where an operator obtains a bob API key. Named constants rather than
    // inline literals so the three links stay in one place if IBM moves them.
    //
    // NOTE ON SCOPE: IBM's own docs disagree. The API-keys page describes
    // General as usable across services, while the bob Shell setup page says
    // "Create an API key with Scope set to Inference". The Shell page is the
    // authoritative one for this CLI, so the dialog says Inference and links
    // BOTH pages so the operator can check if a key is rejected.
    var BOB_PORTAL_URL = 'https://bob.ibm.com';
    var BOB_API_KEY_MANAGE_URL = 'https://bob.ibm.com/admin/apikeys';
    var BOB_API_KEY_DOCS_URL = 'https://bob.ibm.com/docs/ide/account/api-keys';
    var BOB_SHELL_SETUP_DOCS_URL = 'https://bob.ibm.com/docs/shell/getting-started/install-and-setup';

    // testBobKey POSTs to the bob key-test endpoint and reports the verdict
    // via setStatus. `pastedKey` non-empty tests THAT key (pre-save); empty
    // tests the SAVED key — the server resolves it, the value never reaches
    // the browser. Shared by the key dialog and the Bob tab card.
    async function testBobKey(pastedKey, setStatus) {
      setStatus(pastedKey ? 'Testing pasted key…' : 'Testing saved key…');
      try {
        var resp = await fetch('/api/config/governor/bob/test', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(pastedKey ? { apiKey: pastedKey } : {})
        });
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) {
          setStatus(data.error || ('Test failed (HTTP ' + resp.status + ')'), 'var(--red)');
        } else if (data.status === 'ok') {
          setStatus('✓ ' + (data.detail || 'The bob backend accepted the key.'), 'var(--green)');
        } else {
          setStatus('✗ ' + (data.detail || data.reason || 'Test failed'), 'var(--red)');
        }
      } catch (e) {
        setStatus('Test failed: ' + e.message, 'var(--red)');
      }
    }

    function openBobKeyDialog(configured, source, keyName) {
      var overlay = document.getElementById('bob-key-overlay');
      if (overlay) { overlay.remove(); }
      overlay = document.createElement('div');
      overlay.id = 'bob-key-overlay';
      overlay.className = 'gh-auth-overlay cred-dialog-overlay';
      overlay.innerHTML = '<div class="gh-auth-modal" style="max-width:520px;text-align:left">' +
        '<h3 style="color:#4589ff;text-align:center">bob API key</h3>' +
        '<p style="color:var(--muted);font-size:0.85rem;margin:var(--sp-5) var(--sp-0)">' +
          'bob cannot sign in with a browser inside this hive, so it authenticates with an API key. ' +
          'The key applies to every agent using the bob backend. It is stored in a private file ' +
          'on this hive and is never shown again.</p>' +
        (configured
          ? '<div style="color:var(--green);font-size:0.8rem;margin:var(--sp-4) var(--sp-0)">✓ A key is currently set' +
              (source ? ' (' + esc(source) + ')' : '') + '. Entering a new one replaces it.</div>'
          : '<div style="color:var(--muted);font-size:0.8rem;margin:var(--sp-4) var(--sp-0)">No key is set — agents using bob cannot start.</div>') +
        /* Where to get a key. Without this the dialog assumes the operator
           already knows the portal flow, and the scope choice is easy to get
           wrong: an Inference-scoped key is what bob Shell documents, and a
           key is shown exactly once, so a mis-scoped key means starting over. */
        '<div style="background:rgba(69,137,255,0.08);border:1px solid rgba(69,137,255,0.25);' +
          'border-radius:var(--r);padding:10px 12px;margin:var(--sp-5) var(--sp-0);font-size:0.8rem;color:var(--muted)">' +
          '<div style="color:var(--text);font-weight:600;margin-bottom:var(--sp-3)">How to get a key</div>' +
          '<ol style="margin:var(--sp-0);padding-left:18px;line-height:1.6">' +
            '<li>Sign in at <a href="' + BOB_PORTAL_URL + '" target="_blank" rel="noopener noreferrer" ' +
              'style="color:#4589ff">bob.ibm.com</a> and open your subscription instance.</li>' +
            '<li>Go to <a href="' + BOB_API_KEY_MANAGE_URL + '" target="_blank" rel="noopener noreferrer" ' +
              'style="color:#4589ff">API key management</a> and create a key with <strong>Scope: Inference</strong>.</li>' +
            '<li>Copy it immediately — IBM shows the value only once.</li>' +
          '</ol>' +
          '<div class="mt-4">' +
            '<a href="' + BOB_API_KEY_DOCS_URL + '" target="_blank" rel="noopener noreferrer" ' +
              'style="color:#4589ff">API key docs</a> · ' +
            '<a href="' + BOB_SHELL_SETUP_DOCS_URL + '" target="_blank" rel="noopener noreferrer" ' +
              'style="color:#4589ff">bob Shell setup</a>' +
          '</div>' +
        '</div>' +
        /* Optional label. Not a secret — it records WHICH key this is
           (e.g. "Team inference key") so managers can tell keys apart later
           without ever seeing the value. Pre-filled with the current name so
           a plain "Replace key" keeps it. */
        '<label for="bob-key-name-input" style="display:block;font-size:0.78rem;color:var(--muted);margin:var(--sp-4) var(--sp-0) var(--sp-2)">' +
          'Key name <span class="text-muted">(optional label, not the secret)</span></label>' +
        '<input id="bob-key-name-input" type="text" autocomplete="off" spellcheck="false" maxlength="128" ' +
          'placeholder="e.g. Team inference key" value="' + esc(keyName || '') + '" ' +
          'style="width:100%;box-sizing:border-box;padding:var(--sp-4);border-radius:var(--r);border:1px solid var(--border);' +
          'background:var(--panel);color:var(--fg);margin:var(--sp-0) var(--sp-0) var(--sp-4)">' +
        '<input id="bob-key-input" type="password" autocomplete="off" spellcheck="false" ' +
          'placeholder="Paste the bob API key" ' +
          'style="width:100%;box-sizing:border-box;padding:var(--sp-4);border-radius:var(--r);border:1px solid var(--border);' +
          'background:var(--panel);color:var(--fg);font-family:monospace;margin:var(--sp-4) var(--sp-0)">' +
        '<div id="bob-key-status" style="color:var(--muted);font-size:0.8rem;min-height:20px;margin:var(--sp-2) var(--sp-0)"></div>' +
        '<div style="display:flex;gap:var(--sp-4);justify-content:flex-end;margin-top:var(--sp-4)">' +
          '<button id="bob-key-cancel" class="hv-btn btn-secondary gh-cancel btn-primary">Cancel</button>' +
          '<button id="bob-key-test" style="background:var(--bg);color:var(--fg);border:1px solid var(--border);' +
            'padding:var(--sp-3) var(--sp-6);border-radius:var(--r);font-weight:600;cursor:pointer" ' +
            'title="Validate against the bob backend without saving: tests the pasted key if the field is non-empty, else the saved key">Test key</button>' +
          '<button id="bob-key-save" style="background:#0f62fe;color:#fff;border:none;padding:var(--sp-3) var(--sp-6);' +
            'border-radius:var(--r);font-weight:600;cursor:pointer">Save key</button>' +
        '</div>' +
      '</div>';
      document.body.appendChild(overlay);
      overlay.style.display = 'flex';

      var input = document.getElementById('bob-key-input');
      var statusEl = document.getElementById('bob-key-status');
      if (input) input.focus();

      function closeDialog() {
        // Drop the secret from the DOM before removing the node.
        if (input) input.value = '';
        overlay.remove();
      }
      function setStatus(msg, color) {
        if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || 'var(--muted)'; }
      }

      var cancelBtn = document.getElementById('bob-key-cancel');
      if (cancelBtn) cancelBtn.addEventListener('click', closeDialog);

      // Test key: pre-save validation of the pasted value, or of the saved
      // key when the field is empty. The pasted value goes only to the test
      // endpoint (never persisted); the result renders inline.
      var testBtn = document.getElementById('bob-key-test');
      if (testBtn) testBtn.addEventListener('click', async function() {
        var pasted = (input && input.value || '').trim();
        if (!pasted && !configured) {
          setStatus('Paste a key to test — none is saved yet.', 'var(--red)');
          return;
        }
        testBtn.disabled = true;
        await testBobKey(pasted, setStatus);
        testBtn.disabled = false;
      });

      var saveBtn = document.getElementById('bob-key-save');
      if (saveBtn) saveBtn.addEventListener('click', async function() {
        var key = (input && input.value || '').trim();
        if (!key) { setStatus('Paste a key first.', 'var(--red)'); return; }
        // The name is an optional plaintext label sent alongside the key. Always
        // sending the field (even empty) lets the operator clear a stale name by
        // blanking it; the server trims and length-bounds it.
        var nameInput = document.getElementById('bob-key-name-input');
        var keyName = (nameInput && nameInput.value || '').trim();
        saveBtn.disabled = true;
        setStatus('Saving…');
        try {
          var resp = await fetch('/api/config/governor/bob', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ apiKey: key, keyName: keyName })
          });
          var data = await resp.json().catch(function() { return {}; });
          if (!resp.ok) {
            setStatus(data.error || ('Save failed (' + resp.status + ')'), 'var(--red)');
            saveBtn.disabled = false;
            return;
          }
          closeDialog();
          // The hive reads the key at each agent launch, so no pod restart is
          // needed — and the save also relaunches any bob agent that was parked
          // for a missing key. Report the count the server actually relaunched
          // rather than telling the user to go start agents by hand.
          var relaunched = Number(data.relaunched) || 0;
          showToast(relaunched > 0
            ? 'bob API key saved — no restart needed; relaunched ' + relaunched +
              ' bob agent' + (relaunched === 1 ? '' : 's') + ' that was waiting for a key'
            : 'bob API key saved — no restart needed', 'success');
          refreshBobKeyPanel();
        } catch (e) {
          setStatus('Save failed: ' + e.message, 'var(--red)');
          saveBtn.disabled = false;
        }
      });
    }

    // clearBobKey revokes the stored key from the governor's Bob tab. Clearing
    // is idempotent server-side; if an admin-managed Secret or env var still
    // supplies a key the response says so, and we report that rather than
    // falsely claiming the key was removed.
    async function clearBobKey() {
      if (!await hiveConfirm({title: 'Remove bob API key?', message: 'Agents using bob will stop being able to start.', confirmLabel: 'Remove key', danger: true})) return;
      var statusEl = document.getElementById('bob-key-inline-status');
      function setStatus(msg, color) {
        if (statusEl) { statusEl.textContent = msg; statusEl.style.color = color || 'var(--muted)'; }
      }
      setStatus('Clearing…');
      try {
        var resp = await fetch('/api/config/governor/bob', { method: 'DELETE' });
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) {
          setStatus(data.error || ('Clear failed (' + resp.status + ')'), 'var(--red)');
          return;
        }
        setStatus('');
        if (data.configured) {
          showToast('Stored key removed, but a key is still provided by ' + (data.source || 'another source'), 'info');
        } else {
          showToast('bob API key cleared', 'success');
        }
        refreshBobKeyPanel();
      } catch (e) {
        setStatus('Clear failed: ' + e.message, 'var(--red)');
      }
    }

    // refreshBobKeyPanel repaints the Bob tab after a set/clear, but only if
    // that tab is still mounted — the user may have switched tabs or closed
    // the dialog in the meantime.
    function refreshBobKeyPanel() {
      if (document.getElementById('gov-bob-host')) loadBobKeyStatus();
    }

    async function checkClaudeAuth() {
      // No UI elements to update — the agent cards show login state
      // via needsLogin from the SSE status stream. This just keeps
      // the status endpoint warm for the paste-modal flow.
      try { await fetch('/api/claude-auth/status'); } catch (e) { /* ignore */ }
    }

    const CLAUDE_AUTH_TAB_DELAY_MS = 5000;

    async function startClaudeLogin() {
      try {
        var resp = await fetch('/api/claude-auth/start', { method: 'POST' });
        var data = await resp.json();
        if (data.error) {
          showToast('Claude login error: ' + data.error, 'error');
          return;
        }
        // Show the paste-back modal with countdown, then open the auth tab
        showClaudePasteModal(data.authorize_url);
      } catch (e) {
        showToast('Claude login error: ' + e.message, 'error');
      }
    }

    function showClaudePasteModal(authorizeUrl) {
      var overlay = document.getElementById('claude-auth-overlay');
      if (overlay) { overlay.remove(); }
      overlay = document.createElement('div');
      overlay.id = 'claude-auth-overlay';
      overlay.className = 'gh-auth-overlay cred-dialog-overlay';
      var countdownSec = CLAUDE_AUTH_TAB_DELAY_MS / 1000;
      overlay.innerHTML = '<div class="gh-auth-modal" style="max-width:560px;text-align:left">' +
        '<h3 style="color:#d97706;text-align:center">Login Claude Code</h3>' +
        '<div id="claude-countdown" style="text-align:center;color:#d97706;font-weight:600;font-size:0.9rem;margin:var(--sp-4) var(--sp-0)">' +
          'Opening Claude login in ' + countdownSec + 's…</div>' +
        '<ol style="color:var(--muted);font-size:0.85rem;line-height:1.8;padding-left:1.5em;margin:var(--sp-5) var(--sp-0)">' +
          '<li>A new tab will open with the Claude authorization page</li>' +
          '<li>Click <strong>Authorize</strong></li>' +
          '<li>Copy the <strong>authorization code</strong> shown on the next page and paste it below</li>' +
        '</ol>' +
        '<input id="claude-callback-input" type="text" placeholder="Paste the authorization code here" ' +
          'style="width:100%;padding:10px;border:1px solid #d1d5db;border-radius:var(--r);font-size:0.9rem;margin:var(--sp-5) var(--sp-0);box-sizing:border-box">' +
        '<div id="claude-exchange-status" style="color:var(--muted);font-size:0.8rem;min-height:20px;margin-bottom:var(--sp-4)"></div>' +
        '<div style="display:flex;gap:var(--sp-4);justify-content:center">' +
          '<button class="hv-btn btn-primary" data-action="submitClaudeCode">Submit</button>' +
          '<button data-action="cancelClaudeLogin" class="hv-btn btn-secondary gh-cancel btn-primary">Cancel</button>' +
        '</div>' +
      '</div>';
      document.body.appendChild(overlay);
      overlay.style.display = 'flex';
      // Countdown timer
      var remaining = countdownSec;
      var cdEl = document.getElementById('claude-countdown');
      var countdownInterval = setInterval(function() {
        remaining--;
        if (remaining <= 0) {
          clearInterval(countdownInterval);
          if (cdEl) cdEl.textContent = 'Claude login tab opened';
          if (authorizeUrl) {
            hiveExportViewer({title:'Claude authorization URL', text:authorizeUrl, sourceURL:authorizeUrl, allowOpen:true});
          }
        } else if (cdEl) {
          cdEl.textContent = 'Opening Claude login in ' + remaining + 's…';
        }
      }, 1000);
      // Store interval ID so cancel can clear it
      overlay._countdownInterval = countdownInterval;
      // Allow pressing Enter to submit
      var input = document.getElementById('claude-callback-input');
      if (input) input.addEventListener('keydown', function(e) { if (e.key === 'Enter') submitClaudeCode(); });
    }

    async function submitClaudeCode() {
      var input = document.getElementById('claude-callback-input');
      var statusEl = document.getElementById('claude-exchange-status');
      if (!input || !input.value.trim()) {
        if (statusEl) { statusEl.textContent = 'Please paste the callback URL'; statusEl.style.color = 'var(--red)'; }
        return;
      }
      var value = input.value.trim();
      // Accept either a full URL or just the code
      var payload = {};
      if (value.startsWith('http')) {
        payload.callback_url = value;
      } else {
        payload.code = value;
      }
      if (statusEl) { statusEl.textContent = 'Exchanging code for token...'; statusEl.style.color = 'var(--muted)'; }
      try {
        var resp = await fetch('/api/claude-auth/exchange', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        var data = await resp.json();
        if (data.error) {
          if (statusEl) { statusEl.textContent = data.error; statusEl.style.color = 'var(--red)'; }
          return;
        }
        if (statusEl) { statusEl.textContent = 'Authenticated!'; statusEl.style.color = 'var(--green)'; }
        showToast('Claude Code authenticated!', 'success');
        var CLAUDE_SUCCESS_DISMISS_MS = 1500;
        setTimeout(function() {
          var overlay = document.getElementById('claude-auth-overlay');
          if (overlay) overlay.style.display = 'none';
          checkClaudeAuth();
        }, CLAUDE_SUCCESS_DISMISS_MS);
      } catch (e) {
        if (statusEl) { statusEl.textContent = 'Error: ' + e.message; statusEl.style.color = 'var(--red)'; }
      }
    }

    function cancelClaudeLogin() {
      var overlay = document.getElementById('claude-auth-overlay');
      if (overlay) {
        if (overlay._countdownInterval) clearInterval(overlay._countdownInterval);
        overlay.remove();
      }
    }

    async function logoutClaude() {
      try {
        await fetch('/api/claude-auth/logout', { method: 'POST' });
        showToast('Claude Code logged out', 'success');
        checkClaudeAuth();
      } catch (e) {
        showToast('Logout error: ' + e.message, 'error');
      }
    }


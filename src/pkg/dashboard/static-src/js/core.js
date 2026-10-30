    // Wrap fetch to inject auth token for mutation requests (PUT/POST/PATCH/DELETE)
    (function() {
      // Token acquisition for the self-hosted (token-only) mode. The dashboard
      // token is the API credential here — so it is NEVER fetched from the
      // server (that would let any visitor read it). It is stored in
      // localStorage by the operator pasting it once. On a hosted/hub-proxied
      // hive there is no token at all (identity comes from the hub login), so
      // this path simply never triggers a dialog.
      let _authToken = localStorage.getItem('hive-token') || null;
      let _authConfigured = null; // null=unknown, true/false once checked
      // Set only while re-prompting after the server rejected a stored token,
      // so the prompt can say WHY it is asking again.
      let _authRejected = false;
      // Single-flight: the dashboard fires many reads concurrently at boot, and
      // since the gateway stopped trusting unauthenticated reads (issue #7695)
      // each of them can 401 at once on a token-secured spoke with no stored
      // token. Without this, every one of those failures would open its own
      // the in-app dialog. All concurrent callers now share one dialog.
      let _ensureAuthTokenFlight = null;
      function _ensureAuthToken() {
        if (!_ensureAuthTokenFlight) {
          _ensureAuthTokenFlight = _ensureAuthTokenInner().finally(() => { _ensureAuthTokenFlight = null; });
        }
        return _ensureAuthTokenFlight;
      }
      async function _ensureAuthTokenInner() {
        if (_authToken) return _authToken;
        if (_authConfigured === null) {
          try {
            const r = await _fetchWithSessionGuard('/api/auth/token');
            if (r.ok) { const d = await r.json(); _authConfigured = d.configured === 'true' || d.configured === true; }
            else { _authConfigured = false; } // 404 = hosted hive, no token needed
          } catch { _authConfigured = false; }
        }
        if (_authConfigured && !_authToken) {
          // Never prompt a user who already has an authenticated identity —
          // a hub login or device-flow session authorizes mutations on its
          // own, and hosted operators are never given the shared token.
          try {
            const st = await _fetchWithSessionGuard('/api/gh-user-auth/status');
            if (st.ok) {
              const d = await st.json();
              if (d.logged_in) { _authConfigured = false; return null; }
            }
          } catch {}
          // Self-hosted hive with a token configured but none stored locally:
          // ask the operator to paste it once. It persists in localStorage.
          const entered = await hivePrompt({
            title: 'Dashboard token required',
            label: _authRejected
              ? 'The stored dashboard token was rejected — it is probably stale (rotated since this browser saved it). Paste the current HIVE_DASHBOARD_TOKEN:'
              : 'This hive requires its dashboard token to make changes. Paste HIVE_DASHBOARD_TOKEN:'
          });
          if (entered && entered.trim()) {
            _authToken = entered.trim();
            try { localStorage.setItem('hive-token', _authToken); } catch {}
          }
        }
        return _authToken;
      }
      const _origFetch = window.fetch;

      /* ---- An expired session must be readable, never opaque (hivecommons/hive#7433) ----
         A hosted spoke sits behind the hub's forward-auth gate, and when the
         hub session lapses that gate answers a 302 to the APEX login host.
         Under fetch()'s default redirect:'follow' the browser chases it
         cross-origin, fails the CORS check, and rejects with an
         unclassifiable "TypeError: Load failed" — so every `res.status === 401`
         branch in this file is dead code on that path, and the UI cannot tell
         "you are signed out" from "this hive is unreachable".

         redirect:'manual' removes the guesswork: a redirect becomes an
         `opaqueredirect` response, which IS observable, and a rejection once
         again means only "the request never completed". An authenticated /api
         call has no legitimate reason to redirect, so an opaqueredirect there
         is exactly the expired session — synthesize the 401 the gate should
         have sent and let the existing status branches run.

         The hive-api-xhr Ingress (src/pkg/hub/saas_provision.go, #7405) makes
         the gate answer 401 directly, but it is provisioned per hive: this
         guard is what covers hives that predate it or have not been
         re-provisioned, and any other proxy in front of a self-hosted hive.

         GET /api/gh-user-auth/session is exempt: it is the device-flow
         landing, a real browser navigation whose own 302 back to "/" is its
         success path, not an XHR. */
      const SESSION_REDIRECT_EXEMPT_PATHS = ['/api/gh-user-auth/session'];
      const SESSION_EXPIRED_BODY = JSON.stringify({
        error: 'session_expired',
        detail: 'Your session has expired or you are not signed in to this hive. Sign in again and retry — the request was NOT applied.',
        sign_in: '/'
      });
      function _isGuardedApiRequest(url) {
        let href = '';
        if (typeof url === 'string') href = url;
        else if (url && typeof url.href === 'string') href = url.href;
        else if (url && typeof url.url === 'string') href = url.url;
        else return false;
        let u;
        try { u = new URL(href, window.location.href); } catch (e) { return false; }
        // Cross-origin calls are somebody else's redirect contract, and the
        // gate only guards this hive's own /api surface.
        if (u.origin !== window.location.origin) return false;
        if (u.pathname !== '/api' && u.pathname.indexOf('/api/') !== 0) return false;
        return SESSION_REDIRECT_EXEMPT_PATHS.indexOf(u.pathname) === -1;
      }
      async function _fetchWithSessionGuard(url, opts) {
        if (!_isGuardedApiRequest(url)) return _origFetch(url, opts);
        const res = await _origFetch(url, Object.assign({}, opts || {}, { redirect: 'manual' }));
        if (res.type !== 'opaqueredirect') return res;
        return new Response(SESSION_EXPIRED_BODY, {
          status: 401,
          statusText: 'Unauthorized',
          headers: { 'Content-Type': 'application/json' }
        });
      }

      function _attachAuthHeader(opts) {
        if (!_authToken) return;
        opts.headers = opts.headers || {};
        if (opts.headers instanceof Headers) {
          opts.headers.set('Authorization', 'Bearer ' + _authToken);
        } else {
          opts.headers['Authorization'] = 'Bearer ' + _authToken;
        }
      }
      window.fetch = async function(url, opts) {
        opts = opts || {};
        const method = (opts.method || 'GET').toUpperCase();
        const isMutation = ['PUT','POST','PATCH','DELETE'].includes(method);
        // Same-origin /api requests need the token for READS too, not just
        // mutations: the gateway no longer injects owner-level trust
        // (X-Hive-Internal) on unauthenticated GETs (issue #7695), so on a
        // token-secured spoke an anonymous read is 401'd by the Go API. Attach
        // the stored token to every guarded request; only mutations proactively
        // PROMPT for a missing one (reads prompt lazily via the 401 retry
        // below, so hosted hives — which have no shared token — never see a
        // prompt on load).
        const isGuarded = _isGuardedApiRequest(url);
        if (isMutation) {
          await _ensureAuthToken();
        }
        if (_authToken && (isMutation || isGuarded)) {
          opts.headers = opts.headers || {};
          if (opts.headers instanceof Headers) {
            if (!opts.headers.has('Authorization')) opts.headers.set('Authorization', 'Bearer ' + _authToken);
          } else {
            if (!opts.headers['Authorization']) opts.headers['Authorization'] = 'Bearer ' + _authToken;
          }
        }
        const res = await _fetchWithSessionGuard(url, opts);
        // A STORED token that the server rejects is stale — the operator
        // rotated HIVE_DASHBOARD_TOKEN since this browser saved it. Without
        // this branch that state is permanent and SILENT: the wrapper only
        // ever prompts when it has no token at all, so every mutation 401s
        // forever while each feature reports the failure in its own idiom (a
        // toast, a no-op button, a confirm dialog that closes as if it
        // worked). Measured on a live hive: a browser window kept failing
        // Save / Re-check / terminal auth / pack-apply for a full day after a
        // rotation. Drop the bad token, re-prompt once saying why, retry once.
        //
        // Retry exactly once (__hiveAuthRetried): a genuinely wrong token
        // must not loop. Guarded READS take this path too (issue #7695): with
        // no stored token the first read 401s, _ensureAuthToken prompts once
        // (or resolves silently to null on hosted/direct-route hives, where
        // /api/auth/token answers 404 and clearing/prompting would be wrong —
        // there a 401/403 is a real session/authorization denial), and the
        // retry carries the entered token. A stored-but-stale token is dropped
        // and re-prompted exactly as before.
        if ((isMutation || isGuarded) && !opts.__hiveAuthRetried &&
            (res.status === 401 || res.status === 403)) {
          if (_authToken) {
            try { localStorage.removeItem('hive-token'); } catch {}
            _authToken = null;
            _authRejected = true;
          }
          try {
            await _ensureAuthToken();
          } finally {
            _authRejected = false;
          }
          if (_authToken) {
            const retryOpts = Object.assign({}, opts, { __hiveAuthRetried: true });
            if (opts.headers instanceof Headers) retryOpts.headers = new Headers(opts.headers);
            else if (opts.headers) retryOpts.headers = Object.assign({}, opts.headers);
            _attachAuthHeader(retryOpts);
            return _fetchWithSessionGuard(url, retryOpts);
          }
        }
        return res;
      };
    })();

    (function initBootPerfSummary() {
      var params;
      try { params = new URLSearchParams(window.location.search); } catch (e) { return; }
      if (!params || params.get('perf') !== '1') return;
      var originalFetch = window.fetch;
      var bootFetches = [];
      var bootStartedAt = performance.now();
      window.__hiveBootPerf = bootFetches;
      window.fetch = async function(url, opts) {
        var started = performance.now();
        var path = '';
        try { path = new URL((url && (url.url || url.href)) || url, window.location.href).pathname; } catch (e) { path = String(url || ''); }
        try {
          var res = await originalFetch(url, opts);
          bootFetches.push({ path: path, status: res.status, ms: Math.round(performance.now() - started) });
          return res;
        } catch (e) {
          bootFetches.push({ path: path, status: 'error', ms: Math.round(performance.now() - started) });
          throw e;
        }
      };
      window.addEventListener('load', function() {
        setTimeout(function() {
          var rows = bootFetches.slice().sort(function(a, b) { return b.ms - a.ms; });
          var total = Math.round(performance.now() - bootStartedAt);
          console.log('[hive perf] boot fetch summary after ' + total + 'ms; ' + rows.length + ' fetches');
          if (console.table) console.table(rows);
          else console.log(rows);
        }, 0);
      });
    })();



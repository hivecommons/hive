    // ── NPS feedback survey, #9610 ───────────────────────────────────
    // An occasional "how is the hive working for you?" card with a 4-point
    // emoji scale, a category-specific follow-up and a dismiss. Timing and
    // backoff are identical to kubestellar/console's useNPSSurvey.ts (see
    // npsTiming). Shown only when GET /api/feedback/nps/status says
    // can_submit: the hive opted in (hosted spokes default on), it has a hub
    // link, and the viewer is signed in with write access. The response goes
    // to this spoke's backend, which forwards it to the hub without the
    // user's identity. Per-browser state lives in localStorage; every storage
    // access is wrapped so a private window or blocked storage just means no
    // prompt. All user-visible text is set with textContent, never a markup sink.
    //
    // The eligibility rules are pure functions of (state, now, timing) so
    // nps_ui_test.go can execute them under node without a browser.
    //
    // Operator options arrive in the same status response: timing overrides
    // (hub.nps_timing), an optional GA4 measurement ID (off unless the
    // operator sets one; gtag.js is loaded only then, only once the card is
    // about to show, and never for a viewer who opted out; events carry
    // score, category and feedback LENGTH, never the text), and the opt-in
    // detractor public issue (score 1, explicit consent, minimum length).

    const NPS_STATE_LS_KEY = 'hive-nps-state-v1';
    const NPS_SESSION_COUNT_LS_KEY = 'hive-nps-session-count-v1';
    const NPS_SESSION_MARK_SS_KEY = 'hive-nps-session-counted-v1';
    const NPS_ENGAGED_MS_SS_KEY = 'hive-nps-engaged-ms-v1';
    // Per-browser analytics opt-out. '1' means never send GA4 events.
    const NPS_ANALYTICS_OPT_OUT_LS_KEY = 'hive-analytics-opt-out-v1';
    // Shape of a GA4 measurement ID; the server applies the same pattern.
    const NPS_GA4_ID_RE = /^G-[A-Z0-9]{4,20}$/;
    const NPS_GTAG_SRC = 'https://www.googletagmanager.com/gtag/js?id=';
    // How long the thank-you card stays up after a submission.
    const NPS_THANK_YOU_DISPLAY_MS = 2000;

    // npsTiming returns the timing/backoff rules, mirroring the console's
    // useNPSSurvey.ts constants one for one, with any operator overrides
    // (hub.nps_timing, from the status response's timing object) applied.
    // An override must be a positive integer; anything else keeps the constant.
    function npsTiming(overrides) {
      const MS_PER_SECOND = 1000;
      const MS_PER_MINUTE = 60 * MS_PER_SECOND;
      const MS_PER_DAY = 24 * 60 * MS_PER_MINUTE;
      const t = {
        MS_PER_DAY: MS_PER_DAY,
        // Never before the 2nd session.
        MIN_SESSIONS_BEFORE_NPS: 2,
        // From the 3rd session on, ask sooner.
        RETURNING_USER_SESSION_THRESHOLD: 3,
        // Engaged time required in the 2nd session.
        SECOND_SESSION_MIN_ENGAGEMENT_MS: 5 * MS_PER_MINUTE,
        // Engaged time required from the 3rd session on.
        RETURNING_USER_MIN_ENGAGEMENT_MS: 1 * MS_PER_MINUTE,
        // How often engagement is re-checked while waiting.
        ENGAGEMENT_POLL_MS: 15 * MS_PER_SECOND,
        // Time without input after which the page stops counting as engaged.
        ENGAGEMENT_IDLE_MS: 1 * MS_PER_MINUTE,
        // Days to wait after a submission before asking again.
        REPROMPT_DAYS: 30,
        // Days to wait after a dismissal.
        DISMISS_RETRY_DAYS: 7,
        // Dismissals after which the wait becomes REPROMPT_DAYS.
        MAX_DISMISSALS: 3,
        // Bound on the submit round-trip (the spoke then calls the hub).
        POST_TIMEOUT_MS: 10 * MS_PER_SECOND,
      };
      const o = overrides && typeof overrides === 'object' ? overrides : {};
      const pick = (v, fallback) => (Number.isInteger(v) && v > 0 ? v : fallback);
      t.MIN_SESSIONS_BEFORE_NPS = pick(o.min_sessions, t.MIN_SESSIONS_BEFORE_NPS);
      t.SECOND_SESSION_MIN_ENGAGEMENT_MS = pick(o.second_session_min_engagement_ms, t.SECOND_SESSION_MIN_ENGAGEMENT_MS);
      t.RETURNING_USER_MIN_ENGAGEMENT_MS = pick(o.returning_min_engagement_ms, t.RETURNING_USER_MIN_ENGAGEMENT_MS);
      t.REPROMPT_DAYS = pick(o.reprompt_days, t.REPROMPT_DAYS);
      t.DISMISS_RETRY_DAYS = pick(o.dismiss_retry_days, t.DISMISS_RETRY_DAYS);
      t.MAX_DISMISSALS = pick(o.max_dismissals, t.MAX_DISMISSALS);
      return t;
    }

    function npsDefaultState() {
      return { lastSubmittedAt: null, lastDismissedAt: null, snoozedUntil: null, dismissCount: 0, maxDismissalsReachedAt: null };
    }

    // npsNormalizeState accepts whatever was in storage and returns a
    // well-typed state, so a hand-edited or corrupt blob cannot crash the
    // prompt or wedge it permanently on or off.
    function npsNormalizeState(raw) {
      const s = npsDefaultState();
      if (!raw || typeof raw !== 'object') return s;
      for (const k of ['lastSubmittedAt', 'lastDismissedAt', 'snoozedUntil', 'maxDismissalsReachedAt']) {
        if (typeof raw[k] === 'string') s[k] = raw[k];
      }
      const n = Number(raw.dismissCount);
      s.dismissCount = Number.isFinite(n) && n > 0 ? Math.floor(n) : 0;
      return s;
    }

    // npsDaysSince: Infinity for never, 0 for an unparseable date (treated as
    // "just now", which errs on the side of NOT prompting).
    function npsDaysSince(iso, nowMs, msPerDay) {
      if (!iso) return Infinity;
      const ts = new Date(iso).getTime();
      if (!Number.isFinite(ts)) return 0;
      return (nowMs - ts) / msPerDay;
    }

    function npsIsEligible(state, nowMs, t) {
      const s = npsNormalizeState(state);
      if (npsDaysSince(s.lastSubmittedAt, nowMs, t.MS_PER_DAY) < t.REPROMPT_DAYS) return false;
      if (s.dismissCount >= t.MAX_DISMISSALS &&
          npsDaysSince(s.maxDismissalsReachedAt, nowMs, t.MS_PER_DAY) < t.REPROMPT_DAYS) return false;
      const snoozedUntil = s.snoozedUntil ? new Date(s.snoozedUntil).getTime() : 0;
      if (Number.isFinite(snoozedUntil) && snoozedUntil > nowMs) return false;
      if (npsDaysSince(s.lastDismissedAt, nowMs, t.MS_PER_DAY) < t.DISMISS_RETRY_DAYS) return false;
      return true;
    }

    function npsRequiredEngagementMs(sessionCount, t) {
      return sessionCount >= t.RETURNING_USER_SESSION_THRESHOLD
        ? t.RETURNING_USER_MIN_ENGAGEMENT_MS
        : t.SECOND_SESSION_MIN_ENGAGEMENT_MS;
    }

    function npsHasEnoughUsage(sessionCount, engagedMs, t) {
      if (sessionCount < t.MIN_SESSIONS_BEFORE_NPS) return false;
      return engagedMs >= npsRequiredEngagementMs(sessionCount, t);
    }

    function npsStateAfterSubmit(nowMs) {
      const s = npsDefaultState();
      s.lastSubmittedAt = new Date(nowMs).toISOString();
      return s;
    }

    function npsStateAfterDismiss(state, nowMs, t) {
      const s = npsNormalizeState(state);
      const count = s.dismissCount + 1;
      const at = new Date(nowMs).toISOString();
      return {
        lastSubmittedAt: s.lastSubmittedAt,
        lastDismissedAt: at,
        snoozedUntil: new Date(nowMs + t.DISMISS_RETRY_DAYS * t.MS_PER_DAY).toISOString(),
        dismissCount: count,
        maxDismissalsReachedAt: count >= t.MAX_DISMISSALS ? at : s.maxDismissalsReachedAt,
      };
    }

    // npsCategory buckets the raw score the same way the hub does.
    function npsCategory(score) {
      if (score >= 4) return 'promoter';
      if (score >= 2) return 'passive';
      return 'detractor';
    }

    function npsFeedbackPrompt(category) {
      if (category === 'detractor') return 'Sorry to hear that. What is not working for you? (optional)';
      if (category === 'promoter') return 'Glad to hear it! What do you like most? (optional)';
      return 'What would make the hive better for you? (optional)';
    }

    // npsGA4MeasurementID returns the status response's GA4 ID when it is
    // well-formed, else '' (GA4 off).
    function npsGA4MeasurementID(status) {
      const id = status && typeof status.ga4_measurement_id === 'string' ? status.ga4_measurement_id : '';
      return NPS_GA4_ID_RE.test(id) ? id : '';
    }

    // npsAnalyticsParams is the ONLY payload builder for the hive_nps_response
    // event. It takes the feedback text solely to measure it: the result holds
    // the score, its category and the text's length, never the text.
    function npsAnalyticsParams(score, feedbackText) {
      const params = {};
      if (Number.isInteger(score)) {
        params.score = score;
        params.category = npsCategory(score);
      }
      params.feedback_length = typeof feedbackText === 'string' ? feedbackText.trim().length : 0;
      return params;
    }

    // npsDetractorIssueAllowed: the hive opted in (status.detractor_issue)
    // and the chosen score is the one allowed to open a public issue.
    function npsDetractorIssueAllowed(status, score) {
      const d = status && status.detractor_issue;
      return !!(d && d.enabled === true && Number.isInteger(d.score) && score === d.score);
    }

    // npsIssueRequestBody builds the POST /api/feedback/nps/issue body, or
    // returns null unless every precondition holds: opted in, detractor
    // score, the consent box ticked, and at least min_feedback_chars of text.
    function npsIssueRequestBody(status, score, feedbackText, consent) {
      if (consent !== true || !npsDetractorIssueAllowed(status, score)) return null;
      const text = typeof feedbackText === 'string' ? feedbackText.trim() : '';
      const min = Number(status.detractor_issue.min_feedback_chars) || 0;
      if (text.length < min) return null;
      return { score: score, feedback: text, consent: true };
    }

    const NPS_OPTIONS = [
      { score: 1, emoji: '\u{1F620}', label: 'Not great' },
      { score: 2, emoji: '\u{1F610}', label: 'Meh' },
      { score: 3, emoji: '\u{1F642}', label: 'Good' },
      { score: 4, emoji: '\u{1F60D}', label: 'Love it' },
    ];

    function npsLoadState() {
      try {
        const raw = localStorage.getItem(NPS_STATE_LS_KEY);
        return npsNormalizeState(raw ? JSON.parse(raw) : null);
      } catch (e) { return null; /* storage blocked or corrupt: no prompt */ }
    }
    function npsSaveState(state) {
      try { localStorage.setItem(NPS_STATE_LS_KEY, JSON.stringify(state)); } catch (e) { /* storage blocked - non-fatal */ }
    }
    // npsBumpSessionCount counts this browser tab session once (sessionStorage
    // marker) and returns the running total, or 0 when storage is blocked.
    function npsBumpSessionCount() {
      try {
        let n = parseInt(localStorage.getItem(NPS_SESSION_COUNT_LS_KEY) || '0', 10);
        if (!Number.isFinite(n) || n < 0) n = 0;
        if (!sessionStorage.getItem(NPS_SESSION_MARK_SS_KEY)) {
          n += 1;
          localStorage.setItem(NPS_SESSION_COUNT_LS_KEY, String(n));
          sessionStorage.setItem(NPS_SESSION_MARK_SS_KEY, '1');
        }
        return n;
      } catch (e) { return 0; }
    }
    function npsEngagedMs() {
      try {
        const n = Number(sessionStorage.getItem(NPS_ENGAGED_MS_SS_KEY) || '0');
        return Number.isFinite(n) && n > 0 ? n : 0;
      } catch (e) { return 0; }
    }
    function npsAddEngagedMs(delta) {
      try { sessionStorage.setItem(NPS_ENGAGED_MS_SS_KEY, String(npsEngagedMs() + delta)); } catch (e) { /* non-fatal */ }
    }
    // npsAnalyticsOptedOut: the browser's Global Privacy Control or Do Not
    // Track signal, or this dashboard's own per-browser opt-out. Unreadable
    // storage counts as opted out (fail closed).
    function npsAnalyticsOptedOut() {
      try {
        if (typeof navigator !== 'undefined' && (navigator.globalPrivacyControl === true || navigator.doNotTrack === '1')) return true;
        return localStorage.getItem(NPS_ANALYTICS_OPT_OUT_LS_KEY) === '1';
      } catch (e) { return true; }
    }
    function npsSetAnalyticsOptOut(optOut) {
      try {
        if (optOut) localStorage.setItem(NPS_ANALYTICS_OPT_OUT_LS_KEY, '1');
        else localStorage.removeItem(NPS_ANALYTICS_OPT_OUT_LS_KEY);
      } catch (e) { /* storage blocked - npsAnalyticsOptedOut already fails closed */ }
    }

    let _npsGtagId = '';
    // npsEnsureGtag loads gtag.js once, for a configured ID only. Automatic
    // page views, Google signals and ad personalization are all off: the only
    // hits are the hive_nps_* events npsTrack sends.
    function npsEnsureGtag(id) {
      if (_npsGtagId) return;
      _npsGtagId = id;
      window.dataLayer = window.dataLayer || [];
      window.gtag = function () { window.dataLayer.push(arguments); };
      window.gtag('js', new Date());
      window.gtag('config', id, { send_page_view: false, allow_google_signals: false, allow_ad_personalization_signals: false });
      const el = document.createElement('script');
      el.async = true;
      el.src = NPS_GTAG_SRC + encodeURIComponent(id);
      document.head.appendChild(el);
    }
    // npsTrack sends one hive_nps_* event, or does nothing (returns false)
    // when GA4 is not configured or the viewer opted out.
    function npsTrack(status, name, params) {
      const id = npsGA4MeasurementID(status);
      if (!id || npsAnalyticsOptedOut()) return false;
      npsEnsureGtag(id);
      window.gtag('event', name, params);
      return true;
    }

    let _npsShownThisLoad = false;
    let _npsCard = null;
    let _npsKeyHandler = null;
    // The status response and effective timing for this page load.
    let _npsStatus = null;
    let _npsTimingActive = null;
    function npsActiveTiming() { return _npsTimingActive || npsTiming(); }

    // initNPSSurvey decides whether this page load may show the prompt and,
    // if so, waits for enough engaged time. At most once per page load.
    // The status is fetched first because it carries the operator's timing
    // overrides, which decide the session and backoff checks below.
    async function initNPSSurvey() {
      const sessionCount = npsBumpSessionCount();
      if (sessionCount < 1) return; // storage blocked: no prompt
      let status = null;
      try {
        const resp = await fetch('/api/feedback/nps/status');
        if (!resp.ok) return;
        status = await resp.json();
      } catch (e) { return; }
      if (!status || status.can_submit !== true) return;
      const t = npsTiming(status.timing);
      _npsStatus = status;
      _npsTimingActive = t;
      if (sessionCount < t.MIN_SESSIONS_BEFORE_NPS) return;
      const state = npsLoadState();
      if (!state || !npsIsEligible(state, Date.now(), t)) return;

      let lastInput = Date.now();
      let lastTick = Date.now();
      const markInput = () => { lastInput = Date.now(); };
      for (const ev of ['pointerdown', 'keydown', 'scroll', 'mousemove', 'touchstart']) {
        window.addEventListener(ev, markInput, { passive: true });
      }
      const timer = setInterval(() => {
        const now = Date.now();
        if (document.visibilityState === 'visible' && now - lastInput < t.ENGAGEMENT_IDLE_MS) {
          npsAddEngagedMs(now - lastTick);
        }
        lastTick = now;
        if (npsHasEnoughUsage(sessionCount, npsEngagedMs(), t)) {
          clearInterval(timer);
          for (const ev of ['pointerdown', 'keydown', 'scroll', 'mousemove', 'touchstart']) {
            window.removeEventListener(ev, markInput);
          }
          showNPSSurvey(status);
        }
      }, t.ENGAGEMENT_POLL_MS);
    }

    function npsCloseCard() {
      if (_npsKeyHandler) document.removeEventListener('keydown', _npsKeyHandler);
      _npsKeyHandler = null;
      if (_npsCard && _npsCard.parentNode) _npsCard.parentNode.removeChild(_npsCard);
      _npsCard = null;
    }

    function dismissNPSSurvey() {
      const t = npsActiveTiming();
      npsSaveState(npsStateAfterDismiss(npsLoadState() || npsDefaultState(), Date.now(), t));
      npsTrack(_npsStatus, 'hive_nps_dismissed', {});
      npsCloseCard();
    }

    // npsOpenIssue files the detractor's public issue after their response
    // was accepted. Failure is reported but never undoes the response.
    async function npsOpenIssue(body) {
      try {
        const resp = await fetch('/api/feedback/nps/issue', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(body),
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) throw new Error(String((data && data.error) || ('HTTP ' + resp.status)));
        showToast('Opened public issue #' + String(Number(data && data.number) || '') + ' - thank you', 'success');
      } catch (e) {
        showToast('Could not open the public issue: ' + (e && e.message ? e.message : 'network error'), 'error');
      }
    }

    function npsText(tag, cls, text) {
      const el = document.createElement(tag);
      if (cls) el.className = cls;
      if (text !== undefined && text !== null) el.textContent = String(text);
      return el;
    }

    function showNPSSurvey(status) {
      if (_npsShownThisLoad || _npsCard) return;
      if (window.hiveKioskMode && window.hiveKioskMode()) return;
      _npsShownThisLoad = true;
      const maxChars = Number(status && status.max_feedback_chars) || 0;
      let selected = null;

      const card = npsText('div', 'nps-card');
      card.id = 'nps-survey-card';
      card.setAttribute('role', 'dialog');
      card.setAttribute('aria-label', 'Feedback');
      const head = npsText('div', 'nps-card-head');
      head.appendChild(npsText('div', 'nps-card-title', 'How is the hive working for you?'));
      const close = npsText('button', 'hv-btn btn-ghost btn-icon btn-sm nps-card-close', '×');
      close.type = 'button';
      close.setAttribute('aria-label', 'Dismiss');
      close.addEventListener('click', dismissNPSSurvey);
      head.appendChild(close);
      card.appendChild(head);

      const scale = npsText('div', 'nps-card-scale');
      const follow = npsText('div', 'nps-card-follow');
      follow.hidden = true;
      const prompt = npsText('label', 'nps-card-prompt');
      prompt.setAttribute('for', 'nps-card-feedback');
      const text = document.createElement('textarea');
      text.id = 'nps-card-feedback';
      text.className = 'nps-card-text';
      text.rows = 3;
      if (maxChars > 0) text.maxLength = maxChars;
      const submit = npsText('button', 'hv-btn btn-primary btn-sm', 'Send');
      submit.type = 'button';
      follow.appendChild(prompt);
      follow.appendChild(text);
      // Opt-in detractor public issue (hub.nps_detractor_issues): a consent
      // checkbox, unticked by default, shown only for the detractor score.
      const consentRow = npsText('label', 'nps-card-consent');
      consentRow.hidden = true;
      const consent = document.createElement('input');
      consent.type = 'checkbox';
      consentRow.appendChild(consent);
      if (status && status.detractor_issue && status.detractor_issue.enabled === true) {
        const where = typeof status.detractor_issue.repo === 'string' && status.detractor_issue.repo
          ? ' in ' + status.detractor_issue.repo : '';
        consentRow.appendChild(npsText('span', '',
          'Also open a public issue' + where + ' with this feedback. Anyone can read it; your name is not included.'));
        follow.appendChild(consentRow);
      }
      follow.appendChild(submit);

      for (const opt of NPS_OPTIONS) {
        const b = npsText('button', 'nps-card-option');
        b.type = 'button';
        b.setAttribute('aria-label', opt.label);
        b.setAttribute('aria-pressed', 'false');
        b.appendChild(npsText('span', 'nps-card-emoji', opt.emoji));
        b.appendChild(npsText('span', 'nps-card-option-label', opt.label));
        b.addEventListener('click', () => {
          selected = opt.score;
          for (const other of scale.children) other.setAttribute('aria-pressed', 'false');
          b.setAttribute('aria-pressed', 'true');
          prompt.textContent = npsFeedbackPrompt(npsCategory(opt.score));
          consentRow.hidden = !npsDetractorIssueAllowed(status, opt.score);
          if (consentRow.hidden) consent.checked = false;
          follow.hidden = false;
        });
        scale.appendChild(b);
      }
      card.appendChild(scale);
      card.appendChild(follow);
      if (npsGA4MeasurementID(status)) card.appendChild(npsAnalyticsNotice());

      submit.addEventListener('click', async () => {
        if (selected === null) return;
        const feedbackText = text.value.trim();
        const wantIssue = consent.checked && npsDetractorIssueAllowed(status, selected);
        const issueBody = wantIssue ? npsIssueRequestBody(status, selected, feedbackText, true) : null;
        if (wantIssue && !issueBody) {
          const min = Number(status.detractor_issue.min_feedback_chars) || 0;
          showToast('Please write at least ' + min + ' characters to open a public issue, or untick the box.', 'error');
          return;
        }
        submit.disabled = true;
        const t = npsActiveTiming();
        try {
          const ctrl = new AbortController();
          const timeout = setTimeout(() => ctrl.abort(), t.POST_TIMEOUT_MS);
          const resp = await fetch('/api/feedback/nps', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ score: selected, feedback: feedbackText || undefined }),
            signal: ctrl.signal,
          });
          clearTimeout(timeout);
          const data = await resp.json().catch(() => ({}));
          if (resp.status === 429) {
            // Already answered (or the hive's daily share is used): treat as
            // answered here too, so this browser backs off instead of nagging.
            npsSaveState(npsStateAfterSubmit(Date.now()));
            npsCloseCard();
            showToast(String((data && data.error) || 'Feedback already sent recently - thank you!'), 'info');
            return;
          }
          if (!resp.ok) throw new Error(String((data && data.error) || ('HTTP ' + resp.status)));
          npsSaveState(npsStateAfterSubmit(Date.now()));
          npsTrack(status, 'hive_nps_response', npsAnalyticsParams(selected, feedbackText));
          if (issueBody) npsOpenIssue(issueBody);
          while (card.firstChild) card.removeChild(card.firstChild);
          card.appendChild(npsText('div', 'nps-card-title', 'Thank you!'));
          card.appendChild(npsText('div', 'nps-card-prompt', 'Your feedback helps shape the hive.'));
      const help = npsText('a', 'nps-card-prompt nps-card-discord', 'Need help? Join our Discord');
      help.href = 'https://hivecommons.dev/discord';
      help.target = '_blank';
      help.rel = 'noopener';
      card.appendChild(help);
          if (_npsKeyHandler) document.removeEventListener('keydown', _npsKeyHandler);
          _npsKeyHandler = null;
          setTimeout(npsCloseCard, NPS_THANK_YOU_DISPLAY_MS);
        } catch (e) {
          // Keep the card open so the user can retry; never claim success.
          submit.disabled = false;
          showToast('Could not send feedback: ' + (e && e.message ? e.message : 'network error'), 'error');
        }
      });

      _npsCard = card;
      document.body.appendChild(card);
      npsTrack(status, 'hive_nps_survey_shown', {});
    }

    // npsAnalyticsNotice says, on the card itself, that this prompt sends
    // anonymous usage events, and lets the viewer turn them off (or back on)
    // for this browser.
    function npsAnalyticsNotice() {
      const row = npsText('div', 'nps-card-analytics');
      const label = npsText('span', '');
      const toggle = npsText('button', 'hv-btn btn-ghost btn-sm');
      toggle.type = 'button';
      const render = () => {
        const off = npsAnalyticsOptedOut();
        label.textContent = off
          ? 'Usage analytics are off in this browser.'
          : 'This prompt sends anonymous usage events (never your text) to Google Analytics.';
        toggle.textContent = off ? 'Turn on' : 'Turn off';
      };
      toggle.addEventListener('click', () => {
        npsSetAnalyticsOptOut(!npsAnalyticsOptedOut());
        render();
      });
      render();
      row.appendChild(label);
      row.appendChild(toggle);
      return row;
    }


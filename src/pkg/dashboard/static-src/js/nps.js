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

    const NPS_STATE_LS_KEY = 'hive-nps-state-v1';
    const NPS_SESSION_COUNT_LS_KEY = 'hive-nps-session-count-v1';
    const NPS_SESSION_MARK_SS_KEY = 'hive-nps-session-counted-v1';
    const NPS_ENGAGED_MS_SS_KEY = 'hive-nps-engaged-ms-v1';
    // How long the thank-you card stays up after a submission.
    const NPS_THANK_YOU_DISPLAY_MS = 2000;

    // npsTiming returns the timing/backoff rules, mirroring the console's
    // useNPSSurvey.ts constants one for one.
    function npsTiming() {
      const MS_PER_SECOND = 1000;
      const MS_PER_MINUTE = 60 * MS_PER_SECOND;
      const MS_PER_DAY = 24 * 60 * MS_PER_MINUTE;
      return {
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

    let _npsShownThisLoad = false;
    let _npsCard = null;
    let _npsKeyHandler = null;

    // initNPSSurvey decides whether this page load may show the prompt and,
    // if so, waits for enough engaged time. At most once per page load.
    async function initNPSSurvey() {
      const t = npsTiming();
      const sessionCount = npsBumpSessionCount();
      if (sessionCount < t.MIN_SESSIONS_BEFORE_NPS) return;
      const state = npsLoadState();
      if (!state || !npsIsEligible(state, Date.now(), t)) return;
      let status = null;
      try {
        const resp = await fetch('/api/feedback/nps/status');
        if (!resp.ok) return;
        status = await resp.json();
      } catch (e) { return; }
      if (!status || status.can_submit !== true) return;

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
      const t = npsTiming();
      npsSaveState(npsStateAfterDismiss(npsLoadState() || npsDefaultState(), Date.now(), t));
      npsCloseCard();
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
      if (window.hivePopoutMode && window.hivePopoutMode()) return;
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
          follow.hidden = false;
        });
        scale.appendChild(b);
      }
      card.appendChild(scale);
      card.appendChild(follow);

      submit.addEventListener('click', async () => {
        if (selected === null) return;
        submit.disabled = true;
        const t = npsTiming();
        try {
          const ctrl = new AbortController();
          const timeout = setTimeout(() => ctrl.abort(), t.POST_TIMEOUT_MS);
          const resp = await fetch('/api/feedback/nps', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ score: selected, feedback: text.value.trim() || undefined }),
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
    }


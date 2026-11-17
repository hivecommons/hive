    // ── Inbox: recent mail (hivecommons/hive#18699) ──
    // What was mailed to this hive lately, accepted or refused, and why a
    // refused mail was turned away: GET /api/inbox/email/recent. A mail that
    // never showed up in the Inbox is explained here. No subject, file name
    // or full sender address is ever listed, only the sender's domain.
    // Self-contained: it renders into #inbox-mail-log and nothing else.
    const INBOX_MAIL_LOG_FIRST_LOAD_MS = 6000;
    // A reload is never more frequent than this, whatever asks for it.
    const INBOX_MAIL_LOG_MIN_RELOAD_MS = 30000;
    const INBOX_MAIL_LOG_BYTES_PER_KB = 1024;
    const INBOX_MAIL_LOG_BYTES_PER_MB = 1048576;
    const INBOX_MAIL_LOG_REASONS = {
      empty_message: 'the message had no text, calendar invite or attachment',
      unsupported_attachment: 'attachment type not accepted',
      attachment_content_mismatch: 'an attachment was damaged, macro-enabled or not what its extension says',
      attachment_too_large: 'an attachment was over the size limit',
      too_many_attachments: 'too many attachments',
      message_too_large: 'the message was over the size limit',
      malformed_message: 'the message could not be parsed',
      hive_daily_volume: 'this hive reached its daily volume of mail (the sender retries)',
      hive_refused: 'this hive refused the message',
      mail_in_off: 'mail-in files are turned off on this hive',
      hive_storage_full: 'this hive\'s mail storage is full',
    };
    const INBOX_MAIL_LOG_SKIPS = { type: 'type not accepted', content: 'content does not match', size: 'too large', count: 'over the attachment count' };
    let _inboxMailLog = null; // { events, files_enabled }
    let _inboxMailLogOpen = false;
    let _inboxMailLogLoadedAt = 0;

    function inboxMailLogCanView() { return window._hiveRole === 'owner' || window._hiveRole === 'read-write'; }
    function inboxMailLogSize(bytes) {
      const n = Number(bytes) || 0;
      if (n <= 0) return '';
      if (n >= INBOX_MAIL_LOG_BYTES_PER_MB) return (n / INBOX_MAIL_LOG_BYTES_PER_MB).toFixed(1) + ' MB';
      return Math.max(1, Math.round(n / INBOX_MAIL_LOG_BYTES_PER_KB)) + ' KB';
    }
    // Why DMARC came out as it did: the SPF and DKIM results and the server
    // that handed the mail over.
    function inboxMailLogAuth(a) {
      if (!a) return '';
      const dkim = (a.dkim || []).length ? 'DKIM ' + a.dkim.join(', ') : 'no DKIM signature';
      const via = a.helo || a.remote_ip ? 'sent through ' + [a.helo, a.remote_ip].filter(Boolean).join(' ') : '';
      return ['DMARC ' + (a.dmarc || 'none'), 'SPF ' + (a.spf || 'none') + (a.spf_domain ? ' for ' + a.spf_domain : ''), dkim, via].filter(Boolean).join('; ');
    }
    function inboxMailLogApproval(ev) {
      if (ev.outcome !== 'awaiting_approval') return '';
      if (ev.approval_reason === 'sender_not_authenticated') {
        return 'the sender could not be authenticated, so the owner must approve it (' + inboxMailLogAuth(ev.auth) + ')';
      }
      return 'the sender is authenticated but is not a proven calendar email of a user of this hive (Settings → Meeting attendee → Email)';
    }
    function inboxMailLogReason(ev) {
      const skipped = (ev.skipped || []).map(s => (s.ext ? '.' + s.ext : 'no extension') + ' (' + (INBOX_MAIL_LOG_SKIPS[s.reason] || s.reason || 'left out') + ')');
      const why = ev.reason ? (INBOX_MAIL_LOG_REASONS[ev.reason] || ev.reason) : '';
      const left = skipped.length ? (ev.outcome === 'rejected' ? '' : 'left out: ') + skipped.join(', ') : '';
      return [[why, left].filter(Boolean).join(': '), inboxMailLogApproval(ev)].filter(Boolean).join(' · ');
    }
    function inboxMailLogOutcome(ev) {
      if (ev.outcome === 'rejected') return { status: 'error', label: 'refused' };
      if (ev.outcome === 'awaiting_approval') return { status: 'warn', label: 'awaiting approval' };
      return { status: 'ok', label: 'received' };
    }
    function inboxMailLogWhen(ev) {
      return typeof relativeTimeShort === 'function' ? relativeTimeShort(ev.at) : String(ev.at || '');
    }
    function inboxMailLogRow(ev) {
      const o = inboxMailLogOutcome(ev);
      const files = Number(ev.files) || 0;
      const parts = [
        escapeHtml(inboxMailLogWhen(ev)),
        '<span class="kc-pill" data-status="' + o.status + '">' + o.label + '</span>',
        ev.sender_domain ? 'from ' + escapeHtml(ev.sender_domain) : '',
        ev.kind ? escapeHtml(ev.kind) : '',
        files ? files + (files === 1 ? ' file' : ' files') : '',
        escapeHtml(inboxMailLogSize(ev.bytes)),
        escapeHtml(inboxMailLogReason(ev)),
      ].filter(Boolean);
      return '<li class="muted-xs">' + parts.join(' · ') + '</li>';
    }
    function inboxMailLogHtml(data) {
      const events = (data && Array.isArray(data.events)) ? data.events : [];
      if (!events.length) return '';
      const refused = events.filter(e => e.outcome === 'rejected').length;
      const received = events.length - refused;
      const latest = events[0];
      const latestWhy = inboxMailLogReason(latest);
      const summary = '✉️ Recent mail: ' + received + ' received, ' + refused + ' refused · latest ' + escapeHtml(inboxMailLogWhen(latest)) + ': ' +
        inboxMailLogOutcome(latest).label + (latestWhy ? ', ' + escapeHtml(latestWhy) : '');
      const off = data.files_enabled === false ? '<div class="muted-xs">Mail-in files are turned off on this hive: attachments mailed to it are refused.</div>' : '';
      return '<details class="inbox-mail-log"' + (_inboxMailLogOpen ? ' open' : '') + '><summary class="muted-xs">' + summary + '</summary>' + off +
        '<ul class="inbox-mail-log-list">' + events.map(inboxMailLogRow).join('') + '</ul>' +
        '<div class="muted-xs">Only the sender\'s domain, the size and the reason are kept; a refused mail also bounces to its sender with the reason.</div></details>';
    }
    function renderInboxMailLog() {
      const el = document.getElementById('inbox-mail-log');
      if (!el) return;
      const html = inboxMailLogCanView() ? inboxMailLogHtml(_inboxMailLog) : '';
      el.hidden = !html;
      el.innerHTML = html;
      const details = el.querySelector('details');
      // Opening the list reloads it (no timer of its own: the list also
      // reloads when the tab comes back to the foreground).
      if (details) details.addEventListener('toggle', () => { _inboxMailLogOpen = details.open; if (details.open) fetchInboxMailLog(); });
    }
    async function fetchInboxMailLog() {
      if (!inboxMailLogCanView()) return;
      if (Date.now() - _inboxMailLogLoadedAt < INBOX_MAIL_LOG_MIN_RELOAD_MS) return;
      _inboxMailLogLoadedAt = Date.now();
      try {
        const r = await fetch('/api/inbox/email/recent');
        if (!r.ok) return;
        _inboxMailLog = await r.json();
        renderInboxMailLog();
      } catch (e) { /* the list is a diagnostic aid: keep the last one */ }
    }
    if (typeof window !== 'undefined' && typeof document !== 'undefined') {
      setTimeout(fetchInboxMailLog, INBOX_MAIL_LOG_FIRST_LOAD_MS);
      document.addEventListener('visibilitychange', () => { if (!document.hidden) fetchInboxMailLog(); });
    }

    // ── Email-in address row: Inbox, Calendar, Meetings (hivecommons/hive#18693) ──
    // Each hive has one address, hive@<dashboard host>: mailed files and text
    // become Inbox items, calendar invites become Calendar entries that hive
    // joins. Each section's own state endpoint carries it as `email_in`
    // ({address, ready, reason, files_enabled, invites_enabled, docs_url}),
    // for owner and read-write viewers; this file renders the one shared row.
    const EMAIL_IN_DOCS_URL = 'https://github.com/hivecommons/hive/blob/v5/src/docs/meetings.md#is-the-address-set-up';
    // Per section: the label, the one-line hint, and which switch must be on.
    const EMAIL_IN_KINDS = {
      inbox: { label: 'Send to this inbox', needs: 'files_enabled', off: 'Mailed-in files are turned off on this hive',
        hint: 'Email or forward a recording, document or note to this address and it lands here. Mail from a sender this hive does not know waits for the owner\'s approval.' },
      calendar: { label: 'Invite by email', needs: 'invites_enabled', off: 'Email invites are turned off on this hive',
        hint: 'Add this address to a calendar invite and hive joins the meeting. An invite from a sender this hive does not know waits for the owner\'s approval.' },
      meetings: { label: 'Invite by email', needs: 'invites_enabled', off: 'Email invites are turned off on this hive',
        hint: 'Add this address to a calendar invite and hive joins the meeting, or paste a link below.' }
    };
    const EMAIL_IN_REASONS = {
      no_relay: 'this hive is not connected to a hub that relays mail to it',
      no_host: 'this hive\'s public address is not known'
    };
    // The Meetings section's copy of the block (GET /api/meetings).
    let _meetingsEmailIn = null;

    // emailInValid: one local part, one "@", a host; no spaces or controls.
    function emailInValid(address) {
      return typeof address === 'string' && /^[^\s@<>"',;:\\]+@[^\s@<>"',;:\\]+$/.test(address);
    }
    // emailInMailto is the mailto: link of an address. The local part and
    // the host are percent-encoded on their own so the "@" between them
    // stays literal (RFC 6068). No subject is prefilled: the subject of a
    // mail becomes the Inbox item's title.
    function emailInMailto(address) {
      const at = String(address).lastIndexOf('@');
      return 'mailto:' + encodeURIComponent(String(address).slice(0, at)) + '@' + encodeURIComponent(String(address).slice(at + 1));
    }
    // emailInRowHtml renders the row for one section ('inbox', 'calendar' or
    // 'meetings'). It returns '' when the viewer may not see the address
    // (no email_in block), and a "not set up yet" line with the docs link
    // when mail cannot reach this hive.
    function emailInRowHtml(kind, view) {
      const k = EMAIL_IN_KINDS[kind];
      if (!k || !view || typeof view !== 'object') return '';
      const docs = '<a class="inbox-link" href="' + escapeHtml(/^https:\/\//.test(view.docs_url || '') ? view.docs_url : EMAIL_IN_DOCS_URL) + '" target="_blank" rel="noopener noreferrer">How to set it up ↗</a>';
      const off = why => '<div class="email-in is-off" data-email-in="' + kind + '" data-email-in-state="off" role="group" aria-label="Email-in address">' +
        '<span class="email-in-label">' + escapeHtml(k.label) + '</span>' +
        '<span class="muted-xs email-in-hint">' + escapeHtml(why) + ' ' + docs + '</span></div>';
      if (!view.ready || !emailInValid(view.address)) return off('Not set up yet: ' + (EMAIL_IN_REASONS[view.reason] || 'mail cannot reach this hive') + '.');
      if (!view[k.needs]) return off(k.off + ' (Settings, Meeting attendee, Email).');
      const addr = escapeHtml(view.address);
      return '<div class="email-in" data-email-in="' + kind + '" data-email-in-state="ready" role="group" aria-label="Email-in address">' +
        '<span class="email-in-label">' + escapeHtml(k.label) + '</span>' +
        '<a class="email-in-address" href="' + escapeHtml(emailInMailto(view.address)) + '" data-email-in-address title="Open a new email to this address">' + addr + '</a>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="emailInCopy" data-arg0="' + addr + '" aria-label="Copy ' + addr + '">Copy</button>' +
        '<span class="muted-xs email-in-hint">' + escapeHtml(k.hint) + '</span></div>';
    }
    async function emailInCopy(address) {
      if (!emailInValid(address)) return;
      try { await navigator.clipboard.writeText(address); showToast('Copied ' + address, 'success'); }
      catch (e) { showToast('Copy failed: ' + e.message, 'error'); }
    }

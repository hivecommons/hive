    // ── Inbox: Quick note (hivecommons/hive#18747) ──
    // A text box in the ＋ Add panel that makes an Inbox item from typed or
    // dictated text. The note is sent as a markdown file through the same
    // upload path as a dropped file (POST /api/inbox/uploads), so the hive
    // applies the same size cap, retention, extraction and auto-routing, and
    // records who added it. A multi-line paste onto the Inbox lands in the
    // box for review; nothing is created until Add note is chosen.
    // See src/docs/inbox-uploads.md.
    const INBOX_NOTE_FIELD_ID = 'inbox-add-note';
    const INBOX_NOTE_MIME = 'text/markdown';
    let _inboxNoteDraft = '';
    let _inboxNoteError = '';
    let _inboxNoteBusy = false;
    function inboxNoteBytes(text) { return new Blob([text]).size; }
    // inboxNoteName is the generated file name: note-YYYY-MM-DD-HHMMSS.md
    // in the viewer's local time.
    function inboxNoteName(when) {
      const p = n => String(n).padStart(2, '0');
      return 'note-' + when.getFullYear() + '-' + p(when.getMonth() + 1) + '-' + p(when.getDate()) + '-' + p(when.getHours()) + p(when.getMinutes()) + p(when.getSeconds()) + '.md';
    }
    function inboxNoteFile(text, when) { return new File([text + '\n'], inboxNoteName(when), { type: INBOX_NOTE_MIME }); }
    function inboxNoteHtml() {
      const max = Number(inboxUploadSettings().max_file_bytes) || 0;
      return '<div class="inbox-note"><label class="inbox-note-label" for="' + INBOX_NOTE_FIELD_ID + '">Quick note</label>' +
        '<textarea class="kb-search-box inbox-note-text" id="' + INBOX_NOTE_FIELD_ID + '" rows="3" placeholder="Type or dictate a note" aria-describedby="inbox-note-help" data-input-action="inboxNoteInput" data-arg-types="v">' + escapeHtml(_inboxNoteDraft) + '</textarea>' +
        '<div class="kc-actions mt-2"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxAddNote"' + (_inboxNoteBusy ? ' disabled' : '') + '>Add note</button>' +
        '<span class="muted-xs" id="inbox-note-help">Becomes an Inbox item, saved as a markdown file under your name' + (max ? ' (up to ' + escapeHtml(inboxFmtBytes(max)) + ')' : '') + '. Text of several lines pasted onto the Inbox lands here first.</span></div>' +
        (_inboxNoteError ? '<p class="kc-error muted-xs m-0" role="alert">' + escapeHtml(_inboxNoteError) + '</p>' : '') + '</div>';
    }
    function inboxNoteInput(value) { _inboxNoteDraft = String(value == null ? '' : value); }
    // inboxAddNote sends the note as one markdown file. The text stays in the
    // box until the hive has accepted it, so a refused or failed note is not
    // lost.
    function inboxAddNote() {
      if (!inboxCanAdd() || _inboxNoteBusy) return Promise.resolve(null);
      const el = document.getElementById(INBOX_NOTE_FIELD_ID);
      if (el) _inboxNoteDraft = String(el.value || '');
      const text = _inboxNoteDraft.trim();
      const max = Number(inboxUploadSettings().max_file_bytes) || 0;
      _inboxNoteError = '';
      if (!text) _inboxNoteError = 'Type or dictate a note first.';
      else if (max && inboxNoteBytes(text + '\n') > max) _inboxNoteError = 'Too long: ' + inboxFmtBytes(inboxNoteBytes(text + '\n')) + ' (the limit is ' + inboxFmtBytes(max) + '). Shorten it or add it as a file.';
      if (_inboxNoteError) { renderInbox(); return Promise.resolve(null); }
      _inboxNoteBusy = true;
      return inboxAddFiles([inboxNoteFile(text, new Date())]).then(rows => {
        const row = (rows || [])[0] || null;
        _inboxNoteBusy = false;
        if (row && row.status === 'done') { if (_inboxNoteDraft.trim() === text) _inboxNoteDraft = ''; }
        else _inboxNoteError = 'The note was not added' + (row && row.error ? ': ' + row.error : '.') + ' Your text is still here.';
        renderInbox();
        return row;
      });
    }
    // inboxPasteAsNote takes text of more than one line pasted onto the
    // Inbox (outside a field): it opens ＋ Add and puts the text in the note
    // box, after anything already typed there. It reports whether it took
    // the paste.
    function inboxPasteAsNote(text) {
      const t = String(text || '').trim();
      if (!inboxCanAdd() || t.indexOf('\n') < 0) return false;
      const draft = _inboxNoteDraft.replace(/\s+$/, '');
      _inboxNoteDraft = draft ? draft + '\n\n' + t : t;
      _inboxNoteError = '';
      _inboxAddOpen = true;
      renderInbox();
      const el = document.getElementById(INBOX_NOTE_FIELD_ID);
      if (el && el.focus) el.focus();
      inboxAnnounce('The pasted text is in the Quick note box. Review it, then choose Add note.');
      return true;
    }
    // inboxNoteKeepFocus runs a re-render and puts the caret back in the note
    // box when it was there, so a background refresh does not interrupt
    // typing or dictation.
    function inboxNoteKeepFocus(render) {
      const el = document.getElementById(INBOX_NOTE_FIELD_ID);
      const had = !!el && document.activeElement === el;
      const start = had ? el.selectionStart : 0, end = had ? el.selectionEnd : 0;
      render();
      if (!had) return;
      const next = document.getElementById(INBOX_NOTE_FIELD_ID);
      if (!next || !next.focus) return;
      next.focus();
      try { next.setSelectionRange(start, end); } catch (e) { /* not a text control */ }
    }

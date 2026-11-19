    // ── Inbox: input methods (hivecommons/hive#18747) ──
    // A quiet row under the source chips, from the catalogue GET /api/inbox
    // returns as input_methods (src/pkg/inbox/input_methods.go). Two groups:
    //   works_today: tools that already type or dictate into any text box,
    //     so they reach the Inbox through Quick note;
    //   coming_soon: neural interfaces with no public API or export yet.
    // These are not connectors. A chip only shows a short explanation, on
    // click or on focus; there is nothing to configure or connect. Names are
    // plain text and each note says hive is not affiliated with the project.
    // The row shows the featured entries; the full list is in the docs.
    const INBOX_INPUT_DOCS_URL = 'https://github.com/hivecommons/hive/blob/v5/src/docs/inbox-input-methods.md';
    const INBOX_INPUT_WORKS = 'works_today';
    const INBOX_INPUT_SOON = 'coming_soon';
    const INBOX_INPUT_GROUP_LABELS = { works_today: 'Works today', coming_soon: 'Neural input (coming soon)' };
    let _inboxInputOpen = '';
    function inboxInputMethods() {
      const list = (_inboxCache || {}).input_methods;
      return (Array.isArray(list) ? list : []).filter(m => m && m.id && m.name && INBOX_INPUT_GROUP_LABELS[m.state]);
    }
    function inboxInputLink(m) { return /^https:\/\//.test(String(m.url || '')) ? String(m.url) : ''; }
    function inboxInputChipHtml(m) {
      const soon = m.state === INBOX_INPUT_SOON;
      const label = m.name + (soon ? ', coming soon' : ', works today') + ': show details';
      return '<button type="button" class="kc-pill inbox-input-chip" data-input-state="' + escapeHtml(m.state) + '" data-input-id="' + escapeHtml(m.id) + '" data-action="inboxInputExplain" data-arg0="' + escapeHtml(m.id) + '" data-keydown-action="inboxInputClose" data-keys="Escape" aria-expanded="' + (_inboxInputOpen === m.id ? 'true' : 'false') + '" aria-controls="inbox-input-note" aria-label="' + escapeHtml(label) + '">' + escapeHtml(m.name) + '</button>';
    }
    // inboxInputNoteText is the whole explanation for one entry.
    function inboxInputNoteText(m) {
      const what = m.name + ': ' + String(m.note || '').trim();
      if (m.state === INBOX_INPUT_WORKS) {
        return what + ' It types into any text box, so it already reaches the Inbox: choose ＋ Add and use Quick note. There is nothing to connect. Hive is not affiliated with ' + m.name + '.';
      }
      return what + ' There is no public API or export of text or speech that hive can read yet, so it cannot be connected. Hive will add it as a source when one exists. What works today: anything that types or dictates into a text box can use ＋ Add, then Quick note. Hive is not affiliated with ' + m.name + '; this listing reflects interest, not a commitment from them.';
    }
    function inboxInputNoteHtml() {
      const m = inboxInputMethods().find(x => x.id === _inboxInputOpen);
      if (!m) return '<div class="inbox-input-note muted-xs" id="inbox-input-note" role="status" hidden></div>';
      const url = inboxInputLink(m);
      return '<div class="inbox-input-note muted-xs" id="inbox-input-note" role="status">' + escapeHtml(inboxInputNoteText(m)) +
        (url ? ' <a class="inbox-link" href="' + escapeHtml(url) + '" target="_blank" rel="noopener noreferrer">' + escapeHtml(m.name) + ' site</a>' : '') +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxInputClose">Close</button></div>';
    }
    function inboxInputMethodsHtml() {
      const shown = inboxInputMethods().filter(m => m.featured);
      if (!shown.length) return '';
      let html = '<div class="inbox-inputs" id="inbox-inputs" role="group" aria-label="Input methods (not connected sources)">';
      for (const state of [INBOX_INPUT_WORKS, INBOX_INPUT_SOON]) {
        const group = shown.filter(m => m.state === state);
        if (group.length) html += '<span class="inbox-input-group"><span class="muted-xs">' + escapeHtml(INBOX_INPUT_GROUP_LABELS[state]) + ':</span> ' + group.map(inboxInputChipHtml).join(' ') + '</span>';
      }
      return html + '<a class="inbox-link" href="' + INBOX_INPUT_DOCS_URL + '" target="_blank" rel="noopener noreferrer" title="The full list of input methods, and what each can and cannot do today">more</a></div>' + inboxInputNoteHtml();
    }
    // inboxInputSync redraws the note and the chips' expanded state in
    // place, so the focused chip keeps the focus.
    function inboxInputSync() {
      const row = document.getElementById('inbox-inputs');
      if (!row) return;
      row.querySelectorAll('.inbox-input-chip').forEach(c => c.setAttribute('aria-expanded', c.getAttribute('data-input-id') === _inboxInputOpen ? 'true' : 'false'));
      const note = document.getElementById('inbox-input-note');
      if (note) note.outerHTML = inboxInputNoteHtml();
    }
    function inboxInputExplain(id) {
      if (_inboxInputOpen === id) return;
      _inboxInputOpen = String(id || '');
      inboxInputSync();
    }
    function inboxInputClose() {
      if (!_inboxInputOpen) return;
      _inboxInputOpen = '';
      inboxInputSync();
    }
    function inboxInputOnFocus(e) {
      const chip = e && e.target && e.target.closest ? e.target.closest('.inbox-input-chip') : null;
      if (chip) inboxInputExplain(chip.getAttribute('data-input-id'));
    }
    document.addEventListener('focusin', inboxInputOnFocus);

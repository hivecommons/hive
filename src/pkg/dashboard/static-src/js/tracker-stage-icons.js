/* ---- Stage icons for workflow-model chevrons (hivecommons/hive#18033) ---
       A chevron that is not the row's active stage used to carry only its
       clipped column label. Now every column has a glyph that says what the
       stage means: picked by the column's name (STAGE_ICON_BY_NAME, keyed
       by the normalised label), or by the model's own `icon` on the column
       — an allowlisted id below or a single emoji, never markup. The set is
       16-unit monochrome strokes in currentColor, so it follows the theme
       like STAGE_TRACKER_ICONS does. Pure helpers: the node harness
       (stage_tracker_stage_icons_ui_test.go) lifts them by name, and a Go
       test keeps the id list equal to workflowmodel.ColumnIconIDs. */
    const STAGE_ICON_SET = {
      inbox: '<path d="M2 9.5h3.5l1 2h3l1-2H14"/><path d="M2 9.5 4 3.5h8l2 6v4H2z"/>',
      circle: '<circle cx="8" cy="8" r="5"/>',
      bolt: '<path d="M9 1 3 9h4l-1 6 6-8H8z"/>',
      hand: '<path d="M4.5 8.5V4a1 1 0 0 1 2 0v3.5M6.5 7V3a1 1 0 0 1 2 0v4M8.5 7V3.5a1 1 0 0 1 2 0V8M10.5 8V5a1 1 0 0 1 2 0v5.5c0 2.5-1.8 4-4.3 4-2 0-3-.9-3.9-2.3L2.6 9.9a1 1 0 0 1 1.6-1.2L4.5 9"/>',
      person: '<circle cx="8" cy="5" r="3"/><path d="M2.5 14.5a5.5 5.5 0 0 1 11 0"/>',
      pencil: '<path d="M11.5 2.5 13.5 4.5 5.5 12.5 2.5 13.5 3.5 10.5z"/><path d="M9.5 4.5l2 2"/>',
      'pull-request': '<circle cx="4" cy="3" r="1.5"/><circle cx="4" cy="13" r="1.5"/><circle cx="12" cy="13" r="1.5"/><path d="M4 4.5v7M12 11.5V7a2 2 0 0 0-2-2H8"/>',
      flask: '<path d="M5.5 1.5h5M6.5 1.5v4.6L2.6 12.9a1 1 0 0 0 .9 1.6h9a1 1 0 0 0 .9-1.6L9.5 6.1V1.5"/><path d="M4.4 10.5h7.2"/>',
      gear: '<circle cx="8" cy="8" r="2.2"/><path d="M8 1.5v2M8 12.5v2M1.5 8h2M12.5 8h2M3.4 3.4l1.4 1.4M11.2 11.2l1.4 1.4M3.4 12.6l1.4-1.4M11.2 4.8l1.4-1.4"/>',
      eye: '<path d="M1 8s2.5-5 7-5 7 5 7 5-2.5 5-7 5-7-5-7-5z"/><circle cx="8" cy="8" r="2"/>',
      people: '<circle cx="5.5" cy="5" r="2.5"/><path d="M1 14a4.5 4.5 0 0 1 9 0"/><path d="M10.5 3a2.5 2.5 0 0 1 0 4.5M12 10a4 4 0 0 1 3 4"/>',
      'thumbs-up': '<path d="M5 7.5 8 1.5c1.5 0 2 1 2 2L9.5 6H13a1.5 1.5 0 0 1 1.5 1.7l-1 5A1.5 1.5 0 0 1 12 14H5z"/><path d="M5 7.5H2v6.5h3"/>',
      'shield-check': '<path d="M8 1.5 13.5 3.5v4c0 3.5-2.5 5.9-5.5 7-3-1.1-5.5-3.5-5.5-7v-4z"/><path d="M5.5 8l1.8 1.8L10.8 6"/>',
      merge: '<path stroke-width="2" d="M6 14.5V3M3 6l3-3 3 3M13 14.5c0-3.8-3.4-4.6-7-6.5"/>',
      check: '<path d="M3 8l3 3 7-7"/>',
      checkbox: '<rect x="2.5" y="2.5" width="11" height="11" rx="1.5"/><path d="M5 8l2 2 4-4"/>',
      list: '<path d="M3 5h10M3 8h10M3 11h10"/>',
      tag: '<path d="M2.5 2.5h5.5l5.5 5.5-5.5 5.5-5.5-5.5z"/><circle cx="5.5" cy="5.5" r="1"/>',
      flag: '<path d="M3.5 14.5V2.5h8.5l-2 3 2 3H3.5"/>',
      rocket: '<path d="M8 1.5c2.5 1.5 3.5 4.5 3 8l-3 3-3-3c-.5-3.5.5-6.5 3-8z"/><circle cx="8" cy="6.5" r="1.2"/><path d="M5 9.5 3 11.5l.5 2 2-1M11 9.5l2 2-.5 2-2-1"/>',
      book: '<path d="M2.5 2.5h4a1.5 1.5 0 0 1 1.5 1.5v9.5a1 1 0 0 0-1-1h-4.5zM13.5 2.5h-4A1.5 1.5 0 0 0 8 4v9.5a1 1 0 0 1 1-1h4.5z"/>',
      search: '<circle cx="7" cy="7" r="4.5"/><path d="M10.5 10.5 14 14"/>',
      clock: '<circle cx="8" cy="8" r="6"/><path d="M8 4.5V8l2.5 1.5"/>',
      pause: '<path d="M5.5 3v10M10.5 3v10"/>',
      archive: '<rect x="2" y="2.5" width="12" height="3"/><path d="M3 5.5v8h10v-8M6.5 8.5h3"/>',
      dot: '<circle cx="8" cy="8" r="1.75" fill="currentColor"/>',
    };
    const STAGE_ICON_DEFAULT = 'dot';

    // Pure: the key a column name is matched by — lower-case, trimmed,
    // inner whitespace and the " / " and " · " separators collapsed to one
    // space, so "Reviewer check", "reviewer  check" and "CI / Review" each
    // land on one entry.
    function stageIconNormalizeName(name) {
      return String(name == null ? '' : name).toLowerCase().replace(/[\s·/]+/g, ' ').trim();
    }

    // Canonical stage name → icon id. Every column label across hive-default,
    // the curated marketplace bundles (cncf-techdocs-assessment), the ingest
    // skeleton (Intake, Triage) and the editor palette has an entry; the
    // workflow editor's column inspector shows the one its label resolves to.
    const STAGE_ICON_BY_NAME = {
      // hive-default
      'open': 'inbox',
      'actionable': 'bolt',
      'claimed': 'hand',
      'pr in progress': 'pull-request',
      'ci review': 'flask',
      'ci': 'flask',
      'review': 'eye',
      'merge queue': 'merge',
      'merged': 'check',
      // cncf-techdocs-assessment
      'requested': 'inbox',
      'accepted': 'checkbox',
      'drafting': 'pencil',
      'reviewer check': 'eye',
      'stakeholder review': 'people',
      'approval': 'thumbs-up',
      'done': 'check',
      // ingest skeleton and editor palette
      'intake': 'inbox',
      'triage': 'tag',
      'analysis': 'search',
      'writing': 'pencil',
      'publish': 'rocket',
      'published': 'rocket',
      'needs decision': 'person',
      'work in progress': 'pencil',
      'in progress': 'pencil',
      'in review': 'eye',
      'ci failing': 'flask',
      'released': 'rocket',
      'deployed': 'rocket',
      'deployed staging': 'rocket',
      'deployed production': 'rocket',
      'closed': 'archive',
      'verified': 'shield-check',
      'verification': 'shield-check',
      'approved': 'thumbs-up',
      'blocked': 'pause',
      'hold': 'pause',
      'on hold': 'pause',
      'waiting': 'clock',
      'backlog': 'list',
      'todo': 'list',
      'to do': 'list',
      'docs': 'book',
      'documentation': 'book',
    };

    // Pure: is v an allowlisted icon id?
    function stageIconIsId(v) { return Object.prototype.hasOwnProperty.call(STAGE_ICON_SET, String(v || '')); }

    // Pure: is v a single emoji — one or two pictographs (a ZWJ sequence or
    // a flag) plus modifiers, no letters, digits, spaces or markup? The
    // mirror of workflowmodel.IsColumnIconEmoji.
    function stageIconIsEmoji(v) {
      const s = String(v == null ? '' : v);
      if (!s || Array.from(s).length > 8) return false;
      let pict = 0;
      for (const ch of s) {
        const cp = ch.codePointAt(0);
        if (cp === 0x200D || cp === 0xFE0F || cp === 0xFE0E || cp === 0x20E3 || (cp >= 0x1F3FB && cp <= 0x1F3FF)) continue;
        if (cp < 0x80 || /[\p{L}\p{N}\s\p{Cc}]/u.test(ch)) return false;
        if ((cp >= 0x1F000 && cp <= 0x1FAFF) || (cp >= 0x2600 && cp <= 0x27BF) || (cp >= 0x2B00 && cp <= 0x2BFF) || (cp >= 0x2300 && cp <= 0x23FF) || (cp >= 0x2190 && cp <= 0x21FF) || (cp >= 0x25A0 && cp <= 0x25FF) || (cp >= 0x2900 && cp <= 0x297F) || (cp >= 0x1F1E6 && cp <= 0x1F1FF) || [0xA9, 0xAE, 0x2122, 0x2139, 0x3030, 0x303D, 0x3297, 0x3299].indexOf(cp) >= 0) { pict++; continue; }
        return false;
      }
      return pict >= 1 && pict <= 2;
    }

    // Pure: '' (default), an icon id or a single emoji validate; anything
    // else is refused — the editor shows why and keeps the last good value.
    function stageIconIsValid(v) {
      const s = String(v == null ? '' : v).trim();
      return s === '' || stageIconIsId(s) || stageIconIsEmoji(s);
    }

    // Pure: the icon id a column name resolves to; STAGE_ICON_DEFAULT when
    // the name is unknown.
    function stageIconIdForName(name) {
      return STAGE_ICON_BY_NAME[stageIconNormalizeName(name)] || STAGE_ICON_DEFAULT;
    }

    // Pure: what a column draws — its valid `icon` override (id or emoji),
    // else the id its label resolves to. An invalid override is ignored,
    // never rendered.
    function stageIconFor(col) {
      const c = col || {};
      const over = String(c.icon == null ? '' : c.icon).trim();
      if (over && (stageIconIsId(over) || stageIconIsEmoji(over))) return over;
      return stageIconIdForName(c.label || c.id);
    }

    // The icon as markup: an allowlisted id becomes its inline SVG, an
    // emoji a text span (escaped), anything else nothing. label is for
    // assistive tech only; the chevron's title carries the hover text.
    function stageIconHtml(icon, cls, label) {
      const v = String(icon == null ? '' : icon).trim();
      const aria = label ? ` role="img" aria-label="${esc(label)}"` : ' aria-hidden="true"';
      if (stageIconIsId(v)) return `<svg class="${cls}" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round"${aria}>${STAGE_ICON_SET[v]}</svg>`;
      if (stageIconIsEmoji(v)) return `<span class="${cls} ham-emoji-ico"${aria}>${esc(v)}</span>`;
      return '';
    }

    // The glyph a column's chevron wears, by its override or its name.
    function stageIconColumnHtml(col, cls, label) {
      return stageIconHtml(stageIconFor(col), cls, label);
    }

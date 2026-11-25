/*
 * Ask Hive help panel (src/docs/design/dashboard-ask-ai-help.md, #19177).
 *
 * Our own markup and script, served from 'self'. It talks only to this
 * dashboard's own backend (GET <base>/status, PUT|DELETE <base>/consent,
 * POST <base>) and never to the vendor. Everything the backend returns is
 * rendered with textContent; links come only from the allowlisted sources the
 * backend kept. No inline handlers, so it runs under a CSP with
 * script-src-attr 'none'.
 *
 * Usage: window.HiveAsk.mount({slot, base, section, docsHref, headers}).
 * The hub mounts it from the data-ask-* attributes on its own script tag.
 * Once mounted, window.HiveAsk.open() and close() let another control (the
 * dashboard Help menu's Ask Hive item) open or close the same panel. Both
 * return false when no panel was built, for example with Ask off.
 */
(function () {
  'use strict';

  var MAX_QUESTION_CHARS = 1000;
  // ASK_TIMEOUT_MS bounds one question in the browser. It must stay longer
  // than the server's own bound (askai.DefaultUpstreamTimeout, 30 s, for both
  // the vendor call and the spoke-to-hub relay) so a slow but successful
  // answer is never dropped; web_test.go pins the margin.
  var ASK_TIMEOUT_MS = 45000;
  // STILL_WORKING_AFTER_MS is when the busy status changes to "Still working".
  var STILL_WORKING_AFTER_MS = 10000;
  var ASKING_TEXT = 'Asking Hive, this can take up to 30 seconds.';
  var STILL_WORKING_TEXT = 'Still working...';
  var TIMEOUT_TEXT = 'Ask Hive took too long to answer. Try again, or use the docs link.';
  var UNAVAILABLE_TEXT = 'Ask is unavailable right now. Try again, or use the docs link.';
  var PANEL_ID = 'hive-ask-panel';
  var OPEN_CLASS = 'hive-ask-open';
  // SCROLL_LOCK_CLASS locks page scroll while the panel is a bottom-sheet
  // overlay; the stylesheet applies it only below SHEET_QUERY's width.
  var SCROLL_LOCK_CLASS = 'hive-ask-scroll-lock';
  // SHEET_QUERY must match the stylesheet's bottom-sheet breakpoint.
  var SHEET_QUERY = '(max-width: 899px)';
  // The built panel's open and close, set by build(). Null until the
  // backend has said Ask is on for this person.
  var active = null;

  // safeHost returns the host of an absolute https URL, or '' for anything
  // else (javascript:, data:, http:, relative paths). The single gate for
  // every link the panel shows, sources and links inside answers alike.
  function safeHost(url) {
    if (typeof url !== 'string' || !/^https:\/\//.test(url)) return '';
    try { return new URL(url).hostname || ''; } catch (e) { return ''; }
  }

  // SOURCE_TITLE_SEPARATOR joins a source's page and section for display.
  var SOURCE_TITLE_SEPARATOR = ' \u203a ';

  // sourceTitle splits a vendor source title such as
  // "Agent Configuration|Changing backend" into its page and section, and
  // the label the panel shows ("Agent Configuration > Changing backend",
  // with a single right-pointing angle quote). Pure, for tests.
  function sourceTitle(title) {
    var parts = String(title || '').split('|').map(function (p) { return p.trim(); }).filter(Boolean);
    var page = parts[0] || '';
    var section = parts.slice(1).join(SOURCE_TITLE_SEPARATOR);
    return { page: page, section: section, label: section ? page + SOURCE_TITLE_SEPARATOR + section : page };
  }

  function mdNorm(text) {
    return String(text || '').toLowerCase().replace(/[^a-z0-9]+/g, ' ').trim();
  }

  // citationIndex maps an inline citation marker the vendor leaves in an
  // answer, such as "[agent configuration]" or "[2]", to the index of the
  // matching source, or -1 when nothing matches (the marker is then
  // dropped). Pure, for tests.
  function citationIndex(marker, sources) {
    var list = Array.isArray(sources) ? sources : [];
    var m = mdNorm(marker);
    if (!m) return -1;
    if (/^\d+$/.test(m)) {
      var n = Number(m) - 1;
      return n >= 0 && n < list.length ? n : -1;
    }
    var candidates = list.map(function (s) {
      var path = '';
      try { path = new URL(s.href).pathname.split('/').filter(Boolean).pop() || ''; } catch (e) { path = ''; }
      return [s.label, s.page, s.section, path].map(mdNorm).filter(Boolean);
    });
    for (var i = 0; i < candidates.length; i++) {
      if (candidates[i].indexOf(m) !== -1) return i;
    }
    var padded = ' ' + m + ' ';
    for (var j = 0; j < candidates.length; j++) {
      var page = candidates[j][1] || candidates[j][0] || '';
      if (page && ((' ' + page + ' ').indexOf(padded) !== -1 || padded.indexOf(' ' + page + ' ') !== -1)) return j;
    }
    return -1;
  }

  // askViewModel turns a backend Result into what the panel shows. Pure, so
  // it can be tested without a DOM.
  function askViewModel(result) {
    var r = result || {};
    var sources = Array.isArray(r.sources) ? r.sources : [];
    var view = {
      kind: r.outcome === 'ok' ? 'answer' : 'notice',
      text: r.outcome === 'ok' ? String(r.answer || '') : String(r.message || 'Ask is unavailable right now.'),
      uncertain: r.outcome === 'ok' && !!r.uncertain,
      sources: [],
      docsUrl: typeof r.docs_url === 'string' ? r.docs_url : '',
      conversation: typeof r.conversation === 'string' ? r.conversation : '',
      retryAfter: Number(r.retry_after_seconds) || 0
    };
    if (view.kind === 'answer') {
      sources.forEach(function (s) {
        var url = s && typeof s.url === 'string' ? s.url : '';
        var host = safeHost(url);
        if (!host) return;
        var t = sourceTitle((s && s.title) || '');
        view.sources.push({ label: t.label || url, page: t.page, section: t.section, host: host, href: url });
      });
      if (!view.sources.length) {
        view.kind = 'notice';
        view.text = 'No cited answer found in the docs. Try the docs link.';
        view.uncertain = false;
      }
    }
    return view;
  }

  function el(tag, attrs, text) {
    var node = document.createElement(tag);
    Object.keys(attrs || {}).forEach(function (k) {
      if (k === 'className') node.className = attrs[k];
      else node.setAttribute(k, attrs[k]);
    });
    if (text != null) node.textContent = text;
    return node;
  }

  // ── Answer markdown ────────────────────────────────────────────────────
  // Answers come from an outside service, so they are untrusted. They are
  // parsed into plain data (answerBlocks, inlineTokens) and then built as DOM
  // nodes whose text is set with textContent. No answer text is ever parsed as
  // HTML: a <script> or <img onerror> in an answer shows as literal text.
  // Links keep only absolute https URLs (safeHost); anything else, such as
  // javascript: or data:, is dropped to its label. Supported: headings,
  // paragraphs, bullet and numbered lists (nested by indent), quotes, fenced
  // code, pipe tables, and inline code, bold, italics and links.

  // MD_INDENT_PER_LEVEL is how many leading spaces nest a list item one level.
  var MD_INDENT_PER_LEVEL = 2;
  // MD_SUBHEAD_SPLIT_LEVEL: markdown levels up to this render as h3, deeper
  // ones as h4, below the panel's own h2 title.
  var MD_SUBHEAD_SPLIT_LEVEL = 2;
  var MD_FENCE = /^\s*(```|~~~)/;
  var MD_ITEM = /^(\s*)(?:[-*+]|(\d+)[.)])\s+(.*)$/;
  var MD_HEADING = /^\s*(#{1,6})\s+(.*?)\s*#*\s*$/;
  var MD_QUOTE = /^\s*>\s?(.*)$/;
  var MD_TABLE_SEP = /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$/;

  function mdTableCells(line) {
    var t = line.trim();
    if (t.charAt(0) === '|') t = t.slice(1);
    if (t.charAt(t.length - 1) === '|') t = t.slice(0, -1);
    return t.split('|').map(function (c) { return c.trim(); });
  }

  function answerBlocks(text) {
    var lines = String(text || '').replace(/\r\n?/g, '\n').split('\n');
    var blocks = [];
    var para = [];
    var quote = [];
    var list = null;
    var listGap = false;
    var code = null;
    function flushPara() {
      if (para.length) blocks.push({ type: 'p', text: para.join(' ') });
      para = [];
    }
    function flushQuote() {
      if (quote.length) blocks.push({ type: 'quote', text: quote.join(' ') });
      quote = [];
    }
    function flushList() {
      if (list) blocks.push(list);
      list = null;
      listGap = false;
    }
    function flushAll() { flushPara(); flushQuote(); flushList(); }
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i];
      if (code) {
        if (MD_FENCE.test(line)) { blocks.push(code); code = null; }
        else code.text += (code.lines++ ? '\n' : '') + line;
        continue;
      }
      if (MD_FENCE.test(line)) {
        flushAll();
        code = { type: 'code', text: '', lines: 0 };
        continue;
      }
      var item = MD_ITEM.exec(line);
      if (item) {
        flushPara();
        flushQuote();
        var depth = Math.floor(item[1].replace(/\t/g, '    ').length / MD_INDENT_PER_LEVEL);
        var ordered = item[2] !== undefined;
        if (list && depth === 0 && list.ordered !== ordered) flushList();
        if (!list) {
          depth = 0;
          list = { type: 'list', ordered: ordered, start: ordered ? Number(item[2]) : 1, items: [] };
        }
        var prev = list.items.length ? list.items[list.items.length - 1].depth : 0;
        if (depth > prev + 1) depth = prev + 1;
        list.items.push({ depth: depth, ordered: ordered, text: item[3] });
        listGap = false;
        continue;
      }
      if (!line.trim()) {
        flushPara();
        flushQuote();
        if (list) listGap = true;
        continue;
      }
      // An indented line right under an item continues that item.
      if (list && !listGap && /^\s+\S/.test(line)) {
        list.items[list.items.length - 1].text += ' ' + line.trim();
        continue;
      }
      flushList();
      var heading = MD_HEADING.exec(line);
      if (heading) {
        flushPara();
        flushQuote();
        blocks.push({ type: 'h', level: heading[1].length, text: heading[2] });
        continue;
      }
      if (line.indexOf('|') !== -1 && i + 1 < lines.length && MD_TABLE_SEP.test(lines[i + 1])) {
        flushPara();
        flushQuote();
        var table = { type: 'table', head: mdTableCells(line), rows: [] };
        i += 1;
        while (i + 1 < lines.length && lines[i + 1].indexOf('|') !== -1 && lines[i + 1].trim()) {
          i += 1;
          table.rows.push(mdTableCells(lines[i]));
        }
        blocks.push(table);
        continue;
      }
      var q = MD_QUOTE.exec(line);
      if (q) { flushPara(); quote.push(q[1]); continue; }
      flushQuote();
      para.push(line.trim());
    }
    if (code) blocks.push(code);
    flushAll();
    blocks.forEach(function (b) { if (b.type === 'code') delete b.lines; });
    return blocks;
  }

  // MD_INLINE matches, in order: `code`, **bold** or __bold__, *em* or _em_,
  // [label](url) with at most one level of parentheses in the url, and a
  // bare [marker], which is how the vendor cites a source inline.
  var MD_INLINE = /`([^`]+)`|\*\*(.+?)\*\*|__(.+?)__|\*([^*\s](?:[^*]*[^*\s])?)\*|\b_([^_\s](?:[^_]*[^_\s])?)_\b|\[([^\]]+)\]\(((?:[^()\s]|\([^()\s]*\))+)(?:\s+"[^"]*")?\)|\[([^\[\]\n]{1,80})\](?!\()/g;

  // inlineTokens turns one line of inline markdown into plain tokens:
  // {type: 'text'|'code', text}, {type: 'strong'|'em', children},
  // {type: 'link', href, children} or {type: 'cite', n, href, title}.
  // A citation marker that matches no source is dropped, so bracket text
  // never shows. Pure, for tests.
  function inlineTokens(text, sources) {
    var src = String(text || '');
    var out = [];
    var last = 0;
    var m;
    function pushText(t) {
      if (!t) return;
      if (out.length && out[out.length - 1].type === 'text') out[out.length - 1].text += t;
      else out.push({ type: 'text', text: t });
    }
    // A fresh regex per call: bold, italics and link labels recurse, and a
    // shared global regex would lose its place.
    var re = new RegExp(MD_INLINE.source, 'g');
    while ((m = re.exec(src)) !== null) {
      pushText(src.slice(last, m.index));
      last = re.lastIndex;
      if (m[1] !== undefined) out.push({ type: 'code', text: m[1] });
      else if (m[2] !== undefined || m[3] !== undefined) out.push({ type: 'strong', children: inlineTokens(m[2] !== undefined ? m[2] : m[3], sources) });
      else if (m[4] !== undefined || m[5] !== undefined) out.push({ type: 'em', children: inlineTokens(m[4] !== undefined ? m[4] : m[5], sources) });
      else if (m[8] !== undefined) {
        var at = citationIndex(m[8], sources);
        // The space before a marker goes: a citation hugs its word, and a
        // dropped marker must not leave a space before punctuation.
        if (out.length && out[out.length - 1].type === 'text') out[out.length - 1].text = out[out.length - 1].text.replace(/\s+$/, '');
        if (at !== -1) out.push({ type: 'cite', n: at + 1, href: sources[at].href, title: sources[at].label });
      }
      else if (safeHost(m[7])) out.push({ type: 'link', href: m[7], children: inlineTokens(m[6], sources) });
      else inlineTokens(m[6], sources).forEach(function (t) { if (t.type === 'text') pushText(t.text); else out.push(t); });
    }
    pushText(src.slice(last));
    return out;
  }

  function appendInline(parent, text, sources) {
    inlineTokens(text, sources).forEach(function (tok) { appendToken(parent, tok); });
    return parent;
  }

  function appendToken(parent, tok) {
    if (tok.type === 'text') { parent.appendChild(document.createTextNode(tok.text)); return; }
    if (tok.type === 'code') { parent.appendChild(el('code', { className: 'hive-ask-inline-code' }, tok.text)); return; }
    if (tok.type === 'cite') {
      var sup = el('sup', { className: 'hive-ask-cite' });
      sup.appendChild(el('a', { href: tok.href, target: '_blank', rel: 'noopener noreferrer', title: tok.title, 'aria-label': 'Source ' + tok.n + ': ' + tok.title }, '[' + tok.n + ']'));
      parent.appendChild(sup);
      return;
    }
    var node = tok.type === 'link'
      ? el('a', { href: tok.href, target: '_blank', rel: 'noopener noreferrer', className: 'hive-ask-docs' })
      : el(tok.type === 'strong' ? 'strong' : 'em', {});
    tok.children.forEach(function (c) { appendToken(node, c); });
    parent.appendChild(node);
  }

  function renderList(box, b, sources) {
    var root = el(b.ordered ? 'ol' : 'ul', { className: 'hive-ask-list' });
    if (b.ordered && b.start !== 1) root.setAttribute('start', String(b.start));
    box.appendChild(root);
    // stack[d] is the open list at depth d; lastItem[d] its last li.
    var stack = [root];
    var lastItem = [];
    b.items.forEach(function (it) {
      while (stack.length - 1 > it.depth) { stack.pop(); lastItem.pop(); }
      if (it.depth > stack.length - 1) {
        var sub = el(it.ordered ? 'ol' : 'ul', { className: 'hive-ask-list' });
        lastItem[stack.length - 1].appendChild(sub);
        stack.push(sub);
      }
      var li = appendInline(el('li', {}), it.text, sources);
      stack[stack.length - 1].appendChild(li);
      lastItem[stack.length - 1] = li;
    });
  }

  // renderAnswer builds an answer's DOM. sources are the view model's
  // sources, so inline citation markers can link to them.
  function renderAnswer(text, sources) {
    var box = el('div', { className: 'hive-ask-text hive-ask-md' });
    answerBlocks(text).forEach(function (b) {
      if (b.type === 'code') {
        box.appendChild(el('pre', { className: 'hive-ask-code', tabindex: '0' })).appendChild(el('code', {}, b.text));
      } else if (b.type === 'list') {
        renderList(box, b, sources);
      } else if (b.type === 'h') {
        box.appendChild(appendInline(el(b.level <= MD_SUBHEAD_SPLIT_LEVEL ? 'h3' : 'h4', { className: 'hive-ask-subhead' }), b.text, sources));
      } else if (b.type === 'quote') {
        box.appendChild(appendInline(el('blockquote', { className: 'hive-ask-quote' }), b.text, sources));
      } else if (b.type === 'table') {
        var wrap = box.appendChild(el('div', { className: 'hive-ask-table-wrap', tabindex: '0' }));
        var table = wrap.appendChild(el('table', { className: 'hive-ask-table' }));
        var headRow = table.appendChild(el('thead', {})).appendChild(el('tr', {}));
        b.head.forEach(function (c) { headRow.appendChild(appendInline(el('th', { scope: 'col' }), c, sources)); });
        var tbody = table.appendChild(el('tbody', {}));
        b.rows.forEach(function (r) {
          var tr = tbody.appendChild(el('tr', {}));
          b.head.forEach(function (_, idx) { tr.appendChild(appendInline(el('td', {}), r[idx] || '', sources)); });
        });
      } else {
        box.appendChild(appendInline(el('p', {}), b.text, sources));
      }
    });
    return box;
  }

  function docsLink(href, text) {
    return el('a', { href: href, target: '_blank', rel: 'noopener noreferrer', className: 'hive-ask-docs' }, text);
  }

  function mount(opts) {
    opts = opts || {};
    var slot = typeof opts.slot === 'string' ? document.getElementById(opts.slot) : opts.slot;
    var base = opts.base || '/api/ask';
    if (!slot || document.getElementById(PANEL_ID)) return null;
    var sectionOf = typeof opts.section === 'function' ? opts.section : function () { return opts.section || ''; };
    var docsOf = typeof opts.docsHref === 'function' ? opts.docsHref : function () { return opts.docsHref || ''; };
    var extraHeaders = typeof opts.headers === 'function' ? opts.headers : function () { return {}; };
    var status = null;
    var conversation = '';
    var busy = false;

    function request(method, path, body, signal) {
      var headers = { Accept: 'application/json' };
      var extra = extraHeaders() || {};
      Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
      if (body !== undefined) headers['Content-Type'] = 'application/json';
      var init = {
        method: method, headers: headers, credentials: 'same-origin', cache: 'no-store',
        body: body === undefined ? undefined : JSON.stringify(body)
      };
      if (signal) init.signal = signal;
      return fetch(base + path, init);
    }

    // Build nothing until the backend says the feature is on for this person.
    return request('GET', '/status').then(function (resp) {
      if (!resp.ok) return null;
      return resp.json();
    }).then(function (data) {
      if (!data || data.enabled !== true) return null;
      status = data;
      return build();
    }).catch(function () { return null; });

    function build() {
      var opener = el('button', { type: 'button', className: 'hive-ask-opener', 'aria-expanded': 'false', 'aria-controls': PANEL_ID }, 'Ask');
      opener.title = 'Ask Hive (AI)';
      slot.appendChild(opener);

      var panel = el('section', { id: PANEL_ID, className: 'hive-ask-panel', 'aria-labelledby': 'hive-ask-title' });
      panel.setAttribute('role', 'region');
      panel.hidden = true;
      var head = el('div', { className: 'hive-ask-head' });
      head.appendChild(el('h2', { id: 'hive-ask-title' }, 'Ask Hive'));
      var close = el('button', { type: 'button', className: 'hive-ask-close', 'aria-label': 'Close Ask Hive' }, '×');
      head.appendChild(close);
      panel.appendChild(head);
      // Everything under the title row sits in one padded body, so the title
      // and close stay pinned while the body scrolls inside the panel.
      var body = el('div', { className: 'hive-ask-body' });
      panel.appendChild(body);
      body.appendChild(el('p', { className: 'hive-ask-label' }, status.ai_label || 'AI generated, may be wrong.'));
      // note shows, visibly, what a person must act on (a failed save, a
      // changed notice). The live region below is screen-reader only and
      // only ever holds short status strings, never answer text.
      var note = el('p', { className: 'hive-ask-note' });
      body.appendChild(note);

      var consentBox = el('div', { className: 'hive-ask-consent' });
      consentBox.appendChild(el('p', { className: 'hive-ask-disclosure' }, status.disclosure || ''));
      // The primary action and the docs fallback share one row (wrapping on
      // a narrow phone) instead of stacking with uneven spacing.
      var actions = el('div', { className: 'hive-ask-actions' });
      var confirm = el('button', { type: 'button', className: 'hive-ask-confirm' }, 'I understand, continue');
      actions.appendChild(confirm);
      actions.appendChild(docsLink(docsOf() || status.docs_url, 'Read the docs instead'));
      consentBox.appendChild(actions);
      body.appendChild(consentBox);

      var chat = el('div', { className: 'hive-ask-chat' });
      // The secrets warning sits once, as a muted line by the AI label; the
      // consent notice already carried it before consent.
      chat.appendChild(el('p', { className: 'hive-ask-label' }, 'Do not include secrets or private details.'));
      var log = el('ol', { className: 'hive-ask-log', 'aria-label': 'Answers' });
      chat.appendChild(log);
      // The live region sits outside the consent and chat boxes, so it is
      // heard in either state, and is visually hidden (see the stylesheet).
      var live = el('p', { className: 'hive-ask-status', role: 'status', 'aria-live': 'polite' });
      body.appendChild(live);
      var form = el('form', { className: 'hive-ask-form' });
      form.appendChild(el('label', { for: 'hive-ask-input' }, 'Ask Hive a question'));
      var input = el('textarea', { id: 'hive-ask-input', rows: '3', maxlength: String(MAX_QUESTION_CHARS) });
      form.appendChild(input);
      // Hint and primary button share one row: hint left, Ask right-aligned.
      var formRow = el('div', { className: 'hive-ask-form-row' });
      formRow.appendChild(el('p', { className: 'hive-ask-hint' }, 'Enter to send, Shift+Enter for a new line'));
      var send = el('button', { type: 'submit', className: 'hive-ask-send' }, 'Ask');
      formRow.appendChild(send);
      form.appendChild(formRow);
      chat.appendChild(form);
      // Quiet footer: secondary text links, never as prominent as Ask.
      var foot = el('div', { className: 'hive-ask-foot' });
      var sectionDocs = docsLink(docsOf() || status.docs_url, 'Docs for this section');
      foot.appendChild(sectionDocs);
      var withdraw = el('button', { type: 'button', className: 'hive-ask-withdraw' }, 'Stop sending my questions');
      foot.appendChild(withdraw);
      chat.appendChild(foot);
      body.appendChild(chat);
      document.body.appendChild(panel);

      function showConsent(consented) {
        consentBox.hidden = consented;
        chat.hidden = !consented;
      }
      showConsent(status.consented === true);

      function announce(text) { live.textContent = text; }
      function notify(text) {
        note.textContent = text;
        announce(text);
      }

      function setOpen(open) {
        panel.hidden = !open;
        opener.setAttribute('aria-expanded', open ? 'true' : 'false');
        document.documentElement.classList.toggle(OPEN_CLASS, open);
        document.documentElement.classList.toggle(SCROLL_LOCK_CLASS, open);
        if (open) {
          sectionDocs.href = docsOf() || status.docs_url;
          (status.consented ? input : confirm).focus();
        } else {
          opener.focus();
        }
      }

      // addEntry shows one question and its answer or notice. retry, when
      // given, adds a Retry button that asks the same question again.
      function addEntry(view, question, retry) {
        var item = el('li', { className: 'hive-ask-entry' });
        if (question) item.appendChild(el('p', { className: 'hive-ask-q' }, question));
        var answer = el('div', { className: view.kind === 'answer' ? 'hive-ask-a' : 'hive-ask-a hive-ask-notice' });
        if (view.kind === 'answer') {
          // Order: the answer, its numbered sources, then one muted
          // disclaimer line.
          answer.appendChild(renderAnswer(view.text, view.sources));
          var sources = el('div', { className: 'hive-ask-sources-block' });
          sources.appendChild(el('p', { className: 'hive-ask-sources-title' }, 'Sources'));
          var list = el('ol', { className: 'hive-ask-sources' });
          view.sources.forEach(function (s, idx) {
            var li = el('li');
            li.appendChild(el('span', { className: 'hive-ask-source-n' }, '[' + (idx + 1) + ']'));
            li.appendChild(docsLink(s.href, s.label));
            li.appendChild(el('span', { className: 'hive-ask-source-host' }, s.host));
            list.appendChild(li);
          });
          sources.appendChild(list);
          answer.appendChild(sources);
          answer.appendChild(el('p', { className: 'hive-ask-ai' }, view.uncertain
            ? 'AI generated, may be wrong. The service marked this answer as uncertain.'
            : 'AI generated, may be wrong.'));
        } else {
          answer.appendChild(el('p', { className: 'hive-ask-text' }, view.text));
          var row = el('div', { className: 'hive-ask-actions' });
          if (retry) {
            var again = el('button', { type: 'button', className: 'hive-ask-retry' }, 'Retry');
            again.addEventListener('click', function () {
              if (busy) return;
              if (item.parentNode) item.parentNode.removeChild(item);
              input.value = question;
              ask();
            });
            row.appendChild(again);
          }
          row.appendChild(docsLink(docsOf() || view.docsUrl || status.docs_url, 'Open the docs'));
          answer.appendChild(row);
        }
        item.appendChild(answer);
        log.appendChild(item);
        // A long answer is read from its top, so bring the question there.
        if (typeof item.scrollIntoView === 'function') item.scrollIntoView({ block: 'start' });
        return item;
      }

      // setBusy puts the form into, or out of, its waiting state: Ask shows
      // a spinner and "Asking...", the question is read-only, and the log is
      // marked busy. Always called with false when a question settles.
      function setBusy(on) {
        busy = on;
        send.disabled = on;
        input.readOnly = on;
        send.className = on ? 'hive-ask-send is-busy' : 'hive-ask-send';
        send.textContent = '';
        if (on) {
          send.appendChild(el('span', { className: 'hive-ask-spinner', 'aria-hidden': 'true' }));
          send.appendChild(el('span', {}, 'Asking...'));
          send.setAttribute('aria-busy', 'true');
          log.setAttribute('aria-busy', 'true');
        } else {
          send.textContent = 'Ask';
          send.removeAttribute('aria-busy');
          log.removeAttribute('aria-busy');
        }
      }

      function ask() {
        var question = input.value.trim();
        if (!question || busy) return;
        setBusy(true);
        announce(ASKING_TEXT);
        var stillTimer = setTimeout(function () { announce(STILL_WORKING_TEXT); }, STILL_WORKING_AFTER_MS);
        var timedOut = false;
        var controller = typeof AbortController === 'function' ? new AbortController() : null;
        // One timer ends the wait: it aborts the fetch where the browser can,
        // and rejects the deadline so the question settles even where it cannot.
        var expire = null;
        var deadline = new Promise(function (resolve, reject) { expire = reject; });
        var abortTimer = setTimeout(function () {
          timedOut = true;
          if (controller) controller.abort();
          expire(new Error('timeout'));
        }, ASK_TIMEOUT_MS);
        var body = { question: question, section: sectionOf() || '' };
        if (conversation) body.conversation = conversation;
        // The backend answers with one JSON document, not a stream, so the
        // busy state holds until the whole answer lands.
        var pending = request('POST', '', body, controller ? controller.signal : undefined).then(function (resp) {
          if (!resp.ok) throw new Error('HTTP ' + resp.status);
          return resp.json();
        });
        Promise.race([pending, deadline]).then(function (result) {
          var view = askViewModel(result);
          if (view.conversation) conversation = view.conversation;
          if (result && result.outcome === 'not_consented') {
            status.consented = false;
            showConsent(false);
          }
          addEntry(view, question, result && result.outcome === 'upstream_error');
          if (view.kind === 'answer') input.value = '';
          announce(view.kind === 'answer' ? 'Answer received.' : view.text);
        }).catch(function () {
          var text = timedOut ? TIMEOUT_TEXT : UNAVAILABLE_TEXT;
          addEntry(askViewModel({ outcome: 'upstream_error', message: text }), question, true);
          announce(text);
        }).then(settle, settle);
        // settle runs whatever happened above, so Ask is never left disabled.
        function settle() {
          clearTimeout(stillTimer);
          clearTimeout(abortTimer);
          setBusy(false);
        }
      }

      // open() from another control reuses setOpen, so focus lands where the
      // opener would put it; on an open panel it only moves focus back in.
      active = {
        open: function () {
          if (panel.hidden) setOpen(true);
          else (status.consented ? input : confirm).focus();
        },
        close: function () { if (!panel.hidden) setOpen(false); }
      };
      // iOS Safari can ignore overscroll-behavior, so on the bottom sheet a
      // vertical drag that would run past the panel's top or bottom is
      // stopped here instead of scrolling the page behind it.
      var touchX = 0;
      var touchY = 0;
      panel.addEventListener('touchstart', function (e) {
        if (e.touches && e.touches.length === 1) { touchX = e.touches[0].clientX; touchY = e.touches[0].clientY; }
      }, { passive: true });
      panel.addEventListener('touchmove', function (e) {
        if (!e.touches || e.touches.length !== 1 || e.target === input) return;
        if (!(window.matchMedia && window.matchMedia(SHEET_QUERY).matches)) return;
        var dx = e.touches[0].clientX - touchX;
        var dy = e.touches[0].clientY - touchY;
        if (Math.abs(dx) > Math.abs(dy)) return;
        var atTop = panel.scrollTop <= 0;
        var atBottom = panel.scrollTop + panel.clientHeight >= panel.scrollHeight - 1;
        if (((dy > 0 && atTop) || (dy < 0 && atBottom)) && e.cancelable) e.preventDefault();
        e.stopPropagation();
      }, { passive: false });
      opener.addEventListener('click', function () { setOpen(panel.hidden); });
      close.addEventListener('click', function () { setOpen(false); });
      panel.addEventListener('keydown', function (e) {
        if (e.key === 'Escape') { e.preventDefault(); setOpen(false); }
      });
      input.addEventListener('keydown', function (e) {
        if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); ask(); }
      });
      form.addEventListener('submit', function (e) { e.preventDefault(); ask(); });
      confirm.addEventListener('click', function () {
        request('PUT', '/consent', { text_version: status.text_version }).then(function (resp) {
          if (resp.status === 409) { notify('The notice changed. Reload the page and read it again.'); return; }
          if (!resp.ok) throw new Error('HTTP ' + resp.status);
          status.consented = true;
          showConsent(true);
          note.textContent = '';
          announce('Thanks. You can ask now.');
          input.focus();
        }).catch(function () { notify('Could not save your choice. Try again.'); });
      });
      withdraw.addEventListener('click', function () {
        request('DELETE', '/consent').then(function (resp) {
          if (!resp.ok) throw new Error('HTTP ' + resp.status);
          status.consented = false;
          conversation = '';
          showConsent(false);
          notify('Your questions will no longer be sent. What the service already holds cannot be recalled.');
          confirm.focus();
        }).catch(function () { notify('Could not save your choice. Try again.'); });
      });
      return panel;
    }
  }

  function open() {
    if (!active) return false;
    active.open();
    return true;
  }

  function close() {
    if (!active) return false;
    active.close();
    return true;
  }

  var api = {
    mount: mount, open: open, close: close, askViewModel: askViewModel, answerBlocks: answerBlocks,
    inlineTokens: inlineTokens, renderAnswer: renderAnswer, sourceTitle: sourceTitle, citationIndex: citationIndex
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (typeof window === 'undefined') return;
  window.HiveAsk = api;

  // Auto-mount from the script tag's data-ask-* attributes (hub dashboard).
  var script = document.currentScript;
  if (script && script.dataset && script.dataset.askSlot) {
    var start = function () {
      mount({
        slot: script.dataset.askSlot,
        base: script.dataset.askBase || '/api/ask',
        section: script.dataset.askSection || '',
        docsHref: script.dataset.askDocs || ''
      });
    };
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start);
    else start();
  }
})();

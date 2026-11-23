/*
 * Ask AI help panel (src/docs/design/dashboard-ask-ai-help.md, #19177).
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
 */
(function () {
  'use strict';

  var MAX_QUESTION_CHARS = 1000;
  var PANEL_ID = 'hive-ask-panel';
  var OPEN_CLASS = 'hive-ask-open';

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
        if (!/^https:\/\//.test(url)) return;
        var host = '';
        try { host = new URL(url).hostname; } catch (e) { return; }
        view.sources.push({ label: String((s && s.title) || url), host: host, href: url });
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

    function request(method, path, body) {
      var headers = { Accept: 'application/json' };
      var extra = extraHeaders() || {};
      Object.keys(extra).forEach(function (k) { headers[k] = extra[k]; });
      if (body !== undefined) headers['Content-Type'] = 'application/json';
      return fetch(base + path, {
        method: method, headers: headers, credentials: 'same-origin', cache: 'no-store',
        body: body === undefined ? undefined : JSON.stringify(body)
      });
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
      opener.title = 'Ask the docs (AI)';
      slot.appendChild(opener);

      var panel = el('section', { id: PANEL_ID, className: 'hive-ask-panel', 'aria-labelledby': 'hive-ask-title' });
      panel.setAttribute('role', 'region');
      panel.hidden = true;
      var head = el('div', { className: 'hive-ask-head' });
      head.appendChild(el('h2', { id: 'hive-ask-title' }, 'Ask the docs'));
      var close = el('button', { type: 'button', className: 'hive-ask-close', 'aria-label': 'Close Ask' }, '×');
      head.appendChild(close);
      panel.appendChild(head);
      panel.appendChild(el('p', { className: 'hive-ask-label' }, status.ai_label || 'AI generated, may be wrong.'));

      var consentBox = el('div', { className: 'hive-ask-consent' });
      consentBox.appendChild(el('p', {}, status.disclosure || ''));
      var confirm = el('button', { type: 'button', className: 'hive-ask-confirm' }, 'I understand, continue');
      consentBox.appendChild(confirm);
      consentBox.appendChild(el('p', {})).appendChild(docsLink(docsOf() || status.docs_url, 'Read the docs instead'));
      panel.appendChild(consentBox);

      var chat = el('div', { className: 'hive-ask-chat' });
      var log = el('ol', { className: 'hive-ask-log', 'aria-label': 'Answers' });
      chat.appendChild(log);
      var live = el('p', { className: 'hive-ask-status', role: 'status', 'aria-live': 'polite' });
      chat.appendChild(live);
      var form = el('form', { className: 'hive-ask-form' });
      form.appendChild(el('label', { for: 'hive-ask-input' }, 'Your question about the dashboard'));
      var input = el('textarea', { id: 'hive-ask-input', rows: '3', maxlength: String(MAX_QUESTION_CHARS) });
      form.appendChild(input);
      form.appendChild(el('p', { className: 'hive-ask-hint' }, 'Enter sends, Shift+Enter adds a line. Do not include secrets or private details.'));
      var send = el('button', { type: 'submit', className: 'hive-ask-send' }, 'Ask');
      form.appendChild(send);
      chat.appendChild(form);
      var foot = el('p', { className: 'hive-ask-foot' });
      var sectionDocs = docsLink(docsOf() || status.docs_url, 'Docs for this section');
      foot.appendChild(sectionDocs);
      var withdraw = el('button', { type: 'button', className: 'hive-ask-withdraw' }, 'Stop sending my questions');
      foot.appendChild(withdraw);
      chat.appendChild(foot);
      panel.appendChild(chat);
      document.body.appendChild(panel);

      function showConsent(consented) {
        consentBox.hidden = consented;
        chat.hidden = !consented;
      }
      showConsent(status.consented === true);

      function announce(text) { live.textContent = text; }

      function setOpen(open) {
        panel.hidden = !open;
        opener.setAttribute('aria-expanded', open ? 'true' : 'false');
        document.documentElement.classList.toggle(OPEN_CLASS, open);
        if (open) {
          sectionDocs.href = docsOf() || status.docs_url;
          (status.consented ? input : confirm).focus();
        } else {
          opener.focus();
        }
      }

      function addEntry(view, question) {
        var item = el('li', { className: 'hive-ask-entry' });
        if (question) item.appendChild(el('p', { className: 'hive-ask-q' }, question));
        var answer = el('div', { className: view.kind === 'answer' ? 'hive-ask-a' : 'hive-ask-a hive-ask-notice' });
        answer.appendChild(el('p', { className: 'hive-ask-text' }, view.text));
        if (view.kind === 'answer') {
          answer.appendChild(el('p', { className: 'hive-ask-ai' }, 'AI generated, may be wrong.'));
          if (view.uncertain) answer.appendChild(el('p', { className: 'hive-ask-ai' }, 'The service marked this answer as uncertain.'));
          answer.appendChild(el('p', { className: 'hive-ask-sources-title' }, 'Sources'));
          var list = el('ul', { className: 'hive-ask-sources' });
          view.sources.forEach(function (s) {
            var li = el('li');
            li.appendChild(docsLink(s.href, s.label + ' (' + s.host + ')'));
            list.appendChild(li);
          });
          answer.appendChild(list);
        } else {
          answer.appendChild(el('p', {})).appendChild(docsLink(docsOf() || view.docsUrl || status.docs_url, 'Open the docs'));
        }
        item.appendChild(answer);
        log.appendChild(item);
        item.scrollIntoView({ block: 'nearest' });
      }

      function ask() {
        var question = input.value.trim();
        if (!question || busy) return;
        busy = true;
        send.disabled = true;
        log.setAttribute('aria-busy', 'true');
        announce('Asking…');
        var body = { question: question, section: sectionOf() || '' };
        if (conversation) body.conversation = conversation;
        request('POST', '', body).then(function (resp) {
          if (!resp.ok) throw new Error('HTTP ' + resp.status);
          return resp.json();
        }).then(function (result) {
          var view = askViewModel(result);
          if (view.conversation) conversation = view.conversation;
          if (result && result.outcome === 'not_consented') {
            status.consented = false;
            showConsent(false);
          }
          addEntry(view, question);
          if (view.kind === 'answer') input.value = '';
          announce(view.kind === 'answer' ? 'Answer received. ' + view.text : view.text);
        }).catch(function () {
          var view = askViewModel({ outcome: 'upstream_error', message: 'Ask is unavailable right now. Try the docs link.' });
          addEntry(view, question);
          announce(view.text);
        }).then(function () {
          busy = false;
          send.disabled = false;
          log.removeAttribute('aria-busy');
        });
      }

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
          if (resp.status === 409) { announce('The notice changed. Reload the page and read it again.'); return; }
          if (!resp.ok) throw new Error('HTTP ' + resp.status);
          status.consented = true;
          showConsent(true);
          announce('Thanks. You can ask now.');
          input.focus();
        }).catch(function () { announce('Could not save your choice. Try again.'); });
      });
      withdraw.addEventListener('click', function () {
        request('DELETE', '/consent').then(function (resp) {
          if (!resp.ok) throw new Error('HTTP ' + resp.status);
          status.consented = false;
          conversation = '';
          showConsent(false);
          announce('Your questions will no longer be sent. What the service already holds cannot be recalled.');
          confirm.focus();
        }).catch(function () { announce('Could not save your choice. Try again.'); });
      });
      return panel;
    }
  }

  var api = { mount: mount, askViewModel: askViewModel };
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

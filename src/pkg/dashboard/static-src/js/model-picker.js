    // Searchable, grouped model picker for gateways with large catalogs
    // (OpenRouter lists ~470 models). A combobox: type to filter, options are
    // grouped by provider prefix (z-ai/, deepseek/, anthropic/, …), curated
    // suggestions are pinned on top, and each option shows its price per 1M
    // input/output tokens and context window when the catalog publishes them.
    // The typed text IS the value, so a model the catalog does not list yet is
    // always enterable ("Custom…"). ARIA: input[role=combobox] owning a
    // ul[role=listbox]; ArrowUp/ArrowDown move, Enter picks, Escape closes.
    // The helpers above modelComboboxHtml are pure (no DOM) so node tests can
    // run them.

    // Below this many options a plain <select> stays simpler to use.
    const MODEL_COMBOBOX_MIN_OPTIONS = 20;
    const MODEL_TOKENS_PER_MILLION = 1000000;
    const MODEL_CTX_MILLION = 1000000;
    const MODEL_CTX_THOUSAND = 1000;
    // Prices under this many dollars per 1M get extra significant digits so
    // $0.0042 does not render as $0.00.
    const MODEL_PRICE_SMALL_USD = 0.1;
    const MODEL_PRICE_SIG_DIGITS = 2;
    const MODEL_PRICE_FIXED_DIGITS = 2;
    const MODEL_GROUP_OTHER = 'other';
    const MODEL_GROUP_SUGGESTED = 'Suggested';

    // modelProviderOf returns the provider prefix of a namespaced id
    // ("z-ai/glm-5.3" → "z-ai"); '' for a bare id. A leading "~" (OpenRouter's
    // "-latest" aliases) is ignored so ~anthropic/... groups with anthropic.
    function modelProviderOf(id) {
      const s = String(id || '').replace(/^~/, '');
      const i = s.indexOf('/');
      return i > 0 ? s.slice(0, i) : '';
    }

    // modelInfoIndex turns a model_info array into an id → info map.
    function modelInfoIndex(info) {
      const idx = {};
      (Array.isArray(info) ? info : []).forEach(m => { if (m && m.id) idx[m.id] = m; });
      return idx;
    }

    // formatModelPricePerM renders a per-token USD price as dollars per 1M
    // tokens; '' when unknown. Zero is a real price ("$0", free models).
    function formatModelPricePerM(perToken) {
      if (perToken === null || perToken === undefined || perToken === '') return '';
      const v = Number(perToken) * MODEL_TOKENS_PER_MILLION;
      if (!Number.isFinite(v) || v < 0) return '';
      if (v === 0) return '$0';
      if (v < MODEL_PRICE_SMALL_USD) return '$' + Number(v.toPrecision(MODEL_PRICE_SIG_DIGITS));
      return '$' + v.toFixed(MODEL_PRICE_FIXED_DIGITS);
    }

    // formatModelContext renders a context window ("1M ctx", "200K ctx").
    function formatModelContext(n) {
      const v = Number(n);
      if (!Number.isFinite(v) || v <= 0) return '';
      if (v >= MODEL_CTX_MILLION) return Number((v / MODEL_CTX_MILLION).toFixed(1)) + 'M ctx';
      return Math.round(v / MODEL_CTX_THOUSAND) + 'K ctx';
    }

    // modelPriceLabel is the right-hand detail of an option: price per 1M
    // input / output tokens (and cached input when published) plus context.
    // A model with no published price says so instead of looking free.
    function modelPriceLabel(info) {
      if (!info) return '';
      const p = info.pricing || {};
      const parts = [];
      const inP = formatModelPricePerM(p.prompt);
      const outP = formatModelPricePerM(p.completion);
      if (inP && outP) {
        const cached = formatModelPricePerM(p.input_cache_read);
        parts.push(inP + ' in · ' + outP + ' out /1M' + (cached ? ' · ' + cached + ' cached' : ''));
      } else {
        parts.push('price unknown');
      }
      const ctx = formatModelContext(info.context_length);
      if (ctx) parts.push(ctx);
      return parts.join(' · ');
    }

    // filterModelOptions keeps ids whose id or display name contains EVERY
    // whitespace-separated term of query (case-insensitive). An empty query
    // keeps everything.
    function filterModelOptions(ids, query, infoIdx) {
      const terms = String(query || '').toLowerCase().split(/\s+/).filter(Boolean);
      const list = Array.isArray(ids) ? ids : [];
      if (!terms.length) return list.slice();
      const idx = infoIdx || {};
      return list.filter(id => {
        const hay = (String(id) + ' ' + ((idx[id] && idx[id].name) || '')).toLowerCase();
        return terms.every(t => hay.indexOf(t) !== -1);
      });
    }

    // groupModelOptions buckets ids by provider prefix, groups sorted by name
    // and ids kept in their given order; bare ids land in "other", last.
    // Suggested ids (when given and present) form a first "Suggested" group
    // and are not repeated below.
    function groupModelOptions(ids, suggested) {
      const list = Array.isArray(ids) ? ids : [];
      const present = new Set(list);
      const pinned = (Array.isArray(suggested) ? suggested : [])
        .map(s => (s && typeof s === 'object') ? s.id : s)
        .filter(id => id && present.has(id));
      const pinnedSet = new Set(pinned);
      const byProvider = {};
      list.forEach(id => {
        if (pinnedSet.has(id)) return;
        const prov = modelProviderOf(id) || MODEL_GROUP_OTHER;
        (byProvider[prov] = byProvider[prov] || []).push(id);
      });
      const names = Object.keys(byProvider).sort((a, b) => {
        if (a === MODEL_GROUP_OTHER) return 1;
        if (b === MODEL_GROUP_OTHER) return -1;
        return a.localeCompare(b);
      });
      const groups = names.map(name => ({ provider: name, ids: byProvider[name] }));
      if (pinned.length) groups.unshift({ provider: MODEL_GROUP_SUGGESTED, ids: pinned, suggested: true });
      return groups;
    }

    // In-page state per combobox: its full id list, info index and
    // suggestions, so typing re-filters without another fetch.
    const _modelCombos = {};

    function _modelComboOptionId(comboId, i) { return comboId + '-opt-' + i; }

    // modelComboListHtml renders the listbox body for a query: grouped
    // options, then a custom entry for the typed text when it is not an exact
    // catalog id. Options carry data-value; the active one is aria-selected.
    function modelComboListHtml(comboId, query) {
      const st = _modelCombos[comboId] || { ids: [], info: {}, suggested: [] };
      const labels = {};
      (st.suggested || []).forEach(s => { if (s && s.id) labels[s.id] = s.label; });
      const matches = filterModelOptions(st.ids, query, st.info);
      const groups = groupModelOptions(matches, query ? [] : st.suggested);
      let n = 0;
      let html = '';
      groups.forEach(g => {
        html += `<li role="presentation" class="model-combo-group">${esc(g.provider)}</li>`;
        g.ids.forEach(id => {
          const detail = modelPriceLabel(st.info[id]);
          const name = labels[id] || id;
          html += `<li role="option" id="${esc(_modelComboOptionId(comboId, n))}" tabindex="-1" class="model-combo-option" aria-selected="false" data-value="${esc(id)}" data-action="modelComboPick" data-arg0="${esc(comboId)}" data-arg1="${esc(id)}">`
            + `<span class="model-combo-name">${esc(name)}</span>`
            + (detail ? `<span class="model-combo-detail">${esc(detail)}</span>` : '')
            + '</li>';
          n++;
        });
      });
      const typed = String(query || '').trim();
      const exact = typed && st.ids.indexOf(typed) !== -1;
      const customText = typed && !exact ? `Use “${typed}” as a custom model id` : 'Custom… (type any model id)';
      html += `<li role="option" id="${esc(_modelComboOptionId(comboId, n))}" tabindex="-1" class="model-combo-option model-combo-custom" aria-selected="false" data-value="${esc(typed && !exact ? typed : '')}" data-custom="1" data-action="modelComboPick" data-arg0="${esc(comboId)}" data-arg1="${esc(typed && !exact ? typed : '')}">${esc(customText)}</li>`;
      if (!matches.length && typed) {
        html = `<li role="presentation" class="model-combo-empty">No catalog model matches “${esc(typed)}”.</li>` + html;
      }
      return html;
    }

    // modelComboboxHtml renders the combobox and records its data. opts:
    // { id, ids, info (model_info array), suggested ([{id,label}]), current,
    //   placeholder, inputId, label }.
    function modelComboboxHtml(opts) {
      const o = opts || {};
      const comboId = o.id;
      _modelCombos[comboId] = {
        ids: Array.isArray(o.ids) ? o.ids.slice() : [],
        info: modelInfoIndex(o.info),
        suggested: Array.isArray(o.suggested) ? o.suggested : [],
      };
      const listId = comboId + '-list';
      const inputId = o.inputId || (comboId + '-input');
      const cur = o.current || '';
      const curDetail = modelPriceLabel(_modelCombos[comboId].info[cur]);
      return `<div class="model-combo" data-model-combo="${esc(comboId)}">`
        + `<input type="text" id="${esc(inputId)}" class="model-combo-input" role="combobox" aria-autocomplete="list" aria-expanded="false" aria-controls="${esc(listId)}"`
        + (o.label ? ` aria-label="${esc(o.label)}"` : '')
        + ` autocomplete="off" spellcheck="false" value="${esc(cur)}" placeholder="${esc(o.placeholder || 'Search models or type an id…')}"`
        + ` data-model-combo-input="${esc(comboId)}" data-input-action="modelComboInput" data-keydown-action="modelComboKeydown" data-keys="*" data-arg0="${esc(comboId)}" data-arg-types="s,e"`
        + ` data-action="modelComboOpen">`
        + `<ul id="${esc(listId)}" class="model-combo-list" role="listbox" hidden></ul>`
        + `<div class="model-combo-current" data-model-combo-detail="${esc(comboId)}">${esc(curDetail)}</div>`
        + '</div>';
    }

    function _modelComboEls(comboId) {
      const root = document.querySelector(`[data-model-combo="${CSS.escape(comboId)}"]`);
      if (!root) return null;
      return { root, input: root.querySelector('[role="combobox"]'), list: root.querySelector('[role="listbox"]'), detail: root.querySelector('[data-model-combo-detail]') };
    }

    function _modelComboSetOpen(els, open) {
      els.list.hidden = !open;
      els.input.setAttribute('aria-expanded', open ? 'true' : 'false');
      if (!open) els.input.removeAttribute('aria-activedescendant');
    }

    function _modelComboRender(comboId, query) {
      const els = _modelComboEls(comboId);
      if (!els) return null;
      els.list.innerHTML = modelComboListHtml(comboId, query);
      _modelComboSetOpen(els, true);
      return els;
    }

    // modelComboOpen opens the full (unfiltered) list on click.
    function modelComboOpen(comboId) {
      if (comboId) _modelComboRender(comboId, '');
    }

    function modelComboInput(comboId) {
      const els = _modelComboEls(comboId);
      if (!els) return;
      _modelComboRender(comboId, els.input.value);
      _modelComboUpdateDetail(comboId);
    }

    function _modelComboUpdateDetail(comboId) {
      const els = _modelComboEls(comboId);
      const st = _modelCombos[comboId];
      if (!els || !st || !els.detail) return;
      els.detail.textContent = modelPriceLabel(st.info[els.input.value.trim()]);
    }

    function _modelComboMove(els, delta) {
      const opts = Array.from(els.list.querySelectorAll('[role="option"]'));
      if (!opts.length) return;
      let i = opts.findIndex(o => o.getAttribute('aria-selected') === 'true');
      if (i >= 0) opts[i].setAttribute('aria-selected', 'false');
      i = i < 0 ? (delta > 0 ? 0 : opts.length - 1) : (i + delta + opts.length) % opts.length;
      opts[i].setAttribute('aria-selected', 'true');
      els.input.setAttribute('aria-activedescendant', opts[i].id);
      if (opts[i].scrollIntoView) opts[i].scrollIntoView({ block: 'nearest' });
    }

    function modelComboKeydown(comboId, e) {
      const els = _modelComboEls(comboId);
      if (!els || !e) return;
      if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
        e.preventDefault();
        if (els.list.hidden) _modelComboRender(comboId, els.input.value);
        _modelComboMove(els, e.key === 'ArrowDown' ? 1 : -1);
      } else if (e.key === 'Enter') {
        if (els.list.hidden) return;
        e.preventDefault();
        const active = els.list.querySelector('[role="option"][aria-selected="true"]');
        modelComboPick(comboId, active ? active.dataset.value : els.input.value.trim());
      } else if (e.key === 'Escape') {
        if (!els.list.hidden) { e.preventDefault(); e.stopPropagation(); _modelComboSetOpen(els, false); }
      }
    }

    // modelComboPick commits a value (a catalog id or typed custom text). An
    // empty custom pick clears the field and keeps focus for typing.
    function modelComboPick(comboId, value) {
      const els = _modelComboEls(comboId);
      if (!els) return;
      const v = String(value || '').trim();
      if (!v) {
        els.input.value = '';
        els.input.focus();
        _modelComboRender(comboId, '');
        return;
      }
      els.input.value = v;
      _modelComboSetOpen(els, false);
      _modelComboUpdateDetail(comboId);
      els.input.dispatchEvent(new Event('change', { bubbles: true }));
      els.input.focus();
    }

    // modelComboValue reads a combobox's committed text ('' when absent).
    function modelComboValue(comboId) {
      const els = _modelComboEls(comboId);
      return els ? els.input.value.trim() : '';
    }

    // Close an open list when focus leaves the combobox entirely (options are
    // tabindex=-1, so clicking one keeps focus inside and the pick lands).
    document.addEventListener('focusout', (e) => {
      const root = e.target && e.target.closest ? e.target.closest('[data-model-combo]') : null;
      if (!root || (e.relatedTarget && root.contains(e.relatedTarget))) return;
      const input = root.querySelector('[role="combobox"]');
      const list = root.querySelector('[role="listbox"]');
      if (input && list) _modelComboSetOpen({ input, list }, false);
    });

    // shouldGroupModelOptions: a native <select> (the per-agent model pickers,
    // which other code drives by value) gets <optgroup>s by provider once the
    // list is large and spans more than one provider.
    function shouldGroupModelOptions(values) {
      const list = Array.isArray(values) ? values : [];
      return list.length >= MODEL_COMBOBOX_MIN_OPTIONS && new Set(list.map(modelProviderOf)).size > 1;
    }

    // modelOptgroupsHtml renders values as provider <optgroup>s, each option
    // produced by render(value) so callers keep their own selected/label logic.
    function modelOptgroupsHtml(values, render) {
      return groupModelOptions(values).map(g =>
        `<optgroup label="${esc(g.provider)}">${g.ids.map(render).join('')}</optgroup>`).join('');
    }

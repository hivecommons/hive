    /* Review feedback rules editor (#19138): review_feedback.rules.*, loaded
       from the owner-only /api/config/review-feedback/rules endpoint and saved
       back to it. Rendered with DOM nodes and textContent only, so a phrase or
       a server string is never parsed as markup. The server validates the
       values and applies them without a restart; its error message is shown
       as-is and nothing is saved on an error. */
    const REVIEW_FEEDBACK_RULES_HOST_ID = 'cfg-review-feedback-rules-host';
    const REVIEW_FEEDBACK_RULES_URL = '/api/config/review-feedback/rules';

    function rfrNode(tag, className, text) {
      const node = document.createElement(tag);
      if (className) node.className = className;
      if (text !== undefined) node.textContent = text;
      return node;
    }

    async function loadReviewFeedbackRules() {
      let host = null;
      try {
        const r = await fetch(REVIEW_FEEDBACK_RULES_URL);
        host = document.getElementById(REVIEW_FEEDBACK_RULES_HOST_ID);
        if (!host) return;
        if (r.status === 403) {
          host.textContent = '';
          host.appendChild(rfrNode('div', 'fs-sm text-muted', 'Only the hive owner can view and edit review-feedback rules.'));
          return;
        }
        if (!r.ok) throw new Error('HTTP ' + r.status);
        renderReviewFeedbackRulesPanel(host, await r.json());
      } catch (e) {
        host = host || document.getElementById(REVIEW_FEEDBACK_RULES_HOST_ID);
        if (!host) return;
        host.textContent = '';
        host.appendChild(rfrNode('div', 'fs-sm text-danger', 'Failed to load review-feedback rules: ' + e.message));
      }
    }

    /* reviewFeedbackRulesPhrases turns the textarea into the phrase list: one
       phrase per line, blank lines dropped. */
    function reviewFeedbackRulesPhrases(text) {
      return String(text || '').split('\n').map(s => s.trim()).filter(Boolean);
    }

    function renderReviewFeedbackRulesPanel(host, data) {
      if (!host) return;
      data = data || {};
      const associations = Array.isArray(data.associations) ? data.associations : [];
      const signals = Array.isArray(data.signals) ? data.signals : [];
      const defaults = Array.isArray(data.default_trusted_associations) ? data.default_trusted_associations : [];
      /* An empty stored trusted list means "the default", so the boxes show the
         default set ticked; saving that set back sends an empty list. */
      const storedTrusted = Array.isArray(data.trusted_associations) ? data.trusted_associations : [];
      const state = {
        enabled: data.enabled !== false,
        trusted: new Set(storedTrusted.length ? storedTrusted : defaults),
        phrases: (Array.isArray(data.extra_phrases) ? data.extra_phrases : []).join('\n'),
        disabled: new Set(Array.isArray(data.disabled_signals) ? data.disabled_signals : []),
      };
      const saved = JSON.stringify([state.enabled, [...state.trusted].sort(), state.phrases, [...state.disabled].sort()]);
      const snapshot = () => JSON.stringify([state.enabled, [...state.trusted].sort(), state.phrases, [...state.disabled].sort()]);
      host.textContent = '';
      host.appendChild(rfrNode('p', 'config-help',
        'When someone reviewing a hive PR states a rule for later work, such as "X is the source of truth" or "going forward, ...", the hive records it in its knowledge and tells the reviewer on the PR. ' +
        'A rule from a reviewer whose GitHub association is not ticked below waits for a person to approve it. ' +
        'Saving applies the change on the next sweep pass, without a restart.'));
      if (data.enabled_from_env) {
        host.appendChild(rfrNode('p', 'config-help text-danger',
          'The HIVE_REVIEW_FEEDBACK_RULES environment variable is set, so it decides whether capture runs (currently ' + (data.effective_enabled ? 'on' : 'off') + '); the switch below is saved but has no effect until the variable is removed.'));
      }
      if (data.trusted_from_env) {
        host.appendChild(rfrNode('p', 'config-help text-danger',
          'The HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS environment variable is set (currently ' + (data.effective_trusted_associations || []).join(', ') + '), so it decides whose rules count at once; the boxes below are saved but have no effect until the variable is removed.'));
      }

      const statusEl = rfrNode('div', 'fs-sm');
      const setStatus = (msg, cls) => { statusEl.textContent = msg; statusEl.className = 'fs-sm ' + (cls || 'text-muted'); };
      const saveBtn = rfrNode('button', 'hv-btn btn-primary btn-sm', 'Save review-feedback rules');
      const syncSave = () => { saveBtn.disabled = snapshot() === saved; };

      const checkRow = (label, hint, checked, onChange) => {
        const row = rfrNode('label', 'config-toggle');
        const box = document.createElement('input');
        box.type = 'checkbox';
        box.checked = checked;
        box.addEventListener('change', () => { onChange(box.checked); setStatus(''); syncSave(); });
        row.appendChild(box);
        const text = rfrNode('span', 'config-toggle-label', label);
        if (hint) text.appendChild(rfrNode('span', 'text-muted fs-sm', ' ' + hint));
        row.appendChild(text);
        return row;
      };

      host.appendChild(checkRow('Capture rules reviewers state', '', state.enabled, v => { state.enabled = v; }));

      host.appendChild(rfrNode('div', 'config-subgroup', 'Rules count at once from'));
      const trustedList = rfrNode('div', 'config-list');
      associations.forEach(a => {
        trustedList.appendChild(checkRow(a, '', state.trusted.has(a), v => { if (v) state.trusted.add(a); else state.trusted.delete(a); }));
      });
      host.appendChild(trustedList);

      host.appendChild(rfrNode('div', 'config-subgroup', 'Extra phrases that mark a comment as a rule (one per line)'));
      const area = document.createElement('textarea');
      area.rows = 3;
      area.value = state.phrases;
      area.placeholder = 'e.g. house rule';
      area.spellcheck = false;
      area.setAttribute('aria-label', 'Extra phrases that mark a comment as a rule');
      area.addEventListener('input', () => { state.phrases = area.value; setStatus(''); syncSave(); });
      host.appendChild(area);
      host.appendChild(rfrNode('div', 'config-help', 'Matched case-insensitively as whole words, never as patterns. Quoted lines, code blocks and questions are ignored.'));

      host.appendChild(rfrNode('div', 'config-subgroup', 'Built-in signals (untick to stop one from counting)'));
      const signalList = rfrNode('div', 'config-list');
      signals.forEach(sig => {
        signalList.appendChild(checkRow(sig.name, sig.description, !state.disabled.has(sig.name), v => { if (v) state.disabled.delete(sig.name); else state.disabled.add(sig.name); }));
      });
      host.appendChild(signalList);

      const hints = rfrNode('div', 'config-help');
      const hint = rfrNode('span', 'settings-yaml-hint', 'YAML ');
      hint.appendChild(rfrNode('code', '', 'review_feedback.rules.enabled | trusted_associations | extra_phrases | disabled_signals'));
      hints.appendChild(hint);
      host.appendChild(hints);

      const actions = rfrNode('div', 'config-list-add');
      actions.appendChild(saveBtn);
      actions.appendChild(statusEl);
      host.appendChild(actions);
      syncSave();

      saveBtn.addEventListener('click', async () => {
        const trusted = [...state.trusted].filter(a => associations.includes(a));
        /* An empty list means "the default" to the server, so an owner who
           unticked everything would silently get it back. Ask instead. */
        if (!trusted.length) { setStatus('Tick at least one association, or keep the default set.', 'text-danger'); return; }
        saveBtn.disabled = true;
        setStatus('Saving…');
        const sameAsDefault = trusted.length === defaults.length && defaults.every(a => state.trusted.has(a));
        try {
          const res = await fetch(REVIEW_FEEDBACK_RULES_URL, {
            method: 'PUT',
            headers: _inceptionAuthHeaders(),
            body: JSON.stringify({
              enabled: state.enabled,
              trusted_associations: sameAsDefault ? [] : trusted,
              extra_phrases: reviewFeedbackRulesPhrases(state.phrases),
              disabled_signals: [...state.disabled],
            }),
          });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          renderReviewFeedbackRulesPanel(host, await res.json());
          showToast('Review-feedback rules saved', 'success');
        } catch (e) {
          setStatus(e.message, 'text-danger');
          syncSave();
        }
      });
    }

    /* Reviewer-stated rules editor (#19135): review_feedback.rules.enabled,
       review_feedback.rules.trusted_associations,
       review_feedback.rules.extra_phrases and
       review_feedback.rules.disabled_signals, loaded from the owner-only
       /api/config/review-feedback/rules endpoint and saved back to it.
       Rendered with DOM nodes and textContent only, so a phrase or a signal
       name is never parsed as markup. The server validates and applies the
       values on the sweep's next pass without a restart; its error message is
       shown as-is and nothing is saved on an error. */
    const REVIEW_RULES_HOST_ID = 'cfg-review-rules-host';

    function reviewRulesNode(tag, className, text) {
      const node = document.createElement(tag);
      if (className) node.className = className;
      if (text !== undefined) node.textContent = text;
      return node;
    }

    async function loadReviewRulesSettings() {
      let host = null;
      try {
        const r = await fetch('/api/config/review-feedback/rules');
        host = document.getElementById(REVIEW_RULES_HOST_ID);
        if (!host) return;
        if (r.status === 403) {
          host.textContent = '';
          host.appendChild(reviewRulesNode('div', 'fs-sm text-muted', 'Only the hive owner can view and edit reviewer-stated rule settings.'));
          return;
        }
        if (!r.ok) throw new Error('HTTP ' + r.status);
        renderReviewRulesPanel(host, await r.json());
      } catch (e) {
        host = host || document.getElementById(REVIEW_RULES_HOST_ID);
        if (!host) return;
        host.textContent = '';
        host.appendChild(reviewRulesNode('div', 'fs-sm text-danger', 'Failed to load reviewer-stated rule settings: ' + e.message));
      }
    }

    /* reviewRulesSourceText says where an effective value comes from. */
    function reviewRulesSourceText(field) {
      if (!field) return '';
      if (field.source === 'settings') return 'saved here';
      if (field.source === 'env') return 'from ' + (field.env_var || 'the environment');
      return 'default';
    }

    /* renderReviewRulesPanel paints the four preferences, each with a one-line
       explanation, its default and where the current value comes from. A
       blank list falls back to the env var, then the default. */
    function renderReviewRulesPanel(host, data) {
      if (!host) return;
      data = data || {};
      const enabled = data.enabled || {};
      const trusted = data.trusted_associations || {};
      const phrases = data.extra_phrases || {};
      const disabled = data.disabled_signals || {};
      const associations = Array.isArray(data.associations) ? data.associations : [];
      const signals = Array.isArray(data.signals) ? data.signals : [];
      const list = v => (Array.isArray(v) ? v : []);
      const state = {
        enabled: enabled.saved === null || enabled.saved === undefined ? '' : (enabled.saved ? 'on' : 'off'),
        trusted: new Set(list(trusted.saved === null || trusted.saved === undefined ? trusted.effective : trusted.saved)),
        trustedSaved: Array.isArray(trusted.saved),
        phrases: list(phrases.saved).join('\n'),
        disabled: new Set(list(disabled.saved === null || disabled.saved === undefined ? disabled.effective : disabled.saved)),
        disabledSaved: Array.isArray(disabled.saved),
      };

      host.textContent = '';
      host.appendChild(reviewRulesNode('p', 'config-help',
        'When a person reviewing one of the hive\'s own PRs states a rule or a source of truth ("X is the source of truth", "in future assessments, check Y"), ' +
        'the hive records it in its knowledge, replies "Recorded as rule R-n" on the PR, and gives it to the authoring agent on its next run. ' +
        'Retract a rule with hivectl knowledge state rule-r-N deprecated. Saving applies on the next sweep pass without a restart.'));

      const statusEl = reviewRulesNode('div', 'fs-sm');
      const setStatus = (msg, cls) => { statusEl.textContent = msg; statusEl.className = 'fs-sm ' + (cls || 'text-muted'); };
      const saveBtn = reviewRulesNode('button', 'hv-btn btn-primary btn-sm', 'Save reviewer-rule settings');
      const dirty = () => { saveBtn.disabled = false; setStatus(''); };

      const yamlLine = key => {
        const hint = reviewRulesNode('span', 'settings-yaml-hint', 'YAML ');
        hint.appendChild(reviewRulesNode('code', '', key));
        return hint;
      };
      const field = (label, help, yamlKey, now) => {
        const box = reviewRulesNode('div', 'config-field mt-3');
        const l = reviewRulesNode('label', '', label + ' ');
        l.appendChild(yamlLine(yamlKey));
        box.appendChild(l);
        box.appendChild(reviewRulesNode('div', 'config-help', help));
        box.appendChild(reviewRulesNode('div', 'rt-inherit', now));
        host.appendChild(box);
        return box;
      };

      /* Capture on/off */
      const enBox = field('Capture reviewer-stated rules',
        'On: rules reviewers state on the hive\'s PRs are recorded. Off: they are not recorded; the sweep still routes the feedback. Default: on.',
        'review_feedback.rules.enabled',
        'now: ' + (enabled.effective === false ? 'off' : 'on') + ' (' + reviewRulesSourceText(enabled) + ')');
      const enSelect = document.createElement('select');
      enSelect.title = 'Capture reviewer-stated rules';
      [['', 'Use default (' + (enabled.env_var || 'env') + ', else on)'], ['on', 'On'], ['off', 'Off']].forEach(([v, text]) => {
        const opt = document.createElement('option');
        opt.value = v;
        opt.textContent = text;
        opt.selected = v === state.enabled;
        enSelect.appendChild(opt);
      });
      enSelect.addEventListener('change', () => { state.enabled = enSelect.value; dirty(); });
      enBox.appendChild(enSelect);

      /* Trusted associations */
      const trBox = field('Recorded at once for',
        'GitHub author associations whose rules are recorded at once; anyone else\'s rule is queued for you to confirm. ' +
        'An org member whose membership is private shows as CONTRIBUTOR. Default: ' + list(trusted.default).join(', ') + '.',
        'review_feedback.rules.trusted_associations[]',
        'now: ' + list(trusted.effective).join(', ') + ' (' + reviewRulesSourceText(trusted) + ')');
      const trRow = reviewRulesNode('div', 'config-list');
      associations.forEach(a => {
        const lab = reviewRulesNode('label', 'config-list-item');
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.checked = state.trusted.has(a);
        cb.addEventListener('change', () => {
          if (cb.checked) state.trusted.add(a); else state.trusted.delete(a);
          state.trustedSaved = true;
          dirty();
        });
        lab.appendChild(cb);
        lab.appendChild(reviewRulesNode('code', '', a));
        trRow.appendChild(lab);
      });
      trBox.appendChild(trRow);
      const trReset = reviewRulesNode('button', 'hv-btn btn-secondary btn-sm', 'Use default');
      trReset.addEventListener('click', () => {
        state.trustedSaved = false;
        state.trusted = new Set(list(trusted.default));
        trRow.querySelectorAll('input[type=checkbox]').forEach((cb, i) => { cb.checked = state.trusted.has(associations[i]); });
        dirty();
      });
      trBox.appendChild(trReset);

      /* Extra phrases */
      const phBox = field('Extra phrases that mark a rule',
        'One plain phrase per line, matched case-insensitively as whole words, on top of the built-in signals below (for example: worth considering). ' +
        'Quoted text and questions never count. At most ' + (data.max_phrases || 20) + '. Default: none.',
        'review_feedback.rules.extra_phrases[]',
        'now: ' + (list(phrases.effective).join(' | ') || 'none') + ' (' + reviewRulesSourceText(phrases) + ')');
      const ta = document.createElement('textarea');
      ta.rows = 3;
      ta.value = state.phrases;
      ta.placeholder = 'worth considering';
      ta.addEventListener('input', () => { state.phrases = ta.value; dirty(); });
      phBox.appendChild(ta);

      /* Built-in signals */
      const sgBox = field('Built-in signals',
        'Uncheck a signal to stop it marking a comment as a rule. Default: all on.',
        'review_feedback.rules.disabled_signals[]',
        'now off: ' + (list(disabled.effective).join(', ') || 'none') + ' (' + reviewRulesSourceText(disabled) + ')');
      const sgRow = reviewRulesNode('div', 'config-list');
      signals.forEach(sg => {
        const lab = reviewRulesNode('label', 'config-list-item');
        const cb = document.createElement('input');
        cb.type = 'checkbox';
        cb.checked = !state.disabled.has(sg.name);
        cb.addEventListener('change', () => {
          if (cb.checked) state.disabled.delete(sg.name); else state.disabled.add(sg.name);
          state.disabledSaved = true;
          dirty();
        });
        lab.appendChild(cb);
        lab.appendChild(reviewRulesNode('code', '', sg.name));
        lab.appendChild(reviewRulesNode('span', 'text-muted fs-sm', ' ' + (sg.examples || '')));
        sgRow.appendChild(lab);
      });
      sgBox.appendChild(sgRow);

      const actions = reviewRulesNode('div', 'config-list-add');
      actions.appendChild(saveBtn);
      actions.appendChild(statusEl);
      host.appendChild(actions);
      saveBtn.disabled = true;

      saveBtn.addEventListener('click', async () => {
        const body = {};
        if (state.enabled) body.enabled = state.enabled === 'on';
        if (state.trustedSaved) body.trusted_associations = associations.filter(a => state.trusted.has(a));
        const ph = state.phrases.split('\n').map(s => s.trim()).filter(Boolean);
        if (ph.length) body.extra_phrases = ph;
        if (state.disabledSaved && state.disabled.size) body.disabled_signals = Array.from(state.disabled);
        saveBtn.disabled = true;
        setStatus('Saving…');
        try {
          const res = await fetch('/api/config/review-feedback/rules', {
            method: 'PUT',
            headers: _inceptionAuthHeaders(),
            body: JSON.stringify(body),
          });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          renderReviewRulesPanel(host, await res.json());
          showToast('Reviewer-rule settings saved', 'success');
        } catch (e) {
          saveBtn.disabled = false;
          setStatus(e.message, 'text-danger');
        }
      });
    }

    /* Stranded review-feedback retry editor (#19320):
       review_feedback.retry.max_retries and review_feedback.retry.backoff,
       loaded from the owner-only /api/config/review-feedback/retry endpoint
       and saved back to it. Rendered with DOM nodes and textContent only. The
       server validates and applies the values on the sweep's next pass; its
       error message is shown as-is and nothing is saved on an error. */
    const REVIEW_RETRY_HOST_ID = 'cfg-review-retry-host';

    function reviewRetryNode(tag, className, text) {
      const node = document.createElement(tag);
      if (className) node.className = className;
      if (text !== undefined) node.textContent = text;
      return node;
    }

    async function loadReviewRetrySettings() {
      let host = null;
      try {
        const r = await fetch('/api/config/review-feedback/retry');
        host = document.getElementById(REVIEW_RETRY_HOST_ID);
        if (!host) return;
        if (r.status === 403) {
          host.textContent = '';
          host.appendChild(reviewRetryNode('div', 'fs-sm text-muted', 'Only the hive owner can view and edit stranded-feedback retry settings.'));
          return;
        }
        if (!r.ok) throw new Error('HTTP ' + r.status);
        renderReviewRetryPanel(host, await r.json());
      } catch (e) {
        host = host || document.getElementById(REVIEW_RETRY_HOST_ID);
        if (!host) return;
        host.textContent = '';
        host.appendChild(reviewRetryNode('div', 'fs-sm text-danger', 'Failed to load stranded-feedback retry settings: ' + e.message));
      }
    }

    /* reviewRetrySourceText says where an effective value comes from. */
    function reviewRetrySourceText(field) {
      if (!field) return '';
      if (field.source === 'settings') return 'saved here';
      if (field.source === 'env') return 'from ' + (field.env_var || 'the environment');
      return 'default';
    }

    /* renderReviewRetryPanel paints the two preferences, each with a one-line
       explanation, its default and where the current value comes from. A
       blank input falls back to the env var, then the default. */
    function renderReviewRetryPanel(host, data) {
      if (!host) return;
      data = data || {};
      const maxR = data.max_retries || {};
      const back = data.backoff || {};
      const state = {
        maxRetries: maxR.saved === null || maxR.saved === undefined ? '' : String(maxR.saved),
        backoff: back.saved === null || back.saved === undefined ? '' : String(back.saved),
      };

      host.textContent = '';
      host.appendChild(reviewRetryNode('p', 'config-help',
        'When review feedback on one of the hive\'s PRs is routed to its agent and the run ends with no push and no reply on the PR, ' +
        'the hive routes the same comments again after a wait that doubles each time, then posts a needs-human note on the PR saying why. ' +
        'Saving applies on the next sweep pass without a restart.'));

      const statusEl = reviewRetryNode('div', 'fs-sm');
      const setStatus = (msg, cls) => { statusEl.textContent = msg; statusEl.className = 'fs-sm ' + (cls || 'text-muted'); };
      const saveBtn = reviewRetryNode('button', 'hv-btn btn-primary btn-sm', 'Save retry settings');
      const dirty = () => { saveBtn.disabled = false; setStatus(''); };

      const field = (label, help, yamlKey, now) => {
        const box = reviewRetryNode('div', 'config-field mt-3');
        const l = reviewRetryNode('label', '', label + ' ');
        const hint = reviewRetryNode('span', 'settings-yaml-hint', 'YAML ');
        hint.appendChild(reviewRetryNode('code', '', yamlKey));
        l.appendChild(hint);
        box.appendChild(l);
        box.appendChild(reviewRetryNode('div', 'config-help', help));
        box.appendChild(reviewRetryNode('div', 'rt-inherit', now));
        host.appendChild(box);
        return box;
      };

      const mrBox = field('Retries before asking a person',
        'How many times stranded feedback is routed to the agent again. 0 asks a person after the first wait. ' +
        'At most ' + (data.max_max_retries || 10) + '. Blank uses ' + (maxR.env_var || 'the env var') + ', else ' + maxR.default + '.',
        'review_feedback.retry.max_retries',
        'now: ' + maxR.effective + ' (' + reviewRetrySourceText(maxR) + ')');
      const mrInput = document.createElement('input');
      mrInput.type = 'number';
      mrInput.min = '0';
      mrInput.max = String(data.max_max_retries || 10);
      mrInput.title = 'Retries before asking a person';
      mrInput.value = state.maxRetries;
      mrInput.placeholder = String(maxR.default);
      mrInput.addEventListener('input', () => { state.maxRetries = mrInput.value; dirty(); });
      mrBox.appendChild(mrInput);

      const bkBox = field('First wait',
        'How long after a kick the hive waits before judging it stranded; each retry doubles it. A duration such as 45m or 2h, at least ' +
        (data.min_backoff || '10m') + '. Blank uses ' + (back.env_var || 'the env var') + ', else ' + back.default + '.',
        'review_feedback.retry.backoff',
        'now: ' + back.effective + ' (' + reviewRetrySourceText(back) + ')');
      const bkInput = document.createElement('input');
      bkInput.type = 'text';
      bkInput.title = 'First wait';
      bkInput.value = state.backoff;
      bkInput.placeholder = String(back.default);
      bkInput.addEventListener('input', () => { state.backoff = bkInput.value; dirty(); });
      bkBox.appendChild(bkInput);

      const actions = reviewRetryNode('div', 'config-list-add');
      actions.appendChild(saveBtn);
      actions.appendChild(statusEl);
      host.appendChild(actions);
      saveBtn.disabled = true;

      saveBtn.addEventListener('click', async () => {
        const body = {};
        const mr = String(state.maxRetries).trim();
        if (mr !== '') {
          const n = Number(mr);
          if (!Number.isInteger(n)) {
            setStatus('Retries must be a whole number', 'text-danger');
            return;
          }
          body.max_retries = n;
        }
        const bk = String(state.backoff).trim();
        if (bk !== '') body.backoff = bk;
        saveBtn.disabled = true;
        setStatus('Saving…');
        try {
          const res = await fetch('/api/config/review-feedback/retry', {
            method: 'PUT',
            headers: _inceptionAuthHeaders(),
            body: JSON.stringify(body),
          });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          renderReviewRetryPanel(host, await res.json());
          showToast('Retry settings saved', 'success');
        } catch (e) {
          saveBtn.disabled = false;
          setStatus(e.message, 'text-danger');
        }
      });
    }

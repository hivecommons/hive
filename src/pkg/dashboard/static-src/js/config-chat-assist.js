    /* Chat config assist panel (RFC #19788, issues #19979 and #20092):
       chat.config_assist.enabled and chat.config_assist.model, loaded from
       and saved to the owner-only /api/config/chat/config-assist endpoint
       (GET and PUT), so other roles get the owner-only note instead of
       controls that would 403 on save. Off by default. Rendered with DOM
       nodes and textContent only. A save persists and is audited
       (config_chat_assist); nothing is saved on an error. */
    const CHAT_ASSIST_HOST_ID = 'cfg-chat-assist-host';

    function chatAssistNode(tag, className, text) {
      const node = document.createElement(tag);
      if (className) node.className = className;
      if (text !== undefined) node.textContent = text;
      return node;
    }

    async function loadChatConfigAssistSettings() {
      let host = null;
      try {
        const r = await fetch('/api/config/chat/config-assist');
        host = document.getElementById(CHAT_ASSIST_HOST_ID);
        if (!host) return;
        if (r.status === 403) {
          host.textContent = '';
          host.appendChild(chatAssistNode('div', 'fs-sm text-muted', 'Only the hive owner can view and edit chat config assist.'));
          return;
        }
        if (!r.ok) throw new Error('HTTP ' + r.status);
        renderChatConfigAssistPanel(host, await r.json());
      } catch (e) {
        host = host || document.getElementById(CHAT_ASSIST_HOST_ID);
        if (!host) return;
        host.textContent = '';
        host.appendChild(chatAssistNode('div', 'fs-sm text-danger', 'Failed to load the chat config assist toggle: ' + e.message));
      }
    }

    /* renderChatConfigAssistPanel paints the toggle, the proposal model
       picker (empty falls back to chat.grounded_answers.model, then the
       reviewer model) and whether a grounded-answers runner is attached (the
       panel still saves either way; it just has nothing to propose with
       until one is). */
    function renderChatConfigAssistPanel(host, data) {
      if (!host) return;
      data = data || {};
      const state = { enabled: data.enabled === true, model: typeof data.model === 'string' ? data.model : '' };

      host.textContent = '';
      host.appendChild(chatAssistNode('p', 'config-help',
        'Lets Hive Chat propose config changes (model, reasoning effort, cadence) for you to confirm, instead of only answering questions. Off by default.'));
      if (!data.runner_attached) {
        host.appendChild(chatAssistNode('p', 'fs-sm text-muted', 'No grounded-answers runner is attached yet, so turning this on has nothing to propose with until one is.'));
      }

      const toggleRow = chatAssistNode('div', 'config-toggle');
      const switchEl = chatAssistNode('div', 'config-toggle-switch' + (state.enabled ? ' on' : ''));
      const label = chatAssistNode('span', 'config-toggle-label', 'Chat config assist enabled ');
      const hint = chatAssistNode('span', 'settings-yaml-hint', 'YAML ');
      hint.appendChild(chatAssistNode('code', '', 'chat.config_assist.enabled'));
      label.appendChild(hint);
      toggleRow.appendChild(switchEl);
      toggleRow.appendChild(label);
      host.appendChild(toggleRow);

      const modelField = chatAssistNode('div', 'config-field');
      const modelLabel = chatAssistNode('label', '', 'Proposal model ');
      const modelHint = chatAssistNode('span', 'settings-yaml-hint', 'YAML ');
      modelHint.appendChild(chatAssistNode('code', '', 'chat.config_assist.model'));
      modelLabel.appendChild(modelHint);
      const modelInput = chatAssistNode('input');
      modelInput.type = 'text';
      modelInput.setAttribute('list', 'security-model-options');
      modelInput.value = state.model;
      modelInput.placeholder = data.fallback_model ? 'empty uses ' + data.fallback_model + ' (chat.grounded_answers.model)' : 'empty uses the reviewer model';
      modelField.appendChild(modelLabel);
      modelField.appendChild(modelInput);
      modelField.appendChild(chatAssistNode('div', 'config-help', 'The model the proposal turn runs on. Empty falls back to chat.grounded_answers.model, then the governor reviewer model.'));
      host.appendChild(modelField);

      const statusEl = chatAssistNode('div', 'fs-sm');
      const setStatus = (msg, cls) => { statusEl.textContent = msg; statusEl.className = 'fs-sm ' + (cls || 'text-muted'); };
      const saveBtn = chatAssistNode('button', 'hv-btn btn-primary btn-sm', 'Save');
      saveBtn.disabled = true;

      switchEl.addEventListener('click', () => {
        state.enabled = !state.enabled;
        switchEl.classList.toggle('on', state.enabled);
        saveBtn.disabled = false;
        setStatus('');
      });
      modelInput.addEventListener('input', () => {
        state.model = modelInput.value.trim();
        saveBtn.disabled = false;
        setStatus('');
      });

      const actions = chatAssistNode('div', 'config-list-add');
      actions.appendChild(saveBtn);
      actions.appendChild(statusEl);
      host.appendChild(actions);

      saveBtn.addEventListener('click', async () => {
        saveBtn.disabled = true;
        setStatus('Saving…');
        try {
          const res = await fetch('/api/config/chat/config-assist', {
            method: 'PUT',
            headers: _inceptionAuthHeaders(),
            body: JSON.stringify({ enabled: state.enabled, model: state.model }),
          });
          if (!res.ok) throw new Error(await saveErrorMessage(res));
          await res.json();
          await loadChatConfigAssistSettings();
          showToast('Chat config assist ' + (state.enabled ? 'enabled' : 'disabled'), 'success');
        } catch (e) {
          saveBtn.disabled = false;
          setStatus(e.message, 'text-danger');
        }
      });
    }

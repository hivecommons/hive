    /* Chat config assist toggle, phase 1 (RFC #19788, issue #19979):
       chat.config_assist.enabled, loaded from and saved to the owner-only
       /api/config/chat/config-assist endpoint. Off by default. Rendered with
       DOM nodes and textContent only. The toggle persists and is audited
       (config_chat_assist) on save; nothing is saved on an error. */
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
          host.appendChild(chatAssistNode('div', 'fs-sm text-muted', 'Only the hive owner can view and edit the chat config assist toggle.'));
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

    /* renderChatConfigAssistPanel paints the single toggle plus whether a
       grounded-answers runner is attached (the toggle still saves either way;
       it just has nothing to propose yet without one). */
    function renderChatConfigAssistPanel(host, data) {
      if (!host) return;
      data = data || {};
      const state = { enabled: data.enabled === true };

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
            body: JSON.stringify({ enabled: state.enabled }),
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

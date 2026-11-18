    /* Parent auto-close editor (#19070): repos.<repo>.parent_autoclose, one
       choice per monitored repo, loaded from the owner-only
       /api/config/repos/parent-autoclose endpoint and saved back to it.
       Rendered with DOM nodes and textContent only, so a repo name or an
       option string is never parsed as markup. The server validates the value
       and applies it without a restart; its error message is shown as-is and
       nothing is saved on an error. */
    const PARENT_AUTOCLOSE_HOST_ID = 'cfg-parent-autoclose-host';

    function parentAutoCloseNode(tag, className, text) {
      const node = document.createElement(tag);
      if (className) node.className = className;
      if (text !== undefined) node.textContent = text;
      return node;
    }

    async function loadParentAutoClose() {
      let host = null;
      try {
        const r = await fetch('/api/config/repos/parent-autoclose');
        host = document.getElementById(PARENT_AUTOCLOSE_HOST_ID);
        if (!host) return;
        if (r.status === 403) {
          host.textContent = '';
          host.appendChild(parentAutoCloseNode('div', 'fs-sm text-muted', 'Only the hive owner can view and edit parent auto-close.'));
          return;
        }
        if (!r.ok) throw new Error('HTTP ' + r.status);
        renderParentAutoClosePanel(host, await r.json());
      } catch (e) {
        host = host || document.getElementById(PARENT_AUTOCLOSE_HOST_ID);
        if (!host) return;
        host.textContent = '';
        host.appendChild(parentAutoCloseNode('div', 'fs-sm text-danger', 'Failed to load parent auto-close: ' + e.message));
      }
    }

    /* renderParentAutoClosePanel paints one row per monitored repo: a select
       with the three options, the effective value, and the one-line
       explanation of the chosen option. Save sends only the rows that
       changed. */
    function renderParentAutoClosePanel(host, data) {
      if (!host) return;
      data = data || {};
      const options = Array.isArray(data.options) ? data.options : [];
      const repos = (Array.isArray(data.repos) ? data.repos : []).map(r => ({
        repo: r.repo || '', saved: r.value || data.default || 'true', value: r.value || data.default || 'true', explicit: !!r.explicit,
      }));
      const labelOf = v => { const o = options.find(x => x.value === v); return o ? o.label : v; };
      const textOf = v => { const o = options.find(x => x.value === v); return o ? o.description : ''; };
      host.textContent = '';
      host.appendChild(parentAutoCloseNode('p', 'config-help',
        'When every child of a split-up parent issue is closed, the hive can close the parent too. Choose, per repo, how far that goes. ' +
        'A parent that a person reopened after the hive closed it is never closed again, whatever you choose. ' +
        'Issues marked as tracking issues (the hive/tracking label, the hive:tracking marker or "Keep this issue open") are never auto-closed; only you closing them from the dashboard does that. ' +
        'Saving applies the change without a restart.'));
      const list = parentAutoCloseNode('div', 'config-list');
      host.appendChild(list);
      const statusEl = parentAutoCloseNode('div', 'fs-sm');
      const setStatus = (msg, cls) => { statusEl.textContent = msg; statusEl.className = 'fs-sm ' + (cls || 'text-muted'); };
      const saveBtn = parentAutoCloseNode('button', 'hv-btn btn-primary btn-sm', 'Save parent auto-close');

      const dirty = () => repos.filter(r => r.value !== r.saved);
      const syncSave = () => { saveBtn.disabled = dirty().length === 0; };

      const paintRows = () => {
        list.textContent = '';
        if (!repos.length) list.appendChild(parentAutoCloseNode('div', 'fs-sm text-muted mb-3', 'No monitored repos configured'));
        repos.forEach(entry => {
          const row = parentAutoCloseNode('div', 'config-list-item config-repo-row');
          row.appendChild(parentAutoCloseNode('span', 'config-repo-name', entry.repo));
          const select = document.createElement('select');
          select.title = 'Parent auto-close policy for ' + entry.repo;
          options.forEach(o => {
            const opt = document.createElement('option');
            opt.value = o.value;
            opt.textContent = o.label;
            opt.selected = o.value === entry.value;
            select.appendChild(opt);
          });
          const note = parentAutoCloseNode('span', 'text-muted fs-sm', textOf(entry.value));
          select.addEventListener('change', () => {
            entry.value = select.value;
            note.textContent = textOf(entry.value);
            setStatus('');
            syncSave();
          });
          row.appendChild(select);
          row.appendChild(parentAutoCloseNode('span', 'rt-inherit', 'now: ' + labelOf(entry.saved) + (entry.explicit ? '' : ' (inherited)')));
          row.appendChild(note);
          list.appendChild(row);
        });
      };

      const hints = parentAutoCloseNode('div', 'config-help');
      const hint = parentAutoCloseNode('span', 'settings-yaml-hint', 'YAML ');
      hint.appendChild(parentAutoCloseNode('code', '', 'repos.<key>.parent_autoclose'));
      hints.appendChild(hint);
      host.appendChild(hints);

      const actions = parentAutoCloseNode('div', 'config-list-add');
      actions.appendChild(saveBtn);
      actions.appendChild(statusEl);
      host.appendChild(actions);
      paintRows();
      syncSave();

      saveBtn.addEventListener('click', async () => {
        const pending = dirty();
        if (!pending.length) return;
        saveBtn.disabled = true;
        setStatus('Saving…');
        try {
          for (const entry of pending) {
            const res = await fetch('/api/config/repos/parent-autoclose', {
              method: 'PUT',
              headers: _inceptionAuthHeaders(),
              body: JSON.stringify({ repo: entry.repo, value: entry.value }),
            });
            if (!res.ok) throw new Error(entry.repo + ': ' + await saveErrorMessage(res));
            const d = await res.json();
            const fresh = (d.repos || []).find(x => x.repo === entry.repo);
            entry.saved = entry.value;
            entry.explicit = fresh ? !!fresh.explicit : entry.value !== (d.default || 'true');
          }
          paintRows();
          setStatus('');
          showToast('Parent auto-close saved', 'success');
        } catch (e) {
          paintRows();
          setStatus(e.message, 'text-danger');
        }
        syncSave();
      });
    }

    // ── Inbox: delete any item (hivecommons/hive#18694) ──
    // 🗑 Delete on every item and Delete… in the bulk menu, through the
    // in-page dialog. POST /api/inbox/delete {items, remove_knowledge}. What
    // a delete removes depends on where the item lives:
    //   recording, upload, email: this hive stores the file, so it goes;
    //   meeting: the Inbox item only (the record stays under Meetings);
    //   synced sources: the Inbox item only, tombstoned so the next sync
    //   does not bring it back; the recording stays with its source.
    // The Knowledge copy is kept unless the box in the dialog is ticked, and
    // only a copy this hive wrote itself (recording, upload) can be removed.
    const INBOX_DELETE_KNOWLEDGE_LABEL = 'Also remove the Knowledge copy';

    function inboxDeleteKind(it) {
      const src = (it && it.source) || '';
      if (src === 'hive-recording') return 'recording';
      if (src === 'upload' || src === 'email' || src === 'meeting') return src;
      return 'synced';
    }
    // The Knowledge copy of an item can be removed with it only when this
    // hive wrote the fact itself; a synced source's fact belongs to its sync.
    function inboxDeleteKnowledgeRemovable(it) {
      const kind = inboxDeleteKind(it);
      return !!(it && it.fact_slug) && (kind === 'recording' || kind === 'upload');
    }
    // inboxCanDelete: owner or read-write. A hive recording keeps its own
    // rule (its recorder, or an owner); a mailed-in item that still waits for
    // approval is the owner's to decide.
    function inboxCanDelete(it, role, user) {
      if (!it || (role !== 'owner' && role !== 'read-write')) return false;
      if (it.awaiting_approval) return role === 'owner';
      if (inboxDeleteKind(it) !== 'recording' || role === 'owner') return true;
      return !!user && !!it.recorded_by && String(it.recorded_by).toLowerCase() === String(user).toLowerCase();
    }
    function inboxDeleteBusy(it) {
      const st = it && it.transcription;
      return !!st && (st.status === 'queued' || st.status === 'running');
    }
    // inboxDeleteRemovesText says what one item's delete removes.
    function inboxDeleteRemovesText(it) {
      switch (inboxDeleteKind(it)) {
        case 'recording': return 'This deletes the recording\'s audio stored on this hive, its machine transcript record and this Inbox item.';
        case 'upload': return 'This deletes the file stored on this hive, its extracted text or machine transcript record, and this Inbox item.';
        case 'email': return 'This deletes the mailed-in file stored on this hive, its machine transcript record if it has one, and this Inbox item.';
        case 'meeting': return 'This removes the meeting from the Inbox only. The meeting record, with its transcript and notes, stays in the Meetings section; delete it there to erase it.';
        default: return 'This removes the item from the Inbox only, and the next sync does not bring it back. The recording itself stays in ' + (it.source || 'its source') + '.';
      }
    }
    // inboxDeleteKnowledgeText says what happens to the Knowledge copy.
    function inboxDeleteKnowledgeText(it) {
      if (!it || !it.fact_slug) return '';
      if (inboxDeleteKnowledgeRemovable(it)) return 'Its Knowledge copy is kept, unless you tick the box below.';
      return 'Its Knowledge fact is kept: the ' + (it.source || 'source') + ' sync maintains it.';
    }
    function inboxDeleteSharesText(n) {
      return n ? 'What was already shared (' + n + ' share' + (n === 1 ? '' : 's') + ': RFCs, issues, DIBS ideas, spek seeds) stays where it is.' : '';
    }
    // inboxDeleteDialog builds the confirmation for the items: the title,
    // the message (what is removed, the Knowledge copy, the meeting record,
    // the shares) and whether to offer the Knowledge box.
    function inboxDeleteDialog(items) {
      const list = (items || []).filter(Boolean);
      const shares = list.reduce((n, it) => n + ((it.share_history || []).length), 0);
      const removable = list.some(inboxDeleteKnowledgeRemovable);
      let parts;
      if (list.length === 1) {
        parts = [inboxDeleteRemovesText(list[0]), inboxDeleteKnowledgeText(list[0])];
      } else {
        const count = kind => list.filter(it => inboxDeleteKind(it) === kind).length;
        const n = (c, one, many) => c + ' ' + (c === 1 ? one : many);
        const stored = count('recording') + count('upload') + count('email');
        const kept = list.filter(it => it.fact_slug && !inboxDeleteKnowledgeRemovable(it)).length;
        parts = [
          'This deletes ' + list.length + ' Inbox items.',
          stored ? n(stored, 'is a recording, upload or mailed-in file', 'are recordings, uploads or mailed-in files') + ' stored on this hive: the stored file and its transcript record are deleted.' : '',
          count('meeting') ? n(count('meeting'), 'is a meeting', 'are meetings') + ' hive attended: the meeting record, with its transcript and notes, stays in the Meetings section.' : '',
          count('synced') ? n(count('synced'), 'comes', 'come') + ' from a synced source: the recording stays with its source, and the next sync does not bring the item back.' : '',
          removable ? 'Knowledge copies are kept, unless you tick the box below.' : '',
          kept ? n(kept, 'Knowledge fact', 'Knowledge facts') + ' of synced sources ' + (kept === 1 ? 'is' : 'are') + ' kept: the sync maintains ' + (kept === 1 ? 'it' : 'them') + '.' : ''
        ];
      }
      parts.push(list.map(it => '• ' + (it.title || inboxItemKey(it))).join('\n'), inboxDeleteSharesText(shares), 'This cannot be undone.');
      return {
        title: list.length === 1 ? 'Delete this item?' : 'Delete ' + list.length + ' items?',
        message: parts.filter(Boolean).join('\n\n'),
        knowledge: removable
      };
    }
    function inboxDeleteButtonHtml(it) {
      // A mailed-in item that waits for approval has ✕ Decline instead.
      if (it.awaiting_approval || !inboxCanDelete(it, window._hiveRole, window._hiveUser)) return '';
      const busy = inboxDeleteBusy(it);
      return '<button type="button" class="hv-btn btn-icon btn-sm inbox-action inbox-action-danger inbox-delete" data-action="inboxDeleteItem" data-arg0="' + escapeHtml(it.source) + '" data-arg1="' + escapeHtml(it.id) + '" aria-label="Delete"' +
        (busy ? ' disabled aria-disabled="true" title="Being transcribed: delete it once the transcription finishes"' : ' title="Delete this item from the Inbox; anything already shared from it stays"') + '>' + inboxIconSvg('trash') + '</button>';
    }
    // The bulk menu's own group, after the Hive group.
    function inboxDeleteBulkGroups() {
      if (window._hiveRole !== 'owner' && window._hiveRole !== 'read-write') return [];
      return [{ id: 'manage', label: 'Manage', items: [{ id: 'delete', label: 'Delete…', disabled: false, hint: '' }] }];
    }
    function inboxDeleteEligible(it) {
      return inboxCanDelete(it, window._hiveRole, window._hiveUser) && !inboxDeleteBusy(it);
    }
    async function inboxDeleteRun(items, bulk = false) {
      const list = (items || []).filter(Boolean);
      if (!list.length) return;
      const dlg = inboxDeleteDialog(list);
      const cfg = { title: dlg.title, message: dlg.message, confirmLabel: 'Delete', danger: true, scrollBody: list.length > 1 };
      if (dlg.knowledge) cfg.check = { label: INBOX_DELETE_KNOWLEDGE_LABEL, checked: false };
      const answer = await hiveConfirm(cfg);
      if (!answer) return;
      const visible = bulk ? new Map(inboxShownItems().map(it => [inboxItemKey(it), it])) : null;
      const current = list.map(it => bulk ? visible.get(inboxItemKey(it)) : inboxFind(it.source, it.id));
      if (current.some(it => !inboxDeleteEligible(it) || (bulk && !_inboxSelected.has(inboxItemKey(it))))) {
        showToast(bulk ? 'The Inbox selection changed. Review it before deleting.' : 'This item can no longer be deleted. Review the Inbox.', 'error');
        return;
      }
      const removeKnowledge = !!(dlg.knowledge && answer.checked);
      if (removeKnowledge && list.some((it, i) => {
        const latest = current[i];
        return (inboxDeleteKnowledgeRemovable(it) || inboxDeleteKnowledgeRemovable(latest)) &&
          (it.fact_slug !== latest.fact_slug || it.layer !== latest.layer);
      })) {
        showToast('A Knowledge copy changed. Review the items before deleting.', 'error');
        return;
      }
      try {
        const r = await fetch('/api/inbox/delete', { method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ items: list.map(it => ({ source: it.source, item_id: it.id })), remove_knowledge: removeKnowledge }) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        for (const d of (data.items || [])) {
          const key = d.source + '/' + d.item_id;
          delete _inboxContent[key];
          _inboxSelected.delete(key);
        }
        const failed = data.failed || [];
        const n = Number(data.deleted) || 0;
        if (failed.length) showToast(n + ' deleted, ' + failed.length + ' not deleted: ' + (failed[0].error || 'failed'), 'error');
        else showToast(n === 1 ? 'Item deleted' : n + ' items deleted', 'success');
      } catch (e) {
        showToast('Could not delete: ' + e.message, 'error');
      }
      await fetchInbox();
    }
    function inboxDeleteItem(src, id) {
      const it = inboxFind(src, id);
      return it ? inboxDeleteRun([it]) : undefined;
    }
    // Delete… in the bulk menu: the selected items the viewer may delete.
    function inboxDeleteSelected() {
      const picked = Array.from(_inboxSelected).map(k => { const i = k.indexOf('/'); return inboxFind(k.slice(0, i), k.slice(i + 1)); }).filter(Boolean);
      const allowed = picked.filter(inboxDeleteEligible);
      if (!allowed.length) { showToast(picked.length ? 'None of the selected items can be deleted by you right now' : 'Nothing selected', 'error'); return undefined; }
      return inboxDeleteRun(allowed, true);
    }

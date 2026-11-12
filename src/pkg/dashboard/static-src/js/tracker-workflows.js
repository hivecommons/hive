    /* ---- Workflow-model columns (hivecommons/hive#17620, RFC phase 2) -----
       GET /api/stage-tracker carries `workflows` only when at least one
       repository is bound to a model other than hive-default. For such a
       repository the group header names the model and draws the model's own
       column header, and each row's strip is one chevron per model column,
       placed by the stage/NNN number the row wears. Everything else on the
       row (lead cell, chips, human gate, notes) is unchanged. Hives with no
       binding never reach this code: stageTrackerWorkflowOf returns null and
       the built-in strip renders exactly as before. Pure helpers so the node
       harness (stage_tracker_workflows_ui_test.go) can run them. */
    const STAGE_TRACKER_WORKFLOW_UNCOVERED_WORD = 'Not covered';

    // The model block for repo, or null when the repo follows hive-default
    // (unbound, bound to hive-default itself, or bound to a model the
    // server reported as unusable — the row then falls back to the
    // built-in strip, which is what the engine does too).
    function stageTrackerWorkflowOf(ctx, repo) {
      const w = ctx && ctx.workflows;
      if (!w || !w.repos) return null;
      const wf = w.repos[String(repo || '')];
      if (!wf || wf.error || !Array.isArray(wf.columns) || !wf.columns.length) return null;
      if (w.default && wf.name === w.default.name && wf.version === w.default.version) return null;
      return wf;
    }

    // Index of the column holding stage number `stage`; 0 for an unstaged
    // item (the first column is the model's entry), the terminal column for
    // a terminal stage, -1 when no column anchors the number (the model
    // does not cover it: the engine abstained with "workflow: not covered").
    function stageTrackerWorkflowColumnIndex(wf, stage) {
      const n = Number(stage) || 0;
      const cols = wf.columns;
      if (n === 0) return 0;
      if (n >= (typeof STAGE_TRACKER_TERMINAL === 'number' ? STAGE_TRACKER_TERMINAL : 110)) {
        const t = cols.findIndex(c => c.terminal);
        return t >= 0 ? t : cols.length - 1;
      }
      return cols.findIndex(c => Array.isArray(c.numbers) && c.numbers.indexOf(n) >= 0);
    }

    // Pure: the reply verbs the bound model declares for the item's
    // current state (RFC phase 4, `workflows.repos[repo].replies` keyed by
    // stage number), in the model's order; [] for an unbound repository, a
    // PR row (the dashboard records model replies on issues only), or a
    // state without replies.
    function stageTrackerWorkflowReplies(item, workflows) {
      if (!item || item.kind === 'pr' || item.via_pr) return [];
      const wf = stageTrackerWorkflowOf({ workflows }, item.repo);
      if (!wf || !wf.replies) return [];
      const list = wf.replies[String(Number(item.stage) || 0)];
      return Array.isArray(list) ? list.map(v => String(v || '').trim()).filter(Boolean) : [];
    }

    // Pure: a model verb as the pill prints it — "approve analysis" →
    // "Approve analysis".
    function stageTrackerWorkflowReplyLabel(verb) {
      const v = String(verb || '').trim();
      return v ? v.charAt(0).toUpperCase() + v.slice(1) : '';
    }

    function stageTrackerWorkflowBadgeHtml(wf) {
      if (!wf) return '';
      const key = `${wf.name}@${wf.version}`;
      return `<span class="ham-wf-badge" role="button" tabindex="0" data-action="openWorkflowLibraryEntry" data-keydown-action="openWorkflowLibraryEntry" data-arg0="${esc(wf.name)}" data-arg1="${esc(wf.version)}" data-stop="1" title="${esc('This repository follows the ' + key + ' workflow model; its columns are the model\'s, not hive-default\'s. Click to open it in Settings → Workflows.')}">${esc(key)}</span>`;
    }

    // The model's column header, drawn under a bound repository's group
    // header in place of the shared hive-default header.
    function stageTrackerWorkflowHeaderHtml(wf) {
      const cols = wf.columns.map(c => `<li class="ham-seg ham-wf-col${c.terminal ? ' cap' : ''}" title="${esc(c.hint || c.label)}"><span class="ham-seg-t">${esc(c.label)}</span></li>`).join('');
      return `<div class="ham-cols ham-wf-cols" aria-hidden="true" data-ham-workflow="${esc(wf.name + '@' + wf.version)}"><div class="ham-strip-row"><ol class="ham-strip">${cols}</ol><span class="ham-since">since</span></div></div>`;
    }

    // Pure: the key a row's model resolves to ('' for hive-default).
    function stageTrackerWorkflowKey(wf) { return wf ? wf.name + '@' + wf.version : ''; }

    // Pure: the column header the rows under it follow — the model's own
    // when they are bound to one, else hive-default's. The one path for
    // the sticky header and the run headers, so the names above a strip
    // are always the names its chevrons wear (hivecommons/hive#18002:
    // hive-default's "PR in progress / Merge queue" over model chevrons).
    function stageTrackerColumnsHeaderHtml(wf, stages) {
      return wf ? stageTrackerWorkflowHeaderHtml(wf) : stageTrackerHeaderHtml(stages);
    }

    // Pure: an ungrouped page's rows partitioned, stably and in first-seen
    // order, by the model each row's repository follows: [{ wf, key, rows }].
    // One run when every row follows the same model (or none does), so a
    // hive-default page is left exactly as sorted.
    function stageTrackerWorkflowRuns(rows, ctx) {
      const runs = [];
      const by = {};
      for (const it of rows || []) {
        const wf = stageTrackerWorkflowOf(ctx, it.repo);
        const key = stageTrackerWorkflowKey(wf);
        if (!by[key]) { by[key] = { wf, key, rows: [] }; runs.push(by[key]); }
        by[key].rows.push(it);
      }
      return runs;
    }

    // Run headers are keyed under the group prefix so the selection and
    // the reconcile treat them as headers, never as item rows.
    const STAGE_TRACKER_WORKFLOW_HEADER_KEY_PREFIX = STAGE_TRACKER_GROUP_KEY_PREFIX + 'workflow:';

    // Pure: the page's keyed rows with a column header spliced in before
    // each run after the first (the first run sits under the sticky
    // header). A run whose lead row the train/preparation wrappers did not
    // keep by key gets no header rather than a misplaced one.
    function stageTrackerWorkflowRunHeaderRows(rows, runs, stages) {
      const out = (rows || []).slice();
      (runs || []).slice(1).forEach(run => {
        const lead = run.rows[0] ? stageTrackerItemKey(run.rows[0]) : null;
        const at = out.findIndex(r => r.key === lead);
        if (at >= 0) out.splice(at, 0, { key: STAGE_TRACKER_WORKFLOW_HEADER_KEY_PREFIX + (run.key || 'hive-default'), html: stageTrackerColumnsHeaderHtml(run.wf, stages) });
      });
      return out;
    }

    // Under a group header: a bound repository's own column header; a
    // hive-default repository's only when the sticky header above is a
    // model's (ctx.leadWorkflow), so its rows are not read against that
    // model's names. Collapsed groups draw none.
    function stageTrackerGroupColumnsHtml(g, groupWorkflow, stages, ctx) {
      if (!g || g.collapsed) return '';
      if (groupWorkflow) return stageTrackerWorkflowHeaderHtml(groupWorkflow);
      return ctx && ctx.leadWorkflow ? stageTrackerHeaderHtml(stages) : '';
    }

    // One row's strip under its model: done before the anchored column,
    // active at it (the stage's own word from `stages`, or the column
    // label), todo after. An uncovered stage lights the first column amber
    // with the "Not covered" word so the gap is visible, not silent.
    function stageTrackerWorkflowStripItems(item, wf, stages) {
      const cur = Number(item.stage) || 0;
      const terminal = cur >= (typeof STAGE_TRACKER_TERMINAL === 'number' ? STAGE_TRACKER_TERMINAL : 110);
      let at = stageTrackerWorkflowColumnIndex(wf, cur);
      const uncovered = at < 0;
      if (uncovered) at = 0;
      const st = (stages || []).find(s => Number(s.number) === cur) || {};
      return wf.columns.map((c, i) => {
        let state = 'todo';
        if (terminal || i < at) state = 'done';
        else if (i === at) state = 'active';
        if (terminal && c.terminal) state = 'active';
        const word = state !== 'active' ? c.label : (uncovered ? STAGE_TRACKER_WORKFLOW_UNCOVERED_WORD : (st.short || st.label || c.label));
        const tip = state === 'active' && uncovered
          ? `stage/${String(cur).padStart(3, '0')} is not a state of ${wf.name}@${wf.version}; the engine leaves this item where it is.`
          : (c.hint || c.label) + (state === 'active' && cur ? ` — stage/${String(cur).padStart(3, '0')}` : '');
        const cls = ['ham-seg', 'ham-wf-seg', state, c.terminal ? 'cap' : '', uncovered && state === 'active' ? 'ham-wf-uncovered' : ''].filter(Boolean).join(' ');
        return `<li class="${cls}" title="${esc(tip)}"><span class="ham-seg-t">${esc(word)}</span></li>`;
      }).join('');
    }

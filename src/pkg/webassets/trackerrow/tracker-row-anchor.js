    // Which strip segment a stage lights: main stages light themselves,
    // 040 shares the PR in progress column with 030, 060 shares CI / Review
    // with 050, 095 and 100 share the Merge gate column with 080, detours
    // light the segment they branch from (stageTrackerColumn in Go).
    // kind is 'issue' or 'pr': an issue parked on a person (045, or a
    // pre-045 085) is parked before any PR exists, so it lights Actionable
    // rather than the merge gate. item (optional) is the row: once a merge
    // call has been made on its head (merge_gate.state merging /
    // merge-failed) the outcome is a Merge gate matter whatever numeric
    // stage the lane fell back to — hivecommons/docs#512 drew "Merge
    // failed" in CI / Review because the PR sat at 060.
    //
    // The column follows the CURRENT state, never the highest stage the
    // row once reached: a PR under repair (stageTrackerUnderRepair) lights
    // CI / Review even when a stale merge_gate ("merge-failed" from the
    // attempt that found the conflict) or a planner-ordered merge_train
    // still rides on the record — hive's own #15574 read "Fixing" under
    // Merge queue after "fell back to Fixing — conflicting".
    function stageTrackerAnchor(stage, stages, kind, item) {
      const cur = Number(stage) || 0;
      if (cur >= STAGE_TRACKER_TERMINAL) return STAGE_TRACKER_TERMINAL;
      // Under repair past 030 → CI / Review, before any gate or train
      // promotion. The one park the predicate also names, 085 needs
      // decision, stays at the gate: the person decides there.
      const repairing = typeof stageTrackerUnderRepair === 'function' && !!stageTrackerUnderRepair(cur, item && item.reason);
      if (repairing && cur > STAGE_TRACKER_PR_COLUMN && cur !== STAGE_TRACKER_NEEDS_DECISION) return 50;
      const mg = item && item.merge_gate;
      const refusal = mg && mg.state === 'merge-failed'
        ? mg.merge_failed_kind || (typeof stageTrackerMergeRefusalFromRaw === 'function' ? stageTrackerMergeRefusalFromRaw(String(mg.merge_failed || '')).kind : '') : '';
      if ((cur === 50 || cur === 60) && refusal === 'checks-pending') return 50;
      if (mg && (mg.state === 'merging' || mg.state === 'merge-failed') && cur > STAGE_TRACKER_PR_COLUMN) return STAGE_TRACKER_MERGE_COLUMN;
      // A merge-train member is a Merge queue matter too, 1st included:
      // the train's facts are mirrored onto its head while the head still
      // sits at 050/070, and hive's own #15400 ("1st") lit CI / Review
      // next to its 2nd-place sibling in Merge queue (hivecommons/hive#15395).
      if (item && item.merge_train && item.merge_train.state && cur > STAGE_TRACKER_PR_COLUMN) return STAGE_TRACKER_MERGE_COLUMN;
      if (cur === STAGE_TRACKER_ISSUE_NEEDS_DECISION || cur === STAGE_TRACKER_TRIAGE_NEEDED || cur === STAGE_TRACKER_POSSIBLY_EXISTS) return STAGE_TRACKER_ISSUE_DECISION_COLUMN;
      if (cur === STAGE_TRACKER_NEEDS_DECISION && kind === 'issue') return STAGE_TRACKER_ISSUE_DECISION_COLUMN;
      if (STAGE_TRACKER_PATH.indexOf(cur) >= 0) return cur;
      if (cur === 40) return STAGE_TRACKER_PR_COLUMN;
      // 060 reviewed is pre-admission (approved, CI pending or red; it can
      // regress to 070): it lights CI / Review (50), never the merge gate.
      if (cur === 60) return 50;
      if (cur === STAGE_TRACKER_PRE_MERGE_SOAK || cur === 100) return STAGE_TRACKER_MERGE_COLUMN;
      const st = (stages || []).find(x => Number(x.number) === cur) || {};
      let from = Number(st.branch_from) || 0;
      if (from === 60) from = 50;
      return STAGE_TRACKER_PATH.indexOf(from) >= 0 ? from : 0;
    }

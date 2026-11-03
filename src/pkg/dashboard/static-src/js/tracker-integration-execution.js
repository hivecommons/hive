    let _stageTrackerExecution = null;
    function stageTrackerExecutionSync(data, nowMs) {
      const view = data && data.integration_execution;
      const observed = view && Date.parse(view.observed_at);
      const maxAge = view && Number(view.observation_age_seconds) * 1000;
      _stageTrackerExecution = view && view.mode !== 'off' && Array.isArray(view.candidates)
        ? Object.assign({}, view, {nowMs, maxAge, stale: !Number.isFinite(observed) || !(maxAge > 0) || nowMs - observed > maxAge})
        : null;
    }

    function stageTrackerExecutionHtml(item) {
      const view = _stageTrackerExecution;
      if (!view || !item || item.kind !== 'pr' || !item.head || !item.base_ref) return '';
      const candidate = view.candidates.find(entry => {
        const members = entry.members || [];
        const member = members[members.length - 1];
        return member && String(entry.repo).toLowerCase() === String(item.repo).toLowerCase()
          && Number(member.pr) === Number(item.number) && member.head === item.head
          && entry.base === item.base_ref;
      });
      if (!candidate) return '';
      const candidateAt = Date.parse(candidate.observed_at);
      const stale = view.stale || !Number.isFinite(candidateAt) || view.nowMs - candidateAt > view.maxAge;
      const state = stale ? 'unknown (stale observation)' : candidate.state || 'unknown';
      const kind = candidate.kind === 'preparation' ? 'Candidate preparation' : 'Integration CI';
      const members = (candidate.members || []).map(member => '#' + member.pr + '@' + String(member.head).slice(0, 8)).join(' → ');
      const text = kind + ' ' + state + ' · ordered prefix ' + (candidate.members || []).length
        + (state === 'passed' ? ' · live merge guards still required' : '')
        + (view.mode === 'advisory' ? ' · advisory, no writer' : '');
      const detail = text + ' · ' + members + ' · subject ' + candidate.subject_id
        + ' · tested ' + (candidate.tested_commit || 'unknown') + ' · ' + (candidate.reason || '')
        + ' · runtime ' + (view.runtime_version || 'unknown') + ' · source/image unknown';
      return `<div class="cue-event" data-integration-subject="${esc(candidate.subject_id)}"><span class="cue-event-body" title="${esc(detail)}">${esc(text)}</span></div>`;
    }

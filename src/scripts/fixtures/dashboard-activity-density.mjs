// Dedicated file:// acceptance page only. All evidence/permissions are mocks;
// real production renderers and delegated interaction handlers remain in use.
export function activityDensityFixture(input) {
  const panel = document.getElementById('activity-monitor-panel');
  if (!panel) throw new Error('Actual Activity Monitor panel is missing');
  const nowMs = Date.now(), observed = new Date(nowMs).toISOString();
  const saved = { data: _stageTracker, rows: _stageTrackerRows,
    role: window._hiveRole, user: window._hiveUser,
    preparation: [_stageTrackerPreparationShown, _stageTrackerPreparationRendered,
      _stageTrackerPreparationInline, _stageTrackerPreparationNow] };
  const items = structuredClone(input.rows);
  const rebase = item => {
    item.state = 'open';
    item.stage_since = observed;
    for (const key of ['ci_activity', 'review_activity', 'code_activity']) {
      if (item[key]) item[key].observed_at = observed;
    }
    if (item.decision_needed && !item.decision_needed.options) {
      item.decision_needed.options = [{ action: 'decide:approve', enabled: true,
        label: 'Review mock decision', consequence: 'Mock fixture only' }];
    }
    for (const child of item.linked_prs || []) rebase(child);
  };
  items.forEach(rebase);
  const mockPR = (number, stage, extra = {}) => ({
    repo: 'fixture/activity', kind: 'pr', number, stage, state: 'open',
    title: 'Mock acceptance projection — not a live PR',
    head: 'mock-current-head-' + number, stage_since: observed,
    url: 'https://example.invalid/activity/' + number, ...extra,
  });
  const track = (item, state, caption) => ({
    head: item.head, state, caption, observed_at: observed, source: 'acceptance-fixture',
  });
  const unknown = mockPR(9001, 50, { caption: 'Current-head evidence unavailable' });
  const conflict = mockPR(9002, 80, {
    merge_gate: { state: 'claim-conflict:9003', duplicate_issue: 9000 },
    decision_needed: { question: 'Which mock implementation should be kept?',
      blocker: 'Both claim mock issue #9000; neither is admitted.',
      options: [{ action: 'decide:approve', enabled: true, label: 'Keep this implementation', consequence: 'Mock fixture only' }] },
  });
  const longHandle = 'thatguywob'.repeat(12);
  const waiting = mockPR(9004, 71, { display_column: 30,
    caption: 'Waiting for @' + longHandle + ' · not observed coding',
    author: longHandle, author_kind: 'human',
    code_activity: { state: 'waiting_owner', head: 'mock-current-head-9004',
      caption: 'Waiting for @' + longHandle + ' · not observed coding', source: 'native-author-wait' },
    push: { state: 'waiting', login: longHandle, since: observed, next: 're-review' },
  });
  const running = mockPR(9005, 50);
  running.ci_activity = track(running, 'running', 'CI running');
  running.ci_activity.active = true;
  running.review_activity = track(running, 'approved', 'Review approved');
  const held = mockPR(9006, 70, { caption: 'Waiting for fixer · no observed code work',
    hold: { by: 'hanthor', pill: 'Held by @hanthor', reason: 'Mock human hold' },
    modifiers: ['hold'] });
  const coding = mockPR(9007, 70, { display_column: 30 });
  coding.code_activity = { ...track(coding, 'fixing', 'Remediating · fixer'), active: true,
    since: observed, source: 'fix-dispatch+successful-edit+verified-git-worktree' };
  coding.ci_activity = { ...track(coding, 'running', 'CI running'), active: true };
  coding.review_activity = { ...track(coding, 'reviewing', 'Review working · reviewer'), active: true };
  const triage = { repo: 'fixture/activity', kind: 'issue', number: 9008, stage: 15,
    state: 'open', title: 'Mock issue-only triage controls and long wrapping metadata',
    url: 'https://example.invalid/activity/9008', operator_action_count: 7 };
  // One short caption ("Open · hold") → a one-line-tall chevron row; the
  // long-handle row above wraps to two. Both heights are probed.
  const parked = { repo: 'fixture/activity', kind: 'issue', number: 9009, stage: 0,
    state: 'open', title: 'Mock held issue with a one-line chevron caption',
    url: 'https://example.invalid/activity/9009', modifiers: ['hold'],
    hold: { by: 'hanthor', pill: 'Held by @hanthor', reason: 'Mock human hold' } };
  items.push(unknown, conflict, waiting, running, held, coding, triage, parked);
  const prUnder = (repo, issueNumber, pr, title) => ({
    repo, kind: 'issue', number: issueNumber, stage: 40, state: 'open',
    title, url: 'https://example.invalid/activity/' + issueNumber, linked_prs: [pr],
  });
  const prepFront = (number, base, ciState, reviewState) => {
    const item = mockPR(number, 74, { repo: 'hivecommons/hive', base_ref: base,
      caption: 'Head changed; preparation unverified; CI/re-review queued',
      push: { state: 'pushed', login: 'fixture-owner', since: observed, next: 're-review' } });
    item.ci_activity = track(item, ciState, ciState === 'unknown' ? 'Required checks not observed' : 'CI waiting · test not observed');
    item.review_activity = track(item, reviewState, 'Review ' + reviewState + ' · ' + (reviewState === 'assigned' ? 'adjudicator' : 'unassigned'));
    return item;
  };
  const front16450 = prepFront(16450, 'v6', 'unknown', 'assigned');
  const front16451 = prepFront(16451, 'v5', 'waiting', 'queued');
  const issue16420 = items.find(item => item.kind === 'issue' && item.number === 16420);
  issue16420.stage = 40;
  issue16420.linked_prs = [front16450, { number: 16436, stage: 74, state: 'closed',
    title: 'Mock closed adopted original', superseded_by: 16450, base_ref: 'v6' }];
  items.push(prUnder('hivecommons/hive', 16363, front16451, 'Mock preparation front on a one-to-one issue'));
  const pluk = mockPR(436, 50, { repo: 'hivecommons/pluk' });
  pluk.ci_activity = track(pluk, 'passing', 'CI passed');
  pluk.review_activity = track(pluk, 'assigned', 'Review assigned · ci-maintainer');
  items.push(prUnder(pluk.repo, 435, pluk, 'Mock CI-passed / review-assigned linked row'));
  const pr16437Index = items.findIndex(item => item.kind === 'pr' && item.number === 16437);
  const pr16437 = items.splice(pr16437Index, 1)[0];
  pr16437.stage = 60;
  pr16437.manual_merge = false;
  pr16437.ci_activity = track(pr16437, 'unknown', 'Required checks not observed');
  pr16437.review_activity = track(pr16437, 'approved', 'Review approved');
  items.push(prUnder(pr16437.repo, 16421, pr16437, 'Mock approved-review / unknown-CI linked row'));
  const superseded = items.find(item => item.number === 16438).linked_prs.find(pr => pr.number === 16446);
  superseded.stage = 70;
  superseded.display_column = 50;
  superseded.superseded = true;
  superseded.human_attention = { kind: 'pr', number: superseded.number, head: superseded.head,
    reason: 'Superseded by #16449 — a maintainer decides whether to close this mock implementation' };
  superseded.code_activity = { ...track(superseded, 'waiting_fixer', 'Waiting for fixer · no observed code work') };
  superseded.decision_needed = { question: 'Close this mock implementation as superseded by #16449?',
    options: [{ action: 'decide:close-superseded', enabled: true, label: 'Close mock superseded PR' }] };
  superseded.review_activity = track(superseded, 'changes_requested', 'Review findings · human decision');
  const base_preparation = [front16450, front16451].map(front => ({
    repo: front.repo, base_ref: front.base_ref, state: 'observed',
    owner: { number: front.number, role: 'owner', state: 'current',
      head_sha: 'mock-previous-head-' + front.number, current_head: front.head,
      since: observed, detail: 'Head changed; current preparation unverified' }, waiting: [],
  }));
  const host = document.createElement('div');
  host.id = 'activity-density-layout-fixture';
  const note = document.createElement('p');
  note.textContent = 'Acceptance fixture: screenshot-shaped identities with MOCK stages/evidence/permissions; not authenticated live projection.';
  host.appendChild(note);
  window.__activityDensityCleanup = () => {
    stageTrackerCloseDecisionPop(false);
    if (typeof hideConfigTooltip === 'function') hideConfigTooltip();
    host.remove();
    _stageTracker = saved.data;
    _stageTrackerRows = saved.rows;
    window._hiveRole = saved.role;
    window._hiveUser = saved.user;
    [_stageTrackerPreparationShown, _stageTrackerPreparationRendered,
      _stageTrackerPreparationInline, _stageTrackerPreparationNow] = saved.preparation;
    delete window.__activityDensityCleanup;
  };
  _stageTracker = { items, base_preparation };
  _stageTrackerRows = items;
  window._hiveRole = 'owner';
  window._hiveUser = 'fixture-owner';
  // Display labels are fixture inputs; physical order/boundaries stay canonical.
  const labels = { 0: 'Open', 10: 'Actionable', 20: 'Claimed', 30: 'PR in progress',
    50: 'CI / Review', 80: 'Merge queue' };
  const stages = STAGE_TRACKER_PATH.map(number => ({ number, short: labels[number] || String(number) })).concat([
    { number: 15, short: 'Triage needed', branch_from: 10 },
    { number: 70, short: 'Remediating', branch_from: 50 },
    { number: 71, short: 'Needs author', branch_from: 30 },
    { number: 72, short: 'Returned to contributor', branch_from: 30 },
    { number: 74, short: 'Adopted by hive', branch_from: 50 },
    { number: 85, short: 'Needs human decision', branch_from: 80 },
    { number: 90, short: 'Ready for human merge', branch_from: 80 },
  ]);
  const ctx = { nowMs, items, events: {}, noteTuck: false, sharedPRs: {}, workers: {} };
  try {
    host.appendChild(stageTrackerRowNode('fixture-columns', stageTrackerHeaderHtml(stages)));
    const ordered = stageTrackerPreparationOrderRows(_stageTracker, items, stages);
    _stageTrackerRows = ordered;
    stageTrackerPreparationVisualSync(_stageTracker, ordered, ordered, stages, nowMs);
    const rows = ordered.map(item => ({ key: stageTrackerItemKey(item), html: stageTrackerRowHtml(item, stages, ctx) }));
    for (const row of stageTrackerPreparationInlineRows(rows, ordered, stages)) host.appendChild(stageTrackerRowNode(row.key, row.html));
    panel.appendChild(host);
    if (panel.dataset.hamWired !== '1') stageTrackerWireRowActions(panel);
  } catch (error) {
    window.__activityDensityCleanup();
    throw error;
  }
  host.scrollIntoView();
  return { provenance: input.provenance, mockEvidence: true, observedAt: observed,
    additionalScreenshotShape: 'Screenshot 2026-11-03 at 12.36.31 PM.png',
    rows: items.map(item => stageTrackerItemKey(item)) };
}

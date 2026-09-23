'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const INDEX = fs.readFileSync(path.join(__dirname, '..', '..', 'src', 'pkg', 'dashboard', 'static', 'index.html'), 'utf8');

test('#8307: dashboard renders the Runs navigation and collapsible section', () => {
  assert.match(INDEX, /data-section="runs-section"/);
  assert.match(INDEX, /id="runs-section"/);
  assert.match(INDEX, /data-action="toggleSection" data-arg0="runs-section"/);
  assert.match(INDEX, /'runs-section'/);
});

test('#8307: runs render defensively from status without substituting absent data', () => {
  assert.match(INDEX, /function dashboardRunsFromStatus\(data\)/);
  assert.match(INDEX, /hasOwnProperty\.call\(data, 'runs'\)/);
  assert.match(INDEX, /runs: unknown/);
  assert.match(INDEX, /renderRuns\(dashboardRunsFromStatus\(data\)\)/);
});

test('#8307: run cards expose owner-only plan actions and detail lookup', () => {
  assert.match(INDEX, /function renderRuns\(runs\)/);
  assert.match(INDEX, /dashboardRoleAtLeast\(window\._hiveRole \|\| 'read', 'owner'\)/);
  assert.match(INDEX, /data-action="runPlanAction"/);
  assert.match(INDEX, /\/api\/plan\//);
  assert.match(INDEX, /function openRunDetail\(key\)/);
  assert.match(INDEX, /\/api\/runs\//);
});

test('#8307: run state appears in agent, governor, and health surfaces', () => {
  assert.match(INDEX, /agentCurrentRunRow\(a\)/);
  assert.match(INDEX, /current run/);
  assert.match(INDEX, /blocked on human/);
  assert.match(INDEX, /RUN_STALL_THRESHOLD_MS = 60 \* 60 \* 1000/);
  assert.match(INDEX, /stalledRunHealthRows/);
});

test('#8349: run history from the status payload is merged without inventing absent data', () => {
  assert.match(INDEX, /data\.runHistory && Array\.isArray\(data\.runHistory\.recent\)/);
  assert.match(INDEX, /run\.outcome && run\.outcome !== 'active'/);
  assert.match(INDEX, /Run history \(\$\{finishedRuns\.length\}\)/);
  assert.match(INDEX, /run\.completed_at/);
});

test('#8349: leaderboard renders stages completed and falls back to unknown, never 0', () => {
  assert.match(INDEX, /function leaderboardStagesText\(e\)/);
  assert.match(INDEX, /e\.stages_completed === null \|\| e\.stages_completed === undefined/);
  assert.match(INDEX, /leaderboardStagesText\(e\)/);
  assert.match(INDEX, />Stages<\/th>/);
});

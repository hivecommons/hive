// Execute the deployed inline watchdog with a fake GitHub API and clock.
const { readFileSync } = require('node:fs');
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { join } = require('node:path');

const workflow = readFileSync(join(__dirname, '../workflows/ci-runner-canary.yml'), 'utf8');
const script = workflow.split('          script: |\n')[1]
  .split('\n  report:')[0].replace(/^            /gm, '');
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
const now = Date.parse('2026-10-27T13:31:30Z');
const ago = minutes => new Date(now - minutes * 60_000).toISOString();

async function failuresFor(jobs) {
  const failures = [];
  const github = {
    rest: { actions: { listWorkflowRunsForRepo: 'runs', listJobsForWorkflowRun: 'jobs' } },
    paginate: async (endpoint, args) => endpoint === 'runs'
      ? [{ id: 1, created_at: ago(23), html_url: 'https://example.test/run/1' }]
      : args.run_id === 1 ? jobs : [{ name: 'canary (1)', status: 'completed' }],
  };
  await new AsyncFunction('github', 'context', 'core', 'process', 'Date', script)(
    github, { repo: { owner: 'test', repo: 'test' }, runId: 2 },
    { info() {}, warning() {}, setOutput() {}, setFailed: message => failures.push(message) },
    { env: { CANARY_PICKUP_MINUTES: '10', QUEUE_STALL_MINUTES: '15', QUEUE_ZOMBIE_HOURS: '6' } },
    class extends Date { static now() { return now; } },
  );
  return failures;
}

test('old run with a newly eligible downstream job is healthy', async () => {
  assert.deepEqual(await failuresFor([{ status: 'queued', created_at: ago(2) }]), []);
});

test('a job queued beyond the threshold still raises the alarm', async () => {
  assert.equal((await failuresFor([{ status: 'queued', created_at: ago(16) }])).length, 1);
});

test('completed and running jobs do not raise the alarm', async () => {
  assert.deepEqual(await failuresFor([
    { status: 'completed', created_at: ago(23) },
    { status: 'in_progress', created_at: ago(20) },
  ]), []);
});

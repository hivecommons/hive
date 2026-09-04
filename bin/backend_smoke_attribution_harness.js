#!/usr/bin/env node
'use strict';

// Executes backend-smoke.yml's failure-issue script against fixtures, so its
// ATTRIBUTION is verified by running it rather than by reading it (#5883).
//
// WHY THIS EXISTS. On 2026-09-03 the pinned lane moved to the self-hosted fleet
// (#5862), the fleet image has no podman, and every scheduled run died at the
// image pull in ~7s with exit 127. The failure-issue job then filed
// "[Backend smoke: pinned] codex, claude failure on d7571a1 — The CLI versions
// hive SHIPS in the contributor image failed the smoke — contributors are
// likely broken right now" about a suite that had never executed. Nothing was
// wrong with the shipped CLIs; the runner had no container engine.
//
// The script that composes that text is non-trivial JavaScript embedded in a
// YAML block scalar, which is the least-tested shape of code in the repo: it
// runs only on a scheduled failure, in production, where its output is an issue
// somebody acts on. So it is extracted and run here against three fixtures —
// an environment failure, a real suite failure, and a run containing both.
//
// The script is read out of the workflow rather than copied, so this cannot
// drift from what CI actually runs. If the block moves or is renamed, the
// extraction fails loudly instead of testing a stale copy.

const fs = require('fs');

const workflow = process.argv[2];
if (!workflow) {
  console.error('usage: backend_smoke_attribution_harness.js <backend-smoke.yml>');
  process.exit(2);
}

// ---------------------------------------------------------------------------
// Extract the `script: |` block scalar. Indentation-based, no YAML dependency:
// the block ends at the first non-blank line indented no further than the key.
// ---------------------------------------------------------------------------
function extractScript(path) {
  const lines = fs.readFileSync(path, 'utf8').split('\n');
  const start = lines.findIndex(l => /^\s*script:\s*\|\s*$/.test(l));
  if (start < 0) throw new Error(`no "script: |" block found in ${path}`);
  const keyIndent = lines[start].match(/^\s*/)[0].length;

  const body = [];
  for (let i = start + 1; i < lines.length; i++) {
    const line = lines[i];
    if (line.trim() === '') { body.push(''); continue; }
    if (line.match(/^\s*/)[0].length <= keyIndent) break;
    body.push(line);
  }
  while (body.length && body[body.length - 1] === '') body.pop();
  if (body.length === 0) throw new Error('the script block is empty');

  const blockIndent = body.find(l => l !== '').match(/^\s*/)[0].length;
  return body.map(l => l.slice(blockIndent)).join('\n');
}

const source = extractScript(workflow);
// github-script wraps the body in an async function, so top-level await is
// legal there and has to be legal here too.
const run = new Function('github', 'context', `return (async () => {${source}\n})();`);

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------
const step = (name, conclusion) => ({ name, conclusion });
const job = (name, conclusion, steps) => ({ name, conclusion, steps });

// The real run 33788303199: both pinned arms died at the image pull because the
// runner had no podman; both latest arms passed on the same fleet.
const ENV_FAILURE = [
  job('smoke (codex, latest)', 'success', []),
  job('smoke (claude, latest)', 'success', []),
  job('smoke (codex, pinned)', 'failure', [
    step('Set up job', 'success'),
    step("Restrict to this arm's backend", 'success'),
    step('Backend smoke (latest codex)', 'skipped'),
    step('Pull the contributor image', 'failure'),
    step('Backend smoke (pinned codex, in-container)', 'skipped'),
  ]),
  job('smoke (claude, pinned)', 'failure', [
    step('Set up job', 'success'),
    step('Pull the contributor image', 'failure'),
    step('Backend smoke (pinned claude, in-container)', 'skipped'),
  ]),
];

// The condition the pinned lane exists to detect: the suite ran inside the
// image and failed.
const SUITE_FAILURE = [
  job('smoke (codex, pinned)', 'failure', [
    step('Require a container engine', 'success'),
    step('Pull the contributor image', 'success'),
    step('Backend smoke (pinned codex, in-container)', 'failure'),
  ]),
];

// Both in one run: codex died before the suite, claude ran it and failed.
// They are different problems with different owners, so they must not land in
// one thread — the same rule the lane split already follows.
const BOTH = [
  job('smoke (codex, pinned)', 'failure', [
    step('Pull the contributor image', 'failure'),
    step('Backend smoke (pinned codex, in-container)', 'skipped'),
  ]),
  job('smoke (claude, pinned)', 'failure', [
    step('Pull the contributor image', 'success'),
    step('Backend smoke (pinned claude, in-container)', 'failure'),
  ]),
];

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------
function harness(jobs, openIssues) {
  const created = [];
  const comments = [];
  return {
    created,
    comments,
    github: {
      paginate: async () => jobs,
      rest: {
        actions: { listJobsForWorkflowRun: {} },
        issues: {
          listForRepo: async () => ({ data: openIssues || [] }),
          create: async (o) => { created.push(o); },
          createComment: async (o) => { comments.push(o); },
        },
      },
    },
    context: {
      serverUrl: 'https://github.com',
      repo: { owner: 'hivecommons', repo: 'hive' },
      runId: 33788303199,
      sha: 'd7571a155c973a36e4741144811e29964ad9af6a',
    },
  };
}

let failures = 0;
function check(label, ok, detail) {
  if (ok) {
    console.log(`  PASS: ${label}`);
    return;
  }
  failures++;
  console.log(`  FAIL: ${label}`);
  if (detail) console.log(`        ${detail}`);
}

(async () => {
  console.log('=== backend-smoke failure-issue attribution (#5883) ===\n');

  // --- an environment failure must not blame the shipped CLIs --------------
  console.log('the runner had no container engine, so the suite never ran:');
  let h = harness(ENV_FAILURE);
  await run(h.github, h.context);
  check('files exactly one issue', h.created.length === 1, `filed ${h.created.length}`);
  const env = h.created[0] || { title: '', body: '' };
  check('does NOT claim contributors are broken',
    !env.body.includes('contributors are likely broken'),
    'the suite never executed, so it says nothing about the shipped CLIs');
  check('says the suite did not run', env.body.includes('DID NOT RUN'));
  check('names the step that actually failed', env.body.includes('Pull the contributor image'));
  check('marks the title as environmental', env.title.includes('environment'), env.title);
  check('does not point at suite evidence that does not exist',
    !env.body.includes("suite's evidence block"));

  // --- a real suite failure keeps its full urgency -------------------------
  console.log('\nthe suite ran inside the image and failed:');
  h = harness(SUITE_FAILURE);
  await run(h.github, h.context);
  check('files exactly one issue', h.created.length === 1, `filed ${h.created.length}`);
  const suite = h.created[0] || { title: '', body: '' };
  check('warns that contributors are likely broken',
    suite.body.includes('contributors are likely broken'));
  check('keeps the evidence pointer', suite.body.includes("suite's evidence block"));
  check('is NOT marked environmental', !suite.title.includes('environment'), suite.title);

  // --- the two must not share a thread -------------------------------------
  console.log('\nboth kinds of failure in one run:');
  h = harness(BOTH);
  await run(h.github, h.context);
  check('files two separate issues', h.created.length === 2, `filed ${h.created.length}`);
  const titles = h.created.map(c => c.title);
  check('their titles differ', new Set(titles).size === titles.length, titles.join(' | '));

  // --- dedupe still works, per thread --------------------------------------
  console.log('\na second environment failure with the thread already open:');
  h = harness(ENV_FAILURE, [{ number: 5883, title: '[Backend smoke: pinned — environment] codex, claude failure on abc1234' }]);
  await run(h.github, h.context);
  check('comments instead of filing a duplicate',
    h.created.length === 0 && h.comments.length === 1,
    `created=${h.created.length} comments=${h.comments.length}`);

  console.log(failures === 0
    ? '\nAll attribution checks passed.'
    : `\n${failures} attribution check(s) failed.`);
  process.exit(failures === 0 ? 0 : 1);
})().catch(e => {
  console.error('harness error: ' + (e && e.stack ? e.stack : e));
  process.exit(1);
});

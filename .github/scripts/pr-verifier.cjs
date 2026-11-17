'use strict';

// Records the required `PR Type` and `PR Issue` check runs for one PR head
// (#18764). Each check is ONE create call that already carries its final
// status, conclusion and output: no in_progress run is ever created, so no
// later update can fail and leave one hanging. The verdicts come from the
// in-repo port of the upstream verifier (integration-governance.cjs).
//
// A content verdict (a title without a type prefix, an issue number in the
// title) is reported by the check itself; the wrapper job stays green for it.
// Failing to RECORD a verdict is different: the job fails and says so, and a
// completed `failure` run carrying the reason is posted when GitHub allows it.

const {prTypeVerdict, prIssueVerdict} = require('./integration-governance.cjs');

const CHECK_NAMES = ['PR Type', 'PR Issue'];

const describe = error =>
  [error?.status, error?.message || String(error)].filter(Boolean).join(' ');

async function create(github, owner, repo, headSha, verdict) {
  const output = {title: verdict.title, summary: verdict.summary};
  if (verdict.text) output.text = verdict.text;
  const {data} = await github.rest.checks.create({
    owner, repo, name: verdict.name, head_sha: headSha,
    status: 'completed', conclusion: verdict.conclusion, output,
  });
  // The response is GitHub's own record of the run: anything other than the
  // completed verdict on this head means the required context is not settled.
  if (data?.name !== verdict.name || data?.head_sha !== headSha ||
      data?.status !== 'completed' || data?.conclusion !== verdict.conclusion) {
    throw new Error(`GitHub recorded ${JSON.stringify({name: data?.name, head_sha: data?.head_sha,
      status: data?.status, conclusion: data?.conclusion})} instead of a completed ${verdict.conclusion}`);
  }
  return data;
}

async function record({github, core, owner, repo, pr}) {
  const headSha = pr?.head?.sha;
  if (!/^[0-9a-f]{40}$/.test(headSha || '') || typeof pr?.title !== 'string') {
    core.setFailed('PR Verifier could not record its result: the event carries no PR head SHA or title (#18764).');
    return {recorded: [], unrecorded: CHECK_NAMES.slice()};
  }
  const verdicts = [prTypeVerdict(pr.title), prIssueVerdict(pr.title)];
  const recorded = [], unrecorded = [];
  for (const verdict of verdicts) {
    let error;
    // One retry covers a transient API error; each attempt is a single,
    // already-completed create, so a retry can never leave a run in progress.
    for (let attempt = 1; attempt <= 2; attempt++) {
      try {
        const run = await create(github, owner, repo, headSha, verdict);
        core.info(`${verdict.name}: ${verdict.conclusion} — ${verdict.summary} (check run ${run.id} on ${headSha})`);
        recorded.push(verdict.name);
        error = undefined;
        break;
      } catch (caught) {
        error = caught;
        core.info(`${verdict.name}: attempt ${attempt} to record the result failed: ${describe(caught)}`);
      }
    }
    if (!error) {
      if (verdict.conclusion !== 'success') {
        // Advisory for the wrapper job: the required context carries it.
        core.warning(`${verdict.name}: ${verdict.summary} The required "${verdict.name}" check reports this; the wrapper job stays green.`);
      }
      continue;
    }
    unrecorded.push(verdict.name);
    const reason = `PR Verifier could not record its "${verdict.name}" result on ${headSha}: ${describe(error)}`;
    core.error(reason);
    try {
      await github.rest.checks.create({
        owner, repo, name: verdict.name, head_sha: headSha,
        status: 'completed', conclusion: 'failure',
        output: {title: verdict.title, summary: `${reason}. This is a verifier failure, not a verdict on the PR: re-run this PR Verifier run, or add or remove any label, to record a real result (#18764).`},
      });
    } catch (fallback) {
      core.error(`${verdict.name}: the fallback failure run could not be posted either: ${describe(fallback)}`);
    }
  }
  if (unrecorded.length) {
    core.setFailed(`PR Verifier could not record its result for ${unrecorded.join(' and ')} on ${headSha}; ` +
      'the required check(s) may be missing or show no verdict for this head (#18764).');
  }
  return {recorded, unrecorded};
}

module.exports = {CHECK_NAMES, record};

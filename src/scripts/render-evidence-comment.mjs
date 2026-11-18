#!/usr/bin/env node
// Posts or updates the ONE sticky "UI render evidence" PR comment, and records
// a check run, from the artifact the unprivileged capture job uploaded (#18915).
//
// Runs from dashboard-render-evidence-comment.yml on workflow_run, i.e. with
// this file as committed on the DEFAULT branch, never the PR's copy. The
// artifact is untrusted data: it is validated and reduced to a fixed alphabet
// (lib/render-evidence.mjs), the PR number it names is accepted only when that
// PR's head commit is the commit the capture run built, and nothing from it is
// executed or interpolated into a shell. It never fails the workflow: every
// problem is logged as a notice and the process exits 0.
//
// Environment: GH_TOKEN, REPO (owner/name), RUN_ID, HEAD_SHA, PR_HINT (optional
// pull request number from the workflow_run payload), INDEX_PATH.
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import {
  RENDER_EVIDENCE_MARKER, STATUS_OK, buildIndex, validateIndex, renderCommentBody,
} from './lib/render-evidence.mjs';

const BOT_LOGIN = 'github-actions[bot]';
const CHECK_NAME = 'UI render evidence';

function ghCLI(args, input) {
  return execFileSync('gh', args, { encoding: 'utf8', input, stdio: ['pipe', 'pipe', 'pipe'] });
}

// gh is injectable so the sticky/update-in-place behaviour is testable.
export function run({ env, gh = ghCLI, readFile = (p) => fs.readFileSync(p, 'utf8'), log = console.log }) {
  const repo = env.REPO;
  const runId = String(env.RUN_ID || '');
  const head = String(env.HEAD_SHA || '');
  if (!/^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/.test(repo || '') || !/^\d+$/.test(runId) || !/^[0-9a-f]{40}$/.test(head)) {
    log('::notice::render evidence: missing or malformed workflow context; nothing posted');
    return { posted: false };
  }

  let index = null;
  try { index = validateIndex(JSON.parse(readFile(env.INDEX_PATH))); } catch (_) { index = null; }
  if (!index) {
    index = validateIndex(buildIndex({ pr: 0, head, status: 'neutral',
      reason: 'the capture run produced no usable index', shots: [], skipped: [], budgetMs: 0, elapsedMs: 0 }));
  }

  const candidate = index.pr || parseInt(env.PR_HINT || '0', 10) || 0;
  if (!Number.isInteger(candidate) || candidate <= 0) {
    log('::notice::render evidence: no pull request to comment on');
    return { posted: false };
  }
  // The artifact is author-controlled: only believe a PR number whose current
  // head is the commit this run built.
  let prHead = '';
  try { prHead = gh(['api', `repos/${repo}/pulls/${candidate}`, '--jq', '.head.sha']).trim(); } catch (_) { prHead = ''; }
  if (prHead !== head) {
    log(`::notice::render evidence: PR ${candidate} head does not match the run's commit; nothing posted`);
    return { posted: false };
  }

  const runURL = `https://github.com/${repo}/actions/runs/${runId}`;
  let artifactURL = '';
  try {
    const id = gh(['api', `repos/${repo}/actions/runs/${runId}/artifacts`, '--jq',
      '[.artifacts[] | select(.name=="render-evidence") | .id][0] // empty']).trim();
    if (/^\d+$/.test(id)) artifactURL = `${runURL}/artifacts/${id}`;
  } catch (_) { /* the comment is still useful without the direct link */ }
  const body = renderCommentBody(index, { run: runURL, artifact: artifactURL });

  let existing = '';
  try {
    existing = gh(['api', '--paginate', `repos/${repo}/issues/${candidate}/comments`, '--jq',
      `[.[] | select(.user.login=="${BOT_LOGIN}" and (.body | contains("${RENDER_EVIDENCE_MARKER}"))) | .id][0] // empty`]).trim().split('\n')[0];
  } catch (_) { existing = ''; }

  let posted = false;
  try {
    if (/^\d+$/.test(existing)) {
      gh(['api', '-X', 'PATCH', `repos/${repo}/issues/comments/${existing}`, '-F', 'body=@-'], body);
      log(`render evidence: updated comment ${existing} on PR ${candidate}`);
    } else {
      gh(['api', '-X', 'POST', `repos/${repo}/issues/${candidate}/comments`, '-F', 'body=@-'], body);
      log(`render evidence: posted a new comment on PR ${candidate}`);
    }
    posted = true;
  } catch (error) {
    log(`::notice::render evidence: could not write the PR comment (${String(error.message).split('\n')[0]})`);
  }

  try {
    const conclusion = index.status === STATUS_OK ? 'success' : 'neutral';
    const title = index.status === STATUS_OK ? 'Render evidence captured' : 'Render evidence is neutral';
    const summary = (index.reason || 'See the PR comment for the screenshot index.') +
      ' Fixture renders, not live data.';
    gh(['api', '-X', 'POST', `repos/${repo}/check-runs`,
      '-f', `name=${CHECK_NAME}`, '-f', `head_sha=${head}`, '-f', 'status=completed',
      '-f', `conclusion=${conclusion}`, '-f', `output[title]=${title}`, '-f', `output[summary]=${summary}`]);
  } catch (error) {
    log(`::notice::render evidence: could not record the check run (${String(error.message).split('\n')[0]})`);
  }
  return { posted, pr: candidate };
}

if (import.meta.url === `file://${process.argv[1]}`) {
  try { run({ env: process.env }); } catch (error) { console.log(`::notice::render evidence: ${error.message}`); }
  process.exit(0);
}

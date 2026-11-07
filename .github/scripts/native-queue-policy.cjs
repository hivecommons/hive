'use strict';

// This is an owned evidence gate, not a publisher of another App's statuses.
const ACTIONS_APP_ID = 15368;
const DCO_APP_ID = 1861;
const POLICY_CHECKS = {
  gate: ['docker.yml', ['push', 'pull_request']],
  DCO: null,
  'changelog-fragment-guard': ['changelog-fragment-guard.yml', ['pull_request']],
  'verify PR contents': ['pr-verifier.yml', ['pull_request_target']],
  'Sign-off survives the squash': ['dco-squash-attribution.yml', ['pull_request']],
};
const BUILD_CHECKS = {
  'build-and-test': ['v2-ci.yml', ['pull_request']],
  test: ['v2-tests.yml', ['pull_request']],
  coverage: ['v2-tests.yml', ['pull_request']],
  'golangci-lint': ['go-security-analysis.yml', ['pull_request']],
  'gosec (insecure patterns)': ['go-security-analysis.yml', ['pull_request']],
  'govulncheck (known vulnerabilities)': ['go-security-analysis.yml', ['pull_request']],
};
const SHA = /^[a-f0-9]{40}$/;
const BLOCKED_LABEL = /^(hold|needs-human|release-in-progress|do-not-merge(?:\/.*)?)$/i;
const MAX_QUEUE_PAGES = 10;
const POLICY_CHECK_NAME = 'native-queue-policy';
const POLICY_CHECK_SOURCE = ['native-queue-policy.yml', ['pull_request_target']];
// Decision B on #17030: a v5→v6 forward-merge opened by v6-topup.yml carries
// commits that already passed v5's DCO gate, and probot DCO cannot be repaired
// PR-side. Only that exact shape may substitute prow's `dco` status.
const FORWARD_MERGE = {
  head: 'sync/v5-to-v6', base: 'v6', source: 'v5',
  author: { login: 'github-actions[bot]', id: 41898282, type: 'Bot' },
};
const PROW = { login: 'kubestellar-prow[bot]', id: 208851060, context: 'dco' };
// REST pull-request commit listing stops at 250; a longer list is unverifiable.
const MAX_PR_COMMITS = 250;
const PROBOT_DCO_PATH = 'DCO path: probot DCO (App 1861)';

const closedNoop = (pr) => `PR #${pr.number} is closed; nothing to evaluate`;

class PendingEvidence extends Error {}

// A provider's inability to evaluate is not a negative sign-off verdict. It
// still blocks admission, and is deliberately not PendingEvidence: polling or
// retrying the admission job cannot repair the external provider.
const DCO_ENUMERATION_SUMMARY_PREFIX =
  'The DCO check could not be evaluated because the complete pull request commit list could not be retrieved.';
const DCO_ENUMERATION_SUMMARY_FALLBACK =
  'That fallback did not complete, so the DCO verdict is unknown.';
const DCO_PROVIDER_UNAVAILABLE_TEXT = 'DCO provider unavailable';
const DCO_PROVIDER_RUNBOOK = 'src/docs/operator-reference/dco-provider-unavailable.md';

class ProviderUnavailable extends Error {
  constructor(run, head) {
    super(`${DCO_PROVIDER_UNAVAILABLE_TEXT} on ${head}: verdict UNKNOWN (App ${DCO_APP_ID}, ` +
      `check run ${run.id}, conclusion ${run.conclusion}). Admission remains blocked. ` +
      'An operator must investigate the DCO provider enumeration path; see ' +
      `${DCO_PROVIDER_RUNBOOK}. No PR source change is requested.`);
  }
}

function dcoEnumerationUnavailable(run) {
  // This text only classifies a diagnostic, never supplies passing evidence.
  // Call only after validating provider, head and completed status.
  return run.name === 'DCO' && run.conclusion === 'failure' &&
    typeof run.output?.summary === 'string' &&
    run.output.summary.startsWith(DCO_ENUMERATION_SUMMARY_PREFIX) &&
    run.output.summary.includes(DCO_ENUMERATION_SUMMARY_FALLBACK);
}

function membersForGroup(entries, group) {
  if (!SHA.test(group.head_sha) || !SHA.test(group.base_sha) ||
      !group.base_ref?.startsWith('refs/heads/') ||
      !group.head_ref?.startsWith('refs/heads/gh-readonly-queue/')) {
    throw new Error('Malformed merge-group identity');
  }
  const ordered = [...entries].sort((a, b) => a.position - b.position);
  const last = ordered.findIndex(e => e.headCommit?.oid === group.head_sha);
  if (last < 0) throw new PendingEvidence('Group SHA is not present in the live queue');
  const members = ordered.slice(0, last + 1);
  const seen = new Set();
  for (const [index, entry] of members.entries()) {
    const pr = entry.pullRequest;
    if (entry.position !== index + 1 || !entry.id || !pr ||
        !Number.isSafeInteger(pr.number) || pr.number <= 0 ||
        !SHA.test(pr.headRefOid) || !SHA.test(entry.headCommit?.oid) ||
        !SHA.test(entry.baseCommit?.oid) || seen.has(pr.number) ||
        pr.baseRefName !== group.base_ref.slice('refs/heads/'.length)) {
      throw new Error('Incomplete or inconsistent queue membership');
    }
    seen.add(pr.number);
  }
  // The event's base and every intermediate merge must describe one chain;
  // guessing from the PR number in gh-readonly-queue/... misses earlier PRs.
  let base = group.base_sha;
  for (const entry of members) {
    if (entry.baseCommit.oid !== base) throw new Error('Queue base chain changed');
    base = entry.headCommit.oid;
  }
  return members;
}

function assertEligible(pr, head, base) {
  if (pr.state !== 'open' || pr.draft || pr.head?.sha !== head ||
      pr.base?.ref !== base || pr.labels?.some(l => BLOCKED_LABEL.test(l.name))) {
    throw new Error(`#${pr.number}: head/base moved, draft, closed, or held`);
  }
}

function evidenceForHead(runs, head, checks) {
  return Object.entries(checks).map(([name, source]) => {
    const run = runs.filter(r => r.name === name).sort((a, b) => b.id - a.id)[0];
    if (!run) throw new PendingEvidence(`Missing ${name} on ${head}`);
    const app = source ? ACTIONS_APP_ID : DCO_APP_ID;
    if (run.app?.id !== app || run.head_sha !== head) {
      throw new Error(`Untrusted provider or wrong head for ${name}`);
    }
    if (run.status !== 'completed') throw new PendingEvidence(`${name} is pending`);
    if (dcoEnumerationUnavailable(run)) throw new ProviderUnavailable(run, head);
    // Existing PR path scoping legitimately skips technical jobs only. Policy
    // proofs, especially external DCO, must have actually succeeded.
    const scopedSkip = Object.hasOwn(BUILD_CHECKS, name) && run.conclusion === 'skipped';
    if (run.conclusion !== 'success' && !scopedSkip) throw new Error(`${name}: ${run.conclusion}`);
    return { run, source };
  });
}

// Every Actions workflow shares one App id, so a same-named check run proves
// nothing: bind it to the expected workflow file, event, head, suite and job.
async function assertActionsProvenance(github, repo, number, head, run, source, workflows) {
  const url = new URL(run.details_url);
  const prefix = `/${repo.owner}/${repo.repo}/actions/runs/`;
  const id = url.pathname.startsWith(prefix) && url.pathname.slice(prefix.length).split('/')[0];
  if (url.hostname !== 'github.com' || !/^[0-9]+$/.test(id)) {
    throw new Error(`Invalid Actions provenance for ${run.name}`);
  }
  const runID = Number(id);
  if (!workflows.has(runID)) {
    const { data: workflow } = await github.rest.actions.getWorkflowRun({ ...repo, run_id: runID });
    const jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
      ...repo, run_id: runID, per_page: 100, filter: 'latest',
    });
    workflows.set(runID, { workflow, jobs });
  }
  const { workflow, jobs } = workflows.get(runID);
  const [file, events] = source;
  if (workflow.path !== `.github/workflows/${file}` || !events.includes(workflow.event) ||
      workflow.head_sha !== head || workflow.id !== runID) {
    throw new Error(`Unexpected workflow/event/head for ${run.name} on #${number}`);
  }
  // details_url is only a locator. The API-owned suite and job identities
  // must independently bind this exact check run to the expected workflow.
  const checkURL = `https://api.github.com/repos/${repo.owner}/${repo.repo}/check-runs/${run.id}`;
  if (!Number.isSafeInteger(run.check_suite?.id) ||
      run.check_suite.id !== workflow.check_suite_id ||
      !jobs.some(job => job.run_id === runID && job.name === run.name &&
        job.head_sha === head && job.check_run_url === checkURL &&
        job.status === 'completed' && job.conclusion === run.conclusion)) {
    throw new Error(`Check suite/job identity mismatch for ${run.name} on #${number}`);
  }
}

async function reachableFrom(github, repo, branch, commit) {
  const { data } = await github.rest.repos.compareCommitsWithBasehead({
    ...repo, basehead: `${branch}...${commit}`,
  });
  return ['behind', 'identical'].includes(data.status);
}

// Returns null when the PR is not the top-up shape at all, otherwise whether
// the narrow prow path applies and why. Any doubt keeps probot DCO required.
async function forwardMergeDCOPath(github, repo, pr, head) {
  const author = FORWARD_MERGE.author;
  if (pr?.head?.ref !== FORWARD_MERGE.head || pr.base?.ref !== FORWARD_MERGE.base ||
      pr.head.repo?.full_name !== `${repo.owner}/${repo.repo}` ||
      pr.user?.login !== author.login || pr.user.id !== author.id || pr.user.type !== author.type) {
    return null;
  }
  const commits = await github.paginate(github.rest.pulls.listCommits, {
    ...repo, pull_number: pr.number, per_page: 100,
  });
  const fallback = why => ({ prow: false, path: `${PROBOT_DCO_PATH}; ${FORWARD_MERGE.head} fallback: ${why}` });
  if (commits.length === 0 || commits.length >= MAX_PR_COMMITS) {
    return fallback(`commit list unverifiable (${commits.length} commits)`);
  }
  for (const commit of commits) {
    if (!SHA.test(commit.sha)) return fallback('malformed commit list');
    // The tip is v6-topup.yml's own `--no-ff` merge of origin/v5 onto v6, so it
    // is never on v5 itself: its parents must be v6 and v5 history exactly.
    if (commit.sha === head) {
      const [ours, theirs] = (commit.parents || []).map(p => p.sha);
      if (commit.parents?.length !== 2 || !SHA.test(ours) || !SHA.test(theirs) ||
          !await reachableFrom(github, repo, FORWARD_MERGE.base, ours) ||
          !await reachableFrom(github, repo, FORWARD_MERGE.source, theirs)) {
        return fallback(`head ${head} is not a merge of ${FORWARD_MERGE.source} into ${FORWARD_MERGE.base}`);
      }
    } else if (!await reachableFrom(github, repo, FORWARD_MERGE.source, commit.sha)) {
      return fallback(`${commit.sha} is not reachable from ${FORWARD_MERGE.source}`);
    }
  }
  if (!commits.some(c => c.sha === head)) return fallback(`head ${head} is not in the commit list`);
  return {
    prow: true,
    path: `DCO path: prow ${PROW.context} for ${FORWARD_MERGE.source}-reachable ${FORWARD_MERGE.head} forward-merge (#17030)`,
  };
}

async function assertProwDCO(github, repo, head) {
  const statuses = await github.paginate(github.rest.repos.listCommitStatusesForRef, {
    ...repo, ref: head, per_page: 100,
  });
  // Any workflow token can post a `dco` status; only prow's own is evidence.
  const status = statuses
    .filter(s => s.context === PROW.context && s.creator?.login === PROW.login && s.creator.id === PROW.id)
    .sort((a, b) => b.id - a.id)[0];
  if (!status) throw new PendingEvidence(`Missing prow ${PROW.context} on ${head}`);
  if (status.state === 'pending') throw new PendingEvidence(`prow ${PROW.context} is pending`);
  if (status.state !== 'success') throw new Error(`prow ${PROW.context}: ${status.state}`);
}

async function assertEvidence(github, repo, number, head, checks, pr) {
  const dco = Object.hasOwn(checks, 'DCO') ? await forwardMergeDCOPath(github, repo, pr, head) : null;
  const required = { ...checks };
  if (dco?.prow) delete required.DCO;
  const runs = await github.paginate(github.rest.checks.listForRef, {
    ...repo, ref: head, per_page: 100, filter: 'latest',
  });
  const workflows = new Map();
  try {
    for (const { run, source } of evidenceForHead(runs, head, required)) {
      if (!source) continue; // Probot DCO is deliberately not impersonated.
      await assertActionsProvenance(github, repo, number, head, run, source, workflows);
    }
    if (dco?.prow) await assertProwDCO(github, repo, head);
  } catch (error) {
    if (dco && !(error instanceof ProviderUnavailable)) error.message += ` [${dco.path}]`;
    throw error;
  }
  return dco?.path || PROBOT_DCO_PATH;
}

async function assertReviews(github, repo, number, head) {
  const reviews = await github.paginate(github.rest.pulls.listReviews, {
    ...repo, pull_number: number, per_page: 100,
  });
  const latest = new Map();
  for (const review of reviews.sort((a, b) => a.id - b.id)) {
    if (['APPROVED', 'CHANGES_REQUESTED', 'DISMISSED'].includes(review.state)) {
      latest.set(review.user?.login, review);
    }
  }
  for (const review of latest.values()) {
    if (review.state === 'CHANGES_REQUESTED' || review.state === 'DISMISSED' ||
        (review.state === 'APPROVED' && review.commit_id !== head)) {
      throw new Error(`#${number}: changes requested or stale approval`);
    }
  }
}

async function readQueue(github, repo, group) {
  const entries = [];
  let cursor = null;
  for (let page = 0; page < MAX_QUEUE_PAGES; page++) {
    const data = await github.graphql(`query($owner:String!,$repo:String!,$base:String!,$cursor:String) {
      repository(owner:$owner,name:$repo) { mergeQueue(branch:$base) {
        entries(first:100,after:$cursor) {
          nodes { id position baseCommit { oid } headCommit { oid }
            pullRequest { number headRefOid baseRefName } }
          pageInfo { hasNextPage endCursor }
        }
      } }
    }`, { ...repo, base: group.base_ref.slice('refs/heads/'.length), cursor });
    const queue = data.repository?.mergeQueue?.entries;
    if (!queue) throw new Error('Live queue unavailable');
    entries.push(...queue.nodes);
    if (entries.some(e => e.headCommit?.oid === group.head_sha) || !queue.pageInfo.hasNextPage) {
      return membersForGroup(entries, group);
    }
    cursor = queue.pageInfo.endCursor;
    if (!cursor) throw new Error('Incomplete queue pagination');
  }
  throw new Error('Queue exceeds bounded membership lookup');
}

function snapshot(members) {
  return JSON.stringify(members);
}

// A 5xx from GitHub is the API failing, not the evidence failing: three v5
// PRs saw `GET .../check-runs - 500` in the same second (#16258) and the
// bounded wait above threw on the first attempt. Retry it like pending
// evidence; the loop still fails closed when it never clears. 4xx stays a
// hard failure (missing refs and permissions do not fix themselves).
function isTransientAPIError(error) {
  const status = Number(error?.status);
  return Number.isInteger(status) && status >= 500 && status <= 599;
}

async function evaluate(args) {
  try {
    return await evaluateOnce(args);
  } catch (error) {
    if (!isTransientAPIError(error)) throw error;
    throw new PendingEvidence(`GitHub API ${error.status} (${error.message}); retrying`, { cause: error });
  }
}

// A labeled/unlabeled event only matters when the label is one assertEligible
// reads (BLOCKED_LABEL); every other flip cannot change the verdict.
function isCosmeticLabelFlip({ eventName, payload }) {
  return eventName === 'pull_request_target' &&
    ['labeled', 'unlabeled'].includes(payload.action) &&
    typeof payload.label?.name === 'string' &&
    !BLOCKED_LABEL.test(payload.label.name);
}

// A cosmetic flip skips the evidence wait but must not mint a fresh passing
// required check: replay the latest completed `success` run of this check on
// the head (rejecting one not from this workflow) and still read live PR
// eligibility and the POLICY_CHECKS evidence. Returns null when there is
// nothing to replay — no completed run, or the latest one did not succeed (a
// failed, cancelled or timed-out wait), or a policy provider is missing,
// still running or reporting UNKNOWN — so the caller falls through to the
// full evidence wait instead of pinning that earlier verdict onto every later
// stage-label flip.
async function replayLastVerdict({ github, context, currentCheckRunId }) {
  const repo = context.repo;
  const event = context.payload.pull_request;
  const { data: pr } = await github.rest.pulls.get({ ...repo, pull_number: event.number });
  if (pr.state === 'closed') return closedNoop(pr);
  assertEligible(pr, event.head.sha, event.base.ref);
  const runs = await github.paginate(github.rest.checks.listForRef, {
    ...repo, ref: pr.head.sha, check_name: POLICY_CHECK_NAME, per_page: 100, filter: 'all',
  });
  const prior = runs
    .filter(r => r.name === POLICY_CHECK_NAME && r.status === 'completed' &&
      r.head_sha === pr.head.sha && r.app?.id === ACTIONS_APP_ID && r.id !== currentCheckRunId)
    .sort((a, b) => b.id - a.id)[0];
  if (!prior) return null;
  // A same-named job from any other workflow shares the Actions App id; only
  // a run of this workflow file on this event may be replayed.
  await assertActionsProvenance(github, repo, pr.number, pr.head.sha, prior, POLICY_CHECK_SOURCE, new Map());
  if (prior.conclusion !== 'success') return null;
  // The prior success is only valid while the providers it validated are
  // still green: one re-run red on this head must not be papered over. A
  // provider that is missing or still running is not a verdict either way;
  // fall through so the caller runs the full polling wait instead of failing.
  // A DCO enumeration UNKNOWN also falls through: the full wait re-raises it
  // once and renders the runbook job summary, so operators see the same
  // surface whichever event fired.
  let path;
  try {
    path = await assertEvidence(github, repo, pr.number, pr.head.sha, POLICY_CHECKS, pr);
  } catch (error) {
    if (error instanceof PendingEvidence || error instanceof ProviderUnavailable) return null;
    throw error;
  }
  return `PR #${pr.number}: replayed ${POLICY_CHECK_NAME} run ${prior.id} (success) on ${pr.head.sha}; ${path}`;
}

async function evaluateOnce({ github, context }) {
  const repo = context.repo;
  if (context.eventName === 'pull_request_target') {
    const event = context.payload.pull_request;
    const { data: pr } = await github.rest.pulls.get({ ...repo, pull_number: event.number });
    // Post-merge label/comment edits re-fire this event; a closed PR has nothing to gate.
    if (pr.state === 'closed') return closedNoop(pr);
    assertEligible(pr, event.head.sha, event.base.ref);
    const path = await assertEvidence(github, repo, pr.number, pr.head.sha, POLICY_CHECKS, pr);
    const { data: current } = await github.rest.pulls.get({ ...repo, pull_number: event.number });
    if (current.state === 'closed') return closedNoop(current);
    assertEligible(current, event.head.sha, event.base.ref);
    return `PR #${pr.number}: five existing policy providers succeeded on ${pr.head.sha}; ${path}`;
  }
  if (context.eventName !== 'merge_group' || context.payload.action !== 'checks_requested') {
    throw new Error('Unsupported policy event');
  }
  const group = context.payload.merge_group;
  const members = await readQueue(github, repo, group);
  const paths = [];
  for (const entry of members) {
    const { number, headRefOid: head } = entry.pullRequest;
    const { data: pr } = await github.rest.pulls.get({ ...repo, pull_number: number });
    assertEligible(pr, head, group.base_ref.slice('refs/heads/'.length));
    const { data: comparison } = await github.rest.repos.compareCommitsWithBasehead({
      ...repo, basehead: `${head}...${group.head_sha}`,
    });
    if (!['ahead', 'identical'].includes(comparison.status) || comparison.merge_base_commit?.sha !== head) {
      throw new Error(`#${number}: current PR head is not contained in the tested group`);
    }
    await assertReviews(github, repo, number, head);
    paths.push(`#${number} ${await assertEvidence(github, repo, number, head, { ...POLICY_CHECKS, ...BUILD_CHECKS }, pr)}`);
  }
  // Re-read both membership and mutable gates after all evidence reads. This
  // bounds TOCTOU, but GitHub/Hive reconciliation is not an atomic transaction.
  if (snapshot(await readQueue(github, repo, group)) !== snapshot(members)) {
    throw new Error('Queue membership/head changed during validation');
  }
  for (const entry of members) {
    const { number, headRefOid: head } = entry.pullRequest;
    const { data: pr } = await github.rest.pulls.get({ ...repo, pull_number: number });
    assertEligible(pr, head, group.base_ref.slice('refs/heads/'.length));
    await assertReviews(github, repo, number, head);
  }
  const { data: ref } = await github.rest.git.getRef({
    ...repo, ref: group.head_ref.slice('refs/'.length),
  });
  if (ref.object?.sha !== group.head_sha) throw new Error('Synthetic group ref moved');
  return `Group ${group.head_sha}: validated all ${members.length} member heads (${paths.join('; ')})`;
}

module.exports = {
  ACTIONS_APP_ID, DCO_APP_ID, POLICY_CHECKS, BUILD_CHECKS,
  PendingEvidence, membersForGroup, assertEligible, evidenceForHead, closedNoop, evaluate,
  isTransientAPIError, isCosmeticLabelFlip, replayLastVerdict, BLOCKED_LABEL,
  ProviderUnavailable, FORWARD_MERGE, PROW,
  DCO_ENUMERATION_SUMMARY_PREFIX, DCO_ENUMERATION_SUMMARY_FALLBACK,
  DCO_PROVIDER_UNAVAILABLE_TEXT, DCO_PROVIDER_RUNBOOK,
};

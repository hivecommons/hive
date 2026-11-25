'use strict';

// This is an owned evidence gate, not a publisher of another App's statuses.
const ACTIONS_APP_ID = 15368;
const DCO_APP_ID = 1861;
// The PR-side events the gate and its legacy providers may run on.
// `pull_request_target` is blocked by default on public repositories from
// 2026-12-02, so the plain `pull_request` event is accepted beside it.
const PR_EVENTS = ['pull_request', 'pull_request_target'];
// The PR verifier also runs from `workflow_dispatch` (the github.token autofix
// path, where no PR event fires); that run is a legitimate source of evidence.
const LEGACY_VERIFIER_EVENTS = ['pull_request', 'pull_request_target', 'workflow_dispatch'];
// Contexts the hive may post as a commit status instead of an Actions check
// run. During migration either provider satisfies the context.
const HIVE_STATUS_CHECKS = new Set(['verify PR contents']);
// Repository variable naming the App bot login whose statuses count as hive
// evidence. Installations use differently named Apps, so it is configuration.
// Unset or blank means the hive provider is not configured, never "any creator".
const HIVE_CREATOR_ENV = 'HIVE_PR_GATE_CREATOR_LOGIN';
const POLICY_CHECKS = {
  gate: ['docker.yml', ['push', 'pull_request']],
  DCO: null,
  'changelog-fragment-guard': ['changelog-fragment-guard.yml', ['pull_request']],
  'verify PR contents': ['pr-verifier.yml', LEGACY_VERIFIER_EVENTS],
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
const POLICY_CHECK_SOURCE = ['native-queue-policy.yml', PR_EVENTS];
// Decision B on #17030: a v5→v6 forward-merge opened by v6-topup.yml carries
// commits that already passed v5's DCO gate, and probot DCO cannot be repaired
// PR-side. Only that exact shape may substitute prow's `dco` status.
const FORWARD_MERGE = {
  head: 'sync/v5-to-v6', base: 'v6', source: 'v5',
  author: { login: 'github-actions[bot]', id: 41898282, type: 'Bot' },
};
const PROW = { login: 'kubestellar-prow[bot]', id: 208851060, context: 'dco' };
// Line-to-line forward-merge heads (v6-topup.yml's `sync/v5-to-v6`, a hand-carried
// `sync/v5-to-v6-<date>`, `sync/v4-to-v5`, `topup/...`). They must land by merge
// commit: a squash or rebase keeps the content but drops the source line's
// history, and every later top-up then re-conflicts on all of it (#18931).
const FORWARD_MERGE_HEAD = /^(?:sync\/v\d+-to-v\d+|topup\/)/;
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

// A provider that completed red on this exact head is that PR's own verdict
// (a missing sign-off, a missing changelog fragment, a failed verifier), never
// a broken admission workflow. Name it as such: four PRs each red on their own
// provider read like one shared incident and got filed as one (#18280). It is
// a hard failure, deliberately not PendingEvidence: waiting cannot change a
// completed verdict, and only a new push to the PR re-runs the providers.
const PROVIDER_VERDICT_NOTE =
  'per-PR provider verdict, not a shared CI incident: fix it on this PR; the next push re-evaluates';

class ProviderVerdict extends Error {
  constructor(provider, conclusion, head) {
    super(`${provider}: ${conclusion} on ${head} (${PROVIDER_VERDICT_NOTE})`);
    this.provider = provider;
    this.conclusion = conclusion;
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

// Name the condition that tripped: every one of these is a per-PR state the
// author or an operator must change, never a shared CI incident. An opaque
// "moved, draft, closed, or held" verdict across several held or draft PRs
// reads like a broken workflow and gets filed as one (#18128).
function ineligibleReason(pr, head, base) {
  if (pr.state !== 'open') return `is ${pr.state}, not open`;
  if (pr.draft) return 'is a draft';
  if (pr.head?.sha !== head) return `head moved to ${pr.head?.sha} (event head ${head})`;
  if (pr.base?.ref !== base) return `base moved to ${pr.base?.ref} (event base ${base})`;
  const held = (pr.labels ?? []).filter(l => BLOCKED_LABEL.test(l.name)).map(l => l.name);
  if (held.length) return `is held by label ${held.join(', ')}`;
  return null;
}

function assertEligible(pr, head, base) {
  const reason = ineligibleReason(pr, head, base);
  if (reason) {
    throw new Error(`#${pr.number} ${reason}; not eligible for admission ` +
      '(PR state, not a CI failure: re-evaluated on the next synchronize, ready_for_review or hold-label change)');
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
    if (run.conclusion !== 'success' && !scopedSkip) throw new ProviderVerdict(name, run.conclusion, head);
    return { run, source };
  });
}

// The workflow run id an Actions check run's details_url points at, or null
// when it is not a github.com Actions run URL of this repository.
function actionsRunID(repo, run) {
  let url;
  try {
    url = new URL(run.details_url);
  } catch {
    return null;
  }
  const prefix = `/${repo.owner}/${repo.repo}/actions/runs/`;
  const id = url.pathname.startsWith(prefix) && url.pathname.slice(prefix.length).split('/')[0];
  return url.hostname === 'github.com' && /^[0-9]+$/.test(id) ? Number(id) : null;
}

// One workflow-run read per run id per evaluation. Jobs are read only when a
// run is bound (assertActionsProvenance), never for one that is passed over.
async function workflowRunFor(github, repo, runID, workflows) {
  if (!workflows.has(runID)) {
    const { data: workflow } = await github.rest.actions.getWorkflowRun({ ...repo, run_id: runID });
    workflows.set(runID, { workflow });
  }
  return workflows.get(runID);
}

// Every Actions workflow shares one App id, so a same-named check run proves
// nothing: bind it to the expected workflow file, event, head, suite and job.
async function assertActionsProvenance(github, repo, number, head, run, source, workflows) {
  const runID = actionsRunID(repo, run);
  if (runID === null) throw new Error(`Invalid Actions provenance for ${run.name}`);
  const cached = await workflowRunFor(github, repo, runID, workflows);
  const { workflow } = cached;
  const [file, events] = source;
  if (workflow.path !== `.github/workflows/${file}` || !events.includes(workflow.event) ||
      workflow.head_sha !== head || workflow.id !== runID) {
    throw new Error(`Unexpected workflow/event/head for ${run.name} on #${number}`);
  }
  if (!cached.jobs) {
    cached.jobs = await github.paginate(github.rest.actions.listJobsForWorkflowRun, {
      ...repo, run_id: runID, per_page: 100, filter: 'latest',
    });
  }
  const { jobs } = cached;
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

// All Actions workflows share one App id, so the newest run of a check name
// can come from another workflow: feedback.yml's `gate` job is skipped when a
// PR closes unmerged, and a same-head adoption (copy opened, original closed)
// leaves that skip newer than docker.yml's `gate`. Pass over, newest first,
// only runs that are completed as skipped AND whose suite-bound
// workflow run is another workflow file. The scan stops at the first run that
// is still running (no API call, so the pending poll costs what it did), did
// not conclude skipped (a foreign failure or success still blocks), is not an
// Actions run of this repository, or is not bound to its workflow run's
// suite; that run, and everything older, is left for
// evidenceForHead and assertActionsProvenance exactly as before. If every run
// of the name is passed over, the newest is bound here, so the result is its
// provenance failure, never its conclusion read as this PR's verdict.
async function sourceWorkflowRuns(github, repo, number, head, runs, checks, workflows) {
  const foreign = new Set();
  for (const [name, source] of Object.entries(checks)) {
    if (!source) continue; // Probot DCO is not an Actions workflow.
    const candidates = runs.filter(r => r.name === name).sort((a, b) => b.id - a.id);
    const passed = [];
    for (const run of candidates) {
      if (run.status !== 'completed' || run.conclusion !== 'skipped' || run.app?.id !== ACTIONS_APP_ID) break;
      const runID = actionsRunID(repo, run);
      if (runID === null) break;
      const { workflow } = await workflowRunFor(github, repo, runID, workflows);
      if (workflow.id !== runID || !Number.isSafeInteger(run.check_suite?.id) ||
          run.check_suite.id !== workflow.check_suite_id ||
          workflow.path === `.github/workflows/${source[0]}`) {
        break;
      }
      passed.push(run);
    }
    if (passed.length && passed.length === candidates.length) {
      await assertActionsProvenance(github, repo, number, head, passed[0], source, workflows);
    }
    for (const run of passed) foreign.add(run);
  }
  return foreign.size ? runs.filter(r => !foreign.has(r)) : runs;
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
  if (status.state !== 'success') throw new ProviderVerdict(`prow ${PROW.context}`, status.state, head);
}

// The configured hive App bot login, or '' when the hive provider is not
// configured. Whitespace-only counts as unset.
function hiveCreatorLogin(env = process.env) {
  const value = env?.[HIVE_CREATOR_ENV];
  return typeof value === 'string' ? value.trim() : '';
}

// Hive-posted commit status as evidence for a context. Anyone with a workflow
// token can post a status of any name, so only the configured App bot's own
// status counts; every other creator is ignored. Not configured is a plain
// error (never PendingEvidence: waiting cannot configure it), and callers
// only reach this when a login is set.
async function assertHiveStatus(github, repo, head, name, creatorLogin = hiveCreatorLogin()) {
  if (!creatorLogin) throw new Error(`Hive status provider is not configured (${HIVE_CREATOR_ENV} is unset)`);
  const statuses = await github.paginate(github.rest.repos.listCommitStatusesForRef, {
    ...repo, ref: head, per_page: 100,
  });
  const wanted = creatorLogin.toLowerCase();
  const status = statuses
    .filter(s => s.context === name && typeof s.creator?.login === 'string' &&
      s.creator.login.toLowerCase() === wanted)
    .sort((a, b) => b.id - a.id)[0];
  if (!status) throw new PendingEvidence(`Missing hive ${name} status on ${head}`);
  if (status.state === 'pending') throw new PendingEvidence(`hive ${name} status is pending`);
  if (status.state !== 'success') throw new ProviderVerdict(`hive ${name} status`, status.state, head);
}

// Accept either provider for one context. The legacy Actions evidence is
// validated exactly as before; the hive status is only consulted when it
// fails. A completed (non-pending) failure from either provider outranks a
// pending one, so a real verdict is never hidden behind a wait.
async function assertEitherProvider(github, repo, number, head, name, source, runs, workflows, creatorLogin) {
  let legacy;
  try {
    const chosen = await sourceWorkflowRuns(github, repo, number, head, runs, { [name]: source }, workflows);
    for (const { run, source: src } of evidenceForHead(chosen, head, { [name]: source })) {
      await assertActionsProvenance(github, repo, number, head, run, src, workflows);
    }
    return 'actions';
  } catch (error) {
    legacy = error;
  }
  try {
    await assertHiveStatus(github, repo, head, name, creatorLogin);
    return 'hive';
  } catch (hive) {
    throw !(legacy instanceof PendingEvidence) ? legacy : !(hive instanceof PendingEvidence) ? hive : legacy;
  }
}

async function assertEvidence(github, repo, number, head, checks, pr) {
  const dco = Object.hasOwn(checks, 'DCO') ? await forwardMergeDCOPath(github, repo, pr, head) : null;
  const required = { ...checks };
  if (dco?.prow) delete required.DCO;
  // With the hive provider unconfigured this set is empty and the legacy
  // path below is exactly what it was before.
  const creatorLogin = hiveCreatorLogin();
  const either = creatorLogin ? Object.keys(required).filter(n => HIVE_STATUS_CHECKS.has(n)) : [];
  const eitherSources = Object.fromEntries(either.map(n => [n, required[n]]));
  for (const n of either) delete required[n];
  const viaHive = [];
  const runs = await github.paginate(github.rest.checks.listForRef, {
    ...repo, ref: head, per_page: 100, filter: 'latest',
  });
  const workflows = new Map();
  try {
    const chosen = await sourceWorkflowRuns(github, repo, number, head, runs, required, workflows);
    for (const { run, source } of evidenceForHead(chosen, head, required)) {
      if (!source) continue; // Probot DCO is deliberately not impersonated.
      await assertActionsProvenance(github, repo, number, head, run, source, workflows);
    }
    if (dco?.prow) await assertProwDCO(github, repo, head);
    for (const [n, source] of Object.entries(eitherSources)) {
      if (await assertEitherProvider(github, repo, number, head, n, source, runs, workflows, creatorLogin) === 'hive') {
        viaHive.push(n);
      }
    }
  } catch (error) {
    if (dco && !(error instanceof ProviderUnavailable)) error.message += ` [${dco.path}]`;
    throw error;
  }
  const path = dco?.path || PROBOT_DCO_PATH;
  return viaHive.length ? `${path}; hive status accepted for ${viaHive.join(', ')}` : path;
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
  return PR_EVENTS.includes(eventName) &&
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
  if (PR_EVENTS.includes(context.eventName)) {
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
      // A queue configured to squash or rebase never contains the PR's own head
      // commit, so the containment test above is also the merge-method guard
      // for top-ups; name it, because "not contained" reads like a CI fault.
      throw new Error(FORWARD_MERGE_HEAD.test(pr.head?.ref ?? '')
        ? `#${number}: forward-merge PR ${pr.head.ref} must land with a merge commit, never a squash or rebase ` +
          `(the tested group does not contain its head ${head}, so v5's ancestry would be lost; #18931). ` +
          'Remove it from the queue and merge it with the merge-commit method'
        : `#${number}: current PR head is not contained in the tested group`);
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
  PR_EVENTS, LEGACY_VERIFIER_EVENTS, HIVE_STATUS_CHECKS, HIVE_CREATOR_ENV, POLICY_CHECK_SOURCE,
  hiveCreatorLogin, assertHiveStatus, assertEvidence,
  ProviderUnavailable, ProviderVerdict, PROVIDER_VERDICT_NOTE, FORWARD_MERGE, FORWARD_MERGE_HEAD, PROW,
  DCO_ENUMERATION_SUMMARY_PREFIX, DCO_ENUMERATION_SUMMARY_FALLBACK,
  DCO_PROVIDER_UNAVAILABLE_TEXT, DCO_PROVIDER_RUNBOOK,
};

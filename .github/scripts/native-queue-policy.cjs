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

class PendingEvidence extends Error {}

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
    // Existing PR path scoping legitimately skips technical jobs only. Policy
    // proofs, especially external DCO, must have actually succeeded.
    const scopedSkip = Object.hasOwn(BUILD_CHECKS, name) && run.conclusion === 'skipped';
    if (run.conclusion !== 'success' && !scopedSkip) throw new Error(`${name}: ${run.conclusion}`);
    return { run, source };
  });
}

async function assertEvidence(github, repo, number, head, checks) {
  const runs = await github.paginate(github.rest.checks.listForRef, {
    ...repo, ref: head, per_page: 100, filter: 'latest',
  });
  const workflows = new Map();
  for (const { run, source } of evidenceForHead(runs, head, checks)) {
    if (!source) continue; // Probot DCO is deliberately not impersonated.
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

async function evaluate({ github, context }) {
  const repo = context.repo;
  if (context.eventName === 'pull_request_target') {
    const event = context.payload.pull_request;
    const { data: pr } = await github.rest.pulls.get({ ...repo, pull_number: event.number });
    assertEligible(pr, event.head.sha, event.base.ref);
    await assertEvidence(github, repo, pr.number, pr.head.sha, POLICY_CHECKS);
    const { data: current } = await github.rest.pulls.get({ ...repo, pull_number: event.number });
    assertEligible(current, event.head.sha, event.base.ref);
    return `PR #${pr.number}: five existing policy providers succeeded on ${pr.head.sha}`;
  }
  if (context.eventName !== 'merge_group' || context.payload.action !== 'checks_requested') {
    throw new Error('Unsupported policy event');
  }
  const group = context.payload.merge_group;
  const members = await readQueue(github, repo, group);
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
    await assertEvidence(github, repo, number, head, { ...POLICY_CHECKS, ...BUILD_CHECKS });
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
  return `Group ${group.head_sha}: validated all ${members.length} member heads`;
}

module.exports = {
  ACTIONS_APP_ID, DCO_APP_ID, POLICY_CHECKS, BUILD_CHECKS,
  PendingEvidence, membersForGroup, assertEligible, evidenceForHead, evaluate,
};

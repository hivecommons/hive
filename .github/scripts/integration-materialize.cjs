'use strict';

const crypto = require('node:crypto');
const {execFileSync} = require('node:child_process');

const PROTOCOL = 'integration-prefix-v1';
const MAX_MEMBERS = 8;
const MAX_REQUEST_BYTES = 32 * 1024;
const MAX_GIT_OUTPUT_BYTES = 1024 * 1024;
const GIT_TIMEOUT_MS = 120000;
const SHA = /^[a-f0-9]{40}$/;
const DIGEST = /^[a-f0-9]{64}$/;
const DECIMAL_ID = /^[1-9][0-9]*$/;
const SAFE_POLICY_PATH = /^[a-zA-Z0-9_.-]+(?:\/[a-zA-Z0-9_.-]+)*$/;

function canonical(value) {
  if (value === null || typeof value !== 'object') return JSON.stringify(value);
  if (Array.isArray(value)) return `[${value.map(canonical).join(',')}]`;
  return `{${Object.keys(value).sort().map(key => `${JSON.stringify(key)}:${canonical(value[key])}`).join(',')}}`;
}

function digest(value) {
  return crypto.createHash('sha256').update(value).digest('hex');
}

function objectID(value) {
  return typeof value === 'string' && SHA.test(value) && !/^0+$/.test(value);
}

function reject(code, message) {
  const error = new Error(`${code}: ${message}`);
  error.reason = code;
  throw error;
}

function parseRequest(raw) {
  if (typeof raw !== 'string' || Buffer.byteLength(raw, 'utf8') > MAX_REQUEST_BYTES) {
    reject('request_size', 'bounded request required');
  }
  let request;
  try { request = JSON.parse(raw); } catch { reject('request_json', 'invalid request JSON'); }
  const fields = ['schema', 'protocol', 'engine', 'repo', 'base', 'baseSHA',
    'policy', 'generation', 'members', 'createdAt'];
  if (!request || request.schema !== 1 || request.protocol !== PROTOCOL ||
      Object.keys(request).some(key => !fields.includes(key)) ||
      request.engine !== 'hive' || typeof request.repo !== 'string' ||
      typeof request.base !== 'string' || !request.base.length ||
      !objectID(request.baseSHA) || typeof request.policy !== 'string' ||
      !DIGEST.test(request.policy) || /^0+$/.test(request.policy) ||
      !Number.isSafeInteger(request.generation) || request.generation < 1 ||
      !Array.isArray(request.members) || !request.members.length ||
      request.members.length > MAX_MEMBERS ||
      typeof request.createdAt !== 'string' ||
      !Number.isFinite(Date.parse(request.createdAt)) ||
      new Date(request.createdAt).toISOString() !== request.createdAt) {
    reject('request_identity', 'invalid versioned preparation identity');
  }
  const seen = new Set();
  const heads = new Set();
  for (const member of request.members) {
    if (!member || !Number.isSafeInteger(member.pr) || member.pr < 1 ||
        Object.keys(member).some(key => !['pr', 'head'].includes(key)) ||
        !objectID(member.head) || seen.has(member.pr) || heads.has(member.head)) {
      reject('request_member', 'invalid or duplicate ordered member');
    }
    seen.add(member.pr);
    heads.add(member.head);
  }
  return request;
}

function authorize(request, policyText, actorID, repo, sourceEvent, sourceRef, defaultBranch, eventAction) {
  let policy;
  try { policy = JSON.parse(policyText); } catch { reject('policy_json', 'invalid installed producer policy'); }
  const installedEvent = sourceEvent === 'workflow_dispatch' ||
    (sourceEvent === 'repository_dispatch' && eventAction === 'hive.integration.prepare.v1');
  if (!installedEvent || sourceRef !== `refs/heads/${defaultBranch}`) {
    reject('source_ref', 'materializer must use its trusted default-branch dispatch');
  }
  if (!policy || policy.schema !== 1 || policy.enabled !== true ||
      policy.authority !== 'forge-single-attempt-v1' ||
      !Array.isArray(policy.actorIDs) || policy.actorIDs.length !== 1 || !DECIMAL_ID.test(actorID) ||
      !policy.actorIDs.includes(actorID) || !Array.isArray(policy.scopes) ||
      request.repo !== repo || digest(policyText) !== request.policy ||
      !policy.scopes.some(scope => scope && scope.repo === repo && scope.base === request.base)) {
    reject('producer_not_enrolled', 'disabled, unauthorized actor, policy, or destination');
  }
  if (!Number.isSafeInteger(policy.maxMembers) || policy.maxMembers < 1 ||
      policy.maxMembers > MAX_MEMBERS || request.members.length > policy.maxMembers ||
      !Array.isArray(policy.protectedPaths) || !policy.protectedPaths.includes('.github') ||
      !policy.protectedPaths.length ||
      policy.protectedPaths.some(path => typeof path !== 'string' ||
        !SAFE_POLICY_PATH.test(path) || path.split('/').some(part => part === '.' || part === '..' || part === '.git'))) {
    reject('producer_bounds', 'invalid installed bounds or protected policy closure');
  }
  const committer = policy.committer;
  if (!committer || typeof committer.name !== 'string' || !committer.name.trim() ||
      typeof committer.email !== 'string' || !/^[^\s<>@\r\n]+@[^\s<>@\r\n]+$/.test(committer.email) ||
      /[\r\n<>]/.test(committer.name)) {
    reject('producer_committer', 'explicit valid DCO identity required');
  }
  return policy;
}

function gitRunner(token) {
  if (typeof token !== 'string' || !token) reject('producer_token', 'trusted job credential missing');
  const header = `AUTHORIZATION: basic ${Buffer.from(`x-access-token:${token}`).toString('base64')}`;
  const env = {
    ...process.env,
    GIT_CONFIG_GLOBAL: '/dev/null',
    GIT_CONFIG_SYSTEM: '/dev/null',
    GIT_CONFIG_COUNT: '1',
    GIT_CONFIG_KEY_0: 'http.https://github.com/.extraheader',
    GIT_CONFIG_VALUE_0: header,
    GIT_TERMINAL_PROMPT: '0',
    GIT_TRACE: '0',
    GIT_TRACE_CURL: '0',
    GIT_CURL_VERBOSE: '0',
  };
  return (args, extraEnv = {}) => execFileSync(
    'git', ['-c', 'core.hooksPath=/dev/null', '--literal-pathspecs', ...args],
    {env: {...env, ...extraEnv}, encoding: 'utf8', timeout: GIT_TIMEOUT_MS,
      maxBuffer: MAX_GIT_OUTPUT_BYTES, stdio: ['ignore', 'pipe', 'pipe']}
  ).trim();
}

function makeSubject(request, members, prefixes, workflowTree, preparedBy) {
  const subject = {
    schema: 1, protocol: PROTOCOL, engine: 'hive', repo: request.repo,
    base: request.base, baseSHA: request.baseSHA, policy: request.policy,
    workflowTree, generation: request.generation, members, prefixes,
  };
  const {generation, ...immutable} = subject;
  subject.id = digest(canonical(immutable));
  subject.preparedBy = preparedBy;
  return subject;
}

async function claimCandidateDispatch({github, source, subject, execution, committer, now}) {
  const key = digest(`integration-ci-initial-v1/${subject.id}`);
  const ref = `heads/hive-integration-intents/${key}`;
  let existing;
  try { existing = await github.rest.git.getRef({...source, ref}); } catch (error) {
    if (error.status !== 404) return {state: 'unknown', key, ref};
  }
  if (existing) return {state: 'observer', key, ref, commit: existing.data.object?.sha};
  const owner = {...execution, nonce: crypto.randomBytes(16).toString('hex')};
  const record = {schema: 1, kind: 'integration-ci-single-attempt-v1', key, subjectID: subject.id, owner};
  const last = subject.prefixes.at(-1);
  let claimCommit;
  try {
    const {data: commit} = await github.rest.git.createCommit({
      ...source, tree: last.tree, parents: [last.commit],
      message: `Integration CI intent ${key}\n\n${canonical(record)}\n\nSigned-off-by: ${committer.name} <${committer.email}>\n`,
      author: {...committer, date: now.toISOString()},
      committer: {...committer, date: now.toISOString()},
    });
    claimCommit = commit.sha;
    if (!objectID(claimCommit)) return {state: 'unknown', key, ref};
    const acknowledgement = await github.rest.git.createRef({...source, ref: `refs/${ref}`, sha: claimCommit});
    if (acknowledgement.status !== 201 || acknowledgement.data?.ref !== `refs/${ref}` ||
        acknowledgement.data.object?.sha !== claimCommit) {
      return {state: 'unknown', key, ref, commit: claimCommit, owner};
    }
    return {state: 'claimed', key, ref, commit: claimCommit, owner};
  } catch {
    try {
      const {data: current} = await github.rest.git.getRef({...source, ref});
      return {state: claimCommit && current.object?.sha === claimCommit ? 'claimed' : 'observer',
        key, ref, commit: current.object?.sha, owner};
    } catch { return {state: 'unknown', key, ref, commit: claimCommit, owner}; }
  }
}

async function prepare({github, context, requestText, policyText, actorID, execution, git, now = new Date()}) {
  if (context.serverUrl !== 'https://github.com' || context.apiUrl !== 'https://api.github.com') {
    reject('provider_unsupported', 'this installed producer supports public GitHub only; no credential forwarding');
  }
  const request = parseRequest(requestText);
  const repo = `${context.repo.owner}/${context.repo.repo}`;
  const policy = authorize(request, policyText, actorID, repo, context.eventName,
    context.ref, context.payload.repository.default_branch, context.payload.action);
  const maxRequestAgeMs = 15 * 60 * 1000;
  const futureClockAllowanceMs = 2 * 60 * 1000;
  const age = now.getTime() - Date.parse(request.createdAt);
  if (age > maxRequestAgeMs || age < -futureClockAllowanceMs) {
    reject('request_expired', 'request is outside the bounded preparation interval');
  }
  if (!execution || !DECIMAL_ID.test(execution.runID) || !DECIMAL_ID.test(execution.attempt)) {
    reject('executor_unknown', 'actual trusted job run/attempt is required; no TTL takeover');
  }
  git(['check-ref-format', `refs/heads/${request.base}`]);
  const sourceSHA = context.sha;
  if (!objectID(sourceSHA)) reject('source_sha', 'trusted source commit is unknown');
  const source = {owner: context.repo.owner, repo: context.repo.repo};
  const {data: destination} = await github.rest.git.getRef({...source, ref: `heads/${request.base}`});
  if (destination.object?.sha !== request.baseSHA) {
    reject('base_changed', 'requested destination is no longer its exact starting base');
  }
  const protectedObjects = new Map(policy.protectedPaths.map(path =>
    [path, git(['ls-tree', sourceSHA, '--', path])]));
  if ([...protectedObjects.values()].some(value => !value)) {
    reject('policy_closure_unknown', 'protected path missing from trusted source');
  }
  const workflowTree = git(['rev-parse', '--verify', `${sourceSHA}:.github`]);
  if (!objectID(workflowTree)) reject('policy_closure_unknown', 'trusted workflow tree missing');
  const gitVersion = git(['--version']);
  const requestDigest = digest(canonical(request));
  const preparedBy = {sourceSHA, requestDigest, gitVersion};
  const remote = `https://github.com/${repo}.git`;
  const prepared = [];
  const members = [];
  const prefixes = [];
  let parent = request.baseSHA;
  const commitEnv = {
    GIT_AUTHOR_NAME: policy.committer.name,
    GIT_AUTHOR_EMAIL: policy.committer.email,
    GIT_COMMITTER_NAME: policy.committer.name,
    GIT_COMMITTER_EMAIL: policy.committer.email,
  };
  let failure = null;
  try {
    git(['fetch', '--quiet', '--no-tags', '--depth=1', remote, request.baseSHA]);
    for (const member of request.members) {
      const {data: pr} = await github.rest.pulls.get({...source, pull_number: member.pr});
      if (pr.state !== 'open' || pr.draft || pr.head?.sha !== member.head ||
          pr.head?.repo?.full_name !== repo || pr.base?.repo?.full_name !== repo ||
          pr.base?.ref !== request.base) {
        reject('member_changed', `member #${member.pr} changed, forked, closed, or became draft`);
      }
      const {data: comparison} = await github.rest.repos.compareCommitsWithBasehead({
        ...source, basehead: `${parent}...${member.head}`, per_page: 1,
      });
      const mergeBase = comparison.merge_base_commit?.sha;
      if (!objectID(mergeBase)) reject('merge_base_unknown', `no authoritative merge base for #${member.pr}`);
      if (mergeBase === member.head) reject('member_integrated', `#${member.pr} already occurs in the prefix`);
      git(['fetch', '--quiet', '--no-tags', '--depth=1', remote, member.head, mergeBase]);
      let tree;
      try {
        tree = git(['merge-tree', '--write-tree', `--merge-base=${mergeBase}`, parent, member.head]).split('\n')[0];
      } catch (error) {
        if (error.status === 1) {
          reject('materialization_conflict', `ordered prefix through #${member.pr} needs owner-first repair`);
        }
        reject('materialization_unknown', `Git execution for #${member.pr} is unavailable; not a proven textual conflict`);
      }
      if (!objectID(tree)) reject('materialization_unknown', 'merge did not return an exact tree');
      const timestamp = Math.max(...[parent, member.head].map(sha => {
        const value = git(['show', '-s', '--format=%ct', sha]);
        if (!DECIMAL_ID.test(value) || !Number.isSafeInteger(Number(value))) {
          reject('commit_time_unknown', 'deterministic parent commit timestamp unavailable');
        }
        return Number(value);
      }));
      const date = new Date((timestamp + 1) * 1000);
      if (!Number.isFinite(date.getTime())) reject('commit_time_unknown', 'parent commit timestamp is out of range');
      const prefixKey = digest(canonical({protocol: PROTOCOL, repo, base: request.base,
        baseSHA: request.baseSHA, policy: request.policy, workflowTree,
        members: [...members, member]}));
      const message = `Integration candidate ${prefixKey}\n\nSigned-off-by: ${policy.committer.name} <${policy.committer.email}>\n`;
      const commit = git(['-c', 'commit.gpgsign=false', 'commit-tree', tree,
        '-p', parent, '-p', member.head, '-m', message],
      {...commitEnv, GIT_AUTHOR_DATE: date.toISOString(), GIT_COMMITTER_DATE: date.toISOString()});
      if (!objectID(commit)) reject('materialization_unknown', 'merge did not return an exact commit');
      for (const [path, expected] of protectedObjects) {
        if (git(['ls-tree', commit, '--', path]) !== expected) {
          reject('policy_change', `candidate changes installed policy closure ${path}; separate qualification required`);
        }
      }
      members.push({...member});
      prefixes.push({commit, tree});
      const subject = makeSubject(request, [...members], [...prefixes], workflowTree, preparedBy);
      // GitHub-token publication suppresses unrelated push-triggered workflows.
      // No candidate tree is checked out and no author or destination ref is changed.
      const result = {subject, publication: 'unresolved', dispatch: 'not_requested'};
      prepared.push(result);
      git(['push', '--quiet', remote, `${commit}:refs/tags/hive-integration/${subject.id}`]);
      result.publication = 'accepted';
      result.ownership = await claimCandidateDispatch({github, source, subject, execution, committer: policy.committer, now});
      if (result.ownership.state === 'unknown') {
        reject('dispatch_authority_unknown', 'shared dispatch intent is unresolved; no TTL steal or mutation');
      }
      if (result.ownership.state === 'observer') {
        result.dispatch = 'observer';
        parent = commit;
        continue;
      }
      subject.dispatchClaim = result.ownership;
      const manifest = canonical(subject);
      if (Buffer.byteLength(manifest, 'utf8') > MAX_REQUEST_BYTES) reject('subject_size', 'candidate manifest exceeds dispatch bounds');
      result.dispatch = 'unresolved';
      try {
        const acknowledgement = await github.rest.actions.createWorkflowDispatch({
          ...source, workflow_id: 'integration-candidate.yml',
          ref: `hive-integration/${subject.id}`, inputs: {manifest},
        });
        if (acknowledgement.status !== 204) throw new Error('unknown dispatch acknowledgement');
        result.dispatch = 'accepted';
      } catch {
        reject('dispatch_unresolved', `candidate ${subject.id} dispatch has no authoritative acknowledgement; reconcile, never blindly retry`);
      }
      parent = commit;
    }
  } catch (error) {
    failure = {reason: error.reason || 'provider_unknown', message: error.reason ? error.message : 'Provider/materializer failed; outcome requires bounded reconciliation'};
  }
  return {schema: 1, protocol: 'integration-materializer-receipt-v1',
    request, prepared, failure, preparedBy};
}

module.exports = {authorize, canonical, claimCandidateDispatch, digest, gitRunner, makeSubject, parseRequest, prepare};

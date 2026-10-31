'use strict';

const crypto = require('node:crypto');
const cp = require('node:child_process');
const fs = require('node:fs');
const path = require('node:path');
const MAX_MEMBERS = 8;
const MAX_COMMITS = 1000;
const MAX_AUTHOR_BATCH = 50;
// This literal is inside the enrolled .github closure. A caller-supplied
// preparedBy.sourceSHA must never choose a new executable verifier tree.
const TRUSTED_SCRIPTS_TREE = 'c1b6e83ee24e5108657ef7f9b15865fd23f4ebce';
const SOURCE = 'https://github.com/kubernetes-sigs/kubebuilder-release-tools/tree/012269a88fa4c034a0acf1ba84c26b195c0dbab4';
const sha = value => crypto.createHash('sha256').update(value).digest('hex');
const git = (args, cwd = process.cwd()) =>
  cp.execFileSync('git', ['-c', 'core.hooksPath=/dev/null', ...args], {cwd, encoding: 'utf8'}).trim();

function row(pr) {
  return [pr.number, pr.head.sha, pr.base.repo.full_name, sha(pr.base.ref),
    pr.user.id, pr.user.login, Boolean(pr.draft), sha(pr.title), sha(pr.body || ''),
    pr.labels.map(label => sha(label.name)).sort(), sha(pr.head.ref)];
}
const digest = members => sha(JSON.stringify(members.map(row)));

async function snapshot(github, subject) {
  if (!Array.isArray(subject.members) || !subject.members.length || subject.members.length > MAX_MEMBERS ||
      subject.members.length !== subject.prefixes.length || subject.repo.split('/').length !== 2) {
    throw new Error('Invalid bounded candidate membership');
  }
  const [owner, repo] = subject.repo.split('/');
  const members = [], seen = new Set();
  for (const [index, member] of subject.members.entries()) {
    const {data: pr} = await github.rest.pulls.get({owner, repo, pull_number: member.pr});
    if (seen.has(member.pr) || pr.number !== member.pr || pr.head.sha !== member.head ||
        pr.head.repo?.full_name !== subject.repo || pr.base.repo.full_name !== subject.repo ||
        pr.base.ref !== subject.base || pr.draft || !pr.user?.id || !pr.user.login ||
        typeof pr.title !== 'string' || !Array.isArray(pr.labels) ||
        !(pr.state === 'open' || pr.merged === true && pr.merge_commit_sha === subject.prefixes[index].commit)) {
      throw new Error(`Member ${member.pr} does not match exact candidate provenance`);
    }
    seen.add(member.pr);
    members.push(pr);
  }
  return members;
}

// Readonly adaptation of verify/type.go, verify/issue.go and notes/common/prefix.go
// at SOURCE (SPDX-License-Identifier: Apache-2.0, Copyright 2020 The Kubernetes Authors).
// https://www.apache.org/licenses/LICENSE-2.0 . Unlike the
// advisory image wrapper, unavailable validation never becomes success.
function verifyContents(title) {
  const trimmed = title.replace(/^\W?WIP\W/iu, '').trim().replace(/^\[[\w.-]*\]/, '').trim();
  const prefixes = ['✨', '🐛', '📖', '🌱', '⚠', '🚀', '🏃',
    ':sparkles:', ':bug:', ':book:', ':seedling:', ':warning:', ':rocket:', ':running:'];
  if (!prefixes.some(prefix => trimmed.startsWith(prefix)) || /#[0-9]+\b/.test(title)) {
    throw new Error(`PR content policy rejected title; immutable source ${SOURCE}`);
  }
}

function run(script, args, cwd, extra = {}) {
  cp.execFileSync('bash', [path.resolve('src/scripts', script), ...args], {
    cwd, stdio: 'inherit', env: {...process.env, ...extra},
  });
}

async function attributionAuthors(github, subject, members) {
  const commits = [...new Set(members.flatMap(pr =>
    git(['rev-list', '--no-merges', `${subject.baseSHA}..${pr.head.sha}`]).split('\n').filter(Boolean)))];
  if (!github?.graphql || !commits.length || commits.length > MAX_COMMITS) {
    throw new Error('Exact bounded commit-author attribution unavailable');
  }
  const [owner, name] = subject.repo.split('/'), mapping = [];
  for (let start = 0; start < commits.length; start += MAX_AUTHOR_BATCH) {
    const batch = commits.slice(start, start + MAX_AUTHOR_BATCH), variables = {owner, name};
    const declarations = ['$owner:String!', '$name:String!'], fields = [];
    for (const [index, oid] of batch.entries()) {
      declarations.push(`$oid${index}:GitObjectID!`); variables[`oid${index}`] = oid;
      fields.push(`c${index}:object(oid:$oid${index}){... on Commit{oid author{user{login}}}}`);
    }
    const {repository} = await github.graphql(`query(${declarations.join(',')}){repository(owner:$owner,name:$name){nameWithOwner ${fields.join(' ')}}}`, variables);
    if (repository?.nameWithOwner !== subject.repo) throw new Error('Commit-author repository differs');
    for (const [index, oid] of batch.entries()) {
      const actual = repository[`c${index}`], login = actual?.author?.user?.login;
      if (actual?.oid !== oid || !/^[A-Za-z0-9][A-Za-z0-9-]*(?:\[bot\])?$/.test(login || '')) {
        throw new Error('Original commit author association unknown; candidate cannot use degraded attribution success');
      }
      mapping.push(`${oid}=${login}`);
    }
  }
  return mapping.join(',');
}

async function verify(kind, subject, members, github) {
  if (!['dco', 'changelog', 'contents', 'attribution'].includes(kind)) throw new Error('Unknown governance policy');
  const root = process.cwd(), source = subject.preparedBy?.sourceSHA;
  if (!/^[a-f0-9]{40}$/.test(source || '') ||
      git(['rev-parse', `${source}:.github`]) !== subject.workflowTree ||
      git(['rev-parse', `${source}:src/scripts`]) !== TRUSTED_SCRIPTS_TREE ||
      git(['rev-parse', 'HEAD:src/scripts']) !== TRUSTED_SCRIPTS_TREE) {
    throw new Error('Trusted control/helper closure unavailable; no candidate script execution');
  }
  if (kind === 'contents') {
    for (const pr of members) verifyContents(pr.title);
    return;
  }
  const tested = subject.prefixes.at(-1).commit;
  const range = `${subject.baseSHA}..${tested}`;
  const count = Number(git(['rev-list', '--count', range]));
  if (!Number.isSafeInteger(count) || count < 1 || count > MAX_COMMITS) throw new Error('Introduced governance commit cohort unknown/too large');
  if (kind === 'dco') {
    run('check-dco-trailers.sh', [String(count), range], root, {
      DCO_SKIP_BOT_AUTHORS: 'false', DCO_WAIVED_COMMITS: '', DCO_ALLOWLIST_EMAILS: '',
      DCO_AUTHOR_LOGIN_MAP: '', DCO_SAME_PERSON_EVIDENCE_REFS: '',
      DCO_GITHUB_REPOSITORY: subject.repo,
    });
    for (const prefix of subject.prefixes) {
      const author = git(['show', '-s', '--format=%an <%ae>', prefix.commit]);
      const message = git(['show', '-s', '--format=%B', prefix.commit]);
      if (!message.split('\n').some(line => line.toLowerCase() === `Signed-off-by: ${author}`.toLowerCase())) {
        throw new Error('Generated merge lacks its own actual author signoff');
      }
    }
    return;
  }
  if (kind === 'attribution') {
    const authors = await attributionAuthors(github, subject, members);
    for (const pr of members) {
      run('check-squash-signoff-attribution.sh', [subject.baseSHA, pr.head.sha, pr.user.login], root, {
        PR_AUTHOR_LOGIN: pr.user.login,
        PR_MERGE_METHOD: pr.labels.some(label => label.name === 'merge-method/merge') ? 'merge' : '',
        LANDED_REFS: subject.baseSHA,
        DCO_AUTHOR_LOGIN_MAP: authors,
      });
    }
    return;
  }
  const work = path.resolve('.integration-governance');
  fs.mkdirSync(work, {recursive: true});
  for (const pr of members) {
    const target = path.join(work, String(pr.number));
    git(['worktree', 'add', '--detach', target, pr.head.sha]);
    try {
      run('check-changelog-fragment.sh', [subject.base], target, {
        BASE_SHA: subject.baseSHA, HEAD_REF: pr.head.ref,
        HAS_NO_CHANGELOG_LABEL: String(pr.labels.some(label => label.name === 'no-changelog')),
      });
    } finally { git(['worktree', 'remove', '--force', target]); }
  }
  if (git(['diff', '--name-only', subject.baseSHA, subject.prefixes.at(-1).commit])
    .split('\n').some(file => /^src\/pkg\/(hub|dashboard)\/server\.go$|^src\/pkg\/(hub|dashboard)\/static\//.test(file))) {
    run('check-api-reference-citations.sh', [], root);
  }
}

module.exports = {SOURCE, row, digest, snapshot, verifyContents, verify};

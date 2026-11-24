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
const TRUSTED_SCRIPTS_TREE = 'e6cd3c24b1fdeb9b193296170100328c2c8cca48';
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
// https://www.apache.org/licenses/LICENSE-2.0 . The same two verdicts back
// the required `PR Type` / `PR Issue` checks that pr-verifier.yml posts
// (.github/scripts/pr-verifier.cjs, #18764) and the integration `contents`
// policy below, where unavailable validation never becomes success.
// [type, String(), shortcodes and emoji in the order PRTypeFromTitle tries them]
const PR_TYPES = [
  ['feature', ':sparkles:', '✨'], ['bugfix', ':bug:', '🐛'], ['docs', ':book:', '📖'],
  ['infra', ':seedling:', '🌱'], ['breaking', ':warning:', '⚠'], ['release', ':rocket:', '🚀'],
  ['infra', ':running:', '🏃'],
];
const EMOJI = {feature: '✨', bugfix: '🐛', docs: '📖', infra: '🌱', breaking: '⚠', release: '🚀'};

// notes/common/prefix.go PRTypeFromTitle: [type or null, title without prefix].
function prTypeFromTitle(title) {
  title = title.trim();
  for (const [type, code, emoji] of PR_TYPES) {
    if (title.startsWith(code) || title.startsWith(emoji)) {
      title = title.startsWith(code) ? title.slice(code.length) : title;
      title = title.startsWith(emoji) ? title.slice(emoji.length) : title;
      title = title.startsWith('\uFE0F') ? title.slice(1) : title;
      return [type, title.trim()];
    }
  }
  return [null, title];
}

// Go's %#q: a backquoted string when it can be, else a quoted one.
const goQuote = text => (/[`\u0000-\u0008\u000A-\u001F\u007F\uFEFF]/.test(text) ? JSON.stringify(text) : `\`${text}\``);

// verify/type.go verifyPRType; output title from verify/main.go.
function prTypeVerdict(rawTitle) {
  const title = String(rawTitle ?? '').replace(/^\W?WIP\W/iu, '').trim().replace(/^\[[\w.-]*\]/, '').trim();
  const [type, finalTitle] = prTypeFromTitle(title);
  if (!type) {
    return {name: 'PR Type', conclusion: 'failure', title: 'PR Type in Title',
      summary: 'No matching PR type indicator found in title.',
      text: `I saw a title of ${goQuote(title)}, which doesn't seem to have any of the acceptable prefixes.

You need to have one of these as the prefix of your PR title:

- Breaking change: ⚠ (\`:warning:\`)
- Non-breaking feature: ✨ (\`:sparkles:\`)
- Patch fix: 🐛 (\`:bug:\`)
- Docs: 📖 (\`:book:\`)
- Release: 🚀 (\`:rocket:\`)
- Infra/Tests/Other: 🌱 (\`:seedling:\`)

More details can be found at [sigs.k8s.io/kubebuilder-release-tools/VERSIONING.md](https://sigs.k8s.io/kubebuilder-release-tools/VERSIONING.md).`};
  }
  return {name: 'PR Type', conclusion: 'success', title: 'PR Type in Title',
    summary: `Found ${EMOJI[type]} PR (${type})`, text: `Final title:\n\n\t${finalTitle}\n`};
}

// verify/issue.go checkIssueInTitle; output title from verify/main.go.
function prIssueVerdict(rawTitle) {
  const [, title] = prTypeFromTitle(String(rawTitle ?? ''));
  if (/#[0-9]+\b/.test(title)) {
    return {name: 'PR Issue', conclusion: 'failure', title: 'Issue/PR tag in PR title',
      summary: 'Your PR has an Issue or PR number in the title.',
      text: `The title should just be descriptive.
Issue numbers belong in the PR body as either \`Fixes #XYZ\` (if it closes the issue or PR), or something like \`Related to #XYZ\` (if it's just related).`};
  }
  return {name: 'PR Issue', conclusion: 'success', title: 'Issue/PR tag in PR title',
    summary: 'Your PR title does not contain any Issue or PR tags', text: ''};
}

function verifyContents(title) {
  if (prTypeVerdict(title).conclusion !== 'success' || prIssueVerdict(title).conclusion !== 'success') {
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
      git(['rev-parse', `${source}:src/scripts`]) !== TRUSTED_SCRIPTS_TREE) {
    throw new Error('Trusted control/helper closure unavailable; no candidate script execution');
  }
  const scriptsTree = git(['rev-parse', 'HEAD:src/scripts']);
  if (scriptsTree !== TRUSTED_SCRIPTS_TREE) {
    throw new Error(`src/scripts tree ${scriptsTree} is not the pinned TRUSTED_SCRIPTS_TREE ${TRUSTED_SCRIPTS_TREE}; a PR that edits src/scripts must re-pin the literal in .github/scripts/integration-governance.cjs to git rev-parse HEAD:src/scripts`);
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
      DCO_AUTHOR_LOGIN_MAP: '', DCO_SAME_PERSON_EVIDENCE_REFS: '', DCO_PR_EVIDENCE: 'false',
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

module.exports = {SOURCE, row, digest, snapshot, prTypeVerdict, prIssueVerdict, verifyContents, verify};

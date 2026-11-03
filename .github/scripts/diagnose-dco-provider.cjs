'use strict';

// Read-only enumeration reproducer, NOT a DCO evaluator or status publisher.
// Selection mirrors dcoapp/app index.js at 828cc0b29a141821ddecd674bca0d8e634234c18.
const { execFileSync } = require('node:child_process');
const QUERY = `query PullRequestCommits(
  $owner: String!, $repo: String!, $pullNumber: Int!, $cursor: String, $parentLimit: Int!
) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pullNumber) {
      headRefOid
      commits(first: 100, after: $cursor) {
        totalCount
        nodes { commit {
          oid url message
          author { name email date user { login __typename } }
          committer { name email date user { login __typename } }
          signature { isValid state signature payload }
          parents(first: $parentLimit) { nodes { oid url } }
        } }
        pageInfo { hasNextPage endCursor }
      }
    }
  }
}`;
const SHA = /^[a-f0-9]{40}$/;

function ghJSON(args, input) {
  return JSON.parse(execFileSync('gh', args, {
    input: input && JSON.stringify(input), encoding: 'utf8',
    stdio: ['pipe', 'pipe', 'pipe'], maxBuffer: 64 * 1024 * 1024,
  }));
}

async function diagnose({ owner, repo, number, parentLimit = 100 }, api = ghJSON) {
  if (!/^[\w.-]+$/.test(owner) || !/^[\w.-]+$/.test(repo) ||
      !Number.isSafeInteger(number) || number <= 0 || ![2, 100].includes(parentLimit)) {
    throw new Error('Usage: node .github/scripts/diagnose-dco-provider.cjs OWNER REPO PR [--parents=2|100]');
  }
  const endpoint = `repos/${owner}/${repo}/pulls/${number}`;
  const pr = await api(['api', endpoint]);
  const head = pr.head?.sha;
  if (!SHA.test(head) || !Number.isSafeInteger(pr.commits) || pr.commits <= 0) {
    throw new Error('Missing PR head or expected commit count');
  }
  const restPages = await api(['api', `${endpoint}/commits?per_page=100`, '--paginate', '--slurp']);
  const restCommits = restPages.flat();
  const commits = new Map();
  const cursors = new Set();
  let cursor = null;
  let expected;
  for (let page = 0; ; page++) {
    // gh uses POST for GraphQL queries; this query only reads repository data.
    const response = await api(['api', 'graphql', '--input', '-'], {
      query: QUERY, variables: { owner, repo, pullNumber: number, cursor, parentLimit },
    });
    if (response.errors?.length) {
      throw new Error(`GraphQL errors: ${JSON.stringify(response.errors)}`);
    }
    const current = response.data?.repository?.pullRequest;
    if (current?.headRefOid !== head) throw new Error('PR head changed or unavailable during GraphQL enumeration');
    const connection = current.commits;
    if (!Number.isSafeInteger(connection?.totalCount) || connection.totalCount !== pr.commits ||
        (expected !== undefined && connection.totalCount !== expected)) {
      throw new Error('Incomplete or changed GraphQL totalCount');
    }
    expected = connection.totalCount;
    if (!Array.isArray(connection.nodes)) throw new Error('Missing GraphQL commit nodes');
    for (const node of connection.nodes) {
      const commit = node?.commit;
      if (!SHA.test(commit?.oid) || commits.has(commit.oid)) {
        throw new Error('Malformed or duplicate GraphQL commit');
      }
      commits.set(commit.oid, commit);
    }
    const info = connection.pageInfo;
    if (typeof info?.hasNextPage !== 'boolean') throw new Error('Missing GraphQL pageInfo');
    if (!info.hasNextPage) break;
    if (!info.endCursor || cursors.has(info.endCursor) || page + 1 >= Math.ceil(expected / 100) + 1) {
      throw new Error('GraphQL pagination did not advance or exceeded expected pages');
    }
    cursor = info.endCursor;
    cursors.add(cursor);
  }
  if (commits.size !== expected || !commits.has(head) ||
      restCommits.some(commit => !commits.has(commit.sha))) {
    throw new Error(`Incomplete history: GraphQL returned ${commits.size} of ${expected} commits, or REST/head mismatch`);
  }
  const final = await api(['api', endpoint]);
  if (final.head?.sha !== head || final.commits !== expected || final.base?.sha !== pr.base?.sha) {
    throw new Error('PR head/base/count changed during diagnosis');
  }
  return {
    head, restCount: restCommits.length, graphQLCount: commits.size, parentLimit,
    // Full records remain available to callers inspecting mapping/sign-off
    // failures beyond commit 250; the CLI only prints enumeration metadata.
    commits: [...commits.values()],
  };
}

if (require.main === module) {
  const [owner, repo, pr, option, ...extra] = process.argv.slice(2);
  const parentLimit = option === undefined ? 100 : Number(option.replace(/^--parents=/, ''));
  Promise.resolve().then(() => {
    if (extra.length || (option && !/^--parents=(2|100)$/.test(option))) throw new Error('Invalid arguments');
    return diagnose({ owner, repo, number: Number(pr), parentLimit });
  }).then(({ commits, ...result }) => {
    console.log(JSON.stringify({ ...result, note: 'Enumeration only; no DCO verdict or admission proof.' }, null, 2));
  }).catch(error => {
    console.error('DCO enumeration UNKNOWN:', error.stderr?.toString() || error.message);
    process.exitCode = 1;
  });
}

module.exports = { diagnose, QUERY };

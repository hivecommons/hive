# Hive CodeQL queries

`hive-queries/` is a CodeQL query pack for Go with hive's own taint queries.

| Query | Id | Flow |
|---|---|---|
| `UntrustedGitHubTextReachesKick.ql` | `hive/go/untrusted-github-text-to-kick` | The title or body of an issue or pull request, or the body of a comment, (`pkg/github`) reaches a `scheduler.KickMessage`'s `Message` without first passing `ioscan.EnforceInputFrom`, `EnforceInput`, `EnforceThread[From]`, or a scheduler wrapper (`enforceTextFrom`, `enforceIssueTextVerdict`, `enforceThread`, `enforceLabelsWithPolicy`). |

Package names are matched by their `/pkg/<name>` suffix, so the same query runs
against `github.com/hivecommons/hive` and the fixture module.

## Tests

`fixtures/untrusted-text-to-kick/` is a small Go module that stubs the three
packages. A line that ends in `// $ Alert` must be reported, and no other line
may be. `.github/workflows/static-security.yml` (job `codeql (hive query
fixture)`) builds the fixture under CodeQL, runs the query, and compares the
SARIF with those markers using `check-fixture-alerts.py`. Both a missed alert
and an unexpected one fail the job.

The `codeql (hive query on src, report)` job runs the query on `src/` after a
merge and weekly. It reports each result as a warning annotation and does not
upload to code scanning or block anything. Results on `src/` are triaged the
same way as Semgrep findings: fix the flow, or, if the text really is not
GitHub-authored, change the query and its fixture together.

## Running locally

```sh
cd .github/codeql/fixtures/untrusted-text-to-kick
codeql database create /tmp/kick-db --language=go --command='go build ./...'
codeql database analyze /tmp/kick-db ../../hive-queries/UntrustedGitHubTextReachesKick.ql \
  --format=sarif-latest --output=/tmp/kick.sarif
python3 ../../check-fixture-alerts.py /tmp/kick.sarif . hive/go/untrusted-github-text-to-kick
```

# Code scanning issues

GitHub code scanning (CodeQL, OSSF Scorecard, Trivy, or any tool that uploads
SARIF) shows its findings on a repository's **Security → Code scanning** tab.
Nothing there reaches the hive's lanes on its own: an owner has to read the
alerts and file issues by hand.

The code scanning sweep does that filing. It reads a repository's open alerts,
groups them into a few issues sized for one PR each, and keeps those issues in
step with the alerts. It is **off by default** and turned on per repository.

It opens issues only. It never opens PRs, never dismisses an alert, and never
changes the code scanning setup.

## Turning it on

```yaml
code_scanning:
  repos:
    acme/widget:              # owner/repo, or the bare name under project.org
      enabled: true
```

That is enough: the defaults file `critical` and `high` alerts, at most five new
issues per repository per pass, once an hour. Every key under `code_scanning`
is a default for all repositories. `repos.<repo>` turns one repository on or
off and can override any of them.

| Key | Default | What it does |
| --- | --- | --- |
| `code_scanning.enabled` | `false` | Turn the sweep on for every repository in `project.repos`. A `repos.<repo>.enabled` value wins. |
| `code_scanning.interval` | `1h` | Time between passes. At least `10m`. |
| `code_scanning.min_severity` | `high` | The lowest severity filed: `critical`, `high`, `medium`, `low` or `note` (see [Severity](#severity)). |
| `code_scanning.include_tools` | every tool | File only alerts from these tools, by name: `CodeQL`, `Scorecard`, `Trivy`, … |
| `code_scanning.exclude_rules` | the [posture checks](#what-is-not-filed) | Rule-ID patterns to leave out, added to the default list. `*` matches within a path segment (`cpp/commented-out-*`). An entry starting with `!` takes a default back out (`!SASTID`). |
| `code_scanning.max_new_issues_per_run` | `5` | The most new issues filed in one repository in one pass. |
| `code_scanning.repos.<repo>.enabled` | the top-level `enabled` | Turn this repository on or off. |
| `code_scanning.repos.<repo>.min_severity`, `.include_tools`, `.exclude_rules`, `.max_new_issues_per_run` | the top-level value | Per-repository overrides. |

Loading fails when a `repos` key is not in `project.repos`, a severity is not
one of the names above, a pattern is malformed, the cap is negative, or the
interval is under ten minutes. These keys are YAML-only for now; the Settings
dialog does not edit them.

## Severity

GitHub gives an alert up to two severities: every rule has a SARIF level
(`error`, `warning`, `note`), and security rules also have a security severity
(`critical`, `high`, `medium`, `low`). The sweep puts both on one scale:

| The alert has | Level on the sweep's scale |
| --- | --- |
| security severity `critical` | critical |
| security severity `high` | high |
| security severity `medium` | medium |
| security severity `low` | low |
| no security severity, SARIF `error` | medium |
| no security severity, SARIF `warning` | low |
| no security severity, SARIF `note` | note |

So `min_severity: high` files exactly what the Security tab shows as High or
Critical. Quality findings without a security severity (for example CodeQL's
`cpp/commented-out-code` notes) start at `medium` at most. In `min_severity`,
`error` and `warning` are accepted as names for `medium` and `low`.

## What is not filed

OSSF Scorecard reports some checks as code scanning alerts even though they
score repository settings or project policy, not code. No PR an agent can write
fixes "add branch protection" or "get an OpenSSF badge", so filing them would
only park issues with the owner. These rules are left out by default:

`BinaryArtifactsID`, `BranchProtectionID`, `CIIBestPracticesID`, `CITestsID`,
`CodeReviewID`, `ContributorsID`, `DependencyUpdateToolID`, `FuzzingID`,
`LicenseID`, `MaintainedID`, `PackagingID`, `SASTID`, `SecurityPolicyID`,
`SignedReleasesID`, `VulnerabilitiesID`, `WebhooksID`.

`VulnerabilitiesID` is on the list because it repeats what the repository's
dependency scanners already report. `BinaryArtifactsID` asks to delete
checked-in binaries, which is the project's decision. The left-out alerts still
count on the dashboard as `excluded`. To file one anyway, add `!<rule>` to
`exclude_rules`.

Scorecard's file-based checks (`TokenPermissionsID`, `PinnedDependenciesID`,
`DangerousWorkflowID`) are ordinary fixable work and are filed.

## How alerts become issues

Each issue covers alerts a single PR can fix:

- **Code alerts** are grouped by tool and rule. If a rule's alerts are in at
  most five files, they make one issue. Otherwise the files are taken directory
  by directory, in path order, and packed into issues of at most five files.
  A directory is only split when it alone holds more than five files.
- **Dependency alerts** (an alert whose message names a `Package:`, as Trivy's
  do) are grouped by tool and package: one issue per package, listing every
  CVE and every flagged artifact, because one version bump fixes them all.
- **Repository-level alerts** (no file) make one issue per rule. With the
  default exclude list, that only happens for a posture check you re-included.

New issues are filed highest severity first, then by tool, rule and path. Once
a pass has filed `max_new_issues_per_run` issues, it stops; the rest wait for
later passes and show as `untracked` on the dashboard. On the real CodeQL
results of `Danathar/exult`, a C++ game engine fork (84 critical and high alerts over 13 rules),
that is 18 issues, filed five an hour.

A new alert joins an **open** issue that already lists the same tool and rule
in the same file (or the same package). Otherwise it waits for a new group.

## What an issue says

The title is `[governor] Code scanning: <rule> in <where> (<n> <severity> alerts)`,
or `[governor] Code scanning: <package> has <n> known vulnerabilities (<severity>)`.
The issue carries `origin/governor` and `hive/code-scanning`.

The body is written so a lane can do the work without other context:

- **Summary**, with a plain warning that the alerts may be false positives and
  must be checked before changing code.
- **Evidence**: the analysed commit, a table of every alert (link, status,
  `path:line`, severity, message), a numbered excerpt of the code around the
  first file's alerts at that commit, and the rule's documentation link.
- **Wanted behaviour**, **Acceptance criteria**, **Test plan**, and **Out of
  scope** (other alerts, the code scanning setup, dismissing alerts).
- **If an alert is a false positive**: say so on the issue with the reason.
  Do not weaken code, add suppression comments or change the analysis config;
  a maintainer decides whether to dismiss the alert.

Some issues carry more:

- **Workflow files.** When every file is under `.github/workflows/` (typical of
  Scorecard's `TokenPermissionsID` and `PinnedDependenciesID`), the body says the
  fix needs the Workflows permission to push. If an enabled lane serving the
  repository can push workflow files, the issue also gets `hive/workflows-lane`,
  so the scheduler sends it to that lane
  ([hive-open-issue](hive-open-issue.md), [labels reference](labels-and-control-signals/reference.md#hiveworkflows-lane)).
  If none can, the body tells a lane to park the issue with the workflows
  blocker rather than try and fail.
- **Dependency alerts.** The body says the flagged path is often a built binary,
  and to find where it gets the package (the program's `go.mod` or lockfile, or
  the `Containerfile`/`Dockerfile`) and bump it there, to the scanner's fixed
  version. For Go's `stdlib` that means a newer Go toolchain.
- **Forks.** On a fork, the sweep reads the same file at the upstream parent's
  HEAD and looks for the flagged lines. If they are gone, it reads the last
  upstream commit to touch the file and says "port upstream `<sha>`" if that
  change fixes the alert. If they are still there, it says upstream has no fix
  to port. That is at most two reads per new issue.

The first line of the body is a hidden marker:

```
<!-- hive:code-scanning tool=CodeQL rule=cpp%2Fpath-injection paths=tools%2Frip.cc,tools%2Fwuc.cc alerts=40,41,57 -->
```

Values are URL-escaped. `alerts=` is every alert number the issue has ever
tracked.

## Keeping issues in step

Each pass, for each open code scanning issue:

- When some of its alerts were fixed or dismissed, the table row's status
  changes to `fixed` or `dismissed`. Added alerts get new rows. The body is
  edited only when the table actually changed, and an edit does not notify
  anyone.
- When **all** its alerts are fixed or dismissed, the sweep posts one comment
  listing them and closes the issue: as completed if any were fixed, as not
  planned if all were dismissed. A merged PR does not close it; the next
  analysis does.

The sweep **never reopens** an issue, and an alert number that appears on any
of its issues, open or closed, is never filed again. So an issue a person
closes as not planned stays closed even while its alerts are open, and a
dismissed alert that is later reopened on GitHub is not filed a second time.
Those alerts count as `tracked_closed` on the dashboard.

If the open-alert listing hits its bound (1,000 alerts), the pass does not
mark anything fixed or close anything, because an alert missing from a partial
list may still be open.

### Where the state lives

There is no local state file. The sweep lists its own issues by the
`hive/code-scanning` label and reads the marker from each one written by the
hive's App account, so a restart or a lost volume rebuilds everything from
GitHub. A marker on an issue someone else wrote is ignored.

## GitHub App permission

Reading alerts needs the repository permission **Code scanning alerts:
Read-only** (`security_events`). It is only needed on repositories where the
sweep is on. See [GitHub App setup](github-app-setup.md).

Without it, GitHub answers the alert listing with `403` (or a `404` for no
access). The sweep then:

- shows a notice on the dashboard: a pinned **Code Scanning Alerts Not
  Readable** banner naming the repository and the permission, with a link to
  its Security tab, and the same text in `/api/status` `code_scanning.notices`;
- logs `code scanning sweep: cannot read alerts; backing off` once;
- leaves the repository alone for six hours, then asks again. Granting the
  permission (and accepting it on the installation) takes effect at that next
  try or after a restart.

A repository with no code scanning analysis (`404 no analysis found`, or a
private repository without GitHub Advanced Security) gets the same treatment
with a "not set up" notice.

## GitHub API budget

The sweep is an optional sweep in the sweep scheduler: its first pass after a
restart is staggered with the others, and it is skipped entirely whenever the
API budget mode is not `normal`. A pass on one repository costs:

- one request per 100 open alerts (bounded at ten), plus one for the issue
  list. These URLs carry no time filter, so an unchanged list comes back as a
  free `304` from the ETag cache;
- one request for dismissed alerts, only when a tracked alert left the open
  list;
- for each new issue: one file read for the excerpt, and on a fork one or two
  upstream reads;
- one write per new issue, body edit or close (plus one comment per close).

Its calls appear as `hive:code_scanning` in `/api/gh-rate-limits`.

## Write-back gate

Every write (create, edit, comment, close) asks the write-back gate as an
`issue_file` write on the hive's own issue. A paused repository, or
`writeback.per_repo.<repo>.mode: off` or `labels-only`, stops it; the pass
records the refusal as the repository's `error` and writes nothing.

## Dashboard

`/api/status` carries `code_scanning` while the sweep is on for any repository:

- `enabled`;
- `repos[]`: per repository, `open_alerts`, `by_severity` (`critical`, `high`,
  `medium`, `low`, `note`), `qualifying`, `below_threshold`, `excluded`,
  `tracked` (open alerts on an open hive issue), `tracked_closed`, `untracked`
  (waiting for the cap), `open_issues`, this pass's `filed` / `updated` /
  `closed`, `truncated`, `security_url` (the Security tab), `checked_at`, and
  `notice` / `notice_kind` / `error` when something is wrong;
- `notices[]`: the notice texts the banner shows.

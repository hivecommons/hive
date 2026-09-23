# Work sources (`governor.work_source`)

`work_source` selects where a hive reads actionable work items from (Step 01
of the governor loop — see [Architecture](architecture.md)). It accepts four
primary `type` values: `github` (default), `github_projects`, `linear`, and
`jira`. A fifth item kind, `type: run`, is additive rather than a primary
adapter: enable it with `run_stages: true` to append pending long-running run
stages as work items without fabricating GitHub issues. A second additive
kind, `wavefront`, lists the ready nodes of an imported migration graph the
same way (see below).
Absent or `type: ""` behaves exactly like existing hives with no
`work_source` block — GitHub Issues on the configured `project.repos`
(`pkg/config/config.go:1336-1348`, `pkg/worksource/factory.go:15-89`).

This page documents all four. Linear has its own deeper guide —
[Linear agent integration](linear-agent.md) — for the two-way agent-session
integration (webhooks, session acknowledgement, writing back to Linear);
this page covers only the read side (`work_source.linear`) for parity with
the other three.

## Run stages (`run_stages: true`)

Run stages let the governor offer a pending run stage such as `spec`, `plan`,
or `implement` as a non-issue-shaped work item. The primary source remains the
configured adapter; run stages are appended only when the flag is enabled:

```yaml
governor:
  work_source:
    type: github
    run_stages: true
```

With the flag omitted or set to `false`, `ListIssues` output is unchanged. With
it enabled, pending stages use source type `run`, `Number: 0`, and an external
ID of `<runKey>:<stage>`. Their stable key is therefore
`<owner/repo>!<runKey>:<stage>`, not a fabricated `repo#0`. Later stages carry
a dependency on the previous stage's run key, so `plan` records its dependency
on `spec` and `implement` records its dependency on `plan`.

The dashboard's `RunStageAccessor` backs this source with the lease registry:
each stage lease is the run's pending stage, a stage some connection is
currently working is not offered again, and `implement` is listed only once the
run's imported plan is approved. The Spektacular stage runner that advances the
lease is described in [spektacular.md](spektacular.md).

## Wavefront migration graph (`wavefront.enabled: true`)

The second additive item kind is a versioned code-migration graph such as
Crustify's C/C++ to Rust migration driven by Wavefront (#8362, the second
proving workload of #7620). Wavefront stays authoritative for its semantic
graph: it owns dependency closure, cycle handling, waves, and batching. Hive
reads the graph, lists each ready node as a run-stage work item, withholds
blocked nodes, and records a receipt when a node completes. Hive never
re-derives the graph with an LLM, never fabricates GitHub issues or pull
requests for nodes, and the adapter (`pkg/worksource/wavefront`) makes no
GitHub call at all. Publication of results is out of scope (#8353).

```yaml
governor:
  work_source:
    type: github
    wavefront:
      enabled: true
      path: /data/wavefront/graph.json     # or url: https://wavefront.example/graph.json
      repo: acme/crust                     # owner/name every node key is scoped to
      receipts_dir: /data/wavefront/receipts
```

`enabled` defaults to `false`; with it off, `ListIssues` output is
byte-identical to a hive that has never heard of the block. Exactly one of
`path` or `url` is required when enabled, and `repo` is required. The graph
is re-read on every governor cycle, so a plan Wavefront republishes is picked
up without a restart. `receipts_dir` is optional; empty keeps receipts in
memory for the life of the process.

**Graph document.** A JSON object with a `graph` name, a `revision`, and a
`nodes` list. Each node has `id`, `title`, an optional `kind`, an optional
`depends_on` list of node ids, and an optional `status` (`pending` (default),
`ready`, `done`, or `blocked`). Hive validates structure only: an unnamed
graph, a missing revision, a duplicate or malformed id, an edge to an
undefined node, or a cycle is refused as a whole; the graph is never repaired.
A small fixture lives at
`pkg/worksource/wavefront/testdata/wavefront-fixture/graph.json`.

**Listing.** A node is ready when it is not `done` or `blocked`, holds no
completion receipt at the current revision, and every node it depends on is
either `done` in the graph or holds a completion receipt at the current
revision. Ready nodes are listed in wave order as run-stage items: source type
`run`, `Number: 0`, stage `implement`, external id `<graph>:<node>`, stable
key `<owner/repo>!<graph>:<node>`, labels `hive-run`, `stage/implement`,
`wavefront`, `wavefront/<kind>`, and `graph-rev/<revision>`. Each
`depends_on` edge is carried as a `DependsOn` entry keyed
`<owner/repo>!<graph>:<dep>`, so admission sees the same edges Wavefront
declared. Blocked nodes and everything downstream of them are withheld, not
listed as unresolved.

**Stale-plan guard.** The `graph-rev/<revision>` label is the revision the
item was minted at. `Source.Verify` and `Source.Complete` take that revision
back and refuse it with `ErrStaleRevision` when the graph has since moved.
A refused item is never mapped onto the new graph; it is simply re-listed
from the current revision if it is still ready. A receipt recorded under an
older revision no longer satisfies anything, so a dependent whose upstream
changed is withheld again rather than silently continuing (the #8346
provenance guard, expressed through the revision).

**Receipts.** `Source.Complete(ctx, externalID, revision, artifacts, startedAt)`
writes one `stage-receipt/v1` record (#8295) per completed node under
`<receipts_dir>/<graph>/<node>.json`: work key, assignment id
`<graph>:<node>`, stage `implement`, contract `wavefront-node/v1`,
`input_revision` `wavefront-graph@<graph digest>`, and the artifact-borne
progress anchors the stage produced (default `wavefront/<graph>/<node>`).
Completing an already-completed node at the same revision returns the existing
receipt unchanged, so a retry is idempotent. Completing a node whose
dependencies are not all satisfied is refused with `ErrNotReady`. Receipts are
the only state Hive keeps for the migration; there is no side database.

**Plan import.** `wavefront.ImportPlan(store, epic, graph, expectedRevision,
opts)` admits the graph as an epic's child beads through
`planning.DecomposeFromOutput`: the graph is rendered as the already-structured
task list that function consumes (`N. [<node>] <title> (depends: ...)
[agent_suitable]`, in wave order) and materialized with no prompt and no
planner call. Node ids become each child's `plan_ref`; edges become bead
dependencies. A graph whose revision is not `expectedRevision` is refused
before anything is written.

**Not wired yet.** The adapter lists work and records receipts. Handing a
listed node to an agent worktree, the mid-node crash reconciling to Unknown,
and the burndown counts on `/api/runs/{key}` are follow-ups on the #8290
umbrella; the adapter's `Receipts()` and `Ready()` are the seams they read.

## `type: github` (default)

No config needed. Enumerates open issues on `project.repos` via the existing
GitHub client (`pkg/worksource/factory.go:17-18`). This is the only source
type most hives ever need.

## `type: github_projects` — GitHub Projects (Projects v2)

Reads items from a GitHub Projects v2 board via the GraphQL API
(`pkg/worksource/github_projects.go`). Use this when work is tracked on a
project board rather than as bare repo issues.

```yaml
governor:
  work_source:
    type: github_projects
    github_projects:
      project_number: 7            # required — the number in the project URL
      org: your-org                # optional — falls back to project.org
      states: [Todo, In Progress]  # optional — Status field values to include; empty = all
      priority_field: Priority     # optional — name of a single-select priority field
      iteration_field: Sprint      # optional — name of an iteration field; only current-iteration items are returned when set
      default_repo: your-org/repo  # optional — used when an item's own repo can't be determined
```

Config fields (`GitHubProjectsSourceConfig`, `pkg/config/config.go:1696-1703`):

| YAML key | Go field | Required | Notes |
|---|---|---|---|
| `project_number` | `ProjectNumber` | Yes | The project's number, visible in its URL (`.../projects/7`). |
| `org` | `Org` | No | Defaults to `project.org` if empty (`factory.go:23`). |
| `states` | `States` | No | Filters on the project's `Status` single-select field. Empty means no filtering. |
| `priority_field` | `PriorityField` | No | Name of a single-select field to map to normalized priority (`urgent`/`high`/`medium`/`low`/`none`; see `normalizePriority`, `github_projects.go:318-331`). Empty means every issue gets priority `none`. |
| `iteration_field` | `IterationField` | No | Name of an iteration field. When set, only items whose iteration window covers "now" are returned (`inCurrentIteration`, `github_projects.go:299-315`). |
| `default_repo` | `DefaultRepo` | No | `owner/repo` used when the project item's own repository can't be read from the GraphQL response. |

**Credentials.** The adapter authenticates with the same GitHub token the
hive already uses for issues/PRs (`ghToken` passed into
`worksource.FromConfig`, `pkg/worksource/factory.go:15,23`) — there is no
separate `token` field in `github_projects:`. That token needs the
`read:project` scope in addition to whatever repo scopes it already carries;
a classic PAT with only `repo` cannot read a Projects v2 board and the
GraphQL call fails with a GraphQL-level authorization error surfaced as
`worksource/github_projects: graphql error: ...`
(`github_projects.go:219-221`).

**Failure mode.** A wrong `project_number` or an org the token can't see
returns a GraphQL error on every governor cycle, logged but not fatal — the
hive keeps running with an empty work-item list from this source.

## `type: linear`

```yaml
governor:
  work_source:
    type: linear
    linear:
      api_key: ${LINEAR_API_KEY}
      hold_labels: [hold]              # additive; GitHub's hold defaults still apply
      teams:
        - key: ENG
          repo: your-org/default-repo
          states: [Todo, In Progress]
          cycles: current              # optional; only issues in the active cycle
          projects:                    # optional; when present, only these projects enumerate
            - name: Platform
              repo: your-org/platform
      assigned_only: true              # optional; requires a connected Linear agent app
      session_agent: scanner           # which hive agent takes Linear agent sessions
```

`api_key` and at least one `teams[].key`/`teams[].repo` are required —
construction fails closed with an explicit error naming the missing field
otherwise (`factory.go:32-53`). `assigned_only: true` additionally requires
the Linear agent app to already be connected (`linearagent.StoredViewerID`);
without a connected app it is a startup error, not "enumerate everything"
(`factory.go:58-67`).

See [Linear agent integration](linear-agent.md) for the OAuth app setup,
webhook wiring, `LINEAR_CLIENT_ID`/`LINEAR_CLIENT_SECRET`/
`LINEAR_WEBHOOK_SECRET`, and how agents write back to Linear.

**ACMM gap issues.** The dashboard's ACMM "Open Issue" / "Open All" buttons
file on GitHub by default even on a Linear-sourced hive. Set
`governor.acmm.issue_tracker: work_source` to file them on the Linear team
mapped to the criterion's repo instead (`teams[].repo` match, else the first
team) — see [ACMM policy matrix → Where gap issues are filed](acmm-policy-matrix.md#where-acmm-gap-issues-are-filed).

## `type: jira` — Jira Cloud

Reads issues from Jira Cloud via the REST API v3
(`pkg/worksource/jira.go`). Read-only ("Phase 1" per the code comment —
there is no write-back to Jira the way there is for Linear).

```yaml
governor:
  work_source:
    type: jira
    jira:
      base_url: https://your-org.atlassian.net   # required
      email: bot@your-org.com                    # required — Atlassian account email
      api_token: ${JIRA_API_TOKEN}                # required
      project_keys: [ENG, OPS]                    # used to build the default JQL
      # jql: "project in (ENG) AND statusCategory != Done"  # optional full override
      repo: your-org/default-repo                 # required for agents to know what to clone
      hold_labels: [hold, blocked]                # optional — Jira labels that gate an issue
```

Config fields (`JiraSourceConfig`, `pkg/config/config.go:1738-1746`):

| YAML key | Go field | Required | Notes |
|---|---|---|---|
| `base_url` | `BaseURL` | Yes | Jira Cloud instance root, e.g. `https://myorg.atlassian.net` (`jira.go:17`). |
| `email` | `Email` | Yes | Atlassian account email; used for HTTP Basic auth alongside the API token (`jira.go:189`). |
| `api_token` | `APIToken` | Yes | Jira API token (Atlassian account → **Security → API tokens**). |
| `project_keys` | `ProjectKeys` | No¹ | Project keys to enumerate, e.g. `["ENG","OPS"]`. Used to build the default JQL when `jql` is empty. |
| `jql` | `JQL` | No | Full JQL override. When empty, the adapter builds `project in (<keys>) AND statusCategory != Done AND issuetype != Epic` (`jira.go:59-67`). |
| `repo` | `Repo` | Yes in practice | GitHub `owner/name` repo agents clone to work these issues; every returned `Issue.Repo` is set to this single value (`jira.go:214`) — Jira source config maps to exactly one repo, unlike Linear's per-team repo map. |
| `hold_labels` | `HoldLabels` | No | Jira label values that gate an issue out of the work list, the Jira analogue of GitHub's `hold` label (`jira.go:33,143-153`). |

¹ `project_keys` is not enforced as required by the constructor, but if both
it and `jql` are empty the built JQL becomes `project in () AND ...`, which
Jira will reject — set one or the other.

**Credentials.** Jira Cloud REST v3 uses HTTP Basic auth with the account
email as username and the **API token** (not the account password) as
password (`req.SetBasicAuth(s.cfg.Email, s.cfg.APIToken)`, `jira.go:189`).
Generate the token from the Atlassian account's **Security → API tokens**
page. Supply it via `${JIRA_API_TOKEN}` environment-variable substitution in
`hive.yaml` — never inline the literal token in the committed config.

**Priority mapping.** Jira priority names are normalized case-insensitively
(`normalizeJiraPriority`, `jira.go:126-141`):

| Jira priority | Normalized |
|---|---|
| `highest`, `critical`, `blocker`, `p0` | `urgent` |
| `high`, `p1` | `high` |
| `medium`, `p2` | `medium` |
| `low`, `lowest`, `p3`, `p4` | `low` |
| anything else | `none` |

**Hold-label gating.** `ListIssues` drops any issue whose Jira `labels`
field contains one of the configured `hold_labels` before it is returned to
the governor — the same shape as GitHub's `hold` label, but Jira has no
built-in "hold" concept so this is opt-in and additive
(`jira.go:143-175`).

**Pagination.** The adapter pages the Jira search API 100 issues
(`jiraMaxResults`, `jira.go:38`) at a time until it has walked the full
result set.

**Failure mode.** A bad `base_url`, invalid credentials, or non-2xx Jira
response returns `worksource/jira: search returned <status>: <body>` on
every governor cycle, logged but not fatal — the hive keeps running with an
empty work-item list from this source until the misconfiguration is fixed.

## Open questions

- The issue that requested this page (#4926) says GitHub Projects requires
  a "project node ID". The code instead takes a plain integer
  `project_number` (the number visible in the project's URL) and resolves it
  server-side via GraphQL (`organization(login).projectV2(number)`,
  `github_projects.go:56-57`) — there is no node-ID field in
  `GitHubProjectsSourceConfig`. This page documents the code as written.

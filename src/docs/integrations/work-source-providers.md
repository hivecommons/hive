# Work source providers

## Audience

This page is for teams that want Hive to read actionable work items from a planning system. There are two routes: contribute an in-tree Go adapter (a PR to `hivecommons/hive`), or run an HTTPS shim and connect it with the `external` work source described in [External provider](#external-provider), which needs no Hive PR.

## Concepts

A **work source** is the Step 01 input to Hive's governor loop. The normalized record is `worksource.Issue`: it carries source type, target repo, source-native external ID, title, author, labels, assignees, priority, state, timestamps, canonical URL, tracker flag, stage, and dependency edges (`src/pkg/worksource/worksource.go:18`). The `WorkSource` interface itself has only `SourceType()` and `ListIssues(context.Context)` (`src/pkg/worksource/worksource.go:73`).

`governor.work_source` chooses one primary source: `github`/empty, `github_projects`, `linear`, `jira`, `gitea`, `gitlab`, or `external`. Pending run stages and Wavefront graph nodes are additive sources, not replacement primaries. `FromConfig` resolves the configured type through the primary registry (`RegisterPrimary`) and rejects unknown values, so a bad type still makes the governor fail closed.

## Interface

Implement this contract:

- `SourceType() string`: return the stable source label used in logs, dashboard badges, serialized items, and work keys (`src/pkg/worksource/worksource.go:74`).
- `ListIssues(ctx) ([]Issue, error)`: return only currently actionable items. State filters, hold labels, assignment filters, project/cycle filters, and pagination belong in the adapter; Hive should not need source-specific post-processing (`src/pkg/worksource/worksource.go:77`).
- Populate `Issue.Repo` with the `owner/name` repository agents clone and open change requests against, and `ExternalID` with the native item identifier (`src/pkg/worksource/worksource.go:22`, `src/pkg/worksource/worksource.go:25`).
- Use `DependsOn` for source-native blockers. Linear maps incoming `blocks` relations into `Dependency` values and marks completed/canceled blockers resolved (`src/pkg/worksource/linear.go:357`).

Writes are not part of the `WorkSource` interface. Existing write paths are source-specific helpers: Linear exposes `CreateIssue` for ACMM gap filing (`src/pkg/worksource/linear.go:610`); Jira has internal `addComment` and `transitionIssue` helpers (`src/pkg/worksource/jira.go:403`, `src/pkg/worksource/jira.go:414`). The review backlog sinks (`src/pkg/worksource/review_backlog_sink.go`) add `CreateIssueInState`/`CommentOnIssue` for Linear (`src/pkg/worksource/linear.go:686`, `src/pkg/worksource/linear.go:751`), `createIssue`/`transitionToStatus` for Jira (`src/pkg/worksource/jira.go:434`, `src/pkg/worksource/jira.go:471`), and `AddIssueToColumn` for GitHub Projects. Claiming, comments, state transitions, and PR/MR equivalents are therefore not pluggable provider methods today.

Hive's change-request execution still assumes a Git forge target repository. The work source tells Hive what work exists; agents still clone `Issue.Repo` and use Hive's existing pull-request relays for code changes.

## Step-by-step: contribute a primary adapter

1. Add config fields under `WorkSourceConfig` in `src/pkg/config/work_sources.go` and include them in `IsZero`/validation if needed (`src/pkg/config/work_sources.go:12`).
2. Implement a `WorkSource` in `src/pkg/worksource`, following `LinearSource`, `jiraSource`, or `githubProjectsSource` as shapes (`src/pkg/worksource/linear.go:80`, `src/pkg/worksource/jira.go:84`, `src/pkg/worksource/github_projects.go:49`).
3. Register the adapter with `RegisterPrimary("<type>", build)` in `src/pkg/worksource/factory.go` and return a useful config error from the builder for every required field. Duplicate registrations panic, as with `RegisterAdditive`.
4. Add dashboard settings round-trip support if operators should configure it from Settings -> Work source; the existing route is `handleGovernorWorkSourceGet`/`Put` (`src/pkg/dashboard/api_governor_features.go:723`, `src/pkg/dashboard/api_governor_features.go:737`).
5. Add docs to [Work sources](../work-sources.md) and this guide, using source-neutral terms from [Work-source terminology](../work-source-terminology.md).
6. Add tests mirroring the adapter's fixture style: `linear_test.go`, `jira_test.go`, `github_projects_test.go`, plus factory round-trip tests (`src/pkg/worksource/linear_test.go:124`, `src/pkg/worksource/jira_test.go:124`, `src/pkg/worksource/github_projects_test.go:109`, `src/pkg/worksource/factory_test.go:51`).

Additive sources use a separate compile-time registry. `RegisterAdditive` is called by a linked subpackage and panics on duplicate names. Use this only when your source appends extra work items alongside the primary source, as run stages and Wavefront do.

If you do not want to maintain an in-tree adapter at all, use [External provider](#external-provider) instead: it is the same seam, reached over HTTP/JSON.

## Configuration examples

```yaml
governor:
  work_source:
    type: linear
    linear:
      api_key: ${LINEAR_API_KEY}
      hold_labels: [hold]
      teams:
        - key: ENG
          repo: your-org/app
          states: [Todo, In Progress]
          cycles: current
          projects:
            - name: Platform
              repo: your-org/platform
      assigned_only: true
```

```yaml
governor:
  work_source:
    type: jira
    jira:
      deployment: cloud
      base_url: https://your-org.atlassian.net
      email: bot@your-org.com
      api_token: ${JIRA_API_TOKEN}
      project_keys: [ENG]
      repo: your-org/app
      hold_labels: [hold, blocked]
```

```yaml
governor:
  work_source:
    type: github_projects
    github_projects:
      org: your-org
      project_number: 7
      states: [Todo, In Progress]
      default_repo: your-org/app
```

## Worked example: Linear

The Linear adapter is the best reference for a non-GitHub work source.

- `LinearConfig` maps teams to target repos, optional project routing, hold labels, cycle filtering, and an optional viewer ID used by `assigned_only` (`src/pkg/worksource/linear.go:51`).
- `linearIssuesQuery` enumerates by team and state with pagination; `linearAssignedIssuesQuery` adds assignee/delegate filters when `assigned_only` is enabled (`src/pkg/worksource/linear.go:94`, `src/pkg/worksource/linear.go:138`).
- `ListIssues` loops teams, applies default states, filters current cycle/project/hold labels, maps Linear priority to Hive priority, records tracker status, and carries dependency edges (`src/pkg/worksource/linear.go:282`).
- `linearGraphQL` sends one GraphQL POST with Linear's API key in `Authorization`, enforces HTTP 200, and checks top-level GraphQL errors (`src/pkg/worksource/linear.go:492`).
- `CreateIssue` resolves a team key and calls `issueCreate`; this is a Linear-specific write helper, not part of the generic interface (`src/pkg/worksource/linear.go:610`).
- `FromConfig` resolves `${LINEAR_API_KEY}`, requires at least one team with `key` and `repo`, validates `cycles`, and fails closed when `assigned_only` lacks a connected Linear agent (`buildLinearSource` in `src/pkg/worksource/factory.go`).

## Dashboard behavior

Work-source configuration appears in Settings -> Work Source. The UI labels GitHub Issues as the default and lists GitHub Projects v2, Linear, Jira, Gitea/Forgejo, GitLab, and External provider (HTTP) as alternate sources (`src/pkg/dashboard/static/index.html:34233`). The Projects navigation label is intentionally neutral (`src/pkg/dashboard/static/index.html:3912`). Overview bands render source-neutral open/held item counts, while Throughput describes merged change requests "across tracked forges" (`src/pkg/dashboard/static/index.html:4204`).

## Testing

Use adapter-local HTTP/GraphQL fakes and table fixtures. Existing patterns cover pagination, filtering, dependency mapping, secret references, config parsing, and additive-source composition (`src/pkg/worksource/linear_test.go:234`, `src/pkg/worksource/jira_test.go:341`, `src/pkg/worksource/factory_secret_ref_test.go:1`, `src/pkg/worksource/factory_test.go:153`). Also update dashboard work-source API tests when adding UI-visible config (`src/pkg/dashboard/api_governor_worksource_test.go:24`).

## Operational notes

- Auth and secrets: resolve whole-value environment references at use time so dashboard overlays can store `${NAME}` without persisting literal credentials (`resolveSecretRef` in `src/pkg/worksource/factory.go`). The `external` adapter goes further and rejects literal credentials outright.
- Rate limits: page at the provider maximum where documented. Linear and GitHub Projects use 100-item GraphQL pages (`src/pkg/worksource/linear.go:90`, `src/pkg/worksource/github_projects.go:55`); Jira uses `jiraSearchPageSize = 100` (`src/pkg/worksource/jira.go:19`).
- Webhooks vs polling: primary work sources are polled by the governor through `ListIssues`; Linear's webhook-backed agent sessions are a separate integration documented in [Linear agent integration](../linear-agent.md).
- Identity mapping: always preserve source-native IDs in `ExternalID`, and route to a target repo through config rather than guessing.

## External provider

`work_source.type: external` connects a tracker without a Hive PR. The provider runs a small HTTPS service that speaks the versioned `hive.worksource/v1` contract; Hive calls it read-only and stays the only authority over work identity, credentials, and admission. The design and its rationale are [ADR-0020](../adr/0020-external-work-source-boundary.md).

### Configuration

```yaml
governor:
  work_source:
    type: external
    external:
      name: acme                      # stable SourceType; ^[a-z][a-z0-9_]{1,31}$
      display_name: Acme Tracker      # dashboard label; defaults to name
      base_url: https://acme-shim.internal:8443
      auth_token: $ACME_WORKSOURCE_TOKEN     # must be an env reference
      # ca_bundle: $ACME_WORKSOURCE_CA       # optional env reference (PEM bundle)
      repos: [your-org/app, your-org/platform]  # required allow-list
      hold_labels: [hold, blocked]    # applied by Hive as well as the provider
      timeout_seconds: 30             # whole ListIssues budget; default 30
```

`ExternalSourceConfig.Validate()` fails closed on every one of these rules:

- `name` matches `^[a-z][a-z0-9_]{1,31}$` and is not one of the reserved types (`github`, `github_projects`, `linear`, `jira`, `gitea`, `gitlab`, `run`, `wavefront`, `external`).
- `base_url` uses `https://`. Plain `http://` is accepted only for loopback hosts, so a sidecar shim works.
- `auth_token` is required, and both `auth_token` and `ca_bundle` must be whole-value environment references. A literal value is rejected so a resolved credential never lands in the saved config or the dashboard overlay on disk.
- `repos` is non-empty and every entry is `owner/name`.
- `timeout_seconds` is 0 (the 30s default) or 1..300.

**Write `$NAME`, not `${NAME}`, in `hive.yaml`.** Config load expands `${NAME}` in the raw document before it is parsed, which would turn the reference into the credential itself. The unbraced spelling survives that pass and is resolved at use time instead; the dashboard overlay (`PUT /api/config/governor/work-source`) stores either spelling verbatim and `GET` returns the reference, never a value.

### Wire contract `hive.worksource/v1`

Every request carries `Authorization: Bearer <auth_token>`, `Accept: application/json`, and `Hive-Worksource-Contract: hive.worksource/v1`. Hive refuses to follow a redirect to a different host, so the bearer token is never forwarded off `base_url`.

**`GET {base_url}/v1/source`** — called once per process (and by "test connection"), never per cycle:

```json
{"contract": "hive.worksource/v1", "source_type": "acme", "display_name": "Acme Tracker"}
```

`source_type` must equal the configured `name`; a mismatch is an error.

**`GET {base_url}/v1/issues?cursor=<opaque>`** — called for each enumeration, following `next_cursor` until it is empty:

```json
{
  "contract": "hive.worksource/v1",
  "items": [{
    "repo": "your-org/app",
    "external_id": "ACME-123",
    "title": "Retry webhook delivery",
    "body": "…",
    "author": "jdoe",
    "labels": ["bug"],
    "assignees": [],
    "is_tracker": false,
    "priority": "high",
    "state": "Todo",
    "created_at": "2026-09-30T12:00:00Z",
    "updated_at": "2026-10-01T08:00:00Z",
    "url": "https://acme.example/issues/ACME-123",
    "depends_on": [{"repo": "your-org/app", "external_id": "ACME-100", "resolved": false}]
  }],
  "next_cursor": ""
}
```

Item fields use the same names as the JSON tags on `worksource.Issue`. `depends_on` has its own wire shape because `Dependency.Ref` is tagged `json:"-"`. `source_type`, `number`, and `stage` are **not** accepted from the wire: Hive sets them, and an item that carries any of them is dropped.

Limits per call, raised only by a contract revision: 50 pages, 5,000 items, and 8 MiB per response body, all inside `timeout_seconds`.

### What Hive validates before it trusts an item

1. `repo` must appear in `repos`.
2. `external_id` must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`. No `!`, `#`, `:`, `/`, or whitespace, so the resulting key is always the `repo!externalID` form and can never be read as a GitHub `repo#N` key or a run-stage key.
3. `number` is always 0, so GitHub-only observers (PR-claim ledger, bead dependency gate) skip these items.
4. Two items with the same key in one listing fail the **whole** call; keeping either would be a guess about which one the stored holds and claims belong to.
5. A `depends_on` entry that fails rule 1 or 2 is dropped while the item is kept. Hive never invents a resolved dependency.
6. `url` must be `https` (or loopback `http`) or empty. `author` and `assignees` stay provider-native handles and are never mapped to GitHub logins.
7. `priority` outside `urgent|high|medium|low|none` becomes empty.

Items that fail a rule are withheld, logged with the source name and the offending field, and counted per source.

**Your `external_id` values must be stable for the life of an item.** The key is persisted: Hive stores holds, cooldowns, queue order, failure quarantine, and claims under `repo!external_id`. If the provider renumbers items, every one of those records is lost and the work is re-dispatched.

### Errors, admission, and write-back

Any failure — transport error, non-2xx, contract mismatch, bad JSON, a limit exceeded, a duplicate key — fails the enumeration. The governor then fails closed for that cycle: no issues are listed, GitHub pull-request maintenance continues, and the error is logged with the source name. Hold labels are applied in the adapter, and `FilterExemptIssues` plus the issue filter still run afterwards.

`hive.worksource/v1` is read-only: the adapter implements none of `LabelMutator`, `Commenter`, or `StatusTransitioner`, and design mode shows its "unsupported" message for these items. Write-back is deferred to a later contract revision.

## Gaps

- The external boundary is HTTP/JSON only; gRPC, an MCP-style child process, and Go `plugin` loading were considered and rejected or deferred in [ADR-0020](../adr/0020-external-work-source-boundary.md). In-tree Go adapters remain the route for anything that needs write-back.
- The generic interface is read-only. Provider-specific claim/comment/transition helpers exist, but a source-neutral write interface — including write-back for external providers — would require a design PR. Track it in [#10174](https://github.com/hivecommons/hive/issues/10174).

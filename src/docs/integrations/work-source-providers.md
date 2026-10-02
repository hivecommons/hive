# Work source providers

## Audience

This page is for teams that want Hive to read actionable work items from a planning system. It is written from v5 code: the public seam is a Go interface inside this module, so a new primary provider currently requires a PR to `hivecommons/hive` rather than installing an external plugin.

## Concepts

A **work source** is the Step 01 input to Hive's governor loop. The normalized record is `worksource.Issue`: it carries source type, target repo, source-native external ID, title, author, labels, assignees, priority, state, timestamps, canonical URL, tracker flag, stage, and dependency edges (`src/pkg/worksource/worksource.go:18`). The `WorkSource` interface itself has only `SourceType()` and `ListIssues(context.Context)` (`src/pkg/worksource/worksource.go:73`).

`governor.work_source` chooses one primary source: `github`/empty, `github_projects`, `linear`, or `jira` (`src/pkg/config/config.go:2196`). Pending run stages and Wavefront graph nodes are additive sources, not replacement primaries (`src/pkg/config/config.go:2199`, `src/pkg/config/config.go:2209`). The factory dispatches those primary names in `FromConfig` and rejects unknown values (`src/pkg/worksource/factory.go:18`, `src/pkg/worksource/factory.go:122`).

## Interface

Implement this contract:

- `SourceType() string`: return the stable source label used in logs, dashboard badges, serialized items, and work keys (`src/pkg/worksource/worksource.go:74`).
- `ListIssues(ctx) ([]Issue, error)`: return only currently actionable items. State filters, hold labels, assignment filters, project/cycle filters, and pagination belong in the adapter; Hive should not need source-specific post-processing (`src/pkg/worksource/worksource.go:77`).
- Populate `Issue.Repo` with the `owner/name` repository agents clone and open change requests against, and `ExternalID` with the native item identifier (`src/pkg/worksource/worksource.go:22`, `src/pkg/worksource/worksource.go:25`).
- Use `DependsOn` for source-native blockers. Linear maps incoming `blocks` relations into `Dependency` values and marks completed/canceled blockers resolved (`src/pkg/worksource/linear.go:357`).

Writes are not part of the `WorkSource` interface. Existing write paths are source-specific helpers: Linear exposes `CreateIssue` for ACMM gap filing (`src/pkg/worksource/linear.go:610`); Jira has internal `addComment` and `transitionIssue` helpers used by Jira tests and future wiring (`src/pkg/worksource/jira.go:403`, `src/pkg/worksource/jira.go:414`). Claiming, comments, state transitions, and PR/MR equivalents are therefore not pluggable provider methods today.

Hive's change-request execution still assumes a Git forge target repository. The work source tells Hive what work exists; agents still clone `Issue.Repo` and use Hive's existing pull-request relays for code changes.

## Step-by-step: contribute a primary adapter

1. Add config fields under `WorkSourceConfig` in `src/pkg/config/config.go` and include them in `IsZero`/validation if needed (`src/pkg/config/config.go:2196`).
2. Implement a `WorkSource` in `src/pkg/worksource`, following `LinearSource`, `jiraSource`, or `githubProjectsSource` as shapes (`src/pkg/worksource/linear.go:80`, `src/pkg/worksource/jira.go:84`, `src/pkg/worksource/github_projects.go:49`).
3. Add a `case` in `FromConfig` and return a useful config error for every required field (`src/pkg/worksource/factory.go:18`).
4. Add dashboard settings round-trip support if operators should configure it from Settings -> Work source; the existing route is `handleGovernorWorkSourceGet`/`Put` (`src/pkg/dashboard/api_governor_features.go:723`, `src/pkg/dashboard/api_governor_features.go:737`).
5. Add docs to [Work sources](../work-sources.md) and this guide, using source-neutral terms from [Work-source terminology](../work-source-terminology.md).
6. Add tests mirroring the adapter's fixture style: `linear_test.go`, `jira_test.go`, `github_projects_test.go`, plus factory round-trip tests (`src/pkg/worksource/linear_test.go:124`, `src/pkg/worksource/jira_test.go:124`, `src/pkg/worksource/github_projects_test.go:109`, `src/pkg/worksource/factory_test.go:51`).

Additive sources use a separate compile-time registry. `RegisterAdditive` is called by a linked subpackage and panics on duplicate names (`src/pkg/worksource/factory.go:158`). Use this only when your source appends extra work items alongside the primary source, as run stages and Wavefront do.

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
- `FromConfig` resolves `${LINEAR_API_KEY}`, requires at least one team with `key` and `repo`, validates `cycles`, and fails closed when `assigned_only` lacks a connected Linear agent (`src/pkg/worksource/factory.go:34`).

## Dashboard behavior

Work-source configuration appears in Settings -> Work Source. The UI labels GitHub Issues as the default and lists GitHub Projects v2, Linear, and Jira as alternate sources (`src/pkg/dashboard/static/index.html:31330`). The Projects navigation label is intentionally neutral (`src/pkg/dashboard/static/index.html:3912`). Overview bands render source-neutral open/held item counts, while Change Throughput describes merged change requests "across tracked forges" (`src/pkg/dashboard/static/index.html:4204`).

## Testing

Use adapter-local HTTP/GraphQL fakes and table fixtures. Existing patterns cover pagination, filtering, dependency mapping, secret references, config parsing, and additive-source composition (`src/pkg/worksource/linear_test.go:234`, `src/pkg/worksource/jira_test.go:341`, `src/pkg/worksource/factory_secret_ref_test.go:1`, `src/pkg/worksource/factory_test.go:153`). Also update dashboard work-source API tests when adding UI-visible config (`src/pkg/dashboard/api_governor_worksource_test.go:24`).

## Operational notes

- Auth and secrets: resolve whole-value environment references at use time so dashboard overlays can store `${NAME}` without persisting literal credentials (`src/pkg/worksource/factory.go:211`).
- Rate limits: page at the provider maximum where documented. Linear and GitHub Projects use 100-item GraphQL pages (`src/pkg/worksource/linear.go:90`, `src/pkg/worksource/github_projects.go:55`); Jira uses `jiraSearchPageSize = 100` (`src/pkg/worksource/jira.go:19`).
- Webhooks vs polling: primary work sources are polled by the governor through `ListIssues`; Linear's webhook-backed agent sessions are a separate integration documented in [Linear agent integration](../linear-agent.md).
- Identity mapping: always preserve source-native IDs in `ExternalID`, and route to a target repo through config rather than guessing.

## Gaps

- Primary providers are compile-time only. There is no external plugin, HTTP, gRPC, or MCP boundary for `WorkSource` in v5. Track the gap in [#10174](https://github.com/hivecommons/hive/issues/10174).
- The generic interface is read-only. Provider-specific claim/comment/transition helpers exist, but adding a source-neutral write interface would require a design PR.

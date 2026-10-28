# Portfolio search

Portfolio search ([#14491](https://github.com/hivecommons/hive/issues/14491), Part A) indexes what a spoke already
stores — review evidence bundles and their findings and sentinel hits, complexity snapshots, beads and knowledge
facts — so an operator can search across every repo the hive looks after. It is **off by default** and
**spoke-local**: there is no hub fan-out and no federation (RFC Part B).

## Turning it on

```yaml
search:
  enabled: true
```

With `search.enabled` unset or `false` no indexer goroutine starts and no `/data/search` directory is created;
`GET /api/search` answers `404` and `GET /api/repos/{owner}/{repo}/health` answers `503`. Once enabled, the
indexer rebuilds incrementally every 30 minutes, off the request path.

## Configuration keys

| Key | Default | Meaning |
| --- | --- | --- |
| `search.enabled` | `false` | Run the background indexer and serve the search and health endpoints. |
| `search.max_index_bytes_per_repo` | `33554432` (32 MiB) | Cap on one repo's serialised index. Over budget, the previous snapshot stays published and the repo is reported as a coverage gap. |
| `search.max_memory_bytes` | `268435456` (256 MiB) | Cap on loaded index snapshots; least-recently-queried repos are dropped from memory and reloaded from disk on demand. |
| `search.retention` | `24` | Months of documents kept, by document time. |
| `search.health.stale_after` | `60d` | How long an open issue may sit idle before it counts as stale (`60d` or a Go duration). |
| `search.health.security_doc_max_age` | `365d` | Age of the last `SECURITY.md` / threat-model change before the security-doc component decays. |
| `search.health.weights.<component>` | see below | Per-component weight; unset keeps the default, `0` reports the component without scoring it. |

Default health weights (sum 100): `ci_pass_rate` 20, `time_to_merge` 15, `revert_followup_rate` 20,
`review_verdict_mix` 10, `stale_issue_ratio` 10, `signoff_rate` 10, `dependency_freshness` 10, `security_doc_rot` 5.

## Using it

- `hivectl search <query>` with `--kind`, `--repo`, `--from`, `--to` and `--limit`, and
  `hivectl repo health <owner/repo>`; see [hivectl](hivectl.md#search--repo-health--portfolio-search).
- `GET /api/search?q=&kind=&repo=&from=&to=&limit=` (owner or merger) returns highlighted hits and facets by
  `kind`, `repo`, `verdict` and `sentinel.rule`. Queries are audited with actor and role; results never are. Text
  that ioscan flags as injected comes back as a visible redaction marker.
- `GET /api/repos/{owner}/{repo}/health` returns the report-only health score with every component's inputs, `n`
  and weight. A component without at least 10 samples is `not_reported`, never zero. Nothing that decides a merge,
  ACMM level or Governor priority reads the score.

A repo listed under `coverage` in a search response has an unreadable or over-budget index: missing hits there are
not proof that nothing matches. Index internals are described in
[`pkg/search/README.md`](../pkg/search/README.md).

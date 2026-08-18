# Knowledge curator configuration

The knowledge curator extracts candidate facts from merged PR activity and can prepare high-confidence facts for promotion between llm-wiki layers. The live code is in `pkg/knowledge/curator.go` and `pkg/knowledge/promote.go`.

Enable the knowledge system before tuning the curator:

```yaml
knowledge:
  enabled: true
  engine: llm-wiki
  curator:
    schedule: daily
    extract_from:
      - pr_comments
      - review_comments
    auto_promote_threshold: 0.9
```

## Fields

| Field | Current behavior in v2 HEAD |
| --- | --- |
| `schedule` | Defaults to `daily` when knowledge is enabled. The value is stored as `CuratorConfig.Schedule`. No scheduler in v2 HEAD parses or actions it. Extraction runs only when Go code invokes `Curator.RunExtraction` explicitly; see [Scheduling extraction](#scheduling-extraction). |
| `extract_from` | Sources the curator should inspect. Implemented sources are `pr_comments` and `review_comments`. `ci_failures` appears in the example config/design but is not currently extracted by `Curator.extractFromPR`. |
| `auto_promote_threshold` | Defaults to `0.9` when knowledge is enabled. `Promoter.AutoPromoteCandidates` selects facts whose llm-wiki page has `status == "verified"` and `confidence >= threshold`. |

## Extraction behavior

`RunExtraction(ctx, since)` lists up to 50 recently updated closed PRs per configured repo, keeps merged PRs newer than `since`, and examines configured comment sources. Comments shorter than 20 characters are ignored.

The classifier is heuristic. It emits candidates for comments containing signals such as:

- `always`, `never`, `must`, `do not` → gotchas,
- `regression`, `broke`, `reverted` → regressions,
- `pattern`, `convention`, `prefer`, `best practice` → patterns,
- `test`, `coverage`, `mock`, `fixture`, `assert` → test scaffolding,
- `decided`, `agreed`, `going forward`, `from now on` → decisions.

Extracted facts are sent to the wiki `/api/ingest` endpoint. Promotion only flows upward (for example project → org), preserving provenance in the promoted fact.

## Scheduling extraction

Extraction runs only when Go code invokes `Curator.RunExtraction` explicitly. No scheduler, CLI command, or HTTP endpoint triggers extraction in v2 HEAD.

A Go caller builds a `Curator` with `NewCurator(ghClient, wikiURL, org, repos, config, logger)`. The caller calls `RunExtraction(ctx, since)` with a `since` time. The caller then calls `Ingest(ctx, facts)`. The call POSTs the facts to the wiki `/api/ingest` endpoint:

```go
c := NewCurator(ghClient, wikiURL, org, repos, config, logger)
facts, err := c.RunExtraction(ctx, since)
err = c.Ingest(ctx, facts)
```

The design doc `src/docs/design/knowledge-system.md` plans a `scheduler.RunCurator(schedule)` wiring with a daily or on-merge trigger. This wiring is not implemented in v2 HEAD. The programmatic path above is the only way to run extraction today.

See [Knowledge system design](design/knowledge-system.md) for architecture context; this page documents the implemented operator-facing knobs.

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
| `schedule` | Defaults to `daily` when knowledge is enabled. The config field is stored for the scheduled curator flow described in the design docs; v2 HEAD does not enforce a parser here, so use simple operator conventions such as `daily` until your deployment wires a scheduler. |
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

See [Knowledge system design](design/knowledge-system.md) for architecture context; this page documents the implemented operator-facing knobs.

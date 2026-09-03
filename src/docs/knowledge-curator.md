# Knowledge promotion configuration

The `knowledge.curator` block now controls only promotion of existing verified facts between llm-wiki layers. The earlier merged-PR extraction pipeline was never wired into any binary and has been removed rather than leaving a config-promised feature that did not run.

```yaml
knowledge:
  enabled: true
  engine: llm-wiki
  curator:
    auto_promote_threshold: 0.9
```

| Field | Current behavior |
| --- | --- |
| `auto_promote_threshold` | Defaults to `0.9` when knowledge is enabled. `Promoter.AutoPromoteCandidates` selects facts whose llm-wiki page has `status == "verified"` and `confidence >= threshold`. |

Merged-PR extraction and its old scheduling/source-list knobs are intentionally not documented as supported configuration because no scheduler, CLI command, or HTTP endpoint triggered extraction.

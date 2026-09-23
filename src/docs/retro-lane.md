# Retro lane

The retro lane is a post-completion analysis pass for Hive work. It is disabled by default (`retro.enabled: false`) so existing hives see no behavior change unless operators opt in. LLM analysis is separately opt-in with `retro.analysis_model`; the empty default preserves phase-1 deterministic-only behavior.

When enabled, the lane runs from the governor tick on its own scan interval. It scans done/closed beads that have a closed or merged PR association in bead metadata or the lifecycle timeline, reconstructs a compact `RetroRecord`, and applies rule-based pattern detection. It does not post to GitHub.

## Reconstructed record

For each eligible bead the lane combines local ledgers:

- bead metadata: issue/bead identity, PR reference/state, claim/close times, explicit counters when present;
- lifecycle timeline: kicks, PR-open/merge events, and trajectory drift/pause markers;
- escalation ledger when still available: failed fix-attempt count for the PR.

The compact record tracks bead metadata, issue/PR refs, kicks received, CI failures/fix attempts, drift pauses, and wall-clock time from claim to close.
It also carries advisory autonomy outcome signals when producers record them:
plan revisions before approval, PR rework commits after review, rollback
events, and the affected user/repo/change-class scope. These signals are facts
for retro analysis only; they do not change ACMM configuration.

## Deterministic findings

Named threshold defaults are:

- excessive fix attempts: `>= 3`;
- excessive kicks before completion: `>= 5`;
- long stall: claim-to-close `> 7 days`;
- drift pause occurred: any trajectory drift/pause marker.
- plan accepted first pass: a recorded approval outcome with `0` plan
  revisions before approval;
- PR merged with no rework: a merged PR outcome with `0` review-driven rework
  commits;
- run rolled back: one or more rollback events.

Each finding is filed as an `advisory` bead attributed to actor `retro`, using the existing advisory-bead digest path. Source beads are marked with `retro_analyzed_at` after analysis to avoid duplicate findings.
Autonomy signal findings include scope metadata (`autonomy_scope_type`,
`autonomy_scope_value`, `autonomy_level`, and `autonomy_direction`) so the
dashboard can show them as ACMM overlay facts with no action button. A lack of
qualifying signals is terminal and acceptable; no nudge is shown. Automatic
promotion or demotion, including demotion-on-failure and per-repo scoping, is
deferred until the per-repo ceiling design settles.

## Optional LLM analysis

### Prerequisites: configure `governor.litellm` first

`retro.analysis_model` names a model that must be served by the hive's LiteLLM
gateway (`governor.litellm`). Configure the gateway before setting this field:

1. Set the endpoint and API key — either via environment variables
   (`HIVE_LITELLM_ENDPOINT`, `HIVE_LITELLM_API_KEY`, documented in
   [env-vars.md](env-vars.md)) or via the dashboard API
   (`PUT /api/config/governor/litellm`, see [api-reference.md](api-reference.md)).
2. Verify connectivity with `POST /api/config/governor/litellm/test`.
3. Set `retro.analysis_model` to any model name the gateway exposes, for example
   `gpt-4o-mini` or the default model configured in the gateway itself.

```yaml
retro:
  enabled: true
  analysis_model: gpt-4o-mini   # a model your governor.litellm serves
```

If `governor.litellm` is not configured, the retro lane silently skips model
calls and files only deterministic findings — no error, no alert.

### Analysis behavior

Set `retro.analysis_model` to a model served by `governor.litellm` to enable bounded model analysis. The lane only calls the model for records that already triggered deterministic findings, keeping cost proportional to actionable anomalies. The prompt contains the compact record and finding types/details with hard truncation bounds.

The model must return structured JSON:

- `root_cause_hypothesis`
- `process_improvement`
- `generalizable`
- `generalizable_lesson`

Invalid JSON is retried up to the shared structured-output retry bound. Transport, timeout, or validation failures fail open: deterministic advisory beads are still filed without model enrichment.

When analysis succeeds, advisory bead notes include a clearly marked “Model-generated retro analysis” section with the root-cause hypothesis and actionable process improvement.

## Knowledge graph feeding

If the model marks a lesson generalizable, Hive quality-gates it before ingestion: length bounds are enforced and secret-like token patterns are rejected with the shared log-scrub detector. Accepted lessons are stored as `pattern` facts tagged `retro` and `lesson`, attributed to source `retro` with the source bead and PR reference. Existing knowledge search/vault deduplication is checked first; when no matching fact exists, the normalized lesson hash keys the fact slug to avoid duplicates.

If a graph store is attached (`/data/graph/knowledge.db`), the fact gets a `derived_from` edge to the retro source reference. The fact is then available to the primer like other knowledge entries.

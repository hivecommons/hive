# Run artifacts

Long-running runs keep review state in the existing plan and lease artifacts.
They do not introduce a new store, CRD, DSL, or GitHub credential path.

## Multi-repo waves

A plan may declare a repository, repository role, and wave on each child task
using bracket annotations such as `[repo:owner/repo] [role:service] [wave:1]`.
Hive stores those annotations as child metadata and adds a wave barrier: children
in the next wave depend on all children in the previous wave, so the next wave is
not offerable until the prior wave has finalized or been explicitly skipped.

The stage runner reads the Spektacular status verb, refuses fan-out when the hub
constellation overlap index reports two spokes claiming one repository, and then
creates one implementation lease per participating repository for the wave. Each
lease is keyed by the run key and wave number and remains subject to the existing
role, capability, and ioscan guard invariant.

Partial failure uses forward-fix by default. A failed repository's lease ends
without `document_status: final` and enters the normal stale/human handling path.
The run remains open with `waiting_on=human`; repositories that already merged
are not reverted. The run detail groups all PRs for a wave under one header and
offers one approve action for the plan wave rather than per-PR approvals.

## Finding identity

Audit findings use a deterministic offline identity:

```text
(subject digest, predicate, normalized location)
```

The subject digest is the immutable content or artifact digest under inspection.
The predicate is the versioned claim being made about that subject. The location
normalizer keeps the path or logical scope but drops volatile line and column
coordinates, so two workers that cite the same file at different line numbers
for the same subject and predicate report one finding. Different predicates
never dedupe, even when they share a subject and title.

This identity is used only as a candidate/reporting key. Missing or malformed
identity fields fall back to the existing title/file heuristics, and uncertain
matches remain `Unknown` for human review; no LLM or external embedding service
participates in the identity path.

## Provenance guard

Stage receipts carry `input_revision` and `contract_revision`. Until the
Spektacular status verb exposes a stable current-spec join key, Hive uses the
receipt chain as the provenance source: when a plan stage finalizes, the plan's
metadata records the spec `input_revision` as `spec_revision`. Before an
implementation stage starts, Hive compares that recorded revision with the
current spec artifact revision. A mismatch refuses the kick, sets
`waiting_on=human` with reason `stale_plan`, and leaves the existing plan gate
as the recovery path: re-run or re-approve a fresh plan so the stored revision
matches the current spec. This is intentionally metadata-only and adds no
store, CRD, DSL, or credential path.


## Cross-repo audit query and retention

Hive exposes the run audit index as a read-only projection over artifacts it
already writes; it is not a new store, CRD, DSL, or credential path. `GET
/api/runs/audit` is owner-gated and accepts `repo`, canonical `run`
(`owner/repo#number`), `since`, `until`, `kind`, `limit`, and `page_token`. The
handler scans the retained dashboard audit log, lifecycle timeline, lease stage
receipts, and plan epics from all configured bead stores, normalizes each row to
`source`, `kind`, canonical run key, repo, timestamp, actor, artifact id, and
attributes, then returns a deterministic paginated list. `stage_approval`
timeline events and `stage_receipt` lease receipts are first-class rows so
reviewers can trace owner approvals and runner receipts without reading chat
history.

The retention contract reuses current behavior. Audit entries are retained by
the dashboard audit log rotation (`MaxAge=90d`, `MaxSize=5MiB`,
`MaxBackups=3`), with an in-memory fallback ring of 500 entries for deployments
without `/data`. Timeline events are retained as lifecycle journeys with the
existing `timeline.MaxJourneys` capacity (500 journeys) and optional timeline
persistence. Plan epics and lease receipt metadata live in their existing bead
stores and lease/timeline artifacts. There is deliberately no
`runs.audit.retention` setting unless a future change replaces these underlying
retention knobs. When a query reaches before retained coverage or asks for a run
whose artifacts may have aged out, the response includes a row with
`kind=expired`, `status=expired`, and `state=Unknown`; callers must render that
as unknown/expired evidence rather than treating the missing source rows as an
empty audit history.

## Commit artifact linkage

Implementation-stage commits may carry informational trailers that connect a
commit back to the run, plan, and specification clause that motivated it:

```text
Hive-Run: <work item key>
Hive-Plan: <plan section or plan name>
Hive-Spec: <spec name>#<clause id>
```

- `Hive-Run` is the canonical run key, such as `hivecommons/hive#8311`.
- `Hive-Plan` names the approved plan section or plan identifier used for the
  implementation stage.
- `Hive-Spec` names the Spektacular spec and clause identifier that supplied the
  requirement.

The implementation stage writes all three trailers on each commit it creates.
The grammar is parsed by the shared `pkg/runtrailer` package so the dashboard
trace reader and the PR attribution reader accept the same trailer spelling and
whitespace rules. The dashboard trace reader resolves them through the retained
timeline and audit entries so reviewers can find the plan section, spec clause,
approval record, and agent rationale without reading chat history.

These trailers are evidence, not authority. A missing trailer is recorded as an
`artifact_link_missing` audit finding; it does not fail the run, reject the
commit, or write any state outside the existing audit log and timeline.

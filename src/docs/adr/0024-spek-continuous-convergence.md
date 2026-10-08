# ADR-0024: Spek continuous convergence

## Status

Accepted.

## Context

Spektacular campaigns previously ran `spec -> plan -> implement` once. That
converges one generation, but long-lived projects drift as the codebase and
surrounding requirements change.

## Decision

Continuous convergence is opt-in under `runs.spektacular.recheck`. A due
campaign creates a linked revision (`revision_of`, `revision`) and starts that
revision at `spec`; the original campaign is never overwritten. Manual
`POST /api/campaigns/{id}/recheck` uses the same path, returns `409` if a
revision is already in flight, and requires either enabled rechecks or
`?force=true`.

Cadence is scheduler-owned and runs from the existing Spek runner tick. The
default interval is `168h`; per-campaign metadata can override it. Recheck
revisions keep the existing spec and plan human approval checkpoints.

When the revision plan is final, Hive diffs its task list against the prior
final plan by normalized task-content hash. Only new or changed tasks are
imported into the planner. The import is capped by
`runs.spektacular.recheck.max_delta_tasks` (default `50`) and records an audit
entry when capped. Empty deltas create no planner children.

Each revision records drift evidence:

- `codebase_changed`
- `prior_revision`
- `delta_count`
- `recheck_reason` (`cadence` or `manual`)
- optional `external[]`, `external_count`, and `sources_failed[]` discovery
  evidence when outward discovery is enabled

The evidence source interface is `RecheckEvidenceSource`.

### Discovery sources

The outward-looking half is opt-in under
`runs.spektacular.recheck.discovery` for typed release/activity/feed signals and
under `runs.spektacular.recheck.sources` for exact document snapshots. Typed
discovery rejects undeclared kinds and hosts outside
`variables.security.http_allowlist`, records evidence on `drift.external`, and
includes that evidence in the spec-stage prompt as context. Exact document
sources are retained as `drift.drift_source` before the revision starts its
spec lease; they require an explicit relay egress proxy and exact-host
allow-list, never follow redirects or links, and have fixed request, time, and
response-size bounds. Empty source lists perform no discovery network access.
All discovery is read-only, non-fatal, and untrusted evidence only; source
failures are recorded and never bypass human checkpoints or automatically apply
external changes.

## Consequences

Operators can keep a Spek campaign converging over time without duplicating
unchanged planner work. The generation contract stays durable and review-gated,
while future evidence sources can be added behind the same interface.

## v6 addendum: run-rewind semantics (#10734, #10799)

Status: Accepted (2026-10-06). The design owner is the maintainer who
approves and merges this addendum.

This addendum amends the Decision above for the `v6` line. Where the two
disagree on `v6`, the addendum wins. `v5` keeps the linked-revision behaviour
described above.

### Context

`v6` revises campaigns in place. `InceptionEngine.ReviseExternalCampaign`
(`src/pkg/knowledge/inception.go`) already rewrites the existing archive: it
appends a `CampaignRevisionHistory` entry, bumps `Revision`, writes
`RevisionOf = ""` and takes a 24h `CampaignLease{Surface: "revise"}`
(`campaignLeaseTTL`). The recheck path in
`src/pkg/dashboard/campaign_recheck.go` was ported from `v5` and still uses
the linked-revision model:

- `triggerCampaignRecheck` records a second hub lease on
  `<repo>!<revision.ID>:spec` with task `spek-recheck-<revision.ID>` (from
  `recheckTaskPrefix`) under `runAdmissionIdentity`.
- `decorateCampaignRechecks` decides in-flight from `RevisionOf != ""`, which
  is never true on `v6`.
- `CampaignDrift.PriorRevision` is filled with
  `firstRunNonEmpty(base.RevisionOf, base.ID)`.

So recheck is turned off on `v6` until #10734 lands:
`spekRecheckDisabledOnV6 = true` makes `TickCampaignRechecks` return early and
makes `triggerCampaignRecheck` return `errRecheckUnavailable`. The recheck
tests in `src/pkg/dashboard/api_campaigns_test.go` and
`src/pkg/dashboard/recheck_discovery_test.go` are skipped with `t.Skip("Spek
continuous convergence recheck is disabled on v6 pending #10734")`.

The rules below define a recheck as a rewind of the existing run, so that the
implementation can turn recheck back on. Terms used below:

- A **generation** is one pass of a run from `spec` to completion.
- `runKey` is the run's canonical work-item key (`worksource.Ref.Key()`, or
  `Campaign.RunKey`).
- `repo` is the run's repository.

### Decision

**R1. A recheck rewinds the existing run to a new generation.**

1. A recheck does not create a sibling campaign, a `-rN`/`-rev-N` archive, or
   a `RevisionOf` link. It rewrites the same archive (same `ID`) and the same
   run (same `runKey`). On that archive it:
   - sets `Revision` to N+1;
   - resets the campaign to `CurrentStage = spec`;
   - writes the new `Drift` for generation N+1.
2. The rewind is one locked write in `src/pkg/knowledge/inception.go`. The
   implementation can reuse `ReviseExternalCampaign` or add a dedicated rewind
   helper next to it.
3. Prior generations' stage receipts are kept as history and are never deleted,
   overwritten or invalidated. This covers files written by `writeStageReceipt`
   to `<runReceiptsDir>/<runKey>/<stage>-gen<gen>.json`, stage captures, audit
   proofs (`deps.AuditProofs`) and run outcome rows (`RunHistoryEntry` /
   `RunOutcomeCompleted` / `RunOutcomeMerged`). To keep receipts from
   overwriting each other, the rewound `spec` stage lease uses a hub `gen`
   higher than every `gen` the run has already used. Generations never reuse
   or reset `gen`.
4. Only the current generation determines the campaign's stage, status, plan
   diff baseline and outcome. A prior generation's completion or merge never
   marks the rewound generation completed.
5. `CampaignRevisionHistory` becomes the run's generation log. Each rewind
   appends exactly one entry, and nothing else appends to it during a recheck.
   The entry records:
   - the closed generation's snapshot, using the existing fields `id`,
     `revision`, `phase`, `archived_at` and `title`;
   - details of the rewind that closed it: reason (`cadence` or `manual`),
     actor, rewind timestamp, the last hub `gen` the closed generation used
     (so its receipts can be found), and a copy of the `CampaignDrift` that
     triggered the rewind.

   The field names for these additions are left to the implementation. They
   must be `omitempty`, so archives written before the change still decode.
   Entries are never rewritten or removed.

**R2. In-flight predicate.**

1. A campaign's recheck is in flight when either of these holds:
   - its revise lease is held: the archive's `Lease` is non-nil, has
     `Surface == "revise"`, and `now` is before `ExpiresAt`;
   - `Drift != nil` and the campaign is not completed (`CurrentStage !=
     "completed"` and `Status != "shipped"`).
2. The revise lease is the `CampaignLease` on the run's own archive, which is
   stored at `inception/campaigns/<id>/state.json`. The rewind takes it with
   owner `actor` (`system` for cadence). It lasts until the rewound `spec` stage
   lease is recorded (R3), or until it expires after `campaignLeaseTTL`. After
   that, `Drift` carries the in-flight state.
3. `decorateCampaignRechecks` computes `Recheck.InFlight` from this predicate
   for the campaign itself. It no longer uses the `RevisionOf` map.
4. `Drift` is cleared from the archive when the rewound generation's final stage
   completes, meaning the run records a completed or merged outcome for the
   current generation. By then R1 has already copied the drift snapshot into
   the generation log at rewind time, so clearing it loses no evidence.
   Clearing `Drift` also clears in-flight. While `Drift` is set,
   `campaignFromInceptionArchive` keeps reporting the stage as `spec` (current
   behaviour).
5. `CampaignDrift.PriorRevision` holds the closed generation's `Revision` as a
   decimal string (for example `"3"`), not a campaign ID.

**R3. The rewound generation reuses the run's own lease key.**

1. The rewound generation's `spec` stage is admitted on the run's own stage
   lease key `<repo>!<runKey>:spec`. This is the key `AdmitTriagedRun` builds in
   `src/pkg/dashboard/stage_leases.go`, with task `run-admit-<runKey>`
   (`runAdmissionTaskPrefix`, through `sanitizeReceiptSegment`) and identity
   `runAdmissionIdentity`. `runKeyOfLease` therefore maps the rewound
   generation to the same run, and later stages advance through
   `AdvanceStageLease` as for any run.
2. The second lease goes away. `triggerCampaignRecheck` no longer records
   `<repo>!<revision.ID>:spec`, the `spek-recheck-*` task ID is not issued, and
   `recheckTaskPrefix` is removed.
3. Concurrent rechecks: when the in-flight predicate (R2) holds, a second
   recheck creates no generation, no lease and no history entry. A manual
   `POST /api/campaigns/{id}/recheck` returns `409` (`errRecheckInFlight`). The
   cadence tick (`TickCampaignRechecks`) skips the campaign.
4. A rewind is refused, and the archive is left unchanged, when either of these
   is live:
   - a revise lease held by a different owner (`ErrCampaignLeaseHeld`);
   - a stage lease for the same `runKey`.

   This holds even if two requests race past the predicate.

**R4. API compatibility on v6.**

1. `Campaign.RevisionOf` (`src/pkg/dashboard/api_campaigns.go`) and
   `InceptionCampaignArchive.RevisionOf` stay in `v6`. Both are deprecated and
   `omitempty`, and new code never writes them, so the API value is always empty
   and the key is omitted from JSON.
2. `RevisionOf` is not dropped outright for two reasons:
   - `dedupeCampaignArchives` and `stableCampaignArchiveID` still read it to
     fold legacy pre-`v6` revision chains;
   - removing it from `Campaign` would break clients that still decode the
     key.
3. The `Campaign.RevisionOf` API field is removed in the next release line
   after `v6`. The archive field stays only for as long as legacy chains are
   still folded.
4. `revision` (the generation number) and `history` (the generation log) are
   the supported way to follow a campaign across rechecks.
5. The dashboard UI must not render a sibling-archive or "revision of" link. As
   of this addendum, no dashboard asset reads `revision_of`.

### Consequences

- A Spek campaign keeps one ID, one run key and one card for its whole life.
  Rechecks show up as a rising `revision` and a longer `history`, not as new
  campaigns.
- Evidence is append-only: receipts stay distinct by `gen`, and drift snapshots
  are kept in the generation log.
- In-flight state comes from data stored on the run itself, so it survives
  restarts without cross-campaign lookups.
- The `prior_revision` value changes from a campaign ID to a revision number.
  Consumers that parsed it as an ID must switch to it.

### What the implementation children must do

- #10800, recheck core:
  - implement R1–R4 in `src/pkg/dashboard/campaign_recheck.go`,
    `src/pkg/dashboard/api_campaigns.go` and `src/pkg/knowledge/inception.go`;
  - remove `spekRecheckDisabledOnV6` and `errRecheckUnavailable`;
  - un-skip the recheck tests and assert that `Revision` increments, the same
    `ID` and `runKey` are kept, `current_stage=spec`, the in-flight `409` is
    returned, and no `spek-recheck-*` lease is created;
  - update the linked-revision wording in `src/docs/spektacular.md` and
    `src/docs/api-reference.md`.
- #10801, outward discovery: record discovery evidence on the rewound
  generation's `Drift` (and therefore in its generation-log entry, R1), then
  un-skip `src/pkg/dashboard/recheck_discovery_test.go`.

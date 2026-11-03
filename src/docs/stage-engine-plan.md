# Central stage engine — implementation plan (ADR-0025)

Companion to [ADR-0025](adr/0025-central-stage-engine.md), which is the
authoritative definition of every interface named below. Six PRs, each
≤ ~600 LOC, designed so PR-A, PR-B and (after B) PR-C/PR-D can be worked in
parallel by different agents. Base branch `v5`.

## Dependency graph

```
PR-A goldens + shadow harness ─────────────┐
PR-B pkg skeleton (interfaces, Facts, adapters) ──┬── PR-C single writer + admission test ──┐
                                                  └── PR-D relay/claim paths submit facts ──┼── PR-E flag, migration, docs ── PR-F delete legacy
```

PR-A and PR-B are independent. PR-C and PR-D depend only on PR-B (they can
be developed against PR-B's branch and rebased). PR-E depends on A, B, C.
PR-F is a follow-up after 48 h in `enforce`.

PR-F's remaining PR-side relocation (#13779) puts the classifier and pass
post-steps in `pkg/stageengine/derive_pr*.go`. `pkg/stageengine/prstage`
owns the lane reason vocabulary and shared observation shapes; automerge
only gathers facts and previews through engine helpers. Both derivers are
built in: the temporary `SetDerivers` registry and PR adapter are removed.
The PR goldens now live in `pkg/stageengine/testdata/stage_goldens`; a thin
automerge facts-fold parity replay is retained for one release. Review prose
classification lives in the dependency-free `pkg/review/reviewtext` leaf,
avoiding the `review` → `config` → `stageengine` import cycle.

## PR-A — golden fixtures and shadow harness (independent)

Scope: freeze today's behaviour. No production code changes.

- `src/pkg/stageengine/testdata/stage_goldens/*.json`: one fixture per
  lane × reason × verdict × author-kind × adoption combination reachable in
  `ClassifyStage`/`ResolveStage` (`src/pkg/stageengine/derive_pr_classify.go:261`,
  `src/pkg/stageengine/derive_pr_classify.go:454`), serialised as
  `[]stageObservation` → `{stage, rank, reason, role, terminal}`. Generated
  by a `go test -run TestStageGoldens -update` flag; checked in.
- `src/pkg/github/testdata/issue_stage_goldens/*.json`: one per bucket ×
  claim kind × park label × likely-done × covering-PR stage for
  `IssueStageFor` (`src/pkg/github/issue_stage_labels.go:205`).
- `src/pkg/github/automerge/stage_goldens_test.go`,
  `src/pkg/github/issue_stage_goldens_test.go`: replay fixtures; fail on any
  diff.
- `src/pkg/stagelabels/shadowdiff/` (tiny): `Diff(legacy, engine Decision) []Mismatch`
  and a `Recorder` that counts `stage_shadow_mismatch_total{kind,legacy,engine}`
  and logs one line per distinct mismatch. Pure; no engine import. (Served
  the rollout; deleted with the legacy comparator in PR-F.)

Tests: the golden tests themselves; `shadowdiff` unit test.
Files touched: ~6 new, 0 modified. Depends on: nothing.

## PR-B — `pkg/stageengine` skeleton, shared `Facts`, adapters (independent)

Scope: land the ADR's types verbatim and wrap today's derivation behind them
without moving its logic.

- `src/pkg/stageengine/{facts.go,decision.go,deriver.go,writer.go,reconciler.go,transition.go,facts_sink.go,mode.go,metrics.go}`:
  the interfaces and types exactly as in ADR-0025 §Design. `New(Options)`
  returns a reconciler whose `Reconcile` derives, calls `Expected`, records
  through `Store.Observe`, and in `ModeEnforce` calls `Writer.Apply`; in
  `ModeShadow` it calls `Options.Legacy` and the shadow recorder; in `ModeOff`
  it returns without doing anything.
- `transition_table.go`: `Expected(kind, from, to)` generated from the
  mermaid diagram (`src/docs/labels-and-control-signals/stage-labels.md:120`) plus
  `stagelabels.IsRegression` rules; `transition_table_test.go` asserts the
  table matches the diagram (parse the fenced block).
- Adapters (no logic moved yet):
  - `src/pkg/github/automerge/stage_facts.go`: `factsFrom([]stageObservation) stageengine.Facts`
    and `prDeriverAdapter` implementing `stageengine.Deriver` by converting
    `LaneFact` back to `stageObservation` and calling `resolveStage`, then
    applying ApplyStageLabels' pure post-steps (07x author-kind split,
    normalized-fixing guard, `RouteHuman`, DCO override, 074 yield) from
    `PRFacts.Pass` (`stageengine.PRPassFacts`), which the pass fills with
    the forge reads and store state those steps consumed. The shadow
    comparison is therefore written-vs-derived, and
    `TestStageGoldensEngineParity` holds the deriver to every golden's
    `final_stage` / `final_reason`.
  - `src/pkg/github/issue_stage_facts.go`: `factsFrom(IssueStageInput) stageengine.Facts`
    and `issueDeriverAdapter` wrapping `IssueStageFor`.
  - Both adapters originally backed `stageengine.DeriveIssue`/`DerivePR`
    through `SetDerivers(issue, pr Deriver)`. PR-F moved both bodies into
    the engine and removed that temporary registry (#13778, #13779).
- Property test `TestReconcileIdempotent`: for random `Facts`, `Reconcile`
  twice with a fake writer → second call writes nothing and the item carries
  exactly one `stage/*` label.

Tests: adapters round-trip the PR-A goldens unchanged (if PR-A has landed;
otherwise a smaller inline set); idempotency property test; transition-table
test. Files touched: ~10 new, 2 new adapter files in existing packages.
Depends on: nothing (uses PR-A goldens when present).

## PR-C — single `StageWriter`, admission test, sweeps switched (after B)

Scope: one writer, every sweep uses it.

- `src/pkg/github/stage_writer.go`: `type stageWriter struct{ c *Client }`
  implementing `stageengine.StageWriter` over `stagelabels.Enforce` with a
  per-`Ref` mutex and the existing fresh-read/journal behaviour from
  `src/pkg/github/stage_labeler.go:63`. This is the only implementation.
- `src/pkg/github/automerge/automerge_stage_labels.go`: `ApplyStageLabels`
  builds `Facts` per PR and calls `Reconciler.ReconcileBatch`; delete
  `engineStageLabeler` (replaced by `pkg/github/stage_writer.go`).
  The 072 nudge / 074 adoption hooks read the returned `Transition`s instead
  of the local verdict.
- `src/pkg/github/issue_stage_labels.go`: `SyncIssueStageLabels` builds
  `Facts` and calls `ReconcileBatch`; the PR-link safeguard stays, keyed off
  the `Transition`.
- `src/pkg/stageengine/admission_test.go`: walks `src/` with `go/ast`; fails
  if any file outside `stage_writer.go` and `pkg/stagelabels` calls
  `AddLabels`/`RemoveLabel*`/`EnsureLabel*` with an argument that is a
  `stage/` literal, `stagelabels.*.Label()`, `claims.LabelClaimed`,
  `claims.LabelPRInProgress` or `CoveredByPRLabel`, or implements
  `stageengine.StageWriter`. Allow-list is a short slice in the test; PR-D
  empties it.
- Race test `TestRelayVsSweepOneStage` (`-race`): a `FactSink.Claimed` and a
  `ReconcileBatch` for the same ref interleaved N times → exactly one stage
  label at the end and no anomaly other than the expected 010→020.

Tests: as above plus existing automerge/issue-stage tests passing unchanged
(the writer is the same `Enforce`). Files touched: ~5 modified, 2 new.
Depends on: PR-B.

## PR-D — relay, claim, ledger and dashboard paths submit facts (after B)

Scope: no out-of-cycle code names a stage label.

- `src/cmd/hive/claimswire.go`: ledger hooks call `FactSink.Claimed` /
  `ClaimReleased` instead of `claimStageFor` + label writes
  (`src/cmd/hive/claimswire.go:176-178`, `src/cmd/hive/claimswire.go:236-238`).
- `src/pkg/github/issue_inprogress.go`: the `claim` request submits
  `InProgress`; the sweep's 040 swap becomes `CoveredBy`
  (`src/pkg/github/issue_inprogress.go:126-141`, `src/pkg/github/issue_inprogress.go:186`).
- `src/pkg/github/prclaims.go`: 040 add/remove → `CoveredBy`/`Uncovered`
  (`src/pkg/github/prclaims.go:1777`, `src/pkg/github/prclaims.go:2037`).
- `src/pkg/dashboard/contribute_verdict_settle.go:216` → `CoveredBy`.
- `src/pkg/github/issue_request_label.go`: an agent request for a parking
  label becomes `Parked`; `agentMayAddLabel` keeps the allow-list for the
  non-stage `needs-human` (`src/pkg/github/issue_request_label.go:75`).
- `src/pkg/github/legacy_decision_labels.go` un-park → `Unparked`.
- Delete `Client.ApplyStageLabel` and the stage parameter of
  `AddLabelsFresh` (`src/pkg/github/stage_labeler.go:63`, `src/pkg/github/stage_labeler.go:114`).
- Vocabulary cleanup (independent of B/C): `claims.LabelClaimed` and
  `claims.LabelPRInProgress` render through `stagelabels` (kept as vars for
  callers); review dispatch uses `stagelabels.AdoptedByHive.Label()`.
  `stagelabels` owns the retired claim spellings, with aliases in `claims`.
  Config registers stage slugs from its own side, removing both the direct
  `stagelabels` → `claims` and indirect `stagelabels` → `config` → `claims`
  dependencies so claims can import the vocabulary without a cycle.
- Empty the admission-test allow-list.

Tests: existing claimswire/in-progress/prclaims tests rewritten to assert
facts submitted (fake sink) rather than labels written; the fake sink runs a
real reconciler with a fake writer so label expectations still hold.
Files touched: ~8 modified. Depends on: PR-B (the `FactSink` type); rebases
over PR-C for the allow-list.

Landed as [#13009](https://github.com/hivecommons/hive/issues/13009). The
sink hangs off `github.Client` (`StageFacts()` / `SetStageFactSink`); its
default is an enforce-mode reconciler over `stagelabels.Shared()` whose
Enrich hook re-reads the item (labels, open/closed, issue-vs-PR) before
deriving. Deviation: `src/pkg/claims/comments.go` keeps its literals —
`pkg/config` imports `pkg/claims` and `pkg/stagelabels` imports `pkg/config`,
so the import would cycle; `TestClaimLabelConstantsMatchStages` in
`pkg/stagelabels` pins the spellings instead. Test doubles live in
`src/pkg/stageengine/stageenginetest` (`Writer`, `NewSink`, `Recorder`).

## PR-E — `stage_engine.mode` flag, shadow→enforce rollout, migration, docs (after A, B, C)

Landed (hivecommons/hive#13010): `cmd/hive/stage_engine.go` builds the one
`stageengine.Options`, `Client.StageReconciler` the one reconciler; the mode
is a runtime switch (`stageengine.ModeSwitch`) re-read before each sweep,
`off` was legacy pass-through (retired by PR-F); `/api/status.stageEngine` and the tracker strip
report mode / anomalies. The original scope follows.

- `src/pkg/config`: `governor.labels.stage.engine.mode` (`off|shadow|enforce`,
  default `shadow` during the rollout, `enforce` since the default flip); Settings → Labels tab exposes it; `hivectl config`
  validation.
- `cmd/hive` wiring: construct one `stageengine.New(Options{...})` with the
  `stageWriter`, `stagelabels.Shared()`, the adapters and the PR-A shadow
  recorder; pass it to the automerge engine, issue sweep and relay.
- Metrics registered; the activity tracker status strip shows mode,
  mismatch count (shadow) and anomaly count.
- Docs: `labels-and-control-signals.md` (mermaid stays; "How a stage is
  written" section replaces the direct-write paragraph at
  `src/docs/labels-and-control-signals/reference.md:313-316`), `dashboard-sections.md`,
  `config-layering.md`, ADR-0025 status → Accepted when enforce is default.
- Changelog fragment.

Rollout: ship with `shadow` on hive's own spoke; after 48 h with
`stage_shadow_mismatch_total == 0` and zero duplicate-stage pills (tracker
`Healed` count 0), flip the spoke to `enforce`; after a further 48 h flip the
default. Rollback: set `mode: off` (legacy paths intact until PR-F).
Files touched: ~8. Depends on: PR-A, PR-B, PR-C (PR-D is not required for
shadow but is for the default flip).

Default flip landed (hivecommons/hive#12999): an unset
`governor.labels.stage.engine.mode` now yields `enforce`
(`config.DefaultStageEngineMode`) after 48 h clean `enforce` on hive's spoke;
an unparsable value still falls back to `shadow`, never `enforce`. A hive
that wants to pause stage writes sets `mode: shadow`. ADR-0025 is Accepted.
(`stageengine.DefaultMode` was `shadow` as the library default until PR-F
retired the legacy comparator; with nothing else able to write it is
`enforce` too.)

## PR-F — move derivation in, delete legacy (follow-up, ≥48 h after enforce default)

Shipped as three PRs (hivecommons/hive#13011):

- **F1 — retire the legacy comparator** (landed): `Options.Legacy`,
  `LegacyTable`, `ModeOff`, the shadow-diff recorder
  (`pkg/stagelabels/shadowdiff`), `stage_shadow_mismatch_total` and the
  status/strip mismatch fields are gone. `shadow` is now a pure pause
  (derive, count, write nothing). The sweeps no longer feed their own
  derivation to the reconciler; each package registers its deriver at
  init. Retiring the comparator exposed that the PR adapter's enforce-mode
  Decision dropped the pass's `Stalled` and configured `KindLabels`
  (`PRPassFacts` now carries both) and kept the role on a terminal PR.
- **F2 — issue derivation moves in, review-state fold closed** (landed):
  `IssueStageFor`'s body is `stageengine.deriveIssue` over `IssueFacts`
  that now carry the resolved label families (`HoldLabel`, `BlockedReason`,
  `UmbrellaLabel`, `TriageAwaiting`, `NeedsHumanLabel`); `Decision.Covered`
  replaces the shim's second return. `pkg/github` keeps the one place the
  hive's configurable label vocabulary meets the engine
  (`resolveIssueLabelFacts`, used by the sweep's facts builder, the sink's
  fresh-read enrichment and the `github.IssueDeriver` wrapper), so the
  issue goldens stay in `pkg/github` and replay through the shim unchanged
  — the corpus is keyed on labels, and the labels are configuration.
  `cmd/hive/stageengine_wire.go` is gone: the issue deriver is built in and
  automerge registers the PR deriver at init. The ADR's open question is
  closed: `ReviewFacts.State` is the fold `verdictStateFrom` defines
  (Approved → `VerdictApproved`; unconfigured → `VerdictUnavailable`;
  `changes_requested`/`reject`/`request_changes` → `VerdictChangesRequested`;
  `requires_human` or a decision request → `VerdictRequiresHuman`; else
  `VerdictPending`), the 050-after-reviews stall was fixed in
  hivecommons/hive#13112, and the PR deriver reads `State` for the
  verdict's routing while the raw verdict string stays for the reason text.
- **PR derivation body** (`classifyStage`/`resolveStage`, ~1,200 lines over
  the lanes' reason vocabulary) stays in `pkg/github/automerge` behind the
  `PRDeriver` adapter and the PR goldens; moving it is a pure relocation
  that the goldens pin and is tracked separately (see #13011's close-out
  comment) rather than held against the epic.

## Acceptance metrics

| Gate | Metric | Target |
|---|---|---|
| shadow → enforce (spoke) | `stage_shadow_mismatch_total` (series retired by PR-F; held at 0) | 0 over 48 h |
| shadow → enforce (spoke) | duplicate-stage pills (`Outcome.Healed`) | 0 over 48 h |
| enforce → default | `stage_transition_anomaly_total` | only pairs explained by a documented event (merge, close, human label edit) |
| any | GitHub REST calls per cycle (`hive:issue_stage_labels`, `stageRESTCaller`) | not above the pre-engine baseline |

## Amendments after PR-E

- **030 needs PR evidence** ([#13069](https://github.com/hivecommons/hive/issues/13069)):
  `ClaimFact.PRStarted` (ledger `Claim.PRStarted`, marker `pr_started=true`)
  is set by the start-signal seam only for a `hive-open-pr --issues` request
  or a `<!-- hive:start branch=… -->` / `pr=…` marker; `IssueStageFor` and
  the engine adapter derive `030-pr-in-progress` only when it is set and
  `020-claimed` for every other live claim or start mark. The diagram and
  `transition_table.go` gained `020 → 010`, `030 → 010` (claim released /
  lapsed with no PR) and `020 → 040`, so claim expiry is no longer an
  anomaly. Landed in the same PR for legacy and engine so the shadow
  mismatch gate stays at 0.

- **Covering-PR close and claim reshuffles** ([#12999](https://github.com/hivecommons/hive/issues/12999)):
  the first `enforce` window surfaced legitimate undrawn issue edges. The
  diagram and `transition_table.go` gained `040 → 010`, `040 → 030`,
  `040 → 020` (covering PR closed with nothing / a start marker / the claim
  still live) and `030 → 020` (PR evidence lapsed, claim held).

## Tracking

Tracking issue: [#12999](https://github.com/hivecommons/hive/issues/12999) *Central stage engine (ADR-0025)* — one checkbox per PR
above; each PR body carries `Refs #<tracking>`.

# ADR-0025: One stage engine derives and writes every `stage/*` label

Status: Accepted

## Current state and reading route

One engine derives the stage from observed issue/PR facts; one writer applies
`stage/*` labels. Unexpected transitions are recorded, not rejected. This
engine describes lifecycle state; it does not grant merge authority.
The [scope section](#scope-and-extension-points) also records the later,
scheduler-owned `stage/017-possible-existing` exception; do not infer that
the original single-writer decision removed that separate prior-art gate.

- **Normal operation:** `governor.labels.stage.engine.mode: enforce` (default).
- **Pause engine label writes:** use `shadow`; derivation and observation
  continue. It does not reactivate legacy writers, stop merges, or disable
  the separate scheduler-owned prior-art gate.
- **Not a rollback option:** `off` and the legacy derivation paths were retired.
  To revert code, use the deployment's normal release rollback procedure.
- **Implementers:** [Decision](#decision) and [Design](#design) contain the
  contract; [Review-state fold](#review-state-fold-resolved) describes the
  resolved review-state rule.

The [implementation plan](../stage-engine-plan.md) and Context below preserve
the rollout rationale. They are not a list of remaining operator actions.

## Context

**Original rollout context, with later implementation annotations.** The
half-centralized design and gaps below motivated the migration; do not read
the old writers as supported fallback paths.

The owner's intent: *labels applied from a central facility so that managing
them via a state machine is easier.* Hive is half-way there.

**What is already central.** `pkg/stagelabels` is the single vocabulary and
renderer: the ordered `Table` (`src/pkg/stagelabels/stagelabels.go:220`), the
kind clamp `Stage.For` (`src/pkg/stagelabels/stagelabels.go:205`), the
idempotent label `Plan` (`src/pkg/stagelabels/stagelabels.go:662`), the
re-read-then-write `Enforce` that heals duplicate stage pills
(`src/pkg/stagelabels/enforce.go:59`), in-place renames (`Renames`,
`src/pkg/stagelabels/stagelabels.go:762`), and the `Store` whose `Observe`
records transitions, regressions and history hops
(`src/pkg/stagelabels/store.go:397`, `IsRegression` at
`src/pkg/stagelabels/store.go:540`). The dashboard activity tracker and the
hub read that store and nothing else (`stageStoreOrShared`,
`src/pkg/dashboard/stage_tracker.go:1927-1933`).

**What is not central — three gaps.**

1. *Two derivation engines.* Issues are staged by `IssueStageFor` /
   `SyncIssueStageLabels` (`src/pkg/github/issue_stage_labels.go:205`,
   `src/pkg/github/issue_stage_labels.go:337`) from the enumeration bucket. PRs
   are staged by the automerge engine: each lane calls `recordStage`
   (`src/pkg/github/automerge/automerge_stage_labels.go:365`), observations are
   sorted by `stageLanePrecedence` (self 3 > trusted 2 > contributor 1,
   `src/pkg/github/automerge/automerge_stage_labels.go:177`), each is classified
   by `ClassifyStage` (`src/pkg/stageengine/derive_pr_classify.go:261`)
   into a ranked opinion (`prstage.RankNone..prstage.RankOwned`,
   `src/pkg/stageengine/prstage/types.go:145-148`), and
   `ResolveStage` picks the winner
   (`src/pkg/stageengine/derive_pr_classify.go:454`). These PR helpers now
   live in the engine (#13779); automerge keeps only gathering-side previews. Shared concepts —
   parking (045/085, `needs-human`, hold labels), author kind (070/071/072),
   adoption (074), terminal states, `origin/*` — are modelled twice with
   different field names, so every taxonomy change lands in both:
   [#12701](https://github.com/hivecommons/hive/pull/12701) (86 files),
   [#12713](https://github.com/hivecommons/hive/pull/12713) (31),
   [#12722](https://github.com/hivecommons/hive/pull/12722) (33),
   [#12715](https://github.com/hivecommons/hive/pull/12715) (53),
   [#12737](https://github.com/hivecommons/hive/pull/12737) (8),
   [#12856](https://github.com/hivecommons/hive/pull/12856) (44).
2. *Not one writer.* Automerge writes through its own
   `engineStageLabeler` (now retired in favour of `pkg/github/stage_writer.go`);
   the issue path uses `Client.StageLabeler` + `Enforce`
   (`src/pkg/github/stage_labeler.go:63`); the claim relay and ledger hooks
   write `stage/020` / `stage/030` directly as a side effect of a claim
   (`src/cmd/hive/claimswire.go:236-238`, `src/cmd/hive/claimswire.go:176-178`,
   `src/pkg/github/issue_inprogress.go:126-141`, `src/pkg/github/issue_inprogress.go:186`,
   `src/pkg/github/stage_labeler.go:114`); the PR-claim sync writes 040
   (`src/pkg/github/prclaims.go:1777`, `src/pkg/github/prclaims.go:2037`) and so
   does a dashboard verdict-settle handler through a plain `AddLabels`
   (`src/pkg/dashboard/contribute_verdict_settle.go:216`). Agents may add the
   parking stages through the label relay (`agentMayAddLabel`,
   `src/pkg/github/issue_request_label.go:75`), so a clanker can set a stage
   label and only the next sweep's self-heal corrects it. The docs admit the
   arrangement (`src/docs/labels-and-control-signals.md:313-316`). The PR-D
   vocabulary cleanup makes claim labels and review adoption checks use
   `stagelabels` directly; their fact-submission migration remains separate.
3. *No explicit transition model.* `Store.Observe` records that a stage
   changed and whether that counts as a regression, but nothing asserts that
   a from→to pair is one the pipeline expects, so a derivation bug (or a
   second writer) shows up as a label the owner notices, not as a metric.

A concurrent investigation ("review-pending stuck": PRs sitting at 050 after
reviews arrive) shows a fourth cost of the split: "what counts as reviewed"
is computed from `Detail.Verdict`, `HumanApprover`/`HumanApprovalHead`,
`Reviews`/`ReviewsKnown` and the lane reason in several places of
`classifyStage`, so a fix must be re-derived per call site.

## Decision

1. **One engine, `pkg/stageengine`**, owns stage *derivation* for both kinds
   and the only code path that *writes* a `stage/*` label. `pkg/stagelabels`
   stays the vocabulary (Table, colours, Plan, Store); it gains nothing that
   knows about lanes or buckets.
2. **Stage stays derived from GitHub truth every cycle.** The engine is a
   reconciler, not a gatekeeper: each sweep builds `Facts` from the forge,
   derives one `Decision`, and makes the item carry exactly that stage. A
   transition the model does not expect is **asserted, not rejected**: it is
   counted (`stage_transition_anomaly_total{kind,from,to}`), written once to
   the audit log, and the derived stage is still applied. Reconcile-from-truth
   is therefore always available and self-healing is preserved.
3. **Single writer.** Exactly one `StageWriter` implementation performs every
   `stage/*` add/remove. Relay, claim, ledger, PR-claim-sync and dashboard
   paths **submit facts** (`claim`, `in-progress`, `lease`, `covered-by`,
   `unpark`) through `FactSink`; the engine turns them into a reconcile of
   that one item immediately, so the label is still right "as soon as the
   claim lands". Automerge's `engineStageLabeler` is deleted. An admission
   test over `src/` fails the build on any other `stage/*` write.
4. **Shared fact model, kind-specific derivers.** `Facts` carries the
   concepts common to issues and PRs once (park, author kind, adoption,
   terminal, origin, review); `IssueFacts` / `PRFacts` carry the rest.
   `DeriveIssue` and `DerivePR` are two `Deriver` implementations behind one
   interface — not one function.
5. **Rollout invariant: behaviour first, semantics later.** The automerge lane precedence is
   merge-critical. Golden fixtures are captured from the *current* derivation
   before any code moves; the new derivers must reproduce them byte-for-byte
   under `mode: shadow` (compute, diff, log, no writes) with a 0 % mismatch
   rate before `mode: enforce` is switched on. Only then may semantics change
   (each change updates the goldens in its own PR).
6. **"Reviewed" is one fact.** `PRFacts.Review` is the single place where the
   swarm verdict, a trusted human's same-head approval and the review count
   are folded into `ReviewFacts.State` for the head; derivers read only that.
   The review-pending-stuck finding lands as a change to that one function.

## Design

Package: `src/pkg/stageengine` (imports `stagelabels`, `authorkind`,
`claims`; never `github` or `automerge`, which import it — adapters live on
their side). Config: `governor.labels.stage.engine.mode: shadow | enforce`
(short name `stage_engine.mode` in dashboards/logs; default `enforce` since
the plan's acceptance metrics held on hive's own spoke — it was `shadow`
during the rollout. `shadow` is now a pause: derive and count, write
nothing. The rollback value `off` kept the legacy paths and was retired
with them in plan PR-F).

### Facts

```go
package stageengine

// Ref identifies one forge item. Key() equals stagelabels.Key(owner/repo, number)
// so Store records keep their identity.
type Ref struct {
	Source      string // work source: "github" (default when ""); "jira", "linear", "projects" planned
	Owner, Repo string
	Number      int
	Kind        stagelabels.Item // stagelabels.IssueItem | stagelabels.PRItem
}

// Facts is everything a Deriver may look at. Derivers do no forge I/O.
// Fields common to both kinds live here once; kind-specific evidence is the
// one non-nil pointer.
type Facts struct {
	Ref        Ref
	ObservedAt time.Time
	Labels     []string // list payload or fresh read; includes non-hive labels
	Open       bool
	Merged     bool
	Draft      bool
	Author     AuthorFacts
	Park       ParkFacts
	Adoption   AdoptionFacts
	Origin     string // the origin/<source> label, "" for a human filing
	Issue      *IssueFacts
	PR         *PRFacts
}

type AuthorFacts struct {
	Login        string
	Kind         authorkind.Kind // Hive | Clanker | Human | Unknown
	SelfAuthored bool            // this App wrote it (selects 070 over 071/072)
	Role         stagelabels.Role
}

// ParkFacts: why the item is (or is not) waiting on a person.
type ParkFacts struct {
	Holds            []string // stagelabels.Holds(labels), lower-cased
	NeedsHuman       bool
	DecisionLabel    string // configured issue park label present, or 085 on a PR
	Sentinel         bool
	HumanOnlyReason  string // lane rule unrelated to trust (workflow edits, tests removed, …)
	ApprovedDirection bool  // /hive approve|decision recorded; un-park keeps needs-human (#12715)
	DecisionQuestion string
}

type AdoptionFacts struct {
	Adopted bool
	Head    string // adopted head SHA; stale when PR.HeadSHA differs
	InPlace bool
	Live    bool // superseder still open / hive still pushing
	Yielded bool
}

type IssueFacts struct {
	Bucket          IssueBucket // Actionable | Claimed | InProgress | CoveredByPR | Filtered(reason)
	FilteredReason  string
	Claim           *ClaimFact
	LikelyDone      bool
	CoveringPR      *Ref
	CoveringPRStage stagelabels.Stage // from the Store; None when unknown
}

type ClaimFact struct {
	Holder string
	Kind   claims.Kind
	Until  time.Time
	// PRStarted is PR evidence behind the claim: the holder's
	// `hive-open-pr --issues` request named the issue, or its start
	// marker named a branch / PR. Only a claim with it derives 030;
	// any other live claim, an agent's included, derives 020 (#13069).
	PRStarted bool
	PRRepo    string
	PRNumber  int
}

type PRFacts struct {
	HeadSHA          string
	HeadFresh        bool // nothing has reported on the head yet
	BaseRef          string
	Conflicting      bool
	ChangesRequested bool
	Checks           stagelabels.CheckSummary
	DCO              stagelabels.DCOState
	DCOGate          bool // repo enforces DCO on this head
	Review           ReviewFacts
	Soak, SoakElapsed time.Duration
	SharedCIIncident int
	Lanes            []LaneFact // automerge lane opinions, precedence order (self > trusted > contributor)
}

// ReviewFacts is THE "what counts as reviewed" fact (Decision §6).
type ReviewFacts struct {
	State           stagelabels.VerdictState // folded for HeadSHA
	Verdict         string                   // raw swarm verdict, "" when none
	VerdictKnown    bool
	HumanApprover   string // trusted human whose APPROVED review matches HeadSHA
	HumanReviews    int    // review submissions by people on this head
	HumanReviewsKnown bool
}

// LaneFact is one automerge lane's view of the PR this cycle. It is the
// typed form of today's stageObservation + stageDetail; the automerge adapter
// fills it, the PR deriver reads it.
type LaneFact struct {
	Lane      string // "self-authored" | "trusted-author" | <contributor tier>
	Reason    string // lane skip/admission reason (today's reason constants)
	Err       bool
	Merged    bool
	Role      stagelabels.Role
	RedChecks []string // completed-red non-required checks on RedChecksHead
	RedChecksHead string
}
```

### Decision, Deriver, Reconciler, StageWriter

```go
// Decision is one derivation's output: exactly one stage (None = leave the
// pipeline, remove every hive-owned label) and why.
type Decision struct {
	Stage    stagelabels.Stage
	Role     stagelabels.Role
	Kind     authorkind.Kind
	DCO      stagelabels.DCOState
	Stalled  bool
	Terminal bool   // Stage == None because the item closed/merged/left
	Reason   string // stored on the Record, rendered in comments/tracker
	Human    *stagelabels.Route // set when the stage is a hand-off (085/090)
}

// Deriver maps Facts to a Decision. Pure: same Facts → same Decision.
type Deriver interface {
	Kind() stagelabels.Item
	Derive(f Facts) Decision
}

// Two derivers, one interface. Each asserts f.Ref.Kind matches.
func DeriveIssue(f Facts) Decision
func DerivePR(f Facts) Decision

// StageWriter is the ONLY code that adds or removes a stage/* label.
// Implementations wrap stagelabels.Enforce (fresh read, removes before adds,
// kind clamp). The admission test allows exactly one implementation.
type StageWriter interface {
	Apply(ctx context.Context, ref Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error)
}

// Transition is what Reconcile reports to its caller and the audit log.
type Transition struct {
	Ref      Ref
	From, To stagelabels.Stage
	Reason   string
	Anomaly  string // "" when expected; otherwise why the from→to pair is unexpected
	Outcome  stagelabels.Outcome
}

// Reconciler derives, asserts the transition, records it in the Store and —
// in enforce mode — writes it. One instance per process.
type Reconciler interface {
	// Reconcile handles one item. In shadow mode it computes the Decision,
	// diffs it against legacy (via Mode/Shadow below) and writes nothing.
	Reconcile(ctx context.Context, f Facts) (Transition, error)
	// ReconcileBatch is what the sweeps call; it dedupes refs and preserves
	// per-item ordering with concurrent FactSink submissions.
	ReconcileBatch(ctx context.Context, facts []Facts) ([]Transition, error)
}

type Mode string

const (
	ModeShadow  Mode = "shadow"  // derive and count, write nothing
	ModeEnforce Mode = "enforce" // default
)

type Options struct {
	Mode    Mode
	Store   *stagelabels.Store // stagelabels.Shared() by default; API unchanged
	Writer  StageWriter
	Issue   Deriver // defaults to DeriveIssue
	PR      Deriver // defaults to DerivePR
	// (Legacy, the shadow-mode comparator, existed for the rollout and left with plan PR-F.)
	Now     func() time.Time
	Logger  *slog.Logger
	Metrics Metrics
}

func New(o Options) Reconciler
```

### Transition assertion (anomalies, never gates)

```go
// Expected reports whether from→to is a transition the pipeline model
// anticipates. It is consulted AFTER derivation and never changes the
// Decision: an unexpected pair increments
// stage_transition_anomaly_total{kind,from,to} and writes one audit line
// ("stage: unexpected <from>→<to> on <ref>: <reason>"), then the derived
// stage is applied as usual. Reconcile-from-truth therefore always wins.
func Expected(kind stagelabels.Item, from, to stagelabels.Stage) bool
```

The initial table of expected pairs is generated from the mermaid diagram
in `labels-and-control-signals.md` and from the Store's existing
`IsRegression` rules (a non-regression backward hop such as 090 → 072 is
expected). `None → *` and `* → None` are always expected.

### Fact submission (relay, claim, ledger, dashboard)

```go
// FactSink is what out-of-cycle writers call instead of touching labels.
// Each call reconciles that one item immediately (fresh read, single
// writer), so a claim's stage lands as fast as it does today.
type FactSink interface {
	Claimed(ctx context.Context, ref Ref, c ClaimFact) error
	ClaimReleased(ctx context.Context, ref Ref, holder string) error
	InProgress(ctx context.Context, ref Ref, agent string, until time.Time) error
	CoveredBy(ctx context.Context, issue Ref, pr Ref) error
	Uncovered(ctx context.Context, issue Ref, pr Ref) error
	Parked(ctx context.Context, ref Ref, label string) error   // agent escalation (045/085/needs-human)
	Unparked(ctx context.Context, ref Ref, by string) error    // /hive approve|decision
}
```

`Claimed` carries the claim's PR evidence (`ClaimFact.PRStarted`, recorded
on the ledger claim by the start-signal seam when the signal is a PR
request or a branch-naming start marker, and stamped `pr_started=true` on
the `hive:claim` marker so the enumeration reads the same thing); it is the
only way an issue derives `030-pr-in-progress`. `InProgress` — the bare
`hive-open-issue claim` — derives `020-claimed` (amendment, #13069: the
scanner's triage claim on hivecommons/hive#13069 wore 030 with no PR
anywhere).

`Client.ApplyStageLabel`, `AddLabelsFresh(stage, …)` and the automerge
labeler are removed once their callers submit facts. The label relay keeps
letting agents *request* the parking labels (ADR-0019), but the request is
turned into `Parked(...)` — the agent never names a `stage/*` label that the
writer did not derive.

### Metrics

`stage_derivations_total{kind,stage}`, `stage_transition_anomaly_total{kind,from,to}`,
`stage_writer_writes_total{kind,op}`, `stage_duplicate_pills_healed_total{kind}`
(`stage_shadow_mismatch_total{kind,legacy,engine}` existed for the rollout and
was retired with the legacy comparator in plan PR-F). Exposed on the existing
`/metrics` path and summarised on the activity tracker's status strip.

## Scope and extension points

**What the engine owns.** Only the *derived* pipeline family: `stage/*`,
the `author/<kind>` kind label, `role/*`, `dco` / `no-dco` and
`stage/stalled` / `stage/regressed` — exactly the set `stagelabels.Desired`
renders. **Signals it consumes but never writes:** `origin/*`, `agent/*`,
`needs-human`, `approved-direction`, `triage/*`, `good-first-issue`,
`recommend/*`. They reach the engine as `Facts` (labels, `ParkFacts`,
`Origin`) and keep their existing writers; the admission test checks only
the derived family.

**Other work sources (planned extension, out of scope for this epic).**
Jira, Linear and GitHub Projects (the Settings → *Work Source* tab) are
reached by keeping `Derive*`, `Reconciler`, `Facts` and `Decision`
source-agnostic and making the edges per-source: `Ref.Source` names the
source (`""`/`"github"` today), fact adapters are per-source, and each source
gets its own `StageWriter` (Jira status/field, Linear state, Projects status
field) selected by `Ref.Source`. The current epic (#12999) ships only the
GitHub adapters and writer; `Ref.Source` is carried from PR-B so later
sources need no signature change.

**Prior-art park (`stage/017-possible-existing`, #13575).** Added after
the epic: an issue-only stage between 015 and 020 with no dwell. The label is
written by the scheduler's prior-art gate (not by the deriver) and read as an
issue-level hard suppress; `DeriveIssue` reproduces it from the label (or the
enumerator's filtered-reason) the way it reproduces 045 from `needs-spec`,
and the resume sweep removes it on `/hive decision proceed` / `exists`.
Leaving 017 for 010 or closed is never a regression.

## Consequences

- A taxonomy change (new stage, renamed stage, new author kind) touches the
  `Table`, one deriver and its goldens — not 30–80 files.
- Stage labels become trustworthy as *display*: nothing but the writer sets
  them, and an unexpected hop is visible as a metric the same cycle instead of
  as a surprised owner.
- Relay/claim code gets simpler (submit a fact, no label bookkeeping) and
  loses its special-cased fresh-read/journal logic, which moves into the one
  writer.
- The dashboard tracker and hub keep reading `stagelabels.Store` unchanged;
  `Record`, `Hop`, `Transition` and `Shared()` keep their API.
- Costs: one more package to understand; the shadow period adds a derivation
  per item per cycle (CPU only, no extra GitHub calls because Facts are built
  from the reads the sweeps already do).

## Risks and mitigations

| Risk | Mitigation |
|---|---|
| Merge-critical lane precedence regresses | Goldens captured from the current `resolveStage` across all lane/reason/verdict fixtures *before* any move; shadow mode must report 0 mismatches over ≥48 h on hive's own spoke before enforce. |
| Concurrent sweeps / relay vs sweep race write opposite stages | One writer with a per-`Ref` mutex; `ReconcileBatch` and `FactSink` both go through it; race test (`-race`) asserts exactly one stage after interleavings. |
| Rate-limit cost of re-reads | Writer re-reads only when the hint implies a write (today's `Enforce` rule); facts are assembled from list payloads the sweeps already fetch; shadow mode makes zero extra calls. |
| In-flight labels during migration | Enforce mode's first pass is a plain reconcile: whatever the legacy writers left is replaced by the derived stage; no label migration script is needed. Legacy spellings keep healing through `Renames`. |
| Anomaly noise after a Table change | Anomalies are counted and logged once per item per from→to, never block, and the expected-pair table is regenerated from the diagram in the same PR as the Table change. |
| Pause writes after rollout | Set `governor.labels.stage.engine.mode: shadow` to derive and observe without writes. `off` no longer restores legacy derivation; those paths were deleted. A code rollback is a separate deployment action. |

## Alternatives rejected

- **Gating state machine** (reject writes whose from→to is not in a table).
  Breaks self-healing: a wrong label would need a human to unstick it, and
  GitHub truth (a merge, a closed PR, a human removing a label) does not ask
  permission. Anomaly assertion keeps the observability benefit without the
  failure mode.
- **Status quo** (two engines, many writers, heal eventually). Each taxonomy
  change costs 30–80 files and a day of churn; agents can set stage labels;
  stuck states (050 after review) need per-call-site fixes.
- **GitHub Projects fields instead of labels.** Fields are per-project, not
  per-repo; are invisible in issue lists, search and the label relay; need
  GraphQL and a project per managed repo; and the fleet's spokes manage
  repos they do not own projects in. Labels stay the carrier; the engine is
  about who writes them.
- **One `Derive(Facts)` for both kinds.** Issues and PRs share facts but not
  the pipeline shape (010–045 vs 050–100); forcing one function recreates the
  clamp special-cases that `Stage.For` exists to handle.

## Review-state fold (resolved)

`ReviewFacts.State` is the one verdict fact the PR deriver routes on; it is
folded for `HeadSHA` by `verdictStateFrom` in `pkg/github/automerge`:

| Evidence at `HeadSHA` | `State` |
|---|---|
| no verdict recorded yet | `VerdictPending` |
| aggregate verdict approved (merge-eligible) | `VerdictApproved` |
| no reviewer configured for the repo | `VerdictUnavailable` |
| `changes_requested` / `reject` / `rejected` / `request_changes` | `VerdictChangesRequested` |
| `requires_human`, or the reviewer asked for a decision | `VerdictRequiresHuman` |
| anything else | `VerdictPending` |

`HumanApprover` is set only when the recorded human approval is for
`HeadSHA`; `HumanReviews` is the review count the stall rule reads. The raw
verdict string is carried for the reason text (`reviewer-verdict:<verdict>`),
never for routing. The "review-pending stuck at 050 after reviews" case that
prompted the placeholder was fixed in hivecommons/hive#13112 (a
`requires_human` verdict routes 085, not 050), pinned by the PR goldens.

## References

- ADR-0019 (escalation over stalling) — parking stages 045/085 stay the
  agents' escalation move; only the writer changes.
- ADR-0023 (trusted-author automerge) — the trusted-author lane is one
  `LaneFact` source; its merge decision is untouched by this ADR.
- `src/docs/labels-and-control-signals.md` — current stage semantics and the
  state diagram the expected-transition table is generated from.

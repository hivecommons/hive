# Standby contributors: protocol, configuration, state machine, and the suspend rule

Status: **Partly shipped — this is step S0, the design gate, for
[RFC #7629](https://github.com/hivecommons/hive/blob/v6/docs/rfc-7629-standby-contributors.md).
The configuration surface below (phase S2) is in tree, defaulted and
validated, and nothing reads it. Everything else described here is design
only.** Target line: `v6`. Check the phase map and its issues for what has
landed since; this page is the plan and is not rewritten as phases ship.

[RFC #7629](../../../docs/rfc-7629-standby-contributors.md) proposes that a lane
paused for budget offer its queue to approved standby contributors, behind a
per-lane model floor. This page is the design the RFC's own rollout asks for
before any code: the wire messages, the configuration schema, the state machine
and who owns each transition, the matching and suspend rules as pure functions,
answers to the RFC's four open questions, a phase map from which S1 to S8 can be
filed, and the test plan each of those phases must satisfy.

It is a design record, not operator documentation. Read the
[status vocabulary](README.md) at the top of this directory: this page is
**design only**, and it describes behaviour that does not exist.

Two neighbouring documents matter. [RFC #6825](../../../docs/rfc-6825-capability-aware-contributor-task-assignment.md)
owns the T1/T2/T3 capability vocabulary and the offered-configuration inventory;
standby is a consumer of both and settles neither.
[The hub/relay decision boundary](capability-aware-assignment-boundary.md)
records the invariants any capability matching on the contributor path inherits
— above all that self-declared capability is *input, never authority*, and that
an undeterminable capability fails closed. Standby adds nothing to that boundary
and must not weaken it.

## The one property this design exists to protect

The RFC's central worry is not that donated work is bad. It is that an owner
watching a stuck queue will lower the bar until something qualifies, and a lane
that used to mean something stops meaning it. Everything below is arranged so
that lowering the floor is *possible and deliberate*, and so that no surface
Hive renders ever proposes it.

Three mechanisms carry that, and they are the parts of this design that are not
negotiable:

1. **The floor has no write API.** `min_model_capability` is readable
   everywhere and writable only by editing `hive.yaml`. There is no dashboard
   control, no `PUT` field, no chat command. A rule about what the UI *says* can
   be violated by a well-meant copy change; an absent endpoint cannot.
2. **Unknown never qualifies, structurally and then again explicitly.** A
   configuration with no tier mapping resolves to `unknown`, and `unknown`
   compares below every floor. A second, explicit guard rejects it before the
   comparison, and its test fails when that guard is deleted.
3. **"Nobody qualifies" is a terminal, acceptable state.** A paused lane with
   zero qualified standby contributors renders as exactly that and offers no
   next step.

The shape to avoid is already in the tree.
`checkModelAllowed` (`src/pkg/dashboard/contribute_ws.go:3460`) returns
`true` for a relay that reported *no* model whenever
`contribute_reject_unknown_models` is false — an undeterminable value admitted
as permission. That is the fail-open the boundary document names as the
recurring bug class here. Standby matching must not copy it, and must not be
implemented by extending it.

## What already exists, and what is genuinely new

The pieces standby needs are mostly present. Naming them precisely is how the
phase map stays small.

**Present:**

- **The budget pause.** `budgetExhausted` (`src/pkg/governor/governor.go:783`)
  closes the kick gate, and `agentsDueForKick`
  (`src/pkg/governor/governor.go:787`) counts the agents it suppressed and logs
  the count. The signal exists. It is not published: `EvalSnapshot`
  (`src/pkg/governor/governor.go:41`) carries hive-wide queue counts and a
  `BudgetExhausted` flag, but nothing says *which lanes* were suppressed or how
  deep each one's queue is.
- **The lane vocabulary.** `classify.LaneConfig`
  (`src/pkg/classify/classifier.go:28`) and `classifyLane`
  (`src/pkg/classify/classifier.go:249`) route an issue to a lane;
  `filterByLane` (`src/pkg/scheduler/scheduler.go:1862`) is the per-lane
  selection already used to build kicks. Per-lane queue depth is a count over
  work the scheduler already computes.
- **The tier vocabulary.** `RotationConfig.AgentTiers`
  (`src/pkg/config/config.go:1745`) already spells T1/T2/T3 in `hive.yaml`. It
  maps *agent names*, not models — so standby borrows the vocabulary and needs
  its own mapping (below).
- **The relay's configuration report.** `auth_response` already carries
  `cli_backend`, `model`, `reasoning_effort` and, since #7760, `advisor_model`
  and `advisor_reasoning_effort` (`src/pkg/dashboard/contribute_ws.go:289-295`;
  sent at `bin/contributor-relay.js:6992`). Those five fields *are* the
  configuration standby matches on. Nothing new needs to be invented to report
  it.
- **Additive protocol negotiation.** `relay_capabilities` (declared by the
  relay, `RELAY_CAPABILITIES` at `bin/contributor-relay.js:1167`) and
  `serverCapabilities()` (`src/pkg/dashboard/contribute_protocol.go:85`) let a
  new feature be gated on an advertised token rather than a version proxy. Both
  peers already ignore message types they do not know: the hub's read-loop
  `switch msg.Type` has no default case
  (`src/pkg/dashboard/contribute_ws.go:1867-1902`) and the relay's logs and
  continues (`bin/contributor-relay.js:7424`).
- **A consecutive-streak ledger with a reset.** `advanceNoPRStreakLocked`
  (`src/pkg/dashboard/contribute_ledgers.go:173`) over `noPRStreakRecord`
  (`src/pkg/dashboard/contribute_ledgers.go:91`) is the exact shape the suspend
  rule needs, persisted in the contributors directory alongside the cooldown
  and verdict ledgers.
- **Hold gating.** `agentmode.CanCreatePRs`
  (`src/pkg/agentmode/agentmode.go:67`) is the mode ceiling, and the
  `holdByLevel` path (`src/pkg/github/pr_request_watcher.go:443-460`) applies
  the `hold` label server-side from authoritative config, failing the request
  rather than silently shipping an unlabelled PR.
- **The attribution trailer.** `AttributionTrailerPrefix`
  (`src/pkg/github/attribution.go:20`) and `InvocationMeta.pairs`
  (`src/pkg/github/attribution.go:149`) already stamp `— hive: agent=… model=…`
  on contributor PRs. The donated marker is two more pairs, not a new format.
- **A per-contributor owner-managed allow-list.** `AgentRoleGrants`
  (`src/pkg/dashboard/contribute_profiles.go:167`) is the precedent for owner
  grants that never change trust tier.
- **Refusal vocabulary.** The `taskUnavailable*` reason constants
  (`taskUnavailableTokenMintFailed`,
  `src/pkg/dashboard/contribute_select.go:37`) are the model for
  machine-readable standby rejection reasons.

**Genuinely new:** the pause *signal* (reason + per-lane depth), the standby
*mode* on the relay, a *model-configuration → tier* mapping, the matching
function, the donated-task dispatch path, and the outcome ledger with its
suspend rule. That is six things, and the phase map allocates them.

## Relay protocol

Standby is **connection state, not durable state**. Approval is durable and
lives in configuration; availability is a property of a machine that is
switched on right now. A hub restart therefore forgets who was standing by, and
relays re-declare on reconnect. This is the correct semantics, not a
limitation: a hub that remembered standby would offer work to machines that are
off.

Four messages, all additive, every new field `omitempty`, gated on one new
capability token.

### Capability token and version

- New server token `standby_v1` appended to `serverCapabilities()`
  (`src/pkg/dashboard/contribute_protocol.go:85`), advertised on `auth_ok`.
- New relay token `standby_v1` appended to `RELAY_CAPABILITIES`
  (`bin/contributor-relay.js:1167`), declared inside `capabilities`.
- `contributorProtocolVersion` (`src/pkg/dashboard/contribute_protocol.go:16`)
  is **not** bumped. The precedent is `token_refresh_failed`: a new message type
  introduced behind a capability token left the version at 1.2, because the
  gate is the advertised token and both peers already tolerate unknown types.
  A relay that does not advertise `standby_v1` is never sent `standby_state`,
  and a hub that does not advertise it answers a `standby_declare` with nothing
  — which the relay treats as "this hive has no standby" after its ack timeout.

### `standby_declare` (relay → hub)

```jsonc
{
  "type": "standby_declare",
  "seq": 42,
  "cli_backend": "claude",          // same five fields, same spelling,
  "model": "claude-opus-5",         // as auth_response — one parser
  "reasoning_effort": "high",
  "advisor_model": "…",             // omitted when the CLI runs no advisor
  "advisor_reasoning_effort": "…",
  "standby": {
    "lanes": ["quality"],           // omitted/empty = every standby lane the hive offers
    "max_concurrent": 1,            // the contributor's OWN limits, enforced locally
    "daily_cap": 2
  }
}
```

Sent immediately after `auth_ok` when the relay is started in standby mode, and
re-sent on every reconnect. The configuration fields are re-reported rather than
inherited so that a relay whose CLI restarted on a different model refreshes
what the hub matches on; the hub recomputes the tier on every refresh, and a
refresh that drops the configuration below the floor ends the qualification
immediately and is surfaced, never ignored.

The relay does not need to know the hive's lane names in advance. Omitting
`lanes` means "any lane this hive offers standby on", and the acknowledgement
names what was actually recorded.

### `standby_ack` (hub → relay)

```jsonc
{
  "type": "standby_ack",
  "seq": 42,
  "standby": {
    "accepted": [
      { "lane": "quality", "floor": "T2", "tier": "T2", "daily_cap_remaining": 3 }
    ],
    "rejected": [
      { "lane": "scanner", "reason": "standby_disabled" },
      { "lane": "ci-maintainer", "reason": "below_floor" }
    ]
  }
}
```

Rejection reasons, in the `taskUnavailable*` style: `lane_unknown`,
`standby_disabled`, `not_approved`, `below_floor`, `configuration_unknown`,
`suspended`, `cap_exhausted`.

**A rejection states the reason class and never the delta.** `below_floor`
carries no `floor` value and no "you would qualify at T3". An accepted entry
*does* carry the floor, because a contributor who is in now needs to know what
they are held to. The asymmetry is deliberate: honest refusal, no lobbying
material. The anti-nudge rule binds the contributor-facing surface as well as
the owner's.

### `standby_release` (relay → hub)

```jsonc
{ "type": "standby_release", "seq": 43, "standby": { "lanes": ["quality"] } }
```

Omitted `lanes` releases everything. Closing the socket releases implicitly —
`standby_release` exists so a relay can stay connected for ordinary contribute
work while stepping out of standby.

### `standby_state` (hub → relay)

Sent **only to connections currently in standby**, when a lane they are recorded
against changes pause state, queue depth, or their own qualification:

```jsonc
{
  "type": "standby_state",
  "standby": {
    "lanes": [
      { "lane": "quality", "paused": true, "reason": "budget_exhausted",
        "queue_depth": 6, "qualified": true, "daily_cap_remaining": 2 }
    ]
  }
}
```

There is deliberately **no broadcast to every relay**. A relay that never
declared standby sees no new traffic at all, which is what makes "an older relay
is unaffected" a property of the design rather than a hope.

### Mixed-version matrix

| Relay declares `standby_v1` | Hub advertises `standby_v1` | Behaviour |
| --- | --- | --- |
| no | no | Today's behaviour, byte for byte. |
| no | yes | Relay never declares; hub never counts it as standby; no new frames on the wire. |
| yes | no | `standby_declare` is ignored (no default case in the hub switch); the relay's ack timeout resolves to "no standby here" and it falls back to ordinary contribute work. |
| yes | yes | Standby. |

## Configuration schema

Two places, matching where each kind of setting already lives: per-lane
behaviour under the agent, hive-wide allow-lists under `hub:` next to
`contribute_allow_models` (`ContributeAllowModels`, `src/pkg/config/config.go:4110`) and
`contribute_delegatable_roles` (`ContributeDelegatableRoles`, `src/pkg/config/config.go:4201`).

```yaml
agents:
  quality:
    standby:
      enabled: false                 # default
      min_model_capability: T1       # default T1
      daily_cap_per_contributor: 0   # default 0 — nothing dispatches

hub:
  standby_contributors: [alice, bob] # GitHub logins; approval is not volunteering
  standby_model_tiers:               # owner-authored; NO shipped defaults
    - { backend: claude, model: claude-opus-5, reasoning_effort: high, tier: T1 }
    - { backend: codex,  model: gpt-5.6-terra, reasoning_effort: high, tier: T2 }
  standby_item_tiers:                # S7; ships EMPTY, and empty is not in force
    - { repo: my-org/repo-a, label: dependencies, tier: T3, signal: "the lockfile diff plus green CI" }
    - { label: kind/security, tier: T1 }
```

`standby` is a new block on `AgentConfig` (`src/pkg/config/config.go:974`); the
three `hub.standby_*` keys are new fields on `HubConfig`. Both landed in S2:
`AgentConfig.Standby` (`src/pkg/config/config.go:1140`) and
`HubConfig.StandbyContributors` (`src/pkg/config/config.go:4211`) with its two
neighbours. The types, defaults and validation are `pkg/config/standby.go`.

### Validation rules

Each is a unit test in `pkg/config`:

1. `min_model_capability` absent → `T1`. The default is the strongest floor, so
   a hive that adopts the block without thinking about it gets the safe answer.
2. `min_model_capability` must be exactly one of `T1`, `T2`, `T3`.
   **`unknown` is rejected at parse**, with the error naming the field — it is
   not a floor, it is the absence of one, and accepting it would create a lane
   that anything clears.
3. `enabled: true` with an empty `hub.standby_contributors` is a **load error**,
   not a warning. A lane that is on with nobody approved is a misconfiguration
   the operator should see at boot, not a silently inert setting.
4. `daily_cap_per_contributor` absent → `0`, and `0` means *nothing is
   dispatched*. This is what makes S2's configuration safe to adopt before S5
   exists: a hive can enable standby, see counts, and dispatch nothing. Negative
   is a load error; values above a clamp ceiling are clamped with a logged
   warning, following `ContributeCooldownHours`.
5. `standby_model_tiers` ships **empty**. An unmapped configuration is `unknown`
   and never qualifies, so the out-of-the-box outcome on every hive is
   "0 qualify" until an owner writes the mapping deliberately. Each entry's
   `tier` must be `T1`/`T2`/`T3`; duplicate tuples are a load error rather than
   a last-one-wins.
6. `standby_contributors` entries are GitHub logins, sanitised and bounded the
   way the other hub lists are. A login here grants *nothing else*: not a trust
   tier, not a role, not a credential.

### The rule that nothing suggests lowering the floor

- `min_model_capability` is exposed **read-only** on every surface. The
  governor's configuration `PUT` (`src/pkg/dashboard/api_governor.go`) — which
  today accepts `contribute_allow_models` and friends — must not gain a standby
  floor field, and a test asserts that no route writes it.
- The tile renders facts and stops: *"quality — paused, out of budget. 6
  waiting, 0 qualify."* No comparative phrasing ("2 would qualify at T3"), no
  control, no link to configuration, no empty-state call to action.
- `standby_ack` rejections carry no floor value (above).
- The same rules bind the non-dashboard surfaces. The v6 line's
  [guard invariant](../../../ROADMAP.md#v6--dashboard-optional-operation-line-open)
  is that every surface routes through the same authorization and safety
  machinery and none grows its own;
  the anti-nudge rule is part of that machinery, so a chat command that renders
  the tile renders it under the same constraint.

## State machine

Two machines that touch: one per **lane**, one per **(contributor,
configuration)** pair.

### Lane

| From | To | Trigger | Owner |
| --- | --- | --- | --- |
| `running` | `paused_budget` | Budget window exhausted; the lane's kick is suppressed | **Governor** (`budgetExhausted`, `agentsDueForKick`) |
| `paused_budget` | `paused_budget` + published | Pause reason and per-lane queue depth recorded on the eval snapshot and served | **Governor** publishes; **scheduler** supplies depth; **dashboard** serves (S1) |
| `paused_budget` | `standby_offered` | ≥1 approved, unsuspended, qualified standby connection with cap remaining | **Hub** (S4) |
| `standby_offered` | `paused_budget` | Last qualified connection drops, is suspended, or exhausts its cap | **Hub** |
| `standby_offered` | `dispatched` | Owner presses dispatch for an item or the lane | **Dashboard/chat** authorizes, **hub** executes (S5) |
| any | `running` | Budget window rolls, spend drops, or the lane is made budget-exempt | **Governor** |

A lane whose mode cannot open PRs never reaches `standby_offered` — see the
ceiling rule below.

### Contributor configuration, per hive

| From | To | Trigger | Owner |
| --- | --- | --- | --- |
| `unapproved` | `approved` | Login added to `hub.standby_contributors` | **Owner**, config edit |
| `approved` | `standing_by` | `standby_declare` on a live connection | **Relay** declares, **hub** records |
| `standing_by` | `qualified` | Tier clears the lane floor, cap remaining, not suspended | **Hub**, pure matching |
| `qualified` | `dispatched` | A donated task is assigned; the daily cap decrements **at dispatch** | **Hub** |
| `dispatched` | `pr_open_held` | The relay's agent opens a PR from the contributor's account; the hub verifies it and applies `hold` | **Relay** opens, **hub** verifies and labels |
| `pr_open_held` | `outcome_recorded` | The PR merges, closes unmerged, or merges after human rework | **Hub**, from the GitHub watcher |
| `outcome_recorded` | `suspended` | N consecutive closed-unmerged outcomes | **Hub**, pure function over the ledger |
| `suspended` | `approved` | Owner clears | **Owner** via dashboard or chat; **hub** appends the clear |
| any | `standing_by`/gone | Socket closes | **Relay**/transport |

Note the split at `pr_open_held`. The contributor's agent opens the PR with the
contributor's own scoped credential, so the PR does not travel the
`pr_request_watcher` path that applies `hold` today
(`src/pkg/github/pr_request_watcher.go:443-460`). The hub verifies the reported
PR already (`verifyReportedPR`, `src/pkg/dashboard/contribute_ws.go:1141`) and
must apply the label itself on that path. **This is the first hub-side GitHub
label write on the contributor path** — the `verdict_blocked` design
deliberately left labelling to the relay — and the change is intentional: the
hold is an enforcement gate, and a gate the client applies is not a gate. The
prompt also instructs the agent to open the PR held, but that is belt, not
enforcement.

Failure handling copies `holdByLevel`: if the label cannot be applied, the
completion is recorded as failed-to-gate and retried, never settled as success.

### The mode ceiling

The RFC's rule is that a donated agent gets no more than the owner's own agents
had: same policy, same repos, same mode ceiling, minus merge. Applied to Hive's
mode ladder (`src/docs/acmm-policy-matrix.md`) that has a consequence worth
stating plainly:

- **Holdgated lane** — donated PR is held, exactly as the lane's own PRs are.
- **Full lane** — donated PR is held anyway. Standby subtracts merge; it never
  adds it.
- **Advisory or measured lane** — the lane has no authority to open PRs at all
  (`CanCreatePRs`, `src/pkg/agentmode/agentmode.go:67`). Standby dispatch on
  such a lane is **refused**, with reason `mode_ceiling`. Donating cannot raise
  a lane's ceiling, so there is no PR to hold and nothing to dispatch.

So "a donated task yields a hold-gated PR at every ACMM level" holds for every
level where the lane may open a PR, and at the levels where it may not, the
dispatch does not happen. Both halves are tested.

## Matching

A new package `pkg/standby`, pure: no I/O, no clock except an injected one, no
hub types. The hub reads state, calls it, and renders the result.

```go
// Tier is the RFC #6825 capability vocabulary. Unknown is the zero value on
// purpose: a Tier nobody set is the one that never qualifies.
type Tier string // "", "T1", "T2", "T3"; "" and "unknown" both mean unknown

// Configuration is the WHOLE thing that is matched, never the model alone.
type Configuration struct {
    Backend, Model, ReasoningEffort, AdvisorModel, AdvisorEffort string
}

type Candidate struct {
    Contributor string
    Config      Configuration
    Approved    bool
    Suspended   bool
    Dispatches  []time.Time // this contributor's dispatches on this lane
}

type LanePolicy struct {
    Floor    Tier
    DailyCap int
}

// Qualifies is the whole matching decision for one candidate on one lane.
// Reason is always set, including on success, so every tile cell and every
// standby_ack entry has a machine-readable cause.
// As built in S7, `item` is an ItemMatch — the owner's list's answer about
// the work — whose zero value means item-tier matching is not in force.
func Qualifies(c Candidate, p LanePolicy, item ItemMatch, tiers TierMap, now time.Time) (bool, Reason)
```

**Rules:**

- **Strength ordering.** `T1 > T2 > T3 > unknown`. A floor of `T2` admits `T1`
  and `T2`. Encoded as a strength score where `unknown` is zero and every legal
  floor is at least one, so `unknown` fails the comparison mechanically — *and*
  a preceding explicit `if tier is unknown → reject` guard states the invariant
  in one place. The test for that guard asserts the behaviour, so it fails when
  the guard is removed rather than passing on the encoding's coincidence
  (CONTRIBUTING, "Test policy").
- **The whole configuration is matched.** `TierMap` is keyed on the full
  `Configuration` tuple. Any component that is absent, or any tuple with no
  entry, yields `unknown`. This is what makes "nobody can offer Opus and run
  something else" structural rather than a promise: changing the reasoning
  effort changes the key, and a key with no mapping does not qualify. The
  configuration re-reported on `task_progress` is re-checked; a mid-task change
  is recorded and the outcome is attributed to the configuration that actually
  ran.
- **Item tier (S7).** `item == ""` (unclassified) is `unknown`, and `unknown`
  item tier means the item is not standby-eligible at all. The lane floor and
  the item tier are *both* required: a T1 configuration may take a T3 item, a T3
  configuration may not take a T1 item. As built, "unclassified" is an item that
  matches no entry of the owner's list — and an *empty* list is a separate
  state, "not in force", in which there is no item bar at all and the decision
  is S4's exactly. Without that distinction, shipping the list empty would
  disqualify everybody rather than change nothing.
- **Daily cap.** Counted as dispatches inside a trailing 24-hour window, exactly
  like `tier_limits.max_per_day` (`rateLimitDayWindow`,
  `src/pkg/dashboard/contribute_select.go:22-25`), not a calendar bucket — same
  reason: a calendar reset lets a burst straddle the boundary. The cap
  decrements **at dispatch**, not at completion, so an abandoned donated task
  still costs a slot. The contributor's own `standby.daily_cap` is enforced
  locally by the relay *in addition*; the effective cap is the minimum, and
  neither side trusts the other's.
- **Nobody qualifies.** `Qualifies` returning false for every candidate is a
  normal outcome that leaves the lane paused. It produces a count of zero and no
  other effect.

## Suspend rule

**N consecutive closed-unmerged donated PRs (default 2) suspends that
configuration for that hive until the owner clears it.**

### Where the outcome is recorded

A new ledger file in the contributors directory (`HIVE_CONTRIBUTORS_DIR`)
alongside `noPRStreaks`, the completed-task ledger and the no-work verdicts, so
it survives a hub restart on the same PVC and is pruned on the same schedule.
One append-only row per donated PR:

```jsonc
{ "key": "alice|claude|claude-opus-5|high|",   // contributor + configuration tuple
  "lane": "quality", "repo": "org/x", "number": 41,
  "dispatched_at": "…", "outcome": "closed_unmerged", "outcome_at": "…",
  "human_reworked": false }
```

Keyed on the **configuration**, not the person: the RFC suspends a
configuration, and the same contributor on a different model is a different
donor.

Outcomes: `open`, `merged`, `closed_unmerged`, `merged_after_rework`, and
`cleared` (the owner's clear, written as a row so the ledger stays an audit
record rather than being edited).

### The rule as a pure function

```go
// SuspendState walks rows newest-first and stops at the first row that settles
// the question. Rows that count as neither are skipped, not counted.
func SuspendState(rows []Outcome, threshold int) (suspended bool, streak int)
```

- `closed_unmerged` → increments the streak.
- `merged` → resets to zero and stops. A merge in between breaks the streak.
- `merged_after_rework` → **counts as neither**; skipped, streak preserved.
- `cleared` → resets to zero and stops.
- `open` → skipped (not yet an outcome).
- `suspended` when `streak >= threshold`; `threshold` defaults to 2.

The function is total, takes no clock, and is tested as a table. The default of
2 is a hive-wide setting, not per lane: it is a statement about a donor, and a
donor is not a lane.

### Detecting "reworked by a human first"

The distinction matters — a PR a maintainer fixed and merged is not evidence the
configuration produces mergeable work, and it is not evidence it produces
garbage either. Hive already has the primitive: `holdguard.Snapshot`
(`src/pkg/holdguard/holdguard.go:148`) records a PR's head SHA and the set of
its commit **authors** (`commitSets`, `src/pkg/holdguard/holdguard.go:292`), and
`holdguard.Diff` (`src/pkg/holdguard/holdguard.go:314`) reports the drift.

So: when the hub applies the `hold` to a donated PR, it snapshots the PR through
holdguard. At outcome time, a merged PR whose author set has grown beyond the
donating contributor is `merged_after_rework`; one that merged with the
contributor as sole author is `merged`. This reuses a mechanism whose job is
already "who touched this after we recorded it", rather than inventing a second
answer to the same question.

Two limits, recorded rather than papered over: a maintainer who rewrites history
before merging can collapse the author set, and a maintainer who fixes the work
in a *separate* follow-up PR is invisible to this. Both bias toward `merged`
(generous to the donor), which is the safer direction for a rule whose effect is
suspension.

### Clearing

The owner clears a suspension from the dashboard or any v6 surface, through the
same authorization as the dispatch control (owner / read-write role floor). The
clear appends a `cleared` row; nothing is deleted. A cleared configuration
returns to `approved` and must re-declare standby to stand by again.

## Answers to the RFC's open questions

These are recommendations for maintainers to accept or reject. They are written
as answers because the acceptance criterion for S0 is that each open question
has one written answer, not a survey.

### 1. Per lane or per repo for the floor? — **Per lane.**

The floor's job is to protect *reviewer trust in a lane*, and a lane is where
policy is already written: `agents.<name>` in `hive.yaml`, `LaneConfig` in the
classifier, `filterByLane` in the scheduler. A per-repo floor would need a new
scoping concept and would still have to answer "which floor applies when a lane
serves five repos".

The cost is real and worth naming: a hive whose `quality` lane serves several
repos cannot raise the bar for one of them alone. The escape hatch already
exists — `agents.<name>.repos` makes a lane a per-repo specialist
(`src/docs/per-repo-agents.md`), and an operator who needs a stricter floor for
one repo raises it on a lane scoped to that repo. That is a configuration the
hive already supports and already enforces at the proxy, rather than a new axis.

### 2. Do donated PRs count against the hive's PR budget and cadence? — **Against cadence and queue accounting, yes. Against the token budget, no.**

Split the question, because "budget" means two different things in this repo.

*Cadence and queue accounting: yes, and nothing should exempt them.* A donated
PR is an ordinary contributor PR. It is already seen by the open-PR claim ledger
(`issueClaimedByOpenPR`), the duplicate sweep, and the governor's `OpenPRs` and
queue-depth counts. Leaving that alone is precisely what stops a generous donor
flooding a repo, which is the concern the RFC raises. The implementation note is
therefore "add no exemption", not "add a counter".

*Token budget: no.* `governor.budget` meters the hive's own model spend. No hive
tokens were spent on a donated task. Counting it there would deepen the very
exhaustion that opened standby in the first place — the lane would be re-paused
for spend that did not happen. A donated PR must be visible in spend reporting
as donated (attributable through the trailer's `standby_lane` pair) and must
contribute zero to the budget gate.

The live-hive evidence for the first half is the last step of the runbook below:
merge one donated PR and confirm it counted against the hive's PR cadence.

### 3. Private repos, and what the approval screen says. — **Opt-in, default off, and the screen states the read-access equivalence verbatim.**

A standby contributor receives task context — issue title, body, labels, the
lane's policy text, and a scoped credential for the work. For a private repo
that is read access in substance.

Two parts:

- **Mechanical.** Standby dispatch is refused for a repository GitHub reports as
  private unless the hive sets `hub.standby_allow_private_repos: true`
  (default `false`). The refusal reason is `private_repo`. This is a distinct
  gate from the existing `disabled_repos` curation, and it fails closed when the
  repository's visibility cannot be read.
- **Copy.** The approval control states, without softening: *"An approved
  standby contributor receives the full task context for any repository the lane
  serves — including private ones if you have enabled that. Approve only people
  you would give read access to."* The same sentence appears wherever approval
  is granted, dashboard or otherwise.

### 4. Is hold-gated review of a T3 donated PR cheaper than doing it by hand, and how narrow is the T3 list? — **Often not, so the list is defined by verifiability and ships empty.**

The honest answer to the RFC's own example is no: reviewing a donated changelog
entry costs more than writing one.

The useful reframing is that the break-even is not *size*. It is whether the
reviewer must **re-derive the work to check it**. A one-line change whose
correctness requires understanding the surrounding subsystem is expensive to
review; a larger change whose correctness is asserted by a signal the reviewer
can read in seconds is cheap. So the T3 eligibility test is:

> An item is T3-eligible only if its correctness is established by an automated
> signal a reviewer can read without reconstructing the change.

Under that test:

- The shipped list (`hub.standby_item_tiers`, as built in S7) is **empty**, and
  an empty list is not in force at all, so S7 changes nothing until an owner
  opts in. The test above is enforced rather than advisory: a `T3` entry that
  names no `signal` fails the load.
- Three candidates a maintainer can start from, each with its signal named: a
  dependency bump (the lockfile diff plus green CI), a broken-link fix (the link
  checker), and adding a test for an already-specified pure function (the test
  fails on the parent commit and passes on the change — the repository's own
  bug-fix test standard, applied as the review signal).
- Explicitly *not* T3: anything whose acceptance rests on taste, naming, prose
  quality, or "does this belong here" — including changelog entries and doc
  rewrites, which look small and review expensively.

The evidence that revisits this is the S5 runbook: record reviewer minutes per
donated PR against a comparable item done by hand. If the ratio does not favour
donation for the candidates above, the right conclusion is that standby is a
T1/T2 mechanism and the T3 list stays empty permanently.

## Phase map

S1 to S4 are independent once S0 merges. S5 onward waits for S4 to be live
somewhere. Each row names the files it touches so the issue can be filed from
this table.

### S1 — Publish the pause reason and per-lane queue depth

`src/pkg/governor/governor.go` (`EvalSnapshot` gains the suppressed-lane set and
per-lane depth; `agentsDueForKick` already computes the suppression) ·
`src/pkg/scheduler/scheduler.go` (per-lane depth over `filterByLane`) ·
`src/pkg/dashboard/api.go`, `src/pkg/dashboard/server.go` (status payload) ·
`dashboard/index.html` (the `agent-card` paused state) ·
`src/pkg/hivectl/`, `src/pkg/tui/` (same fields on the non-dashboard surfaces,
per the v6 guard invariant). Nothing dispatches.

### S2 — Configuration and the approved list, validation only

`src/pkg/config/config.go` (`AgentConfig.Standby`; `HubConfig.StandbyContributors`,
`StandbyModelTiers`, `StandbyAllowPrivateRepos`) · the config `Normalize`/
validation path · `src/hive.yaml.example` · `src/docs/agent-configuration.md`.
No behaviour reads the block yet.

### S3 — Relay standby mode; the hub records it

`bin/contributor-relay.js` (standby mode, `standby_declare` after `auth_ok`,
re-declare on reconnect, local cap enforcement, `standby_v1` in
`RELAY_CAPABILITIES`) · `src/pkg/dashboard/contribute_protocol.go` (`standby_v1`
in `serverCapabilities()`) · `src/pkg/dashboard/contribute_ws.go` (`WSMessage`
standby fields, `case "standby_declare"`/`"standby_release"`, per-connection
standby state) · `Justfile` (a `contribute-standby` recipe) ·
`src/docs/contributor-relay.md`.

### S4 — Matching; qualified count on the tile

`src/pkg/standby/` (new: `Tier`, `Configuration`, `TierMap`, `Qualifies`, cap
accounting — all pure) · `src/pkg/dashboard/contribute_ws.go` (count per paused
lane, `standby_state` emission) · `src/pkg/dashboard/api.go`,
`dashboard/index.html` (the "N waiting, M qualify" cell) ·
`src/pkg/dashboard/api_governor.go` (assert: no floor write). RFC rollout step 1
is complete here.

### S5 — Manual dispatch, packaging, marker, hold

`src/pkg/dashboard/api_contribute.go` (authenticated dispatch endpoint, owner /
read-write role floor) · `src/pkg/dashboard/contribute_task_prompt.go` (the
lane's policy text in the packaged task — and nothing the owner's own agents
would not get) · `src/pkg/dashboard/contribute_ws.go` (donated-task marker on
the assignment; apply `hold` after `verifyReportedPR`) ·
`src/pkg/github/attribution.go` (`standby_lane` and `standby_tier` pairs in the
existing trailer) · `src/pkg/chat/` (the dispatch control on a non-dashboard
surface). RFC rollout step 2 begins here.

### S6 — Outcome tracking, suspend-after-N, owner clear

`src/pkg/standby/suspend.go` (the pure ledger function) ·
`src/pkg/dashboard/contribute_ledgers.go` (the new persisted ledger) ·
`src/pkg/holdguard/` (reused for the rework detection; no change expected) ·
`src/pkg/dashboard/api_contribute.go` (owner clear) · `dashboard/index.html`
(suspension shown on the tile).

### S7 — Item-tier matching and the owner-editable T3 list

`src/pkg/config/config.go` (`hub.standby_item_tiers`, empty default) ·
`src/pkg/standby/` (item tier threaded into `Qualifies`) ·
`src/pkg/classify/classifier.go` (reuse label routing to propose an item's tier
candidate; the owner's list is authoritative).

**As built.** The list is keyed on `(repo, label)`, with an empty `repo`
applying an entry to every repository the lane serves, and a `T3` entry must
name the `signal` that makes it cheap to review — the eligibility test below,
as a load error rather than as prose. `standby.ItemTiers`
(`src/pkg/standby/item.go`) resolves an item against the list; overlapping
entries resolve to the STRONGEST, so scoping can only narrow eligibility.
`Qualifies` takes the resolved `ItemMatch`, whose zero value means item-tier
matching is not in force — which is how an empty list reproduces the S4 answer
exactly. `ItemTiers.Match` is the single place the classifier's proposal
(`classify.ProposeStandbyItemTier`, labels only — the title is not evidence)
meets the list, and it is where the proposal stops: it is carried for logging
and decides nothing. The no-writer guard that protects the floor was widened to
cover the list's key (`TestStandbyItemTiersHaveNoWriterOutsideConfig`), so no
surface may name it, let alone write it. The tile's "M qualify" counts
contributors who could be offered at least one item queued on the lane, from
the same per-lane split `buildLaneQueueDepths` uses for the N beside it.

### S8 — Automatic dispatch, opt-in per lane

`src/pkg/config/standby.go` (`standby.auto_dispatch`, default `false`) ·
`src/pkg/dashboard/contribute_standby_dispatch.go` (the automatic path reuses
`DispatchStandby`, and therefore reuses `Qualifies`, the cap, the floor and the
hold unchanged). Gated on S5 evidence from a real-hive runbook, per the RFC.

**As built.** `auto_dispatch` is a per-lane standby flag and remains off unless
an owner edits `hive.yaml`. When the governor publishes a lane as paused for
budget, Hive attempts automatic dispatch only for lanes with both `enabled` and
`auto_dispatch` set. The automatic path calls the manual standby dispatcher with
no contributor key, so zero qualified contributors, exhausted caps, below-floor
configurations, suspended contributors, ACMM mode ceilings and hold-gated PR
completion all behave exactly as the manual path does.

## Test plan

The doc ships with the test plan; each phase cites the parts it must add.

### Unit, in `pkg/`

- **Config parsing and validation** (S2) — `min_model_capability` defaults to
  T1; `unknown` is rejected at parse; `enabled: true` with an empty approved
  list is a load error; `daily_cap_per_contributor` defaults to 0 and negatives
  are rejected; `standby_model_tiers` is empty by default and duplicate tuples
  are an error.
- **Matching as a pure function** (S4, extended in S7) — lane floor versus item
  tier across the full T1/T2/T3 grid; `unknown` never qualifies (the test fails
  when the explicit guard is removed); whole-configuration comparison (same
  model, different reasoning effort → different tier key → no match); daily cap
  decrement and the trailing-window expiry, with an injected clock; and the
  "nobody qualifies, the lane stays paused" case asserted as a normal outcome
  with no side effect.
- **The suspend rule as a pure function over an outcome ledger** (S6) — two
  closed-unmerged in a row suspends; a merge in between resets; the owner clear
  reinstates; `merged_after_rework` counts as neither and preserves the streak;
  an empty ledger is not suspended.
- **Hold gating** (S5) — a donated task yields a hold-gated PR at every ACMM
  level that permits PRs at all, and dispatch is refused with `mode_ceiling` at
  the levels that do not. A failure to apply the label fails the completion
  rather than settling it.

### Protocol and UI, with the existing fakes

*Hub side, in the `contribute_ws` harness (`contribute_ws_*_test.go`):*

- standby declare and acknowledge, including every rejection reason;
- an old relay that sends no standby fields is unaffected — same frames, same
  behaviour, byte for byte;
- a revoked lease cannot complete a donated task (the `TaskGen` fence applies
  unchanged);
- the pause signal carries reason and queue depth;
- a rejection's `standby_ack` entry carries no floor value.

*Relay side, in the `bin/contributor-relay.test.js` pattern with a fake hub:*

- standby survives a hub restart — the relay re-declares on reconnect and is
  back in the pool (standby is connection state; this asserts the recovery, not
  persistence);
- the relay reports the configuration actually running, and a CLI restart on a
  different model re-reports it;
- the relay enforces the contributor's own cap locally, independent of the hub's.

*Dashboard:*

- the tile renders "paused, out of budget, N waiting, M qualify";
- it never renders a lower-the-floor nudge — asserted against the rendered
  output for the `M == 0` case;
- no route writes `min_model_capability`;
- there is no dispatch control before S5.

*Task packaging (S5):*

- the donated task carries the lane policy and nothing the owner's own agents
  would not get — asserted as a diff against the ordinary contributor prompt
  for the same item.

### End to end on a real hive (runbook)

Recorded here so it can be run as written. A self-hosted hub and one contributor
container; no CI hardware.

1. **(S1) Force the pause.** Set a lane budget already spent. Confirm the tile
   shows the lane paused, the reason `out of budget`, and the queue depth.
2. **(S4) Approve and fail to qualify.** Approve the container as a standby
   contributor with a T2 configuration against the default T1 floor. Confirm the
   tile reads "0 qualify", that the lane stays paused, and that nothing on any
   surface proposes lowering the floor.
3. **(S5) Lower the floor deliberately and dispatch.** Edit `hive.yaml` to set
   the floor to T2. Confirm "1 qualifies". Dispatch one T3 item by hand. Confirm
   the PR arrives from the *contributor's* account, carries the `hold` label,
   and carries the lane and tier markers in its `— hive:` trailer.
4. **(S6) Suspend and clear.** Close two donated PRs unmerged in a row. Confirm
   the configuration is suspended, that the tile says so, and that the relay's
   next `standby_declare` is rejected with `suspended`. Clear it as owner and
   confirm it returns to standby.
5. **(Open question 2) Cadence evidence.** Merge one donated PR and confirm it
   counted against the hive's PR cadence and queue accounting — and that it
   contributed nothing to the token budget.

Record the reviewer minutes spent on step 3's PR. That number is the evidence
for open question 4.

### CI

One standby scenario added to the **keyless stub tier** of
`bin/test_backend_smoke.sh`, which the v2-ci lane runs on every PR
(`src/docs/backend-smoke.md`). That tier already drives the **real relay against
a real hub** with a stub CLI, so the standby handshake is exercised on every PR
and not only in fakes: declare standby, assert the ack, dispatch one item,
assert the donated marker and the relay's local cap refusal on the second.

No new CI hardware and no model spend — the stub tier is keyless by
construction. The live-hive runbook above stays a human's job.

## What this design does not decide

- **The tier evidence source.** `standby_model_tiers` is owner-authored with no
  shipped defaults precisely so this design does not pre-empt RFC #6825's
  unresolved benchmark/evidence question. If #6825 lands a published mapping,
  standby consumes it; until then, "unmapped is unknown is not qualified" is the
  safe default and the out-of-the-box behaviour.
- **Automatic dispatch policy.** S8 exists in the phase map and is deliberately
  last, opt-in, and conditioned on evidence from S5. As shipped, the policy is
  per-lane `standby.auto_dispatch: true`; the default is still false.
- **Cross-hive standby.** A contributor standing by for several hives is
  multiple independent relationships here; federation of standby state is out of
  scope.
- **Anything about credentials.** Nothing in this design moves a token, a key,
  or a credential between machines. The relay's existing rule is unchanged and
  is not restated as a feature.

## References

- RFC: [#7629](https://github.com/hivecommons/hive/issues/7629), committed at
  [`docs/rfc-7629-standby-contributors.md`](../../../docs/rfc-7629-standby-contributors.md).
- S0 tracking issue: [#8034](https://github.com/hivecommons/hive/issues/8034).
- Capability vocabulary: [RFC #6825](../../../docs/rfc-6825-capability-aware-contributor-task-assignment.md)
  and [the hub/relay decision boundary](capability-aware-assignment-boundary.md).
- Capacity context: [RFC #5698](https://github.com/hivecommons/hive/blob/v5/docs/rfc-5698-backend-capacity-model-inventory-placement.md).
- Contributor protocol: `src/pkg/dashboard/contribute_protocol.go`,
  `src/pkg/dashboard/contribute_ws.go`, `bin/contributor-relay.js`.
- Mode ladder and hold gating: [`src/docs/acmm-policy-matrix.md`](../acmm-policy-matrix.md),
  `src/pkg/agentmode/agentmode.go`, `src/pkg/github/pr_request_watcher.go`.
- CI surface: [`src/docs/backend-smoke.md`](../backend-smoke.md).

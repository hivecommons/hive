# Planning Intelligence

**Planning intelligence** turns a big, vague GitHub issue into an ordered,
reviewable plan of small sub-tasks — and gates that plan behind a human approval
before any agent starts work. It is how Hive handles work that is too large for a
single agent kick: instead of one agent thrashing on an epic, the **architect**
lane decomposes it into a DAG of child beads, a human reviews the plan, and only
then are the children released to the fleet.

The feature has four moving parts:

1. **Decompose** — the architect breaks an epic into ordered, dependency-linked,
   execution-tagged sub-tasks (child beads).
2. **Plan-review gate** — a decomposed plan starts as a **draft**; its children
   are withheld from `Ready()` (no agent can claim them) until a human approves.
3. **Stall-replan** — if an approved plan stops progressing, the governor
   re-kicks the architect to revise the unfinished work, bounded by a replan cap.
4. **The issue entry point** (this page's focus) — how a GitHub issue *becomes*
   an epic to plan, by label or by a dashboard click.

## Planning a GitHub issue

There are two ways to ask Hive to plan an issue. **The `plan` label is the
primary, first-class trigger** — maintainers think in labels, and it needs no
dashboard at all. The dashboard button is a convenience for the same flow.

### 1. The `plan` label (recommended)

Add the label **`plan`** (or **`epic`**) to any issue in a repo Hive watches. On
the next eval cycle Hive:

- mints an **epic bead** from the issue (idempotently — labeling the same issue
  twice never creates a second epic),
- marks it `plan_status=draft` and `decompose_pending`, so it shows up
  immediately in the dashboard as a queued plan, and
- hands it to the architect to decompose.

The architect closes the loop itself: its kick prompt names the epic and the
issue URL, asks it to read the issue, write the task list to a file and run
`bd decompose <epic-id> --plan <file>`. That command is the only thing that
creates child beads and clears `decompose_pending`; once it runs, the plan
appears in the plan-review view for your approval. Remove/re-add the label at
will — the epic is keyed to the issue.

The label trigger does **not** kick the architect every cycle. A pending epic
is kicked once, then left alone for 30 minutes (`planning.DecomposeRekickAfter`)
before it may be kicked again, and after 3 kicks with no children
(`planning.DecomposeMaxAttempts`) it is marked **stuck** (`decompose_failed`)
instead of being kicked a fourth time. Stuck plans show as **⚠** on the
PLANNING tile and first in the plan list; clicking **⧉ Plan** on the issue
again resets the budget and kicks immediately.

The label trigger is **OFF by default** and must be enabled explicitly. The
enumerated issue carries only its title and labels; the architect is handed the
issue URL and reads the body itself, with no per-kick review, so a maintainer
merely labeling an attacker's issue would otherwise auto-fire
attacker-controlled text into the highest-autonomy agent. Making it opt-in
forces an operator to consciously accept that. When enabled, it still only
fires at ACMM **L5+** (where the decomposing architect is scheduled), so
enabling it below L5 is inert.

```yaml
planning:
  plan_from_label: true   # opt in — fires at ACMM L5+; omit/false = off (default)
```

### 2. The **⧉ Plan** button (dashboard)

Every actionable issue pill on the dashboard has a **⧉ Plan** button. Click it to
run the exact same flow as the label — mint the epic and request decomposition —
without leaving the dashboard. Like the label path, the button needs ACMM
**L5+** (below that the request is refused with an explanation — the architect
that would build the plan has no cadence). Unlike the label path it sends the
issue body along, and a click is an explicit request: it bypasses the re-kick
throttle and gives a stuck epic a fresh attempt budget.

## The architect is a shared agent — and its pause is respected

Decomposition is driven by the **architect** agent, which is a *general-purpose*
lane with its own pre-existing job (RFCs, refactor and performance scans).
Decomposition is just one more thing it does, so:

> **If you have paused the architect, Hive does NOT auto-unpause it and does NOT
> silently drop your plan request.** The epic is minted and queued
> (`decompose_pending`), and it waits until you resume the architect.

When a plan is queued because the architect is paused, Hive tells you clearly —
in the **⧉ Plan** toast, and on the governor **PLANNING** tile:

> ⏸ Architect is paused — this plan won't be built until you resume the architect agent.

You paused it deliberately; this message makes sure you know that is *why*
nothing is happening, and points you at the fix. Resume the architect (or let its
cadence fire) and it picks up every queued plan on the next cycle, clears the
`decompose_pending` marker as it materializes children, and the normal
plan-review flow takes over.

Everything here drives the architect through the same out-of-band kick path the
governor already uses (`SendKick` / `IsPaused`); it never touches the
agent-launch mutex.

## Reviewing and approving a plan

A decomposed epic is a **draft**: its child beads are hidden from `Ready()`, so no
agent can start the work yet. In the plan-review view you can:

- inspect the ordered children with their execution tags (`agent_suitable` /
  `human_required`) and dependency edges,
- **retag** or **remove** a child before approving,
- **approve** — releasing the children through `Ready()` so the fleet can claim
  them, or
- **reject** — returning the plan to draft (re-gating the children) so the
  architect can revise it.

High-maturity ACMM packs may enable `plan_auto_approve`, which approves a plan at
decomposition time (no review gate); by default the gate is on.

Each child row also shows **who is on it** and **where the work is**: the
task's `claimed_by` metadata and its PR link (`pr_url` metadata, or the bead's
`external_ref` when that is a PR URL). Agents and people record these with
`bd update <id> --claim --set-metadata claimed_by=<name>` and
`bd update <id> --set-metadata pr_url=<url>`; nothing is shown until they do.

### Seeing plan state from the issue itself

Once an issue has a plan, its pill in **REPOSITORIES** stops showing the
⧉ Plan button and shows a **state chip** instead — the same lifecycle the Plans
modal uses, compressed to fit next to the issue number:

| chip | state | meaning |
|------|-------|---------|
| `⧗` | queued | accepted, waiting for the architect to break it into tasks |
| `⚠` | stuck | the architect exhausted its attempts, or its last kick is over 12h old (`DecomposeStuckAfter`) with nothing built — click to retry |
| `● N` | review | N tasks drafted, waiting for your approval |
| `▶ d/N` | executing | approved; d of N tasks done |
| `✓ N` | done | approved and every task closed |

Clicking the chip opens that plan's review view. The chip is driven by
`planning.issues` on the status payload (one entry per issue-sourced epic), so
it needs no extra fetch and updates with the governor cycle.

### Mirroring the plan onto the issue (opt-in)

People who never open the dashboard can still see the plan. With
`planning.mirror_to_issue: true`, **approving** a plan posts its task list as a
GitHub checklist comment on the epic's source issue — ticked for closed tasks,
with claimant and PR where known, and a `#plan=<epicId>` link back to the
dashboard review. The comment is a snapshot taken at approval (the beads remain
the source of truth) and is marked `<!-- hive-plan-mirror -->`. It is off by
default because it writes to the issue thread; a failed post is logged and
audited (`plan_mirror_failed`) but never fails the approval.

## The PLANNING governor tile

The governor's **PLANNING** metric summarizes plan state across all bead stores:

- **active** — epics that have been decomposed (draft or approved),
- **review** — drafts awaiting human approval,
- **queued** — issue-sourced epics not yet built by the architect
  (`decompose_pending`),
- **⚠N** — plans stuck after the architect's attempt budget ran out,
- and a **⏸** marker plus a warning tooltip when queued work is blocked on a
  paused architect.

The tile's tooltip also lists **which** plans are waiting on a person
(`planning.waiting_on_human`): one line per plan, `● review` or `⚠ stuck`,
with its `owner/repo#N` and title — so "what does the hive need from me?" is
answered on hover. The Plans modal lists the same items first.

When there is no planning activity at all, the tile's tooltip nudges you toward
the feature: *click ⧉ Plan on any issue, or add the `plan` label on GitHub.*

## Configuration reference

```yaml
# Auto-plan issues carrying the `plan`/`epic` label. Off unless set to true;
# even when true it only fires at ACMM L5+.
planning:
  plan_from_label: true
  # Post an approved plan's checklist on the source issue. Off by default.
  mirror_to_issue: true

# Tier-classification keywords (used to pick a model per issue) are config-driven
# and shown in the dashboard governor-config view. Empty/absent keeps the
# built-in defaults, so behavior is unchanged when unset.
classifier:
  simple_keywords: [typo, i18n, rename, const, label, badge, tooltip, placeholder, aria, "alt text"]
  complex_signals: ["race condition", deadlock, "memory leak", performance, "api change"]

# Stall-replan lane (Phase 3): re-kicks the architect on stalled plans.
governor:
  replan:
    enabled: true
    interval_s: 1800        # scan cadence (default 30m)
    stall_threshold_s: 21600 # no child progress for 6h → stalled
    max_replans: 5          # cap before escalating to a human
```

## Safety properties

- **Idempotent**: an issue maps to exactly one epic (keyed by a stable issue
  ref); the label trigger and repeated clicks never duplicate it.
- **Respects pause**: a paused architect queues plans, it is never force-resumed.
- **Gated**: draft-plan children are unclaimable until a human approves.
- **No launch-path mutex**: all architect interaction is via `SendKick`/`IsPaused`
  from the governor tick or an HTTP handler — never the agent-launch path.

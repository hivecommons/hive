# ACMM Policy Matrix

**ACMM** is the **AI Codebase Maturity Model**: a six-level framework that ties
how much autonomy AI agents get in a repository to how mature that repository's
safeguards are — tests, coverage, review practice, security posture. A project
starts at a low level where agents only observe and advise, and earns higher
levels (opening PRs, then merging on green CI) as the deterministic checks that
catch mistakes are put in place. Hive enforces each level in code: the level
decides which agents run, in which policy mode, with which GitHub permissions.
Background: [the ACMM paper](https://arxiv.org/abs/2604.09388).

**Terms used in the tables:** a **PR** is a pull request proposing a code
change; **CI** is continuous integration (automated checks); **GH Auth** means
GitHub credentials. **Beads** are internal work-ledger entries, not GitHub
issues. The **KB** is the knowledge base of reusable facts; an **RFC** is a
request for comments on a proposed design. For a first deployment's trust
choices, start with [Securing your hive](securing-your-hive.md).

## Policy Modes

The four policy tiers below describe the main autonomy ladder. The roster also
uses `issues-only` for measured/`ISSUES_ONLY` and `issues+prs` for
`ISSUES_AND_PRS` (PR creation without merge authority). `converse` is an
orthogonal comments/reviews capability, not a fifth autonomy level: the reviewer
remains advisory but can authenticate to post reviews. Always read the role's
level-specific qualifications, not just its template's `full` suffix.

| Mode | Suffix | Beads | Issues | PRs | Merge | GH Auth |
|------|--------|-------|--------|-----|-------|---------|
| **Advisory** | `-advisory.md` | Yes | No | No | No | No |
| **Measured** | `-measured.md` | Yes | Yes | No | No | Yes |
| **Holdgated** | `-holdgated.md` | Yes | Yes | Yes + `hold` label | No | Yes |
| **Full** | `-full.md` | Yes | Yes | Yes | Yes (auto on green CI) | Yes |

- **Advisory**: Agent observes and records findings as beads on the dashboard. No GitHub interaction.
- **Measured**: Agent can file GitHub issues to make findings visible to the team. No code changes.
- **Holdgated**: Agent can write code and open PRs, but every PR gets a `hold` label. A human must review and remove `hold` before merge. Agent never merges. Level-applied holds are not released automatically on promotion. A human removes them after review; a deliberate one-off `release_level_holds: true` request can release attributable Hive level holds — see the promotion note below. A hold **you** applied is never removed automatically.
- **Full**: Agent operates autonomously — opens PRs and merges on green CI. Highest trust level.

## ACMM Levels

### L1 — Inception (Assisted) (2 agents)

A single interactive advisor helps with repo setup and architecture decisions. Guide agent makes advisory beads. Brainstorm agent handles project inception — turning raw ideas into structured KB facts and scaffold. No feedback loops. See the [Inception operator guide](inception.md) for the end-to-end workflow and API reference.

| Agent | Mode | Template |
|-------|------|----------|
| guide | advisory | `guide-advisory.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L2 — Advisory (Instructed) (6 agents)

Agents observe and report findings as advisory beads on the dashboard and tracking issue. No GitHub issues or PRs created. The reviewer is the review-first entry point: it posts COMMENT-only PR reviews (no approvals, no fixer agent, no auto-merge). Humans decide what to act on.

Decision taken by hive: review is part of L2 proper (COMMENT-only); no L2.5 level — a new level would ripple through level enums, advisor and dashboard; keeping L2 triage+review preserves the review-first entry point the vote asked for.

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | advisory | `scanner-advisory.md` |
| quality | advisory | `quality-advisory.md` |
| guide | advisory | `guide-advisory.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |
| reviewer | converse | `reviewer-queue.md` |

### L3 — Quality-Gated (Measured) (7 agents)

Quality agent opens GitHub issues and PRs about testing gaps, coverage, and CI workflows. All other agents remain advisory. CI-maintainer joins to monitor build health. Reviewer works the `hold`-gated PR queue every 30 minutes and posts repo-grounded review comments for the human who lifts the hold; it has no lgtm, approve, or merge power. Key artifact: measurement infrastructure. L3 is also the first level at which the dashboard shows the **Experiment Lab (nous)** section when `dashboard.experiment_lab` is on (see [Experiment Lab](experiment-lab.md#visibility)).

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | advisory | `scanner-advisory.md` |
| ci-maintainer | advisory | `ci-maintainer-advisory.md` |
| **quality** | **holdgated** | `quality-holdgated.md` |
| guide | advisory | `guide-advisory.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |
| reviewer | converse | `reviewer-queue.md` |

### L4 — Security-Aware (Adaptive) (8 agents)

Delivery agents open GitHub issues — bugs, docs gaps, CI problems, security vulnerabilities. Only Quality, sec-check, and ci-maintainer may open PRs. Security agent joins. Reviewer works the `hold`-gated PR queue every 30 minutes and posts repo-grounded review comments for the human who lifts the hold; it has no lgtm, approve, or merge power.

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | measured | `scanner-issues.md` |
| ci-maintainer | holdgated | `ci-maintainer-holdgated.md` |
| **quality** | **holdgated** | `quality-holdgated.md` |
| guide | measured | `guide-issues.md` |
| **sec-check** | **holdgated** | `sec-check-holdgated.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |
| reviewer | converse | `reviewer-queue.md` |

### L5 — Semi-Autonomous (Semi-Automated) (14 agents)

**Permission boundary:** agents open issues and PRs, but do not merge
autonomously. Agent PRs get literal `hold` from the level gate for human
batch-review and release. The dashboard `hive-pause/<hive-id>` is a separate
manual hold; `hive/<hive-id>` records provenance only.

**Specialist lanes:**

- Architect produces RFCs; strategist coordinates across agents.
- Reviewer works the hold-gated PR queue every 30 minutes.
- Adjudicator works escalated (`needs-human`) Hive PRs through the reviewer
  lane every 30 minutes. It may recommend closure, but below L6 only an operator
  closes them.
- Dedupe runs daily, reads all open issues/PRs, and posts one consolidation
  checklist per repository (near-duplicates, common root causes, unlinked
  PR/issue pairs and already-resolved items), parked with `--needs-decision`.
  It never closes or edits anyone else's items.

Per-repo opt-in `auto_merge.queue_on_approve_and_unhold` (off by default) lets a person finish an agent PR in one step: when one trusted merger both submits an APPROVED review on an agent PR's current head and removes its level `hold`, Hive queues the PR for auto-merge on green CI exactly as the dashboard's queue button does. Nothing else counts as consent, and the self-merge floor stays at L6. See [hive-merge.md](hive-merge.md#one-human-step-at-l5-approve-and-unhold).

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | holdgated | `scanner-holdgated.md` |
| ci-maintainer | holdgated | `ci-maintainer-holdgated.md` |
| quality | holdgated | `quality-holdgated.md` |
| guide | holdgated | `guide-holdgated.md` |
| sec-check | holdgated | `sec-check-holdgated.md` |
| architect | holdgated | `architect-holdgated.md` |
| strategist | holdgated | `strategist-holdgated.md` |
| reviewer | converse | `reviewer-queue.md` |
| adjudicator | issues+prs | `reviewer-lane.md` (by role; no `kick_template`) |
| dedupe | issues-only | `dedupe-issues.md` |
| telemetry (paused) | holdgated | `telemetry-holdgated.md` |
| operations (paused) | holdgated | `operations-holdgated.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L6 — Autonomous (15 agents)

**Permission boundary:** autonomous delivery lanes can open issues/PRs and
eligible PRs can merge on green CI. Auto-merge is off below L6. Switching to
L6 enables it for every active repository; owners can then toggle repositories
individually. L6 does not remove other merge gates or grant every role merge
authority.

#### Destination and author exceptions

- **Outreach:** no level hold for PRs into a repository managed by this hive.
  Outreach PRs into another repository remain held for human review at every
  level, including L6.
- **Contributors:** the contributor lane admits operator-trusted authors or
  those admitted by the conservative
  `auto_merge.contributor_tier.auto_promote_after_merged` clean-history ratchet.
  Review, CI, diff fences, soak and branch protection still apply.
- **Nonmerging roles:** reviewer never merges. Adjudicator repairs,
  de-escalates, recommends closure and may close escalated `needs-human` Hive
  PRs at L6, but never merges. Dedupe stays recommendation-only. Telemetry and
  operations stay paused and use `ISSUES_AND_PRS`, despite their `full`
  template names; they never merge their own PRs.

#### Review and decision gates

The reviewer role remains advisory. Its verdict becomes an enforced merge gate
when `review.require_approval: true` or `governor.review_gate.mode: enforce`
is set. The App sweep checks current-head aggregate approval on self-authored,
trusted-bot and trusted-author paths; it is not scanner-only.
A trusted human merger's GitHub **Approve** review on that exact head can
override any swarm outcome, including `changes_requested`/`reject`; the audit
records who overrode which verdict. Other holds, CI, author admission, path
fences and GitHub rules still bind. A head-bound `/hive decision approve`
on an eligible Hive-authored PR can also answer `requires_human`; bare
`/hive approve` cannot. See [the L6 gate contract](running-at-level-6.md#does-the-hive-reviewer-stop-a-merge).

For Hive-authored PRs, `review.self_authored_decision_policy: resolve`
(default) lets the reviewer resolve non-risk decision questions. The exceptions
are permissions/credentials, security, sensitive paths, data loss/irreversible
actions, merge/trust policy, external side effects, licensing/legal,
unverifiable claims, or `hive-decision-risk: high`. Set `escalate` to park
every decision question at PR stage 085. Use actual holds or GitHub branch
rules for a universal merge stop.

#### Workflow approval, not App Actions-write permission

For trusted same-repo bot/App/registry-trusted clanker PRs with
`action_required` workflow runs, Hive posts `/ok-to-test` once per head.
The repository's self-contained `hive-ok-to-test` workflow approves the runs
using its own `GITHUB_TOKEN`; the App needs no Actions-write grant. If that
workflow is missing or `sentinel-alert` is present, Hive surfaces
`workflow-approval-required:<ctx>` / PR stage 085 for human action.

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | full | `scanner-automerge.md` |
| ci-maintainer | full | `ci-maintainer-full.md` |
| quality | full | `quality-full.md` |
| guide | full | `guide-full.md` |
| sec-check | full | `sec-check-full.md` |
| architect | full | `architect-full.md` |
| strategist | full | `strategist-full.md` |
| outreach | full | `outreach-full.md` |
| reviewer | converse | `reviewer-queue.md` |
| adjudicator | issues+prs | `reviewer-lane.md` (by role; no `kick_template`) |
| dedupe | issues-only | `dedupe-issues.md` |
| telemetry (paused) | full | `telemetry-full.md` |
| operations (paused) | full | `operations-full.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

## What hive will say on GitHub at each level

ACMM constrains *agents*; the platform sweeps (sentinel, hold guard, stage
labels, supersession, …) are not agents and used to run unconditionally
over `project.repos`. Since the write-back containment gate
(`pkg/writeback`, see
[operator-reference.md](operator-reference.md#what-hive-writes-to-github-and-when))
each *unsolicited* write-back kind has its own floor, and on a PR hive did
not open it additionally needs the review opt-in label
(`review.require_labels`, default `hive-review`). Hive-authored PRs and
issues, `/hive` command replies and dashboard-triggered actions are never
gated. Defaults (override under `writeback.kinds.<kind>`):

| Write-back kind | L1–L3 | L4 | L5 | L6 |
| --- | --- | --- | --- | --- |
| `sentinel` comment + `sentinel-alert` label | dashboard only | dashboard only | opt-in label only | all PRs |
| `hold_gate_notice` (mergeability / drift) | — | hive PRs only | hive PRs only | hive PRs only |
| `stage_label`, `needs_decision_comment` on foreign PRs | internal tracking | internal tracking | opt-in label | opt-in label |
| `supersession_notice`, `duplicate_fix_notice`, `reporter_trust_hold`, `covered_by_pr_label`, `issue_close`, `parent_issue_edit`, `gate_comment` | — | — | opt-in label | opt-in label |
| `supersession_close`, `contributor_adopt`, `ci_rerun` | — | — | — | opt-in label |
| `verdict_label` | L3: opt-in label | opt-in label | opt-in label | opt-in label |
| `claim_comment` | — | opt-in label | opt-in label | opt-in label |
| `welcome_comment` | — | — | opt-in label | all items |
| `issue_file` (new hive-authored issues) | L3+ | yes | yes | yes |

In practice: **L4 means hive files issues and talks only on its own PRs**;
**L5 adds comments and labels on PRs a maintainer labelled `hive-review`**;
**L6 lets sentinel and welcome comments reach unlabelled PRs**. A repo
listed only so an agent can file regression issues should carry
`writeback.per_repo.<repo>.mode: issues-only`, which holds at every level.
`backfill: false` (default) additionally keeps every kind off items that
predate hive's first sight of, or write access to, the repo.

## Example config alignment

`src/hive.yaml.example` uses the built-in `quality` lane for automated test suites, coverage gates, and regression checks. There is no built-in `tester` lane in the ACMM packs, known-agent defaults, or shipped policy templates; operators who want a separate tester must define it as a custom agent with explicit metadata and a policy template.

## Key Rules

1. **Level holds use literal `hold`.** Agent PRs below L6 are hold-gated with `hold`, and cross-repo outreach PRs (target repo not managed by this hive) are held at every level; same-repo outreach PRs follow the level like any other lane's PR. The dashboard `hive-pause/<hive-id>` label is for manual item holds. See [Hive Labels and Control Signals](labels-and-control-signals.md).
2. **Advisory agents never get GH auth.** The `${GH_AUTH}` template variable is only injected into measured, holdgated, full, and converse templates. The converse tier is the one place an agent writes to GitHub without sitting on the mode ladder: `reviewer` is `mode: ADVISORY` plus the orthogonal `converse` capability ([#4492](https://github.com/hivecommons/hive/issues/4492)), which grants comments and PR reviews and nothing else — no issue creation, no relabelling, no push, no merge. It needs the auth block because posting a review *is* a GitHub write.
3. **Supervisor uses no-GitHub advisory mode.** At every level, supervisor uses `supervisor-nogithub.md` in the built-in ACMM packs — it monitors agent health, not code.
4. **Mode escalation is per-agent.** At L4, some agents are measured (issues only) while others are holdgated (issues + PRs). The level defines the mix.
5. **Knowledge priming works at all levels.** The `${KNOWLEDGE}` template variable injects relevant facts from git sources and wiki layers regardless of the agent's mode.
6. **Brainstorm is always advisory.** It produces KB facts and beads, never GitHub issues or PRs. Its role evolves from inception (L1) to ongoing ideation (L2+), but its mode stays advisory at all levels.
7. **Reviewer is in every pack from L2 up and never merges.** It runs every
   30 minutes in every governor mode. At L2 it is COMMENT-only; at L3/L4 it
   comments on the held queue and routes to a human, with no lgtm/approve/merge
   authority. L1 omits it; a manually created L1 reviewer's mode, model,
   backend and pause state survive pack apply. Its mode remains `ADVISORY`,
   including at L6. This role restriction is separate from the enforcing
   review gate described above, which **does** read verdicts on the App sweep.
   Use a hold or GitHub branch rule for a universal stop.
   Roster history: [#8023](https://github.com/hivecommons/hive/issues/8023)
   and [#13574](https://github.com/hivecommons/hive/issues/13574).
8. **Telemetry and operations are L5/L6-only opt-in agents.** Below L5 they are absent from the pack roster and dashboard, do not spawn panes, and cannot be kicked. At L5–L6 they use a paused cadence in every governor mode until an operator opts in; they may open issues and PRs but never merge.

## ioscan hardening defaults per level

Two `ioscan` hardening modes take their default from the pack governor rather
than being fixed globally. Both are overridable per hive in either direction —
the pack only supplies the default when the hive leaves the key unset.

| Setting | L1–L4 default | L5–L6 default | What the default does | Override |
|---|---|---|---|---|
| `ioscan.canaries` | **on** | **on** | Plants a per-kick `HIVE-CANARY-*` marker and scans agent egress for it. Default flipped on ([#7083](https://github.com/hivecommons/hive/issues/7083)) now that the egress scan is encoding-aware ([#6701](https://github.com/hivecommons/hive/issues/6701), [#6720](https://github.com/hivecommons/hive/issues/6720)). | `ioscan.canaries: false` |
| `ioscan.fail_mode` | `open` | **`closed`** (set by the L5/L6 packs' `governor.ioscan_fail_mode`) | `open` redacts a Critical injection finding and continues the kick; `closed` blocks the kick and records an `ioscan_fail_closed` audit entry. | `ioscan.fail_mode: open` (or `closed` to opt in below L5) |

`fail_mode: closed` is the default only at L5–L6, where agent PR creation is
broadest and L6 also permits merges, so a Critical finding that slips through has the highest
blast radius. **The tradeoff is real and worth stating plainly: under `closed`,
every Critical false-positive becomes a stalled queue item that an operator must
clear by hand.** A hive that cannot absorb that operational load should set
`ioscan.fail_mode: open` explicitly; an L1–L4 hive that wants the stricter
posture sets `ioscan.fail_mode: closed`. An explicit value always wins over the
pack default. The knob lives on the pack governor:

```yaml
# packs/level-5.yaml (and level-6.yaml)
governor:
  ioscan_fail_mode: closed   # "" (open) below L5; closed at L5/L6
```

## Which levels may publish audit findings

The audit campaign's issue publisher ([audit-campaign.md](audit-campaign.md#publication)) files validated findings as issues only at **L3 and above**, the first level whose pack grants an agent the measured (issues) mode. At L1 and L2 every agent is advisory, so the publisher refuses with a typed error and an audit entry instead of filing. Security-sensitive findings never become public issues at any level; they go to `publication.private_channel` or are refused. Publication also requires `publication.enabled: true` and the `enforce` convergence mode; `shadow` records `withheld:mode` and writes nothing.

## Where ACMM gap issues are filed

The dashboard's ACMM evaluation lists each criterion a repo is missing, and
the level dialog offers **Open Issue** (per criterion) and **Open All** (every
failing criterion at that level). Both call `POST /api/acmm/issue`. By default
the issue goes to the repo's **GitHub Issues** with the `acmm` and
`ai-fix-requested` labels.

A hive whose backlog lives in Linear (`governor.work_source.type: linear`) can
file them there instead:

```yaml
governor:
  acmm:
    issue_tracker: work_source   # "" | github | work_source (default github)
```

| Value | Where the issue goes |
|---|---|
| `github` (or unset) | GitHub Issues on the criterion's repo — unchanged behavior. |
| `work_source` | Wherever the backlog lives. Linear work source → Linear `issueCreate` on the team whose `teams[].repo` matches the criterion's repo (else the first team). GitHub / GitHub Projects / Jira / unset → GitHub Issues as above. |

Any other value fails config validation at load time.

The choice can also be made **per click**: the request body accepts an
optional `tracker` field (`"github"` or `"work_source"`) that overrides the
configured default for that one issue; an unknown value is a `400`. When the
work source is Linear the level dialog shows a small **File in: GitHub /
Linear** selector, pre-selected from the config, that sets this field. The
response always says where the issue went:

```json
{"tracker": "linear", "issue_number": 42, "identifier": "ENG-42",
 "issue_url": "https://linear.app/acme/issue/ENG-42/...", "team": "ENG"}
```

(`tracker: "github"` responses carry `issue_number` and `issue_url` as
before.) `GET /api/acmm/evaluation` reports the effective default as
`issue_tracker` (`github` | `linear`) plus `work_source_type`. Title and body
are identical on both trackers; Linear uses `work_source.linear.api_key`, and
the audit log records `tracker` alongside the URL either way.

## Changing a hive's ACMM level

Promoting or demoting a running hive between levels is a single operation — the
hive reconciles its agent roster and per-agent modes to match the target level.

**From the dashboard:** open the Governor config and set the ACMM level. This is
the normal path. When a change makes the L6 self-authored merge sweep eligible,
Hive turns auto-merge on for every active repository, and the dashboard shows an informational modal listing held App-authored PRs and
the watched repositories' auto-merge switches so owners can switch off repos that should not participate; it does not release holds.

**Over the API:** `PUT /api/packs/level` with `{"level": N}` where N is 1–6.
Level-applied `hold` labels are never released automatically on a level change.
For a deliberate one-off recovery, include `release_level_holds: true` on this
PUT; Hive removes only its own level-applied `hold` labels whose latest hold
event was by the App and comments on each PR with the operator and target level.

What happens when the level changes (`handlePackSetLevel` → `ApplyPack`):

1. The new level is written to `acmm_level` in `hive.yaml` and persisted.
2. Per-agent `mode` overrides are cleared so stale modes from the previous level
   are not re-applied on the next config reload.
3. `ApplyPack` cascades the target level's pack-defined agent fields (mode,
   `kick_template`, description, …) into the live config **and reconciles the
   roster** — adding every agent the level introduces (for example
   architect/strategist at higher levels) and applying each agent's mode for
   that level (advisory → measured → holdgated → full).
4. If the change crosses the L6 self-merge boundary, Hive restarts the request
   relay generation so the self-authored auto-merge sweep starts or stops under
   the new ACMM verdict without waiting for a pod restart or GitHub App re-save.
   Existing level-applied holds remain held unless this `PUT /api/packs/level`
   request explicitly included `release_level_holds: true`. When the sweep just
   became eligible, the response includes `self_merge_sweep_active`,
   `level_holds_pending`, `repos`, and `level_changed_at` so the dashboard can
   inform the owner what remains held and which repos participate in L6
   auto-merge.

Notes:

- **Promotion adds agents and capability; demotion narrows it.** Moving up to L6
  makes agents auto-merge on green CI; moving down returns them to holdgated or
  advisory. The per-level capability grid is the table at the top of this page.
  Promotion does **not** release level holds the hive itself applied; a human
  removes `hold`, or an operator makes the one-off API call above. The one-off
  release is fail-closed: it applies only to PRs carrying the hive's own
  attributable level-hold notice and only when the most recent `hold` label
  event was applied by the App. A hold a human applied — or re-applied after the
  hive removed one — is never touched.
- **Operator-created agents are preserved.** `ApplyPack` reconciles pack agents;
  agents you created yourself are not removed by a level change (deletion is
  tombstoned separately — see agent configuration).
- On a hosted hive the level can also be hub/admin-managed; the ConfigMap seed is
  authoritative for `acmm_level` on those (see the operator reference on config
  precedence).

## Automated level-up guidance

A separate, advisory-only computation — the ACMM advisor (`pkg/acmmadvisor`) —
can tell you whether a hive has earned progression to the next level, based on
test coverage, green-CI streak, merge success rate, and backlog/hold signals.
It never changes the applied level itself; changing the level is always the
manual process described above. See the [ACMM advisor](acmm-advisor.md) page
for the exact thresholds per target level and what the `GET
/api/acmm-recommendation` endpoint returns.

## Automatic autonomy-signal level changes

The retro lane can also act on recorded autonomy signal findings. This policy is
additive and **off by default**:

```yaml
autonomy:
  auto_promote: false
  auto_demote: false
  promote_after: 3
  demote_on: rollback   # rollback | rework | either
  max_level: 6
  cooldown_days: 7
```

When enabled, three consecutive qualifying repo-scoped retro findings promote
the repo by one level, never skipping a level and never above `max_level`.
A rollback finding demotes by one level immediately; demotions are not blocked
by cooldown. A pinned repo policy (`project.repo_policies[].acmm_pinned: true`)
is never moved automatically.

Every automatic move writes a repo-keyed `project.repo_policies[]` record with
the last change, evidence bead IDs, and pin state, and also records an audit
entry plus a visible decision bead. The hive-wide `acmm_level` remains the
ceiling. Until the per-repo ACMM RFC (#6111) lands, Hive keeps this repo-keyed
seam and teaches the live proxy to apply the repo override on matching
repository requests so enforcement observes the decision without a restart.

For the narrow L6-except-one-repo case, `project.repo_policies[].auto_merge:
false` disables only auto-merge on that repository. Below L6, auto-merge is
effectively off for every repo regardless of stored values. The hive-wide ACMM
level still determines issue/PR creation authority, but merge authority is removed at
the `hive-merge` relay, App self-authored sweep, and proxy direct-merge seam.
The dashboard toggle (`POST /api/repos/auto-merge`) is gated asymmetrically:
switching auto-merge *off* needs the same tier as pausing the repo (verified
owner or GitHub repo write), but switching it back *on* restores Hive's merge
authority and requires a verified owner (#9070).

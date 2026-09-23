# ACMM Policy Matrix

## Policy Modes

Each agent runs in one of four modes, controlling what actions it can take on GitHub.

| Mode | Suffix | Beads | Issues | PRs | Merge | GH Auth |
|------|--------|-------|--------|-----|-------|---------|
| **Advisory** | `-advisory.md` | Yes | No | No | No | No |
| **Measured** | `-measured.md` | Yes | Yes | No | No | Yes |
| **Holdgated** | `-holdgated.md` | Yes | Yes | Yes + `hold` label | No | Yes |
| **Full** | `-full.md` | Yes | Yes | Yes | Yes (auto on green CI) | Yes |

- **Advisory**: Agent observes and records findings as beads on the dashboard. No GitHub interaction.
- **Measured**: Agent can file GitHub issues to make findings visible to the team. No code changes.
- **Holdgated**: Agent can write code and open PRs, but every PR gets a `hold` label. A human must review and remove `hold` before merge. Agent never merges. One exception: a hold the hive applied *for level reasons* is released automatically once the current level no longer calls for it — see the promotion note below. A hold **you** applied is never removed automatically.
- **Full**: Agent operates autonomously — opens PRs and merges on green CI. Highest trust level.

## ACMM Levels

### L1 — Inception (Assisted) (2 agents)

A single interactive advisor helps with repo setup and architecture decisions. Guide agent makes advisory beads. Brainstorm agent handles project inception — turning raw ideas into structured KB facts and scaffold. No feedback loops. See the [Inception operator guide](inception.md) for the end-to-end workflow and API reference.

| Agent | Mode | Template |
|-------|------|----------|
| guide | advisory | `guide-advisory.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L2 — Advisory (Instructed) (5 agents)

Agents observe and report findings as advisory beads on the dashboard and tracking issue. No GitHub issues or PRs created. Humans decide what to act on.

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | advisory | `scanner-advisory.md` |
| quality | advisory | `quality-advisory.md` |
| guide | advisory | `guide-advisory.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L3 — Quality-Gated (Measured) (6 agents)

Quality agent opens GitHub issues and PRs about testing gaps, coverage, and CI workflows. All other agents remain advisory. CI-maintainer joins to monitor build health. Key artifact: measurement infrastructure.

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | advisory | `scanner-advisory.md` |
| ci-maintainer | advisory | `ci-maintainer-advisory.md` |
| **quality** | **holdgated** | `quality-holdgated.md` |
| guide | advisory | `guide-advisory.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L4 — Security-Aware (Adaptive) (7 agents)

Delivery agents open GitHub issues — bugs, docs gaps, CI problems, security vulnerabilities. Only Quality, sec-check, and ci-maintainer may open PRs. Security agent joins.

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | measured | `scanner-issues.md` |
| ci-maintainer | holdgated | `ci-maintainer-holdgated.md` |
| **quality** | **holdgated** | `quality-holdgated.md` |
| guide | measured | `guide-issues.md` |
| **sec-check** | **holdgated** | `sec-check-holdgated.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L5 — Semi-Autonomous (Semi-Automated) (12 agents)

Agents open issues AND pull requests. All PRs get a hold label — humans batch-review and approve. Architect produces RFCs, strategist coordinates across agents, and reviewer works the hold-gated PR queue every 30 minutes. The system proposes; it does not merge autonomously.

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
| telemetry (paused) | holdgated | `telemetry-holdgated.md` |
| operations (paused) | holdgated | `operations-holdgated.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L6 — Fully Autonomous (13 agents)

Existing autonomous lanes can open issues, create PRs, and auto-merge on green CI. No hold label. Outreach handles community engagement. Reviewer stays advisory even here — its `requires_human` verdict is what pulls a PR out of the auto-merge lane. Telemetry and operations remain paused and use `ISSUES_AND_PRS`, so they never merge their own PRs.

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
| telemetry (paused) | full | `telemetry-full.md` |
| operations (paused) | full | `operations-full.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

## Example config alignment

`src/hive.yaml.example` uses the built-in `quality` lane for automated test suites, coverage gates, and regression checks. There is no built-in `tester` lane in the ACMM packs, known-agent defaults, or shipped policy templates; operators who want a separate tester must define it as a custom agent with explicit metadata and a policy template.

## Key Rules

1. **All PRs are holdgated below L6.** No agent can auto-merge unless running at L6 (Fully Autonomous).
2. **Advisory agents never get GH auth.** The `${GH_AUTH}` template variable is only injected into measured, holdgated, full, and converse templates. The converse tier is the one place an agent writes to GitHub without sitting on the mode ladder: `reviewer` is `mode: ADVISORY` plus the orthogonal `converse` capability ([#4492](https://github.com/hivecommons/hive/issues/4492)), which grants comments and PR reviews and nothing else — no issue creation, no relabelling, no push, no merge. It needs the auth block because posting a review *is* a GitHub write.
3. **Supervisor uses no-GitHub advisory mode.** At every level, supervisor uses `supervisor-nogithub.md` in the built-in ACMM packs — it monitors agent health, not code.
4. **Mode escalation is per-agent.** At L4, some agents are measured (issues only) while others are holdgated (issues + PRs). The level defines the mix.
5. **Knowledge priming works at all levels.** The `${KNOWLEDGE}` template variable injects relevant facts from git sources and wiki layers regardless of the agent's mode.
6. **Brainstorm is always advisory.** It produces KB facts and beads, never GitHub issues or PRs. Its role evolves from inception (L1) to ongoing ideation (L2+), but its mode stays advisory at all levels.
7. **Reviewer is L5/L6-only by default and never merges.** It joined the L5 and L6 rosters in [#8023](https://github.com/hivecommons/hive/issues/8023) at a 30-minute cadence in every governor mode. Below L5 no pack lists it, so an operator who wants repo-grounded PR review creates it by hand and a pack apply leaves that agent's mode, model, backend, and pause state alone. Its mode stays `ADVISORY` at both levels, including L6: it reads the queue, comments, and returns a verdict, and it is that verdict — `requires_human` or `reject` — that pulls a PR out of the auto-merge lane.
8. **Telemetry and operations are L5/L6-only opt-in agents.** Below L5 they are absent from the pack roster and dashboard, do not spawn panes, and cannot be kicked. At L5–L6 they use a paused cadence in every governor mode until an operator opts in; they may open issues and PRs but never merge.

## ioscan hardening defaults per level

Two `ioscan` hardening modes take their default from the pack governor rather
than being fixed globally. Both are overridable per hive in either direction —
the pack only supplies the default when the hive leaves the key unset.

| Setting | L1–L4 default | L5–L6 default | What the default does | Override |
|---|---|---|---|---|
| `ioscan.canaries` | **on** | **on** | Plants a per-kick `HIVE-CANARY-*` marker and scans agent egress for it. Default flipped on ([#7083](https://github.com/hivecommons/hive/issues/7083)) now that the egress scan is encoding-aware ([#6701](https://github.com/hivecommons/hive/issues/6701), [#6720](https://github.com/hivecommons/hive/issues/6720)). | `ioscan.canaries: false` |
| `ioscan.fail_mode` | `open` | **`closed`** (set by the L5/L6 packs' `governor.ioscan_fail_mode`) | `open` redacts a Critical injection finding and continues the kick; `closed` blocks the kick and records an `ioscan_fail_closed` audit entry. | `ioscan.fail_mode: open` (or `closed` to opt in below L5) |

`fail_mode: closed` is the default only at L5–L6 because those are the levels
where agents can merge, so a Critical finding that slips through has the highest
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
the normal path.

**Over the API:** `PUT /api/packs/level` with `{"level": N}` where N is 1–6.

What happens when the level changes (`handlePackSetLevel` → `ApplyPack`):

1. The new level is written to `acmm_level` in `hive.yaml` and persisted.
2. Per-agent `mode` overrides are cleared so stale modes from the previous level
   are not re-applied on the next config reload.
3. `ApplyPack` cascades the target level's pack-defined agent fields (mode,
   `kick_template`, description, …) into the live config **and reconciles the
   roster** — adding every agent the level introduces (for example
   architect/strategist at higher levels) and applying each agent's mode for
   that level (advisory → measured → holdgated → full).

Notes:

- **Promotion adds agents and capability; demotion narrows it.** Moving up to L6
  makes agents auto-merge on green CI; moving down returns them to holdgated or
  advisory. The per-level capability grid is the table at the top of this page.
  Promotion also **releases the level holds the hive itself applied** to open App
  PRs that the new level no longer requires, so you do not have to clean them up
  by hand after a level bump. Release is fail-closed: it applies only to
  App-authored PRs carrying the hive's own attributable level-hold notice, only
  when the most recent `hold` label event was applied by the App, and never while
  a self-authorization hold applies. A hold a human applied — or re-applied after
  the hive removed one — is never touched.
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

# ACMM Policy Matrix

> This matrix documents the legacy multi-agent service. The integrated Hive + Visual Hive product exposes the simpler `advisory`, `issues`, `repair-pr`, and `auto-merge` authority levels documented in the [integrated quickstart](integrated-quickstart.md). `repair-pr` is open-PRs-only; only `auto-merge` permits Hive to merge.

This matrix is enforced by central action policy and the GitHub proxy. Agent templates describe intent but do not grant authority. Coverage depth is configured separately and never escalates GitHub writes.

| Level | Issues | Repair branches/PRs | Merge |
|---|---|---|---|
| L1-L2 | denied | denied | denied |
| L3 | quality/testing findings only | selected quality/testing agents, hold-gated | denied |
| L4 | all applicable agents | selected quality/security/CI agents, hold-gated | denied |
| L5 | allowed agents | allowed agents, human-controlled | denied |
| L6 | allowed agents | allowed agents | only exact-SHA, all-green, protected, allowlisted low-risk changes |

Every allowed and denied lifecycle action is appended to the audit log. Pause, kill switch, WIP, rate, retry, time, token, and cost budgets apply at every level. A denied action is an error, not advisory fallback.

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
- **Holdgated**: Agent can write code and open PRs, but every PR gets a `hold` label. A human must review and remove `hold` before merge. Agent never merges.
- **Full**: Agent operates autonomously — opens PRs and merges on green CI. Highest trust level.

## ACMM Levels

### L1 — Inception (Assisted) (2 agents)

A single interactive advisor helps with repo setup and architecture decisions. Guide agent makes advisory beads. Brainstorm agent handles project inception — turning raw ideas into structured KB facts and scaffold. No feedback loops.

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

All agents open GitHub issues — bugs, docs gaps, CI problems, security vulnerabilities. Only Quality, sec-check, and ci-maintainer may open PRs. Security agent joins. Closed-loop feedback: agents act on their own findings.

| Agent | Mode | Template |
|-------|------|----------|
| supervisor | advisory | `supervisor-nogithub.md` |
| scanner | measured | `scanner-issues.md` |
| ci-maintainer | holdgated | `ci-maintainer-holdgated.md` |
| **quality** | **holdgated** | `quality-holdgated.md` |
| guide | measured | `guide-issues.md` |
| **sec-check** | **holdgated** | `sec-check-holdgated.md` |
| brainstorm | advisory | `brainstorm-advisory.md` |

### L5 — Semi-Autonomous (Semi-Automated) (9 agents)

Agents open issues AND pull requests. All PRs get a hold label — humans batch-review and approve. Architect produces RFCs, strategist coordinates across agents. The system proposes; it does not merge autonomously.

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
| brainstorm | advisory | `brainstorm-advisory.md` |

### L6 — Fully Autonomous (10 agents)

Agents open issues, create PRs, and auto-merge on green CI. No hold label. Outreach agent handles community engagement (highest trust — external-facing). Governor at fastest cadence.

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
| brainstorm | advisory | `brainstorm-advisory.md` |

## Tester lane

`v2/hive.yaml.example` includes a `tester` support agent that runs automated test suites, coverage gates, and nightly regression checks. It is intentionally outside the built-in ACMM policy packs in `v2/pkg/config/packs/`: the packs generate governance lanes, while `tester` is an operator-enabled local lane with cadences in the example config. If enabled, give it an explicit policy/template appropriate for the repository before granting write permissions.

## Key Rules

1. **All PRs are holdgated below L6.** No agent can auto-merge unless running at L6 (Fully Autonomous).
2. **Advisory agents never get GH auth.** The `${GH_AUTH}` template variable is only injected into measured, holdgated, and full templates.
3. **Supervisor uses no-GitHub advisory mode.** At every level, supervisor uses `supervisor-nogithub.md` in the built-in ACMM packs — it monitors agent health, not code.
4. **Mode escalation is per-agent.** At L4, some agents are measured (issues only) while others are holdgated (issues + PRs). The level defines the mix.
5. **Knowledge priming works at all levels.** The `${KNOWLEDGE}` template variable injects relevant facts from git sources and wiki layers regardless of the agent's mode.
6. **Brainstorm is always advisory.** It produces KB facts and beads, never GitHub issues or PRs. Its role evolves from inception (L1) to ongoing ideation (L2+), but its mode stays advisory at all levels.

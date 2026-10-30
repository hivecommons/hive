# Dashboard glossary and sidebar IA

Use this glossary when a dashboard word is unfamiliar. “Sidebar IA” means information architecture: how the navigation is grouped. The [sidebar map](#sidebar-map) helps you find a destination; [UI naming reference](#ui-naming-reference) is for people maintaining the interface.

For a plain-language explanation of every dashboard section — what it shows, how its numbers are worked out, and what to do about them — see [Dashboard sections explained](dashboard-sections.md). Each section title's **?** mark opens its entry there.

## Terms

| Term | What it means for a reader |
| --- | --- |
| Hive | The service that coordinates work, agents and policy for a set of repositories. |
| Hub / spoke | A hub coordinates shared services or discovery for Hives. A spoke is a Hive connected to a hub; its dashboard may still have its own access rules. |
| Agent | A running automation process or configured role that plans, scans, reviews or does other work. |
| Contributor | A person or GitHub account participating through the contributor portal. |
| Contributor agent | The contributor's worker, connected through a relay that requests work and reports results. ClankeR is the relay's product name; `author/clanker` is an exact label, not a different kind of person. |
| Stage | The item's current pipeline step, such as actionable, claimed, review or merge gate. A stage is not permission to bypass a hold or policy. |
| Bead | A record in Hive's work ledger. It can track an issue, a finding or other work; it is not itself a GitHub issue or an approved knowledge entry. |
| ACMM | AI Codebase Maturity Model: levels L1–L6 describe how much the Hive may do on its own. The dashboard also calls this the autonomy level. See the [policy matrix](acmm-policy-matrix.md). |
| Governor | The policy engine controlling cadence, autonomy, budgets and merge/apply gates. |
| Fleet | The agents and contributor agents being monitored together. |
| Lane | A named worker or processing path, such as an agent lane or an auto-merge lane. A lane can have its own admission checks. |
| Sentinel | The suspicious-activity check. A `sentinel-alert` flags a PR for maintainer attention; the label name can be configured. |
| Adopted | The Hive has taken responsibility for repairing a contributor-agent PR after the hand-back process. It may create a superseding PR; adoption does not allow rewriting a human author's work or lifting their holds. |
| Ratchet | A gate that records an existing baseline and rejects increases, rather than requiring every old problem to be fixed first. |
| Head | The current commit at the tip of a PR branch. Reviews and approvals may apply to that exact commit, not later pushes. |
| Verdict / dwell / soak | A verdict is a review result. Dwell is time at a stage before it is considered stalled. Soak is an admitted PR's waiting period before merge. |
| Swarm | Several reviewers working on the same review. Their results still pass through the merge policy. |
| Re-home | Continue work in a replacement PR owned by the appropriate author; do not silently rewrite the original author's commits. |
| Issue band / PR band | A display group in Projects, not a scheduler or merge permission. See the exact groups below. |

For exact stage labels, repair ownership and hold rules, see [Labels and control signals](labels-and-control-signals.md). For commands that inspect beads, see [`hivectl bead`](hivectl.md#bead--work-items).

## UI naming reference

The ADR-0018 information-architecture/naming pass is implemented. The table below records names and presentation rules for UI authors; it is not a second set of operator controls.

| Term | Meaning | Use in operator UI |
| --- | --- | --- |
| Agent | Runnable automation process or configured role that can plan, scan, review, or otherwise work on hive tasks. | Use for dashboard automation and agent detail pages. |
| Contributor | Human or GitHub account participating through the contributor portal. | Use for people/accounts, leaderboards, trust controls, and profile/admin copy. |
| Contributor agent (ClankeR) | A contributor's relay-backed worker. Use the full form on first or prominent mention, then Contributor agent for repeated inline mentions. | Use when the operator UI refers to the worker connected through the contributor relay. |
| Governor | Policy engine that decides cadence, autonomy, budgets, and merge/apply gates. | Use for the overview/governance surface and settings that control automation policy. |
| Fleet | The set of agents or contributor agents under observation in an operational context. | Use for aggregate operational controls and live monitoring. |
| Issue band | A display-only group in a repository card's issue column, named for the operator's action: Unclaimed, Claimed, Needs triage (agent-filed and not yet acknowledged by a human), Parked, or Confirm & close. | Use for Projects card grouping; do not imply scheduler eligibility changed. Hover a band header, Overview slice, or legend row for its rule. |
| Pill legend | The collapsible Projects-section key explaining pill colours, borders, glyphs, and badges as a colour key plus captioned issue, pull-request, review, link, and action pills, including the red sentinel pill for PRs that carry the configured sentinel alert label. The aligned repository-card rows keep the left chip for the issue band or PR review class and align actions in fixed slots. | Use for the compact legend above project cards. |
| PR band | A display-only group in a repository card's pull-request column: Gated, Merge-eligible, Blocked, In review, Open, or Draft. | Use for Projects card grouping; do not imply merge queue, hold, or review eligibility changed. |
| Stale issue | An actionable repository-card issue whose `updated_at` activity is older than the configured `dashboard.issue_bands.stale_days` threshold. | Use for the `N no activity > Nd` counter and `🕒` badge. |
| Stale PR | A repository-card PR whose `updated_at` activity is older than the configured `dashboard.issue_bands.stale_days` threshold. | Use for the PR `🕒` badge. |

Keep **ClankeR** only when naming the contributor relay product/brand, for example the contributor portal line “Powered by ClankeR.” Avoid casual lowercase `clanker` as an operator-facing noun.

## Sidebar map

The operator sidebar keeps the existing destinations, IDs, `data-action` handlers, and route hashes, but groups them by operator task:

| Group | Items |
| --- | --- |
| Dashboard | Overview, Governor, Throughput |
| Agents | Dynamic agent tree, `+ Add agent`, `+ Group` |
| Resources | Projects, Contributors |
| Intelligence | Advisory, ACMM Eval, Inception, Knowledge, Experiment Lab |
| Admin | Tokens, Cost, Review, Agents, Agent activity, Audit Log, Diagnostics |
| Help | FAQ, Getting Started, API Spec (Redoc), Getting Started Guide, Join our Discord, Report an Issue |

On the v6 line the sidebar also lists Runs. Sections hidden by the ACMM level or a feature setting are hidden from the sidebar too.

Count badges use the shared `.badge-count` recipe. Zero counts render as dimmed `0` badges with `data-zero`.

Internal `clanker-*` CSS classes, DOM ids, data keys, and JavaScript names are stable compatibility identifiers. They are not user-facing names and should remain as-is.

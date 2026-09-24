# Dashboard glossary and sidebar IA

This glossary records the operator-facing names used by the dashboard. The ADR-0018 IA/naming pass is implemented.

## Terms

| Term | Meaning | Use in operator UI |
| --- | --- | --- |
| Agent | Runnable automation process or configured role that can plan, scan, review, or otherwise work on hive tasks. | Use for dashboard automation and agent detail pages. |
| Contributor | Human or GitHub account participating through the contributor portal. | Use for people/accounts, leaderboards, trust controls, and profile/admin copy. |
| Contributor agent (ClankeR) | A contributor's relay-backed worker. Use the full form on first or prominent mention, then Contributor agent for repeated inline mentions. | Use when the operator UI refers to the worker connected through the contributor relay. |
| Governor | Policy engine that decides cadence, autonomy, budgets, and merge/apply gates. | Use for the overview/governance surface and settings that control automation policy. |
| Fleet | The set of agents or contributor agents under observation in an operational context. | Use for aggregate operational controls and live monitoring. |

Keep **ClankeR** only when naming the contributor relay product/brand, for example the contributor portal line “Powered by ClankeR.” Avoid casual lowercase `clanker` as an operator-facing noun.

## Sidebar map

The operator sidebar keeps the existing destinations, IDs, `data-action` handlers, and route hashes, but groups them by operator task:

| Group | Items |
| --- | --- |
| Overview | Governor |
| Agents | Dynamic agent tree, `+ agent`, `+ group` |
| Resources | Repos, Beads, Contributors |
| Intelligence | Advisory, ACMM Eval, Inception, Knowledge, Strategy Lab |
| Admin | Tokens, Cost, Audit Log |
| Help | FAQ, Getting Started, API Spec (Redoc), Getting Started Guide, Report an Issue |

Count badges use the shared `.badge-count` recipe. Zero counts render as dimmed `0` badges with `data-zero`.

Internal `clanker-*` CSS classes, DOM ids, data keys, and JavaScript names are stable compatibility identifiers. They are not user-facing names and should remain as-is.

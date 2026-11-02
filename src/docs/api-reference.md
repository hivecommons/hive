# Dashboard REST API reference

Pragmatic v1 endpoint index, split into one page per area under [`api-reference/`](api-reference/) and compiled by hand from route registrations in `src/pkg/dashboard/*.go` and `src/pkg/hub/*.go` (tests excluded). There is no generator; update this page in the same change that adds or renames a route. It lists method, path, coarse auth level, and one-line purpose. Request/response schemas are intentionally not hand-written here; see the handler source for exact payloads and validation.

Auth levels are derived from dashboard middleware (`isPublicPath`, dashboard token/session auth, and the `/api/v1` GitHub-token wrapper) or from hub route wrappers such as `requireAuth` (rows marked `Hub auth`) and `requireAdmin` (rows marked `Hub admin`). Hub rows marked handler-specific have no dashboard middleware; check the named handler for bearer secrets, admin checks, or public behavior.

History point limits, timestamp cutoffs, incremental cost reads and ETag/304 behavior are documented in [Dashboard history reads](dashboard-history-api.md).

## Areas

| Area | Page | Routes |
|---|---|---|
| Health, status, events | [`api-reference/health-status-events.md`](api-reference/health-status-events.md) | 50 |
| Snapshots and style | [`api-reference/snapshots-and-style.md`](api-reference/snapshots-and-style.md) | 5 |
| Auth and external accounts | [`api-reference/auth.md`](api-reference/auth.md) | 22 |
| Configuration | [`api-reference/configuration.md`](api-reference/configuration.md) | 105 |
| Agents and controls | [`api-reference/agents-and-controls.md`](api-reference/agents-and-controls.md) | 36 |
| Packs and ACMM | [`api-reference/packs-and-acmm.md`](api-reference/packs-and-acmm.md) | 18 |
| Cost, tokens, telemetry | [`api-reference/cost-tokens-telemetry.md`](api-reference/cost-tokens-telemetry.md) | 13 |
| Knowledge | [`api-reference/knowledge.md`](api-reference/knowledge.md) | 56 |
| Contribute | [`api-reference/contribute.md`](api-reference/contribute.md) | 68 |
| Nous / strategy lab | [`api-reference/nous.md`](api-reference/nous.md) | 25 |
| Inception | [`api-reference/inception.md`](api-reference/inception.md) | 14 |
| Beads | [`api-reference/beads.md`](api-reference/beads.md) | 5 |
| Dashboard miscellaneous | [`api-reference/dashboard-misc.md`](api-reference/dashboard-misc.md) | 56 |
| Hub SaaS | [`api-reference/hub-saas.md`](api-reference/hub-saas.md) | 102 |
| Hub server | [`api-reference/hub-server.md`](api-reference/hub-server.md) | 46 |

## Health, status, events

50 routes — see [api-reference/health-status-events.md](api-reference/health-status-events.md).

## Snapshots and style

5 routes — see [api-reference/snapshots-and-style.md](api-reference/snapshots-and-style.md).

## Auth and external accounts

22 routes — see [api-reference/auth.md](api-reference/auth.md).

## Configuration

105 routes — see [api-reference/configuration.md](api-reference/configuration.md).

## Agents and controls

36 routes — see [api-reference/agents-and-controls.md](api-reference/agents-and-controls.md).

## Packs and ACMM

18 routes — see [api-reference/packs-and-acmm.md](api-reference/packs-and-acmm.md).

## Cost, tokens, telemetry

13 routes — see [api-reference/cost-tokens-telemetry.md](api-reference/cost-tokens-telemetry.md).

## Knowledge

56 routes — see [api-reference/knowledge.md](api-reference/knowledge.md).

## Contribute

68 routes — see [api-reference/contribute.md](api-reference/contribute.md).

## Nous / strategy lab

25 routes — see [api-reference/nous.md](api-reference/nous.md).

## Inception

14 routes — see [api-reference/inception.md](api-reference/inception.md).

## Beads

5 routes — see [api-reference/beads.md](api-reference/beads.md).

## Dashboard miscellaneous

56 routes — see [api-reference/dashboard-misc.md](api-reference/dashboard-misc.md).

## Hub SaaS

102 routes — see [api-reference/hub-saas.md](api-reference/hub-saas.md).

## Hub server

46 routes — see [api-reference/hub-server.md](api-reference/hub-server.md).

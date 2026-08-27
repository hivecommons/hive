# ADR-0014: Hub/spoke fleet over heartbeat callbacks

Status: Accepted (back-filled)

## Context

Hive needs a shared fleet view without requiring every operator's cluster to be
reachable from the public hub. The architecture defines every hive as a spoke
and the hosted site as the hub, both running the same image with `HIVE_MODE=hub`
selecting the role ([architecture §8](../architecture.md#8-hub--spoke)). The hub
must maintain registry, leaderboard, provisioning, and operator controls even
for firewalled spokes.

## Decision

Use spoke-initiated heartbeats as the control channel. A spoke posts identity,
repos, ACMM level, agents, governor state, contributors, leaderboard, health,
version, image, and related status to `/api/heartbeat`; the hub persists a
sanitized registry entry and marks the spoke online
([heartbeat payload](../../pkg/hub/heartbeat.go), [hub registry](../../pkg/hub/server.go)).
The heartbeat response carries callbacks for upgrade, branch switch, GitHub App
config, banners, visibility, authorized users, project config, gateway config,
and restart requests, letting the hub act even when it cannot open a connection
back to the cluster.

Keep hub and spoke roles in one deployable image, and report the spoke's actual
image reference so the hub can detect pinned or stale deployments. Merge public
spoke leaderboards centrally while excluding agent identities, and use
contributor counts plus active leaderboard tasks as evidence that the ClankeR
contributor relay is carrying work. Contributor relay credentials remain on the
contributor's machine; the hub sees trust tier, task counts, and current task
metadata, not the user's credential material.

## Consequences

Rationale not recorded beyond the implementation, linked code, and cited design notes.

The fleet works through firewalls and NAT because all routine control flows ride
spoke-initiated HTTP. Operators get one registry, fleet-stats surface, and
leaderboard while individual hives retain local runtime state. The trade-off is
that heartbeat freshness and callback delivery become critical infrastructure:
missed beats make hives appear offline, callback actions are eventually delivered
rather than immediate, and the shared hub must carefully sanitize and bound every
spoke-reported field.

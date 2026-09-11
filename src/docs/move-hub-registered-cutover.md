# Hub-registered hive cutover: identity, heartbeat, dashboard URL

This is a cross-cutting reference for what happens at the **hub/heartbeat
layer** whenever a hub-registered hive's deployment moves — cluster-to-cluster
Kubernetes, or a host-to-host Compose/Quadlet move. It applies whether the
hive is **hub-hosted** (provisioned and managed by a hub) or **self-hosted
with heartbeats** (an operator-run deployment that merely reports in to a
hub). It does not repeat any runtime-specific copy/restore steps — those live
in [move-kubernetes.md](move-kubernetes.md) (Kubernetes → Kubernetes, this
hive's own cluster),
[cross-cluster-migration.md](cross-cluster-migration.md) (hub-hosted cluster
move), and `move-host.md` (Compose/Quadlet host moves, filed under
[epic #6521](https://github.com/hivecommons/hive/issues/6521), sub-issues
#6522–#6524) — this document is what they all link back to for the
concurrent-identity and cutover-order rules.

> **Status.** The concurrent-identity hazard and the cutover order below are
> grounded directly in the hub's duplicate-reporter detection code and the
> agent-side duplicate-PR guard (both cited below), which are real,
> already-shipped defenses against exactly this failure mode — that much is
> **OBSERVED** as existing, active code. Nobody has executed an actual
> concurrent-source/target cutover against a live hub in this environment, so
> the specific claim "the hazards below occur if you skip the cutover order"
> is **DOCUMENTED, NOT EXECUTED**, not a reproduced incident.

## What "identity" is

A hub-registered hive's identity is three pieces of state, none of which the
move procedures in the other guides should regenerate:

- **`hive-id`** — the stable identifier (`/data/hive-id` on Kubernetes,
  `hiveIDFilePath` in `src/pkg/dashboard/api.go:8884`). Passed through to
  launched agents and used to name the hive's namespace/registry entry.
- **The GitHub App private key** (or PAT) — what lets the hive act as itself
  on GitHub. Kept in the `hive-secrets` Secret on Kubernetes
  (`src/deploy/k8s/secret.yaml`), or the mounted `secrets/` directory on
  Compose/Quadlet.
- **`HIVE_HUB_SECRET`** — the hub's master secret, present in the spoke's own
  pod env. Every per-hive derived key (heartbeat bearer, session key, invite
  key, terminal key) is `HMAC(HIVE_HUB_SECRET, domain-string, HIVE_ID)` —
  see `src/docs/env-vars.md:305-320` and, for the heartbeat bearer
  specifically, `src/docs/heartbeat-bearer-cutover.md`:

  ```
  HMAC(master, "hive-heartbeat-v1" || 0x00 || hiveID)
  ```

  This means the heartbeat bearer is a **pure function of `HIVE_HUB_SECRET` +
  `HIVE_ID`** — anything holding both values can authenticate as this hive to
  the hub, with no additional hub-side action, registration, or new secret
  required (`src/docs/heartbeat-bearer-cutover.md` "Why an in-place migration
  is possible"). That is precisely what makes concurrent identity dangerous:
  a target you have restored but not yet cut over to is **already a fully
  authenticated second copy of this hive** the moment it starts, whether or
  not you intended it to heartbeat yet.

## How the hub sees heartbeat and `dashboard_url`

- The spoke sends a heartbeat payload including `dashboard_url` (JSON field
  `dashboard_url`, `src/pkg/hub/heartbeat.go:714`,
  `DashboardURL string json:"dashboard_url"`).
- The hub's heartbeat handler validates it (`src/pkg/hub/server.go:1733`,
  must start with `http://` or `https://`) and writes it straight into the
  registry entry for that hive (`src/pkg/hub/server.go:1800`,
  `DashboardURL: payload.DashboardURL`). **The heartbeat is the only writer.**
  Nothing else — not a hand-edit of the hub's registry file, not a hub API
  call — durably sets it, because the next heartbeat overwrites whatever was
  there (this is the same "heartbeat owns `dashboardUrl`" gotcha
  `cross-cluster-migration.md` documents for the hub-hosted case, generalized
  here to any runtime).
- If a spoke has no `hub.dashboard_url` configured, it falls back to reading
  its own Ingress/Route (`SpokeServedHost`, `src/pkg/hub/heartbeat.go:1850`)
  — which is why the in-namespace `hive-dashboard-route-reader` RBAC
  (`src/deploy/k8s/dashboard-route-rbac.yaml` on the self-hosted manifest;
  the hub-hosted equivalent is `hive-route-reader`,
  `cross-cluster-migration.md`) matters on every runtime that self-discovers
  its host this way.
- The "My Hives → Dashboard" button and any hub UI showing where to reach the
  hive read `registry.Hives[i].DashboardURL` (`src/pkg/hub/saas.go:4418,7279-7280,10204`)
  — i.e. exactly the value the last heartbeat wrote, never a value you set by
  hand anywhere else.

**Practical rule:** after a move, the hub only "knows" the new URL once (a)
`hub.dashboard_url` in the target's config is set to it (or the target's own
Ingress/Route now serves it and route-reader RBAC exists), and (b) the target
has sent at least one heartbeat. There is no faster path and no manual
override that survives the next beat.

## Why source and target must never run concurrently with the same identity

Two independent, already-shipped hub-side and agent-side mechanisms make
"just bring the target up before tearing down the source" an active hazard,
not merely an aesthetic one:

### 1. Duplicate heartbeats — the hub detects and reports this today

`noteReporter` (`src/pkg/hub/spoke_restart.go:56-111`) is a real, shipped
detector: each heartbeat carries an opaque `reporter` string (`"<pod>/<pid>"`
on modern spokes), and the hub tracks, per `hive_id`, every reporter seen
within a 15-minute window (`reporterConflictWindow`,
`src/pkg/hub/spoke_restart.go:22`). If a reporter that had stopped beating
**reappears** while a different reporter has been beating in between — the
signature of two live instances alternating, not a rollout — the hub sets
`ConflictingReporters` on the registry entry
(`src/pkg/hub/server.go:290-294,1781`) after two such alternations
(`reporterFlipsToConfirm = 2`,
`src/pkg/hub/spoke_restart.go:28`). This is surfaced as a real drift signal:
`src/pkg/hub/drift.go:572-574` turns a non-empty `ConflictingReporters` into
an alert reading "two spoke instances are reporting as this hive". A move
where the source is left running while the target starts heartbeating with
the same `hive-id` **produces exactly this alarm** — the mechanism is not
speculative, it exists because this has happened before (the comment at
`spoke_restart.go:147` calls it "a stale ReplicaSet pod, an orphaned duplicate
deployment").

### 2. Duplicate PR claims / dibs — the agent-side guard is a race, not a lock

The duplicate-PR guard (`src/pkg/github/prclaims.go`) exists precisely
because independent agent processes filing PRs against the same issue
produced duplicates (`prclaims.go:18-27`, "a restart storm... made a quality
agent open nine near-identical PRs overnight"). Its ledger,
`ClaimLedgerPath = "/data/pr-claims.json"` (`prclaims.go:73-76`), is **local
to each pod's own PVC** and is reconciled against live GitHub API state on
each cycle, falling back to the persisted ledger only when the API call
fails (`prclaims.go:954-962`, "duplicate-PR guard: claim fetch failed,
falling back to persisted ledger (fail closed)"). Two live instances of the
same hive — source still running, target already started — each maintain
**their own** claim ledger and each independently poll GitHub for the same
issue set. Nothing serializes the two: if both see an issue as unclaimed in
the same polling window (the exact race the guard was built to close for a
*single* restarting process), both can open a PR against it, using the
**same GitHub App identity** on both sides, before either ledger or the
GitHub API state converges. This is a direct extension of the documented
restart-storm failure mode, not a hypothetical: the guard was built for one
process racing itself; two live processes racing each other over the same
identity is the same hazard with a second actor.

### Compounding effect

Because the heartbeat bearer authenticates as "this hive" purely from
`HIVE_HUB_SECRET` + `HIVE_ID` (see above), the hub has no independent way to
tell source and target apart beyond the reporter string — which is exactly
what `noteReporter` is watching for, and exactly what a careless move
triggers.

## Required cutover order

1. **Stop the source first.** Scale it to zero (or stop the container/unit)
   before the target's copy of the data is put into service. This is the same
   ordering `backup-restore.md`'s Podman migration insists on ("the ordering
   of steps 4 and 5 is the safety property, not a formality") and
   `cross-cluster-migration.md` step 5 assumes ("Source stopped before the
   target starts") — generalized here as the rule for **any** hub-registered
   move, not just that one runtime pair.
2. **Restore/copy the data and config to the target**, per the runtime-specific
   guide (`move-kubernetes.md`, `cross-cluster-migration.md`, or
   `move-host.md`).
3. **Start the target.** Verify locally (`hive-id`, GitHub App key SHA-256,
   beads present) before it is allowed to heartbeat, if your rollout process
   lets you gate that; if not, proceed straight to verification below since
   the source is already stopped and there is no second live reporter to
   conflict with.
4. **Verify the hub sees the new dashboard URL** (below) before you delete or
   destroy the source's storage.

Never run steps 1 and 3 in the other order, and never leave the source
running "just in case" once the target has started heartbeating — that
window is exactly what `noteReporter` and the PR-claim race above are
watching for.

## GitHub App Setup URL / callback URL / OAuth implications

When the reachable dashboard URL changes:

- If the hive uses **device-flow login** (a self-hosted or heartbeat-only
  hive with its own OAuth, not proxied by a hub), update the GitHub App's
  **Setup URL** and, if applicable, its callback URL to the new host — see
  [github-app-setup.md](github-app-setup.md). Until this is done, sign-in
  from the new URL may fail even though heartbeats succeed, since OAuth
  configuration is independent of the heartbeat/registry path entirely.
- If `dashboard.public_url` is set (needed when an ingress rewrites `Host`,
  or for a Linear-agent/OpenRouter OAuth callback hostname mismatch — see
  `src/docs/linear-agent.md#setup`), update it to the new origin. It is
  **not** the same setting as `hub.dashboard_url` and is not derived from it
  — both may need updating independently.
- A **hub-hosted** hive behind the hub's own auth proxy needs none of this:
  the hub's nginx ingress is the OAuth/auth surface, not the spoke
  (`cross-cluster-migration.md` "Background: how the two clusters differ").
  This is the one place hub-hosted and self-hosted-with-heartbeats genuinely
  differ — see the next section.

## Hub-hosted vs. self-hosted-with-heartbeats: what differs

| | Hub-hosted | Self-hosted, reports heartbeats |
|---|---|---|
| Who terminates dashboard auth | Hub's nginx ingress (`auth-url` → `/api/saas/auth-check`) | The spoke itself (device-flow or `dashboard.auth_token`) |
| `hub.dashboard_url` needed on the spoke | Yes, to avoid the RBAC/route-discovery fallback | Optional — only if you want the hub's fleet views to show the real URL, since there's no hub auth proxy depending on it |
| Who owns `meta.json` / hub-registry bookkeeping | The hub (`cross-cluster-migration.md` step 4) | N/A — a self-hosted hive that heartbeats has no hub-managed `meta.json`; it is not hub-provisioned |
| Duplicate-identity hazard (this document) | Applies unchanged | Applies unchanged — `HIVE_HUB_SECRET` + `HIVE_ID` authenticate the heartbeat bearer the same way regardless of who provisioned the hive |
| Duplicate-PR hazard (this document) | Applies unchanged | Applies unchanged — the claim ledger and GitHub App identity are hive-local either way |

The concurrent-identity and cutover-order rules above are **identical** for
both columns; only the auth-proxy and registry-bookkeeping mechanics differ.

## Verify

- Hub registry shows the hive online with a fresh heartbeat timestamp and no
  `ConflictingReporters` value (`GET` the hub's hive detail, or check the
  dashboard's fleet view) — see `src/pkg/hub/drift.go` for what a non-empty
  value renders as.
- The hub's "My Hives → Dashboard" (or equivalent fleet UI) button targets the
  **new** URL — confirms `registry.Hives[i].DashboardURL` has been updated by
  a heartbeat from the target, not stale data from the source.
- Sign-in works from the new URL if OAuth/Setup URL was updated.

## Rollback

If the target fails verification, the safest rollback is to restart the
**source** from its last-known-good state (it was stopped, not destroyed, in
step 1 above) rather than trying to patch the target forward — this is why
cutover order step 4 above insists on verifying before the source's storage
is deleted. Do not run source and target simultaneously to "compare" —
that recreates the exact hazard this document describes. If the target has
already sent heartbeats with `ConflictingReporters` set, expect the hub to
keep flagging the conflict for up to `reporterConflictWindow` (15 minutes,
`src/pkg/hub/spoke_restart.go:22`) after the duplicate reporter stops
beating.

## Open questions this issue answers

| # | Question | Status |
|---|---|---|
| 11 | Concurrent identity: hazard and required cutover order | **DOCUMENTED, NOT EXECUTED** for the specific end-to-end scenario; the hazard mechanisms themselves (`noteReporter`, the PR-claim ledger) are **OBSERVED** as existing, shipped code — see citations above |
| 12 | Host-bound settings at the hub/heartbeat layer | **DOCUMENTED, NOT EXECUTED** — see "How the hub sees heartbeat and `dashboard_url`" and "GitHub App Setup URL" sections above |

See the epic ([#6521](https://github.com/hivecommons/hive/issues/6521)) for
the full table and the other sub-issues.

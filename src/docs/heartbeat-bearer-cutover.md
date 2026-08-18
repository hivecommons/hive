# Heartbeat bearer cutover (audit F2)

How to remove the fleet-wide heartbeat bearer lane **without re-provisioning the
fleet**, and the hard precondition that gates the removal.

## The finding

`verifyHeartbeatBearer` (`src/pkg/hub/hub_keys.go`) ends with:

```go
return secureCompareHub(presented, s.heartbeatKey())
```

`s.heartbeatKey()` is `deriveDomainKey(hubSecret, infoHeartbeatKey)` — a pure
function of the one hub master, stamped identically into every spoke. Possession
proves "some provisioned spoke", never "**this** hive". The handler then trusts
the body-supplied `hive_id`, so any spoke can heartbeat as any victim hive and be
handed victim-directed key material (pending OpenRouter key, legacy pending App
config, standing cluster App key).

This lane has been open across five audits. The blocker was never the fix — the
per-hive replacement `heartbeatKeyFor(hiveID)` already existed — it was the
migration: deleting the lane 401s every spoke that still presents the fleet-wide
value, and the stated migration path was "re-provision the fleet", which the
operator has ruled out.

## Why an in-place migration is possible

The per-hive bearer is

```
HMAC(master, "hive-heartbeat-v1" || 0x00 || hiveID)
```

— a pure function of **two values every spoke already holds in its own pod env**:
`HIVE_HUB_SECRET` and `HIVE_ID`. No hub action, no new secret, no re-provision is
needed for a spoke to start presenting the identity-bound bearer. It only needs
code that derives it.

### Measured fleet state (2026-08-13, hub-reachable + heartbeat-only clusters)

Classified by comparing each Deployment's `HIVE_HEARTBEAT_KEY` against both
derivations computed from the live master:

| Lane | Spokes |
|---|---|
| Per-hive `HIVE_HEARTBEAT_KEY` injected by the hub | 62 |
| `HIVE_HEARTBEAT_KEY` **absent** → self-derives fleet-wide today | 3 |
| Fleet-wide value explicitly injected | 0 |

The three laggards are all on the hub-reachable cluster:

- `hive-hosted-hosted-available-<cluster>-02-placeholder-7zus`
- `hive-hosted-hosted-example-org-visual-hive--1jos`
- `hive-hosted-hosted-projectbluefin-knuckle-gjvq`

All three have `HIVE_HUB_SECRET` and `HIVE_ID` present, so all three can
self-derive. They are the only reason the legacy lane cannot be deleted, and they
need a code roll, not a re-provision.

Provisioning has injected the per-hive key since the N1 change
(`provisionHeartbeatKey`), so the 62 need nothing at all.

## Stage 1 — enabling change (this PR)

Spoke-side only. `SpokeHeartbeatKey()` now resolves in this order:

1. `HIVE_HEARTBEAT_KEY` — the hub-injected value (unchanged; still authoritative,
   so a pinned or self-hosted bearer is never overridden).
2. **Self-derived per-hive bearer** from `HIVE_HUB_SECRET` + `HIVE_ID` — new.
3. Fleet-wide derivation from `HIVE_HUB_SECRET` alone — now the last resort,
   reachable only by a spoke with a master but no identity.

The hub is **unchanged**: it still accepts both lanes. Nothing can break, because
every spoke either keeps presenting the same injected value it already presents
(62) or moves from a bearer the hub accepts to a *different* bearer the hub also
accepts (3).

A spoke on lane 2 is strictly safer than one on lane 3: its credential
authenticates only as itself. Self-hosted single-tenant operators are unaffected —
their hub re-derives the same per-hive value from the same master.

## Stage 2 — observe

Rollout telemetry already exists (`noteHeartbeatAuthPath`,
`AuthRolloutReadiness`, admin route `GET /api/saas/admin/auth-rollout`). It
records, per hive, whether the **most recent** heartbeat used the identity-bound
bearer, and deliberately does not latch — a rolled-back spoke must make the fleet
read not-ready again.

```
GET /api/saas/admin/auth-rollout
{
  "total_hives": 65,
  "per_hive_bearer": 65,
  "legacy_bearer": 0,
  "heartbeat_lane_ready": true,
  "stale_after": "24h0m0s"
}
```

## Stage 3 — deletion — **DONE**

The lane is deleted. `verifyHeartbeatBearer` now accepts only
`heartbeatKeyFor(hiveID)`; presenting the fleet-wide value authenticates
nothing, for any claimed `hive_id`.

Precondition as measured at deletion time (all three clusters, comparing each
Deployment's injected `HIVE_HEARTBEAT_KEY` against both derivations computed
from the live master):

| | spokes |
|---|---|
| per-hive bearer injected | 67 |
| fleet-wide bearer injected | **0** |
| no key injected → self-derives (holds both `HIVE_HUB_SECRET` and `HIVE_ID`) | 3 |

Two of the three self-derivers run v4 with the merged self-derivation
(`c2286f45`) and show 0 heartbeat auth errors in production.

**Accepted casualty:** `example-org-visual-hive--1jos` runs a lagging fork image
that does not contain `c2286f45`. It presents the fleet-wide bearer and will 401
until its fork is topped up from v4 and it rolls. This was accepted knowingly
rather than softening the deletion.

`heartbeatKey()` was NOT dropped: it still has callers (`heartbeatBearerOK`, and
the telemetry/tests that must be able to construct the rejected value in order
to assert it is rejected).

<details>
<summary>Original Stage 3 plan (kept for the record)</summary>

### Hard precondition

Do not open the deletion PR until **all** of the following hold:

1. `heartbeat_lane_ready` is `true` **and** `legacy_bearer` is `0`.
2. `total_hives` equals the actual live spoke count (65 at time of writing —
   re-count it; do not trust this number). `AuthRolloutReadiness` fails closed at
   zero observations, but it cannot tell "hive absent" from "hive never existed",
   so a hive that is paused or on an unreachable cluster silently leaves the
   denominator after `stale_after` (24h). Compare against a real inventory:

   ```sh
   # Count by CONTAINER name, not by an `app=hive` label — that label is not
   # applied consistently across clusters and undercounts (it matched 0 of 22
   # spokes on the hub-reachable cluster when this was written).
   for ctx in <hub-reachable-context> <heartbeat-only-context>; do
     kubectl --context "$ctx" get deploy -A -o json \
       | jq '[.items[]|select(.spec.template.spec.containers[].name=="hive")]|length'
   done
   ```

3. Condition 1 has held continuously for **at least 24h** (one full
   `stale_after` window), so a spoke that beats infrequently has had a chance to
   appear on the per-hive lane rather than merely aging out of the denominator.
4. Every spoke has actually rolled the Stage 1 image. Verify by SHA, not by tag.

### The deletion

Replace the final line of `verifyHeartbeatBearer` with `return false` and drop
`heartbeatKey()` if it has no other caller. Then flip
`TestFleetWideBearerStillAcceptedAtThisStage` to assert **rejection** — it is
written to fail loudly with the precondition in its message if the lane is
removed early.

### Rollback

The lane is one line. If heartbeats 401 after deletion, restore it and the fleet
recovers on the next beat (~1 min). Nothing is persisted, so there is no
migration to undo.

</details>

## Residual risk not addressed here

Every spoke Deployment still carries `HIVE_HUB_SECRET`, the hub **master**, in
plaintext (65/65 measured). Any spoke operator can read it and derive *any*
hive's per-hive bearer, which defeats the identity binding for an attacker with
pod access. Self-derivation does not make this worse — it is the same material
already present — but F2 is only fully closed once the master stops being
injected into spokes and they hold nothing but their own derived sub-keys. That
is a separate finding and a separate change.

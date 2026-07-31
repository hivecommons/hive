# Spoke configuration layering

**Which layer sets this field, and where do I write to change it?**

Until this document existed, that question was answerable only by reading a
single line of pod boot logs. It cost real time: a GitHub Enterprise hive was
repaired on the **fourth** attempt, after patching the hub's `meta.json`, then
`clusters.json`, then the spoke's ConfigMap — three layers that are *inert* for
`github.app_id` — before patching the dashboard overlay, the layer that
actually wins.

Ask the hive instead:

```bash
curl -s localhost:3002/api/config/provenance | jq .
```

## Precedence order — highest to lowest

| Rank | Layer | Path | Writable by |
|---|---|---|---|
| 1 (wins) | `config-env` | `/etc/hive/config.env` | cluster admin (pod spec) |
| 2 | `agent-overlay` | `/data/agent-configs/<agent>.yaml` | spoke (dashboard agent edits) |
| 3 | `dashboard-overlay` | `/data/hive.yaml.dashboard` | **spoke (`Config.Save`) and hub (heartbeat)** |
| 4 (loses) | `configmap-seed` | ConfigMap `hive-config`, key `hive.yaml` | **nobody** |

Three documented exceptions where a lower layer affects the result:

| # | Field | Behaviour |
|---|---|---|
| A | `hub.is_public` | The seed's value is applied over the overlay's. The **only** unconditional seed-wins assignment in the merge. |
| B | `acmm_level` | The seed is a **fallback only**, used when the overlay has no level at all (issue #1856). |
| C | `github.*` | The seed wins only as an **anti-wipe ratchet**: when the seed holds real App credentials and the overlay's look like a placeholder. A safety catch, not precedence. |

Everything else: the highest layer that sets the field wins.

## The ConfigMap decides almost nothing, and nothing can write it

This is the most important and least obvious fact about the system.

Measured on the live fleet — ConfigMap seed vs running config:

| Hive | Seed | Running | Seed share |
|---|---|---|---|
| `hosted-aslom-hive-agent` | 559 B | 12,380 B | 4.5% |
| `hosted-projectbluefin-knuckle` | 1,661 B | 14,609 B | 11% |
| `hosted-kubestellar-console` | 1,051 B | 17,564 B | 6% |

The seed omits whole top-level blocks (`policies`, `data`, `knowledge`,
`notifications`, `hive_id`) that the running config carries.

Its one exclusive field, `hub.is_public`, is owned by a component that
**cannot write this layer**:

- **hub → ConfigMap**: one reference in `pkg/hub`
  (`saas_provision.go:1282`), and it is a `get`. There is no write path.
- **spoke → ConfigMap**: the spoke Role (`saas_provision.go:1478-1518`) grants
  `deployments` and `secrets`. `configmaps` appears in no rule.

The hub also re-asserts `is_public` over the heartbeat
(`server.go:1271` pushes on disagreement; `server.go:1077` keeps a hub-set
public flag from being reverted), so the seed's win survives at most one beat.

**Consequence:** editing the ConfigMap and restarting usually changes nothing.
It looks authoritative — it is the only layer visible to `kubectl` without
`exec`ing into a pod — and it is not. This is the trap the GHE incident fell
into.

It also explains why every config fix this week travelled by heartbeat into the
PVC overlay: #2354 (ACMM), #2360 (forge), #2363 (GHE App IDs). The overlay is
the only writable channel. The bespoke per-field delivery latches are a
rational response to having one road, not duplicated effort.

## Where to write to change a field

| Field | Winning layer | Write here |
|---|---|---|
| `acmm_level` | dashboard-overlay | overlay, or hub `RequestedACMMLevel` |
| `github.app_id` | dashboard-overlay | overlay (hub source is `clusters.json`) |
| `github.installation_id` | dashboard-overlay | overlay |
| `github.key_file` / PEM | hub (cluster PEM) | `/data/saas/app-keys/<cluster>.pem` |
| `github.app_slug` | hub push | cluster config |
| `github.api_url` / `base_url` | spoke; hub pushes via forge latch | `handleSwitchForge` |
| `github_host` | spoke reports, hub adopts | `handleSwitchForge` |
| agent list | union of seed + overlay + agent-configs | see caveat below |
| agent `model` / `backend` | dashboard-overlay | dashboard (ownership-guarded, #2340) |
| governor thresholds | dashboard-overlay | dashboard |
| `hub.is_public` | **configmap-seed** | hub (the seed is not writable) |

> **Caveat — agent deletion.** `MergeAgentOverrides` unions its three sources
> and only ever *adds*, so a deleted agent reappears on the next config reload.
> Tracked in #2361.

## `hive.yaml.bak` is a backup, not an input

Despite the name suggesting otherwise, and despite what some code comments and
the DR runbook currently say, `/data/hive.yaml.bak` is **written** by the
entrypoint after the merge (`entrypoint.sh:150`) and is **read only** when the
ConfigMap is missing or empty (`entrypoint.sh:152-161`), or in Docker mode.

Three older hives run a `copy-config` init container variant that *does*
restore from `.bak` first. That variant is not what new hives get. The DR
documentation is being corrected separately.

## Reading the provenance report

```jsonc
{
  "fields": [
    {
      "field": "github.app_id",
      "layer": "dashboard-overlay",
      "path": "/data/hive.yaml.dashboard",
      "writer": "spoke (Config.Save) and hub (via heartbeat delivery)",
      "writable": true,
      "value": "5686",
      "also_set_by": ["configmap-seed"]   // a losing, stale value
    }
  ],
  "overlay_rejected": false,
  "identity_issues": []
}
```

- **`also_set_by`** names lower layers that set the field and lost. A
  `configmap-seed` entry here means the ConfigMap holds a stale value that
  looks authoritative and is not.
- **`overlay_rejected`** is the alarming one. It means the overlay failed the
  plausibility guard and was discarded, so the hive is running on the bare
  seed — which, per the table above, can be a small fraction of the real
  config, including a fraction of the agent roster.
- **`identity_issues`** names inconsistencies in the GitHub identity set.

## The GitHub identity set is atomic

`app_id`, `app_slug`, `api_url` and `base_url` are **one identity**. No valid
mixture exists.

| Forge | `app_id` | `app_slug` | `api_url` |
|---|---|---|---|
| GHE | `5686` | `kubestellar-hive-ghe` | `https://github.ibm.com/api/v3` |
| Public | `3568013` | `kubestellar-hive` | empty (defaults to `api.github.com`) |

Delivering a GHE `app_id` **without** `api_url` leaves `api_url` empty, which
defaults to public GitHub, and every token request fails:

```
POST https://api.github.com/app/installations/<id>/access_tokens
404 Integration not found
```

This happened on live hives. `identity_issues` in the provenance report names
the condition directly.

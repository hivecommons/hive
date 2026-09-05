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

## `hive.yaml.runtime` — a snapshot on Kubernetes, an input everywhere else

This file was called `hive.yaml.bak` until the rename. The old name implied
"the restorable backup", which is true of only half its behaviour, and the
ambiguity cost real debugging time.

**On Kubernetes it is a snapshot.** The entrypoint *writes* it after the merge
and *reads* it only when the ConfigMap is missing or empty — the disaster
fallback. A minority of older hives run a `copy-config` init container variant
that does restore from it first; that variant is not what new hives get.

**Outside Kubernetes it is a live boot input, and the source of truth.** There is no
ConfigMap and no overlay in that mode, so the entrypoint restores this file over
the config path on every boot (or points `HIVE_CONFIG` straight at it, and
pins the same path into the launch argv as `--config`, when the config path is
read-only — the argv half is required, because the image's `CMD` passes an
explicit `--config` that would otherwise outrank the variable, [#4973](https://github.com/hivecommons/hive/issues/4973)). It is also the only reason a dashboard save survives
a container recreation: `Config.saveDashboardOverlay` deliberately early-returns
outside Kubernetes because this file already plays that role.

> An earlier version of this section was headed "`hive.yaml.bak` is a backup,
> not an input" and mentioned Docker only in passing. That was wrong for every
> non-Kubernetes hive, where the file is precisely an input.

### Which mode am I in?

There is no Docker-specific or Podman-specific code path. The entrypoint and
`config.IsKubernetesPod()` both ask one question — *is this a Kubernetes pod?*
— and everything that is not one takes the same branch:

```sh
# src/deploy/entrypoint.sh
if [ -n "${KUBERNETES_SERVICE_HOST:-}" ] || [ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
  IS_KUBERNETES=true
```

So **Docker, Podman (rootful or rootless) and LXC all behave identically here**,
as does any other container runtime and a bare binary on a host. This document
says "outside Kubernetes" rather than naming runtimes, because naming a subset
of them is how a Podman operator concludes the section is about someone else's
deployment ([#5220](https://github.com/hivecommons/hive/issues/5220)).

Ask the running hive which file actually decides a field:

```bash
# Same endpoint as above; use whichever port this hive serves the dashboard on
# (3002 direct, or 3001 when an nginx gateway fronts it).
curl -s localhost:3002/api/config/provenance | jq '.fields[] | select(.field=="acmm_level")'
```

```jsonc
{
  "field": "acmm_level",
  "layer": "configmap-seed",        // the layer's stable contract name…
  "path": "/data/hive.yaml.runtime", // …but outside Kubernetes this is the real file (#4971)
  "writer": "spoke (Config.Save)",
  "writable": true,
  "value": "4"
}
```

Note the `layer` name stays `configmap-seed` on a host that has no ConfigMap:
the layer identity is a stable part of the provenance contract, while `path`
and `writer` are the environment-aware fields that tell you where to write.
Trust `path`, not the layer name.

Two consequences worth stating outright for a non-Kubernetes hive:

- **The bind-mounted seed at `/etc/hive/hive.yaml` goes stale and stays stale.**
  A dashboard change to `acmm_level` is written to `/data/hive.yaml.runtime`;
  nothing writes it back to the seed. Reading the seed to find out what level a
  hive is running will mislead you. This is by design (issue #1856 — letting
  the seed win reverted a hive to its provisioned level on every restart), not
  drift to be repaired.
- **`/data` is the only copy of the live config.** Losing that volume reverts
  the hive to whatever the seed was provisioned with, silently downgrading
  every hold-gated agent. Back up the volume, not just the seed file.

### Migration

The rename is **copy-forward, never destructive**. Writers emit
`/data/hive.yaml.runtime`; readers prefer it and fall back to
`/data/hive.yaml.bak` when it is absent. Nothing renames or deletes the legacy
file on a PVC — outside Kubernetes it is the single copy of the live config, so
mutating it at boot could lose owner customisations with no warning.

A hive booting new code with only the legacy file present therefore boots
normally from that file, and gains the new name on the next config save (on
non-Kubernetes, the entrypoint also copies it forward immediately). Backups capture
both names for the same reason. The legacy fallback can be removed one release
after every live hive has written the new name.

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
  config, including a fraction of the agent roster. A rejection is also logged
  at `ERROR` naming the specific cause.
- **`last_good_used`** means a rejected overlay was replaced by the rolling
  last-good config (`/data/hive.yaml.bak`) rather than by the sparse seed. The
  config in effect is then neither the overlay the operator last wrote nor the
  seed, so it is reported explicitly.

### What makes an overlay valid

The guard catches a **truncated or corrupt** overlay — not an unusual one.
To check a config — overlays included — before booting into it, run
[`hive validate`](troubleshooting.md#validate-without-booting-hive-validate):
it loads exactly what a real boot would and exits `0`/`1` without starting
anything.

| Overlay | Verdict |
|---|---|
| `project.org` + one or more agents | valid |
| Zero agents **with** `removed_agents` tombstones | **valid** — a deliberate deletion (#2361) |
| All agents paused | valid |
| Missing `policies` / `data` / `knowledge` / `hive_id` | valid — all optional |
| No `project.org` | rejected — truncated write |
| Zero agents **and** no tombstones | rejected — truncated write |

The tombstone distinction matters: before it existed, deleting your last agent
produced a zero-agent overlay that the guard called corrupt, so the hive booted
from a seed that still listed the deleted agents — silently undoing the
deletion.
- **`identity_issues`** names inconsistencies in the GitHub identity set.

## The GitHub identity set is atomic

`app_id`, `app_slug`, `api_url` and `base_url` are **one identity**. No valid
mixture exists.

| Forge | `app_id` | `app_slug` | `api_url` |
|---|---|---|---|
| GHE | `5686` | `kubestellar-hive-ghe` | `https://<your-ghe-host>/api/v3` |
| Public | `3568013` | `kubestellar-hive` | empty (defaults to `api.github.com`) |

Delivering a GHE `app_id` **without** `api_url` leaves `api_url` empty, which
defaults to public GitHub, and every token request fails:

```
POST https://api.github.com/app/installations/<id>/access_tokens
404 Integration not found
```

This happened on live hives. `identity_issues` in the provenance report names
the condition directly.

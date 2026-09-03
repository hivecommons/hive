# Migrating a deployment from v2 to v4

This guide is for operators running a Hive deployment built from the `v2` branch
who want to move it to `v4`, the active development line.

Every claim below was derived by diffing the two branches, not from release
notes. Where something was verified by running it, that is stated. Where it was
not, that is stated too — see [Limits](#limits) before you touch production.

## The short version

The configuration file is the part operators worry about most, and it is the
part that does not change: **a v2 `hive.yaml` loads on v4 unmodified.** The v4
schema is additive over v2 — no key was renamed or removed.

What actually changes is the image tag, one published port, and (if you build
from source) the directory layout:

| | v2 | v4 |
| --- | --- | --- |
| Compose image | `ghcr.io/kubestellar/hive:v2-latest` | `ghcr.io/kubestellar/hive:stable` |
| Kubernetes image | `hive:latest` | `ghcr.io/kubestellar/hive:stable` |
| Compose published ports | `3001`, `7681` | `3001` only |
| Source tree | `v2/` | `src/` |
| Build context | `v2/Dockerfile` | `src/Dockerfile` |

## Branch and tag status

`v4` is the active branch: at the time of writing it carries 652 commits the
`v2` branch does not have. `v2` is in low-rate maintenance rather than fully
stopped — it still receives occasional backports — but it is not where
development happens, and new work targets `v4`
(see [CONTRIBUTING.md](https://github.com/hivecommons/hive/blob/v4/CONTRIBUTING.md)).

The `v2-latest` image tag belongs to the `v2` branch and **resolves to a
different, older digest than the v4 channels**. It is not a rolling tag that
will carry you forward; moving to v4 means changing the tag, not waiting for it
to update.

## Configuration: no changes required

The v4 config loader accepts a v2 configuration file as-is. This was verified by
loading the `v2` branch's own shipped `v2/hive.yaml.example` through v4's
`config.Load()`: it parsed cleanly, yielding the same project org, both
repositories, and all ten agents.

A key-by-key comparison of the two shipped examples found **no key present in v2
that is absent in v4**. v4 only adds:

- the `ioscan:` block (the untrusted-input scanner — see
  [ioscan.md](ioscan.md)), with `fail_mode`, `warn_threshold`,
  `block_threshold`, `canaries`, and `classifier`

Because these are additions, omitting them keeps v4's defaults. You do not need
to add an `ioscan:` block to start on v4.

> Your own `hive.yaml` may contain keys that neither shipped example does. The
> statement above is about the documented schema; re-run your config through a
> v4 binary (`hive --config <path>` fails fast on a bad config) before cutting
> over.

## Docker Compose

### Port 7681 is no longer published — this is deliberate

v2's compose file published both `3001` and `7681`. v4 publishes only `3001`.

`7681` is the raw ttyd terminal: an unauthenticated, writable shell into the
container that holds your GitHub credentials. v4 reaches the terminal through
`:3001/terminal`, which authenticates first.

**If any of your tooling connects to `7681` directly, it will stop working, and
that is the point.** Re-point it at `:3001/terminal`. Do not re-add the port
mapping to "fix" the breakage — doing so restores an unauthenticated shell.

### Image and build context

```diff
-    image: ghcr.io/kubestellar/hive:v2-latest
+    image: ghcr.io/kubestellar/hive:stable
     build:
       context: ..
-      dockerfile: v2/Dockerfile
+      dockerfile: src/Dockerfile
```

`stable` is the operator-blessed release channel. For a reproducible deployment,
pin a digest instead — `ghcr.io/kubestellar/hive@sha256:<digest>` — and manage
upgrades yourself.

### Other compose changes worth knowing

- The `nginx:alpine` gateway image is pinned by digest as well as tag. Refresh
  both together; the file documents the exact commands.
- v4 adds a filtered Docker API proxy in front of Watchtower for the optional
  auto-update profile, so Watchtower no longer receives the full daemon socket.
  See [auto-update-profile.md](auto-update-profile.md) before enabling it.
- Standalone image references now come from one source of truth,
  [`src/deploy/standalone-images.sh`](https://github.com/hivecommons/hive/blob/v4/src/deploy/standalone-images.sh),
  which a build test enforces.

## Kubernetes

Container ports are unchanged (`3002`). The security posture is tightened, and
the changes are not optional — the manifest will not grant what the entrypoint
needs unless you carry them across.

**Image**

```diff
-          image: hive:latest
+          image: ghcr.io/kubestellar/hive:stable
```

**Pod securityContext** — `fsGroup: 1002` is added:

```diff
       securityContext:
-        seccompProfile:
-          type: RuntimeDefault
+        fsGroup: 1002
```

`1002` is the `hive-launch` GID pinned in `src/Dockerfile`. Paired with
`defaultMode: 0440` on the secrets projection (also new), it makes the GitHub
App private keys readable by `dev` while excluding the agent UIDs (2001+). Mode
alone cannot achieve that split, because `dev`'s primary group is shared with
every agent UID. If you copy `fsGroup` across but not `defaultMode`, you lose
the exclusion.

**Container securityContext** — `seccompProfile` moves here from the pod level,
and the capability set inverts from "drop one" to "drop all, add back eight":

```diff
           securityContext:
+            seccompProfile:
+              type: RuntimeDefault
             capabilities:
               drop:
-                - NET_RAW
+                - ALL
+              add:
+                - NET_ADMIN   # ACMM proxy iptables redirect of outbound :443
+                - SETUID      # su-exec dev->per-agent UID switch
+                - SETGID
+                - SETPCAP
+                - CHOWN       # chown per-agent dirs/beads to the agent UID
+                - DAC_OVERRIDE
+                - FOWNER      # entrypoint chmod -R g+rwX over dev-owned /data/home
+                - FSETID      # entrypoint chmod g+s on dev:node dirs
```

`NET_ADMIN` is load-bearing: without it the forced-proxy egress gate cannot
install its iptables redirect, and the container refuses to start rather than
running unenforced. See
[net-admin-requirement.md](net-admin-requirement.md).

## If you build from source

The `v2/` directory was renamed to `src/`, and the Go module dropped its `/v2`
path suffix. The files inside kept their names — `v2/deploy/nginx.conf` is now
`src/deploy/nginx.conf`, and so on — so most breakage is a path prefix, not a
restructure.

Update any of your own scripts, bind mounts, or CI that reference `v2/` paths.

## Upgrade procedure

This procedure is derived from the manifests. **Rehearse it on a non-production
hive first** — see [Limits](#limits).

1. **Back up first.** Use the documented path in
   [backup-restore.md](backup-restore.md). Your `/data` volume carries agent
   state, beads, and contributor records.
2. **Read your config once with a v4 binary** before cutting over, so a config
   problem surfaces on your terms rather than during a restart.
3. **Compose:** change the image tag to `stable`, remove the `7681` port
   mapping, and update `dockerfile:` to `src/Dockerfile` if you build locally.
   Then `docker compose up -d`.
4. **Kubernetes:** apply the image, `fsGroup`, secret `defaultMode`, and
   capability changes together. Applying the capability drop without the `add:`
   list will leave the container unable to install the egress gate.
5. **Re-point anything that used `:7681`** at `:3001/terminal`.

## Verifying the upgrade

- The container reaches steady state rather than restart-looping. A fail-closed
  egress gate exits `77` (`EX_NOPERM`) when `CAP_NET_ADMIN` is missing — that is
  the signature of a capability list that did not carry across.
- The dashboard answers on `:3001`.
- `:7681` is no longer reachable from outside the container, and
  `:3001/terminal` prompts for authentication.
- Your agents appear with the same names and stats as before the upgrade.

## Limits

Be aware of what this guide is and is not:

- **No production v2→v4 upgrade was performed to write it.** Every statement
  comes from diffing the two branches and from loading a v2 config with a v4
  loader. The procedure above is a reading of the manifests, not a rehearsed
  runbook.
- **`/data` compatibility was not tested end-to-end.** No data-migration step
  exists anywhere in the v4 tree, which is why none is listed here — but
  "no migration code" is weaker evidence than a tested restore. Back up first.
- **Your config is not the shipped example.** The compatibility result covers
  the documented schema.
- **Hub/spoke, hosted, and self-hosted fleets differ.** This covers the
  standalone Compose and Kubernetes assets in the repository. Hosted spokes are
  provisioned by the hub and are not upgraded by hand.
- **The two branches keep moving.** Re-diff before a large migration.

If you hit something this guide got wrong, please file an issue — a correction
from a real upgrade is worth more than the analysis above.

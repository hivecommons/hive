# Moving a Hive between hosts, same runtime

Part of [#6521](https://github.com/hivecommons/hive/issues/6521). Closes
[#6522](https://github.com/hivecommons/hive/issues/6522) (Docker Compose →
Docker Compose), [#6523](https://github.com/hivecommons/hive/issues/6523)
(Podman Quadlet rootless → rootless, different `/etc/subuid` base), and
[#6524](https://github.com/hivecommons/hive/issues/6524) (Podman Quadlet
rootful → rootful).

## Status of everything on this page

**DOCUMENTED, NOT EXECUTED.** Every procedure below is written from reading
`src/deploy/entrypoint.sh`, the Compose and Quadlet unit files, and the
existing same-host backup/restore evidence in
[backup-restore.md](backup-restore.md); none of it was run against two
distinct hosts in this environment (this agent has one filesystem, not two
machines). Where a step reuses a command sequence that *was* executed
same-host in `backup-restore.md`, that fact is cited so you know which half of
the risk is retired and which half — the cross-host part — is new. Nothing
here is labeled OBSERVED. If you run this procedure, please record what
actually happened (exit codes, timings, `hive-id` before/after) back into this
file or a linked issue, the way `backup-restore.md`'s "Podman: what was
observed" section does.

## What is host-bound and must change on the target

This is common to all three runtimes below, and to the cluster-move and
hub-registered cases documented elsewhere. Read this section once.

| What | Where it lives | What changes on a host move |
| --- | --- | --- |
| Dashboard URL / published port | `HIVE_DASHBOARD_TOKEN` gates the proxy; `hive-gateway` publishes `3001:3001` in Compose ([`src/docker-compose.yaml:13-19`](../docker-compose.yaml)) and via `PublishPort=3001:3001` in Quadlet ([`src/deploy/quadlet/hive-gateway.container:73`](../deploy/quadlet/hive-gateway.container)) | The port binding is host-local. If the target's reachable hostname/IP differs from the source's, anything bookmarking the old URL (browser bookmarks, a reverse proxy, DNS) must be repointed. Nothing inside the hive-data volume encodes the URL for a standalone (non-hub-registered) deployment — see the next row for the hub-registered case. |
| TLS termination | Not done by `hive` or `hive-gateway` themselves; both Compose and Quadlet publish plain HTTP on `3001` and expect a reverse proxy or the operator's own TLS in front | A move does not carry over externally-managed certificates or reverse-proxy config. Re-provision TLS on the target before pointing traffic at it. |
| GitHub App callback / Setup URL | Configured on the GitHub App itself (not in the hive's data), per [github-app-setup.md](github-app-setup.md) — "permissions, Setup URL, and `/gh-setup`" | If the target is reachable at a different public hostname than the source, the GitHub App's Setup URL and any OAuth callback URL registered with GitHub must be updated on the GitHub side, or `/gh-setup`-driven flows and OAuth logins will redirect to a host that no longer serves them. The App's private key itself (`gh-app-key*.pem`) is data, not host state — see the per-runtime sections below for why its SHA-256 must be unchanged. |
| Hub heartbeat / `dashboardUrl` (hub-registered hives only) | `hub.dashboard_url` in `hive.yaml`; the hub registry's `dashboardUrl` field is heartbeat-owned, not operator-editable — see [cross-cluster-migration.md](cross-cluster-migration.md#2-create-the-service-and-routes) ("the heartbeat owns `dashboardUrl`") | **Out of scope for this page.** If this hive reports to a hub, the identity/cutover/heartbeat mechanics (avoiding duplicate heartbeats, the hub picking up the new `dashboard_url`) are the concern of [move-hub-registered-cutover.md](move-hub-registered-cutover.md), which #6526 is adding concurrently. Read that doc first if `hub.dashboard_url` (or any `hub.*` block) is set in this hive's config; the procedures below only cover a hive that is not hub-registered, or where you accept the hub will see a heartbeat gap during the move. |
| Agent backend credentials (Claude, Copilot, Codex, Gemini, Bob) | Under `/data/home/.claude`, `/data/home/.copilot`, `/data/home/.codex`, `/data/home/.gemini`, `/data/home/.bob`, `/data/config/github-copilot`, seeded/chowned by the entrypoint ([`src/deploy/entrypoint.sh:937-955`](../deploy/entrypoint.sh)) | **These live under `/data`**, so they move with the volume/bind-mount archive described per-runtime below — `backup-restore.md` calls `home/` "regenerable bulk state" and explicitly allows excluding it from a backup, but that's a size/scope tradeoff for disaster-recovery archives, not a statement that it's safe to skip on a host move. Skip `/data/home` on a move and every agent backend re-authenticates from scratch on first use on the target. Archive it if you want to avoid that. |
| Image tag / digest | `docker-compose.yaml` and the Quadlet `hive.container` both pin `ghcr.io/hivecommons/hive:stable` (or a digest drop-in, [`src/deploy/quadlet/hive.container:88-91`](../deploy/quadlet/hive.container)) | **UNCONFIRMED as a hard requirement**, framed as a question per epic item #5: nothing found in the entrypoint or config-migration code that fails a restore against a newer image, but no migration/version-check code was found in `entrypoint.sh` either — it treats `/data/hive.yaml.runtime` as authoritative and restores it verbatim over the config path ([`src/deploy/entrypoint.sh:1939-1942`](../deploy/entrypoint.sh)). Safest assumption until tested: pull the **same digest** on the target that the source was running, then upgrade deliberately afterward. |

## Preflight checklist for the target host (all runtimes)

Derived from `bin/hive-podman-preflight-ids.sh` / `bin/hive-podman-preflight-host.sh`
(documented in [podman-preflight-ids.md](podman-preflight-ids.md) and
[podman-preflight-host.md](podman-preflight-host.md); Podman-specific items
marked) and from the install prerequisites in
[podman-standalone-quadlet.md](podman-standalone-quadlet.md). Run before
restoring anything on the target:

- [ ] **Engine installed and matching mode.** Docker on the target for #6522;
  Podman with the correct root mode (rootless/rootful) on the target for
  #6523/#6524 — the two root modes are not interchangeable hosts (different
  storage paths, different UID mapping; see the per-runtime sections).
- [ ] **(Podman only) Subordinate UID/GID delegated**, rootless only:
  `HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight-ids.sh` — checks
  `/etc/subuid`/`/etc/subgid` have at least `HIVE_PODMAN_MIN_SUBID_COUNT`
  (default 65536) for the invoking user. **Do not assume this matches the
  source host's range** — the whole point of #6523 is that it usually won't.
  Record the target's actual range (`grep "^$(whoami):" /etc/subuid`) before
  restoring; you'll need it to reason about ownership below.
- [ ] **(Podman only) Graphroot on a local filesystem**, not NFS/CIFS/9p/etc —
  same script, check 2. Rootless storage defaults under `$HOME`; an NFS home
  directory silently breaks overlayfs extraction.
- [ ] **(Podman only) Rootless networking helper installed** (`pasta` or
  `slirp4netns`, matching what the engine names) — same script, check 3.
- [ ] **(Podman only) SELinux labeling consistent with the kernel mode**:
  `HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight-host.sh` — an
  enforcing kernel with Podman labeling disabled denies every bind mount with
  `:z`/`:Z` doing nothing (`podman-preflight-host.md`, section 1).
- [ ] **(Podman only) `%E/hive` config/secrets paths exist and are readable**:
  same script, section 3 — missing `hive.yaml` or `nginx.conf` fails the
  check; the secrets directory needs the traverse bit for GID 1002
  (`hive-launch`), not merely narrow permissions — see the rootless-specific
  callout in the #6523 section below.
- [ ] **(Podman rootless only) Unprivileged port floor** allows the published
  port: `sysctl net.ipv4.ip_unprivileged_port_start` must be ≤ 3001, or raise
  it — `podman-preflight-host.md`, section 4. Not applicable rootful.
- [ ] **(Podman rootless only) Lingering enabled** so the user's systemd
  `--user` services survive logout: `loginctl enable-linger $(whoami)` —
  referenced by `podman-standalone-quadlet.md`'s install path; not checked by
  either preflight script, listed here because a move that lands the service
  in a session-bound state defeats the point of moving it.
- [ ] **Target port free / firewalled the same way as source** — preflight's
  port check (`HIVE_PODMAN_PREFLIGHT_PORTS`, default `3001`) only tells you
  whether it's occupied on the target, not whether your firewall/security
  group allows the traffic your users expect; that's environment-specific and
  outside what any script here checks.
- [ ] **CPU architecture matches the image** you intend to run (epic item #15)
  — not checked by either preflight script; verify manually (`uname -m`) if
  source and target might differ (e.g. moving off an arm64 host).
- [ ] **No source and target both live at once with the same identity** — see
  "Stop source before starting target" in each section below.

## Docker Compose → Docker Compose (#6522)

### What must move, cited to the deployment files

The Compose deployment persists three things
([backup-restore.md](backup-restore.md#durable-state)):

- the named volume `hive-data` (prefixed `src_hive-data` under the default
  project name — [`src/docker-compose.yaml:59-62`](../docker-compose.yaml)),
  mounted at `/data`, holding `hive-id`, `hive-state.json`,
  `gh-app-key*.pem`, `beads/`, `/data/home/*` (agent backend credentials —
  see the shared table above), and the runtime config overlay
  `hive.yaml.runtime` / `hive.yaml.dashboard` restored over `/etc/hive/hive.yaml`
  at every boot ([`src/deploy/entrypoint.sh:69`](../deploy/entrypoint.sh),
  [`src/deploy/entrypoint.sh:1939-1942`](../deploy/entrypoint.sh));
- the host directory `./secrets`, bind-mounted read-only at `/secrets`
  ([`src/docker-compose.yaml:62`](../docker-compose.yaml));
- the host file `./hive.yaml`, bind-mounted at `/etc/hive/hive.yaml`
  ([`src/docker-compose.yaml:60`](../docker-compose.yaml)).

`/data/hive.yaml.dashboard` (the dashboard save overlay) lives inside the
volume already, so archiving `hive-data` carries it — no separate step needed,
unlike the config-file case in the Podman sections below where `hive.yaml`
lives outside the volume too.

### Open questions answered

- **#8 (host-level tar pattern)**: `backup-restore.md`'s "Host-level backup
  pattern" (`docker run --rm -v src_hive-data:/data ... alpine tar czf ...`) is
  a plain root-owned container doing the tar, with no user-namespace
  complication — Compose has no rootless mode. Nothing in the entrypoint or
  the compose file suggests this wouldn't round-trip; it is the same shape as
  the Docker side of the Docker→Podman migration in `backup-restore.md`,
  which **was** executed and did round-trip. Still DOCUMENTED, NOT EXECUTED
  as a two-host cycle specifically.
- **#12 (host-bound)**: answered by the shared table above — published port,
  any TLS/reverse-proxy in front, and (if hub-registered) heartbeat/dashboard
  URL. Nothing else was found bound to the source host in
  `docker-compose.yaml` or `entrypoint.sh`.
- **#13 (agent backend credentials)**: under `/data/home/*`, inside the
  volume — see the shared table. Skipping the volume archive (impossible,
  since it also holds `hive-id`) or specifically excluding `home/` from it
  forces re-authentication on the target.
- **#14 (backup key escrow)**: the dashboard-set backup encryption key is
  written to `/data/secrets/backup_encryption_key`
  (`backup-restore.md`, "Setting the backup encryption key"), i.e. inside the
  same volume this procedure archives — it moves with the volume archive.
  Escrow a copy of it outside both hosts before starting, the same as any
  other disaster-recovery key: if the archive step below fails partway and
  you lose both hosts, an un-escrowed key makes any *other* encrypted archive
  (a `hive-backup`/spoke-backup archive taken earlier) unrecoverable.
- **#5 (image digest)**: see the shared table — UNCONFIRMED as a hard
  requirement; pin the same digest on the target as a precaution.

### Procedure

1. On the **source**, confirm current identity for later comparison:
   ```bash
   docker compose -f src/docker-compose.yaml exec hive cat /data/hive-id
   docker compose -f src/docker-compose.yaml exec hive sh -c 'sha256sum /data/gh-app-key*.pem'
   ```
2. **Stop the source before archiving.** `docker compose stop` (not `down -v`)
   leaves the volume in place while quiescing writes to `hive-state.json` and
   `beads/`:
   ```bash
   docker compose -f src/docker-compose.yaml stop
   ```
3. Archive the volume and the two host paths, per
   [backup-restore.md](backup-restore.md#host-level-backup-pattern):
   ```bash
   docker run --rm -v src_hive-data:/data -v "$(pwd)":/backup alpine \
     tar czf /backup/hive-data-$(date +%F).tar.gz -C /data .
   tar czf hive-config-$(date +%F).tar.gz ./src/hive.yaml ./src/secrets
   ```
4. Copy both archives to the target host (`scp`, or your usual transfer
   mechanism — out of scope here).
5. On the **target**, with the repo checked out at the same relative layout:
   ```bash
   docker volume create src_hive-data
   docker run --rm -v src_hive-data:/data -v "$(pwd)":/backup alpine \
     tar xzf /backup/hive-data-<date>.tar.gz -C /data
   tar xzf hive-config-<date>.tar.gz   # restores ./src/hive.yaml and ./src/secrets
   docker compose -f src/docker-compose.yaml up -d
   ```
6. **Verify before destroying the source** (see "Post-restore verification"
   below).
7. Only after verification passes, tear down the source:
   ```bash
   docker compose -f src/docker-compose.yaml down -v
   ```

## Podman Quadlet rootless → rootless, different `/etc/subuid` base (#6523)

This is the case `backup-restore.md` explicitly flags as not executed: "restore
onto a **different** host, with a different `/etc/subuid` base — not
executed". Same-host rootless backup/restore (identical subuid range on both
sides of the restore, because it's the same host) **is** OBSERVED there; this
section is about what changes when it isn't the same range.

### The concrete chown / `podman unshare` implications

Read together, `backup-restore.md`'s "Rootless: never `tar` the volume from
the host shell" section and `podman-preflight-ids.md`'s subordinate-ID
explanation give a precise mechanism, not just a warning:

- A rootless container's UIDs are remapped through the host's delegated
  subordinate range: on the source host measured in `backup-restore.md`,
  `/etc/subuid` reads `dbaggett:524288:65536`, so container UID 1001 (`dev`)
  is host UID `524288 + 1001 = 525289`... (backup-restore.md's own arithmetic
  gives `525288`/`525287` for UID 1001/GID 1000 respectively — the exact
  offset depends on whether the range starts at the base or base-1; **use
  `podman unshare cat /proc/self/uid_map` on each host to read the real
  mapping rather than computing it by hand**, since off-by-one errors here are
  exactly the failure mode this section exists to avoid).
- **The namespace-aware export forms are what make the archive
  host-independent, and this is the whole point of using them.** `podman
  volume export`, the alpine-container form, and `podman unshare tar` (all
  three, per `backup-restore.md`) each write the archive with **container-side**
  UIDs/GIDs (`1001`/`1000`), not host-mapped ones (`525288`/`525287`). A
  plain host-shell `tar -C "$VOLUME_PATH" .` instead would bake in the
  **source** host's mapped IDs (`525288`/`525287`) — `backup-restore.md`
  calls this out as the reason not to do it, but doesn't spell out what
  happens on extraction to a *different* mapping: a target host whose
  `/etc/subuid` starts at, say, `dbaggett:100000:65536` maps container UID
  1001 to host UID `101001`. An archive that recorded host UID `525288` for
  that file, extracted with a plain host-shell `tar x`, would leave a file on
  disk owned by host UID `525288` on the target — which the target's own
  user namespace maps to a **different, wrong container UID** (or no UID the
  container has ever heard of, if `525288` isn't inside the target's mapped
  range at all). The container would then either see the wrong file owner or
  fail to read it, and neither failure looks like a subuid problem from
  inside the container.
- Using `podman volume export` (or the other two namespace-aware forms) on
  the source and `podman volume import` on the target sidesteps this
  entirely: the archive holds container UID/GID `1001`/`1000`, and **each
  host's own Podman re-applies its own current subuid mapping on import**,
  independent of what the source's mapping was. This is the concrete reason
  the different-subuid-base case is not actually harder than the same-host
  case for the volume — it's the same three commands from
  `backup-restore.md`, and the subuid difference is invisible to them by
  construction.
- The `%E/hive/secrets` directory is a different story, because it is a
  **bind mount**, not a namespace-remapped volume: the container reads it as
  `dev` (UID 1001) through the `hive-launch` supplementary group (GID 1002),
  but the *host-side* ownership needed to make that work is
  `subgid_start + 1001` for the group — i.e. it genuinely differs per host.
  `podman-preflight-host.md` gives the exact remediation, and it must be
  **re-run on the target with the target's own mapping**, not copied from the
  source:
  ```bash
  chmod 0750 ~/.config/hive/secrets
  podman unshare chown -R 0:1002 ~/.config/hive/secrets
  ```
  `podman unshare` is what makes this host-independent: it runs the `chown`
  *inside* the invoking user's own namespace, so `0:1002` is resolved against
  **whichever host it's run on**'s mapping automatically. Running a plain
  `chown 0:1002` from the host shell instead — the mistake `podman unshare`
  exists to prevent — would target host GID 1002 literally, which on a
  rootless host is either some unrelated system group or nothing at all, not
  the container's `hive-launch` group.

### Open questions answered

- **#4 (different subuid base)**: answered above — the volume side is a
  non-issue given the namespace-aware export/import forms; the secrets side
  needs `podman unshare chown` re-run per-host, not copied.
- **#7 (target host with no Docker installed)**: **not demonstrated here**,
  matching the same caveat in `backup-restore.md`'s "Not executed" table. The
  procedure below uses no Docker component (the alpine-container step uses
  `docker.io/library/alpine` as an *image reference* pulled by Podman, not
  Docker itself, matching `backup-restore.md`'s existing usage), so nothing
  in it should require Docker, but that has not been verified on a Docker-free
  host.
- **#12 (host-bound)**: shared table above, plus SELinux MCS categories are
  explicitly **not** something to restore — `backup-restore.md` observed
  different category pairs (`c269,c605` vs `c580,c750`) on two rootless
  deployments and states the pair belongs to the mount, stamped fresh by
  Podman on every mount, not to the backup.
- **#13, #14**: same answers as the Compose section — `/data/home/*` for
  agent credentials (moves with the volume export/import), backup key under
  `/data/secrets/backup_encryption_key` (moves with it too, still escrow a
  copy externally).
- **#15 (target prerequisites)**: the preflight checklist above, run with
  `HIVE_DEPLOY_RUNTIME=podman`, is exactly this — subuid/subgid range,
  graphroot filesystem, networking helper, SELinux/labeling, secrets
  traversability, port floor, lingering.
- **#5 (image digest)**: same UNCONFIRMED answer as Compose.
- **#11 (concurrent identity)**: see "Stop source before starting target"
  below — no code found anywhere that detects or rejects a duplicate
  `hive-id` running concurrently on two hosts; this is enforced by the
  operator's procedure, not by the software.

### Procedure

1. On the **source**, confirm each host's actual subuid mapping and current
   identity:
   ```bash
   podman unshare cat /proc/self/uid_map
   grep "^$(whoami):" /etc/subuid /etc/subgid
   podman exec hive cat /data/hive-id
   podman exec hive sh -c 'sha256sum /data/gh-app-key*.pem'
   ```
2. **Stop the source before archiving** — not destructive
   ([backup-restore.md](backup-restore.md#docker-compose-down--v-and-its-podman-counterpart)):
   ```bash
   systemctl --user stop hive.service
   systemctl --user stop hive-gateway.service
   ```
3. Export the volume with a namespace-aware form (per
   [backup-restore.md](backup-restore.md#rootless-never-tar-the-volume-from-the-host-shell)):
   ```bash
   podman volume export hive-data -o hive-data-$(date +%F).tar
   ```
4. Archive the config/secrets tree:
   ```bash
   CONF=~/.config/hive
   tar czf hive-config-$(date +%F).tar.gz -C "$(dirname "$CONF")" "$(basename "$CONF")"
   ```
5. Copy both archives to the target.
6. On the **target**, check the target's own subuid range first (it will
   likely differ — that's this issue):
   ```bash
   grep "^$(whoami):" /etc/subuid /etc/subgid
   ```
   Run the full preflight checklist above with `HIVE_DEPLOY_RUNTIME=podman`.
7. Recreate the volume **through its unit**, not `podman volume create`
   directly — `backup-restore.md` documents that only the unit-created volume
   carries the ownership labels `bin/hive-podman-teardown.sh` and
   `bin/hive-podman-setup.sh` rely on:
   ```bash
   systemctl --user start hive-data-volume.service
   podman volume import hive-data hive-data-<date>.tar
   ```
8. Extract config/secrets, then re-apply secrets ownership **with the
   target's own mapping** — this is the step that must not be skipped or
   copied from the source host:
   ```bash
   tar xzf hive-config-<date>.tar.gz -C "$(dirname "$CONF")"
   chmod 0750 "$CONF/secrets"
   podman unshare chown -R 0:1002 "$CONF/secrets"
   ```
9. Start and verify (see below) before destroying the source:
   ```bash
   systemctl --user start hive.service
   systemctl --user start hive-gateway.service
   ```
10. Only after verification passes:
    ```bash
    systemctl --user stop hive.service hive-gateway.service
    bin/hive-podman-teardown.sh plan     # confirm what would be removed
    bin/hive-podman-teardown.sh run --yes
    ```

## Podman Quadlet rootful → rootful (#6524)

### What's different from rootless, concretely

Rootful has no user namespace, so host UIDs are container UIDs throughout —
`backup-restore.md` observed `uid=1001 gid=1000` on the volume the whole way
through a rootful backup/restore cycle, with no remapping step. This removes
the subuid-mapping problem #6523 has to solve: there is no per-host mapping to
recompute, so an export taken with a plain host-shell `tar` (run as root)
already records the identity the target's container will also see, **as long
as the target doesn't already have a conflicting UID 1001 / GID 1000 assigned
to something else** — which is the different risk this issue's scope
description raises, and is a straightforward host-administration check
(`id 1001`, `getent group 1000` on the target) rather than a namespace
mechanism. `backup-restore.md` recommends using `podman volume export`/`podman
unshare` even rootful anyway, "to keep one procedure for both modes" — that
recommendation still holds here for consistency, not because rootful needs it.

### Open questions answered

- **#15 (target prerequisites, rootful-specific)**: `podman-preflight-ids.md`
  and `podman-preflight-host.md` both note several checks are pass-through or
  skipped for a rootful engine — no subuid/subgid check (rootful has none),
  no rootless networking-helper check, no unprivileged-port-floor check.
  What still applies: SELinux labeling consistency, graphroot filesystem,
  secrets/config readability (with `sudo chgrp -R 1002` rather than `podman
  unshare chown`, per `podman-preflight-host.md`'s rootful remediation), and
  — new for this issue — confirming host UID 1001 / GID 1000 aren't already
  assigned to an unrelated account or group on the target.
- **#11, #12, #13, #14, #5**: same answers as the rootless section, since
  none of those are subuid-dependent.

### Procedure

1. On the **source**, confirm identity:
   ```bash
   sudo podman exec hive cat /data/hive-id
   sudo podman exec hive sh -c 'sha256sum /data/gh-app-key*.pem'
   ```
2. **Stop the source before archiving:**
   ```bash
   sudo systemctl stop hive.service hive-gateway.service
   ```
3. Export (same commands as rootless, prefixed `sudo`, per
   [backup-restore.md](backup-restore.md#rootless-never-tar-the-volume-from-the-host-shell)'s
   own note that rootful "has no user namespace... use the same commands
   anyway, with `sudo`"):
   ```bash
   sudo podman volume export hive-data -o hive-data-$(date +%F).tar
   sudo tar czf hive-config-$(date +%F).tar.gz -C /etc /etc/hive
   ```
4. Copy both archives to the target.
5. On the **target**, check for a UID/GID 1001/1000 collision first:
   ```bash
   id 1001 ; getent group 1000
   ```
   If either already exists and refers to something other than this hive's
   expected `dev`/`node` identity, resolve that before proceeding — extracting
   over an unrelated existing UID/GID is a host-administration decision this
   doc will not make for you. Then run the preflight checklist above with
   `HIVE_DEPLOY_RUNTIME=podman` (as root).
6. Recreate the volume through its unit and import:
   ```bash
   sudo systemctl start hive-data-volume.service
   sudo podman volume import hive-data hive-data-<date>.tar
   ```
7. Extract config/secrets and re-apply ownership the rootful way:
   ```bash
   sudo tar xzf hive-config-<date>.tar.gz -C /etc
   sudo chgrp -R 1002 /etc/hive/secrets
   sudo chmod 0750 /etc/hive/secrets
   ```
8. Start and verify before destroying the source:
   ```bash
   sudo systemctl start hive.service hive-gateway.service
   ```
9. Only after verification passes:
   ```bash
   sudo systemctl stop hive.service hive-gateway.service
   sudo bin/hive-podman-teardown.sh plan
   sudo bin/hive-podman-teardown.sh run --yes
   ```

## Stop source before starting target — the rule, for all three runtimes

No code in `entrypoint.sh`, the Compose file, or the Quadlet units detects or
rejects a second instance of the same `hive-id` starting elsewhere while the
first is still running (epic item #11, UNCONFIRMED as anything other than an
operator discipline). Two live instances sharing one GitHub App key would
independently poll/act on the same issues and PRs and could double-heartbeat
if hub-registered ([move-hub-registered-cutover.md](move-hub-registered-cutover.md)
covers that case). Every procedure above therefore:

1. Stops the source (`docker compose stop` / `systemctl --user stop` /
   `sudo systemctl stop`) — never `down -v` or the teardown script — **before**
   taking the archive, so `hive-state.json` and `beads/` are quiesced rather
   than captured mid-write.
2. Starts the target and runs the verification steps below **before**
   destroying anything on the source.
3. Only destroys the source (`down -v` / `hive-podman-teardown.sh run --yes`)
   **after** verification passes on the target.

If step 2's verification fails, the source is still intact and stopped —
restart it (`docker compose start` / `systemctl --user start` /
`sudo systemctl start`) to fail back with no data loss, and re-diagnose the
target before trying again.

## Post-restore verification

Modelled on `backup-restore.md`'s own verify steps and the "Podman: what was
observed" table (`hive-id`, GitHub App key SHA-256, and identity checks after
a restore).

```bash
# hive-id must match what you recorded on the source before stopping it
docker compose -f src/docker-compose.yaml exec hive cat /data/hive-id   # Compose
podman exec hive cat /data/hive-id                                      # Podman, either root mode

# GitHub App key SHA-256 must match too -- a byte-identical key file, not just
# a same-named one
docker compose -f src/docker-compose.yaml exec hive sh -c 'sha256sum /data/gh-app-key*.pem'
podman exec hive sh -c 'sha256sum /data/gh-app-key*.pem'

# beads ledger present and non-empty (an empty dir after a restore that should
# have carried history is a red flag, not proof of a fresh install)
docker compose -f src/docker-compose.yaml exec hive sh -c 'find /data/beads -type f | wc -l'
podman exec hive sh -c 'find /data/beads -type f | wc -l'

# dashboard config overlay present
docker compose -f src/docker-compose.yaml exec hive test -f /data/hive.yaml.dashboard && echo present
podman exec hive test -f /data/hive.yaml.dashboard && echo present

# service reports healthy (Notify=healthy / the compose healthcheck already
# gate on this, but confirm from the API too)
curl -sf http://<target-host>:3001/api/health
```

`hive-state.json`'s hash is **expected to change** across a restore —
`backup-restore.md` notes the running service rewrites it at boot, and a
whole-volume checksum comparison will not match and is not supposed to. Do not
use it as a verification signal; use the four checks above instead.

If agent backend credentials were carried over (`/data/home/*` in the
archive), confirm at least one agent's backend is usable on the target
(e.g. its next scheduled action runs without a re-auth prompt) rather than
assuming the file copy alone proves it — the entrypoint's chown/symlink pass
over `/data/home` and `/home/dev/*-beads` runs again on the target's first
boot ([`src/deploy/entrypoint.sh:894-955`](../deploy/entrypoint.sh)) and could
in principle rewrite ownership in a way a same-named-but-different-mapped
rootless target might not reproduce identically; if it wasn't carried over,
expect and document the re-authentication prompt instead.

## See also

- [Backup and restore](backup-restore.md) — the same-host backup/wipe/restore
  cycle this page builds on, including the observed ownership and label
  values referenced throughout.
- [Podman preflight: subordinate IDs, graphroot, and networking](podman-preflight-ids.md)
  and [Podman preflight: SELinux, mounts, secrets, and ports](podman-preflight-host.md)
  — the scripts the preflight checklist above is derived from.
- [Podman standalone with Quadlet](podman-standalone-quadlet.md) — the install
  path the target host prerequisites assume.
- [Cross-cluster migration](cross-cluster-migration.md) — the equivalent
  procedure for a hub-hosted hive moving between Kubernetes clusters, not
  covered by this page.
- [move-hub-registered-cutover.md](move-hub-registered-cutover.md) — the
  cross-cutting identity/heartbeat/dashboard-URL cutover concerns for a hive
  that reports to a hub, whichever runtime it's deployed on. Read that page
  first if this hive has a `hub.*` block configured.

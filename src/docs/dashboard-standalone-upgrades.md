# Dashboard-triggered standalone upgrades

Hive exposes its deployment runtime in `/api/version` and only shows the
dashboard upgrade button when the runtime is explicit and its upgrade bridge is
available. If Hive cannot prove the runtime, it reports `unknown` and the
dashboard says to update manually. This is deliberate: running the wrong host
lifecycle is worse than hiding an unsafe button.

## Runtime contract

Set the runtime where the container can read it:

- Docker Compose: `HIVE_DEPLOYMENT_RUNTIME=docker-compose`
- Podman/Quadlet: `HIVE_DEPLOYMENT_RUNTIME=podman-quadlet`
- Hub/Kubernetes: `HIVE_DEPLOYMENT_RUNTIME=kubernetes`

Podman/Quadlet also requires an explicit manager mode:

- `HIVE_DEPLOYMENT_PODMAN_MODE=rootless` for `systemctl --user`
- `HIVE_DEPLOYMENT_PODMAN_MODE=rootful` for the system manager

New Compose assets set the Compose runtime in `src/docker-compose.yaml`. New
Quadlet installs set the runtime and mode in `hive.env`. For an existing
Quadlet install, use the migration below rather than editing those values by
hand.

## Upgrading an existing Podman install

From a current `v5` checkout containing the host request bridge, run **one**
command as the account that owns the install (choose the matching manager):

```sh
# Rootless: run as the user who installed Hive, not with sudo.
bin/hive-podman-update.sh reconcile migrate --rootless

# Rootful: the script uses sudo for system-manager operations.
bin/hive-podman-update.sh reconcile migrate --rootful
```

This is an explicit downtime operation: it reconciles repo-owned units and
helpers, repairs/deduplicates `HIVE_DEPLOYMENT_RUNTIME`,
`HIVE_DEPLOYMENT_PODMAN_MODE`, and `HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR` in
`hive.env`, creates/repairs the host request directory with mode 770 and the
container launch group (using `podman unshare` for rootless ownership), enables
only `hive-upgrade.path`, and recreates Hive so the environment and writable
request mount actually reach the container. The request path is read from the
checkout's Quadlet mount, not guessed. Both host helpers are installed together
so the drain can invoke the update script without depending on the checkout's
location. Gateway health is checked before success is reported.

`hive.yaml`, `secrets/`, and all other environment lines (including tokens) are
preserved. A second run reports `already current`, changes no files, and exits
0. A failed recreate leaves a pending marker so rerunning the same command
retries activation even if the files already match. Missing bridge assets fail
before modifying the install. This migrates host assets, not the image or its
pin: the running image must also contain dashboard request-bridge support for
the button to become available. No Podman or systemd socket is mounted.

**Ownership:** `hive-podman-setup.sh` provisions new installs;
`hive-podman-update.sh reconcile migrate` upgrades existing ones, delegating
repo-owned file copying to its existing `reconcile apply` path. Ordinary
`reconcile check` remains read-only and `reconcile apply` still leaves
`hive.env` and the running Hive container untouched. Do not use `setup --force`
for this migration: that can replace operator configuration and tokens.

## Upgrade target

For a standalone deployment with `HIVE_SELF_IMAGE` naming a release channel
(`stable`, `candidate`, or `edge`), the default install target is that image
reference, not the head of the binary's build branch. Setup derives this
metadata from the installed image, whose default comes from
`src/deploy/standalone-images.sh`; no separate default channel is invented by
the dashboard. `HIVE_SELF_IMAGE_TRACKING=registry|pinned` describes the host's
update posture; it does not turn a digest pin into a channel subscription.

`/api/version` reports `target.source=channel`, `target.channel`, `target.ref`,
and the image's OCI revision in `target.sha`/`target.short`. If the registry
cannot resolve that revision, `target.resolved=false`: the dashboard must not
substitute a branch head or offer an unverified upgrade. The button identifies
the channel and revision, and confirmation displays the full image ref. The
helper receives the channel ref, so a promotion between viewing the dashboard
and pulling can advance the installed revision.

An empty `/api/self-upgrade` target resolves to the installed channel ref. An
explicit `target` overrides it: either a legacy 7–40 character hexadecimal
commit (mapped to its short image tag), or a fully qualified
`ghcr.io/hivecommons/hive:<tag>` / `ghcr.io/hivecommons/hive@sha256:<digest>`
reference accepted by the host helper's existing allow-list. A deployment
without channel metadata must supply an explicit target for this API default;
a digest pin is never guessed to be `stable`.

Target selection does not itself change the host executor's update semantics.
The registry-tracking executor work is tracked in #10421, and the host request
bridge in #10423; this target contract supplies one channel ref to either
executor rather than independently selecting a branch commit.

## Host request bridge (Podman/Quadlet)

Podman/Quadlet never executes an upgrade helper inside the container. Install
and enable the host bridge tracked in [#10416](https://github.com/hivecommons/hive/issues/10416),
bind-mount its request directory read-write, and set the **container-visible**
path using `HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR` (or YAML
`deployment.upgrade_request_dir`). The environment variable takes precedence.
For example, the mounted path can be `/run/hive/upgrade-requests`.
The dashboard does not create the directory or install host units. Runtime,
manager mode, and a successful write probe are required to enable the button.
Absent, missing, or unwritable directories retain the existing host-command
instructions; an executable `HIVE_DASHBOARD_UPGRADE_HELPER` does not enable
Podman upgrades.

The dashboard publishes one `hive-upgrade-<unique-id>.json` per accepted request:

```json
{"target_ref":"ghcr.io/hivecommons/hive:abcdef1","requester":"owner-login","requested_at":"2026-11-12T12:00:00Z"}
```

`target_ref` uses the dashboard target grammar described above: a validated
channel/tag/digest image reference, or a legacy 7–40 character hexadecimal
commit normalized to the first seven lowercase characters.
`requester` is the authenticated request's audit user (`local` for local token
access), not a value supplied in the JSON body. `requested_at` is UTC RFC3339.
Files have mode `0644` and are written and closed under a hidden temporary name
in the same directory before an atomic rename. The host must watch **only
`*.json`** in the directory's top level, ignoring `.hive-upgrade-*` temporary
files and `.hive-upgrade-probe-*` write probes.

This is an asynchronous trust boundary: the host bridge validates references
and request age before invoking the host lifecycle, then archives results in
`done/` or `failed/`. No Docker, Podman, or systemd socket is exposed to Hive.
Only the authorized Hive container and host bridge should have access to the
request directory; the host consumer must be able to read its files. They are
`0644` because on rootless Podman the container uid maps to a host subuid, so
the host user reads them only through the "other" bits; the payload holds no
secrets, and the `770` request directory still limits who can reach them.
Rootless bridges run as the hive user, rootful bridges under the system manager.
The host installation, units, age limit, and result handling belong to #10416;
this dashboard change alone does not install a consumer.

`/api/self-upgrade` returns `status=accepted` and a message that the upgrade has
not completed as soon as the request is published. It does not wait for the
host, and acceptance is not proof of rollout success.

## Host helper contract (Docker Compose)

Standalone upgrades are host lifecycle operations. Hive must not mount Docker,
Podman, or systemd sockets into the Hive container. Compose retains its existing
executable-helper contract:

```sh
HIVE_DASHBOARD_UPGRADE_HELPER=/usr/local/libexec/hive-dashboard-upgrade-helper
```

The helper protocol is closed:

```sh
hive-dashboard-upgrade-helper upgrade \
  --runtime docker-compose \
  --ref ghcr.io/hivecommons/hive:<tag-or-digest>
```

`bin/hive-dashboard-upgrade-helper.sh` implements that protocol. It validates
the image reference with an allow-list for `ghcr.io/hivecommons/hive`, passes
arguments as argv, and delegates to existing reviewed host scripts:

- Docker Compose: `src/deploy/blue-green-deploy.sh --skip-build --image-ref
  <ref>`, which pulls the requested image, starts `hive-next`, proves health,
  then swaps the container and reloads the gateway.

For Compose, if the helper is missing or not executable, `/api/version` reports the detected
runtime but `upgradeSupported=false`, and `/api/self-upgrade` returns an honest
error instead of relaying to the Hub.

## Host bridge installation (Podman/Quadlet)

A Hive under Quadlet runs in its own mount, PID and user namespace, so the
helper contract above cannot be satisfied from inside it: the helper is not in
the image, and the lifecycle it has to drive — `systemctl`, `podman`, the
Quadlet drop-ins under `%E/hive` — is not reachable from a container that
deliberately mounts no Podman, Docker, or systemd socket. On such a host the
Upgrade button could only ever be disabled with an honest reason
([#10344](https://github.com/hivecommons/hive/issues/10344)).

The bridge closes that without opening a socket. The container writes a
**request file** into one bind-mounted directory; a host-side `.path` unit
notices and runs the real update script outside every container namespace.

```
container  %E/hive/upgrade-requests -> /run/hive/upgrade-requests   (rw, the only writable mount)
host       hive-upgrade.path  --PathExistsGlob-->  hive-upgrade.service
                                                   -> hive-upgrade-request.sh apply
                                                      -> bin/hive-podman-update.sh upgrade <ref>
```

Assets: `src/deploy/systemd/hive-upgrade.path`,
`src/deploy/systemd/hive-upgrade.service`, `bin/hive-upgrade-request.sh`, and
the `Volume=` line in `src/deploy/quadlet/hive.container`.
`bin/hive-podman-setup.sh` installs all of them on a new install, creates the
request directory mode 770 / group 1002, and sets
`HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR` in `hive.env` to the **container-side**
path. Rootless installs get `--user` units in `~/.config/systemd/user`;
rootful installs get system units in `/etc/systemd/system`. The enable step is
the `.path` unit and never the service:

```sh
systemctl --user enable --now hive-upgrade.path   # rootless, as the hive user
sudo systemctl enable --now hive-upgrade.path     # rootful
```

Enabling `hive-upgrade.service` instead would run an upgrade at every boot.

### Request format

One JSON object per file, named `*.json`, written **atomically** — to a
temporary name in the same directory and then `rename(2)`d into place, so the
`.path` unit can never observe a half-written request:

```json
{"target_ref":"ghcr.io/hivecommons/hive:a1b2c3d","requester":"owner-login","requested_at":"2026-10-03T11:04:05Z"}
```

`target_ref` is required and is exactly what the dashboard publishes (see
"Host request bridge" above). `requester` and `requested_at` are advisory:
they are recorded with the result and nothing is decided from them. The legacy
spellings `ref` and `requestedAt` are accepted for hand-written requests.
Unknown fields are ignored.

Each handled request is **moved** into `done/` or `failed/` beside a
`.result` file before its outcome is recorded, so neither a success nor a
failure can be replayed by the watch re-triggering. Archive directories the
bridge creates are `0700`. A request the host user cannot read is retried
through `podman unshare cat` on a rootless host; if it is still unreadable the
bridge reports `cannot read request: permission denied (owner uid …, drain uid
…)`, archives it as `rejected: unreadable request` and exits `78`. Inspect
either archive with:

```sh
hive-upgrade-request.sh status
```

### Trust boundary

Everything in a request was written by the container and is treated as
untrusted input:

- The ref is matched against the same closed allow-list
  `bin/hive-dashboard-upgrade-helper.sh` uses — `ghcr.io/hivecommons/hive` by
  tag or by `sha256` digest, and nothing else. A ref that does not match never
  reaches `podman`.
- Every value is passed as **argv**. Nothing read from a request is ever
  evaluated as shell text, sourced, or interpolated into a command line.
- The root mode comes from the host's own manager (`$MANAGERPID`), never from
  the request: a container cannot ask for the system manager.
- A request older than `HIVE_UPGRADE_REQUEST_MAX_AGE` (default 3600s) is
  archived **unapplied**, so a request left behind by a host that was down
  cannot silently install a stale ref days later. The age is the file's mtime,
  not the timestamp in the file, because the container writes the latter.
- The request directory and its `done/` and `failed/` archives must be real
  directories, and a result file is only created at a path that does not yet
  exist (`O_EXCL`). The container can place symlinks in the bind mount; the
  bridge never follows one, so its writes — which run as the host user, root
  on a rootful install — cannot be steered onto another host path.
- The container's entire capability across this boundary is "cause
  `hive-upgrade-request.sh` to run".

The bridge changes **where** an upgrade runs, not what it does: it still
delegates to `bin/hive-podman-update.sh upgrade <ref>`, which is `pin` on a
host that is not tracking the registry and `podman auto-update` semantics
(unchanged timer, unchanged `AutoUpdate=registry` drop-in) on one that is —
see [podman-auto-update.md](podman-auto-update.md) and gap 3 of
[#10344](https://github.com/hivecommons/hive/issues/10344) for why a plain
`pin` would silently disable tracking.

## Operational notes

- Owner authorization is still required on `/api/self-upgrade`.
- Podman dashboard upgrades go through `bin/hive-podman-update.sh upgrade
  <ref>`. On a host **not** using `podman-auto-update.timer`, this writes a
  digest pin, same as `bin/hive-podman-update.sh pin <ref>` always has. On a
  host that **is** tracking the registry, `upgrade` instead drives
  `podman auto-update` directly so the timer and the `AutoUpdate=registry`
  drop-in are both left in place; it REFUSES rather than silently pinning if
  `<ref>` is not the tag the host already tracks (a digest, or a different
  tag), and `--force-pin` is the explicit, documented way to override that.
- Podman dashboard upgrades are image-only. The update script refreshes the
  gateway config but does not rewrite Quadlet/boot units; run
  `bin/hive-podman-update.sh reconcile check` or `reconcile apply` for host
  asset drift.
- Compose dashboard upgrades are blue-green image upgrades. They do not change
  the Watchtower profile; operators should avoid simultaneously using the
  60-second Watchtower poller and manual dashboard upgrades unless they accept
  that Watchtower may observe the image as already current.
- Rollback remains runtime-specific: Podman uses the update script's newest
  healthy pin (an auto-update-driven upgrade does not add a pin history entry;
  `rollback` after one requires `unpin` was never run and a prior pin exists);
  Compose keeps the old image available for an operator/helper to run with
  Docker if a post-swap rollback is needed.

## Release notes before and after an upgrade

The dashboard Upgrade button does not start the upgrade immediately. It opens a
"What's new" modal headed `Upgrade <runtime>: <current> → <target>` (channel
targets keep the caveat that the channel may advance before the host pulls it).
The body is read from `GET /api/version/release-notes`: one collapsible block
per release, newest first with the first expanded, grouped Added / Changed /
Fixed / Security / Deprecated, `#NNNN` references linked to the repository, and
unreleased changelog fragments last under "Also in this build". If the notes
cannot be fetched the modal shows one muted "Release notes unavailable
(<reason>)" line and **Upgrade** stays enabled. **Upgrade** runs the normal
`POST /api/self-upgrade` flow; **Cancel** (or Esc) closes the modal without
upgrading. Focus is trapped in the modal and returns to the Upgrade button.

After the dashboard notices the running build changed, owners and mergers see a
dismissible banner at the top of the overview, "Upgraded <old> → <new>: N
releases. What changed ▸". Expanding it opens the same modal read-only (no
Upgrade button). The last-seen SHA is stored per browser, so the banner shows
once per upgrade, never on a first visit, and covers upgrades whose modal the
operator never opened.

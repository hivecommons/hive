# Quadlet update and rollback: moving the image, and getting back

How to move the Hive Quadlet unit to a new image, and — the half that matters —
how to get back when the new one does not work. Measured rather than inferred
(#4378). [ADR-0017](adr/0017-podman-quadlet-lifecycle.md) chose Quadlet because
`Notify=healthy` means a unit that reports started is actually serving;
[#4354](podman-standalone-quadlet.md) shipped the units and
[#4377](podman-quadlet-lifecycle.md) exercised their lifecycle. Neither said how
to change the image, which is where that readiness property earns its keep.

Scope is the **manual, deliberate** path: an operator decides to move, and
decides to come back. Nothing here polls a registry, and `AutoUpdate=registry`
appears nowhere. Health-aware auto-update — the Podman answer to the Docker
stack's Watchtower — is a separate later slice that this one is the foundation
for.

## The short version

1. **The shipped unit cannot be rolled back to.** `hive.container` names
   `ghcr.io/hivecommons/hive:stable`, a floating tag, and today's `:stable` and
   last week's `:stable` are different images with the same name. The pin
   therefore lives in a Quadlet **drop-in**, `hive.container.d/10-image.conf`,
   which sets `Image=` to a digest without editing the file the repo owns.
2. **A healthy update is 11 seconds, and it is not zero-downtime.** Measured
   rootless and rootful: ~1s `deactivating`, ~10s `activating` with the new
   container `(starting)`, then `active` in the same second the container turns
   `(healthy)`. Hive is not serving for that whole window.
3. **A failed update costs 301 seconds before anything says so, and the unit
   never reads `failed`.** An image that never answers `/api/health` held the
   unit in `activating/start` for the full `TimeoutStartSec=300`, then went to
   `activating/auto-restart` and straight back into another 300-second attempt.
   `systemctl is-failed` reported **`activating`**, not `failed`, throughout.
   Monitoring that watches for `failed` will never see a bad update.
4. **Rollback from that failed state took about 11 seconds and one decision.**
   Rewrite the digest, `daemon-reload`, `systemctl stop` (555ms — it cancels the
   in-flight bad start job), `systemctl start` (10-11s, healthy). Measured in
   both root modes, out of a real failed update rather than a healthy one.
5. **`hive-data` survived all of it**: the original update, the failed update,
   and the rollback, verified with a marker file written into `/data`.

## Running it yourself

```sh
bin/hive-podman-update.sh status                       # read-only
bin/hive-podman-update.sh resolve ghcr.io/hivecommons/hive:stable
bin/hive-podman-update.sh pin ghcr.io/hivecommons/hive:stable    # RESTARTS HIVE
bin/hive-podman-update.sh rollback                               # RESTARTS HIVE
bin/hive-podman-update.sh unpin
bin/hive-podman-update.sh reconcile          # read-only; 78 when the host is stale
bin/hive-podman-update.sh reconcile apply    # re-copies the repo-owned files
```

`--rootful` drives the system manager through `sudo`; the default is the user
manager. `status` and `resolve` are read-only — `status` starts nothing, stops
nothing and pulls nothing, and `resolve` on a reference that is already a digest
contacts no registry at all.

Exit codes follow the other Podman scripts: 0 success, 78 the operation did not
end healthy, 64 an unusable invocation.
`bin/test_hive_podman_update.sh` covers it with every input mocked — no Podman,
no Quadlet, no privileges, no pull — and the cases that matter are the failed
ones, because a rollback that only works from a healthy unit is not a rollback.

## The image is not the whole deployment (#6078)

Updating the image moves the **binary**. It does not move the files this host
runs *from*, and there are eight of them:

| File | Installed to | Owned by |
| --- | --- | --- |
| `nginx.conf` | `%E/hive/nginx.conf` | the repo |
| `hive.network`, `hive-data.volume`, `hive.container`, `hive-gateway.container` | the Quadlet directory | the repo |
| `hive-boot.target`, `hive-boot-gate.service` | the systemd unit directory | the repo |
| `hive.yaml`, `hive.env`, `secrets/` | `%E/hive/` | **the operator** |

The gateway does not read `nginx.conf` out of the image — `hive-gateway.container`
bind-mounts it from the host — so a change to `src/deploy/nginx.conf` ships,
releases, and never reaches a deployment that is already running.

That was measured, not theorised. #5200 fixed the gateway dropping the WebSocket
upgrade on `/api/` and merged on 2026-08-30. Six days later a host installed on
2026-08-22 was still returning HTTP 400 on the contributor handshake while
`/api/contribute/status` reported a `served_sha` that **contained** the fix —
because that SHA describes the binary. Copying the current conf in by hand and
restarting only the gateway fixed it immediately.

`hive.container` has the same staleness with no symptom at all: its `Image=`
moved from `ghcr.io/kubestellar/hive` to `ghcr.io/hivecommons/hive`, and a host
still naming the old path polls it, finds it unchanged, and reports
`UPDATED=false`, exit 0, timer green — byte-identical to a host that is
genuinely current. It keeps working only while the old org stays mirrored.

### What `reconcile` does about it

```sh
bin/hive-podman-update.sh reconcile          # compare only. 0 in sync, 78 stale.
bin/hive-podman-update.sh reconcile apply    # re-copy the ones that fell behind
```

It compares the eight repo-owned files against the checkout it is run from and,
with `apply`, re-copies the ones that differ. It **never** writes `hive.yaml`,
`hive.env` or `secrets/`, and prints that list before it writes anything —
which is the whole reason it exists rather than `setup --force`, since that
re-copies those three too and regenerating `hive.env` takes
`HIVE_DASHBOARD_TOKEN` with it.

Two things it deliberately does not do:

- **It needs a checkout.** Every other command in this script works against the
  host alone. Run it from a clone, or point `HIVE_UPDATE_SRC_ROOT` at one. With
  no checkout it refuses and says so, rather than reporting that everything
  matches — "not checked" and "up to date" must not print the same thing.
- **It does not recreate the container.** A rewritten `hive.container` is picked
  up by `daemon-reload`, but the *running* container keeps the reference it was
  created with until something recreates it, and that is downtime. `apply` says
  so and names the two commands that do it.

`reconcile` exits **78 on drift**, on purpose. The second half of #6078 is that
a frozen host is indistinguishable from a current one in everything a monitor
can read; this exit status is the readable state that did not exist before.

### Where it happens on its own

`pin` refreshes the **gateway config** and restarts the gateway, because that
file carries no operator state, `pin` has already been told to move this
deployment forward, and it restarts the gateway a few lines later anyway.
Leaving it stale there is exactly the reported defect: an operator runs an
explicit update, is told it is serving, and keeps the old gateway. Unit files
are reported by `pin`, never rewritten by it.

`status` reports drift without being asked. That is the only cover the
**auto-update** path gets: `podman auto-update` is a systemd timer calling
podman directly, so no script is in that path and there is no "refresh on every
update" to hook. Detection is what is available, so detection is what it does.

### Existing hosts need one manual run

This fix ships *inside* the files that are never refreshed, so it cannot reach
the hosts that need it most. Every deployment installed before it merges needs
one run from a checkout, once:

```sh
git clone https://github.com/hivecommons/hive && cd hive
bin/hive-podman-update.sh reconcile              # see what is stale
bin/hive-podman-update.sh reconcile apply        # add --rootful for a system install
```

If that reports a rewritten `hive.container`, the running container is still on
the old image reference until you recreate it:

```sh
systemctl --user restart hive.service            # or: sudo systemctl restart, rootful
```


## How the pin works

### Why a drop-in and not an edit to `hive.container`

`src/deploy/standalone-images.sh` is the one image-reference source of truth
(#4206) and `src/deploy/test_standalone_image_refs.sh` fails the build when a
deployment asset stops agreeing with it. `hive.container` must keep naming
`ghcr.io/hivecommons/hive:stable`, so the operator's pin cannot live there.

Quadlet supports systemd-style drop-ins on its own source files: for
`hive.container` it scans `hive.container.d/` for `*.conf` and merges them into
the base file, in alphabetical order, on every `daemon-reload`. So:

```
~/.config/containers/systemd/hive.container.d/10-image.conf     # rootless
/etc/containers/systemd/hive.container.d/10-image.conf          # rootful
```

```ini
[Container]
Image=ghcr.io/hivecommons/hive@sha256:ec8e69bc00ff044bf893c8828966bc9d70bdb8661297cc72e949b5de86a37bc3
```

Update and rollback are then the same two-line operation on one file. The base
unit is never touched, the current pin is one `grep` away, and the later
auto-update slice has an obvious file to manage.

### The pin history lives in that same file

`bin/hive-podman-update.sh` writes comment lines Quadlet ignores:

```
# HIVE-PIN healthy 2026-08-21T10:25:49Z sha256:ec8e69bc... ghcr.io/hivecommons/hive:stable
# HIVE-PIN failed  2026-08-21T10:31:02Z sha256:4a73073b... docker.io/library/nginx@sha256:4a73073b...
```

newest first, each recording whether the script *watched that pin become
healthy*. `rollback` returns to the newest `healthy` entry that is not the
current one. That is what makes it usable from a failed update: after a bad pin
the top entry is the one that broke, and skipping it is the entire job. It also
rules out the ping-pong a naive "go to the previous entry" would give you, where
rolling back twice returns you to the broken image.

### Digest, not tag, and specifically the *manifest list* digest

The obvious way to turn a tag into a digest is wrong:

```
$ podman image inspect ghcr.io/hivecommons/hive:stable --format '{{index .RepoDigests 0}}'
ghcr.io/hivecommons/hive@sha256:5d3f442fd7c59d3e453a9447501ff1662751ba8dd0564cff51188330b13dbc77

$ podman images --digests | grep stable
ghcr.io/hivecommons/hive  stable  sha256:ec8e69bc00ff044bf893c8828966bc9d70bdb8661297cc72e949b5de86a37bc3
```

Both are real digests of the same tag, and only the second is the **manifest
list**. `5d3f442f` is the amd64 manifest inside it — measured, `podman manifest
inspect` lists it as the `linux/amd64` entry. Pinning that one produces a pin
that resolves on this host and fails on arm64, which is exactly the kind of
error a digest pin is supposed to prevent. `resolve` reads the registry through
`skopeo` when it is present and falls back to the `podman images --digests`
column, never to `RepoDigests`.

## What was measured

Fedora 44, Podman 5.8.4, systemd 259, cgroup v2, SELinux **enforcing**, against
the real `ghcr.io/hivecommons/hive` image, in **both** root modes. The two Hive
digests are genuine consecutive-ish builds of `v4`, not synthetic tags:

| build | digest | `hive --version` inside the container |
| --- | --- | --- |
| older (tag `b35e9cc`, built 2026-08-14) | `sha256:2326e85d…` | `hive 3.0.0 (commit b35e9cc, branch v4)` |
| newer (tag `stable`, built 2026-08-21) | `sha256:ec8e69bc…` | `hive 3.0.0 (commit 77300c8, branch v4)` |

The version string is how each step below was checked to have actually changed
the running code rather than only the reference: the image carries no
`org.opencontainers.image.*` labels, so `podman inspect` gives you the digest and
`hive --version` gives you the commit.

### A healthy update

```
$ bin/hive-podman-update.sh pin ghcr.io/hivecommons/hive:stable
  PASS  ghcr.io/hivecommons/hive:stable resolves to sha256:ec8e69bc…
  PASS  ghcr.io/hivecommons/hive@sha256:ec8e69bc… is present locally
  PASS  wrote …/hive.container.d/10-image.conf
  PASS  daemon-reload: the generated unit now names …@sha256:ec8e69bc…
        the running container is still …@sha256:2326e85d… -- a reload does not restart
  PASS  restart returned 0 after 11s, state active/running/success
        running  ghcr.io/hivecommons/hive@sha256:ec8e69bc…
        version  hive 3.0.0 (commit 77300c8, branch v4)
```

Sampled once a second across the restart:

```
t+01s  active/running        Up 50 seconds (healthy)      …@sha256:2326e85d…
t+02s  deactivating/stop     Stopping (healthy)           …@sha256:2326e85d…
t+03s  activating/start      Up Less than a second (starting)  …@sha256:ec8e69bc…
 …
t+12s  activating/start      Up 9 seconds (starting)      …@sha256:ec8e69bc…
t+13s  activating/start      Up 10 seconds (healthy)      …@sha256:ec8e69bc…
t+14s  active/running        Up 11 seconds (healthy)      …@sha256:ec8e69bc…
```

Two things to read off that. The unit becomes `active` in the same second the
container becomes `(healthy)` and not before — `Notify=healthy` doing exactly
what ADR-0017 chose it for. And there are eleven seconds in the middle where
Hive is not serving: this is a stop-and-replace, not a rolling update, and the
`--rm` in the generated `ExecStart` means the old container is gone rather than
kept warm.

| step | rootless | rootful |
| --- | --- | --- |
| `daemon-reload` after rewriting the pin | ExecStart names the new digest, container unchanged | same |
| `restart`, old build → new build | rc 0 in **11.6s**, `active/running` | rc 0 in **11s**, `active/running` |
| `restart`, new build → old build (a downgrade) | rc 0 in **11s**, `active/running` | rc 0 in **11s**, `active/running` |

A `daemon-reload` on its own changes nothing an operator can see from `podman
ps`: the generated unit names the new digest and the running container still
names the old one until a restart. The script prints both for that reason.

### A failed update, which is the case the feature exists for

Executed in both root modes. Staged by pinning a digest that runs but never
answers `/api/health` — the gateway's own
`docker.io/library/nginx@sha256:4a73073b…`, standing in for a bad Hive build.
What is being measured is what the **unit** does when the newly pinned image
never becomes healthy, and that does not depend on why it doesn't. Recorded
plainly because it is a stand-in: no genuinely broken published Hive image was
available to pin.

```
t+000s  active/running/success          Up About a minute (healthy)  …hive@sha256:ec8e69bc…
t+010s  active/stop/success             Stopping                     …hive@sha256:ec8e69bc…
t+020s  activating/start/success        Up 9 seconds (starting)      …nginx@sha256:4a73073b…
  …
t+150s  activating/start/success        Up 2 minutes (starting)      …nginx@sha256:4a73073b…
t+160s  activating/start/success        Up 2 minutes (unhealthy)     …nginx@sha256:4a73073b…
  …
t+300s  activating/start/success        Up 4 minutes (unhealthy)     …nginx@sha256:4a73073b…
t+310s  activating/auto-restart/timeout (no container)
t+330s  activating/auto-restart/timeout (no container)
t+340s  activating/start/success        Up Less than a second (starting)  …nginx@sha256:4a73073b…
```

```
$ systemctl --user restart hive.service
Job for hive.service failed because a timeout was exceeded.
restart rc=1 elapsed_s=301
```

```
hive.service: start operation timed out. Terminating.
hive.service: Failed with result 'timeout'.
Failed to start hive.service - Hive service.
hive.service: Scheduled restart job, restart counter is at 1.
```

**What the unit looks like while that is happening**, which is the row the
acceptance criteria asked for:

| window | `ActiveState/SubState/Result` | container | `is-failed` |
| --- | --- | --- | --- |
| 0 – ~150s | `activating/start/success` | `(starting)` | `activating` |
| ~150s – 300s | `activating/start/success` | `(unhealthy)` | `activating` |
| 300s | `restart` returns **1**, "a timeout was exceeded" | terminated | — |
| ~300 – 330s | `activating/auto-restart/timeout` | none | `activating` |
| ~330s onwards | `activating/start/success` again, next 300s attempt | `(starting)` | `activating` |

Three things in that table are worth an operator's attention.

The container reports `(starting)` for the first ~150 seconds and only then
`(unhealthy)`: that is `HealthStartPeriod=120s` plus the retry budget, and
during it there is nothing in `podman ps` that distinguishes a slow start from a
broken image. The unit does **not** fail when the container turns unhealthy — it
waits out the whole `TimeoutStartSec`.

`systemctl restart` blocks for the entire 300 seconds and *then* returns 1 —
`Job for hive.service failed because a timeout was exceeded.` An operator
running the update by hand gets no signal at all for five minutes. Measured at
**301s** in both root modes.

And the unit **never reads `failed`**. `Restart=always` — which #4377 added, for
good reasons that have nothing to do with this — moves it from the timeout
straight to `auto-restart` and back into another attempt, so `is-failed` reports
`activating` at every point above and `ActiveState` stays `activating`
indefinitely, in a ~330-second loop, until someone intervenes. Alerting keyed on
`systemctl is-failed` or on `ActiveState=failed` does not fire on a bad update.

`Result=timeout` during the `auto-restart` window is the honest signal, and
`NRestarts` climbing is the other one — but note it is not immediate. Sampled
straight after `restart` returned, rootful still read `NRestarts=0`, because the
first scheduled restart had not fired yet; rootless, sampled a little later,
read `1`. A check that runs once and sees 0 has not shown the unit is fine.

### The rollback

`status` during the failure, which is what an operator has to work from:

```
  PASS  pinned by drop-in: docker.io/library/nginx@sha256:4a73073b…
        unit ExecStart names   docker.io/library/nginx@sha256:4a73073b…
        running container      <none>
        unit state             activating/auto-restart/timeout

Pin history (newest first)
        failed  2026-08-21T10:26:15Z sha256:4a73073b… docker.io/library/nginx@sha256:4a73073b…
        healthy 2026-08-21T10:25:49Z sha256:ec8e69bc… ghcr.io/hivecommons/hive:stable
        healthy 2026-08-21T10:25:35Z sha256:2326e85d… ghcr.io/hivecommons/hive@sha256:2326e85d…

Rollback
  PASS  rollback would return to sha256:ec8e69bc…  (ghcr.io/hivecommons/hive:stable)
```

Then, from that failed state and without waiting for anything:

```
$ bin/hive-podman-update.sh rollback
  PASS  returning to sha256:ec8e69bc… (first pinned from ghcr.io/hivecommons/hive:stable)
  PASS  ghcr.io/hivecommons/hive@sha256:ec8e69bc… is present locally
  PASS  wrote …/hive.container.d/10-image.conf
  PASS  daemon-reload: the generated unit now names …@sha256:ec8e69bc…
        stopped the failing unit first: inactive/dead/timeout
  PASS  restart returned 0 after 10s, state active/running/success
        version  hive 3.0.0 (commit 77300c8, branch v4)
  PASS  rolled back to sha256:ec8e69bc… and it is serving

rollback exit: 0 elapsed_s=12
```

Note which entry it went to. The newest history entry is the `failed` nginx pin
and the one below it is the `healthy` `:stable` digest; rollback skipped the
first and took the second. A "go to the previous entry" rule would have walked
straight back into the image that had just failed.

The transcript above predates #4493 and its last PASS line was a lie: the
failed update had already stopped `hive-gateway.service` (`Requires=`), and a
`systemctl restart hive.service` only re-starts dependents that are ACTIVE
when the job runs, so the gateway stayed inactive and the published dashboard
port stayed dead while the script reported "and it is serving". Measured on
that state: `hive active / gateway inactive / :3001 DEAD / :3002 answering
inside the container`. The script now starts the gateway on every path that
restarts hive — `start` is idempotent, a no-op when it is already up — and
claims success only after the gateway answers `/api/health` on its published
port from the host, the same end-to-end assertion `bin/hive-podman-setup.sh`
ends on. A rollback whose gateway does not come up serving exits 78, with the
restored digest still recorded `healthy`: Hive served on it, and the fault is
in front of Hive.

| step | measured |
| --- | --- |
| `systemctl stop` on a unit mid-failing-start | rc 0 in **555ms**, `inactive/dead/success` |
| `systemctl start` on the restored digest | rc 0 in **10s**, `active/running`, healthy |
| total, from the operator's decision | **~12s** rootless, **~11s** rootful |

**Stop first, then start; do not `restart`.** After a failed update the unit is
not sitting still — it is in `Restart=always`'s loop, each attempt holding
`activating` for `TimeoutStartSec`. A stop job cancels the start job in flight
and returned in 555ms; a `restart` queued behind a bad attempt waits it out. The
script does the stop unconditionally for that reason.

The state that stop leaves behind depends on which half of the loop it
interrupted, and neither half looks alarming:

| stop issued during | left the unit at |
| --- | --- |
| `activating/start` — an attempt in progress | `inactive/dead/**success**` |
| `activating/auto-restart` — the gap between attempts | `inactive/dead/timeout` |

The first of those is `SuccessExitStatus=143 SIGTERM` from #4377 doing its job:
the container was terminated with SIGTERM and 143 is a success. It means the
tidy-looking state after a rollback is not evidence that the update was fine —
read the pin history or `NRestarts`, not the state.

### `hive-data` survived both directions

Proven the way #4377 proved it, with a marker file written into `/data` inside
the running container — on the `hive-data` volume, not in the image:

```
$ podman exec hive sh -c 'echo update-rollback-probe-… > /data/.update-probe'
```

| after | rootless marker | rootful marker |
| --- | --- | --- |
| written on the older build | `…probe-20260821T101346Z` | `…rootful-20260821T102908Z` |
| update, old build → new build | unchanged | unchanged |
| failed update to the never-healthy image | volume untouched — the stand-in image never wrote to `/data` | same |
| rollback to the new build | unchanged | unchanged |
| downgrade, new build → old build | not exercised in this run | unchanged |

Nothing in the update path touches `hive-data.volume`: the pin changes `Image=`
only, `ExecStop` is `podman rm -v -f -i hive` and `-v` removes anonymous volumes
only, and the volume unit is not restarted. The `:Z` trap characterised in
[podman-volume-persistence.md](podman-volume-persistence.md) does not come into
play either, because the volume is mounted without a relabel suffix and an
update does not change that.

The same real state was in `/data` throughout — the bead ledger, agent working
state, and the proxy CA the service generates on first boot — so this is not a
marker in an otherwise empty volume.

## The procedure, without the script

The script is convenience; this is what it does. `%E/containers/systemd` is
`~/.config/containers/systemd` rootless and `/etc/containers/systemd` rootful.

**Update:**

```sh
# 1. Resolve the target to a digest. NOT `podman image inspect ... RepoDigests`.
skopeo inspect --format '{{.Digest}}' docker://ghcr.io/hivecommons/hive:stable
# -> sha256:ec8e69bc…

# 2. Pull it BEFORE touching the unit. The Hive image measured 3.8GB, and a
#    pull done by the generated ExecStart is spent inside TimeoutStartSec.
podman pull ghcr.io/hivecommons/hive@sha256:ec8e69bc…

# 3. Write the pin, keeping a note of the digest you are leaving.
mkdir -p ~/.config/containers/systemd/hive.container.d
cat > ~/.config/containers/systemd/hive.container.d/10-image.conf <<'EOF'
[Container]
Image=ghcr.io/hivecommons/hive@sha256:ec8e69bc…
EOF

# 4. Regenerate and restart. The reload alone does nothing visible.
systemctl --user daemon-reload
systemctl --user restart hive.service      # returns only once /api/health answers

# 5. Confirm what is actually running.
podman inspect hive --format '{{.ImageName}}'
podman exec hive hive --version
```

**Rollback**, including from a failed update:

```sh
# 1. Cancel whatever the unit is doing. From a failed update this is the step
#    that matters -- it returns in under a second instead of waiting out the
#    remaining TimeoutStartSec.
systemctl --user stop hive.service

# 2. Put the previous digest back.
cat > ~/.config/containers/systemd/hive.container.d/10-image.conf <<'EOF'
[Container]
Image=ghcr.io/hivecommons/hive@sha256:2326e85d…
EOF
systemctl --user daemon-reload
systemctl --user start hive.service

# 3. Confirm.
podman exec hive hive --version
```

Rootful is the same with `sudo`, `/etc/containers/systemd`, and `systemctl`
without `--user`.

**Keep the previous image on the host.** A rollback that has to re-pull is a
rollback that a registry outage can take away, and a digest that has been
garbage-collected upstream cannot be pulled at all. The script refuses and
changes nothing when the rollback target will not pull, rather than rewriting
the pin and discovering the problem after the service is already down.

## Traps, measured not guessed

**`systemctl cat hive.service` does not name the drop-in.** For a normal systemd
unit `cat` prints each drop-in with its path as a header. Quadlet merges its
drop-ins *before* generating the service, so `cat` shows only the merged result:
the string `10-image.conf` appears nowhere in its output, and the pin is visible
only as the image at the end of `ExecStart=`. The drop-in file is the single
place the pin is written down, which is why the script keeps the history there
too.

**A `daemon-reload` is not an update.** After the reload the generated
`ExecStart` names the new digest and the running container still names the old
one, indefinitely. An operator who reloads and walks away has changed what will
happen at the next restart and nothing else.

**A bad update never reads `failed`** — see the table above. This is why
`bin/hive-podman-update.sh` decides an update succeeded from the pair
*`systemctl restart` returned 0* **and** *`ActiveState` is now `active`*, and
never from the absence of `failed`. A check written the obvious way, waiting for
`failed` to appear, waits forever.

**Prose that mentions the history marker parses as history.** The script's own
drop-in header explains the `HIVE-PIN` format, and the first parser matched
`^# HIVE-PIN ` — so the explanatory sentence became the newest history entry and
`rollback` read a sentence where it expected a digest. Caught by
`bin/test_hive_podman_update.sh`, which now asserts the header cannot be parsed
as an entry; the matcher requires a timestamp.

**`podman image inspect … RepoDigests` is the wrong digest** for a multi-arch
tag — see above. It is right often enough to look fine on the machine you
develop on.

## Not executed here

Per #4378's stop condition, recorded as not executed rather than inferred.

| | Status |
| --- | --- |
| update and rollback with a genuinely broken *Hive* image | **not executed** — the never-healthy image was `nginx:alpine`, pinned by digest, standing in for one |
| rollback across a Hive schema change in `/data` | **not executed** |
| update or rollback of `hive-gateway.container` | **not executed** — it is already digest-pinned in the repo and was out of scope |

The middle row is the one to be careful with. Everything measured here says the
*volume* is untouched in both directions; nothing here says a newer Hive that
has migrated the contents of `/data` can be rolled back to an older one that
expects the previous layout. That is a property of Hive, not of Quadlet, and it
needs its own slice — as do backup and restore.

## Related

- [podman-auto-update.md](podman-auto-update.md) — #4411, built directly on the measurement above.
  The `never reads failed` result does NOT rule out `podman auto-update --rollback`: podman reads
  the D-Bus start-job result (`timeout`), not `ActiveState`, so the rollback fires while alerting
  keyed on `is-failed` still does not. Opt-in, and it states what a digest pin written by THIS
  script does to it (wins, silently).
- [ADR-0017](adr/0017-podman-quadlet-lifecycle.md) — why Quadlet, and why `Notify=healthy`
- [podman-standalone-quadlet.md](podman-standalone-quadlet.md) — the units and how to install them
- [podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md) — stop, start, restart, recreate, boot; `SuccessExitStatus=143` and `Restart=always`, both of which shape what a failed update looks like
- [podman-volume-persistence.md](podman-volume-persistence.md) — what `hive-data` guarantees, and the `:Z` trap an update must not introduce

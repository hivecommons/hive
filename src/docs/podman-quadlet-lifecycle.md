# Quadlet lifecycle: stop, start, restart, recreate, and boot persistence

What the Hive Quadlet units actually do when an operator drives them, measured
rather than inferred (#4377, and the reboot rows in #4413).
[ADR-0017](adr/0017-podman-quadlet-lifecycle.md)
chose Quadlet for the persistent lifecycle and
[#4354](podman-standalone-quadlet.md) shipped the units; this page is the
lifecycle those two decisions implied, exercised.

Scope is lifecycle only: stop, start, restart, recreate, and boot persistence,
in both root modes. Every row is now executed: the two reboot rows this page
once recorded as **not executed** were executed by #4413 and are below. Update
and rollback are their own slice and are now written down in
[podman-quadlet-update-rollback.md](podman-quadlet-update-rollback.md)
(#4378) — including what a unit looks like while a bad update is timing out,
which is a lifecycle state this page does not produce. Auto-update is a later
slice again, as are backup, restore, and migration.

## The short version

Three things looked fine and were not.

1. **A clean `systemctl stop` left the unit `failed`.** Hive's entrypoint
   handles SIGTERM, persists state, and exits **143**; systemd's default
   success set is exit 0 alone. So a deliberate stop ended
   `ActiveState=failed`, `Result=exit-code`, `ExecMainStatus=143` — red in
   `systemctl status`, true for `systemctl is-failed`, and enough to put the
   host into `degraded`. Fixed by `SuccessExitStatus=143 SIGTERM`.
2. **`systemctl enable hive.service` does not work and never did.** Quadlet
   units are *generated*, and systemd refuses to enable a generated unit. The
   documented boot-persistence step was an error message in both root modes.
   What actually wires these units to boot is the `[Install]` section inside
   `hive.container`.
3. **`systemctl is-enabled` cannot answer "will Hive come back?"** It reports
   `generated` for these units in every case — lingering on, lingering off,
   symlink present or absent. #4377 was written to forbid inferring the reboot
   row from `is-enabled`, and this is why that was right.

The first of those needed a change to `hive.container`; the other two needed a
correction to [podman-standalone-quadlet.md](podman-standalone-quadlet.md).

## Running the check yourself

```sh
bin/hive-podman-lifecycle-probe.sh check              # read-only, rootless
bin/hive-podman-lifecycle-probe.sh check --rootful
bin/hive-podman-lifecycle-probe.sh exercise           # STOPS AND STARTS HIVE
bin/hive-podman-lifecycle-probe.sh boot-check         # run me after a reboot
```

`check` is read-only: it starts nothing, stops nothing, and relabels nothing,
so it is safe on a host that is serving. `exercise` drives the full lifecycle
and is not. `boot-check` reads the journal and needs no arming beforehand.

`exercise` ends with a restore step that returns the stack to its pre-probe
state, and that step exists because of a systemd asymmetry worth knowing about
([#4491](https://github.com/hivecommons/hive/issues/4491)):
`hive-gateway.service` has `Requires=hive.service`, and `Requires=` propagates
a *stop* but not a *start*. The probe's deliberate stop/start pair therefore
takes the gateway — and with it `:3001`, the only published port — down and
does not bring it back, where a single `systemctl restart hive.service` (one
job, one transaction) keeps it up. The restore step restarts the gateway,
confirms `:3001` answers as it did before the probe, runs even on a failed
step or a Ctrl-C, and reports a failed restoration as a finding rather than
"no findings".

Exit codes follow the other Podman scripts: 0 no findings, 78 at least one
finding, 64 an unusable invocation.
`bin/test_hive_podman_lifecycle_probe.sh` covers it with every input mocked,
so the suite runs on a host with no Podman and no lingering user.

## What was measured

Fedora 44, Podman 5.8.4, cgroup v2, SELinux **enforcing**, against the real
`ghcr.io/hivecommons/hive:stable` image, in **both** root modes. Rootless
config in `~/.config/hive`, rootful in `/etc/hive`, both with
`dashboard.port: 3002` as [#4367](podman-standalone-quadlet.md) requires.

### Stop, start, restart

| Step | rootless | rootful |
| --- | --- | --- |
| first `start` (image already pulled) | rc 0 in **11.4s**, `active/running`, container `healthy` | rc 0 in **11.7s**, `active/running`, container `healthy` |
| `stop`, before the fix | **0.7s**, `failed/failed/exit-code`, `ExecMainStatus=143` | **0.7s**, `failed/failed/exit-code`, `ExecMainStatus=143` |
| `stop`, after the fix | 0.5s, `inactive/dead/success`, `is-failed` → `inactive` | 0.6s, `inactive/dead/success`, `is-failed` → `inactive` |
| `start` again | rc 0 in **10.7s**, `active/running` | rc 0 in **10.8s**, `active/running` |
| `restart` | rc 0 in **11.8s**, `active/running` | rc 0 in **11.4s**, `active/running` |

**`start` returning still means healthy**, and that is visible in the numbers:
every start above took ten seconds or more because `Notify=healthy` held the
unit in `activating` until `/api/health` answered. It is visible more directly
during a recreate, below, where the container is `Up 10 seconds (starting)`
while the unit still reads `activating` — and only becomes `active` in the same
step the container becomes `healthy`.

A start timed from an already-active unit is **not** a start time: systemd
returns without running `ExecStart` at all. The probe says so rather than
printing 7ms as though Hive had started in 7ms.

### Recreate reattaches the existing volume

Proven with a marker file written into `/data` inside the running container,
which is on the `hive-data` volume and not in the image:

```
$ podman exec hive sh -c 'echo lifecycle-marker-... > /data/.lifecycle-probe'
$ ls -l .../volumes/hive-data/_data/.lifecycle-probe     # present on the host
$ podman run --rm --entrypoint sh ghcr.io/hivecommons/hive:stable -c 'ls /data/.lifecycle-probe'
ls: cannot access '/data/.lifecycle-probe': No such file or directory
```

The marker survived a stop/start, a restart, and a recreate, in both root
modes. **And the test has teeth**: with the volume itself removed
(`systemctl stop hive-data-volume.service; podman volume rm hive-data`) the
service still started cleanly in 11.4s and the marker was **gone** — the volume
unit had recreated an empty `hive-data`, carrying the #4210 ownership labels.
That is exactly the "silently starting empty" failure the acceptance criteria
asked to rule out, and the only thing distinguishing it from a healthy start is
the marker.

`ExecStop` is `podman rm -v -f -i hive`. The `-v` looks alarming and is not:
it removes *anonymous* volumes only, and `hive-data` is named. Measured — the
volume outlived every stop.

### Recreate recovers on its own

Removing the container out from under a running unit (`podman rm -f hive`):

```
t+3s    activating/auto-restart   (no container)
t+25s   activating/start          hive  Up 3 seconds (starting)
t+35s   active/running            hive  Up 13 seconds (healthy)
```

Rootful behaved the same way, sampled on a coarser interval:

```
t+5s    activating/auto-restart   (no container)
t+25s   activating/start          hive  Up Less than a second (starting)
t+45s   active/running            hive  Up 20 seconds (healthy)
```

`RestartSec=30` is the gap before the container reappears; the rest is the
healthcheck. The marker was intact afterwards in both modes.

### Boot wiring

`systemctl enable hive.service` **fails**, in both root modes:

```
$ sudo systemctl enable hive.service
Failed to enable unit: Unit /run/systemd/generator/hive.service is transient or generated
$ systemctl --user enable hive.service
Failed to enable unit: Unit /run/user/1000/systemd/generator/hive.service is transient or generated
```

What wires them instead is the `[Install]` section inside `hive.container`,
which the generator turns into a symlink in its own output directory on every
`daemon-reload`. Before #4478 the target was `default.target`; since #4478 it
is `hive-boot.target`, started by `hive-boot-gate.service` after the boot has
settled — see below:

```
l /run/systemd/generator/hive-boot.target.wants/hive.service          -> ../hive.service
l /run/user/1000/systemd/generator/hive-boot.target.wants/hive.service -> ../hive.service
```

So the only thing an operator enables is `hive-boot-gate.service` — a plain
unit, so enabling it works — and for the generated units `is-enabled` reports
`generated` rather than `enabled`, permanently, in every configuration.

### `%E` in a rootful unit, measured

[#4354](podman-standalone-quadlet.md) recorded that the rootful half of `%E`
came from `systemd.unit(5)` rather than from a measurement. It is measured now
— from the running rootful container:

```
/etc/hive/hive.yaml                             -> /etc/hive/hive.yaml
/etc/hive/secrets                               -> /secrets
/var/lib/containers/storage/volumes/hive-data/_data -> /data
```

### Lingering, and what happens without it

Lingering decides whether `user@UID.service` exists at boot at all. Measured on
a throwaway user, with **no login session**, which is what boot looks like:

| | `Linger` | `user@UID.service` | the user's enabled unit |
| --- | --- | --- | --- |
| A. before | `no` | `inactive` | never ran |
| B. `loginctl enable-linger` | `yes` | `active/running` within **305ms** | started, no session involved |
| C. `loginctl disable-linger` | `no` | `inactive/dead` | process **gone** |

State B is the boot path: `loginctl list-users` showed the user as `lingering`
with no login session, and logind started its manager and the unit anyway —
the same thing it does at startup for every lingering user. State C is the
failure operators actually hit. Throughout all three the unit's
`default.target.wants/` symlink was present and unchanged, which is the point:
**nothing in the unit's own enablement notices that it will never run.**

One artifact worth recording because it nearly became a wrong result:
`systemctl --user -M <user>@ is-active` **starts the user manager it is asked
about**, so querying state C that way reported the unit as active and recreated
its marker file. The table above was re-measured without any `-M` query.

## Reboot persistence: executed, both modes

#4413, on a host that could actually be rebooted. Two reboots, one per root
mode: both `hive-gateway.container` units carry `PublishPort=3001:3001`, and
rootless can bind 3001 because it is above 1024, so the two stacks contend for
the port and cannot be staged together.

| | Came back | Healthy after boot | `hive-data` marker |
| --- | --- | --- | --- |
| rootless, lingering enabled, actual reboot | **yes** | **11.6s** after the user manager started | **identical** |
| rootful via the system manager, actual reboot | **yes** | **16.1s** after userspace started | **identical** |

Both figures are time-to-*healthy*, not time-to-spawned: `Notify=healthy` holds
the unit in `activating` until `/api/health` answers, which is the whole reason
ADR-0017 chose Quadlet.

Host: Bluefin (Fedora Silverblue) 44.20260818, kernel 7.1.4, podman 5.8.4,
cgroup v2, netavark, SELinux enforcing. Both rows ran the same image —
`d465287054`, `sha256:73d88c4c…` — which took a deliberate step; see
[the tag moved](#the-tag-moved-mid-experiment-and-that-is-why-the-rows-are-pinned).

### What "came back" was taken to mean

Not `is-active`, and not the probe alone. The container serves on two ports and
the unit's `HealthCmd` probes one of them: 3002, the Go API. The Node auth proxy
on 3001 — what `nginx.conf`'s `upstream hive_api { server hive:3001; }` points
at, and therefore what an operator reaches — is not probed at all (#4476). That
was found the hard way while staging the rootful row: with `HIVE_DASHBOARD_TOKEN`
unset, the proxy refused to start, and `hive.service` reported `active` with its
container `healthy` for the entire two minutes in which the deployment was
unusable.

So a green `boot-check` alone would not have been evidence. Each row was
recorded only after all four of:

```sh
bin/hive-podman-lifecycle-probe.sh boot-check [--rootful]
systemctl [--user] is-active hive.service hive-gateway.service \
                             hive-network.service hive-data-volume.service
curl -sf http://127.0.0.1:3001/api/health          # through the published gateway
podman exec hive cat /data/reboot-marker.txt       # armed before the reboot
```

Both rows passed all four. On both boots the 3001 and 3002 probes agreed — but
that agreement is a result here, not an assumption.

### rootful

```
$ bin/hive-podman-lifecycle-probe.sh boot-check --rootful

Boot persistence -- rootful (system manager)
        uptime      up 1 minute
        booted at   2026-08-21 14:11:33
        is-active   active
        state       active/running/success/0
  PASS  hive.service is active after boot
        became active 16s after userspace started
        (Notify=healthy, so that is time-to-HEALTHY, not time-to-spawned)

Result
  no findings
```

Measured against `UserspaceTimestampMonotonic=2.304s`:

| Unit | Active at | After userspace |
| --- | --- | --- |
| `hive-network.service` | 6.500s | +4.14s |
| `hive-data-volume.service` | 6.499s | +4.14s |
| `hive.service` | 18.503s | **+16.15s** |
| `hive-gateway.service` | 19.211s | **+16.85s** |

```
$ curl -sf http://127.0.0.1:3001/api/health
{"status":"ok"}
$ podman exec hive cat /data/reboot-marker.txt
2026-08-21T18:05:09+00:00        # armed before the reboot, byte-identical after
```

### rootless, with lingering

Lingering was enabled before the reboot and survived it. #4441 measured
`Linger=no` on this host and deliberately left it; the rootless row is
meaningless without it, since a rootless result without lingering proves the
negative case rather than the positive one.

```
$ loginctl show-user "$USER" -p Linger
Linger=yes

$ bin/hive-podman-lifecycle-probe.sh boot-check

Boot persistence -- rootless (user manager, uid 1000)
        uptime      up 1 minute
        booted at   2026-08-21 14:19:30
        is-active   active
        state       active/running/success/0
  PASS  hive.service is active after boot
        became active 11s after userspace started
        (Notify=healthy, so that is time-to-HEALTHY, not time-to-spawned)

Result
  no findings
```

| Unit | Active at | After `user@1000` | After userspace |
| --- | --- | --- | --- |
| `user@1000.service` | 6.052s | — | +3.75s |
| `hive-network.service` | 6.453s | +0.40s | +4.15s |
| `hive-data-volume.service` | 6.459s | +0.41s | +4.15s |
| `hive.service` | 17.620s | **+11.57s** | +15.32s |
| `hive-gateway.service` | 18.503s | +12.45s | +16.20s |

The probe's "11s" is measured from the user manager, which is the honest
reference for a rootless unit: nothing could have started before `user@1000`
existed, and logind only created it because `Linger=yes`.

```
$ curl -sf http://127.0.0.1:3001/api/health
{"status":"ok"}
$ podman exec hive cat /data/reboot-marker.txt
2026-08-21T18:18:18+00:00        # armed before the reboot, byte-identical after
```

The two `hive-data` volumes are in different stores — the rootful one under
`/var/lib/containers`, the rootless one under `~/.local/share/containers` — so
the two markers are two independent proofs, not one volume read twice.

### Rootful Hive is inside the system boot transaction; rootless is not

Not something either row set out to measure, but it fell out of the timings and
it is the sharpest behavioural difference between the modes.

| | userspace | total boot | Hive healthy at |
| --- | --- | --- | --- |
| boot 1, **rootful** | 16.854s | 19.211s | gateway 19.2107s |
| boot 2, **rootless** | 6.928s | 9.233s | gateway 18.503s |

On the rootful boot, `systemd`'s own `FinishTimestampMonotonic` was
**19.2112s** — 549µs after `hive-gateway.service` became active. The boot was
not declared finished until Hive was serving. Both units carry
`WantedBy=default.target`, and in the system manager that target is the boot
transaction, so a rootful Hive is on its critical path.

In the user manager the same `WantedBy=default.target` resolves to the *user*
manager's default target, which logind starts after the system boot has already
completed. Hence boot 2 finishing in 9.2s with Hive not healthy until 18.5s.

The consequence is worth stating plainly, because it was the operational cost
of choosing rootful: with the units as they shipped then, a Hive that never
became healthy held a rootful boot for its `TimeoutStartSec` — 5min for
`hive.service`, 2min for `hive-gateway.service`. The 120s is not hypothetical
— the first rootful start attempt here sat in `activating` for exactly that
before timing out. On rootless the same failure delays nothing but Hive
itself. Filed as #4478; the units have since been rewired so that neither mode
puts Hive in the boot transaction — see
[the fix, measured](#the-fix-shipped-for-4478-measured-the-same-way) below.

#### The held boot was inferred, and is now measured

The paragraph above once ended "a deliberately-broken rootful boot was not
executed", because executing it meant rebooting the measuring host into a
broken state. It is executed now, somewhere disposable: a systemd container,
whose PID 1 runs the same system manager and the same job transaction.
`src/deploy/probe_boot_transaction_coupling.sh` is the probe; three cases differ
in exactly one thing, whether the unit is reached through
`default.target.wants/`.

| case | in `default.target.wants/` | READY | boot finished |
| --- | --- | --- | --- |
| control | yes | sent at once | **+0.154s** — 8.1ms after the unit went active |
| broken | yes | **never sent** | **+20.315s** — held **20.188s**, the unit's whole `TimeoutStartSec` |
| unwired | **no** | never sent | **+0.133s** |

`TimeoutStartSec=20s` throughout, to keep the probe short; the shape is what
scales, not the number.

The control reproduces the 549µs coupling of the real rootful reboot above: the
manager does not declare the boot finished until a `WantedBy=default.target`
`Type=notify` unit is ready. The broken case is the consequence — `Result=timeout`,
and the whole timeout spent inside the boot transaction.

**The third case is the argument.** Without it, the second only shows that a
failing unit takes `TimeoutStartSec` to fail, which nobody doubted. The same
unit with the same timeout, not wired into `default.target`, finishes the boot
in 0.133s; starting it by hand afterwards blocks the caller for 20s. The cost
does not vanish on rootless, it moves off the boot and onto whoever asked.

**What this does and does not establish.** It establishes the *manager*
behaviour, which is the half that was inferred. It is not a bare-metal boot —
no firmware, no initrd — and the stand-in unit is a `sleep`, not a Hive. The
Hive half is what the two real reboots above already measured. What was missing
was the link between them, and the link is a property of systemd, not of the
image.

#### The fix shipped for #4478, measured the same way

The decision #4478 held open is taken: the shipped units no longer sit in the
boot transaction, in either mode. Of the options the issue listed,
`DefaultDependencies=`/ordering (option 2 as first written) was measured and
**does not work** — a start job in the boot transaction delays the
boot-finished timestamp regardless of ordering, and a `DefaultDependencies=no`
variant held the boot the full 20s just like `broken`; an immediately-elapsing
timer fails the same way, because its job is enqueued before the initial queue
empties. Shortening the rootful `TimeoutStartSec` (option 3) was rejected: 300s
and 120s are load-bearing — `HealthStartPeriod` plus the retry budget plus the
headroom a ~3.8GB first pull needs — and the D-Bus start-job timeout is what
`podman auto-update --rollback` keys on (#4407).

What ships instead is the one shape that measurably decouples: the units are
`WantedBy=hive-boot.target`, which nothing at boot wants, and
`hive-boot-gate.service` — the only Hive unit in `default.target`, `Type=exec`,
so its own start job costs the transaction nothing — waits for
`systemctl is-system-running --wait` to return (which happens at exactly the
manager's `FinishTimestamp`, in every terminal state) and then starts the
target with `--no-block` as a **new** transaction. The probe's fourth case is
that shape in miniature, same never-ready unit, same 20s timeout:

| case | wiring | boot finished | the unit itself |
| --- | --- | --- | --- |
| broken | `default.target.wants/` (pre-#4478) | **+20.297s** — held 20.167s | `Result=timeout` |
| decoupled | `hive-boot.target` + gate (shipped) | **+0.122s** | still auto-started, still `Result=timeout`, `Restart=` still cycling |

Everything the timeout semantics protect is preserved and asserted by the
probe: the unit still starts on every boot, its start job still times out with
`Result=timeout` — the result `podman auto-update --rollback` reads — an
interactive `systemctl start` still blocks until healthy, and `Restart=always`
still recovers it. The only thing that changed is who pays for a Hive that
cannot become healthy: the Hive units, not the host's boot. If the gate cannot
wait (`is-system-running --wait` missing or erroring), it starts the target
immediately, which is exactly the pre-#4478 behaviour — the failure mode is
losing the decoupling, never losing the deployment.

### The tag moved mid-experiment, and that is why the rows are pinned

`ghcr.io/hivecommons/hive:stable` was pulled twice, once per store:

```
rootful  store, pulled 17:46:43Z -> sha256:73d88c4c…  (image d46528705417)
rootless store, pulled 18:07:53Z -> sha256:461ae71a…  (image 880525a77498)
```

Twenty-one minutes apart, two different images. Left alone, the two rows would
have differed in root mode *and* in what they ran, and a rootless failure could
not have been attributed to either. This is exactly the floating-tag problem
[the update and rollback doc](podman-quadlet-update-rollback.md) exists for.

The rootless row was therefore pinned to the digest the rootful row had already
run, through the documented drop-in rather than an edit to the unit:

```
$ bin/hive-podman-update.sh pin ghcr.io/hivecommons/hive@sha256:73d88c4c…
  PASS  wrote ~/.config/containers/systemd/hive.container.d/10-image.conf
  PASS  restart returned 0 after 10s, state active/running/success
$ podman inspect hive --format '{{.Image}}'
d46528705417c049d79cc02d230070ac5264bf93ba29d981743db9e14f18673d   # == rootful
```

The pin survived the reboot along with everything else. #4413 says not to change
the units, and this does not: the base `hive.container` is untouched and still
names the floating tag.

### What this does not cover

- **One host, one distribution.** Fedora Silverblue with SELinux enforcing and
  cgroup v2. Nothing here says what a SELinux-permissive or cgroup v1 host does.
- **Two boots, not a sample.** Each row is a single reboot. The times are what
  those boots did, not a distribution.
- **A clean shutdown.** Both reboots were `systemctl reboot`. Neither row says
  what a power cut does to `hive-data` mid-write.
- **The dashboard was reached, not exercised.** `/api/health` answered through
  the gateway; no agent work was driven across the boot.

To re-run it, arm and check first — `boot-check` refuses to present a delta
larger than ten minutes as boot evidence, since a unit started by hand hours
after boot also has an `ActiveEnterTimestamp` later than boot:

```sh
podman exec hive sh -c 'date -Is > /data/reboot-marker.txt'   # arm
bin/hive-podman-lifecycle-probe.sh check [--rootful]          # confirm the wiring
sudo systemctl reboot
# then, promptly, in a fresh login:
bin/hive-podman-lifecycle-probe.sh boot-check [--rootful]
curl -sf http://127.0.0.1:3001/api/health
podman exec hive cat /data/reboot-marker.txt
```

## The two unit changes, and why they come as a pair

```ini
SuccessExitStatus=143 SIGTERM
Restart=always
```

`SuccessExitStatus=143 SIGTERM` is what stops a deliberate stop being recorded
as a crash. It is not sufficient on its own, and this was measured rather than
reasoned: making 143 a success takes it out of `Restart=on-failure`'s set, so
with the old `Restart=on-failure` still in place an external
`podman rm -f hive` left the unit at

```
inactive/dead/success/143      # for 45s, and indefinitely
```

— service down, nothing restarted, and **nothing for monitoring to see**,
which is strictly worse than the `failed` state the first line was fixing.

`Restart=always` closes that. systemd never restarts a unit that a
`systemctl stop` job stopped, so a deliberate stop still stays stopped and
still reads clean; an externally removed container is recovered after
`RestartSec`. Both halves were measured in both root modes:

| | with `on-failure` | with `always` |
| --- | --- | --- |
| `systemctl stop` | `inactive/dead/success`, stays stopped | `inactive/dead/success`, stays stopped (verified over 35s) |
| `podman rm -f hive` | `inactive/dead/success`, **stays down** | `activating/auto-restart` → `active/running` healthy in ~35s |

`bin/hive-podman-lifecycle-probe.sh check` fails on `Restart=on-failure`
whenever `SuccessExitStatus` is set, so the pair cannot be split up again by
accident.

## Related

- [ADR-0017](adr/0017-podman-quadlet-lifecycle.md) — why Quadlet
- [podman-standalone-quadlet.md](podman-standalone-quadlet.md) — the units and how to install them
- [podman-quadlet-update-rollback.md](podman-quadlet-update-rollback.md) — moving the image and getting back; `SuccessExitStatus=143` and `Restart=always` from this page are what shape a failed update
- [podman-selinux-release-qualification.md](podman-selinux-release-qualification.md) — the `:z`/`:Z` measurement, and the precedent for recording a live run as not performed here

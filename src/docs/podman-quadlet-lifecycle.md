# Quadlet lifecycle: stop, start, restart, recreate, and boot persistence

What the Hive Quadlet units actually do when an operator drives them, measured
rather than inferred (#4377). [ADR-0017](adr/0017-podman-quadlet-lifecycle.md)
chose Quadlet for the persistent lifecycle and
[#4354](podman-standalone-quadlet.md) shipped the units; this page is the
lifecycle those two decisions implied, exercised.

Scope is lifecycle only: stop, start, restart, recreate, and boot persistence,
in both root modes. Update, rollback, and auto-update are their own slice, as
are backup, restore, and migration.

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

Exit codes follow the other Podman scripts: 0 no findings, 78 at least one
finding, 64 an unusable invocation.
`bin/test_hive_podman_lifecycle_probe.sh` covers it with every input mocked,
so the suite runs on a host with no Podman and no lingering user.

## What was measured

Fedora 44, Podman 5.8.4, cgroup v2, SELinux **enforcing**, against the real
`ghcr.io/kubestellar/hive:stable` image, in **both** root modes. Rootless
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
$ podman run --rm --entrypoint sh ghcr.io/kubestellar/hive:stable -c 'ls /data/.lifecycle-probe'
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

What wires them instead is `[Install] WantedBy=default.target` inside
`hive.container`, which the generator turns into a symlink in its own output
directory on every `daemon-reload`:

```
l /run/systemd/generator/default.target.wants/hive.service          -> ../hive.service
l /run/user/1000/systemd/generator/default.target.wants/hive.service -> ../hive.service
```

So there is nothing for an operator to enable, and `is-enabled` reports
`generated` rather than `enabled` — permanently, in every configuration. On
this host the rootful `WantedBy` resolved to `graphical.target`, because the
system `default.target` is an alias for it; on a headless server it resolves to
`multi-user.target`. Both are reached at boot.

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

## Reboot persistence: NOT executed here

Per #4377's stop condition, recorded as not executed rather than inferred.

| | Status |
| --- | --- |
| rootless, lingering enabled, actual reboot | **not executed** |
| rootful via the system manager, actual reboot | **not executed** |

The host this was measured on could not be rebooted: it was the host running
the session doing the measuring, and a reboot would have ended it mid-run. The
issue anticipates exactly this and calls for the row to be recorded as not
executed, which is what this is. `is-enabled` was **not** used as a substitute
— see above for why it could not have been one.

What *is* established without a reboot is every link in the chain except the
boot itself: the generator installs the `default.target.wants/` symlink in both
modes; `default.target` is reached in both managers; lingering starts a
sessionless user manager and its enabled units in 305ms, and its absence stops
them. What remains unproven is the composition of those links across an actual
kernel boot.

To close it, on a host that can be rebooted:

```sh
bin/hive-podman-lifecycle-probe.sh check              # confirm the wiring first
bin/hive-podman-lifecycle-probe.sh check --rootful
sudo systemctl reboot
# after boot, in a fresh login:
bin/hive-podman-lifecycle-probe.sh boot-check
bin/hive-podman-lifecycle-probe.sh boot-check --rootful
```

`boot-check` reports `is-active` and how long after userspace started the unit
became active, and — because `Notify=healthy` — that figure is time-to-healthy
rather than time-to-spawned. It refuses to present a delta larger than ten
minutes as boot evidence, since a unit started by hand hours after boot also
has an `ActiveEnterTimestamp` later than boot.

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
- [podman-selinux-release-qualification.md](podman-selinux-release-qualification.md) — the `:z`/`:Z` measurement, and the precedent for recording a live run as not performed here

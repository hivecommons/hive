# Health-aware auto-update: whether it works on this unit, and what it costs

`podman auto-update --rollback` decides an update went bad by restarting the
unit and seeing the restart fail. [#4378](podman-quadlet-update-rollback.md)
measured that `hive.service` **never reads `failed`** — `Restart=always` moves it
from a `TimeoutStartSec` expiry straight to `activating/auto-restart`, and
`systemctl is-failed` reports `activating` at every point during a bad update.

That looked like it ruled auto-update out. **It does not**, and the reason is the
whole result of this slice: those are two different signals.

## The short version

| | |
| --- | --- |
| Is health-aware auto-update achievable on `hive.container` as shipped? | **Yes.** No change to the unit. |
| What happens to `Restart=always`? | **Kept, untouched.** It never even fires — `NRestarts` stayed `0` through every rollback. No tradeoff is taken. |
| What detects "bad"? | The **D-Bus start-job result**, which is `timeout`. Not `ActiveState`, not `systemctl is-failed`. |
| Does #4378's finding still hold? | **Yes, unchanged.** Alerting keyed on `is-failed` or `ActiveState=failed` still does not fire. It is just not what podman uses. |
| What does a bad update cost? | One full `TimeoutStartSec` of downtime — ~5 minutes on the shipped unit — then automatic recovery **of the whole deployment**, gateway included (#4516). |
| And if the bad image stays published? | The **same outage on every timer firing**. Podman does not remember that it rolled a digest back. |
| Interaction with a #4378 digest pin? | The pin wins, **silently**: `UPDATED=false`, exit 0, unit untouched. |
| Is it on by default? | **No**, per [#4188]. Opt-in only: `bin/hive-podman-update.sh autoupdate on`. |
| `hive-data`? | Survived every direction, proven with a marker file. |
| Both root modes? | **Yes, both executed.** Rootless first (#4411), then rootful under the system manager (#4447) — same trigger, same `is-failed` blind spot, same outcome. See [Rootful, under the system manager](#rootful-under-the-system-manager). |

## Why `is-failed` and podman's trigger are not the same thing

Podman restarts the unit over D-Bus and reads the **job result** that the restart
returns. A `TimeoutStartSec` expiry makes that result `timeout`; podman rolls
back on anything that is not `done`. `ActiveState` never has to reach `failed`
for that to happen — and on this unit it never does, because `Restart=always`
converts the timeout into another start attempt.

So #4378's measurement and this one are both true at once:

- **For an operator or a monitoring system**, the unit is a black hole during a
  bad update. `is-failed` says `activating`, `ActiveState` says `activating`,
  `NRestarts` says `0`. Nothing fires. That is #4378's finding and nothing here
  changes it.
- **For podman**, the restart it issued came back `timeout`, which is all it
  needs.

## What was measured

Podman 5.8.4, rootless, systemd user manager — the same version every
#4201/#4202/#4203 spike and #4378 itself ran on.

Driven against a **bad-but-startable** image, as [#4188] requires: one that runs
forever and never becomes healthy, **not** one that fails to pull. A pull failure
is a different and much easier case — podman never restarts the unit at all.

The fixture reproduces the decisive properties of `hive.container`
(`Notify=healthy`, `HealthCmd`, `HealthStartPeriod`, `HealthRetries`,
`TimeoutStartSec`, `Restart=always`, `SuccessExitStatus=143 SIGTERM`, a named
volume at `/data`) against a local registry, with the timers **scaled down** so a
run finishes in under a minute: `HealthStartPeriod=6s`, `HealthRetries=3`,
`HealthInterval=2s`, `TimeoutStartSec=30s`, versus 120s/3/10s/300s shipped. The
mechanism under test is timer-independent; the *costs* below are given as
multiples of `TimeoutStartSec` so they carry over. The entrypoint traps `SIGTERM`
and exits 143, as the real one does — an earlier run without that trap inflated
the measured downtime by ~50s of stop-timeout that the real unit does not pay.

### A bad update, detected and rolled back

`:stable` moved from the good image to the bad one, then a single
`podman auto-update --rollback=true`:

```
UNIT                 CONTAINER                   IMAGE                              POLICY    UPDATED
hivefixture.service  f6c9a572ed04 (hivefixture)  localhost:5000/hivefixture:stable  registry  rolled back
auto-update elapsed_s=36
```

```
t+003s  active/running/success        is-failed=active       NRestarts=0  container=good
t+006s  activating/start/success      is-failed=activating   NRestarts=0  container=bad
t+009s  activating/start/success      is-failed=activating   NRestarts=0  container=bad
  …                                                                       (bad image running, never healthy)
t+033s  activating/start/success      is-failed=activating   NRestarts=0  container=bad
t+036s  activating/start/success      is-failed=activating   NRestarts=0  container=good
t+039s  active/running/success        is-failed=active       NRestarts=0  container=good
```

Three things in that timeline are worth an operator's attention.

**36 seconds against `TimeoutStartSec=30`.** The cost of a bad update is one full
start budget plus a few seconds of stop/start. Podman cannot know the image is
bad any sooner — that is what the budget is for. On the shipped unit's 300s,
expect **~5 minutes of downtime**, then automatic recovery to the previous image.

> **The gateway is part of "recovery", and did not used to be.** `hive-gateway.service`
> carries `Requires=hive.service`, which propagates a **stop** only. A failed update
> stops the gateway along with the failing unit, and podman's rollback restart
> re-starts only those dependents that are *active* when the job runs — which the
> gateway is not. Measured on a rootless deployment at the shipped `TimeoutStartSec=300`:
> the rollback restored the digest correctly, `hive.service` went `active`, `NRestarts`
> stayed `0`, `podman auto-update` exited `0` — and `:3001`, the only published port,
> stayed dead until a human noticed. `hive.container` now carries
> `Wants=hive-gateway.service` so every path that starts Hive brings the gateway with
> it ([#4516](https://github.com/hivecommons/hive/issues/4516)).

**`is-failed` never leaves `activating`, and `NRestarts` never leaves `0`.**
#4378's warning stands: monitoring keyed on unit state does not fire. Worse for
this feature, `podman auto-update` **exits 0** when it rolls back, so
`podman-auto-update.service` succeeding tells you nothing either. The only signal
is the `UPDATED` column reading `rolled back`.

**`Restart=always` is not involved.** `NRestarts=0` throughout: podman's rollback
restart lands before `RestartSec` elapses, so the auto-restart loop never starts.
Keeping `Restart=always` costs this feature nothing, and removing it — which
would expose `failed` for alerting — would remove the crash recovery #4377 added
it for. **Kept, unchanged.**

### It does not remember

A second `podman auto-update --rollback=true`, with **nothing changed** in the
registry — `:stable` still points at the bad image:

```
hivefixture.service  e997e2bf339d (hivefixture)  localhost:5000/hivefixture:stable  registry  rolled back
elapsed_s=102
```

Rolled back again, at full cost. Podman keeps no record that it just rejected
that digest, so **every timer firing repeats the whole outage** until the bad
image is replaced upstream. `podman-auto-update.timer` is daily by default, so
this is a recurring daily outage, not a one-off. This is the single strongest
reason the feature is opt-in.

*(The 102s here rather than 36s is the earlier fixture without the `SIGTERM`
trap; the extra ~66s is stop-timeout the real unit does not pay. The behaviour —
a second full rollback cycle — is the measurement.)*

### Auto-update versus a #4378 digest pin

`AutoUpdate=registry` polls a floating tag. #4378's rollback pins a **digest**.
Both want the same `Image=` line. Measured, with `10-image.conf` pinning a digest
and `:stable` moved to the bad image:

```
UNIT                 CONTAINER     IMAGE                                POLICY    UPDATED
hivefixture.service  684a9270f271  localhost:5000/hivefixture@sha256:…  registry  false
rc=0 elapsed_s=1
after     : active/running/success  container=good   <- pin held, no outage
```

**The pin wins, and it wins silently.** A digest cannot change, so auto-update
reports `UPDATED=false`, exits 0, and does not touch the unit — output that is
*indistinguishable from "already up to date"*. A host left pinned after a manual
rollback therefore has a daily timer reporting success while updating nothing,
possibly for months.

That is safe but invisible, so the tooling makes it loud:
`hive-podman-update.sh autoupdate on` **refuses** while a pin is in place, and
both `status` and `autoupdate status` flag the combination when it exists.

There is a worse case. If the pinned digest stops resolving in the registry —
garbage-collected, or pinned across repositories, which #4378's own failure run
did when it pinned `docker.io/library/nginx@sha256:…` onto a unit whose tag is
`ghcr.io/kubestellar/hive:stable` — auto-update does not skip it:

```
hivefixture.service  0fd250ba7b08  localhost:5000/hivefixture@sha256:3775a702…  registry  failed
Error: checking image updates for container 0fd250ba7b08…: reading manifest
sha256:3775a702… in localhost:5000/hivefixture: manifest unknown
rc=125
```

`rc=125` fails **the entire auto-update run**, including for every other
auto-update container on the host. The unit itself was untouched.

### `hive-data` survived both directions

A marker written into the named volume before the update:

```
marker written to /data:  hive-data-marker-A2
… bad image started, never healthy, rolled back …
final : active/running/success  container=good  marker=hive-data-marker-A2
```

Same method #4378 used, same result: the volume is not in the update path.

### The opt-in delivery works as a drop-in

`AutoUpdate=` is a `[Container]` key, so it ships the same way #4378's pin does —
a Quadlet drop-in — and the two compose in one `hive.container.d/` directory:

```
base unit, no drop-in:            autoupdate label = ''      (not listed by auto-update)
+ 20-autoupdate.conf:             autoupdate label = 'registry'
+ 10-image.conf as well:          label = 'registry', ExecStart = …@sha256:…  (pin held)
```

## Using it

```sh
# read what it costs first — particularly "it does not remember"
bin/hive-podman-update.sh autoupdate status
bin/hive-podman-update.sh autoupdate on           # rootless
bin/hive-podman-update.sh autoupdate on --rootful # rootful
bin/hive-podman-update.sh autoupdate off
```

`on` copies [`../deploy/quadlet/optional/hive-autoupdate.conf`](../deploy/quadlet/optional/hive-autoupdate.conf)
to `<quadlet dir>/hive.container.d/20-autoupdate.conf`, runs `daemon-reload`,
enables `podman-auto-update.timer`, and **restarts the unit** — the policy label
is applied at container-create time, so a container that predates the drop-in is
invisible to auto-update until it is recreated. `off` reverses all of it.

The two drop-ins are deliberately separate files: `unpin` does not disable
auto-update, and `autoupdate off` does not drop a pin someone is relying on.

### What to monitor

Not the unit. Measured, during a bad update the unit reports `activating`,
`is-failed` reports `activating`, `NRestarts` reports `0`, and
`podman-auto-update.service` exits 0 whether it rolled back or did nothing.

Watch the auto-update run's own output for `rolled back` in the `UPDATED`
column — for example:

```sh
journalctl --user -u podman-auto-update.service --since -1d | grep -E 'rolled back|failed'
```

A `rolled back` line means the published image is bad and **will be retried on
the next firing**. The response is to pin deliberately (which also stops the
retries) and let the bad tag be fixed upstream:

```sh
bin/hive-podman-update.sh autoupdate off
bin/hive-podman-update.sh pin ghcr.io/kubestellar/hive@sha256:<last good>
```

## Rootful, under the system manager

Executed (#4447). The rootless run above left this as reasoning — "nothing in
podman's rollback path is uid-dependent" — and rootful is the wrong half to leave
unproven: it is the **enforcing** cell in
[the support matrix](podman-support-matrix.md), the mode where the egress gate is
fully installed.

Same fixture and the same scaled timers as the rootless run, so the two are
comparable: units installed to `/etc/containers/systemd/`, driven through
`bin/hive-podman-update.sh ... --rootful`, which uses `sudo` against the system
manager.

**Arming it.** `autoupdate on --rootful` installed the drop-in, enabled the
system timer, and — the part that matters, because it is what
`podman auto-update` acts on — the recreated container really carries the label:

```
  PASS  installed /etc/containers/systemd/hive.container.d/20-autoupdate.conf
  PASS  daemon-reload: the unit now carries AutoUpdate=registry
  PASS  enabled podman-auto-update.timer
  PASS  restart returned 0 after 6s, state active/running/success
  PASS  container label: registry

io.containers.autoupdate = 'registry'
system timer             = enabled
```

**The bad update.** The same bad-but-startable image — one that runs and never
becomes healthy, not one that fails to pull — published to the same tag:

```
UNIT          CONTAINER            IMAGE                              POLICY    UPDATED
hive.service  0028344b7801 (hive)  127.0.0.1:5000/hivefixture:stable  registry  rolled back
elapsed_s=37        (rootless was 36s, TimeoutStartSec=30)
```

```
t+03s  active/running/success/0             is-failed=active
t+06s  activating/start/success/0           is-failed=activating
  …                                          (bad image running, never healthy)
t+30s  activating/start/success/0           is-failed=activating
t+33s  deactivating/stop-sigterm/timeout/0  is-failed=deactivating
t+36s  activating/start/success/0           is-failed=activating
t+39s  active/running/success/0             is-failed=active
```

**The rollback trigger is the same one**, which is the single thing most likely
to differ between managers and the reason this row was worth executing rather
than reasoning about. systemd's own record of the failed start, from the system
journal:

```
hive.service: start operation timed out. Terminating.
hive.service: Failed with result 'timeout'.
Failed to start hive.service - Hive (4447 rootful auto-update fixture).
```

`Result=timeout`, visible in the sample at t+33s as well — and `ActiveState`
never reaches `failed`. So podman rolled back on the start-job result under the
system manager exactly as it did under the user manager.

**The monitoring warning carries over, measured rather than assumed.**
`systemctl is-failed` reported `active`, `activating` and `deactivating` and at
no point `failed`, and `NRestarts` stayed `0` throughout. An alert keyed on
`failed` does not fire on a rootful bad update either.

**`hive-data` survived.** A marker written into the volume before the update was
still there afterwards, with the container back on the good image:

```
container  : good
hive-data  : rootful-marker-4447
```

**Reversal.** `autoupdate off --rootful` removed the drop-in and returned the
system timer to `disabled / inactive`, which is what it was before the run:

```
  PASS  removed /etc/containers/systemd/hive.container.d/20-autoupdate.conf
  PASS  disabled podman-auto-update.timer
```

## Not executed here
- A genuinely broken published Hive image. The bad image is a stand-in, recorded
  plainly the way #4378 recorded its own — what is being measured is what the
  *unit* does when the new image never becomes healthy, which does not depend on
  why it doesn't.
- The shipped 300s timers end to end. The scaled fixture is what was driven; the
  costs above are stated as multiples of `TimeoutStartSec` for that reason.
- Any change to the Docker/Watchtower path, which #4411 rules out.

## Related

- [#4378 / #4407](podman-quadlet-update-rollback.md) — the manual path this
  builds on, and the `failed`-state measurement above
- [ADR-0017](podman-standalone-quadlet.md) — the unit and its install
- #4354 the unit, #4377 lifecycle in both root modes, #4188 the umbrella

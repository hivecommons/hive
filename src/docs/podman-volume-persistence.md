# `hive-data` under SELinux enforcing: what the volume actually guarantees

[#4354](https://github.com/hivecommons/hive/issues/4354) shipped
`src/deploy/quadlet/hive-data.volume` and said, in the unit itself, that
"persistence and SELinux volume semantics are deliberately NOT characterised
here — that is its own slice". This is that slice.

It builds on [the SELinux release qualification](podman-selinux-release-qualification.md)
(#4337) and [the AVC evidence](podman-selinux-avc-evidence.md) (#4357) rather
than re-deriving them. Those two established the `:z` / `:Z` / unlabeled matrix
for **bind mounts**, with the kernel's own AVC records per case. Neither
measured the **named volume**, and the named volume is the interesting one:
podman labels it *itself, without being asked*, and the label it picks is what
decides whether a recreated container can still read the data.

Scope, matching the issue: label, ownership, what survives container removal and
recreation, and how a bind mount differs. **Not** backup/restore or migration
(#4188 bullet 8) and **not** reboot persistence (bullet 6).

## Result

Every row is **observed** on this host unless the table says otherwise.
[What was not executed](#what-was-not-executed) lists what is expectation only.

| | Measured |
| --- | --- |
| label on a fresh volume's `_data` | `system_u:object_r:container_file_t:s0` |
| MCS category on it | **none** — level is bare `s0` |
| who applies it | **podman, at `volume create`** — the parent directory is `data_home_t` and a directory made by hand beside the volume stays `data_home_t` |
| ownership before first use | host `1000:1000` (the invoking user) |
| ownership after first use | host `525288:525287` = container `1001:1000`, matching the image's `/data` — podman's copy-up chown |
| label after first use | unchanged |
| survives `podman rm` | **yes** |
| survives `systemctl stop` (generated `ExecStart` carries `--rm`) | **yes** — the container is deleted, the volume is not |
| survives deleting the unit files, `daemon-reload`, reinstalling, starting | **yes** — same `hive-id`, same proxy CA digest |
| read back by a *differently categorised* container | **yes**, every time |
| `:Z` added to the volume line | stamps a private category; the next mount **without** the flag is **denied** |
| bind source with no flag | **denied** (`user_tmp_t`) |
| `EnvironmentFile=` source | never relabeled — it is not a mount |
| `:z`/`:Z` effect on modes and ownership | **none** — nothing widened |

## The label, and the command that shows it

```sh
$ podman volume create hive-data
$ MP=$(podman volume inspect hive-data --format '{{.Mountpoint}}')
$ /usr/bin/stat -c '%C' "$MP"
system_u:object_r:container_file_t:s0
```

`container_file_t` is the type a container can read and write. The **level** is
bare `s0`: **no MCS category is applied.** That single fact is what the rest of
this page follows from.

Use `/usr/bin/stat -c '%C'`, not `stat -c '%C'`, and not `ls -Z`. #4357 finding 1
measured that where uutils coreutils shadows GNU on `PATH` — the default on
Bluefin/Universal Blue — uutils `stat` prints `unsupported for this operating
system` **to stdout and exits 0**, and uutils `ls -Z` prints no context at all.
Either one flows into a comparison as if it were a label. The probe resolves its
reader by trying it against a known file and requiring an `:object_r:` field.

**It is podman's doing, not inheritance.** The distinction matters, because an
inherited label would be an accident of where volumes happen to live:

```sh
$ /usr/bin/stat -c '%C' ~/.local/share/containers/storage/volumes
unconfined_u:object_r:data_home_t:s0
$ mkdir -p ~/.local/share/containers/storage/volumes/by-hand/_data
$ /usr/bin/stat -c '%C' ~/.local/share/containers/storage/volumes/by-hand/_data
unconfined_u:object_r:data_home_t:s0      # a hand-made directory keeps data_home_t
```

## Ownership: the copy-up

The volume root's owner **changes on first use** and its label does not:

```
volume root before first use:  1000:1000        (the invoking user)
volume root after first use:   525288:525287    (host view)
image /data owner:             1001:1000        (container view: dev:node)
```

`525288:525287` is container `1001:1000` seen through this user's
`subuid`/`subgid` range. Podman copies the image's `/data` into the empty volume
on first mount and chowns the volume root to match it, so the Hive service —
which runs as `dev` (1001) — owns its own state directory without the operator
doing anything. The image's `/data` ships `home/`, `litellm/`, and `nous/`, and
those three appear in the volume after first use.

One cosmetic consequence, recorded so it is not mistaken for a problem: entries
**copied up from the image** carry `unconfined_u:object_r:container_file_t:s0`
and entries **created by the container** carry
`system_u:object_r:container_file_t:s0`. The type and the level — the two fields
type enforcement and the MCS constraint actually consult — are identical. Only
the SELinux *user* field differs, reflecting which process created the entry.

## What survives

The generated `ExecStart` carries `--rm`, so **the container is deleted every
time the unit stops.** That makes the volume the only thing that persists, which
is worth stating plainly rather than leaving as an inference.

Live, on a real `ghcr.io/kubestellar/hive:stable` started through
`hive.container` and `hive-data.volume`. Hive wrote its own state — `hive-id`,
`hive-state.json`, `hive.yaml.runtime`, `proxy-ca.pem`, `beads/`, `agents/` — and
a sentinel was planted alongside it:

```sh
$ podman exec hive sh -c 'cat /data/hive-id; sha256sum /data/proxy-ca.pem'
hive-keen-ray
278dcad37f9148cfd7ffe99c44b6da34619fc271e5e129981438d6284a6d7eea  /data/proxy-ca.pem
```

Then the units were **deleted**, not merely restarted:

```sh
$ systemctl --user stop hive.service
$ podman ps -a --format '{{.Names}}' | grep -cx hive
0                                          # --rm deleted it
$ rm ~/.config/containers/systemd/hive.container ~/.config/containers/systemd/hive-data.volume
$ systemctl --user daemon-reload           # 0 generated units remain
$ install -Dm644 ...                       # reinstall both
$ systemctl --user daemon-reload           # 2 generated units
$ systemctl --user start hive.service      # returned 0 in 11s

$ podman exec hive sh -c 'cat /data/hive-id; sha256sum /data/proxy-ca.pem'
hive-keen-ray
278dcad37f9148cfd7ffe99c44b6da34619fc271e5e129981438d6284a6d7eea  /data/proxy-ca.pem
```

Same identity, same CA, and the sentinel intact. Hive said so itself in the
journal for that start:

```
{"level":"INFO","msg":"state restored","saved_at":"2026-08-20T21:01:33-04:00","agents":0}
{"level":"INFO","msg":"mode history restored","entries":1}
{"level":"INFO","msg":"cadence overrides restored","modes":4}
{"level":"INFO","msg":"governor mode restored","mode":"IDLE"}
```

**Every one of those containers had a different MCS category** — `c94,c887`,
then `c126,c769`, then `c87,c359` — and every one could read the data. That is
the bare `s0` on the volume doing its job. A category on the volume would have
made the second container a stranger to its own state.

### What does destroy it

| | |
| --- | --- |
| `systemctl stop hive.service` | no — deletes the container only |
| `systemctl stop hive-data-volume.service` | no — the generated unit is `Type=oneshot` with `RemainAfterExit=yes` and **no `ExecStop`**; stopping it runs nothing |
| `podman rm -v hive` | no — `-v` removes *anonymous* volumes; a named one is untouched (verified separately) |
| `podman volume rm hive-data` | **yes** |
| `podman volume prune`, and the system-wide prune's `--volumes` form (see podman-system-prune(1)) | **yes**, and they do not ask about this volume specifically |
| `bin/hive-podman-teardown.sh` | **yes** — by design. It selects on the #4210 ownership labels, and the volume carries them: `podman volume ls --filter label=io.kubestellar.hive.owned=true` returns `hive-data` |

## The one thing not to do: `:Z` on the volume line

The unit mounts the volume with no relabel suffix:

```
Volume=hive-data.volume:/data
```

Adding `:Z` there looks like hardening and is the opposite. Measured:

```sh
$ podman run --rm -v hive-data:/data:Z ...            # one :Z mount
$ /usr/bin/stat -c '%C' "$MP"
system_u:object_r:container_file_t:s0:c619,c667       # a private category now

$ podman run --rm -v hive-data:/data ... -c 'cat /data/probe.txt'
cat: /data/probe.txt: Permission denied

$ podman run --rm -v hive-data:/data:z ...            # :z puts it back
$ /usr/bin/stat -c '%C' "$MP"
system_u:object_r:container_file_t:s0
```

It does not fail immediately, which is what makes it dangerous. As long as
*every* mount carries `:Z`, podman re-relabels to the current container's
category on each start and the unit keeps working. It breaks the first time
anything mounts the volume without the flag — a `podman run` to look at the
data, a second container, an edit that drops the suffix — and per **#4357
finding 3** that denial is **silent**: a category denial produces no AVC and no
`SELINUX_ERR`, so `ausearch` returns nothing and an operator reasonably
concludes SELinux was not involved. The label on the file is the diagnostic;
compare its `cN,cN` against the container that needs it.

The data is never at risk — only the label changes, and `:z` restores it — but
the service is down until someone works out why.

## Named volume versus bind mount

Both appear in the same unit, and they want **opposite** treatment:

| Mount | Flag | Why |
| --- | --- | --- |
| `hive-data.volume:/data` | **none** | podman already labels it `container_file_t:s0`. It must stay category-free so every recreated container can read it. |
| `%E/hive/hive.yaml:/etc/hive/hive.yaml:ro,Z` | **`:Z`** | a host path keeps whatever type it had and is denied without a flag. Exactly one container reads it, so the private label is correct. |
| `%E/hive/secrets:/secrets:ro,Z` | **`:Z`** | same, and more so — `:z` is the **shared** label, and these are secrets. #4357 measured both flags leaving the `0440` mode and the hive-launch group ownership untouched. |
| `%E/hive/hive.env` (`EnvironmentFile=`) | **none, and none is possible** | not a mount |

The bind control, re-measured here only as the contrast that makes the volume
result mean something (#4357 is the authority on this matrix):

```sh
$ /usr/bin/stat -c '%C' /var/tmp/fixture/f
unconfined_u:object_r:user_tmp_t:s0
$ podman run --rm -v /var/tmp/fixture:/mnt:ro ... -c 'cat /mnt/f'
cat: /mnt/f: Permission denied                        # no flag: denied
$ podman run --rm -v /var/tmp/fixture:/mnt:ro,z ...   # -> container_file_t:s0        (shared)
$ podman run --rm -v /var/tmp/fixture:/mnt:ro,Z ...   # -> container_file_t:s0:c912,c943 (private)
$ /usr/bin/stat -c '%a' /var/tmp/fixture/f
644                                                   # neither flag widened anything
```

And with the real unit running, its own `:Z` mounts carried the running
container's exact category while the volume beside them carried none:

```
hive.yaml   system_u:object_r:container_file_t:s0:c87,c359   mode=644
secrets     system_u:object_r:container_file_t:s0:c87,c359   mode=750  gid=1002 (in the userns)
hive-data   system_u:object_r:container_file_t:s0            mode=755
container   system_u:system_r:container_t:s0:c87,c359
```

The secrets directory came through with its mode and group ownership unchanged.
**No remediation anywhere on this page disables SELinux or widens a mode**, and
none is needed: every case that failed, failed for a reason a label fixes.

### `EnvironmentFile=` is not a mount

`EnvironmentFile=` becomes `podman run --env-file`, which podman reads **on the
host** before the container exists. The file's label is never touched and never
needs to be:

```
before: unconfined_u:object_r:user_tmp_t:s0
after:  unconfined_u:object_r:user_tmp_t:s0     # variable arrived in the container
```

So do not "fix" a start failure by relabeling `hive.env`. If it fails, the cause
is that the file is missing — `podman run` fails outright on a missing
`--env-file`, and Quadlet does not honour systemd's leading `-` prefix (see
[the Quadlet page](podman-standalone-quadlet.md#traps-measured-not-guessed)).

## Environment

| | |
| --- | --- |
| SELinux | **Enforcing**, policy `targeted`, MLS enabled |
| Podman | 5.8.4, **rootless** |
| OCI runtime | crun 1.28 |
| Kernel / host | `7.1.4-200.fc44.x86_64`, Aurora 44.20260815.1 (Kinoite) |
| Policy packages | `container-selinux-2.250.0-1.fc44`, `selinux-policy-44.5-1.fc44` |
| Image | `ghcr.io/kubestellar/hive:stable` |
| Architecture | `amd64` only |
| coreutils | GNU 9.10 at `/usr/bin/stat`, **not** shadowed by uutils on this host |
| `subuid`/`subgid` | `524288:65536`, so container `1001:1000` is host `525288:525287` |

This is a Fedora **atomic** variant, as #4357's host was — a different one
(Aurora/Kinoite rather than Bluefin), and notably one where GNU coreutils is
*not* shadowed, so #4357 finding 1's trigger condition is absent here. That is
why the probe resolves its label reader rather than assuming either outcome.

## What was not executed

Recorded rather than implied, and none of it is a stop condition for this slice:

- **Rootful.** Everything above is rootless. A rootful volume lives under
  `/var/lib/containers/storage/volumes` and there is no user-namespace mapping,
  so the ownership numbers will differ. The *label* is **expected** to be the
  same `container_file_t:s0`, because it is podman that applies it — but that is
  expectation, not measurement.
- **Reboot persistence.** #4188 bullet 6, out of scope here. Nothing was
  rebooted.
- **Backup, restore, and migration.** #4188 bullet 8, out of scope here.
- **Non-atomic hosts.** Stock Fedora Server and CentOS Stream were not
  exercised. Same caveat #4357 records.
- **The mechanism behind podman's choice of the shared label.** Observed at the
  filesystem, not traced through podman's source. What is asserted is the
  behaviour, which is what the units depend on.
- **A second host.** One host, one architecture. Every number above is from it.

## Safety: what this touched

`:z` and `:Z` **relabel the host directory they are pointed at and the relabel
outlives the container.** So:

- Every bind-mount source was a fixture created under `/var/tmp` by the probe and
  removed on exit. No checkout, no real secrets directory, nothing under `$HOME`,
  `/usr`, or `/var/lib` was ever a mount target.
- The live unit run used a scratch configuration directory rather than
  `%E/hive`, so no operator configuration could be relabeled. Its agents were
  disabled in that scratch config.
- Every object the probe creates is named `hive-volprobe-<pid>-*`, and **the
  probe refuses to start if any of those names already exists.** Cleanup removes
  exactly the names it created. Nothing is pruned; no pre-existing volume is
  read, written, or removed.
- No `setenforce`. No `--security-opt label=disable`. No `--privileged`. No
  `chcon` outside the probe's own fixture. No `chmod` widening anything.

The probe does **not** pin `--root`/`--runroot` the way #4357's does, and that is
deliberate: the property being characterised is what podman does to a volume in
a real store, and a throwaway store would also mean re-pulling a 3.8 GB image to
say anything at all. Name discipline is the safety property instead. `--store`
runs it against an isolated store anyway, for a host where that trade is the
wrong way round.

## Reproducing

```sh
# Stop condition: this reports nothing unless the host is enforcing.
getenforce                                    # must print Enforcing
podman pull ghcr.io/kubestellar/hive:stable   # the probe does not pull

src/deploy/probe_podman_volume_persistence.sh
```

Exit `0` every case matched what this page records, `1` at least one did not,
`78` not qualifiable here. Options: `--image`, `--store`, `--keep`.

The `static` section — that the shipped units put no relabel suffix on the
volume line and **do** put `:Z` on the config and secret bind mounts — runs
before the enforcing gate, so it is checked even on a host where nothing else
here can be. All four of its assertions were confirmed to FAIL under the
corresponding mutation (`:Z` on the volume line, `:z` on the volume line, either
bind mount downgraded from `:Z` to `:z`) before being trusted.

**This probe is not wired into CI, deliberately**, and neither are
`probe_podman_selinux_avc.sh` or `qualify_podman_selinux.sh`. Hosted runners are
not SELinux-enforcing, so a CI job would reach the stop condition and report `78`
on every run — the vacuous pass these probes exist to avoid. This is the
per-release qualification lane #4337 established, run on a host of the right
class, with the result recorded here.

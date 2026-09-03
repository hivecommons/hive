# SELinux AVC evidence, and the hive-launch group secret

The follow-up to
[Release qualification: SELinux-enforcing Podman](podman-selinux-release-qualification.md)
(#4337). That document establishes the `:z`/`:Z`/unlabeled matrix and is not
superseded here. This one closes two gaps in it.

The first is evidential. #4337's suite decides "denied" from the container's
exit status — it never reads the audit log — so every denial it reports is
**inferred**, and a failure with nothing to do with SELinux would look
identical. This probe records the kernel's own AVC entries per case, against a
baseline taken before any container starts.

The second is the secret case the release image actually depends on: not a
`0600` file read by its owner, but `0440` owned by the **hive-launch** group
(GID 1002, pinned in `src/Dockerfile` and used as `fsGroup` in
`src/deploy/k8s/deployment.yaml`), read by `dev` (UID 1001) through a
*supplementary* group.

## Result

Executed on an enforcing host. Every case passed, but the passes are not the
interesting part — **three findings below say a shipped check or a shipped
remediation is wrong on a host of this class: two in #4209's preflight, one in
#4337's remediation table.**

| Case | Measured | AVC recorded |
| --- | --- | --- |
| control — no relabel flag | **denied** | **yes** — `container_t:s0:c590,c998` → `user_tmp_t:s0`, `{ open }` |
| `:Z` | granted, label `container_file_t:s0:c648,c780` | none |
| `:Z` isolation — 2nd container | **denied** | **none — silent** |
| `:z` | granted, label `container_file_t:s0`, no category | none |
| `:z` sharing — 2nd container | granted | none |
| secret `0440` gid 1002, dir `0700`, `:z` | **denied** | none — the refuser is DAC, not SELinux |
| secret `0440` gid 1002, dir `0750`, unlabeled | **denied** | **yes** — `container_t:s0:c495,c631` → `user_tmp_t:s0` |
| secret `0440` gid 1002, dir `0750`, `:z` | **granted** | none |
| secret `0440` gid 1002, dir `0750`, `:Z` | **granted** | none |
| `0440` mode and GID after both relabels | `440`, gid unchanged | — |
| #4209 preflight | detected Enforcing, read-only, no weakening remedy | — |

Container-domain denials over the run: 4 → 6. Ambient domains in the baseline
were `bootupd_t` (unrelated, and `permissive=1`) plus container denials left by
this probe's own earlier runs.

### Finding 1 — the preflight's label check is broken where uutils shadows GNU coreutils

`bin/hive-podman-preflight-host.sh` reads a mount label with:

```sh
stat_out="$(stat -c '%C' "$path" 2>/dev/null)" || stat_out=""
```

On a host where **uutils coreutils** is ahead of GNU coreutils on `PATH` —
the default on Bluefin/Universal Blue, i.e. exactly the Fedora-atomic class
this lane targets — `stat` does not implement `%C`. Measured:

```
$ stat -c '%C' /var/tmp/f      # uutils coreutils 0.9.0
unsupported for this operating system
$ echo $?
0
```

It prints the error **to stdout and exits 0**, so the `|| stat_out=""` guard
never fires and the string flows into the label comparison as if it were a
context. The preflight then emitted, verbatim:

```
△ Mount label: deploy/nginx.conf (gateway configuration) is unsupported for
  this operating system — a container cannot read it as-is
```

The consequence is worse than the garbled sentence. `_pfh_label_is_container_readable`
can never match, so on such a host the check **can never report a correctly
labelled path** — it warns unconditionally, including for a file already
labelled `container_file_t`. The `:z` advice it gave happens to be right, but it
is right by luck rather than from a reading of the label.

`ls -Z` is not a fallback: uutils `ls` accepts `-Z` and prints no context at
all. `/usr/bin/stat -c '%C'` and `getfattr -n security.selinux` both work here.
This probe resolves its label reader by trying it against a known file and
requiring a `:object_r:` field in the result, rather than assuming.

**Not fixed here** — this document records the defect; changing the preflight is
a separate change against #4209.

### Finding 2 — `mkdir -m 700` makes the secrets directory unreadable under rootless Podman

The preflight advises, for a missing secrets directory:

```
→ Only needed for GitHub App key auth. Create it narrow: mkdir -m 700 -p '.../secrets'
```

and elsewhere recommends mounting `secrets` with `:Z`. Under rootless Podman
that combination cannot work, and `:Z` cannot rescue it. Three measurements
isolate the cause — the middle one is the only variable:

| Secrets dir | Mount | Read as `dev` (1001) | Refuser |
| --- | --- | --- | --- |
| `0700`, owner = host user | `:z` | **denied** | DAC — no AVC recorded |
| `0750`, group 1002 | *unlabeled* | **denied** | MAC — AVC recorded |
| `0750`, group 1002 | `:z` / `:Z` | **granted** | — |

The host user maps to container **root**, not to `dev`:

```
$ podman run --rm ... --entrypoint /bin/sh $IMAGE -c 'id -u'
0
```

So a `0700` directory owned by the operator grants `dev` (1001) nothing — not
even the traverse bit — and the failure surfaces as `Permission denied` on the
key file. That reads exactly like an SELinux problem, is not one, and **no
labeling action the preflight offers will change it.** Row 1 confirms it: with
`:z` applied and SELinux therefore satisfied, the read still fails and the
audit log stays empty.

What works is the mode `fsGroup: 1002` already produces in the Kubernetes path:
group-owned by hive-launch with the group traverse bit set. The `0440` file
itself is fine — it is the directory above it that refuses.

This is a gap between the two deployment paths rather than a broken one: the
k8s manifests get it right via `fsGroup`, and the standalone Podman advice does
not carry the equivalent.

### Finding 3 — MCS category denials are silent

The `:Z` isolation case is the failure an operator is most likely to meet:
one container relabels a file with a private category, and every *other*
container that needs it is refused. Measured, twice, independently of the
suite:

```
$ podman run --rm -v $F:/mnt:ro,Z ... -c 'cat /mnt/f'   # first container
x
$ /usr/bin/stat -c '%C' $F/f
system_u:object_r:container_file_t:s0:c71,c694
$ podman run --rm -v $F:/mnt:ro ... -c 'cat /mnt/f'     # second container
cat: /mnt/f: Permission denied
$ sudo ausearch -ts <mark> | grep -E 'denied|SELINUX_ERR'
$ sudo ausearch -m AVC -ts <mark> | grep -c 'avc:  denied'
0
```

**No audit record of any kind** — no AVC, no `SELINUX_ERR`. A *type* denial on
the same host is audited (the control case, above), so this is specific to the
category check rather than a broken audit path.

That undermines the remediation line in #4337's own remediation table
([podman-selinux-release-qualification.md](podman-selinux-release-qualification.md)):

> | you cannot tell what was denied | `ausearch -m AVC -ts recent` … |

For the single most likely `:Z` failure, `ausearch` returns nothing, and an
operator following that advice concludes SELinux was not involved. The label
itself is the diagnostic: compare the `cN,cN` categories on the file against
the container that needs it.

The *mechanism* is inferred, not measured — MCS is enforced as a constraint
rather than an allow rule, and constraint denials are not audited the same way.
Confirming that would mean rebuilding the policy with `semodule -DB`, which
changes system state, so it was not done. See "What remains unproven".

### What the preflight got right

Under enforcing, on its first real exercise, it detected `Enforcing` and that
Podman labeling was enabled; changed no label and no mode on its own inputs;
offered no `setenforce`, no `label=disable`, no `--privileged`, and no widening
`chmod`. The `0440` secret came through both `:z` and `:Z` with its mode and
group ownership untouched — neither flag widens a secret.

Its exit `78` in the transcript is **not** an SELinux failure: it is
`✗ Hive config: .../src/hive.yaml does not exist`, because a checkout has no
`hive.yaml` until one is created. Nothing should read that exit code as a
qualification result.

## Environment

| | |
| --- | --- |
| SELinux | **Enforcing**, policy `targeted`, MLS enabled |
| Podman | 5.8.4, **rootless** (`SELinuxEnabled=true`) |
| OCI runtime | crun 1.28 |
| Kernel / host | `7.0.12-201.fc44.x86_64`, Bluefin 44.20260811 (Silverblue) |
| Policy packages | `container-selinux-2.250.0-1.fc44`, `selinux-policy-44.5-1.fc44` |
| Audit | `audit-4.2.1-1.fc44`, `auditd` active; log read via `sudo ausearch` |
| Image | `ghcr.io/hivecommons/hive:v4-latest`, digest `sha256:8f6b63a796fadbe05000e48445ac85b44f9b7c459fc81a82a5deef64abe05275` |
| Architecture | `amd64` only |
| coreutils | GNU 9.10 at `/usr/bin`, **uutils 0.9.0 ahead of it on `PATH`** |

`subuid`/`subgid` for the running user: `524288:65536`. Container GID 1002
therefore appears on the host as GID `525289`, which is why the fixture is built
through `podman unshare` rather than with a plain `chgrp`.

This is a Fedora **atomic** variant, not stock Fedora Server or CentOS Stream —
see "What remains unproven".

### Storage safety

`:z` and `:Z` **relabel the host directory they are pointed at, and the relabel
outlives the container.** Every mount source is therefore a fixture the probe
creates under `/var/tmp` and removes on exit. No checkout, no real secrets
directory, nothing under `/usr`, `/var/lib`, or `$HOME` is ever a mount target.

Every Podman call goes through `pod()`, pinned to a throwaway
`--root`/`--runroot`. The probe refuses to run if the store resolves to
`/var/lib/containers`, `/run/containers`, or the host's reported `graphRoot` —
the same guard as the #4200 probe. Containers are removed by exact name;
nothing is pruned. Files group-owned through the user namespace are removed with
`podman unshare rm -rf`, since a plain `rm` cannot touch a mapped subgid.

Nothing was installed and nothing was rebooted: `/usr` is read-only on this
host and `rpm-ostree` was used only to *query* package versions.

### What was deliberately not done

No `setenforce`. No `--security-opt label=disable`, no `--privileged`. No
`chcon` on anything outside the probe's own fixture. No `chmod` widening the
`0440` secret — the suite *asserts* the mode is unchanged rather than adjusting
it. Where a case failed, the fixture was corrected only when the fixture itself
was wrong (the first run's secrets directory was `0700`, which conflated DAC
with MAC); the enforcement was never relaxed to obtain a pass.

## Reproducing

```sh
# Stop condition: this reports nothing at all unless the host is enforcing.
getenforce                      # must print Enforcing
findmnt -t selinuxfs            # must be mounted

src/deploy/probe_podman_selinux_avc.sh
```

Exit `0` every case matched, `1` at least one did not, `78` not qualifiable
here. Options: `--image`, `--store`, `--keep`, `--skip-preflight`.

Reading the audit log needs root. The probe uses `sudo -n ausearch` when it
can, falls back to `journalctl`, and **fails rather than reporting a clean run**
when neither is available — a probe whose purpose is recording denials must not
report silence it could not observe.

### Doing it by hand

```sh
S=$(mktemp -d -p /var/tmp store-XXXX); mkdir -p $S/graph $S/run
POD="podman --root $S/graph --runroot $S/run"
IMG=$(bash src/deploy/standalone-images.sh get hive)   # the #4206 source of truth
F=$(mktemp -d -p /var/tmp fix-XXXX); printf 'x\n' > $F/f; chmod 600 $F/f

# 0. Baseline, BEFORE any container. Hosts of this class carry ambient denials.
sudo ausearch -m AVC -ts today | grep -c 'avc:  denied'

# 1. Control. Must be denied, and must produce an AVC.
$POD run --rm -v $F:/mnt:ro --entrypoint /bin/sh $IMG -c 'cat /mnt/f'
sudo ausearch -m AVC -ts recent | grep container_t

# 2. :Z grants, and takes a private category.
$POD run --rm -v $F:/mnt:ro,Z --entrypoint /bin/sh $IMG -c 'cat /mnt/f'
/usr/bin/stat -c '%C' $F/f          # NOT `stat` — see Finding 1

# 3. That category isolates — and the denial is SILENT.
$POD run --rm -v $F:/mnt:ro --entrypoint /bin/sh $IMG -c 'cat /mnt/f'
sudo ausearch -m AVC -ts recent | grep container_t   # expect nothing new

# 4. The hive-launch secret. Build it THROUGH the user namespace.
G=$F/secrets; mkdir -p $G; printf 'not a real key\n' > $G/github-app.pem
$POD unshare chgrp 1002 $G/github-app.pem; $POD unshare chmod 0440 $G/github-app.pem
$POD unshare chgrp 1002 $G
chmod 0700 $G && $POD run --rm -v $G:/mnt:ro,z --user 1001 \
  --entrypoint /bin/sh $IMG -c 'cat /mnt/github-app.pem'   # DENIED — DAC
chmod 0750 $G && $POD run --rm -v $G:/mnt:ro,z --user 1001 \
  --entrypoint /bin/sh $IMG -c 'cat /mnt/github-app.pem'   # granted

# Clean up through the namespace: mapped subgids resist plain rm.
$POD unshare rm -rf $F $S
```

## Results ledger

Recorded per release, not per PR — the reason is #4211's measurement, restated
in [the qualification document](podman-selinux-release-qualification.md).

| Release | Date | Result | Engine | AVC | Image digest |
| --- | --- | --- | --- | --- | --- |
| unreleased (`v4` @ `46fdc651`) | 2026-08-20 | PASS | podman 5.8.4 rootless=true | container AVCs 4→6, MCS audited=no | `sha256:8f6b63a796fa` |

The probe prints a ready-formatted row with the release cell left as a
placeholder; only the person cutting the release knows which one it is.

## What remains unproven

- **Rootful.** Rootless only. The ID mapping that drives Finding 2 does not
  exist rootful — the host user *is* root — so that finding is expected not to
  apply there, but that expectation is untested.
- **Distribution.** A Fedora 44 **atomic** variant (Bluefin/Silverblue), not
  stock Fedora Server and not CentOS Stream. The uutils-on-`PATH` condition
  behind Finding 1 is a property of this image family; a stock Fedora host with
  GNU coreutils alone will not hit it, and the preflight's label check will work
  there. How common the shadowing is across the target hosts is not measured.
- **Architecture.** `amd64` only. Nothing here was run on `arm64`.
- **The mechanism behind Finding 3.** That MCS constraint denials go unaudited
  is *observed*; the explanation offered for it is *inferred* from how MCS is
  expressed in policy. Confirming it would need `semodule -DB` or a reading of
  the policy source, neither of which was done — the former changes system
  state.
- **Whether Finding 3 holds for writes.** Only `open`/`read` was exercised. A
  denied write under a foreign category may or may not audit.
- **The rest of the preflight.** Only the SELinux, mount-label, secrets, and
  port checks that ran on this checkout were exercised, and `hive.yaml` was
  absent, so the configuration path was never measured with a real file present.
  #4207 and #4208's layers were not run at all.
- **No deployment was started.** The real entrypoint never ran; every container
  here was `/bin/sh -c cat`. Nothing in this document says the standalone stack
  works under enforcing — only that these mount and secret mechanics behave as
  recorded.
- **Finding 2's impact on the shipped standalone path.** The measurement uses a
  fixture. Whether the documented standalone deployment actually mounts
  `secrets` in a way that trips it was not traced through
  `src/docker-compose.yaml`.

## References

- [Release qualification: SELinux-enforcing Podman](podman-selinux-release-qualification.md) — #4337, the matrix this extends
- [Podman preflight: SELinux, mounts, secrets, and ports](podman-preflight-host.md) — #4209, the script Findings 1–3 concern
- [Podman CI runner map](podman-ci-runner-map.md) — #4211's measurement: no SELinux on any hosted runner, which is why this is a release lane
- [Rootful Podman egress-gate baseline](podman-rootful-egress-baseline.md) — #4200, the probe and document shape this follows
- `src/deploy/probe_podman_selinux_avc.sh` — the probe
- `src/Dockerfile` — where `HIVE_LAUNCH_GID=1002` and `dev` (UID 1001) are pinned
- `src/deploy/k8s/deployment.yaml` — `fsGroup: 1002`, the Kubernetes path that gets Finding 2 right

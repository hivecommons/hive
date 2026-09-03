# Release qualification: SELinux-enforcing Podman

The one Podman lane that cannot be CI. Run it on an enforcing host before a
release, and record the result in the ledger at the bottom of this page.

## Why this is not a hosted lane

[The runner map](podman-ci-runner-map.md) (#4211) measured it rather than
assuming it: **hosted GitHub runners have no SELinux at all** — no
`getenforce`, no `/sys/fs/selinux`, AppArmor instead — on `ubuntu-latest` and
`ubuntu-24.04-arm` alike. There is no way to make an Ubuntu hosted runner
enforce SELinux, because the kernel is what enforces and that kernel has no
SELinux compiled in.

So this cannot move to `ubuntu-latest`, and the failure mode if someone tries
is the quiet one: every check below would still pass, on a host where `:z` and
`:Z` are no-ops and nothing is ever denied. A green tick would mean the
opposite of what it appears to mean. The qualification script refuses to run
outside Enforcing for exactly this reason — see [Stop condition](#stop-condition).

One tempting third option is also not equivalent: running a Fedora *container*
on an Ubuntu runner does not give you SELinux enforcement, for the same reason
— the host kernel enforces, and it has none.

#4188 anticipated this case and allows "a documented, repeatable
release-qualification job" as the alternative. This is that job.

## Why it matters

#4209 shipped the SELinux, mount, secret, and port preflight
(`bin/hive-podman-preflight-host.sh`, documented in
[the operator guide](podman-preflight-host.md)). Because of the measurement
above, **none of it has ever run on a host where SELinux enforces anything.**
Its advice — `:z` for the checked-out config files, `:Z` for the secrets
directory — was reasoned from the policy, not observed.

The bind-mount labeling it diagnoses is also what breaks first on a
Fedora/RHEL-class host: a container process sees `EACCES` on a file that is
plainly mode `0644`, and the error names neither SELinux nor the mount.

## When to run it

**Per release, not per PR.** Append a row to the ledger for every release. If
the deployment's mount set, the secrets layout, or the container image's user
model changes, run it again for that release even if the previous row passed.

## What it covers

| Check | What a pass means |
| --- | --- |
| enforcement control | an unlabeled bind mount is **denied** — the kernel is enforcing on this path |
| `:Z` | grants access, relabels to `container_file_t` with a private MCS category |
| `:Z` isolation | a second container without that category is **denied** |
| `:z` | grants access, relabels to `container_file_t` at `s0`, no category |
| `:z` sharing | a second container can read it |
| secrets | neither flag widens a `0600` file's mode |
| preflight | #4209's preflight runs here, detects Enforcing, and changes nothing |

The enforcement control comes first and is not optional. Every "access was
granted" result is only meaningful if the kernel would otherwise have said no,
so a host that permits the unlabeled mount aborts the run rather than
continuing — on such a host the remaining checks prove nothing.

## Prerequisites

- A **Fedora, CentOS Stream, or RHEL-class host with SELinux Enforcing** and the
  `targeted` policy. Verify with `getenforce` — it must print `Enforcing`.
- `podman`, and `container-selinux` installed (it is a dependency of the
  distribution `podman` package).
- A checkout of this repository.
- Outbound access to `ghcr.io` for the image under test.

No root is required. The qualification runs rootless by default and records
which root mode it measured.

## Running it

```
bash src/deploy/qualify_podman_selinux.sh
```

Useful flags:

| Flag | Use |
| --- | --- |
| `--image REF` | qualify a specific release image (default: the hive reference in `src/deploy/standalone-images.sh`, the #4206 source of truth — `ghcr.io/hivecommons/hive:stable` today) |
| `--store DIR` | pull into a throwaway store instead of the host's own |
| `--fixture DIR` | parent directory for the throwaway fixture (default `$HOME`) |
| `--skip-preflight` | do not run `bin/hive-podman-preflight-host.sh` |

Exit codes: `0` every check passed, `1` at least one failed, `78` the host is
not qualifiable and the release must be recorded as **UNEXECUTED**.

The script prints a ready-formatted ledger row at the end. Paste it into the
table below and commit it.

### What it will and will not touch

It creates its own fixture directory, relabels **that**, and removes it on
exit. It is never pointed at a real checkout or a real secrets directory:
`:Z` relabels in place, so doing that to an operator's files is a change to
the host rather than a test of it. It runs no `setenforce`, changes no mode or
owner, and installs nothing.

## Doing it by hand

The script is a convenience; the qualification is these commands. Run them on
the enforcing host to reproduce the result without it.

The label reads below say `/usr/bin/stat` on purpose, not `stat`. On hosts
where uutils coreutils is first on `PATH` — the Bluefin/Universal Blue default
— `stat -c '%C'` prints `unsupported for this operating system` to stdout and
exits 0, which is not a label (#4490). If `/usr/bin/stat` is not GNU either,
use `getfattr -n security.selinux --only-values --absolute-names <path>`. The
script resolves a working reader the same way and exits `78` if none exists.

```sh
# 0. Establish the mode. If this is not "Enforcing", stop — see below.
getenforce

# 1. A fixture with a host label no container type can read, and a 0600 secret.
D="$(mktemp -d -p "$HOME" hive-selinux-qual-XXXXXX)"
printf 'qualification fixture' >"$D/secret.txt"
chmod 600 "$D/secret.txt"
/usr/bin/stat -c '%C %a' "$D/secret.txt"  # unconfined_u:object_r:user_home_t:s0 600

# The image the release ships — the same default the script reads out of
# src/deploy/standalone-images.sh (#4206).
IMG="$(bash src/deploy/standalone-images.sh get hive)"
read_it() { podman run --rm -v "$D:/mnt:ro${1:+,$1}" --entrypoint /bin/sh "$IMG" -c 'cat /mnt/secret.txt'; }

# 2. CONTROL: no relabel flag. MUST be denied.
read_it                              # cat: can't open '/mnt/secret.txt': Permission denied
/usr/bin/stat -c '%C' "$D/secret.txt"  # unchanged — a denied mount relabels nothing

# 3. :Z — private relabel. Reads, and takes an MCS category.
read_it Z                            # qualification fixture
/usr/bin/stat -c '%C %a' "$D/secret.txt"  # ...:container_file_t:s0:cNNN,cNNN 600

# 4. That category isolates: another container without it is denied.
read_it                              # Permission denied

# 5. :z — shared relabel. Reads, and carries no category.
read_it z                            # qualification fixture
/usr/bin/stat -c '%C %a' "$D/secret.txt"  # ...:container_file_t:s0 600

# 6. Shared means shared: another container reads it.
read_it                              # qualification fixture

# 7. #4209's preflight, on this enforcing host.
HIVE_DEPLOY_RUNTIME=podman bash bin/hive-podman-preflight-host.sh

rm -rf "$D"
```

Note steps 3 and 5 both end with mode `600`. That is the point of checking it:
relabeling grants access through the **label**, and must never be accompanied
by widening the file.

## Reading the result

The `:Z` / `:z` distinction is the operational finding, and it is what
#4209's preflight advises:

- **`:z` (shared)** for files several containers read, and for anything checked
  out of git — a config file, `nginx.conf`. `container_file_t:s0` with no
  category.
- **`:Z` (private)** for a directory belonging to exactly one container — the
  secrets directory. `container_file_t:s0:cNNN,cNNN`.

Using `:Z` on a shared or checked-out file is the mistake this qualification
makes visible: step 4 above is precisely what happens to every *other*
container that needs that file, and to git, which will hand you a file whose
label no longer matches the rest of the tree.

## Remediation, and what is never offered as one

The constraint #4209's preflight holds to applies here too:

- **Never disable or weaken SELinux.** No `setenforce 0`, no editing
  `/etc/selinux/config`, no `--security-opt label=disable`, no
  `--privileged`. Every remedy below leaves the kernel enforcing.
- **Never widen a secret.** Modes that are too open are reported with the
  command to *narrow* them. Nothing here `chmod`s a file wider, and the
  qualification asserts modes are unchanged rather than adjusting them.

| Symptom | Remedy |
| --- | --- |
| `EACCES` on a plainly readable file inside the container | add `:z` (shared) or `:Z` (private) to that bind mount |
| the label reverts, or you want it set once rather than per run | `semanage fcontext -a -t container_file_t '<path>(/.*)?'` then `restorecon -R '<path>'` |
| a second container is denied a file that worked for the first | it was mounted `:Z`; use `:z` for anything shared |
| `podman info` reports `SELinuxEnabled=false` on an enforcing kernel | install `container-selinux`; `:z`/`:Z` are no-ops without it |
| you cannot tell what was denied | `ausearch -m AVC -ts recent`, or `journalctl -t setroubleshoot` |

## Stop condition

If no enforcing host is available, **record the release as UNEXECUTED**. Do not
run the suite on a permissive or SELinux-less host and record a pass — a pass
from such a host is worse than no row at all, because it looks like coverage.

The script enforces this rather than trusting the operator: a missing
`/sys/fs/selinux`, a missing `getenforce`, or a mode other than `Enforcing`
exits `78` and reports no result. So does a host where no candidate label
reader (`stat -c %C`, `/usr/bin/stat -c %C`, `getfattr`) returns something
shaped like a context — "I could not measure this" must never read as "this
host failed", and it must never emit a ledger row (#4490).

## Results ledger

One row per release. Paste the block the script prints.

| Release | Date (UTC) | Host | Policy | Kernel | Podman | Result |
| --- | --- | --- | --- | --- | --- | --- |
| _pre-release, first execution_ | 2026-08-20 | Fedora 44 (Aurora/Kinoite 44.20260815.1, ostree) | targeted | 7.1.4-200.fc44 | 5.8.4, rootless | **PASS** |

The first row is the run that accompanied #4337, against
`ghcr.io/hivecommons/hive:v4-latest`. Every check passed, including the
enforcement control, and #4209's preflight detected Enforcing and changed
nothing. Two things about it are worth carrying forward rather than reading as
broader than they are:

- The host is a **Fedora 44 atomic variant (Aurora/Kinoite)**, not stock Fedora
  Server or CentOS Stream. The kernel, policy, and `container-selinux`
  (2.250.0-1.fc44) are Fedora 44's, so the labeling result is a Fedora 44
  result — but a CentOS Stream or RHEL row is still worth adding when one is
  available, since those ship an older policy.
- It was measured **rootless only**. Rootful under enforcing is not covered by
  this row; the [support matrix](podman-support-matrix.md) is where that cell's
  grade lives.

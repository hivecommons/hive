# Podman preflight: subordinate IDs, graphroot, and networking

`bin/hive-podman-preflight-ids.sh` diagnoses the three host conditions rootless
Podman needs that have nothing to do with the engine version and everything to
do with how the box was prepared. Part of
[#4188](https://github.com/hivecommons/hive/issues/4188); implements
[#4208](https://github.com/hivecommons/hive/issues/4208).

It is **read-only**. In particular it never writes to `/etc/subuid` or
`/etc/subgid`: delegating a subordinate range is a host-administration decision
with security consequences, made by someone who knows which ranges are already
spoken for — not by a preflight running as an unprivileged user. It starts no
container, moves no storage, installs nothing, and contacts no registry.

```bash
HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight-ids.sh
```

| Exit | Meaning |
| --- | --- |
| `0` | No failing check. Warnings may still be present. |
| `64` | `HIVE_DEPLOY_RUNTIME` names something other than `docker` or `podman`. |
| `78` | At least one failing check. |

With the Docker default selected — `HIVE_DEPLOY_RUNTIME` unset or set to
`docker` — the script reports that it was skipped and exits `0` without running
a single Podman command. Docker deployments are unaffected.

Related layers: the engine, connection, root mode, and cgroup checks are
[#4207](https://github.com/hivecommons/hive/issues/4207); SELinux, mounts,
secrets, and ports are
[#4209](https://github.com/hivecommons/hive/issues/4209) — see
[Podman preflight: SELinux, mounts, secrets, and ports](podman-preflight-host.md).

## What it checks

### 1. Subordinate UID/GID mappings

Rootless containers map container UIDs onto a range the host has delegated to
the user. With no range, `podman run` fails with "cannot find UID/GID for
user" — *after* it has already created the container. With a range too small,
the container starts and then breaks on the first file owned by a UID past the
end of it, which is much harder to recognise.

The tables are read directly rather than taken from `podman info`. The engine
reports the mappings it *resolved*, which on a host using a non-file source
(SSSD, a directory service) can be correct while the files are empty — and can
be stale relative to a file edited since the engine last started. The files are
what an administrator edits and what the remediation changes, so the files are
what gets reported.

| Finding | Level |
| --- | --- |
| A range of at least `HIVE_PODMAN_MIN_SUBID_COUNT` (default 65536) in both tables | pass |
| No entry for the user in either table | **fail** |
| An entry smaller than the threshold | warn |
| A table that cannot be read | warn |
| Rootful engine | pass — subordinate IDs are not in the picture |

Several ranges for one user add up, so a host that delegates two 32768-wide
ranges reads as 65536.

Remediation for a missing range, which an administrator runs:

```bash
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>
podman system migrate
```

Pick ranges that do not overlap another user's. Preflight will not choose for
you and will not edit the tables.

### 2. Graphroot filesystem

Container storage needs a local filesystem with the ownership and extended-
attribute semantics overlayfs relies on. On NFS it does not have them: layers
fail to extract, or extract and then behave incorrectly. Operators hit this
when the home directory is on NFS, because rootless storage defaults under
`$HOME`.

| Filesystem | Level |
| --- | --- |
| Local (`btrfs`, `ext4`, `xfs`, `overlay`, …) | pass |
| `nfs`, `nfs4`, `cifs`, `smb`, `fuse.sshfs`, `9p`, `ceph`, `glusterfs`, `afs`, `lustre`, `gpfs` | **fail** |
| `tmpfs`, `ramfs` | warn — works, but images are lost on reboot and consume RAM |
| Undeterminable | warn |

Remediation for an unsupported filesystem:

```bash
# rootless: ~/.config/containers/storage.conf     rootful: /etc/containers/storage.conf
[storage]
graphroot = "/var/lib/my-local-disk/containers/storage"
```

then reset the container store — see `podman-system-reset(1)` — which destroys
existing local containers and images.

### 3. Rootless networking

A rootless container reaches the network through a helper (`pasta` or
`slirp4netns`) driven by a backend (`netavark`, or the retired `cni`). The
engine names the helper it intends to use; whether that helper is installed is
a separate question, and the answer only shows up as a container with no
network.

| Finding | Level |
| --- | --- |
| Backend `netavark` | pass |
| Backend `cni` | warn — retired upstream in favour of netavark |
| Helper named by the engine and present on the host | pass |
| Helper named by the engine but **not installed** | **fail** |
| Engine reports no helper | warn |
| Rootful engine | pass — no helper process is involved |

## Environment

| Variable | Default | Purpose |
| --- | --- | --- |
| `HIVE_PODMAN_MIN_SUBID_COUNT` | `65536` | Smallest delegated range treated as sufficient. |
| `HIVE_PODMAN_SUBUID_FILE` | `/etc/subuid` | Table to read. Parameterised for the contract tests. |
| `HIVE_PODMAN_SUBGID_FILE` | `/etc/subgid` | Table to read. Parameterised for the contract tests. |

## Tests

`bin/test_hive_podman_preflight_ids.sh` (61 assertions) mocks every input — a stub `podman`
answering the `info` templates, a stub `stat` answering the filesystem type,
and subid tables the case writes — so the whole matrix runs on a host with no
NFS mount, no privileges, and no Podman, and never executes a container
command. `PATH` is the fake bin alone, which is what makes "slirp4netns is not
installed" a fact of the fixture rather than a fact of the machine running the
tests.

```bash
bash bin/test_hive_podman_preflight_ids.sh
```

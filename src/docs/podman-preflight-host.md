# Podman preflight: SELinux, mounts, secrets, and ports

`bin/hive-podman-preflight-host.sh` diagnoses the three host conditions that
break a standalone Podman deployment of Hive without ever mentioning Podman in
the error message. Part of [#4188](https://github.com/hivecommons/hive/issues/4188);
implements [#4209](https://github.com/hivecommons/hive/issues/4209).

It is **read-only**. It starts no container, relabels nothing, changes no mode
or owner, writes nothing to the host, and contacts no registry. Every finding
comes with a command for the operator to run, and preflight never runs it.

```bash
HIVE_DEPLOY_RUNTIME=podman bin/hive-podman-preflight-host.sh
```

| Exit | Meaning |
| --- | --- |
| `0` | No failing check. Warnings may still be present. |
| `64` | `HIVE_DEPLOY_RUNTIME` names something other than `docker` or `podman`. |
| `78` | At least one failing check. |

With the Docker default selected — that is, with `HIVE_DEPLOY_RUNTIME` unset or
set to `docker` — the script reports that it was skipped and exits `0` without
running a single Podman command. Docker deployments are unaffected.

Related layers: the engine, connection, root mode, and cgroup checks are
[#4207](https://github.com/hivecommons/hive/issues/4207); subordinate IDs,
graphroot, and networking are
[#4208](https://github.com/hivecommons/hive/issues/4208).

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `HIVE_DEPLOY_RUNTIME` | `docker` | Selects the engine. The checks run only for `podman`. |
| `HIVE_SRC_DIR` | `<repo>/src` | Directory holding `hive.yaml`, `deploy/`, and `secrets/`. |
| `HIVE_PODMAN_PREFLIGHT_PORTS` | `3001` | Published host ports to check, space- or comma-separated. |

Port 3001 is the authenticated gateway and is checked by default. Port 7681 —
the raw writable ttyd terminal — is deliberately *not* published by
`src/docker-compose.yaml` and is deliberately not checked here; if it ever
appears in this list, something has gone wrong upstream of preflight.

## 1. SELinux state and Podman labeling

Two facts are read independently and they can disagree:

* The kernel's mode (`getenforce`, or `/sys/fs/selinux/enforce`).
* Whether Podman itself will apply container labels
  (`podman info --format '{{.Host.Security.SELinuxEnabled}}'`).

| Kernel | Podman labeling | Result |
| --- | --- | --- |
| Enforcing | enabled | Pass. |
| Enforcing | **disabled** | **Fail.** Every bind mount is denied and `:z`/`:Z` do nothing, because Podman will not set a label the container type can read. |
| Enforcing | unreported | Warn — confirm labeling before deploying. |
| Permissive | either | Warn. Label problems are logged, not enforced, so the deployment will appear healthy here and fail on the first enforcing host. |
| Disabled / not built in | n/a | Pass. Mount labeling does not apply; this is the normal Debian/Ubuntu state. |

For the enforcing-with-labeling-off case the remedy is to install the container
policy and remove whatever turned labeling off:

```bash
sudo dnf install container-selinux
grep -rn 'label' /etc/containers/containers.conf /etc/containers/containers.conf.d/ 2>/dev/null
```

**Disabling SELinux is not offered as a fix anywhere in this check**, and
`setenforce 0`, `SELINUX=disabled`, and `--security-opt label=disable` never
appear in its output. A test asserts that across every case.

If you are on a permissive host and want to qualify the deployment properly,
switch to enforcing and re-run rather than shipping the permissive result:

```bash
sudo setenforce 1
sudo ausearch -m AVC -ts recent   # denials accumulated while permissive
```

## 2. Mount labeling

The three bind-mount sources in `src/docker-compose.yaml` are checked against
the label their host path actually carries:

| Source | Consumed by | Recommended option |
| --- | --- | --- |
| `hive.yaml` | `hive` | `:z` |
| `deploy/nginx.conf` | `gateway` | `:z` |
| `secrets/` | `hive` | `:Z` |

`container_file_t`, `container_share_t`, and `container_ro_file_t` are readable
by a container as-is and pass. Anything else — `user_home_t` for a checkout
under `$HOME` is the everyday case — warns, because a lifecycle asset that does
not carry `:z` or `:Z` will hit `EACCES` on a file that looks perfectly
readable on the host.

The read-only configuration files get `:z` rather than `:Z`: they live inside
the operator's checkout and are likely read by other tooling, and `:Z` stamps a
per-container MCS category that makes them unreadable to everything else. The
secrets directory gets `:Z`, because exactly one container should be able to
read it.

To label a path once instead of relabeling on every run, preflight prints the
persistent form:

```bash
sudo semanage fcontext -a -t container_file_t '/opt/hive/src/secrets(/.*)?'
sudo restorecon -R /opt/hive/src/secrets
```

A source that carries no label at all is reported separately: that usually
means the filesystem underneath it does not support security xattrs, and the
deployment needs to move rather than be relabeled.

### Reading the label at all

`stat -c '%C'` is not assumed to work. Under **uutils coreutils** — the default
on Fedora-atomic hosts such as Bluefin — `stat` has no `%C` and answers
`unsupported for this operating system` on *stdout*, with exit status `0`. A
`|| fallback` never fires, and that sentence used to flow into the report as if
it were a context, so no path could ever match a container type and every
source was warned about, including one the operator had already labelled
correctly (#4359).

Preflight now resolves a reader once per run — `stat`, then `/usr/bin/stat`,
then `getfattr` — and requires the output to look like a real context
(`user:role:type:level`) before trusting it. When none of them works, that is
reported as its own outcome:

```
△ Mount labeling: no tool on this host can read an SELinux label
  → Install GNU coreutils or attr (getfattr), then re-run. Mount labeling is
    UNCHECKED until then — this is not a pass.
```

"Cannot read a label" and "this path has no label" are deliberately different
messages. Only the second one means the filesystem is the problem.

## 3. Configuration and secrets readability

Readable by the process that needs it, and by nobody else. Both halves are
reported; **neither is repaired**.

* A missing `hive.yaml` or `deploy/nginx.conf` fails — the bind mount has
  nothing to bind.
* A missing `secrets/` warns rather than fails: token-only deployments never
  create it. The suggested command creates it **traversable by the container**,
  not merely narrow — see below.
* A source the invoking user cannot read fails. Rootless Podman opens bind
  mounts with that user's credentials, so an unreadable source is an unreadable
  mount.
* Under rootless Podman, a source owned by *another* host user warns: only the
  invoking user maps to container root, and everyone else lands as `nobody`
  inside the user namespace. That is a latent failure even when the file reads
  fine today. Rootful Podman does not map UIDs that way, so the check does not
  apply there.
* A secrets directory wider than `0750`, or a secret file wider than `0600`,
  warns and prints the `chmod` that **narrows** it.

### The secrets directory the container can actually traverse

`0700` looks like the safe choice and is not. The image reads GitHub App keys
as `dev` (UID 1001) through the `hive-launch` supplementary group (GID 1002,
pinned in `src/Dockerfile`). A `0700` directory owned by the operator grants
`dev` nothing — **not even the traverse bit** — so the key is unreadable
however it is labelled. The refusal is DAC and never reaches SELinux, so it
produces **no AVC at all**: it presents as `Permission denied` on a key file on
an enforcing host, reads as a labeling problem, and no relabeling fixes it
(#4359).

The Kubernetes path already gets this right with `fsGroup: 1002` in
`src/deploy/k8s/deployment.yaml`. Preflight now checks the standalone
equivalent — whether the directory is group-owned by the group container GID
1002 actually maps to, with the group-execute bit set — and prints the
remediation for the root mode in use.

Rootless, the mapping is the catch. The invoking user maps to container root
and everything above comes out of the subordinate range, so container GID 1002
is `subgid_start + 1001` on the host — **not** 1002. A plain
`chgrp 1002` therefore targets the wrong group, and is not even permitted for a
user who is not a member of it. `podman unshare` does the translation:

```bash
chmod 0750 /opt/hive/src/secrets
podman unshare chown -R 0:1002 /opt/hive/src/secrets
```

Rootful maps identity, so there it is simply:

```bash
sudo chgrp -R 1002 /opt/hive/src/secrets
sudo chmod 0750 /opt/hive/src/secrets
```

Key files stay `0640`. Nothing here grants anything to *other*, so the
narrowing-only rule is intact: `0750` is not a widening of `0700` toward the
world, it is the group bit the container needs and nothing more.

Preflight never widens permissions, and the test suite asserts both that no
output path proposes `chmod 777`, `chmod a+r`, or similar, and that the
deployment tree is byte-identical in mode, owner, and size after every run.

## 4. Host port availability

Each selected port is checked against the host's listening sockets via `ss`,
falling back to `netstat`. Neither installed means the port is reported as
unverified — never as free.

Rootless Podman additionally cannot publish a port below
`net.ipv4.ip_unprivileged_port_start`, which defaults to 1024. Preflight reads
that floor and fails a sub-floor port with the sysctl that raises the host's
allowance:

```bash
sudo sysctl -w net.ipv4.ip_unprivileged_port_start=443
```

The alternative it offers is to publish at or above the floor and put a
privileged listener in front. Running Hive as root is not offered. The floor
does not apply rootful, and the check is skipped there.

An occupied port fails and names the listener. Preflight does not stop it, does
not touch firewall rules, and does not reserve anything.

## Tests

```bash
bash bin/test_hive_podman_preflight_host.sh
```

Every input is mocked — a stub `podman`, `getenforce`, `sysctl`, and `ss`, plus
a `stat` that fakes SELinux labels and ownership per path while letting real
permissions through. The enforcing, permissive, and disabled matrix therefore
runs on a host with no SELinux, no Podman, and no privileges, which is what
`.github/workflows/v2-ci.yml` runs it on.

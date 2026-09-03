# Host-execution capability matrix: what this execution environment can actually do

[#4188](https://github.com/hivecommons/hive/issues/4188) requires "Boot persistence
... tested for the chosen Podman lifecycle", and when this page was written
[podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md) recorded both reboot rows
as **NOT executed**. Before anyone proposes a new execution runtime on the strength
of "the current execution paths cannot run this work", that claim deserves to be
measured rather than asserted. This page is the measurement.

Those rows have since been executed, on a host that could be rebooted. That is not
a contradiction of what follows, it is the remedy this page implies: what stopped
them running *here* is topology, and the answer to topology is a different host.

**It is one environment, measured once.** Every row below is a command that was run
here, with its real exit status and its verbatim output. Nothing is inferred from a
neighbouring row, and nothing is generalised to any other host — see
[What this does and does not establish](#what-this-does-and-does-not-establish).

The headline result was not the expected one. Most of these capabilities are
**present**, including the two that matter most for a disposable-guest proposal:
`/dev/kvm` is usable and passwordless `sudo` works. And the reason
[#4413](https://github.com/hivecommons/hive/issues/4413) could not run here turns
out not to be a permission gap at all.

## Which execution path this was

**The contributor relay in `local` (native host) mode.** Determined by direct
evidence rather than inference:

```
$ pgrep -af "contributor-relay|contributor-agent"
102327 node /var/home/dbaggett/hive-dev/hive/bin/contributor-relay.sh
  [exit 0]

$ readlink /proc/102327/cwd
/var/home/dbaggett/hive-dev/hive
  [exit 0]

$ tr '\0' '\n' < /proc/102327/environ | grep '^HIVE_AGENT_SESSION='
HIVE_AGENT_SESSION=hive-claude-d649
  [exit 0]

$ tmux display-message -p '#{session_name}'
hive-claude-d649
  [exit 0]
```

The relay's `HIVE_AGENT_SESSION` is **the tmux session this probe ran in**, so this
process is the agent CLI that relay launched — not a coincidental neighbour. The
relay is a sibling process rather than an ancestor because tmux daemonises; the
process chain is `bash → claude → bash → tmux: server → systemd`.

It is **not** container mode and **not** `agent_sandbox` rootless Podman:

```
$ ls -la /run/.containerenv /.dockerenv
"/run/.containerenv": No such file or directory (os error 2)
"/.dockerenv": No such file or directory (os error 2)
  [exit 2]

$ systemd-detect-virt --container
none
  [exit 1]

$ cat /proc/1/comm
systemd
  [exit 0]
```

Environment: Bluefin (Fedora Silverblue variant) 44.20260818, kernel
`7.1.4-200.fc44.x86_64`, systemd 259, podman 5.8.4, user `dbaggett` (uid 1000, in
`wheel`, `docker`, `libvirt`). `systemd-detect-virt` reports **`kvm`** — this host is
itself a virtual machine, which makes the `/dev/kvm` row below a nested-virtualisation
result.

## The matrix

| Capability | Available? | Evidence |
| --- | --- | --- |
| `/dev/kvm` present and usable | **yes** | opens read-write |
| systemd as PID 1 / `systemctl` usable | **yes** | user manager `running`; system manager `degraded` |
| `systemctl reboot` outcome | **permitted** — blocked by topology, not permission | polkit authorises it; `--dry-run` exits 0 |
| user lingering | **authorised, currently off** | `Linger=no`; polkit permits setting it |
| SELinux mode / whose policy | **enforcing, the HOST's policy** | `targeted`, context `unconfined_t` |
| rootful podman | **yes** | passwordless `sudo`; `Rootless=false` |
| kernel module load (`modprobe`) | **binary present; not loadable as this user** | `CapEff` is empty; `sudo` is available |
| `NET_ADMIN` | **not held; obtainable via user namespace** | `CapEff=0`, `CapBnd` full, `unshare -rn` works |

### `/dev/kvm` — present and usable

```
$ ls -l /dev/kvm
crw-rw-rw-+ 1 root kvm 10, 232 Aug 21 08:39 /dev/kvm
  [exit 0]

$ test -r /dev/kvm
  [exit 0]

$ test -w /dev/kvm
  [exit 0]

$ sh -c 'exec 3<>/dev/kvm && echo "opened /dev/kvm read-write ok" && exec 3>&-'
opened /dev/kvm read-write ok
  [exit 0]
```

The node is world read-write (`crw-rw-rw-`) and actually opens for read-write from
this process. Opening the device is the check that matters — the mode bits alone
would not have proved the kernel would hand it over.

### systemd — available, system manager degraded

```
$ cat /proc/1/comm
systemd
  [exit 0]

$ systemctl --user is-system-running
running
  [exit 0]

$ systemctl is-system-running
degraded
  [exit 1]
```

The **user** manager is healthy. The **system** manager answers `degraded` and exits
1 — some system unit on this host is in a failed state. That is host condition, not
a capability limit, and it is recorded because the exit status is non-zero and a
reader would otherwise wonder.

### `systemctl reboot` — permitted; the blocker is topology

```
$ pkcheck --action-id org.freedesktop.login1.reboot --process <pid>
  [exit 0]

$ systemctl --dry-run reboot
  [exit 0]
```

polkit authorises this user to reboot, and `systemctl --dry-run reboot` — which only
prints what it would do — exits 0. **No reboot was performed**; #4413 explicitly
reserves that.

This corrects the expectation #4441 itself carried. The reason #4413's rows could
not be executed here is **not** that reboot is forbidden. It is that this session runs
*on the host that would be rebooted*, exactly as
[podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md) put it while those rows
were still open:

> The host this was measured on could not be rebooted: it was the host running the
> session doing the measuring, and a reboot would have ended it mid-run.

That is a **topology** problem, not a permission one — and it is the single most
useful thing this page establishes, because the two have entirely different remedies.
Taking the topology remedy is how #4413's rows were eventually run: on a host that
was not the one running the session doing the measuring.

### User lingering — authorised, currently off

```
$ loginctl show-user dbaggett -p Linger
Linger=no
  [exit 0]

$ pkcheck --action-id org.freedesktop.login1.set-self-linger --process <pid>
  [exit 0]
```

Lingering is **off** right now, and polkit **would permit** this user to turn it on.

`loginctl enable-linger` was deliberately **not run**: it reconfigures the host, which
#4441 places out of scope. So this row establishes authorisation, not a completed
enable — and per the precedent [#4377](https://github.com/hivecommons/hive/issues/4377)
set, authorisation is *not* evidence that units would survive a boot. Only an actual
reboot can establish that. #4413 has since done so, on another host and with
`Linger=yes` — the enable this row deliberately did not perform.

### SELinux — enforcing, and it is the host's policy

```
$ sestatus
SELinux status:                 enabled
SELinuxfs mount:                /sys/fs/selinux
SELinux root directory:         /etc/selinux
Loaded policy name:             targeted
Current mode:                   enforcing
Mode from config file:          enforcing
Policy MLS status:              enabled
Policy deny_unknown status:     allowed
Memory protection checking:     actual (secure)
Max kernel policy version:      35
  [exit 0]

$ cat /proc/self/attr/current
unconfined_u:unconfined_r:unconfined_t:s0-s0:c0.c1023
  [exit 0]
```

Enforcing, `targeted`, and the process runs **unconfined**. Combined with
`systemd-detect-virt --container` returning `none`, this is the **host's** policy —
there is no container policy in play, so
[podman-selinux-avc-evidence.md](podman-selinux-avc-evidence.md)'s container-labelling
cases do not describe this environment.

**A trap worth recording.** The obvious probe gives the wrong answer here:

```
$ id -Z
id: --context (-Z) works only on an SELinux/SMACK-enabled kernel
  [exit 1]

$ command -v id
/home/linuxbrew/.linuxbrew/opt/uutils-coreutils/libexec/uubin/id

$ id --version
id (uutils coreutils) 0.9.0
```

`id -Z` reports SELinux as unavailable **on a host where it is enforcing**, because
`id` on this machine is **uutils coreutils**, which does not implement `-Z`. This is
the same class of trap `bin/hive-podman-preflight-host.sh` already documents for
uutils `stat -c %C`. Any probe that reaches for a GNU-coreutils-specific flag will
mis-read this host; `/proc/self/attr/current` and `sestatus` do not.

### Rootful podman — available

```
$ podman info --format '{{.Host.Security.Rootless}}'
true
  [exit 0]

$ sudo -n true
  [exit 0]

$ sudo -n podman info --format '{{.Host.Security.Rootless}}'
false
  [exit 0]
```

Rootless podman runs as this user, and **passwordless `sudo` is available**
(`sudo -n` exits 0), so rootful podman runs too and reports `Rootless=false`. The
"first live rootful start" that [#4377](https://github.com/hivecommons/hive/issues/4377)
wanted is therefore not blocked by privilege in this environment.

### Kernel module load — resolvable, not loadable as this user

```
$ command -v modprobe
/usr/bin/modprobe
  [exit 0]

$ modprobe -n dummy
  [exit 0]

$ sudo -n modprobe -n dummy
  [exit 0]
```

`modprobe` is present and `dummy` **resolves**. Note precisely what `-n` proves: it
is a dry run, so these exit codes say the module was found, **not** that loading is
permitted. Loading needs `CAP_SYS_MODULE`, and this process holds no capabilities at
all (see the next row) — so this user cannot load a module directly, while `sudo`
is available and root could. **No module was loaded**; doing so would change host
kernel state, which #4441 places out of scope.

### `NET_ADMIN` — not held; obtainable inside a user namespace

```
$ grep -E "^Cap(Eff|Bnd|Prm)" /proc/self/status
CapPrm:	0000000000000000
CapEff:	0000000000000000
CapBnd:	000001ffffffffff
  [exit 0]

$ capsh --decode=000001ffffffffff
0x000001ffffffffff=cap_chown,cap_dac_override,...,cap_net_admin,...,cap_checkpoint_restore
  [exit 0]

$ unshare -rn true
  [exit 0]
```

This process holds **no effective capabilities** (`CapEff=0`), so it does not have
`NET_ADMIN` directly. The **bounding** set is full and includes `cap_net_admin`, and
`unshare -rn` succeeds — user namespaces work — which is the mechanism by which
rootless podman obtains `NET_ADMIN` inside a container without the host process ever
holding it. That is what
[net-admin-requirement.md](net-admin-requirement.md) depends on.

The bounding set is **not** evidence that any particular container will get the
capability; it is the ceiling, not a grant.

## What this does and does not establish

**Establishes**, for this environment only:

- The path measured was the contributor relay in `local` mode, identified by the
  relay's own `HIVE_AGENT_SESSION` matching this probe's tmux session.
- `/dev/kvm` opens read-write, on a host that is itself a KVM guest — so nested
  virtualisation is available here.
- Reboot is authorised. #4413 was unrunnable here for a **topology** reason (the
  session lives on the host that would restart), not a permission one — and it
  ran elsewhere on that basis.
- Lingering is off but authorisable; rootful podman is reachable via passwordless
  `sudo`; SELinux is the host's, enforcing, unconfined; the process holds no
  capabilities but user namespaces work.

**Does not establish:**

- **Anything about a different execution path.** Container mode and
  `agent_sandbox` rootless Podman were not measured. A container's `/dev/kvm`,
  capability set and SELinux label are all different questions, and nothing here
  answers them.
- **Anything about a different host.** One environment is one data point. A CI
  runner, a hosted spoke, or another contributor's machine may differ in every row —
  `/dev/kvm` in particular is commonly absent on hosted runners.
- **That boot persistence works.** Lingering being *authorisable* is not evidence
  that units survive a boot. #4377 set that precedent explicitly and #4413 settled
  it on a host that could be rebooted; nothing measured here contributed to that
  result.
- **That anything should be built.** This is evidence for judging a proposal, not a
  proposal. A disposable-guest design should be argued against these numbers, not
  the other way round.

**Deliberately not attempted**, per #4441's boundary: no reboot, no
`loginctl enable-linger`, no module load, no package installed, and no host
reconfiguration of any kind. Where a row needed a mutation to prove more, it says so
rather than performing it.

## Related

- [#4188](https://github.com/hivecommons/hive/issues/4188) — the umbrella, and its boot-persistence requirement
- [#4413](https://github.com/hivecommons/hive/issues/4413) — the reboot rows this page explains but does not execute; now executed in both root modes
- [podman-quadlet-lifecycle.md](podman-quadlet-lifecycle.md) — where those rows are recorded, measured
- [podman-selinux-avc-evidence.md](podman-selinux-avc-evidence.md) — container SELinux behaviour, which is *not* what this environment shows
- [net-admin-requirement.md](net-admin-requirement.md) — why `NET_ADMIN` matters
- [podman-support-matrix.md](podman-support-matrix.md) — rootful/rootless × enforcing/advisory support levels

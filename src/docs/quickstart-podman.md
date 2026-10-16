Podman runs the same standalone Hive services through Quadlet and systemd; this page preserves the former top-level README Podman quick start in full.

## Quick Start (Podman)

Same two services as the Compose stack, run as systemd units through Quadlet, so
`systemctl start` returning means Hive answered `/api/health` rather than merely
that a process was spawned. Docker is not required and is not used.

**Prerequisites**

- **Podman 5.0.0+** (ADR-0017 recommends **5.6.0**; the verified floor is
  unknown — see [the requirements note](podman-standalone-quadlet.md#requirements))
- **systemd**, and **cgroup v2** — `podman info --format '{{.Host.CgroupsVersion}}'`
- The **Quadlet generator** at `/usr/libexec/podman/quadlet`. It ships with the
  distribution `podman` package; a hand-installed podman binary may not carry it.
- **`aardvark-dns`** — `podman info --format '{{.Host.NetworkBackend}}'` should
  say `netavark`. Without it the gateway starts and cannot resolve `hive`, so
  `:3001` serves 502s.
- `git`, `openssl`, and a GitHub token (PAT or App) for the org the hive works on

### One command

`bin/hive-podman-setup.sh` does everything in the manual sequence below —
preflights, configuration, the four Quadlet units, the boot wiring, and a final
check that the **gateway** answers on the published port before it returns.

```bash
git clone https://github.com/hivecommons/hive.git
cd hive

export HIVE_DEPLOY_RUNTIME=podman
bin/hive-podman-setup.sh --rootless        # or --rootful
```

It installs no packages and clones nothing, is idempotent, never overwrites an
existing config without `--force` and never touches `secrets/`. A failing step
stops the run and names itself; nothing is rolled back, so the partial state is
there to inspect. It also enforces three couplings that are easy to get wrong by
hand:

- `dashboard.port` is read out of the unit that will enforce it, and the run
  stops if the config does not read back agreeing — the 300-second silent hang
  the manual block warns about below.
- the volume is created **through its unit**, so it carries the ownership labels
  that make `bin/hive-podman-teardown.sh` able to see it.
- the secrets directory gets the right ownership for the root mode, and rootless
  installs are told when lingering is off and the deployment will not survive a
  reboot.

Add `--enable-linger` to fix that last one during the install rather than after.

### Or, by hand

Worth reading even if you use the script: the comments below are where the traps
are documented, and the script enforces the same ones.

The block below is **rootless**. For rootful, set `CONF=/etc/hive`, drop the
`podman unshare` line in favour of the `chgrp` beside it, install the units into
`/etc/containers/systemd/` with `sudo`, and drop `--user` from every `systemctl`.

```bash
# Selects the Podman path. WITHOUT THIS the preflights below exit 0 having
# checked nothing — they default to Docker and skip.
export HIVE_DEPLOY_RUNTIME=podman

git clone https://github.com/hivecommons/hive.git
cd hive

# Engine, root mode, cgroups; then subordinate IDs, graphroot, networking.
# A missing subuid range or cgroup v1 host fails HERE rather than as a start
# that times out five minutes later.
bin/hive-podman-preflight.sh
bin/hive-podman-preflight-ids.sh

CONF=~/.config/hive                       # rootful: CONF=/etc/hive
mkdir -p "$CONF/secrets" && chmod 750 "$CONF/secrets"
podman unshare chown -R 0:1002 "$CONF/secrets"    # rootful: chgrp -R 1002 "$CONF/secrets"

cp src/hive.yaml.example "$CONF/hive.yaml"
# REQUIRED. The example ships 3001 for local source runs; the unit's healthcheck
# probes 3002. Keeping 3001 costs a silent 300-second hang with no container
# left to inspect.
sed -i 's/^  port: 3001$/  port: 3002/' "$CONF/hive.yaml"
# then edit the rest of "$CONF/hive.yaml" for your project

cp src/deploy/nginx.conf "$CONF/nginx.conf"

# Must EXIST, even if every line stays commented out: EnvironmentFile= becomes
# `podman run --env-file`, which fails on a missing file.
cp src/deploy/quadlet/hive.env.example "$CONF/hive.env"
chmod 600 "$CONF/hive.env"
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> "$CONF/hive.env"
# Classic PAT: `repo` scope (`public_repo` for public-only), plus `workflow` at
# L5/L6 if agent PRs may touch `.github/workflows/`. See
# src/docs/github-app-setup.md#personal-access-token-pat-scopes
printf 'HIVE_GITHUB_TOKEN=%s\n'    'ghp_...'                 >> "$CONF/hive.env"

# Now the host preflight, which checks what the steps above just created:
# SELinux labels on the bind sources, secrets reachability, hive.env, port 3001.
HIVE_SRC_DIR="$CONF" bin/hive-podman-preflight-host.sh

# Pull before starting. The generated ExecStart pulls a missing image itself and
# that pull is spent inside TimeoutStartSec; the Hive image is ~3.8GB.
podman pull ghcr.io/hivecommons/hive:stable

# All four Quadlet units — the gateway will not generate without the network it
# names — plus the plain units that wire the stack to boot (#4478).
install -Dm644 src/deploy/quadlet/hive.container         ~/.config/containers/systemd/hive.container
install -Dm644 src/deploy/quadlet/hive-data.volume       ~/.config/containers/systemd/hive-data.volume
install -Dm644 src/deploy/quadlet/hive.network           ~/.config/containers/systemd/hive.network
install -Dm644 src/deploy/quadlet/hive-gateway.container ~/.config/containers/systemd/hive-gateway.container
install -Dm644 src/deploy/systemd/hive-boot.target       ~/.config/systemd/user/hive-boot.target
install -Dm644 src/deploy/systemd/hive-boot-gate.service ~/.config/systemd/user/hive-boot-gate.service
systemctl --user daemon-reload
systemctl --user enable hive-boot-gate.service

# Starting the gateway pulls Hive, the network and the volume up in order.
systemctl --user start hive-gateway.service
```

Dashboard at `http://localhost:3001`, the same port and the same single
published port as the Compose stack — Hive's own 3001/3002 and the raw ttyd
terminal on 7681 stay inside the container network. Confirm the stack end to
end, which also proves the gateway resolved `hive` over the shared network:

```bash
curl -sf http://127.0.0.1:3001/api/health     # -> {"status":"ok"}

# Post-install verification. Healthy NOW is not the same as back after a
# reboot: this is what catches rootless Linger=no, which nothing else reports.
bin/hive-podman-lifecycle-probe.sh check
```

`daemon-reload` runs the generator, and `[Install] WantedBy=hive-boot.target`
inside the units is half of what wires them to boot; the other half is
`hive-boot-gate.service` — the one real (enableable) unit, so the `enable`
above works and is required. **Rootless additionally needs
`loginctl enable-linger "$USER"`** or the user manager never starts at boot.
Check with `bin/hive-podman-lifecycle-probe.sh check`, not with
`systemctl is-enabled hive.service`, which reports `generated` either way.

The gate is why booting never waits on Hive: it starts `hive-boot.target` only
after systemd declares startup finished, so a Hive that cannot become healthy
costs itself its `TimeoutStartSec` — not the host's boot, in either root mode.
Before #4478 a rootful Hive sat inside the boot transaction and a broken one
held the boot for up to five minutes, on every boot, until fixed. Measured,
including the fix:
[Boot persistence](podman-standalone-quadlet.md#4-boot-persistence).

**Security posture — pick deliberately.** The shipped unit requests
`CAP_NET_ADMIN`, so the forced-proxy egress gate is *enforced* by default. Where
that capability is unavailable, `HIVE_PROXY_ADVISORY_OK=true` in `$CONF/hive.env`
starts Hive with the gate **not installed**; without either, Hive refuses to
start with exit 77 rather than running an unenforced capability model.

| | Enforcing (default) | Advisory (`HIVE_PROXY_ADVISORY_OK=true`) |
| --- | --- | --- |
| **Rootful** | **Supported** | Supported as a deliberate choice, **unenforced** |
| **Rootless** | **Supported** (needs `loginctl enable-linger` to survive reboot) | Supported as a deliberate choice, **unenforced** |

Advisory mode is **not** a weaker grade of enforcing and **not** a fallback:
agents can bypass the MITM proxy and the ACMM capability model is not enforced.
Choose it knowingly. Full matrix and the evidence behind each cell:
[src/docs/podman-support-matrix.md](podman-support-matrix.md).

To build from source instead of pulling the pre-built image, build and tag it
under the name the unit already names, then start as above:

```bash
podman build -t ghcr.io/hivecommons/hive:stable -f src/Dockerfile .
```

Full install detail — unit search paths, the traps behind each step above, boot
persistence, and what was measured in both root modes — is in
**[src/docs/podman-standalone-quadlet.md](podman-standalone-quadlet.md)**.
Update and rollback: [src/docs/podman-quadlet-update-rollback.md](podman-quadlet-update-rollback.md).
Teardown: `bin/hive-podman-teardown.sh`.

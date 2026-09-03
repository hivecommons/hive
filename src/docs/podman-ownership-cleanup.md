# Podman: Hive-owned resources and the safe cleanup contract

Rootless Podman and Buildah keep containers, pods, networks, volumes, images,
and build cache in **one store per user**. On a developer workstation that same
store usually holds unrelated development containers, Distroboxes, and local
builds. Docker's cleanup habits do not transfer: `docker system prune` on a
dedicated Docker host is inconvenient, while `podman system prune` on a
contributor's laptop deletes their other work.

This page defines what Hive owns and what Hive lifecycle tooling is allowed to
remove. It is the contract that the Podman lifecycle work in
[#4188](https://github.com/hivecommons/hive/issues/4188) must build on.

The contract lives in `bin/hive-podman-cleanup.sh` and is enforced by
`bin/test_hive_podman_cleanup.sh` in CI. `bin/hive-podman-teardown.sh` is the
teardown built on it (#4326) — see [Teardown](#teardown).

> Docker cleanup behavior is unchanged. `src/deploy/blue-green-deploy.sh` still
> prunes Docker images and build cache on the dedicated deployment host. This
> contract covers Podman and Buildah only, and the guard deliberately refuses to
> render a verdict on a `docker` command rather than quietly blessing it.

## Ownership labels

Every Hive-owned Podman resource carries the same four labels, applied at
create or build time:

| Label | Value | Purpose |
| --- | --- | --- |
| `io.kubestellar.hive.owned` | `true` | The single selector every cleanup command filters on. |
| `io.kubestellar.hive.component` | for example `hive`, `gateway` | Which Hive service the resource belongs to. |
| `io.kubestellar.hive.instance` | `HIVE_DEPLOY_INSTANCE`, default `default` | Which deployment on this host, so two Hive installs clean up separately. |
| `io.kubestellar.hive.runtime` | `podman` | The engine that created the resource. |

The keys are reverse-DNS so they cannot collide with labels other tools write
into the same shared store. They apply uniformly to **containers, pods,
networks, volumes, and images** — a caller only ever supplies the component
name:

```bash
mapfile -t labels < <(bin/hive-podman-cleanup.sh labels gateway)
podman run "${labels[@]}" ...
podman volume create "${labels[@]}" hive-data
podman build "${labels[@]}" -f src/Dockerfile ..
```

`HIVE_DEPLOY_INSTANCE` and the component name are validated against
`[A-Za-z0-9][A-Za-z0-9._-]*`, so neither can smuggle a second label or a shell
metacharacter into a filter.

## Selection

Cleanup selects on the ownership labels and nothing else:

```bash
mapfile -t filters < <(bin/hive-podman-cleanup.sh filters)
podman ps --all "${filters[@]}" --format '{{.ID}}'
```

`bin/hive-podman-cleanup.sh plan` prints the full set of scoped commands for
every resource kind. It only prints them — it never contacts an engine.

## What is rejected

`bin/hive-podman-cleanup.sh check -- <command>` exits `0` when a command is
scoped to Hive-owned resources and `65` when the ownership contract rejects it.
Hive lifecycle code must run every destructive Podman or Buildah command
through it first.

Rejected outright, because no filter can narrow them to Hive-owned resources:

- `podman system prune`, `podman system reset`, `podman machine reset` — the
  whole shared store.
- `podman builder prune` — build cache shared with unrelated builds, with no
  per-entry ownership label.

Rejected because they reach past what Hive owns:

- Any `--all`/`-a` form, including bundled short flags: `podman image prune -a`,
  `podman rmi -a -f`, `podman rmi -af`, `podman volume rm --all`,
  `buildah rm --all`, `buildah rmi --all`.
- Any `prune` without `--filter label=io.kubestellar.hive.owned=true`, including
  a prune filtered on some *other* label.
- Any `rm`/`rmi` that neither names explicit operands nor carries the ownership
  filter.

Global options are parsed before the subcommand, so
`podman --root /var/tmp/store system prune` and
`podman --connection remote image prune -a` are rejected too.

Allowed, because they are scoped:

```bash
podman rm --force hive hive-gateway
podman volume prune --force --filter label=io.kubestellar.hive.owned=true
podman rmi ghcr.io/kubestellar/hive:v4
buildah rm hive-build
```

## Testing

`bin/test_hive_podman_cleanup.sh` runs in CI and **deletes nothing**. The guard
is pure argument analysis, every engine binary on the test `PATH` is a tripwire
that records the call and fails the run, and the test asserts the tripwire log
stayed empty.

It also scans every tracked file for the broad commands above, so a future
change cannot reintroduce `podman system prune` or `buildah rm --all` into Hive
tooling without the test failing.

## Teardown

`bin/hive-podman-teardown.sh` removes one standalone Podman deployment. It is
the only supported way to tear a Hive deployment down under Podman, and it is
built on the contract above rather than beside it.

```bash
bin/hive-podman-teardown.sh plan             # print what would go; removes nothing
bin/hive-podman-teardown.sh run --yes        # remove it
bin/hive-podman-teardown.sh run --yes --images
```

What it does, in order: first it stops the Quadlet-generated services that own
the resources — `hive-gateway.service`, `hive.service`,
`hive-data-volume.service`, `hive-network.service`, the reverse of the order
setup starts them — and clears their failed state. Then it removes containers,
then pods, then volumes, then networks. The order is load-bearing (#4484):
`hive-network.service` is run-once and stays `active (exited)` after creating
the network, so removing the network underneath it leaves systemd believing
the network still exists and the next install fails with `network not found`;
and the containers carry `Restart=always`, so a container removed while its
unit runs is recreated 30 seconds later. The unit *files* and the
configuration under `~/.config/hive` are left installed — a later
`daemon-reload` + start, or `bin/hive-podman-setup.sh`, recreates the
deployment from them. Images are opt-in behind `--images`, because an image is
shared with every other deployment that pulled the same reference.
`HIVE_DEPLOY_INSTANCE` scopes the whole run to one deployment, so two Hive
installs on one host tear down separately; the Quadlet units belong to the
default instance and are only stopped when tearing that instance down.

Three properties are worth stating explicitly, because they are what the
teardown is for:

- **Selection is the ownership label and nothing else.** Every listing is built
  from `hive_podman_filters`. An unlabelled container, pod, volume, or network
  is not merely skipped — it is never returned, so there is no code path in
  which it could be removed.
- **Every command goes through the guard before it runs**, reads included. A
  command the guard rejects aborts the teardown with `70`; it is never adjusted
  and retried. That matters because the resource IDs come from the engine and
  are therefore input: if a listing ever returned something that would turn
  `podman rm --force <ids>` into a store-wide removal, the guard is what stops
  it, and `bin/test_hive_podman_teardown.sh` asserts exactly that case.
- **Removal names operands.** No `prune` and no `--all` appears anywhere in the
  path. The commands name the exact IDs the label filter returned.

`run` requires `--yes`, and the teardown refuses to run at all when
`HIVE_DEPLOY_RUNTIME` selects a runtime other than Podman. Docker deployments
are torn down through their own Compose path, which this script does not touch.

### What is not labelled yet

The teardown can only remove what the create side labelled, and the standalone
Podman deployment asset does not exist yet — it is its own slice of
[#4188](https://github.com/hivecommons/hive/issues/4188), and
[ADR-0017](adr/0017-podman-quadlet-lifecycle.md) records Quadlet as the
mechanism it will use. When it lands, every container, pod, network, and volume
it creates must carry the label set above, applied through
`hive-podman-cleanup.sh labels <component>`.

That is not left to reviewer memory. `bin/test_hive_podman_teardown.sh` scans
the tracked deployment assets for anything that creates a Podman resource and
fails if the file does not go through the labels seam. The scan passes
vacuously today and stops being vacuous the moment an asset lands. Spike probes
and tests are deliberately out of its scope: they run against a throwaway store
with a private graphroot and remove what they create by exact name.

## Scope

This document is the ownership contract, its guard, and the teardown built on
them. It does not create anything. The standalone Podman deployment assets —
the units that will carry these labels at create time — belong to the service
asset slice of #4188, which must use `hive-podman-cleanup.sh labels` rather
than inventing a second label scheme.

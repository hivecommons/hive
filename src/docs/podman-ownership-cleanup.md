# Podman: Hive-owned resources and the safe cleanup contract

Rootless Podman and Buildah keep containers, pods, networks, volumes, images,
and build cache in **one store per user**. On a developer workstation that same
store usually holds unrelated development containers, Distroboxes, and local
builds. Docker's cleanup habits do not transfer: `docker system prune` on a
dedicated Docker host is inconvenient, while `podman system prune` on a
contributor's laptop deletes their other work.

This page defines what Hive owns and what Hive lifecycle tooling is allowed to
remove. It is the contract that the Podman lifecycle work in
[#4188](https://github.com/kubestellar/hive/issues/4188) must build on.

The contract lives in `bin/hive-podman-cleanup.sh` and is enforced by
`bin/test_hive_podman_cleanup.sh` in CI.

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

## Scope

This is the ownership and cleanup contract only. Applying the labels to real
deployment assets, and implementing the teardown itself, belong to the later
Podman lifecycle slices of #4188 — which must use this module rather than
inventing a second label scheme.

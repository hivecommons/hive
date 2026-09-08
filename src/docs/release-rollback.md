# Digest-verifiable rollback

This is the operator runbook for pinning a hive back to a prior, immutable
build and **proving** that the pin landed. It covers all three published
images and every deployment shape Hive ships. It assembles material that
already lives in [release channels](release-channels.md) (how tags are
published, digest resolution, the retention window), [tagged
releases](releases.md) (which tags are immutable), and the Podman
[update and rollback](podman-quadlet-update-rollback.md) page (the pin
mechanics on a standalone host) into one procedure.

The rule the whole page rests on: **a rollback is verified by digest, never by
tag.** Every moving tag (`stable`, `candidate`, `edge`, `<branch>-latest`) can
be re-pointed at any time, and a container runtime that already holds a tag
does not necessarily re-pull it, so "the image string says `stable`" proves
nothing about what a pod is running. A `sha256:` manifest digest names exactly
one build, and a runtime that pulled by digest either runs that build or
fails loudly.

## What you can roll back to

Three kinds of image reference never move once written (see "What is
immutable vs. moving" in [releases.md](releases.md)):

| Reference | Written by | Notes |
|---|---|---|
| `ghcr.io/hivecommons/<image>:<7-hex-sha>` | `docker.yml`, every successful build of a release line | Retained for a bounded window: the scheduled GHCR prune deletes package versions whose only tags are short-SHA tags after **90 days** (`RETENTION_DAYS` in `.github/workflows/prune-ghcr*.yml`). Versions still carrying any moving tag are never pruned. |
| `ghcr.io/hivecommons/<image>:v<X.Y.Z>` | `tagged-release.yml`, only when a release is cut | A retag of the short-SHA digest, byte-identical to it, not subject to the 90-day prune. `v4` line only today; see the [v5 semver policy](releases.md#release-lines-and-the-v5-semver-policy). |
| `ghcr.io/hivecommons/<image>@sha256:<digest>` | You, in the deployment manifest | The digest form is what this runbook actually writes. A tag is how you *find* the digest; the digest is what you *pin*. |

The three images are **separate build jobs** tagged with the same short SHA,
so one can succeed while another fails for the same commit (this is why the
hub verifies `hive-hub:<sha>` rather than `hive:<sha>` before targeting its
own upgrade). Resolve all three before you change anything, and treat a
missing tag on any of them as "this commit is not a rollback target".

Which image runs where:

| Image | Runs as | Hosted (hub-managed) location | Self-managed location |
|---|---|---|---|
| `hive` | the spoke | `deployment/hive` in namespace `hive-hosted-<hive-id>` | `deployment/hive` from `src/deploy/k8s/deployment.yaml`, the `hive` service in `src/docker-compose.yaml`, or the `hive.container` Quadlet unit |
| `hive-hub` | the hub | `deployment/hive-hub`, container `hub`, namespace `hive-hub` | same manifest shape on a self-hosted hub |
| `hive-contributor` | a contributor relay | not hub-managed | `deployment/hive-contributor` in the namespace you chose for `just contribute-k8s`, or the container you run by hand ([contributor relay](contributor-relay.md)) |

## Step 1: resolve the target digests

Pick the short SHA of the commit you want to return to. The hub's **My Hives**
page shows each hive's current commit and, for channels, the resolved short
digest; the `docker.yml` run for a commit names the short-SHA tag it published;
`git log --oneline` on the release-line branch gives you the candidates.

Then resolve the **manifest-list** digest for each image from the registry,
not from a local image cache:

```bash
SHA=<7-hex-sha>
for img in hive hive-contributor hive-hub; do
  printf '%s ' "$img"
  docker buildx imagetools inspect "ghcr.io/hivecommons/$img:$SHA" \
    --format '{{.Manifest.Digest}}'
done
```

`skopeo inspect --format '{{.Digest}}' docker://ghcr.io/hivecommons/<img>:<sha>`
gives the same answer on hosts without buildx. Do **not** read
`podman image inspect ... RepoDigests` or `docker image inspect` for this:
those return the digest of the per-architecture manifest a local pull
happened to select, and a pin built from it resolves on one architecture and
fails on the other. `hive` and `hive-hub` are multi-arch. The
[Podman page](podman-quadlet-update-rollback.md#digest-not-tag-and-specifically-the-manifest-list-digest)
measured this.

Record the three digests alongside the SHA before you continue. The
`ghcr.io/kubestellar/*` mirrors carry the same manifest digest as the
`hivecommons` packages during the org transfer, so a digest resolved against
either registry verifies against either.

A `404 manifest unknown` on any of the three means the tag was never
published for that image, or has aged past the retention window. Choose a
different commit; do not fall back to a tag.

## Step 2: understand what would undo the pin, and what holds it

The hub has several paths that write images onto a spoke's Deployment on
their own, and one of them is specifically designed to fight drift:

- **Auto-upgrade** arms a rolling upgrade for hives riding a moving tag.
- **Channel re-arm** ([#3771](https://github.com/hivecommons/hive/pull/3771)):
  a hive with a persisted `tracked_channel` is re-armed back onto that
  channel on every heartbeat whose reported image tag differs from the
  channel. A digest pin has no tag, so a channel-tracking hive whose
  Deployment is patched **by hand** is dragged back to the channel on its
  next beat. This is the self-healing that makes channels durable, and it is
  exactly what a rollback needs stopped.

A pin taken through the hub (step 3) is a **run-state on the hive record**,
recorded with who, when, the digest and your reason, and every one of those
paths honours it: the channel re-arm skips a pinned hive, auto-upgrade never
arms it, any switch or upgrade armed before the pin is withheld from the
heartbeat, and the manual Upgrade, branch/channel switch and bulk
equivalents return `409` naming the pin. `tracked_channel` is left exactly
as it was, so lifting the pin (step 5) resumes the selection the hive had
before the incident. The pin survives hub restarts and hub self-upgrades.
Nothing lifts it except an explicit unpin by an owner.

The admin **upgrade pause** is therefore no longer a prerequisite for
rolling back one hive. It remains the right tool when the incident is
fleet-wide and you want *every* image change stopped while you decide what
to pin:

```text
POST /api/saas/upgrade-pause
{"target": "spokes", "paused": true}
```

and, if you are rolling back the hub itself, `{"target": "hub", "paused":
true}` as well. Pinning is allowed while the pause is on (a rollback is the
operator steering by hand while the train is stopped); unpinning is refused
with `409` until the pause is lifted, because it puts the hive back on the
train.

Self-managed deployments have their own movers: Watchtower in the Compose
file, `podman auto-update` on a Quadlet host, or a GitOps controller
reconciling the manifest. A digest pin is inert to Watchtower and to
`podman auto-update` (there is no tag to re-resolve), but a GitOps controller
will revert a `kubectl set image` on its next sync, so change the manifest at
its source of truth instead.

## Step 3: write the pin, by digest, for each image

Use the `@sha256:` form. Never write the short-SHA *tag* as the rollback
target: it is immutable on the registry, but it can be pruned after 90 days,
and a tag cannot be verified from inside the cluster without another registry
round-trip.

**Hosted spoke** (`hive`), through the hub. This is the sanctioned path: it
needs hub owner (or admin) rights and no cluster `kubectl` access, it writes
the same `deployment/hive` object with the same `*=` form the hub's upgrade
path uses, and it records the pin on the hive:

```text
POST /api/saas/hives/{id}/pin-digest
{"digest": "sha256:<hive-digest>", "reason": "rollback: 3f2a1c9 broke the proxy"}
```

You may give `{"sha": "<7-hex-sha>"}` instead of `digest`; the hub resolves
the short-SHA tag to its manifest-list digest on GHCR and records both. The
response carries the pin (`by`, `at`, `digest`, `source_sha`, `reason`) and
the exact image reference written. The same action is **Pin to digest** in
the row menu on **My Hives**.

The hub refuses, and records nothing, when the digest is malformed, when no
spoke image is published under it (pruned, or never built), when the hive is
on a cluster the hub cannot reach over `kubectl` (a digest cannot ride the
heartbeat fallback, which carries a bare tag), or when the `kubectl set
image` itself fails. A `200` therefore means the Deployment holds the
digest. Re-pinning the same digest is a no-op that keeps the original
provenance.

On a cluster the hub cannot reach, the direct patch remains available, from
a kube-context that reaches the spoke's cluster:

```bash
HIVE=<hive-id>
kubectl -n "hive-hosted-$HIVE" set image deployment/hive \
  "*=ghcr.io/hivecommons/hive@sha256:<hive-digest>"
kubectl -n "hive-hosted-$HIVE" rollout status deployment/hive
```

but the hub then has no record of it and its channel re-arm will undo it on
the next heartbeat, so pair it with the fleet-wide upgrade pause from step 2
and leave the pause on for as long as the rollback must hold.

**Hub** (`hive-hub`). The container is named `hub`, not `hive-hub`; a `set
image` that names the wrong container no-ops silently:

```bash
kubectl -n hive-hub set image deployment/hive-hub \
  "hub=ghcr.io/hivecommons/hive-hub@sha256:<hub-digest>"
kubectl -n hive-hub rollout status deployment/hive-hub
```

The hub's self-upgrade only ever targets the newest SHA for its branch, so a
hub rolled back by hand stays rolled back only while hub upgrades are paused
or hub auto-upgrade is off.

**Contributor relay** (`hive-contributor`), in the namespace you deployed it
to:

```bash
kubectl -n <namespace> set image deployment/hive-contributor \
  "*=ghcr.io/hivecommons/hive-contributor@sha256:<contributor-digest>"
kubectl -n <namespace> rollout status deployment/hive-contributor
```

Or regenerate the workload with the SHA pinned: the third argument to `just
contribute-k8s <namespace> <out.yaml> <sha>` pins the image tag; edit the
emitted `image:` line to the digest form before applying.

**Self-managed Kubernetes**: edit `image:` on the `hive` container in your
copy of `src/deploy/k8s/deployment.yaml` (or your kustomize overlay) to the
digest form and apply. The base manifest must keep naming
`ghcr.io/hivecommons/hive:stable` because
`src/deploy/test_standalone_image_refs.sh` asserts it; carry the pin in your
overlay.

**Docker Compose**: replace the tag with the digest on the `hive` service
and `docker compose up -d hive`, exactly as the [operator
reference](operator-reference.md#verify-and-pin-a-digest) shows:

```yaml
services:
  hive:
    image: ghcr.io/hivecommons/hive@sha256:<hive-digest>
```

**Podman Quadlet**: the scripted path is `bin/hive-podman-update.sh rollback`
(returns to the newest pin the script watched become healthy) or
`bin/hive-podman-update.sh pin ghcr.io/hivecommons/hive@sha256:<hive-digest>`.
Both write the drop-in `hive.container.d/10-image.conf`, pull before touching
the unit, and verify the running digest themselves. The unscripted procedure
is on the [Podman page](podman-quadlet-update-rollback.md#the-procedure-without-the-script).

The `/data` volume is carried across the image change unchanged in every
shape above. Before rolling back across a release whose notes announce a
`/data` schema migration, check the note for an explicit downgrade step;
[UPGRADE.md](../../UPGRADE.md) records that schema-changing rollback has not
been exercised.

## Step 4: verify the digest landed

This is the step that makes it a rollback rather than a hope. Two things have
to agree, and only the second is proof:

1. **What was asked for.** The Deployment's template image is the digest
   form. This is also what an in-cluster spoke reports to the hub as its
   `image_ref` on every heartbeat, and what the hub's drift check reads to
   decide the hive is pinned.

   ```bash
   kubectl -n "hive-hosted-$HIVE" get deployment/hive \
     -o jsonpath='{.spec.template.spec.containers[*].image}'
   ```

2. **What is actually running.** The pod's container status carries the
   digest the runtime resolved and pulled, independent of anything in
   `spec`:

   ```bash
   kubectl -n "hive-hosted-$HIVE" get pods -o \
     jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[*].imageID}{"\n"}{end}'
   ```

   The `imageID` must contain the digest you resolved in step 1. Some
   runtimes report the per-architecture manifest digest here rather than the
   manifest-list digest; `docker buildx imagetools inspect
   ghcr.io/hivecommons/<img>@sha256:<digest>` (without `--format`) lists the
   per-platform manifests inside the list, so match against those. A digest
   from any other build, or a pod still reporting the previous digest, means
   the rollout has not landed (an old ReplicaSet is still serving, which is
   how an unresolvable reference fails: the component looks up while
   running stale code).

Repeat both checks for `deployment/hive-hub` in `hive-hub` and for
`deployment/hive-contributor` in its namespace.

Then confirm from the hub's side, which sees the spoke only through its
heartbeat:

- `GET /api/saas/hives/{id}/digest-pin` returns the pin with its provenance,
  the `reported_image` from the last heartbeat, and `landed: true` once that
  reported image carries the pinned digest. Until then the **PINNED** pill on
  **My Hives** shows an hourglass; the pill's hover carries who pinned it,
  when, why, and the full digest.
- **My Hives** shows the hive's commit; it must be the short SHA you rolled
  back to. The spoke embeds its git hash at build time and reports it as
  `git_hash`, so this is a second, independent witness that the pod runs the
  build the digest names, not a pod that merely has the right `spec`.
- The version pill shows the SHA rather than a branch or channel, and the
  hive sorts under `Up to date` in the **Upgrade state** grouping (a pinned
  hive is never `Queued`).
- `git_branch` still reports the release line the image was built from; that
  is expected and is not evidence either way.

On a standalone host:

```bash
# Podman: the container's resolved digest
podman inspect --format '{{.ImageDigest}}' hive
# Docker: the repo digests attached to the image the container runs
docker image inspect "$(docker inspect --format '{{.Image}}' hive)" \
  --format '{{.RepoDigests}}'
```

Do not accept any of the following as verification: the image string in a
Compose file, a `stable (v4)` pill, `rollout status` returning success, or a
`--version` string. The binary's semver field is `0.0.0-dev` on every build
that was not cut as a release (see "The version constant" in
[releases.md](releases.md)); only the git hash and the digest identify a
build.

## Step 5: leave it pinned, or return to a moving tag

A digest-pinned hive is deliberately outside the upgrade train: the hub
reports it as pinned, never auto-upgrades it, and never counts it as behind.
That is the correct state for as long as the rollback needs to hold.

To rejoin a moving tag later, lift the pin:

```text
POST /api/saas/hives/{id}/unpin-digest
{"reason": "fix shipped in 7c1d2e0"}
```

or click the **PINNED** pill (owners) or **Unpin digest** in the row menu.
Unpin clears the run-state, records the lift on the timeline, and writes the
hive's tracked channel back onto the Deployment; a plain-branch hive returns
to the moving tag it was running when pinned. On a cluster the hub cannot
patch, the tag is armed for the spoke's next heartbeat instead (the response
says `"via": "heartbeat"`), and for a channel hive the durable re-arm, no
longer skipped, converges it too. Unpin is refused with `409` while the
admin spoke-upgrade pause is on; clear the pause first. The version pill and
`switch-branch` are unavailable while the pin holds, by design: a pin that a
routine switch could overwrite is not a pin.

Switching is considered complete when the heartbeat's reported tag matches
the target; since you are leaving a digest for a tag, that is the right check
for *that* operation. It is not a check you can use in the other direction,
which is why this page exists.

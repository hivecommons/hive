# Podman Quadlet `.kube` compatibility spike

## Decision

Direct reuse of the current standalone Kustomize overlay in one Quadlet
`.kube` unit is **not viable**.

Podman can reuse the core `Deployment`, `ConfigMap`, `Secret`, and
`PersistentVolumeClaim` documents after they are rendered. The adaptations
needed around them are no longer a small compatibility layer, though: the
rendered overlay also depends on Kubernetes-only discovery and access objects,
does not contain the standalone gateway, and loses security and readiness
semantics when played by Podman.

This is a compatibility result only. It does not change the existing
Kubernetes manifests. The sibling Quadlet work in #4202 should use explicit
`.container`/`.pod` units instead of treating this overlay as the Podman source
of truth.

## Probe

The representative path was
`src/deploy/kustomize/overlays/standalone`. It was rendered with Kustomize
5.7.1 and inspected with rootless Podman 5.8.4 on cgroup v2.

The rendered manifest contained two `Deployment` objects plus these kinds:

- `ConfigMap`, `Secret`, and `PersistentVolumeClaim`, which `podman kube play`
  supports.
- `Namespace`, `ServiceAccount`, `Role`, `RoleBinding`, `Service`, and
  `InferencePool`, which it does not support.

The probe used an isolated storage and runtime directory under `/tmp`, replaced
the three workload images with an empty local test image, and passed
`--start=false`. This exercised object parsing and container creation without
pulling production images or starting Hive. A temporary `.kube` file was then
run through the Quadlet generator and `systemd-analyze verify`.

The important commands were equivalent to:

```bash
go run sigs.k8s.io/kustomize/kustomize/v5@v5.7.1 build \
  src/deploy/kustomize/overlays/standalone > /tmp/hive-standalone.yaml

podman kube play --start=false --build=false /tmp/hive-standalone.yaml

QUADLET_UNIT_DIRS=/tmp/hive-quadlet \
  /usr/lib/systemd/system-generators/podman-system-generator \
  --user --dryrun
```

The first unmodified play stopped on the short-name image
`curlimages/curl:8.13.0`, after already creating the `hive-data` volume. With
local test images, play completed and created `hive-pod` and `vllm-pod`.
Podman 5.8.4 silently ignored every unsupported object listed above, so a zero
exit status did not mean that the Kubernetes topology survived.

The current Podman documentation lists the supported Kubernetes kinds and
`.kube` keys in
[`podman-kube-play(1)`](https://docs.podman.io/en/latest/markdown/podman-kube-play.1.html)
and
[`podman-kube.unit(5)`](https://docs.podman.io/en/latest/markdown/podman-kube.unit.5.html).

## Compatibility findings

### Ports and service discovery

The Kubernetes `Service` objects were ignored. `containerPort: 3002` did not
publish a host port, and names such as
`vllm-svc.hive-inference.svc.cluster.local` had no Podman DNS equivalent.

A temporary Quadlet `PublishPort=127.0.0.1:3002:3002` correctly generated a
`podman kube play --publish` argument. Because the YAML contains two
Deployments, however, Podman applied the same host binding to both pod infra
containers. The two pods would contend for the same host port when started.
Publishing only Hive would require a Podman-specific `hostPort` patch or a
separate YAML/Quadlet unit.

The Kubernetes path also has no equivalent of the Compose `gateway` service or
its `nginx.conf` mount. A Podman deployment would have to add that container,
publish only its authenticated port `3001`, and preserve the rule that raw
ttyd port `7681` is not published.

### Volumes, config, and secrets

The PVC became a Podman named volume and the ConfigMap and Secret became
read-only projected volumes. The PVC storage class, access mode, and requested
capacity have no local Podman enforcement; only the volume name survived.

Secret key references were populated when the test Secret contained a value,
and `defaultMode: 0440` was honored. The pod-level `fsGroup: 1002` was not:
inside the rootless user namespace the projected file was `root:root` mode
`0440`, and the container had no supplementary group. That breaks the
Kubernetes contract which makes Forge App keys readable by the `hive-launch`
group while excluding agent UIDs. A Podman path needs an ownership-aware
host/volume setup rather than reusing this Secret projection unchanged.

Production cleanup must not set Quadlet `KubeDownForce=true`, because that
maps to `podman kube down --force` and removes PVC-backed volumes. The force
option was used only against the isolated temporary store during this probe.

### User namespace and capabilities

The manifest's requested effective capability set survived inspection:
`CHOWN`, `DAC_OVERRIDE`, `NET_ADMIN`, `SETGID`, `SETPCAP`, and `SETUID`.

The Kubernetes YAML does not select a Podman user namespace. Quadlet
`UserNS=keep-id` generated `--userns keep-id` and produced a private user
namespace in the no-start probe, but this does not prove Hive's forced-egress
contract. Rootless startup, iptables redirect installation, privilege drop,
and bypass resistance remain the live security gate tracked by #4199.

### Health and readiness

Podman translated the startup probe and liveness probe into startup and regular
container health checks. The liveness failure action was `restart`.

The readiness probe did not appear in the created container configuration, and
the generated `.kube` service used `Type=notify` without waiting for Hive's
`/api/health` readiness result. A successful systemd start therefore cannot
stand in for Kubernetes readiness or for the Compose gateway's
`service_healthy` dependency.

## Smallest safe adaptation set

Making this topology work through `.kube` would require all of the following:

1. Render Podman-only YAML containing supported workload/config/volume kinds,
   split so per-pod port publication and lifecycle are unambiguous.
2. Replace Kubernetes Service DNS with explicit Podman network aliases and
   Podman-specific endpoint configuration.
3. Add and order the authenticated gateway workload, with only port `3001`
   published.
4. Replace the Secret projection with a mechanism that preserves the required
   `root:hive-launch` ownership and mode, and choose a tested user namespace.
5. Add a systemd readiness gate before dependent services are considered
   ready.

That is a second deployment definition with its own drift surface, not reuse of
the existing Kubernetes path. No `.kube` implementation follow-up is
recommended from this spike; #4202 can consume the result when choosing the
native Quadlet unit layout.

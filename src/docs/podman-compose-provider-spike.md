# Podman Compose-provider selection spike

## Recommendation

If standalone Hive uses `podman compose` at all, it must name the provider
explicitly and report which one ran. **`podman-compose` is the recommended
provider**, because it is the only one of the two that needs no Docker tooling
and no always-on API socket.

Hive should set both of these, not one:

```ini
# ~/.config/containers/containers.conf (rootless) or /etc/containers/containers.conf
[engine]
compose_providers = ["podman-compose"]
```

```bash
# and, for any Hive-invoked compose command, the explicit override
PODMAN_COMPOSE_PROVIDER=podman-compose
```

The environment variable takes precedence over `containers.conf` (measured
below), so `containers.conf` sets the host default and the variable makes each
Hive invocation deterministic regardless of what the host default became.

**Do not set `compose_warning_logs = false`.** That banner is currently the
only thing that tells an operator which provider executed. Hive should report
the provider itself — the resolved path plus its `version` output — rather than
silencing the one signal that exists.

This is an evaluation only. It adds no deployment asset and changes no runtime.
Docker Engine and Docker Compose behavior are untouched.

The wider recommendation is unchanged by this spike: use Quadlet for persistent
installations, per the `.container`/`.pod` feasibility result in #4202. Compose
remains reasonable for a development loop, and this document is what makes that
loop deterministic.

## The hazard, reproduced

The premise of #4201 is real and reproduces on a stock Fedora 44 host with both
providers installed:

```
$ podman compose version
>>>> Executing external compose provider "/usr/libexec/docker/cli-plugins/docker-compose" <<<<
Docker Compose version v5.4.0
```

`podman-compose` 1.6.0 was installed and on `PATH`. Podman still chose the
Docker Compose plugin. Nothing warned that a Docker-built provider had been
selected on a Podman deployment; the banner names the path, but only if the
operator reads it.

Every functional probe below named its provider explicitly. Auto-selection was
exercised **only** to document this hazard.

## Selection surfaces, measured

| Invocation | Provider that ran |
| --- | --- |
| `podman compose version` | `/usr/libexec/docker/cli-plugins/docker-compose` (v5.4.0) |
| `PODMAN_COMPOSE_PROVIDER=podman-compose podman compose version` | `podman-compose` 1.6.0 |
| `CONTAINERS_CONF=<conf with compose_providers=["podman-compose"]> podman compose version` | `podman-compose` 1.6.0 |
| both set, disagreeing (conf → `podman-compose`, env → docker plugin) | the **docker plugin** — the environment variable wins |

`compose_warning_logs = false` in the same `[engine]` table suppressed the
provider banner entirely, which is why the recommendation above declines it.

The configuration surface is therefore exactly:

- `PODMAN_COMPOSE_PROVIDER` — environment, highest precedence, accepts a bare
  name resolved on `PATH` or an absolute path.
- `containers.conf` `[engine] compose_providers` — a list, first match wins.
- `containers.conf` `[engine] compose_warning_logs` — banner only, no effect on
  selection.

## What Podman hands the provider

Substituting a provider that prints its own environment shows precisely what
`podman compose` sets before exec:

```
PROVIDER-ARGS: -f <file> version
DOCKER_BUILDKIT=0
DOCKER_CONFIG=
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
```

`podman compose` is a thin exec wrapper that points the provider at the Podman
API socket through `DOCKER_HOST`. That single line decides the Docker-freeness
question for each provider.

## Docker-free assessment

| | `podman-compose` 1.6.0 | Docker Compose plugin v5.4.0 |
| --- | --- | --- |
| Needs the `docker` binary | No | It **is** Docker tooling (`docker-compose-plugin` package) |
| Needs Docker Engine or `/var/run/docker.sock` | No | No — it is pointed at the Podman socket |
| Needs `podman-docker` or a `docker` alias | No | No |
| Needs a running Podman API socket | **No** — drives the `podman` CLI | **Yes** |

Measured on a host where Docker Engine happened to be running, so the test
removed Docker rather than trusting its absence. With `docker` absent from
`PATH` and `DOCKER_HOST` pointed at a path that does not exist:

```
$ env PATH=<no docker> DOCKER_HOST=unix:///nonexistent/docker.sock \
    podman-compose -f docker-compose.yaml ps
CONTAINER ID  IMAGE  COMMAND  CREATED  STATUS  PORTS  NAMES
rc=0
```

The same conditions against the Docker Compose plugin:

```
$ env DOCKER_HOST=unix:///nonexistent/podman.sock docker-compose ps
failed to connect to the docker API at unix:///nonexistent/podman.sock;
check if the path is correct and if the daemon is running
```

So the Docker Compose plugin can drive Podman, but only with `podman.socket`
(or a `podman system service`) enabled. That is an additional always-on local
API surface, and it is Docker-packaged software on a deployment whose point is
not needing Docker. It satisfies neither the letter nor the intent of #4188's
"at least one supported Podman installation path must require no Docker
Engine, Docker daemon, Docker socket, Docker CLI compatibility shim, or Docker
Compose provider."

`podman-compose` needs no socket at all, which is also one fewer thing for
#4208's preflight to check.

## Compose field support

A minimal Hive-compatible fragment — mirroring the field set of
`src/docker-compose.yaml` against an empty locally imported image — was created
through **both** explicitly selected providers with `up --no-start --no-build`,
in an isolated Podman store (`CONTAINERS_STORAGE_CONF` with a private graphroot
and runroot, plus a private `podman system service` socket for the Docker
Compose plugin). No production image was pulled and the host's own container
store was not touched.

Both providers translated every field correctly. Each row below was
inspected on a container created by `podman-compose`; capabilities,
healthcheck, ports, expose, restart, `security_opt`, and profile handling
were additionally inspected under the Docker Compose plugin and matched.


| Field, as used by `src/docker-compose.yaml` | Result under both providers |
| --- | --- |
| `cap_add: [NET_ADMIN]` | `CAP_NET_ADMIN` present in the container's effective caps |
| `healthcheck` incl. `start_period: 120s` | exec-form test, `StartPeriod` 2m0s |
| `depends_on: {condition: service_healthy}` | accepted; podman-compose 1.6.0 implements `service_healthy`, `service_started`, and `service_completed_successfully` |
| `ports: "3001:3001"` | published host binding created |
| `expose: [3001, 7681]` | exposed, not published — the gateway rule survives |
| named volumes | created with the project prefix |
| bind mount with `:ro` | preserved — the mount was created read-only |
| `labels` | preserved, alongside each provider's own `com.docker.compose.*` bookkeeping labels |
| `restart: unless-stopped` | preserved |
| `security_opt: no-new-privileges:true` | applied — `no-new-privileges` present under both providers |
| `profiles: [auto-update]` | honored — the profile service was not created by either provider |
| `networks: {internal: true}` | network created with `Internal=true` |

**No unsupported Compose field was found in `src/docker-compose.yaml`** at the
parse-and-create layer. `podman-compose -f src/docker-compose.yaml config`
returned cleanly with no warnings against the real file.

One divergence was checked and is a non-issue for Hive: podman-compose collapses
a single-element `test: ["CMD", "x"]` to `CMD-SHELL`, while the Docker plugin
keeps exec form. Hive's healthchecks are multi-element, and for those both
providers produced byte-identical exec-form tests.

A real difference worth knowing: on unset interpolation variables the Docker
plugin warns per variable, while podman-compose defaults silently. Anything
that depends on noticing a missing `HIVE_GITHUB_TOKEN` cannot rely on the
provider to say so.

## Versions tested

| Component | Version |
| --- | --- |
| Podman | 5.8.4, rootless |
| `podman-compose` | 1.6.0 (Python, Fedora 44 package) |
| Docker Compose plugin | v5.4.0 (`docker-compose-plugin-5.4.0-1.fc44.x86_64`) |
| Docker CLI present on the host | `docker-ce-cli-29.7.2` — removed from `PATH` for the Docker-free probes |
| Host | Fedora 44, cgroup v2 |

These are tested versions, not established minimums. Establishing a supported
floor needs a second host at older versions and is listed as a follow-up.

## Gaps — follow-up candidates, not implemented here

1. **Minimum supported versions.** Only one Podman/provider combination was
   tested. A supported floor for Podman and `podman-compose`, plus a preflight
   check that reports both, belongs with the #4207/#4208 preflight work.
2. **Provider reporting.** Hive should print the resolved provider path and its
   `version` output as part of its own startup reporting, rather than depending
   on the Podman banner that `compose_warning_logs` can switch off.
3. **`build:` parity.** The local-build workflow was not exercised; only
   `--no-build`. `DOCKER_BUILDKIT=0` is forced by `podman compose`, so the
   build path needs its own check.
4. **Live ordering.** `depends_on: service_healthy` was verified as implemented
   and accepted, not as observed ordering during a real `up`. That needs a live
   startup test.
5. **Rootless port publication.** That 3001 publishes and 7681 does not was
   confirmed at the container-configuration layer, not against a live rootless
   listener.
6. **The auto-update profile stays Docker-only.** It is built on the Docker
   socket and a filtered Docker API proxy, and #4188 already rules out
   repointing it at the Podman socket. The Podman equivalent is Quadlet
   auto-update, evaluated in #4202 — not a Compose provider question.

## References

- [`podman-compose(1)`](https://docs.podman.io/en/latest/markdown/podman-compose.1.html)
- [`containers.conf(5)`](https://github.com/containers/common/blob/main/docs/containers.conf.5.md)
- Sibling spike for #4202: the Quadlet `.container`/`.pod` feasibility result, which recommends Quadlet for persistent installations.

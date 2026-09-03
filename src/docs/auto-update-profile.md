# The `auto-update` Compose profile

The `auto-update` profile runs [Watchtower](https://github.com/containrrr/watchtower)
alongside the hive so that containers labelled
`com.centurylinklabs.watchtower.enable=true` are replaced automatically when a
new image is published.

It is **opt-in** and off by default:

```bash
docker compose --profile auto-update up -d
```

> **Running Podman?** This page does not apply to you, and the feature is not missing. The profile is built on the Docker socket and a filtered Docker API proxy, and [#4188](https://github.com/hivecommons/hive/issues/4188) keeps it deliberately Docker-only. The Podman equivalent is opt-in health-aware auto-update through `podman auto-update` — see [podman-auto-update.md](podman-auto-update.md).

**Read this whole page before enabling it in production.** The profile trades a
real, permanent increase in blast radius for the convenience of unattended
updates, and the trade is worse on a hive host than on most hosts, because this
host holds the GitHub App private key and every agent credential.

## Why unattended updates need the Docker daemon

Replacing a running container is a daemon operation: pull the new image, stop
the old container, create and start a new one with the same configuration.
There is no way to do that without an API that can create and start containers,
and an API that can create and start containers can create a *privileged*
container that mounts the host filesystem. That is the irreducible core of the
risk, and no proxy in front of it changes that fact.

## What changed in [#3865](https://github.com/hivecommons/hive/issues/3865)

Watchtower used to bind-mount `/var/run/docker.sock` directly into its own
container. That is the **entire** daemon API, including:

- `POST /containers/{id}/exec` — run a command inside any container. Against the
  hive container that reads `/secrets` directly. No escalation required, no
  privileged container needed, nothing to detect at the host level.
- `/volumes`, `/networks`, `/secrets`, `/configs`, `/swarm`, `/build`, `/info`.

Now the socket is held by a single `docker-socket-proxy` service that speaks a
deny-by-default subset of the API over an internal-only network, and Watchtower
reaches the daemon only through it (`DOCKER_HOST=tcp://docker-socket-proxy:2375`).

| | Before | After |
| --- | --- | --- |
| Raw socket in Watchtower's filesystem | yes | no |
| `/exec` into the hive container | **allowed** | **blocked** |
| `/volumes`, `/networks`, `/secrets`, `/configs`, `/swarm` | allowed | blocked |
| `/build`, `/commit`, `/info`, `/system` | allowed | blocked |
| Docker API reachable from the `hive` container | on the shared network | **no** — proxy is on an `internal:` network Watchtower alone joins |
| Docker API published to the host | no | no |

## What the proxy does NOT fix

This section is the point of the page. The proxy is defence in depth, not a fix,
and a deployment that treats it as a fix is worse off than one that understands
the residual — because it will enable the profile believing the finding is
closed.

Watchtower cannot do its job without `CONTAINERS=1` and `POST=1`. With those two
flags, a compromised Watchtower can still:

1. **Create a privileged container that mounts the host root.**
   `POST /containers/create` is permitted, and the proxy filters *paths and
   methods*, not request bodies — it does not and cannot inspect
   `HostConfig.Privileged` or `HostConfig.Binds`. This is host root.
2. **Copy files out of any container.** `GET /containers/{id}/archive` falls
   under `CONTAINERS`, and reads are permitted for enabled sections. That is the
   hive container's `/secrets` — the App private key — without needing step 1.

So the honest summary is: the proxy removes the *easiest* path (`/exec`) and a
broad set of unrelated API surface, and it stops the Docker API from being
reachable by the semi-trusted agent code running in the hive container. It does
not make a compromised Watchtower harmless. **If Watchtower is compromised, treat
the host and every credential on it as compromised.**

### A trap: `:ro` on the socket is not a mitigation

Mounting `/var/run/docker.sock:ro` looks like a fix and is not one. The
read-only flag applies to the *inode*; the Docker API is a protocol spoken over
the socket, and writes over that connection are unaffected. A container with a
read-only socket mount has exactly the same daemon access as one without.

The `:ro` on the proxy's mount in `docker-compose.yaml` is there for hygiene and
is explicitly *not* the control. The control is the environment allowlist.

## Recommended posture

In rough order of preference:

1. **Do not enable `auto-update` on a hive host that holds production
   credentials.** Update deliberately: `docker compose pull && docker compose up -d`
   on your own schedule. This is the only option that removes the risk rather
   than narrowing it.
2. **On Kubernetes, do not use this profile at all.** Use a rolling update of the
   Deployment (`kubectl set image` / a GitOps controller reconciling the image
   digest). Kubernetes already owns container lifecycle there, so nothing needs
   the Docker socket, and the whole class of finding disappears.
3. **If you enable it anyway**, then at minimum:
   - keep both images pinned by digest (`src/deploy/test_supply_chain_pins.sh`
     enforces this in CI);
   - keep the proxy on the internal network and never publish `2375`;
   - do not add `EXEC=1` — see the deny list in `docker-compose.yaml`;
   - treat a Watchtower CVE as a host-compromise event, not a container one.

## Private registries

`AUTH=0` is set, so the proxy does not expose `POST /auth`. Pulling from a
private registry needs the daemon's own credentials (`~/.docker/config.json` on
the host) rather than Watchtower-supplied ones. If you must pass registry
credentials to Watchtower, review whether the profile is the right mechanism at
all before enabling `AUTH`.

## Verifying the deployed posture

```bash
# Watchtower must NOT have the socket:
docker inspect hive-watchtower --format '{{json .HostConfig.Binds}}'    # → null

# The proxy must not be published to the host:
docker inspect hive-docker-socket-proxy --format '{{json .HostConfig.PortBindings}}'   # → {}

# The hive container must not be able to reach the Docker API:
docker exec hive-hive sh -c 'wget -qO- http://docker-socket-proxy:2375/version'  # → fails to resolve
```

`src/deploy/test_watchtower_socket_contract.sh` asserts the same invariants
against `docker-compose.yaml` in CI, so a future edit cannot quietly restore the
raw socket mount, publish the proxy, widen the allowlist, or put the proxy on a
network the hive container can reach.

Docker Compose is the default standalone runtime for a local Hive spoke; this page preserves the root README quick start in full.

## Quick Start

Two supported standalone runtimes. **Docker Compose is the default** and is what
the rest of this README assumes; **Podman** is a parallel supported choice, not
an experiment and not a recommendation over Docker. Pick one — they install the
same two services (Hive plus its authenticating gateway) and land the dashboard
on the same port.

| | [Docker Compose](#quick-start-docker-compose) | [Podman](quickstart-podman.md) |
| --- | --- | --- |
| Lifecycle | `docker compose up -d` | Quadlet units under systemd |
| Runs as | the Docker daemon | rootful **or** rootless |
| Update path | pull and recreate; optional Watchtower profile | [pinned by digest, with rollback](podman-quadlet-update-rollback.md) |

## Quick Start (Docker Compose)

**Prerequisites**

- Docker Engine 24+ with the Compose v2 plugin (`docker compose`, not the legacy `docker-compose`)
- A Linux, macOS, or Windows (WSL2) host on `amd64` or `arm64` — the pre-built images are multi-arch
- `git`, `openssl`, and a GitHub token (PAT or App) for the org you want the hive to work on

```bash
git clone https://github.com/hivecommons/hive.git
cd hive

cp src/hive.yaml.example src/hive.yaml
# REQUIRED: set project.org and project.repos (optionally primary_repo and
# ai_author) in src/hive.yaml — the placeholders do not work as shipped. See
# src/docs/operator-reference.md#minimum-required-configuration
# REQUIRED: the example ships dashboard.port 3001 for local source runs; the
# container healthcheck probes 3002, so keeping 3001 never becomes healthy.
sed -i 's/^  port: 3001$/  port: 3002/' src/hive.yaml

# src/.env, NOT ./.env. `-f src/docker-compose.yaml` makes `src/` the project
# directory, and that is where Compose reads `.env` from — the same place the
# compose file's own `./hive.yaml` and `./secrets` mounts resolve against. A
# `.env` at the repo root is read by nothing, and since both paths are
# gitignored, neither git nor Compose says so: the hive starts and then 401s on
# every GitHub call, which reads like a bad token rather than an unread file.
install -m 600 /dev/null src/.env   # holds live tokens — create it 0600, never world-readable
echo "HIVE_GITHUB_TOKEN=ghp_..." >> src/.env   # classic PAT: repo scope (see src/docs/github-app-setup.md#personal-access-token-pat-scopes)

# REQUIRED. The dashboard's auth proxy enforces this token and refuses to start
# without one, so the gateway on :3001 would proxy to a port nothing is
# listening on. See src/deploy/quadlet/hive.env.example, which is the contract
# for both runtimes.
printf 'HIVE_DASHBOARD_TOKEN=%s\n' "$(openssl rand -hex 32)" >> src/.env

docker compose -f src/docker-compose.yaml up -d
```

Dashboard at `http://localhost:3001`. Confirm it end to end rather than assuming
the port answers — the gateway publishes 3001 whether or not the proxy behind it
came up:

```bash
curl -sf http://127.0.0.1:3001/api/health     # -> {"status":"ok"}
```

The pre-built image tag is documented in [src/docs/operator-reference.md#image-provenance-and-tags](operator-reference.md#image-provenance-and-tags). Standalone image references come from one source of truth, [`src/deploy/standalone-images.sh`](../deploy/standalone-images.sh).

To build from source instead of pulling the pre-built image:

```bash
docker compose -f src/docker-compose.yaml build
docker compose -f src/docker-compose.yaml up -d
```

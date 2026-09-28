# A contributor with its own accounts

This companion Compose example runs the published v5 contributor image as an
unattended service. Only a container runtime with Compose and `curl` are needed
on the host. GitHub, Codex, git and Hive run in the image. The named
`contributor-home` volume contains the bot's entire home: provider logins,
GitHub login, Hive registration, configuration, checkouts and relay state.
No host home, credentials, sockets or source checkout are mounted.

The walkthrough uses Codex with a separate ChatGPT subscription. Other backends
can be selected with `AGENT_BACKEND`, but need their own in-container login and
must support Hive's headless mode; see the [relay guide](../../docs/contributor-relay.md).

## Download and configure

```bash
mkdir hive-bot
cd hive-bot
curl -fLO https://raw.githubusercontent.com/hivecommons/hive/v5/src/examples/contributor-isolated/compose.yaml
curl -fL https://raw.githubusercontent.com/hivecommons/hive/v5/src/examples/contributor-isolated/.env.example -o .env
chmod 600 .env
# Edit .env: choose the hive, backend, model and resource limits.
podman compose pull
```

Install a Compose provider for `podman compose` (such as podman-compose or Docker
Compose). Docker users can replace `podman compose` with `docker compose`.
Use the same directory and project name for every command, including login:
changing `COMPOSE_PROJECT_NAME` selects a different home volume. For repeatable
upgrades, pin `HIVE_CONTRIBUTOR_IMAGE` to a published version or digest.

Run rootless Podman as your normal user. The image and this example use container
UID/GID 1000; a new named volume inherits the image's `/home/dev` ownership.
If your Podman deployment requires keep-id, create `compose.override.yaml`:

```yaml
services:
  contributor:
    userns_mode: "keep-id:uid=1000,gid=1000"
```

This override is Podman-specific. Do not substitute your host UID in the service's
`user` field: the image's writable tool directories belong to container UID 1000.
If a previously created volume has different ownership, use a fresh project name
or correct its ownership with the runtime before proceeding.

## Sign in to the bot's accounts

These operator commands override the service entrypoint, so they work before Hive
registration exists. `/usr/bin/gh` is the image's real GitHub CLI; the normal
agent-facing `gh` wrapper intentionally refuses authentication management.

```bash
podman compose run --rm --entrypoint /usr/bin/gh contributor auth login --hostname github.com --git-protocol https --web --scopes repo,read:org --insecure-storage
podman compose run --rm --entrypoint /usr/bin/gh contributor auth status --hostname github.com
podman compose run --rm --entrypoint /bin/bash contributor -euc 'umask 077; mkdir -p "$HOME/.codex"; touch "$HOME/.codex/config.toml"; if ! grep -q "^cli_auth_credentials_store" "$HOME/.codex/config.toml"; then printf "cli_auth_credentials_store = \"file\"\n" | cat - "$HOME/.codex/config.toml" > "$HOME/.codex/config.toml.new"; mv "$HOME/.codex/config.toml.new" "$HOME/.codex/config.toml"; fi'
podman compose run --rm --entrypoint codex contributor login --device-auth
podman compose run --rm --entrypoint codex contributor login status
```

Use the browser URL and device code printed by each login command. Choose the
GitHub account and ChatGPT account intended for the bot; a private browser window
helps avoid selecting your personal account accidentally. Device authentication
may need enabling in your ChatGPT security settings or by your workspace admin.
The [Codex authentication documentation](https://developers.openai.com/codex/auth)
describes device login and `cli_auth_credentials_store = "file"`.

File storage keeps credentials in the named volume without a host keyring.
Treat the volume and its backups as secrets: GitHub stores its token in plaintext
with `--insecure-storage`, and Codex stores its login in `~/.codex/auth.json`.
The bot can access its own credentials. Host login changes have no effect here.

## Register with the chosen Hive

Run this once after GitHub login. It sends only the bot's GitHub username to the
Hive registration endpoint and saves the returned token privately inside the
home volume. It never sends the GitHub token to the Hive.

```bash
podman compose run --rm -T --entrypoint /bin/bash contributor -se <<'REGISTER'
set -euo pipefail
umask 077
hub="${HIVE_HUB/#wss:\/\//https://}"
hub="${hub%/contribute}"
case "$hub" in https://*) ;; *) echo 'Use an HTTPS Hive endpoint' >&2; exit 1 ;; esac
username=$(/usr/bin/gh api --hostname github.com user --jq .login)
mkdir -p "$HOME/.config/hive"
config="$HOME/.config/hive/contributor.env"
if [ -s "$config" ]; then
  echo 'Registration already saved; keep it or follow the account-change instructions.' >&2
  exit 1
fi
response=$(curl --fail --silent --show-error --max-time 30 \
  -H 'Content-Type: application/json' \
  --data "$(jq -nc --arg u "$username" '{github_username: $u}')" \
  "$hub/api/contribute/register")
token=$(jq -er '.registration_token | strings | select(length > 0)' <<<"$response") || {
  echo 'No token returned. If already registered, recover the existing token or use the reissue procedure in the relay guide.' >&2
  exit 1
}
id=$(jq -er '.contributor_id | strings | select(length > 0)' <<<"$response")
printf 'export HIVE_REGISTRATION_TOKEN=%q\nexport CONTRIBUTOR_USERNAME=%q\nexport CONTRIBUTOR_ID=%q\n' \
  "$token" "$username" "$id" > "$config"
echo "Registered $username; credentials saved in the bot volume."
REGISTER
```

Registration tokens are returned only once. For an existing registration, use
an existing token belonging to the same username, or follow
[Moving to another machine](../../docs/contributor-relay.md#moving-the-relay-to-another-machine)
for the authenticated `/api/contribute/reissue-token` procedure. Reissuing rotates
the token and disconnects any relay using the old one. If importing an existing
token, write `HIVE_REGISTRATION_TOKEN`, `CONTRIBUTOR_USERNAME`, and `CONTRIBUTOR_ID`
to `/home/dev/.config/hive/contributor.env` in a one-off container, with shell-quoted
values and mode 600. Do not copy your host's GitHub or provider configuration.

## Run and maintain

```bash
podman compose up -d
podman compose logs -f --tail=100 contributor
# Stop without deleting accounts or work:
podman compose down
# Upgrade or recreate while preserving the home volume:
podman compose pull
podman compose up -d --force-recreate
```

The entrypoint reads the saved GitHub login into `GH_TOKEN` on each start and
executes Hive's original entrypoint. The agent-facing wrapper and its write gates
remain active. No host `GH_TOKEN`, `GITHUB_TOKEN` or provider API key is forwarded.

`HIVE_CONTRIBUTOR_NICE=10` lowers scheduling priority for the relay and its child
processes; use 0 for normal priority or 1–19 to yield more under contention.
It needs no extra capability. Niceness is relative priority, not a CPU cap, and
cgroup scheduling also affects how CPU time is shared. `HIVE_CONTAINER_CPUS=2`
sets a hard CPU ceiling; raise it to use more spare cores. Memory and total
memory-plus-swap both default to 4 GiB. Rootless resource limits require runtime
and cgroup-controller support; check runtime warnings and `podman stats`.

Codex uses `HIVE_CODEX_SANDBOX_MODE=danger-full-access` **inside this container**:
the container is the isolation boundary. This avoids nested Bubblewrap/user
namespace/devpts failures. Keep the runtime's default seccomp profile and the
example's dropped capabilities; do not add privileged mode, a runtime socket or
host home mounts. This isolates host files and accounts, not network access to
your LAN. Only connect the bot to hives you trust to assign work.

To change accounts, stop the service first:

```bash
podman compose down
# Change GitHub identity, then repeat GitHub login and Hive registration:
podman compose run --rm --entrypoint /usr/bin/gh contributor auth logout --hostname github.com
podman compose run --rm --entrypoint /bin/bash contributor -euc 'mv "$HOME/.config/hive/contributor.env" "$HOME/.config/hive/contributor.env.previous"'
# Or change only the ChatGPT subscription, then repeat device login:
podman compose run --rm --entrypoint codex contributor logout
```

The saved Hive registration must match the new GitHub username before restarting.
The `.previous` file preserves the old registration; back it up separately before
changing identities again. Neither logout changes the host's accounts.

To reset **all** bot state, including credentials and unpushed work, deliberately
run `podman compose down --volumes`. This deletes the project's home volume;
ordinary `down` and container recreation do not. Resetting local state does not
revoke remote credentials or unregister the contributor: revoke credentials at
GitHub/OpenAI if needed, and recover or reissue the Hive token before registering
an already-known username again.

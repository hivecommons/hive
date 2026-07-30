#!/bin/sh
set -e

export TZ="${TZ:-America/New_York}"
export HIVE_API_PORT="${HIVE_API_PORT:-3002}"
export HIVE_PROXY_PORT="${HIVE_PROXY_PORT:-3001}"
export HIVE_STATIC_DIR="${HIVE_STATIC_DIR:-/opt/hive/proxy/public}"

# ── Config backup/restore across container recreation ─────────────────
# When Watchtower recreates the container (pull new image, stop old, start
# new), the Go binary's config.Save() may write an empty/default config to
# the bind-mounted hive.yaml during shutdown or early startup, wiping the
# host file. /data/ is a Docker named volume that persists across container
# recreations, so we keep a rolling backup there.
#
# In Kubernetes, the ConfigMap is the source of truth — admins update it to
# change settings like acmm_level, is_public, etc. The PVC backup is only
# used for disaster recovery if the ConfigMap volume is missing or empty.
HIVE_CONFIG_PATH="${HIVE_CONFIG:-/etc/hive/hive.yaml}"
HIVE_CONFIG_BACKUP="/data/hive.yaml.bak"

# Detect Kubernetes vs Docker environment
IS_KUBERNETES=false
if [ -n "${KUBERNETES_SERVICE_HOST:-}" ] || [ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
  IS_KUBERNETES=true
fi

if [ "$IS_KUBERNETES" = "true" ]; then
  # ── Kubernetes mode: ConfigMap seed + dashboard overlay ────────────
  if [ -f "$HIVE_CONFIG_PATH" ] && [ -s "$HIVE_CONFIG_PATH" ]; then
    # The copy-config init container re-seeded $HIVE_CONFIG_PATH from the
    # ConfigMap on this boot. Config saved via the dashboard (Config.Save
    # writes /etc/hive/hive.yaml, an emptyDir) would be silently lost —
    # so the Go binary also persists a secret-free overlay to the PVC
    # (/data/hive.yaml.dashboard) on every save, and we merge it over the
    # seed here, BEFORE the backup copy and before the agent-UID
    # enumeration below reads hive.yaml.
    #
    # Precedence: the overlay (last dashboard save) wins for everything
    # EXCEPT the hub/admin-managed keys, which the ConfigMap owns:
    #   - acmm_level     (set by the hub at provision / level change)
    #   - hub.is_public  (hub-managed visibility)
    # A missing, empty, unparsable, or implausible overlay (no
    # project.org / agents) leaves the ConfigMap seed untouched.
    #
    # github.{app_id,installation_id,key_file}: a dashboard-installed (or
    # hub-pushed, post App-install-webhook) GitHub App is PVC-overlay-only
    # state — the ConfigMap seed is never updated after install and keeps
    # whatever placeholder it was provisioned with. The overlay must win
    # here too, and a seed that already shows a real installed App must
    # never be silently downgraded by a missing/placeholder overlay.
    HIVE_DASHBOARD_OVERLAY="/data/hive.yaml.dashboard"
    if [ -f "$HIVE_DASHBOARD_OVERLAY" ] && [ -s "$HIVE_DASHBOARD_OVERLAY" ]; then
      if python3 - "$HIVE_CONFIG_PATH" "$HIVE_DASHBOARD_OVERLAY" <<'PYEOF'
import os, sys, yaml

seed_path, overlay_path = sys.argv[1], sys.argv[2]
with open(seed_path) as f:
    seed = yaml.safe_load(f) or {}
with open(overlay_path) as f:
    overlay = yaml.safe_load(f) or {}

# Sanity: the overlay must look like a full hive config. Save()'s
# validateSaveGuard enforces this on write; re-check before trusting it.
if not isinstance(overlay, dict):
    sys.exit(1)
if not (overlay.get('project') or {}).get('org') or not overlay.get('agents'):
    sys.exit(1)

# GitHub App credentials installed at runtime via the dashboard (or pushed
# by the hub after the App install webhook fires — see
# pkg/dashboard/api.go handleConfigGitHub) are persisted ONLY to this
# overlay, never back to the ConfigMap. The ConfigMap seed for a freshly
# claimed/placeholder hive still carries the placeholder app_id/
# installation_id/key_file, so if the overlay is missing, truncated, or
# otherwise reverted to a placeholder-looking github block, letting the
# seed win here would silently wipe a live App installation on every
# restart (observed: a hive's installed App reverted to placeholder
# creds after a pod restart, 401-looping on every GitHub call). Treat a
# seed that already has a real (non-placeholder) app_id/installation_id
# as a signal this hive has an installed App — refuse to let a
# github-less or placeholder-looking overlay silently downgrade it.
seed_gh = seed.get('github') or {}
overlay_gh = overlay.get('github') or {}


def _looks_placeholder(gh):
    key_file = gh.get('key_file') or ''
    app_id = gh.get('app_id')
    installation_id = gh.get('installation_id')
    if not app_id or not installation_id:
        return True
    if isinstance(key_file, str) and 'PLACEHOLDER' in key_file.upper():
        return True
    return False


seed_has_real_app = isinstance(seed_gh, dict) and not _looks_placeholder(seed_gh)
overlay_has_real_app = isinstance(overlay_gh, dict) and not _looks_placeholder(overlay_gh)
if seed_has_real_app and not overlay_has_real_app:
    sys.stderr.write(
        "[entrypoint] github: overlay missing/placeholder app credentials but "
        "ConfigMap seed has a real app_id/installation_id — keeping the seed's "
        "github block to avoid wiping an installed GitHub App\n"
    )
    overlay['github'] = seed_gh
elif overlay_has_real_app:
    # The overlay's installed-App credentials win over the seed, per the
    # standard overlay-wins precedence — but only trust key_file if the
    # PEM it names actually exists on this PVC. A dangling reference
    # (e.g. an interrupted key write) must not silently authenticate with
    # a missing/placeholder file; fail loud instead of 401-looping quietly.
    key_file = overlay_gh.get('key_file') or ''
    if isinstance(key_file, str) and key_file.startswith('/data/') and not os.path.isfile(key_file):
        sys.stderr.write(
            "[entrypoint] WARNING: overlay github.key_file=%s does not exist on "
            "the PVC — GitHub App auth will fail until it is re-installed\n" % key_file
        )

# The dashboard OVERLAY wins for acmm_level: the ConfigMap seed only ever
# carries the provision-time level and is NOT updated when the level is
# changed via the dashboard/apply-pack (which persists to the overlay). So
# the seed value is stale after any level change, and letting it win here
# silently reverted the level on every restart (issue #1856: a hive raised
# to L5 dropped back to its provisioned L3 on the next pod restart). Only
# fall back to the seed's acmm_level when the overlay has none.
if seed.get('acmm_level') is not None and overlay.get('acmm_level') is not None and seed['acmm_level'] != overlay['acmm_level']:
    sys.stderr.write("[entrypoint] acmm_level: overlay=%s wins over stale ConfigMap seed=%s (issue #1856)\n" % (overlay['acmm_level'], seed['acmm_level']))
if 'acmm_level' not in overlay and 'acmm_level' in seed:
    overlay['acmm_level'] = seed['acmm_level']
# hub.is_public is genuinely hub-managed (updated live via heartbeat), so
# the seed still wins for it when set.
if isinstance(seed.get('hub'), dict) and 'is_public' in seed['hub']:
    overlay.setdefault('hub', {})['is_public'] = seed['hub']['is_public']

# Atomic replace so a failure mid-write can never corrupt the seed.
tmp_path = seed_path + '.merged'
with open(tmp_path, 'w') as f:
    yaml.safe_dump(overlay, f, default_flow_style=False, sort_keys=False)
os.replace(tmp_path, seed_path)
PYEOF
      then
        echo "[entrypoint] K8s mode — dashboard overlay merged over ConfigMap seed (overlay wins for acmm_level and github app creds; ConfigMap wins for hub.is_public)"
      else
        echo "[entrypoint] K8s mode — dashboard overlay invalid, using ConfigMap seed as-is"
      fi
    fi
    # Write the (merged) config as the disaster-recovery backup.
    cp "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_BACKUP" 2>/dev/null || true
    echo "[entrypoint] K8s mode — ConfigMap is the seed, backup written to $HIVE_CONFIG_BACKUP"
  elif [ -f "$HIVE_CONFIG_BACKUP" ] && [ -s "$HIVE_CONFIG_BACKUP" ]; then
    # ConfigMap is missing or empty but a PVC backup exists — recover
    if cp "$HIVE_CONFIG_BACKUP" "$HIVE_CONFIG_PATH" 2>/dev/null; then
      echo "[entrypoint] K8s mode — ConfigMap missing/empty, RECOVERED from PVC backup"
    else
      export HIVE_CONFIG="$HIVE_CONFIG_BACKUP"
      echo "[entrypoint] K8s mode — ConfigMap missing/empty and read-only, using PVC backup directly"
    fi
  else
    echo "[entrypoint] ERROR: No ConfigMap at $HIVE_CONFIG_PATH and no PVC backup at $HIVE_CONFIG_BACKUP."
    echo "[entrypoint] Ensure the hive ConfigMap is created before deploying."
    exit 1
  fi
else
  # ── Docker mode: PVC backup is the source of truth ─────────────────
  if [ -f "$HIVE_CONFIG_PATH" ] && [ -s "$HIVE_CONFIG_PATH" ] && [ ! -f "$HIVE_CONFIG_BACKUP" ]; then
    # First boot: config exists but no PVC backup yet — seed the backup
    cp "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_BACKUP"
    echo "[entrypoint] First boot — config seeded to PVC: $HIVE_CONFIG_BACKUP"
  elif [ -f "$HIVE_CONFIG_PATH" ] && [ -s "$HIVE_CONFIG_PATH" ] && [ -f "$HIVE_CONFIG_BACKUP" ] && [ -s "$HIVE_CONFIG_BACKUP" ]; then
    # PVC backup is the source of truth (updated by Save()).
    # Try to copy it over the config path; if read-only (Docker bind mount),
    # override HIVE_CONFIG so the Go binary reads from the backup directly.
    if cp "$HIVE_CONFIG_BACKUP" "$HIVE_CONFIG_PATH" 2>/dev/null; then
      echo "[entrypoint] PVC backup restored to config path"
    else
      export HIVE_CONFIG="$HIVE_CONFIG_BACKUP"
      echo "[entrypoint] Config path is read-only — using PVC backup directly"
    fi
  elif [ -f "$HIVE_CONFIG_PATH" ] && [ ! -s "$HIVE_CONFIG_PATH" ] && [ -f "$HIVE_CONFIG_BACKUP" ] && [ -s "$HIVE_CONFIG_BACKUP" ]; then
    # Config was wiped to 0 bytes (Watchtower recreation) but backup exists — restore
    cp "$HIVE_CONFIG_BACKUP" "$HIVE_CONFIG_PATH"
    echo "[entrypoint] RECOVERED: $HIVE_CONFIG_PATH was empty (0 bytes), restored from $HIVE_CONFIG_BACKUP"
  elif [ -f "$HIVE_CONFIG_PATH" ] && [ ! -s "$HIVE_CONFIG_PATH" ]; then
    # Config is empty and no backup exists — fatal, cannot recover
    echo "[entrypoint] ERROR: $HIVE_CONFIG_PATH exists but is empty (0 bytes)."
    echo "[entrypoint] No backup found at $HIVE_CONFIG_BACKUP."
    echo "[entrypoint] This usually happens after 'docker compose down -v' wipes the data volume."
    echo "[entrypoint] Restore your hive.yaml from backup or version control and restart."
    exit 1
  fi
fi

# ── Cleanup stale stderr logs from previous agent launches ────────────
STDERR_LOG_MAX_AGE_MINUTES=60
find /tmp -maxdepth 1 -name '.hive-launch-stderr-*.log' -mmin +"$STDERR_LOG_MAX_AGE_MINUTES" -delete 2>/dev/null || true

# ── Root-only setup (runs once, then re-execs as dev) ──────────────────
if [ "$(id -u)" = "0" ]; then
  # Fix ownership of mounted volumes (may be root-owned from host bind mounts).
  # Skip recursive chown if /data is already owned by dev — critical for NFS
  # where recursive chown over thousands of files causes multi-minute delays.
  DATA_OWNER=$(stat -c '%u' /data 2>/dev/null || echo "0")
  if [ "$DATA_OWNER" != "1001" ]; then
    echo "[entrypoint] Fixing /data ownership (currently uid=$DATA_OWNER)..."
    chown -R dev:node /data 2>/dev/null || true
  fi
  chown dev:node /home/dev 2>/dev/null || true
  chown dev:node /etc/hive/hive.yaml "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_BACKUP" 2>/dev/null || true
  chmod u+rw,go-w "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_BACKUP" 2>/dev/null || true

  # Ensure the PVC secrets dir the dashboard writes API keys into
  # (/data/secrets/litellm_api_key) exists and is owned by the dev user.
  # The Go binary runs as non-root (uid 1001) and CANNOT chown, so if this
  # dir ever ends up root-owned the key save fails with EACCES. We create
  # and chown it here (as root, before dropping to dev) UNCONDITIONALLY —
  # not gated on DATA_OWNER — because the recursive /data chown above is
  # skipped when /data is already dev-owned, which would leave a
  # root-owned /data/secrets uncorrected.
  #
  # Mode 710, not 700: the directory needs group EXECUTE so agent UIDs (2001+,
  # group node) can TRAVERSE it to open /data/secrets/bob_api_key, which the
  # bobshell CLI reads as the agent (see the bob_api_key chmod 440 below).
  # Execute-without-read means agents can open that one path by name but
  # cannot LIST the directory, so other secrets in here are not enumerable.
  # Per-file modes remain the actual access control.
  mkdir -p /data/secrets && chown dev:node /data/secrets 2>/dev/null || true
  chmod 710 /data/secrets 2>/dev/null || true
  # The dashboard writes this file as dev with mode 600 (bobKeyFileMode); the
  # agent UID must be able to read it. Re-apply on every boot so a key saved
  # before this fix shipped is corrected without re-entering it.
  if [ -f /data/secrets/bob_api_key ]; then
    chown dev:node /data/secrets/bob_api_key 2>/dev/null || true
    chmod 440 /data/secrets/bob_api_key 2>/dev/null || true
  fi

  mkdir -p /var/run/hive-metrics && chown dev:node /var/run/hive-metrics 2>/dev/null || true
  mkdir -p /var/run/hive-metrics/agent-tokens && chown dev:node /var/run/hive-metrics/agent-tokens 2>/dev/null || true

  # Fix permissions on bind-mounted secret files (host may own them as
  # a different UID with mode 600, making them unreadable by dev/UID 1001)
  chown dev:node /secrets/*.pem 2>/dev/null || true
  chmod 644 /secrets/*.pem 2>/dev/null || true
  # API-key files are not .pem, so they need the same treatment. Without this
  # a host-side mode-600 file owned by a foreign UID reads as EACCES from the
  # Go binary (uid 1001), and every key-file read swallows the error — the key
  # silently resolves to "" and the backend looks unconfigured. Mode 400:
  # owner-read only (stricter than the .pem files, which ttyd/git also read).
  #
  # bob_api_key is the ONE exception and must be 440 (group node), not 400.
  # Every other key here is consumed IN-PROCESS by the Go binary running as
  # dev, so owner-read is sufficient. bob is different: the bobshell CLI reads
  # BOBSHELL_API_KEY itself while running AS THE AGENT UID (2001+, group node),
  # so a 400 dev-owned file is EACCES for it. The key then resolves empty and
  # bob silently falls back to the W3ID browser SSO flow that cannot complete
  # in a pod — the fleet-wide "stuck at the auth prompt" bug. Group-read only;
  # never world-readable.
  for key_file in /secrets/litellm_api_key; do
    [ -f "$key_file" ] || continue
    chown dev:node "$key_file" 2>/dev/null || true
    chmod 400 "$key_file" 2>/dev/null || true
  done
  if [ -f /secrets/bob_api_key ]; then
    chown dev:node /secrets/bob_api_key 2>/dev/null || true
    chmod 440 /secrets/bob_api_key 2>/dev/null || true
  fi

  # Copy read-only mounted secrets so dev user can read them
  if [ -f /etc/hive/gh-app-key.pem ]; then
    cp /etc/hive/gh-app-key.pem /var/run/hive-metrics/gh-app-key.pem
    chown dev:node /var/run/hive-metrics/gh-app-key.pem
    chmod 400 /var/run/hive-metrics/gh-app-key.pem
    export GH_APP_KEY_FILE=/var/run/hive-metrics/gh-app-key.pem
  fi

  # Seed data files from image into /data if they don't already exist
  if [ -d /opt/hive/seed-data ]; then
    echo "[entrypoint] Seeding data files..."
    # Seed as the runtime principal. A fresh volume is repaired above and is
    # writable by dev; copying as root here would recreate /data/agents and
    # other first-boot paths as root after the ownership repair, preventing
    # ordinary agents from creating their work directories.
    gosu dev:node cp -rn /opt/hive/seed-data/* /data/ 2>/dev/null || true
  fi

  # Create the shared root before role discovery. Config-only installations
  # have no /etc/hive/agents and no pre-existing /data/beads, so deferring the
  # mkdir to UID provisioning would inherit the caller's owner-only umask and
  # prevent ordinary Hive from traversing every role store on first boot.
  mkdir -p /home/dev /data/beads
  chown dev:node /data/beads 2>/dev/null || true
  chmod 0750 /data/beads 2>/dev/null || true

  # Create beads symlinks: /home/dev/<agent>-beads -> /data/beads/<agent>
  if [ -d /etc/hive/agents ]; then
    for envfile in /etc/hive/agents/*.env; do
      [ -f "$envfile" ] || continue
      agent="$(basename "$envfile" .env)"
      mkdir -p "/data/beads/${agent}"
      ln -sfn "/data/beads/${agent}" "/home/dev/${agent}-beads"
      echo "[entrypoint] Beads symlink: /home/dev/${agent}-beads -> /data/beads/${agent}"
    done
  fi
  # Include first-level hidden role stores while excluding . and .. themselves.
  for beaddir in /data/beads/*/ /data/beads/.[!.]*/ /data/beads/..?*/; do
    beaddir="${beaddir%/}"
    [ -d "$beaddir" ] && [ ! -L "$beaddir" ] || continue
    agent="$(basename "$beaddir")"
    if [ ! -L "/home/dev/${agent}-beads" ]; then
      ln -sfn "/data/beads/${agent}" "/home/dev/${agent}-beads"
      echo "[entrypoint] Beads symlink: /home/dev/${agent}-beads -> /data/beads/${agent}"
    fi
    # Retired/disabled role stores are reopened only by ordinary Hive. Make
    # legacy owner-only stores readable by dev without following any link;
    # active roles are reassigned to their scoped group below.
    find -P "$beaddir" -xdev -type d -exec chown dev:node {} + -exec chmod 0700 {} + 2>/dev/null || true
    find -P "$beaddir" -xdev -type f -exec chown dev:node {} + -exec chmod 0600 {} + 2>/dev/null || true
  done
  chown dev:node /home/dev 2>/dev/null || true

  # Shared CLI auth/cache lives in /data/home (persistent volume).
  # Make it group-writable so all agent UIDs (node group) can use it.
  # The manager sets HOME=/data/home for agent tmux sessions.
  mkdir -p /data/home/.config /data/home/.copilot /data/home/.claude/session-env /data/home/.codex /data/config/github-copilot /home/dev/.config
  # These parents are created after the mounted-volume ownership check above.
  # On a fresh volume whose root was pre-seeded as dev, mkdir would otherwise
  # recreate them as root while DATA_OWNER=1001 skips the recursive repair.
  # Normalize the shared parents unconditionally before any agent can start.
  chown dev:node /data/home /data/home/.config /data/config /data/config/github-copilot 2>/dev/null || true
  chmod 2775 /data/home /data/home/.config /data/config /data/config/github-copilot 2>/dev/null || true
  chmod 2770 /data/home/.copilot 2>/dev/null || true
  chown dev:node /data/home/.copilot 2>/dev/null || true
  chmod 2775 /data/home/.claude /data/home/.claude/session-env 2>/dev/null || true
  chown -R dev:node /data/home/.claude 2>/dev/null || true
  # Codex (newer method) writes a local sqlite state db under $HOME/.codex —
  # pre-create it group-writable + setgid so every agent UID (node group) can
  # init it, matching .claude/.copilot. Without this, switching an agent to the
  # codex backend fails with "Permission denied" initializing state_N.sqlite.
  chmod 2775 /data/home/.codex 2>/dev/null || true
  chown -R dev:node /data/home/.codex 2>/dev/null || true
  # The dashboard process keeps HOME=/home/dev, while authenticated CLI state
  # lives on the persistent volume. Give controller-owned Codex health checks
  # the explicit bounded home instead of symlinking credential directories or
  # copying auth.json into the image account.
  export CODEX_HOME="${CODEX_HOME:-/data/home/.codex}"
  ln -sfn /data/config/github-copilot /home/dev/.config/github-copilot
  ln -sfn /data/config/github-copilot /data/home/.config/github-copilot
  ln -sfn /data/home/.copilot /home/dev/.copilot
  # Set group-write + setgid on shared dirs — skip if already done (saves 100s+ on NFS).
  # The polling perm guard handles ongoing config.json fixes regardless.
  NEED_PERM_FIX=false
  if [ -d "/data/home/.copilot" ] && [ -d "/data/home/.claude" ]; then
    COPILOT_PERMS=$(stat -c '%a' "/data/home/.copilot" 2>/dev/null || echo "755")
    CLAUDE_PERMS=$(stat -c '%a' "/data/home/.claude" 2>/dev/null || echo "755")
    case "$COPILOT_PERMS" in
      27[0-9][0-9]|37[0-9][0-9]) ;; # already has group-write + setgid
      *) NEED_PERM_FIX=true ;;
    esac
    case "$CLAUDE_PERMS" in
      27[0-9][0-9]|37[0-9][0-9]) ;; # already has group-write + setgid
      *) NEED_PERM_FIX=true ;;
    esac
  else
    NEED_PERM_FIX=true
  fi
  if [ "$NEED_PERM_FIX" = "true" ]; then
    # Run synchronously — on fresh provisions /data/home is nearly empty so
    # this completes in <1s. Running in background caused a race where agents
    # started before perms were fixed, hitting EACCES on config.json.
    echo "[entrypoint] Fixing /data/home perms..."
    chmod -R g+rwX /data/home 2>/dev/null
    find /data/home -type d -exec chmod g+s {} + 2>/dev/null
    if [ "$DATA_OWNER" != "1001" ]; then
      chown -R dev:node /data/config /data/home 2>/dev/null
    fi
    echo "[entrypoint] perm fix complete"
  else
    echo "[entrypoint] /data/home perms OK — skipping"
  fi
  chown dev:node /home/dev/.config 2>/dev/null || true
  # Copilot CLI rewrites config.json with 0600 on every token refresh,
  # locking out other agent UIDs. Run inotify (if available) AND polling
  # as belt-and-suspenders — inotify is unreliable on NFS but instant on
  # local storage; polling is reliable everywhere but has a 5s delay.
  if command -v inotifywait >/dev/null 2>&1; then
    (
      while inotifywait -qq -e close_write,moved_to /data/home/.copilot/ 2>/dev/null; do
        chmod 660 /data/home/.copilot/config.json 2>/dev/null
        chown dev:node /data/home/.copilot/config.json 2>/dev/null
      done
    ) &
    (
      while inotifywait -qq -e close_write,moved_to,create /data/home/.claude/ 2>/dev/null; do
        chmod -R g+rwX /data/home/.claude 2>/dev/null
        find /data/home/.claude -type d -exec chmod g+s {} + 2>/dev/null
        chown -R dev:node /data/home/.claude 2>/dev/null
      done
    ) &
    (
      while inotifywait -qq -e close_write,moved_to,create /data/home/.codex/ 2>/dev/null; do
        chmod -R g+rwX /data/home/.codex 2>/dev/null
        find /data/home/.codex -type d -exec chmod g+s {} + 2>/dev/null
        chown -R dev:node /data/home/.codex 2>/dev/null
      done
    ) &
    echo "[entrypoint] inotify perm guard active (copilot + claude + codex)"
  fi
  (
    CYCLE=0
    while true; do
      # Fast cycle: fix config.json every 5s (copilot rewrites it with 600)
      chmod 660 /data/home/.copilot/config.json 2>/dev/null
      chown dev:node /data/home/.copilot/config.json 2>/dev/null
      # Slow cycle: fix entire /data/home tree every 5 min (new dirs from agents)
      CYCLE=$((CYCLE + 1))
      if [ "$CYCLE" -ge 60 ]; then
        chmod -R g+rwX /data/home/.cache /data/home/.copilot /data/home/.claude /data/home/.codex 2>/dev/null
        find /data/home/.cache /data/home/.claude /data/home/.codex -type d -exec chmod g+s {} + 2>/dev/null
        CYCLE=0
      fi
      sleep 5
    done
  ) &
  echo "[entrypoint] polling perm guard active (config.json 5s, cache 5m)"
  echo "[entrypoint] CLI config: /data/home (shared, group-writable for agent UIDs)"

  # Write .bashrc for agent shells. GH_TOKEN is NOT exported here — gh-wrapper.sh
  # injects the token at call time, avoiding exposure in shell env / transcripts.
  cat > /data/home/.bashrc <<'BASHRC' 2>/dev/null || true
# Hive agent shell environment
export SSL_CERT_FILE=/data/proxy-ca.pem
BASHRC
  chmod 644 /data/home/.bashrc 2>/dev/null || true

  # ── Per-agent UID isolation ──────────────────────────────────────────
  # Extract agent names from config + pack YAML, create system users,
  # write UID map, and set up iptables to force all outbound :443
  # through the MITM proxy (so agents can't bypass it via unset HTTPS_PROXY).
  HIVE_CONFIG="${HIVE_CONFIG:-/etc/hive/hive.yaml}"
  HIVE_UID_BASE=2001
  PROXY_UID=1001

  # Collect agent names from hive.yaml (map keys) and pack YAML (list items)
  AGENT_NAMES=""
  if [ -f "$HIVE_CONFIG" ]; then
    AGENT_NAMES=$(python3 -c "
import glob, yaml, sys, os
effective_agents = {}
with open('$HIVE_CONFIG') as f:
    cfg = yaml.safe_load(f) or {}
agents = cfg.get('agents', {})
if isinstance(agents, dict):
    effective_agents.update(agents)
elif isinstance(agents, list):
    for a in agents:
        if isinstance(a, dict) and 'name' in a:
            effective_agents[a['name']] = a
# Per-agent files replace the base entry, matching config.LoadWithOverrides.
data_cfg = cfg.get('data', {})
agents_dir = '/data/agent-configs'
if isinstance(data_cfg, dict) and data_cfg.get('agents_dir'):
    agents_dir = os.path.expandvars(str(data_cfg['agents_dir']))
for path in sorted(glob.glob(os.path.join(agents_dir, '*.yaml'))):
    with open(path) as af:
        agent = yaml.safe_load(af) or {}
    effective_agents[os.path.basename(path)[:-5]] = agent
# Also check pack YAML if HIVE_LEVEL is set
level = os.environ.get('HIVE_LEVEL', '')
if level:
    for p in glob.glob('/opt/hive/packs/level-*.yaml') + glob.glob('/data/packs/level-*.yaml'):
        try:
            with open(p) as pf:
                pack = yaml.safe_load(pf) or {}
            pack_agents = pack.get('agents', [])
            if isinstance(pack_agents, list):
                for a in pack_agents:
                    if isinstance(a, dict) and 'name' in a and a.get('enabled', True) is not False:
                        effective_agents.setdefault(a['name'], a)
            elif isinstance(pack_agents, dict):
                for name, agent in pack_agents.items():
                    if not isinstance(agent, dict) or agent.get('enabled', True) is not False:
                        effective_agents.setdefault(name, agent)
        except Exception:
            pass
names = {
    name for name, agent in effective_agents.items()
    if not isinstance(agent, dict) or agent.get('enabled', True) is not False
}
print('\n'.join(sorted(names)))
" 2>/dev/null) || true
  fi

  # Pre-cleanup: remove stale workspace artifacts from agent dirs.
  # Runs in BACKGROUND to avoid blocking startup — on slow CephFS with many
  # dirs, sequential rm -rf can exceed the startup probe timeout (10 min),
  # causing a livelock where the container restarts before cleanup finishes.
  # The Go binary's workspace_cleanup.go handles ongoing cleanup at runtime.
  WORKSPACE_MAX_AGE_SECS=7200
  if [ -d "$agentWorkspaceRoot" ] 2>/dev/null || [ -d /data/agents ]; then
    _CLEANUP_ROOT="${agentWorkspaceRoot:-/data/agents}"
    (
      _CLEANUP_DIRS=0
      _CLEANUP_FILES=0
      _NOW=$(date +%s)
      for _agent_dir in "$_CLEANUP_ROOT"/*/; do
        [ -d "$_agent_dir" ] || continue
        _agent_base=$(basename "$_agent_dir")
        for _entry in "$_agent_dir"*; do
          [ -e "$_entry" ] || continue
          _entry_name=$(basename "$_entry")
          case "$_entry_name" in .cache|.config|.npm-cache|bin|beads.json|stats.json|.*) continue ;; esac
          _MTIME=$(stat -c '%Y' "$_entry" 2>/dev/null || echo "$_NOW")
          _AGE=$((_NOW - _MTIME))
          if [ "$_AGE" -gt "$WORKSPACE_MAX_AGE_SECS" ]; then
            if [ -d "$_entry" ]; then
              echo "[entrypoint] bg-cleanup: removing dir ${_agent_base}/${_entry_name}"
              rm -rf "$_entry" 2>/dev/null && _CLEANUP_DIRS=$((_CLEANUP_DIRS + 1))
            else
              rm -f "$_entry" 2>/dev/null && _CLEANUP_FILES=$((_CLEANUP_FILES + 1))
            fi
          fi
        done
      done
      echo "[entrypoint] bg-cleanup: done — removed $_CLEANUP_DIRS dirs, $_CLEANUP_FILES files"
    ) &
    echo "[entrypoint] Pre-cleanup: started in background (PID $!)"
  fi

  if [ -n "$AGENT_NAMES" ]; then
    echo "[entrypoint] Creating per-agent users for UID isolation..."
    mkdir -p /var/run/hive
    chown dev:node /var/run/hive
    chmod 0755 /var/run/hive
    UID_OFFSET=0
    UID_JSON='{"agents":{'
    FIRST=true
    echo "$AGENT_NAMES" | while read -r agent_name; do
      [ -z "$agent_name" ] && continue
      AGENT_UID=$((HIVE_UID_BASE + UID_OFFSET))
      ROLE_GROUP="hive-${agent_name}"
      if ! getent group "$ROLE_GROUP" >/dev/null 2>&1; then
        groupadd --gid "$AGENT_UID" "$ROLE_GROUP" 2>/dev/null || true
      fi
      if ! id "hive-${agent_name}" >/dev/null 2>&1; then
        useradd --system -u "$AGENT_UID" -g "$ROLE_GROUP" -G node -d /data/home -M -s /bin/bash "hive-${agent_name}" 2>/dev/null || true
      else
        usermod -g "$ROLE_GROUP" -a -G node "hive-${agent_name}" 2>/dev/null || true
      fi
      usermod -a -G "$ROLE_GROUP" dev 2>/dev/null || true
      mkdir -p "/data/agents/${agent_name}"
      # Skip recursive chown if already owned by this agent — avoids NFS
      # contention during rolling updates when the old pod is still writing.
      AGENT_DIR_OWNER=$(stat -c '%u' "/data/agents/${agent_name}" 2>/dev/null || echo "0")
      if [ "$AGENT_DIR_OWNER" != "$AGENT_UID" ]; then
        chown -R "hive-${agent_name}:node" "/data/agents/${agent_name}" 2>/dev/null || true
      fi
      # Ensure agent dirs are group-writable so the dev user (same node group)
      # can clean up stale workspaces at runtime without root privileges.
      chmod g+rwX "/data/agents/${agent_name}" 2>/dev/null || true
      mkdir -p "/data/beads/${agent_name}"
      ln -sfn "/data/beads/${agent_name}" "/home/dev/${agent_name}-beads"
      # Only dev and this role join ROLE_GROUP. Setgid preserves that group on
      # files atomically replaced by either principal; links are never followed.
      find -P "/data/beads/${agent_name}" -xdev -type d -exec chown "hive-${agent_name}:${ROLE_GROUP}" {} + -exec chmod 2770 {} + 2>/dev/null || true
      find -P "/data/beads/${agent_name}" -xdev -type f -exec chown "hive-${agent_name}:${ROLE_GROUP}" {} + -exec chmod 0660 {} + 2>/dev/null || true
      echo "[entrypoint] Agent user: hive-${agent_name} (UID ${AGENT_UID})"
      UID_OFFSET=$((UID_OFFSET + 1))
    done

    # Write uid-map.json using python for proper JSON
    python3 -c "
import json, os
names = '''$AGENT_NAMES'''.strip().split('\n')
names = [n for n in names if n]
agents = {}
for i, name in enumerate(sorted(names)):
    agents[name] = $HIVE_UID_BASE + i
uid_map = {
    'agents': agents,
    'proxy_uid': $PROXY_UID,
    'base_uid': $HIVE_UID_BASE,
    'iptables_active': False
}
os.makedirs('/var/run/hive', exist_ok=True)
with open('/var/run/hive/uid-map.json', 'w') as f:
    json.dump(uid_map, f, indent=2)
print('[entrypoint] UID map written to /var/run/hive/uid-map.json')
" 2>/dev/null || echo "[entrypoint] WARN: Failed to write UID map"
    chown dev:node /var/run/hive/uid-map.json 2>/dev/null || true
    chmod 0644 /var/run/hive/uid-map.json 2>/dev/null || true

    # Set up iptables: redirect all outbound :443 to the MITM proxy port,
    # except traffic from the proxy itself (UID 1001 / dev user).
    PROXY_PORT=18443
    if command -v iptables >/dev/null 2>&1; then
      if iptables -t nat -N HIVE_PROXY 2>/dev/null; then
        iptables -t nat -A HIVE_PROXY -m owner --uid-owner 0 -j RETURN
        iptables -t nat -A HIVE_PROXY -m owner --uid-owner "$PROXY_UID" -j RETURN
        iptables -t nat -A HIVE_PROXY -p tcp --dport 443 -j REDIRECT --to-ports "$PROXY_PORT"
        iptables -t nat -A OUTPUT -j HIVE_PROXY
        echo "[entrypoint] iptables: outbound :443 -> :${PROXY_PORT} (proxy UID ${PROXY_UID} exempt)"
        # Update uid-map to record iptables active
        python3 -c "
import json
with open('/var/run/hive/uid-map.json') as f:
    m = json.load(f)
m['iptables_active'] = True
with open('/var/run/hive/uid-map.json', 'w') as f:
    json.dump(m, f, indent=2)
" 2>/dev/null || true
      else
        echo "[entrypoint] WARN: iptables chain creation failed (need NET_ADMIN capability)"
      fi
    else
      echo "[entrypoint] WARN: iptables not found, proxy enforcement is advisory-only"
    fi
  fi

  # Drop to non-root user for all runtime processes.
  # Claude Code refuses --dangerously-skip-permissions as root.
  # Test gosu first — on some NFS/container combos it fails with EPERM.
  if command -v gosu >/dev/null 2>&1 && gosu dev true 2>/dev/null; then
    echo "[entrypoint] Dropping to dev user"
    exec gosu dev "$0" "$@"
  else
    echo "[entrypoint] WARN: gosu unavailable or failed, continuing as root"
  fi
fi

# ── Non-root setup and process launch (runs as dev) ────────────────────

# Ensure vault directories exist
mkdir -p /data/vaults
if [ -n "${HIVE_WIKI_GIT_URL:-}" ] && [ ! -d /data/vaults/hive-wiki/.git ]; then
  echo "[entrypoint] Cloning wiki vault from ${HIVE_WIKI_GIT_URL}..."
  git clone "${HIVE_WIKI_GIT_URL}" /data/vaults/hive-wiki 2>/dev/null || \
    echo "[entrypoint] Git clone failed — vault will be initialized empty"
fi
mkdir -p /data/vaults/hive-wiki

# Configure git identity and credential helper for GitHub App token
git config --global user.name "kubestellar-hive"
git config --global user.email "hive-bot@kubestellar.io"
git config --global --replace-all credential.helper ""
git config --global --replace-all "credential.https://github.com.helper" "/usr/local/bin/git-credential-hive.sh"

# Generate initial GitHub App token if credentials are available
if [ -x /usr/local/bin/hive-config.sh ]; then
  . /usr/local/bin/hive-config.sh 2>/dev/null || true
fi
# Use the dev-readable copy if the configured key file isn't readable
if [ -n "${GH_APP_KEY_FILE:-}" ] && [ ! -r "$GH_APP_KEY_FILE" ]; then
  if [ -r /var/run/hive-metrics/gh-app-key.pem ]; then
    export GH_APP_KEY_FILE=/var/run/hive-metrics/gh-app-key.pem
  fi
fi
if [ -n "${GH_APP_ID:-}" ] && [ -n "${GH_APP_INSTALLATION_ID:-}" ]; then
  echo "[entrypoint] Generating GitHub App token..."
  /usr/local/bin/gh-app-token.sh >/dev/null 2>&1 && \
    echo "[entrypoint] Token cached at /var/run/hive-metrics/gh-app-token.cache" || \
    echo "[entrypoint] WARN: GitHub App token generation failed"
  export HIVE_GITHUB_TOKEN="$(cat /var/run/hive-metrics/gh-app-token.cache 2>/dev/null || true)"
fi

# Load Copilot PAT from persistent volume so the Go binary can inject it
# into agent tmux sessions via COPILOT_GITHUB_TOKEN env var.
COPILOT_PAT_FILE="/data/copilot-token-pat"
if [ -f "$COPILOT_PAT_FILE" ] && [ -s "$COPILOT_PAT_FILE" ]; then
  export COPILOT_GITHUB_TOKEN
  COPILOT_GITHUB_TOKEN="$(cat "$COPILOT_PAT_FILE")"
  echo "[entrypoint] Copilot PAT loaded from $COPILOT_PAT_FILE"
fi

echo "[entrypoint] Starting Go binary on :${HIVE_API_PORT} (uid=$(id -u))"
hive "$@" &
HIVE_PID=$!

sleep 1

# Install the MITM proxy CA into the system trust store so that
# agent sub-processes (git, curl) trust the forged certificates.
# Also set NODE_EXTRA_CA_CERTS for Node.js (Copilot, etc.).
if [ -f /data/proxy-ca.pem ]; then
  if command -v gosu >/dev/null 2>&1; then
    gosu root sh -c 'cp /data/proxy-ca.pem /usr/local/share/ca-certificates/hive-proxy-ca.crt && update-ca-certificates' 2>/dev/null \
      && echo "[entrypoint] proxy CA installed to system trust store" \
      || echo "[entrypoint] WARN: proxy CA install via gosu failed"
  elif cp /data/proxy-ca.pem /usr/local/share/ca-certificates/hive-proxy-ca.crt 2>/dev/null; then
    update-ca-certificates 2>/dev/null && echo "[entrypoint] proxy CA installed to system trust store"
  else
    echo "[entrypoint] WARN: could not install proxy CA to system store (non-root)"
  fi
  export NODE_EXTRA_CA_CERTS=/data/proxy-ca.pem
  echo "[entrypoint] NODE_EXTRA_CA_CERTS set for Node.js agents"
fi
# Watch for CA cert changes (Go binary may regenerate it) and re-install
(
  PREV_HASH=""
  while true; do
    sleep 10
    [ -f /data/proxy-ca.pem ] || continue
    CUR_HASH=$(sha256sum /data/proxy-ca.pem 2>/dev/null | cut -d' ' -f1)
    if [ -n "$CUR_HASH" ] && [ "$CUR_HASH" != "$PREV_HASH" ]; then
      cp /data/proxy-ca.pem /usr/local/share/ca-certificates/hive-proxy-ca.crt 2>/dev/null \
        && update-ca-certificates 2>/dev/null \
        && echo "[entrypoint] proxy CA re-installed to system trust store (hash changed)"
      PREV_HASH="$CUR_HASH"
    fi
  done
) &

echo "[entrypoint] Starting Node.js proxy on :${HIVE_PROXY_PORT} → :${HIVE_API_PORT}"
cd /opt/hive/proxy && node server.js &
PROXY_PID=$!

TTYD_PORT="${HIVE_TTYD_PORT:-7681}"
echo "[entrypoint] Starting ttyd on :${TTYD_PORT}"
# Wrap ttyd in a respawn loop: ttyd exits on SIGHUP (its close signal),
# and orphaned LISTEN sockets block rebind, so we wait before retrying.
TTYD_RESPAWN_DELAY_SECS=5
(
  trap '' HUP
  while true; do
    ttyd -W -a -p "${TTYD_PORT}" -t fontSize=14 -t disableLeaveAlert=true /usr/local/bin/ttyd-tmux.sh
    echo "[entrypoint] ttyd exited (rc=$?), respawning in ${TTYD_RESPAWN_DELAY_SECS}s..."
    sleep "$TTYD_RESPAWN_DELAY_SECS"
  done
) &
TTYD_PID=$!

cleanup() {
  echo "[entrypoint] Shutting down..."
  # PVC backup is managed by Save() — no shutdown backup needed
  kill "$TTYD_PID" 2>/dev/null || true
  kill "$PROXY_PID" 2>/dev/null || true
  kill "$HIVE_PID" 2>/dev/null || true
  wait "$HIVE_PID" 2>/dev/null || true
  wait "$PROXY_PID" 2>/dev/null || true
}
trap cleanup INT TERM

wait "$HIVE_PID"

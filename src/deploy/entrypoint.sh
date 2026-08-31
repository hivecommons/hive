#!/bin/sh
set -e

export TZ="${TZ:-America/New_York}"
export HIVE_API_PORT="${HIVE_API_PORT:-3002}"
export HIVE_PROXY_PORT="${HIVE_PROXY_PORT:-3001}"
export HIVE_STATIC_DIR="${HIVE_STATIC_DIR:-/opt/hive/proxy/public}"

# Packet mark (fwmark / SO_MARK) used to exempt the MITM proxy's OWN upstream
# :443 dials from the forced-egress redirect, so its traffic is not looped back
# into itself. This is the SINGLE SOURCE OF TRUTH: it is exported here (in both
# the root and the re-exec'd dev pass) so the iptables `-m mark --mark` exemption
# below and the Go proxy (which reads HIVE_PROXY_EGRESS_MARK, defaulting to this
# same 0x1112) stay in lockstep. Unlike `-m owner --uid-owner`, a packet mark
# needs no xt_owner kernel module, so the exemption works on BOTH OKE and
# OpenShift/OVN (where xt_owner is absent). Value is arbitrary but must match the
# Go default (proxy.defaultProxyEgressMark).
export HIVE_PROXY_EGRESS_MARK="${HIVE_PROXY_EGRESS_MARK:-0x1112}"

# Distinct preflight exit code (#3760 follow-up) for the "refusing to start
# because CAP_NET_ADMIN is missing" case, so an operator or orchestrator can
# tell it apart from every other startup failure in this script (bad config,
# missing iptables binary, netfilter lock contention — all still plain `exit
# 1`) from the exit code alone, without parsing the log. 77 is sysexits.h's
# EX_NOPERM ("permission denied"), the closest standard-adjacent match for
# "the container was not granted a capability it needs" — chosen over a
# script-local number so it means the same thing to anyone who already knows
# sysexits.h. See the FATAL branch below and src/docs/net-admin-requirement.md.
EXIT_NET_ADMIN_REQUIRED=77

# ── Config backup/restore across container recreation ─────────────────
# When Watchtower recreates the container (pull new image, stop old, start
# new), the Go binary's config.Save() may write an empty/default config to
# the bind-mounted hive.yaml during shutdown or early startup, wiping the
# host file. /data/ is a Docker named volume that persists across container
# recreations, so we keep a rolling backup there.
#
# In Kubernetes, the ConfigMap is the source of truth — admins update it to
# change settings like acmm_level, is_public, etc. The PVC copy is only
# used for disaster recovery if the ConfigMap volume is missing or empty.
#
# In Docker/LXC there is no ConfigMap and no overlay: the PVC copy IS the
# boot-time source of truth, restored over the config path (or read directly
# when that path is read-only) on every boot. See the Docker branch below.
#
# ── Naming: hive.yaml.runtime, formerly hive.yaml.bak ─────────────────
# The old .bak name implied "the restorable backup", which is only half
# true and cost real debugging time: on K8s it is a post-merge SNAPSHOT
# written after the merge, while on Docker/LXC it is a live boot INPUT.
# ".runtime" is honest for both — it is the runtime config as this hive
# last had it.
#
# MIGRATION (one release): ~51 live hives have only the old-named file on
# their PVC. We WRITE the new name and READ the old one when the new is
# absent. We deliberately do NOT rename on the PVC: on Docker/LXC that
# file is the only copy of the live config, and mutating it at boot risks
# losing owner customisations with no warning. The old name is picked up
# read-only until the first save writes the new one, after which both
# exist and the new one wins.
HIVE_CONFIG_PATH="${HIVE_CONFIG:-/etc/hive/hive.yaml}"
HIVE_CONFIG_RUNTIME="/data/hive.yaml.runtime"
HIVE_CONFIG_RUNTIME_LEGACY="/data/hive.yaml.bak"

# These PVC config copies can carry dashboard.auth_token (and github.token in
# PAT mode). Files written before the 0600 fix (#5331) are world-readable, and
# /data is world-traversable, so tighten pre-existing copies at boot.
# Best-effort: on a read-only or foreign-owned PVC this must not abort boot.
for _cfg in "$HIVE_CONFIG_RUNTIME" "$HIVE_CONFIG_RUNTIME_LEGACY" /data/hive.yaml.dashboard; do
  [ -f "$_cfg" ] && chmod 600 "$_cfg" 2>/dev/null || true
done
unset _cfg

# hive_runtime_config_read echoes the path to read the persisted runtime
# config from: the new name when it is present and non-empty, else the
# legacy name when that is, else empty. Read-only — it never creates,
# renames or removes anything on the PVC.
hive_runtime_config_read() {
  if [ -f "$HIVE_CONFIG_RUNTIME" ] && [ -s "$HIVE_CONFIG_RUNTIME" ]; then
    echo "$HIVE_CONFIG_RUNTIME"
  elif [ -f "$HIVE_CONFIG_RUNTIME_LEGACY" ] && [ -s "$HIVE_CONFIG_RUNTIME_LEGACY" ]; then
    echo "$HIVE_CONFIG_RUNTIME_LEGACY"
  fi
}

# hive_ghe_git_host echoes this hive's configured GitHub host when it is NOT
# public github.com (i.e. a GitHub Enterprise instance such as github.ibm.com),
# and nothing at all otherwise. Derived the same way the Go binary's
# GitHubConfig.HostLabel() derives it (pkg/config/config.go): prefer
# github.base_url, fall back to the host portion of github.api_url, strip the
# scheme and a trailing /api/v3.
#
# Defined here, at the top, because BOTH boot phases need it: the root phase
# writes /etc/gitconfig (which every agent UID reads) and the dev phase writes
# ~dev/.gitconfig. Deriving it once keeps the two from drifting apart, which is
# exactly the failure mode #5343 was.
hive_ghe_git_host() {
  _hgh_cfg="${HIVE_CONFIG:-/etc/hive/hive.yaml}"
  [ -f "$_hgh_cfg" ] || return 0
  python3 -c "
import sys, yaml
try:
    with open(sys.argv[1]) as f:
        cfg = yaml.safe_load(f) or {}
except Exception:
    sys.exit(0)
gh = cfg.get('github') or {}
pick = (gh.get('base_url') or gh.get('api_url') or '').strip()
if pick.startswith('https://'):
    pick = pick[len('https://'):]
elif pick.startswith('http://'):
    pick = pick[len('http://'):]
pick = pick.rstrip('/')
if pick.endswith('/api/v3'):
    pick = pick[: -len('/api/v3')]
host = pick.split('/', 1)[0]
if host and host.lower() != 'api.github.com' and host.lower() != 'github.com':
    print(host)
" "$_hgh_cfg" 2>/dev/null || true
}

# hive_write_system_gitconfig writes /etc/gitconfig — the SYSTEM-level git
# config, read by EVERY UID regardless of $HOME.
#
# WHY THIS EXISTS (#5343). The credential helper used to be installed with
# `git config --global`, which is per-$HOME. The entrypoint runs as dev
# (ENV HOME=/home/dev), so it landed in /home/dev/.gitconfig — while agents run
# under per-agent UIDs with HOME=/data/home/agents/<name>. Measured on a hosted
# GHE spoke: `su -s /bin/sh hive-quality -c 'git config --get-regexp credential'`
# returned NOTHING for both --global and --system, because there was no
# /etc/gitconfig either. Agents committed branches they could never push, and
# the failure surfaced only as a line inside an otherwise-healthy session.
#
# /etc/gitconfig is the right home for it: the helper is already a single
# system-wide binary at /usr/local/bin/git-credential-hive.sh, and a system file
# sidesteps the per-UID ownership question that a shared /data/home/.gitconfig
# would introduce (multiple agent UIDs, one directory).
#
# NO SECRET LIVES HERE. This file names a helper PATH and a bot identity. The
# token is minted by the helper, per agent, from the per-agent scoped cache. So
# 0644 (world-readable) is correct and required — every agent UID must read it.
#
# PRECEDENCE is safe: git reads system < global < local. /home/dev/.gitconfig
# still exists for the dev user and for the contributor-relay/local-mode paths,
# and it sets the SAME helper for the SAME hosts, so it shadows nothing. Agents
# have no global config at all, so for them the system file is the only layer.
#
# HIVE_SYSTEM_GITCONFIG is a TEST SEAM (same convention as sharedAgentHome in
# pkg/agent). It is never set in production; the regression test points it at a
# temp file so it can exercise the real writer without touching /etc.
hive_write_system_gitconfig() {
  _hwsg_path="${HIVE_SYSTEM_GITCONFIG:-/etc/gitconfig}"
  _hwsg_host="$(hive_ghe_git_host)"

  # Refuse a planted symlink: /etc/gitconfig is read by every UID including
  # root, so it must never be redirected somewhere agent-writable.
  if [ -L "$_hwsg_path" ]; then
    rm -f -- "$_hwsg_path" 2>/dev/null || true
  fi

  {
    echo "# Managed by the hive entrypoint (kubestellar/hive#5343). Regenerated on every boot."
    echo "# System-level so EVERY agent UID reads it regardless of \$HOME. Contains no secret:"
    echo "# it names a helper path; the helper mints the per-agent scoped token."
    echo "[user]"
    echo "	name = kubestellar-hive"
    echo "	email = hive-bot@kubestellar.io"
    echo "[credential]"
    echo "	helper = "
    echo '[credential "https://github.com"]'
    echo "	helper = /usr/local/bin/git-credential-hive.sh"
    if [ -n "$_hwsg_host" ]; then
      echo "[credential \"https://${_hwsg_host}\"]"
      echo "	helper = /usr/local/bin/git-credential-hive.sh"
    fi
  } > "$_hwsg_path" 2>/dev/null || {
    echo "[entrypoint] WARN: could not write $_hwsg_path — agents may be unable to push (see kubestellar/hive#5343)"
    return 0
  }
  chmod 0644 "$_hwsg_path" 2>/dev/null || true
  if [ -n "$_hwsg_host" ]; then
    echo "[entrypoint] git credential helper wired system-wide in $_hwsg_path (github.com + GHE host ${_hwsg_host}) — readable by every agent UID"
  else
    echo "[entrypoint] git credential helper wired system-wide in $_hwsg_path (github.com) — readable by every agent UID"
  fi
}

# Detect Kubernetes vs Docker environment
IS_KUBERNETES=false
if [ -n "${KUBERNETES_SERVICE_HOST:-}" ] || [ -f /var/run/secrets/kubernetes.io/serviceaccount/token ]; then
  IS_KUBERNETES=true
fi

if [ "$IS_KUBERNETES" = "true" ]; then
  # ── Kubernetes mode: the PVC runtime config is the boot input ──────
  #
  # PHASE 2 OF THE LAYER COLLAPSE. The ConfigMap is now a SEED — consulted
  # on first boot, when the PVC has no runtime config yet — and not a
  # per-boot input that has to be merged over.
  #
  # Why this is the right way round: the ConfigMap is frozen at provision
  # time and cannot be written by anything at runtime (the hub has only
  # `get` on ConfigMaps, spoke RBAC omits them entirely, and the hub has no
  # kubectl path to the heartbeat-only cluster at all). The runtime config on the PVC is the
  # only layer any component can actually write. Treating the writable
  # layer as the input — instead of merging it over a frozen one on every
  # boot — is what removes the "which layer wins" question that cost about
  # an hour during the 2026-07-31 GHE incident, when three read-only layers
  # were patched before the writable one was found.
  #
  # Measured before the change: on 50 readable spokes all three layers held
  # identical values for every identity field. The merge was recomputing a
  # result equal to its own input on every boot.
  HIVE_CONFIG_BOOT="$(hive_runtime_config_read)"
  if [ -n "$HIVE_CONFIG_BOOT" ]; then
    # Steady state: boot from what this hive last had. The ConfigMap seed is
    # deliberately NOT merged — it is frozen at provision and every field it
    # could contribute is either already here or re-asserted by the hub on
    # the next heartbeat (~30s), including hub.is_public.
    if cp "$HIVE_CONFIG_BOOT" "$HIVE_CONFIG_PATH" 2>/dev/null; then
      echo "[entrypoint] K8s mode — booting from runtime config $HIVE_CONFIG_BOOT (ConfigMap is a seed, not merged)"
    else
      export HIVE_CONFIG="$HIVE_CONFIG_BOOT"
      echo "[entrypoint] K8s mode — config path read-only, using $HIVE_CONFIG_BOOT directly"
    fi
  elif [ -f "$HIVE_CONFIG_PATH" ] && [ -s "$HIVE_CONFIG_PATH" ]; then
    # FIRST BOOT ONLY. The copy-config init container now seeds the config path
    # solely when the PVC has no runtime config, so reaching here means this
    # hive has never written one.
    #
    # The overlay merge below is kept for one case that is real: a hive
    # REPROVISIONED onto an existing PVC has a dashboard overlay but no runtime
    # config, and dropping its overlay would silently discard the owner's agent
    # roster and level. On a genuinely new hive there is no overlay and the
    # merge is a no-op.
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
    #
    # BOTH delivery locations, not just the PVC (#4368). /secrets is the
    # provisioning mount, and the provisioning template used to seed
    # key_file=/secrets/gh-app-key.pem for every App-using hive while creating
    # that Secret entry only for hives provisioned WITH an inline private key.
    # Four hives shipped naming a file that would never exist — under the one
    # prefix this check did not look at, so the warning written for exactly
    # this symptom stayed silent on every one of them, and the fault first
    # surfaced as github_auth: fail hours later. A path outside both prefixes
    # is an operator's own location and is deliberately not second-guessed.
    key_file = overlay_gh.get('key_file') or ''
    if isinstance(key_file, str) and key_file.startswith(('/data/', '/secrets/')) and not os.path.isfile(key_file):
        where = 'the PVC' if key_file.startswith('/data/') else 'the provisioning secret mount'
        sys.stderr.write(
            "[entrypoint] WARNING: overlay github.key_file=%s does not exist on "
            "%s — GitHub App auth will fail until it is re-installed. An explicit "
            "key_file also short-circuits the fallback that would otherwise find a "
            "delivered key; leaving it unset lets the path be derived from app_id\n"
            % (key_file, where)
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
    # Write the (merged) config as the disaster-recovery snapshot. Always
    # under the new name; the legacy file is left untouched on the PVC.
    cp "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_RUNTIME" 2>/dev/null || true
    echo "[entrypoint] K8s mode — ConfigMap is the seed, runtime config written to $HIVE_CONFIG_RUNTIME"
  else
    # Neither source exists: no runtime config on the PVC (checked first,
    # above) and no ConfigMap seed. There is nothing to boot from.
    echo "[entrypoint] ERROR: no runtime config at $HIVE_CONFIG_RUNTIME (or legacy $HIVE_CONFIG_RUNTIME_LEGACY) and no ConfigMap seed at $HIVE_CONFIG_PATH."
    echo "[entrypoint] Ensure the hive ConfigMap is created before deploying."
    exit 1
  fi
else
  # ── Docker mode: the PVC runtime config is the source of truth ─────
  # Unlike K8s there is no ConfigMap and no overlay here, so this file is a
  # boot INPUT, not a snapshot: it is restored over the config path (or read
  # directly when that path is read-only) on every boot, and it is the only
  # thing that makes a dashboard save survive a container recreation.
  #
  # During the migration a hive may still have only the legacy name. We read
  # whichever exists (new preferred) and always WRITE the new one, so the
  # first boot on new code seeds hive.yaml.runtime from the legacy content
  # without touching the legacy file itself.
  HIVE_CONFIG_SOURCE="$(hive_runtime_config_read)"
  if [ -f "$HIVE_CONFIG_PATH" ] && [ -s "$HIVE_CONFIG_PATH" ] && [ -z "$HIVE_CONFIG_SOURCE" ]; then
    # First boot: config exists but no PVC runtime config yet — seed it
    cp "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_RUNTIME"
    echo "[entrypoint] First boot — config seeded to PVC: $HIVE_CONFIG_RUNTIME"
  elif [ -f "$HIVE_CONFIG_PATH" ] && [ -s "$HIVE_CONFIG_PATH" ] && [ -n "$HIVE_CONFIG_SOURCE" ]; then
    # The PVC runtime config is the source of truth (updated by Save()).
    # Try to copy it over the config path; if read-only (Docker bind mount),
    # override HIVE_CONFIG so the Go binary reads from the PVC directly.
    if cp "$HIVE_CONFIG_SOURCE" "$HIVE_CONFIG_PATH" 2>/dev/null; then
      echo "[entrypoint] PVC runtime config restored to config path (from $HIVE_CONFIG_SOURCE)"
    else
      export HIVE_CONFIG="$HIVE_CONFIG_SOURCE"
      echo "[entrypoint] Config path is read-only — using $HIVE_CONFIG_SOURCE directly"
    fi
    # Migration: if we booted from the legacy name, also seed the new name so
    # the next boot finds it. Copy, never rename — the legacy file stays put
    # as the untouched fallback until Save() takes over writing the new one.
    if [ "$HIVE_CONFIG_SOURCE" = "$HIVE_CONFIG_RUNTIME_LEGACY" ]; then
      if cp "$HIVE_CONFIG_RUNTIME_LEGACY" "$HIVE_CONFIG_RUNTIME" 2>/dev/null; then
        echo "[entrypoint] Migration — seeded $HIVE_CONFIG_RUNTIME from legacy $HIVE_CONFIG_RUNTIME_LEGACY (legacy left in place)"
      fi
    fi
  elif [ -f "$HIVE_CONFIG_PATH" ] && [ ! -s "$HIVE_CONFIG_PATH" ] && [ -n "$HIVE_CONFIG_SOURCE" ]; then
    # Config was wiped to 0 bytes (Watchtower recreation) but the PVC copy exists — restore
    cp "$HIVE_CONFIG_SOURCE" "$HIVE_CONFIG_PATH"
    echo "[entrypoint] RECOVERED: $HIVE_CONFIG_PATH was empty (0 bytes), restored from $HIVE_CONFIG_SOURCE"
  elif [ -f "$HIVE_CONFIG_PATH" ] && [ ! -s "$HIVE_CONFIG_PATH" ]; then
    # Config is empty and no PVC copy exists — fatal, cannot recover
    echo "[entrypoint] ERROR: $HIVE_CONFIG_PATH exists but is empty (0 bytes)."
    echo "[entrypoint] No runtime config found at $HIVE_CONFIG_RUNTIME (or legacy $HIVE_CONFIG_RUNTIME_LEGACY)."
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
  # ── CAP_NET_ADMIN bounding-set probe (#3760 follow-up) ──────────────────
  # Computed ONCE, here, up front — root's own bounding set does not change
  # for the rest of this block, so both consumers below (the forced-egress
  # FATAL branch and the ambient-cap-raise NOTICE branch further down) read
  # the same value instead of re-deriving it. CAP_NET_ADMIN is capability
  # number 12, so its bitmask is (1<<12) == 0x1000. (0x2000 would be bit 13 /
  # CAP_NET_RAW — which IS in the docker default bounding set, so testing
  # 0x2000 would wrongly read as "present" on a self-hosted spoke.) Read the
  # bounding set from /proc/self/status (a hex bitmask) and test the bit with
  # a pure-shell arithmetic AND (no external tools).
  _cap_net_admin_in_bset=false
  _capbnd_hex="$(grep -m1 '^CapBnd:' /proc/self/status 2>/dev/null | awk '{print $2}')"
  if [ -n "$_capbnd_hex" ]; then
    # 0x1000 == CAP_NET_ADMIN (bit 12). `$(( ))` parses the 0x-prefixed hex.
    if [ $(( 0x${_capbnd_hex} & 0x1000 )) -ne 0 ]; then
      _cap_net_admin_in_bset=true
    fi
  fi

  # Fix ownership of mounted volumes (may be root-owned from host bind mounts).
  # Skip recursive chown if /data is already owned by dev — critical for NFS
  # where recursive chown over thousands of files causes multi-minute delays.
  DATA_OWNER=$(stat -c '%u' /data 2>/dev/null || echo "0")
  if [ "$DATA_OWNER" != "1001" ]; then
    echo "[entrypoint] Fixing /data ownership (currently uid=$DATA_OWNER)..."
    chown -R dev:node /data 2>/dev/null || true
  fi
  chown dev:node /home/dev 2>/dev/null || true
  chown dev:node /etc/hive/hive.yaml 2>/dev/null || true

  # Keep the MITM CA private key out of /data's agent-writable namespace.
  # /data contains shared agent state and may be group-writable on PVCs. The
  # certificate remains at /data/proxy-ca.pem because agents need to trust it,
  # but the private key lives below an owner-only directory. If the legacy key
  # is present, discard the old CA pair so a key that may already have been
  # copied by an agent can never remain trusted. Reject symlinks so the proxy
  # cannot be redirected to an attacker-controlled path.
  mkdir -p /data/.hive && chown dev:node /data/.hive 2>/dev/null || true
  chmod 700 /data/.hive 2>/dev/null || true
  if [ -L /data/.hive/proxy-ca-key.pem ]; then
    rm -f -- /data/.hive/proxy-ca-key.pem 2>/dev/null || true
  fi
  if [ -L /data/proxy-ca-key.pem ] || [ -f /data/proxy-ca-key.pem ]; then
    # A key that was exposed in the old location may already have been copied
    # by an agent. Do not migrate it: discard the old key and certificate so
    # the proxy creates a fresh CA pair on this boot.
    rm -f -- /data/proxy-ca-key.pem /data/proxy-ca.pem /data/proxy-ca-bundle.pem 2>/dev/null || true
  fi
  chmod 600 /data/.hive/proxy-ca-key.pem 2>/dev/null || true

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
  # 0755 explicitly, not umask-dependent: gh-wrapper.sh's author gate trusts
  # the bot-identity file in this directory BECAUSE no agent UID can write here
  # (#4044). Group is "node" — which every agent is a member of — so a
  # group-writable mode would let any agent swap that file and spoof the
  # identity the gate validates against. Re-asserted on every boot.
  chmod 755 /var/run/hive-metrics/agent-tokens 2>/dev/null || true

  # Fix permissions on bind-mounted secret files (host may own them as
  # a different UID with mode 600, making them unreadable by dev/UID 1001)
  chown dev:node /secrets/*.pem 2>/dev/null || true
  # Owner-only (0600): the GitHub App private key is consumed IN-PROCESS by the
  # Go binary, ttyd, and git — all running as dev (the owner) — so 0600 keeps
  # every legitimate reader while removing group/other read. Agents run as
  # separate UIDs (2001+) and MUST NOT be able to read the org-level App key
  # (a prompt-injected agent could otherwise exfiltrate it). Was 0644.
  chmod 600 /secrets/*.pem 2>/dev/null || true
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
    cp -rn /opt/hive/seed-data/* /data/ 2>/dev/null || true
  fi

  # The seed copy above runs as root, so everything it creates is root-owned.
  # Roster agents get re-chowned to their own hive-<agent> UID in the
  # per-agent loop below, but /data/agents ITSELF and any seeded directory for
  # an agent no longer in the roster (e.g. the retired "reviewer" seed) stay
  # root-owned forever. The permissions watcher runs as dev after the
  # privilege drop, cannot chown, and would warn about them on every tick
  # (#4488). This is the one moment we are still root and the chown actually
  # succeeds, so hand root-owned agent-data entries to dev:node NOW.
  # Entries already owned by an agent UID or dev are left untouched.
  if [ -d /data/agents ]; then
    if [ "$(stat -c '%u' /data/agents 2>/dev/null || echo 0)" = "0" ]; then
      chown dev:node /data/agents 2>/dev/null || true
      chmod g+rwX /data/agents 2>/dev/null || true
    fi
    for _seeded_dir in /data/agents/*/; do
      [ -d "$_seeded_dir" ] || continue
      if [ "$(stat -c '%u' "$_seeded_dir" 2>/dev/null || echo 0)" = "0" ]; then
        chown -R dev:node "$_seeded_dir" 2>/dev/null || true
        chmod -R g+rwX "$_seeded_dir" 2>/dev/null || true
      fi
    done
  fi

  # Create beads symlinks: /home/dev/<agent>-beads -> /data/beads/<agent>
  if [ -d /etc/hive/agents ] || [ -d /data/beads ]; then
    mkdir -p /home/dev /data/beads
    if [ -d /etc/hive/agents ]; then
      for envfile in /etc/hive/agents/*.env; do
        [ -f "$envfile" ] || continue
        agent="$(basename "$envfile" .env)"
        mkdir -p "/data/beads/${agent}"
        ln -sfn "/data/beads/${agent}" "/home/dev/${agent}-beads"
        echo "[entrypoint] Beads symlink: /home/dev/${agent}-beads -> /data/beads/${agent}"
      done
    fi
    for beaddir in /data/beads/*/; do
      [ -d "$beaddir" ] || continue
      agent="$(basename "$beaddir")"
      if [ ! -L "/home/dev/${agent}-beads" ]; then
        ln -sfn "/data/beads/${agent}" "/home/dev/${agent}-beads"
        echo "[entrypoint] Beads symlink: /home/dev/${agent}-beads -> /data/beads/${agent}"
      fi
    done
    chown dev:node /home/dev 2>/dev/null || true
    if [ "$DATA_OWNER" != "1001" ]; then
      chown -R dev:node /data/beads 2>/dev/null || true
      chmod -R g+rwX /data/beads 2>/dev/null || true
    fi
    # UNCONDITIONAL group repair (fma incident, llm-d-fast-model-actuation):
    # OpenShift's restricted SCC assigns the namespace a random fsGroup
    # (e.g. 1001670000); kubelet chgrps the PVC to it WITH setgid on mount, so
    # bead dirs created afterwards inherit that foreign gid at mode 0770. The
    # hive server drops to dev (groups node + hive-launch) at privilege drop,
    # losing the fsGroup supplementary group, and then EACCESes on every store
    # at startup — the advisory digest builds empty and silently goes stale
    # while agents keep writing findings. The DATA_OWNER-gated chown above
    # never fires (fsGroup changes group, not owner), so re-group the beads
    # tree to node on every boot while we are still root. The tree is small
    # (JSON ledgers), so the recursive walk is cheap even on NFS.
    chgrp -R node /data/beads 2>/dev/null || true
    chmod -R g+rwX /data/beads 2>/dev/null || true
  fi

  # Shared CLI auth/cache lives in /data/home (persistent volume).
  # Make it group-writable so all agent UIDs (node group) can use it.
  # The manager sets HOME=/data/home for agent tmux sessions.
  mkdir -p /data/home/.config /data/home/.copilot /data/home/.claude/session-env /data/home/.codex /data/home/.bob/settings /data/config/github-copilot /home/dev/.config
  # $HOME itself must be group-writable, not just its children. bob calls
  # mkdirSync('$HOME/.bob') on first run, which needs write on /data/home — a
  # 0755 root-owned $HOME makes that EACCES even though every child dir below
  # is perfectly writable. 2775 = rwxrwxr-x + setgid (new entries inherit node).
  chmod 2775 /data/home 2>/dev/null || true
  chown dev:node /data/home 2>/dev/null || true
  # Per-agent interactive HOMEs live here (#4596): the manager provisions
  # /data/home/agents/<name> per agent at launch, bridged by symlinks back to
  # the shared dot-dirs below. 0755: every agent UID traverses it, none write
  # directly in it (each agent's own home is chowned to that agent).
  mkdir -p /data/home/agents
  chmod 0755 /data/home/agents 2>/dev/null || true
  chown dev:node /data/home/agents 2>/dev/null || true
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
  # bob writes installation_id, settings.json, trustedFolders.json and tmp/ under
  # $HOME/.bob, plus custom modes under $HOME/.bob/settings. Pre-create both
  # group-writable + setgid so the FIRST agent to launch (a 2001+ UID in group
  # node) never has to mkdir them itself — that mkdir is what failed with EACCES
  # in #2284/#2253. Pre-creating also removes the ordering dependency on which
  # process touches .bob first. 2770: no world access, these hold auth settings.
  chmod 2770 /data/home/.bob /data/home/.bob/settings 2>/dev/null || true
  chown -R dev:node /data/home/.bob 2>/dev/null || true
  ln -sfn /data/config/github-copilot /home/dev/.config/github-copilot
  ln -sfn /data/config/github-copilot /data/home/.config/github-copilot
  ln -sfn /data/home/.copilot /home/dev/.copilot
  # Set group-write + setgid on shared dirs — skip if already done (saves 100s+ on NFS).
  # The polling perm guard handles ongoing config.json fixes regardless.
  # The sampled set MUST include every dir the repair below is relied on to fix.
  # It previously sampled only .copilot and .claude, so a hive whose those two
  # were already correct skipped the whole-tree repair forever — leaving
  # /data/home itself at its original root-owned 0755 and making bob's
  # mkdir('$HOME/.bob') fail with EACCES (#2284). /data/home and .bob are sampled
  # too so that gap cannot reopen.
  NEED_PERM_FIX=false
  if [ -d "/data/home/.copilot" ] && [ -d "/data/home/.claude" ]; then
    for perm_dir in /data/home /data/home/.copilot /data/home/.claude /data/home/.bob; do
      # Missing dir → repair. Default 755 → repair (no group-write/setgid).
      DIR_PERMS=$(stat -c '%a' "$perm_dir" 2>/dev/null || echo "755")
      case "$DIR_PERMS" in
        27[0-9][0-9]|37[0-9][0-9]) ;; # already has group-write + setgid
        *) NEED_PERM_FIX=true ;;
      esac
    done
  else
    NEED_PERM_FIX=true
  fi
  if [ "$NEED_PERM_FIX" = "true" ]; then
    # Run synchronously — on fresh provisions /data/home is nearly empty so
    # this completes in <1s. Running in background caused a race where agents
    # started before perms were fixed, hitting EACCES on config.json.
    echo "[entrypoint] Fixing /data/home perms..."
    # This file runs under `set -e`. chmod on an inode root does not OWN (the
    # tree is dev-owned) needs CAP_FOWNER, and keeping the setgid bit needs
    # CAP_FSETID — both are in deployment.yaml's capabilities.add. Before they
    # were, a failed chmod here exited the container with status 1 right after
    # the line above and NO error text, crash-looping every fresh PVC. Log and
    # continue instead: agents may hit EACCES later, but the operator gets a
    # WARN naming the cause rather than a silent exit.
    chmod -R g+rwX /data/home 2>/dev/null \
      || echo "[entrypoint] WARN: chmod -R g+rwX /data/home failed — is CAP_FOWNER in the pod's capabilities.add? Continuing; agents may hit EACCES under /data/home"
    find /data/home -type d -exec chmod g+s {} + 2>/dev/null \
      || echo "[entrypoint] WARN: chmod g+s on /data/home dirs failed — is CAP_FOWNER/CAP_FSETID in the pod's capabilities.add? Continuing"
    if [ "$DATA_OWNER" != "1001" ]; then
      chown -R dev:node /data/config /data/home 2>/dev/null \
        || echo "[entrypoint] WARN: chown -R dev:node /data/config /data/home failed — is CAP_CHOWN in the pod's capabilities.add? Continuing"
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
        chmod -R g+rwX /data/home/.cache /data/home/.copilot /data/home/.claude /data/home/.codex /data/home/.bob 2>/dev/null
        find /data/home/.cache /data/home/.claude /data/home/.codex /data/home/.bob -type d -exec chmod g+s {} + 2>/dev/null
        CYCLE=0
      fi
      sleep 5
    done
  ) &
  echo "[entrypoint] polling perm guard active (config.json 5s, cache 5m)"
  echo "[entrypoint] CLI config: /data/home (shared, group-writable for agent UIDs)"

  # Write .bashrc for agent shells. GH_TOKEN is NOT exported here — gh-wrapper.sh
  # injects the token at call time, avoiding exposure in shell env / transcripts.
  # Point agent shells (git/curl, which also go through the MITM proxy) at the
  # COMBINED bundle (public roots + proxy CA) the entrypoint builds before the Go
  # launch. Falling back to the proxy CA alone if the combined bundle isn't there
  # yet keeps proxied HTTPS working; the combined bundle is strictly safer because
  # it also trusts real public roots for any non-proxied endpoint (advisory mode).
  cat > /data/home/.bashrc <<'BASHRC' 2>/dev/null || true
# Hive agent shell environment
if [ -f /data/proxy-ca-bundle.pem ]; then
  export SSL_CERT_FILE=/data/proxy-ca-bundle.pem
else
  export SSL_CERT_FILE=/data/proxy-ca.pem
fi
# The image ships Go at /usr/local/go/bin but /etc/profile resets PATH to the
# Debian default, so agent shells lose it — and agents then report "no Go
# toolchain" when asked to run tests.
case ":$PATH:" in *:/usr/local/go/bin:*) ;; *) export PATH="$PATH:/usr/local/go/bin" ;; esac
BASHRC
  chmod 644 /data/home/.bashrc 2>/dev/null || true
  # Login shells (tmux default-command) read ~/.profile, not ~/.bashrc — chain
  # them so both shell flavors get the same environment.
  if [ ! -f /data/home/.profile ]; then
    printf '[ -f "$HOME/.bashrc" ] && . "$HOME/.bashrc"\n' > /data/home/.profile 2>/dev/null || true
    chmod 644 /data/home/.profile 2>/dev/null || true
  fi

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
import yaml, sys, os
names = set()
with open('$HIVE_CONFIG') as f:
    cfg = yaml.safe_load(f) or {}
agents = cfg.get('agents', {})
if isinstance(agents, dict):
    names.update(agents.keys())
elif isinstance(agents, list):
    for a in agents:
        if isinstance(a, dict) and 'name' in a:
            names.add(a['name'])
# Also check pack YAML if HIVE_LEVEL is set
level = os.environ.get('HIVE_LEVEL', '')
if level:
    import glob
    for p in glob.glob('/opt/hive/packs/level-*.yaml') + glob.glob('/data/packs/level-*.yaml'):
        try:
            with open(p) as pf:
                pack = yaml.safe_load(pf) or {}
            pack_agents = pack.get('agents', [])
            if isinstance(pack_agents, list):
                for a in pack_agents:
                    if isinstance(a, dict) and 'name' in a:
                        names.add(a['name'])
            elif isinstance(pack_agents, dict):
                names.update(pack_agents.keys())
        except Exception:
            pass
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
    UID_OFFSET=0
    UID_JSON='{"agents":{'
    FIRST=true
    echo "$AGENT_NAMES" | while read -r agent_name; do
      [ -z "$agent_name" ] && continue
      AGENT_UID=$((HIVE_UID_BASE + UID_OFFSET))
      if ! id "hive-${agent_name}" >/dev/null 2>&1; then
        useradd --system -u "$AGENT_UID" -g node -d /data/home -M -s /bin/bash "hive-${agent_name}" 2>/dev/null || true
      fi
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
      if [ -d "/data/beads/${agent_name}" ]; then
        BEAD_DIR_OWNER=$(stat -c '%u' "/data/beads/${agent_name}" 2>/dev/null || echo "0")
        if [ "$BEAD_DIR_OWNER" != "$AGENT_UID" ]; then
          chown -R "hive-${agent_name}:node" "/data/beads/${agent_name}" 2>/dev/null || true
        fi
      fi
      # ── Per-agent GitHub App token cache file ────────────────────────
      # The hive binary runs as dev (UID 1001) after privilege drop and CANNOT
      # chown — an unprivileged process cannot give a file away to another UID
      # without CAP_CHOWN, and capabilities are deliberately restricted here
      # (the same container already fails NET_ADMIN for iptables above). So the
      # runtime write path must never call chown. We pre-create the cache file
      # HERE, as root, with the final ownership and mode:
      #
      #   owner = dev  (UID 1001)      -> hive can rewrite it in place, no chown
      #   group = hive-<agent>         -> ONLY this agent can read it
      #   mode  = 0640                 -> owner rw, group r, world none
      #
      # The group MUST be a per-agent private group. The agent users are all in
      # the shared "node" group (gid 1000), so group-node would let EVERY agent
      # read EVERY other agent's scoped token — exactly the cross-agent leak the
      # per-agent token exists to prevent. groupadd + usermod -aG gives each
      # agent a private group it is the only member of, while leaving "node" as
      # its primary group so nothing else about the agent's access changes.
      #
      # dev deliberately does NOT join these groups: it writes as the file
      # OWNER, so it needs no group membership. That keeps dev out of every
      # agent's private group and avoids N usermod calls.
      #
      # gids are auto-allocated by `groupadd --system` rather than derived from
      # the agent UID. A derived gid (e.g. uid+1000) can collide with an
      # existing group; letting groupadd pick from the system range cannot.
      #
      # Verified in ghcr.io/kubestellar/hive:v2-latest: dev writes in place OK,
      # the owning agent reads its own token OK, and a second agent gets
      # EACCES on the first agent's token.
      AGENT_TOKEN_GROUP="hive-${agent_name}"
      if ! getent group "$AGENT_TOKEN_GROUP" >/dev/null 2>&1; then
        groupadd --system "$AGENT_TOKEN_GROUP" 2>/dev/null || true
      fi
      usermod -aG "$AGENT_TOKEN_GROUP" "hive-${agent_name}" 2>/dev/null || true
      AGENT_TOKEN_FILE="/var/run/hive-metrics/agent-tokens/gh-token-${agent_name}.cache"
      # Create empty (never seeded with a value) — hive fills it at launch.
      # touch is idempotent across restarts; chown/chmod re-assert the contract
      # unconditionally in case a previous image left the file mis-owned.
      touch "$AGENT_TOKEN_FILE" 2>/dev/null || true
      chown "dev:${AGENT_TOKEN_GROUP}" "$AGENT_TOKEN_FILE" 2>/dev/null || true
      chmod 640 "$AGENT_TOKEN_FILE" 2>/dev/null || true
      echo "[entrypoint] Agent token cache: ${AGENT_TOKEN_FILE} (dev:${AGENT_TOKEN_GROUP} 0640)"
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

    # Set up iptables: redirect all outbound :443 to the MITM proxy port,
    # except traffic from the proxy itself (UID 1001 / dev user).
    #
    # ── Fail-closed egress enforcement (security-critical: audit F5, CWE-693) ──
    # The ACMM capability model's binding control is FORCED egress: agents hold
    # tier-scoped tokens, but the *forced* redirect of outbound :443 to the MITM
    # proxy is what actually confines a raw token to the proxy's policy. If that
    # redirect is not established (iptables missing, or NET_ADMIN unavailable),
    # an agent holding a raw token can reach api.github.com directly and the
    # entire capability model degrades to advisory-only — a fail-OPEN control.
    #
    # Therefore: a failure to establish the redirect is FATAL by default. The
    # container refuses to start rather than run with unenforced egress. The
    # ONLY escape hatch is an EXPLICIT operator opt-in via
    # HIVE_PROXY_ADVISORY_OK=true, for deliberate advisory deployments (local
    # dev, platforms without NET_ADMIN). We never silently continue.
    PROXY_PORT=18443
    PROXY_ADVISORY_OK="${HIVE_PROXY_ADVISORY_OK:-false}"
    _iptables_ok=false

    # Select the iptables binary. Both OKE and OpenShift/RHEL9 hosts run the
    # kernel in nft mode, where the legacy `iptables` (xtables-legacy) backend
    # cannot write the nat table the node actually consults. Prefer the explicit
    # nft binary; fall back to plain `iptables` (which on these images is a
    # symlink to xtables-nft-multi anyway, so it resolves to nft).
    IPT=""
    if command -v iptables-nft >/dev/null 2>&1; then
      IPT="iptables-nft"
    elif command -v iptables >/dev/null 2>&1; then
      IPT="iptables"
    fi

    if [ -n "$IPT" ]; then
      # Self-exemption uses BOTH owner-UID and packet-mark RETURNs, because the
      # two platforms we run on each support a DIFFERENT one, and a single
      # mechanism is not enough for both:
      #
      #   - OKE: the `-m owner --uid-owner` match works (xt_owner is present) and
      #     is the RELIABLE exemption. The proxy's own upstream :443 dials matched
      #     by UID are RETURNed before the redirect.
      #   - OpenShift/OVN: xt_owner is ABSENT, so the `-m owner` `-A` lines FAIL to
      #     load — but the PACKET MARK (SO_MARK / HIVE_PROXY_EGRESS_MARK, stamped
      #     by the Go proxy's markDialer, exported above) works there and exempts
      #     the proxy's traffic.
      #
      # REGRESSION HISTORY: PR #2678 replaced the working owner-UID exemption with
      # a MARK-ONLY exemption. That was verified only on OpenShift / the heartbeat-only cluster. On the
      # LIVE OKE console hive the SO_MARK did NOT reliably stick on the proxy's
      # outbound sockets, so its OWN :443 to api.github.com hit the REDIRECT rule,
      # looped back into itself (:18443) → EPERM/ECONNREFUSED/EOF → no GitHub App
      # token minting → no repo enumeration → the whole L6 loop went dead
      # (refs #2678, #2674). Restoring the owner-UID RETURNs immediately fixed OKE.
      #
      # FIX: keep ALL THREE exemptions, in order, BEFORE the redirect. Each
      # platform is covered by whichever mechanism works there; neither breaks.
      # The owner `-A` lines are appended DEFENSIVELY with `|| true` so that on a
      # host WITHOUT xt_owner their failure does NOT abort the ruleset or flip the
      # F5 fatal `_iptables_ok` flag — the mark exemption + redirect still
      # establish, and `_iptables_ok=true` is set as long as the chain is built.
      # Chain creation retries with captured stderr and lock-wait. One-shot
      # creation with silenced stderr turned a TRANSIENT netlink/xtables
      # failure into a fail-closed FATAL crash-loop: on 2026-08-13 three
      # spokes sharing one node rolled simultaneously at the daily upgrade
      # window and kept re-colliding in synchronized backoff for three
      # hours (a spoke cluster). Jittered retries break the lockstep; the real
      # iptables error is logged instead of discarded.
      _ipt_chain_ok=false
      _ipt_try=0
      while [ "$_ipt_try" -lt 5 ]; do
        _ipt_try=$((_ipt_try + 1))
        # A leftover chain from a prior partial attempt counts as created —
        # flush it so rule appends below start from a clean slate.
        if $IPT -t nat -nL HIVE_PROXY >/dev/null 2>&1; then
          $IPT -t nat -F HIVE_PROXY 2>/dev/null || true
          _ipt_chain_ok=true
          break
        fi
        if $IPT -w 10 -t nat -N HIVE_PROXY 2>/tmp/hive-ipt-err.log; then
          _ipt_chain_ok=true
          break
        fi
        echo "[entrypoint] WARN: iptables chain creation attempt ${_ipt_try}/5 failed: $(cat /tmp/hive-ipt-err.log 2>/dev/null) — retrying"
        sleep $(( _ipt_try * 2 + $$ % 3 ))
      done
      if [ "$_ipt_chain_ok" = "true" ]; then
        # OKE: owner-match exemption (reliable where xt_owner is present).
        # `|| true` keeps a failed append non-fatal on OpenShift (no xt_owner).
        $IPT -t nat -A HIVE_PROXY -m owner --uid-owner 0 -j RETURN || true
        $IPT -t nat -A HIVE_PROXY -m owner --uid-owner "$PROXY_UID" -j RETURN || true
        # OpenShift/OVN: packet-mark exemption (works with no xt_owner).
        $IPT -t nat -A HIVE_PROXY -m mark --mark "$HIVE_PROXY_EGRESS_MARK" -j RETURN
        $IPT -t nat -A HIVE_PROXY -p tcp --dport 443 -j REDIRECT --to-ports "$PROXY_PORT"
        $IPT -t nat -A OUTPUT -j HIVE_PROXY
        echo "[entrypoint] iptables ($IPT): outbound :443 -> :${PROXY_PORT} (proxy UID ${PROXY_UID} + egress mark ${HIVE_PROXY_EGRESS_MARK} exempt)"
        _iptables_ok=true
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
        echo "[entrypoint] ERROR: iptables chain creation failed after ${_ipt_try} attempts: $(cat /tmp/hive-ipt-err.log 2>/dev/null)"
      fi
    else
      echo "[entrypoint] ERROR: iptables not found — cannot force proxy egress"
    fi

    # ── IPv6 egress gate (#4319) ────────────────────────────────────────────
    # The redirect above exists ONLY in the IPv4 nat table. On any network that
    # gives the container a global IPv6 address, an agent that resolves an AAAA
    # record reaches :443 over IPv6 and never meets it — a SILENT bypass of the
    # capability model (no ADVISORY-ONLY warning fires, because the IPv4 gate
    # installed successfully). The proxy listens on 127.0.0.1 only
    # (proxy.GitHubProxy listenAddr), so mirroring the REDIRECT with ip6tables
    # could not deliver the traffic anywhere; the IPv6 family is instead CLOSED
    # with a filter-table REJECT carrying the SAME three exemptions — xt_owner
    # and xt_mark are family-agnostic, and SO_MARK (stamped by the Go proxy's
    # markDialer) marks IPv6 packets identically, so the proxy's own upstream
    # dials stay exempt on IPv6-capable networks.
    #
    # REJECT --reject-with tcp-reset rather than DROP: dual-stack clients get an
    # immediate RST and fall back to IPv4 (Happy Eyeballs), landing in the IPv4
    # redirect above, instead of hanging through a timeout on every dial.
    #
    # sysctl net.ipv6.conf.all.disable_ipv6=1 was REJECTED as the mechanism:
    # the image depends on IPv6 loopback internally (the gateway nginx binds
    # `listen [::]:PORT` for ::1 clients — gateway_nginx_dualstack_test.go —
    # and the proxy's self-lookup reads /proc/net/tcp6), and /proc/sys is
    # read-only in most container runtimes anyway.
    #
    # A kernel with no IPv6 stack at all (/proc/sys/net/ipv6 absent) has no
    # IPv6 route to gate; that case passes vacuously rather than failing a
    # container that cannot carry the traffic in the first place.
    #
    # The SAME reasoning applies one step further in, and the stack-presence
    # check alone does not catch it (#4327 follow-up): a pod can have the IPv6
    # stack compiled in — /proc/sys/net/ipv6 present, ::1 up for the gateway
    # nginx and the proxy's /proc/net/tcp6 self-lookup — while having NO global
    # IPv6 address and NO IPv6 default route. Such a pod cannot originate IPv6
    # egress at all, so there is no bypass to close. Fail-closing there took
    # down a healthy hive on an IPv4-only OpenShift cluster whose nodes also
    # lack the ip6tables `owner`/`REJECT` extensions, so the gate could not be
    # installed even though nothing could ever traverse it.
    #
    # Routability, not stack presence, is the property that decides whether an
    # IPv6 bypass is reachable — so that is what is tested. A pod that later
    # gains a global address gets the gate on its next start, and any pod that
    # HAS IPv6 egress still fails closed exactly as before.
    _ip6tables_ok=false
    _ip6_routable=false
    if [ -d /proc/sys/net/ipv6 ]; then
      # A global-scope address AND a default route are both required to send
      # IPv6 off-host. Prefer `ip`; fall back to /proc for minimal images.
      if command -v ip >/dev/null 2>&1; then
        if ip -6 addr show scope global 2>/dev/null | grep -q 'inet6' \
           && ip -6 route show default 2>/dev/null | grep -q .; then
          _ip6_routable=true
        fi
      elif [ -r /proc/net/if_inet6 ] && [ -r /proc/net/ipv6_route ]; then
        # if_inet6 scope 0x00 == global; ipv6_route holds a ::/0 default when
        # the destination prefix length field (col 2) is 00.
        if awk '$4 == "00" { found=1 } END { exit !found }' /proc/net/if_inet6 2>/dev/null \
           && awk '$2 == "00" { found=1 } END { exit !found }' /proc/net/ipv6_route 2>/dev/null; then
          _ip6_routable=true
        fi
      else
        # Neither `ip` nor the /proc files are readable, so routability cannot
        # be determined. Assume routable and let the gate decide: an
        # indeterminate probe must not be the thing that disables enforcement.
        _ip6_routable=true
      fi
    fi
    if [ ! -d /proc/sys/net/ipv6 ]; then
      echo "[entrypoint] IPv6 stack absent from kernel — no IPv6 egress to gate"
      _ip6tables_ok=true
    elif [ "$_ip6_routable" != "true" ]; then
      echo "[entrypoint] IPv6 present but not routable (no global address and/or no default route) — no IPv6 egress to gate"
      _ip6tables_ok=true
    else
      # Same binary-selection rationale as IPT above: prefer the explicit nft
      # frontend on nft-mode kernels (OKE, OpenShift/RHEL9).
      IP6T=""
      if command -v ip6tables-nft >/dev/null 2>&1; then
        IP6T="ip6tables-nft"
      elif command -v ip6tables >/dev/null 2>&1; then
        IP6T="ip6tables"
      fi
      if [ -n "$IP6T" ]; then
        # Same jittered chain-creation retry as the IPv4 gate: one-shot
        # creation turns transient netlink/xtables contention into a
        # fail-closed crash-loop (see the 2026-08-13 note above).
        _ip6_chain_ok=false
        _ip6_try=0
        while [ "$_ip6_try" -lt 5 ]; do
          _ip6_try=$((_ip6_try + 1))
          if $IP6T -nL HIVE_PROXY6 >/dev/null 2>&1; then
            $IP6T -F HIVE_PROXY6 2>/dev/null || true
            _ip6_chain_ok=true
            break
          fi
          if $IP6T -w 10 -N HIVE_PROXY6 2>/tmp/hive-ip6t-err.log; then
            _ip6_chain_ok=true
            break
          fi
          echo "[entrypoint] WARN: ip6tables chain creation attempt ${_ip6_try}/5 failed: $(cat /tmp/hive-ip6t-err.log 2>/dev/null) — retrying"
          sleep $(( _ip6_try * 2 + $$ % 3 ))
        done
        if [ "$_ip6_chain_ok" = "true" ]; then
          # Exemptions mirror the IPv4 chain exactly, in the same order, for
          # the same two-platform reasons (owner-UID where xt_owner exists,
          # packet mark where it does not). `|| true` on the owner lines keeps
          # their failure non-fatal on hosts without xt_owner.
          $IP6T -A HIVE_PROXY6 -m owner --uid-owner 0 -j RETURN || true
          $IP6T -A HIVE_PROXY6 -m owner --uid-owner "$PROXY_UID" -j RETURN || true
          $IP6T -A HIVE_PROXY6 -m mark --mark "$HIVE_PROXY_EGRESS_MARK" -j RETURN
          $IP6T -A HIVE_PROXY6 -p tcp --dport 443 -j REJECT --reject-with tcp-reset
          $IP6T -A OUTPUT -j HIVE_PROXY6
          echo "[entrypoint] ip6tables ($IP6T): outbound IPv6 :443 REJECTed (proxy has no IPv6 listener; proxy UID ${PROXY_UID} + egress mark ${HIVE_PROXY_EGRESS_MARK} exempt)"
          _ip6tables_ok=true
        else
          echo "[entrypoint] ERROR: ip6tables chain creation failed after ${_ip6_try} attempts: $(cat /tmp/hive-ip6t-err.log 2>/dev/null)"
        fi
      else
        echo "[entrypoint] ERROR: ip6tables not found — IPv6 egress cannot be gated"
      fi
    fi

    if [ "$_iptables_ok" != "true" ] || [ "$_ip6tables_ok" != "true" ]; then
      if [ "$PROXY_ADVISORY_OK" = "true" ]; then
        echo "[entrypoint] WARN: proxy egress enforcement is ADVISORY-ONLY (HIVE_PROXY_ADVISORY_OK=true set). Agents can bypass the MITM proxy — capability model is NOT enforced."
      else
        echo "[entrypoint] FATAL: could not establish forced proxy egress (IPv4 redirect established: ${_iptables_ok}; IPv6 gate established: ${_ip6tables_ok}). The ACMM capability model would be advisory-only for the failed family, allowing agents with raw tokens to bypass the MITM proxy." >&2
        echo "[entrypoint] FATAL: refusing to start. Grant NET_ADMIN + install iptables/ip6tables, or set HIVE_PROXY_ADVISORY_OK=true to deliberately run in advisory mode." >&2
        # Distinguish root cause by exit code (#3760 follow-up): netfilter
        # chain manipulation itself requires CAP_NET_ADMIN, so if the
        # bounding set lacks it, that absence is BY ITSELF sufficient to
        # explain the failure above — this IS the missing-capability case,
        # not merely "iptables had some other problem". See
        # EXIT_NET_ADMIN_REQUIRED's definition near the top of this file.
        if [ "$_cap_net_admin_in_bset" != "true" ]; then
          echo "[entrypoint] FATAL: CAP_NET_ADMIN is not in the container's capability bounding set — exiting ${EXIT_NET_ADMIN_REQUIRED} (EX_NOPERM) rather than 1." >&2
          exit "$EXIT_NET_ADMIN_REQUIRED"
        fi
        exit 1
      fi
    fi
  fi

  # Drop to non-root user for all runtime processes.
  # Claude Code refuses --dangerously-skip-permissions as root.
  #
  # ── NET_ADMIN for the proxy SO_MARK egress-gate, WITHOUT a file cap (#3760) ──
  # The hive process (proxy) needs CAP_NET_ADMIN in its EFFECTIVE set to
  # setsockopt(SO_MARK) on its own upstream dials so the forced-egress REDIRECT
  # exempts them (the ONLY exemption that works on OpenShift/OVN, where xt_owner
  # is absent — refs #2674/#2678). We used to grant this as a FILE capability on
  # /usr/local/bin/hive, but the kernel refuses execve of a file-capped binary
  # whenever the runtime's BOUNDING set lacks that cap (default docker/podman/
  # containerd, i.e. every self-hosted k3s/rootless spoke) → bare EPERM at exec,
  # the crash-loop in #3760. The binary now ships with NO file capability; we
  # instead raise NET_ADMIN as an AMBIENT capability HERE, gated on the bounding
  # set actually having it — so managed spokes (SCC/PSA grant NET_ADMIN) get the
  # cap and self-hosted spokes exec cleanly and degrade the SO_MARK path.
  #
  # $_cap_net_admin_in_bset was already computed at the top of this "if root"
  # block (before the forced-egress FATAL check above) — reused here rather
  # than re-derived, since it can't have changed and the FATAL branch's exit
  # code already depends on the two staying the same value.

  # ── System-wide git credential helper (#5343) ───────────────────────────
  # MUST happen here, in the root phase: /etc is root-owned, and the dev phase
  # below cannot write it. This is the ONLY git config any per-agent UID reads
  # — their $HOME (/data/home/agents/<name>) has no .gitconfig of its own.
  hive_write_system_gitconfig

  # setpriv identity mirrors `gosu dev` exactly: reuid=dev (UID 1001), regid=node
  # (dev's PRIMARY login group, GID 1000 — there is NO group named `dev`), and
  # --init-groups to populate the supplementary groups from the user db for dev
  # (crucially hive-launch, so dev can still exec the 4750 su-exec helper). A
  # regid of a nonexistent `dev` group would make setpriv fail and silently drop
  # us to the gosu fallback (losing the ambient cap on managed spokes).
  _SETPRIV_ID="--reuid dev --regid node --init-groups"

  # ── The ambient set CANNOT be raised without the INHERITABLE set (#3874) ──
  # An ambient capability is only permitted to hold a bit that is in BOTH the
  # permitted and the inheritable set (kernel: cap_ambient_raise → PR_CAP_AMBIENT
  # requires the cap in pP *and* pI, capability.c). setpriv applies --ambient-caps
  # AFTER the UID change, at which point a setuid transition has already zeroed
  # pI. So `--ambient-caps +net_admin` ALONE is a silent no-op: setpriv exits 0,
  # the entrypoint prints its success line, and the process lands with
  # CapAmb=0x0 — exactly the signature seen fleet-wide (CapBnd=...a80435fb with
  # CapInh/CapPrm/CapEff/CapAmb all zero). The proxy then cannot setsockopt
  # (SO_MARK) its own upstream dial, that dial is REDIRECTed back into the proxy
  # itself, and every outbound :443 hangs until timeout while inbound is fine.
  #
  # Raising --inh-caps in the SAME setpriv call fixes it: pI keeps NET_ADMIN
  # across the credential change, so the ambient raise is legal and sticks
  # (verified live: CapInh/CapPrm/CapEff/CapAmb all become 0x1000).
  #
  # This grants net_admin and NOTHING else — no file capability, no SUID, and the
  # bounding set is still the gate above, so self-hosted spokes are unaffected.
  _SETPRIV_CAPS="--inh-caps +net_admin --ambient-caps +net_admin"

  # Probe the full invocation once (as root) so a bad flag/identity falls through
  # to the gosu fallback instead of exec-failing after the point of no return.
  #
  # The probe VERIFIES THE OUTCOME rather than trusting the exit status: it reads
  # CapAmb back out of /proc/self/status in the dropped child and requires bit 12
  # to actually be set. A setpriv that "succeeds" while producing CapAmb=0x0 (the
  # bug above, and any future kernel/util-linux regression of the same shape) is
  # therefore treated as a FAILURE and falls through to the honest gosu path,
  # instead of silently claiming an egress exemption the proxy does not have.
  _setpriv_grants_ambient_net_admin() {
    _probe_amb="$(setpriv $_SETPRIV_CAPS $_SETPRIV_ID \
      sh -c 'grep -m1 "^CapAmb:" /proc/self/status' 2>/dev/null | awk '{print $2}')"
    [ -n "$_probe_amb" ] && [ $(( 0x${_probe_amb} & 0x1000 )) -ne 0 ]
  }

  if [ "$_cap_net_admin_in_bset" = "true" ] \
     && command -v setpriv >/dev/null 2>&1 \
     && _setpriv_grants_ambient_net_admin; then
    # Managed spoke: bounding set HAS NET_ADMIN and setpriv can raise it into the
    # ambient set. Drop to dev WITH ambient+effective NET_ADMIN so the Go hive
    # process can SO_MARK its proxy dials.
    echo "[entrypoint] Dropping to dev user (ambient CAP_NET_ADMIN granted for proxy SO_MARK egress-gate)"
    exec setpriv $_SETPRIV_CAPS $_SETPRIV_ID "$0" "$@"
  elif command -v gosu >/dev/null 2>&1 && gosu dev true 2>/dev/null; then
    if [ "$_cap_net_admin_in_bset" != "true" ]; then
      # Self-hosted / rootless spoke: no NET_ADMIN in the bounding set. Do NOT
      # attempt the ambient raise (setpriv would error). The binary execs fine
      # (no file cap), but the proxy's SO_MARK egress exemption is unavailable.
      # The only remaining self-exemption is the owner-UID RETURN appended above
      # — and that one is CONDITIONAL: it is appended with `|| true` and silently
      # does not exist on kernels without the xt_owner module (OpenShift/OVN and
      # some IKS/OKE node images: "Extension owner revision 0 not supported").
      # So do NOT promise it here. Report what is actually in the chain, which is
      # verifiable, instead of asserting a backstop that may not be loaded.
      # The Go proxy logs the unmarked dial once
      # (src/pkg/proxy/github_proxy.go warnSockMarkOnce).
      if iptables -t nat -C HIVE_PROXY -m owner --uid-owner "$PROXY_UID" -j RETURN 2>/dev/null; then
        _owner_backstop="present (xt_owner loaded) — proxy dials still exempt via owner-UID"
      else
        _owner_backstop="ABSENT (no xt_owner on this kernel) — the proxy has NO self-exemption; outbound :443 from the proxy will loop back into the redirect"
      fi
      echo "[entrypoint] NOTICE: CAP_NET_ADMIN is not in the bounding set — the forced-proxy egress exemption (SO_MARK) is unavailable. Owner-UID backstop: ${_owner_backstop}. Grant NET_ADMIN (securityContext capabilities.add / --cap-add NET_ADMIN) for full egress attribution."
    else
      # Bounding set HAD NET_ADMIN but setpriv was missing, or the probe proved
      # the ambient bit did NOT survive the drop (the #3874 silent no-op). Fall
      # back to gosu with an honest warning: this path has no SO_MARK exemption.
      echo "[entrypoint] WARN: setpriv unavailable, or the ambient CAP_NET_ADMIN raise did not survive the privilege drop (verified CapAmb bit 12 unset), despite NET_ADMIN in the bounding set — falling back to gosu (SO_MARK egress exemption unavailable)."
    fi
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

# Configure git identity and credential helper for GitHub App token.
#
# TWO LAYERS, DELIBERATELY (#5343):
#
#  1. /etc/gitconfig (SYSTEM) — written in the root phase above by
#     hive_write_system_gitconfig. This is the layer that matters for AGENTS:
#     every per-agent UID runs with its own $HOME (/data/home/agents/<name>)
#     which has no .gitconfig, so the system file is the ONLY config they read.
#
#  2. ~dev/.gitconfig (GLOBAL, this block) — the dev user's own config, which
#     the contributor-relay / local-mode / `just contribute-*` paths and any
#     interactive `docker exec` shell have always used. Kept because those
#     paths are not agent-UID paths, and because a hive that could not become
#     root (the "continuing as root" / already-non-root boot) never reaches
#     layer 1 at all — this block is then the only wiring there is.
#
# These do not fight: git precedence is system < global < local, and both
# layers set the SAME helper for the SAME hosts, so the global layer shadows
# nothing. What went wrong before was having ONLY layer 2.
git config --global user.name "kubestellar-hive"
git config --global user.email "hive-bot@kubestellar.io"
git config --global --replace-all credential.helper ""
git config --global --replace-all "credential.https://github.com.helper" "/usr/local/bin/git-credential-hive.sh"

# Also wire the credential helper for this hive's ACTUAL GitHub host when it
# is NOT public github.com (a GitHub Enterprise instance, e.g. github.ibm.com).
#
# Without this, a hive whose repos/App live on GHE only ever gets the helper
# registered for credential.https://github.com.helper (above). git matches
# credential helpers by exact host, so `git clone https://github.ibm.com/...`
# never invokes ANY helper for that host, falls through to an interactive
# prompt, and fails in this non-interactive shell with "fatal: could not read
# Username for 'https://github.ibm.com': terminal prompts disabled" — this was
# reported live from a GHE hive (devx-prod/epx-vscode-ext-poc) where scanner
# and quality (which talk to the GitHub API, not git-over-HTTPS) worked fine
# while guide's `git clone` could not authenticate at all.
#
# The host derivation lives in hive_ghe_git_host() at the top of this file so
# the system and global layers cannot drift apart.
GHE_GIT_HOST="$(hive_ghe_git_host)"
if [ -n "$GHE_GIT_HOST" ]; then
  git config --global --replace-all "credential.https://${GHE_GIT_HOST}.helper" "/usr/local/bin/git-credential-hive.sh"
  echo "[entrypoint] git credential helper wired for GitHub Enterprise host: ${GHE_GIT_HOST}"
fi

# ── Startup assertion: is the helper actually reachable from an AGENT UID? ──
#
# The whole point of #5343 is that the wiring LOOKED right (it was present in
# ~dev/.gitconfig) while being invisible to every agent. So assert the property
# that actually matters — "a process whose $HOME is not /home/dev resolves the
# helper" — rather than "we ran git config successfully".
#
# HOME=/nonexistent is the cheapest faithful stand-in for an agent UID here:
# it removes the global layer exactly the way an agent's own empty $HOME does,
# leaving only the system layer under test. GIT_CONFIG_NOSYSTEM is explicitly
# NOT set — the system layer is the thing being verified.
_cred_probe_host="${GHE_GIT_HOST:-github.com}"
_cred_probe="$(HOME=/nonexistent XDG_CONFIG_HOME=/nonexistent \
  git config --get-regexp "^credential\." 2>/dev/null | grep -c "git-credential-hive.sh" || true)"
if [ "${_cred_probe:-0}" -gt 0 ]; then
  echo "[entrypoint] git credential helper VERIFIED reachable without a per-user .gitconfig (system layer, ${_cred_probe} host entries; agent UIDs will resolve it for ${_cred_probe_host})"
else
  echo "[entrypoint] WARN: git credential helper is NOT reachable from a process without a per-user .gitconfig. Every per-agent UID will commit branches it cannot push, and hive-open-pr will report the branch as missing from the remote. Check that /etc/gitconfig exists and is mode 0644. See kubestellar/hive#5343."
fi

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

# ── Proxy CA trust for the MAIN Go hive process (race-free) ──────────────
#
# CHICKEN-AND-EGG (fixed in the Go binary; this block is now belt-and-suspenders):
# /data/proxy-ca.pem is GENERATED BY the Go proxy at runtime
# (proxy.NewGitHubProxy → loadOrGenerateCA). On a fresh-PVC boot it does NOT
# exist when the entrypoint reaches the `[ -f /data/proxy-ca.pem ]` gate below,
# so SSL_CERT_FILE was never exported and the Go process launched trusting only
# the (still-installing) system store → a multi-minute post-roll window where
# every token mint failed "x509: certificate signed by unknown authority".
# The Go binary now establishes its OWN proxy-CA trust: the token-mint HTTP
# client (pkg/github/proxytrust.go) carries a RootCAs pool = system roots +
# /data/proxy-ca.pem, LAZILY reloaded from disk, so the first mint after the
# proxy writes the CA trusts it immediately, with no dependency on this export.
# The block below is kept because it still helps Node/sub-processes and any Go
# restart, and it is now correct whether or not the CA exists yet at this point.
#
# The Go hive process makes GitHub App token calls
# (POST /app/installations/{id}/access_tokens). With F5 forced egress enabled
# (the iptables REDIRECT of all outbound :443 to the MITM proxy, set up above),
# those calls hit the proxy, which presents a cert forged by /data/proxy-ca.pem.
# Go's crypto/x509 must trust that CA or every token mint fails with
# "x509: certificate signed by unknown authority" → "all N repos failed" → the
# agent loop is dead (verified live on the console hive).
#
# We CANNOT rely on the system-trust install below (the gosu/update-ca-certificates
# block after the launch) for the Go process, for two reasons:
#   1. RACE: the Go process starts here, BEFORE that install runs, and Go caches
#      the system cert pool at first TLS handshake. A boot-time gosu failure
#      ("WARN: proxy CA install via gosu failed", seen live) then leaves the
#      already-running Go process permanently unable to verify the proxy cert —
#      the later CA-watch re-install loop cannot fix a pool Go already cached.
#   2. Go honors SSL_CERT_FILE at handshake time (NOT cached like the system
#      bundle), so pointing it at a file we control is robust and race-free.
#
# We point SSL_CERT_FILE at a COMBINED bundle = public roots + proxy CA, NOT the
# proxy CA alone. Rationale: SSL_CERT_FILE REPLACES the trust set, so proxy-CA-only
# would make Go trust ONLY the proxy CA and break any HTTPS that does NOT traverse
# the MITM proxy — which happens in advisory mode (HIVE_PROXY_ADVISORY_OK=true, no
# iptables redirect: the Go process then reaches api.github.com / a GHE host
# directly with a REAL public cert). The combined bundle trusts BOTH the real
# public roots AND the proxy CA, so it is correct whether or not egress is forced.
SSL_CERT_BUNDLE="/data/proxy-ca-bundle.pem"
SYSTEM_CA_BUNDLE="/etc/ssl/certs/ca-certificates.crt"
if [ -f /data/proxy-ca.pem ] && [ -f "$SYSTEM_CA_BUNDLE" ]; then
  if cat "$SYSTEM_CA_BUNDLE" /data/proxy-ca.pem > "$SSL_CERT_BUNDLE" 2>/dev/null; then
    export SSL_CERT_FILE="$SSL_CERT_BUNDLE"
    echo "[entrypoint] SSL_CERT_FILE=$SSL_CERT_BUNDLE (public roots + proxy CA) for Go hive process"
  else
    echo "[entrypoint] WARN: could not build combined CA bundle $SSL_CERT_BUNDLE; Go hive will rely on system trust store (racy)"
  fi
elif [ -f /data/proxy-ca.pem ]; then
  # No system bundle on disk (unusual): fall back to proxy CA alone so at least
  # proxied HTTPS verifies. Direct (non-proxied) HTTPS would fail here, but with
  # forced egress every :443 is proxied anyway.
  export SSL_CERT_FILE="/data/proxy-ca.pem"
  echo "[entrypoint] WARN: no system CA bundle at $SYSTEM_CA_BUNDLE; SSL_CERT_FILE=/data/proxy-ca.pem (proxy CA only)"
fi

# ── Config-path argv reconciliation (#4973) ───────────────────────────
# Both config branches above have a read-only escape hatch: when the config
# path cannot be written they `export HIVE_CONFIG=<runtime config>` and log
# "config path is read-only — using ... directly". That export was INERT.
#
# main.go reads HIVE_CONFIG only to pick the DEFAULT of its -config flag:
#
#   defaultConfig := "/etc/hive/hive.yaml"
#   if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" { defaultConfig = envCfg }
#   configPath := flag.String("config", defaultConfig, ...)
#
# and the image's own CMD passes the flag EXPLICITLY
# (Dockerfile: CMD ["--config", "/etc/hive/hive.yaml"]), which outranks any
# default. So on every hive whose config path is read-only — a bind-mounted
# hive.yaml under docker/podman is the common case — the binary loaded the
# stale read-only file while the entrypoint believed it had redirected it.
#
# The damage is not confined to that boot. Config.Save() writes
# /data/hive.yaml.runtime unconditionally, so the first save of any kind
# (dashboard edit, heartbeat-delivered config) wrote the STALE config back
# over the runtime file that held the operator's real state. Observed as an
# ACMM level set from the dashboard reverting on the next restart, twice, with
# /data fully intact (#4973).
#
# main.go cannot fix this on its own: an explicit --config coming from the
# image's CMD is indistinguishable from one an operator typed. The entrypoint
# is the component that knows the config path is stale, so it has to say so in
# the argv it actually launches with.
#
# APPENDED, not substituted: Go's flag package processes occurrences in order
# and keeps the last, so `--config <stale> ... --config $HIVE_CONFIG` resolves
# to HIVE_CONFIG without this script having to parse and rewrite the CMD.
#
# When HIVE_CONFIG is unset (the ordinary case, where the cp above succeeded
# and the config path IS current) nothing is appended and the CMD applies
# unchanged. When an operator sets HIVE_CONFIG themselves, this makes the
# binary honour it — which is what the variable already means everywhere else
# in this script (HIVE_CONFIG_PATH is derived from it at the top).
if [ -n "${HIVE_CONFIG:-}" ]; then
  set -- "$@" --config "$HIVE_CONFIG"
  echo "[entrypoint] Config path pinned for the Go binary: --config $HIVE_CONFIG"
fi
echo "[entrypoint] Starting Go binary on :${HIVE_API_PORT} (uid=$(id -u))"
hive "$@" &
HIVE_PID=$!

sleep 1

# Install the MITM proxy CA into the system trust store so that
# agent sub-processes (git, curl) trust the forged certificates.
# Also set NODE_EXTRA_CA_CERTS for Node.js (Copilot, etc.).
#
# NOTE: this system-bundle install is NO LONGER the trust path for the Go hive
# process — that now uses SSL_CERT_FILE (the combined bundle built before the
# launch above), which is race-free and survives a gosu failure here. So unlike
# the F5 egress enforcement (fatal on failure), a failure of THIS install is
# recoverable and must NOT be fatal: it only affects sub-processes that read the
# system store, and even those have the combined-bundle fallback via .bashrc /
# NODE_EXTRA_CA_CERTS. We log clearly but continue.
if [ -f /data/proxy-ca.pem ]; then
  if command -v gosu >/dev/null 2>&1; then
    gosu root sh -c 'cp /data/proxy-ca.pem /usr/local/share/ca-certificates/hive-proxy-ca.crt && update-ca-certificates' 2>/dev/null \
      && echo "[entrypoint] proxy CA installed to system trust store" \
      || echo "[entrypoint] WARN: proxy CA install via gosu failed (recoverable: Go hive uses SSL_CERT_FILE combined bundle; sub-processes use NODE_EXTRA_CA_CERTS / .bashrc SSL_CERT_FILE)"
  elif cp /data/proxy-ca.pem /usr/local/share/ca-certificates/hive-proxy-ca.crt 2>/dev/null; then
    update-ca-certificates 2>/dev/null && echo "[entrypoint] proxy CA installed to system trust store"
  else
    echo "[entrypoint] WARN: could not install proxy CA to system store (non-root; recoverable via SSL_CERT_FILE / NODE_EXTRA_CA_CERTS)"
  fi
  # Prefer the combined bundle (public roots + proxy CA) for Node.js too, for the
  # same advisory-mode safety as the Go process; fall back to the proxy CA alone.
  if [ -f /data/proxy-ca-bundle.pem ]; then
    export NODE_EXTRA_CA_CERTS=/data/proxy-ca-bundle.pem
  else
    export NODE_EXTRA_CA_CERTS=/data/proxy-ca.pem
  fi
  echo "[entrypoint] NODE_EXTRA_CA_CERTS=$NODE_EXTRA_CA_CERTS set for Node.js agents"
fi
# Watch for CA cert changes (Go binary may regenerate it) and re-install into
# the system store AND rebuild the combined SSL_CERT_FILE bundle so sub-processes
# and any future Go restart pick up the new proxy CA. (A restart is required for
# the already-running Go process to re-read SSL_CERT_FILE — Go caches it — but the
# supervisor restarts the process, which then reads the freshly-rebuilt bundle.)
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
      if [ -f "$SYSTEM_CA_BUNDLE" ]; then
        cat "$SYSTEM_CA_BUNDLE" /data/proxy-ca.pem > "$SSL_CERT_BUNDLE" 2>/dev/null \
          && echo "[entrypoint] combined CA bundle rebuilt at $SSL_CERT_BUNDLE (proxy CA hash changed)"
      fi
      PREV_HASH="$CUR_HASH"
    fi
  done
) &

echo "[entrypoint] Starting Node.js proxy on :${HIVE_PROXY_PORT} → :${HIVE_API_PORT}"
cd /opt/hive/proxy && node server.js &
PROXY_PID=$!

TTYD_PORT="${HIVE_TTYD_PORT:-7681}"
# SECURITY: ttyd is a WRITABLE terminal into the container that holds the GitHub
# credentials, so :7681 must NEVER be reachable directly — the only path to it is
# the Node proxy's /terminal route, which authenticates first (cookie on hosted
# hives, ?token= on self-hosted). It binds to loopback so nothing on the pod
# network (or a raw nginx stream proxy) can reach it without going through that
# gate. As defense in depth, ttyd is additionally credentialed when a dashboard
# token is available.
TTYD_BIND="${HIVE_TTYD_BIND:-127.0.0.1}"
TTYD_CRED="${HIVE_TTYD_CREDENTIAL:-}"
if [ -z "$TTYD_CRED" ] && [ -n "${HIVE_DASHBOARD_TOKEN:-}" ]; then
  TTYD_CRED="hive:${HIVE_DASHBOARD_TOKEN}"
fi
# CRED_ARGS carries ONLY the credential. --url-arg (-a) is passed unconditionally
# on the ttyd command line below and must never live in here: #4593 was caused by
# this branch REPLACING a CRED_ARGS that was also carrying -a, which silently
# dropped --url-arg on every hive with a dashboard token (i.e. effectively all of
# them — bin/hive-podman-setup.sh generates HIVE_DASHBOARD_TOKEN unconditionally
# and the Compose stack requires it). Keep the two concerns in separate places so
# the next edit to the credential branch cannot take --url-arg down with it.
CRED_ARGS=""
if [ -n "$TTYD_CRED" ]; then
  CRED_ARGS="-c ${TTYD_CRED}"
fi
echo "[entrypoint] Starting ttyd on ${TTYD_BIND}:${TTYD_PORT}"
# Wrap ttyd in a respawn loop: ttyd exits on SIGHUP (its close signal),
# and orphaned LISTEN sockets block rebind, so we wait before retrying.
TTYD_RESPAWN_DELAY_SECS=5
(
  trap '' HUP
  while true; do
    # -a/--url-arg lets ttyd forward the ?arg=<session> that the dashboard puts in
    # every terminal link (src/pkg/dashboard/static/index.html) through to
    # ttyd-tmux.sh. Without it ttyd discards the query, the attach script falls
    # back to its default session name, and the browser terminal dies with
    # "no tmux socket found for session 'supervisor'" (#4593).
    ttyd -W -a ${CRED_ARGS} -i "${TTYD_BIND}" -p "${TTYD_PORT}" -t fontSize=14 -t disableLeaveAlert=true /usr/local/bin/ttyd-tmux.sh
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

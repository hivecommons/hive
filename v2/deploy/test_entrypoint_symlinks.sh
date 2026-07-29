#!/usr/bin/env bash
# Tests that the entrypoint creates the required symlinks for
# Copilot CLI token persistence across container restarts.
# Run: bash v2/deploy/test_entrypoint_symlinks.sh
set -euo pipefail

PASS=0
FAIL=0

assert_contains() {
  local file="$1" pattern="$2" label="$3"
  if grep -q "$pattern" "$file"; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        pattern '$pattern' not found in $file"
    FAIL=$((FAIL + 1))
  fi
}

assert_not_contains_literal() {
  local file="$1" pattern="$2" label="$3"
  if grep -Fq "$pattern" "$file"; then
    echo "  FAIL: $label"
    echo "        unexpected literal '$pattern' found in $file"
    FAIL=$((FAIL + 1))
  else
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  fi
}

ENTRYPOINT="$(cd "$(dirname "$0")" && pwd)/entrypoint.sh"

echo "=== Entrypoint symlink regression tests ==="

# 1. ~/.copilot symlink to /data/home/.copilot must exist in entrypoint
assert_contains "$ENTRYPOINT" \
  'ln -sfn /data/home/.copilot /home/dev/.copilot' \
  "~/.copilot -> /data/home/.copilot symlink"

# 2. mkdir must create /data/home/.copilot
assert_contains "$ENTRYPOINT" \
  '/data/home/.copilot' \
  "/data/home/.copilot directory referenced"

# 3. Controller-owned Codex health checks use the bounded persistent auth home.
assert_contains "$ENTRYPOINT" \
  'export CODEX_HOME="${CODEX_HOME:-/data/home/.codex}"' \
  "Codex controller health uses persistent bounded auth home"
assert_contains "$ENTRYPOINT" \
  'chmod 0660 /data/home/.codex/auth.json' \
  "persisted Codex login remains readable by isolated agent UIDs"
assert_contains "$ENTRYPOINT" \
  '[ ! -L /data/home/.codex/auth.json ]' \
  "Codex credential normalization rejects linked auth paths"

# 4. ~/.config/github-copilot symlink must still exist (pre-existing)
assert_contains "$ENTRYPOINT" \
  'ln -sfn /data/config/github-copilot /home/dev/.config/github-copilot' \
  "~/.config/github-copilot -> /data/config/github-copilot symlink"

# 5. /data/home/.config/github-copilot symlink must still exist (pre-existing)
assert_contains "$ENTRYPOINT" \
  'ln -sfn /data/config/github-copilot /data/home/.config/github-copilot' \
  "/data/home/.config/github-copilot -> /data/config/github-copilot symlink"

# 6. group-writable chmod on /data/home (ensures all agent UIDs can access)
assert_contains "$ENTRYPOINT" \
  'chmod -R g+rwX /data/home' \
  "/data/home is group-writable"
assert_contains "$ENTRYPOINT" \
  'chown dev:node /data/home /data/home/.config /data/config /data/config/github-copilot' \
  "fresh shared CLI parents are owned by the runtime principal"
assert_contains "$ENTRYPOINT" \
  'chmod 2775 /data/home /data/home/.config /data/config /data/config/github-copilot' \
  "fresh shared CLI parents remain traversable and group-writable"

# 7. Entrypoint loads Copilot PAT for Go binary
assert_contains "$ENTRYPOINT" \
  'COPILOT_GITHUB_TOKEN' \
  "Entrypoint exports COPILOT_GITHUB_TOKEN"

# 8. PAT file path references /data volume (not hardcoded secret)
assert_contains "$ENTRYPOINT" \
  '/data/copilot-token-pat' \
  "PAT read from /data volume file"

# 9. agent-launch.sh also loads PAT as fallback
LAUNCH_SCRIPT="$(cd "$(dirname "$0")/../../bin" && pwd)/agent-launch.sh"
assert_contains "$LAUNCH_SCRIPT" \
  'COPILOT_GITHUB_TOKEN' \
  "agent-launch.sh exports COPILOT_GITHUB_TOKEN"

# 10. Role bead stores remain shared only with their scoped group after UID chown.
assert_contains "$ENTRYPOINT" \
  'mkdir -p /home/dev /data/beads' \
  "fresh config-only installs create the shared beads root before role provisioning"
assert_not_contains_literal "$ENTRYPOINT" \
  'if [ -d /etc/hive/agents ] || [ -d /data/beads ]; then' \
  "shared beads root initialization is not gated on pre-existing role state"
assert_contains "$ENTRYPOINT" \
  'chmod 0750 /data/beads' \
  "/data/beads parent is traversable but not writable by roles"
assert_contains "$ENTRYPOINT" \
  'chmod 2770 {} +' \
  "role bead directories preserve their scoped group"
assert_contains "$ENTRYPOINT" \
  'chmod 0660 {} +' \
  "role bead files survive cross-principal replacement"
assert_contains "$ENTRYPOINT" \
  'usermod -a -G "$ROLE_GROUP" dev' \
  "ordinary Hive joins each role-scoped bead group"
assert_contains "$ENTRYPOINT" \
  'ln -sfn "/data/beads/${agent_name}" "/home/dev/${agent_name}-beads"' \
  "freshly provisioned config roles receive their normal beads symlink"
assert_contains "$ENTRYPOINT" \
  'find -P "$beaddir" -xdev -type d' \
  "retired stores are normalized without following links"
assert_contains "$ENTRYPOINT" \
  'Include first-level hidden role stores' \
  "hidden retired role stores are normalized"
assert_contains "$ENTRYPOINT" \
  "agent.get('enabled', True) is not False" \
  "explicitly disabled role stores remain private to ordinary Hive"
assert_contains "$ENTRYPOINT" \
  'Per-agent files replace the base entry' \
  "per-agent overlays replace base role provisioning"
assert_contains "$ENTRYPOINT" \
  "effective_agents.setdefault" \
  "pack roles cannot re-enable an explicitly configured disabled role"

# 10. Restrictive caller umasks cannot hide the root-written UID map from Hive.
assert_contains "$ENTRYPOINT" \
  'chmod 0755 /var/run/hive' \
  "UID map parent is traversable by the dev process"
assert_contains "$ENTRYPOINT" \
  'chown dev:node /var/run/hive' \
  "UID map parent remains writable by the dev process"
assert_contains "$ENTRYPOINT" \
  'chmod 0644 /var/run/hive/uid-map.json' \
  "UID map is readable by the dev process"
assert_contains "$ENTRYPOINT" \
  'chown dev:node /var/run/hive/uid-map.json' \
  "dynamic UID map updates remain owned by the dev process"

# 11. An explicit writable HIVE_CONFIG remains writable after root setup.
assert_contains "$ENTRYPOINT" \
  'chown dev:node /etc/hive/hive.yaml "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_BACKUP"' \
  "custom config and backup ownership are normalized"
assert_contains "$ENTRYPOINT" \
  'chmod u+rw,go-w "$HIVE_CONFIG_PATH" "$HIVE_CONFIG_BACKUP"' \
  "custom config is writable only by ordinary Hive"

# 12. Image seed files must not recreate root-owned runtime paths after the
# mounted volume ownership repair.
assert_contains "$ENTRYPOINT" \
  'gosu dev:node cp -rn /opt/hive/seed-data/\* /data/' \
  "fresh-volume seed paths are created by the runtime principal"
assert_not_contains_literal "$ENTRYPOINT" \
  '    cp -rn /opt/hive/seed-data/* /data/' \
  "fresh-volume seed does not run as root"

echo ""
echo "Results: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ] || exit 1

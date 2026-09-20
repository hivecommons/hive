#!/usr/bin/env bash
# Exercises the real `just contribute-hive claude` container recipe against a
# stubbed runtime and a populated fake ~/.claude, and asserts what the
# container actually receives (#7836).
#
# The defect was behavioral, not textual: the recipe staged the contributor's
# ENTIRE ~/.claude — every Claude Code transcript on the machine, the full
# prompt history, paste cache, file history, plans, per-project memory and the
# private CLAUDE.md, 137 MB on the reporting host — into the sandbox where
# third-party test suites run for real. A grep for an allowlist would pass
# while a `cp -a` two lines later put the whole directory back, so the stub
# runtime lists the staged ~/.claude from inside the `run` call, before the
# recipe's cleanup trap removes it. No image, daemon, credential, or network
# is used.
#
# Run: bash src/deploy/test_contributor_claude_staging.sh
set -uo pipefail

PASS=0
FAIL=0
# Shared skip discipline (#5388): CI guarantees `just`, so a missing binary
# there is a broken test rather than a permissible skip.
# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && [ -n "${2:-}" ] && echo "        $2"
  FAIL=$((FAIL + 1))
}
check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then pass "$label"; else fail "$label" "want: '$want'  got: '$got'"; fi
}
contains() {
  local label="$1" haystack="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then pass "$label"
  else fail "$label" "missing: '$needle'"; fi
}
lacks() {
  local label="$1" haystack="$2" needle="$3"
  if grep -qF -- "$needle" <<<"$haystack"; then fail "$label" "unexpectedly present: '$needle'"
  else pass "$label"; fi
}

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if ! command -v just >/dev/null 2>&1; then
  hive_test_skip "'just' not installed; cannot exercise the recipe"
  hive_test_report; exit $?
fi

echo "=== contributor container stages an allowlist of ~/.claude (#7836) ==="

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAKE_HOME="$WORK/home"
STUB_BIN="$WORK/bin"
CAPTURE="$WORK/runtime-argv"
FAKE_RUNTIME_DIR="$WORK/runtime-dir"
# The recipe creates its staging dir under ${TMPDIR:-/tmp}. Pointing that at a
# private dir makes "nothing left behind" checkable.
FAKE_TMP="$WORK/tmp"
mkdir -p "$FAKE_HOME/.config/hive" "$FAKE_HOME/.config/claude-code" "$STUB_BIN" "$FAKE_RUNTIME_DIR" "$FAKE_TMP"

printf '%s\n' \
  'HIVE_HUB=wss://hive.example.test/contribute' \
  'HIVE_REGISTRATION_TOKEN=placeholder-registration-token' \
  'AGENT_BACKEND=claude' \
  > "$FAKE_HOME/.config/hive/contributor.env"
printf '%s\n' 'GH_TOKEN=placeholder-github-token' \
  > "$FAKE_HOME/.config/hive/gh-auth.env"
printf '%s\n' '{}' > "$FAKE_HOME/.config/claude-code/config.json"

# A credential the recipe's #5088 gate accepts (non-empty accessToken, no
# expiry), so the populated runs exercise the normal launch path.
CRED_FIXTURE='{"claudeAiOauth":{"accessToken":"stub-access-token","refreshToken":"stub-refresh-token","expiresAt":0}}'
SETTINGS_FIXTURE='{"model":"settings-fixture"}'

# populate_claude_home lays out ~/.claude the way the reporting host's looked.
# Everything outside the allowlist carries a LEAK marker, so a regression is
# attributable to a file, not just to a count.
populate_claude_home() {
  local dir="$FAKE_HOME/.claude"
  rm -rf "$dir"
  mkdir -p "$dir/projects/-home-someone-unrelated-repo/memory" \
    "$dir/paste-cache" "$dir/file-history/abc" "$dir/plans" "$dir/backups" \
    "$dir/todos" "$dir/statsig" "$dir/shell-snapshots" "$dir/session-env" \
    "$dir/plugins/repos"
  printf '%s\n' "$CRED_FIXTURE" > "$dir/.credentials.json"
  chmod 600 "$dir/.credentials.json"
  printf '%s\n' "$SETTINGS_FIXTURE" > "$dir/settings.json"
  printf '%s\n' '{"display":"LEAK-history: a prompt typed into an unrelated project"}' > "$dir/history.jsonl"
  printf '%s\n' '{"type":"user","message":"LEAK-transcript"}' > "$dir/projects/-home-someone-unrelated-repo/abc.jsonl"
  printf '%s\n' 'LEAK-memory' > "$dir/projects/-home-someone-unrelated-repo/memory/MEMORY.md"
  printf '%s\n' 'LEAK-claude-md: private global instructions' > "$dir/CLAUDE.md"
  printf '%s\n' 'LEAK-paste' > "$dir/paste-cache/1.txt"
  printf '%s\n' 'LEAK-file-history' > "$dir/file-history/abc/1.txt"
  printf '%s\n' 'LEAK-plan' > "$dir/plans/plan.md"
  printf '%s\n' 'LEAK-backup' > "$dir/backups/settings.json.bak"
  printf '%s\n' 'LEAK-todo' > "$dir/todos/1.json"
  printf '%s\n' 'LEAK-statsig' > "$dir/statsig/cache"
  printf '%s\n' 'LEAK-snapshot' > "$dir/shell-snapshots/snap.sh"
  printf '%s\n' 'LEAK-session-env' > "$dir/session-env/1.env"
  printf '%s\n' 'LEAK-plugin' > "$dir/plugins/installed_plugins.json"
}

# One implementation serves both runtime names. `run` records its argv one
# argument per line and, before returning, lists the directory that will be
# mounted at /home/dev/.claude — the recipe deletes it on exit, so this is the
# only moment the container's view of ~/.claude can be observed.
cat > "$STUB_BIN/runtime" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
  run)
    shift
    printf '%s\n' "$@" > "$RUNTIME_CAPTURE"
    for arg in "$@"; do
      case "$arg" in
        *:/home/dev/.claude|*:/home/dev/.claude:*)
          src="${arg%%:/home/dev/.claude*}"
          ( cd "$src" && find . -mindepth 1 | LC_ALL=C sort ) > "$RUNTIME_CAPTURE.claude-tree"
          if [ -f "$src/.credentials.json" ]; then
            cat "$src/.credentials.json" > "$RUNTIME_CAPTURE.claude-cred"
            ( stat -c %a "$src/.credentials.json" 2>/dev/null || stat -f %Lp "$src/.credentials.json" ) > "$RUNTIME_CAPTURE.claude-mode"
          fi
          ;;
      esac
    done
    echo stub-container-id
    ;;
  inspect)
    case "$*" in
      *'.State.Running'*)   echo true ;;
      *'.State.ExitCode'*)  echo 0 ;;
      *'.State.OOMKilled'*) echo false ;;
      *) exit 1 ;;
    esac
    ;;
  logs|rm|pull) exit 0 ;;
  *) exit 1 ;;
esac
EOF
chmod +x "$STUB_BIN/runtime"
ln -s runtime "$STUB_BIN/docker"
ln -s runtime "$STUB_BIN/podman"

cat > "$STUB_BIN/gh" <<'EOF'
#!/usr/bin/env bash
[ "${1:-} ${2:-}" = "api user" ] && echo test-contributor
exit 0
EOF
cat > "$STUB_BIN/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$STUB_BIN/gh" "$STUB_BIN/sleep"

run_contributor() {
  local runtime="$1"
  shift
  rm -f "$CAPTURE" "$CAPTURE.claude-tree" "$CAPTURE.claude-cred" "$CAPTURE.claude-mode"
  (
    cd "$ROOT" || exit 1
    # ANTHROPIC_API_KEY would short-circuit the credential gate; CONTRIBUTOR_MODE
    # is set per case below.
    env -u ANTHROPIC_API_KEY -u CONTRIBUTOR_MODE \
      -u HIVE_CONTAINER_MEMORY -u HIVE_CONTAINER_CPUS \
      HOME="$FAKE_HOME" PATH="$STUB_BIN:$PATH" TMPDIR="$FAKE_TMP" \
      XDG_RUNTIME_DIR="$FAKE_RUNTIME_DIR" \
      RUNTIME_CAPTURE="$CAPTURE" HIVE_CONTAINER_RUNTIME="$runtime" \
      HIVE_SKIP_VERSION_CHECK=true HIVE_SKIP_PULL=true \
      "$@" just contribute-hive claude 2>&1
  )
}

# claude_mount_source prints the host path the recipe mounted at
# /home/dev/.claude (with any :Z suffix stripped), or nothing.
claude_mount_source() {
  local line
  line="$(grep -E ':/home/dev/\.claude(:|$)' "$CAPTURE" 2>/dev/null | head -n 1)"
  printf '%s' "${line%%:/home/dev/.claude*}"
}

staged_tree() { cat "$CAPTURE.claude-tree" 2>/dev/null; }

for runtime in docker podman; do
  echo ""
  echo "-- $runtime: a populated ~/.claude --"
  populate_claude_home
  OUT="$(run_contributor "$runtime")"; RC=$?
  check "$runtime: recipe exits successfully" "0" "$RC"
  TREE="$(staged_tree)"
  check "$runtime: the container's ~/.claude holds exactly the credential and settings" \
    "$(printf '%s\n%s' ./.credentials.json ./settings.json)" "$TREE"
  for leak in history.jsonl projects CLAUDE.md paste-cache file-history plans \
      backups todos statsig shell-snapshots session-env plugins; do
    lacks "$runtime: $leak does not reach the container" "$TREE" "$leak"
  done
  check "$runtime: the staged credential is the host's, byte for byte" \
    "$CRED_FIXTURE" "$(cat "$CAPTURE.claude-cred" 2>/dev/null)"
  check "$runtime: the staged credential keeps its 0600 mode" \
    "600" "$(cat "$CAPTURE.claude-mode" 2>/dev/null)"
  SRC="$(claude_mount_source)"
  case "$SRC" in
    "$FAKE_HOME/.claude")
      fail "$runtime: mounts the staged copy, not the host's ~/.claude" "the host directory itself is mounted" ;;
    "$FAKE_TMP"/hive-cli-stage.*/.claude)
      pass "$runtime: mounts the staged copy, not the host's ~/.claude" ;;
    *)
      fail "$runtime: mounts the staged copy, not the host's ~/.claude" "mount source: '$SRC'" ;;
  esac
  contains "$runtime: ~/.config/claude-code is still mounted" \
    "$(cat "$CAPTURE" 2>/dev/null)" ":/home/dev/.config/claude-code"
  contains "$runtime: the operator is told what was staged" \
    "$OUT" "Staged:    .credentials.json settings.json from $FAKE_HOME/.claude"
  lacks "$runtime: a usable credential raises no login warning" "$OUT" "No usable Claude credential"
done

echo ""
echo "-- a host with a credential but no settings.json --"
populate_claude_home
rm -f "$FAKE_HOME/.claude/settings.json"
OUT="$(run_contributor docker)"; RC=$?
check "recipe exits successfully" "0" "$RC"
check "the container's ~/.claude holds only the credential" "./.credentials.json" "$(staged_tree)"
contains "the operator is told only the credential was staged" \
  "$OUT" "Staged:    .credentials.json from $FAKE_HOME/.claude"

echo ""
echo "-- a host that has never run Claude Code (no ~/.claude) --"
rm -rf "$FAKE_HOME/.claude"
OUT="$(run_contributor docker)"; RC=$?
check "an interactive run still launches" "0" "$RC"
if [ -f "$CAPTURE.claude-tree" ]; then
  check "the container gets an empty, writable ~/.claude" "" "$(staged_tree)"
else
  fail "the container gets an empty, writable ~/.claude" "nothing was mounted at /home/dev/.claude"
fi
contains "the operator is told nothing was staged" "$OUT" "Staged:    nothing from $FAKE_HOME/.claude"
contains "the #5088 warning still fires" "$OUT" "No usable Claude credential"

echo ""
echo "-- headless with no credential (#5088) --"
OUT="$(run_contributor docker CONTRIBUTOR_MODE=headless)"; RC=$?
check "a headless run with nothing to stage refuses to start" "1" "$RC"
check "no container was started" "" "$(cat "$CAPTURE" 2>/dev/null)"

echo ""
echo "-- nothing left behind --"
LEFT="$(find "$FAKE_TMP" -maxdepth 1 -name 'hive-cli-stage.*' 2>/dev/null | wc -l | tr -d ' ')"
check "every run removed its staging dir, including the headless refusal" "0" "$LEFT"

hive_test_report

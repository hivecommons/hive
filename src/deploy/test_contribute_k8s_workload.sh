#!/usr/bin/env bash
# Tests the `just contribute-k8s` contributor WORKLOAD generation (#2549).
#
# Before #2549 the recipe emitted only Namespace + ConfigMap + Secret and
# nothing ran. It now ALSO emits a Deployment that runs the relay HEADLESS
# (#2660) — a headless pod has no TTY, so it MUST set CONTRIBUTOR_MODE=headless
# or the interactive tmux path stalls forever. This executes the real recipe
# against a synthetic contributor.env and asserts the workload it produces,
# rather than grepping the Justfile, so a regression fails the PR.
#
# Run: bash src/deploy/test_contribute_k8s_workload.sh
set -euo pipefail

PASS=0
FAIL=0

# Shared skip discipline (#5388): hive_test_skip is permissive by default and
# FATAL under HIVE_TEST_REQUIRE_BEHAVIOURAL=1, so a lane whose runner GUARANTEES
# the precondition below turns a silent skip into a red build.
# shellcheck source=src/deploy/test_lib.sh
. "$(cd "$(dirname "$0")" && pwd)/test_lib.sh"

check() {
  local label="$1" want="$2" got="$3"
  if [ "$want" = "$got" ]; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label"
    echo "        want: '$want'"
    echo "        got:  '$got'"
    FAIL=$((FAIL + 1))
  fi
}

contains() {
  local label="$1" haystack="$2" needle="$3"
  if printf '%s' "$haystack" | grep -qF "$needle"; then
    echo "  PASS: $label"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $label (missing: '$needle')"
    FAIL=$((FAIL + 1))
  fi
}

# Locate the repo root (this script lives in src/deploy/) and require `just`.
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
if ! command -v just >/dev/null 2>&1; then
  hive_test_skip "'just' not installed; cannot exercise the recipe"
  hive_test_report; exit $?
fi

echo "=== contribute-k8s workload generation tests ==="

# Synthetic contributor config under a throwaway HOME (config_dir is
# $HOME/.config/hive). Placeholder token values only — never a real credential.
FAKE_HOME="$(mktemp -d)"
trap 'rm -rf "$FAKE_HOME"' EXIT
mkdir -p "$FAKE_HOME/.config/hive"
cat > "$FAKE_HOME/.config/hive/contributor.env" <<'EOF'
HIVE_HUB=wss://hive.example.io:3001/contribute
CONTRIBUTOR_ID=c-test01
CONTRIBUTOR_USERNAME=testuser
AGENT_BACKEND=claude
HIVE_REGISTRATION_TOKEN=placeholder-reg-token
EOF
cat > "$FAKE_HOME/.config/hive/gh-auth.env" <<'EOF'
GH_TOKEN=placeholder-gh-token
EOF
# A logged-in Claude credential file (#5103): the generator refuses to emit a
# claude workload without one (or ANTHROPIC_API_KEY), so the happy path seeds
# a placeholder. Never a real credential.
mkdir -p "$FAKE_HOME/.claude"
printf '%s' '{"claudeAiOauth":{"accessToken":"placeholder-claude-oauth","refreshToken":"placeholder-claude-refresh"}}' \
  > "$FAKE_HOME/.claude/.credentials.json"

# ── Supported backend (claude): full workload on stdout ──
OUT="$(cd "$REPO_ROOT" && HOME="$FAKE_HOME" ANTHROPIC_API_KEY="" just contribute-k8s hive-contributor 2>/dev/null)"

contains "emits a Deployment"                 "$OUT" "kind: Deployment"
contains "workload runs headless (#2660)"     "$OUT" "CONTRIBUTOR_MODE: \"headless\""
contains "status file env for the probe"      "$OUT" "HIVE_HEADLESS_STATUS_FILE:"
contains "uses the published contributor image" "$OUT" "image: ghcr.io/hivecommons/hive-contributor:v4"
contains "wires the ConfigMap via envFrom"    "$OUT" "configMapRef:"
contains "wires the Secret via secretRef"     "$OUT" "secretRef:"
contains "has a readiness probe"              "$OUT" "readinessProbe:"
contains "has a liveness probe"               "$OUT" "livenessProbe:"
contains "probe reads the status file"        "$OUT" "contributor-headless-status.json"
contains "requests realistic memory"          "$OUT" "memory: \"1Gi\""
contains "single replica"                     "$OUT" "replicas: 1"
contains "interim credential note defers to #2537" "$OUT" "#2537"

# ── Placeholder discipline: no real token bytes leak; still base64 placeholders. ──
if printf '%s' "$OUT" | grep -q "GH_TOKEN: cGxhY2Vob2xkZXItZ2gtdG9rZW4="; then
  echo "  PASS: Secret carries the base64 placeholder token (templated, not raw)"
  PASS=$((PASS + 1))
else
  echo "  FAIL: expected base64-encoded placeholder token in the Secret"
  FAIL=$((FAIL + 1))
fi

# ── The generated YAML must parse. Prefer python3+yaml; fall back to a
#    kind-count sanity check if PyYAML is unavailable. ──
if command -v python3 >/dev/null 2>&1 && python3 -c 'import yaml' >/dev/null 2>&1; then
  KINDS="$(printf '%s' "$OUT" | python3 -c 'import sys,yaml; print(",".join(d["kind"] for d in yaml.safe_load_all(sys.stdin) if d))')"
  check "YAML parses to the four expected kinds" "Namespace,ConfigMap,Secret,Deployment" "$KINDS"
else
  hive_test_skip "PyYAML unavailable; skipping structural parse"
fi

# ── Backend credential delivery (#5103) ──
# The workload used to carry NO credential for the agent CLI itself: the pod
# authenticated to the hub and GitHub, then launched a backend with nothing to
# authenticate with. These pin the three claude outcomes: OAuth file shipped,
# explicit API key preferred, and an honest refusal when neither exists.
contains "Secret ships the operator's Claude credential (#5103)" "$OUT" "HIVE_CLAUDE_CREDENTIALS_B64:"
# Round-trip, not just presence: the Secret value, decoded once by Kubernetes
# (envFrom) and once by the entrypoint, must reproduce the credential file
# byte-for-byte. Presence-only checking passed an EMPTY value during
# development (the b64 helper was called before its definition), so this
# assertion is the one that actually guards the mechanism.
CRED_VAL="$(printf '%s' "$OUT" | grep 'HIVE_CLAUDE_CREDENTIALS_B64:' | awk '{print $2}')"
ROUNDTRIP="$(printf '%s' "$CRED_VAL" | base64 -d 2>/dev/null | base64 -d 2>/dev/null || true)"
check "credential round-trips byte-for-byte through Secret + entrypoint decode" \
  "$(cat "$FAKE_HOME/.claude/.credentials.json")" "$ROUNDTRIP"
if printf '%s' "$OUT" | grep -q "placeholder-claude-oauth"; then
  echo "  FAIL: raw Claude credential bytes leaked into the YAML unencoded"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: Claude credential travels encoded, never as raw bytes"
  PASS=$((PASS + 1))
fi
contains "credential note covers the backend credential" "$OUT" "#5103"

# An explicit ANTHROPIC_API_KEY beats the file — operator intent wins.
KEYED="$(cd "$REPO_ROOT" && HOME="$FAKE_HOME" ANTHROPIC_API_KEY="placeholder-anthropic-key" just contribute-k8s hive-contributor 2>/dev/null)"
contains "explicit ANTHROPIC_API_KEY is shipped" "$KEYED" "ANTHROPIC_API_KEY:"
if printf '%s' "$KEYED" | grep -q "HIVE_CLAUDE_CREDENTIALS_B64:"; then
  echo "  FAIL: API key set, but the OAuth file was shipped anyway"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: API key takes precedence over the OAuth file"
  PASS=$((PASS + 1))
fi

# No credential at all: refuse at generation, naming the fix, exiting nonzero —
# a refusal here beats a manifest that deploys cleanly and cannot work.
NOCRED_HOME="$(mktemp -d)"
mkdir -p "$NOCRED_HOME/.config/hive"
cp "$FAKE_HOME/.config/hive/contributor.env" "$NOCRED_HOME/.config/hive/"
cp "$FAKE_HOME/.config/hive/gh-auth.env" "$NOCRED_HOME/.config/hive/"
if (cd "$REPO_ROOT" && HOME="$NOCRED_HOME" ANTHROPIC_API_KEY="" just contribute-k8s hive-contributor >/dev/null 2>"$NOCRED_HOME/err"); then
  echo "  FAIL: generation succeeded with no claude credential to ship"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: generation refuses when the claude CLI would have no credential"
  PASS=$((PASS + 1))
fi
contains "refusal names the missing credential" "$(cat "$NOCRED_HOME/err")" "no credential to ship"
contains "refusal points at the tracking issue" "$(cat "$NOCRED_HOME/err")" "#5103"

# The escape hatch emits anyway (credentials provided out of band), warning on
# stderr and keeping stdout clean YAML for kubectl.
HATCH_OUT="$(cd "$REPO_ROOT" && HOME="$NOCRED_HOME" ANTHROPIC_API_KEY="" HIVE_K8S_ALLOW_MISSING_BACKEND_CREDENTIALS=1 just contribute-k8s hive-contributor 2>"$NOCRED_HOME/hatch-err")"
contains "escape hatch still emits the Deployment" "$HATCH_OUT" "kind: Deployment"
contains "escape hatch warns on stderr" "$(cat "$NOCRED_HOME/hatch-err")" "NO claude credential"
if printf '%s' "$HATCH_OUT" | grep -q "WARNING:"; then
  echo "  FAIL: escape-hatch warning leaked into stdout"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: escape-hatch stdout stays clean YAML"
  PASS=$((PASS + 1))
fi

# copilot: OAuth state directory, unverified in an unattended pod — refused
# with a pointer rather than emitted broken.
sed -i.bak 's/^AGENT_BACKEND=.*/AGENT_BACKEND=copilot/' "$NOCRED_HOME/.config/hive/contributor.env"
if (cd "$REPO_ROOT" && HOME="$NOCRED_HOME" just contribute-k8s hive-contributor >/dev/null 2>"$NOCRED_HOME/copilot-err"); then
  echo "  FAIL: copilot generation succeeded despite unverified pod auth"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: copilot refuses with its credential path unverified"
  PASS=$((PASS + 1))
fi
contains "copilot refusal names the alternative" "$(cat "$NOCRED_HOME/copilot-err")" "contribute-hive copilot"
rm -rf "$NOCRED_HOME"

# ── Unsupported headless backend (bob): warn on STDERR, keep stdout clean. ──
# bob drives an interactive TUI with no known one-shot entry point. goose used
# to be the example here, but it is headless-capable via `goose run` (#2828).
cat > "$FAKE_HOME/.config/hive/contributor.env" <<'EOF'
AGENT_BACKEND=bob
HIVE_REGISTRATION_TOKEN=placeholder-reg-token
EOF
ERR="$(cd "$REPO_ROOT" && HOME="$FAKE_HOME" just contribute-k8s hive-contributor 2>&1 >/dev/null)"
STDOUT_ONLY="$(cd "$REPO_ROOT" && HOME="$FAKE_HOME" just contribute-k8s hive-contributor 2>/dev/null)"
contains "unsupported backend warns on stderr" "$ERR" "no headless (non-interactive) mode"
# The warning must NOT pollute stdout (stdout is piped straight to kubectl).
if printf '%s' "$STDOUT_ONLY" | grep -q "WARNING:"; then
  echo "  FAIL: warning leaked into stdout (would corrupt 'kubectl apply -f -')"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: stdout stays clean YAML even for an unsupported backend"
  PASS=$((PASS + 1))
fi

# ── Image pinning via the 3rd argument. ──
PINNED="$(cd "$REPO_ROOT" && HOME="$FAKE_HOME" just contribute-k8s hive-contributor "" abc1234 2>/dev/null)"
contains "3rd arg pins the image tag" "$PINNED" "image: ghcr.io/hivecommons/hive-contributor:abc1234"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

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
  echo "  SKIP: 'just' not installed; cannot exercise the recipe"
  exit 0
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

# ── Supported backend (claude): full workload on stdout ──
OUT="$(cd "$REPO_ROOT" && HOME="$FAKE_HOME" just contribute-k8s hive-contributor 2>/dev/null)"

contains "emits a Deployment"                 "$OUT" "kind: Deployment"
contains "workload runs headless (#2660)"     "$OUT" "CONTRIBUTOR_MODE: \"headless\""
contains "status file env for the probe"      "$OUT" "HIVE_HEADLESS_STATUS_FILE:"
contains "uses the published contributor image" "$OUT" "image: ghcr.io/kubestellar/hive-contributor:v4"
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
  echo "  SKIP: PyYAML unavailable; skipping structural parse"
fi

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
contains "3rd arg pins the image tag" "$PINNED" "image: ghcr.io/kubestellar/hive-contributor:abc1234"

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

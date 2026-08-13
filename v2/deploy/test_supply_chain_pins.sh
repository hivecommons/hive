#!/usr/bin/env bash
# Asserts that supply-chain-sensitive versions stay PINNED.
#
# Why this exists: the PI_VERSION pin (#3443, commit 2011739b) was landed and
# then silently LOST. The commit is an ancestor of v4, but a sync/v2-into-v4-*
# merge resolved the conflict in favour of the older v2 side and restored
# `ARG PI_VERSION=latest`. Nothing failed, so nobody noticed. The same class of
# regression hit audit findings F14 and F18.
#
# So this does NOT test behaviour — it tests the INVARIANT. A floating tag means
# any malicious npm publish or image push lands automatically in the next build,
# and for watchtower (which mounts /var/run/docker.sock) that is full host
# control. Re-introducing `=latest`, or dropping the watchtower digest, must
# fail the PR rather than quietly ship.
#
# Run: bash v2/deploy/test_supply_chain_pins.sh
set -euo pipefail

PASS=0
FAIL=0

V2_DIR="$(cd "$(dirname "$0")/.." && pwd)"
DOCKERFILE="$V2_DIR/Dockerfile"
DOCKERFILE_CONTRIB="$V2_DIR/Dockerfile.contributor"
COMPOSE="$V2_DIR/docker-compose.yaml"

pass() {
  echo "  PASS: $1"
  PASS=$((PASS + 1))
}

fail() {
  echo "  FAIL: $1"
  [ $# -gt 1 ] && echo "        $2"
  FAIL=$((FAIL + 1))
}

echo "=== supply-chain pin tests ==="

# --- 1. No ARG *_VERSION=latest in any v2 Dockerfile -------------------------
# Matches optional whitespace and quoting so `ARG X_VERSION = "latest"` and
# `ARG X_VERSION=LATEST` cannot slip through the guard.
for df in "$DOCKERFILE" "$DOCKERFILE_CONTRIB"; do
  rel="${df#"$(dirname "$V2_DIR")"/}"
  if [ ! -f "$df" ]; then
    fail "$rel exists" "file not found — guard cannot verify pins"
    continue
  fi

  floating="$(grep -inE '^[[:space:]]*ARG[[:space:]]+[A-Z0-9_]*VERSION[[:space:]]*=[[:space:]]*"?'"'"'?latest' "$df" || true)"
  if [ -n "$floating" ]; then
    fail "$rel has no floating ARG *_VERSION=latest" \
         "found: $(echo "$floating" | tr '\n' ' ')"
  else
    pass "$rel has no floating ARG *_VERSION=latest"
  fi

  # Every ARG *_VERSION must carry a non-empty value; `ARG FOO_VERSION` with no
  # default lets the builder inject anything via --build-arg at build time.
  valueless="$(grep -inE '^[[:space:]]*ARG[[:space:]]+[A-Z0-9_]*VERSION[[:space:]]*$' "$df" || true)"
  if [ -n "$valueless" ]; then
    fail "$rel has no valueless ARG *_VERSION" \
         "found: $(echo "$valueless" | tr '\n' ' ')"
  else
    pass "$rel has no valueless ARG *_VERSION"
  fi
done

# --- 2. Specific pins that have regressed before ------------------------------
# Named explicitly so that deleting the line (rather than setting it to
# `latest`) also trips the guard.
check_pinned_arg() {
  local name="$1" file="$2"
  local line
  line="$(grep -E "^[[:space:]]*ARG[[:space:]]+${name}[[:space:]]*=" "$file" | head -1 || true)"
  if [ -z "$line" ]; then
    fail "$name is present and pinned" "no ARG $name found in $file"
    return
  fi
  local value="${line#*=}"
  value="$(echo "$value" | tr -d ' "'"'"'')"
  if [ -z "$value" ] || [ "$value" = "latest" ]; then
    fail "$name is pinned to a concrete version" "got: '$value'"
  else
    pass "$name is pinned ($value)"
  fi
}

check_pinned_arg CLAUDE_CODE_VERSION "$DOCKERFILE"
check_pinned_arg PI_VERSION "$DOCKERFILE"
check_pinned_arg COPILOT_VERSION "$DOCKERFILE"
check_pinned_arg CODEX_VERSION "$DOCKERFILE"

# --- 3. watchtower must be pinned BY DIGEST ----------------------------------
# A tag alone is mutable: containrrr could re-push 1.7.1. Since this container
# mounts the Docker socket, the digest is the only thing that actually binds
# the running code to what was reviewed.
if [ ! -f "$COMPOSE" ]; then
  fail "docker-compose.yaml exists" "file not found at $COMPOSE"
else
  wt_line="$(grep -E '^[[:space:]]*image:[[:space:]]*containrrr/watchtower' "$COMPOSE" | head -1 || true)"
  if [ -z "$wt_line" ]; then
    fail "watchtower image line found" "no containrrr/watchtower image: line in docker-compose.yaml"
  elif echo "$wt_line" | grep -qE '@sha256:[0-9a-f]{64}'; then
    pass "watchtower is pinned by @sha256 digest"
  else
    fail "watchtower is pinned by @sha256 digest" "got: $(echo "$wt_line" | xargs)"
  fi
fi

# --- 4. No floating :latest image tags anywhere in compose -------------------
latest_images="$(grep -inE '^[[:space:]]*image:[[:space:]]*[^#]*:latest[[:space:]]*$' "$COMPOSE" || true)"
if [ -n "$latest_images" ]; then
  fail "no compose service uses a :latest image tag" \
       "found: $(echo "$latest_images" | tr '\n' ' ')"
else
  pass "no compose service uses a :latest image tag"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1

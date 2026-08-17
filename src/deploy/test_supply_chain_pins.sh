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
# Run: bash src/deploy/test_supply_chain_pins.sh
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

# --- 5. NodeSource GPG key must be pinned by SHA-256 before use --------------
# issue #3821: the key used to be curl | gpg --dearmor with no verification, so
# a compromised deb.nodesource.com or a build-network MITM could swap in a
# trusted signing key silently. Guard both that the ARG is a real 64-hex-char
# digest (not blank/placeholder) and that the RUN step actually verifies it
# with sha256sum -c before dearmoring -- pinning the ARG alone without wiring
# the check into the RUN would regress silently back to "trust on first use".
nodesource_arg="$(grep -E '^[[:space:]]*ARG[[:space:]]+NODESOURCE_KEY_SHA256[[:space:]]*=' "$DOCKERFILE_CONTRIB" | head -1 || true)"
if [ -z "$nodesource_arg" ]; then
  fail "NODESOURCE_KEY_SHA256 is present and pinned" "no ARG NODESOURCE_KEY_SHA256 found in $DOCKERFILE_CONTRIB"
else
  nodesource_value="$(echo "${nodesource_arg#*=}" | tr -d ' "'"'"'')"
  if echo "$nodesource_value" | grep -qE '^[0-9a-f]{64}$'; then
    pass "NODESOURCE_KEY_SHA256 is pinned to a 64-hex-char digest"
  else
    fail "NODESOURCE_KEY_SHA256 is pinned to a 64-hex-char digest" "got: '$nodesource_value'"
  fi
fi

if grep -A2 'NODESOURCE_KEY_SHA256}  /tmp/nodesource.key' "$DOCKERFILE_CONTRIB" | grep -q 'sha256sum -c'; then
  pass "nodesource key fetch is verified with sha256sum -c before dearmoring"
else
  fail "nodesource key fetch is verified with sha256sum -c before dearmoring" \
       "expected an 'echo \"\$NODESOURCE_KEY_SHA256  /tmp/nodesource.key\" | sha256sum -c -' step in $DOCKERFILE_CONTRIB"
fi

# --- 6. Every FROM must be digest-pinned (#3843) ------------------------------
# A tag is mutable. `alpine:3.24` was the last tag-only base here, and Alpine
# re-pushes patch tags on every CVE respin — so the hub image's whole trusted
# computing base could change with no diff in this repo.
#
# This is written as a SWEEP over every FROM in every v2 Dockerfile rather than
# a named check for alpine, deliberately: a named check would pass forever while
# a NEW tag-pinned base was added next to it. The failure mode being guarded is
# "someone adds an unpinned FROM", which only a sweep catches.
#
# `FROM x AS y` and `COPY --from=<stage>` refer to build STAGES, not registry
# images, and have no digest — so stage names defined earlier in the same file
# are excluded from the requirement.
for df in "$DOCKERFILE" "$DOCKERFILE_CONTRIB" "$V2_DIR/Dockerfile.hub"; do
  rel="${df#"$(dirname "$V2_DIR")"/}"
  if [ ! -f "$df" ]; then
    fail "$rel exists for FROM digest sweep" "file not found"
    continue
  fi

  # Collect stage aliases (the `AS <name>` on each FROM) so intra-file stage
  # references are not mistaken for unpinned registry images.
  stages="$(grep -iE '^[[:space:]]*FROM[[:space:]]' "$df" \
            | sed -nE 's/.*[[:space:]][aA][sS][[:space:]]+([A-Za-z0-9_.-]+).*/\1/p')"

  unpinned=""
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    # Image reference is the first token after FROM, skipping --platform=... flags.
    img="$(printf '%s\n' "$line" | sed -E 's/^[[:space:]]*[fF][rR][oO][mA-Za-z]*[[:space:]]+//; s/^(--[a-z-]+(=[^ ]+)?[[:space:]]+)*//' | awk '{print $1}')"
    [ -z "$img" ] && continue
    # Skip references to a stage declared in this same file.
    if printf '%s\n' "$stages" | grep -qxF "$img"; then continue; fi
    # Skip scratch, which has no digest by definition.
    [ "$img" = "scratch" ] && continue
    if ! printf '%s\n' "$img" | grep -qE '@sha256:[0-9a-f]{64}$'; then
      unpinned="${unpinned}${img} "
    fi
  done <<EOF
$(grep -iE '^[[:space:]]*FROM[[:space:]]' "$df")
EOF

  if [ -n "$unpinned" ]; then
    fail "$rel: every FROM is digest-pinned" "tag-pinned (mutable): $unpinned"
  else
    pass "$rel: every FROM is digest-pinned"
  fi
done

# --- 7. Nous must install from a pinned COMMIT, not a branch (#3843) ---------
# The Nous framework is `git clone`d and pip-installed with `-e`. Cloning
# without a checkout would track the default branch, so the content behind a
# fixed Dockerfile line could change on any upstream push — a supply-chain
# injection with no diff here. Guard BOTH halves: the ARG must be a full 40-hex
# commit SHA (a branch name or short SHA is not good enough — a short SHA is
# ambiguous and a branch moves), AND the RUN must actually `git checkout` it.
# Pinning the ARG without wiring the checkout would regress silently to
# "whatever the default branch says", which is exactly the shape of the
# PI_VERSION regression this file was written for.
nous_arg="$(grep -E '^[[:space:]]*ARG[[:space:]]+NOUS_COMMIT[[:space:]]*=' "$DOCKERFILE" | head -1 || true)"
if [ -z "$nous_arg" ]; then
  fail "NOUS_COMMIT is present and pinned" "no ARG NOUS_COMMIT found in $DOCKERFILE"
else
  nous_value="$(echo "${nous_arg#*=}" | tr -d ' "'"'"'')"
  if echo "$nous_value" | grep -qE '^[0-9a-f]{40}$'; then
    pass "NOUS_COMMIT is pinned to a full 40-hex commit SHA"
  else
    fail "NOUS_COMMIT is pinned to a full 40-hex commit SHA" "got: '$nous_value'"
  fi
fi

if grep -qE 'git -C /opt/nous checkout "\$\{NOUS_COMMIT\}"' "$DOCKERFILE"; then
  pass "nous clone is checked out at the pinned NOUS_COMMIT"
else
  fail "nous clone is checked out at the pinned NOUS_COMMIT" \
       "expected a 'git -C /opt/nous checkout \"\${NOUS_COMMIT}\"' step in $DOCKERFILE"
fi

echo
echo "=== $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ] || exit 1

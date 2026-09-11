#!/usr/bin/env bash
# In-container attach hints must name the runtime that actually launched the
# container (kubestellar/hive#5145).
# Run: bash src/deploy/test_attach_hint_runtime.sh
#
# WHAT WENT WRONG. `just contribute-hive` (container mode) resolves docker OR
# podman. Both hints printed from INSIDE the container hardcoded docker, because
# a container cannot see its own launcher. Observed live on a podman launch, in
# one screen of output:
#
#   Attach: podman exec -it hive-contributor-agy-... tmux attach -t contributor
#   Tmux:   docker exec -it hive-contributor-agy-... tmux attach -t contributor
#
# Two contradictory instructions for the same container. The operator pastes the
# second and gets "permission denied ... /var/run/docker.sock", or "no such
# container" if docker happens to be running too.
#
# The fix passes the resolved runtime in as HIVE_CONTAINER_RUNTIME, so this test
# pins the whole chain: the recipe PASSES it, the entrypoint READS it, and the
# host-side and in-container hints AGREE. Agreement is the assertion the bug
# report is actually about — either hint alone can be self-consistently wrong.
#
# The relay's own banner (the worse of the two sites: it fires when a human MUST
# attach to complete a login) is covered behaviourally next door, in
# bin/contributor-relay.test.js — it is JavaScript, and loading it to read the
# value it prints is strictly stronger than asserting on its source text here.
#
# Every string under test is READ FROM ITS SHIPPED SOURCE and evaluated, never
# restated: a copy would keep passing after the real one regressed.
set -uo pipefail

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [ $# -gt 1 ] && echo "        $2"; FAIL=$((FAIL + 1)); }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
JUSTFILE="${ROOT}/Justfile"
AGENT="${ROOT}/bin/contributor-agent.sh"
RELAY="${ROOT}/bin/contributor-relay.js"

echo "=== in-container attach hints name the real runtime (#5145) ==="

# ── 1. The recipe passes the resolved runtime in ────────────────────────────
#
# It must ride along on the SAME container-run invocation that already passes
# HIVE_CONTAINER_NAME, not merely appear somewhere in a 2,000-line Justfile.
RUN_BLOCK="$(awk '/"\$RUNTIME" run -d/{inblock=1} inblock{print} inblock && /hive_image/{exit}' "$JUSTFILE")"
if ! grep -qF -- '-e HIVE_CONTAINER_NAME=' <<<"$RUN_BLOCK"; then
  fail "locate the contributor container-run invocation in the Justfile" \
       "the anchors moved; this test cannot verify what the recipe passes"
elif grep -qF -- '-e HIVE_CONTAINER_RUNTIME="${RUNTIME}"' <<<"$RUN_BLOCK"; then
  pass "the Justfile passes the resolved runtime as HIVE_CONTAINER_RUNTIME"
else
  fail "the Justfile passes HIVE_CONTAINER_RUNTIME" \
       "without it the container has no way to know its launcher and guesses docker"
fi

# ── 2. Neither in-container script hardcodes a runtime ──────────────────────
for f in "$AGENT" "$RELAY"; do
  name="${f#"${ROOT}/"}"
  if hits="$(grep -n 'docker exec' "$f")"; then
    fail "$name has no hardcoded 'docker exec'" "$(head -3 <<<"$hits")"
  else
    pass "$name has no hardcoded 'docker exec'"
  fi
done

# ── 3. The entrypoint's hint renders the runtime it is given ────────────────
HINT_LINE="$(grep -F 'echo "  Tmux:' "$AGENT" | head -1)"

# render_hint <container-name> <runtime-or-empty> — evaluates the shipped echo
# with the same defaulting the entrypoint applies around it. env -i so a
# HIVE_CONTAINER_RUNTIME exported by whoever runs this suite cannot leak in.
render_hint() {
  env -i CONTAINER="$1" RT="${2-}" HINT="$HINT_LINE" bash -c '
    CONTAINER_NAME="${CONTAINER}"
    CONTAINER_RUNTIME="${RT:-docker}"
    TMUX_SESSION="contributor"
    eval "$HINT"
  '
}

if [ -z "$HINT_LINE" ]; then
  fail "extract the Tmux hint from bin/contributor-agent.sh" \
       "the anchor moved — this test cannot verify the real hint"
else
  pass "Tmux hint extracted from bin/contributor-agent.sh (not restated here)"

  GOT="$(render_hint hive-contributor-agy-5b4f podman)"
  WANT='  Tmux:    podman exec -it hive-contributor-agy-5b4f tmux attach -t contributor'
  if [ "$GOT" = "$WANT" ]; then
    pass "a podman launch is told to run podman"
  else
    fail "a podman launch is told to run podman" "got:  $GOT
        want: $WANT"
  fi

  # An older image, or a launch by anything that does not pass the variable,
  # must print exactly what it printed before the fix.
  GOT="$(render_hint hive-contributor '')"
  WANT='  Tmux:    docker exec -it hive-contributor tmux attach -t contributor'
  if [ "$GOT" = "$WANT" ]; then
    pass "with no runtime passed the hint is unchanged (docker)"
  else
    fail "with no runtime passed the hint is unchanged (docker)" "got:  $GOT
        want: $WANT"
  fi
fi

# ── 4. The two hints for the same container agree ──────────────────────────
#
# The reported bug in one assertion: the host-side hint the recipe prints and
# the in-container hint the entrypoint prints must be the same command.
HOST_TPL="$(grep -F 'ATTACH_CMD=' "$JUSTFILE" | head -1)"
if [ -z "$HOST_TPL" ] || [ -z "$HINT_LINE" ]; then
  fail "extract both hints for comparison" "an anchor moved"
else
  HOST_RENDERED="$(env -i TPL="$HOST_TPL" bash -c '
    RUNTIME=podman
    CONTAINER_NAME=hive-contributor-agy-5b4f
    eval "$TPL"
    echo "$ATTACH_CMD"
  ')"
  # Strip the status block's label and column padding: that is layout, not the
  # command an operator pastes.
  CONTAINER_RENDERED="$(render_hint hive-contributor-agy-5b4f podman | sed 's/^ *Tmux: *//')"
  if [ "$HOST_RENDERED" = "$CONTAINER_RENDERED" ]; then
    pass "host-side and in-container hints are the same command"
  else
    fail "host-side and in-container hints are the same command" "host:      $HOST_RENDERED
        container: $CONTAINER_RENDERED"
  fi
fi

echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[ "$FAIL" -eq 0 ]

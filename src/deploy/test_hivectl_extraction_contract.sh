#!/usr/bin/env bash
# Contract test for the hivectl distribution route (#5646).
# Run: bash src/deploy/test_hivectl_extraction_contract.sh
#
# On hosts installed by bin/hive-podman-setup.sh — image-based systems with no
# Go toolchain — the ONLY supported way to obtain hivectl is extraction from
# the image the deployment already runs: src/Dockerfile stows the binary as
# cargo at a well-known non-PATH location, and the installer lifts it onto the
# host with `podman create` + `podman cp`. That route rests on two files
# agreeing on one string, and NOTHING at runtime checks it: an image that quietly
# stopped carrying the binary, or an installer extracting from a path the
# Dockerfile renamed, fails only on an operator's machine, after a green CI run.
# This suite is the coupling made checkable. Pure string analysis: it contacts
# no registry, builds no image, and runs offline.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DOCKERFILE="${ROOT}/src/Dockerfile"
SETUP="${ROOT}/bin/hive-podman-setup.sh"
BOOTSTRAP="${ROOT}/bin/hivectl-bootstrap.sh"
DOCS="${ROOT}/src/docs/hivectl.md"

pass_count=0
failures=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

ok() {
  printf '  PASS: %s\n' "$1"
  pass_count=$((pass_count + 1))
}

# --- 1. The build target is real -------------------------------------------

if [ -f "${ROOT}/src/cmd/hivectl/main.go" ]; then
  ok "src/cmd/hivectl exists — the Dockerfile has something to build"
else
  fail "src/cmd/hivectl/main.go is gone; the extraction route ships nothing"
fi

# --- 2. The builder stage builds hivectl, statically ------------------------
#
# CGO_ENABLED=0 is load-bearing, not style: the extracted binary runs on the
# HOST, whose libc owes nothing to the image's. A dynamically linked hivectl
# would extract fine and then fail to exec on exactly the hosts this exists for.

if grep -qE 'CGO_ENABLED=0 go build[^&]* -o /hivectl \./cmd/hivectl' "$DOCKERFILE"; then
  ok "builder compiles /hivectl from ./cmd/hivectl with CGO_ENABLED=0"
else
  fail "src/Dockerfile does not statically build /hivectl from ./cmd/hivectl"
fi

# --- 3. The image carries it, off PATH --------------------------------------
#
# The COPY's destination is the single string every other assertion hangs off.

image_path="$(sed -n 's|^COPY --from=builder /hivectl \(/[^[:space:]]*\)$|\1|p' "$DOCKERFILE" | head -n1)"
if [ -n "$image_path" ]; then
  ok "runtime stage copies /hivectl into the image at ${image_path}"
else
  fail "no 'COPY --from=builder /hivectl <path>' in src/Dockerfile"
fi

# Off the container's PATH by design: hivectl is a client of the dashboard API
# and must never run inside the thing it inspects (#5646 quotes #4907 on this).
# Cargo in /usr/local/bin would put an operator one `podman exec` away from
# doing exactly that, and would look supported.
case "$image_path" in
  /usr/local/bin/*|/usr/bin/*|/bin/*|/usr/local/sbin/*|/usr/sbin/*|/sbin/*)
    fail "hivectl sits at ${image_path}, on the container's PATH — it is cargo, not runtime"
    ;;
  /*)
    ok "the in-image location is not on the container's PATH — cargo, not runtime"
    ;;
  *)
    fail "could not judge the in-image location ('${image_path}')"
    ;;
esac

# --- 4. The #3760 metacopy rewrite covers it --------------------------------
#
# Every binary that crosses stages via COPY --from gets rewritten in the final
# stage to defeat the overlayfs metacopy redirect. hivectl crosses the same
# way; a redirect-laid file can misbehave under `podman cp` on the fleet's
# runtimes exactly as it did under execve.

rewrite_block="$(grep -B3 -- 'cp --remove-destination' "$DOCKERFILE" || true)"
if [ -n "$image_path" ] && printf '%s\n' "$rewrite_block" | grep -qF "$image_path"; then
  ok "the #3760 fresh-rewrite loop names ${image_path}"
else
  fail "the #3760 fresh-rewrite loop does not cover ${image_path:-<missing>}"
fi

# --- 5. The installer extracts from the SAME path ---------------------------
#
# bin/hive-podman-setup.sh cannot read this path out of a unit the way it reads
# ports, so its constant is pinned here instead: the one place the two
# spellings are compared.

setup_path="$(sed -n 's|^HIVECTL_IMAGE_PATH="\(/[^"]*\)"$|\1|p' "$SETUP" | head -n1)"
if [ -z "$setup_path" ]; then
  fail "bin/hive-podman-setup.sh no longer declares HIVECTL_IMAGE_PATH"
elif [ "$setup_path" = "$image_path" ]; then
  ok "installer extracts from ${setup_path}, exactly where the Dockerfile stows it"
else
  fail "path drift: Dockerfile stows at '${image_path:-<missing>}', installer extracts from '${setup_path}'"
fi

# --- 6. The contributor bootstrap extracts from the SAME path ---------------
#
# just contribute-tui/contribute-hives use the same image cargo path when a
# checkout has no hivectl yet. Pin that third spelling to the Dockerfile too.

bootstrap_path="$(sed -n 's|^HIVECTL_IMAGE_PATH_DEFAULT="\(/[^"]*\)"$|\1|p' "$BOOTSTRAP" | head -n1)"
if [ -z "$bootstrap_path" ]; then
  fail "bin/hivectl-bootstrap.sh no longer declares HIVECTL_IMAGE_PATH_DEFAULT"
elif [ "$bootstrap_path" = "$image_path" ]; then
  ok "contributor bootstrap extracts from ${bootstrap_path}, exactly where the Dockerfile stows it"
else
  fail "path drift: Dockerfile stows at '${image_path:-<missing>}', contributor bootstrap extracts from '${bootstrap_path}'"
fi

# --- 7. The documented manual route names the real path ---------------------
#
# src/docs/hivectl.md leads with extraction (the #5646 acceptance criterion);
# a doc quoting a stale path fails only for the reader.

if [ -n "$image_path" ] && grep -qF "$image_path" "$DOCS"; then
  ok "src/docs/hivectl.md names ${image_path} in the manual extraction route"
else
  fail "src/docs/hivectl.md does not mention ${image_path:-<missing>}"
fi

printf '\n%d passed, %d failed\n' "$pass_count" "$failures"
[ "$failures" -eq 0 ] || exit 1
exit 0

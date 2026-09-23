#!/usr/bin/env bash
# Self-test for cli-pin-bump.sh (#8419). No network: exercises `current`,
# `apply` and `bump` against fixture Dockerfiles, with the resolver replaced by
# a stub, and proves the guards that keep a wrong value out of a Dockerfile:
# a digest of the wrong length, a floating version, a missing ARG line.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$HERE/cli-pin-bump.sh"
PASS=0; FAIL=0
pass() { PASS=$((PASS+1)); echo "  ok: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1"; [ -n "${2:-}" ] && echo "        $2"; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

SHA256_A="$(printf 'a%.0s' $(seq 64))"; readonly SHA256_A
SHA256_B="$(printf 'b%.0s' $(seq 64))"; readonly SHA256_B
SHA512_C="$(printf 'c%.0s' $(seq 128))"; readonly SHA512_C
SHA512_D="$(printf 'd%.0s' $(seq 128))"; readonly SHA512_D

make_fixtures() {
  # A miniature of every ARG line the real Dockerfiles carry for the pinned
  # CLIs, plus an unrelated ARG that must survive untouched.
  cat > "$TMP/Dockerfile" <<'DF'
FROM scratch
ARG TMUX_VERSION=3.5a
ARG GH_VERSION=2.101.0
ARG GH_SHA256_AMD64=0000000000000000000000000000000000000000000000000000000000000000
ARG GH_SHA256_ARM64=1111111111111111111111111111111111111111111111111111111111111111
ARG COPILOT_VERSION=1.0.78
ARG CODEX_VERSION=0.153.4
ARG CLAUDE_CODE_VERSION=2.1.226
ARG GOOSE_VERSION=1.45.0
ARG GOOSE_SHA256_AMD64=2222222222222222222222222222222222222222222222222222222222222222
ARG GOOSE_SHA256_ARM64=3333333333333333333333333333333333333333333333333333333333333333
ARG AGY_VERSION=1.1.19
ARG AGY_BUILD=4894004681244672
ARG AGY_SHA512_AMD64=44444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444444
ARG AGY_SHA512_ARM64=55555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555555
ARG OMP_VERSION=18.2.6
ARG OMP_SHA256_AMD64=6666666666666666666666666666666666666666666666666666666666666666
ARG OMP_SHA256_ARM64=7777777777777777777777777777777777777777777777777777777777777777
ARG MUSE_VERSION=1.0.3-R2198.1
ARG MUSE_SHA256_AMD64=8888888888888888888888888888888888888888888888888888888888888888
ARG MUSE_SHA256_ARM64=9999999999999999999999999999999999999999999999999999999999999999
ARG PI_VERSION=0.84.1
ARG BOBSHELL_VERSION=1.0.6
ARG BOBSHELL_SHA256_AMD64=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
ARG BOBSHELL_SHA256_ARM64=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
RUN echo "${CLAUDE_CODE_VERSION}"
DF
  cat > "$TMP/Dockerfile.contributor" <<'DF'
FROM scratch
ARG CLAUDE_CODE_VERSION=2.1.226
ARG COPILOT_VERSION=1.0.59
ARG KILO_CLI_VERSION=7.5.6
ARG CODEX_VERSION=0.153.4
ARG MUSE_VERSION=1.0.3-R2198.1
ARG MUSE_SHA256_AMD64=8888888888888888888888888888888888888888888888888888888888888888
ARG MUSE_SHA256_ARM64=9999999999999999999999999999999999999999999999999999999999999999
ARG BOBSHELL_VERSION=1.0.6
ARG BOBSHELL_SHA256_AMD64=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
ARG BOBSHELL_SHA256_ARM64=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
ARG GOOSE_VERSION=1.45.0
ARG GOOSE_SHA256_AMD64=2222222222222222222222222222222222222222222222222222222222222222
ARG GOOSE_SHA256_ARM64=3333333333333333333333333333333333333333333333333333333333333333
ARG AGY_VERSION=1.1.22-5711547746615296
ARG AGY_SHA256_AMD64=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
ARG AGY_SHA256_ARM64=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
ARG OMP_VERSION=18.2.6
ARG OMP_SHA256_AMD64=6666666666666666666666666666666666666666666666666666666666666666
ARG OMP_SHA256_ARM64=7777777777777777777777777777777777777777777777777777777777777777
ARG PI_CODING_AGENT_VERSION=0.84.1
DF
  cat > "$TMP/pi_test.go" <<'GO'
	for _, want := range []string{
		"ARG PI_CODING_AGENT_VERSION=0.84.1",
		"@earendil-works/pi-coding-agent@${PI_CODING_AGENT_VERSION}",
GO
}

run() {
  # run <args...>: the script against the fixtures; stdout to $OUT, rc in $RC
  OUT="$(HIVE_PIN_DOCKERFILE="$TMP/Dockerfile" HIVE_PIN_CONTRIB_DOCKERFILE="$TMP/Dockerfile.contributor" \
         HIVE_PIN_PI_TEST="$TMP/pi_test.go" HIVE_PIN_WORKDIR="$TMP/work" \
         bash "$SCRIPT" "$@" 2>"$TMP/stderr")" && RC=0 || RC=$?
  ERR="$(cat "$TMP/stderr")"
}

arg() { grep -E "^ARG $2=" "$TMP/$1" | cut -d= -f2-; }

echo "-- list / current --"
make_fixtures
run list
[ "$OUT" = "$(printf 'claude\ncodex\ncopilot\npi\ngoose\nagy\nomp\nmuse\nbob\ngh')" ] && pass "list prints the ten CLIs in matrix order" || fail "list" "$OUT"
run current claude; [ "$OUT" = "2.1.226" ] && pass "current claude reads the hub pin" || fail "current claude" "$OUT"
run current agy;    [ "$OUT" = "1.1.19" ]  && pass "current agy reads the hub version (not the build)" || fail "current agy" "$OUT"
run current nope;   [ $RC -ne 0 ] && pass "current rejects an unknown CLI" || fail "unknown cli accepted"

echo "-- apply: npm-style CLIs --"
printf 'VERSION=2.1.280\n' > "$TMP/claude.kv"
run apply claude "$TMP/claude.kv"
[ $RC -eq 0 ] && [ "$(arg Dockerfile CLAUDE_CODE_VERSION)" = "2.1.280" ] && [ "$(arg Dockerfile.contributor CLAUDE_CODE_VERSION)" = "2.1.280" ] \
  && pass "claude pin moves in both Dockerfiles" || fail "claude apply" "$ERR"
[ "$(arg Dockerfile TMUX_VERSION)" = "3.5a" ] && [ "$(arg Dockerfile.contributor KILO_CLI_VERSION)" = "7.5.6" ] \
  && pass "unrelated ARG lines are untouched" || fail "collateral edit"
printf 'VERSION=0.87.1\n' > "$TMP/pi.kv"
run apply pi "$TMP/pi.kv"
[ $RC -eq 0 ] && [ "$(arg Dockerfile PI_VERSION)" = "0.87.1" ] && [ "$(arg Dockerfile.contributor PI_CODING_AGENT_VERSION)" = "0.87.1" ] \
  && grep -q '"ARG PI_CODING_AGENT_VERSION=0.87.1"' "$TMP/pi_test.go" \
  && pass "pi moves PI_VERSION, PI_CODING_AGENT_VERSION and the Go test's ARG assertion" || fail "pi apply" "$ERR"

echo "-- apply: download-verified CLIs --"
printf 'VERSION=1.51.0\nSHA256_AMD64=%s\nSHA256_ARM64=%s\n' "$SHA256_A" "$SHA256_B" > "$TMP/goose.kv"
run apply goose "$TMP/goose.kv"
[ $RC -eq 0 ] && [ "$(arg Dockerfile GOOSE_SHA256_AMD64)" = "$SHA256_A" ] && [ "$(arg Dockerfile.contributor GOOSE_SHA256_ARM64)" = "$SHA256_B" ] \
  && [ "$(arg Dockerfile.contributor GOOSE_VERSION)" = "1.51.0" ] \
  && pass "goose writes version + both per-arch digests to both files" || fail "goose apply" "$ERR"
printf 'VERSION=2.102.0\nSHA256_AMD64=%s\nSHA256_ARM64=%s\n' "$SHA256_A" "$SHA256_B" > "$TMP/gh.kv"
run apply gh "$TMP/gh.kv"
[ $RC -eq 0 ] && [ "$(arg Dockerfile GH_VERSION)" = "2.102.0" ] && [ "$(arg Dockerfile GH_SHA256_ARM64)" = "$SHA256_B" ] \
  && ! grep -q GH_VERSION "$TMP/Dockerfile.contributor" \
  && pass "gh is hub-only" || fail "gh apply" "$ERR"
printf 'VERSION=1.2.9\nBUILD=5905287731871744\nSHA256_AMD64=%s\nSHA256_ARM64=%s\nSHA512_AMD64=%s\nSHA512_ARM64=%s\n' \
  "$SHA256_A" "$SHA256_B" "$SHA512_C" "$SHA512_D" > "$TMP/agy.kv"
run apply agy "$TMP/agy.kv"
[ $RC -eq 0 ] && [ "$(arg Dockerfile AGY_VERSION)" = "1.2.9" ] && [ "$(arg Dockerfile AGY_BUILD)" = "5905287731871744" ] \
  && [ "$(arg Dockerfile AGY_SHA512_ARM64)" = "$SHA512_D" ] \
  && [ "$(arg Dockerfile.contributor AGY_VERSION)" = "1.2.9-5905287731871744" ] \
  && [ "$(arg Dockerfile.contributor AGY_SHA256_AMD64)" = "$SHA256_A" ] \
  && pass "agy: hub gets version+build+SHA-512, contributor gets version-build+SHA-256" || fail "agy apply" "$ERR"
printf 'VERSION=2.0.4\nSHA256_AMD64=%s\nSHA256_ARM64=%s\n' "$SHA256_A" "$SHA256_A" > "$TMP/bob.kv"
run apply bob "$TMP/bob.kv"
[ $RC -eq 0 ] && [ "$(arg Dockerfile.contributor BOBSHELL_VERSION)" = "2.0.4" ] && [ "$(arg Dockerfile BOBSHELL_SHA256_ARM64)" = "$SHA256_A" ] \
  && pass "bob writes the shared tarball digest to both arches" || fail "bob apply" "$ERR"
printf 'VERSION=1.3.0-R3401.1\nSHA256_AMD64=%s\nSHA256_ARM64=%s\n' "$SHA256_A" "$SHA256_B" > "$TMP/muse.kv"
run apply muse "$TMP/muse.kv"
[ $RC -eq 0 ] && [ "$(arg Dockerfile MUSE_VERSION)" = "1.3.0-R3401.1" ] && pass "muse accepts the vendor's <semver>-R<build>.<n> version shape" || fail "muse apply" "$ERR"

echo "-- guards --"
make_fixtures
printf 'VERSION=1.51.0\nSHA256_AMD64=deadbeef\nSHA256_ARM64=%s\n' "$SHA256_B" > "$TMP/bad.kv"
run apply goose "$TMP/bad.kv"
[ $RC -ne 0 ] && [ "$(arg Dockerfile GOOSE_SHA256_ARM64)" = "3333333333333333333333333333333333333333333333333333333333333333" ] \
  && pass "a digest that is not 64 hex chars is refused (no partial write of the arm64 value either)" || fail "short digest accepted" "$ERR"
printf 'VERSION=1.51.0\nSHA256_AMD64=%s\nSHA256_ARM64=deadbeef\n' "$SHA256_A" > "$TMP/bad2.kv"
run apply goose "$TMP/bad2.kv"
[ $RC -ne 0 ] && [ "$(arg Dockerfile GOOSE_VERSION)" = "1.45.0" ] && [ "$(arg Dockerfile GOOSE_SHA256_AMD64)" = "2222222222222222222222222222222222222222222222222222222222222222" ] \
  && pass "a bad arm64 digest is refused before the version or amd64 digest is written (validate-then-write)" || fail "partial write on bad arm64 digest" "$ERR"
printf 'VERSION=1.51.0\nSHA256_AMD64=%s\n' "$SHA256_A" > "$TMP/missing.kv"
run apply goose "$TMP/missing.kv"
[ $RC -ne 0 ] && pass "a missing per-arch digest is refused" || fail "missing digest accepted"
printf 'VERSION=latest\n' > "$TMP/latest.kv"
run apply claude "$TMP/latest.kv"
[ $RC -ne 0 ] && [ "$(arg Dockerfile CLAUDE_CODE_VERSION)" = "2.1.226" ] && pass "a floating 'latest' version is refused (#3443)" || fail "latest accepted"
printf 'VERSION=1.2.9\nBUILD=notanumber\nSHA256_AMD64=%s\nSHA256_ARM64=%s\nSHA512_AMD64=%s\nSHA512_ARM64=%s\n' \
  "$SHA256_A" "$SHA256_B" "$SHA512_C" "$SHA512_D" > "$TMP/agybad.kv"
run apply agy "$TMP/agybad.kv"
[ $RC -ne 0 ] && pass "a non-numeric agy build id is refused" || fail "bad agy build accepted"
sed -i.bak '/^ARG OMP_SHA256_ARM64=/d' "$TMP/Dockerfile.contributor"
printf 'VERSION=18.2.11\nSHA256_AMD64=%s\nSHA256_ARM64=%s\n' "$SHA256_A" "$SHA256_B" > "$TMP/omp.kv"
run apply omp "$TMP/omp.kv"
[ $RC -ne 0 ] && printf '%s' "$ERR" | grep -q "no 'ARG OMP_SHA256_ARM64=' line" \
  && pass "a Dockerfile that lost an expected ARG line fails loudly instead of being half-edited" || fail "missing ARG tolerated" "$ERR"

echo "-- bump (resolver stubbed through HIVE_PIN_HTTP) --"
make_fixtures
# The stub serves the npm registry's /latest document for claude.
cat > "$TMP/http-stub.sh" <<'STUB'
#!/usr/bin/env bash
url="$1"; out="$2"
case "$url" in
  *"/@anthropic-ai/claude-code/latest") printf '{"version":"2.1.280"}' > "$out" ;;
  *"/@openai/codex/latest") printf '{"version":"0.153.4"}' > "$out" ;;
  *) echo "stub: unexpected url $url" >&2; exit 1 ;;
esac
STUB
chmod +x "$TMP/http-stub.sh"
export HIVE_PIN_HTTP="$TMP/http-stub.sh"
run bump claude
[ $RC -eq 0 ] && [ "$OUT" = "$(printf 'OLD=2.1.226\nNEW=2.1.280\nMAJOR=false\nCHANGED=true')" ] \
  && [ "$(arg Dockerfile.contributor CLAUDE_CODE_VERSION)" = "2.1.280" ] \
  && pass "bump resolves, applies and reports OLD/NEW/MAJOR/CHANGED" || fail "bump claude" "$OUT $ERR"
run bump codex
[ $RC -eq 0 ] && [ "$OUT" = "$(printf 'OLD=0.153.4\nNEW=0.153.4\nMAJOR=false\nCHANGED=false')" ] \
  && pass "bump reports CHANGED=false when the pin is already current" || fail "bump codex" "$OUT $ERR"
make_fixtures
sed -i.bak 's/^ARG CLAUDE_CODE_VERSION=.*/ARG CLAUDE_CODE_VERSION=1.9.9/' "$TMP/Dockerfile"
run bump claude
[ $RC -eq 0 ] && printf '%s' "$OUT" | grep -q '^MAJOR=true$' && pass "a major-version change is flagged MAJOR=true" || fail "major flag" "$OUT"
unset HIVE_PIN_HTTP

echo
echo "passed: $PASS  failed: $FAIL"
[ "$FAIL" -eq 0 ]

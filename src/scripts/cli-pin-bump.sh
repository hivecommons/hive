#!/usr/bin/env bash
# cli-pin-bump.sh — keep the agent CLI pins in src/Dockerfile and
# src/Dockerfile.contributor current without giving up the pin (#8419).
#
# Every CLI hive drives is installed at a fixed version: npm packages through
# `ARG X_VERSION`, release downloads through `ARG X_VERSION` plus a per-arch
# SHA-256 / SHA-512 that the build checks with `sha256sum -c` before install.
# That pinning is deliberate (#2903) and stays. What was missing is anything
# that MOVES the pins: dependabot's docker updater only follows `FROM` tags, so
# the pins sat until a user hit a wall (#8417: Claude Code 2.1.226 refusing
# claude-opus-5-5 for six weeks).
#
# This script is the mechanical half of the fix. .github/workflows/cli-pin-bump.yml
# runs it daily, builds the image with the new pin, smokes `<cli> --version`,
# and opens one PR per CLI. It never guesses a hash: every digest below is
# computed from the artifact it downloads, and cross-checked against the
# vendor's published digest wherever one exists.
#
# Usage:
#   cli-pin-bump.sh current <cli>            print the version pinned in src/Dockerfile
#   cli-pin-bump.sh resolve <cli> [out]      network: resolve the latest release and
#                                            its digests; writes KEY=VALUE lines to
#                                            <out> (default stdout)
#   cli-pin-bump.sh apply <cli> <kv-file>    edit both Dockerfiles (and the Go test
#                                            that pins pi's ARG line) from a KEY=VALUE
#                                            file; no network
#   cli-pin-bump.sh bump <cli> [out]         resolve + apply; writes OLD=/NEW=/MAJOR=
#                                            /CHANGED= lines to <out> (default stdout);
#                                            exit 0 whether or not anything changed
#   cli-pin-bump.sh list                     print the known CLI names
#
# KEY=VALUE keys: VERSION (always); SHA256_AMD64 / SHA256_ARM64 and
# SHA512_AMD64 / SHA512_ARM64 for download-verified CLIs; BUILD for agy.
#
# Env:
#   HIVE_PIN_ROOT                repo root (default: two levels above this script)
#   HIVE_PIN_DOCKERFILE          override src/Dockerfile
#   HIVE_PIN_CONTRIB_DOCKERFILE  override src/Dockerfile.contributor
#   HIVE_PIN_PI_TEST             override src/pkg/config/dockerfile_contributor_test.go
#   HIVE_PIN_WORKDIR             where downloads land (default: mktemp -d, removed on exit)
#   GH_TOKEN / GITHUB_TOKEN      optional; raises the GitHub API rate limit
#   HIVE_PIN_HTTP                override the fetch command (tests only)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${HIVE_PIN_ROOT:-$(cd "$SCRIPT_DIR/../.." && pwd)}"
DOCKERFILE="${HIVE_PIN_DOCKERFILE:-$ROOT/src/Dockerfile}"
CONTRIB="${HIVE_PIN_CONTRIB_DOCKERFILE:-$ROOT/src/Dockerfile.contributor}"
PI_TEST="${HIVE_PIN_PI_TEST:-$ROOT/src/pkg/config/dockerfile_contributor_test.go}"

# Every CLI this script knows. Order is the order the workflow's matrix uses.
CLIS="claude codex copilot pi goose agy omp muse bob gh"

# Digest lengths in hex characters — a value of any other length is never
# written into a Dockerfile (the "never guess a hash" guard).
readonly SHA256_HEX_LEN=64
readonly SHA512_HEX_LEN=128

# curl budget: a dead vendor endpoint should fail in minutes, not hang the job.
readonly CURL_RETRIES=5
readonly CURL_RETRY_DELAY_SECONDS=10
readonly CURL_CONNECT_TIMEOUT_SECONDS=20
readonly CURL_MAX_TIME_SECONDS=900

# Vendor endpoints.
readonly NPM_REGISTRY="https://registry.npmjs.org"
readonly GITHUB_API="https://api.github.com"
readonly AGY_CASK_URL="https://formulae.brew.sh/api/cask/antigravity-cli.json"
readonly AGY_DOWNLOAD_BASE="https://storage.googleapis.com/antigravity-public/antigravity-cli"
readonly MUSE_CHANNEL_URL="https://api.meta.ai/muse-code/channels/muse-stable"
readonly MUSE_USER_AGENT="muse-code/launcher-3"
readonly BOBSHELL_BASE_URL="https://s3.us-south.cloud-object-storage.appdomain.cloud/bob-shell"
readonly BOBSHELL_VERSION_FILE="bobshell2-version.txt"

die() { printf 'cli-pin-bump: %s\n' "$*" >&2; exit 1; }
log() { printf 'cli-pin-bump: %s\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# Shared helpers
# ---------------------------------------------------------------------------

http() {
  # http <url> <out-file> [extra curl args...]
  local url="$1" out="$2"; shift 2
  if [ -n "${HIVE_PIN_HTTP:-}" ]; then
    "$HIVE_PIN_HTTP" "$url" "$out" "$@"
    return
  fi
  curl -fsSL --retry "$CURL_RETRIES" --retry-delay "$CURL_RETRY_DELAY_SECONDS" \
    --retry-all-errors --connect-timeout "$CURL_CONNECT_TIMEOUT_SECONDS" \
    --max-time "$CURL_MAX_TIME_SECONDS" "$@" -o "$out" "$url"
}

gh_api() {
  # gh_api <path> <out-file>: GitHub REST with the optional token.
  local args=(-H 'Accept: application/vnd.github+json')
  local tok="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
  [ -n "$tok" ] && args+=(-H "Authorization: Bearer $tok")
  http "$GITHUB_API/$1" "$2" "${args[@]}"
}

digest() {
  # digest <256|512> <file>
  local bits="$1" f="$2"
  if command -v "sha${bits}sum" >/dev/null 2>&1; then
    "sha${bits}sum" "$f" | cut -d' ' -f1
  else
    shasum -a "$bits" "$f" | cut -d' ' -f1
  fi
}

require_hex() {
  # require_hex <name> <value> <len>
  local name="$1" v="$2" len="$3"
  [[ "$v" =~ ^[0-9a-f]+$ ]] && [ "${#v}" -eq "$len" ] \
    || die "$name is not a ${len}-hex-char digest: '${v}'"
}

require_version() {
  # A version must look like a release, never a tag like "latest" (#3443).
  local name="$1" v="$2"
  [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.]+)*$ ]] \
    || die "$name is not a release version: '${v}'"
}

cross_check() {
  # cross_check <what> <computed> <published>: the vendor's published digest,
  # when there is one, must equal what we computed from the bytes we fetched.
  local what="$1" got="$2" want="$3"
  [ "$got" = "$want" ] || die "$what: computed digest $got does not match the vendor-published $want"
  log "$what: digest matches the vendor-published value"
}

read_arg() {
  # read_arg <file> <ARG_NAME> -> value
  local f="$1" name="$2" line
  line="$(grep -E "^ARG ${name}=" "$f" || true)"
  [ -n "$line" ] || die "no 'ARG ${name}=' line in $f"
  [ "$(printf '%s\n' "$line" | wc -l | tr -d ' ')" = "1" ] || die "more than one 'ARG ${name}=' line in $f"
  printf '%s\n' "${line#ARG ${name}=}"
}

set_arg() {
  # set_arg <file> <ARG_NAME> <value>: rewrite exactly one `ARG NAME=` line.
  local f="$1" name="$2" value="$3" tmp
  read_arg "$f" "$name" >/dev/null
  tmp="$(mktemp)"
  awk -v name="$name" -v value="$value" '
    index($0, "ARG " name "=") == 1 { print "ARG " name "=" value; next }
    { print }
  ' "$f" > "$tmp"
  cat "$tmp" > "$f"
  rm -f "$tmp"
}

kv_get() {
  # kv_get <kv-file> <KEY> [required]
  local f="$1" key="$2" required="${3:-required}" v
  v="$(grep -E "^${key}=" "$f" | head -1 | cut -d= -f2- || true)"
  if [ -z "$v" ] && [ "$required" = "required" ]; then
    die "resolved data for this CLI has no ${key}="
  fi
  printf '%s\n' "$v"
}

major_of() { printf '%s\n' "${1%%.*}"; }

is_major_bump() {
  # is_major_bump <old> <new> -> prints true/false
  if [ "$(major_of "$1")" != "$(major_of "$2")" ]; then echo true; else echo false; fi
}

# ---------------------------------------------------------------------------
# Resolution: one function per source kind
# ---------------------------------------------------------------------------

resolve_npm() {
  # resolve_npm <package>: the registry's dist-tags.latest
  local pkg="$1" out v
  out="$WORK/npm-$(printf '%s' "$pkg" | tr '/@' '__').json"
  http "$NPM_REGISTRY/$pkg/latest" "$out"
  v="$(jq -r '.version // empty' "$out")"
  [ -n "$v" ] || die "npm registry returned no version for $pkg"
  require_version "$pkg" "$v"
  echo "VERSION=$v"
}

resolve_github_release_tag() {
  # resolve_github_release_tag <owner/repo> -> tag_name of releases/latest
  local out tag
  out="$WORK/release-$(printf '%s' "$1" | tr '/' '_').json"
  gh_api "repos/$1/releases/latest" "$out"
  tag="$(jq -r '.tag_name // empty' "$out")"
  [ -n "$tag" ] || die "GitHub returned no latest release for $1"
  printf '%s\n' "$tag"
}

resolve_goose() {
  local tag v f
  tag="$(resolve_github_release_tag block/goose)"
  v="${tag#v}"; require_version goose "$v"
  echo "VERSION=$v"
  for pair in x86_64:AMD64 aarch64:ARM64; do
    f="$WORK/goose-${pair%%:*}.tar.gz"
    http "https://github.com/block/goose/releases/download/v${v}/goose-${pair%%:*}-unknown-linux-gnu.tar.gz" "$f"
    echo "SHA256_${pair##*:}=$(digest 256 "$f")"
  done
}

resolve_omp() {
  local tag v f sums
  tag="$(resolve_github_release_tag can1357/oh-my-pi)"
  v="${tag#v}"; require_version omp "$v"
  echo "VERSION=$v"
  sums="$WORK/omp-SHA256SUMS.txt"
  http "https://github.com/can1357/oh-my-pi/releases/download/v${v}/SHA256SUMS.txt" "$sums"
  for pair in x64:AMD64 arm64:ARM64; do
    f="$WORK/omp-linux-${pair%%:*}"
    http "https://github.com/can1357/oh-my-pi/releases/download/v${v}/omp-linux-${pair%%:*}" "$f"
    local got want
    got="$(digest 256 "$f")"
    want="$(awk -v n="omp-linux-${pair%%:*}" '$2 == n { print $1 }' "$sums")"
    [ -n "$want" ] || die "omp SHA256SUMS.txt has no entry for omp-linux-${pair%%:*}"
    cross_check "omp-linux-${pair%%:*}" "$got" "$want"
    echo "SHA256_${pair##*:}=$got"
  done
}

resolve_gh() {
  local tag v f sums
  tag="$(resolve_github_release_tag cli/cli)"
  v="${tag#v}"; require_version gh "$v"
  echo "VERSION=$v"
  sums="$WORK/gh_checksums.txt"
  http "https://github.com/cli/cli/releases/download/v${v}/gh_${v}_checksums.txt" "$sums"
  for arch in amd64 arm64; do
    f="$WORK/gh_${arch}.tar.gz"
    http "https://github.com/cli/cli/releases/download/v${v}/gh_${v}_linux_${arch}.tar.gz" "$f"
    local got want
    got="$(digest 256 "$f")"
    want="$(awk -v n="gh_${v}_linux_${arch}.tar.gz" '$2 == n { print $1 }' "$sums")"
    [ -n "$want" ] || die "gh checksums file has no entry for gh_${v}_linux_${arch}.tar.gz"
    cross_check "gh linux/${arch}" "$got" "$want"
    echo "SHA256_$(printf '%s' "$arch" | tr '[:lower:]' '[:upper:]')=$got"
  done
}

resolve_agy() {
  # Google publishes no release index; the Homebrew cask tracks the vendor's
  # updater manifest and is the same artifact the hub image already pins
  # (the Dockerfile.contributor comment says so). Its version is
  # "<semver>,<build>" and it carries per-platform SHA-256s.
  local cask="$WORK/agy-cask.json" ver v build f
  http "$AGY_CASK_URL" "$cask"
  ver="$(jq -r '.version // empty' "$cask")"
  [ -n "$ver" ] || die "antigravity-cli cask has no version"
  v="${ver%%,*}"; build="${ver##*,}"
  require_version agy "$v"
  [[ "$build" =~ ^[0-9]+$ ]] || die "antigravity-cli cask build id is not numeric: '$build'"
  echo "VERSION=$v"
  echo "BUILD=$build"
  for triple in x64:x64:x86_64_linux:AMD64 arm:arm64:arm64_linux:ARM64; do
    IFS=: read -r urlarch filearch variation key <<<"$triple"
    f="$WORK/agy-linux-${filearch}.tar.gz"
    http "$AGY_DOWNLOAD_BASE/${v}-${build}/linux-${urlarch}/cli_linux_${filearch}.tar.gz" "$f"
    local got256 want256
    got256="$(digest 256 "$f")"
    want256="$(jq -r --arg k "$variation" '.variations[$k].sha256 // empty' "$cask")"
    [ -n "$want256" ] || die "antigravity-cli cask has no sha256 for $variation"
    cross_check "agy linux ${filearch}" "$got256" "$want256"
    echo "SHA256_${key}=$got256"
    echo "SHA512_${key}=$(digest 512 "$f")"
  done
}

resolve_muse() {
  # The vendor launcher resolves a channel manifest (version + manifest_url),
  # then a release manifest with per-artifact SHA-256s. Same walk here.
  local chan="$WORK/muse-channel.json" rel="$WORK/muse-release.json" v murl f
  http "$MUSE_CHANNEL_URL" "$chan" -A "$MUSE_USER_AGENT"
  v="$(jq -r '.version // empty' "$chan")"
  murl="$(jq -r '.manifest_url // empty' "$chan")"
  [ -n "$v" ] && [ -n "$murl" ] || die "muse channel manifest has no version/manifest_url"
  require_version muse "$v"
  http "$murl" "$rel" -A "$MUSE_USER_AGENT"
  [ "$(jq -r '.version' "$rel")" = "$v" ] || die "muse release manifest version does not match the channel"
  echo "VERSION=$v"
  for pair in x86_linux:AMD64 aarch64_linux:ARM64; do
    local key="${pair%%:*}" url want got
    url="$(jq -r --arg k "$key" '.artifacts[$k].url // empty' "$rel")"
    want="$(jq -r --arg k "$key" '.artifacts[$k].checksum // empty' "$rel")"
    [ -n "$url" ] && [ -n "$want" ] || die "muse release manifest has no $key artifact"
    f="$WORK/muse-$key"
    http "$url" "$f" -A "$MUSE_USER_AGENT"
    got="$(digest 256 "$f")"
    cross_check "muse $key" "$got" "$want"
    echo "SHA256_${pair##*:}=$got"
  done
}

resolve_bob() {
  # bobshell is not on npm; the vendor's installer reads a version file next
  # to the tarball and a .sha256 beside it. One tarball serves every arch.
  local vf="$WORK/bobshell-version.txt" v f sf got want
  http "$BOBSHELL_BASE_URL/$BOBSHELL_VERSION_FILE" "$vf"
  v="$(tr -d '[:space:]' < "$vf")"
  require_version bobshell "$v"
  echo "VERSION=$v"
  f="$WORK/bobshell-${v}.tgz"; sf="$WORK/bobshell-${v}.tgz.sha256"
  http "$BOBSHELL_BASE_URL/bobshell-${v}.tgz" "$f"
  http "$BOBSHELL_BASE_URL/bobshell-${v}.tgz.sha256" "$sf"
  got="$(digest 256 "$f")"
  want="$(tr -d '[:space:]' < "$sf" | cut -c1-"$SHA256_HEX_LEN")"
  cross_check "bobshell-${v}.tgz" "$got" "$want"
  echo "SHA256_AMD64=$got"
  echo "SHA256_ARM64=$got"
}

resolve_cli() {
  case "$1" in
    claude)  resolve_npm '@anthropic-ai/claude-code' ;;
    codex)   resolve_npm '@openai/codex' ;;
    copilot) resolve_npm '@github/copilot' ;;
    pi)      resolve_npm '@earendil-works/pi-coding-agent' ;;
    goose)   resolve_goose ;;
    agy)     resolve_agy ;;
    omp)     resolve_omp ;;
    muse)    resolve_muse ;;
    bob)     resolve_bob ;;
    gh)      resolve_gh ;;
    *) die "unknown CLI '$1' (known: $CLIS)" ;;
  esac
}

# ---------------------------------------------------------------------------
# Current pin and apply
# ---------------------------------------------------------------------------

hub_version_arg() {
  case "$1" in
    claude)  echo CLAUDE_CODE_VERSION ;;
    codex)   echo CODEX_VERSION ;;
    copilot) echo COPILOT_VERSION ;;
    pi)      echo PI_VERSION ;;
    goose)   echo GOOSE_VERSION ;;
    agy)     echo AGY_VERSION ;;
    omp)     echo OMP_VERSION ;;
    muse)    echo MUSE_VERSION ;;
    bob)     echo BOBSHELL_VERSION ;;
    gh)      echo GH_VERSION ;;
    *) die "unknown CLI '$1' (known: $CLIS)" ;;
  esac
}

current_version() { read_arg "$DOCKERFILE" "$(hub_version_arg "$1")"; }

apply_sha_pair() {
  # apply_sha_pair <file> <PREFIX> <kv-file> <256|512>
  local f="$1" prefix="$2" kv="$3" bits="$4" len a v
  [ "$bits" = 256 ] && len="$SHA256_HEX_LEN" || len="$SHA512_HEX_LEN"
  for a in AMD64 ARM64; do
    v="$(kv_get "$kv" "SHA${bits}_${a}")"
    require_hex "SHA${bits}_${a}" "$v" "$len"
    set_arg "$f" "${prefix}_SHA${bits}_${a}" "$v"
  done
}

validate_kv() {
  # validate_kv <cli> <kv-file>: every value the apply step will write is
  # checked BEFORE the first write, so a bad resolver result never leaves a
  # Dockerfile half-edited (version moved, digest not).
  local cli="$1" kv="$2" v a
  v="$(kv_get "$kv" VERSION)"
  require_version "$cli" "$v"
  case "$cli" in
    goose|omp|muse|bob|gh)
      for a in AMD64 ARM64; do require_hex "SHA256_${a}" "$(kv_get "$kv" "SHA256_${a}")" "$SHA256_HEX_LEN"; done ;;
    agy)
      [[ "$(kv_get "$kv" BUILD)" =~ ^[0-9]+$ ]] || die "agy BUILD is not numeric: '$(kv_get "$kv" BUILD)'"
      for a in AMD64 ARM64; do
        require_hex "SHA256_${a}" "$(kv_get "$kv" "SHA256_${a}")" "$SHA256_HEX_LEN"
        require_hex "SHA512_${a}" "$(kv_get "$kv" "SHA512_${a}")" "$SHA512_HEX_LEN"
      done ;;
  esac
}

apply_cli() {
  # apply_cli <cli> <kv-file>
  local cli="$1" kv="$2" v
  validate_kv "$cli" "$kv"
  v="$(kv_get "$kv" VERSION)"
  case "$cli" in
    claude)  set_arg "$DOCKERFILE" CLAUDE_CODE_VERSION "$v"; set_arg "$CONTRIB" CLAUDE_CODE_VERSION "$v" ;;
    codex)   set_arg "$DOCKERFILE" CODEX_VERSION "$v";       set_arg "$CONTRIB" CODEX_VERSION "$v" ;;
    copilot) set_arg "$DOCKERFILE" COPILOT_VERSION "$v";     set_arg "$CONTRIB" COPILOT_VERSION "$v" ;;
    pi)
      set_arg "$DOCKERFILE" PI_VERSION "$v"
      set_arg "$CONTRIB" PI_CODING_AGENT_VERSION "$v"
      # TestContributorDockerfileInstallsPiWithoutCurlPipeShell asserts the
      # exact ARG line, so the pin and the test move together.
      if [ -f "$PI_TEST" ]; then
        grep -q '"ARG PI_CODING_AGENT_VERSION=' "$PI_TEST" || die "$PI_TEST no longer pins the pi ARG line; update apply_cli"
        local tmp; tmp="$(mktemp)"
        sed -E "s|\"ARG PI_CODING_AGENT_VERSION=[^\"]*\"|\"ARG PI_CODING_AGENT_VERSION=${v}\"|" "$PI_TEST" > "$tmp"
        cat "$tmp" > "$PI_TEST"; rm -f "$tmp"
      fi ;;
    goose)
      for f in "$DOCKERFILE" "$CONTRIB"; do set_arg "$f" GOOSE_VERSION "$v"; apply_sha_pair "$f" GOOSE "$kv" 256; done ;;
    omp)
      for f in "$DOCKERFILE" "$CONTRIB"; do set_arg "$f" OMP_VERSION "$v"; apply_sha_pair "$f" OMP "$kv" 256; done ;;
    muse)
      for f in "$DOCKERFILE" "$CONTRIB"; do set_arg "$f" MUSE_VERSION "$v"; apply_sha_pair "$f" MUSE "$kv" 256; done ;;
    bob)
      for f in "$DOCKERFILE" "$CONTRIB"; do set_arg "$f" BOBSHELL_VERSION "$v"; apply_sha_pair "$f" BOBSHELL "$kv" 256; done ;;
    gh)
      set_arg "$DOCKERFILE" GH_VERSION "$v"; apply_sha_pair "$DOCKERFILE" GH "$kv" 256 ;;
    agy)
      # The two images spell the same release differently: the hub image
      # keeps version and build id in separate ARGs and verifies SHA-512, the
      # contributor image joins them as "<version>-<build>" and verifies
      # SHA-256. Both come from the same downloaded artifact.
      local build; build="$(kv_get "$kv" BUILD)"
      [[ "$build" =~ ^[0-9]+$ ]] || die "agy BUILD is not numeric: '$build'"
      set_arg "$DOCKERFILE" AGY_VERSION "$v"
      set_arg "$DOCKERFILE" AGY_BUILD "$build"
      apply_sha_pair "$DOCKERFILE" AGY "$kv" 512
      set_arg "$CONTRIB" AGY_VERSION "${v}-${build}"
      apply_sha_pair "$CONTRIB" AGY "$kv" 256 ;;
    *) die "unknown CLI '$cli' (known: $CLIS)" ;;
  esac
}

# ---------------------------------------------------------------------------
# Entry points
# ---------------------------------------------------------------------------

cmd_current() { current_version "$1"; }

cmd_resolve() {
  local cli="$1" out="${2:-/dev/stdout}"
  resolve_cli "$cli" > "$out"
}

cmd_apply() {
  local cli="$1" kv="$2"
  [ -f "$kv" ] || die "no such KEY=VALUE file: $kv"
  apply_cli "$cli" "$kv"
}

cmd_bump() {
  local cli="$1" out="${2:-/dev/stdout}" kv old new
  kv="$WORK/$cli.kv"
  old="$(current_version "$cli")"
  resolve_cli "$cli" > "$kv"
  new="$(kv_get "$kv" VERSION)"
  # The hub image pins agy as "<version>" while the resolved value is the same
  # shape, so a plain compare works for every CLI.
  if [ "$old" = "$new" ]; then
    printf 'OLD=%s\nNEW=%s\nMAJOR=false\nCHANGED=false\n' "$old" "$new" > "$out"
    log "$cli: $old is current"
    return 0
  fi
  apply_cli "$cli" "$kv"
  printf 'OLD=%s\nNEW=%s\nMAJOR=%s\nCHANGED=true\n' "$old" "$new" "$(is_major_bump "$old" "$new")" > "$out"
  log "$cli: $old -> $new"
}

main() {
  local cmd="${1:-}"; shift || true
  case "$cmd" in
    list) printf '%s\n' $CLIS ;;
    current) [ $# -eq 1 ] || die "usage: current <cli>"; cmd_current "$1" ;;
    resolve|bump|apply)
      [ $# -ge 1 ] || die "usage: $cmd <cli> ..."
      for c in $CLIS; do [ "$c" = "$1" ] && break; done
      [ "$c" = "$1" ] || die "unknown CLI '$1' (known: $CLIS)"
      WORK="${HIVE_PIN_WORKDIR:-}"
      if [ -z "$WORK" ]; then
        WORK="$(mktemp -d)"
        trap 'rm -rf "$WORK"' EXIT
      fi
      mkdir -p "$WORK"
      case "$cmd" in
        resolve) cmd_resolve "$@" ;;
        apply)   [ $# -eq 2 ] || die "usage: apply <cli> <kv-file>"; cmd_apply "$@" ;;
        bump)    cmd_bump "$@" ;;
      esac ;;
    ''|-h|--help) sed -n '2,40p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; ;;
    *) die "unknown command '$cmd' (list|current|resolve|apply|bump)" ;;
  esac
}

main "$@"

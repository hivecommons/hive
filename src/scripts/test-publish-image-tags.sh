#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
publisher="$script_dir/publish-image-tags.sh"
tmp_root=${TMPDIR:-"$script_dir/../.test-tmp"}
mkdir -p "$tmp_root"
tmp=$(mktemp -d "$tmp_root/publish-tags.XXXXXX")
trap 'rm -rf "$tmp"; rmdir "$tmp_root" 2>/dev/null || true' EXIT
mkdir -p "$tmp/bin" "$tmp/digests"
touch "$tmp/digests/aaaaaaaa" "$tmp/digests/bbbbbbbb"

cat >"$tmp/bin/docker" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
if [[ $1 == buildx && $2 == imagetools && $3 == inspect ]]; then
  ref=${@: -1}
  if [[ -e ${MOCK_CAPTURE}.created ]]; then
    case "${MOCK_INSPECT_MODE:-legacy}" in
      publish-missing-arm64)
        if [[ $* == *'linux/arm64'* ]]; then echo 'null'; else echo '{"config":{"Labels":{}}}'; fi
        ;;
      *) echo '{"config":{"Labels":{}}}' ;;
    esac
    exit 0
  fi
  case "${MOCK_INSPECT_MODE:-legacy}" in
    missing) echo "ERROR: $ref: not found" >&2; exit 1 ;;
    failure) echo 'dial tcp: registry unavailable' >&2; exit 1 ;;
    legacy) echo '{"config":{"Labels":{}}}' ;;
    sha-missing-moving-newer)
      if [[ $ref == *':abcdef1' ]]; then echo 'manifest unknown' >&2; exit 1; fi
      printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"101"}}}\n'
      ;;
    mixed)
      if [[ $ref == *':latest' ]]; then value=101; else value=99; fi
      printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"%s"}}}\n' "$value"
      ;;
    publish-missing-arm64) echo '{"config":{"Labels":{}}}' ;;
    *) printf '{"config":{"Labels":{"io.kubestellar.hive.github-actions-run-number":"%s"}}}\n' "$MOCK_INSPECT_MODE" ;;
  esac
  exit 0
fi
printf '%q ' "$@" >"$MOCK_CAPTURE"
printf '\n' >>"$MOCK_CAPTURE"
touch "${MOCK_CAPTURE}.created"
MOCK
chmod +x "$tmp/bin/docker"

run_case() {
  local mode=$1 run=$2 capture=$3
  PATH="$tmp/bin:$PATH" MOCK_INSPECT_MODE=$mode MOCK_CAPTURE="$capture" \
    "$publisher" ghcr.io/hivecommons/hive "$tmp/digests" v4 abcdef123456 "$run" v4 false
}

run_custom_case() {
  local mode=$1 run=$2 capture=$3 branch=$4 include_latest=$5
  PATH="$tmp/bin:$PATH" MOCK_INSPECT_MODE=$mode MOCK_CAPTURE="$capture" \
    "$publisher" ghcr.io/hivecommons/hive-hub "$tmp/digests" "$branch" abcdef123456 "$run" v4 "$include_latest"
}

capture="$tmp/create-new"
run_case missing 100 "$capture"
grep -q 'hive:abcdef1' "$capture"
grep -q 'hive:v4-latest' "$capture"
if grep -q 'hive:stable' "$capture"; then
  echo "v4 merge publish moved stable instead of candidate only" >&2
  exit 1
fi

capture="$tmp/create-forward"
run_case 99 100 "$capture"
grep -q 'hive:v4-latest' "$capture"

capture="$tmp/create-stale"
run_case sha-missing-moving-newer 100 "$capture"
grep -q 'hive:abcdef1' "$capture"
if grep -q 'hive:v4-latest\|hive:stable\|hive:candidate\|hive:edge' "$capture"; then
  echo "stale run moved a mutable tag" >&2
  exit 1
fi

capture="$tmp/create-idempotent"
run_case 100 100 "$capture"
if [ -e "$capture" ]; then
  echo "rerun of an already-published workflow generation published tags again" >&2
  exit 1
fi

capture="$tmp/create-legacy"
run_case legacy 100 "$capture"
grep -q 'hive:v4-latest' "$capture"

if run_case failure 100 "$tmp/create-failure"; then
  echo "registry inspection failure did not fail closed" >&2
  exit 1
fi

if run_case publish-missing-arm64 100 "$tmp/create-missing-arm64"; then
  echo "post-publish platform assertion did not fail closed" >&2
  exit 1
fi

capture="$tmp/create-feature"
run_custom_case missing 100 "$capture" feat/demo true
grep -q 'hive-hub:feat-demo-latest' "$capture"
grep -q 'hive-hub:latest' "$capture"
if grep -q 'hive-hub:stable\|hive-hub:candidate\|hive-hub:edge' "$capture"; then
  echo "non-release branch moved a release channel" >&2
  exit 1
fi

# Moving tags are independent: a newer global :latest published from another
# branch must not block this branch's own -latest tag.
capture="$tmp/create-partial"
run_custom_case mixed 100 "$capture" feat/demo true
grep -q 'hive-hub:feat-demo-latest' "$capture"
if grep -q 'hive-hub:latest' "$capture"; then
  echo "older run regressed the independently newer global latest tag" >&2
  exit 1
fi

# Pin all three workflow integrations. The publisher test alone would still
# pass if a later workflow edit bypassed it or forgot to stamp build metadata.
workflow="$script_dir/../../.github/workflows/docker.yml"
[[ $(grep -c 'io.kubestellar.hive.github-actions-run-number=' "$workflow") -eq 3 ]]
[[ $(grep -c 'src/scripts/publish-image-tags.sh' "$workflow") -eq 3 ]]
grep -q 'v5 true edge' "$workflow"
if grep -q 'v5 true stable\|CHANNELS: "stable' "$workflow"; then
  echo "docker workflow publishes v4-owned stable from the v5 lane" >&2
  exit 1
fi
grep -q 'INCLUDE_LATEST: "true"' "$workflow"
if grep -q 'head-check\|Verify build commit is still HEAD' "$workflow"; then
  echo "HEAD-only publication guard was reintroduced" >&2
  exit 1
fi

echo "publish-image-tags tests: PASS"

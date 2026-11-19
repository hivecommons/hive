#!/usr/bin/env bash
# test-refresh-only-head.sh - exercise .github/scripts/refresh-only-head.sh
# against a local git fixture (served over file://) and a stubbed `gh`.
# Every negative case must answer refresh_only=false: false means "run
# everything", so a regression that answers true wrongly skips CI.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SCRIPT="$HERE/refresh-only-head.sh"
BOT='hivecommons-hive[bot]'
WF='.github/workflows/docker.yml'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

# --- git fixture: owner/repo.git served at file://$tmp/srv ---------------
srv="$tmp/srv"
repo="$srv/owner/repo.git"
mkdir -p "$repo"
git -C "$repo" init -q -b v5
gitc() { git -C "$repo" -c user.name=t -c user.email=t@example.com -c commit.gpgsign=false "$@"; }
gitc config uploadpack.allowFilter true
gitc config uploadpack.allowAnySHA1InWant true
put() { mkdir -p "$repo/$(dirname "$1")"; printf '%s\n' "$2" > "$repo/$1"; gitc add "$1"; }
put a.txt base; gitc commit -qm A
A=$(gitc rev-parse HEAD)
gitc checkout -qb pr
put pr.txt change; gitc commit -qm P
P=$(gitc rev-parse HEAD)
gitc checkout -q v5
put base.txt one; gitc commit -qm B
B=$(gitc rev-parse HEAD)
# M: clean refresh of P onto B.
gitc checkout -q pr
gitc merge -q --no-ff --no-edit v5
M=$(gitc rev-parse HEAD)
# B2 / M2: second clean refresh of M.
gitc checkout -q v5
put base2.txt two; gitc commit -qm B2
B2=$(gitc rev-parse HEAD)
gitc checkout -q pr
gitc merge -q --no-ff --no-edit v5
M2=$(gitc rev-parse HEAD)
# X: a refresh whose merge also changed PR content.
gitc checkout -q -b evil "$P"
gitc merge -q --no-ff --no-commit "$B" || true
put pr.txt altered
gitc commit -qm X
X=$(gitc rev-parse HEAD)
# D: a refresh whose base delta touches a Dockerfile.
gitc checkout -q -b v5d "$B"
put src/Dockerfile 'FROM scratch'; gitc commit -qm BD
BD=$(gitc rev-parse HEAD)
gitc checkout -q -b prd "$P"
gitc merge -q --no-ff --no-edit "$BD"
D=$(gitc rev-parse HEAD)
# One refresh of P per kind of base delta: base commit B<kind> on a branch
# off A (so the delta holds only its own files), refresh R<kind> merging it
# into P. Arguments after the kind are path/content pairs.
refresh_with() {
  local kind=$1; shift
  gitc checkout -q -b "v5$kind" "$A"
  while [ "$#" -ge 2 ]; do put "$1" "$2"; shift 2; done
  gitc commit -qm "B$kind"
  printf -v "B$kind" '%s' "$(gitc rev-parse HEAD)"
  gitc checkout -q -b "pr$kind" "$P"
  gitc merge -q --no-ff --no-edit "v5$kind"
  printf -v "R$kind" '%s' "$(gitc rev-parse HEAD)"
}
refresh_with doc docs/guide.md docs changelog.d/fixed-x.md '- x' src/docs/y.md y CHANGELOG.md c README.md r
refresh_with mk Makefile 'all:'
refresh_with go src/pkg/x/x.go 'package x'
refresh_with pkg web/ui/package-lock.json '{}'
# The base branch the script fetches: v5 must contain every base parent.
gitc checkout -q v5
for b in "$BD" "$Bdoc" "$Bmk" "$Bgo" "$Bpkg"; do gitc merge -q --no-ff --no-edit "$b"; done

# --- gh stub: answers from $tmp/fix ------------------------------------
fix="$tmp/fix"
mkdir -p "$fix" "$tmp/bin"
cat > "$tmp/bin/gh" <<'STUB'
#!/usr/bin/env bash
[ "$1" = api ] || exit 1
path=$2; shift 2
jqexpr=''
if [ "${1:-}" = --jq ]; then jqexpr=$2; fi
case "$path" in
  repos/*/commits/*) f="$FIX/commit-${path##*/}.json" ;;
  repos/*/actions/runs\?head_sha=*) sha=${path#*head_sha=}; sha=${sha%%&*}; f="$FIX/runs-$sha.json"
    [ -f "$f" ] || { echo '{"workflow_runs":[]}' > "$f"; } ;;
  *) exit 1 ;;
esac
[ -f "$f" ] || exit 1
if [ -n "$jqexpr" ]; then jq -r "$jqexpr" "$f"; else cat "$f"; fi
STUB
chmod +x "$tmp/bin/gh"

commit() { # sha verified author committer parent1 parent2
  printf '{"commit":{"verification":{"verified":%s}},"author":{"login":"%s"},"committer":{"login":"%s"},"parents":[{"sha":"%s"},{"sha":"%s"}]}\n' \
    "$2" "$3" "$4" "$5" "$6" > "$fix/commit-$1.json"
}
runs() { # sha conclusion
  printf '{"workflow_runs":[{"path":"%s","conclusion":"%s"}]}\n' "$WF" "$2" > "$fix/runs-$1.json"
}
reset() { rm -f "$fix"/*.json; }

check() { # name want before head [extra env...]
  local name=$1 want=$2 before=$3 head=$4 got
  shift 4
  got=$(env -i PATH="$tmp/bin:$PATH" HOME="$tmp" FIX="$fix" GITHUB_SERVER_URL="file://$srv" \
    EVENT_NAME=pull_request EVENT_ACTION=synchronize BEFORE="$before" HEAD_SHA="$head" \
    BASE_REF=v5 HEAD_REPO=owner/repo REPO=owner/repo WORKFLOW_PATH="$WF" GH_TOKEN=t \
    GITHUB_OUTPUT="$tmp/out" GITHUB_STEP_SUMMARY="$tmp/summary" "$@" bash "$SCRIPT" | sed -n 's/^refresh_only=\([a-z]*\):.*/\1/p')
  if [ "$got" = "$want" ]; then
    echo "ok   $name"
  else
    echo "FAIL $name: want $want, got '${got}'"; sed 's/^/     /' "$tmp/summary" 2>/dev/null || true
    fail=1
  fi
  rm -f "$tmp/out" "$tmp/summary"
}

reset; commit "$M" true "$BOT" web-flow "$P" "$B"; runs "$P" success
check "clean refresh of a passing head" true "$P" "$M"
check "non-synchronize event" false "$P" "$M" EVENT_ACTION=opened
check "fork head" false "$P" "$M" HEAD_REPO=fork/repo
check "zero previous head" false 0000000000000000000000000000000000000000 "$M"
check "no token" false "$P" "$M" GH_TOKEN=
check "previous head not a parent" false "$A" "$M"

reset; commit "$M" false "$BOT" web-flow "$P" "$B"; runs "$P" success
check "unverified signature" false "$P" "$M"

reset; commit "$M" true someone-else web-flow "$P" "$B"; runs "$P" success
check "not authored by the App" false "$P" "$M"

reset; commit "$M" true "$BOT" someone-else "$P" "$B"; runs "$P" success
check "not committed by web-flow" false "$P" "$M"

reset; commit "$M" true "$BOT" web-flow "$P" "$B"; runs "$P" cancelled
check "previous head cancelled, not a refresh" false "$P" "$M"

reset; commit "$X" true "$BOT" web-flow "$P" "$B"; runs "$P" success
check "merge altered PR content" false "$P" "$X"

reset; commit "$D" true "$BOT" web-flow "$P" "$BD"; runs "$P" success
check "base delta touches a Dockerfile" false "$P" "$D"

reset; commit "$M2" true "$BOT" web-flow "$M" "$B2"; commit "$M" true "$BOT" web-flow "$P" "$B"
runs "$M" cancelled; runs "$P" success
check "two refresh hops back to a passing head" true "$M" "$M2"

reset; commit "$M2" true "$BOT" web-flow "$M" "$B2"; commit "$M" false "$BOT" web-flow "$P" "$B"
runs "$M" cancelled; runs "$P" success
check "unverified hop in the walk" false "$M" "$M2"

# docs-only (docker.yml): every base-delta path must be docs or changelog.
reset; commit "$M" true "$BOT" web-flow "$P" "$B"; runs "$P" success
check "docs-only: base delta with a plain file runs" false "$P" "$M" DELTA_POLICY=docs-only
reset; commit "$Rdoc" true "$BOT" web-flow "$P" "$Bdoc"; runs "$P" success
check "docs-only: docs and changelog delta may skip" true "$P" "$Rdoc" DELTA_POLICY=docs-only
reset; commit "$Rmk" true "$BOT" web-flow "$P" "$Bmk"; runs "$P" success
check "docs-only: Makefile in the delta runs" false "$P" "$Rmk" DELTA_POLICY=docs-only
check "deny: Makefile in the delta may skip" true "$P" "$Rmk"
reset; commit "$Rgo" true "$BOT" web-flow "$P" "$Bgo"; runs "$P" success
check "docs-only: .go file in the delta runs" false "$P" "$Rgo" DELTA_POLICY=docs-only
reset; commit "$Rpkg" true "$BOT" web-flow "$P" "$Bpkg"; runs "$P" success
check "deny: nested package-lock.json in the delta runs" false "$P" "$Rpkg"
check "unknown policy runs" false "$P" "$Rpkg" DELTA_POLICY=bogus

exit "$fail"

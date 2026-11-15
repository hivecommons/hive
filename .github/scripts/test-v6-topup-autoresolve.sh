#!/usr/bin/env bash
# test-v6-topup-autoresolve.sh — exercises .github/scripts/v6-topup-autoresolve.sh
# against throwaway repositories that reproduce each known v5 -> v6 conflict
# pattern, plus the cases that must still fail for a human.
set -euo pipefail

RESOLVER="$(cd "$(dirname "$0")" && pwd)/v6-topup-autoresolve.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail=0

pin_line() { printf "const TRUSTED_SCRIPTS_TREE = '%s';\n" "$1"; }
zeros=0000000000000000000000000000000000000000

# new_repo DIR: a base commit on v5 with the files every pattern touches,
# then a v6 branch. Leaves the repo on v5.
new_repo() {
  local d="$1"
  git init -q -b v5 "$d"
  cd "$d"
  git config user.name test
  git config user.email test@example.com
  mkdir -p .github/scripts src/scripts src/pkg/config src/pkg/dashboard/static-src src/pkg/dashboard/static discord
  printf '# Changelog\n\n## Unreleased\n\n## 1.0.0\n\n- base\n' > CHANGELOG.md
  { echo "'use strict';"; pin_line "$zeros"; echo "const OTHER = 1;"; } > .github/scripts/integration-governance.cjs
  echo base > src/scripts/a.sh
  echo 'package config // base' > src/pkg/config/config.go
  printf 'a.html\n# comment\n\nb.html\n' > src/pkg/dashboard/static-src/manifest.txt
  echo A > src/pkg/dashboard/static-src/a.html
  echo B > src/pkg/dashboard/static-src/b.html
  printf 'A\nB\n' > src/pkg/dashboard/static/index.html
  echo bot > discord/bot.go
  echo other > other.txt
  git add -A && git commit -qm base
  git checkout -qb v6
  git rm -q -r discord && git commit -qm "v6 drops discord"
  git checkout -q v5
}

finish_pin() {
  # Re-pin both sides honestly so each branch is self-consistent.
  local tree
  tree="$(git rev-parse HEAD:src/scripts)"
  { echo "'use strict';"; pin_line "$tree"; echo "const OTHER = 1;"; } > .github/scripts/integration-governance.cjs
  git add -A && git commit -qm "$1 re-pin"
}

expect() {
  local name="$1" want="$2"
  shift 2
  local rc=0
  ( "$@" ) >"$WORK/$name.log" 2>&1 || rc=$?
  if { [ "$want" = ok ] && [ "$rc" -ne 0 ]; } || { [ "$want" = fail ] && [ "$rc" -eq 0 ]; }; then
    echo "FAIL $name (want $want, rc=$rc)"; sed 's/^/  /' "$WORK/$name.log"; fail=1
  else
    echo "ok   $name"
  fi
}

case_all_known() {
  new_repo "$WORK/known" && cd "$WORK/known"
  # v5 side
  echo v5bot > discord/bot.go
  printf '# Changelog\n\n## Unreleased\n\n## 1.1.0 (v5)\n\n- v5 entry\n\n## 1.0.0\n\n- base\n' > CHANGELOG.md
  echo v5 > src/scripts/b.sh
  echo 'package config // v5 monolith edit' > src/pkg/config/config.go
  echo A5 > src/pkg/dashboard/static-src/a.html
  printf 'A5\nB\n' > src/pkg/dashboard/static/index.html
  git add -A && git commit -qm v5 && finish_pin v5
  # v6 side
  git checkout -q v6
  printf '# Changelog\n\n## Unreleased\n\n## 6.1.0 (v6)\n\n- v6 entry\n\n## 1.0.0\n\n- base\n' > CHANGELOG.md
  echo v6 > src/scripts/c.sh
  echo 'package config // v6 slim' > src/pkg/config/config.go
  echo B6 > src/pkg/dashboard/static-src/b.html
  printf 'A\nB6\n' > src/pkg/dashboard/static/index.html
  git add -A && git commit -qm v6 && finish_pin v6
  git merge --no-ff -q v5 -m merge >/dev/null 2>&1 && { echo "expected conflicts"; return 1; }
  bash "$RESOLVER"
  git commit -q --no-edit
  [ ! -e discord/bot.go ] || { echo "discord not deleted"; return 1; }
  grep -q 'v6 slim' src/pkg/config/config.go || { echo "config.go not v6"; return 1; }
  grep -q '6.1.0 (v6)' CHANGELOG.md && grep -q '1.1.0 (v5)' CHANGELOG.md || { echo "changelog lost a side"; return 1; }
  ! grep -qE '^(<<<<<<<|=======|>>>>>>>)' CHANGELOG.md .github/scripts/integration-governance.cjs || { echo "markers left"; return 1; }
  [ "$(cat src/pkg/dashboard/static/index.html)" = "$(printf 'A5\nB6')" ] || { echo "index not regenerated"; return 1; }
  grep -qF "$(pin_line "$(git rev-parse HEAD:src/scripts)")" .github/scripts/integration-governance.cjs || { echo "pin not merged tree"; return 1; }
  grep -q 'const OTHER = 1;' .github/scripts/integration-governance.cjs || { echo "pin file lost lines"; return 1; }
}

case_unknown_conflict() {
  new_repo "$WORK/unknown" && cd "$WORK/unknown"
  echo v5 > other.txt && git commit -qam v5
  git checkout -q v6 && echo v6 > other.txt && git commit -qam v6
  git merge --no-ff -q v5 -m merge >/dev/null 2>&1 || true
  bash "$RESOLVER"
}

case_pin_file_real_edit() {
  new_repo "$WORK/pinedit" && cd "$WORK/pinedit"
  sed -i.bak 's/const OTHER = 1;/const OTHER = 5;/' .github/scripts/integration-governance.cjs && rm -f .github/scripts/integration-governance.cjs.bak
  git commit -qam v5
  git checkout -q v6
  sed -i.bak 's/const OTHER = 1;/const OTHER = 6;/' .github/scripts/integration-governance.cjs && rm -f .github/scripts/integration-governance.cjs.bak
  git commit -qam v6
  git merge --no-ff -q v5 -m merge >/dev/null 2>&1 || true
  bash "$RESOLVER"
}

expect all-known-patterns-resolve ok case_all_known
expect unknown-conflict-refused fail case_unknown_conflict
expect pin-file-real-edit-refused fail case_pin_file_real_edit

exit "$fail"

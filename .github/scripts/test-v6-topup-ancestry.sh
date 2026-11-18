#!/usr/bin/env bash
# test-v6-topup-ancestry.sh — exercises .github/scripts/v6-topup-ancestry.sh
# against throwaway repositories: a merge-commit top-up (ancestry intact), a
# squash-merged top-up (ancestry lost, #18931), the ours-merge heal that makes
# the next forward-merge conflict-free again, and the forged-marker cases the
# heal must refuse.
set -euo pipefail

ANCESTRY="$(cd "$(dirname "$0")" && pwd)/v6-topup-ancestry.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
fail=0
export V5_REF=v5 V6_REF=v6

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

# new_repo DIR: base commit on v5 plus a v6 branch with one adapted file.
new_repo() {
  git init -q -b v5 "$1"
  cd "$1"
  git config user.name test
  git config user.email test@example.com
  echo base > shared.txt
  git add -A && git commit -qm base
  git branch v6
}

# topup_pr: build the bot's top-up head (merge of v5 into v6) on sync/v5-to-v6
# and leave the repo on v6. Prints nothing; sets HEAD_SHA and CARRIED.
topup_pr() {
  CARRIED="$(git rev-parse v5)"
  git checkout -q -B sync/v5-to-v6 v6
  # -X theirs: v5's side wins every overlap, standing in for the human resolution.
  git merge --no-ff -q -X theirs --signoff v5 -m "topup" >/dev/null
  HEAD_SHA="$(git rev-parse HEAD)"
  git checkout -q v6
}

# v5 and v6 both touch shared.txt in the carried range (v6 adapts it), then a
# top-up PR carries v5. Leaves v5 one commit past the carried head.
scenario_lost() {
  new_repo "$1"
  echo v5-one > shared.txt && git commit -qam "v5 one"
  echo v5-only > newfile.txt && git add -A && git commit -qm "v5 adds newfile"
  git checkout -q v6
  echo v6-adapted > shared.txt && git commit -qam "v6 adapts"
  topup_pr
  # Squash-merge the PR: v6 gets the content as ONE single-parent commit.
  git merge --squash -q sync/v5-to-v6 >/dev/null 2>&1 || true
  git commit -qm "squash of the top-up" || true
  MERGED_SHA="$(git rev-parse v6)"
  # v5 moves on over the same lines.
  git checkout -q v5
  echo v5-two > shared.txt && git commit -qam "v5 two"
  echo v5-two-new > newfile.txt && git commit -qam "v5 two newfile"
  git checkout -q v6
}

case_marker_roundtrip() {
  new_repo "$WORK/m" && cd "$WORK/m"
  local sha=0123456789abcdef0123456789abcdef01234567 line
  line="$(bash "$ANCESTRY" marker-line "$sha")"
  [ "$line" = "<!-- hive:topup carried=$sha -->" ] || { echo "bad marker line: $line"; return 1; }
  printf 'text\n%s\nmore\n' "$line" > "$WORK/body.md"
  [ "$(bash "$ANCESTRY" marker "$WORK/body.md")" = "$sha" ] || { echo "marker not read back"; return 1; }
  printf 'no marker here\n<!-- hive:topup carried=NOTASHA -->\n' > "$WORK/body2.md"
  [ -z "$(bash "$ANCESTRY" marker "$WORK/body2.md")" ] || { echo "malformed marker accepted"; return 1; }
  bash "$ANCESTRY" marker-line abc && { echo "short sha accepted"; return 1; }
  return 0
}

case_level_after_merge_commit() {
  new_repo "$WORK/lvl" && cd "$WORK/lvl"
  echo v5-one > newfile.txt && git add -A && git commit -qm "v5 one"
  git checkout -q v6
  topup_pr
  git merge --no-ff -q sync/v5-to-v6 -m "merge commit landing" >/dev/null
  [ "$(bash "$ANCESTRY" classify "$CARRIED")" = level ] || { echo "merge-commit landing not level"; return 1; }
}

case_lost_after_squash() {
  scenario_lost "$WORK/lost" && cd "$WORK/lost"
  [ "$(bash "$ANCESTRY" classify "$CARRIED")" = lost ] || { echo "squash landing not detected"; return 1; }
  bash "$ANCESTRY" verify "$CARRIED" "$HEAD_SHA" "$MERGED_SHA" || { echo "genuine marker refused"; return 1; }
}

case_unknown_without_marker() {
  scenario_lost "$WORK/unk" && cd "$WORK/unk"
  [ "$(bash "$ANCESTRY" classify "")" = unknown ] || { echo "empty carried not unknown"; return 1; }
}

# Without the heal the next forward-merge conflicts; with the ours-merge it is
# clean and v5 ends up an ancestor of the result.
case_heal_restores_clean_merge() {
  scenario_lost "$WORK/heal" && cd "$WORK/heal"
  git checkout -q -B plain v6
  git merge --no-ff -q --signoff v5 -m plain >/dev/null 2>&1 && { echo "expected the un-healed merge to conflict"; return 1; }
  git merge --abort
  git checkout -q -B healed v6
  git merge -s ours --no-ff --signoff "$CARRIED" -m "restore ancestry" >/dev/null
  [ "$(git rev-parse 'HEAD^{tree}')" = "$(git rev-parse 'v6^{tree}')" ] || { echo "ours-merge changed the tree"; return 1; }
  git merge --no-ff -q --signoff v5 -m healed >/dev/null 2>&1 || { echo "healed merge still conflicts"; return 1; }
  git merge-base --is-ancestor v5 HEAD || { echo "v5 is not an ancestor after the heal"; return 1; }
  git log -1 --format=%B HEAD~1 | grep -q '^Signed-off-by: test <test@example.com>$' || { echo "heal commit not signed off"; return 1; }
}

case_forged_marker_refused() {
  scenario_lost "$WORK/forged" && cd "$WORK/forged"
  # A v5 commit the top-up did NOT carry: ours-merging it would drop its content.
  local uncarried
  uncarried="$(git rev-parse v5)"
  bash "$ANCESTRY" verify "$uncarried" "$HEAD_SHA" "$MERGED_SHA" && { echo "uncarried head accepted"; return 1; }
  # The PR's merge commit must be on v6.
  bash "$ANCESTRY" verify "$CARRIED" "$HEAD_SHA" "$uncarried" && { echo "merge commit off v6 accepted"; return 1; }
  # The PR head must be a two-parent merge.
  bash "$ANCESTRY" verify "$CARRIED" "$CARRIED" "$MERGED_SHA" && { echo "non-merge head accepted"; return 1; }
  return 0
}

case_unrecoverable_off_v5() {
  scenario_lost "$WORK/off" && cd "$WORK/off"
  git checkout -q -b stray v5 && echo s > s.txt && git add -A && git commit -qm stray
  [ "$(bash "$ANCESTRY" classify "$(git rev-parse stray)")" = unrecoverable ] || { echo "off-v5 head not unrecoverable"; return 1; }
  [ "$(bash "$ANCESTRY" classify 0000000000000000000000000000000000000000)" = unrecoverable ] || { echo "missing commit not unrecoverable"; return 1; }
}

expect marker-roundtrip ok case_marker_roundtrip
expect level-after-merge-commit ok case_level_after_merge_commit
expect lost-after-squash ok case_lost_after_squash
expect unknown-without-marker ok case_unknown_without_marker
expect heal-restores-clean-merge ok case_heal_restores_clean_merge
expect forged-marker-refused ok case_forged_marker_refused
expect unrecoverable-off-v5 ok case_unrecoverable_off_v5

exit "$fail"

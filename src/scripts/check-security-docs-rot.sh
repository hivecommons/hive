#!/usr/bin/env bash
# check-security-docs-rot.sh — fail when a security document has gone stale.
#
# Two checks, over the same fixed file list as check-security-docs-readability.sh:
#
#   1. Review date. Each file must carry a `Last reviewed: YYYY-MM-DD` line in
#      its first 15 lines, and that date must be no older than MAX_AGE_DAYS
#      (default 120). Bump the date when you re-read the page end to end, not
#      when you touch one sentence.
#
#   2. Closed "tracked in" citations. Any sentence that says an item is
#      "tracked in", a "tracking issue", "open work", "is planned" or
#      "in progress" and cites #NNNN within 80 characters is checked against
#      GitHub. If that issue or PR is CLOSED or MERGED the sentence is rot:
#      rewrite it to say what landed, or point at the live successor. This part
#      needs a token (GH_TOKEN or GITHUB_TOKEN); without one it prints a notice
#      and is skipped so the script still works offline.
#
# Pure bash + python3 standard library + gh. Run from the repo root.

set -euo pipefail

cd "$(dirname "$0")/../.."

MAX_AGE_DAYS="${MAX_AGE_DAYS:-120}"
REPO="${SECURITY_DOCS_REPO:-hivecommons/hive}"

FILES=(
  SECURITY.md
  src/docs/security-self-assessment.md
  src/docs/security-response.md
  src/docs/security-threat-model.md
  src/docs/security-model.md
  src/docs/securing-your-hive.md
  src/docs/ioscan.md
  src/docs/ioscan-red-team.md
)

failed=0

# --- 1. review date -------------------------------------------------------

today_epoch=$(date -u +%s)
for f in "${FILES[@]}"; do
  if [ ! -f "$f" ]; then
    echo "FAIL $f: file missing"
    failed=$((failed + 1))
    continue
  fi
  stamp=$(head -n 15 "$f" | sed -nE 's/^[*_ ]*Last reviewed: ([0-9]{4}-[0-9]{2}-[0-9]{2}).*$/\1/p' | head -n 1)
  if [ -z "$stamp" ]; then
    echo "FAIL $f: no 'Last reviewed: YYYY-MM-DD' line in the first 15 lines"
    failed=$((failed + 1))
    continue
  fi
  stamp_epoch=$(python3 -c 'import sys,datetime;print(int(datetime.datetime.strptime(sys.argv[1],"%Y-%m-%d").replace(tzinfo=datetime.timezone.utc).timestamp()))' "$stamp")
  age_days=$(( (today_epoch - stamp_epoch) / 86400 ))
  if [ "$age_days" -gt "$MAX_AGE_DAYS" ]; then
    echo "FAIL $f: last reviewed $stamp, $age_days days ago (max $MAX_AGE_DAYS)"
    failed=$((failed + 1))
  else
    echo "ok   $f: last reviewed $stamp ($age_days days ago)"
  fi
done

# --- 2. closed "tracked in" citations -------------------------------------

# Prints "file<TAB>issue<TAB>phrase" for every tracking phrase that cites an
# issue number within 80 characters, with the text unwrapped first so a
# citation split across a line break is still seen.
citations=$(python3 - "${FILES[@]}" <<'PY'
import re
import sys

pat = re.compile(
    r"(tracked in|tracking issue|open work|is planned|in progress)[^.]{0,80}?#(\d+)",
    re.I,
)
for path in sys.argv[1:]:
    try:
        text = open(path, encoding="utf-8").read()
    except OSError:
        continue
    flat = re.sub(r"\s+", " ", text)
    for m in pat.finditer(flat):
        print(f"{path}\t{m.group(2)}\t{m.group(1)}")
PY
)

if [ -z "$citations" ]; then
  echo "ok   no tracking-phrase citations found"
elif [ -z "${GH_TOKEN:-}${GITHUB_TOKEN:-}" ]; then
  echo "note: no GH_TOKEN/GITHUB_TOKEN set; skipping the GitHub state check for:"
  printf '      %s\n' "$citations"
else
  while IFS=$'\t' read -r file num phrase; do
    [ -n "$num" ] || continue
    state=$(gh issue view "$num" --repo "$REPO" --json state --jq .state 2>/dev/null || echo "UNKNOWN")
    case "$state" in
      OPEN)
        echo "ok   $file: '$phrase' cites #$num, which is open" ;;
      CLOSED|MERGED)
        echo "FAIL $file: '$phrase' cites #$num, which is $state. Say what landed or cite the live successor."
        failed=$((failed + 1)) ;;
      *)
        echo "FAIL $file: could not read the state of #$num (got '$state')"
        failed=$((failed + 1)) ;;
    esac
  done <<<"$citations"
fi

if [ "$failed" -gt 0 ]; then
  echo
  echo "$failed security doc rot finding(s)."
  exit 1
fi
echo "all ${#FILES[@]} security docs are within $MAX_AGE_DAYS days of review and cite no closed tracking issues"

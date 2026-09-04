# changelog-unreleased.awk — the ONE fence-aware scanner for CHANGELOG.md's
# real `## Unreleased` heading.
#
# WHY THIS FILE EXISTS. The rule "the Unreleased heading is a line matching
# `^## Unreleased[[:space:]]*$` that is NOT inside a ```/~~~ fenced block" was
# hand-copied into compile-changelog.sh (twice), derive-release-version.sh and
# move-unreleased-to-release.py. #5891 was exactly one copy getting that rule
# wrong (anchoring on a prose mention of "Unreleased" instead of the heading),
# and #5918 had to re-align the copies. This file is the single source of
# truth for the locate/extract half of that rule so the next tweak happens
# once. The insertion transformer in compile-changelog.sh and the Python
# mover (move-unreleased-to-release.py) still carry the fence toggle inline —
# their scans are entangled with rewriting — but both cite this file as the
# canonical definition; if their matching ever diverges from this scanner,
# that divergence is a bug.
#
# Usage:
#   awk -v mode=exists -f changelog-unreleased.awk CHANGELOG.md
#       exits 0 when a real `## Unreleased` heading exists, 1 otherwise
#   awk -v mode=body   -f changelog-unreleased.awk CHANGELOG.md
#       prints the section body: every line between the heading and the next
#       non-fenced `## ` heading (or EOF), exactly as derive-release-version.sh
#       has always consumed it
/^[[:space:]]*(```|~~~)/ { in_fence = !in_fence }
!in_fence && /^## Unreleased[[:space:]]*$/ { found = 1; capture = 1; next }
!in_fence && /^## / && capture { if (mode == "body") exit; capture = 0 }
capture && mode == "body" { print }
END { if (mode != "body") exit found ? 0 : 1 }

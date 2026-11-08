#!/usr/bin/env bash
# release-notes-cap.sh — cap a GitHub Release notes body at a deterministic size.
#
# GitHub rejects a release whose body exceeds 125000 characters (HTTP 422
# "body is too long"). The first release on a new line compiles every fragment
# since the line diverged — v6.0.0 carried ~1400 commits' worth and the
# `Create GitHub Release` step of .github/workflows/tagged-release.yml failed
# AFTER the tag was pushed and the images were retagged, leaving a tag with no
# Release and no SBOM/NOTICE assets. This script sits between
# extract-release-notes.sh and the workflow's SBOM/NOTICE trailer so the
# changelog section is what gets cut and the trailer always survives.
#
# Usage: src/scripts/release-notes-cap.sh NOTES.md VERSION REPO [CHANGELOG.md]
#   NOTES.md     the extracted release section (rewritten in place when capped)
#   VERSION      X.Y.Z, no leading v — names the tag the link points at
#   REPO         owner/name, for the CHANGELOG.md permalink
#   CHANGELOG.md optional; when given, the `## ... (vVERSION)` heading is used
#                to build the section anchor of the permalink
#
# RELEASE_NOTES_MAX_CHARS (default 120000) is the cap for the section body in
# bytes (bytes >= characters, so this is conservative). The workflow appends a
# ~700-byte trailer after this script runs; the 5000-byte headroom under
# GitHub's 125000 covers that trailer plus the truncation marker.
#
# When the body fits, the file is left untouched and nothing is printed. When
# it does not, it is cut at the last bullet/subsection boundary that fits and a
# final line is appended:
#   … (N more entries) — full notes in [CHANGELOG.md](https://github.com/REPO/blob/vVERSION/CHANGELOG.md#anchor)
# where N counts the `- ` bullets that were dropped. The cut is a pure function
# of the input, so a re-run produces the same body.
set -euo pipefail

NOTES="${1:?usage: release-notes-cap.sh NOTES.md VERSION REPO [CHANGELOG.md]}"
VERSION="${2:?usage: release-notes-cap.sh NOTES.md VERSION REPO [CHANGELOG.md]}"
REPO="${3:?usage: release-notes-cap.sh NOTES.md VERSION REPO [CHANGELOG.md]}"
CHANGELOG="${4:-}"

RELEASE_NOTES_MAX_CHARS="${RELEASE_NOTES_MAX_CHARS:-120000}"
# Bytes reserved for the blank line plus the truncation marker appended below.
MARKER_RESERVE=400

if [[ ! -f "$NOTES" ]]; then
  echo "notes file not found: ${NOTES}" >&2
  exit 1
fi
if ! [[ "$RELEASE_NOTES_MAX_CHARS" =~ ^[0-9]+$ ]] || [[ "$RELEASE_NOTES_MAX_CHARS" -le "$MARKER_RESERVE" ]]; then
  echo "RELEASE_NOTES_MAX_CHARS must be an integer greater than ${MARKER_RESERVE}: ${RELEASE_NOTES_MAX_CHARS}" >&2
  exit 1
fi

size="$(wc -c < "$NOTES" | tr -d ' ')"
if [[ "$size" -le "$RELEASE_NOTES_MAX_CHARS" ]]; then
  exit 0
fi

# GitHub heading anchor: lowercase, drop everything but letters, digits,
# spaces and hyphens, then spaces -> hyphens. `## 2026-11-08 (v6.0.0)` becomes
# `2026-11-08-v600`.
anchor=""
if [[ -n "$CHANGELOG" && -f "$CHANGELOG" ]]; then
  heading="$(grep -m1 -E "^## .*\(v${VERSION//./\\.}\)" "$CHANGELOG" || true)"
  if [[ -n "$heading" ]]; then
    anchor="$(printf '%s' "${heading#\#\# }" \
      | tr '[:upper:]' '[:lower:]' \
      | sed -e 's/[^a-z0-9 -]//g' -e 's/ /-/g')"
  fi
fi
link="https://github.com/${REPO}/blob/v${VERSION}/CHANGELOG.md"
[[ -n "$anchor" ]] && link="${link}#${anchor}"

budget=$(( RELEASE_NOTES_MAX_CHARS - MARKER_RESERVE ))
capped="${NOTES}.capped"
LC_ALL=C awk -v budget="$budget" -v link="$link" '
  { lines[++n] = $0; bytes[n] = length($0) + 1 }
  END {
    # Candidate cut points are lines that start a bullet or a subsection:
    # everything before such a line is a whole number of entries.
    total = 0; cut = 0
    for (i = 1; i <= n; i++) {
      if (i > 1 && lines[i] ~ /^(- |### |## )/ && total <= budget) cut = i
      total += bytes[i]
      if (total > budget) break
    }
    if (cut == 0) { print "no bullet boundary fits under the cap" > "/dev/stderr"; exit 1 }
    dropped = 0
    for (i = cut; i <= n; i++) if (lines[i] ~ /^- /) dropped++
    end = cut - 1
    while (end > 0 && lines[end] ~ /^[[:space:]]*$/) end--
    for (i = 1; i <= end; i++) print lines[i]
    print ""
    printf "… (%d more entr%s) — full notes in [CHANGELOG.md](%s)\n", dropped, (dropped == 1 ? "y" : "ies"), link
  }
' "$NOTES" > "$capped"
mv "$capped" "$NOTES"
echo "release notes capped: ${size} bytes exceeded RELEASE_NOTES_MAX_CHARS=${RELEASE_NOTES_MAX_CHARS}; body is now $(wc -c < "$NOTES" | tr -d ' ') bytes, remainder linked at ${link}"

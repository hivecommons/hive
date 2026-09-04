#!/usr/bin/env bash
# derive-release-version.sh — decide whether a merge to v4 warrants a tagged
# release, and if so, what version.
#
# WHY CHANGELOG.md AND NOT COMMIT-MESSAGE EMOJI:
#
# This repo's CONTRIBUTING.md asks PR titles to start with an emoji convention
# (🐛 fix, ✨ feature, 📖 docs, ...), which looks at first glance like a ready-
# made conventional-commit signal for semver inference. It is not enforced:
# `git log --oneline` on this branch has merge commits with no emoji at all
# ("test(watchdog): ...", "fix(dashboard): ...", "[quality] ...") mixed in with
# ones that have it, at roughly 70% coverage measured over the last 300
# commits. Inferring a MAJOR/MINOR/PATCH bump — and therefore a published,
# immutable, GitHub-Release-noted number — from a signal nothing gates would
# mean an unlabeled fix commit silently produces a patch release with the
# wrong shape, or worse, a feature commit missing its prefix produces no
# release at all, and nobody would notice because the whole point of this
# workflow is that nobody is watching each merge.
#
# CHANGELOG.md is a stronger signal for three concrete reasons:
#   1. It already IS the release-worthiness decision, made by a human, at PR
#      time — "Add entries under `Unreleased` for user-visible ... changes" is
#      an existing, human-authored editorial judgment call, not something this
#      script has to reconstruct from commit text.
#   2. It already carries the file's own convention for retiring a batch of
#      entries into a release ("Move entries into a dated release section when
#      a release is cut"), so automating this is completing a documented
#      manual step, not inventing a new contract.
#   3. It is advisory-reminded (.github/workflows/changelog-reminder.yml) on
#      every code PR that skips it, so coverage is actively nudged upward,
#      unlike the emoji convention which has no such reminder.
#
# It is not perfect either — an author can still forget the reminder, or judge
# a change not user-visible when a maintainer would disagree — but getting it
# wrong FAILS SAFE: a missed entry means no release fires (the conservative
# failure), not a released version with the wrong number.
#
# RULE:
#   - No content under `## Unreleased` (blank, or only the four boilerplate
#     bullet points under "How we maintain this file" — those live above the
#     Unreleased heading and are never scanned) => NO RELEASE. Exit 0,
#     release=false. This is the common case: most merges are docs, tests, CI,
#     or refactors and must silently produce nothing.
#   - `### Security` present under Unreleased => MAJOR bump. Security changes
#     in this changelog have historically included breaking auth/permission
#     changes (e.g. the metrics-token fail-closed change), so this errs
#     toward the more disruptive bump rather than assuming safe.
#   - Else `### Added` present => MINOR bump.
#   - Else (`### Changed` / `### Fixed` / `### Deprecated` only) => PATCH bump.
#   - An explicit HTML-comment marker anywhere in the Unreleased section
#     overrides the above, and is the human escape hatch:
#       <!-- release: none -->            never release this merge
#       <!-- release: major|minor|patch --> force this bump regardless of
#                                            which ### headers are present
#     A marker resolves the whole decision; if more than one conflicting
#     marker is present this is an error (loud, not a silent pick).
#
# VERSION BASE: the latest `vX.Y.Z` tag reachable from HEAD (git tag -l
# 'v*.*.*' sorted by version, not by date — see the -V sort below). No tags
# yet => base 0.0.0, so the very first automated release is v0.1.0 (Added
# present, the common case) or v0.0.1 (patch-only). If a maintainer wants the
# first tag to be v1.0.0 instead, cut it by hand once — see src/docs/releases.md
# "Escape hatch: cutting the first / an out-of-band release".
#
# OUTPUT: writes `release=true|false`, and when true `version=X.Y.Z` (no
# leading "v") and `bump=major|minor|patch`, to $GITHUB_OUTPUT if set,
# otherwise to stdout as `key=value` lines (for local/manual invocation).
#
# Usage: src/scripts/derive-release-version.sh [changelog-path]
set -euo pipefail

CHANGELOG="${1:-CHANGELOG.md}"

emit() {
  local key=$1 value=$2
  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "${key}=${value}" >> "$GITHUB_OUTPUT"
  else
    echo "${key}=${value}"
  fi
}

if [[ ! -f "$CHANGELOG" ]]; then
  echo "::error::changelog not found at ${CHANGELOG}" >&2
  exit 1
fi

# Extract the body of the `## Unreleased` section: everything between that
# heading and the next `## ` heading (or end of file), via the shared
# fence-aware scanner (single source of truth, #5939).
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
unreleased="$(awk -v mode=body -f "${script_dir}/lib/changelog-unreleased.awk" "$CHANGELOG")"

# Strip blank lines for the emptiness check so a section with only whitespace
# still counts as empty.
non_blank="$(printf '%s\n' "$unreleased" | grep -c '[^[:space:]]' || true)"

# ---------------------------------------------------------------------------
# Escape hatch: explicit override markers win outright.
# ---------------------------------------------------------------------------
mapfile -t markers < <(printf '%s\n' "$unreleased" | grep -oE '<!-- *release: *(none|major|minor|patch) *-->' | grep -oE 'none|major|minor|patch' || true)
if [[ ${#markers[@]} -gt 1 ]]; then
  # de-dupe identical repeats before treating this as a conflict
  mapfile -t uniq_markers < <(printf '%s\n' "${markers[@]}" | sort -u)
  if [[ ${#uniq_markers[@]} -gt 1 ]]; then
    echo "::error::CHANGELOG.md Unreleased section carries conflicting release markers: ${markers[*]}. Leave exactly one <!-- release: ... --> marker." >&2
    exit 1
  fi
  markers=("${uniq_markers[0]}")
fi

override=""
if [[ ${#markers[@]} -eq 1 ]]; then
  override="${markers[0]}"
fi

if [[ "$override" == "none" ]]; then
  echo "Unreleased section carries <!-- release: none -->; skipping this merge by explicit human override."
  emit release false
  exit 0
fi

if [[ -z "$override" && "$non_blank" -eq 0 ]]; then
  echo "Unreleased section is empty; nothing release-worthy landed in this merge."
  emit release false
  exit 0
fi

if [[ -n "$override" ]]; then
  bump="$override"
  echo "Bump forced to '${bump}' by explicit <!-- release: ${bump} --> marker in CHANGELOG.md."
else
  # NOTE: `### Security` does NOT imply major here, and that is deliberate.
  #
  # In this repository the MAJOR version IS the release line: `v2`, `v4` and
  # `v5` are real long-lived branches listed in .github/release-lines.yml, and
  # cutting a new line is a coordinated decision (new branch, workflow pins,
  # docker LONG_LIVED policy, hub branch switcher). A tag that crosses a major
  # boundary is therefore not "a bigger release" — it claims a line that may
  # already exist and be owned by other work.
  #
  # This fired for real: a merge to v4 whose Unreleased section contained two
  # Security entries (an action-pin hardening and a proxy gate) derived
  # VERSION=5.0.0 while a `v5` branch already existed. Security entries are
  # common and usually NOT breaking — pinning an action is the opposite of a
  # breaking change — so inferring major from them mislabels routine hardening
  # as a line cut.
  #
  # Security now maps to PATCH by default, which is the honest floor: a
  # security fix is at least a patch, and anything genuinely breaking is
  # declared with the explicit `<!-- release: major -->` marker. Crossing a
  # release line stays a human decision, which is what it already is
  # everywhere else in this repo.
  if printf '%s\n' "$unreleased" | grep -qE '^### Added[[:space:]]*$'; then
    bump=minor
  else
    bump=patch
  fi
  echo "Inferred bump '${bump}' from CHANGELOG.md Unreleased section headers."
fi

# ---------------------------------------------------------------------------
# HARD GUARD: never derive a version whose MAJOR differs from the release line
# this branch belongs to.
#
# Belt-and-braces alongside the inference above. Even with an explicit
# `<!-- release: major -->` marker, a merge to v4 must not mint a v5.x.y tag:
# in this repo the major IS the line (see .github/release-lines.yml), a `v5`
# branch can already exist, and a tag minted from the wrong branch is not
# fixable by deleting it — consumers may already have pulled the image it
# retagged.
#
# Cutting a new line stays a deliberate, human, multi-step operation. If that
# is genuinely what you want, tag it by hand on the correct branch.
# ---------------------------------------------------------------------------
branch_line="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || true)"
if [[ "$branch_line" =~ ^v([0-9]+)$ ]]; then
  line_major="${BASH_REMATCH[1]}"
  if [[ "$bump" == "major" ]]; then
    echo "::error::refusing to derive a MAJOR bump on release line ${branch_line}: the major version is the release line here, so a major bump would mint a v$((line_major + 1)).0.0 tag from the ${branch_line} branch. Cut a new line deliberately instead." >&2
    exit 1
  fi
fi

# ---------------------------------------------------------------------------
# Base version: latest vX.Y.Z tag, sorted as versions (not tag creation date,
# which a re-tag or annotated/lightweight mix could get wrong).
# ---------------------------------------------------------------------------
latest_tag="$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sort -V | tail -1 || true)"
if [[ -z "$latest_tag" ]]; then
  base="0.0.0"
  echo "No existing vX.Y.Z tag found; treating base version as ${base}."
else
  base="${latest_tag#v}"
  echo "Latest tag: ${latest_tag} (base version ${base})."
fi

IFS='.' read -r major minor patch <<< "$base"
if ! [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ && "$patch" =~ ^[0-9]+$ ]]; then
  echo "::error::latest tag ${latest_tag:-<none>} does not parse as MAJOR.MINOR.PATCH (got '${base}')" >&2
  exit 1
fi

case "$bump" in
  major)
    major=$((major + 1)); minor=0; patch=0
    ;;
  minor)
    minor=$((minor + 1)); patch=0
    ;;
  patch)
    patch=$((patch + 1))
    ;;
  *)
    echo "::error::unknown bump type '${bump}'" >&2
    exit 1
    ;;
esac

version="${major}.${minor}.${patch}"
echo "Derived release version: v${version} (bump=${bump})"
emit release true
emit version "$version"
emit bump "$bump"

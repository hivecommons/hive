#!/usr/bin/env bash
# check-release-sha-ancestry.sh — refuses a workflow_dispatch `release_sha`
# that is not actually reachable from the dispatched branch (#6419).
#
# docker.yml's `gate` job only checked that `release_sha` was well-formed
# 40-hex, then every downstream build/merge job checked out and PUBLISHED that
# exact commit under the dispatched branch's moving tags (`<branch>-latest`,
# `candidate`, ...). Well-formed hex is not "in this branch's history" — any
# collaborator (or automation) with `actions: write` could dispatch an
# unreviewed PR head commit and have it published as the current release
# image, laundered through the normal build pipeline with legitimate
# provenance labels. tagged-release.yml, the intended caller, already runs
# this exact `git merge-base --is-ancestor` check before it ever tags a
# commit (see its "push_v4" step); the gate job re-verified nothing.
#
# This is a standalone, testable helper rather than an inline `run:` block so
# it can be driven against fixture repos (see test-check-release-sha-ancestry.sh)
# instead of only being provable inside a real GitHub Actions run.
#
# Usage: src/scripts/check-release-sha-ancestry.sh <target-sha> [ref] [repo-dir]
#   target-sha   the candidate commit SHA (must be full 40-char lowercase hex)
#   ref          the ref the SHA must be an ancestor of; default: HEAD
#   repo-dir     git working tree to check; default: .
#
# Exit 0 = target-sha is a real commit AND an ancestor of ref (safe to build
#          and publish). Exit 1 = anything else — malformed, missing object,
#          or not an ancestor (fail closed).
set -uo pipefail

TARGET_SHA="${1:-}"
REF="${2:-HEAD}"
REPO_DIR="${3:-.}"

if [[ -z "$TARGET_SHA" ]]; then
  echo "::error::check-release-sha-ancestry.sh requires a target SHA argument" >&2
  exit 1
fi

if ! [[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]]; then
  echo "::error::release_sha must be a full 40-character lowercase commit SHA, got '${TARGET_SHA}'" >&2
  exit 1
fi

if ! git -C "$REPO_DIR" cat-file -e "${TARGET_SHA}^{commit}" 2>/dev/null; then
  echo "::error::release_sha '${TARGET_SHA}' does not resolve to a commit object reachable from this checkout; refusing to build or publish it." >&2
  exit 1
fi

if ! git -C "$REPO_DIR" merge-base --is-ancestor "$TARGET_SHA" "$REF"; then
  echo "::error::release_sha '${TARGET_SHA}' is not an ancestor of '${REF}'; refusing to build or publish a commit outside the dispatched branch's history." >&2
  exit 1
fi

echo "release_sha '${TARGET_SHA}' verified as an ancestor of '${REF}'."

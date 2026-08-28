#!/usr/bin/env bash
# generate-notice.sh — assemble the repo-root NOTICE file from the Go module
# graph under src/.
#
# WHY go-licenses AND NOT A HAND-ROLLED go-list WALK
#
# Two ways to get from "go.mod" to "license text per dependency" were
# considered:
#
#   1. github.com/google/go-licenses — walks the resolved module graph
#      (`go list -m -json all`), finds each module's license file in the
#      module cache, and classifies it against SPDX identifiers using the
#      same detection library (github.com/google/licensecheck) that
#      pkg.go.dev itself uses for the "License" badge on a module page. It
#      is purpose-built for exactly this NOTICE-generation problem and is
#      the tool most Go projects reach for when a CNCF/OSPO review asks this
#      question.
#   2. A hand-rolled `go list -m -json all` + grep for LICENSE/COPYING files
#      in $(go env GOMODCACHE) — more code to maintain, and classification
#      (which SPDX identifier a given LICENSE file text IS) would have to be
#      reinvented rather than reusing a maintained detector. That reinvention
#      is exactly the kind of homegrown parsing this project's own
#      CLAUDE.md-equivalent guidance says to avoid when a well-known tool
#      already solves it.
#
# go-licenses wins on maintenance cost and correctness for the same reason
# this repo already prefers `go install <tool>@<pinned version>` over
# reimplementing a scanner: govulncheck and gosec in go-security-analysis.yml
# follow the identical pattern (see that workflow's header). This script
# follows it too — no `@latest`, ever.
#
# WHY NOT go-licenses check / go-licenses csv
#
# `go-licenses save` (writes each dependency's license file to a directory
# tree) plus a small formatting pass is used here rather than `go-licenses
# report` (a maintained-but-less-flexible NOTICE template), because the
# repository wants module path, version, AND full license text per entry in
# one flat file, and controlling that layout directly in this script is
# simpler than fighting a report template.
#
# REPRODUCIBILITY
#
# The go-licenses version below is pinned to an exact tag, installed via
# `go install <module>@<version>`, same as govulncheck/gosec in
# go-security-analysis.yml. Bump it deliberately, in its own commit, when a
# newer go-licenses release is wanted — never track a moving ref.
#
# WHAT THIS SCRIPT CANNOT DO WITHOUT `go`
#
# This script requires a working `go` toolchain (it runs `go list` under the
# hood via go-licenses) and network access to the module proxy for any module
# not already in the local module cache. It cannot run in an environment
# without `go` installed — CI (go-security-analysis.yml) is the environment
# that actually produces the authoritative NOTICE; this script is what that
# CI job runs, not a replacement for CI.
#
# Usage: src/scripts/generate-notice.sh [output-path]
#   output-path defaults to NOTICE at the repo root.

set -euo pipefail

# Pinned exact release — bump deliberately, never track @latest or a branch.
GO_LICENSES_VERSION="v1.6.0"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${SRC_DIR}/.." && pwd)"
OUT_PATH="${1:-${REPO_ROOT}/NOTICE}"

if ! command -v go >/dev/null 2>&1; then
  echo "generate-notice.sh: 'go' is required (this script shells out to go-licenses, which itself runs 'go list') and was not found on PATH." >&2
  exit 1
fi

echo "Installing go-licenses@${GO_LICENSES_VERSION} (pinned)..." >&2
go install "github.com/google/go-licenses@${GO_LICENSES_VERSION}"

GOBIN="$(go env GOPATH)/bin"
GO_LICENSES_BIN="${GOBIN}/go-licenses"
if [[ ! -x "${GO_LICENSES_BIN}" ]]; then
  echo "generate-notice.sh: go-licenses did not install to ${GO_LICENSES_BIN}" >&2
  exit 1
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

SAVE_DIR="${WORK_DIR}/licenses"

echo "Walking the module graph and saving license files..." >&2
(
  cd "${SRC_DIR}"
  "${GO_LICENSES_BIN}" save ./... --save_path="${SAVE_DIR}" --force
)

echo "Classifying each module's license..." >&2
REPORT_CSV="${WORK_DIR}/report.csv"
(
  cd "${SRC_DIR}"
  "${GO_LICENSES_BIN}" csv ./... > "${REPORT_CSV}"
)

{
  cat <<'HEADER'
Hive — Third-Party Notices
===========================

This NOTICE file was generated AUTHORITATIVELY by CI
(.github/workflows/go-security-analysis.yml, job "notice-drift") by running
src/scripts/generate-notice.sh, which walks the resolved Go module graph
(src/go.mod, src/go.sum) with google/go-licenses (pinned version — see this
script's header for why that tool was chosen over a hand-rolled walk) and
reproduces each dependency's own license text.

Regenerate locally with a Go toolchain installed:

    src/scripts/generate-notice.sh

CI regenerates this file on every change to src/go.mod, src/go.sum, or this
script, and fails the build if the committed NOTICE differs from a fresh
regeneration — see the "notice-drift" job in go-security-analysis.yml. Do not
hand-edit the dependency entries below; edit go.mod/go.sum (to change what is
depended on) or this script (to change how the file is assembled), then
regenerate.

Scope: this file covers Go module dependencies compiled into the `hive`,
`hive-hub`, and `hive-contributor` binaries. It does NOT cover base-image OS
packages, or the Node.js/tmux layers built from source in the container
images — those are covered by the per-image SBOMs attached to every tagged
release (see src/docs/releases.md, "Software bill of materials (SBOM)").

-----------------------------------------------------------------------------

HEADER

  # go-licenses csv output is "import path,license URL,license type" per
  # line. Group by module (best-effort: the import path's leading segments
  # up to the go.mod-declared module root) is skipped in favor of listing
  # each reported package's license line directly — go-licenses already
  # resolves transitively and de-duplicates identical license files, so this
  # stays a flat, auditable list rather than re-deriving module boundaries
  # with a second parsing pass.
  if [[ -s "${REPORT_CSV}" ]]; then
    while IFS=',' read -r pkg url ltype; do
      [[ -z "${pkg}" ]] && continue
      echo "Package:  ${pkg}"
      echo "License:  ${ltype:-UNKNOWN (see source URL)}"
      echo "Source:   ${url}"
      lic_file=""
      if [[ -d "${SAVE_DIR}" ]]; then
        candidate="$(find "${SAVE_DIR}" -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' \) -path "*${pkg}*" 2>/dev/null | head -1)"
        [[ -n "${candidate}" ]] && lic_file="${candidate}"
      fi
      if [[ -n "${lic_file}" ]]; then
        echo
        sed 's/^/    /' "${lic_file}"
      else
        echo "    (license text not found by go-licenses save; verify manually)"
      fi
      echo
      echo "-----------------------------------------------------------------------------"
      echo
    done < "${REPORT_CSV}"
  fi
} > "${OUT_PATH}"

echo "Wrote ${OUT_PATH}" >&2

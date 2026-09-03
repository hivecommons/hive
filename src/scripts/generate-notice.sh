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
# WHY go-licenses report, NOT save / check / csv
#
# `go-licenses report` with a repository-owned template exposes the package
# path, resolved module version, detected license identifier, source URL, and
# license text directly. That is exactly the flat NOTICE layout this
# repository needs, without reconstructing versions or trying to match a CSV
# package path back to a directory written by `save`.
#
# This distinction also keeps attribution separate from license policy.
# `go-licenses save` refuses to produce output when it finds a license class
# the tool considers incompatible. A NOTICE must still identify and reproduce
# a restrictive dependency's license; omitting the dependency would make the
# inventory incomplete. Deciding whether a dependency is acceptable belongs
# in a separate policy gate, not in the generator that records what ships.
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

install_go_licenses() {
  local attempt max_attempts sleep_seconds
  max_attempts=3
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    echo "Installing go-licenses@${GO_LICENSES_VERSION} (pinned), attempt ${attempt}/${max_attempts}..." >&2
    if go install "github.com/google/go-licenses@${GO_LICENSES_VERSION}"; then
      return 0
    fi
    if (( attempt == max_attempts )); then
      break
    fi
    sleep_seconds=$((attempt * 5))
    echo "go-licenses install failed; retrying in ${sleep_seconds}s..." >&2
    sleep "${sleep_seconds}"
  done
  echo "generate-notice.sh: failed to install go-licenses@${GO_LICENSES_VERSION} after ${max_attempts} attempts" >&2
  return 1
}

install_go_licenses

GOBIN="$(go env GOPATH)/bin"
GO_LICENSES_BIN="${GOBIN}/go-licenses"
if [[ ! -x "${GO_LICENSES_BIN}" ]]; then
  echo "generate-notice.sh: go-licenses did not install to ${GO_LICENSES_BIN}" >&2
  exit 1
fi

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

REPORT_TEMPLATE="${WORK_DIR}/notice.tpl"
GENERATED_NOTICE="${WORK_DIR}/NOTICE.generated"

cat > "${REPORT_TEMPLATE}" <<'TEMPLATE'
{{range . -}}
Package:  {{.Name}}
Version:  {{.Version}}
License:  {{.LicenseName}}
Source:   {{.LicenseURL}}

{{if .LicensePath}}{{.LicenseText}}{{else}}    (license text not found by go-licenses; verify manually)
{{end}}
-----------------------------------------------------------------------------

{{end -}}
TEMPLATE

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

  echo "Walking the module graph and rendering license entries..." >&2
  (
    cd "${SRC_DIR}"
    # --ignore the project's own module: LICENSE lives at the REPO root while
    # the Go module is src/, so go-licenses' upward search stops at src/ and
    # reports Hive packages as unlicensed. Hive's own code does not belong in
    # a THIRD-PARTY notice regardless, so excluding it is the correct scope.
    "${GO_LICENSES_BIN}" report --template="${REPORT_TEMPLATE}" \
      --ignore github.com/hivecommons/hive ./...
  # Preserve the complete text while normalizing insignificant end-of-line
  # whitespace from upstream license files, so the committed output passes
  # repository whitespace checks and stays stable across tooling/editors.
  ) | sed 's/[[:space:]]\+$//'
} > "${GENERATED_NOTICE}"

# Replace the requested output only after generation succeeds, so a failed
# tool run cannot leave a truncated NOTICE behind.
mv -- "${GENERATED_NOTICE}" "${OUT_PATH}"

echo "Wrote ${OUT_PATH}" >&2

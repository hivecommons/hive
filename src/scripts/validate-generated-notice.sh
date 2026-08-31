#!/usr/bin/env bash
# validate-generated-notice.sh — assert that a generated NOTICE is fit to be
# committed automatically.
#
# WHY THIS EXISTS (#5256)
#
# .github/workflows/notice-autofix.yml commits a regenerated NOTICE onto
# Dependabot branches without a human reading it first. That is only safe if
# something refuses the file when it is degraded or when it records a license
# this project cannot ship. This script is that something.
#
# It deliberately does NOT compare against the committed NOTICE — drift is
# check-notice-drift.sh's job, and drift is the expected state here (a
# dependency bump changed the graph). This script asks a different question:
# "is the freshly generated file itself trustworthy enough to commit
# unattended?"
#
# Usage: src/scripts/validate-generated-notice.sh <generated-notice>
set -euo pipefail

GENERATED_NOTICE="${1:?usage: validate-generated-notice.sh <generated-notice>}"

if [[ ! -s "${GENERATED_NOTICE}" ]]; then
  echo "::error::${GENERATED_NOTICE} is missing or empty; refusing to overwrite NOTICE." >&2
  exit 1
fi

fail=0

# 1. STRUCTURAL INTEGRITY -----------------------------------------------------
# A truncated artifact (interrupted upload, partial generator run) must never
# replace a good NOTICE. The generator's header and at least one rendered
# dependency entry are the cheapest proof the file is whole.
if ! grep -qF 'Hive — Third-Party Notices' -- "${GENERATED_NOTICE}"; then
  echo "::error::${GENERATED_NOTICE} does not carry the generator's header; it looks truncated or hand-made. Refusing to commit it." >&2
  fail=1
fi

entry_count="$(grep -c '^Package:  ' -- "${GENERATED_NOTICE}" || true)"
if (( entry_count == 0 )); then
  echo "::error::${GENERATED_NOTICE} contains no dependency entries; a NOTICE with zero third-party packages is a generator failure, not a real module graph. Refusing to commit it." >&2
  fail=1
fi

# 2. BYTE-REPRODUCIBILITY (#5064) --------------------------------------------
# The drift gate is byte-for-byte and whitespace-significant. generate-notice.sh
# strips end-of-line whitespace from upstream license texts precisely so local
# and CI output agree. If a trailing-space line survives into the artifact, the
# generator's normalization has regressed, and committing the file would bake a
# permanent phantom diff into the repository.
trailing="$(grep -c '[[:space:]]$' -- "${GENERATED_NOTICE}" || true)"
if (( trailing > 0 )); then
  echo "::error::${GENERATED_NOTICE} has ${trailing} line(s) with trailing whitespace. generate-notice.sh is supposed to normalize these (#5064); committing this file would guarantee permanent NOTICE drift. Refusing to commit it." >&2
  fail=1
fi

# 3. ATTRIBUTION COMPLETENESS -------------------------------------------------
# The generator emits this marker when go-licenses could not locate a
# dependency's license file. That is a genuine gap in attribution and needs a
# human, not an automatic commit that makes it look resolved.
if grep -qF 'license text not found by go-licenses' -- "${GENERATED_NOTICE}"; then
  echo "::error::${GENERATED_NOTICE} contains dependencies whose license text go-licenses could not find. Attribution is incomplete; a human must resolve this rather than auto-committing. Offending entries:" >&2
  grep -B 5 -F 'license text not found by go-licenses' -- "${GENERATED_NOTICE}" | grep '^Package:  ' >&2 || true
  fail=1
fi

# An empty alternation branch ("(a|b|)") is a GNU-grep extension that other
# POSIX greps reject outright, so the "no identifier at all" case is spelled as
# its own alternative rather than an empty branch.
UNKNOWN_SPDX_PATTERN='^License:  ([Uu]nknown|UNKNOWN|none|NONE)?[[:space:]]*$'

if grep -qE "${UNKNOWN_SPDX_PATTERN}" -- "${GENERATED_NOTICE}"; then
  echo "::error::${GENERATED_NOTICE} contains entries with an unknown or empty license identifier. Refusing to commit an unverified attribution record." >&2
  grep -nE "${UNKNOWN_SPDX_PATTERN}" -- "${GENERATED_NOTICE}" >&2 || true
  fail=1
fi

# 4. LICENSE POLICY -----------------------------------------------------------
# Hive ships under Apache-2.0. A strong-copyleft dependency entering the module
# graph is exactly the event the notice-drift gate exists to surface — it is how
# the AGPL-3.0 go-docx dependency was caught (#5016). Auto-committing a NOTICE
# that records such a dependency would turn that red signal green and launder a
# real license problem into a routine bot commit. This check fails LOUDLY and
# stops the autofix; the human path (read the finding, drop or replace the
# dependency) is unchanged.
#
# Matched on the SPDX identifiers go-licenses emits, anchored to the generator's
# "License:  " field so license BODY text mentioning a licence name cannot
# trigger a false positive.
FORBIDDEN_SPDX_PATTERN='^License:  (AGPL-[0-9.]+(-only|-or-later)?|GPL-[0-9.]+(-only|-or-later)?|SSPL-[0-9.]+|OSL-[0-9.]+|CC-BY-NC[-.A-Za-z0-9]*|BUSL-[0-9.]+|EUPL-[0-9.]+)([[:space:]]|$)'

if grep -qE "${FORBIDDEN_SPDX_PATTERN}" -- "${GENERATED_NOTICE}"; then
  echo "::error::${GENERATED_NOTICE} records a dependency under a license that is FORBIDDEN in this Apache-2.0 project. Refusing to auto-commit. A human must remove or replace the dependency. Offending entries:" >&2
  grep -nE "${FORBIDDEN_SPDX_PATTERN}" -- "${GENERATED_NOTICE}" >&2 || true
  fail=1
fi

if (( fail != 0 )); then
  exit 1
fi

echo "Generated NOTICE validated: ${entry_count} dependency entries, no trailing whitespace, no unverified attribution, no forbidden licenses."

#!/usr/bin/env bash
# Regression coverage for the autofix validation contract (#5256).
#
# Each case asserts the INVARIANT, not just an exit code: a validator that
# accepted everything would still pass an exit-status-only test on the happy
# path, so every rejection case also asserts the specific diagnostic that
# names WHY the file was refused.
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
VALIDATOR="${SCRIPT_DIR}/validate-generated-notice.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

# A minimal but structurally complete generated NOTICE.
write_good_notice() {
  cat > "$1" <<'EOF'
Hive — Third-Party Notices
===========================

-----------------------------------------------------------------------------

Package:  github.com/example/one
Version:  v1.0.0
License:  Apache-2.0
Source:   https://example.invalid/one/LICENSE

Licensed under the Apache License, Version 2.0.

-----------------------------------------------------------------------------

EOF
}

GOOD="${TMP}/NOTICE.good"
write_good_notice "${GOOD}"

# --- happy path --------------------------------------------------------------
output="$("${VALIDATOR}" "${GOOD}" 2>&1)"
grep -qF 'Generated NOTICE validated' <<< "${output}"
grep -qF '1 dependency entries' <<< "${output}"

# --- empty / missing ---------------------------------------------------------
: > "${TMP}/NOTICE.empty"
if output="$("${VALIDATOR}" "${TMP}/NOTICE.empty" 2>&1)"; then
  echo "FAIL: empty NOTICE accepted" >&2; exit 1
fi
grep -qF 'missing or empty' <<< "${output}"

if output="$("${VALIDATOR}" "${TMP}/does-not-exist" 2>&1)"; then
  echo "FAIL: nonexistent NOTICE accepted" >&2; exit 1
fi

# --- truncated (no header) ---------------------------------------------------
grep -v 'Third-Party Notices' "${GOOD}" > "${TMP}/NOTICE.noheader"
if output="$("${VALIDATOR}" "${TMP}/NOTICE.noheader" 2>&1)"; then
  echo "FAIL: headerless NOTICE accepted" >&2; exit 1
fi
grep -qF 'looks truncated' <<< "${output}"

# --- header but zero dependency entries --------------------------------------
printf 'Hive — Third-Party Notices\n===========================\n\n' > "${TMP}/NOTICE.noentries"
if output="$("${VALIDATOR}" "${TMP}/NOTICE.noentries" 2>&1)"; then
  echo "FAIL: NOTICE with no dependency entries accepted" >&2; exit 1
fi
grep -qF 'no dependency entries' <<< "${output}"

# --- trailing whitespace (#5064) ---------------------------------------------
write_good_notice "${TMP}/NOTICE.trailing"
printf 'some license line with a trailing space \n' >> "${TMP}/NOTICE.trailing"
if output="$("${VALIDATOR}" "${TMP}/NOTICE.trailing" 2>&1)"; then
  echo "FAIL: trailing-whitespace NOTICE accepted" >&2; exit 1
fi
grep -qF 'trailing whitespace' <<< "${output}"
grep -qF '#5064' <<< "${output}"

# --- unverified attribution --------------------------------------------------
write_good_notice "${TMP}/NOTICE.unverified"
cat >> "${TMP}/NOTICE.unverified" <<'EOF'
Package:  github.com/example/two
Version:  v2.0.0
License:  MIT
Source:   https://example.invalid/two

    (license text not found by go-licenses; verify manually)

-----------------------------------------------------------------------------

EOF
if output="$("${VALIDATOR}" "${TMP}/NOTICE.unverified" 2>&1)"; then
  echo "FAIL: NOTICE with unverified license text accepted" >&2; exit 1
fi
grep -qF 'Attribution is incomplete' <<< "${output}"

# --- unknown license identifier ----------------------------------------------
write_good_notice "${TMP}/NOTICE.unknown"
printf 'Package:  github.com/example/three\nVersion:  v3.0.0\nLicense:  Unknown\nSource:   https://example.invalid/three\n\ntext\n' >> "${TMP}/NOTICE.unknown"
if output="$("${VALIDATOR}" "${TMP}/NOTICE.unknown" 2>&1)"; then
  echo "FAIL: NOTICE with Unknown license accepted" >&2; exit 1
fi
grep -qF 'unknown or empty license identifier' <<< "${output}"

# --- FORBIDDEN licenses must fail loudly -------------------------------------
# This is the invariant that matters most: the autofix must never launder a
# copyleft dependency into a green check (#5016 caught AGPL-3.0 go-docx).
for spdx in AGPL-3.0 AGPL-3.0-or-later GPL-2.0 GPL-3.0-only SSPL-1.0 BUSL-1.1 EUPL-1.2 OSL-3.0 CC-BY-NC-4.0; do
  f="${TMP}/NOTICE.forbidden"
  write_good_notice "${f}"
  printf 'Package:  github.com/example/bad\nVersion:  v1.0.0\nLicense:  %s\nSource:   https://example.invalid/bad\n\ntext\n' "${spdx}" >> "${f}"
  if output="$("${VALIDATOR}" "${f}" 2>&1)"; then
    echo "FAIL: forbidden license ${spdx} accepted" >&2; exit 1
  fi
  grep -qF 'FORBIDDEN' <<< "${output}" || {
    echo "FAIL: ${spdx} rejected without naming it FORBIDDEN" >&2; exit 1; }
  grep -qF "${spdx}" <<< "${output}" || {
    echo "FAIL: diagnostic for ${spdx} did not name the offending license" >&2; exit 1; }
done

# --- permissive licenses must NOT be misclassified as forbidden --------------
# Guards against an over-broad pattern: LGPL and MPL are not on the forbidden
# list, and no permissive identifier may be caught by the copyleft regex.
for spdx in MIT Apache-2.0 BSD-3-Clause BSD-2-Clause ISC MPL-2.0 LGPL-3.0 Unlicense Zlib; do
  f="${TMP}/NOTICE.ok"
  write_good_notice "${f}"
  printf 'Package:  github.com/example/fine\nVersion:  v1.0.0\nLicense:  %s\nSource:   https://example.invalid/fine\n\ntext\n' "${spdx}" >> "${f}"
  if ! output="$("${VALIDATOR}" "${f}" 2>&1)"; then
    echo "FAIL: permissive license ${spdx} rejected: ${output}" >&2; exit 1
  fi
done

# --- license BODY text naming a copyleft licence must not false-positive -----
# Many permissive license texts mention "GNU General Public License" in prose;
# only the anchored "License:  " field may trigger the policy check.
f="${TMP}/NOTICE.prose"
write_good_notice "${f}"
cat >> "${f}" <<'EOF'
Package:  github.com/example/prose
Version:  v1.0.0
License:  MIT
Source:   https://example.invalid/prose

This library is not licensed under the AGPL-3.0 or GPL-3.0; see LICENSE.

-----------------------------------------------------------------------------

EOF
if ! output="$("${VALIDATOR}" "${f}" 2>&1)"; then
  echo "FAIL: prose mentioning AGPL-3.0 triggered a false positive: ${output}" >&2; exit 1
fi

echo "validate-generated-notice tests: PASS"

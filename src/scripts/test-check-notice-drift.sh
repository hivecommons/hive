#!/usr/bin/env bash
# Regression coverage for the notice-drift failure contract (#5064).
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
CHECKER="${SCRIPT_DIR}/check-notice-drift.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

COMMITTED="${TMP}/NOTICE.committed"
GENERATED="${TMP}/NOTICE.generated"

printf 'license text\n' > "${COMMITTED}"
cp "${COMMITTED}" "${GENERATED}"
output="$("${CHECKER}" "${COMMITTED}" "${GENERATED}" 2>&1)"
grep -qF 'NOTICE matches the current module graph.' <<< "${output}"

# Whitespace-only drift is still real drift: NOTICE is compared byte-for-byte.
printf 'license text \n' > "${GENERATED}"
if output="$("${CHECKER}" "${COMMITTED}" "${GENERATED}" 2>&1)"; then
  echo "whitespace-only NOTICE drift passed unexpectedly" >&2
  exit 1
fi

for required in \
  'src/scripts/generate-notice.sh' \
  'byte-for-byte' \
  'whitespace-significant' \
  'commit the regenerated NOTICE verbatim'; do
  if ! grep -qF -- "${required}" <<< "${output}"; then
    echo "notice-drift diagnostic omitted '${required}'" >&2
    echo "output: ${output}" >&2
    exit 1
  fi
done

# A failed go-licenses source lookup renders "Source:   Unknown" where the
# committed file has a URL. That must be reported as a generator failure that
# needs a re-run — NOT as drift telling the contributor to commit the file.
write_entries() {
  # $1 = output path, $2 = Source value for pkg two
  cat > "$1" <<EOF
Package:  gopkg.in/example/one.v1
Version:  v1.0.0
License:  MIT
Source:   https://example.invalid/one/LICENSE

text

Package:  gopkg.in/example/two.v2
Version:  v2.0.0
License:  MIT
Source:   $2

text
EOF
}
write_entries "${COMMITTED}" 'https://example.invalid/two/LICENSE'
write_entries "${GENERATED}" 'Unknown'
if output="$("${CHECKER}" "${COMMITTED}" "${GENERATED}" 2>&1)"; then
  echo "generated NOTICE with an unresolved Source passed unexpectedly" >&2
  exit 1
fi
for required in \
  'generator failure, not NOTICE drift' \
  'do NOT commit the generated file' \
  'gopkg.in/example/two.v2'; do
  if ! grep -qF -- "${required}" <<< "${output}"; then
    echo "unresolved-source diagnostic omitted '${required}'" >&2
    echo "output: ${output}" >&2
    exit 1
  fi
done
if grep -qF 'commit the regenerated NOTICE verbatim' <<< "${output}"; then
  echo "unresolved-source failure was misreported as drift (told the contributor to commit it)" >&2
  exit 1
fi
if grep -qF 'gopkg.in/example/one.v1' <<< "${output}"; then
  echo "unresolved-source diagnostic named a package whose Source resolved" >&2
  exit 1
fi

# An entry that is "Unknown" in BOTH files is not a resolver regression: it is
# whatever state the committed NOTICE already records, so plain byte-diff rules.
write_entries "${COMMITTED}" 'Unknown'
write_entries "${GENERATED}" 'Unknown'
output="$("${CHECKER}" "${COMMITTED}" "${GENERATED}" 2>&1)"
grep -qF 'NOTICE matches the current module graph.' <<< "${output}"

# A package new to the graph that renders Unknown has no URL anywhere; the
# committed file records zero Unknown sources, so this too is a resolver
# failure (the validator refuses it as well) — never a commit-it diagnostic.
write_entries "${COMMITTED}" 'https://example.invalid/two/LICENSE'
write_entries "${GENERATED}" 'https://example.invalid/two/LICENSE'
printf '\nPackage:  gopkg.in/example/three.v3\nVersion:  v3.0.0\nLicense:  MIT\nSource:   Unknown\n\ntext\n' >> "${GENERATED}"
if output="$("${CHECKER}" "${COMMITTED}" "${GENERATED}" 2>&1)"; then
  echo "new package with unresolved Source passed unexpectedly" >&2
  exit 1
fi
grep -qF 'generator failure, not NOTICE drift' <<< "${output}"
grep -qF 'gopkg.in/example/three.v3' <<< "${output}"

echo "notice-drift checker tests: PASS"

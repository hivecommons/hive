#!/usr/bin/env bash
# patch-coverage.sh — calculate and report patch coverage on changed Go lines.
#
# Part of Phase 1 of issue hivecommons/hive#14161.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${SCRIPT_DIR}/patch_coverage.py" "$@"

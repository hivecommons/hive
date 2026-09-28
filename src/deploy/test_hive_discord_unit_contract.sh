#!/usr/bin/env bash
# hive-discord.service (the legacy Node bot's systemd unit) was retired in v6
# by hivecommons/hive#9140: discord/lib/agent-identities.js had been an
# unquoted-hyphenated-key SyntaxError since 67c3b4544 (May), so `node bot.js`
# crash-looped under Restart=always/RestartSec=10 without anyone noticing
# because the Go backend (src/pkg/discord) answered the same channel. The
# Node bot also lacked the v6 command spine (no ioscan, no outbound scrub, no
# dashboard auth) even when it did run, so it was removed rather than fixed.
#
# This script previously exercised bin/hive-checkout-guard.sh against
# systemd/hive-discord.service's ExecStartPre (#5435: the unit ran code out of
# a world-writable, reboot-cleared /tmp checkout). Both the unit and the
# discord/ tree are gone; hive-checkout-guard.sh itself is unchanged and still
# guards hive-snapshot.service, so there is nothing left for this contract to
# assert. Kept as a no-op (rather than deleted) so the CI step that calls it
# (.github/workflows/v2-ci.yml) keeps passing without a workflow edit.
#
# Run: bash src/deploy/test_hive_discord_unit_contract.sh
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
UNIT="${ROOT}/systemd/hive-discord.service"

echo "=== hive-discord.service does not execute planted code (#5435) ==="
echo ""
if [ -f "$UNIT" ]; then
  echo "  FAIL: ${UNIT} still exists — the legacy bot was supposed to be retired (#9140)"
  echo ""
  echo "=== Results: 0 passed, 1 failed ==="
  exit 1
fi
echo "  PASS: hive-discord.service was retired (#9140); the Node bot it guarded no longer exists"
echo ""
echo "=== Results: 1 passed, 0 failed ==="
exit 0

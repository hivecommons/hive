package hub

import (
	"os"
	"strconv"
	"time"
)

// defaultAutoUpgradeCooldown is the minimum interval between two automatic
// upgrades of the SAME hive in instant mode (#11825).
//
// The debounce (#5391) collapses a merge burst into one roll and the wave size
// bounds concurrency, but neither bounds FREQUENCY: two merges landing ~40
// minutes apart (2026-10-14: cc21ab9 at 15:52Z, 5ec6a64 at 16:27Z) retagged
// :stable twice and rolled all 44 instant hives twice. Six hours keeps an
// instant hive close to latest — at most a handful of rolls per working day —
// while turning a run of closely spaced releases into a single restart.
// Daily/weekly hives are already bounded by AutoUpgradeLastFired and are not
// subject to this gate.
const defaultAutoUpgradeCooldown = 6 * time.Hour

// autoUpgradeCooldown resolves the live per-hive cooldown from
// HIVE_UPGRADE_COOLDOWN_SECONDS, read on every trigger tick. Same conventions
// as HIVE_UPGRADE_DEBOUNCE_SECONDS: unset/0/unparseable uses the default, and a
// NEGATIVE value disables the cooldown, restoring the historical
// upgrade-on-every-target instant behaviour.
func autoUpgradeCooldown() time.Duration {
	secs := 0
	if v := os.Getenv("HIVE_UPGRADE_COOLDOWN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			secs = n
		}
	}
	if secs < 0 {
		return 0
	}
	if secs == 0 {
		return defaultAutoUpgradeCooldown
	}
	return time.Duration(secs) * time.Second
}

// autoUpgradeCooldownApplies reports whether the per-hive cooldown gates this
// hive. Only instant-mode hives are gated: daily/weekly already fire at most
// once per window. A candidate hive whose schedule is bypassed (candidate-
// continuous tracking) is a canary meant to follow every published image, so it
// is exempt too.
func autoUpgradeCooldownApplies(mode, channel string) bool {
	if channel == ReleaseChannelCandidate && !candidateAutoUpgradeScheduleOverride() {
		return false
	}
	return normalizeAutoUpgradeMode(mode) == AutoUpgradeModeInstant
}

// autoUpgradeCooldownRemaining is how long a hive that last auto-upgraded at
// lastAt must still wait. Zero means eligible now: never upgraded, cooldown
// disabled, or the cooldown has elapsed.
func autoUpgradeCooldownRemaining(lastAt time.Time, cooldown time.Duration, now time.Time) time.Duration {
	if lastAt.IsZero() || cooldown <= 0 {
		return 0
	}
	if remaining := lastAt.Add(cooldown).Sub(now); remaining > 0 {
		return remaining
	}
	return 0
}

// recordAutoUpgradeFired persists when an automatic upgrade last went out for
// this hive, so the cooldown survives a hub restart. Re-reads the record from
// disk (same pattern as the fire-date and debounce persistence) so fields
// changed by other handlers since the sweep's snapshot are not clobbered. A
// save failure is logged, not fatal — the upgrade itself still proceeds.
func (s *HubServer) recordAutoUpgradeFired(h *SaaSHive, at time.Time) {
	if h == nil {
		return
	}
	h.AutoUpgradeLastAt = at
	stored := loadSaaSHive(h.ID)
	if stored == nil {
		stored = h
	}
	stored.AutoUpgradeLastAt = at
	if err := saveSaaSHive(stored); err != nil {
		s.logger.Warn("failed to persist auto-upgrade timestamp — a hub restart could skip the cooldown",
			"hive_id", h.ID, "error", err)
	}
}

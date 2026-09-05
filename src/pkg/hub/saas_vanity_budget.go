package hub

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func vanityRepairSuccessCooldown() time.Duration {
	return durationEnv(vanityRepairSuccessCooldownEnv, vanityRepairSuccessCooldownDefault)
}

func vanityRepairFailureBackoff() time.Duration {
	return durationEnv(vanityRepairFailureBackoffEnv, vanityRepairFailureBackoffDefault)
}

func vanityMintBudget() int {
	return positiveIntEnv(vanityMintBudgetEnv, vanityMintBudgetDefault)
}

func vanityMintWindow() time.Duration {
	return durationEnv(vanityMintWindowEnv, vanityMintWindowDefault)
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

func positiveIntEnv(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

func vanityMintLedgerPath() string {
	return filepath.Join(filepath.Dir(saasHivesDir), "vanity-mint-times.json")
}

// vanityMintAllowed reports whether the fleet-wide mint budget has room for
// another vanity-host mint, pruning entries older than the rolling window.
func (s *HubServer) vanityMintAllowed() bool {
	s.vanityMintMu.Lock()
	defer s.vanityMintMu.Unlock()
	s.ensureVanityMintLedgerLoadedLocked()
	s.pruneVanityMintsLocked()
	return len(s.vanityMintTimes) < vanityMintBudget()
}

// recordVanityMint charges one mint against the fleet-wide budget.
func (s *HubServer) recordVanityMint() {
	s.vanityMintMu.Lock()
	defer s.vanityMintMu.Unlock()
	s.ensureVanityMintLedgerLoadedLocked()
	s.pruneVanityMintsLocked()
	s.vanityMintTimes = append(s.vanityMintTimes, time.Now())
	s.saveVanityMintLedgerLocked()
}

// acquireVanityMintSlot atomically reserves one fleet-wide mint slot before
// the repair path mutates cluster ingress. The reservation happens before the
// slow kubectl call so concurrent repairs cannot all observe the same free slot
// and collectively exceed the ACME-protecting cap.
func (s *HubServer) acquireVanityMintSlot() bool {
	s.vanityMintMu.Lock()
	defer s.vanityMintMu.Unlock()
	s.ensureVanityMintLedgerLoadedLocked()
	s.pruneVanityMintsLocked()
	if len(s.vanityMintTimes) >= vanityMintBudget() {
		return false
	}
	s.vanityMintTimes = append(s.vanityMintTimes, time.Now())
	s.saveVanityMintLedgerLocked()
	return true
}

// vanityMintRemaining returns how many mints the budget has left in the
// current window, for the mint log line — the visibility #5923 asked for.
func (s *HubServer) vanityMintRemaining() int {
	s.vanityMintMu.Lock()
	defer s.vanityMintMu.Unlock()
	s.ensureVanityMintLedgerLoadedLocked()
	s.pruneVanityMintsLocked()
	if r := vanityMintBudget() - len(s.vanityMintTimes); r > 0 {
		return r
	}
	return 0
}

// pruneVanityMintsLocked drops mint timestamps that have aged out of the
// rolling window. Callers must hold vanityMintMu.
func (s *HubServer) pruneVanityMintsLocked() {
	cutoff := time.Now().Add(-vanityMintWindow())
	kept := s.vanityMintTimes[:0]
	for _, t := range s.vanityMintTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	changed := len(kept) != len(s.vanityMintTimes)
	s.vanityMintTimes = kept
	if changed {
		s.saveVanityMintLedgerLocked()
	}
}

func (s *HubServer) ensureVanityMintLedgerLoadedLocked() {
	if s.vanityMintLedgerLoaded || len(s.vanityMintTimes) > 0 {
		s.vanityMintLedgerLoaded = true
		return
	}
	data, err := os.ReadFile(vanityMintLedgerPath())
	if err != nil {
		s.vanityMintLedgerLoaded = true
		return
	}
	var times []time.Time
	if err := json.Unmarshal(data, &times); err != nil {
		s.logger.Warn("vanity mint budget: failed to parse persisted ledger", "path", vanityMintLedgerPath(), "error", err)
		s.vanityMintLedgerLoaded = true
		return
	}
	s.vanityMintTimes = times
	s.vanityMintLedgerLoaded = true
}

func (s *HubServer) saveVanityMintLedgerLocked() {
	path := vanityMintLedgerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.logger.Warn("vanity mint budget: failed to create ledger directory", "path", path, "error", err)
		return
	}
	data, err := json.MarshalIndent(s.vanityMintTimes, "", "  ")
	if err != nil {
		s.logger.Warn("vanity mint budget: failed to marshal ledger", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		s.logger.Warn("vanity mint budget: failed to write ledger", "path", path, "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		s.logger.Warn("vanity mint budget: failed to replace ledger", "path", path, "error", err)
	}
}

func (s *HubServer) recordVanityRepairFailure(hiveID, reason string) {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return
	}
	h.LastVanityRepairFailureAt = time.Now()
	h.LastVanityRepairFailure = reason
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("vanity url repair: failed to persist repair failure backoff", "hive", hiveID, "error", err)
	}
}

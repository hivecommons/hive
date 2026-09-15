package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReconcileUpgradeOutcomeAtBoot pins the success-detection the spoke needs
// for #7092: an in-flight marker whose target IS the now-running commit means
// the upgrade LANDED, so a durable success record is written and the marker
// cleared. A marker whose target does NOT match (still in flight, or failed) is
// left untouched, and an absent marker records nothing.
func TestReconcileUpgradeOutcomeAtBoot(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	reqAt := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

	writeMarker := func(t *testing.T, path string, m upgradeMarker) {
		t.Helper()
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("landed target records success and clears marker", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "upgrade-requested")
		outcome := filepath.Join(dir, "last-upgrade-outcome")
		writeMarker(t, marker, upgradeMarker{
			TargetSHA: "abc1234", CurrentSHA: "def5678", RequestedAt: reqAt, Attempts: 1,
		})

		reconcileUpgradeOutcomeAtBoot(marker, outcome, "abc1234", logger)

		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("in-flight marker should be cleared on success, stat err = %v", err)
		}
		data, err := os.ReadFile(outcome)
		if err != nil {
			t.Fatalf("outcome record not written: %v", err)
		}
		var o upgradeOutcome
		if err := json.Unmarshal(data, &o); err != nil {
			t.Fatal(err)
		}
		if o.TargetSHA != "abc1234" || o.CurrentSHA != "def5678" || !o.RequestedAt.Equal(reqAt) {
			t.Errorf("outcome = %+v, want target abc1234 from def5678 requested %v", o, reqAt)
		}
		if o.CompletedAt.IsZero() {
			t.Errorf("outcome must stamp a completion time")
		}
	})

	t.Run("short/full SHA mismatch still counts as landed", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "upgrade-requested")
		outcome := filepath.Join(dir, "last-upgrade-outcome")
		writeMarker(t, marker, upgradeMarker{TargetSHA: "abc1234def", CurrentSHA: "old", RequestedAt: reqAt, Attempts: 1})

		reconcileUpgradeOutcomeAtBoot(marker, outcome, "abc1234", logger)

		if _, err := os.Stat(outcome); err != nil {
			t.Errorf("landed (prefix match) should record success: %v", err)
		}
	})

	t.Run("unlanded target leaves marker and writes no outcome", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "upgrade-requested")
		outcome := filepath.Join(dir, "last-upgrade-outcome")
		writeMarker(t, marker, upgradeMarker{TargetSHA: "abc1234", CurrentSHA: "old", RequestedAt: reqAt, Attempts: 5})

		reconcileUpgradeOutcomeAtBoot(marker, outcome, "still-old", logger)

		if _, err := os.Stat(marker); err != nil {
			t.Errorf("unlanded marker must be preserved: %v", err)
		}
		if _, err := os.Stat(outcome); !os.IsNotExist(err) {
			t.Errorf("no success should be recorded for an unlanded upgrade")
		}
	})

	t.Run("absent marker is a no-op", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "upgrade-requested")
		outcome := filepath.Join(dir, "last-upgrade-outcome")

		reconcileUpgradeOutcomeAtBoot(marker, outcome, "abc1234", logger)

		if _, err := os.Stat(outcome); !os.IsNotExist(err) {
			t.Errorf("no marker present: nothing should be written")
		}
	})
}

package hub

// Retention for the copies colony-only keeps or sets aside (#20114):
// users.migrated*, consents.migrated*, consents.pre-restore* and
// consents.ignored* hold personal data, so they are removed by the consent
// sweep once customerConsentRetention has passed since they were set aside.

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// hubKeptCopy is one glob of kept copies in the hub data directory.
type hubKeptCopy struct {
	pattern string
	// migrated copies are kept from the migration: their clock starts no
	// earlier than the colony-only document's migrated_at.
	migrated bool
}

// hubKeptCopies lists every copy colony-only keeps or sets aside: .migrated,
// and the consents directories a restore or a colony-only boot set aside.
func hubKeptCopies() []hubKeptCopy {
	return []hubKeptCopy{
		{filepath.Base(saasUsersDir) + hubMigratedSuffix + "*", true},
		{filepath.Base(customerConsentDir()) + hubMigratedSuffix + "*", true},
		{filepath.Base(customerConsentDir()) + hubRestoredAsideSuffix + "*", false},
		{filepath.Base(customerConsentDir()) + hubIgnoredSuffix + "*", false},
	}
}

// markHubKeptCopy starts a kept copy's retention clock: its modification
// time becomes the time it was set aside (a rename keeps the old one).
func markHubKeptCopy(path string, now time.Time) {
	if err := os.Chtimes(path, now, now); err != nil {
		slog.Warn("hub customer store: a kept copy was not stamped; its retention runs from its last change", "error", crmErrClass(err))
	}
}

// pruneHubKeptCopies removes every kept copy set aside at least
// customerConsentRetention before now, and reports how many it removed. It
// removes nothing while a colony.db on disk is not open: a migration that
// did not finish may have left consents.migrated as the only consent
// records, and only the boot that opens colony.db puts it back.
func pruneHubKeptCopies(now time.Time) (int, error) {
	if crmStoreUnavailable() {
		return 0, nil
	}
	var migratedAt time.Time
	if st, _ := hubCRM.active(); st != nil {
		doc, ok, err := hubColonyOnlyDocOf(st)
		if err != nil {
			return 0, err
		}
		if ok {
			migratedAt = doc.MigratedAt
		}
	}
	saasDir := filepath.Dir(saasUsersDir)
	removed := 0
	for _, k := range hubKeptCopies() {
		matches, err := filepath.Glob(filepath.Join(saasDir, k.pattern))
		if err != nil {
			return removed, err
		}
		for _, d := range matches {
			fi, err := os.Lstat(d)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return removed, err
			}
			if !fi.IsDir() {
				continue
			}
			since := fi.ModTime()
			if k.migrated && migratedAt.After(since) {
				since = migratedAt
			}
			if now.Before(since.Add(customerConsentRetention)) {
				continue
			}
			if err := os.RemoveAll(d); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

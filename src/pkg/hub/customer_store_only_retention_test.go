package hub

// Tests for the retention of the copies colony-only keeps or sets aside
// (#20114). Every person here is synthetic.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// keptCopy makes a directory name beside /data/saas/users holding one
// synthetic file, last changed at mtime.
func keptCopy(t *testing.T, name string, mtime time.Time) string {
	t.Helper()
	d := filepath.Join(filepath.Dir(saasUsersDir), name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "ada-example.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(d, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return d
}

func keptCopyExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// Every kind of kept copy goes once the retention window has passed since
// it was set aside; a newer one and the live consents directory stay.
func TestPruneHubKeptCopiesAfterRetention(t *testing.T) {
	defer helperSetupTempDirs(t)()
	now := time.Now()
	old := now.Add(-customerConsentRetention - time.Hour)
	expired := []string{
		keptCopy(t, "users"+hubMigratedSuffix, old),
		keptCopy(t, "consents"+hubMigratedSuffix+"-20260101T000000Z", old),
		keptCopy(t, "consents"+hubRestoredAsideSuffix, old),
		keptCopy(t, "consents"+hubIgnoredSuffix, old),
	}
	recent := keptCopy(t, "consents"+hubIgnoredSuffix+"-20261101T000000Z", now.Add(-time.Hour))
	live := keptCopy(t, "consents", old)

	n, err := pruneHubKeptCopies(now)
	if err != nil || n != len(expired) {
		t.Fatalf("pruned %d, %v; want %d", n, err, len(expired))
	}
	for _, d := range expired {
		if keptCopyExists(d) {
			t.Errorf("%s outlived the retention window", filepath.Base(d))
		}
	}
	if !keptCopyExists(recent) {
		t.Error("a copy inside the retention window was removed")
	}
	if !keptCopyExists(live) {
		t.Error("the live consents directory was removed")
	}
}

// A colony.db this boot did not open may name consents.migrated as the only
// consent records (an unfinished migration): nothing is pruned.
func TestPruneHubKeptCopiesWaitsForUnopenedStore(t *testing.T) {
	defer helperSetupTempDirs(t)()
	d := keptCopy(t, "consents"+hubMigratedSuffix, time.Now().Add(-2*customerConsentRetention))
	if err := os.WriteFile(hubCustomerStorePath(), []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := pruneHubKeptCopies(time.Now()); err != nil || n != 0 || !keptCopyExists(d) {
		t.Errorf("pruned %d, %v with colony.db not open; want nothing", n, err)
	}
}

// A directory set aside (by a colony-only boot or a rollback) starts its
// retention clock then, not at its last change, and goes once the window
// has passed, with the migration's own copies.
func TestHubKeptCopyRetentionStartsWhenSetAside(t *testing.T) {
	for _, tc := range []struct{ mode, aside string }{
		{hubCustomerStoreColonyOnly, hubIgnoredSuffix},
		{hubCustomerStoreColony, hubRestoredAsideSuffix},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			defer helperSetupTempDirs(t)()
			fixtureUsers(t)
			st := useHubCustomerStore(t, hubCustomerStoreColonyOnly)
			at := hubMigratedAt(t, st)
			closeHubCustomerStore()
			writeReappearedConsents(t, `{}`, at.Add(-2*customerConsentRetention))
			useHubCustomerStore(t, tc.mode)
			aside := customerConsentDir() + tc.aside
			if !keptCopyExists(aside) {
				t.Fatalf("consents%s was not set aside", tc.aside)
			}
			if _, err := pruneHubKeptCopies(time.Now()); err != nil {
				t.Fatal(err)
			}
			if !keptCopyExists(aside) {
				t.Fatal("a copy set aside just now was pruned by its last change")
			}
			if _, err := pruneHubKeptCopies(time.Now().Add(customerConsentRetention + time.Hour)); err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{aside, migratedDir("users"), migratedDir("consents")} {
				if keptCopyExists(d) {
					t.Errorf("%s outlived the retention window", filepath.Base(d))
				}
			}
		})
	}
}

// Removing a deleted user's copy does not restart the retention clock.
func TestScrubHubMigratedKeepsRetentionClock(t *testing.T) {
	defer helperSetupTempDirs(t)()
	users := fixtureUsers(t)
	old := time.Now().Add(-customerConsentRetention - time.Hour)
	d := keptCopy(t, "consents"+hubIgnoredSuffix, old)
	if err := scrubHubMigrated(users[0]); err != nil {
		t.Fatal(err)
	}
	if keptCopyExists(filepath.Join(d, "ada-example.json")) {
		t.Fatal("the deleted user's copy is still there")
	}
	fi, err := os.Stat(d)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(fi.ModTime()) < customerConsentRetention {
		t.Errorf("a scrub restarted the retention clock: mtime %v", fi.ModTime())
	}
}

// The consent sweep drives the prune.
func TestCustomerConsentSweepPrunesKeptCopies(t *testing.T) {
	defer helperSetupTempDirs(t)()
	d := keptCopy(t, "consents"+hubRestoredAsideSuffix, time.Now().Add(-customerConsentRetention-time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	(&HubServer{logger: slog.Default()}).startCustomerConsentSweep(ctx)
	if keptCopyExists(d) {
		t.Error("the consent sweep kept a copy past the retention window")
	}
}

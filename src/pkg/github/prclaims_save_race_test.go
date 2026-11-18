package github

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent Saves must not lose a shared temp file (hivecommons/hive#18975).
func TestClaimLedgerSaveConcurrent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pr-claims.json")
	l := NewClaimLedger(path, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := l.Save(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Save failed: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the ledger file, got %d entries", len(entries))
	}
}

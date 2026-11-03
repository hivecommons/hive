package github

import (
	"path/filepath"
	"sync"
	"testing"
)

// Two lock handles on one directory exclude each other even within a process,
// so this exercises the same flock path two processes would contend on.
func TestLockReviewEvidenceDirSerializesHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "o", "r", "5", "abc.json")
	const workers = 8
	var (
		wg      sync.WaitGroup
		inside  int
		maxSeen int
		mu      sync.Mutex
		counter int
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := lockReviewEvidenceDir(path)
			defer unlock()
			mu.Lock()
			inside++
			if inside > maxSeen {
				maxSeen = inside
			}
			mu.Unlock()
			counter++ // unsynchronized on purpose: only the flock protects it
			mu.Lock()
			inside--
			mu.Unlock()
		}()
	}
	wg.Wait()
	if maxSeen != 1 {
		t.Fatalf("lock admitted %d holders at once, want 1", maxSeen)
	}
	if counter != workers {
		t.Fatalf("counter = %d, want %d", counter, workers)
	}
}

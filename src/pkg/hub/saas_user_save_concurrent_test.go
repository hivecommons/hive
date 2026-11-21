package hub

import (
	"path/filepath"
	"sync"
	"testing"
)

// Concurrent saves of one user record must all succeed (#19346): they used to
// share a single "<path>.tmp" and one writer's rename removed the other's file.
func TestSaveSaaSUserConcurrentSavesSucceed(t *testing.T) {
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	defer func() { saasUsersDir = orig }()

	const n = 64
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- saveSaaSUser(&SaaSUser{GitHubUsername: "racer"})
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent save failed: %v", err)
		}
	}
	if loadSaaSUser("racer") == nil {
		t.Fatal("record missing after concurrent saves")
	}
	left, _ := filepath.Glob(filepath.Join(saasUsersDir, "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("leftover temp files: %v", left)
	}
}

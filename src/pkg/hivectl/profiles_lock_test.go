package hivectl

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
)

// Cross-process safety for profiles.yml (#13179): CLI, TUI and Web writers
// share one flock and an expected-revision check, so no edit is lost.

func TestCommitExpectingStaleRevisionLeavesFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := NewProfileStore(dir).Save(opsSet()); err != nil {
		t.Fatal(err)
	}
	mine, theirs := NewProfileStore(dir), NewProfileStore(dir)
	stale, err := mine.Load()
	if err != nil {
		t.Fatal(err)
	}
	other, err := theirs.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SetCommonsStrategy(CommonsStrategySpread); err != nil {
		t.Fatal(err)
	}
	if err := theirs.CommitExpecting(other, other.Revision()); err != nil {
		t.Fatalf("fresh commit: %v", err)
	}
	before, err := os.ReadFile(mine.Path())
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := stale.Remove("other"); err != nil {
		t.Fatal(err)
	}
	err = mine.CommitExpecting(stale, stale.Revision())
	if !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("stale commit err = %v, want ErrStaleRevision", err)
	}
	after, err := os.ReadFile(mine.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("stale commit rewrote profiles.yml:\n%s", after)
	}
}

func TestCommitExpectingFreshRevisionCommitsAndAdvances(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	if err := store.Save(opsSet()); err != nil {
		t.Fatal(err)
	}
	set, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	loaded := set.Revision()
	if loaded == "" {
		t.Fatal("loaded set has no revision")
	}
	if _, _, err := set.Use("other"); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitExpecting(set, loaded); err != nil {
		t.Fatalf("fresh commit: %v", err)
	}
	if set.Revision() == loaded {
		t.Fatal("revision did not advance after commit")
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Active != "other" || reloaded.Revision() != set.Revision() {
		t.Fatalf("reloaded active/revision = %q/%q, want other/%q", reloaded.Active, reloaded.Revision(), set.Revision())
	}
	// The committed set carries its new revision, so a second edit in the
	// same session is not mistaken for a stale one.
	if _, _, err := set.Use("acme"); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitExpecting(set, set.Revision()); err != nil {
		t.Fatalf("second commit: %v", err)
	}
}

func TestCommitExpectingEmptyRevisionMeansNoFileYet(t *testing.T) {
	dir := t.TempDir()
	first := &ProfileSet{Version: ProfilesVersion}
	if err := first.Add(Profile{Name: "acme", Hub: "wss://acme.example/contribute", RegistrationToken: "t1"}, true); err != nil {
		t.Fatal(err)
	}
	if err := NewProfileStore(dir).CommitExpecting(first, ""); err != nil {
		t.Fatalf("first add: %v", err)
	}
	second := &ProfileSet{Version: ProfilesVersion}
	if err := second.Add(Profile{Name: "other", Hub: "wss://other.example/contribute", RegistrationToken: "t2"}, true); err != nil {
		t.Fatal(err)
	}
	if err := NewProfileStore(dir).CommitExpecting(second, ""); !errors.Is(err, ErrStaleRevision) {
		t.Fatalf("racing first add err = %v, want ErrStaleRevision", err)
	}
}

func TestConcurrentCommitsAreSerialisedByTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := NewProfileStore(dir).Save(opsSet()); err != nil {
		t.Fatal(err)
	}
	const writers, rounds = 4, 10
	start := make(chan struct{})
	errs := make(chan error, writers*rounds)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store := NewProfileStore(dir)
			<-start
			for i := 0; i < rounds; i++ {
				// A set that never read the file: only the lock keeps the
				// scope-version read-increment-write from losing updates.
				errs <- store.Commit(opsSet())
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent commit: %v", err)
		}
	}
	final, err := NewProfileStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if final.RepoScopeVersion != writers*rounds {
		t.Fatalf("repo scope version = %d, want %d (an update was lost)", final.RepoScopeVersion, writers*rounds)
	}
}

func TestConcurrentExpectingWritersNeverLoseAnEdit(t *testing.T) {
	dir := t.TempDir()
	if err := NewProfileStore(dir).Save(opsSet()); err != nil {
		t.Fatal(err)
	}
	const writers, rounds = 3, 5
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			store := NewProfileStore(dir)
			<-start
			for i := 0; i < rounds; i++ {
				for {
					set, err := store.Load()
					if err != nil {
						errs <- err
						return
					}
					name := fmt.Sprintf("w%d-%d", w, i)
					if err := set.Add(Profile{Name: name, Hub: "wss://" + name + ".example/contribute", RegistrationToken: "t"}, false); err != nil {
						errs <- err
						return
					}
					err = store.CommitExpecting(set, set.Revision())
					if errors.Is(err, ErrStaleRevision) {
						continue
					}
					if err != nil {
						errs <- err
						return
					}
					break
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("writer: %v", err)
	}
	final, err := NewProfileStore(dir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := len(opsSet().Profiles) + writers*rounds; len(final.Profiles) != want {
		t.Fatalf("profiles = %d, want %d (an edit was lost)", len(final.Profiles), want)
	}
}

func TestProfilesLockIsOwnerOnlyAndExcludesOtherDescriptors(t *testing.T) {
	store := NewProfileStore(t.TempDir())
	if err := store.Commit(opsSet()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.LockPath())
	if err != nil {
		t.Fatalf("lock file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("lock file mode = %o, want 600", perm)
	}

	unlock, err := store.lock()
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Open(store.LockPath())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("second descriptor flock while held = %v, want EWOULDBLOCK", err)
	}
	unlock()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock after release = %v", err)
	}
	_ = syscall.Flock(int(other.Fd()), syscall.LOCK_UN)
}

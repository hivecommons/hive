package hub

// Boot retry for a colony.db another process holds (#20103). During a hub pod
// handover the old pod keeps the bbolt lock until it exits; one failed open
// must not leave the new pod in json mode for its whole life.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/colony/store"
)

// fastHubCustomerStoreLockTiming shrinks the per-attempt lock wait and the
// backoff so the retry tests run in about a second, and sets the retry window.
func fastHubCustomerStoreLockTiming(t *testing.T, wait string) {
	t.Helper()
	lockTimeout, backoff, backoffMax := hubCustomerStoreLockTimeout, hubCustomerStoreRetryBackoff, hubCustomerStoreRetryBackoffMax
	hubCustomerStoreLockTimeout = 50 * time.Millisecond
	hubCustomerStoreRetryBackoff = 50 * time.Millisecond
	hubCustomerStoreRetryBackoffMax = 200 * time.Millisecond
	t.Cleanup(func() {
		hubCustomerStoreLockTimeout, hubCustomerStoreRetryBackoff, hubCustomerStoreRetryBackoffMax = lockTimeout, backoff, backoffMax
	})
	t.Setenv(hubCustomerStoreLockWaitEnv, wait)
}

// holdHubCustomerStoreLock opens colony.db as a second process would (the old
// pod) and returns a func that releases it. Releasing twice is safe.
func holdHubCustomerStoreLock(t *testing.T) func() {
	t.Helper()
	holder, err := store.Open(hubCustomerStorePath(), store.Options{LockTimeout: hubCustomerStoreLockTimeout})
	if err != nil {
		t.Fatalf("hold colony.db lock: %v", err)
	}
	released := make(chan struct{})
	release := func() {
		select {
		case <-released:
		default:
			close(released)
			_ = holder.Close()
		}
	}
	t.Cleanup(release)
	return release
}

// A lock the old pod releases after a second ends in colony mode, not json.
func TestHubCustomerStoreLockReleasedDuringRetryOpensColony(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	fastHubCustomerStoreLockTiming(t, "30s")
	release := holdHubCustomerStoreLock(t)
	const holdFor = time.Second
	timer := time.AfterFunc(holdFor, release)
	defer timer.Stop()

	start := time.Now()
	t.Cleanup(closeHubCustomerStore)
	got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColony)
	if got != hubCustomerStoreColony {
		t.Fatalf("mode = %q after the lock was released, want colony", got)
	}
	if elapsed := time.Since(start); elapsed < holdFor {
		t.Fatalf("opened after %v, before the holder released the lock at %v", elapsed, holdFor)
	}
	if st, mode := hubCRM.active(); st == nil || mode != hubCustomerStoreColony {
		t.Fatalf("active store = %v, mode %q; want an open colony store", st, mode)
	}
	if u := loadSaaSUser("ada-example"); u == nil || u.FullName != "Ada Example" {
		t.Fatalf("read after retried open = %+v", u)
	}
}

// A lock still held when the retry window ends falls back to json.
func TestHubCustomerStoreLockHeldPastWaitFallsBackToJSON(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	fastHubCustomerStoreLockTiming(t, "300ms")
	holdHubCustomerStoreLock(t)

	t.Cleanup(closeHubCustomerStore)
	if got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColony); got != hubCustomerStoreJSON {
		t.Fatalf("mode = %q with the lock held past the wait, want json", got)
	}
	if st, _ := hubCRM.active(); st != nil {
		t.Fatal("a store is active although the open failed")
	}
}

// A wait of 0 keeps the old behaviour: one attempt, then json.
func TestHubCustomerStoreLockWaitZeroDoesNotRetry(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	fastHubCustomerStoreLockTiming(t, "0")
	// A retry would pause at least this long.
	hubCustomerStoreRetryBackoff = 20 * time.Second
	holdHubCustomerStoreLock(t)

	start := time.Now()
	t.Cleanup(closeHubCustomerStore)
	if got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColony); got != hubCustomerStoreJSON {
		t.Fatalf("mode = %q, want json", got)
	}
	if elapsed := time.Since(start); elapsed >= hubCustomerStoreRetryBackoff {
		t.Fatalf("took %v: a wait of 0 must not retry", elapsed)
	}
}

// Only a held lock is retried: an unreadable file fails closed at once.
func TestHubCustomerStoreNonLockErrorIsNotRetried(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	fastHubCustomerStoreLockTiming(t, "1h")
	hubCustomerStoreRetryBackoff = 20 * time.Second
	if err := os.WriteFile(hubCustomerStorePath(), []byte("not a bbolt file, synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	t.Cleanup(closeHubCustomerStore)
	if got := openHubCustomerStore(context.Background(), nil, hubCustomerStoreColony); got != hubCustomerStoreJSON {
		t.Fatalf("mode = %q, want json", got)
	}
	if elapsed := time.Since(start); elapsed >= hubCustomerStoreRetryBackoff {
		t.Fatalf("took %v: a non-lock open error must not be retried", elapsed)
	}
}

// Shutdown during the wait stops the retry instead of holding the boot.
func TestHubCustomerStoreLockRetryStopsOnCancel(t *testing.T) {
	defer helperSetupTempDirs(t)()
	fixtureUsers(t)
	fastHubCustomerStoreLockTiming(t, "1h")
	hubCustomerStoreRetryBackoff = 20 * time.Second
	holdHubCustomerStoreLock(t)
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(200*time.Millisecond, cancel)
	defer timer.Stop()
	defer cancel()

	start := time.Now()
	t.Cleanup(closeHubCustomerStore)
	if got := openHubCustomerStore(ctx, nil, hubCustomerStoreColony); got != hubCustomerStoreJSON {
		t.Fatalf("mode = %q, want json", got)
	}
	if elapsed := time.Since(start); elapsed >= hubCustomerStoreRetryBackoff {
		t.Fatalf("took %v: cancel must end the retry", elapsed)
	}
}

func TestHubCustomerStoreLockWaitEnv(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"", hubCustomerStoreLockWaitDefault},
		{"0", 0},
		{"45s", 45 * time.Second},
		{" 3m ", 3 * time.Minute},
		{"-1s", hubCustomerStoreLockWaitDefault},
		{"soon", hubCustomerStoreLockWaitDefault},
	} {
		t.Setenv(hubCustomerStoreLockWaitEnv, tc.raw)
		if got := hubCustomerStoreLockWait(slog.New(slog.NewTextHandler(io.Discard, nil))); got != tc.want {
			t.Errorf("%s=%q: wait = %v, want %v", hubCustomerStoreLockWaitEnv, tc.raw, got, tc.want)
		}
	}
}

package mutation

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const ttl = time.Hour

func openLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := OpenLedger(filepath.Join(t.TempDir(), "claims.json"), 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	return l
}

// Row: two claimants race — one epoch wins; the loser cannot mutate.
func TestLedger_TwoClaimantsRace_OneEpochWins(t *testing.T) {
	l := openLedger(t)
	c := TaskClaim("acme/widgets", "acme/widgets#7")
	now := time.Now()

	var wg sync.WaitGroup
	wins := make(chan Entry, 2)
	for _, holder := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			if g, err := l.Acquire(c, h, ttl, now); err == nil {
				wins <- g
			}
		}(holder)
	}
	wg.Wait()
	close(wins)
	var grants []Entry
	for g := range wins {
		grants = append(grants, g)
	}
	if len(grants) != 1 {
		t.Fatalf("exactly one claimant must win, got %d", len(grants))
	}
	if err := l.ValidateEpoch(c.Key(), grants[0].Epoch, now); err != nil {
		t.Fatalf("winner's epoch must validate: %v", err)
	}
}

// Row: overlap — a repo-top claim conflicts with any narrower claim; disjoint
// claims proceed concurrently as the positive control... except when the
// writer capacity (default 1 per repo) is consumed, in which case the second
// same-repo writer waits WITHOUT consuming capacity.
func TestLedger_OverlapAndCapacity(t *testing.T) {
	// Capacity 2 so overlap (not capacity) is what refuses the top claim.
	l, err := OpenLedger(filepath.Join(t.TempDir(), "claims.json"), 2)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	now := time.Now()
	task7 := TaskClaim("acme/widgets", "acme/widgets#7")
	task9 := TaskClaim("acme/widgets", "acme/widgets#9")
	other := TaskClaim("acme/gadgets", "acme/gadgets#1")

	if _, err := l.Acquire(task7, "alice", ttl, now); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := l.Acquire(RepoClaim("acme/widgets"), "carol", ttl, now); !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("repo-top claim must conflict with a held task claim: %v", err)
	}
	if _, err := l.Acquire(task9, "bob", ttl, now); err != nil {
		t.Fatalf("disjoint same-repo claim within capacity must proceed: %v", err)
	}
	if _, err := l.Acquire(other, "dave", ttl, now); err != nil {
		t.Fatalf("disjoint other-repo claim must proceed: %v", err)
	}
	// Both widgets slots consumed now: a third disjoint claim is a capacity
	// wait, not an overlap conflict.
	task11 := TaskClaim("acme/widgets", "acme/widgets#11")
	if _, err := l.Acquire(task11, "erin", ttl, now); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("exhausted writer slots must refuse with ErrNoCapacity: %v", err)
	}
}

// Row: a repo-top hold blocks every narrower claim in that repo.
func TestLedger_TopClaimBlocksNarrower(t *testing.T) {
	l := openLedger(t)
	now := time.Now()
	if _, err := l.Acquire(RepoClaim("acme/widgets"), "release-bot", ttl, now); err != nil {
		t.Fatalf("acquire top: %v", err)
	}
	if _, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#7"), "alice", ttl, now); !errors.Is(err, ErrClaimHeld) {
		t.Fatalf("narrow claim must not bypass the top claim: %v", err)
	}
	// Unrelated repo proceeds: reject-everything control.
	if _, err := l.Acquire(TaskClaim("acme/gadgets", "acme/gadgets#1"), "bob", ttl, now); err != nil {
		t.Fatalf("unrelated repo must proceed: %v", err)
	}
}

// Row: waiting releases capacity and fences the epoch; reacquisition mints a
// strictly higher epoch; every stale-epoch action is fenced.
func TestLedger_WaitReleasesCapacityAndFences(t *testing.T) {
	l := openLedger(t)
	now := time.Now()
	c := TaskClaim("acme/widgets", "acme/widgets#7")
	g1, err := l.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Enter CI/review wait: slot freed, epoch fenced.
	if _, err := l.Wait(c.Key(), g1.Epoch, now); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if err := l.ValidateEpoch(c.Key(), g1.Epoch, now); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("a waiting hold must not authorize mutation: %v", err)
	}
	// The freed slot is usable by a disjoint claim (waiters consume nothing).
	gBob, err := l.Acquire(TaskClaim("acme/widgets", "acme/widgets#9"), "bob", ttl, now)
	if err != nil {
		t.Fatalf("freed capacity must be available: %v", err)
	}
	if _, err := l.Release(TaskClaim("acme/widgets", "acme/widgets#9").Key(), gBob.Epoch, now); err != nil {
		t.Fatalf("release bob: %v", err)
	}
	if _, err := l.Release(c.Key()+"", g1.Epoch, now); err == nil {
		// Release from Waiting is allowed at the SAME epoch (it is the
		// recorded epoch), so this must succeed.
	} else {
		t.Fatalf("release from waiting: %v", err)
	}

	// Reacquire: strictly higher epoch; the old epoch stays fenced forever.
	g2, err := l.Acquire(c, "alice", ttl, now)
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	if g2.Epoch <= g1.Epoch {
		t.Fatalf("reacquisition must mint a strictly higher epoch: %d then %d", g1.Epoch, g2.Epoch)
	}
	if err := l.ValidateEpoch(c.Key(), g1.Epoch, now); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("old epoch must remain fenced: %v", err)
	}
	if err := l.ValidateEpoch(c.Key(), g2.Epoch, now); err != nil {
		t.Fatalf("current epoch must validate: %v", err)
	}
	// A stale-epoch state transition is fenced too.
	if _, err := l.Wait(c.Key(), g1.Epoch, now); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("stale wait must be fenced: %v", err)
	}
}

// Row: lease expiry — the expired hold is fenced (Waiting, never silently
// Released) and the replacement receives a strictly higher epoch.
func TestLedger_ExpiryFencesAndReplacementIsHigher(t *testing.T) {
	l := openLedger(t)
	now := time.Now()
	c := TaskClaim("acme/widgets", "acme/widgets#7")
	g1, err := l.Acquire(c, "alice", time.Minute, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	later := now.Add(2 * time.Minute)
	if err := l.ValidateEpoch(c.Key(), g1.Epoch, later); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("expired hold must be fenced: %v", err)
	}
	e, _ := l.Get(c.Key())
	if e.State != StateWaiting {
		t.Fatalf("expiry reconciles to Waiting (fenced), never silent release: %s", e.State)
	}
	g2, err := l.Acquire(c, "bob", ttl, later)
	if err != nil {
		t.Fatalf("replacement acquire: %v", err)
	}
	if g2.Epoch <= g1.Epoch {
		t.Fatalf("replacement epoch must be strictly higher")
	}
}

// Row: restart — claim, epoch, and state reconstruct from durable state; an
// entry active at shutdown whose window lapsed reconciles fenced.
func TestLedger_RestartReconstructs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	l, err := OpenLedger(path, 0)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}
	now := time.Now()
	c := TaskClaim("acme/widgets", "acme/widgets#7")
	g, err := l.Acquire(c, "alice", time.Minute, now)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	reopened, err := OpenLedger(path, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := reopened.ValidateEpoch(c.Key(), g.Epoch, now); err != nil {
		t.Fatalf("fresh hold must survive restart: %v", err)
	}
	// After the window lapses across a restart, the old owner is fenced and
	// a replacement mints a higher epoch — process memory was never authority.
	later := now.Add(2 * time.Minute)
	if err := reopened.ValidateEpoch(c.Key(), g.Epoch, later); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("lapsed hold must be fenced after restart: %v", err)
	}
	g2, err := reopened.Acquire(c, "bob", ttl, later)
	if err != nil {
		t.Fatalf("post-restart reacquire: %v", err)
	}
	if g2.Epoch <= g.Epoch {
		t.Fatalf("epoch monotonicity must survive restart")
	}
}

// Row: corrupt durable ownership refuses visibly; the bytes stay untouched.
func TestLedger_CorruptFileRefuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.json")
	if err := writeFile(path, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(path, 0); err == nil {
		t.Fatal("corrupt ledger must refuse to open")
	}
	if got := readFile(t, path); got != "{not json" {
		t.Fatalf("corrupt bytes must be left for inspection, got %q", got)
	}
}

func TestLedger_InvalidClaimAndHolderRefused(t *testing.T) {
	l := openLedger(t)
	now := time.Now()
	if _, err := l.Acquire(Claim{}, "alice", ttl, now); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("invalid claim: %v", err)
	}
	if _, err := l.Acquire(RepoClaim("acme/widgets"), "", ttl, now); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("empty holder: %v", err)
	}
	if err := l.ValidateEpoch("mutation:acme/none", 1, now); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("unknown key can never authorize: %v", err)
	}
}

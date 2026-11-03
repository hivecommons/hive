package forgestate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// budgetedAPI is a reconcile source that behaves like GitHub Search under
// its 30 calls/minute limit: after `limit` calls within a minute every call
// is refused with a rate-limit error until the clock moves on.
type budgetedAPI struct {
	mu       sync.Mutex
	clock    *fakeClock
	limit    int
	window   time.Time
	inWindow int
	calls    int
	refused  int
}

func (b *budgetedAPI) reconcile(_ context.Context, repo string) (int, int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock.Now()
	if now.Sub(b.window) >= time.Minute {
		b.window, b.inWindow = now, 0
	}
	b.calls++
	b.inWindow++
	if b.inWindow > b.limit {
		b.refused++
		return 0, 0, errors.New("403 API rate limit exceeded for installation")
	}
	return 10, 20, nil
}

func nineteenRepos() []string {
	repos := make([]string, 0, 19)
	for i := 0; i < 19; i++ {
		repos = append(repos, fmt.Sprintf("projectbluefin/repo-%02d", i))
	}
	return repos
}

// The failure projectbluefin's 19-repo hive hit for 19 days: a single pass
// asked about every repo, blew the per-minute search limit, and the
// aggregate was never Ready because the refused repos had no baseline.
func TestCountsReconcileUnbudgetedBlowsSearchLimit(t *testing.T) {
	clock := newClock()
	// Two-to-four search calls per repo collapse into one reconcile call
	// here, so a limit of 7 repos/minute stands in for 30 searches/minute.
	api := &budgetedAPI{clock: clock, limit: 7}
	c := NewCounts(CountsOptions{Repos: nineteenRepos(), Reconcile: api.reconcile, Now: clock.Now, Budget: 1000})
	if err := c.Reconcile(context.Background()); err == nil {
		t.Fatal("expected rate-limit failures")
	}
	got := c.Outcomes()
	if got.Ready {
		t.Fatalf("an unbudgeted pass must not be fully ready: %+v", got)
	}
	// The refusal stops the pass: the remaining repos are not asked.
	if api.refused != 1 || api.calls != 8 {
		t.Fatalf("calls = %d refused = %d, want the pass to stop at the first refusal (8 calls, 1 refused)", api.calls, api.refused)
	}
	if !got.Partial || got.ReposReady != 7 || got.ReposTotal != 19 || got.MergedPRs != 70 || got.ClosedIssues != 140 {
		t.Fatalf("outcomes = %+v, want a partial aggregate over the 7 repos that answered", got)
	}
	if !got.Stale || !strings.Contains(got.StaleReason, "rate limit") {
		t.Fatalf("outcomes = %+v, want stale with the rate-limit reason", got)
	}
	if c.ReconcilePending() != 11 {
		t.Fatalf("pending = %d, want the 11 repos the refusal skipped", c.ReconcilePending())
	}
}

func TestCountsReconcileBudgetedRoundsStayUnderSearchLimit(t *testing.T) {
	clock := newClock()
	api := &budgetedAPI{clock: clock, limit: 7}
	c := NewCounts(CountsOptions{Repos: nineteenRepos(), Reconcile: api.reconcile, Now: clock.Now})

	if !c.ReconcileDue() {
		t.Fatal("first pass is due immediately")
	}
	passes := 0
	for c.ReconcilePending() > 0 || passes == 0 {
		if !c.ReconcileDue() {
			clock.Advance(time.Minute)
			continue
		}
		passes++
		if err := c.Reconcile(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", passes, err)
		}
		if passes > 10 {
			t.Fatal("round did not complete")
		}
	}
	if api.refused != 0 {
		t.Fatalf("budgeted passes must never be refused; %d were", api.refused)
	}
	if passes != 4 {
		t.Fatalf("passes = %d, want ceil(19/%d) = 4", passes, CountsReconcileBudget)
	}
	got := c.Outcomes()
	if !got.Ready || got.Partial || got.Stale || got.ReposReady != 19 || got.MergedPRs != 190 || got.ClosedIssues != 380 {
		t.Fatalf("outcomes after the round = %+v, want every repo ready", got)
	}
	// The round is complete: the next one waits the full interval, not
	// the pace.
	if c.ReconcileDue() {
		t.Fatal("not due right after the round completes")
	}
	// The round began at t0 and its passes ran through t0+6m.
	clock.Advance(CountsReconcileInterval - 7*time.Minute)
	if c.ReconcileDue() {
		t.Fatal("not due before the interval elapses since the round began")
	}
	clock.Advance(time.Minute)
	if !c.ReconcileDue() {
		t.Fatal("due once the interval elapses since the round began")
	}
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.ReconcilePending() != 14 {
		t.Fatalf("pending after the next round's first pass = %d, want 14", c.ReconcilePending())
	}
}

func TestCountsReconcilePaceBetweenPasses(t *testing.T) {
	clock := newClock()
	api := &budgetedAPI{clock: clock, limit: 100}
	c := NewCounts(CountsOptions{Repos: []string{"o/a", "o/b", "o/c"}, Reconcile: api.reconcile, Now: clock.Now, Budget: 2})
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.ReconcilePending() != 1 {
		t.Fatalf("pending = %d, want 1", c.ReconcilePending())
	}
	if !c.Outcomes().Partial {
		t.Fatal("two of three repos ready is partial")
	}
	if c.ReconcileDue() {
		t.Fatal("the next pass waits for the pace")
	}
	clock.Advance(CountsReconcilePace)
	if !c.ReconcileDue() {
		t.Fatal("due once the pace elapses with repos pending")
	}
}

// Repos without a baseline go first so a hive becomes useful soonest.
func TestCountsReconcileAsksUnreadyReposFirst(t *testing.T) {
	clock := newClock()
	api := &budgetedAPI{clock: clock, limit: 100}
	c := NewCounts(CountsOptions{Repos: []string{"o/a", "o/b", "o/c"}, Reconcile: api.reconcile, Now: clock.Now, Budget: 2})
	c.Seed("o/a", 1, 1, t0)
	c.Seed("o/b", 1, 1, t0)
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c.Outcomes("o/c"); !got.Ready || got.MergedPRs != 10 {
		t.Fatalf("o/c (the only unseeded repo) should be asked in the first pass: %+v", got)
	}
}

func TestCountsNonRateLimitErrorDoesNotStopPass(t *testing.T) {
	clock := newClock()
	calls := 0
	reconcile := func(_ context.Context, repo string) (int, int, error) {
		calls++
		if repo == "o/a" {
			return 0, 0, errors.New("422 Validation Failed")
		}
		return 3, 4, nil
	}
	c := NewCounts(CountsOptions{Repos: []string{"o/a", "o/b"}, Reconcile: reconcile, Now: clock.Now})
	if err := c.Reconcile(context.Background()); err == nil {
		t.Fatal("expected o/a's error")
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want both repos asked", calls)
	}
	if got := c.Outcomes(); !got.Partial || got.MergedPRs != 3 || got.ClosedIssues != 4 || !got.Stale {
		t.Fatalf("outcomes = %+v", got)
	}
}

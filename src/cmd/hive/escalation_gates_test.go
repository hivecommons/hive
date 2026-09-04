package main

import (
	"path/filepath"
	"testing"
	"time"

	"log/slog"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"

	"io"
)

// swapEscalationStore points the package's lazily-loaded singleton at a fresh,
// temp-backed store for one test, restoring the previous pointer afterwards.
// Marking the sync.Once done first keeps getEscalationStore from clobbering
// the injected store with the real /data-backed ledger.
func swapEscalationStore(t *testing.T, s *escalation.Store) {
	t.Helper()
	escalationStoreOnce.Do(func() {})
	old := escalationStore
	escalationStore = s
	t.Cleanup(func() {
		// Once is now marked done for the whole test binary, so never restore
		// a nil pointer: a later test calling getEscalationStore would crash.
		// Leaving the last injected temp-backed store is safe AND hermetic
		// (it also keeps unrelated tests off the real /data ledger).
		if old != nil {
			escalationStore = old
		}
	})
}

func newTestEscalationStore(t *testing.T) (*escalation.Store, *time.Time) {
	t.Helper()
	s := escalation.Load(filepath.Join(t.TempDir(), "fix-streaks.json"))
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	clock := &now
	s.SetClock(func() time.Time { return *clock })
	swapEscalationStore(t, s)
	return s, clock
}

func escalationTestConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.AIAuthor = "hive-agent"
	return cfg
}

func redPR(repo string, number int, author, headSHA string) github.PullRequest {
	return github.PullRequest{
		Repo:          repo,
		Number:        number,
		Author:        author,
		HeadSHA:       headSHA,
		CIStatus:      "failure",
		FailingChecks: []string{"test"},
	}
}

func actionableWith(prs ...github.PullRequest) *github.ActionableResult {
	a := &github.ActionableResult{}
	a.PRs.Items = prs
	return a
}

func TestRecordRedStaleness(t *testing.T) {
	t.Run("disabled escalation records nothing", func(t *testing.T) {
		store, clock := newTestEscalationStore(t)
		cfg := escalationTestConfig()
		cfg.Escalation.Disabled = true

		recordRedStaleness(cfg, actionableWith(redPR("widgets", 7, "hive-agent", "abc")))
		*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
		if store.StaleRed("acme/widgets", 7, "abc") {
			t.Error("disabled escalation must not feed the staleness clock")
		}
	})

	t.Run("red agent PR becomes stale after the threshold, human PR never observed", func(t *testing.T) {
		store, clock := newTestEscalationStore(t)
		cfg := escalationTestConfig()

		recordRedStaleness(cfg, actionableWith(
			redPR("widgets", 7, "hive-agent", "abc"),
			redPR("widgets", 8, "some-human", "def"),
		))
		if store.StaleRed("acme/widgets", 7, "abc") {
			t.Error("fresh red must not read as stale")
		}
		*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
		if !store.StaleRed("acme/widgets", 7, "abc") {
			t.Error("agent PR red past the threshold must read stale (and bare repo must be org-prefixed)")
		}
		if store.StaleRed("acme/widgets", 8, "def") {
			t.Error("human-authored PR must never enter the staleness clock")
		}
	})

	t.Run("going green clears the clock", func(t *testing.T) {
		store, clock := newTestEscalationStore(t)
		cfg := escalationTestConfig()

		recordRedStaleness(cfg, actionableWith(redPR("widgets", 7, "hive-agent", "abc")))
		*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
		green := redPR("widgets", 7, "hive-agent", "abc")
		green.FailingChecks = nil
		recordRedStaleness(cfg, actionableWith(green))
		if store.StaleRed("acme/widgets", 7, "abc") {
			t.Error("a PR observed green must drop its staleness record")
		}
	})
}

func TestMergeReEngageHook(t *testing.T) {
	t.Run("disabled escalation yields a nil hook", func(t *testing.T) {
		cfg := escalationTestConfig()
		cfg.Escalation.Disabled = true
		if mergeReEngageHook(cfg) != nil {
			t.Error("hook must be nil when escalation is disabled")
		}
	})

	t.Run("re-engagement is capped per red SHA", func(t *testing.T) {
		store, _ := newTestEscalationStore(t)
		cfg := escalationTestConfig()
		recordRedStaleness(cfg, actionableWith(redPR("widgets", 7, "hive-agent", "abc")))

		hook := mergeReEngageHook(cfg)
		if hook == nil {
			t.Fatal("expected a hook when escalation is enabled")
		}
		for i := 0; i < escalation.MaxReEngagements; i++ {
			if !hook("widgets", 7) {
				t.Fatalf("re-engagement %d/%d should be allowed", i+1, escalation.MaxReEngagements)
			}
		}
		if hook("widgets", 7) {
			t.Error("re-engagement past the cap must be refused")
		}
		if got := store.ReEngagements("acme/widgets", 7); got != escalation.MaxReEngagements {
			t.Errorf("ReEngagements = %d, want %d (bare repo must resolve to the org-prefixed key)", got, escalation.MaxReEngagements)
		}
	})
}

func TestClaimingPRRedStale(t *testing.T) {
	t.Run("disabled escalation yields a nil predicate", func(t *testing.T) {
		cfg := escalationTestConfig()
		cfg.Escalation.Disabled = true
		if claimingPRRedStale(cfg, actionableWith()) != nil {
			t.Error("predicate must be nil when escalation is disabled")
		}
	})

	t.Run("release only for red AND stale claiming PRs", func(t *testing.T) {
		_, clock := newTestEscalationStore(t)
		cfg := escalationTestConfig()
		green := redPR("widgets", 9, "hive-agent", "ggg")
		green.FailingChecks = nil
		actionable := actionableWith(
			redPR("widgets", 7, "hive-agent", "abc"),
			green,
		)
		recordRedStaleness(cfg, actionable)

		pred := claimingPRRedStale(cfg, actionable)
		if pred == nil {
			t.Fatal("expected a predicate when escalation is enabled")
		}
		if pred("widgets", 404) {
			t.Error("a PR missing from the enumeration must keep suppressing (false)")
		}
		if pred("widgets", 9) {
			t.Error("a green claiming PR must keep suppressing (false)")
		}
		if pred("widgets", 7) {
			t.Error("a freshly-red claiming PR must keep suppressing (false)")
		}
		*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
		if !pred("widgets", 7) {
			t.Error("a red claiming PR stale past the threshold must release (true)")
		}
		if !pred("acme/widgets", 7) {
			t.Error("the predicate must resolve owner-prefixed claim repos too")
		}
	})
}

func TestReapStuckRedPRs(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("skips escalated, fresh, and green PRs; re-engages stale red up to the cap", func(t *testing.T) {
		store, clock := newTestEscalationStore(t)
		cfg := escalationTestConfig()
		green := redPR("widgets", 9, "hive-agent", "ggg")
		green.FailingChecks = nil
		actionable := actionableWith(
			redPR("widgets", 7, "hive-agent", "abc"),  // will go stale → reaped
			redPR("widgets", 8, "hive-agent", "def"),  // escalated to a human → skipped
			redPR("widgets", 10, "hive-agent", "xyz"), // stays fresh in the second pass
			green,
		)
		recordRedStaleness(cfg, actionable)
		escalated := map[string]bool{escalation.Key("acme/widgets", 8): true}

		// First pass: nothing is stale yet — nobody gets re-engaged.
		reapStuckRedPRs(cfg, actionable, escalated, discard)
		for _, n := range []int{7, 8, 9, 10} {
			if got := store.ReEngagements("acme/widgets", n); got != 0 {
				t.Errorf("fresh pass: PR %d re-engagements = %d, want 0", n, got)
			}
		}

		// Age PRs 7 and 8 past the threshold; PR 10's head moves (fresh clock).
		*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)
		moved := redPR("widgets", 10, "hive-agent", "xyz2")
		actionable = actionableWith(actionable.PRs.Items[0], actionable.PRs.Items[1], moved, green)
		recordRedStaleness(cfg, actionable)

		for i := 0; i < escalation.MaxReEngagements+2; i++ {
			reapStuckRedPRs(cfg, actionable, escalated, discard)
		}
		if got := store.ReEngagements("acme/widgets", 7); got != escalation.MaxReEngagements {
			t.Errorf("stale red PR 7 re-engagements = %d, want capped at %d", got, escalation.MaxReEngagements)
		}
		if got := store.ReEngagements("acme/widgets", 8); got != 0 {
			t.Errorf("escalated PR 8 re-engagements = %d, want 0 (human owns it)", got)
		}
		if got := store.ReEngagements("acme/widgets", 10); got != 0 {
			t.Errorf("churning PR 10 re-engagements = %d, want 0 (fresh red SHA)", got)
		}
		if got := store.ReEngagements("acme/widgets", 9); got != 0 {
			t.Errorf("green PR 9 re-engagements = %d, want 0", got)
		}
	})

	t.Run("disabled escalation is a no-op", func(t *testing.T) {
		store, clock := newTestEscalationStore(t)
		cfg := escalationTestConfig()
		actionable := actionableWith(redPR("widgets", 7, "hive-agent", "abc"))
		recordRedStaleness(cfg, actionable)
		*clock = clock.Add(escalation.RedPRStaleAfter + time.Minute)

		cfg.Escalation.Disabled = true
		reapStuckRedPRs(cfg, actionable, map[string]bool{}, discard)
		if got := store.ReEngagements("acme/widgets", 7); got != 0 {
			t.Errorf("disabled reaper re-engaged anyway: %d", got)
		}
	})
}

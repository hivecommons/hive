package tokens

import (
	"errors"
	"testing"
	"time"
)

func TestManagedAgentsLedgerDeduplicatesUsageAndPersistsBudget(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ledger, err := NewManagedAgentsLedger(dir, DefaultManagedAgentsDailyBudgetCents)
	if err != nil {
		t.Fatal(err)
	}
	ledger.now = func() time.Time { return clock }
	processed := clock.Add(time.Second)
	added, err := ledger.RecordModelRequest("ses_1", "evt_1", "scanner", "claude-sonnet-4-6", processed, 100, 50, 25, 10)
	if err != nil || !added {
		t.Fatalf("first model event: added=%v err=%v", added, err)
	}
	added, err = ledger.RecordModelRequest("ses_1", "evt_1", "scanner", "claude-sonnet-4-6", processed, 100, 50, 25, 10)
	if err != nil || added {
		t.Fatalf("duplicate model event: added=%v err=%v", added, err)
	}
	added, err = ledger.RecordModelRequest("ses_2", "evt_1", "scanner", "claude-sonnet-4-6", processed, 100, 50, 25, 10)
	if err != nil || added {
		t.Fatalf("cross-session duplicate model event: added=%v err=%v", added, err)
	}
	if hit, err := ledger.RecordSessionUsage("ses_1", "USD", 120, 4, 2); err != nil || hit {
		t.Fatalf("first session usage: hit=%v err=%v", hit, err)
	}
	if hit, err := ledger.RecordSessionUsage("ses_1", "USD", 120, 5, 2); err != nil || hit {
		t.Fatalf("repeated session usage: hit=%v err=%v", hit, err)
	}
	if got := ledger.DailySpendCents(); got != 120 {
		t.Fatalf("daily spend after repeated observation = %d, want 120", got)
	}
	if hit, err := ledger.RecordSessionUsage("ses_2", "USD", 2380, 1, 0); err != nil || !hit {
		t.Fatalf("budget edge: hit=%v err=%v", hit, err)
	}
	if got := ledger.RemainingDailyCents(); got != 0 {
		t.Fatalf("remaining budget = %d, want 0", got)
	}

	reloaded, err := NewManagedAgentsLedger(dir, DefaultManagedAgentsDailyBudgetCents)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = func() time.Time { return clock }
	if got := reloaded.DailySpendCents(); got != DefaultManagedAgentsDailyBudgetCents {
		t.Fatalf("reloaded daily spend = %d, want %d", got, DefaultManagedAgentsDailyBudgetCents)
	}
	added, err = reloaded.RecordModelRequest("ses_1", "evt_1", "scanner", "claude-sonnet-4-6", processed, 100, 50, 25, 10)
	if err != nil || added {
		t.Fatalf("reloaded duplicate model event: added=%v err=%v", added, err)
	}

	agg, err := CollectFromDir(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := agg.ByAgent["scanner"]; got != 185 {
		t.Fatalf("collected scanner tokens = %d, want 185", got)
	}
	if got := agg.Sessions[0].Backend; got != BackendManagedAgents {
		t.Fatalf("collected backend = %q, want %q", got, BackendManagedAgents)
	}
}

func TestManagedAgentsLedgerReservationsKeepConcurrentStartsBelowCap(t *testing.T) {
	dir := t.TempDir()
	clock := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	ledger, err := NewManagedAgentsLedger(dir, DefaultManagedAgentsDailyBudgetCents)
	if err != nil {
		t.Fatal(err)
	}
	ledger.now = func() time.Time { return clock }
	limit, err := ledger.ReserveSession("execution-a")
	if err != nil || limit != DefaultManagedAgentsDailyBudgetCents {
		t.Fatalf("first reservation = %d, err=%v", limit, err)
	}
	if _, err := ledger.ReserveSession("execution-b"); !errors.Is(err, ErrManagedAgentsBudgetExceeded) {
		t.Fatalf("concurrent reservation error = %v, want budget exceeded", err)
	}
	if got := ledger.RemainingDailyCents(); got != 0 {
		t.Fatalf("remaining reserved budget = %d, want 0", got)
	}
	if _, err := ledger.RecordSessionUsage("session-a", "USD", 125, 1, 0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ReleaseSession("execution-a"); err != nil {
		t.Fatal(err)
	}
	if got := ledger.RemainingDailyCents(); got != DefaultManagedAgentsDailyBudgetCents-125 {
		t.Fatalf("remaining after releasing reservation = %d, want %d", got, DefaultManagedAgentsDailyBudgetCents-125)
	}
	if limit, err := ledger.ReserveSession("execution-b"); err != nil || limit != DefaultManagedAgentsDailyBudgetCents-125 {
		t.Fatalf("second reservation = %d, err=%v", limit, err)
	}
	reloaded, err := NewManagedAgentsLedger(dir, DefaultManagedAgentsDailyBudgetCents)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.now = func() time.Time { return clock }
	if got := reloaded.RemainingDailyCents(); got != 0 {
		t.Fatalf("reloaded reservation budget = %d, want 0", got)
	}
}

func TestManagedAgentsLedgerRejectsBudgetAboveGuardrail(t *testing.T) {
	if _, err := NewManagedAgentsLedger(t.TempDir(), DefaultManagedAgentsDailyBudgetCents+1); err == nil {
		t.Fatal("budget above the USD 25 guardrail was accepted")
	}
}

func TestManagedAgentsLedgerSessionSpanningMidnightAddsOnlyDelta(t *testing.T) {
	clock := time.Date(2026, 9, 24, 23, 59, 0, 0, time.UTC)
	ledger, err := NewManagedAgentsLedger(t.TempDir(), DefaultManagedAgentsDailyBudgetCents)
	if err != nil {
		t.Fatal(err)
	}
	ledger.now = func() time.Time { return clock }
	if _, err := ledger.RecordSessionUsage("ses_1", "USD", 100, 1, 0); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	if _, err := ledger.RecordSessionUsage("ses_1", "USD", 130, 2, 0); err != nil {
		t.Fatal(err)
	}
	if got := ledger.DailySpendCents(); got != 30 {
		t.Fatalf("next-day spend = %d, want 30", got)
	}
	if _, err := ledger.RecordSessionUsage("ses_1", "USD", 130, 2, 0); err != nil {
		t.Fatal(err)
	}
	if got := ledger.DailySpendCents(); got != 30 {
		t.Fatalf("next-day spend after repeat = %d, want 30", got)
	}
}

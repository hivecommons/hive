package tokens

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newFixedManagedAgentsLedger(t *testing.T, dir string, budget int64, clock time.Time) *ManagedAgentsLedger {
	t.Helper()
	ledger, err := NewManagedAgentsLedger(dir, budget)
	if err != nil {
		t.Fatal(err)
	}
	ledger.now = func() time.Time { return clock }
	return ledger
}

// regularFile returns a path to an existing regular file so that any path
// nested beneath it cannot be created, even by root.
func regularFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var managedAgentsTestClock = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

func TestManagedAgentsLedgerConstructorValidation(t *testing.T) {
	if _, err := NewManagedAgentsLedger("  ", 100); err == nil {
		t.Fatal("empty directory accepted")
	}
	if _, err := NewManagedAgentsLedger(t.TempDir(), -1); err == nil {
		t.Fatal("negative budget accepted")
	}
	ledger, err := NewManagedAgentsLedger(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger.BudgetCents(); got != DefaultManagedAgentsDailyBudgetCents {
		t.Fatalf("default budget = %d", got)
	}
	if _, err := NewManagedAgentsLedger(filepath.Join(regularFile(t), "sub"), 100); err == nil {
		t.Fatal("uncreatable directory accepted")
	}
}

func TestManagedAgentsLedgerLoadErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "managed-agents-ledger.json"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManagedAgentsLedger(dir, 100); err == nil || !strings.Contains(err.Error(), "read managed agents ledger") {
		t.Fatalf("directory ledger path err = %v", err)
	}

	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "managed-agents-ledger.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewManagedAgentsLedger(dir, 100); err == nil || !strings.Contains(err.Error(), "decode managed agents ledger") {
		t.Fatalf("corrupt ledger err = %v", err)
	}
}

func TestManagedAgentsLedgerLoadFillsNilMaps(t *testing.T) {
	dir := t.TempDir()
	raw := `{"days":{"2026-09-24":{"spend_cents":40}}}`
	if err := os.WriteFile(filepath.Join(dir, "managed-agents-ledger.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := newFixedManagedAgentsLedger(t, dir, 100, managedAgentsTestClock)
	if ledger.data.Events == nil || ledger.data.Reservations == nil {
		t.Fatal("nil maps were not initialised")
	}
	if ledger.data.Days["2026-09-24"].Sessions == nil {
		t.Fatal("nil day sessions were not initialised")
	}
	if got := ledger.DailySpendCents(); got != 40 {
		t.Fatalf("loaded spend = %d", got)
	}
	if got := ledger.RemainingDailyCents(); got != 60 {
		t.Fatalf("loaded remaining = %d", got)
	}

	empty := t.TempDir()
	if err := os.WriteFile(filepath.Join(empty, "managed-agents-ledger.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger = newFixedManagedAgentsLedger(t, empty, 100, managedAgentsTestClock)
	if ledger.data.Days == nil || ledger.data.Events == nil || ledger.data.Reservations == nil {
		t.Fatal("empty ledger maps were not initialised")
	}
}

func TestManagedAgentsLedgerNilReceiver(t *testing.T) {
	var ledger *ManagedAgentsLedger
	if ledger.BudgetCents() != 0 || ledger.DailySpendCents() != 0 || ledger.RemainingDailyCents() != 0 {
		t.Fatal("nil ledger reported non-zero values")
	}
	if _, err := ledger.ReserveSession("a"); err == nil {
		t.Fatal("nil ReserveSession succeeded")
	}
	if err := ledger.MoveSessionReservation("a", "b"); err == nil {
		t.Fatal("nil MoveSessionReservation succeeded")
	}
	if err := ledger.ReleaseSession("a"); err == nil {
		t.Fatal("nil ReleaseSession succeeded")
	}
	if _, err := ledger.RecordModelRequest("s", "e", "a", "m", time.Time{}, 1, 1, 1, 1); err == nil {
		t.Fatal("nil RecordModelRequest succeeded")
	}
	if _, err := ledger.RecordSessionUsage("s", "USD", 1, 1, 1); err == nil {
		t.Fatal("nil RecordSessionUsage succeeded")
	}
}

func TestManagedAgentsLedgerRemainingClampsOverspend(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	hit, err := ledger.RecordSessionUsage("ses_over", "usd", 150, 0, 0)
	if err != nil || !hit {
		t.Fatalf("overspend: hit=%v err=%v", hit, err)
	}
	if got := ledger.RemainingDailyCents(); got != 0 {
		t.Fatalf("remaining after overspend = %d, want 0", got)
	}
	if _, err := ledger.ReserveSession("exec"); !errors.Is(err, ErrManagedAgentsBudgetExceeded) {
		t.Fatalf("reserve after overspend err = %v", err)
	}
	if len(ledger.data.Reservations) != 0 {
		t.Fatalf("failed reservation left state: %+v", ledger.data.Reservations)
	}
}

func TestManagedAgentsLedgerReserveSessionBranches(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	if _, err := ledger.ReserveSession("  "); err == nil {
		t.Fatal("empty reservation key accepted")
	}
	ledger.data.Reservations["stale"] = managedAgentsReservation{Day: "2026-09-23", LimitCents: 100}
	limit, err := ledger.ReserveSession("exec")
	if err != nil || limit != 100 {
		t.Fatalf("reserve with stale reservation: limit=%d err=%v", limit, err)
	}
	if _, ok := ledger.data.Reservations["stale"]; ok {
		t.Fatal("stale reservation was not pruned")
	}
	again, err := ledger.ReserveSession("exec")
	if err != nil || again != 100 {
		t.Fatalf("idempotent reserve: limit=%d err=%v", again, err)
	}
}

func TestManagedAgentsLedgerReserveSessionInitialisesNilMap(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	ledger.data.Reservations = nil
	limit, err := ledger.ReserveSession("exec")
	if err != nil || limit != 100 {
		t.Fatalf("reserve into nil map: limit=%d err=%v", limit, err)
	}
}

func TestManagedAgentsLedgerReserveSessionPersistFailureRollsBack(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	ledger.path = filepath.Join(regularFile(t), "ledger.json")
	if _, err := ledger.ReserveSession("exec"); err == nil {
		t.Fatal("reserve with unwritable ledger succeeded")
	}
	if len(ledger.data.Reservations) != 0 {
		t.Fatalf("reservation not rolled back: %+v", ledger.data.Reservations)
	}
}

func TestManagedAgentsLedgerMoveSessionReservation(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	if err := ledger.MoveSessionReservation("", "b"); err == nil {
		t.Fatal("empty from accepted")
	}
	if err := ledger.MoveSessionReservation("a", " "); err == nil {
		t.Fatal("empty to accepted")
	}
	if err := ledger.MoveSessionReservation("missing", "b"); err != nil {
		t.Fatalf("missing reservation move err = %v", err)
	}
	if _, err := ledger.ReserveSession("exec"); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MoveSessionReservation("exec", "exec"); err != nil {
		t.Fatalf("self move err = %v", err)
	}
	ledger.data.Reservations["ses_taken"] = managedAgentsReservation{Day: "2026-09-24", LimitCents: 1}
	if err := ledger.MoveSessionReservation("exec", "ses_taken"); err == nil {
		t.Fatal("move onto existing reservation accepted")
	}
	delete(ledger.data.Reservations, "ses_taken")
	if err := ledger.MoveSessionReservation("exec", "ses_1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ledger.data.Reservations["ses_1"]; !ok {
		t.Fatal("reservation was not moved")
	}
	if _, ok := ledger.data.Reservations["exec"]; ok {
		t.Fatal("old reservation key survived")
	}

	ledger.path = filepath.Join(regularFile(t), "ledger.json")
	if err := ledger.MoveSessionReservation("ses_1", "ses_2"); err == nil {
		t.Fatal("move with unwritable ledger succeeded")
	}
	if _, ok := ledger.data.Reservations["ses_1"]; !ok {
		t.Fatal("failed move was not rolled back")
	}
}

func TestManagedAgentsLedgerReleaseSession(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	if err := ledger.ReleaseSession(" "); err == nil {
		t.Fatal("empty release key accepted")
	}
	if err := ledger.ReleaseSession("absent"); err != nil {
		t.Fatalf("release absent err = %v", err)
	}
	if _, err := ledger.ReserveSession("exec"); err != nil {
		t.Fatal(err)
	}
	ledger.path = filepath.Join(regularFile(t), "ledger.json")
	if err := ledger.ReleaseSession("exec"); err == nil {
		t.Fatal("release with unwritable ledger succeeded")
	}
	if _, ok := ledger.data.Reservations["exec"]; !ok {
		t.Fatal("failed release was not rolled back")
	}
}

func TestManagedAgentsLedgerRecordModelRequestValidationAndDefaults(t *testing.T) {
	dir := t.TempDir()
	ledger := newFixedManagedAgentsLedger(t, dir, 100, managedAgentsTestClock)
	if _, err := ledger.RecordModelRequest(" ", "evt", "a", "m", managedAgentsTestClock, 1, 1, 1, 1); err == nil {
		t.Fatal("empty session accepted")
	}
	if _, err := ledger.RecordModelRequest("ses", " ", "a", "m", managedAgentsTestClock, 1, 1, 1, 1); err == nil {
		t.Fatal("empty event accepted")
	}
	added, err := ledger.RecordModelRequest("ses", "evt_b", " agent ", " model ", time.Time{}, -5, 10, -1, 2)
	if err != nil || !added {
		t.Fatalf("zero-time event: added=%v err=%v", added, err)
	}
	event := ledger.data.Events["evt_b"]
	if !event.Timestamp.Equal(managedAgentsTestClock) || event.Input != 0 || event.CacheRead != 0 || event.Agent != "agent" || event.Model != "model" {
		t.Fatalf("recorded event = %+v", event)
	}
	// A second event at the same instant exercises the event-ID tie break.
	if added, err := ledger.RecordModelRequest("ses", "evt_a", "agent", "model", managedAgentsTestClock, 1, 1, 0, 0); err != nil || !added {
		t.Fatalf("tie event: added=%v err=%v", added, err)
	}
	if added, err := ledger.RecordModelRequest("ses", "evt_c", "agent", "model", managedAgentsTestClock.Add(-time.Minute), 1, 1, 0, 0); err != nil || !added {
		t.Fatalf("earlier event: added=%v err=%v", added, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, "managed-agents-*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("session logs = %v err=%v", matches, err)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("session log lines = %d, want 4:\n%s", len(lines), raw)
	}
}

func TestManagedAgentsLedgerRecordModelRequestWriteFailures(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	ledger.dir = filepath.Join(regularFile(t), "sub")
	if _, err := ledger.RecordModelRequest("ses", "evt_1", "a", "m", managedAgentsTestClock, 1, 1, 0, 0); err == nil || !strings.Contains(err.Error(), "write managed agents session") {
		t.Fatalf("session write failure err = %v", err)
	}
	if _, ok := ledger.data.Events["evt_1"]; ok {
		t.Fatal("failed session write left the event recorded")
	}

	ledger = newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	ledger.path = filepath.Join(regularFile(t), "ledger.json")
	if _, err := ledger.RecordModelRequest("ses", "evt_2", "a", "m", managedAgentsTestClock, 1, 1, 0, 0); err == nil || !strings.Contains(err.Error(), "write managed agents ledger") {
		t.Fatalf("ledger persist failure err = %v", err)
	}
	if _, ok := ledger.data.Events["evt_2"]; ok {
		t.Fatal("failed persist left the event recorded")
	}
}

func TestManagedAgentsLedgerRecordSessionUsageValidation(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	if _, err := ledger.RecordSessionUsage(" ", "USD", 1, 0, 0); err == nil {
		t.Fatal("empty session accepted")
	}
	if _, err := ledger.RecordSessionUsage("ses", "EUR", 1, 0, 0); err == nil {
		t.Fatal("non-USD currency accepted")
	}
	for _, values := range [][3]int64{{-1, 0, 0}, {0, -1, 0}, {0, 0, -1}} {
		if _, err := ledger.RecordSessionUsage("ses", "USD", values[0], values[1], values[2]); err == nil {
			t.Fatalf("negative usage %v accepted", values)
		}
	}
	hit, err := ledger.RecordSessionUsage("ses", "", 10, 3, 2)
	if err != nil || hit {
		t.Fatalf("default currency: hit=%v err=%v", hit, err)
	}
	// A lower cumulative reading must not reduce stored maxima or add spend.
	if _, err := ledger.RecordSessionUsage("ses", "USD", 5, 1, 1); err != nil {
		t.Fatal(err)
	}
	use := ledger.data.Days["2026-09-24"].Sessions["ses"]
	if use.ListCostCents != 10 || use.ActiveSeconds != 3 || use.WebSearches != 2 {
		t.Fatalf("session use = %+v", use)
	}
	if got := ledger.DailySpendCents(); got != 10 {
		t.Fatalf("spend = %d, want 10", got)
	}
}

func TestManagedAgentsLedgerRecordSessionUsagePersistFailureRollsBack(t *testing.T) {
	ledger := newFixedManagedAgentsLedger(t, t.TempDir(), 100, managedAgentsTestClock)
	good := ledger.path
	ledger.path = filepath.Join(regularFile(t), "ledger.json")
	if _, err := ledger.RecordSessionUsage("ses", "USD", 10, 0, 0); err == nil {
		t.Fatal("usage with unwritable ledger succeeded")
	}
	if _, ok := ledger.data.Days["2026-09-24"]; ok {
		t.Fatal("new day survived a failed persist")
	}

	ledger.path = good
	if _, err := ledger.RecordSessionUsage("ses", "USD", 10, 0, 0); err != nil {
		t.Fatal(err)
	}
	ledger.path = filepath.Join(regularFile(t), "ledger.json")
	if _, err := ledger.RecordSessionUsage("ses", "USD", 30, 0, 0); err == nil {
		t.Fatal("usage with unwritable ledger succeeded")
	}
	if got := ledger.DailySpendCents(); got != 10 {
		t.Fatalf("spend after rollback = %d, want 10", got)
	}
}

func TestManagedAgentsLedgerCrossDayBaselineUsesHighestPriorDay(t *testing.T) {
	clock := managedAgentsTestClock
	ledger, err := NewManagedAgentsLedger(t.TempDir(), DefaultManagedAgentsDailyBudgetCents)
	if err != nil {
		t.Fatal(err)
	}
	ledger.now = func() time.Time { return clock }
	if _, err := ledger.RecordSessionUsage("ses", "USD", 200, 0, 0); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(24 * time.Hour)
	// A stale, lower reading on the next day adds nothing.
	if _, err := ledger.RecordSessionUsage("ses", "USD", 150, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := ledger.DailySpendCents(); got != 0 {
		t.Fatalf("next-day spend after stale reading = %d, want 0", got)
	}
	if _, err := ledger.RecordSessionUsage("ses", "USD", 260, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := ledger.DailySpendCents(); got != 60 {
		t.Fatalf("next-day spend = %d, want 60", got)
	}
}

func TestManagedAgentsAtomicWriteFailures(t *testing.T) {
	if err := atomicWrite(filepath.Join(regularFile(t), "x", "y.json"), []byte("{}"), 0o600); err == nil {
		t.Fatal("atomicWrite under a regular file succeeded")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(target, []byte("{}"), 0o600); err == nil {
		t.Fatal("atomicWrite over a non-empty directory succeeded")
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".managed-agents-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v err=%v", leftovers, err)
	}
	if err := atomicWrite(filepath.Join(dir, "ok.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

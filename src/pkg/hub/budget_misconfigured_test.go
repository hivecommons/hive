package hub

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// exhaustedEntry builds an online L4 entry whose governor reports the budget
// gate closed, with the given configured limit.
func exhaustedEntry(limit int64) RegistryEntry {
	return RegistryEntry{
		Online:          true,
		ACMMLevel:       4,
		BudgetExhausted: boolptr(true),
		BudgetLimit:     int64ptr(limit),
		GitHubAppState:  GitHubAppTokenStatusOK,
	}
}

// TestVerdictNamesMisconfiguredBudget covers the second half of #5508: an
// exhausted spoke whose LIMIT could never fund one model call must be
// distinguishable, at a glance, from one that genuinely spent its budget. The
// remedies share nothing — resetting the window fixes the second and does
// nothing at all for the first.
func TestVerdictNamesMisconfiguredBudget(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		name    string
		limit   int64
		wantSub string
		// notSub must NOT appear — this is what stops the misconfigured case
		// from collapsing back into the generic chip.
		notSub string
	}{
		{"devx-gabriel 5 tokens", 5, "misconfigured", "budget exhausted — agents halted"},
		{"z-aiops2 50 tokens", 50, "misconfigured", "budget exhausted — agents halted"},
		{"hosted qa-test 1000 tokens", 1000, "misconfigured", "budget exhausted — agents halted"},
		// A real budget genuinely spent keeps the original wording.
		{"50M genuinely spent", 50_000_000, "budget exhausted — agents halted", "misconfigured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := hiveHealthFor(exhaustedEntry(tc.limit), okRollup(), okApp(), 9, now)

			if v.State != HealthStateRed {
				t.Errorf("state = %q, want %q", v.State, HealthStateRed)
			}
			if !strings.Contains(v.Reason, tc.wantSub) {
				t.Errorf("reason = %q, want it to contain %q", v.Reason, tc.wantSub)
			}
			if strings.Contains(v.Reason, tc.notSub) {
				t.Errorf("reason = %q must NOT contain %q — the two causes need different chips",
					v.Reason, tc.notSub)
			}
		})
	}

	// The misconfigured chip must quote the offending number so the operator
	// can see the unit mistake without opening the config.
	v := hiveHealthFor(exhaustedEntry(50), okRollup(), okApp(), 9, now)
	if !strings.Contains(v.Reason, "50") {
		t.Errorf("misconfigured reason %q does not name the limit", v.Reason)
	}
}

// TestBudgetHealthFlagsMisconfiguredLimit pins the same distinction on the
// budget badge that the fleet table renders.
func TestBudgetHealthFlagsMisconfiguredLimit(t *testing.T) {
	e := exhaustedEntry(50)
	e.BudgetCurrentSpend = int64ptr(50)

	b := budgetHealthFor(e)
	if !b.Exhausted || b.Bucket != budgetBucketExhausted {
		t.Fatalf("bucket = %q exhausted = %v, want exhausted", b.Bucket, b.Exhausted)
	}
	if !b.Misconfigured {
		t.Error("Misconfigured = false for a 50-token limit; the badge cannot distinguish " +
			"a unit mistake from a genuinely spent budget")
	}
	if !strings.Contains(b.Reason, "unit mistake") {
		t.Errorf("reason = %q, want it to name the likely unit mistake", b.Reason)
	}
	// The numbers must survive alongside the new reason.
	if b.LimitTokens != 50 {
		t.Errorf("LimitTokens = %d, want 50", b.LimitTokens)
	}

	// A sane exhausted budget must be untouched — no false positives.
	sane := exhaustedEntry(config.MinUsableBudgetTokens)
	sane.BudgetCurrentSpend = int64ptr(config.MinUsableBudgetTokens)
	if sb := budgetHealthFor(sane); sb.Misconfigured {
		t.Errorf("a limit at the floor was flagged misconfigured: %+v", sb)
	}
}

// TestFleetPageRendersMisconfiguredBudget pins the UI half: the fleet page must
// actually branch on the misconfigured flag, otherwise the server-side
// distinction never reaches the operator's eye.
func TestFleetPageRendersMisconfiguredBudget(t *testing.T) {
	t.Setenv("HUB_DATA_DIR", t.TempDir())
	srv := NewHubServer(0, slog.Default(), "test", "v4")

	req := httptest.NewRequest("GET", "/fleet", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /fleet: %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"b.misconfigured", "budget misconfigured"} {
		if !strings.Contains(body, want) {
			t.Errorf("fleet page does not render the misconfigured budget state (%q missing)", want)
		}
	}
}

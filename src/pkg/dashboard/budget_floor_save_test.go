package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestBelowFloorBudgetSaveRejected is the SAVE-path half of the #5508
// asymmetry. Its twin, TestBelowFloorBudgetStillLoads in pkg/config, asserts
// that these exact same values LOAD successfully with only a warning.
//
// The two behaviours are deliberately opposite on the same input:
//   - load  → warn, accept  (nobody is watching; refusing bricks a live spoke)
//   - save  → reject        (a human is at the dashboard to fix the number)
//
// A change that makes these agree in either direction is a regression, so both
// tests must exist and both must be able to fail.
func TestBelowFloorBudgetSaveRejected(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
		hint  string
	}{
		{"devx-gabriel 5 tokens", 5, "5M"},
		{"z-aiops2 50 tokens", 50, "50M"},
		{"hosted qa-test 1000 tokens", 1000, "1000M"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := govServer(t)
			before := s.deps.Config.Governor.Budget.TotalTokens

			rec := doPut(s, "/api/config/governor/budget", map[string]any{
				"totalTokens": tc.limit, "periodDays": 7, "criticalPct": 90,
			})

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT budget totalTokens=%d returned %d, want %d — the dashboard "+
					"must REFUSE a below-floor limit, not accept it silently",
					tc.limit, rec.Code, http.StatusBadRequest)
			}
			// The message has to name the mistake, or the operator learns nothing.
			body := rec.Body.String()
			if !strings.Contains(body, "did you mean") || !strings.Contains(body, tc.hint) {
				t.Errorf("rejection message does not name the likely unit mistake %q: %s", tc.hint, body)
			}
			// A rejected save must not have mutated live state.
			if got := s.deps.Config.Governor.Budget.TotalTokens; got != before {
				t.Errorf("rejected save still wrote totalTokens=%d (was %d)", got, before)
			}
		})
	}
}

// TestSaneBudgetSaveAccepted is the "no behavior change for sane configs" half:
// at or above the floor — and at zero, which disables budget tracking — the
// save path behaves exactly as it did before the floor existed.
func TestSaneBudgetSaveAccepted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int64
	}{
		{"exactly at the floor", config.MinUsableBudgetTokens},
		{"a realistic 50M budget", 50_000_000},
		{"zero disables budget tracking", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := govServer(t)
			rec := doPut(s, "/api/config/governor/budget", map[string]any{
				"totalTokens": tc.limit, "periodDays": 7, "criticalPct": 90,
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT budget totalTokens=%d returned %d, want 200: %s",
					tc.limit, rec.Code, rec.Body.String())
			}
			if got := s.deps.Config.Governor.Budget.TotalTokens; got != tc.limit {
				t.Errorf("accepted save stored totalTokens=%d, want %d", got, tc.limit)
			}
		})
	}
}

// TestBudgetPartialUpdateJudgedOnSuppliedValue pins the rule that the floor
// judges only what the request SUPPLIED, never a value merely stored.
//
// This is what keeps the three live below-floor spokes usable: if the floor
// were applied to the merged effective value, every budget PUT from those
// hives would 400 — the operator could not adjust period_days and could not
// even send an empty payload — locking them out of the screen they need in
// order to fix the limit.
func TestBudgetPartialUpdateJudgedOnSuppliedValue(t *testing.T) {
	s := govServer(t)
	// Simulate a live spoke that booted with a below-floor limit (load warned
	// and accepted it, so this is a reachable state).
	s.deps.Config.Governor.Budget.TotalTokens = 50
	s.deps.Config.Governor.Budget.PeriodDays = 7
	s.deps.Config.Governor.Budget.CriticalPct = 90

	// Editing an unrelated field must SUCCEED despite the stored below-floor
	// limit, and must leave that limit alone.
	rec := doPut(s, "/api/config/governor/budget", map[string]any{"periodDays": 14})
	if rec.Code != http.StatusOK {
		t.Fatalf("editing periodDays on a spoke with a stored below-floor limit returned %d, want 200: %s; "+
			"the floor must judge only SUPPLIED values or affected spokes are locked out",
			rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.PeriodDays; got != 14 {
		t.Errorf("PeriodDays = %d, want 14", got)
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 50 {
		t.Errorf("TotalTokens = %d, want the stored 50 left untouched", got)
	}

	// But re-supplying a below-floor limit explicitly is still refused.
	rec = doPut(s, "/api/config/governor/budget", map[string]any{"totalTokens": 50})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("explicitly re-supplying 50 returned %d, want 400", rec.Code)
	}

	// And the fix itself must go through in one PUT.
	rec = doPut(s, "/api/config/governor/budget", map[string]any{"totalTokens": 50_000_000})
	if rec.Code != http.StatusOK {
		t.Fatalf("correcting the limit to 50M returned %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Budget.TotalTokens; got != 50_000_000 {
		t.Errorf("corrected limit stored as %d, want 50000000", got)
	}
}

// TestBudgetRejectionIsJSONError keeps the refusal machine-readable for the UI.
func TestBudgetRejectionIsJSONError(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/budget", map[string]any{
		"totalTokens": 50, "periodDays": 7, "criticalPct": 90,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT budget totalTokens=50 returned %d, want 400", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("rejection body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if _, ok := payload["error"]; !ok {
		t.Errorf("rejection JSON has no error field: %v", payload)
	}
}

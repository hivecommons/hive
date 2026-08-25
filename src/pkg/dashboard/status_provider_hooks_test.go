package dashboard

import (
	"testing"
	"time"
)

// Covers the provider-registration seams main() wires at startup:
// SetAgentAuthProvider and SetInferenceBudgetProvider/InferenceBudgetExceeded
// had no coverage on either the nil-provider default or the wired path.

func TestSetAgentAuthProvider(t *testing.T) {
	prev := getAgentAuthFn()
	t.Cleanup(func() { SetAgentAuthProvider(prev) })

	SetAgentAuthProvider(nil)
	if getAgentAuthFn() != nil {
		t.Fatal("getAgentAuthFn() != nil after clearing the provider")
	}

	SetAgentAuthProvider(func(agentName string) (bool, bool) {
		return agentName == "quality", true
	})
	fn := getAgentAuthFn()
	if fn == nil {
		t.Fatal("getAgentAuthFn() = nil after SetAgentAuthProvider")
	}
	if avail, known := fn("quality"); !avail || !known {
		t.Errorf("fn(quality) = (%v, %v), want (true, true)", avail, known)
	}
	if avail, known := fn("scanner"); avail || !known {
		t.Errorf("fn(scanner) = (%v, %v), want (false, true)", avail, known)
	}
}

func TestInferenceBudgetExceeded(t *testing.T) {
	// Capture-and-restore: the provider is package-global state.
	inferenceBudgetMu.RLock()
	prev := inferenceBudgetFn
	inferenceBudgetMu.RUnlock()
	t.Cleanup(func() { SetInferenceBudgetProvider(prev) })

	// No provider registered (tests / spokes with no proxy): zero values.
	SetInferenceBudgetProvider(nil)
	if errMsg, since, lastRebuff, rebuffs := InferenceBudgetExceeded(); errMsg != "" ||
		!since.IsZero() || !lastRebuff.IsZero() || rebuffs != 0 {
		t.Errorf("nil provider: got (%q, %v, %v, %d), want zero values",
			errMsg, since, lastRebuff, rebuffs)
	}

	// A wired provider's signal passes through unchanged.
	wantSince := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	wantRebuff := wantSince.Add(30 * time.Minute)
	SetInferenceBudgetProvider(func() (string, time.Time, time.Time, int) {
		return "provider spending limit reached", wantSince, wantRebuff, 7
	})
	errMsg, since, lastRebuff, rebuffs := InferenceBudgetExceeded()
	if errMsg != "provider spending limit reached" || !since.Equal(wantSince) ||
		!lastRebuff.Equal(wantRebuff) || rebuffs != 7 {
		t.Errorf("wired provider: got (%q, %v, %v, %d), want passthrough",
			errMsg, since, lastRebuff, rebuffs)
	}
}

package main

import (
	"strings"
	"testing"
	"time"
)

// Tests for decideProviderBudgetAlert, the provider-spend banner decision
// lifted out of runEvalCycle (#7232, policy from #4294).
//
// The wording is the product here. This banner is the only way an operator
// learns their hive is firing its cadence into a gateway that is refusing
// 100% of calls -- the field failure #4294 was filed for -- so the tests
// assert the text, not just which branch ran.

func neverCalled(t *testing.T) func() string {
	t.Helper()
	return func() string {
		t.Fatal("quotaReason must not be consulted while the provider latch is held; the original code only read agent statuses on the not-latched branch")
		return ""
	}
}

func TestDecideProviderBudgetAlertLatchedSuspended(t *testing.T) {
	got := decideProviderBudgetAlert(true, true, "spend cap hit", time.Now(), 1, neverCalled(t))

	if got.Clear {
		t.Fatal("a held latch must not clear the banner")
	}
	if !strings.Contains(got.Message, "agent kicks suspended") {
		t.Fatalf("a suppressing latch must say kicks are suspended, got %q", got.Message)
	}
	if !strings.Contains(got.Message, "spend cap hit") {
		t.Fatalf("the banner must carry the provider's cause, got %q", got.Message)
	}
	if got.Cause != got.Message {
		t.Fatalf("the latched banner text is reused as the cause downstream; Cause=%q Message=%q", got.Cause, got.Message)
	}
}

func TestDecideProviderBudgetAlertLatchedProbing(t *testing.T) {
	got := decideProviderBudgetAlert(true, false, "spend cap hit", time.Now(), 1, neverCalled(t))

	if !strings.Contains(got.Message, "probing with a single agent kick") {
		t.Fatalf("a stale latch probes rather than staying muted, got %q", got.Message)
	}
	if strings.Contains(got.Message, "agent kicks suspended") {
		t.Fatalf("probing and suspended are different states; got both in %q", got.Message)
	}
}

// TestDecideProviderBudgetAlertRepeatedRebuffsAreQuantified is the difference
// between "one call was refused" and "this has been refusing since Tuesday".
func TestDecideProviderBudgetAlertRepeatedRebuffsAreQuantified(t *testing.T) {
	since := time.Date(2026, 9, 16, 14, 30, 0, 0, time.UTC)
	got := decideProviderBudgetAlert(true, true, "spend cap hit", since, 47, neverCalled(t))

	if !strings.Contains(got.Message, "47 refused calls") {
		t.Fatalf("repeated rebuffs must be counted, got %q", got.Message)
	}
	if !strings.Contains(got.Message, since.Format(time.RFC1123)) {
		t.Fatalf("repeated rebuffs must name when they started, got %q", got.Message)
	}
}

func TestDecideProviderBudgetAlertSingleRebuffIsNotQuantified(t *testing.T) {
	since := time.Date(2026, 9, 16, 14, 30, 0, 0, time.UTC)
	got := decideProviderBudgetAlert(true, true, "spend cap hit", since, 1, neverCalled(t))

	if strings.Contains(got.Message, "refused calls") {
		t.Fatalf("a single rebuff must not be reported as a run of them, got %q", got.Message)
	}
	if strings.Contains(got.Message, since.Format(time.RFC1123)) {
		t.Fatalf("a single rebuff needs no since-timestamp, got %q", got.Message)
	}
}

// TestDecideProviderBudgetAlertQuotaExhaustedFallback covers the not-latched
// branch: the provider is not refusing on money, but agents are individually
// out of quota, which is a different cause and must not be silent.
func TestDecideProviderBudgetAlertQuotaExhaustedFallback(t *testing.T) {
	got := decideProviderBudgetAlert(false, false, "", time.Now(), 0,
		func() string { return "3 agents out of quota" })

	if got.Clear {
		t.Fatal("an exhausted quota is still a condition to report, not a clear")
	}
	if !strings.Contains(got.Message, "provider quota exhausted") || !strings.Contains(got.Message, "3 agents out of quota") {
		t.Fatalf("quota exhaustion must name itself and its reason, got %q", got.Message)
	}
	if got.Cause != "" {
		t.Fatalf("only the latched branch replaces the cause, got %q", got.Cause)
	}
}

func TestDecideProviderBudgetAlertHealthyClears(t *testing.T) {
	got := decideProviderBudgetAlert(false, false, "", time.Now(), 0, func() string { return "" })

	if !got.Clear {
		t.Fatal("a healthy provider with no exhausted quota must clear the banner, or it sticks forever")
	}
	if got.Message != "" {
		t.Fatalf("clearing and raising are mutually exclusive, got %q", got.Message)
	}
}

// TestDecideProviderBudgetAlertNeverRaisesAndClears pins the invariant the
// call site depends on: it raises Message or sets Clear, never both.
func TestDecideProviderBudgetAlertNeverRaisesAndClears(t *testing.T) {
	cases := []struct {
		name     string
		latched  bool
		suppress bool
		quota    string
	}{
		{"latched suspended", true, true, ""},
		{"latched probing", true, false, ""},
		{"quota exhausted", false, false, "2 agents out of quota"},
		{"healthy", false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideProviderBudgetAlert(tc.latched, tc.suppress, "cause", time.Now(), 1,
				func() string { return tc.quota })
			if got.Message != "" && got.Clear {
				t.Fatalf("must never raise and clear in the same cycle: %+v", got)
			}
			if got.Message == "" && !got.Clear {
				t.Fatalf("every cycle must either raise or clear: %+v", got)
			}
		})
	}
}

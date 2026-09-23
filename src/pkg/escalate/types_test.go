package escalate

import (
	"strings"
	"testing"
	"time"
)

func TestSeverityValid(t *testing.T) {
	for _, s := range []Severity{SeverityInfo, SeverityDecision} {
		if !s.Valid() {
			t.Fatalf("%q should be valid", s)
		}
	}
	if Severity("panic").Valid() {
		t.Fatal("unknown severity reported valid")
	}
}

func TestEventString(t *testing.T) {
	ev := Event{
		Severity: SeverityDecision,
		RunKey:   "20260922132517-hcl-encoding-helpers",
		Stage:    "plan",
		Gen:      7,
		Attempts: 2,
		Reason:   "never reached final",
		At:       time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
	}
	got := ev.String()
	for _, want := range []string{"decision", "20260922132517-hcl-encoding-helpers", "plan", "gen 7", "2 attempts", "never reached final"} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() = %q, missing %q", got, want)
		}
	}
}

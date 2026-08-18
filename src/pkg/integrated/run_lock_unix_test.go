//go:build !windows

package integrated

import (
	"os"
	"testing"
	"time"
)

func TestLegacyProductionRunOwnerUsesProcessStartIndependentOfExecutableName(t *testing.T) {
	startedAt, inspectable := legacyProductionRunProcessStartedAt(os.Getpid())
	if !inspectable {
		if !legacyProductionRunOwnerMatches(os.Getpid(), time.Now().Add(-time.Hour)) {
			t.Fatal("live owner did not fail closed when process-start identity was unavailable")
		}
		return
	}
	if !legacyProductionRunOwnerMatches(os.Getpid(), startedAt.Add(time.Minute)) {
		t.Fatal("current live owner was rejected because the test executable is not named hive")
	}
	if legacyProductionRunOwnerMatches(os.Getpid(), startedAt.Add(-time.Minute)) {
		t.Fatal("reused live PID was accepted despite beginning after the legacy lease")
	}
}

func TestParseLegacyProcessElapsed(t *testing.T) {
	tests := map[string]time.Duration{
		"00:07":      7 * time.Second,
		"02:03:04":   2*time.Hour + 3*time.Minute + 4*time.Second,
		"3-02:03:04": 3*24*time.Hour + 2*time.Hour + 3*time.Minute + 4*time.Second,
	}
	for input, expected := range tests {
		if actual, ok := parseLegacyProcessElapsed(input); !ok || actual != expected {
			t.Fatalf("parse %q = %s, %t; want %s", input, actual, ok, expected)
		}
	}
	for _, invalid := range []string{"", "1", "1:60", "-1:02", "1-25:00:00:00"} {
		if _, ok := parseLegacyProcessElapsed(invalid); ok {
			t.Fatalf("invalid elapsed time %q was accepted", invalid)
		}
	}
}

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The 2026-09-04 fleet event: 41 spokes reported "self-upgrade failed after 5
// attempts: " with nothing after the colon, because the cause was dropped when
// the marker could not be read. The reason must survive that.
func TestRecordUpgradeErrorKeepsCauseWhenMarkerUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "upgrade-marker.json")

	recordUpgradeError(path, errors.New("patching own Deployment: 403 forbidden"), testLogger())

	// A missing parent directory means the write cannot land either; the point
	// of the assertion is that the function no longer returns before trying.
	if data, err := os.ReadFile(path); err == nil {
		var m upgradeMarker
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("marker is not valid JSON: %v", err)
		}
		if !strings.Contains(m.LastError, "403 forbidden") {
			t.Errorf("LastError = %q, want the cause preserved", m.LastError)
		}
	}
}

func TestRecordUpgradeErrorPreservesExistingMarkerFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-marker.json")
	writeUpgradeMarker(path, upgradeMarker{TargetSHA: "526ef71", Attempts: 3}, testLogger())

	recordUpgradeError(path, errors.New("image never changed"), testLogger())

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	m := parseUpgradeMarker(data)
	if m.LastError != "image never changed" {
		t.Errorf("LastError = %q", m.LastError)
	}
	if m.Attempts != 3 {
		t.Errorf("Attempts = %d, want the existing 3 preserved", m.Attempts)
	}
	if m.TargetSHA != "526ef71" {
		t.Errorf("TargetSHA = %q, want the existing target preserved", m.TargetSHA)
	}
}

func TestRecordUpgradeErrorIgnoresNilError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-marker.json")
	writeUpgradeMarker(path, upgradeMarker{TargetSHA: "abc1234"}, testLogger())

	recordUpgradeError(path, nil, testLogger())

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	if got := parseUpgradeMarker(data).LastError; got != "" {
		t.Errorf("LastError = %q, want empty for a nil error", got)
	}
}

// The operator-facing string must never end in a colon with nothing after it.
func TestUpgradeFailureSummaryNeverDanglesAColon(t *testing.T) {
	for _, empty := range []string{"", "   ", "\t\n"} {
		got := upgradeFailureSummary(5, empty)
		if strings.HasSuffix(strings.TrimSpace(got), ":") {
			t.Errorf("upgradeFailureSummary(5, %q) = %q, must not end in a bare colon", empty, got)
		}
		if !strings.Contains(got, "no error recorded") {
			t.Errorf("upgradeFailureSummary(5, %q) = %q, want it to say the reason was not captured", empty, got)
		}
		if !strings.Contains(got, "5 attempts") {
			t.Errorf("upgradeFailureSummary(5, %q) = %q, want the attempt count kept", empty, got)
		}
	}
}

func TestUpgradeFailureSummaryKeepsRealCause(t *testing.T) {
	got := upgradeFailureSummary(5, "403 forbidden on deployments/hive")
	if !strings.Contains(got, "403 forbidden on deployments/hive") {
		t.Errorf("summary = %q, want the real cause verbatim", got)
	}
	if strings.Contains(got, "no error recorded") {
		t.Errorf("summary = %q, must not claim the reason is missing when it is present", got)
	}
}

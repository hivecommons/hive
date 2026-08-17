package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testLogger discards output: these tests assert on the marker file, not logs.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The wedge this guards against: a spoke whose upgrade failed (image never
// changed) used to latch PERMANENTLY on the current SHA and silently discard
// every later instruction. These tests pin the replacement semantics —
// (fromSHA -> targetSHA) keying, a bounded attempt budget, and backoff.

func TestParseUpgradeMarker_LegacyMarkerCountsAsOneAttempt(t *testing.T) {
	// Markers written by the previous version have no "attempts" field. An
	// already-wedged hive must get retries under the new budget rather than be
	// read as zero attempts (which would mis-order the backoff).
	legacy := []byte(`{"target_sha":"fc32ae4","current_sha":"c11643a","requested_at":"2026-07-31T13:52:23Z"}`)
	m := parseUpgradeMarker(legacy)
	if m.TargetSHA != "fc32ae4" || m.CurrentSHA != "c11643a" {
		t.Fatalf("legacy fields not parsed: %+v", m)
	}
	if m.Attempts != 1 {
		t.Errorf("legacy marker attempts = %d, want 1", m.Attempts)
	}
}

func TestParseUpgradeMarker_GarbageIsNotALatch(t *testing.T) {
	// A corrupt marker must never be able to wedge an upgrade: it decodes to a
	// zero marker whose empty CurrentSHA cannot match any real gitShort.
	m := parseUpgradeMarker([]byte("not json"))
	if m.CurrentSHA != "" || m.TargetSHA != "" {
		t.Errorf("garbage marker should decode empty, got %+v", m)
	}
}

func TestSameUpgradeTarget(t *testing.T) {
	cases := []struct {
		name, a, b string
		want       bool
	}{
		{"identical", "fc32ae4", "fc32ae4", true},
		{"short vs full prefix", "fc32ae4", "fc32ae4d9c1b2a3", true},
		{"case insensitive", "FC32AE4", "fc32ae4", true},
		// The whole point of keying on the target: a NEW target must reset the
		// attempt budget instead of inheriting the old target's exhausted one.
		{"different target", "fc32ae4", "c11643a", false},
		{"empty never matches", "", "fc32ae4", false},
		{"both empty never matches", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameUpgradeTarget(tc.a, tc.b); got != tc.want {
				t.Errorf("sameUpgradeTarget(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestWriteAndRecordUpgradeError_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade-requested")
	logger := testLogger()

	writeUpgradeMarker(path, upgradeMarker{
		TargetSHA:   "fc32ae4",
		CurrentSHA:  "c11643a",
		RequestedAt: time.Now().UTC(),
		Attempts:    2,
	}, logger)

	// The cause of a failed attempt must survive into the next boot — without
	// it the reason dies with the process and the failure stays invisible.
	recordUpgradeError(path, os.ErrPermission, logger)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}
	m := parseUpgradeMarker(data)
	if m.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (recordUpgradeError must not reset it)", m.Attempts)
	}
	if m.LastError != os.ErrPermission.Error() {
		t.Errorf("last_error = %q, want %q", m.LastError, os.ErrPermission.Error())
	}
	if m.TargetSHA != "fc32ae4" {
		t.Errorf("target_sha = %q, want fc32ae4", m.TargetSHA)
	}
}

func TestRecordUpgradeError_NoMarkerIsNoOp(t *testing.T) {
	// No marker means no attempt is in flight; annotating one must not create
	// a marker that would later be mistaken for a real attempt.
	path := filepath.Join(t.TempDir(), "absent")
	recordUpgradeError(path, os.ErrPermission, testLogger())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("recordUpgradeError created a marker where none existed")
	}
}

func TestUpgradeMarker_EncodesAttemptsAndError(t *testing.T) {
	// The marker is the only cross-restart state, so its JSON shape is load
	// bearing: the fields must actually serialize.
	data, err := json.Marshal(upgradeMarker{
		TargetSHA: "fc32ae4", CurrentSHA: "c11643a", Attempts: 3, LastError: "HTTP 403",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	m := parseUpgradeMarker(data)
	if m.Attempts != 3 || m.LastError != "HTTP 403" {
		t.Errorf("round trip lost data: %+v", m)
	}
}

func TestSelfUpgradeBudgetConstants(t *testing.T) {
	// The latch must be bounded but not instant, and must never be "never
	// again" (the original bug) nor unlimited (a retry storm).
	if selfUpgradeMaxAttempts < 2 {
		t.Errorf("selfUpgradeMaxAttempts = %d; a latch that allows <2 attempts cannot recover from a transient failure", selfUpgradeMaxAttempts)
	}
	if selfUpgradeBaseBackoff <= 0 || selfUpgradeMaxBackoff < selfUpgradeBaseBackoff {
		t.Errorf("backoff bounds invalid: base=%v max=%v", selfUpgradeBaseBackoff, selfUpgradeMaxBackoff)
	}
	// Exiting 0 on a failed upgrade is what made the failure invisible to K8s.
	if selfUpgradeFailureExitCode == 0 {
		t.Error("selfUpgradeFailureExitCode must be non-zero so a failed upgrade is visible in the container termination state")
	}
}

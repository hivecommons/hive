package main

// Tests for the uncovered branches of writeUpgradeOutcome (main.go, 50%
// covered): the happy path runs only indirectly through
// reconcileUpgradeOutcomeAtBoot, so the on-disk JSON shape was never pinned,
// and the write-failure warn branch had no coverage at all. The outcome file
// is what lets the dashboard tell "upgrade landed" apart from "never
// attempted" (#7092), so both the durable format and the non-fatal failure
// contract deserve direct pins — mirroring what
// upgrade_marker_docs_token_test.go already does for writeUpgradeMarker.
//
// The json.Marshal error arm is deliberately NOT covered: upgradeOutcome is
// four plain string/time fields, so that branch is unreachable without
// changing the type.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A failed outcome write must warn and return — the upgrade itself already
// landed, only the durable record is degraded — never panic or abort boot.
func TestWriteUpgradeOutcomeWriteFailureWarnsAndDoesNotPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "last-upgrade-outcome")

	var logs syncLogBuffer
	writeUpgradeOutcome(path, upgradeOutcome{TargetSHA: "abc"}, markerTestLogger(&logs))

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("no outcome file should exist, stat err = %v", err)
	}
	if s := logs.String(); !strings.Contains(s, "failed to write upgrade outcome") {
		t.Errorf("expected write-failure warn, got: %q", s)
	}
}

// The happy path must produce a file that round-trips into upgradeOutcome
// with every field intact under the documented snake_case JSON keys — the
// dashboard reads this record long after the writing process is gone, so the
// on-disk shape is a compatibility contract, not an implementation detail.
func TestWriteUpgradeOutcomeRoundTripsAllFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "last-upgrade-outcome")
	want := upgradeOutcome{
		TargetSHA:   "fc32ae4d9c1b2a3",
		CurrentSHA:  "fc32ae4",
		RequestedAt: time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 15, 10, 5, 0, 0, time.UTC),
	}

	var logs syncLogBuffer
	writeUpgradeOutcome(path, want, markerTestLogger(&logs))

	if s := logs.String(); s != "" {
		t.Errorf("happy-path write must be silent, got: %q", s)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading outcome file: %v", err)
	}
	var got upgradeOutcome
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("outcome file is not valid JSON: %v", err)
	}
	if got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
	// The snake_case keys are read by consumers outside this process; a
	// silent rename would strand every existing on-disk record.
	for _, key := range []string{"target_sha", "current_sha", "requested_at", "completed_at"} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Errorf("outcome JSON missing documented key %q: %s", key, data)
		}
	}
}

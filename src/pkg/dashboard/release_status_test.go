package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildReleaseChannelStatus pins the honest channel resolution (#7092): a
// real channel resolves, a branch/pin tag renders as an explicit "unknown" with
// the observed tag, and a fallback to the hub's tracked channel works for spokes
// too old to report an image ref. The one non-negotiable: an unresolved channel
// NEVER renders a fabricated default.
func TestBuildReleaseChannelStatus(t *testing.T) {
	cases := []struct {
		name         string
		imageRef     string
		trackedChan  string
		wantChannel  string
		wantResolved bool
		wantTag      string
		detailSubstr string
		detailAbsent string
	}{
		{
			name:         "resolves from image tag",
			imageRef:     "ghcr.io/hivecommons/hive:stable",
			wantChannel:  "stable",
			wantResolved: true,
			wantTag:      "stable",
			detailSubstr: `"stable" release channel`,
		},
		{
			name:         "falls back to tracked channel when no image ref",
			imageRef:     "",
			trackedChan:  "candidate",
			wantChannel:  "candidate",
			wantResolved: true,
			wantTag:      "",
			detailSubstr: `"candidate" release channel`,
		},
		{
			name:         "unresolved: branch tag is not a channel",
			imageRef:     "ghcr.io/hivecommons/hive:v4-abc1234",
			wantChannel:  "",
			wantResolved: false,
			wantTag:      "v4-abc1234",
			detailSubstr: "v4-abc1234",
			// Must not invent a default channel name.
			detailAbsent: "stable",
		},
		{
			name:         "unresolved: image ref unavailable",
			imageRef:     "",
			trackedChan:  "",
			wantChannel:  "",
			wantResolved: false,
			wantTag:      "",
			detailSubstr: "could not be read",
			detailAbsent: "stable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildReleaseChannelStatus(tc.imageRef, tc.trackedChan)
			if got.Channel != tc.wantChannel {
				t.Errorf("Channel = %q, want %q", got.Channel, tc.wantChannel)
			}
			if got.Resolved != tc.wantResolved {
				t.Errorf("Resolved = %v, want %v", got.Resolved, tc.wantResolved)
			}
			if got.ImageTag != tc.wantTag {
				t.Errorf("ImageTag = %q, want %q", got.ImageTag, tc.wantTag)
			}
			if tc.detailSubstr != "" && !strings.Contains(got.Detail, tc.detailSubstr) {
				t.Errorf("Detail = %q, want it to contain %q", got.Detail, tc.detailSubstr)
			}
			// The honesty invariant: when unresolved, the detail must not name a
			// concrete channel as if it were the answer.
			if !got.Resolved && tc.detailAbsent != "" && strings.Contains(got.Detail, tc.detailAbsent) {
				t.Errorf("unresolved Detail = %q must NOT contain fabricated default %q", got.Detail, tc.detailAbsent)
			}
		})
	}
}

// TestBuildUpgradeAttemptStatus pins the three-state machine that is the heart
// of #7092: "never attempted", "succeeded" and "failed" must be distinct, and a
// failure reason must never be swallowed.
func TestBuildUpgradeAttemptStatus(t *testing.T) {
	reqAt := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	doneAt := time.Date(2026, 9, 15, 12, 3, 0, 0, time.UTC)

	cases := []struct {
		name         string
		outcome      *upgradeOutcome
		marker       map[string]any
		wantState    string
		detailSubstr []string
		detailAbsent []string
	}{
		{
			name:         "never attempted",
			outcome:      nil,
			marker:       nil,
			wantState:    upgradeAttemptNever,
			detailSubstr: []string{"No upgrade has been attempted"},
			// A never-attempted panel must not be able to read as success.
			detailAbsent: []string{"SUCCEEDED", "FAILED"},
		},
		{
			name: "succeeded",
			outcome: &upgradeOutcome{
				TargetSHA:   "abc1234",
				CurrentSHA:  "def5678",
				RequestedAt: reqAt,
				CompletedAt: doneAt,
			},
			marker:       nil,
			wantState:    upgradeAttemptSucceeded,
			detailSubstr: []string{"SUCCEEDED", "abc1234", doneAt.Format(time.RFC3339)},
		},
		{
			name: "failed with a reason",
			marker: map[string]any{
				"target": "abc1234", "attempts": 5, "maxAttempts": 5,
				"failed": true, "lastError": "403 patching deployment",
			},
			wantState:    upgradeAttemptFailed,
			detailSubstr: []string{"FAILED", "403 patching deployment"},
		},
		{
			name: "failed without a reason",
			marker: map[string]any{
				"target": "abc1234", "attempts": 5, "maxAttempts": 5,
				"failed": true,
			},
			wantState:    upgradeAttemptFailed,
			detailSubstr: []string{"FAILED", "No failure reason was recorded"},
		},
		{
			name: "in progress",
			marker: map[string]any{
				"target": "abc1234", "attempts": 2, "maxAttempts": 5,
				"failed": false,
			},
			wantState:    upgradeAttemptInProgress,
			detailSubstr: []string{"in progress"},
		},
		{
			name: "in-flight marker outranks a past success",
			outcome: &upgradeOutcome{
				TargetSHA: "old0000", RequestedAt: reqAt, CompletedAt: doneAt,
			},
			marker: map[string]any{
				"target": "new1111", "attempts": 5, "maxAttempts": 5, "failed": true,
			},
			wantState:    upgradeAttemptFailed,
			detailSubstr: []string{"FAILED", "new1111"},
			detailAbsent: []string{"SUCCEEDED"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildUpgradeAttemptStatus(tc.outcome, tc.marker)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			for _, sub := range tc.detailSubstr {
				if !strings.Contains(got.Detail, sub) {
					t.Errorf("Detail = %q, want it to contain %q", got.Detail, sub)
				}
			}
			for _, sub := range tc.detailAbsent {
				if strings.Contains(got.Detail, sub) {
					t.Errorf("Detail = %q must NOT contain %q", got.Detail, sub)
				}
			}
		})
	}
}

// TestNeverAttemptedDistinctFromSucceeded is the explicit regression guard for
// the reporter's core complaint: a blank/never-attempted panel must never be
// indistinguishable from a successful upgrade. State, detail and rendered
// timestamp must all differ.
func TestNeverAttemptedDistinctFromSucceeded(t *testing.T) {
	never := buildUpgradeAttemptStatus(nil, nil)
	succeeded := buildUpgradeAttemptStatus(&upgradeOutcome{
		TargetSHA:   "abc1234",
		RequestedAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 15, 12, 3, 0, 0, time.UTC),
	}, nil)

	if never.State == succeeded.State {
		t.Fatalf("never and succeeded share State %q — they must be distinct", never.State)
	}
	if never.Detail == succeeded.Detail {
		t.Fatalf("never and succeeded share Detail %q — they must be distinct", never.Detail)
	}
	if never.State != upgradeAttemptNever {
		t.Errorf("never.State = %q, want %q", never.State, upgradeAttemptNever)
	}
	if succeeded.State != upgradeAttemptSucceeded {
		t.Errorf("succeeded.State = %q, want %q", succeeded.State, upgradeAttemptSucceeded)
	}
	// A never-attempted attempt carries no timestamp; a success carries one.
	if never.CompletedAt != "" || never.At != "" {
		t.Errorf("never-attempted must carry no timestamp, got At=%q CompletedAt=%q", never.At, never.CompletedAt)
	}
	if succeeded.CompletedAt == "" {
		t.Errorf("succeeded must carry a landed timestamp")
	}
}

// TestReadUpgradeOutcome exercises the persistence round-trip the spoke uses to
// survive a success the in-flight marker cannot record.
func TestReadUpgradeOutcome(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "last-upgrade-outcome")
	orig := upgradeOutcomePath
	upgradeOutcomePath = path
	t.Cleanup(func() { upgradeOutcomePath = orig })

	if got := readUpgradeOutcome(); got != nil {
		t.Fatalf("no file present: want nil, got %+v", got)
	}

	want := upgradeOutcome{
		TargetSHA:   "abc1234",
		CurrentSHA:  "def5678",
		RequestedAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 9, 15, 12, 3, 0, 0, time.UTC),
	}
	data, _ := json.Marshal(want)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	got := readUpgradeOutcome()
	if got == nil || got.TargetSHA != want.TargetSHA || !got.CompletedAt.Equal(want.CompletedAt) {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}

	// A record with no target is not a usable outcome.
	if err := os.WriteFile(path, []byte(`{"current_sha":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readUpgradeOutcome(); got != nil {
		t.Fatalf("targetless record: want nil, got %+v", got)
	}
}

// TestBuildSpokeReleaseStatusStaleness pins the "don't present stale data as
// current" requirement: a fresh beat is reachable, an old beat is not.
func TestBuildSpokeReleaseStatusStaleness(t *testing.T) {
	fresh := buildSpokeReleaseStatus("ghcr.io/hivecommons/hive:stable", "", nil, nil,
		time.Now().Add(-1*time.Minute), true, 6*time.Minute)
	if !fresh.HubReachable {
		t.Errorf("a 1-minute-old beat must be reachable")
	}
	stale := buildSpokeReleaseStatus("ghcr.io/hivecommons/hive:stable", "", nil, nil,
		time.Now().Add(-30*time.Minute), true, 6*time.Minute)
	if stale.HubReachable {
		t.Errorf("a 30-minute-old beat must NOT be reachable")
	}
}

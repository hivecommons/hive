package spoke

import (
	"reflect"
	"testing"
	"time"
)

func TestUpgradeHistoryForDisplay(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	hold := UpgradeAttempt{
		Trigger: UpgradeTriggerWindow, Outcome: UpgradeOutcomeMissed, Reason: "merge debounce",
		StartedAt: now.Add(-10 * time.Minute), EndedAt: now.Add(-5 * time.Minute),
	}
	done := UpgradeAttempt{Outcome: UpgradeOutcomeDone, EndedAt: now}
	failed := UpgradeAttempt{Trigger: UpgradeTriggerScheduled, Outcome: UpgradeOutcomeMissed, EndedAt: now.Add(time.Minute), Reason: "ErrImagePull"}
	delayed := hold
	delayed.Outcome = UpgradeOutcomeDelayed
	newDelay := delayed
	newDelay.EndedAt = now.Add(time.Minute)
	unfinished := UpgradeAttempt{Trigger: UpgradeTriggerScheduled, StartedAt: now}
	noEnd := hold
	noEnd.EndedAt = time.Time{}

	cases := []struct {
		name       string
		history    []UpgradeAttempt
		upgradedAt time.Time
		want       []UpgradeAttempt
	}{
		{name: "empty"},
		{name: "legacy window is delayed", history: []UpgradeAttempt{hold}, want: []UpgradeAttempt{delayed}},
		{name: "new delay stays delayed", history: []UpgradeAttempt{delayed}, want: []UpgradeAttempt{delayed}},
		{name: "success resolves old window", history: []UpgradeAttempt{hold, done}, want: []UpgradeAttempt{done}},
		{name: "unsorted history", history: []UpgradeAttempt{done, hold}, want: []UpgradeAttempt{done}},
		{name: "local success clears stale hub history", history: []UpgradeAttempt{hold}, upgradedAt: now},
		{name: "success at hold timestamp resolves it", history: []UpgradeAttempt{hold}, upgradedAt: hold.EndedAt},
		{name: "real failure survives", history: []UpgradeAttempt{hold, failed, done}, want: []UpgradeAttempt{failed, done}},
		{name: "new delay after success survives", history: []UpgradeAttempt{done, newDelay}, want: []UpgradeAttempt{done, newDelay}},
		{name: "local success does not hide a newer delay", history: []UpgradeAttempt{newDelay}, upgradedAt: now, want: []UpgradeAttempt{newDelay}},
		{name: "open attempt survives", history: []UpgradeAttempt{unfinished, done}, want: []UpgradeAttempt{unfinished, done}},
		{name: "missing end falls back to start", history: []UpgradeAttempt{noEnd}, upgradedAt: now},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]UpgradeAttempt(nil), tc.history...)
			got := UpgradeHistoryForDisplay(tc.history, tc.upgradedAt)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("display history = %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(tc.history, original) {
				t.Fatal("presentation must not rewrite the audit history")
			}
			if len(got) > 0 {
				got[0].Reason = "changed by caller"
				if !reflect.DeepEqual(tc.history, original) {
					t.Fatal("returned history must not alias the input")
				}
			}
		})
	}
}

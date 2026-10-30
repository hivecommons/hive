package spoke

import "time"

// UpgradeHistoryForDisplay keeps the audit history intact while removing
// resolved schedule holds. Older hubs recorded those holds as missed windows.
// upgradedAt lets a spoke clear a hold before its next successful heartbeat.
func UpgradeHistoryForDisplay(history []UpgradeAttempt, upgradedAt time.Time) []UpgradeAttempt {
	for _, a := range history {
		if a.Outcome == UpgradeOutcomeDone && a.EndedAt.After(upgradedAt) {
			upgradedAt = a.EndedAt
		}
	}
	var rows []UpgradeAttempt
	for _, a := range history {
		if a.Outcome == UpgradeOutcomeDelayed ||
			(a.Trigger == UpgradeTriggerWindow && a.Outcome == UpgradeOutcomeMissed) {
			at := a.EndedAt
			if at.IsZero() {
				at = a.StartedAt
			}
			if !upgradedAt.IsZero() && !at.After(upgradedAt) {
				continue
			}
			a.Outcome = UpgradeOutcomeDelayed
		}
		rows = append(rows, a)
	}
	return rows
}

package main

// claudeSharedLoginUnsavedAlertID is the dashboard system-alert id for "the
// hive refreshed the shared Claude login but could not write the new grant"
// (hivecommons/hive#18775). Raised and cleared level-triggered, once per
// evaluation cycle, from the agent manager's own account of the held grant.
const claudeSharedLoginUnsavedAlertID = "claude-shared-login-unsaved"

// claudeSharedLoginUnsavedSource is implemented by the agent manager.
type claudeSharedLoginUnsavedSource interface {
	ClaudeSharedLoginUnsavedAlert() string
}

// claudeSharedLoginUnsavedSink is the subset of *dashboard.Server used.
type claudeSharedLoginUnsavedSink interface {
	AddSystemAlert(id, severity, message string)
	ClearSystemAlert(id string)
}

// reconcileClaudeSharedLoginUnsavedAlert raises the banner while a refreshed
// grant is held unwritten and clears it once it is saved. A runtime that does
// not hold grants (any other crux.Runtime) leaves it cleared.
func reconcileClaudeSharedLoginUnsavedAlert(sink claudeSharedLoginUnsavedSink, rt any) {
	if sink == nil {
		return
	}
	src, ok := rt.(claudeSharedLoginUnsavedSource)
	if !ok {
		sink.ClearSystemAlert(claudeSharedLoginUnsavedAlertID)
		return
	}
	if msg := src.ClaudeSharedLoginUnsavedAlert(); msg != "" {
		sink.AddSystemAlert(claudeSharedLoginUnsavedAlertID, "error", msg)
		return
	}
	sink.ClearSystemAlert(claudeSharedLoginUnsavedAlertID)
}

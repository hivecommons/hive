package agent

import (
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent/panesignal"
	"github.com/hivecommons/hive/crux/watchdog"
)

// Pane-signal parsing lives in the leaf package agent/panesignal; the names
// below keep the package-internal and exported call sites unchanged.

var cliPaneMarkers = panesignal.CLIPaneMarkers

const (
	bobInputPlaceholder       = panesignal.BobInputPlaceholder
	bobProductMarker          = panesignal.BobProductMarker
	piContextMarker           = panesignal.PiContextMarker
	codexInputPromptMarker    = panesignal.CodexInputPromptMarker
	codexProductMarker        = panesignal.CodexProductMarker
	copilotModelFailureMarker = panesignal.CopilotModelFailureMarker
	agyHeadlessReadyMarker    = panesignal.AgyHeadlessReadyMarker
	agyHeadlessRunningMarker  = panesignal.AgyHeadlessRunningMarker
)

var copilotTransportErrorPatterns = panesignal.CopilotTransportErrorPatterns

func paneHasCLIMarker(output string) bool { return panesignal.HasCLIMarker(output) }

func paneShowsAgentWorking(pane string) bool { return panesignal.ShowsAgentWorking(pane) }

func paneShowsInputPrompt(output string) bool { return panesignal.ShowsInputPrompt(output) }

func paneShowsCopilotQuestionForm(output string) bool {
	return panesignal.ShowsCopilotQuestionForm(output)
}

func isVisualNoise(s string) bool { return panesignal.IsVisualNoise(s) }

func isCLIChrome(s string) bool { return panesignal.IsCLIChrome(s) }

func isBufferNoise(s string) bool { return panesignal.IsBufferNoise(s) }

func filterPaneOutput(lines []string, n int) []string { return panesignal.FilterOutput(lines, n) }

// SanitizePaneText removes terminal/TUI chrome from pane-derived text before it
// is shown in chat, notifications or activity summaries. See
// panesignal.SanitizeText.
func SanitizePaneText(text string, maxLines int) string {
	return panesignal.SanitizeText(text, maxLines)
}

// DeduplicateBlocks removes repeated blocks from pane output. See
// panesignal.DeduplicateBlocks.
func DeduplicateBlocks(lines []string) []string { return panesignal.DeduplicateBlocks(lines) }

// snapshot copies the agent's observable state into the crux.Process the
// manager hands out from GetStatus, GetStatusFast and AllStatuses.
func (a *AgentProcess) snapshot() crux.Process {
	history := make([]KickRecord, len(a.KickHistory))
	copy(history, a.KickHistory)
	a.paneMu.RLock()
	pane := make([]string, len(a.lastPaneCapture))
	copy(pane, a.lastPaneCapture)
	// NeedsLogin and LastPaneChange are written by the pane poller under paneMu.
	needsLogin := a.NeedsLogin
	loginURL := a.LoginURL
	quotaExhausted := a.QuotaExhausted
	var subAgentModels []SubAgentModel
	if effectiveBackend(a) == "copilot" && a.LastKick != nil && a.subAgentKick.Equal(*a.LastKick) {
		subAgentModels = append(subAgentModels, a.SubAgentModels...)
	}
	lastPaneChange := a.LastPaneChange
	conds := make([]watchdog.Condition, len(a.WatchdogConditions))
	copy(conds, a.WatchdogConditions)
	a.paneMu.RUnlock()
	snap := crux.Process{
		Name:                      a.Name,
		ID:                        a.ID,
		Config:                    a.Config,
		State:                     a.State,
		PID:                       a.PID,
		UID:                       a.UID,
		StartedAt:                 a.StartedAt,
		LastKick:                  a.LastKick,
		KicksUndeliverable:        a.KicksUndeliverable,
		BusySince:                 a.BusySince,
		LastTranscriptActivity:    a.LastTranscriptActivity,
		BusyCondition:             a.BusyCondition,
		BusyConditionMessage:      a.BusyConditionMessage,
		Paused:                    a.Paused,
		PausedAt:                  a.PausedAt,
		PausedReason:              a.PausedReason,
		PausedTrigger:             a.PausedTrigger,
		PausedBy:                  a.PausedBy,
		OperatorStopped:           a.OperatorStopped,
		PinnedCLI:                 a.PinnedCLI,
		PinnedModel:               a.PinnedModel,
		ModelOverride:             a.ModelOverride,
		BackendOverride:           a.BackendOverride,
		RestartCount:              a.RestartCount,
		RestartEvents:             cloneRestartEvents(a.RestartEvents),
		LastRestartReason:         a.LastRestartReason,
		TurnLoss:                  cloneTurnLoss(a.TurnLoss),
		KickHistory:               history,
		LastKickMessage:           a.LastKickMessage,
		KickRefused:               a.KickRefused,
		KickRefusalReason:         a.KickRefusalReason,
		BootstrapOverride:         a.BootstrapOverride,
		LastError:                 a.LastError,
		KickOutcome:               a.KickOutcome,
		SandboxLastRun:            cloneSandboxRun(a.SandboxLastRun),
		NeedsLogin:                needsLogin,
		LoginURL:                  loginURL,
		Starting:                  a.startupLaunchQueued || a.launching,
		QuotaExhausted:            quotaExhausted,
		LastPaneChange:            lastPaneChange,
		WatchdogConditions:        conds,
		StallNudges:               a.StallNudges,
		StallRecoveries:           a.StallRecoveries,
		SessionRSSMB:              a.SessionRSSMB,
		SessionRecycles:           a.SessionRecycles,
		CIPollNudges:              a.CIPollNudges,
		AwaitingCI:                a.AwaitingCI,
		ActionNudges:              a.ActionNudges,
		TransientNudges:           a.TransientNudges,
		ProviderErrorClass:        a.ProviderErrorClass,
		ProviderErrorLine:         a.ProviderErrorLine,
		ProviderErrorBackoffUntil: a.ProviderErrorBackoffUntil,
		BackendAuth:               a.BackendAuth,
		StartFailureClass:         a.StartFailureClass,
		StartFailureReason:        a.StartFailureReason,
		StartFailureCount:         a.StartFailureCount,
		StartFailureLastAt:        a.StartFailureLastAt,
		StartFailureExitCode:      a.StartFailureExitCode,
		StartFailureSignal:        a.StartFailureSignal,
		StartBlocked:              a.StartBlocked,
		StartBackoffUntil:         a.StartBackoffUntil,
		HasLaunched:               a.HasLaunched,
		LaunchedMode:              a.LaunchedMode,
		OutputBuffer:              a.OutputBuffer,
		SubAgentModels:            subAgentModels,
	}
	snap.SetPaneCapture(pane, filterPaneOutput)
	return snap
}

// PaneLines returns the last n lines from the most recent tmux pane capture,
// preferring content from the current CLI session (after the last ❯ prompt).
// Falls back to showing the full tail if the current session has too few lines.
func (a *AgentProcess) PaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

func (a *AgentProcess) FilteredPaneLines(n int) []string {
	a.paneMu.RLock()
	defer a.paneMu.RUnlock()
	if len(a.lastPaneCapture) == 0 {
		return nil
	}
	return filterPaneOutput(a.lastPaneCapture, n)
}

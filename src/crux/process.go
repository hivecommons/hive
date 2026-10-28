package crux

import (
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// Process is the observable snapshot of one agent: state, pins, overrides,
// kick history, pane-derived conditions. It is what Observer.GetStatus and
// Observer.AllStatuses return.
//
// It is its own type, not the runtime's internal agent record: crux/agent
// copies the exported fields of its AgentProcess into a Process when it takes
// a snapshot, so this package does not import crux/agent (#13962, #13963).
// The field meanings are documented on agent.AgentProcess. A snapshot is a
// read-only display value; writing to one does not change the runtime.
type Process struct {
	Name                      string
	ID                        string
	Config                    config.AgentConfig
	State                     ProcessState
	PID                       int
	UID                       int
	StartedAt                 *time.Time
	LastKick                  *time.Time
	KicksUndeliverable        int
	BusySince                 time.Time
	LastTranscriptActivity    time.Time
	BusyCondition             string
	BusyConditionMessage      string
	Paused                    bool
	PausedAt                  time.Time
	PausedReason              string
	PausedTrigger             string
	PausedBy                  string
	OperatorStopped           bool
	PinnedCLI                 string
	PinnedModel               string
	ModelOverride             string
	BackendOverride           string
	RestartCount              int
	RestartEvents             []RestartEvent
	LastRestartReason         string
	OutputBuffer              *RingBuffer
	SubAgentModels            []SubAgentModel
	KickHistory               []KickRecord
	LastKickMessage           string
	KickRefused               bool
	KickRefusalReason         string
	LaunchedMode              AgentMode
	HasLaunched               bool
	BootstrapOverride         string
	LastError                 string
	NeedsLogin                bool
	LoginURL                  string
	Starting                  bool
	QuotaExhausted            bool
	WatchdogConditions        []Condition
	LastPaneChange            time.Time
	StallNudges               int
	StallRecoveries           int
	CIPollNudges              int
	AwaitingCI                bool
	TransientNudges           int
	ProviderErrorClass        string
	ProviderErrorLine         string
	ProviderErrorBackoffUntil time.Time
	BackendAuth               BackendAuthState
	TurnLoss                  TurnLoss
	ActionNudges              int
	KickOutcome               KickOutcome
	RepeatFailure             RepeatFailureStatus
	StartFailureClass         string
	StartFailureReason        string
	StartFailureCount         int
	StartFailureLastAt        time.Time
	StartFailureExitCode      *int
	StartFailureSignal        string
	StartBlocked              bool
	StartBackoffUntil         time.Time

	paneCapture []string
	paneFilter  func(lines []string, n int) []string
}

// SetPaneCapture records the pane capture a snapshot was taken with and the
// runtime's filter for it, which PaneLines and FilteredPaneLines apply. The
// runtime calls it when it builds a snapshot; lines must not be shared with
// the live capture.
func (p *Process) SetPaneCapture(lines []string, filter func(lines []string, n int) []string) {
	p.paneCapture = lines
	p.paneFilter = filter
}

// PaneLines returns the last n lines from the most recent tmux pane capture,
// preferring content from the current CLI session (after the last ❯ prompt).
// Falls back to showing the full tail if the current session has too few lines.
func (p *Process) PaneLines(n int) []string {
	if len(p.paneCapture) == 0 || p.paneFilter == nil {
		return nil
	}
	return p.paneFilter(p.paneCapture, n)
}

// FilteredPaneLines is PaneLines; both names are kept for their callers.
func (p *Process) FilteredPaneLines(n int) []string {
	return p.PaneLines(n)
}

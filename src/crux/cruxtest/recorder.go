package cruxtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/trajectory"
	"github.com/hivecommons/hive/crux/watchdog"
)

// Call is one recorded method call: the crux.Runtime (or crux.Host) method
// name and its arguments in order, contexts omitted.
type Call struct {
	Method string
	Args   []any
}

// String renders the call as Method(arg, arg, ...) for assertions.
func (c Call) String() string {
	return fmt.Sprintf("%s%v", c.Method, c.Args)
}

// Recorder is a crux.Runtime (and crux.Host, and a watchdog source) that
// records every call and answers from canned state, for tests of anything
// that drives a runtime without running one: the API server, the API client,
// the CLI. Every method appends a Call; the exported fields are the answers.
// Err, when set, is returned by every method that returns an error, except
// the ones with their own error field (ResumeErr, AuthorizeErr, KickErr).
// Methods that look an agent up answer "agent X not found" for a name absent
// from Procs. The zero value is usable; NewRecorder pre-sizes the maps.
type Recorder struct {
	mu    sync.Mutex
	calls []Call

	// Procs is the agent table AllStatuses/GetStatus answer from.
	Procs map[string]*crux.Process
	// Pane, Buffer, FullLog and Header answer the Observer log reads.
	Pane    map[string][]string
	Buffer  map[string][]string
	FullLog map[string]string
	Header  map[string]string
	// Err is the generic error answer; see the type comment.
	Err error
	// KickErr is SendKick / SendKickAsync / ValidateKick's error.
	KickErr error
	// KickStarted is SendKickAsync's started result.
	KickStarted bool
	// Dispatch answers KickDispatchState.
	Dispatch map[string]crux.KickDispatch
	// KickLogs and KickLogContent answer ListKickLogs and ReadKickLog (keyed
	// by agent, then by id).
	KickLogs       map[string][]crux.KickLogInfo
	KickLogContent map[string]map[string]string
	// Reconciled is ReconcileAgents' result.
	Reconciled []string
	// Breaker is the breaker state; EngageBreaker sets Engaged and returns
	// Paused, ReleaseBreaker clears it and returns Resumed.
	BreakerEngaged bool
	BreakerPaused  []string
	Resumed        []string
	// ACMM is the fleet level.
	ACMM int
	// Shared is SharedDevUID.
	Shared bool
	// Relaunched is RelaunchBobAgentsAwaitingKey's result; Crashed is
	// CheckAndRestartCrashedAgents'; Wedged is ConsentWedgedAgents'.
	Relaunched []string
	Crashed    []string
	Wedged     []string
	// WithModel is CountAgentsWithModel.
	WithModel int
	// Copilot and CopilotSource answer the Copilot credential reads;
	// SetCopilotToken and ActivateCopilotToken overwrite Copilot.
	Copilot       string
	CopilotSource string
	// ClaudePaths is ClaudeCredentialCandidatePaths.
	ClaudePaths []string
	// OmpHomes is OmpAgentHomes.
	OmpHomes []string
	// Handles, Sessions and Missing answer the Resumer reads.
	Handles  map[string]crux.ResumeHandle
	Sessions map[string]string
	Missing  map[string]bool
	// ResumeErr is SendResumeKick's error.
	ResumeErr error
	// Supervision answers the per-agent Supervisor reads.
	MayStart     bool
	Refreshable  bool
	RefreshKnown bool
	Valid        bool
	Caps         [4]bool
	Backend      string
	BackendOK    bool
	Invocation   [3]string
	InvocationOK bool
	Restarts     struct {
		Total, Last24h int
		LastAt         time.Time
		LastReason     string
		OK             bool
	}
	StartFailure   crux.StartFailureSnapshot
	StartFailureOK bool
	Backoff        struct {
		Remaining        time.Duration
		Provider, Reason string
		OK               bool
	}
	OOM          bool
	AuthAvail    bool
	AuthKnown    bool
	BackendAuth  map[string][2]bool
	Views        []trajectory.AgentView
	AuthorizeErr error

	// Observers installed through Kicker.SetKickObserver and crux.Host.
	KickObserver        func(agentName, event, detail string)
	KickOutcomeObserver func(agentName string, outcome crux.KickOutcome)
	PauseObserver       func(crux.PauseTransitionEvent)
	RelaunchObserver    func(agentName string)
	PersistPause        func(name string, paused bool)
	RecordPrompt        func(agent, trigger, prompt string)
	SandboxAudit        func(agent, action, detail string)
	Audit               crux.AuditSink
	Conditions          crux.ConditionReporter

	// Watchdog source answers.
	WatchdogNames  []string
	Observations   map[string]watchdog.Observation
	Production     map[string]time.Time
	SetConds       map[string][]crux.Condition
	BackgroundRuns []string
}

// NewRecorder returns a Recorder with its maps allocated.
func NewRecorder() *Recorder {
	return &Recorder{
		Procs:          map[string]*crux.Process{},
		Pane:           map[string][]string{},
		Buffer:         map[string][]string{},
		FullLog:        map[string]string{},
		Header:         map[string]string{},
		Dispatch:       map[string]crux.KickDispatch{},
		KickLogs:       map[string][]crux.KickLogInfo{},
		KickLogContent: map[string]map[string]string{},
		Handles:        map[string]crux.ResumeHandle{},
		Sessions:       map[string]string{},
		Missing:        map[string]bool{},
		BackendAuth:    map[string][2]bool{},
		Observations:   map[string]watchdog.Observation{},
		Production:     map[string]time.Time{},
		SetConds:       map[string][]crux.Condition{},
	}
}

// Add registers an agent in Procs with the given id and state and returns
// its record.
func (r *Recorder) Add(name, id string, state crux.ProcessState) *crux.Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &crux.Process{Name: name, ID: id, State: state, Config: config.AgentConfig{ID: id, Backend: "claude", Model: "opus", Enabled: true}}
	r.Procs[name] = p
	return p
}

// Calls returns a copy of the recorded calls.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Methods returns the recorded method names in order.
func (r *Recorder) Methods() []string {
	calls := r.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Method
	}
	return out
}

// Last returns the most recent call to method, if any.
func (r *Recorder) Last(method string) (Call, bool) {
	calls := r.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Method == method {
			return calls[i], true
		}
	}
	return Call{}, false
}

// Reset forgets the recorded calls.
func (r *Recorder) Reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

func (r *Recorder) record(method string, args ...any) {
	r.mu.Lock()
	r.calls = append(r.calls, Call{Method: method, Args: args})
	r.mu.Unlock()
}

func (r *Recorder) lookup(name string) (*crux.Process, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.Procs[name]
	if !ok {
		return nil, fmt.Errorf("agent %s not found", name)
	}
	return p, nil
}

// errFor is the error a lookup-then-act method returns: the generic Err if
// set, else not-found for an unknown agent.
func (r *Recorder) errFor(name string) error {
	if r.Err != nil {
		return r.Err
	}
	_, err := r.lookup(name)
	return err
}

func copyProcess(p *crux.Process) *crux.Process {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// ---- Lifecycle ----

func (r *Recorder) Start(_ context.Context, name string) error {
	r.record("Start", name)
	if err := r.errFor(name); err != nil {
		return err
	}
	r.mu.Lock()
	r.Procs[name].State = crux.StateRunning
	r.mu.Unlock()
	return nil
}

func (r *Recorder) Stop(name string) error {
	r.record("Stop", name)
	if err := r.errFor(name); err != nil {
		return err
	}
	r.mu.Lock()
	r.Procs[name].State = crux.StateStopped
	r.mu.Unlock()
	return nil
}

func (r *Recorder) Restart(_ context.Context, name string) error {
	r.record("Restart", name)
	return r.errFor(name)
}

func (r *Recorder) RestartWithBootstrap(_ context.Context, name, prompt string) error {
	r.record("RestartWithBootstrap", name, prompt)
	return r.errFor(name)
}

func (r *Recorder) Pause(name, trigger, reason string) error {
	r.record("Pause", name, trigger, reason)
	return r.setPaused(name, true, trigger, reason, "")
}

func (r *Recorder) PauseBy(name, trigger, reason, by string) error {
	r.record("PauseBy", name, trigger, reason, by)
	return r.setPaused(name, true, trigger, reason, by)
}

func (r *Recorder) PauseByCause(name, trigger, reason, by string, cause crux.PauseCausation) error {
	r.record("PauseByCause", name, trigger, reason, by, cause)
	return r.setPaused(name, true, trigger, reason, by)
}

func (r *Recorder) Resume(_ context.Context, name, trigger, reason string) error {
	r.record("Resume", name, trigger, reason)
	return r.setPaused(name, false, trigger, reason, "")
}

func (r *Recorder) setPaused(name string, paused bool, trigger, reason, by string) error {
	if err := r.errFor(name); err != nil {
		return err
	}
	r.mu.Lock()
	p := r.Procs[name]
	p.Paused, p.PausedTrigger, p.PausedReason, p.PausedBy = paused, trigger, reason, by
	r.mu.Unlock()
	return nil
}

func (r *Recorder) IsPaused(name string) bool {
	r.record("IsPaused", name)
	p, err := r.lookup(name)
	return err == nil && p.Paused
}

func (r *Recorder) ResetRestartCount(name string) error {
	r.record("ResetRestartCount", name)
	if err := r.errFor(name); err != nil {
		return err
	}
	r.mu.Lock()
	r.Procs[name].RestartCount = 0
	r.mu.Unlock()
	return nil
}

// ---- Observer ----

func (r *Recorder) GetStatus(name string) (*crux.Process, error) {
	r.record("GetStatus", name)
	p, err := r.lookup(name)
	if err != nil {
		return nil, err
	}
	c := copyProcess(p)
	r.mu.Lock()
	pane := append([]string(nil), r.Pane[name]...)
	r.mu.Unlock()
	if len(pane) > 0 {
		c.SetPaneCapture(pane, tail)
	}
	return c, nil
}

func tail(lines []string, n int) []string {
	if n <= 0 || n >= len(lines) {
		return lines
	}
	return lines[len(lines)-n:]
}

func (r *Recorder) GetStatusFast(name string) (*crux.Process, error) {
	r.record("GetStatusFast", name)
	p, err := r.lookup(name)
	if err != nil {
		return nil, err
	}
	return copyProcess(p), nil
}

func (r *Recorder) AllStatuses() map[string]*crux.Process {
	r.record("AllStatuses")
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]*crux.Process, len(r.Procs))
	for k, v := range r.Procs {
		out[k] = copyProcess(v)
	}
	return out
}

func (r *Recorder) ResolveAgent(nameOrID string) string {
	r.record("ResolveAgent", nameOrID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.Procs[nameOrID]; ok {
		return nameOrID
	}
	for name, p := range r.Procs {
		if p.ID == nameOrID {
			return name
		}
	}
	return nameOrID
}

func (r *Recorder) GetOutput(name string, lines int) ([]string, error) {
	r.record("GetOutput", name, lines)
	if err := r.errFor(name); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return tail(append([]string(nil), r.Pane[name]...), lines), nil
}

func (r *Recorder) GetBufferOutput(name string, lines int) ([]string, error) {
	r.record("GetBufferOutput", name, lines)
	if err := r.errFor(name); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return tail(append([]string(nil), r.Buffer[name]...), lines), nil
}

func (r *Recorder) CaptureFullLog(name string) (string, error) {
	r.record("CaptureFullLog", name)
	if err := r.errFor(name); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.FullLog[name], nil
}

func (r *Recorder) FullLogHeader(name string) string {
	r.record("FullLogHeader", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Header[name]
}

// ---- Kicker ----

func (r *Recorder) SendKick(name string, message string) error {
	r.record("SendKick", name, message)
	if r.KickErr != nil {
		return r.KickErr
	}
	return r.errFor(name)
}

func (r *Recorder) SendKickWithSource(name string, message string, source string) error {
	r.record("SendKickWithSource", name, message, source)
	if r.KickErr != nil {
		return r.KickErr
	}
	return r.errFor(name)
}

func (r *Recorder) SendKickAsync(name string, message string) (bool, error) {
	r.record("SendKickAsync", name, message)
	if r.KickErr != nil {
		return false, r.KickErr
	}
	if err := r.errFor(name); err != nil {
		return false, err
	}
	return r.KickStarted, nil
}

func (r *Recorder) ValidateKick(name string) error {
	r.record("ValidateKick", name)
	if r.KickErr != nil {
		return r.KickErr
	}
	return r.errFor(name)
}

func (r *Recorder) KickDispatchState(name string) (crux.KickDispatch, bool) {
	r.record("KickDispatchState", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.Dispatch[name]
	return d, ok
}

func (r *Recorder) ListKickLogs(name string) ([]crux.KickLogInfo, error) {
	r.record("ListKickLogs", name)
	if err := r.errFor(name); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]crux.KickLogInfo(nil), r.KickLogs[name]...), nil
}

func (r *Recorder) ReadKickLog(name, id string) (string, error) {
	r.record("ReadKickLog", name, id)
	if err := r.errFor(name); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	content, ok := r.KickLogContent[name][id]
	if !ok {
		return "", fmt.Errorf("kick log %s not found", id)
	}
	return content, nil
}

func (r *Recorder) SetKickObserver(fn func(agentName, event, detail string)) {
	r.record("SetKickObserver", fn != nil)
	r.mu.Lock()
	r.KickObserver = fn
	r.mu.Unlock()
}

// EmitKick calls the installed kick observer, if any.
func (r *Recorder) EmitKick(agentName, event, detail string) {
	r.mu.Lock()
	fn := r.KickObserver
	r.mu.Unlock()
	if fn != nil {
		fn(agentName, event, detail)
	}
}

// ---- Definitions ----

func (r *Recorder) AddAgent(name string, cfg config.AgentConfig) {
	r.record("AddAgent", name, cfg)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.Procs[name]; exists {
		return
	}
	r.Procs[name] = &crux.Process{Name: name, ID: cfg.ID, Config: cfg, State: crux.StateStopped}
}

func (r *Recorder) RemoveAgent(name string) {
	r.record("RemoveAgent", name)
	r.mu.Lock()
	delete(r.Procs, name)
	r.mu.Unlock()
}

func (r *Recorder) UpdateConfig(name string, cfg config.AgentConfig) error {
	r.record("UpdateConfig", name, cfg)
	if err := r.errFor(name); err != nil {
		return err
	}
	r.mu.Lock()
	r.Procs[name].Config = cfg
	r.mu.Unlock()
	return nil
}

func (r *Recorder) ReconcileAgents(configs map[string]config.AgentConfig) []string {
	names := make([]string, 0, len(configs))
	for n := range configs {
		names = append(names, n)
	}
	sort.Strings(names)
	r.record("ReconcileAgents", names)
	return append([]string(nil), r.Reconciled...)
}

func (r *Recorder) SetModelOverride(name, model string) error {
	r.record("SetModelOverride", name, model)
	if err := r.errFor(name); err != nil {
		return err
	}
	r.mu.Lock()
	r.Procs[name].ModelOverride = model
	r.mu.Unlock()
	return nil
}

func (r *Recorder) SetBackendOverride(name, backend string) error {
	r.record("SetBackendOverride", name, backend)
	if err := r.errFor(name); err != nil {
		return err
	}
	r.mu.Lock()
	r.Procs[name].BackendOverride = backend
	r.mu.Unlock()
	return nil
}

func (r *Recorder) PinCLI(name, version string) error {
	r.record("PinCLI", name, version)
	return r.errFor(name)
}

func (r *Recorder) PinModel(name, model string) error {
	r.record("PinModel", name, model)
	return r.errFor(name)
}

func (r *Recorder) UnpinCLI(name string) error {
	r.record("UnpinCLI", name)
	return r.errFor(name)
}

func (r *Recorder) UnpinModel(name string) error {
	r.record("UnpinModel", name)
	return r.errFor(name)
}

func (r *Recorder) ClearModeOverrides(names []string) {
	r.record("ClearModeOverrides", append([]string(nil), names...))
}

func (r *Recorder) SyncModeFiles(level int) {
	r.record("SyncModeFiles", level)
}

func (r *Recorder) RefreshInferenceRoutes(backend string) {
	r.record("RefreshInferenceRoutes", backend)
}

// ---- Fleet ----

func (r *Recorder) BreakerState() (bool, []string) {
	r.record("BreakerState")
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.BreakerEngaged, append([]string(nil), r.BreakerPaused...)
}

func (r *Recorder) EngageBreaker() []string {
	r.record("EngageBreaker")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.BreakerEngaged = true
	return append([]string(nil), r.BreakerPaused...)
}

func (r *Recorder) ReleaseBreaker(context.Context) []string {
	r.record("ReleaseBreaker")
	r.mu.Lock()
	defer r.mu.Unlock()
	r.BreakerEngaged = false
	return append([]string(nil), r.Resumed...)
}

func (r *Recorder) SetACMMLevel(level int) {
	r.record("SetACMMLevel", level)
	r.mu.Lock()
	r.ACMM = level
	r.mu.Unlock()
}

func (r *Recorder) GetACMMLevel() int {
	r.record("GetACMMLevel")
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ACMM
}

func (r *Recorder) RelaunchBobAgentsAwaitingKey(context.Context) []string {
	r.record("RelaunchBobAgentsAwaitingKey")
	return append([]string(nil), r.Relaunched...)
}

func (r *Recorder) SharedDevUID() bool {
	r.record("SharedDevUID")
	return r.Shared
}

// ---- Credentials ----

func (r *Recorder) SubmitLoginCode(name, code string) error {
	r.record("SubmitLoginCode", name, code)
	return r.errFor(name)
}

func (r *Recorder) CopilotToken() string {
	r.record("CopilotToken")
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Copilot
}

func (r *Recorder) CopilotTokenSource() string {
	r.record("CopilotTokenSource")
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.CopilotSource
}

func (r *Recorder) SetCopilotToken(token string) {
	r.record("SetCopilotToken", token)
	r.mu.Lock()
	r.Copilot = token
	r.mu.Unlock()
}

func (r *Recorder) ActivateCopilotToken(token string) error {
	r.record("ActivateCopilotToken", token)
	if r.Err != nil {
		return r.Err
	}
	r.mu.Lock()
	r.Copilot = token
	r.mu.Unlock()
	return nil
}

func (r *Recorder) ReloadClaudeToken() {
	r.record("ReloadClaudeToken")
}

func (r *Recorder) ClaudeCredentialCandidatePaths() []string {
	r.record("ClaudeCredentialCandidatePaths")
	return append([]string(nil), r.ClaudePaths...)
}

func (r *Recorder) OmpAgentHomes() []string {
	r.record("OmpAgentHomes")
	return append([]string(nil), r.OmpHomes...)
}

// ---- Resumer ----

func (r *Recorder) ResumeHandle(name string) (crux.ResumeHandle, bool) {
	r.record("ResumeHandle", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.Handles[name]
	return h, ok
}

func (r *Recorder) SendResumeKick(name, message, sessionID string) error {
	r.record("SendResumeKick", name, message, sessionID)
	if r.ResumeErr != nil {
		return r.ResumeErr
	}
	return r.errFor(name)
}

func (r *Recorder) SessionID(name string) (string, bool) {
	r.record("SessionID", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.Sessions[name]
	return id, ok
}

func (r *Recorder) SessionMissing(name string) bool {
	r.record("SessionMissing", name)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.Missing[name]
}

// ---- Supervisor ----

func (r *Recorder) GovernorMayStart(name string) bool {
	r.record("GovernorMayStart", name)
	return r.MayStart
}

func (r *Recorder) MarkStartupLaunchQueued(names []string) {
	r.record("MarkStartupLaunchQueued", append([]string(nil), names...))
}

func (r *Recorder) CheckAndRestartCrashedAgents(context.Context) []string {
	r.record("CheckAndRestartCrashedAgents")
	return append([]string(nil), r.Crashed...)
}

func (r *Recorder) ConsentWedgedAgents() []string {
	r.record("ConsentWedgedAgents")
	return append([]string(nil), r.Wedged...)
}

func (r *Recorder) RefreshAgentTokens(context.Context) {
	r.record("RefreshAgentTokens")
}

func (r *Recorder) AgentCredentialRefreshable(agentName string) (bool, bool) {
	r.record("AgentCredentialRefreshable", agentName)
	return r.Refreshable, r.RefreshKnown
}

func (r *Recorder) AgentCapabilities(agentName string) (bool, bool, bool, bool) {
	r.record("AgentCapabilities", agentName)
	return r.Caps[0], r.Caps[1], r.Caps[2], r.Caps[3]
}

func (r *Recorder) EffectiveBackend(agentName string) (string, bool) {
	r.record("EffectiveBackend", agentName)
	return r.Backend, r.BackendOK
}

func (r *Recorder) InvocationMetadata(agentName string) (string, string, string, bool) {
	r.record("InvocationMetadata", agentName)
	return r.Invocation[0], r.Invocation[1], r.Invocation[2], r.InvocationOK
}

func (r *Recorder) RestartTelemetry(name string) (int, int, time.Time, string, bool) {
	r.record("RestartTelemetry", name)
	t := r.Restarts
	return t.Total, t.Last24h, t.LastAt, t.LastReason, t.OK
}

func (r *Recorder) StartFailureState(name string) (crux.StartFailureSnapshot, bool) {
	r.record("StartFailureState", name)
	return r.StartFailure, r.StartFailureOK
}

func (r *Recorder) ProviderErrorBackoffRemaining(name string) (time.Duration, string, string, bool) {
	r.record("ProviderErrorBackoffRemaining", name)
	b := r.Backoff
	return b.Remaining, b.Provider, b.Reason, b.OK
}

func (r *Recorder) ArchiveAllKickLogs(reason string) {
	r.record("ArchiveAllKickLogs", reason)
}

func (r *Recorder) CountAgentsWithModel() int {
	r.record("CountAgentsWithModel")
	return r.WithModel
}

func (r *Recorder) CrashOOMSuspected(name string) bool {
	r.record("CrashOOMSuspected", name)
	return r.OOM
}

func (r *Recorder) BackendAuthAvailable(backend string) (bool, bool) {
	r.record("BackendAuthAvailable", backend)
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.BackendAuth[backend]
	return v[0], v[1]
}

func (r *Recorder) AgentAuthAvailable(agentName string) (bool, bool) {
	r.record("AgentAuthAvailable", agentName)
	return r.AuthAvail, r.AuthKnown
}

func (r *Recorder) SendGitHubAPIBudgetNudge(name, message string) {
	r.record("SendGitHubAPIBudgetNudge", name, message)
}

func (r *Recorder) RecordAudit(action, agentName string, fields map[string]any) {
	r.record("RecordAudit", action, agentName, fields)
}

func (r *Recorder) AgentHasValidCredential(agentName string) bool {
	r.record("AgentHasValidCredential", agentName)
	return r.Valid
}

func (r *Recorder) RefreshAgentTokenFor(_ context.Context, name string) error {
	r.record("RefreshAgentTokenFor", name)
	return r.errFor(name)
}

func (r *Recorder) TrajectoryAgents() []trajectory.AgentView {
	r.record("TrajectoryAgents")
	return append([]trajectory.AgentView(nil), r.Views...)
}

// ---- Restorer ----

func (r *Recorder) SeedPauseState(name string, pausedAt time.Time, trigger, reason, by string) {
	r.record("SeedPauseState", name, pausedAt, trigger, reason, by)
}

func (r *Recorder) SeedRestartTelemetry(name string, count int, events []crux.RestartEvent, lastReason string) {
	r.record("SeedRestartTelemetry", name, count, append([]crux.RestartEvent(nil), events...), lastReason)
}

func (r *Recorder) SeedTurnLoss(name string, loss crux.TurnLoss) {
	r.record("SeedTurnLoss", name, loss)
}

func (r *Recorder) SeedLastKick(name string, t time.Time) {
	r.record("SeedLastKick", name, t)
}

func (r *Recorder) SeedKickHistory(name string, records []crux.KickRecord) {
	r.record("SeedKickHistory", name, append([]crux.KickRecord(nil), records...))
}

func (r *Recorder) RestoreBreaker(engaged bool, paused []string) {
	r.record("RestoreBreaker", engaged, append([]string(nil), paused...))
	r.mu.Lock()
	r.BreakerEngaged, r.BreakerPaused = engaged, append([]string(nil), paused...)
	r.mu.Unlock()
}

// ---- Background ----
//
// Each loop records its start and returns when ctx ends.

func (r *Recorder) StartAgentTokenRefresh(ctx context.Context) {
	r.background(ctx, "StartAgentTokenRefresh")
}

func (r *Recorder) StartCredentialWatchdog(ctx context.Context) {
	r.background(ctx, "StartCredentialWatchdog")
}

func (r *Recorder) StartCopilotSessionRefresh(ctx context.Context) {
	r.background(ctx, "StartCopilotSessionRefresh")
}

func (r *Recorder) StartTranscriptActivityRefresh(ctx context.Context) {
	r.background(ctx, "StartTranscriptActivityRefresh")
}

func (r *Recorder) StartRepeatFailureMonitor(ctx context.Context) {
	r.background(ctx, "StartRepeatFailureMonitor")
}

// Background lists the Background loops that have been started, in order;
// safe to call while they run.
func (r *Recorder) Background() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.BackgroundRuns...)
}

func (r *Recorder) background(ctx context.Context, name string) {
	r.record(name)
	r.mu.Lock()
	r.BackgroundRuns = append(r.BackgroundRuns, name)
	r.mu.Unlock()
	<-ctx.Done()
}

// ---- Authorizer ----

func (r *Recorder) AuthorizePROpen(agentName string, fileUID int) error {
	r.record("AuthorizePROpen", agentName, fileUID)
	return r.AuthorizeErr
}

func (r *Recorder) AuthorizePushBranch(agentName string, fileUID int) error {
	r.record("AuthorizePushBranch", agentName, fileUID)
	return r.AuthorizeErr
}

func (r *Recorder) AuthorizeIssueOpen(agentName string, fileUID int, kind string) error {
	r.record("AuthorizeIssueOpen", agentName, fileUID, kind)
	return r.AuthorizeErr
}

func (r *Recorder) AuthorizeMerge(agentName string, fileUID int, repo string) error {
	r.record("AuthorizeMerge", agentName, fileUID, repo)
	return r.AuthorizeErr
}

func (r *Recorder) AuthorizeReviewRequest(agentName string, fileUID int) error {
	r.record("AuthorizeReviewRequest", agentName, fileUID)
	return r.AuthorizeErr
}

// ---- crux.Host (observers kept; the rest recorded) ----

func (r *Recorder) SetAppAuth(auth crux.AppTokenMinter)      { r.record("SetAppAuth", auth != nil) }
func (r *Recorder) SetAgentMint(issuer crux.AgentMintIssuer) { r.record("SetAgentMint", issuer != nil) }
func (r *Recorder) SetLinearCredentialResolver(fn func() crux.LinearCredential) {
	r.record("SetLinearCredentialResolver", fn != nil)
}
func (r *Recorder) SetBobAPIKeyResolver(fn func() string) {
	r.record("SetBobAPIKeyResolver", fn != nil)
}
func (r *Recorder) SetBobKeySourceResolver(fn func() string) {
	r.record("SetBobKeySourceResolver", fn != nil)
}
func (r *Recorder) SetSandboxPushMinter(minter pushbroker.TokenMinter) {
	r.record("SetSandboxPushMinter", minter != nil)
}
func (r *Recorder) SetSandboxPRClient(client crux.PRCreator) {
	r.record("SetSandboxPRClient", client != nil)
}
func (r *Recorder) SetBuildInfo(fn func() (version, commit string)) {
	r.record("SetBuildInfo", fn != nil)
}
func (r *Recorder) SetGatewayBackendChecker(fn func(backend string) bool) {
	r.record("SetGatewayBackendChecker", fn != nil)
}
func (r *Recorder) SetExplainModeDefaultResolver(fn func() string) {
	r.record("SetExplainModeDefaultResolver", fn != nil)
}

func (r *Recorder) SetAdvisorEnabledResolver(fn func(string) bool) {
	r.record("SetAdvisorEnabledResolver", fn != nil)
}

func (r *Recorder) SetAdvisorRolesResolver(fn func() map[string]config.ModelRole) {
	r.record("SetAdvisorRolesResolver", fn != nil)
}

func (r *Recorder) SetRepeatFailureEnabledResolver(fn func() bool) {
	r.record("SetRepeatFailureEnabledResolver", fn != nil)
}
func (r *Recorder) SetRepeatFailureConfigResolver(fn func() *config.RepeatFailureConfig) {
	r.record("SetRepeatFailureConfigResolver", fn != nil)
}
func (r *Recorder) SetRepoAutoMergeEnabledResolver(fn func(repo string) bool) {
	r.record("SetRepoAutoMergeEnabledResolver", fn != nil)
}
func (r *Recorder) SetKickLogSourceResolver(fn func() string) {
	r.record("SetKickLogSourceResolver", fn != nil)
}
func (r *Recorder) SetInferenceCallbacks(setRoute func(agentName, backend, model string), clearRoute func(agentName string)) {
	r.record("SetInferenceCallbacks", setRoute != nil, clearRoute != nil)
}
func (r *Recorder) SetSandboxConfig(cfg config.AgentSandboxConfig) { r.record("SetSandboxConfig", cfg) }
func (r *Recorder) SetSandboxMutationBoundary(boundary effects.Boundary) {
	r.record("SetSandboxMutationBoundary", boundary != nil)
}
func (r *Recorder) SetDeadSessionRecoveryOwner(ownedElsewhere bool) {
	r.record("SetDeadSessionRecoveryOwner", ownedElsewhere)
}
func (r *Recorder) SetSpanStarter(fn crux.SpanStarter) { r.record("SetSpanStarter", fn != nil) }

func (r *Recorder) SetKickOutcomeObserver(fn func(agentName string, outcome crux.KickOutcome)) {
	r.record("SetKickOutcomeObserver", fn != nil)
	r.mu.Lock()
	r.KickOutcomeObserver = fn
	r.mu.Unlock()
}

func (r *Recorder) SetPauseTransitionObserver(fn func(crux.PauseTransitionEvent)) {
	r.record("SetPauseTransitionObserver", fn != nil)
	r.mu.Lock()
	r.PauseObserver = fn
	r.mu.Unlock()
}

func (r *Recorder) SetRelaunchObserver(fn func(agentName string)) {
	r.record("SetRelaunchObserver", fn != nil)
	r.mu.Lock()
	r.RelaunchObserver = fn
	r.mu.Unlock()
}

func (r *Recorder) SetPersistPauseCallback(fn func(name string, paused bool)) {
	r.record("SetPersistPauseCallback", fn != nil)
	r.mu.Lock()
	r.PersistPause = fn
	r.mu.Unlock()
}

func (r *Recorder) SetRecordPromptCallback(fn func(agent, trigger, prompt string)) {
	r.record("SetRecordPromptCallback", fn != nil)
	r.mu.Lock()
	r.RecordPrompt = fn
	r.mu.Unlock()
}

func (r *Recorder) SetSandboxAuditCallback(fn func(agent, action, detail string)) {
	r.record("SetSandboxAuditCallback", fn != nil)
	r.mu.Lock()
	r.SandboxAudit = fn
	r.mu.Unlock()
}

func (r *Recorder) SetAuditSink(sink crux.AuditSink) {
	r.record("SetAuditSink", sink != nil)
	r.mu.Lock()
	r.Audit = sink
	r.mu.Unlock()
}

func (r *Recorder) SetConditionReporter(rep crux.ConditionReporter) {
	r.record("SetConditionReporter", rep != nil)
	r.mu.Lock()
	r.Conditions = rep
	r.mu.Unlock()
}

// Observers returns the observer callbacks installed through crux.Host, for
// a test to fire them.
func (r *Recorder) Observers() (kickOutcome func(string, crux.KickOutcome), pause func(crux.PauseTransitionEvent), relaunch func(string), persist func(string, bool), prompt func(string, string, string), sandbox func(string, string, string), audit crux.AuditSink, conditions crux.ConditionReporter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.KickOutcomeObserver, r.PauseObserver, r.RelaunchObserver, r.PersistPause, r.RecordPrompt, r.SandboxAudit, r.Audit, r.Conditions
}

// ErrRecorder is a convenient generic failure for tests that set Err.
var ErrRecorder = errors.New("recorder: injected failure")

var (
	_ crux.Runtime = (*Recorder)(nil)
	_ crux.Host    = (*Recorder)(nil)
)

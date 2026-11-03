// Package client is the sidecar half of the Crux runtime seam
// (hivecommons/hive#14138, stage 3): Runtime implements crux.Runtime by
// calling a `crux serve` process over the Crux API, so a host built against
// the interface runs unchanged whether the agents live in its process or in
// a sidecar. crux/runtime.New returns one for config.ModeSidecar.
//
// Every method maps to a route in api.RuntimeRoutes; TestParity in this
// package holds the interface and the table to each other, so a method
// cannot be added to crux.Runtime without an endpoint. Methods whose
// signature has no error return log the failure through Logger and answer
// their zero value, never a guess: a read that fails reports "unknown"
// (ok=false), a write that fails is dropped with a log line. Authorizer
// methods fail closed: anything but a 204 is a denial.
//
// The Background family is a no-op here. The loops run inside the serving
// process (serve.Server.Background), which lists them on /v1/health.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/trajectory"
)

// DefaultTimeout bounds one API call when the method has no context of its
// own. Kicks and restarts can take a while (tmux round trips, CLI boot).
const DefaultTimeout = 2 * time.Minute

// eventsRetry is how long the event stream waits before reconnecting after
// the server dropped it.
const eventsRetry = time.Second

// Runtime is crux.Runtime over a `crux serve` process. Construct with New
// or Wrap; the zero value is not usable.
type Runtime struct {
	c *api.Client
	// Logger receives dropped-write and failed-read diagnostics; nil discards.
	Logger *slog.Logger
	// Timeout bounds calls that have no context parameter; zero means
	// DefaultTimeout.
	Timeout time.Duration

	mu            sync.Mutex
	kickObserver  func(agentName, event, detail string)
	eventObserver func(api.Event)
	cancelEvents  context.CancelFunc
	eventsDone    chan struct{}
	retry         time.Duration
}

// New connects to the server at addr (see api.ParseAddress) with token.
func New(addr, token string) (*Runtime, error) {
	c, err := api.NewClient(addr, token)
	if err != nil {
		return nil, err
	}
	return Wrap(c), nil
}

// Wrap returns a Runtime over an existing api.Client.
func Wrap(c *api.Client) *Runtime {
	return &Runtime{c: c, Logger: slog.New(slog.DiscardHandler), retry: eventsRetry}
}

// Client returns the underlying api.Client.
func (r *Runtime) Client() *api.Client { return r.c }

// Ready blocks until the server answers its health route or ctx ends.
func (r *Runtime) Ready(ctx context.Context, interval time.Duration) error {
	return r.c.WaitReady(ctx, interval)
}

func (r *Runtime) ctx() (context.Context, context.CancelFunc) {
	d := r.Timeout
	if d <= 0 {
		d = DefaultTimeout
	}
	return context.WithTimeout(context.Background(), d)
}

func (r *Runtime) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

// call is one request with a fresh timeout context.
func (r *Runtime) call(method, path string, in, out any) error {
	ctx, cancel := r.ctx()
	defer cancel()
	return unavailable(r.c.Do(ctx, method, path, in, out))
}

// callCtx is one request under the caller's context.
func (r *Runtime) callCtx(ctx context.Context, method, path string, in, out any) error {
	return unavailable(r.c.Do(ctx, method, path, in, out))
}

// unavailable marks a transport failure as crux.ErrRuntimeUnavailable so a
// host that only knows crux can tell "runtime gone" from "runtime said no".
func unavailable(err error) error {
	if err != nil && errors.Is(err, api.ErrUnreachable) {
		return fmt.Errorf("%w: %w", crux.ErrRuntimeUnavailable, err)
	}
	return err
}

// Status is the server's reachability as of the last call.
func (r *Runtime) Status() api.Status { return r.c.Status() }

// drop logs a failed write whose method has no error return.
func (r *Runtime) drop(method string, err error, attrs ...any) {
	r.logger().Warn("crux client: "+method+" dropped", append(attrs, "err", err)...)
}

func esc(name string) string { return url.PathEscape(name) }

// ---- Lifecycle ----

func (r *Runtime) Start(ctx context.Context, name string) error {
	return r.callCtx(ctx, http.MethodPost, api.StartPath(esc(name)), nil, nil)
}

func (r *Runtime) Stop(name string) error {
	return r.call(http.MethodPost, api.StopPath(esc(name)), nil, nil)
}

func (r *Runtime) Restart(ctx context.Context, name string) error {
	return r.callCtx(ctx, http.MethodPost, api.RestartPath(esc(name)), api.RestartRequest{}, nil)
}

func (r *Runtime) RestartWithBootstrap(ctx context.Context, name, prompt string) error {
	return r.callCtx(ctx, http.MethodPost, api.RestartPath(esc(name)), api.RestartRequest{Bootstrap: prompt}, nil)
}

func (r *Runtime) Pause(name, trigger, reason string) error {
	return r.call(http.MethodPost, api.PausePath(esc(name)), api.PauseRequest{Trigger: trigger, Reason: reason}, nil)
}

func (r *Runtime) PauseBy(name, trigger, reason, by string) error {
	return r.call(http.MethodPost, api.PausePath(esc(name)), api.PauseRequest{Trigger: trigger, Reason: reason, By: by}, nil)
}

func (r *Runtime) PauseByCause(name, trigger, reason, by string, cause crux.PauseCausation) error {
	return r.call(http.MethodPost, api.PausePath(esc(name)), api.PauseRequest{
		Trigger: trigger, Reason: reason, By: by,
		Cause: &api.PauseCausation{Depth: cause.Depth, HookName: cause.HookName, OriginTransition: cause.OriginTransition},
	}, nil)
}

func (r *Runtime) Resume(ctx context.Context, name, trigger, reason string) error {
	return r.callCtx(ctx, http.MethodPost, api.ResumePath(esc(name)), api.ResumeRequest{Trigger: trigger, Reason: reason}, nil)
}

func (r *Runtime) IsPaused(name string) bool {
	var p api.Process
	if err := r.call(http.MethodGet, api.AgentPath(esc(name)), nil, &p); err != nil {
		return false
	}
	return p.Paused
}

func (r *Runtime) ResetRestartCount(name string) error {
	return r.call(http.MethodDelete, api.RestartCountPath(esc(name)), nil, nil)
}

// ---- Observer ----

// tail is the pane filter a wire snapshot gets: the server already applied
// the runtime's current-session filter, so the client keeps the last n.
func tail(lines []string, n int) []string {
	if n <= 0 || n >= len(lines) {
		return lines
	}
	return lines[len(lines)-n:]
}

func decodeSnapshot(snap api.Snapshot) (*crux.Process, error) {
	var p crux.Process
	if err := json.Unmarshal(snap.Process, &p); err != nil {
		return nil, fmt.Errorf("decoding snapshot of %s: %w", snap.Agent, err)
	}
	if len(snap.Pane) > 0 {
		p.SetPaneCapture(snap.Pane, tail)
	}
	return &p, nil
}

func (r *Runtime) snapshot(name string, fast bool) (*crux.Process, error) {
	path := api.SnapshotPath(esc(name))
	if fast {
		path += "?" + api.QueryFast + "=1"
	}
	var snap api.Snapshot
	if err := r.call(http.MethodGet, path, nil, &snap); err != nil {
		return nil, err
	}
	return decodeSnapshot(snap)
}

func (r *Runtime) GetStatus(name string) (*crux.Process, error)     { return r.snapshot(name, false) }
func (r *Runtime) GetStatusFast(name string) (*crux.Process, error) { return r.snapshot(name, true) }

func (r *Runtime) AllStatuses() map[string]*crux.Process {
	var list api.SnapshotList
	if err := r.call(http.MethodGet, api.PathSnapshots, nil, &list); err != nil {
		r.drop("AllStatuses", err)
		return nil
	}
	out := make(map[string]*crux.Process, len(list.Snapshots))
	for _, snap := range list.Snapshots {
		p, err := decodeSnapshot(snap)
		if err != nil {
			r.drop("AllStatuses", err, "agent", snap.Agent)
			continue
		}
		out[snap.Agent] = p
	}
	return out
}

func (r *Runtime) ResolveAgent(nameOrID string) string {
	var p api.Process
	if err := r.call(http.MethodGet, api.AgentPath(esc(nameOrID)), nil, &p); err != nil || p.Name == "" {
		return nameOrID
	}
	return p.Name
}

func (r *Runtime) logs(name, source string, lines int) (api.Logs, error) {
	ctx, cancel := r.ctx()
	defer cancel()
	return r.c.Logs(ctx, name, source, lines)
}

func (r *Runtime) GetOutput(name string, lines int) ([]string, error) {
	out, err := r.logs(name, api.LogSourcePane, lines)
	return out.Lines, err
}

func (r *Runtime) GetBufferOutput(name string, lines int) ([]string, error) {
	out, err := r.logs(name, api.LogSourceBuffer, lines)
	return out.Lines, err
}

func (r *Runtime) CaptureFullLog(name string) (string, error) {
	out, err := r.logs(name, api.LogSourceFull, 0)
	return out.Content, err
}

// FullLogHeader reads the header off the full-log route; the content it
// carries alongside is discarded.
func (r *Runtime) FullLogHeader(name string) string {
	out, err := r.logs(name, api.LogSourceFull, 0)
	if err != nil {
		return ""
	}
	return out.Header
}

// ---- Kicker ----

func (r *Runtime) SendKick(name string, message string) error {
	return r.call(http.MethodPost, api.KickPath(esc(name)), api.KickRequest{Message: message}, nil)
}

func (r *Runtime) SendKickWithSource(name string, message string, source string) error {
	return r.call(http.MethodPost, api.KickPath(esc(name)), api.KickRequest{Message: message, Source: source}, nil)
}

func (r *Runtime) SendKickAsync(name string, message string) (bool, error) {
	var resp api.AgentResponse
	if err := r.call(http.MethodPost, api.KickPath(esc(name)), api.KickRequest{Message: message, Async: true}, &resp); err != nil {
		return false, err
	}
	return resp.Started != nil && *resp.Started, nil
}

func (r *Runtime) ValidateKick(name string) error {
	return r.call(http.MethodPost, api.KickValidatePath(esc(name)), nil, nil)
}

func (r *Runtime) KickDispatchState(name string) (crux.KickDispatch, bool) {
	var resp api.KickDispatchResponse
	if err := r.call(http.MethodGet, api.KickPath(esc(name)), nil, &resp); err != nil || resp.Dispatch == nil {
		return crux.KickDispatch{}, false
	}
	d := resp.Dispatch
	return crux.KickDispatch{Agent: d.Agent, Phase: d.Phase, Error: d.Error, QueuedAt: d.QueuedAt, SettledAt: d.SettledAt}, true
}

func (r *Runtime) ListKickLogs(name string) ([]crux.KickLogInfo, error) {
	var list api.KickLogList
	if err := r.call(http.MethodGet, api.KickLogsPath(esc(name)), nil, &list); err != nil {
		return nil, err
	}
	out := make([]crux.KickLogInfo, 0, len(list.Logs))
	for _, l := range list.Logs {
		out = append(out, crux.KickLogInfo{ID: l.ID, Timestamp: l.Timestamp, SizeBytes: l.SizeBytes, Reason: l.Reason, Source: l.Source})
	}
	return out, nil
}

func (r *Runtime) ReadKickLog(name, id string) (string, error) {
	var log api.KickLog
	if err := r.call(http.MethodGet, api.KickLogPath(esc(name), esc(id)), nil, &log); err != nil {
		return "", err
	}
	return log.Content, nil
}

// SetKickObserver subscribes to the server's event stream and relays the
// kick relay events (api.EventKick) to fn; nil unsubscribes. The stream
// reconnects after a drop until unsubscribed. SetEventObserver shares the
// same stream.
func (r *Runtime) SetKickObserver(fn func(agentName, event, detail string)) {
	r.mu.Lock()
	r.kickObserver = fn
	r.mu.Unlock()
	r.syncStream()
}

// SetEventObserver delivers every event on the stream to fn (nil stops):
// the Host observer events (api.EventKickOutcome, api.EventPauseTransition,
// ...) a sidecar host would otherwise never see.
func (r *Runtime) SetEventObserver(fn func(api.Event)) {
	r.mu.Lock()
	r.eventObserver = fn
	r.mu.Unlock()
	r.syncStream()
}

// syncStream starts the event stream when an observer is set and none runs,
// and stops it when the last observer is cleared.
func (r *Runtime) syncStream() {
	r.mu.Lock()
	want := r.kickObserver != nil || r.eventObserver != nil
	running := r.cancelEvents != nil
	var cancel context.CancelFunc
	var done chan struct{}
	if want && !running {
		ctx, c := context.WithCancel(context.Background())
		r.cancelEvents = c
		r.eventsDone = make(chan struct{})
		go r.streamEvents(ctx, r.eventsDone)
	}
	if !want && running {
		cancel, done = r.cancelEvents, r.eventsDone
		r.cancelEvents, r.eventsDone = nil, nil
	}
	r.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (r *Runtime) streamEvents(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		err := r.c.Events(ctx, func(ev api.Event) error {
			r.dispatch(ev)
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.logger().Warn("crux client: event stream dropped; reconnecting", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.retry):
		}
	}
}

func (r *Runtime) dispatch(ev api.Event) {
	r.mu.Lock()
	kick, all := r.kickObserver, r.eventObserver
	r.mu.Unlock()
	if ev.Type == api.EventKick && kick != nil {
		kick(ev.Agent, ev.State, ev.Detail)
	}
	if all != nil {
		all(ev)
	}
}

// ---- Definitions ----

func (r *Runtime) AddAgent(name string, cfg config.AgentConfig) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		r.drop("AddAgent", err, "agent", name)
		return
	}
	if err := r.call(http.MethodPost, api.PathDefinitions, api.DefinitionRequest{Name: name, Definition: raw}, nil); err != nil {
		r.drop("AddAgent", err, "agent", name)
	}
}

func (r *Runtime) RemoveAgent(name string) {
	if err := r.call(http.MethodDelete, api.AgentPath(esc(name)), nil, nil); err != nil && !api.IsNotFound(err) {
		r.drop("RemoveAgent", err, "agent", name)
	}
}

func (r *Runtime) UpdateConfig(name string, cfg config.AgentConfig) error {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return r.call(http.MethodPut, api.DefinitionPath(esc(name)), api.DefinitionRequest{Name: name, Definition: raw}, nil)
}

func (r *Runtime) ReconcileAgents(configs map[string]config.AgentConfig) []string {
	req := api.ReconcileRequest{Definitions: make(map[string]json.RawMessage, len(configs))}
	for name, cfg := range configs {
		raw, err := json.Marshal(cfg)
		if err != nil {
			r.drop("ReconcileAgents", err, "agent", name)
			return nil
		}
		req.Definitions[name] = raw
	}
	var out api.Names
	if err := r.call(http.MethodPost, api.PathReconcile, req, &out); err != nil {
		r.drop("ReconcileAgents", err)
		return nil
	}
	return out.Names
}

func (r *Runtime) SetModelOverride(name, model string) error {
	return r.call(http.MethodPut, api.OverridesPath(esc(name)), api.OverridesRequest{Model: &model}, nil)
}

func (r *Runtime) SetBackendOverride(name, backend string) error {
	return r.call(http.MethodPut, api.OverridesPath(esc(name)), api.OverridesRequest{Backend: &backend}, nil)
}

func (r *Runtime) PinCLI(name, version string) error {
	return r.call(http.MethodPut, api.PinPath(esc(name), api.PinCLI), api.PinRequest{Value: version}, nil)
}

func (r *Runtime) PinModel(name, model string) error {
	return r.call(http.MethodPut, api.PinPath(esc(name), api.PinModel), api.PinRequest{Value: model}, nil)
}

func (r *Runtime) UnpinCLI(name string) error {
	return r.call(http.MethodDelete, api.PinPath(esc(name), api.PinCLI), nil, nil)
}

func (r *Runtime) UnpinModel(name string) error {
	return r.call(http.MethodDelete, api.PinPath(esc(name), api.PinModel), nil, nil)
}

func (r *Runtime) ClearModeOverrides(names []string) {
	if err := r.call(http.MethodPost, api.PathModesClear, api.Names{Names: names}, nil); err != nil {
		r.drop("ClearModeOverrides", err)
	}
}

func (r *Runtime) SyncModeFiles(level int) {
	if err := r.call(http.MethodPost, api.PathModesSync, api.Level{Level: level}, nil); err != nil {
		r.drop("SyncModeFiles", err)
	}
}

func (r *Runtime) RefreshInferenceRoutes(backend string) {
	if err := r.call(http.MethodPost, api.PathInference, api.Backend{Backend: backend}, nil); err != nil {
		r.drop("RefreshInferenceRoutes", err)
	}
}

// ---- Fleet ----

func (r *Runtime) fleet() (api.FleetInfo, error) {
	var info api.FleetInfo
	err := r.call(http.MethodGet, api.PathFleet, nil, &info)
	return info, err
}

func (r *Runtime) BreakerState() (bool, []string) {
	var b api.Breaker
	if err := r.call(http.MethodGet, api.PathBreaker, nil, &b); err != nil {
		r.drop("BreakerState", err)
		return false, nil
	}
	return b.Engaged, b.Paused
}

// names posts/gets a Names list under the caller's context.
func (r *Runtime) names(ctx context.Context, method, verb, path string) []string {
	var out api.Names
	if err := r.c.Do(ctx, verb, path, nil, &out); err != nil {
		r.drop(method, err)
		return nil
	}
	return out.Names
}

func (r *Runtime) EngageBreaker() []string {
	ctx, cancel := r.ctx()
	defer cancel()
	return r.names(ctx, "EngageBreaker", http.MethodPost, api.PathBreakerOn)
}

func (r *Runtime) ReleaseBreaker(ctx context.Context) []string {
	return r.names(ctx, "ReleaseBreaker", http.MethodPost, api.PathBreakerOff)
}

func (r *Runtime) SetACMMLevel(level int) {
	if err := r.call(http.MethodPut, api.PathACMM, api.Level{Level: level}, nil); err != nil {
		r.drop("SetACMMLevel", err)
	}
}

func (r *Runtime) GetACMMLevel() int {
	info, err := r.fleet()
	if err != nil {
		r.drop("GetACMMLevel", err)
	}
	return info.ACMMLevel
}

func (r *Runtime) RelaunchBobAgentsAwaitingKey(ctx context.Context) []string {
	return r.names(ctx, "RelaunchBobAgentsAwaitingKey", http.MethodPost, api.PathRelaunchBob)
}

func (r *Runtime) SharedDevUID() bool {
	info, err := r.fleet()
	if err != nil {
		r.drop("SharedDevUID", err)
	}
	return info.SharedDevUID
}

// ---- Credentials ----

func (r *Runtime) SubmitLoginCode(name, code string) error {
	return r.call(http.MethodPost, api.LoginCodePath(esc(name)), api.LoginCodeRequest{Code: code}, nil)
}

func (r *Runtime) copilot() api.CopilotCredential {
	var c api.CopilotCredential
	if err := r.call(http.MethodGet, api.PathCopilotCred, nil, &c); err != nil {
		r.drop("CopilotToken", err)
	}
	return c
}

func (r *Runtime) CopilotToken() string       { return r.copilot().Token }
func (r *Runtime) CopilotTokenSource() string { return r.copilot().Source }

func (r *Runtime) SetCopilotToken(token string) {
	if err := r.call(http.MethodPut, api.PathCopilotCred, api.CopilotCredential{Token: token}, nil); err != nil {
		r.drop("SetCopilotToken", err)
	}
}

func (r *Runtime) ActivateCopilotToken(token string) error {
	return r.call(http.MethodPut, api.PathCopilotCred, api.CopilotCredential{Token: token, Activate: true}, nil)
}

func (r *Runtime) ReloadClaudeToken() {
	if err := r.call(http.MethodPost, api.PathClaudeReload, nil, nil); err != nil {
		r.drop("ReloadClaudeToken", err)
	}
}

func (r *Runtime) ClaudeCredentialCandidatePaths() []string {
	var c api.ClaudeCredential
	if err := r.call(http.MethodGet, api.PathClaudeCred, nil, &c); err != nil {
		r.drop("ClaudeCredentialCandidatePaths", err)
		return nil
	}
	return c.CandidatePaths
}

func (r *Runtime) OmpAgentHomes() []string {
	var h api.OmpHomes
	if err := r.call(http.MethodGet, api.PathOmpHomes, nil, &h); err != nil {
		r.drop("OmpAgentHomes", err)
		return nil
	}
	return h.Homes
}

// ---- Resumer ----

func (r *Runtime) session(name string) (api.Session, bool) {
	var s api.Session
	if err := r.call(http.MethodGet, api.SessionPath(esc(name)), nil, &s); err != nil {
		if !api.IsNotFound(err) {
			r.drop("Session", err, "agent", name)
		}
		return api.Session{}, false
	}
	return s, true
}

func (r *Runtime) ResumeHandle(name string) (crux.ResumeHandle, bool) {
	s, ok := r.session(name)
	if !ok || s.Handle == nil {
		return crux.ResumeHandle{}, false
	}
	h := s.Handle
	return crux.ResumeHandle{Backend: h.Backend, SessionID: h.SessionID, Transcript: h.Transcript, Command: h.Command, LastActive: h.LastActive, CapturedAt: h.CapturedAt}, true
}

// SendResumeKick maps the server's resume codes back onto the crux
// sentinels so errors.Is works for a sidecar host as it does in-process.
func (r *Runtime) SendResumeKick(name, message, sessionID string) error {
	err := r.call(http.MethodPost, api.SessionKickPath(esc(name)), api.ResumeKickRequest{Message: message, SessionID: sessionID}, nil)
	var se *api.StatusError
	if errors.As(err, &se) {
		switch se.Body.Code {
		case api.CodeResumeBusy:
			return fmt.Errorf("%w: %s", crux.ErrResumeBusy, se.Body.Error)
		case api.CodeResumeSessionGone:
			return fmt.Errorf("%w: %s", crux.ErrResumeSessionGone, se.Body.Error)
		case api.CodeResumeUnsupported:
			return fmt.Errorf("%w: %s", crux.ErrResumeUnsupported, se.Body.Error)
		}
	}
	return err
}

func (r *Runtime) SessionID(name string) (string, bool) {
	s, ok := r.session(name)
	if !ok || !s.HasSession {
		return "", false
	}
	return s.SessionID, true
}

func (r *Runtime) SessionMissing(name string) bool {
	s, _ := r.session(name)
	return s.Missing
}

// ---- Supervisor ----

func (r *Runtime) supervision(name string) (api.Supervision, bool) {
	var s api.Supervision
	if err := r.call(http.MethodGet, api.SupervisionPath(esc(name)), nil, &s); err != nil {
		if !api.IsNotFound(err) {
			r.drop("Supervision", err, "agent", name)
		}
		return api.Supervision{}, false
	}
	return s, true
}

func (r *Runtime) GovernorMayStart(name string) bool {
	s, _ := r.supervision(name)
	return s.MayStart
}

func (r *Runtime) MarkStartupLaunchQueued(names []string) {
	if err := r.call(http.MethodPost, api.PathStartupQueued, api.Names{Names: names}, nil); err != nil {
		r.drop("MarkStartupLaunchQueued", err)
	}
}

func (r *Runtime) CheckAndRestartCrashedAgents(ctx context.Context) []string {
	return r.names(ctx, "CheckAndRestartCrashedAgents", http.MethodPost, api.PathRestartCrashed)
}

func (r *Runtime) ConsentWedgedAgents() []string {
	info, err := r.fleet()
	if err != nil {
		r.drop("ConsentWedgedAgents", err)
	}
	return info.ConsentWedged
}

func (r *Runtime) RefreshAgentTokens(ctx context.Context) {
	if err := r.callCtx(ctx, http.MethodPost, api.PathTokensRefresh, nil, nil); err != nil {
		r.drop("RefreshAgentTokens", err)
	}
}

func (r *Runtime) AgentCredentialRefreshable(agentName string) (bool, bool) {
	s, ok := r.supervision(agentName)
	return s.CredentialRefreshable, ok && s.CredentialKnown
}

func (r *Runtime) AgentCapabilities(agentName string) (bool, bool, bool, bool) {
	s, ok := r.supervision(agentName)
	if !ok || s.Capabilities == nil {
		return false, false, false, false
	}
	return s.Capabilities.CanOpenIssue, s.Capabilities.CanOpenPR, s.Capabilities.CanMerge, true
}

func (r *Runtime) EffectiveBackend(agentName string) (string, bool) {
	s, ok := r.supervision(agentName)
	if !ok || s.Backend == nil {
		return "", false
	}
	return *s.Backend, true
}

func (r *Runtime) InvocationMetadata(agentName string) (string, string, string, bool) {
	s, ok := r.supervision(agentName)
	if !ok || s.Invocation == nil {
		return "", "", "", false
	}
	return s.Invocation.Backend, s.Invocation.Model, s.Invocation.Effort, true
}

func (r *Runtime) RestartTelemetry(name string) (int, int, time.Time, string, bool) {
	s, ok := r.supervision(name)
	if !ok || s.Restarts == nil {
		return 0, 0, time.Time{}, "", false
	}
	t := s.Restarts
	return t.Total, t.Last24h, t.LastRestartAt, t.LastReason, true
}

func (r *Runtime) StartFailureState(name string) (crux.StartFailureSnapshot, bool) {
	s, ok := r.supervision(name)
	if !ok || s.StartFailure == nil {
		return crux.StartFailureSnapshot{}, false
	}
	f := s.StartFailure
	return crux.StartFailureSnapshot{Reason: f.Reason, Count: f.Count, LastAt: f.LastAt, LastExitCode: f.LastExitCode, LastSignal: f.LastSignal, Blocked: f.Blocked}, true
}

func (r *Runtime) ProviderErrorBackoffRemaining(name string) (time.Duration, string, string, bool) {
	s, ok := r.supervision(name)
	if !ok || s.ProviderBackoff == nil {
		return 0, "", "", false
	}
	b := s.ProviderBackoff
	return time.Duration(b.RemainingMS) * time.Millisecond, b.Provider, b.Reason, true
}

func (r *Runtime) ArchiveAllKickLogs(reason string) {
	if err := r.call(http.MethodPost, api.PathKickLogsArchive, api.Reason{Reason: reason}, nil); err != nil {
		r.drop("ArchiveAllKickLogs", err)
	}
}

func (r *Runtime) CountAgentsWithModel() int {
	info, err := r.fleet()
	if err != nil {
		r.drop("CountAgentsWithModel", err)
	}
	return info.AgentsWithModel
}

func (r *Runtime) CrashOOMSuspected(name string) bool {
	s, _ := r.supervision(name)
	return s.CrashOOMSuspected
}

func (r *Runtime) BackendAuthAvailable(backend string) (bool, bool) {
	var a api.BackendAuth
	if err := r.call(http.MethodGet, api.BackendAuthPath(esc(backend)), nil, &a); err != nil {
		r.drop("BackendAuthAvailable", err, "backend", backend)
		return false, false
	}
	return a.Available, a.Known
}

func (r *Runtime) AgentAuthAvailable(agentName string) (bool, bool) {
	s, ok := r.supervision(agentName)
	return s.AuthAvailable, ok && s.AuthKnown
}

func (r *Runtime) SendGitHubAPIBudgetNudge(name, message string) {
	if err := r.call(http.MethodPost, api.NudgePath(esc(name)), api.NudgeRequest{Message: message}, nil); err != nil {
		r.drop("SendGitHubAPIBudgetNudge", err, "agent", name)
	}
}

func (r *Runtime) RecordAudit(action, agentName string, fields map[string]any) {
	if err := r.call(http.MethodPost, api.PathAudit, api.AuditRequest{Action: action, Agent: agentName, Fields: fields}, nil); err != nil {
		r.drop("RecordAudit", err, "action", action)
	}
}

func (r *Runtime) AgentHasValidCredential(agentName string) bool {
	s, _ := r.supervision(agentName)
	return s.HasValidCredential
}

func (r *Runtime) RefreshAgentTokenFor(ctx context.Context, name string) error {
	return r.callCtx(ctx, http.MethodPost, api.AgentTokenRefreshPath(esc(name)), nil, nil)
}

func (r *Runtime) TrajectoryAgents() []trajectory.AgentView {
	var list api.TrajectoryList
	if err := r.call(http.MethodGet, api.PathTrajectory, nil, &list); err != nil {
		r.drop("TrajectoryAgents", err)
		return nil
	}
	out := make([]trajectory.AgentView, 0, len(list.Agents))
	for _, a := range list.Agents {
		out = append(out, trajectory.AgentView{Name: a.Name, Running: a.Running, Intent: a.Intent, Transcript: a.Transcript, Idle: a.Idle, LastOutputAt: a.LastOutputAt})
	}
	return out
}

// ---- Restorer ----

func (r *Runtime) seed(method, name string, req api.SeedRequest) {
	if err := r.call(http.MethodPost, api.SeedPath(esc(name)), req, nil); err != nil {
		r.drop(method, err, "agent", name)
	}
}

func (r *Runtime) SeedPauseState(name string, pausedAt time.Time, trigger, reason, by string) {
	r.seed("SeedPauseState", name, api.SeedRequest{Pause: &api.PauseSeed{PausedAt: pausedAt, Trigger: trigger, Reason: reason, By: by}})
}

func (r *Runtime) SeedRestartTelemetry(name string, count int, events []crux.RestartEvent, lastReason string) {
	wire := make([]api.RestartEvent, 0, len(events))
	for _, e := range events {
		wire = append(wire, api.RestartEvent{At: e.At, Reason: e.Reason})
	}
	r.seed("SeedRestartTelemetry", name, api.SeedRequest{Restarts: &api.RestartSeed{Count: count, Events: wire, LastReason: lastReason}})
}

func (r *Runtime) SeedTurnLoss(name string, loss crux.TurnLoss) {
	raw, err := json.Marshal(loss)
	if err != nil {
		r.drop("SeedTurnLoss", err, "agent", name)
		return
	}
	r.seed("SeedTurnLoss", name, api.SeedRequest{TurnLoss: raw})
}

func (r *Runtime) SeedLastKick(name string, t time.Time) {
	r.seed("SeedLastKick", name, api.SeedRequest{LastKick: &t})
}

func (r *Runtime) SeedKickHistory(name string, records []crux.KickRecord) {
	wire := make([]api.KickRecord, 0, len(records))
	for _, k := range records {
		wire = append(wire, api.KickRecord{Timestamp: k.Timestamp, Agent: k.Agent, Snippet: k.Snippet})
	}
	r.seed("SeedKickHistory", name, api.SeedRequest{KickHistory: &wire})
}

func (r *Runtime) RestoreBreaker(engaged bool, paused []string) {
	if err := r.call(http.MethodPut, api.PathBreaker, api.Breaker{Engaged: engaged, Paused: paused}, nil); err != nil {
		r.drop("RestoreBreaker", err)
	}
}

// ---- Background (server-side; no-ops here) ----

func (r *Runtime) StartAgentTokenRefresh(context.Context)         {}
func (r *Runtime) StartCredentialWatchdog(context.Context)        {}
func (r *Runtime) StartCopilotSessionRefresh(context.Context)     {}
func (r *Runtime) StartTranscriptActivityRefresh(context.Context) {}
func (r *Runtime) StartRepeatFailureMonitor(context.Context)      {}

// ---- Authorizer (fail closed) ----

// authorize returns nil only for a 204. A 403 carries the runtime's reason
// as a plain error, the way the in-process Authorizer reports it; any other
// failure, including the server being unreachable, is a denial too.
func (r *Runtime) authorize(req api.AuthorizeRequest) error {
	err := r.call(http.MethodPost, api.PathAuthorize, req, nil)
	if err == nil {
		return nil
	}
	var se *api.StatusError
	if errors.As(err, &se) && se.Status == http.StatusForbidden && se.Body.Error != "" {
		return errors.New(se.Body.Error)
	}
	return fmt.Errorf("authorize %s for %s: denied: %w", req.Action, req.Agent, err)
}

func (r *Runtime) AuthorizePROpen(agentName string, fileUID int) error {
	return r.authorize(api.AuthorizeRequest{Action: api.AuthorizePROpen, Agent: agentName, FileUID: fileUID})
}

func (r *Runtime) AuthorizePushBranch(agentName string, fileUID int) error {
	return r.authorize(api.AuthorizeRequest{Action: api.AuthorizePushBranch, Agent: agentName, FileUID: fileUID})
}

func (r *Runtime) AuthorizeIssueOpen(agentName string, fileUID int, kind string) error {
	return r.authorize(api.AuthorizeRequest{Action: api.AuthorizeIssueOpen, Agent: agentName, FileUID: fileUID, Kind: kind})
}

func (r *Runtime) AuthorizeMerge(agentName string, fileUID int, repo string) error {
	return r.authorize(api.AuthorizeRequest{Action: api.AuthorizeMerge, Agent: agentName, FileUID: fileUID, Repo: repo})
}

func (r *Runtime) AuthorizeReviewRequest(agentName string, fileUID int) error {
	return r.authorize(api.AuthorizeRequest{Action: api.AuthorizeReviewRequest, Agent: agentName, FileUID: fileUID})
}

var _ crux.Runtime = (*Runtime)(nil)

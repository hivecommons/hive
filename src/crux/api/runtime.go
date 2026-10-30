package api

import (
	"encoding/json"
	"time"
)

// This file is the wire contract for the rest of crux.Runtime
// (hivecommons/hive#14138, stage 3): every family the CLI verbs do not cover
// (Lifecycle beyond run/stop, the full Observer snapshot, Kicker's async and
// archive reads, Definitions, Fleet, Credentials, Resumer, Supervisor,
// Restorer, Background, Authorizer) plus the watchdog's observe/restart seam.
// It stays standard-library only like the rest of the package: payloads the
// runtime owns the shape of (a crux.Process snapshot, a config.AgentConfig
// definition, a crux.TurnLoss record) cross as json.RawMessage in the
// runtime's own JSON form, the way RunRequest.Definition already does; small
// values are mirrored here field for field so the wire shape is pinned
// independently of the runtime's types.
//
// RuntimeRoutes is the parity table: every crux.Runtime method names the
// route that serves it. crux/api/client's tests assert the table and the
// interface agree in both directions; crux/serve's tests assert every route
// in the table is registered.

// Routes beyond the CLI-shaped ones in api.go. Agent routes take the agent's
// name or ID in place of {name}.
const (
	PathSnapshots   = "/" + Version + "/snapshots"
	PathDefinitions = "/" + Version + "/definitions"
	PathReconcile   = PathDefinitions + "/reconcile"
	PathFleet       = "/" + Version + "/fleet"
	PathBreaker     = PathFleet + "/breaker"
	PathBreakerOn   = PathBreaker + "/engage"
	PathBreakerOff  = PathBreaker + "/release"
	PathACMM        = PathFleet + "/acmm"
	PathModesSync   = PathFleet + "/modes/sync"
	PathModesClear  = PathFleet + "/modes/clear"
	PathInference   = PathFleet + "/inference/refresh"
	PathRelaunchBob = PathFleet + "/relaunch-bob"
	// PathStartupQueued records the host's staggered boot queue.
	PathStartupQueued = PathFleet + "/startup-queued"
	// PathRestartCrashed relaunches crashed agents.
	PathRestartCrashed = PathFleet + "/restart-crashed"
	// PathTokensRefresh rewrites every agent's scoped token.
	PathTokensRefresh = PathFleet + "/tokens/refresh"
	// PathKickLogsArchive rotates every agent's kick log.
	PathKickLogsArchive = PathFleet + "/kicklogs/archive"
	PathCopilotCred     = "/" + Version + "/credentials/copilot"
	PathClaudeCred      = "/" + Version + "/credentials/claude"
	PathClaudeReload    = PathClaudeCred + "/reload"
	PathOmpHomes        = "/" + Version + "/credentials/omp/homes"
	PathBackends        = "/" + Version + "/backends"
	PathAudit           = "/" + Version + "/audit"
	PathTrajectory      = "/" + Version + "/trajectory"
	PathAuthorize       = "/" + Version + "/authorize"
	PathWatchdogAgents  = "/" + Version + "/watchdog/agents"
)

// Per-agent routes.
func StartPath(name string) string              { return AgentPath(name) + "/start" }
func RestartPath(name string) string            { return AgentPath(name) + "/restart" }
func PausePath(name string) string              { return AgentPath(name) + "/pause" }
func ResumePath(name string) string             { return AgentPath(name) + "/resume" }
func RestartCountPath(name string) string       { return AgentPath(name) + "/restart-count" }
func SnapshotPath(name string) string           { return AgentPath(name) + "/snapshot" }
func KickValidatePath(name string) string       { return KickPath(name) + "/validate" }
func KickLogsPath(name string) string           { return AgentPath(name) + "/kicklogs" }
func KickLogPath(name, id string) string        { return KickLogsPath(name) + "/" + id }
func DefinitionPath(name string) string         { return AgentPath(name) + "/definition" }
func OverridesPath(name string) string          { return AgentPath(name) + "/overrides" }
func PinPath(name, kind string) string          { return AgentPath(name) + "/pins/" + kind }
func LoginCodePath(name string) string          { return AgentPath(name) + "/login-code" }
func SessionPath(name string) string            { return AgentPath(name) + "/session" }
func SessionKickPath(name string) string        { return SessionPath(name) + "/kick" }
func SupervisionPath(name string) string        { return AgentPath(name) + "/supervision" }
func AgentTokenRefreshPath(name string) string  { return AgentPath(name) + "/tokens/refresh" }
func NudgePath(name string) string              { return AgentPath(name) + "/nudge" }
func SeedPath(name string) string               { return AgentPath(name) + "/seed" }
func WatchdogPath(name string) string           { return AgentPath(name) + "/watchdog" }
func WatchdogConditionsPath(name string) string { return WatchdogPath(name) + "/conditions" }

// BackendAuthPath is the route reporting whether a backend has credentials.
func BackendAuthPath(backend string) string { return PathBackends + "/" + backend + "/auth" }

// Pin kinds, the {kind} of the pins route.
const (
	PinCLI   = "cli"
	PinModel = "model"
)

// Query parameters of the snapshot route.
const (
	// QueryFast asks for the snapshot without a pane re-read
	// (Observer.GetStatusFast).
	QueryFast = "fast"
)

// Snapshot is the body of GET /v1/agents/{name}/snapshot: the runtime's full
// crux.Process for one agent in its own JSON form (the compact api.Process
// is what GET /v1/agents/{name} returns), plus the pane capture the snapshot
// was taken with, which crux.Process keeps unexported. Process never carries
// the output ring buffer; the logs route serves it.
type Snapshot struct {
	Agent   string          `json:"agent"`
	Process json.RawMessage `json:"process"`
	Pane    []string        `json:"pane,omitempty"`
}

// SnapshotList is the body of GET /v1/snapshots (Observer.AllStatuses),
// sorted by agent.
type SnapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// RestartRequest is the body of POST /v1/agents/{name}/restart. A non-empty
// Bootstrap selects Lifecycle.RestartWithBootstrap.
type RestartRequest struct {
	Bootstrap string `json:"bootstrap,omitempty"`
}

// PauseRequest is the body of POST /v1/agents/{name}/pause. By selects
// Lifecycle.PauseBy; Cause selects Supervisor.PauseByCause (By may then be
// empty, as the runtime allows).
type PauseRequest struct {
	Trigger string          `json:"trigger"`
	Reason  string          `json:"reason"`
	By      string          `json:"by,omitempty"`
	Cause   *PauseCausation `json:"cause,omitempty"`
}

// PauseCausation mirrors crux.PauseCausation.
type PauseCausation struct {
	Depth            int    `json:"depth"`
	HookName         string `json:"hook_name,omitempty"`
	OriginTransition string `json:"origin_transition,omitempty"`
}

// ResumeRequest is the body of POST /v1/agents/{name}/resume.
type ResumeRequest struct {
	Trigger string `json:"trigger"`
	Reason  string `json:"reason"`
}

// KickDispatch mirrors crux.KickDispatch.
type KickDispatch struct {
	Agent     string    `json:"agent"`
	Phase     string    `json:"phase"`
	Error     string    `json:"error,omitempty"`
	QueuedAt  time.Time `json:"queuedAt"`
	SettledAt time.Time `json:"settledAt,omitempty"`
}

// KickDispatchResponse is the body of GET /v1/agents/{name}/kick
// (Kicker.KickDispatchState); Dispatch is nil when the agent has none.
type KickDispatchResponse struct {
	Agent    string        `json:"agent"`
	Dispatch *KickDispatch `json:"dispatch,omitempty"`
}

// KickLogInfo mirrors crux.KickLogInfo.
type KickLogInfo struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	SizeBytes int64     `json:"size_bytes"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source,omitempty"`
}

// KickLogList is the body of GET /v1/agents/{name}/kicklogs.
type KickLogList struct {
	Agent string        `json:"agent"`
	Logs  []KickLogInfo `json:"logs"`
}

// KickLog is the body of GET /v1/agents/{name}/kicklogs/{id}.
type KickLog struct {
	Agent   string `json:"agent"`
	ID      string `json:"id"`
	Content string `json:"content"`
}

// DefinitionRequest is the body of POST /v1/definitions
// (Definitions.AddAgent). Definition is the JSON form of config.AgentConfig
// and is registered verbatim: unlike POST /v1/agents, no defaults are filled,
// because the caller is a host handing over a complete definition. The
// response is 201 when the name was new and 200 when the runtime already
// held it (AddAgent is a no-op then). PUT /v1/agents/{name}/definition
// (Definitions.UpdateConfig) takes the same body with Name ignored.
type DefinitionRequest struct {
	Name       string          `json:"name"`
	Definition json.RawMessage `json:"definition"`
}

// ReconcileRequest is the body of POST /v1/definitions/reconcile
// (Definitions.ReconcileAgents): the host's full definition set, each in
// the JSON form of config.AgentConfig.
type ReconcileRequest struct {
	Definitions map[string]json.RawMessage `json:"definitions"`
}

// Names is the body of responses that list agent names (reconcile, breaker
// release, relaunch, restart-crashed, watchdog agents) and of requests that
// take one (modes clear, startup queued).
type Names struct {
	Names []string `json:"names"`
}

// OverridesRequest is the body of PUT /v1/agents/{name}/overrides. Each
// pointer that is set is applied (Definitions.SetModelOverride /
// SetBackendOverride); nil leaves the override alone. An empty string clears
// it, as the runtime does.
type OverridesRequest struct {
	Model   *string `json:"model,omitempty"`
	Backend *string `json:"backend,omitempty"`
}

// PinRequest is the body of PUT /v1/agents/{name}/pins/{kind}
// (Definitions.PinCLI / PinModel); DELETE on the same route unpins.
type PinRequest struct {
	Value string `json:"value"`
}

// Level is the body of PUT /v1/fleet/acmm (Fleet.SetACMMLevel) and POST
// /v1/fleet/modes/sync (Definitions.SyncModeFiles).
type Level struct {
	Level int `json:"level"`
}

// Backend is the body of POST /v1/fleet/inference/refresh
// (Definitions.RefreshInferenceRoutes).
type Backend struct {
	Backend string `json:"backend"`
}

// Breaker is the fleet breaker as GET /v1/fleet/breaker reports it
// (Fleet.BreakerState), as POST /v1/fleet/breaker/engage returns it, and as
// PUT /v1/fleet/breaker restores it (Restorer.RestoreBreaker).
type Breaker struct {
	Engaged bool     `json:"engaged"`
	Paused  []string `json:"paused"`
}

// FleetInfo is the body of GET /v1/fleet: the fleet-wide reads that have no
// agent (Fleet.GetACMMLevel, Fleet.SharedDevUID,
// Supervisor.ConsentWedgedAgents, Supervisor.CountAgentsWithModel).
type FleetInfo struct {
	ACMMLevel       int      `json:"acmm_level"`
	SharedDevUID    bool     `json:"shared_dev_uid"`
	AgentsWithModel int      `json:"agents_with_model"`
	ConsentWedged   []string `json:"consent_wedged"`
}

// Reason is the body of POST /v1/fleet/kicklogs/archive
// (Supervisor.ArchiveAllKickLogs).
type Reason struct {
	Reason string `json:"reason"`
}

// LoginCodeRequest is the body of POST /v1/agents/{name}/login-code
// (Credentials.SubmitLoginCode).
type LoginCodeRequest struct {
	Code string `json:"code"`
}

// CopilotCredential is the body of GET /v1/credentials/copilot
// (Credentials.CopilotToken, CopilotTokenSource) and of PUT on the same
// route: Activate false is Credentials.SetCopilotToken, true is
// ActivateCopilotToken. Source is read-only.
type CopilotCredential struct {
	Token    string `json:"token"`
	Source   string `json:"source,omitempty"`
	Activate bool   `json:"activate,omitempty"`
}

// ClaudeCredential is the body of GET /v1/credentials/claude
// (Credentials.ClaudeCredentialCandidatePaths).
type ClaudeCredential struct {
	CandidatePaths []string `json:"candidate_paths"`
}

// OmpHomes is the body of GET /v1/credentials/omp/homes
// (Credentials.OmpAgentHomes).
type OmpHomes struct {
	Homes []string `json:"homes"`
}

// ResumeHandle mirrors crux.ResumeHandle.
type ResumeHandle struct {
	Backend    string    `json:"backend"`
	SessionID  string    `json:"session_id"`
	Transcript string    `json:"transcript,omitempty"`
	Command    string    `json:"command,omitempty"`
	LastActive time.Time `json:"last_active"`
	CapturedAt time.Time `json:"captured_at"`
}

// Session is the body of GET /v1/agents/{name}/session: the Resumer reads
// (SessionID, SessionMissing, ResumeHandle) in one object. Handle is nil
// when the agent has no resumable conversation.
type Session struct {
	Agent      string        `json:"agent"`
	SessionID  string        `json:"session_id,omitempty"`
	HasSession bool          `json:"has_session"`
	Missing    bool          `json:"missing"`
	Handle     *ResumeHandle `json:"handle,omitempty"`
}

// ResumeKickRequest is the body of POST /v1/agents/{name}/session/kick
// (Resumer.SendResumeKick). The response distinguishes the runtime's resume
// sentinels by status: 409 CodeResumeBusy, 410 CodeResumeSessionGone, 422
// CodeResumeUnsupported.
type ResumeKickRequest struct {
	Message   string `json:"message"`
	SessionID string `json:"session_id"`
}

// StartFailureSnapshot mirrors crux.StartFailureSnapshot.
type StartFailureSnapshot struct {
	Reason       string    `json:"reason"`
	Count        int       `json:"count"`
	LastAt       time.Time `json:"last_at"`
	LastExitCode *int      `json:"last_exit_code,omitempty"`
	LastSignal   string    `json:"last_signal,omitempty"`
	Blocked      bool      `json:"blocked"`
}

// RestartTelemetry mirrors Supervisor.RestartTelemetry's results.
type RestartTelemetry struct {
	Total         int       `json:"total"`
	Last24h       int       `json:"last_24h"`
	LastRestartAt time.Time `json:"last_restart_at"`
	LastReason    string    `json:"last_reason,omitempty"`
}

// ProviderBackoff mirrors Supervisor.ProviderErrorBackoffRemaining's
// results; RemainingMS is milliseconds.
type ProviderBackoff struct {
	RemainingMS int64  `json:"remaining_ms"`
	Provider    string `json:"provider,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// Supervision is the body of GET /v1/agents/{name}/supervision: every
// per-agent Supervisor read in one object, so the host's heartbeat takes one
// round trip per agent. Each optional field is nil when the runtime's ok
// result was false.
type Supervision struct {
	Agent string `json:"agent"`
	// MayStart is Supervisor.GovernorMayStart.
	MayStart bool `json:"may_start"`
	// CredentialRefreshable / CredentialKnown are AgentCredentialRefreshable.
	CredentialRefreshable bool `json:"credential_refreshable"`
	CredentialKnown       bool `json:"credential_known"`
	// HasValidCredential is AgentHasValidCredential.
	HasValidCredential bool `json:"has_valid_credential"`
	// Capabilities is AgentCapabilities; nil when ok was false.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	// Backend is EffectiveBackend; nil when ok was false.
	Backend *string `json:"backend,omitempty"`
	// Invocation is InvocationMetadata; nil when ok was false.
	Invocation *Invocation `json:"invocation,omitempty"`
	// Restarts is RestartTelemetry; nil when ok was false.
	Restarts *RestartTelemetry `json:"restarts,omitempty"`
	// StartFailure is StartFailureState; nil when none.
	StartFailure *StartFailureSnapshot `json:"start_failure,omitempty"`
	// ProviderBackoff is ProviderErrorBackoffRemaining; nil when none.
	ProviderBackoff *ProviderBackoff `json:"provider_backoff,omitempty"`
	// CrashOOMSuspected is CrashOOMSuspected.
	CrashOOMSuspected bool `json:"crash_oom_suspected"`
	// AuthAvailable / AuthKnown are AgentAuthAvailable.
	AuthAvailable bool `json:"auth_available"`
	AuthKnown     bool `json:"auth_known"`
}

// Capabilities mirrors Supervisor.AgentCapabilities' bits.
type Capabilities struct {
	CanOpenIssue bool `json:"can_open_issue"`
	CanOpenPR    bool `json:"can_open_pr"`
	CanMerge     bool `json:"can_merge"`
}

// Invocation mirrors Supervisor.InvocationMetadata's results.
type Invocation struct {
	Backend string `json:"backend"`
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
}

// BackendAuth is the body of GET /v1/backends/{backend}/auth
// (Supervisor.BackendAuthAvailable).
type BackendAuth struct {
	Backend   string `json:"backend"`
	Available bool   `json:"available"`
	Known     bool   `json:"known"`
}

// NudgeRequest is the body of POST /v1/agents/{name}/nudge
// (Supervisor.SendGitHubAPIBudgetNudge).
type NudgeRequest struct {
	Message string `json:"message"`
}

// AuditRequest is the body of POST /v1/audit (Supervisor.RecordAudit).
type AuditRequest struct {
	Action string         `json:"action"`
	Agent  string         `json:"agent,omitempty"`
	Fields map[string]any `json:"fields,omitempty"`
}

// TrajectoryAgent mirrors trajectory.AgentView.
type TrajectoryAgent struct {
	Name         string    `json:"name"`
	Running      bool      `json:"running"`
	Intent       string    `json:"intent,omitempty"`
	Transcript   string    `json:"transcript,omitempty"`
	Idle         bool      `json:"idle"`
	LastOutputAt time.Time `json:"last_output_at"`
}

// TrajectoryList is the body of GET /v1/trajectory
// (Supervisor.TrajectoryAgents).
type TrajectoryList struct {
	Agents []TrajectoryAgent `json:"agents"`
}

// RestartEvent mirrors crux.RestartEvent.
type RestartEvent struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
}

// KickRecord mirrors crux.KickRecord.
type KickRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Agent     string    `json:"agent"`
	Snippet   string    `json:"snippet"`
}

// PauseSeed is Restorer.SeedPauseState's arguments.
type PauseSeed struct {
	PausedAt time.Time `json:"paused_at"`
	Trigger  string    `json:"trigger"`
	Reason   string    `json:"reason"`
	By       string    `json:"by,omitempty"`
}

// RestartSeed is Restorer.SeedRestartTelemetry's arguments.
type RestartSeed struct {
	Count      int            `json:"count"`
	Events     []RestartEvent `json:"events"`
	LastReason string         `json:"last_reason,omitempty"`
}

// SeedRequest is the body of POST /v1/agents/{name}/seed: the Restorer's
// per-agent seeds, each applied when present. TurnLoss is the JSON form of
// crux.TurnLoss, the runtime's own persisted shape. KickHistory being
// present but empty replaces the history with nothing.
type SeedRequest struct {
	Pause       *PauseSeed      `json:"pause,omitempty"`
	Restarts    *RestartSeed    `json:"restarts,omitempty"`
	TurnLoss    json.RawMessage `json:"turn_loss,omitempty"`
	LastKick    *time.Time      `json:"last_kick,omitempty"`
	KickHistory *[]KickRecord   `json:"kick_history,omitempty"`
}

// Authorize actions, the Action of AuthorizeRequest; one per Authorizer
// method.
const (
	AuthorizePROpen        = "pr_open"
	AuthorizePushBranch    = "push_branch"
	AuthorizeIssueOpen     = "issue_open"
	AuthorizeMerge         = "merge"
	AuthorizeReviewRequest = "review_request"
)

// AuthorizeRequest is the body of POST /v1/authorize, the synchronous gate
// on the host's relay request path. Kind is for AuthorizeIssueOpen, Repo for
// AuthorizeMerge. 204 means permitted; 403 CodeForbidden carries the
// runtime's refusal; any other status is also a refusal to the caller.
type AuthorizeRequest struct {
	Action  string `json:"action"`
	Agent   string `json:"agent"`
	FileUID int    `json:"file_uid"`
	Kind    string `json:"kind,omitempty"`
	Repo    string `json:"repo,omitempty"`
}

// WatchdogObservation is the body of GET /v1/agents/{name}/watchdog: the
// fleet watchdog's observed truth for one agent (watchdog.Observation) and
// the newest production evidence (watchdog.Fleet.LastProduction), nil when
// no evidence source was readable.
type WatchdogObservation struct {
	Agent              string     `json:"agent"`
	Backend            string     `json:"backend"`
	SessionExists      bool       `json:"session_exists"`
	Pane               string     `json:"pane,omitempty"`
	HasCLIMarker       bool       `json:"has_cli_marker"`
	ShowsLoginPrompt   bool       `json:"shows_login_prompt"`
	LastChange         time.Time  `json:"last_change"`
	StartedAt          time.Time  `json:"started_at"`
	AuthAvailable      bool       `json:"auth_available"`
	AuthKnown          bool       `json:"auth_known"`
	CredentialProven   bool       `json:"credential_proven"`
	ProviderErrorClass string     `json:"provider_error_class,omitempty"`
	ProviderErrorLine  string     `json:"provider_error_line,omitempty"`
	LastProduction     *time.Time `json:"last_production,omitempty"`
}

// Condition mirrors crux.Condition.
type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// Conditions is the body of PUT /v1/agents/{name}/watchdog/conditions
// (watchdog.Fleet.SetConditions).
type Conditions struct {
	Conditions []Condition `json:"conditions"`
}

// Additional error codes.
const (
	// CodeForbidden is an Authorizer refusal.
	CodeForbidden = "forbidden"
	// CodeKickRefused is Kicker.ValidateKick's refusal (409).
	CodeKickRefused = "kick_refused"
	// CodeResumeBusy, CodeResumeSessionGone and CodeResumeUnsupported are
	// the Resumer sentinels (409, 410, 422).
	CodeResumeBusy        = "resume_busy"
	CodeResumeSessionGone = "resume_session_gone"
	CodeResumeUnsupported = "resume_unsupported"
	// CodeNotSupported is a route the server's runtime cannot serve (the
	// watchdog routes on a runtime without a watchdog source).
	CodeNotSupported = "not_supported"
)

// Event types for the observer half of crux.Host (hivecommons/hive#14138
// stage 3): the server relays each observer the runtime offers as an event,
// with the observer's payload in Event.Data.
const (
	// EventKickOutcome carries a KickOutcome (Host.SetKickOutcomeObserver).
	EventKickOutcome = "agent.kick_outcome"
	// EventPauseTransition carries a PauseTransition
	// (Host.SetPauseTransitionObserver).
	EventPauseTransition = "agent.pause_transition"
	// EventRelaunch has no payload (Host.SetRelaunchObserver).
	EventRelaunch = "agent.relaunch"
	// EventPausePersist carries a PausePersist (Host.SetPersistPauseCallback).
	EventPausePersist = "agent.pause_persist"
	// EventPrompt carries a Prompt (Host.SetRecordPromptCallback).
	EventPrompt = "agent.prompt"
	// EventSandboxAudit carries a SandboxAudit (Host.SetSandboxAuditCallback).
	EventSandboxAudit = "agent.sandbox_audit"
	// EventAudit carries an Audit (Host.SetAuditSink).
	EventAudit = "runtime.audit"
	// EventCondition carries a ConditionEvent (Host.SetConditionReporter).
	EventCondition = "runtime.condition"
)

// KickOutcome mirrors crux.KickOutcome.
type KickOutcome struct {
	Kind   string    `json:"kind"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
	KickAt time.Time `json:"kickAt"`
}

// PauseTransition mirrors crux.PauseTransitionEvent.
type PauseTransition struct {
	Paused    bool           `json:"paused"`
	Trigger   string         `json:"trigger,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	By        string         `json:"by,omitempty"`
	Causation PauseCausation `json:"causation"`
	At        time.Time      `json:"at"`
}

// PausePersist is the payload of EventPausePersist.
type PausePersist struct {
	Paused bool `json:"paused"`
}

// Prompt is the payload of EventPrompt.
type Prompt struct {
	Trigger string `json:"trigger"`
	Prompt  string `json:"prompt"`
}

// SandboxAudit is the payload of EventSandboxAudit.
type SandboxAudit struct {
	Action string `json:"action"`
	Detail string `json:"detail,omitempty"`
}

// Audit is the payload of EventAudit.
type Audit struct {
	Actor  string         `json:"actor"`
	Action string         `json:"action"`
	Fields map[string]any `json:"fields,omitempty"`
}

// ConditionEvent is the payload of EventCondition; Action is "observe" or
// "clear".
type ConditionEvent struct {
	Action      string `json:"action"`
	Class       string `json:"class"`
	Key         string `json:"key"`
	Message     string `json:"message,omitempty"`
	Diagnostics string `json:"diagnostics,omitempty"`
}

// Condition actions.
const (
	ConditionObserve = "observe"
	ConditionClear   = "clear"
)

// Route is one HTTP route: the method and the path pattern as the server
// registers it (with {name}, {kind}, {id}, {backend} placeholders).
type Route struct {
	Method string
	Path   string
}

// RuntimeRoutes maps every crux.Runtime method to the route that serves it.
// Several methods share a route (one GET answers a family of reads; one POST
// takes a request whose fields select the variant). The Background family
// maps to the health route: the loops run inside the serving process, which
// reports them, and a client's Background methods are no-ops. Observers
// (SetKickObserver) map to the event stream. crux/api/client's parity test
// holds this table and the interface to each other.
var RuntimeRoutes = map[string]Route{
	// Lifecycle
	"Start":                {"POST", PathAgents + "/{name}/start"},
	"Stop":                 {"POST", PathAgents + "/{name}/stop"},
	"Restart":              {"POST", PathAgents + "/{name}/restart"},
	"RestartWithBootstrap": {"POST", PathAgents + "/{name}/restart"},
	"Pause":                {"POST", PathAgents + "/{name}/pause"},
	"PauseBy":              {"POST", PathAgents + "/{name}/pause"},
	"Resume":               {"POST", PathAgents + "/{name}/resume"},
	"IsPaused":             {"GET", PathAgents + "/{name}"},
	"ResetRestartCount":    {"DELETE", PathAgents + "/{name}/restart-count"},
	// Observer
	"GetStatus":       {"GET", PathAgents + "/{name}/snapshot"},
	"GetStatusFast":   {"GET", PathAgents + "/{name}/snapshot"},
	"AllStatuses":     {"GET", PathSnapshots},
	"ResolveAgent":    {"GET", PathAgents + "/{name}"},
	"GetOutput":       {"GET", PathAgents + "/{name}/logs"},
	"GetBufferOutput": {"GET", PathAgents + "/{name}/logs"},
	"CaptureFullLog":  {"GET", PathAgents + "/{name}/logs"},
	"FullLogHeader":   {"GET", PathAgents + "/{name}/logs"},
	// Kicker
	"SendKick":           {"POST", PathAgents + "/{name}/kick"},
	"SendKickAsync":      {"POST", PathAgents + "/{name}/kick"},
	"SendKickWithSource": {"POST", PathAgents + "/{name}/kick"},
	"ValidateKick":       {"POST", PathAgents + "/{name}/kick/validate"},
	"KickDispatchState":  {"GET", PathAgents + "/{name}/kick"},
	"ListKickLogs":       {"GET", PathAgents + "/{name}/kicklogs"},
	"ReadKickLog":        {"GET", PathAgents + "/{name}/kicklogs/{id}"},
	"SetKickObserver":    {"GET", PathEvents},
	// Definitions
	"AddAgent":               {"POST", PathDefinitions},
	"RemoveAgent":            {"DELETE", PathAgents + "/{name}"},
	"UpdateConfig":           {"PUT", PathAgents + "/{name}/definition"},
	"ReconcileAgents":        {"POST", PathReconcile},
	"SetModelOverride":       {"PUT", PathAgents + "/{name}/overrides"},
	"SetBackendOverride":     {"PUT", PathAgents + "/{name}/overrides"},
	"PinCLI":                 {"PUT", PathAgents + "/{name}/pins/{kind}"},
	"PinModel":               {"PUT", PathAgents + "/{name}/pins/{kind}"},
	"UnpinCLI":               {"DELETE", PathAgents + "/{name}/pins/{kind}"},
	"UnpinModel":             {"DELETE", PathAgents + "/{name}/pins/{kind}"},
	"ClearModeOverrides":     {"POST", PathModesClear},
	"SyncModeFiles":          {"POST", PathModesSync},
	"RefreshInferenceRoutes": {"POST", PathInference},
	// Fleet
	"BreakerState":                 {"GET", PathBreaker},
	"EngageBreaker":                {"POST", PathBreakerOn},
	"ReleaseBreaker":               {"POST", PathBreakerOff},
	"SetACMMLevel":                 {"PUT", PathACMM},
	"GetACMMLevel":                 {"GET", PathFleet},
	"RelaunchBobAgentsAwaitingKey": {"POST", PathRelaunchBob},
	"SharedDevUID":                 {"GET", PathFleet},
	// Credentials
	"SubmitLoginCode":                {"POST", PathAgents + "/{name}/login-code"},
	"CopilotToken":                   {"GET", PathCopilotCred},
	"CopilotTokenSource":             {"GET", PathCopilotCred},
	"SetCopilotToken":                {"PUT", PathCopilotCred},
	"ActivateCopilotToken":           {"PUT", PathCopilotCred},
	"ReloadClaudeToken":              {"POST", PathClaudeReload},
	"ClaudeCredentialCandidatePaths": {"GET", PathClaudeCred},
	"OmpAgentHomes":                  {"GET", PathOmpHomes},
	// Resumer
	"ResumeHandle":   {"GET", PathAgents + "/{name}/session"},
	"SendResumeKick": {"POST", PathAgents + "/{name}/session/kick"},
	"SessionID":      {"GET", PathAgents + "/{name}/session"},
	"SessionMissing": {"GET", PathAgents + "/{name}/session"},
	// Supervisor
	"GovernorMayStart":              {"GET", PathAgents + "/{name}/supervision"},
	"MarkStartupLaunchQueued":       {"POST", PathStartupQueued},
	"CheckAndRestartCrashedAgents":  {"POST", PathRestartCrashed},
	"ConsentWedgedAgents":           {"GET", PathFleet},
	"RefreshAgentTokens":            {"POST", PathTokensRefresh},
	"RefreshAgentTokenFor":          {"POST", PathAgents + "/{name}/tokens/refresh"},
	"AgentCredentialRefreshable":    {"GET", PathAgents + "/{name}/supervision"},
	"AgentHasValidCredential":       {"GET", PathAgents + "/{name}/supervision"},
	"AgentCapabilities":             {"GET", PathAgents + "/{name}/supervision"},
	"EffectiveBackend":              {"GET", PathAgents + "/{name}/supervision"},
	"InvocationMetadata":            {"GET", PathAgents + "/{name}/supervision"},
	"RestartTelemetry":              {"GET", PathAgents + "/{name}/supervision"},
	"StartFailureState":             {"GET", PathAgents + "/{name}/supervision"},
	"ProviderErrorBackoffRemaining": {"GET", PathAgents + "/{name}/supervision"},
	"PauseByCause":                  {"POST", PathAgents + "/{name}/pause"},
	"ArchiveAllKickLogs":            {"POST", PathKickLogsArchive},
	"CountAgentsWithModel":          {"GET", PathFleet},
	"CrashOOMSuspected":             {"GET", PathAgents + "/{name}/supervision"},
	"BackendAuthAvailable":          {"GET", PathBackends + "/{backend}/auth"},
	"AgentAuthAvailable":            {"GET", PathAgents + "/{name}/supervision"},
	"SendGitHubAPIBudgetNudge":      {"POST", PathAgents + "/{name}/nudge"},
	"RecordAudit":                   {"POST", PathAudit},
	"TrajectoryAgents":              {"GET", PathTrajectory},
	// Restorer
	"SeedPauseState":       {"POST", PathAgents + "/{name}/seed"},
	"SeedRestartTelemetry": {"POST", PathAgents + "/{name}/seed"},
	"SeedTurnLoss":         {"POST", PathAgents + "/{name}/seed"},
	"SeedLastKick":         {"POST", PathAgents + "/{name}/seed"},
	"SeedKickHistory":      {"POST", PathAgents + "/{name}/seed"},
	"RestoreBreaker":       {"PUT", PathBreaker},
	// Background (server-side loops; the health body lists them)
	"StartAgentTokenRefresh":         {"GET", PathHealth},
	"StartCredentialWatchdog":        {"GET", PathHealth},
	"StartCopilotSessionRefresh":     {"GET", PathHealth},
	"StartTranscriptActivityRefresh": {"GET", PathHealth},
	"StartRepeatFailureMonitor":      {"GET", PathHealth},
	// Authorizer
	"AuthorizePROpen":        {"POST", PathAuthorize},
	"AuthorizePushBranch":    {"POST", PathAuthorize},
	"AuthorizeIssueOpen":     {"POST", PathAuthorize},
	"AuthorizeMerge":         {"POST", PathAuthorize},
	"AuthorizeReviewRequest": {"POST", PathAuthorize},
}

// WatchdogRoutes are the watchdog seam's routes (crux/runtime.NewWatchdogFleet
// over a sidecar). They are not crux.Runtime methods, so they sit outside
// RuntimeRoutes; crux/serve's tests assert they are registered.
var WatchdogRoutes = map[string]Route{
	"AgentNames":     {"GET", PathWatchdogAgents},
	"Observe":        {"GET", PathAgents + "/{name}/watchdog"},
	"LastProduction": {"GET", PathAgents + "/{name}/watchdog"},
	"SetConditions":  {"PUT", PathAgents + "/{name}/watchdog/conditions"},
}

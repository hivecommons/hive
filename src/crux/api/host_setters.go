package api

// HostSetterKind says how a crux.Host setter reaches a sidecar runtime
// (hivecommons/hive#14138 stage 4).
type HostSetterKind string

const (
	// HostSetterEvent: the setter installs an observer; the sidecar's
	// runtime emits the observation on the event stream and hive's host
	// shim invokes the observer from it. Via is the event type.
	HostSetterEvent HostSetterKind = "event"
	// HostSetterCallback: the setter hands the runtime a hive-provided
	// value or call; the sidecar's runtime reaches the equivalent on the
	// hive callback endpoint (crux/api/callback). Via is the route.
	HostSetterCallback HostSetterKind = "callback"
	// HostSetterRoute: hive pushes the value to the runtime over the
	// runtime API. Via is the route.
	HostSetterRoute HostSetterKind = "route"
	// HostSetterConfig: the sidecar reads the value from its own
	// configuration. Via is the key.
	HostSetterConfig HostSetterKind = "config"
	// HostSetterInProcessOnly: the setter has no serialisable form and the
	// sidecar runtime uses its own in-process equivalent. Via says what.
	HostSetterInProcessOnly HostSetterKind = "inprocess"
)

// HostSetter is one row of HostSetters.
type HostSetter struct {
	Kind HostSetterKind
	Via  string
}

// Callback endpoint routes the table refers to, spelled here because
// crux/api/callback imports this package; its tests hold the two in step.
const (
	hostViaPolicy          = "GET /v1/policy"
	hostViaRepoAutoMerge   = "POST /v1/policy/repo-auto-merge"
	hostViaTokens          = "POST /v1/tokens"
	hostViaMint            = "POST /v1/tokens/mint"
	hostViaBobCredential   = "GET /v1/bob/credential"
	hostViaLinear          = "GET /v1/linear/credential"
	hostViaPulls           = "POST /v1/pulls"
	hostViaInferenceRoutes = "PUT|DELETE /v1/inference/routes/{agent}"
	hostViaEffects         = "POST /v1/effects, POST /v1/effects/{id}/end"
	hostViaConditions      = "POST /v1/conditions"
)

// HostSetters maps every crux.Host setter to how a sidecar runtime gets
// what an in-process runtime is handed through it. crux/api/client's parity
// test holds this table and the interface to each other, so a new setter
// cannot be added without saying how the sidecar is served.
var HostSetters = map[string]HostSetter{
	// Credentials -> hive callback endpoint (stage 2; the mint issuer and
	// Bob key in stage 4).
	"SetAppAuth":                  {HostSetterCallback, hostViaTokens},
	"SetAgentMint":                {HostSetterCallback, hostViaMint},
	"SetLinearCredentialResolver": {HostSetterCallback, hostViaLinear},
	"SetBobAPIKeyResolver":        {HostSetterCallback, hostViaBobCredential},
	"SetBobKeySourceResolver":     {HostSetterCallback, hostViaBobCredential},
	"SetSandboxPushMinter":        {HostSetterCallback, hostViaTokens},
	"SetSandboxPRClient":          {HostSetterCallback, hostViaPulls},

	// Policy -> the hive callback's policy snapshot (stage 4).
	"SetBuildInfo":                    {HostSetterCallback, hostViaPolicy},
	"SetGatewayBackendChecker":        {HostSetterCallback, hostViaPolicy},
	"SetExplainModeDefaultResolver":   {HostSetterCallback, hostViaPolicy},
	"SetRepoAutoMergeEnabledResolver": {HostSetterCallback, hostViaRepoAutoMerge},
	"SetKickLogSourceResolver":        {HostSetterCallback, hostViaPolicy},
	"SetSandboxConfig":                {HostSetterCallback, hostViaPolicy},
	"SetDeadSessionRecoveryOwner":     {HostSetterCallback, hostViaPolicy},

	// Calls into hive -> their own callback families (stage 4).
	"SetInferenceCallbacks":      {HostSetterCallback, hostViaInferenceRoutes},
	"SetSandboxMutationBoundary": {HostSetterCallback, hostViaEffects},

	// Observers -> the event stream (stage 3). Conditions also go back
	// over the callback so hive's fleet report sees them.
	"SetKickOutcomeObserver":     {HostSetterEvent, EventKickOutcome},
	"SetPauseTransitionObserver": {HostSetterEvent, EventPauseTransition},
	"SetRelaunchObserver":        {HostSetterEvent, EventRelaunch},
	"SetPersistPauseCallback":    {HostSetterEvent, EventPausePersist},
	"SetRecordPromptCallback":    {HostSetterEvent, EventPrompt},
	"SetSandboxAuditCallback":    {HostSetterEvent, EventSandboxAudit},
	"SetAuditSink":               {HostSetterEvent, EventAudit},
	"SetConditionReporter":       {HostSetterEvent, EventCondition + ", " + hostViaConditions},

	// In-process only.
	"SetSpanStarter": {HostSetterInProcessOnly, "OpenTelemetry global tracer in the runtime process; trajectory events are independent of spans"},
	// Advisor lane (v6, #9722): the sidecar runtime does not run the advisor
	// lane yet; resolvers are installed only in-process until the lane's
	// policy rides the hive callback.
	"SetAdvisorEnabledResolver": {HostSetterInProcessOnly, "advisor lane is in-process only; sidecar follow-up"},
	"SetAdvisorRolesResolver":   {HostSetterInProcessOnly, "advisor lane is in-process only; sidecar follow-up"},
	// Repeat-failure BASH_ENV route (v6, #14574): the enabled switch is read
	// in-process until it rides the hive callback policy.
	"SetRepeatFailureEnabledResolver": {HostSetterInProcessOnly, "repeat-failure capture switch is in-process only; sidecar follow-up"},
	"SetRepeatFailureConfigResolver":  {HostSetterInProcessOnly, "repeat-failure monitor config is in-process only; sidecar follow-up"},
}

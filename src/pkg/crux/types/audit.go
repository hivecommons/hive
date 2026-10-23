package types

// Audit action names the runtime itself records: agent lifecycle events, the
// tool-approval verdict, and the credential watchdog's missing-token signals.
// Snake_case to match the dashboard's existing audit vocabulary (add_agent,
// config_agent_models, …) so the Audit Log UI's free-text/regex filter
// treats them uniformly.
//
// The governor-side vocabulary (lease stages, relay events, jev decisions,
// commit trailers) is pkg/agentaudit's alone; this leaf holds only what the
// runtime emits, so the runtime does not depend on the governor's package
// (hivecommons/hive#13536). pkg/agentaudit aliases these so the recorder
// and every other emitter keep their existing spelling.
const (
	AuditAgentStarted     = "agent_started"
	AuditAgentStartFailed = "agent_start_failed"
	AuditAgentStopped     = "agent_stopped"
	AuditAgentKicked      = "agent_kicked"
	AuditAgentPaused      = "agent_paused"
	AuditAgentResumed     = "agent_resumed"
	AuditAgentAdded       = "agent_added"
	AuditAgentRemoved     = "agent_removed"
	AuditAgentBackendSet  = "agent_backend_changed"
	AuditAgentModelSet    = "agent_model_changed"
	AuditToolApproval     = "tool_approval"

	// AuditCopilotTokenMissing is recorded by the credential watchdog when the
	// durable Copilot device-flow token file (/data/copilot-user-token) is
	// absent or empty on a Copilot-backend hive. It surfaces the "stuck at
	// /login after an upgrade roll" failure — where a bare in-CLI `/login`
	// wrote config.json but never the durable file, so fresh agent pods have
	// no token to auto-refresh from — as an immediate, queryable Audit Log
	// signal instead of a silent multi-hour outage. Recovery is an operator
	// dashboard device-flow login; the watchdog never touches token material.
	AuditCopilotTokenMissing = "copilot_token_missing"

	// AuditClaudeTokenMissing is the Claude-backend analogue: recorded when the
	// durable Claude OAuth credentials file (/data/home/.claude/.credentials.json)
	// is absent, unparseable, or expired on a Claude-backend hive. Same silent
	// failure shape and same operator-login recovery as the Copilot case; the
	// detail's outcome= distinguishes "missing" from "invalid or expired".
	AuditClaudeTokenMissing = "claude_token_missing"
)

// AuditSink receives agent lifecycle events for durable, queryable recording.
//
// It exists so audit emitters can feed the dashboard's audit store WITHOUT
// importing pkg/dashboard: the dependency already runs the other way
// (pkg/dashboard imports the emitting packages), so a direct import would be
// an import cycle. The wiring layer (cmd/hive) owns both objects and injects
// the dashboard's recorder as this interface.
//
// Implementations must be safe for concurrent use and must not block: the
// agent manager calls Record while holding its mutex on the launch path.
type AuditSink interface {
	// Record persists one lifecycle event. actor is the responsible user, or
	// "system" for hive-originated events. fields carries structured context
	// (backend, model, outcome, error, …) that the sink renders into the
	// entry's detail.
	Record(actor, action, agentName string, fields map[string]any)
}

// Fields is a small helper for building the fields map from alternating
// key/value pairs, mirroring slog's variadic style so a call site can pass the
// same context to both the logger and the audit sink without restating it.
// An odd trailing key is dropped. Empty-string values are dropped so an
// unset backend/model does not render as a noisy "model=" in the UI.
func Fields(kv ...any) map[string]any {
	fields := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		if s, isStr := kv[i+1].(string); isStr && s == "" {
			continue
		}
		fields[key] = kv[i+1]
	}
	return fields
}

package agent

import (
	"fmt"
	"sort"
	"strings"
)

// Audit action names recorded for agent lifecycle events. Snake_case to match
// the dashboard's existing audit vocabulary (add_agent, config_agent_models,
// …) so the Audit Log UI's free-text/regex filter treats them uniformly.
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

// auditActorSystem attributes an event to the hive process itself rather than
// to a person. Matches the dashboard's pseudo-user convention (see
// auditPseudoUsers there): "system" entries never count as user engagement.
// Lifecycle events originate in the agent manager, which has no HTTP request
// and therefore no authenticated user, so they are all system-attributed; the
// human-vs-governor distinction is carried in the detail's trigger= field.
const auditActorSystem = "system"

// AuditSink receives agent lifecycle events for durable, queryable recording.
//
// It exists so pkg/agent can feed the dashboard's audit store WITHOUT
// importing pkg/dashboard: the dependency already runs the other way
// (pkg/dashboard imports pkg/agent for agent types and auth), so a direct
// import here would be an import cycle. The wiring layer (cmd/hive) owns both
// objects and injects the dashboard's recorder as this interface.
//
// Implementations must be safe for concurrent use and must not block: the
// manager calls Record while holding m.mu on the agent launch path.
type AuditSink interface {
	// Record persists one lifecycle event. actor is the responsible user, or
	// "system" for hive-originated events. fields carries structured context
	// (backend, model, outcome, error, …) that the sink renders into the
	// entry's detail.
	Record(actor, action, agentName string, fields map[string]any)
}

// SetAuditSink installs the durable audit sink. Safe to leave unset: a nil
// sink makes every audit call a no-op, which is what unit tests and any
// non-dashboard embedding of the manager get.
func (m *Manager) SetAuditSink(sink AuditSink) {
	m.auditSink.Store(&sink)
}

// audit records a lifecycle event to the durable audit store, if one is
// installed. It is deliberately a no-op when no sink is set rather than an
// error: audit recording must never be able to fail an agent operation.
//
// Stored/read via atomic.Pointer rather than under m.mu for the same reason as
// isGatewayBackend and bobAPIKeyResolver: this is called from the launch path
// (launchInTmux via Start), which ALREADY holds m.mu.Lock(). Re-locking a
// non-reentrant RWMutex on the same goroutine would deadlock startup before
// MarkReady and crash-loop every spoke.
func (m *Manager) audit(action, agentName string, fields map[string]any) {
	m.auditWithActor(auditActorSystem, action, agentName, fields)
}

// auditWithActor is audit with an explicit responsible user, for the paths
// where an operator's identity is known (dashboard-initiated switches).
func (m *Manager) auditWithActor(actor, action, agentName string, fields map[string]any) {
	p := m.auditSink.Load()
	if p == nil || *p == nil {
		return
	}
	(*p).Record(actor, action, agentName, fields)
}

// auditFields is a small helper for building the fields map from alternating
// key/value pairs, mirroring slog's variadic style so a call site can pass the
// same context to both the logger and the audit sink without restating it.
// An odd trailing key is dropped. Empty-string values are dropped so an
// unset backend/model does not render as a noisy "model=" in the UI.
func auditFields(kv ...any) map[string]any {
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

// FormatAuditDetail renders a fields map as the stable "k=v, k=v" detail
// string the Audit Log UI displays and filters over. Keys are sorted so the
// same event always renders identically (Go map iteration is randomized),
// except that the conventional lead keys — outcome, backend, model — are
// hoisted to the front where a reader scans first.
//
// Exported because the sink implementation lives in pkg/dashboard and must
// produce the identical shape.
func FormatAuditDetail(fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	leadKeys := []string{"outcome", "backend", "model"}
	seen := make(map[string]bool, len(leadKeys))
	ordered := make([]string, 0, len(fields))
	for _, k := range leadKeys {
		if _, ok := fields[k]; ok {
			ordered = append(ordered, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(fields))
	for k := range fields {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	ordered = append(ordered, rest...)

	var b strings.Builder
	for _, k := range ordered {
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s=%v", k, fields[k])
	}
	return b.String()
}

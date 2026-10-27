// Package agentaudit holds the agent audit vocabulary: the lifecycle action
// names, the AuditSink interface, and the detail-string formatting shared by
// the recorder (pkg/dashboard) and the emitters (pkg/agent, pkg/toolapprove,
// pkg/turn).
//
// It is a leaf in the import graph apart from the crux root. AuditSink
// originally lived in pkg/agent to break an agent→dashboard import cycle;
// moving it here also frees policy-layer packages (pkg/toolapprove, pkg/turn)
// from importing the pkg/agent runtime and its transitive dependency closure
// just to accept a sink. pkg/agent re-exports everything here under its
// original names, so existing consumers are unaffected.
package agentaudit

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux"
)

// Audit action names recorded for agent lifecycle events. Snake_case to match
// the dashboard's existing audit vocabulary (add_agent, config_agent_models,
// …) so the Audit Log UI's free-text/regex filter treats them uniformly.
//
// The runtime-emitted names (agent lifecycle, tool approval, missing-token
// signals) are DEFINED in the crux root package, which the runtime
// owns, and aliased here so the runtime does not import this governor-side
// package (hivecommons/hive#13536). Spellings are unchanged.
const (
	AuditAgentStarted     = crux.AuditAgentStarted
	AuditAgentStartFailed = crux.AuditAgentStartFailed
	AuditAgentStopped     = crux.AuditAgentStopped
	AuditAgentKicked      = crux.AuditAgentKicked
	AuditAgentPaused      = crux.AuditAgentPaused
	AuditAgentResumed     = crux.AuditAgentResumed
	AuditAgentAdded       = crux.AuditAgentAdded
	AuditAgentRemoved     = crux.AuditAgentRemoved
	AuditAgentBackendSet  = crux.AuditAgentBackendSet
	AuditAgentModelSet    = crux.AuditAgentModelSet
	AuditToolApproval     = crux.AuditToolApproval
	// AuditJevDecision records one agent call to the Jev typed-decision
	// endpoint (hivecommons/hive#8939): question type, confidence and the
	// input tokens billed to the agent's budget. Never the question text or
	// state — those can carry repository content.
	AuditJevDecision = "jev_decision"

	// AuditCopilotTokenMissing is recorded by the credential watchdog when the
	// durable Copilot device-flow token file (/data/copilot-user-token) is
	// absent or empty on a Copilot-backend hive. It surfaces the "stuck at
	// /login after an upgrade roll" failure — where a bare in-CLI `/login`
	// wrote config.json but never the durable file, so fresh agent pods have
	// no token to auto-refresh from — as an immediate, queryable Audit Log
	// signal instead of a silent multi-hour outage. Recovery is an operator
	// dashboard device-flow login; the watchdog never touches token material.
	AuditCopilotTokenMissing = crux.AuditCopilotTokenMissing

	// AuditClaudeTokenMissing is the Claude-backend analogue: recorded when the
	// durable Claude OAuth credentials file (/data/home/.claude/.credentials.json)
	// is absent, unparseable, or expired on a Claude-backend hive. Same silent
	// failure shape and same operator-login recovery as the Copilot case; the
	// detail's outcome= distinguishes "missing" from "invalid or expired".
	AuditClaudeTokenMissing = crux.AuditClaudeTokenMissing

	// AuditLeaseStageAdvanced, AuditLeaseStageRetried and AuditLeaseStageReset
	// record Hive-owned run stage transitions on the existing task lease
	// record. They are system actions: no contributor can advance, retry or
	// reset a stage by editing client fields. A reset (#8350) is the only
	// backwards move and is reachable solely through an owner-gated route;
	// its detail carries the reason the owner gave.
	AuditLeaseStageAdvanced = "lease_stage_advanced"
	AuditLeaseStageRetried  = "lease_stage_retried"
	AuditLeaseStageReset    = "lease_stage_reset"
	// AuditLeaseStageRefused records the Spektacular runner declining to
	// advance a stage (a final artifact went back to draft, or the artifact the
	// lease is bound to vanished). AuditLeaseStageEscalated records the runner
	// exhausting a stage's retry budget and raising a decision escalation
	// (hivecommons/hive#8303).
	AuditLeaseStageRefused    = "lease_stage_refused"
	AuditLeaseStageEscalated  = "lease_stage_escalated"
	AuditTaskMCPStageMismatch = "task_mcp_stage_mismatch"

	// AuditPersonaAdjusted records a user accepting, rejecting, or undoing a
	// persona learning suggestion (hivecommons/hive#8363). The actor is the
	// user; the detail carries key/from/to and the evidence counts. It never
	// names an agent because no agent permission is involved.
	AuditPersonaAdjusted = "persona_adjusted"
)

// Audit action names recorded by the hub for the ClankeR contributor relay
// (hivecommons/hive#13441). The relay is a principal acting on the hub, so
// these rows carry the contributor's GitHub login as the user and the
// contributor session identity as the agent; task rows also carry the task's
// repository and issue/PR number. Progress and heartbeat frames are telemetry
// and are never audited. relay_token_minted and relay_task_revoked count
// toward the dashboard's sensitive_24h; relay_connected does not, because it
// fires on every reconnect. No row ever carries token material.
const (
	AuditRelayConnected       = "relay_connected"
	AuditRelayRejected        = "relay_rejected"
	AuditRelayDisconnected    = "relay_disconnected"
	AuditRelayTaskAssigned    = "relay_task_assigned"
	AuditRelayTaskAccepted    = "relay_task_accepted"
	AuditRelayTaskDeclined    = "relay_task_declined"
	AuditRelayTokenMinted     = "relay_token_minted"
	AuditRelayMCPLeaseIssued  = "relay_mcp_lease_issued"
	AuditRelayMCPLeaseRevoked = "relay_mcp_lease_revoked"
	AuditRelayTaskCompleted   = "relay_task_completed"
	AuditRelayTaskFailed      = "relay_task_failed"
	AuditRelayTaskRevoked     = "relay_task_revoked"
	AuditRelayTaskAbandoned   = "relay_task_abandoned"
	// AuditRelayTaskAttested records audit rows a contributor CHOSE to share
	// with a terminal frame (hivecommons/hive#13458). Opt-in only: a relay
	// without `--share-audit` never sends any, and this row is never written.
	AuditRelayTaskAttested = "relay_task_attested"
)

// AuditSink receives agent lifecycle events for durable, queryable recording.
// See crux.AuditSink for the contract; the alias keeps every recorder and
// emitter on this package's spelling.
type AuditSink = crux.AuditSink

// Fields builds the fields map from alternating key/value pairs; see
// crux.Fields.
func Fields(kv ...any) map[string]any {
	return crux.Fields(kv...)
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

package proxy

import "fmt"

// Machine-readable reason codes for a proxy 403 (#12127). Every blocked
// request carries exactly one, both as the trailing `reason=<code>` token of
// the one-line body and as the X-Hive-Proxy-Reason header, so an agent policy,
// a test or a log scraper can key on WHICH gate refused without parsing prose.
// The codes are stable identifiers; the prose in front of them may be
// reworded, the codes may not. bin/gh-wrapper.sh uses the same vocabulary for
// the gates it enforces client-side.
const (
	DenyReasonRepoPaused           = "repo_paused"
	DenyReasonAgentRepoScope       = "agent_repo_scope"
	DenyReasonRepoAutoMergeOff     = "repo_auto_merge_disabled"
	DenyReasonWriteSurfaceEnforced = "write_surface_enforce"
	DenyReasonLinear               = "linear_policy"
	DenyReasonGraphQLMutation      = "graphql_mutation"
	DenyReasonGraphQL              = "graphql_denied"
	DenyReasonHardDeny             = "hard_deny"
	DenyReasonMode                 = "mode_denied"
	DenyReasonRepoNotInConfig      = "repo_not_in_config"
	DenyReasonIssueShape           = "issue_shape"
	DenyReasonCanaryEgress         = "canary_egress"
	DenyReasonUnknown              = "denied"
)

// ProxyDenialHeader names the response header that carries the reason code.
const ProxyDenialHeader = "X-Hive-Proxy-Reason"

// ProxyDenialBody renders the ONE-line plain-text body of a proxy 403. The
// shape is fixed: who was refused (agent, mode), what (method + detail — the
// gate's actionable directive when it has one, else the path), and the
// reason code last so it is trivially greppable. The directive text is kept
// verbatim because it is what tells the agent what to do instead; what this
// format removes is everything that was not that (multi-line framing and
// repetition), and it adds the code.
func ProxyDenialBody(agentName string, mode fmt.Stringer, method, detail, code string) string {
	if code == "" {
		code = DenyReasonUnknown
	}
	return fmt.Sprintf("⛔ ACMM proxy: %s (%s) blocked %s %s reason=%s\n", agentName, mode, method, detail, code)
}

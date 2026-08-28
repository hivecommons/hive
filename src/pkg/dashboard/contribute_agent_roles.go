package dashboard

import (
	"fmt"
	"github.com/kubestellar/hive/pkg/worksource"
	"strings"
)

var roleClaimMinTier = map[string]string{
	"scanner":       "contributor",
	"quality":       "contributor",
	"outreach":      "contributor",
	"ci-maintainer": "trusted",
	"sec-check":     "trusted",
	"architect":     "trusted",
}

var roleClaimNeedsGrant = map[string]bool{
	"ci-maintainer": true,
	"sec-check":     true,
	"architect":     true,
}

var trustTierRank = map[string]int{
	"newcomer":    0,
	"advisor":     0,
	"contributor": 1,
	"trusted":     2,
	"merger":      3,
}

func normalizeAgentRole(role string) string {
	return strings.ToLower(strings.TrimSpace(role))
}

func effectiveAssignedAgentRole(assigned string) string {
	assigned = normalizeAgentRole(assigned)
	if assigned == "none" {
		return ""
	}
	return assigned
}

func hasOwnerAgentRoleAssignment(profile *ContributorProfile) bool {
	return profile != nil && normalizeAgentRole(profile.AssignedAgentRole) != ""
}

func hasAgentRoleGrant(profile *ContributorProfile, role string) bool {
	if profile == nil {
		return false
	}
	role = normalizeAgentRole(role)
	for _, grant := range profile.AgentRoleGrants {
		if normalizeAgentRole(grant) == role {
			return true
		}
	}
	return false
}

func (h *ContributeWSHub) roleClaimAllowed(c *ContributorConnection, role string) (bool, string) {
	role = normalizeAgentRole(role)
	if role == "" {
		return true, ""
	}
	if role == "supervisor" {
		return false, "supervisor is not delegatable"
	}
	if c == nil || c.profile == nil {
		return false, "missing contributor profile"
	}
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Config == nil {
		// Unit-test and legacy no-config hubs have no work-selector config to
		// consult. Preserve their historical "role is a display label" behavior;
		// real hives have Config and are gated below before any role-shaped task.
		return true, ""
	}
	cfg := h.server.deps.Config
	agent, ok := cfg.Agents[role]
	if !ok {
		return false, fmt.Sprintf("unknown agent role %q", role)
	}
	if !agent.Enabled {
		return false, fmt.Sprintf("agent role %q is disabled", role)
	}
	if !cfg.Hub.IsContributeRoleDelegatable(role) {
		return false, fmt.Sprintf("agent role %q is not enabled for contributor delegation", role)
	}
	tier := normalizeAgentRole(c.profile.TrustTier)
	minTier := roleClaimMinTier[role]
	if minTier == "" {
		minTier = "trusted"
	}
	if trustTierRank[tier] < trustTierRank[minTier] {
		return false, fmt.Sprintf("trust tier %q cannot claim agent role %q", tier, role)
	}
	if roleClaimNeedsGrant[role] && !hasAgentRoleGrant(c.profile, role) {
		return false, fmt.Sprintf("agent role %q requires an operator grant", role)
	}
	return true, ""
}

func (h *ContributeWSHub) roleKickPrompt(role string) string {
	role = normalizeAgentRole(role)
	if role == "" || h == nil || h.server == nil || h.server.deps == nil || h.server.deps.Scheduler == nil {
		return ""
	}
	return h.server.deps.Scheduler.BuildAgentMessageFromLastActionable(role)
}

// buildRoleTaskPromptForRef wraps the source-aware assignment prompt in the
// role framing. Only the base prompt carries work identity, so the role text
// itself is unchanged for every source.
func buildRoleTaskPromptForRef(ref worksource.Ref, title, role, agentPrompt string) string {
	base := buildTaskPromptForRef(ref, title)
	role = normalizeAgentRole(role)
	if role == "" {
		return base
	}
	if strings.TrimSpace(agentPrompt) == "" {
		return fmt.Sprintf("[clanker-agent-role:%s]\n\nOperate as the hive spoke agent role %q for this contributor-owned task. Your credentials and permissions remain those of the contributor; do not assume any extra access from the role.\n\n%s", role, role, base)
	}
	return fmt.Sprintf("[clanker-agent-role:%s]\n\nA contributor is temporarily taking on the hive spoke agent role %q. Attribution remains the contributor's GitHub identity, and permissions remain the contributor tier's permissions.\n\nROLE KICK PROMPT (same template used for the spoke agent):\n%s\n\nASSIGNED CONTRIBUTOR TASK:\n%s", role, role, agentPrompt, base)
}

func (h *ContributeWSHub) issueMatchesAgentRole(role, title string, labels []string, lane string) bool {
	role = normalizeAgentRole(role)
	if role == "" || role == "scanner" {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(lane), role) {
		return true
	}
	var keywords []string
	if h != nil && h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		if agent, ok := h.server.deps.Config.Agents[role]; ok {
			keywords = append(keywords, agent.LaneKeywords...)
			keywords = append(keywords, agent.DetectKeywords...)
		}
	}
	if len(keywords) == 0 {
		keywords = append(keywords, role)
	}
	hay := strings.ToLower(title + " " + strings.Join(labels, " "))
	for _, kw := range keywords {
		kw = strings.ToLower(strings.TrimSpace(kw))
		if kw != "" && strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

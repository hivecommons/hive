package agent

import (
	"fmt"
	"strings"
)

// AuthorizeReviewRequest enforces the policy for the review-request watcher.
// It mirrors AuthorizePROpen's forge-resistance, but grants on capability as
// well as mode (hivecommons/hive#7485).
//
// The review-request watcher is the SANCTIONED path: its doc comment tells an
// agent to write a request file "INSTEAD of running `gh pr review` from its
// own shell", because the file relay is App-authored and lands on the audit
// trail. But it was gated with AuthorizePROpen, which requires CanPush() —
// while the proxy, which is what a direct `gh pr review` goes through, grants
// the same write on Converse alone:
//
//	MinMode: agent.ModeIssuesAndPRs, Capability: agent.AgentCapabilities.CanConverse
//	                                            — pkg/proxy/rules.go:152
//
// So an ADVISORY+converse agent could post a review by going AROUND the relay
// but not THROUGH it: the audited path was strictly stricter than the
// unaudited one, which is exactly backwards — it pressures agents off the
// trail. This aligns the relay with the proxy so the sanctioned route is never
// the more restricted one. Converse stays conversation-only: it does not grant
// pushing, opening PRs, or merging, which all remain on the mode ladder.
//
// A nil manager or unknown agent is denied.
func (m *Manager) AuthorizeReviewRequest(agentName string, fileUID int) error {
	if strings.TrimSpace(agentName) == "" {
		return fmt.Errorf("no agent named in the request")
	}
	// Forge check: when we have a UID map and a real owning UID, the file owner
	// must BE this agent.
	if m.uidMap != nil && fileUID > 0 {
		owner := m.uidMap.LookupByUID(fileUID)
		if owner == "" {
			return fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
		}
		if owner != agentName {
			return fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
		}
	}
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return fmt.Errorf("unknown agent %q", agentName)
	}
	if m.agentCapabilities(agent).CanConverse() {
		return nil
	}
	if !m.agentMode(agent).CanPush() {
		return fmt.Errorf("agent %q may not review PRs at this ACMM level (mode %s) and does not have the `converse` capability",
			agentName, m.agentMode(agent).String())
	}
	return nil
}

package agent

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/crux/agentmode"
)

// AuthorizeForgeClone is the gate for the forge clone relay
// (hivecommons/hive#19997): the hive fetches a private forge repo with a
// short-lived token and hands the requesting agent a bundle of its objects.
// It returns the token tier the clone is minted for.
//
// A clone is a read, so there is no CanPush gate, but it is stricter than the
// write gates on identity: the request file must be owned by a registered
// agent UID that is the agent the request names. With no UID map, or a file
// the UID map cannot place, the relay is refused instead of falling back to
// the name in the request: without per-agent UIDs the bundle cannot be made
// readable to the requesting agent alone, and any agent could claim another
// agent's repo scope.
func (m *Manager) AuthorizeForgeClone(agentName string, fileUID int) (agentmode.TokenTier, error) {
	if strings.TrimSpace(agentName) == "" {
		return "", fmt.Errorf("no agent named in the request")
	}
	if m.uidMap == nil || fileUID <= 0 {
		return "", fmt.Errorf("the clone relay needs per-agent UIDs, and the request file's owner (uid %d) cannot be checked against one", fileUID)
	}
	owner := m.uidMap.LookupByUID(fileUID)
	if owner == "" {
		return "", fmt.Errorf("request file owned by unknown uid %d (not a registered agent)", fileUID)
	}
	if owner != agentName {
		return "", fmt.Errorf("request claims agent %q but file is owned by agent %q (uid %d)", agentName, owner, fileUID)
	}
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	if agent == nil {
		return "", fmt.Errorf("unknown agent %q", agentName)
	}
	return TokenTierForRole(m.agentMode(agent), agent.Config.Role), nil
}

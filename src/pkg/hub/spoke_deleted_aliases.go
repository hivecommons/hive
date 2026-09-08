package hub

import (
	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/hub/spoke"

	"time"
)

// This file holds the surviving pass-through aliases from the spoke
// extraction (#6068). Only aliases with live production callers remain;
// new code should import pkg/hub/spoke directly (as pkg/dashboard already
// does) instead of adding aliases here.

func QuotaExhaustedProcessCount(statuses map[string]*agent.AgentProcess) int {
	return spoke.QuotaExhaustedProcessCount(statuses)
}
func QuotaExhaustedAgentReason(count int) string { return spoke.QuotaExhaustedAgentReason(count) }

func HashDashboardToken(token string) string { return spoke.HashDashboardToken(token) }

func SelfImageReleaseChannel() string { return spoke.SelfImageReleaseChannel() }
func SelfDeploymentImage() string     { return spoke.SelfDeploymentImage() }

func MintSSOToken(seedHex, username, role, hiveID string, now time.Time) string {
	return spoke.MintSSOToken(seedHex, username, role, hiveID, now)
}

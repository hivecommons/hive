package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/hivecommons/hive/pkg/config"
)

// Request parsing and audit wording for the proxy's per-agent GitHub API
// hourly cap: the fleet default (agent_github_api_hourly_cap on
// PUT /api/config/github, the Forge App tab) and one agent's override
// (githubApiHourlyCap on PUT /api/config/agent/{name}/general, the agent's
// General tab). Both write the live config the proxy reads, then save it
// through the normal dashboard save (hive.yaml, the dashboard overlay and the
// PVC runtime copy).

const agentGitHubCapField = "githubApiHourlyCap"

// fleetAgentGitHubCap decodes agent_github_api_hourly_cap: present=false when
// the field is absent, a nil cap for null (back to the default), else a whole
// number of at least 0 (0 = no cap).
func (b githubAPISettings) fleetAgentGitHubCap() (cap *int, present bool, err error) {
	raw := bytes.TrimSpace(b.AgentGitHubAPIHourlyCap)
	if len(raw) == 0 {
		return nil, false, nil
	}
	if string(raw) == "null" {
		return nil, true, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
		return nil, true, fmt.Errorf("agent_github_api_hourly_cap must be a whole number of at least 0 (0 = no cap), or null for the default (%d)", config.DefaultAgentsGitHubAPIHourlyCap)
	}
	return &n, true, nil
}

// applyFleetAgentGitHubCap sets agents.github_api_hourly_cap on the live
// config and returns the audit detail naming the change.
func applyFleetAgentGitHubCap(cfg *config.Config, cap *int) string {
	before := fleetAgentGitHubCapLabel(cfg)
	cfg.SetFleetAgentGitHubAPIHourlyCap(cap)
	return auditDetail("agents.github_api_hourly_cap", before+" -> "+fleetAgentGitHubCapLabel(cfg))
}

func fleetAgentGitHubCapLabel(cfg *config.Config) string {
	label := agentGitHubCapLabel(cfg.FleetAgentGitHubAPIHourlyCap())
	if !cfg.AgentsGitHubAPIHourlyCapSet {
		label += " (default)"
	}
	return label
}

func agentGitHubCapLabel(n int) string {
	if n == 0 {
		return "0 (no cap)"
	}
	return strconv.Itoa(n)
}

func agentGitHubOverrideLabel(cap *int) string {
	if cap == nil {
		return "inherit"
	}
	return agentGitHubCapLabel(*cap)
}

// agentGitHubCapFromBody reads githubApiHourlyCap from a General-tab save:
// present=false when absent, a nil cap for null (inherit the fleet default),
// else a whole number of at least 0 (0 = no cap for this agent).
func agentGitHubCapFromBody(body map[string]interface{}) (cap *int, present bool, err error) {
	v, ok := body[agentGitHubCapField]
	if !ok {
		return nil, false, nil
	}
	if v == nil {
		return nil, true, nil
	}
	f, ok := v.(float64)
	if !ok || f < 0 || f != math.Trunc(f) || f > math.MaxInt32 {
		return nil, true, fmt.Errorf("%s must be a whole number of at least 0 (0 = no cap), or null to inherit the fleet default", agentGitHubCapField)
	}
	n := int(f)
	return &n, true, nil
}

// agentGitHubCapReplicaError refuses an override on a replica or review head:
// those are rebuilt from their base agent on every load, so the value would
// be dropped. The base agent's override covers them.
func agentGitHubCapReplicaError(name, base string) error {
	return fmt.Errorf("%s is a copy of agent %s and takes its GitHub API hourly cap from it; set %s on %s instead", name, base, agentGitHubCapField, base)
}

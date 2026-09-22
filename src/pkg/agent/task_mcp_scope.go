package agent

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/taskmcp"
)

func (m *Manager) ActiveLaunches() []taskmcp.LaunchScope {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]taskmcp.LaunchScope, 0, len(m.agents))
	for _, agent := range m.agents {
		if agent == nil || agent.State != StateRunning || agent.StartedAt == nil {
			continue
		}
		scope := m.launchScopeForAgentLocked(agent, agent.launchGen)
		if strings.TrimSpace(scope.TaskID) == "" || strings.TrimSpace(scope.Repo) == "" {
			continue
		}
		out = append(out, scope)
	}
	return out
}

func (m *Manager) launchScopeForAgentLocked(agent *AgentProcess, generation int) taskmcp.LaunchScope {
	if m == nil || agent == nil {
		return taskmcp.LaunchScope{}
	}
	repo := m.launchRepoForAgentLocked(agent.Name)
	if repo == "" {
		return taskmcp.LaunchScope{}
	}
	if generation < 0 {
		generation = 0
	}
	scope := taskmcp.LaunchScope{
		TaskID:     deterministicTaskMCPTaskID(agent.Name, repo, 0, generation),
		Repo:       repo,
		Agent:      agent.Name,
		Generation: uint64(generation),
	}
	if agent.StartedAt != nil {
		scope.StartedAt = *agent.StartedAt
	}
	return scope
}

func (m *Manager) launchRepoForAgentLocked(agentName string) string {
	if m == nil {
		return ""
	}
	pick := ""
	if m.project.AgentPrimaryRepo != nil {
		pick = m.project.AgentPrimaryRepo(agentName)
	}
	if pick == "" && m.project.AgentRepos != nil {
		repos := m.project.AgentRepos(agentName)
		if len(repos) > 0 {
			pick = repos[0]
		}
	}
	if pick == "" {
		pick = m.project.PrimaryRepo()
	}
	if pick == "" && len(m.project.Repos) > 0 {
		pick = m.project.Repos[0]
	}
	if pick == "" {
		return ""
	}
	return config.QualifyRepo(m.project.Org, pick)
}

func deterministicTaskMCPTaskID(agentName, repo string, number, generation int) string {
	return fmt.Sprintf("%s:%s#%d:%d", strings.TrimSpace(agentName), strings.TrimSpace(repo), number, generation)
}

package proxy

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/agent"
)

var autonomyRepoLevels sync.Map
var graphQLRepositoryRefRe = regexp.MustCompile(`(?is)repository\s*\(\s*owner\s*:\s*"([^"]+)"\s*,\s*name\s*:\s*"([^"]+)"`)

func SetAutonomyRepoLevel(repo string, level int) {
	repo = strings.TrimSpace(strings.ToLower(repo))
	if repo == "" || level <= 0 {
		return
	}
	autonomyRepoLevels.Store(repo, level)
}

func autonomyModeForRepo(agentName, repo string, fallback agent.AgentMode) agent.AgentMode {
	repo = strings.TrimSpace(strings.ToLower(repo))
	if repo == "" {
		return fallback
	}
	if v, ok := autonomyRepoLevels.Load(repo); ok {
		if level, ok := v.(int); ok && level > 0 {
			return agent.DefaultAgentMode(agentName, level)
		}
	}
	return fallback
}

func autonomyGraphQLMode(agentName string, body []byte, fallback agent.AgentMode) agent.AgentMode {
	if !hasAutonomyRepoLevels() {
		return fallback
	}
	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return fallback
	}
	query := strings.TrimSpace(req.Query)
	for _, match := range graphQLRepositoryRefRe.FindAllStringSubmatch(query, -1) {
		if len(match) == 3 {
			mode := autonomyModeForRepo(agentName, match[1]+"/"+match[2], fallback)
			if mode != fallback {
				return mode
			}
		}
	}
	if graphQLMergeMutationRe.MatchString(query) || graphQLPRWriteMutationRe.MatchString(query) {
		return agent.ModeAdvisory
	}
	return fallback
}

func hasAutonomyRepoLevels() bool {
	found := false
	autonomyRepoLevels.Range(func(_, _ any) bool {
		found = true
		return false
	})
	return found
}

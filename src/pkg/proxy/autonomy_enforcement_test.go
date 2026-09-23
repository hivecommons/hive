package proxy

import (
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

func TestAutonomyDemotedRepoPRWriteRefused(t *testing.T) {
	hiveMode := agent.DefaultAgentMode("scanner", 5)
	SetAutonomyRepoLevel("hivecommons/hive", 4)
	modeAfterDemotion := autonomyModeForRepo("scanner", "hivecommons/hive", hiveMode)
	if AllowedByMode(modeAfterDemotion, "POST", "/repos/hivecommons/hive/git/refs") {
		t.Fatal("demoted L4 scanner must not be allowed to create PR branches")
	}

	if !AllowedByMode(hiveMode, "POST", "/repos/hivecommons/other/git/refs") {
		t.Fatal("L5 scanner should still be able to create PR branches before demotion")
	}
}

func TestAutonomyDemotedRepoGraphQLPRWriteRefused(t *testing.T) {
	hiveMode := agent.DefaultAgentMode("scanner", 5)
	SetAutonomyRepoLevel("hivecommons/hive", 4)
	body := []byte(`{"query":"mutation { createPullRequest(input: {repositoryId: \"R\"}) { pullRequest { id } } }"}`)
	allowed, mutation := GraphQLAllowedCaps(autonomyGraphQLMode("scanner", body, hiveMode), agent.AgentCapabilities{}, body)
	if !mutation {
		t.Fatal("fixture must be classified as a mutation")
	}
	if allowed {
		t.Fatal("unresolved GraphQL PR write must fail closed while repo autonomy overrides are active")
	}
}

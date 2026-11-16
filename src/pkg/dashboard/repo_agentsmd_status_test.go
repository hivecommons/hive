package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/crux/agentsmd"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// #18495: the repo status payload carries the last AGENTS.md injection
// status, so a truncated file is visible without reading logs.
func TestBuildReposCarriesAgentsMdStatus(t *testing.T) {
	agentsmd.RecordStatus("acme18495/capped", agentsmd.InjectionStatus{
		SourceBytes: 50000, RenderedBytes: 32000, CapBytes: 32768,
		Truncated: true, OmittedSections: []string{"Style guide"}, OmittedBytes: 18000,
	})
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme18495", Repos: []string{"capped", "unread"}}}
	repos := buildRepos(cfg, nil, governor.State{})
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	got := repos[0].AgentsMd
	if got == nil || !got.Truncated || got.CapBytes != 32768 || got.SourceBytes != 50000 || len(got.OmittedSections) != 1 {
		t.Errorf("capped repo status wrong: %+v", got)
	}
	if repos[1].AgentsMd != nil {
		t.Errorf("repo without a recorded injection should carry no status: %+v", repos[1].AgentsMd)
	}
}

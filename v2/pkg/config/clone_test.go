package config

import (
	"bytes"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigCloneDetachesAllMutableConfigState(t *testing.T) {
	openPRs := true
	includeRepos := true
	channelEnabled := true
	synthEnabled := true
	retention := RetentionPolicy{MaxBeads: 10}
	level := 2
	cfg := &Config{
		Project: ProjectConfig{Repos: []string{"repo"}, OpenPRs: &openPRs},
		Agents: map[string]AgentConfig{"worker": {
			Aliases:        []string{"alias"},
			LaneKeywords:   []string{"lane"},
			DetectKeywords: []string{"detect"},
			IncludeRepos:   &includeRepos,
			StatsDisplay:   []StatsDisplayEntry{{Key: "stat"}},
			ACMMLevels:     []int{2},
			Channels: []ChannelConfig{{
				Enabled:  &channelEnabled,
				Events:   []string{"event"},
				Patterns: []string{"pattern"},
				Match:    map[string]string{"key": "match"},
				Repos:    []string{"repo"},
			}},
			Tools: &ToolsConfig{Rules: []ToolRule{{Pattern: "tool"}}},
			Connections: []ConnectionConfig{{
				Auth:    &ConnectionAuth{Type: "env"},
				Options: map[string]string{"key": "option"},
			}},
		}},
		Governor: GovernorConfig{
			Modes:  map[string]ModeConfig{"idle": {Cadences: map[string]string{"worker": "1m"}}},
			Labels: LabelsConfig{Exempt: []string{"hold"}},
			Sensing: SensingConfig{
				GHRatePatterns:     []string{"rate"},
				CLIExcludePatterns: []string{"exclude"},
				LoginPatterns:      []string{"login"},
			},
		},
		Notifications: NotificationsConfig{
			Ntfy:    &NtfyConfig{Topic: "topic"},
			Slack:   &SlackConfig{Webhook: "slack"},
			Discord: &DiscordConfig{Webhook: "discord"},
		},
		Dashboard: DashboardConfig{AuthorizedUsers: []string{"owner"}},
		Knowledge: KnowledgeConfig{
			Layers:     []KnowledgeLayer{{Type: "project"}},
			Vaults:     []VaultConfig{{Name: "vault"}},
			GitSources: []GitSourceConfigYAML{{Name: "git"}},
			Documents:  []DocSourceConfigYAML{{Name: "doc"}},
			Curator:    KnowledgeCurator{ExtractFrom: []string{"beads"}},
			Primer:     KnowledgePrimer{Priority: []string{"regression"}},
			BeadSynthesizer: BeadSynthesizerConfig{
				Enabled:         &synthEnabled,
				RetentionPolicy: &retention,
			},
		},
		Hub: HubConfig{
			ContributeAllowLabels: []string{"allow"},
			ContributeDenyLabels:  []string{"deny"},
			ContributeDenyTitles:  []string{"title"},
			ContributeDenyAuthors: []string{"author"},
			ContributeAllowModels: []string{"model"},
			DisabledRepos:         []string{"repo"},
			DisabledTiers:         []string{"tier"},
			TierLimits:            map[string]TierRate{"tier": {MaxPerHour: 1}},
		},
		ACMMLevel: &level,
	}

	expected, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cloned := cfg.Clone()
	cloned.Project.Repos[0] = "changed"
	*cloned.Project.OpenPRs = false
	agent := cloned.Agents["worker"]
	agent.Aliases[0] = "changed"
	agent.LaneKeywords[0] = "changed"
	agent.DetectKeywords[0] = "changed"
	*agent.IncludeRepos = false
	agent.StatsDisplay[0].Key = "changed"
	agent.ACMMLevels[0] = 6
	*agent.Channels[0].Enabled = false
	agent.Channels[0].Events[0] = "changed"
	agent.Channels[0].Patterns[0] = "changed"
	agent.Channels[0].Match["key"] = "changed"
	agent.Channels[0].Repos[0] = "changed"
	agent.Tools.Rules[0].Pattern = "changed"
	agent.Connections[0].Auth.Type = "changed"
	agent.Connections[0].Options["key"] = "changed"
	cloned.Agents["worker"] = agent
	mode := cloned.Governor.Modes["idle"]
	mode.Cadences["worker"] = "changed"
	cloned.Governor.Modes["idle"] = mode
	cloned.Governor.Labels.Exempt[0] = "changed"
	cloned.Governor.Sensing.GHRatePatterns[0] = "changed"
	cloned.Governor.Sensing.CLIExcludePatterns[0] = "changed"
	cloned.Governor.Sensing.LoginPatterns[0] = "changed"
	cloned.Notifications.Ntfy.Topic = "changed"
	cloned.Notifications.Slack.Webhook = "changed"
	cloned.Notifications.Discord.Webhook = "changed"
	cloned.Dashboard.AuthorizedUsers[0] = "changed"
	cloned.Knowledge.Layers[0].Type = "changed"
	cloned.Knowledge.Vaults[0].Name = "changed"
	cloned.Knowledge.GitSources[0].Name = "changed"
	cloned.Knowledge.Documents[0].Name = "changed"
	cloned.Knowledge.Curator.ExtractFrom[0] = "changed"
	cloned.Knowledge.Primer.Priority[0] = "changed"
	*cloned.Knowledge.BeadSynthesizer.Enabled = false
	cloned.Knowledge.BeadSynthesizer.RetentionPolicy.MaxBeads = 99
	cloned.Hub.ContributeAllowLabels[0] = "changed"
	cloned.Hub.ContributeDenyLabels[0] = "changed"
	cloned.Hub.ContributeDenyTitles[0] = "changed"
	cloned.Hub.ContributeDenyAuthors[0] = "changed"
	cloned.Hub.ContributeAllowModels[0] = "changed"
	cloned.Hub.DisabledRepos[0] = "changed"
	cloned.Hub.DisabledTiers[0] = "changed"
	cloned.Hub.TierLimits["tier"] = TierRate{MaxPerHour: 99}
	*cloned.ACMMLevel = 6

	actual, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("mutating clone changed source config:\n got:\n%s\nwant:\n%s", actual, expected)
	}
}

func TestConfigCloneDetachesVariables(t *testing.T) {
	defaultValue := "fallback"
	cfg := &Config{Variables: VariablesConfig{
		Security: VarSecurityConfig{HTTPAllowlist: []string{"api.example.com"}},
		Defs: map[string]VarDef{
			"REMOTE": {
				Default: &defaultValue,
				Command: []string{"fetch", "--safe"},
				Headers: map[string]string{"X-Trace": "source"},
			},
		},
	}}

	cloned := cfg.Clone()
	cloned.Variables.Security.HTTPAllowlist[0] = "attacker.example.com"
	definition := cloned.Variables.Defs["REMOTE"]
	*definition.Default = "changed"
	definition.Command[0] = "execute"
	definition.Headers["X-Trace"] = "changed"
	cloned.Variables.Defs["REMOTE"] = definition
	cloned.Variables.Defs["ADDED"] = VarDef{Value: "new"}

	if got := cfg.Variables.Security.HTTPAllowlist[0]; got != "api.example.com" {
		t.Fatalf("mutating cloned HTTP allowlist changed source: got %q", got)
	}
	sourceDefinition := cfg.Variables.Defs["REMOTE"]
	if got := *sourceDefinition.Default; got != "fallback" {
		t.Fatalf("mutating cloned default changed source: got %q", got)
	}
	if got := sourceDefinition.Command[0]; got != "fetch" {
		t.Fatalf("mutating cloned command changed source: got %q", got)
	}
	if got := sourceDefinition.Headers["X-Trace"]; got != "source" {
		t.Fatalf("mutating cloned headers changed source: got %q", got)
	}
	if _, exists := cfg.Variables.Defs["ADDED"]; exists {
		t.Fatal("adding a cloned variable definition changed source")
	}
}

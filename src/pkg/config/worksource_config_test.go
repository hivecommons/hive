package config

import (
	"encoding/json"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWorkSourceConfig_YAMLRoundTrip marshals and unmarshals a fully populated
// WorkSourceConfig for each source type and verifies every field survives.
func TestWorkSourceConfig_YAMLRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		cfg  WorkSourceConfig
	}{
		{"github_projects", WorkSourceConfig{
			Type: "github_projects",
			GitHubProjects: GitHubProjectsSourceConfig{
				ProjectNumber:  12,
				Org:            "my-org",
				States:         []string{"Todo", "In Progress"},
				PriorityField:  "Priority",
				IterationField: "Sprint",
				DefaultRepo:    "my-org/my-repo",
			},
		}},
		{"linear", WorkSourceConfig{
			Type: "linear",
			Linear: LinearSourceConfig{
				APIKey: "lin_key",
				Teams: []LinearTeamSourceConfig{
					{Key: "ENG", Repo: "my-org/my-repo", States: []string{"Todo", "Backlog"}},
				},
				HoldLabels: []string{"hold", "blocked"},
			},
		}},
		{"jira", WorkSourceConfig{
			Type: "jira",
			Jira: JiraSourceConfig{
				BaseURL:     "https://myorg.atlassian.net",
				Email:       "ops@example.com",
				APIToken:    "tok",
				ProjectKeys: []string{"ENG", "OPS"},
				JQL:         "project = ENG",
				Repo:        "my-org/my-repo",
				HoldLabels:  []string{"hold"},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := yaml.Marshal(tc.cfg)
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}
			var got WorkSourceConfig
			if err := yaml.Unmarshal(data, &got); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(got, tc.cfg) {
				t.Errorf("yaml round trip mismatch:\n got %+v\nwant %+v", got, tc.cfg)
			}

			jdata, err := json.Marshal(tc.cfg)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var jgot WorkSourceConfig
			if err := json.Unmarshal(jdata, &jgot); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(jgot, tc.cfg) {
				t.Errorf("json round trip mismatch:\n got %+v\nwant %+v", jgot, tc.cfg)
			}
		})
	}
}

// TestGovernorConfig_WorkSourceAbsent pins backward compatibility: a governor
// block with no work_source key yields the zero WorkSourceConfig (Type ""),
// which the factory maps to the default GitHub Issues source.
func TestGovernorConfig_WorkSourceAbsent(t *testing.T) {
	var g GovernorConfig
	if err := yaml.Unmarshal([]byte("eval_interval_s: 60\n"), &g); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if g.WorkSource.Type != "" {
		t.Errorf("WorkSource.Type = %q, want empty (GitHub Issues default)", g.WorkSource.Type)
	}
}

// TestGovernorConfig_WorkSourceParsed verifies the work_source block parses
// from a governor-level YAML document.
func TestGovernorConfig_WorkSourceParsed(t *testing.T) {
	src := `
work_source:
  type: linear
  linear:
    api_key: lin_key
    teams:
      - key: ENG
        repo: my-org/my-repo
        states: [Todo]
    hold_labels: [hold]
`
	var g GovernorConfig
	if err := yaml.Unmarshal([]byte(src), &g); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if g.WorkSource.Type != "linear" {
		t.Fatalf("Type = %q, want linear", g.WorkSource.Type)
	}
	if g.WorkSource.Linear.APIKey != "lin_key" {
		t.Errorf("APIKey = %q", g.WorkSource.Linear.APIKey)
	}
	if len(g.WorkSource.Linear.Teams) != 1 || g.WorkSource.Linear.Teams[0].Repo != "my-org/my-repo" {
		t.Errorf("Teams = %+v", g.WorkSource.Linear.Teams)
	}
}

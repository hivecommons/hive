package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

const opsFeaturesPath = "/api/config/governor/features"

// The issues, jev, duplicate_sweep, policies and turn keys round-trip through
// the Features PUT/GET; invalid values are rejected without changing anything,
// and an absent key leaves its value untouched.
func TestGovernorFeaturesOpsSettingsRoundTrip(t *testing.T) {
	s := covApiServer(t)
	beforeDupEnabled := s.deps.Config.DuplicateSweep.Enabled
	beforeJev := s.deps.Config.Jev

	for _, bad := range []map[string]any{
		{"issuesCloseOnMergeInterval": "soon"},
		{"issuesCloseOnMergeBackfillInterval": "-1m"},
		{"issuesCloseOnMergeRecheck": "1 day"},
		{"jevProvider": "acme"},
		{"jevEndpoint": "ftp://jev.example"},
		{"jevApiKeyEnv": "sk-not-a-name"},
		{"jevTimeout": "-5s"},
		{"duplicateSweepMaxComments": -1},
		{"duplicateSweepMaxPRsPerRepo": -1},
		{"policiesLocalDir": "relative/dir"},
		{"policiesPath": "../outside"},
		{"policiesPollInterval": "often"},
		{"policiesBranch": "-x"},
		{"prFollowUpMaxAge": "0s"},
		{"prFollowUpResumeIdMaxAge": "-1h"},
		{"prFollowUpRetention": "forever"},
		{"duplicateSweepEnabled": true, "jevTimeout": "-1s"},
	} {
		if rec := doPut(s, opsFeaturesPath, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d", bad, rec.Code)
		}
	}
	if s.deps.Config.DuplicateSweep.Enabled != beforeDupEnabled || s.deps.Config.Jev != beforeJev {
		t.Fatal("a rejected PUT changed the config")
	}

	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"issuesCloseOnMerge":                 false,
		"issuesCloseOnMergeInterval":         "15m",
		"issuesCloseOnMergeBackfillInterval": "2h",
		"issuesCloseOnMergeRecheck":          "48h",
		"issuesReporterConfirmation":         true,

		"jevProvider":  " TypeSafe ",
		"jevModel":     "jev-next",
		"jevEndpoint":  "https://jev.example/v1/systemone",
		"jevApiKeyEnv": "MY_JEV_KEY",
		"jevTimeout":   "7s",

		"duplicateSweepEnabled":       true,
		"duplicateSweepPostComments":  true,
		"duplicateSweepMaxComments":   3,
		"duplicateSweepMaxPRsPerRepo": 50,

		"policiesRepo":         "https://github.com/example/hive-config",
		"policiesBranch":       "stable",
		"policiesPath":         "agents/",
		"policiesLocalDir":     "/data/policies",
		"policiesPollInterval": "10m",

		"prFollowUpMaxAge":         "12h",
		"prFollowUpResumeIdMaxAge": "96h",
		"prFollowUpRetention":      "336h",

		"reentrantTurnEnabled":                true,
		"reentrantTurnBackgroundFleetEnabled": true,
	}); rec.Code != http.StatusOK {
		t.Fatalf("ops PUT: %d — %s", rec.Code, rec.Body.String())
	}
	c := s.deps.Config
	if c.Issues.CloseOnMergeEnabled() || !c.Issues.ReporterConfirmationEnabled() ||
		c.Issues.CloseOnMergeInterval != 15*time.Minute || c.Issues.CloseOnMergeBackfillInterval != 2*time.Hour ||
		c.Issues.CloseOnMergeRecheck != 48*time.Hour {
		t.Fatalf("issues not applied: %+v", c.Issues)
	}
	if c.Jev != (config.JevConfig{Provider: "typesafe", Model: "jev-next", Endpoint: "https://jev.example/v1/systemone", APIKeyEnv: "MY_JEV_KEY", Timeout: 7 * time.Second}) {
		t.Fatalf("jev not applied: %+v", c.Jev)
	}
	if !c.DuplicateSweep.Enabled || !c.DuplicateSweep.PostComments || c.DuplicateSweep.MaxComments != 3 || c.DuplicateSweep.MaxPRsPerRepo != 50 {
		t.Fatalf("duplicate sweep not applied: %+v", c.DuplicateSweep)
	}
	if c.Policies != (config.PoliciesConfig{Repo: "https://github.com/example/hive-config", Branch: "stable", Path: "agents/", LocalDir: "/data/policies", PollInterval: 10 * time.Minute}) {
		t.Fatalf("policies not applied: %+v", c.Policies)
	}
	if c.Turn.PRFollowUp.MaxAge != "12h" || c.Turn.PRFollowUp.ResumeIDMaxAge != "96h" || c.Turn.PRFollowUp.Retention != "336h" {
		t.Fatalf("pr follow-up windows not applied: %+v", c.Turn.PRFollowUp)
	}
	if !c.Turn.Reentrant.Enabled || !c.Turn.Reentrant.BackgroundFleetEnabled {
		t.Fatalf("reentrant gates not applied: %+v", c.Turn.Reentrant)
	}

	// A partial PUT changes only what it carries; empty strings reset to the
	// default.
	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"jevTimeout":                 "",
		"issuesCloseOnMergeInterval": "",
		"prFollowUpMaxAge":           "",
		"duplicateSweepMaxComments":  0,
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial PUT: %d — %s", rec.Code, rec.Body.String())
	}
	c = s.deps.Config
	if c.Jev.Timeout != 0 || c.Jev.Model != "jev-next" || c.Issues.CloseOnMergeInterval != 0 ||
		c.Issues.CloseOnMergeRecheck != 48*time.Hour || c.Turn.PRFollowUp.MaxAge != "" ||
		c.Turn.PRFollowUp.Retention != "336h" || c.DuplicateSweep.MaxComments != 0 || c.DuplicateSweep.MaxPRsPerRepo != 50 {
		t.Fatalf("partial PUT changed unrelated keys or did not reset: jev=%+v issues=%+v turn=%+v dup=%+v", c.Jev, c.Issues, c.Turn, c.DuplicateSweep)
	}

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d", rec.Code)
	}
	var payload struct {
		Features struct {
			IssuesCloseOnMerge           bool              `json:"issuesCloseOnMerge"`
			IssuesCloseOnMergeInterval   string            `json:"issuesCloseOnMergeInterval"`
			IssuesCloseOnMergeBackfill   string            `json:"issuesCloseOnMergeBackfillInterval"`
			IssuesCloseOnMergeRecheck    string            `json:"issuesCloseOnMergeRecheck"`
			IssuesReporterConfirmation   bool              `json:"issuesReporterConfirmation"`
			IssuesDefaults               map[string]string `json:"issuesDefaults"`
			JevProvider                  string            `json:"jevProvider"`
			JevModel                     string            `json:"jevModel"`
			JevEndpoint                  string            `json:"jevEndpoint"`
			JevAPIKeyEnv                 string            `json:"jevApiKeyEnv"`
			JevTimeout                   string            `json:"jevTimeout"`
			JevDefaults                  map[string]string `json:"jevDefaults"`
			DuplicateSweepEnabled        bool              `json:"duplicateSweepEnabled"`
			DuplicateSweepPostComments   bool              `json:"duplicateSweepPostComments"`
			DuplicateSweepMaxComments    int               `json:"duplicateSweepMaxComments"`
			DuplicateSweepMaxPRsPerRepo  int               `json:"duplicateSweepMaxPRsPerRepo"`
			DuplicateSweepDefaults       map[string]int    `json:"duplicateSweepDefaults"`
			PoliciesRepo                 string            `json:"policiesRepo"`
			PoliciesBranch               string            `json:"policiesBranch"`
			PoliciesPath                 string            `json:"policiesPath"`
			PoliciesLocalDir             string            `json:"policiesLocalDir"`
			PoliciesPollInterval         string            `json:"policiesPollInterval"`
			PRFollowUpMaxAge             string            `json:"prFollowUpMaxAge"`
			PRFollowUpResumeIDMaxAge     string            `json:"prFollowUpResumeIdMaxAge"`
			PRFollowUpRetention          string            `json:"prFollowUpRetention"`
			PRFollowUpDefaults           map[string]string `json:"prFollowUpDefaults"`
			ReentrantTurnEnabled         bool              `json:"reentrantTurnEnabled"`
			ReentrantTurnBackgroundFleet bool              `json:"reentrantTurnBackgroundFleetEnabled"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	f := payload.Features
	if f.IssuesCloseOnMerge || !f.IssuesReporterConfirmation || f.IssuesCloseOnMergeInterval != "" ||
		f.IssuesCloseOnMergeBackfill != "2h0m0s" || f.IssuesCloseOnMergeRecheck != "48h0m0s" ||
		f.IssuesDefaults["issuesCloseOnMergeInterval"] != config.DefaultCloseOnMergeInterval.String() {
		t.Fatalf("issues payload = %+v", f)
	}
	if f.JevProvider != "typesafe" || f.JevModel != "jev-next" || f.JevEndpoint != "https://jev.example/v1/systemone" ||
		f.JevAPIKeyEnv != "MY_JEV_KEY" || f.JevTimeout != "" || f.JevDefaults["jevApiKeyEnv"] != config.JevDefaultAPIKeyEnv ||
		f.JevDefaults["jevTimeout"] != "5s" {
		t.Fatalf("jev payload = %+v", f)
	}
	if !f.DuplicateSweepEnabled || !f.DuplicateSweepPostComments || f.DuplicateSweepMaxComments != 0 || f.DuplicateSweepMaxPRsPerRepo != 50 ||
		f.DuplicateSweepDefaults["duplicateSweepMaxComments"] != github.DefaultDuplicateSweepMaxComments {
		t.Fatalf("duplicate sweep payload = %+v", f)
	}
	if f.PoliciesRepo != "https://github.com/example/hive-config" || f.PoliciesBranch != "stable" || f.PoliciesPath != "agents/" ||
		f.PoliciesLocalDir != "/data/policies" || f.PoliciesPollInterval != "10m0s" {
		t.Fatalf("policies payload = %+v", f)
	}
	if f.PRFollowUpMaxAge != "" || f.PRFollowUpResumeIDMaxAge != "96h" || f.PRFollowUpRetention != "336h" ||
		f.PRFollowUpDefaults["prFollowUpMaxAge"] != config.DefaultPRFollowUpMaxAge.String() ||
		!f.ReentrantTurnEnabled || !f.ReentrantTurnBackgroundFleet {
		t.Fatalf("turn payload = %+v", f)
	}
}

// Every newly exposed key is rendered on the Features tab with its YAML-key
// hint and a control bound to the Features dirty map.
func TestOpsSettingsRenderEveryKey(t *testing.T) {
	body := extractJSFunction(t, indexHTML(t), "renderGovFeatures")
	for _, c := range []struct{ yaml, control string }{
		{"issues.close_on_merge", `data-section="features" data-key="issuesCloseOnMerge"`},
		{"issues.reporter_confirmation", `data-section="features" data-key="issuesReporterConfirmation"`},
		{"issues.close_on_merge_interval", `data-arg0="features" data-arg1="issuesCloseOnMergeInterval" data-arg-types="s,s,w"`},
		{"issues.close_on_merge_backfill_interval", `data-arg0="features" data-arg1="issuesCloseOnMergeBackfillInterval" data-arg-types="s,s,w"`},
		{"issues.close_on_merge_recheck", `data-arg0="features" data-arg1="issuesCloseOnMergeRecheck" data-arg-types="s,s,w"`},
		{"jev.provider", `data-arg0="features" data-arg1="jevProvider" data-arg-types="s,s,v"`},
		{"jev.model", `data-arg0="features" data-arg1="jevModel" data-arg-types="s,s,w"`},
		{"jev.endpoint", `data-arg0="features" data-arg1="jevEndpoint" data-arg-types="s,s,w"`},
		{"jev.api_key_env", `data-arg0="features" data-arg1="jevApiKeyEnv" data-arg-types="s,s,w"`},
		{"jev.timeout", `data-arg0="features" data-arg1="jevTimeout" data-arg-types="s,s,w"`},
		{"duplicate_sweep.enabled", `data-section="features" data-key="duplicateSweepEnabled"`},
		{"duplicate_sweep.post_comments", `data-section="features" data-key="duplicateSweepPostComments"`},
		{"duplicate_sweep.max_comments", `data-arg0="features" data-arg1="duplicateSweepMaxComments" data-arg-types="s,s,N"`},
		{"duplicate_sweep.max_prs_per_repo", `data-arg0="features" data-arg1="duplicateSweepMaxPRsPerRepo" data-arg-types="s,s,N"`},
		{"good_first_issue.enabled", `data-section="features" data-key="goodFirstIssueEnabled"`},
		{"good_first_issue.max_per_repo", `data-arg0="features" data-arg1="goodFirstIssueMaxPerRepo" data-arg-types="s,s,N"`},
		{"good_first_issue.max_per_sweep", `data-arg0="features" data-arg1="goodFirstIssueMaxPerSweep" data-arg-types="s,s,N"`},
		{"good_first_issue.min_age", `data-arg0="features" data-arg1="goodFirstIssueMinAge" data-arg-types="s,s,w"`},
		{"good_first_issue.interval", `data-arg0="features" data-arg1="goodFirstIssueInterval" data-arg-types="s,s,w"`},
		{"policies.repo", `data-arg0="features" data-arg1="policiesRepo" data-arg-types="s,s,w"`},
		{"policies.branch", `data-arg0="features" data-arg1="policiesBranch" data-arg-types="s,s,w"`},
		{"policies.path", `data-arg0="features" data-arg1="policiesPath" data-arg-types="s,s,w"`},
		{"policies.local_dir", `data-arg0="features" data-arg1="policiesLocalDir" data-arg-types="s,s,w"`},
		{"policies.poll_interval", `data-arg0="features" data-arg1="policiesPollInterval" data-arg-types="s,s,w"`},
		{"turn.pr_follow_up.max_age", `data-arg0="features" data-arg1="prFollowUpMaxAge" data-arg-types="s,s,w"`},
		{"turn.pr_follow_up.resume_id_max_age", `data-arg0="features" data-arg1="prFollowUpResumeIdMaxAge" data-arg-types="s,s,w"`},
		{"turn.pr_follow_up.retention", `data-arg0="features" data-arg1="prFollowUpRetention" data-arg-types="s,s,w"`},
		{"turn.reentrant.enabled", `data-section="features" data-key="reentrantTurnEnabled"`},
		{"turn.reentrant.background_fleet_enabled", `data-section="features" data-key="reentrantTurnBackgroundFleetEnabled"`},
	} {
		if !strings.Contains(body, "yamlHint('"+c.yaml+"')") {
			t.Errorf("%s has no YAML-key hint", c.yaml)
		}
		if !strings.Contains(body, c.control) {
			t.Errorf("%s has no Features control (%s)", c.yaml, c.control)
		}
	}
	if !strings.Contains(body, "every policy repo change requires a restart") {
		t.Error("policy repo section must say its changes require a restart")
	}
}

// The data, deployment, evidence, forge, mint, supersession, compliance,
// planning, swarm and tool_approval keys round-trip through the Features
// PUT/GET; invalid values are rejected without changing anything, an absent key
// leaves its value untouched, and the read-only key paths are never written.
func TestGovernorFeaturesPlatformSettingsRoundTrip(t *testing.T) {
	s := covApiServer(t)
	root := t.TempDir()
	s.deps.Config.Mint.KeyPath = "/keys/mint.pem"
	s.deps.Config.Evidence.SigningKeyFile = "/keys/evidence.key"
	s.deps.Config.Deployment.UpgradeHelper = "/usr/local/libexec/helper"
	beforeData := s.deps.Config.Data
	beforeGitLab := s.deps.Config.GitLab

	for _, bad := range []map[string]any{
		{"dataAgentsDir": ""},
		{"dataLogsDir": "relative/logs"},
		{"dataMetricsDir": "/data/../etc"},
		{"dataClaudeSessionsDir": "claude"},
		{"dataCopilotSessionsDir": "/data/copilot/"},
		{"dataBobSessionsDir": "bob"},
		{"dataSessionRetentionDays": "-1"},
		{"dataSessionRetentionDays": "a week"},
		{"deploymentPodmanMode": "sometimes"},
		{"deploymentUpgradeRequestDir": "requests"},
		{"giteaUrl": "ftp://gitea.example"},
		{"gitlabUrl": "gitlab.example.com"},
		{"giteaTokenEnv": "glpat-not-a-name"},
		{"gitlabTokenEnv": "has space"},
		{"mintMaxTtlSeconds": -1},
		{"mintMaxTtlSeconds": 7200},
		{"supersessionGracePeriod": "-1h"},
		{"compliancePostureWindowDays": 91},
		{"compliancePostureHistoryDays": 10},
		{"planningDesignApprovedLabel": "<b>ok</b>"},
		{"planningMaxConcurrentDesigns": -1},
		{"planningMaxDesignRevisions": -1},
		{"swarmEventName": strings.Repeat("x", maxSwarmTextLen+1)},
		{"toolApprovalInboxPath": "inbox.json"},
		{"dataLogsDir": "/data/new-logs", "gitlabUrl": "nope"},
	} {
		if rec := doPut(s, opsFeaturesPath, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d", bad, rec.Code)
		}
	}
	if s.deps.Config.Data.LogsDir != beforeData.LogsDir || s.deps.Config.GitLab != beforeGitLab {
		t.Fatal("a rejected PUT changed the config")
	}

	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"dataAgentsDir":            root + "/agent-configs",
		"dataBobSessionsDir":       root + "/bob",
		"dataClaudeSessionsDir":    root + "/claude",
		"dataCopilotSessionsDir":   root + "/copilot",
		"dataLogsDir":              root + "/logs",
		"dataMetricsDir":           root + "/metrics",
		"dataSessionRetentionDays": "0",

		"deploymentPodmanMode":        " Rootless ",
		"deploymentUpgradeRequestDir": "/run/hive-upgrade",
		"deploymentUpgradeHelper":     "/elsewhere/helper",

		"evidenceEnabled":        false,
		"evidenceSigningKeyFile": "/elsewhere/other.key",

		"giteaUrl":       "https://codeberg.org/",
		"giteaTokenEnv":  "MY_GITEA_TOKEN",
		"gitlabUrl":      "https://gitlab.example.com",
		"gitlabTokenEnv": "MY_GITLAB_TOKEN",

		"mintKeyPath":       "/elsewhere/other.pem",
		"mintMaxTtlSeconds": 1800,

		"supersessionGracePeriod": "48h",

		"compliancePostureWindowDays":  60,
		"compliancePostureHistoryDays": 730,

		"planningDesignApprovedLabel":  "design-ok",
		"planningMaxConcurrentDesigns": 5,
		"planningMaxDesignRevisions":   2,

		"swarmEventName":        "Bug Bash",
		"swarmCallToArms":       "Squash them all",
		"swarmLeaderboardTitle": "Top squashers",

		"toolApprovalInboxPath": "/data/approvals/custom.json",
	}); rec.Code != http.StatusOK {
		t.Fatalf("platform PUT: %d — %s", rec.Code, rec.Body.String())
	}
	c := s.deps.Config
	if c.Data.AgentsDir != root+"/agent-configs" || c.Data.BobSessionsDir != root+"/bob" ||
		c.Data.ClaudeSessionsDir != root+"/claude" || c.Data.CopilotSessionsDir != root+"/copilot" ||
		c.Data.LogsDir != root+"/logs" || c.Data.MetricsDir != root+"/metrics" ||
		c.Data.SessionRetentionDays == nil || *c.Data.SessionRetentionDays != 0 {
		t.Fatalf("data not applied: %+v", c.Data)
	}
	if c.Deployment.PodmanMode != "rootless" || c.Deployment.UpgradeRequestDir != "/run/hive-upgrade" ||
		c.Deployment.UpgradeHelper != "/usr/local/libexec/helper" {
		t.Fatalf("deployment not applied (or upgrade_helper written): %+v", c.Deployment)
	}
	if c.Evidence.IsEnabled() || c.Evidence.SigningKeyFile != "/keys/evidence.key" {
		t.Fatalf("evidence not applied (or signing_key_file written): %+v", c.Evidence)
	}
	if c.Gitea != (config.GiteaConfig{URL: "https://codeberg.org", TokenEnv: "MY_GITEA_TOKEN"}) ||
		c.GitLab != (config.GitLabConfig{URL: "https://gitlab.example.com", TokenEnv: "MY_GITLAB_TOKEN"}) {
		t.Fatalf("forges not applied: gitea=%+v gitlab=%+v", c.Gitea, c.GitLab)
	}
	if c.Mint.MaxTTLSeconds != 1800 || c.Mint.KeyPath != "/keys/mint.pem" {
		t.Fatalf("mint not applied (or key_path written): %+v", c.Mint)
	}
	if c.SupersessionSweep.GracePeriod != 48*time.Hour {
		t.Fatalf("supersession grace not applied: %+v", c.SupersessionSweep)
	}
	if c.Compliance.PostureChecks.WindowDays != 60 || c.Compliance.PostureChecks.HistoryDays != 730 {
		t.Fatalf("compliance posture windows not applied: %+v", c.Compliance.PostureChecks)
	}
	if c.Planning.DesignApprovedLabel != "design-ok" || c.Planning.MaxConcurrentDesigns != 5 || c.Planning.MaxDesignRevisions != 2 {
		t.Fatalf("planning not applied: %+v", c.Planning)
	}
	if c.Swarm.EventName != "Bug Bash" || c.Swarm.CallToArms != "Squash them all" || c.Swarm.LeaderboardTitle != "Top squashers" {
		t.Fatalf("swarm not applied: %+v", c.Swarm)
	}
	if c.ToolApproval.InboxPath != "/data/approvals/custom.json" {
		t.Fatalf("tool approval inbox not applied: %+v", c.ToolApproval)
	}

	// A partial PUT changes only what it carries; empty values reset to the
	// default.
	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"dataSessionRetentionDays": "",
		"deploymentPodmanMode":     "",
		"gitlabUrl":                "",
		"mintMaxTtlSeconds":        0,
		"supersessionGracePeriod":  "",
		"toolApprovalInboxPath":    "",
	}); rec.Code != http.StatusOK {
		t.Fatalf("partial PUT: %d — %s", rec.Code, rec.Body.String())
	}
	c = s.deps.Config
	if c.Data.SessionRetentionDays != nil || c.Data.LogsDir != root+"/logs" || c.Deployment.PodmanMode != "" ||
		c.Deployment.UpgradeRequestDir != "/run/hive-upgrade" || c.GitLab.URL != "" || c.GitLab.TokenEnv != "MY_GITLAB_TOKEN" ||
		c.Mint.MaxTTLSeconds != 0 || c.SupersessionSweep.GracePeriod != 0 || c.ToolApproval.InboxPath != "" ||
		c.Planning.MaxDesignRevisions != 2 || c.Swarm.EventName != "Bug Bash" {
		t.Fatalf("partial PUT changed unrelated keys or did not reset: data=%+v deployment=%+v gitlab=%+v mint=%+v", c.Data, c.Deployment, c.GitLab, c.Mint)
	}

	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d", rec.Code)
	}
	var payload struct {
		Features struct {
			DataAgentsDir                string         `json:"dataAgentsDir"`
			DataSessionRetentionDays     string         `json:"dataSessionRetentionDays"`
			DeploymentPodmanMode         string         `json:"deploymentPodmanMode"`
			DeploymentUpgradeRequestDir  string         `json:"deploymentUpgradeRequestDir"`
			DeploymentUpgradeHelper      string         `json:"deploymentUpgradeHelper"`
			EvidenceEnabled              bool           `json:"evidenceEnabled"`
			EvidenceSigningKeyFile       string         `json:"evidenceSigningKeyFile"`
			GiteaURL                     string         `json:"giteaUrl"`
			GiteaTokenEnv                string         `json:"giteaTokenEnv"`
			GitLabURL                    string         `json:"gitlabUrl"`
			GitLabTokenEnv               string         `json:"gitlabTokenEnv"`
			MintKeyPath                  string         `json:"mintKeyPath"`
			MintMaxTTLSeconds            int            `json:"mintMaxTtlSeconds"`
			SupersessionGracePeriod      string         `json:"supersessionGracePeriod"`
			CompliancePostureWindowDays  int            `json:"compliancePostureWindowDays"`
			CompliancePostureHistoryDays int            `json:"compliancePostureHistoryDays"`
			PlanningDesignApprovedLabel  string         `json:"planningDesignApprovedLabel"`
			PlanningMaxConcurrentDesigns int            `json:"planningMaxConcurrentDesigns"`
			PlanningMaxDesignRevisions   int            `json:"planningMaxDesignRevisions"`
			SwarmEventName               string         `json:"swarmEventName"`
			SwarmCallToArms              string         `json:"swarmCallToArms"`
			SwarmLeaderboardTitle        string         `json:"swarmLeaderboardTitle"`
			ToolApprovalInboxPath        string         `json:"toolApprovalInboxPath"`
			PlatformDefaults             map[string]any `json:"platformDefaults"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding governor payload: %v", err)
	}
	f := payload.Features
	if f.DataAgentsDir != root+"/agent-configs" || f.DataSessionRetentionDays != "" {
		t.Fatalf("data payload = %+v", f)
	}
	if f.DeploymentPodmanMode != "" || f.DeploymentUpgradeRequestDir != "/run/hive-upgrade" || f.DeploymentUpgradeHelper != "/usr/local/libexec/helper" {
		t.Fatalf("deployment payload = %+v", f)
	}
	if f.EvidenceEnabled || f.EvidenceSigningKeyFile != "/keys/evidence.key" || f.MintKeyPath != "/keys/mint.pem" || f.MintMaxTTLSeconds != 0 {
		t.Fatalf("evidence/mint payload = %+v", f)
	}
	if f.GiteaURL != "https://codeberg.org" || f.GiteaTokenEnv != "MY_GITEA_TOKEN" || f.GitLabURL != "" || f.GitLabTokenEnv != "MY_GITLAB_TOKEN" {
		t.Fatalf("forge payload = %+v", f)
	}
	if f.SupersessionGracePeriod != "" || f.CompliancePostureWindowDays != 60 || f.CompliancePostureHistoryDays != 730 {
		t.Fatalf("supersession/compliance payload = %+v", f)
	}
	if f.PlanningDesignApprovedLabel != "design-ok" || f.PlanningMaxConcurrentDesigns != 5 || f.PlanningMaxDesignRevisions != 2 {
		t.Fatalf("planning payload = %+v", f)
	}
	if f.SwarmEventName != "Bug Bash" || f.SwarmCallToArms != "Squash them all" || f.SwarmLeaderboardTitle != "Top squashers" || f.ToolApprovalInboxPath != "" {
		t.Fatalf("swarm/tool approval payload = %+v", f)
	}
	d := f.PlatformDefaults
	if d["gitlabUrl"] != config.DefaultGitLabURL || d["gitlabTokenEnv"] != config.DefaultGitLabTokenEnv ||
		d["supersessionGracePeriod"] != github.DefaultSupersessionGracePeriod.String() ||
		d["mintMaxTtlSeconds"] != float64(900) || d["planningDesignApprovedLabel"] != config.DefaultDesignApprovedLabel {
		t.Fatalf("platform defaults = %+v", d)
	}
}

// Every batch-3 key is rendered in the Settings dialog with its YAML-key hint
// next to a control bound to the section that saves it.
func TestPlatformSettingsRenderEveryKey(t *testing.T) {
	html := indexHTML(t)
	for _, c := range []struct{ fn, yaml, control string }{
		{"renderGovFeatures", "data.agents_dir", `data-arg0="features" data-arg1="dataAgentsDir" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "data.bob_sessions_dir", `data-arg0="features" data-arg1="dataBobSessionsDir" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "data.claude_sessions_dir", `data-arg0="features" data-arg1="dataClaudeSessionsDir" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "data.copilot_sessions_dir", `data-arg0="features" data-arg1="dataCopilotSessionsDir" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "data.logs_dir", `data-arg0="features" data-arg1="dataLogsDir" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "data.metrics_dir", `data-arg0="features" data-arg1="dataMetricsDir" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "data.session_retention_days", `data-arg0="features" data-arg1="dataSessionRetentionDays" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "deployment.podman_mode", `data-arg0="features" data-arg1="deploymentPodmanMode" data-arg-types="s,s,v"`},
		{"renderGovFeatures", "deployment.upgrade_request_dir", `data-arg0="features" data-arg1="deploymentUpgradeRequestDir" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "deployment.upgrade_helper", `value="${esc(f.deploymentUpgradeHelper || '')}"`},
		{"renderGovFeatures", "evidence.enabled", `data-section="features" data-key="evidenceEnabled"`},
		{"renderGovFeatures", "evidence.signing_key_file", `value="${esc(f.evidenceSigningKeyFile || '')}"`},
		{"renderGovFeatures", "gitea.gitea_url", `data-arg0="features" data-arg1="giteaUrl" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "gitea.token_env", `data-arg0="features" data-arg1="giteaTokenEnv" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "gitlab.gitlab_url", `data-arg0="features" data-arg1="gitlabUrl" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "gitlab.token_env", `data-arg0="features" data-arg1="gitlabTokenEnv" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "mint.key_path", `value="${esc(f.mintKeyPath || '')}"`},
		{"renderGovFeatures", "mint.max_ttl_seconds", `data-arg0="features" data-arg1="mintMaxTtlSeconds" data-arg-types="s,s,N"`},
		{"renderGovFeatures", "supersession_sweep.grace_period", `data-arg0="features" data-arg1="supersessionGracePeriod" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "compliance.posture_checks.window_days", `data-arg0="features" data-arg1="compliancePostureWindowDays" data-arg-types="s,s,N"`},
		{"renderGovFeatures", "compliance.posture_checks.history_days", `data-arg0="features" data-arg1="compliancePostureHistoryDays" data-arg-types="s,s,N"`},
		{"renderGovFeatures", "planning.design_approved_label", `data-arg0="features" data-arg1="planningDesignApprovedLabel" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "planning.max_concurrent_designs", `data-arg0="features" data-arg1="planningMaxConcurrentDesigns" data-arg-types="s,s,N"`},
		{"renderGovFeatures", "planning.max_design_revisions", `data-arg0="features" data-arg1="planningMaxDesignRevisions" data-arg-types="s,s,N"`},
		{"renderGovFeatures", "planning.plan_from_label", `data-section="features" data-key="planFromLabel"`},
		{"renderGovFeatures", "publication.owner", `data-arg0="features" data-arg1="publicationOwner" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "swarm.event_name", `data-arg0="features" data-arg1="swarmEventName" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "swarm.call_to_arms", `data-arg0="features" data-arg1="swarmCallToArms" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "swarm.leaderboard_title", `data-arg0="features" data-arg1="swarmLeaderboardTitle" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "tool_approval.inbox_path", `data-arg0="features" data-arg1="toolApprovalInboxPath" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "auto_merge.max_merges", `data-change-action="gh71"`},
		{"renderGovFeatures", "auto_merge.min_head_age", `data-arg0="auto-merge" data-arg1="min_head_age" data-arg-types="s,s,w"`},
		{"renderGovFeatures", "auto_merge.trusted_authors.require_role", `id="am-trusted-authors-require-role" data-change-action="gh101"`},
		{"renderEscalationSection", "escalation.threshold", `data-arg0="escalation" data-arg1="threshold" data-arg-types="s,s,N"`},
		{"renderGovSecurity", "intent.enforce", `data-section="security" data-key="intentEnforce"`},
		{"renderGovSecurity", "intent.alignment_model", `securityModelInput('intentAlignmentModel'`},
		{"renderGovSecurity", "sentinel.max_actions", `data-change-action="gh82"`},
		{"renderGovSecurity", "sentinel.trusted_authors_block", `data-action="toggleSentinelTrustedAuthorsBlock"`},
	} {
		body := extractJSFunction(t, html, c.fn)
		if !strings.Contains(body, "yamlHint('"+c.yaml+"')") {
			t.Errorf("%s has no YAML-key hint in %s", c.yaml, c.fn)
		}
		if !strings.Contains(body, c.control) {
			t.Errorf("%s has no control in %s (%s)", c.yaml, c.fn, c.control)
		}
	}
	body := extractJSFunction(t, html, "renderGovFeatures")
	for _, want := range []string{
		"Requires restart: these directories are read at boot.",
		"Requires restart: the forge client is built at boot.",
		"Requires restart: the swarm view reads its default theme once.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("Features tab must say %q", want)
		}
	}
	if n := strings.Count(body, `readonly aria-readonly="true"`); n < 3 {
		t.Errorf("upgrade_helper, evidence signing_key_file and mint key_path must render read-only; found %d read-only inputs", n)
	}
}

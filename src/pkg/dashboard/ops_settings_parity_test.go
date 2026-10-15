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

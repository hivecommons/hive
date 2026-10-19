package dashboard

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The github.* quota, scan and authorship keys round-trip through
// PUT /api/config/github and GET /api/config/github/forge-apps; invalid
// values are rejected without changing anything and the token never leaves
// the hive.
func TestConfigGitHubParityBatch6RoundTrip(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	s.deps.Config.GitHub.Token = "ghp_parity_batch6_secret"
	s.deps.Config.GitHub.APIReserve = 800
	s.deps.Config.GitHub.APICritical = 250
	before := s.deps.Config.GitHub

	for _, bad := range []map[string]any{
		{"agent_reserve_floor": -1},
		{"pr_detail_ttl_s": -1},
		{"graphql_pr_batch_page_size": -1},
		{"graphql_pr_batch_page_size": 101},
		{"api_reserve": -1},
		{"api_critical": 900},
		{"api_reserve": 200},
		{"api_reserve": 100, "api_critical": 100},
		{"app_signed_commits": true, "api_critical": 801},
	} {
		if rec := doPut(s, "/api/config/github", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d — %s", bad, rec.Code, rec.Body.String())
		}
	}
	if g := s.deps.Config.GitHub; g.APIReserve != before.APIReserve || g.APICritical != before.APICritical || g.AppSignedCommits != nil {
		t.Fatal("a rejected PUT changed the config")
	}

	if rec := doPut(s, "/api/config/github", map[string]any{
		"agent_reserve_floor":        500,
		"api_reserve":                1000,
		"api_critical":               300,
		"pr_detail_ttl_s":            600,
		"graphql_pr_batch":           false,
		"graphql_pr_batch_page_size": 25,
		"app_authored_prs":           false,
		"app_signed_commits":         true,
	}); rec.Code != http.StatusOK {
		t.Fatalf("github PUT: %d — %s", rec.Code, rec.Body.String())
	}
	g := s.deps.Config.GitHub
	if g.AgentReserveFloor != 500 || g.APIReserve != 1000 || g.APICritical != 300 || g.PRDetailTTLS != 600 ||
		g.GraphQLPRBatchEnabled() || g.GraphQLPRBatchPageSize != 25 || g.AppAuthoredPRsEnabled() || !g.AppSignedCommitsEnabled() {
		t.Fatalf("github settings not applied: %+v", g)
	}

	// Clearing a quota field restores the default, as config.Validate and
	// applyDefaults treat 0 at boot; the bound is checked against the default.
	if rec := doPut(s, "/api/config/github", map[string]any{"api_critical": 850}); rec.Code != http.StatusOK {
		t.Fatalf("api_critical 850 under reserve 1000: %d — %s", rec.Code, rec.Body.String())
	}
	if rec := doPut(s, "/api/config/github", map[string]any{"api_reserve": 0}); rec.Code != http.StatusBadRequest {
		t.Fatalf("api_reserve 0 (default 800) under critical 850: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/github", map[string]any{"api_reserve": 0, "api_critical": 0}); rec.Code != http.StatusOK {
		t.Fatalf("clearing api_reserve/api_critical: %d — %s", rec.Code, rec.Body.String())
	}
	if g := s.deps.Config.GitHub; g.APIReserve != 800 || g.APICritical != 250 {
		t.Fatalf("cleared quota fields did not restore defaults: reserve=%d critical=%d", g.APIReserve, g.APICritical)
	}
	if rec := doPut(s, "/api/config/github", map[string]any{"api_reserve": 1000, "api_critical": 300}); rec.Code != http.StatusOK {
		t.Fatalf("restore quota: %d — %s", rec.Code, rec.Body.String())
	}

	// A partial PUT changes only what it carries.
	if rec := doPut(s, "/api/config/github", map[string]any{"graphql_pr_batch": true}); rec.Code != http.StatusOK {
		t.Fatalf("partial PUT: %d — %s", rec.Code, rec.Body.String())
	}
	g = s.deps.Config.GitHub
	if !g.GraphQLPRBatchEnabled() || g.APIReserve != 1000 || g.PRDetailTTLS != 600 || !g.AppSignedCommitsEnabled() {
		t.Fatalf("partial PUT changed unrelated keys: %+v", g)
	}

	rec := doGet(s, "/api/config/github/forge-apps")
	if rec.Code != http.StatusOK {
		t.Fatalf("forge-apps GET: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "ghp_parity_batch6_secret") {
		t.Fatal("forge-apps response leaked github.token")
	}
	var got struct {
		Settings forgeGitHubSettings `json:"settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	st := got.Settings
	if !st.TokenSet || st.AgentReserveFloor != 500 || st.APIReserve != 1000 || st.APICritical != 300 || st.PRDetailTTLS != 600 ||
		!st.GraphQLPRBatch || st.GraphQLPRBatchPageSize != 25 || st.AppAuthoredPRs || !st.AppSignedCommits {
		t.Fatalf("forge-apps settings: %+v", st)
	}
}

// The scorer-tuning good_first_issue.* keys and auto_merge.bot_takeover
// round-trip through their section PUTs.
func TestParityBatch6FeaturesRoundTrip(t *testing.T) {
	s := covApiServer(t)
	before := s.deps.Config.GoodFirstIssue

	for _, bad := range []map[string]any{
		{"goodFirstIssueMinBodyChars": -1},
		{"goodFirstIssueMaxPackages": -1},
		{"goodFirstIssueMinScore": -1},
		{"goodFirstIssueMinBodyChars": 500, "goodFirstIssueMaxBodyChars": 100},
		{"goodFirstIssueReselectCooldown": "a week"},
		{"goodFirstIssueDocsUrl": "docs/contributing"},
		{"goodFirstIssueDocsUrl": "ftp://example.com/docs"},
		{"goodFirstIssueCommentTemplate": "no marker here"},
		{"goodFirstIssueCommentTemplate": "{{ .Marker }} {{ .Broken "},
	} {
		if rec := doPut(s, opsFeaturesPath, bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d", bad, rec.Code)
		}
	}
	if s.deps.Config.GoodFirstIssue.MinBodyChars != before.MinBodyChars || s.deps.Config.GoodFirstIssue.DocsURL != before.DocsURL {
		t.Fatal("a rejected PUT changed the config")
	}

	if rec := doPut(s, opsFeaturesPath, map[string]any{
		"goodFirstIssueMinBodyChars":     120,
		"goodFirstIssueMaxBodyChars":     3000,
		"goodFirstIssueMaxPackages":      3,
		"goodFirstIssueMinScore":         1,
		"goodFirstIssueReselectCooldown": "72h",
		"goodFirstIssueDocsUrl":          " https://example.com/docs ",
		"goodFirstIssueCommentTemplate":  "{{ .Marker }}\nWelcome!\n",
	}); rec.Code != http.StatusOK {
		t.Fatalf("features PUT: %d — %s", rec.Code, rec.Body.String())
	}
	g := s.deps.Config.GoodFirstIssue
	if g.MinBodyChars != 120 || g.MaxBodyChars != 3000 || g.MaxPackages != 3 || g.MinScore != 1 ||
		g.ReselectCooldown != 72*time.Hour || g.DocsURL != "https://example.com/docs" || g.CommentTemplate != "{{ .Marker }}\nWelcome!\n" {
		t.Fatalf("good_first_issue settings not applied: %+v", g)
	}
	if rec := doPut(s, opsFeaturesPath, map[string]any{"goodFirstIssueCommentTemplate": "  ", "goodFirstIssueReselectCooldown": ""}); rec.Code != http.StatusOK {
		t.Fatalf("reset PUT: %d — %s", rec.Code, rec.Body.String())
	}
	g = s.deps.Config.GoodFirstIssue
	if g.CommentTemplate != "" || g.ReselectCooldown != 0 || g.MinBodyChars != 120 {
		t.Fatalf("reset PUT did not reset only what it carried: %+v", g)
	}

	if !s.deps.Config.AutoMerge.BotTakeover.IsEnabled() {
		t.Fatal("bot takeover should default on")
	}
	rec := doPut(s, "/api/config/auto-merge", map[string]any{"bot_takeover_enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-merge PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.AutoMerge.BotTakeover.IsEnabled() {
		t.Fatal("bot_takeover_enabled=false not applied")
	}
	var am map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &am); err != nil {
		t.Fatal(err)
	}
	if am["bot_takeover_enabled"] != false {
		t.Fatalf("auto-merge response bot_takeover_enabled = %v", am["bot_takeover_enabled"])
	}
	if !s.deps.Config.AutoMerge.WorkflowApproval.TrustedBotsEnabled() {
		t.Fatal("workflow approval should default on")
	}
	rec = doPut(s, "/api/config/auto-merge", map[string]any{"workflow_approval_trusted_bots": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-merge workflow approval PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.AutoMerge.WorkflowApproval.TrustedBotsEnabled() {
		t.Fatal("workflow_approval_trusted_bots=false not applied")
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &am); err != nil {
		t.Fatal(err)
	}
	if am["workflow_approval_trusted_bots"] != false {
		t.Fatalf("auto-merge response workflow_approval_trusted_bots = %v", am["workflow_approval_trusted_bots"])
	}
}

// Every key this batch exposes renders with its YAML-key hint next to a
// control bound to the right save path.
func TestParityBatch6SettingsRenderEveryKey(t *testing.T) {
	html := indexHTML(t)
	features := extractJSFunction(t, html, "renderGovFeatures")
	for _, c := range []struct{ yaml, control string }{
		{"good_first_issue.min_body_chars", `data-arg1="goodFirstIssueMinBodyChars" data-arg-types="s,s,N"`},
		{"good_first_issue.max_body_chars", `data-arg1="goodFirstIssueMaxBodyChars" data-arg-types="s,s,N"`},
		{"good_first_issue.max_packages", `data-arg1="goodFirstIssueMaxPackages" data-arg-types="s,s,N"`},
		{"good_first_issue.min_score", `data-arg1="goodFirstIssueMinScore" data-arg-types="s,s,N"`},
		{"good_first_issue.reselect_cooldown", `data-arg1="goodFirstIssueReselectCooldown" data-arg-types="s,s,w"`},
		{"good_first_issue.docs_url", `data-arg1="goodFirstIssueDocsUrl" data-arg-types="s,s,w"`},
		{"good_first_issue.comment_template", `data-arg1="goodFirstIssueCommentTemplate" data-arg-types="s,s,v"`},
		{"auto_merge.bot_takeover.enabled", `data-section="auto-merge" data-key="bot_takeover_enabled"`},
	} {
		if !strings.Contains(features, "yamlHint('"+c.yaml+"')") {
			t.Errorf("%s has no YAML-key hint", c.yaml)
		}
		if !strings.Contains(features, c.control) {
			t.Errorf("%s has no Features control (%s)", c.yaml, c.control)
		}
	}

	gh := extractJSFunction(t, html, "renderForgeGitHubSettings")
	for _, c := range []struct{ yaml, control string }{
		{"github.api_reserve", `num('api_reserve'`},
		{"github.api_critical", `num('api_critical'`},
		{"github.agent_reserve_floor", `num('agent_reserve_floor'`},
		{"github.pr_detail_ttl_s", `num('pr_detail_ttl_s'`},
		{"github.graphql_pr_batch_page_size", `num('graphql_pr_batch_page_size'`},
		{"github.graphql_pr_batch", `toggle('graphql_pr_batch'`},
		{"github.app_authored_prs", `toggle('app_authored_prs'`},
		{"github.app_signed_commits", `toggle('app_signed_commits'`},
		{"github.forge", `roRow('Forge'`},
		{"github.api_url", `roRow('API URL'`},
		{"github.docs_installation_id", `roRow('Docs org installation ID'`},
		{"github.oauth_client_id", `roRow('Login OAuth client ID'`},
		{"github.token", `roRow('Personal access token'`},
	} {
		if !strings.Contains(gh, c.control) || !strings.Contains(gh, "'"+c.yaml+"'") {
			t.Errorf("%s has no Forge App control with its YAML hint (%s)", c.yaml, c.control)
		}
	}
	if !strings.Contains(gh, "managed by the hub") {
		t.Error("identity keys do not say they are hub-managed")
	}
	if !strings.Contains(html, "yamlHint('github.key_file')") {
		t.Error("github.key_file has no YAML hint on the Forge App edit form")
	}
	if !strings.Contains(html, "yamlHint('github.reporter_trust_hold')") {
		t.Error("github.reporter_trust_hold has no YAML hint on the Repos tab")
	}
	if save := extractJSFunction(t, html, "saveForgeGitHubSettings"); !strings.Contains(save, "'/api/config/github'") {
		t.Error("saveForgeGitHubSettings does not save through PUT /api/config/github")
	}
}

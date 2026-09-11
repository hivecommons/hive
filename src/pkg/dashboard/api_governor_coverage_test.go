package dashboard

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func govServer(t *testing.T) *Server {
	t.Helper()
	return covApiServer(t)
}

func TestCovGov_Thresholds(t *testing.T) {
	s := govServer(t)
	if rec := doPutRaw(s, "/api/config/governor/thresholds", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("thresholds bad body: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/thresholds", map[string]int{"quiet": 3, "busy": 12}); rec.Code != http.StatusOK {
		t.Fatalf("thresholds ok: %d", rec.Code)
	}
}

func TestCovGov_Labels(t *testing.T) {
	s := govServer(t)
	if rec := doPutRaw(s, "/api/config/governor/labels", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("labels bad body: %d", rec.Code)
	}
	// Include a permanent/hold label so it is filtered out.
	if rec := doPut(s, "/api/config/governor/labels", map[string]any{"labels": []string{"custom-exempt", "hold"}}); rec.Code != http.StatusOK {
		t.Fatalf("labels ok: %d", rec.Code)
	}
}

func TestCovGov_Budget(t *testing.T) {
	s := govServer(t)
	if rec := doPutRaw(s, "/api/config/governor/budget", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("budget bad body: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/budget", map[string]any{"totalTokens": 1000000, "periodDays": 7, "criticalPct": 90}); rec.Code != http.StatusOK {
		t.Fatalf("budget ok: %d", rec.Code)
	}
}

func TestCovGov_Notifications(t *testing.T) {
	s := govServer(t)
	if rec := doPutRaw(s, "/api/config/governor/notifications", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("notifications bad body: %d", rec.Code)
	}
	// Valid values for ntfy + discord.
	if rec := doPut(s, "/api/config/governor/notifications", map[string]any{
		"ntfyServer":     "https://ntfy.example.com",
		"ntfyTopic":      "hive",
		"discordWebhook": "https://discord.com/api/webhooks/1/abc",
	}); rec.Code != http.StatusOK {
		t.Fatalf("notifications ok: %d", rec.Code)
	}
	// Masked values are ignored (bullet prefix).
	if rec := doPut(s, "/api/config/governor/notifications", map[string]any{
		"ntfyServer": "•••masked",
		"ntfyTopic":  "•••masked",
	}); rec.Code != http.StatusOK {
		t.Fatalf("notifications masked: %d", rec.Code)
	}
}

func TestCovGov_Health(t *testing.T) {
	s := govServer(t)
	if rec := doPutRaw(s, "/api/config/governor/health", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("health bad body: %d", rec.Code)
	}
	lock := true
	if rec := doPut(s, "/api/config/governor/health", map[string]any{
		"healthcheckInterval": 300, "restartCooldown": 60, "modelLock": lock,
	}); rec.Code != http.StatusOK {
		t.Fatalf("health ok: %d", rec.Code)
	}
}

func TestCovGov_Logging(t *testing.T) {
	s := govServer(t)
	if rec := doPutRaw(s, "/api/config/governor/logging", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("logging bad body: %d", rec.Code)
	}
	comp := true
	if rec := doPut(s, "/api/config/governor/logging", map[string]any{
		"maxSizeMB": 50, "maxAgeDays": 7, "maxBackups": 3, "compress": comp, "level": "info",
	}); rec.Code != http.StatusOK {
		t.Fatalf("logging ok: %d", rec.Code)
	}
}

func TestCovGov_AddAgent(t *testing.T) {
	s := govServer(t)
	if rec := doPost(s, "/api/config/governor/agents", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("addagent no name: %d", rec.Code)
	}
	if rec := doPost(s, "/api/config/governor/agents", map[string]any{"name": "bad name!"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("addagent bad name: %d", rec.Code)
	}
	// Existing agent → conflict.
	if rec := doPost(s, "/api/config/governor/agents", map[string]any{"name": "scanner"}); rec.Code != http.StatusConflict {
		t.Fatalf("addagent conflict: %d", rec.Code)
	}
	// New agent with default backend.
	if rec := doPost(s, "/api/config/governor/agents", map[string]any{"name": "newbie"}); rec.Code != http.StatusOK {
		t.Fatalf("addagent ok: %d", rec.Code)
	}
}

func TestCovGov_RemoveAgent(t *testing.T) {
	s := govServer(t)
	if rec := doDelete(s, "/api/config/governor/agents/ghost", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("removeagent not found: %d", rec.Code)
	}
	if rec := doDelete(s, "/api/config/governor/agents/scanner", nil); rec.Code != http.StatusOK {
		t.Fatalf("removeagent ok: %d", rec.Code)
	}
}

func TestCovGov_Repos(t *testing.T) {
	s := govServer(t)
	if rec := doPutRaw(s, "/api/config/governor/repos", "bad"); rec.Code != http.StatusBadRequest {
		t.Fatalf("repos bad body: %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/repos", map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("repos empty: %d", rec.Code)
	}
	// Invalid repo name (shell metachars).
	if rec := doPut(s, "/api/config/governor/repos", map[string]any{"repos": []string{"bad;rm"}}); rec.Code != http.StatusBadRequest {
		t.Fatalf("repos invalid: %d", rec.Code)
	}
	// Valid plain repo list (with a default among them — required now).
	if rec := doPut(s, "/api/config/governor/repos", map[string]any{"repos": []string{"repoA", "repoB"}, "primaryRepo": "repoA"}); rec.Code != http.StatusOK {
		t.Fatalf("repos ok: %d", rec.Code)
	}
	// A cross-forge repo (GHE URL on a public-github.com hive) must be REJECTED
	// by the single-host-per-spoke guard — a hive's repos all live on one host.
	if rec := doPut(s, "/api/config/governor/repos", map[string]any{"repos": []string{"https://github.ibm.com/myorg/repoC"}, "primaryRepo": "repoC"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-forge repo should be rejected: %d", rec.Code)
	}
	// A full same-forge (github.com) URL in the repo field is accepted as an
	// explicit org/repo migration and stored in canonical org + bare repo form.
	if rec := doPut(s, "/api/config/governor/repos", map[string]any{"repos": []string{"https://github.com/myorg/repoC"}, "primaryRepo": "repoC"}); rec.Code != http.StatusOK {
		t.Fatalf("repos url should be accepted: %d", rec.Code)
	}
	if s.deps.Config.Project.Org != "myorg" {
		t.Fatalf("org = %q, want myorg", s.deps.Config.Project.Org)
	}
}

func TestGovernorReposSelfAuthorizationHoldRoundTrip(t *testing.T) {
	s := govServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	rec := doPut(s, "/api/config/governor/repos", map[string]any{
		"repos":                     []string{"myorg/repoA", "myorg/repoB"},
		"primaryRepo":               "myorg/repoA",
		"selfAuthorizationHold":     false,
		"repoSelfAuthorizationHold": map[string]any{"myorg/repoB": true},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("repos update: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if s.deps.Config.GitHub.SelfAuthorizationHold == nil || *s.deps.Config.GitHub.SelfAuthorizationHold {
		t.Fatalf("hive-wide self auth hold = %+v, want false", s.deps.Config.GitHub.SelfAuthorizationHold)
	}
	if s.deps.Config.SelfAuthorizationHoldEnabledForRepo("repoA") {
		t.Fatal("repoA should inherit disabled hive-wide hold")
	}
	if !s.deps.Config.SelfAuthorizationHoldEnabledForRepo("repoB") {
		t.Fatal("repoB explicit override should enable hold")
	}

	get := doOwnerGet(s, "/api/config/governor")
	if get.Code != http.StatusOK {
		t.Fatalf("governor get: want 200, got %d", get.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if body["selfAuthorizationHold"] != false {
		t.Fatalf("selfAuthorizationHold response = %v, want false", body["selfAuthorizationHold"])
	}
	policies, ok := body["repoSelfAuthorizationHold"].(map[string]any)
	if !ok {
		t.Fatalf("repoSelfAuthorizationHold response type %T", body["repoSelfAuthorizationHold"])
	}
	repoB, ok := policies["myorg/repoB"].(map[string]any)
	if !ok || repoB["value"] != true || repoB["effective"] != true {
		t.Fatalf("repoB policy response = %#v, want value/effective true", policies["myorg/repoB"])
	}
}

func TestGovernorReposClearsSelfAuthorizationOverridesOnOrgMigration(t *testing.T) {
	s := govServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	disabled := false
	s.deps.Config.Project.RepoPolicies = append(s.deps.Config.Project.RepoPolicies, config.RepoPolicy{
		Repo:                  "repoA",
		SelfAuthorizationHold: &disabled,
	})

	rec := doPut(s, "/api/config/governor/repos", map[string]any{
		"repos":                     []string{"neworg/repoA"},
		"primaryRepo":               "neworg/repoA",
		"repoSelfAuthorizationHold": map[string]any{"myorg/repoA": false},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("repos migration: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if len(s.deps.Config.Project.RepoPolicies) != 0 {
		t.Fatalf("repo policies after org migration = %+v, want cleared", s.deps.Config.Project.RepoPolicies)
	}
	if !s.deps.Config.SelfAuthorizationHoldEnabledForRepo("repoA") {
		t.Fatal("old org's repo override should not retarget onto the new org")
	}
}

func TestGovernorReposClearsSelfAuthorizationOverridesOnPrimaryOnlyOrgMigration(t *testing.T) {
	s := govServer(t)
	s.deps.Config.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	s.deps.Config.Project.Repos = []string{"repoA"}
	s.deps.Config.Project.PrimaryRepo = "repoA"
	disabled := false
	s.deps.Config.Project.RepoPolicies = append(s.deps.Config.Project.RepoPolicies, config.RepoPolicy{
		Repo:                  "repoA",
		SelfAuthorizationHold: &disabled,
	})

	rec := doPut(s, "/api/config/governor/repos", map[string]any{
		"primaryRepo": "neworg/repoA",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("primary-only migration: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Project.Org != "neworg" {
		t.Fatalf("org = %q, want neworg", s.deps.Config.Project.Org)
	}
	if len(s.deps.Config.Project.RepoPolicies) != 0 {
		t.Fatalf("repo policies after primary-only org migration = %+v, want cleared", s.deps.Config.Project.RepoPolicies)
	}
	if !s.deps.Config.SelfAuthorizationHoldEnabledForRepo("repoA") {
		t.Fatal("old org's repo override should not retarget through primary-only migration")
	}
}

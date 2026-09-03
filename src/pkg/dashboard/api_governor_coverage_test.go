package dashboard

import (
	"net/http"
	"testing"
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

package worksource

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestFromConfig_LinearAPIKeyEnvRef reproduces the dashboard bug: a work
// source saved via PUT /api/config/governor/work-source with
// `api_key: ${LINEAR_API_KEY}` is stored verbatim (file loads env-expand, API
// saves do not), so the adapter sent the literal `${LINEAR_API_KEY}` as its
// Authorization header and got 401. The reference must resolve from the
// environment at construction time; an unset variable is a clear error.
func TestFromConfig_LinearAPIKeyEnvRef(t *testing.T) {
	logger := slog.Default()
	base := config.WorkSourceConfig{Type: "linear"}
	base.Linear.Teams = []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "acme/app"}}

	t.Setenv("LINEAR_API_KEY", "lin_api_resolved")
	for _, ref := range []string{"${LINEAR_API_KEY}", "$LINEAR_API_KEY"} {
		cfg := base
		cfg.Linear.APIKey = ref
		ws, err := FromConfig(cfg, nil, "", "", logger)
		if err != nil {
			t.Fatalf("FromConfig(%q): %v", ref, err)
		}
		ls, ok := ws.(*LinearSource)
		if !ok {
			t.Fatalf("source type %T", ws)
		}
		if ls.cfg.APIKey != "lin_api_resolved" {
			t.Errorf("api_key %q resolved to %q, want lin_api_resolved", ref, ls.cfg.APIKey)
		}
	}

	// A literal key is used as-is.
	cfg := base
	cfg.Linear.APIKey = "lin_api_literal"
	ws, err := FromConfig(cfg, nil, "", "", logger)
	if err != nil {
		t.Fatalf("FromConfig(literal): %v", err)
	}
	if got := ws.(*LinearSource).cfg.APIKey; got != "lin_api_literal" {
		t.Errorf("literal api_key changed to %q", got)
	}

	// Unset variable: clear error naming the field and the variable, never a
	// literal "${LINEAR_API_KEY}" header.
	os.Unsetenv("LINEAR_API_KEY")
	cfg.Linear.APIKey = "${LINEAR_API_KEY}"
	_, err = FromConfig(cfg, nil, "", "", logger)
	if err == nil {
		t.Fatal("unset ${LINEAR_API_KEY} did not error")
	}
	for _, want := range []string{"work_source.linear.api_key", "LINEAR_API_KEY", "not set"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestFromConfig_JiraAPITokenEnvRef: the same contract for the Jira adapter.
func TestFromConfig_JiraAPITokenEnvRef(t *testing.T) {
	logger := slog.Default()
	cfg := config.WorkSourceConfig{Type: "jira"}
	cfg.Jira.BaseURL = "https://acme.atlassian.net"
	cfg.Jira.Email = "bot@acme.example"
	cfg.Jira.APIToken = "${JIRA_API_TOKEN}"

	t.Setenv("JIRA_API_TOKEN", "atl_resolved")
	ws, err := FromConfig(cfg, nil, "", "", logger)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	js, ok := ws.(*jiraSource)
	if !ok {
		t.Fatalf("source type %T", ws)
	}
	if js.cfg.APIToken != "atl_resolved" {
		t.Errorf("api_token resolved to %q, want atl_resolved", js.cfg.APIToken)
	}

	os.Unsetenv("JIRA_API_TOKEN")
	if _, err := FromConfig(cfg, nil, "", "", logger); err == nil || !strings.Contains(err.Error(), "JIRA_API_TOKEN") {
		t.Errorf("unset ${JIRA_API_TOKEN}: err = %v, want clear error", err)
	}
}

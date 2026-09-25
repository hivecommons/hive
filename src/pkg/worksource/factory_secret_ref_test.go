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

func TestFromConfig_JiraDataCenterPasswordEnvRef(t *testing.T) {
	logger := slog.Default()
	caPEM, _, clientCertPEM, clientKeyPEM := jiraTestTLSMaterials(t)
	cfg := config.WorkSourceConfig{Type: "jira"}
	cfg.Jira.Deployment = "datacenter"
	cfg.Jira.BaseURL = "https://jira.example.com/jira"
	cfg.Jira.Username = "bot"
	cfg.Jira.Password = "${JIRA_PASSWORD}"
	cfg.Jira.CABundle = "${JIRA_CA_BUNDLE}"
	cfg.Jira.ClientCert = "${JIRA_CLIENT_CERT}"
	cfg.Jira.ClientKey = "${JIRA_CLIENT_KEY}"

	t.Setenv("JIRA_PASSWORD", "dc_pw_resolved")
	t.Setenv("JIRA_CA_BUNDLE", caPEM)
	t.Setenv("JIRA_CLIENT_CERT", clientCertPEM)
	t.Setenv("JIRA_CLIENT_KEY", clientKeyPEM)
	ws, err := FromConfig(cfg, nil, "", "", logger)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}

	js, ok := ws.(*jiraSource)
	if !ok {
		t.Fatalf("source type %T", ws)
	}
	if js.cfg.Password != "dc_pw_resolved" {
		t.Errorf("password resolved to %q, want dc_pw_resolved", js.cfg.Password)
	}
	if js.cfg.CABundle != caPEM || js.cfg.ClientCert != clientCertPEM || js.cfg.ClientKey != clientKeyPEM {
		t.Error("TLS secret refs were not resolved")
	}
}

func TestFromConfig_JiraCloudIgnoresDataCenterTLSRefs(t *testing.T) {
	logger := slog.Default()
	cfg := config.WorkSourceConfig{Type: "jira"}
	cfg.Jira.Deployment = "cloud"
	cfg.Jira.BaseURL = "https://acme.atlassian.net"
	cfg.Jira.Email = "bot@acme.example"
	cfg.Jira.APIToken = "tok"
	cfg.Jira.CABundle = "${UNSET_JIRA_CA_BUNDLE}"
	cfg.Jira.ClientCert = "${UNSET_JIRA_CLIENT_CERT}"
	cfg.Jira.ClientKey = "${UNSET_JIRA_CLIENT_KEY}"

	if _, err := FromConfig(cfg, nil, "", "", logger); err != nil {
		t.Fatalf("Cloud Jira should ignore Data Center-only TLS refs: %v", err)
	}
}

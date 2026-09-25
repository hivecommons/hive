package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadWithDashboardOverlay_AdoptsGovernorWorkSource reproduces the
// standalone-Kubernetes report: a work source set from the dashboard was
// written to /data/hive.yaml.dashboard but after a pod restart the reload
// only adopted OTel, RemovedAgents and Agents from the overlay, so
// GET /api/config/governor/work-source came back with type "".
func TestLoadWithDashboardOverlay_AdoptsGovernorWorkSource(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
`
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	// The overlay is what saveConfig wrote after the dashboard PUT. It is
	// deliberately "short" (no agents) so the fullness guard would bail early:
	// the work source must be adopted regardless.
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	overlay := `
project:
  org: testorg
  repos: [repo1]
governor:
  work_source:
    type: linear
    linear:
      api_key: ${LINEAR_API_KEY}
      hold_labels: [blocked]
      teams:
        - key: ENG
          repo: testorg/repo1
`
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}

	origOverlay := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = origOverlay })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	// Unset so the reference survives the overlay's env expansion; the
	// worksource factory resolves it at the point of use.
	os.Unsetenv("LINEAR_API_KEY")

	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	ws := merged.Governor.WorkSource
	if ws.Type != "linear" {
		t.Fatalf("work_source.type not restored from overlay: got %q want linear", ws.Type)
	}
	if ws.Linear.APIKey != "${LINEAR_API_KEY}" {
		t.Errorf("work_source.linear.api_key = %q, want the ${LINEAR_API_KEY} reference", ws.Linear.APIKey)
	}
	if len(ws.Linear.Teams) != 1 || ws.Linear.Teams[0].Key != "ENG" || ws.Linear.Teams[0].Repo != "testorg/repo1" {
		t.Errorf("work_source.linear.teams not restored: %+v", ws.Linear.Teams)
	}
	if len(ws.Linear.HoldLabels) != 1 || ws.Linear.HoldLabels[0] != "blocked" {
		t.Errorf("work_source.linear.hold_labels not restored: %v", ws.Linear.HoldLabels)
	}

	// An overlay with no work source must not clobber one set in the seed.
	seedWS := seed + `
governor:
  work_source:
    type: github_projects
    github_projects:
      project_number: 7
`
	if err := os.WriteFile(seedPath, []byte(seedWS), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayPath, []byte("project:\n  org: testorg\n  repos: [repo1]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err = LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay (empty overlay): %v", err)
	}
	if merged.Governor.WorkSource.Type != "github_projects" || merged.Governor.WorkSource.GitHubProjects.ProjectNumber != 7 {
		t.Errorf("seed work source clobbered by empty overlay: %+v", merged.Governor.WorkSource)
	}
}

// TestRedactedForPersist_WorkSourceCredentials: the dashboard PUT stores the
// operator's literal `${LINEAR_API_KEY}` reference (API saves are not
// env-expanded) and worksource.FromConfig resolves it at use, so the persisted
// copy (seed rewrite and dashboard overlay) must carry the reference through
// unchanged, whether or not the variable is set in the environment.
func TestRedactedForPersist_WorkSourceCredentials(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_api_supersecretvalue0123456789")
	t.Setenv("JIRA_API_TOKEN", "atlassian_supersecrettoken0123456789")
	cfg := &Config{}
	cfg.Governor.WorkSource.Type = "linear"
	cfg.Governor.WorkSource.Linear.APIKey = "${LINEAR_API_KEY}"
	cfg.Governor.WorkSource.Jira.APIToken = "${JIRA_API_TOKEN}"
	cfg.Governor.WorkSource.Jira.Password = "${JIRA_DATACENTER_PASSWORD}"
	cfg.Governor.WorkSource.Jira.CABundle = "${JIRA_DATACENTER_CA_BUNDLE}"
	cfg.Governor.WorkSource.Jira.ClientCert = "${JIRA_DATACENTER_CLIENT_CERT}"
	cfg.Governor.WorkSource.Jira.ClientKey = "${JIRA_DATACENTER_CLIENT_KEY}"

	red := cfg.redactedForPersist()
	if got := red.Governor.WorkSource.Linear.APIKey; got != "${LINEAR_API_KEY}" {
		t.Errorf("linear.api_key persisted as %q, want ${LINEAR_API_KEY}", got)
	}
	if got := red.Governor.WorkSource.Jira.APIToken; got != "${JIRA_API_TOKEN}" {
		t.Errorf("jira.api_token persisted as %q, want ${JIRA_API_TOKEN}", got)
	}
	if got := red.Governor.WorkSource.Jira.Password; got != "${JIRA_DATACENTER_PASSWORD}" {
		t.Errorf("jira.password persisted as %q, want ${JIRA_DATACENTER_PASSWORD}", got)
	}
	if got := red.Governor.WorkSource.Jira.CABundle; got != "${JIRA_DATACENTER_CA_BUNDLE}" {
		t.Errorf("jira.ca_bundle persisted as %q, want ${JIRA_DATACENTER_CA_BUNDLE}", got)
	}
	if got := red.Governor.WorkSource.Jira.ClientCert; got != "${JIRA_DATACENTER_CLIENT_CERT}" {
		t.Errorf("jira.client_cert persisted as %q, want ${JIRA_DATACENTER_CLIENT_CERT}", got)
	}
	if got := red.Governor.WorkSource.Jira.ClientKey; got != "${JIRA_DATACENTER_CLIENT_KEY}" {
		t.Errorf("jira.client_key persisted as %q, want ${JIRA_DATACENTER_CLIENT_KEY}", got)
	}
	if !strings.HasPrefix(cfg.Governor.WorkSource.Linear.APIKey, "${") {
		t.Errorf("redactedForPersist mutated the live config: %q", cfg.Governor.WorkSource.Linear.APIKey)
	}
}

// TestRedactedForPersist_WorkSourceRefSurvivesEnvSubstrings is the regression
// for the CI failure on #4976: redactedForPersist used to scan os.Environ()
// and replace every env VALUE found inside the key with ${NAME}. CI exports
// ACCEPT_EULA=Y, so the trailing Y of the literal `${LINEAR_API_KEY}` became
// `${LINEAR_API_KE${ACCEPT_EULA}}`. Any env value that is a substring of the
// key corrupts it; the persisted value must be byte-identical to the input.
func TestRedactedForPersist_WorkSourceRefSurvivesEnvSubstrings(t *testing.T) {
	t.Setenv("ACCEPT_EULA", "Y")
	t.Setenv("HIVE_TEST_SUBSTRING", "API_KEY")
	t.Setenv("HIVE_TEST_DOLLAR", "${")
	cfg := &Config{}
	cfg.Governor.WorkSource.Type = "linear"
	cfg.Governor.WorkSource.Linear.APIKey = "${LINEAR_API_KEY}"
	cfg.Governor.WorkSource.Jira.APIToken = "${JIRA_API_TOKEN}"
	cfg.Governor.WorkSource.Jira.Password = "${JIRA_DATACENTER_PASSWORD}"
	cfg.Governor.WorkSource.Jira.CABundle = "${JIRA_DATACENTER_CA_BUNDLE}"
	cfg.Governor.WorkSource.Jira.ClientCert = "${JIRA_DATACENTER_CLIENT_CERT}"
	cfg.Governor.WorkSource.Jira.ClientKey = "${JIRA_DATACENTER_CLIENT_KEY}"

	red := cfg.redactedForPersist()
	if got := red.Governor.WorkSource.Linear.APIKey; got != "${LINEAR_API_KEY}" {
		t.Errorf("linear.api_key persisted as %q, want ${LINEAR_API_KEY} untouched", got)
	}
	if got := red.Governor.WorkSource.Jira.APIToken; got != "${JIRA_API_TOKEN}" {
		t.Errorf("jira.api_token persisted as %q, want ${JIRA_API_TOKEN} untouched", got)
	}
	if got := red.Governor.WorkSource.Jira.Password; got != "${JIRA_DATACENTER_PASSWORD}" {
		t.Errorf("jira.password persisted as %q, want ${JIRA_DATACENTER_PASSWORD} untouched", got)
	}
	if got := red.Governor.WorkSource.Jira.CABundle; got != "${JIRA_DATACENTER_CA_BUNDLE}" {
		t.Errorf("jira.ca_bundle persisted as %q, want ${JIRA_DATACENTER_CA_BUNDLE} untouched", got)
	}
	if got := red.Governor.WorkSource.Jira.ClientCert; got != "${JIRA_DATACENTER_CLIENT_CERT}" {
		t.Errorf("jira.client_cert persisted as %q, want ${JIRA_DATACENTER_CLIENT_CERT} untouched", got)
	}
	if got := red.Governor.WorkSource.Jira.ClientKey; got != "${JIRA_DATACENTER_CLIENT_KEY}" {
		t.Errorf("jira.client_key persisted as %q, want ${JIRA_DATACENTER_CLIENT_KEY} untouched", got)
	}
}

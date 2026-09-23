package main

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/taskmcp"
)

// #8348: the task MCP URL and the project context the boot hands to the agent
// manager are the channels through which hub-launched agents learn about the
// hive, so neither may carry the dashboard bearer. On v6 the URL used to embed
// cfg.Dashboard.AuthToken as ?token=; agents now receive a per-launch token
// minted by the boot and bound to their launch scope instead.

// bootDashboardTokenSentinel cannot occur by accident in any context field,
// so a substring match is conclusive.
const bootDashboardTokenSentinel = "hive-dashboard-token-sentinel-8348"

func TestBootTaskMCPURLNeverCarriesDashboardToken(t *testing.T) {
	cfg := bootAdvisoryConfig(t)
	cfg.Dashboard.AuthToken = bootDashboardTokenSentinel
	b, _ := newDepsTestBoot(t, cfg)

	raw := b.taskMCPURLForAgents()
	if strings.Contains(raw, bootDashboardTokenSentinel) {
		t.Fatalf("task MCP URL carries the dashboard token: %q", raw)
	}
	// Positive control: the URL is well formed and names the MCP endpoint.
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("task MCP URL does not parse: %q: %v", raw, err)
	}
	if u.Scheme == "" || u.Host == "" || u.Path != taskmcp.EndpointPath {
		t.Fatalf("task MCP URL = %q, want an absolute URL at %s", raw, taskmcp.EndpointPath)
	}
	if u.Query().Has(taskmcp.TokenQueryParam) {
		t.Fatalf("task MCP URL carries a token query parameter: %q", raw)
	}
}

func TestBootProjectContextNeverCarriesDashboardToken(t *testing.T) {
	t.Setenv("HIVE_ADVISORY_ISSUE", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", bootDashboardTokenSentinel)
	cfg := bootAdvisoryConfig(t)
	// The context copies cfg.Project.PrimaryRepo verbatim; set it explicitly
	// (as TestBootAdvisoryWithPrefersPrimaryRepo does) so the positive
	// control below proves the context was built from this config.
	cfg.Project.PrimaryRepo = "acme/widgets"
	cfg.Dashboard.AuthToken = bootDashboardTokenSentinel
	b, _ := newDepsTestBoot(t, cfg)
	b.ghClient = fakeGitHubClient(t)

	b.bootAdvisoryWith(newBootAdvisoryFake().deps)

	// Positive control: the context was actually built from this config and
	// carries the task MCP wiring.
	if b.projectCtx.Org != "acme" || b.projectCtx.PrimaryRepoName != "acme/widgets" {
		t.Fatalf("project context not built: org=%q primary=%q", b.projectCtx.Org, b.projectCtx.PrimaryRepoName)
	}
	if b.projectCtx.TaskMCPURL == "" || b.projectCtx.TaskMCPLaunchToken == nil {
		t.Fatalf("task MCP wiring missing: url=%q minter=%v", b.projectCtx.TaskMCPURL, b.projectCtx.TaskMCPLaunchToken != nil)
	}

	v := reflect.ValueOf(b.projectCtx)
	typ := v.Type()
	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		name := typ.Field(i).Name
		switch field.Kind() {
		case reflect.String:
			if strings.Contains(field.String(), bootDashboardTokenSentinel) {
				t.Errorf("ProjectContext.%s carries the dashboard token: %q", name, field.String())
			}
		case reflect.Slice:
			if field.Type().Elem().Kind() != reflect.String {
				continue
			}
			for j := 0; j < field.Len(); j++ {
				if strings.Contains(field.Index(j).String(), bootDashboardTokenSentinel) {
					t.Errorf("ProjectContext.%s[%d] carries the dashboard token: %q", name, j, field.Index(j).String())
				}
			}
		}
	}
}

func TestBootTaskMCPLaunchTokenIsLeaseScoped(t *testing.T) {
	cfg := bootAdvisoryConfig(t)
	cfg.Dashboard.AuthToken = bootDashboardTokenSentinel
	b, _ := newDepsTestBoot(t, cfg)
	scope := taskmcp.LaunchScope{TaskID: "scanner:acme/widgets#0:2", Repo: "acme/widgets", Agent: "scanner", Generation: 2}

	token := b.taskMCPLaunchTokenForAgents(scope)
	if token == "" {
		t.Fatal("no launch token minted for a complete scope")
	}
	if strings.Contains(token, bootDashboardTokenSentinel) || token == bootDashboardTokenSentinel {
		t.Fatalf("launch token carries the dashboard token: %q", token)
	}
	// Positive control: the hub can verify it with its own secret and it is
	// bound to exactly this launch.
	claims, err := taskmcp.VerifyLeaseToken([]byte(bootDashboardTokenSentinel), token, time.Now())
	if err != nil {
		t.Fatalf("launch token does not verify against the dashboard secret: %v", err)
	}
	if claims.TaskID != scope.TaskID || claims.Repo != scope.Repo || claims.Identity != scope.Agent {
		t.Fatalf("claims = %#v, want bound to %#v", claims, scope)
	}
	if !claims.ExpiresAt.After(time.Now()) {
		t.Fatalf("launch token already expired: %v", claims.ExpiresAt)
	}
	if _, err := taskmcp.VerifyLeaseToken([]byte("some-other-secret"), token, time.Now()); err == nil {
		t.Fatal("launch token verified under a foreign secret")
	}

	// Open spoke (no dashboard token): no credential rather than a bogus one.
	cfg.Dashboard.AuthToken = ""
	if got := b.taskMCPLaunchTokenForAgents(scope); got != "" {
		t.Fatalf("launch token without a dashboard secret = %q, want empty", got)
	}
}

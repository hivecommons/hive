package main

import (
	"reflect"
	"strings"
	"testing"
)

// #8348: the project context the boot hands to the agent manager is the only
// channel through which hub-launched agents learn about the hive, so it must
// never carry the dashboard bearer - neither the configured auth token nor the
// HIVE_DASHBOARD_TOKEN the discord bot reads from the environment. On v5 there
// is no task-MCP URL field at all; this test pins that no string-valued field
// of the context carries the token, so a future backport of the v6 task-MCP
// wiring cannot reintroduce the dashboard token silently.

// bootDashboardTokenSentinel cannot occur by accident in any context field,
// so a substring match is conclusive.
const bootDashboardTokenSentinel = "hive-dashboard-token-sentinel-8348"

func TestBootProjectContextNeverCarriesDashboardToken(t *testing.T) {
	t.Setenv("HIVE_ADVISORY_ISSUE", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", bootDashboardTokenSentinel)
	cfg := bootAdvisoryConfig(t)
	cfg.Project.PrimaryRepo = "acme/widgets"
	cfg.Dashboard.AuthToken = bootDashboardTokenSentinel
	b, _ := newDepsTestBoot(t, cfg)
	b.ghClient = fakeGitHubClient(t)

	b.bootAdvisoryWith(newBootAdvisoryFake().deps)

	// Positive control: the context was actually built from this config.
	if b.projectCtx.Org != "acme" || b.projectCtx.PrimaryRepoName != "acme/widgets" {
		t.Fatalf("project context not built: org=%q primary=%q", b.projectCtx.Org, b.projectCtx.PrimaryRepoName)
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

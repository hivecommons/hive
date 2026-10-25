package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestAgentPathHelpers(t *testing.T) {
	for got, want := range map[string]string{
		StartPath("a"):              PathAgents + "/a/start",
		RestartPath("a"):            PathAgents + "/a/restart",
		PausePath("a"):              PathAgents + "/a/pause",
		ResumePath("a"):             PathAgents + "/a/resume",
		RestartCountPath("a"):       PathAgents + "/a/restart-count",
		SnapshotPath("a"):           PathAgents + "/a/snapshot",
		KickValidatePath("a"):       PathAgents + "/a/kick/validate",
		KickLogsPath("a"):           PathAgents + "/a/kicklogs",
		KickLogPath("a", "k"):       PathAgents + "/a/kicklogs/k",
		DefinitionPath("a"):         PathAgents + "/a/definition",
		OverridesPath("a"):          PathAgents + "/a/overrides",
		PinPath("a", PinCLI):        PathAgents + "/a/pins/cli",
		LoginCodePath("a"):          PathAgents + "/a/login-code",
		SessionPath("a"):            PathAgents + "/a/session",
		SessionKickPath("a"):        PathAgents + "/a/session/kick",
		SupervisionPath("a"):        PathAgents + "/a/supervision",
		AgentTokenRefreshPath("a"):  PathAgents + "/a/tokens/refresh",
		NudgePath("a"):              PathAgents + "/a/nudge",
		SeedPath("a"):               PathAgents + "/a/seed",
		WatchdogPath("a"):           PathAgents + "/a/watchdog",
		WatchdogConditionsPath("a"): PathAgents + "/a/watchdog/conditions",
		BackendAuthPath("claude"):   PathBackends + "/claude/auth",
	} {
		if got != want {
			t.Errorf("got %s want %s", got, want)
		}
	}
}

// TestRouteTablesAreWellFormed checks every route uses a known method and a
// versioned path, and that each path helper's shape matches a table entry
// once the placeholders are filled.
func TestRouteTablesAreWellFormed(t *testing.T) {
	methods := map[string]bool{http.MethodGet: true, http.MethodPost: true, http.MethodPut: true, http.MethodDelete: true}
	fill := strings.NewReplacer("{name}", "a", "{id}", "k", "{kind}", PinCLI, "{backend}", "claude")
	paths := map[string]bool{}
	for _, table := range []map[string]Route{RuntimeRoutes, WatchdogRoutes} {
		for method, r := range table {
			if !methods[r.Method] {
				t.Errorf("%s: method %q", method, r.Method)
			}
			if !strings.HasPrefix(r.Path, "/"+Version+"/") {
				t.Errorf("%s: path %q is not versioned", method, r.Path)
			}
			paths[fill.Replace(r.Path)] = true
		}
	}
	for _, p := range []string{StartPath("a"), KickLogPath("a", "k"), PinPath("a", PinCLI), SessionKickPath("a"), WatchdogConditionsPath("a"), BackendAuthPath("claude"), PathSnapshots, PathReconcile, PathBreakerOn, PathAuthorize} {
		if !paths[p] {
			t.Errorf("%s has no route table entry", p)
		}
	}
}

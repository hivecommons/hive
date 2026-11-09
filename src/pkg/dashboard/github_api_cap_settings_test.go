package dashboard

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
	"gopkg.in/yaml.v3"
)

// The proxy's per-agent GitHub API hourly cap (agents.github_api_hourly_cap
// and agents.<name>.github_api_hourly_cap) was hive.yaml-only, so on a
// hub-managed spoke nobody but the hub operator could raise a reviewer that
// hit its 300/h cap overnight. The Forge App tab now edits the fleet default
// and each agent's General tab its override. Both save through the normal
// dashboard save (hive.yaml, the dashboard overlay), audit the change, and
// write the live config the proxy reads, so the cap applies without a restart.

// capTestLayers points every config layer at a temp dir and forces
// Kubernetes mode, so Save writes the dashboard overlay as on a hosted spoke.
func capTestLayers(t *testing.T, cfg *config.Config) (overlay string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1") // IsKubernetesPod() → true
	cfg.SourcePath = filepath.Join(dir, "hive.yaml")
	oldRuntime, oldOverlay := config.RuntimeConfigFile, config.DashboardOverlayFile
	config.RuntimeConfigFile = filepath.Join(dir, "hive.yaml.runtime")
	config.DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	t.Cleanup(func() { config.RuntimeConfigFile, config.DashboardOverlayFile = oldRuntime, oldOverlay })
	// What Load's applyDefaults leaves for an absent fleet key.
	cfg.AgentsGitHubAPIHourlyCap = config.DefaultAgentsGitHubAPIHourlyCap
	return config.DashboardOverlayFile
}

func readOverlayConfig(t *testing.T, path string) config.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dashboard overlay not written: %v", err)
	}
	var c config.Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("overlay does not parse: %v\n%s", err, data)
	}
	return c
}

func lastCapAudit(t *testing.T, s *Server) AuditEntry {
	t.Helper()
	entries := s.GetAudit().Recent(1)
	if len(entries) != 1 {
		t.Fatal("no audit entry recorded")
	}
	return entries[0]
}

func TestFleetAgentGitHubCapSavesAuditsAndAppliesLive(t *testing.T) {
	s, deps := apiServer(t)
	overlay := capTestLayers(t, deps.Config)
	// main.go hands the proxy this method value, bound to the live *Config.
	proxyCap := deps.Config.AgentGitHubAPIHourlyCap

	for _, bad := range []string{`{"agent_github_api_hourly_cap": -1}`, `{"agent_github_api_hourly_cap": 1.5}`, `{"agent_github_api_hourly_cap": "600"}`} {
		if rec := doPutRaw(s, "/api/config/github", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400 (%s)", bad, rec.Code, rec.Body.String())
		}
	}
	if deps.Config.AgentsGitHubAPIHourlyCapSet || proxyCap("scanner") != 300 {
		t.Fatal("a rejected PUT changed the fleet cap")
	}

	rec := doPut(s, "/api/config/github", map[string]any{"agent_github_api_hourly_cap": 600})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if got := proxyCap("scanner"); got != 600 {
		t.Fatalf("proxy cap after save = %d, want 600 without a reload", got)
	}
	if o := readOverlayConfig(t, overlay); !o.AgentsGitHubAPIHourlyCapSet || o.AgentsGitHubAPIHourlyCap != 600 {
		t.Fatalf("overlay fleet cap = %d (set %v), want 600", o.AgentsGitHubAPIHourlyCap, o.AgentsGitHubAPIHourlyCapSet)
	}
	if e := lastCapAudit(t, s); e.Action != "config_github" || !strings.Contains(e.Detail, "agents.github_api_hourly_cap=300 (default) -> 600") {
		t.Fatalf("audit = %+v", e)
	}
	forge := decodeJSON(t, doOwnerGet(s, "/api/config/github/forge-apps"))
	settings, _ := forge["settings"].(map[string]any)
	if settings["agent_github_api_hourly_cap"] != float64(600) || settings["agent_github_api_hourly_cap_set"] != true {
		t.Fatalf("forge-apps settings = %v", settings)
	}

	// 0 is a real value: no cap. It is kept, not read as "unset".
	if rec := doPut(s, "/api/config/github", map[string]any{"agent_github_api_hourly_cap": 0}); rec.Code != http.StatusOK {
		t.Fatalf("PUT 0: %d %s", rec.Code, rec.Body.String())
	}
	if got := proxyCap("scanner"); got != 0 || !deps.Config.AgentsGitHubAPIHourlyCapSet {
		t.Fatalf("after 0: proxy cap = %d, set = %v; want 0 and set", got, deps.Config.AgentsGitHubAPIHourlyCapSet)
	}
	if o := readOverlayConfig(t, overlay); !o.AgentsGitHubAPIHourlyCapSet || o.AgentsGitHubAPIHourlyCap != 0 {
		t.Fatalf("overlay after 0 = %d (set %v), want an explicit 0", o.AgentsGitHubAPIHourlyCap, o.AgentsGitHubAPIHourlyCapSet)
	}

	// null removes the key: the default applies again and the overlay omits it.
	if rec := doPutRaw(s, "/api/config/github", `{"agent_github_api_hourly_cap": null}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT null: %d %s", rec.Code, rec.Body.String())
	}
	if got := proxyCap("scanner"); got != 300 || deps.Config.AgentsGitHubAPIHourlyCapSet {
		t.Fatalf("after null: proxy cap = %d, set = %v; want the default 300, unset", got, deps.Config.AgentsGitHubAPIHourlyCapSet)
	}
	if o := readOverlayConfig(t, overlay); o.AgentsGitHubAPIHourlyCapSet {
		t.Fatal("overlay still carries agents.github_api_hourly_cap after a reset to the default")
	}
	if e := lastCapAudit(t, s); !strings.Contains(e.Detail, "0 (no cap) -> 300 (default)") {
		t.Fatalf("audit after reset = %+v", e)
	}
}

// capReplicaServer is apiServer with scanner running two replicas, built
// before the agent manager so the save does not see scanner-2 as new.
func capReplicaServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	deps := testDeps(t)
	sc := deps.Config.Agents["scanner"]
	sc.Replicas = 2
	deps.Config.Agents["scanner"] = sc
	if err := deps.Config.ExpandAgentReplicas(); err != nil {
		t.Fatal(err)
	}
	if deps.Config.Agents["scanner-2"].ReplicaOf != "scanner" {
		t.Fatalf("scanner-2 not expanded: %+v", deps.Config.Agents)
	}
	deps.AgentMgr = cruxtest.NewRuntime(deps.Config.Agents, deps.Logger, crux.ProjectContext{})
	s := NewServer(0, deps.Logger)
	s.RegisterAPI(deps)
	return s, deps
}

func TestAgentGitHubCapGeneralSaveInheritZeroAndReplicas(t *testing.T) {
	s, deps := capReplicaServer(t)
	overlay := capTestLayers(t, deps.Config)
	deps.Config.Data.AgentsDir = t.TempDir()
	proxyCap := deps.Config.AgentGitHubAPIHourlyCap

	g := generalSection(t, s, "scanner")
	if v, present := g["githubApiHourlyCap"]; !present || v != nil {
		t.Fatalf("githubApiHourlyCap = %v (present %v), want null", v, present)
	}
	if g["githubApiHourlyCapEffective"] != float64(300) || g["githubApiHourlyCapFleet"] != float64(300) || g["githubApiHourlyCapBase"] != "" {
		t.Fatalf("general before save = %v", g)
	}

	for _, bad := range []any{-5, 2.5, "600"} {
		if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"githubApiHourlyCap": bad}); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: status = %d, want 400", bad, rec.Code)
		}
	}

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"githubApiHourlyCap": 600})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT 600: %d %s", rec.Code, rec.Body.String())
	}
	if proxyCap("scanner") != 600 || proxyCap("scanner-2") != 600 {
		t.Fatalf("proxy caps = %d / %d, want 600 for the agent and its replica at once", proxyCap("scanner"), proxyCap("scanner-2"))
	}
	if e := lastCapAudit(t, s); e.Action != "config_agent_general" || e.Agent != "scanner" || !strings.Contains(e.Detail, "github_api_hourly_cap=inherit -> 600") {
		t.Fatalf("audit = %+v", e)
	}
	if o := readOverlayConfig(t, overlay); o.Agents["scanner"].GitHubAPIHourlyCap == nil || *o.Agents["scanner"].GitHubAPIHourlyCap != 600 {
		t.Fatalf("overlay scanner = %+v", o.Agents["scanner"])
	}
	if _, ok := readOverlayConfig(t, overlay).Agents["scanner-2"]; ok {
		t.Fatal("the overlay persisted the derived replica")
	}
	agentFile, err := os.ReadFile(filepath.Join(deps.Config.Data.AgentsDir, "scanner.yaml"))
	if err != nil || !strings.Contains(string(agentFile), "github_api_hourly_cap: 600") {
		t.Fatalf("agent file (%v):\n%s", err, agentFile)
	}

	// A replica shows its base agent's cap and refuses an override of its own.
	r := generalSection(t, s, "scanner-2")
	if r["githubApiHourlyCapBase"] != "scanner" || r["githubApiHourlyCapEffective"] != float64(600) {
		t.Fatalf("replica general = %v", r)
	}
	if rec := doPut(s, "/api/config/agent/scanner-2/general", map[string]any{"githubApiHourlyCap": 50}); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "scanner") {
		t.Fatalf("replica PUT: %d %s, want 400 naming scanner", rec.Code, rec.Body.String())
	}

	// 0 is kept as "no cap for this agent", not cleared.
	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"githubApiHourlyCap": 0}); rec.Code != http.StatusOK {
		t.Fatalf("PUT 0: %d %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].GitHubAPIHourlyCap; got == nil || *got != 0 || proxyCap("scanner") != 0 {
		t.Fatalf("after 0: override = %v, proxy cap = %d; want an explicit 0", got, proxyCap("scanner"))
	}

	// null clears the override: the agent inherits the fleet default again.
	if rec := doPutRaw(s, "/api/config/agent/scanner/general", `{"githubApiHourlyCap": null}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT null: %d %s", rec.Code, rec.Body.String())
	}
	if got := deps.Config.Agents["scanner"].GitHubAPIHourlyCap; got != nil || proxyCap("scanner") != 300 || proxyCap("scanner-2") != 300 {
		t.Fatalf("after null: override = %v, caps = %d / %d; want inherit and 300", got, proxyCap("scanner"), proxyCap("scanner-2"))
	}
	if o := readOverlayConfig(t, overlay); o.Agents["scanner"].GitHubAPIHourlyCap != nil {
		t.Fatal("overlay still carries the cleared override")
	}
	if e := lastCapAudit(t, s); !strings.Contains(e.Detail, "github_api_hourly_cap=0 (no cap) -> inherit") {
		t.Fatalf("audit after inherit = %+v", e)
	}

	// A save that does not name the cap leaves it alone and says nothing of it.
	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"githubApiHourlyCap": 450}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"description": "scans"}); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	if proxyCap("scanner") != 450 || strings.Contains(lastCapAudit(t, s).Detail, "github_api_hourly_cap") {
		t.Fatalf("an unrelated save touched the cap: %d, audit %+v", proxyCap("scanner"), lastCapAudit(t, s))
	}
}

// The GitHub API budget panel carries each agent's effective cap, where it
// comes from, and the calls the proxy counted against it this hour.
func TestGHRateLimitsCarryAgentCaps(t *testing.T) {
	SetAgentGitHubCallsProvider(func() map[string]int { return map[string]int{"scanner": 42, "scanner-2": 7, "reviewer": 9} })
	t.Cleanup(func() { SetAgentGitHubCallsProvider(nil) })
	six := 600
	zero := 0
	cfg := &config.Config{AgentRuntimeConfig: config.AgentRuntimeConfig{
		AgentsGitHubAPIHourlyCap: config.DefaultAgentsGitHubAPIHourlyCap,
		Agents: map[string]config.AgentConfig{
			"scanner":   {GitHubAPIHourlyCap: &six},
			"scanner-2": {GitHubAPIHourlyCap: &six, ReplicaOf: "scanner"},
			"reviewer":  {GitHubAPIHourlyCap: &zero},
			"architect": {},
		},
	}}

	caps, ok := buildGHRateLimits(nil, nil, cfg)["agent_caps"].(map[string]any)
	if !ok {
		t.Fatal("ghRateLimits has no agent_caps")
	}
	if caps["fleet_cap"] != 300 || caps["fleet_cap_set"] != false {
		t.Fatalf("fleet = %v / %v", caps["fleet_cap"], caps["fleet_cap_set"])
	}
	rows, _ := caps["agents"].([]map[string]any)
	byName := map[string]map[string]any{}
	var order []string
	for _, r := range rows {
		byName[r["agent"].(string)] = r
		order = append(order, r["agent"].(string))
	}
	if strings.Join(order, ",") != "architect,reviewer,scanner,scanner-2" {
		t.Fatalf("rows = %v, want every agent sorted", order)
	}
	for name, want := range map[string]struct {
		cap    int
		calls  any
		source string
	}{
		"scanner":   {600, 42, "agent"},
		"scanner-2": {600, 7, "base_agent"},
		"reviewer":  {0, nil, "agent"},
		"architect": {300, 0, "fleet"},
	} {
		r := byName[name]
		if r["cap"] != want.cap || r["calls"] != want.calls || r["source"] != want.source {
			t.Errorf("%s = %v, want cap %d calls %v source %s", name, r, want.cap, want.calls, want.source)
		}
	}
	if byName["scanner-2"]["base_agent"] != "scanner" {
		t.Errorf("replica row = %v, want base_agent scanner", byName["scanner-2"])
	}
}

// config_effective_read shows both caps: the keys' values (they are on the
// non-secret value list) and the effective cap per agent.
func TestDiagConfigShowsAgentGitHubCaps(t *testing.T) {
	writeProvenanceFixture(t, diagSeedYAML, `project:
  org: acme
agents:
  github_api_hourly_cap: 300
  scanner: {backend: claude}
  reviewer:
    backend: claude
    github_api_hourly_cap: 600
`)
	s, _ := apiServer(t)
	res, _ := diagConfigRead(t, s, "/api/diag/config")
	for key, want := range map[string]string{
		"agents.github_api_hourly_cap":          "300",
		"agents.reviewer.github_api_hourly_cap": "600",
	} {
		got := diagKey(t, res, key)
		if !got.ValueShown || got.Value != want {
			t.Errorf("%s = %+v, want %s shown", key, got, want)
		}
	}
	caps := res.AgentGitHubCaps
	if caps == nil {
		t.Fatal("no agent_github_api_hourly_caps in the result")
	}
	if caps.Fleet != 300 || !caps.FleetSet || caps.Agents["reviewer"] != 600 || caps.Agents["scanner"] != 300 {
		t.Fatalf("agent_github_api_hourly_caps = %+v", caps)
	}
	if strings.Join(caps.Overrides, ",") != "reviewer" {
		t.Fatalf("overrides = %v", caps.Overrides)
	}
	for _, key := range []string{"agents.github_api_hourly_cap", "agents.reviewer.github_api_hourly_cap"} {
		if !diagConfigValueKey(key) {
			t.Errorf("%s is not on the value list", key)
		}
	}
}

// Both controls render with their YAML key and a ? help mark, the fleet save
// sends null for a blank field, and the budget panel lists the agent caps.
func TestGitHubCapControlsRender(t *testing.T) {
	html := indexHTML(t)
	gh := extractJSFunction(t, html, "renderForgeGitHubSettings")
	for _, want := range []string{
		"yamlHint('agents.github_api_hourly_cap')",
		`id="forge-gh-${capKey}"`,
		"settingHelpMark('operator-reference/runtime-and-quota', 'github-api-quota', 'Agent GitHub API hourly cap', ",
		"${fleetCapField}",
	} {
		if !strings.Contains(gh, want) {
			t.Errorf("Forge App settings missing %q", want)
		}
	}
	if save := extractJSFunction(t, html, "saveForgeGitHubSettings"); !strings.Contains(save, "body.agent_github_api_hourly_cap = null") || !strings.Contains(save, "body.agent_github_api_hourly_cap = v") {
		t.Error("saveForgeGitHubSettings does not send the fleet cap (null for blank)")
	}
	field := extractJSFunction(t, html, "agentGitHubCapFieldHtml")
	for _, want := range []string{
		"yamlHint('agents.<key>.github_api_hourly_cap')",
		"settingHelpMark('operator-reference/runtime-and-quota', 'github-api-quota', 'GitHub API hourly cap', tip)",
		`data-change-action="githubApiCapChanged"`,
		"g.githubApiHourlyCapBase",
	} {
		if !strings.Contains(field, want) {
			t.Errorf("agent General cap field missing %q", want)
		}
	}
	if !strings.Contains(extractJSFunction(t, html, "renderAgentGeneral"), "agentGitHubCapFieldHtml(g)") {
		t.Error("the General tab does not render the cap field")
	}
	if !strings.Contains(html, "githubApiCapChanged: function") || !strings.Contains(html, "markDirty('general','githubApiHourlyCap',null)") {
		t.Error("no change handler saving null for a blank cap")
	}
	panel := extractJSFunction(t, html, "renderGitHubAPIBudgetPanel")
	for _, want := range []string{"ghRate.agent_caps", `id="gh-api-budget-agent-caps"`, "ghApiBudgetHelpMark('agentCaps', 'Agent caps')"} {
		if !strings.Contains(panel, want) {
			t.Errorf("budget panel missing %q", want)
		}
	}
}

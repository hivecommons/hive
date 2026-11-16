package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// agentSettingsProvider answers settings_read section agent with a
// JSON-decoded copy of the agent's settings, as GET /api/config/agent/{name}
// answers, and records writes.
type agentSettingsProvider struct {
	agents   map[string]map[string]any // name → general block
	requests []WriteRequest
}

func (p *agentSettingsProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != agentSettingsSection {
		return map[string]any{}, nil
	}
	name, _ := args["agent"].(string)
	general, ok := p.agents[name]
	if !ok {
		return nil, fmt.Errorf("agent not found")
	}
	b, err := json.Marshal(map[string]any{"general": general, "cadences": map[string]any{}})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *agentSettingsProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"status": "updated"}, nil
}

// agentSettingsTestProvider is a hive with redteam on at ISSUES_ONLY scoped
// to one repository, a docs agent that is off and on demand, and quality-2,
// a replica of quality.
func agentSettingsTestProvider() *agentSettingsProvider {
	return &agentSettingsProvider{agents: map[string]map[string]any{
		"redteam": {
			"enabled": true, "mode": "ISSUES_ONLY", "onDemand": false, "repos": []string{"acme/app"},
			"includeRepos": true, "replicas": 1, "continuous": false, "continuousBudgetPct": 80,
			"continuousCooldown": 60, "contributorFirst": false, "displayName": "Red Team",
			"githubApiHourlyCapBase": "",
		},
		"docs": {
			"enabled": false, "mode": "", "onDemand": true, "repos": nil, "includeRepos": false,
			"replicas": 2, "continuous": true, "continuousBudgetPct": 50, "continuousCooldown": 120,
			"contributorFirst": false, "githubApiHourlyCapBase": "",
		},
		"quality-2": {"enabled": true, "githubApiHourlyCapBase": "quality"},
	}}
}

func agentSettingsHandler(p *agentSettingsProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func agentSettingsPreview(t *testing.T, h *Handler, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAgentSettings, "args": args})
	if err != nil {
		t.Fatalf("%v: preview: %v", args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func TestAgentSettingsOpRegistered(t *testing.T) {
	if _, ok := DefaultWriteRegistry().Get(WriteOpAgentSettings); !ok {
		t.Fatalf("%s is not registered", WriteOpAgentSettings)
	}
}

// The schema offers agent plus exactly the allowed fields: no refused field
// appears in it.
func TestAgentSettingsSchemaListsExactlyTheAllowedFields(t *testing.T) {
	props, _ := agentSettingsOp{}.InputSchema()["properties"].(map[string]any)
	var got []string
	for k := range props {
		if k != "agent" {
			got = append(got, k)
		}
	}
	want := []string{
		"enabled", "mode", "onDemand", "repos", "includeRepos", "replicas", "staleTimeout",
		"restartStrategy", "clearOnKick", "continuous", "continuousBudgetPct", "continuousCooldown",
		"contributorFirst", "contributorFirstGrace", "kickTemplate", "beadRole", "displayName",
		"description", "emoji", "color", "sortOrder", "aliases", "laneKeywords", "detectKeywords",
		"bobSessionLabel",
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("schema fields = %v, want %v", got, want)
	}
	for _, refused := range AgentSettingsRefusedFields() {
		if _, ok := props[refused]; ok {
			t.Errorf("the schema offers refused field %s", refused)
		}
	}
	if _, ok := props["agent"]; !ok {
		t.Fatal("the schema has no agent")
	}
}

// Each refused field is refused by name with where to change it instead,
// before anything is read or written.
func TestAgentSettingsRefusesDashboardOnlyFields(t *testing.T) {
	pointers := map[string]string{
		"launchCmd":          "on the dashboard",
		"cliPinned":          "on the dashboard",
		"cliPinValue":        "on the dashboard",
		"sandboxEnabled":     "on the dashboard",
		"sandboxSubstrate":   "on the dashboard",
		"engine":             "on the dashboard",
		"role":               "on the dashboard",
		"promptSource":       "on the dashboard",
		"definitionSource":   "on the dashboard",
		"model":              "agent.model",
		"githubApiHourlyCap": "agent.github_api_hourly_cap",
	}
	refused := AgentSettingsRefusedFields()
	if len(refused) != len(pointers) {
		t.Fatalf("refused fields = %v, want the %d this test covers", refused, len(pointers))
	}
	for _, key := range refused {
		want, ok := pointers[key]
		if !ok {
			t.Errorf("refused field %s is not covered by this test", key)
			continue
		}
		p := agentSettingsTestProvider()
		_, err := agentSettingsHandler(p).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAgentSettings, "args": map[string]any{"agent": "redteam", "enabled": false, key: "x"}})
		if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want it refused with %q", key, err, want)
		}
		if len(p.requests) != 0 {
			t.Errorf("%s: a refused field wrote %+v", key, p.requests)
		}
	}
}

// Bad arguments are refused with their reason, before anything is written.
func TestAgentSettingsRefusesBadArgs(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"agent": "redteam"}, "name at least one field"},
		{map[string]any{"enabled": false}, "agent is required"},
		{map[string]any{"agent": "redteam", "enabeld": false}, `unknown field "enabeld"`},
		{map[string]any{"agent": "redteam", "cavemanMode": "lite"}, `unknown field "cavemanMode"`},
		{map[string]any{"agent": "redteam", "enabled": "false"}, "enabled must be true or false"},
		{map[string]any{"agent": "redteam", "enabled": nil}, "enabled is null"},
		{map[string]any{"agent": "redteam", "mode": "ISSUES_AND_MERGE"}, "mode must be one of"},
		{map[string]any{"agent": "redteam", "restartStrategy": "later"}, "restartStrategy must be one of"},
		{map[string]any{"agent": "redteam", "beadRole": "boss"}, "beadRole must be one of"},
		{map[string]any{"agent": "redteam", "replicas": float64(0)}, "replicas must be between 1 and"},
		{map[string]any{"agent": "redteam", "replicas": 1.5}, "replicas must be a whole number"},
		{map[string]any{"agent": "redteam", "sortOrder": float64(-1)}, "sortOrder must be a whole number"},
		{map[string]any{"agent": "redteam", "continuousCooldown": float64(0)}, "greater than 0"},
		{map[string]any{"agent": "redteam", "repos": "acme/app"}, "repos must be a list of strings"},
		{map[string]any{"agent": "redteam", "repos": []any{"acme/app", " "}}, "blank entry"},
		{map[string]any{"agent": "ghost", "enabled": false}, "agent not found"},
		{map[string]any{"agent": "quality-2", "enabled": false}, "change quality instead"},
	} {
		p := agentSettingsTestProvider()
		_, err := agentSettingsHandler(p).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAgentSettings, "args": tc.args})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
		if len(p.requests) != 0 {
			t.Errorf("%v: refused args wrote %+v", tc.args, p.requests)
		}
	}
}

// Turning an agent off sends only enabled to the General endpoint, shows it
// before and after, is not widening, and write_confirm reads it back.
func TestAgentSettingsSendsOnlyNamedFields(t *testing.T) {
	p := agentSettingsTestProvider()
	h := agentSettingsHandler(p)
	preview, id := agentSettingsPreview(t, h, map[string]any{"agent": "redteam", "enabled": false})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/agent/redteam/general" {
		t.Fatalf("request = %+v", preview.Request)
	}
	if got := bodyJSON(t, preview.Request.Body); got != `{"enabled":false}` {
		t.Fatalf("body = %s", got)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "No widening:") {
		t.Fatalf("turning an agent off: %q", preview.WideningDisclosure)
	}
	if got := bodyJSON(t, preview.Details["changes"]); got != `[{"after":false,"before":true,"field":"enabled"}]` {
		t.Fatalf("changes = %s", got)
	}
	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != `{"enabled":false}` {
		t.Fatalf("executed %+v", p.requests)
	}
	if after, _ := out.(map[string]any)["after"].(map[string]any); len(after) != 1 {
		t.Fatalf("write_confirm read back %#v, want only enabled", out.(map[string]any)["after"])
	}

	// Several fields: still only those, typed as the endpoint reads them.
	preview, _ = agentSettingsPreview(t, h, map[string]any{"agent": "redteam", "displayName": " Red ", "sortOrder": float64(3), "laneKeywords": []any{"security"}})
	if got := bodyJSON(t, preview.Request.Body); got != `{"displayName":"Red","laneKeywords":["security"],"sortOrder":3}` {
		t.Fatalf("body = %s", got)
	}
}

// Each change that lets the agent do more or run more is named as
// widening; the reverse is not.
func TestAgentSettingsWidening(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  map[string]any
		widen string // "" for no widening
	}{
		{"enabled true → false", map[string]any{"agent": "redteam", "enabled": false}, ""},
		{"enabled false → true", map[string]any{"agent": "docs", "enabled": true}, "is turned on"},
		{"enabled stays true", map[string]any{"agent": "redteam", "enabled": true}, ""},
		{"mode up", map[string]any{"agent": "redteam", "mode": "ISSUES_AND_PRS"}, "goes up from ISSUES_ONLY to ISSUES_AND_PRS"},
		{"mode to merge", map[string]any{"agent": "redteam", "mode": "ISSUES_PRS_MERGE"}, "merge on green CI"},
		{"mode down", map[string]any{"agent": "redteam", "mode": "ADVISORY"}, ""},
		{"mode NO_GITHUB", map[string]any{"agent": "redteam", "mode": "NO_GITHUB"}, ""},
		{"mode to the ACMM default", map[string]any{"agent": "redteam", "mode": ""}, "ACMM level's default, which may be higher"},
		{"mode from the ACMM default to advisory", map[string]any{"agent": "docs", "mode": "ADVISORY"}, ""},
		{"mode from the ACMM default up", map[string]any{"agent": "docs", "mode": "ISSUES_ONLY"}, "may be above the ACMM level's default"},
		{"repos gains one", map[string]any{"agent": "redteam", "repos": []any{"acme/app", "acme/lib"}}, "gains acme/lib"},
		{"repos cleared", map[string]any{"agent": "redteam", "repos": []any{}}, "every repository of the hive"},
		{"repos narrowed", map[string]any{"agent": "docs", "repos": []any{"acme/app"}}, ""},
		{"repos swapped", map[string]any{"agent": "redteam", "repos": []any{"acme/lib"}}, "gains acme/lib"},
		{"includeRepos false → true", map[string]any{"agent": "docs", "includeRepos": true}, "includeRepos false → true"},
		{"includeRepos true → false", map[string]any{"agent": "redteam", "includeRepos": false}, ""},
		{"replicas up", map[string]any{"agent": "redteam", "replicas": float64(3)}, "3 copies instead of 1"},
		{"replicas down", map[string]any{"agent": "docs", "replicas": float64(1)}, ""},
		{"continuous on", map[string]any{"agent": "redteam", "continuous": true}, "runs continuously"},
		{"continuous off", map[string]any{"agent": "docs", "continuous": false}, ""},
		{"continuous budget up", map[string]any{"agent": "docs", "continuousBudgetPct": float64(90)}, "90% of the budget"},
		{"continuous budget up while not continuous", map[string]any{"agent": "redteam", "continuousBudgetPct": float64(90)}, ""},
		{"continuous cooldown down", map[string]any{"agent": "docs", "continuousCooldown": float64(30)}, "waits 30s"},
		{"onDemand true → false", map[string]any{"agent": "docs", "onDemand": false}, "leaves on-demand"},
		{"onDemand false → true", map[string]any{"agent": "redteam", "onDemand": true}, ""},
		{"contributorFirst on", map[string]any{"agent": "redteam", "contributorFirst": true}, "contributor-first lane"},
		{"cosmetic", map[string]any{"agent": "redteam", "emoji": "🛡", "color": "#FF0000"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview, _ := agentSettingsPreview(t, agentSettingsHandler(agentSettingsTestProvider()), tc.args)
			d := preview.WideningDisclosure
			if tc.widen == "" {
				if !strings.HasPrefix(d, "No widening:") {
					t.Fatalf("want no widening, got %q", d)
				}
				return
			}
			if !strings.HasPrefix(d, "Potential widening:") || !strings.Contains(d, tc.widen) {
				t.Fatalf("want widening naming %q, got %q", tc.widen, d)
			}
		})
	}
}

// A dashboard edit to a named field between preview and confirm makes the
// preview's before/after wrong, so the confirm is refused and nothing is
// written; an edit to a field the op does not change does not refuse it.
func TestAgentSettingsRefusesStalePreview(t *testing.T) {
	p := agentSettingsTestProvider()
	h := agentSettingsHandler(p)
	_, id := agentSettingsPreview(t, h, map[string]any{"agent": "redteam", "enabled": false, "mode": "ADVISORY"})
	p.agents["redteam"]["mode"] = "ISSUES_PRS_MERGE"
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirmation wrote %+v", p.requests)
	}

	_, id = agentSettingsPreview(t, h, map[string]any{"agent": "redteam", "enabled": false})
	p.agents["redteam"]["displayName"] = "Renamed"
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("an edit to an unnamed field refused the confirm: %v", err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("executed %+v", p.requests)
	}
}

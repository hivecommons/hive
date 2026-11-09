package adminmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// capSettingsProvider answers settings_read for one agent's General tab and
// the Forge App settings, and records writes.
type capSettingsProvider struct {
	agentGeneral map[string]any
	forge        map[string]any
	requests     []WriteRequest
}

func (p *capSettingsProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead {
		return map[string]any{}, nil
	}
	switch args["section"] {
	case "agent":
		return map[string]any{"general": p.agentGeneral}, nil
	case "github_apps":
		return map[string]any{"settings": p.forge}, nil
	}
	return map[string]any{}, nil
}

func (p *capSettingsProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"status": "updated"}, nil
}

func capPreview(t *testing.T, h *Handler, op string, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": op, "args": args})
	if err != nil {
		t.Fatalf("%s %v: preview: %v", op, args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func isWidening(p WritePreview) bool {
	return strings.HasPrefix(p.WideningDisclosure, "Widening:") || strings.HasPrefix(p.WideningDisclosure, "Potential widening:")
}

func namesBucketAndFloor(p WritePreview) bool {
	return strings.Contains(p.WideningDisclosure, "App installation") && strings.Contains(p.WideningDisclosure, "github.agent_reserve_floor")
}

func TestGitHubCapWriteOpsRegistered(t *testing.T) {
	for _, name := range []string{WriteOpAgentGitHubAPIHourlyCap, WriteOpGitHubAgentAPIHourlyCap} {
		if _, ok := DefaultWriteRegistry().Get(name); !ok {
			t.Fatalf("%s is not registered", name)
		}
	}
}

// Raising an agent's cap, removing it (0), or inheriting a higher fleet value
// is widening and names the shared bucket and the reserve floor; lowering is
// not.
func TestAgentGitHubCapPreviewDisclosesWideningOnlyWhenRaising(t *testing.T) {
	provider := &capSettingsProvider{agentGeneral: map[string]any{
		"githubApiHourlyCap": float64(300), "githubApiHourlyCapEffective": float64(300),
		"githubApiHourlyCapFleet": float64(500), "githubApiHourlyCapBase": "",
	}}
	h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))

	for _, tc := range []struct {
		name  string
		cap   any
		widen bool
		body  any
	}{
		{"raise", float64(600), true, 600},
		{"no cap", float64(0), true, 0},
		{"inherit a higher fleet value", "inherit", true, nil},
		{"lower", float64(100), false, 100},
		{"same", float64(300), false, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := capPreview(t, h, WriteOpAgentGitHubAPIHourlyCap, map[string]any{"agent": "reviewer", "cap": tc.cap})
			if p.Request.Method != http.MethodPut || p.Request.Path != "/api/config/agent/reviewer/general" {
				t.Fatalf("request = %s %s", p.Request.Method, p.Request.Path)
			}
			got, _ := json.Marshal(p.Request.Body)
			want, _ := json.Marshal(map[string]any{"githubApiHourlyCap": tc.body})
			if string(got) != string(want) {
				t.Fatalf("body = %s, want %s", got, want)
			}
			if isWidening(p) != tc.widen || namesBucketAndFloor(p) != tc.widen {
				t.Fatalf("widening_disclosure = %q, want widening=%v", p.WideningDisclosure, tc.widen)
			}
			if p.WideningDisclosure == "" || p.ConfirmationMessage == "" || len(p.Effects) == 0 {
				t.Fatalf("incomplete preview %#v", p)
			}
		})
	}

	// Inheriting a LOWER fleet value is not widening.
	provider.agentGeneral["githubApiHourlyCapFleet"] = float64(200)
	if p, _ := capPreview(t, h, WriteOpAgentGitHubAPIHourlyCap, map[string]any{"agent": "reviewer", "cap": "inherit"}); isWidening(p) {
		t.Fatalf("inheriting 200 over 300 disclosed as widening: %q", p.WideningDisclosure)
	}
	// An uncapped agent cannot be widened.
	provider.agentGeneral["githubApiHourlyCapEffective"] = float64(0)
	if p, _ := capPreview(t, h, WriteOpAgentGitHubAPIHourlyCap, map[string]any{"agent": "reviewer", "cap": float64(900)}); isWidening(p) {
		t.Fatalf("capping an uncapped agent disclosed as widening: %q", p.WideningDisclosure)
	}
}

func TestAgentGitHubCapPreviewRefusesReplicasAndBadValues(t *testing.T) {
	provider := &capSettingsProvider{agentGeneral: map[string]any{"githubApiHourlyCapEffective": float64(600), "githubApiHourlyCapFleet": float64(300), "githubApiHourlyCapBase": "reviewer"}}
	h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAgentGitHubAPIHourlyCap, "args": map[string]any{"agent": "reviewer-2", "cap": float64(900)}})
	if err == nil || !strings.Contains(err.Error(), "set the cap on reviewer") {
		t.Fatalf("replica preview err = %v", err)
	}
	for _, bad := range []any{float64(-1), float64(1.5), "lots", nil} {
		if _, err := (agentGitHubCapOp{}).Preview(context.Background(), map[string]any{"agent": "reviewer", "cap": bad}); err == nil {
			t.Errorf("cap %v accepted", bad)
		}
	}
	if _, err := (fleetGitHubCapOp{}).Preview(context.Background(), map[string]any{"cap": "inherit"}); err == nil {
		t.Error("the fleet op accepted inherit; its keyword is default")
	}
}

// Without a reading of the current cap, the preview cannot rule widening out.
func TestGitHubCapPreviewWithoutProviderDisclosesPotentialWidening(t *testing.T) {
	p, err := (agentGitHubCapOp{}).Preview(context.Background(), map[string]any{"agent": "reviewer", "cap": float64(100)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.WideningDisclosure, "Potential widening:") || !namesBucketAndFloor(p) {
		t.Fatalf("disclosure = %q", p.WideningDisclosure)
	}
}

func TestFleetGitHubCapPreviewAndConfirm(t *testing.T) {
	provider := &capSettingsProvider{forge: map[string]any{"agent_github_api_hourly_cap": float64(300), "agent_github_api_hourly_cap_set": false}}
	h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))

	raise, id := capPreview(t, h, WriteOpGitHubAgentAPIHourlyCap, map[string]any{"cap": float64(600)})
	if !isWidening(raise) || !namesBucketAndFloor(raise) {
		t.Fatalf("raise disclosure = %q", raise.WideningDisclosure)
	}
	if raise.Request.Method != http.MethodPut || raise.Request.Path != "/api/config/github" {
		t.Fatalf("request = %s %s", raise.Request.Method, raise.Request.Path)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("writes = %+v", provider.requests)
	}
	got, _ := json.Marshal(provider.requests[0])
	if !strings.Contains(string(got), `"path":"/api/config/github"`) || !strings.Contains(string(got), `"agent_github_api_hourly_cap":600`) {
		t.Fatalf("executed %s", got)
	}

	if lower, _ := capPreview(t, h, WriteOpGitHubAgentAPIHourlyCap, map[string]any{"cap": float64(150)}); isWidening(lower) || namesBucketAndFloor(lower) {
		t.Fatalf("lowering disclosed as widening: %q", lower.WideningDisclosure)
	}
	if none, _ := capPreview(t, h, WriteOpGitHubAgentAPIHourlyCap, map[string]any{"cap": float64(0)}); !isWidening(none) {
		t.Fatalf("removing the cap not disclosed: %q", none.WideningDisclosure)
	}
	// "default" removes the key (null); from 600 back to 300 it is not widening.
	provider.forge["agent_github_api_hourly_cap"] = float64(600)
	reset, _ := capPreview(t, h, WriteOpGitHubAgentAPIHourlyCap, map[string]any{"cap": "default"})
	body, _ := json.Marshal(reset.Request.Body)
	if string(body) != `{"agent_github_api_hourly_cap":null}` || isWidening(reset) {
		t.Fatalf("reset body = %s, disclosure = %q", body, reset.WideningDisclosure)
	}
}

// The agent op confirms through the same General endpoint the dashboard uses.
func TestAgentGitHubCapConfirmExecutesGeneralSave(t *testing.T) {
	provider := &capSettingsProvider{agentGeneral: map[string]any{"githubApiHourlyCapEffective": float64(300), "githubApiHourlyCapFleet": float64(300), "githubApiHourlyCapBase": ""}}
	h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
	_, id := capPreview(t, h, WriteOpAgentGitHubAPIHourlyCap, map[string]any{"agent": "reviewer", "cap": "inherit"})
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || provider.requests[0].Path != "/api/config/agent/reviewer/general" {
		t.Fatalf("writes = %+v", provider.requests)
	}
	got, _ := json.Marshal(provider.requests[0].Body)
	if string(got) != `{"githubApiHourlyCap":null}` {
		t.Fatalf("body = %s", got)
	}
}

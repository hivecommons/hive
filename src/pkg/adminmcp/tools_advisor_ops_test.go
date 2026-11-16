package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// advisorProvider answers settings_read section advisor with a JSON-decoded
// copy of section, as GET /api/config/advisor answers, and records writes.
type advisorProvider struct {
	section  map[string]any
	requests []WriteRequest
}

func (p *advisorProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != advisorSettingsSection {
		return map[string]any{}, nil
	}
	b, err := json.Marshal(p.section)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (p *advisorProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return p.section, nil
}

// advisorTestProvider is a hive with the advisor off, one role, scanner on
// copilot inheriting the fleet, and reviewer on claude opted in with its own
// model.
func advisorTestProvider() *advisorProvider {
	return &advisorProvider{section: map[string]any{
		"fleet":       map[string]any{"enabled": false, "daily_budget_tokens": 50000, "model": "@advisor"},
		"model_roles": map[string]any{"advisor": map[string]any{"model": "claude-haiku-5-5"}, "cheap": map[string]any{"model": "gpt-mini"}},
		"defaults":    map[string]any{"timeout_s": 30, "max_consecutive_blocks": 3, "block_bound": 8},
		"agents": []any{
			map[string]any{"name": "scanner", "backend": "copilot", "override": nil,
				"effective": map[string]any{"enabled": false, "model": "@advisor", "daily_budget_tokens": 50000, "timeout_s": 30, "max_consecutive_blocks": 3}},
			map[string]any{"name": "reviewer", "backend": "claude", "override": map[string]any{"enabled": true, "model": "@cheap"},
				"effective": map[string]any{"enabled": true, "model": "@cheap", "daily_budget_tokens": 50000, "timeout_s": 30, "max_consecutive_blocks": 3}},
			map[string]any{"name": "triage", "backend": "goose", "override": nil,
				"effective": map[string]any{"enabled": false, "model": "@advisor", "daily_budget_tokens": 50000, "timeout_s": 30, "max_consecutive_blocks": 3}},
		},
	}}
}

func advisorHandler(p *advisorProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func advisorPreview(t *testing.T, h *Handler, op string, args map[string]any) (WritePreview, string) {
	t.Helper()
	out, err := h.PreviewWrite(context.Background(), map[string]any{"operation": op, "args": args})
	if err != nil {
		t.Fatalf("%s %v: preview: %v", op, args, err)
	}
	m := out.(map[string]any)
	return m["preview"].(WritePreview), m["confirmation_id"].(string)
}

func TestAdvisorWriteOpsRegistered(t *testing.T) {
	for _, name := range []string{WriteOpAdvisorSettings, WriteOpAdvisorAgent, WriteOpAdvisorRoles} {
		if _, ok := DefaultWriteRegistry().Get(name); !ok {
			t.Fatalf("%s is not registered", name)
		}
	}
}

// advisor.settings sends only the named fields to PUT /api/config/advisor,
// shows them before and after, and discloses turning the advisor on as
// widening; write_confirm returns the endpoint's refreshed section.
func TestAdvisorSettingsSendsOnlyNamedFields(t *testing.T) {
	p := advisorTestProvider()
	h := advisorHandler(p)
	preview, id := advisorPreview(t, h, WriteOpAdvisorSettings, map[string]any{"enabled": true, "timeout_s": float64(45)})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/advisor" {
		t.Fatalf("request = %+v", preview.Request)
	}
	if got := bodyJSON(t, preview.Request.Body); got != `{"enabled":true,"timeout_s":45}` {
		t.Fatalf("body = %s", got)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "Widening:") || !strings.Contains(preview.WideningDisclosure, "starts reviewing") {
		t.Fatalf("disclosure = %q", preview.WideningDisclosure)
	}
	if !strings.Contains(strings.Join(preview.Effects, " "), "reviewer") {
		t.Fatalf("effects do not name the agent with its own override: %v", preview.Effects)
	}
	out, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != `{"enabled":true,"timeout_s":45}` {
		t.Fatalf("executed %+v", p.requests)
	}
	if out.(map[string]any)["result"] == nil {
		t.Fatal("write_confirm did not return the endpoint's answer")
	}
}

// Raising the daily budget, removing it (0), or raising the blocker limit
// above the default is widening only while the advisor is on; lowering is
// not.
func TestAdvisorSettingsWidening(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		args    map[string]any
		widen   bool
	}{
		{"raise budget while on", true, map[string]any{"daily_budget_tokens": float64(90000)}, true},
		{"remove budget while on", true, map[string]any{"daily_budget_tokens": float64(0)}, true},
		{"lower budget while on", true, map[string]any{"daily_budget_tokens": float64(10000)}, false},
		{"raise budget while off", false, map[string]any{"daily_budget_tokens": float64(90000)}, false},
		{"blocks above the default", true, map[string]any{"max_consecutive_blocks": float64(5)}, true},
		{"blocks below the default", true, map[string]any{"max_consecutive_blocks": float64(2)}, false},
		{"turn off", true, map[string]any{"enabled": false}, false},
		{"instructions only", true, map[string]any{"instructions": "Flag scope creep."}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := advisorTestProvider()
			p.section["fleet"].(map[string]any)["enabled"] = tc.enabled
			preview, _ := advisorPreview(t, advisorHandler(p), WriteOpAdvisorSettings, tc.args)
			if got := strings.HasPrefix(preview.WideningDisclosure, "Widening:"); got != tc.widen {
				t.Fatalf("widening = %v, want %v: %q", got, tc.widen, preview.WideningDisclosure)
			}
		})
	}
}

// Bad arguments are refused with their reason, before anything is read or
// written.
func TestAdvisorSettingsRefusesBadArgs(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{}, "name at least one field"},
		{map[string]any{"enabeld": true}, `unknown field "enabeld"`},
		{map[string]any{"enabled": "yes"}, "enabled must be true or false"},
		{map[string]any{"model": nil}, "model is null"},
		{map[string]any{"timeout_s": float64(-1)}, "timeout_s must be a whole number"},
		{map[string]any{"daily_budget_tokens": 1.5}, "daily_budget_tokens must be a whole number"},
	} {
		p := advisorTestProvider()
		_, err := advisorHandler(p).PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAdvisorSettings, "args": tc.args})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
		if len(p.requests) != 0 {
			t.Errorf("%v: refused args wrote %+v", tc.args, p.requests)
		}
	}
}

// A dashboard edit to the advisor settings between preview and confirm makes
// the preview's before/after wrong, so the confirm is refused.
func TestAdvisorWriteOpsRefuseStalePreview(t *testing.T) {
	for _, tc := range []struct {
		op     string
		args   map[string]any
		change func(p *advisorProvider)
	}{
		{WriteOpAdvisorSettings, map[string]any{"enabled": true}, func(p *advisorProvider) {
			p.section["fleet"].(map[string]any)["model"] = "other"
		}},
		{WriteOpAdvisorAgent, map[string]any{"agent": "reviewer", "override": "inherit"}, func(p *advisorProvider) {
			p.section["agents"].([]any)[1].(map[string]any)["override"] = map[string]any{"enabled": false}
		}},
		{WriteOpAdvisorRoles, map[string]any{"set": map[string]any{"deep": map[string]any{"model": "claude-opus-5-5"}}}, func(p *advisorProvider) {
			p.section["model_roles"].(map[string]any)["late"] = map[string]any{"model": "x"}
		}},
	} {
		p := advisorTestProvider()
		h := advisorHandler(p)
		_, id := advisorPreview(t, h, tc.op, tc.args)
		tc.change(p)
		if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
			t.Errorf("%s: confirm err = %v, want ErrConfirmationStale", tc.op, err)
		}
		if len(p.requests) != 0 {
			t.Errorf("%s: a stale confirmation wrote %+v", tc.op, p.requests)
		}
	}
}

// advisor.agent sends the agent's whole override; fields the old override
// set and the new one leaves out are named as cleared, and "inherit" sends
// null.
func TestAdvisorAgentOverride(t *testing.T) {
	p := advisorTestProvider()
	h := advisorHandler(p)

	preview, _ := advisorPreview(t, h, WriteOpAdvisorAgent, map[string]any{"agent": "reviewer", "override": map[string]any{"enabled": true}})
	if preview.Request.Path != "/api/config/advisor/agent/reviewer" || bodyJSON(t, preview.Request.Body) != `{"override":{"enabled":true}}` {
		t.Fatalf("request = %+v", preview.Request)
	}
	if cleared, _ := preview.Details["cleared"].([]string); len(cleared) != 1 || cleared[0] != "model" {
		t.Fatalf("cleared = %#v", preview.Details["cleared"])
	}
	if strings.HasPrefix(preview.WideningDisclosure, "Widening:") {
		t.Fatalf("keeping an opted-in agent on is not widening: %q", preview.WideningDisclosure)
	}

	preview, _ = advisorPreview(t, h, WriteOpAdvisorAgent, map[string]any{"agent": "reviewer", "override": "inherit"})
	if bodyJSON(t, preview.Request.Body) != `{"override":null}` {
		t.Fatalf("inherit body = %s", bodyJSON(t, preview.Request.Body))
	}

	preview, _ = advisorPreview(t, h, WriteOpAdvisorAgent, map[string]any{"agent": "scanner", "override": map[string]any{"enabled": true}})
	if !strings.HasPrefix(preview.WideningDisclosure, "Widening:") || !strings.Contains(preview.WideningDisclosure, "agent scanner") {
		t.Fatalf("opting scanner in: %q", preview.WideningDisclosure)
	}

	preview, _ = advisorPreview(t, h, WriteOpAdvisorAgent, map[string]any{"agent": "scanner", "override": map[string]any{"enabled": false}})
	if !strings.HasPrefix(preview.Summary, "Opt agent scanner out") {
		t.Fatalf("summary = %q", preview.Summary)
	}

	// An agent on a backend without an advisor adapter is told so.
	preview, _ = advisorPreview(t, h, WriteOpAdvisorAgent, map[string]any{"agent": "triage", "override": map[string]any{"enabled": true}})
	if !strings.Contains(strings.Join(preview.Effects, " "), "not active on this agent's backend (goose") {
		t.Fatalf("effects = %v", preview.Effects)
	}

	for _, args := range []map[string]any{
		{"agent": "ghost", "override": "inherit"},
		{"agent": "scanner", "override": "maybe"},
		{"agent": "scanner", "override": map[string]any{"enabled": true, "colour": "red"}},
		{"agent": "scanner"},
	} {
		if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAdvisorAgent, "args": args}); err == nil {
			t.Errorf("%v was not refused", args)
		}
	}
}

// advisor.roles sends the whole map back with only the named roles changed,
// and a role the advisor resolves is disclosed as a possible cost change.
func TestAdvisorRolesMergesOntoCurrentMap(t *testing.T) {
	p := advisorTestProvider()
	h := advisorHandler(p)

	preview, id := advisorPreview(t, h, WriteOpAdvisorRoles, map[string]any{
		"set":    map[string]any{"advisor": map[string]any{"model": "claude-sonnet-5-5", "reasoning_effort": "high"}},
		"remove": []any{"cheap"},
	})
	if got := bodyJSON(t, preview.Request.Body); got != `{"model_roles":{"advisor":{"model":"claude-sonnet-5-5","reasoning_effort":"high"}}}` {
		t.Fatalf("body = %s", got)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "Potential widening:") || !strings.Contains(preview.WideningDisclosure, "the fleet setting") || !strings.Contains(preview.WideningDisclosure, "agent reviewer") {
		t.Fatalf("disclosure = %q", preview.WideningDisclosure)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}

	preview, _ = advisorPreview(t, h, WriteOpAdvisorRoles, map[string]any{"set": map[string]any{"deep": map[string]any{"model": "claude-opus-5-5"}}})
	if got := bodyJSON(t, preview.Request.Body); !strings.Contains(got, `"deep":{"model":"claude-opus-5-5"}`) || !strings.Contains(got, `"cheap":{"model":"gpt-mini"}`) || !strings.Contains(got, `"advisor":{"model":"claude-haiku-5-5"}`) {
		t.Fatalf("adding a role must keep the others: %s", got)
	}
	if !strings.HasPrefix(preview.WideningDisclosure, "No widening:") {
		t.Fatalf("an unused new role: %q", preview.WideningDisclosure)
	}

	for _, args := range []map[string]any{
		{},
		{"remove": []any{"missing"}},
		{"set": map[string]any{"bad name": map[string]any{"model": "m"}}},
		{"set": map[string]any{"x": map[string]any{"backend": "claude"}}},
		{"set": map[string]any{"x": map[string]any{"model": "m"}}, "remove": []any{"x"}},
	} {
		if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAdvisorRoles, "args": args}); err == nil {
			t.Errorf("%v was not refused", args)
		}
	}
}

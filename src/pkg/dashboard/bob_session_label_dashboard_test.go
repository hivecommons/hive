package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestBobSessionLabelUIWiring(t *testing.T) {
	html := indexHTML(t)
	cases := []struct {
		name    string
		snippet string
	}{
		{"global prefix label", "Bob session prefix"},
		{"global prefix dirty path", `data-arg0="bob" data-arg1="sessionPrefix"`},
		{"agent label field", "Bob session label"},
		{"agent label handler", "function onBobSessionLabelChange(el)"},
		{"effective label hint", "cfg-bob-session-effective"},
		{"bob-only visibility helper", "function updateBobSessionLabelVisibility()"},
		{"no alert", "window.alert("},
		{"no confirm", "window.confirm("},
		{"no prompt", "window.prompt("},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			has := strings.Contains(html, tc.snippet)
			if strings.HasPrefix(tc.name, "no ") {
				if has {
					t.Fatalf("index.html must not use %s", tc.snippet)
				}
				return
			}
			if !has {
				t.Fatalf("index.html is missing %q", tc.snippet)
			}
		})
	}
}

func TestBobSessionLabelAgentSaveReload(t *testing.T) {
	s := acfgServer(t)
	agentCfg := s.deps.Config.Agents["scanner"]
	agentCfg.Backend = "bob"
	s.deps.Config.Agents["scanner"] = agentCfg

	rec := doPut(s, "/api/config/agent/scanner/general", map[string]any{"bobSessionLabel": "hive-scanner"})
	if rec.Code != http.StatusOK {
		t.Fatalf("save label: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Agents["scanner"].Bob.SessionLabel; got != "hive-scanner" {
		t.Fatalf("config label = %q, want hive-scanner", got)
	}

	get := doGet(s, "/api/config/agent/scanner")
	if get.Code != http.StatusOK {
		t.Fatalf("reload label: want 200, got %d (%s)", get.Code, get.Body.String())
	}
	var payload struct {
		General map[string]any `json:"general"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload.General["bobSessionLabel"]; got != "hive-scanner" {
		t.Fatalf("reloaded bobSessionLabel = %v, want hive-scanner", got)
	}

	bad := doPut(s, "/api/config/agent/scanner/general", map[string]any{"bobSessionLabel": "bad label"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid label: want 400, got %d", bad.Code)
	}

	s.deps.Config.Governor.Bob.SessionPrefix = strings.Repeat("a", 58)
	clear := doPut(s, "/api/config/agent/scanner/general", map[string]any{"bobSessionLabel": ""})
	if clear.Code != http.StatusBadRequest {
		t.Fatalf("clear with oversized prefix fallback: want 400, got %d", clear.Code)
	}
}

func TestBobSessionPrefixGovernorSaveReload(t *testing.T) {
	s := covApiServer(t)

	rec := doPut(s, "/api/config/governor/bob", map[string]any{"sessionPrefix": "hive-"})
	if rec.Code != http.StatusOK {
		t.Fatalf("save prefix: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Bob.SessionPrefix; got != "hive-" {
		t.Fatalf("config prefix = %q, want hive-", got)
	}

	get := doGet(s, "/api/config/governor/bob")
	if get.Code != http.StatusOK {
		t.Fatalf("reload prefix: want 200, got %d (%s)", get.Code, get.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["sessionPrefix"]; got != "hive-" {
		t.Fatalf("reloaded sessionPrefix = %v, want hive-", got)
	}

	bad := doPut(s, "/api/config/governor/bob", map[string]any{"sessionPrefix": "bad/prefix"})
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid prefix: want 400, got %d", bad.Code)
	}
}

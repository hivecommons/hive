package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/rotation"
)

// The headroom response names, per provider, the enabled agents whose backend
// spends its plan, so the usage gauge shows one row per provider in use. A
// Claude model run through Copilot counts against Copilot; disabled agents and
// unknown backends are left out; a backend listed under rotation.providers
// follows that listing.
func TestProvidersHeadroomUsedBy(t *testing.T) {
	cfg := &config.Config{}
	cfg.Agents = map[string]config.AgentConfig{
		"scanner":  {Enabled: true, Backend: "copilot", Model: "claude-sonnet-5.5"},
		"reviewer": {Enabled: true, Backend: "copilot"},
		"fixer":    {Enabled: true, Backend: "claude"},
		"quiet":    {Enabled: false, Backend: "claude"},
		"router":   {Enabled: true, Backend: "litellm"},
		"odd":      {Enabled: true, Backend: "something-else"},
		"blank":    {Enabled: true},
	}
	cfg.Governor.Rotation.Providers = map[string]config.ProviderRotationConfig{
		"deepseek": {Class: rotation.ClassMetered, Backends: []string{"litellm"}},
	}
	s := &Server{deps: &Dependencies{Config: cfg}}
	req := httptest.NewRequest(http.MethodGet, "/api/providers/headroom", nil)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.handleProvidersHeadroom(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%q", rec.Code, rec.Body.String())
	}
	var body struct {
		Providers []any               `json:"providers"`
		UsedBy    map[string][]string `json:"used_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	want := map[string][]string{
		"github":    {"reviewer", "scanner"},
		"anthropic": {"fixer"},
		"deepseek":  {"router"},
	}
	if !reflect.DeepEqual(body.UsedBy, want) {
		t.Fatalf("used_by = %v, want %v", body.UsedBy, want)
	}
	if body.Providers == nil {
		t.Fatalf("providers must stay present: %q", rec.Body.String())
	}
}

func TestUsageProviderForBackend(t *testing.T) {
	configured := map[string]config.ProviderRotationConfig{"anthropic": {Backends: []string{"pi"}}}
	for backend, want := range map[string]string{
		"claude": "anthropic", "Copilot": "github", "codex": "openai", "agy": "google",
		"gemini": "google", "kiro": "aws-kiro", "deepseek": "deepseek", "pi": "anthropic",
		"": "", "unknown": "",
	} {
		if got := rotation.UsageProviderForBackend(backend, configured); got != want {
			t.Errorf("UsageProviderForBackend(%q) = %q, want %q", backend, got, want)
		}
	}
}

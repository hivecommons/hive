package rotation

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func deepSeekServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDeepSeekProber_Available(t *testing.T) {
	srv := deepSeekServer(t, http.StatusOK, `{"is_available":true,"total_balance":"8.42"}`)
	p := DeepSeekProber{APIKey: "test-key", BaseURL: srv.URL}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true")
	}
	if h.PctRemaining != 100 {
		t.Errorf("PctRemaining = %d, want 100", h.PctRemaining)
	}
}

func TestDeepSeekProber_Exhausted(t *testing.T) {
	srv := deepSeekServer(t, http.StatusOK, `{"is_available":true,"total_balance":"0.42"}`)
	p := DeepSeekProber{APIKey: "test-key", BaseURL: srv.URL}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if h.Available {
		t.Error("Available = true, want false (balance < $1.00)")
	}
}

func TestDeepSeekProber_HTTPError(t *testing.T) {
	srv := deepSeekServer(t, http.StatusInternalServerError, `oops`)
	p := DeepSeekProber{APIKey: "test-key", BaseURL: srv.URL}
	h := p.Probe(context.Background())
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want error")
	}
	if !h.Available {
		t.Error("Available = false, want true (fail-open on probe failure)")
	}
}

func rotationTestConfig() config.RotationConfig {
	return config.RotationConfig{
		Enabled: true,
		Providers: map[string]config.ProviderRotationConfig{
			"anthropic": {Class: ClassSubscription, Backends: []string{"claude", "pi"}},
			"openai":    {Class: ClassSubscription, Backends: []string{"codex"}},
			"deepseek":  {Class: ClassMetered, Backends: []string{"litellm"}},
		},
		AgentTiers: map[string]string{"worker": "T1"},
	}
}

func TestManager_ShouldRotate_ExhaustedProvider(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, PctRemaining: 5})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 60})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: true, PctRemaining: 100})
	if !m.ShouldRotate("worker", "claude", 14400) {
		t.Error("ShouldRotate = false, want true (anthropic exhausted, alternatives exist)")
	}
}

func TestManager_ShouldRotate_ProbeError(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, ProbeErr: errors.New("probe failed")})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 60})
	if m.ShouldRotate("worker", "claude", 14400) {
		t.Error("ShouldRotate = true, want false (never rotate on failed measurement)")
	}
}

func TestManager_ShouldRotate_HealthyProvider(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: true, PctRemaining: 50})
	if m.ShouldRotate("worker", "claude", 14400) {
		t.Error("ShouldRotate = true, want false (provider has headroom)")
	}
}

func TestManager_NextBackend_SameTier(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, PctRemaining: 5})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 60})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false, PctRemaining: 0})
	if got := m.NextBackend("worker", "claude"); got != "codex" {
		t.Errorf("NextBackend = %q, want %q", got, "codex")
	}
}

func TestManager_NextBackend_NoHeadroom(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: false})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false})
	if got := m.NextBackend("worker", "claude"); got != "" {
		t.Errorf("NextBackend = %q, want \"\" (strand)", got)
	}
}

func TestManager_NextBackend_NeverOntoProbeError(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, ProbeErr: errors.New("x")})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false})
	if got := m.NextBackend("worker", "claude"); got != "" {
		t.Errorf("NextBackend = %q, want \"\" (fail-open target never receives load)", got)
	}
}

func TestManager_HighVolumeCadence(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 90})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false})
	// 300s cadence is high-volume: openai (subscription) is off-limits even
	// though it has headroom, and deepseek has none → strand.
	if got := m.NextBackendForCadence("worker", "claude", 300); got != "" {
		t.Errorf("NextBackendForCadence = %q, want \"\" (high-volume must not use subscription)", got)
	}
	// The metered provider is allowed once it has headroom.
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: true, PctRemaining: 100})
	if got := m.NextBackendForCadence("worker", "claude", 300); got != "litellm" {
		t.Errorf("NextBackendForCadence = %q, want %q", got, "litellm")
	}
}

func TestManager_HighVolumeMeteredExhaustionFailsOverToSubscription(t *testing.T) {
	m := NewManager(rotationTestConfig())
	// worker is on the metered DeepSeek/litellm rung. Its balance is positively
	// measured exhausted, while Codex/OpenAI has headroom. A high-cadence
	// agent must fail over rather than strand.
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false, PctRemaining: 0})
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 90})
	if got := m.NextBackendForCadence("worker", "litellm", 300); got != "codex" {
		t.Errorf("NextBackendForCadence = %q, want %q (metered exhaustion must fail over)", got, "codex")
	}
}

func TestManager_StrandRecovered(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	if m.StrandRecovered("claude") {
		t.Error("StrandRecovered = true, want false")
	}
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: true, PctRemaining: 100})
	if !m.StrandRecovered("claude") {
		t.Error("StrandRecovered = false, want true")
	}
	// A probe error is not recovery.
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: true, ProbeErr: errors.New("x")})
	if m.StrandRecovered("claude") {
		t.Error("StrandRecovered = true on probe error, want false")
	}
}

func TestManager_HeadroomResponse(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 60})
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, PctRemaining: 5})
	resp := m.HeadroomResponse()
	if len(resp.Providers) != 2 {
		t.Fatalf("len(Providers) = %d, want 2", len(resp.Providers))
	}
	if resp.Providers[0].Provider != "anthropic" || resp.Providers[1].Provider != "openai" {
		t.Errorf("providers not sorted: %+v", resp.Providers)
	}
	if resp.UpdatedAt.IsZero() {
		t.Error("UpdatedAt is zero")
	}
}

func TestManager_UnknownBackend(t *testing.T) {
	m := NewManager(rotationTestConfig())
	if m.ShouldRotate("worker", "unmapped-backend", 0) {
		t.Error("ShouldRotate = true for unmapped backend, want false")
	}
	if m.Exhausted("unmapped-backend") {
		t.Error("Exhausted = true for unmapped backend, want false")
	}
}

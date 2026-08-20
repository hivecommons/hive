package rotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

// fakeCLI installs an executable shell script named `name` on PATH that
// prints `output` and exits with `exitCode`.
func fakeCLI(t *testing.T, name, output string, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", output, exitCode)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestClaudeProber_UsedBelowThreshold(t *testing.T) {
	fakeCLI(t, "claude", "Current week (all models): 40% used", 0)
	p := ClaudeProber{ThresholdPct: 80}
	h := p.Probe(context.Background())
	if p.Provider() != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", p.Provider())
	}
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true (40 used < 80 threshold)")
	}
	if h.PctRemaining != 60 {
		t.Errorf("PctRemaining = %d, want 60", h.PctRemaining)
	}
}

func TestClaudeProber_Exhausted(t *testing.T) {
	fakeCLI(t, "claude", "Current week (all models): 95% used", 0)
	h := ClaudeProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if h.Available {
		t.Error("Available = true, want false (95 used >= 80 threshold)")
	}
}

func TestClaudeProber_UnparseableOutput(t *testing.T) {
	fakeCLI(t, "claude", "unexpected output", 0)
	h := ClaudeProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want parse error")
	}
	if !h.Available {
		t.Error("Available = false, want true (fail-open)")
	}
}

func TestClaudeProber_CommandFailure(t *testing.T) {
	fakeCLI(t, "claude", "boom", 1)
	h := ClaudeProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want command error")
	}
	if !h.Available {
		t.Error("Available = false, want true (fail-open)")
	}
}

func TestCodexProber(t *testing.T) {
	fakeCLI(t, "codex", "Weekly limit: 70% remaining", 0)
	p := CodexProber{ThresholdPct: 80}
	if p.Provider() != "openai" {
		t.Errorf("Provider = %q, want openai", p.Provider())
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true (30 used < 80 threshold)")
	}
	if h.PctRemaining != 70 {
		t.Errorf("PctRemaining = %d, want 70", h.PctRemaining)
	}
}

func TestCodexProber_ExhaustedAndErrors(t *testing.T) {
	fakeCLI(t, "codex", "Weekly limit: 5% remaining", 0)
	h := CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.Available {
		t.Error("Available = true, want false (95 used >= 80 threshold)")
	}

	fakeCLI(t, "codex", "garbage", 0)
	h = CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with parse error on garbage output")
	}

	fakeCLI(t, "codex", "x", 2)
	h = CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on non-zero exit")
	}
}

func TestAgyProber(t *testing.T) {
	fakeCLI(t, "agy", "Weekly Limit Remaining: 55%", 0)
	p := AgyProber{ThresholdPct: 80}
	if p.Provider() != "google" {
		t.Errorf("Provider = %q, want google", p.Provider())
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true (45 used < 80 threshold)")
	}
	if h.PctRemaining != 55 {
		t.Errorf("PctRemaining = %d, want 55", h.PctRemaining)
	}
}

func TestAgyProber_ExhaustedAndErrors(t *testing.T) {
	fakeCLI(t, "agy", "Weekly Limit Remaining: 10%", 0)
	h := AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.Available {
		t.Error("Available = true, want false (90 used >= 80 threshold)")
	}

	fakeCLI(t, "agy", "nope", 0)
	h = AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with parse error on unmatched output")
	}

	fakeCLI(t, "agy", "x", 3)
	h = AgyProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open with error on non-zero exit")
	}
}

func TestDeepSeekProber_BadJSONAndBadBalance(t *testing.T) {
	srv := deepSeekServer(t, http.StatusOK, `not-json`)
	h := DeepSeekProber{APIKey: "test-key", BaseURL: srv.URL}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open on unparseable JSON")
	}

	srv2 := deepSeekServer(t, http.StatusOK, `{"is_available":true,"total_balance":"NaN$"}`)
	h = DeepSeekProber{APIKey: "test-key", BaseURL: srv2.URL}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open on unparseable balance string")
	}
}

func TestDeepSeekProber_BalanceInfosFallback(t *testing.T) {
	srv := deepSeekServer(t, http.StatusOK, `{"is_available":true,"balance_infos":[{"total_balance":"3.50"}]}`)
	p := DeepSeekProber{APIKey: "test-key", BaseURL: srv.URL}
	if p.Provider() != "deepseek" {
		t.Errorf("Provider = %q, want deepseek", p.Provider())
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if !h.Available || h.PctRemaining != 100 {
		t.Errorf("Available=%v PctRemaining=%d, want true/100", h.Available, h.PctRemaining)
	}
}

func TestDeepSeekProber_ConnectionRefused(t *testing.T) {
	h := DeepSeekProber{APIKey: "test-key", BaseURL: "http://127.0.0.1:1"}.Probe(context.Background())
	if h.ProbeErr == nil || !h.Available {
		t.Error("want fail-open when the balance endpoint is unreachable")
	}
}

func TestHeadroom_ProbeErrorAndMarshalJSON(t *testing.T) {
	h := Headroom{Provider: "anthropic", Available: true, PctRemaining: 42}
	if h.ProbeError() != "" {
		t.Errorf("ProbeError = %q, want empty", h.ProbeError())
	}
	h.ProbeErr = errors.New("probe blew up")
	if h.ProbeError() != "probe blew up" {
		t.Errorf("ProbeError = %q", h.ProbeError())
	}
	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["provider"] != "anthropic" {
		t.Errorf("provider = %v", decoded["provider"])
	}
	if decoded["probe_error"] != "probe blew up" {
		t.Errorf("probe_error = %v", decoded["probe_error"])
	}
	if decoded["pct_remaining"] != float64(42) {
		t.Errorf("pct_remaining = %v", decoded["pct_remaining"])
	}
}

func TestNewManager_DefaultProbers(t *testing.T) {
	cfg := config.RotationConfig{
		Enabled: true,
		Providers: map[string]config.ProviderRotationConfig{
			"anthropic": {Class: ClassSubscription, Backends: []string{"claude"}},
			"openai":    {Class: ClassSubscription, Backends: []string{"codex"}},
			"google":    {Class: ClassSubscription, Backends: []string{"agy"}},
			"deepseek":  {Class: ClassMetered, Backends: []string{"litellm"}},
			"unknown":   {Class: ClassMetered, Backends: []string{"other"}},
		},
	}
	m := NewManager(cfg)
	if len(m.probers) != 4 {
		t.Fatalf("len(probers) = %d, want 4 (unknown provider gets none)", len(m.probers))
	}
	got := map[string]bool{}
	for _, p := range m.probers {
		got[p.Provider()] = true
	}
	for _, want := range []string{"anthropic", "openai", "google", "deepseek"} {
		if !got[want] {
			t.Errorf("missing default prober for %q", want)
		}
	}
}

// stubProber records probes and returns a canned headroom.
type stubProber struct {
	name   string
	h      Headroom
	probed chan struct{}
}

func (s *stubProber) Provider() string { return s.name }

func (s *stubProber) Probe(context.Context) Headroom {
	select {
	case s.probed <- struct{}{}:
	default:
	}
	return s.h
}

func TestManager_StartProbesAndStores(t *testing.T) {
	m := NewManager(rotationTestConfig())
	stub := &stubProber{
		name:   "anthropic",
		h:      Headroom{Provider: "anthropic", Available: false, PctRemaining: 3},
		probed: make(chan struct{}, 1),
	}
	m.SetProbers([]Prober{stub})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	select {
	case <-stub.probed:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not probe within 5s")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		h := m.HeadroomFor("anthropic")
		if h.ProbeErr == nil {
			if h.Available || h.PctRemaining != 3 {
				t.Errorf("stored headroom = %+v", h)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe result never stored")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
}

func TestManager_HeadroomFor_NeverProbed(t *testing.T) {
	m := NewManager(rotationTestConfig())
	h := m.HeadroomFor("anthropic")
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want never-probed error")
	}
	if !h.Available {
		t.Error("Available = false, want true (fail-open)")
	}
	if !strings.Contains(h.ProbeErr.Error(), "never probed") {
		t.Errorf("ProbeErr = %v", h.ProbeErr)
	}
}

func TestManager_Exhausted(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, PctRemaining: 0})
	if !m.Exhausted("claude") {
		t.Error("Exhausted = false, want true (positive exhaustion measurement)")
	}
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false, ProbeErr: errors.New("x")})
	if m.Exhausted("claude") {
		t.Error("Exhausted = true on probe error, want false")
	}
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: true, PctRemaining: 50})
	if m.Exhausted("claude") {
		t.Error("Exhausted = true for available provider, want false")
	}
}

func TestManager_StrandRecovered_UnknownBackend(t *testing.T) {
	m := NewManager(rotationTestConfig())
	if m.StrandRecovered("unmapped-backend") {
		t.Error("StrandRecovered = true for unmapped backend, want false")
	}
}

func TestManager_ShouldRotate_NoAlternative(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: false})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false})
	if m.ShouldRotate("worker", "claude", 14400) {
		t.Error("ShouldRotate = true, want false (nowhere to go)")
	}
}

func TestManager_NextBackend_PrefersHighestHeadroom(t *testing.T) {
	m := NewManager(rotationTestConfig())
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 40})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: true, PctRemaining: 100})
	if got := m.NextBackend("worker", "claude"); got != "litellm" {
		t.Errorf("NextBackend = %q, want litellm (highest headroom wins)", got)
	}
}

func TestManager_NextBackend_SkipsEmptyBackendProvider(t *testing.T) {
	cfg := rotationTestConfig()
	cfg.Providers["google"] = config.ProviderRotationConfig{Class: ClassSubscription}
	m := NewManager(cfg)
	m.SetHeadroom(Headroom{Provider: "anthropic", Available: false})
	m.SetHeadroom(Headroom{Provider: "google", Available: true, PctRemaining: 100})
	m.SetHeadroom(Headroom{Provider: "openai", Available: true, PctRemaining: 60})
	m.SetHeadroom(Headroom{Provider: "deepseek", Available: false})
	if got := m.NextBackend("worker", "claude"); got != "codex" {
		t.Errorf("NextBackend = %q, want codex (google has no backends)", got)
	}
}

func TestRunCLI_MissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	out, err := runCLI(context.Background(), "definitely-not-a-real-binary-4232")
	if err == nil {
		t.Fatalf("err = nil, out = %q; want lookup error", out)
	}
}

package governor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackendProviderMapping(t *testing.T) {
	tests := []struct {
		backend  string
		provider string
		class    ProviderClass
	}{
		{"claude", "anthropic", ProviderClassSubscription},
		{"litellm", "anthropic", ProviderClassSubscription},
		{"copilot", "openai", ProviderClassSubscription},
		{"codex", "openai", ProviderClassSubscription},
		{"deepseek", "deepseek", ProviderClassMetered},
		{"agy", "google", ProviderClassUnmeasurable},
		{"bob", "ibm", ProviderClassUnmeasurable},
		{"goose", "goose", ProviderClassUnmeasurable},
		{"pi", "pi", ProviderClassUnmeasurable},
		{"aider", "aider", ProviderClassUnmeasurable},
		{"unknown_backend", "unknown", ProviderClassUnmeasurable},
	}

	for _, tt := range tests {
		t.Run(tt.backend, func(t *testing.T) {
			p := BackendProvider(tt.backend)
			if p != tt.provider {
				t.Errorf("BackendProvider(%q) = %q, want %q", tt.backend, p, tt.provider)
			}
			c := DefaultProviderClass(p)
			if c != tt.class {
				t.Errorf("DefaultProviderClass(%q) = %q, want %q", p, c, tt.class)
			}
		})
	}
}

func TestBackendProviderMatchesBackendsConf(t *testing.T) {
	// Verify Go BackendProvider agrees with bash backend_provider in config/backends.conf
	confPath := filepath.Join("..", "..", "..", "config", "backends.conf")
	if _, err := os.Stat(confPath); err != nil {
		t.Skipf("backends.conf not found at %s: %v", confPath, err)
	}

	backends := []string{"claude", "copilot", "codex", "agy", "bob", "litellm", "goose", "pi", "aider"}
	for _, b := range backends {
		cmd := exec.Command("bash", "-c", "source "+confPath+"; backend_provider "+b)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("failed to run backend_provider %s: %v", b, err)
		}
		bashProvider := strings.TrimSpace(string(out))
		goProvider := BackendProvider(b)
		if bashProvider != goProvider {
			t.Errorf("backend %q: backends.conf returned %q, Go BackendProvider returned %q", b, bashProvider, goProvider)
		}
	}
}

func TestParseHeadroomAnthropic(t *testing.T) {
	// 1. All models binding limit
	out1 := `
Claude Code Usage:
Current week (all models): 50% used
Resets in 3 days (19 Aug 2026)
Sonnet: 15% used
Haiku: 5% used
`
	hr, err := ParseHeadroomOutput("anthropic", out1)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if !hr.Available {
		t.Error("expected available")
	}
	if hr.UsagePct == nil || *hr.UsagePct != 50.0 {
		t.Errorf("expected UsagePct = 50.0, got %v", hr.UsagePct)
	}

	// 2. Generic match
	out2 := `Usage: 75.5% used`
	hr2, err := ParseHeadroomOutput("anthropic", out2)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if hr2.UsagePct == nil || *hr2.UsagePct != 75.5 {
		t.Errorf("expected UsagePct = 75.5, got %v", hr2.UsagePct)
	}

	// 3. Exhausted / 100%
	out3 := `Current week (all models): 100% used. Rate limit exceeded.`
	hr3, err := ParseHeadroomOutput("anthropic", out3)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if hr3.Available {
		t.Error("expected unavailable for 100% used")
	}

	// 4. Bad output
	out4 := `Some unrelated text with no numbers`
	hr4, err := ParseHeadroomOutput("anthropic", out4)
	if err == nil {
		t.Error("expected error for unparseable output")
	}
	if hr4.Error == "" {
		t.Error("expected error message populated in headroom record")
	}
}

func TestParseHeadroomOpenAI(t *testing.T) {
	// 1. Weekly limit left
	out1 := `Weekly limit: 88% left (resets 23:45 on 19 Aug)`
	hr, err := ParseHeadroomOutput("openai", out1)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if !hr.Available {
		t.Error("expected available")
	}
	if hr.UsagePct == nil || *hr.UsagePct != 12.0 {
		t.Errorf("expected UsagePct = 12.0, got %v", hr.UsagePct)
	}

	// 2. Weekly limit used
	out2 := `Weekly limit: 95% used`
	hr2, err := ParseHeadroomOutput("openai", out2)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if hr2.UsagePct == nil || *hr2.UsagePct != 95.0 {
		t.Errorf("expected UsagePct = 95.0, got %v", hr2.UsagePct)
	}

	// 3. 0% left (exhausted)
	out3 := `Weekly limit: 0% left`
	hr3, err := ParseHeadroomOutput("openai", out3)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if hr3.Available {
		t.Error("expected unavailable for 0% left")
	}
}

func TestParseHeadroomDeepSeek(t *testing.T) {
	// 1. Available with balance
	out1 := `{"is_available":true,"total_balance":"8.42"}`
	hr, err := ParseHeadroomOutput("deepseek", out1)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if !hr.Available {
		t.Error("expected available")
	}
	if hr.TotalBalance != "8.42" {
		t.Errorf("expected TotalBalance = '8.42', got %q", hr.TotalBalance)
	}

	// 2. Unavailable
	out2 := `{"is_available":false,"total_balance":"0.00"}`
	hr2, err := ParseHeadroomOutput("deepseek", out2)
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if hr2.Available {
		t.Error("expected unavailable")
	}

	// 3. Invalid JSON
	out3 := `not-json`
	_, err = ParseHeadroomOutput("deepseek", out3)
	if err == nil {
		t.Error("expected error for invalid json")
	}
}

func TestParseHeadroomUnmeasurable(t *testing.T) {
	hr, err := ParseHeadroomOutput("google", "anything")
	if err != nil {
		t.Fatalf("ParseHeadroomOutput error: %v", err)
	}
	if hr.Class != ProviderClassUnmeasurable {
		t.Errorf("expected class unmeasurable, got %v", hr.Class)
	}
	if !hr.Available {
		t.Error("expected available default for unmeasurable")
	}
}

func TestHeadroomTracker(t *testing.T) {
	tracker := NewHeadroomTracker()

	// Initial check
	if _, ok := tracker.GetHeadroom("anthropic"); ok {
		t.Error("expected no headroom recorded initially")
	}
	if len(tracker.AllHeadroom()) != 0 {
		t.Errorf("expected 0 headroom entries, got %d", len(tracker.AllHeadroom()))
	}

	// Record headroom
	u80 := 80.0
	hr1 := ProviderHeadroom{
		Provider:  "anthropic",
		Class:     ProviderClassSubscription,
		Available: true,
		UsagePct:  &u80,
		ProbedAt:  time.Now(),
	}
	tracker.RecordHeadroom(hr1)

	got, ok := tracker.GetHeadroom("anthropic")
	if !ok || got.UsagePct == nil || *got.UsagePct != 80.0 {
		t.Errorf("GetHeadroom failed, got %+v", got)
	}

	// IsExhausted check
	if tracker.IsExhausted("anthropic", 85.0) {
		t.Error("80% usage should not be exhausted at 85% threshold")
	}
	if !tracker.IsExhausted("anthropic", 75.0) {
		t.Error("80% usage should be exhausted at 75% threshold")
	}

	// Record strand
	sr := StrandRecord{
		Agent:       "supervisor",
		Provider:    "anthropic",
		Backend:     "claude",
		Timestamp:   time.Now(),
		Reason:      "anthropic pool exhausted (80% used)",
		CooldownEnd: time.Now().Add(24 * time.Hour),
		AutoResume:  true,
	}
	tracker.RecordStrand(sr)

	strands := tracker.ActiveStrands()
	if len(strands) != 1 || strands[0].Agent != "supervisor" {
		t.Errorf("ActiveStrands failed, got %+v", strands)
	}

	// Clear strand
	tracker.ClearStrand("supervisor")
	if len(tracker.ActiveStrands()) != 0 {
		t.Errorf("expected 0 strands after clear, got %d", len(tracker.ActiveStrands()))
	}
}

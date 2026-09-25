package rotation

import (
	"context"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ccleft "github.com/tuna-os/ccleft"
)

type fakeCCLeftClient struct{ reading ccleft.Reading }

func (f fakeCCLeftClient) Get(context.Context, ccleft.Source) ccleft.Reading { return f.reading }

func TestNewManager_SelectsCCLeftProbersAndFallsBackForUnknown(t *testing.T) {
	cfg := config.RotationConfig{
		Enabled:        true,
		HeadroomSource: config.RotationHeadroomSourceCCLeft,
		Providers: map[string]config.ProviderRotationConfig{
			"anthropic": {Class: ClassSubscription, Backends: []string{"claude"}},
			"github":    {Class: ClassSubscription, Backends: []string{"copilot"}},
			"unknown":   {Class: ClassMetered, Backends: []string{"other"}},
		},
	}
	m := NewManager(cfg)
	if len(m.probers) != 2 {
		t.Fatalf("len(probers) = %d, want 2", len(m.probers))
	}
	for _, p := range m.probers {
		if _, ok := p.(ccleftHeadroomSource); !ok {
			t.Fatalf("prober %q is %T, want ccleftHeadroomSource", p.Provider(), p)
		}
	}
}

func TestNewManager_DefaultsToBuiltinHeadroomSource(t *testing.T) {
	m := NewManager(config.RotationConfig{
		Providers: map[string]config.ProviderRotationConfig{
			"github": {Class: ClassSubscription, Backends: []string{"copilot"}, MonthlyAllowance: 1500},
		},
	})
	if len(m.probers) != 1 {
		t.Fatalf("len(probers) = %d, want 1", len(m.probers))
	}
	if _, ok := m.probers[0].(CopilotProber); !ok {
		t.Fatalf("prober = %T, want CopilotProber", m.probers[0])
	}
}

func TestCCLeftAdapterMapsReadingToHeadroom(t *testing.T) {
	reset := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	fetched := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	used, remaining := 91.0, 9.0
	r := ccleft.Reading{
		Provider:  ccleft.Claude,
		State:     ccleft.StateOK,
		Plan:      "pro",
		FetchedAt: fetched,
		Windows: []ccleft.Window{{
			ID: "five_hour", Kind: ccleft.KindFiveHour, Binding: true,
			UsedPct: &used, RemainingPct: &remaining, ResetsAt: &reset,
		}},
	}
	h := ccleftReadingToHeadroom("anthropic", 85, r)
	if h.Provider != "anthropic" || h.Available {
		t.Fatalf("headroom = %+v, want anthropic unavailable at 9%% remaining", h)
	}
	if h.PctRemaining != 9 || !h.ResetAt.Equal(reset) || !h.CapturedAt.Equal(fetched) {
		t.Fatalf("headroom timing/percent = %+v", h)
	}
	if h.PlanType != "pro" || len(h.Limits) != 1 || h.Limits[0].Kind != "five_hour" || h.Limits[0].DurationMins != 300 {
		t.Fatalf("limits/plan = %+v", h)
	}
}

func TestCCLeftAdapterMapsErrorsAndStaleCause(t *testing.T) {
	used, remaining := 20.0, 80.0
	r := ccleft.Reading{
		Provider: ccleft.Claude,
		State:    ccleft.StateOK,
		Stale:    true,
		Cause:    "http_429",
		Message:  "serving last-good after HTTP 429",
		Windows:  []ccleft.Window{{ID: "weekly", Kind: ccleft.KindWeekly, Binding: true, UsedPct: &used, RemainingPct: &remaining}},
	}
	h := ccleftReadingToHeadroom("anthropic", 85, r)
	if !h.Available || !h.Stale || h.PctRemaining != 80 {
		t.Fatalf("headroom = %+v, want stale available last-good", h)
	}
	if h.ProbeErr == nil || h.ProbeErrCause != ProbeCauseRateLimited {
		t.Fatalf("ProbeErr/Cause = %v/%q, want rate_limited", h.ProbeErr, h.ProbeErrCause)
	}
}

func TestCCLeftHeadroomSourceUsesFallbackMapping(t *testing.T) {
	src := ccleftHeadroomSource{
		provider:     "deepseek",
		thresholdPct: 85,
		source:       ccleft.Source{Provider: ccleft.DeepSeek},
		client: fakeCCLeftClient{reading: ccleft.Reading{
			Provider: ccleft.DeepSeek,
			State:    ccleft.StateError,
			Cause:    "schema",
			Message:  "balance schema changed",
		}},
	}
	h := src.Probe(context.Background())
	if h.Provider != "deepseek" || !h.Available {
		t.Fatalf("headroom = %+v, want fail-open deepseek", h)
	}
	if h.ProbeErr == nil || h.ProbeErrCause != ProbeCauseUnrecognizedSchema {
		t.Fatalf("ProbeErr/Cause = %v/%q", h.ProbeErr, h.ProbeErrCause)
	}
}

func TestCCLeftCauseMappingNotInstalled(t *testing.T) {
	r := ccleft.Reading{Provider: ccleft.Agy, State: ccleft.StateError, Cause: "not_installed"}
	if got := ccleftProbeCause(r); got != ProbeCauseNotInstalled {
		t.Fatalf("cause = %q, want not_installed", got)
	}
}

func TestCCLeftProbeErrorMessageFallback(t *testing.T) {
	r := ccleft.Reading{Provider: ccleft.Codex, State: ccleft.StateError, Cause: "network"}
	if got := ccleftProbeError(r).Error(); got != "ccleft codex reading state error (network)" {
		t.Fatalf("error = %q", got)
	}
}

func TestCCLeftProviderMappingCoversCurrentProviders(t *testing.T) {
	want := map[string]ccleft.Provider{
		"anthropic": ccleft.Claude,
		"openai":    ccleft.Codex,
		"google":    ccleft.Agy,
		"github":    ccleft.Copilot,
		"deepseek":  ccleft.DeepSeek,
		"aws-kiro":  ccleft.Kiro,
	}
	for hive, provider := range want {
		got, ok := ccleftProviderForHive(hive)
		if !ok || got != provider {
			t.Fatalf("ccleftProviderForHive(%q) = %q, %v; want %q, true", hive, got, ok, provider)
		}
	}
	if _, ok := ccleftProviderForHive("muse"); ok {
		t.Fatal("ccleftProviderForHive(muse) ok = true, want false until Hive has a provider")
	}
}

func TestCCLeftHeadroomSourceProvider(t *testing.T) {
	s := ccleftHeadroomSource{provider: "openai"}
	if got := s.Provider(); got != "openai" {
		t.Fatalf("Provider = %q", got)
	}
}

func TestCCLeftWindowMappingCoversUnitsScopesAndAmounts(t *testing.T) {
	reset := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	used := 101.7
	remaining := -5.2
	creditRemaining := 3.0
	balanceEmpty := 0.0
	limits := ccleftWindowsToLimits([]ccleft.Window{
		{ID: "daily", Kind: ccleft.KindDaily, Binding: true, UsedPct: &used, RemainingPct: &remaining, ResetsAt: &reset},
		{ID: "credits", Kind: ccleft.KindCredits, Binding: false, Remaining: &creditRemaining, Unit: ccleft.UnitCredits, Scope: "overage"},
		{ID: "balance", Kind: ccleft.KindBalance, Binding: true, Remaining: &balanceEmpty, Unit: ccleft.UnitUSD},
	})
	if len(limits) != 3 {
		t.Fatalf("len(limits) = %d", len(limits))
	}
	if limits[0].PercentUsed != 100 || limits[0].PctRemaining != 0 || limits[0].DurationMins != 1440 || !limits[0].ResetAt.Equal(reset) {
		t.Fatalf("daily limit = %+v", limits[0])
	}
	if limits[1].PercentUsed != 0 || limits[1].PctRemaining != 100 || limits[1].Scope["binding"] != "false" || limits[1].Scope["unit"] != ccleft.UnitCredits || limits[1].Scope["scope"] != "overage" {
		t.Fatalf("credits limit = %+v", limits[1])
	}
	if limits[2].PercentUsed != 100 || limits[2].PctRemaining != 0 || limits[2].Scope["unit"] != ccleft.UnitUSD {
		t.Fatalf("balance limit = %+v", limits[2])
	}
}

func TestCCLeftStateOnlyRemaining(t *testing.T) {
	ok := ccleftReadingToHeadroom("aws-kiro", 85, ccleft.Reading{Provider: ccleft.Kiro, State: ccleft.StateOK})
	if !ok.Available || ok.PctRemaining != 100 {
		t.Fatalf("ok state-only headroom = %+v", ok)
	}
	limited := ccleftReadingToHeadroom("aws-kiro", 85, ccleft.Reading{Provider: ccleft.Kiro, State: ccleft.StateLimited})
	if limited.Available || limited.PctRemaining != 0 || limited.ProbeErr != nil {
		t.Fatalf("limited state-only headroom = %+v", limited)
	}
}

func TestCCLeftProbeCauseMapping(t *testing.T) {
	tests := []struct {
		name string
		r    ccleft.Reading
		want ProbeErrorCause
	}{
		{"rate state", ccleft.Reading{State: ccleft.StateRateLimited}, ProbeCauseRateLimited},
		{"auth state", ccleft.Reading{State: ccleft.StateAuthRequired}, ProbeCauseNoCredentials},
		{"unsupported state", ccleft.Reading{State: ccleft.StateUnsupported}, ProbeCauseNoCredentials},
		{"login", ccleft.Reading{State: ccleft.StateError, Cause: "login_required"}, ProbeCauseNoCredentials},
		{"schema substring", ccleft.Reading{State: ccleft.StateError, Cause: "payload_schema_changed"}, ProbeCauseUnrecognizedSchema},
		{"timeout substring", ccleft.Reading{State: ccleft.StateError, Cause: "dial_timeout"}, ProbeCauseTimeout},
		{"429 substring", ccleft.Reading{State: ccleft.StateError, Cause: "http_429_retry"}, ProbeCauseRateLimited},
		{"generic", ccleft.Reading{State: ccleft.StateError, Cause: "network"}, ProbeCauseProbeFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ccleftProbeCause(tt.r); got != tt.want {
				t.Fatalf("cause = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCCLeftAvailabilityDefaultThreshold(t *testing.T) {
	if remainingAvailable(10, true, 0) {
		t.Fatal("remainingAvailable at 90% used with default threshold = true, want false")
	}
	if !remainingAvailable(100, false, 0) {
		t.Fatal("remainingAvailable without a percentage = false, want true")
	}
}

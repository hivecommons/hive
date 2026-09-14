package rotation

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ── kubestellar/hive#6980 ───────────────────────────────────────────────────

// copilotFixture loads the schema-derived premium-request usage payload (see
// testdata/README.md for provenance).
func copilotFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "copilot_premium_request_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// sep2026 is inside the fixture's reported timePeriod (2026-09).
var sep2026 = time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)

func TestCopilotHeadroomFromFixture(t *testing.T) {
	// Fixture: copilot gross = 220 + 80 + 540 = 840, net = 20; the non-copilot
	// actions item (gross 999) must not be counted.
	h := copilotHeadroom("github", 85, 1000, sep2026, copilotFixture(t))
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v, want nil", h.ProbeErr)
	}
	if len(h.Limits) != 1 {
		t.Fatalf("len(Limits) = %d, want 1", len(h.Limits))
	}
	lw := h.Limits[0]
	if lw.Kind != "monthly" || lw.ID != "monthly-premium-requests" {
		t.Errorf("window = %s/%s, want monthly-premium-requests/monthly", lw.ID, lw.Kind)
	}
	if lw.PercentUsed != 84 || lw.PctRemaining != 16 {
		t.Errorf("PercentUsed/PctRemaining = %d/%d, want 84/16 (840 of 1000)", lw.PercentUsed, lw.PctRemaining)
	}
	wantReset := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	if !lw.ResetAt.Equal(wantReset) || !h.ResetAt.Equal(wantReset) {
		t.Errorf("ResetAt = %v/%v, want %v (first of next month)", lw.ResetAt, h.ResetAt, wantReset)
	}
	if want := 30 * 24 * 60; lw.DurationMins != want {
		t.Errorf("DurationMins = %d, want %d (September)", lw.DurationMins, want)
	}
	if !h.Available || h.PctRemaining != 16 {
		t.Errorf("Available/PctRemaining = %v/%d, want true/16 (84%% used < 85%% threshold)", h.Available, h.PctRemaining)
	}
	// netQuantity 20 > 0: billed overage is being consumed — the paid-usage
	// bit is set, from a read-only source.
	if h.PaidCreditsAvailable == nil || !*h.PaidCreditsAvailable {
		t.Errorf("PaidCreditsAvailable = %v, want true (net overage in fixture)", h.PaidCreditsAvailable)
	}
}

func TestCopilotHeadroomExhaustedAndClamped(t *testing.T) {
	// Allowance 800 against 840 gross: over 100% used clamps to 100 and the
	// window binds at the threshold.
	h := copilotHeadroom("github", 85, 800, sep2026, copilotFixture(t))
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v, want nil", h.ProbeErr)
	}
	if h.Available {
		t.Error("Available = true, want false (105% used, clamped to 100)")
	}
	if h.PctRemaining != 0 || h.Limits[0].PercentUsed != 100 {
		t.Errorf("PctRemaining/PercentUsed = %d/%d, want 0/100", h.PctRemaining, h.Limits[0].PercentUsed)
	}
}

func TestCopilotHeadroomUnknownWithoutAllowance(t *testing.T) {
	// The documented endpoint reports consumption only. With no stated
	// allowance the reading is an explicit unknown (ProbeErr set, never
	// exhaustion), with the consumed count surfaced informationally.
	h := copilotHeadroom("github", 85, 0, sep2026, copilotFixture(t))
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want explicit unknown without a stated allowance")
	}
	if !h.Available {
		t.Error("Available = false, want true (unknown is never exhaustion)")
	}
	if len(h.Limits) != 0 {
		t.Errorf("Limits = %v, want none (no percentage can be derived)", h.Limits)
	}
	for _, want := range []string{"840 consumed", "20 billed", "monthly_allowance"} {
		if !strings.Contains(h.ProbeErr.Error(), want) {
			t.Errorf("ProbeErr %q does not mention %q", h.ProbeErr.Error(), want)
		}
	}
	if h.PaidCreditsAvailable == nil || !*h.PaidCreditsAvailable {
		t.Errorf("PaidCreditsAvailable = %v, want true even when remaining is unknown", h.PaidCreditsAvailable)
	}
	if want := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC); !h.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", h.ResetAt, want)
	}
}

func TestCopilotHeadroomRejectsUnrecognizedSchema(t *testing.T) {
	cases := map[string]string{
		"no usageItems key": `{"timePeriod":{"year":2026,"month":9}}`,
		"not json":          `<!DOCTYPE html>`,
		"items not a list":  `{"usageItems":{"gross":1}}`,
	}
	for name, raw := range cases {
		h := copilotHeadroom("github", 85, 1000, sep2026, []byte(raw))
		if h.ProbeErr == nil {
			t.Errorf("%s: ProbeErr = nil, want schema rejection", name)
		}
		if !h.Available {
			t.Errorf("%s: Available = false, want true (failed measurement is not exhaustion)", name)
		}
	}
	// An EMPTY usageItems list is a valid month with no usage, not drift.
	h := copilotHeadroom("github", 85, 1000, sep2026, []byte(`{"usageItems":[]}`))
	if h.ProbeErr != nil {
		t.Errorf("empty usageItems: ProbeErr = %v, want nil", h.ProbeErr)
	}
	if !h.Available || h.PctRemaining != 100 {
		t.Errorf("empty usageItems: Available/PctRemaining = %v/%d, want true/100", h.Available, h.PctRemaining)
	}
	if h.PaidCreditsAvailable != nil {
		t.Errorf("empty usageItems: PaidCreditsAvailable = %v, want nil (provider did not say)", *h.PaidCreditsAvailable)
	}
}

func TestCopilotHeadroomMonthFallsBackToClock(t *testing.T) {
	feb := time.Date(2027, time.February, 3, 0, 0, 0, 0, time.UTC)
	h := copilotHeadroom("github", 85, 100, feb, []byte(`{"usageItems":[]}`))
	if want := time.Date(2027, time.March, 1, 0, 0, 0, 0, time.UTC); !h.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v (clock month when timePeriod is absent)", h.ResetAt, want)
	}
}

// copilotFakeGH returns a Run seam that answers `gh api user` with a login and
// the usage path with the fixture, recording every invocation.
func copilotFakeGH(t *testing.T, calls *[][]string, usage string, usageErr error) func(context.Context, string, ...string) (string, error) {
	t.Helper()
	return func(_ context.Context, name string, args ...string) (string, error) {
		*calls = append(*calls, append([]string{name}, args...))
		if name != "gh" || len(args) < 2 || args[0] != "api" {
			return "", fmt.Errorf("unexpected invocation: %s %v", name, args)
		}
		if args[1] == "user" {
			return "octocat\n", nil
		}
		if usageErr != nil {
			return "", usageErr
		}
		return usage, nil
	}
}

func TestCopilotProber_ProbesDocumentedEndpoint(t *testing.T) {
	var calls [][]string
	p := CopilotProber{
		ThresholdPct:     85,
		MonthlyAllowance: 1000,
		Run:              copilotFakeGH(t, &calls, string(copilotFixture(t)), nil),
		Now:              func() time.Time { return sep2026 },
	}
	h := p.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if h.Provider != "github" || h.PctRemaining != 16 {
		t.Errorf("Provider/PctRemaining = %s/%d, want github/16", h.Provider, h.PctRemaining)
	}
	if len(calls) != 2 {
		t.Fatalf("gh invoked %d times, want 2 (login, usage)", len(calls))
	}
	if want := "/users/octocat/settings/billing/premium_request/usage"; calls[1][2] != want {
		t.Errorf("usage path = %q, want %q", calls[1][2], want)
	}
}

func TestCopilotProber_NeedsLoginAndScopeErrors(t *testing.T) {
	// Logged-out gh: the login resolution fails and the probe says so.
	p := CopilotProber{Run: func(_ context.Context, _ string, args ...string) (string, error) {
		return "", errors.New("gh: To get started with GitHub CLI, please run: gh auth login")
	}}
	h := p.Probe(context.Background())
	if h.ProbeErr == nil || !strings.Contains(h.ProbeErr.Error(), "gh auth login") {
		t.Errorf("ProbeErr = %v, want explicit needs-login", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true (needs-login is unknown, not exhaustion)")
	}

	// Token without the Plan scope: HTTP 403 on the usage read, reported.
	var calls [][]string
	p = CopilotProber{Run: copilotFakeGH(t, &calls, "", errors.New("HTTP 403: Resource not accessible"))}
	h = p.Probe(context.Background())
	if h.ProbeErr == nil || !strings.Contains(h.ProbeErr.Error(), "403") {
		t.Errorf("ProbeErr = %v, want the 403 surfaced", h.ProbeErr)
	}
	if !h.Available {
		t.Error("Available = false, want true")
	}
}

func TestCopilotProber_MissingGHBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	h := CopilotProber{}.Probe(context.Background())
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want failure with no gh on PATH")
	}
	if !h.Available {
		t.Error("Available = false, want true (fail-open)")
	}
}

func TestNewManager_WiresCopilotProber(t *testing.T) {
	m := NewManager(config.RotationConfig{
		Enabled: true,
		Providers: map[string]config.ProviderRotationConfig{
			"github": {Class: ClassSubscription, Backends: []string{"copilot"}, MonthlyAllowance: 1500},
		},
	})
	if len(m.probers) != 1 {
		t.Fatalf("len(probers) = %d, want 1", len(m.probers))
	}
	cp, ok := m.probers[0].(CopilotProber)
	if !ok {
		t.Fatalf("prober = %T, want CopilotProber", m.probers[0])
	}
	if cp.MonthlyAllowance != 1500 {
		t.Errorf("MonthlyAllowance = %d, want 1500 (from provider config)", cp.MonthlyAllowance)
	}
	if cp.ThresholdPct != 85 {
		t.Errorf("ThresholdPct = %d, want default 85", cp.ThresholdPct)
	}
}

// TestNoCopilotBudgetMutation pins #6980's acceptance criterion (inherited
// from #6833): the paid-overage signal is observed, never enabled — no code
// path may call any budget-mutating endpoint
// (POST/PATCH/DELETE …/settings/billing/budgets…). Two layers:
//
//  1. Source pin, in the style of TestNoCodexSpendOrBillingMutation: nothing
//     in the tree references the budgets surface at all.
//  2. Runtime pin: every gh invocation a probe makes is a plain `gh api`
//     read — no --method/-X override, no --input, no budgets path.
func TestNoCopilotBudgetMutation(t *testing.T) {
	root := filepath.Join("..", "..")
	banned := []string{"billing/budgets", "settings/billing/budget"}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, s := range banned {
			if strings.Contains(string(b), s) {
				t.Errorf("%s references %q — no code path may mutate a Copilot budget", path, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	p := CopilotProber{
		ThresholdPct:     85,
		MonthlyAllowance: 1000,
		Run:              copilotFakeGH(t, &calls, string(copilotFixture(t)), nil),
	}
	_ = p.Probe(context.Background())
	for _, call := range calls {
		if call[1] != "api" {
			t.Errorf("gh invoked as %v — only `gh api` reads are allowed", call)
		}
		for _, arg := range call {
			lower := strings.ToLower(arg)
			if arg == "-X" || arg == "--method" || arg == "--input" || strings.Contains(lower, "budget") {
				t.Errorf("gh invocation %v carries %q — probes must be plain GET reads", call, arg)
			}
		}
	}
}

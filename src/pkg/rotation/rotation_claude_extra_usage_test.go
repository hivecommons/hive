package rotation

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claudeLimitsJSON is one plan window, so each case below parses as a valid
// reading and only its extra-usage part varies.
const claudeLimitsJSON = `"limits":[{"kind":"weekly_all","percent":46,"resets_at":"2026-11-23T07:00:00Z","scope":null}]`

func claudeExtraUsageReading(t *testing.T, extra string) Headroom {
	t.Helper()
	body := "{" + claudeLimitsJSON
	if extra != "" {
		body += "," + extra
	}
	h, err := claudeHeadroom("anthropic", 80, []byte(body+"}"))
	if err != nil {
		t.Fatalf("claudeHeadroom: %v", err)
	}
	if len(h.Limits) != 1 {
		t.Fatalf("extra usage must not disturb the plan windows: Limits=%+v", h.Limits)
	}
	return h
}

// The fixture is shaped like a live /api/oauth/usage response (2026-11-19):
// extra usage on, $0.00 of a $350.00 monthly cap spent, no balance.
func TestClaudeExtraUsageFromLiveShapedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "claude_oauth_usage_extra_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := claudeUsageServer(t, http.StatusOK, string(raw))
	h := ClaudeProber{ThresholdPct: 80, BaseURL: srv.URL, CredentialsPath: claudeCredsFile(t, "test-token")}.Probe(context.Background())
	if h.ProbeErr != nil {
		t.Fatalf("ProbeErr = %v", h.ProbeErr)
	}
	if len(h.Limits) != 2 || h.PctRemaining != 54 {
		t.Fatalf("plan windows changed: PctRemaining=%d Limits=%+v", h.PctRemaining, h.Limits)
	}
	eu := h.ExtraUsage
	if eu == nil {
		t.Fatal("ExtraUsage = nil, want the spend reading")
	}
	if eu.Enabled == nil || !*eu.Enabled {
		t.Errorf("Enabled = %v, want true", eu.Enabled)
	}
	if eu.UsedMinor == nil || *eu.UsedMinor != 0 {
		t.Errorf("UsedMinor = %v, want 0 (stated, not unknown)", eu.UsedMinor)
	}
	if eu.LimitMinor == nil || *eu.LimitMinor != 35000 {
		t.Errorf("LimitMinor = %v, want 35000", eu.LimitMinor)
	}
	if eu.Exponent != 2 || eu.Currency != "USD" {
		t.Errorf("Exponent/Currency = %d/%q, want 2/USD", eu.Exponent, eu.Currency)
	}
	if eu.SpendLimitReached == nil || *eu.SpendLimitReached {
		t.Errorf("SpendLimitReached = %v, want false", eu.SpendLimitReached)
	}
	if h.PaidCreditsAvailable == nil || !*h.PaidCreditsAvailable {
		t.Errorf("PaidCreditsAvailable = %v, want true from the enabled flag", h.PaidCreditsAvailable)
	}
	out, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	want := `"extra_usage":{"enabled":true,"used_minor":0,"limit_minor":35000,"exponent":2,"currency":"USD","spend_limit_reached":false}`
	if !strings.Contains(string(out), want) {
		t.Errorf("headroom JSON missing %s:\n%s", want, out)
	}
}

func TestClaudeExtraUsageDisabled(t *testing.T) {
	h := claudeExtraUsageReading(t, `"spend":{"enabled":false,"used":null,"limit":null},"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null}`)
	if h.ExtraUsage == nil || h.ExtraUsage.Enabled == nil || *h.ExtraUsage.Enabled {
		t.Fatalf("ExtraUsage = %+v, want enabled=false", h.ExtraUsage)
	}
	if h.ExtraUsage.UsedMinor != nil || h.ExtraUsage.LimitMinor != nil {
		t.Errorf("null amounts must stay nil, got used=%v limit=%v", h.ExtraUsage.UsedMinor, h.ExtraUsage.LimitMinor)
	}
	if h.PaidCreditsAvailable == nil || *h.PaidCreditsAvailable {
		t.Errorf("PaidCreditsAvailable = %v, want false", h.PaidCreditsAvailable)
	}
}

func TestClaudeExtraUsageLimitReached(t *testing.T) {
	h := claudeExtraUsageReading(t, `"spend":{"enabled":true,"used":{"amount_minor":35000,"currency":"USD","exponent":2},"limit":{"amount_minor":35000,"currency":"USD","exponent":2}},"extra_usage":{"is_enabled":true,"spend_limit_reached":true}`)
	eu := h.ExtraUsage
	if eu == nil || eu.SpendLimitReached == nil || !*eu.SpendLimitReached {
		t.Fatalf("ExtraUsage = %+v, want spend_limit_reached=true", eu)
	}
	if *eu.UsedMinor != 35000 || *eu.LimitMinor != 35000 {
		t.Errorf("used/limit = %d/%d, want 35000/35000", *eu.UsedMinor, *eu.LimitMinor)
	}
}

// A response without spend or extra_usage (the older schema, and the
// original fixture) leaves every extra-usage field unknown: nil, never 0/off.
func TestClaudeExtraUsageAbsentStaysNil(t *testing.T) {
	for name, extra := range map[string]string{
		"absent":      "",
		"null":        `"spend":null,"extra_usage":null`,
		"empty":       `"spend":{},"extra_usage":{}`,
		"all nulls":   `"spend":{"enabled":null,"used":null,"limit":null},"extra_usage":{"is_enabled":null,"monthly_limit":null,"used_credits":null,"spend_limit_reached":null}`,
		"wrong types": `"spend":"on","extra_usage":["x"]`,
	} {
		t.Run(name, func(t *testing.T) {
			h := claudeExtraUsageReading(t, extra)
			if h.ExtraUsage != nil {
				t.Errorf("ExtraUsage = %+v, want nil", h.ExtraUsage)
			}
			if h.PaidCreditsAvailable != nil {
				t.Errorf("PaidCreditsAvailable = %v, want nil", *h.PaidCreditsAvailable)
			}
			out, _ := json.Marshal(h)
			if strings.Contains(string(out), "extra_usage") {
				t.Errorf("headroom JSON names extra_usage with nothing reported:\n%s", out)
			}
		})
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "claude_oauth_usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	h, err := claudeHeadroom("anthropic", 80, raw)
	if err != nil || h.ExtraUsage != nil {
		t.Fatalf("old-schema fixture: err=%v ExtraUsage=%+v, want nil", err, h.ExtraUsage)
	}
}

// Without spend, the amounts come from extra_usage (cents, decimal_places).
// A malformed spend object is ignored rather than failing the reading.
func TestClaudeExtraUsageFallsBackToExtraUsageObject(t *testing.T) {
	for name, spend := range map[string]string{
		"no spend":        "",
		"malformed spend": `"spend":{"used":"lots"},`,
	} {
		t.Run(name, func(t *testing.T) {
			h := claudeExtraUsageReading(t, spend+`"extra_usage":{"is_enabled":true,"monthly_limit":35000,"used_credits":1234.0,"currency":"USD","decimal_places":2,"spend_limit_reached":false}`)
			eu := h.ExtraUsage
			if eu == nil || eu.Enabled == nil || !*eu.Enabled {
				t.Fatalf("ExtraUsage = %+v, want enabled", eu)
			}
			if eu.UsedMinor == nil || *eu.UsedMinor != 1234 || eu.LimitMinor == nil || *eu.LimitMinor != 35000 || eu.Exponent != 2 || eu.Currency != "USD" {
				t.Errorf("ExtraUsage = %+v, want 1234 of 35000 cents USD", eu)
			}
		})
	}
}

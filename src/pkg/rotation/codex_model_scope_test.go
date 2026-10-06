package rotation

import (
	"context"
	"encoding/json"
	"testing"
)

// Tests for scoping Codex rate-limit buckets to the selected model
// (hivecommons/hive#10865). The fixture mirrors the issue's
// account/rateLimits/read reply: a shared "codex" bucket with weekly allowance
// left, plus a bucket scoped by normalModelSlug to another model.

const codexScopeFixture = `{
  "ordinaryUsageAllowed": true,
  "rateLimitsByLimitId": {
    "codex": {
      "normalModelSlug": null,
      "primary": {"usedPercent": 10, "windowDurationMins": 300},
      "secondary": {"usedPercent": 50, "windowDurationMins": 10080}
    },
    "base_model_inference": {
      "normalModelSlug": "gpt-5.6-luna",
      "primary": {"usedPercent": 100, "windowDurationMins": 10080}
    }
  }
}`

func codexScopeLimitIDs(h Headroom) map[string]LimitWindow {
	out := map[string]LimitWindow{}
	for _, w := range h.Limits {
		out[w.ID] = w
	}
	return out
}

func TestCodexLimitAppliesToModel(t *testing.T) {
	cases := []struct {
		scope, selected string
		want            bool
	}{
		{"", "", true},
		{"gpt-5.6-luna", "", true},
		{"", "gpt-6-astra", true},
		{"  ", "gpt-6-astra", true},
		{"gpt-6-astra", "gpt-6-astra", true},
		{"gpt-5.6-luna", "openai/GPT-5-6-luna", true},
		{"gpt_5.6_luna", "gpt-5.6-luna", true},
		{"gpt-5.6-luna", "gpt-6-astra", false},
	}
	for _, tc := range cases {
		if got := codexLimitAppliesToModel(tc.scope, tc.selected); got != tc.want {
			t.Errorf("codexLimitAppliesToModel(%q, %q) = %v, want %v", tc.scope, tc.selected, got, tc.want)
		}
	}
}

func TestCodexHeadroomForModel_ExcludesOtherModelBucket(t *testing.T) {
	h, err := codexHeadroomForModel("openai", 80, json.RawMessage(codexScopeFixture), "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	ids := codexScopeLimitIDs(h)
	if _, ok := ids["base_model_inference"]; ok {
		t.Fatalf("limits = %+v, want the gpt-5.6-luna bucket excluded", h.Limits)
	}
	if _, ok := ids["codex"]; !ok {
		t.Fatalf("limits = %+v, want the shared codex short window kept", h.Limits)
	}
	if w, ok := ids["codex:secondary"]; !ok || w.Kind != "weekly" || w.PctRemaining != 50 {
		t.Fatalf("limits = %+v, want the shared weekly window kept", h.Limits)
	}
	if !h.Available || h.PctRemaining != 50 {
		t.Fatalf("headroom = %+v, want the shared weekly window binding and work admitted", h)
	}
	if codexWeeklyExhaustedForModel(h, "gpt-6-astra") {
		t.Fatal("another model's exhausted weekly window must not read as exhaustion")
	}
}

func TestCodexHeadroomForModel_NoModelKeepsEveryBucket(t *testing.T) {
	for _, h := range []func() (Headroom, error){
		func() (Headroom, error) { return codexHeadroom("openai", 80, json.RawMessage(codexScopeFixture)) },
		func() (Headroom, error) {
			return codexHeadroomForModel("openai", 80, json.RawMessage(codexScopeFixture), "  ")
		},
	} {
		got, err := h()
		if err != nil {
			t.Fatal(err)
		}
		w, ok := codexScopeLimitIDs(got)["base_model_inference"]
		if !ok || w.Scope[codexScopeModelKey] != "gpt-5.6-luna" {
			t.Fatalf("limits = %+v, want the scoped bucket kept and labelled with its model", got.Limits)
		}
		if got.Available || !codexWeeklyExhausted(got) {
			t.Fatalf("headroom = %+v, want unchanged behaviour with no selected model", got)
		}
	}
}

func TestCodexHeadroomForModel_MatchingModelBucketApplies(t *testing.T) {
	h, err := codexHeadroomForModel("openai", 80, json.RawMessage(codexScopeFixture), "openai/gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	w, ok := codexScopeLimitIDs(h)["base_model_inference"]
	if !ok || w.Scope[codexScopeModelKey] != "gpt-5.6-luna" {
		t.Fatalf("limits = %+v, want the matching model bucket kept", h.Limits)
	}
	if h.Available || !codexWeeklyExhaustedForModel(h, "gpt-5.6-luna") {
		t.Fatalf("headroom = %+v, want the selected model's exhausted weekly window to hold", h)
	}
}

// Absent or empty normalModelSlug is a shared bucket, never another model's.
func TestCodexHeadroomForModel_MissingScopeIsShared(t *testing.T) {
	payload := `{"rateLimitsByLimitId": {
	  "absent": {"primary": {"usedPercent": 100, "windowDurationMins": 10080}},
	  "empty": {"normalModelSlug": "", "primary": {"usedPercent": 30, "windowDurationMins": 300}}
	}}`
	h, err := codexHeadroomForModel("openai", 80, json.RawMessage(payload), "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	ids := codexScopeLimitIDs(h)
	if _, ok := ids["absent"]; !ok {
		t.Fatalf("limits = %+v, want the bucket without normalModelSlug kept", h.Limits)
	}
	if _, ok := ids["empty"]; !ok {
		t.Fatalf("limits = %+v, want the bucket with an empty normalModelSlug kept", h.Limits)
	}
	if ids["absent"].Scope[codexScopeModelKey] != "" {
		t.Fatal("a shared window must not carry a model scope")
	}
	if h.Available || !codexWeeklyExhaustedForModel(h, "gpt-6-astra") {
		t.Fatalf("headroom = %+v, want a shared exhausted weekly window to hold", h)
	}
}

func TestCodexHeadroomForModel_ScopedTopLevelSnapshot(t *testing.T) {
	payload := `{"rateLimits": {
	  "normalModelSlug": "gpt-5.6-luna",
	  "primary": {"usedPercent": 100, "windowDurationMins": 10080},
	  "secondary": {"usedPercent": 100, "windowDurationMins": 300},
	  "rateLimitsByLimitId": {"nested": {"usedPercent": 100, "windowDurationMins": 10080}}
	}, "rateLimitsByLimitId": {
	  "codex": {"primary": {"usedPercent": 20, "windowDurationMins": 10080}}
	}}`
	h, err := codexHeadroomForModel("openai", 80, json.RawMessage(payload), "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Limits) != 1 || h.Limits[0].ID != "codex" || !h.Available {
		t.Fatalf("headroom = %+v, want only the shared codex bucket", h)
	}
	h, err = codexHeadroomForModel("openai", 80, json.RawMessage(payload), "gpt-5.6-luna")
	if err != nil {
		t.Fatal(err)
	}
	if ids := codexScopeLimitIDs(h); len(ids) != 4 || ids["primary"].Scope[codexScopeModelKey] != "gpt-5.6-luna" || h.Available {
		t.Fatalf("headroom = %+v, want the matching top-level windows kept", h)
	}
}

// Provider-wide refusal survives scoping.
func TestCodexHeadroomForModel_KeepsOrdinaryUsageRefusal(t *testing.T) {
	payload := `{"ordinaryUsageAllowed": false, "rateLimitResetCredits": {"availableCount": 2}, "rateLimitsByLimitId": {
	  "codex": {"normalModelSlug": null, "primary": {"usedPercent": 0, "windowDurationMins": 10080}},
	  "other": {"normalModelSlug": "gpt-5.6-luna", "primary": {"usedPercent": 100, "windowDurationMins": 10080}}
	}}`
	h, err := codexHeadroomForModel("openai", 80, json.RawMessage(payload), "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	if h.Available || h.OrdinaryUsageAllowed == nil || *h.OrdinaryUsageAllowed {
		t.Fatalf("headroom = %+v, want ordinaryUsageAllowed=false to still refuse work", h)
	}
	if h.ResetCreditsAvailable == nil || *h.ResetCreditsAvailable != 2 {
		t.Fatalf("reset credits = %v, want the account-wide count kept", h.ResetCreditsAvailable)
	}
}

// When every bucket belongs to another model there is no reading for the
// selected one: that is an error (published as unknown), never headroom.
func TestCodexHeadroomForModel_NoApplicableWindowIsUnknown(t *testing.T) {
	payload := `{"rateLimitsByLimitId": {
	  "other": {"normalModelSlug": "gpt-5.6-luna", "primary": {"usedPercent": 0, "windowDurationMins": 10080}}
	}}`
	if _, err := codexHeadroomForModel("openai", 80, json.RawMessage(payload), "gpt-6-astra"); err == nil {
		t.Fatal("want an error when no window applies to the selected model")
	}
}

func codexScopeReading(sharedWeekly, otherWeekly int) Headroom {
	return Headroom{
		Provider: "openai",
		Limits: []LimitWindow{
			{ID: "codex:secondary", Kind: "weekly", PctRemaining: sharedWeekly},
			{ID: "base_model_inference", Kind: "weekly", PctRemaining: otherWeekly, Scope: map[string]string{codexScopeModelKey: "gpt-5.6-luna"}},
		},
		ResetCreditsAvailable: redeemIntp(1),
	}
}

func TestCodexResetRedeem_UnrelatedModelExhaustionDoesNotConsume(t *testing.T) {
	fake := &redeemFake{reply: redeemReply(`"reset"`)}
	r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
	r.model = "gpt-6-astra"
	h := codexScopeReading(50, 0)
	if codexRedeemTrigger(h, r.model) {
		t.Fatal("trigger must ignore another model's exhausted weekly window")
	}
	rereads := 0
	r.evaluate(context.Background(), h, func(context.Context) Headroom { rereads++; return h })
	if len(fake.calls()) != 0 || rereads != 0 {
		t.Fatalf("redemptions=%d rereads=%d, want none for an unrelated model", len(fake.calls()), rereads)
	}
}

func TestCodexResetRedeem_ApplicableExhaustionStillConsumes(t *testing.T) {
	cases := []struct {
		name  string
		model string
		h     Headroom
	}{
		{"shared weekly exhausted", "gpt-6-astra", codexScopeReading(0, 50)},
		{"selected model weekly exhausted", "gpt-5.6-luna", codexScopeReading(50, 0)},
		{"no selected model", "", codexScopeReading(50, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &redeemFake{reply: redeemReply(`"reset"`)}
			r := newTestRedeemer(t.TempDir(), fake, newRedeemClock())
			r.model = tc.model
			if !codexRedeemTrigger(tc.h, r.model) {
				t.Fatal("an applicable exhausted weekly window must trigger")
			}
			r.evaluate(context.Background(), tc.h, redeemSeq(tc.h, codexScopeReading(100, 100)))
			if len(fake.calls()) != 1 {
				t.Fatalf("redemptions = %d, want 1", len(fake.calls()))
			}
		})
	}
}

func TestManagerSetCodexModel_ScopesProberAndRedeemer(t *testing.T) {
	m, ok := NewContributorBackendReadingPublisher(t.TempDir(), "", "codex")
	if !ok {
		t.Fatal("codex must get a publisher")
	}
	m.SetCodexModel(" gpt-6-astra ")
	m.EnableCodexResetRedeem()
	if m.codexResetRedeem == nil || m.codexResetRedeem.model != "gpt-6-astra" {
		t.Fatal("the redeemer must inherit the selected model")
	}
	found := false
	for _, p := range m.probers {
		if cp, ok := p.(CodexProber); ok {
			found = true
			if cp.Model != "gpt-6-astra" {
				t.Fatalf("CodexProber.Model = %q, want gpt-6-astra", cp.Model)
			}
		}
	}
	if !found {
		t.Fatal("the codex publisher must probe with a CodexProber")
	}
	m.SetCodexModel("gpt-5.6-luna")
	if m.codexResetRedeem.model != "gpt-5.6-luna" {
		t.Fatal("re-selecting the model must rescope a running redeemer")
	}
}

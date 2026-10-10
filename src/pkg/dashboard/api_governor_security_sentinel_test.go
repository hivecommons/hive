package dashboard

import (
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestGovernorSecuritySentinelDefaultsInResponse(t *testing.T) {
	s, _ := apiServer(t)
	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sec := body["security"].(map[string]any)
	sn, ok := sec["sentinel"].(map[string]any)
	if !ok {
		t.Fatalf("sentinel block missing: %#v", sec["sentinel"])
	}
	if sn["enabled"] != true || sn["label"] != "sentinel-alert" || sn["sensitivePathsDefault"] != true || sn["trustedAuthorsBlock"] != false {
		t.Fatalf("sentinel defaults wrong: %#v", sn)
	}
	if got := len(sn["sensitivePaths"].([]any)); got != len(config.DefaultSentinelSensitivePaths()) {
		t.Fatalf("sensitivePaths len = %d, want %d", got, len(config.DefaultSentinelSensitivePaths()))
	}
	behaviors := sn["behaviors"].([]any)
	if len(behaviors) != len(config.SentinelBehaviors()) {
		t.Fatalf("behaviors len = %d, want %d", len(behaviors), len(config.SentinelBehaviors()))
	}
	for _, b := range behaviors {
		m := b.(map[string]any)
		if m["enabled"] != true || m["description"] == "" {
			t.Fatalf("behavior not enabled/described by default: %#v", m)
		}
	}
}

func TestGovernorSecuritySentinelPartialUpdatePreservesOtherFields(t *testing.T) {
	s, deps := apiServer(t)

	rec := doPut(s, "/api/config/governor/security", map[string]any{
		"sentinel": map[string]any{
			"sensitivePaths":      []string{"OWNERS", "deploy/**"},
			"exemptLogins":        []string{" clubanderson ", ""},
			"label":               "needs-security-eyes",
			"maxActions":          5,
			"trustedAuthorsBlock": true,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	sc := deps.Config.Sentinel
	if len(sc.SensitivePaths) != 2 || len(sc.ExemptLogins) != 1 || sc.ExemptLogins[0] != "clubanderson" ||
		sc.Label != "needs-security-eyes" || sc.MaxActions != 5 || !sc.TrustedAuthorsBlock {
		t.Fatalf("sentinel not persisted: %+v", sc)
	}

	// Second save only toggles a behavior off; the path list must survive.
	off := false
	rec = doPut(s, "/api/config/governor/security", map[string]any{
		"sentinel": map[string]any{"disabledBehaviors": []string{"test_removal"}, "enabled": off},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	sc = deps.Config.Sentinel
	if len(sc.SensitivePaths) != 2 || sc.Label != "needs-security-eyes" || sc.IsEnabled() || !sc.TrustedAuthorsBlock ||
		len(sc.DisabledBehaviors) != 1 {
		t.Fatalf("partial update clobbered fields: %+v", sc)
	}

	// Empty list restores defaults.
	rec = doPut(s, "/api/config/governor/security", map[string]any{
		"sentinel": map[string]any{"sensitivePaths": []string{}},
	})
	if rec.Code != http.StatusOK || deps.Config.Sentinel.SensitivePaths != nil {
		t.Fatalf("empty sensitivePaths should restore defaults: code=%d paths=%v", rec.Code, deps.Config.Sentinel.SensitivePaths)
	}
	rec = doPut(s, "/api/config/governor/security", map[string]any{
		"sentinel": map[string]any{"sensitivePaths": []string{"OWNERS", "deploy/**"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}

	body := decodeJSON(t, doGet(s, "/api/config/governor"))
	sn := body["security"].(map[string]any)["sentinel"].(map[string]any)
	if sn["enabled"] != false || sn["sensitivePathsDefault"] != false || sn["maxActions"].(float64) != 5 || sn["trustedAuthorsBlock"] != true {
		t.Fatalf("response wrong after update: %#v", sn)
	}
	for _, b := range sn["behaviors"].([]any) {
		m := b.(map[string]any)
		if m["name"] == "test_removal" && m["enabled"] != false {
			t.Fatalf("test_removal should read disabled: %#v", m)
		}
	}
}

func TestGovernorSecuritySentinelRejectsBadInput(t *testing.T) {
	s, deps := apiServer(t)
	for name, sn := range map[string]map[string]any{
		"unknown behavior": {"disabledBehaviors": []string{"nope"}},
		"html label":       {"label": "<b>x</b>"},
	} {
		rec := doPut(s, "/api/config/governor/security", map[string]any{"sentinel": sn})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: PUT = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	if deps.Config.Sentinel.Label != "" || deps.Config.Sentinel.DisabledBehaviors != nil {
		t.Fatalf("rejected input leaked into config: %+v", deps.Config.Sentinel)
	}
}

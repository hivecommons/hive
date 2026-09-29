package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSecuritySectionSurfacesResolvedProxyInjection (#9586): the Security tab
// carries the injection state and its source. Injection is opt-in, so an
// unset HIVE_PROXY_INJECT_GH_AUTH reports off with the opt-in hint.
func TestSecuritySectionSurfacesResolvedProxyInjection(t *testing.T) {
	cases := []struct {
		value      string
		wantOn     bool
		wantSource config.ProxyInjectGHAuthSource
		wantReason string
	}{
		{value: config.ProxyInjectGHAuthOnValue, wantOn: true, wantSource: config.ProxyInjectGHAuthSourceExplicitOn},
		{value: config.ProxyInjectGHAuthOffValue, wantSource: config.ProxyInjectGHAuthSourceExplicitOff},
		{value: "", wantSource: config.ProxyInjectGHAuthSourceDefaultOff, wantReason: "opt-in: set " + config.ProxyInjectGHAuthEnv + "=" + config.ProxyInjectGHAuthOnValue},
	}
	for _, tc := range cases {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(config.ProxyInjectGHAuthEnv, tc.value)
			s, _ := apiServer(t)
			body := decodeJSON(t, doGet(s, "/api/config/governor"))
			sec, ok := body["security"].(map[string]any)
			if !ok {
				t.Fatalf("security section missing: %#v", body["security"])
			}
			inj, ok := sec["credentialInjection"].(map[string]any)
			if !ok {
				t.Fatalf("credentialInjection missing or not an object: %#v", sec["credentialInjection"])
			}
			if got, _ := inj["enabled"].(bool); got != tc.wantOn {
				t.Errorf("credentialInjection.enabled = %v, want %v", inj["enabled"], tc.wantOn)
			}
			if got, _ := inj["source"].(string); got != string(tc.wantSource) {
				t.Errorf("credentialInjection.source = %v, want %q", inj["source"], tc.wantSource)
			}
			got, _ := inj["reason"].(string)
			if got == "" {
				t.Error("credentialInjection.reason is empty")
			}
			if tc.wantReason != "" && got != tc.wantReason {
				t.Errorf("credentialInjection.reason = %q, want %q", got, tc.wantReason)
			}
		})
	}
}

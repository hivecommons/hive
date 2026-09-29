package dashboard

import (
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSecuritySectionSurfacesResolvedProxyInjection (#9586): the Security tab
// carries the RESOLVED injection state and its source, so an operator can see
// what an unset HIVE_PROXY_INJECT_GH_AUTH meant on this spoke. With no boot
// decision recorded (this test binary) it falls back to the explicit env.
func TestSecuritySectionSurfacesResolvedProxyInjection(t *testing.T) {
	cases := []struct {
		value      string
		wantOn     bool
		wantSource config.ProxyInjectGHAuthSource
	}{
		{value: config.ProxyInjectGHAuthOnValue, wantOn: true, wantSource: config.ProxyInjectGHAuthSourceExplicitOn},
		{value: config.ProxyInjectGHAuthOffValue, wantSource: config.ProxyInjectGHAuthSourceExplicitOff},
		{value: "", wantSource: config.ProxyInjectGHAuthSourceDefaultOff},
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
			if got, _ := inj["reason"].(string); got == "" {
				t.Error("credentialInjection.reason is empty")
			}
		})
	}
}

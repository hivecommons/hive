package proxy

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

// testCopilotUserToken stands in for the user's Copilot OAuth token
// (COPILOT_GITHUB_TOKEN) the Copilot CLI sends to its auth-exchange endpoints.
const testCopilotUserToken = "gho_copilot_user_oauth_for_test"

// testCopilotGHEHost is a registered GitHub Enterprise host for the /api/v3
// variant of the exchange.
const testCopilotGHEHost = "ghe.copilot-exchange-test.example.com"

// TestInjectAuth_CopilotAuthExchangeKeepsAgentCredential (#9586): with
// injection on, the Copilot CLI's session-token exchange must reach GitHub
// with the agent's own Copilot OAuth token. Rewriting it to the App
// installation token (or stripping it) cuts every copilot-backend agent off
// from its model. The hub-held token source must not even be consulted.
func TestInjectAuth_CopilotAuthExchangeKeepsAgentCredential(t *testing.T) {
	cases := []struct {
		name string
		path string
		host string
	}{
		{name: "session token exchange", path: "/copilot_internal/v2/token", host: "api.github.com"},
		{name: "copilot user discovery", path: "/copilot_internal/user", host: "api.github.com"},
		{name: "GHE api/v3 prefix", path: "/api/v3/copilot_internal/v2/token", host: testCopilotGHEHost},
	}
	RegisterGitHubHost(testCopilotGHEHost)
	t.Cleanup(func() { unregisterGitHubHost(testCopilotGHEHost) })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			p := injectionTestProxy(&calls)
			c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
				"GET "+tc.path+" HTTP/1.1\r\nHost: "+tc.host+"\r\nAuthorization: token "+testCopilotUserToken+"\r\n\r\n")

			if got, want := c.req.Header.Get("Authorization"), "token "+testCopilotUserToken; got != want {
				t.Errorf("Copilot exchange Authorization = %q, want the agent's own %q", got, want)
			}
			if strings.Contains(c.raw, testScopedToken) {
				t.Errorf("hub-held App token attached to a Copilot exchange:\n%s", c.raw)
			}
			if len(calls) != 0 {
				t.Errorf("token source consulted for a Copilot exchange (%v)", calls)
			}
		})
	}
}

// TestInjectAuth_CopilotExemptionIsPathScoped: the exemption must not leak
// onto ordinary REST paths - a REST call from the same agent still gets the
// injected token, and a lookalike path that merely CONTAINS the segment is not
// exempt.
func TestInjectAuth_CopilotExemptionIsPathScoped(t *testing.T) {
	for _, path := range []string{"/repos/org/repo", "/repos/org/copilot_internal/issues", "/user"} {
		t.Run(path, func(t *testing.T) {
			p := injectionTestProxy(nil)
			c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
				"GET "+path+" HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token "+testCopilotUserToken+"\r\n\r\n")

			if got, want := c.req.Header.Get("Authorization"), "token "+testScopedToken; got != want {
				t.Errorf("REST Authorization = %q, want injected %q", got, want)
			}
			if strings.Contains(c.raw, testCopilotUserToken) {
				t.Errorf("agent credential reached upstream on a non-exempt path:\n%s", c.raw)
			}
		})
	}
}

// TestInjectAuth_CopilotExemptionFlagOff: with injection off nothing changes on
// any path, the exchange included.
func TestInjectAuth_CopilotExemptionFlagOff(t *testing.T) {
	p := injectionTestProxy(nil)
	p.injectGHAuth = false
	c := runInjectionExchange(t, p, testAgentName, agent.ModeAdvisory,
		"GET /copilot_internal/v2/token HTTP/1.1\r\nHost: api.github.com\r\nAuthorization: token "+testCopilotUserToken+"\r\n\r\n")
	if got, want := c.req.Header.Get("Authorization"), "token "+testCopilotUserToken; got != want {
		t.Errorf("flag-off Authorization = %q, want untouched %q", got, want)
	}
}

func TestIsCopilotAuthExchangePath(t *testing.T) {
	cases := map[string]bool{
		"/copilot_internal/v2/token":        true,
		"/copilot_internal/user":            true,
		"/api/v3/copilot_internal/v2/token": true,
		"/copilot_internal":                 false,
		"/repos/o/r/copilot_internal/x":     false,
		"/login/device/code":                false,
		"/graphql":                          false,
		"":                                  false,
	}
	for path, want := range cases {
		if got := isCopilotAuthExchangePath(path); got != want {
			t.Errorf("isCopilotAuthExchangePath(%q) = %v, want %v", path, got, want)
		}
	}
}

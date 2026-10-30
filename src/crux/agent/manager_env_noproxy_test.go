package agent

import "testing"

func TestAgentNoProxy(t *testing.T) {
	const loop = "127.0.0.1,localhost,::1"
	cases := []struct {
		name, url, want string
	}{
		{"unset", "", loop},
		{"ipv4 with port", "http://172.18.0.1:18765/v1/snapshot", loop + ",172.18.0.1"},
		{"hostname", "http://broker.internal:8080", loop + ",broker.internal"},
		{"ipv6 literal", "http://[fd00::1]:18765", loop + ",fd00::1"},
		{"invalid", "http://[::1", loop},
		{"no host", "/just/a/path", loop},
		{"opaque", "mailto:x@y", loop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OMP_AUTH_BROKER_URL", tc.url)
			if got := agentNoProxy(); got != tc.want {
				t.Errorf("agentNoProxy() = %q, want %q", got, tc.want)
			}
		})
	}
}

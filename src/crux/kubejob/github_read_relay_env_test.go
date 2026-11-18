package kubejob

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/sandbox"
)

// A Job routed through the sandbox GitHub read relay (#18865) carries the
// proxy URL and CA paths, and never a GitHub credential, even when one sits in
// hive's own environment or in the launch spec.
func TestManifestRelayEnvCarriesNoGitHubToken(t *testing.T) {
	const secret = "ghs_MUST_NOT_REACH_THE_JOB"
	t.Setenv("GITHUB_TOKEN", secret)
	t.Setenv("GH_TOKEN", secret)
	t.Setenv("HIVE_GITHUB_TOKEN", secret)

	l := &Launcher{Options: Options{WorkspaceClaim: "c"}}
	m := l.manifest("ns", "job", "sub", sandbox.LaunchSpec{
		Image:        "i",
		EnvAllowlist: []string{"GITHUB_TOKEN", "GH_TOKEN", "HIVE_GITHUB_TOKEN"},
		Env: map[string]string{
			"HTTPS_PROXY":    "https://hive-sandbox:relaykey@hive.ns.svc:18445",
			"SSL_CERT_FILE":  "/workspace/.hive/github-read-ca.pem",
			"GIT_SSL_CAINFO": "/workspace/.hive/github-read-ca.pem",
			"GITHUB_TOKEN":   secret,
			"GH_TOKEN":       secret,
		},
	})
	raw, _ := json.Marshal(m)
	s := string(raw)
	if strings.Contains(s, secret) {
		t.Fatalf("the Job manifest leaks a GitHub token:\n%s", s)
	}
	for _, want := range []string{`"name":"HTTPS_PROXY"`, `"name":"SSL_CERT_FILE"`, `"name":"GIT_SSL_CAINFO"`, "relaykey"} {
		if !strings.Contains(s, want) {
			t.Errorf("manifest missing %s\n%s", want, s)
		}
	}
}

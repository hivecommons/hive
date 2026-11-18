package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
)

const relayTestGitHubToken = "ghs_SHOULD_NEVER_APPEAR"

type fakeReadRelay struct {
	enrolled []string
	released int
	err      error
}

func (f *fakeReadRelay) Enroll(agentName string) (string, []byte, func(), error) {
	if f.err != nil {
		return "", nil, nil, f.err
	}
	f.enrolled = append(f.enrolled, agentName)
	return "http://hive-sandbox:kickkey@hive.ns.svc:18445", []byte("-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----\n"), func() { f.released++ }, nil
}

func TestSandboxReadEnvCarriesNoGitHubToken(t *testing.T) {
	env := sandboxReadEnv("http://hive-sandbox:kickkey@hive.ns.svc:18445", "/workspace/.hive/github-read-ca.pem", []string{"internal.example"})
	for k, v := range env {
		if strings.Contains(v, relayTestGitHubToken) || strings.Contains(strings.ToUpper(k), "TOKEN") {
			t.Errorf("env %s=%s looks like a GitHub credential", k, v)
		}
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "SSL_CERT_FILE", "GIT_SSL_CAINFO", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if env[k] == "" {
			t.Errorf("env %s missing", k)
		}
	}
	if !strings.Contains(env["NO_PROXY"], ".svc") || !strings.Contains(env["NO_PROXY"], "internal.example") {
		t.Errorf("NO_PROXY = %q", env["NO_PROXY"])
	}
	if _, ok := env["HTTP_PROXY"]; ok {
		t.Error("plain HTTP must not be proxied")
	}
}

func TestEnrollGitHubReadRelayOffByDefault(t *testing.T) {
	e := &SandboxExecutor{}
	env, release := e.enrollGitHubReadRelay(SandboxKickSpec{Agent: "deps", Runtime: config.SandboxRuntimeJob}, t.TempDir())
	release()
	if env != nil {
		t.Fatalf("no relay configured but env = %v", env)
	}
}

func TestEnrollGitHubReadRelayOnlyForJobRuntime(t *testing.T) {
	relay := &fakeReadRelay{}
	e := &SandboxExecutor{GitHubReadRelay: relay}
	for _, rt := range []string{"", config.SandboxRuntimePodman, config.SandboxRuntimeOpenShell, config.SandboxRuntimeSubstrate} {
		env, release := e.enrollGitHubReadRelay(SandboxKickSpec{Agent: "deps", Runtime: rt}, t.TempDir())
		release()
		if env != nil {
			t.Errorf("runtime %q must not be routed through the relay", rt)
		}
	}
	if len(relay.enrolled) != 0 {
		t.Fatalf("relay enrolled %v for non-Job runtimes", relay.enrolled)
	}
}

func TestEnrollGitHubReadRelayJobRuntime(t *testing.T) {
	relay := &fakeReadRelay{}
	e := &SandboxExecutor{GitHubReadRelay: relay}
	ws := t.TempDir()
	env, release := e.enrollGitHubReadRelay(SandboxKickSpec{Agent: "deps", Runtime: config.SandboxRuntimeJob}, ws)
	if env["HTTPS_PROXY"] == "" || env["SSL_CERT_FILE"] != "/workspace/.hive/github-read-ca.pem" {
		t.Fatalf("env = %v", env)
	}
	if _, err := os.Stat(filepath.Join(ws, ".hive", "github-read-ca.pem")); err != nil {
		t.Fatalf("CA bundle not written: %v", err)
	}
	if relay.released != 0 {
		t.Fatal("key released before the kick ended")
	}
	release()
	if relay.released != 1 {
		t.Fatal("key not released when the kick ended")
	}
}

func TestEnrollGitHubReadRelayFailureIsNotFatal(t *testing.T) {
	e := &SandboxExecutor{GitHubReadRelay: &fakeReadRelay{err: errors.New("boom")}}
	env, release := e.enrollGitHubReadRelay(SandboxKickSpec{Agent: "deps", Runtime: config.SandboxRuntimeJob}, t.TempDir())
	release()
	if env != nil {
		t.Fatalf("env = %v, want none so the kick runs as before", env)
	}
}

func TestMergeSandboxEnvDoesNotMutateInputs(t *testing.T) {
	base := map[string]string{"A": "1"}
	out := mergeSandboxEnv(base, map[string]string{"B": "2"})
	if len(base) != 1 || out["A"] != "1" || out["B"] != "2" {
		t.Fatalf("base=%v out=%v", base, out)
	}
	if got := mergeSandboxEnv(nil, nil); got != nil {
		t.Fatalf("merge of nothing = %v", got)
	}
}

// The relay is installed through crux.Host like every other host capability
// (no type assertion in boot).
func TestManagerSetSandboxGitHubReadRelayInstallsAndClears(t *testing.T) {
	m := &Manager{}
	var host crux.Host = m
	relay := &fakeReadRelay{}
	noProxy := []string{"internal.example"}
	host.SetSandboxGitHubReadRelay(relay, noProxy)
	noProxy[0] = "mutated.example"

	in := m.sandboxReadRelay.cur.Load()
	if in == nil || in.relay != SandboxGitHubReadRelay(relay) {
		t.Fatal("relay was not installed")
	}
	if len(in.noProxy) != 1 || in.noProxy[0] != "internal.example" {
		t.Fatalf("noProxy = %v; the caller's slice must be copied", in.noProxy)
	}
	host.SetSandboxGitHubReadRelay(nil, []string{"ignored.example"})
	if m.sandboxReadRelay.cur.Load() != nil {
		t.Fatal("a nil relay must switch the feature off")
	}
}

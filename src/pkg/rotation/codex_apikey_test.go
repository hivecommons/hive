package rotation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateCodexAuth points Codex auth discovery at an empty temp CODEX_HOME and
// clears the API-key variables, so a developer's own login cannot leak in.
// It returns the CODEX_HOME directory.
func isolateCodexAuth(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	for _, name := range codexAPIKeyEnvVars {
		t.Setenv(name, "")
	}
	return home
}

// fakeCodexAuthRequired installs a codex app-server that refuses the
// initialize handshake the way an install with no ChatGPT login does.
func fakeCodexAuthRequired(t *testing.T) {
	t.Helper()
	script := "#!/bin/sh\n" +
		"IFS= read -r line\n" +
		"printf '%s\\n' '{\"id\":0,\"error\":{\"code\":-32000,\"message\":\"account authentication required\"}}'\n" +
		"exit 0\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeCodexReply installs a codex app-server that answers the rate-limits
// call with result.
func fakeCodexReply(t *testing.T, result string) {
	t.Helper()
	reply := fmt.Sprintf(`{"id":1,"result":%s}`, result)
	script := "#!/bin/sh\n" +
		"IFS= read -r line\n" +
		"printf '%s\\n' '{\"id\":0,\"result\":{\"userAgent\":\"fake/1\"}}'\n" +
		"IFS= read -r line\n" +
		fmt.Sprintf("printf '%%s\\n' '%s'\n", reply) +
		"exit 0\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func writeCodexAuth(t *testing.T, home, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertAPIKeyReading(t *testing.T, h Headroom) {
	t.Helper()
	if h.ProbeErrCause != ProbeCauseAPIKeyLogin || h.AuthMode != AuthModeAPIKey {
		t.Fatalf("headroom cause=%q auth_mode=%q (err %v), want %q/%q", h.ProbeErrCause, h.AuthMode, h.ProbeErr, ProbeCauseAPIKeyLogin, AuthModeAPIKey)
	}
	// Rotation must still see a failed measurement: never a rotation target.
	if h.ProbeErr == nil {
		t.Fatal("API-key headroom has no ProbeErr; rotation would treat it as a measured target")
	}
	r := HeadroomToContributorReading(h)
	if r.State != "available" || r.AuthMode != AuthModeAPIKey || len(r.Limits) != 0 {
		t.Fatalf("reading = %+v, want an available api_key reading with no windows", r)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"auth_mode":"api_key"`) || !strings.Contains(string(b), `"limits":[]`) {
		t.Fatalf("published reading %s lacks auth_mode or an empty limits array", b)
	}
}

func assertHeldReading(t *testing.T, h Headroom) {
	t.Helper()
	if h.AuthMode != "" || h.ProbeErrCause == ProbeCauseAPIKeyLogin {
		t.Fatalf("headroom marked as API-key login: %+v", h)
	}
	if r := HeadroomToContributorReading(h); r.State != "unknown" || r.AuthMode != "" {
		t.Fatalf("reading = %+v, want unknown (hold) with no auth_mode", r)
	}
}

// #19932: an API key in the environment with no ChatGPT login publishes a
// reading the relay admits, not `unknown`.
func TestCodexProberAPIKeyEnvLoginIsMetered(t *testing.T) {
	for _, name := range codexAPIKeyEnvVars {
		t.Run(name, func(t *testing.T) {
			isolateCodexAuth(t)
			fakeCodexAuthRequired(t)
			t.Setenv(name, "test-api-key-value")
			assertAPIKeyReading(t, CodexProber{ThresholdPct: 80}.Probe(context.Background()))
		})
	}
}

// `codex login --with-api-key` stores the key in auth.json with no tokens.
func TestCodexProberAPIKeyInAuthFileIsMetered(t *testing.T) {
	home := isolateCodexAuth(t)
	fakeCodexAuthRequired(t)
	writeCodexAuth(t, home, `{"OPENAI_API_KEY":"test-api-key-value","tokens":null}`)
	assertAPIKeyReading(t, CodexProber{ThresholdPct: 80}.Probe(context.Background()))
}

// An app-server that answers with no windows takes the same path.
func TestCodexProberAPIKeyEmptyWindowsIsMetered(t *testing.T) {
	isolateCodexAuth(t)
	fakeCodexReply(t, `{"rateLimits":{}}`)
	t.Setenv("OPENAI_API_KEY", "test-api-key-value")
	assertAPIKeyReading(t, CodexProber{ThresholdPct: 80}.Probe(context.Background()))
}

// No key and no login is still no_credentials, which holds.
func TestCodexProberNoLoginStillHolds(t *testing.T) {
	isolateCodexAuth(t)
	fakeCodexAuthRequired(t)
	h := CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErrCause != ProbeCauseNoCredentials {
		t.Fatalf("cause = %q, want %q", h.ProbeErrCause, ProbeCauseNoCredentials)
	}
	assertHeldReading(t, h)
}

// A ChatGPT login outranks an API key in the environment: its failed probe is
// a real failure and must keep holding.
func TestCodexProberChatGPTLoginFailureStillHolds(t *testing.T) {
	home := isolateCodexAuth(t)
	fakeCodexAuthRequired(t)
	t.Setenv("OPENAI_API_KEY", "test-api-key-value")
	writeCodexAuth(t, home, `{"OPENAI_API_KEY":null,"tokens":{"access_token":"at","refresh_token":"rt","id_token":"it"}}`)
	assertHeldReading(t, CodexProber{ThresholdPct: 80}.Probe(context.Background()))
}

// An auth.json that cannot be parsed never admits, whatever the environment.
func TestCodexProberUnreadableAuthFileStillHolds(t *testing.T) {
	home := isolateCodexAuth(t)
	fakeCodexAuthRequired(t)
	t.Setenv("CODEX_API_KEY", "test-api-key-value")
	writeCodexAuth(t, home, `{not json`)
	assertHeldReading(t, CodexProber{ThresholdPct: 80}.Probe(context.Background()))
}

// Real windows are always used, even with an API key in the environment.
func TestCodexProberWindowsWinOverAPIKey(t *testing.T) {
	isolateCodexAuth(t)
	fakeCodexAppServer(t, 30, 1787968245)
	t.Setenv("OPENAI_API_KEY", "test-api-key-value")
	h := CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr != nil || h.AuthMode != "" || h.PctRemaining != 70 {
		t.Fatalf("headroom = %+v (err %v), want the measured window and no auth_mode", h, h.ProbeErr)
	}
}

func TestCodexAuthFilePathFollowsAppServerHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	oldShared := sharedCLIHomePath
	t.Cleanup(func() { sharedCLIHomePath = oldShared })

	shared := t.TempDir()
	sharedCLIHomePath = shared
	if got, want := codexAuthFilePath(), filepath.Join(shared, ".codex", "auth.json"); got != want {
		t.Fatalf("with a shared CLI home: path = %q, want %q", got, want)
	}

	sharedCLIHomePath = filepath.Join(t.TempDir(), "missing")
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := codexAuthFilePath(), filepath.Join(home, ".codex", "auth.json"); got != want {
		t.Fatalf("without a shared CLI home: path = %q, want %q", got, want)
	}

	t.Setenv("CODEX_HOME", "/opt/codex-home")
	if got, want := codexAuthFilePath(), filepath.Join("/opt/codex-home", "auth.json"); got != want {
		t.Fatalf("with CODEX_HOME: path = %q, want %q", got, want)
	}
}

func TestHeadroomJSONCarriesAuthMode(t *testing.T) {
	b, err := json.Marshal(codexAPIKeyHeadroom("openai"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"auth_mode":"api_key"`) || !strings.Contains(string(b), `"probe_error_cause":"api_key_login"`) {
		t.Fatalf("headroom JSON %s lacks auth_mode or its cause", b)
	}
}

// A probe failure that says nothing about the auth mode keeps holding even
// with an API key exported: a ChatGPT login the probe cannot see (keyring
// storage, another home) must not fail open on a timeout or transport error.
func TestCodexProberUnrelatedFailureWithAPIKeyStillHolds(t *testing.T) {
	isolateCodexAuth(t)
	t.Setenv("OPENAI_API_KEY", "test-api-key-value")
	fakeCLI(t, "codex", "", 3)
	h := CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErr == nil {
		t.Fatal("ProbeErr = nil, want a failed probe")
	}
	assertHeldReading(t, h)

	// A reply that does not parse is a schema problem, not an API-key login.
	fakeCodexReply(t, `"not an object"`)
	assertHeldReading(t, CodexProber{ThresholdPct: 80}.Probe(context.Background()))
}

// A missing codex CLI stays not_installed with a key exported, so the
// publish-only manager still skips the pool and removes its marker.
func TestCodexProberNotInstalledWithAPIKeyStaysNotInstalled(t *testing.T) {
	isolateCodexAuth(t)
	t.Setenv("OPENAI_API_KEY", "test-api-key-value")
	t.Setenv("PATH", t.TempDir())
	h := CodexProber{ThresholdPct: 80}.Probe(context.Background())
	if h.ProbeErrCause != ProbeCauseNotInstalled || h.AuthMode != "" {
		t.Fatalf("cause = %q auth_mode = %q, want %q and no auth_mode", h.ProbeErrCause, h.AuthMode, ProbeCauseNotInstalled)
	}
}

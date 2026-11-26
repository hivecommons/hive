package github

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// pushRedirectingConfig is every checkout config key that can send the
// relayed push, or its token header, somewhere other than the hive's remote
// (hivecommons/hive#19959). Each must make the push be refused.
var pushRedirectingConfig = [][2]string{
	{"url.https://evil.example/.insteadOf", "https://github.com/"},
	{"url.https://evil.example/.pushInsteadOf", "https://github.com/"},
	{"http.proxy", "http://evil.example:3128"},
	{"http.https://github.com/.proxy", "http://evil.example:3128"},
	{"http.https://github.com/o/r.git.sslVerify", "false"},
	{"http.sslCAInfo", "/tmp/evil-ca.pem"},
	{"http.sslCAPath", "/tmp/evil-ca"},
	{"http.curloptResolve", "github.com:443:203.0.113.9"},
	{"http.proxySSLCAInfo", "/tmp/evil-ca.pem"},
	{"credential.https://github.com.helper", "!evil"},
	{"remote.origin.proxy", "http://evil.example:3128"},
}

func TestPushBranchConfigRefusal_ParsesListing(t *testing.T) {
	clean := "local\x00core.bare\nfalse\x00local\x00user.email\nme@example.com\x00" +
		"local\x00http.postbuffer\n524288000\x00local\x00credential.username\nme\x00" +
		"local\x00remote.origin.url\nhttps://github.com/o/r.git\x00" +
		// The hive's own command-scope overrides are never offenders.
		"command\x00http.https://github.com/o/r.git.extraheader\nAuthorization: Basic x\x00" +
		"command\x00credential.helper\n\x00"
	if reason := pushBranchConfigRefusal(clean); reason != "" {
		t.Fatalf("clean listing refused: %s", reason)
	}
	for _, kv := range pushRedirectingConfig {
		listing := clean + "local\x00" + strings.ToLower(kv[0]) + "\n" + kv[1] + "\x00"
		reason := pushBranchConfigRefusal(listing)
		if reason == "" || !strings.Contains(reason, strings.ToLower(kv[0])) {
			t.Errorf("%s=%s: reason = %q, want a refusal naming the key", kv[0], kv[1], reason)
		}
	}
	// Worktree-scope and included config count too.
	if pushBranchConfigRefusal("worktree\x00url.https://evil/.insteadof\nhttps://github.com/\x00") == "" {
		t.Error("worktree-scope insteadOf not refused")
	}
}

// gitCheckout makes a real repo with the given local config, for the tests
// that check what git itself does under pushBranchGitEnv.
func gitCheckout(t *testing.T, config ...[2]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	for _, kv := range config {
		if out, err := exec.Command("git", "-C", dir, "config", "--local", kv[0], kv[1]).CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v: %s", kv[0], err, out)
		}
	}
	return dir
}

// Each redirecting key, set in a real checkout's local config, is seen by the
// relay's config check and refused.
func TestPushBranchConfigRefusal_RealCheckout(t *testing.T) {
	const remote = "https://github.com/o/r.git"
	for _, kv := range pushRedirectingConfig {
		t.Run(kv[0], func(t *testing.T) {
			dir := gitCheckout(t, kv)
			env := pushBranchGitEnv(dir, remote, gitBasicAuthHeader("tok-secret"))
			out, err := runPushBranchCommand(context.Background(), dir, env, "git", pushBranchConfigListArgs...)
			if err != nil {
				t.Fatalf("git config --list: %v: %s", err, out)
			}
			if reason := pushBranchConfigRefusal(out); reason == "" {
				t.Errorf("%s=%s was not refused; listing:\n%q", kv[0], kv[1], out)
			}
		})
	}
	// An included file is listed under the including scope, so it is checked.
	inc := t.TempDir() + "/evil.inc"
	if err := os.WriteFile(inc, []byte("[url \"https://evil.example/\"]\n\tinsteadOf = https://github.com/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := gitCheckout(t, [2]string{"include.path", inc})
	out, err := runPushBranchCommand(context.Background(), dir, pushBranchGitEnv(dir, remote, ""), "git", pushBranchConfigListArgs...)
	if err != nil {
		t.Fatalf("git config --list: %v: %s", err, out)
	}
	if pushBranchConfigRefusal(out) == "" {
		t.Errorf("an insteadOf from an included file was not refused; listing:\n%q", out)
	}
}

// The keys pushBranchGitEnv pins are overridden whatever the checkout says:
// git resolves them to the hive's values, so no helper or askpass program
// runs and TLS stays verified.
func TestPushBranchGitEnv_PinsCheckoutConfig(t *testing.T) {
	dir := gitCheckout(t,
		[2]string{"credential.helper", "!evil"},
		[2]string{"core.askPass", "/tmp/evil-askpass"},
		[2]string{"http.sslVerify", "false"},
		[2]string{"core.hooksPath", "/tmp/evil-hooks"},
	)
	env := pushBranchGitEnv(dir, "https://github.com/o/r.git", gitBasicAuthHeader("tok-secret"))
	for key, want := range map[string]string{
		"credential.helper": "",
		"core.askpass":      "",
		"http.sslverify":    "true",
		"core.hookspath":    os.DevNull,
	} {
		out, err := runPushBranchCommand(context.Background(), dir, env, "git", "config", "--get", key)
		if err != nil {
			t.Fatalf("git config --get %s: %v: %s", key, err, out)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("effective %s = %q, want %q", key, got, want)
		}
	}
	got := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}
	if got["GIT_CONFIG_NOSYSTEM"] != "1" || got["GIT_CONFIG_GLOBAL"] != os.DevNull {
		t.Errorf("system/global config not switched off: %v", env)
	}
	if v, ok := got["GIT_ASKPASS"]; !ok || v != "" {
		t.Errorf("GIT_ASKPASS not cleared: %v", env)
	}
	if v, ok := got["SSH_ASKPASS"]; !ok || v != "" {
		t.Errorf("SSH_ASKPASS not cleared: %v", env)
	}
}

// A checkout whose config could redirect the push is refused by the GitHub
// watcher, and nothing is pushed.
func TestPushBranchRequestWatcher_RefusesRedirectingConfig(t *testing.T) {
	for _, kv := range pushRedirectingConfig {
		t.Run(kv[0], func(t *testing.T) {
			c := pushTestClient(t, "http://127.0.0.1:1")
			dir := withPushDir(t)
			var calls []pushExecCall
			old := pushBranchExec
			pushBranchExec = func(_ context.Context, d string, env []string, name string, args ...string) (string, error) {
				calls = append(calls, pushExecCall{dir: d, env: env, args: append([]string{name}, args...)})
				switch args[0] {
				case "rev-parse":
					return "cafe1234\n", nil
				case "config":
					return "local\x00core.bare\nfalse\x00local\x00" + strings.ToLower(kv[0]) + "\n" + kv[1] + "\x00", nil
				}
				return "", nil
			}
			t.Cleanup(func() { pushBranchExec = old })

			reqPath, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-9", Dir: t.TempDir(), Agent: "scanner"})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessPushBranchRequestsOnce(context.Background())

			for _, call := range calls {
				if call.args[1] == "push" {
					t.Fatalf("pushed from a checkout setting %s: %v", kv[0], call.args)
				}
			}
			resp := readPushBranchResult(t, reqPath)
			if resp.OK || !strings.Contains(resp.Error, "authorization denied") || !strings.Contains(resp.Error, strings.ToLower(kv[0])) {
				t.Errorf("result = %+v, want a denial naming %s", resp, kv[0])
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("denied request not parked: %v", err)
			}
		})
	}
}

// The token never reaches git's argv: it rides only as a command-scope
// extraHeader for the exact remote.
func TestPushBranchRequestWatcher_TokenNotInArgv(t *testing.T) {
	c := pushTestClient(t, "http://127.0.0.1:1")
	c.authToken = "tok-secret-argv"
	calls := withPushExec(t, "cafe1234", nil, "")
	dir := withPushDir(t)
	if _, err := WritePushBranchRequest(dir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-10", Dir: t.TempDir(), Agent: "scanner"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPushBranchRequestsOnce(context.Background())
	header := gitBasicAuthHeader("tok-secret-argv")
	for _, call := range *calls {
		for _, a := range call.args {
			if strings.Contains(a, "tok-secret-argv") || strings.Contains(a, header) {
				t.Fatalf("token in argv: %v", call.args)
			}
		}
		for _, e := range call.env {
			if strings.Contains(e, header) && !strings.HasPrefix(e, "GIT_CONFIG_VALUE_") {
				t.Errorf("token header outside a command-scope config value: %q", e)
			}
		}
	}
}

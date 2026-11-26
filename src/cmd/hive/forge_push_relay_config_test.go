package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

// realForgePushExec is the relay's real git runner, captured before any test
// installs a fake.
var realForgePushExec = forgePushExec

// forgeRedirectingConfigKeys are checkout config keys that can send the forge
// push relay's credentialed push away from the Forgejo remote it chose
// (hivecommons/hive#19959). Values are unroutable so a regression cannot
// reach a real host.
var forgeRedirectingConfigKeys = [][2]string{
	{"url.https://127.0.0.1:1/.insteadOf", "https://fj.example/"},
	{"url.https://127.0.0.1:1/.pushInsteadOf", "https://fj.example/"},
	{"http.proxy", "http://127.0.0.1:1"},
	{"http.sslCAInfo", "/tmp/evil-ca.pem"},
	{"http.https://fj.example/.proxy", "http://127.0.0.1:1"},
	{"http.https://fj.example/.sslVerify", "false"},
	{"credential.https://fj.example.helper", "!echo evil"},
	{"remote.https://fj.example/acme/widgets.git.pushurl", "https://127.0.0.1:1/acme/widgets.git"},
	{"remote.origin.proxy", "http://127.0.0.1:1"},
}

func forgePushTestHeader() string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+forgePushTestToken))
}

// Each redirecting key in the checkout's config refuses the push as a policy
// denial, before any push, and the listing that found it ran without the
// token in argv or environment.
func TestForgeRelayPushRefusesRedirectingCheckoutConfig(t *testing.T) {
	for _, kv := range forgeRedirectingConfigKeys {
		t.Run(kv[0], func(t *testing.T) {
			h := newPushHarness(t)
			h.configListing = "local\x00core.bare\nfalse\x00local\x00" + strings.ToLower(kv[0]) + "\n" + kv[1] + "\x00"
			path := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
			h.relay.scanOnce(context.Background())

			if len(h.pushCalls()) != 0 {
				t.Fatalf("pushed from a checkout with %s: %+v", kv[0], h.calls)
			}
			var resp github.PushBranchResponse
			readRelayResult(t, path, &resp)
			if resp.OK || !strings.Contains(resp.Error, "authorization denied") || !strings.Contains(resp.Error, strings.ToLower(kv[0])) {
				t.Errorf("resp = %+v, want a denial naming %s", resp, kv[0])
			}
			assertExists(t, path+".denied", true)
			assertForgeConfigListingTokenFree(t, h.calls)
		})
	}
}

// A checkout whose config cannot be read is retried, never pushed.
func TestForgeRelayPushConfigReadFailureRetries(t *testing.T) {
	h := newPushHarness(t)
	h.configErr = errors.New("exit status 128")
	h.configListing = "fatal: bad config line 3 in file .git/config\n"
	path := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
	h.relay.scanOnce(context.Background())
	if len(h.pushCalls()) != 0 {
		t.Fatalf("pushed without reading the checkout config: %+v", h.calls)
	}
	assertExists(t, path, true)
	var resp github.PushBranchResponse
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "reading the git config") || !strings.Contains(resp.Error, "bad config line") {
		t.Errorf("resp = %+v", resp)
	}
}

// The push itself runs with system and global config off, askpass unset and
// TLS verification pinned, and the token only in the remote-scoped header.
func TestForgeRelayPushEnvPinsRedirectingSettings(t *testing.T) {
	h := newPushHarness(t)
	path := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
	h.relay.scanOnce(context.Background())
	pushes := h.pushCalls()
	if len(pushes) != 1 {
		t.Fatalf("pushes = %+v, want one", h.calls)
	}
	env := strings.Join(pushes[0].env, "\n")
	for _, want := range []string{
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_ASKPASS=\n", "SSH_ASKPASS=\n",
		"GIT_ALLOW_PROTOCOL=https", "=http.sslVerify\n", "=core.askPass\n", "GIT_NO_LAZY_FETCH=1\n",
		"GIT_DIR=" + pushes[0].dir + "\n", "GIT_OBJECT_DIRECTORY=",
	} {
		if !strings.Contains(env+"\n", want) {
			t.Errorf("push env lacks %q:\n%s", want, env)
		}
	}
	for _, a := range pushes[0].args {
		if strings.Contains(a, forgePushTestToken) {
			t.Fatalf("the token reached git's argv: %v", pushes[0].args)
		}
	}
	assertForgeConfigListingTokenFree(t, h.calls)
	var resp github.PushBranchResponse
	readRelayResult(t, path, &resp)
	if !resp.OK {
		t.Errorf("resp = %+v, want ok", resp)
	}
}

// End to end with a real git for everything but the push: a Forgejo checkout
// whose local config redirects the push is refused.
func TestForgeRelayPushRefusesRedirectingConfigRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	for _, kv := range forgeRedirectingConfigKeys {
		t.Run(kv[0], func(t *testing.T) {
			h := newPushHarness(t)
			gitIn(t, h.checkout, "init", "-q", "-b", "fix-1")
			gitIn(t, h.checkout, "-c", "user.name=worker", "-c", "user.email=worker@example.invalid",
				"commit", "-q", "--allow-empty", "-m", "change\n\nSigned-off-by: worker <worker@example.invalid>")
			gitIn(t, h.checkout, "config", "--local", kv[0], kv[1])
			forgePushExec = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
				h.calls = append(h.calls, gitCall{dir: dir, env: env, args: args})
				if args[0] == "push" {
					return "", nil
				}
				return realForgePushExec(ctx, dir, env, args...)
			}
			path := writeRelayRequest(t, h.relay.pushDir, "a.json", h.goodPush())
			h.relay.scanOnce(context.Background())

			if len(h.pushCalls()) != 0 {
				t.Fatalf("pushed from a checkout with %s", kv[0])
			}
			var resp github.PushBranchResponse
			readRelayResult(t, path, &resp)
			if resp.OK || !strings.Contains(resp.Error, "can redirect") {
				t.Errorf("resp = %+v, want the redirecting-config denial", resp)
			}
			assertForgeConfigListingTokenFree(t, h.calls)
		})
	}
}

// gitIn runs git in dir with system and global config off.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// assertForgeConfigListingTokenFree checks the config listing ran, and that
// neither it nor any other call put the token in argv, nor the listing in
// its environment.
func assertForgeConfigListingTokenFree(t *testing.T, calls []gitCall) {
	t.Helper()
	header := forgePushTestHeader()
	saw := false
	for _, c := range calls {
		joined := strings.Join(c.args, " ")
		if strings.Contains(joined, forgePushTestToken) || strings.Contains(joined, header) {
			t.Errorf("the token reached git's argv: %v", c.args)
		}
		if c.args[0] == "config" {
			saw = true
		}
		if c.args[0] != "push" {
			if env := strings.Join(c.env, "\n"); strings.Contains(env, header) || strings.Contains(env, "extraHeader") {
				t.Errorf("git %s ran in the agent's checkout with the token in its environment", c.args[0])
			}
		}
	}
	if !saw {
		t.Error("the checkout's config was never read")
	}
}

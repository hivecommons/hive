package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pushRedirectingConfigKeys are repository-scoped keys that can send the
// push-branch relay's credentialed push somewhere other than the URL it chose
// or weaken how it is protected (hivecommons/hive#19959). Each must refuse
// the push. Values point at unroutable addresses so a regression cannot
// reach a real host.
var pushRedirectingConfigKeys = [][2]string{
	{"url.https://127.0.0.1:1/.insteadOf", "https://github.com/"},
	{"url.https://127.0.0.1:1/.pushInsteadOf", "https://github.com/"},
	{"http.proxy", "http://127.0.0.1:1"},
	{"http.proxyAuthMethod", "basic"},
	{"http.proxySSLCAInfo", "/tmp/evil-ca.pem"},
	{"http.sslCAInfo", "/tmp/evil-ca.pem"},
	{"http.sslCAPath", "/tmp/evil-ca"},
	{"http.extraHeader", "X-Evil: 1"},
	{"http.curloptResolve", "github.com:443:127.0.0.1"},
	{"http.cookieFile", "/tmp/cookies"},
	{"http.https://github.com/.proxy", "http://127.0.0.1:1"},
	{"http.https://github.com/.sslVerify", "false"},
	{"http.https://github.com/o/r.git/info/refs.sslCAInfo", "/tmp/evil-ca.pem"},
	{"credential.https://github.com.helper", "!echo evil"},
	{"remote.https://github.com/o/r.git.pushurl", "https://127.0.0.1:1/o/r.git"},
	{"remote.origin.proxy", "http://127.0.0.1:1"},
	{"remote.origin.proxyAuthMethod", "basic"},
}

// pushBenignConfigKeys are keys a fresh clone, an ordinary agent session or
// a transport tweak writes. None may refuse the push.
var pushBenignConfigKeys = [][2]string{
	{"core.bare", "false"},
	{"remote.origin.url", "https://github.com/o/r.git"},
	{"remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
	{"remote.origin.promisor", "true"},
	{"remote.origin.partialclonefilter", "blob:none"},
	{"branch.main.remote", "origin"},
	{"branch.main.merge", "refs/heads/main"},
	{"user.name", "scanner"},
	{"user.email", "scanner@example.invalid"},
	{"http.postBuffer", "524288000"},
	{"http.lowSpeedLimit", "1000"},
	{"http.version", "HTTP/1.1"},
	// Pinned at command scope, which outranks a plain local value.
	{"http.sslVerify", "false"},
	{"http.followRedirects", "true"},
	{"credential.helper", "!echo evil"},
	{"core.askPass", "/tmp/evil-askpass"},
	{"push.gpgSign", "true"},
	{"log.showSignature", "true"},
	{"push.recurseSubmodules", "on-demand"},
}

// configListing renders (scope, key, value) records the way
// `git config --list -z --show-scope` prints them.
func configListing(records ...[3]string) string {
	var b strings.Builder
	for _, r := range records {
		b.WriteString(r[0] + "\x00" + r[1] + "\n" + r[2] + "\x00")
	}
	return b.String()
}

func TestPushBranchConfigRefusal_Listing(t *testing.T) {
	for _, kv := range pushRedirectingConfigKeys {
		for _, scope := range []string{"local", "worktree"} {
			listing := configListing([3]string{"local", "core.bare", "false"}, [3]string{scope, strings.ToLower(kv[0]), kv[1]})
			reason := pushBranchConfigRefusal(listing)
			if reason == "" || !strings.Contains(reason, strings.ToLower(kv[0])) {
				t.Errorf("%s %s: refusal = %q, want it to name the key", scope, kv[0], reason)
			}
			if strings.Contains(reason, kv[1]) {
				t.Errorf("%s %s: refusal quotes the value: %q", scope, kv[0], reason)
			}
		}
	}
	for _, kv := range pushBenignConfigKeys {
		if reason := pushBranchConfigRefusal(configListing([3]string{"local", strings.ToLower(kv[0]), kv[1]})); reason != "" {
			t.Errorf("benign %s refused: %q", kv[0], reason)
		}
	}
	// The relay's own command-scope pins (and its token header) are not the
	// agent's config and never refuse; system and global are off anyway.
	ours := configListing(
		[3]string{"command", "http.https://github.com/o/r.git.extraheader", "Authorization: Basic secret"},
		[3]string{"command", "http.proxy", "http://hive-egress:3128"},
		[3]string{"global", "url.https://mirror/.insteadof", "https://github.com/"},
	)
	if reason := pushBranchConfigRefusal(ours); reason != "" {
		t.Errorf("command/global scope refused: %q", reason)
	}
	if reason := pushBranchConfigRefusal(""); reason != "" {
		t.Errorf("empty listing refused: %q", reason)
	}
}

// Anything the parser does not recognise fails closed: the listing is the
// relay's only view of the checkout's config.
func TestPushBranchConfigRefusal_UnparseableFailsClosed(t *testing.T) {
	for name, listing := range map[string]string{
		"stderr noise":  "warning: something\nlocal\x00core.bare\nfalse\x00",
		"odd fields":    "local\x00core.bare\nfalse\x00local\x00",
		"unknown scope": "agent\x00http.proxy\nhttp://127.0.0.1:1\x00",
		"empty key":     "local\x00\nvalue\x00",
	} {
		if reason := pushBranchConfigRefusal(listing); reason == "" {
			t.Errorf("%s: listing %q accepted", name, listing)
		}
	}
}

// A key with no value (an implicit true) is printed without a newline; it
// is still a key, judged like any other.
func TestPushBranchConfigRefusal_ValuelessKey(t *testing.T) {
	if reason := pushBranchConfigRefusal("local\x00foo.bar\x00"); reason != "" {
		t.Errorf("a value-less ordinary key was refused: %q", reason)
	}
	if reason := pushBranchConfigRefusal("local\x00http.sslcainfo\x00"); !strings.Contains(reason, "http.sslcainfo") {
		t.Errorf("a value-less redirecting key was accepted: %q", reason)
	}
}

func TestPushBranchConfigRefusal_CapsNamedKeys(t *testing.T) {
	var records [][3]string
	for i := 0; i < pushBranchConfigRefusalMaxKeys+3; i++ {
		records = append(records, [3]string{"local", "url.https://h" + string(rune('a'+i)) + "/.insteadof", "x"})
	}
	reason := pushBranchConfigRefusal(configListing(records...))
	if !strings.Contains(reason, "and 3 more") || strings.Contains(reason, "url.https://h"+string(rune('a'+pushBranchConfigRefusalMaxKeys))+"/") {
		t.Errorf("refusal = %q, want %d keys named and the rest counted", reason, pushBranchConfigRefusalMaxKeys)
	}
}

// requireGit skips a test that needs a real git binary.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// gitRepo creates a repository with one commit on branch and returns its path.
func gitRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_AUTHOR_NAME=scanner", "GIT_AUTHOR_EMAIL=scanner@example.invalid",
		"GIT_COMMITTER_NAME=scanner", "GIT_COMMITTER_EMAIL=scanner@example.invalid")
	for _, args := range [][]string{
		{"init", "-q", "-b", branch},
		{"commit", "-q", "--allow-empty", "-m", "change\n\nSigned-off-by: scanner <scanner@example.invalid>"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// setLocalConfig writes key=value into the repository's own config.
func setLocalConfig(t *testing.T, dir, key, value string) {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", key, value)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config %s: %v\n%s", key, err, out)
	}
}

// Against a real git: every redirecting key set in the checkout, directly or
// through an include, is seen by the listing the relay reads and refuses the
// push; the ordinary keys do not.
func TestPushBranchConfigRefusal_RealGit(t *testing.T) {
	requireGit(t)
	const remote = "https://github.com/o/r.git"
	list := func(dir string) string {
		out, err := runPushBranchCommand(context.Background(), dir, pushBranchGitEnv(dir, remote, ""), "git", pushBranchConfigListArgs...)
		if err != nil {
			t.Fatalf("listing config: %v\n%s", err, out)
		}
		return out
	}
	for _, kv := range pushRedirectingConfigKeys {
		dir := gitRepo(t, "fix")
		setLocalConfig(t, dir, kv[0], kv[1])
		if reason := pushBranchConfigRefusal(list(dir)); reason == "" {
			t.Errorf("%s=%s in the checkout was not refused", kv[0], kv[1])
		}
	}

	dir := gitRepo(t, "fix")
	included := filepath.Join(t.TempDir(), "extra.gitconfig")
	if err := os.WriteFile(included, []byte("[url \"https://127.0.0.1:1/\"]\n\tinsteadOf = https://github.com/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setLocalConfig(t, dir, "include.path", included)
	if reason := pushBranchConfigRefusal(list(dir)); !strings.Contains(reason, "insteadof") {
		t.Errorf("an insteadOf behind include.path was not refused: %q", reason)
	}

	dir = gitRepo(t, "fix")
	for _, kv := range pushBenignConfigKeys {
		setLocalConfig(t, dir, kv[0], kv[1])
	}
	if reason := pushBranchConfigRefusal(list(dir)); reason != "" {
		t.Errorf("a checkout with only ordinary keys was refused: %q", reason)
	}
}

// Against a real git: with the relay's environment, the checkout's values
// for every pinned key lose to the pin, and system/global config (which the
// hive's process environment could point anywhere) is not read at all.
func TestPushBranchGitEnv_RealGitPinsOutrankCheckout(t *testing.T) {
	requireGit(t)
	global := filepath.Join(t.TempDir(), "global.gitconfig")
	if err := os.WriteFile(global, []byte("[http]\n\tproxy = http://127.0.0.1:1\n[url \"https://127.0.0.1:1/\"]\n\tinsteadOf = https://github.com/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_ASKPASS", "/tmp/evil-askpass")
	t.Setenv("SSH_ASKPASS", "/tmp/evil-askpass")

	dir := gitRepo(t, "fix")
	for _, kv := range pushBenignConfigKeys {
		setLocalConfig(t, dir, kv[0], kv[1])
	}
	const remote = "https://github.com/o/r.git"
	env := pushBranchGitEnv(dir, remote, "")
	get := func(args ...string) string {
		out, _ := runPushBranchCommand(context.Background(), dir, env, "git", append([]string{"config"}, args...)...)
		return strings.TrimSpace(out)
	}
	for key, want := range map[string]string{
		"http.sslverify":         "true",
		"http.followredirects":   "initial",
		"core.askpass":           "",
		"push.gpgsign":           "false",
		"log.showsignature":      "false",
		"push.recursesubmodules": "no",
	} {
		if got := get("--get-urlmatch", key, remote); got != want {
			t.Errorf("effective %s = %q, want the pinned %q", key, got, want)
		}
	}
	// credential.helper is a list; an empty entry resets it, so the last
	// value is what git acts on.
	raw, _ := runPushBranchCommand(context.Background(), dir, env, "git", "config", "--get-all", "credential.helper")
	helpers := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if last := helpers[len(helpers)-1]; last != "" {
		t.Errorf("last credential.helper = %q, want the empty reset", last)
	}
	if got := get("--get-urlmatch", "http.proxy", remote); got != "" {
		t.Errorf("global http.proxy reached the push: %q", got)
	}
	if got := get("--get-regexp", `^url\.`); got != "" {
		t.Errorf("global url rewrite reached the push: %q", got)
	}
	// The environment git hands the programs it starts carries no askpass
	// program and forbids lazy fetches: a shell alias is such a child.
	out, err := runPushBranchCommand(context.Background(), dir, env, "git",
		"-c", `alias.envdump=!printf '%s|%s|%s' "$GIT_ASKPASS" "$SSH_ASKPASS" "$GIT_NO_LAZY_FETCH"`, "envdump")
	if err != nil || out != "||1" {
		t.Errorf("child environment = %q (%v), want askpass unset and GIT_NO_LAZY_FETCH=1", out, err)
	}
}

// End to end through the GitHub push watcher with a real git for every
// command except the push itself: a checkout carrying a redirecting key is
// refused before any push, and the config listing never sees the token.
func TestPushBranchRequestWatcher_RefusesRedirectingCheckoutConfig(t *testing.T) {
	requireGit(t)
	const token = "ghs_push_token_19959"
	for _, kv := range pushRedirectingConfigKeys {
		t.Run(kv[0], func(t *testing.T) {
			c := pushTestClient(t, "http://127.0.0.1:1")
			c.authToken = token
			calls := withRealGitExceptPush(t)
			reqDir := withPushDir(t)
			checkout := gitRepo(t, "scanner/fix-9")
			setLocalConfig(t, checkout, kv[0], kv[1])

			reqPath, err := WritePushBranchRequest(reqDir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-9", Dir: checkout, Agent: "scanner"})
			if err != nil {
				t.Fatal(err)
			}
			c.ProcessPushBranchRequestsOnce(context.Background())

			resp := readPushBranchResult(t, reqPath)
			if resp.OK || !strings.Contains(resp.Error, "authorization denied") || !strings.Contains(resp.Error, "can redirect") {
				t.Errorf("result = %+v, want a denial for the redirecting config", resp)
			}
			if _, err := os.Stat(reqPath + ".denied"); err != nil {
				t.Errorf("refused request not parked as .denied: %v", err)
			}
			assertNoPushAndNoTokenLeak(t, *calls, token)
		})
	}

	t.Run("ordinary checkout pushes", func(t *testing.T) {
		c := pushTestClient(t, "http://127.0.0.1:1")
		c.authToken = token
		calls := withRealGitExceptPush(t)
		reqDir := withPushDir(t)
		checkout := gitRepo(t, "scanner/fix-9")
		for _, kv := range pushBenignConfigKeys {
			setLocalConfig(t, checkout, kv[0], kv[1])
		}
		reqPath, err := WritePushBranchRequest(reqDir, PushBranchRequest{Repo: "o/r", Branch: "scanner/fix-9", Dir: checkout, Agent: "scanner"})
		if err != nil {
			t.Fatal(err)
		}
		c.ProcessPushBranchRequestsOnce(context.Background())
		var pushes []pushExecCall
		for _, call := range *calls {
			if call.args[1] == "push" {
				pushes = append(pushes, call)
			}
		}
		if len(pushes) != 1 {
			t.Fatalf("pushes = %d (result %+v), want 1", len(pushes), readPushBranchResult(t, reqPath))
		}
		if !strings.Contains(strings.Join(pushes[0].args, " "), "https://github.com/o/r.git") {
			t.Errorf("push args = %v, want the explicit remote URL", pushes[0].args)
		}
		if !strings.Contains(strings.Join(pushes[0].env, "\n"), gitBasicAuthHeader(token)) {
			t.Error("the push itself lost its scoped auth header")
		}
	})
}

// withRealGitExceptPush runs every watcher git command for real and records
// it, except `git push`, which is recorded and fails without touching the
// network.
func withRealGitExceptPush(t *testing.T) *[]pushExecCall {
	t.Helper()
	var calls []pushExecCall
	old := pushBranchExec
	pushBranchExec = func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
		calls = append(calls, pushExecCall{dir: dir, env: env, args: append([]string{name}, args...)})
		if len(args) > 0 && args[0] == "push" {
			return "", nil
		}
		return runPushBranchCommand(ctx, dir, env, name, args...)
	}
	t.Cleanup(func() { pushBranchExec = old })
	return &calls
}

// assertNoPushAndNoTokenLeak checks that no push ran, the token never reached
// argv, and the config listing ran without the auth header.
func assertNoPushAndNoTokenLeak(t *testing.T, calls []pushExecCall, token string) {
	t.Helper()
	header := gitBasicAuthHeader(token)
	sawListing := false
	for _, call := range calls {
		joined := strings.Join(call.args, " ")
		if call.args[1] == "push" {
			t.Errorf("a push ran from a refused checkout: %v", call.args)
		}
		if strings.Contains(joined, token) || strings.Contains(joined, header) {
			t.Errorf("the token reached git's argv: %v", call.args)
		}
		if call.args[1] == "config" {
			sawListing = true
		}
		if call.args[1] != "push" {
			if env := strings.Join(call.env, "\n"); strings.Contains(env, header) || strings.Contains(env, "extraHeader") {
				t.Errorf("git %s ran in the agent's checkout with the auth header in its environment", call.args[1])
			}
		}
	}
	if !sawListing {
		t.Error("the checkout's config was never read")
	}
}

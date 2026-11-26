package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/proxy"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/config"
)

// fakeBundle is what the stub `git bundle create` writes.
const fakeBundle = "# v2 git bundle\n0123456789abcdef0123456789abcdef01234567 refs/heads/main\n\nPACK-objects-only"

type cloneHarness struct {
	*relayHarness
	calls     []gitCall
	tmp       string
	keyFile   string
	chowned   []int
	fetchErr  error
	fetchOut  string
	verifyErr error
	defErr    error
}

func writeCloneSigningKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "forgejo-jwt.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func newCloneHarness(t *testing.T) *cloneHarness {
	t.Helper()
	t.Setenv(forgejoTestTokenEnv, forgePushTestToken)
	t.Cleanup(func() { proxy.SetForgeHosts(nil) })
	h := &cloneHarness{relayHarness: newRelayHarness(t), tmp: t.TempDir(), keyFile: writeCloneSigningKey(t)}
	// Staging dirs land here, so a test can see that none is left behind.
	t.Setenv("TMPDIR", h.tmp)
	h.relay.cloneDir = t.TempDir()
	h.cfg.Forgejo.Integration = &config.ForgejoIntegrationConfig{
		Issuer:    "https://keys.example.org",
		KeyFile:   h.keyFile,
		Audiences: map[string]string{"contributor": "aud-contributor"},
	}
	uid := os.Getuid()
	h.relay.withCloneRelay(func(agent string, fileUID int) (agentmode.TokenTier, error) {
		if agent != "worker" || fileUID != uid {
			return "", errors.New("not this agent's request")
		}
		return agentmode.TokenTierContributor, nil
	})
	prevExec, prevDef, prevChown := forgeCloneExec, forgeRepoDefaultBranch, forgeCloneChown
	t.Cleanup(func() { forgeCloneExec, forgeRepoDefaultBranch, forgeCloneChown = prevExec, prevDef, prevChown })
	forgeCloneExec = func(_ context.Context, dir string, env []string, args ...string) (string, error) {
		h.calls = append(h.calls, gitCall{dir: dir, env: env, args: args})
		switch args[0] {
		case "fetch":
			return h.fetchOut, h.fetchErr
		case "rev-parse":
			return "", h.verifyErr
		case "bundle":
			return "", os.WriteFile(args[2], []byte(fakeBundle), 0o600)
		}
		return "", nil
	}
	forgeRepoDefaultBranch = func(context.Context, *config.Config, string) (string, error) { return "main", h.defErr }
	forgeCloneChown = func(_ *os.File, uid int) error {
		h.chowned = append(h.chowned, uid)
		return nil
	}
	return h
}

func goodClone() forgeCloneRequest { return forgeCloneRequest{Agent: "worker", Repo: "widgets"} }

func (h *cloneHarness) callsOf(cmd string) []gitCall {
	var out []gitCall
	for _, c := range h.calls {
		if c.args[0] == cmd {
			out = append(out, c)
		}
	}
	return out
}

// mintedToken is the JWT the fetch carried, read back from its extraHeader.
func (h *cloneHarness) mintedToken(t *testing.T) string {
	t.Helper()
	fetches := h.callsOf("fetch")
	if len(fetches) != 1 {
		t.Fatalf("fetches = %d, want 1", len(fetches))
	}
	for _, e := range fetches[0].env {
		_, v, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(e, "GIT_CONFIG_VALUE_") || !strings.HasPrefix(v, "Authorization: Basic ") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "Authorization: Basic "))
		if err != nil {
			t.Fatal(err)
		}
		_, tok, _ := strings.Cut(string(raw), ":")
		return tok
	}
	t.Fatalf("the fetch carried no Authorization header: %q", fetches[0].env)
	return ""
}

// stagingDirs lists the hive-clone-* staging dirs left under TMPDIR.
func (h *cloneHarness) stagingDirs(t *testing.T) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(h.tmp, "hive-clone-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A forge-only hive clones a private repo for a pod-run agent (#19997):
// the agent gets a bundle it alone owns, and nothing else.
func TestForgeCloneRelayHandsTheAgentABundle(t *testing.T) {
	h := newCloneHarness(t)
	path := writeRelayRequest(t, h.relay.cloneDir, "worker-1.json", goodClone())
	h.relay.scanOnce(context.Background())

	var resp forgeCloneResponse
	readRelayResult(t, path, &resp)
	if !resp.OK || resp.Repo != "acme/widgets" || resp.DefaultBranch != "main" || resp.RemoteURL != "https://fj.example/acme/widgets.git" {
		t.Fatalf("resp = %+v", resp)
	}
	assertExists(t, path, false)
	if filepath.Dir(resp.Bundle) != h.relay.cloneDir || !strings.HasSuffix(resp.Bundle, forgeCloneBundleSuffix) || !strings.HasPrefix(filepath.Base(resp.Bundle), "worker-1.") {
		t.Fatalf("bundle path %q", resp.Bundle)
	}
	fi, err := os.Lstat(resp.Bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bundle mode = %v, want a regular 0600 file", fi.Mode())
	}
	if got, _ := os.ReadFile(resp.Bundle); string(got) != fakeBundle {
		t.Fatalf("bundle = %q", got)
	}
	if len(h.chowned) != 1 || h.chowned[0] != os.Getuid() {
		t.Fatalf("bundle given to %v, want the request file's owner %d", h.chowned, os.Getuid())
	}
	if left := h.stagingDirs(t); len(left) != 0 {
		t.Fatalf("staging dirs left behind: %v", left)
	}

	// The fetch ran in the hive-owned staging dir, against the configured
	// URL, with the hive's own config and the hardening of the push relay.
	fetch := h.callsOf("fetch")[0]
	if !strings.HasPrefix(fetch.dir, h.tmp+string(os.PathSeparator)) {
		t.Fatalf("fetch ran in %q, want a staging dir under TMPDIR", fetch.dir)
	}
	env := strings.Join(fetch.env, "\n")
	for _, want := range []string{"GIT_DIR=" + fetch.dir, "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_ALLOW_PROTOCOL=https", "GIT_TERMINAL_PROMPT=0",
		"=http.https://fj.example/acme/widgets.git.extraHeader", "=core.hooksPath", "=credential.helper", "=http.sslVerify"} {
		if !strings.Contains(env, want) {
			t.Errorf("fetch env lacks %q:\n%s", want, env)
		}
	}
	if args := strings.Join(fetch.args, " "); !strings.Contains(args, "https://fj.example/acme/widgets.git") || !strings.Contains(args, "--no-recurse-submodules") {
		t.Errorf("fetch args = %q", args)
	}
}

// The token reaches only the hive's own fetch: never an argument, never the
// other git commands, never the hive process environment that agent panes
// inherit, and never any file left on disk, the bundle and result included.
func TestForgeCloneRelayTokenNeverReachesTheAgent(t *testing.T) {
	h := newCloneHarness(t)
	path := writeRelayRequest(t, h.relay.cloneDir, "worker-1.json", goodClone())
	h.relay.scanOnce(context.Background())
	token := h.mintedToken(t)
	if strings.Count(token, ".") != 2 {
		t.Fatalf("the fetch token %q is not a minted JWT", token)
	}
	if token == forgePushTestToken {
		t.Fatal("the fetch used the long-lived access token")
	}
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	secrets := []string{token, header}

	for _, c := range h.calls {
		for _, s := range secrets {
			if strings.Contains(strings.Join(c.args, " "), s) {
				t.Fatalf("token in argv of git %v", c.args)
			}
			if c.args[0] != "fetch" && strings.Contains(strings.Join(c.env, "\n"), s) {
				t.Fatalf("token in the env of git %s", c.args[0])
			}
		}
	}
	for _, e := range os.Environ() {
		for _, s := range secrets {
			if strings.Contains(e, s) {
				t.Fatalf("token in the hive process environment: %q", e)
			}
		}
	}
	// Everything the clone left on disk: the queue (bundle, result) and
	// TMPDIR (staging, removed).
	for _, root := range []string{h.relay.cloneDir, h.tmp} {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				t.Fatalf("read %s: %v", p, rerr)
			}
			for _, s := range secrets {
				if strings.Contains(string(b), s) {
					t.Fatalf("token on disk in %s", p)
				}
			}
			return nil
		})
	}
	var resp forgeCloneResponse
	readRelayResult(t, path, &resp)
	if !resp.OK {
		t.Fatalf("resp = %+v", resp)
	}
}

// git output that echoes the credential is scrubbed before it reaches the
// agent's result or the log.
func TestForgeCloneRelayScrubsTokenFromGitOutput(t *testing.T) {
	h := newCloneHarness(t)
	h.fetchErr = errors.New("exit status 128")
	path := writeRelayRequest(t, h.relay.cloneDir, "worker-1.json", goodClone())
	// The stub echoes whatever header the fetch carried.
	prev := forgeCloneExec
	forgeCloneExec = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		out, err := prev(ctx, dir, env, args...)
		if args[0] == "fetch" {
			for _, e := range env {
				if _, v, _ := strings.Cut(e, "="); strings.HasPrefix(v, "Authorization:") {
					raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "Authorization: Basic "))
					out = "fatal: " + v + " " + string(raw) + "\n"
				}
			}
		}
		return out, err
	}
	h.relay.scanOnce(context.Background())
	token := h.mintedToken(t)
	b, err := os.ReadFile(strings.TrimSuffix(path, ".json") + ".result.json")
	if err != nil {
		t.Fatal(err)
	}
	header := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	if strings.Contains(string(b), token) || strings.Contains(string(b), header) {
		t.Fatalf("result carries the credential: %s", b)
	}
	if !strings.Contains(string(b), forgeCloneRedacted) {
		t.Fatalf("result = %s, want the echoed credential redacted", b)
	}
	assertExists(t, path, true) // retried, not consumed
	if bundles, _ := filepath.Glob(filepath.Join(h.relay.cloneDir, "*"+forgeCloneBundleSuffix)); len(bundles) != 0 {
		t.Fatalf("a failed fetch left bundles: %v", bundles)
	}
}

// A failed mint fails closed: no git runs at all, so there is no anonymous
// or access-token fallback, and the request is retried and then quarantined.
func TestForgeCloneRelayFailedMintFailsClosed(t *testing.T) {
	h := newCloneHarness(t)
	if err := os.Remove(h.keyFile); err != nil {
		t.Fatal(err)
	}
	path := writeRelayRequest(t, h.relay.cloneDir, "worker-1.json", goodClone())
	for range forgeRelayMaxAttempts {
		h.relay.scanOnce(context.Background())
	}
	if len(h.calls) != 0 {
		t.Fatalf("ran git after a failed mint: %+v", h.calls)
	}
	var resp forgeCloneResponse
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "minting the clone token") || strings.Contains(resp.Error, h.keyFile) {
		t.Fatalf("resp = %+v, want a mint failure that does not name the key path", resp)
	}
	assertExists(t, path+".failed", true)
	if len(h.chowned) != 0 {
		t.Fatal("a bundle was handed out after a failed mint")
	}

	// A minter that returns an empty token is a failed mint too.
	h2 := newCloneHarness(t)
	h2.relay.pushRemote = func(string) (pushbroker.ForgeRemote, bool, error) {
		return pushbroker.ForgeRemote{BaseURL: "https://fj.example", Minter: pushbroker.StaticTokenMinter{Token: func() (string, error) { return " ", nil }}}, true, nil
	}
	path = writeRelayRequest(t, h2.relay.cloneDir, "worker-2.json", goodClone())
	h2.relay.scanOnce(context.Background())
	if len(h2.calls) != 0 {
		t.Fatalf("ran git with an empty token: %+v", h2.calls)
	}
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "minting the clone token") {
		t.Fatalf("empty token resp = %+v", resp)
	}
}

// Only the agent's own, configured repos are cloned, only by the agent the
// request file belongs to, and only with forgejo.integration.
func TestForgeCloneRelayRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(*cloneHarness, *forgeCloneRequest)
		suffix string
		errHas string
	}{
		"wrong agent":    {func(_ *cloneHarness, r *forgeCloneRequest) { r.Agent = "intruder" }, ".denied", "authorization denied"},
		"nil authz":      {func(h *cloneHarness, _ *forgeCloneRequest) { h.relay.cloneAuthz = nil }, ".denied", "fail closed"},
		"unmanaged repo": {func(_ *cloneHarness, r *forgeCloneRequest) { r.Repo = "other" }, ".rejected", "not managed"},
		"other org":      {func(_ *cloneHarness, r *forgeCloneRequest) { r.Repo = "evil/widgets" }, ".rejected", "not managed"},
		"path in repo":   {func(_ *cloneHarness, r *forgeCloneRequest) { r.Repo = "acme/../widgets" }, ".rejected", "not managed"},
		"out of scope": {func(h *cloneHarness, _ *forgeCloneRequest) {
			h.cfg.Project.Repos = []string{"widgets", "gadgets"}
			h.cfg.Agents = map[string]config.AgentConfig{"worker": {Repos: []string{"gadgets"}}}
		}, ".rejected", "clone rejected"},
		"paused repo": {func(h *cloneHarness, _ *forgeCloneRequest) {
			h.cfg.Project.PausedRepos = []config.RepoPause{{Repo: "widgets"}}
		}, ".rejected", "clone rejected"},
		"no integration": {func(h *cloneHarness, _ *forgeCloneRequest) { h.cfg.Forgejo.Integration = nil }, ".rejected", "needs forgejo.integration"},
		"gitea repo": {func(h *cloneHarness, _ *forgeCloneRequest) {
			h.cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "widgets", Forge: config.ForgeGitea}}
		}, ".rejected", "forgejo repos only"},
		"no resolver": {func(h *cloneHarness, _ *forgeCloneRequest) { h.relay.pushRemote = nil }, ".rejected", "fail closed"},
		"resolver says github": {func(h *cloneHarness, _ *forgeCloneRequest) {
			h.relay.pushRemote = func(string) (pushbroker.ForgeRemote, bool, error) { return pushbroker.ForgeRemote{}, false, nil }
		}, ".rejected", "no forge remote"},
		"empty repo": {func(h *cloneHarness, _ *forgeCloneRequest) { h.verifyErr = errors.New("exit status 1") }, ".rejected", "no commit on its default branch"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newCloneHarness(t)
			req := goodClone()
			tc.mutate(h, &req)
			path := writeRelayRequest(t, h.relay.cloneDir, "a.json", req)
			h.relay.scanOnce(context.Background())

			if name != "empty repo" && len(h.callsOf("fetch")) != 0 {
				t.Fatalf("fetched on a refused request: %+v", h.calls)
			}
			if len(h.chowned) != 0 {
				t.Fatal("a bundle was handed out on a refused request")
			}
			var resp forgeCloneResponse
			readRelayResult(t, path, &resp)
			if resp.OK || resp.Bundle != "" || !strings.Contains(resp.Error, tc.errHas) {
				t.Errorf("resp = %+v, want an error containing %q", resp, tc.errHas)
			}
			assertExists(t, path+tc.suffix, true)
			if left := h.stagingDirs(t); len(left) != 0 {
				t.Fatalf("staging dirs left behind: %v", left)
			}
		})
	}
}

// The request file must belong to an agent UID: root-owned (uid 0) or
// unreadable (-1) files are denied before the authorizer is asked, so the
// shared-UID fallback of the write relays does not apply to clones.
func TestForgeCloneRelayNeedsAnAgentOwnedRequest(t *testing.T) {
	for _, owner := range []int{0, -1} {
		h := newCloneHarness(t)
		asked := false
		h.relay.withCloneRelay(func(string, int) (agentmode.TokenTier, error) {
			asked = true
			return agentmode.TokenTierContributor, nil
		})
		prev := forgeCloneFileOwnerUID
		forgeCloneFileOwnerUID = func(string) int { return owner }
		path := writeRelayRequest(t, h.relay.cloneDir, "a.json", goodClone())
		h.relay.scanOnce(context.Background())
		forgeCloneFileOwnerUID = prev
		if asked || len(h.calls) != 0 {
			t.Fatalf("owner %d: authorizer asked %v, git calls %+v", owner, asked, h.calls)
		}
		var resp forgeCloneResponse
		readRelayResult(t, path, &resp)
		if resp.OK || !strings.Contains(resp.Error, "agent's own UID") {
			t.Fatalf("owner %d: resp %+v", owner, resp)
		}
		assertExists(t, path+".denied", true)
	}
}

// Transient failures are retried; a bundle over the cap is not handed out.
func TestForgeCloneRelayRetriesAndCaps(t *testing.T) {
	h := newCloneHarness(t)
	h.defErr = errors.New("forge unavailable")
	path := writeRelayRequest(t, h.relay.cloneDir, "a.json", goodClone())
	h.relay.scanOnce(context.Background())
	var resp forgeCloneResponse
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "default branch") || len(h.calls) != 0 {
		t.Fatalf("default-branch failure: resp %+v calls %+v", resp, h.calls)
	}
	assertExists(t, path, true)

	h.defErr = nil
	t.Setenv(forgeCloneMaxBundleBytesEnv, "8")
	h.relay.scanOnce(context.Background())
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "limit") {
		t.Fatalf("oversized bundle: resp %+v", resp)
	}
	if bundles, _ := filepath.Glob(filepath.Join(h.relay.cloneDir, "*"+forgeCloneBundleSuffix)); len(bundles) != 0 || len(h.chowned) != 0 {
		t.Fatalf("an oversized bundle was handed out: %v %v", bundles, h.chowned)
	}

	// A chown that fails leaves no bundle behind.
	t.Setenv(forgeCloneMaxBundleBytesEnv, "")
	forgeCloneChown = func(*os.File, int) error { return errors.New("operation not permitted") }
	h.relay.scanOnce(context.Background())
	readRelayResult(t, path, &resp)
	if resp.OK || !strings.Contains(resp.Error, "giving the bundle") {
		t.Fatalf("chown failure: resp %+v", resp)
	}
	if bundles, _ := filepath.Glob(filepath.Join(h.relay.cloneDir, "*"+forgeCloneBundleSuffix)); len(bundles) != 0 {
		t.Fatalf("a bundle the agent does not own was left: %v", bundles)
	}
}

func TestForgeCloneMaxBundleBytes(t *testing.T) {
	for v, want := range map[string]int64{"": forgeCloneDefaultMaxBundleBytes, "0": forgeCloneDefaultMaxBundleBytes, "-5": forgeCloneDefaultMaxBundleBytes, "junk": forgeCloneDefaultMaxBundleBytes, " 1024 ": 1024} {
		t.Setenv(forgeCloneMaxBundleBytesEnv, v)
		if got := forgeCloneMaxBundleBytes(); got != want {
			t.Errorf("%q: %d, want %d", v, got, want)
		}
	}
}

// One scan serves at most forgeCloneMaxPerScan clones, and the queue is
// idle on a hive with a GitHub repo.
func TestForgeCloneRelayBoundedPerScanAndForgeOnly(t *testing.T) {
	h := newCloneHarness(t)
	h.cfg.Project.RepoPolicies = []config.RepoPolicy{{Repo: "gh-repo", Forge: config.ForgeGitHub}}
	first := writeRelayRequest(t, h.relay.cloneDir, "a.json", goodClone())
	h.relay.scanOnce(context.Background())
	if len(h.calls) != 0 {
		t.Fatalf("served a clone on a hive with a GitHub repo: %+v", h.calls)
	}
	assertExists(t, first, true)

	h.cfg.Project.RepoPolicies = nil
	for _, n := range []string{"b.json", "c.json"} {
		writeRelayRequest(t, h.relay.cloneDir, n, goodClone())
	}
	h.relay.scanOnce(context.Background())
	if got := len(h.callsOf("fetch")); got != forgeCloneMaxPerScan {
		t.Fatalf("fetches in one scan = %d, want %d", got, forgeCloneMaxPerScan)
	}
}

// Bundles the agent leaves behind are removed after the TTL; fresh ones,
// other files and symlinks are left alone.
func TestForgeCloneRelayExpiresOldBundles(t *testing.T) {
	h := newCloneHarness(t)
	dir := h.relay.cloneDir
	old := filepath.Join(dir, "a.1"+forgeCloneBundleSuffix)
	fresh := filepath.Join(dir, "b.2"+forgeCloneBundleSuffix)
	other := filepath.Join(dir, "notes.txt")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "c.3"+forgeCloneBundleSuffix)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * forgeCloneBundleTTL)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(target, past, past); err != nil {
		t.Fatal(err)
	}
	h.relay.expireCloneBundles()
	assertExists(t, old, false)
	assertExists(t, fresh, true)
	assertExists(t, other, true)
	assertExists(t, target, true)
}

func TestForgeCloneBaseEnvDropsTraceAndConfig(t *testing.T) {
	env := forgeCloneBaseEnv([]string{
		"PATH=/usr/bin", "HOME=/root",
		"GIT_TRACE=1", "GIT_TRACE_CURL=1", "GIT_TRACE_PACKET=1", "GIT_CURL_VERBOSE=1", "GIT_REDACT_COOKIES=",
		"GIT_CONFIG_PARAMETERS='http.extraHeader=x'", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_GLOBAL=/tmp/evil",
		"GIT_DIR=/tmp/x", "GIT_OBJECT_DIRECTORY=/tmp/o", "GIT_ALTERNATE_OBJECT_DIRECTORIES=/tmp/a", "GIT_WORK_TREE=/tmp/w", "GIT_SSL_NO_VERIFY=1",
		"FORGEJO_TOKEN=" + forgePushTestToken,
	})
	if got := strings.Join(env, " "); got != "PATH=/usr/bin HOME=/root" {
		t.Fatalf("base env = %q", got)
	}
}

func TestForgeCloneScrubber(t *testing.T) {
	env := []string{"GIT_CONFIG_KEY_0=http.x.extraHeader", "GIT_CONFIG_VALUE_0=Authorization: Basic c2VjcmV0", "GIT_CONFIG_VALUE_1=false"}
	scrub := forgeCloneScrubber("tok-123", env)
	got := scrub("a tok-123 b Authorization: Basic c2VjcmV0 c c2VjcmV0 false")
	if strings.Contains(got, "tok-123") || strings.Contains(got, "c2VjcmV0") || !strings.Contains(got, "false") {
		t.Fatalf("scrubbed = %q", got)
	}
}

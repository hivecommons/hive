package cli

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/checkout"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/serve"
)

// checkoutFixtureRepo is a bare repo with one commit, as a file:// URL,
// plus the allowlist prefix that admits it.
func checkoutFixtureRepo(t *testing.T) (repo, sha, allow string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(work, "f"), []byte("f"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "f")
	git("commit", "-q", "-m", "c")
	sha = git("rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "fixture.git")
	git("clone", "-q", "--bare", work, bare)
	return "file://" + bare, sha, "file://" + filepath.Dir(bare)
}

func TestCheckoutLocalAddListRm(t *testing.T) {
	repo, sha, allow := checkoutFixtureRepo(t)
	t.Setenv(config.CheckoutAllowlistEnvVar, "")
	t.Setenv(config.CheckoutDirEnvVar, "")
	h := newHarness(t)
	dir := filepath.Join(t.TempDir(), "chk")

	if code := h.run("checkout", "add", "-allow", allow, "-dir", dir, "-repo", repo, "-commit", sha, "-ttl", "1h"); code != ExitOK {
		t.Fatalf("add: %d %s", code, h.stderr)
	}
	fields := strings.Fields(h.stdout.String())
	if len(fields) != 2 || !strings.HasPrefix(fields[0], checkout.IDPrefix) || !strings.HasPrefix(fields[1], dir) {
		t.Fatalf("add output = %q", h.stdout.String())
	}
	id := fields[0]
	if _, err := os.Stat(filepath.Join(fields[1], "f")); err != nil {
		t.Fatalf("pinned file: %v", err)
	}

	// Without an allowlist the local service still lists and removes.
	h.stdout.Reset()
	if code := h.run("checkout", "list", "-dir", dir); code != ExitOK {
		t.Fatalf("list: %d %s", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "ID") || !strings.Contains(out, id) || !strings.Contains(out, serve.DefaultOperatorName) || !strings.Contains(out, sha) {
		t.Fatalf("list output = %q", out)
	}
	h.stdout.Reset()
	if code := h.run("checkout", "list", "-dir", dir, "-owner", "nobody", "-json"); code != ExitOK || strings.TrimSpace(h.stdout.String()) != "[]" {
		t.Fatalf("list nobody: %d %q", code, h.stdout.String())
	}
	h.stdout.Reset()
	if code := h.run("checkout", "add", "-dir", dir, "-repo", repo, "-commit", sha); code != ExitUsage || !strings.Contains(h.stderr.String(), "allowlist is empty") {
		t.Fatalf("add without allowlist: %d %s", code, h.stderr)
	}
	h.stderr.Reset()
	if code := h.run("checkout", "add", "-allow", allow, "-dir", dir, "-repo", repo, "-commit", "main", "-json"); code != ExitUsage || !strings.Contains(h.stderr.String(), "40-hex") {
		t.Fatalf("add branch: %d %s", code, h.stderr)
	}
	h.stderr.Reset()
	if code := h.run("checkout", "rm", "-dir", dir, id); code != ExitOK {
		t.Fatalf("rm: %d %s", code, h.stderr)
	}
	if code := h.run("checkout", "rm", "-dir", dir, id); code != ExitError || !strings.Contains(h.stderr.String(), "not found") {
		t.Fatalf("rm again: %d %s", code, h.stderr)
	}

	// The allowlist and dir also come from the environment, and the dir
	// defaults under the work dir.
	t.Setenv(config.CheckoutAllowlistEnvVar, allow)
	work := t.TempDir()
	h.stdout.Reset()
	if code := h.env.Run(t.Context(), []string{"--workdir", work, "checkout", "add", "-repo", repo, "-commit", sha, "-no-branches", "-json"}); code != ExitOK {
		t.Fatalf("add via env: %d %s", code, h.stderr)
	}
	var rec api.Checkout
	if err := json.Unmarshal(h.stdout.Bytes(), &rec); err != nil || !strings.HasPrefix(rec.Path, checkout.DefaultDir(work)) || rec.Branches {
		t.Fatalf("add via env = %s %v", h.stdout.String(), err)
	}
	if got := checkoutDirFor("", work); got != checkout.DefaultDir(work) {
		t.Fatalf("checkoutDirFor = %q", got)
	}
	t.Setenv(config.CheckoutDirEnvVar, "/elsewhere")
	if got := checkoutDirFor("", work); got != "/elsewhere" {
		t.Fatalf("checkoutDirFor from env = %q", got)
	}
	if got := checkoutDirFor("/flag", work); got != "/flag" {
		t.Fatalf("checkoutDirFor from flag = %q", got)
	}
}

func TestCheckoutUsageErrors(t *testing.T) {
	t.Setenv(config.CheckoutAllowlistEnvVar, "")
	h := newHarness(t)
	cases := [][]string{
		{"checkout"},
		{"checkout", "bogus"},
		{"checkout", "add"},
		{"checkout", "add", "-repo", "x"},
		{"checkout", "add", "-repo", "x", "-commit", "y", "extra"},
		{"checkout", "rm"},
		{"checkout", "rm", "a", "b"},
		{"checkout", "list", "extra"},
		{"checkout", "list", "-nope"},
	}
	for _, args := range cases {
		h.stderr.Reset()
		if code := h.run(args...); code != ExitUsage {
			t.Errorf("%v: code %d, want usage; stderr %s", args, code, h.stderr)
		}
	}
	h.stderr.Reset()
	if code := h.run("checkout", "list", "-h"); code != ExitOK {
		t.Errorf("list -h: %d", code)
	}
	if code := h.run("checkout", "add", "-allow", "ssh://bad", "-repo", "https://x/y", "-commit", strings.Repeat("a", 40)); code != ExitUsage || !strings.Contains(h.stderr.String(), "crux: checkout:") {
		t.Errorf("bad allowlist: %d %s", code, h.stderr)
	}
	// The ceiling comes from the flag, then the environment, and must parse.
	t.Setenv(config.CheckoutMaxBytesEnvVar, "lots")
	h.stderr.Reset()
	if code := h.run("checkout", "list", "-allow", "file:///", "-dir", t.TempDir()); code != ExitUsage || !strings.Contains(h.stderr.String(), config.CheckoutMaxBytesEnvVar) {
		t.Errorf("bad ceiling env: %d %s", code, h.stderr)
	}
	if code := h.run("checkout", "list", "-allow", "file:///", "-dir", t.TempDir(), "-max-bytes-ceiling", "1024"); code != ExitOK {
		t.Errorf("ceiling flag overrides a bad env: %d %s", code, h.stderr)
	}
	t.Setenv(config.CheckoutMaxBytesEnvVar, "2048")
	if n, err := checkoutMaxBytesFromEnv(); err != nil || n != 2048 {
		t.Errorf("ceiling env = %d, %v", n, err)
	}
	t.Setenv(config.CheckoutMaxBytesEnvVar, "-5")
	if _, err := checkoutMaxBytesFromEnv(); err == nil {
		t.Error("negative ceiling env accepted")
	}
	// The control socket does not forward checkout.
	e := &Env{Stdout: h.stdout, Stderr: h.stderr}
	h.stderr.Reset()
	if code := e.dispatch(t.Context(), globalOptions{}, "checkout", []string{"list"}, true); code != ExitUsage || !strings.Contains(h.stderr.String(), "control socket") {
		t.Errorf("remote dispatch: %d %s", code, h.stderr)
	}
}

func TestCheckoutRemote(t *testing.T) {
	repo, sha, allow := checkoutFixtureRepo(t)
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	svc, err := checkout.New(checkout.Config{Dir: filepath.Join(t.TempDir(), "chk"), AllowedRepos: []string{allow}})
	if err != nil {
		t.Fatal(err)
	}
	s := serve.New(newFakeRuntime())
	s.Token, s.Checkouts = "tok", svc
	path := filepath.Join(shortSocketDir(t), "api.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)

	h := newHarness(t)
	remote := func(args ...string) int {
		return h.run(append([]string{"--server", path, "--token", "tok", "checkout"}, args...)...)
	}
	if code := remote(); code != ExitUsage {
		t.Fatalf("no verb: %d", code)
	}
	if code := remote("add", "-repo", repo, "-commit", sha, "-owner", "atlas"); code != ExitOK {
		t.Fatalf("add: %d %s", code, h.stderr)
	}
	id := strings.Fields(h.stdout.String())[0]
	h.stdout.Reset()
	if code := remote("list", "-owner", "atlas"); code != ExitOK || !strings.Contains(h.stdout.String(), id) {
		t.Fatalf("list: %d %q %s", code, h.stdout.String(), h.stderr)
	}
	h.stderr.Reset()
	if code := remote("add", "-repo", "https://github.com/o/r", "-commit", sha); code != ExitUsage || !strings.Contains(h.stderr.String(), "allowed prefixes") {
		t.Fatalf("off allowlist: %d %s", code, h.stderr)
	}
	if code := remote("rm", id); code != ExitOK {
		t.Fatalf("rm: %d %s", code, h.stderr)
	}
	h.stderr.Reset()
	if code := remote("rm", id); code != ExitError || !strings.Contains(h.stderr.String(), "not_found") && !strings.Contains(h.stderr.String(), "not found") {
		t.Fatalf("rm again: %d %s", code, h.stderr)
	}
	h.stderr.Reset()
	if code := h.run("--server", path, "--token", "wrong", "checkout", "list"); code != ExitError || !strings.Contains(h.stderr.String(), "bearer token") {
		t.Fatalf("wrong token: %d %s", code, h.stderr)
	}
}

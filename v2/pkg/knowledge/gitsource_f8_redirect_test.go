package knowledge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// AUDIT F8 (residual): redirect SSRF after validation.
//
// GitSource validates the configured URL (scheme, host allow-list, and since the
// DNS-pinning fix, the resolved address too). git then follows HTTP 3xx by
// DEFAULT, so a public, fully-validated remote can answer the first request with
// `302 Location: http://169.254.169.254/…` — or any in-cluster Service — and git
// re-issues the request there with every check already behind it. Validating a
// URL is worthless if the fetch is allowed to end up somewhere else.
//
// TestF8GitCloneRefusesRedirect / TestF8GitPullRefusesRedirect are the
// regressions, driven against a real local HTTP server that redirects.
// TestF8GitCloneStillWorksWithoutRedirect is the POSITIVE CONTROL: `git -c
// http.followRedirects=false` must still clone a normal, non-redirecting remote,
// or "block all git" would pass the regressions.

func f8RequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// f8RedirectServer models the F8 scenario with TWO servers.
//
// CRITICAL TEST-DESIGN NOTE. The obvious version of this test redirects at
// 169.254.169.254 and asserts the clone fails. That test passes WITH OR WITHOUT
// the fix — the clone fails either way, because link-local is simply
// unroutable. It proves nothing. (Verified: with the fix neutered it still went
// green.)
//
// So the "internal" target here is a REACHABLE local server that counts its
// requests. The assertion is therefore not "did the clone fail" but "was the
// internal target contacted AT ALL" — which is exactly the SSRF property, and
// which flips the moment redirects are followed.
//
// front = the validated, allow-listed remote the operator configured.
// internal = the address it tries to pivot the fetch to.
func f8RedirectServer(t *testing.T) (remoteURL string, frontHits, internalHits *int) {
	t.Helper()
	nFront, nInternal := 0, 0

	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nInternal++
		// Answer plausibly so that, if git DOES follow, it keeps going and the
		// hit is unambiguous rather than an instant transport error.
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(internal.Close)

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nFront++
		http.Redirect(w, r, internal.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(front.Close)

	return front.URL + "/repo.git", &nFront, &nInternal
}

// f8LocalOriginRepo builds a real git repo on disk and serves it over HTTP via
// git's dumb protocol, giving the positive control a remote that actually
// clones without any redirect involved.
func f8LocalOriginRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "origin")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "init")
	// Dumb HTTP needs the server info + a bare-visible layout; simplest reliable
	// remote for a unit test is the local path itself, which exercises the same
	// argument plumbing. The redirect cases below are the ones that need HTTP.
	return repo
}

// TestF8GitCloneRefusesRedirect: a clone against a remote that 302s must FAIL,
// and must not have issued a request to the redirect target.
func TestF8GitCloneRefusesRedirect(t *testing.T) {
	f8RequireGit(t)
	remote, frontHits, internalHits := f8RedirectServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dest := filepath.Join(t.TempDir(), "clone")
	args := gitCmdArgs("clone", "--depth", "1", "--", remote, dest)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, _ := cmd.CombinedOutput()

	// Guard against the test passing for the wrong reason: the front server must
	// actually have been reached, or nothing about redirects was exercised.
	if *frontHits == 0 {
		t.Fatalf("test never reached the validated remote — no redirect was "+
			"exercised. output: %s", out)
	}
	// THE assertion. Following the redirect means issuing a request to the
	// internal target; suppressing it means never touching that host.
	if *internalHits != 0 {
		t.Errorf("git issued %d request(s) to the redirect target after cloning a "+
			"validated URL — redirect SSRF is live (F8). output: %s", *internalHits, out)
	}
	if _, statErr := os.Stat(filepath.Join(dest, ".git")); statErr == nil {
		t.Error("clone dir was populated despite the redirect")
	}
}

// TestF8GitPullRefusesRedirect: same for the sync path. A repo whose origin is
// a redirecting remote must not follow it on pull.
func TestF8GitPullRefusesRedirect(t *testing.T) {
	f8RequireGit(t)
	remote, frontHits, internalHits := f8RedirectServer(t)

	// gitSourceEnv sets GIT_ALLOW_PROTOCOL=https unless the private opt-in is on,
	// and httptest only speaks plain HTTP. Without this the pull fails on the
	// PROTOCOL filter and never reaches the redirect at all — a green test that
	// proves nothing. (The *hits assertion below is what surfaced this.) The
	// opt-in is exactly the configuration in which F8 still bites: an operator
	// who has allowed private/HTTP sources still must not be redirect-pivoted.
	t.Setenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE", "true")

	// A local repo pointed at the hostile remote — the state after a source has
	// been cloned once and is now being refreshed.
	repo := t.TempDir()
	//
	// NOTE the branch/upstream wiring: without it `git pull` fails LOCALLY
	// ("no tracking information") before it ever opens a socket, and the test
	// would pass whether or not redirects are suppressed. The *hits assertion
	// below is what catches that — it caught exactly this while writing the test.
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"remote", "add", "origin", remote},
		{"config", "branch.main.remote", "origin"},
		{"config", "branch.main.merge", "refs/heads/main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("setup git %v: %v: %s", args, err, out)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The pull is expected to fail; what matters is WHERE it did not go.
	_ = gitSourcePull(ctx, repo)

	if *frontHits == 0 {
		t.Fatal("test never reached the validated remote — no redirect was exercised")
	}
	if *internalHits != 0 {
		t.Errorf("git pull issued %d request(s) to the redirect target — redirect "+
			"SSRF is live on the sync path (F8)", *internalHits)
	}
}

// TestF8GitCloneStillWorksWithoutRedirect is the POSITIVE CONTROL. With redirect
// suppression on, an ordinary remote must still clone. Without this, a change
// that simply broke every git invocation would pass both regressions above.
func TestF8GitCloneStillWorksWithoutRedirect(t *testing.T) {
	f8RequireGit(t)
	origin := f8LocalOriginRepo(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dest := filepath.Join(t.TempDir(), "clone")
	args := gitCmdArgs("clone", "--depth", "1", "--branch", "main", "--", origin, dest)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone of a normal remote must still succeed with redirect "+
			"suppression on; got %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Errorf("cloned repo missing content: %v", err)
	}

	// And the positive control extends to pull: a fast-forward from the same
	// non-redirecting origin still works.
	if err := gitPull(ctx, dest); err != nil {
		t.Errorf("pull from a normal remote must still succeed; got %v", err)
	}
}

// TestF8RedirectArgsArePrefixed pins the ARGUMENT SHAPE. `-c` overrides are only
// honoured before the subcommand; if a refactor ever appends them after
// "clone", git treats them as a path and the protection silently evaporates
// with no test failure anywhere else.
func TestF8RedirectArgsArePrefixed(t *testing.T) {
	got := gitCmdArgs("clone", "--depth", "1")
	want := []string{"-c", "http.followRedirects=false", "clone", "--depth", "1"}
	if len(got) != len(want) {
		t.Fatalf("gitCmdArgs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("gitCmdArgs = %v, want %v", got, want)
		}
	}
	// gitCmdArgs must not alias or mutate a shared backing array — two calls
	// must be independent.
	a := gitCmdArgs("pull")
	b := gitCmdArgs("fetch")
	if a[len(a)-1] != "pull" || b[len(b)-1] != "fetch" {
		t.Errorf("gitCmdArgs calls interfere: a=%v b=%v", a, b)
	}
}

package github

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commonGitDir makes a directory shaped like a checkout's common git dir.
func commonGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPushBranchIsolatedGitDir_WritesHiveOwnedRepo(t *testing.T) {
	common := commonGitDir(t)
	if err := os.WriteFile(filepath.Join(common, "shallow"), []byte("abcd1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	dir, env, cleanup, err := pushBranchIsolatedGitDir(common+"\nsha1\n", "scanner/fix-1", sha, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(dir, common) {
		t.Fatalf("isolated GIT_DIR %q is inside the agent's git dir", dir)
	}
	for name, want := range map[string]string{
		"HEAD":                     "ref: refs/heads/scanner/fix-1\n",
		"refs/heads/scanner/fix-1": sha + "\n",
		"shallow":                  "abcd1234\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config")); strings.Contains(string(cfg), "objectformat") {
		t.Errorf("sha1 config names an object format: %q", cfg)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{"GIT_DIR=" + dir, "GIT_OBJECT_DIRECTORY=" + filepath.Join(common, "objects")} {
		if !strings.Contains(joined, want) {
			t.Errorf("env lacks %q: %v", want, env)
		}
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("cleanup left %s behind: %v", dir, err)
	}

	dir, _, cleanup, err = pushBranchIsolatedGitDir(common+"\nsha256\n", "b", strings.Repeat("ab", 32), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if cfg, _ := os.ReadFile(filepath.Join(dir, "config")); !strings.Contains(string(cfg), "objectformat = sha256") || !strings.Contains(string(cfg), "repositoryformatversion = 1") {
		t.Errorf("sha256 config = %q", cfg)
	}
}

func TestPushBranchIsolatedGitDir_Refusals(t *testing.T) {
	common := commonGitDir(t)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	noObjects := t.TempDir()
	linked := t.TempDir()
	if err := os.Symlink(filepath.Join(common, "objects"), filepath.Join(linked, "objects")); err != nil {
		t.Fatal(err)
	}
	shallowLink := commonGitDir(t)
	if err := os.Symlink("/etc/passwd", filepath.Join(shallowLink, "shallow")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		info, sha string
		uid       int
	}{
		"one line":         {common + "\n", sha, 0},
		"relative dir":     {"rel/.git\nsha1\n", sha, 0},
		"unknown format":   {common + "\nmd5\n", sha, 0},
		"sha with newline": {common + "\nsha1\n", "abcd\nref: x", 0},
		"sha with path":    {common + "\nsha1\n", "../../x", 0},
		"no objects":       {noObjects + "\nsha1\n", sha, 0},
		"objects symlink":  {linked + "\nsha1\n", sha, 0},
		"shallow symlink":  {shallowLink + "\nsha1\n", sha, 0},
		"other owner":      {common + "\nsha1\n", sha, os.Getuid() + 1},
	}
	for name, tc := range cases {
		if name == "other owner" && os.Getuid() <= 0 {
			continue
		}
		if _, _, cleanup, err := pushBranchIsolatedGitDir(tc.info, "b", tc.sha, tc.uid); err == nil {
			cleanup()
			t.Errorf("%s: accepted", name)
		}
	}
}

// Against a real git: a push from the isolated GIT_DIR reaches the remote the
// relay named even though the checkout's own config rewrites that URL,
// renames a remote after it and is shallow, and the pushed commit is the one
// the relay resolved.
func TestPushBranchIsolatedGitDir_RealGitPushIgnoresCheckoutConfig(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_AUTHOR_NAME=scanner", "GIT_AUTHOR_EMAIL=scanner@example.invalid",
			"GIT_COMMITTER_NAME=scanner", "GIT_COMMITTER_EMAIL=scanner@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	seed := gitRepo(t, "main")
	run(seed, "commit", "-q", "--allow-empty", "-m", "second")
	run(base, "clone", "-q", "--bare", seed, remote)
	remoteURL := "file://" + remote
	checkout := filepath.Join(base, "checkout")
	run(base, "clone", "-q", "--depth", "1", remoteURL, checkout)
	run(checkout, "checkout", "-q", "-b", "scanner/fix-9")
	if err := os.WriteFile(filepath.Join(checkout, "f"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(checkout, "add", "f")
	run(checkout, "commit", "-q", "-m", "change")
	sha := run(checkout, "rev-parse", "HEAD")
	// Hostile checkout config: each would send the push elsewhere.
	nowhere := filepath.Join(base, "nowhere.git")
	run(base, "clone", "-q", "--bare", seed, nowhere)
	elsewhere := "file://" + nowhere
	setLocalConfig(t, checkout, "url."+elsewhere+".pushInsteadOf", remoteURL)
	setLocalConfig(t, checkout, "remote."+remoteURL+".pushurl", elsewhere)
	setLocalConfig(t, checkout, "core.hooksPath", filepath.Join(base, "hooks"))

	info, err := runPushBranchCommand(context.Background(), checkout, pushBranchGitEnv(checkout, remoteURL, ""), "git", pushBranchRepoInfoArgs...)
	if err != nil {
		t.Fatalf("repo info: %v\n%s", err, info)
	}
	gitDir, isolation, cleanup, err := pushBranchIsolatedGitDir(info, "scanner/fix-9", sha, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	env := append(pushBranchGitEnv(gitDir, remoteURL, ""), isolation...)
	if out, err := runPushBranchCommand(context.Background(), gitDir, env, "git", "push", remoteURL, "refs/heads/scanner/fix-9:refs/heads/scanner/fix-9"); err != nil {
		t.Fatalf("isolated push failed: %v\n%s", err, out)
	}
	if got := run(remote, "rev-parse", "refs/heads/scanner/fix-9"); got != sha {
		t.Errorf("remote branch = %s, want the resolved %s", got, sha)
	}
	probe := exec.Command("git", "rev-parse", "--verify", "-q", "refs/heads/scanner/fix-9")
	probe.Dir = nowhere
	if out, err := probe.CombinedOutput(); err == nil {
		t.Errorf("the checkout's rewrite was followed: %s has the branch at %s", nowhere, out)
	}
}

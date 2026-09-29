package releasesentinel

// These tests drive GitRetagger against real git repositories in a temp dir:
// a bare "remote" plus a work clone that shapes its history. That is the only
// honest way to check the invariants that matter here - the tag moves only
// on a fast-forward, only to a commit on the release branch, only while the
// remote still has the expected old value, and no branch is ever pushed.

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/pushbroker"
)

const (
	retagTestTag    = "v1.0.0"
	retagTestBranch = "main"
	// retagTestToken is distinctive so a leak into argv is detectable.
	retagTestToken = "s3cr3t-retag-token-value"
)

type fakeMinter struct {
	token string
	err   error
	repos []string
}

func (m *fakeMinter) MintPushToken(_ context.Context, repo string) (string, error) {
	m.repos = append(m.repos, repo)
	return m.token, m.err
}

// recordingRunner runs real git and records every invocation. before, when
// set, runs ahead of a matching command (to simulate a concurrent writer).
type recordingRunner struct {
	calls  [][]string
	before func(args []string)
	fail   func(args []string) (out []byte, handled bool, err error)
}

func (r *recordingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.before != nil {
		r.before(args)
	}
	if r.fail != nil {
		if out, handled, err := r.fail(args); handled {
			return out, err
		}
	}
	return pushbroker.ExecRunner{}.Run(ctx, dir, env, name, args...)
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func (r *recordingRunner) pushes() [][]string {
	var out [][]string
	for _, c := range r.calls {
		if hasArg(c, "push") {
			out = append(out, c)
		}
	}
	return out
}

type gitFixture struct {
	t      *testing.T
	remote string // bare repo path
	work   string
	env    []string
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	f := &gitFixture{
		t:      t,
		remote: filepath.Join(root, "remote.git"),
		work:   filepath.Join(root, "work"),
		env: append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "HOME="+root,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test"),
	}
	f.git(root, "init", "--quiet", "--bare", f.remote)
	f.git(root, "init", "--quiet", f.work)
	f.git(f.work, "symbolic-ref", "HEAD", "refs/heads/"+retagTestBranch)
	f.git(f.work, "remote", "add", "origin", f.remote)
	return f
}

func (f *gitFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *gitFixture) commit(msg string) string {
	f.git(f.work, "commit", "--quiet", "--allow-empty", "-m", msg)
	return f.git(f.work, "rev-parse", "HEAD")
}

func (f *gitFixture) push(refspecs ...string) {
	f.git(f.work, append([]string{"push", "--quiet", "--force", "origin"}, refspecs...)...)
}

func (f *gitFixture) remoteRef(ref string) string {
	return f.git(f.remote, "rev-parse", ref)
}

func (f *gitFixture) retagger(runner pushbroker.CommandRunner) *GitRetagger {
	return &GitRetagger{
		RemoteURL: "file://" + f.remote,
		Dir:       filepath.Join(f.t.TempDir(), "state", "release-sentinel-git"),
		Minter:    &fakeMinter{token: retagTestToken},
		Runner:    runner,
	}
}

// release cuts base -> release (tagged v1.0.0) on main and pushes both.
func (f *gitFixture) release() (base, rel string) {
	base = f.commit("base")
	rel = f.commit("release")
	f.git(f.work, "tag", retagTestTag, rel)
	f.push("HEAD:refs/heads/"+retagTestBranch, "refs/tags/"+retagTestTag)
	return base, rel
}

func retagReq(oldSHA, newSHA string, fix ...string) RetagRequest {
	return RetagRequest{Repo: "acme/widgets", Tag: retagTestTag, Branch: retagTestBranch, OldSHA: oldSHA, NewSHA: newSHA, FixCommits: fix}
}

func TestGitRetagger_MovesOnlyTheTagWithOneAtomicLeasedPush(t *testing.T) {
	f := newGitFixture(t)
	_, rel := f.release()
	fix := f.commit("fix")
	f.push("HEAD:refs/heads/" + retagTestBranch)

	runner := &recordingRunner{}
	g := f.retagger(runner)
	if err := g.Retag(context.Background(), retagReq(rel, fix, fix)); err != nil {
		t.Fatalf("Retag: %v", err)
	}
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != fix {
		t.Fatalf("remote tag = %s, want the fix %s", got, fix)
	}
	if got := f.remoteRef("refs/heads/" + retagTestBranch); got != fix {
		t.Fatalf("release branch moved to %s", got)
	}
	pushes := runner.pushes()
	if len(pushes) != 1 {
		t.Fatalf("pushes = %d, want exactly 1: %v", len(pushes), pushes)
	}
	p := pushes[0]
	for _, want := range []string{"--atomic", "--force-with-lease=refs/tags/" + retagTestTag + ":" + rel, fix + ":refs/tags/" + retagTestTag} {
		if !hasArg(p, want) {
			t.Errorf("push is missing %q: %v", want, p)
		}
	}
	for _, a := range p {
		if strings.Contains(a, "refs/heads/") || a == "--force" || a == "-f" || strings.HasPrefix(a, "+") {
			t.Errorf("push writes a branch or forces without a lease: %q in %v", a, p)
		}
		if strings.Contains(a, retagTestToken) {
			t.Errorf("token leaked into argv: %v", p)
		}
	}

	// The scratch repo is reused on the next move.
	fix2 := f.commit("fix 2")
	f.push("HEAD:refs/heads/" + retagTestBranch)
	if err := g.Retag(context.Background(), retagReq(fix, fix2, fix2)); err != nil {
		t.Fatalf("second Retag: %v", err)
	}
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != fix2 {
		t.Fatalf("remote tag = %s after the second move, want %s", got, fix2)
	}
}

func assertRefused(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, ErrRetagRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %q does not say %q", err, want)
	}
}

func TestGitRetagger_RefusesNonFastForward(t *testing.T) {
	f := newGitFixture(t)
	base, rel := f.release()
	// The release branch is rewritten under the tag: base -> other. The tag
	// still points at rel, which "other" does not descend from.
	f.git(f.work, "checkout", "--quiet", "--detach", base)
	other := f.commit("diverged")
	f.push(other + ":refs/heads/" + retagTestBranch)

	runner := &recordingRunner{}
	err := f.retagger(runner).Retag(context.Background(), retagReq(rel, other, other))
	assertRefused(t, err, "not a fast-forward")
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != rel {
		t.Fatalf("tag moved to %s on a non-fast-forward", got)
	}
	if len(runner.pushes()) != 0 {
		t.Fatal("pushed despite a non-fast-forward")
	}
}

func TestGitRetagger_RefusesUnrelatedCommitsUnlessAllowed(t *testing.T) {
	f := newGitFixture(t)
	_, rel := f.release()
	f.commit("someone else's merge")
	fix := f.commit("fix")
	f.push("HEAD:refs/heads/" + retagTestBranch)

	err := f.retagger(nil).Retag(context.Background(), retagReq(rel, fix, fix))
	assertRefused(t, err, "1 commit(s)")
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != rel {
		t.Fatalf("tag moved past an unrelated commit: %s", got)
	}

	req := retagReq(rel, fix, fix)
	req.AllowIntervening = true
	if err := f.retagger(nil).Retag(context.Background(), req); err != nil {
		t.Fatalf("allowed intervening Retag: %v", err)
	}
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != fix {
		t.Fatalf("remote tag = %s, want %s", got, fix)
	}
}

func TestGitRetagger_RefusesCommitNotOnReleaseBranch(t *testing.T) {
	f := newGitFixture(t)
	_, rel := f.release()
	side := f.commit("fix on an unmerged branch")
	f.push("HEAD:refs/heads/side")
	// main still ends at rel.
	err := f.retagger(nil).Retag(context.Background(), retagReq(rel, side, side))
	assertRefused(t, err, "not on release branch")
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != rel {
		t.Fatalf("tag moved to an unmerged commit: %s", got)
	}
}

func TestGitRetagger_RefusesWhenTagAlreadyMoved(t *testing.T) {
	f := newGitFixture(t)
	base, rel := f.release()
	fix := f.commit("fix")
	f.push("HEAD:refs/heads/" + retagTestBranch)
	err := f.retagger(nil).Retag(context.Background(), retagReq(base, fix, fix))
	assertRefused(t, err, "already points at")
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != rel {
		t.Fatalf("tag = %s", got)
	}
}

func TestGitRetagger_LostLeaseNeverOverwrites(t *testing.T) {
	f := newGitFixture(t)
	base, rel := f.release()
	fix := f.commit("fix")
	f.push("HEAD:refs/heads/" + retagTestBranch)

	// A concurrent writer re-points the tag between the checks and the push.
	runner := &recordingRunner{before: func(args []string) {
		if hasArg(args, "push") {
			f.git(f.remote, "update-ref", "refs/tags/"+retagTestTag, base)
		}
	}}
	err := f.retagger(runner).Retag(context.Background(), retagReq(rel, fix, fix))
	assertRefused(t, err, "lease lost")
	if got := f.remoteRef("refs/tags/" + retagTestTag); got != base {
		t.Fatalf("the other writer's tag was overwritten: %s", got)
	}
}

func TestGitRetagger_RefusesAnnotatedTag(t *testing.T) {
	f := newGitFixture(t)
	_, rel := f.release()
	f.git(f.work, "tag", "-a", "v2.0.0", "-m", "annotated", rel)
	f.push("refs/tags/v2.0.0")
	fix := f.commit("fix")
	f.push("HEAD:refs/heads/" + retagTestBranch)
	req := retagReq(rel, fix, fix)
	req.Tag = "v2.0.0"
	assertRefused(t, f.retagger(nil).Retag(context.Background(), req), "annotated")
}

func TestGitRetagger_ValidationRefusesBeforeAnyGit(t *testing.T) {
	good := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	cases := map[string]RetagRequest{
		"not a release tag": {Tag: "latest", Branch: "main", OldSHA: good, NewSHA: other},
		"short sha":         {Tag: retagTestTag, Branch: "main", OldSHA: "abc", NewSHA: other},
		"same sha":          {Tag: retagTestTag, Branch: "main", OldSHA: good, NewSHA: good},
		"option branch":     {Tag: retagTestTag, Branch: "-main", OldSHA: good, NewSHA: other},
		"dotdot branch":     {Tag: retagTestTag, Branch: "a..b", OldSHA: good, NewSHA: other},
		"refspec branch":    {Tag: retagTestTag, Branch: "main:x", OldSHA: good, NewSHA: other},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &recordingRunner{}
			g := &GitRetagger{RemoteURL: "file:///nowhere", Dir: t.TempDir(), Minter: &fakeMinter{token: "tok"}, Runner: runner}
			if err := g.Retag(context.Background(), req); !errors.Is(err, ErrRetagRefused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("git ran for an invalid request: %v", runner.calls)
			}
		})
	}
}

func TestGitRetagger_SetupErrors(t *testing.T) {
	good := retagReq(strings.Repeat("a", 40), strings.Repeat("b", 40))
	t.Run("missing config", func(t *testing.T) {
		for _, g := range []*GitRetagger{
			{Dir: "x", Minter: &fakeMinter{token: "t"}},
			{RemoteURL: "file:///x", Minter: &fakeMinter{token: "t"}},
			{RemoteURL: "file:///x", Dir: "x"},
		} {
			err := g.Retag(context.Background(), good)
			if err == nil || errors.Is(err, ErrRetagRefused) {
				t.Fatalf("err = %v, want a plain configuration error", err)
			}
		}
	})
	t.Run("mint error", func(t *testing.T) {
		boom := errors.New("mint down")
		g := &GitRetagger{RemoteURL: "file:///x", Dir: t.TempDir(), Minter: &fakeMinter{err: boom}}
		if err := g.Retag(context.Background(), good); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("empty token", func(t *testing.T) {
		g := &GitRetagger{RemoteURL: "file:///x", Dir: t.TempDir(), Minter: &fakeMinter{token: " "}}
		if err := g.Retag(context.Background(), good); err == nil || errors.Is(err, ErrRetagRefused) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreachable remote is transient, not a refusal", func(t *testing.T) {
		if _, err := exec.LookPath("git"); err != nil {
			t.Skip("git not installed")
		}
		g := &GitRetagger{RemoteURL: "file://" + filepath.Join(t.TempDir(), "missing.git"), Dir: filepath.Join(t.TempDir(), "s"), Minter: &fakeMinter{token: "t"}}
		if err := g.Retag(context.Background(), good); err == nil || errors.Is(err, ErrRetagRefused) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("scratch dir cannot be created", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		g := &GitRetagger{RemoteURL: "file:///x", Dir: filepath.Join(file, "sub", "repo"), Minter: &fakeMinter{token: "t"}}
		if err := g.Retag(context.Background(), good); err == nil || errors.Is(err, ErrRetagRefused) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestGitRetagger_CommandFailuresAreTransient(t *testing.T) {
	boom := errors.New("boom")
	failOn := func(sub string) func(args []string) ([]byte, bool, error) {
		return func(args []string) ([]byte, bool, error) {
			if hasArg(args, sub) {
				return []byte("fatal: something else"), true, boom
			}
			return nil, false, nil
		}
	}
	for _, sub := range []string{"init", "config", "rev-parse", "merge-base", "rev-list", "push"} {
		t.Run(sub, func(t *testing.T) {
			f := newGitFixture(t)
			_, rel := f.release()
			fix := f.commit("fix")
			f.push("HEAD:refs/heads/" + retagTestBranch)
			runner := &recordingRunner{fail: failOn(sub)}
			err := f.retagger(runner).Retag(context.Background(), retagReq(rel, fix, fix))
			if !errors.Is(err, boom) || errors.Is(err, ErrRetagRefused) {
				t.Fatalf("err = %v, want the transient %v", err, boom)
			}
			if got := f.remoteRef("refs/tags/" + retagTestTag); got != rel {
				t.Fatalf("tag moved despite a failure: %s", got)
			}
		})
	}
}

func TestGitRetagger_PeelFailure(t *testing.T) {
	f := newGitFixture(t)
	_, rel := f.release()
	fix := f.commit("fix")
	f.push("HEAD:refs/heads/" + retagTestBranch)
	boom := errors.New("boom")
	runner := &recordingRunner{fail: func(args []string) ([]byte, bool, error) {
		for _, a := range args {
			if strings.HasSuffix(a, "^{commit}") && hasArg(args, "rev-parse") {
				return nil, true, boom
			}
		}
		return nil, false, nil
	}}
	if err := f.retagger(runner).Retag(context.Background(), retagReq(rel, fix, fix)); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

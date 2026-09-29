package releasesentinel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/pushbroker"
)

const (
	// retagTimeout bounds one whole retag (fetch, checks, push).
	retagTimeout = 2 * time.Minute
	// retagRemote is the remote name inside the scratch repository.
	retagRemote = "origin"
	// retagBranchRef and retagTagRef are where the scratch repository keeps
	// the fetched release branch and tag. They are private to that
	// repository and never pushed.
	retagBranchRef = "refs/sentinel/branch"
	retagTagRef    = "refs/sentinel/tag"
	// retagTokenEnvVar carries the minted token to git out-of-band: only the
	// variable NAME appears in argv (see pushbroker's audit F5 note).
	retagTokenEnvVar = "HIVE_RELEASE_SENTINEL_TOKEN"
	// retagDirPerm is the mode the scratch repository's parent is created with.
	retagDirPerm os.FileMode = 0o755
	// gitExitFalse is the exit status `git merge-base --is-ancestor` uses for
	// "not an ancestor" (any other non-zero status is an error).
	gitExitFalse = 1
	// leaseLostMarker is how git reports a --force-with-lease mismatch.
	leaseLostMarker = "stale info"
)

// retagCredentialHelper echoes the token from the environment. The empty
// helper before it clears any helper inherited from system or global config.
const retagCredentialHelper = `!f() { echo username=x-access-token; echo "password=$` + retagTokenEnvVar + `"; }; f`

var (
	fullSHAPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	// branchNamePattern is deliberately narrower than git's ref rules: the
	// branch name reaches git argv, so nothing that could read as an option
	// or a refspec modifier is accepted.
	branchNamePattern = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
)

// GitRetagger moves a release tag with a single leased, atomic git push from
// a scratch bare repository. It never pushes a branch.
type GitRetagger struct {
	// RemoteURL is the repository's https clone URL.
	RemoteURL string
	// Dir is the scratch bare repository (created on first use). It holds
	// commits only (a tree-less partial fetch), enough for the ancestry checks.
	Dir string
	// Minter mints the push token for Repo.
	Minter pushbroker.TokenMinter
	// Runner runs git; nil means pushbroker.ExecRunner.
	Runner pushbroker.CommandRunner
}

func (g *GitRetagger) runner() pushbroker.CommandRunner {
	if g.Runner != nil {
		return g.Runner
	}
	return pushbroker.ExecRunner{}
}

func validateRetag(req RetagRequest) error {
	if !releaseTagPattern.MatchString(req.Tag) {
		return refused("%q is not a v<MAJOR>.<MINOR>.<PATCH> release tag", req.Tag)
	}
	if !fullSHAPattern.MatchString(req.OldSHA) || !fullSHAPattern.MatchString(req.NewSHA) {
		return refused("old %q and new %q must both be full commit SHAs", req.OldSHA, req.NewSHA)
	}
	if req.OldSHA == req.NewSHA {
		return refused("the tag already points at %s", shortSHA(req.NewSHA))
	}
	b := req.Branch
	if !branchNamePattern.MatchString(b) || strings.HasPrefix(b, "-") || strings.Contains(b, "..") {
		return refused("release branch %q is not a plain branch name", b)
	}
	return nil
}

// Retag verifies the move against the remote and then performs it:
//
//  1. fetch the release branch and the tag (commits only);
//  2. the tag must still point at OldSHA, and be a lightweight tag;
//  3. NewSHA must be a fast-forward of OldSHA and contained in the branch;
//  4. unless AllowIntervening, every commit in OldSHA..NewSHA must be NewSHA
//     or one of FixCommits;
//  5. git push --atomic --force-with-lease=refs/tags/<tag>:<OldSHA>
//     <NewSHA>:refs/tags/<tag>. The only ref in the push is the tag.
func (g *GitRetagger) Retag(ctx context.Context, req RetagRequest) error {
	if err := validateRetag(req); err != nil {
		return err
	}
	if strings.TrimSpace(g.RemoteURL) == "" || strings.TrimSpace(g.Dir) == "" || g.Minter == nil {
		return errors.New("release sentinel retagger: remote URL, scratch dir and token minter are required")
	}
	ctx, cancel := context.WithTimeout(ctx, retagTimeout)
	defer cancel()

	token, err := g.Minter.MintPushToken(ctx, req.Repo)
	if err != nil {
		return fmt.Errorf("mint retag token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("mint retag token: empty token")
	}
	env := append(pushbroker.PushEnv(os.Environ()), "GIT_TERMINAL_PROMPT=0", retagTokenEnvVar+"="+token)

	if err := g.ensureRepo(ctx, env); err != nil {
		return err
	}
	tagRef := "refs/tags/" + req.Tag
	if _, err := g.git(ctx, env, true, "fetch", "--quiet", "--no-tags", "--filter=tree:0", retagRemote,
		"+refs/heads/"+req.Branch+":"+retagBranchRef, "+"+tagRef+":"+retagTagRef); err != nil {
		return fmt.Errorf("fetch %s and %s: %w", req.Branch, req.Tag, err)
	}

	obj, err := g.git(ctx, env, false, "rev-parse", "--verify", retagTagRef)
	if err != nil {
		return fmt.Errorf("resolve fetched tag: %w", err)
	}
	peeled, err := g.git(ctx, env, false, "rev-parse", "--verify", retagTagRef+"^{commit}")
	if err != nil {
		return fmt.Errorf("peel fetched tag: %w", err)
	}
	if obj != peeled {
		return refused("%s is an annotated tag; only lightweight tags are moved", req.Tag)
	}
	if obj != req.OldSHA {
		return refused("%s already points at %s, not %s", req.Tag, shortSHA(obj), shortSHA(req.OldSHA))
	}
	if _, err := g.git(ctx, env, false, "cat-file", "-e", req.NewSHA+"^{commit}"); err != nil {
		return refused("fix commit %s is not on release branch %s", shortSHA(req.NewSHA), req.Branch)
	}
	fastForward, err := g.isAncestor(ctx, env, req.OldSHA, req.NewSHA)
	if err != nil {
		return err
	}
	if !fastForward {
		return refused("fix commit %s is not a fast-forward of %s at %s", shortSHA(req.NewSHA), req.Tag, shortSHA(req.OldSHA))
	}
	onBranch, err := g.isAncestor(ctx, env, req.NewSHA, retagBranchRef)
	if err != nil {
		return err
	}
	if !onBranch {
		return refused("fix commit %s is not on release branch %s", shortSHA(req.NewSHA), req.Branch)
	}
	if !req.AllowIntervening {
		out, err := g.git(ctx, env, false, "rev-list", req.OldSHA+".."+req.NewSHA)
		if err != nil {
			return fmt.Errorf("list commits %s..%s: %w", shortSHA(req.OldSHA), shortSHA(req.NewSHA), err)
		}
		allowed := map[string]bool{req.NewSHA: true}
		for _, c := range req.FixCommits {
			allowed[c] = true
		}
		unrelated := 0
		for _, c := range strings.Fields(out) {
			if !allowed[c] {
				unrelated++
			}
		}
		if unrelated > 0 {
			return refused("%d commit(s) between %s and the fix are not part of the fix PR (retag_allow_intervening_commits is off)", unrelated, req.Tag)
		}
	}

	out, err := g.git(ctx, env, true, "push", "--atomic", "--no-verify",
		"--force-with-lease="+tagRef+":"+req.OldSHA, retagRemote, req.NewSHA+":"+tagRef)
	if err != nil {
		if strings.Contains(out, leaseLostMarker) {
			return refused("%s moved on the remote after it was checked (lease lost)", req.Tag)
		}
		return fmt.Errorf("push %s: %w", req.Tag, err)
	}
	return nil
}

// ensureRepo creates the scratch bare repository on first use and points
// its remote at RemoteURL.
func (g *GitRetagger) ensureRepo(ctx context.Context, env []string) error {
	if _, err := os.Stat(filepath.Join(g.Dir, "HEAD")); err != nil {
		parent := filepath.Dir(g.Dir)
		if err := os.MkdirAll(parent, retagDirPerm); err != nil {
			return fmt.Errorf("create retag scratch dir: %w", err)
		}
		if out, err := g.runner().Run(ctx, parent, env, "git", "init", "--quiet", "--bare", g.Dir); err != nil {
			return fmt.Errorf("init retag scratch repo: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if _, err := g.git(ctx, env, false, "config", "remote."+retagRemote+".url", g.RemoteURL); err != nil {
		return fmt.Errorf("configure retag remote: %w", err)
	}
	return nil
}

// git runs one git command in the scratch repository and returns its
// trimmed combined output. Network commands get the token credential helper
// and no hooks.
func (g *GitRetagger) git(ctx context.Context, env []string, network bool, args ...string) (string, error) {
	full := []string{"-c", "core.hooksPath=/dev/null"}
	if network {
		full = append(full, "-c", "credential.helper=", "-c", "credential.helper="+retagCredentialHelper)
	}
	full = append(full, args...)
	out, err := g.runner().Run(ctx, g.Dir, env, "git", full...)
	text := strings.TrimSpace(string(out))
	if err != nil {
		return text, fmt.Errorf("git %s: %w: %s", args[0], err, text)
	}
	return text, nil
}

// isAncestor reports whether a is an ancestor of (or equal to) b.
func (g *GitRetagger) isAncestor(ctx context.Context, env []string, a, b string) (bool, error) {
	_, err := g.git(ctx, env, false, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == gitExitFalse {
		return false, nil
	}
	return false, fmt.Errorf("check ancestry %s -> %s: %w", shortSHA(a), shortSHA(b), err)
}

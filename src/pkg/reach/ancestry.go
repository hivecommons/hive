package reach

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Ancestry answers the one immutable question the reach join needs (#3994):
// is `descendant` the same commit as, or a descendant of, `ancestor`? A hive
// counts toward a PR's reach only when the commit it reports RUNNING contains
// the PR's merge commit — the anchoring rule of #3973 ("merged is not
// deployed", see the #3816 postmortem in the design doc).
//
// It is an interface so the join logic tests against an in-memory fake and
// the hub picks the real implementation at wiring time.
type Ancestry interface {
	IsAncestor(ancestor, descendant string) (bool, error)
}

// GitAncestry resolves ancestry against a local git clone (per the epic's
// resolved open question: the hub's repo clone provides the commit graph)
// using the plumbing command built for exactly this test:
//
//	git -C <dir> merge-base --is-ancestor <ancestor> <descendant>
//
// Exit status 0 means ancestor is an ancestor of (or equal to) descendant;
// exit status 1 means it is not; anything else is an error (unknown SHA,
// shallow clone missing history, not a repo).
//
// Ancestry is immutable, so definitive answers are cached forever; the cache
// is bounded the same way commitOrderCache in pkg/hub is — dropped wholesale
// on overflow rather than LRU'd, because a re-resolve costs one local git
// invocation.
type GitAncestry struct {
	// RepoDir is the path of the hub's repo clone. The clone must have real
	// history (not --depth 1) for the commits being compared.
	RepoDir string

	mu    sync.Mutex
	cache map[[2]string]bool
}

// gitAncestryCacheMax bounds the resolved-pair cache. Spokes report the
// commit SHAs being compared, so the key space is externally influenced and
// must not grow without bound; see commitOrderCacheMax in pkg/hub for the
// same reasoning.
const gitAncestryCacheMax = 4096

// NewGitAncestry returns an Ancestry backed by the git clone at repoDir.
func NewGitAncestry(repoDir string) *GitAncestry {
	return &GitAncestry{RepoDir: repoDir, cache: map[[2]string]bool{}}
}

// IsAncestor reports whether ancestor is an ancestor of (or the same commit
// as) descendant, per `git merge-base --is-ancestor`. Empty inputs are an
// error: an unknown commit must never silently read as reached.
func (g *GitAncestry) IsAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == "" || descendant == "" {
		return false, fmt.Errorf("ancestry: empty commit (ancestor=%q descendant=%q)", ancestor, descendant)
	}
	key := [2]string{ancestor, descendant}

	g.mu.Lock()
	if g.cache == nil {
		g.cache = map[[2]string]bool{}
	}
	if ok, hit := g.cache[key]; hit {
		g.mu.Unlock()
		return ok, nil
	}
	g.mu.Unlock()

	// merge-base --is-ancestor communicates its answer via exit status:
	// 0 = yes, 1 = no, anything else = real failure.
	cmd := exec.Command("git", "-C", g.RepoDir, "merge-base", "--is-ancestor", ancestor, descendant)
	out, err := cmd.CombinedOutput()
	var answer bool
	switch {
	case err == nil:
		answer = true
	case cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1:
		answer = false
	default:
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: %w (%s)",
			ancestor, descendant, err, strings.TrimSpace(string(out)))
	}

	g.mu.Lock()
	if len(g.cache) >= gitAncestryCacheMax {
		g.cache = map[[2]string]bool{}
	}
	g.cache[key] = answer
	g.mu.Unlock()
	return answer, nil
}

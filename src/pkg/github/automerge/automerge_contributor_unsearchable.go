package automerge

import (
	"errors"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// contributorUnsearchableTTL is how long contributorMergedRepos leaves an
// (author, repository) pair out of its search after GitHub refused to search
// it. The refusal is deterministic — the repository was renamed, deleted or
// made private to the App, or the author's account cannot be searched — so
// re-asking every sweep only repeats the 422 (hivecommons/hive#17148); the
// TTL lets a repaired repository or account come back without a restart.
const contributorUnsearchableTTL = time.Hour

// contributorUnsearchableMessage is logged at WARN only for a refusal whose
// author and searched repository have both not been warned for yet in this
// process, then at Debug. Keying the WARN by pair let one invisible
// repository warn once for every contributor (or one unsearchable author once
// for every hive repository), which the self-report classifier counted as a
// repeated unclassified error (hivecommons/hive#18059).
const contributorUnsearchableMessage = "contributor-lane auto-promotion search cannot see repository for this author; leaving it out of the hive-wide count"

// searchQueryUnsearchable reports whether err is GitHub's 422 Validation
// Failed for a search whose listed users or repositories "cannot be searched
// either because the resources do not exist or you do not have permission to
// view them". It does not say which qualifier it refused.
func searchQueryUnsearchable(err error) bool {
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity
}

func contributorUnsearchableKey(author string, ref contributorRepoRef) string {
	return strings.ToLower(author + "|" + ref.Owner + "/" + ref.Repo)
}

// contributorSearchSkips reports whether ref is still inside the window in
// which author's search leaves it out.
func (c *Engine) contributorSearchSkips(author string, ref contributorRepoRef) bool {
	key := contributorUnsearchableKey(author, ref)
	c.contributorUnsearchableMu.Lock()
	until, ok := c.contributorUnsearchable[key]
	c.contributorUnsearchableMu.Unlock()
	return ok && c.clock().Before(until)
}

// noteContributorUnsearchable records that GitHub refused to search ref for
// author, so the next contributorUnsearchableTTL of sweeps leave it out. A
// refusal logs at WARN only when neither its author nor its searched
// repository has warned before in this process; every other refusal —
// another author for the same invisible repository, another repository for
// the same unsearchable author, or the same pair again after the TTL — logs
// at Debug, so the warnings stay bounded by the distinct causes rather than
// growing with authors × repositories.
func (c *Engine) noteContributorUnsearchable(owner, repo, author string, ref contributorRepoRef, err error) {
	key := contributorUnsearchableKey(author, ref)
	authorKey := "author:" + strings.ToLower(author)
	repoKey := "repo:" + strings.ToLower(ref.Owner+"/"+ref.Repo)
	c.contributorUnsearchableMu.Lock()
	if c.contributorUnsearchable == nil || len(c.contributorUnsearchable) > 4096 {
		c.contributorUnsearchable = map[string]time.Time{}
	}
	if c.contributorUnsearchableWarned == nil || len(c.contributorUnsearchableWarned) > 4096 {
		c.contributorUnsearchableWarned = map[string]bool{}
	}
	c.contributorUnsearchable[key] = c.clock().Add(contributorUnsearchableTTL)
	warned := c.contributorUnsearchableWarned[authorKey] || c.contributorUnsearchableWarned[repoKey]
	c.contributorUnsearchableWarned[authorKey] = true
	c.contributorUnsearchableWarned[repoKey] = true
	c.contributorUnsearchableMu.Unlock()
	args := []any{"repo", owner + "/" + repo, "author", author, "searched_repo", ref.Owner + "/" + ref.Repo, "retry_after", contributorUnsearchableTTL, "error", err}
	if warned {
		c.debug(contributorUnsearchableMessage, args...)
		return
	}
	c.warn(contributorUnsearchableMessage, args...)
}

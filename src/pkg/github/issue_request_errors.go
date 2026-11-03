package github

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
)

// Permanent vs transient failures of a relay request.
//
// Observed live on the hive's own hive (2026-10-22): three agents smoke-tested
// the `hive-open-issue` relay with the literal placeholder repo `owner/repo`
// (title "t", body "b"). Eleven request files, all naming a repository that
// cannot exist, were retried on the watcher's 24h exponential backoff for
// three hours — 175 WARN lines — and every attempt failed with the mutation
// journal's "must be reconciled" refusal: the first create had 404ed, which
// the journal recorded as Unknown (an errored call may still have taken
// effect), and nothing ever reconciled it, so no later attempt — including the
// ten other files, which derived the SAME logical operation from the same
// content — could ever proceed.
//
// Three defences, all here or next to here:
//
//  1. a request whose repo is structurally impossible, a well-known template
//     placeholder, or outside the hive's configured repo set is quarantined
//     at once (permanentIssueRequestError) — retrying can never change it;
//  2. a GitHub refusal that is definite (404, 410, 422, 401, non-rate-limit
//     403) is classified permanent for the watcher AND journaled NotApplied
//     rather than Unknown (isDefiniteIssueRefusal), so an identical request
//     is never fenced behind it;
//  3. an operation the journal still holds Unknown is reconciled against the
//     repo's open issues, and failing that expired after
//     issueCreateUnknownTTL — see reconcileIssueCreate.

// permanentIssueRequestError marks a relay request as never able to succeed:
// the watcher quarantines it (.failed) on the first attempt instead of
// scheduling a retry.
type permanentIssueRequestError struct{ msg string }

func (e *permanentIssueRequestError) Error() string { return e.msg }

func permanentIssueRequestErrorf(format string, args ...any) error {
	return &permanentIssueRequestError{msg: fmt.Sprintf(format, args...)}
}

// issuePlaceholderRepoOwners and issuePlaceholderRepoNames are the owner and
// repository components agents are observed to copy out of documentation,
// shell help text and prompt templates instead of a real repository. The
// match is case-insensitive on each component independently: `OWNER/REPO`,
// `example/anything` and `anything/your-repo` are all placeholders. The
// hive-open-issue shim refuses the same set before it writes a request (see
// bin/hive-open-issue.sh); keep the two lists in step.
var (
	issuePlaceholderRepoOwners = map[string]bool{
		"owner": true, "org": true, "organization": true, "orgname": true, "org-name": true,
		"example": true, "example-org": true, "exampleorg": true,
		"your-org": true, "yourorg": true, "your_org": true, "your-organization": true,
		"my-org": true, "myorg": true, "my_org": true,
		"acme": true, "user": true, "username": true, "placeholder": true,
	}
	issuePlaceholderRepoNames = map[string]bool{
		"repo": true, "repository": true, "reponame": true, "repo-name": true,
		"your-repo": true, "yourrepo": true, "your_repo": true, "your-repository": true,
		"my-repo": true, "myrepo": true, "my_repo": true,
		"example-repo": true, "placeholder": true,
	}
)

// isPlaceholderRepoRef reports whether owner/name is a documentation
// placeholder rather than a repository anyone could own.
func isPlaceholderRepoRef(owner, name string) bool {
	return issuePlaceholderRepoOwners[strings.ToLower(owner)] || issuePlaceholderRepoNames[strings.ToLower(name)]
}

// repoInConfiguredSet reports whether repo (bare or owner/name) is one of the
// repositories this hive is configured to work on. configured is false when
// the hive has no repo list at all (a client built without one), in which
// case nothing can be said and callers must not refuse on it.
func (c *Client) repoInConfiguredSet(repo string) (in bool, configured bool) {
	repos := c.getRepos()
	if len(repos) == 0 {
		return false, false
	}
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	want := strings.ToLower(owner + "/" + name)
	for _, r := range repos {
		o, n := c.splitRepo(strings.TrimSpace(r))
		if strings.ToLower(o+"/"+n) == want {
			return true, true
		}
	}
	return false, true
}

// validateIssueRequestRepo is the relay's repository gate, run before
// authorization and before any GitHub call. It returns a
// permanentIssueRequestError — never a retryable one — because none of the
// three refusals can change on a later tick:
//
//   - the ref is not a syntactically possible owner/name (validateRepoRef);
//   - the ref is a template placeholder such as owner/repo, UNLESS the
//     operator has literally configured that repository (then it is real);
//   - the hive has a configured repo set and the ref is not in it: the relay
//     writes with the App installation token, and an agent's scope
//     ($HIVE_REPOS) is always a subset of the hive's.
func (c *Client) validateIssueRequestRepo(repo string) error {
	owner, name := c.splitRepo(strings.TrimSpace(repo))
	if err := validateRepoRef(owner, name); err != nil {
		return permanentIssueRequestErrorf("%v", err)
	}
	in, configured := c.repoInConfiguredSet(repo)
	if isPlaceholderRepoRef(owner, name) && !in {
		return permanentIssueRequestErrorf("refusing repo %q: this is a documentation placeholder, not a repository — name a real repo from $HIVE_REPOS (test the relay with hive-open-issue --dry-run; live calls create real GitHub objects)", owner+"/"+name)
	}
	if configured && !in {
		return permanentIssueRequestErrorf("refusing repo %q: not in this hive's configured repository set (%s)", owner+"/"+name, strings.Join(c.getRepos(), ", "))
	}
	return nil
}

// githubStatusOf returns the HTTP status of a go-github error response, or 0
// when err carries none (transport errors, rate-limit types, local errors).
func githubStatusOf(err error) int {
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		return ghErr.Response.StatusCode
	}
	return 0
}

// isDefiniteIssueRefusal reports whether err is GitHub definitively refusing
// a write — the request was understood and rejected, so the effect certainly
// did not take place: 404 (no such repo/issue, or the installation cannot see
// it), 410 (issues disabled), 422 (validation), 401 and a 403 that is not a
// rate limit (missing permission). The journal records these NotApplied
// instead of Unknown, so the same logical operation may be attempted again
// (by a corrected request) instead of being fenced forever behind a refusal
// that never needed reconciling.
func isDefiniteIssueRefusal(err error) bool {
	if err == nil || isRateLimitErr(err) {
		return false
	}
	switch githubStatusOf(err) {
	case http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity, http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

// issueRequestFailureIsPermanent classifies a fulfilment error for the
// watcher: true means quarantine now (.failed), false means retry with
// backoff. Permanent: a relay repo refusal (permanentIssueRequestError), the
// structural repo refusal createIssue and friends make themselves
// (validateRepoRef), and every definite GitHub refusal. Transient: 5xx,
// network, rate limits, and the journal's needs-reconciliation refusal (the
// create path reconciles it; the watcher caps how often it may recur).
func issueRequestFailureIsPermanent(err error) bool {
	if err == nil {
		return false
	}
	var perm *permanentIssueRequestError
	if errors.As(err, &perm) {
		return true
	}
	var repoRef *invalidRepoRefError
	if errors.As(err, &repoRef) {
		return true
	}
	if errors.Is(err, effects.ErrNeedsReconciliation) || isRetryableGitHubError(err) {
		return false
	}
	return isDefiniteIssueRefusal(err)
}

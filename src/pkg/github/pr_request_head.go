package github

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// A PR request's head must name a real, pushed branch. An agent whose checkout
// is in detached-HEAD state (a sandbox run-stage worktree, `git checkout
// <sha>`) gets the literal string "HEAD" from `git rev-parse --abbrev-ref
// HEAD`; before this guard that string travelled through the signed-commit
// step (compare base...HEAD, "HEAD has no changes against main") and CreatePR
// (GET git/ref/heads/HEAD 404, POST /pulls 422 head invalid), and the watcher
// retried the request every 15 minutes for 24 hours.

const (
	// prRequestHeadInvalidMaxAttempts bounds retries of a GitHub 422 "head
	// invalid" on POST /pulls. The same response is also what GitHub returns
	// for a branch whose push has not landed yet, so a few backed-off retries
	// are kept before the failure is declared terminal.
	prRequestHeadInvalidMaxAttempts = 3

	// detachedHeadRef is what git prints for the current branch when HEAD is
	// detached.
	detachedHeadRef = "HEAD"
)

// errInvalidHead reports a PR head that can never be a branch name, so no
// retry can make the request valid.
type errInvalidHead struct {
	head   string
	reason string
}

func (e *errInvalidHead) Error() string {
	return fmt.Sprintf("PR head %q is not a pushable branch: %s", e.head, e.reason)
}

// invalidHeadReason explains why head cannot be a PR head branch, or returns
// "" when it is plausible. It is a local check: whether the branch exists on
// the remote is the content gate's question.
func invalidHeadReason(head string) string {
	h := strings.TrimSpace(head)
	switch {
	case h == "":
		return "no head branch was given"
	case strings.EqualFold(h, detachedHeadRef) || h == "@":
		return "the agent's checkout is in detached-HEAD state, so `git rev-parse --abbrev-ref HEAD` returned the literal HEAD instead of a branch; create and push a named branch (git switch -c <name>; hive-push-branch) and request the PR with --head <name>"
	case strings.HasPrefix(h, "refs/") && !strings.HasPrefix(h, "refs/heads/"):
		return "only branches (refs/heads/...) can be a PR head"
	case strings.HasPrefix(h, "-"), strings.HasPrefix(h, "/"), strings.HasSuffix(h, "/"),
		strings.HasSuffix(h, "."), strings.HasSuffix(h, ".lock"),
		strings.Contains(h, ".."), strings.Contains(h, "//"), strings.Contains(h, "@{"),
		strings.ContainsAny(h, " \t\r\n~^:?*[\\"):
		return "the name is not a valid git branch name"
	}
	return ""
}

// isHeadInvalid422 reports whether err is GitHub's 422 Validation Failed
// naming the head field as invalid.
func isHeadInvalid422(err error) bool {
	var er *gh.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil || er.Response.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	for _, e := range er.Errors {
		if e.Field == "head" && e.Code == "invalid" {
			return true
		}
	}
	return false
}

// invalidHeadFailure returns the terminal reason when err means this request
// can never succeed because of its head: a local errInvalidHead, or a 422 head
// invalid once attempts (failures already recorded for the request, plus this
// one) reaches prRequestHeadInvalidMaxAttempts.
func invalidHeadFailure(err error, attempts int) (string, bool) {
	var bad *errInvalidHead
	if errors.As(err, &bad) {
		return bad.Error(), true
	}
	if isHeadInvalid422(err) && attempts >= prRequestHeadInvalidMaxAttempts {
		return fmt.Sprintf("GitHub rejected the head branch as invalid (422) %d times; the branch is not on the remote under that name or is not a branch: %s", attempts, err.Error()), true
	}
	return "", false
}

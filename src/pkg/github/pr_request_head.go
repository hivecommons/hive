package github

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Head-branch validity for PR requests (hivecommons/hive#18907).
//
// A request whose head is the literal "HEAD" (hive-open-pr run in a detached
// worktree, where `git rev-parse --abbrev-ref HEAD` answers "HEAD") can never
// open a PR: GitHub resolves compares against it as the default branch, so
// the signed-commit step reports "HEAD has no changes" and carries on, and
// POST /pulls then refuses it with 422 head invalid. The bounded retry turned
// that into a day of 15-minute retries while the agent's real work was never
// published. Such a head is refused before any GitHub call, and a 422 head
// invalid from GitHub ends the request after prHeadInvalidMaxAttempts.

// AuditActionAgentPRRejected is the audit action recorded when the
// pr-request watcher gives up on a request terminally because its head can
// never open a PR, so the failure is visible beside agent_pr_created rather
// than only in a quarantined result file.
const AuditActionAgentPRRejected = "agent_pr_rejected"

// prHeadInvalidMaxAttempts bounds how many times a request GitHub refused
// with 422 head invalid is attempted. More than one, because a branch pushed
// moments before the request can briefly be invisible to the pulls API; small,
// because no retry fixes a head that does not name a branch.
const prHeadInvalidMaxAttempts = 3

// invalidPRHeadReason reports why head can never be a PR head branch, or ""
// when it may be. It refuses the names that denote a checkout rather than a
// branch; anything else is left to GitHub, whose 422 is handled by
// prHeadInvalid.
func invalidPRHeadReason(head string) string {
	h := strings.TrimPrefix(strings.TrimSpace(head), "refs/heads/")
	switch h {
	case "HEAD", "@":
		return fmt.Sprintf("head %q names the agent's checkout, not a branch (hive-open-pr ran on a detached HEAD, where `git rev-parse --abbrev-ref HEAD` prints \"HEAD\"); GitHub rejects it as an invalid head on every attempt. Push a named branch and request the PR with --head <branch>", strings.TrimSpace(head))
	}
	return ""
}

// prHeadInvalid reports whether err is GitHub refusing a PR create because
// the head is invalid: 422 Validation Failed with {Field:head Code:invalid}.
func prHeadInvalid(err error) bool {
	if err == nil {
		return false
	}
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
		for _, e := range ghErr.Errors {
			if strings.EqualFold(e.Field, "head") && strings.EqualFold(e.Code, "invalid") {
				return true
			}
		}
		return false
	}
	// Fallback for an error that reached us only as text (wrapped without
	// %w somewhere along the mutation boundary).
	msg := err.Error()
	return strings.Contains(msg, "422") && strings.Contains(msg, "Field:head Code:invalid")
}

// rejectPRRequestHead ends a request terminally because its head can never
// open a PR: the same quarantine as any policy rejection, plus an audit entry
// so the dropped work shows on the activity surfaces instead of only in a
// .rejected file nobody reads.
func (c *Client) rejectPRRequestHead(path string, req PRRequest, reason string, attempts int, nowFn func() time.Time) {
	c.rejectPRRequest(path, req, "head", reason, nowFn)
	c.recordWriteAudit(AuditActionAgentPRRejected, c.attributionMeta(req.Agent),
		WriteTarget{Repo: req.Repo},
		"head", req.Head,
		"gate", "head",
		"attempts", strconv.Itoa(attempts),
		"reason", reason)
	c.logger.Error("pr-request watcher: request ended — its head can never open a PR",
		slog.String("repo", req.Repo), slog.String("head", req.Head),
		slog.String("agent", req.Agent), slog.Int("attempts", attempts),
		slog.String("reason", reason), slog.String("path", path))
}

// maybeEndHeadInvalidPRRequest ends the request at path when err is a 422
// head invalid that has now been seen prHeadInvalidMaxAttempts times, and
// reports whether it did. Below the bound it returns false and the caller
// records an ordinary retryable failure.
func (c *Client) maybeEndHeadInvalidPRRequest(path string, req PRRequest, err error, nowFn func() time.Time) bool {
	if !prHeadInvalid(err) {
		return false
	}
	attempt := c.prRetries.attempts(path) + 1
	if attempt < prHeadInvalidMaxAttempts {
		return false
	}
	reason := fmt.Sprintf("GitHub refused head %q as invalid (422) on %d attempts: the branch is not on the repository under that name, so no retry can open this PR. Push the branch and request the PR again with --head <the pushed branch>. Last error: %v", req.Head, attempt, err)
	c.rejectPRRequestHead(path, req, reason, attempt, nowFn)
	return true
}

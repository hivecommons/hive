package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
)

// issueWriteAPIErrorNotApplied wraps a definite GitHub refusal of an issue
// comment or label write in effects.NotApplied, so the mutation journal
// records NotApplied and the same logical write retries once the cause is
// fixed (#11556). Ambiguous failures (timeouts, 5xx, transport errors) are
// returned unchanged and stay Unknown.
func issueWriteAPIErrorNotApplied(err error) error {
	if err == nil {
		return nil
	}
	if isDefiniteIssueWriteRefusal(err) {
		return effects.NotApplied(err)
	}
	return err
}

// isDefiniteIssueWriteRefusal reports whether an issue comment / label write
// error is GitHub refusing the request before applying anything: a rate or
// abuse limit, or a 4xx such as 403 "Resource not accessible by integration",
// 404 unknown repo/issue, 410 gone or 422 validation failure.
func isDefiniteIssueWriteRefusal(err error) bool {
	if isRateLimitErr(err) {
		return true
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	switch ghErr.Response.StatusCode {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound,
		http.StatusGone, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	}
	return false
}

// executeIssueWrite runs an issue comment / label write through the mutation
// boundary. A write the journal holds as unresolved (an earlier attempt
// errored ambiguously) is reconciled from the issue's authoritative state via
// observe and replayed once, mirroring reconcileMerge: a write already present
// on GitHub replays as idempotent success, an absent one runs again. When the
// issue cannot be read the original refusal stands (#11556).
func (c *Client) executeIssueWrite(ctx context.Context, claim effects.Claim, effect func(context.Context) (effects.Result, error), observe func(context.Context) (effects.ExternalState, error)) error {
	boundary := c.mutationBoundary()
	_, err := effects.Execute(ctx, boundary, claim, effect)
	if !errors.Is(err, effects.ErrNeedsReconciliation) {
		return err
	}
	state, obsErr := observe(ctx)
	if obsErr != nil {
		return fmt.Errorf("%w (reconciliation lookup failed: %v)", err, obsErr)
	}
	if recErr := effects.Reconcile(ctx, boundary, claim, state); recErr != nil {
		return fmt.Errorf("%w (reconciliation failed: %v)", err, recErr)
	}
	c.logger.Info("reconciled unresolved issue write against GitHub",
		slog.String("repo", claim.Repo), slog.String("kind", claim.Kind),
		slog.String("target", claim.Target), slog.Bool("applied", state.Applied))
	_, err = effects.Execute(ctx, boundary, claim, effect)
	return err
}

// observeIssueComment reports whether a comment with exactly body already
// exists on the issue, carrying its URL as provenance.
func (c *Client) observeIssueComment(ctx context.Context, owner, repo string, number int, body string) (effects.ExternalState, error) {
	want := normalizeCommentBody(body)
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.client.Issues.ListComments(WithRESTCaller(ctx, "hive:reconcile_issue_comment"), owner, repo, number, opts)
		if err != nil {
			return effects.ExternalState{}, err
		}
		for _, comment := range comments {
			if normalizeCommentBody(comment.GetBody()) == want {
				return effects.ExternalState{Applied: true, Provenance: comment.GetHTMLURL()}, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return effects.ExternalState{}, nil
		}
		opts.Page = resp.NextPage
	}
}

func normalizeCommentBody(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
}

// observeIssueLabels reports whether the issue's labels already satisfy a
// label write: every label in add is present and remove (when set) is absent.
func (c *Client) observeIssueLabels(ctx context.Context, owner, repo string, number int, add []string, remove string) (effects.ExternalState, error) {
	issue, _, err := c.client.Issues.Get(WithRESTCaller(ctx, "hive:reconcile_label_mutation"), owner, repo, number)
	if err != nil {
		return effects.ExternalState{}, err
	}
	present := make(map[string]bool, len(issue.Labels))
	for _, l := range issue.Labels {
		present[strings.ToLower(l.GetName())] = true
	}
	applied := true
	for _, l := range add {
		if !present[strings.ToLower(l)] {
			applied = false
		}
	}
	if remove != "" && present[strings.ToLower(remove)] {
		applied = false
	}
	state := effects.ExternalState{Applied: applied}
	if applied {
		state.Provenance = owner + "/" + repo + "#" + strconv.Itoa(number)
	}
	return state, nil
}

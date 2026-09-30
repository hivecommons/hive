package github

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
)

// The `request_review` relay operation (hivecommons/hive#9587).
//
// Asking a person or team to review a PR had no relay: an agent could only
// reach it with a direct `gh pr edit --add-reviewer` (or the REST call behind
// it), outside the audited write surface. The issue-request watcher's
// `request_review` kind closes that gap with the same file-UID authorizer,
// lane allowlist, repo pause, repo scope and redacted audit entry as every
// other relay operation.
//
// Reviewer names are validated before any GitHub call. A request that names
// an impossible login or team slug, or more reviewers than GitHub accepts,
// can never succeed, so it is quarantined as malformed instead of retried.

// maxReviewRequestReviewers is GitHub's cap on requested reviewers (users and
// teams together) for a pull request.
const maxReviewRequestReviewers = 15

// githubLoginPattern matches a GitHub user login: alphanumerics and single
// hyphens, no leading hyphen, at most 39 characters. Bot logins keep their
// `[bot]` suffix.
var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:\[bot\])?$`)

// githubTeamSlugPattern matches a team slug. An `org/` prefix is accepted
// and stripped by normalizeTeamReviewers, since the API takes the bare slug.
var githubTeamSlugPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// normalizeReviewers trims, drops empties, strips a leading `@` and
// de-duplicates (case-insensitively) a requested reviewer list, keeping the
// order the agent asked for. Entries may be comma-separated.
func normalizeReviewers(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			name := strings.TrimPrefix(strings.TrimSpace(part), "@")
			if name == "" {
				continue
			}
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, name)
		}
	}
	return out
}

// normalizeTeamReviewers is normalizeReviewers for team slugs, also dropping
// an `org/` prefix.
func normalizeTeamReviewers(in []string) []string {
	var trimmed []string
	for _, raw := range in {
		for _, part := range strings.Split(raw, ",") {
			name := strings.TrimPrefix(strings.TrimSpace(part), "@")
			if i := strings.LastIndex(name, "/"); i >= 0 {
				name = name[i+1:]
			}
			trimmed = append(trimmed, name)
		}
	}
	return normalizeReviewers(trimmed)
}

// reviewRequestShapeError returns why a request_review request can never
// succeed, or "" when its reviewer lists are well formed.
func reviewRequestShapeError(reviewers, teams []string) string {
	if len(reviewers) == 0 && len(teams) == 0 {
		return "request_review request requires at least one reviewer in reviewers or team_reviewers"
	}
	if n := len(reviewers) + len(teams); n > maxReviewRequestReviewers {
		return "request_review request names " + strconv.Itoa(n) + " reviewers; GitHub accepts at most " +
			strconv.Itoa(maxReviewRequestReviewers)
	}
	for _, login := range reviewers {
		if !githubLoginPattern.MatchString(login) || strings.Contains(login, "--") || strings.HasSuffix(strings.TrimSuffix(login, "[bot]"), "-") {
			return "request_review request names an invalid GitHub login " + strconv.Quote(login)
		}
	}
	for _, slug := range teams {
		if !githubTeamSlugPattern.MatchString(slug) {
			return "request_review request names an invalid team slug " + strconv.Quote(slug)
		}
	}
	return ""
}

// RequestReviewers asks users and/or teams to review a pull request. An empty
// request is a no-op.
func (c *Client) RequestReviewers(ctx context.Context, repo string, number int, reviewers, teams []string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if len(reviewers) == 0 && len(teams) == 0 {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindReviewRequest,
		Target: strconv.Itoa(number),
		Inputs: map[string]string{
			"reviewers":      strings.Join(reviewers, ","),
			"team_reviewers": strings.Join(teams, ","),
		},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.client.PullRequests.RequestReviewers(ctx, owner, repoName, number, gh.ReviewersRequest{
			Reviewers:     reviewers,
			TeamReviewers: teams,
		})
		return effects.Result{Provenance: owner + "/" + repoName + "#" + strconv.Itoa(number)}, apiErr
	})
	return err
}

package reviewgate

import (
	"context"
	"errors"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// githubReviewsPerPage is the page size for the review listing.
const githubReviewsPerPage = 100

// GitHubReviews lists every review on owner/repo#number, all pages. A 404
// (not a pull request, or not visible) is no reviews.
func GitHubReviews(ctx context.Context, client *gh.Client, owner, repo string, number int) ([]Review, error) {
	if client == nil {
		return nil, errors.New("no GitHub client")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts := &gh.ListOptions{PerPage: githubReviewsPerPage}
	var out []Review
	for {
		page, resp, err := client.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			if isNotFound(resp, err) {
				return nil, nil
			}
			return nil, err
		}
		for _, rv := range page {
			if rv == nil {
				continue
			}
			out = append(out, Review{
				ID:          rv.GetID(),
				Reviewer:    rv.GetUser().GetLogin(),
				State:       rv.GetState(),
				CommitID:    rv.GetCommitID(),
				SubmittedAt: rv.GetSubmittedAt().Time,
				URL:         rv.GetHTMLURL(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// GitHubWriter answers WriterFunc from the collaborator permission endpoint:
// admin, maintain and write count. A 404 (not a collaborator) is not a
// writer; any other failure is returned so Evaluate fails closed.
func GitHubWriter(client *gh.Client, owner, repo string) WriterFunc {
	return func(ctx context.Context, login string) (bool, error) {
		if client == nil {
			return false, errors.New("no GitHub client")
		}
		level, resp, err := client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
		if err != nil {
			if isNotFound(resp, err) {
				return false, nil
			}
			return false, err
		}
		return WriteOrAbove(level.GetPermission()) || WriteOrAbove(level.GetRoleName()), nil
	}
}

// WriteOrAbove reports whether a permission or role name grants write.
func WriteOrAbove(permission string) bool {
	switch strings.ToLower(strings.TrimSpace(permission)) {
	case "admin", "owner", "maintain", "write":
		return true
	}
	return false
}

// CheckGitHub reads owner/repo#number's reviews and returns its open change
// requests from writers.
func CheckGitHub(ctx context.Context, client *gh.Client, owner, repo string, number int) ([]Block, error) {
	reviews, err := GitHubReviews(ctx, client, owner, repo, number)
	if err != nil {
		return nil, err
	}
	return Evaluate(ctx, reviews, GitHubWriter(client, owner, repo)), nil
}

func isNotFound(resp *gh.Response, err error) bool {
	if resp != nil && resp.Response != nil && resp.StatusCode == http.StatusNotFound {
		return true
	}
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

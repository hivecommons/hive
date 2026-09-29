package github

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/hivecommons/hive/pkg/effects"
)

// AddSubIssue links an already-created issue as a GitHub sub-issue of
// parentNumber, giving the parent a native sub-issue list and completion
// progress bar (hivecommons/hive#9435) instead of a plain-text "Part of #N"
// reference. subIssueID is the child issue's numeric database ID (GitHub
// calls it `id`), NOT its repo-scoped issue number — the sub-issues API
// requires the former.
//
// go-github v72 has no typed wrapper for this endpoint, so the request is
// built and sent through the same NewRequest/Do escape hatch used elsewhere
// in this package (see graphql.go, tokenscopes.go) for calls the library
// does not yet cover.
func (c *Client) AddSubIssue(ctx context.Context, repo string, parentNumber int, subIssueID int64) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	if parentNumber <= 0 || subIssueID <= 0 {
		return fmt.Errorf("AddSubIssue: parent issue number and sub-issue id are required")
	}
	owner, repoName := c.splitRepo(repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		return fmt.Errorf("AddSubIssue: %w", err)
	}
	path := fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues", owner, repoName, parentNumber)
	payload := struct {
		SubIssueID int64 `json:"sub_issue_id"`
	}{SubIssueID: subIssueID}

	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindSubIssueLink,
		Target: strconv.Itoa(parentNumber),
		Inputs: map[string]string{"sub_issue_id": strconv.FormatInt(subIssueID, 10)},
	}, func(ctx context.Context) (effects.Result, error) {
		req, reqErr := c.client.NewRequest(http.MethodPost, path, payload)
		if reqErr != nil {
			return effects.Result{}, reqErr
		}
		if _, doErr := c.client.Do(ctx, req, nil); doErr != nil {
			return effects.Result{}, doErr
		}
		return effects.Result{Provenance: fmt.Sprintf("%s/%s#%d sub_issue_id=%d", owner, repoName, parentNumber, subIssueID)}, nil
	})
	if err != nil {
		return fmt.Errorf("linking issue id %d as a sub-issue of %s/%s#%d: %w", subIssueID, owner, repoName, parentNumber, err)
	}
	return nil
}

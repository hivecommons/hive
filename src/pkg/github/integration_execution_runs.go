package github

import (
	"context"
	"fmt"
	"net/url"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const integrationExecutionRunPage = 100

// ReadIntegrationExecutionRuns discovers only the enrolled workflow's bounded
// cohort. A truncated page is unknown, never evidence of absence or permission
// to repeat a dispatch. Receipts subsequently validate each selected run.
func (c *Client) ReadIntegrationExecutionRuns(ctx context.Context, repo string, workflowID int64, after time.Time) ([]*gh.WorkflowRun, error) {
	if c == nil || c.client == nil || !integrationPromotionRepo(repo) || workflowID <= 0 || after.IsZero() {
		return nil, fmt.Errorf("incomplete exact integration workflow discovery")
	}
	var response struct {
		Total int               `json:"total_count"`
		Runs  []*gh.WorkflowRun `json:"workflow_runs"`
	}
	path := fmt.Sprintf("repos/%s/actions/workflows/%d/runs?per_page=%d&created=%s",
		repo, workflowID, integrationExecutionRunPage, url.QueryEscape(">="+after.UTC().Format(time.RFC3339)))
	if err := c.integrationCandidateRead(ctx, path, &response); err != nil {
		return nil, err
	}
	if response.Total != len(response.Runs) || response.Total > integrationExecutionRunPage {
		return nil, fmt.Errorf("integration workflow discovery truncated; cohort unknown")
	}
	return response.Runs, nil
}

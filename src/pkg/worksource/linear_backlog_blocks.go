package worksource

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Linear writes the review-backlog block sweep needs (hivecommons/hive#13833):
// lifting the `blocked` label when the reviewed PR merges, and cancelling the
// item when it closes unmerged.

const linearIssueBlockQuery = `query($id: String!) {
  issue(id: $id) {
    id
    labels(first: 50) { nodes { id name } }
    team { states(first: 100) { nodes { id name type } } }
  }
}`

const linearIssueRemoveLabelMutation = `mutation($id: String!, $labelId: String!) {
  issueRemoveLabel(id: $id, labelId: $labelId) { success }
}`

const linearIssueUpdateStateMutation = `mutation($id: String!, $input: IssueUpdateInput!) {
  issueUpdate(id: $id, input: $input) { success }
}`

type linearIssueBlockResponse struct {
	Data struct {
		Issue *struct {
			ID     string `json:"id"`
			Labels struct {
				Nodes []linearNamedNode `json:"nodes"`
			} `json:"labels"`
			Team struct {
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	} `json:"data"`
}

func (s *LinearSource) issueBlockInfo(ctx context.Context, issueID string) (*linearIssueBlockResponse, error) {
	issueID = strings.TrimSpace(issueID)
	if issueID == "" {
		return nil, fmt.Errorf("linear: issue id is required")
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearIssueBlockQuery, map[string]interface{}{"id": issueID})
	if err != nil {
		return nil, fmt.Errorf("linear: read issue %s: %w", issueID, err)
	}
	var resp linearIssueBlockResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("linear: read issue %s: decode response: %w", issueID, err)
	}
	if resp.Data.Issue == nil {
		return nil, fmt.Errorf("linear: issue %s not found", issueID)
	}
	return &resp, nil
}

// RemoveIssueLabel removes the label named name from the issue. An issue that
// does not carry it is left alone: the desired end state already holds.
func (s *LinearSource) RemoveIssueLabel(ctx context.Context, issueID, name string) error {
	info, err := s.issueBlockInfo(ctx, issueID)
	if err != nil {
		return err
	}
	for _, l := range info.Data.Issue.Labels.Nodes {
		if !strings.EqualFold(l.Name, strings.TrimSpace(name)) {
			continue
		}
		raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearIssueRemoveLabelMutation, map[string]interface{}{"id": info.Data.Issue.ID, "labelId": l.ID})
		if err != nil {
			return fmt.Errorf("linear: issueRemoveLabel on %s: %w", issueID, err)
		}
		var resp struct {
			Data struct {
				IssueRemoveLabel struct {
					Success bool `json:"success"`
				} `json:"issueRemoveLabel"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return fmt.Errorf("linear: issueRemoveLabel on %s: decode response: %w", issueID, err)
		}
		if !resp.Data.IssueRemoveLabel.Success {
			return fmt.Errorf("linear: issueRemoveLabel on %s reported success=false", issueID)
		}
	}
	return nil
}

// CancelIssue moves the issue to the first workflow state of type
// "canceled" on its team.
func (s *LinearSource) CancelIssue(ctx context.Context, issueID string) error {
	info, err := s.issueBlockInfo(ctx, issueID)
	if err != nil {
		return err
	}
	stateID := ""
	for _, st := range info.Data.Issue.Team.States.Nodes {
		if strings.EqualFold(st.Type, "canceled") {
			stateID = st.ID
			break
		}
	}
	if stateID == "" {
		return fmt.Errorf("linear: issue %s's team has no canceled workflow state", issueID)
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearIssueUpdateStateMutation, map[string]interface{}{"id": info.Data.Issue.ID, "input": map[string]interface{}{"stateId": stateID}})
	if err != nil {
		return fmt.Errorf("linear: issueUpdate on %s: %w", issueID, err)
	}
	var resp struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("linear: issueUpdate on %s: decode response: %w", issueID, err)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("linear: issueUpdate on %s reported success=false", issueID)
	}
	return nil
}

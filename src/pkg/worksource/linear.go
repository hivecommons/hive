// Package worksource — Linear GraphQL adapter.
//
// Phase 1 (read-only): hive reads issues from Linear and assigns them to agents.
// Linear's native GitHub integration closes the ticket when a referenced PR
// merges, so no write-back is needed in Phase 1.
//
// Security note: Linear egress from AGENTS is gated as of #4492 component F —
// NeedsMITM now intercepts api.linear.app and pkg/proxy/linear_rules.go
// enforces ACMM by GraphQL operation name, fail-closed on anything it cannot
// classify. The calls in THIS file originate from the hive control plane, not
// an agent, so they are exempt (internalCallerName) and remain read-only.
package worksource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultLinearBaseURL is Linear's single GraphQL endpoint.
const defaultLinearBaseURL = "https://api.linear.app/graphql"

// linearPageSize is the number of issues requested per GraphQL page.
const linearPageSize = 100

// defaultLinearStates is used when a team's States list is empty.
var defaultLinearStates = []string{"Todo", "In Progress", "Backlog"}

// LinearTeamConfig maps one Linear team to the GitHub repo its agents clone.
type LinearTeamConfig struct {
	Key    string   // e.g. "ENG"
	Repo   string   // GitHub repo agents clone, e.g. "my-org/my-repo"
	States []string // Linear state names to include, e.g. ["Todo","Backlog"]
}

// LinearConfig configures the Linear work source adapter.
type LinearConfig struct {
	APIKey     string // LINEAR_API_KEY
	Teams      []LinearTeamConfig
	HoldLabels []string // label names that gate an issue (like GitHub "hold")
	// BaseURL overrides the Linear API endpoint (default: "https://api.linear.app/graphql")
	BaseURL string
}

// LinearSource is a read-only WorkSource backed by Linear's GraphQL API.
type LinearSource struct {
	cfg    LinearConfig
	client *http.Client
}

// NewLinearSource constructs a LinearSource from cfg. A nil httpClient falls
// back to a client with a sane timeout.
func NewLinearSource(cfg LinearConfig, httpClient *http.Client) *LinearSource {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &LinearSource{cfg: cfg, client: httpClient}
}

// SourceType implements WorkSource.
func (s *LinearSource) SourceType() string { return "linear" }

const linearIssuesQuery = `query($teamKey: String!, $states: [String!], $cursor: String) {
  issues(
    filter: {
      team: { key: { eq: $teamKey } }
      state: { name: { in: $states } }
    }
    first: 100
    after: $cursor
  ) {
    pageInfo { hasNextPage endCursor }
    nodes {
      id
      identifier
      title
      url
      createdAt
      updatedAt
      priority
      state { name }
      assignee { displayName }
      labels { nodes { name } }
      team { key }
    }
  }
}`

type linearGraphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type linearIssueNode struct {
	ID         string    `json:"id"`
	Identifier string    `json:"identifier"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Priority   int       `json:"priority"`
	State      struct {
		Name string `json:"name"`
	} `json:"state"`
	Assignee *struct {
		DisplayName string `json:"displayName"`
	} `json:"assignee"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Team struct {
		Key string `json:"key"`
	} `json:"team"`
}

type linearGraphQLResponse struct {
	Data struct {
		Issues struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"issues"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// linearPriorityString maps Linear's numeric priority to hive's normalized
// priority strings. 0=urgent 1=high 2=medium 3=low 4=no priority.
func linearPriorityString(p int) string {
	switch p {
	case 0:
		return "urgent"
	case 1:
		return "high"
	case 2:
		return "medium"
	case 3:
		return "low"
	default:
		return "none"
	}
}

// ListIssues implements WorkSource. It enumerates actionable issues across
// all configured teams, skipping any issue carrying a hold label.
func (s *LinearSource) ListIssues(ctx context.Context) ([]Issue, error) {
	hold := make(map[string]bool, len(s.cfg.HoldLabels))
	for _, l := range s.cfg.HoldLabels {
		hold[l] = true
	}

	var out []Issue
	for _, team := range s.cfg.Teams {
		states := team.States
		if len(states) == 0 {
			states = defaultLinearStates
		}
		nodes, err := s.fetchTeamIssues(ctx, team.Key, states)
		if err != nil {
			return nil, fmt.Errorf("linear: team %s: %w", team.Key, err)
		}
	node:
		for _, n := range nodes {
			labels := make([]string, 0, len(n.Labels.Nodes))
			for _, l := range n.Labels.Nodes {
				if hold[l.Name] {
					continue node
				}
				labels = append(labels, l.Name)
			}
			var assignees []string
			if n.Assignee != nil && n.Assignee.DisplayName != "" {
				assignees = []string{n.Assignee.DisplayName}
			}
			out = append(out, Issue{
				SourceType: "linear",
				Repo:       team.Repo,
				ExternalID: n.Identifier,
				Title:      n.Title,
				Labels:     labels,
				Assignees:  assignees,
				Priority:   linearPriorityString(n.Priority),
				State:      n.State.Name,
				CreatedAt:  n.CreatedAt,
				UpdatedAt:  n.UpdatedAt,
				URL:        n.URL,
			})
		}
	}
	return out, nil
}

// fetchTeamIssues pages through the Linear issues query for one team.
func (s *LinearSource) fetchTeamIssues(ctx context.Context, teamKey string, states []string) ([]linearIssueNode, error) {
	var nodes []linearIssueNode
	var cursor *string
	for {
		vars := map[string]interface{}{
			"teamKey": teamKey,
			"states":  states,
			"cursor":  cursor,
		}
		resp, err := s.doQuery(ctx, vars)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, resp.Data.Issues.Nodes...)
		if !resp.Data.Issues.PageInfo.HasNextPage {
			return nodes, nil
		}
		c := resp.Data.Issues.PageInfo.EndCursor
		cursor = &c
	}
}

func (s *LinearSource) doQuery(ctx context.Context, vars map[string]interface{}) (*linearGraphQLResponse, error) {
	baseURL := s.cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultLinearBaseURL
	}
	body, err := json.Marshal(linearGraphQLRequest{Query: linearIssuesQuery, Variables: vars})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", s.cfg.APIKey)

	httpResp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", httpResp.StatusCode, string(raw))
	}
	var resp linearGraphQLResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", resp.Errors[0].Message)
	}
	return &resp, nil
}

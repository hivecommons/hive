// Package worksource — GitHub Projects v2 adapter.
//
// Uses the GitHub GraphQL API projectV2 query to enumerate issues in a GitHub
// Project board. Same auth surface as api.github.com; no new ACMM rules needed.
package worksource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const githubProjectsDefaultBaseURL = "https://api.github.com"

// GitHubProjectsConfig configures the GitHub Projects v2 work source.
type GitHubProjectsConfig struct {
	// Token is the GitHub personal access token (needs read:project scope).
	Token string
	// Org is the GitHub organization that owns the project.
	Org string
	// ProjectNumber is the project's number (visible in the project URL).
	ProjectNumber int
	// States filters items by the project's Status field value. Empty = all.
	States []string
	// PriorityField is the name of the project's priority custom field.
	// Empty = no priority mapping.
	PriorityField string
	// IterationField is the name of the project's iteration/sprint field.
	// When set, only items in the current iteration are returned.
	IterationField string
	// DefaultRepo is the GitHub repo (owner/name) to assign to all issues
	// when the issue's own repo cannot be determined from the project item.
	DefaultRepo string
	// BaseURL overrides the GitHub API base (default: "https://api.github.com").
	BaseURL string
}

type githubProjectsSource struct {
	cfg    GitHubProjectsConfig
	client *http.Client
}

// NewGitHubProjectsSource builds a WorkSource backed by GitHub Projects v2.
func NewGitHubProjectsSource(cfg GitHubProjectsConfig) WorkSource {
	return &githubProjectsSource{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
}

func (s *githubProjectsSource) SourceType() string { return "github_projects" }

const githubProjectsQuery = `query($org: String!, $number: Int!, $cursor: String) {
  organization(login: $org) {
    projectV2(number: $number) {
      items(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          type
          fieldValues(first: 20) {
            nodes {
              ... on ProjectV2ItemFieldSingleSelectValue {
                name
                field { ... on ProjectV2SingleSelectField { name } }
              }
              ... on ProjectV2ItemFieldIterationValue {
                title
                startDate
                duration
                field { ... on ProjectV2IterationField { name } }
              }
            }
          }
          content {
            ... on Issue {
              number
              title
              url
              createdAt
              updatedAt
              state
              author { login }
              labels(first: 20) { nodes { name } }
              assignees(first: 10) { nodes { login } }
              repository { nameWithOwner }
            }
          }
        }
      }
    }
  }
}`

type gqlFieldValue struct {
	Name      string `json:"name"`
	Title     string `json:"title"`
	StartDate string `json:"startDate"`
	Duration  int    `json:"duration"`
	Field     struct {
		Name string `json:"name"`
	} `json:"field"`
}

type gqlItem struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	FieldValues struct {
		Nodes []gqlFieldValue `json:"nodes"`
	} `json:"fieldValues"`
	Content struct {
		Number    int       `json:"number"`
		Title     string    `json:"title"`
		URL       string    `json:"url"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
		State     string    `json:"state"`
		Author    struct {
			Login string `json:"login"`
		} `json:"author"`
		Labels struct {
			Nodes []struct {
				Name string `json:"name"`
			} `json:"nodes"`
		} `json:"labels"`
		Assignees struct {
			Nodes []struct {
				Login string `json:"login"`
			} `json:"nodes"`
		} `json:"assignees"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	} `json:"content"`
}

type gqlResponse struct {
	Data struct {
		Organization struct {
			ProjectV2 struct {
				Items struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []gqlItem `json:"nodes"`
				} `json:"items"`
			} `json:"projectV2"`
		} `json:"organization"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (s *githubProjectsSource) ListIssues(ctx context.Context) ([]Issue, error) {
	baseURL := s.cfg.BaseURL
	if baseURL == "" {
		baseURL = githubProjectsDefaultBaseURL
	}
	endpoint := strings.TrimSuffix(baseURL, "/") + "/graphql"

	var issues []Issue
	var cursor *string
	for {
		resp, err := s.queryPage(ctx, endpoint, cursor)
		if err != nil {
			return nil, err
		}
		for _, item := range resp.Data.Organization.ProjectV2.Items.Nodes {
			if issue, ok := s.mapItem(item); ok {
				issues = append(issues, issue)
			}
		}
		pi := resp.Data.Organization.ProjectV2.Items.PageInfo
		if !pi.HasNextPage {
			break
		}
		c := pi.EndCursor
		cursor = &c
	}
	return issues, nil
}

func (s *githubProjectsSource) queryPage(ctx context.Context, endpoint string, cursor *string) (*gqlResponse, error) {
	vars := map[string]any{
		"org":    s.cfg.Org,
		"number": s.cfg.ProjectNumber,
		"cursor": cursor,
	}
	body, err := json.Marshal(map[string]any{
		"query":     githubProjectsQuery,
		"variables": vars,
	})
	if err != nil {
		return nil, fmt.Errorf("worksource/github_projects: marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("worksource/github_projects: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("worksource/github_projects: request failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worksource/github_projects: unexpected status %d", httpResp.StatusCode)
	}
	var resp gqlResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("worksource/github_projects: decode response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("worksource/github_projects: graphql error: %s", resp.Errors[0].Message)
	}
	return &resp, nil
}

// mapItem converts a project item into a normalized Issue. It returns false
// when the item should be skipped (non-issue, filtered out by state, or not
// in the current iteration).
func (s *githubProjectsSource) mapItem(item gqlItem) (Issue, bool) {
	if item.Type != "ISSUE" {
		return Issue{}, false
	}

	status := fieldValueByName(item.FieldValues.Nodes, "Status")
	if len(s.cfg.States) > 0 {
		matched := false
		for _, want := range s.cfg.States {
			if strings.EqualFold(status, want) {
				matched = true
				break
			}
		}
		if !matched {
			return Issue{}, false
		}
	}

	if s.cfg.IterationField != "" && !inCurrentIteration(item.FieldValues.Nodes, s.cfg.IterationField) {
		return Issue{}, false
	}

	priority := "none"
	if s.cfg.PriorityField != "" {
		priority = normalizePriority(fieldValueByName(item.FieldValues.Nodes, s.cfg.PriorityField))
	}

	repo := item.Content.Repository.NameWithOwner
	if repo == "" {
		repo = s.cfg.DefaultRepo
	}

	labels := make([]string, 0, len(item.Content.Labels.Nodes))
	for _, l := range item.Content.Labels.Nodes {
		labels = append(labels, l.Name)
	}
	assignees := make([]string, 0, len(item.Content.Assignees.Nodes))
	for _, a := range item.Content.Assignees.Nodes {
		assignees = append(assignees, a.Login)
	}

	return Issue{
		SourceType: "github_projects",
		Repo:       repo,
		ExternalID: strconv.Itoa(item.Content.Number),
		Number:     item.Content.Number,
		Title:      item.Content.Title,
		Author:     item.Content.Author.Login,
		Labels:     labels,
		Assignees:  assignees,
		Priority:   priority,
		State:      item.Content.State,
		CreatedAt:  item.Content.CreatedAt,
		UpdatedAt:  item.Content.UpdatedAt,
		URL:        item.Content.URL,
	}, true
}

// fieldValueByName returns the single-select value for the named field.
func fieldValueByName(values []gqlFieldValue, fieldName string) string {
	for _, v := range values {
		if strings.EqualFold(v.Field.Name, fieldName) && v.Name != "" {
			return v.Name
		}
	}
	return ""
}

// inCurrentIteration reports whether the item's iteration field value covers
// today's date (startDate <= now < startDate + duration days).
func inCurrentIteration(values []gqlFieldValue, fieldName string) bool {
	for _, v := range values {
		if !strings.EqualFold(v.Field.Name, fieldName) || v.StartDate == "" {
			continue
		}
		start, err := time.Parse("2006-01-02", v.StartDate)
		if err != nil {
			continue
		}
		end := start.AddDate(0, 0, v.Duration)
		now := time.Now().UTC()
		if !now.Before(start) && now.Before(end) {
			return true
		}
	}
	return false
}

// normalizePriority maps project priority field values to normalized strings.
func normalizePriority(value string) string {
	switch strings.ToLower(value) {
	case "p0", "urgent", "critical":
		return "urgent"
	case "p1", "high":
		return "high"
	case "p2", "medium":
		return "medium"
	case "p3", "low":
		return "low"
	default:
		return "none"
	}
}

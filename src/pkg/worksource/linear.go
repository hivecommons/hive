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
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/github"
)

// defaultLinearBaseURL is Linear's single GraphQL endpoint.
const defaultLinearBaseURL = "https://api.linear.app/graphql"

// defaultLinearStates is used when a team's States list is empty.
var defaultLinearStates = []string{"Todo", "In Progress", "Backlog"}

// LinearTeamConfig maps one Linear team to the GitHub repo its agents clone.
type LinearTeamConfig struct {
	Key      string                // e.g. "ENG"
	Repo     string                // default GitHub repo agents clone, e.g. "my-org/my-repo"
	States   []string              // Linear state names to include, e.g. ["Todo","Backlog"]
	Projects []LinearProjectConfig // optional project filter/routing
	Cycles   string                // "" or "current"
}

type LinearProjectConfig struct {
	Name string
	Repo string
}

// LinearConfig configures the Linear work source adapter.
type LinearConfig struct {
	APIKey     string // LINEAR_API_KEY
	Teams      []LinearTeamConfig
	HoldLabels []string // label names that gate an issue (like GitHub "hold")
	// BaseURL overrides the Linear API endpoint (default: "https://api.linear.app/graphql")
	BaseURL string
	// ViewerID, when non-empty, narrows enumeration to issues assigned or
	// delegated to this Linear user id — the installed agent app's per-
	// workspace viewer.id (RFC #4492 Part 2, component E). "Delegate this
	// issue to Hive" then IS the enumeration filter, replacing state-name
	// matching with Linear's own assignment UX. The filter matches EITHER
	// assignee or delegate: Linear's agent platform sets an app given an
	// issue as its `delegate` (humans keep `assignee` ownership), while the
	// RFC and older workspaces phrase it as assignment — either field
	// pointing at the app user means "the hive owns this".
	ViewerID string
	Logger   *slog.Logger
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

// linearIssuesQuery pages at `first: 100`, Linear's maximum page size. The page
// size is part of the query document rather than a separate constant so it
// cannot be changed in one place and left unchanged in the document that
// actually runs.
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
      description
      priority
      state { name }
      assignee { displayName }
      labels { nodes { name } }
      project { name }
      cycle { name startsAt endsAt }
      children { nodes { id } }
      inverseRelations(first: 100) {
        nodes {
          type
          issue {
            identifier
            url
            state { type }
            team { key }
            project { name }
          }
        }
      }
    }
  }
}`

// linearAssignedIssuesQuery is linearIssuesQuery narrowed to issues whose
// assignee OR delegate is the given user id. A separate document rather than
// a dynamically-built filter so both shapes stay readable.
const linearAssignedIssuesQuery = `query($teamKey: String!, $states: [String!], $viewerID: ID, $cursor: String) {
  issues(
    filter: {
      team: { key: { eq: $teamKey } }
      state: { name: { in: $states } }
      or: [
        { assignee: { id: { eq: $viewerID } } }
        { delegate: { id: { eq: $viewerID } } }
      ]
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
      description
      priority
      state { name }
      assignee { displayName }
      labels { nodes { name } }
      team { key }
      project { name }
      cycle { name startsAt endsAt }
      children { nodes { id } }
      inverseRelations(first: 100) {
        nodes {
          type
          issue {
            identifier
            url
            state { type }
            team { key }
            project { name }
          }
        }
      }
    }
  }
}`

type linearGraphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type linearIssueNode struct {
	ID          string    `json:"id"`
	Identifier  string    `json:"identifier"`
	Title       string    `json:"title"`
	URL         string    `json:"url"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Description string    `json:"description"`
	Priority    int       `json:"priority"`
	State       struct {
		Name string `json:"name"`
		Type string `json:"type"`
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
	Project *struct {
		Name string `json:"name"`
	} `json:"project"`
	Cycle *struct {
		Name     string `json:"name"`
		StartsAt string `json:"startsAt"`
		EndsAt   string `json:"endsAt"`
	} `json:"cycle"`
	Children struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
	} `json:"children"`
	InverseRelations struct {
		Nodes []struct {
			Type  string             `json:"type"`
			Issue linearRelatedIssue `json:"issue"`
		} `json:"nodes"`
	} `json:"inverseRelations"`
}

type linearRelatedIssue struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
	State      struct {
		Type string `json:"type"`
	} `json:"state"`
	Team struct {
		Key string `json:"key"`
	} `json:"team"`
	Project *struct {
		Name string `json:"name"`
	} `json:"project"`
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
			if team.Cycles == "current" && !linearIssueInCurrentCycle(n, time.Now()) {
				continue
			}
			repo := team.Repo
			if len(team.Projects) > 0 {
				projectRepo, ok := linearProjectRepo(team, n)
				if !ok {
					continue
				}
				repo = projectRepo
			}
			labels := make([]string, 0, len(n.Labels.Nodes))
			for _, l := range n.Labels.Nodes {
				if github.HasHoldLabelWith([]string{l.Name}, s.cfg.HoldLabels) {
					continue node
				}
				labels = append(labels, l.Name)
			}
			var assignees []string
			if n.Assignee != nil && n.Assignee.DisplayName != "" {
				assignees = []string{n.Assignee.DisplayName}
			}
			issue := Issue{
				SourceType: "linear",
				Repo:       repo,
				ExternalID: n.Identifier,
				Title:      n.Title,
				Labels:     labels,
				Assignees:  assignees,
				IsTracker:  github.IsTrackerIssue(n.Title, labels, n.Description) || len(n.Children.Nodes) > 0,
				Priority:   linearPriorityString(n.Priority),
				State:      n.State.Name,
				CreatedAt:  n.CreatedAt,
				UpdatedAt:  n.UpdatedAt,
				URL:        n.URL,
				DependsOn:  s.linearDependencies(team, n),
			}
			if RefFromIssue(issue).Key() == "" {
				if s.cfg.Logger != nil {
					s.cfg.Logger.Warn("linear: skipping issue with empty work identity", "team", team.Key, "external_id", n.Identifier, "repo", repo)
				}
				continue
			}
			out = append(out, issue)
		}
	}
	return out, nil
}

func (s *LinearSource) linearDependencies(fallback LinearTeamConfig, n linearIssueNode) []Dependency {
	deps := make([]Dependency, 0, len(n.InverseRelations.Nodes))
	for _, relation := range n.InverseRelations.Nodes {
		if !strings.EqualFold(relation.Type, "blocks") {
			continue
		}
		blocker := relation.Issue
		ref := Ref{
			SourceType: "linear",
			Repo:       s.linearRelatedRepo(fallback, blocker),
			ExternalID: blocker.Identifier,
			URL:        blocker.URL,
		}
		if ref.Key() == "" {
			continue
		}
		terminal := strings.EqualFold(blocker.State.Type, "completed") ||
			strings.EqualFold(blocker.State.Type, "canceled")
		deps = append(deps, Dependency{Ref: ref, Resolved: terminal})
	}
	return deps
}

func (s *LinearSource) linearRelatedRepo(fallback LinearTeamConfig, issue linearRelatedIssue) string {
	for _, team := range s.cfg.Teams {
		if !strings.EqualFold(team.Key, issue.Team.Key) {
			continue
		}
		if issue.Project != nil {
			for _, project := range team.Projects {
				if strings.EqualFold(project.Name, issue.Project.Name) {
					if project.Repo != "" {
						return project.Repo
					}
					return team.Repo
				}
			}
		}
		return team.Repo
	}
	// Relations may cross into a Linear team that is not itself enumerated by
	// this hive. Scope that diagnostic identity to the dependent's routed repo;
	// the native identifier still keeps the blocker distinct.
	return fallback.Repo
}

func linearProjectRepo(team LinearTeamConfig, n linearIssueNode) (string, bool) {
	if n.Project == nil {
		return "", false
	}
	for _, p := range team.Projects {
		if strings.EqualFold(p.Name, n.Project.Name) {
			if p.Repo != "" {
				return p.Repo, true
			}
			return team.Repo, true
		}
	}
	return "", false
}

func linearIssueInCurrentCycle(n linearIssueNode, now time.Time) bool {
	if n.Cycle == nil {
		return false
	}
	start, ok := parseLinearCycleTime(n.Cycle.StartsAt, false)
	if !ok || now.Before(start) {
		return false
	}
	end, ok := parseLinearCycleTime(n.Cycle.EndsAt, true)
	if !ok {
		return true
	}
	return now.Before(end)
}

func parseLinearCycleTime(raw string, endOfDay bool) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, true
	}
	if day, err := time.Parse("2006-01-02", raw); err == nil {
		if endOfDay {
			return day.Add(24 * time.Hour), true
		}
		return day, true
	}
	return time.Time{}, false
}

// fetchTeamIssues pages through the Linear issues query for one team.
func (s *LinearSource) fetchTeamIssues(ctx context.Context, teamKey string, states []string) ([]linearIssueNode, error) {
	query := linearIssuesQuery
	var nodes []linearIssueNode
	var cursor *string
	for {
		vars := map[string]interface{}{
			"teamKey": teamKey,
			"states":  states,
			"cursor":  cursor,
		}
		if s.cfg.ViewerID != "" {
			query = linearAssignedIssuesQuery
			vars["viewerID"] = s.cfg.ViewerID
		}
		resp, err := s.doQuery(ctx, query, vars)
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

func (s *LinearSource) doQuery(ctx context.Context, query string, vars map[string]interface{}) (*linearGraphQLResponse, error) {
	baseURL := s.cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultLinearBaseURL
	}
	body, err := json.Marshal(linearGraphQLRequest{Query: query, Variables: vars})
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

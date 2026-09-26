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

	"github.com/hivecommons/hive/pkg/github"
)

// defaultLinearBaseURL is Linear's single GraphQL endpoint.
const defaultLinearBaseURL = "https://api.linear.app/graphql"

// defaultLinearStates is used when a team's States list is empty.
var defaultLinearStates = []string{"Todo", "In Progress", "Backlog"}

// defaultLinearHoldLabels mirrors the GitHub hold-label defaults without
// reading the mutable github.HoldLabels slice during Linear tests.
var defaultLinearHoldLabels = []string{"hold", "on-hold", "hold/review"}

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
	// Transitions maps Hive design status names to Linear state names or ids.
	Transitions map[string]string
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

const linearDesignIssueLabelsQuery = `query($identifier: String!) {
  issue(id: $identifier) {
    id
    labels { nodes { id name } }
  }
  issueLabels(first: 250) {
    nodes { id name }
  }
}`

const linearDesignIssueUpdateLabels = `mutation($id: String!, $labelIds: [String!]) {
  issueUpdate(id: $id, input: { labelIds: $labelIds }) { success }
}`

const linearDesignCommentCreate = `mutation($issueId: String!, $body: String!) {
  commentCreate(input: { issueId: $issueId, body: $body }) { success }
}`

const linearDesignIssueStatesQuery = `query($identifier: String!) {
  issue(id: $identifier) {
    id
    team { states { nodes { id name } } }
  }
}`

const linearDesignIssueUpdateState = `mutation($id: String!, $stateId: String!) {
  issueUpdate(id: $id, input: { stateId: $stateId }) { success }
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
				if linearHasHoldLabel(l.Name, s.cfg.HoldLabels) {
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

func (s *LinearSource) AddLabel(ctx context.Context, ref Ref, label string) error {
	return s.updateDesignLabels(ctx, ref, strings.TrimSpace(label), true)
}

func (s *LinearSource) RemoveLabel(ctx context.Context, ref Ref, label string) error {
	return s.updateDesignLabels(ctx, ref, strings.TrimSpace(label), false)
}

func (s *LinearSource) AddComment(ctx context.Context, ref Ref, body string) error {
	issueID, _, err := s.designIssueLabels(ctx, ref)
	if err != nil {
		return err
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearDesignCommentCreate, map[string]interface{}{"issueId": issueID, "body": body})
	if err != nil {
		return fmt.Errorf("worksource/linear: add comment: %w", err)
	}
	var resp struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("worksource/linear: decode comment mutation: %w", err)
	}
	if !resp.Data.CommentCreate.Success {
		return fmt.Errorf("worksource/linear: comment mutation reported failure")
	}
	return nil
}

func (s *LinearSource) TransitionStatus(ctx context.Context, ref Ref, status string) error {
	if s == nil {
		return fmt.Errorf("worksource/linear: source unavailable")
	}
	if len(s.cfg.Transitions) == 0 {
		return ErrStatusTransitionUnsupported
	}
	target := strings.TrimSpace(status)
	if mapped := strings.TrimSpace(s.cfg.Transitions[target]); mapped != "" {
		target = mapped
	}
	if target == "" {
		return ErrStatusTransitionUnsupported
	}
	issueID, stateID, err := s.designIssueState(ctx, ref, target)
	if err != nil {
		return err
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearDesignIssueUpdateState, map[string]interface{}{"id": issueID, "stateId": stateID})
	if err != nil {
		return fmt.Errorf("worksource/linear: update status: %w", err)
	}
	var resp struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("worksource/linear: decode status mutation: %w", err)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("worksource/linear: status mutation reported failure")
	}
	return nil
}

func (s *LinearSource) updateDesignLabels(ctx context.Context, ref Ref, label string, add bool) error {
	if label == "" {
		return nil
	}
	issueID, labels, err := s.designIssueLabels(ctx, ref)
	if err != nil {
		return err
	}
	labelIDs := make([]string, 0, len(labels.current)+1)
	have := false
	for _, l := range labels.current {
		if strings.EqualFold(l.name, label) {
			have = true
			if add {
				labelIDs = append(labelIDs, l.id)
			}
			continue
		}
		labelIDs = append(labelIDs, l.id)
	}
	if add && !have {
		id := ""
		for _, l := range labels.all {
			if strings.EqualFold(l.name, label) {
				id = l.id
				break
			}
		}
		if id == "" {
			return fmt.Errorf("worksource/linear: label %q does not exist", label)
		}
		labelIDs = append(labelIDs, id)
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearDesignIssueUpdateLabels, map[string]interface{}{"id": issueID, "labelIds": labelIDs})
	if err != nil {
		return fmt.Errorf("worksource/linear: update labels: %w", err)
	}
	var resp struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return fmt.Errorf("worksource/linear: decode label mutation: %w", err)
	}
	if !resp.Data.IssueUpdate.Success {
		return fmt.Errorf("worksource/linear: label mutation reported failure")
	}
	return nil
}

type linearDesignLabel struct {
	id   string
	name string
}

type linearDesignLabels struct {
	current []linearDesignLabel
	all     []linearDesignLabel
}

func (s *LinearSource) designIssueLabels(ctx context.Context, ref Ref) (string, linearDesignLabels, error) {
	if s == nil {
		return "", linearDesignLabels{}, fmt.Errorf("worksource/linear: source unavailable")
	}
	identifier := strings.TrimSpace(ref.ExternalID)
	if identifier == "" {
		return "", linearDesignLabels{}, fmt.Errorf("worksource/linear: external id is required")
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearDesignIssueLabelsQuery, map[string]interface{}{"identifier": identifier})
	if err != nil {
		return "", linearDesignLabels{}, fmt.Errorf("worksource/linear: lookup issue labels: %w", err)
	}
	var resp struct {
		Data struct {
			Issue *struct {
				ID     string `json:"id"`
				Labels struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"issue"`
			IssueLabels struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"issueLabels"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", linearDesignLabels{}, fmt.Errorf("worksource/linear: decode labels: %w", err)
	}
	if resp.Data.Issue == nil || resp.Data.Issue.ID == "" {
		return "", linearDesignLabels{}, fmt.Errorf("worksource/linear: issue %q not found", identifier)
	}
	labels := linearDesignLabels{}
	for _, n := range resp.Data.Issue.Labels.Nodes {
		labels.current = append(labels.current, linearDesignLabel{id: n.ID, name: n.Name})
	}
	for _, n := range resp.Data.IssueLabels.Nodes {
		labels.all = append(labels.all, linearDesignLabel{id: n.ID, name: n.Name})
	}
	return resp.Data.Issue.ID, labels, nil
}

func linearHasHoldLabel(label string, extraHoldLabels []string) bool {
	lower := strings.ToLower(label)
	for _, sub := range append(append([]string{}, defaultLinearHoldLabels...), extraHoldLabels...) {
		sub = strings.ToLower(strings.TrimSpace(sub))
		if sub != "" && strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

func (s *LinearSource) designIssueState(ctx context.Context, ref Ref, target string) (string, string, error) {
	identifier := strings.TrimSpace(ref.ExternalID)
	if identifier == "" {
		return "", "", fmt.Errorf("worksource/linear: external id is required")
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearDesignIssueStatesQuery, map[string]interface{}{"identifier": identifier})
	if err != nil {
		return "", "", fmt.Errorf("worksource/linear: lookup issue states: %w", err)
	}
	var resp struct {
		Data struct {
			Issue *struct {
				ID   string `json:"id"`
				Team struct {
					States struct {
						Nodes []struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"nodes"`
					} `json:"states"`
				} `json:"team"`
			} `json:"issue"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", fmt.Errorf("worksource/linear: decode states: %w", err)
	}
	if resp.Data.Issue == nil || resp.Data.Issue.ID == "" {
		return "", "", fmt.Errorf("worksource/linear: issue %q not found", identifier)
	}
	for _, st := range resp.Data.Issue.Team.States.Nodes {
		if st.ID == target || strings.EqualFold(st.Name, target) {
			return resp.Data.Issue.ID, st.ID, nil
		}
	}
	if strings.Contains(target, "-") || strings.HasPrefix(target, "lin_") {
		return resp.Data.Issue.ID, target, nil
	}
	return "", "", fmt.Errorf("worksource/linear: state %q not available for %s", target, identifier)
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
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, query, vars)
	if err != nil {
		return nil, err
	}
	var resp linearGraphQLResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &resp, nil
}

// linearGraphQL posts one GraphQL request to Linear with the bare API-key
// Authorization header Linear expects and returns the raw response body once
// it has passed the HTTP-status and top-level `errors` checks. Shared by the
// issue enumerator (doQuery) and the advisory digest poster so both speak to
// Linear the same way. An empty baseURL means the production endpoint.
func linearGraphQL(ctx context.Context, client *http.Client, baseURL, apiKey, query string, vars map[string]interface{}) ([]byte, error) {
	if baseURL == "" {
		baseURL = defaultLinearBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
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
	req.Header.Set("Authorization", apiKey)

	httpResp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if httpResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d: %s", httpResp.StatusCode, string(raw))
	}
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", envelope.Errors[0].Message)
	}
	return raw, nil
}

// TeamForRepo picks the Linear team that owns work for repo: the first
// configured team whose Repo matches (either the full "owner/name" or the
// bare name), else the first team. ok is false only when no teams are
// configured at all. This is the team the ACMM gap issue for a repo is
// filed on when governor.acmm.issue_tracker is work_source.
func (s *LinearSource) TeamForRepo(repo string) (LinearTeamConfig, bool) {
	if len(s.cfg.Teams) == 0 {
		return LinearTeamConfig{}, false
	}
	repo = strings.TrimSpace(repo)
	for _, t := range s.cfg.Teams {
		if t.Repo == repo || repoBaseName(t.Repo) == repo {
			return t, true
		}
	}
	return s.cfg.Teams[0], true
}

func repoBaseName(full string) string {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}

// linearTeamIDQuery resolves a team key to the UUID issueCreate needs.
const linearTeamIDQuery = `query($key: String!) {
  teams(filter: { key: { eq: $key } }, first: 1) { nodes { id key } }
}`

// linearIssueCreateMutation is the minimal write: team, title, description.
const linearIssueCreateMutation = `mutation($input: IssueCreateInput!) {
  issueCreate(input: $input) {
    success
    issue { id identifier number url title }
  }
}`

type linearTeamIDResponse struct {
	Data struct {
		Teams struct {
			Nodes []struct {
				ID  string `json:"id"`
				Key string `json:"key"`
			} `json:"nodes"`
		} `json:"teams"`
	} `json:"data"`
}

type linearIssueCreateResponse struct {
	Data struct {
		IssueCreate struct {
			Success bool               `json:"success"`
			Issue   LinearCreatedIssue `json:"issue"`
		} `json:"issueCreate"`
	} `json:"data"`
}

// LinearCreatedIssue is what issueCreate returns for the new issue.
type LinearCreatedIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"` // e.g. "ENG-123"
	Number     int    `json:"number"`     // the numeric part of Identifier
	URL        string `json:"url"`
	Title      string `json:"title"`
}

// CreateIssue files a new issue on the team with key teamKey via Linear's
// issueCreate mutation. It is the write counterpart of ListIssues and goes
// through the same linearGraphQL transport (credential, endpoint, error
// envelope) as the enumerator and the advisory digest poster. The
// description is Markdown, which Linear renders natively, so callers can
// pass the same body they would give GitHub.
func (s *LinearSource) CreateIssue(ctx context.Context, teamKey, title, description string) (*LinearCreatedIssue, error) {
	teamKey = strings.TrimSpace(teamKey)
	if teamKey == "" {
		return nil, fmt.Errorf("linear: team key is required")
	}
	raw, err := linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearTeamIDQuery, map[string]interface{}{"key": teamKey})
	if err != nil {
		return nil, fmt.Errorf("linear: resolve team %s: %w", teamKey, err)
	}
	var teamResp linearTeamIDResponse
	if err := json.Unmarshal(raw, &teamResp); err != nil {
		return nil, fmt.Errorf("linear: resolve team %s: decode response: %w", teamKey, err)
	}
	if len(teamResp.Data.Teams.Nodes) == 0 {
		return nil, fmt.Errorf("linear: team %q not found", teamKey)
	}
	input := map[string]interface{}{
		"teamId":      teamResp.Data.Teams.Nodes[0].ID,
		"title":       title,
		"description": description,
	}
	raw, err = linearGraphQL(ctx, s.client, s.cfg.BaseURL, s.cfg.APIKey, linearIssueCreateMutation, map[string]interface{}{"input": input})
	if err != nil {
		return nil, fmt.Errorf("linear: issueCreate: %w", err)
	}
	var resp linearIssueCreateResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("linear: issueCreate: decode response: %w", err)
	}
	if !resp.Data.IssueCreate.Success || resp.Data.IssueCreate.Issue.Identifier == "" {
		return nil, fmt.Errorf("linear: issueCreate did not return an issue")
	}
	issue := resp.Data.IssueCreate.Issue
	return &issue, nil
}

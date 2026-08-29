package worksource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// JiraConfig configures the Jira Cloud REST API v3 work source adapter
// (Phase 1, read-only).
type JiraConfig struct {
	// BaseURL is the Jira Cloud instance root, e.g. "https://myorg.atlassian.net".
	BaseURL string
	// Email is the Atlassian account email for Basic auth.
	Email string
	// APIToken is the Jira API token (Atlassian account → Security → API tokens).
	// Stored as a secret reference; the actual value is resolved from env by the caller.
	APIToken string
	// ProjectKeys is the list of Jira project keys to enumerate, e.g. ["ENG","OPS"].
	ProjectKeys []string
	// JQL is an optional JQL override. When empty, the adapter builds a default
	// JQL: "project in (<keys>) AND statusCategory != Done AND issuetype != Epic"
	JQL string
	// Repo is the GitHub repo (owner/name) agents clone for these issues.
	Repo string
	// PriorityField is the Jira field name for priority (default: "priority").
	PriorityField string
	// HoldLabels are Jira label values that gate an issue (like GitHub "hold").
	HoldLabels []string
}

// jiraMaxResults is the page size requested from the Jira search API.
const jiraMaxResults = 100

// jiraSource is the Jira Cloud WorkSource adapter.
type jiraSource struct {
	cfg    JiraConfig
	client *http.Client
}

// NewJiraSource builds a WorkSource backed by the Jira Cloud REST API v3.
func NewJiraSource(cfg JiraConfig) WorkSource {
	if cfg.PriorityField == "" {
		cfg.PriorityField = "priority"
	}
	return &jiraSource{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (s *jiraSource) SourceType() string { return "jira" }

// jql returns the effective JQL: the configured override, or the default
// built from ProjectKeys.
func (s *jiraSource) jql() string {
	if s.cfg.JQL != "" {
		return s.cfg.JQL
	}
	return fmt.Sprintf("project in (%s) AND statusCategory != Done AND issuetype != Epic",
		strings.Join(s.cfg.ProjectKeys, ","))
}

// Jira search response wire types (only the fields we read).
type jiraSearchResponse struct {
	StartAt    int         `json:"startAt"`
	MaxResults int         `json:"maxResults"`
	Total      int         `json:"total"`
	Issues     []jiraIssue `json:"issues"`
}

type jiraIssue struct {
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
}

type jiraFields struct {
	Summary  string        `json:"summary"`
	Status   *jiraNamed    `json:"status"`
	Priority *jiraNamed    `json:"priority"`
	Assignee *jiraUser     `json:"assignee"`
	Reporter *jiraUser     `json:"reporter"`
	Labels   []string      `json:"labels"`
	Created  jiraTimestamp `json:"created"`
	Updated  jiraTimestamp `json:"updated"`
}

type jiraNamed struct {
	Name string `json:"name"`
}

type jiraUser struct {
	DisplayName string `json:"displayName"`
}

// jiraTimestamp parses Jira's "2006-01-02T15:04:05.000-0700" timestamps, and
// falls back to RFC 3339.
type jiraTimestamp struct {
	time.Time
}

func (t *jiraTimestamp) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	parsed, err := time.Parse("2006-01-02T15:04:05.000-0700", s)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("worksource/jira: parse timestamp %q: %w", s, err)
		}
	}
	t.Time = parsed
	return nil
}

// normalizeJiraPriority maps Jira priority names (across common naming
// schemes) to the worksource Priority strings.
func normalizeJiraPriority(name string) string {
	switch strings.ToLower(name) {
	case "highest", "critical", "blocker", "p0":
		return "urgent"
	case "high", "p1":
		return "high"
	case "medium", "p2":
		return "medium"
	case "low", "lowest", "p3", "p4":
		return "low"
	default:
		return "none"
	}
}

// hasHoldLabel reports whether any issue label matches a configured hold label.
func (s *jiraSource) hasHoldLabel(labels []string) bool {
	for _, l := range labels {
		for _, h := range s.cfg.HoldLabels {
			if l == h {
				return true
			}
		}
	}
	return false
}

func (s *jiraSource) ListIssues(ctx context.Context) ([]Issue, error) {
	var out []Issue
	startAt := 0
	for {
		page, err := s.searchPage(ctx, startAt)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Issues {
			if s.hasHoldLabel(it.Fields.Labels) {
				continue
			}
			out = append(out, s.toIssue(it))
		}
		if page.StartAt+page.MaxResults >= page.Total || len(page.Issues) == 0 {
			break
		}
		startAt = page.StartAt + page.MaxResults
	}
	return out, nil
}

func (s *jiraSource) searchPage(ctx context.Context, startAt int) (*jiraSearchResponse, error) {
	q := url.Values{}
	q.Set("jql", s.jql())
	q.Set("fields", "summary,status,priority,assignee,reporter,labels,created,updated")
	q.Set("maxResults", fmt.Sprintf("%d", jiraMaxResults))
	q.Set("startAt", fmt.Sprintf("%d", startAt))
	reqURL := strings.TrimSuffix(s.cfg.BaseURL, "/") + "/rest/api/3/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("worksource/jira: build request: %w", err)
	}
	req.SetBasicAuth(s.cfg.Email, s.cfg.APIToken)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("worksource/jira: search: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("worksource/jira: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worksource/jira: search returned %d: %s", resp.StatusCode, string(body))
	}
	var page jiraSearchResponse
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, fmt.Errorf("worksource/jira: decode response: %w", err)
	}
	return &page, nil
}

func (s *jiraSource) toIssue(it jiraIssue) Issue {
	iss := Issue{
		SourceType: "jira",
		Repo:       s.cfg.Repo,
		ExternalID: it.Key,
		Number:     0,
		Title:      it.Fields.Summary,
		Labels:     it.Fields.Labels,
		Priority:   "none",
		CreatedAt:  it.Fields.Created.Time,
		UpdatedAt:  it.Fields.Updated.Time,
		URL:        strings.TrimSuffix(s.cfg.BaseURL, "/") + "/browse/" + it.Key,
	}
	if it.Fields.Status != nil {
		iss.State = it.Fields.Status.Name
	}
	if it.Fields.Priority != nil {
		iss.Priority = normalizeJiraPriority(it.Fields.Priority.Name)
	}
	if it.Fields.Reporter != nil {
		iss.Author = it.Fields.Reporter.DisplayName
	}
	if it.Fields.Assignee != nil {
		iss.Assignees = []string{it.Fields.Assignee.DisplayName}
	}
	return iss
}

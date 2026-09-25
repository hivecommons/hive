package worksource

import (
	"bytes"
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
	// Transitions maps Hive design status names to Jira transition names or ids.
	// When a status is not present, the status string itself is used as the
	// desired transition name/id.
	Transitions map[string]string
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

func (s *jiraSource) AddLabel(ctx context.Context, ref Ref, label string) error {
	return s.updateLabels(ctx, ref, map[string][]map[string]string{"labels": []map[string]string{{"add": strings.TrimSpace(label)}}})
}

func (s *jiraSource) RemoveLabel(ctx context.Context, ref Ref, label string) error {
	return s.updateLabels(ctx, ref, map[string][]map[string]string{"labels": []map[string]string{{"remove": strings.TrimSpace(label)}}})
}

func (s *jiraSource) AddComment(ctx context.Context, ref Ref, body string) error {
	if s == nil {
		return fmt.Errorf("worksource/jira: source unavailable")
	}
	key := strings.TrimSpace(ref.ExternalID)
	if key == "" {
		key = strings.TrimPrefix(strings.TrimSpace(ref.Display()), "#")
	}
	if key == "" {
		return fmt.Errorf("worksource/jira: external id is required")
	}
	payload := map[string]any{"body": map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []map[string]any{{
			"type": "paragraph",
			"content": []map[string]any{{
				"type": "text",
				"text": strings.TrimSpace(body),
			}},
		}},
	}}
	return s.doJSON(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/comment", payload, http.StatusCreated)
}

func (s *jiraSource) TransitionStatus(ctx context.Context, ref Ref, status string) error {
	if s == nil {
		return fmt.Errorf("worksource/jira: source unavailable")
	}
	key := strings.TrimSpace(ref.ExternalID)
	if key == "" {
		return fmt.Errorf("worksource/jira: external id is required")
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
	id, err := s.findTransition(ctx, key, target)
	if err != nil {
		return err
	}
	return s.doJSON(ctx, http.MethodPost, "/rest/api/3/issue/"+url.PathEscape(key)+"/transitions", map[string]any{
		"transition": map[string]string{"id": id},
	}, http.StatusNoContent)
}

func (s *jiraSource) updateLabels(ctx context.Context, ref Ref, update map[string][]map[string]string) error {
	if s == nil {
		return fmt.Errorf("worksource/jira: source unavailable")
	}
	key := strings.TrimSpace(ref.ExternalID)
	if key == "" {
		return fmt.Errorf("worksource/jira: external id is required")
	}
	return s.doJSON(ctx, http.MethodPut, "/rest/api/3/issue/"+url.PathEscape(key), map[string]any{"update": update}, http.StatusNoContent)
}

func (s *jiraSource) doJSON(ctx context.Context, method, path string, payload any, want int) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(payload); err != nil {
		return fmt.Errorf("worksource/jira: encode request: %w", err)
	}
	reqURL := strings.TrimSuffix(s.cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, &buf)
	if err != nil {
		return fmt.Errorf("worksource/jira: build request: %w", err)
	}
	req.SetBasicAuth(s.cfg.Email, s.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("worksource/jira: mutation: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		return fmt.Errorf("worksource/jira: mutation returned %d: %s", resp.StatusCode, string(raw))
	}
	return nil
}

func (s *jiraSource) findTransition(ctx context.Context, key, target string) (string, error) {
	reqURL := strings.TrimSuffix(s.cfg.BaseURL, "/") + "/rest/api/3/issue/" + url.PathEscape(key) + "/transitions"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", fmt.Errorf("worksource/jira: build transition request: %w", err)
	}
	req.SetBasicAuth(s.cfg.Email, s.cfg.APIToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("worksource/jira: list transitions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("worksource/jira: transitions returned %d: %s", resp.StatusCode, string(raw))
	}
	var body struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"transitions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("worksource/jira: decode transitions: %w", err)
	}
	for _, tr := range body.Transitions {
		if tr.ID == target || strings.EqualFold(tr.Name, target) {
			return tr.ID, nil
		}
	}
	return "", fmt.Errorf("worksource/jira: transition %q not available for %s", target, key)
}

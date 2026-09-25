package worksource

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	jiraDeploymentCloud      = "cloud"
	jiraDeploymentDataCenter = "datacenter"
	jiraDeploymentServer     = "server"
	jiraCloudAPIVersion      = "3"
	jiraDataCenterAPIVersion = "2"
	jiraSearchPageSize       = 100
	jiraHTTPTimeout          = 30 * time.Second
)

// JiraConfig configures the Jira work source adapter. Empty Deployment keeps
// the original Jira Cloud REST API v3 behavior; "datacenter"/"server" switches
// to Jira Data Center/Server REST API v2 and Data Center auth/body shapes.
type JiraConfig struct {
	// Deployment is "cloud" (default) or "datacenter"/"server".
	Deployment string
	// BaseURL is the Jira instance root, e.g. "https://myorg.atlassian.net" or
	// "https://jira.example.com/jira" when Data Center runs under a context path.
	BaseURL string
	// Email is the Atlassian account email for Jira Cloud Basic auth.
	Email string
	// Username is the Jira Data Center username for Basic auth when Password is used.
	Username string
	// APIToken is the Jira Cloud API token, or the Data Center Personal Access
	// Token when Deployment is "datacenter".
	APIToken string
	// Password is the Jira Data Center password for Basic auth when APIToken is empty.
	Password string
	// CABundle is an optional PEM CA bundle appended to the system roots for Jira Data Center.
	CABundle string
	// InsecureSkipVerify disables server certificate verification for Jira Data Center.
	InsecureSkipVerify bool
	// ClientCert and ClientKey are optional PEM materials for Jira Data Center mTLS.
	ClientCert string
	ClientKey  string
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
	// Logger receives a warning whenever a Data Center client is built with TLS verification disabled.
	Logger *slog.Logger
}

// jiraMaxResults is the page size requested from the Jira search API.
const jiraMaxResults = jiraSearchPageSize

// jiraSearchFields lists the issue fields the search response parser reads. The
// Jira Cloud enhanced search endpoint returns only the fields explicitly
// requested, so this list must cover everything toIssue consumes.
var jiraSearchFields = []string{"summary", "status", "priority", "assignee", "reporter", "labels", "created", "updated"}

// jiraSource is the Jira WorkSource adapter.
type jiraSource struct {
	cfg    JiraConfig
	client *http.Client
	tlsErr error
}

// NewJiraSource builds a WorkSource backed by the Jira REST API.
func NewJiraSource(cfg JiraConfig) WorkSource {
	if cfg.PriorityField == "" {
		cfg.PriorityField = "priority"
	}
	client, err := jiraHTTPClient(cfg)
	return &jiraSource{
		cfg:    cfg,
		client: client,
		tlsErr: err,
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

func (s *jiraSource) isDataCenter() bool {
	return jiraConfigIsDataCenter(s.cfg.Deployment)
}

func (s *jiraSource) apiVersion() string {
	if s.isDataCenter() {
		return jiraDataCenterAPIVersion
	}
	return jiraCloudAPIVersion
}

func (s *jiraSource) restURL(path string) string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/rest/api/" + s.apiVersion() + "/" + strings.TrimLeft(path, "/")
}

func (s *jiraSource) browseURL(key string) string {
	return strings.TrimRight(s.cfg.BaseURL, "/") + "/browse/" + key
}

// ValidateJiraTLSConfig parses the Data Center TLS materials without building
// a client. Cloud deployments ignore these fields so existing Cloud configs
// remain unchanged.
func ValidateJiraTLSConfig(cfg JiraConfig) error {
	if !jiraConfigIsDataCenter(cfg.Deployment) {
		return nil
	}
	if _, err := jiraTLSConfig(cfg, false); err != nil {
		return err
	}
	return nil
}

func jiraHTTPClient(cfg JiraConfig) (*http.Client, error) {
	tlsConfig, err := jiraTLSConfig(cfg, true)
	if err != nil {
		return &http.Client{Timeout: jiraHTTPTimeout}, err
	}
	if tlsConfig == nil {
		return &http.Client{Timeout: jiraHTTPTimeout}, nil
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Client{Timeout: jiraHTTPTimeout}, fmt.Errorf("worksource/jira: default transport is %T, want *http.Transport", http.DefaultTransport)
	}
	transport := defaultTransport.Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Timeout: jiraHTTPTimeout, Transport: transport}, nil
}

func jiraTLSConfig(cfg JiraConfig, warnInsecure bool) (*tls.Config, error) {
	if !jiraConfigIsDataCenter(cfg.Deployment) {
		return nil, nil
	}
	hasTLSConfig := cfg.CABundle != "" || cfg.InsecureSkipVerify || cfg.ClientCert != "" || cfg.ClientKey != ""
	if !hasTLSConfig {
		return nil, nil
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CABundle != "" {
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if ok := roots.AppendCertsFromPEM([]byte(cfg.CABundle)); !ok {
			return nil, fmt.Errorf("work_source.jira.ca_bundle must contain at least one valid PEM certificate")
		}
		tlsConfig.RootCAs = roots
	}
	if cfg.ClientCert != "" || cfg.ClientKey != "" {
		if cfg.ClientCert == "" || cfg.ClientKey == "" {
			return nil, fmt.Errorf("work_source.jira.client_cert and client_key must be set together")
		}
		cert, err := tls.X509KeyPair([]byte(cfg.ClientCert), []byte(cfg.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("work_source.jira.client_cert/client_key must contain a valid PEM certificate and key pair: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}
	if cfg.InsecureSkipVerify {
		logger := cfg.Logger
		if logger == nil {
			logger = slog.Default()
		}
		if warnInsecure {
			logger.Warn("worksource/jira: TLS certificate verification is disabled for Jira Data Center; use only for testing", "base_url", cfg.BaseURL)
		}
		tlsConfig.InsecureSkipVerify = true // #nosec G402 -- explicit operator opt-in, warned in UI/logs.
	}
	return tlsConfig, nil
}

func jiraConfigIsDataCenter(deployment string) bool {
	switch strings.ToLower(strings.TrimSpace(deployment)) {
	case jiraDeploymentDataCenter, jiraDeploymentServer:
		return true
	default:
		return false
	}
}

// Jira search response wire types (only the fields we read).
type jiraSearchResponse struct {
	StartAt    int         `json:"startAt"`
	MaxResults int         `json:"maxResults"`
	Total      int         `json:"total"`
	Issues     []jiraIssue `json:"issues"`
	// NextPageToken and IsLast drive token-based pagination for the Jira Cloud
	// enhanced search endpoint (/rest/api/3/search/jql), which does not return
	// startAt/total. Data Center still paginates with StartAt/MaxResults/Total.
	NextPageToken string `json:"nextPageToken"`
	IsLast        bool   `json:"isLast"`
}

type jiraIssue struct {
	Key    string     `json:"key"`
	Fields jiraFields `json:"fields"`
}

type jiraFields struct {
	Summary     string          `json:"summary"`
	Description json.RawMessage `json:"description"`
	Status      *jiraNamed      `json:"status"`
	Priority    *jiraNamed      `json:"priority"`
	Assignee    *jiraUser       `json:"assignee"`
	Reporter    *jiraUser       `json:"reporter"`
	Labels      []string        `json:"labels"`
	Created     jiraTimestamp   `json:"created"`
	Updated     jiraTimestamp   `json:"updated"`
}

type jiraNamed struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type jiraUser struct {
	AccountID   string `json:"accountId"`
	Name        string `json:"name"`
	Key         string `json:"key"`
	DisplayName string `json:"displayName"`
}

func (u *jiraUser) identity(dataCenter bool) string {
	if u == nil {
		return ""
	}
	if dataCenter {
		for _, v := range []string{u.Name, u.Key, u.DisplayName, u.AccountID} {
			if v != "" {
				return v
			}
		}
		return ""
	}
	for _, v := range []string{u.DisplayName, u.AccountID, u.Name, u.Key} {
		if v != "" {
			return v
		}
	}
	return ""
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
	if s.tlsErr != nil {
		return nil, s.tlsErr
	}
	var out []Issue
	startAt := 0
	pageToken := ""
	for {
		page, err := s.searchPage(ctx, startAt, pageToken)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Issues {
			if s.hasHoldLabel(it.Fields.Labels) {
				continue
			}
			out = append(out, s.toIssue(it))
		}
		if len(page.Issues) == 0 {
			break
		}
		if s.isDataCenter() {
			if page.StartAt+page.MaxResults >= page.Total {
				break
			}
			startAt = page.StartAt + page.MaxResults
			continue
		}
		// Jira Cloud enhanced search paginates with an opaque nextPageToken and
		// has no startAt/total; stop once the API signals the last page.
		if page.IsLast || page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	return out, nil
}

func (s *jiraSource) searchPage(ctx context.Context, startAt int, pageToken string) (*jiraSearchResponse, error) {
	var page jiraSearchResponse
	if s.isDataCenter() {
		// Jira Data Center/Server still supports the classic /search endpoint.
		q := url.Values{}
		q.Set("jql", s.jql())
		q.Set("fields", strings.Join(jiraSearchFields, ","))
		q.Set("maxResults", fmt.Sprintf("%d", jiraMaxResults))
		q.Set("startAt", fmt.Sprintf("%d", startAt))
		if err := s.doJSON(ctx, http.MethodGet, s.restURL("search")+"?"+q.Encode(), nil, http.StatusOK, &page); err != nil {
			return nil, fmt.Errorf("worksource/jira: search: %w", err)
		}
		return &page, nil
	}
	// Jira Cloud removed the classic /search endpoint (HTTP 410, Atlassian
	// CHANGE-2046). Use the enhanced JQL search endpoint, which needs an explicit
	// fields list and paginates with a nextPageToken.
	payload := map[string]any{
		"jql":        s.jql(),
		"fields":     jiraSearchFields,
		"maxResults": jiraMaxResults,
	}
	if pageToken != "" {
		payload["nextPageToken"] = pageToken
	}
	if err := s.doJSON(ctx, http.MethodPost, s.restURL("search/jql"), payload, http.StatusOK, &page); err != nil {
		return nil, fmt.Errorf("worksource/jira: search: %w", err)
	}
	return &page, nil
}

func (s *jiraSource) fetchIssue(ctx context.Context, key string) (*jiraIssue, error) {
	q := url.Values{}
	q.Set("fields", "summary,description,status,priority,assignee,reporter,labels,created,updated")
	var issue jiraIssue
	if err := s.doJSON(ctx, http.MethodGet, s.restURL("issue/"+url.PathEscape(key))+"?"+q.Encode(), nil, http.StatusOK, &issue); err != nil {
		return nil, fmt.Errorf("worksource/jira: fetch issue %s: %w", key, err)
	}
	return &issue, nil
}

func (s *jiraSource) addComment(ctx context.Context, key, body string) error {
	payload := map[string]any{"body": body}
	if !s.isDataCenter() {
		payload["body"] = jiraADFDocument(body)
	}
	if err := s.doJSON(ctx, http.MethodPost, s.restURL("issue/"+url.PathEscape(key)+"/comment"), payload, http.StatusCreated, nil); err != nil {
		return fmt.Errorf("worksource/jira: add comment on %s: %w", key, err)
	}
	return nil
}

func (s *jiraSource) transitionIssue(ctx context.Context, key, transitionID string) error {
	payload := map[string]any{"transition": map[string]string{"id": transitionID}}
	if err := s.doJSON(ctx, http.MethodPost, s.restURL("issue/"+url.PathEscape(key)+"/transitions"), payload, http.StatusNoContent, nil); err != nil {
		return fmt.Errorf("worksource/jira: transition %s: %w", key, err)
	}
	return nil
}

func jiraADFDocument(text string) map[string]any {
	return map[string]any{
		"type":    "doc",
		"version": 1,
		"content": []map[string]any{{
			"type": "paragraph",
			"content": []map[string]string{{
				"type": "text",
				"text": text,
			}},
		}},
	}
}

func (s *jiraSource) doJSON(ctx context.Context, method, reqURL string, payload any, wantStatus int, out any) error {
	if s.tlsErr != nil {
		return s.tlsErr
	}
	var body io.Reader
	if payload != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(payload); err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	s.authorize(req)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("returned %d: %s", resp.StatusCode, string(respBody))
	}
	if out == nil || len(strings.TrimSpace(string(respBody))) == 0 {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (s *jiraSource) authorize(req *http.Request) {
	if s.isDataCenter() && s.cfg.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.APIToken)
		return
	}
	username := s.cfg.Email
	password := s.cfg.APIToken
	if s.isDataCenter() {
		username = s.cfg.Username
		if username == "" {
			username = s.cfg.Email
		}
		password = s.cfg.Password
	}
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}
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
		URL:        s.browseURL(it.Key),
	}
	if it.Fields.Status != nil {
		iss.State = it.Fields.Status.Name
	}
	if it.Fields.Priority != nil {
		iss.Priority = normalizeJiraPriority(it.Fields.Priority.Name)
	}
	if it.Fields.Reporter != nil {
		iss.Author = it.Fields.Reporter.identity(s.isDataCenter())
	}
	if assignee := it.Fields.Assignee.identity(s.isDataCenter()); assignee != "" {
		iss.Assignees = []string{assignee}
	}
	return iss
}

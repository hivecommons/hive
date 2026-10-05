package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/agent"
)

const (
	feedbackIngestPath             = "/api/feedback/ingest"
	feedbackIssuesPath             = "/api/feedback/issues"
	feedbackMaxRequestBytes        = 15 << 20
	feedbackMaxTextBytes           = 64 << 10
	feedbackMaxScreenshots         = 5
	feedbackMaxScreenshotBytes     = 2 << 20
	feedbackHubRateWindow          = 24 * time.Hour
	feedbackHubMaxPerHivePerWindow = 10
	feedbackTargetHive             = "hive"
	feedbackTargetDocs             = "docs"
	feedbackTypeBug                = "bug"
	feedbackTypeFeature            = "feature"
	feedbackMaxIssueRefs           = 50
	// feedbackScreenshotBranch is the only branch screenshots are ever
	// committed to. Uploads are unreviewed writes made with the hub's token,
	// so they must never land on the repository's default branch.
	feedbackScreenshotBranch = "feedback-screenshots"
	feedbackScreenshotDir    = "feedback-screenshots"
)

var feedbackGitHubAPIBase = "https://api.github.com"
var lookupHubFeedbackTokenLogin = agent.GitHubTokenLogin

type feedbackConsoleError struct {
	Timestamp string `json:"timestamp,omitempty"`
	Level     string `json:"level,omitempty"`
	Message   string `json:"message,omitempty"`
	Source    string `json:"source,omitempty"`
}
type feedbackFailedAPICall struct {
	Timestamp string `json:"timestamp,omitempty"`
	Status    string `json:"status,omitempty"`
	Path      string `json:"path,omitempty"`
}
type feedbackAgentDiagnostic struct {
	Name    string `json:"name,omitempty"`
	Backend string `json:"backend,omitempty"`
	Model   string `json:"model,omitempty"`
	State   string `json:"state,omitempty"`
	Repo    string `json:"repo,omitempty"`
	Org     string `json:"org,omitempty"`
}
type feedbackDiagnostics struct {
	Version             string                    `json:"version,omitempty"`
	Commit              string                    `json:"commit,omitempty"`
	Channel             string                    `json:"channel,omitempty"`
	ACMMLevel           string                    `json:"acmm_level,omitempty"`
	HiveID              string                    `json:"hive_id,omitempty"`
	Hosted              bool                      `json:"hosted,omitempty"`
	HubLinked           bool                      `json:"hub_linked,omitempty"`
	AgentCount          int                       `json:"agent_count,omitempty"`
	Agents              []feedbackAgentDiagnostic `json:"agents,omitempty"`
	IncludeProjectRepos bool                      `json:"include_project_repos,omitempty"`
	BrowserUA           string                    `json:"browser_user_agent,omitempty"`
	BrowserPlatform     string                    `json:"browser_platform,omitempty"`
	BrowserLanguage     string                    `json:"browser_language,omitempty"`
	ScreenSize          string                    `json:"screen_size,omitempty"`
	WindowSize          string                    `json:"window_size,omitempty"`
	Page                string                    `json:"page,omitempty"`
}
type feedbackSubmitterIdentity struct {
	Name        string `json:"name,omitempty"`
	GitHubLogin string `json:"github_login,omitempty"`
	Source      string `json:"source,omitempty"`
}
type feedbackReportRequest struct {
	Title              string                    `json:"title"`
	Description        string                    `json:"description"`
	RequestType        string                    `json:"request_type"`
	TargetRepo         string                    `json:"target_repo"`
	HiveID             string                    `json:"hive_id,omitempty"`
	CredentialLogin    string                    `json:"credential_login,omitempty"`
	HubName            string                    `json:"hub_name,omitempty"`
	Submitter          feedbackSubmitterIdentity `json:"submitter,omitempty"`
	OpenedByHive       bool                      `json:"opened_by_hive,omitempty"`
	Screenshots        []string                  `json:"screenshots,omitempty"`
	IncludeDiagnostics bool                      `json:"include_diagnostics"`
	Diagnostics        *feedbackDiagnostics      `json:"diagnostics,omitempty"`
	ConsoleErrors      []feedbackConsoleError    `json:"console_errors,omitempty"`
	FailedAPICalls     []feedbackFailedAPICall   `json:"failed_api_calls,omitempty"`
}
type feedbackReportResponse struct {
	OK          bool   `json:"ok"`
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
	Warning     string `json:"warning,omitempty"`
}
type feedbackIssueResult struct {
	Number int
	URL    string
	ID     int64
}
type feedbackIssueStatus struct {
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Title     string `json:"title,omitempty"`
	State     string `json:"state,omitempty"`
	HTMLURL   string `json:"html_url,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Comments  int    `json:"comments,omitempty"`
}

type feedbackRateLimiter struct {
	mu    sync.Mutex
	hives map[string][]time.Time
}

var hubFeedbackRate feedbackRateLimiter
var errFeedbackRateLimited = errors.New("feedback: hive rate limit exceeded")

func (l *feedbackRateLimiter) reserve(hiveID string, now time.Time) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.hives == nil {
		l.hives = map[string][]time.Time{}
	}
	cutoff := now.Add(-feedbackHubRateWindow)
	kept := l.hives[hiveID][:0]
	for _, t := range l.hives[hiveID] {
		if !t.Before(cutoff) {
			kept = append(kept, t)
		}
	}
	l.hives[hiveID] = kept
	if len(kept) >= feedbackHubMaxPerHivePerWindow {
		return nil, errFeedbackRateLimited
	}
	l.hives[hiveID] = append(l.hives[hiveID], now)
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		xs := l.hives[hiveID]
		for i, t := range xs {
			if t.Equal(now) {
				l.hives[hiveID] = append(xs[:i], xs[i+1:]...)
				break
			}
		}
	}, nil
}

var feedbackTokenPattern = regexp.MustCompile(`(?i)(ghp_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+`)
var feedbackBearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
var feedbackEmailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
var feedbackKVPattern = regexp.MustCompile(`(?i)(secret|token|password|key)\s*[:=]\s*[^\s,;]+`)

func (s *HubServer) handleFeedbackIngest(w http.ResponseWriter, r *http.Request) {
	if s.hubSecret == "" {
		npsJSONError(w, "feedback ingest requires a configured hub secret", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	presented := strings.TrimPrefix(auth, "Bearer ")
	body, err := io.ReadAll(io.LimitReader(r.Body, feedbackMaxRequestBytes+1))
	if err != nil {
		npsJSONError(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > feedbackMaxRequestBytes {
		npsJSONError(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req feedbackReportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		npsJSONError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if err := validateHubFeedbackRequest(&req); err != nil {
		npsJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	hiveID := sanitizeHeartbeatField(req.HiveID)
	if req.Diagnostics != nil {
		if diagHiveID := sanitizeHeartbeatField(req.Diagnostics.HiveID); diagHiveID != "" {
			hiveID = diagHiveID
		}
	}
	if hiveID == "" || !isValidName(hiveID) {
		npsJSONError(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	if !s.verifyHeartbeatBearer(presented, hiveID) {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.npsHiveRegistered(hiveID) {
		npsJSONError(w, "unknown hive - heartbeat first", http.StatusForbidden)
		return
	}
	release, err := hubFeedbackRate.reserve(hiveID, time.Now())
	if err != nil {
		npsJSONError(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	token := strings.TrimSpace(s.envGitHubToken)
	if token == "" {
		token = hubGitHubToken()
	}
	if token == "" {
		release()
		npsJSONError(w, "hub GitHub token is not configured", http.StatusServiceUnavailable)
		return
	}
	req.CredentialLogin = hubGitHubLoginForMention(lookupHubFeedbackTokenLogin(token))
	result, warning, err := createHubFeedbackIssue(r.Context(), http.DefaultClient, token, req, feedbackGitHubAPIBase)
	if err != nil {
		release()
		if s.logger != nil {
			s.logger.Error("feedback: failed to create issue", "hive", hiveID, "error", err)
		}
		npsJSONError(w, "failed to create issue", http.StatusBadGateway)
		return
	}
	if s.logger != nil {
		s.logger.Info("feedback: created issue", "hive", hiveID, "target", req.TargetRepo, "type", req.RequestType, "issue", result.Number)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(feedbackReportResponse{OK: true, IssueNumber: result.Number, IssueURL: result.URL, Warning: warning})
}

func (s *HubServer) handleFeedbackIssues(w http.ResponseWriter, r *http.Request) {
	if s.hubSecret == "" {
		npsJSONError(w, "feedback issue lookup requires a configured hub secret", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	hiveID := sanitizeHeartbeatField(r.URL.Query().Get("hive_id"))
	if hiveID == "" || !isValidName(hiveID) {
		npsJSONError(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	if !s.verifyHeartbeatBearer(strings.TrimPrefix(auth, "Bearer "), hiveID) {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.npsHiveRegistered(hiveID) {
		npsJSONError(w, "unknown hive - heartbeat first", http.StatusForbidden)
		return
	}
	refs, err := parseHubFeedbackRefs(r.URL.Query().Get("refs"))
	if err != nil {
		npsJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(s.envGitHubToken)
	if token == "" {
		token = hubGitHubToken()
	}
	if token == "" {
		npsJSONError(w, "hub GitHub token is not configured", http.StatusServiceUnavailable)
		return
	}
	items, err := fetchHubFeedbackIssueStatuses(r.Context(), http.DefaultClient, feedbackGitHubAPIBase, token, refs)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("feedback: issue lookup failed", "hive", hiveID, "error", err)
		}
		npsJSONError(w, "failed to refresh issue status", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "items": items})
}

func validateHubFeedbackRequest(req *feedbackReportRequest) error {
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	if req.Title == "" || utf8.RuneCountInString(req.Title) > 200 {
		return errors.New("title is required and must be 200 characters or fewer")
	}
	if req.Description == "" {
		return errors.New("description is required")
	}
	if req.RequestType != feedbackTypeBug && req.RequestType != feedbackTypeFeature {
		return errors.New("request_type must be bug or feature")
	}
	if req.TargetRepo == "" {
		req.TargetRepo = feedbackTargetHive
	}
	if req.TargetRepo != feedbackTargetHive && req.TargetRepo != feedbackTargetDocs {
		return errors.New("target_repo must be hive or docs")
	}
	if len(req.Screenshots) > feedbackMaxScreenshots {
		return fmt.Errorf("at most %d screenshots are allowed", feedbackMaxScreenshots)
	}
	textBytes := len(req.Title) + len(req.Description) + len(mustHubFeedbackJSON(req.Diagnostics)) + len(mustHubFeedbackJSON(req.ConsoleErrors)) + len(mustHubFeedbackJSON(req.FailedAPICalls))
	if textBytes > feedbackMaxTextBytes {
		return errors.New("feedback text and diagnostics are too large")
	}
	for _, ss := range req.Screenshots {
		if _, _, err := decodeHubFeedbackDataURI(ss); err != nil {
			return err
		}
	}
	sanitizeHubFeedbackRequest(req)
	return nil
}
func mustHubFeedbackJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
func sanitizeHubFeedbackRequest(req *feedbackReportRequest) {
	req.Title = truncateRunes(hubFeedbackRedact(req.Title), 200)
	req.Description = truncateRunes(hubFeedbackRedact(req.Description), 5000)
	req.OpenedByHive = true
	req.CredentialLogin = hubGitHubLoginForMention(req.CredentialLogin)
	req.HubName = truncateRunes(sanitizeHubFeedbackIdentityValue(req.HubName), 160)
	sanitizeHubFeedbackSubmitter(&req.Submitter)
	if len(req.ConsoleErrors) > 20 {
		req.ConsoleErrors = req.ConsoleErrors[len(req.ConsoleErrors)-20:]
	}
	for i := range req.ConsoleErrors {
		req.ConsoleErrors[i].Message = truncateRunes(hubFeedbackRedact(req.ConsoleErrors[i].Message), 500)
		req.ConsoleErrors[i].Source = truncateRunes(hubFeedbackRedact(stripHubFeedbackQuery(req.ConsoleErrors[i].Source)), 200)
	}
	if len(req.FailedAPICalls) > 20 {
		req.FailedAPICalls = req.FailedAPICalls[len(req.FailedAPICalls)-20:]
	}
	for i := range req.FailedAPICalls {
		req.FailedAPICalls[i].Path = truncateRunes(hubFeedbackRedact(stripHubFeedbackQuery(req.FailedAPICalls[i].Path)), 200)
	}
	if !req.IncludeDiagnostics {
		req.Diagnostics = nil
	}
	if req.Diagnostics != nil {
		sanitizeHubFeedbackDiagnostics(req.Diagnostics)
	}
}
func sanitizeHubFeedbackSubmitter(s *feedbackSubmitterIdentity) {
	s.Name = truncateRunes(sanitizeHubFeedbackIdentityValue(s.Name), 120)
	s.GitHubLogin = hubGitHubLoginForMention(s.GitHubLogin)
	s.Source = truncateRunes(sanitizeHubFeedbackIdentityValue(s.Source), 80)
	if s.Name == "" {
		s.Name = "an unidentified dashboard user"
		s.Source = "unidentified dashboard user"
	}
}
func sanitizeHubFeedbackIdentityValue(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return -1
		}
		return r
	}, v)
	return hubFeedbackRedact(v)
}

// hubFeedbackGitHubLoginPattern mirrors dashboard.feedbackGitHubLoginPattern:
// GitHub logins with single, non-leading, non-trailing hyphens and an optional
// "[bot]" suffix. RE2 has no lookahead, so the length cap lives in
// hubGitHubLoginForMention.
var hubFeedbackGitHubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9])*(?:\[bot\])?$`)

// hubGitHubLoginMaxLen is GitHub's documented username length limit.
const hubGitHubLoginMaxLen = 39

func hubGitHubLoginForMention(login string) string {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if len(strings.TrimSuffix(login, "[bot]")) > hubGitHubLoginMaxLen {
		return ""
	}
	if hubFeedbackGitHubLoginPattern.MatchString(login) {
		return login
	}
	return ""
}
func sanitizeHubFeedbackDiagnostics(d *feedbackDiagnostics) {
	d.HiveID = truncateRunes(hubFeedbackRedact(d.HiveID), 120)
	d.Page = truncateRunes(hubFeedbackRedact(stripHubFeedbackQuery(d.Page)), 200)
	d.BrowserUA = truncateRunes(hubFeedbackRedact(d.BrowserUA), 300)
	if len(d.Agents) > 50 {
		d.Agents = d.Agents[:50]
	}
	for i := range d.Agents {
		d.Agents[i].Name = truncateRunes(hubFeedbackRedact(d.Agents[i].Name), 80)
		d.Agents[i].Backend = truncateRunes(hubFeedbackRedact(d.Agents[i].Backend), 80)
		d.Agents[i].Model = truncateRunes(hubFeedbackRedact(d.Agents[i].Model), 80)
		d.Agents[i].State = truncateRunes(hubFeedbackRedact(d.Agents[i].State), 80)
		if !d.IncludeProjectRepos {
			d.Agents[i].Repo = ""
			d.Agents[i].Org = ""
		}
	}
}
func hubFeedbackRedact(s string) string {
	s = feedbackTokenPattern.ReplaceAllString(s, "[REDACTED_TOKEN]")
	s = feedbackBearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = feedbackEmailPattern.ReplaceAllString(s, "[REDACTED_EMAIL]")
	return feedbackKVPattern.ReplaceAllString(s, "$1=[REDACTED]")
}
func stripHubFeedbackQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}
func hubFeedbackRepo(req feedbackReportRequest) (string, string) {
	if req.TargetRepo == feedbackTargetDocs {
		return "hivecommons", "docs"
	}
	return "hivecommons", "hive"
}
func hubFeedbackLabels(req feedbackReportRequest) []string {
	if req.RequestType == feedbackTypeBug {
		return []string{"kind/bug", "user-feedback"}
	}
	return []string{"enhancement", "user-feedback"}
}

func parseHubFeedbackRefs(raw string) ([]feedbackIssueStatus, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > feedbackMaxIssueRefs {
		return nil, fmt.Errorf("at most %d issue refs are allowed", feedbackMaxIssueRefs)
	}
	out := make([]feedbackIssueStatus, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		hash := strings.LastIndex(part, "#")
		slash := strings.Index(part, "/")
		if hash < 0 || slash < 0 || slash > hash {
			return nil, errors.New("invalid issue ref")
		}
		var number int
		if _, err := fmt.Sscanf(part[hash+1:], "%d", &number); err != nil || number <= 0 {
			return nil, errors.New("invalid issue number")
		}
		owner := part[:slash]
		repo := part[slash+1 : hash]
		if owner != "hivecommons" || (repo != "hive" && repo != "docs") {
			return nil, errors.New("issue refs must target hivecommons/hive or hivecommons/docs")
		}
		out = append(out, feedbackIssueStatus{Owner: owner, Repo: repo, Number: number})
	}
	return out, nil
}

func fetchHubFeedbackIssueStatuses(ctx context.Context, client *http.Client, apiBase, token string, refs []feedbackIssueStatus) ([]feedbackIssueStatus, error) {
	out := make([]feedbackIssueStatus, 0, len(refs))
	for _, ref := range refs {
		st, err := fetchHubFeedbackIssueStatus(ctx, client, apiBase, token, ref.Owner, ref.Repo, ref.Number)
		if err != nil {
			return out, err
		}
		out = append(out, st)
	}
	return out, nil
}

func fetchHubFeedbackIssueStatus(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, number int) (feedbackIssueStatus, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", strings.TrimRight(apiBase, "/"), owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return feedbackIssueStatus{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return feedbackIssueStatus{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return feedbackIssueStatus{}, fmt.Errorf("github issue status: %d", resp.StatusCode)
	}
	var got struct {
		Title     string `json:"title"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		UpdatedAt string `json:"updated_at"`
		Comments  int    `json:"comments"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return feedbackIssueStatus{}, err
	}
	return feedbackIssueStatus{Owner: owner, Repo: repo, Number: number, Title: got.Title, State: got.State, HTMLURL: got.HTMLURL, UpdatedAt: got.UpdatedAt, Comments: got.Comments}, nil
}

func createHubFeedbackIssue(ctx context.Context, client *http.Client, token string, req feedbackReportRequest, apiBase string) (feedbackIssueResult, string, error) {
	owner, repo := hubFeedbackRepo(req)
	labels := hubFeedbackLabels(req)
	res, status, err := postHubGitHubIssue(ctx, client, apiBase, token, owner, repo, req.Title, buildHubFeedbackIssueBody(req), labels)
	warning := ""
	if err != nil && status == http.StatusForbidden {
		res, _, err = postHubGitHubIssue(ctx, client, apiBase, token, owner, repo, req.Title, buildHubFeedbackIssueBody(req), nil)
		warning = "Created without labels because GitHub denied label access."
	}
	if err != nil {
		return feedbackIssueResult{}, "", err
	}
	valid := []string{}
	for _, ss := range req.Screenshots {
		if _, _, err := decodeHubFeedbackDataURI(ss); err == nil {
			valid = append(valid, ss)
		}
	}
	if len(valid) > 0 {
		go uploadHubFeedbackScreenshots(context.Background(), client, apiBase, token, owner, repo, res.Number, valid)
	}
	return res, warning, nil
}
func buildHubFeedbackIssueBody(req feedbackReportRequest) string {
	sanitizeHubFeedbackSubmitter(&req.Submitter)
	var b strings.Builder
	b.WriteString(hubFeedbackOpenedByLine(req))
	b.WriteString("\n\n")
	b.WriteString(req.Description)
	b.WriteString("\n\n---\nSubmitted from the Hive spoke dashboard feedback form.\n")
	if req.TargetRepo == feedbackTargetDocs {
		b.WriteString("Target: Documentation\n")
	} else {
		b.WriteString("Target: Hive\n")
	}
	if hubFeedbackSubmitterVerified(req.Submitter) {
		b.WriteString(fmt.Sprintf("/cc @%s\n", req.Submitter.GitHubLogin))
	}
	b.WriteString("\n<details>\n<summary>Diagnostics</summary>\n\n")
	writeHubFeedbackDiagnostics(&b, req.Diagnostics, req.Submitter)
	b.WriteString("\n</details>\n")
	if len(req.ConsoleErrors) > 0 {
		b.WriteString(fmt.Sprintf("\n<details>\n<summary>Browser Console Errors (%d captured)</summary>\n\n", len(req.ConsoleErrors)))
		for _, e := range req.ConsoleErrors {
			b.WriteString(fmt.Sprintf("- `[%s]` **%s**: %s\n", e.Timestamp, e.Level, e.Message))
		}
		b.WriteString("\n</details>\n")
	}
	if len(req.FailedAPICalls) > 0 {
		b.WriteString(fmt.Sprintf("\n<details>\n<summary>Failed API Calls (%d captured)</summary>\n\n", len(req.FailedAPICalls)))
		for _, c := range req.FailedAPICalls {
			b.WriteString(fmt.Sprintf("- `[%s]` %s %s\n", c.Timestamp, c.Status, c.Path))
		}
		b.WriteString("\n</details>\n")
	}
	if len(req.Screenshots) > 0 {
		b.WriteString(fmt.Sprintf("\nScreenshots: %d attached; the dashboard will upload them as issue comments.\n", len(req.Screenshots)))
	}
	return truncateRunes(b.String(), 60000)
}

func hubFeedbackOpenedByLine(req feedbackReportRequest) string {
	credential := hubFeedbackCredentialBodyText(req.CredentialLogin)
	submitter := hubFeedbackSubmitterBodyText(req.Submitter)
	hiveID := strings.TrimSpace(req.HiveID)
	if hiveID == "" && req.Diagnostics != nil {
		hiveID = strings.TrimSpace(req.Diagnostics.HiveID)
	}
	if hiveID == "" {
		hiveID = "unknown"
	}
	hub := strings.TrimSpace(req.HubName)
	if hub == "" {
		hub = "hub"
	}
	line := "Opened by " + credential
	if !hubFeedbackSameActor(req.CredentialLogin, req.Submitter.GitHubLogin) {
		line += " on behalf of " + submitter
	}
	line += " from hive " + hiveID + " (" + hub + ")"
	return line
}

func hubFeedbackCredentialBodyText(login string) string {
	login = hubGitHubLoginForMention(login)
	if login == "" {
		return "the hive"
	}
	return "@" + login
}

func hubFeedbackSameActor(credentialLogin, submitterLogin string) bool {
	credentialLogin = hubGitHubLoginForMention(credentialLogin)
	submitterLogin = hubGitHubLoginForMention(submitterLogin)
	return credentialLogin != "" && strings.EqualFold(credentialLogin, submitterLogin)
}

// hubFeedbackSourceEntered mirrors dashboard.feedbackSourceEntered: a login
// the user typed into the spoke's form, which nothing has verified.
const hubFeedbackSourceEntered = "entered GitHub username"

func hubFeedbackSubmitterVerified(s feedbackSubmitterIdentity) bool {
	return s.GitHubLogin != "" && s.Source != hubFeedbackSourceEntered
}

// hubFeedbackSubmitterBodyText mirrors dashboard.feedbackSubmitterBodyText:
// only a verified login is @-mentioned, because the hub opens the issue with
// its own credential and must not notify or misattribute to an arbitrary
// account on the strength of a self-reported handle.
func hubFeedbackSubmitterBodyText(s feedbackSubmitterIdentity) string {
	if hubFeedbackSubmitterVerified(s) {
		return "@" + s.GitHubLogin
	}
	if s.GitHubLogin != "" {
		return "`" + s.GitHubLogin + "` (self-reported GitHub username, unverified)"
	}
	return s.Name
}

func hubFeedbackSubmitterDiagnosticsText(s feedbackSubmitterIdentity) string {
	name := hubFeedbackSubmitterBodyText(s)
	if s.Source != "" && s.Source != s.Name {
		return fmt.Sprintf("%s (%s)", name, s.Source)
	}
	return name
}

func writeHubFeedbackDiagnostics(b *strings.Builder, d *feedbackDiagnostics, submitter feedbackSubmitterIdentity) {
	b.WriteString("| Field | Value |\n|---|---|\n")
	submitterText := hubFeedbackSubmitterDiagnosticsText(submitter)
	if d == nil {
		b.WriteString(fmt.Sprintf("| Submitted by | %s |\n", strings.ReplaceAll(submitterText, "|", "\\|")))
		return
	}
	rows := [][2]string{{"Submitted by", submitterText}, {"Version", d.Version}, {"Commit", d.Commit}, {"Channel", d.Channel}, {"ACMM Level", d.ACMMLevel}, {"Hive ID", d.HiveID}, {"Hosted", fmt.Sprintf("%t", d.Hosted)}, {"Hub linked", fmt.Sprintf("%t", d.HubLinked)}, {"Agent count", fmt.Sprintf("%d", d.AgentCount)}, {"Browser UA", d.BrowserUA}, {"Browser platform", d.BrowserPlatform}, {"Browser language", d.BrowserLanguage}, {"Screen", d.ScreenSize}, {"Window", d.WindowSize}, {"Page", d.Page}}
	for _, r := range rows {
		if r[1] != "" {
			b.WriteString(fmt.Sprintf("| %s | %s |\n", r[0], strings.ReplaceAll(r[1], "|", "\\|")))
		}
	}
	if len(d.Agents) > 0 {
		b.WriteString("\nAgents:\n")
		for _, a := range d.Agents {
			b.WriteString(fmt.Sprintf("- %s: backend=%s model=%s state=%s\n", a.Name, a.Backend, a.Model, a.State))
		}
	}
}
func postHubGitHubIssue(ctx context.Context, client *http.Client, apiBase, token, owner, repo, title, body string, labels []string) (feedbackIssueResult, int, error) {
	payload := map[string]any{"title": title, "body": body}
	if labels != nil {
		payload["labels"] = labels
	}
	data, _ := json.Marshal(payload)
	u := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo + "/issues"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return feedbackIssueResult{}, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return feedbackIssueResult{}, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return feedbackIssueResult{}, resp.StatusCode, fmt.Errorf("github create issue: %d", resp.StatusCode)
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		ID      int64  `json:"id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return feedbackIssueResult{}, resp.StatusCode, err
	}
	return feedbackIssueResult{Number: out.Number, URL: out.HTMLURL, ID: out.ID}, resp.StatusCode, nil
}

// feedbackScreenshotExtensions maps the sniffed content type of a decoded
// screenshot to the file extension it is stored under. Only these types are
// accepted: the declared data-URI media type is never trusted on its own.
var feedbackScreenshotExtensions = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
}

// decodeHubFeedbackDataURI decodes one screenshot and returns its bytes with
// the extension matching its sniffed content type. The bytes must actually be
// a PNG or JPEG image; any other content is rejected regardless of the media
// type the data URI claims.
func decodeHubFeedbackDataURI(dataURI string) ([]byte, string, error) {
	parts := strings.SplitN(dataURI, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "data:image/") {
		return nil, "", errors.New("screenshots must be image data URIs")
	}
	b, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, "", errors.New("invalid screenshot data")
	}
	if len(b) > feedbackMaxScreenshotBytes {
		return nil, "", fmt.Errorf("each screenshot must be %d MiB or smaller", feedbackMaxScreenshotBytes>>20)
	}
	ext, ok := feedbackScreenshotExtensions[http.DetectContentType(b)]
	if !ok {
		return nil, "", errors.New("screenshots must be PNG or JPEG images")
	}
	return b, ext, nil
}
func uploadHubFeedbackScreenshots(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, issue int, screenshots []string) {
	_, _ = postHubGitHubComment(ctx, client, apiBase, token, owner, repo, issue, "Processing feedback screenshots…")
	lines := []string{}
	for i, ss := range screenshots {
		content, ext, err := decodeHubFeedbackDataURI(ss)
		if err != nil {
			continue
		}
		path := fmt.Sprintf("%s/%d/screenshot-%d.%s", feedbackScreenshotDir, issue, i+1, ext)
		dl, err := putHubGitHubContent(ctx, client, apiBase, token, owner, repo, path, content)
		if err == nil && dl != "" {
			lines = append(lines, fmt.Sprintf("![screenshot %d](%s)", i+1, dl))
		}
	}
	if len(lines) > 0 {
		_, _ = postHubGitHubComment(ctx, client, apiBase, token, owner, repo, issue, "Feedback screenshots:\n\n"+strings.Join(lines, "\n\n"))
	}
}

// putHubGitHubContent commits one screenshot to the dedicated
// feedbackScreenshotBranch, never to the repository's default branch. The
// branch is created from the default branch head on first use.
func putHubGitHubContent(ctx context.Context, client *http.Client, apiBase, token, owner, repo, path string, content []byte) (string, error) {
	dl, status, err := putHubGitHubContentOnBranch(ctx, client, apiBase, token, owner, repo, path, content)
	if err == nil || (status != http.StatusNotFound && status != http.StatusUnprocessableEntity) {
		return dl, err
	}
	if err := ensureHubGitHubBranch(ctx, client, apiBase, token, owner, repo, feedbackScreenshotBranch); err != nil {
		return "", err
	}
	dl, _, err = putHubGitHubContentOnBranch(ctx, client, apiBase, token, owner, repo, path, content)
	return dl, err
}
func putHubGitHubContentOnBranch(ctx context.Context, client *http.Client, apiBase, token, owner, repo, path string, content []byte) (string, int, error) {
	payload := map[string]string{
		"message": "Add feedback screenshot",
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  feedbackScreenshotBranch,
	}
	data, _ := json.Marshal(payload)
	u := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo + "/contents/" + url.PathEscape(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(data))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", resp.StatusCode, fmt.Errorf("github upload: %d", resp.StatusCode)
	}
	var out struct {
		Content struct {
			DownloadURL string `json:"download_url"`
		} `json:"content"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.Content.DownloadURL, resp.StatusCode, nil
}

// ensureHubGitHubBranch creates `branch` from the repository's default branch
// head when it does not exist yet. An already-existing branch is left alone.
func ensureHubGitHubBranch(ctx context.Context, client *http.Client, apiBase, token, owner, repo, branch string) error {
	base := strings.TrimRight(apiBase, "/") + "/repos/" + owner + "/" + repo
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := hubGitHubJSON(ctx, client, token, http.MethodGet, base, nil, &repoInfo); err != nil {
		return err
	}
	if repoInfo.DefaultBranch == "" {
		return errors.New("github: default branch unknown")
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := hubGitHubJSON(ctx, client, token, http.MethodGet, base+"/git/ref/"+url.PathEscape("heads/"+repoInfo.DefaultBranch), nil, &ref); err != nil {
		return err
	}
	if ref.Object.SHA == "" {
		return errors.New("github: default branch head unknown")
	}
	payload := map[string]string{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}
	status, err := hubGitHubJSON(ctx, client, token, http.MethodPost, base+"/git/refs", payload, nil)
	// 422 means the ref already exists (a concurrent upload created it).
	if err != nil && status != http.StatusUnprocessableEntity {
		return err
	}
	return nil
}
func hubGitHubJSON(ctx context.Context, client *http.Client, token, method, u string, payload any, out any) (int, error) {
	var body io.Reader
	if payload != nil {
		data, _ := json.Marshal(payload)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("github %s: %d", method, resp.StatusCode)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}
func postHubGitHubComment(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, issue int, body string) (string, error) {
	payload := map[string]string{"body": body}
	data, _ := json.Marshal(payload)
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", strings.TrimRight(apiBase, "/"), owner, repo, issue)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github comment: %d", resp.StatusCode)
	}
	return "", nil
}

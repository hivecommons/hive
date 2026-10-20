package dashboard

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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/feedbackbundle"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

const (
	feedbackMaxRequestBytes     = 15 << 20
	feedbackMaxTextBytes        = 64 << 10
	feedbackMaxScreenshots      = 5
	feedbackMaxScreenshotBytes  = 2 << 20
	feedbackHubIngestPath       = "/api/feedback/ingest"
	feedbackHubIssuesPath       = "/api/feedback/issues"
	feedbackForwardTimeout      = 20 * time.Second
	feedbackMaxHubResponseBytes = 8 << 10
	feedbackTargetHive          = "hive"
	feedbackTargetDocs          = "docs"
	feedbackTypeBug             = "bug"
	feedbackTypeFeature         = "feature"
	feedbackPrefillBaseHive     = "https://github.com/hivecommons/hive/issues/new"
	feedbackPrefillBaseDocs     = "https://github.com/hivecommons/docs/issues/new"
	feedbackSubmissionsFileMode = 0o600
	feedbackSubmissionsDirMode  = 0o755
	feedbackMaxMineRefs         = 50
)

var feedbackSubmissionsPath = "/data/feedback-submissions.json"

type feedbackConsoleError struct {
	Timestamp string `json:"timestamp,omitempty"`
	Level     string `json:"level,omitempty"`
	Message   string `json:"message,omitempty"`
	Source    string `json:"source,omitempty"`
}

func sanitizeFeedbackSubmitter(s *feedbackSubmitterIdentity) {
	s.Name = truncateRunes(sanitizeFeedbackIdentityValue(s.Name), 120)
	s.GitHubLogin = githubLoginForMention(s.GitHubLogin)
	s.Source = truncateRunes(sanitizeFeedbackIdentityValue(s.Source), 80)
	if s.Name == "" {
		s.Name = "an unidentified dashboard user"
		s.Source = "unidentified dashboard user"
	}
}

func sanitizeFeedbackIdentityValue(v string) string {
	v = strings.TrimSpace(v)
	v = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return -1
		}
		return r
	}, v)
	return feedbackRedact(v)
}

// feedbackGitHubLoginPattern accepts GitHub logins: alphanumerics with single,
// non-leading, non-trailing hyphens, plus an optional "[bot]" suffix. Go's RE2
// engine has no lookahead, so "no double hyphen" is expressed by requiring
// every hyphen to be followed by an alphanumeric; the 39-character cap is
// enforced separately in githubLoginForMention.
var feedbackGitHubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:-?[A-Za-z0-9])*(?:\[bot\])?$`)

// gitHubLoginMaxLen is GitHub's documented username length limit.
const gitHubLoginMaxLen = 39

func githubLoginForMention(login string) string {
	login = strings.TrimPrefix(strings.TrimSpace(login), "@")
	if len(strings.TrimSuffix(login, "[bot]")) > gitHubLoginMaxLen {
		return ""
	}
	if feedbackGitHubLoginPattern.MatchString(login) {
		return login
	}
	return ""
}

type feedbackSubmissionRecord struct {
	Owner             string `json:"owner"`
	Repo              string `json:"repo"`
	Number            int    `json:"number"`
	Title             string `json:"title,omitempty"`
	State             string `json:"state,omitempty"`
	HTMLURL           string `json:"html_url,omitempty"`
	SubmittedAt       string `json:"submitted_at"`
	UpdatedAt         string `json:"updated_at,omitempty"`
	Comments          int    `json:"comments,omitempty"`
	LastSeenUpdatedAt string `json:"last_seen_updated_at,omitempty"`
}

// feedbackSubmitterVerified reports whether the submitter login came from an
// authenticated identity rather than free text typed into the form.
func feedbackSubmitterVerified(s feedbackSubmitterIdentity) bool {
	return s.GitHubLogin != "" && s.Source != feedbackSourceEntered
}

// feedbackSubmitterBodyText renders the submitter for the issue body. Only a
// verified login is @-mentioned: the issue is opened with the hive's own
// credential, so mentioning a self-reported handle would let any dashboard
// user make the bot notify (and misattribute feedback to) an arbitrary
// GitHub account.
func feedbackSubmitterBodyText(s feedbackSubmitterIdentity) string {
	if feedbackSubmitterVerified(s) {
		return "@" + s.GitHubLogin
	}
	if s.GitHubLogin != "" {
		return "`" + s.GitHubLogin + "` (self-reported GitHub username, unverified)"
	}
	return s.Name
}

func feedbackSubmitterDiagnosticsText(s feedbackSubmitterIdentity) string {
	name := feedbackSubmitterBodyText(s)
	if s.Source != "" && s.Source != s.Name {
		return fmt.Sprintf("%s (%s)", name, s.Source)
	}
	return name
}

type feedbackMineResponse struct {
	OK      bool                       `json:"ok"`
	Items   []feedbackSubmissionRecord `json:"items"`
	Unread  int                        `json:"unread"`
	Warning string                     `json:"warning,omitempty"`
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

type feedbackAttributionContext struct {
	CredentialLogin string `json:"credential_login,omitempty"`
	SubmitterLogin  string `json:"submitter_login,omitempty"`
	SubmitterName   string `json:"submitter_name,omitempty"`
	SubmitterSource string `json:"submitter_source,omitempty"`
	HiveID          string `json:"hive_id,omitempty"`
	HubLinked       bool   `json:"hub_linked"`
	HubName         string `json:"hub_name,omitempty"`
	NeedsIdentity   bool   `json:"needs_identity"`
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

// feedbackReportResponse is what POST /api/feedback/report returns. Since the
// zero-scope handoff (#1927) the hive never creates the issue itself: it stores
// the diagnostic bundle with its GitHub App installation token and returns the
// prefilled issues/new URL for the user to submit as themselves. IssueNumber /
// IssueURL remain only for hub relay compatibility with older spokes.
type feedbackReportResponse struct {
	OK          bool   `json:"ok"`
	IssueNumber int    `json:"issue_number,omitempty"`
	IssueURL    string `json:"issue_url,omitempty"`
	PrefillURL  string `json:"prefill_url,omitempty"`
	BundleURL   string `json:"bundle_url,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

var (
	feedbackTokenPattern  = regexp.MustCompile(`(?i)(ghp_|ghs_|ghu_|ghr_|github_pat_)[A-Za-z0-9_]+`)
	feedbackBearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`)
	feedbackEmailPattern  = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	feedbackKVPattern     = regexp.MustCompile(`(?i)(secret|token|password|key)\s*[:=]\s*[^\s,;]+`)
)

func (s *Server) handleFeedbackStatus(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, s.feedbackAttributionContext(r, ""))
}

func (s *Server) handleFeedbackReport(w http.ResponseWriter, r *http.Request) {
	reject := func(msg string, status int, err error) {
		if s.logger != nil {
			s.logger.Warn("feedback: report rejected", "status", status, "reason", msg, "error", err)
		}
		jsonError(w, msg, status)
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, feedbackMaxRequestBytes+1))
	if err != nil {
		reject("could not read request", http.StatusBadRequest, err)
		return
	}
	if len(body) > feedbackMaxRequestBytes {
		reject("request too large", http.StatusRequestEntityTooLarge, nil)
		return
	}
	var req feedbackReportRequest
	if err := json.Unmarshal(body, &req); err != nil {
		reject("invalid JSON", http.StatusBadRequest, err)
		return
	}
	if err := validateFeedbackRequest(&req); err != nil {
		reject(err.Error(), http.StatusBadRequest, err)
		return
	}
	sanitizeFeedbackRequest(&req)
	ctx := s.feedbackAttributionContext(r, req.Submitter.GitHubLogin)
	req.Submitter = feedbackSubmitterIdentity{Name: ctx.SubmitterName, GitHubLogin: ctx.SubmitterLogin}
	req.Submitter.Source = ctx.SubmitterSource
	// Nobody but the user authors the issue: not the operator PAT, not the App
	// bot, and — because login requests no OAuth scope (#1927) — not a user
	// token either. The hive only stores the bundle.
	req.CredentialLogin = ""
	req.HubName = ctx.HubName
	if req.HiveID == "" {
		req.HiveID = ctx.HiveID
	}

	bundleURL, warning := s.storeFeedbackBundle(r.Context(), req)
	prefill := feedbackPrefillURL(req, bundleURL)
	via := "prefill"
	if bundleURL != "" {
		via = "prefill+bundle"
	}
	s.auditFromRequest(r, "feedback_submit", auditDetail("target", req.TargetRepo, "type", req.RequestType, "via", via), "")
	jsonResponse(w, feedbackReportResponse{OK: true, PrefillURL: prefill, BundleURL: bundleURL, Warning: warning})
}

// storeFeedbackBundle persists the full diagnostic report (and screenshots) so
// the prefilled issue can link to it. Hub-linked spokes relay to the hub, which
// stores it with the spoke's GitHub App installation token; hub-less spokes use
// their own installation token. Returns "" (with a user-facing warning) when
// no installation token is available — the prefilled issue still opens, it
// just carries no bundle link.
func (s *Server) storeFeedbackBundle(ctx context.Context, req feedbackReportRequest) (bundleURL, warning string) {
	const noBundle = "The diagnostic bundle could not be stored; the prefilled issue carries only your summary."
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.NPSHubLinked() {
		req.HiveID = strings.TrimSpace(s.deps.Config.HiveID)
		resp, status, err := s.forwardFeedbackToHub(ctx, req)
		if err != nil {
			if s.logger != nil {
				s.logger.Warn("feedback: hub bundle relay failed", "status", status, "error", err)
			}
			return "", noBundle
		}
		return strings.TrimSpace(resp.BundleURL), strings.TrimSpace(resp.Warning)
	}
	token := s.feedbackAppIssueToken(ctx, nil)
	if token == "" {
		return "", noBundle
	}
	owner, repo := feedbackRepo(req)
	stored, err := feedbackbundle.Store(ctx, http.DefaultClient, feedbackGitHubAPIBase(), token, owner, repo,
		feedbackbundle.NewID(time.Now()), buildFeedbackBundleReport(req), feedbackScreenshotsForBundle(req))
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("feedback: bundle store failed", "error", err)
		}
		return "", noBundle
	}
	return stored.ReportURL, ""
}

// feedbackScreenshotsForBundle decodes the accepted screenshots; invalid ones
// are skipped rather than failing the report.
func feedbackScreenshotsForBundle(req feedbackReportRequest) []feedbackbundle.Screenshot {
	out := make([]feedbackbundle.Screenshot, 0, len(req.Screenshots))
	for _, ss := range req.Screenshots {
		if content, ext, err := decodeFeedbackDataURI(ss); err == nil {
			out = append(out, feedbackbundle.Screenshot{Content: content, Ext: ext})
		}
	}
	return out
}

func (s *Server) handleFeedbackMine(w http.ResponseWriter, r *http.Request) {
	if !s.feedbackMineAuthorized(r) {
		jsonError(w, "you must be signed in to view feedback reports", http.StatusForbidden)
		return
	}
	items, err := loadFeedbackSubmissions()
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("feedback: could not load submissions", "error", err)
		}
		jsonError(w, "could not load feedback reports", http.StatusInternalServerError)
		return
	}
	var warning string
	if len(items) > 0 {
		statuses, err := s.refreshFeedbackIssueStatuses(r.Context(), r, items)
		if err != nil {
			warning = "Could not refresh issue activity right now."
			if s.logger != nil {
				s.logger.Warn("feedback: issue refresh failed", "error", err)
			}
		} else {
			items = mergeFeedbackStatuses(items, statuses)
		}
	}
	if r.URL.Query().Get("mark_seen") == "true" {
		for i := range items {
			items[i].LastSeenUpdatedAt = items[i].UpdatedAt
			if items[i].LastSeenUpdatedAt == "" {
				items[i].LastSeenUpdatedAt = time.Now().UTC().Format(time.RFC3339)
			}
		}
		if err := saveFeedbackSubmissions(items); err != nil && s.logger != nil {
			s.logger.Warn("feedback: could not mark submissions seen", "error", err)
		}
	} else if warning == "" {
		_ = saveFeedbackSubmissions(items)
	}
	jsonResponse(w, feedbackMineResponse{OK: true, Items: items, Unread: feedbackUnreadCount(items), Warning: warning})
}

func (s *Server) feedbackMineAuthorized(r *http.Request) bool {
	if r.Header.Get(ownerRoleVerifiedHeader) == "true" {
		return true
	}
	if sess := s.sessionFromRequest(r); sess != nil {
		_, ok := s.liveSessionRole(sess)
		return ok
	}
	return r.Header.Get("X-Hive-User") != ""
}

func (s *Server) refreshFeedbackIssueStatuses(ctx context.Context, r *http.Request, items []feedbackSubmissionRecord) ([]feedbackIssueStatus, error) {
	refs := feedbackRefs(items)
	if len(refs) == 0 {
		return nil, nil
	}
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.NPSHubLinked() {
		return s.fetchFeedbackStatusesFromHub(ctx, refs)
	}
	// Read with the App installation token when available, otherwise
	// anonymously — hivecommons/hive and hivecommons/docs are public. The
	// owner's login token is never used here (#1927: it has no scope anyway).
	return fetchFeedbackIssueStatuses(ctx, http.DefaultClient, feedbackGitHubAPIBase(), s.feedbackAppIssueToken(ctx, nil), refs)
}

func (s *Server) fetchFeedbackStatusesFromHub(ctx context.Context, refs []feedbackSubmissionRecord) ([]feedbackIssueStatus, error) {
	hub := s.deps.Config.Hub
	bearer := spoke.SpokeHeartbeatKey()
	if bearer == "" {
		return nil, errors.New("no hub credential configured")
	}
	hiveID := strings.TrimSpace(s.deps.Config.HiveID)
	values := url.Values{}
	values.Set("hive_id", hiveID)
	values.Set("refs", encodeFeedbackRefs(refs))
	endpoint := strings.TrimRight(strings.TrimSpace(hub.URL), "/") + feedbackHubIssuesPath + "?" + values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := npsNoRedirectClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer closeHTTPBody(resp.Body)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, feedbackMaxHubResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("hub answered %d", resp.StatusCode)
	}
	var out struct {
		OK    bool                  `json:"ok"`
		Items []feedbackIssueStatus `json:"items"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

func loadFeedbackSubmissions() ([]feedbackSubmissionRecord, error) {
	raw, err := os.ReadFile(feedbackSubmissionsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var f struct {
		Items []feedbackSubmissionRecord `json:"items"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if len(f.Items) > feedbackMaxMineRefs {
		f.Items = f.Items[:feedbackMaxMineRefs]
	}
	return f.Items, nil
}

func saveFeedbackSubmissions(items []feedbackSubmissionRecord) error {
	if len(items) > feedbackMaxMineRefs {
		items = items[:feedbackMaxMineRefs]
	}
	data, err := json.MarshalIndent(struct {
		Items []feedbackSubmissionRecord `json:"items"`
	}{Items: items}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(feedbackSubmissionsPath)
	if err := os.MkdirAll(dir, feedbackSubmissionsDirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".feedback-submissions-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(feedbackSubmissionsFileMode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, feedbackSubmissionsPath)
}

func feedbackRefs(items []feedbackSubmissionRecord) []feedbackSubmissionRecord {
	seen := map[string]bool{}
	refs := make([]feedbackSubmissionRecord, 0, len(items))
	for _, item := range items {
		if item.Owner == "" || item.Repo == "" || item.Number <= 0 {
			continue
		}
		key := strings.ToLower(item.Owner + "/" + item.Repo + "#" + fmt.Sprintf("%d", item.Number))
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, item)
		if len(refs) >= feedbackMaxMineRefs {
			break
		}
	}
	return refs
}

func encodeFeedbackRefs(refs []feedbackSubmissionRecord) string {
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		parts = append(parts, fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number))
	}
	return strings.Join(parts, ",")
}

func mergeFeedbackStatuses(items []feedbackSubmissionRecord, statuses []feedbackIssueStatus) []feedbackSubmissionRecord {
	byKey := map[string]feedbackIssueStatus{}
	for _, st := range statuses {
		byKey[strings.ToLower(fmt.Sprintf("%s/%s#%d", st.Owner, st.Repo, st.Number))] = st
	}
	for i := range items {
		key := strings.ToLower(fmt.Sprintf("%s/%s#%d", items[i].Owner, items[i].Repo, items[i].Number))
		st, ok := byKey[key]
		if !ok {
			continue
		}
		if st.Title != "" {
			items[i].Title = st.Title
		}
		if st.State != "" {
			items[i].State = st.State
		}
		if st.HTMLURL != "" {
			items[i].HTMLURL = st.HTMLURL
		}
		if st.UpdatedAt != "" {
			items[i].UpdatedAt = st.UpdatedAt
		}
		items[i].Comments = st.Comments
	}
	return items
}

func feedbackUnreadCount(items []feedbackSubmissionRecord) int {
	n := 0
	for _, item := range items {
		if item.UpdatedAt != "" && item.LastSeenUpdatedAt != "" && item.UpdatedAt > item.LastSeenUpdatedAt {
			n++
		}
	}
	return n
}

func validateFeedbackRequest(req *feedbackReportRequest) error {
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
	textBytes := len(req.Title) + len(req.Description) + len(mustFeedbackJSON(req.Diagnostics)) + len(mustFeedbackJSON(req.ConsoleErrors)) + len(mustFeedbackJSON(req.FailedAPICalls))
	if textBytes > feedbackMaxTextBytes {
		return errors.New("feedback text and diagnostics are too large")
	}
	for _, ss := range req.Screenshots {
		if _, _, err := decodeFeedbackDataURI(ss); err != nil {
			return err
		}
	}
	return nil
}

func mustFeedbackJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func sanitizeFeedbackRequest(req *feedbackReportRequest) {
	req.Title = truncateRunes(feedbackRedact(req.Title), 200)
	req.Description = truncateRunes(feedbackRedact(req.Description), 5000)
	req.CredentialLogin = githubLoginForMention(req.CredentialLogin)
	req.HubName = truncateRunes(sanitizeFeedbackIdentityValue(req.HubName), 160)
	sanitizeFeedbackSubmitter(&req.Submitter)
	if len(req.ConsoleErrors) > 20 {
		req.ConsoleErrors = req.ConsoleErrors[len(req.ConsoleErrors)-20:]
	}
	for i := range req.ConsoleErrors {
		req.ConsoleErrors[i].Message = truncateRunes(feedbackRedact(req.ConsoleErrors[i].Message), 500)
		req.ConsoleErrors[i].Source = truncateRunes(feedbackRedact(stripQuery(req.ConsoleErrors[i].Source)), 200)
	}
	if len(req.FailedAPICalls) > 20 {
		req.FailedAPICalls = req.FailedAPICalls[len(req.FailedAPICalls)-20:]
	}
	for i := range req.FailedAPICalls {
		req.FailedAPICalls[i].Path = truncateRunes(feedbackRedact(stripQuery(req.FailedAPICalls[i].Path)), 200)
		req.FailedAPICalls[i].Status = truncateRunes(feedbackRedact(req.FailedAPICalls[i].Status), 40)
	}
	if !req.IncludeDiagnostics {
		req.Diagnostics = nil
		return
	}
	if req.Diagnostics != nil {
		sanitizeFeedbackDiagnostics(req.Diagnostics)
	}
}

func sanitizeFeedbackDiagnostics(d *feedbackDiagnostics) {
	d.Version = truncateRunes(feedbackRedact(d.Version), 80)
	d.Commit = truncateRunes(feedbackRedact(d.Commit), 80)
	d.Channel = truncateRunes(feedbackRedact(d.Channel), 40)
	d.ACMMLevel = truncateRunes(feedbackRedact(d.ACMMLevel), 40)
	d.HiveID = truncateRunes(feedbackRedact(d.HiveID), 120)
	d.BrowserUA = truncateRunes(feedbackRedact(d.BrowserUA), 300)
	d.BrowserPlatform = truncateRunes(feedbackRedact(d.BrowserPlatform), 80)
	d.BrowserLanguage = truncateRunes(feedbackRedact(d.BrowserLanguage), 40)
	d.ScreenSize = truncateRunes(feedbackRedact(d.ScreenSize), 40)
	d.WindowSize = truncateRunes(feedbackRedact(d.WindowSize), 40)
	d.Page = truncateRunes(feedbackRedact(stripQuery(d.Page)), 200)
	if len(d.Agents) > 50 {
		d.Agents = d.Agents[:50]
	}
	for i := range d.Agents {
		d.Agents[i].Name = truncateRunes(feedbackRedact(d.Agents[i].Name), 80)
		d.Agents[i].Backend = truncateRunes(feedbackRedact(d.Agents[i].Backend), 80)
		d.Agents[i].Model = truncateRunes(feedbackRedact(d.Agents[i].Model), 80)
		d.Agents[i].State = truncateRunes(feedbackRedact(d.Agents[i].State), 80)
		if !d.IncludeProjectRepos {
			d.Agents[i].Repo = ""
			d.Agents[i].Org = ""
		} else {
			d.Agents[i].Repo = truncateRunes(feedbackRedact(d.Agents[i].Repo), 160)
			d.Agents[i].Org = truncateRunes(feedbackRedact(d.Agents[i].Org), 120)
		}
	}
}

func feedbackRedact(s string) string {
	s = feedbackTokenPattern.ReplaceAllString(s, "[REDACTED_TOKEN]")
	s = feedbackBearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	s = feedbackEmailPattern.ReplaceAllString(s, "[REDACTED_EMAIL]")
	s = feedbackKVPattern.ReplaceAllString(s, "$1=[REDACTED]")
	return s
}
func stripQuery(s string) string {
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return s[:i]
	}
	return s
}

func (s *Server) forwardFeedbackToHub(ctx context.Context, req feedbackReportRequest) (feedbackReportResponse, int, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return feedbackReportResponse{}, 0, err
	}
	bearer := spoke.SpokeHeartbeatKey()
	if bearer == "" {
		return feedbackReportResponse{}, 0, errors.New("no hub credential configured")
	}
	ctx, cancel := context.WithTimeout(ctx, feedbackForwardTimeout)
	defer cancel()
	endpoint := strings.TrimRight(strings.TrimSpace(s.deps.Config.Hub.URL), "/") + feedbackHubIngestPath
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return feedbackReportResponse{}, 0, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := npsNoRedirectClient().Do(hreq)
	if err != nil {
		return feedbackReportResponse{}, 0, err
	}
	defer closeHTTPBody(resp.Body)
	b, _ := io.ReadAll(io.LimitReader(resp.Body, feedbackMaxHubResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return feedbackReportResponse{}, resp.StatusCode, fmt.Errorf("hub answered %d", resp.StatusCode)
	}
	var out feedbackReportResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return feedbackReportResponse{}, resp.StatusCode, err
	}
	return out, resp.StatusCode, nil
}

// feedbackAppIssueToken mints the hive's GitHub App installation token. It is
// used to STORE the diagnostic bundle and to READ issue statuses — never to
// author an issue. The operator PAT / github.token is never consulted here.
func (s *Server) feedbackAppIssueToken(ctx context.Context, req *feedbackReportRequest) string {
	if s == nil || s.deps == nil || s.deps.Config == nil || s.deps.GHAppAuth == nil {
		return ""
	}
	if token, err := s.deps.GHAppAuth.Token(ctx); err == nil && strings.TrimSpace(token) != "" {
		if req != nil {
			req.OpenedByHive = true
			req.CredentialLogin = githubLoginForMention(s.deps.Config.GitHub.BotLogin())
			if req.CredentialLogin == "" {
				req.CredentialLogin = config.DefaultGitHubAppSlug + "[bot]"
			}
		}
		return strings.TrimSpace(token)
	} else if s.logger != nil && err != nil {
		s.logger.Warn("feedback: could not mint GitHub App token", "error", err)
	}
	return ""
}

func (s *Server) feedbackAttributionContext(r *http.Request, enteredLogin string) feedbackAttributionContext {
	ctx := feedbackAttributionContext{HubName: "hub-less"}
	if s != nil && s.deps != nil && s.deps.Config != nil {
		cfg := s.deps.Config
		ctx.HiveID = strings.TrimSpace(cfg.HiveID)
		ctx.HubLinked = cfg.Hub.NPSHubLinked()
		if ctx.HubLinked {
			ctx.HubName = feedbackHubDisplayName(cfg)
		}
	}
	submitter := s.feedbackSubmitterIdentity(r, enteredLogin)
	ctx.SubmitterName = submitter.Name
	ctx.SubmitterLogin = submitter.GitHubLogin
	ctx.SubmitterSource = submitter.Source
	// CredentialLogin stays empty on purpose: the user opens the issue on
	// GitHub as themselves (#1927 zero-scope handoff); the hive only stores
	// the diagnostic bundle with its App installation token.
	ctx.NeedsIdentity = ctx.SubmitterLogin == ""
	return ctx
}

func feedbackHubDisplayName(cfg *config.Config) string {
	if cfg == nil {
		return "hub"
	}
	if u := strings.TrimSpace(cfg.Hub.URL); u != "" {
		return u
	}
	return "hub"
}

// feedbackSourceEntered marks a submitter login the user typed into the form.
// Nothing verifies it, so renderers must not @-mention it (see
// feedbackSubmitterBodyText).
const feedbackSourceEntered = "entered GitHub username"

func (s *Server) feedbackSubmitterIdentity(r *http.Request, enteredLogin string) feedbackSubmitterIdentity {
	// A verified identity always wins: an entered login is only a fallback for
	// sessions that carry no GitHub login of their own.
	if s != nil {
		if sess := s.sessionFromRequest(r); sess != nil && strings.TrimSpace(sess.Username) != "" {
			name := sanitizeFeedbackIdentityValue(sess.Username)
			login := githubLoginForMention(name)
			if login != "" {
				return feedbackSubmitterIdentity{Name: login, GitHubLogin: login, Source: "GitHub dashboard identity"}
			}
			if entered := githubLoginForMention(enteredLogin); entered != "" {
				return feedbackSubmitterIdentity{Name: entered, GitHubLogin: entered, Source: feedbackSourceEntered}
			}
			return feedbackSubmitterIdentity{Name: name, Source: "authenticated dashboard user"}
		}
	}
	if login := githubLoginForMention(enteredLogin); login != "" {
		return feedbackSubmitterIdentity{Name: login, GitHubLogin: login, Source: feedbackSourceEntered}
	}
	if user := sanitizeFeedbackIdentityValue(r.Header.Get("X-Hive-User")); user != "" {
		return feedbackSubmitterIdentity{Name: user, Source: "authenticated dashboard user"}
	}
	return feedbackSubmitterIdentity{Name: "an unidentified dashboard user", Source: "unidentified dashboard user"}
}

var feedbackGitHubAPIBase = func() string { return "https://api.github.com" }

func feedbackRepo(req feedbackReportRequest) (owner, repo string) {
	if req.TargetRepo == feedbackTargetDocs {
		return "hivecommons", "docs"
	}
	return "hivecommons", "hive"
}
func feedbackLabels(req feedbackReportRequest) []string {
	if req.RequestType == feedbackTypeBug {
		return []string{"kind/bug", "user-feedback"}
	}
	return []string{"enhancement", "user-feedback"}
}

func fetchFeedbackIssueStatuses(ctx context.Context, client *http.Client, apiBase, token string, refs []feedbackSubmissionRecord) ([]feedbackIssueStatus, error) {
	out := make([]feedbackIssueStatus, 0, len(refs))
	for _, ref := range refs {
		st, err := fetchFeedbackIssueStatus(ctx, client, apiBase, token, ref.Owner, ref.Repo, ref.Number)
		if err != nil {
			return out, err
		}
		out = append(out, st)
	}
	return out, nil
}

func fetchFeedbackIssueStatus(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, number int) (feedbackIssueStatus, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", strings.TrimRight(apiBase, "/"), owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return feedbackIssueStatus{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return feedbackIssueStatus{}, err
	}
	defer closeHTTPBody(resp.Body)
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

// buildFeedbackBundleReport renders the full diagnostic report stored in the
// feedback bundle (not the issue body — that is the short prefill, see
// feedbackPrefillURL). The 60 000-rune cap matches GitHub's rendered-markdown
// comfort zone; stored files have no hard limit.
func buildFeedbackBundleReport(req feedbackReportRequest) string {
	sanitizeFeedbackSubmitter(&req.Submitter)
	var b strings.Builder
	b.WriteString("# " + strings.TrimSpace(req.Title) + "\n\n")
	b.WriteString(feedbackPrefillOpenedByLine(req))
	b.WriteString("\n\n")
	b.WriteString(req.Description)
	b.WriteString("\n\n---\nDiagnostic bundle collected by the Hive spoke dashboard feedback form; stored by the hive's GitHub App installation, never by a user or operator token.\n")
	if req.TargetRepo == feedbackTargetDocs {
		b.WriteString("Target: Documentation\n")
	} else {
		b.WriteString("Target: Hive\n")
	}
	if feedbackSubmitterVerified(req.Submitter) {
		b.WriteString(fmt.Sprintf("/cc @%s\n", req.Submitter.GitHubLogin))
	}
	if req.Diagnostics != nil {
		writeFeedbackDiagnosticsIncluded(&b, req)
	}
	b.WriteString("\n<details>\n<summary>Diagnostics</summary>\n\n")
	if req.Diagnostics != nil {
		writeFeedbackDiagnostics(&b, req.Diagnostics, req.Submitter)
	} else {
		writeFeedbackDiagnostics(&b, nil, req.Submitter)
	}
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
	return truncateRunes(b.String(), 60000)
}

// feedbackPrefillOpenedByLine is the first line of both the prefilled issue
// body and the bundle report. The USER opens the issue, so it names them (when
// the dashboard verified their GitHub login) rather than any hive credential.
func feedbackPrefillOpenedByLine(req feedbackReportRequest) string {
	hiveID := strings.TrimSpace(req.HiveID)
	if hiveID == "" && req.Diagnostics != nil {
		hiveID = strings.TrimSpace(req.Diagnostics.HiveID)
	}
	if hiveID == "" {
		hiveID = "unknown"
	}
	hub := strings.TrimSpace(req.HubName)
	if hub == "" {
		hub = "hub-less"
	}
	return "Opened by " + feedbackSubmitterBodyText(req.Submitter) + " from hive " + hiveID + " (" + hub + ")"
}

type feedbackDiagnosticDisclosureRow struct {
	Field           string
	Why             string
	OptionalProject bool
}

func feedbackDiagnosticDisclosureRows() []feedbackDiagnosticDisclosureRow {
	return []feedbackDiagnosticDisclosureRow{
		{"Version", "so we can reproduce on the same dashboard release build", false},
		{"Commit", "so we can inspect the exact code revision", false},
		{"Release channel", "so we know which update stream you are running", false},
		{"ACMM level", "so we can match the dashboard policy mode", false},
		{"Hive ID", "so hub operators can correlate this report with this hive only", false},
		{"Hosted flag", "so we know whether this is a hosted or self-managed dashboard", false},
		{"Hub-linked flag", "so we can follow the same relay path", false},
		{"Agent count", "so we can spot empty or unexpectedly large agent sets", false},
		{"Agent names, backend, model, and state", "so we can reproduce the affected agent configuration", false},
		{"Whether project org/repos were included", "so the report shows if the optional project context was shared", false},
		{"Browser user-agent", "so we can reproduce browser-specific rendering bugs", false},
		{"Browser platform", "so we know the operating system/browser platform family", false},
		{"Browser language", "so we can reproduce locale-sensitive formatting", false},
		{"Screen size", "so we can reproduce layout issues at the same display size", false},
		{"Window size", "so we can reproduce the dashboard viewport", false},
		{"Dashboard section/path", "so we know where you opened the form", false},
		{"Project repo for each agent", "so we can identify the affected project when you opt in", true},
		{"Project org for each agent", "so we can identify the affected organization when you opt in", true},
		{"Recent browser console errors", "so we can see client-side failures that happened before submit", false},
		{"Recent failed /api calls", "so we can see backend requests that failed before submit", false},
	}
}

func writeFeedbackDiagnosticsIncluded(b *strings.Builder, req feedbackReportRequest) {
	b.WriteString("\n## Diagnostics included\n\n")
	b.WriteString("| Field | Included | Why |\n|---|---|---|\n")
	includeProjects := req.Diagnostics != nil && req.Diagnostics.IncludeProjectRepos
	for _, row := range feedbackDiagnosticDisclosureRows() {
		included := "Yes"
		if row.OptionalProject && !includeProjects {
			included = "No (optional project context not selected)"
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s |\n", row.Field, included, row.Why))
	}
}

func writeFeedbackDiagnostics(b *strings.Builder, d *feedbackDiagnostics, submitter feedbackSubmitterIdentity) {
	b.WriteString("| Field | Value |\n|---|---|\n")
	submitterText := feedbackSubmitterDiagnosticsText(submitter)
	if d == nil {
		rows := [][2]string{{"Submitted by", submitterText}}
		for _, r := range rows {
			b.WriteString(fmt.Sprintf("| %s | %s |\n", r[0], strings.ReplaceAll(r[1], "|", "\\|")))
		}
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
			if d.IncludeProjectRepos {
				b.WriteString(fmt.Sprintf("- %s: backend=%s model=%s state=%s org=%s repo=%s\n", a.Name, a.Backend, a.Model, a.State, a.Org, a.Repo))
			} else {
				b.WriteString(fmt.Sprintf("- %s: backend=%s model=%s state=%s\n", a.Name, a.Backend, a.Model, a.State))
			}
		}
	}
}

// feedbackPrefillURL builds the issues/new URL the browser opens so the user
// files the issue as themselves. The body is a short summary plus a link to
// the stored bundle; feedbackbundle caps it so the URL stays browser-safe.
func feedbackPrefillURL(req feedbackReportRequest, bundleURL string) string {
	base := feedbackPrefillBaseHive
	target := "Target: Hive"
	if req.TargetRepo == feedbackTargetDocs {
		base = feedbackPrefillBaseDocs
		target = "Target: Documentation"
	}
	footer := "Submitted from the Hive spoke dashboard feedback form.\n" + target
	if feedbackSubmitterVerified(req.Submitter) {
		footer += "\n/cc @" + req.Submitter.GitHubLogin
	}
	if n := len(req.Screenshots); n > 0 && bundleURL != "" {
		footer += fmt.Sprintf("\nScreenshots: %d, in the diagnostic bundle.", n)
	}
	return feedbackbundle.PrefillURL(feedbackbundle.Prefill{
		Base:        base,
		Title:       req.Title,
		OpenedBy:    feedbackPrefillOpenedByLine(req),
		Description: req.Description,
		Footer:      footer,
		BundleURL:   bundleURL,
		Labels:      feedbackLabels(req),
	})
}

// feedbackScreenshotExtensions maps the sniffed content type of a decoded
// screenshot to the file extension it is stored under. Only these types are
// accepted: the declared data-URI media type is never trusted on its own.
var feedbackScreenshotExtensions = map[string]string{
	"image/png":  "png",
	"image/jpeg": "jpg",
}

// decodeFeedbackDataURI decodes one screenshot and returns its bytes with the
// extension matching its sniffed content type. The bytes must actually be a
// PNG or JPEG image; any other content is rejected regardless of the media
// type the data URI claims.
func decodeFeedbackDataURI(dataURI string) ([]byte, string, error) {
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

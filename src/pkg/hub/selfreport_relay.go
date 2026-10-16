package hub

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

// Self-report relay (hivecommons/hive#11868). Spokes whose GitHub credential
// cannot write to the report repo (self-hosted PATs scoped to their own
// repos) POST their fleet self-report here; the hub files or joins the issue
// under its App identity. Auth is the spoke heartbeat bearer, exactly like
// /api/feedback/ingest. The hub performs the same fingerprint dedupe the
// spoke would: one search for the marker, comment if found, create if not.

const (
	selfReportIngestPath      = "/api/selfreport/ingest"
	selfReportMaxRequestBytes = 256 << 10
	selfReportDefaultRepo     = "hivecommons/hive"
	selfReportRelayLabel      = "hive/selfreport"
)

// selfReportRate reuses the feedback limiter shape (per-hive, 24h window,
// feedbackHubMaxPerHivePerWindow creates) as its own instance so relayed
// self-reports never eat the feedback budget or vice versa.
var selfReportRate feedbackRateLimiter

// selfReportRelayRequest mirrors cmd/hive's wire struct; Evidence is kept
// opaque so pkg/hub does not import pkg/fleetreport.
type selfReportRelayRequest struct {
	HiveID      string          `json:"hive_id"`
	Fingerprint string          `json:"fingerprint"`
	Class       string          `json:"class,omitempty"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	Labels      []string        `json:"labels,omitempty"`
	Version     string          `json:"version,omitempty"`
	Repo        string          `json:"repo,omitempty"`
	Evidence    json.RawMessage `json:"evidence,omitempty"`
}

type selfReportRelayResponse struct {
	OK     bool   `json:"ok"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Joined bool   `json:"joined"`
}

func validateSelfReportRelayRequest(req *selfReportRelayRequest) error {
	req.HiveID = sanitizeHeartbeatField(req.HiveID)
	req.Fingerprint = strings.TrimSpace(req.Fingerprint)
	req.Title = strings.TrimSpace(req.Title)
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Fingerprint == "" || len(req.Fingerprint) > 128 || strings.ContainsAny(req.Fingerprint, " \t\n\"'<>") {
		return fmt.Errorf("invalid fingerprint")
	}
	if req.Title == "" || len(req.Title) > 256 {
		return fmt.Errorf("title must be 1-256 characters")
	}
	if strings.TrimSpace(req.Body) == "" || len(req.Body) > selfReportMaxRequestBytes {
		return fmt.Errorf("body is required")
	}
	if req.Repo == "" {
		req.Repo = selfReportDefaultRepo
	}
	if _, _, err := splitHubRepo(req.Repo); err != nil {
		return err
	}
	labels := req.Labels[:0]
	for _, l := range req.Labels {
		l = strings.TrimSpace(l)
		if l == "" || len(l) > 50 {
			continue
		}
		labels = append(labels, l)
	}
	req.Labels = labels
	return nil
}

func splitHubRepo(full string) (string, string, error) {
	owner, repo, ok := strings.Cut(full, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || !isValidName(owner) || !isValidName(repo) {
		return "", "", fmt.Errorf("invalid repo %q", full)
	}
	return owner, repo, nil
}

func (s *HubServer) handleSelfReportIngest(w http.ResponseWriter, r *http.Request) {
	if s.hubSecret == "" {
		npsJSONError(w, "self-report ingest requires a configured hub secret", http.StatusServiceUnavailable)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	presented := strings.TrimPrefix(auth, "Bearer ")
	body, err := io.ReadAll(io.LimitReader(r.Body, selfReportMaxRequestBytes+1))
	if err != nil {
		npsJSONError(w, "read error", http.StatusBadRequest)
		return
	}
	if len(body) > selfReportMaxRequestBytes {
		npsJSONError(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req selfReportRelayRequest
	if err := json.Unmarshal(body, &req); err != nil {
		npsJSONError(w, "invalid payload", http.StatusBadRequest)
		return
	}
	if err := validateSelfReportRelayRequest(&req); err != nil {
		npsJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.HiveID == "" || !isValidName(req.HiveID) {
		npsJSONError(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	if !s.verifyHeartbeatBearer(presented, req.HiveID) {
		npsJSONError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.npsHiveRegistered(req.HiveID) {
		npsJSONError(w, "unknown hive - heartbeat first", http.StatusForbidden)
		return
	}
	token, botLogin := mintHubFeedbackAppToken(r.Context(), s, req.HiveID)
	if token == "" {
		npsJSONError(w, "hub GitHub App token is not configured", http.StatusServiceUnavailable)
		return
	}
	_ = botLogin
	client := hubFeedbackAuthoringHTTPClient()
	owner, repo, _ := splitHubRepo(req.Repo)

	existing, err := findSelfReportIssueByFingerprint(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, req.Fingerprint)
	if err != nil && s.logger != nil {
		s.logger.Warn("selfreport relay: fingerprint search failed; will create", "hive", req.HiveID, "error", err)
	}
	if existing.Number > 0 {
		comment := fmt.Sprintf("<!-- hive:selfreport-seen hive=%s fp=%s --> Also seen on **%s** (v%s) at %s — relayed via hub.",
			req.HiveID, req.Fingerprint, req.HiveID, orUnknown(req.Version), time.Now().UTC().Format(time.RFC3339))
		if _, err := postHubGitHubIssueComment(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, existing.Number, comment); err != nil {
			if s.logger != nil {
				s.logger.Error("selfreport relay: comment failed", "hive", req.HiveID, "issue", existing.Number, "error", err)
			}
			npsJSONError(w, "failed to update issue", http.StatusBadGateway)
			return
		}
		// hive/selfreport only: the hive/<id> provenance label is retired
		// (hivecommons/hive#12539); the seen marker above names the hive.
		_ = postHubGitHubIssueLabels(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, existing.Number, []string{selfReportRelayLabel})
		if s.logger != nil {
			s.logger.Info("selfreport relay: joined existing issue", "hive", req.HiveID, "issue", existing.Number, "class", req.Class)
		}
		writeSelfReportRelayResponse(w, http.StatusOK, selfReportRelayResponse{OK: true, Number: existing.Number, URL: existing.URL, Joined: true})
		return
	}

	release, err := selfReportRate.reserve(req.HiveID, time.Now())
	if err != nil {
		npsJSONError(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	labels := append([]string(nil), req.Labels...)
	if !containsString(labels, selfReportRelayLabel) {
		labels = append(labels, selfReportRelayLabel)
	}
	res, status, err := postHubGitHubIssue(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, req.Title, req.Body, labels)
	if err != nil && status == http.StatusUnprocessableEntity {
		// Unknown labels: retry without them rather than lose the report.
		res, _, err = postHubGitHubIssue(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, req.Title, req.Body, nil)
	}
	if err != nil {
		release()
		if s.logger != nil {
			s.logger.Error("selfreport relay: create failed", "hive", req.HiveID, "error", err)
		}
		npsJSONError(w, "failed to create issue", http.StatusBadGateway)
		return
	}
	if s.logger != nil {
		s.logger.Info("selfreport relay: created issue", "hive", req.HiveID, "issue", res.Number, "class", req.Class)
	}
	writeSelfReportRelayResponse(w, http.StatusCreated, selfReportRelayResponse{OK: true, Number: res.Number, URL: res.URL})
}

func writeSelfReportRelayResponse(w http.ResponseWriter, status int, resp selfReportRelayResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// findSelfReportIssueByFingerprint searches open issues for the fingerprint
// marker. Returns zero Number when nothing matches.
func findSelfReportIssueByFingerprint(ctx context.Context, client *http.Client, apiBase, token, owner, repo, fingerprint string) (feedbackIssueResult, error) {
	ctx = hubGitHubCallerContext(ctx, "selfreport_search")
	q := fmt.Sprintf("repo:%s/%s is:issue is:open in:body %q", owner, repo, "hive-fleet-fingerprint:"+fingerprint)
	u := strings.TrimRight(apiBase, "/") + "/search/issues?per_page=5&q=" + url.QueryEscape(q)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return feedbackIssueResult{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return feedbackIssueResult{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return feedbackIssueResult{}, fmt.Errorf("github search: %d", resp.StatusCode)
	}
	var out struct {
		Items []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return feedbackIssueResult{}, err
	}
	for _, it := range out.Items {
		if strings.Contains(it.Body, "hive-fleet-fingerprint:"+fingerprint) {
			return feedbackIssueResult{Number: it.Number, URL: it.HTMLURL}, nil
		}
	}
	return feedbackIssueResult{}, nil
}

func postHubGitHubIssueComment(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, number int, body string) (int, error) {
	ctx = hubGitHubCallerContext(ctx, "selfreport_comment")
	data, _ := json.Marshal(map[string]string{"body": body})
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", strings.TrimRight(apiBase, "/"), owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("github create comment: %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func postHubGitHubIssueLabels(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, number int, labels []string) error {
	ctx = hubGitHubCallerContext(ctx, "selfreport_labels")
	data, _ := json.Marshal(map[string][]string{"labels": labels})
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/labels", strings.TrimRight(apiBase, "/"), owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github add labels: %d", resp.StatusCode)
	}
	return nil
}

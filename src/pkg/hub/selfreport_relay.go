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

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/fleetreport"
)

// Self-report relay (hivecommons/hive#11868). Spokes whose GitHub credential
// cannot write to the report repo (self-hosted PATs scoped to their own
// repos) POST their fleet self-report here; the hub files or joins the issue
// under its App identity. Auth is the spoke heartbeat bearer, exactly like
// /api/feedback/ingest. The hub performs the same fingerprint dedupe the
// spoke would: one search for the marker, comment if found, create if not.

const (
	selfReportIngestPath       = "/api/selfreport/ingest"
	selfReportMaxRequestBytes  = 256 << 10
	selfReportDefaultRepo      = "hivecommons/hive"
	selfReportRelayLabel       = "hive/selfreport"
	selfReportRecoverAction    = "recover"
	selfReportObservationClass = "observation"
	selfReportObservationLabel = "hive-observation"
)

// selfReportRate reuses the feedback limiter shape (per-hive, 24h window,
// feedbackHubMaxPerHivePerWindow creates) as its own instance so relayed
// self-reports never eat the feedback budget or vice versa.
var selfReportRate feedbackRateLimiter

// selfReportRelayRequest mirrors cmd/hive's wire struct; Evidence is kept
// opaque. Owners / OwnersHidden / InstanceID / Repos feed the affected block
// the hub edits in place when it joins or files the issue.
type selfReportRelayRequest struct {
	Action      string   `json:"action,omitempty"`
	HiveID      string   `json:"hive_id"`
	Fingerprint string   `json:"fingerprint"`
	Class       string   `json:"class,omitempty"`
	Title       string   `json:"title,omitempty"`
	Body        string   `json:"body"`
	Labels      []string `json:"labels,omitempty"`
	Version     string   `json:"version,omitempty"`
	Repo        string   `json:"repo,omitempty"`
	InstanceID  string   `json:"instance_id,omitempty"`
	Repos       []string `json:"repos,omitempty"`
	Owners      []string `json:"owners,omitempty"`
	// OwnersHidden is the spoke's include_owners opt-out; the hub then never
	// substitutes its own owner record.
	OwnersHidden bool            `json:"owners_hidden,omitempty"`
	Evidence     json.RawMessage `json:"evidence,omitempty"`
	Observation  json.RawMessage `json:"observation,omitempty"`
	HiveState    json.RawMessage `json:"hive_state,omitempty"`
}

type selfReportRelayResponse struct {
	OK     bool   `json:"ok"`
	Number int    `json:"number,omitempty"`
	URL    string `json:"url,omitempty"`
	Joined bool   `json:"joined,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func validateSelfReportRelayRequest(req *selfReportRelayRequest) error {
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	req.HiveID = sanitizeHeartbeatField(req.HiveID)
	req.Fingerprint = strings.TrimSpace(req.Fingerprint)
	req.Class = strings.ToLower(strings.TrimSpace(req.Class))
	req.Title = strings.TrimSpace(req.Title)
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Fingerprint == "" || len(req.Fingerprint) > 128 || strings.ContainsAny(req.Fingerprint, " \t\n\"'<>") {
		return fmt.Errorf("invalid fingerprint")
	}
	switch req.Action {
	case "":
		if req.Title == "" || len(req.Title) > 256 {
			return fmt.Errorf("title must be 1-256 characters")
		}
	case selfReportRecoverAction:
		if req.Class != selfReportObservationClass {
			return fmt.Errorf("recover action requires class observation")
		}
	default:
		return fmt.Errorf("unsupported self-report action")
	}
	if strings.TrimSpace(req.Body) == "" || len(req.Body) > selfReportMaxRequestBytes {
		return fmt.Errorf("body is required")
	}
	if req.Action == selfReportRecoverAction || req.Repo == "" {
		req.Repo = selfReportDefaultRepo
	}
	// Observation issues are always hub-owned. Do not trust a spoke-supplied
	// project repo, even if an older spoke sends one by mistake.
	if req.Class == selfReportObservationClass {
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
	req.InstanceID = strings.TrimSpace(req.InstanceID)
	req.Repos = fleetreport.NormalizeRepos(req.Repos)
	req.Owners = fleetreport.NormalizeLogins(req.Owners)
	if req.OwnersHidden {
		req.Owners = nil
	}
	return nil
}

// selfReportAffectedEntry is the affected-block line the hub writes for the
// relaying hive. Owners are the spoke's, falling back to the hub's own
// record of the hive owner for spokes that predate the owners field, unless
// the spoke opted out.
func selfReportAffectedEntry(req selfReportRelayRequest, now time.Time) fleetreport.AffectedHive {
	owners := req.Owners
	if len(owners) == 0 && !req.OwnersHidden {
		if hive := loadSaaSHive(req.HiveID); hive != nil {
			owners = fleetreport.NormalizeLogins([]string{hive.Owner})
		}
	}
	return fleetreport.AffectedHive{Hive: req.HiveID, Instance: req.InstanceID, Owners: owners, OwnersHidden: req.OwnersHidden, Last: now}
}

func splitHubRepo(full string) (string, string, error) {
	owner, repo, ok := strings.Cut(full, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") || !isValidName(owner) || !isValidName(repo) {
		return "", "", fmt.Errorf("invalid repo %q", full)
	}
	return owner, repo, nil
}

func selfReportNeedsDecisionLabel(hiveID string) string {
	if hive := loadSaaSHive(hiveID); hive != nil && hive.IssueFilter != nil {
		return strings.TrimSpace(hive.IssueFilter.NeedsDecisionLabel())
	}
	return config.DefaultNeedsDecisionLabel
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
	if req.Action == selfReportRecoverAction {
		existing, err := findSelfReportIssueByFingerprint(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, req.Fingerprint)
		if err != nil {
			if s.logger != nil {
				s.logger.Error("selfreport relay: recovery search failed", "hive", req.HiveID, "fingerprint", req.Fingerprint, "error", err)
			}
			npsJSONError(w, "failed to find observation issue", http.StatusBadGateway)
			return
		}
		if existing.Number <= 0 {
			// A closed or never-created issue is an idempotent no-op. The
			// spoke must not turn this into an eval-cycle failure.
			writeSelfReportRelayResponse(w, http.StatusOK, selfReportRelayResponse{Reason: "unknown_fingerprint"})
			return
		}
		// Every hive-filed fleet issue (runtime errors, ACMM criteria,
		// defects) carries the same fingerprint marker, and those
		// fingerprints are deterministic and fleet-wide. Only an issue the
		// hub itself filed as an observation may be closed by a spoke's
		// recovery; the request's class is the caller's claim, the label is
		// the issue's.
		if !containsString(existing.Labels, selfReportObservationLabel) {
			if s.logger != nil {
				s.logger.Warn("selfreport relay: recovery target is not an observation issue; ignoring", "hive", req.HiveID, "fingerprint", req.Fingerprint, "issue", existing.Number)
			}
			writeSelfReportRelayResponse(w, http.StatusOK, selfReportRelayResponse{Reason: "not_observation"})
			return
		}
		// Fingerprints may be supplied by detectors, so neither the request
		// nor a fingerprint prefix proves ownership. Bind recovery to the
		// hive identity recorded in the matched observation's evidence.
		if !selfReportObservationBelongsToHive(existing.Body, req.HiveID) {
			writeSelfReportRelayResponse(w, http.StatusOK, selfReportRelayResponse{Reason: "hive_mismatch"})
			return
		}
		if _, err := postHubGitHubIssueComment(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, existing.Number, req.Body); err != nil {
			if s.logger != nil {
				s.logger.Error("selfreport relay: recovery comment failed", "hive", req.HiveID, "issue", existing.Number, "error", err)
			}
			npsJSONError(w, "failed to comment on observation issue", http.StatusBadGateway)
			return
		}
		if err := closeHubGitHubIssue(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, existing.Number); err != nil {
			if s.logger != nil {
				s.logger.Error("selfreport relay: recovery close failed", "hive", req.HiveID, "issue", existing.Number, "error", err)
			}
			npsJSONError(w, "failed to close observation issue", http.StatusBadGateway)
			return
		}
		writeSelfReportRelayResponse(w, http.StatusOK, selfReportRelayResponse{OK: true, Number: existing.Number, URL: existing.URL})
		return
	}

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
		// Only hive-filed runtime-error issues carry the block; the hub never
		// appends one to an observation or ACMM report.
		if strings.Contains(existing.Body, fleetreport.AffectedBlockStart) {
			if newBody, changed := fleetreport.UpdateAffected(existing.Body, selfReportAffectedEntry(req, time.Now()), req.Repos, true); changed {
				if err := patchHubGitHubIssueBody(r.Context(), client, feedbackGitHubAPIBase, token, owner, repo, existing.Number, newBody); err != nil && s.logger != nil {
					s.logger.Warn("selfreport relay: could not update affected-hives block", "hive", req.HiveID, "issue", existing.Number, "error", err)
				}
			}
		}
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
	if req.Class == selfReportObservationClass {
		if !containsString(labels, selfReportObservationLabel) {
			labels = append(labels, selfReportObservationLabel)
		}
		if decision := selfReportNeedsDecisionLabel(req.HiveID); decision != "" && !containsString(labels, decision) {
			labels = append(labels, decision)
		}
	}
	if !containsString(labels, selfReportRelayLabel) {
		labels = append(labels, selfReportRelayLabel)
	}
	if strings.Contains(req.Body, fleetreport.AffectedBlockStart) {
		// A spoke that predates the owners field sends a block with only the
		// hive name; fill the hive's line from what the hub knows.
		if newBody, changed := fleetreport.UpdateAffected(req.Body, selfReportAffectedEntry(req, time.Now()), req.Repos, false); changed {
			req.Body = newBody
		}
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

// selfReportObservationBelongsToHive reads the evidence field emitted by
// fleetreport.BuildObservationReport. Missing, malformed or duplicate ownership
// fields fail closed, including when the fingerprint looks hive-scoped.
func selfReportObservationBelongsToHive(body, hiveID string) bool {
	found := false
	for _, line := range strings.Split(body, "\n") {
		cells := strings.Split(strings.TrimSpace(line), "|")
		if len(cells) < 2 || strings.TrimSpace(cells[1]) != "hive id" {
			continue
		}
		if found || len(cells) != 4 || strings.TrimSpace(cells[0]) != "" || strings.TrimSpace(cells[3]) != "" || strings.TrimSpace(cells[2]) != hiveID {
			return false
		}
		found = true
	}
	return found
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
			Labels  []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return feedbackIssueResult{}, err
	}
	for _, it := range out.Items {
		if strings.Contains(it.Body, "hive-fleet-fingerprint:"+fingerprint) {
			labels := make([]string, 0, len(it.Labels))
			for _, l := range it.Labels {
				labels = append(labels, l.Name)
			}
			return feedbackIssueResult{Number: it.Number, URL: it.HTMLURL, Body: it.Body, Labels: labels}, nil
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

func patchHubGitHubIssueBody(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, number int, body string) error {
	ctx = hubGitHubCallerContext(ctx, "selfreport_affected")
	data, _ := json.Marshal(map[string]string{"body": body})
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", strings.TrimRight(apiBase, "/"), owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(data))
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
		return fmt.Errorf("github edit issue: %d", resp.StatusCode)
	}
	return nil
}

func closeHubGitHubIssue(ctx context.Context, client *http.Client, apiBase, token, owner, repo string, number int) error {
	ctx = hubGitHubCallerContext(ctx, "selfreport_recover")
	data, _ := json.Marshal(map[string]string{"state": "closed", "state_reason": "completed"})
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", strings.TrimRight(apiBase, "/"), owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(data))
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
		return fmt.Errorf("github close issue: %d", resp.StatusCode)
	}
	return nil
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

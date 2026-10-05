package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

func TestFeedbackRedactsAndBuildsFallbackURL(t *testing.T) {
	req := feedbackReportRequest{
		Title:              "Bug with token ghp_secret123",
		Description:        "bearer abc.def user@example.com password=opensesame",
		RequestType:        feedbackTypeBug,
		TargetRepo:         feedbackTargetHive,
		IncludeDiagnostics: true,
		Diagnostics:        &feedbackDiagnostics{HiveID: "hive-one", Page: "/?token=secret", Agents: []feedbackAgentDiagnostic{{Name: "scanner", Repo: "private/repo"}}},
	}

	if err := validateFeedbackRequest(&req); err != nil {
		t.Fatal(err)
	}
	sanitizeFeedbackRequest(&req)
	body := buildFeedbackIssueBody(req)
	if strings.Contains(body, "ghp_secret") || strings.Contains(body, "abc.def") || strings.Contains(body, "user@example.com") || strings.Contains(body, "opensesame") || strings.Contains(body, "private/repo") {
		t.Fatalf("feedback body leaked sensitive data:\n%s", body)
	}
	if !strings.Contains(body, "## Diagnostics included") || !strings.Contains(body, "Recent failed /api calls") {
		t.Fatalf("feedback body missing diagnostics disclosure:\n%s", body)
	}
	if !strings.Contains(feedbackFallbackURL(req), "github.com/hivecommons/hive/issues/new") {
		t.Fatalf("fallback did not target hive repo")
	}
}

func TestFeedbackIssueBodyAttributionCases(t *testing.T) {
	tests := []struct {
		name      string
		submitter feedbackSubmitterIdentity
		want      []string
		notWant   []string
	}{
		{
			name:      "github oauth login",
			submitter: feedbackSubmitterIdentity{Name: "alice", GitHubLogin: "alice", Source: "GitHub OAuth dashboard login"},
			want: []string{
				"| Submitted by | @alice (GitHub OAuth dashboard login) |",
				"Opened by the hive on behalf of @alice.",
				"/cc @alice",
			},
		},
		{
			name:      "authenticated dashboard user",
			submitter: feedbackSubmitterIdentity{Name: "basic-user", Source: "authenticated dashboard user"},
			want: []string{
				"| Submitted by | basic-user (authenticated dashboard user) |",
				"Opened by the hive on behalf of basic-user.",
			},
			notWant: []string{"/cc @basic-user"},
		},
		{
			name:      "anonymous dashboard session",
			submitter: feedbackSubmitterIdentity{Name: "anonymous dashboard session", Source: "anonymous dashboard session"},
			want: []string{
				"| Submitted by | anonymous dashboard session |",
				"Opened by the hive on behalf of anonymous dashboard session.",
			},
			notWant: []string{"/cc @"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildFeedbackIssueBody(feedbackReportRequest{
				Title:        "Bug from dashboard",
				Description:  "Something went wrong",
				RequestType:  feedbackTypeBug,
				TargetRepo:   feedbackTargetHive,
				Submitter:    tt.submitter,
				OpenedByHive: true,
				Diagnostics:  &feedbackDiagnostics{HiveID: "hive-one", Channel: "edge"},
			})
			for _, want := range tt.want {
				if !strings.Contains(body, want) {
					t.Fatalf("feedback body missing %q:\n%s", want, body)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(body, notWant) {
					t.Fatalf("feedback body unexpectedly contained %q:\n%s", notWant, body)
				}
			}
			if !strings.Contains(body, "| Hive ID | hive-one |") || !strings.Contains(body, "| Channel | edge |") {
				t.Fatalf("feedback body missing hive diagnostics:\n%s", body)
			}
		})
	}
}

func TestFeedbackSubmitterIdentitySources(t *testing.T) {
	s := NewServer(0, dismissLogger())
	sessionID := s.createUserSession("octocat", "owner")
	oauthReq := httptest.NewRequest(http.MethodPost, "/api/feedback/report", nil)
	oauthReq.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionID})
	if got := s.feedbackSubmitterIdentity(oauthReq); got.Name != "octocat" || got.GitHubLogin != "octocat" || got.Source != "GitHub OAuth dashboard login" {
		t.Fatalf("oauth identity = %+v", got)
	}

	headerReq := httptest.NewRequest(http.MethodPost, "/api/feedback/report", nil)
	headerReq.Header.Set("X-Hive-User", "basic-user")
	if got := s.feedbackSubmitterIdentity(headerReq); got.Name != "basic-user" || got.GitHubLogin != "" || got.Source != "authenticated dashboard user" {
		t.Fatalf("header identity = %+v", got)
	}

	anonReq := httptest.NewRequest(http.MethodPost, "/api/feedback/report", nil)
	if got := s.feedbackSubmitterIdentity(anonReq); got.Name != "anonymous dashboard session" || got.GitHubLogin != "" {
		t.Fatalf("anonymous identity = %+v", got)
	}
}

func TestFeedbackReportStandaloneReturnsFallback(t *testing.T) {
	s, _ := apiServer(t)
	body := `{"title":"A useful bug report","description":"Something went wrong in the dashboard","request_type":"bug","target_repo":"docs"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/report", strings.NewReader(body))
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out feedbackReportResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !strings.Contains(out.FallbackURL, "github.com/hivecommons/docs/issues/new") {
		t.Fatalf("unexpected response: %+v", out)
	}
}

func TestFeedbackHubRelayCarriesHiveIDEvenWithoutDiagnostics(t *testing.T) {
	t.Setenv(spoke.EnvHeartbeatKey, "test-heartbeat")
	var got feedbackReportRequest
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != feedbackHubIngestPath {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-heartbeat" {
			t.Fatalf("missing heartbeat bearer")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(feedbackReportResponse{OK: true, IssueNumber: 7, IssueURL: "https://github.com/hivecommons/hive/issues/7"})
	}))
	defer hub.Close()
	s := NewServer(0, dismissLogger())
	s.deps = &Dependencies{Config: &config.Config{HiveID: "hive-one", Hub: config.HubConfig{Enabled: true, URL: hub.URL, HiveType: "hosted"}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/feedback/report", strings.NewReader(`{"title":"A useful bug report","description":"Something went wrong in the dashboard","request_type":"bug","include_diagnostics":false}`))
	markOwnerRequest(req)
	req.Header.Set("X-Hive-User", "dashboard-user")
	s.handleFeedbackReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got.HiveID != "hive-one" {
		t.Fatalf("relayed hive_id = %q", got.HiveID)
	}
	if got.Diagnostics != nil {
		t.Fatalf("diagnostics should remain excluded, got %+v", got.Diagnostics)
	}
	if got.Submitter.Name != "dashboard-user" || got.Submitter.Source != "authenticated dashboard user" {
		t.Fatalf("submitter = %+v", got.Submitter)
	}
	if !got.OpenedByHive {
		t.Fatal("hub-relayed feedback should be marked as opened by hive")
	}
}

func TestFeedbackStaticUIWiring(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	for _, want := range []string{"data-action=\"openFeedbackModal\"", "id=\"feedback-bug-btn\"", "id=\"feedback-unread-pill\"", "installFeedbackCapture();", "FEEDBACK_DRAFT_KEY", "feedbackRedact", "/api/feedback/report"} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	for _, removed := range []string{`class="hv-btn btn-primary feedback-fab"`, ".feedback-fab {", "💬 Feedback"} {
		if strings.Contains(html, removed) {
			t.Errorf("index.html still contains removed floating feedback button marker %q", removed)
		}
	}
	if strings.Contains(html, `href="https://github.com/hivecommons/hive/issues"`) {
		t.Fatal("sidebar Report an Issue still links externally instead of opening feedback modal")
	}
	if !strings.Contains(html, `data-action="openFeedbackModal" data-arg0="bug"`) {
		t.Fatal("sidebar Report an Issue does not open the feedback modal on the bug tab")
	}
	open := jsFunctionBody(t, html, "function openFeedbackModal(tab)")
	if strings.Contains(open, "window.prompt") || strings.Contains(open, "alert(") || strings.Contains(open, "confirm(") {
		t.Fatal("feedback modal uses a native browser dialog")
	}
	for _, want := range []string{
		".feedback-modal { max-width: 720px",
		"max-height: calc(100vh - var(--modal-gap) - var(--modal-gap))",
		".feedback-body { flex:1 1 auto; min-height:0; overflow-y:auto; padding:var(--sp-8);",
		".feedback-footer { position: sticky; bottom:0;",
		"class=\"feedback-footer\"",
		"class=\"feedback-body\"",
		"class=\"feedback-diagnostics-fieldset\"",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("feedback modal layout missing %q", want)
		}
	}
	if strings.Contains(open, "<details open") {
		t.Fatal("feedback preview should be collapsed by default")
	}
	if !strings.Contains(open, `<details class="feedback-preview-shell">`) {
		t.Fatal("feedback preview details element missing")
	}
	if !strings.Contains(open, "overlay.addEventListener('click', function(e) { if (e.target === overlay) closeFeedbackModal(); });") {
		t.Fatal("feedback modal does not close on backdrop click")
	}
	escapeHandler := regexp.MustCompile(`(?s)function hiveDismissTopOverlay\(\) \{.*?document\.getElementById\('feedback-overlay'\).*?closeFeedbackModal`).FindString(html)
	if escapeHandler == "" {
		t.Fatal("global Escape dismissal path is not wired to closeFeedbackModal")
	}
	submit := jsFunctionBody(t, html, "async function submitFeedbackReport()")
	if !strings.Contains(submit, "fetch('/api/feedback/report'") {
		t.Fatal("feedback submit does not post to the feedback endpoint")
	}
	unread := jsFunctionBody(t, html, "function feedbackUnreadCount(items)")
	if !strings.Contains(unread, "updated_at > item.last_seen_updated_at") {
		t.Fatal("feedback unread pill is not derived from updated_at > last_seen_updated_at")
	}
}

func TestFeedbackDiagnosticDisclosureMatchesPayload(t *testing.T) {
	b, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(b)
	constBlockStart := strings.Index(html, "const FEEDBACK_DIAGNOSTIC_FIELDS = [")
	if constBlockStart < 0 {
		t.Fatal("missing FEEDBACK_DIAGNOSTIC_FIELDS")
	}
	constBlockEnd := strings.Index(html[constBlockStart:], "    ];")
	if constBlockEnd < 0 {
		t.Fatal("unterminated FEEDBACK_DIAGNOSTIC_FIELDS")
	}
	constBlock := html[constBlockStart : constBlockStart+constBlockEnd]
	keyRe := regexp.MustCompile(`key: '([^']+)'([^}]+)}`)
	var disclosedPayloadKeys []string
	for _, m := range keyRe.FindAllStringSubmatch(constBlock, -1) {
		if strings.Contains(m[2], "optionalProject") || strings.Contains(m[2], "outsideDiagnostics") {
			continue
		}
		disclosedPayloadKeys = append(disclosedPayloadKeys, m[1])
	}
	collect := jsFunctionBody(t, html, "async function collectFeedbackDiagnostics()")
	call := regexp.MustCompile(`feedbackDiagnosticPayloadFromValues\(\{([^;]+)\}\);`).FindStringSubmatch(collect)
	if call == nil {
		t.Fatal("collectFeedbackDiagnostics does not derive its payload from feedbackDiagnosticPayloadFromValues")
	}
	payloadKeyRe := regexp.MustCompile(`([a-z_]+):`)
	var collectedKeys []string
	for _, m := range payloadKeyRe.FindAllStringSubmatch(call[1], -1) {
		collectedKeys = append(collectedKeys, m[1])
	}
	if strings.Join(disclosedPayloadKeys, ",") != strings.Join(collectedKeys, ",") {
		t.Fatalf("disclosure keys %v != diagnostics payload keys %v", disclosedPayloadKeys, collectedKeys)
	}
	for _, want := range []string{"agents[].repo", "agents[].org", "console_errors", "failed_api_calls"} {
		if !strings.Contains(constBlock, "key: '"+want+"'") {
			t.Fatalf("diagnostics disclosure missing %s", want)
		}
	}
	if !strings.Contains(html, "function renderFeedbackDiagnosticsDisclosure()") || !strings.Contains(html, "FEEDBACK_DIAGNOSTIC_FIELDS.map") {
		t.Fatal("diagnostics disclosure is not rendered from FEEDBACK_DIAGNOSTIC_FIELDS")
	}
}

func TestFeedbackMineRequiresAuthAndRefreshesUserToken(t *testing.T) {
	oldStore := feedbackSubmissionsPath
	oldToken := userTokenPath
	oldBase := feedbackGitHubAPIBase
	t.Cleanup(func() {
		feedbackSubmissionsPath = oldStore
		userTokenPath = oldToken
		feedbackGitHubAPIBase = oldBase
	})
	dir := t.TempDir()
	feedbackSubmissionsPath = filepath.Join(dir, "feedback-submissions.json")
	userTokenPath = filepath.Join(dir, "gh-user-token")
	if err := os.WriteFile(userTokenPath, []byte("ghu_test_token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveFeedbackSubmissions([]feedbackSubmissionRecord{{
		Owner:             "hivecommons",
		Repo:              "hive",
		Number:            42,
		Title:             "Old title",
		State:             "open",
		HTMLURL:           "https://github.com/hivecommons/hive/issues/42",
		SubmittedAt:       "2026-10-01T00:00:00Z",
		UpdatedAt:         "2026-10-01T00:00:00Z",
		LastSeenUpdatedAt: "2026-10-01T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/hivecommons/hive/issues/42" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer ghu_test_token" {
			t.Fatalf("missing user token auth")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title":      "Updated feedback",
			"state":      "open",
			"html_url":   "https://github.com/hivecommons/hive/issues/42",
			"updated_at": "2026-10-02T00:00:00Z",
			"comments":   3,
		})
	}))
	defer gh.Close()
	feedbackGitHubAPIBase = func() string { return gh.URL }
	s := NewServer(0, dismissLogger())
	s.deps = &Dependencies{Config: &config.Config{}}

	unauth := httptest.NewRecorder()
	s.handleFeedbackMine(unauth, httptest.NewRequest(http.MethodGet, "/api/feedback/mine", nil))
	if unauth.Code != http.StatusForbidden {
		t.Fatalf("unauth status = %d", unauth.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/feedback/mine", nil)
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.handleFeedbackMine(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got feedbackMineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Unread != 1 || len(got.Items) != 1 || got.Items[0].Title != "Updated feedback" || got.Items[0].Comments != 3 {
		t.Fatalf("unexpected mine response: %+v", got)
	}

	reqSeen := httptest.NewRequest(http.MethodGet, "/api/feedback/mine?mark_seen=true", nil)
	markOwnerRequest(reqSeen)
	recSeen := httptest.NewRecorder()
	s.handleFeedbackMine(recSeen, reqSeen)
	if recSeen.Code != http.StatusOK {
		t.Fatalf("seen status = %d body=%s", recSeen.Code, recSeen.Body.String())
	}
	reloaded, err := loadFeedbackSubmissions()
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded) != 1 || reloaded[0].LastSeenUpdatedAt != "2026-10-02T00:00:00Z" {
		t.Fatalf("last seen not updated: %+v", reloaded)
	}
}

func TestFeedbackUnreadCount(t *testing.T) {
	got := feedbackUnreadCount([]feedbackSubmissionRecord{
		{UpdatedAt: "2026-10-02T00:00:00Z", LastSeenUpdatedAt: "2026-10-01T00:00:00Z"},
		{UpdatedAt: "2026-10-01T00:00:00Z", LastSeenUpdatedAt: "2026-10-01T00:00:00Z"},
		{UpdatedAt: "", LastSeenUpdatedAt: "2026-10-01T00:00:00Z"},
	})
	if got != 1 {
		t.Fatalf("unread = %s, want 1", strconv.Itoa(got))
	}
}

func TestFeedbackScreenshotsMustBeRealImages(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 24)...)
	if _, ext, err := decodeFeedbackDataURI("data:image/png;base64," + base64.StdEncoding.EncodeToString(png)); err != nil || ext != "png" {
		t.Fatalf("real png rejected: ext=%q err=%v", ext, err)
	}
	for name, payload := range map[string][]byte{
		"shell": []byte("#!/bin/sh\necho hello\n"),
		"html":  []byte("<html><script>alert(1)</script></html>"),
		"svg":   []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`),
	} {
		uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)
		if _, _, err := decodeFeedbackDataURI(uri); err == nil {
			t.Fatalf("%s payload accepted as image", name)
		}
		req := &feedbackReportRequest{Title: "A useful bug report", Description: "Something went wrong", RequestType: feedbackTypeBug, Screenshots: []string{uri}}
		if err := validateFeedbackRequest(req); err == nil {
			t.Fatalf("%s payload passed request validation", name)
		}
	}
}

func TestFeedbackScreenshotsCommitToDedicatedBranch(t *testing.T) {
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 24)...)
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	var branches, paths []string
	refCreated := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/repos/hivecommons/docs/contents/"):
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			branches = append(branches, payload["branch"])
			paths = append(paths, strings.TrimPrefix(r.URL.Path, "/repos/hivecommons/docs/contents/"))
			if !refCreated {
				http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]string{"download_url": "https://example.invalid/x.png"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/docs":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "main"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/docs/git/ref/heads/main":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "abc123"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/docs/git/refs":
			refCreated = true
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	uploadFeedbackScreenshots(context.Background(), srv.Client(), srv.URL, "tok", "hivecommons", "docs", 3, []string{uri})

	if !refCreated || len(branches) != 2 {
		t.Fatalf("refCreated=%v branches=%v", refCreated, branches)
	}
	for i, b := range branches {
		if b != feedbackScreenshotBranch {
			t.Fatalf("put %d targeted branch %q", i, b)
		}
		if strings.HasPrefix(paths[i], ".github") || !strings.HasPrefix(paths[i], feedbackScreenshotDir+"/3/screenshot-1.png") {
			t.Fatalf("unexpected content path %s", paths[i])
		}
	}
}

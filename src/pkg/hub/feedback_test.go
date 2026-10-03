package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHubFeedbackCreateIssueRetriesWithoutLabels(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/hivecommons/hive/issues" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		calls++
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if calls == 1 {
			if _, ok := payload["labels"]; !ok {
				t.Fatal("first request did not include labels")
			}
			http.Error(w, "labels denied", http.StatusForbidden)
			return
		}
		if _, ok := payload["labels"]; ok {
			t.Fatal("retry still included labels")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "html_url": "https://github.com/hivecommons/hive/issues/42", "id": 420})
	}))
	defer srv.Close()
	req := feedbackReportRequest{Title: "Bug from dashboard", Description: "Something bad happened", RequestType: feedbackTypeBug, TargetRepo: feedbackTargetHive, IncludeDiagnostics: true, Diagnostics: &feedbackDiagnostics{HiveID: "hive-one"}}
	res, warning, err := createHubFeedbackIssue(context.Background(), srv.Client(), "tok", req, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if res.Number != 42 || !strings.Contains(warning, "without labels") || calls != 2 {
		t.Fatalf("res=%+v warning=%q calls=%d", res, warning, calls)
	}
}

func TestHubFeedbackRateLimiter(t *testing.T) {
	var l feedbackRateLimiter
	for i := 0; i < feedbackHubMaxPerHivePerWindow; i++ {
		if _, err := l.reserve("hive-one", time.Unix(int64(i), 0)); err != nil {
			t.Fatalf("reserve %d: %v", i, err)
		}
	}
	if _, err := l.reserve("hive-one", time.Unix(99, 0)); err != errFeedbackRateLimited {
		t.Fatalf("err=%v want rate limited", err)
	}
}

func TestHubFeedbackIssuesRequiresBearerAndReturnsState(t *testing.T) {
	var sawAuth string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/hivecommons/hive/issues/42" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title":      "Feedback bug",
			"state":      "open",
			"html_url":   "https://github.com/hivecommons/hive/issues/42",
			"updated_at": "2026-10-02T12:00:00Z",
			"comments":   4,
		})
	}))
	defer gh.Close()
	s := npsTestHub("h1")
	s.envGitHubToken = "hub-token"
	q := url.Values{}
	q.Set("hive_id", "h1")
	q.Set("refs", "hivecommons/hive#42")
	req := httptest.NewRequest(http.MethodGet, feedbackIssuesPath+"?"+q.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+s.heartbeatKeyFor("h1"))
	rec := httptest.NewRecorder()
	oldBase := feedbackGitHubAPIBase
	feedbackGitHubAPIBase = gh.URL
	t.Cleanup(func() { feedbackGitHubAPIBase = oldBase })
	s.handleFeedbackIssues(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if sawAuth != "Bearer hub-token" {
		t.Fatalf("GitHub auth = %q", sawAuth)
	}
	var out struct {
		Items []feedbackIssueStatus `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].Title != "Feedback bug" || out.Items[0].Comments != 4 {
		t.Fatalf("items = %+v", out.Items)
	}

	bad := httptest.NewRecorder()
	s.handleFeedbackIssues(bad, httptest.NewRequest(http.MethodGet, feedbackIssuesPath+"?"+q.Encode(), nil))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status = %d", bad.Code)
	}
}

// pngDataURI is a data URI whose decoded bytes carry the PNG magic header.
func pngDataURI(t *testing.T) string {
	t.Helper()
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 24)...)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
}

func TestHubFeedbackScreenshotsMustBeRealImages(t *testing.T) {
	if _, ext, err := decodeHubFeedbackDataURI(pngDataURI(t)); err != nil || ext != "png" {
		t.Fatalf("real png rejected: ext=%q err=%v", ext, err)
	}
	// The declared media type is not trusted: a "png" whose bytes are a shell
	// script, HTML, or anything else that is not an image is rejected.
	for name, payload := range map[string][]byte{
		"shell":  []byte("#!/bin/sh\necho hello\n"),
		"html":   []byte("<html><script>alert(1)</script></html>"),
		"binary": {0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07},
		"svg":    []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>1</script></svg>`),
	} {
		uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)
		if _, _, err := decodeHubFeedbackDataURI(uri); err == nil {
			t.Fatalf("%s payload accepted as image", name)
		}
		req := &feedbackReportRequest{Title: "t", Description: "d", RequestType: feedbackTypeBug, Screenshots: []string{uri}}
		if err := validateHubFeedbackRequest(req); err == nil {
			t.Fatalf("%s payload passed request validation", name)
		}
	}
	// The extension follows the sniffed type, not the declared one.
	jpeg := append([]byte{0xff, 0xd8, 0xff, 0xe0}, make([]byte, 24)...)
	if _, ext, err := decodeHubFeedbackDataURI("data:image/png;base64," + base64.StdEncoding.EncodeToString(jpeg)); err != nil || ext != "jpg" {
		t.Fatalf("jpeg bytes declared as png: ext=%q err=%v", ext, err)
	}
}

func TestHubFeedbackScreenshotsCommitToDedicatedBranch(t *testing.T) {
	var (
		puts         []map[string]string
		refCreated   bool
		contentPaths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/repos/hivecommons/hive/contents/"):
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			puts = append(puts, payload)
			contentPaths = append(contentPaths, strings.TrimPrefix(r.URL.Path, "/repos/hivecommons/hive/contents/"))
			if !refCreated {
				// GitHub answers 404 for a branch that does not exist yet.
				http.Error(w, `{"message":"Branch not found"}`, http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]string{"download_url": "https://raw.githubusercontent.com/hivecommons/hive/" + payload["branch"] + "/" + r.URL.Path}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive":
			_ = json.NewEncoder(w).Encode(map[string]string{"default_branch": "v5"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/git/ref/heads/v5":
			_ = json.NewEncoder(w).Encode(map[string]any{"object": map[string]string{"sha": "abc123"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/git/refs":
			var payload map[string]string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["ref"] != "refs/heads/"+feedbackScreenshotBranch || payload["sha"] != "abc123" {
				t.Fatalf("unexpected ref creation payload %v", payload)
			}
			refCreated = true
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			w.WriteHeader(http.StatusCreated)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	uploadHubFeedbackScreenshots(context.Background(), srv.Client(), srv.URL, "tok", "hivecommons", "hive", 7, []string{pngDataURI(t)})

	if !refCreated {
		t.Fatal("dedicated branch was not created")
	}
	if len(puts) != 2 {
		t.Fatalf("expected a retried upload after creating the branch, got %d puts", len(puts))
	}
	for i, p := range puts {
		if p["branch"] != feedbackScreenshotBranch {
			t.Fatalf("put %d targeted branch %q, want %q", i, p["branch"], feedbackScreenshotBranch)
		}
	}
	for _, p := range contentPaths {
		if strings.HasPrefix(p, ".github") {
			t.Fatalf("screenshot committed under .github: %s", p)
		}
		if !strings.HasPrefix(p, feedbackScreenshotDir+"/7/screenshot-1.png") {
			t.Fatalf("unexpected content path %s", p)
		}
	}
}

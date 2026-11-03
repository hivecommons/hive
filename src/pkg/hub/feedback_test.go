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

	"github.com/hivecommons/hive/pkg/feedbackbundle"
)

// TestHubFeedbackStoreBundleNeverCreatesIssue pins the #1927 handoff: the hub
// commits the bundle (screenshots then report) to the hive-feedback branch
// with the App token and never touches the issues API.
func TestHubFeedbackStoreBundleNeverCreatesIssue(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/issues") || strings.Contains(r.URL.Path, "/issues/") {
			t.Fatalf("issues API must not be called: %s %s", r.Method, r.URL.Path)
		}
		if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/repos/hivecommons/hive/contents/") {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["branch"] != feedbackbundle.Branch {
			t.Fatalf("branch = %q", payload["branch"])
		}
		rel := strings.TrimPrefix(r.URL.Path, "/repos/hivecommons/hive/contents/")
		paths = append(paths, rel)
		_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]string{"html_url": "https://github.com/hivecommons/hive/blob/" + feedbackbundle.Branch + "/" + rel, "download_url": "https://raw.example/" + rel}})
	}))
	defer srv.Close()
	req := feedbackReportRequest{Title: "Bug from dashboard", Description: "Something bad happened", RequestType: feedbackTypeBug, TargetRepo: feedbackTargetHive, IncludeDiagnostics: true, Diagnostics: &feedbackDiagnostics{HiveID: "hive-one"}, Screenshots: []string{pngDataURI(t)}}
	stored, err := storeHubFeedbackBundle(context.Background(), srv.Client(), "tok", req, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/screenshot-1.png") || !strings.HasSuffix(paths[1], "/"+feedbackbundle.ReportFile) {
		t.Fatalf("paths = %v", paths)
	}
	if stored.ID == "" || !strings.HasSuffix(stored.ReportURL, "/"+feedbackbundle.Dir+"/"+stored.ID+"/"+feedbackbundle.ReportFile) || len(stored.Screenshots) != 1 {
		t.Fatalf("stored = %+v", stored)
	}
}

func TestHubFeedbackBundleReportAttributionShapes(t *testing.T) {
	// Whatever credential_login an (older) spoke claims, the bundle report
	// names only the user: they file the issue themselves (#1927).
	tests := []struct {
		name            string
		credentialLogin string
		hubName         string
		hiveID          string
		submitter       feedbackSubmitterIdentity
		wantLine        string
		notWant         []string
	}{
		{
			name:            "hub-linked verified user",
			credentialLogin: "hivecommons-hive[bot]",
			hubName:         "https://hub.example",
			hiveID:          "hive-linked",
			submitter:       feedbackSubmitterIdentity{Name: "alice", GitHubLogin: "alice", Source: "GitHub dashboard identity"},
			wantLine:        "Opened by @alice from hive hive-linked (https://hub.example)",
			notWant:         []string{"on behalf of", "[bot]"},
		},
		{
			name:            "missing identity",
			credentialLogin: "hivecommons-hive[bot]",
			hubName:         "hub-less",
			hiveID:          "hive-solo",
			submitter:       feedbackSubmitterIdentity{Name: "an unidentified dashboard user", Source: "unidentified dashboard user"},
			wantLine:        "Opened by an unidentified dashboard user from hive hive-solo (hub-less)",
			notWant:         []string{"/cc @", "[bot]"},
		},
		{
			name:            "self-reported login is not mentioned",
			credentialLogin: "hivecommons-hive[bot]",
			hubName:         "https://hub.example",
			hiveID:          "hive-linked",
			submitter:       feedbackSubmitterIdentity{Name: "mallory", GitHubLogin: "mallory", Source: hubFeedbackSourceEntered},
			wantLine:        "Opened by `mallory` (self-reported GitHub username, unverified) from hive hive-linked (https://hub.example)",
			notWant:         []string{"/cc @", "@mallory", "[bot]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := buildHubFeedbackBundleReport(feedbackReportRequest{
				Title:           "Bug from dashboard",
				Description:     "Something went wrong",
				RequestType:     feedbackTypeBug,
				TargetRepo:      feedbackTargetHive,
				HiveID:          tt.hiveID,
				CredentialLogin: tt.credentialLogin,
				HubName:         tt.hubName,
				Submitter:       tt.submitter,
				OpenedByHive:    true,
				Diagnostics:     &feedbackDiagnostics{HiveID: "hive-one", Channel: "edge"},
			})
			if !strings.HasPrefix(body, "# Bug from dashboard\n\n"+tt.wantLine+"\n\nSomething went wrong") {
				t.Fatalf("feedback body prefix mismatch; want %q:\n%s", tt.wantLine, body)
			}
			if tt.submitter.GitHubLogin != "" {
				wantRow := "| Submitted by | @" + tt.submitter.GitHubLogin + " (GitHub dashboard identity) |"
				if tt.submitter.Source == hubFeedbackSourceEntered {
					wantRow = "| Submitted by | `" + tt.submitter.GitHubLogin + "` (self-reported GitHub username, unverified) (" + hubFeedbackSourceEntered + ") |"
				}
				if !strings.Contains(body, wantRow) {
					t.Fatalf("feedback body missing Submitted by row %q:\n%s", wantRow, body)
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

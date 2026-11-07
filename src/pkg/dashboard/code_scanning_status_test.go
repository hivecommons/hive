package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/codescanning"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// /api/status code_scanning carries per-repo alert counts by severity, how
// many open alerts hive issues track, the Security-tab link, and a notice
// for a repo whose alerts the App cannot read.
func TestStatusCodeScanning(t *testing.T) {
	tracked := codescanning.Marker{Tool: "CodeQL", Rules: []string{"cpp/use-after-free"}, Paths: []string{"a.cc"}, Alerts: []int{7}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/ok/code-scanning/alerts":
			_, _ = w.Write([]byte(`[
				{"number":7,"state":"open","rule":{"id":"cpp/use-after-free","severity":"warning","security_severity_level":"critical"},"tool":{"name":"CodeQL"},"most_recent_instance":{"location":{"path":"a.cc","start_line":4}}},
				{"number":8,"state":"open","rule":{"id":"cpp/commented-out-code","severity":"note"},"tool":{"name":"CodeQL"},"most_recent_instance":{"location":{"path":"b.cc","start_line":9}}}]`))
		case "/repos/o/ok/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 3, "state": "open", "title": "x", "body": tracked.String(), "user": map[string]any{"login": "hive-app[bot]"}}})
		case "/repos/o/denied/code-scanning/alerts":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer srv.Close()
	s := newTestServer()
	gh := github.NewClientForTest(srv.URL, "o", []string{"ok", "denied"}, s.logger)
	gh.SetAppBotLogin("hive-app[bot]")
	cfg := &config.Config{}
	cfg.Project.Org, cfg.Project.Repos = "o", []string{"ok", "denied"}
	cfg.CodeScanning.Enabled = true
	gh.RunCodeScanningSweep(context.Background(), cfg, time.Now())
	s.deps = &Dependencies{GHClient: gh}
	s.UpdateStatus(minimalPayload())

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status?fields=code_scanning", nil))
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	raw, ok := payload["code_scanning"]
	if !ok {
		t.Fatalf("code_scanning missing: %s", rec.Body.String())
	}
	var got github.CodeScanningStatusBlock
	if err := json.Unmarshal(raw, &got); err != nil || !got.Enabled || len(got.Repos) != 2 {
		t.Fatalf("code_scanning=%s err=%v", raw, err)
	}
	denied, okRepo := got.Repos[0], got.Repos[1]
	if okRepo.Repo != "o/ok" || okRepo.BySeverity["critical"] != 1 || okRepo.BySeverity["note"] != 1 || okRepo.Tracked != 1 || okRepo.OpenIssues != 1 {
		t.Fatalf("o/ok status %+v", okRepo)
	}
	if okRepo.SecurityURL != "https://github.com/o/ok/security/code-scanning" {
		t.Fatalf("security link %q", okRepo.SecurityURL)
	}
	if denied.NoticeKind != codescanning.NoticeMissingPermission || len(got.Notices) != 1 || !strings.Contains(got.Notices[0], "Code scanning alerts") {
		t.Fatalf("missing-permission notice: %+v / %v", denied, got.Notices)
	}
}

// A hive with the sweep off carries no code_scanning block.
func TestStatusCodeScanningAbsentWhenOff(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{GHClient: github.NewClient("fake", "org", []string{"repo"}, s.logger, "")}
	s.UpdateStatus(minimalPayload())
	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if strings.Contains(rec.Body.String(), `"code_scanning"`) {
		t.Fatal("code_scanning present with the sweep off")
	}
}

// The notices reach the operator as a pinned banner with a Security-tab link.
func TestCodeScanningBannerWired(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`id="code-scanning-banner" class="gh-app-install-banner perm-issue" hidden`,
		`id="code-scanning-banner-list"`,
		"renderCodeScanningBanner(data);",
		"data.code_scanning.repos",
		"r.security_url",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("code scanning banner is missing %q", want)
		}
	}
	notices := strings.Index(html, `<div id="dash-notices">`)
	banner := strings.Index(html, `id="code-scanning-banner"`)
	firstSection := strings.Index(html, `data-dashboard-section="overview-section"`)
	if notices < 0 || banner < notices || banner > firstSection {
		t.Fatal("code scanning banner must sit in the pinned dashboard notices")
	}
}

package hub

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

type fakeLiteRepoClient struct {
	dirs   map[string][]liteRepoEntry
	exists map[string]bool
}

func (f fakeLiteRepoClient) ListDir(_ context.Context, _, _, path string) ([]liteRepoEntry, error) {
	entries, ok := f.dirs[path]
	if !ok {
		return nil, http.ErrMissingFile
	}
	return entries, nil
}

func (f fakeLiteRepoClient) Exists(_ context.Context, _, _, path string) (bool, error) {
	return f.exists[path], nil
}

func (f fakeLiteRepoClient) UpsertIssue(context.Context, string, string, string, string, string) (int, string, error) {
	return 0, "", nil
}

func TestEvaluateLiteRepoTable(t *testing.T) {
	tests := []struct {
		name          string
		dirs          map[string][]liteRepoEntry
		exists        map[string]bool
		wantPassedMin int
		wantMissing   string
		wantCodebase  int
	}{
		{
			name: "detects common readiness signals",
			dirs: map[string][]liteRepoEntry{
				"":                  {{Name: "go.mod"}, {Name: "CONTRIBUTING.md"}, {Name: "AGENTS.md"}, {Name: ".editorconfig"}},
				".github":           {{Name: "workflows", Type: "dir"}, {Name: "pull_request_template.md"}},
				".github/workflows": {{Name: "ci.yml"}},
			},
			wantPassedMin: 5,
			wantMissing:   "Coverage gate",
			wantCodebase:  0,
		},
		{
			name: "empty repo reports missing ci",
			dirs: map[string][]liteRepoEntry{
				"": {},
			},
			wantPassedMin: 0,
			wantMissing:   "CI/CD pipeline",
			wantCodebase:  0,
		},
		{
			name: "non-prefetched file path uses existence check",
			dirs: map[string][]liteRepoEntry{
				"": {},
			},
			exists:        map[string]bool{"web/playwright.config.ts": true},
			wantPassedMin: 1,
			wantMissing:   "CI/CD pipeline",
			wantCodebase:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := evaluateLiteRepo(context.Background(), fakeLiteRepoClient{dirs: tt.dirs, exists: tt.exists}, "o", "r", 2)
			if err != nil {
				t.Fatalf("evaluateLiteRepo: %v", err)
			}
			if report.ACMMLevel != 2 || report.CodebaseLevel != tt.wantCodebase || report.CriteriaPassed < tt.wantPassedMin {
				t.Fatalf("report = %#v", report)
			}
			found := false
			for _, f := range report.Findings {
				if f.Title == tt.wantMissing {
					found = true
				}
				if f.Level > 2 {
					t.Fatalf("lite report included above-cap finding %#v", f)
				}
			}

			if !found {
				t.Fatalf("missing finding %q not present in %#v", tt.wantMissing, report.Findings)
			}
		})
	}
}

func TestLiteAppIdentityFollowsRepoHost(t *testing.T) {
	s := &HubServer{clusters: map[string]ClusterConfig{
		defaultClusterID: {ID: defaultClusterID, GitHubAppID: 1},
		"ghe":            {ID: "ghe", GitHubBaseURL: "https://github.example.com", GitHubAPIURL: "https://github.example.com/api/v3", GitHubAppID: 2},
	}}
	got := s.liteAppIdentityForEntry(RegistryEntry{GitHubHost: "github.example.com"})
	if got == nil || got.APIURL != "https://github.example.com/api/v3" || got.AppID != 2 {
		t.Fatalf("identity = %#v, want GHE app identity", got)
	}
}

func TestLiteExecutionRateLimitsScans(t *testing.T) {
	now := time.Now().UTC()
	s := &HubServer{
		logger: slog.Default(),
		saveCh: make(chan struct{}, 1),
		registry: Registry{Hives: []RegistryEntry{
			{ID: "due", Lite: true, HiveType: "lite", Org: "o", PrimaryRepo: "due", GitHubInstallationID: 1},
			{ID: "fresh", Lite: true, HiveType: "lite", Org: "o", PrimaryRepo: "fresh", GitHubInstallationID: 1, LiteReport: &LiteAdvisoryReport{GeneratedAt: now.Format(time.RFC3339)}},
			{ID: "spoke", Org: "o", PrimaryRepo: "spoke", GitHubInstallationID: 1},
		}},
		liteExecution: &liteExecutionState{},
	}
	var scanned []string
	s.liteExecution.scanner = func(_ context.Context, entry RegistryEntry) (*LiteAdvisoryReport, error) {
		scanned = append(scanned, entry.ID)
		return &LiteAdvisoryReport{GeneratedAt: now.Add(time.Minute).Format(time.RFC3339), Status: "ok", ACMMLevel: 2}, nil
	}
	s.runLiteExecutionOnce(context.Background())
	if strings.Join(scanned, ",") != "due" {
		t.Fatalf("scanned = %v, want only due lite entry", scanned)
	}
	if s.registry.Hives[0].LiteReport == nil || s.registry.Hives[0].LiteReport.Status != "ok" {
		t.Fatalf("due report not stored: %#v", s.registry.Hives[0].LiteReport)
	}
}

func TestLiteReportAuthz(t *testing.T) {
	s, cleanup := newLiteTestHub(t)
	defer cleanup()
	s.registry.Hives = []RegistryEntry{{
		ID:         "lite-1",
		Owner:      "lite-user",
		Lite:       true,
		HiveType:   "lite",
		LiteReport: &LiteAdvisoryReport{GeneratedAt: time.Now().UTC().Format(time.RFC3339), Status: "ok", ACMMLevel: 2},
	}}
	mkUser(t, "stranger")

	tests := []struct {
		name       string
		user       string
		wantStatus int
	}{
		{name: "owner can read", user: "lite-user", wantStatus: http.StatusOK},
		{name: "stranger denied", user: "stranger", wantStatus: http.StatusForbidden},
		{name: "anonymous denied", user: "", wantStatus: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := reqWithUser(http.MethodGet, "/api/saas/lite/lite-1/report", "", tt.user)
			req.SetPathValue("id", "lite-1")
			s.handleLiteReport(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s want %d", rec.Code, rec.Body.String(), tt.wantStatus)
			}
		})
	}
}

func TestLiteGitHubClientUpsertIssueIdempotent(t *testing.T) {
	var created, edited int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			q, _ := url.QueryUnescape(r.URL.Query().Get("q"))
			if strings.Contains(q, liteExecutionIssueMarker) && created > 0 {
				io.WriteString(w, `{"total_count":1,"items":[{"number":7,"state":"open","html_url":"https://github.com/o/r/issues/7"}]}`)
				return
			}
			io.WriteString(w, `{"total_count":0,"items":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			created++
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"number":7,"html_url":"https://github.com/o/r/issues/7"}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/issues/7":
			edited++
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body["body"].(string), liteExecutionIssueMarker) {
				t.Fatalf("bad edit body: %#v err=%v", body, err)
			}
			io.WriteString(w, `{"number":7,"html_url":"https://github.com/o/r/issues/7"}`)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	client := &liteGitHubClient{gh: gh.NewClient(srv.Client())}
	base, _ := url.Parse(srv.URL + "/")
	client.gh.BaseURL = base
	body := liteExecutionIssueMarker + "\nbody"
	for i := 0; i < 2; i++ {
		num, issueURL, err := client.UpsertIssue(context.Background(), "o", "r", liteExecutionIssueTitle, liteExecutionIssueMarker, body)
		if err != nil {
			t.Fatalf("UpsertIssue %d: %v", i, err)
		}
		if num != 7 || issueURL == "" {
			t.Fatalf("issue result = %d %q", num, issueURL)
		}
	}
	if created != 1 || edited != 1 {
		t.Fatalf("created=%d edited=%d, want one create then one edit", created, edited)
	}
}

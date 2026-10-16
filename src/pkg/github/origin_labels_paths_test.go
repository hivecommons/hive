package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/originlabel"
)

// Every server-side filer in this package must stamp exactly one origin/*
// label at creation (hivecommons/hive#12432), whatever its caller or the
// agent prompt supplied. These tests drive each filer through a fake GitHub
// and inspect the labels on the POST /issues payload.

// createdLabels decodes the labels array of a captured issue-create payload.
func createdLabels(t *testing.T, body []byte) []string {
	t.Helper()
	var payload struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode create payload: %v\n%s", err, body)
	}
	return payload.Labels
}

func assertExactlyOneOrigin(t *testing.T, labels []string, want string) {
	t.Helper()
	n := 0
	for _, l := range labels {
		if originlabel.Is(l) {
			n++
			if l != want {
				t.Fatalf("labels %v carry %q, want %q", labels, l, want)
			}
		}
	}
	if n != 1 {
		t.Fatalf("labels %v carry %d origin/* labels, want exactly one (%s)", labels, n, want)
	}
}

// The relay (hive-open-issue) stamps origin/agent regardless of what the
// agent asked for, including an origin/* it tried to pick itself.
func TestIssueRequestWatcher_StampsOriginAgentAndOverridesAgentSupplied(t *testing.T) {
	var mu sync.Mutex
	var created [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"x","color":"8250df"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			created = append(created, b)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":1,"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := issueTestClient(t, srv.URL)
	dir := withIssueDir(t)

	for i, labels := range [][]string{
		{"agent/scanner", "hive/h1", "kind/bug"},
		{"agent/reviewer", "origin/reviewer", "from-review", "origin/agent"},
	} {
		if _, err := WriteIssueRequest(dir, IssueRequest{
			Repo: "o/r", Title: fmt.Sprintf("[scanner] finding %d", i), Body: "detail",
			Labels: labels, Agent: "scanner",
		}); err != nil {
			t.Fatal(err)
		}
	}
	c.ProcessIssueRequestsOnce(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(created) != 2 {
		t.Fatalf("created %d issues, want 2", len(created))
	}
	for _, body := range created {
		labels := createdLabels(t, body)
		assertExactlyOneOrigin(t, labels, OriginAgentLabel)
		for _, l := range labels {
			if l == "from-review" || l == "origin/reviewer" {
				t.Fatalf("agent-supplied provenance %q survived: %v", l, labels)
			}
		}
	}
	if got := createdLabels(t, created[0]); strings.Join(got, ",") != "agent/scanner,hive/h1,kind/bug,origin/agent" {
		t.Fatalf("plain request labels = %v", got)
	}
}

// The advisory report carries origin/advisory next to the hive/advisory
// signal label, and the ensure pass on an existing issue adds the same pair.
func TestEnsureAdvisoryIssue_StampsOriginAdvisory(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var mu sync.Mutex
	var created [][]byte
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s", org, repo), func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": org + "/" + repo, "has_issues": true})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			created = append(created, b)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "title": advisoryTitle})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "x"})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels/", org, repo), func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	if _, err := c.EnsureAdvisoryIssue(context.Background(), repo); err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 1 {
		t.Fatalf("created %d, want 1", len(created))
	}
	labels := createdLabels(t, created[0])
	assertExactlyOneOrigin(t, labels, OriginAdvisoryLabel)
	if strings.Join(labels, ",") != advisoryLabelName+","+OriginAdvisoryLabel {
		t.Fatalf("labels = %v, want [%s %s]", labels, advisoryLabelName, OriginAdvisoryLabel)
	}
}

// The fleet / self report filer stamps origin/selfreport next to the
// hive/selfreport dedupe signal, even when the report builder omitted it.
func TestEnsureFleetReport_StampsOriginSelfReport(t *testing.T) {
	var mu sync.Mutex
	var created [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/labels":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "x"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			created = append(created, b)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "html_url": "https://github.com/hivecommons/hive/issues/9", "user": map[string]any{"login": "bot"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	c.appBotLogin = "hive-app[bot]"
	_, err := c.EnsureFleetReport(context.Background(), fleetreport.Report{
		Fingerprint: "fp", Title: "Fleet report", Body: "<!-- hive-fleet-fingerprint:fp -->\nbody",
		Labels: []string{"fleet-report", "hive/selfreport", "from-review"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(created) != 1 {
		t.Fatalf("created %d, want 1", len(created))
	}
	labels := createdLabels(t, created[0])
	assertExactlyOneOrigin(t, labels, OriginSelfReportLabel)
	if strings.Join(labels, ",") != "fleet-report,hive/selfreport,"+OriginSelfReportLabel {
		t.Fatalf("labels = %v", labels)
	}
}

// Every origin/* label the filers use is in the shared vocabulary, so the
// ensure helper gives each one the same colour and a description.
func TestFilerOriginLabelsAreInVocabulary(t *testing.T) {
	for _, l := range []string{OriginAgentLabel, OriginReviewLabel, OriginAdvisoryLabel, OriginRecommendationsLabel, OriginSelfReportLabel, OriginFeedbackLabel, OriginUpstreamLabel, OriginAuditLabel, OriginDashboardLabel} {
		def, ok := originLabelDefinition(l)
		if !ok || def.color != OriginLabelColor {
			t.Errorf("%s: definition = %+v ok=%v", l, def, ok)
		}
		if _, known := originlabel.Descriptions[l]; !known {
			t.Errorf("%s has no description in pkg/originlabel", l)
		}
	}
}

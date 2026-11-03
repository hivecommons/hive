package github

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/claims"
)

// Only the retired per-login spellings are candidates: the generic label,
// the agent fallback, backend names and this hive's agents are skipped.
func TestLegacyClaimedByLoginLabels(t *testing.T) {
	defined := []string{
		"bug", "hive/claimed-by-human", "hive/claimed-by-agent", "hive/claimed-by-copilot",
		"hive/claimed-by-scanner", "hive/claimed-by-Contributor", "hive/claimed-by-clubanderson",
		"hive/claimed-by-Karibhou", "HIVE/claimed-by-karibhou", "stage/020-claimed",
	}
	got := legacyClaimedByLoginLabels(defined, []string{"scanner", "contributor", "copilot"})
	// GitHub label names are case-insensitive, so the two Karibhou
	// spellings are one candidate.
	if strings.Join(got, ",") != "hive/claimed-by-Karibhou,hive/claimed-by-clubanderson" {
		t.Fatalf("candidates = %v", got)
	}
}

// One sweep: every open item wearing a per-login label gets the generic
// label and loses the login; the login is recorded in the report and the
// per-login definition is deleted. An item whose live ledger claim is a
// hive-agent claim named by the label (another hive's agent) is skipped and
// its definition kept. The marker makes the second run a no-op.
func TestMigrateClaimedByHumanLabels(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issuesByLabel := map[string][]wireIssueWithBody{
		"hive/claimed-by-clubanderson": {
			{wireIssue: wireIssue{Number: 1, Title: "login only", Labels: []wireLabel{{Name: "hive/claimed-by-clubanderson"}, {Name: "stage/020-claimed"}}, CreatedAt: hoursAgo(3)}},
			{wireIssue: wireIssue{Number: 2, Title: "already generic too", Labels: []wireLabel{{Name: "hive/claimed-by-clubanderson"}, {Name: claims.ClaimedByHumanLabel}}, CreatedAt: hoursAgo(2)}},
		},
		"hive/claimed-by-Karibhou": {
			{wireIssue: wireIssue{Number: 3, Title: "contributor PR", Labels: []wireLabel{{Name: "hive/claimed-by-Karibhou"}}, CreatedAt: hoursAgo(2), PullRequest: &struct{}{}}},
		},
		"hive/claimed-by-otherhive-agent": {
			{wireIssue: wireIssue{Number: 4, Title: "foreign agent", Labels: []wireLabel{{Name: "hive/claimed-by-otherhive-agent"}}, CreatedAt: hoursAgo(1)}},
		},
	}
	var mu sync.Mutex
	added := map[int][]string{}
	removed := map[int][]string{}
	var definitionDeletes []string
	labelLists := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		labelLists++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"bug"},{"name":"hive/claimed-by-human"},{"name":"hive/claimed-by-scanner"},{"name":"hive/claimed-by-copilot"},{"name":"hive/claimed-by-clubanderson"},{"name":"hive/claimed-by-Karibhou"},{"name":"hive/claimed-by-otherhive-agent"}]`))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		name, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/repos/"+org+"/"+repo+"/labels/"))
		mu.Lock()
		definitionDeletes = append(definitionDeletes, name)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("labels")
		w.Header().Set("Content-Type", "application/json")
		items, ok := issuesByLabel[label]
		if !ok {
			t.Errorf("listed unexpected label %q", label)
		}
		_, _ = w.Write(mustMarshal(t, items))
	})
	for _, n := range []int{1, 2, 3, 4} {
		n := n
		base := "/repos/" + org + "/" + repo + "/issues/" + strconv.Itoa(n) + "/labels"
		mux.HandleFunc(base, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			added[n] = append(added[n], string(body))
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		})
		mux.HandleFunc(base+"/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
			name, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, base+"/"))
			mu.Lock()
			removed[n] = append(removed[n], name)
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	c := newTestClient(t, server, org, []string{repo})
	opts := ClaimedByHumanMigrationOptions{
		AgentNames: []string{"scanner", "copilot"},
		LookupClaim: func(repo string, number int) (claims.Claim, bool) {
			switch number {
			case 1:
				return claims.Claim{Repo: repo, Issue: 1, Holder: "clubanderson", Kind: claims.KindHuman}, true
			case 4:
				return claims.Claim{Repo: repo, Issue: 4, Holder: "otherhive-agent", Kind: claims.KindAgent}, true
			}
			return claims.Claim{}, false
		},
		DataDir:    dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath: filepath.Join(dir, "marker"),
		ReportPath: filepath.Join(dir, "report.json"),
	}
	report, err := c.MigrateClaimedByHumanLabels(context.Background(), opts)
	if err != nil {
		t.Fatalf("MigrateClaimedByHumanLabels: %v", err)
	}
	if report == nil || len(report.Errors) != 0 || report.To != "hive/claimed-by-human" {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Migrated) != 3 {
		t.Fatalf("migrated = %+v, want 3 items", report.Migrated)
	}
	byNumber := map[int]ClaimedByHumanMigrationItem{}
	for _, it := range report.Migrated {
		byNumber[it.Number] = it
	}
	if it := byNumber[1]; it.Login != "clubanderson" || !it.LiveClaim || !it.Added {
		t.Fatalf("item 1 = %+v, want login recorded from a live human claim and the generic label added", it)
	}
	if it := byNumber[2]; it.Login != "clubanderson" || it.LiveClaim || it.Added {
		t.Fatalf("item 2 = %+v, want no add when the generic label is already present", it)
	}
	if it := byNumber[3]; it.Login != "Karibhou" || it.Type != "pr" || !it.Added {
		t.Fatalf("item 3 = %+v", it)
	}
	if strings.Join(report.Skipped, ",") != org+"/"+repo+"#4:hive/claimed-by-otherhive-agent" {
		t.Fatalf("skipped = %v, want the live foreign-agent claim kept", report.Skipped)
	}
	mu.Lock()
	defer mu.Unlock()
	for n, want := range map[int]string{1: `["hive/claimed-by-human"]`, 2: "", 3: `["hive/claimed-by-human"]`, 4: ""} {
		got := strings.TrimSpace(strings.Join(added[n], ","))
		if got != want {
			t.Fatalf("issue %d added %q, want %q", n, got, want)
		}
	}
	if strings.Join(removed[1], ",") != "hive/claimed-by-clubanderson" || strings.Join(removed[2], ",") != "hive/claimed-by-clubanderson" || strings.Join(removed[3], ",") != "hive/claimed-by-Karibhou" || len(removed[4]) != 0 {
		t.Fatalf("removed = %v", removed)
	}
	if strings.Join(definitionDeletes, ",") != "hive/claimed-by-Karibhou,hive/claimed-by-clubanderson" {
		t.Fatalf("definition deletes = %v, want both per-login labels and not the foreign agent's", definitionDeletes)
	}
	if strings.Join(report.DefinitionsDeleted, ",") != org+"/"+repo+":hive/claimed-by-Karibhou,"+org+"/"+repo+":hive/claimed-by-clubanderson" {
		t.Fatalf("report definitions deleted = %v", report.DefinitionsDeleted)
	}
	if _, err := os.Stat(opts.MarkerPath); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	data, err := os.ReadFile(opts.ReportPath)
	if err != nil || !strings.Contains(string(data), ClaimedByHumanMigrationID) || !strings.Contains(string(data), `"login": "clubanderson"`) {
		t.Fatalf("report file = %q, %v", data, err)
	}

	again, err := c.MigrateClaimedByHumanLabels(context.Background(), opts)
	if err != nil || again != nil {
		t.Fatalf("second run = %+v, %v; want a no-op once the marker exists", again, err)
	}
	if labelLists != 1 {
		t.Fatalf("second run listed labels again (%d lists)", labelLists)
	}
}

// A failed write leaves the definition in place, writes the report, skips
// the marker and returns the error so the next boot retries.
func TestMigrateClaimedByHumanLabelsRetriesOnError(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var mu sync.Mutex
	definitionDeletes := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"name":"hive/claimed-by-clubanderson"}]`))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		definitionDeletes++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, []wireIssueWithBody{{wireIssue: wireIssue{Number: 1, Labels: []wireLabel{{Name: "hive/claimed-by-clubanderson"}}, CreatedAt: hoursAgo(1)}}}))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/1/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	c := newTestClient(t, server, org, []string{repo})
	opts := ClaimedByHumanMigrationOptions{
		DataDir:    dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath: filepath.Join(dir, "marker"),
		ReportPath: filepath.Join(dir, "report.json"),
	}
	report, err := c.MigrateClaimedByHumanLabels(context.Background(), opts)
	if err == nil || report == nil || len(report.Errors) != 1 || len(report.Migrated) != 0 {
		t.Fatalf("report = %+v, err = %v; want one error and nothing migrated", report, err)
	}
	if definitionDeletes != 0 {
		t.Fatal("definition must be kept while an open item still carries the label")
	}
	if _, err := os.Stat(opts.MarkerPath); err == nil {
		t.Fatal("marker must not be written after errors")
	}
	if _, err := os.Stat(opts.ReportPath); err != nil {
		t.Fatalf("report not written: %v", err)
	}
}

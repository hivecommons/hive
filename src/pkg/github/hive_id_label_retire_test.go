package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type wireIssueWithBody struct {
	wireIssue
	Body string `json:"body"`
}

func TestRetireHiveIDLabelsStripsOpenItemsAndDeletesDefinitionOnce(t *testing.T) {
	org, repo := "testorg", "testrepo"
	hiveID := "hosted-available-oke-11-placeholder-r05x"
	label := HiveProvenanceLabel(hiveID)
	issues := []wireIssueWithBody{
		{wireIssue: wireIssue{Number: 1, Title: "marker in body", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: label}, {Name: "agent/scanner"}}, CreatedAt: hoursAgo(3)}, Body: "prose\n" + OriginMarker(hiveID, time.Now().Add(-3*time.Hour))},
		{wireIssue: wireIssue{Number: 2, Title: "label only PR", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: label}, {Name: "hive/selfreport"}}, CreatedAt: hoursAgo(2), PullRequest: &struct{}{}}, Body: "prose"},
	}
	var mu sync.Mutex
	removed := map[int][]string{}
	var definitionDeletes []string
	lists := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lists++
		mu.Unlock()
		if !strings.Contains(r.URL.RawQuery, "labels=hive%2F"+hiveID) {
			t.Errorf("labels query = %q, want %s filter", r.URL.RawQuery, label)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, issues))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		definitionDeletes = append(definitionDeletes, strings.TrimPrefix(r.URL.Path, "/repos/"+org+"/"+repo+"/labels/"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	for _, n := range []int{1, 2} {
		n := n
		mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(n)+"/labels/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			}
			mu.Lock()
			removed[n] = append(removed[n], strings.TrimPrefix(r.URL.Path, "/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(n)+"/labels/"))
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	c := newTestClient(t, server, org, []string{repo})
	opts := HiveIDLabelRetireOptions{
		HiveID:      hiveID,
		ExtraLabels: []string{label, "hive/selfreport", " "},
		DataDir:     dir,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath:  filepath.Join(dir, "marker"),
		ReportPath:  filepath.Join(dir, "report.json"),
	}
	report, err := c.RetireHiveIDLabels(context.Background(), opts)
	if err != nil {
		t.Fatalf("RetireHiveIDLabels: %v", err)
	}
	if report == nil || len(report.Errors) != 0 {
		t.Fatalf("report = %+v", report)
	}
	// The duplicate spelling and the signal label collapse to one label.
	if strings.Join(report.Labels, ",") != label {
		t.Fatalf("labels swept = %v, want only %s", report.Labels, label)
	}
	if lists != 1 {
		t.Fatalf("listed %d times, want once per label", lists)
	}
	if len(report.Stripped) != 2 || report.Stripped[0].Number != 1 || report.Stripped[1].Number != 2 || report.Stripped[1].Type != "pr" {
		t.Fatalf("stripped = %+v", report.Stripped)
	}
	if report.Stripped[0].OriginSource != OriginSourceMarker || report.Stripped[1].OriginSource != OriginSourceNone {
		t.Fatalf("origin sources = %+v", report.Stripped)
	}
	if strings.Join(removed[1], ",") != label || strings.Join(removed[2], ",") != label {
		t.Fatalf("removed = %v, want %s off both items", removed, label)
	}
	if strings.Join(definitionDeletes, ",") != label || strings.Join(report.DefinitionsDeleted, ",") != org+"/"+repo+":"+label {
		t.Fatalf("definition deletes = %v / %v", definitionDeletes, report.DefinitionsDeleted)
	}
	if _, err := os.Stat(opts.MarkerPath); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	data, err := os.ReadFile(opts.ReportPath)
	if err != nil || !strings.Contains(string(data), HiveIDLabelRetireID) {
		t.Fatalf("report file = %q, %v", data, err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("report json: %v", err)
	}

	again, err := c.RetireHiveIDLabels(context.Background(), opts)
	if err != nil || again != nil {
		t.Fatalf("second run = %+v, %v; want a no-op once the marker exists", again, err)
	}
	if lists != 1 {
		t.Fatalf("second run listed issues again (%d lists)", lists)
	}
}

func TestRetireHiveIDLabelsKeepsDefinitionAndRetriesOnError(t *testing.T) {
	org, repo := "testorg", "testrepo"
	label := HiveProvenanceLabel("hive-bold-fox")
	issues := []wireIssueWithBody{
		{wireIssue: wireIssue{Number: 1, Title: "ok", User: wireUser{"b"}, Labels: []wireLabel{{Name: label}}, CreatedAt: hoursAgo(3)}, Body: ""},
		{wireIssue: wireIssue{Number: 2, Title: "fails", User: wireUser{"b"}, Labels: []wireLabel{{Name: label}}, CreatedAt: hoursAgo(2)}, Body: ""},
	}
	var mu sync.Mutex
	definitionDeletes := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, issues))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		definitionDeletes++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/1/labels/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/2/labels/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	c := newTestClient(t, server, org, []string{repo})
	opts := HiveIDLabelRetireOptions{
		HiveID:     "hive-bold-fox",
		DataDir:    dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath: filepath.Join(dir, "marker"),
		ReportPath: filepath.Join(dir, "report.json"),
	}
	report, err := c.RetireHiveIDLabels(context.Background(), opts)
	if err == nil || report == nil || len(report.Errors) != 1 || len(report.Stripped) != 1 {
		t.Fatalf("report = %+v, err = %v; want one stripped, one error", report, err)
	}
	if definitionDeletes != 0 {
		t.Fatalf("definition deleted while an open item still carries the label")
	}
	if _, err := os.Stat(opts.MarkerPath); err == nil {
		t.Fatalf("marker written despite errors; the next boot must retry")
	}
	if _, err := os.Stat(opts.ReportPath); err != nil {
		t.Fatalf("report not written on error: %v", err)
	}

	// KeepDefinitions leaves the repo's label set alone even on a clean run.
	issues = issues[:1]
	opts.KeepDefinitions = true
	report, err = c.RetireHiveIDLabels(context.Background(), opts)
	if err != nil || report == nil || len(report.Stripped) != 1 || len(report.DefinitionsDeleted) != 0 || definitionDeletes != 0 {
		t.Fatalf("keep-definitions run = %+v, %v (deletes=%d)", report, err, definitionDeletes)
	}
	if _, err := os.Stat(opts.MarkerPath); err != nil {
		t.Fatalf("marker not written after clean run: %v", err)
	}
}

func TestRetireHiveIDLabelsNoLabelsIsNoop(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NotFoundHandler()), "o", []string{"r"})
	if report, err := c.RetireHiveIDLabels(context.Background(), HiveIDLabelRetireOptions{HiveID: "", ExtraLabels: []string{"hive/selfreport"}}); report != nil || err != nil {
		t.Fatalf("no sweepable labels = %+v, %v; want nil, nil", report, err)
	}
}

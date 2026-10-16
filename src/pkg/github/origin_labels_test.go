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
)

func TestOriginLabelDefinitionsShareOneColour(t *testing.T) {
	if !IsOriginLabel("origin/review") || !IsOriginLabel(" Origin/Selfreport ") || IsOriginLabel("origin/") || IsOriginLabel("from-review") || IsOriginLabel("agent/reviewer") {
		t.Fatal("IsOriginLabel prefix check")
	}
	known, ok := originLabelDefinition(OriginReviewLabel)
	if !ok || known.color != OriginLabelColor || !strings.Contains(known.description, "review") {
		t.Fatalf("origin/review definition = %+v, %v", known, ok)
	}
	other, ok := originLabelDefinition("origin/sentinel")
	if !ok || other.color != OriginLabelColor || other.description == "" {
		t.Fatalf("unknown origin source should still get the family colour: %+v, %v", other, ok)
	}
	if _, ok := originLabelDefinition("agent/reviewer"); ok {
		t.Fatal("agent/* is not a provenance label")
	}
	if def, ok := issueLabelDefinitionFor("meta"); !ok || def.color != "5319e7" {
		t.Fatalf("escalation definitions still win: %+v %v", def, ok)
	}
	if reviewBacklogLabel != "origin/review" || LegacyFromReviewLabel != "from-review" {
		t.Fatalf("backlog label = %q legacy = %q", reviewBacklogLabel, LegacyFromReviewLabel)
	}
}

func TestEnsureOriginLabelCreatesThenRecoloursIdempotently(t *testing.T) {
	org, repo := "o", "r"
	var mu sync.Mutex
	var existing map[string]any
	var creates, edits int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/origin/review", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if existing == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(existing)
		case http.MethodPatch:
			edits++
			var label map[string]any
			_ = json.NewDecoder(r.Body).Decode(&label)
			existing = label
			_ = json.NewEncoder(w).Encode(label)
		default:
			t.Errorf("unexpected %s", r.Method)
		}
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		creates++
		var label map[string]any
		_ = json.NewDecoder(r.Body).Decode(&label)
		existing = label
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(label)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	c := newTestClient(t, server, org, []string{repo})
	ctx := context.Background()

	if err := c.EnsureIssueLabel(ctx, org+"/"+repo, "origin/review", "ff0000", "ignored: origin/* owns its colour"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if creates != 1 || edits != 0 || existing["color"] != OriginLabelColor {
		t.Fatalf("after create: creates=%d edits=%d label=%v", creates, edits, existing)
	}
	if err := c.ensureCreateIssueLabel(ctx, org, repo, "origin/review"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if creates != 1 || edits != 0 {
		t.Fatalf("matching colour must be a no-op: creates=%d edits=%d", creates, edits)
	}
	existing["color"] = "d93f0b"
	if err := c.ensureCreateIssueLabel(ctx, org, repo, "origin/review"); err != nil {
		t.Fatalf("recolour: %v", err)
	}
	if creates != 1 || edits != 1 || existing["color"] != OriginLabelColor {
		t.Fatalf("drifted colour should be edited back: creates=%d edits=%d label=%v", creates, edits, existing)
	}
}

func TestMigrateOriginLabelsSwapsFromReviewOnceWithoutDoubleLabelling(t *testing.T) {
	org, repo := "testorg", "testrepo"
	issues := []wireIssue{
		{Number: 1, Title: "only legacy", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "from-review"}, {Name: "agent/scanner"}}, CreatedAt: hoursAgo(3)},
		{Number: 2, Title: "both names", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: "from-review"}, {Name: "origin/review"}}, CreatedAt: hoursAgo(2)},
	}
	var mu sync.Mutex
	added := map[int][]string{}
	removed := map[int][]string{}
	created := map[string]string{}
	lists := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lists++
		mu.Unlock()
		if !strings.Contains(r.URL.RawQuery, "labels=from-review") {
			t.Errorf("labels query = %q, want from-review filter", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, issues))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/origin/review", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		var label map[string]any
		_ = json.NewDecoder(r.Body).Decode(&label)
		mu.Lock()
		created[label["name"].(string)], _ = label["color"].(string)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(label)
	})
	for _, n := range []int{1, 2} {
		n := n
		mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/"+strconv.Itoa(n)+"/labels", func(w http.ResponseWriter, r *http.Request) {
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			mu.Lock()
			added[n] = append(added[n], labels...)
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		})
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
	opts := OriginLabelMigrationOptions{
		DataDir:    dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath: filepath.Join(dir, "marker"),
		ReportPath: filepath.Join(dir, "report.json"),
	}
	report, err := c.MigrateOriginLabels(context.Background(), opts)
	if err != nil {
		t.Fatalf("MigrateOriginLabels: %v", err)
	}
	if report == nil || len(report.Migrated) != 2 || len(report.Errors) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if strings.Join(added[1], ",") != "origin/review" {
		t.Fatalf("issue 1 added = %v, want origin/review", added[1])
	}
	if _, ok := added[2]; ok {
		t.Fatalf("issue 2 already carried origin/review; must not be added again: %v", added[2])
	}
	if strings.Join(removed[1], ",") != "from-review" || strings.Join(removed[2], ",") != "from-review" {
		t.Fatalf("removed = %v, want from-review off both", removed)
	}
	if created["origin/review"] != OriginLabelColor {
		t.Fatalf("created labels = %v, want origin/review in the family colour", created)
	}
	if !report.Migrated[1].AlreadyHadNew || report.Migrated[0].AlreadyHadNew {
		t.Fatalf("already_had_new flags = %+v", report.Migrated)
	}
	if _, err := os.Stat(opts.MarkerPath); err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if data, err := os.ReadFile(opts.ReportPath); err != nil || !strings.Contains(string(data), OriginLabelMigrationID) {
		t.Fatalf("report file = %q, %v", data, err)
	}

	again, err := c.MigrateOriginLabels(context.Background(), opts)
	if err != nil || again != nil {
		t.Fatalf("second run = %+v, %v; want a no-op once the marker exists", again, err)
	}
	if lists != 1 {
		t.Fatalf("second run listed issues again (%d lists)", lists)
	}
}

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
	"strings"
	"testing"
	"time"
)

func TestMigrateHiveHoldLabelClassifiesAuditProvenanceAndAmbiguous(t *testing.T) {
	org, repo := "testorg", "testrepo"
	oldLabel := "hive/h1"
	newLabel := "hive-pause/h1"
	issues := []wireIssue{
		{Number: 1, Title: "audit hold", User: wireUser{"alice"}, Labels: []wireLabel{{Name: oldLabel}}, CreatedAt: hoursAgo(3)},
		{Number: 2, Title: "provenance", User: wireUser{"hivecommons-hive[bot]"}, Labels: []wireLabel{{Name: oldLabel}, {Name: "agent/scanner"}}, CreatedAt: hoursAgo(2)},
		{Number: 3, Title: "ambiguous", User: wireUser{"bob"}, Labels: []wireLabel{{Name: oldLabel}}, CreatedAt: hoursAgo(1)},
	}
	added := map[int][]string{}
	created := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "labels=hive%2Fh1") {
			t.Fatalf("labels query = %q, want hive/h1 filter", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustMarshal(t, issues))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels/"+newLabel, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		var label map[string]any
		_ = json.NewDecoder(r.Body).Decode(&label)
		name, _ := label["name"].(string)
		created[name] = true
		_ = json.NewEncoder(w).Encode(label)
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/1/labels", addLabelsHandler(t, added, 1))
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/3/labels", addLabelsHandler(t, added, 3))
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/2/events", func(w http.ResponseWriter, r *http.Request) {
		writeEvents(t, w, oldLabel, "agent/scanner", time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	})
	mux.HandleFunc("/repos/"+org+"/"+repo+"/issues/3/events", func(w http.ResponseWriter, r *http.Request) {
		writeEvents(t, w, oldLabel, "", time.Date(2026, 9, 25, 11, 0, 0, 0, time.UTC))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(auditPath, []byte(`{"ts":"2026-09-25T10:51:15Z","action":"repo_item_hold_add","detail":"repo=testorg/testrepo, number=1, label=hive/h1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, server, org, []string{repo})
	report, err := c.MigrateHiveHoldLabel(context.Background(), HiveHoldMigrationOptions{
		HiveID:     "h1",
		AuditPath:  auditPath,
		DataDir:    dir,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		MarkerPath: filepath.Join(dir, "marker"),
		ReportPath: filepath.Join(dir, "report.json"),
	})
	if err != nil {
		t.Fatalf("MigrateHiveHoldLabel: %v", err)
	}
	if report.Summary.AuditHolds != 1 || report.Summary.ProvenanceOnly != 1 || report.Summary.Ambiguous != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
	if strings.Join(added[1], ",") != newLabel || strings.Join(added[3], ",") != newLabel {
		t.Fatalf("added labels = %+v, want new label on audit and ambiguous only", added)
	}
	if _, ok := added[2]; ok {
		t.Fatalf("provenance-only item got a hold label: %+v", added[2])
	}
	if !created[newLabel] {
		t.Fatalf("created labels = %+v, want %s", created, newLabel)
	}
}

func TestActiveHiveHoldAuditHoldsReadsRotatedAuditNames(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "audit.jsonl")
	rotatedPath := filepath.Join(dir, "audit-2026-09-25T10-51-15.jsonl")
	line := `{"ts":"2026-09-25T10:51:15Z","action":"repo_item_hold_add","detail":"repo=testorg/testrepo, number=7, label=hive/h1"}` + "\n"
	if err := os.WriteFile(rotatedPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	active := activeHiveHoldAuditHolds(auditPath, "hive/h1")
	if !active[migrationItemKey("testorg/testrepo", 7)] {
		t.Fatalf("rotated audit hold was not active: %+v", active)
	}
}

func addLabelsHandler(t *testing.T, added map[int][]string, number int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var labels []string
		_ = json.NewDecoder(r.Body).Decode(&labels)
		added[number] = append(added[number], labels...)
		_, _ = w.Write([]byte(`[]`))
	}
}

func writeEvents(t *testing.T, w http.ResponseWriter, hiveLabel, agentLabel string, at time.Time) {
	t.Helper()
	events := []map[string]any{{
		"event":      "labeled",
		"label":      map[string]string{"name": hiveLabel},
		"created_at": at.Format(time.RFC3339),
	}}
	if agentLabel != "" {
		events = append(events, map[string]any{
			"event":      "labeled",
			"label":      map[string]string{"name": agentLabel},
			"created_at": at.Add(time.Second).Format(time.RFC3339),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(events)
}

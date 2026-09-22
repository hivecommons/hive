package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunsDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "runs.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").Runs(context.Background())
	if err != nil {
		t.Fatalf("Runs() = %v", err)
	}
	if gotPath != "/api/runs" || gotMethod != http.MethodGet {
		t.Fatalf("request = %s %s, want GET /api/runs", gotMethod, gotPath)
	}
	if len(got) != 2 {
		t.Fatalf("Runs() returned %d rows, want 2", len(got))
	}
	if got[0].Key != "hivecommons/hive#8309" || got[0].WaitingOn != RunWaitingOnHuman || got[0].PlanEpicID != "epic-8309" {
		t.Errorf("first run = %+v", got[0])
	}
	if got[0].Stages[1].Receipt != "sha256:abc123" {
		t.Errorf("stage receipt = %q", got[0].Stages[1].Receipt)
	}
}

func TestRunFetchesEscapedDetail(t *testing.T) {
	var gotRawPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawPath = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"key":"hivecommons/hive#8309","waiting_on":"human","plan_epic_id":"epic-8309"}`)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "tok").Run(context.Background(), "hivecommons/hive#8309")
	if err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if gotRawPath != "/api/runs/hivecommons%2Fhive%238309" {
		t.Errorf("path = %q, want escaped run key", gotRawPath)
	}
	if got.PlanEpicID != "epic-8309" {
		t.Errorf("PlanEpicID = %q", got.PlanEpicID)
	}
}

func TestRunRequiresKey(t *testing.T) {
	if _, err := newTestClient(t, httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), "tok").Run(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "run key is required") {
		t.Fatalf("Run(empty) error = %v, want key error", err)
	}
}

func TestRoleAndRunActions(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.EscapedPath())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/role":
			_, _ = io.WriteString(w, `{"role":"owner","user":"operator"}`)
		case "/api/plan/epic-8309/approve":
			_, _ = io.WriteString(w, `{"ok":true,"status":"approved"}`)
		case "/api/plan/epic-8309/reject":
			_, _ = io.WriteString(w, `{"ok":true,"status":"draft"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	c := newTestClient(t, server, "tok")

	role, err := c.Role(context.Background())
	if err != nil || !role.Owner() {
		t.Fatalf("Role() = %+v, %v; want owner", role, err)
	}
	approved, err := c.ApproveRun(context.Background(), "epic-8309")
	if err != nil || approved.Status != "approved" {
		t.Fatalf("ApproveRun() = %+v, %v", approved, err)
	}
	rejected, err := c.RejectRun(context.Background(), "epic-8309")
	if err != nil || rejected.Status != "draft" {
		t.Fatalf("RejectRun() = %+v, %v", rejected, err)
	}
	want := []string{"GET /api/role", "POST /api/plan/epic-8309/approve", "POST /api/plan/epic-8309/reject"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestRunActionRequiresPlanIDAndReportsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"owner access required"}`)
	}))
	defer server.Close()
	c := newTestClient(t, server, "tok")

	if _, err := c.ApproveRun(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "plan epic id is required") {
		t.Fatalf("ApproveRun(empty) error = %v, want plan id error", err)
	}
	_, err := c.RejectRun(context.Background(), "epic")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Method != http.MethodPost {
		t.Fatalf("RejectRun forbidden error = %v, want POST 403 APIError", err)
	}
}

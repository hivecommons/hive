package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

func TestEnsureFleetReportCreatesWhenNoFingerprintMatch(t *testing.T) {
	var creates atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/labels":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "fleet-report"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/hivecommons/hive/issues/comments":
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues":
			creates.Add(1)
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "hive-fleet-fingerprint:fp") {
				t.Fatalf("create body missing fingerprint: %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 9, "html_url": "https://github.com/hivecommons/hive/issues/9", "user": map[string]any{"login": "bot"}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	res, err := c.EnsureFleetReport(context.Background(), fleetreport.Report{Fingerprint: "fp", Title: "Fleet report", Body: "<!-- hive-fleet-fingerprint:fp -->\nbody", Labels: []string{"fleet-report"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Created || res.Number != 9 || creates.Load() != 1 {
		t.Fatalf("result = %#v creates=%d", res, creates.Load())
	}
}

func TestEnsureFleetReportCommentsAndReactsOnFingerprintMatch(t *testing.T) {
	var comments, reactions atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/issues":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"number": 7, "html_url": "https://github.com/hivecommons/hive/issues/7"}}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/comments":
			comments.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/7/reactions":
			reactions.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 2})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	res, err := c.EnsureFleetReport(context.Background(), fleetreport.Report{Fingerprint: "fp", Body: "evidence"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Commented || !res.ReactionSent || comments.Load() != 1 || reactions.Load() != 1 {
		t.Fatalf("result=%#v comments=%d reactions=%d", res, comments.Load(), reactions.Load())
	}
}

func TestPostFleetReportRecoveryClosesOnlyWhenOwner(t *testing.T) {
	var patches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/hivecommons/hive/issues/5/comments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/hivecommons/hive/issues/5":
			patches.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 5})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "hivecommons", nil, slog.Default())
	if err := c.PostFleetReportRecovery(context.Background(), 5, fleetreport.Report{Body: "recovered"}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.PostFleetReportRecovery(context.Background(), 5, fleetreport.Report{Body: "recovered"}, true); err != nil {
		t.Fatal(err)
	}
	if patches.Load() != 1 {
		t.Fatalf("patches=%d, want 1", patches.Load())
	}
}

func TestEnsureFleetReportRejectsMissingFingerprint(t *testing.T) {
	c := NewClientForTest("http://127.0.0.1:1", "hivecommons", nil, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.EnsureFleetReport(ctx, fleetreport.Report{}); err == nil {
		t.Fatal("expected missing fingerprint error")
	}
}

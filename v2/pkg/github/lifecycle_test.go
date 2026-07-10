package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLifecycleIssueUpsertDeduplicatesByMarker(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: abc -->"
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			if created {
				_, _ = io.WriteString(writer, `[{"number":7,"html_url":"https://github.test/owner/repo/issues/7","body":"`+marker+`"}]`)
			} else {
				_, _ = io.WriteString(writer, `[]`)
			}
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/issues":
			created = true
			assertIssueRequest(t, request, "open")
			writer.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://github.test/owner/repo/issues/7"}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/issues/7":
			assertIssueRequest(t, request, "open")
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://github.test/owner/repo/issues/7"}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	firstNumber, _, firstCreated, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\nbody", []string{"hive/active"})
	if err != nil {
		t.Fatal(err)
	}
	secondNumber, _, secondCreated, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\nupdated", []string{"hive/active"})
	if err != nil {
		t.Fatal(err)
	}
	if !firstCreated || secondCreated || firstNumber != 7 || secondNumber != 7 {
		t.Fatalf("upsert did not deduplicate: first=%d/%t second=%d/%t", firstNumber, firstCreated, secondNumber, secondCreated)
	}
}

func TestLifecycleIssueUpsertAdoptsLegacyVisualHiveFingerprint(t *testing.T) {
	marker := "<!-- hive-visual-fingerprint: repository-hash -->"
	legacy := "<!-- visual-hive-issue dedupe:visual-hive:owner:repo:finding -->"
	created := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/issues":
			_, _ = io.WriteString(writer, `[{"number":11,"html_url":"https://github.test/owner/repo/issues/11","body":"`+legacy+`"}]`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/issues/11":
			assertIssueRequest(t, request, "open")
			_, _ = io.WriteString(writer, `{"number":11,"html_url":"https://github.test/owner/repo/issues/11"}`)
		case request.Method == http.MethodPost:
			created = true
			http.Error(writer, "must adopt", http.StatusInternalServerError)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())

	number, _, wasCreated, err := client.UpsertLifecycleIssue(context.Background(), "owner/repo", marker, "Finding", marker+"\n"+legacy+"\nbody", []string{"hive/active"})

	if err != nil || number != 11 || wasCreated || created {
		t.Fatalf("legacy issue was not adopted: number=%d created=%t post=%t err=%v", number, wasCreated, created, err)
	}
}

func TestUpdateLifecycleIssueClosesExplicitly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPatch || request.URL.Path != "/repos/owner/repo/issues/9" {
			http.Error(writer, "unexpected request", http.StatusNotFound)
			return
		}
		assertIssueRequest(t, request, "closed")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"number":9,"html_url":"https://github.test/owner/repo/issues/9"}`)
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	number, _, err := client.UpdateLifecycleIssue(context.Background(), "owner/repo", 9, "Resolved", "evidence", "closed", []string{"hive/resolved"})
	if err != nil || number != 9 {
		t.Fatalf("close failed: issue=%d err=%v", number, err)
	}
}

func assertIssueRequest(t *testing.T, request *http.Request, expectedState string) {
	t.Helper()
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		State  string   `json:"state"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.State != expectedState || strings.TrimSpace(body.Title) == "" || len(body.Labels) == 0 {
		t.Fatalf("unexpected issue request: %+v", body)
	}
}

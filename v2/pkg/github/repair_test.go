package github

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpsertRepairPullRequestCreatesThenUpdates(t *testing.T) {
	marker := "<!-- hive-repair: owner/repo:fingerprint -->"
	listCalls, createCalls, editCalls := 0, 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			listCalls++
			if createCalls == 0 {
				_, _ = io.WriteString(writer, `[]`)
			} else {
				_, _ = io.WriteString(writer, fmt.Sprintf(`[{"number":7,"html_url":"https://example.test/pull/7","body":%q,"head":{"sha":"abc","ref":"hive/repair"},"base":{"ref":"main"}}]`, marker))
			}
		case request.Method == http.MethodPost && request.URL.Path == "/repos/owner/repo/pulls":
			createCalls++
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://example.test/pull/7","head":{"sha":"abc"}}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/repos/owner/repo/pulls/7":
			editCalls++
			_, _ = io.WriteString(writer, `{"number":7,"html_url":"https://example.test/pull/7","head":{"sha":"def"}}`)
		default:
			http.Error(writer, request.Method+" "+request.URL.Path, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	first, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair", "main", "Fix", marker+"\nRefs #3", marker)
	if err != nil || !first.Created || first.Number != 7 {
		t.Fatalf("first upsert = %+v, %v", first, err)
	}
	second, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "hive/repair", "main", "Fix again", marker, marker)
	if err != nil || second.Created || second.HeadSHA != "def" {
		t.Fatalf("second upsert = %+v, %v", second, err)
	}
	if listCalls != 2 || createCalls != 1 || editCalls != 1 {
		t.Fatalf("unexpected calls list=%d create=%d edit=%d", listCalls, createCalls, editCalls)
	}
}

func TestUpsertRepairPullRequestRejectsDuplicateMarker(t *testing.T) {
	marker := "<!-- hive-repair: duplicate -->"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, fmt.Sprintf(`[{"number":1,"body":%q},{"number":2,"body":%q}]`, marker, marker))
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, err := client.UpsertRepairPullRequest(context.Background(), "owner/repo", "branch", "main", "title", marker, marker)
	if err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("expected duplicate rejection, got %v", err)
	}
}

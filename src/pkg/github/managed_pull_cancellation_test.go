package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestManagedPullCancellationFailsClosedOnAmbiguousPreparedMarker(t *testing.T) {
	const marker = "<!-- hive-uninstall: owner/repo:0123456789abcdef0123456789abcdef -->"
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo":
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 123, "full_name": "owner/repo", "default_branch": "main"})
		case request.Method == http.MethodGet && request.URL.Path == "/repos/owner/repo/pulls":
			repository := map[string]any{"id": 123, "full_name": "owner/repo"}
			pulls := []any{}
			for number, head := range []string{strings.Repeat("a", 40), strings.Repeat("b", 40)} {
				pulls = append(pulls, map[string]any{
					"number": number + 1, "html_url": "https://example.test/owner/repo/pull/" + strconv.Itoa(number+1),
					"state": "closed", "merged": false, "body": marker,
					"head": map[string]any{"ref": "hive/uninstall-123", "sha": head, "repo": repository},
					"base": map[string]any{"ref": "main", "sha": strings.Repeat("c", 40), "repo": repository},
				})
			}
			_ = json.NewEncoder(writer).Encode(pulls)
		case request.Method == http.MethodPatch:
			patches++
			http.Error(writer, `{"message":"unexpected mutation"}`, http.StatusInternalServerError)
		default:
			http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClientForTest(server.URL, "owner", []string{"repo"}, slog.Default())
	_, _, err := client.CloseManagedPullRequestForCancellation(context.Background(), "owner/repo", "123", 0, "", marker, "hive/uninstall-123", "main")
	if err == nil || !strings.Contains(err.Error(), "multiple pull requests") {
		t.Fatalf("ambiguous cancellation marker was accepted: %v", err)
	}
	if patches != 0 {
		t.Fatalf("ambiguity triggered %d pull request mutation(s)", patches)
	}
}

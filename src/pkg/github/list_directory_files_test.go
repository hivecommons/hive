package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestListDirectoryFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/contents/.hive/agents"):
			if got := r.URL.Query().Get("ref"); got != "" && got != "main" {
				t.Errorf("ref query = %q, want empty or main", got)
			}
			_, _ = w.Write([]byte(`[
				{"type":"file","name":"foo.yaml","path":".hive/agents/foo.yaml"},
				{"type":"dir","name":"nested","path":".hive/agents/nested"},
				{"type":"symlink","name":"link.yaml","path":".hive/agents/link.yaml"},
				{"type":"file","name":"foo.md","path":".hive/agents/foo.md"}
			]`))
		case strings.HasSuffix(r.URL.Path, "/contents/README.md"):
			_, _ = w.Write([]byte(`{"type":"file","name":"README.md","path":"README.md"}`))
		case strings.HasSuffix(r.URL.Path, "/contents/missing"):
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hive"})
	ctx := context.Background()

	got, err := c.ListDirectoryFiles(ctx, "hivecommons", "hive", ".hive/agents", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if want := []string{"foo.yaml", "foo.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("files = %v, want %v (dirs and symlinks skipped)", got, want)
	}
	if _, err := c.ListDirectoryFiles(ctx, "hivecommons", "hive", ".hive/agents", "main"); err != nil {
		t.Errorf("list at ref: %v", err)
	}

	got, err = c.ListDirectoryFiles(ctx, "hivecommons", "hive", "missing", "")
	if err != nil || got != nil {
		t.Errorf("404: files=%v err=%v, want nil,nil", got, err)
	}
	if _, err := c.ListDirectoryFiles(ctx, "hivecommons", "hive", "README.md", ""); err == nil {
		t.Error("file path: want error, got nil")
	}
	if _, err := c.ListDirectoryFiles(ctx, "hivecommons", "hive", "boom", ""); err == nil {
		t.Error("500: want error, got nil")
	}
	var nilClient *Client
	if _, err := nilClient.ListDirectoryFiles(ctx, "o", "r", "p", ""); err != ErrNoGitHubClient {
		t.Errorf("nil client err = %v, want ErrNoGitHubClient", err)
	}
}

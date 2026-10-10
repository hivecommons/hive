package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKnowledgeSuggestWritesEntryAndPRBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("suggest must not call the dashboard: %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	root := t.TempDir()
	prBody := filepath.Join(root, "pr.md")

	stdout, _, err := execute(t, server, "Retry relay uploads with backoff.\n",
		"--output", "json", "knowledge", "suggest",
		"--repo-root", root, "--stdin", "--title", "Retry relay uploads",
		"--type", "gotcha", "--repo", "acme/app", "--layer", "project", "--tags", "relay,ops",
		"--source", "https://github.com/acme/app/pull/12", "--reason", "learned while fixing #12",
		"--pr-body-file", prBody)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Entry   string   `json:"entry"`
		Files   []string `json:"files"`
		Branch  string   `json:"branch"`
		PRTitle string   `json:"pr_title"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout = %q: %v", stdout, err)
	}
	if result.Entry != "retry-relay-uploads" || len(result.Files) != 1 || result.Branch != "knowledge/add-retry-relay-uploads" {
		t.Fatalf("result = %+v", result)
	}
	entry, err := os.ReadFile(filepath.Join(root, ".hive", "wiki", "retry-relay-uploads.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"status: approved", "source: https://github.com/acme/app/pull/12", "tags: relay, ops", "suggested_at: ", "Retry relay uploads with backoff."} {
		if !strings.Contains(string(entry), want) {
			t.Errorf("entry missing %q:\n%s", want, entry)
		}
	}
	body, err := os.ReadFile(prBody)
	if err != nil || !strings.Contains(string(body), "Merging this PR approves it") {
		t.Fatalf("pr body = %q, %v", body, err)
	}
}

func TestKnowledgeSuggestBodyFileAndDeprecate(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	root := t.TempDir()
	note := filepath.Join(root, "note.md")
	if err := os.WriteFile(note, []byte("Deploy with make deploy."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, server, "", "knowledge", "suggest", "--repo-root", root,
		"--title", "Deploy runbook", "--body-file", note, "--source", "acme/app#3", "--reason", "new runbook"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := execute(t, server, "", "knowledge", "suggest", "--repo-root", root,
		"--action", "deprecate", "--target", "deploy-runbook", "--source", "acme/app#4", "--reason", "replaced by CD"); err != nil {
		t.Fatal(err)
	}
	entry, err := os.ReadFile(filepath.Join(root, ".hive", "wiki", "deploy-runbook.md"))
	if err != nil || !strings.Contains(string(entry), "status: deprecated") || !strings.Contains(string(entry), "Deploy with make deploy.") {
		t.Fatalf("entry = %q, %v", entry, err)
	}
}

func TestKnowledgeSuggestUsageErrors(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	root := t.TempDir()
	for name, args := range map[string][]string{
		"two body sources": {"--body", "x", "--stdin", "--title", "t", "--source", "#1", "--reason", "r"},
		"bad action":       {"--action", "drop", "--title", "t", "--body", "x", "--source", "#1", "--reason", "r"},
		"missing source":   {"--title", "t", "--body", "x", "--reason", "r"},
		"missing reason":   {"--title", "t", "--body", "x", "--source", "#1"},
		"bad status":       {"--title", "t", "--body", "x", "--source", "#1", "--reason", "r", "--status", "live"},
	} {
		_, _, err := execute(t, server, "", append([]string{"knowledge", "suggest", "--repo-root", root}, args...)...)
		if err == nil || ExitCode(err) != ExitUsage {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	_, _, err := execute(t, server, "", "knowledge", "suggest", "--repo-root", root,
		"--action", "update", "--target", "missing", "--title", "t", "--body", "x", "--source", "#1", "--reason", "r")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing target err = %v", err)
	}
	_, _, err = execute(t, server, "", "knowledge", "suggest", "--repo-root", root,
		"--body-file", filepath.Join(root, "nope.md"), "--title", "t", "--source", "#1", "--reason", "r")
	if err == nil {
		t.Fatal("missing body file accepted")
	}
}

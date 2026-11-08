package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCreateTypeArg(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--title", "x", "--type", "knowledge"}, "knowledge"},
		{[]string{"-type=knowledge", "--title", "x"}, "knowledge"},
		{[]string{"--type", "task", "--type=knowledge"}, "knowledge"},
		{[]string{"--type=knowledge", "--type", "bug"}, "bug"},
		{[]string{"--title", "x"}, ""},
		{[]string{"--", "--type", "knowledge"}, ""},
	}
	for _, tc := range cases {
		if got := createTypeArg(tc.args); got != tc.want {
			t.Errorf("createTypeArg(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestParseKnowledgeCreate(t *testing.T) {
	bodyFile := filepath.Join(t.TempDir(), "entry.md")
	if err := os.WriteFile(bodyFile, []byte("from a file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseKnowledgeCreate([]string{
		"--type", "knowledge", "--title", "T", "--body-file", bodyFile,
		"--state", "Draft", "--tag", "cli-memory", "--tag", "agent:scanner", "--layer", "drafts",
	}, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	want := knowledgeCreateRequest{Title: "T", Body: "from a file\n", Tags: []string{"cli-memory", "agent:scanner"}, Layer: "drafts", State: "draft"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	if got, err := parseKnowledgeCreate([]string{"--title", "T", "--body-file", "-"}, strings.NewReader("stdin body")); err != nil || got.Body != "stdin body" {
		t.Fatalf("stdin body: %+v %v", got, err)
	}

	for _, args := range [][]string{
		{"--body", "b"},  // no title
		{"--title", "T"}, // no body
		{"--title", "T", "--body", "b", "--body-file", bodyFile}, // both bodies
		{"--title", "T", "--body", "   "},                        // blank body
		{"--title", "T", "--body", "b", "extra"},                 // stray argument
		{"--title", "T", "--body", "b", "--priority", "1"},       // bead-only flag
	} {
		if _, err := parseKnowledgeCreate(args, strings.NewReader("")); err == nil {
			t.Errorf("parseKnowledgeCreate(%q) accepted", args)
		}
	}
}

// bd create --type knowledge posts to the dashboard, not the bead store.
func TestCmdCreateKnowledgePostsToDashboard(t *testing.T) {
	t.Setenv("BD_DIR", t.TempDir())
	var got map[string]any
	kbTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/knowledge/create" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		jsonResponse(t, w, map[string]any{"ok": true, "title": "T", "layer": "drafts", "state": "draft"})
	})
	cmdCreate([]string{"--type", "knowledge", "--title", "T", "--body", "b", "--state", "draft", "--tag", "cli-memory", "--layer", "drafts"})
	want := map[string]any{"title": "T", "body": "b", "state": "draft", "tags": []any{"cli-memory"}, "layer": "drafts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("payload = %v, want %v", got, want)
	}
	if entries, _ := os.ReadDir(os.Getenv("BD_DIR")); len(entries) != 0 {
		t.Fatalf("knowledge create touched the bead store: %d entries", len(entries))
	}
}

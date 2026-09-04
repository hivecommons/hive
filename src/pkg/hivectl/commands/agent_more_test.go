package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentGetFetchesConfig covers agentGetCommand's RunE success path.
func TestAgentGetFetchesConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/agent/quality" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"quality"}`)
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "json", "agent", "get", "quality")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"name": "quality"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestAgentSpecsCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go-testing.md"), []byte(`---
name: go-testing
version: 1.0.0
description: Go testing
tags: [go]
---
Use t.TempDir.`), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "json", "agent", "specs", "--dir", dir, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "go-testing") {
		t.Fatalf("list stdout = %q", stdout)
	}
	stdout, _, err = execute(t, server, "", "--output", "json", "agent", "specs", "--dir", dir, "search", "go", "--limit", "5")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "go-testing") {
		t.Fatalf("search stdout = %q", stdout)
	}
	_, _, err = execute(t, server, "", "agent", "specs", "--dir", dir, "search", "go", "--limit", "-1")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("negative limit err = %v, want usage", err)
	}
}

// TestAgentImportFromFile covers the readInput file path used in the
// import command (not stdin), plus the "url" is empty branch.
func TestAgentImportFromFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(filePath, []byte("kind: AgentDefinition\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["source"] != "paste" || !strings.Contains(body["content"].(string), "AgentDefinition") {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "import", "--file", filePath); err != nil {
		t.Fatal(err)
	}
}

// TestAgentImportFromURL covers the --url branch of agentImportCommand.
func TestAgentImportFromURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["source"] != "url" || body["url"] != "https://example.com/agent.yaml" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "import", "--url", "https://example.com/agent.yaml"); err != nil {
		t.Fatal(err)
	}
}

// TestAgentImportURLCombinedWithFileRejected covers the usage-error branch
// when --url is combined with --file.
func TestAgentImportURLCombinedWithFileRejected(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "agent", "import", "--url", "https://example.com/x.yaml", "--file", "whatever.yaml")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v, want usage error", err)
	}
}

// TestAgentExportUnexpectedResponseShape covers the "unexpected agent
// export response" and "did not include YAML" error branches.
func TestAgentExportUnexpectedResponseShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `["not", "a", "map"]`)
	}))
	defer server.Close()

	_, _, err := execute(t, server, "", "agent", "export", "quality")
	if err == nil || !strings.Contains(err.Error(), "unexpected agent export response") {
		t.Fatalf("err = %v", err)
	}
}

// TestAgentExportMissingYAMLField covers the branch where the response map
// lacks a usable "yaml" string field.
func TestAgentExportMissingYAMLField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"name":"quality"}`)
	}))
	defer server.Close()

	_, _, err := execute(t, server, "", "agent", "export", "quality")
	if err == nil || !strings.Contains(err.Error(), "did not include YAML") {
		t.Fatalf("err = %v", err)
	}
}

// TestAgentExportJSONOutputDecodesYAML covers the branch of agentExportCommand
// where output format is not table/yaml, so the YAML is decoded into a
// structured value and re-printed.
func TestAgentExportJSONOutputDecodesYAML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"name": "quality",
			"yaml": "kind: AgentDefinition\nmetadata:\n  name: quality\n",
		})
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "json", "agent", "export", "quality")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"kind": "AgentDefinition"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

// TestAgentKickWithPromptFlag covers the direct --prompt branch of
// agentKickCommand (no file/stdin read).
func TestAgentKickWithPromptFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["prompt"] != "hello there" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "kick", "quality", "--prompt", "hello there"); err != nil {
		t.Fatal(err)
	}
}

// TestAgentKickPromptCombinedWithStdinRejected covers the usage-error
// branch when --prompt is combined with --stdin.
func TestAgentKickPromptCombinedWithStdinRejected(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "extra", "agent", "kick", "quality", "--prompt", "hi", "--stdin")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v, want usage error", err)
	}
}

// TestAgentKickWithPromptFile covers the --prompt-file branch of
// agentKickCommand.
func TestAgentKickWithPromptFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(filePath, []byte("scan now"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["prompt"] != "scan now" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "kick", "quality", "--prompt-file", filePath); err != nil {
		t.Fatal(err)
	}
}

// TestAgentKickNoPromptSendsEmptyString covers agentKickCommand when
// no --prompt/--prompt-file/--stdin is given at all.
func TestAgentKickNoPromptSendsEmptyString(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["prompt"] != "" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "kick", "quality"); err != nil {
		t.Fatal(err)
	}
}

// TestAgentLogsSuccess covers agentLogsCommand's happy path including the
// --source=buffer branch.
func TestAgentLogsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pane/quality" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("lines") != "200" || r.URL.Query().Get("source") != "buffer" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"lines":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "logs", "quality", "--lines", "200", "--source", "buffer"); err != nil {
		t.Fatal(err)
	}
}

// TestAgentLogsRejectsOutOfRangeLines covers the --lines validation branch.
func TestAgentLogsRejectsOutOfRangeLines(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	for _, lines := range []string{"0", "1001"} {
		_, _, err := execute(t, server, "", "agent", "logs", "quality", "--lines", lines)
		if err == nil || ExitCode(err) != ExitUsage {
			t.Fatalf("lines=%s err = %v, want usage error", lines, err)
		}
	}
}

// TestAgentLogsRejectsInvalidSource covers the --source validation branch.
func TestAgentLogsRejectsInvalidSource(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "agent", "logs", "quality", "--source", "bogus")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v, want usage error", err)
	}
}

// TestAgentBackendSetSendsBackend covers agentBackendSetCommand.
func TestAgentBackendSetSendsBackend(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/agent/quality/models" || r.Method != http.MethodPut {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["backend"] != "claude" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"updated"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "backend-set", "quality", "claude"); err != nil {
		t.Fatal(err)
	}
}

// TestAgentDeleteSuccess covers agentDeleteCommand's success path when --yes
// is supplied.
func TestAgentDeleteSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents/quality" || r.Method != http.MethodDelete {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"deleted"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "delete", "quality", "--yes"); err != nil {
		t.Fatal(err)
	}
}

// TestAgentPauseResumeRestart covers the generated pause/resume/restart
// subcommands.
func TestAgentPauseResumeRestart(t *testing.T) {
	for _, action := range []string{"pause", "resume", "restart"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/"+action+"/quality" || r.Method != http.MethodPost {
				t.Fatalf("action %s: request = %s %s", action, r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"ok"}`)
		}))
		if _, _, err := execute(t, server, "", "agent", action, "quality"); err != nil {
			t.Fatalf("action %s: %v", action, err)
		}
		server.Close()
	}
}

// TestAgentListSuccess covers the "agent list" RunE.
func TestAgentListSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "list"); err != nil {
		t.Fatal(err)
	}
}

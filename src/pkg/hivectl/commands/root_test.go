package commands

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func execute(t *testing.T, server *httptest.Server, input string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := NewRootCommand(strings.NewReader(input), &stdout, &stderr)
	command.SetArgs(append([]string{"--server", server.URL}, args...))
	err := command.Execute()
	return stdout.String(), stderr.String(), err
}

func TestSystemStatusJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"IDLE","agents":2}`)
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "json", "system", "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"mode": "IDLE"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestAgentImportPreviewFromStdin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents/import" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("missing authorization: %q", r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["source"] != "paste" || body["preview"] != true || !strings.Contains(body["content"].(string), "AgentDefinition") {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"parsed":{"kind":"AgentDefinition"}}`)
	}))
	defer server.Close()
	t.Setenv("HIVE_DASHBOARD_TOKEN", "test-token")

	input := "apiVersion: hive.kubestellar.io/v1\nkind: AgentDefinition\nmetadata:\n  name: ux-discovery\n"
	stdout, _, err := execute(t, server, input, "--output", "json", "agent", "import", "--stdin", "--preview")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"ok": true`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestAgentExportExtractsPortableYAML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/agent/ux-discovery/export" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"name": "ux-discovery",
			"yaml": "apiVersion: hive.kubestellar.io/v1\nkind: AgentDefinition\nmetadata:\n  name: ux-discovery\n",
		})
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "yaml", "agent", "export", "ux-discovery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "kind: AgentDefinition") || strings.Contains(stdout, "yaml: |") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestDestructiveCommandsRequireYesWithoutCallingAPI(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()

	for _, args := range [][]string{
		{"agent", "delete", "ux-discovery"},
		{"bead", "reset"},
		{"knowledge", "delete", "project", "journey"},
		{"governor", "agent-remove", "ux-discovery"},
	} {
		_, _, err := execute(t, server, "", args...)
		if err == nil || ExitCode(err) != ExitUsage {
			t.Fatalf("args %v error = %v, exit = %d", args, err, ExitCode(err))
		}
	}
	if calls != 0 {
		t.Fatalf("API calls = %d, want 0", calls)
	}
}

func TestKnowledgeSearchBuildsQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/knowledge/search" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("q") != "user journey" || query.Get("type") != "pattern" || query.Get("limit") != "7" {
			t.Fatalf("query = %v", query)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"count":0,"results":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "knowledge", "search", "user journey", "--type", "pattern", "--limit", "7"); err != nil {
		t.Fatal(err)
	}
}

func TestGovernorThresholdsAcceptsYAMLStdin(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/governor/thresholds" || r.Method != http.MethodPut {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]int
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["surge"] != 20 || body["busy"] != 10 {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"status":"updated"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "surge: 20\nbusy: 10\n", "governor", "thresholds-set", "--stdin"); err != nil {
		t.Fatal(err)
	}
}

func TestBeadCreateSendsExistingAPIShape(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/beads/quality" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["title"] != "Review UX" || body["external_ref"] != "ux://review" || body["priority"] != float64(1) {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"created","id":"b1"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "Review UX", "--priority", "1", "--external-ref", "ux://review"); err != nil {
		t.Fatal(err)
	}
}

func TestExitCodeMappings(t *testing.T) {
	for _, test := range []struct {
		status int
		exit   int
	}{
		{status: http.StatusUnauthorized, exit: ExitAuth},
		{status: http.StatusForbidden, exit: ExitAuth},
		{status: http.StatusNotFound, exit: ExitAPI},
		{status: http.StatusConflict, exit: ExitAPI},
		{status: http.StatusInternalServerError, exit: ExitAPI},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(test.status)
			io.WriteString(w, `{"error":"request failed"}`)
		}))
		_, _, err := execute(t, server, "", "system", "status")
		server.Close()
		if ExitCode(err) != test.exit {
			t.Fatalf("status %d error = %v, exit = %d", test.status, err, ExitCode(err))
		}
	}
}

func TestSystemEventsRequiresExplicitFollowAndJSONL(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "system", "events")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("missing --follow error = %v", err)
	}
	_, _, err = execute(t, server, "", "system", "events", "--follow")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("missing JSONL error = %v", err)
	}
}

func TestStructuredInputRejectsEmptyAndInvalidData(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "governor", "thresholds-set", "--stdin")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("empty stdin error = %v", err)
	}
	_, _, err = execute(t, server, ":\n  - [", "governor", "thresholds-set", "--stdin")
	if err == nil {
		t.Fatal("expected malformed input error")
	}
	_, _, err = execute(t, server, strings.Repeat("x", maxAPIInputBytes+1), "agent", "import", "--stdin")
	if err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("oversized input error = %v", err)
	}
}

func TestAgentPromptGetSendsRawQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/agent/quality/prompt" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("raw") != "1" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"agent":"quality","prompt":"# Policy"}`)
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "--output", "json", "agent", "prompt", "get", "quality", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"prompt": "# Policy"`) {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestAgentPromptSetSendsTemplate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/agent/quality/prompt" || r.Method != http.MethodPut {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["template"] != "# New policy\n" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"updated"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "# New policy\n", "agent", "prompt", "set", "quality", "--stdin"); err != nil {
		t.Fatal(err)
	}
}

func TestAgentModelSetSendsConfigModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/agent/quality/models" || r.Method != http.MethodPut {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "claude-sonnet-4-6" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"updated"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "agent", "model-set", "quality", "claude-sonnet-4-6"); err != nil {
		t.Fatal(err)
	}
}

func TestAgentPipelineSetRejectsNonBoolean(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()

	_, _, err := execute(t, server, "lint: yes\nbuild: 3\n", "agent", "pipeline-set", "quality", "--stdin")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("non-boolean pipeline error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("API calls = %d, want 0", calls)
	}
}

func TestObserveTrendsBuildsRangeQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/trends" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("range") != "week" {
			t.Fatalf("query = %v", r.URL.Query())
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"evals":[],"tokenHistory":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "observe", "trends", "--range", "week"); err != nil {
		t.Fatal(err)
	}
}

func TestObserveTrendsRejectsRangeAndHoursTogether(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "observe", "trends", "--range", "week", "--hours", "12")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("combined range/hours error = %v", err)
	}
}

func TestObserveTokensHitsExpectedPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tokens" || r.Method != http.MethodGet {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"total_tokens":0,"sessions":[]}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "observe", "tokens"); err != nil {
		t.Fatal(err)
	}
}

func TestJSONInflatedBodyRejectedBeforeSend(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()

	// Raw input is under 1 MiB, but each quote becomes \" (two bytes) once the
	// content is embedded in the JSON request body, pushing the encoded body past
	// the server's 1 MiB limit. The client must catch this before sending.
	raw := strings.Repeat("\"", 600*1024)
	if len(raw) >= maxAPIInputBytes {
		t.Fatalf("test input %d not below the raw cap %d", len(raw), maxAPIInputBytes)
	}
	_, _, err := execute(t, server, raw, "agent", "import", "--stdin", "--preview")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("inflated body error = %v, exit = %d", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("error should mention the limit: %v", err)
	}
	if calls != 0 {
		t.Fatalf("API calls = %d, want 0 (rejected before send)", calls)
	}
}

func TestBeadResetRejectsPositionalArg(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()

	// A stray positional arg must not be silently ignored into a global reset.
	_, _, err := execute(t, server, "", "bead", "reset", "scanner", "--yes")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("bead reset positional error = %v, exit = %d", err, ExitCode(err))
	}
	if calls != 0 {
		t.Fatalf("API calls = %d, want 0", calls)
	}
}

func TestKnowledgeListRejectsReservedLayer(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls++
	}))
	defer server.Close()

	_, _, err := execute(t, server, "", "knowledge", "list", "--layer", "search")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("reserved layer error = %v, exit = %d", err, ExitCode(err))
	}
	if calls != 0 {
		t.Fatalf("API calls = %d, want 0", calls)
	}
}

func TestObserveTrendsRejectsOutOfRangeHours(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "observe", "trends", "--hours", "999")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("out-of-range hours error = %v, exit = %d", err, ExitCode(err))
	}
}

func TestInvalidServerURLMapsToUsageExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := NewRootCommand(strings.NewReader(""), &stdout, &stderr)
	command.SetArgs([]string{"--server", "not-a-url", "system", "status"})
	err := command.Execute()
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("invalid server error = %v, exit = %d", err, ExitCode(err))
	}
}

func TestUnknownFlagMapsToUsageExit(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "system", "status", "--bogus")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("unknown flag error = %v, exit = %d", err, ExitCode(err))
	}
}

func TestCommandTreeHasExamples(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	var stdout, stderr bytes.Buffer
	root := NewRootCommand(strings.NewReader(""), &stdout, &stderr)
	for _, command := range root.Commands() {
		if command.Short == "" {
			t.Errorf("%s missing short help", command.CommandPath())
		}
		if command.Name() != "completion" && command.Name() != "help" && command.Example == "" {
			t.Errorf("%s missing examples", command.CommandPath())
		}
	}
	root.SetArgs([]string{"agent", "import", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Examples:") || !strings.Contains(stdout.String(), "--preview") {
		t.Fatalf("help = %q", stdout.String())
	}
}

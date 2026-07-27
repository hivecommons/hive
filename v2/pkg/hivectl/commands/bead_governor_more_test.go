package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBeadListWithAgentFilter covers beadListCommand's --agent branch.
func TestBeadListWithAgentFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/beads/quality" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "bead", "list", "--agent", "quality"); err != nil {
		t.Fatal(err)
	}
}

// TestBeadListWithoutAgentFilter covers beadListCommand's no-filter branch.
func TestBeadListWithoutAgentFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/beads" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `[]`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "bead", "list"); err != nil {
		t.Fatal(err)
	}
}

// TestBeadCreateRequiresAgentAndTitle covers the missing --agent/--title
// validation branch.
func TestBeadCreateRequiresAgentAndTitle(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "bead", "create", "--title", "x")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("missing agent err = %v", err)
	}
	_, _, err = execute(t, server, "", "bead", "create", "--agent", "quality")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("missing title err = %v", err)
	}
}

// TestBeadCreateRejectsOutOfRangePriority covers the --priority bounds check.
func TestBeadCreateRejectsOutOfRangePriority(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--priority", "-1")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("negative priority err = %v", err)
	}
	_, _, err = execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--priority", "5")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("too-large priority err = %v", err)
	}
}

// TestBeadCreateRejectsInvalidType covers the --type validation branch.
func TestBeadCreateRejectsInvalidType(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--type", "bogus")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v", err)
	}
}

// TestBeadCreateRejectsTooManyMetadata covers the metadata count limit.
func TestBeadCreateRejectsTooManyMetadata(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	args := []string{"bead", "create", "--agent", "quality", "--title", "x"}
	for i := 0; i < 51; i++ {
		args = append(args, "--metadata", "k=v")
	}
	_, _, err := execute(t, server, "", args...)
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("err = %v", err)
	}
}

// TestBeadCreateRejectsMalformedMetadata covers the "key=value" parsing
// validation branch (missing "=" and empty key).
func TestBeadCreateRejectsMalformedMetadata(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	_, _, err := execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--metadata", "novalue")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("missing '=' err = %v", err)
	}
	_, _, err = execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--metadata", "=v")
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("empty key err = %v", err)
	}
}

// TestBeadCreateRejectsOversizedMetadata covers the key/value length limits.
func TestBeadCreateRejectsOversizedMetadata(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	bigKey := strings.Repeat("k", 101) + "=v"
	_, _, err := execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--metadata", bigKey)
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("oversized key err = %v", err)
	}
	bigValue := "k=" + strings.Repeat("v", 1001)
	_, _, err = execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--metadata", bigValue)
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("oversized value err = %v", err)
	}
}

// TestBeadCreateWithValidMetadata covers the successful metadata parsing
// path with a well-formed key=value pair.
func TestBeadCreateWithValidMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		metadata, ok := body["metadata"].(map[string]any)
		if !ok || metadata["env"] != "prod" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"created"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "bead", "create", "--agent", "quality", "--title", "x", "--metadata", "env=prod"); err != nil {
		t.Fatal(err)
	}
}

// TestBeadResetGlobalWithoutAgent covers beadResetCommand's global (no
// --agent) path and default example text branch.
func TestBeadResetGlobalWithoutAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/beads/reset" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"reset"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "bead", "reset", "--yes"); err != nil {
		t.Fatal(err)
	}
}

// TestBeadResetWithAgent covers beadResetCommand's --agent-scoped path.
func TestBeadResetWithAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/beads/reset/quality" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"reset"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "bead", "reset", "--agent", "quality", "--yes"); err != nil {
		t.Fatal(err)
	}
}

// TestGovernorGetSuccess covers the "governor get" inline RunE.
func TestGovernorGetSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/governor" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"mode":"auto"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "governor", "get"); err != nil {
		t.Fatal(err)
	}
}

// TestGovernorBudgetSetAndReposSet cover the other governorFileCommand
// instantiations besides thresholds-set.
func TestGovernorBudgetSetAndReposSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
	}{
		{name: "budget-set", path: "/api/config/governor/budget"},
		{name: "repos-set", path: "/api/config/governor/repos"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != tc.path || r.Method != http.MethodPut {
				t.Fatalf("%s: request = %s %s", tc.name, r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"status":"updated"}`)
		}))
		if _, _, err := execute(t, server, `{"limit":10}`, "governor", tc.name, "--stdin"); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		server.Close()
	}
}

// TestGovernorAgentAddSuccess covers governorAgentAddCommand.
func TestGovernorAgentAddSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/governor/agents" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "quality" || body["backend"] != "copilot" || body["model"] != "claude-sonnet-4-6" {
			t.Fatalf("body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"added"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "governor", "agent-add", "quality", "--backend", "copilot", "--model", "claude-sonnet-4-6"); err != nil {
		t.Fatal(err)
	}
}

// TestGovernorAgentRemoveSuccess covers governorAgentRemoveCommand's
// success path when --yes is supplied.
func TestGovernorAgentRemoveSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config/governor/agents/quality" || r.Method != http.MethodDelete {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"removed"}`)
	}))
	defer server.Close()

	if _, _, err := execute(t, server, "", "governor", "agent-remove", "quality", "--yes"); err != nil {
		t.Fatal(err)
	}
}

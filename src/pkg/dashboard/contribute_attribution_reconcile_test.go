package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestWSMessage_ReasoningEffortJSON(t *testing.T) {
	raw := `{"type":"auth_response","cli_backend":"codex","model":"gpt-5.6-terra","reasoning_effort":"high"}`
	var msg WSMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal WSMessage failed: %v", err)
	}
	if msg.CLIBackend != "codex" || msg.Model != "gpt-5.6-terra" || msg.ReasoningEffort != "high" {
		t.Errorf("unmarshaled msg = %+v, want backend=codex, model=gpt-5.6-terra, effort=high", msg)
	}

	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal WSMessage failed: %v", err)
	}
	if !strings.Contains(string(encoded), `"reasoning_effort":"high"`) {
		t.Errorf("marshaled msg missing reasoning_effort: %s", string(encoded))
	}
}

func TestContributeWSHub_ReconcilePRAttribution(t *testing.T) {
	var mu sync.Mutex
	var patchedBody string
	var patchCount int

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls/42", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   42,
				"html_url": "https://github.com/myorg/repo1/pull/42",
				"body":     "Initial PR description\nFixes #5",
				"user":     map[string]any{"login": "alice"},
				"base": map[string]any{
					"repo": map[string]any{
						"name":      "repo1",
						"full_name": "myorg/repo1",
						"owner":     map[string]any{"login": "myorg"},
					},
				},
			})
		case "PATCH", "POST":
			raw, _ := io.ReadAll(r.Body)
			var patch map[string]any
			_ = json.Unmarshal(raw, &patch)
			mu.Lock()
			if b, ok := patch["body"].(string); ok {
				patchedBody = b
			}
			patchCount++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   42,
				"html_url": "https://github.com/myorg/repo1/pull/42",
				"body":     patchedBody,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)
	hub := NewContributeWSHub(logger, s)

	conn := &ContributorConnection{
		cliBackend:      "codex",
		model:           "gpt-5.6-terra",
		reasoningEffort: "high",
		role:            "quality",
		profile: &ContributorProfile{
			GitHubUsername: "alice",
		},
		capabilities: &ContributorCapabilities{
			AgentCLIVersion: "0.5.2",
		},
	}

	prURL := "https://github.com/myorg/repo1/pull/42"
	hub.reconcilePRAttribution(prURL, conn)

	mu.Lock()
	count := patchCount
	gotBody := patchedBody
	mu.Unlock()

	if count != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", count)
	}

	wantTrailer := "— hive: agent=quality backend=codex model=gpt-5.6-terra effort=high codex=0.5.2"
	if !strings.Contains(gotBody, wantTrailer) {
		t.Errorf("reconciled PR body missing expected trailer: got %q, want trailer %q", gotBody, wantTrailer)
	}
	if !strings.Contains(gotBody, "Initial PR description\nFixes #5") {
		t.Errorf("reconciled PR body dropped initial content: got %q", gotBody)
	}
}

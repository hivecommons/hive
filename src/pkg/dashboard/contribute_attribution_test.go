package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ghpkg "github.com/kubestellar/hive/pkg/github"
)

// #4083/#4084 shared prerequisite: the relay has been sending reasoning_effort
// on auth_response all along — the hub's message struct must not silently drop
// it at JSON decode any more.
func TestWSMessage_DecodesReasoningEffort(t *testing.T) {
	raw := `{"type":"auth_response","cli_backend":"codex","model":"gpt-5.6-terra","reasoning_effort":"high"}`
	var msg WSMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if msg.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort = %q, want %q (field silently dropped?)", msg.ReasoningEffort, "high")
	}
}

// #4084: effort threads through addActivity to the stored/served entries, and
// entries persisted before the field simply carry none.
func TestAddActivity_CarriesEffort(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := NewContributeWSHub(logger, nil)
	hub.addActivity("alice", "picked up", "scanner", "codex", "gpt-5.6-terra", "high", "issue o/r#1: t")
	hub.addActivity("bob", "joined", "", "bob", "", "", "")

	entries := hub.RecentActivity()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Effort != "high" || entries[0].CLI != "codex" || entries[0].Model != "gpt-5.6-terra" {
		t.Errorf("entry 0 loadout = (%q, %q, %q)", entries[0].CLI, entries[0].Model, entries[0].Effort)
	}
	if entries[1].Effort != "" {
		t.Errorf("no-effort entry must omit effort, got %q", entries[1].Effort)
	}
	// The JSON the feed consumes omits an unknown effort rather than sending "".
	b, err := json.Marshal(entries[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "effort") {
		t.Errorf("empty effort must be omitted from JSON, got %s", b)
	}
}

// #4084: the fleet view surfaces the handshake-declared effort read-only.
func TestFleetSnapshot_CarriesEffort(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := NewContributeWSHub(logger, nil)
	hub.connections["c1"] = &ContributorConnection{
		profile:         &ContributorProfile{ContributorID: "c1", GitHubUsername: "alice"},
		cliBackend:      "codex",
		model:           "gpt-5.6-terra",
		reasoningEffort: "high",
	}
	snap := hub.FleetSnapshot()
	if len(snap.Clankers) != 1 {
		t.Fatalf("expected 1 clanker, got %d", len(snap.Clankers))
	}
	if snap.Clankers[0].Effort != "high" {
		t.Errorf("FleetClanker.Effort = %q, want %q", snap.Clankers[0].Effort, "high")
	}
}

// #4085: the assignment prompt ships the LITERAL footer line, interpolated from
// the hub's handshake record, degrading to no instruction at all when the hub
// knows nothing to state.
func TestAttributionPromptInstruction(t *testing.T) {
	got := attributionPromptInstruction("codex", "gpt-5.6-terra", "high")
	if !strings.Contains(got, "'— hive: backend=codex model=gpt-5.6-terra effort=high'") {
		t.Errorf("instruction missing literal trailer: %q", got)
	}
	// No effort → the line simply has no effort pair.
	got = attributionPromptInstruction("codex", "gpt-5.6-terra", "")
	if strings.Contains(got, "effort") {
		t.Errorf("no-effort instruction must omit effort: %q", got)
	}
	// bob self-selects: model is honestly "auto" (RequestedModel).
	got = attributionPromptInstruction("bob", "", "")
	if !strings.Contains(got, "backend=bob model=auto") {
		t.Errorf("bob instruction = %q", got)
	}
	// Nothing known → no instruction, never a dangling sentence.
	if got := attributionPromptInstruction("", "", ""); got != "" {
		t.Errorf("unknown loadout must produce no instruction, got %q", got)
	}
}

// #4085 verify-don't-trust: on a verified completion the hub appends the
// missing trailer to the PR with its own handshake record of the connection,
// and never stacks a second one on a body that already carries it.
func TestReconcilePRAttribution_AppendsMissingTrailer(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	prBody := "Fixes #3\n\nAgent-written body."
	var patched string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			patched = req.Body
			prBody = req.Body
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":   7,
			"html_url": "https://github.com/myorg/repo1/pull/7",
			"body":     prBody,
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghpkg.NewClientForTest(ts.URL, "myorg", []string{"repo1"}, logger)
	s.RegisterAPI(deps)
	hub := NewContributeWSHub(logger, s)

	hub.reconcilePRAttribution("https://github.com/myorg/repo1/pull/7", "alice", "codex", "gpt-5.6-terra", "high")
	want := "— hive: backend=codex model=gpt-5.6-terra effort=high"
	if !strings.HasSuffix(patched, want) {
		t.Fatalf("PATCHed body = %q, want suffix %q", patched, want)
	}

	// Second reconciliation (retry / duplicate completion): exactly one line.
	patched = ""
	hub.reconcilePRAttribution("https://github.com/myorg/repo1/pull/7", "alice", "codex", "gpt-5.6-terra", "high")
	if patched != "" {
		t.Errorf("second reconciliation must be a no-op, PATCHed %q", patched)
	}
	if strings.Count(prBody, ghpkg.AttributionTrailerPrefix) != 1 {
		t.Errorf("body must carry exactly one trailer, got %d in %q",
			strings.Count(prBody, ghpkg.AttributionTrailerPrefix), prBody)
	}
}

// Degradation: no GitHub client / empty URL must be silent no-ops (the read
// loop calls this fire-and-forget on every verified completion).
func TestReconcilePRAttribution_DegradesQuietly(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	hub := NewContributeWSHub(logger, nil)
	hub.reconcilePRAttribution("", "alice", "codex", "m", "high")
	hub.reconcilePRAttribution("https://github.com/o/r/pull/1", "alice", "codex", "m", "high")
}

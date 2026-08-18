package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reconcileTestClient serves GET /repos/myorg/repo1/pulls/7 with the given
// body and records any PATCH (Edit) it receives. Returns the client and a
// pointer to the last PATCHed body ("" until an edit happens).
func reconcileTestClient(t *testing.T, prBody string) (*Client, *string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	patched := new(string)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/myorg/repo1/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			*patched = req.Body
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
	return NewClientForTest(ts.URL, "myorg", []string{"repo1"}, logger), patched
}

const reconcileURL = "https://github.com/myorg/repo1/pull/7"

// #4085 happy path: a PR whose body carries no trailer (a local-mode relay or
// MCP-opened PR) is amended with the hub-recorded backend/model/effort.
func TestEnsurePRAttribution_AppendsWhenMissing(t *testing.T) {
	c, patched := reconcileTestClient(t, "Fixes #12\n\nSome agent-written body.")
	m := InvocationMeta{Backend: "codex", Model: "gpt-5.6-terra", Effort: "high"}

	updated, err := c.EnsurePRAttribution(context.Background(), reconcileURL, m)
	if err != nil {
		t.Fatalf("EnsurePRAttribution: %v", err)
	}
	if !updated {
		t.Fatal("expected the PR body to be updated")
	}
	want := "Fixes #12\n\nSome agent-written body.\n\n— hive: backend=codex model=gpt-5.6-terra effort=high"
	if *patched != want {
		t.Errorf("patched body = %q, want %q", *patched, want)
	}
}

// A body that already carries the trailer — written by the agent following its
// prompt instruction, stamped at creation, or reconciled once already — is left
// untouched: exactly one trailer, never two (idempotence, #4085).
func TestEnsurePRAttribution_NoOpWhenPresent(t *testing.T) {
	body := "Body.\n\n" + AttributionTrailerPrefix + " backend=codex model=gpt-5.6-terra effort=high"
	c, patched := reconcileTestClient(t, body)
	m := InvocationMeta{Backend: "codex", Model: "gpt-5.6-terra", Effort: "high"}

	updated, err := c.EnsurePRAttribution(context.Background(), reconcileURL, m)
	if err != nil {
		t.Fatalf("EnsurePRAttribution: %v", err)
	}
	if updated || *patched != "" {
		t.Errorf("must not stack a second trailer: updated=%v patched=%q", updated, *patched)
	}
}

// All-unknown metadata has nothing honest to stamp: no lookup result matters,
// no edit is made.
func TestEnsurePRAttribution_EmptyMetaNoOp(t *testing.T) {
	c, patched := reconcileTestClient(t, "Body.")
	updated, err := c.EnsurePRAttribution(context.Background(), reconcileURL, InvocationMeta{})
	if err != nil {
		t.Fatalf("EnsurePRAttribution: %v", err)
	}
	if updated || *patched != "" {
		t.Errorf("empty meta must be a no-op: updated=%v patched=%q", updated, *patched)
	}
}

// The visible reconciliation honours the same governor.attribution_trailer
// toggle as the creation-time trailer.
func TestEnsurePRAttribution_RespectsTrailerToggle(t *testing.T) {
	c, patched := reconcileTestClient(t, "Body.")
	c.SetAttributionHooks(AttributionHooks{TrailerEnabled: func() bool { return false }})
	m := InvocationMeta{Backend: "codex", Model: "gpt-5.6-terra"}

	updated, err := c.EnsurePRAttribution(context.Background(), reconcileURL, m)
	if err != nil {
		t.Fatalf("EnsurePRAttribution: %v", err)
	}
	if updated || *patched != "" {
		t.Errorf("toggle off must suppress the edit: updated=%v patched=%q", updated, *patched)
	}
}

// A malformed URL and a nil client both fail cleanly — the caller logs and
// moves on; completion handling must never be affected.
func TestEnsurePRAttribution_ErrorPaths(t *testing.T) {
	c, _ := reconcileTestClient(t, "Body.")
	if _, err := c.EnsurePRAttribution(context.Background(), "not-a-pr-url", InvocationMeta{Backend: "codex"}); err == nil {
		t.Error("expected an error for an unparseable PR URL")
	}
	var nilClient *Client
	if _, err := nilClient.EnsurePRAttribution(context.Background(), reconcileURL, InvocationMeta{Backend: "codex"}); err == nil {
		t.Error("expected ErrNoGitHubClient for a nil client")
	}
}

// A successful reconciliation records the audit entry with the artifact
// coordinates and the hub-recorded loadout.
func TestEnsurePRAttribution_Audits(t *testing.T) {
	c, _ := reconcileTestClient(t, "Body.")
	var gotAction, gotDetail string
	c.SetAttributionAudit(func(action, detail, agent string) {
		gotAction, gotDetail = action, detail
	})
	m := InvocationMeta{Backend: "codex", Model: "gpt-5.6-terra", Effort: "high"}
	if _, err := c.EnsurePRAttribution(context.Background(), reconcileURL, m); err != nil {
		t.Fatalf("EnsurePRAttribution: %v", err)
	}
	if gotAction != AuditActionPRAttributionReconciled {
		t.Errorf("audit action = %q, want %q", gotAction, AuditActionPRAttributionReconciled)
	}
	for _, part := range []string{"repo=myorg/repo1", "number=7", "url=" + reconcileURL, "effort=high"} {
		if !strings.Contains(gotDetail, part) {
			t.Errorf("audit detail %q missing %q", gotDetail, part)
		}
	}
}

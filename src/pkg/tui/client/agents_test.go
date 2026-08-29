package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAgentsDecodesFixture decodes the full testdata/agents.json fixture and
// asserts every field on every entry, including the zero-value cases: an
// agent whose config has no display name (handler falls back to Name but
// still marshals `omitempty`, so the wire can legitimately omit it) and an
// agent that is disabled.
func TestAgentsDecodesFixture(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "agents.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents() = %v, want nil", err)
	}

	if gotPath != "/api/agents" {
		t.Errorf("path = %q, want /api/agents", gotPath)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}

	want := []Agent{
		{
			Name:        "scanner",
			ID:          "agt_scanner01",
			DisplayName: "Scanner",
			Enabled:     true,
			Managed:     false,
			Backend:     "claude",
			Model:       "claude-opus-4-5",
		},
		{
			Name:        "quality",
			ID:          "agt_quality01",
			DisplayName: "", // omitted on the wire; handler's Name fallback is a caller concern, not a decode concern
			Enabled:     true,
			Managed:     true,
			Backend:     "copilot",
			Model:       "gpt-5",
		},
		{
			Name:        "reviewer",
			ID:          "agt_reviewer01",
			DisplayName: "Reviewer",
			Enabled:     false,
			Managed:     false,
			Backend:     "claude",
			Model:       "claude-sonnet-4-5",
		},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d agents, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("agent[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAgentsEmptyList covers the zero-agents case: a valid empty JSON array,
// not a nil/error.
func TestAgentsEmptyList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Agents(context.Background())
	if err != nil {
		t.Fatalf("Agents() = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("Agents() = %+v, want empty", got)
	}
}

// TestAgentsMalformedBody: a 200 carrying something that is not a JSON array
// of agents is a decode error, not a silent empty/zero result.
func TestAgentsMalformedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html>not json</html>`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Agents(context.Background())
	if err == nil {
		t.Fatal("Agents() = nil error on a non-JSON 200, want a decode error")
	}
	if got != nil {
		t.Errorf("Agents() = %+v, want nil on error", got)
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Fatalf("error = %v, want it to name the decode failure", err)
	}
}

// TestAgentsNonOKReturnsAPIError checks the typed-error contract on a failed
// request, matching TestHealthNonOKReturnsAPIError's coverage for this
// endpoint.
func TestAgentsNonOKReturnsAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer server.Close()

	got, err := newTestClient(t, server, "t").Agents(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Agents() error = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusInternalServerError)
	}
	if apiErr.Path != "/api/agents" {
		t.Errorf("Path = %q, want /api/agents", apiErr.Path)
	}
	if got != nil {
		t.Errorf("Agents() = %+v, want nil on error", got)
	}
}

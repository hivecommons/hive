package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
)

// A name that needs percent-encoding reached the hive double-encoded over
// stdio, so agent.model with a model id such as "kiro-api-key/claude-opus-5:high"
// asked the hive for the model "kiro-api-key%2Fclaude-opus-5:high".
func TestStdioWriteSendsSlashBearingModelAsNamed(t *testing.T) {
	const model = "kiro-api-key/claude-opus-5:high"
	var gotAgent, gotModel string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/model/{agent}/{model}", func(w http.ResponseWriter, r *http.Request) {
		gotAgent, gotModel = r.PathValue("agent"), r.PathValue("model")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	p := readProvider{roster: r, writesEnabled: true, pendingStore: adminmcp.NewMemoryPendingStore()}

	previewed, err := p.callWrite(context.Background(), adminmcp.ToolWritePreview, map[string]any{"operation": adminmcp.WriteOpAgentModel, "args": map[string]any{"agent": "scanner", "model": model}})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := previewed.(map[string]any)["confirmation_id"].(string)
	if _, err := p.callWrite(context.Background(), adminmcp.ToolWriteConfirm, map[string]any{"confirmation_id": id}); err != nil {
		t.Fatal(err)
	}
	if gotAgent != "scanner" || gotModel != model {
		t.Fatalf("hive was asked to set agent %q to model %q, want scanner and %q", gotAgent, gotModel, model)
	}
}

// The same for a read: the agent in the request path is the agent named.
func TestStdioReadSendsEscapedAgentNameOnce(t *testing.T) {
	const agent = "team/scanner"
	var gotAgent string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/kick/{agent}/status", func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.PathValue("agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"delivered"}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	if _, err := (readProvider{roster: r}).Read(context.Background(), adminmcp.ToolAgentNudgeStatus, map[string]any{"agent": agent}); err != nil {
		t.Fatal(err)
	}
	if gotAgent != agent {
		t.Fatalf("hive was asked about agent %q, want %q", gotAgent, agent)
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSameToolAnswerMatchesHTTPAndStdioWrapper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r}

	stdioResult, err := provider.handler(adminmcp.ToolHiveStatus)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stdioResult.Content) != 1 {
		t.Fatalf("stdio content = %#v", stdioResult.Content)
	}
	stdioText := mustMarshalContentText(t, stdioResult.Content[0])

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`))
	adminmcp.NewHandler(provider).ServeHTTP(rec, req)
	httpText := adminResultText(t, rec.Body.String())
	if stdioText != httpText {
		t.Fatalf("stdio %s != http %s", stdioText, httpText)
	}
}

func mustMarshalContentText(t *testing.T, c mcp.Content) string {
	t.Helper()
	text, ok := c.(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %#v", c)
	}
	return text.Text
}

func adminResultText(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %#v body=%s", resp.Error, body)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("content = %#v", resp.Result.Content)
	}
	return resp.Result.Content[0].Text
}

func TestReadProviderCapsActiveHiveList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"name":"a"},{"name":"b"}]}`))
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	data, err := (readProvider{roster: r}).Read(context.Background(), adminmcp.ToolAgentsList, map[string]any{"limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("data = %#v", data)
	}
	agents, ok := m["agents"].([]any)
	if !ok || len(agents) != 1 || m["agents_truncated"] != true {
		t.Fatalf("capped data = %#v", data)
	}
}

func TestRosterActiveHiveOnly(t *testing.T) {
	var hitsA, hitsB int
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA++
		if got := r.Header.Get("Authorization"); got != "Bearer token-a" {
			t.Fatalf("auth A = %q", got)
		}
		for _, forbidden := range []string{"X-Hive-User", "X-Hive-Role", "X-Hive-Owner-Role-Verified"} {
			if r.Header.Get(forbidden) != "" {
				t.Fatalf("sent forbidden header %s", forbidden)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hive":"a"}`))
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hive":"b"}`))
	}))
	defer serverB.Close()
	r := &roster{hives: []hiveConfig{{Name: "a", Address: serverA.URL, Token: "token-a"}, {Name: "b", Address: serverB.URL, Token: "token-b"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r}
	data, err := provider.Read(context.Background(), adminmcp.ToolHiveStatus, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := data.(map[string]any)
	if !ok || m["hive"] != "a" {
		t.Fatalf("data = %#v", data)
	}
	if hitsA != 1 || hitsB != 0 {
		t.Fatalf("hits A=%d B=%d, want A only", hitsA, hitsB)
	}
}

func TestSelectHiveDiagnosesWrongToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "wrong", Address: server.URL, Token: "bad"}}, timeout: time.Second}
	_, err := r.selectHive(context.Background(), "wrong")
	if err == nil || !strings.Contains(err.Error(), "wrong dashboard token") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagnoseSelectionErrorDirectRoute(t *testing.T) {
	err := diagnoseSelectionError(&hivectl.APIError{StatusCode: http.StatusUnauthorized, Message: "direct-route shared token refused"})
	if err == nil || !strings.Contains(err.Error(), "direct-route spoke") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagnoseSelectionErrorUnreachable(t *testing.T) {
	err := diagnoseSelectionError(&hivectl.ConnectionError{Err: errors.New("dial failed")})
	if err == nil || !strings.Contains(err.Error(), "unreachable host") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRosterFromEnvSelectsConfiguredActiveHive(t *testing.T) {
	t.Setenv(envHives, `[{"name":"east","address":"http://east.example","token":"east-token"},{"name":"west","address":"http://west.example","token":"west-token"}]`)
	t.Setenv(envActiveHive, "west")

	r, err := loadRosterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if r.active != 1 || len(r.hives) != 2 {
		t.Fatalf("roster active=%d hives=%#v", r.active, r.hives)
	}
	if r.timeout != defaultTimeout {
		t.Fatalf("timeout = %v", r.timeout)
	}
}

func TestLoadRosterFromEnvRejectsInvalidRosters(t *testing.T) {
	tests := []struct {
		name       string
		hives      string
		activeHive string
		want       string
	}{
		{name: "missing", want: envHives + " must contain"},
		{name: "bad json", hives: `{`, want: "parse " + envHives},
		{name: "empty", hives: `[]`, want: envHives + " must contain at least one hive"},
		{name: "blank field", hives: `[{"name":"east","address":"http://east.example","token":""}]`, want: "require name, address, and token"},
		{name: "duplicate", hives: `[{"name":"east","address":"http://east.example","token":"one"},{"name":"east","address":"http://other.example","token":"two"}]`, want: `duplicate hive name "east"`},
		{name: "unknown active", hives: `[{"name":"east","address":"http://east.example","token":"one"}]`, activeHive: "west", want: `active hive "west" is not in the roster`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envHives, tt.hives)
			t.Setenv(envActiveHive, tt.activeHive)
			_, err := loadRosterFromEnv()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

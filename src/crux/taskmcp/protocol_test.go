package taskmcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rpcCall(t *testing.T, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	NewHandler(nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body.String())
	}
	return resp
}

// MCP's ping is a liveness check the receiver must answer with an empty
// result. The task MCP answered "method not found", so an agent's client that
// pings to see whether the server is still there was told it is not.
func TestTaskMCPAnswersPing(t *testing.T) {
	resp := rpcCall(t, `{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	if resp["error"] != nil {
		t.Fatalf("ping answered with an error: %v", resp["error"])
	}
	if result, ok := resp["result"].(map[string]any); !ok || len(result) != 0 {
		t.Fatalf("ping result = %#v, want an empty object", resp["result"])
	}
	if resp["id"] != float64(7) {
		t.Fatalf("ping answer id = %v, want 7", resp["id"])
	}
}

// A notification expects no answer. The endpoint always writes one, so every
// notification gets the empty acknowledgement notifications/initialized has
// always had, not "method not found".
func TestTaskMCPAcknowledgesEveryNotification(t *testing.T) {
	for _, method := range []string{"notifications/initialized", "notifications/cancelled", "notifications/progress", "notifications/roots/list_changed"} {
		resp := rpcCall(t, `{"jsonrpc":"2.0","method":"`+method+`","params":{"requestId":3,"reason":"timeout"}}`)
		if resp["error"] != nil {
			t.Errorf("%s answered with an error: %v", method, resp["error"])
		}
		if result, ok := resp["result"].(map[string]any); !ok || len(result) != 0 {
			t.Errorf("%s result = %#v, want an empty object", method, resp["result"])
		}
	}
}

func TestTaskMCPStillRefusesUnknownMethods(t *testing.T) {
	for _, method := range []string{"resources/list", "prompts/list", "notifications", "tools/unknown", ""} {
		resp := rpcCall(t, `{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`)
		rpcErr, _ := resp["error"].(map[string]any)
		if rpcErr["code"] != float64(-32601) {
			t.Errorf("%q answered %v, want error -32601", method, resp)
		}
	}
}

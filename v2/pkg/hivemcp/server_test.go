package hivemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestServerInitializeListAndStructuredCall(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hive_status","arguments":{"state_dir":"/tmp/hive"}}}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	err := Serve(context.Background(), strings.NewReader(input), &output, func(_ context.Context, name string, arguments map[string]any) (any, error) {
		return map[string]any{"tool": name, "state_dir": arguments["state_dir"]}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var initialize, list, call map[string]any
	for _, target := range []*map[string]any{&initialize, &list, &call} {
		if err := decoder.Decode(target); err != nil {
			t.Fatal(err)
		}
	}
	if initialize["result"].(map[string]any)["protocolVersion"] != ProtocolVersion {
		t.Fatalf("bad initialize response: %+v", initialize)
	}
	tools := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 12 {
		t.Fatalf("expected production tool set, got %d", len(tools))
	}
	result := call["result"].(map[string]any)
	if result["isError"] != false || result["structuredContent"].(map[string]any)["tool"] != "hive_status" {
		t.Fatalf("bad tool call response: %+v", call)
	}
}

func TestServerReportsToolExecutionErrorsInsideResult(t *testing.T) {
	input := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_run","arguments":{}}}` + "\n"
	var output bytes.Buffer
	_ = Serve(context.Background(), strings.NewReader(input), &output, func(context.Context, string, map[string]any) (any, error) {
		return nil, context.DeadlineExceeded
	})
	if !strings.Contains(output.String(), `"isError":true`) || !strings.Contains(output.String(), "deadline exceeded") {
		t.Fatalf("expected tool execution error result: %s", output.String())
	}
}

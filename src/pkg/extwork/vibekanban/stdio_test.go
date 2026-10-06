package vibekanban

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSplitCommand(t *testing.T) {
	got, err := splitCommand(`cmd "two words" 'three words'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"cmd", "two words", "three words"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("split = %v, want %v", got, want)
	}
	if _, err := splitCommand(`cmd "unterminated`); err == nil {
		t.Fatal("unterminated quote succeeded")
	}
}

func TestDecodeToolResult(t *testing.T) {
	raw := []byte(`{"content":[{"type":"text","text":"{\"issue_id\":\"card-1\"}"}]}`)
	got, err := decodeToolResult("create_issue", raw)
	if err != nil {
		t.Fatal(err)
	}
	if got["issue_id"] != "card-1" {
		t.Fatalf("decoded = %v", got)
	}
	if _, err := decodeToolResult("bad", []byte(`{"content":[{"type":"text","text":"boom"}],"isError":true}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("tool error = %v", err)
	}
}

func TestStdioClientCallTool(t *testing.T) {
	t.Setenv("VK_HELPER_PROCESS", "1")
	client := NewStdioClient(fmt.Sprintf("%q -test.run=TestMCPHelperProcess --", os.Args[0]))
	client.timeout = 5 * time.Second
	got, err := client.CallTool(context.Background(), "create_issue", map[string]any{"title": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got["issue_id"] != "card-from-helper" {
		t.Fatalf("CallTool = %v", got)
	}
}

func TestStdioClientToolError(t *testing.T) {
	t.Setenv("VK_HELPER_PROCESS", "1")
	client := NewStdioClient(fmt.Sprintf("%q -test.run=TestMCPHelperProcess --", os.Args[0]))
	client.timeout = 5 * time.Second
	_, err := client.CallTool(context.Background(), "fail_tool", nil)
	if err == nil || !strings.Contains(err.Error(), "helper failure") {
		t.Fatalf("CallTool error = %v", err)
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("VK_HELPER_PROCESS") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var msg struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil || msg.ID == 0 {
			continue
		}
		switch msg.Method {
		case "initialize":
			helperSend(msg.ID, map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "helper", "version": "1"}}, nil)
		case "tools/call":
			if msg.Params.Name == "fail_tool" {
				helperSend(msg.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": `{"error":"helper failure"}`}}, "isError": true}, nil)
			} else {
				helperSend(msg.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": `{"issue_id":"card-from-helper"}`}}}, nil)
			}
		default:
			helperSend(msg.ID, nil, map[string]any{"code": -32601, "message": "unknown"})
		}
	}
	os.Exit(0)
}

func helperSend(id int, result any, rpcErr any) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	raw, _ := json.Marshal(msg)
	fmt.Println(string(raw))
}

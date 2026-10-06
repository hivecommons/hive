package vibekanban

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
	if os.Getenv("VK_HELPER_MODE") == "init-error" {
		fmt.Fprintln(os.Stderr, "helper stderr")
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
			if os.Getenv("VK_HELPER_MODE") == "init-error" {
				helperSend(msg.ID, nil, map[string]any{"code": -32000, "message": "init failed"})
			} else {
				helperSend(msg.ID, map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "helper", "version": "1"}}, nil)
			}
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

func TestStdioClientSetupErrors(t *testing.T) {
	client := NewStdioClient("unterminated '")
	if _, err := client.CallTool(context.Background(), "x", nil); err == nil {
		t.Fatal("split error succeeded")
	}
	client = NewStdioClient("")
	if _, err := client.CallTool(context.Background(), "x", nil); err == nil {
		t.Fatal("empty command succeeded")
	}
	client = NewStdioClient("/definitely/missing/vibe-kanban-helper")
	if _, err := client.CallTool(context.Background(), "x", nil); err == nil {
		t.Fatal("missing command succeeded")
	}
}

func TestStdioClientRPCErrorAndStderr(t *testing.T) {
	t.Setenv("VK_HELPER_PROCESS", "1")
	t.Setenv("VK_HELPER_MODE", "init-error")
	client := NewStdioClient(fmt.Sprintf("%q -test.run=TestMCPHelperProcess --", os.Args[0]))
	client.timeout = 5 * time.Second
	if _, err := client.CallTool(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "helper stderr") {
		t.Fatalf("init error = %v", err)
	}
	if err := withStderr(context.Canceled, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("withStderr empty = %v", err)
	}
}

func TestRPCSessionEdges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &rpcSession{stdin: &strings.Builder{}, reader: bufio.NewReader(strings.NewReader("")), pending: map[int]chan rpcReply{}, nextID: 1}
	if _, err := s.request(ctx, "never", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("request canceled = %v", err)
	}
	badWrite := &failingWriter{}
	s = &rpcSession{stdin: badWrite, reader: bufio.NewReader(strings.NewReader("")), pending: map[int]chan rpcReply{}, nextID: 1}
	if _, err := s.request(context.Background(), "write", nil); err == nil {
		t.Fatal("write failure succeeded")
	}
	var wrote strings.Builder
	input := strings.Join([]string{
		`not-json`,
		`{"jsonrpc":"2.0","method":"ping","id":7}`,
		`{"jsonrpc":"2.0","method":"other","id":8}`,
		`{"jsonrpc":"2.0","method":"notice"}`,
		`{"jsonrpc":"2.0","id":99,"result":{}}`,
		"",
	}, "\n")
	s = &rpcSession{stdin: &wrote, reader: bufio.NewReader(strings.NewReader(input)), pending: map[int]chan rpcReply{99: make(chan rpcReply, 1)}}
	s.readLoop()
	out := wrote.String()
	if !strings.Contains(out, `"id":7`) || !strings.Contains(out, `"id":8`) || !strings.Contains(out, "method not supported") {
		t.Fatalf("readLoop replies = %s", out)
	}
}

func TestDecodeToolResultEdges(t *testing.T) {
	if _, err := decodeToolResult("bad-json", []byte("{")); err == nil {
		t.Fatal("bad json decoded")
	}
	got, err := decodeToolResult("empty", []byte(`{"content":[{"type":"text","text":"not json"}]}`))
	if err != nil || len(got) != 0 {
		t.Fatalf("plain text result = %v %v", got, err)
	}
	got, err = decodeToolResult("structured", []byte(`{"structuredContent":{"ok":"yes"}}`))
	if err != nil || got["ok"] != "yes" {
		t.Fatalf("structured result = %v %v", got, err)
	}
}

type failingWriter struct{}

func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

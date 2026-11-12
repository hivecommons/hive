package connector

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMCPServer answers JSON-RPC over pipes like a stdio MCP server. tools
// maps a tool name to a handler returning (text, isError).
type fakeMCPServer struct {
	tools   map[string]func(args map[string]any) (string, bool)
	mu      sync.Mutex
	calls   []string
	initReq map[string]any
}

func (f *fakeMCPServer) serve(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), mcpMaxLineBytes)
	for sc.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
				Proto     string         `json:"protocolVersion"`
			} `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		var result any
		var rpcErr map[string]any
		switch req.Method {
		case "initialize":
			f.mu.Lock()
			f.initReq = map[string]any{"protocolVersion": req.Params.Proto}
			f.mu.Unlock()
			// Interleave a logging notification to prove the client skips it.
			fmt.Fprintf(w, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"booting https://plaud.example/notice"}}`+"\n")
			result = map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}}
		case "tools/call":
			f.mu.Lock()
			f.calls = append(f.calls, req.Params.Name)
			h := f.tools[req.Params.Name]
			f.mu.Unlock()
			if h == nil {
				rpcErr = map[string]any{"code": -32601, "message": "unknown tool " + req.Params.Name}
				break
			}
			text, isErr := h(req.Params.Arguments)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": isErr}
		default:
			rpcErr = map[string]any{"code": -32601, "message": "method not found"}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID)}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		b, _ := json.Marshal(resp)
		fmt.Fprintf(w, "%s\n", b)
	}
}

func (f *fakeMCPServer) callNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// pipeDialer connects an mcpSession to srv over in-memory pipes.
func pipeDialer(srv *fakeMCPServer) mcpDialer {
	return func(ctx context.Context) (mcpClient, error) {
		cr, sw := io.Pipe() // server → client
		sr, cw := io.Pipe() // client → server
		go srv.serve(sr, sw)
		closer := func() error { _ = cw.Close(); _ = sw.Close(); return nil }
		return newMCPSession(ctx, cr, cw, nil, closer)
	}
}

func TestMCPSessionHandshakeCallAndErrors(t *testing.T) {
	srv := &fakeMCPServer{tools: map[string]func(map[string]any) (string, bool){
		"echo": func(a map[string]any) (string, bool) { return fmt.Sprint(a["x"]), false },
		"boom": func(map[string]any) (string, bool) { return "nope: not authenticated", true },
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pipeDialer(srv)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, err := c.Call(ctx, "echo", map[string]any{"x": "hi"}); err != nil || got != "hi" {
		t.Fatalf("echo = %q, %v", got, err)
	}
	if _, err := c.Call(ctx, "boom", nil); err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("isError must surface as error, got %v", err)
	}
	if _, err := c.Call(ctx, "missing", nil); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("rpc error = %v", err)
	}
	if !strings.Contains(c.Notices(), "plaud.example/notice") {
		t.Fatalf("notification not recorded: %q", c.Notices())
	}
	if srv.initReq["protocolVersion"] != mcpProtocolVersion {
		t.Fatalf("initialize params = %v", srv.initReq)
	}
}

func TestMCPSessionServerExit(t *testing.T) {
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	go func() { _, _ = io.Copy(io.Discard, sr) }()
	go func() {
		// Reply to initialize, then hang up before any tool call is answered.
		fmt.Fprintln(sw, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		_ = sw.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := newMCPSession(ctx, cr, cw, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Call(ctx, "x", nil); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("want closed-connection error, got %v", err)
	}
}

func TestSplitCommandLine(t *testing.T) {
	got, err := splitCommandLine(`npx -y "@plaud-ai/mcp@latest" --flag 'a b'`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"npx", "-y", "@plaud-ai/mcp@latest", "--flag", "a b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("split = %q", got)
	}
	if _, err := splitCommandLine(`npx "unterminated`); err == nil {
		t.Fatal("unterminated quote must fail")
	}
}

func TestStdioMCPDialerSpawnsProcess(t *testing.T) {
	// A tiny shell MCP server: answers initialize and one tools/call.
	dir := t.TempDir()
	script := filepath.Join(dir, "server.sh")
	body := "read line\necho '{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}'\nread line\nread line\necho '{\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"pong\"}]}}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	dial := stdioMCPDialer("sh "+script, filepath.Join(dir, "home"))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	if got, err := c.Call(ctx, "ping", nil); err != nil || got != "pong" {
		t.Fatalf("ping = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "home")); err != nil {
		t.Fatalf("HOME dir must be created: %v", err)
	}
}

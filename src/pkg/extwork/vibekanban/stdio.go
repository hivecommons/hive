package vibekanban

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const mcpProtocolVersion = "2024-11-05"

// StdioClient starts the configured MCP command for each tool call. It avoids a
// long-lived child in the adapter so retries after a crashed MCP process are clean.
type StdioClient struct {
	command   string
	timeout   time.Duration
	splitFunc func(string) ([]string, error)
}

// NewStdioClient returns a ToolClient backed by a local MCP stdio command.
func NewStdioClient(command string) *StdioClient {
	return &StdioClient{command: command, timeout: 60 * time.Second, splitFunc: splitCommand}
}

// CallTool implements ToolClient.
func (c *StdioClient) CallTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	argv, err := c.splitFunc(c.command)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 {
		return nil, errors.New("MCP command is empty")
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr lockedBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	client := rpcSession{stdin: stdin, reader: bufio.NewReader(stdout), pending: map[int]chan rpcReply{}, nextID: 1}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		client.readLoop()
	}()
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
		wg.Wait()
	}()
	if _, err := client.request(ctx, "initialize", map[string]any{"protocolVersion": mcpProtocolVersion, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "hive-vibe-kanban-extwork", "version": "1"}}); err != nil {
		return nil, withStderr(err, stderr.String())
	}
	_ = client.notify("notifications/initialized", nil)
	res, err := client.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, withStderr(err, stderr.String())
	}
	return decodeToolResult(name, res)
}

type rpcSession struct {
	stdin   io.Writer
	reader  *bufio.Reader
	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcReply
}

type rpcReply struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (s *rpcSession) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	ch := make(chan rpcReply, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case reply := <-ch:
		if reply.Error != nil {
			return nil, errors.New(reply.Error.Message)
		}
		return reply.Result, nil
	}
}

func (s *rpcSession) notify(method string, params any) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (s *rpcSession) write(msg any) error {
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.stdin, "%s\n", raw)
	return err
}

func (s *rpcSession) readLoop() {
	for {
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var msg struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
			continue
		}
		if msg.Method != "" {
			if msg.ID != nil {
				if msg.Method == "ping" {
					_ = s.write(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "result": map[string]any{}})
				} else {
					_ = s.write(map[string]any{"jsonrpc": "2.0", "id": *msg.ID, "error": map[string]any{"code": -32601, "message": "method not supported: " + msg.Method}})
				}
			}
			continue
		}
		if msg.ID == nil {
			continue
		}
		s.mu.Lock()
		ch := s.pending[*msg.ID]
		delete(s.pending, *msg.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- rpcReply{Result: msg.Result, Error: msg.Error}
		}
	}
}

func decodeToolResult(name string, raw json.RawMessage) (map[string]any, error) {
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	out := res.StructuredContent
	var text string
	if out == nil {
		for _, c := range res.Content {
			if c.Type == "text" {
				if text != "" {
					text += "\n"
				}
				text += c.Text
			}
		}
		_ = json.Unmarshal([]byte(text), &out)
	}
	if res.IsError {
		if out != nil && out["error"] != nil {
			return nil, fmt.Errorf("%s: %v", name, out["error"])
		}
		return nil, fmt.Errorf("%s: %s", name, text)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func splitCommand(command string) ([]string, error) {
	var words []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range command {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in MCP command")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

func withStderr(err error, stderr string) error {
	if tail := strings.TrimSpace(stderr); tail != "" {
		return fmt.Errorf("%w: %s", err, tail)
	}
	return err
}

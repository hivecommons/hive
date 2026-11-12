package connector

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// mcpProtocolVersion is the MCP revision hive speaks as a stdio client.
const mcpProtocolVersion = "2024-11-05"

// mcpMaxLineBytes caps one JSON-RPC line read from an MCP server.
const mcpMaxLineBytes = 32 << 20

// mcpClient is one connected MCP session. Call invokes a tool and returns
// the concatenated text content of the result.
type mcpClient interface {
	Call(ctx context.Context, tool string, args map[string]any) (string, error)
	// Notices returns side-channel text seen so far (logging notifications
	// and stderr), used to surface OAuth URLs printed by the server.
	Notices() string
	Close() error
}

// mcpDialer opens a fresh MCP session.
type mcpDialer func(ctx context.Context) (mcpClient, error)

// mcpSession speaks newline-delimited JSON-RPC 2.0 over a reader/writer
// pair. Requests are serialized; notifications from the server are kept as
// notices. Close terminates the underlying process when there is one.
type mcpSession struct {
	w      io.Writer
	lines  chan []byte
	done   chan struct{}
	nextID atomic.Int64
	mu     sync.Mutex
	notice strings.Builder
	stderr *lockedBuffer
	closer func() error
	reqMu  sync.Mutex
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// newMCPSession wires a session over r/w and performs the MCP handshake.
func newMCPSession(ctx context.Context, r io.Reader, w io.Writer, stderr *lockedBuffer, closer func() error) (*mcpSession, error) {
	s := &mcpSession{w: w, lines: make(chan []byte, 64), done: make(chan struct{}), stderr: stderr, closer: closer}
	go s.readLoop(r)
	init := map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "hive-knowledge-connector", "version": "1"},
	}
	if _, err := s.request(ctx, "initialize", init); err != nil {
		_ = s.Close()
		return nil, err
	}
	_ = s.notify("notifications/initialized", nil)
	return s, nil
}

func (s *mcpSession) readLoop(r io.Reader) {
	defer close(s.done)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), mcpMaxLineBytes)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		s.lines <- []byte(line)
	}
}

func (s *mcpSession) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.w.Write(append(data, '\n'))
	return err
}

func (s *mcpSession) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return s.send(msg)
}

// request sends one request and waits for its reply, recording any
// notifications that arrive in between.
func (s *mcpSession) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	id := s.nextID.Add(1)
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, fmt.Errorf("mcp: sending %s: %w", method, err)
	}
	want := fmt.Sprintf("%d", id)
	for {
		select {
		case <-ctx.Done():
			return nil, s.withStderr(fmt.Errorf("mcp: %s: %w", method, ctx.Err()))
		case <-s.done:
			// Drain replies that arrived just before the server hung up.
			for {
				select {
				case line := <-s.lines:
					if res, ok, err := s.match(line, want, method); ok {
						return res, err
					}
					continue
				default:
				}
				return nil, s.withStderr(fmt.Errorf("mcp: %s: server closed the connection", method))
			}
		case line := <-s.lines:
			if res, ok, err := s.match(line, want, method); ok {
				return res, err
			}
		}
	}
}

// match decodes one line; ok is true when it is the reply to request id want.
func (s *mcpSession) match(line []byte, want, method string) (json.RawMessage, bool, error) {
	var msg rpcMessage
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, false, nil
	}
	if len(msg.ID) == 0 || msg.Method != "" {
		s.recordNotification(msg)
		return nil, false, nil
	}
	if strings.Trim(string(msg.ID), `"`) != want {
		return nil, false, nil
	}
	if msg.Error != nil {
		return nil, true, fmt.Errorf("mcp: %s: %s (code %d)", method, msg.Error.Message, msg.Error.Code)
	}
	return msg.Result, true, nil
}

func (s *mcpSession) recordNotification(msg rpcMessage) {
	if len(msg.Params) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.notice.Len() < 64<<10 {
		s.notice.Write(msg.Params)
		s.notice.WriteByte('\n')
	}
}

func (s *mcpSession) withStderr(err error) error {
	if s.stderr == nil {
		return err
	}
	if tail := strings.TrimSpace(s.stderr.Tail(2000)); tail != "" {
		return fmt.Errorf("%w (stderr: %s)", err, tail)
	}
	return err
}

// Notices implements mcpClient.
func (s *mcpSession) Notices() string {
	s.mu.Lock()
	out := s.notice.String()
	s.mu.Unlock()
	if s.stderr != nil {
		out += s.stderr.Tail(16 << 10)
	}
	return out
}

// Call implements mcpClient: tools/call with the text content joined.
func (s *mcpSession) Call(ctx context.Context, tool string, args map[string]any) (string, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := s.request(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("mcp: %s: decoding result: %w", tool, err)
	}
	var parts []string
	for _, c := range res.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		if text == "" {
			text = "tool reported an error"
		}
		return text, fmt.Errorf("mcp: %s: %s", tool, firstLine(text))
	}
	return text, nil
}

// Close implements mcpClient.
func (s *mcpSession) Close() error {
	if s.closer != nil {
		return s.closer()
	}
	return nil
}

// lockedBuffer is a goroutine-safe byte buffer used for child stderr.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() > 256<<10 {
		return len(p), nil
	}
	return b.buf.Write(p)
}

// Tail returns the last n bytes written.
func (b *lockedBuffer) Tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// mcpChildEnvNames are the exact environment variables a stdio MCP server
// inherits from the hive process; mcpChildEnvPrefixes extend the set by
// prefix. The server is third-party code fetched at run time (the Plaud
// default is `npx -y @plaud-ai/mcp@latest`), so it gets what a Node process
// needs to start and reach the registry through the egress proxy — never the
// hub-injected credentials the hive process carries (DASHBOARD_AUTH_TOKEN,
// HIVE_TERMINAL_KEY, HIVE_UPSTREAM_PROXY_URL, provider API keys, …), which
// crux/agent strips from agent panes for the same reason.
var mcpChildEnvNames = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"TMPDIR": true, "TMP": true, "TEMP": true, "TZ": true, "TERM": true, "LANG": true, "LANGUAGE": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
}

var mcpChildEnvPrefixes = []string{"LC_", "XDG_", "NODE_", "NPM_CONFIG_", "npm_config_", "PLAUD_"}

// mcpChildEnv filters base (KEY=VALUE pairs) down to the allowlisted names.
// A name that looks like a credential is dropped even when it matches, so a
// future prefix cannot readmit one.
func mcpChildEnv(base []string) []string {
	out := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || mcpCredentialName(key) {
			continue
		}
		if mcpChildEnvNames[key] {
			out = append(out, entry)
			continue
		}
		for _, p := range mcpChildEnvPrefixes {
			if strings.HasPrefix(key, p) {
				out = append(out, entry)
				break
			}
		}
	}
	return out
}

func mcpCredentialName(key string) bool {
	u := strings.ToUpper(key)
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL"} {
		if strings.Contains(u, marker) {
			return true
		}
	}
	return false
}

// stdioMCPDialer starts command as a child process per session. home, when
// non-empty, overrides HOME so the server keeps its own token store in a
// persistent directory. The child's environment is the allowlisted subset
// mcpChildEnv keeps, not the hive process environment.
func stdioMCPDialer(command, home string) mcpDialer {
	return func(ctx context.Context) (mcpClient, error) {
		argv, err := splitCommandLine(command)
		if err != nil {
			return nil, err
		}
		if len(argv) == 0 {
			return nil, errors.New("mcp: command is empty")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = mcpChildEnv(os.Environ())
		if home != "" {
			if err := os.MkdirAll(home, 0o700); err != nil {
				return nil, fmt.Errorf("mcp: creating home %s: %w", home, err)
			}
			cmd.Env = append(cmd.Env, "HOME="+home)
			cmd.Dir = home
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		stderr := &lockedBuffer{}
		cmd.Stderr = stderr
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("mcp: starting %q: %w", argv[0], err)
		}
		closer := sync.OnceValue(func() error {
			_ = stdin.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			return cmd.Wait()
		})
		return newMCPSession(ctx, stdout, stdin, stderr, closer)
	}
}

// splitCommandLine splits a shell-like command line on whitespace, honouring
// single and double quotes. No expansion is performed.
func splitCommandLine(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
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
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("mcp: unterminated quote in command %q", s)
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}

var httpsURLRe = regexp.MustCompile(`https://[^\s"'<>\\)\]]+`)

// firstHTTPSURL returns the first https:// URL found in s, or "".
func firstHTTPSURL(s string) string {
	return httpsURLRe.FindString(s)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ensureDir creates dir (and parents) and returns it.
func ensureDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Clean(dir), 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

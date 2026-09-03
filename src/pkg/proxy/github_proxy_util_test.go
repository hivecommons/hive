package proxy

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
)

// ---------- isGitPath ----------

func TestIsGitPath_TableDriven(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/owner/repo/git-receive-pack", true},
		{"/owner/repo/git-upload-pack", true},
		{"/owner/repo/info/refs", true},
		{"/owner/repo/info/refs?service=git-upload-pack", false},
		{"/owner/repo/pulls", false},
		{"/git-receive-pack", true},
		{"", false},
		{"/owner/repo/git-receive-pack/extra", false},
		{"/info/refs", true},
		{"git-upload-pack", false},
	}
	for _, tt := range tests {
		if got := isGitPath(tt.path); got != tt.want {
			t.Errorf("isGitPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// ---------- truncateBytes ----------

func TestTruncateBytes_TableDriven(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		max  int
		want string
	}{
		{"nil", nil, 10, ""},
		{"empty", []byte{}, 5, ""},
		{"under limit", []byte("hello"), 10, "hello"},
		{"at limit", []byte("hello"), 5, "hello"},
		{"over limit", []byte("hello world"), 5, "hello..."},
		{"zero max", []byte("abc"), 0, "..."},
		{"one over", []byte("ab"), 1, "a..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateBytes(tt.in, tt.max); got != tt.want {
				t.Errorf("truncateBytes(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}

// ---------- jsonEscape ----------

func TestJsonEscape_TableDriven(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"simple", "simple"},
		{`has "quotes"`, `has \"quotes\"`},
		{"has\nnewline", `has\nnewline`},
		{"backslash\\path", `backslash\\path`},
		{"tab\there", `tab\there`},
		{"", ""},
		{`{"key":"val"}`, `{\"key\":\"val\"}`},
		{"unicode: \u2603", "unicode: \u2603"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := jsonEscape(tt.in); got != tt.want {
				t.Errorf("jsonEscape(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// ---------- recordViolation ----------

func TestRecordViolation_Basic(t *testing.T) {
	p := &GitHubProxy{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		violations: make(map[string]int),
	}

	p.recordViolation("agent-a", "POST", "/repos/owner/repo/issues")
	p.recordViolation("agent-a", "POST", "/repos/owner/repo/pulls")
	p.recordViolation("agent-b", "PUT", "/repos/owner/repo/contents/f")

	if got := p.AgentViolations("agent-a"); got != 2 {
		t.Errorf("agent-a violations = %d, want 2", got)
	}
	if got := p.AgentViolations("agent-b"); got != 1 {
		t.Errorf("agent-b violations = %d, want 1", got)
	}
	if got := p.AgentViolations("unknown"); got != 0 {
		t.Errorf("unknown violations = %d, want 0", got)
	}

	all := p.Violations()
	if len(all) != 2 {
		t.Errorf("Violations() len = %d, want 2", len(all))
	}
}

func TestRecordViolation_MaxCap(t *testing.T) {
	p := &GitHubProxy{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		violations: make(map[string]int),
	}

	// Fill to maxViolationLog unique agents
	for i := 0; i < maxViolationLog; i++ {
		name := "agent-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		p.recordViolation(name, "GET", "/x")
	}
	if got := len(p.Violations()); got != maxViolationLog {
		t.Fatalf("violations count = %d, want %d", got, maxViolationLog)
	}

	// A new agent beyond the cap should not be added
	p.recordViolation("overflow-agent-xyz", "GET", "/x")
	if got := p.AgentViolations("overflow-agent-xyz"); got != 0 {
		t.Errorf("overflow agent should not be recorded, got %d", got)
	}

	// But an existing agent can still increment
	first := "agent-a0"
	before := p.AgentViolations(first)
	p.recordViolation(first, "GET", "/y")
	if got := p.AgentViolations(first); got != before+1 {
		t.Errorf("existing agent increment: got %d, want %d", got, before+1)
	}
}

// ---------- Violations snapshot isolation ----------

func TestViolations_SnapshotIsolation(t *testing.T) {
	p := &GitHubProxy{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		violations: make(map[string]int),
	}

	p.recordViolation("x", "GET", "/")
	snap := p.Violations()
	p.recordViolation("x", "GET", "/")

	if snap["x"] != 1 {
		t.Errorf("snapshot x = %d, want 1", snap["x"])
	}
	if p.AgentViolations("x") != 2 {
		t.Errorf("live x = %d, want 2", p.AgentViolations("x"))
	}
}

// ---------- concurrent recordViolation safety ----------

func TestRecordViolation_Concurrent(t *testing.T) {
	p := &GitHubProxy{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		violations: make(map[string]int),
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.recordViolation("concurrent", "POST", "/x")
		}()
	}
	wg.Wait()

	if got := p.AgentViolations("concurrent"); got != 100 {
		t.Errorf("concurrent violations = %d, want 100", got)
	}
}

// ---------- writeHTTPError ----------

func TestWriteHTTPError_Format(t *testing.T) {
	p := &GitHubProxy{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	var buf bytes.Buffer
	conn := &testBufConn{Writer: &buf}

	p.writeHTTPError(conn, 403, "blocked by proxy")

	resp, err := http.ReadResponse(bufio.NewReader(&buf), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 403 {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("blocked by proxy")) {
		t.Errorf("body = %q, missing 'blocked by proxy'", body)
	}
}

func TestWriteHTTPError_SpecialChars(t *testing.T) {
	p := &GitHubProxy{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	var buf bytes.Buffer
	conn := &testBufConn{Writer: &buf}

	p.writeHTTPError(conn, 500, `msg with "quotes" and \backslash`)

	resp, err := http.ReadResponse(bufio.NewReader(&buf), nil)
	if err != nil {
		t.Fatalf("ReadResponse: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// ---------- readAgentMode fallback ----------

func TestReadAgentMode_EmptyName(t *testing.T) {
	got := readAgentMode("")
	if got != agent.ModeAdvisory {
		t.Errorf("readAgentMode('') = %v, want ModeAdvisory", got)
	}
}

func TestReadAgentMode_MissingFile(t *testing.T) {
	got := readAgentMode("nonexistent-agent-xyz-99999")
	if got != agent.ModeAdvisory {
		t.Errorf("readAgentMode('nonexistent') = %v, want ModeAdvisory", got)
	}
}

// ---------- helper: testBufConn implements net.Conn ----------

type testBufConn struct {
	Writer io.Writer
}

func (c *testBufConn) Read([]byte) (int, error)          { return 0, io.EOF }
func (c *testBufConn) Write(b []byte) (int, error)       { return c.Writer.Write(b) }
func (c *testBufConn) Close() error                      { return nil }
func (c *testBufConn) LocalAddr() net.Addr               { return &net.TCPAddr{} }
func (c *testBufConn) RemoteAddr() net.Addr              { return &net.TCPAddr{} }
func (c *testBufConn) SetDeadline(_ time.Time) error     { return nil }
func (c *testBufConn) SetReadDeadline(_ time.Time) error { return nil }
func (c *testBufConn) SetWriteDeadline(_ time.Time) error { return nil }

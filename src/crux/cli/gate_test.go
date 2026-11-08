package cli

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/gates"
	"github.com/hivecommons/hive/crux/serve"
)

// gateServer is a real crux serve gate family on a token-bearing httptest
// server, with a clock the test moves.
type gateServer struct {
	url string
	mu  sync.Mutex
	now time.Time
}

func (g *gateServer) clock() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.now
}

func (g *gateServer) advance(d time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.now = g.now.Add(d)
}

func newGateServer(t *testing.T) *gateServer {
	t.Helper()
	g := &gateServer{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	svc, err := gates.New(gates.Config{Dir: filepath.Join(t.TempDir(), gates.DirName), Now: g.clock})
	if err != nil {
		t.Fatal(err)
	}
	srv := serve.New(newFakeRuntime())
	srv.Token, srv.OperatorName, srv.Gates = "gate-token", "kelly", svc
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	g.url = ts.URL
	return g
}

func TestGateIsRemoteOnly(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	h := newHarness(t)
	if code := h.run("gate", "list"); code != ExitUsage || !strings.Contains(h.stderr.String(), "needs --server") {
		t.Fatalf("local gate: %d %s", code, h.stderr)
	}
	h = newHarness(t)
	h.run("help")
	if !strings.Contains(h.stderr.String(), "gate open|list|answer|wait") {
		t.Fatalf("usage does not list gate:\n%s", h.stderr)
	}
}

func TestGateOpenAnswerWait(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	g := newGateServer(t)
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", g.url, "--token", "gate-token", "gate"}, args...)...)
	}

	h, code := remote("open", "--subject", "ship it?", "--choice", "ship", "--choice", "hold", "--context", `{"pr":17}`, "--opened-by", "release-bot")
	fields := strings.Fields(h.stdout.String())
	if code != ExitOK || len(fields) != 2 || !strings.HasPrefix(fields[0], gates.IDPrefix) || fields[1] != api.GateWaiting {
		t.Fatalf("open: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	id := fields[0]

	// The operator opened it, so answering needs the explicit override; an
	// off-menu answer is refused either way.
	if h, code := remote("answer", id, "--choice", "ship"); code != ExitUsage || !strings.Contains(h.stderr.String(), "override") {
		t.Fatalf("self answer: %d %s", code, h.stderr)
	}
	if h, code := remote("answer", id, "--choice", "sure", "--as-operator"); code != ExitUsage || !strings.Contains(h.stderr.String(), "not one of") {
		t.Fatalf("off-menu answer: %d %s", code, h.stderr)
	}
	if h, code := remote("answer", "--choice", "ship", "--note", "green", "--as-operator", id); code != ExitOK || !strings.Contains(h.stdout.String(), "answered\tship") {
		t.Fatalf("answer: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	if h, code := remote("wait", id); code != ExitOK || h.stdout.String() != "ship\n" {
		t.Fatalf("wait: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	h, code = remote("list", "--status", "answered", "--json")
	var list []api.Gate
	if err := json.Unmarshal(h.stdout.Bytes(), &list); code != ExitOK || err != nil || len(list) != 1 || list[0].AnsweredBy != "kelly" ||
		!list[0].OperatorOverride || list[0].Note != "green" || list[0].OpenedBy != "release-bot" {
		t.Fatalf("list: %d %v %s", code, err, h.stdout)
	}
	// The list output is indented, so the raw context carries that
	// whitespace; compare it compacted.
	var compact bytes.Buffer
	if err := json.Compact(&compact, list[0].Context); err != nil || compact.String() != `{"pr":17}` {
		t.Fatalf("list context: %v %q", err, list[0].Context)
	}

	// Expiry is reported as its own outcome: nothing on stdout, exit 1.
	h, _ = remote("open", "--subject", "quick", "--timeout", "1m")
	expiring := strings.Fields(h.stdout.String())[0]
	g.advance(2 * time.Minute)
	if h, code := remote("wait", expiring); code != ExitError || h.stdout.String() != "" || !strings.Contains(h.stderr.String(), "expired without an answer") {
		t.Fatalf("wait on expired: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
}

func TestGateUsage(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	g := newGateServer(t)
	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"gate"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"gate", "help"}, ExitOK, "usage: crux --server ADDR gate open"},
		{[]string{"gate", "bogus"}, ExitUsage, `unknown subcommand "bogus"`},
		{[]string{"gate", "open"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"gate", "open", "--subject", "s", "--context", "{"}, ExitUsage, "not valid JSON"},
		{[]string{"gate", "answer", "gate_a"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"gate", "answer", "gate_a", "gate_b", "--choice", "x"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"gate", "wait"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"gate", "list", "--status", "pending"}, ExitUsage, "pending"},
	} {
		h := newHarness(t)
		if code := h.run(append([]string{"--server", g.url, "--token", "gate-token"}, tc.args...)...); code != tc.code || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("%v: %d %s", tc.args, code, h.stderr)
		}
	}
}

func TestGateOutputsAndFailures(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	g := newGateServer(t)
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", g.url, "--token", "gate-token", "gate"}, args...)...)
	}

	h, code := remote("open", "--subject", "json please", "--json")
	var opened api.Gate
	if err := json.Unmarshal(h.stdout.Bytes(), &opened); code != ExitOK || err != nil || opened.Status != api.GateWaiting || strings.Join(opened.Choices, ",") != "approve,deny" {
		t.Fatalf("open --json: %d %v %s", code, err, h.stdout)
	}
	h, code = remote("answer", opened.ID, "--choice", "deny", "--as-operator", "--json")
	var answered api.Gate
	if err := json.Unmarshal(h.stdout.Bytes(), &answered); code != ExitOK || err != nil || answered.Answer != "deny" || !answered.OperatorOverride {
		t.Fatalf("answer --json: %d %v %s", code, err, h.stdout)
	}
	h, code = remote("wait", opened.ID, "--json")
	var waited api.Gate
	if err := json.Unmarshal(h.stdout.Bytes(), &waited); code != ExitOK || err != nil || waited.Answer != "deny" {
		t.Fatalf("wait --json: %d %v %s", code, err, h.stdout)
	}
	g.advance(time.Second)
	h, _ = remote("open", "--subject", "still open")
	open := strings.Fields(h.stdout.String())[0]

	// The table lists both, answered first (it was opened first), with
	// the answerer beside the answer and a dash for none.
	h, code = remote("list")
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if code != ExitOK || len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], "deny (kelly)") ||
		!strings.Contains(lines[1], "approve|deny") || !strings.Contains(lines[2], open) || !strings.Contains(lines[2], "  -  ") {
		t.Fatalf("list: %d\n%s", code, h.stdout)
	}

	// --timeout bounds the whole wait; a gate still waiting when it
	// passes is reported as such, not as an error from the server.
	if h, code := remote("wait", open, "--timeout", "50ms"); code != ExitError || !strings.Contains(h.stderr.String(), "still waiting") {
		t.Fatalf("wait --timeout: %d %s", code, h.stderr)
	}

	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"open", "--subject", "s", "extra"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"open", "--subject", "s", "--timeout", "soon"}, ExitUsage, "timeout"},
		{[]string{"open", "--subject", "s", "--choice", "a", "--choice", "a"}, ExitUsage, "duplicate choice"},
		{[]string{"list", "extra"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"answer", "--choice", "approve", "gate_a", "gate_b"}, ExitUsage, "usage: crux --server ADDR gate open"},
		{[]string{"answer", "gate_nope", "--choice", "approve"}, ExitError, "not found"},
		{[]string{"wait", "gate_nope"}, ExitError, "not found"},
		{[]string{"wait", "--timeout", "nope", "gate_a"}, ExitUsage, "invalid value"},
	} {
		h := newHarness(t)
		if code := h.run(append([]string{"--server", g.url, "--token", "gate-token", "gate"}, tc.args...)...); code != tc.code || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("%v: %d %s", tc.args, code, h.stderr)
		}
	}

	h = newHarness(t)
	if code := h.run("--server", g.url, "--token", "wrong", "gate", "list"); code != ExitError || !strings.Contains(h.stderr.String(), "bearer token") {
		t.Fatalf("wrong token: %d %s", code, h.stderr)
	}
}

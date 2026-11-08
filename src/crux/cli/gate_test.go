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

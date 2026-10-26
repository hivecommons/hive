package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/crux/effects"
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/serve"
	"github.com/hivecommons/hive/crux/toolapprove"
)

// managerLike is a fake runtime with the manager setters the hive callback
// feeds; it records what it was handed.
type managerLike struct {
	*fakeRuntime
	mu     sync.Mutex
	prs    agent.PRCreator
	linear func() agent.LinearCredential
	minter pushbroker.TokenMinter
	route  func(agentName, backend, model string)
	clear  func(agentName string)
	bound  effects.Boundary
}

func (m *managerLike) SetInferenceCallbacks(set func(agentName, backend, model string), clear func(agentName string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.route, m.clear = set, clear
}

func (m *managerLike) SetSandboxMutationBoundary(b effects.Boundary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bound = b
}

func (m *managerLike) SetSandboxPRClient(c agent.PRCreator) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prs = c
}

func (m *managerLike) SetLinearCredentialResolver(fn func() agent.LinearCredential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.linear = fn
}

func (m *managerLike) SetSandboxPushMinter(t pushbroker.TokenMinter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.minter = t
}

func TestServeHiveCallbackFeedsTheManagerSeams(t *testing.T) {
	t.Setenv(config.ServeTokenEnvVar, "")
	t.Setenv(callback.TokenEnvVar, "")
	t.Cleanup(func() { toolapprove.SetDefaultTextScanner(nil) })
	h := newHarness(t)
	m := &managerLike{fakeRuntime: h.rt}
	h.env.newRuntime = func(map[string]config.AgentConfig, crux.ProjectContext) crux.Runtime { return m }
	dir := shortSocketDir(t)
	socket := filepath.Join(dir, "api.sock")
	// Nothing listens on the hive socket: every capability fails closed and
	// the outage is reported.
	hiveSocket := filepath.Join(dir, "hive.sock")
	t.Setenv(callback.SocketEnvVar, hiveSocket)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- h.env.Run(ctx, append(h.workDirArgs(), "serve", "--socket", socket))
	}()
	waitForSocket(t, socket)

	m.mu.Lock()
	linear, prs, minter, route, clear, bound := m.linear, m.prs, m.minter, m.route, m.clear, m.bound
	m.mu.Unlock()
	if linear == nil || prs == nil || minter == nil || route == nil || clear == nil || bound == nil {
		t.Fatalf("seams not fed: linear=%v prs=%v minter=%v route=%v clear=%v boundary=%v", linear != nil, prs != nil, minter != nil, route != nil, clear != nil, bound != nil)
	}
	if toolapprove.DefaultTextScanner() == nil {
		t.Fatal("text scanner not installed")
	}

	health := func() api.Health {
		t.Helper()
		resp, err := socketClient(socket).Get("http://crux" + api.PathHealth)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out api.Health
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if hc := health().HiveCallback; hc == nil || !hc.Reachable || hc.Address != "unix://"+hiveSocket {
		t.Fatalf("health before any call: %+v", hc)
	}
	if cred := linear(); !cred.Empty() {
		t.Fatalf("linear over a dead socket = %+v, want empty", cred)
	}
	if _, err := minter.MintPushToken(ctx, "repo"); err == nil {
		t.Fatal("mint over a dead socket must fail")
	}
	// The route callbacks return at once even with hive away; the boundary
	// refuses the effect rather than running it unjournaled.
	route("a", "litellm", "m")
	clear("a")
	if _, err := bound.Execute(ctx, effects.Claim{Repo: "o/r", Kind: effects.KindBranchPush, Target: "b"}, func(context.Context) (effects.Result, error) {
		t.Fatal("effect ran with hive unreachable")
		return effects.Result{}, nil
	}); err == nil {
		t.Fatal("boundary over a dead socket must refuse")
	}
	if hc := health().HiveCallback; hc == nil || hc.Reachable || hc.LastError == "" || hc.FailedCalls < 2 || hc.Since == nil {
		t.Fatalf("health after failed calls: %+v", hc)
	}
	cancel()
	<-done
	if !strings.Contains(h.stdout.String(), "hive callback unix://"+hiveSocket+": text scanner, conditions, pull requests, Linear credential, push tokens, inference routes, mutation boundary") {
		t.Fatalf("stdout = %q", h.stdout.String())
	}
}

func TestServeHiveCallbackOnAPlainRuntime(t *testing.T) {
	t.Setenv(config.ServeTokenEnvVar, "")
	t.Setenv(callback.SocketEnvVar, "")
	t.Cleanup(func() { toolapprove.SetDefaultTextScanner(nil) })
	h := newHarness(t)
	socket := filepath.Join(shortSocketDir(t), "api.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- h.env.Run(ctx, append(h.workDirArgs(), "serve", "--socket", socket, "--hive-callback", "http://127.0.0.1:1"))
	}()
	waitForSocket(t, socket)
	cancel()
	if code := <-done; code != ExitOK {
		t.Fatalf("exit %d stderr %q", code, h.stderr)
	}
	if out := h.stdout.String(); !strings.Contains(out, "hive callback http://127.0.0.1:1: text scanner, conditions\n") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestServeHiveCallbackBadAddress(t *testing.T) {
	t.Setenv(config.ServeTokenEnvVar, "")
	h := newHarness(t)
	if code := h.env.Run(context.Background(), append(h.workDirArgs(), "serve", "--hive-callback", "ftp://nope")); code != ExitUsage {
		t.Fatalf("exit %d stderr %q", code, h.stderr)
	}
	if !strings.Contains(h.stderr.String(), "hive callback ftp://nope") {
		t.Fatalf("stderr = %q", h.stderr.String())
	}
}

// Conditions reach hive through the server's tee, which Serve installs on a
// Host runtime and which forwards to srv.ConditionReporter; the manager
// setter would be overwritten when Serve starts. The watch's own
// hive_callback_unavailable goes to the stream only, never over the
// callback it describes.
func TestHiveCallbackInstallRoutesConditionsThroughTheServerTee(t *testing.T) {
	t.Cleanup(func() { toolapprove.SetDefaultTextScanner(nil) })
	hiveSocket := filepath.Join(shortSocketDir(t), "hive.sock")
	h, err := dialHiveCallback(hiveSocket, "")
	if err != nil {
		t.Fatal(err)
	}
	rec := cruxtest.NewRecorder()
	srv := serve.New(rec)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out strings.Builder
	h.install(ctx, rec, srv, &out)

	if srv.ConditionReporter != h.set.Conditions {
		t.Fatalf("srv.ConditionReporter = %T, want the hivecap reporter", srv.ConditionReporter)
	}
	if !strings.Contains(out.String(), "conditions") {
		t.Fatalf("banner does not list conditions: %q", out.String())
	}
	// Through the tee, a runtime condition is attempted over the (dead)
	// callback: fails closed and counts as a failed call.
	before := h.client.Status().FailedCalls
	srv.Conditions().Observe("stall", "scanner", "m", "d")
	if after := h.client.Status().FailedCalls; after != before+1 {
		t.Fatalf("condition not forwarded over the callback: failed calls %d → %d", before, after)
	}
	// The stream-only reporter never touches the callback.
	srv.StreamConditions().Observe(callback.ConditionClassUnavailable, "endpoint", "down", "")
	if after := h.client.Status().FailedCalls; after != before+1 {
		t.Fatalf("stream-only condition travelled over the callback: failed calls %d", after)
	}
}

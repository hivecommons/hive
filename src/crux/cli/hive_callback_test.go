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
	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/toolapprove"
)

// managerLike is a fake runtime with the manager setters the hive callback
// feeds; it records what it was handed.
type managerLike struct {
	*fakeRuntime
	mu         sync.Mutex
	conditions crux.ConditionReporter
	prs        agent.PRCreator
	linear     func() agent.LinearCredential
	minter     pushbroker.TokenMinter
}

func (m *managerLike) SetConditionReporter(r crux.ConditionReporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.conditions = r
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
	linear, prs, minter, conds := m.linear, m.prs, m.minter, m.conditions
	m.mu.Unlock()
	if linear == nil || prs == nil || minter == nil || conds == nil {
		t.Fatalf("seams not fed: linear=%v prs=%v minter=%v conds=%v", linear != nil, prs != nil, minter != nil, conds != nil)
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
	if hc := health().HiveCallback; hc == nil || hc.Reachable || hc.LastError == "" || hc.FailedCalls < 2 || hc.Since == nil {
		t.Fatalf("health after failed calls: %+v", hc)
	}
	cancel()
	<-done
	if !strings.Contains(h.stdout.String(), "hive callback unix://"+hiveSocket+": text scanner, conditions, pull requests, Linear credential, push tokens") {
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
	if out := h.stdout.String(); !strings.Contains(out, "hive callback http://127.0.0.1:1: text scanner\n") {
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

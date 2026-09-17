package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/hub"
	"github.com/hivecommons/hive/pkg/notify"
)

// Residual branch coverage for the hub boot seam (hub_boot_seams.go) and the
// hook wiring (hookwire.go). The seams themselves are tested; these tests pin
// the branches the existing suites never enter:
//
//   - wireHubHooks' SUCCESS path — a valid config must actually arm hooks at
//     hub boot, not just fail politely when the config is missing or broken.
//   - buildHookDispatcher's nil-config guard and the Notifier/ApprovalQueue
//     sink wiring branches.
//   - installAgentPauseEmitter's nil-manager guard.
//   - notifierAdapter.Send's forwarding line (only its nil guards were run).
//   - defaultHubDeps' startPollers and serve closures — the production wiring
//     runHub hands to runHubWithDeps had never been executed by any test.

// TestWireHubHooksValidConfigArmsDispatcher covers the branch wireHubHooks
// exists for: a config that loads cleanly must produce an armed dispatcher.
// The existing tests only proved the two FAILURE modes (missing file is
// silent, malformed file warns); a regression that broke the success path —
// hooks silently never arming on a hub — would have passed both.
func TestWireHubHooksValidConfigArmsDispatcher(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	path := filepath.Join(t.TempDir(), "hive.yaml")
	cfgYAML := `hive_id: test-hive
project:
  org: test-org
  repos:
    - repo-a
github:
  token: test-token-not-real
agents:
  worker:
    backend: claude
    enabled: true
hooks:
  - name: boot-hook
    on: review_rejected
    action: notify
`
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	logger, logs := capturingLogger()
	wireHubHooks(logger, path)

	if strings.Contains(logs(), "hub hooks disabled") {
		t.Fatalf("a valid config must not disable hooks:\n%s", logs())
	}
	d := hookDispatcher()
	if d == nil {
		t.Fatal("a valid config with one hook must arm the dispatcher")
	}
	if d.Len() != 1 {
		t.Fatalf("dispatcher armed with %d hooks, want 1", d.Len())
	}
}

// TestBuildHookDispatcherNilConfigIsNoOp pins the nil guard: a nil config must
// leave the dispatcher untouched rather than panic or disarm a working set.
func TestBuildHookDispatcherNilConfigIsNoOp(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	buildHookDispatcher(nil, hookSinks{}, hookTestLogger())
	if hookDispatcher() != nil {
		t.Fatal("nil config must not arm a dispatcher")
	}

	// And it must not disarm one that already exists.
	armed := &config.Config{Hooks: []config.HookRule{{Name: "h", On: "review_rejected", Action: "notify"}}}
	buildHookDispatcher(armed, hookSinks{}, hookTestLogger())
	buildHookDispatcher(nil, hookSinks{}, hookTestLogger())
	if hookDispatcher() == nil {
		t.Fatal("nil config must keep the previously armed dispatcher")
	}
}

// stubApprovalQueue satisfies hooks.ApprovalQueue for sink-wiring coverage.
type stubApprovalQueue struct{}

func (stubApprovalQueue) EnqueueApproval(context.Context, hooks.ApprovalRequest) error { return nil }

// TestBuildHookDispatcherWiresNotifierAndApprovalSinks exercises the two sink
// branches no existing test passes (Notifier, Approvals). Every other sink
// branch is covered by the pause/annotate/audit tests; these two appends had
// never run, so a typo swapping the options would have compiled and shipped.
func TestBuildHookDispatcherWiresNotifierAndApprovalSinks(t *testing.T) {
	resetHookDispatcher(t)
	t.Cleanup(func() { resetHookDispatcher(t) })

	cfg := &config.Config{Hooks: []config.HookRule{{
		Name: "h", On: "review_rejected", Action: "notify",
	}}}
	sinks := hookSinks{
		Notifier:  notify.New(config.NotificationsConfig{}, hookTestLogger()),
		Approvals: stubApprovalQueue{},
	}
	buildHookDispatcher(cfg, sinks, hookTestLogger())
	if hookDispatcher() == nil {
		t.Fatal("dispatcher must arm with notifier and approval sinks wired")
	}
}

// TestNotifierAdapterForwardsWithoutChannels runs the forwarding line with a
// real Notifier whose config has no channels: Send must be a safe no-op, not
// a panic — the adapter is called from hook dispatch on a live hub.
func TestNotifierAdapterForwardsWithoutChannels(t *testing.T) {
	n := notify.New(config.NotificationsConfig{}, hookTestLogger())
	a := &notifierAdapter{n: n}
	a.Send("title", "message", "high") // must not panic and must not block
}

// TestInstallAgentPauseEmitterNilManagerIsNoOp pins the guard that lets
// agent-less modes (hub mode) call the installer unconditionally.
func TestInstallAgentPauseEmitterNilManagerIsNoOp(t *testing.T) {
	installAgentPauseEmitter(nil) // must not panic
}

// TestDefaultHubDepsStartPollersRunsAndStops executes the production
// startPollers closure — the wiring runHub actually boots with. A cancelled
// context must let every poller exit; if the closure ever pointed at a
// blocking or non-cancellable call, this test would hang instead of pass.
func TestDefaultHubDepsStartPollersRunsAndStops(t *testing.T) {
	isolateHubServer(t)
	logger, _ := capturingLogger()
	srv := hub.NewHubServer(0, logger, "abc1234", "v4")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defaultHubDeps().startPollers(ctx, srv)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("production startPollers did not return with a cancelled context")
	}
}

// TestDefaultHubDepsServeIsTheRealListener executes the production serve
// closure: it must bind a real listener via HubServer.Start and report
// http.ErrServerClosed on graceful shutdown — the exact error runHubWithDeps
// treats as a clean stop. Port 0 keeps the bind ephemeral and conflict-free.
func TestDefaultHubDepsServeIsTheRealListener(t *testing.T) {
	isolateHubServer(t)
	logger, logs := capturingLogger()
	srv := hub.NewHubServer(0, logger, "abc1234", "v4")

	serveErr := make(chan error, 1)
	go func() { serveErr <- defaultHubDeps().serve(srv, 0) }()

	// Wait for the listener to be up before shutting it down.
	deadline := time.After(10 * time.Second)
	for !strings.Contains(logs(), "hub server starting") {
		select {
		case err := <-serveErr:
			t.Fatalf("serve returned before shutdown: %v", err)
		case <-deadline:
			t.Fatal("hub server never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Start stores the *http.Server handle before ListenAndServe; Shutdown
	// waits for it. Retry briefly in case shutdown wins the race with bind.
	var err error
	select {
	case <-time.After(50 * time.Millisecond):
	case err = <-serveErr:
		t.Fatalf("serve returned before shutdown: %v", err)
	}
	if err := srv.Shutdown(5 * time.Second); err != nil {
		t.Fatalf("graceful shutdown failed: %v", err)
	}
	select {
	case err = <-serveErr:
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
	if !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("production serve must surface ErrServerClosed on graceful stop, got %v", err)
	}
}

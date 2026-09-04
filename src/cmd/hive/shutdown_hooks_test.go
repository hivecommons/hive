package main

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// TestShutdownHooksRunsBothArchiveAndDrain is the regression guard for the trap
// kubestellar/hive#5390 walked into.
//
// The pre-shutdown slot used to be a single atomic.Pointer[func()]. Storing a
// second hook SILENTLY discarded the first — no error, no failing test, a
// shutdown side effect simply stopped happening. Since #4296's kick-log archive
// already held that slot, adding the contributor drain to it would have deleted
// the archive on every pod roll, invisibly.
//
// This asserts the OBSERVABLE consequence: after registering both, running the
// sequence produces both effects.
func TestShutdownHooksRunsBothArchiveAndDrain(t *testing.T) {
	var (
		mu       sync.Mutex
		archived bool
		drained  bool
	)

	var hooks shutdownHooks
	hooks.add("archive-kick-logs", func() {
		mu.Lock()
		defer mu.Unlock()
		archived = true
	})
	hooks.addUrgent("drain-contributor-websockets", func() {
		mu.Lock()
		defer mu.Unlock()
		drained = true
	})

	if got := hooks.count(); got != 2 {
		t.Fatalf("registered hook count = %d, want 2 — a hook was overwritten "+
			"rather than appended", got)
	}

	hooks.run()

	mu.Lock()
	if !archived {
		t.Error("ArchiveAllKickLogs equivalent did NOT run — registering the " +
			"WebSocket drain destroyed the #4296 kick-log archive")
	}
	if !drained {
		t.Error("contributor drain did NOT run")
	}
	archived, drained = false, false
	mu.Unlock()

	// Same assertion with BOTH registered via add(), so the guard does not
	// depend on addUrgent's prepend happening to survive a destructive add.
	var plain shutdownHooks
	plain.add("archive-kick-logs", func() {
		mu.Lock()
		defer mu.Unlock()
		archived = true
	})
	plain.add("drain-contributor-websockets", func() {
		mu.Lock()
		defer mu.Unlock()
		drained = true
	})
	if got := plain.count(); got != 2 {
		t.Fatalf("add() is destructive: count = %d after two registrations, want 2", got)
	}
	plain.run()

	mu.Lock()
	defer mu.Unlock()
	if !archived || !drained {
		t.Errorf("add()-registered hooks did not both run (archived=%v drained=%v) — "+
			"the second registration discarded the first", archived, drained)
	}
}

// TestShutdownHooksAddUrgentRunsFirst pins the ordering the drain depends on.
// The drain is registered later than the archive (the dashboard server is
// constructed after the agent manager) but must run first: it puts a close
// frame on a wire a relay is waiting on, while the archive does PVC I/O on NFS
// that nobody is waiting on.
func TestShutdownHooksAddUrgentRunsFirst(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	record := func(name string) func() {
		return func() {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
		}
	}

	var hooks shutdownHooks
	hooks.add("archive", record("archive"))
	hooks.add("other", record("other"))
	hooks.addUrgent("drain", record("drain"))

	hooks.run()

	mu.Lock()
	defer mu.Unlock()
	want := []string{"drain", "archive", "other"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("hook order = %v, want %v", order, want)
	}
}

// TestShutdownHooksPanicDoesNotSkipRemaining pins that one failing hook cannot
// cost the others theirs. A panic inside the drain must not prevent the
// kick-log archive from running, and vice versa.
func TestShutdownHooksPanicDoesNotSkipRemaining(t *testing.T) {
	var (
		mu     sync.Mutex
		ranAll []string
	)

	var hooks shutdownHooks
	hooks.add("boom", func() { panic("hook exploded") })
	hooks.add("archive", func() {
		mu.Lock()
		defer mu.Unlock()
		ranAll = append(ranAll, "archive")
	})

	// Must not propagate the panic out of run().
	hooks.run()

	mu.Lock()
	defer mu.Unlock()
	if len(ranAll) != 1 || ranAll[0] != "archive" {
		t.Errorf("hooks after a panicking hook did not run: %v", ranAll)
	}
}

// TestShutdownHooksZeroValueAndNilAreSafe covers the signal handler firing
// before anything registered — a SIGTERM during early startup.
func TestShutdownHooksZeroValueAndNilAreSafe(t *testing.T) {
	var hooks shutdownHooks
	hooks.run() // must not panic
	hooks.add("nil-fn", nil)
	hooks.addUrgent("nil-urgent", nil)
	if got := hooks.count(); got != 0 {
		t.Errorf("nil hooks were registered: count = %d, want 0", got)
	}
	hooks.run()
}

// TestMainRegistersBothPreShutdownHooks is a STRUCTURAL guard on the wiring in
// main.go, which the unit tests above cannot reach: main() is a 2000-line
// function that stands up the whole process and cannot be invoked from a test.
//
// It pins three things that a future edit could silently undo:
//  1. both hooks are still registered,
//  2. the mechanism is still additive (no atomic.Pointer Store on the slot),
//  3. the drain still runs BEFORE cancel() in the signal handler, while the
//     connections are still live.
//
// It is deliberately narrow — it does not attempt to verify runtime behaviour
// from source text.
func TestMainRegistersBothPreShutdownHooks(t *testing.T) {
	configSrc, err := os.ReadFile("configwire.go")
	if err != nil {
		t.Fatalf("read configwire.go: %v", err)
	}
	governorSrc, err := os.ReadFile("governorwire.go")
	if err != nil {
		t.Fatalf("read governorwire.go: %v", err)
	}
	notifySrc, err := os.ReadFile("notifywire.go")
	if err != nil {
		t.Fatalf("read notifywire.go: %v", err)
	}
	dashboardSrc, err := os.ReadFile("dashboardwire.go")
	if err != nil {
		t.Fatalf("read dashboardwire.go: %v", err)
	}
	body := string(configSrc) + "\n" + string(governorSrc) + "\n" + string(notifySrc) + "\n" + string(dashboardSrc)

	for _, want := range []string{
		`w.preShutdownHooks.add("archive-kick-logs"`,
		`w.preShutdownHooks.addUrgent("drain-contributor-websockets"`,
		`DrainContributorsForShutdown()`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("spoke wiring no longer contains %q — a pre-shutdown hook was dropped", want)
		}
	}

	// The destructive single-slot mechanism must not come back.
	if regexp.MustCompile(`preShutdownHook\b.*\.Store\(`).MatchString(body) {
		t.Error("main.go stores into a single-slot preShutdownHook again — that " +
			"mechanism silently discards previously registered hooks (#5390)")
	}

	// Ordering: hooks must run before cancel() in the signal goroutine.
	runIdx := strings.Index(body, "w.preShutdownHooks.run()")
	if runIdx < 0 {
		t.Fatal("signal handler no longer runs the pre-shutdown hooks")
	}
	cancelIdx := strings.Index(body[runIdx:], "cancel()")
	if cancelIdx < 0 {
		t.Fatal("could not locate cancel() after the hook run in the signal handler")
	}
	// Guard against the two being reordered: anything between them must be short.
	if between := body[runIdx : runIdx+cancelIdx]; strings.Count(between, "\n") > 3 {
		t.Errorf("cancel() is no longer immediately after preShutdownHooks.run(); "+
			"the drain must run while connections are still live. Between them:\n%s", between)
	}
}

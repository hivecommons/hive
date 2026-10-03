//go:build !windows

package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/config"
)

func startBlockedSpekExecutor(t *testing.T) (*ContributeWSHub, *Server, *SpekHubExecutor, <-chan struct{}, chan struct{}, string) {
	t.Helper()
	hub, s, _, _ := spekHub(t)
	st := spekHubStage{runKey: spekRunKey, stage: StageSpec, gen: 1}
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "admit")] = &taskLease{identity: runAdmissionIdentity, taskID: "admit", repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e.Exec = func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	}
	s.SetStageExecutor(e)
	work := spekHubRunWorktreePath(e.Identity, st.runKey)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	// Cleanup registration order ensures no worker touches the fixture after it
	// resets the process-wide workspace and lifecycle store.
	t.Cleanup(e.Stop)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	e.Tick(context.Background(), time.Now())
	waitSpekSignal(t, started, "workspace preparation")
	return hub, s, e, cancelled, release, work
}

func waitSpekSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func TestSpekHubExecutorRevocationPreservesWorkUntilJoined(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "removed"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			hub, _, e, cancelled, release, work := startBlockedSpekExecutor(t)
			hub.leaseMu.Lock()
			for key, lease := range hub.leases {
				if reset {
					lease.gen++
				} else {
					delete(hub.leases, key)
				}
			}
			hub.leaseMu.Unlock()
			if err := e.sweepStaleWorktrees(context.Background()); err != nil {
				t.Fatal(err)
			}
			waitSpekSignal(t, cancelled, "lease cancellation")
			if _, err := os.Stat(work); err != nil {
				t.Fatalf("swept running worktree: %v", err)
			}
			if !e.IsExecuting(spekRunKey, StageSpec) {
				t.Fatal("released slot before worker exited")
			}
			close(release)
			e.Stop()
			if e.Status().Running != 0 {
				t.Fatal("worker survived Stop")
			}
			if e.Status().LastError != "" {
				t.Fatalf("revocation counted as failure: %s", e.Status().LastError)
			}
			hub.leaseMu.Lock()
			clear(hub.leases)
			hub.leaseMu.Unlock()
			e.mu.Lock()
			e.held["obsolete"] = true
			e.mu.Unlock()
			if err := e.sweepStaleWorktrees(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(work); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale worktree retained: %v", err)
			}
			if len(e.held) != 0 || len(e.activity) != 0 {
				t.Fatal("obsolete bookkeeping retained")
			}
			e.Tick(context.Background(), time.Now())
			if e.Status().Running != 0 {
				t.Fatal("stopped executor launched work")
			}
		})
	}
}

func TestSpekHubExecutorShutdownAndReplacementJoinWorkers(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "shutdown"
		if replace {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			hub, s, e, cancelled, release, _ := startBlockedSpekExecutor(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if replace {
					s.SetStageExecutor(nil)
				} else {
					s.Close()
				}
			}()
			waitSpekSignal(t, cancelled, "shutdown cancellation")
			select {
			case <-done:
				t.Fatal("shutdown returned before worker joined")
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			waitSpekSignal(t, done, "shutdown join")
			e.Tick(context.Background(), time.Now())
			if e.Status().Running != 0 {
				t.Fatal("stopped executor relaunched persisted lease")
			}
			// A generation cancelled by shutdown was not spent: it must not be
			// recorded as a failure, and its lease keeps its generation so the
			// next hub relaunches it rather than a retry.
			if msg := e.Status().LastError; msg != "" {
				t.Fatalf("shutdown counted as failure: %s", msg)
			}
			assertSpekLeaseGens(t, hub, 1)
		})
	}
}

func assertSpekLeaseGens(t *testing.T, hub *ContributeWSHub, want uint64) {
	t.Helper()
	hub.leaseMu.Lock()
	defer hub.leaseMu.Unlock()
	if len(hub.leases) == 0 {
		t.Fatal("stage lease vanished")
	}
	for key, lease := range hub.leases {
		if lease.gen != want {
			t.Fatalf("lease %s at gen %d, want %d", key, lease.gen, want)
		}
	}
}

// A launch the fence refuses (another process still owns the run's worktree)
// neither fails nor spends the generation: once the owner releases the lock the
// next tick launches that same generation.
func TestSpekHubExecutorFencedLaunchRetriesSameGeneration(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "admit")] = &taskLease{identity: runAdmissionIdentity, taskID: "admit", repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	started, release := make(chan struct{}), make(chan struct{})
	e.Exec = func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}
	s.SetStageExecutor(e)
	work := spekHubRunWorktreePath(e.Identity, spekRunKey)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	t.Cleanup(func() { close(release) })
	orphan, err := acquireSpekHubFence(filepath.Join(filepath.Dir(work), ".executor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer orphan.Close()

	e.Tick(context.Background(), time.Now())
	e.mu.Lock()
	var worker <-chan struct{}
	for _, run := range e.inFlight {
		worker = run.done
	}
	e.mu.Unlock()
	if worker != nil {
		waitSpekSignal(t, worker, "fenced worker exit")
	}
	select {
	case <-started:
		t.Fatal("launched while another process owned the worktree")
	default:
	}
	if msg := e.Status().LastError; msg != "" {
		t.Fatalf("fenced launch counted as failure: %s", msg)
	}
	assertSpekLeaseGens(t, hub, 1)

	orphan.Close()
	e.Tick(context.Background(), time.Now())
	waitSpekSignal(t, started, "launch after the fence was released")
}

func TestSpekHubExecutorStatusCaptureStopJoinsPoll(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{}, "copilot", "", nil, nil)
	started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	e.Exec = func(ctx context.Context, _ string, _ []string, _ string, _ ...string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return nil, ctx.Err()
	}
	_, stop := e.startStageStatusCapture(context.Background(), spekHubStage{}, t.TempDir(), nil, StageSpec, "test")
	waitSpekSignal(t, started, "status poll")
	done := make(chan struct{})
	go func() { stop(); close(done) }()
	waitSpekSignal(t, cancelled, "poll cancellation")
	select {
	case <-done:
		t.Error("stop returned before status poll exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	waitSpekSignal(t, done, "poll join")
	stop() // idempotent
}

func TestSpekHubExecutorFenceSurvivesParentClose(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{}, "copilot", "", nil, nil)
	work := spekHubRunWorktreePath(e.Identity, spekRunKey)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(work), ".executor.lock")
	fence, err := acquireSpekHubFence(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fence.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", "cat >/dev/null")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	spekHubInheritFence(cmd, context.WithValue(ctx, spekHubFenceContextKey{}, fence))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()
	// Simulate the old hub disappearing while the CLI retains the descriptor.
	fence.Close()
	if other, err := acquireSpekHubFence(path); !errors.Is(err, errSpekHubFenceBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("second hub acquired orphan's lock: %v", err)
	}
	if err := e.executeStage(context.Background(), spekHubStage{runKey: spekRunKey}); !errors.Is(err, errSpekHubFenceBusy) {
		t.Fatalf("double launch not fenced: %v", err)
	}
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("swept orphan's worktree: %v", err)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireSpekHubFence(path)
	if err != nil {
		t.Fatalf("lock not released on exit: %v", err)
	}
	next.Close()
}

func TestSpekHubExecutorOutputTailIsBounded(t *testing.T) {
	var w spekHubTailWriter
	full := strings.Repeat("a", spekHubOutputTailBytes*3) + strings.Repeat("b", 1024)
	for i := 0; i < len(full); i += 1000 {
		end := min(i+1000, len(full))
		n, err := w.Write([]byte(full[i:end]))
		if n != end-i || err != nil {
			t.Fatal("short write", n, err)
		}
		if len(w.tail) > spekHubOutputTailBytes {
			t.Fatal("unbounded output")
		}
	}
	if w.String() != full[len(full)-spekHubOutputTailBytes:] {
		t.Fatal("wrong diagnostic tail")
	}
	_, _ = w.Write([]byte(full))
	if w.String() != full[len(full)-spekHubOutputTailBytes:] {
		t.Fatal("wrong oversized-write tail")
	}
}

func TestSpekHubExecutorCommandStreamsFullLogWithBoundedTail(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{}, "copilot", "", nil, nil)
	work := t.TempDir()
	out, _, err := e.runStageCommand(context.Background(), work, os.Environ(), spekHubStage{stage: StageSpec, gen: 1}, []string{"sh", "-c", `head -c 131072 /dev/zero | tr '\000' x; printf '\nEND\n'`})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != spekHubOutputTailBytes || !strings.HasSuffix(string(out), "\nEND\n") {
		t.Fatalf("unexpected tail length=%d", len(out))
	}
	log, err := os.ReadFile(filepath.Join(work, ".hive", "spek-stage-spec-1.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 131072+5 {
		t.Fatalf("full log truncated: %d bytes", len(log))
	}
}

// A clone/fetch failure before the agent launches is infrastructure, not the
// stage's fault: it backs off and retries the same generation (#10077).
func TestSpekHubExecutorWorkspaceFailureBacksOffWithoutSpendingGeneration(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "admit")] = &taskLease{identity: runAdmissionIdentity, taskID: "admit", repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 1, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	clones := make(chan struct{}, 4)
	e.Exec = func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		if name == "git" && len(args) > 0 && args[0] == "clone" {
			clones <- struct{}{}
			return []byte("could not resolve host"), errors.New("exit status 128")
		}
		return nil, nil
	}
	t.Cleanup(e.Stop)
	tickAndJoin := func(now time.Time) {
		t.Helper()
		e.Tick(context.Background(), now)
		e.mu.Lock()
		var worker <-chan struct{}
		for _, run := range e.inFlight {
			worker = run.done
		}
		e.mu.Unlock()
		if worker != nil {
			waitSpekSignal(t, worker, "worker exit")
		}
	}
	now := time.Now()
	tickAndJoin(now)
	waitSpekSignal(t, clones, "first clone attempt")
	if e.Status().LastError == "" {
		t.Fatal("workspace failure not surfaced")
	}
	assertSpekLeaseGens(t, hub, 1)
	hub.leaseMu.Lock()
	for _, lease := range hub.leases {
		if !lease.stageEscalatedAt.IsZero() {
			hub.leaseMu.Unlock()
			t.Fatal("workspace failure escalated the stage")
		}
	}
	hub.leaseMu.Unlock()
	e.mu.Lock()
	held := len(e.held)
	e.mu.Unlock()
	if held != 0 {
		t.Fatal("workspace failure held the generation")
	}

	tickAndJoin(now.Add(time.Second))
	select {
	case <-clones:
		t.Fatal("relaunched inside the backoff window")
	default:
	}
	tickAndJoin(now.Add(spekHubInfraBackoffBase + time.Minute))
	waitSpekSignal(t, clones, "retry after backoff")
	assertSpekLeaseGens(t, hub, 1)
}

// A missing backend credential is infrastructure, not the stage's fault: it
// backs off and retries instead of launching the agent anyway (#10077).
func TestSpekHubExecutorMissingCredentialBacksOffWithoutLaunching(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "admit")] = &taskLease{identity: runAdmissionIdentity, taskID: "admit", repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	worktree := runStageWorktreePath(config.DefaultSpektacularHubExecutorIdentity, spekRunKey, StageSpec, 1)
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, ".spektacular"), 0o755); err != nil {
		t.Fatal(err)
	}
	// bob requires a configured API key; leaving Governor.Bob unset makes
	// executorEnv's credential lookup fail every time.
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 1, Spektacular: config.SpektacularConfig{Enabled: true}}, "bob", "", nil, nil)
	launched := make(chan struct{}, 4)
	e.Exec = func(_ context.Context, _ string, _ []string, name string, _ ...string) ([]byte, error) {
		if name == "bob" {
			launched <- struct{}{}
		}
		return nil, nil
	}
	t.Cleanup(e.Stop)
	tickAndJoin := func(now time.Time) {
		t.Helper()
		e.Tick(context.Background(), now)
		e.mu.Lock()
		var worker <-chan struct{}
		for _, run := range e.inFlight {
			worker = run.done
		}
		e.mu.Unlock()
		if worker != nil {
			waitSpekSignal(t, worker, "worker exit")
		}
	}
	now := time.Now()
	tickAndJoin(now)
	select {
	case <-launched:
		t.Fatal("agent launched despite missing credential")
	default:
	}
	if e.Status().LastError == "" {
		t.Fatal("missing credential not surfaced")
	}
	assertSpekLeaseGens(t, hub, 1)
	hub.leaseMu.Lock()
	for _, lease := range hub.leases {
		if !lease.stageEscalatedAt.IsZero() {
			hub.leaseMu.Unlock()
			t.Fatal("missing credential escalated the stage")
		}
	}
	hub.leaseMu.Unlock()
	e.mu.Lock()
	held := len(e.held)
	e.mu.Unlock()
	if held != 0 {
		t.Fatal("missing credential held the generation")
	}

	tickAndJoin(now.Add(spekHubInfraBackoffBase + time.Minute))
	assertSpekLeaseGens(t, hub, 1)
}

// A colliding run-worktree slug is permanent, not an infrastructure outage:
// it must spend the generation instead of entering the retry/backoff loop.
func TestSpekHubExecutorRunWorktreeSlugCollisionSpendsGeneration(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 1, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	first := "foo/bar-baz#1"
	second := "foo-bar/baz#1"
	if sanitizeRunPromptPath(first) != sanitizeRunPromptPath(second) {
		t.Fatalf("test fixture no longer collides: %q vs %q", sanitizeRunPromptPath(first), sanitizeRunPromptPath(second))
	}
	worktree := spekHubRunWorktreePath(e.Identity, second)
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, spekHubRunKeyMarkerFile), []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	taskID := e.stageTaskID(spekHubStage{runKey: second, stage: StageSpec, gen: 1})
	hub.leaseMu.Lock()
	hub.leases[leaseKey(e.Identity, taskID)] = &taskLease{identity: e.Identity, taskID: taskID, repo: spekRepo, number: 1, key: spekRepo + "!" + second + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	st := spekHubStage{runKey: second, key: spekRepo + "!" + second + ":" + StageSpec, stage: StageSpec, identity: e.Identity, taskID: taskID, repo: spekRepo, number: 1, gen: 1}

	e.runStage(context.Background(), st, e.executionKey(st))

	if msg := e.Status().LastError; !strings.Contains(msg, "collision") {
		t.Fatalf("last error = %q, want slug collision", msg)
	}
	e.mu.Lock()
	_, held := e.held[e.executionKey(st)]
	_, backedOff := e.backoff[e.executionKey(st)]
	e.mu.Unlock()
	if !held {
		t.Fatal("slug collision did not hold/spend the generation")
	}
	if backedOff {
		t.Fatal("slug collision was treated as retryable infrastructure")
	}
	hub.leaseMu.Lock()
	lease := hub.leases[leaseKey(e.Identity, taskID)]
	hub.leaseMu.Unlock()
	if lease == nil || lease.stageEscalatedAt.IsZero() {
		t.Fatalf("slug collision did not escalate the spent generation: %#v", lease)
	}
}

// A settlement that fails to persist is retried instead of leaving the
// generation held in memory only (#10108).
func TestSpekHubExecutorRetriesFailedSettlement(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{MaxStageRetries: 1, Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	taskID := "run-hub-settle"
	hub.leaseMu.Lock()
	hub.leases[leaseKey(e.Identity, taskID)] = &taskLease{identity: e.Identity, taskID: taskID, repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hub.persistTaskLedgers = true
	hub.taskLeasesFile = filepath.Join(blocker, "task-leases.json")
	st := spekHubStage{runKey: spekRunKey, stage: StageSpec, identity: e.Identity, taskID: taskID, repo: spekRepo, number: 57, gen: 1}
	e.settleGeneration(st, spekHubFailureReason)
	e.mu.Lock()
	_, pending := e.unsettled[e.executionKey(st)]
	e.mu.Unlock()
	if !pending {
		t.Fatal("failed settlement was not kept for retry")
	}

	hub.taskLeasesFile = filepath.Join(t.TempDir(), "task-leases.json")
	e.retryUnsettled()
	e.mu.Lock()
	left := len(e.unsettled)
	e.mu.Unlock()
	if left != 0 {
		t.Fatal("settlement still pending after a successful retry")
	}
	hub.leaseMu.Lock()
	escalated := !hub.leases[leaseKey(e.Identity, taskID)].stageEscalatedAt.IsZero()
	hub.leaseMu.Unlock()
	if !escalated {
		t.Fatal("retried settlement did not escalate the exhausted stage")
	}
}

// An agent that exits right at the stage deadline is still judged by its
// document: the post-exit status check is not bound by the expired deadline
// (#10110).
func TestSpekHubExecutorPostExitStatusOutlivesStageDeadline(t *testing.T) {
	_, s, _, _ := spekHub(t)
	runs := config.RunsConfig{MaxStageRetries: 1, Spektacular: config.SpektacularConfig{Enabled: true, HubExecutor: config.SpektacularHubExecutorConfig{TimeoutSeconds: 1}}}
	e := NewSpekHubExecutor(s, runs, "copilot", "", nil, nil)
	worktree := spekHubRunWorktreePath(e.Identity, spekRunKey)
	for _, dir := range []string{filepath.Join(worktree, ".spektacular"), filepath.Join(currentAgentWorkspaceRoot(), e.Identity, filepath.FromSlash(spekRepo), ".git")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.Exec = func(ctx context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		switch name {
		case "git":
			return nil, nil
		case "spektacular":
			if len(args) >= 3 && args[1] == "status" {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return []byte(`{"error":false,"kind":"spec","name":"` + args[2] + `","document_status":"final"}`), nil
			}
			return nil, nil
		}
		// The agent finishes its document and exits 0 as the deadline passes.
		<-ctx.Done()
		return []byte("done"), nil
	}
	st := spekHubStage{runKey: spekRunKey, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, identity: e.Identity, taskID: "run-hub-deadline", repo: spekRepo, number: 57, gen: 1}
	if err := e.executeStage(context.Background(), st); err != nil {
		t.Fatalf("final document at the deadline spent the generation: %v", err)
	}
}

// A finished run's lock and empty run directory are removed by the sweep
// (#10111), and a later fence on that path takes a fresh lock file.
func TestSpekHubExecutorSweepRemovesFinishedRunLock(t *testing.T) {
	_, s, _, _ := spekHub(t)
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	work := spekHubRunWorktreePath(e.Identity, spekRunKey)
	runDir := filepath.Dir(work)
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	fence, err := acquireSpekHubFence(filepath.Join(runDir, ".executor.lock"))
	if err != nil {
		t.Fatal(err)
	}
	fence.Close()
	var sweepErr, statErr error
	testutil.EventuallyEveryFunc(t, 5*time.Second, 10*time.Millisecond, func() bool {
		sweepErr = e.sweepStaleWorktrees(context.Background())
		if sweepErr != nil {
			return false
		}
		_, statErr = os.Stat(runDir)
		return errors.Is(statErr, os.ErrNotExist)
	}, func() string {
		return fmt.Sprintf("finished run directory retained: stat=%v sweep=%v", statErr, sweepErr)
	})

	lock := filepath.Join(t.TempDir(), "run", ".executor.lock")
	held, err := acquireSpekHubFence(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	next, err := acquireSpekHubFence(lock)
	if err != nil {
		t.Fatalf("fence on a fresh lock file: %v", err)
	}
	next.Close()
}

func TestSpekHubExecutorSweepKeepsLockOfActiveRun(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	hub.leaseMu.Lock()
	hub.leases[leaseKey(runAdmissionIdentity, "admit")] = &taskLease{identity: runAdmissionIdentity, taskID: "admit", repo: spekRepo, number: 57, key: spekRepo + "!" + spekRunKey + ":" + StageSpec, stage: StageSpec, gen: 1, expiresAt: time.Now().Add(leaseTTL)}
	hub.leaseMu.Unlock()
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	lock := filepath.Join(filepath.Dir(spekHubRunWorktreePath(e.Identity, spekRunKey)), ".executor.lock")
	fence, err := acquireSpekHubFence(lock)
	if err != nil {
		t.Fatal(err)
	}
	fence.Close()
	if err := e.sweepStaleWorktrees(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("active run lock removed: %v", err)
	}
}

func TestValidateSpekHubRepoPath(t *testing.T) {
	for _, repo := range []string{"myorg/repo1", "group/sub/repo", "o/hive.github.io"} {
		if err := validateSpekHubRepoPath(repo); err != nil {
			t.Errorf("%q rejected: %v", repo, err)
		}
	}
	for _, repo := range []string{"", "../../tmp/evil/x", "o/..", "./x", "/abs/repo", "o//r", `o\r`} {
		if err := validateSpekHubRepoPath(repo); err == nil {
			t.Errorf("%q accepted", repo)
		}
	}
}

func TestAdmitRunRejectsEscapingRepo(t *testing.T) {
	_, s, _, _ := spekHub(t)
	s.deps.Config.Runs.Spektacular.Enabled = true
	if err := s.AdmitRun("../../tmp/evil/x", 1, "x", time.Now()); err == nil || !strings.Contains(err.Error(), "invalid run repo") {
		t.Fatalf("admitting a repo that escapes the workspace: %v", err)
	}
}

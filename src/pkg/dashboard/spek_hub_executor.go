package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logscrub"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	spekHubExecutorTaskPrefix     = "run-hub-"
	spekHubPromptRelPath          = ".hive/spek-stage-prompt.txt"
	spekHubOutputTailBytes        = 64 * 1024
	spekHubLastErrorMaxBytes      = 2 * 1024
	spekHubLogPartialLineMaxBytes = 64 * 1024
	spekHubExecutorTier           = "trusted"
	spekHubFailureReason          = "hub_executor_failed"
	spekHubNonFinalReason         = "hub_executor_cli_exited_nonfinal"
	spekHubLeaseRenewInterval     = 5 * time.Minute
)

type SpekHubCloneAuth func(ctx context.Context, repo, dir string) (authArgs []string, token string, cleanup func(), err error)

type StageExecutor interface {
	Tick(ctx context.Context, now time.Time)
	Status() FrontendSpektacularHubExecutor
}

type StageExecutionTracker interface {
	IsExecuting(runKey, stage string) bool
}

type SpekHubExecutor struct {
	Server    *Server
	Config    config.RunsConfig
	Backend   string
	Model     string
	Identity  string
	CloneAuth SpekHubCloneAuth
	Logger    *slog.Logger
	Exec      func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
	CloneURL  func(repo string) string
	Artifact  func(runKey string) string

	tickMu   sync.Mutex
	mu       sync.Mutex
	stopped  bool
	inFlight map[string]*spekHubExecution
	// held marks generations (executionKey) Tick must not launch again: the
	// document is already final, or the generation was spent and settled
	// against the stage budget. The key changes with every new generation.
	held      map[string]bool
	activity  map[string]runActivitySignal
	lastError string
}

type spekHubExecution struct {
	started time.Time
	stage   spekHubStage
	cancel  context.CancelFunc
	done    chan struct{}
}

// Stop prevents new launches, cancels every owned process group, and waits for
// workers (including their status pollers) with a shutdown bound.
func (e *SpekHubExecutor) Stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.stopped = true
	var done []<-chan struct{}
	for _, run := range e.inFlight {
		run.cancel()
		done = append(done, run.done)
	}
	e.mu.Unlock()
	timer := time.NewTimer(2 * spekHubWaitDelay)
	defer timer.Stop()
	for _, ch := range done {
		select {
		case <-ch:
		case <-timer.C:
			e.log().Warn("[spektacular] executor shutdown timed out")
			return
		}
	}
}

type spekHubStage struct {
	runKey, key, stage, identity, taskID, repo, title string
	number                                            int
	gen                                               uint64
	workItem                                          worksource.WorkItemContext
}

type FrontendSpektacularHubExecutor struct {
	Running   int    `json:"running"`
	LastError string `json:"last_error,omitempty"`
}

func NewSpekHubExecutor(s *Server, runs config.RunsConfig, backend, model string, cloneAuth SpekHubCloneAuth, logger *slog.Logger) *SpekHubExecutor {
	return &SpekHubExecutor{
		Server:    s,
		Config:    runs,
		Backend:   runs.Spektacular.HubExecutor.BackendOrDefault(backend),
		Model:     firstSpekNonEmpty(strings.TrimSpace(runs.Spektacular.HubExecutor.Model), strings.TrimSpace(model)),
		Identity:  runs.Spektacular.HubExecutor.IdentityOrDefault(),
		CloneAuth: cloneAuth,
		Logger:    logger,
	}
}

func (e *SpekHubExecutor) Tick(ctx context.Context, now time.Time) {
	if e == nil || e.Server == nil || !e.Config.Spektacular.Enabled || !e.Config.Spektacular.HubExecutorEnabled() {
		return
	}
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	e.mu.Lock()
	stopped := e.stopped
	e.mu.Unlock()
	if stopped {
		return
	}
	// Sweep first: it runs git and can take a while, and the lease snapshot
	// below must be as fresh as possible before it is compared with inFlight.
	if err := e.sweepStaleWorktrees(ctx); err != nil {
		e.log().Warn("[spektacular] hub executor worktree sweep failed", "error", err)
	}
	stages, err := e.unclaimedStages()
	if err != nil {
		e.setLastError(err.Error())
		return
	}
	for _, st := range stages {
		if st.stage != StageSpec && st.stage != StagePlan {
			continue
		}
		if e.stageInterviewMode() != "auto" {
			if _, _, _, _, pending := spekInterviewPending(spekHubRunWorktreePath(e.Identity, st.runKey)); pending {
				continue
			}
		}
		key := e.executionKey(st)
		e.mu.Lock()
		if e.inFlight == nil {
			e.inFlight = map[string]*spekHubExecution{}
		}
		if e.held == nil {
			e.held = map[string]bool{}
		}
		if _, running := e.inFlight[key]; e.stopped || running || e.held[key] || e.runningLocked() >= e.maxConcurrent() {
			e.mu.Unlock()
			continue
		}
		runCtx, cancel := context.WithCancel(ctx)
		e.inFlight[key] = &spekHubExecution{started: now, stage: st, cancel: cancel, done: make(chan struct{})}
		e.recordActivityLocked(st, now, "executor claimed stage slot", 0)
		e.mu.Unlock()
		go e.runStage(runCtx, st, key)
	}
}

func (e *SpekHubExecutor) Status() FrontendSpektacularHubExecutor {
	if e == nil {
		return FrontendSpektacularHubExecutor{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return FrontendSpektacularHubExecutor{Running: e.runningLocked(), LastError: spekHubLastErrorSummary(e.lastError)}
}

func (e *SpekHubExecutor) IsExecuting(runKey, stage string) bool {
	if e == nil {
		return false
	}
	prefix := e.executionKeyPrefix(runKey, stage)
	e.mu.Lock()
	defer e.mu.Unlock()
	for key := range e.inFlight {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func (e *SpekHubExecutor) sweepStaleWorktrees(ctx context.Context) error {
	live := map[string]bool{}
	active := map[string]bool{}
	if err := e.Server.VisitActiveStageLeases(func(runKey, key, stage, identity, taskID, repo string, gen uint64, expiresAt time.Time) {
		active[e.executionKey(spekHubStage{runKey: runKey, stage: stage, gen: gen})] = true
		// A run's single worktree carries its spec/plan artifacts across
		// generations, so it stays live while ANY lease on the run is
		// active — including a freshly minted, not-yet-claimed retry
		// generation or an admission lease. Only stage-scoped worktrees are
		// tied to this executor's own leases.
		if path := spekHubRunWorktreePath(e.Identity, runKey); path != "" {
			live[path] = true
		}
		for _, path := range existingRunWorktreeDirs(e.Identity, runKey) {
			live[path] = true
		}
		if identity == e.Identity {
			if path := runStageWorktreePath(identity, runKey, stage, gen); path != "" {
				live[path] = true
			}
		}
	}); err != nil {
		return err
	}
	// A revoked generation must finish cancellation before its directory can
	// be swept. Keep its bookkeeping until that worker has joined as well.
	e.mu.Lock()
	for key, run := range e.inFlight {
		if !active[key] {
			run.cancel()
		}
		active[key] = true
		live[spekHubRunWorktreePath(e.Identity, run.stage.runKey)] = true
		for _, path := range existingRunWorktreeDirs(e.Identity, run.stage.runKey) {
			live[path] = true
		}
	}
	for key := range e.held {
		if !active[key] {
			delete(e.held, key)
		}
	}
	for key := range e.activity {
		if !active[key] {
			delete(e.activity, key)
		}
	}
	e.mu.Unlock()
	root := filepath.Join(currentAgentWorkspaceRoot(), e.Identity, "runs")
	runDirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return err
	}
	repos := e.sharedRepoDirs()
	for _, runDir := range runDirs {
		if !runDir.IsDir() {
			continue
		}
		// The stable lock lives outside the removable worktree. Never unlink it:
		// another hub or an orphaned CLI may still hold that inode.
		fence, err := acquireSpekHubFence(filepath.Join(root, runDir.Name(), ".executor.lock"))
		if err != nil {
			continue
		}
		stageDirs, err := os.ReadDir(filepath.Join(root, runDir.Name()))
		if err != nil {
			fence.Close()
			continue
		}
		for _, stageDir := range stageDirs {
			if !stageDir.IsDir() {
				continue
			}
			path := filepath.Join(root, runDir.Name(), stageDir.Name())
			if live[path] {
				continue
			}
			if err := e.removeWorktree(ctx, repos, path); err != nil {
				e.log().Warn("[spektacular] removing stale worktree failed", "path", path, "error", err)
			}
		}
		fence.Close()
	}
	return nil
}

func (e *SpekHubExecutor) sharedRepoDirs() []string {
	root := filepath.Join(currentAgentWorkspaceRoot(), e.Identity)
	owners, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, owner := range owners {
		if !owner.IsDir() || owner.Name() == "runs" || owner.Name() == "home" {
			continue
		}
		repos, err := os.ReadDir(filepath.Join(root, owner.Name()))
		if err != nil {
			continue
		}
		for _, repo := range repos {
			if repo.IsDir() {
				dir := filepath.Join(root, owner.Name(), repo.Name())
				if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
					out = append(out, dir)
				}
			}
		}
	}
	return out
}

func (e *SpekHubExecutor) removeWorktree(ctx context.Context, repos []string, path string) error {
	var last error
	for _, repo := range repos {
		if _, err := e.runner()(ctx, repo, spekGitEnv(os.Environ()), "git", "worktree", "remove", "--force", path); err == nil {
			return nil
		} else {
			last = err
		}
	}
	if err := removeRunStageWorktreeByPath(path); err != nil {
		if last != nil {
			return fmt.Errorf("%w; fallback remove: %v", last, err)
		}
		return err
	}
	return nil
}

func removeRunStageWorktreeByPath(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	return os.RemoveAll(path)
}

func spekHubRunWorktreePath(identity, runKey string) string {
	if identity == "" || runKey == "" {
		return ""
	}
	return filepath.Join(currentAgentWorkspaceRoot(), identity, "runs", sanitizeRunPromptPath(runKey), "work")
}

func existingRunWorktreeDirs(identity, runKey string) []string {
	root := filepath.Join(currentAgentWorkspaceRoot(), identity, "runs", sanitizeRunPromptPath(runKey))
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, filepath.Join(root, entry.Name()))
		}
	}
	return out
}

func (e *SpekHubExecutor) runStage(parent context.Context, st spekHubStage, key string) {
	defer func() {
		e.mu.Lock()
		if run := e.inFlight[key]; run != nil {
			run.cancel()
			close(run.done)
			delete(e.inFlight, key)
		}
		e.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(parent, e.Config.Spektacular.HubExecutor.Timeout())
	defer cancel()
	// The snapshot in Tick may predate a stage advance made by a run that
	// finished in the meantime; never relaunch a stage the lease has left.
	if !e.stageStillActive(st) {
		e.log().Info("[spektacular] hub executor skipping stale stage snapshot", "run", st.runKey, "stage", st.stage, "gen", st.gen)
		return
	}
	// A canceled parent is hub shutdown, not a failed generation: spending
	// the stage budget on it would burn a retry on every restart.
	// A fence held by another process is not a failed generation either.
	if err := e.executeStage(ctx, st); err != nil && parent.Err() == nil && !errors.Is(err, errSpekHubFenceBusy) {
		e.recordFailure(st, key, err)
		e.settleGeneration(st, spekHubFailureReason)
	}
}

func (e *SpekHubExecutor) executeStage(ctx context.Context, st spekHubStage) error {
	fence, err := acquireSpekHubFence(filepath.Join(filepath.Dir(spekHubRunWorktreePath(e.Identity, st.runKey)), ".executor.lock"))
	if err != nil {
		return err
	}
	defer fence.Close()
	ctx = context.WithValue(ctx, spekHubFenceContextKey{}, fence)
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	receiptDir := runReceiptsDir
	taskID := e.stageTaskID(st)
	if st.identity != e.Identity {
		e.log().Info("[spektacular] hub executor claiming stage", "run", st.runKey, "stage", st.stage, "gen", st.gen, "task", taskID)
		if err := e.Server.contributeHub.recordLeaseForKeyStage(e.Identity, taskID, st.repo, st.number, st.key, spekHubExecutorTier, st.stage, st.gen, now); err != nil {
			return err
		}
	}
	stopRenew := e.startRenewing(ctx, taskID)
	defer stopRenew()
	appToken, err := e.prepareWorkspace(ctx, st)
	if err != nil {
		return err
	}
	if err := e.Server.contributeHub.renewLease(e.Identity, taskID, time.Now().UTC()); err != nil {
		e.log().Warn("[spektacular] hub executor lease renew failed", "task", taskID, "error", err)
	}
	worktree := spekHubRunWorktreePath(e.Identity, st.runKey)
	if e.stageInterviewMode() != "auto" {
		if req, _, pending, askedAt, ok := spekInterviewPending(worktree); ok {
			e.recordInterviewWait(st, taskID, worktree, req, pending, askedAt)
			return nil
		}
	}
	e.recordStageProgress(st, "worktree_prepared", map[string]string{"worktree": worktree, "backend": e.backend()})
	artifact := e.artifact(st.runKey)
	env, err := e.executorEnv(appToken)
	if err != nil {
		return err
	}
	if status, final := e.finalArtifact(ctx, worktree, env, st.stage, artifact); final {
		// The document is already final (e.g. the plan is held for human
		// approval); launching the agent again would only burn a slot.
		e.log().Info("[spektacular] hub executor stage document already final; not relaunching", "run", st.runKey, "stage", st.stage, "gen", st.gen, "artifact", status.JoinKey())
		e.recordStageProgress(st, "document_already_final", map[string]string{stageAttrArtifact: status.JoinKey(), stageAttrDocumentStatus: status.DocumentStatus})
		if err := e.captureCompletedStage(st, worktree, artifact, status, nil, now, nil, receiptDir, spekCaptureAlreadyFinal); err != nil {
			e.log().Warn("[spektacular] stage transcript capture failed", "run", st.runKey, "stage", st.stage, "gen", st.gen, "error", err)
		}
		e.holdGeneration(st)
		e.Server.tickStageRunner(time.Now().UTC())
		return nil
	}
	var interviewAnswers []byte
	prompt := e.stagePrompt(st, artifact)
	if e.stageInterviewMode() != "auto" {
		interviewAnswers = readSpekInterviewAnswers(worktree)
		prompt += spekInterviewPromptBlock(st.stage, artifact, interviewAnswers)
	}
	if err := writeSpekHubPrompt(worktree, prompt); err != nil {
		return err
	}
	cmd, err := agent.HeadlessPromptCommand(e.backend(), e.Model, spekHubPromptRelPath)
	if err != nil {
		return err
	}
	started := time.Now()
	e.log().Info("[spektacular] hub executor launching stage", "run", st.runKey, "stage", st.stage, "gen", st.gen, "worktree", worktree, "cmd", cmd[0])
	e.recordStageProgress(st, "cli_launching", map[string]string{"backend": e.backend(), "worktree": worktree})
	statusHistory, stopStatusPolling := e.startStageStatusCapture(ctx, st, worktree, env, st.stage, artifact)
	out, pid, err := e.runStageCommand(ctx, worktree, env, st, cmd)
	stopStatusPolling()
	// Preserve files through transcript capture, and keep them on launch failure.
	if err == nil || pid > 0 {
		defer func() {
			if cleanupErr := clearConsumedSpekInterview(worktree, interviewAnswers); cleanupErr != nil {
				e.log().Warn("[spektacular] clearing consumed interview failed", "run", st.runKey, "error", cleanupErr)
			}
		}()
	}
	exitCode := 0
	if err != nil {
		exitCode = commandExitCode(err)
	}
	e.log().Info("[spektacular] hub executor finished stage", "run", st.runKey, "stage", st.stage, "gen", st.gen, "worktree", worktree, "pid", pid, "exit_code", exitCode, "duration", time.Since(started).String())
	e.recordStageProgress(st, "cli_exited", map[string]string{"backend": e.backend(), "pid": strconv.Itoa(pid), "exit_code": strconv.Itoa(exitCode), "duration": time.Since(started).String(), "worktree": worktree})
	if e.stageInterviewMode() != "auto" {
		if req, _, pending, askedAt, ok := spekInterviewPending(worktree); ok {
			e.recordInterviewWait(st, taskID, worktree, req, pending, askedAt)
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("agent CLI failed: %w: %s", err, tailString(string(out), spekHubOutputTailBytes))
	}
	if err := e.afterCLIExit(ctx, st, taskID, worktree, env, artifact, out, started, statusHistory(), receiptDir); err != nil {
		// The CLI exited but whether it reached final is unknown; treat the
		// generation as spent rather than relaunching it on every tick. If the
		// document is in fact final the poll runner still advances it.
		return fmt.Errorf("post-cli status check failed: %w", err)
	}
	return nil
}

// spekHubWaitDelay bounds how long exec.Cmd.Wait may block on inherited
// pipes after the stage process itself has exited or been killed.
const spekHubWaitDelay = 10 * time.Second

func (e *SpekHubExecutor) runStageCommand(ctx context.Context, worktree string, env []string, st spekHubStage, cmd []string) ([]byte, int, error) {
	logPath := filepath.Join(worktree, ".hive", fmt.Sprintf("spek-stage-%s-%d.log", sanitizeRunPromptPath(st.stage), st.gen))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, 0, err
	}
	writeLog := func(out []byte) {
		if writeErr := os.WriteFile(logPath, out, 0o600); writeErr != nil {
			e.log().Warn("[spektacular] writing hub executor cli log failed", "path", logPath, "error", writeErr)
		}
	}
	if e.Exec == nil {
		var buf spekHubTailWriter
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return nil, 0, err
		}
		defer f.Close()
		logWriter := newSpekHubScrubWriter(f)
		c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
		c.Dir = worktree
		c.Env = env
		// The agent CLI is `sh -c …` that forks node grandchildren. Kill the
		// whole group on timeout/cancel and bound Wait so an orphan holding
		// the inherited output pipe cannot pin the executor slot forever.
		spekHubConfigureProcessGroup(c)
		spekHubInheritFence(c, ctx)
		c.WaitDelay = spekHubWaitDelay
		w := io.MultiWriter(&buf, logWriter)
		c.Stdout = w
		c.Stderr = w
		if err := c.Start(); err != nil {
			closeErr := logWriter.Close()
			out := []byte(scrubSpekHubOutput(buf.String()))
			if closeErr != nil {
				e.log().Warn("[spektacular] flushing hub executor cli log failed", "path", logPath, "error", closeErr)
			}
			return out, 0, err
		}
		pid := c.Process.Pid
		e.log().Info("[spektacular] hub executor process started", "run", st.runKey, "stage", st.stage, "gen", st.gen, "worktree", worktree, "pid", pid)
		e.recordStageProgress(st, "cli_launched", map[string]string{"backend": e.backend(), "pid": strconv.Itoa(pid), "worktree": worktree})
		err = c.Wait()
		// The process has exited: reap any orphan still holding the output
		// pipe or the inherited fence fd, then judge the run by the exit
		// status rather than by pipe EOF (ErrWaitDelay implies exit 0).
		spekHubKillProcessGroup(c)
		if errors.Is(err, exec.ErrWaitDelay) {
			err = nil
		}
		if closeErr := logWriter.Close(); closeErr != nil {
			e.log().Warn("[spektacular] flushing hub executor cli log failed", "path", logPath, "error", closeErr)
		}
		out := []byte(scrubSpekHubOutput(buf.String()))
		return out, pid, err
	}
	out, err := e.runner()(ctx, worktree, env, cmd[0], cmd[1:]...)
	out = []byte(scrubSpekHubOutput(string(out)))
	writeLog(out)
	return out, 0, err
}

type spekHubScrubWriter struct {
	dst     io.Writer
	partial []byte
}

func newSpekHubScrubWriter(dst io.Writer) *spekHubScrubWriter {
	return &spekHubScrubWriter{dst: dst}
}

func (w *spekHubScrubWriter) Write(p []byte) (int, error) {
	if w == nil || w.dst == nil {
		return len(p), nil
	}
	written := len(p)
	for len(p) > 0 {
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			w.appendPartial(p[:i+1])
			if err := w.flushPartial(); err != nil {
				return written, err
			}
			p = p[i+1:]
			continue
		}
		for len(p) > 0 {
			space := spekHubLogPartialLineMaxBytes - len(w.partial)
			if space <= 0 {
				if err := w.flushPartial(); err != nil {
					return written, err
				}
				space = spekHubLogPartialLineMaxBytes
			}
			if len(p) <= space {
				w.appendPartial(p)
				p = nil
				break
			}
			w.appendPartial(p[:space])
			p = p[space:]
			if err := w.flushPartial(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (w *spekHubScrubWriter) Close() error {
	if w == nil || len(w.partial) == 0 {
		return nil
	}
	return w.flushPartial()
}

func (w *spekHubScrubWriter) appendPartial(p []byte) {
	w.partial = append(w.partial, p...)
}

func (w *spekHubScrubWriter) flushPartial() error {
	if len(w.partial) == 0 {
		return nil
	}
	_, err := io.WriteString(w.dst, scrubSpekHubOutput(string(w.partial)))
	w.partial = w.partial[:0]
	return err
}

func (e *SpekHubExecutor) afterCLIExit(ctx context.Context, st spekHubStage, taskID, worktree string, env []string, artifact string, cliOut []byte, started time.Time, history []RunDetailStageStatus, receiptDir string) error {
	kind := st.stage
	if kind != StageSpec && kind != StagePlan {
		return nil
	}
	status, err := e.spekStatus(ctx, worktree, env, kind, artifact)
	if err != nil {
		if resolved := resolveSpekArtifactFromFiles(worktree, kind, artifact); resolved != "" && resolved != artifact {
			e.recordStageProgress(st, "artifact_resolved", map[string]string{stageAttrArtifact: resolved})
			status, err = e.spekStatus(ctx, worktree, env, kind, resolved)
		}
	}
	if err != nil {
		return err
	}
	if strings.EqualFold(status.DocumentStatus, "final") {
		e.recordStageProgress(st, "document_status", map[string]string{stageAttrArtifact: status.JoinKey(), stageAttrDocumentStatus: status.DocumentStatus, stageAttrCurrentStep: status.CurrentStep})
		if err := e.captureCompletedStage(st, worktree, artifact, status, cliOut, started, history, receiptDir, "session"); err != nil {
			e.log().Warn("[spektacular] stage transcript capture failed", "run", st.runKey, "stage", st.stage, "gen", st.gen, "error", err)
		}
		// Run the poll runner immediately instead of waiting for the next
		// cleanup cadence; it owns advancement/receipts.
		e.Server.tickStageRunner(time.Now().UTC())
		return nil
	}
	e.log().Warn("[spektacular] hub executor cli exited before final document",
		"run", st.runKey, "stage", st.stage, "gen", st.gen, "artifact", status.JoinKey(), "document_status", status.DocumentStatus,
		"output_tail", tailString(string(cliOut), spekHubOutputTailBytes))
	e.recordNonFinal(st, taskID, status)
	if strings.EqualFold(status.DocumentStatus, "stale") {
		// Spek invalidated the plan; the poll runner parks the run as
		// stale_plan for a fresh plan or re-approval. Retrying the generation
		// cannot fix that, so it spends nothing and stays held.
		return nil
	}
	e.settleGeneration(st, spekHubNonFinalReason)
	return nil
}

func (e *SpekHubExecutor) startStageStatusCapture(ctx context.Context, st spekHubStage, worktree string, env []string, kind, artifact string) (func() []RunDetailStageStatus, func()) {
	pollCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var mu sync.Mutex
	var history []RunDetailStageStatus
	remember := func(status spekHubArtifactStatus) {
		snap := RunDetailStageStatus{
			At:             time.Now().UTC().Format(time.RFC3339Nano),
			Step:           status.CurrentStep,
			Instruction:    status.Instruction(),
			DocumentStatus: status.DocumentStatus,
			CompletedSteps: append([]string(nil), status.CompletedSteps...),
			Artifact:       status.JoinKey(),
			Raw:            status.Raw,
		}
		key := stageStatusCaptureKey(snap)
		mu.Lock()
		defer mu.Unlock()
		if len(history) > 0 {
			last := history[len(history)-1]
			lastKey := stageStatusCaptureKey(last)
			if key == lastKey {
				return
			}
		}
		history = append(history, snap)
	}
	poll := func() {
		resolved := firstRunNonEmpty(resolveSpekArtifactFromFiles(worktree, kind, artifact), artifact)
		if status, err := e.spekStatus(pollCtx, worktree, env, kind, resolved); err == nil {
			remember(status)
			e.recordStageProgress(st, "status_poll", map[string]string{stageAttrArtifact: status.JoinKey(), stageAttrDocumentStatus: status.DocumentStatus, stageAttrCurrentStep: status.CurrentStep})
		} else if isSpekArtifactNotFound(err) {
			e.recordActivity(st, "artifact_missing", map[string]string{stageAttrStage: kind}, 0)
		}
	}
	go func() {
		defer close(done)
		poll()
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				poll()
			case <-pollCtx.Done():
				return
			}
		}
	}()
	return func() []RunDetailStageStatus {
			mu.Lock()
			defer mu.Unlock()
			return append([]RunDetailStageStatus(nil), history...)
		}, func() {
			cancel()
			<-done
		}
}

func (e *SpekHubExecutor) captureCompletedStage(st spekHubStage, worktree, artifact string, status spekHubArtifactStatus, cliOut []byte, started time.Time, history []RunDetailStageStatus, receiptDir, captureMode string) error {
	if st.stage != StageSpec && st.stage != StagePlan {
		return nil
	}
	ended := time.Now().UTC()
	if started.IsZero() {
		started = ended
	}
	promptBytes, _ := os.ReadFile(filepath.Join(worktree, spekHubPromptRelPath))
	artifactKey := firstRunNonEmpty(status.JoinKey(), artifact)
	docs, files, interview, notes := collectSpekArtifactFiles(worktree, st.stage, artifactKey)
	interview = appendSpekHumanInterview(worktree, interview, ended.Format(time.RFC3339Nano))
	if len(interview) == 0 {
		interview = interviewFromStatusHistory(history, docs, ended)
	}
	if len(interview) == 0 {
		for _, step := range status.CompletedSteps {
			interview = append(interview, RunDetailInterview{
				Step:       step,
				Question:   step,
				Answer:     "Answer text was captured in the final document and agent transcript.",
				AnsweredAt: ended.Format(time.RFC3339Nano),
				Source:     "agent_assumed",
			})
		}
	}
	capture := RunDetailStageCapture{
		SchemaVersion: spekStageTranscriptSchema,
		RunKey:        st.runKey,
		Stage:         st.stage,
		Generation:    st.gen,
		Artifact:      artifactKey,
		Capture:       captureMode,
		CapturedAt:    ended.Format(time.RFC3339Nano),
		StartedAt:     started.Format(time.RFC3339Nano),
		EndedAt:       ended.Format(time.RFC3339Nano),
		Backend:       e.backend(),
		Model:         e.Model,
		StatusHistory: appendDistinctStageStatus(history, RunDetailStageStatus{
			At:             ended.Format(time.RFC3339Nano),
			Step:           status.CurrentStep,
			Instruction:    status.Instruction(),
			DocumentStatus: status.DocumentStatus,
			CompletedSteps: append([]string(nil), status.CompletedSteps...),
			Artifact:       status.JoinKey(),
			Raw:            status.Raw,
		}),
		Documents: docs,
		Files:     files,
		Interview: interview,
		Notes:     notes,
	}
	prompt := textBlock(promptBytes, spekStagePromptMaxTextBytes)
	if prompt.Text != "" {
		capture.Prompt = &prompt
	}
	out := textBlock([]byte(scrubSpekHubOutput(string(cliOut))), spekStageTranscriptMaxTextBytes)
	if out.Text != "" {
		capture.AgentTranscript = &out
		capture.AgentStdoutStderr = &out
	}
	return writeSpekStageCaptureInDir(receiptDir, st.runKey, st.stage, st.gen, capture)
}

func interviewFromStatusHistory(history []RunDetailStageStatus, docs []RunDetailStageDocument, at time.Time) []RunDetailInterview {
	var doc string
	if len(docs) > 0 {
		doc = firstRunNonEmpty(docs[0].Content, docs[0].Markdown)
	}
	var out []RunDetailInterview
	seen := map[string]bool{}
	for _, st := range history {
		q := strings.TrimSpace(st.Instruction)
		if q == "" {
			q = strings.TrimSpace(runDetailStringFromAny(firstAny(st.Raw, "question", "prompt", "description")))
		}
		if q == "" || seen[st.Step+"\x00"+q] {
			continue
		}
		seen[st.Step+"\x00"+q] = true
		answer := "The answer is reflected in the final document."
		if strings.TrimSpace(doc) != "" {
			answer = doc
		}
		out = append(out, RunDetailInterview{Step: st.Step, Question: q, Answer: answer, AnsweredAt: firstRunNonEmpty(st.At, at.Format(time.RFC3339Nano)), Source: "spektacular status"})
	}
	return out
}

func appendDistinctStageStatus(history []RunDetailStageStatus, statuses ...RunDetailStageStatus) []RunDetailStageStatus {
	for _, final := range statuses {
		key := stageStatusCaptureKey(final)
		found := false
		for _, item := range history {
			itemKey := stageStatusCaptureKey(item)
			if itemKey == key {
				found = true
				break
			}
		}
		if !found {
			history = append(history, final)
		}
	}
	return history
}

func stageStatusCaptureKey(st RunDetailStageStatus) string {
	raw, _ := json.Marshal(st.Raw)
	return st.Step + "\x00" + st.Instruction + "\x00" + st.DocumentStatus + "\x00" + strings.Join(st.CompletedSteps, "\x00") + "\x00" + string(raw)
}

func (e *SpekHubExecutor) recordStageProgress(st spekHubStage, event string, attrs map[string]string) {
	if e == nil || e.Server == nil {
		return
	}
	pid := 0
	if raw := strings.TrimSpace(attrs["pid"]); raw != "" {
		pid, _ = strconv.Atoi(raw)
	}
	e.recordActivity(st, event, attrs, pid)
	eventAttrs := map[string]string{
		stageAttrRunKey: st.runKey,
		stageAttrStage:  st.stage,
		stageAttrGen:    strconv.FormatUint(st.gen, 10),
		"event":         event,
		"identity":      e.Identity,
	}
	for k, v := range attrs {
		if v != "" {
			eventAttrs[k] = v
		}
	}
	e.Server.RecordStageProgress(st.runKey, st.taskID, eventAttrs, time.Now())
}

func (e *SpekHubExecutor) recordActivity(st spekHubStage, event string, attrs map[string]string, pid int) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.recordActivityLocked(st, time.Now().UTC(), spekActivityEvent(event, attrs), pid)
}

func (e *SpekHubExecutor) recordActivityLocked(st spekHubStage, at time.Time, event string, pid int) {
	if e.activity == nil {
		e.activity = map[string]runActivitySignal{}
	}
	key := spekActivityKey(st.runKey, st.stage, st.gen)
	sig := e.activity[key]
	if sig.lastEvent == event && (strings.HasPrefix(event, "agent polled status") || strings.HasPrefix(event, "waiting for agent")) {
		return
	}
	if sig.stageStartedAt.IsZero() {
		if run := e.inFlight[e.executionKey(st)]; run != nil {
			sig.stageStartedAt = run.started.UTC()
		} else {
			sig.stageStartedAt = at.UTC()
		}
	}
	sig.lastActivityAt = at.UTC()
	sig.lastEvent = event
	if pid > 0 {
		sig.agentPID = pid
	}
	e.activity[key] = sig
}

func (e *SpekHubExecutor) RunActivitySnapshot(runKey, stage string, gen uint64) (runActivitySignal, bool) {
	if e == nil {
		return runActivitySignal{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sig, ok := e.activity[spekActivityKey(runKey, stage, gen)]
	if !ok {
		return runActivitySignal{}, false
	}
	return sig, true
}

func spekActivityKey(runKey, stage string, gen uint64) string {
	return runKey + "\x1f" + stage + "\x1f" + strconv.FormatUint(gen, 10)
}

func spekActivityEvent(event string, attrs map[string]string) string {
	switch event {
	case "status_poll":
		status := firstRunNonEmpty(attrs[stageAttrDocumentStatus], "running")
		step := strings.TrimSpace(attrs[stageAttrCurrentStep])
		if step != "" {
			return "agent polled status (" + status + ", step " + step + ")"
		}
		return "agent polled status (" + status + ")"
	case "cli_launched":
		if pid := strings.TrimSpace(attrs["pid"]); pid != "" {
			return "agent process started (pid " + pid + ")"
		}
		return "agent process started"
	case "cli_launching":
		return "agent launching"
	case "worktree_prepared":
		return "worktree prepared"
	case "document_already_final":
		return "document already final"
	case "document_status":
		status := firstRunNonEmpty(attrs[stageAttrDocumentStatus], "updated")
		return "document status " + status
	case "cli_exited":
		return "agent CLI exited (" + firstRunNonEmpty(attrs["exit_code"], "unknown") + ")"
	case "artifact_resolved":
		return "artifact resolved"
	case "artifact_missing":
		stage := firstRunNonEmpty(attrs[stageAttrStage], "stage")
		return "waiting for agent to create the " + stage + " artifact"
	default:
		event = strings.TrimSpace(strings.ReplaceAll(event, "_", " "))
		if event == "" {
			return "spektacular activity observed"
		}
		return event
	}
}

func isSpekArtifactNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "artifact_not_found")
}

type spekHubArtifactStatus struct {
	Name           string         `json:"name"`
	ArtifactID     string         `json:"artifact_id"`
	DocumentStatus string         `json:"document_status"`
	CurrentStep    string         `json:"current_step"`
	CompletedSteps []string       `json:"completed_steps"`
	Raw            map[string]any `json:"-"`
}

func (s spekHubArtifactStatus) JoinKey() string {
	if strings.TrimSpace(s.ArtifactID) != "" {
		return strings.TrimSpace(s.ArtifactID)
	}
	return strings.TrimSpace(s.Name)
}

func (s spekHubArtifactStatus) Instruction() string {
	return firstRunNonEmpty(
		runDetailStringFromAny(firstAny(s.Raw, "instruction", "question", "prompt", "current_instruction", "step_instruction")),
		nestedStatusString(s.Raw, "current_step", "instruction"),
		nestedStatusString(s.Raw, "step", "instruction"),
		nestedStatusString(s.Raw, "current_step", "question"),
		nestedStatusString(s.Raw, "step", "question"),
	)
}

func nestedStatusString(raw map[string]any, key, nested string) string {
	if raw == nil {
		return ""
	}
	if m, ok := raw[key].(map[string]any); ok {
		return runDetailStringFromAny(m[nested])
	}
	return ""
}

func (e *SpekHubExecutor) spekStatus(ctx context.Context, worktree string, env []string, kind, artifact string) (spekHubArtifactStatus, error) {
	out, err := e.runner()(ctx, worktree, env, e.binary(), kind, "status", artifact)
	if err != nil {
		return spekHubArtifactStatus{}, fmt.Errorf("spektacular %s status %s: %w: %s", kind, artifact, err, tailString(scrubSpekHubOutput(string(out)), spekHubOutputTailBytes))
	}
	var st spekHubArtifactStatus
	if err := json.Unmarshal(bytes.TrimSpace(out), &st); err != nil {
		return spekHubArtifactStatus{}, err
	}
	_ = json.Unmarshal(bytes.TrimSpace(out), &st.Raw)
	return st, nil
}

// finalArtifact reports whether the stage's artifact already exists in the
// worktree with document_status final.
func (e *SpekHubExecutor) finalArtifact(ctx context.Context, worktree string, env []string, kind, artifact string) (spekHubArtifactStatus, bool) {
	if kind != StageSpec && kind != StagePlan {
		return spekHubArtifactStatus{}, false
	}
	resolved := resolveSpekArtifactFromFiles(worktree, kind, artifact)
	if resolved == "" {
		return spekHubArtifactStatus{}, false
	}
	status, err := e.spekStatus(ctx, worktree, env, kind, resolved)
	if err != nil {
		return spekHubArtifactStatus{}, false
	}
	return status, strings.EqualFold(status.DocumentStatus, "final")
}

// holdGeneration stops Tick from relaunching this stage generation; the
// key changes as soon as the lease advances or a new generation is minted.
func (e *SpekHubExecutor) holdGeneration(st spekHubStage) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.holdLocked(e.executionKey(st))
}

func (e *SpekHubExecutor) holdLocked(key string) {
	if e.held == nil {
		e.held = map[string]bool{}
	}
	e.held[key] = true
}

func (e *SpekHubExecutor) recordNonFinal(st spekHubStage, taskID string, status spekHubArtifactStatus) {
	e.mu.Lock()
	e.holdLocked(e.executionKey(st))
	e.lastError = spekHubNonFinalReason
	e.mu.Unlock()
	attrs := map[string]string{
		stageAttrRunKey:         st.runKey,
		stageAttrStage:          st.stage,
		stageAttrGen:            strconv.FormatUint(st.gen, 10),
		stageAttrReason:         spekHubNonFinalReason,
		stageAttrArtifact:       status.JoinKey(),
		stageAttrDocumentStatus: status.DocumentStatus,
		stageAttrCurrentStep:    status.CurrentStep,
		"waiting_on":            worksource.RunWaitingOnHuman,
	}
	e.Server.AgentAuditSink().Record("system", agent.AuditLeaseStageRefused, taskID, agent.Fields("run", st.runKey, "stage", st.stage, "gen", st.gen, "reason", spekHubNonFinalReason, "artifact", status.JoinKey(), "document_status", status.DocumentStatus))
	e.Server.LifecycleTimeline().Record(timeline.Event{IssueRef: st.runKey, Kind: timeline.KindBlocked, At: time.Now().UnixMilli(), Attrs: attrs})
}

func resolveSpekArtifactFromFiles(worktree, kind, slug string) string {
	slug = sanitizeRunPromptPath(slug)
	rootName := "specs"
	if kind == StagePlan {
		rootName = "plans"
	}
	root := filepath.Join(worktree, ".spektacular", rootName)
	type candidate struct {
		id string
		mt time.Time
	}
	var matches []candidate
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		id := bareArtifactName(filepath.ToSlash(rel))
		if id == slug || strings.HasSuffix(id, "-"+slug) {
			mt := time.Time{}
			if info, statErr := d.Info(); statErr == nil {
				mt = info.ModTime()
			}
			matches = append(matches, candidate{id: id, mt: mt})
		}
		return nil
	})
	if len(matches) == 0 {
		return ""
	}
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].mt.Equal(matches[j].mt) {
			return matches[i].mt.After(matches[j].mt)
		}
		return matches[i].id > matches[j].id
	})
	return matches[0].id
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func (e *SpekHubExecutor) startRenewing(ctx context.Context, taskID string) func() {
	done := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(spekHubLeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := e.Server.contributeHub.renewLease(e.Identity, taskID, time.Now().UTC()); err != nil {
					e.log().Warn("[spektacular] hub executor lease renew failed", "task", taskID, "error", err)
				}
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() { close(done); <-joined }
}

func (e *SpekHubExecutor) prepareWorkspace(ctx context.Context, st spekHubStage) (string, error) {
	repoDir := filepath.Join(currentAgentWorkspaceRoot(), e.Identity, filepath.FromSlash(st.repo))
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
		return "", err
	}
	authArgs, appToken, cleanup, err := e.cloneAuthArgs(ctx, st.repo, filepath.Dir(repoDir))
	if err != nil {
		return "", err
	}
	defer cleanup()
	runGit := func(dir string, args ...string) error {
		full := append([]string{}, authArgs...)
		full = append(full, args...)
		out, err := e.runner()(ctx, dir, spekGitEnv(os.Environ()), "git", full...)
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, tailString(string(out), spekHubOutputTailBytes))
		}
		return nil
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		if err := runGit(filepath.Dir(repoDir), "clone", "--no-checkout", e.cloneURL(st.repo), filepath.Base(repoDir)); err != nil {
			return "", err
		}
	}
	if err := runGit(repoDir, "fetch", "origin", "HEAD"); err != nil {
		return "", err
	}
	worktree := spekHubRunWorktreePath(e.Identity, st.runKey)
	if _, err := os.Stat(worktree); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
			return "", err
		}
		// A swept directory can leave git with a "missing but already
		// registered" worktree entry that makes `worktree add` refuse.
		if err := runGit(repoDir, "worktree", "prune"); err != nil {
			e.log().Warn("[spektacular] git worktree prune failed", "repo", repoDir, "error", err)
		}
		if err := runGit(repoDir, "worktree", "add", "--detach", worktree, "FETCH_HEAD"); err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(filepath.Join(worktree, ".spektacular")); errors.Is(err, os.ErrNotExist) {
		if copied, copyErr := copyPreviousSpektacularProject(e.Identity, st.runKey, worktree); copyErr != nil {
			e.log().Warn("[spektacular] copying previous Spektacular project failed", "run", st.runKey, "worktree", worktree, "error", copyErr)
		} else if copied {
			return appToken, nil
		}
		if _, err := e.runner()(ctx, worktree, spekGitEnv(os.Environ()), e.binary(), "init", spekInitAgent(e.backend()), "--name", filepath.Base(st.repo)); err != nil {
			return "", err
		}
	}
	return appToken, nil
}

func copyPreviousSpektacularProject(identity, runKey, worktree string) (bool, error) {
	runRoot := filepath.Join(currentAgentWorkspaceRoot(), identity, "runs", sanitizeRunPromptPath(runKey))
	entries, err := os.ReadDir(runRoot)
	if err != nil {
		return false, nil
	}
	type candidate struct {
		path string
		mt   time.Time
	}
	var candidates []candidate
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "work" {
			continue
		}
		src := filepath.Join(runRoot, entry.Name(), ".spektacular")
		info, statErr := os.Stat(src)
		if statErr == nil && info.IsDir() {
			candidates = append(candidates, candidate{path: src, mt: info.ModTime()})
		}
	}
	if len(candidates) == 0 {
		return false, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mt.After(candidates[j].mt) })
	return true, copyDir(candidates[0].path, filepath.Join(worktree, ".spektacular"))
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info, statErr := d.Info(); statErr == nil {
			mode = info.Mode().Perm()
		}
		return os.WriteFile(target, data, mode)
	})
}

func (e *SpekHubExecutor) cloneAuthArgs(ctx context.Context, repo, dir string) ([]string, string, func(), error) {
	if e.CloneAuth == nil {
		return nil, "", func() {}, nil
	}
	return e.CloneAuth(ctx, repo, dir)
}

func (e *SpekHubExecutor) executorEnv(appToken string) ([]string, error) {
	home := filepath.Join(currentAgentWorkspaceRoot(), e.Identity, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, err
	}
	env := allowedSpekHubExecutorEnv(os.Environ())
	env = spekGitEnv(env)
	env = append(env, "HOME="+home, "npm_config_cache="+filepath.Join(home, ".npm-cache"))
	creds, err := agent.HeadlessCredentialEnv(e.backend())
	if err != nil {
		e.log().Warn("[spektacular] headless credential env unavailable", "backend", e.backend(), "error", err)
	} else {
		env = append(env, creds...)
	}
	if tok := strings.TrimSpace(appToken); tok != "" {
		env = append(env, "GH_TOKEN="+tok, "GITHUB_TOKEN="+tok)
	}
	return env, nil
}

func allowedSpekHubExecutorEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if spekHubExecutorEnvAllowed(key) {
			out = append(out, entry)
		}
	}
	return out
}

func spekHubExecutorEnvAllowed(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	switch upper {
	case "PATH", "LANG", "LANGUAGE", "TERM", "TZ", "NPM_CONFIG_CACHE":
		return true
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		return true
	}
	return strings.HasPrefix(upper, "LC_")
}

// spekGitEnv marks the executor's own workspace root as a safe git directory.
// The hub process may run as a different uid than the one that originally
// created the shared clone (e.g. after an image change), and git otherwise
// refuses every command there with "detected dubious ownership".
func spekGitEnv(env []string) []string {
	env = filteredSpekEnv(env, "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0")
	return append(env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*")
}

func filteredSpekEnv(env []string, keys ...string) []string {
	block := map[string]bool{}
	for _, key := range keys {
		block[key] = true
	}
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !block[key] {
			out = append(out, entry)
		}
	}
	return out
}

func (e *SpekHubExecutor) recordFailure(st spekHubStage, key string, err error) {
	msg := spekHubLastErrorSummary(err.Error())
	e.mu.Lock()
	e.holdLocked(key)
	e.lastError = msg
	e.mu.Unlock()
	attrs := map[string]string{
		stageAttrRunKey: st.runKey,
		stageAttrStage:  st.stage,
		stageAttrGen:    strconv.FormatUint(st.gen, 10),
		stageAttrReason: spekHubFailureReason,
		"waiting_on":    worksource.RunWaitingOnHuman,
	}
	e.Server.AgentAuditSink().Record("system", agent.AuditLeaseStageRefused, st.taskID, agent.Fields("run", st.runKey, "stage", st.stage, "gen", st.gen, "reason", spekHubFailureReason, "error", msg))
	e.Server.LifecycleTimeline().Record(timeline.Event{IssueRef: st.runKey, Kind: timeline.KindBlocked, At: time.Now().UnixMilli(), Attrs: attrs})
	e.log().Warn("[spektacular] hub executor failed", "run", st.runKey, "stage", st.stage, "gen", st.gen, "error", msg)
}

// settleGeneration spends one generation of the stage budget
// (runs.max_stage_retries) once a generation has ended without a final
// document — the agent CLI failed, or exited with the document still draft —
// and either mints the retry generation Tick launches next or raises the
// decision escalation (#9143). The executor is the budget's only owner: it is
// the one component that knows a generation is spent. Leases it holds are kept
// alive by keepPendingStageLeasesAlive, so waiting for them to lapse would
// never settle anything.
func (e *SpekHubExecutor) settleGeneration(st spekHubStage, reason string) {
	hub := e.Server.contributeHub
	if hub == nil {
		return
	}
	now := time.Now().UTC()
	budget := e.budget()
	identity, taskID := e.Identity, e.stageTaskID(st)
	outcome, lease, attempts, err := hub.settleStageGeneration(identity, taskID, st.gen, budget, now)
	if errors.Is(err, errLeaseNotFound) && st.identity != identity {
		// The claim itself failed, so the spent generation is still held by
		// the placeholder the executor was claiming.
		identity, taskID = st.identity, st.taskID
		outcome, lease, attempts, err = hub.settleStageGeneration(identity, taskID, st.gen, budget, now)
	}
	if err != nil {
		e.log().Warn("[spektacular] settling spent stage generation failed", "run", st.runKey, "stage", st.stage, "gen", st.gen, "task", taskID, "error", err)
		return
	}
	switch outcome {
	case stageSettleRetried:
		e.recordStageProgress(st, "retry_generation_minted", map[string]string{
			stageAttrReason:   reason,
			stageAttrAttempts: strconv.Itoa(attempts + 1),
			stageAttrBudget:   strconv.Itoa(budget),
			"next_gen":        strconv.FormatUint(lease.gen, 10),
		})
		e.log().Info("[spektacular] stage generation spent without final; retry generation minted",
			"run", st.runKey, "stage", st.stage, "gen", st.gen, "next_gen", lease.gen, "attempt", attempts+1, "budget", budget, "reason", reason)
	case stageSettleEscalated:
		e.Server.escalateStageLease(st.runKey, now, map[string]string{
			stageAttrStage:    st.stage,
			stageAttrGen:      strconv.FormatUint(st.gen, 10),
			stageAttrAttempts: strconv.Itoa(attempts),
			stageAttrBudget:   strconv.Itoa(budget),
			stageAttrSeverity: stageEscalationSeverity,
			stageAttrReason:   fmt.Sprintf("%d of %d stage generations spent without document_status final (last: %s)", attempts, budget, reason),
			"waiting_on":      worksource.RunWaitingOnHuman,
		})
	}
}

func (e *SpekHubExecutor) unclaimedStages() ([]spekHubStage, error) {
	out := []spekHubStage{}
	err := e.Server.VisitActiveStageLeases(func(runKey, key, stage, identity, taskID, repo string, gen uint64, expiresAt time.Time) {
		if identity != runAdmissionIdentity && identity != worksource.RunAdmissionIdentity && identity != e.Identity {
			return
		}
		if e.Server.runCheckpointStageHeld(runKey, stage, gen) {
			return
		}
		number := 0
		if ref, ok := worksource.ParseKey(runKey); ok {
			number = ref.Number
		}
		out = append(out, spekHubStage{runKey: runKey, key: key, stage: stage, identity: identity, taskID: taskID, repo: repo, number: number, gen: gen})
	})
	// An escalated generation spent the stage budget; it waits for a person,
	// across restarts too (the flag is persisted on the lease).
	live := out[:0]
	for _, st := range out {
		if e.Server.contributeHub.stageLeaseEscalated(st.identity, st.taskID) {
			continue
		}
		st.title = e.Server.stageLeaseTitle(st.identity, st.taskID)
		st.workItem = e.Server.workItemContextForRun(st.runKey)
		live = append(live, st)
	}
	return live, err
}

// stageStillActive reports whether some active lease on st's run key is
// still at st.stage / st.gen.
func (e *SpekHubExecutor) stageStillActive(st spekHubStage) bool {
	active := false
	if err := e.Server.VisitActiveStageLeases(func(runKey, _, stage, _, _, _ string, gen uint64, _ time.Time) {
		if runKey == st.runKey && stage == st.stage && gen == st.gen {
			active = true
		}
	}); err != nil {
		return false
	}
	return active
}

func (s *Server) stageLeaseTitle(identity, taskID string) string {
	if s == nil || s.contributeHub == nil {
		return ""
	}
	s.contributeHub.leaseMu.Lock()
	defer s.contributeHub.leaseMu.Unlock()
	if l := s.contributeHub.leaseForLocked(identity, taskID); l != nil {
		return l.title
	}
	return ""
}

func (e *SpekHubExecutor) executionKey(st spekHubStage) string {
	return e.executionKeyPrefix(st.runKey, st.stage) + strconv.FormatUint(st.gen, 10)
}

func (e *SpekHubExecutor) executionKeyPrefix(runKey, stage string) string {
	return strings.TrimSpace(runKey) + "\x1f" + strings.TrimSpace(stage) + "\x1f"
}

// budget is runs.max_stage_retries: how many generations one stage may spend,
// the first included, before settleGeneration escalates instead of retrying.
// Each generation is launched once; a launch failure spends it like a
// non-final exit does.
func (e *SpekHubExecutor) budget() int { return e.Config.MaxStageRetriesOrDefault() }

// stageTaskID is the task the executor holds st under: the lease's own task
// when the executor already owns it, else the task it claims st as.
func (e *SpekHubExecutor) stageTaskID(st spekHubStage) string {
	if st.identity == e.Identity && strings.TrimSpace(st.taskID) != "" {
		return st.taskID
	}
	return spekHubExecutorTaskPrefix + sanitizeReceiptSegment(st.runKey) + "-" + st.stage + "-" + strconv.FormatUint(st.gen, 10)
}

func (e *SpekHubExecutor) maxConcurrent() int {
	return e.Config.Spektacular.HubExecutor.MaxConcurrentOrDefault()
}

func (e *SpekHubExecutor) runningLocked() int {
	n := 0
	for range e.inFlight {
		n++
	}
	return n
}

func (e *SpekHubExecutor) setLastError(msg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastError = spekHubLastErrorSummary(msg)
}

func scrubSpekHubOutput(text string) string {
	return redactTokens(logscrub.ScrubString(text, logscrub.WithMarkers()))
}

func spekHubLastErrorSummary(msg string) string {
	msg = scrubSpekHubOutput(msg)
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) <= spekHubLastErrorMaxBytes {
		return msg
	}
	out := msg[:spekHubLastErrorMaxBytes]
	for len(out) > 0 && !utf8.ValidString(out) {
		out = out[:len(out)-1]
	}
	return out
}

func (e *SpekHubExecutor) binary() string {
	return e.Config.Spektacular.BinaryOrDefault()
}

func (e *SpekHubExecutor) stagePrompt(st spekHubStage, artifact string) string {
	return spekHubStagePromptWithBinary(st.stage, st.repo, st.number, st.runKey, st.title, artifact, st.workItem, e.binary())
}

func (e *SpekHubExecutor) runner() func(context.Context, string, []string, string, ...string) ([]byte, error) {
	if e.Exec != nil {
		return e.Exec
	}
	return func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		var buf bytes.Buffer
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = env
		spekHubConfigureProcessGroup(cmd)
		spekHubInheritFence(cmd, ctx)
		cmd.WaitDelay = spekHubWaitDelay
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		return buf.Bytes(), err
	}
}

func (e *SpekHubExecutor) cloneURL(repo string) string {
	if e.CloneURL != nil {
		return e.CloneURL(repo)
	}
	return "https://github.com/" + strings.TrimSpace(repo) + ".git"
}

func (e *SpekHubExecutor) artifact(runKey string) string {
	if e.Artifact != nil {
		return e.Artifact(runKey)
	}
	return sanitizeRunPromptPath(runKey)
}

func (e *SpekHubExecutor) backend() string {
	if b := strings.TrimSpace(e.Backend); b != "" {
		return b
	}
	return config.DefaultSpektacularHubExecutorBackend
}

func (e *SpekHubExecutor) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

func writeSpekHubPrompt(worktree, prompt string) error {
	path := filepath.Join(worktree, spekHubPromptRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(prompt), 0o600)
}

func SpekHubStagePrompt(stage, repo string, number int, runKey, title, artifact string) string {
	return SpekHubStagePromptWithContext(stage, repo, number, runKey, title, artifact, worksource.WorkItemContextFromRef(worksource.Ref{Repo: repo, Number: number}, title))
}

func SpekHubStagePromptWithContext(stage, repo string, number int, runKey, title, artifact string, item worksource.WorkItemContext) string {
	return spekHubStagePromptWithBinary(stage, repo, number, runKey, title, artifact, item, config.DefaultSpektacularBinary)
}

func spekHubStagePromptWithBinary(stage, repo string, number int, runKey, title, artifact string, item worksource.WorkItemContext, binary string) string {
	// Quote custom executable names for the shell, including paths with spaces.
	cli := binary
	if binary != config.DefaultSpektacularBinary {
		cli = "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
	}
	item = item.Normalized()
	if item.Repo == "" {
		item.Repo = repo
	}
	if item.Number == 0 {
		item.Number = number
	}
	if item.Title == "" {
		item.Title = title
	}
	item = item.Normalized()
	issue := runKey
	if item.Repo != "" && item.Number > 0 {
		issue = item.Repo + "#" + strconv.Itoa(item.Number)
	}
	title = strings.TrimSpace(firstSpekNonEmpty(item.Title, title))
	titleText := ""
	if title != "" {
		titleText = " " + strconv.Quote(title)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Hive-Run: %s\n\n", runKey)
	sourceType := strings.TrimSpace(item.SourceType)
	if sourceType == "" {
		sourceType = "github"
	}
	if item.Number <= 0 || (sourceType != "github" && sourceType != "github_projects") {
		fmt.Fprintf(&b, "Source work item: %s %s%s\nTarget repository: %s\n", sourceType, firstSpekNonEmpty(item.ExternalID, runKey), titleText, item.Repo)
		if item.URL != "" {
			fmt.Fprintf(&b, "Source URL: %s\n", item.URL)
		}
		if item.Body != "" {
			fmt.Fprintf(&b, "Source description:\n%s\n\n", item.Body)
		} else {
			b.WriteString("\n")
		}
		switch stage {
		case StagePlan:
			fmt.Fprintf(&b, "You are authoring a Spektacular plan for the source work item above in this repository checkout. Use the target repository %s for code changes and PRs. Use the `%s` CLI configured for this run: run `%s plan new --data '{\"name\":\"%s\",\"spec\":\"%s\"}'`, then follow each step it returns (`%s plan status %s` shows the current step and its instruction; `%s plan file ...` reads/writes the plan document) until the plan's `document_status` is `final`. Do not implement code, do not commit, do not push, do not open PRs. Stop when `%s plan status %s` reports `document_status: final`.", item.Repo, cli, cli, artifact, artifact, cli, artifact, cli, cli, artifact)
		default:
			fmt.Fprintf(&b, "You are authoring a Spektacular spec for the source work item above in this repository checkout. Use the target repository %s for code changes and PRs. Use the `%s` CLI configured for this run: run `%s spec new --data '{\"name\":\"%s\"}'`, then follow each step it returns (`%s spec status %s` shows the current step and its instruction; `%s spec file ...` reads/writes the spec document) until the spec's `document_status` is `final`. Do not implement code, do not commit, do not push, do not open PRs. Stop when `%s spec status %s` reports `document_status: final`.", item.Repo, cli, cli, artifact, cli, artifact, cli, cli, artifact)
		}
		return b.String()
	}
	switch stage {
	case StagePlan:
		fmt.Fprintf(&b, "You are authoring a Spektacular plan for GitHub issue %s%s in this repository checkout. Read the issue with `gh issue view %d --repo %s` (if `gh` is available; otherwise use the GitHub API) and the relevant code. Use the `%s` CLI configured for this run: run `%s plan new --data '{\"name\":\"%s\",\"spec\":\"%s\"}'`, then follow each step it returns (`%s plan status %s` shows the current step and its instruction; `%s plan file ...` reads/writes the plan document) until the plan's `document_status` is `final`. Do not implement code, do not commit, do not push, do not open PRs. Stop when `%s plan status %s` reports `document_status: final`.", issue, titleText, number, repo, cli, cli, artifact, artifact, cli, artifact, cli, cli, artifact)
	default:
		fmt.Fprintf(&b, "You are authoring a Spektacular spec for GitHub issue %s%s in this repository checkout. Read the issue with `gh issue view %d --repo %s` (if `gh` is available; otherwise use the GitHub API) and the relevant code. Use the `%s` CLI configured for this run: run `%s spec new --data '{\"name\":\"%s\"}'`, then follow each step it returns (`%s spec status %s` shows the current step and its instruction; `%s spec file ...` reads/writes the spec document) until the spec's `document_status` is `final`. Do not implement code, do not commit, do not push, do not open PRs. Stop when `%s spec status %s` reports `document_status: final`.", issue, titleText, number, repo, cli, cli, artifact, cli, artifact, cli, cli, artifact)
	}
	return b.String()
}

func spekInitAgent(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "bob":
		return "bob"
	case "codex", "copilot":
		return "codex"
	default:
		return "claude"
	}
}

func tailString(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func firstSpekNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Keep a bounded diagnostic tail in memory; the scrubbed full transcript is
// streamed to disk by the other MultiWriter destination.
type spekHubTailWriter struct{ tail []byte }

func (w *spekHubTailWriter) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= spekHubOutputTailBytes {
		w.tail = append(w.tail[:0], p[len(p)-spekHubOutputTailBytes:]...)
	} else {
		if excess := len(w.tail) + len(p) - spekHubOutputTailBytes; excess > 0 {
			copy(w.tail, w.tail[excess:])
			w.tail = w.tail[:len(w.tail)-excess]
		}
		w.tail = append(w.tail, p...)
	}
	return n, nil
}
func (w *spekHubTailWriter) String() string { return string(w.tail) }

package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalate"
	"github.com/hivecommons/hive/pkg/hooks"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/spektacular"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

const (
	spekRunKey   = "20260922132517-hcl-encoding-helpers"
	spekRepo     = "myorg/repo1"
	spekIdentity = config.DefaultSpektacularHubExecutorIdentity
	spekTaskID   = "task-spek"
	spekGen      = uint64(11)
	spekPoll     = time.Minute
)

// spekExec answers status calls from a per-kind queue (the last entry
// repeats) and plan export calls with a fixed three-task plan.
type spekExec struct {
	queues map[string][]string
	idx    map[string]int
	calls  int
}

func newSpekExec() *spekExec {
	return &spekExec{queues: map[string][]string{}, idx: map[string]int{}}
}

func (e *spekExec) push(kind string, statuses ...spektacular.DocumentStatus) {
	for _, st := range statuses {
		// The exact jumppad-labs/spektacular#45 shape: error:false envelope,
		// closed_at "" while open, no updated_at once the document is closed
		// and no workflow state matches it.
		closed, updated := `""`, `,"updated_at":"2026-09-22T13:30:00Z"`
		if st == spektacular.DocumentFinal {
			closed, updated = `"2026-09-22T00:00:00Z"`, ``
		}
		e.queues[kind] = append(e.queues[kind], fmt.Sprintf(
			`{"error":false,"kind":%q,"name":%q,"document_status":%q,"current_step":"authoring","completed_steps":["interview"],"created_at":"2026-09-21T00:00:00Z"%s,"closed_at":%s,"spec":"","plan":""}`,
			kind, spekRunKey, st, updated, closed))
	}
}

func (e *spekExec) exec(_ context.Context, _ string, args []string) ([]byte, error) {
	e.calls++
	if len(args) > 3 && !(len(args) == 5 && args[1] == "export" && args[3] == "--format" && args[4] == "json") {
		// The CLI has no --json flag; unexpected extra status args are usage errors.
		return []byte(`{"error":true,"code":"usage","message":"unknown flag: ` + args[3] + `"}`), errors.New("exit status 64")
	}
	if len(args) >= 2 && args[1] == "export" {
		return []byte(`{"error":false,"kind":"plan","name":"` + spekRunKey + `","tasks":[{"ref":"T1","title":"Add encoding helpers"},{"ref":"T2","title":"Wire helpers","depends_on":["T1"]},{"ref":"T3","title":"Sign off","depends_on":["T2"],"execution":"human_required"}]}`), nil
	}
	q := e.queues[args[0]]
	if len(q) == 0 {
		return []byte(`{"error":true,"code":"artifact_not_found","message":"` + args[0] + ` artifact \"` + args[2] + `\" was not found","resource":"` + args[2] + `","next_action":"run spektacular ` + args[0] + ` file list"}`), errors.New("exit status 1")
	}
	i := e.idx[args[0]]
	if i >= len(q) {
		i = len(q) - 1
	}
	e.idx[args[0]]++
	return []byte(q[i]), nil
}

func spekHub(t *testing.T) (*ContributeWSHub, *Server, *beads.Store, *hookCapture) {
	t.Helper()
	stubSpekHubExecutorCredential(t)
	// The lifecycle timeline is a process-wide singleton and every test here
	// drives the SAME lease key (spekRepo!spekRunKey:<stage>), so its
	// per-(ref,kind) Stage.Count accumulates across tests and receipt
	// assertions read other tests' receipts under -shuffle. Reset it per test,
	// the package idiom from api_lifecycle_timeline_test.go.
	resetLifecycleStore()
	hub, s := covK2Hub(t)
	s.contributeHub = hub
	setAgentWorkspaceRootForTest(t, t.TempDir())
	old := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	t.Cleanup(func() { runReceiptsDir = old })
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("bead store: %v", err)
	}
	s.deps.BeadStores = map[string]*beads.Store{planning.ArchitectAgentName: store}
	disableSpekHubExecutorForRelayTests(s)
	capture := &hookCapture{}
	s.deps.HookFire = capture.fire
	return hub, s, store, capture
}

func stubSpekHubExecutorCredential(t *testing.T) {
	t.Helper()
	t.Setenv("COPILOT_GITHUB_TOKEN", "ghu_spek_hub_test")
}

func disableSpekHubExecutorForRelayTests(s *Server) {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return
	}
	off := false
	s.deps.Config.Runs.Spektacular.HubExecutor.Enabled = &off
}

func spekRunner(hub *ContributeWSHub, ex *spekExec) *spektacular.Runner {
	return &spektacular.Runner{
		Exec:     ex.exec,
		Poll:     spekPoll,
		Registry: spektacular.NewLeaseRegistryAdapter(hub.server),
		Logger:   hub.logger,
	}
}

// spekStageRunner installs a *spektacular.Runner as the Server's StageRunner.
type spekStageRunner struct{ r *spektacular.Runner }

func (s spekStageRunner) Tick(ctx context.Context, now time.Time) { s.r.Tick(ctx, now) }

func spekLease(t *testing.T, hub *ContributeWSHub, stage string, now time.Time) {
	t.Helper()
	key := spekRepo + "!" + spekRunKey + ":" + stage
	if err := hub.recordLeaseForKeyStage(spekIdentity, spekTaskID, spekRepo, 0, key, "contributor", stage, spekGen, now); err != nil {
		t.Fatalf("record stage lease: %v", err)
	}
	if err := os.MkdirAll(runStageWorktreePath(spekIdentity, spekRunKey, stage, spekGen), 0o755); err != nil {
		t.Fatalf("create run worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(currentAgentWorkspaceRoot(), spekIdentity, filepath.FromSlash(spekRepo)), 0o755); err != nil {
		t.Fatalf("create shared checkout: %v", err)
	}
}

func spekLeaseState(hub *ContributeWSHub) (string, uint64) {
	hub.leaseMu.Lock()
	defer hub.leaseMu.Unlock()
	l := hub.leaseForLocked(spekIdentity, spekTaskID)
	if l == nil {
		return "", 0
	}
	return l.stage, l.gen
}

func spekListed(t *testing.T, s *Server) []worksource.Issue {
	t.Helper()
	issues, err := worksource.NewRunStageSource(s.RunStageAccessor()).ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	return issues
}

func auditActions(t *testing.T, hub *ContributeWSHub) []string {
	t.Helper()
	var out []string
	for _, e := range srvAuditEntries(t, hub) {
		out = append(out, e.Action)
	}
	return out
}

// TestSpektacularRunner_LeaseWorksourceHookIntegration drives a run from spec
// to implement through the real lease registry (#8297), the run-stage work
// source (#8296), and the stage_completed hook (#8298): draft leaves the lease
// alone; final writes one receipt, advances once, fires the hook, and lists
// the next stage; a final plan is admitted as a draft Hive plan and implement
// stays unlisted until ApprovePlan.
func TestSpektacularRunner_LeaseWorksourceHookIntegration(t *testing.T) {
	hub, s, store, capture := spekHub(t)
	off := false
	s.deps.Config.Runs.Checkpoints.Spec = &off
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	ex := newSpekExec()
	ex.push(spektacular.KindSpec, spektacular.DocumentDraft, spektacular.DocumentFinal)
	ex.push(spektacular.KindPlan, spektacular.DocumentFinal)
	r := spekRunner(hub, ex)

	// Draft: nothing moves, the spec stage is the offerable item.
	if res := r.Tick(context.Background(), now); res.Advanced != 0 || res.Polled != 1 {
		t.Fatalf("draft tick = %+v", res)
	}
	if stage, gen := spekLeaseState(hub); stage != StageSpec || gen != spekGen {
		t.Fatalf("lease after draft = %s/%d", stage, gen)
	}
	if listed := spekListed(t, s); len(listed) != 1 || listed[0].Stage != StageSpec || listed[0].ExternalID != spekRunKey+":"+StageSpec {
		t.Fatalf("listed after draft = %+v", listed)
	}

	// Final: exactly one advance, one receipt, one hook, one timeline receipt.
	now = now.Add(spekPoll)
	if res := r.Tick(context.Background(), now); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("final tick = %+v", res)
	}
	stage, gen := spekLeaseState(hub)
	if stage != StagePlan || gen <= spekGen {
		t.Fatalf("lease after final = %s/%d, want plan and gen > %d", stage, gen, spekGen)
	}
	receiptPath := filepath.Join(runReceiptsDir, spekRunKey, fmt.Sprintf("%s-gen%d.json", StageSpec, spekGen))
	raw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("receipt not written: %v", err)
	}
	var receipt outputschema.StageReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Stage != StageSpec || receipt.Generation != spekGen || receipt.WorkKey != spekRepo+"!"+spekRunKey {
		t.Fatalf("receipt = %+v err=%v", receipt, err)
	}
	entries, _ := os.ReadDir(filepath.Join(runReceiptsDir, spekRunKey))
	if len(entries) != 1 {
		t.Fatalf("receipts written = %d, want exactly one", len(entries))
	}
	var stageHooks []hooks.Payload
	for _, p := range capture.all() {
		if p.Transition == hooks.TransitionStageCompleted {
			stageHooks = append(stageHooks, p)
		}
	}
	if len(stageHooks) != 1 || stageHooks[0].StageFrom != StageSpec || stageHooks[0].StageTo != StagePlan || stageHooks[0].Run != spekTaskID || stageHooks[0].Gen != gen {
		t.Fatalf("stage_completed hooks = %+v", stageHooks)
	}
	journeyEvents := s.LifecycleTimeline().ByIssue(spekRepo + "!" + spekRunKey + ":" + StageSpec)
	var receiptEvents, completedEvents int
	for _, ev := range journeyEvents {
		switch ev.Kind {
		case timeline.KindStageReceipt:
			receiptEvents++
			if ev.Attrs["receipt"] != receipt.OutputDigest || ev.Attrs["path"] != receiptPath || ev.Attrs["stage"] != StageSpec {
				t.Fatalf("receipt event attrs = %+v", ev.Attrs)
			}
		case timeline.KindStageCompleted:
			completedEvents++
		}
	}
	if receiptEvents != 1 || completedEvents != 1 {
		t.Fatalf("timeline receipt=%d completed=%d, want 1/1: %+v", receiptEvents, completedEvents, journeyEvents)
	}
	if actions := auditActions(t, hub); !contains(actions, agentaudit.AuditLeaseStageAdvanced) {
		t.Fatalf("audit actions = %v, want %s", actions, agentaudit.AuditLeaseStageAdvanced)
	}
	if listed := spekListed(t, s); len(listed) != 1 || listed[0].Stage != StagePlan || len(listed[0].DependsOn) != 1 || listed[0].DependsOn[0].Ref.ExternalID != spekRunKey+":"+StageSpec {
		t.Fatalf("listed after spec final = %+v", listed)
	}
	// A repeated final for the spec is never re-advanced: the runner now polls
	// the plan (its own generation), and the receipt count stays at one.
	if res := r.Tick(context.Background(), now); res.Advanced != 0 || res.Polled != 0 {
		t.Fatalf("same-instant tick = %+v", res)
	}

	// Plan final: imported as a DRAFT plan, and the lease stays parked at plan
	// until the checkpoint is approved (hivecommons/hive#8550).
	now = now.Add(spekPoll)
	if res := r.Tick(context.Background(), now); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("plan final tick = %+v", res)
	}
	if stage, _ := spekLeaseState(hub); stage != StagePlan {
		t.Fatalf("lease after plan final = %s, want plan", stage)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("plan final did not create the run epic")
	}
	if epic.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft || epic.Meta(planning.MetaRunKey) != spekRunKey || epic.Meta(planning.MetaIssueRepo) != spekRepo {
		t.Fatalf("epic meta = %+v", epic.Metadata)
	}
	children, err := planning.GetPlanTree(store, epic.ID)
	if err != nil || len(children.Children) != 3 {
		t.Fatalf("plan tree = %+v err=%v", children, err)
	}
	if listed := spekListed(t, s); len(listed) != 0 {
		t.Fatalf("listed before ApprovePlan = %+v, want no relay-offered held checkpoint", listed)
	}
	approvalAt := now.Add(leaseTTL + time.Second)
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if err := s.advanceApprovedPlanLease(spekRunKey, epic.ID, "test-operator", approvalAt); err != nil {
		t.Fatalf("advance approved plan lease: %v", err)
	}
	if listed := spekListed(t, s); len(listed) != 1 || listed[0].Stage != StageImplement {
		t.Fatalf("listed after ApprovePlan = %+v", listed)
	}
	// The implement stage has no Spektacular document: further ticks are inert
	// and the plan is never re-imported.
	if res := r.Tick(context.Background(), now.Add(spekPoll)); res.Polled != 0 || res.Advanced != 0 {
		t.Fatalf("implement tick = %+v", res)
	}
	if again, _ := planning.GetPlanTree(store, epic.ID); len(again.Children) != 3 {
		t.Fatal("plan re-imported")
	}
	if !contains(auditActions(t, hub), agentaudit.AuditLeaseStageAdvanced) {
		t.Fatal("advance audit missing")
	}
	if ex.calls == 0 {
		t.Fatal("exec never called")
	}
}

func TestImportRunPlanFansOutMultiRepoPlanWhenEnabled(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	var gotRunKey string
	var gotRepos []string
	s.deps.RunFanout = func(_ context.Context, runKey string, repos []string) ([]string, error) {
		gotRunKey = runKey
		gotRepos = append([]string(nil), repos...)
		var waveIDs []string
		for i, repo := range repos {
			wave := i + 1
			status := &worksource.SpektacularRunStatus{
				RunKey: runKey,
				Waves: []worksource.RunWaveStatus{{
					Wave:         wave,
					Repositories: []worksource.RunRepositoryStatus{{Repo: repo}},
				}},
			}
			result, err := (worksource.RunFanoutRunner{Status: status, Leases: s}).FanOutWave(context.Background(), runKey, wave)
			if err != nil {
				return nil, err
			}
			if len(result.Created) > 0 {
				waveIDs = append(waveIDs, fmt.Sprintf("wave-%d:%s", wave, repo))
			}
		}
		return waveIDs, nil
	}

	taskList := strings.Join([]string{
		"1. [T1] API changes [repo:acme/api] [agent_suitable]",
		"2. [T2] UI changes [repo:acme/ui] [agent_suitable]",
		"3. [T3] Docs changes [repo:acme/docs] [agent_suitable]",
	}, "\n")
	countFanoutLeases := func() int {
		hub.leaseMu.Lock()
		defer hub.leaseMu.Unlock()
		n := 0
		for _, l := range hub.leases {
			if l.identity == runFanoutIdentity && l.stage == StageImplement {
				n++
			}
		}
		return n
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	// A draft plan is not fanned out: its implement leases would mask the
	// plan checkpoint (hivecommons/hive#10089).
	if gotRepos != nil || countFanoutLeases() != 0 {
		t.Fatalf("draft plan fanned out: repos=%v leases=%d", gotRepos, countFanoutLeases())
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("import did not create epic")
	}
	if got := epic.Meta(planning.MetaRunWaveIDs); got != "" {
		t.Fatalf("draft plan wave ids = %q, want none", got)
	}
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("ImportRunPlan after approval: %v", err)
	}
	if gotRunKey != spekRunKey || !reflect.DeepEqual(gotRepos, []string{"acme/api", "acme/ui", "acme/docs"}) {
		t.Fatalf("fanout got run=%q repos=%v", gotRunKey, gotRepos)
	}
	if n := countFanoutLeases(); n != 3 {
		t.Fatalf("fanout leases = %d, want 3", n)
	}
	_, epic = s.findRunEpic(spekRunKey)
	if got := epic.Meta(planning.MetaRunWaveIDs); got != "wave-1:acme/api,wave-2:acme/ui,wave-3:acme/docs" {
		t.Fatalf("wave ids = %q", got)
	}
	if err := hub.recordLeaseForKeyStage(spekIdentity, spekTaskID, spekRepo, 0, spekRepo+"!"+spekRunKey+":"+StageImplement, "contributor", StageImplement, spekGen, time.Now()); err != nil {
		t.Fatalf("record implement lease: %v", err)
	}
	runs, err := s.activeRuns(false)
	if err != nil {
		t.Fatalf("activeRuns: %v", err)
	}
	var primary *Run
	for i := range runs {
		if runs[i].Key == spekRepo+"!"+spekRunKey+":"+StageImplement {
			primary = &runs[i]
		}
	}
	if primary == nil || !reflect.DeepEqual(primary.WaveIDs, []string{"wave-1:acme/api", "wave-2:acme/ui", "wave-3:acme/docs"}) || primary.PlanEpicID == "" {
		t.Fatalf("run record = %+v", runs)
	}
	children, err := planning.GetPlanTree(store, epic.ID)
	if err != nil || len(children.Children) != 3 {
		t.Fatalf("plan tree = %+v err=%v", children, err)
	}
	listed := spekListed(t, s)
	if len(listed) != 3 {
		t.Fatalf("listed fanout stages = %+v, want 3 repo waves", listed)
	}
}

// A fan-out that fails after an approved import is retried by the next
// import tick instead of being skipped as already imported
// (hivecommons/hive#10089).
func TestImportRunPlanRetriesFailedFanout(t *testing.T) {
	_, s, store, _ := spekHub(t)
	calls := 0
	s.deps.RunFanout = func(_ context.Context, _ string, repos []string) ([]string, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("fan-out unavailable")
		}
		ids := make([]string, 0, len(repos))
		for i, repo := range repos {
			ids = append(ids, fmt.Sprintf("wave-%d:%s", i+1, repo))
		}
		return ids, nil
	}
	taskList := strings.Join([]string{
		"1. [T1] API changes [repo:acme/api] [agent_suitable]",
		"2. [T2] UI changes [repo:acme/ui] [agent_suitable]",
	}, "\n")
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("import did not create epic")
	}
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err == nil {
		t.Fatal("failed fan-out was not reported")
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("retried fan-out: %v", err)
	}
	_, epic = s.findRunEpic(spekRunKey)
	if got := epic.Meta(planning.MetaRunWaveIDs); got != "wave-1:acme/api,wave-2:acme/ui" {
		t.Fatalf("wave ids after retry = %q", got)
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil || calls != 2 {
		t.Fatalf("fanned-out plan re-fanned: calls=%d err=%v", calls, err)
	}
}

func TestImportRunPlanFanoutDisabledIsNoop(t *testing.T) {
	_, s, _, _ := spekHub(t)
	taskList := "1. [T1] API changes [repo:acme/api] [agent_suitable]"
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("import did not create epic")
	}
	if got := epic.Meta(planning.MetaRunWaveIDs); got != "" {
		t.Fatalf("wave ids = %q, want disabled no-op", got)
	}
}

// TestHubExecutorSpendsStageBudgetUnderCleanupLoop is the #9143 regression: a
// hub-executed spec stage whose agent never produces a final document — it
// exits with the document still draft, or the CLI fails outright. Driven by
// the stage worker's tick and tickLeaseLifecycle (the cleanup loop's own
// order, in which keepPendingStageLeasesAlive re-arms the executor's lease on
// every tick), the default budget of two must mint exactly one retry
// generation and then raise one decision escalation, launch nothing further
// for two lease windows, and keep the escalated run leased (not pruned)
// across a restart so a person can act on it. Before the fix the keepalive
// re-armed the lease forever and neither a retry nor an escalation ever
// happened.
func TestHubExecutorSpendsStageBudgetUnderCleanupLoop(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cliErr error
	}{
		{name: "exits_draft"},
		{name: "cli_fails", cliErr: errors.New("exit status 1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hub, s, _, _ := spekHub(t)
			// Drive the cleanup lifecycle with the synthetic clock below. The
			// fixture's real 30s cleanup ticker can otherwise interleave with
			// these manual ticks and make the stage-budget assertions depend on
			// wall-clock scheduling under -race.
			hub.Close()
			hub.persistTaskLedgers = true
			const runKey = spekRepo + "#9143"
			start := time.Now()
			admitTask := runAdmissionTaskPrefix + sanitizeReceiptSegment(runKey)
			if err := hub.recordLeaseForKeyStage(runAdmissionIdentity, admitTask, spekRepo, 9143, spekRepo+"!"+runKey+":"+StageSpec, "triage", StageSpec, 1, start); err != nil {
				t.Fatal(err)
			}

			e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
			worktree := spekHubRunWorktreePath(e.Identity, runKey)
			for _, dir := range []string{
				filepath.Join(worktree, ".spektacular", "specs"),
				filepath.Join(currentAgentWorkspaceRoot(), e.Identity, filepath.FromSlash(spekRepo), ".git"),
			} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(worktree, ".spektacular", "specs", sanitizeRunPromptPath(e.artifact(runKey))+".md"), []byte("# spec"), 0o644); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			launches := 0
			e.Exec = func(_ context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
				switch name {
				case "git":
					return []byte("ok"), nil
				case "spektacular":
					if len(args) >= 3 && args[1] == "status" {
						return []byte(`{"error":false,"kind":"spec","name":"` + args[2] + `","document_status":"draft","current_step":"authoring"}`), nil
					}
					return []byte("ok"), nil
				}
				mu.Lock()
				launches++
				mu.Unlock()
				return []byte("agent exited"), tc.cliErr
			}
			s.SetStageExecutor(e)
			ex := newSpekExec()
			ex.push(spektacular.KindSpec, spektacular.DocumentDraft)
			s.SetStageRunner(spekStageRunner{r: spekRunner(hub, ex)})

			execTask := e.stageTaskID(spekHubStage{runKey: runKey, stage: StageSpec, gen: 1})
			leaseNow := func() (taskLease, bool) {
				hub.leaseMu.Lock()
				defer hub.leaseMu.Unlock()
				l := hub.leaseForLocked(e.Identity, execTask)
				if l == nil {
					return taskLease{}, false
				}
				return *l, true
			}
			// Every 30 s, as the stage worker and cleanupLoop do, for two lease
			// windows; each tick waits for the executor's stage goroutine before
			// the next.
			now := start
			for now.Before(start.Add(2 * leaseTTL)) {
				s.tickStageRunner(now)
				hub.tickLeaseLifecycle(now)
				testutil.Eventually(t, 10*time.Second, func() bool {
					return e.Status().Running == 0
				}, "hub executor stage did not finish")
				now = now.Add(30 * time.Second)
			}

			mu.Lock()
			got := launches
			mu.Unlock()
			if got != config.DefaultMaxStageRetries {
				t.Fatalf("agent launches = %d, want one per generation of the budget (%d)", got, config.DefaultMaxStageRetries)
			}
			retried, escalated := 0, 0
			for _, action := range auditActions(t, hub) {
				switch action {
				case agentaudit.AuditLeaseStageRetried:
					retried++
				case agentaudit.AuditLeaseStageEscalated:
					escalated++
				}
			}
			if retried != 1 || escalated != 1 {
				t.Fatalf("audit retried=%d escalated=%d, want 1 and 1", retried, escalated)
			}
			var decision *timeline.Event
			for _, ev := range s.LifecycleTimeline().ByIssue(runKey) {
				if ev.Kind == timeline.KindBlocked && ev.Attrs[stageAttrSeverity] == string(escalate.SeverityDecision) {
					ev := ev
					decision = &ev
				}
			}
			if decision == nil || decision.Attrs[stageAttrStage] != StageSpec || decision.Attrs[stageAttrAttempts] != "2" || decision.Attrs["waiting_on"] != worksource.RunWaitingOnHuman {
				t.Fatalf("decision escalation on the timeline = %+v", decision)
			}
			l, ok := leaseNow()
			if !ok {
				t.Fatal("escalated run's lease was pruned; nobody can reset it")
			}
			if l.stage != StageSpec || l.gen != 2 || l.stageEscalatedAt.IsZero() || l.stageRetries != 1 || now.After(l.expiresAt) {
				t.Fatalf("escalated lease = stage %s gen %d escalated %s retries %d expires %s (now %s)", l.stage, l.gen, l.stageEscalatedAt, l.stageRetries, l.expiresAt, now)
			}
			// /api/runs reports the run as waiting on a human, which is what the
			// run-wait escalation sweep dispatches to the escalation sinks.
			waiting := false
			for _, run := range s.RunWaitSnapshot() {
				if strings.Contains(run.Key, "9143") && run.Stage == StageSpec && run.WaitingOn == worksource.RunWaitingOnHuman && !run.WaitingSince.IsZero() {
					waiting = true
				}
			}
			if !waiting {
				t.Fatalf("escalated run not waiting on a human in /api/runs: %+v", s.RunWaitSnapshot())
			}

			// A restart must neither refund the budget nor relaunch the stage.
			h2 := &ContributeWSHub{logger: hub.logger, persistTaskLedgers: true, taskLeasesFile: hub.taskLeasesPath()}
			h2.loadLeases()
			h2.leaseMu.Lock()
			restored := h2.leaseForLocked(e.Identity, execTask)
			h2.leaseMu.Unlock()
			if restored == nil || !restored.stageEscalatedAt.Equal(l.stageEscalatedAt) || restored.stageRetries != 1 || restored.gen != 2 {
				t.Fatalf("restored lease = %+v, want escalated gen 2 after one retry", restored)
			}
			// The fresh executor must see only the reloaded registry.
			s.SetStageExecutor(nil)
			s.contributeHub = h2
			t.Cleanup(func() { s.contributeHub = hub })
			fresh := NewSpekHubExecutor(s, e.Config, "copilot", "", nil, nil)
			if stages, err := fresh.unclaimedStages(); err != nil || len(stages) != 0 {
				t.Fatalf("a fresh executor would relaunch the escalated stage: %+v, %v", stages, err)
			}
		})
	}
}

// Hub shutdown cancels the stage worker's lifecycle context, and with it any
// agent job the hub executor launched from that tick. That cancellation is not
// a failed generation: it must not record a failure or spend the stage budget
// (#9143), or every restart would burn a retry and eventually escalate.
func TestHubExecutorShutdownDoesNotSpendStageBudget(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	const runKey = spekRepo + "#9221"
	start := time.Now()
	admitTask := runAdmissionTaskPrefix + sanitizeReceiptSegment(runKey)
	if err := hub.recordLeaseForKeyStage(runAdmissionIdentity, admitTask, spekRepo, 9221, spekRepo+"!"+runKey+":"+StageSpec, "triage", StageSpec, 1, start); err != nil {
		t.Fatal(err)
	}
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	worktree := spekHubRunWorktreePath(e.Identity, runKey)
	for _, dir := range []string{
		filepath.Join(worktree, ".spektacular", "specs"),
		filepath.Join(currentAgentWorkspaceRoot(), e.Identity, filepath.FromSlash(spekRepo), ".git"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// With no spec file on disk the only status call is the in-flight status
	// capture. It blocks until the stage is canceled, so that goroutine
	// records nothing after the test ends.
	var captureOnce sync.Once
	captureDone := make(chan struct{})
	launched := make(chan struct{})
	e.Exec = func(ctx context.Context, _ string, _ []string, name string, args ...string) ([]byte, error) {
		switch name {
		case "git":
			return []byte("ok"), nil
		case "spektacular":
			if len(args) >= 3 && args[1] == "status" {
				<-ctx.Done()
				captureOnce.Do(func() { close(captureDone) })
				return nil, ctx.Err()
			}
			return []byte("ok"), nil
		}
		close(launched)
		<-ctx.Done()
		return []byte("killed"), ctx.Err()
	}
	s.SetStageExecutor(e)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Tick(ctx, start)
	select {
	case <-launched:
	case <-time.After(10 * time.Second):
		t.Fatal("hub executor did not launch the agent")
	}
	cancel()
	testutil.Eventually(t, 10*time.Second, func() bool {
		return e.Status().Running == 0
	}, "hub executor stage did not stop on shutdown")
	select {
	case <-captureDone:
	case <-time.After(10 * time.Second):
		t.Fatal("status capture did not stop on shutdown")
	}

	if got := e.Status().LastError; got != "" {
		t.Fatalf("shutdown recorded a stage failure: %q", got)
	}
	for _, action := range auditActions(t, hub) {
		if action == agentaudit.AuditLeaseStageRetried || action == agentaudit.AuditLeaseStageEscalated {
			t.Fatalf("shutdown spent the stage budget: %s", action)
		}
	}
	hub.leaseMu.Lock()
	l := hub.leaseForLocked(e.Identity, e.stageTaskID(spekHubStage{runKey: runKey, stage: StageSpec, gen: 1}))
	hub.leaseMu.Unlock()
	if l == nil || l.gen != 1 || l.stageRetries != 0 || !l.stageEscalatedAt.IsZero() {
		t.Fatalf("lease after shutdown = %+v, want generation 1 unspent", l)
	}
}

// TestSpektacularRunner_StaleAndReplacedDocumentsAreRefused: final -> draft
// under the same name and a vanished document both park the lease with an
// audited refusal instead of advancing or rebinding.
func TestSpektacularRunner_StaleAndReplacedDocumentsAreRefused(t *testing.T) {
	hub, _, _, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	ex := newSpekExec()
	ex.push(spektacular.KindSpec, spektacular.DocumentDraft)
	r := spekRunner(hub, ex)
	r.Tick(context.Background(), now)
	// The queue runs dry: the fake now answers not_found for the spec.
	ex.queues[spektacular.KindSpec] = nil
	if res := r.Tick(context.Background(), now.Add(spekPoll)); res.Refused != 1 {
		t.Fatalf("replaced-document tick = %+v", res)
	}
	if stage, gen := spekLeaseState(hub); stage != StageSpec || gen != spekGen {
		t.Fatalf("lease was moved by a refusal: %s/%d", stage, gen)
	}
	var refused bool
	for _, e := range srvAuditEntries(t, hub) {
		if e.Action == agentaudit.AuditLeaseStageRefused && strings.Contains(e.Detail, spektacular.RefuseReplacedDocument) {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("refusal audit missing: %v", auditActions(t, hub))
	}

	// Stale plan: final observed but the advance could not persist, then draft.
	hub2, _, _, _ := spekHub(t)
	spekLease(t, hub2, StagePlan, now)
	ex2 := newSpekExec()
	ex2.push(spektacular.KindPlan, spektacular.DocumentFinal, spektacular.DocumentDraft)
	r2 := spekRunner(hub2, ex2)
	old := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(runReceiptsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := r2.Tick(context.Background(), now); res.Errors != 1 || res.Advanced != 0 {
		t.Fatalf("unpersistable advance tick = %+v", res)
	}
	runReceiptsDir = old
	if res := r2.Tick(context.Background(), now.Add(spekPoll)); res.Refused != 1 {
		t.Fatalf("stale plan tick = %+v", res)
	}
	if stage, _ := spekLeaseState(hub2); stage != StagePlan {
		t.Fatalf("stale plan advanced to %s", stage)
	}
}

func TestSpektacularRunner_StalePlanStatusHoldsRunForHuman(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	epic, err := store.Create(spekRunKey, beads.TypeEpic, beads.PriorityMedium, planning.ArchitectAgentName, spekRunKey)
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if err := store.SetMetadata(epic.ID, planning.MetaRunKey, spekRunKey); err != nil {
		t.Fatalf("tag epic: %v", err)
	}
	ex := newSpekExec()
	ex.push(spektacular.KindPlan, spektacular.DocumentStale)
	r := spekRunner(hub, ex)

	if res := r.Tick(context.Background(), now); res.Refused != 1 || res.Advanced != 0 {
		t.Fatalf("stale status tick = %+v", res)
	}
	if stage, _ := spekLeaseState(hub); stage != StagePlan {
		t.Fatalf("stale plan advanced to %s", stage)
	}
	updated, _ := store.Get(epic.ID)
	if updated.Meta(planning.MetaRunWaitingOn) != worksource.RunWaitingOnHuman || updated.Meta(planning.MetaRunWaitingReason) != planning.WaitingReasonStalePlan {
		t.Fatalf("epic wait metadata = %+v", updated.Metadata)
	}
	var blocked bool
	for _, ev := range s.LifecycleTimeline().ByIssue(spekRunKey) {
		if ev.Kind == timeline.KindBlocked && ev.Attrs["reason"] == planning.WaitingReasonStalePlan && ev.Attrs["waiting_on"] == worksource.RunWaitingOnHuman {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("stale plan did not record blocked timeline: %+v", s.LifecycleTimeline().ByIssue(spekRunKey))
	}
}

// countingRunner is a StageRunner that records how often the hub ticked it.
type countingRunner struct {
	ticks int
	last  time.Time
}

func (c *countingRunner) Tick(_ context.Context, now time.Time) {
	c.ticks++
	c.last = now
}

func (c *countingRunner) Status() FrontendSpektacularHubExecutor {
	return FrontendSpektacularHubExecutor{}
}

func (c *countingRunner) IsExecuting(string, string) bool { return false }

func TestRunStageAccessor_HubExecutorOwnsSpecAndPlan(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	s.deps.Config.Runs.Spektacular.Enabled = true
	on := true
	s.deps.Config.Runs.Spektacular.HubExecutor.Enabled = &on
	now := time.Now()
	for _, stage := range []string{StageSpec, StagePlan} {
		key := spekRepo + "!" + spekRunKey + ":" + stage
		if err := hub.recordLeaseForKeyStage(runAdmissionIdentity, "admit-"+stage, spekRepo, 0, key, "trusted", stage, spekGen, now); err != nil {
			t.Fatal(err)
		}
	}

	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("pending stages with hub executor enabled = %+v, want spec/plan hidden", stages)
	}

	off := false
	s.deps.Config.Runs.Spektacular.HubExecutor.Enabled = &off
	stages, err = s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 2 {
		t.Fatalf("pending stages with hub executor disabled = %+v, want spec/plan", stages)
	}
}

func TestRunStageAccessor_SkipsExecutorInFlightStage(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	off := false
	s.deps.Config.Runs.Spektacular.Enabled = true
	s.deps.Config.Runs.Spektacular.HubExecutor.Enabled = &off
	now := time.Now()
	key := spekRepo + "!" + spekRunKey + ":" + StageImplement
	if err := hub.recordLeaseForKeyStage(runAdmissionIdentity, "admit-implement", spekRepo, 0, key, "trusted", StageImplement, spekGen, now); err != nil {
		t.Fatal(err)
	}
	e := NewSpekHubExecutor(s, config.RunsConfig{Spektacular: config.SpektacularConfig{Enabled: true}}, "copilot", "", nil, nil)
	e.mu.Lock()
	e.inFlight = map[string]*spekHubExecution{e.executionKey(spekHubStage{runKey: spekRunKey, stage: StageImplement, gen: spekGen}): {started: now}}
	e.mu.Unlock()
	defer func() { e.mu.Lock(); clear(e.inFlight); e.mu.Unlock() }()
	s.SetStageExecutor(e)

	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 0 {
		t.Fatalf("in-flight executor stage was offered: %+v", stages)
	}
}

func TestSpektacularRunner_SkipsRelayHeldLeaseWithoutRefusal(t *testing.T) {
	hub, _, _, _ := spekHub(t)
	now := time.Now()
	key := spekRepo + "!" + spekRunKey + ":" + StagePlan
	if err := hub.recordLeaseForKeyStage("relay", "relay-task", spekRepo, 0, key, "contributor", StagePlan, spekGen, now); err != nil {
		t.Fatal(err)
	}
	r := spekRunner(hub, newSpekExec())
	res := r.Tick(context.Background(), now)
	if res.RelayHeld != 1 || res.Refused != 0 || res.Polled != 0 {
		t.Fatalf("tick = %+v, want one relay-held skip and no refusal/poll", res)
	}
}

func TestStageRunner_InstallAndTick(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	if s.tickStageRunner(now) {
		t.Fatal("ticked without an installed runner")
	}

	cr := &countingRunner{}
	s.SetStageRunner(cr)
	if !s.tickStageRunner(now) || cr.ticks != 1 || !cr.last.Equal(now) {
		t.Fatalf("installed runner not ticked: %+v", cr)
	}
	s.SetStageRunner(nil)
	if s.tickStageRunner(now) || cr.ticks != 1 {
		t.Fatal("removed runner still ticked")
	}
	var nilServer *Server
	nilServer.SetStageRunner(cr)
	if nilServer.tickStageRunner(now) {
		t.Fatal("nil server ticked")
	}
	_ = hub
}

// TestStageAttrKeysMatchSpektacular pins the primitives contract: the
// dashboard spells the attribute keys itself (it must not import
// pkg/spektacular) so the two lists have to agree.
func TestStageAttrKeysMatchSpektacular(t *testing.T) {
	pairs := map[string]string{
		stageAttrRunKey:         spektacular.AttrRunKey,
		stageAttrStage:          spektacular.AttrStage,
		stageAttrGen:            spektacular.AttrGen,
		stageAttrReceipt:        spektacular.AttrReceipt,
		stageAttrArtifact:       spektacular.AttrArtifact,
		stageAttrArtifactBody:   spektacular.AttrArtifactBody,
		stageAttrDocumentStatus: spektacular.AttrDocumentStatus,
		stageAttrCurrentStep:    spektacular.AttrCurrentStep,
		stageAttrReason:         spektacular.AttrReason,
		stageEscalationSeverity: string(escalate.SeverityDecision),
	}
	for dash, spek := range pairs {
		if dash != spek {
			t.Fatalf("attr key drift: dashboard %q vs spektacular %q", dash, spek)
		}
	}
	// *Server must keep satisfying the runner's registry interface.
	var _ spektacular.LeaseRegistry = (*Server)(nil)
}

func TestStageLeaseSurface_HelpersAndErrorPaths(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	for key, want := range map[string]string{
		spekRepo + "!" + spekRunKey + ":" + StagePlan: spekRunKey,
		spekRepo + "!" + spekRunKey:                   spekRunKey,
		spekRepo + "#42":                              spekRepo + "#42",
		"other/repo!" + spekRunKey + ":nope":          "other/repo!" + spekRunKey + ":nope",
		// File-address spellings join the same run (spektacular#45, #46).
		spekRepo + "!" + spekRunKey + ".md:" + StageSpec:      spekRunKey,
		spekRepo + "!" + spekRunKey + "/plan.md:" + StagePlan: spekRunKey,
		spekRunKey + ".md": spekRunKey,
	} {
		if got := runKeyOfLease(key, spekRepo); got != want {
			t.Fatalf("runKeyOfLease(%q) = %q, want %q", key, got, want)
		}
	}
	// The dashboard's copy of the bare-name rule must agree with the runner's.
	for _, name := range []string{"000057_git-commit", "000057_git-commit.md", "000057_git-commit/plan.md", "000058_v1.2-upgrade", "myorg/repo1#42", "  ", "x.MD"} {
		if dash, spek := bareArtifactName(name), spektacular.ArtifactKey(name); dash != spek {
			t.Fatalf("bare-name drift for %q: dashboard %q vs spektacular %q", name, dash, spek)
		}
	}
	if got := sanitizeReceiptSegment("a/b c:d"); got != "a_b_c_d" {
		t.Fatalf("sanitize = %q", got)
	}
	if got := sanitizeReceiptSegment(""); got != "_" {
		t.Fatalf("sanitize empty = %q", got)
	}

	// Registry surface without a hub errors; nil server is inert.
	bare := &Server{}
	if err := bare.VisitActiveStageLeases(func(string, string, string, string, string, string, uint64, time.Time) {}); err == nil {
		t.Fatal("bare VisitActiveStageLeases did not error")
	}
	if err := bare.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, time.Now(), nil, nil); err == nil {
		t.Fatal("bare AdvanceStageLease did not error")
	}
	var nilServer *Server
	nilServer.RefuseStageLease(spekTaskID, nil)
	nilServer.escalateStageLease(spekRunKey, time.Now(), nil)
	if err := nilServer.ImportRunPlan(spekRunKey, spekRepo, "1. x"); err == nil {
		t.Fatal("nil ImportRunPlan did not error")
	}
	// Unknown leases cannot be advanced.
	if err := s.AdvanceStageLease("nobody", "none", StagePlan, time.Now(), nil, nil); err == nil {
		t.Fatal("advance of an unknown lease did not error")
	}
	// A receipt that cannot be written blocks the advance, and the run key
	// falls back to the lease key when the runner sent none.
	spekLease(t, hub, StageSpec, time.Now())
	old := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(runReceiptsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, time.Now(), []byte("{}"), nil); err == nil {
		t.Fatal("unwritable receipt did not block the advance")
	}
	runReceiptsDir = old
	if stage, _ := spekLeaseState(hub); stage != StageSpec {
		t.Fatalf("lease advanced despite receipt failure: %s", stage)
	}
	off := false
	s.deps.Config.Runs.Checkpoints.Spec = &off
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, time.Now(), []byte("{}"), nil); err != nil {
		t.Fatalf("advance with derived run key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runReceiptsDir, spekRunKey, fmt.Sprintf("%s-gen%d.json", StageSpec, spekGen))); err != nil {
		t.Fatalf("receipt not written under the derived run key: %v", err)
	}

	// Plan import without a bead store is an error, and an existing epic bound
	// by external ref is reused rather than duplicated.
	s.deps.BeadStores = nil
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err == nil {
		t.Fatal("import without a store succeeded")
	}
	store, _ := beads.NewStore(t.TempDir())
	s.deps.BeadStores = map[string]*beads.Store{"any": store}
	epic, _ := store.Create("bound", beads.TypeEpic, beads.PriorityMedium, "architect", spekRunKey)
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("import into bound epic: %v", err)
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("bound epic plan_status = %q", got.Meta(planning.MetaPlanStatus))
	}
	if s.runPlanApproved(spekRunKey) {
		t.Fatal("draft plan reported approved")
	}
	if s.runPlanApproved("unknown-run") {
		t.Fatal("unknown run reported approved")
	}
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("re-import of the same plan must be a no-op: %v", err)
	}

	// Accessor without a hub errors on both methods.
	bareAcc := (&Server{}).RunStageAccessor()
	if _, err := bareAcc.PendingRunStages(context.Background()); err == nil {
		t.Fatal("bare PendingRunStages did not error")
	}
	if _, err := bareAcc.StageHasLiveLease(context.Background(), spekRunKey, StageSpec); err == nil {
		t.Fatal("bare StageHasLiveLease did not error")
	}
	// A connection working the stage makes it live (not offerable).
	conn := &ContributorConnection{
		profile:     &ContributorProfile{GitHubUsername: "spek", ContributorID: spekIdentity, TrustTier: "contributor"},
		currentTask: &WSTaskAssign{TaskID: spekTaskID, Repo: spekRepo, Title: "plan: " + spekRunKey},
	}
	hub.mu.Lock()
	hub.connections["conn-spek"] = conn
	hub.mu.Unlock()
	acc := s.RunStageAccessor()
	if live, err := acc.StageHasLiveLease(context.Background(), spekRunKey, StagePlan); err != nil || !live {
		t.Fatalf("live lease = %v err=%v", live, err)
	}
	if live, _ := acc.StageHasLiveLease(context.Background(), spekRunKey, StageSpec); live {
		t.Fatal("spec stage reported live after the advance")
	}
	if listed := spekListed(t, s); len(listed) != 0 {
		t.Fatalf("live stage was offered: %+v", listed)
	}
}

func TestSpecCheckpointDisabledRecordsAutoActor(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	off := false
	s.deps.Config.SourcePath = "hive.yaml"
	s.deps.Config.Runs.Checkpoints.Spec = &off
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	ex := newSpekExec()
	ex.push(spektacular.KindSpec, spektacular.DocumentFinal)
	r := spekRunner(hub, ex)
	if res := r.Tick(context.Background(), now); res.Advanced != 1 || res.Errors != 0 {
		t.Fatalf("final tick = %+v", res)
	}
	found := false
	for _, e := range s.audit.Recent(10) {
		found = found || (e.User == runCheckpointAutoActor && strings.Contains(e.Detail, "stage=spec") && strings.Contains(e.Detail, "config_source=hive.yaml"))
	}
	if !found {
		t.Fatalf("auto spec approval audit not recorded: %+v", s.audit.Recent(10))
	}
}

func TestRunCheckpointAutoApprovalRecordsAutoActor(t *testing.T) {
	_, s, store, _ := spekHub(t)
	off := false
	level := config.RunImplementCheckpointMinACMM
	s.deps.Config.ACMMLevel = &level
	s.deps.Config.SourcePath = "hive.yaml"
	s.deps.Config.Runs.Checkpoints.Plan = &off
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("plan import did not create epic")
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusApproved {
		t.Fatalf("plan_status = %q, want approved", got.Meta(planning.MetaPlanStatus))
	}
	foundAudit := false
	for _, e := range s.audit.Recent(10) {
		if e.User == runCheckpointAutoActor && e.Action == "plan_approve" && strings.Contains(e.Detail, "stage=plan") && strings.Contains(e.Detail, "config_source=hive.yaml") {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatalf("auto plan approval audit not recorded: %+v", s.audit.Recent(10))
	}
	foundEvent := false
	for _, ev := range s.LifecycleTimeline().ByIssue(spekRunKey) {
		if ev.Agent == runCheckpointAutoActor && ev.Attrs[runCheckpointActorKey] == runCheckpointAutoActor && ev.Attrs[runCheckpointConfigSourceKey] == "hive.yaml" {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatalf("auto plan approval event not recorded: %+v", s.LifecycleTimeline().ByIssue(spekRunKey))
	}
}

func TestAdvanceApprovedPlanLeaseMatchesCanonicalIssueKey(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	s.deps.Config.Project.Org = "clubanderson"
	now := time.Now()
	repo := "hive-runs-e2e"
	runKey := "hive-runs-e2e#7"
	leaseKey := repo + "!" + runKey + ":" + StagePlan
	if err := hub.recordLeaseForKeyStage(spekIdentity, spekTaskID, repo, 0, leaseKey, "contributor", StagePlan, spekGen, now); err != nil {
		t.Fatalf("record stage lease: %v", err)
	}
	if err := s.advanceApprovedPlanLease("clubanderson/hive-runs-e2e#7", "", "test-operator", now.Add(time.Second)); err != nil {
		t.Fatalf("advance approved plan lease: %v", err)
	}
	if stage, _ := spekLeaseState(hub); stage != StageImplement {
		t.Fatalf("stage after canonical approval = %s, want implement", stage)
	}
}

func TestCovGov_FeaturesSpektacularRoundTrip(t *testing.T) {
	s := covApiServer(t)
	if s.deps.Config.Runs.Spektacular.Enabled {
		t.Fatal("spektacular runner enabled by default")
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"spektacularEnabled": true, "spektacularBinary": " /opt/spektacular "}); rec.Code != 200 {
		t.Fatalf("PUT features: %d %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Runs.Spektacular.Enabled || s.deps.Config.Runs.Spektacular.Binary != "/opt/spektacular" {
		t.Fatalf("runs config = %+v", s.deps.Config.Runs)
	}
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != 200 {
		t.Fatalf("GET governor: %d", rec.Code)
	}
	var payload struct {
		Features struct {
			SpektacularEnabled bool   `json:"spektacularEnabled"`
			SpektacularBinary  string `json:"spektacularBinary"`
			SpektacularPollS   int    `json:"spektacularPollS"`
			MaxStageRetries    int    `json:"maxStageRetries"`
		} `json:"features"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	f := payload.Features
	if !f.SpektacularEnabled || f.SpektacularBinary != "/opt/spektacular" || f.SpektacularPollS != config.DefaultSpektacularPollS || f.MaxStageRetries != config.DefaultMaxStageRetries {
		t.Fatalf("features payload = %+v", f)
	}
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"spektacularEnabled": false}); rec.Code != 200 || s.deps.Config.Runs.Spektacular.Enabled {
		t.Fatal("could not switch the runner back off")
	}
}

// spekPlanLeaseExpiry reads the held lease's expiry so a hold can be told from
// a plain no-op.
func spekPlanLeaseExpiry(hub *ContributeWSHub) time.Time {
	hub.leaseMu.Lock()
	defer hub.leaseMu.Unlock()
	l := hub.leaseForLocked(spekIdentity, spekTaskID)
	if l == nil {
		return time.Time{}
	}
	return l.expiresAt
}

// spekDraftEpic binds a draft-plan epic to the run so the checkpoint has
// something unapproved to hold on.
func spekDraftEpic(t *testing.T, store *beads.Store) *beads.Bead {
	t.Helper()
	epic, err := store.Create("bound", beads.TypeEpic, beads.PriorityMedium, "architect", spekRunKey)
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}

	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaRunKey] = spekRunKey
		b.Metadata[planning.MetaIssueRepo] = spekRepo
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusDraft
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	got, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("reload epic: %v", err)
	}
	return got
}

func spekDesignEpic(t *testing.T, store *beads.Store) *beads.Bead {
	t.Helper()
	epic := spekDraftEpic(t, store)
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaDesignVia] = planning.DesignViaSpektacular
		b.Metadata[planning.MetaDesignStatus] = planning.DesignStatusRequested
	}); err != nil {
		t.Fatalf("mark design epic: %v", err)
	}
	got, err := store.Get(epic.ID)
	if err != nil {
		t.Fatalf("reload design epic: %v", err)
	}
	return got
}

// spekReceipts counts the stage receipts recorded for the run's plan stage.
// The timeline folds repeats of one IssueRef+Kind into a single stage and
// carries the cardinality in Count, so the number of recordings is that
// Count - ByIssue would synthesize one event no matter how many were written.
func spekReceipts(s *Server) int {
	return spekStageReceipts(s, StagePlan)
}

func spekStageReceipts(s *Server, stage string) int {
	j, ok := s.LifecycleTimeline().Journey(spekRepo + "!" + spekRunKey + ":" + stage)
	if !ok {
		return 0
	}
	st, ok := j.Stages[timeline.KindStageReceipt]
	if !ok || st == nil {
		return 0
	}
	return st.Count
}

func spekLeaseExpiry(hub *ContributeWSHub) time.Time {
	hub.leaseMu.Lock()
	defer hub.leaseMu.Unlock()
	l := hub.leaseForLocked(spekIdentity, spekTaskID)
	if l == nil {
		return time.Time{}
	}
	return l.expiresAt
}

func TestSpecCheckpointHoldsUnapprovedDesign(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	spekDesignEpic(t, store)
	before := spekLeaseExpiry(hub)

	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("AdvanceStageLease on held spec returned an error: %v", err)
	}
	if stage, _ := spekLeaseState(hub); stage != StageSpec {
		t.Fatalf("stage after held spec advance = %s, want spec", stage)
	}
	if after := spekLeaseExpiry(hub); !after.After(before) {
		t.Fatalf("held spec lease not extended: before=%s after=%s", before, after)
	}
	if got := spekStageReceipts(s, StageSpec); got != 1 {
		t.Fatalf("spec receipts on hold = %d, want 1", got)
	}
	runs, err := s.activeRuns(true)
	if err != nil || len(runs) != 1 {
		t.Fatalf("activeRuns = %d, %v", len(runs), err)
	}
	if runs[0].WaitingOn != RunWaitingOnHuman || runs[0].WaitingReason != "checkpoint_enabled" || runs[0].Stage != StageSpec {
		t.Fatalf("held spec run projection = %+v", runs[0])
	}
	if listed := spekListed(t, s); len(listed) != 0 {
		t.Fatalf("held spec was offered to relays: %+v", listed)
	}
}

func TestSpecCheckpointApproveAdvancesToPlan(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	epic := spekDesignEpic(t, store)
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held spec advance: %v", err)
	}

	checkpointKey := spekRepo + "!" + spekRunKey + ":" + StageSpec
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(checkpointKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: spekGen})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve spec checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	if stage, _ := spekLeaseState(hub); stage != StagePlan {
		t.Fatalf("stage after spec approval = %s, want plan", stage)
	}
	if got, _ := store.Get(epic.ID); planning.DesignStatus(got) != planning.DesignStatusApproved {
		t.Fatalf("design status = %q, want approved", planning.DesignStatus(got))
	}
}

// Rejecting a design's spec forgets the rejected artifact digest, so the spec
// the re-minted generation drafts is posted to the work item again even when
// its text is unchanged (hivecommons/hive#10062).
func TestSpecCheckpointRejectClearsDesignArtifactDigest(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	epic := spekDesignEpic(t, store)
	if err := store.SetMetadata(epic.ID, planning.MetaDesignArtifactDigest, designArtifactDigest("rejected spec")); err != nil {
		t.Fatalf("record design artifact digest: %v", err)
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held spec advance: %v", err)
	}

	checkpointKey := spekRepo + "!" + spekRunKey + ":" + StageSpec
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(checkpointKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: spekGen})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject spec checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaDesignArtifactDigest) != "" {
		t.Fatalf("design artifact digest after reject = %q, want cleared", got.Meta(planning.MetaDesignArtifactDigest))
	}
}

// An owner reset back to spec forgets the superseded design artifact digest
// too, so the re-run Spec stage posts its artifact to the work item again even
// when the text is unchanged - the same gap the checkpoint reject closes
// (hivecommons/hive#10062).
func TestRunResetToSpecClearsDesignArtifactDigest(t *testing.T) {
	_, s, store, _ := spekHub(t)
	epic := spekDesignEpic(t, store)
	if err := store.SetMetadata(epic.ID, planning.MetaDesignArtifactDigest, designArtifactDigest("superseded spec")); err != nil {
		t.Fatalf("record design artifact digest: %v", err)
	}

	if err := s.resetRunDesignForRespec(spekRunKey); err != nil {
		t.Fatalf("resetRunDesignForRespec: %v", err)
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaDesignArtifactDigest) != "" {
		t.Fatalf("design artifact digest after reset = %q, want cleared", got.Meta(planning.MetaDesignArtifactDigest))
	}
	if err := s.resetRunDesignForRespec("unknown-run"); err != nil {
		t.Fatalf("resetRunDesignForRespec on an unknown run: %v", err)
	}
}

// Rejecting a design's spec re-mints the spec generation so a revised spec is
// drafted, and marks the design requested (hivecommons/hive#10062).
func TestSpecCheckpointRejectRetriesDesignSpec(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StageSpec, now)
	epic := spekDesignEpic(t, store)
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StagePlan, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held spec advance: %v", err)
	}

	checkpointKey := spekRepo + "!" + spekRunKey + ":" + StageSpec
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(checkpointKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionReject, Gen: spekGen})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject spec checkpoint = %d body=%s", rec.Code, rec.Body.String())
	}
	stage, gen := spekLeaseState(hub)
	if stage != StageSpec || gen <= spekGen {
		t.Fatalf("lease after spec reject = %s gen %d, want spec past gen %d", stage, gen, spekGen)
	}
	if s.runCheckpointStageHeld(spekRunKey, StageSpec, gen) {
		t.Fatal("retried spec generation is still held")
	}
	if got, _ := store.Get(epic.ID); planning.DesignStatus(got) != planning.DesignStatusRequested {
		t.Fatalf("design status after reject = %q, want requested", planning.DesignStatus(got))
	}
	runs, err := s.activeRuns(true)
	if err != nil || len(runs) != 1 {
		t.Fatalf("activeRuns = %d, %v", len(runs), err)
	}
	if runs[0].WaitingOn == RunWaitingOnHuman {
		t.Fatalf("retried design spec still waiting on a human: %+v", runs[0])
	}
}

// A design run still drafting its spec is agent work: it is not waiting on a
// human and its checkpoint cannot be read or approved (hivecommons/hive#10061).
func TestSpecCheckpointPendingDesignNotHeldBeforeReceipt(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	spekLease(t, hub, StageSpec, time.Now())
	epic := spekDesignEpic(t, store)

	runs, err := s.activeRuns(true)
	if err != nil || len(runs) != 1 {
		t.Fatalf("activeRuns = %d, %v", len(runs), err)
	}
	if runs[0].WaitingOn == RunWaitingOnHuman {
		t.Fatalf("design run waiting on a human before its spec exists: %+v", runs[0])
	}
	if _, err := s.RunCheckpointPayload(runs[0].Key); !errors.Is(err, errRunCheckpointNotHeld) {
		t.Fatalf("checkpoint payload before spec receipt err = %v, want not held", err)
	}
	checkpointKey := spekRepo + "!" + spekRunKey + ":" + StageSpec
	rec := doOwnerPost(s, "/api/runs/"+url.PathEscape(checkpointKey)+"/checkpoint", runCheckpointDecisionRequest{Action: runCheckpointDecisionApprove, Gen: spekGen})
	if rec.Code != http.StatusConflict {
		t.Fatalf("approve before spec receipt = %d body=%s, want 409", rec.Code, rec.Body.String())
	}
	if stage, gen := spekLeaseState(hub); stage != StageSpec || gen != spekGen {
		t.Fatalf("lease after refused approve = %s gen %d, want spec gen %d", stage, gen, spekGen)
	}
	if got, _ := store.Get(epic.ID); planning.DesignStatus(got) == planning.DesignStatusApproved {
		t.Fatal("design approved before any spec existed")
	}
}

// TestPlanCheckpointHoldsUnapprovedPlan is the core of hivecommons/hive#8550
// on v5: an enabled plan checkpoint with a draft plan neither advances the
// lease nor errors, it extends the hold and still writes the receipt.
func TestPlanCheckpointHoldsUnapprovedPlan(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	spekDraftEpic(t, store)
	before := spekPlanLeaseExpiry(hub)

	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("AdvanceStageLease on held plan returned an error: %v", err)
	}
	if stage, _ := spekLeaseState(hub); stage != StagePlan {
		t.Fatalf("stage after held advance = %s, want plan", stage)
	}
	if after := spekPlanLeaseExpiry(hub); !after.After(before) {
		t.Fatalf("held plan lease not extended: before=%s after=%s", before, after)
	}
	if got := spekReceipts(s); got != 1 {
		t.Fatalf("stage receipts on hold = %d, want 1", got)
	}
}

// TestPlanCheckpointAdvancesAfterApprovePlan pins the release half: once the
// plan is approved, the very next advance reaches implement and records its
// own receipt.
func TestPlanCheckpointAdvancesAfterApprovePlan(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	epic := spekDraftEpic(t, store)

	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("held advance: %v", err)
	}
	if stage, _ := spekLeaseState(hub); stage != StagePlan {
		t.Fatalf("stage before approval = %s, want plan", stage)
	}
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now.Add(time.Second), []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("advance after approval: %v", err)
	}
	if stage, _ := spekLeaseState(hub); stage != StageImplement {
		t.Fatalf("stage after approval = %s, want implement", stage)
	}
	if got := spekReceipts(s); got != 2 {
		t.Fatalf("stage receipts across hold and advance = %d, want 2", got)
	}
}

// TestPlanCheckpointDisabledAutoApprovesAndAdvances covers the opposite
// rollout position: with runs.checkpoints.plan false the plan is approved by
// `auto` and the lease advances without a human.
func TestPlanCheckpointDisabledAutoApprovesAndAdvances(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	off := false
	s.deps.Config.SourcePath = "hive.yaml"
	s.deps.Config.Runs.Checkpoints.Plan = &off
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("plan import did not create the epic")
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusApproved {
		t.Fatalf("plan_status = %q, want approved", got.Meta(planning.MetaPlanStatus))
	}
	if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now, []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
		t.Fatalf("AdvanceStageLease: %v", err)
	}
	if stage, _ := spekLeaseState(hub); stage != StageImplement {
		t.Fatalf("stage with checkpoint disabled = %s, want implement", stage)
	}
	found := false
	for _, e := range s.audit.Recent(10) {
		if e.User == runCheckpointAutoActor && e.Action == "plan_approve" && strings.Contains(e.Detail, "stage=plan") && strings.Contains(e.Detail, "config_source=hive.yaml") {
			found = true
		}
	}
	if !found {
		t.Fatalf("auto plan approval audit not recorded: %+v", s.audit.Recent(10))
	}
}

func TestACMMRunPlanAutoApproveRequiresDisabledCheckpointAndRecordsProvenance(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	s.deps.Config.SourcePath = "hive.yaml"
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "1. [T1] x [agent_suitable]"); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("plan import did not create the epic")
	}
	if err := s.AutoApproveRunPlanCheckpointForACMM(store, epic, spekRunKey, 6); err == nil {
		t.Fatal("ACMM run auto-approve succeeded despite held plan checkpoint")
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusDraft {
		t.Fatalf("held checkpoint status = %q, want draft", got.Meta(planning.MetaPlanStatus))
	}

	off := false
	s.deps.Config.Runs.Checkpoints.Plan = &off
	if err := s.AutoApproveRunPlanCheckpointForACMM(store, epic, spekRunKey, 6); err != nil {
		t.Fatalf("ACMM run auto-approve with checkpoint disabled: %v", err)
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusApproved {
		t.Fatalf("disabled checkpoint status = %q, want approved", got.Meta(planning.MetaPlanStatus))
	}
	var approval *timeline.Event
	for _, ev := range s.LifecycleTimeline().ByIssue(spekRunKey) {
		if ev.Kind == timeline.KindStageApproval {
			cp := ev
			approval = &cp
		}
	}
	if approval == nil {
		t.Fatalf("stage approval event not recorded: %+v", s.LifecycleTimeline().ByIssue(spekRunKey))
	}
	if approval.Agent != runCheckpointAutoActor ||
		approval.Attrs[runCheckpointActorKey] != runCheckpointAutoActor ||
		approval.Attrs[runCheckpointConfigSourceKey] != "hive.yaml" ||
		!strings.Contains(approval.Attrs[runCheckpointReasonKey], "ACMM L6") {
		t.Fatalf("approval provenance = %+v", approval)
	}
}

// TestImplementCheckpointRespectsACMMFloor pins the floor: below
// RunImplementCheckpointMinACMM an explicit `implement: false` is ignored and
// the checkpoint keeps blocking.
func TestImplementCheckpointRespectsACMMFloor(t *testing.T) {
	_, s, _, _ := spekHub(t)
	off := false
	s.deps.Config.Runs.Checkpoints.Implement = &off

	below := config.RunImplementCheckpointMinACMM - 1
	s.deps.Config.ACMMLevel = &below
	if decision := s.runCheckpointPolicy(StageImplement); !decision.blocks {
		t.Fatalf("implement checkpoint opened below the ACMM floor: %+v", decision)
	}

	atFloor := config.RunImplementCheckpointMinACMM
	s.deps.Config.ACMMLevel = &atFloor
	if decision := s.runCheckpointPolicy(StageImplement); decision.blocks {
		t.Fatalf("implement checkpoint still blocks at the ACMM floor: %+v", decision)
	} else if decision.reason != runCheckpointDisabledReason {
		t.Fatalf("reason = %q, want %q", decision.reason, runCheckpointDisabledReason)
	}
}

// TestIssue8550RunCannotReachImplementWithDraftPlan is the regression guard
// for hivecommons/hive#8550: no sequence of advances may leave a run at
// stage=implement while its plan is still draft.
func TestIssue8550RunCannotReachImplementWithDraftPlan(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	epic := spekDraftEpic(t, store)

	for i := 0; i < 3; i++ {
		if err := s.AdvanceStageLease(spekIdentity, spekTaskID, StageImplement, now.Add(time.Duration(i)*time.Second), []byte(`{}`), map[string]string{stageAttrRunKey: spekRunKey}); err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		stage, _ := spekLeaseState(hub)
		got, err := store.Get(epic.ID)
		if err != nil {
			t.Fatalf("reload epic: %v", err)
		}
		if stage == StageImplement && got.Meta(planning.MetaPlanStatus) == planning.PlanStatusDraft {
			t.Fatalf("#8550 regression: run reached stage=implement with a draft plan after advance %d", i)
		}
	}
	if listed := spekListed(t, s); len(listed) != 0 {
		t.Fatalf("listed with a draft plan = %+v, want no relay-offered held checkpoint", listed)
	}
}

// Regression: PendingRunStages used to call ensureRunPlanApproved inside the
// VisitActiveStageLeases callback; auto-approval re-enters the lease registry
// for the generation and deadlocked on leaseMu.
func TestRunStageAccessorAutoApprovesDisabledImplementCheckpoint(t *testing.T) {
	hub, s, store, _ := spekHub(t)
	off := false
	level := config.RunImplementCheckpointMinACMM
	s.deps.Config.ACMMLevel = &level
	s.deps.Config.SourcePath = "hive.yaml"
	s.deps.Config.Runs.Checkpoints.Implement = &off
	now := time.Now()
	spekLease(t, hub, StageImplement, now)
	epic, err := store.Create("bound", beads.TypeEpic, beads.PriorityMedium, "architect", spekRunKey)
	if err != nil {
		t.Fatalf("create epic: %v", err)
	}
	if err := store.Update(epic.ID, func(b *beads.Bead) {
		b.Metadata[planning.MetaRunKey] = spekRunKey
		b.Metadata[planning.MetaPlanStatus] = planning.PlanStatusDraft
	}); err != nil {
		t.Fatalf("update epic: %v", err)
	}
	done := make(chan []worksource.Issue, 1)
	go func() { done <- spekListed(t, s) }()
	var listed []worksource.Issue
	select {
	case listed = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PendingRunStages deadlocked on leaseMu during checkpoint auto-approval")
	}
	if len(listed) != 1 || listed[0].Stage != StageImplement {
		t.Fatalf("listed = %+v, want auto-approved implement stage", listed)
	}
	if got, _ := store.Get(epic.ID); got.Meta(planning.MetaPlanStatus) != planning.PlanStatusApproved {
		t.Fatalf("plan_status = %q, want approved", got.Meta(planning.MetaPlanStatus))
	}
	found := false
	for _, e := range s.audit.Recent(10) {
		found = found || (e.User == runCheckpointAutoActor && e.Action == "plan_approve" && strings.Contains(e.Detail, "stage=implement"))
	}
	if !found {
		t.Fatalf("auto implement approval audit not recorded: %+v", s.audit.Recent(10))
	}
}

type blockingStageRunner struct{ started chan context.Context }

func (r *blockingStageRunner) Tick(ctx context.Context, _ time.Time) {
	r.started <- ctx
	<-ctx.Done()
}

func TestStageRunnerWorker_CancelsAndSerializes(t *testing.T) {
	s := &Server{}
	r := &blockingStageRunner{started: make(chan context.Context, 2)}
	s.SetStageRunner(r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() { defer close(done); stageRunnerLoop(ctx, ticks, func() *Server { return s }) }()
	ticks <- time.Now()
	select {
	case tickCtx := <-r.started:
		if deadline, ok := tickCtx.Deadline(); !ok || time.Until(deadline) > 30*time.Second {
			t.Fatal("tick has no bounded deadline")
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	// A blocked runner does not block the producer or start overlapping ticks.
	ticks <- time.Now()
	select {
	case <-r.started:
		t.Fatal("overlapping tick")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not cancel its active tick on shutdown")
	}
}

type contextStageExecutor struct{ ctx context.Context }

func (e *contextStageExecutor) Tick(ctx context.Context, _ time.Time) { e.ctx = ctx }
func (e *contextStageExecutor) Status() FrontendSpektacularHubExecutor {
	return FrontendSpektacularHubExecutor{}
}

func TestStageRunner_PollDoesNotCancelExecutorJobs(t *testing.T) {
	s := &Server{}
	e := &contextStageExecutor{}
	s.SetStageExecutor(e)
	s.SetStageRunner(&countingRunner{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.tickStageRunnerContext(ctx, time.Now())
	if e.ctx == nil || e.ctx.Err() != nil {
		t.Fatal("poll completion canceled executor jobs")
	}
	cancel()
	if e.ctx.Err() != context.Canceled {
		t.Fatal("executor jobs lost lifecycle cancellation")
	}
}

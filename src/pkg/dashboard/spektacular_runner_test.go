package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	spekIdentity = "c-spek"
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

func (e *spekExec) exec(_ context.Context, args []string) ([]byte, error) {
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
	// The lifecycle timeline is a process-wide singleton and every test here
	// drives the SAME lease key (spekRepo!spekRunKey:<stage>), so its
	// per-(ref,kind) Stage.Count accumulates across tests and receipt
	// assertions read other tests' receipts under -shuffle. Reset it per test,
	// the package idiom from api_lifecycle_timeline_test.go.
	resetLifecycleStore()
	hub, s := covK2Hub(t)
	s.contributeHub = hub
	old := runReceiptsDir
	runReceiptsDir = filepath.Join(t.TempDir(), "receipts")
	t.Cleanup(func() { runReceiptsDir = old })
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("bead store: %v", err)
	}
	s.deps.BeadStores = map[string]*beads.Store{planning.ArchitectAgentName: store}
	capture := &hookCapture{}
	s.deps.HookFire = capture.fire
	return hub, s, store, capture
}

func spekRunner(hub *ContributeWSHub, ex *spekExec) *spektacular.Runner {
	return &spektacular.Runner{
		Exec:     ex.exec,
		Poll:     spekPoll,
		Registry: spektacular.NewLeaseRegistryAdapter(hub.server),
		Escalate: spektacular.EscalationSink(hub.server),
		Logger:   hub.logger,
	}
}

func spekLease(t *testing.T, hub *ContributeWSHub, stage string, now time.Time) {
	t.Helper()
	key := spekRepo + "!" + spekRunKey + ":" + stage
	if err := hub.recordLeaseForKeyStage(spekIdentity, spekTaskID, spekRepo, 0, key, "contributor", stage, spekGen, now); err != nil {
		t.Fatalf("record stage lease: %v", err)
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
	if listed := spekListed(t, s); len(listed) != 1 || listed[0].Stage != StagePlan {
		t.Fatalf("listed before ApprovePlan = %+v, want the held plan stage", listed)
	}
	approvalAt := now.Add(leaseTTL + time.Second)
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	if err := s.advanceApprovedPlanLease(spekRunKey, approvalAt); err != nil {
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
	if err := s.ImportRunPlan(spekRunKey, spekRepo, taskList); err != nil {
		t.Fatalf("ImportRunPlan: %v", err)
	}
	if gotRunKey != spekRunKey || !reflect.DeepEqual(gotRepos, []string{"acme/api", "acme/ui", "acme/docs"}) {
		t.Fatalf("fanout got run=%q repos=%v", gotRunKey, gotRepos)
	}
	hub.leaseMu.Lock()
	fanoutLeases := 0
	for _, l := range hub.leases {
		if l.identity == runFanoutIdentity && l.stage == StageImplement {
			fanoutLeases++
		}
	}
	hub.leaseMu.Unlock()
	if fanoutLeases != 3 {
		t.Fatalf("fanout leases = %d, want 3", fanoutLeases)
	}
	_, epic := s.findRunEpic(spekRunKey)
	if epic == nil {
		t.Fatal("import did not create epic")
	}
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
	if err := planning.ApprovePlan(store, epic.ID); err != nil {
		t.Fatalf("ApprovePlan: %v", err)
	}
	listed := spekListed(t, s)
	if len(listed) != 3 {
		t.Fatalf("listed fanout stages = %+v, want 3 repo waves", listed)
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

// TestSpektacularRunner_ExpiryRetriesThenEscalates: a stage whose lease lapses
// without final is retried through retryLeaseStage exactly once at the default
// budget of two, then escalated with decision severity, and never gets a third
// generation.
func TestSpektacularRunner_ExpiryRetriesThenEscalates(t *testing.T) {
	hub, s, _, _ := spekHub(t)
	now := time.Now()
	spekLease(t, hub, StagePlan, now)
	ex := newSpekExec()
	ex.push(spektacular.KindPlan, spektacular.DocumentDraft)
	r := spekRunner(hub, ex)
	r.MaxRetries = config.DefaultMaxStageRetries

	r.Tick(context.Background(), now)
	now = now.Add(leaseTTL + time.Second)
	if res := r.Tick(context.Background(), now); res.Retried != 1 || res.Escalated != 0 {
		t.Fatalf("first expiry = %+v", res)
	}
	stage, gen := spekLeaseState(hub)
	if stage != StagePlan || gen <= spekGen {
		t.Fatalf("lease after retry = %s/%d", stage, gen)
	}
	if !contains(auditActions(t, hub), agentaudit.AuditLeaseStageRetried) {
		t.Fatalf("retry audit missing: %v", auditActions(t, hub))
	}
	now = now.Add(leaseTTL + time.Second)
	if res := r.Tick(context.Background(), now); res.Retried != 0 || res.Escalated != 1 {
		t.Fatalf("second expiry = %+v", res)
	}
	if _, after := spekLeaseState(hub); after != gen {
		t.Fatalf("a third generation was minted: %d -> %d", gen, after)
	}
	if !contains(auditActions(t, hub), agentaudit.AuditLeaseStageEscalated) {
		t.Fatalf("escalation audit missing: %v", auditActions(t, hub))
	}
	var blocked bool
	for _, ev := range s.LifecycleTimeline().ByIssue(spekRunKey) {
		if ev.Kind == timeline.KindBlocked && ev.Attrs["severity"] == string(escalate.SeverityDecision) && ev.Attrs["stage"] == StagePlan {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("escalation did not mark the run blocked on the timeline")
	}
	now = now.Add(leaseTTL + time.Second)
	if res := r.Tick(context.Background(), now); res.Retried != 0 || res.Escalated != 0 || res.Polled != 0 {
		t.Fatalf("post-escalation tick = %+v", res)
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

// countingRunner is a StageRunner that records how often the hub ticked it.
type countingRunner struct {
	ticks int
	last  time.Time
}

func (c *countingRunner) Tick(_ context.Context, now time.Time) {
	c.ticks++
	c.last = now
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
		stageAttrRunKey:   spektacular.AttrRunKey,
		stageAttrStage:    spektacular.AttrStage,
		stageAttrGen:      spektacular.AttrGen,
		stageAttrReceipt:  spektacular.AttrReceipt,
		stageAttrReason:   spektacular.AttrReason,
		stageAttrSeverity: spektacular.AttrSeverity,
		stageAttrAttempts: spektacular.AttrAttempts,
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
	if err := bare.RetryStageLease(spekIdentity, spekTaskID, time.Now()); err == nil {
		t.Fatal("bare RetryStageLease did not error")
	}
	var nilServer *Server
	nilServer.RefuseStageLease(spekTaskID, nil)
	nilServer.EscalateStageLease(spekRunKey, time.Now(), nil)
	if err := nilServer.ImportRunPlan(spekRunKey, spekRepo, "1. x"); err == nil {
		t.Fatal("nil ImportRunPlan did not error")
	}
	// Unknown leases cannot be advanced or retried.
	if err := s.AdvanceStageLease("nobody", "none", StagePlan, time.Now(), nil, nil); err == nil {
		t.Fatal("advance of an unknown lease did not error")
	}
	if err := s.RetryStageLease("nobody", "none", time.Now()); err == nil {
		t.Fatal("retry of an unknown lease did not error")
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
	if err := s.ImportRunPlan(spekRunKey, spekRepo, "not a task list"); err != nil {
		t.Fatalf("re-import of a planned epic must be a no-op: %v", err)
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

// spekReceipts counts the stage receipts recorded for the run's plan stage.
// The timeline folds repeats of one IssueRef+Kind into a single stage and
// carries the cardinality in Count, so the number of recordings is that
// Count - ByIssue would synthesize one event no matter how many were written.
func spekReceipts(s *Server) int {
	j, ok := s.LifecycleTimeline().Journey(spekRepo + "!" + spekRunKey + ":" + StagePlan)
	if !ok {
		return 0
	}
	st, ok := j.Stages[timeline.KindStageReceipt]
	if !ok || st == nil {
		return 0
	}
	return st.Count
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
	if listed := spekListed(t, s); len(listed) != 1 || listed[0].Stage != StagePlan {
		t.Fatalf("listed with a draft plan = %+v, want the held plan stage only", listed)
	}
}

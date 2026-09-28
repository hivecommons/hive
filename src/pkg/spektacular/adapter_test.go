package spektacular

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// fakeLeaseRegistry is a primitives-only registry the adapter drives, with
// the same generation rules the dashboard applies.
type fakeLeaseRegistry struct {
	workDir    string
	runKey     string
	stage      string
	gen        uint64
	expiresAt  time.Time
	present    bool
	visitErr   error
	advanceErr error
	advances   []map[string]string
	receipts   [][]byte
	refusals   []map[string]string
	progress   []map[string]string
	plans      []string
}

func (f *fakeLeaseRegistry) VisitActiveStageLeases(visit func(runKey, key, stage, identity, taskID, repo string, gen uint64, expiresAt time.Time)) error {
	if f.visitErr != nil {
		return f.visitErr
	}
	if f.present {
		runKey := f.runKey
		if runKey == "" {
			runKey = testRunKey
		}
		visit(runKey, testRepo+"!"+runKey+":"+f.stage, f.stage, testIdentity, testTaskID, testRepo, f.gen, f.expiresAt)
	}
	return nil
}

func (f *fakeLeaseRegistry) ResolveRunStageWorkDir(_, _, _, _ string, _ uint64) (string, error) {
	if f.workDir != "" {
		return f.workDir, nil
	}
	return "/workspace", nil
}

func (f *fakeLeaseRegistry) AdvanceStageLease(identity, taskID, to string, now time.Time, receipt []byte, attrs map[string]string) error {
	if f.advanceErr != nil {
		return f.advanceErr
	}
	if identity != testIdentity || taskID != testTaskID {
		return errors.New("unknown lease")
	}
	f.advances = append(f.advances, attrs)
	f.receipts = append(f.receipts, receipt)
	f.stage = to
	f.gen++
	f.expiresAt = now.Add(testLeaseTTL)
	return nil
}

func (f *fakeLeaseRegistry) RefuseStageLease(_ string, attrs map[string]string) {
	f.refusals = append(f.refusals, attrs)
}

func (f *fakeLeaseRegistry) RecordStageProgress(_ string, _ string, attrs map[string]string, _ time.Time) {
	f.progress = append(f.progress, attrs)
}

func (f *fakeLeaseRegistry) ImportRunPlan(_, _, taskList string) error {
	f.plans = append(f.plans, taskList)
	return nil
}

func TestLeaseAdapter_ActiveStagesUsesRunArtifactNameForIssueRunKey(t *testing.T) {
	reg := &fakeLeaseRegistry{runKey: "KubeStellar/Console#23735", stage: StageSpec, gen: 1, expiresAt: t0.Add(testLeaseTTL), present: true}
	stages, err := NewLeaseRegistryAdapter(reg).ActiveStages(t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 1 || stages[0].Artifact != "kubestellar-console-23735" {
		t.Fatalf("artifact = %#v, want kubestellar-console-23735", stages)
	}
}

func TestLeaseAdapter_AdvanceAndRefuseThroughPrimitives(t *testing.T) {
	reg := &fakeLeaseRegistry{stage: StagePlan, gen: 1, expiresAt: t0.Add(testLeaseTTL), present: true}
	ex := &scriptedExec{statuses: []string{statusJSON(KindPlan, testRunKey, DocumentFinal)}, exportJSON: exportJSON}
	r := &Runner{Exec: ex.exec, Poll: testPoll, Registry: NewLeaseRegistryAdapter(reg), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if res := r.Tick(context.Background(), t0); res.Advanced != 1 {
		t.Fatalf("plan final tick = %+v", res)
	}
	if len(reg.plans) != 1 || len(reg.advances) != 1 || len(reg.receipts) != 1 || reg.stage != StageImplement {
		t.Fatalf("registry after advance: plans=%d advances=%d receipts=%d stage=%q", len(reg.plans), len(reg.advances), len(reg.receipts), reg.stage)
	}
	attrs := reg.advances[0]
	if attrs[AttrRunKey] != testRunKey || attrs[AttrStage] != StagePlan || attrs[AttrGen] != "1" || attrs[AttrDocumentStatus] != string(DocumentFinal) || attrs[AttrArtifact] != testRunKey || attrs[AttrReceipt] == "" {
		t.Fatalf("advance attrs = %+v", attrs)
	}
	var receipt outputschema.StageReceipt
	if err := json.Unmarshal(reg.receipts[0], &receipt); err != nil || receipt.OutputDigest != attrs[AttrReceipt] {
		t.Fatalf("receipt bytes = %s err=%v", reg.receipts[0], err)
	}
	// A second tick at the same instant is a no-op: the successor (implement)
	// is never polled and the advanced generation is not advanced again.
	if res := r.Tick(context.Background(), t0); res != (TickResult{}) {
		t.Fatalf("same-instant tick = %+v", res)
	}

	// Refusal reaches the registry as attrs.
	reg2 := &fakeLeaseRegistry{stage: StageSpec, gen: 1, expiresAt: t0.Add(testLeaseTTL), present: true}
	ex2 := &scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft), notFoundJSON}}
	r2 := &Runner{Exec: ex2.exec, Poll: testPoll, Registry: NewLeaseRegistryAdapter(reg2), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r2.Tick(context.Background(), t0)
	r2.Tick(context.Background(), t0.Add(testPoll))
	if len(reg2.refusals) != 1 || reg2.refusals[0][AttrReason] != RefuseReplacedDocument || reg2.refusals[0][AttrStage] != StageSpec {
		t.Fatalf("refusals = %+v", reg2.refusals)
	}
	// Visit errors surface as counted errors.
	reg3 := &fakeLeaseRegistry{stage: StageSpec, gen: 1, expiresAt: t0.Add(testLeaseTTL), present: true, visitErr: errors.New("registry down")}
	r3 := &Runner{Exec: (&scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentDraft)}}).exec, Poll: testPoll, Registry: NewLeaseRegistryAdapter(reg3), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r3.Tick(context.Background(), t0); res.Errors != 1 {
		t.Fatalf("visit error tick = %+v", res)
	}
	// Advance errors surface and do not advance.
	reg4 := &fakeLeaseRegistry{stage: StageSpec, gen: 1, expiresAt: t0.Add(testLeaseTTL), present: true, advanceErr: errors.New("persist failed")}
	r4 := &Runner{Exec: (&scriptedExec{statuses: []string{statusJSON(KindSpec, testRunKey, DocumentFinal)}}).exec, Poll: testPoll, Registry: NewLeaseRegistryAdapter(reg4), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if res := r4.Tick(context.Background(), t0); res.Errors != 1 || res.Advanced != 0 {
		t.Fatalf("advance error tick = %+v", res)
	}
}

func TestHubRunner_BuildsFromConfigAndTicks(t *testing.T) {
	reg := &fakeLeaseRegistry{}
	cfg := config.RunsConfig{MaxStageRetries: 3, Spektacular: config.SpektacularConfig{Binary: "false", PollIntervalS: 7}}
	h := NewHubRunner(cfg, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := h.Runner()
	if r == nil || r.Poll != 7*time.Second || r.Exec == nil || r.Registry == nil {
		t.Fatalf("hub runner = %+v", r)
	}
	h.Tick(context.Background(), t0) // no leases: inert
	var nilHub *HubRunner
	nilHub.Tick(context.Background(), t0)
	if nilHub.Runner() != nil {
		t.Fatal("nil hub runner exposed a runner")
	}
	(&HubRunner{}).Tick(context.Background(), t0)
}

// TestLeaseAdapter_SameInstantTickAfterAdvanceIsNoOp reproduces the dashboard
// integration sequence exactly: the spec advances to plan under a new
// generation, then a second Tick at the same instant. The successor entry
// seeded by the advance must survive the end-of-tick prune, so the plan is
// not polled (and not advanced on a final answer) until one Poll has passed.
func TestLeaseAdapter_SameInstantTickAfterAdvanceIsNoOp(t *testing.T) {
	reg := &fakeLeaseRegistry{stage: StageSpec, gen: 11, expiresAt: t0.Add(testLeaseTTL), present: true}
	ex := &scriptedExec{statuses: []string{
		statusJSON(KindSpec, testRunKey, DocumentDraft),
		statusJSON(KindSpec, testRunKey, DocumentFinal),
		statusJSON(KindPlan, testRunKey, DocumentFinal),
	}, exportJSON: exportJSON}
	r := &Runner{Exec: ex.exec, Poll: testPoll, Registry: NewLeaseRegistryAdapter(reg), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	now := t0
	if res := r.Tick(context.Background(), now); res.Advanced != 0 || res.Polled != 1 {
		t.Fatalf("draft tick = %+v", res)
	}
	now = now.Add(testPoll)
	if res := r.Tick(context.Background(), now); res.Advanced != 1 || res.Polled != 1 {
		t.Fatalf("final tick = %+v", res)
	}
	if reg.stage != StagePlan || reg.gen != 12 {
		t.Fatalf("registry after advance = %s/%d", reg.stage, reg.gen)
	}
	// Same instant: the plan (gen 12) is seeded, not polled, not advanced.
	if res := r.Tick(context.Background(), now); res != (TickResult{}) {
		t.Fatalf("same-instant tick = %+v, want a no-op", res)
	}
	if len(reg.advances) != 1 || ex.statusCalls() != 2 {
		t.Fatalf("same-instant tick reached the CLI or registry: advances=%d status calls=%d", len(reg.advances), ex.statusCalls())
	}
	// One Poll later the plan is polled once and advances once.
	now = now.Add(testPoll)
	if res := r.Tick(context.Background(), now); res.Advanced != 1 || res.Polled != 1 {
		t.Fatalf("plan tick = %+v", res)
	}
	if len(reg.advances) != 2 || reg.stage != StageImplement {
		t.Fatalf("registry after plan advance = %s advances=%d", reg.stage, len(reg.advances))
	}
	// And again a same-instant tick is inert.
	if res := r.Tick(context.Background(), now); res != (TickResult{}) {
		t.Fatalf("second same-instant tick = %+v", res)
	}
}

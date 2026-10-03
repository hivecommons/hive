package spektacular

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/planengine"
)

func TestEngine_IdentityAndArtifactNames(t *testing.T) {
	e := NewEngine("spektacular", (&scriptedExec{}).exec)
	if e.Name() != "spektacular" || e.ContractRevision() != "spektacular-status/v1" || e.EngineVersion() != "spektacular-pr-45" {
		t.Fatalf("engine identity = %q %q %q", e.Name(), e.ContractRevision(), e.EngineVersion())
	}
	// A worksource run key becomes the CLI-safe slug; every other spelling
	// reduces to the bare artifact name.
	cases := map[string]string{
		"KubeStellar/Console#23735": "kubestellar-console-23735",
		"owner/repo!ENG-7":          "owner-repo-eng-7",
		testRunKey + ".md":          testRunKey,
		testRunKey:                  testRunKey,
	}
	for in, want := range cases {
		if got := e.ArtifactName(in); got != want {
			t.Fatalf("ArtifactName(%q) = %q, want %q", in, got, want)
		}
	}
	// Long repository names are capped by the slug rules.
	long := "some-organization/" + strings.Repeat("very-long-repository-name-", 4) + "x#123"
	if got := e.ArtifactName(long); len(got) != 64 || !strings.HasSuffix(got, "-x-123") {
		t.Fatalf("artifact = %q (len %d), want 64 chars ending in -x-123", got, len(got))
	}
}

func TestEngine_StatusExportAndReadThroughTheCLI(t *testing.T) {
	ex := &scriptedExec{
		statuses:   []string{statusJSON(KindPlan, testRunKey, DocumentFinal)},
		exportJSON: exportJSON,
		files:      map[string]string{testRunKey + "/tasks.json": `{"tasks":[{"id":"T1","title":"x"}]}`},
	}
	e := NewEngine("spektacular", ex.exec)
	st, err := e.Status(context.Background(), ".", KindPlan, testRunKey)
	if err != nil || !st.Final() {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	plan, err := e.ExportPlan(context.Background(), ".", testRunKey)
	if err != nil || len(plan.Tasks) != 3 {
		t.Fatalf("ExportPlan = %+v, %v", plan, err)
	}
	if ex.dirs[0] != "." {
		t.Fatalf("the engine ran the CLI in %q, want the stage work dir", ex.dirs[0])
	}
	var _ planengine.Engine = e
}

func TestHubRunner_BuildsFromConfigAndTicks(t *testing.T) {
	reg := &fakeLeaseRegistry{}
	cfg := config.RunsConfig{MaxStageRetries: 3, Spektacular: config.SpektacularConfig{Binary: "false", PollIntervalS: 7}}
	h := NewHubRunner(cfg, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := h.Runner()
	if r == nil || r.Poll != 7*time.Second || r.Engine == nil || r.Registry == nil {
		t.Fatalf("hub runner = %+v", r)
	}
	if r.Engine.Name() != EngineName {
		t.Fatalf("hub runner engine = %q", r.Engine.Name())
	}
	h.Tick(context.Background(), t0) // no leases: inert
	var nilHub *HubRunner
	nilHub.Tick(context.Background(), t0)
	if nilHub.Runner() != nil {
		t.Fatal("nil hub runner exposed a runner")
	}
	(&HubRunner{}).Tick(context.Background(), t0)
}

// fakeLeaseRegistry is an empty primitives-only registry: the hub runner has
// nothing to poll, which is all this package needs from it.
type fakeLeaseRegistry struct{}

func (*fakeLeaseRegistry) VisitActiveStageLeases(func(runKey, key, stage, identity, taskID, repo string, gen uint64, expiresAt time.Time)) error {
	return nil
}

func (*fakeLeaseRegistry) AdvanceStageLease(string, string, string, time.Time, []byte, map[string]string) error {
	return nil
}

func (*fakeLeaseRegistry) RefuseStageLease(string, map[string]string) {}

func (*fakeLeaseRegistry) RecordStageProgress(string, string, map[string]string, time.Time) {}

func (*fakeLeaseRegistry) ResolveRunStageWorkDir(string, string, string, string, uint64) (string, error) {
	return "/workspace", nil
}

func (*fakeLeaseRegistry) ImportRunPlan(string, string, string) error { return nil }

// TestEngine_ReceiptGolden pins the receipt the stage observer builds for
// this engine (#10353): the move off ExecFunc onto the Engine interface may
// not change a byte of it. pkg/planengine pins the same JSON from its side.
func TestEngine_ReceiptGolden(t *testing.T) {
	st := planengine.Stage{
		RunKey: testRunKey, Artifact: testRunKey, Stage: StagePlan,
		Identity: testIdentity, TaskID: testTaskID, Repo: testRepo, WorkDir: testWorkDir, Gen: 11,
	}
	status := ArtifactStatus{
		Kind: KindPlan, Name: testRunKey, ArtifactID: testRunKey,
		DocumentStatus: DocumentFinal, CurrentStep: "authoring", CompletedSteps: []string{"interview"},
		CreatedAt: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 9, 22, 13, 30, 0, 0, time.UTC),
		ClosedAt:  time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC),
	}
	receipt := planengine.BuildReceipt(NewEngine("spektacular", (&scriptedExec{}).exec), st, status, t0)
	got, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatalf("encoding receipt: %v", err)
	}
	golden := filepath.Join("testdata", "receipt.golden.json")
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading %s: %v", golden, err)
	}
	if string(got) != strings.TrimRight(string(want), "\n") {
		t.Fatalf("receipt JSON moved.\n got: %s\nwant: %s", got, want)
	}
	if receipt.ContractRevision != ContractRevision || receipt.Engine.Name != EngineName || receipt.Engine.Version != engineVersion {
		t.Fatalf("engine identity = %+v %q", receipt.Engine, receipt.ContractRevision)
	}
}

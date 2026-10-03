package planengine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

func validateReceipt(t *testing.T, r outputschema.StageReceipt) {
	t.Helper()
	raw, err := json.Marshal(outputschema.AgentReport{
		Lane: "runs", Kind: outputschema.KindStageReceipt, Summary: "stage receipt",
		Findings: []outputschema.Finding{}, PRsOpened: []outputschema.PROpened{}, BeadsFiled: []outputschema.BeadFiled{},
		Receipt: &r,
	})
	if err != nil {
		t.Fatalf("marshal receipt: %v", err)
	}
	if _, err := outputschema.Validate(raw); err != nil {
		t.Fatalf("receipt does not satisfy outputschema: %v\n%s", err, raw)
	}
}

func TestBuildReceipt_FallsBackWhenStatusLacksTimes(t *testing.T) {
	eng := &scriptedEngine{}
	st := Stage{RunKey: testRunKey, Artifact: testRunKey, Stage: StageSpec, TaskID: testTaskID, Gen: 3}
	receipt := BuildReceipt(eng, st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal}, t0)
	validateReceipt(t, receipt)
	if receipt.WorkKey != testRunKey || receipt.StartedAt != t0.Format(time.RFC3339Nano) || receipt.EndedAt != receipt.StartedAt {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receipt.Artifacts[0].Repo != testRunKey {
		t.Fatalf("artifact repo fell back to %q", receipt.Artifacts[0].Repo)
	}
	// updated_at is a file mtime whenever no workflow state matches the
	// artifact, so it never stands in for closed_at: without closed_at the
	// receipt ends at the advance instant.
	updated := t0.Add(time.Hour)
	now := t0.Add(2 * time.Hour)
	receipt = BuildReceipt(eng, st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: t0, UpdatedAt: updated}, now)
	if receipt.EndedAt != now.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("EndedAt = %s, want the advance instant, never updated_at", receipt.EndedAt)
	}
	dateOnly := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	receipt = BuildReceipt(eng, st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: dateOnly, ClosedAt: dateOnly}, now)
	if receipt.StartedAt == dateOnly.Format(time.RFC3339Nano) || receipt.EndedAt == dateOnly.Format(time.RFC3339Nano) {
		t.Fatalf("receipt used date-only frontmatter timestamps: %+v", receipt)
	}
	// Two observations of the same final document that differ only in
	// updated_at (a checkout or reformat moved the mtime) are the same input.
	a := BuildReceipt(eng, st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: t0, UpdatedAt: updated, ClosedAt: t0.Add(time.Hour)}, now)
	b := BuildReceipt(eng, st, ArtifactStatus{Kind: KindSpec, Name: testRunKey, DocumentStatus: DocumentFinal, CreatedAt: t0, ClosedAt: t0.Add(time.Hour)}, now)
	if a.InputRevision != b.InputRevision {
		t.Fatalf("InputRevision moved with updated_at: %s vs %s", a.InputRevision, b.InputRevision)
	}
	withID := BuildReceipt(eng, st, ArtifactStatus{Kind: KindSpec, Name: "000057_old", ArtifactID: "20260922132517-a1b2c3d4-new", DocumentStatus: DocumentFinal, CreatedAt: t0, ClosedAt: t0.Add(time.Hour)}, now)
	if withID.Artifacts[0].Path != "spec/20260922132517-a1b2c3d4-new" || withID.InputRevision == a.InputRevision {
		t.Fatalf("artifact_id was not used as receipt join key: %+v", withID)
	}
	if !strings.Contains(a.Provenance.Query, "spektacular spec status "+testRunKey) || strings.Contains(a.Provenance.Query, "--json") {
		t.Fatalf("provenance query = %q", a.Provenance.Query)
	}
}

// TestBuildReceipt_SpektacularGolden pins the exact receipt JSON the
// spektacular engine produces, so the port of the observer off ExecFunc onto
// the Engine interface (#10353) cannot move a byte of it: the contract
// revision, the engine name and version, the InputRevision hash inputs and
// the ExecutionKey all stay as they were.
func TestBuildReceipt_SpektacularGolden(t *testing.T) {
	st := Stage{
		RunKey: testRunKey, Artifact: testRunKey, Stage: StagePlan,
		Identity: testIdentity, TaskID: testTaskID, Repo: testRepo, WorkDir: testWorkDir, Gen: 11,
	}
	receipt := BuildReceipt(&scriptedEngine{}, st, doc(KindPlan, testRunKey, DocumentFinal), t0)
	validateReceipt(t, receipt)
	got, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatalf("encoding receipt: %v", err)
	}
	golden := filepath.Join("testdata", "spektacular-receipt.golden.json")
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading %s: %v", golden, err)
	}
	if string(got) != strings.TrimRight(string(want), "\n") {
		t.Fatalf("receipt JSON moved.\n got: %s\nwant: %s", got, want)
	}
	if receipt.ContractRevision != "spektacular-status/v1" || receipt.Engine.Name != "spektacular" || receipt.Engine.Version != "spektacular-pr-45" {
		t.Fatalf("engine identity = %+v %q", receipt.Engine, receipt.ContractRevision)
	}
}

// An engine that reports no version is stamped with its name.
func TestBuildReceipt_UnversionedEngine(t *testing.T) {
	receipt := BuildReceipt(bareEngine{name: "otherplanner"}, Stage{RunKey: testRunKey, Stage: StageSpec}, doc(KindSpec, testRunKey, DocumentFinal), t0)
	if receipt.Engine.Version != "otherplanner" || receipt.Engine.Name != "otherplanner" {
		t.Fatalf("engine = %+v", receipt.Engine)
	}
	if receipt.Provenance.Query != "otherplanner spec status "+testRunKey {
		t.Fatalf("provenance query = %q", receipt.Provenance.Query)
	}
	if receipt.Artifacts[0].Description != "Otherplanner spec reached document_status final" {
		t.Fatalf("artifact description = %q", receipt.Artifacts[0].Description)
	}
}

// bareEngine implements the Engine interface and nothing else: no artifact
// naming and no version, so it pins the neutral defaults.
type bareEngine struct{ name string }

func (e bareEngine) Name() string             { return e.name }
func (e bareEngine) ContractRevision() string { return e.name + "-status/v1" }

func (bareEngine) Probe(context.Context) (ProbeResult, error) { return ProbeResult{}, nil }

func (bareEngine) Status(context.Context, string, string, string) (ArtifactStatus, error) {
	return ArtifactStatus{}, nil
}

func (bareEngine) ResolveArtifact(context.Context, string, string, string) (string, error) {
	return "", nil
}

func (bareEngine) ExportPlan(context.Context, string, string) (Plan, error) { return Plan{}, nil }

func (bareEngine) ReadSpec(context.Context, string, string) (string, error) { return "", nil }

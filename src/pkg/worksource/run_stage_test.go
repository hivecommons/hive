package worksource

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

type stubRunStageLeases struct {
	stages     []RunStage
	live       map[string]bool
	pendingErr error
	liveErr    error
}

func (s *stubRunStageLeases) PendingRunStages(context.Context) ([]RunStage, error) {
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	return s.stages, nil
}

func (s *stubRunStageLeases) StageHasLiveLease(_ context.Context, runKey, stage string) (bool, error) {
	if s.liveErr != nil {
		return false, s.liveErr
	}
	return s.live[runKey+":"+stage], nil
}

func TestRunStageSourceListsPendingStages(t *testing.T) {
	created := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	src := NewRunStageSource(&stubRunStageLeases{stages: []RunStage{{
		RunKey: "run-123", Stage: RunStageSpec, Title: "imported plan",
		Repo: "hivecommons/hive", Author: "operator", Priority: "high",
		URL: "https://example.invalid/runs/run-123", CreatedAt: created,
	}}})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d issues, want 1: %+v", len(got), got)
	}
	item := got[0]
	if item.SourceType != SourceTypeRun || item.Number != 0 || item.ExternalID != "run-123:spec" ||
		item.Repo != "hivecommons/hive" || item.Title != "spec: imported plan" || item.Stage != RunStageSpec {
		t.Fatalf("run stage shape mismatch: %+v", item)
	}
	if !reflect.DeepEqual(item.Labels, []string{"hive-run", "stage/spec"}) {
		t.Fatalf("labels = %v", item.Labels)
	}
	if len(item.DependsOn) != 0 {
		t.Fatalf("spec must not depend on a previous stage: %+v", item.DependsOn)
	}
	if TaskKey(item) != "hivecommons/hive!run-123:spec" {
		t.Fatalf("TaskKey = %q", TaskKey(item))
	}
}

func TestRunStageSourceAdvancesAndDependsOnPreviousStage(t *testing.T) {
	src := NewRunStageSource(&stubRunStageLeases{stages: []RunStage{{
		RunKey: "run-123", Stage: RunStagePlan, Title: "imported plan",
		Repo: "hivecommons/hive", Priority: "medium",
	}}})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d issues, want 1", len(got))
	}
	item := got[0]
	if item.ExternalID != "run-123:plan" || item.Title != "plan: imported plan" {
		t.Fatalf("plan stage mismatch: %+v", item)
	}
	if len(item.DependsOn) != 1 {
		t.Fatalf("DependsOn len = %d, want 1", len(item.DependsOn))
	}
	if key := item.DependsOn[0].Ref.Key(); key != "hivecommons/hive!run-123:spec" {
		t.Fatalf("dependency key = %q", key)
	}
}

func TestRunStageSourceSkipsLiveLease(t *testing.T) {
	src := NewRunStageSource(&stubRunStageLeases{
		stages: []RunStage{
			{RunKey: "run-live", Stage: RunStageSpec, Title: "live", Repo: "hivecommons/hive"},
			{RunKey: "run-free", Stage: RunStageSpec, Title: "free", Repo: "hivecommons/hive"},
		},
		live: map[string]bool{"run-live:spec": true},
	})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 || got[0].ExternalID != "run-free:spec" {
		t.Fatalf("live lease was not excluded: %+v", got)
	}
}

func TestRunStageListMakesNoGitHubCalls(t *testing.T) {
	src := NewRunStageSource(&stubRunStageLeases{stages: []RunStage{{
		RunKey: "run-123", Stage: RunStageSpec, Title: "imported plan", Repo: "hivecommons/hive",
	}}})
	if src.SourceType() != SourceTypeRun {
		t.Fatalf("SourceType = %q", src.SourceType())
	}
	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
}

func TestRunStageSourceNilAndErrors(t *testing.T) {
	if got, err := NewRunStageSource(nil).ListIssues(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("nil accessor = %+v, %v; want empty nil-error", got, err)
	}
	src := NewRunStageSource(nil)
	src.SetAccessor(&stubRunStageLeases{stages: []RunStage{{
		RunKey: "run-wired", Stage: RunStageSpec, Repo: "hivecommons/hive",
	}}})
	if got, err := src.ListIssues(context.Background()); err != nil || len(got) != 1 || got[0].ExternalID != "run-wired:spec" {
		t.Fatalf("wired accessor = %+v, %v; want run-wired:spec", got, err)
	}
	if _, err := NewRunStageSource(&stubRunStageLeases{pendingErr: errors.New("registry down")}).ListIssues(context.Background()); err == nil {
		t.Fatalf("pending error should be returned")
	}
	if _, err := NewRunStageSource(&stubRunStageLeases{
		stages:  []RunStage{{RunKey: "run-123", Stage: RunStageSpec, Repo: "hivecommons/hive"}},
		liveErr: errors.New("lease read failed"),
	}).ListIssues(context.Background()); err == nil {
		t.Fatalf("live-lease error should be returned")
	}
}

func TestRunStageSourceSkipsMalformedAndImplementDependsOnPlan(t *testing.T) {
	src := NewRunStageSource(&stubRunStageLeases{stages: []RunStage{
		{RunKey: "", Stage: RunStageSpec, Title: "missing run", Repo: "hivecommons/hive"},
		{RunKey: "run-123", Stage: "", Title: "missing stage", Repo: "hivecommons/hive"},
		{RunKey: "run-456", Stage: RunStageImplement, Repo: "hivecommons/hive"},
	}})
	got, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d issues, want 1: %+v", len(got), got)
	}
	if got[0].Title != "implement: run-456" {
		t.Fatalf("empty title should fall back to run key: %q", got[0].Title)
	}
	if len(got[0].DependsOn) != 1 || got[0].DependsOn[0].Ref.ExternalID != "run-456:plan" {
		t.Fatalf("implement dependency = %+v", got[0].DependsOn)
	}
}

func TestCompositeFlagOffGoldenOutput(t *testing.T) {
	primary := staticSource{sourceType: "github", issues: []Issue{{
		SourceType: "github", Repo: "hivecommons/hive", ExternalID: "42", Number: 42,
		Title: "regular issue", State: "open",
	}}}
	got, err := primary.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent: %v", err)
	}
	want, err := os.ReadFile("testdata/list_issues_flag_off.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(data)+"\n" != string(want) {
		t.Fatalf("flag-off ListIssues output changed:\n%s", data)
	}
}

func TestCompositeAppendsExtras(t *testing.T) {
	primary := staticSource{sourceType: "github", issues: []Issue{{SourceType: "github", Repo: "acme/repo", ExternalID: "1", Number: 1}}}
	extra := staticSource{sourceType: SourceTypeRun, issues: []Issue{{SourceType: SourceTypeRun, Repo: "acme/repo", ExternalID: "run-1:spec"}}}
	got, err := NewComposite(primary, extra).ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(got) != 2 || got[0].Number != 1 || got[1].ExternalID != "run-1:spec" {
		t.Fatalf("composite output = %+v", got)
	}
	if got, err := (*Composite)(nil).ListIssues(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("nil composite ListIssues = %+v, %v", got, err)
	}
	if got := (*Composite)(nil).SourceType(); got != "composite" {
		t.Fatalf("nil composite SourceType = %q", got)
	}
}

type staticSource struct {
	sourceType string
	issues     []Issue
}

func (s staticSource) SourceType() string { return s.sourceType }
func (s staticSource) ListIssues(context.Context) ([]Issue, error) {
	return append([]Issue(nil), s.issues...), nil
}

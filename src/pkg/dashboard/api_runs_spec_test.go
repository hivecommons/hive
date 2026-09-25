package dashboard

import (
	"context"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
)

func TestHandleRunSpecStart(t *testing.T) {
	s := covApiServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	if rec := doPost(s, "/api/runs/spec", map[string]any{"target": "acme/widgets#42", "title": "Spec widgets"}); rec.Code != http.StatusOK {
		t.Fatalf("spec start: %d %s", rec.Code, rec.Body.String())
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatalf("PendingRunStages: %v", err)
	}
	if len(stages) != 1 || stages[0].Repo != "acme/widgets" || stages[0].Stage != StageSpec {
		t.Fatalf("stages = %+v", stages)
	}
}

func TestHandleRunSpecStartValidationAndAuth(t *testing.T) {
	s := covApiServer(t)
	if rec := doPostNoOwner(s, "/api/runs/spec", map[string]any{"target": "acme/widgets#42"}); rec.Code != http.StatusForbidden {
		t.Fatalf("no owner = %d", rec.Code)
	}
	s.deps.Config.Runs.Spektacular.Enabled = true
	if rec := doPost(s, "/api/runs/spec", map[string]any{"target": "not-a-target"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad target = %d", rec.Code)
	}
}

func TestStartDesignSpektacularLinksEpicAndRun(t *testing.T) {
	s := covApiServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.deps.BeadStores = map[string]*beads.Store{planning.ArchitectAgentName: store}
	epic, runKey, err := s.startDesignSpektacular(context.Background(), store, github.Issue{Repo: "acme/widgets", Number: 42, Title: "Design widgets"}, "body", false)
	if err != nil {
		t.Fatalf("startDesignSpektacular: %v", err)
	}
	if runKey != "acme/widgets#42" || epic.Meta(planning.MetaRunKey) != runKey || epic.Meta(planning.MetaDesignVia) != planning.DesignViaSpektacular {
		t.Fatalf("run linkage = key %q meta run %q via %q", runKey, epic.Meta(planning.MetaRunKey), epic.Meta(planning.MetaDesignVia))
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatalf("PendingRunStages: %v", err)
	}
	if len(stages) != 1 || stages[0].Stage != StageSpec || stages[0].Repo != "acme/widgets" {
		t.Fatalf("stages = %+v", stages)
	}
}

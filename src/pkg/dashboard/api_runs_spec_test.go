package dashboard

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	ghpkg "github.com/hivecommons/hive/pkg/github"
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
	epic, runKey, err := s.startDesignSpektacular(context.Background(), store, ghpkg.Issue{Repo: "acme/widgets", Number: 42, Title: "Design widgets"}, "body", false)
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

func TestStartDesignSpektacularAdmitsNonGitHubWorkItem(t *testing.T) {
	s := covApiServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	s.deps.BeadStores = map[string]*beads.Store{planning.ArchitectAgentName: store}
	epic, runKey, err := s.startDesignSpektacular(context.Background(), store, ghpkg.Issue{
		SourceType: "jira",
		Repo:       "acme/widgets",
		ExternalID: "ENG-7",
		Title:      "Design widgets from Jira",
		URL:        "https://jira.example/browse/ENG-7",
	}, "body", false)
	if err != nil {
		t.Fatalf("startDesignSpektacular: %v", err)
	}
	if runKey != "acme/widgets!ENG-7" || epic.Meta(planning.MetaIssueExternalID) != "ENG-7" || epic.Meta(planning.MetaIssueSourceType) != "jira" {
		t.Fatalf("non-github linkage = key %q external %q source %q", runKey, epic.Meta(planning.MetaIssueExternalID), epic.Meta(planning.MetaIssueSourceType))
	}
	stages, err := s.RunStageAccessor().PendingRunStages(context.Background())
	if err != nil {
		t.Fatalf("PendingRunStages: %v", err)
	}
	if len(stages) != 1 || stages[0].RunKey != runKey || stages[0].Stage != StageSpec {
		t.Fatalf("stages = %+v", stages)
	}
}

func TestPostDesignArtifactUsesWorksourceCommenter(t *testing.T) {
	var posted string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/acme/widgets/issues/42/comments" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		posted = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/acme/widgets/issues/42#issuecomment-1"}`))
	}))
	defer gh.Close()

	s := covApiServer(t)
	s.deps.GHClient = ghpkg.NewClientForTest(gh.URL, "acme", []string{"widgets"}, s.logger)
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	epic, err := store.Create("Design widgets", beads.TypeEpic, beads.PriorityMedium, "architect", "gh-acme/widgets#42")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for k, v := range map[string]string{
		planning.MetaIssueRepo:       "acme/widgets",
		planning.MetaIssueNumber:     "42",
		planning.MetaIssueSourceType: "github",
		planning.MetaDesignVia:       planning.DesignViaSpektacular,
	} {
		if err := store.SetMetadata(epic.ID, k, v); err != nil {
			t.Fatalf("SetMetadata %s: %v", k, err)
		}
	}
	epic, _ = store.Get(epic.ID)
	if err := s.postDesignArtifact(context.Background(), store, epic, "digest-1", "## Design\nShip it"); err != nil {
		t.Fatalf("postDesignArtifact: %v", err)
	}
	if !strings.Contains(posted, "Spektacular design artifact") || !strings.Contains(posted, "Ship it") {
		t.Fatalf("posted body = %q", posted)
	}
	updated, _ := store.Get(epic.ID)
	if got := updated.Meta(planning.MetaDesignArtifactDigest); got != "digest-1" {
		t.Fatalf("digest meta = %q", got)
	}
}

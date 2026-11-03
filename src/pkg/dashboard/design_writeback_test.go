package dashboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/beads"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/worksource"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestDesignWritebackRequiresBoundOwnerProvenance(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	epic, err := planning.EpicFromIssue(store, ghpkg.Issue{Repo: "acme/widgets", Number: 42, Title: "Design"}, "")
	if err != nil {
		t.Fatal(err)
	}
	ref := worksource.Ref{Repo: "acme/widgets", Number: 42}
	for _, step := range []struct {
		key, value string
		wantActor  string
	}{
		{planning.MetaDesignVia, planning.DesignViaSpektacular, ""},
		{planning.MetaRunKey, ref.Key(), ""},
		{designWritebackRequestedBy, "verified-owner", ""},
		{designWritebackRequestedRef, "acme/widgets#43", ""},
		{designWritebackRequestedRef, ref.Key(), "verified-owner"},
	} {
		if err := store.SetMetadata(epic.ID, step.key, step.value); err != nil {
			t.Fatal(err)
		}
		epic, _ = store.Get(epic.ID)
		ctx := designWritebackContext(context.Background(), epic)
		if got := worksource.SolicitedWritebackActor(ctx, ref); got != step.wantActor {
			t.Fatalf("after %s=%s: actor=%q want %q", step.key, step.value, got, step.wantActor)
		}
		if got := worksource.SolicitedWritebackActor(ctx, worksource.Ref{Repo: ref.Repo, Number: 43}); got != "" {
			t.Fatalf("request for #42 authorized another issue: %q", got)
		}
	}
}

func TestOwnerRequestedDesignWritebackSurvivesLifecycle(t *testing.T) {
	labels, comments := 0, 0
	paused := false
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/42":
			_, _ = w.Write([]byte(`{"number":42,"user":{"login":"human"},"labels":[],"created_at":"2020-01-01T00:00:00Z"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			labels++
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			comments++
			_, _ = w.Write([]byte(`{"id":1,"html_url":"https://github.com/acme/widgets/issues/42#issuecomment-1"}`))
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer gh.Close()
	s := covApiServer(t)
	s.contributeHub.persistTaskLedgers = false
	s.deps.Config.Runs.Spektacular.Enabled = true
	disableSpekHubExecutorForRelayTests(s)
	dir := t.TempDir()
	store, err := beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.deps.BeadStores = map[string]*beads.Store{planning.ArchitectAgentName: store}
	client := ghpkg.NewClientForTest(gh.URL, "acme", []string{"widgets"}, s.logger)
	client.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
		LevelFor: func(string) int { return 1 },
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
			return writeback.RepoPauseInfo{By: "owner", Reason: "freeze"}, paused
		},
	}, ""))
	s.deps.GHClient = client
	if rec := doPostNoOwner(s, "/api/runs/spec", map[string]any{"target": "acme/widgets#42", "title": "Design", "mode": "design"}); rec.Code != http.StatusForbidden {
		t.Fatalf("unverified admission: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doPost(s, "/api/runs/spec", map[string]any{"target": "acme/widgets#42", "title": "Design", "mode": "design"}); rec.Code != http.StatusOK {
		t.Fatalf("owner admission: %d %s", rec.Code, rec.Body.String())
	}
	// Reopen the store and use background contexts, as asynchronous callbacks
	// do after the HTTP request (or the process that admitted it) has ended.
	store, err = beads.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.deps.BeadStores[planning.ArchitectAgentName] = store
	_, epic := s.findRunEpic("acme/widgets#42")
	if epic == nil || epic.Meta(designWritebackRequestedBy) != "local" || epic.Meta(designWritebackRequestedRef) != "acme/widgets#42" {
		t.Fatalf("missing persisted owner provenance: %+v", epic)
	}
	if err := s.postDesignArtifact(context.Background(), store, epic, "digest-1", "requested design"); err != nil {
		t.Fatal(err)
	}
	item := worksource.WorkItemContext{Repo: "acme/widgets", Number: 42, SourceType: "github"}
	task := &WSTaskAssign{Stage: StageImplement, SourceType: worksource.SourceTypeRun, Repo: "acme/widgets", Key: "acme/widgets!acme/widgets#42:implement"}
	s.contributeHub.postCompletionPRComment(context.Background(), task, item, "https://github.com/acme/widgets/pull/43")
	if labels != 1 || comments != 2 {
		t.Fatalf("requested writes: labels=%d comments=%d", labels, comments)
	}
	ref := worksource.Ref{Repo: "acme/widgets", Number: 42}
	source := worksource.NewGitHubIssuesSource(client)
	if err := source.(worksource.LabelMutator).AddLabel(context.Background(), ref, "automatic"); !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("unrequested mutation: %v", err)
	}
	if err := source.(worksource.Commenter).AddComment(context.Background(), ref, "automatic"); !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("unrequested comment: %v", err)
	}
	paused = true
	if err := source.(worksource.LabelMutator).AddLabel(designWritebackContext(context.Background(), epic), ref, "hive-design"); !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("paused requested label: %v", err)
	}
	if rec := doPost(s, "/api/runs/spec", map[string]any{"target": "acme/widgets#42", "title": "Design", "mode": "design"}); rec.Code == http.StatusOK {
		t.Fatal("paused owner admission should not write its design label")
	}
	if err := s.postDesignArtifact(context.Background(), store, epic, "digest-2", "paused design"); !errors.Is(err, writeback.ErrSuppressed) {
		t.Fatalf("paused requested artifact: %v", err)
	}
	s.contributeHub.postCompletionPRComment(context.Background(), task, item, "https://github.com/acme/widgets/pull/44")
	if labels != 1 || comments != 2 {
		t.Fatalf("unrequested or paused callbacks wrote: labels=%d comments=%d", labels, comments)
	}
}

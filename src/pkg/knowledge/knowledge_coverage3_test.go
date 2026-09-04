package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// --- KnowledgeAPI.ConnectGitSource ---
//
// ConnectGitSource clones into localKnowledgeDir in production. Tests override
// the Init and sync-loop seams so the successful bookkeeping path stays
// deterministic and does not need network or filesystem git operations.

func TestKnowledgeAPI_ConnectGitSource_Duplicate(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	// Pre-seed a git source so the duplicate check fires before any clone.
	gs := NewGitSource(GitSourceConfig{URL: "https://example.com/x.git", Subpath: "docs"}, t.TempDir(), covLogger())
	api.gitSources = append(api.gitSources, gs)

	err := api.ConnectGitSource(context.Background(), GitSourceConfig{
		URL:     " https://example.com/x.git ",
		Subpath: "docs",
		Layer:   LayerProject,
	})
	if err == nil {
		t.Error("expected duplicate git source error")
	}
}

func TestKnowledgeAPI_ConnectGitSource_InitFailure(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	err := api.ConnectGitSource(context.Background(), GitSourceConfig{
		Name:  "bad",
		URL:   "http://127.0.0.1:1/repo.git",
		Layer: LayerProject,
	})
	if err == nil {
		t.Error("expected init failure")
	}
}

func TestKnowledgeAPI_ConnectGitSource_InvalidURL(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	err := api.ConnectGitSource(context.Background(), GitSourceConfig{
		Name:  "bad",
		URL:   "git@github.com:org/repo.git",
		Layer: LayerProject,
	})
	if err == nil {
		t.Error("expected invalid URL error")
	}
}

func TestKnowledgeAPI_ConnectGitSource_SuccessAddsSourceAndStartsSync(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	origInit := initGitSourceForConnect
	origStart := startGitSourceSyncLoopForConnect
	t.Cleanup(func() {
		initGitSourceForConnect = origInit
		startGitSourceSyncLoopForConnect = origStart
	})

	started := make(chan GitSourceConfig, 1)
	initGitSourceForConnect = func(gs *GitSource, ctx context.Context) error {
		if gs.Config().URL != "https://example.com/repo.git" {
			t.Fatalf("init URL = %q, want trimmed URL", gs.Config().URL)
		}
		return nil
	}
	startGitSourceSyncLoopForConnect = func(gs *GitSource, ctx context.Context) {
		started <- gs.Config()
	}

	err := api.ConnectGitSource(context.Background(), GitSourceConfig{
		Name:    "docs",
		URL:     " https://example.com/repo.git ",
		Subpath: "docs",
		Layer:   LayerProject,
	})
	if err != nil {
		t.Fatalf("ConnectGitSource: %v", err)
	}
	if len(api.gitSources) != 1 {
		t.Fatalf("gitSources = %d, want 1", len(api.gitSources))
	}

	const timeout = 2 * time.Second
	select {
	case cfg := <-started:
		if cfg.URL != "https://example.com/repo.git" || cfg.Subpath != "docs" {
			t.Fatalf("started config = %+v, want trimmed URL and subpath docs", cfg)
		}
	case <-time.After(timeout):
		t.Fatal("sync loop was not started")
	}
}

func TestKnowledgeAPI_DisconnectGitSource(t *testing.T) {
	api := &KnowledgeAPI{logger: covLogger()}
	gs := NewGitSource(GitSourceConfig{URL: "file:///y", Subpath: ""}, t.TempDir(), covLogger())
	api.gitSources = append(api.gitSources, gs)

	if len(api.GitSources()) != 1 {
		t.Fatalf("expected 1 git source")
	}
	if err := api.DisconnectGitSource("file:///y", ""); err != nil {
		t.Fatalf("DisconnectGitSource: %v", err)
	}
	if err := api.DisconnectGitSource("file:///y", ""); err == nil {
		t.Error("expected error disconnecting already-removed git source")
	}
}

// --- GitSyncer.Start runs at least the setup then returns on cancel ---

func TestGitSyncer_Start_NoVaults(t *testing.T) {
	syncer := NewGitSyncer(covLogger())
	// No vaults -> returns immediately.
	syncer.Start(context.Background())
}

func TestGitSyncer_Start_Cancels(t *testing.T) {
	clone := t.TempDir()
	store, err := NewFileStore(clone, "vault", covLogger())
	if err != nil {
		t.Fatal(err)
	}
	syncer := NewGitSyncer(covLogger())
	syncer.Add("vault", clone, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		syncer.Start(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GitSyncer.Start did not return after cancel")
	}
}

// --- findRelatedByTags / resolveBeadRelations via a synthesizer with a tagged vault ---

func TestRunSynthesis_WithRelatedTagsAndGraph(t *testing.T) {
	srv, ingested := newMockWikiServer(t)

	// A vault with a fact tagged "testing" so findRelatedByTags finds it.
	vaultDir := t.TempDir()
	writeVaultPage(t, vaultDir, "existing-testing-fact", "Testing Guidance", "content")
	// Overwrite with an explicit tag so ListPages("testing") matches.
	tagged := "---\ntitle: Testing Guidance\ntype: pattern\ntags: [testing]\nslug: existing-testing-fact\n---\nUse table-driven tests."
	if err := os.WriteFile(filepath.Join(vaultDir, "existing-testing-fact.md"), []byte(tagged), 0o644); err != nil {
		t.Fatal(err)
	}

	api := NewKnowledgeAPI([]LayerConfig{{Type: LayerProject, URL: srv.URL}},
		KnowledgeConfig{Enabled: true}, covLogger())
	if err := api.ConnectVault(vaultDir, "vault"); err != nil {
		t.Fatal(err)
	}
	gs := newTestGraphStore(t)
	api.SetGraphStore(gs)

	synth := NewBeadSynthesizer(map[string]*beads.Store{}, api, BeadSynthesizerConfig{
		Schedule:         "hourly",
		MinConfidence:    0.1,
		TargetLayer:      "project",
		MaxFactsPerCycle: 20,
		VaultPath:        t.TempDir(),
	}, covLogger(), nil)
	synth.SetGraphStore(gs)

	store := newTestStore(t)
	// A bug bead whose title yields a "testing" tag -> resolves related facts.
	b, _ := store.Create("flaky testing bug in ci", beads.TypeBug, beads.PriorityHigh, "scanner", "")
	store.Update(b.ID, func(bd *beads.Bead) { bd.Notes = "The test suite is flaky under load." })
	store.Close(b.ID)
	synth.beadStores = map[string]*beads.Store{"scanner": store}

	n, err := synth.RunSynthesis(context.Background())
	if err != nil {
		t.Fatalf("RunSynthesis: %v", err)
	}
	if n == 0 {
		t.Error("expected at least one fact synthesized")
	}
	if len(*ingested) == 0 {
		t.Error("expected ingested facts")
	}
}

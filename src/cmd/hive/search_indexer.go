package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/search"
)

// searchIndexerShutdownHook names the pre-shutdown hook that waits for an
// in-flight search rebuild to finish.
const searchIndexerShutdownHook = "stop-search-indexer"

// searchIndexDir is a var so tests can point it at a temp dir.
var searchIndexDir = search.DefaultDir

// searchHomeRepo is the repo that holds hive-wide documents (beads without
// an owner/repo#N ref, knowledge facts): the primary repo, else the first.
func searchHomeRepo(cfg *config.Config) string {
	repo := strings.TrimSpace(cfg.Project.PrimaryRepo)
	if repo == "" && len(cfg.Project.Repos) > 0 {
		repo = cfg.Project.Repos[0]
	}
	if repo == "" {
		return ""
	}
	return fullRepoName(repo, cfg.Project.Org)
}

// searchKnowledgeFacts lists every layer's facts; a nil API yields none.
func searchKnowledgeFacts(api *knowledge.KnowledgeAPI) func(context.Context) []knowledge.Fact {
	return func(ctx context.Context) []knowledge.Fact {
		if api == nil {
			return nil
		}
		var out []knowledge.Fact
		for _, layer := range api.Layers() {
			out = append(out, api.LayerFacts(ctx, layer, "")...)
		}
		return out
	}
}

// searchHealthOptions maps search.health onto the indexer's health score
// options (hivecommons/hive#14638).
func searchHealthOptions(h config.SearchHealthConfig) search.HealthOptions {
	return search.HealthOptions{
		Weights:           h.Weights.Resolved(),
		StaleAfter:        h.StaleAfterOrDefault(),
		SecurityDocMaxAge: h.SecurityDocMaxAgeOrDefault(),
	}
}

// newSearchIndexer builds the portfolio search indexer (#14635), or returns
// nil without touching the filesystem when search.enabled is not true.
func newSearchIndexer(cfg *config.Config, beadStores map[string]*beads.Store, api *knowledge.KnowledgeAPI, logger *slog.Logger) (*search.Indexer, error) {
	if cfg == nil || !cfg.Search.IsEnabled() {
		return nil, nil
	}
	backend, err := search.NewEmbedded(search.Options{
		Dir:                  searchIndexDir,
		MaxIndexBytesPerRepo: cfg.Search.MaxIndexBytesPerRepoOrDefault(),
		MaxMemoryBytes:       cfg.Search.MaxMemoryBytesOrDefault(),
	})
	if err != nil {
		return nil, err
	}
	return &search.Indexer{
		Backend:         backend,
		EvidenceRoot:    github.ReviewEvidenceRoot,
		Beads:           func() map[string]*beads.Store { return beadStores },
		Facts:           searchKnowledgeFacts(api),
		HomeRepo:        searchHomeRepo(cfg),
		RetentionMonths: cfg.Search.RetentionMonthsOrDefault(),
		StartupBudget:   spoke.HostedStartupBudget,
		Started:         time.Now(),
		Logger:          logger,
		HealthOptions:   searchHealthOptions(cfg.Search.Health),
	}, nil
}

// startSearchIndexer starts the indexer when search.enabled is set and
// registers a pre-shutdown hook that waits for its loop to exit.
func (b *boot) startSearchIndexer() {
	ix, err := newSearchIndexer(b.cfg, b.beadStores, b.knowledgeAPI, b.logger)
	if err != nil {
		b.logger.Warn("search indexer disabled", "error", err)
		return
	}
	if ix == nil {
		return
	}
	if b.dashSrv != nil {
		b.dashSrv.SetRepoHealthSource(ix.RepoHealth)
	}
	ctx, cancel := context.WithCancel(b.ctx)
	done := ix.Start(ctx)
	if b.dashSrv != nil {
		b.dashSrv.SetSearchBackend(ix.Backend)
	}
	b.preShutdownHooks.add(searchIndexerShutdownHook, func() {
		cancel()
		<-done
	})
	b.logger.Info("search indexer started", "dir", searchIndexDir, "interval", search.DefaultRebuildInterval)
}

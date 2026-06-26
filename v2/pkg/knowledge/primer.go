package knowledge

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"
)

// Primer queries wiki layers and merges results with precedence for injection
// into agent kick prompts. It runs during kick preparation — agents never talk
// to the wiki directly.
//
// After collecting BM25 results from wiki layers, the primer reranks them using
// Fisher-Rao geodesic distance on term-frequency embeddings. This catches
// semantic matches that keyword search misses.
// Context7Suggester checks if Context7 has docs for a keyword. Returns a hint
// string if docs are available and not yet imported, or "" if not.
type Context7Suggester func(ctx context.Context, keyword string) string

type Primer struct {
	layers           []layerClient
	fileStores       []localStore
	graphStore       *GraphStore
	config           PrimerConfig
	logger           *slog.Logger
	embedder         *EmbeddingCache
	context7Suggest  Context7Suggester
}

type layerClient struct {
	layerType LayerType
	client    *Client
}

// localStore pairs a FileStore with the layer it belongs to.
type localStore struct {
	layerType LayerType
	store     *FileStore
	name      string
}

// NewPrimer creates a primer from the configured wiki layers. Layers that are
// unreachable at construction time are skipped with a warning — the primer
// degrades gracefully.
func NewPrimer(layers []LayerConfig, config PrimerConfig, logger *slog.Logger) *Primer {
	var clients []layerClient
	for _, l := range layers {
		endpoint := l.Endpoint()
		if endpoint == "" {
			logger.Debug("skipping local-only layer (no HTTP endpoint)", "type", l.Type)
			continue
		}
		c := NewClient(endpoint, logger)
		clients = append(clients, layerClient{
			layerType: l.Type,
			client:    c,
		})
	}

	if config.MaxFacts <= 0 {
		config.MaxFacts = 25
	}

	return &Primer{
		layers:   clients,
		config:   config,
		logger:   logger,
		embedder: NewEmbeddingCache(NewTFEmbedder(EmbeddingDim)),
	}
}

// AddFileStore registers a local FileStore (from a git source or vault) so
// the primer includes its facts alongside HTTP wiki layer results.
func (p *Primer) AddFileStore(name string, store *FileStore, layer LayerType) {
	p.fileStores = append(p.fileStores, localStore{
		layerType: layer,
		store:     store,
		name:      name,
	})
	p.logger.Info("primer: added file store", "name", name, "layer", layer)
}

// Prime queries all reachable wiki layers for facts relevant to the given
// file paths and keywords, merges with precedence, and returns a result ready
// for prompt injection.
func (p *Primer) Prime(ctx context.Context, filePaths []string, keywords []string) *PrimedKnowledge {
	start := time.Now()

	query := buildQuery(filePaths, keywords)
	if query == "" {
		return &PrimedKnowledge{}
	}

	// Query each HTTP wiki layer; collect results tagged with their layer.
	var allFacts []Fact
	for _, lc := range p.layers {
		results, err := lc.client.Search(ctx, query, "", p.config.MaxFacts)
		if err != nil {
			p.logger.Warn("wiki layer unreachable, skipping",
				"layer", lc.layerType,
				"error", err,
			)
			continue
		}

		for _, r := range results {
			allFacts = append(allFacts, Fact{
				Slug:       r.Slug,
				Title:      r.Title,
				Type:       FactType(r.Type),
				Body:       r.Snippet,
				Confidence: r.Confidence,
				Status:     r.Status,
				Tags:       r.Tags,
				Layer:      lc.layerType,
			})
		}
	}

	// Query local FileStores (git sources, vaults).
	for _, ls := range p.fileStores {
		facts := ls.store.Search(query, p.config.MaxFacts)
		for i := range facts {
			facts[i].Layer = ls.layerType
		}
		allFacts = append(allFacts, facts...)
	}

	merged := p.mergeWithPrecedence(allFacts)
	reranked := p.rerankFisherRao(query, merged)
	prioritized := p.applyPriority(reranked)

	if len(prioritized) > p.config.MaxFacts {
		prioritized = prioritized[:p.config.MaxFacts]
	}

	prioritized = p.expandWithGraph(prioritized)

	elapsed := time.Since(start).Milliseconds()
	p.logger.Info("knowledge primed",
		"facts", len(prioritized),
		"http_layers", len(p.layers),
		"file_stores", len(p.fileStores),
		"graph", p.graphStore != nil,
		"query_ms", elapsed,
	)

	var hints []string
	if p.context7Suggest != nil {
		seen := make(map[string]bool)
		for _, kw := range keywords {
			if seen[kw] {
				continue
			}
			seen[kw] = true
			if hint := p.context7Suggest(ctx, kw); hint != "" {
				hints = append(hints, hint)
			}
		}
	}

	return &PrimedKnowledge{
		Facts:     prioritized,
		Hints:     hints,
		QueryTime: elapsed,
	}
}

// rerankFisherRao reranks facts using Fisher-Rao similarity between the query
// embedding and each fact's title+body embedding. The Confidence field is updated
// to reflect the geometric score, which the downstream priority sort uses as a
// tiebreaker within the same fact type.
func (p *Primer) rerankFisherRao(query string, facts []Fact) []Fact {
	if len(facts) == 0 {
		return facts
	}

	queryEmb := p.embedder.Embed(query)
	queryGaussian := NewGaussianFromEmbedding(queryEmb, defaultSigmaInitial)

	for i := range facts {
		factText := facts[i].Title + " " + facts[i].Body
		factEmb := p.embedder.Embed(factText)
		factGaussian := NewGaussianFromEmbedding(factEmb, defaultSigmaInitial)

		frScore := GraduatedScore(queryGaussian, factGaussian, facts[i].UsageCount)

		// Blend with the original wiki confidence so BM25 signal isn't lost.
		const bm25Weight = 0.4
		facts[i].Confidence = clampScore(bm25Weight*facts[i].Confidence + (1-bm25Weight)*frScore)
	}

	sort.Slice(facts, func(i, j int) bool {
		return facts[i].Confidence > facts[j].Confidence
	})

	return facts
}

// mergeWithPrecedence deduplicates facts by slug, keeping the version from the
// highest-precedence layer (personal > project > org > community).
func (p *Primer) mergeWithPrecedence(facts []Fact) []Fact {
	seen := make(map[string]Fact)
	for _, f := range facts {
		existing, exists := seen[f.Slug]
		if !exists || f.Layer.Precedence() < existing.Layer.Precedence() {
			seen[f.Slug] = f
		}
	}

	result := make([]Fact, 0, len(seen))
	for _, f := range seen {
		result = append(result, f)
	}
	return result
}

// applyPriority sorts facts by configured type priority, then by confidence.
func (p *Primer) applyPriority(facts []Fact) []Fact {
	priorityMap := make(map[string]int)
	for i, typ := range p.config.Priority {
		priorityMap[typ] = i
	}
	defaultPriority := len(p.config.Priority)

	sort.Slice(facts, func(i, j int) bool {
		pi := defaultPriority
		pj := defaultPriority
		if idx, ok := priorityMap[string(facts[i].Type)]; ok {
			pi = idx
		}
		if idx, ok := priorityMap[string(facts[j].Type)]; ok {
			pj = idx
		}
		if pi != pj {
			return pi < pj
		}
		return facts[i].Confidence > facts[j].Confidence
	})

	return facts
}

// SetContext7Suggester sets the callback that checks Context7 for library docs.
func (p *Primer) SetContext7Suggester(fn Context7Suggester) {
	p.context7Suggest = fn
}

// SetGraphStore attaches a graph store for one-hop related-fact expansion.
func (p *Primer) SetGraphStore(gs *GraphStore) {
	p.graphStore = gs
	p.logger.Info("primer: graph store attached")
}

const graphExpansionConfidenceDecay = 0.8

// expandWithGraph performs one-hop traversal from each primed fact's slug,
// pulling in related facts that aren't already in the result set.
func (p *Primer) expandWithGraph(facts []Fact) []Fact {
	if p.graphStore == nil || len(facts) == 0 {
		return facts
	}

	slugSet := make(map[string]bool, len(facts))
	for _, f := range facts {
		slugSet[f.Slug] = true
	}

	const graphTraversalDepth = 1
	var expanded []Fact
	for _, f := range facts {
		neighbors := p.graphStore.Neighbors(f.Slug, graphTraversalDepth)
		for _, t := range neighbors {
			relSlug := t.Object
			if t.Object == f.Slug {
				relSlug = t.Subject
			}
			if slugSet[relSlug] {
				continue
			}
			slugSet[relSlug] = true
			for _, ls := range p.fileStores {
				if relFact, err := ls.store.ReadPage(relSlug); err == nil {
					relFact.Confidence *= graphExpansionConfidenceDecay
					relFact.Layer = ls.layerType
					expanded = append(expanded, *relFact)
					break
				}
			}
		}
	}

	facts = append(facts, expanded...)
	if len(facts) > p.config.MaxFacts {
		facts = facts[:p.config.MaxFacts]
	}
	return facts
}

// buildQuery constructs a search string from file paths and keywords.
func buildQuery(filePaths []string, keywords []string) string {
	var parts []string

	for _, fp := range filePaths {
		segments := strings.Split(fp, "/")
		if len(segments) > 0 {
			filename := segments[len(segments)-1]
			name := strings.TrimSuffix(filename, ".go")
			name = strings.TrimSuffix(name, ".ts")
			name = strings.TrimSuffix(name, ".tsx")
			name = strings.TrimSuffix(name, ".js")
			if name != "" && name != "index" {
				parts = append(parts, name)
			}
		}
	}

	parts = append(parts, keywords...)

	return strings.Join(parts, " ")
}

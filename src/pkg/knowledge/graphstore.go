package knowledge

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketSPO = []byte("spo")
	bucketPOS = []byte("pos")
	bucketOSP = []byte("osp")
)

const tripleKeyDelim = "\x00"

// Triple represents a single directed relationship in the knowledge graph.
type Triple struct {
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
	Object    string `json:"object"`
}

// Predicate constants for well-known relationship types.
const (
	PredicateRelatedTo   = "related_to"
	PredicateSupersedes  = "supersedes"
	PredicateDependsOn   = "depends_on"
	PredicateDerivedFrom = "derived_from"
)

// GraphStore persists knowledge-graph triples in a bbolt database with three
// indexes (SPO, POS, OSP) for efficient traversal in any direction.
type GraphStore struct {
	db     *bolt.DB
	logger *slog.Logger
	mu     sync.RWMutex
}

// NewGraphStore opens or creates a bbolt-backed graph store at the given path.
func NewGraphStore(path string, logger *slog.Logger) (*GraphStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create graph store dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{NoSync: false})
	if err != nil {
		return nil, fmt.Errorf("open graph store: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketSPO, bucketPOS, bucketOSP} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close() // best-effort cleanup; the init error is what's returned
		return nil, fmt.Errorf("init graph store buckets: %w", err)
	}
	return &GraphStore{db: db, logger: logger}, nil
}

func tripleKey(a, b, c string) []byte {
	return []byte(a + tripleKeyDelim + b + tripleKeyDelim + c)
}

// AddTriple inserts a triple into all three indexes atomically.
func (g *GraphStore) AddTriple(t Triple) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.db.Update(func(tx *bolt.Tx) error {
		empty := []byte{}
		if err := tx.Bucket(bucketSPO).Put(tripleKey(t.Subject, t.Predicate, t.Object), empty); err != nil {
			return err
		}
		if err := tx.Bucket(bucketPOS).Put(tripleKey(t.Predicate, t.Object, t.Subject), empty); err != nil {
			return err
		}
		return tx.Bucket(bucketOSP).Put(tripleKey(t.Object, t.Subject, t.Predicate), empty)
	})
}

// AddTriples inserts multiple triples in a single transaction.
func (g *GraphStore) AddTriples(triples []Triple) error {
	if len(triples) == 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.db.Update(func(tx *bolt.Tx) error {
		spo := tx.Bucket(bucketSPO)
		pos := tx.Bucket(bucketPOS)
		osp := tx.Bucket(bucketOSP)
		empty := []byte{}
		for _, t := range triples {
			if err := spo.Put(tripleKey(t.Subject, t.Predicate, t.Object), empty); err != nil {
				return err
			}
			if err := pos.Put(tripleKey(t.Predicate, t.Object, t.Subject), empty); err != nil {
				return err
			}
			if err := osp.Put(tripleKey(t.Object, t.Subject, t.Predicate), empty); err != nil {
				return err
			}
		}
		return nil
	})
}

// RemoveTriple deletes a triple from all three indexes.
func (g *GraphStore) RemoveTriple(t Triple) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketSPO).Delete(tripleKey(t.Subject, t.Predicate, t.Object)); err != nil {
			return err
		}
		if err := tx.Bucket(bucketPOS).Delete(tripleKey(t.Predicate, t.Object, t.Subject)); err != nil {
			return err
		}
		return tx.Bucket(bucketOSP).Delete(tripleKey(t.Object, t.Subject, t.Predicate))
	})
}

// Outgoing returns triples where the given slug is the subject.
// If predicates are specified, only matching predicates are returned.
func (g *GraphStore) Outgoing(subject string, predicates ...string) []Triple {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var results []Triple
	predSet := toSet(predicates)
	prefix := []byte(subject + tripleKeyDelim)

	if err := g.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketSPO).Cursor()
		for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			parts := splitKey(k)
			if len(parts) != 3 {
				continue
			}
			if len(predSet) > 0 && !predSet[parts[1]] {
				continue
			}
			results = append(results, Triple{Subject: parts[0], Predicate: parts[1], Object: parts[2]})
		}
		return nil
	}); err != nil {
		g.logger.Warn("graph store view failed", "op", "Outgoing", "subject", subject, "error", err)
	}
	return results
}

// Incoming returns triples where the given slug is the object.
func (g *GraphStore) Incoming(object string, predicates ...string) []Triple {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var results []Triple
	predSet := toSet(predicates)

	if err := g.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketOSP).Cursor()
		prefix := []byte(object + tripleKeyDelim)
		for k, _ := c.Seek(prefix); k != nil && hasPrefix(k, prefix); k, _ = c.Next() {
			parts := splitKey(k)
			if len(parts) != 3 {
				continue
			}
			pred := parts[2]
			if len(predSet) > 0 && !predSet[pred] {
				continue
			}
			results = append(results, Triple{Subject: parts[1], Predicate: pred, Object: parts[0]})
		}
		return nil
	}); err != nil {
		g.logger.Warn("graph store view failed", "op", "Incoming", "object", object, "error", err)
	}
	return results
}

// Neighbors performs a BFS traversal up to the given depth, returning all
// triples reachable from the starting slug in either direction.
func (g *GraphStore) Neighbors(slug string, depth int) []Triple {
	if depth <= 0 {
		depth = 1
	}

	visited := map[string]bool{slug: true}
	frontier := []string{slug}
	var allTriples []Triple

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var nextFrontier []string
		for _, node := range frontier {
			for _, t := range g.Outgoing(node) {
				allTriples = append(allTriples, t)
				if !visited[t.Object] {
					visited[t.Object] = true
					nextFrontier = append(nextFrontier, t.Object)
				}
			}
			for _, t := range g.Incoming(node) {
				allTriples = append(allTriples, t)
				if !visited[t.Subject] {
					visited[t.Subject] = true
					nextFrontier = append(nextFrontier, t.Subject)
				}
			}
		}
		frontier = nextFrontier
	}
	return allTriples
}

// AllTriples returns every triple in the store.
func (g *GraphStore) AllTriples() []Triple {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var results []Triple
	if err := g.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketSPO).Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			parts := splitKey(k)
			if len(parts) != 3 {
				continue
			}
			results = append(results, Triple{Subject: parts[0], Predicate: parts[1], Object: parts[2]})
		}
		return nil
	}); err != nil {
		g.logger.Warn("graph store view failed", "op", "AllTriples", "error", err)
	}
	return results
}

// Count returns the total number of triples.
func (g *GraphStore) Count() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	count := 0
	if err := g.db.View(func(tx *bolt.Tx) error {
		count = tx.Bucket(bucketSPO).Stats().KeyN
		return nil
	}); err != nil {
		g.logger.Warn("graph store view failed", "op", "Count", "error", err)
	}
	return count
}

// GraphData is the JSON response for the dashboard graph API.
type GraphData struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GraphNode represents a fact in the graph visualization.
type GraphNode struct {
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	Type       string   `json:"type"`
	Layer      string   `json:"layer"`
	Confidence float64  `json:"confidence"`
	UsageCount int      `json:"usage_count"`
	Tags       []string `json:"tags,omitempty"`
	Body       string   `json:"body,omitempty"`
}

// GraphEdge represents a relationship between two facts.
type GraphEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Predicate string `json:"predicate"`
}

// BuildGraphData constructs a GraphData response by combining triples with
// fact metadata from the provided FileStores.
func (g *GraphStore) BuildGraphData(stores []*FileStore, rootSlug string, depth int) GraphData {
	var triples []Triple
	if rootSlug != "" {
		triples = g.Neighbors(rootSlug, depth)
	} else {
		triples = g.AllTriples()
	}

	slugSet := make(map[string]bool)
	var edges []GraphEdge
	for _, t := range triples {
		slugSet[t.Subject] = true
		slugSet[t.Object] = true
		edges = append(edges, GraphEdge{From: t.Subject, To: t.Object, Predicate: t.Predicate})
	}

	var nodes []GraphNode
	for slug := range slugSet {
		node := GraphNode{Slug: slug, Title: slug}
		for _, store := range stores {
			if f, err := store.ReadPage(slug); err == nil {
				node.Title = f.Title
				node.Type = string(f.Type)
				node.Layer = string(f.Layer)
				node.Confidence = f.Confidence
				node.UsageCount = f.UsageCount
				node.Tags = f.Tags
				const graphBodyMaxLen = 500
				if len(f.Body) > graphBodyMaxLen {
					node.Body = f.Body[:graphBodyMaxLen]
				} else {
					node.Body = f.Body
				}
				break
			}
		}
		nodes = append(nodes, node)
	}

	if edges == nil {
		edges = []GraphEdge{}
	}
	if nodes == nil {
		nodes = []GraphNode{}
	}
	return GraphData{Nodes: nodes, Edges: edges}
}

// SyncFromFileStore scans all pages in the given FileStore and adds
// "related_to" triples for each page's Related slugs, plus "shared_tag"
// triples between facts that share a tag.
func (g *GraphStore) SyncFromFileStore(store *FileStore) (int, error) {
	store.refreshIfStale()
	store.mu.RLock()
	pages := make([]filePage, 0, len(store.pages))
	for _, p := range store.pages {
		pages = append(pages, p)
	}
	store.mu.RUnlock()

	var triples []Triple
	for _, p := range pages {
		for _, rel := range p.Related {
			triples = append(triples, Triple{
				Subject:   p.Slug,
				Predicate: PredicateRelatedTo,
				Object:    rel,
			})
		}
	}

	triples = append(triples, buildTagEdges(pages)...)

	if err := g.AddTriples(triples); err != nil {
		return 0, fmt.Errorf("sync graph from file store: %w", err)
	}

	g.logger.Info("graph synced from file store",
		"store", store.Name(),
		"pages", len(pages),
		"triples_added", len(triples),
	)
	return len(triples), nil
}

const (
	// PredicateSharedTag links two facts that share a tag.
	PredicateSharedTag = "shared_tag"
	// maxEdgesPerTag caps edges for popular tags to avoid graph clutter.
	maxEdgesPerTag = 20
)

func buildTagEdges(pages []filePage) []Triple {
	tagToSlugs := make(map[string][]string)
	for _, p := range pages {
		for _, tag := range p.Tags {
			tagToSlugs[tag] = append(tagToSlugs[tag], p.Slug)
		}
	}

	var triples []Triple
	for _, slugs := range tagToSlugs {
		if len(slugs) < 2 || len(slugs) > maxEdgesPerTag {
			continue
		}
		for i := 0; i < len(slugs); i++ {
			for j := i + 1; j < len(slugs); j++ {
				triples = append(triples, Triple{
					Subject:   slugs[i],
					Predicate: PredicateSharedTag,
					Object:    slugs[j],
				})
			}
		}
	}
	return triples
}

// ExportJSON serializes all triples as JSON for backup/debugging.
func (g *GraphStore) ExportJSON() ([]byte, error) {
	return json.Marshal(g.AllTriples())
}

// Close closes the underlying bbolt database.
func (g *GraphStore) Close() error {
	return g.db.Close()
}

func splitKey(k []byte) []string {
	return strings.Split(string(k), tripleKeyDelim)
}

func hasPrefix(key, prefix []byte) bool {
	if len(key) < len(prefix) {
		return false
	}
	for i := range prefix {
		if key[i] != prefix[i] {
			return false
		}
	}
	return true
}

func toSet(items []string) map[string]bool {
	if len(items) == 0 {
		return nil
	}
	s := make(map[string]bool, len(items))
	for _, item := range items {
		s[item] = true
	}
	return s
}

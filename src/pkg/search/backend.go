package search

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Backend is the pluggable search store. The embedded backend is the default;
// other adapters (for example OpenSearch, #14491 Part B) implement the same
// contract. Implementations must be safe for concurrent use.
type Backend interface {
	// Index adds or replaces documents by (Repo, ID). Per-repo failures are
	// returned as *CoverageError values (joined); other repos still publish.
	Index(ctx context.Context, docs []Document) error
	// Delete removes documents by ID from one repo. Unknown IDs are ignored.
	Delete(ctx context.Context, repo string, ids []string) error
	// Query returns ranked hits and facet counts over every matching document.
	Query(ctx context.Context, q Query) (Result, error)
	// Stats reports per-repo size, load state and coverage errors.
	Stats(ctx context.Context) (Stats, error)
}

// DefaultLimit applies when Query.Limit is not positive.
const DefaultLimit = 20

// Query selects documents. Text is tokenised like documents and matched by
// exact (case-insensitive) terms; documents sharing more distinct terms rank
// higher. Empty Text matches every document admitted by the filters; Text
// that yields no terms (punctuation only) matches nothing. Filters
// map a field (kind, repo, visibility or a Document.Fields key) to accepted
// values: values within one field are ORed, fields are ANDed. From is
// inclusive and To exclusive; zero times are unbounded. Facets lists extra
// fields to count in addition to kind and repo.
type Query struct {
	Text    string
	Filters map[string][]string
	From    time.Time
	To      time.Time
	Facets  []string
	Limit   int
}

// Hit is a matching document. Terms are the distinct query terms it shares.
type Hit struct {
	Document  Document
	Score     int
	Terms     []string
	Highlight Highlight
}

// Highlight is the strongest matching line of Title or Text. Spans are
// [start, end) byte offsets into Snippet of matched terms, so renderers can
// mark them up safely in their own format.
type Highlight struct {
	Snippet string
	Spans   [][2]int
}

// FacetCount is one facet bucket.
type FacetCount struct {
	Value string
	Count int
}

// Result holds at most Limit hits; Total and Facets cover all matches.
// Coverage lists repos whose index could not be read; their absence from
// hits is not evidence that no matching document exists.
type Result struct {
	Hits     []Hit
	Total    int
	Facets   map[string][]FacetCount
	Coverage map[string]error
}

// RepoStats describes one repo index. Documents is -1 when the repo's index
// exists on disk but has not been loaded by this process.
type RepoStats struct {
	Documents int
	Bytes     int64
	Loaded    bool
}

// Stats summarises a backend.
type Stats struct {
	Repos                map[string]RepoStats
	Coverage             map[string]error
	MemoryBytes          int64
	MaxMemoryBytes       int64
	MaxIndexBytesPerRepo int64
}

// ErrIndexBudget reports that a repo's serialised index would exceed
// search.max_index_bytes_per_repo. The previous snapshot stays published.
var ErrIndexBudget = errors.New("search: index size budget exceeded")

// ErrCorruptIndex reports an unreadable index file. It is ignored (treated as
// empty) and replaced on the repo's next successful Index or Delete.
var ErrCorruptIndex = errors.New("search: corrupt index file")

// CoverageError is a per-repo failure. As in pkg/codeindex, a repo with a
// coverage error must not be treated as proof that no document exists.
type CoverageError struct {
	Repo string
	Err  error
}

func (e *CoverageError) Error() string { return fmt.Sprintf("search: coverage %s: %v", e.Repo, e.Err) }

func (e *CoverageError) Unwrap() error { return e.Err }

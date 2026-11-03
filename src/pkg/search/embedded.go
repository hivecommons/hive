package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Defaults for the embedded backend. MaxIndexBytesPerRepo and MaxMemoryBytes
// correspond to search.max_index_bytes_per_repo and search.max_memory_bytes.
const (
	DefaultDir                        = "/data/search"
	DefaultMaxIndexBytesPerRepo int64 = 32 << 20
	DefaultMaxMemoryBytes       int64 = 256 << 20
	maxSnippetBytes                   = 280
)

// Options configures NewEmbedded; zero values select the defaults.
type Options struct {
	Dir                  string
	MaxIndexBytesPerRepo int64
	MaxMemoryBytes       int64
}

// shard is an immutable per-repo snapshot. Once published it is never
// mutated, so queries read it without holding any lock.
type shard struct {
	docs     []Document // sorted by ID; posting entries index this slice
	postings map[string][]int
	facets   map[string]map[string][]int
	bytes    int64
}

func buildShard(docs []Document, size int64) *shard {
	docs = append([]Document(nil), docs...)
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	sh := &shard{docs: docs, postings: map[string][]int{}, facets: map[string]map[string][]int{}, bytes: size}
	addFacet := func(field, value string, id int) {
		if sh.facets[field] == nil {
			sh.facets[field] = map[string][]int{}
		}
		sh.facets[field][value] = append(sh.facets[field][value], id)
	}
	for id, d := range docs {
		text := []string{d.Title, d.Text}
		addFacet(FieldKind, d.Kind, id)
		addFacet(FieldRepo, d.Repo, id)
		addFacet(FieldVisibility, d.Visibility, id)
		for k, v := range d.Fields {
			addFacet(k, v, id)
			text = append(text, v)
		}
		for token := range tokens(strings.Join(text, "\n")) {
			sh.postings[token] = append(sh.postings[token], id)
		}
	}
	return sh
}

// match returns admitted doc ids with the query terms each shares. When all
// is set every admitted document matches (empty query text); otherwise a
// query with no terms matches nothing.
func (sh *shard) match(terms []string, all bool, q Query) map[int][]string {
	hits := map[int][]string{}
	if all {
		for id := range sh.docs {
			hits[id] = nil
		}
	}
	for _, term := range terms {
		for _, id := range sh.postings[term] {
			hits[id] = append(hits[id], term)
		}
	}
	for id := range hits {
		if !sh.admit(id, q) {
			delete(hits, id)
		}
	}
	return hits
}

func (sh *shard) admit(id int, q Query) bool {
	at := sh.docs[id].At
	if (!q.From.IsZero() && at.Before(q.From)) || (!q.To.IsZero() && !at.Before(q.To)) {
		return false
	}
	for field, values := range q.Filters {
		if len(values) == 0 {
			continue
		}
		ok := false
		for _, v := range values {
			ids := sh.facets[field][v]
			if i := sort.SearchInts(ids, id); i < len(ids) && ids[i] == id {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

type loaded struct {
	sh          *shard
	lastQueried uint64
}

// Embedded is the default Backend: one immutable inverted-index snapshot per
// repo, persisted to <Dir>/<owner>/<name>.idx and loaded lazily. Loaded
// snapshots are accounted by serialised size; when the total exceeds
// MaxMemoryBytes the least-recently-queried repos are dropped from memory
// (their files remain and reload on the next query).
type Embedded struct {
	dir       string
	maxIndex  int64
	maxMemory int64

	writeMu sync.Mutex // serialises Index/Delete read-modify-write cycles

	mu       sync.Mutex // guards the fields below
	shards   map[string]*loaded
	coverage map[string]error
	tick     uint64
}

var _ Backend = (*Embedded)(nil)

// NewEmbedded creates the index directory if needed. Existing index files are
// not read until a repo is queried or updated.
func NewEmbedded(opts Options) (*Embedded, error) {
	e := &Embedded{dir: opts.Dir, maxIndex: opts.MaxIndexBytesPerRepo, maxMemory: opts.MaxMemoryBytes,
		shards: map[string]*loaded{}, coverage: map[string]error{}}
	if e.dir == "" {
		e.dir = DefaultDir
	}
	if e.maxIndex <= 0 {
		e.maxIndex = DefaultMaxIndexBytesPerRepo
	}
	if e.maxMemory <= 0 {
		e.maxMemory = DefaultMaxMemoryBytes
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return nil, fmt.Errorf("search: index dir: %w", err)
	}
	return e, nil
}

// Index validates every document before writing anything, then publishes one
// new snapshot per affected repo. A repo whose new image exceeds the size
// budget, or whose existing index cannot be read, keeps its previous snapshot
// and reports a *CoverageError.
func (e *Embedded) Index(ctx context.Context, docs []Document) error {
	byRepo := map[string][]Document{}
	for _, d := range docs {
		if err := d.validate(); err != nil {
			return err
		}
		byRepo[d.Repo] = append(byRepo[d.Repo], d.clone())
	}
	return e.update(ctx, byRepo, func(cur map[string]Document, docs []Document) bool {
		for _, d := range docs {
			cur[d.ID] = d
		}
		return len(docs) > 0
	})
}

// Delete removes ids from repo and republishes its snapshot if anything changed.
func (e *Embedded) Delete(ctx context.Context, repo string, ids []string) error {
	if !validRepo(repo) {
		return fmt.Errorf("search: invalid repo %q", repo)
	}
	return e.update(ctx, map[string][]Document{repo: nil}, func(cur map[string]Document, _ []Document) bool {
		changed := false
		for _, id := range ids {
			if _, ok := cur[id]; ok {
				delete(cur, id)
				changed = true
			}
		}
		return changed
	})
}

func (e *Embedded) update(ctx context.Context, byRepo map[string][]Document, apply func(map[string]Document, []Document) bool) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	repos := make([]string, 0, len(byRepo))
	for repo := range byRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	var errs []error
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(errs, err)...)
		}
		cur := map[string]Document{}
		sh, corrupt, err := e.current(repo)
		if err != nil {
			ce := &CoverageError{Repo: repo, Err: err}
			e.mu.Lock()
			e.coverage[repo] = ce
			e.mu.Unlock()
			errs = append(errs, ce)
			continue
		}
		if sh != nil {
			for _, d := range sh.docs {
				cur[d.ID] = d
			}
		}
		// A corrupt file is republished even by a no-op update so the
		// coverage error clears, as the ErrCorruptIndex contract promises.
		if !apply(cur, byRepo[repo]) && !corrupt {
			continue
		}
		if err := e.publish(repo, cur); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// current returns the repo's snapshot, loading it if needed. A corrupt file
// is ignored (reported via corrupt) so the next publish rebuilds it from
// scratch; any other read error is returned so the on-disk snapshot is never
// overwritten blindly.
func (e *Embedded) current(repo string) (sh *shard, corrupt bool, err error) {
	e.mu.Lock()
	l := e.shards[repo]
	e.mu.Unlock()
	if l != nil {
		return l.sh, false, nil
	}
	sh, err = readShard(e.dir, repo)
	if err != nil {
		if !errors.Is(err, ErrCorruptIndex) {
			return nil, false, err
		}
		return nil, true, nil
	}
	return sh, false, nil
}

func (e *Embedded) publish(repo string, byID map[string]Document) error {
	docs := make([]Document, 0, len(byID))
	for _, d := range byID {
		docs = append(docs, d)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].ID < docs[j].ID })
	data, err := encode(repo, docs)
	if err == nil && int64(len(data)) > e.maxIndex {
		err = fmt.Errorf("%w: %d bytes exceeds %d", ErrIndexBudget, len(data), e.maxIndex)
	}
	if err == nil {
		err = writeAtomic(indexPath(e.dir, repo), data)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		ce := &CoverageError{Repo: repo, Err: err}
		e.coverage[repo] = ce
		return ce
	}
	var last uint64
	if l := e.shards[repo]; l != nil {
		last = l.lastQueried
	}
	e.shards[repo] = &loaded{sh: buildShard(docs, int64(len(data))), lastQueried: last}
	delete(e.coverage, repo)
	e.evict(repo)
	return nil
}

// acquire returns the repo's snapshot (loading it if needed), marks it as
// queried and reports any coverage error recorded for the repo.
func (e *Embedded) acquire(repo string) (*shard, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tick++
	if l := e.shards[repo]; l != nil {
		l.lastQueried = e.tick
		return l.sh, e.coverage[repo]
	}
	sh, err := readShard(e.dir, repo)
	if err != nil {
		ce := &CoverageError{Repo: repo, Err: err}
		e.coverage[repo] = ce
		return nil, ce
	}
	if sh != nil {
		e.shards[repo] = &loaded{sh: sh, lastQueried: e.tick}
		e.evict(repo)
	}
	return sh, e.coverage[repo]
}

// evict drops least-recently-queried snapshots, never keep, until loaded
// snapshots fit the memory budget. Caller holds e.mu.
func (e *Embedded) evict(keep string) {
	var total int64
	for _, l := range e.shards {
		total += l.sh.bytes
	}
	for total > e.maxMemory {
		victim := ""
		for repo, l := range e.shards {
			if repo == keep {
				continue
			}
			if v := e.shards[victim]; victim == "" || l.lastQueried < v.lastQueried || (l.lastQueried == v.lastQueried && repo < victim) {
				victim = repo
			}
		}
		if victim == "" {
			return
		}
		total -= e.shards[victim].sh.bytes
		delete(e.shards, victim)
	}
}

func (e *Embedded) queryRepos(filter []string) []string {
	set := map[string]bool{}
	if len(filter) > 0 {
		for _, repo := range filter {
			if validRepo(repo) {
				set[repo] = true
			}
		}
	} else {
		for repo := range diskRepos(e.dir) {
			set[repo] = true
		}
		e.mu.Lock()
		for repo := range e.shards {
			set[repo] = true
		}
		e.mu.Unlock()
	}
	out := make([]string, 0, len(set))
	for repo := range set {
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// Query searches every repo admitted by the repo filter (all indexed repos
// when unset). Ranking is by number of distinct shared terms, then newest
// first, then repo and ID, so results are deterministic.
func (e *Embedded) Query(ctx context.Context, q Query) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	termSet := tokens(q.Text)
	terms := make([]string, 0, len(termSet))
	for t := range termSet {
		terms = append(terms, t)
	}
	sort.Strings(terms)
	// Empty text matches every admitted document; non-empty text that
	// tokenises to nothing (punctuation only) matches none, as in codeindex.
	matchAll := strings.TrimSpace(q.Text) == ""
	type candidate struct {
		doc   *Document
		terms []string
	}
	var found []candidate
	res := Result{Facets: map[string][]FacetCount{}, Coverage: map[string]error{}}
	for _, repo := range e.queryRepos(q.Filters[FieldRepo]) {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		sh, cov := e.acquire(repo)
		if cov != nil {
			res.Coverage[repo] = cov
		}
		if sh == nil {
			continue
		}
		for id, ts := range sh.match(terms, matchAll, q) {
			found = append(found, candidate{doc: &sh.docs[id], terms: ts})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		a, b := found[i], found[j]
		switch {
		case len(a.terms) != len(b.terms):
			return len(a.terms) > len(b.terms)
		case !a.doc.At.Equal(b.doc.At):
			return a.doc.At.After(b.doc.At)
		case a.doc.Repo != b.doc.Repo:
			return a.doc.Repo < b.doc.Repo
		}
		return a.doc.ID < b.doc.ID
	})
	res.Total = len(found)

	fields := []string{FieldKind, FieldRepo}
	for _, f := range q.Facets {
		if f != "" && !contains(fields, f) {
			fields = append(fields, f)
		}
	}
	for _, field := range fields {
		counts := map[string]int{}
		for _, c := range found {
			if v := c.doc.field(field); v != "" {
				counts[v]++
			}
		}
		buckets := make([]FacetCount, 0, len(counts))
		for v, n := range counts {
			buckets = append(buckets, FacetCount{Value: v, Count: n})
		}
		sort.Slice(buckets, func(i, j int) bool {
			if buckets[i].Count != buckets[j].Count {
				return buckets[i].Count > buckets[j].Count
			}
			return buckets[i].Value < buckets[j].Value
		})
		res.Facets[field] = buckets
	}

	if len(found) > limit {
		found = found[:limit]
	}
	for _, c := range found {
		res.Hits = append(res.Hits, Hit{Document: c.doc.clone(), Score: len(c.terms),
			Terms: append([]string(nil), c.terms...), Highlight: highlight(*c.doc, terms)})
	}
	return res, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// highlight picks the Title/Text line sharing the most distinct terms (the
// first non-empty line when none match) and clips it around the first match.
func highlight(d Document, terms []string) Highlight {
	want := map[string]bool{}
	for _, t := range terms {
		want[t] = true
	}
	var best Highlight
	bestScore := -1
	for _, line := range strings.Split(d.Title+"\n"+d.Text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		spans, score := termSpans(line, want)
		if score > bestScore {
			best, bestScore = Highlight{Snippet: line, Spans: spans}, score
		}
	}
	if len(best.Snippet) <= maxSnippetBytes {
		return best
	}
	start := 0
	if len(best.Spans) > 0 {
		start = max(0, best.Spans[0][0]-maxSnippetBytes/8)
	}
	for start > 0 && !utf8.RuneStart(best.Snippet[start]) {
		start--
	}
	end := min(len(best.Snippet), start+maxSnippetBytes)
	for end < len(best.Snippet) && !utf8.RuneStart(best.Snippet[end]) {
		end--
	}
	out := Highlight{Snippet: best.Snippet[start:end]}
	for _, s := range best.Spans {
		if s[0] >= start && s[1] <= end {
			out.Spans = append(out.Spans, [2]int{s[0] - start, s[1] - start})
		}
	}
	return out
}

func termSpans(line string, want map[string]bool) ([][2]int, int) {
	var spans [][2]int
	seen := map[string]bool{}
	for _, loc := range tokenRE.FindAllStringIndex(line, -1) {
		token := line[loc[0]:loc[1]]
		if t := strings.ToLower(token); want[t] {
			spans = append(spans, [2]int{loc[0], loc[1]})
			seen[t] = true
			continue
		}
		for _, p := range componentRE.FindAllStringIndex(token, -1) {
			if part := strings.ToLower(token[p[0]:p[1]]); want[part] {
				spans = append(spans, [2]int{loc[0] + p[0], loc[0] + p[1]})
				seen[part] = true
			}
		}
	}
	return spans, len(seen)
}

// Stats lists every repo with an index file or loaded snapshot.
func (e *Embedded) Stats(ctx context.Context) (Stats, error) {
	if err := ctx.Err(); err != nil {
		return Stats{}, err
	}
	disk := diskRepos(e.dir)
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Stats{Repos: map[string]RepoStats{}, Coverage: map[string]error{},
		MaxMemoryBytes: e.maxMemory, MaxIndexBytesPerRepo: e.maxIndex}
	for repo, size := range disk {
		st.Repos[repo] = RepoStats{Documents: -1, Bytes: size}
	}
	for repo, l := range e.shards {
		st.Repos[repo] = RepoStats{Documents: len(l.sh.docs), Bytes: l.sh.bytes, Loaded: true}
		st.MemoryBytes += l.sh.bytes
	}
	for repo, err := range e.coverage {
		st.Coverage[repo] = err
	}
	return st, nil
}

package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// DefaultRebuildInterval is the indexer cadence, the same as the prior-art
// code index rebuild (github.DefaultPriorArtCodeIndexTTL).
const DefaultRebuildInterval = 30 * time.Minute

// DefaultRetentionMonths is how many months of documents are kept, by
// Document.At, when Indexer.RetentionMonths is not positive.
const DefaultRetentionMonths = 24

// Document kinds produced by the Indexer. Reconciliation only ever deletes
// documents of these kinds, so later sources can share the backend.
const (
	KindEvidence   = "evidence"
	KindFinding    = "finding"
	KindSentinel   = "sentinel"
	KindComplexity = "complexity"
	KindBead       = "bead"
	KindFact       = "fact"
)

var indexerKinds = []string{KindEvidence, KindFinding, KindSentinel, KindComplexity, KindBead, KindFact}

// Indexer keeps a Backend in step with the hive's local stores: evidence
// bundles and complexity snapshots under EvidenceRoot (one walk per
// rebuild), bead stores and knowledge facts. Stores are only read, through
// their package APIs. Rebuilds are incremental: a source whose stamp (bundle
// updated_at, snapshot captured_at, bead updated_at, fact content hash) is
// unchanged since the last rebuild is not re-read into the index. Every
// Title, Text and field value passes through logscrub before indexing.
type Indexer struct {
	Backend Backend
	// EvidenceRoot is the review evidence root (<owner>/<repo>/<n>/<head>.json
	// bundles and <owner>/<repo>/complexity/<YYYY-MM>.json snapshots); empty
	// skips both sources.
	EvidenceRoot string
	// Beads returns the bead stores to index, keyed by store (agent) name.
	Beads func() map[string]*beads.Store
	// Facts returns the knowledge facts to index.
	Facts func(context.Context) []knowledge.Fact
	// HomeRepo ("owner/name") holds beads without an owner/repo#N external
	// ref and knowledge facts; empty skips them.
	HomeRepo string
	// Visibility reports a repo's visibility. Anything but Public (including
	// a nil func) indexes the repo's documents as Private, so they stay local.
	Visibility func(repo string) string
	// RetentionMonths drops documents whose At is older; <=0 means
	// DefaultRetentionMonths.
	RetentionMonths int
	// Interval is the rebuild cadence; <=0 means DefaultRebuildInterval.
	Interval time.Duration
	// StartupBudget and Started describe the process startup budget
	// (hivecommons/hive#14389): no ingest runs before Started+StartupBudget.
	StartupBudget time.Duration
	Started       time.Time
	// Now is the clock; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
	// HealthOptions tunes the per-repo health score recomputed after every
	// rebuild (see ComputeHealth); HealthSignals supplies its non-evidence
	// inputs per repo. A nil HealthSignals leaves those components
	// not_reported.
	HealthOptions HealthOptions
	HealthSignals func(repo string) HealthSignals

	healthMu sync.RWMutex // guards health; never held with mu across a rebuild's I/O
	health   map[string]Health

	mu         sync.Mutex // serialises rebuilds and guards the state below
	entries    map[string]*sourceEntry
	reconciled bool
	// pending lists the repos reconcile still has to sweep (nil: every repo).
	pending map[string]bool
}

// sourceEntry is what the indexer remembers about one source record.
type sourceEntry struct {
	mod   time.Time // file sources: mtime and size short-circuit re-reads
	size  int64
	stamp string
	first time.Time // when the source was first seen
	docs  []docRef
	// bundle is the trimmed evidence bundle (healthBundle) the health score
	// is recomputed from; nil for other sources.
	bundle *evidence.Bundle
}

type docRef struct {
	repo, id string
	at       time.Time
}

// RebuildStats summarises one rebuild.
type RebuildStats struct {
	// Skipped is set when the startup budget was still active.
	Skipped   bool
	Indexed   int
	Unchanged int
	Deleted   int
	Invalid   int
}

func (i *Indexer) now() time.Time {
	if i.Now != nil {
		return i.Now()
	}
	return time.Now()
}

func (i *Indexer) logger() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.Default()
}

func (i *Indexer) interval() time.Duration {
	if i.Interval > 0 {
		return i.Interval
	}
	return DefaultRebuildInterval
}

func (i *Indexer) retentionMonths() int {
	if i.RetentionMonths > 0 {
		return i.RetentionMonths
	}
	return DefaultRetentionMonths
}

// startupActive reports whether the startup budget still forbids ingest.
func (i *Indexer) startupActive(now time.Time) bool {
	return i.StartupBudget > 0 && !i.Started.IsZero() && now.Before(i.Started.Add(i.StartupBudget))
}

// rebuild is the per-pass state.
type rebuild struct {
	now     time.Time
	cutoff  time.Time
	seen    map[string]bool
	touched map[string]*sourceEntry // previous entry (nil when new) per key changed this pass
	docs    []Document
	stale   map[string][]string
	stats   RebuildStats
}

// Rebuild runs one incremental pass. It is a no-op (Skipped) while the
// startup budget is active.
func (i *Indexer) Rebuild(ctx context.Context) (RebuildStats, error) {
	if i.Backend == nil {
		return RebuildStats{}, errors.New("search: indexer has no backend")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.now()
	if i.startupActive(now) {
		return RebuildStats{Skipped: true}, nil
	}
	if i.entries == nil {
		i.entries = map[string]*sourceEntry{}
	}
	r := &rebuild{now: now, cutoff: now.AddDate(0, -i.retentionMonths(), 0), seen: map[string]bool{},
		touched: map[string]*sourceEntry{}, stale: map[string][]string{}}

	var errs []error
	walkFailed := false
	if err := i.ingestEvidence(ctx, r); err != nil {
		// A failed walk must not read as "every bundle was removed".
		i.markSeenUnder(r, i.EvidenceRoot)
		errs = append(errs, err)
		walkFailed = true
	}
	i.ingestBeads(r)
	if i.Facts != nil {
		i.ingestFacts(i.Facts(ctx), r)
	}
	if err := ctx.Err(); err != nil {
		i.rollback(r)
		return r.stats, err
	}
	i.expire(r)

	// A *CoverageError is a per-repo failure (for example a repo over its
	// index budget, which can persist for many passes): only that repo's
	// sources are forgotten and retried, so the other repos stay incremental.
	// Any other error forgets the whole pass.
	failed := map[string]bool{}
	repos := make([]string, 0, len(r.stale))
	for repo := range r.stale {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		if err := i.Backend.Delete(ctx, repo, r.stale[repo]); err != nil {
			if _, ok := coverageRepos(err); !ok {
				i.rollback(r)
				return r.stats, errors.Join(append(errs, err)...)
			}
			failed[repo] = true
			errs = append(errs, err)
			continue
		}
		r.stats.Deleted += len(r.stale[repo])
	}
	if len(r.docs) > 0 {
		if err := i.Backend.Index(ctx, r.docs); err != nil {
			cov, ok := coverageRepos(err)
			if !ok {
				i.rollback(r)
				return r.stats, errors.Join(append(errs, err)...)
			}
			for repo := range cov {
				failed[repo] = true
			}
			errs = append(errs, err)
		}
		for _, d := range r.docs {
			if !failed[d.Repo] {
				r.stats.Indexed++
			}
		}
	}
	if len(failed) > 0 {
		i.rollbackRepos(r, failed)
	}
	if !i.reconciled && !walkFailed {
		n, err := i.reconcile(ctx, failed)
		if err != nil {
			errs = append(errs, err)
		}
		r.stats.Deleted += n
	}
	i.recomputeHealth(now)
	return r.stats, errors.Join(errs...)
}

// coverageRepos reports the repos named by err when every error it wraps
// (through errors.Join) is a *CoverageError; ok is false otherwise.
func coverageRepos(err error) (repos map[string]bool, ok bool) {
	repos = map[string]bool{}
	var walk func(error) bool
	walk = func(err error) bool {
		if j, isJoin := err.(interface{ Unwrap() []error }); isJoin {
			for _, e := range j.Unwrap() {
				if !walk(e) {
					return false
				}
			}
			return true
		}
		var ce *CoverageError
		if errors.As(err, &ce) {
			repos[ce.Repo] = true
			return true
		}
		return false
	}
	if err == nil || !walk(err) {
		return nil, false
	}
	return repos, true
}

// rollback restores the entries changed in r to their previous state.
func (i *Indexer) rollback(r *rebuild) {
	for key, prev := range r.touched {
		i.restore(key, prev)
	}
}

// rollbackRepos restores the entries changed in r whose previous or new
// documents include any of repos, so the next pass retries only those
// sources.
func (i *Indexer) rollbackRepos(r *rebuild, repos map[string]bool) {
	for key, prev := range r.touched {
		if prev.touches(repos) || i.entries[key].touches(repos) {
			i.restore(key, prev)
		}
	}
}

func (i *Indexer) restore(key string, prev *sourceEntry) {
	if prev == nil {
		delete(i.entries, key)
	} else {
		i.entries[key] = prev
	}
}

// touches reports whether any of e's documents belongs to one of repos.
func (e *sourceEntry) touches(repos map[string]bool) bool {
	if e == nil {
		return false
	}
	for _, ref := range e.docs {
		if repos[ref.repo] {
			return true
		}
	}
	return false
}

// unchanged reports whether key's stamp matches the last pass, marking it
// seen either way.
func (i *Indexer) unchanged(r *rebuild, key, stamp string) bool {
	r.seen[key] = true
	if prev := i.entries[key]; prev != nil && prev.stamp == stamp {
		r.stats.Unchanged++
		return true
	}
	return false
}

// put records key's new documents, queueing them for indexing and queueing
// any document the source no longer produces for deletion.
func (i *Indexer) put(r *rebuild, key string, e *sourceEntry, docs []Document) {
	r.seen[key] = true
	prev := i.entries[key]
	if _, ok := r.touched[key]; !ok {
		r.touched[key] = prev
	}
	if prev != nil && !prev.first.IsZero() {
		e.first = prev.first
	} else if e.first.IsZero() {
		e.first = r.now
	}
	keep := map[docRef]bool{}
	for _, d := range docs {
		if d.At.Before(r.cutoff) {
			continue
		}
		d = i.finalize(d)
		if err := d.validate(); err != nil {
			r.stats.Invalid++
			i.logger().Debug("search: skipping invalid document", "source", key, "error", err)
			continue
		}
		ref := docRef{repo: d.Repo, id: d.ID, at: d.At}
		e.docs = append(e.docs, ref)
		keep[docRef{repo: d.Repo, id: d.ID}] = true
		r.docs = append(r.docs, d)
	}
	if prev != nil {
		for _, ref := range prev.docs {
			if !keep[docRef{repo: ref.repo, id: ref.id}] {
				r.stale[ref.repo] = append(r.stale[ref.repo], ref.id)
			}
		}
	}
	i.entries[key] = e
}

// markSeen keeps key's previous documents without re-reading the source
// (for example a file that could not be read this pass).
//
// On the first pass of a process there is no previous entry to keep, so a
// source that is unreadable or half-written at that moment contributes no
// live documents and reconcile removes whatever an earlier process indexed
// from it. That is transient: the source has no entry, so the next pass
// re-reads it (no mtime/size short-circuit) and re-indexes its documents.
func (i *Indexer) markSeen(r *rebuild, key string) { r.seen[key] = true }

// expire deletes the documents of sources that disappeared and documents
// that aged past the retention window.
func (i *Indexer) expire(r *rebuild) {
	for key, e := range i.entries {
		if !r.seen[key] {
			if _, ok := r.touched[key]; !ok {
				r.touched[key] = e
			}
			for _, ref := range e.docs {
				r.stale[ref.repo] = append(r.stale[ref.repo], ref.id)
			}
			delete(i.entries, key)
			continue
		}
		kept := e.docs[:0:0]
		for _, ref := range e.docs {
			if ref.at.Before(r.cutoff) {
				r.stale[ref.repo] = append(r.stale[ref.repo], ref.id)
				continue
			}
			kept = append(kept, ref)
		}
		if len(kept) != len(e.docs) {
			if _, ok := r.touched[key]; !ok {
				r.touched[key] = e
			}
			c := *e
			c.docs = kept
			i.entries[key] = &c
		}
	}
}

// reconcile runs until it has swept every repo once per process: documents
// of indexer kinds left in the backend by an earlier process that no current
// source produces (deleted sources, expired documents) are removed. Repos in
// skip (per-repo failures this pass), repos the backend could not read and
// repos whose deletes fail stay pending and are retried on the next pass;
// once none are pending the indexer is reconciled.
func (i *Indexer) reconcile(ctx context.Context, skip map[string]bool) (int, error) {
	live := map[docRef]bool{}
	for _, e := range i.entries {
		for _, ref := range e.docs {
			live[docRef{repo: ref.repo, id: ref.id}] = true
		}
	}
	q := Query{Filters: map[string][]string{FieldKind: indexerKinds}, Limit: math.MaxInt32}
	if i.pending != nil {
		repos := make([]string, 0, len(i.pending))
		for repo := range i.pending {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		q.Filters[FieldRepo] = repos
	}
	res, err := i.Backend.Query(ctx, q)
	if err != nil {
		return 0, err
	}
	pending := map[string]bool{}
	for repo := range skip {
		if i.pending == nil || i.pending[repo] {
			pending[repo] = true
		}
	}
	for repo := range res.Coverage {
		pending[repo] = true
	}
	gone := map[string][]string{}
	for _, h := range res.Hits {
		if d := h.Document; !pending[d.Repo] && !live[docRef{repo: d.Repo, id: d.ID}] {
			gone[d.Repo] = append(gone[d.Repo], d.ID)
		}
	}
	repos := make([]string, 0, len(gone))
	for repo := range gone {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	n := 0
	var errs []error
	for _, repo := range repos {
		if err := i.Backend.Delete(ctx, repo, gone[repo]); err != nil {
			pending[repo] = true
			errs = append(errs, err)
			continue
		}
		n += len(gone[repo])
	}
	i.pending = pending
	i.reconciled = len(pending) == 0
	return n, errors.Join(errs...)
}

// finalize scrubs every indexed string and applies the repo's visibility.
func (i *Indexer) finalize(d Document) Document {
	d.Title = logscrub.ScrubString(d.Title)
	d.Text = logscrub.ScrubString(d.Text)
	if len(d.Fields) > 0 {
		fields := make(map[string]string, len(d.Fields))
		for k, v := range d.Fields {
			if v = logscrub.ScrubString(v); v != "" {
				fields[k] = v
			}
		}
		d.Fields = fields
	}
	d.Visibility = Private
	if i.Visibility != nil && i.Visibility(d.Repo) == Public {
		d.Visibility = Public
	}
	return d
}

// Start runs Rebuild in the background: first once the startup budget has
// elapsed, then every Interval. The returned channel closes when the loop has
// stopped after ctx is cancelled.
func (i *Indexer) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		delay := time.Duration(0)
		if i.startupActive(i.now()) {
			delay = i.Started.Add(i.StartupBudget).Sub(i.now())
		}
		first := time.NewTimer(delay)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		i.runOnce(ctx)
		t := time.NewTicker(i.interval())
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				i.runOnce(ctx)
			}
		}
	}()
	return done
}

func (i *Indexer) runOnce(ctx context.Context) {
	start := time.Now()
	st, err := i.Rebuild(ctx)
	if err != nil && ctx.Err() == nil {
		i.logger().Warn("search: rebuild failed", "error", err)
	}
	i.logger().Debug("search: rebuild", "skipped", st.Skipped, "indexed", st.Indexed, "unchanged", st.Unchanged,
		"deleted", st.Deleted, "invalid", st.Invalid, "duration", time.Since(start))
}

func timeStamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func firstNonZero(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func repoURL(repo string) string { return fmt.Sprintf("https://github.com/%s", repo) }

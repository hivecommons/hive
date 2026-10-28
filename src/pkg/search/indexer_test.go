package search

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// recordingBackend counts what the indexer asks of the embedded backend.
type recordingBackend struct {
	*Embedded
	mu      sync.Mutex
	indexed []string
}

func (b *recordingBackend) Index(ctx context.Context, docs []Document) error {
	b.mu.Lock()
	for _, d := range docs {
		b.indexed = append(b.indexed, d.Repo+"#"+d.ID)
	}
	b.mu.Unlock()
	return b.Embedded.Index(ctx, docs)
}

func (b *recordingBackend) take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.indexed
	b.indexed = nil
	sort.Strings(out)
	return out
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func testBundle(repo string, n int, head string, updated time.Time) evidence.Bundle {
	return evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, Repo: repo, Number: n, HeadSHA: head,
		Author: evidence.Author{Login: "octo", Kind: "agent"},
		Verdicts: []evidence.Verdict{
			{Perspective: "security", Verdict: "changes_requested", RecordedAt: updated.Add(-2 * time.Hour),
				Findings: []evidence.Finding{{Path: "pkg/auth/token.go", Line: 12, Severity: "high", Summary: "token refresh skips expiry check"}}},
			{Perspective: "correctness", Verdict: "approve", RecordedAt: updated.Add(-time.Hour)},
		},
		Sentinel:  []evidence.SentinelFinding{{Rule: "workflow-permissions", Summary: "workflow grants write-all", Paths: []string{".github/workflows/ci.yml"}}},
		CI:        evidence.CI{Status: "success"},
		CreatedAt: updated.Add(-3 * time.Hour),
		UpdatedAt: updated,
	}
}

func writeBundle(t *testing.T, root string, b evidence.Bundle) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(b.Repo), strconv.Itoa(b.Number), b.HeadSHA+".json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type fixture14635 struct {
	root    string
	backend *recordingBackend
	clock   *clock
	store   *beads.Store
	facts   []knowledge.Fact
}

func newIndexerFixture(t *testing.T) (*fixture14635, *Indexer) {
	t.Helper()
	f := &fixture14635{root: t.TempDir(), clock: &clock{t: time.Now().UTC()}}
	f.backend = &recordingBackend{Embedded: newBackend(t, Options{})}
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	ix := &Indexer{
		Backend:      f.backend,
		EvidenceRoot: f.root,
		Beads:        func() map[string]*beads.Store { return map[string]*beads.Store{"scanner": f.store} },
		Facts:        func(context.Context) []knowledge.Fact { return f.facts },
		HomeRepo:     "org/home",
		Now:          f.clock.now,
	}
	return f, ix
}

func rebuildOK(t *testing.T, ix *Indexer) RebuildStats {
	t.Helper()
	st, err := ix.Rebuild(context.Background())
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	return st
}

func facetMap(res Result, field string) map[string]int {
	out := map[string]int{}
	for _, b := range res.Facets[field] {
		out[b.Value] = b.Count
	}
	return out
}

// Fixture evidence, complexity, bead and knowledge data become documents
// with verdict and sentinel.rule facets (hivecommons/hive#14635).
func TestIndexerFixtureDocumentsAndFacets(t *testing.T) {
	f, ix := newIndexerFixture(t)
	ix.Visibility = func(repo string) string {
		if repo == "org/a" {
			return Public
		}
		return Public + "-not"
	}
	writeBundle(t, f.root, testBundle("org/a", 7, "abc123", f.clock.t.Add(-time.Hour)))
	if _, err := complexity.Write(f.root, complexity.Snapshot{SchemaVersion: complexity.SchemaVersion, Repo: "org/a",
		Month: complexity.Month(f.clock.t), CapturedAt: f.clock.t.Add(-time.Hour), MaxCyclo: 31,
		Hotspots: []complexity.Function{{File: "pkg/auth/token.go", Name: "Refresh", Cyclo: 31}}}); err != nil {
		t.Fatal(err)
	}
	// Issue series files share the root but are not bundles.
	series := filepath.Join(f.root, "org", "a", evidence.IssueSeriesDirName, "2026-W01.json")
	if err := os.MkdirAll(filepath.Dir(series), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(series, []byte(`{"schema_version":"v1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create("Investigate flaky auth test", beads.TypeBug, beads.PriorityHigh, "scanner", "org/b#9"); err != nil {
		t.Fatal(err)
	}
	f.facts = []knowledge.Fact{{Slug: "auth-tokens", Title: "Auth tokens rotate hourly", Body: "Refresh before expiry.",
		Type: "convention", Layer: knowledge.LayerProject}}

	st := rebuildOK(t, ix)
	if st.Indexed != 6 {
		t.Fatalf("Indexed = %d, want 6 (bundle, finding, sentinel, complexity, bead, fact); stats %+v", st.Indexed, st)
	}
	res := query(t, f.backend.Embedded, Query{Facets: []string{"verdict", "sentinel.rule"}, Limit: 100})
	wantKinds := map[string]int{KindEvidence: 1, KindFinding: 1, KindSentinel: 1, KindComplexity: 1, KindBead: 1, KindFact: 1}
	if got := facetMap(res, FieldKind); !equalCounts(got, wantKinds) {
		t.Errorf("kind facet = %v, want %v", got, wantKinds)
	}
	if got := facetMap(res, "verdict"); got["approve"] != 1 || got["changes_requested"] != 1 {
		t.Errorf("verdict facet = %v, want bundle approve (latest) and finding changes_requested", got)
	}
	if got := facetMap(res, "sentinel.rule"); !equalCounts(got, map[string]int{"workflow-permissions": 1}) {
		t.Errorf("sentinel.rule facet = %v", got)
	}
	for _, h := range res.Hits {
		d := h.Document
		wantVis := Private
		if d.Repo == "org/a" {
			wantVis = Public
		}
		if d.Visibility != wantVis {
			t.Errorf("%s#%s visibility = %q, want %q", d.Repo, d.ID, d.Visibility, wantVis)
		}
		switch d.Kind {
		case KindBead:
			if d.Repo != "org/b" || d.URL != "https://github.com/org/b/issues/9" {
				t.Errorf("bead doc = %+v, want repo org/b from its external ref", d)
			}
		case KindFact:
			if d.Repo != "org/home" {
				t.Errorf("fact repo = %q, want HomeRepo", d.Repo)
			}
		case KindEvidence:
			if d.ID != "evidence/7/abc123" || d.URL != "https://github.com/org/a/pull/7" {
				t.Errorf("bundle doc id/url = %q %q", d.ID, d.URL)
			}
		}
	}
	if got := query(t, f.backend.Embedded, Query{Text: "token refresh expiry", Filters: map[string][]string{FieldKind: {KindFinding}}}); got.Total != 1 {
		t.Errorf("finding full-text hits = %d, want 1", got.Total)
	}
}

func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// A second rebuild re-indexes nothing; a bundle whose file changed but whose
// updated_at did not is skipped; a bundle with a new updated_at is the only
// one re-indexed, and documents it no longer yields are deleted.
func TestIndexerIncrementalRebuild(t *testing.T) {
	f, ix := newIndexerFixture(t)
	updated := f.clock.t.Add(-time.Hour)
	pa := writeBundle(t, f.root, testBundle("org/a", 1, "aaa", updated))
	writeBundle(t, f.root, testBundle("org/a", 2, "bbb", updated))
	bead, err := f.store.Create("bead one", beads.TypeTask, beads.PriorityLow, "scanner", "")
	if err != nil {
		t.Fatal(err)
	}
	rebuildOK(t, ix)
	if got := len(f.backend.take()); got != 7 {
		t.Fatalf("first rebuild indexed %d docs, want 7", got)
	}

	if st := rebuildOK(t, ix); st.Indexed != 0 || st.Unchanged != 3 {
		t.Fatalf("second rebuild = %+v, want nothing indexed and 3 unchanged sources", st)
	}
	if got := f.backend.take(); len(got) != 0 {
		t.Fatalf("second rebuild indexed %v", got)
	}

	// Rewritten with the same updated_at: re-read but not re-indexed.
	b := testBundle("org/a", 1, "aaa", updated)
	b.CI.Status = "pending"
	writeBundle(t, f.root, b)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(pa, later, later); err != nil {
		t.Fatal(err)
	}
	if st := rebuildOK(t, ix); st.Indexed != 0 {
		t.Fatalf("same updated_at rebuild indexed %d docs", st.Indexed)
	}

	// New revision of bundle 1 without its sentinel hit.
	b = testBundle("org/a", 1, "aaa", updated.Add(30*time.Minute))
	b.Sentinel = nil
	writeBundle(t, f.root, b)
	if err := f.store.Update(bead.ID, func(b *beads.Bead) { b.Status = beads.StatusClosed }); err != nil {
		t.Fatal(err)
	}
	st := rebuildOK(t, ix)
	want := []string{"org/a#evidence/1/aaa", "org/a#evidence/1/aaa/finding/0-0", "org/home#bead/scanner/" + bead.ID}
	if got := f.backend.take(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("incremental rebuild indexed %v, want %v", got, want)
	}
	if st.Deleted != 1 {
		t.Errorf("Deleted = %d, want 1 (dropped sentinel doc)", st.Deleted)
	}
	res := query(t, f.backend.Embedded, Query{Filters: map[string][]string{FieldKind: {KindSentinel}}})
	if got := ids(res); len(got) != 1 || got[0] != "org/a#evidence/2/bbb/sentinel/0" {
		t.Errorf("sentinel docs = %v, want only bundle 2's", got)
	}

	// A removed bundle's documents are deleted.
	if err := os.Remove(pa); err != nil {
		t.Fatal(err)
	}
	rebuildOK(t, ix)
	if got := query(t, f.backend.Embedded, Query{Filters: map[string][]string{"number": {"1"}}}); got.Total != 0 {
		t.Errorf("removed bundle still indexed: %v", ids(got))
	}
}

// Documents older than the retention window are never indexed and age out
// of the index; a new process reconciles leftovers on its first rebuild.
func TestIndexerRetention(t *testing.T) {
	f, ix := newIndexerFixture(t)
	ix.RetentionMonths = 24
	writeBundle(t, f.root, testBundle("org/a", 1, "old", f.clock.t.AddDate(0, -30, 0)))
	writeBundle(t, f.root, testBundle("org/a", 2, "new", f.clock.t.Add(-time.Hour)))
	rebuildOK(t, ix)
	res := query(t, f.backend.Embedded, Query{Filters: map[string][]string{FieldKind: {KindEvidence}}})
	if got := ids(res); len(got) != 1 || got[0] != "org/a#evidence/2/new" {
		t.Fatalf("evidence docs = %v, want only the in-window bundle", got)
	}

	f.clock.t = f.clock.t.AddDate(0, 25, 0)
	st := rebuildOK(t, ix)
	if st.Deleted != 3 {
		t.Errorf("Deleted = %d, want 3 (bundle, finding, sentinel aged out)", st.Deleted)
	}
	if res := query(t, f.backend.Embedded, Query{}); res.Total != 0 {
		t.Errorf("index after retention = %v, want empty", ids(res))
	}

	// Leftovers from an earlier process with no current source are dropped.
	f.clock.t = time.Now().UTC()
	if err := f.backend.Embedded.Index(context.Background(), []Document{
		doc("org/a", "evidence/9/gone", KindEvidence, f.clock.t, "gone", "", nil),
		doc("org/a", "forge/1", "issue", f.clock.t, "not ours", "", nil),
	}); err != nil {
		t.Fatal(err)
	}
	fresh := &Indexer{Backend: f.backend, EvidenceRoot: t.TempDir(), Now: f.clock.now}
	rebuildOK(t, fresh)
	if got := ids(query(t, f.backend.Embedded, Query{})); len(got) != 1 || got[0] != "org/a#forge/1" {
		t.Errorf("after reconcile = %v, want only the foreign-kind doc", got)
	}
}

// A secret pasted in a PR body (quoted by a finding, a human action and a
// bead's notes) never reaches the index, in memory or on disk.
func TestIndexerScrubsSecrets(t *testing.T) {
	f, ix := newIndexerFixture(t)
	const secret = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	body := "PR body: deploy with token " + secret + " please"
	b := testBundle("org/a", 3, "sss", f.clock.t.Add(-time.Hour))
	b.Verdicts[0].Findings[0].Summary = "quoted from the PR body: " + body
	b.HumanActions = []evidence.Action{{Actor: "octo", Kind: "comment", Detail: body, At: f.clock.t}}
	writeBundle(t, f.root, b)
	bead, err := f.store.Create("PR body follow-up "+secret, beads.TypeTask, beads.PriorityLow, "scanner", "org/a#3")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Update(bead.ID, func(b *beads.Bead) { b.Notes = body }); err != nil {
		t.Fatal(err)
	}
	f.facts = []knowledge.Fact{{Slug: "leak", Title: "leak", Body: body, Layer: knowledge.LayerProject}}
	rebuildOK(t, ix)

	if res := query(t, f.backend.Embedded, Query{Text: "deploy"}); res.Total == 0 {
		t.Fatal("scrubbed documents were not indexed at all")
	}
	if res := query(t, f.backend.Embedded, Query{Text: secret}); res.Total != 0 {
		t.Errorf("secret query matched %v", ids(res))
	}
	err = filepath.WalkDir(f.backend.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(data), "abcdefghijklmnopqrstuvwxyz0123456789") {
			t.Errorf("%s contains the secret", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// No ingest runs while the startup budget (#14389) is active.
func TestIndexerSkipsDuringStartupBudget(t *testing.T) {
	f, ix := newIndexerFixture(t)
	ix.Started = f.clock.t
	ix.StartupBudget = 10 * time.Minute
	writeBundle(t, f.root, testBundle("org/a", 1, "aaa", f.clock.t.Add(-time.Hour)))
	if st := rebuildOK(t, ix); !st.Skipped || st.Indexed != 0 {
		t.Fatalf("rebuild during startup budget = %+v, want skipped", st)
	}
	if got := f.backend.take(); len(got) != 0 {
		t.Fatalf("indexed during startup budget: %v", got)
	}
	f.clock.t = f.clock.t.Add(10 * time.Minute)
	if st := rebuildOK(t, ix); st.Skipped || st.Indexed == 0 {
		t.Fatalf("rebuild after startup budget = %+v, want indexed", st)
	}
}

// Start waits out the startup budget and stops when ctx is cancelled.
func TestIndexerStartStops(t *testing.T) {
	f, ix := newIndexerFixture(t)
	ix.Now = nil
	ix.Started = time.Now()
	ix.StartupBudget = time.Hour
	writeBundle(t, f.root, testBundle("org/a", 1, "aaa", time.Now().Add(-time.Hour)))
	ctx, cancel := context.WithCancel(context.Background())
	done := ix.Start(ctx)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("indexer loop did not stop")
	}
	if got := f.backend.take(); len(got) != 0 {
		t.Fatalf("indexed before the startup budget elapsed: %v", got)
	}
}

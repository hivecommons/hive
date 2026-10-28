package search

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func doc(repo, id, kind string, at time.Time, title, text string, fields map[string]string) Document {
	return Document{ID: id, Repo: repo, Kind: kind, At: at, URL: "https://github.com/" + repo + "/pull/" + id,
		Visibility: Public, Title: title, Text: text, Fields: fields}
}

func newBackend(t *testing.T, opts Options) *Embedded {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	e, err := NewEmbedded(opts)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func fixture() []Document {
	return []Document{
		doc("org/a", "1", "evidence", t0, "Fix auth token refresh", "reviewer flagged auth.token_refresh path\nunrelated line", map[string]string{"verdict": "changes_requested"}),
		doc("org/a", "2", "bead", t0.Add(time.Hour), "Auth notes", "token cache", map[string]string{"verdict": "approved"}),
		doc("org/b", "3", "evidence", t0.Add(2*time.Hour), "Docs", "nothing relevant", nil),
	}
}

func query(t *testing.T, e *Embedded, q Query) Result {
	t.Helper()
	res, err := e.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func ids(res Result) []string {
	var out []string
	for _, h := range res.Hits {
		out = append(out, h.Document.Repo+"#"+h.Document.ID)
	}
	return out
}

func TestIndexQueryFacetsAndHighlight(t *testing.T) {
	e := newBackend(t, Options{})
	if err := e.Index(context.Background(), fixture()); err != nil {
		t.Fatal(err)
	}
	res := query(t, e, Query{Text: "AUTH token", Facets: []string{"verdict", "kind", ""}})
	if got := ids(res); !reflect.DeepEqual(got, []string{"org/a#2", "org/a#1"}) || res.Total != 2 {
		t.Fatalf("hits=%v total=%d", got, res.Total)
	}
	if len(res.Coverage) != 0 {
		t.Fatalf("coverage=%v", res.Coverage)
	}
	h := res.Hits[1]
	if h.Score != 2 || !reflect.DeepEqual(h.Terms, []string{"auth", "token"}) {
		t.Fatalf("hit=%+v", h)
	}
	if h.Highlight.Snippet != "Fix auth token refresh" || !reflect.DeepEqual(h.Highlight.Spans, [][2]int{{4, 8}, {9, 14}}) {
		t.Fatalf("highlight=%+v", h.Highlight)
	}
	if got := res.Hits[0].Highlight; got.Snippet != "Auth notes" || !reflect.DeepEqual(got.Spans, [][2]int{{0, 4}}) {
		t.Fatalf("highlight=%+v", got)
	}
	want := map[string][]FacetCount{
		"kind":    {{"bead", 1}, {"evidence", 1}},
		"repo":    {{"org/a", 2}},
		"verdict": {{"approved", 1}, {"changes_requested", 1}},
	}
	if !reflect.DeepEqual(res.Facets, want) {
		t.Fatalf("facets=%v", res.Facets)
	}

	// Facets count all matches, not just the returned page.
	res = query(t, e, Query{Limit: 1})
	if res.Total != 3 || len(res.Hits) != 1 || ids(res)[0] != "org/b#3" {
		t.Fatalf("hits=%v total=%d", ids(res), res.Total)
	}
	if !reflect.DeepEqual(res.Facets["kind"], []FacetCount{{"evidence", 2}, {"bead", 1}}) ||
		!reflect.DeepEqual(res.Facets["repo"], []FacetCount{{"org/a", 2}, {"org/b", 1}}) {
		t.Fatalf("facets=%v", res.Facets)
	}
	if hl := res.Hits[0].Highlight; hl.Snippet != "Docs" || hl.Spans != nil {
		t.Fatalf("highlight=%+v", hl)
	}

	res = query(t, e, Query{Text: "zzz"})
	if res.Total != 0 || res.Hits != nil || len(res.Facets["kind"]) != 0 {
		t.Fatalf("res=%+v", res)
	}
}

func TestQueryExactTermsAndComponents(t *testing.T) {
	e := newBackend(t, Options{})
	d := doc("org/a", "1", "issue", t0, "", "set triage.prior_art\nuse --max-findings now", nil)
	if err := e.Index(context.Background(), []Document{d}); err != nil {
		t.Fatal(err)
	}
	res := query(t, e, Query{Text: "--max-findings"})
	if len(res.Hits) != 1 || res.Hits[0].Highlight.Snippet != "use --max-findings now" ||
		!reflect.DeepEqual(res.Hits[0].Highlight.Spans, [][2]int{{4, 18}}) {
		t.Fatalf("res=%+v", res.Hits)
	}
	res = query(t, e, Query{Text: "prior"})
	if len(res.Hits) != 1 || res.Hits[0].Highlight.Snippet != "set triage.prior_art" {
		t.Fatalf("res=%+v", res.Hits)
	}
	if len(query(t, e, Query{Text: "find"}).Hits) != 0 {
		t.Fatal("terms must match exactly, not by prefix")
	}
}

func TestQueryNumericTermsAndZeroTokenText(t *testing.T) {
	e := newBackend(t, Options{})
	docs := []Document{
		doc("org/a", "1", "issue", t0, "Follow-up to #14491", "see PR 14634", nil),
		doc("org/a", "2", "issue", t0, "Other", "nothing", nil),
	}
	if err := e.Index(context.Background(), docs); err != nil {
		t.Fatal(err)
	}
	res := query(t, e, Query{Text: "14491"})
	if got := ids(res); !reflect.DeepEqual(got, []string{"org/a#1"}) || res.Hits[0].Score != 1 {
		t.Fatalf("hits=%v res=%+v", got, res.Hits)
	}
	if hl := res.Hits[0].Highlight; hl.Snippet != "Follow-up to #14491" || !reflect.DeepEqual(hl.Spans, [][2]int{{14, 19}}) {
		t.Fatalf("highlight=%+v", hl)
	}
	if got := ids(query(t, e, Query{Text: "pr 14634"})); !reflect.DeepEqual(got, []string{"org/a#1"}) {
		t.Fatalf("hits=%v", got)
	}
	for _, text := range []string{"#", "...", "@@ !!", "99999"} {
		if res := query(t, e, Query{Text: text}); res.Total != 0 || res.Hits != nil {
			t.Fatalf("Text=%q must match nothing, got %+v", text, res)
		}
	}
	if res := query(t, e, Query{Text: "  "}); res.Total != 2 {
		t.Fatalf("blank text must match everything, got %+v", res)
	}
}

func TestQueryFiltersAndTimeRange(t *testing.T) {
	e := newBackend(t, Options{})
	if err := e.Index(context.Background(), fixture()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		q    Query
		want []string
	}{
		{"kind", Query{Filters: map[string][]string{"kind": {"evidence"}}}, []string{"org/b#3", "org/a#1"}},
		{"field or", Query{Filters: map[string][]string{"verdict": {"approved", "changes_requested"}}, From: t0.Add(30 * time.Minute)}, []string{"org/a#2"}},
		{"to exclusive", Query{To: t0.Add(time.Hour)}, []string{"org/a#1"}},
		{"repo", Query{Filters: map[string][]string{"repo": {"bad repo", "org/b"}}}, []string{"org/b#3"}},
		{"visibility", Query{Filters: map[string][]string{"visibility": {Private}}}, nil},
		{"empty values ignored", Query{Filters: map[string][]string{"kind": {}}}, []string{"org/b#3", "org/a#2", "org/a#1"}},
		{"text and filter", Query{Text: "token", Filters: map[string][]string{"kind": {"bead"}}}, []string{"org/a#2"}},
		{"unindexed repo", Query{Filters: map[string][]string{"repo": {"org/none"}}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := query(t, e, tc.q)
			if got := ids(res); !reflect.DeepEqual(got, tc.want) || res.Total != len(tc.want) || len(res.Coverage) != 0 {
				t.Fatalf("hits=%v total=%d coverage=%v", got, res.Total, res.Coverage)
			}
		})
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	e := newBackend(t, Options{})
	if err := e.Index(ctx, fixture()); err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(ctx, "org/a", []string{"1", "missing"}); err != nil {
		t.Fatal(err)
	}
	if got := ids(query(t, e, Query{})); !reflect.DeepEqual(got, []string{"org/b#3", "org/a#2"}) {
		t.Fatalf("hits=%v", got)
	}
	path := filepath.Join(e.dir, "org", "a.idx")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(ctx, "org/a", []string{"missing"}); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("no-op delete rewrote the index")
	}
	if err := e.Delete(ctx, "../a", []string{"1"}); err == nil {
		t.Fatal("invalid repo accepted")
	}
	// Re-indexing an ID replaces the document.
	if err := e.Index(ctx, []Document{doc("org/a", "2", "issue", t0, "replaced", "", nil)}); err != nil {
		t.Fatal(err)
	}
	res := query(t, e, Query{Filters: map[string][]string{"repo": {"org/a"}}})
	if len(res.Hits) != 1 || res.Hits[0].Document.Kind != "issue" {
		t.Fatalf("hits=%+v", res.Hits)
	}
}

func TestDocumentValidation(t *testing.T) {
	valid := doc("org/a", "1", "bead", t0, "", "", nil)
	cases := map[string]func(*Document){
		"repo without owner": func(d *Document) { d.Repo = "org" },
		"dot owner":          func(d *Document) { d.Repo = "../a" },
		"dot name":           func(d *Document) { d.Repo = "org/.." },
		"id":                 func(d *Document) { d.ID = "" },
		"kind":               func(d *Document) { d.Kind = "" },
		"at":                 func(d *Document) { d.At = time.Time{} },
		"url":                func(d *Document) { d.URL = "" },
		"visibility":         func(d *Document) { d.Visibility = "secret" },
		"reserved field":     func(d *Document) { d.Fields = map[string]string{"kind": "x"} },
		"empty field":        func(d *Document) { d.Fields = map[string]string{"": "x"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := newBackend(t, Options{})
			bad := valid
			mutate(&bad)
			if err := e.Index(context.Background(), []Document{valid, bad}); err == nil {
				t.Fatal("invalid document accepted")
			}
			st, _ := e.Stats(context.Background())
			if len(st.Repos) != 0 {
				t.Fatalf("batch partially written: %v", st.Repos)
			}
		})
	}
	private := valid
	private.Visibility = Private
	if err := private.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotsAreImmutable(t *testing.T) {
	e := newBackend(t, Options{})
	fields := map[string]string{"verdict": "approved"}
	if err := e.Index(context.Background(), []Document{doc("org/a", "1", "bead", t0, "x", "", fields)}); err != nil {
		t.Fatal(err)
	}
	fields["verdict"] = "mutated"
	res := query(t, e, Query{Filters: map[string][]string{"verdict": {"approved"}}})
	if len(res.Hits) != 1 {
		t.Fatalf("caller mutation leaked into index: %+v", res)
	}
	res.Hits[0].Document.Fields["verdict"] = "mutated"
	if got := query(t, e, Query{}).Hits[0].Document.Fields["verdict"]; got != "approved" {
		t.Fatalf("result mutation leaked into index: %q", got)
	}
}

func TestSizeBudgetIsCoverageError(t *testing.T) {
	ctx := context.Background()
	small := doc("org/a", "1", "bead", t0, "auth", "", nil)
	data, err := encode("org/a", []Document{small})
	if err != nil {
		t.Fatal(err)
	}
	e := newBackend(t, Options{MaxIndexBytesPerRepo: int64(len(data)) + 64})
	if err := e.Index(ctx, []Document{small}); err != nil {
		t.Fatal(err)
	}
	big := doc("org/a", "2", "bead", t0, "auth", strings.Repeat("x ", 1000), nil)
	other := doc("org/b", "1", "bead", t0, "auth", "", nil)
	err = e.Index(ctx, []Document{big, other})
	var ce *CoverageError
	if !errors.Is(err, ErrIndexBudget) || !errors.As(err, &ce) || ce.Repo != "org/a" || !strings.Contains(ce.Error(), "org/a") {
		t.Fatalf("err=%v", err)
	}
	// The previous org/a snapshot stays published; org/b was still indexed.
	res := query(t, e, Query{Text: "auth"})
	if got := ids(res); !reflect.DeepEqual(got, []string{"org/a#1", "org/b#1"}) {
		t.Fatalf("hits=%v", got)
	}
	if !errors.Is(res.Coverage["org/a"], ErrIndexBudget) || len(res.Coverage) != 1 {
		t.Fatalf("coverage=%v", res.Coverage)
	}
	if st, _ := e.Stats(ctx); !errors.Is(st.Coverage["org/a"], ErrIndexBudget) {
		t.Fatalf("stats coverage=%v", st.Coverage)
	}
	// A successful publish clears the coverage error.
	if err := e.Index(ctx, []Document{doc("org/a", "1", "issue", t0, "auth", "", nil)}); err != nil {
		t.Fatal(err)
	}
	if res := query(t, e, Query{Text: "auth"}); len(res.Coverage) != 0 {
		t.Fatalf("coverage=%v", res.Coverage)
	}
}

func TestPublishFailuresAreCoverageErrors(t *testing.T) {
	ctx := context.Background()
	e := newBackend(t, Options{})
	// encoding/json cannot represent years past 9999.
	err := e.Index(ctx, []Document{doc("org/a", "1", "bead", time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), "", "", nil)})
	var ce *CoverageError
	if !errors.As(err, &ce) || ce.Repo != "org/a" || errors.Is(err, ErrIndexBudget) {
		t.Fatalf("err=%v", err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "blocked"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.Index(ctx, []Document{doc("blocked/a", "1", "bead", t0, "", "", nil)}); !errors.As(err, &ce) || ce.Repo != "blocked/a" {
		t.Fatalf("err=%v", err)
	}
}

func TestPersistAndReload(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e1 := newBackend(t, Options{Dir: dir})
	docs := fixture()
	if err := e1.Index(ctx, docs); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "org", "a.idx"))
	if err != nil || !strings.HasPrefix(string(data), fileMagic) {
		t.Fatalf("index file: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "org"))
	if err != nil || len(entries) != 2 || entries[0].Name() != "a.idx" || entries[1].Name() != "b.idx" {
		t.Fatalf("temp files left behind: %v %v", entries, err)
	}

	e2 := newBackend(t, Options{Dir: dir})
	st, err := e2.Stats(ctx)
	if err != nil || st.Repos["org/a"] != (RepoStats{Documents: -1, Bytes: int64(len(data))}) || st.MemoryBytes != 0 {
		t.Fatalf("stats=%+v err=%v", st, err)
	}
	res := query(t, e2, Query{Text: "auth token", Filters: map[string][]string{"repo": {"org/a"}}})
	if len(res.Hits) != 2 {
		t.Fatalf("hits=%v", ids(res))
	}
	got := res.Hits[1].Document
	if !got.At.Equal(docs[0].At) {
		t.Fatalf("at=%v", got.At)
	}
	got.At = docs[0].At
	if !reflect.DeepEqual(got, docs[0]) {
		t.Fatalf("reloaded %+v, want %+v", got, docs[0])
	}
	st, _ = e2.Stats(ctx)
	if st.Repos["org/a"] != (RepoStats{Documents: 2, Bytes: int64(len(data)), Loaded: true}) || st.MemoryBytes != int64(len(data)) {
		t.Fatalf("stats=%+v", st)
	}
	if st.MaxIndexBytesPerRepo != DefaultMaxIndexBytesPerRepo || st.MaxMemoryBytes != DefaultMaxMemoryBytes {
		t.Fatalf("defaults=%+v", st)
	}
}

func TestCorruptIndexIgnoredAndRebuilt(t *testing.T) {
	cases := map[string]string{
		"header":       "garbage",
		"json":         fileMagic + "{",
		"other repo":   fileMagic + `{"repo":"org/x","docs":[]}`,
		"invalid doc":  fileMagic + `{"repo":"org/a","docs":[{"id":"1","repo":"org/a"}]}`,
		"duplicate id": fileMagic + `{"repo":"org/a","docs":[` + strings.Repeat(`{"id":"1","repo":"org/a","kind":"k","at":"2026-01-01T00:00:00Z","url":"u","visibility":"public"},`, 2) + `{}]}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "org"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "org", "a.idx"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			e := newBackend(t, Options{Dir: dir})
			res := query(t, e, Query{})
			if len(res.Hits) != 0 || !errors.Is(res.Coverage["org/a"], ErrCorruptIndex) {
				t.Fatalf("res=%+v", res)
			}
			if err := e.Index(ctx, []Document{doc("org/a", "9", "bead", t0, "rebuilt", "", nil)}); err != nil {
				t.Fatal(err)
			}
			res = query(t, e, Query{})
			if got := ids(res); !reflect.DeepEqual(got, []string{"org/a#9"}) || len(res.Coverage) != 0 {
				t.Fatalf("hits=%v coverage=%v", got, res.Coverage)
			}
			if res := query(t, newBackend(t, Options{Dir: dir}), Query{}); len(res.Hits) != 1 {
				t.Fatalf("rebuilt file not reloadable: %+v", res)
			}
		})
	}
}

func TestCorruptIndexReplacedByNoopDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "org"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "org", "a.idx"), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newBackend(t, Options{Dir: dir})
	if res := query(t, e, Query{}); !errors.Is(res.Coverage["org/a"], ErrCorruptIndex) {
		t.Fatalf("res=%+v", res)
	}
	if err := e.Delete(ctx, "org/a", []string{"absent"}); err != nil {
		t.Fatal(err)
	}
	if res := query(t, e, Query{}); len(res.Coverage) != 0 || res.Total != 0 {
		t.Fatalf("coverage must clear after Delete, got %+v", res)
	}
	sh, err := readShard(dir, "org/a")
	if err != nil || sh == nil || len(sh.docs) != 0 {
		t.Fatalf("file not rewritten as empty index: sh=%+v err=%v", sh, err)
	}
	// A healthy repo is not rewritten by a no-op Delete.
	if err := e.Index(ctx, []Document{doc("org/a", "1", "bead", t0, "x", "", nil)}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(indexPath(dir, "org/a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(ctx, "org/a", []string{"absent"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(indexPath(dir, "org/a"))
	if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("no-op Delete rewrote a healthy index: before=%v after=%v err=%v", before.ModTime(), after.ModTime(), err)
	}
}

func TestUnreadableIndexIsCoverageError(t *testing.T) {
	e := newBackend(t, Options{})
	if err := os.MkdirAll(filepath.Join(e.dir, "org", "a.idx"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.dir, "bad owner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "bad owner", "x.idx"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := e.Stats(context.Background())
	if len(st.Repos) != 0 {
		t.Fatalf("non-index entries listed: %v", st.Repos)
	}
	res := query(t, e, Query{Filters: map[string][]string{"repo": {"org/a"}}})
	var ce *CoverageError
	if !errors.As(res.Coverage["org/a"], &ce) || errors.Is(ce, ErrCorruptIndex) {
		t.Fatalf("coverage=%v", res.Coverage)
	}
}

func TestUnreadableIndexIsNotOverwritten(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 0 does not block root")
	}
	ctx := context.Background()
	dir := t.TempDir()
	seed := newBackend(t, Options{Dir: dir})
	if err := seed.Index(ctx, []Document{doc("org/a", "1", "bead", t0, "old", "", nil)}); err != nil {
		t.Fatal(err)
	}
	path := indexPath(dir, "org/a")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	e := newBackend(t, Options{Dir: dir})
	err = e.Index(ctx, []Document{doc("org/a", "2", "bead", t0, "new", "", nil)})
	var ce *CoverageError
	if !errors.As(err, &ce) || ce.Repo != "org/a" || errors.Is(ce, ErrCorruptIndex) {
		t.Fatalf("err=%v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("unreadable index was overwritten")
	}
	res := query(t, newBackend(t, Options{Dir: dir}), Query{Filters: map[string][]string{"repo": {"org/a"}}})
	if got := ids(res); !reflect.DeepEqual(got, []string{"org/a#1"}) {
		t.Fatalf("hits=%v", got)
	}
}

func TestMemoryCapEvictsLeastRecentlyQueried(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	seed := newBackend(t, Options{Dir: dir})
	for _, repo := range []string{"org/a", "org/b", "org/c"} {
		if err := seed.Index(ctx, []Document{doc(repo, "1", "bead", t0, "same", "", nil)}); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := seed.Stats(ctx)
	total := st.Repos["org/a"].Bytes + st.Repos["org/b"].Bytes + st.Repos["org/c"].Bytes

	e := newBackend(t, Options{Dir: dir, MaxMemoryBytes: total - 1})
	loaded := func() []bool {
		st, _ := e.Stats(ctx)
		if st.MemoryBytes > total-1 {
			t.Fatalf("memory %d over cap", st.MemoryBytes)
		}
		return []bool{st.Repos["org/a"].Loaded, st.Repos["org/b"].Loaded, st.Repos["org/c"].Loaded}
	}
	for _, repo := range []string{"org/a", "org/b", "org/c"} {
		query(t, e, Query{Filters: map[string][]string{"repo": {repo}}})
	}
	if got := loaded(); !reflect.DeepEqual(got, []bool{false, true, true}) {
		t.Fatalf("loaded=%v", got)
	}
	query(t, e, Query{Filters: map[string][]string{"repo": {"org/a"}}})
	if got := loaded(); !reflect.DeepEqual(got, []bool{true, false, true}) {
		t.Fatalf("loaded=%v", got)
	}
	// Evicted repos still answer queries by reloading from disk.
	if res := query(t, e, Query{Text: "same"}); res.Total != 3 {
		t.Fatalf("total=%d", res.Total)
	}

	// A snapshot larger than the whole budget is still usable on its own.
	tiny := newBackend(t, Options{Dir: dir, MaxMemoryBytes: 1})
	if err := tiny.Index(ctx, []Document{doc("org/d", "1", "bead", t0, "same", "", nil)}); err != nil {
		t.Fatal(err)
	}
	if res := query(t, tiny, Query{Text: "same"}); res.Total != 4 {
		t.Fatalf("total=%d", res.Total)
	}
	st, _ = tiny.Stats(ctx)
	if n := st.Repos["org/a"].Loaded || st.Repos["org/b"].Loaded || st.Repos["org/c"].Loaded; n || !st.Repos["org/d"].Loaded {
		t.Fatalf("stats=%+v", st.Repos)
	}
}

func TestHighlightClipsLongLines(t *testing.T) {
	line := strings.Repeat("€", 200) + " needle " + strings.Repeat("€", 200)
	hl := highlight(Document{Text: line}, []string{"needle"})
	if len(hl.Snippet) > maxSnippetBytes || !utf8.ValidString(hl.Snippet) || len(hl.Spans) != 1 ||
		hl.Snippet[hl.Spans[0][0]:hl.Spans[0][1]] != "needle" {
		t.Fatalf("highlight=%q %v", hl.Snippet, hl.Spans)
	}
	line = "needle " + strings.Repeat("€", 200) + " needle"
	hl = highlight(Document{Title: line}, []string{"needle"})
	if !strings.HasPrefix(hl.Snippet, "needle ") || !reflect.DeepEqual(hl.Spans, [][2]int{{0, 6}}) {
		t.Fatalf("highlight=%q %v", hl.Snippet, hl.Spans)
	}
	hl = highlight(Document{Text: strings.Repeat("é", 300)}, nil)
	if len(hl.Snippet) != maxSnippetBytes || !utf8.ValidString(hl.Snippet) || hl.Spans != nil {
		t.Fatalf("highlight len=%d", len(hl.Snippet))
	}
	if hl := highlight(Document{}, []string{"x"}); hl.Snippet != "" || hl.Spans != nil {
		t.Fatalf("highlight=%+v", hl)
	}
}

func TestCanceledContext(t *testing.T) {
	e := newBackend(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Query(ctx, Query{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("query err=%v", err)
	}
	if _, err := e.Stats(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("stats err=%v", err)
	}
	if err := e.Index(ctx, fixture()); !errors.Is(err, context.Canceled) {
		t.Fatalf("index err=%v", err)
	}
	if st, _ := e.Stats(context.Background()); len(st.Repos) != 0 {
		t.Fatalf("canceled index wrote %v", st.Repos)
	}
}

func TestNewEmbeddedRejectsUnusableDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEmbedded(Options{Dir: filepath.Join(file, "search")}); err == nil {
		t.Fatal("expected error")
	}
}

func TestConcurrentQueriesSeeWholeSnapshots(t *testing.T) {
	ctx := context.Background()
	e := newBackend(t, Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				res, err := e.Query(ctx, Query{Text: "pair"})
				if err != nil {
					errs <- err
					return
				}
				if res.Total%2 != 0 {
					errs <- errors.New("observed a half-published batch")
					return
				}
			}
		}()
	}
	for i := 0; i < 25; i++ {
		id := strings.Repeat("x", i+1)
		if err := e.Index(ctx, []Document{doc("org/a", id+"1", "bead", t0, "pair", "", nil), doc("org/a", id+"2", "bead", t0, "pair", "", nil)}); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

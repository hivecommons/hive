package rfccoverage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func loadIndex(t *testing.T) []Setting {
	t.Helper()
	page, err := os.ReadFile(filepath.Join("testdata", "settings_index.html"))
	if err != nil {
		t.Fatal(err)
	}
	return ParseSettingsIndex(page)
}

func loadRFC(t *testing.T, name string) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	title, body, _ := strings.Cut(string(raw), "\n")
	return title, body
}

type fakeForge struct {
	marked    map[string]bool
	comments  map[string][]string
	lookups   int
	lookupErr error
	createErr error
}

func newFakeForge() *fakeForge {
	return &fakeForge{marked: map[string]bool{}, comments: map[string][]string{}}
}

func (f *fakeForge) FindMarkerComment(_ context.Context, repo string, number int, marker string) (bool, error) {
	f.lookups++
	if f.lookupErr != nil {
		return false, f.lookupErr
	}
	k := Candidate{Repo: repo, Number: number}.Key()
	for _, c := range f.comments[k] {
		if strings.Contains(c, marker) {
			return true, nil
		}
	}
	return f.marked[k], nil
}

func (f *fakeForge) CreateComment(_ context.Context, repo string, number int, body string) error {
	if f.createErr != nil {
		return f.createErr
	}
	k := Candidate{Repo: repo, Number: number}.Key()
	f.comments[k] = append(f.comments[k], body)
	return nil
}

var testNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func rfcCandidate(t *testing.T, number int, fixture string) Candidate {
	title, body := loadRFC(t, fixture)
	return Candidate{Repo: "acme/widgets", Number: number, Title: title, Body: body, Labels: []string{"RFC"}, CreatedAt: testNow.Add(-time.Hour)}
}

func testOptions(t *testing.T) Options {
	return Options{
		Settings:   loadIndex(t),
		Origin:     "https://hive.example/",
		Labels:     []string{"rfc", "enhancement"},
		MaxMatches: 3,
		MaxAge:     7 * 24 * time.Hour,
		Now:        testNow,
	}
}

func TestParseSettingsIndex(t *testing.T) {
	got := loadIndex(t)
	if len(got) != 7 {
		t.Fatalf("parsed %d entries, want 7 (keyless and out-of-table entries dropped): %+v", len(got), got)
	}
	var quoted *Setting
	for i := range got {
		if got[i].Key == "not.indexed" {
			t.Fatal("parsed an entry outside SETTINGS_SEARCH_STATIC_HITS")
		}
		if got[i].Key == "edge.quoted" {
			quoted = &got[i]
		}
	}
	if quoted == nil || quoted.Label != "Reviewer's note" || quoted.Text != "it's a quoted entry" || quoted.Section != "Edge" || quoted.Tab != "Features" {
		t.Fatalf("escaped entry = %+v", quoted)
	}
	if ParseSettingsIndex([]byte("<html>no index</html>")) != nil {
		t.Fatal("a page without the index must parse to nil")
	}
}

func TestTokens(t *testing.T) {
	got := Tokens("Please add the duplicate_sweep.enabled Labels to PRs, it's https://x.io")
	want := []string{"duplicate", "sweep", "label", "prs"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Tokens = %v, want %v", got, want)
	}
	if got := Tokens("glass class"); !reflect.DeepEqual(got, []string{"glass", "class"}) {
		t.Fatalf("double-s words must keep their s: %v", got)
	}
}

// The known-duplicate fixture asks for close-on-merge, which the hive already
// has: the comment must list it first with its Settings deep link.
func TestKnownDuplicateRFCListsExistingSetting(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	res := s.Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 7, "known_duplicate_rfc.md")}, testOptions(t))
	if len(res.Commented) != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v", res)
	}
	comments := forge.comments["acme/widgets#7"]
	if len(comments) != 1 {
		t.Fatalf("got %d comments", len(comments))
	}
	body := comments[0]
	if !strings.HasPrefix(body, Marker+"\n") {
		t.Fatalf("comment must open with the marker:\n%s", body)
	}
	link := "[Close issues on merge](https://hive.example/#config/governor/Features/issues.close_on_merge)"
	first := strings.Index(body, "\n- ")
	if first < 0 || !strings.HasPrefix(body[first+1:], "- "+link) {
		t.Fatalf("close-on-merge must be the first match with its Settings link %s:\n%s", link, body)
	}
	if !strings.Contains(body, DocsSearchURL("issues.close_on_merge")) {
		t.Fatalf("missing docs link:\n%s", body)
	}
	if strings.Count(body, "\n- ") > 3 {
		t.Fatalf("MaxMatches not honoured:\n%s", body)
	}
	if !strings.Contains(body, "not a decision") {
		t.Fatalf("comment must say it is advisory:\n%s", body)
	}

	// Never repost: not from memory, and not after a restart (marker).
	again := s.Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 7, "known_duplicate_rfc.md")}, testOptions(t))
	if len(again.Commented) != 0 || again.Skipped[SkipDone] != 1 {
		t.Fatalf("second pass = %+v", again)
	}
	restarted := NewSweeper().Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 7, "known_duplicate_rfc.md")}, testOptions(t))
	if len(restarted.Commented) != 0 || restarted.Skipped[SkipDone] != 1 || len(forge.comments["acme/widgets#7"]) != 1 {
		t.Fatalf("restart reposted: %+v", restarted)
	}
}

func TestNothingFound(t *testing.T) {
	forge := newFakeForge()
	opts := testOptions(t)
	res := NewSweeper().Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 8, "unrelated_rfc.md")}, opts)
	if len(res.Commented) != 0 || res.Skipped[SkipNothingFound] != 1 || forge.lookups != 0 {
		t.Fatalf("no match must cost no call and post nothing by default: %+v lookups=%d", res, forge.lookups)
	}
	opts.PostWhenNothingFound = true
	s := NewSweeper()
	res = s.Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 8, "unrelated_rfc.md")}, opts)
	if len(res.Commented) != 1 || !strings.Contains(forge.comments["acme/widgets#8"][0], "Nothing found") {
		t.Fatalf("nothing-found comment = %+v %v", res, forge.comments)
	}
	if res = s.Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 8, "unrelated_rfc.md")}, opts); len(res.Commented) != 0 {
		t.Fatal("nothing-found comment reposted")
	}
}

func TestSweepSkipsAndErrors(t *testing.T) {
	opts := testOptions(t)
	notRFC := rfcCandidate(t, 1, "known_duplicate_rfc.md")
	notRFC.Labels = []string{"bug"}
	old := rfcCandidate(t, 2, "known_duplicate_rfc.md")
	old.CreatedAt = testNow.Add(-30 * 24 * time.Hour)
	forge := newFakeForge()
	res := NewSweeper().Sweep(context.Background(), forge, []Candidate{notRFC, old}, opts)
	if res.Scanned != 2 || res.Skipped[SkipNotRFC] != 1 || res.Skipped[SkipTooOld] != 1 || forge.lookups != 0 {
		t.Fatalf("result = %+v", res)
	}

	forge.lookupErr = errors.New("boom")
	if res := NewSweeper().Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 3, "known_duplicate_rfc.md")}, opts); res.Errors != 1 {
		t.Fatalf("lookup error not counted: %+v", res)
	}
	forge.lookupErr, forge.createErr = nil, errors.New("boom")
	s := NewSweeper()
	if res := s.Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 3, "known_duplicate_rfc.md")}, opts); res.Errors != 1 {
		t.Fatalf("create error not counted: %+v", res)
	}
	forge.createErr = nil
	if res := s.Sweep(context.Background(), forge, []Candidate{rfcCandidate(t, 3, "known_duplicate_rfc.md")}, opts); len(res.Commented) != 1 {
		t.Fatalf("a failed comment must be retried next pass: %+v", res)
	}

	var nilSweeper *Sweeper
	if res := nilSweeper.Sweep(context.Background(), forge, nil, opts); res.Scanned != 0 {
		t.Fatal("nil sweeper must no-op")
	}
	if res := NewSweeper().Sweep(context.Background(), nil, []Candidate{notRFC}, opts); res.Scanned != 0 {
		t.Fatal("nil forge must no-op")
	}
	opts.Now, opts.Logger = time.Time{}, nil
	if res := NewSweeper().Sweep(context.Background(), newFakeForge(), []Candidate{notRFC}, opts); res.Skipped[SkipNotRFC] != 1 {
		t.Fatalf("defaults: %+v", res)
	}
}

func TestFindMatchesRanksAndCaps(t *testing.T) {
	settings := []Setting{
		{Tab: "A", Label: "Alpha widget", Key: "alpha.widget", Text: "widget sprocket gizmo"},
		{Tab: "B", Label: "Alpha widget again", Key: "alpha.widget", Text: "widget gizmo"},
		{Tab: "C", Label: "Beta", Key: "beta.sprocket", Text: "sprocket gizmo"},
		{Tab: "D", Label: "Gamma", Key: "gamma", Text: "gizmo"},
	}
	got := FindMatches("widget sprocket", "gizmo", settings, 0)
	if len(got) != 2 || got[0].Key != "alpha.widget" || got[0].Tab != "A" || got[1].Key != "beta.sprocket" {
		t.Fatalf("matches = %+v", got)
	}
	if got := FindMatches("widget sprocket", "gizmo", settings, 1); len(got) != 1 {
		t.Fatalf("limit not applied: %+v", got)
	}
	if got := FindMatches("", "please wire gamma", settings, 0); len(got) != 1 || got[0].Key != "gamma" || got[0].Score < 5 {
		t.Fatalf("verbatim key must match: %+v", got)
	}
}

func TestRenderCommentWithoutOrigin(t *testing.T) {
	m := Match{Setting: Setting{Tab: "Features", Section: "Features", Key: "x.y"}, Shared: []string{"foo"}}
	body := RenderComment("", []Match{m})
	if strings.Contains(body, "#config/governor") || !strings.Contains(body, "**x.y** — `x.y` (Settings → Features; search Settings for the key)") {
		t.Fatalf("body:\n%s", body)
	}
	if SettingsURL(" ", m.Setting) != "" {
		t.Fatal("no origin, no link")
	}
	if got := SettingsURL("https://h.example", Setting{Tab: "Project Observability", Key: "a[]"}); got != "https://h.example/#config/governor/Project%20Observability/a%5B%5D" {
		t.Fatalf("SettingsURL = %s", got)
	}
}

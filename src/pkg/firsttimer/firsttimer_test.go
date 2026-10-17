package firsttimer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeForge struct {
	comments map[string][]string // repo#n -> bodies
	authored map[string][]int    // repo@author -> PR numbers
	peekErr  error
	listErr  error
	postErr  error
	peeks    int
	lists    int
	posts    int
}

func newFakeForge() *fakeForge {
	return &fakeForge{comments: map[string][]string{}, authored: map[string][]int{}}
}

func (f *fakeForge) UpsertIssueMarkerComment(_ context.Context, repo string, number int, marker, body string, create bool) (MarkerComment, error) {
	key := Candidate{Repo: repo, Number: number}.Key()
	if body == "" && !create {
		f.peeks++
		if f.peekErr != nil {
			return MarkerComment{}, f.peekErr
		}
		for _, b := range f.comments[key] {
			if strings.Contains(b, marker) {
				return MarkerComment{Found: true, Previous: b}, nil
			}
		}
		return MarkerComment{}, nil
	}
	f.posts++
	if f.postErr != nil {
		return MarkerComment{}, f.postErr
	}
	for _, b := range f.comments[key] {
		if strings.Contains(b, marker) {
			return MarkerComment{Found: true, Previous: b}, nil
		}
	}
	f.comments[key] = append(f.comments[key], body)
	return MarkerComment{Created: true}, nil
}

func (f *fakeForge) ListAuthorPullRequests(_ context.Context, repo, author string, limit int) ([]int, error) {
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := f.authored[AuthorKey(repo, author)]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func human(repo string, n int, author string) Candidate {
	return Candidate{
		Repo: repo, Number: n, Author: author, Title: "Fix typo",
		AuthorAssociation: AssociationFirstTimeContributor,
		CreatedAt:         time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Minute),
	}
}

var links = Links{Origin: "https://hive.example.com/", HiveID: "acme-hive"}

func TestDecide(t *testing.T) {
	known := func(login string) bool { return login == "veteran" }
	cases := []struct {
		name string
		c    Candidate
		want string
	}{
		{"first-timer human", human("o/r", 1, "newbie"), ""},
		{"FIRST_TIMER", func() Candidate { c := human("o/r", 1, "newbie"); c.AuthorAssociation = "first_timer"; return c }(), ""},
		{"returning contributor", func() Candidate { c := human("o/r", 1, "old"); c.AuthorAssociation = "CONTRIBUTOR"; return c }(), SkipNotFirstTimer},
		{"member", func() Candidate { c := human("o/r", 1, "m"); c.AuthorAssociation = "MEMBER"; return c }(), SkipNotFirstTimer},
		{"unknown association", func() Candidate { c := human("o/r", 1, "x"); c.AuthorAssociation = ""; return c }(), SkipNotFirstTimer},
		{"NONE association", func() Candidate { c := human("o/r", 1, "x"); c.AuthorAssociation = "NONE"; return c }(), SkipNotFirstTimer},
		{"bot suffix", human("o/r", 1, "dependabot[bot]"), SkipNonHuman},
		{"non-human flag", func() Candidate { c := human("o/r", 1, "hive-app"); c.AuthorNonHuman = true; return c }(), SkipNonHuman},
		{"hive attributed", func() Candidate { c := human("o/r", 1, "p"); c.HiveAttributed = true; return c }(), SkipHiveAttributed},
		{"author/clanker label", func() Candidate { c := human("o/r", 1, "p"); c.Labels = []string{"size/s", "Author/Clanker"}; return c }(), SkipClankerLabel},
		{"author/hive label", func() Candidate { c := human("o/r", 1, "p"); c.Labels = []string{"author/hive"}; return c }(), SkipClankerLabel},
		{"author/human label is fine", func() Candidate { c := human("o/r", 1, "p"); c.Labels = []string{"author/human"}; return c }(), ""},
		{"draft", func() Candidate { c := human("o/r", 1, "p"); c.Draft = true; return c }(), SkipDraft},
		{"registered contributor", human("o/r", 1, "veteran"), SkipRegisteredContributor},
		{"no author", human("o/r", 1, "  "), SkipNoAuthor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := Decide(tc.c, known)
			if d.Welcome != (tc.want == "") || d.Skip != tc.want {
				t.Fatalf("Decide = %+v, want skip %q", d, tc.want)
			}
		})
	}
	if d := Decide(human("o/r", 1, "veteran"), nil); !d.Welcome {
		t.Fatalf("nil known must not skip: %+v", d)
	}
}

func TestSweepWelcomesOncePerAuthorPerRepo(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	ctx := context.Background()
	var events []Event
	opts := SweepOptions{Links: links, Audit: func(e Event) { events = append(events, e) }}

	first := human("o/r", 7, "newbie")
	res := s.Sweep(ctx, forge, []Candidate{first}, opts)
	if len(res.Welcomed) != 1 || res.Welcomed[0] != "o/r#7" || res.Errors != 0 {
		t.Fatalf("first sweep: %+v", res)
	}
	body := forge.comments["o/r#7"][0]
	for _, want := range []string{Marker, "@newbie", "Run your own hive (we call them clankers)",
		"https://hive.example.com/contribute/dossier/newbie", "https://hive.example.com/contribute/leaderboard",
		"https://hive.example.com/contribute", "contributor-relay.md#basic-setup", "newcomer → contributor → trusted", "acme-hive"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment lacks %q:\n%s", want, body)
		}
	}
	if len(events) != 1 || events[0].Author != "newbie" || events[0].Action != "welcome" {
		t.Fatalf("audit events = %+v", events)
	}
	if !s.Welcomed("o/r", "NEWBIE") {
		t.Fatal("memory must record the author (case-insensitive)")
	}

	// Same PR again: memory short-circuits, nothing posted.
	posts := forge.posts
	res = s.Sweep(ctx, forge, []Candidate{first}, opts)
	if len(res.Welcomed) != 0 || res.Skipped[SkipAlreadyWelcomed] != 1 || forge.posts != posts {
		t.Fatalf("second sweep same PR: %+v posts=%d", res, forge.posts)
	}

	// Second PR by the same author (still FIRST_TIME_CONTRIBUTOR): none.
	second := human("o/r", 9, "newbie")
	res = s.Sweep(ctx, forge, []Candidate{second}, opts)
	if len(res.Welcomed) != 0 || res.Skipped[SkipAlreadyWelcomed] != 1 || len(forge.comments["o/r#9"]) != 0 {
		t.Fatalf("second PR: %+v", res)
	}

	// Same author, different repo: welcomed there too.
	res = s.Sweep(ctx, forge, []Candidate{human("o/other", 1, "newbie")}, opts)
	if len(res.Welcomed) != 1 {
		t.Fatalf("other repo: %+v", res)
	}
	if st := s.Stats(); st.Welcomed != 2 || st.Skipped != 2 || st.Errors != 0 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestSweepFindsEarlierWelcomeAcrossRestart(t *testing.T) {
	ctx := context.Background()
	forge := newFakeForge()
	// A previous process welcomed newbie on PR 3; this process has no memory.
	forge.comments["o/r#3"] = []string{Marker + "\nhello"}
	forge.authored[AuthorKey("o/r", "newbie")] = []int{3, 8}

	s := NewSweeper()
	res := s.Sweep(ctx, forge, []Candidate{human("o/r", 8, "newbie")}, SweepOptions{Links: links})
	if len(res.Welcomed) != 0 || res.Skipped[SkipAlreadyWelcomed] != 1 || forge.posts != 0 {
		t.Fatalf("restart dedupe: %+v posts=%d", res, forge.posts)
	}
	if !s.Welcomed("o/r", "newbie") {
		t.Fatal("found-welcomed must be remembered")
	}
	// And the marker on the PR itself is enough on its own.
	forge2 := newFakeForge()
	forge2.comments["o/r#8"] = []string{"x " + Marker}
	res = NewSweeper().Sweep(ctx, forge2, []Candidate{human("o/r", 8, "newbie")}, SweepOptions{Links: links})
	if len(res.Welcomed) != 0 || forge2.lists != 0 || forge2.posts != 0 {
		t.Fatalf("own-marker dedupe: %+v lists=%d posts=%d", res, forge2.lists, forge2.posts)
	}
}

func TestSweepSkipsAndOrders(t *testing.T) {
	ctx := context.Background()
	forge := newFakeForge()
	s := NewSweeper()
	later := human("o/r", 5, "newbie")
	earlier := human("o/r", 4, "newbie")
	clanker := human("o/r", 6, "runs-a-hive")
	clanker.Labels = []string{"author/clanker"}
	bot := human("o/r", 2, "renovate[bot]")
	returning := human("o/r", 1, "oldhand")
	returning.AuthorAssociation = "CONTRIBUTOR"
	other := human("x/y", 1, "someone")
	res := s.Sweep(ctx, forge, []Candidate{later, clanker, bot, returning, earlier, other}, SweepOptions{
		Links:            links,
		KnownContributor: func(l string) bool { return false },
		RepoAllowed:      func(r string) bool { return r == "o/r" },
	})
	if res.Scanned != 6 || len(res.Welcomed) != 1 || res.Welcomed[0] != "o/r#4" {
		t.Fatalf("welcomed = %v (scanned %d)", res.Welcomed, res.Scanned)
	}
	for reason, want := range map[string]int{
		SkipAlreadyWelcomed: 1, SkipClankerLabel: 1, SkipNonHuman: 1, SkipNotFirstTimer: 1, SkipRepoNotAllowed: 1,
	} {
		if res.Skipped[reason] != want {
			t.Errorf("skipped[%s] = %d, want %d (%v)", reason, res.Skipped[reason], want, res.Skipped)
		}
	}
	if len(forge.comments["o/r#5"]) != 0 {
		t.Fatal("the later PR must not be welcomed too")
	}
}

func TestSweepErrorsAndNilInputs(t *testing.T) {
	ctx := context.Background()
	c := human("o/r", 1, "newbie")

	forge := newFakeForge()
	forge.peekErr = errors.New("boom")
	res := NewSweeper().Sweep(ctx, forge, []Candidate{c}, SweepOptions{Links: links})
	if res.Errors != 1 || len(res.Welcomed) != 0 || forge.posts != 0 {
		t.Fatalf("peek error: %+v", res)
	}

	forge = newFakeForge()
	forge.listErr = errors.New("boom")
	s := NewSweeper()
	res = s.Sweep(ctx, forge, []Candidate{c}, SweepOptions{Links: links})
	if res.Errors != 1 || forge.posts != 0 || s.Welcomed("o/r", "newbie") {
		t.Fatalf("list error must not post nor remember: %+v", res)
	}

	forge = newFakeForge()
	forge.postErr = errors.New("boom")
	s = NewSweeper()
	res = s.Sweep(ctx, forge, []Candidate{c}, SweepOptions{Links: links})
	if res.Errors != 1 || s.Welcomed("o/r", "newbie") {
		t.Fatalf("post error must not remember: %+v", res)
	}
	if st := s.Stats(); st.Errors != 1 {
		t.Fatalf("stats = %+v", st)
	}

	// Invalid override template falls back to the default.
	forge = newFakeForge()
	res = NewSweeper().Sweep(ctx, forge, []Candidate{c}, SweepOptions{Links: links, Template: "no marker here"})
	if len(res.Welcomed) != 1 || !strings.Contains(forge.comments["o/r#1"][0], Marker) {
		t.Fatalf("bad template fallback: %+v", res)
	}

	if res := (*Sweeper)(nil).Sweep(ctx, forge, []Candidate{c}, SweepOptions{}); res.Scanned != 0 {
		t.Fatal("nil sweeper must no-op")
	}
	if res := NewSweeper().Sweep(ctx, nil, []Candidate{c}, SweepOptions{}); res.Scanned != 0 {
		t.Fatal("nil forge must no-op")
	}
	if (*Sweeper)(nil).Welcomed("o/r", "x") || (*Sweeper)(nil).Stats() != (Stats{}) {
		t.Fatal("nil sweeper accessors")
	}
	if _, err := (&Sweeper{}).alreadyWelcomed(ctx, nil, c); !errors.Is(err, errNilForge) {
		t.Fatalf("alreadyWelcomed(nil) = %v", err)
	}
}

func TestTemplateParseAndRender(t *testing.T) {
	if _, err := ParseTemplate("hello {{ .Author }}"); err == nil {
		t.Fatal("template without marker must be rejected")
	}
	if unknown, err := ParseTemplate(Marker + " {{ .Nope }}"); err != nil {
		t.Fatal(err)
	} else if _, err := RenderComment(unknown, Vars{}); err == nil {
		t.Fatal("unknown field must fail to execute")
	}
	tmpl, err := ParseTemplate("")
	if err != nil {
		t.Fatal(err)
	}
	c := human("o/r", 12, "newbie")
	v := Vars{Candidate: c, Links: Links{Origin: "https://h.example", DocsBase: "https://docs.example/"}}
	out, err := RenderComment(tmpl, v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, Marker+"\n") || !strings.HasSuffix(out, "\n") {
		t.Fatalf("rendered shape:\n%s", out)
	}
	if strings.Count(out, Marker) != 1 {
		t.Fatalf("marker must appear exactly once:\n%s", out)
	}
	if lines := strings.Count(strings.TrimSpace(out), "\n"); lines > 12 {
		t.Fatalf("comment is %d lines; keep it short", lines)
	}
	if strings.Count(strings.ToLower(out), "clanker") != 1 {
		t.Fatalf("say clanker exactly once:\n%s", out)
	}
	for _, want := range []string{"https://docs.example/README.md", "this hive", "https://h.example/contribute/dossier/newbie",
		"https://h.example/contribute/leaderboard", "https://h.example/contribute ", "auto-merge", "offline", "leaderboard", "profile"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered comment lacks %q:\n%s", want, out)
		}
	}
	if v.Docs() != "https://docs.example" || (Vars{}).Docs() != DefaultDocsBase || (Vars{}).Hive() != "this hive" {
		t.Fatal("Docs/Hive defaults")
	}
	if c.PRURL() != "https://github.com/o/r/pull/12" || (Candidate{URL: "u"}).PRURL() != "u" {
		t.Fatal("PRURL")
	}

	// A template that renders the marker away is rejected at render time.
	hollow, err := ParseTemplate(`{{ if false }}{{ .Marker }}{{ end }}hi`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RenderComment(hollow, v); err == nil {
		t.Fatal("rendered comment without the marker must fail")
	}
	if _, err := RenderComment(nil, v); err == nil {
		t.Fatal("nil template must fail")
	}
}

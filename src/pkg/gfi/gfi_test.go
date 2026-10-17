package gfi

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func good() Candidate {
	return Candidate{
		Repo: "acme/widgets", Number: 42, Title: "Fix off-by-one in pkg/paginate page count",
		Body: strings.Repeat("The last page is dropped when total is a multiple of page size. ", 2) +
			"\n\n## Acceptance criteria\n- `pkg/paginate.Pages(10, 5)` returns 2\n- add a table test\n",
		Labels:    []string{"kind/bug", "size/S"},
		CreatedAt: now.Add(-48 * time.Hour),
	}
}

func TestScoreTable(t *testing.T) {
	policy := Policy{HardSuppressLabels: []string{"needs-human", "needs-direction"}}
	cases := []struct {
		name     string
		mutate   func(*Candidate)
		eligible bool
		skip     string
		reason   string
	}{
		{"baseline eligible", func(*Candidate) {}, true, "", "scoped to one package (pkg/paginate)"},
		{"pull request", func(c *Candidate) { c.IsPR = true }, false, "pull-request", ""},
		{"no identity", func(c *Candidate) { c.Number = 0 }, false, "no-identity", ""},
		{"claimed", func(c *Candidate) { c.ClaimedBy = "someone" }, false, "claimed", ""},
		{"assigned", func(c *Candidate) { c.Assignees = []string{"bob"} }, false, "assigned", ""},
		{"linked pr", func(c *Candidate) { c.HasLinkedPR = true }, false, "linked-pr", ""},
		{"hard suppress", func(c *Candidate) { c.Labels = append(c.Labels, "Needs-Human") }, false, "hard-suppress:needs-human", ""},
		{"excluded label", func(c *Candidate) { c.Labels = append(c.Labels, "kind/epic") }, false, "excluded-label:kind/epic", ""},
		{"security", func(c *Candidate) { c.Labels = append(c.Labels, "security") }, false, "excluded-label:security", ""},
		{"hive claimed-by", func(c *Candidate) { c.Labels = append(c.Labels, "hive/claimed-by-worker") }, false, "excluded-label:hive/claimed-by-worker", ""},
		{"epic title", func(c *Candidate) { c.Title = "Epic: rewrite everything" }, false, "title:epic", ""},
		{"emoji epic title", func(c *Candidate) { c.Title = "📝 v6 epic + readiness bar: dashboard-optional operation" }, false, "title:epic", ""},
		{"bracketed rfc title", func(c *Candidate) { c.Title = "[RFC]: Evidence pipeline — measure effectiveness" }, false, "title:rfc", ""},
		{"meta tag title", func(c *Candidate) { c.Title = "✨ [meta] tidy the docs tree" }, false, "title:meta", ""},
		{"security in title", func(c *Candidate) { c.Title = "Review backlog (security): snapshot_url accepted without validation" }, false, "title:security", ""},
		{"cve in title", func(c *Candidate) { c.Title = "Bump x/net for CVE-2026-1234" }, false, "title:security", ""},
		{"advisory report", func(c *Candidate) {
			c.Title = "🐝 Hive Advisory Report"
			c.Labels = append(c.Labels, "hive/advisory")
		}, false, "excluded-label:hive/advisory", ""},
		{"self report label", func(c *Candidate) { c.Labels = append(c.Labels, "hive/selfreport") }, false, "excluded-label:hive/selfreport", ""},
		{"roadmap label", func(c *Candidate) { c.Labels = append(c.Labels, "roadmap") }, false, "excluded-label:roadmap", ""},
		{"metadata is not meta", func(c *Candidate) { c.Title = "Metadata fields missing from pkg/paginate response" }, true, "", ""},
		{"securely is not security", func(c *Candidate) { c.Title = "Securely-named variable is misspelled in pkg/paginate" }, true, "", ""},
		{"too fresh", func(c *Candidate) { c.CreatedAt = now.Add(-30 * time.Minute) }, false, "too-fresh", ""},
		{"an hour old is old enough", func(c *Candidate) { c.CreatedAt = now.Add(-61 * time.Minute) }, true, "", ""},
		{"zero created", func(c *Candidate) { c.CreatedAt = time.Time{} }, false, "too-fresh", ""},
		{"body too short", func(c *Candidate) { c.Body = "fix it" }, false, "body-too-short", ""},
		{"body too long", func(c *Candidate) { c.Body = strings.Repeat("x ", 3000) }, false, "body-too-long", ""},
		{"too many packages", func(c *Candidate) {
			c.Body += "\nAlso touches pkg/a/x.go, pkg/b/y.go and cmd/hive/main.go"
		}, false, "touches-4-packages", ""},
		{"score below min", func(c *Candidate) {
			c.Labels = nil
			c.Body = strings.Repeat("A long paragraph with no structure at all and nothing scoped here whatsoever. ", 40)
		}, false, "score-0-below-2", ""},
		{"help wanted counts", func(c *Candidate) { c.Labels = []string{"help wanted"} }, true, "", "maintainers asked for help on it"},
		{"already labelled is noted", func(c *Candidate) { c.Labels = append(c.Labels, "Good First Issue") }, true, "", "already sized small by a maintainer"},
		{"two packages", func(c *Candidate) { c.Body += "\nSee also pkg/render/table.go" }, true, "", "scoped to two packages (pkg/paginate, pkg/render)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mutate(&c)
			d := Score(c, policy, now)
			if d.Eligible != tc.eligible {
				t.Fatalf("eligible=%v want %v (skip=%q reasons=%v)", d.Eligible, tc.eligible, d.Skip, d.Reasons)
			}
			if d.Skip != tc.skip {
				t.Fatalf("skip=%q want %q", d.Skip, tc.skip)
			}
			if tc.reason != "" && !contains(d.Reasons, tc.reason) {
				t.Fatalf("reasons %v lack %q", d.Reasons, tc.reason)
			}
			if tc.name == "already labelled is noted" && !d.AlreadyLabelled {
				t.Fatal("AlreadyLabelled should be set")
			}
		})
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestNormalizeTitle(t *testing.T) {
	for in, want := range map[string]string{
		"[RFC]: Evidence pipeline":            "rfc : evidence pipeline",
		"📝 v6 epic + readiness bar":           "v6 epic + readiness bar",
		"✨ [v6] hive-admin-mcp (stdio): warn": "v6 hive-admin-mcp (stdio): warn",
		"  Fix off-by-one  ":                  "fix off-by-one",
		"":                                    "",
	} {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMentionedPackages(t *testing.T) {
	got := MentionedPackages("Edit src/pkg/gfi/gfi.go and pkg/gfi/comment.go; see https://example.com/a/b and README.md and cmd/hive/main.go. Also docs/foo.md")
	want := []string{"cmd/hive", "docs", "pkg/gfi"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
	if n := MentionedPackages(""); len(n) != 0 {
		t.Fatalf("empty body → %v", n)
	}
}

func TestPolicyNormalized(t *testing.T) {
	p := Policy{}.Normalized()
	if p.MaxPerRepo != DefaultMaxPerRepo || p.MaxPerSweep != DefaultMaxPerSweep || p.MinAge != DefaultMinAge ||
		p.MinBodyChars != DefaultMinBodyChars || p.MaxBodyChars != DefaultMaxBodyChars || p.MaxPackages != DefaultMaxPackages ||
		p.MinScore != DefaultMinScore || p.ReselectCooldown != DefaultReselectCooldown || len(p.ExcludeLabels) != len(DefaultExcludeLabels) {
		t.Fatalf("defaults not applied: %+v", p)
	}
	q := Policy{MaxPerRepo: 1, ExcludeLabels: []string{"x"}}.Normalized()
	if q.MaxPerRepo != 1 || len(q.ExcludeLabels) != 1 {
		t.Fatalf("explicit values overridden: %+v", q)
	}
}

func TestClaimURL(t *testing.T) {
	if got := ClaimURL("https://hive.example/", "acme/widgets", 42); got != "https://hive.example/contribute?issue=acme%2Fwidgets%2342" {
		t.Fatalf("got %q", got)
	}
	if ClaimURL("", "acme/widgets", 42) != "" {
		t.Fatal("empty origin should yield empty URL")
	}
	c := Candidate{Repo: "acme/widgets", Number: 42}
	if c.IssueURL() != "https://github.com/acme/widgets/issues/42" || c.Key() != "acme/widgets#42" {
		t.Fatalf("derived url/key wrong: %q %q", c.IssueURL(), c.Key())
	}
	c.URL = "https://ghe.local/x"
	if c.IssueURL() != "https://ghe.local/x" {
		t.Fatal("explicit URL not preferred")
	}
}

func TestRenderCommentDefault(t *testing.T) {
	tmpl, err := ParseTemplate("")
	if err != nil {
		t.Fatal(err)
	}
	v := Vars{Candidate: good(), Links: Links{Origin: "https://hive.example", HiveID: "acme-hive"},
		Reasons:     []string{"scoped to one package (pkg/paginate)", "has clear steps or acceptance criteria", "third"},
		AgentsMDURL: "https://github.com/acme/widgets/blob/HEAD/AGENTS.md"}
	out, err := RenderComment(tmpl, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		Marker, "Claude Code, Codex, Copilot CLI, Bob",
		"https://hive.example/contribute?issue=acme%2Fwidgets%2342",
		"Fixes #42", "git commit -s", "- scoped to one package (pkg/paginate)", "- has clear steps or acceptance criteria",
		"Paste this to your agent", "AGENTS.md", "acme-hive picked this issue",
		DefaultDocsBase + "/contributor-relay.md", "hive-merge.md#contributor-lane-auto-merge",
		"Run your own hive (we call them clankers)", "leaderboard place: see https://hive.example/contribute.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("comment lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "- third") {
		t.Error("more than two reasons rendered")
	}
	if !strings.HasPrefix(out, Marker) {
		t.Error("marker must be the first line")
	}
	prompt := v.AgentPrompt()
	for _, want := range []string{"#42", "github.com/acme/widgets", "Fixes #42", "git commit -s", "AGENTS.md", v.ClaimURL()} {
		if !strings.Contains(prompt, want) {
			t.Errorf("agent prompt lacks %q: %s", want, prompt)
		}
	}
	v.AgentsMDURL = ""
	v.HiveID = ""
	v.DocsBase = "https://docs.example/"
	if strings.Contains(v.AgentPrompt(), "AGENTS") || v.Hive() != "this hive" || v.Docs() != "https://docs.example" || v.RelayURL() != "https://hive.example/contribute" {
		t.Error("fallbacks wrong")
	}
	out2, _ := RenderComment(tmpl, v)
	if strings.Contains(out2, "AGENTS.md") {
		t.Error("AGENTS.md link rendered without a URL")
	}
}

func TestParseTemplateRejectsMissingMarker(t *testing.T) {
	if _, err := ParseTemplate("hello {{ .Number }}"); err == nil {
		t.Fatal("template without marker accepted")
	}
	if _, err := ParseTemplate(Marker + " {{ .Nope }}"); err == nil {
		// Parsing succeeds; execution fails on the unknown field.
		tmpl, _ := ParseTemplate(Marker + " {{ .Nope }}")
		if _, err := RenderComment(tmpl, Vars{}); err == nil {
			t.Fatal("unknown field rendered")
		}
	}
	tmpl, err := ParseTemplate(Marker + "\nclaim at {{ .ClaimURL }}")
	if err != nil {
		t.Fatal(err)
	}
	out, err := RenderComment(tmpl, Vars{Candidate: good(), Links: Links{Origin: "https://h"}})
	if err != nil || !strings.Contains(out, "https://h/contribute?issue=") {
		t.Fatalf("override failed: %v %q", err, out)
	}
	if _, err := ParseTemplate("{{ .Marker }} {{ if }}"); err == nil {
		t.Fatal("syntax error accepted")
	}
}

func TestSupersededComment(t *testing.T) {
	out := SupersededComment(good(), ReasonClaimedByHive, Links{Origin: "https://h/", HiveID: "acme-hive"}, now)
	for _, want := range []string{SupersededMarker, "acme-hive", "claimed-by-hive", "2026-09-25T12:00:00Z", "https://h/contribute"} {
		if !strings.Contains(out, want) {
			t.Errorf("lacks %q: %s", want, out)
		}
	}
	if strings.Contains(out, Marker) {
		t.Error("superseded body must not contain the live marker")
	}
	if !strings.Contains(SupersededComment(good(), "x", Links{}, now), "this hive") {
		t.Error("hive fallback")
	}
}

// fakeForge records calls and simulates marker comments.
type fakeForge struct {
	mu       sync.Mutex
	labels   map[string][]string // key → ensured label names
	added    map[string][]string
	removed  map[string][]string
	comments map[string][]string // key → comment bodies
	failOn   string
}

func newFakeForge() *fakeForge {
	return &fakeForge{labels: map[string][]string{}, added: map[string][]string{}, removed: map[string][]string{}, comments: map[string][]string{}}
}

func key(repo string, n int) string { return Candidate{Repo: repo, Number: n}.Key() }

func (f *fakeForge) EnsureIssueLabel(_ context.Context, repo, name, _, _ string) error {
	if f.failOn == "ensure" {
		return errors.New("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labels[repo] = append(f.labels[repo], name)
	return nil
}

func (f *fakeForge) AddLabels(_ context.Context, repo string, n int, labels []string) error {
	if f.failOn == "add" {
		return errors.New("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added[key(repo, n)] = append(f.added[key(repo, n)], labels...)
	return nil
}

func (f *fakeForge) RemoveLabel(_ context.Context, repo string, n int, label string) error {
	if f.failOn == "remove" {
		return errors.New("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[key(repo, n)] = append(f.removed[key(repo, n)], label)
	return nil
}

func (f *fakeForge) UpsertIssueMarkerComment(_ context.Context, repo string, n int, marker, body string, create bool) (MarkerComment, error) {
	if f.failOn == "comment" {
		return MarkerComment{}, errors.New("boom")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(repo, n)
	for i, existing := range f.comments[k] {
		if strings.Contains(existing, marker) {
			res := MarkerComment{Found: true, Previous: existing}
			if body != "" && existing != body {
				f.comments[k][i] = body
			}
			return res, nil
		}
	}
	if !create {
		return MarkerComment{}, nil
	}
	f.comments[k] = append(f.comments[k], body)
	return MarkerComment{Created: true}, nil
}

func (f *fakeForge) commentCount(repo string, n int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments[key(repo, n)])
}

func quietOpts() SweepOptions {
	return SweepOptions{Links: Links{Origin: "https://hive.example", HiveID: "acme-hive"}, Now: now, Logger: slog.New(slog.DiscardHandler)}
}

func TestSweepSelectsLabelsAndCommentsIdempotently(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	var events []Event
	opts := quietOpts()
	opts.Audit = func(e Event) { events = append(events, e) }
	opts.AgentsMD = func(context.Context, string) string { return "https://github.com/acme/widgets/blob/HEAD/AGENTS.md" }

	c := good()
	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Selected) != 1 || res.Scanned != 1 {
		t.Fatalf("result %+v", res)
	}
	if got := forge.added[c.Key()]; strings.Join(got, ",") != Label+","+HelpWantedLabel {
		t.Fatalf("labels added %v", got)
	}
	if got := forge.labels[c.Repo]; len(got) != 2 {
		t.Fatalf("labels ensured %v", got)
	}
	if forge.commentCount(c.Repo, c.Number) != 1 || !strings.Contains(forge.comments[c.Key()][0], "AGENTS.md") {
		t.Fatalf("comment %v", forge.comments[c.Key()])
	}
	if len(events) != 1 || events[0].Action != "select" || len(events[0].Reasons) == 0 {
		t.Fatalf("events %+v", events)
	}
	if st := s.Stats(); st.Selected != 1 || st.Labelled != 2 {
		t.Fatalf("stats %+v", st)
	}

	// Second pass in the same process: remembered, no second comment/labels.
	res = s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Selected) != 0 || res.Skipped["already-selected"] != 1 {
		t.Fatalf("second pass %+v", res)
	}
	if forge.commentCount(c.Repo, c.Number) != 1 {
		t.Fatal("second comment posted")
	}

	// A fresh process (new Sweeper) sees the labelled issue: it is counted
	// against the repo cap, comment is unchanged, and no labels re-added.
	s2 := NewSweeper()
	labelled := c
	labelled.Labels = append(labelled.Labels, Label, HelpWantedLabel)
	res = s2.Sweep(context.Background(), forge, []Candidate{labelled}, opts)
	if len(res.Selected) != 1 || forge.commentCount(c.Repo, c.Number) != 1 || len(forge.added[c.Key()]) != 2 {
		t.Fatalf("restart pass %+v added=%v comments=%d", res, forge.added[c.Key()], forge.commentCount(c.Repo, c.Number))
	}
	if !s2.Selected(c.Key()) || s2.Stats().Labelled != 0 {
		t.Fatalf("restart pass should remember without counting new labels: %+v", s2.Stats())
	}
}

func TestSweepCaps(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	opts := quietOpts()
	opts.Policy = Policy{MaxPerSweep: 1, MaxPerRepo: 2}
	var cands []Candidate
	for i := 1; i <= 3; i++ {
		c := good()
		c.Number = i
		cands = append(cands, c)
	}
	// Another repo already at its cap.
	full := good()
	full.Repo = "acme/full"
	full.Number = 1
	full.Labels = append(full.Labels, Label)
	full2 := good()
	full2.Repo = "acme/full"
	full2.Number = 2
	full2.Labels = append(full2.Labels, Label)
	fresh := good()
	fresh.Repo = "acme/full"
	fresh.Number = 3
	cands = append(cands, full, full2, fresh)

	res := s.Sweep(context.Background(), forge, cands, opts)
	if len(res.Selected) != 2 || res.Skipped["sweep-cap"] != 4 {
		t.Fatalf("result %+v", res)
	}
	if contains(res.Selected, "acme/full#3") {
		t.Fatalf("unlabelled issue selected over cap: %v", res.Selected)
	}
	// A fresh process with the sweep cap lifted: the repo cap counts the
	// issues already wearing the label, so the unlabelled one is skipped.
	res = NewSweeper().Sweep(context.Background(), newFakeForge(), []Candidate{fresh, full, full2}, SweepOptions{Policy: Policy{MaxPerSweep: 5, MaxPerRepo: 2}, Now: now, Logger: opts.Logger, Links: opts.Links})
	if res.Skipped["repo-cap"] != 1 || len(res.Selected) != 2 {
		t.Fatalf("repo-cap not enforced: %+v", res)
	}
}

func TestSweepErrorsAndSuperseded(t *testing.T) {
	c := good()
	for _, mode := range []string{"ensure", "add", "comment"} {
		forge := newFakeForge()
		forge.failOn = mode
		res := NewSweeper().Sweep(context.Background(), forge, []Candidate{c}, quietOpts())
		if res.Errors != 1 || len(res.Selected) != 0 {
			t.Fatalf("%s: %+v", mode, res)
		}
	}
	// A superseded comment from an earlier process blocks re-selection and
	// starts the cooldown.
	forge := newFakeForge()
	forge.comments[c.Key()] = []string{SupersededComment(c, ReasonClaimedByHive, Links{}, now.Add(-time.Hour))}
	s := NewSweeper()
	res := s.Sweep(context.Background(), forge, []Candidate{c}, quietOpts())
	if res.Skipped["previously-released"] != 1 || len(forge.added) != 0 {
		t.Fatalf("superseded not honoured: %+v added=%v", res, forge.added)
	}
	res = s.Sweep(context.Background(), forge, []Candidate{c}, quietOpts())
	if res.Skipped["reselect-cooldown"] != 1 {
		t.Fatalf("cooldown not applied: %+v", res)
	}
	// Bad template falls back to the default rather than failing.
	forge = newFakeForge()
	opts := quietOpts()
	opts.Template = "no marker here"
	res = NewSweeper().Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Selected) != 1 || !strings.Contains(forge.comments[c.Key()][0], Marker) {
		t.Fatalf("fallback template: %+v", res)
	}
	// nil forge and zero Now are tolerated.
	if r := NewSweeper().Sweep(context.Background(), nil, []Candidate{c}, SweepOptions{}); r.Scanned != 0 {
		t.Fatal("nil forge should no-op")
	}
	if r := NewSweeper().Sweep(context.Background(), newFakeForge(), []Candidate{c}, SweepOptions{Links: Links{Origin: "https://h"}}); len(r.Selected) != 1 {
		t.Fatalf("default now/logger: %+v", r)
	}
}

func TestReleaseOnClaim(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	opts := quietOpts()
	var events []Event
	opts.Audit = func(e Event) { events = append(events, e) }
	c := good()
	s.Sweep(context.Background(), forge, []Candidate{c}, opts)

	if err := s.Release(context.Background(), forge, c, ReasonClaimedByHive, opts); err != nil {
		t.Fatal(err)
	}
	if got := forge.removed[c.Key()]; strings.Join(got, ",") != Label+","+HelpWantedLabel {
		t.Fatalf("removed %v", got)
	}
	if n := forge.commentCount(c.Repo, c.Number); n != 1 {
		t.Fatalf("comments=%d", n)
	}
	body := forge.comments[c.Key()][0]
	if !strings.Contains(body, SupersededMarker) || strings.Contains(body, Marker) || !strings.Contains(body, "superseded") {
		t.Fatalf("comment not superseded: %s", body)
	}
	if s.Selected(c.Key()) || s.Stats().Released != 1 {
		t.Fatalf("state after release: selected=%v stats=%+v", s.Selected(c.Key()), s.Stats())
	}
	if len(events) != 2 || events[1].Action != "release" || events[1].Reason != ReasonClaimedByHive {
		t.Fatalf("events %+v", events)
	}
	// Re-selection is blocked by the cooldown.
	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if res.Skipped["reselect-cooldown"] != 1 {
		t.Fatalf("cooldown: %+v", res)
	}
	// Release on an issue this process never labelled (operator added the
	// label): only the GFI label comes off and no comment is created.
	other := good()
	other.Number = 7
	if err := s.Release(context.Background(), forge, other, ReasonClaimedByHive, opts); err != nil {
		t.Fatal(err)
	}
	if got := forge.removed[other.Key()]; strings.Join(got, ",") != Label || forge.commentCount(other.Repo, 7) != 0 {
		t.Fatalf("unknown release: removed=%v comments=%d", got, forge.commentCount(other.Repo, 7))
	}
	// Errors surface but state still moves.
	forge.failOn = "remove"
	if err := s.Release(context.Background(), forge, good(), ReasonClaimedByHive, SweepOptions{}); err == nil {
		t.Fatal("remove error swallowed")
	}
	forge.failOn = "comment"
	if err := s.Release(context.Background(), forge, good(), ReasonClaimedByHive, SweepOptions{}); err == nil {
		t.Fatal("comment error swallowed")
	}
	if err := s.Release(context.Background(), nil, good(), ReasonClaimedByHive, SweepOptions{}); err != nil {
		t.Fatal("nil forge should no-op")
	}
}

func TestConvertAndStats(t *testing.T) {
	s := NewSweeper()
	var events []Event
	opts := quietOpts()
	opts.Audit = func(e Event) { events = append(events, e) }
	s.Convert(good(), "octocat", "contributor", opts)
	s.Convert(good(), "octocat", "contributor", SweepOptions{})
	if s.Stats().Converted != 2 || len(events) != 1 || events[0].Action != "convert" || events[0].Reason != "contributor:octocat" {
		t.Fatalf("stats=%+v events=%+v", s.Stats(), events)
	}
	var nilS *Sweeper
	if nilS.Stats() != (Stats{}) || nilS.Selected("x") {
		t.Fatal("nil sweeper accessors")
	}
	if !HasLabel([]string{"Good First Issue"}) || HasLabel([]string{"bug"}) {
		t.Fatal("HasLabel")
	}
}

// A parked, human-held or agent-claimed issue is never advertised, whatever
// the caller's policy says (hivecommons/hive#12680 was stamped minutes after
// its owner wrote "Agents stand down" and the un-park sweep had cleared
// `needs-human`).
func TestScoreAlwaysExcludesParkedAndClaimed(t *testing.T) {
	// A policy that supplies NO hard-suppress set and REPLACES the default
	// exclusions with something unrelated: the built-in set must still win.
	policy := Policy{ExcludeLabels: []string{"wontfix"}}
	cases := []struct {
		name   string
		labels []string
		skip   string
	}{
		{"needs-human", []string{"kind/bug", "Needs-Human"}, "hard-suppress:needs-human"},
		{"approved-direction with needs-human", []string{"approved-direction", "needs-human"}, "hard-suppress:needs-human"},
		{"stage 085", []string{"kind/bug", "stage/085-needs-decision"}, "hard-suppress:stage/085-needs-decision"},
		{"retired needs-decision", []string{"needs-decision"}, "hard-suppress:needs-decision"},
		{"claimed-by", []string{"kind/bug", "hive/claimed-by-karibhou"}, "excluded-label:hive/claimed-by-karibhou"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			c.Labels = tc.labels
			d := Score(c, policy, now)
			if d.Eligible || d.Skip != tc.skip {
				t.Fatalf("eligible=%v skip=%q, want ineligible %q", d.Eligible, d.Skip, tc.skip)
			}
		})
	}
	c := good()
	c.Labels = []string{"kind/bug", "approved-direction"}
	if d := Score(c, policy, now); !d.Eligible {
		t.Fatalf("approved-direction alone must not disqualify, skip=%q", d.Skip)
	}
}

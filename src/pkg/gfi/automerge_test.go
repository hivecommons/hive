package gfi

import (
	"context"
	"strings"
	"testing"
)

// The welcome comment promises an automatic merge only where the hive's
// contributor lane merges: in a repository whose auto-merge is off a
// maintainer merges, so the invite says so (projectbluefin/common#1688).
func TestRenderCommentAutoMerge(t *testing.T) {
	tmpl, err := ParseTemplate("")
	if err != nil {
		t.Fatal(err)
	}
	v := Vars{Candidate: good(), Links: Links{Origin: "https://hive.example", HiveID: "acme-hive"},
		Reasons: []string{"scoped to one package (pkg/paginate)"}, AutoMerge: true}
	on, err := RenderComment(tmpl, v)
	if err != nil {
		t.Fatal(err)
	}
	// On: byte-identical to the template before the switch existed, i.e.
	// the same text with each conditional spelled out as its true branch.
	legacy := strings.NewReplacer(
		"{{ if .AutoMerge }} (no waiting on a human){{ end }}", " (no waiting on a human)",
		"{{ if .AutoMerge }}Hive runs CI and **merges green PRs from claimed contributors automatically**.{{ else }}Hive runs CI and reviews the PR; **a maintainer of this repository merges it**.{{ end }}",
		"Hive runs CI and **merges green PRs from claimed contributors automatically**.",
		"[contributor lane{{ if .AutoMerge }} auto-merge{{ end }}]", "[contributor lane auto-merge]",
	).Replace(DefaultTemplate)
	if strings.Contains(legacy, "AutoMerge") {
		t.Fatalf("legacy template still branches:\n%s", legacy)
	}
	legacyTmpl, err := ParseTemplate(legacy)
	if err != nil {
		t.Fatal(err)
	}
	want, err := RenderComment(legacyTmpl, v)
	if err != nil {
		t.Fatal(err)
	}
	if on != want {
		t.Fatalf("auto-merge on changed the comment:\n got %q\nwant %q", on, want)
	}
	for _, s := range []string{"**How to land it (no waiting on a human):**", "4. Hive runs CI and **merges green PRs from claimed contributors automatically**."} {
		if !strings.Contains(on, s) {
			t.Errorf("auto-merge on: comment lacks %q", s)
		}
	}
	if p := v.AgentPrompt(); !strings.HasSuffix(p, "so the hive's contributor lane can auto-merge it once CI is green.") {
		t.Errorf("auto-merge on: agent prompt changed: %s", p)
	}

	v.AutoMerge = false
	off, err := RenderComment(tmpl, v)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"**How to land it:**", "4. Hive runs CI and reviews the PR; **a maintainer of this repository merges it**.", "Fixes #42", "[contributor lane](" + DefaultDocsBase + "/hive-merge.md#contributor-lane-auto-merge)"} {
		if !strings.Contains(off, s) {
			t.Errorf("auto-merge off: comment lacks %q:\n%s", s, off)
		}
	}
	for _, s := range []string{"automatically", "no waiting on a human", "[contributor lane auto-merge]", "can auto-merge"} {
		if strings.Contains(off, s) {
			t.Errorf("auto-merge off: comment still promises %q:\n%s", s, off)
		}
	}
	p := v.AgentPrompt()
	if strings.Contains(p, "auto-merge") || !strings.Contains(p, "links it to your claim; the hive reviews it and a maintainer merges it.") || !strings.Contains(p, "'Fixes #42'") {
		t.Errorf("auto-merge off: agent prompt = %s", p)
	}
}

func TestCustomTemplateCanBranchOnAutoMerge(t *testing.T) {
	tmpl, err := ParseTemplate(Marker + "\n{{ if .AutoMerge }}merged for you{{ else }}a maintainer merges{{ end }}")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		on   bool
		want string
	}{{true, "merged for you"}, {false, "a maintainer merges"}} {
		out, err := RenderComment(tmpl, Vars{Candidate: good(), AutoMerge: tc.on})
		if err != nil || !strings.Contains(out, tc.want) {
			t.Fatalf("AutoMerge=%v: %q, %v", tc.on, out, err)
		}
	}
}

// The sweep asks per repository, keeps the promise when nothing is wired,
// and corrects an invite it already posted on the next selecting pass.
func TestSweepRendersPerRepoAutoMerge(t *testing.T) {
	forge := newFakeForge()
	off := good()
	on := good()
	on.Repo = "acme/gadgets"
	opts := quietOpts()
	opts.RepoAutoMerge = func(repo string) bool { return repo != off.Repo }
	if res := NewSweeper().Sweep(context.Background(), forge, []Candidate{off, on}, opts); len(res.Selected) != 2 {
		t.Fatalf("result %+v", res)
	}
	if body := forge.comments[off.Key()][0]; strings.Contains(body, "automatically") || !strings.Contains(body, "a maintainer of this repository merges it") {
		t.Fatalf("auto-merge-off repo: %s", body)
	}
	if body := forge.comments[on.Key()][0]; !strings.Contains(body, "merges green PRs from claimed contributors automatically") {
		t.Fatalf("auto-merge-on repo: %s", body)
	}

	// Unwired (nil) keeps today's promise.
	plain := newFakeForge()
	NewSweeper().Sweep(context.Background(), plain, []Candidate{off}, quietOpts())
	if body := plain.comments[off.Key()][0]; !strings.Contains(body, "automatically") {
		t.Fatalf("nil RepoAutoMerge: %s", body)
	}

	// An invite posted with the old promise is edited in place by the next
	// process's pass once the repo's auto-merge is off.
	labelled := off
	labelled.Labels = append(labelled.Labels, Label, HelpWantedLabel)
	opts.RepoAutoMerge = func(string) bool { return false }
	NewSweeper().Sweep(context.Background(), plain, []Candidate{labelled}, opts)
	if n := plain.commentCount(off.Repo, off.Number); n != 1 {
		t.Fatalf("comments = %d, want the one edited in place", n)
	}
	if body := plain.comments[off.Key()][0]; strings.Contains(body, "automatically") {
		t.Fatalf("invite not corrected: %s", body)
	}
}

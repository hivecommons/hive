package gfi

import (
	"context"
	"strings"
	"testing"
)

// Each recognition rule, table-driven (hivecommons/hive#19078).
func TestUmbrellaReason(t *testing.T) {
	cases := []struct {
		name     string
		policy   UmbrellaPolicy
		title    string
		labels   []string
		body     string
		children int
		want     string
	}{
		{name: "plain issue", title: "Fix the paginator", body: "Some body."},
		{name: "umbrella label", title: "x", labels: []string{"Umbrella"}, want: "label:umbrella"},
		{name: "tracker label", title: "x", labels: []string{"tracker"}, want: "label:tracker"},
		{name: "epic label", title: "x", labels: []string{"epic"}, want: "label:epic"},
		{name: "prefixed epic label", title: "x", labels: []string{"kind/epic"}, want: "label:kind/epic"},
		{name: "body marker", title: "x", body: "intro\n<!-- hive:umbrella -->\n", want: "marker"},
		{name: "marker documented in a code span is not a marker", title: "x", body: "Use `<!-- hive:umbrella -->` to mark one."},
		{name: "marker documented in a fence is not a marker", title: "x", body: "```\n<!-- hive:umbrella -->\n```\n"},
		{name: "body marker whitespace", title: "x", body: "<!--hive:umbrella-->", want: "marker"},
		{name: "tracking prefix", title: "[Tracking] fail-closed defaults", want: "title-prefix:[tracking]"},
		{name: "emoji then epic prefix", title: "📝 [Epic] v7 backends", want: "title-prefix:[epic]"},
		{name: "umbrella prefix", title: "[umbrella] audit", want: "title-prefix:[umbrella]"},
		{name: "task list of two", title: "x", body: "- [ ] #12\n- [x] owner/repo#13\n", want: "task-list"},
		{name: "task list of one is not enough", title: "x", body: "blocked on\n- [ ] #12\n"},
		{name: "plain checklist", title: "x", body: "- [ ] write tests\n- [ ] docs\n"},
		{name: "sub-issues", title: "x", children: 3, want: "sub-issues"},
		{name: "custom labels replace defaults", policy: UmbrellaPolicy{Labels: []string{"program"}}, title: "x", labels: []string{"epic"}},
		{name: "custom label matches", policy: UmbrellaPolicy{Labels: []string{"program"}}, title: "x", labels: []string{"program"}, want: "label:program"},
		{name: "custom prefix", policy: UmbrellaPolicy{TitlePrefixes: []string{"program:"}}, title: "Program: q4", want: "title-prefix:program:"},
		{name: "custom task list min", policy: UmbrellaPolicy{TaskListMin: 3}, title: "x", body: "- [ ] #1\n- [ ] #2\n"},
		{name: "disabled", policy: UmbrellaPolicy{Disabled: true}, title: "[Epic] x", labels: []string{"epic"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.policy.UmbrellaReason(tc.title, tc.labels, tc.body, tc.children)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("UmbrellaReason = %q,%v want %q", got, ok, tc.want)
			}
		})
	}
}

func TestHumanOnlyReason(t *testing.T) {
	cases := []struct {
		name   string
		policy UmbrellaPolicy
		labels []string
		body   string
		want   string
	}{
		{name: "none", labels: []string{"bug"}},
		{name: "needs-human", labels: []string{"Needs-Human"}, want: "label:needs-human"},
		{name: "human-only", labels: []string{"human-only"}, want: "label:human-only"},
		{name: "human-only reason family", labels: []string{"human-only:cla"}, want: "label:human-only:cla"},
		{name: "marker", body: "<!-- hive:human-only -->", want: "marker"},
		{name: "custom replaces", policy: UmbrellaPolicy{HumanOnlyLabels: []string{"owner-task"}}, labels: []string{"needs-human"}},
		{name: "custom matches", policy: UmbrellaPolicy{HumanOnlyLabels: []string{"owner-task"}}, labels: []string{"owner-task"}, want: "label:owner-task"},
		{name: "disabled", policy: UmbrellaPolicy{Disabled: true}, labels: []string{"human-only"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := tc.policy.HumanOnlyReason(tc.labels, tc.body)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("HumanOnlyReason = %q,%v want %q", got, ok, tc.want)
			}
		})
	}
}

// The umbrella vocabulary must never collide with the tracking vocabulary:
// a tracking issue is never auto-closed, an umbrella is.
func TestUmbrellaIsNotTracking(t *testing.T) {
	for _, l := range DefaultUmbrellaLabels {
		if strings.EqualFold(l, TrackingLabel) {
			t.Fatalf("default umbrella label %q reuses the tracking label", l)
		}
	}
	if UmbrellaMarker == TrackingMarker || HumanOnlyMarker == TrackingMarker {
		t.Fatal("umbrella markers must differ from the tracking marker")
	}
	if IsTracking("[Epic] x", []string{"umbrella", "epic"}, UmbrellaMarker) {
		t.Fatal("an umbrella must not read as a tracking issue")
	}
	if !IsTracking("x", []string{TrackingLabel}, "") {
		t.Fatal("control: hive/tracking is tracking")
	}
}

func TestUmbrellaPolicyMerge(t *testing.T) {
	base := UmbrellaPolicy{Labels: []string{"umbrella"}, TaskListMin: 4}
	got := base.Merge(UmbrellaPolicy{TitlePrefixes: []string{"[x]"}, TaskListMin: 2})
	if len(got.Labels) != 1 || got.Labels[0] != "umbrella" || got.TaskListMin != 2 || got.TitlePrefixes[0] != "[x]" {
		t.Fatalf("merge = %+v", got)
	}
	if inherit := base.Merge(UmbrellaPolicy{}); inherit.TaskListMin != 4 {
		t.Fatalf("empty override must inherit, got %+v", inherit)
	}
}

func TestScoreExcludesUmbrellaAndHumanOnly(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Candidate)
		skip   string
		wd     bool
	}{
		{"epic label with invitation", func(c *Candidate) { c.Labels = []string{"epic", HelpWantedLabel} }, "umbrella:label:epic", true},
		{"epic label without invitation", func(c *Candidate) { c.Labels = []string{"epic"} }, "umbrella:label:epic", false},
		{"marker", func(c *Candidate) { c.Body += "\n" + UmbrellaMarker }, "umbrella:marker", false},
		{"claimed umbrella still excluded", func(c *Candidate) { c.ClaimedBy = "agent"; c.Labels = []string{"umbrella", Label} }, "umbrella:label:umbrella", true},
		{"children", func(c *Candidate) { c.Children = 2 }, "umbrella:sub-issues", false},
		{"human-only label", func(c *Candidate) { c.Labels = []string{"human-only", HelpWantedLabel} }, "human-only:label:human-only", true},
		{"human-only marker", func(c *Candidate) { c.Body += "\n" + HumanOnlyMarker }, "human-only:marker", false},
		{"needs-human keeps its legacy name", func(c *Candidate) { c.Labels = []string{"needs-human", HelpWantedLabel} }, "hard-suppress:needs-human", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good()
			tc.mutate(&c)
			d := Score(c, Policy{}, now)
			if d.Eligible || d.Skip != tc.skip || d.WithdrawAd != tc.wd {
				t.Fatalf("eligible=%v skip=%q withdraw=%v, want %q withdraw=%v", d.Eligible, d.Skip, d.WithdrawAd, tc.skip, tc.wd)
			}
		})
	}
	// Per-repo override: the repo that turns the rule off gets a normal score,
	// another repo does not.
	p := Policy{UmbrellaFor: func(repo string) UmbrellaPolicy {
		if repo == good().Repo {
			return UmbrellaPolicy{Disabled: true}
		}
		return UmbrellaPolicy{}
	}}
	c := good()
	c.Body += "\n" + UmbrellaMarker
	if d := Score(c, p, now); !d.Eligible {
		t.Fatalf("disabled repo must score normally: %+v", d)
	}
	c.Repo = "other/repo"
	if d := Score(c, p, now); d.Skip != "umbrella:marker" {
		t.Fatalf("other repo keeps the default: %+v", d)
	}
}

func TestSweepWithdrawsHelpWantedAloneFromUmbrella(t *testing.T) {
	forge := newFakeForge()
	c := good()
	c.Labels = []string{HelpWantedLabel}
	c.Body += "\n" + UmbrellaMarker
	res := NewSweeper().Sweep(context.Background(), forge, []Candidate{c}, quietOpts())
	if len(res.Withdrawn) != 1 || strings.Join(forge.removed[c.Key()], ",") != HelpWantedLabel {
		t.Fatalf("result %+v removed %v", res, forge.removed[c.Key()])
	}
	if !strings.Contains(WithdrawnComment(c, "umbrella:marker", Links{}, now), "umbrella") {
		t.Fatal("withdrawn comment must name the umbrella reason")
	}
}

func TestHasAdvertisement(t *testing.T) {
	if !HasAdvertisement([]string{"Help Wanted"}) || !HasAdvertisement([]string{"good first issue"}) || HasAdvertisement([]string{"bug"}) {
		t.Fatal("HasAdvertisement")
	}
}

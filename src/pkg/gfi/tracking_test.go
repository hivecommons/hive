package gfi

import (
	"context"
	"strings"
	"testing"
)

// ghawDetectionRunsBody is the tail of hivecommons/hive#14646 "[aw] Detection
// Runs" exactly as GitHub Agentic Workflows wrote it: the issue the hive
// stamped `good first issue` + `help wanted` + `stage/010-actionable`.
const ghawDetectionRunsBody = "This issue tracks all runs where threat detection flagged problems in agentic workflows in this repository. Each workflow run that completes with a detection warning or failure posts a comment here.\n\n" +
	"<details>\n<summary>Resources</summary>\n\n- [GitHub Agentic Workflows Documentation](https://github.com/github/gh-aw)\n\n</details>\n\n" +
	"---\n\n" +
	"> This issue is automatically managed by GitHub Agentic Workflows. Do not close this issue manually.\n>\n> **No action to take** - Do not assign to an agent.\n\n" +
	"<!-- gh-aw-detection-runs -->\n\n" +
	"> - [x] expires <!-- gh-aw-expires: 2026-11-26T17:57:22.389Z --> on Nov 26, 2026, 5:57 PM UTC\n"

func TestTrackingReason(t *testing.T) {
	cases := []struct {
		name   string
		title  string
		labels []string
		body   string
		want   string
	}{
		{"gh-aw #14646 verbatim", "[aw] Detection Runs", []string{"good first issue", "help wanted", "stage/010-actionable", GHAWLabel}, ghawDetectionRunsBody, "gh-aw-marker"},
		{"gh-aw detection-runs marker", "", nil, "log\n<!-- gh-aw-detection-runs -->\n", "gh-aw-marker"},
		{"gh-aw expires marker with payload", "", nil, "> - [x] expires <!-- gh-aw-expires: 2026-11-26T17:57:22.389Z --> on Nov 26", "gh-aw-marker"},
		{"gh-aw managed sentence", "", nil, "> This issue is automatically managed by GitHub Agentic Workflows.", "gh-aw-managed"},
		{"gh-aw do-not-close sentence", "", nil, "do not CLOSE this issue manually", "gh-aw-managed"},
		{"gh-aw do-not-assign sentence", "", nil, "**No action to take** - Do not assign to an agent.", "gh-aw-managed"},
		{"gh-aw label with [aw] title", "[aw] Weekly Research", []string{GHAWLabel}, "Research report archive.", "gh-aw-label"},
		{"gh-aw label without [aw] title", "Flaky detection workflow", []string{GHAWLabel}, "The detection job fails on arm64.", ""},
		{"[aw] title without gh-aw label", "[aw] something", []string{"bug"}, "plain", ""},
		{"hive label wins over gh-aw", "[aw] x", []string{GHAWLabel, TrackingLabel}, ghawDetectionRunsBody, "label"},
		{"gh-aw lookalike marker", "", nil, "<!-- gh-awesome -->", ""},
		{"label", "", []string{"bug", TrackingLabel}, "", "label"},
		{"label case-insensitive", "", []string{" Hive/Tracking "}, "", "label"},
		{"marker", "", nil, "Report archive.\n<!-- hive:tracking -->\n", "marker"},
		{"marker with spaces", "", nil, "<!--  hive:tracking  -->", "marker"},
		{"keep-open sentence", "", nil, "Each run appends its report here.\n\nKeep this issue open. If it is closed, the workflow creates a new tracking issue.", "keep-open"},
		{"keep-open re-flowed", "", nil, "please keep  this\nissue open", "keep-open"},
		{"label wins over marker", "", []string{TrackingLabel}, TrackingMarker, "label"},
		{"plain bug", "", []string{"bug"}, "The dashboard crashes on load.", ""},
		{"ops-tracking is not tracking", "", nil, OpsTrackingMarker, ""},
		{"similar prose is not the sentence", "", nil, "keep this open issue in mind", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TrackingReason(tc.title, tc.labels, tc.body)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("TrackingReason = %q,%v want %q", got, ok, tc.want)
			}
			if IsTracking(tc.title, tc.labels, tc.body) != ok {
				t.Fatalf("IsTracking disagrees with TrackingReason")
			}
		})
	}
}

// hivecommons/hive#14613 as the picker met it: an ordinary title, a body in
// the length window, no excluded label — and the keep-open sentence.
func TestScoreSkipsTrackingIssue(t *testing.T) {
	p := Policy{}
	c := good()
	c.Title = "Weekly competitor report"
	c.Body = "Tracking issue for the weekly competitor and SWOT report produced by the `weekly-competitor-report` agentic workflow.\n\n" +
		"Each run appends its report here as a comment so there is a searchable record in the repo.\n\n" +
		"Keep this issue open. If it is closed, the workflow creates a new tracking issue with the same title.\n"
	if d := Score(c, p, now); d.Eligible || d.Skip != SkipTracking+":keep-open" {
		t.Fatalf("keep-open sentence: %+v", d)
	}
	c = good()
	c.Body += "\n" + TrackingMarker + "\n"
	if d := Score(c, p, now); d.Eligible || d.Skip != SkipTracking+":marker" {
		t.Fatalf("marker: %+v", d)
	}
	c = good()
	c.Labels = append(c.Labels, TrackingLabel)
	if d := Score(c, p, now); d.Eligible || d.Skip != SkipTracking+":label" {
		t.Fatalf("label: %+v", d)
	}
	// A tracking issue someone already stamped `good first issue` is
	// withdrawn, not merely skipped.
	for _, skip := range []string{SkipTracking + ":label", SkipTracking + ":marker", SkipTracking + ":keep-open", SkipTracking + ":gh-aw-marker", SkipTracking + ":gh-aw-managed", SkipTracking + ":gh-aw-label"} {
		if !Withdrawable(skip) {
			t.Errorf("%q should withdraw", skip)
		}
	}
	if Withdrawable(SkipTracking) {
		t.Errorf("bare %q is not a Score reason", SkipTracking)
	}
	if d := Score(good(), p, now); !d.Eligible {
		t.Fatalf("control: %+v", d)
	}
}

func TestSweepWithdrawsMislabelledTrackingIssue(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	opts := quietOpts()

	c := good()
	c.Title = "Weekly competitor report"
	c.Body += "\n" + TrackingMarker + "\n"
	c.Labels = []string{Label, HelpWantedLabel}
	forge.comments[c.Key()] = []string{Marker + "\n### 🌱 Good first issue"}

	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Withdrawn) != 1 || res.Withdrawn[0] != c.Key() || len(res.Selected) != 0 || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(forge.removed[c.Key()], ","); got != Label+","+HelpWantedLabel {
		t.Fatalf("removed %q", got)
	}
	if n := forge.commentCount(c.Repo, c.Number); n != 1 {
		t.Fatalf("comment count %d (must edit, not add)", n)
	}
}

// hivecommons/hive#14646 as the picker met it: gh-aw's own labels and
// body, plus the `good first issue` + `help wanted` the hive had already
// applied. The sweep must withdraw them and say why in the correction.
func TestSweepWithdrawsGHAWManagedIssue(t *testing.T) {
	forge := newFakeForge()
	s := NewSweeper()
	opts := quietOpts()

	c := good()
	c.Title = "[aw] Detection Runs"
	c.Body = ghawDetectionRunsBody
	c.Labels = []string{Label, HelpWantedLabel, GHAWLabel}
	forge.comments[c.Key()] = []string{Marker + "\n### 🌱 Good first issue"}

	if d := Score(c, Policy{}, now); d.Eligible || d.Skip != SkipTracking+":gh-aw-marker" {
		t.Fatalf("score: %+v", d)
	}
	res := s.Sweep(context.Background(), forge, []Candidate{c}, opts)
	if len(res.Withdrawn) != 1 || res.Withdrawn[0] != c.Key() || len(res.Selected) != 0 || res.Errors != 0 {
		t.Fatalf("result %+v", res)
	}
	if got := strings.Join(forge.removed[c.Key()], ","); got != Label+","+HelpWantedLabel {
		t.Fatalf("removed %q", got)
	}
	if n := forge.commentCount(c.Repo, c.Number); n != 1 {
		t.Fatalf("comment count %d (must edit, not add)", n)
	}
	note := WithdrawnComment(c, SkipTracking+":gh-aw-marker", opts.Links, now)
	if !strings.Contains(note, "GitHub Agentic Workflows") || strings.Contains(note, "tracking:gh-aw-marker") {
		t.Fatalf("withdrawn note does not explain the gh-aw reason:\n%s", note)
	}
}

package gfi

import (
	"context"
	"strings"
	"testing"
)

func TestTrackingReason(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		body   string
		want   string
	}{
		{"label", []string{"bug", TrackingLabel}, "", "label"},
		{"label case-insensitive", []string{" Hive/Tracking "}, "", "label"},
		{"marker", nil, "Report archive.\n<!-- hive:tracking -->\n", "marker"},
		{"marker with spaces", nil, "<!--  hive:tracking  -->", "marker"},
		{"keep-open sentence", nil, "Each run appends its report here.\n\nKeep this issue open. If it is closed, the workflow creates a new tracking issue.", "keep-open"},
		{"keep-open re-flowed", nil, "please keep  this\nissue open", "keep-open"},
		{"label wins over marker", []string{TrackingLabel}, TrackingMarker, "label"},
		{"plain bug", []string{"bug"}, "The dashboard crashes on load.", ""},
		{"ops-tracking is not tracking", nil, OpsTrackingMarker, ""},
		{"similar prose is not the sentence", nil, "keep this open issue in mind", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TrackingReason(tc.labels, tc.body)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("TrackingReason = %q,%v want %q", got, ok, tc.want)
			}
			if IsTracking(tc.labels, tc.body) != ok {
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
	for _, skip := range []string{SkipTracking + ":label", SkipTracking + ":marker", SkipTracking + ":keep-open"} {
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

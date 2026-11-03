package github

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// unparkLabelsNow is what GET /issues/{n} answers after the sweep: the
// labels the issue really carries, not what a reply says about them.
func unparkLabelsNow(t *testing.T, c *Client, number int) []string {
	t.Helper()
	issue, _, err := c.client.Issues.Get(context.Background(), "org", "repo", number)
	if err != nil {
		t.Fatalf("reading #%d: %v", number, err)
	}
	return extractLabels(issue.Labels)
}

// An accepted decision lifts the hive's stage/045 whether or not
// project.issue_filter admits the issue. projectbluefin/website#926 and
// fsdk-containers#466 (outside the filter: no triage/accepted) kept
// stage/045 under a reply saying it was removed: the un-park handed the
// engine a label view without the park label, the engine derived no stage
// for a filter-refused issue, and the writer, seeing nothing to change
// against that view, never re-read the issue. The issue then stayed parked,
// since stage/045 itself parks an issue at every later sweep.
func TestUnparkDecisionLiftsStageOutsideIssueFilter(t *testing.T) {
	for _, tc := range []struct {
		name      string
		admitted  bool
		labelled  bool // label-driven repo with label approval
		wantStage string
	}{
		{"label-driven, refused by the filter", false, true, ""},
		{"label-driven, admitted by the filter", true, true, stagelabels.Actionable.Label()},
		{"commands, refused by the filter", false, false, ""},
		{"commands, admitted by the filter", true, false, stagelabels.Actionable.Label()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := []string{"agent/reviewer", NeedsDecisionStageLabel}
			if tc.admitted {
				labels = append(labels, "triage/accepted")
			}
			issue := laIssue(926, labels...)
			issue.Comments = []unparkWireComment{humanComment(6403654548, laWriter, "/hive decision keep the page, link the new docs")}
			c, rec := laServer(t, []unparkWireIssue{issue})
			c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})
			rec.timelines[926] = []unparkWireTimelineEvent{
				labeledAt(1, laHive, NeedsDecisionStageLabel, laBase),
			}
			result := laSweep(t, c, tc.labelled, laPolicyFn(laPolicy(), tc.labelled))
			if len(result.Unparked) != 1 {
				t.Fatalf("expected one un-park, got %+v", result.Unparked)
			}

			now := unparkLabelsNow(t, c, 926)
			if issueHasLabel(now, NeedsDecisionStageLabel) {
				t.Fatalf("labels after the decision = %v: %s is still on the issue", now, NeedsDecisionStageLabel)
			}
			if got := stagelabels.StageLabels(now); tc.wantStage == "" && len(got) != 0 || tc.wantStage != "" && (len(got) != 1 || got[0] != tc.wantStage) {
				t.Fatalf("stage labels after the decision = %v, want %q", got, tc.wantStage)
			}
			if !issueHasLabel(now, HumanAckLabel) || !issueHasLabel(now, "agent/reviewer") {
				t.Fatalf("labels after the decision = %v, want %s added and agent/reviewer kept", now, HumanAckLabel)
			}

			var reply string
			for _, body := range rec.posted[926] {
				if strings.Contains(body, unparkAcceptedMarker) {
					if reply != "" {
						t.Fatalf("more than one acceptance reply: %v", rec.posted[926])
					}
					reply = body
				}
			}
			if reply == "" {
				t.Fatalf("no acceptance reply, got %v", rec.posted[926])
			}
			if strings.Contains(reply, "stays") || strings.Contains(reply, "still on the issue") {
				t.Fatalf("reply says the park stayed, but it lifted:\n%s", reply)
			}
			wantRemoved := "`" + NeedsDecisionStageLabel + "` removed"
			if tc.labelled {
				wantRemoved = "Hive's own " + wantRemoved
			}
			if !strings.Contains(reply, wantRemoved) {
				t.Fatalf("reply does not report the lifted stage (%q):\n%s", wantRemoved, reply)
			}
			if hasNote := strings.Contains(reply, "still outside the agent queue"); hasNote == tc.admitted {
				t.Fatalf("filter note present = %v, want %v:\n%s", hasNote, !tc.admitted, reply)
			}
		})
	}
}

// The reply is built from the labels the issue carries once the engine
// has run, so when the stage label is still there (here a sink that
// records the fact and writes nothing) it never says "removed".
func TestUnparkLabelDrivenReplyNeverClaimsAStageStillPresent(t *testing.T) {
	issue := laIssue(466, "agent/sec-check", NeedsDecisionStageLabel)
	issue.Comments = []unparkWireComment{humanComment(700, laWriter, "/hive decision Option A")}
	c, rec := laServer(t, []unparkWireIssue{issue})
	c.SetIssueFilter(config.IssueFilterConfig{RequireLabels: []string{"triage/accepted"}})
	rec.timelines[466] = []unparkWireTimelineEvent{
		labeledAt(1, laHive, NeedsDecisionStageLabel, laBase.Add(-time.Hour)),
	}
	c.SetStageFactSink(&stageenginetest.Recorder{})
	laSweep(t, c, true, laPolicyFn(laPolicy(), true))
	if !issueHasLabel(unparkLabelsNow(t, c, 466), NeedsDecisionStageLabel) {
		t.Fatalf("test setup: the no-op sink should leave %s in place", NeedsDecisionStageLabel)
	}
	if len(rec.posted[466]) != 1 {
		t.Fatalf("expected one reply, got %v", rec.posted[466])
	}
	reply := rec.posted[466][0]
	if strings.Contains(reply, "Hive's own") {
		t.Fatalf("reply claims a removal that did not happen:\n%s", reply)
	}
	if !strings.Contains(reply, "`"+NeedsDecisionStageLabel+"` is still on the issue") {
		t.Fatalf("reply does not say the stage label is still there:\n%s", reply)
	}
}

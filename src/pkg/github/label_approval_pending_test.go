package github

import (
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// An acceptance withheld by a person's park label completes once that label
// is gone. The pending comment used to carry the final once-only marker, so
// every later pass read it as done and the issue stayed parked
// (projectbluefin/chairlift#752: an issue form's needs-human, then
// triage/accepted, then needs-human removed).
func TestSweepLabelApprovalPendingCompletesAfterPersonLabelRemoved(t *testing.T) {
	const n = 60
	accepted := labeledAt(3, laTriager, "triage/accepted", laBase.Add(2*time.Minute))
	filed := []unparkWireTimelineEvent{
		labeledAt(1, laWriter, "needs-human", laBase),
		labeledAt(2, laHive, NeedsDecisionStageLabel, laBase.Add(time.Minute)),
		accepted,
	}
	lifted := append(append([]unparkWireTimelineEvent{}, filed...), unlabeledAt(4, laTriager, "needs-human", laBase.Add(3*time.Minute)))
	acked := append(append([]unparkWireTimelineEvent{}, lifted...), labeledAt(5, laHive, HumanAckLabel, laBase.Add(4*time.Minute)))

	pendingBody := "`triage/accepted` was added by @" + laTriager + " (triage collaborator), " + labelApprovalPendingPhrase + " `needs-human` (added by @" + laWriter + ") is on this issue."
	newPending := humanComment(700, laHive, labelApprovalPendingMarker(accepted.ID)+"\n"+pendingBody)
	legacyPending := humanComment(701, laHive, labelApprovalMarker(accepted.ID)+"\n"+pendingBody+" Remove that label to let the acceptance complete, or reply `/hive approve`.")
	finalReply := humanComment(702, laHive, labelApprovalMarker(accepted.ID)+"\nApproved via `triage/accepted`, added by @"+laTriager+" (triage collaborator).")
	forgedPending := humanComment(703, laReader, labelApprovalPendingMarker(accepted.ID)+"\n"+pendingBody)

	cases := []struct {
		name        string
		labels      []string
		timeline    []unparkWireTimelineEvent
		comments    []unparkWireComment
		wantEvent   bool
		wantPending bool
		wantRemoved string
		wantAck     bool
		wantMarker  string
	}{
		{
			name:        "person's label present: one pending comment, no approval",
			labels:      []string{"needs-human", NeedsDecisionStageLabel, "triage/accepted"},
			timeline:    filed,
			wantEvent:   true,
			wantPending: true,
			wantMarker:  labelApprovalPendingMarker(accepted.ID),
		},
		{
			name:     "person's label present, pending comment already posted: silent",
			labels:   []string{"needs-human", NeedsDecisionStageLabel, "triage/accepted"},
			timeline: filed,
			comments: []unparkWireComment{newPending},
		},
		{
			name:     "person's label present, old-format pending comment: silent",
			labels:   []string{"needs-human", NeedsDecisionStageLabel, "triage/accepted"},
			timeline: filed,
			comments: []unparkWireComment{legacyPending},
		},
		{
			name:        "person's label present, pending marker from someone else: posts its own",
			labels:      []string{"needs-human", NeedsDecisionStageLabel, "triage/accepted"},
			timeline:    filed,
			comments:    []unparkWireComment{forgedPending},
			wantEvent:   true,
			wantPending: true,
			wantMarker:  labelApprovalPendingMarker(accepted.ID),
		},
		{
			name:        "label removed after pending comment: acceptance completes",
			labels:      []string{NeedsDecisionStageLabel, "triage/accepted"},
			timeline:    lifted,
			comments:    []unparkWireComment{newPending},
			wantEvent:   true,
			wantRemoved: NeedsDecisionStageLabel,
			wantAck:     true,
			wantMarker:  labelApprovalMarker(accepted.ID),
		},
		{
			name:        "label removed after old-format pending comment: acceptance completes",
			labels:      []string{NeedsDecisionStageLabel, "triage/accepted"},
			timeline:    lifted,
			comments:    []unparkWireComment{legacyPending},
			wantEvent:   true,
			wantRemoved: NeedsDecisionStageLabel,
			wantAck:     true,
			wantMarker:  labelApprovalMarker(accepted.ID),
		},
		{
			name:     "final reply already posted: silent",
			labels:   []string{NeedsDecisionStageLabel, "triage/accepted"},
			timeline: lifted,
			comments: []unparkWireComment{legacyPending, finalReply},
		},
		{
			name:     "second pass after release: silent",
			labels:   []string{"triage/accepted", HumanAckLabel},
			timeline: acked,
			comments: []unparkWireComment{legacyPending, finalReply},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := laIssue(n, tc.labels...)
			issue.Comments = tc.comments
			c, rec := laServer(t, []unparkWireIssue{issue})
			rec.timelines[n] = tc.timeline
			result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))

			if !tc.wantEvent {
				if len(result.LabelApproved) != 0 || len(rec.posted[n]) != 0 || len(rec.removed[n]) != 0 || len(rec.added[n]) != 0 {
					t.Fatalf("expected no writes: approved=%+v posted=%v removed=%v added=%v", result.LabelApproved, rec.posted[n], rec.removed[n], rec.added[n])
				}
				return
			}
			if len(result.LabelApproved) != 1 {
				t.Fatalf("expected one label approval event, got %+v", result.LabelApproved)
			}
			ev := result.LabelApproved[0]
			if ev.Pending != tc.wantPending || ev.Released == tc.wantPending {
				t.Fatalf("event pending=%v released=%v, want pending=%v", ev.Pending, ev.Released, tc.wantPending)
			}
			if got := strings.Join(rec.removed[n], ","); got != tc.wantRemoved {
				t.Fatalf("removed = %q, want %q", got, tc.wantRemoved)
			}
			if gotAck := len(rec.added[n]) == 1 && rec.added[n][0] == HumanAckLabel; gotAck != tc.wantAck || (!tc.wantAck && len(rec.added[n]) != 0) {
				t.Fatalf("added = %v, want ack=%v", rec.added[n], tc.wantAck)
			}
			if len(rec.posted[n]) != 1 {
				t.Fatalf("expected exactly one comment, got %v", rec.posted[n])
			}
			body := rec.posted[n][0]
			if !strings.HasPrefix(body, tc.wantMarker+"\n") {
				t.Fatalf("comment must start with %q:\n%s", tc.wantMarker, body)
			}
			if tc.wantPending {
				if strings.Contains(body, labelApprovalMarkerPrefix) {
					t.Errorf("pending comment must not carry the final marker:\n%s", body)
				}
				if !strings.Contains(body, labelApprovalPendingPhrase) || !strings.Contains(body, "`needs-human` (added by @"+laWriter+")") {
					t.Errorf("pending comment must name the blocking label:\n%s", body)
				}
				if strings.Contains(body, "/hive approve") {
					t.Errorf("pending comment must not suggest /hive approve:\n%s", body)
				}
			} else if !strings.Contains(body, "now released") || strings.Contains(body, labelApprovalPendingPhrase) {
				t.Errorf("expected the release comment:\n%s", body)
			}
		})
	}
}

// A pending comment counts as a maintainer answer for the task-list sweep in
// both formats, as it did before it had its own marker.
func TestLastMaintainerAcceptanceAtCountsPendingComment(t *testing.T) {
	c, _ := laServer(t, nil)
	for _, marker := range []string{labelApprovalPendingMarker(9), labelApprovalMarker(9)} {
		comments := []*gh.IssueComment{{
			Body:      gh.Ptr(marker + "\n" + labelApprovalPendingPhrase),
			User:      &gh.User{Login: gh.Ptr(laHive), Type: gh.Ptr("Bot")},
			CreatedAt: &gh.Timestamp{Time: laBase},
		}}
		if c.lastMaintainerAcceptanceAt(comments).IsZero() {
			t.Errorf("a hive pending comment with %q must count as an answer", marker)
		}
	}
}

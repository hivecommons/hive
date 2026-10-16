package github

import (
	"context"
	"strings"
	"testing"
	"time"
)

// `blocked` parks an issue on the person who can lift it
// (hivecommons/hive#12376): the un-park sweep posts the "What to reply"
// notice for it, and its commands lift it.

func blockedIssue12376(number int, comments ...unparkWireComment) unparkWireIssue {
	return unparkWireIssue{
		Number:   number,
		Title:    "blocked work",
		Body:     "An org admin must restore the app installation before this can move.",
		User:     wireUser{Login: "reporter"},
		Labels:   []wireLabel{{Name: issueBlockedLabel}, {Name: "hold"}},
		Comments: comments,
	}
}

func TestIssueParkingLabelIncludesBlocked12376(t *testing.T) {
	cases := []struct {
		label string
		want  bool
	}{
		{"blocked", true},
		{"Blocked", true},
		{" blocked ", true},
		{"needs-human", true},
		{"needs-decision", true},
		{"needs-direction", true},
		{"needs-spec", false},
		{"hold", false},
		{"blocked-by-upstream", false},
		{"hive/blocked", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isIssueParkingLabel(tc.label); got != tc.want {
			t.Errorf("isIssueParkingLabel(%q) = %v, want %v", tc.label, got, tc.want)
		}
		if got := hasIssueParkingLabel([]string{"bug", tc.label}); got != tc.want {
			t.Errorf("hasIssueParkingLabel([bug %q]) = %v, want %v", tc.label, got, tc.want)
		}
		var c *Client
		if got := c.isParkingLabel(tc.label); got != tc.want {
			t.Errorf("Client.isParkingLabel(%q) = %v, want %v", tc.label, got, tc.want)
		}
	}
}

func TestSweepIssueUnparkBlockedPostsNotice12376(t *testing.T) {
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{blockedIssue12376(40)}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 1 {
		t.Fatalf("a blocked issue must be seen as parked, got Seen=%d", result.Seen)
	}
	if len(rec.posted[40]) != 1 {
		t.Fatalf("expected one What to reply notice, got %v", rec.posted[40])
	}
	notice := rec.posted[40][0]
	for _, want := range []string{unparkNoticeMarker, "What to reply", "`/hive decision"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice missing %q:\n%s", want, notice)
		}
	}
	if len(rec.removed[40]) != 0 || len(rec.added[40]) != 0 {
		t.Fatalf("the notice alone changes no labels: removed=%v added=%v", rec.removed[40], rec.added[40])
	}
}

func TestSweepIssueUnparkBlockedCommandsLiftIt12376(t *testing.T) {
	cases := []struct {
		name    string
		comment string
	}{
		{"approve lifts and re-offers", "/hive approve"},
		{"decision lifts with instructions", "/hive decision the admin restored the install; retry the sync"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			number := 41 + i
			issues := []unparkWireIssue{blockedIssue12376(number, humanComment(int64(100+i), "maintainer", tc.comment))}
			server, rec := newUnparkServer(t, "org", "repo", issues, map[string]string{"maintainer": "write"})
			c := newTestClient(t, server, "org", []string{"repo"})

			result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
			if err != nil {
				t.Fatalf("sweep: %v", err)
			}
			if len(result.Unparked) != 1 || result.Unparked[0].Number != number {
				t.Fatalf("expected blocked issue %d un-parked, got %+v", number, result.Unparked)
			}
			removed := strings.Join(rec.removed[number], ",")
			if removed != issueBlockedLabel {
				t.Fatalf("removed = %q, want only %s (hold is never removed)", removed, issueBlockedLabel)
			}
			if len(rec.added[number]) != 1 || rec.added[number][0] != HumanAckLabel {
				t.Fatalf("expected %s added, got %v", HumanAckLabel, rec.added[number])
			}
		})
	}
}

// Label approval removes only the park labels the hive added: an App-applied
// `blocked` goes with the acceptance, a person's `blocked` keeps the issue
// pending.
func TestSweepLabelApprovalBlocked12376(t *testing.T) {
	cases := []struct {
		name        string
		blockedBy   string
		wantRemoved string
		wantPending bool
	}{
		{"blocked applied by the hive is lifted", laHive, issueBlockedLabel, false},
		{"blocked applied by a person holds the acceptance", laWriter, "", true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			number := 50 + i
			c, rec := laServer(t, []unparkWireIssue{laIssue(number, issueBlockedLabel, "triage/accepted")})
			rec.timelines[number] = []unparkWireTimelineEvent{
				labeledAt(1, tc.blockedBy, issueBlockedLabel, laBase),
				labeledAt(2, laTriager, "triage/accepted", laBase.Add(time.Minute)),
			}
			result := laSweep(t, c, true, laPolicyFn(laPolicy(), true))
			if len(result.LabelApproved) != 1 || result.LabelApproved[0].Pending != tc.wantPending {
				t.Fatalf("label approval = %+v, want one event with Pending=%v", result.LabelApproved, tc.wantPending)
			}
			if got := strings.Join(rec.removed[number], ","); got != tc.wantRemoved {
				t.Fatalf("removed = %q, want %q", got, tc.wantRemoved)
			}
		})
	}
}

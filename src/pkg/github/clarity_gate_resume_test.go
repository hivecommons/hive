package github

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Tests for SweepClarityGateResume (hivecommons/hive#13588): the clarity
// gate's `needs-spec` park lifts when the author (or a maintainer) answers
// after the gate's question, and only then.

const clarityHiveBot = "hivecommons-hive[bot]"

func clarityAt(d time.Duration) string { return time.Now().UTC().Add(d).Format(time.RFC3339) }

func clarityComment(id int64, login, body string, age time.Duration) unparkWireComment {
	c := humanComment(id, login, body)
	c.CreatedAt = clarityAt(-age)
	c.UpdatedAt = c.CreatedAt
	return c
}

func clarityQuestion(id int64) unparkWireComment {
	return clarityComment(id, clarityHiveBot, ClarityGateMarker+"\nThanks for filing this.", time.Hour)
}

func clarityParkedIssue(number int, comments ...unparkWireComment) unparkWireIssue {
	return unparkWireIssue{
		Number:   number,
		Title:    "make it better",
		Body:     "it is slow",
		User:     wireUser{Login: "reporter"},
		Labels:   []wireLabel{{Name: issueNeedsSpecLabel}, {Name: "hold"}},
		Comments: comments,
	}
}

func runClarityResume(t *testing.T, issue unparkWireIssue, labeledBy string, permissions map[string]string) (*ClarityGateResumeResult, *unparkServer) {
	t.Helper()
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, permissions)
	if labeledBy != "" {
		rec.timelines = map[int][]unparkWireTimelineEvent{
			issue.Number: {labeledAt(1, labeledBy, issueNeedsSpecLabel, time.Now().Add(-time.Hour))},
		}
	}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin(clarityHiveBot)
	result, err := c.SweepClarityGateResume(context.Background(), ClarityGateResumeOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return result, rec
}

func clarityLifted(rec *unparkServer, number int) bool {
	for _, l := range rec.removed[number] {
		if strings.EqualFold(l, issueNeedsSpecLabel) {
			return true
		}
	}
	return false
}

func TestSweepClarityGateResumeAuthorReplyLifts(t *testing.T) {
	issue := clarityParkedIssue(13581, clarityQuestion(10), clarityComment(11, "reporter", "Expected: page loads in 1s. Acceptance: p95 < 1s.", 0))
	var audited []ClarityGateResumeEvent
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
	rec.timelines = map[int][]unparkWireTimelineEvent{13581: {labeledAt(1, clarityHiveBot, issueNeedsSpecLabel, time.Now().Add(-time.Hour))}}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin(clarityHiveBot)

	result, err := c.SweepClarityGateResume(context.Background(), ClarityGateResumeOptions{
		Audit: func(ev ClarityGateResumeEvent) { audited = append(audited, ev) },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 1 || len(result.Resumed) != 1 || result.Resumed[0].Actor != "reporter" || result.Resumed[0].Label != issueNeedsSpecLabel {
		t.Fatalf("result = %+v, want issue 1 resumed by reporter", result)
	}
	if len(audited) != 1 || audited[0].Number != 13581 {
		t.Fatalf("audit = %+v, want one event for #1", audited)
	}
	if !clarityLifted(rec, 13581) {
		t.Fatalf("removed = %v, want %s lifted", rec.removed[13581], issueNeedsSpecLabel)
	}
	for _, l := range rec.removed[13581] {
		if strings.EqualFold(l, "hold") {
			t.Fatalf("hold must never be removed, got %v", rec.removed[13581])
		}
	}
	if rec.permLookups != 0 {
		t.Fatalf("the author's own reply needs no permission lookup, got %d", rec.permLookups)
	}
	if len(rec.posted[13581]) != 0 {
		t.Fatalf("resume posts nothing, got %v", rec.posted[13581])
	}
}

func TestSweepClarityGateResumeMaintainerReplyLifts(t *testing.T) {
	issue := clarityParkedIssue(13582, clarityQuestion(20), clarityComment(21, "maintainer", "Scope is the dashboard only.", 0))
	result, rec := runClarityResume(t, issue, clarityHiveBot, map[string]string{"maintainer": "write"})
	if len(result.Resumed) != 1 || result.Resumed[0].Actor != "maintainer" || !clarityLifted(rec, 13582) {
		t.Fatalf("result = %+v removed = %v, want a write-access reply to lift", result, rec.removed[13582])
	}
}

func TestSweepClarityGateResumeStrangerReplyDoesNotLift(t *testing.T) {
	issue := clarityParkedIssue(13583, clarityQuestion(30), clarityComment(31, "stranger", "+1 me too", 0))
	result, rec := runClarityResume(t, issue, clarityHiveBot, map[string]string{"stranger": "read"})
	if len(result.Resumed) != 0 || clarityLifted(rec, 13583) {
		t.Fatalf("result = %+v removed = %v, want a read-only stranger's reply ignored", result, rec.removed[13583])
	}
	if rec.permLookups != 1 {
		t.Fatalf("permLookups = %d, want the stranger checked once", rec.permLookups)
	}
}

func TestSweepClarityGateResumeNoMarkerDoesNotLift(t *testing.T) {
	cases := map[string]struct {
		issue     unparkWireIssue
		labeledBy string
	}{
		"no gate comment": {
			issue:     clarityParkedIssue(13584, clarityComment(41, "reporter", "more detail", 0)),
			labeledBy: "maintainer",
		},
		"marker pasted by a person": {
			issue:     clarityParkedIssue(13585, clarityComment(50, "maintainer", ClarityGateMarker+"\nplease clarify", time.Hour), clarityComment(51, "reporter", "more detail", 0)),
			labeledBy: "maintainer",
		},
		"needs-spec re-added by a person after the gate asked": {
			issue:     clarityParkedIssue(13586, clarityQuestion(60), clarityComment(61, "reporter", "more detail", 0)),
			labeledBy: "maintainer",
		},
		"reply came before the question": {
			issue:     clarityParkedIssue(13587, clarityComment(70, "reporter", "first", 2*time.Hour), clarityQuestion(71)),
			labeledBy: clarityHiveBot,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result, rec := runClarityResume(t, tc.issue, tc.labeledBy, map[string]string{"maintainer": "admin"})
			if len(result.Resumed) != 0 || clarityLifted(rec, tc.issue.Number) {
				t.Fatalf("result = %+v removed = %v, want the park left alone", result, rec.removed[tc.issue.Number])
			}
		})
	}
}

func TestSweepClarityGateResumeIgnoresUnlabeledIssuesAndNilClient(t *testing.T) {
	issue := clarityParkedIssue(13588, clarityQuestion(80), clarityComment(81, "reporter", "answers", 0))
	issue.Labels = []wireLabel{{Name: "kind/bug"}}
	result, rec := runClarityResume(t, issue, clarityHiveBot, nil)
	if result.Seen != 0 || len(result.Resumed) != 0 || len(rec.removed[13588]) != 0 {
		t.Fatalf("result = %+v removed = %v, want an issue without needs-spec skipped", result, rec.removed[13588])
	}
	if _, err := (*Client)(nil).SweepClarityGateResume(context.Background(), ClarityGateResumeOptions{}); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v, want ErrNoGitHubClient", err)
	}
}

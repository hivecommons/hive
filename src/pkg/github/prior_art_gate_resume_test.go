package github

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/priorart"
)

// Tests for SweepPriorArtGateResume (hivecommons/hive#13575): the gate's
// stage/017-possible-existing park answers `/hive decision proceed`,
// `/hive decision exists` and a plain reply, and closes a clanker's park
// after the TTL with no reply. The clock is injected; nothing sleeps.

var priorArtNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func priorArtComment(id int64, login, body string, age time.Duration) unparkWireComment {
	c := humanComment(id, login, body)
	c.CreatedAt = priorArtNow.Add(-age).Format(time.RFC3339)
	c.UpdatedAt = c.CreatedAt
	c.HTMLURL = "https://github.com/org/repo/issues/1#issuecomment-" + strings.TrimLeft(c.CreatedAt, " ")
	return c
}

func priorArtGateAsk(id int64, age time.Duration) unparkWireComment {
	return priorArtComment(id, clarityHiveBot, priorart.Marker+"\nThis may already exist.", age)
}

func priorArtParkedIssue(number int, author string, labels []string, comments ...unparkWireComment) unparkWireIssue {
	wl := []wireLabel{{Name: PossiblyExistsStageLabel}}
	for _, l := range labels {
		wl = append(wl, wireLabel{Name: l})
	}
	return unparkWireIssue{
		Number:   number,
		Title:    "add a retry flag",
		Body:     "please add --retry",
		User:     wireUser{Login: author},
		Labels:   wl,
		Comments: comments,
	}
}

func runPriorArtResume(t *testing.T, issue unparkWireIssue, parkedBy string, permissions map[string]string, ttl time.Duration) (*PriorArtGateResumeResult, *unparkServer) {
	t.Helper()
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, permissions)
	if parkedBy != "" {
		rec.timelines = map[int][]unparkWireTimelineEvent{
			issue.Number: {labeledAt(1, parkedBy, PossiblyExistsStageLabel, priorArtNow.Add(-4*24*time.Hour))},
		}
	}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin(clarityHiveBot)
	result, err := c.SweepPriorArtGateResume(context.Background(), PriorArtGateResumeOptions{
		BotCloseTTL: ttl,
		Now:         func() time.Time { return priorArtNow },
	})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return result, rec
}

func priorArtLabelRemoved(rec *unparkServer, number int, label string) bool {
	for _, l := range rec.removed[number] {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}

func TestSweepPriorArtGateResumeProceedLiftsAndRecordsScope(t *testing.T) {
	issue := priorArtParkedIssue(1, "reporter", nil,
		priorArtGateAsk(10, 2*time.Hour),
		priorArtComment(11, "reporter", "/hive decision proceed — the existing flag retries once, we need backoff", time.Minute))
	result, rec := runPriorArtResume(t, issue, clarityHiveBot, nil, 72*time.Hour)
	if len(result.Resumed) != 1 || len(result.Closed) != 0 {
		t.Fatalf("result = %+v, want one resume", result)
	}
	ev := result.Resumed[0]
	if ev.Decision != priorart.DecisionProceed || !strings.Contains(ev.Delta, "backoff") || ev.Actor != "reporter" {
		t.Fatalf("event = %+v", ev)
	}
	if !priorArtLabelRemoved(rec, 1, PossiblyExistsStageLabel) {
		t.Fatalf("removed = %v, want %s lifted", rec.removed[1], PossiblyExistsStageLabel)
	}
	for _, l := range rec.added[1] {
		if l == AlreadyExistsLabel {
			t.Fatalf("proceed must not add %s, got %v", AlreadyExistsLabel, rec.added[1])
		}
	}
	for _, reason := range rec.patched[1] {
		if reason != "" {
			t.Fatalf("proceed must not close (only the body edit may PATCH), got %v", rec.patched[1])
		}
	}
	if len(rec.posted[1]) != 1 || !strings.Contains(rec.posted[1][0], "Scope vs existing") {
		t.Fatalf("posted = %v, want one un-park reply naming the scope section", rec.posted[1])
	}
}

func TestSweepPriorArtGateResumeExistsCloses(t *testing.T) {
	issue := priorArtParkedIssue(2, "reporter", nil,
		priorArtGateAsk(20, 2*time.Hour),
		priorArtComment(21, "reporter", "/hive decision exists", time.Minute))
	result, rec := runPriorArtResume(t, issue, clarityHiveBot, nil, 72*time.Hour)
	if len(result.Closed) != 1 || result.Closed[0].AutoClosed || result.Closed[0].Decision != priorart.DecisionExists {
		t.Fatalf("result = %+v, want one reply-driven close", result)
	}
	if len(rec.added[2]) != 1 || rec.added[2][0] != AlreadyExistsLabel {
		t.Fatalf("added = %v, want %s", rec.added[2], AlreadyExistsLabel)
	}
	if !priorArtLabelRemoved(rec, 2, PossiblyExistsStageLabel) {
		t.Fatalf("removed = %v, want the stage label lifted before close", rec.removed[2])
	}
	if len(rec.patched[2]) != 1 || rec.patched[2][0] != "not_planned" {
		t.Fatalf("patched = %v, want one not_planned close", rec.patched[2])
	}
}

func TestSweepPriorArtGateResumeStrangerReplyIgnored(t *testing.T) {
	issue := priorArtParkedIssue(3, "reporter", nil,
		priorArtGateAsk(30, 2*time.Hour),
		priorArtComment(31, "passerby", "/hive decision exists", time.Minute))
	result, rec := runPriorArtResume(t, issue, clarityHiveBot, map[string]string{"passerby": "read"}, 72*time.Hour)
	if len(result.Resumed) != 0 || len(result.Closed) != 0 {
		t.Fatalf("result = %+v, want nothing", result)
	}
	if len(rec.removed[3]) != 0 || len(rec.patched[3]) != 0 {
		t.Fatalf("no writes expected, removed=%v patched=%v", rec.removed[3], rec.patched[3])
	}
}

func TestSweepPriorArtGateResumeHumanParkedByHandNotLifted(t *testing.T) {
	issue := priorArtParkedIssue(4, "reporter", nil,
		priorArtGateAsk(40, 2*time.Hour),
		priorArtComment(41, "reporter", "/hive decision proceed — more", time.Minute))
	result, rec := runPriorArtResume(t, issue, "maintainer", nil, 72*time.Hour)
	if len(result.Resumed) != 0 || len(rec.removed[4]) != 0 {
		t.Fatalf("a person's 017 must stay: result=%+v removed=%v", result, rec.removed[4])
	}
}

func TestSweepPriorArtGateResumeClankerAutoClosesAfterTTL(t *testing.T) {
	issue := priorArtParkedIssue(5, "other-hive[bot]", nil, priorArtGateAsk(50, 80*time.Hour))
	result, rec := runPriorArtResume(t, issue, clarityHiveBot, nil, 72*time.Hour)
	if len(result.Closed) != 1 || !result.Closed[0].AutoClosed {
		t.Fatalf("result = %+v, want one auto-close", result)
	}
	if len(rec.added[5]) != 1 || rec.added[5][0] != AlreadyExistsLabel || len(rec.patched[5]) != 1 || rec.patched[5][0] != "not_planned" {
		t.Fatalf("added=%v patched=%v", rec.added[5], rec.patched[5])
	}
	if len(rec.posted[5]) != 1 || !strings.Contains(rec.posted[5][0], priorart.AutoCloseMarker) {
		t.Fatalf("posted = %v, want the auto-close notice", rec.posted[5])
	}
}

func TestSweepPriorArtGateResumeClankerWithinTTLWaits(t *testing.T) {
	issue := priorArtParkedIssue(6, "other-hive[bot]", nil, priorArtGateAsk(60, 10*time.Hour))
	result, rec := runPriorArtResume(t, issue, clarityHiveBot, nil, 72*time.Hour)
	if len(result.Closed) != 0 || len(rec.patched[6]) != 0 {
		t.Fatalf("within the TTL nothing closes: result=%+v patched=%v", result, rec.patched[6])
	}
}

func TestSweepPriorArtGateResumeTTLMeasuredFromLaterPark(t *testing.T) {
	// The ask is old but the park label landed 4 days ago in the fixture
	// timeline; a park re-applied 1h ago must restart the clock.
	issue := priorArtParkedIssue(7, "other-hive[bot]", nil, priorArtGateAsk(70, 80*time.Hour))
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, nil)
	rec.timelines = map[int][]unparkWireTimelineEvent{7: {labeledAt(1, clarityHiveBot, PossiblyExistsStageLabel, priorArtNow.Add(-time.Hour))}}
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin(clarityHiveBot)
	result, err := c.SweepPriorArtGateResume(context.Background(), PriorArtGateResumeOptions{BotCloseTTL: 72 * time.Hour, Now: func() time.Time { return priorArtNow }})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Closed) != 0 {
		t.Fatalf("park re-applied 1h ago must not auto-close: %+v", result)
	}
}

func TestSweepPriorArtGateResumeHumanNeverAutoCloses(t *testing.T) {
	issue := priorArtParkedIssue(8, "reporter", nil, priorArtGateAsk(80, 400*time.Hour))
	result, rec := runPriorArtResume(t, issue, clarityHiveBot, nil, 72*time.Hour)
	if len(result.Closed) != 0 || len(rec.patched[8]) != 0 {
		t.Fatalf("a person's park is never auto-closed: result=%+v patched=%v", result, rec.patched[8])
	}
}

func TestSweepPriorArtGateResumeAuthorLabelBeatsLoginHeuristic(t *testing.T) {
	// author/human on a bot-looking login: treated as a person, no auto-close.
	issue := priorArtParkedIssue(9, "relay[bot]", []string{"author/human"}, priorArtGateAsk(90, 400*time.Hour))
	result, _ := runPriorArtResume(t, issue, clarityHiveBot, nil, 72*time.Hour)
	if len(result.Closed) != 0 {
		t.Fatalf("author/human must suppress the clanker TTL: %+v", result)
	}
}

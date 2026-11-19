package github

// Tests for the Follow-ups list's quiet-mode delivery
// (hivecommons/hive#18606): with github.notifications.mode: quiet the held
// findings are a collapsible section of the PR's hive:status card, edited
// in place, and never a comment of their own.

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/prstatus"
)

func quietFollowUpsClient(t *testing.T, url string) (*Client, *fakeBacklogSink) {
	t.Helper()
	c := reviewTestClient(t, url)
	c.SetAppBotLogin("hive[bot]")
	c.SetNotificationsQuiet(func() bool { return true })
	sink := &fakeBacklogSink{dest: config.ReviewBacklogLinear}
	c.SetReviewBacklogRouting(func() ReviewBacklogRouting {
		return ReviewBacklogRouting{
			Severity: config.ReviewSeverityConfig{BlockAt: "P1"},
			Backlog:  config.ReviewBacklogConfig{Destination: config.ReviewBacklogLinear, MinSeverity: "P1"},
			Sink:     sink,
		}
	})
	return c, sink
}

func followUpsCommentCount(bodies []string) int {
	n := 0
	for _, b := range bodies {
		if strings.Contains(b, reviewFollowUpsCommentMarker) && !prstatus.IsStatusComment(b) {
			n++
		}
	}
	return n
}

func statusCardBody(t *testing.T, bodies []string) string {
	t.Helper()
	var card string
	n := 0
	for _, b := range bodies {
		if prstatus.IsStatusComment(b) {
			card = b
			n++
		}
	}
	if n != 1 {
		t.Fatalf("status cards = %d, want exactly one: %q", n, bodies)
	}
	return card
}

func TestReviewFollowUpsQuietFoldIntoExistingStatusCard(t *testing.T) {
	withReviewBacklogState(t)
	server := &followUpsCommentServer{bodies: []string{
		prstatus.Upsert("", prstatus.KeyStageStalled, "<!-- hive:stage-stalled --> waiting"),
	}}
	srv := httptest.NewServer(server.handler(t))
	defer srv.Close()
	c, sink := quietFollowUpsClient(t, srv.URL)
	ctx := context.Background()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	report := routingReport(t, 5, "abc",
		nit("stale example", "low", "docs/a.md", 3, "shows the old flag, cc @alice"),
		nit("naming", "info", "pkg/c.go", 4, "prefer camelCase"),
	)
	if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, now); err != nil {
		t.Fatal(err)
	}
	if len(sink.created) != 0 {
		t.Fatalf("held findings filed: %+v", sink.created)
	}
	if got := followUpsCommentCount(server.bodies); got != 0 {
		t.Fatalf("quiet mode posted %d Follow-ups comments, want none: %q", got, server.bodies)
	}
	card := statusCardBody(t, server.bodies)
	if server.patches == 0 {
		t.Fatal("the existing card was never edited")
	}
	if _, ok := prstatus.Get(card, prstatus.KeyStageStalled); !ok {
		t.Fatalf("existing card section lost:\n%s", card)
	}
	section, ok := prstatus.Get(card, prstatus.KeyReviewFollowUps)
	if !ok {
		t.Fatalf("no %s section:\n%s", prstatus.KeyReviewFollowUps, card)
	}
	if strings.Contains(card, reviewFollowUpsCommentMarker) {
		t.Errorf("the card must not carry the legacy comment marker (the marker upsert would overwrite the card):\n%s", card)
	}
	for _, want := range []string{reviewFollowUpsCardMarker, "<details>", "Review follow-ups: 2 open of 2", "min_severity: P1", "- [ ] **P3 low**", "stale example (`docs/a.md:3`)", "naming (`pkg/c.go:4`)", "</details>"} {
		if !strings.Contains(section, want) {
			t.Errorf("section missing %q:\n%s", want, section)
		}
	}
	if strings.Contains(section, "@alice") {
		t.Errorf("section must not mention anyone live:\n%s", section)
	}

	// A person ticks one entry on the card; the next head re-reports it
	// and adds another. The tick survives, nothing is listed twice and no
	// comment is posted.
	server.bodies[0] = strings.Replace(server.bodies[0], "- [ ] **P3 info**", "- [x] **P3 info**", 1)
	again := routingReport(t, 5, "abd",
		nit("naming", "info", "pkg/c.go", 4, "prefer camelCase"),
		nit("typo", "low", "pkg/b.go", 9, "receive"),
	)
	if err := c.fileOutOfScopeReviewBacklog(ctx, ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: again}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(server.bodies) != 1 || followUpsCommentCount(server.bodies) != 0 {
		t.Fatalf("second pass posted a comment: %q", server.bodies)
	}
	section, _ = prstatus.Get(server.bodies[0], prstatus.KeyReviewFollowUps)
	if strings.Count(section, "naming (`pkg/c.go:4`)") != 1 || !strings.Contains(section, "typo (`pkg/b.go:9`)") {
		t.Fatalf("merged section wrong:\n%s", section)
	}
	if !strings.Contains(section, "- [x] **P3 info**") || !strings.Contains(section, "2 open of 3") {
		t.Fatalf("the ticked entry must stay ticked and drop out of the open count:\n%s", section)
	}
}

func TestReviewFollowUpsQuietCreatesCardWhenNone(t *testing.T) {
	withReviewBacklogState(t)
	server := &followUpsCommentServer{}
	srv := httptest.NewServer(server.handler(t))
	defer srv.Close()
	c, _ := quietFollowUpsClient(t, srv.URL)
	report := routingReport(t, 5, "abc", nit("stale example", "low", "docs/a.md", 3, "shows the old flag"))
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if got := followUpsCommentCount(server.bodies); got != 0 {
		t.Fatalf("Follow-ups comments = %d, want the list on the card only: %q", got, server.bodies)
	}
	section, ok := prstatus.Get(statusCardBody(t, server.bodies), prstatus.KeyReviewFollowUps)
	if !ok || !strings.Contains(section, "stale example (`docs/a.md:3`)") {
		t.Fatalf("section = %q (found %v)", section, ok)
	}
}

// A PR that already carries the legacy Follow-ups comment keeps it, edited
// in place, so the list is never shown twice.
func TestReviewFollowUpsQuietKeepsLegacyComment(t *testing.T) {
	withReviewBacklogState(t)
	legacy := reviewFollowUpsCommentMarker + "\n### Review follow-ups\n\n- [ ] **P3 low** docs-currency — old (`docs/z.md:1`)\n"
	server := &followUpsCommentServer{bodies: []string{legacy}}
	srv := httptest.NewServer(server.handler(t))
	defer srv.Close()
	c, _ := quietFollowUpsClient(t, srv.URL)
	report := routingReport(t, 5, "abc", nit("stale example", "low", "docs/a.md", 3, "shows the old flag"))
	if err := c.fileOutOfScopeReviewBacklog(context.Background(), ReviewRequest{Repo: "o/r", Number: 5, Agent: "reviewer", Report: report}, time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, b := range server.bodies {
		if prstatus.IsStatusComment(b) {
			if _, ok := prstatus.Get(b, prstatus.KeyReviewFollowUps); ok {
				t.Fatalf("list duplicated onto the card:\n%s", b)
			}
		}
	}
	if server.patches == 0 || !strings.Contains(server.bodies[0], "stale example (`docs/a.md:3`)") || !strings.Contains(server.bodies[0], "old (`docs/z.md:1`)") {
		t.Fatalf("legacy comment not edited in place: patches=%d body=%q", server.patches, server.bodies[0])
	}
}

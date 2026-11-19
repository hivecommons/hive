package github

// Tests for hivecommons/hive#19262: the sentinel judges the PR's current
// head. A finding on an earlier head no longer keeps the alert label, the
// hive's comment or the status section on a later clean head; a finding on
// the new head keeps the label; a label a person added is never removed.

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/sentinel"
)

const (
	headScopeBot      = "hive[bot]"
	headScopeOld      = "0bee4e4aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headScopeNew      = "f718e15bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	headScopeNumber   = 19192
	headScopeLabelAt  = "2026-11-18T10:00:00Z"
	headScopeAlertAt  = "2026-11-18T10:00:05Z"
	headScopeLaterAt  = "2026-11-18T12:00:00Z"
	headScopeDocsFile = "docs/guide.md"
)

func headScopeOldAlert() string {
	return SentinelComment(DecideSentinelAction("sentinel-alert", SentinelAuthorTrust{}, false), headScopeOld, "eve",
		[]sentinel.Finding{{Rule: sentinel.RuleSensitivePath, Summary: "changes 1 sensitive path(s)", Paths: []string{"Makefile"}}})
}

func headScopeFiles(path string) map[int][]map[string]any {
	return map[int][]map[string]any{headScopeNumber: {{
		"filename": path, "status": "modified", "additions": 1, "deletions": 0, "patch": "@@ -1 +1 @@\n-a\n+b\n",
	}}}
}

// headScopeFixture is a PR still labelled for a finding on headScopeOld, now
// at headScopeNew with files, labelled at labelAt by labelActor.
func headScopeFixture(path, labelActor, labelActorType, labelAt string) *sentinelFixture {
	return &sentinelFixture{
		prs:            []map[string]any{sentinelPR(headScopeNumber, "eve", headScopeNew, "sentinel-alert")},
		files:          headScopeFiles(path),
		labelExists:    true,
		comments:       map[int][]string{headScopeNumber: {headScopeOldAlert()}},
		commentUsers:   map[int][]string{headScopeNumber: {headScopeBot}},
		commentUpdated: map[int][]string{headScopeNumber: {headScopeAlertAt}},
		timeline: map[int][]map[string]any{headScopeNumber: {
			sentinelTimelineLabel("labeled", labelActor, labelActorType, labelAt),
		}},
	}
}

func TestSweepSentinelCleanNewHeadClearsHiveAddedLabel(t *testing.T) {
	f := headScopeFixture(headScopeDocsFile, headScopeBot, "Bot", headScopeLabelAt)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin(headScopeBot)

	var audited []SentinelSweepEvent
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(func(e SentinelSweepEvent) { audited = append(audited, e) }))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Cleared) != 1 || len(res.Flagged) != 0 || len(res.Errors) != 0 {
		t.Fatalf("clean new head must clear the hive's label: res=%+v", res)
	}
	ev := res.Cleared[0]
	if ev.HeadSHA != headScopeNew || ev.ClearedHead != headScopeOld || len(ev.Findings) != 0 || !ev.Cleared {
		t.Fatalf("cleared event=%+v", ev)
	}
	if len(audited) != 1 || !audited[0].Cleared {
		t.Fatalf("the clearance must reach the audit hook: %+v", audited)
	}
	if len(f.removed) != 1 || f.removed[0] != "19192|sentinel-alert" {
		t.Fatalf("removed=%v", f.removed)
	}
	if len(f.labels[headScopeNumber]) != 0 {
		t.Fatalf("no label may be added on a clean head: %v", f.labels[headScopeNumber])
	}
	if len(f.comments[headScopeNumber]) != 1 {
		t.Fatalf("the hive's comment is edited, never a new one: %v", f.comments[headScopeNumber])
	}
	body := f.comments[headScopeNumber][0]
	for _, want := range []string{SentinelMarker + " " + headScopeNew, sentinelCleanHeading, sentinelEarlierAlertSummary, "`Makefile`", shortSHA(headScopeOld)} {
		if !strings.Contains(body, want) {
			t.Errorf("clean comment missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, sentinelAlertHeading) || strings.Contains(body, SentinelMarker+" "+headScopeOld) {
		t.Fatalf("the kept history must not read back as an alert:\n%s", body)
	}
	if sentinelClearedByMaintainer(&gh.IssueComment{Body: gh.Ptr(body)}, true) {
		t.Fatal("a clean record must never count as a maintainer clearance")
	}

	// Same head again: judged already, nothing more is written.
	res, err = c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Cleared) != 0 || len(f.removed) != 1 {
		t.Fatalf("unchanged head must stay quiet: res=%+v err=%v removed=%v", res, err, f.removed)
	}
}

func TestSweepSentinelFindingOnNewHeadKeepsLabel(t *testing.T) {
	f := headScopeFixture("Makefile", headScopeBot, "Bot", headScopeLabelAt)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.SetAppBotLogin(headScopeBot)

	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 1 || len(res.Cleared) != 0 || res.Flagged[0].HeadSHA != headScopeNew {
		t.Fatalf("finding on the new head must stay flagged: res=%+v", res)
	}
	if len(f.removed) != 0 {
		t.Fatalf("label must stay on: removed=%v", f.removed)
	}
	if len(f.labels[headScopeNumber]) != 0 {
		t.Fatalf("label is already on, nothing to add: %v", f.labels[headScopeNumber])
	}
	if len(f.comments[headScopeNumber]) != 1 {
		t.Fatalf("comments=%v", f.comments[headScopeNumber])
	}
	body := f.comments[headScopeNumber][0]
	if !strings.Contains(body, SentinelMarker+" "+headScopeNew) || !strings.Contains(body, sentinelAlertHeading) || strings.Contains(body, SentinelMarker+" "+headScopeOld) {
		t.Fatalf("the alert must be rewritten for the current head:\n%s", body)
	}

	// An unlabelled PR with a finding on its new head gets the label.
	f2 := headScopeFixture("Makefile", headScopeBot, "Bot", headScopeLabelAt)
	f2.prs = []map[string]any{sentinelPR(headScopeNumber, "eve", headScopeNew)}
	f2.timeline = nil
	srv2 := newSentinelServer(t, f2)
	defer srv2.Close()
	c2 := newTestClient(t, srv2, "o", []string{"o/r"})
	c2.SetAppBotLogin(headScopeBot)
	res, err = c2.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || len(res.Flagged) != 1 || len(f2.labels[headScopeNumber]) != 1 {
		t.Fatalf("finding on a new unlabelled head must add the label: res=%+v err=%v labels=%v", res, err, f2.labels[headScopeNumber])
	}
}

func TestSweepSentinelCleanNewHeadLeavesLabelItCannotAttribute(t *testing.T) {
	cases := []struct {
		name      string
		fixture   func() *sentinelFixture
		knowsSelf bool
	}{
		{"label added by a person", func() *sentinelFixture {
			return headScopeFixture(headScopeDocsFile, "maintainer", "User", headScopeLabelAt)
		}, true},
		{"label re-added through the hive after the alert", func() *sentinelFixture {
			return headScopeFixture(headScopeDocsFile, headScopeBot, "Bot", headScopeLaterAt)
		}, true},
		{"no hive alert comment", func() *sentinelFixture {
			f := headScopeFixture(headScopeDocsFile, headScopeBot, "Bot", headScopeLabelAt)
			f.comments = nil
			f.commentUsers = nil
			f.commentUpdated = nil
			return f
		}, true},
		{"alert comment has no timestamp", func() *sentinelFixture {
			f := headScopeFixture(headScopeDocsFile, headScopeBot, "Bot", headScopeLabelAt)
			f.commentUpdated = nil
			return f
		}, true},
		{"hive cannot name its accounts", func() *sentinelFixture {
			return headScopeFixture(headScopeDocsFile, headScopeBot, "Bot", headScopeLabelAt)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fixture()
			before := append([]string(nil), f.comments[headScopeNumber]...)
			srv := newSentinelServer(t, f)
			defer srv.Close()
			c := newTestClient(t, srv, "o", []string{"o/r"})
			if tc.knowsSelf {
				c.SetAppBotLogin(headScopeBot)
			}
			res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
			if err != nil || len(res.Cleared) != 0 || len(res.Flagged) != 0 {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if len(f.removed) != 0 {
				t.Fatalf("a label the hive cannot attribute to its own earlier alert must stay: removed=%v", f.removed)
			}
			if got := f.comments[headScopeNumber]; strings.Join(got, "\x00") != strings.Join(before, "\x00") {
				t.Fatalf("comments must be untouched: %v", got)
			}
		})
	}
}

// TestSentinelQuietTrustedNoticeSectionClearsOnCleanHead: the status card's
// sentinel section named an earlier head's finding ("Head <old> matched");
// a clean new head rewrites it for the current head, and a PR without a
// card never gets one.
func TestSentinelQuietTrustedNoticeSectionClearsOnCleanHead(t *testing.T) {
	f := &sentinelFixture{
		prs:   []map[string]any{sentinelPR(31, "hivecommons-hive[bot]", "head1aaaaaaa")},
		files: sentinelWorkflowFiles(31),
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := quietSentinelClient(t, srv, true)
	if _, err := c.SweepSentinel(context.Background(), trustedSentinelOpts()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(f.comments[31]) != 1 {
		t.Fatalf("want the status card: %v", f.comments[31])
	}

	f.mu.Lock()
	f.commentUsers = map[int][]string{31: {sentinelCardBot}}
	f.prs[0]["head"] = map[string]string{"sha": "head2bbbbbbb"}
	f.files[31] = []map[string]any{{"filename": "README.md", "status": "modified", "patch": "+docs\n"}}
	f.mu.Unlock()
	if _, err := c.SweepSentinel(context.Background(), trustedSentinelOpts()); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(f.comments[31]) != 1 {
		t.Fatalf("the card is edited in place: %v", f.comments[31])
	}
	section, ok := prstatus.Get(f.comments[31][0], SentinelStatusKey)
	if !ok || !strings.Contains(section, SentinelNoticeCardMarker+" head2bbbbbbb") || strings.Contains(section, "head1aaaaaaa") || strings.Contains(section, "matched") {
		t.Fatalf("section must describe the clean current head:\n%s", section)
	}

	// A trusted author's clean PR with no card stays without one.
	g := &sentinelFixture{
		prs:   []map[string]any{sentinelPR(32, "hivecommons-hive[bot]", "head3cccccccc")},
		files: map[int][]map[string]any{32: {{"filename": "README.md", "status": "modified", "patch": "+docs\n"}}},
	}
	srv2 := newSentinelServer(t, g)
	defer srv2.Close()
	c2 := quietSentinelClient(t, srv2, true)
	if _, err := c2.SweepSentinel(context.Background(), trustedSentinelOpts()); err != nil {
		t.Fatalf("sweep clean: %v", err)
	}
	if len(g.comments[32]) != 0 {
		t.Fatalf("a clean PR must not get a card: %v", g.comments[32])
	}
}

func TestSentinelMarkerHead(t *testing.T) {
	if got := sentinelMarkerHead(SentinelMarker + " abc123\n## x"); got != "abc123" {
		t.Fatalf("got %q", got)
	}
	if got := sentinelMarkerHead("no marker"); got != "" {
		t.Fatalf("got %q", got)
	}
}

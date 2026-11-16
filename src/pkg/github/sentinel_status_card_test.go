package github

// Tests for hivecommons/hive#18453: in github.notifications.mode: quiet an
// informational sentinel notice (trusted author, merge not blocked) is the
// `sentinel` section of the PR's hive:status card, edited in place; a
// blocking alert (untrusted author, or trusted_authors_block) still posts
// its own comment; normal mode is unchanged.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/sentinel"
)

const sentinelCardBot = "hive-app[bot]"

func sentinelWorkflowFiles(num int) map[int][]map[string]any {
	return map[int][]map[string]any{num: {{
		"filename": ".github/workflows/ci.yml", "status": "modified", "patch": "+permissions: write-all\n",
	}}}
}

func trustedSentinelOpts() SentinelSweepOptions {
	opts := defaultSentinelOpts(nil)
	opts.TrustedAuthor = func(repo, author string) SentinelAuthorTrust {
		if author == "mallory" {
			return SentinelAuthorTrust{}
		}
		return SentinelAuthorTrust{Trusted: true, Reason: "hive agent"}
	}
	return opts
}

func quietSentinelClient(t *testing.T, srv *httptest.Server, quiet bool) *Client {
	t.Helper()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	c.appBotLogin = sentinelCardBot
	c.SetNotificationsQuiet(func() bool { return quiet })
	return c
}

func countContaining(bodies []string, needle string) int {
	n := 0
	for _, b := range bodies {
		if strings.Contains(b, needle) {
			n++
		}
	}
	return n
}

func TestSentinelQuietTrustedNoticeFoldsIntoStatusCard(t *testing.T) {
	f := &sentinelFixture{
		prs:   []map[string]any{sentinelPR(30, "hivecommons-hive[bot]", "head1aaaaaaa")},
		files: sentinelWorkflowFiles(30),
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := quietSentinelClient(t, srv, true)

	res, err := c.SweepSentinel(context.Background(), trustedSentinelOpts())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Notified) != 1 || len(res.Flagged) != 0 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v", res)
	}
	if len(f.labels[30]) != 0 {
		t.Fatalf("informational notice must not label: %v", f.labels[30])
	}
	if n := countContaining(f.comments[30], SentinelMarker); n != 0 {
		t.Fatalf("informational notice must not post a sentinel comment: %v", f.comments[30])
	}
	if len(f.comments[30]) != 1 || !prstatus.IsStatusComment(f.comments[30][0]) {
		t.Fatalf("want exactly the status card, got %v", f.comments[30])
	}
	section, ok := prstatus.Get(f.comments[30][0], SentinelStatusKey)
	if !ok {
		t.Fatalf("card has no sentinel section:\n%s", f.comments[30][0])
	}
	for _, want := range []string{SentinelNoticeCardMarker + " head1aaaaaaa", "informational", "hive agent", "merge is not blocked", "`permission_escalation`"} {
		if !strings.Contains(section, want) {
			t.Errorf("section missing %q:\n%s", want, section)
		}
	}

	// A new head on the same PR edits the card section in place: still one
	// comment on the PR, now naming the new head.
	f.mu.Lock()
	f.commentUsers = map[int][]string{30: {sentinelCardBot}}
	f.prs[0]["head"] = map[string]string{"sha": "head2bbbbbbb"}
	f.mu.Unlock()
	_, _, suppressedBefore := c.NotificationCounts()
	res, err = c.SweepSentinel(context.Background(), trustedSentinelOpts())
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(res.Notified) != 1 || len(res.Errors) != 0 {
		t.Fatalf("res2=%+v", res)
	}
	if len(f.comments[30]) != 1 {
		t.Fatalf("new head must edit the card, not comment: %v", f.comments[30])
	}
	section, _ = prstatus.Get(f.comments[30][0], SentinelStatusKey)
	if !strings.Contains(section, "head2bbbbbbb") || strings.Contains(section, "head1aaaaaaa") {
		t.Fatalf("section not rewritten for the new head:\n%s", section)
	}
	if _, _, suppressedAfter := c.NotificationCounts(); suppressedAfter <= suppressedBefore {
		t.Fatalf("folded notice should count as suppressed: before=%d after=%d", suppressedBefore, suppressedAfter)
	}
}

func TestSentinelQuietUntrustedHoldStillComments(t *testing.T) {
	card := prstatus.Upsert("", SentinelStatusKey, SentinelNoticeCardMarker+" old -->\nℹ️ **Sentinel:** informational, merge is not blocked.")
	card = prstatus.Upsert(card, "other", "keep me")
	f := &sentinelFixture{
		prs:          []map[string]any{sentinelPR(31, "mallory", "evil1")},
		files:        sentinelWorkflowFiles(31),
		comments:     map[int][]string{31: {card}},
		commentUsers: map[int][]string{31: {sentinelCardBot}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := quietSentinelClient(t, srv, true)

	res, err := c.SweepSentinel(context.Background(), trustedSentinelOpts())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 1 || len(res.Notified) != 0 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v", res)
	}
	if got := f.labels[31]; len(got) != 1 || got[0] != "sentinel-alert" {
		t.Fatalf("hold label=%v", got)
	}
	if n := countContaining(f.comments[31], sentinelAlertHeading); n != 1 {
		t.Fatalf("hold must post its own alert comment: %v", f.comments[31])
	}
	if _, ok := prstatus.Get(f.comments[31][0], SentinelStatusKey); ok {
		t.Fatalf("stale informational section must be cleared on alert:\n%s", f.comments[31][0])
	}
	if body, ok := prstatus.Get(f.comments[31][0], "other"); !ok || body != "keep me" {
		t.Fatalf("other card sections must survive: %q %v", body, ok)
	}
}

func TestSentinelQuietTrustedAuthorsBlockStillComments(t *testing.T) {
	f := &sentinelFixture{
		prs:   []map[string]any{sentinelPR(32, "hivecommons-hive[bot]", "blk1")},
		files: sentinelWorkflowFiles(32),
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := quietSentinelClient(t, srv, true)
	opts := trustedSentinelOpts()
	opts.TrustedAuthorsBlock = true

	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Flagged) != 1 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v", res)
	}
	if len(f.comments[32]) != 1 || !strings.Contains(f.comments[32][0], sentinelAlertHeading) || prstatus.IsStatusComment(f.comments[32][0]) {
		t.Fatalf("security alert must be its own comment, not a card: %v", f.comments[32])
	}
	if len(f.labels[32]) != 1 {
		t.Fatalf("labels=%v", f.labels[32])
	}
}

func TestSentinelNormalModeTrustedNoticeUnchanged(t *testing.T) {
	f := &sentinelFixture{
		prs:   []map[string]any{sentinelPR(33, "hivecommons-hive[bot]", "norm1")},
		files: sentinelWorkflowFiles(33),
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := quietSentinelClient(t, srv, false)

	res, err := c.SweepSentinel(context.Background(), trustedSentinelOpts())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(res.Notified) != 1 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v", res)
	}
	if len(f.comments[33]) != 1 || !strings.Contains(f.comments[33][0], sentinelNoticeHeading) || prstatus.IsStatusComment(f.comments[33][0]) {
		t.Fatalf("normal mode must post the notice comment as before: %v", f.comments[33])
	}
}

func TestSentinelQuietPriorNoticeCommentEditedInPlace(t *testing.T) {
	f := &sentinelFixture{
		prs:          []map[string]any{sentinelPR(34, "hivecommons-hive[bot]", "new1")},
		files:        sentinelWorkflowFiles(34),
		comments:     map[int][]string{34: {SentinelMarker + " old1\n" + sentinelNoticeHeading}},
		commentUsers: map[int][]string{34: {sentinelCardBot}},
	}
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := quietSentinelClient(t, srv, true)

	if _, err := c.SweepSentinel(context.Background(), trustedSentinelOpts()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(f.comments[34]) != 1 || !strings.Contains(f.comments[34][0], SentinelMarker+" new1") {
		t.Fatalf("legacy notice comment should be rewritten in place, no card: %v", f.comments[34])
	}
}

func TestSentinelFoldsNotice(t *testing.T) {
	var nilClient *Client
	if nilClient.sentinelFoldsNotice(false) {
		t.Fatal("nil client must not fold")
	}
	c := &Client{}
	if c.sentinelFoldsNotice(false) {
		t.Fatal("unwired quiet mode must not fold")
	}
	c.SetNotificationsQuiet(func() bool { return true })
	if !c.sentinelFoldsNotice(false) {
		t.Fatal("quiet mode with no prior comment must fold")
	}
	if c.sentinelFoldsNotice(true) {
		t.Fatal("a prior sentinel comment is edited in place, not folded")
	}
}

func TestSentinelStatusSectionShape(t *testing.T) {
	paths := []string{"g", "f", "e", "d", "c", "b", "a"}
	body := SentinelStatusSection(SentinelDecision{Label: "sentinel-alert"}, "0123456789abcdef", []sentinel.Finding{
		{Rule: sentinel.RuleSensitivePath, Summary: "touches sensitive paths", Paths: paths},
		{Rule: "custom", Summary: "no paths"},
	})
	for _, want := range []string{
		SentinelNoticeCardMarker + " 0123456789abcdef -->",
		"(trusted author)",
		"Head `" + shortSHA("0123456789abcdef") + "` matched",
		"(`a`, `b`, `c`, `d`, `e`, +2 more)",
		"- `custom`: no paths\n",
		"No `sentinel-alert` label",
		"sentinel.trusted_authors_block: true",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("section missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, SentinelMarker) {
		t.Fatalf("card section must never carry the sentinel comment marker:\n%s", body)
	}
	if strings.Contains(body, "@") {
		t.Fatalf("card section must not mention anyone:\n%s", body)
	}
	if paths[0] != "g" {
		t.Fatal("input paths must not be reordered")
	}
}

func TestSentinelStatusCardErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := quietSentinelClient(t, srv, true)
	err := c.foldSentinelNotice(context.Background(), "o/r", 1, "h", SentinelDecision{Label: "x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "folding sentinel notice") {
		t.Fatalf("want fold error, got %v", err)
	}
	// Clearing is best effort: an API failure is logged, never panics.
	c.clearSentinelNoticeSection(context.Background(), "o/r", 1)
	// Normal mode never touches the card.
	c.SetNotificationsQuiet(func() bool { return false })
	c.clearSentinelNoticeSection(context.Background(), "o/r", 1)
}

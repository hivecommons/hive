package github

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// The "What to reply" notice stays the newest comment on a parked issue
// (Danathar/exult#374: the notice was posted at 10:03, the agent's new owner
// step at 19:15 re-parked the issue, and the commands sat mid-thread above
// it while the bottom of the issue named none).

func hiveNotice(id int64, body string) unparkWireComment {
	c := humanComment(id, "hive[bot]", body)
	c.HTMLURL = ""
	return c
}

func sweepOneIssue(t *testing.T, issue unparkWireIssue) *unparkServer {
	t.Helper()
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "admin"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetHiveIdentity(HiveIdentity{AppLogin: "hive[bot]"})
	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return rec
}

func noticesPosted(rec *unparkServer, number int) []string {
	var out []string
	for _, body := range rec.posted[number] {
		if strings.Contains(body, unparkNoticeMarker) {
			out = append(out, body)
		}
	}
	return out
}

func TestUnparkNoticeMovesBelowAgentOwnerStep(t *testing.T) {
	notice := hiveNotice(61, unparkNoticeBody(parkedIssue(12).Body))
	ownerStep := hiveNotice(62, "**What I need from you:** apply the change, then reply `/hive done`.\n\n<!-- hive:owner-step -->")
	rec := sweepOneIssue(t, parkedIssue(12, notice, ownerStep))

	posted := noticesPosted(rec, 12)
	if len(posted) != 1 {
		t.Fatalf("want one fresh notice below the owner step, got %v", rec.posted[12])
	}
	if !strings.Contains(posted[0], "> - `/hive done` →") {
		t.Fatalf("the fresh notice should offer the owner step's /hive done, got %q", posted[0])
	}
	if len(rec.deleted) != 1 || rec.deleted[0] != 61 {
		t.Fatalf("the buried notice should be deleted, got %v", rec.deleted)
	}
	if len(rec.edited) != 0 {
		t.Fatalf("a buried notice is replaced, not edited, got %v", rec.edited)
	}
}

func TestUnparkNoticeMovesBelowHumanQuestion(t *testing.T) {
	notice := hiveNotice(63, unparkNoticeBody(parkedIssue(13).Body))
	question := humanComment(64, "maintainer", "Which option keeps the old tags?")
	rec := sweepOneIssue(t, parkedIssue(13, notice, question))

	if len(noticesPosted(rec, 13)) != 1 || len(rec.deleted) != 1 || rec.deleted[0] != 63 {
		t.Fatalf("notice should move below the question: posted=%v deleted=%v", rec.posted[13], rec.deleted)
	}
}

func TestUnparkNoticeStaysAboveCommandAndReply(t *testing.T) {
	notice := hiveNotice(65, unparkNoticeBody(parkedIssue(14).Body))
	help := humanComment(66, "maintainer", "/hive help")
	reply := hiveNotice(67, unparkReplyMarkerPrefix+"66 -->\n**Maintainer commands**")
	rec := sweepOneIssue(t, parkedIssue(14, notice, help, reply))

	if len(noticesPosted(rec, 14)) != 0 || len(rec.deleted) != 0 || len(rec.edited) != 0 {
		t.Fatalf("a command and its reply must not move the notice: posted=%v deleted=%v edited=%v", rec.posted[14], rec.deleted, rec.edited)
	}
}

func TestUnparkNoticeLastEditedInPlace(t *testing.T) {
	notice := hiveNotice(68, unparkNoticeMarker+"\n**What to reply**\n- `/hive help`: an older wording")
	rec := sweepOneIssue(t, parkedIssue(15, notice))

	if len(noticesPosted(rec, 15)) != 0 || len(rec.deleted) != 0 {
		t.Fatalf("a notice that is already last is edited, not moved: posted=%v deleted=%v", rec.posted[15], rec.deleted)
	}
	if body, ok := rec.edited[68]; !ok || body != unparkNoticeBody(parkedIssue(15).Body) {
		t.Fatalf("notice should be edited to the current wording, got %v", rec.edited)
	}
}

func TestUnparkNoticeDeletesLeftoverFromInterruptedMove(t *testing.T) {
	current := unparkNoticeBody(parkedIssue(16).Body)
	old := hiveNotice(69, current)
	ownerStep := hiveNotice(70, "Apply the change, then reply `/hive done`.\n\n<!-- hive:owner-step -->")
	rec := sweepOneIssue(t, parkedIssue(16, old, ownerStep, hiveNotice(71, current)))

	if len(noticesPosted(rec, 16)) != 0 {
		t.Fatalf("the newest notice is already last; nothing should be posted, got %v", rec.posted[16])
	}
	if len(rec.deleted) != 1 || rec.deleted[0] != 69 {
		t.Fatalf("only the leftover notice should be deleted, got %v", rec.deleted)
	}
}

// A write-back gate that refuses the repo (an operator pause here) stops the
// move: nothing is posted or deleted, and the buried notice is only edited in
// place, as before the move existed.
func TestUnparkNoticeMoveHonoursWritebackGate(t *testing.T) {
	server, rec := newUnparkServer(t, "org", "repo", nil, nil)
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetHiveIdentity(HiveIdentity{AppLogin: "hive[bot]"})
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{
		RepoPause: func(string) (writeback.RepoPauseInfo, bool) { return writeback.RepoPauseInfo{}, true },
	}, ""))
	hive := &gh.User{Login: gh.Ptr("hive[bot]")}
	issue := &gh.Issue{Number: gh.Ptr(18), Body: gh.Ptr(parkedIssue(18).Body), User: hive}
	comments := []*gh.IssueComment{
		{ID: gh.Ptr(int64(72)), Body: gh.Ptr(unparkNoticeMarker + "\nold wording"), User: hive},
		{ID: gh.Ptr(int64(73)), Body: gh.Ptr("Apply the change, then reply `/hive done`.\n\n<!-- hive:owner-step -->"), User: hive},
	}
	if err := c.ensureUnparkNoticeComment(context.Background(), "org", "repo", issue, comments, false); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(rec.posted[18]) != 0 || len(rec.deleted) != 0 {
		t.Fatalf("a refused gate must not move the notice: posted=%v deleted=%v", rec.posted[18], rec.deleted)
	}
	if body, ok := rec.edited[72]; !ok || !strings.Contains(body, "> - `/hive done` →") {
		t.Fatalf("the buried notice should still be edited in place, got %v", rec.edited)
	}
}

// The replies sit in a GitHub TIP alert, which GitHub themes green for
// light and dark, with only the recommended reply in bold.
func TestUnparkNoticeIsATipAlertWithRecommendedBold(t *testing.T) {
	body := unparkNoticeBody(parkedIssue(17).Body)
	want := unparkNoticeMarker + "\n> [!TIP]\n> **What to reply**\n" +
		"> - **`/hive approve` → go with the recommendation (option A): strip the secret (recommended)**\n" +
		"> - `/hive decision B` → go with option B: document only\n" +
		"> - `/hive help` → show these again\n\n"
	if !strings.HasPrefix(body, want) {
		t.Fatalf("notice should open with the TIP alert\nwant prefix %q\ngot %q", want, body)
	}
	if strings.Count(body, "**`") != 1 {
		t.Fatalf("only the recommended reply may be bold, got %q", body)
	}
}

func TestUnparkNoticeWithoutRecommendationHasNoBoldReply(t *testing.T) {
	body := renderUnparkNotice(unparkMenu{ownerStep: true})
	if !strings.Contains(body, "> [!TIP]\n> **What to reply**\n> - `/hive done` →") {
		t.Fatalf("owner-step notice should list /hive done in the TIP alert, got %q", body)
	}
	if strings.Contains(body, "**`") || strings.Contains(body, "(recommended)") {
		t.Fatalf("nothing is recommended, so no reply may be bold, got %q", body)
	}
}

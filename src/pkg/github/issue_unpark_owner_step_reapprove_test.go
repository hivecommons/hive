package github

import (
	"context"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// The Danathar/goodreads-mcp#443 shape: a hive-filed owner step (a typed
// release) whose body recommends running the release command, a maintainer
// `/hive approve` the hive accepted, and the architect parking the same step
// again because approving did not start the release. The re-park asks for
// `/hive done`; the notice used to keep recommending `/hive approve` off the
// body's stale "Recommendation" heading, so following it looped.
const reapproveStepMarker = "<!-- hive:owner-step kind=release tag=2026.11.0 target=e22b3b0d649b20f627c7436fadec20d2c4f2b350 -->"

const reapproveIssueBody = "**What I need from you:** Run the one command under Recommendation.\n\n" +
	"## Recommendation\n\nRun the release by hand now:\n\n```bash\ngh workflow run release.yml --ref main\n```\n\n" +
	reapproveStepMarker

const reapproveRepark = "**What I need from you:** Run the command below, then reply `/hive done`.\n\n" +
	"```bash\ngh workflow run release.yml --ref main\n```\n\n" +
	"Approving did not start the release and this agent cannot start it.\n\n" +
	reapproveStepMarker

func reapproveComments(repark string) []*gh.IssueComment {
	approve := &gh.IssueComment{ID: gh.Ptr(int64(101)), User: &gh.User{Login: gh.Ptr("owner")}, Body: gh.Ptr("/hive approve")}
	accepted := &gh.IssueComment{ID: gh.Ptr(int64(102)), User: menuTestHiveUser, Body: gh.Ptr(unparkReplyMarkerPrefix + "101 -->\n" +
		renderUnparkAcceptedReply("owner", "go with the recommendation", []string{issueNeedsDecisionLabel}))}
	reparked := &gh.IssueComment{ID: gh.Ptr(int64(103)), User: menuTestHiveUser, Body: gh.Ptr(repark)}
	return []*gh.IssueComment{approve, accepted, reparked}
}

func TestUnparkMenuReparkedOwnerStepRecommendsDone(t *testing.T) {
	c := menuTestClient()

	// Before the approve: the body's recommendation still offers approve.
	first := c.unparkMenuFor(reapproveIssueBody, menuTestHiveUser, nil)
	if !first.ownerStep || !strings.Contains(first.bullets(), "`/hive approve`") {
		t.Fatalf("the first park offers /hive done and /hive approve:\n%s", first.bullets())
	}

	m := c.unparkMenuFor(reapproveIssueBody, menuTestHiveUser, reapproveComments(reapproveRepark))
	bullets := m.bullets()
	if !m.ownerStep {
		t.Fatal("the re-park is an owner step")
	}
	if !strings.Contains(bullets, "- **`/hive done` → ") || !strings.Contains(bullets, "(recommended)**") {
		t.Fatalf("/hive done must be the recommended reply:\n%s", bullets)
	}
	if strings.Contains(bullets, "/hive approve") {
		t.Fatalf("/hive approve already answered this step and must not be offered again:\n%s", bullets)
	}
	if !strings.Contains(bullets, "`/hive decision <your instructions>`") {
		t.Fatalf("/hive decision stays available:\n%s", bullets)
	}
	if m.hasRecommendation {
		t.Fatal("the body's answered recommendation must not carry over to the re-park")
	}

	// A re-park that does not say `/hive done` still recommends it once
	// approve was answered on the same step, even if it recommends something.
	quiet := "I recommend running the command below yourself.\n\n```bash\ngh workflow run release.yml --ref main\n```\n\n" + reapproveStepMarker
	m = c.unparkMenuFor(reapproveIssueBody, menuTestHiveUser, reapproveComments(quiet))
	if b := m.bullets(); !strings.Contains(b, "- **`/hive done`") || strings.Contains(b, "/hive approve") {
		t.Fatalf("an approved-then-re-parked step recommends /hive done and drops approve:\n%s", b)
	}

	// A different owner step after the approve is a new ask, so approve was
	// not answered for it; it recommends `/hive done` because it asks for it.
	other := strings.Replace(reapproveRepark, "tag=2026.11.0", "tag=2026.11.1", 1)
	m = c.unparkMenuFor(reapproveIssueBody, menuTestHiveUser, reapproveComments(other))
	if b := m.bullets(); !strings.Contains(b, "- **`/hive done`") || strings.Contains(b, "**`/hive approve`") {
		t.Fatalf("a step that asks for /hive done recommends it:\n%s", b)
	}
	// The same new step re-parked with a recommendation of its own still
	// offers approve (it was never approved), just not as the recommendation
	// when it asks for `/hive done`.
	otherRec := "Recommendation: run it now, then reply `/hive done`.\n\n" + strings.Replace(reapproveStepMarker, "tag=2026.11.0", "tag=2026.11.1", 1)
	if b := c.unparkMenuFor(reapproveIssueBody, menuTestHiveUser, reapproveComments(otherRec)).bullets(); !strings.Contains(b, "- `/hive approve`") || !strings.Contains(b, "- **`/hive done`") {
		t.Fatalf("a new, never-approved step keeps approve on offer:\n%s", b)
	}

	// An accepted `/hive decision` is not an approve: nothing is dropped
	// for it, but the re-park is still read from its own text.
	comments := reapproveComments(reapproveRepark)
	comments[0].Body = gh.Ptr("/hive decision go ahead")
	m = c.unparkMenuFor(reapproveIssueBody, menuTestHiveUser, comments)
	if b := m.bullets(); !strings.Contains(b, "- **`/hive done`") || m.hasRecommendation {
		t.Fatalf("decision-answered re-park recommends /hive done from its own text (hasRecommendation=%v):\n%s", m.hasRecommendation, b)
	}

	// A body-level owner step that asks for `/hive done` recommends it, and
	// approve is not bolded beside it.
	body := ownerStepCommentBody("Recommendation: add the secret yourself, then reply `/hive done`.", true)
	b := c.unparkMenuFor(body, menuTestHiveUser, nil).bullets()
	if !strings.Contains(b, "- **`/hive done`") || strings.Contains(b, "**`/hive approve`") {
		t.Fatalf("an owner step that asks for /hive done recommends it, not approve:\n%s", b)
	}
}

// A second `/hive approve` on the re-parked step is answered with a pointer
// to `/hive done` and leaves the issue parked, so the loop cannot restart.
func TestSweepIssueUnparkReapproveOnAnsweredOwnerStepStaysParked(t *testing.T) {
	issue := unparkWireIssue{
		Number: 443,
		Title:  "release not published",
		Body:   reapproveIssueBody,
		User:   wireUser{Login: "hive[bot]"},
		Labels: []wireLabel{{Name: issueNeedsDecisionLabel}},
		Comments: []unparkWireComment{
			humanComment(101, "maintainer", "/hive approve"),
			hiveComment(102, unparkReplyMarkerPrefix+"101 -->\n"+renderUnparkAcceptedReply("maintainer", "go with the recommendation", []string{issueNeedsDecisionLabel})),
			hiveComment(103, reapproveRepark),
			humanComment(104, "maintainer", "/hive approve"),
		},
	}
	server, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{issue}, map[string]string{"maintainer": "write"})
	c := newTestClient(t, server, "org", []string{"repo"})
	c.SetAppBotLogin("hive[bot]")

	result, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(result.Unparked) != 0 || len(rec.removed[443]) != 0 {
		t.Fatalf("a second approve on the same step must not un-park: unparked=%+v removed=%v", result.Unparked, rec.removed[443])
	}
	joined := strings.Join(rec.posted[443], "\n")
	if !strings.Contains(joined, unparkReplyMarkerPrefix+"104 ") || !strings.Contains(joined, "was already accepted for this step") {
		t.Fatalf("expected the already-approved reply to comment 104, got %v", rec.posted[443])
	}
	if strings.Contains(joined, "**`/hive approve`") {
		t.Fatalf("the notice must not recommend /hive approve:\n%s", joined)
	}
}
